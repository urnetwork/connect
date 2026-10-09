package connect

// Pool retirement follows evaluation ownership, including late native dials,
// multiplexed responses, and transport cleanup after response consumption.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Counts underlying closure without needing a scheduler-dependent peer read.
type retirementPoolTestConn struct {
	net.Conn
	closeCount atomic.Int32
}

// The owner closes the underlying stream exactly once.
func (self *retirementPoolTestConn) Close() error {
	self.closeCount.Add(1)
	return self.Conn.Close()
}

// Pipe endpoints give a real connection boundary with no host network.
func newRetirementPoolTestConn(t *testing.T) *retirementPoolTestConn {
	t.Helper()
	conn, peer := net.Pipe()
	t.Cleanup(func() { conn.Close(); peer.Close() })
	return &retirementPoolTestConn{Conn: conn}
}

// Waits only for a positive fixture event, with a failure bound.
func waitRetirementPoolEvent(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatal("pool lifecycle fixture did not reach its barrier")
	}
}

// Retirement must preserve every admitted response, not just the first one
// that finishes, and must reject an old snapshot after the last lease ends.
func TestClientHttpPoolRetirementJoinsLeases(t *testing.T) {
	pool := newClientHttpPool()
	pool.httpClient = retirementTestClient(&retirementTestTransport{})
	if !pool.acquire() || !pool.acquire() {
		t.Fatal("live owner refused a lease")
	}
	conn := newRetirementPoolTestConn(t)
	owned, err := pool.register(conn)
	if err != nil {
		t.Fatal(err)
	}
	pool.retire()
	if conn.closeCount.Load() != 0 || pool.acquire() {
		t.Fatal("retirement closed an active owner or admitted another lease")
	}
	pool.release()
	if conn.closeCount.Load() != 0 {
		t.Fatal("first lease closed another response's connection")
	}
	pool.release()
	if conn.closeCount.Load() != 1 || !pool.drained || len(pool.conns) != 0 {
		t.Fatal("last lease retained the old native stream")
	}
	owned.Close()
	pool.retire()
	if conn.closeCount.Load() != 1 {
		t.Fatal("repeated retirement closed the stream twice")
	}
	late := newRetirementPoolTestConn(t)
	if _, err := pool.register(late); !errors.Is(err, errClientDialerRetired) || late.closeCount.Load() != 1 {
		t.Fatalf("late registration err=%v close=%d", err, late.closeCount.Load())
	}
}

// Normal pool reuse retains only live connections, never their closed history.
func TestClientHttpPoolClosedStreamsLeaveRegistry(t *testing.T) {
	pool := newClientHttpPool()
	pool.httpClient = retirementTestClient(&retirementTestTransport{})
	for range 3 {
		conn := newRetirementPoolTestConn(t)
		owned, err := pool.register(conn)
		if err != nil || len(pool.conns) != 1 {
			t.Fatalf("register err=%v entries=%d", err, len(pool.conns))
		}
		owned.Close()
		if len(pool.conns) != 0 || conn.closeCount.Load() != 1 {
			t.Fatal("closed native stream stayed rooted")
		}
	}
	if !pool.acquire() {
		t.Fatal("ordinary connection closes retired the pool")
	}
	pool.release()
	pool.retire()
}

// An evaluation admitted before retirement may establish another connection
// for a redirect; registration and the final drain cannot lose that stream.
func TestClientHttpPoolAdmittedLateDial(t *testing.T) {
	pool := newClientHttpPool()
	pool.httpClient = retirementTestClient(&retirementTestTransport{})
	if !pool.acquire() {
		t.Fatal("missing lease")
	}
	conn := newRetirementPoolTestConn(t)
	var calls atomic.Int32
	dial := pool.dialContext(func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return conn, nil
	}, false)
	pool.retire()
	if _, err := dial(t.Context(), "tcp", "api.example:443"); err != nil {
		t.Fatalf("admitted redirect dial: %v", err)
	}
	if conn.closeCount.Load() != 0 {
		t.Fatal("active redirect stream closed early")
	}
	pool.release()
	if conn.closeCount.Load() != 1 {
		t.Fatal("redirect stream escaped final drain")
	}
	if _, err := dial(t.Context(), "tcp", "api.example:443"); !errors.Is(err, errClientDialerRetired) || calls.Load() != 1 {
		t.Fatalf("drained dial gate err=%v calls=%d", err, calls.Load())
	}
}

