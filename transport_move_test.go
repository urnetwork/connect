package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

// One H1 upgrade the move test server accepted.
type testMoveH1Request struct {
	host        string
	moveVersion string
	conn        *websocket.Conn
}

// A local TLS WebSocket server standing in for a platform edge. It presents a
// certificate for `certificateHosts` and records the server name each client
// hello asked for and each upgrade's host and move header. The tests reach it
// only through a dial seam that maps a documentation address onto it.
type testMoveH1Server struct {
	address     string
	roots       *x509.CertPool
	serverNames chan string
	requests    chan testMoveH1Request
}

func newTestMoveH1Server(t *testing.T, certificateHosts ...string) *testMoveH1Server {
	t.Helper()
	certPem, keyPem, err := selfSign(certificateHosts, "move-test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return newTestMoveH1ServerWithCertificate(t, certPem, keyPem)
}

// A second edge that presents the same certificate, so one root pool trusts
// both.
func newTestMoveH1ServerWithCertificate(t *testing.T, certPem []byte, keyPem []byte) *testMoveH1Server {
	t.Helper()
	certificate, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPem) {
		t.Fatal("could not add the test certificate root")
	}
	self := &testMoveH1Server{
		roots:       roots,
		serverNames: make(chan string, 64),
		requests:    make(chan testMoveH1Request, 64),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		self.requests <- testMoveH1Request{
			host:        request.Host,
			moveVersion: request.Header.Get(HeaderResidentMoveVersion),
			conn:        conn,
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case self.serverNames <- hello.ServerName:
			default:
			}
			return nil, nil
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	self.address = server.Listener.Addr().String()
	return self
}

// A client strategy whose every stream dial goes through `dial`, and whose
// TLS trusts the test server's root.
func newTestMoveStrategy(
	t *testing.T,
	ctx context.Context,
	roots *x509.CertPool,
	dial DialContextFunction,
) *ClientStrategy {
	t.Helper()
	strategy := NewClientStrategy(ctx, testMoveStrategySettings(roots, dial))
	t.Cleanup(strategy.Close)
	return strategy
}

func testMoveStrategySettings(roots *x509.CertPool, dial DialContextFunction) *ClientStrategySettings {
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = true
	settings.EnableResilient = false
	settings.RequestTimeout = time.Second
	settings.ConnectTimeout = time.Second
	settings.TlsConfig = &tls.Config{RootCAs: roots}
	settings.DialContextSettings = &DialContextSettings{DialContext: dial}
	return settings
}

// A dial seam that routes the named addresses to a test server and refuses
// every other address, recording each address asked for.
type testMoveDialSeam struct {
	stateLock sync.Mutex
	// dial address to the test server's address, "" for no route
	routes    map[string]string
	addresses []string
}

func newTestMoveDialSeam(serverAddress string, routableAddresses ...string) *testMoveDialSeam {
	self := &testMoveDialSeam{
		routes: map[string]string{},
	}
	for _, address := range routableAddresses {
		self.routes[address] = serverAddress
	}
	return self
}

func (self *testMoveDialSeam) setRoutable(address string, routable bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !routable {
		self.routes[address] = ""
		return
	}
	for _, serverAddress := range self.routes {
		if serverAddress != "" {
			self.routes[address] = serverAddress
			return
		}
	}
}

func (self *testMoveDialSeam) setRoute(address string, serverAddress string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.routes[address] = serverAddress
}

func (self *testMoveDialSeam) dialedAddresses() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.addresses)
}

func (self *testMoveDialSeam) resetDialedAddresses() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addresses = nil
}

func (self *testMoveDialSeam) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	serverAddress := func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addresses = append(self.addresses, address)
		return self.routes[address]
	}()
	if serverAddress == "" {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("test: no route to this address")}
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", serverAddress)
}

func testMoveTargetAddress(targetIp netip.Addr) string {
	return net.JoinHostPort(targetIp.String(), "443")
}

func testMoveHostAddress() string {
	return net.JoinHostPort(testMoveHost, "443")
}

func receiveTestMoveValue[T any](t *testing.T, values <-chan T, message string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal(message)
		var empty T
		return empty
	}
}

// The H1 dial override: a move replacement dials only its target address,
// while the TLS server name and the HTTP host stay the platform hostname, so
// the certificate is verified for the hostname. Its arrival records the
// preference, and the connection declares the move version.
func TestPlatformTransportH1MoveDialKeepsTheHostname(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	seam := newTestMoveDialSeam(server.address, testMoveTargetAddress(testMoveTargetIpv4))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	settings := testingPlatformTransportSettings()
	tracker := NewResidentMoveTracker()
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		settings,
		tracker,
		time.Now(),
	)
	settings.ResidentMoveTracker = tracker
	settings.ResidentMoveDial = plan.Dial
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-h1"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	request := receiveTestMoveValue(t, server.requests, "the move target was not reached")
	if request.host != testMoveHost {
		t.Fatalf("HTTP host = %q, want %q", request.host, testMoveHost)
	}
	if request.moveVersion != "1" {
		t.Fatalf("move version header = %q, want 1", request.moveVersion)
	}
	if serverName := receiveTestMoveValue(t, server.serverNames, "no client hello"); serverName != testMoveHost {
		t.Fatalf("TLS server name = %q, want %q", serverName, testMoveHost)
	}
	if !waitForCondition(10*time.Second, plan.Dial.Arrived) {
		t.Fatal("the replacement registered without an arrival")
	}
	for _, address := range seam.dialedAddresses() {
		if address != testMoveTargetAddress(testMoveTargetIpv4) {
			t.Fatalf("dialed %q, want only the move target", address)
		}
	}
	if got := tracker.preferredTargetIps(testMoveHost, 0, time.Now()); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("preference after the arrival = %v", got)
	}
}

