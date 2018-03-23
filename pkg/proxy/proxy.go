// Copyright 2016-2018 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/completion"
	"github.com/cilium/cilium/pkg/envoy"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/maps/proxymap"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/cilium/cilium/pkg/proxy/logger"

	"github.com/go-openapi/strfmt"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	log = logging.DefaultLogger
)

// field names used while logging
const (
	fieldMarker          = "marker"
	fieldSocket          = "socket"
	fieldFd              = "fd"
	fieldProxyRedirectID = "id"

	// portReleaseDelay is the delay until a port is being released
	portReleaseDelay = time.Duration(5) * time.Minute

	// redirectCreationAttempts is the number of attempts to create a redirect
	redirectCreationAttempts = 5
)

// Proxy maintains state about redirects
type Proxy struct {
	*envoy.XDSServer

	// stateDir is the path of the directory where the state of L7 proxies is
	// stored.
	stateDir string

	// mutex is the lock required when modifying any proxy datastructure
	mutex lock.RWMutex

	// rangeMin is the minimum port used for proxy port allocation
	rangeMin uint16

	// rangeMax is the maximum port used for proxy port allocation.
	// If port is unspecified, the proxy will automatically allocate
	// ports out of the rangeMin-rangeMax range.
	rangeMax uint16

	// allocatedPorts is a map of all allocated proxy ports pointing
	// to the redirect rules attached to that port
	allocatedPorts map[uint16]*Redirect

	// redirects is a map of all redirect configurations indexed by
	// the redirect identifier. Redirects may be implemented by different
	// proxies.
	redirects map[string]*Redirect
}

// StartProxySupport starts the servers to support L7 proxies: xDS GRPC server
// and access log server.
func StartProxySupport(minPort uint16, maxPort uint16, stateDir string) *Proxy {
	xdsServer := envoy.StartXDSServer(stateDir)
	envoy.StartAccessLogServer(stateDir, xdsServer, DefaultEndpointInfoRegistry)
	return &Proxy{
		XDSServer:      xdsServer,
		stateDir:       stateDir,
		rangeMin:       minPort,
		rangeMax:       maxPort,
		redirects:      make(map[string]*Redirect),
		allocatedPorts: make(map[uint16]*Redirect),
	}
}

var (
	portRandomizer      = rand.New(rand.NewSource(time.Now().UnixNano()))
	portRandomizerMutex lock.Mutex
)

func (p *Proxy) allocatePort() (uint16, error) {
	portRandomizerMutex.Lock()
	defer portRandomizerMutex.Unlock()

	for _, r := range portRandomizer.Perm(int(p.rangeMax - p.rangeMin + 1)) {
		resPort := uint16(r) + p.rangeMin

		if _, ok := p.allocatedPorts[resPort]; !ok {
			return resPort, nil
		}

	}

	return 0, fmt.Errorf("no available proxy ports")
}

var gcOnce sync.Once

