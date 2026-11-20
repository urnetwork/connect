package connect

// Tests of the waits a platform's refusal asks of a transport's next dial
// (transport_retry_after.go): a 429's Retry-After, read on h1+ and
// WebSocket refusals alike, ends the negotiation instead of falling back at
// once, and holds the next dial at least that long; the platform's QUIC
// connection cap close holds the next h3 dial a jittered 5 to 30 s. Every
// other refusal waits as before.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
)

// Retry-After in seconds or as a date, at most a minute, and nothing for a
// value that isn't one or has passed.
func TestPlatformRetryAfterReadsBothFormsWithinTheCap(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		value string
		want  time.Duration
	}{
		{value: "", want: 0},
		{value: "7", want: 7 * time.Second},
		{value: " 7 ", want: 7 * time.Second},
		{value: "60", want: 60 * time.Second},
		{value: "61", want: PlatformRetryAfterMaxTimeout},
		{value: "999999999999", want: PlatformRetryAfterMaxTimeout},
		{value: "0", want: 0},
		{value: "-5", want: 0},
		{value: "soon", want: 0},
		{value: now.Add(30 * time.Second).Format(http.TimeFormat), want: 30 * time.Second},
		{value: now.Add(time.Hour).Format(http.TimeFormat), want: PlatformRetryAfterMaxTimeout},
		{value: now.Add(-time.Hour).Format(http.TimeFormat), want: 0},
	} {
		header := http.Header{}
		if c.value != "" {
			header.Set("Retry-After", c.value)
		}
		if got := platformRetryAfter(header, now); got != c.want {
			t.Errorf("Retry-After %q = %s, want %s", c.value, got, c.want)
		}
	}
}

// An h1+ refusal with 429 carries its Retry-After, capped, and with one it
// is terminal, so it allows no fresh negotiation; without one, and for any
// other status, the refusal is as before. The error's text is unchanged.
func TestDialFramedUpgradeReadsA429RetryAfter(t *testing.T) {
	for _, c := range []struct {
		name       string
		status     string
		retryAfter string
		statusCode int
		want       time.Duration
		fallback   bool
	}{
		{name: "a 429 with Retry-After", status: "429 Too Many Requests", retryAfter: "7", statusCode: 429, want: 7 * time.Second, fallback: false},
		{name: "a 429 asking for too long", status: "429 Too Many Requests", retryAfter: "120", statusCode: 429, want: PlatformRetryAfterMaxTimeout, fallback: false},
		{name: "a 429 without Retry-After", status: "429 Too Many Requests", statusCode: 429, want: 0, fallback: true},
		{name: "a 429 with an unreadable Retry-After", status: "429 Too Many Requests", retryAfter: "later", statusCode: 429, want: 0, fallback: true},
		{name: "a 503 with Retry-After", status: "503 Service Unavailable", retryAfter: "7", statusCode: 503, want: 0, fallback: true},
	} {
		response := "HTTP/1.1 " + c.status + "\r\nContent-Length: 0\r\n"
		if c.retryAfter != "" {
			response += "Retry-After: " + c.retryAfter + "\r\n"
		}
		raw := newH1UpgradeScriptConn([]byte(response + "\r\n"))
		conn, err := DialFramedUpgrade(context.Background(), "ws://connect.example/", nil, h1UpgradeScriptDialer(raw, nil), H1FramerProtocol)
		if conn != nil {
			conn.Close()
			t.Fatalf("%s: upgraded", c.name)
		}
		var upgradeErr *HTTPUpgradeError
		if !errors.As(err, &upgradeErr) || upgradeErr.StatusCode != c.statusCode || upgradeErr.Reason != "rejected" {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if upgradeErr.Terminal != !c.fallback {
			t.Errorf("%s: terminal %t, want %t", c.name, upgradeErr.Terminal, !c.fallback)
		}
		if upgradeErr.RetryAfter != c.want {
			t.Errorf("%s: RetryAfter %s, want %s", c.name, upgradeErr.RetryAfter, c.want)
		}
		if HTTPUpgradeAllowsFallback(err) != c.fallback {
			t.Errorf("%s: allows fallback %t, want %t", c.name, HTTPUpgradeAllowsFallback(err), c.fallback)
		}
		if want := fmt.Sprintf("HTTP upgrade: rejected (status %d)", c.statusCode); err.Error() != want {
			t.Errorf("%s: %q, want %q", c.name, err.Error(), want)
		}
	}
}

// A platform whose h1+ upgrades are refused with 429 and Retry-After never
// gets a WebSocket negotiation in the same dial: the client waits instead,
// keeps h1+ for the next dial, and counts a handshake failure, not a
// fallback. The refusal's wait is the Retry-After.
func TestDialH1MessagesHonorsA429RetryAfterWithoutFallback(t *testing.T) {
	resetH1UpgradeTestState(t)
	observation := &h1UpgradeObservedRequests{}
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation.record(r)
		w.Header().Set("Retry-After", "9")
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}), false)
	defer server.Close()
	address := "ws" + strings.TrimPrefix(server.URL, "http")
	stats := &H1PlusStats{}
	conn, err := DialH1Messages(context.Background(), address, http.Header{"Authorization": []string{"Bearer synthetic-test"}}, &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, H1FramerProtocol, 65535, true, stats)
	if conn != nil || err == nil {
		t.Fatalf("conn %v, err %v: want the refusal", conn, err)
	}
	if wait := platformDialRefusalWait(err, nil); wait != 9*time.Second {
		t.Fatalf("the refusal's wait = %s, want 9s", wait)
	}
	observation.Lock()
	upgrades := append([]string{}, observation.upgrades...)
	observation.Unlock()
	if len(upgrades) != 1 || upgrades[0] != H1FramerProtocol {
		t.Fatalf("upgrades %v, want the h1+ one only", upgrades)
	}
	if !FramedUpgradePermitted(address, H1FramerProtocol) {
		t.Fatal("a 429 was cached as h1+ unsupported")
	}
	if got := stats.Snapshot(); got.Attempts != 1 || got.Fallbacks != 0 || got.HandshakeFailures != 1 {
		t.Fatalf("counters %+v, want one attempt, a handshake failure and no fallback", got)
	}
}

