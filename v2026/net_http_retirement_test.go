package connect

// Retained evaluation snapshots must not outlive the HTTP pool's owning
// configuration. Barriers force removal both before client acquisition and
// during selected-response consumption; every endpoint is synthetic.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Records sweeps independently of request completion so an active response
// cannot make a too-early close look like terminal ownership cleanup.
type retirementTestTransport struct {
	roundTrip  func(*http.Request) (*http.Response, error)
	closeCount atomic.Int32
	onClose    func()
}

// Runs only the in-memory or loopback exchange installed by the fixture.
func (self *retirementTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self.roundTrip(request)
}

// Records the cleanup boundary before delegating to a real pool when present.
func (self *retirementTestTransport) CloseIdleConnections() {
	self.closeCount.Add(1)
	if self.onClose != nil {
		self.onClose()
	}
}

// Separates response headers from complete body consumption without relying
// on a scheduling delay or a negative timeout.
type retirementTestBody struct {
	ctx         context.Context
	started     chan struct{}
	release     <-chan struct{}
	reader      *strings.Reader
	startedOnce sync.Once
}

// Announces body selection before waiting for completion or cancellation.
func (self *retirementTestBody) Read(buffer []byte) (int, error) {
	self.startedOnce.Do(func() { close(self.started) })
	select {
	case <-self.ctx.Done():
		return 0, self.ctx.Err()
	case <-self.release:
		return self.reader.Read(buffer)
	}
}

// The synthetic body owns no resource beyond its request and release barrier.
func (self *retirementTestBody) Close() error { return nil }

// Builds one configured route without any real dial. Tests install its
// transport before evaluating a request.
func newRetirementTestStrategy(t *testing.T) (*ClientStrategy, *clientDialer) {
	t.Helper()
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExtenderDirectory = nil
	settings.ExpandExtenderProfileCount = 0
	settings.ReconnectTimeout = 0
	settings.HelloRetryTimeout = 0
	settings.RequestTimeout = 10 * time.Second
	settings.Log = NewNoopLogger()
	settings.VlessConfigs = []*VlessConfig{{
		Address:  "relay.example",
		Port:     443,
		Id:       testVlessUserId,
		Network:  VlessNetworkTcp,
		Security: VlessSecurityNone,
	}}
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	for dialer := range strategy.dialers {
		return strategy, dialer
	}
	t.Fatal("configured route missing")
	return nil, nil
}

// A response that can complete entirely in memory; using a factory makes a
// removed snapshot's accidental construction observable without a network.
func retirementTestClient(transport *retirementTestTransport) *http.Client {
	if transport.roundTrip == nil {
		transport.roundTrip = func(request *http.Request) (*http.Response, error) {
			if request.Body != nil {
				request.Body.Close()
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(strings.NewReader("ok")),
				ContentLength: 2,
				Request:       request,
			}, nil
		}
	}
	return &http.Client{Transport: transport}
}

// The cloned request body is built after the dialer snapshot was selected,
// but before HttpClient acquisition. Replacing the configuration at that
// exact boundary used to create a pool on an unreachable dialer.
func TestClientStrategyRetiredSnapshotCannotCreateHttpPool(t *testing.T) {
	strategy, retiredDialer := newRetirementTestStrategy(t)
	var retiredFactoryCount atomic.Int32
	retiredDialer.httpClientFactory = func() *http.Client {
		retiredFactoryCount.Add(1)
		return retirementTestClient(&retirementTestTransport{})
	}
	selected := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example/request", strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	var cloneCount atomic.Int32
	request.GetBody = func() (io.ReadCloser, error) {
		if cloneCount.Add(1) == 1 {
			close(selected)
			<-release
		}
		return io.NopCloser(strings.NewReader("input")), nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := strategy.HttpParallel(request)
		done <- err
	}()
	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("request never selected its route")
	}
	strategy.SetVlessConfigs(strategy.VlessConfigs())
	var currentRequestCount atomic.Int32
	for dialer := range strategy.dialers {
		dialer.httpClientFactory = func() *http.Client {
			transport := &retirementTestTransport{}
			client := retirementTestClient(transport)
			roundTrip := transport.roundTrip
			transport.roundTrip = func(request *http.Request) (*http.Response, error) {
				currentRequestCount.Add(1)
				return roundTrip(request)
			}
			return client
		}
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement request did not finish")
	}
	if calls := retiredFactoryCount.Load(); calls != 0 {
		t.Fatalf("removed snapshot constructed %d unowned HTTP pools", calls)
	}
	if calls := currentRequestCount.Load(); calls != 1 {
		t.Fatalf("replacement route requests = %d, want 1", calls)
	}
}

