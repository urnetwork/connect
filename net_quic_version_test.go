package connect

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect/protocol"
)

// The QUIC version offer of every carrier (EXTENDER.md A13): what the first
// Initial carries per policy at each construction site, that a version 1 only
// peer is still reached through negotiation, that forcing version 2 refuses
// such a peer rather than falling back in the clear, and that a retry after
// a failed handshake leaves its local port behind. Every observation is made
// on the wire bytes of the client's own udp endpoint, which is where a filter
// reads them, or on the version the peer negotiated.

// The wire version numbers as the long header carries them (RFC 9000 §15,
// RFC 9369 §2), and the version field of a Version Negotiation packet
// (RFC 9000 §17.2.1).
const (
	quicWireVersion1           uint32 = 0x00000001
	quicWireVersion2           uint32 = 0x6b3343cf
	quicWireVersionNegotiation uint32 = 0x00000000
)

// The version field of a datagram whose first packet has a long header,
// which every Initial and Version Negotiation packet has; false for a short
// header, which carries no version.
func quicDatagramVersion(datagram []byte) (uint32, bool) {
	if len(datagram) < 5 || datagram[0]&0x80 == 0 {
		return 0, false
	}
	return binary.BigEndian.Uint32(datagram[1:5]), true
}

// A loopback udp endpoint that records the long-header version of every
// datagram it sends and receives, in order. When holdPort is set, the close
// the dial owes it is counted but the socket stays bound until the test ends,
// so the operating system cannot hand the next endpoint the same port and a
// repeated port is a dial's choice, never the allocator's.
type testQuicVersionPacketConn struct {
	net.PacketConn
	holdPort bool
	// whether the endpoint before this one had been closed when this one was
	// asked for
	previousReleased bool

	stateLock       sync.Mutex
	writtenVersions []uint32
	readVersions    []uint32
	closeCount      int
}

func newTestQuicVersionPacketConn(holdPort bool) (*testQuicVersionPacketConn, error) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &testQuicVersionPacketConn{
		PacketConn: packetConn,
		holdPort:   holdPort,
	}, nil
}

func (self *testQuicVersionPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if version, ok := quicDatagramVersion(b); ok {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.writtenVersions = append(self.writtenVersions, version)
		}()
	}
	return self.PacketConn.WriteTo(b, addr)
}

func (self *testQuicVersionPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := self.PacketConn.ReadFrom(b)
	if err == nil {
		if version, ok := quicDatagramVersion(b[:n]); ok {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.readVersions = append(self.readVersions, version)
			}()
		}
	}
	return n, addr, err
}

func (self *testQuicVersionPacketConn) Close() error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closeCount += 1
	}()
	if self.holdPort {
		return nil
	}
	return self.PacketConn.Close()
}

// The versions written so far, the first being what the first Initial
// carried.
func (self *testQuicVersionPacketConn) written() []uint32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writtenVersions)
}

func (self *testQuicVersionPacketConn) read() []uint32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readVersions)
}

func (self *testQuicVersionPacketConn) closes() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closeCount
}

func (self *testQuicVersionPacketConn) port() int {
	return self.PacketConn.LocalAddr().(*net.UDPAddr).Port
}

// The endpoints one test's dials asked for, in order. A factory may run on a
// dial goroutine, so the list is locked and nothing here fails the test
// directly.
type testQuicVersionEndpoints struct {
	t        *testing.T
	holdPort bool

	stateLock sync.Mutex
	endpoints []*testQuicVersionPacketConn
}

// A PacketConnFactory that hands out one recording endpoint per call.
func (self *testQuicVersionEndpoints) factory(context.Context) (net.PacketConn, error) {
	endpoint, err := newTestQuicVersionPacketConn(self.holdPort)
	if err != nil {
		return nil, err
	}
	self.t.Cleanup(func() { endpoint.PacketConn.Close() })
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	endpoint.previousReleased = len(self.endpoints) == 0 ||
		self.endpoints[len(self.endpoints)-1].closes() == 1
	self.endpoints = append(self.endpoints, endpoint)
	return endpoint, nil
}

func (self *testQuicVersionEndpoints) all() []*testQuicVersionPacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.endpoints)
}

// The first endpoint a test's dials asked for, which is the one whose first
// datagram is the offer.
func (self *testQuicVersionEndpoints) first(t *testing.T) *testQuicVersionPacketConn {
	t.Helper()
	endpoints := self.all()
	if len(endpoints) == 0 {
		t.Fatal("no dial asked for an endpoint")
	}
	return endpoints[0]
}