// A WebSocket refusal with 429 carries its Retry-After too, joined to the
// handshake's own error, so an h1 transport without h1+ waits it out.
func TestDialH1MessagesWebSocketRefusalCarriesItsRetryAfter(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, c := range []struct {
		retryAfter string
		want       time.Duration
	}{
		{retryAfter: "11", want: 11 * time.Second},
		{retryAfter: "", want: 0},
	} {
		server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		}), false)
		address := "ws" + strings.TrimPrefix(server.URL, "http")
		_, err := DialH1Messages(context.Background(), address, http.Header{}, &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, H1FramerProtocol, 65535, false, nil)
		server.Close()
		if !errors.Is(err, websocket.ErrBadHandshake) {
			t.Fatalf("Retry-After %q: err = %v, want the handshake's", c.retryAfter, err)
		}
		var upgradeErr *HTTPUpgradeError
		if !errors.As(err, &upgradeErr) || upgradeErr.StatusCode != http.StatusTooManyRequests || upgradeErr.RetryAfter != c.want {
			t.Fatalf("Retry-After %q: err = %v, want a 429 refusal of %s", c.retryAfter, err, c.want)
		}
		if wait := platformDialRefusalWait(err, nil); wait != c.want {
			t.Fatalf("Retry-After %q: wait %s, want %s", c.retryAfter, wait, c.want)
		}
	}
}

