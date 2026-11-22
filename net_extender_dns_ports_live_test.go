package connect

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/http3"

	"github.com/urnetwork/connect/protocol"
)

// The extender dns carrier's port race (net_extender_dns_ports.go) through the
// production dial: the race, the platform owner's memory budget, the socket
// factory, the dns translation, QUIC, the H3 request and the extender's
// answer, against an in-process extender dns carrier on a path with latency
// toward the client. Both ports reach the one carrier; a port the path
// blackholes is dropped on the way out.

const extenderDnsLiveTestTld = "x.example."

// The stagger the race used before 2026-11-22 and the stagger and stall
// timeout it uses now (DefaultConnectSettings). Literals, so this file also
// builds against the old race.
const (
	extenderDnsLiveTestOldStagger   = 250 * time.Millisecond
	extenderDnsLiveTestStagger      = 1 * time.Second
	extenderDnsLiveTestStallTimeout = 2 * time.Second
)

// An in-process extender dns carrier: one QUIC server over the decode53
// translation, as the extender runs on each dns socket (EXTENDER.md A1, A6),
// answering every extender request with an empty response frame.
type testExtenderDnsCarrier struct {
	addr *net.UDPAddr
}

func newTestExtenderDnsCarrier(t *testing.T) *testExtenderDnsCarrier {
	t.Helper()
	certPem, keyPem, err := selfSign([]string{"extender.example"}, "extender-dns-test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	responseFrame, err := ExtenderResponseFrame(&protocol.ExtenderResponse{})
	if err != nil {
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ptSettings := DefaultPacketTranslationSettings()
	ptSettings.DnsTlds = [][]byte{[]byte(extenderDnsLiveTestTld)}
	translation, err := NewPacketTranslation(ctx, PacketTranslationModeDecode53, packetConn, ptSettings)
	if err != nil {
		cancel()
		packetConn.Close()
		t.Fatal(err)
	}
	h3Server := &http3.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(responseFrame)
		}),
		IdleTimeout: 30 * time.Second,
	}
	quicTransport := &quic.Transport{Conn: translation}
	listener, err := quicTransport.Listen(
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{http3.NextProtoH3},
		},
		&quic.Config{MaxIdleTimeout: 30 * time.Second},
	)
	if err != nil {
		cancel()
		quicTransport.Close()
		translation.Close()
		t.Fatal(err)
	}
	go func() {
		for {
			quicConn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				defer quicConn.CloseWithError(0, "")
				h3Server.ServeQUICConn(quicConn)
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		quicTransport.Close()
		translation.Close()
	})
	return &testExtenderDnsCarrier{addr: packetConn.LocalAddr().(*net.UDPAddr)}
}

// A client socket on a path with fixed latency toward the client
// (whodisLatencyTestConn) that drops whatever it would send to a port in
// `blackholedPorts`, as a network that blackholes the port does.
type extenderDnsPathTestConn struct {
	*whodisLatencyTestConn
	blackholedPorts []int
	openTime        time.Time
	closeTime       atomic.Pointer[time.Time]
}

func (self *extenderDnsPathTestConn) WriteTo(b []byte, address net.Addr) (int, error) {
	udpAddr := address.(*net.UDPAddr)
	if !slices.Contains(self.blackholedPorts, udpAddr.Port) {
		return self.whodisLatencyTestConn.WriteTo(b, address)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.dialed == nil {
		self.dialed = udpAddr
		self.firstWriteTime = time.Now()
	}
	return len(b), nil
}

// The port the socket dialed, zero before its first write.
func (self *extenderDnsPathTestConn) dialedPort() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.dialed == nil {
		return 0
	}
	return self.dialed.Port
}

// One extender dns dial through the production race and carrier.
type extenderDnsLiveTestDial struct {
	// the ports the dial races, in order
	dnsPorts []int
	// the carrier claims the owner's budget admits
	claims          int
	latency         time.Duration
	blackholedPorts []int
	timeout         time.Duration
	// a receive clock the caller's context carries, nil for none
	callerClock *quicDialReceiveClock

	startTime time.Time
	endTime   time.Time
	err       error
	// the sockets the dial opened, in order
	sockets []*extenderDnsPathTestConn
	// the most sockets open at once
	maxLive int32
	// sockets closed before the dial returned
	closedEarly int32
}