// All three HTTP evaluation sites must transfer their acquired client to
// result cleanup. Retirement while Read is blocked must preserve that body
// and repeat the idle sweep only after it finishes.
func TestClientStrategyRetirementSweepsAfterActiveHttpResponse(t *testing.T) {
	for _, mode := range []string{"parallel", "serial", "hello", "reset"} {
		strategy, dialer := newRetirementTestStrategy(t)
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		transport := &retirementTestTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
			if request.Body != nil {
				request.Body.Close()
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          &retirementTestBody{ctx: request.Context(), started: started, release: release, reader: strings.NewReader("ok")},
				ContentLength: 2,
				Request:       request,
			}, nil
		}}
		dialer.httpClientFactory = func() *http.Client { return retirementTestClient(transport) }
		if mode == "serial" {
			strategy.RecordDeliveryOutcome(strategy.dialerInfo(dialer), deliveryVerifiedByteCount, false)
		}
		request := newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/request")
		hello := newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/hello")
		done := make(chan error, 1)
		go func() {
			var result *httpResult
			var err error
			if mode == "parallel" || mode == "reset" {
				result, err = strategy.HttpParallel(request)
			} else {
				result, err = strategy.HttpSerial(request, hello)
			}
			if err == nil && string(result.bodyBytes) != "ok" {
				err = io.ErrUnexpectedEOF
			}
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: body was not selected", mode)
		}
		if mode == "reset" {
			strategy.CloseIdleConnections()
		} else {
			strategy.SetVlessConfigs(strategy.VlessConfigs())
			for replacement := range strategy.dialers {
				replacement.httpClientFactory = func() *http.Client { return retirementTestClient(&retirementTestTransport{}) }
			}
		}
		if calls := transport.closeCount.Load(); calls != 1 {
			t.Fatalf("%s: early sweeps = %d, want 1", mode, calls)
		}
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: active request did not finish", mode)
		}
		if calls := transport.closeCount.Load(); calls != 2 {
			t.Fatalf("%s: sweeps after body completion = %d, want 2", mode, calls)
		}
		strategy.Close()
	}
}

// A parallel loser can return an error after the winner cancels it. Its
// captured owner still belongs to result cleanup even without a response.
func TestClientStrategyRetiredHttpLoserErrorSweepsOwner(t *testing.T) {
	strategy, winner := newRetirementTestStrategy(t)
	loser := newVlessClientDialer(strategy.settings, winner.vlessConfig)
	strategy.dialers[loser] = true
	winnerStarted := make(chan struct{})
	loserStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	winnerTransport := &retirementTestTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		close(winnerStarted)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	}}
	loserTransport := &retirementTestTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		close(loserStarted)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}
	winner.httpClientFactory = func() *http.Client { return retirementTestClient(winnerTransport) }
	loser.httpClientFactory = func() *http.Client { return retirementTestClient(loserTransport) }
	done := make(chan error, 1)
	request := newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/request")
	go func() { _, err := strategy.HttpParallel(request); done <- err }()
	for _, started := range []<-chan struct{}{winnerStarted, loserStarted} {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel attempt did not start")
		}
	}
	strategy.SetVlessConfigs(nil)
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("winner did not join its canceled loser")
	}
	for i, transport := range []*retirementTestTransport{winnerTransport, loserTransport} {
		if calls := transport.closeCount.Load(); calls != 2 {
			t.Fatalf("attempt %d: final sweeps = %d, want 2", i, calls)
		}
	}
}

