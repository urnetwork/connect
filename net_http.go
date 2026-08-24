package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	// "golang.org/x/net/proxy"

	"golang.org/x/net/nettest"

	"maps"

	"github.com/gorilla/websocket"
)

// censorship-resistant strategies for making https connections
// this uses a random walk of best practices to obfuscate and double-encrypt https connections

// note the net_* files are migrated from net.IP/IPNet to netip.Addr/Prefix
// TODO generally the entire package should migrate over to these newer structs

type HttpPostRawFunction func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error)
type HttpGetRawFunction func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error)

// DefaultMaxHttpResponseBodyBytes bounds API responses that are materialized
// in memory. API calls return JSON/control data rather than streamed payloads;
// larger payloads must use an explicitly streaming transport.
const DefaultMaxHttpResponseBodyBytes int64 = 2 * 1024 * 1024

var ErrHttpResponseBodyTooLarge = errors.New("http response body exceeds memory limit")

func DefaultClientStrategySettings() *ClientStrategySettings {
	settings := &ClientStrategySettings{
		ExposeServerIps:       true,
		ExposeServerHostNames: true,

		EnableNormal:    true,
		EnableResilient: true,

		ParallelBlockSize: 4,

		ExpandExtenderProfileCount: 8,
		ExtenderNetworks:           []netip.Prefix{},
		ExtenderHostnames:          []string{},
		ReconnectTimeout:           15 * time.Second,
		MaxExtenderCount:           128,
		ExtenderMinimumWeight:      0.1,
		ExtenderDropTimeout:        5 * time.Minute,

		DohSettings: DefaultDohSettings(),

		HelloRetryTimeout:        5 * time.Second,
		MaxHttpResponseBodyBytes: DefaultMaxHttpResponseBodyBytes,

		GetRetryCount:       1,
		GetRetryStatusCodes: []int{http.StatusBadGateway, http.StatusServiceUnavailable},
		GetRetryMinTimeout:  100 * time.Millisecond,
		GetRetryMaxTimeout:  1000 * time.Millisecond,

		MinNextConnectDelay: 100 * time.Millisecond,
		MaxNextConnectDelay: 1000 * time.Millisecond,

		ConnectSettings: *DefaultConnectSettings(),
	}
	// A configured process budget identifies an embedded/mobile client. Bound
	// the connection-resident HTTP/WebSocket working set there; an unset or
	// reference-sized budget leaves Go and gorilla defaults untouched for
	// desktop and server callers.
	if 0 < MemoryBudget() && MemoryBudget() < referenceMemoryBudgetByteCount {
		settings.HttpReadBufferSize = MemoryScaledCount(4*1024, 2*1024)
		settings.HttpWriteBufferSize = MemoryScaledCount(4*1024, 2*1024)
		settings.WebSocketReadBufferSize = MemoryScaledCount(4*1024, 2*1024)
		settings.WebSocketWriteBufferSize = MemoryScaledCount(4*1024, 2*1024)
		settings.Http2MaxDecoderHeaderTableSize = MemoryScaledCount(4*1024, 2*1024)
		settings.Http2MaxEncoderHeaderTableSize = MemoryScaledCount(4*1024, 2*1024)
		settings.Http2MaxReceiveBufferPerConnection = int(
			MemoryScaledByteCount(mib(1), kib(256)),
		)
		settings.Http2MaxReceiveBufferPerStream = int(
			MemoryScaledByteCount(kib(512), kib(128)),
		)
	}
	return settings
}

type ClientStrategySettings struct {
	// Log, when set, is used by the client strategy. nil resolves to
	// `DefaultLogger()`.
	Log Logger

	// expose consistent ips
	// if true, enables ech
	ExposeServerIps bool
	// expose server names
	// if true, enables non-ech
	// TODO set this to default false
	ExposeServerHostNames bool
	// note that extenders and proxy are the only strategies that will be enabled if
	// `ExposeServerIps == false` and `ExposeServerNames == false`

	EnableNormal bool
	// tls frag, retransmit, tls frag + retransmit
	EnableResilient bool

	// for gets and ws connects
	ParallelBlockSize int

	// the number of new profiles to add per expand
	ExpandExtenderProfileCount int
	ExtenderNetworks           []netip.Prefix
	// these are evaluated with DoH to grow the extender ips
	ExtenderHostnames []string
	ReconnectTimeout  time.Duration
	MaxExtenderCount  int
	// extender minimum weight
	ExtenderMinimumWeight float32
	// drop dialers that have not had a successful connect in this timeout
	ExtenderDropTimeout time.Duration
	// ExtenderConfigs installs exact extender endpoints before discovery.
	// Measurement fixtures use it for hermetic production extender paths. Nil
	// retains normal discovery and selection. The strategy copies each entry.
	ExtenderConfigs []*ExtenderConfig

	DohSettings *DohSettings

	HelloRetryTimeout time.Duration

	// MaxHttpResponseBodyBytes is the largest response body the strategy will
	// materialize. Values <= 0 use DefaultMaxHttpResponseBodyBytes so a partial
	// settings struct cannot accidentally restore an unbounded io.ReadAll.
	MaxHttpResponseBodyBytes int64

	// Embedded low-memory transports set explicit connection-resident buffer
	// and HTTP/2 dynamic-table/receive-window bounds. Zero preserves the
	// library defaults, which is the desktop/server behavior.
	HttpReadBufferSize                 int
	HttpWriteBufferSize                int
	WebSocketReadBufferSize            int
	WebSocketWriteBufferSize           int
	Http2MaxDecoderHeaderTableSize     int
	Http2MaxEncoderHeaderTableSize     int
	Http2MaxReceiveBufferPerConnection int
	Http2MaxReceiveBufferPerStream     int

	// retry a GET whose RESPONSE status is in `GetRetryStatusCodes` —
	// transient gateway statuses meaning the lb momentarily had no healthy
	// upstream (the edge of a deploy). The strategy already retries
	// transport-level failures internally; this covers the surfaced-status
	// case, which callers otherwise see immediately as an error. GETs only:
	// the api's GET endpoints are idempotent (the parallel dialer racing
	// already re-issues them), and a POST is never replayed by the client.
	GetRetryCount       int
	GetRetryStatusCodes []int
	// the jittered pause before a retry, uniform in
	// [GetRetryMinTimeout, GetRetryMaxTimeout)
	GetRetryMinTimeout time.Duration
	GetRetryMaxTimeout time.Duration

	MinNextConnectDelay time.Duration
	MaxNextConnectDelay time.Duration

	// ExtraHeaders, when set, are applied (Set semantics, overriding same-named
	// headers) to every request this strategy issues: http serial/parallel,
	// the hello ping, and websocket dials. Intended for test/simulation
	// environments, e.g. presenting a forwarded-for address to a local server.
	ExtraHeaders http.Header

	ConnectSettings
}