// A 429 with Retry-After ends the strategy's logical dial at once, where a
// failed route would be raced again each round until the request timeout:
// the platform hears one upgrade, and the dial returns the refusal. A 429
// without one is retried round after round, as before.
func TestH1DialContextWithDialerEndsTheDialOnA429RetryAfter(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, c := range []struct {
		retryAfter   string
		manyUpgrades bool
	}{
		{retryAfter: "5", manyUpgrades: false},
		{retryAfter: "", manyUpgrades: true},
	} {
		func() {
			platform := newTestingRetryAfterPlatform(t, c.retryAfter)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settings := DefaultClientStrategySettings()
			settings.EnableNormal = true
			settings.EnableResilient = false
			settings.ReconnectTimeout = 20 * time.Millisecond
			settings.RequestTimeout = 2 * time.Second
			strategy := NewClientStrategy(ctx, settings)
			defer strategy.Close()

			start := time.Now()
			conn, _, err := strategy.H1DialContextWithDialer(ctx, platform.url, http.Header{}, 65535, true, nil)
			if conn != nil {
				conn.Close()
				t.Fatalf("Retry-After %q: dialed", c.retryAfter)
			}
			elapsed := time.Since(start)
			upgradeCount := len(platform.upgrades)
			if c.manyUpgrades {
				if upgradeCount < 3 {
					t.Fatalf("without Retry-After: %d upgrades over the request timeout, want the rounds of a failed route", upgradeCount)
				}
				return
			}
			if upgradeCount != 1 || time.Second < elapsed {
				t.Fatalf("Retry-After %q: %d upgrades in %s, want one and the refusal at once", c.retryAfter, upgradeCount, elapsed)
			}
			if wait := platformDialRefusalWait(err, nil); wait != 5*time.Second {
				t.Fatalf("Retry-After %q: err %v, wait %s, want 5s", c.retryAfter, err, wait)
			}
		}()
	}
}

// The wait a refused dial's error asks for: the longest 429 Retry-After
// among its causes, however the strategy joined or wrapped them, capped;
// for the platform's QUIC connection cap close from the remote, 5 s and a
// draw of up to 25 s more; and none for anything else, a 503's Retry-After,
// a local close, another QUIC code and the client limit close included.
func TestPlatformDialRefusalWait(t *testing.T) {
	rejection := func(statusCode int, retryAfter time.Duration) *HTTPUpgradeError {
		return &HTTPUpgradeError{StatusCode: statusCode, Reason: "rejected", RetryAfter: retryAfter}
	}
	capClose := &quic.ApplicationError{Remote: true, ErrorCode: quicConnectionCapCloseCode, ErrorMessage: "too many connections"}
	noJitter := func(time.Duration) time.Duration { return 0 }
	allJitter := func(maxJitter time.Duration) time.Duration { return maxJitter }

	for _, c := range []struct {
		name string
		err  error
		draw func(time.Duration) time.Duration
		want time.Duration
	}{
		{name: "no error", err: nil, want: 0},
		{name: "a deadline", err: context.DeadlineExceeded, want: 0},
		{name: "a 429", err: rejection(429, 7*time.Second), want: 7 * time.Second},
		{name: "a 429 without Retry-After", err: rejection(429, 0), want: 0},
		{name: "a 503 with a wait", err: rejection(503, 7*time.Second), want: 0},
		{name: "a 401", err: &HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true}, want: 0},
		{name: "a 429 asking for too long", err: rejection(429, time.Hour), want: PlatformRetryAfterMaxTimeout},
		{
			name: "the longest of an exhausted strategy's refusals",
			err:  &HttpRequestExhaustedError{causes: []error{io.EOF, rejection(429, 3*time.Second), errors.Join(errors.New("websocket: bad handshake"), rejection(429, 8*time.Second)), context.DeadlineExceeded}},
			want: 8 * time.Second,
		},
		{name: "a wrapped 429", err: fmt.Errorf("dial: %w", rejection(429, 4*time.Second)), want: 4 * time.Second},
		{name: "the cap close, least jitter", err: capClose, draw: noJitter, want: 5 * time.Second},
		{name: "the cap close, most jitter", err: capClose, draw: allJitter, want: 30*time.Second - 1},
		{name: "the cap close, wrapped", err: fmt.Errorf("h3 open stream: %w", capClose), draw: noJitter, want: 5 * time.Second},
		{name: "the cap close beside a longer 429", err: errors.Join(capClose, rejection(429, 40*time.Second)), draw: noJitter, want: 40 * time.Second},
		{name: "the cap code closed locally", err: &quic.ApplicationError{Remote: false, ErrorCode: quicConnectionCapCloseCode}, draw: noJitter, want: 0},
		{name: "the unknown name close", err: &quic.ApplicationError{Remote: true, ErrorCode: 0x1000}, draw: noJitter, want: 0},
		{name: "the client limit close", err: &quic.ApplicationError{Remote: true, ErrorCode: ClientLimitCloseCode}, draw: noJitter, want: 0},
	} {
		if got := platformDialRefusalWait(c.err, c.draw); got != c.want {
			t.Errorf("%s: wait %s, want %s", c.name, got, c.want)
		}
	}
	// the real draw stays in [5 s, 30 s)
	for range 64 {
		if wait := platformDialRefusalWait(capClose, nil); wait < 5*time.Second || 30*time.Second <= wait {
			t.Fatalf("the cap close's wait %s, want in [5s, 30s)", wait)
		}
	}
}

