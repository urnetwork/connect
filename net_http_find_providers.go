package connect

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

type findProvidersHttpOperationKey struct{}

// A short FindProviders2 response is evidence about this HTTP endpoint only.
// It must not change the strategy's delivery score or choose among routes.
type findProvidersHttpOperation struct {
	strategy                                                *ClientStrategy
	dialer                                                  *clientDialer
	origin                                                  string
	networkGeneration, dialerGeneration, extenderGeneration uint64

	pool             *clientHttpPool
	poolVersion      uint64
	positiveResponse bool
	connection       *atomic.Pointer[clientHttpPoolConn]
}

type findProvidersHttpHint struct {
	connection                                              *clientHttpPoolConn
	origin                                                  string
	networkGeneration, dialerGeneration, extenderGeneration uint64
	deadline                                                time.Time
}

func (self *findProvidersHttpOperation) prepare(request *http.Request) bool {
	if request.Method != http.MethodPost || request.URL.Path != "/network/find-providers2" ||
		request.URL.RawPath != "" || request.URL.RawQuery != "" || request.URL.ForceQuery ||
		moveDialOverrideFromContext(request.Context()) != nil ||
		(request.URL.Scheme != "https" && request.URL.Scheme != "http") ||
		request.Host != request.URL.Host || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	// These headers can opt a POST into transport retries after an ambiguous
	// write. Leave requests with either header on their ordinary strategy path.
	if _, ok := request.Header["Idempotency-Key"]; ok {
		return false
	}
	if _, ok := request.Header["X-Idempotency-Key"]; ok {
		return false
	}

	strategy := self.strategy
	strategy.mutex.Lock()
	defer strategy.mutex.Unlock()
	settings := strategy.settings
	if strategy.closed || len(strategy.dialers) != 1 || len(strategy.extenderIpSecrets) != 0 ||
		!settings.EnableNormal || settings.EnableResilient || settings.ExtenderDirectory != nil ||
		settings.AltUrl != "" || settings.ConnectSettings.ProxySettings != nil ||
		len(settings.ExtenderConfigs) != 0 || len(settings.VlessConfigs) != 0 {
		return false
	}
	for dialer := range strategy.dialers {
		if dialer.description != "normal" || dialer.httpClientFactory != nil || dialer.dialContext != nil {
			return false
		}
		self.dialer = dialer
	}
	self.origin = request.URL.Scheme + "://" + request.URL.Host
	self.networkGeneration = strategy.findProvidersNetworkGeneration
	self.dialerGeneration = strategy.dialerGeneration
	self.extenderGeneration = strategy.customExtenderGeneration
	return true
}

func (self *findProvidersHttpOperation) currentWithLock() bool {
	strategy := self.strategy
	return !strategy.closed &&
		strategy.findProvidersNetworkGeneration == self.networkGeneration &&
		strategy.dialerGeneration == self.dialerGeneration &&
		strategy.customExtenderGeneration == self.extenderGeneration
}

// Validate and lease the same live pool. Retirement before this point leaves
// the normal HELLO path available. Entering Do spends the one POST attempt.
func (self *findProvidersHttpOperation) acquire(ctx context.Context) *clientHttpPool {
	if ctx.Err() != nil || self.strategy.dialerDelivered(self.dialer) {
		return nil
	}

	self.strategy.mutex.Lock()
	defer self.strategy.mutex.Unlock()
	if !self.currentWithLock() {
		return nil
	}
	self.dialer.mutex.Lock()
	defer self.dialer.mutex.Unlock()
	pool := self.dialer.httpPool
	if self.dialer.retired || pool == nil {
		return nil
	}

	pool.stateLock.Lock()
	defer pool.stateLock.Unlock()
	hint := pool.findProvidersHint
	if pool.retired || !pool.conns[hint.connection] || hint.origin != self.origin ||
		hint.networkGeneration != self.networkGeneration ||
		hint.dialerGeneration != self.dialerGeneration ||
		hint.extenderGeneration != self.extenderGeneration ||
		!time.Now().Before(hint.deadline) {
		return nil
	}

	pool.findProvidersHint = findProvidersHttpHint{}
	pool.activeCount++
	self.pool = pool
	self.poolVersion = pool.findProvidersVersion
	return pool
}

func (self *findProvidersHttpOperation) begin(request *http.Request, dialer *clientDialer, pool *clientHttpPool) *http.Request {
	if dialer != self.dialer {
		return request
	}
	if self.pool != pool {
		pool.stateLock.Lock()
		self.pool = pool
		self.poolVersion = pool.findProvidersVersion
		pool.stateLock.Unlock()
	}

	// The callback belongs to this POST attempt. An earlier attempt's
	// checkout cannot change which socket this attempt proved reachable.
	connection := &atomic.Pointer[clientHttpPoolConn]{}
	self.connection = connection
	ctx := httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			conn := info.Conn
			if secure, ok := conn.(interface{ NetConn() net.Conn }); ok {
				conn = secure.NetConn()
			}
			owned, _ := conn.(*clientHttpPoolConn)
			connection.Store(owned)
		},
	})
	return request.WithContext(ctx)
}

func (self *findProvidersHttpOperation) observe(result *evalResult) {
	if result.dialer != self.dialer || result.httpPool != self.pool || result.err != nil || result.response == nil {
		return
	}
	request := result.response.Request
	if result.response.StatusCode == http.StatusOK && request != nil && request.Response == nil &&
		request.Method == http.MethodPost && request.URL.Path == "/network/find-providers2" &&
		request.URL.Scheme+"://"+request.URL.Host == self.origin {
		self.positiveResponse = true
	}
}

func (self *findProvidersHttpOperation) finish(result *FindProviders2Result, err error) {
	if self.pool == nil {
		return
	}

	self.strategy.mutex.Lock()
	defer self.strategy.mutex.Unlock()
	if !self.currentWithLock() {
		return
	}
	self.dialer.mutex.Lock()
	defer self.dialer.mutex.Unlock()
	if self.dialer.retired || self.dialer.httpPool != self.pool {
		return
	}

	self.pool.stateLock.Lock()
	defer self.pool.stateLock.Unlock()
	if self.pool.retired {
		return
	}
	connection := self.connection.Load()
	if err != nil || result == nil || result.Providers == nil || !self.positiveResponse || !self.pool.conns[connection] {
		self.pool.findProvidersVersion++
		self.pool.findProvidersHint = findProvidersHttpHint{}
		return
	}

	if self.pool.findProvidersVersion != self.poolVersion {
		return
	}

	timeout := self.strategy.settings.RequestTimeout
	if idleTimeout := self.strategy.settings.ConnectSettings.IdleConnTimeout; 0 < idleTimeout {
		timeout = min(timeout, idleTimeout)
	}
	self.pool.findProvidersHint = findProvidersHttpHint{
		connection:         connection,
		origin:             self.origin,
		networkGeneration:  self.networkGeneration,
		dialerGeneration:   self.dialerGeneration,
		extenderGeneration: self.extenderGeneration,
		deadline:           time.Now().Add(timeout),
	}
}