// stores statistics on client strategies
type ClientStrategy struct {
	ctx context.Context
	log Logger

	settings *ClientStrategySettings

	mutex sync.Mutex
	// dialers are only updated inside the mutex
	dialers             map[*clientDialer]bool
	resolvedExtenderIps []netip.Addr

	// custom extenders
	// these take precedence over other extenders
	extenderIpSecrets map[netip.Addr]string

	nextConnectTime time.Time
	// reconnectFastPathCount is the number of reconnect fast-path slots
	// currently held (see NextReconnectTime). Guarded by mutex. The zero value
	// means all slots free, so a bare test-constructed strategy works
	// unchanged.
	reconnectFastPathCount int
}

func NewClientStrategyWithDefaults(ctx context.Context) *ClientStrategy {
	return NewClientStrategy(ctx, DefaultClientStrategySettings())
}

func newNormalDialTlsContext(
	settings *ClientStrategySettings,
	nextProtos []string,
) DialTlsContextFunction {
	tlsConfig := newClientTlsConfig(settings.TlsConfig, nextProtos)
	if settings.ProxySettings == nil && settings.DialContextSettings == nil {
		netDialer := settings.NetDialer()
		tlsDialer := &tls.Dialer{
			NetDialer: netDialer,
			Config:    tlsConfig,
		}
		return tlsDialer.DialContext
	}

	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		// DialContext preserves injected userspace networks in tests and proxy
		// routing in production before wrapping the resulting connection in TLS.
		conn, err := settings.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		netDialer := settings.NetDialer()
		if netDialer.Timeout != 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, netDialer.Timeout)
			defer cancel()
		}
		if !netDialer.Deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, netDialer.Deadline)
			defer cancel()
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			conn.Close()
			return nil, err
		}

		config := tlsConfig.Clone()
		if config.ServerName == "" {
			config.ServerName = host
		}
		tlsConn := tls.Client(conn, config)
		tlsCtx, tlsCancel := context.WithTimeout(ctx, settings.TlsTimeout)
		defer tlsCancel()
		if err := tlsConn.HandshakeContext(tlsCtx); err != nil {
			tlsConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
}

// extender udp 53 to platform extender
func NewClientStrategy(ctx context.Context, settings *ClientStrategySettings) *ClientStrategy {
	// propagate so a strategy-level logger covers dial logging. Copy instead
	// of writing through the caller's settings: the caller may share them
	// with concurrent constructions or other readers (see the platform
	// transport framer settings for the same rule).
	if settings.ConnectSettings.Log == nil {
		copied := *settings
		copied.ConnectSettings.Log = settings.Log
		settings = &copied
	}

	// create dialers to match settings
	dialers := map[*clientDialer]bool{}
	resolvedExtenderIps := []netip.Addr{}

	if settings.EnableNormal {
		// TODO ECH support
		if settings.ExposeServerHostNames && settings.ExposeServerIps {
			dialer := &clientDialer{
				description:        "normal",
				minimumWeight:      0.5,
				priority:           25,
				dialTlsContext:     newNormalDialTlsContext(settings, clientWebSocketNextProtos),
				httpDialTlsContext: newNormalDialTlsContext(settings, clientHttpNextProtos),
				settings:           settings,
			}
			dialers[dialer] = true
		}
	}
	if settings.EnableResilient {
		// TODO ECH support
		if settings.ExposeServerHostNames && settings.ExposeServerIps {
			// fragment+reorder
			dialer1 := &clientDialer{
				description:        "fragment+reorder",
				minimumWeight:      0.25,
				priority:           50,
				dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, true, true, clientWebSocketNextProtos),
				httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, true, true, clientHttpNextProtos),
				settings:           settings,
			}
			// fragment
			// this is the highest priority because it has no performance impact and additional security benefits
			dialer2 := &clientDialer{
				description:        "fragment",
				minimumWeight:      0.25,
				priority:           0,
				dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, true, false, clientWebSocketNextProtos),
				httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, true, false, clientHttpNextProtos),
				settings:           settings,
			}
			// reorder
			dialer3 := &clientDialer{
				description:        "reorder",
				minimumWeight:      0.25,
				priority:           50,
				dialTlsContext:     newResilientDialTlsContext(&settings.ConnectSettings, false, true, clientWebSocketNextProtos),
				httpDialTlsContext: newResilientDialTlsContext(&settings.ConnectSettings, false, true, clientHttpNextProtos),
				settings:           settings,
			}

			dialers[dialer1] = true
			dialers[dialer2] = true
			dialers[dialer3] = true
		}
	}
	for _, extenderConfig := range settings.ExtenderConfigs {
		if extenderConfig == nil {
			continue
		}
		copiedConfig := *extenderConfig
		dialer := &clientDialer{
			description:        "configured extender",
			persistent:         true,
			minimumWeight:      settings.ExtenderMinimumWeight,
			priority:           100,
			dialTlsContext:     newExtenderDialTlsContext(&settings.ConnectSettings, &copiedConfig, clientWebSocketNextProtos),
			httpDialTlsContext: newExtenderDialTlsContext(&settings.ConnectSettings, &copiedConfig, clientHttpNextProtos),
			extenderConfig:     &copiedConfig,
			settings:           settings,
		}
		dialers[dialer] = true
	}
	// FIXME
	/*
		if settings.EnablePt {
			// these route the api via the connect server
			// the connect server runs the api listening for pt connections to the api host

			ptDialer1 := &clientDialer{
				description:    "dns",
				minimumWeight:  0.25,
				priority:       100,
				dialTlsContext: NewPtDialTlsContext(
					&settings.ConnectSettings,
					PacketTranslationModeDns,
					&settings.PacketTranslationSettings,
				),
				settings:       settings,
			}
			ptDialer2 := &clientDialer{
				description:    "dnspump",
				minimumWeight:  0.25,
				priority:       125,
				dialTlsContext: NewPtDialTlsContext(
					&settings.ConnectSettings,
					PacketTranslationModeDnsPump,
					&settings.PacketTranslationSettings,
				),
				settings:       settings,
			}

			dialers[ptDialer1] = true
			dialers[ptDialer2] = true
		}
	*/

	clientStrategy := &ClientStrategy{
		ctx:                 ctx,
		log:                 loggerOrDefault(settings.Log),
		settings:            settings,
		dialers:             dialers,
		resolvedExtenderIps: resolvedExtenderIps,
		extenderIpSecrets:   map[netip.Addr]string{},
	}
	// a host network path change drops the dialers' pooled http connections:
	// they are bound to the old path, and the next api call (auth,
	// find-providers) would otherwise stall on a dead socket until its
	// timeout. Clients rebuild lazily on next use. Unsubscribe rides ctx.
	unsubNetworkChange := AddNetworkChangeListener(clientStrategy.networkChanged)
	go HandleError(func() {
		<-ctx.Done()
		unsubNetworkChange()
	})
	return clientStrategy
}

// networkChanged drops every dialer's pooled connections (idle sockets bound
// to the old network path); in-flight requests finish on their own
// connections, and the http clients rebuild lazily on next use.
func (self *ClientStrategy) networkChanged() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for dialer := range self.dialers {
		dialer.Close()
	}
}

func (self *ClientStrategy) SetCustomExtenders(extenderIpSecrets map[netip.Addr]string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.extenderIpSecrets = maps.Clone(extenderIpSecrets)
	for dialer, _ := range self.dialers {
		if dialer.IsExtender() && !dialer.persistent {
			dialer.Close()
			delete(self.dialers, dialer)
		}
	}
}