// A target whose certificate is not the platform hostname's fails the TLS
// check: the move fails with a TLS reason and the replacement never connects.
func TestPlatformTransportH1MoveDialVerifiesTheHostnameCertificate(t *testing.T) {
	server := newTestMoveH1Server(t, "other.space.example")
	seam := newTestMoveDialSeam(server.address, testMoveTargetAddress(testMoveTargetIpv4))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	settings := testingPlatformTransportSettings()
	tracker := NewResidentMoveTracker()
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		settings,
		tracker,
		time.Now(),
	)
	settings.ResidentMoveTracker = tracker
	settings.ResidentMoveDial = plan.Dial
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-h1-certificate"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	if serverName := receiveTestMoveValue(t, server.serverNames, "no client hello"); serverName != testMoveHost {
		t.Fatalf("TLS server name = %q, want %q", serverName, testMoveHost)
	}
	if !waitForCondition(15*time.Second, func() bool {
		_, failed := plan.Dial.Failure()
		return failed
	}) {
		t.Fatal("the certificate failure was not counted")
	}
	failure, _ := plan.Dial.Failure()
	if failure.Reason != protocol.ResidentMoveReason_ResidentMoveReasonTlsError || failure.IpFamily != 4 || failure.Elapsed <= 0 {
		t.Fatalf("failure = %+v, want a v4 TLS failure", failure)
	}
	if transport.IsConnected() || plan.Dial.Arrived() {
		t.Fatal("a target with the wrong certificate connected")
	}
	if !tracker.TargetFailedWithin(testMoveTargetIpv4, time.Hour, time.Now()) {
		t.Fatal("the failed target is not held")
	}
	select {
	case request := <-server.requests:
		t.Fatalf("an upgrade reached the server: %+v", request)
	default:
	}
}

// A pinned transport dials the target of its family only (IPV6.md A4), and
// its arrival prefers the target for that family.
func TestPlatformTransportPinnedMoveDialUsesItsFamilyTarget(t *testing.T) {
	restoreProbe := swapControlFamilyProbe(func(int) bool { return true })
	defer restoreProbe()
	server := newTestMoveH1Server(t, testMoveHost)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracker := NewResidentMoveTracker()
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4, testMoveTargetIpv6),
		[]ResidentMoveTransport{testDirectMoveTransport(4), testDirectMoveTransport(6)},
		DefaultPlatformTransportSettings(),
		tracker,
		time.Now(),
	)
	for _, c := range []struct {
		ipFamily  int
		targetIp  netip.Addr
		otherIp   netip.Addr
		routeName string
	}{
		{ipFamily: 4, targetIp: testMoveTargetIpv4, otherIp: testMoveTargetIpv6, routeName: "move-pinned-4"},
		{ipFamily: 6, targetIp: testMoveTargetIpv6, otherIp: testMoveTargetIpv4, routeName: "move-pinned-6"},
	} {
		seam := newTestMoveDialSeam(server.address, testMoveTargetAddress(c.targetIp), testMoveTargetAddress(c.otherIp))
		strategy := NewDirectClientStrategy(ctx, testMoveStrategySettings(server.roots, seam.dial), c.ipFamily)
		settings := testingFamilyTransportSettings()
		settings.IpFamily = c.ipFamily
		settings.ResidentMoveTracker = tracker
		settings.ResidentMoveDial = plan.Dial
		transport := NewPlatformTransportWithTargetMode(
			ctx,
			strategy,
			NewRouteManager(ctx, c.routeName),
			"wss://"+testMoveHost,
			testingFamilyAuth(),
			TransportModeH1,
			settings,
		)
		request := receiveTestMoveValue(t, server.requests, "the pinned transport did not reach its target")
		if request.host != testMoveHost {
			t.Fatalf("ipv%d HTTP host = %q", c.ipFamily, request.host)
		}
		if !waitForCondition(10*time.Second, transport.IsConnected) {
			t.Fatalf("ipv%d transport did not connect", c.ipFamily)
		}
		for _, address := range seam.dialedAddresses() {
			if address != testMoveTargetAddress(c.targetIp) {
				t.Fatalf("ipv%d transport dialed %q, want only %s", c.ipFamily, address, c.targetIp)
			}
		}
		if got := tracker.preferredTargetIps(testMoveHost, c.ipFamily, time.Now()); !slices.Equal(got, []netip.Addr{c.targetIp}) {
			t.Fatalf("ipv%d preference = %v", c.ipFamily, got)
		}
		transport.Close()
		strategy.Close()
	}
}

// Stickiness (D10): a reconnect dials the preferred target first; a failed
// dial to it drops the preference, so the next dial resolves the name; a
// preference past its end is not used.
func TestPlatformTransportPrefersTheMoveTargetForReconnects(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	targetAddress := testMoveTargetAddress(testMoveTargetIpv4)
	seam := newTestMoveDialSeam(server.address, targetAddress, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)

	tracker := NewResidentMoveTracker()
	tracker.preferTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4}, time.Now().Add(time.Hour))
	settings := testingPlatformTransportSettings()
	settings.ResidentMoveTracker = tracker
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-prefer"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	first := receiveTestMoveValue(t, server.requests, "the preferred target was not dialed")
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{targetAddress}) {
		t.Fatalf("first dial = %v, want the preferred target", got)
	}

	// a reconnect keeps the preference
	seam.resetDialedAddresses()
	first.conn.Close()
	second := receiveTestMoveValue(t, server.requests, "the reconnect did not reach the preferred target")
	if got := seam.dialedAddresses(); 0 == len(got) || !slices.Contains(got, targetAddress) || slices.Contains(got, testMoveHostAddress()) {
		t.Fatalf("reconnect dials = %v, want the preferred target only", got)
	}

	// the target fails: that dial drops the preference and the next one
	// resolves the name
	seam.setRoutable(targetAddress, false)
	seam.resetDialedAddresses()
	second.conn.Close()
	receiveTestMoveValue(t, server.requests, "the reconnect did not fall back to the name")
	dialed := seam.dialedAddresses()
	if !slices.Contains(dialed, targetAddress) || dialed[len(dialed)-1] != testMoveHostAddress() {
		t.Fatalf("fallback dials = %v, want the target and then the name", dialed)
	}
	if got := tracker.preferredTargetIps(testMoveHost, 0, time.Now()); 0 < len(got) {
		t.Fatalf("a failed preference was kept: %v", got)
	}
	transport.Close()

	// a preference past its end resolves the name from the first dial
	seam.setRoutable(targetAddress, true)
	seam.resetDialedAddresses()
	expired := NewResidentMoveTracker()
	expired.preferTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4}, time.Now().Add(-time.Second))
	expiredSettings := testingPlatformTransportSettings()
	expiredSettings.ResidentMoveTracker = expired
	expiredTransport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-prefer-expired"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		expiredSettings,
	)
	defer expiredTransport.Close()
	receiveTestMoveValue(t, server.requests, "the expired preference blocked the name")
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{testMoveHostAddress()}) {
		t.Fatalf("dials with an expired preference = %v, want the name", got)
	}
}