// One in-process udp carrier endpoint: a quic listener on loopback whose h3
// handler answers every extender request with an empty response frame, which
// is all DialExtender reads before it hands the stream over. It accepts the
// versions it is given, records the version each accepted connection
// negotiated, and refuses the Initial of every source address refuse names,
// the way a filter that poisoned a 4-tuple does.
type testQuicCarrierServer struct {
	port     int
	versions chan quic.Version
}

func newTestQuicCarrierServer(
	t *testing.T,
	versions []quic.Version,
	refuse func(remoteAddr net.Addr) bool,
) *testQuicCarrierServer {
	t.Helper()
	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "quic-version-test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testQuicCarrierServer{
		port:     packetConn.LocalAddr().(*net.UDPAddr).Port,
		versions: make(chan quic.Version, 16),
	}
	responseFrame, err := ExtenderResponseFrame(&protocol.ExtenderResponse{})
	if err != nil {
		packetConn.Close()
		t.Fatal(err)
	}
	h3Server := &http3.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(responseFrame)
		}),
		IdleTimeout: 30 * time.Second,
	}
	quicConfig := &quic.Config{
		MaxIdleTimeout: 30 * time.Second,
		Versions:       versions,
	}
	quicConfig.GetConfigForClient = func(info *quic.ClientInfo) (*quic.Config, error) {
		if refuse != nil && refuse(info.RemoteAddr) {
			return nil, errors.New("the source address is refused")
		}
		return quicConfig, nil
	}
	quicTransport := &quic.Transport{Conn: packetConn}
	listener, err := quicTransport.Listen(
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{http3.NextProtoH3},
		},
		quicConfig,
	)
	if err != nil {
		quicTransport.Close()
		packetConn.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			quicConn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			select {
			case server.versions <- quicConn.ConnectionState().Version:
			default:
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
		packetConn.Close()
	})
	return server
}

// The version the next accepted connection negotiated.
func (self *testQuicCarrierServer) nextVersion(t *testing.T) quic.Version {
	t.Helper()
	select {
	case version := <-self.versions:
		return version
	case <-time.After(10 * time.Second):
		t.Fatal("the carrier server accepted no connection")
		return 0
	}
}

// One extender dial over the udp carrier to the in-process server, under the
// policy and over the endpoints given.
func dialTestQuicCarrier(
	server *testQuicCarrierServer,
	policy QuicVersionPolicy,
	endpoints *testQuicVersionEndpoints,
) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connectSettings := DefaultConnectSettings()
	connectSettings.QuicVersionPolicy = policy
	connectSettings.DialContextSettings = &DialContextSettings{
		DialContext:       (&net.Dialer{}).DialContext,
		PacketConnFactory: endpoints.factory,
	}
	extenderConfig := &ExtenderConfig{
		Ip: netip.MustParseAddr("127.0.0.1"),
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeQuic,
			Port:        server.port,
		},
	}
	conn, _, err := DialExtender(ctx, connectSettings, extenderConfig, &ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
	})
	return conn, err
}

// The offer of each policy value, and of a value nobody wrote, which must
// not quietly turn a kill switch into a disabled carrier.
func TestQuicVersionPolicyVersions(t *testing.T) {
	for _, c := range []struct {
		policy   QuicVersionPolicy
		versions []quic.Version
	}{
		{policy: "", versions: []quic.Version{quic.Version2, quic.Version1}},
		{policy: QuicVersionPolicyPreferV2, versions: []quic.Version{quic.Version2, quic.Version1}},
		{policy: QuicVersionPolicyV1, versions: []quic.Version{quic.Version1}},
		{policy: QuicVersionPolicyV2, versions: []quic.Version{quic.Version2}},
		{policy: "v3", versions: []quic.Version{quic.Version2, quic.Version1}},
	} {
		if versions := c.policy.Versions(); !slices.Equal(versions, c.versions) {
			t.Errorf("%q offers %v, want %v", c.policy, versions, c.versions)
		}
	}
	// the constants are the wire numbers the filters key on
	if uint32(quic.Version1) != quicWireVersion1 || uint32(quic.Version2) != quicWireVersion2 {
		t.Fatalf("quic-go versions are %#08x and %#08x", uint32(quic.Version1), uint32(quic.Version2))
	}
}