func (self *ClientStrategy) CustomExtenders() map[netip.Addr]string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return maps.Clone(self.extenderIpSecrets)
}

// nextConnectMaxLead caps how far the shared next-connect timestamp may run
// ahead of wall clock. Every cold dial advances the one shared timestamp by
// 100ms-1s, and before the cancel release below existed, a dialer whose pacing
// wait was cancelled never gave its step back — so rapid connect/disconnect
// cycles compounded the lead without bound. In the 2026-08-09 field capture,
// 12 window teardowns of ~10 exits each pushed the staircase 60+ seconds ahead
// of wall clock: every new exit was born 'transport down' (its dial slot was a
// minute in the future), cohorts of ~10 transport-downs expired at the 15s
// evaluation deadline, 0 connections were ever proven, and the replacements
// re-queued at the back of the same staircase — unbounded starvation where
// only the first connect cycle ever worked. The cancel release is the primary
// fix; this clamp is the backstop that bounds the damage of any reservation
// that still leaks: a new dialer never waits more than nextConnectMaxLead.
const nextConnectMaxLead = 10 * time.Second

// new connections should use next connect time to avoid flooding the network
// at once.
//
// The returned release gives this caller's reservation back to the staircase.
// It exists for exactly one situation: the caller's pacing wait was cancelled
// (its context ended) before it ever dialed, so the pacing step it reserved
// paces nobody — without the release the step stays consumed and every later
// caller queues behind a dial that will never happen (see nextConnectMaxLead
// for the field failure this produced). A caller that goes on to dial must NOT
// call release — a consumed step is correct pacing for a dial that happened.
// The release is idempotent and never nil, mirroring NextReconnectTime.
func (self *ClientStrategy) NextConnectTime() (time.Time, func()) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	now := time.Now()
	connectDelayRange := self.settings.MaxNextConnectDelay - self.settings.MinNextConnectDelay
	connectDelay := self.settings.MinNextConnectDelay
	if 0 < connectDelayRange {
		connectDelay += time.Duration(mathrand.Int63n(int64(connectDelayRange)))
	}
	nextConnectTime := self.nextConnectTime.Add(connectDelay)
	if nextConnectTime.Before(now) {
		nextConnectTime = now
	}
	if maxLead := now.Add(nextConnectMaxLead); maxLead.Before(nextConnectTime) {
		nextConnectTime = maxLead
	}
	self.nextConnectTime = nextConnectTime

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			// give back this caller's step. Later callers may have stacked
			// behind it; subtracting shifts them all one step earlier, which
			// is exactly the vacated slot closing up. When the returned time
			// was clamped (to now, or to the lead bound) this can under- or
			// over-release by at most one step; the read path re-clamps, so
			// the shared timestamp stays safe in both directions.
			self.nextConnectTime = self.nextConnectTime.Add(-connectDelay)
		})
	}
	return nextConnectTime, release
}

const (
	// reconnectFastPathLimit caps how many callers may hold the reconnect
	// fast path (NextReconnectTime) at once. A device runs a handful of
	// platform transports (h1 + the h3/pt variants), so 4 covers the common
	// migration re-dial burst while guaranteeing the platform LB never sees
	// more than 4 unpaced dials from one strategy -- callers past the cap fall
	// back to the serialized NextConnectTime staircase.
	reconnectFastPathLimit = 4
	// reconnectFastPathMaxDelay is the independent per-caller jitter for a
	// fast-path reconnect: uniform in [0, 250ms). Enough spread that
	// concurrent reconnects do not hit the LB in the same instant, small
	// enough that it never becomes the dominant term of a reconnect.
	reconnectFastPathMaxDelay = 250 * time.Millisecond
)

// NextReconnectTime is the scoped fast path of NextConnectTime for a caller
// whose transport was connected and just lost its connection.
//
// NextConnectTime advances ONE shared timestamp 100ms-1s per caller, which is
// the right shape for cold connects: a burst of brand-new connections
// staircases instead of stampeding the platform. After a network migration the
// same staircase is wrong -- every transport held a working connection seconds
// ago and every one of them must re-dial now, so ~8 necessary re-dials queue
// behind each other and the last waits multiple seconds for a connection the
// network could carry immediately. A reconnect burst is also not a stampede:
// its size is bounded by how many connections were up, and each caller dials
// once.
//
// So a caller that self-identifies as reconnecting draws a small INDEPENDENT
// jitter (0-250ms) that neither reads nor advances the shared timestamp. The
// fast path is capped at reconnectFastPathLimit concurrent holders (a
// semaphore on the strategy) so the platform LB still never sees an unbounded
// herd; a caller past the cap falls back to the serialized path. The returned
// release func frees the slot and MUST be called once the dial attempt
// completes (success or failure); it is idempotent and never nil. Callers use
// this only for the FIRST dial after a lost connection -- retries after a
// failed reconnect go back through NextConnectTime, restoring the old pacing
// exactly when the platform itself is what is failing.
func (self *ClientStrategy) NextReconnectTime() (time.Time, func()) {
	acquired := false
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.reconnectFastPathCount < reconnectFastPathLimit {
			self.reconnectFastPathCount += 1
			acquired = true
		}
	}()
	if !acquired {
		// over the cap: this burst is not small after all; serialize like any
		// other connect. NextConnectTime takes the mutex itself, so it must be
		// called with the lock released (done above). The staircase release is
		// deliberately dropped: this method's release contract is "dial
		// attempt completed", and handing back a pacing step after a dial that
		// actually happened would defeat the pacing. A cancelled wait on this
		// rare over-cap path therefore leaks its step, bounded by
		// nextConnectMaxLead.
		next, _ := self.NextConnectTime()
		return next, func() {}
	}

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			self.reconnectFastPathCount -= 1
		})
	}
	jitter := time.Duration(mathrand.Int63n(int64(reconnectFastPathMaxDelay)))
	return time.Now().Add(jitter), release
}

func (self *ClientStrategy) dialerWeights() map[*clientDialer]float32 {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	weights := map[*clientDialer]float32{}

	if len(self.extenderIpSecrets) == 0 {
		for dialer, _ := range self.dialers {
			w := dialer.Weight()
			weights[dialer] = w
		}
	} else {
		for dialer, _ := range self.dialers {
			if dialer.IsExtender() {
				weights[dialer] = 1.0
			}
		}
	}

	return weights
}

type httpResult struct {
	response *http.Response

	// status string
	// statusCode int
	// header http.Header
	// trailer http.Header
	bodyBytes []byte
}

type evalResult struct {
	dialer *clientDialer
	wsConn *websocket.Conn
	err    error
	// materialize is run only for the selected HTTP response, while the
	// request context is still alive. Losing parallel responses are closed
	// without allocating their bodies.
	materialize func() error

	httpResult
}