// Without moves enabled, the transport declares no move version and ignores
// a preference.
func TestPlatformTransportWithoutMovesDeclaresNothing(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	seam := newTestMoveDialSeam(server.address, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)
	tracker := NewResidentMoveTracker()
	tracker.preferTargets(testMoveHost, []netip.Addr{testMoveTargetIpv4}, time.Now().Add(time.Hour))
	settings := testingPlatformTransportSettings()
	settings.EnableMove = false
	settings.ResidentMoveTracker = tracker
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-disabled"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()
	request := receiveTestMoveValue(t, server.requests, "the transport did not connect")
	if request.moveVersion != "" {
		t.Fatalf("move version header = %q without moves", request.moveVersion)
	}
	if got := seam.dialedAddresses(); !slices.Equal(got, []string{testMoveHostAddress()}) {
		t.Fatalf("dials = %v, want the name only", got)
	}
}

// The frame auth declares the move version: H3 (every H3 mode builds the same
// frame) and the legacy H1 first frame.
func TestPlatformTransportFrameAuthDeclaresTheMoveVersion(t *testing.T) {
	for _, enableMove := range []bool{true, false} {
		want := uint32(0)
		if enableMove {
			want = ResidentMoveVersion
		}
		authVersions := make(chan uint32, 4)
		observeAuth := func(authFrameBytes []byte) {
			if decoded, err := DecodeFrame(authFrameBytes); err == nil {
				if auth, ok := decoded.(*protocol.Auth); ok {
					select {
					case authVersions <- auth.ResidentMoveVersion:
					default:
					}
				}
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		h3Settings := testingPlatformTransportSettings()
		h3Settings.EnableMove = enableMove
		h3Settings.AuthFrameObserver = observeAuth
		h3Settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
			return nil, errors.New("test: the auth frame is all this test needs")
		}
		h3Transport := NewPlatformTransportWithTargetMode(
			ctx,
			NewClientStrategyWithDefaults(ctx),
			NewRouteManager(ctx, "move-h3-auth"),
			"https://"+testMoveHost,
			testingFamilyAuth(),
			TransportModeH3,
			h3Settings,
		)
		if got := receiveTestMoveValue(t, authVersions, "the H3 auth frame was not built"); got != want {
			t.Fatalf("enable=%t H3 resident move version = %d, want %d", enableMove, got, want)
		}
		h3Transport.Close()

		server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, request, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}), false)
		h1Settings := testingPlatformTransportSettings()
		h1Settings.EnableMove = enableMove
		h1Settings.V2H1Auth = false
		h1Settings.AuthFrameObserver = observeAuth
		strategySettings := DefaultClientStrategySettings()
		strategySettings.EnableNormal = true
		strategySettings.EnableResilient = false
		strategy := NewClientStrategy(ctx, strategySettings)
		h1Transport := NewPlatformTransportWithTargetMode(
			ctx,
			strategy,
			NewRouteManager(ctx, "move-h1-legacy-auth"),
			"ws"+server.URL[len("http"):],
			testingFamilyAuth(),
			TransportModeH1,
			h1Settings,
		)
		if got := receiveTestMoveValue(t, authVersions, "the legacy H1 auth frame was not built"); got != want {
			t.Fatalf("enable=%t legacy H1 resident move version = %d, want %d", enableMove, got, want)
		}
		h1Transport.Close()
		strategy.Close()
		server.Close()
		cancel()
	}
}

// A packet connection that sends to `targetAddr` reaches `serverAddr`, and
// reads from the server appear to come from the target, as a QUIC dial to a
// target address would see an edge. Every destination is recorded.
type testMoveMappedPacketConn struct {
	net.PacketConn
	targetAddr *net.UDPAddr
	serverAddr *net.UDPAddr
	stateLock  sync.Mutex
	addresses  []string
}

func (self *testMoveMappedPacketConn) WriteTo(packet []byte, addr net.Addr) (int, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addresses = append(self.addresses, addr.String())
	}()
	if addr.String() == self.targetAddr.String() {
		addr = self.serverAddr
	}
	return self.PacketConn.WriteTo(packet, addr)
}

func (self *testMoveMappedPacketConn) ReadFrom(packet []byte) (int, net.Addr, error) {
	n, addr, err := self.PacketConn.ReadFrom(packet)
	if addr != nil && addr.String() == self.serverAddr.String() {
		addr = self.targetAddr
	}
	return n, addr, err
}

func (self *testMoveMappedPacketConn) dialedAddresses() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.addresses)
}

// A local QUIC platform edge: it answers the transport's auth frame with an
// echo and records the server name each client hello asked for, and the
// resident move version of each auth frame.
type testMoveH3Server struct {
	addr         *net.UDPAddr
	roots        *x509.CertPool
	serverNames  chan string
	moveVersions chan uint32
}

const testMoveH3NextProto = "urnetwork-move-test"