// Every defaults function offers version 2 first, every config built from the
// defaults carries that offer, and the extender dial's config takes the
// offer of its connect settings rather than of the platform transport that
// sized its windows.
func TestQuicDefaultsOfferVersion2First(t *testing.T) {
	preferV2 := []quic.Version{quic.Version2, quic.Version1}
	if policy := DefaultConnectSettings().QuicVersionPolicy; policy != QuicVersionPolicyPreferV2 {
		t.Errorf("DefaultConnectSettings policy = %q", policy)
	}
	platformSettings := DefaultPlatformTransportSettings()
	if policy := platformSettings.QuicVersionPolicy; policy != QuicVersionPolicyPreferV2 {
		t.Errorf("DefaultPlatformTransportSettings policy = %q", policy)
	}
	if versions := newPlatformQuicConfig(platformSettings, 1).Versions; !slices.Equal(versions, preferV2) {
		t.Errorf("platform h3 config offers %v", versions)
	}
	platformSettings.QuicVersionPolicy = QuicVersionPolicyV1
	if versions := newPlatformQuicConfig(platformSettings, 1).Versions; !slices.Equal(versions, []quic.Version{quic.Version1}) {
		t.Errorf("platform h3 config forced to v1 offers %v", versions)
	}
	ctx := context.WithValue(context.Background(), extenderTransportSettingsContextKey{}, platformSettings)
	connectSettings := DefaultConnectSettings()
	if versions := newExtenderQuicMemoryPolicy(ctx, connectSettings).quicConfig.Versions; !slices.Equal(versions, preferV2) {
		t.Errorf("extender dial config under a v1 platform transport offers %v, want the dial's own %v", versions, preferV2)
	}
	connectSettings.QuicVersionPolicy = QuicVersionPolicyV2
	if versions := newExtenderQuicMemoryPolicy(ctx, connectSettings).quicConfig.Versions; !slices.Equal(versions, []quic.Version{quic.Version2}) {
		t.Errorf("extender dial config forced to v2 offers %v", versions)
	}
}

// The policies a carrier is dialed under and the version the first Initial
// then carries: version 2 by default, under prefer-v2 and when forced, and
// version 1 only when forced.
var testQuicVersionOffers = []struct {
	policy  QuicVersionPolicy
	version uint32
}{
	{policy: "", version: quicWireVersion2},
	{policy: QuicVersionPolicyPreferV2, version: quicWireVersion2},
	{policy: QuicVersionPolicyV1, version: quicWireVersion1},
	{policy: QuicVersionPolicyV2, version: quicWireVersion2},
}

// The extender udp carrier's first Initial carries the version its connect
// settings offer first, and the in-process extender, which accepts both,
// completes the handshake on that version.
func TestExtenderQuicCarrierOffersTheVersionOfItsPolicy(t *testing.T) {
	server := newTestQuicCarrierServer(t, QuicVersionPolicyPreferV2.Versions(), nil)
	for _, c := range testQuicVersionOffers {
		endpoints := &testQuicVersionEndpoints{t: t}
		conn, err := dialTestQuicCarrier(server, c.policy, endpoints)
		if err != nil {
			t.Fatalf("%q: %v", c.policy, err)
		}
		conn.Close()
		if written := endpoints.first(t).written(); len(written) == 0 || written[0] != c.version {
			t.Errorf("%q: first datagram versions = %#08x, want %#08x first", c.policy, written, c.version)
		}
		if negotiated := server.nextVersion(t); uint32(negotiated) != c.version {
			t.Errorf("%q: the extender negotiated %s, want %#08x", c.policy, negotiated, c.version)
		}
	}
}