func readHttpResponseBody(response *http.Response, maxBytes int64) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("http response has no body")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxHttpResponseBodyBytes
	}
	if maxBytes < response.ContentLength {
		return nil, fmt.Errorf(
			"%w: content length %d, limit %d",
			ErrHttpResponseBodyTooLarge,
			response.ContentLength,
			maxBytes,
		)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if maxBytes < int64(len(bodyBytes)) {
		return nil, fmt.Errorf("%w: limit %d", ErrHttpResponseBodyTooLarge, maxBytes)
	}
	return bodyBytes, nil
}

func newEvalResultFromHttpResponse(response *http.Response, err error, maxBodyBytes int64) *evalResult {
	result := &evalResult{
		err: err,
		httpResult: httpResult{
			response: response,
		},
	}
	if err == nil && (response == nil || response.Body == nil) {
		result.err = fmt.Errorf("http response has no body")
	} else if err == nil {
		result.materialize = func() error {
			defer func() {
				response.Body.Close()
				response.Body = nil
			}()
			bodyBytes, readErr := readHttpResponseBody(response, maxBodyBytes)
			if readErr == nil {
				result.bodyBytes = bodyBytes
			}
			return readErr
		}
	}
	return result
}

func (self *evalResult) Selected() *evalResult {
	if self.materialize != nil {
		self.err = self.materialize()
		self.materialize = nil
	}
	return self
}

func (self *evalResult) Close() {
	self.materialize = nil
	if self.wsConn != nil {
		self.wsConn.Close()
		// if wsConn is set, the response does not need to be closed
		// https://pkg.go.dev/github.com/gorilla/websocket#Dialer.DialContext
	} else if self.response != nil && self.response.Body != nil {
		self.response.Body.Close()
	}
}

// materializeHttpResult returns the response body already read when the
// strategy selected this result.
func materializeHttpResult(result *evalResult) (*httpResult, error) {
	defer result.Close()
	return &result.httpResult, result.err
}

func (self *ClientStrategy) parallelEval(ctx context.Context, eval func(ctx context.Context, dialer *clientDialer) *evalResult) *evalResult {
	// in this order:
	// 1. try all dialers that previously worked sequentially
	// 2. try dialers that previously failed in parallel blocks
	// 3. expand the extenders and try new extenders in parallel blocks

	handleCtx, handleCancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer handleCancel()
	// merge handleCtx with self.ctx
	go HandleError(func() {
		defer handleCancel()
		select {
		case <-handleCtx.Done():
			return
		case <-self.ctx.Done():
			return
		}
	})

	out := make(chan *evalResult)

	run := func(dialer *clientDialer) {
		success := false
		defer func() {
			if !success {
				select {
				case out <- nil:
				case <-handleCtx.Done():
				}
			}
		}()
		result := eval(handleCtx, dialer)
		if result == nil {
			return
		}

		result.dialer = dialer
		select {
		case out <- result:
			success = true
		case <-handleCtx.Done():
			result.Close()
		}
	}

	// keep trying as long as there is time left
	for {
		select {
		case <-handleCtx.Done():
			return nil
		default:
		}

		reconnect := NewReconnect(self.settings.ReconnectTimeout)

		self.collapseExtenderDialers()

		// the number of runs with pending out
		p := 0

		dialerWeights := self.dialerWeights()

		if 0 < len(dialerWeights) {
			serialDialers := []*clientDialer{}
			parallelDialers := []*clientDialer{}

			dialers := slices.Collect(maps.Keys(dialerWeights))
			WeightedShuffle(dialers, dialerWeights)

			// always try the top options first
			serialDialers = append(serialDialers, dialers[0])

			for _, dialer := range dialers[1:] {
				if dialer.IsLastSuccess() {
					serialDialers = append(serialDialers, dialer)
				} else {
					parallelDialers = append(parallelDialers, dialer)
				}
			}

			// WeightedShuffle(serialDialers, dialerWeights)
			slices.SortStableFunc(serialDialers, func(a *clientDialer, b *clientDialer) int {
				return a.priority - b.priority
			})
			for _, dialer := range serialDialers {
				select {
				case <-handleCtx.Done():
					return nil
				default:
				}

				result := eval(handleCtx, dialer)
				if result != nil {
					if result.Selected().err == nil {
						if self.log.V(2).Enabled() {
							self.log.Infof("[net][p]select: %s\n", dialer.String())
						}
						return result
					}
					if self.log.V(2).Enabled() {
						self.log.Infof("[net][p]select: %s = %s\n", dialer.String(), result.err)
					}
					result.Close()
				}
			}

			// note parallel dialers is in the original weighted order
			// WeightedShuffle(parallelDialers, dialerWeights)
			n := min(len(parallelDialers), self.settings.ParallelBlockSize)
			p += n
			for _, dialer := range parallelDialers[0:n] {
				go HandleError(func() {
					run(dialer)
				})
			}
			for _, dialer := range parallelDialers[n:] {
				select {
				case <-handleCtx.Done():
					return nil
				case result := <-out:
					if result != nil {
						if result.Selected().err == nil {
							if self.log.V(2).Enabled() {
								self.log.Infof("[net][p]select: %s\n", result.dialer.String())
							}
							return result
						}
						if self.log.V(2).Enabled() {
							self.log.Infof("[net][p]select: %s = %s\n", result.dialer.String(), result.err)
						}
						result.Close()
					}
					go HandleError(func() {
						run(dialer)
					})
				}
			}
		}

		if expandedDialers, _ := self.expandExtenderDialers(); 0 < len(expandedDialers) {
			n := min(len(expandedDialers), self.settings.ParallelBlockSize-p)
			p += n
			for _, dialer := range expandedDialers[0:n] {
				go HandleError(func() {
					run(dialer)
				})
			}
			for _, dialer := range expandedDialers[n:] {
				select {
				case <-handleCtx.Done():
					return nil
				case result := <-out:
					if result != nil {
						if result.Selected().err == nil {
							if self.log.V(2).Enabled() {
								self.log.Infof("[net][p]select: %s\n", result.dialer.String())
							}
							return result
						}
						if self.log.V(2).Enabled() {
							self.log.Infof("[net][p]select: %s = %s\n", result.dialer.String(), result.err)
						}
						result.Close()
					}
					go HandleError(func() {
						run(dialer)
					})
				}
			}
		}

		for range p {
			select {
			case <-handleCtx.Done():
				return nil
			case result := <-out:
				if result != nil {
					if result.Selected().err == nil {
						return result
					}
					result.Close()
				}
			}
		}

		// the rate limit is important when when the connect timeout is small
		// e.g. local closes due to disconnected network
		select {
		case <-handleCtx.Done():
			return nil
		case <-reconnect.After():
		}
	}

}

