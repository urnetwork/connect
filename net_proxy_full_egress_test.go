package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Every egress of a space whose egress is a proxy (connect#227, owner
// 2026-11-18, "route through the proxy"): a provider's platform transports, its
// exit's tcp, udp and icmp, its p2p streams, and the family evidence its dials
// leave behind. Each test puts a recording socks proxy where the space's
// traffic should go, or a canary where this host's own traffic would land, and
// keeps the same dial without a proxy as the control where the control proves
// something.
//
// Addresses are loopback and RFC 5737 / RFC 3849 documentation addresses.

// Waits for the recorder to have seen every target in `expectTargets`, in any
// order, and fails at once when anything reaches `leaked`: a canary standing
// where this host's own dial would land.
func waitTestSocksTargets(t *testing.T, ctx context.Context, socks *testSocksRecorder, leaked <-chan string, expectTargets ...string) {
	t.Helper()
	seen := []string{}
	for {
		missing := false
		for _, target := range expectTargets {
			if !slices.Contains(seen, target) {
				missing = true
				break
			}
		}
		if !missing {
			return
		}
		select {
		case target := <-socks.targets:
			seen = append(seen, target)
		case canary := <-leaked:
			t.Fatalf("%s was reached from this host, outside the proxy", canary)
		case <-ctx.Done():
			t.Fatalf("the proxy saw %v, expected %v among them", seen, expectTargets)
		}
	}
}

// A tcp listener on loopback that names itself on `leaked` for each connection
// that reaches it.
func newTestLeakCanary(t *testing.T, name string, leaked chan string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
			select {
			case leaked <- name:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return listener
}

// A provider whose space egresses through a proxy reaches the platform through
// the proxy on every transport of its group: both family pins and the standby.
// So the platform sees the proxy's address, not this host's, and no h3 mode is
// offered, since the proxy carries no udp. Before, the pins built direct
// strategies that dropped the proxy (J4), so they dialed the platform from this
// host and offered h3 from this host's udp.
func TestProxiedProviderTransportsDialOnlyThroughTheProxy(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	socks := newTestSocksRecorder(t)
	// the platform on this host: a connection here left this host outside the
	// proxy
	leaked := make(chan string, 16)
	pinnedPlatform := newTestLeakCanary(t, "the v4 pin's platform", leaked)
	standbyPlatform := newTestLeakCanary(t, "the standby's platform", leaked)
	pinnedPort := pinnedPlatform.Addr().(*net.TCPAddr).Port
	standbyPort := standbyPlatform.Addr().(*net.TCPAddr).Port

	clientSettings := DefaultClientStrategySettings()
	clientSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: socks.listener.Addr().String(),
	}
	// the strategy the sdk hands a public provider's standby
	standbyStrategy := NewEgressClientStrategy(ctx, clientSettings, 0)
	defer standbyStrategy.Close()

	settings := testingFamilyTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	settings.PinnedReconnectMaxTimeout = 100 * time.Millisecond
	group := NewFamilyPlatformTransportGroup(
		ctx,
		clientSettings,
		standbyStrategy,
		NewRouteManager(ctx, "proxied group"),
		fmt.Sprintf("ws://127.0.0.1:%d", standbyPort),
		fmt.Sprintf("ws://127.0.0.1:%d", pinnedPort),
		"ws://[2001:db8::1]:443",
		testingFamilyAuth(),
		TransportModeH1,
		settings,
		&FamilyPlatformTransportGroupSettings{StandbyDelay: 0},
	)
	defer group.Close()

	waitTestSocksTargets(
		t,
		ctx,
		socks,
		leaked,
		fmt.Sprintf("ip 127.0.0.1:%d", pinnedPort),
		"ip 2001:db8::1:443",
		fmt.Sprintf("ip 127.0.0.1:%d", standbyPort),
	)
	select {
	case canary := <-leaked:
		t.Fatalf("%s was reached from this host, outside the proxy", canary)
	default:
	}
	for _, transport := range []*PlatformTransport{group.ipv4Transport, group.ipv6Transport, group.standbyTransport} {
		for _, h3Mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
			if transport.modePreference(h3Mode) != modePreferenceNone {
				t.Fatalf("the ipv%d transport offers %s, which leaves from this host's udp", transport.ipFamily, h3Mode)
			}
		}
	}
}