// A transport callback can inspect the newly published configuration while
// its old owner closes. No strategy or dialer state lock may cross that call.
func TestClientStrategyRetirementClosesOutsideStateLocks(t *testing.T) {
	strategy, dialer := newRetirementTestStrategy(t)
	transport := &retirementTestTransport{}
	dialer.httpClientFactory = func() *http.Client { return retirementTestClient(transport) }
	dialer.HttpClient()
	transport.onClose = func() {
		if !strategy.mutex.TryLock() {
			t.Error("retirement holds the strategy lock across transport cleanup")
			return
		}
		stillSelected := strategy.dialers[dialer]
		strategy.mutex.Unlock()
		if stillSelected {
			t.Error("old owner still selectable during retirement")
		}
		if !dialer.mutex.TryLock() {
			t.Error("retirement holds the dialer lock across transport cleanup")
			return
		}
		dialer.mutex.Unlock()
		if len(strategy.VlessConfigs()) != 1 {
			t.Error("replacement configuration not published before cleanup")
		}
		if dialer.HttpClient() != nil {
			t.Error("closing owner admitted another pool")
		}
	}
	strategy.SetVlessConfigs(strategy.VlessConfigs())
	if calls := transport.closeCount.Load(); calls != 1 {
		t.Fatalf("terminal sweeps = %d, want 1", calls)
	}
}

// Reports the first selected-body read at the real transport boundary while
// preserving the underlying HTTP/1 or HTTP/2 body's cleanup behavior.
type retirementTestObservedBody struct {
	io.ReadCloser
	started     chan struct{}
	startedOnce sync.Once
}

// Announces selection while leaving native body reads and cleanup intact.
func (self *retirementTestObservedBody) Read(buffer []byte) (int, error) {
	self.startedOnce.Do(func() { close(self.started) })
	return self.ReadCloser.Read(buffer)
}

// Real native HTTP/1 and HTTP/2 sockets must leave a removed owner once the
// selected response completes or cancels. The handler and reader barriers
// force retirement while the connection is active, not already idle.
func TestClientStrategyRetirementClosesNativeHttpSockets(t *testing.T) {
	for _, c := range []struct {
		http2  bool
		cancel bool
	}{{http2: false}, {http2: true}, {http2: false, cancel: true}, {http2: true, cancel: true}} {
		strategy, dialer := newRetirementTestStrategy(t)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		started := make(chan struct{})
		closed := make(chan struct{})
		var closedOnce sync.Once
		server := newFamilyHttptestUnstartedServer(t, 4, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Length", "2")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			select {
			case <-release:
				io.WriteString(writer, "ok")
			case <-request.Context().Done():
			}
		}))
		server.EnableHTTP2 = c.http2
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closedOnce.Do(func() { close(closed) })
			}
		}
		server.StartTLS()
		t.Cleanup(server.Close)
		nativeTransport := server.Client().Transport.(*http.Transport)
		var protocol atomic.Int32
		transport := &retirementTestTransport{
			roundTrip: func(request *http.Request) (*http.Response, error) {
				response, err := nativeTransport.RoundTrip(request)
				if err == nil {
					protocol.Store(int32(response.ProtoMajor))
					response.Body = &retirementTestObservedBody{ReadCloser: response.Body, started: started}
				}
				return response, err
			},
			onClose: nativeTransport.CloseIdleConnections,
		}
		dialer.httpClientFactory = func() *http.Client { return retirementTestClient(transport) }
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request := newClientStrategyLifecycleRequest(t, ctx, server.URL)
		done := make(chan error, 1)
		go func() { _, err := strategy.HttpParallel(request); done <- err }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("%+v: selected body did not start", c)
		}
		if got := protocol.Load(); (got == 2) != c.http2 {
			t.Fatalf("%+v: fixture negotiated HTTP/%d", c, got)
		}
		strategy.SetVlessConfigs(nil)
		if c.cancel {
			cancel()
		} else {
			unblock()
		}
		select {
		case err := <-done:
			if (err != nil) != c.cancel {
				t.Fatalf("%+v: request result = %v", c, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%+v: request did not finish", c)
		}
		if calls := transport.closeCount.Load(); calls != 2 {
			t.Fatalf("%+v: final sweeps = %d, want 2", c, calls)
		}
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("%+v: retired owner retained its native connection", c)
		}
		unblock()
		strategy.Close()
		server.Close()
	}
}