// net/http can detach a dial from the request that wanted it. The owner
// cancels that work, and even a dial ignoring cancellation cannot publish.
func TestClientHttpPoolDrainedDetachedDial(t *testing.T) {
	for _, secure := range []bool{false, true} {
		pool := newClientHttpPool()
		pool.httpClient = retirementTestClient(&retirementTestTransport{})
		conn := newRetirementPoolTestConn(t)
		started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		dial := pool.dialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			if secure {
				return ownClientHttpPoolConn(ctx, conn)
			}
			return conn, nil
		}, secure)
		done := make(chan error, 1)
		go func() { _, err := dial(t.Context(), "tcp", "api.example:443"); done <- err }()
		waitRetirementPoolEvent(t, started)
		pool.retire()
		waitRetirementPoolEvent(t, canceled)
		unblock()
		select {
		case err := <-done:
			if !errors.Is(err, errClientDialerRetired) || conn.closeCount.Load() != 1 || len(pool.conns) != 0 {
				t.Fatalf("secure=%t: late dial err=%v close=%d entries=%d", secure, err, conn.closeCount.Load(), len(pool.conns))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("detached dial did not finish")
		}
	}
}

// Two selected responses on the same native HTTP/2 socket survive removal;
// only the last consumed response releases the retired transport's stream.
func TestClientStrategyRetirementPreservesOtherActiveResponse(t *testing.T) {
	strategy, dialer := newRetirementTestStrategy(t)
	entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var releaseOnce [2]sync.Once
	unblock := func(i int) { releaseOnce[i].Do(func() { close(release[i]) }) }
	defer unblock(0)
	defer unblock(1)
	closed := make(chan struct{})
	var closeOnce sync.Once
	var connections atomic.Int32
	server := newFamilyHttptestUnstartedServer(t, 4, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		i := 0
		if request.URL.Path == "/second" {
			i = 1
		}
		writer.Header().Set("Content-Length", "2")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		close(entered[i])
		select {
		case <-release[i]:
			io.WriteString(writer, "ok")
		case <-request.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
		if state == http.StateClosed {
			closeOnce.Do(func() { close(closed) })
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	strategy.settings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	dialer.httpDialTlsContext = newNormalDialTlsContext(strategy.settings, clientHttpNextProtos)
	done := []chan error{make(chan error, 1), make(chan error, 1)}
	for i, path := range []string{"/first", "/second"} {
		request := newClientStrategyLifecycleRequest(t, t.Context(), server.URL+path)
		go func() {
			result, err := strategy.HttpParallel(request)
			if err == nil && (result.response.ProtoMajor != 2 || string(result.bodyBytes) != "ok") {
				err = errors.New("invalid native response")
			}
			done[i] <- err
		}()
		waitRetirementPoolEvent(t, entered[i])
	}
	if connections.Load() != 1 {
		t.Fatalf("responses used %d sockets, want one multiplexed socket", connections.Load())
	}
	strategy.SetVlessConfigs(nil)
	unblock(0)
	select {
	case err := <-done[0]:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first response did not finish")
	}
	select {
	case <-closed:
		t.Fatal("first response closed another active response's socket")
	default:
	}
	unblock(1)
	select {
	case err := <-done[1]:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second response did not finish")
	}
	waitRetirementPoolEvent(t, closed)
}

// An admitted http.Client redirect keeps its old owner lease across the new
// destination's dial, then closes both sockets after final body consumption.
func TestClientStrategyRetirementPreservesAdmittedRedirect(t *testing.T) {
	strategy, dialer := newRetirementTestStrategy(t)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	closed := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var closedOnce [2]sync.Once
	target := newFamilyHttptestUnstartedServer(t, 4, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { io.WriteString(writer, "ok") }))
	source := newFamilyHttptestUnstartedServer(t, 4, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-release:
			http.Redirect(writer, request, target.URL, http.StatusFound)
		case <-request.Context().Done():
		}
	}))
	for i, server := range []*http.Server{source.Config, target.Config} {
		server.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closedOnce[i].Do(func() { close(closed[i]) })
			}
		}
	}
	target.StartTLS()
	t.Cleanup(target.Close)
	source.StartTLS()
	t.Cleanup(source.Close)
	strategy.settings.TlsConfig = source.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	dialer.httpDialTlsContext = newNormalDialTlsContext(strategy.settings, clientHttpNextProtos)
	request := newClientStrategyLifecycleRequest(t, t.Context(), source.URL)
	done := make(chan error, 1)
	go func() {
		result, err := strategy.HttpParallel(request)
		if err == nil && string(result.bodyBytes) != "ok" {
			err = errors.New("redirect response was not consumed")
		}
		done <- err
	}()
	waitRetirementPoolEvent(t, started)
	strategy.SetVlessConfigs(nil)
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted redirect did not finish")
	}
	for _, event := range closed {
		waitRetirementPoolEvent(t, event)
	}
}