// The egress strategy keeps the space's proxy and drops the extenders and VLESS
// servers; a pinned dial through it reaches the proxy, refused by the pin when
// the address is of the other family. The direct strategy, which an extender's
// activation takes to prove this host's address, still drops the proxy.
func TestNewEgressClientStrategyKeepsTheSpaceProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	socks := newTestSocksRecorder(t)
	leaked := make(chan string, 4)
	canary := newTestLeakCanary(t, "the pinned dial's target", leaked)
	settings := DefaultClientStrategySettings()
	settings.ExtenderDirectory = NewExtenderDirectoryWithDefaults(ctx)
	defer settings.ExtenderDirectory.Close()
	settings.ExtenderConfigs = []*ExtenderConfig{}
	settings.VlessConfigs = []*VlessConfig{}
	settings.ConnectSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: socks.listener.Addr().String(),
	}

	egress := NewEgressClientStrategy(ctx, settings, 4)
	defer egress.Close()
	if egress.settings.ConnectSettings.ProxySettings == nil {
		t.Fatal("the egress strategy dropped the space's proxy")
	}
	if egress.settings.ExtenderDirectory != nil || egress.settings.MaxExtenderCount != 0 ||
		!egress.settings.DisableManualExtenders || len(egress.settings.VlessConfigs) != 0 || !egress.settings.DisableVless {
		t.Fatal("the egress strategy kept an extender or a VLESS server")
	}
	if _, err := egress.ConnectSettings().DialContext(ctx, "tcp", "[2001:db8::1]:443"); err == nil {
		t.Fatal("the v4 pin dialed a v6 literal")
	}
	canaryAddress := canary.Addr().String()
	if conn, err := egress.ConnectSettings().DialContext(ctx, "tcp", canaryAddress); err == nil {
		conn.Close()
	}
	waitTestSocksTargets(t, ctx, socks, leaked, "ip "+canaryAddress)
	if settings.ConnectSettings.ProxySettings == nil || settings.ExtenderDirectory == nil {
		t.Fatal("the caller's settings were changed")
	}

	direct := NewDirectClientStrategy(ctx, settings, 0)
	defer direct.Close()
	if direct.settings.ConnectSettings.ProxySettings != nil {
		t.Fatal("the direct strategy kept the proxy")
	}
}

