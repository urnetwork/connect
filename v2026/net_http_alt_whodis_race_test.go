package connect

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/urnetwork/quic-go/v2026"
	"github.com/urnetwork/quic-go/v2026/http3"
)

// The alt whodis dialer's race on a phone (quic_dial_race.go), through the
// production resolver, candidate race, socket factory, dns translation, QUIC
// handshake and HTTP request.

// What one test socket delivers: a datagram and when it arrived, or the read
// error that ended the socket.
type whodisLatencyTestRead struct {
	data        []byte
	arrivalTime time.Time
	err         error
}

// A socket on a path of fixed one-way latency toward the client: it delivers
// each datagram the latency after it arrived, without holding back the
// datagrams behind it. It sends whatever the candidate names to the alt
// fixture, and reports what it reads as from the candidate.
type whodisLatencyTestConn struct {
	raw       net.PacketConn
	server    *net.UDPAddr
	latency   time.Duration
	reads     chan whodisLatencyTestRead
	closed    chan struct{}
	closeOnce sync.Once
	onClose   func()

	stateLock sync.Mutex
	// the candidate address of the first write
	dialed         *net.UDPAddr
	firstWriteTime time.Time
	firstReadTime  time.Time
}

func newWhodisLatencyTestConn(raw net.PacketConn, server *net.UDPAddr, latency time.Duration, onClose func()) *whodisLatencyTestConn {
	conn := &whodisLatencyTestConn{
		raw:     raw,
		server:  server,
		latency: latency,
		reads:   make(chan whodisLatencyTestRead, 1024),
		closed:  make(chan struct{}),
		onClose: onClose,
	}
	go conn.run()
	return conn
}

func (self *whodisLatencyTestConn) run() {
	for {
		b := make([]byte, 2048)
		n, _, err := self.raw.ReadFrom(b)
		read := whodisLatencyTestRead{data: b[:n], arrivalTime: time.Now(), err: err}
		select {
		case self.reads <- read:
		case <-self.closed:
			return
		}
		if err != nil {
			return
		}
	}
}

func (self *whodisLatencyTestConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case read := <-self.reads:
		if read.err != nil {
			return 0, nil, read.err
		}
		if wait := time.Until(read.arrivalTime.Add(self.latency)); 0 < wait {
			select {
			case <-time.After(wait):
			case <-self.closed:
				return 0, nil, net.ErrClosed
			}
		}
		n := copy(b, read.data)
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.firstReadTime.IsZero() {
			self.firstReadTime = time.Now()
		}
		return n, self.dialed, nil
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
}

func (self *whodisLatencyTestConn) WriteTo(b []byte, address net.Addr) (int, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.dialed == nil {
			self.dialed = address.(*net.UDPAddr)
			self.firstWriteTime = time.Now()
		}
	}()
	return self.raw.WriteTo(b, self.server)
}

func (self *whodisLatencyTestConn) Close() error {
	err := net.ErrClosed
	self.closeOnce.Do(func() {
		close(self.closed)
		err = self.raw.Close()
		self.onClose()
	})
	return err
}

func (self *whodisLatencyTestConn) LocalAddr() net.Addr {
	return self.raw.LocalAddr()
}

func (self *whodisLatencyTestConn) SetDeadline(time.Time) error      { return nil }
func (self *whodisLatencyTestConn) SetReadDeadline(time.Time) error  { return nil }
func (self *whodisLatencyTestConn) SetWriteDeadline(time.Time) error { return nil }

// The port the socket dialed, and how long after its first write it first
// heard alt.
func (self *whodisLatencyTestConn) firstAnswer() (int, time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.dialed == nil || self.firstReadTime.IsZero() {
		return 0, 0
	}
	return self.dialed.Port, self.firstReadTime.Sub(self.firstWriteTime)
}

// The stagger at which the old race ended the only attempt, and the whodis
// race's default stagger (TestQuicDialRaceDefaultSchedules). Literals, so this
// test also builds against the old race.
const (
	whodisTestOldStagger = 250 * time.Millisecond
	whodisTestStagger    = 1 * time.Second
)

// A whodis api dial on a phone whose memory budget holds one carrier claim,
// over a path with latency toward the phone. The handshake on 53 first hears
// alt after the old 250 ms family/port stagger and hears it through to
// completion, so the race keeps it: one socket, port 53, open until its
// response, and 4053 never dialed. The old race ended it at the stagger, with
// no response yet, and dialed 4053 on its claim. At 1.2 s of latency the
// handshake is still out at the whodis stagger, where the budget refuses 4053
// and the race keeps 53.
func TestAltWhodisOneClaimKeepsTheHandshakeHearingFromAlt(t *testing.T) {
	for _, latency := range []time.Duration{300 * time.Millisecond, 1200 * time.Millisecond} {
		checkAltWhodisOneClaimKeepsTheHandshake(t, latency)
	}
}