// A version 1 only extender still carries the dial: the first Initial is
// version 2, the extender answers a Version Negotiation packet, and the
// client re-dials with version 1 and completes.
func TestExtenderQuicCarrierFallsBackToAVersion1OnlyExtender(t *testing.T) {
	server := newTestQuicCarrierServer(t, []quic.Version{quic.Version1}, nil)
	endpoints := &testQuicVersionEndpoints{t: t}
	conn, err := dialTestQuicCarrier(server, QuicVersionPolicyPreferV2, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	endpoint := endpoints.first(t)
	written := endpoint.written()
	if len(written) == 0 || written[0] != quicWireVersion2 {
		t.Fatalf("first datagram versions = %#08x, want version 2 first", written)
	}
	if !slices.Contains(written, quicWireVersion1) {
		t.Fatalf("the client never re-dialed with version 1: %#08x", written)
	}
	if !slices.Contains(endpoint.read(), quicWireVersionNegotiation) {
		t.Fatalf("no Version Negotiation packet was received: %#08x", endpoint.read())
	}
	if negotiated := server.nextVersion(t); negotiated != quic.Version1 {
		t.Fatalf("the extender negotiated %s, want version 1", negotiated)
	}
}

// Forcing version 2 is the one offer with no fallback: a version 1 only
// extender answers a Version Negotiation packet naming no version the client
// offers, and the dial fails with that rather than re-dialing in the clear.
func TestExtenderQuicCarrierForcedToVersion2RefusesAVersion1OnlyExtender(t *testing.T) {
	server := newTestQuicCarrierServer(t, []quic.Version{quic.Version1}, nil)
	endpoints := &testQuicVersionEndpoints{t: t}
	conn, err := dialTestQuicCarrier(server, QuicVersionPolicyV2, endpoints)
	if conn != nil {
		conn.Close()
	}
	var negotiationErr *quic.VersionNegotiationError
	if !errors.As(err, &negotiationErr) {
		t.Fatalf("forced v2 against a v1 only extender = %v, want a version negotiation error", err)
	}
	if !slices.Equal(negotiationErr.Ours, []quic.Version{quic.Version2}) || !slices.Contains(negotiationErr.Theirs, quic.Version1) {
		t.Fatalf("negotiation error ours = %v theirs = %v", negotiationErr.Ours, negotiationErr.Theirs)
	}
	if written := endpoints.first(t).written(); slices.Contains(written, quicWireVersion1) {
		t.Fatalf("a version 1 Initial left the forced v2 endpoint: %#08x", written)
	}
}

// A retry after a failed handshake leaves the local port behind (A13): each
// dial asks for its own endpoint, and the failed dial's endpoint is closed
// before the retry opens its own. The in-process extender refuses the first
// source port it sees for good, the way a filter that poisoned the 4-tuple of
// a failed attempt does for minutes, so a retry on that port could not
// succeed; the first endpoint keeps its port bound until the test ends, so
// the retry's different port is the dial's doing and not the allocator's.
func TestExtenderQuicCarrierRetryRotatesTheSourcePort(t *testing.T) {
	var refusedLock sync.Mutex
	refusedPort := 0
	server := newTestQuicCarrierServer(t, QuicVersionPolicyPreferV2.Versions(), func(remoteAddr net.Addr) bool {
		refusedLock.Lock()
		defer refusedLock.Unlock()
		port := remoteAddr.(*net.UDPAddr).Port
		if refusedPort == 0 {
			refusedPort = port
		}
		return port == refusedPort
	})
	endpoints := &testQuicVersionEndpoints{t: t, holdPort: true}

	conn, err := dialTestQuicCarrier(server, QuicVersionPolicyPreferV2, endpoints)
	if conn != nil {
		conn.Close()
	}
	var transportErr *quic.TransportError
	if !errors.As(err, &transportErr) || transportErr.ErrorCode != quic.ConnectionRefused {
		t.Fatalf("the poisoned dial = %v, want a refusal", err)
	}

	conn, err = dialTestQuicCarrier(server, QuicVersionPolicyPreferV2, endpoints)
	if err != nil {
		t.Fatalf("the retry was not carried: %v", err)
	}
	conn.Close()

	all := endpoints.all()
	if len(all) != 2 {
		t.Fatalf("endpoints = %d, want one per dial", len(all))
	}
	failed, retry := all[0], all[1]
	refusedLock.Lock()
	port := refusedPort
	refusedLock.Unlock()
	if port != failed.port() {
		t.Fatalf("the extender refused port %d, the failed dial used %d", port, failed.port())
	}
	if retry.port() == failed.port() {
		t.Fatalf("the retry reused local port %d", failed.port())
	}
	if !retry.previousReleased {
		t.Fatal("the failed dial's endpoint was still open when the retry asked for its own")
	}
	if failed.closes() != 1 || retry.closes() != 1 {
		t.Fatalf("endpoint closes = %d, %d, want one each", failed.closes(), retry.closes())
	}
}

// The alt h3 dialer's first Initial carries the version its connect settings
// offer first, and the api GET completes on it (L4).
func TestAltQuicDialerOffersTheVersionOfItsPolicy(t *testing.T) {
	for _, c := range testQuicVersionOffers {
		altServer := newTestAltServer(t, false)
		endpoints := &testQuicVersionEndpoints{t: t}
		clientStrategy := newTestAltStrategyWithSettings(t, altServer, func(settings *ClientStrategySettings) {
			settings.ConnectSettings.QuicVersionPolicy = c.policy
			settings.ConnectSettings.DialContextSettings = &DialContextSettings{
				DialContext:       (&net.Dialer{}).DialContext,
				PacketConnFactory: endpoints.factory,
			}
		})
		if body := testAltGet(t, testAltDialer(t, clientStrategy, "alt h3")); body != testAltBodyText {
			t.Fatalf("%q: body = %q", c.policy, body)
		}
		if written := endpoints.first(t).written(); len(written) == 0 || written[0] != c.version {
			t.Errorf("%q: first datagram versions = %#08x, want %#08x first", c.policy, written, c.version)
		}
		if negotiated := altServer.nextVersion(t); uint32(negotiated) != c.version {
			t.Errorf("%q: alt negotiated %s, want %#08x", c.policy, negotiated, c.version)
		}
	}
}

// A version 1 only alt still answers the api GET: version 2 is offered first
// and the dial falls back to version 1 on the negotiation.
func TestAltQuicDialerFallsBackToAVersion1OnlyAlt(t *testing.T) {
	altServer := newTestAltServerWithVersions(t, false, "127.0.0.1", []quic.Version{quic.Version1})
	endpoints := &testQuicVersionEndpoints{t: t}
	clientStrategy := newTestAltStrategyWithSettings(t, altServer, func(settings *ClientStrategySettings) {
		settings.ConnectSettings.DialContextSettings = &DialContextSettings{
			DialContext:       (&net.Dialer{}).DialContext,
			PacketConnFactory: endpoints.factory,
		}
	})
	if body := testAltGet(t, testAltDialer(t, clientStrategy, "alt h3")); body != testAltBodyText {
		t.Fatalf("body = %q", body)
	}
	endpoint := endpoints.first(t)
	written := endpoint.written()
	if len(written) == 0 || written[0] != quicWireVersion2 {
		t.Fatalf("first datagram versions = %#08x, want version 2 first", written)
	}
	if !slices.Contains(endpoint.read(), quicWireVersionNegotiation) {
		t.Fatalf("no Version Negotiation packet was received: %#08x", endpoint.read())
	}
	if negotiated := altServer.nextVersion(t); negotiated != quic.Version1 {
		t.Fatalf("alt negotiated %s, want version 1", negotiated)
	}
}

// One platform h3 transport against the in-process platform, under the
// policy and over the endpoints given, joined once the platform has read its
// auth. The version the platform negotiated is returned.
func connectTestPlatformH3(
	t *testing.T,
	platform *testingH3Platform,
	policy QuicVersionPolicy,
	endpoints *testQuicVersionEndpoints,
) quic.Version {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := platform.transportSettings()
	settings.QuicVersionPolicy = policy
	settings.H3PacketConnFactory = endpoints.factory
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "h3-version"),
		"https://"+testLoopbackHost(4),
		testingFamilyAuth(),
		TransportModeH3,
		settings,
	)
	defer transport.Close()
	select {
	case connection := <-platform.remotes:
		return connection.version
	case <-time.After(15 * time.Second):
		t.Fatal("the h3 transport never connected")
		return 0
	}
}