func newTestMoveH3Server(t *testing.T, certificateHosts ...string) *testMoveH3Server {
	t.Helper()
	certPem, keyPem, err := selfSign(certificateHosts, "move-test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPem) {
		t.Fatal("could not add the test certificate root")
	}
	self := &testMoveH3Server{
		roots:        roots,
		serverNames:  make(chan string, 64),
		moveVersions: make(chan uint32, 64),
	}
	listener, err := quic.ListenAddrEarly(
		"127.0.0.1:0",
		&tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{testMoveH3NextProto},
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				select {
				case self.serverNames <- hello.ServerName:
				default:
				}
				return nil, nil
			},
		},
		&quic.Config{MaxIdleTimeout: 10 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		serverCancel()
		listener.Close()
	})
	self.addr = listener.Addr().(*net.UDPAddr)
	framerSettings := DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
	go func() {
		for {
			connection, err := listener.Accept(serverCtx)
			if err != nil {
				return
			}
			go func() {
				stream, err := connection.AcceptStream(serverCtx)
				if err != nil {
					return
				}
				framer := NewFramer(framerSettings)
				authBytes, err := framer.Read(stream)
				if err != nil {
					return
				}
				if decoded, err := DecodeFrame(authBytes); err == nil {
					if auth, ok := decoded.(*protocol.Auth); ok {
						select {
						case self.moveVersions <- auth.ResidentMoveVersion:
						default:
						}
					}
				}
				err = framer.Write(stream, authBytes)
				MessagePoolReturn(authBytes)
				if err != nil {
					return
				}
				<-connection.Context().Done()
			}()
		}
	}()
	return self
}

// H3 settings that reach `server` only through the target address.
func testMoveH3Settings(t *testing.T, server *testMoveH3Server, targetIp netip.Addr) (*PlatformTransportSettings, func() []string) {
	t.Helper()
	settings := testingPlatformTransportSettings()
	settings.QuicTlsConfig = &tls.Config{
		RootCAs:    server.roots,
		NextProtos: []string{testMoveH3NextProto},
	}
	settings.H3Port = 443
	targetAddr := &net.UDPAddr{IP: net.IP(targetIp.AsSlice()), Port: 443}
	var stateLock sync.Mutex
	packetConns := []*testMoveMappedPacketConn{}
	settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
		packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		mapped := &testMoveMappedPacketConn{
			PacketConn: packetConn,
			targetAddr: targetAddr,
			serverAddr: server.addr,
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		packetConns = append(packetConns, mapped)
		return mapped, nil
	}
	dialedAddresses := func() []string {
		stateLock.Lock()
		defer stateLock.Unlock()
		addresses := []string{}
		for _, packetConn := range packetConns {
			addresses = append(addresses, packetConn.dialedAddresses()...)
		}
		return addresses
	}
	return settings, dialedAddresses
}

// The H3 dial override: a move replacement dials the target address on the
// platform port with the server name unchanged, so QUIC verifies the platform
// hostname's certificate, and its auth frame declares the move version.
func TestPlatformTransportH3MoveDialKeepsTheServerName(t *testing.T) {
	server := newTestMoveH3Server(t, testMoveHost)
	settings, dialedAddresses := testMoveH3Settings(t, server, testMoveTargetIpv4)
	tracker := NewResidentMoveTracker()
	h3Transport := testDirectMoveTransport(0)
	h3Transport.activeMode = TransportModeH3
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{h3Transport},
		settings,
		tracker,
		time.Now(),
	)
	settings.ResidentMoveTracker = tracker
	settings.ResidentMoveDial = plan.Dial
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "move-h3"),
		"https://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH3,
		settings,
	)
	defer transport.Close()

	if serverName := receiveTestMoveValue(t, server.serverNames, "the H3 target was not reached"); serverName != testMoveHost {
		t.Fatalf("QUIC server name = %q, want %q", serverName, testMoveHost)
	}
	if version := receiveTestMoveValue(t, server.moveVersions, "no auth frame"); version != ResidentMoveVersion {
		t.Fatalf("auth resident move version = %d, want %d", version, ResidentMoveVersion)
	}
	if !waitForCondition(10*time.Second, plan.Dial.Arrived) {
		t.Fatal("the H3 replacement did not arrive at its target")
	}
	targetAddress := testMoveTargetAddress(testMoveTargetIpv4)
	for _, address := range dialedAddresses() {
		if address != targetAddress {
			t.Fatalf("H3 sent to %q, want only %s", address, targetAddress)
		}
	}
	if got := tracker.preferredTargetIps(testMoveHost, 4, time.Now()); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("preference after the H3 arrival = %v", got)
	}
}

// QUIC verifies the platform hostname at the target: a target with another
// name's certificate fails the move with a TLS reason.
func TestPlatformTransportH3MoveDialVerifiesTheServerNameCertificate(t *testing.T) {
	server := newTestMoveH3Server(t, "other.space.example")
	settings, _ := testMoveH3Settings(t, server, testMoveTargetIpv4)
	h3Transport := testDirectMoveTransport(0)
	h3Transport.activeMode = TransportModeH3
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{h3Transport},
		settings,
		NewResidentMoveTracker(),
		time.Now(),
	)
	settings.ResidentMoveDial = plan.Dial
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "move-h3-certificate"),
		"https://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH3,
		settings,
	)
	defer transport.Close()
	if !waitForCondition(15*time.Second, func() bool {
		_, failed := plan.Dial.Failure()
		return failed
	}) {
		t.Fatal("the H3 certificate failure was not counted")
	}
	failure, _ := plan.Dial.Failure()
	if failure.Reason != protocol.ResidentMoveReason_ResidentMoveReasonTlsError || failure.IpFamily != 4 {
		t.Fatalf("failure = %+v, want a v4 TLS failure", failure)
	}
	if transport.IsConnected() || plan.Dial.Arrived() {
		t.Fatal("a target with the wrong certificate connected over H3")
	}
}