func checkAltWhodisOneClaimKeepsTheHandshake(t *testing.T, latency time.Duration) {
	t.Helper()
	server := newTestAltServer(t, true)
	remote, err := net.ResolveUDPAddr("udp4", strings.TrimPrefix(server.altUrl, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExpandExtenderProfileCount = 0
	// no port: whodis dials 53, then 4053
	settings.AltUrl = "https://alt-whodis-race.test"
	settings.DnsTlds = [][]byte{[]byte(testAltDnsTld)}
	settings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: server.rootCAs, MinVersion: tls.VersionTLS13}
	settings.ConnectSettings.Resolver = newFamilyTestResolver(t, netip.MustParseAddr("127.0.0.1"))
	// the phone policy: a platform transport's settings own the dial, sized
	// for a 20 MiB process, and its budget holds one complete whodis claim
	platformSettings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ctx = (&PlatformTransport{settings: platformSettings}).dialContext(ctx)
	policy := newExtenderQuicMemoryPolicy(ctx, &settings.ConnectSettings)
	policy.packetTranslationSettings()
	budget := NewPlatformTransportBudget(policy.byteCount, 1)
	platformSettings.PlatformTransportBudget = budget

	var socketsLock sync.Mutex
	sockets := []*whodisLatencyTestConn{}
	var live atomic.Int32
	var closedBeforeResponse atomic.Int32
	responded := atomic.Bool{}
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			if budget.Stats().UsedByteCount != policy.byteCount {
				t.Error("socket factory ran without the full carrier claim")
			}
			raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			if live.Add(1) != 1 {
				t.Error("socket lifetime exceeded the one carrier claim")
			}
			conn := newWhodisLatencyTestConn(raw, remote, latency, func() {
				if !responded.Load() {
					closedBeforeResponse.Add(1)
				}
				live.Add(-1)
			})
			socketsLock.Lock()
			defer socketsLock.Unlock()
			sockets = append(sockets, conn)
			return conn, nil
		},
	}
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	client := testAltDialer(t, strategy, "alt whodis").HttpClient()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testAltApiHost+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("latency %s: %v", latency, err)
	}
	responded.Store(true)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != testAltBodyText {
		t.Fatalf("response = %q, %v", body, err)
	}

	socketsLock.Lock()
	opened := append([]*whodisLatencyTestConn{}, sockets...)
	socketsLock.Unlock()
	if len(opened) != 1 || closedBeforeResponse.Load() != 0 {
		t.Fatalf("latency %s: the race ended a whodis handshake that heard alt: sockets=%d closed early=%d", latency, len(opened), closedBeforeResponse.Load())
	}
	port, firstAnswer := opened[0].firstAnswer()
	if port != 53 {
		t.Fatalf("latency %s: the dial answered on %d, want 53", latency, port)
	}
	if whodisTestStagger < latency && firstAnswer <= whodisTestStagger {
		t.Fatalf("latency %s: alt answered in %s, inside the whodis stagger", latency, firstAnswer)
	}
	if firstAnswer <= whodisTestOldStagger {
		t.Fatalf("latency %s: alt answered in %s, inside the old stagger", latency, firstAnswer)
	}

	client.Transport.(*altQuicBoundedTransport).Close()
	deadline := time.Now().Add(5 * time.Second)
	for (live.Load() != 0 || budget.Stats().UsedByteCount != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := budget.Stats()
	if live.Load() != 0 || stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("latency %s: cleanup leaked: sockets=%d budget=%+v", latency, live.Load(), stats)
	}
}