func (self *ClientStrategy) serialEval(ctx context.Context, eval func(ctx context.Context, dialer *clientDialer) *evalResult, helloEval func(ctx context.Context, dialer *clientDialer) *evalResult) *evalResult {
	handleCtx, handleCancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer handleCancel()
	// merge handleCtx with self.ctx
	go HandleError(func() {
		defer handleCancel()
		select {
		case <-handleCtx.Done():
			return
		case <-self.ctx.Done():
			return
		}
	}, handleCancel)

	// keep trying as long as there is time left
	for {
		select {
		case <-handleCtx.Done():
			return nil
		default:
		}

		self.collapseExtenderDialers()

		dialerWeights := self.dialerWeights()

		serialDialers := []*clientDialer{}

		for dialer, _ := range dialerWeights {
			if dialer.IsLastSuccess() {
				serialDialers = append(serialDialers, dialer)
			}
		}

		slices.SortStableFunc(serialDialers, func(a *clientDialer, b *clientDialer) int {
			return a.priority - b.priority
		})
		for _, dialer := range serialDialers {
			select {
			case <-handleCtx.Done():
				return nil
			default:
			}

			result := eval(handleCtx, dialer)
			if result != nil {
				if result.Selected().err == nil {
					if self.log.V(2).Enabled() {
						self.log.Infof("[net][s]select: %s\n", dialer.String())
					}
					return result
				}
				if self.log.V(2).Enabled() {
					self.log.Infof("[net][s]select: %s = %s\n", dialer.String(), result.err)
				}
				result.Close()
			}
		}

		// it's more efficient to iterate with a parallel hello
		// keep retrying hello until at least one dialer is success
		for {
			helloStartTime := time.Now()
			result := self.parallelEval(handleCtx, helloEval)
			if result != nil {
				result.Close()
			}
			helloEndTime := time.Now()

			// check if any dialer succeeded
			successCount := 0
			for dialer, _ := range self.dialerWeights() {
				if dialer.IsLastSuccess() {
					successCount += 1
				}
			}
			if 0 < successCount {
				break
			}

			timeout := self.settings.HelloRetryTimeout - helloEndTime.Sub(helloStartTime)
			if 0 < timeout {
				select {
				case <-handleCtx.Done():
					return nil
				case <-time.After(timeout):
				}
			} else {
				select {
				case <-handleCtx.Done():
					return nil
				default:
				}
			}
		}
		// if result.err != nil {
		// 	return &evalResult{
		// 		err: result.err,
		// 	}
		// }
	}

}

// applyExtraHeaders sets the strategy's ExtraHeaders on h (override semantics)
func (self *ClientStrategy) applyExtraHeaders(h http.Header) {
	for name, values := range self.settings.ExtraHeaders {
		h.Del(name)
		for _, value := range values {
			h.Add(name, value)
		}
	}
}

func (self *ClientStrategy) HttpParallel(request *http.Request) (*httpResult, error) {
	self.applyExtraHeaders(request.Header)

	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		httpClient := dialer.HttpClient()
		response, err := httpClient.Do(request.WithContext(handleCtx))
		if self.log.V(2).Enabled() {
			if err != nil {
				self.log.Infof("[net]http parallel %s %s = %s\n", request.Method, request.URL, err)
			} else {
				self.log.Infof("[net]http parallel %s %s = %s\n", request.Method, request.URL, response.Status)
			}
		}

		dialer.Update(handleCtx, err)

		return newEvalResultFromHttpResponse(response, err, self.settings.MaxHttpResponseBodyBytes)
	}

	result := self.parallelEval(request.Context(), eval)
	if result == nil {
		return nil, fmt.Errorf("Timeout.")
	}
	return materializeHttpResult(result)
}

func (self *ClientStrategy) HttpSerial(request *http.Request, helloRequest *http.Request) (*httpResult, error) {
	// in this order:
	// 1. try all dialers that previously worked sequentially
	// 2. retest and expand dialers using get of the hello request.
	//    This is a basic ping to the server, which is run in parallel.
	// 3. continue from 1 until timeout

	self.applyExtraHeaders(request.Header)
	self.applyExtraHeaders(helloRequest.Header)

	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		httpClient := dialer.HttpClient()
		response, err := httpClient.Do(request.WithContext(handleCtx))
		if self.log.V(2).Enabled() {
			if err != nil {
				self.log.Infof("[net]http serial %s %s = %s\n", request.Method, request.URL, err)
			} else {
				self.log.Infof("[net]http serial %s %s = %s\n", request.Method, request.URL, response.Status)
			}
		}

		dialer.Update(handleCtx, err)

		return newEvalResultFromHttpResponse(response, err, self.settings.MaxHttpResponseBodyBytes)
	}
	helloEval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		httpClient := dialer.HttpClient()
		response, err := httpClient.Do(helloRequest.WithContext(handleCtx))
		if self.log.V(2).Enabled() {
			if err != nil {
				self.log.Infof("[net]http serial hello %s %s = %s\n", helloRequest.Method, helloRequest.URL, err)
			} else {
				self.log.Infof("[net]http serial hello %s %s = %s\n", helloRequest.Method, helloRequest.URL, response.Status)
			}
		}

		dialer.Update(handleCtx, err)

		return newEvalResultFromHttpResponse(response, err, self.settings.MaxHttpResponseBodyBytes)
	}

	result := self.serialEval(request.Context(), eval, helloEval)
	if result == nil {
		return nil, fmt.Errorf("Timeout.")
	}
	return materializeHttpResult(result)
}

func (self *ClientStrategy) WsDialContext(ctx context.Context, url string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
	if 0 < len(self.settings.ExtraHeaders) {
		// clone so a caller-held header is not mutated across reconnects
		merged := requestHeader.Clone()
		if merged == nil {
			merged = http.Header{}
		}
		self.applyExtraHeaders(merged)
		requestHeader = merged
	}

	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		wsDialer := dialer.WsDialer(self.settings)
		wsConn, response, err := wsDialer.DialContext(handleCtx, url, requestHeader)
		if self.log.V(2).Enabled() {
			if err != nil {
				self.log.Infof("[net]ws dial %s = %s\n", url, err)
			} else {
				self.log.Infof("[net]ws dial %s = %s\n", url, response.Status)
			}
		}

		dialer.Update(handleCtx, err)

		return &evalResult{
			wsConn: wsConn,
			err:    err,
			httpResult: httpResult{
				// status: response.Status,
				// statusCode: response.StatusCode,
				// header: response.Header.Clone(),
				response: response,
			},
		}
	}

	result := self.parallelEval(ctx, eval)
	if result == nil {
		return nil, nil, fmt.Errorf("Timeout.")
	}
	return result.wsConn, result.response, result.err
}

func (self *ClientStrategy) collapseExtenderDialers() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for dialer, _ := range self.dialers {
		if dialer.IsExtender() && !dialer.persistent && dialer.IsLastSuccess() {
			if self.settings.ExtenderDropTimeout <= time.Now().Sub(dialer.lastErrorTime) {
				dialer.Close()
				delete(self.dialers, dialer)
			}
		}
	}
}