// A strict move's replacement holds the carriers that cannot reach a target
// -- the DNS carriers, and H3 through an extender -- until the move concludes.
// H1 and direct H3 dial.
func TestPlatformTransportStrictMoveHoldsCarriersThatCannotUseATarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		DefaultPlatformTransportSettings(),
		NewResidentMoveTracker(),
		time.Now(),
	)
	direct := NewClientStrategyWithDefaults(ctx)
	defer direct.Close()
	extenderOnlySettings := DefaultClientStrategySettings()
	extenderOnlySettings.EnableNormal = false
	extenderOnlySettings.EnableResilient = false
	extenderOnlySettings.ExtenderConfigs = []*ExtenderConfig{{Ip: netip.MustParseAddr("203.0.113.40")}}
	extenderOnly := NewClientStrategy(ctx, extenderOnlySettings)
	defer extenderOnly.Close()
	if extenderOnly.H3ExtenderConfig() == nil {
		t.Fatal("the extender-only strategy has a direct H3 path")
	}

	transport := &PlatformTransport{
		clientStrategy:   direct,
		settings:         DefaultPlatformTransportSettings(),
		residentMoveDial: plan.Dial,
	}
	for _, mode := range []TransportMode{TransportModeH1, TransportModeH3} {
		if !transport.waitMoveCarrierAdmission(ctx, mode) {
			t.Fatalf("%s was held", mode)
		}
	}
	extenderTransport := &PlatformTransport{
		clientStrategy:   extenderOnly,
		settings:         DefaultPlatformTransportSettings(),
		residentMoveDial: plan.Dial,
	}
	held := []func() bool{
		func() bool { return transport.waitMoveCarrierAdmission(ctx, TransportModeH3Dns) },
		func() bool { return transport.waitMoveCarrierAdmission(ctx, TransportModeH3DnsPump) },
		func() bool { return extenderTransport.waitMoveCarrierAdmission(ctx, TransportModeH3) },
	}
	released := make(chan bool, len(held))
	for _, wait := range held {
		go func() {
			released <- wait()
		}()
	}
	select {
	case <-released:
		t.Fatal("a carrier that cannot use a target dialed during a strict move")
	case <-time.After(50 * time.Millisecond):
	}
	plan.Dial.Conclude()
	for range held {
		if !receiveTestMoveValue(t, released, "the conclusion did not release a held carrier") {
			t.Fatal("a released carrier reported its context ended")
		}
	}

	// a move that is not strict holds nothing
	compulsory := PlanResidentMove(
		testCompulsoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		DefaultPlatformTransportSettings(),
		NewResidentMoveTracker(),
		time.Now(),
	)
	compulsoryTransport := &PlatformTransport{
		clientStrategy:   direct,
		settings:         DefaultPlatformTransportSettings(),
		residentMoveDial: compulsory.Dial,
	}
	if !compulsoryTransport.waitMoveCarrierAdmission(ctx, TransportModeH3Dns) {
		t.Fatal("a compulsory move held a DNS carrier")
	}
}

// A strict move dial refuses the relay dialers before they dial, so it
// reaches the target direct or not at all; a dial that is not strict lets a
// relay carry it by name.
func TestStrictMoveDialRefusesRelayDialers(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	vlessAddress := net.JoinHostPort("203.0.113.30", "443")
	for _, strict := range []bool{true, false} {
		// the target refuses, so every dialer the eval allows is tried
		seam := newTestMoveDialSeam(server.address)
		ctx, cancel := context.WithCancel(context.Background())
		strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)
		strategy.SetVlessConfigs([]*VlessConfig{{
			Address: "203.0.113.30",
			Port:    443,
			Id:      "move-test",
		}})
		dialCtx := withMoveDialOverride(ctx, &moveDialOverride{
			host:      testMoveHost,
			targetIps: []netip.Addr{testMoveTargetIpv4},
			strict:    strict,
		})
		_, _, err := strategy.H1DialContextWithDialer(
			dialCtx,
			"wss://"+testMoveHost,
			http.Header{},
			64*1024,
			false,
			nil,
		)
		if err == nil {
			t.Fatalf("strict=%t dial connected through a refused target", strict)
		}
		dialed := seam.dialedAddresses()
		if !slices.Contains(dialed, testMoveTargetAddress(testMoveTargetIpv4)) {
			t.Fatalf("strict=%t direct dialer did not dial the target: %v", strict, dialed)
		}
		if slices.Contains(dialed, testMoveHostAddress()) {
			t.Fatalf("strict=%t a dial resolved the name: %v", strict, dialed)
		}
		if strict == slices.Contains(dialed, vlessAddress) {
			t.Fatalf("strict=%t relay dials = %v", strict, dialed)
		}
		strategy.Close()
		cancel()
	}
}

// The dialer info marks a relay -- an extender or a VLESS server -- and no
// direct dialer.
func TestDialerInfoMarksRelays(t *testing.T) {
	settings := DefaultClientStrategySettings()
	vless := newVlessClientDialer(settings, &VlessConfig{Address: "203.0.113.30", Port: 443, Id: "move-test"})
	if vless == nil || !vless.Info().relay || !vless.relays() {
		t.Fatal("a VLESS dialer is not a relay")
	}
	extender := &clientDialer{extenderConfig: &ExtenderConfig{Ip: netip.MustParseAddr("203.0.113.40")}}
	if !extender.Info().relay || !extender.relays() {
		t.Fatal("an extender dialer is not a relay")
	}
	direct := &clientDialer{description: "normal"}
	if direct.Info().relay || direct.relays() {
		t.Fatal("a direct dialer is a relay")
	}
}

// A relayed connection is counted for exactly its lifetime, and the release is
// idempotent.
func TestPlatformTransportCountsRelayedConnections(t *testing.T) {
	transport := &PlatformTransport{}
	releaseDirect := transport.holdRelayedConnection(false)
	if transport.RelayCarried() {
		t.Fatal("a direct connection counted as relayed")
	}
	release := transport.holdRelayedConnection(true)
	if !transport.RelayCarried() {
		t.Fatal("a relayed connection did not count")
	}
	release()
	release()
	releaseDirect()
	if transport.RelayCarried() || transport.relayedConnectionCount.Load() != 0 {
		t.Fatalf("relayed count = %d after release", transport.relayedConnectionCount.Load())
	}
}