// A datagram dial of a space whose egress is a proxy opens no socket of this
// host's: with no packet endpoint of the owner's it is refused, even where a
// headless host's own stream dial would have opened one, and with an endpoint
// the datagram goes through that endpoint alone. Before, the owner's stream
// dial was handed the udp dial and opened a socket of this host's, a proxy
// with no dial seam failed it only by chance in the socks client, and an
// endpoint without a stream dial panicked the dial.
func TestProxiedDatagramDialTakesOnlyThePacketEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	canary := newTestUdpCanary(t)
	canaryAddress := canary.LocalAddr().String()

	// a headless host's stream dial, which can open a udp socket of this
	// host's: never handed the udp of a proxied space
	streamDialCount := atomic.Int64{}
	hostDialSettings := DefaultConnectSettings()
	hostDialSettings.ProxySettings = testProxySettings()
	hostDialSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			streamDialCount.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	if conn, err := hostDialSettings.DialContext(ctx, "udp", canaryAddress); err == nil {
		conn.Write([]byte("leak"))
		conn.Close()
		t.Fatal("a proxied space opened a udp socket of this host's")
	} else if !errors.Is(err, ErrExtenderPacketCarrierUnavailable) {
		t.Fatalf("the dial failed with %v, expected the proxy's refusal", err)
	}
	if count := streamDialCount.Load(); count != 0 {
		t.Fatalf("the space's stream dial was handed %d udp dials", count)
	}
	if testUdpCanaryReceived(t, canary) {
		t.Fatal("a datagram left this host outside the proxy")
	}

	// a proxy and nothing else: refused as such
	proxySettings := DefaultConnectSettings()
	proxySettings.ProxySettings = testProxySettings()
	if _, err := proxySettings.DialContext(ctx, "udp4", canaryAddress); !errors.Is(err, ErrExtenderPacketCarrierUnavailable) {
		t.Fatalf("the dial failed with %v, expected the proxy's refusal", err)
	}

	// the owner's endpoint carries the datagram, and only the destination's
	// datagrams come back through it
	socks := newTestSocksRecorder(t)
	factoryCount := atomic.Int64{}
	endpoints := make(chan net.PacketConn, 4)
	endpointSettings := DefaultConnectSettings()
	endpointSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: socks.listener.Addr().String(),
	}
	endpointSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
			factoryCount.Add(1)
			endpoint, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err == nil {
				endpoints <- endpoint
			}
			return endpoint, err
		},
	}
	if _, err := endpointSettings.DialContext(ctx, "udp", "dns.space.example:53"); err == nil {
		t.Fatal("a proxied space dialed udp to a name, which this host would look up")
	}
	if count := factoryCount.Load(); count != 0 {
		t.Fatalf("a refused dial opened %d endpoints", count)
	}
	conn, err := endpointSettings.DialContext(ctx, "udp", canaryAddress)
	if err != nil {
		t.Fatalf("the owner's endpoint was refused: %v", err)
	}
	defer conn.Close()
	endpoint := <-endpoints
	if _, err := conn.Write([]byte("probe")); err != nil {
		t.Fatal(err)
	}
	canary.SetReadDeadline(time.Now().Add(10 * time.Second))
	buffer := make([]byte, 64)
	n, from, err := canary.ReadFrom(buffer)
	if err != nil {
		t.Fatalf("the datagram never reached its destination: %v", err)
	}
	if string(buffer[:n]) != "probe" || from.String() != endpoint.LocalAddr().String() {
		t.Fatalf("the destination got %q from %s, expected the probe from the owner's endpoint %s", buffer[:n], from, endpoint.LocalAddr())
	}
	// a stranger's datagram first, then the destination's: only the
	// destination's is read
	stranger := newTestUdpCanary(t)
	if _, err := stranger.WriteTo([]byte("stranger"), endpoint.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if _, err := canary.WriteTo([]byte("answer"), from); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err = conn.Read(buffer)
	if err != nil || string(buffer[:n]) != "answer" {
		t.Fatalf("read %q, %v, expected the destination's answer alone", buffer[:n], err)
	}
	// the endpoint stands in for the udp alone: a stream dial still goes to the
	// proxy
	if conn, err := endpointSettings.DialContext(ctx, "tcp", "192.0.2.10:443"); err == nil {
		conn.Close()
		t.Fatal("the recording proxy accepted a connect")
	}
	waitTestSocksTargets(t, ctx, socks, nil, "ip 192.0.2.10:443")

	// with no proxy, this host's socket, as before
	hostConn, err := DefaultConnectSettings().DialContext(ctx, "udp", canaryAddress)
	if err != nil {
		t.Fatalf("a space with no proxy was refused a udp socket: %v", err)
	}
	hostConn.Close()
}