func (self *ClientStrategy) expandExtenderDialers() (expandedDialers []*clientDialer, expandedExtenderIps []netip.Addr) {

	// - distribute new ips evenly over new profiles
	// - distribute existing ids as weighted where needed
	// - `extenderIpSecrets` overrides new ips

	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.settings.ExpandExtenderProfileCount <= 0 {
		return []*clientDialer{}, []netip.Addr{}
	}

	visitedExtenderProfiles := map[ExtenderProfile]bool{}
	visitedExtenderIps := map[netip.Addr]bool{}

	for dialer, _ := range self.dialers {
		if dialer.IsExtender() {
			visitedExtenderProfiles[dialer.extenderConfig.Profile] = true
			visitedExtenderIps[dialer.extenderConfig.Ip] = true
		}
	}

	if self.settings.MaxExtenderCount <= len(visitedExtenderProfiles) {
		// at maximum extenders
		return []*clientDialer{}, []netip.Addr{}
	}

	extenderProfiles := EnumerateExtenderProfiles(
		min(self.settings.ExpandExtenderProfileCount, self.settings.MaxExtenderCount-len(visitedExtenderProfiles)),
		visitedExtenderProfiles,
	)

	extenderConfigs := []*ExtenderConfig{}
	if len(self.extenderIpSecrets) == 0 {

		// filter resolved ips by visited
		unusedExtenderIps := []netip.Addr{}
		for _, ip := range self.resolvedExtenderIps {
			if !visitedExtenderIps[ip] {
				unusedExtenderIps = append(unusedExtenderIps, ip)
			}
		}

		deviceIpv4 := nettest.SupportsIPv4()
		deviceIpv6 := nettest.SupportsIPv6()

		// expand the ips to have one new ip per profile
		if len(unusedExtenderIps) < len(extenderProfiles) {
			// iterate these for ips not used
			for _, network := range self.settings.ExtenderNetworks {
				if network.Addr().Is4() && deviceIpv4 || network.Addr().Is6() && deviceIpv6 {
					for ip := network.Addr(); network.Contains(ip); ip = ip.Next() {
						if !visitedExtenderIps[ip] {
							visitedExtenderIps[ip] = true
							expandedExtenderIps = append(expandedExtenderIps, ip)
						}
					}
				}
			}

			mathrand.Shuffle(len(expandedExtenderIps), func(i int, j int) {
				expandedExtenderIps[i], expandedExtenderIps[j] = expandedExtenderIps[j], expandedExtenderIps[i]
			})

			if len(extenderProfiles) <= len(expandedExtenderIps) {
				expandedExtenderIps = expandedExtenderIps[0:len(extenderProfiles)]
			}

			// if not enough ips, use DoH to load ips for the extender hostnames
			if len(expandedExtenderIps) < len(extenderProfiles) && 0 < len(self.settings.ExtenderHostnames) {

				// the network can be both ipv4 and ipv6
				if deviceIpv4 {
					ips := DohQuery(self.ctx, 4, "A", self.settings.DohSettings, self.settings.ExtenderHostnames...)
					for ip, _ := range ips {
						if !visitedExtenderIps[ip] {
							visitedExtenderIps[ip] = true
							expandedExtenderIps = append(expandedExtenderIps, ip)
						}
					}
				}
				if deviceIpv6 {
					ips := DohQuery(self.ctx, 6, "AAAA", self.settings.DohSettings, self.settings.ExtenderHostnames...)
					for ip, _ := range ips {
						if !visitedExtenderIps[ip] {
							visitedExtenderIps[ip] = true
							expandedExtenderIps = append(expandedExtenderIps, ip)
						}
					}
				}
			}

			unusedExtenderIps = append(unusedExtenderIps, expandedExtenderIps...)
		}

		// unused ips first
		mathrand.Shuffle(len(unusedExtenderIps), func(i int, j int) {
			unusedExtenderIps[i], unusedExtenderIps[j] = unusedExtenderIps[j], unusedExtenderIps[i]
		})
		n := min(len(extenderProfiles), len(unusedExtenderIps))
		for i := range n {
			extenderConfig := &ExtenderConfig{
				Profile: extenderProfiles[i],
				Ip:      unusedExtenderIps[i],
			}
			extenderConfigs = append(extenderConfigs, extenderConfig)
		}

		// existing ips distributed as weighted
		if n < len(extenderProfiles) {
			weights := map[netip.Addr]float32{}

			netWeight := float32(0)
			for dialer, _ := range self.dialers {
				if dialer.IsExtender() {
					w := dialer.Weight()
					weights[dialer.extenderConfig.Ip] = w
					netWeight += w
				}
			}
			for _, ip := range unusedExtenderIps {
				w := self.settings.ExtenderMinimumWeight
				weights[ip] = w
				netWeight += w
			}

			if 0 < len(weights) {
				ips := slices.Collect(maps.Keys(weights))
				mathrand.Shuffle(len(ips), func(i int, j int) {
					ips[i], ips[j] = ips[j], ips[i]
				})

				for _, extenderProfile := range extenderProfiles[n:] {
					v := mathrand.Float32() * netWeight
					i := 0
					for i < len(ips)-1 {
						v -= weights[ips[i]]
						if v <= 0 {
							break
						}
						i += 1
					}
					extenderConfig := &ExtenderConfig{
						Profile: extenderProfile,
						Ip:      ips[i],
					}
					extenderConfigs = append(extenderConfigs, extenderConfig)
				}
			}
		}
	} else {
		ips := slices.Collect(maps.Keys(self.extenderIpSecrets))
		for _, extenderProfile := range extenderProfiles {
			ip := ips[mathrand.Intn(len(ips))]
			extenderConfig := &ExtenderConfig{
				Profile: extenderProfile,
				Ip:      ip,
				Secret:  self.extenderIpSecrets[ip],
			}
			extenderConfigs = append(extenderConfigs, extenderConfig)
		}
	}

	for _, extenderConfig := range extenderConfigs {
		dialer := &clientDialer{
			minimumWeight:      self.settings.ExtenderMinimumWeight,
			priority:           100,
			dialTlsContext:     newExtenderDialTlsContext(&self.settings.ConnectSettings, extenderConfig, clientWebSocketNextProtos),
			httpDialTlsContext: newExtenderDialTlsContext(&self.settings.ConnectSettings, extenderConfig, clientHttpNextProtos),
			extenderConfig:     extenderConfig,
			settings:           self.settings,
		}
		expandedDialers = append(expandedDialers, dialer)
	}

	for _, dialer := range expandedDialers {
		self.dialers[dialer] = true
	}
	self.resolvedExtenderIps = append(self.resolvedExtenderIps, expandedExtenderIps...)

	return
}

// non-extender dialers are never dropped
type clientDialer struct {
	description string
	// persistent dialers were supplied explicitly and are not discovery cache.
	persistent    bool
	minimumWeight float32
	// 0 is max
	priority int

	// WebSocket upgrades require HTTP/1.1, while ordinary API requests can
	// negotiate HTTP/2. Keep protocol-specific TLS dialers so enabling h2 for
	// the API cannot make gorilla/websocket receive an h2 connection it cannot
	// speak.
	dialTlsContext     DialTlsContextFunction
	httpDialTlsContext DialTlsContextFunction

	extenderConfig *ExtenderConfig

	mutex           sync.Mutex
	successCount    uint64
	errorCount      uint64
	lastSuccessTime time.Time
	lastErrorTime   time.Time

	httpClient      *http.Client
	websocketDialer *websocket.Dialer

	settings *ClientStrategySettings
}