// The platform h3 transport's first Initial carries the version its settings
// offer first, and the platform, which accepts both, completes the handshake
// on that version.
func TestPlatformTransportH3OffersTheVersionOfItsPolicy(t *testing.T) {
	for _, c := range testQuicVersionOffers {
		platform := newTestingH3Platform(t, 4)
		endpoints := &testQuicVersionEndpoints{t: t}
		negotiated := connectTestPlatformH3(t, platform, c.policy, endpoints)
		if written := endpoints.first(t).written(); len(written) == 0 || written[0] != c.version {
			t.Errorf("%q: first datagram versions = %#08x, want %#08x first", c.policy, written, c.version)
		}
		if uint32(negotiated) != c.version {
			t.Errorf("%q: the platform negotiated %s, want %#08x", c.policy, negotiated, c.version)
		}
	}
}

// A version 1 only platform still connects the h3 transport through the
// negotiation, with version 2 offered first.
func TestPlatformTransportH3FallsBackToAVersion1OnlyPlatform(t *testing.T) {
	platform := newTestingH3PlatformWithVersions(t, 4, []quic.Version{quic.Version1})
	endpoints := &testQuicVersionEndpoints{t: t}
	negotiated := connectTestPlatformH3(t, platform, QuicVersionPolicyPreferV2, endpoints)
	endpoint := endpoints.first(t)
	written := endpoint.written()
	if len(written) == 0 || written[0] != quicWireVersion2 {
		t.Fatalf("first datagram versions = %#08x, want version 2 first", written)
	}
	if !slices.Contains(endpoint.read(), quicWireVersionNegotiation) {
		t.Fatalf("no Version Negotiation packet was received: %#08x", endpoint.read())
	}
	if negotiated != quic.Version1 {
		t.Fatalf("the platform negotiated %s, want version 1", negotiated)
	}
}