// The retry after a refused dial waits at least the refusal's wait, on top
// of the transport's ordinary timing: the reconnect timer, the backoff
// after repeated failures, and a pinned transport's timer, whichever is
// longer. Without a refusal's wait the timing is as before.
func TestDialRetryDelayHoldsTheRefusalsWait(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	refusal := &HTTPUpgradeError{StatusCode: http.StatusTooManyRequests, Reason: "rejected", RetryAfter: 2 * time.Second}

	transport := newDialBackoffTestTransport(time.Hour, 4*time.Hour)
	expired := NewReconnect(0)
	if delay := transport.dialRetryDelay(expired, nil); delay != 0 {
		t.Fatalf("no refusal = %s, want the expired timer's 0", delay)
	}
	if delay := transport.dialRetryDelay(expired, refusal); delay != 2*time.Second {
		t.Fatalf("a refusal on the expired timer = %s, want its 2s", delay)
	}
	if delay := transport.dialRetryDelay(NewPacedReconnect(time.Hour), refusal); delay < 59*time.Minute {
		t.Fatalf("a refusal under a longer timer = %s, want the timer's", delay)
	}
	transport.noteDialFailure(refusal)
	transport.noteDialFailure(refusal)
	if delay := transport.dialRetryDelay(expired, refusal); delay < 30*time.Minute {
		t.Fatalf("a refusal under the backoff = %s, want the backoff's", delay)
	}

	pinned := newDialBackoffTestTransport(time.Hour, 4*time.Hour)
	pinned.ipFamily = 4
	if delay := pinned.dialRetryDelay(expired, refusal); delay != 2*time.Second {
		t.Fatalf("pinned, a refusal on the expired timer = %s, want its 2s", delay)
	}

	// the channel waits the delay
	select {
	case <-transport.dialRetryAfter(expired, nil):
		t.Fatal("the backed-off retry fired at once")
	case <-afterDelay(0):
	}
	fresh := newDialBackoffTestTransport(time.Hour, 4*time.Hour)
	select {
	case <-fresh.dialRetryAfter(expired, refusal):
		t.Fatal("the retry after a 2s Retry-After fired within 200ms")
	case <-time.After(200 * time.Millisecond):
	}
}

// One upgrade request the testing platform refused.
type testingRetryAfterUpgrade struct {
	arrived time.Time
	framed  bool
}

// A platform that refuses every h1 upgrade, h1+ and WebSocket alike, with
// 429 and, when set, a Retry-After.
type testingRetryAfterPlatform struct {
	url      string
	upgrades chan testingRetryAfterUpgrade
}

func newTestingRetryAfterPlatform(t *testing.T, retryAfter string) *testingRetryAfterPlatform {
	t.Helper()
	platform := &testingRetryAfterPlatform{
		upgrades: make(chan testingRetryAfterUpgrade, 64),
	}
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case platform.upgrades <- testingRetryAfterUpgrade{arrived: time.Now(), framed: r.Header.Get("Upgrade") == H1FramerProtocol}:
		default:
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}), false)
	platform.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return platform
}

// The next upgrade the platform refused, within timeout.
func (self *testingRetryAfterPlatform) nextUpgrade(t *testing.T, timeout time.Duration) (testingRetryAfterUpgrade, bool) {
	t.Helper()
	select {
	case upgrade := <-self.upgrades:
		return upgrade, true
	case <-time.After(timeout):
		return testingRetryAfterUpgrade{}, false
	}
}