// The move override applies only to the hostname it stands in for, with or
// without a port, case-insensitively.
func TestMoveDialOverrideMatchesOnlyItsHost(t *testing.T) {
	override := &moveDialOverride{host: testMoveHost, targetIps: []netip.Addr{testMoveTargetIpv4, testMoveTargetIpv6}}
	for address, want := range map[string]bool{
		testMoveHostAddress():                     true,
		"CONNECT.space.example:443":               true,
		testMoveHost:                              true,
		"other.space.example:443":                 false,
		testMoveTargetAddress(testMoveTargetIpv4): false,
	} {
		if got := override.matches(address); got != want {
			t.Errorf("matches(%q) = %t, want %t", address, got, want)
		}
	}
	var none *moveDialOverride
	if none.matches(testMoveHostAddress()) {
		t.Fatal("a nil override matched")
	}
	if got := override.targetIpsForNetwork("tcp4"); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("tcp4 targets = %v", got)
	}
	if got := override.targetIpsForNetwork("tcp6"); !slices.Equal(got, []netip.Addr{testMoveTargetIpv6}) {
		t.Fatalf("tcp6 targets = %v", got)
	}
	if got := override.udpAddrs(6, 443); len(got) != 1 || got[0].String() != "[2001:db8::10]:443" {
		t.Fatalf("v6 udp candidates = %v", got)
	}
	if _, err := (&moveDialOverride{host: testMoveHost, targetIps: []netip.Addr{testMoveTargetIpv6}}).dialTargets(
		context.Background(), "tcp4", testMoveHostAddress(),
		func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialed a target the network cannot carry")
			return nil, nil
		},
	); !errors.Is(err, errResidentMoveNoFamilyTarget) {
		t.Fatalf("v4 dial of a v6 target = %v", err)
	}
}

// A move's targets are fixed addresses, as a literal is: a handshake that
// stalls at a target neither demotes its family nor retries the other one,
// and a target that cannot connect does not undo a live demotion. Without the
// move, the same stall demotes and retries
// (TestFamilyFallbackRecoversFromAPostConnectTimeout).
func TestFamilyFallbackTreatsMoveTargetsAsLiterals(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	controlFamilyClear()
	defer controlFamilyClear()
	SetControlIpFamilyPolicy(IpFamilyAuto)
	ctx := withMoveDialOverride(context.Background(), &moveDialOverride{
		host:      "api.example",
		targetIps: []netip.Addr{testMoveTargetIpv6},
	})

	var stateLock sync.Mutex
	dialed := []string{}
	stall := func(ctx context.Context, network string, addr string) (net.Conn, error) {
		stateLock.Lock()
		dialed = append(dialed, network)
		stateLock.Unlock()
		return &stubConn{remote: &net.TCPAddr{IP: net.IP(testMoveTargetIpv6.AsSlice()), Port: 443}}, nil
	}
	handshake := func(ctx context.Context, conn net.Conn) (net.Conn, error) {
		return nil, &timeoutError{}
	}
	if _, err := dialControlTlsWithFamilyFallback(ctx, DefaultConnectSettings(), "tcp", "api.example:443", stall, handshake); err == nil {
		t.Fatal("a stalled handshake at the target connected")
	}
	stateLock.Lock()
	if !slices.Equal(dialed, []string{"tcp"}) {
		t.Fatalf("dialed %v, want one dial with no family retry", dialed)
	}
	stateLock.Unlock()
	if family := controlFamilyDemotedFamily(); family != 0 {
		t.Fatalf("a stall at a move target demoted ipv%d", family)
	}

	if !controlFamilyDemote(6) {
		t.Fatal("expected the demotion to take")
	}
	unreachable := func(ctx context.Context, network string, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connect: network is unreachable")}
	}
	if _, err := dialControlTlsWithFamilyFallback(ctx, DefaultConnectSettings(), "tcp", "api.example:443", unreachable, handshake); err == nil {
		t.Fatal("an unreachable target connected")
	}
	if family := controlFamilyDemotedFamily(); family != 6 {
		t.Fatalf("a move target that could not connect undid the demotion (demoted ipv%d)", family)
	}
}

// The DNS carrier runner of a strict move's replacement builds no attempt
// until the move concludes, and then dials as usual.
func TestPlatformTransportStrictMoveHoldsTheDnsCarrierRunner(t *testing.T) {
	plan := PlanResidentMove(
		testAdvisoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		DefaultPlatformTransportSettings(),
		NewResidentMoveTracker(),
		time.Now(),
	)
	attempts := make(chan struct{}, 16)
	settings := testingPlatformTransportSettings()
	settings.ResidentMoveDial = plan.Dial
	settings.AuthFrameObserver = func([]byte) {
		select {
		case attempts <- struct{}{}:
		default:
		}
	}
	settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
		return nil, errors.New("test: the attempt is all this test needs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "move-dns-hold"),
		"https://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH3Dns,
		settings,
	)
	defer transport.Close()
	select {
	case <-attempts:
		t.Fatal("the DNS carrier dialed during a strict move")
	case <-time.After(200 * time.Millisecond):
	}
	plan.Dial.Conclude()
	receiveTestMoveValue(t, attempts, "the DNS carrier did not dial once the move concluded")
}