// Runs the dial and checks that it released every socket and claim.
func (self *extenderDnsLiveTestDial) run(t *testing.T, carrier *testExtenderDnsCarrier) {
	t.Helper()
	connectSettings := DefaultConnectSettings()
	// the platform owner of the dial, as a platform transport's dials carry it
	platformSettings := testingDefaultPlatformTransportSettings()
	ctx, cancel := context.WithTimeout(t.Context(), self.timeout)
	defer cancel()
	ctx = (&PlatformTransport{settings: platformSettings}).dialContext(ctx)
	if self.callerClock != nil {
		ctx = withQuicDialReceiveClock(ctx, self.callerClock)
	}
	policy := newExtenderQuicMemoryPolicy(ctx, connectSettings)
	policy.packetTranslationSettings()
	budget := NewPlatformTransportBudget(policy.byteCount*ByteCount(self.claims), self.claims)
	platformSettings.PlatformTransportBudget = budget

	var socketsLock sync.Mutex
	var live atomic.Int32
	var closedEarly atomic.Int32
	var returned atomic.Bool
	connectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			liveCount := live.Add(1)
			if int(liveCount) > self.claims {
				t.Errorf("%d sockets open on a budget of %d claims", liveCount, self.claims)
			}
			conn := &extenderDnsPathTestConn{blackholedPorts: self.blackholedPorts, openTime: time.Now()}
			conn.whodisLatencyTestConn = newWhodisLatencyTestConn(raw, carrier.addr, self.latency, func() {
				closeTime := time.Now()
				conn.closeTime.Store(&closeTime)
				if !returned.Load() {
					closedEarly.Add(1)
				}
				live.Add(-1)
			})
			socketsLock.Lock()
			defer socketsLock.Unlock()
			self.sockets = append(self.sockets, conn)
			self.maxLive = max(self.maxLive, liveCount)
			return conn, nil
		},
	}
	extenderConfig := &ExtenderConfig{
		Ip: netip.MustParseAddr("127.0.0.1"),
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeDns,
			Port:        self.dnsPorts[0],
			DnsTld:      extenderDnsLiveTestTld,
		},
		DnsPorts: self.dnsPorts,
	}
	self.startTime = time.Now()
	conn, response, err := DialExtender(ctx, connectSettings, extenderConfig, &ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
	})
	returned.Store(true)
	self.endTime = time.Now()
	self.err = err
	self.closedEarly = closedEarly.Load()
	if err == nil {
		if response == nil {
			t.Error("the dial connected with no extender response")
		}
		conn.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for (live.Load() != 0 || budget.Stats().UsedByteCount != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := budget.Stats()
	if live.Load() != 0 || stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("cleanup leaked: sockets=%d budget=%+v", live.Load(), stats)
	}
}

// The ports the dial's sockets dialed, in order.
func (self *extenderDnsLiveTestDial) dialedPorts() []int {
	ports := []int{}
	for _, socket := range self.sockets {
		ports = append(ports, socket.dialedPort())
	}
	return ports
}

// A dns dial on a phone whose budget holds one carrier claim, to an extender
// that answers on 53, the first port, over a path with latency toward the
// phone. The handshake on 53 first hears the extender after the old 250 ms
// stagger, and at 1.2 s after the 1 s stagger, and hears it through to the
// extender's answer, so the race keeps it: one socket, port 53, open until the
// dial returns, and 4053 never opened.
func TestExtenderDnsLiveOneClaimKeepsTheHandshakeHearingTheExtender(t *testing.T) {
	carrier := newTestExtenderDnsCarrier(t)
	for _, latency := range []time.Duration{300 * time.Millisecond, 1200 * time.Millisecond} {
		dial := &extenderDnsLiveTestDial{
			dnsPorts: []int{DefaultDnsPort, ExtenderDnsPort},
			claims:   1,
			latency:  latency,
			timeout:  20 * time.Second,
		}
		dial.run(t, carrier)
		if dial.err != nil {
			t.Fatalf("latency %s: %v", latency, dial.err)
		}
		if ports := dial.dialedPorts(); !slices.Equal(ports, []int{DefaultDnsPort}) || dial.closedEarly != 0 {
			t.Fatalf("latency %s: the race ended a handshake that heard the extender: ports=%v closed early=%d",
				latency, ports, dial.closedEarly)
		}
		_, firstAnswer := dial.sockets[0].firstAnswer()
		if firstAnswer <= extenderDnsLiveTestOldStagger {
			t.Fatalf("latency %s: the extender answered in %s, inside the old stagger", latency, firstAnswer)
		}
		if extenderDnsLiveTestStagger < latency && firstAnswer <= extenderDnsLiveTestStagger {
			t.Fatalf("latency %s: the extender answered in %s, inside the stagger", latency, firstAnswer)
		}
	}
}

// With room for two claims, a healthy handshake on the first port that takes
// longer than the old 250 ms stagger and less than the 1 s stagger is the only
// one: the old race opened the second port beside it at 250 ms on nearly every
// dial, and abandoned that handshake when the first port answered.
func TestExtenderDnsLiveTwoClaimsOpenOneHealthyHandshake(t *testing.T) {
	carrier := newTestExtenderDnsCarrier(t)
	for _, dnsPorts := range [][]int{{DefaultDnsPort, ExtenderDnsPort}, {ExtenderDnsPort, DefaultDnsPort}} {
		dial := &extenderDnsLiveTestDial{
			dnsPorts: dnsPorts,
			claims:   2,
			latency:  100 * time.Millisecond,
			timeout:  20 * time.Second,
		}
		dial.run(t, carrier)
		if dial.err != nil {
			t.Fatalf("ports %v: %v", dnsPorts, dial.err)
		}
		elapsed := dial.endTime.Sub(dial.startTime)
		if elapsed <= extenderDnsLiveTestOldStagger {
			t.Fatalf("ports %v: the dial took %s, inside the old stagger", dnsPorts, elapsed)
		}
		if ports := dial.dialedPorts(); !slices.Equal(ports, dnsPorts[:1]) || dial.closedEarly != 0 {
			t.Fatalf("ports %v: a %s dial opened ports %v, closed early=%d, want the first port alone",
				dnsPorts, elapsed, ports, dial.closedEarly)
		}
	}
}