// Reset and pressure cleanup callbacks can reenter strategy/dialer readers;
// neither lifecycle boundary may call an external transport under its lock.
func TestClientStrategyPoolCleanupOutsideStateLocks(t *testing.T) {
	for _, reset := range []bool{false, true} {
		strategy, dialer := newRetirementTestStrategy(t)
		transport := &retirementTestTransport{}
		dialer.httpClientFactory = func() *http.Client { return retirementTestClient(transport) }
		client := dialer.HttpClient()
		transport.onClose = func() {
			strategy.VlessConfigs()
			dialer.Weight()
			if !reset && dialer.HttpClient() != client {
				t.Error("pressure replaced the reusable client")
			}
		}
		done := make(chan struct{})
		go func() {
			if reset {
				strategy.CloseIdleConnections()
			} else {
				strategy.shedMemory()
			}
			close(done)
		}()
		waitRetirementPoolEvent(t, done)
		transport.onClose = nil
		strategy.Close()
	}
}

// The ownership wrapper sits above the resilient layer, preserving its
// concrete TCP socket controls while native TLS still negotiates HTTP/2.
func TestClientHttpPoolResilientTlsSeams(t *testing.T) {
	for _, c := range []struct{ fragment, reorder bool }{
		{fragment: true, reorder: true},
		{fragment: true},
		{reorder: true},
	} {
		for _, ipVersion := range testIpVersions {
			if ipVersion == 6 {
				requireIpv6Loopback(t)
			}
			strategy, dialer := newRetirementTestStrategy(t)
			server := newFamilyHttptestUnstartedServer(t, ipVersion, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				io.WriteString(writer, "ok")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)
			strategy.settings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			dialer.httpDialTlsContext = newResilientDialTlsContext(&strategy.settings.ConnectSettings, c.fragment, c.reorder, false, clientHttpNextProtos)
			if _, ok := dialer.HttpClient().Transport.(*http.Transport); !ok {
				t.Fatal("native concrete transport was replaced")
			}
			result, err := strategy.HttpParallel(newClientStrategyLifecycleRequest(t, t.Context(), server.URL))
			if err != nil || result.response.ProtoMajor != 2 || string(result.bodyBytes) != "ok" {
				t.Fatalf("v%d %+v: resilient result=%v err=%v", ipVersion, c, result, err)
			}
			// An IP URL omits SNI, so assert the exact wrapper boundary
			// needed by the fragment/reorder branch's concrete TCP controls.
			pool := dialer.httpPool
			retained, concrete := func() (int, bool) {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()
				for owned := range pool.conns {
					resilient, ok := owned.Conn.(*ResilientTlsConn)
					if !ok {
						return len(pool.conns), false
					}
					if _, ok := resilient.conn.(*net.TCPConn); !ok {
						return len(pool.conns), false
					}
				}
				return len(pool.conns), true
			}()
			if retained != 1 || !concrete {
				t.Fatalf("v%d %+v: native wrapper chain entries=%d concreteTCP=%t", ipVersion, c, retained, concrete)
			}
			strategy.Close()
			server.Close()
		}
	}
}

// The extra pool lease is measured on a warm in-memory transport, where
// scheduler and network costs cannot hide its allocation count.
func BenchmarkClientStrategyRetirementEval(b *testing.B) {
	for _, serial := range []bool{false, true} {
		name := "parallel"
		if serial {
			name = "serial"
		}
		b.Run(name, func(b *testing.B) {
			settings := DefaultClientStrategySettings()
			settings.EnableResilient = false
			settings.ExtenderDirectory = nil
			settings.ExpandExtenderProfileCount = 0
			settings.Log = NewNoopLogger()
			strategy := NewClientStrategy(b.Context(), settings)
			b.Cleanup(strategy.Close)
			for dialer := range strategy.dialers {
				dialer.httpClientFactory = func() *http.Client { return retirementTestClient(&retirementTestTransport{}) }
			}
			request, err := http.NewRequestWithContext(b.Context(), http.MethodGet, "https://api.example/benchmark", strings.NewReader("input"))
			if err != nil {
				b.Fatal(err)
			}
			run := func() {
				var err error
				if serial {
					_, err = strategy.HttpSerial(request, request)
				} else {
					_, err = strategy.HttpParallel(request)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			run()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
		})
	}
}