// A compulsory move's replacement tries its target once per carrier. After the
// target refuses, the next attempt resolves the name and connects; the target
// failure is the move's, and the connection is no arrival.
func TestPlatformTransportCompulsoryMoveDialFallsBackToTheName(t *testing.T) {
	server := newTestMoveH1Server(t, testMoveHost)
	seam := newTestMoveDialSeam(server.address, testMoveHostAddress())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, server.roots, seam.dial)
	tracker := NewResidentMoveTracker()
	plan := PlanResidentMove(
		testCompulsoryMove(testMoveTargetIpv4),
		[]ResidentMoveTransport{testDirectMoveTransport(0)},
		DefaultPlatformTransportSettings(),
		tracker,
		time.Now(),
	)
	settings := testingPlatformTransportSettings()
	settings.ResidentMoveTracker = tracker
	settings.ResidentMoveDial = plan.Dial
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-compulsory"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeH1,
		settings,
	)
	defer transport.Close()
	request := receiveTestMoveValue(t, server.requests, "the replacement did not fall back to the name")
	if request.host != testMoveHost {
		t.Fatalf("HTTP host = %q", request.host)
	}
	dialed := seam.dialedAddresses()
	if len(dialed) < 2 || dialed[0] != testMoveTargetAddress(testMoveTargetIpv4) || dialed[len(dialed)-1] != testMoveHostAddress() {
		t.Fatalf("dials = %v, want the target and then the name", dialed)
	}
	failure, failed := plan.Dial.Failure()
	if !failed || failure.Reason != protocol.ResidentMoveReason_ResidentMoveReasonDialError || failure.IpFamily != 4 {
		t.Fatalf("failure = %+v, want the target's dial error", failure)
	}
	if !waitForCondition(10*time.Second, transport.IsConnected) {
		t.Fatal("the replacement did not connect by name")
	}
	if plan.Dial.Arrived() {
		t.Fatal("a connection by name counted as an arrival")
	}
	if got := tracker.preferredTargetIps(testMoveHost, 0, time.Now()); 0 < len(got) {
		t.Fatalf("a connection by name recorded a preference: %v", got)
	}
}

// A packet connection for an H3 carrier that records every destination and
// sends only to loopback, so a dial to a documentation address leaves the
// machine as nothing.
type testMoveRecordingPacketConn struct {
	net.PacketConn
	stateLock sync.Mutex
	addresses []string
}

func (self *testMoveRecordingPacketConn) WriteTo(packet []byte, addr net.Addr) (int, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addresses = append(self.addresses, addr.String())
	}()
	if udpAddr, ok := addr.(*net.UDPAddr); !ok || !udpAddr.IP.IsLoopback() {
		return len(packet), nil
	}
	return self.PacketConn.WriteTo(packet, addr)
}

func (self *testMoveRecordingPacketConn) dialedAddresses() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.addresses)
}

// An H3 packet connection factory over recording connections, and the
// destinations every connection it made sent to.
func newTestMoveRecordingPacketConnFactory() (func(context.Context) (net.PacketConn, error), func() []string) {
	var stateLock sync.Mutex
	packetConns := []*testMoveRecordingPacketConn{}
	factory := func(context.Context) (net.PacketConn, error) {
		packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		recording := &testMoveRecordingPacketConn{PacketConn: packetConn}
		stateLock.Lock()
		defer stateLock.Unlock()
		packetConns = append(packetConns, recording)
		return recording, nil
	}
	dialedAddresses := func() []string {
		stateLock.Lock()
		defer stateLock.Unlock()
		addresses := []string{}
		for _, packetConn := range packetConns {
			addresses = append(addresses, packetConn.dialedAddresses()...)
		}
		return addresses
	}
	return factory, dialedAddresses
}

// An alt url redirects the plain H3 carrier to alt (EXTENDER.md L4), and a
// move redirects only the platform host's edges: with an alt url, neither a
// move's targets (strict or compulsory) nor a preference stand in for the alt
// address, and the attempt never counts as a dial of its targets. Without one,
// the target stands in on the platform port.
func TestPlatformTransportMoveTargetsNeverRedirectAnAltCarrier(t *testing.T) {
	transports := []ResidentMoveTransport{testDirectMoveTransport(0)}
	strict := PlanResidentMove(testAdvisoryMove(testMoveTargetIpv4), transports, DefaultPlatformTransportSettings(), NewResidentMoveTracker(), time.Now()).Dial
	compulsory := PlanResidentMove(testCompulsoryMove(testMoveTargetIpv4), transports, DefaultPlatformTransportSettings(), NewResidentMoveTracker(), time.Now()).Dial
	for _, c := range []struct {
		name    string
		attempt func() *platformMoveDialAttempt
	}{
		{name: "strict move", attempt: func() *platformMoveDialAttempt {
			return &platformMoveDialAttempt{
				override: &moveDialOverride{host: testMoveHost, targetIps: []netip.Addr{testMoveTargetIpv4}, strict: true},
				dial:     strict,
			}
		}},
		{name: "compulsory move", attempt: func() *platformMoveDialAttempt {
			return &platformMoveDialAttempt{
				override: &moveDialOverride{host: testMoveHost, targetIps: []netip.Addr{testMoveTargetIpv4}},
				dial:     compulsory,
			}
		}},
		{name: "preference", attempt: func() *platformMoveDialAttempt {
			return &platformMoveDialAttempt{
				override: &moveDialOverride{host: testMoveHost, targetIps: []netip.Addr{testMoveTargetIpv4}},
			}
		}},
	} {
		settings := DefaultPlatformTransportSettings()
		settings.AltUrl = "https://" + testAltIp
		transport := newTestAltTransport(t, settings)
		attempt := c.attempt()
		udpAddrs, _, err := transport.h3DialCandidates(attempt.withContext(context.Background()), TransportModeH3, testMoveHost)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := testAltCandidateHostPorts(udpAddrs); !slices.Equal(got, []string{net.JoinHostPort(testAltIp, "443")}) {
			t.Fatalf("%s with an alt url: candidates = %v, want the alt address", c.name, got)
		}
		if attempt.dialedTargets() {
			t.Fatalf("%s with an alt url: the attempt counts as a dial of its targets", c.name)
		}

		settings.AltUrl = ""
		attempt = c.attempt()
		udpAddrs, _, err = transport.h3DialCandidates(attempt.withContext(context.Background()), TransportModeH3, testMoveHost)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := testAltCandidateHostPorts(udpAddrs); !slices.Equal(got, []string{testMoveTargetAddress(testMoveTargetIpv4)}) {
			t.Fatalf("%s without an alt url: candidates = %v, want the target on the platform port", c.name, got)
		}
		if !attempt.dialedTargets() {
			t.Fatalf("%s without an alt url: the target dial was not counted", c.name)
		}
	}
}