// A socks proxy that accepts and never answers fails a dial through it within
// ConnectTimeout, as an unanswered connect does. Before, the negotiation waited
// as long as the caller's context, and the exit nat dials with one that has no
// deadline, so such a proxy held each exit flow's dial for the flow's life.
func TestProxiedDialIsBoundedByConnectTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptedConns := []net.Conn{}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// held open and never answered
			acceptedConns = append(acceptedConns, conn)
		}
	}()
	defer func() {
		listener.Close()
		<-acceptDone
		for _, conn := range acceptedConns {
			conn.Close()
		}
	}()

	settings := DefaultConnectSettings()
	settings.ConnectTimeout = 100 * time.Millisecond
	settings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: listener.Addr().String(),
	}
	// stands in for the flow's context: the test's bound is only a backstop
	outerTimeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), outerTimeout)
	defer cancel()
	startTime := time.Now()
	conn, err := settings.DialContext(ctx, "tcp", "192.0.2.10:443")
	elapsed := time.Since(startTime)
	if err == nil {
		conn.Close()
		t.Fatal("a proxy that never answered connected")
	}
	if outerTimeout/2 <= elapsed {
		t.Fatalf("the dial took %s, bounded only by the caller's context, expected about the connect timeout", elapsed)
	}
}

// A dial through a proxy leaves the process-wide family ledger alone: its
// connection's family is the leg to the proxy, so a handshake that stalls
// through the proxy demotes nothing, retries over no other family, and a
// failed connect undoes no demotion in force. Without a proxy the same stall
// demotes, as before. Before, one slow proxy demoted this host's family for
// every dial of the process, a miner's other slots included.
func TestProxiedDialLeavesTheFamilyLedgerAlone(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	controlFamilyClear()
	defer controlFamilyClear()
	SetControlIpFamilyPolicy(IpFamilyAuto)

	dialCount := atomic.Int64{}
	// the first leg is v6 unless the dial is narrowed to v4, and only the v6
	// handshake stalls, the shape that demotes a family
	dial := func(ctx context.Context, network string, addr string) (net.Conn, error) {
		dialCount.Add(1)
		if network == "tcp4" {
			return &stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.80"), Port: 1080}}, nil
		}
		return &stubConn{remote: &net.TCPAddr{IP: net.ParseIP("2001:db8::1080"), Port: 1080}}, nil
	}
	stall := func(ctx context.Context, conn net.Conn) (net.Conn, error) {
		if connFamily(conn) == 6 {
			return nil, &timeoutError{}
		}
		return conn, nil
	}

	proxied := DefaultConnectSettings()
	proxied.ProxySettings = testProxySettings()
	_, err := dialControlTlsWithFamilyFallback(context.Background(), proxied, "tcp", "api.space.example:443", dial, stall)
	if family := controlFamilyDemotedFamily(); family != 0 {
		t.Fatalf("a dial through a proxy demoted ipv%d", family)
	}
	if count := dialCount.Load(); count != 1 {
		t.Fatalf("a dial through a proxy was dialed %d times, retried over another family, expected once", count)
	}
	if err == nil {
		t.Fatal("a stalled handshake answered")
	}

	// a demotion in force stands when a dial through a proxy fails to connect
	if !controlFamilyDemote(6) {
		t.Fatal("the demotion did not take")
	}
	refused := func(ctx context.Context, network string, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connect: connection refused")}
	}
	if _, err := dialControlTlsWithFamilyFallback(context.Background(), proxied, "tcp", "api.space.example:443", refused, stall); err == nil {
		t.Fatal("a refused dial answered")
	}
	if family := controlFamilyDemotedFamily(); family != 6 {
		t.Fatalf("a dial through a proxy undid the demotion of ipv6, the ledger reads ipv%d", family)
	}
	controlFamilyClear()

	// the control: the same stall without a proxy demotes the family it saw
	// and recovers over the other
	dialCount.Store(0)
	if _, err := dialControlTlsWithFamilyFallback(context.Background(), DefaultConnectSettings(), "tcp", "api.space.example:443", dial, stall); err != nil {
		t.Fatalf("a direct dial did not recover over ipv4: %v", err)
	}
	if family := controlFamilyDemotedFamily(); family != 6 {
		t.Fatalf("a direct stall demoted ipv%d, expected ipv6", family)
	}
}