// CreateOrUpdateRedirect creates or updates a L4 redirect with corresponding
// proxy configuration. This will allocate a proxy port as required and launch
// a proxy instance. If the redirect is already in place, only the rules will be
// updated.
func (p *Proxy) CreateOrUpdateRedirect(l4 *policy.L4Filter, id string, source logger.EndpointInfoSource,
	notifier logger.LogRecordNotifier, wg *completion.WaitGroup) (*Redirect, error) {
	gcOnce.Do(func() {
		logger.SetNotifier(notifier)

		if lf := viper.GetString("access-log"); lf != "" {
			if err := logger.OpenLogfile(lf); err != nil {
				log.WithError(err).WithField(logger.FieldFilePath, lf).
					Warn("Cannot open L7 access log")
			}
		}

		if labels := viper.GetStringSlice("agent-labels"); len(labels) != 0 {
			logger.SetMetadata(labels)
		}

		go func() {
			for {
				time.Sleep(time.Duration(10) * time.Second)
				if deleted := proxymap.GC(); deleted > 0 {
					log.WithField("count", deleted).
						Debug("Evicted entries from proxy table")
				}
			}
		}()
	})

	p.mutex.Lock()
	defer p.mutex.Unlock()

	scopedLog := log.WithField(fieldProxyRedirectID, id)

	if r, ok := p.redirects[id]; ok {
		r.mutex.Lock()
		defer r.mutex.Unlock()

		if r.parserType != l4.L7Parser {
			return nil, fmt.Errorf("invalid type %q, must be of type %q", l4.L7Parser, r.parserType)
		}

		r.updateRules(l4)
		err := r.implementation.UpdateRules(wg)
		if err != nil {
			scopedLog.WithError(err).Error("Unable to update ", l4.L7Parser, " proxy")
			return nil, err
		}

		r.lastUpdated = time.Now()

		scopedLog.WithField(logfields.Object, logfields.Repr(r)).
			Debug("updated existing ", l4.L7Parser, " proxy instance")

		return r, nil
	}

	redir := newRedirect(uint16(l4.Port), source, id)
	redir.endpointID = source.GetID()
	redir.ingress = l4.Ingress
	redir.parserType = l4.L7Parser
	redir.updateRules(l4)

retryCreatePort:
	for nRetry := 0; ; nRetry++ {
		to, err := p.allocatePort()
		if err != nil {
			return nil, err
		}

		redir.ProxyPort = to

		switch l4.L7Parser {
		case policy.ParserTypeKafka:
			redir.implementation, err = createKafkaRedirect(redir, kafkaConfiguration{}, DefaultEndpointInfoRegistry)

		case policy.ParserTypeHTTP:
			redir.implementation, err = createEnvoyRedirect(redir, p.stateDir, p.XDSServer, wg)

		default:
			return nil, fmt.Errorf("unsupported L7 parser type: %s", l4.L7Parser)
		}

		switch {
		case err == nil:
			scopedLog.WithField(logfields.Object, logfields.Repr(redir)).
				Debug("Created new ", l4.L7Parser, " proxy instance")

			p.allocatedPorts[to] = redir
			p.redirects[id] = redir

			break retryCreatePort

		// an error occurred, and we have no more retries
		case nRetry >= redirectCreationAttempts:
			scopedLog.WithError(err).Error("Unable to create ", l4.L7Parser, " proxy")
			return nil, err

		// an error ocurred and we can retry
		default:
			scopedLog.WithError(err).Warning("Unable to create ", l4.L7Parser, " proxy, will retry")
		}
	}

	return redir, nil
}

// RemoveRedirect removes an existing redirect.
func (p *Proxy) RemoveRedirect(id string, wg *completion.WaitGroup) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	r, ok := p.redirects[id]
	if !ok {
		return fmt.Errorf("unable to find redirect %s", id)
	}

	log.WithField(fieldProxyRedirectID, id).
		Debug("removing proxy redirect")
	r.implementation.Close(wg)

	delete(p.redirects, id)

	// delay the release and reuse of the port number so it is guaranteed
	// to be safe to listen on the port again
	go func() {
		time.Sleep(portReleaseDelay)

		// The cleanup of the proxymap is delayed a bit to ensure that
		// the datapath has implemented the redirect change and we
		// cleanup the map before we release the port and allow reuse
		proxymap.CleanupOnRedirectClose(r.ProxyPort)

		p.mutex.Lock()
		delete(p.allocatedPorts, r.ProxyPort)
		p.mutex.Unlock()

		log.WithField(fieldProxyRedirectID, id).Debugf("Delayed release of proxy port %d", r.ProxyPort)
	}()

	return nil
}

// ChangeLogLevel changes proxy log level to correspond to the logrus log level 'level'.
func ChangeLogLevel(level logrus.Level) {
	if envoyProxy != nil {
		envoyProxy.ChangeLogLevel(level)
	}
}

func getRedirectStatusModel(r *Redirect) *models.ProxyRedirectStatus {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	return &models.ProxyRedirectStatus{
		Protocol:           string(r.parserType),
		Port:               int64(r.port),
		AllocatedProxyPort: int64(r.ProxyPort),
		EndpointID:         int64(r.endpointID),
		EndpointIdentity: &models.Identity{
			ID:           int64(r.source.GetIdentity()),
			Labels:       r.source.GetLabels(),
			LabelsSHA256: r.source.GetLabelsSHA(),
		},
		Location:    r.getLocation(),
		Created:     strfmt.DateTime(r.created),
		LastUpdated: strfmt.DateTime(r.lastUpdated),
		Rules:       r.getRulesModel(),
	}
}

// getRedirectStatusModel returns the status of all redirects
func (p *Proxy) getRedirectsStatusModel() []*models.ProxyRedirectStatus {
	redirects := make([]*models.ProxyRedirectStatus, len(p.redirects))

	idx := 0
	for _, redirect := range p.redirects {
		redirects[idx] = getRedirectStatusModel(redirect)
		idx++
	}

	return redirects
}

// GetStatusModel returns the proxy status as API model
func (p *Proxy) GetStatusModel() *models.ProxyStatus {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return &models.ProxyStatus{
		IP:        node.GetInternalIPv4().String(),
		PortRange: fmt.Sprintf("%d-%d", p.rangeMin, p.rangeMax),
		Redirects: p.getRedirectsStatusModel(),
	}
}