// A strict move's replacement parks an alt-redirected H3 carrier, which
// cannot reach a target, until the move concludes; a compulsory move's dials
// alt by name, and without an alt url plain H3 dials the target.
func TestPlatformTransportStrictMoveParksAnAltH3Carrier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	direct := NewClientStrategyWithDefaults(ctx)
	defer direct.Close()
	transports := []ResidentMoveTransport{testDirectMoveTransport(0)}
	strict := PlanResidentMove(testAdvisoryMove(testMoveTargetIpv4), transports, DefaultPlatformTransportSettings(), NewResidentMoveTracker(), time.Now()).Dial
	compulsory := PlanResidentMove(testCompulsoryMove(testMoveTargetIpv4), transports, DefaultPlatformTransportSettings(), NewResidentMoveTracker(), time.Now()).Dial
	altSettings := DefaultPlatformTransportSettings()
	altSettings.AltUrl = "https://" + testAltIp

	for _, c := range []struct {
		name     string
		settings *PlatformTransportSettings
		dial     *ResidentMoveDial
	}{
		{name: "compulsory move on alt", settings: altSettings, dial: compulsory},
		{name: "strict move without alt", settings: DefaultPlatformTransportSettings(), dial: strict},
	} {
		transport := &PlatformTransport{clientStrategy: direct, settings: c.settings, residentMoveDial: c.dial}
		if !transport.waitMoveCarrierAdmission(ctx, TransportModeH3) {
			t.Fatalf("%s: H3 was held", c.name)
		}
	}

	transport := &PlatformTransport{clientStrategy: direct, settings: altSettings, residentMoveDial: strict}
	if !transport.waitMoveCarrierAdmission(ctx, TransportModeH1) {
		t.Fatal("a strict move on alt held H1, the carrier that reaches the target")
	}
	released := make(chan bool, 1)
	go func() {
		released <- transport.waitMoveCarrierAdmission(ctx, TransportModeH3)
	}()
	select {
	case <-released:
		t.Fatal("a strict move dialed the alt-redirected H3 carrier")
	case <-time.After(50 * time.Millisecond):
	}
	strict.Conclude()
	if !receiveTestMoveValue(t, released, "the conclusion did not release the alt H3 carrier") {
		t.Fatal("the released carrier reported its context ended")
	}
}

// A strict move's replacement on alt arrives at the target over H1 while its
// H3 carrier waits. Once the move concludes, H3 runs again and dials alt --
// never the target, not even through the preference the arrival left.
func TestPlatformTransportStrictMoveArrivesOverH1WhileAltH3Waits(t *testing.T) {
	edge := newTestMoveH1Server(t, testMoveHost)
	alt := newTestMoveH3Server(t, testMoveHost)
	seam := newTestMoveDialSeam(edge.address, testMoveTargetAddress(testMoveTargetIpv4))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newTestMoveStrategy(t, ctx, edge.roots, seam.dial)

	h3Attempts := make(chan struct{}, 64)
	factory, h3DialedAddresses := newTestMoveRecordingPacketConnFactory()
	settings := testingPlatformTransportSettings()
	// equal tiers, so H3 does not stand down for H1 once it may dial
	settings.ModePreferences = map[TransportMode]int{TransportModeH1: 1, TransportModeH3: 1}
	settings.AltUrl = "https://" + alt.addr.String()
	settings.QuicTlsConfig = &tls.Config{RootCAs: alt.roots, NextProtos: []string{testMoveH3NextProto}}
	settings.H3PacketConnFactory = factory
	// only H3 builds an auth frame: H1 authenticates in its headers
	settings.AuthFrameObserver = func([]byte) {
		select {
		case h3Attempts <- struct{}{}:
		default:
		}
	}
	// called on the H3 runner before it waits, after any attempt it made
	h3Parked := make(chan struct{}, 8)
	settings.moveCarrierHoldForTest = func(mode TransportMode) {
		if mode == TransportModeH3 {
			select {
			case h3Parked <- struct{}{}:
			default:
			}
		}
	}
	tracker := NewResidentMoveTracker()
	onAlt := testDirectMoveTransport(0)
	onAlt.activeMode = TransportModeH3
	plan := PlanResidentMove(testAdvisoryMove(testMoveTargetIpv4), []ResidentMoveTransport{onAlt}, settings, tracker, time.Now())
	if plan.Declined || plan.Dial == nil {
		t.Fatalf("a transport on H3 through alt was not moved: %+v", plan)
	}
	settings.ResidentMoveTracker = tracker
	settings.ResidentMoveDial = plan.Dial
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "move-alt"),
		"wss://"+testMoveHost,
		testingFamilyAuth(),
		TransportModeAuto,
		settings,
	)
	defer transport.Close()

	request := receiveTestMoveValue(t, edge.requests, "the replacement did not reach the target over H1")
	if request.host != testMoveHost {
		t.Fatalf("HTTP host = %q", request.host)
	}
	if !waitForCondition(10*time.Second, plan.Dial.Arrived) {
		t.Fatal("the H1 arrival was not counted")
	}
	receiveTestMoveValue(t, h3Parked, "the alt H3 carrier was not parked during the strict move")
	select {
	case <-h3Attempts:
		t.Fatal("the alt H3 carrier dialed during the strict move")
	default:
	}

	plan.Dial.Conclude()
	receiveTestMoveValue(t, h3Attempts, "the alt H3 carrier did not run once the move concluded")
	if serverName := receiveTestMoveValue(t, alt.serverNames, "the H3 carrier did not reach alt"); serverName != testMoveHost {
		t.Fatalf("alt server name = %q", serverName)
	}
	for _, address := range h3DialedAddresses() {
		if address != alt.addr.String() {
			t.Fatalf("H3 sent to %q, want only alt at %s", address, alt.addr)
		}
	}
	if got := tracker.preferredTargetIps(testMoveHost, 0, time.Now()); !slices.Equal(got, []netip.Addr{testMoveTargetIpv4}) {
		t.Fatalf("the H1 arrival's preference = %v", got)
	}
}