// A provider exit whose egress is a proxy sends no udp from this host: a
// datagram's flow is refused at once, answered with an unreachable so the
// source moves on, and the destination receives nothing, even where a headless
// host's own stream dial would have opened a socket. Its tcp reaches the
// destination through the proxy. Before, the exit's udp was handed to that
// stream dial, which sent it from this host.
func TestProxiedExitNatSendsNoUdpFromThisHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	socks := newTestSocksRecorder(t)
	udpCanary := newTestUdpCanary(t)
	udpCanaryPort := udpCanary.LocalAddr().(*net.UDPAddr).Port
	tcpLeaked := make(chan string, 4)
	tcpCanary := newTestLeakCanary(t, "the exit's tcp destination", tcpLeaked)
	tcpCanaryPort := tcpCanary.Addr().(*net.TCPAddr).Port

	streamDialCount := atomic.Int64{}
	settings := DefaultLocalUserNatSettings()
	tcpSettings := *settings.TcpBufferSettings
	udpSettings := *settings.UdpBufferSettings
	tcpSettings.ProxySettings = &ProxySettings{
		Network: "tcp",
		Address: socks.listener.Addr().String(),
	}
	udpSettings.ProxySettings = tcpSettings.ProxySettings
	// a headless host's own dial on the udp side, which a proxied exit's udp
	// must never reach
	udpSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			streamDialCount.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	settings.TcpBufferSettings = &tcpSettings
	settings.UdpBufferSettings = &udpSettings
	localUserNat := NewLocalUserNat(ctx, "proxied exit", settings)
	defer localUserNat.Close()

	// the flow's answer: an icmp packet back toward the source on the udp flow
	unreachables := make(chan *IpPath, 4)
	unsub := localUserNat.AddReceivePacketCallback(func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		if ipPath.Protocol != IpProtocolUdp || len(packet) < Ipv4HeaderSizeWithoutExtensions || packet[0]>>4 != 4 || packet[9] != byte(ipProtocolNumberIcmp4) {
			return
		}
		select {
		case unreachables <- ipPath:
		default:
		}
	})
	defer unsub()
	// whatever reaches the destination left this host
	leaked := make(chan string, 1)
	go func() {
		buffer := make([]byte, 64)
		n, _, err := udpCanary.ReadFrom(buffer)
		if err == nil {
			leaked <- string(buffer[:n])
		}
	}()

	source := SourceId(NewId())
	datagram := testingUdp4Packet("10.0.0.1", "127.0.0.1", udpCanaryPort, []byte("exit"))
	if !localUserNat.SendPacket(source, protocol.ProvideMode_Public, datagram, -1) {
		t.Fatal("the datagram was not queued")
	}
	select {
	case ipPath := <-unreachables:
		if ipPath.DestinationPort != udpCanaryPort {
			t.Fatalf("an unreachable answered %+v", ipPath)
		}
	case payload := <-leaked:
		t.Fatalf("the exit's datagram %q left this host outside the proxy", payload)
	case <-ctx.Done():
		t.Fatal("the exit's refused flow was never answered")
	}
	select {
	case payload := <-leaked:
		t.Fatalf("the exit's datagram %q left this host outside the proxy", payload)
	default:
	}
	if count := streamDialCount.Load(); count != 0 {
		t.Fatalf("the exit handed its udp to the stream dial %d times", count)
	}

	syn := testingTcp4SynPacket(t, "10.0.0.1", 40001, "127.0.0.1", tcpCanaryPort)
	if !localUserNat.SendPacket(source, protocol.ProvideMode_Public, syn, -1) {
		t.Fatal("the syn was not queued")
	}
	waitTestSocksTargets(t, ctx, socks, tcpLeaked, fmt.Sprintf("ip 127.0.0.1:%d", tcpCanaryPort))
}