func (self *clientDialer) HttpClient() *http.Client {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.httpClient == nil {
		dialTlsContext := self.httpDialTlsContext
		if dialTlsContext == nil {
			dialTlsContext = self.dialTlsContext
		}
		// control-dial evidence: while an egress interface is forced (the
		// windows service/app providing a tunnel), each api dial logs its
		// local bind address so tester logs prove the escape path. See
		// egress_dial.go; a no-op everywhere else.
		dialTlsContext = wrapControlDial("api", self.settings.ConnectSettings.Log, true, dialTlsContext)
		transport := &http.Transport{
			DialTLSContext:        dialTlsContext,
			IdleConnTimeout:       self.settings.ConnectSettings.IdleConnTimeout,
			TLSHandshakeTimeout:   self.settings.ConnectSettings.TlsTimeout,
			ResponseHeaderTimeout: self.settings.ConnectTimeout,
			ExpectContinueTimeout: self.settings.ConnectTimeout,
			DisableKeepAlives:     false,
			ReadBufferSize:        self.settings.HttpReadBufferSize,
			WriteBufferSize:       self.settings.HttpWriteBufferSize,
			// A custom DialTLSContext disables net/http's automatic HTTP/2
			// attempt unless this is set. ConnectControl and peer-key requests
			// arrive in parallel while a provider window forms; keeping the
			// implicit HTTP/1.1 fallback opened one TLS connection per request,
			// repeatedly paying the pinned P-384 certificate-chain verification
			// and creating visible CPU/pause bursts on mobile. HTTP/2
			// multiplexes those requests over the established connection.
			ForceAttemptHTTP2: true,
		}
		if 0 < self.settings.Http2MaxDecoderHeaderTableSize ||
			0 < self.settings.Http2MaxEncoderHeaderTableSize ||
			0 < self.settings.Http2MaxReceiveBufferPerConnection ||
			0 < self.settings.Http2MaxReceiveBufferPerStream {
			transport.HTTP2 = &http.HTTP2Config{
				MaxDecoderHeaderTableSize:     self.settings.Http2MaxDecoderHeaderTableSize,
				MaxEncoderHeaderTableSize:     self.settings.Http2MaxEncoderHeaderTableSize,
				MaxReceiveBufferPerConnection: self.settings.Http2MaxReceiveBufferPerConnection,
				MaxReceiveBufferPerStream:     self.settings.Http2MaxReceiveBufferPerStream,
			}
		}
		// a custom dial context applies to plain (non-tls) connections;
		// tls connections use the dialTlsContext chain above
		if dialContextSettings := self.settings.ConnectSettings.DialContextSettings; dialContextSettings != nil {
			transport.DialContext = dialContextSettings.DialContext
		}
		self.httpClient = &http.Client{
			Transport: transport,
			Timeout:   self.settings.RequestTimeout,
		}
	}
	return self.httpClient
}

func (self *clientDialer) WsDialer(settings *ClientStrategySettings) *websocket.Dialer {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.websocketDialer == nil {
		var netDialTlsContext DialTlsContextFunction
		if self.dialTlsContext != nil {
			// control-dial evidence for the platform transport dials, same as
			// HttpClient's api tag. See egress_dial.go.
			dialTlsContext := wrapControlDial("platform", settings.ConnectSettings.Log, true, self.dialTlsContext)
			netDialTlsContext = func(
				ctx context.Context,
				network string,
				address string,
			) (net.Conn, error) {
				conn, err := dialTlsContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return NewWebSocketWriteBatchConn(conn), nil
			}
		}
		// pool, size := MessagePool(2048)
		self.websocketDialer = &websocket.Dialer{
			NetDialTLSContext: netDialTlsContext,
			HandshakeTimeout:  settings.HandshakeTimeout,
			ReadBufferSize:    settings.WebSocketReadBufferSize,
			WriteBufferSize:   settings.WebSocketWriteBufferSize,
			// WriteBufferPool: pool,
			EnableCompression: false,
		}
		// a custom dial context applies to plain ws:// connections;
		// wss:// uses the dialTlsContext chain above
		if dialContextSettings := settings.ConnectSettings.DialContextSettings; dialContextSettings != nil {
			self.websocketDialer.NetDialContext = func(
				ctx context.Context,
				network string,
				address string,
			) (net.Conn, error) {
				conn, err := dialContextSettings.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return NewWebSocketWriteBatchConn(conn), nil
			}
		}
	}
	return self.websocketDialer
}

func (self *clientDialer) Weight() float32 {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	c := self.successCount + self.errorCount
	if 0 < c {
		return max(float32(float64(self.successCount)/float64(c)), self.minimumWeight)
	} else {
		return self.minimumWeight
	}
}

func (self *clientDialer) Update(handleCtx context.Context, err error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if err == nil {
		self.successCount += 1
		self.lastSuccessTime = time.Now()
	} else {
		select {
		case <-handleCtx.Done():
			// ignore any error is the context is canceled
		default:
			self.errorCount += 1
			self.lastErrorTime = time.Now()
		}
	}
}

func (self *clientDialer) IsExtender() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.extenderConfig != nil
}

func (self *clientDialer) IsLastSuccess() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return !self.lastSuccessTime.Before(self.lastErrorTime)
}

func (self *clientDialer) String() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.extenderConfig != nil {
		return fmt.Sprintf("extender (%v) success=%d error=%d", self.extenderConfig, self.successCount, self.errorCount)
	} else {
		return fmt.Sprintf("%s success=%d error=%d", self.description, self.successCount, self.errorCount)
	}
}

func (self *clientDialer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.httpClient != nil {
		self.httpClient.CloseIdleConnections()
		self.httpClient = nil
	}
}

type ApiCallback[R any] interface {
	Result(result R, err error)
}

// for internal use
type simpleApiCallback[R any] struct {
	callback func(result R, err error)
}

func NewApiCallback[R any](callback func(result R, err error)) ApiCallback[R] {
	return &simpleApiCallback[R]{
		callback: callback,
	}
}

func NewNoopApiCallback[R any]() ApiCallback[R] {
	return &simpleApiCallback[R]{
		callback: func(result R, err error) {},
	}
}

func (self *simpleApiCallback[R]) Result(result R, err error) {
	self.callback(result, err)
}

type ApiCallbackResult[R any] struct {
	Result R
	Error  error
}

func NewBlockingApiCallback[R any](ctx context.Context) (ApiCallback[R], chan ApiCallbackResult[R]) {
	c := make(chan ApiCallbackResult[R])
	apiCallback := NewApiCallback[R](func(result R, err error) {
		r := ApiCallbackResult[R]{
			Result: result,
			Error:  err,
		}
		select {
		case <-ctx.Done():
		case c <- r:
		}
	})
	return apiCallback, c
}

func HttpPostWithStrategyRaw(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	requestBodyBytes []byte,
	byJwt string,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		"POST",
		requestUrl,
		bytes.NewReader(requestBodyBytes),
	)
	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", "text/json")

	if byJwt != "" {
		auth := fmt.Sprintf("Bearer %s", byJwt)
		request.Header.Add("Authorization", auth)
	}

	helloRequest, err := HelloRequestFromUrl(ctx, requestUrl, byJwt)
	if err != nil {
		return nil, err
	}

	r, err := clientStrategy.HttpSerial(request, helloRequest)
	if err != nil {
		return nil, err
	}

	if http.StatusOK != r.response.StatusCode {
		// the response body is the error message. Typed so a caller can act on the
		// status -- notably 402, whose body carries x402 payment terms.
		return nil, &HttpStatusError{
			StatusCode: r.response.StatusCode,
			Status:     r.response.Status,
			Body:       r.bodyBytes,
		}
	}

	return r.bodyBytes, nil
}