// An h1 transport refused with 429 and Retry-After: 2 dials again no sooner
// than two seconds after the refusal, on h1+ without a WebSocket fallback in
// between, and on WebSocket alone. Refused without Retry-After, h1+ falls
// back to WebSocket at once, as it did.
func TestPlatformTransportH1WaitsOutA429RetryAfter(t *testing.T) {
	resetH1UpgradeTestState(t)
	resetBackendDegraded()
	defer resetBackendDegraded()
	for _, carrier := range []struct {
		name   string
		framed bool
	}{
		{name: "h1+", framed: true},
		{name: "websocket", framed: false},
	} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			platform := newTestingRetryAfterPlatform(t, "2")
			settings := testingPlatformTransportSettings()
			settings.EnableH1Plus = carrier.framed
			newTestingCredentialRefusalTransport(t, ctx, platform.url, settings, TransportModeH1)

			first, ok := platform.nextUpgrade(t, 15*time.Second)
			if !ok || first.framed != carrier.framed {
				t.Fatalf("%s: first upgrade %+v %t", carrier.name, first, ok)
			}
			second, ok := platform.nextUpgrade(t, 15*time.Second)
			if !ok {
				t.Fatalf("%s: no second dial", carrier.name)
			}
			if second.framed != carrier.framed {
				t.Fatalf("%s: the second upgrade was framed %t: a refusal with Retry-After fell back", carrier.name, second.framed)
			}
			if gap := second.arrived.Sub(first.arrived); gap < 2*time.Second {
				t.Fatalf("%s: dialed again %s after a 429 asking for 2s", carrier.name, gap)
			}
			cancel()
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform := newTestingRetryAfterPlatform(t, "")
	settings := testingPlatformTransportSettings()
	settings.EnableH1Plus = true
	newTestingCredentialRefusalTransport(t, ctx, platform.url, settings, TransportModeH1)
	first, ok := platform.nextUpgrade(t, 15*time.Second)
	if !ok || !first.framed {
		t.Fatalf("without Retry-After: first upgrade %+v %t", first, ok)
	}
	fallback, ok := platform.nextUpgrade(t, time.Second)
	if !ok || fallback.framed {
		t.Fatalf("without Retry-After: next upgrade %+v %t, want the WebSocket fallback at once", fallback, ok)
	}
}

// A QUIC platform that closes every connection with the front's connection
// cap code once its handshake completes, as the front does, recording each
// handshake.
type testingConnectionCapH3Platform struct {
	port       int
	nextProto  string
	framerSets *FramerSettings
	handshakes chan time.Time
}

func newTestingConnectionCapH3Platform(t *testing.T) *testingConnectionCapH3Platform {
	t.Helper()
	host := testLoopbackIp(4)
	certPem, keyPem, err := selfSign([]string{host}, host, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	nextProto := "urnetwork-platform-connection-cap-test"
	listener, err := quic.ListenAddrEarly(
		testLoopbackHostPort(4, 0),
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{nextProto},
		},
		&quic.Config{MaxIdleTimeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	platform := &testingConnectionCapH3Platform{
		port:       listener.Addr().(*net.UDPAddr).Port,
		nextProto:  nextProto,
		framerSets: DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit())),
		handshakes: make(chan time.Time, 64),
	}
	var handlers sync.WaitGroup
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			conn, err := listener.Accept(serverCtx)
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				select {
				case <-conn.HandshakeComplete():
				case <-serverCtx.Done():
					return
				}
				select {
				case platform.handshakes <- time.Now():
				default:
				}
				conn.CloseWithError(quicConnectionCapCloseCode, "too many connections")
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		handlers.Wait()
	})
	return platform
}

// An h3 transport closed with the connection cap code right after its
// handshake doesn't dial again for at least the 5 s the close asks of it,
// where it dialed again within a second before.
func TestPlatformTransportH3BacksOffAConnectionCapClose(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform := newTestingConnectionCapH3Platform(t)
	settings := testingFamilyTransportSettings()
	settings.H3Port = platform.port
	settings.QuicConnectTimeout = 2 * time.Second
	settings.QuicHandshakeTimeout = 2 * time.Second
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{platform.nextProto},
	}
	settings.FramerSettings = platform.framerSets
	newTestingCredentialRefusalTransport(t, ctx, "https://"+testLoopbackHost(4), settings, TransportModeH3)

	var first time.Time
	select {
	case first = <-platform.handshakes:
	case <-time.After(15 * time.Second):
		t.Fatal("no h3 handshake")
	}
	select {
	case second := <-platform.handshakes:
		t.Fatalf("dialed again %s after the connection cap close, want at least 5s", second.Sub(first))
	case <-time.After(3 * time.Second):
	}
}