// One tcp syn with checksums, as a tunnel delivers it.
func testingTcp4SynPacket(t *testing.T, sourceIp string, sourcePort int, destinationIp string, destinationPort int) []byte {
	t.Helper()
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    net.ParseIP(sourceIp).To4(),
		DstIP:    net.ParseIP(destinationIp).To4(),
		Protocol: layers.IPProtocolTCP,
	}
	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(sourcePort),
		DstPort: layers.TCPPort(destinationPort),
		Seq:     0x01020304,
		SYN:     true,
		Window:  65535,
	}
	tcp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true}, ip, tcp); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, len(buffer.Bytes()))
	copy(packet, buffer.Bytes())
	return packet
}

// An exit whose egress is a proxy sends no icmp echo of this host's: the nat
// marks its echo settings when its tcp or udp egress names a proxy, and an
// echo flow so marked ends without asking the platform for an echo socket. An
// exit with no proxy asks, as before.
func TestProxiedExitNatOpensNoEchoSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	natIcmpSettings := func(proxied bool) *IcmpBufferSettings {
		settings := DefaultLocalUserNatSettings()
		if proxied {
			udpSettings := *settings.UdpBufferSettings
			udpSettings.ProxySettings = testProxySettings()
			settings.UdpBufferSettings = &udpSettings
		}
		localUserNat := NewLocalUserNat(ctx, "echo exit", settings)
		defer localUserNat.Close()
		return localUserNat.settings.IcmpBufferSettings
	}
	if !natIcmpSettings(true).proxiedEgress {
		t.Fatal("a proxied exit's echo settings were not marked")
	}
	if natIcmpSettings(false).proxiedEgress {
		t.Fatal("an exit with no proxy marked its echo settings")
	}

	runEchoFlow := func(icmpSettings *IcmpBufferSettings) int64 {
		askedCount := atomic.Int64{}
		settings := *icmpSettings
		settings.Log = NewNoopLogger()
		settings.newEgressForTest = func(context.Context, int, net.IP, *IcmpBufferSettings) (icmpEgress, error) {
			askedCount.Add(1)
			// fail soft, so the flow ends here either way
			return nil, errors.New("the test backend opens nothing")
		}
		sequence := NewIcmpSequence(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
			SourceId(NewId()), protocol.ProvideMode_Public, 4,
			net.IPv4(10, 0, 0, 1).To4(), 7, net.IPv4(198, 51, 100, 3).To4(), &settings)
		if sequence == nil {
			t.Fatal("could not admit an echo flow")
		}
		runDone := make(chan struct{})
		go func() {
			defer close(runDone)
			sequence.Run()
		}()
		select {
		case <-runDone:
		case <-ctx.Done():
			t.Fatal("the echo flow did not end")
		}
		return askedCount.Load()
	}
	if count := runEchoFlow(natIcmpSettings(true)); count != 0 {
		t.Fatalf("a proxied exit asked for an echo socket of this host's %d times", count)
	}
	if count := runEchoFlow(natIcmpSettings(false)); count != 1 {
		t.Fatalf("an exit with no proxy asked for an echo socket %d times, expected once", count)
	}
}