// A dns dial on a phone whose budget holds one carrier claim, over a network
// that blackholes the first port. The first port's attempt hears nothing, so
// it gives up the only claim at the stall timeout, and the second port, which
// the budget refused at the stagger, is opened then and answers. The old race
// kept the silent attempt for its whole QUIC handshake bound, so the second
// port was never opened within the dial's deadline. Both port orders.
func TestExtenderDnsLiveOneClaimEndsABlackholedPortForTheNext(t *testing.T) {
	carrier := newTestExtenderDnsCarrier(t)
	for _, dnsPorts := range [][]int{{DefaultDnsPort, ExtenderDnsPort}, {ExtenderDnsPort, DefaultDnsPort}} {
		dial := &extenderDnsLiveTestDial{
			dnsPorts:        dnsPorts,
			claims:          1,
			latency:         50 * time.Millisecond,
			blackholedPorts: dnsPorts[:1],
			timeout:         8 * time.Second,
		}
		dial.run(t, carrier)
		if dial.err != nil {
			t.Fatalf("ports %v: the blackholed port held the only claim: %v (ports=%v)", dnsPorts, dial.err, dial.dialedPorts())
		}
		if ports := dial.dialedPorts(); !slices.Equal(ports, dnsPorts) || dial.maxLive != 1 {
			t.Fatalf("ports %v: opened %v, %d at once, want each port once and one at a time", dnsPorts, ports, dial.maxLive)
		}
		silent, next := dial.sockets[0], dial.sockets[1]
		if port, _ := silent.firstAnswer(); port != 0 {
			t.Fatalf("ports %v: the blackholed port heard the extender", dnsPorts)
		}
		closeTime := silent.closeTime.Load()
		if closeTime == nil || next.openTime.Before(*closeTime) {
			t.Fatalf("ports %v: the next port opened before the silent one gave up its claim", dnsPorts)
		}
		// the silent attempt ends at the stall timeout, not at the stagger
		if yielded := closeTime.Sub(silent.openTime); yielded < extenderDnsLiveTestStallTimeout || extenderDnsLiveTestStallTimeout+time.Second < yielded {
			t.Fatalf("ports %v: the silent port gave up its claim after %s, want the stall timeout", dnsPorts, yielded)
		}
	}
}

// A dns carrier dial reports what QUIC reads from its translation to the
// receive clock its context carries, which is how the race hears an attempt.
func TestExtenderDnsLiveDialFeedsTheAttemptClock(t *testing.T) {
	carrier := newTestExtenderDnsCarrier(t)
	connectSettings := DefaultConnectSettings()
	connectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			return newWhodisLatencyTestConn(raw, carrier.addr, 0, func() {}), nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	clock := newQuicDialReceiveClock(nil)
	extenderConfig := &ExtenderConfig{
		Ip:      netip.MustParseAddr("127.0.0.1"),
		Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeDns, Port: ExtenderDnsPort, DnsTld: extenderDnsLiveTestTld},
	}
	headerBytes, err := extenderRequestHeaderBytes(extenderConfig, &ExtenderDial{DestinationHost: "dest.example", DestinationPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialExtenderQuic(withQuicDialReceiveClock(ctx, clock), connectSettings, extenderConfig,
		newExtenderTlsConfig(extenderConfig), headerBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !clock.silentSince().After(clock.startTime) {
		t.Fatal("the dial heard the extender's answer with its clock hearing nothing")
	}
}

// The race's clocks are its own. A receive clock the caller's context carries
// (a platform h3dns dial's gate lease, which times what that dial hears from
// alt) hears nothing of the extender's carrier, on a race of two ports and on
// a dial of one.
func TestExtenderDnsLiveRaceLeavesTheCallerClock(t *testing.T) {
	carrier := newTestExtenderDnsCarrier(t)
	for _, dnsPorts := range [][]int{{DefaultDnsPort, ExtenderDnsPort}, {ExtenderDnsPort}} {
		callerClock := newQuicDialReceiveClock(nil)
		dial := &extenderDnsLiveTestDial{
			dnsPorts:    dnsPorts,
			claims:      2,
			latency:     50 * time.Millisecond,
			timeout:     20 * time.Second,
			callerClock: callerClock,
		}
		dial.run(t, carrier)
		if dial.err != nil {
			t.Fatalf("ports %v: %v", dnsPorts, dial.err)
		}
		if !callerClock.silentSince().Equal(callerClock.startTime) {
			t.Fatalf("ports %v: the extender's carrier moved the caller's clock", dnsPorts)
		}
	}
}