// A canceled selected response still releases the old transport owner, while
// the existing context-owned body cleanup contract remains unchanged.
func TestClientStrategyRetiredHttpResponseCancellationSweepsOwner(t *testing.T) {
	strategy, dialer := newRetirementTestStrategy(t)
	started := make(chan struct{})
	release := make(chan struct{})
	transport := &retirementTestTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       &retirementTestBody{ctx: request.Context(), started: started, release: release, reader: strings.NewReader("ok")},
			Request:    request,
		}, nil
	}}
	dialer.httpClientFactory = func() *http.Client { return retirementTestClient(transport) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := newClientStrategyLifecycleRequest(t, ctx, "https://api.example/request")
	done := make(chan error, 1)
	go func() { _, err := strategy.HttpParallel(request); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("body was not selected")
	}
	strategy.SetVlessConfigs(nil)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not finish")
	}
	if calls := transport.closeCount.Load(); calls != 2 {
		t.Fatalf("canceled owner's idle sweeps = %d, want 2", calls)
	}
}

// Resetting a live route remains reusable, and ordinary successful requests
// do not sweep its pool. Terminal retirement alone rejects new acquisition.
func TestClientStrategyHttpPoolResetRemainsReusable(t *testing.T) {
	strategy, dialer := newRetirementTestStrategy(t)
	var clients []*http.Client
	var transports []*retirementTestTransport
	dialer.httpClientFactory = func() *http.Client {
		transport := &retirementTestTransport{}
		client := retirementTestClient(transport)
		clients = append(clients, client)
		transports = append(transports, transport)
		return client
	}
	for range 2 {
		if _, err := strategy.HttpParallel(newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/request")); err != nil {
			t.Fatal(err)
		}
	}
	if len(clients) != 1 || transports[0].closeCount.Load() != 0 {
		t.Fatal("ordinary requests released the live pooled owner")
	}
	strategy.CloseIdleConnections()
	if _, err := strategy.HttpParallel(newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/request")); err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 || clients[0] == clients[1] || transports[0].closeCount.Load() != 1 || transports[1].closeCount.Load() != 0 {
		t.Fatal("pool reset did not rebuild a reusable owner")
	}
	strategy.Close()
	if client := dialer.HttpClient(); client != nil {
		t.Fatal("terminal strategy close recreated a pooled owner")
	}
	strategy.SetVlessConfigs(strategy.VlessConfigs())
	for retained := range strategy.dialers {
		if retained.HttpClient() != nil {
			t.Fatal("post-close configuration update added a live owner")
		}
	}
}

// The same snapshot rule covers each extender removal path; a retained
// persistent route must remain usable throughout the sweep.
func TestClientStrategyExtenderRemovalRetiresHttpOwners(t *testing.T) {
	for _, mode := range []string{"custom", "expired", "country"} {
		strategy, persistent := newRetirementTestStrategy(t)
		removed := &clientDialer{
			createTime:        time.Now().Add(-time.Hour),
			extenderConfig:    &ExtenderConfig{Ip: netip.MustParseAddr("192.0.2.19")},
			settings:          strategy.settings,
			httpClientFactory: func() *http.Client { return retirementTestClient(&retirementTestTransport{}) },
		}
		persistent.httpClientFactory = func() *http.Client { return retirementTestClient(&retirementTestTransport{}) }
		strategy.dialers[removed] = true
		switch mode {
		case "custom":
			strategy.SetCustomExtenders(nil)
		case "expired":
			strategy.collapseExtenderDialers()
		case "country":
			strategy.settings.ExpandExtenderProfileCount = 1
			strategy.extenderSpoofCountryCode = "old-test-country"
			strategy.expandExtenderDialers()
		}
		if strategy.dialers[removed] {
			t.Fatalf("%s: expired route remains selectable", mode)
		}
		if removed.HttpClient() != nil {
			t.Fatalf("%s: removed route recreated an HTTP owner", mode)
		}
		if !strategy.dialers[persistent] || persistent.HttpClient() == nil {
			t.Fatalf("%s: persistent route was retired", mode)
		}
		strategy.Close()
	}
}