// A client whose peer connections are off takes no StreamOpen, in a reset or
// alone, whichever way the stream runs, so no p2p transport exists to gather
// this host's candidates, and its manager refuses a peer connection to
// anything that asks without building its factory. A client with them on
// opens the stream, as before.
func TestClientWithPeerConnectionsOffOpensNoStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newProviderClient := func(peerConnectionsOff bool) *Client {
		settings := streamManagerTestSettings()
		settings.ProviderStreamPolicy = true
		webRtcSettings := *settings.WebRtcSettings
		webRtcSettings.DisablePeerConnections = peerConnectionsOff
		settings.WebRtcSettings = &webRtcSettings
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Public: true,
		})
		return client
	}
	receiveControl := func(client *Client, message proto.Message) {
		frame, err := ToFrame(message, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		client.streamManager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	}

	client := newProviderClient(true)
	defer client.Close()
	// runs on the receive goroutine inside every stream open, so a count
	// read after the receive returns is the opens it started
	openCount := atomic.Int64{}
	client.streamManager.streamBuffer.afterStreamOpenGenerationForTest = func(*streamOpenRequest) {
		openCount.Add(1)
	}
	factoryCount := atomic.Int64{}
	func() {
		manager := client.WebRtcManager()
		manager.peerConnectionFactoryLock.Lock()
		defer manager.peerConnectionFactoryLock.Unlock()
		manager.newPeerConnectionFactory = func(*WebRtcSettings, *webrtc.Certificate) (*webRtcPeerConnectionFactory, *webrtc.Certificate, error) {
			factoryCount.Add(1)
			return nil, nil, errors.New("the test builds no factory")
		}
	}()

	peerId := NewId()
	otherPeerId := NewId()
	streamIds := []Id{NewId(), NewId(), NewId()}
	opens := []*protocol.StreamOpen{
		{SourceId: peerId.Bytes(), StreamId: streamIds[0].Bytes()},
		{DestinationId: peerId.Bytes(), StreamId: streamIds[1].Bytes()},
		{SourceId: peerId.Bytes(), DestinationId: otherPeerId.Bytes(), StreamId: streamIds[2].Bytes()},
	}
	for _, open := range opens {
		receiveControl(client, open)
	}
	receiveControl(client, &protocol.StreamReset{Streams: opens})
	if count := openCount.Load(); count != 0 {
		t.Fatalf("a client with peer connections off started %d stream opens", count)
	}
	for _, streamId := range streamIds {
		if client.streamManager.IsStreamOpen(streamId) {
			t.Fatalf("a client with peer connections off opened s(%s)", streamId)
		}
	}
	if _, err := client.WebRtcManager().NewP2pConnActive(ctx, TransferPath{SourceId: client.ClientId(), DestinationId: peerId, StreamId: streamIds[0]}); !errors.Is(err, ErrPeerConnectionsDisabled) {
		t.Fatalf("the manager answered a peer connection with %v, expected its refusal", err)
	}
	if count := factoryCount.Load(); count != 0 {
		t.Fatalf("the manager built its peer connection factory %d times", count)
	}

	// the control: with peer connections on, the same open opens
	onClient := newProviderClient(false)
	defer onClient.Close()
	openTracker := newStreamOpenTestTracker(onClient.streamManager.streamBuffer)
	publication := openTracker.expectPublication(streamIds[0])
	receiveControl(onClient, opens[0])
	publication.wait(t)
	if !onClient.streamManager.IsStreamOpen(streamIds[0]) {
		t.Fatal("a client with peer connections on did not open the stream")
	}
	publication.resume()
}

// A client whose egress is a proxy keeps the platform's ObservedAddresses out
// of the process's provider-local guard: they are its proxy's, and each
// client's message replaces the guard's set, so a proxied slot would replace
// this host's own public address, which a direct slot's exit and extender
// forward are guarded against. A client with no proxy applies them, as before.
// Before, every client applied them.
func TestProxiedClientKeepsObservedAddressesOutOfTheHostGuard(t *testing.T) {
	hostAddress := netip.MustParseAddr("203.0.113.50")
	proxyAddress := netip.MustParseAddr("198.51.100.80")

	direct := newObservedAddressesHarness(t)
	direct.deliver(ControlId, observedAddressesFrame(hostAddress.AsSlice()))
	if observed := direct.observed(); !slices.Equal(observed, []netip.Addr{hostAddress}) {
		t.Fatalf("a direct client's guard holds %v, expected this host's address", observed)
	}

	proxied := newObservedAddressesHarnessWithSettings(t, func(settings *ClientSettings) {
		settings.SkipObservedAddresses = true
	})
	// the guard as a direct slot of the same process left it
	proxied.networks.setObservedAddresses([]netip.Addr{hostAddress})
	proxied.deliver(ControlId, observedAddressesFrame(proxyAddress.AsSlice()))
	if observed := proxied.observed(); !slices.Equal(observed, []netip.Addr{hostAddress}) {
		t.Fatalf("a proxied client replaced this host's address in the guard with %v", observed)
	}
}