// Both whodis dials report what QUIC reads to the receive clock their
// context carries: the alt whodis dialer's attempt and the platform's h3dns
// attempt each complete a handshake through the dns translation to an
// in-process alt, and the clock has heard alt by then. A dial outside a
// whodis race keeps its packet conn unwrapped, and so does a direct H3 dial
// whose context carries a clock: only a translation is ever wrapped, so QUIC
// keeps a UDP socket's batched and ECN reads.
func TestWhodisDialsFeedTheRaceReceiveClock(t *testing.T) {
	server := newTestAltServer(t, true)
	h3Server := newTestAltServer(t, false)
	remote, err := net.ResolveUDPAddr("udp4", strings.TrimPrefix(server.altUrl, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	h3Remote, err := net.ResolveUDPAddr("udp4", strings.TrimPrefix(h3Server.altUrl, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	tlsConfigFor := func(altServer *testAltServer) *tls.Config {
		return &tls.Config{
			ServerName: testAltApiHost,
			RootCAs:    altServer.rootCAs,
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{http3.NextProtoH3},
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	checkClock := func(description string, dial func(context.Context) (*h3DialAttempt, error)) {
		clock := newQuicDialReceiveClock(nil)
		attempt, err := dial(withQuicDialReceiveClock(ctx, clock))
		if err != nil {
			t.Fatalf("%s: %v", description, err)
		}
		defer attempt.close()
		if _, ok := attempt.packetConn.(*quicDialReceiveClockConn); !ok {
			t.Fatalf("%s: QUIC reads a %T, not the clock's conn", description, attempt.packetConn)
		}
		if !clock.silentSince().After(clock.startTime) {
			t.Fatalf("%s: the handshake completed with the clock hearing nothing", description)
		}
		unwrapped, err := dial(ctx)
		if err != nil {
			t.Fatalf("%s outside a race: %v", description, err)
		}
		defer unwrapped.close()
		if _, ok := unwrapped.packetConn.(*quicDialReceiveClockConn); ok {
			t.Fatalf("%s: a dial outside a whodis race was wrapped", description)
		}
	}
	checkDirect := func(description string, dial func(context.Context) (*h3DialAttempt, error)) {
		clock := newQuicDialReceiveClock(nil)
		attempt, err := dial(withQuicDialReceiveClock(ctx, clock))
		if err != nil {
			t.Fatalf("%s: %v", description, err)
		}
		defer attempt.close()
		if _, ok := attempt.packetConn.(*quicDialReceiveClockConn); ok {
			t.Fatalf("%s: a direct H3 socket was wrapped", description)
		}
		if !clock.silentSince().Equal(clock.startTime) {
			t.Fatalf("%s: a direct H3 dial moved a clock", description)
		}
	}

	clientSettings := DefaultClientStrategySettings()
	policy := newExtenderQuicMemoryPolicy(ctx, &clientSettings.ConnectSettings)
	ptSettings := policy.packetTranslationSettings()
	ptSettings.DnsTlds = [][]byte{[]byte(testAltDnsTld)}
	altDial := func(altRemote *net.UDPAddr, wrap h3PacketConnWrapper, tlsConfig *tls.Config) func(context.Context) (*h3DialAttempt, error) {
		return func(dialCtx context.Context) (*h3DialAttempt, error) {
			reservation, err := policy.acquire(dialCtx)
			if err != nil {
				return nil, err
			}
			return dialAltQuicAttemptWithReservation(
				dialCtx, &clientSettings.ConnectSettings, altRemote, wrap, tlsConfig,
				&quic.Config{HandshakeIdleTimeout: 10 * time.Second}, policy, reservation)
		}
	}
	checkClock("alt whodis", altDial(remote, altWhodisPacketConnWrapper(ptSettings), tlsConfigFor(server)))
	checkDirect("alt h3", altDial(h3Remote, func(_ context.Context, packetConn net.PacketConn) (net.PacketConn, error) {
		return packetConn, nil
	}, tlsConfigFor(h3Server)))

	platformSettings := testingDefaultPlatformTransportSettings()
	platformSettings.AltUrl = server.altUrl
	platformSettings.DnsTlds = [][]byte{[]byte(testAltDnsTld)}
	transport := newTestAltTransport(t, platformSettings)
	_, platformWrap, err := transport.h3DialCandidates(ctx, TransportModeH3Dns, testAltApiHost)
	if err != nil {
		t.Fatal(err)
	}
	checkClock("platform h3dns", func(dialCtx context.Context) (*h3DialAttempt, error) {
		return transport.dialH3(dialCtx, TransportModeH3Dns, testAltApiHost, remote, platformWrap,
			tlsConfigFor(server), newPlatformQuicConfig(platformSettings, 1), 1, true)
	})
	h3Settings := testingDefaultPlatformTransportSettings()
	h3Settings.AltUrl = h3Server.altUrl
	h3Transport := newTestAltTransport(t, h3Settings)
	_, h3Wrap, err := h3Transport.h3DialCandidates(ctx, TransportModeH3, testAltApiHost)
	if err != nil {
		t.Fatal(err)
	}
	checkDirect("platform h3", func(dialCtx context.Context) (*h3DialAttempt, error) {
		return h3Transport.dialH3(dialCtx, TransportModeH3, testAltApiHost, h3Remote, h3Wrap,
			tlsConfigFor(h3Server), newPlatformQuicConfig(h3Settings, 1), 1, true)
	})
}