// HttpStatusError carries a non-200 response so callers can act on the STATUS rather
// than string-matching an error message.
//
// This matters for 402: when a network is over its plan, the server answers 402 and
// the body carries x402 payment terms. An agent needs to see the status and the body
// to sign a payment and retry -- an opaque error string is unusable for that.
//
// Error() renders exactly as the untyped error it replaces, so existing callers that
// only log or string-match are unaffected.
type HttpStatusError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (self *HttpStatusError) Error() string {
	return fmt.Sprintf("%s: %s", self.Status, strings.TrimSpace(string(self.Body)))
}

// PaymentRequired reports whether the server answered 402 Payment Required. The Body
// holds the x402 payment terms.
func (self *HttpStatusError) PaymentRequired() bool {
	return self.StatusCode == http.StatusPaymentRequired
}

func HttpPostWithStrategy[R any](
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	args any,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	return HttpPostWithRawFunction(
		ctx,
		func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
			return HttpPostWithStrategyRaw(ctx, clientStrategy, requestUrl, requestBodyBytes, byJwt)
		},
		requestUrl,
		args,
		byJwt,
		result,
		callback,
	)
}

func HttpPostWithRawFunction[R any](
	ctx context.Context,
	httpPostRaw HttpPostRawFunction,
	requestUrl string,
	args any,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	var requestBodyBytes []byte
	if args == nil {
		requestBodyBytes = make([]byte, 0)
	} else {
		var err error
		requestBodyBytes, err = json.Marshal(args)
		if err != nil {
			var empty R
			callback.Result(empty, err)
			return empty, err
		}
	}

	bodyBytes, err := httpPostRaw(ctx, requestUrl, requestBodyBytes, byJwt)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	callback.Result(result, nil)
	return result, nil
}

func HelloRequestFromUrl(ctx context.Context, requestUrl string, byJwt string) (*http.Request, error) {
	u, err := url.Parse(requestUrl)
	if err != nil {
		return nil, err
	}
	helloUrl := fmt.Sprintf("%s://%s/hello", u.Scheme, u.Host)

	req, err := http.NewRequestWithContext(ctx, "GET", helloUrl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "text/json")

	if byJwt != "" {
		auth := fmt.Sprintf("Bearer %s", byJwt)
		req.Header.Add("Authorization", auth)
	}

	return req, nil
}

func HttpGetWithStrategyRaw(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	byJwt string,
) ([]byte, error) {
	settings := clientStrategy.settings

	newRequest := func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
		if err != nil {
			return nil, err
		}

		request.Header.Add("Content-Type", "text/json")

		if byJwt != "" {
			auth := fmt.Sprintf("Bearer %s", byJwt)
			request.Header.Add("Authorization", auth)
		}
		return request, nil
	}

	var statusError *HttpStatusError
	for attempt := 0; attempt <= max(0, settings.GetRetryCount); attempt += 1 {
		if 0 < attempt {
			// jittered pause before the retry
			retryTimeout := settings.GetRetryMinTimeout
			if settings.GetRetryMinTimeout < settings.GetRetryMaxTimeout {
				retryTimeout += time.Duration(mathrand.Int63n(int64(settings.GetRetryMaxTimeout - settings.GetRetryMinTimeout)))
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryTimeout):
			}
		}

		request, err := newRequest()
		if err != nil {
			return nil, err
		}

		r, err := clientStrategy.HttpParallel(request)
		if err != nil {
			return nil, err
		}

		if http.StatusOK == r.response.StatusCode {
			return r.bodyBytes, nil
		}

		// the response body is the error message. Typed so a caller can act on the
		// status -- notably 402, whose body carries x402 payment terms.
		statusError = &HttpStatusError{
			StatusCode: r.response.StatusCode,
			Status:     r.response.Status,
			Body:       r.bodyBytes,
		}
		if !slices.Contains(settings.GetRetryStatusCodes, r.response.StatusCode) {
			return nil, statusError
		}
		// a transient gateway status; pause and retry
	}
	return nil, statusError
}

func HttpGetWithStrategy[R any](
	ctx context.Context,
	clientStrategy *ClientStrategy,
	requestUrl string,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	return HttpGetWithRawFunction[R](
		ctx,
		func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
			return HttpGetWithStrategyRaw(ctx, clientStrategy, requestUrl, byJwt)
		},
		requestUrl,
		byJwt,
		result,
		callback,
	)
}

func HttpGetWithRawFunction[R any](
	ctx context.Context,
	httpGetRaw HttpGetRawFunction,
	requestUrl string,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	bodyBytes, err := httpGetRaw(ctx, requestUrl, byJwt)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}

	callback.Result(result, nil)
	return result, nil
}

/**
 * Streaming POST
 */
func HttpPostStreamWithStrategyRaw(
	ctx context.Context,
	requestUrl string,
	body io.Reader,
	byJwt string,
) ([]byte, error) {

	req, err := http.NewRequestWithContext(ctx, "POST", requestUrl, body)
	if err != nil {
		return nil, err
	}

	if byJwt != "" {
		req.Header.Set("Authorization", "Bearer "+byJwt)
	}

	// NOT http.DefaultClient: on the machine that provides a tunnel, the
	// default transport's unbound sockets and OS name resolution both follow
	// the tun default route into this process's own tunnel (R1). Dial through
	// the connect settings so the socket is egress-bound and the hostname
	// resolves in-process. See egress.go / egress_dial.go; identical behavior
	// everywhere else.
	settings := DefaultConnectSettings()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         wrapControlDial("api", settings.Log, true, settings.DialContext),
			TLSClientConfig:     settings.TlsConfig,
			TLSHandshakeTimeout: settings.TlsTimeout,
			ForceAttemptHTTP2:   true,
		},
	}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	bodyBytes, err := readHttpResponseBody(res, DefaultMaxHttpResponseBodyBytes)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		// typed so a caller can act on the status -- notably 402, whose body carries
		// x402 payment terms
		return nil, &HttpStatusError{
			StatusCode: res.StatusCode,
			Status:     res.Status,
			Body:       bodyBytes,
		}
	}

	return bodyBytes, nil
}

type HttpPostStreamRawFunction func(
	ctx context.Context,
	requestUrl string,
	body io.Reader,
	byJwt string,
) ([]byte, error)

func HttpPostWithStreamFunction[R any](
	ctx context.Context,
	httpPostRaw HttpPostStreamRawFunction,
	requestUrl string,
	body io.Reader,
	byJwt string,
	result R,
	callback ApiCallback[R],
) (R, error) {
	bodyBytes, err := httpPostRaw(ctx, requestUrl, body, byJwt)
	if err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		var empty R
		callback.Result(empty, err)
		return empty, err
	}
	callback.Result(result, nil)
	return result, nil
}
