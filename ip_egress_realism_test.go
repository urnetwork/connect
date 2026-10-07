//go:build unix || windows

// Provider packet-to-socket regressions. Loopback is the only destination;
// source identities and packet addresses are synthetic.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Owns the NAT and real upstream socket until test cleanup joins every worker.
type providerEgressTestFlow struct {
	ctx             context.Context
	nat             *LocalUserNat
	socket          net.Conn
	peer            net.Conn
	udpSink         *net.UDPConn
	path            *IpPath
	source          TransferPath
	sharedLifecycle bool
	tcpResponses    chan struct{}
}

// Builds a valid packet with the client's chosen TTL, including its checksum.
func providerEgressTestPacket(path *IpPath, ttl uint8) []byte {
	var packet []byte
	if path.Protocol == IpProtocolTcp {
		packet = ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil)
	} else {
		packet = ipOosUdpPacket(path, []byte("provider ttl test"))
	}
	if path.Version == 4 {
		packet[8] = ttl
		packet[10], packet[11] = 0, 0
		binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[:20])))
	} else {
		packet[7] = ttl
	}
	return MessagePoolCopy(packet)
}

// Starts a real NAT flow, optionally through the reliable provider receipt
// path, and waits for the actual socket-configuration boundary.
func newProviderEgressTestFlow(t *testing.T, version int, ipProtocol IpProtocol,
	settings *LocalUserNatSettings, ttl uint8, reliable bool, prepareListeners ...func(*net.TCPListener)) *providerEgressTestFlow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	sourceIp, destinationIp := testUdpFlowIps(version)
	f := &providerEgressTestFlow{ctx: ctx, source: SourceId(NewId()), path: &IpPath{
		Version: version, Protocol: ipProtocol, SourceIp: sourceIp, SourcePort: 42000,
		DestinationIp: destinationIp,
	}}
	network := "tcp4"
	if version == 6 {
		network = "tcp6"
	}
	var listener *net.TCPListener
	var err error
	if ipProtocol == IpProtocolTcp {
		listener, err = net.ListenTCP(network, &net.TCPAddr{IP: destinationIp})
		if err == nil {
			t.Cleanup(func() { listener.Close() })
			listener.SetDeadline(time.Now().Add(15 * time.Second))
			f.path.DestinationPort = listener.Addr().(*net.TCPAddr).Port
			for _, prepare := range prepareListeners {
				prepare(listener)
			}
		}
	} else {
		network = "udp4"
		if version == 6 {
			network = "udp6"
		}
		f.udpSink, err = net.ListenUDP(network, &net.UDPAddr{IP: destinationIp})
		if err == nil {
			t.Cleanup(func() { f.udpSink.Close() })
			f.udpSink.SetReadDeadline(time.Now().Add(15 * time.Second))
			f.path.DestinationPort = f.udpSink.LocalAddr().(*net.UDPAddr).Port
		}
	}
	if err != nil {
		t.Fatalf("listen ipv%d %s: %v", version, ipProtocol, err)
	}
	type openedSocket struct {
		conn   net.Conn
		shared bool
	}
	sockets := make(chan openedSocket, 1)
	settings.TcpBufferSettings.afterUpstreamConnectForTest = func(conn *net.TCPConn) { sockets <- openedSocket{conn: conn} }
	settings.UdpBufferSettings.afterSocketOpenForTest = func(sequence *UdpSequence, conn net.Conn) {
		sockets <- openedSocket{conn: conn, shared: sequence.sharedSocketLifecycle}
	}
	settings.Log = NewNoopLogger()
	f.nat = NewLocalUserNat(ctx, "provider egress test", settings)
	if ipProtocol == IpProtocolTcp {
		f.tcpResponses = make(chan struct{}, 2)
		f.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			headerByteCount := Ipv4HeaderSizeWithoutExtensions
			if version == 6 {
				headerByteCount = Ipv6HeaderSize
			}
			if packet[headerByteCount+13]&(tcpFlagSyn|tcpFlagAck) == tcpFlagSyn|tcpFlagAck {
				select {
				case f.tcpResponses <- struct{}{}:
				case <-ctx.Done():
				}
			}
		})
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := f.nat.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	})
	packet := providerEgressTestPacket(f.path, ttl)
	if reliable {
		// The ReceiveSequence receipt forces providerReliablePacket.enterTcp;
		// ordinary SendPacket cannot prove this separate parser retains TTL.
		clientSettings := DefaultClientSettings()
		clientSettings.Log = NewNoopLogger()
		clientSettings.EncryptionSettings.Mode = EncryptionModeOff
		clientSettings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		provider := NewRemoteUserNatProvider(client, f.nat, providerSettings)
		t.Cleanup(func() {
			cancel()
			provider.Close()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer closeCancel()
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Error(err)
			}
		})
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		sequence := NewReceiveSequence(ctx, client, f.source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		sequence.deliverItems = []*receiveItem{{transferItem: transferItem{messageId: NewId(), sequenceNumber: 0},
			receiveCallback: client.receiveCallback, ack: true, frames: []*protocol.Frame{frame}}}
		sequence.deliverFrames = []*protocol.Frame{frame}
		sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
		done := make(chan struct{})
		go func() { defer close(done); runReliableIngressFixtureDelivery(sequence) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("reliable delivery did not join")
			}
		})
	} else if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, packet, 0) {
		MessagePoolReturn(packet)
		t.Fatal("packet did not enter NAT")
	}
	select {
	case opened := <-sockets:
		f.socket, f.sharedLifecycle = opened.conn, opened.shared
	case <-ctx.Done():
		t.Fatalf("ipv%d %s socket was not configured: %v", version, ipProtocol, ctx.Err())
	}
	if listener != nil {
		f.peer, err = listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.peer.Close() })
		select {
		case <-f.tcpResponses:
		case <-ctx.Done():
			t.Fatal("upstream connection never produced a SYN-ACK")
		}
	} else {
		f.receiveDatagram(t)
	}
	return f
}

// Datagrams must reach the origin with exactly the client's application bytes.
func (self *providerEgressTestFlow) receiveDatagram(t *testing.T) {
	t.Helper()
	buffer := make([]byte, 128)
	n, _, err := self.udpSink.ReadFromUDP(buffer)
	if err != nil || !bytes.Equal(buffer[:n], []byte("provider ttl test")) {
		t.Fatalf("upstream datagram = %q, err=%v", buffer[:n], err)
	}
}

// Reads the real socket rather than the requested target or a mock setter.
func providerEgressTestSocketTtl(t *testing.T, conn net.Conn, version int) int {
	t.Helper()
	raw, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ttl int
	var optionErr error
	if err = raw.Control(func(fd uintptr) { ttl, optionErr = providerSocketTtl(fd, version == 6) }); err != nil {
		t.Fatal(err)
	}
	if optionErr != nil {
		t.Fatal(optionErr)
	}
	return ttl
}

// A real SYN/datagram must carry its header value through dispatch, flow
// creation, context, and the native socket option in both address families.
func TestProviderEgressTcpMirrorsClientTtl(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		settings := DefaultLocalUserNatSettingsWithBufferSize(2)
		settings.TcpBufferSettings.EgressTtlMode = EgressTtlModeMirror
		f := newProviderEgressTestFlow(t, version, IpProtocolTcp, settings, 37, false)
		if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 37 {
			t.Fatalf("ipv%d TTL=%d", version, ttl)
		}
		stats := f.nat.EgressRealismStats()
		if stats.TcpFlowCount != 1 || stats.MirrorFlowCount != 1 || stats.PreConnectSocketCount != 1 || stats.PostConnectSocketCount != 0 || stats.TtlApplyCount != 1 {
			t.Fatalf("ipv%d stats=%+v", version, stats)
		}
		// The same initial sequence number is a retransmission, not a new
		// flow. Its different TTL must not retarget an established socket.
		retransmit := providerEgressTestPacket(f.path, 99)
		if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, retransmit, 0) {
			MessagePoolReturn(retransmit)
			t.Fatal("SYN retransmission rejected")
		}
		select {
		case <-f.tcpResponses:
		case <-f.ctx.Done():
			t.Fatal("SYN retransmission not processed")
		}
		if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 37 || f.nat.EgressRealismStats() != stats {
			t.Fatalf("SYN retransmission changed initial target: TTL=%d stats=%+v", ttl, f.nat.EgressRealismStats())
		}
	}
}

// Reliable ownership uses another TCP parser. This fails if only the ordinary
// NAT dispatcher captures TTL, even though the ordinary SYN tests pass.
func TestProviderReliableDeliveryPreservesTtlBeforeSocketCreation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		settings := DefaultLocalUserNatSettingsWithBufferSize(2)
		settings.TcpBufferSettings.EgressTtlMode = EgressTtlModeMirror
		f := newProviderEgressTestFlow(t, version, IpProtocolTcp, settings, 123, true)
		if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 123 {
			t.Fatalf("reliable ipv%d TTL=%d", version, ttl)
		}
		stats := f.nat.EgressRealismStats()
		if stats.PreConnectSocketCount != 1 || stats.TtlApplyCount != 1 || stats.PostConnectSocketCount != 0 || stats.InvalidTtlFlowCount != 0 {
			t.Fatalf("reliable ipv%d stats=%+v", version, stats)
		}
	}
}

// The shared UDP lifecycle opens at sequence creation, before its first send
// item. Both lifecycles must capture then and retain that value for the flow.
func TestProviderEgressUdpMirrorsFirstDatagramTtl(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, shared := range []bool{false, true} {
			settings := DefaultLocalUserNatSettingsWithBufferSize(2)
			settings.UdpBufferSettings.EgressTtlMode = EgressTtlModeMirror
			settings.UdpBufferSettings.SharedSocketLifecycle = shared
			if shared {
				settings.UdpBufferSettings.SocketReadShardCount = 1
			}
			f := newProviderEgressTestFlow(t, version, IpProtocolUdp, settings, 41, false)
			if shared && (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && !f.sharedLifecycle {
				t.Fatal("shared UDP test silently took the legacy lifecycle")
			}
			if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 41 {
				t.Fatalf("ipv%d shared=%t first TTL=%d", version, shared, ttl)
			}
			packet := providerEgressTestPacket(f.path, 93)
			if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, packet, 0) {
				MessagePoolReturn(packet)
				t.Fatal("second datagram rejected")
			}
			f.receiveDatagram(t)
			if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 41 {
				t.Fatalf("ipv%d shared=%t later datagram changed TTL=%d", version, shared, ttl)
			}
			stats := f.nat.EgressRealismStats()
			if stats.UdpFlowCount != 1 || stats.TtlApplyCount != 1 || stats.PreConnectSocketCount != 1 {
				t.Fatalf("ipv%d shared=%t stats=%+v", version, shared, stats)
			}
			// A second flow aggregates into the same NAT, while other cases'
			// still-live NATs have never polluted the count above.
			secondPath := *f.path
			secondPath.SourcePort++
			second := providerEgressTestPacket(&secondPath, 52)
			if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, second, 0) {
				MessagePoolReturn(second)
				t.Fatal("second flow rejected")
			}
			f.receiveDatagram(t)
			if stats := f.nat.EgressRealismStats(); stats.UdpFlowCount != 2 || stats.TtlApplyCount != 2 {
				t.Fatalf("NAT did not aggregate its flows: %+v", stats)
			}
		}
	}
}

// Refused options degrade only realism, leaving the original flow available.
func TestProviderEgressRefusedTtlStillForwards(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
		settings := DefaultLocalUserNatSettingsWithBufferSize(2)
		settings.TcpBufferSettings.EgressTtlMode = EgressTtlModeMirror
		settings.UdpBufferSettings.EgressTtlMode = EgressTtlModeMirror
		var connectSettings *ConnectSettings
		if ipProtocol == IpProtocolTcp {
			connectSettings = &settings.TcpBufferSettings.ConnectSettings
		} else {
			connectSettings = &settings.UdpBufferSettings.ConnectSettings
		}
		base := *connectSettings
		connectSettings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dial := ctx.Value(providerEgressDialKey{}).(*providerEgressDial)
			dial.target.setTtlForTest = func(uintptr, bool, int) error { return syscall.EPERM }
			return base.DialContext(ctx, network, address)
		}}
		f := newProviderEgressTestFlow(t, 4, ipProtocol, settings, 37, false)
		stats := f.nat.EgressRealismStats()
		if stats.TtlApplyErrorCount != 1 || stats.TtlApplyCount != 0 || stats.PreConnectSocketCount != 1 || stats.PostConnectSocketCount != 0 {
			t.Fatalf("%s refusal stats=%+v", ipProtocol, stats)
		}
	}
}

// A raw-descriptor failure is separate from a refused option and never masks
// an error from the caller's preexisting control chain.
type providerEgressFailedRawConn struct{ controls int }

// Refuses descriptor access before the socket-option callback can run.
func (self *providerEgressFailedRawConn) Control(func(uintptr)) error {
	self.controls++
	return syscall.EBADF
}

// This fixture never provides socket I/O.
func (self *providerEgressFailedRawConn) Read(func(uintptr) bool) error { return syscall.EBADF }

// This fixture never provides socket I/O.
func (self *providerEgressFailedRawConn) Write(func(uintptr) bool) error { return syscall.EBADF }

// Best-effort realism must preserve existing control errors and public dialer
// customization, including a preexisting context-aware control callback.
func TestProviderEgressControlPreservesFailuresAndLaterControl(t *testing.T) {
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	ctx, _ := target.dialContext(context.Background())
	dialer := DefaultConnectSettings().NetDialer()
	raw := &providerEgressFailedRawConn{}
	if err := dialer.ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); err != nil || raw.controls != 1 || counters.socketUnavailableCount.Load() != 1 {
		t.Fatalf("raw failure aborted dial: err=%v controls=%d unavailable=%d", err, raw.controls, counters.socketUnavailableCount.Load())
	}
	wantErr := errors.New("existing interface/buffer control failure")
	dialer.Control = func(string, string, syscall.RawConn) error { return wantErr }
	if err := dialer.ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); !errors.Is(err, wantErr) || raw.controls != 1 {
		t.Fatalf("existing control failure lost: err=%v controls=%d", err, raw.controls)
	}
	contextDialer := providerEgressDialer(&net.Dialer{ControlContext: func(context.Context, string, string, syscall.RawConn) error { return wantErr }})
	if err := contextDialer.ControlContext(ctx, "tcp4", "127.0.0.1:9", raw); !errors.Is(err, wantErr) || raw.controls != 1 {
		t.Fatalf("existing context control failure lost: %v", err)
	}
}

// A readable raw handle can still refuse getsockopt/setsockopt. Those failures
// count independently and leave the hook successful.
type providerEgressInvalidFdRawConn struct{}

// Executes the option callback with a deterministically invalid descriptor.
func (self *providerEgressInvalidFdRawConn) Control(fn func(uintptr)) error {
	fn(^uintptr(0))
	return nil
}

// This fixture never provides socket I/O.
func (self *providerEgressInvalidFdRawConn) Read(func(uintptr) bool) error { return syscall.EBADF }

// This fixture never provides socket I/O.
func (self *providerEgressInvalidFdRawConn) Write(func(uintptr) bool) error { return syscall.EBADF }

// Getsockopt failure still permits a write attempt and does not fail the dial.
func TestProviderEgressSocketOptionReadFailuresAreBestEffort(t *testing.T) {
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	ctx, _ := target.dialContext(context.Background())
	err := DefaultConnectSettings().NetDialer().ControlContext(ctx, "tcp4", "127.0.0.1:9", &providerEgressInvalidFdRawConn{})
	if err != nil || counters.ttlReadErrorCount.Load() != 1 || counters.ttlApplyErrorCount.Load() != 1 || counters.socketUnavailableCount.Load() != 0 {
		t.Fatalf("option failure not best effort: err=%v read=%d write=%d raw=%d", err, counters.ttlReadErrorCount.Load(), counters.ttlApplyErrorCount.Load(), counters.socketUnavailableCount.Load())
	}
}

// Forces a known, nonstandard baseline through the existing hook, so shadow
// diagnostics must read the actual socket rather than guess the OS default.
func providerEgressTestBaseline(settings *ConnectSettings, ttl int) {
	previous := settings.DialControl
	settings.DialControl = func(network, address string, raw syscall.RawConn) error {
		if previous != nil {
			if err := previous(network, address, raw); err != nil {
				return err
			}
		}
		var optionErr error
		if err := raw.Control(func(fd uintptr) { optionErr = setProviderSocketTtl(fd, network == "tcp6" || network == "udp6", ttl) }); err != nil {
			return err
		}
		return optionErr
	}
}

// Mirror is the explicit shipping default; independently allocated settings
// still offer shadow/native and preserve the existing keepalive cadence.
func TestProviderEgressDefaultsEnableMirror(t *testing.T) {
	for _, settings := range []*LocalUserNatSettings{
		DefaultLocalUserNatSettings(), DefaultLocalUserNatSettingsWithBufferSize(1),
		DefaultProviderLocalUserNatSettings(), DefaultProviderLocalUserNatSettingsWithMemoryTarget(mib(4)),
	} {
		if settings.TcpBufferSettings.EgressTtlMode != EgressTtlModeMirror || settings.UdpBufferSettings.EgressTtlMode != EgressTtlModeMirror {
			t.Fatal("default provider did not enable TTL mirroring")
		}
		if settings.TcpBufferSettings.ProviderKeepAliveConfig != nil {
			t.Fatal("provider default overrides existing keepalive")
		}
	}
	settings := DefaultConnectSettings()
	if settings.KeepAliveTimeout != 5*time.Second || settings.KeepAliveConfig != (net.KeepAliveConfig{Enable: true, Idle: 5 * time.Second, Interval: 5 * time.Second, Count: 1}) {
		t.Fatalf("control-plane keepalive changed: %+v", settings.KeepAliveConfig)
	}
}

// Both matching and mismatching targets are compared with the socket's actual
// configured baseline, and shadow leaves that value untouched.
func TestProviderEgressShadowReadsNativeTtlWithoutChangingIt(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			for _, clientTtl := range []uint8{91, 37} {
				settings := DefaultLocalUserNatSettingsWithBufferSize(2)
				settings.TcpBufferSettings.EgressTtlMode = EgressTtlModeShadow
				settings.UdpBufferSettings.EgressTtlMode = EgressTtlModeShadow
				providerEgressTestBaseline(&settings.TcpBufferSettings.ConnectSettings, 91)
				providerEgressTestBaseline(&settings.UdpBufferSettings.ConnectSettings, 91)
				f := newProviderEgressTestFlow(t, version, ipProtocol, settings, clientTtl, false)
				if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 91 {
					t.Fatalf("shadow ipv%d %s changed TTL to %d", version, ipProtocol, ttl)
				}
				want := EgressRealismStats{ShadowFlowCount: 1, PreConnectSocketCount: 1}
				if ipProtocol == IpProtocolTcp {
					want.TcpFlowCount = 1
				} else {
					want.UdpFlowCount = 1
				}
				if clientTtl == 91 {
					want.TtlMatchCount = 1
				} else {
					want.TtlMismatchCount = 1
				}
				if got := f.nat.EgressRealismStats(); got != want {
					t.Fatalf("shadow ipv%d %s TTL%d stats=%+v want=%+v", version, ipProtocol, clientTtl, got, want)
				}
			}
		}
	}
}

// Empty/custom legacy settings and explicit native mode make no observations
// and preserve any socket value supplied by the existing control hook.
func TestProviderEgressNativeSkipsObservationsAndSocketChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []EgressTtlMode{EgressTtlModeNative, "", "unrecognized"} {
		for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			settings := DefaultLocalUserNatSettingsWithBufferSize(2)
			settings.TcpBufferSettings.EgressTtlMode = mode
			settings.UdpBufferSettings.EgressTtlMode = mode
			providerEgressTestBaseline(&settings.TcpBufferSettings.ConnectSettings, 91)
			providerEgressTestBaseline(&settings.UdpBufferSettings.ConnectSettings, 91)
			f := newProviderEgressTestFlow(t, 4, ipProtocol, settings, 37, false)
			if ttl := providerEgressTestSocketTtl(t, f.socket, 4); ttl != 91 {
				t.Fatalf("native %q %s changed TTL to %d", mode, ipProtocol, ttl)
			}
			if stats := f.nat.EgressRealismStats(); stats != (EgressRealismStats{}) {
				t.Fatalf("native %q %s observed socket: %+v", mode, ipProtocol, stats)
			}
		}
	}
}

// Zero is invalid for unicast options. Valid small values remain available to
// probes rather than silently being replaced with a guessed OS profile.
func TestProviderEgressInvalidAndLowTtl(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, ttl := range []uint8{0, 1, 31, 255} {
			settings := DefaultLocalUserNatSettingsWithBufferSize(2)
			providerEgressTestBaseline(&settings.TcpBufferSettings.ConnectSettings, 91)
			f := newProviderEgressTestFlow(t, version, IpProtocolTcp, settings, ttl, false)
			want := int(ttl)
			if ttl == 0 {
				want = 91
			}
			if got := providerEgressTestSocketTtl(t, f.socket, version); got != want {
				t.Fatalf("ipv%d TTL%d got%d want%d", version, ttl, got, want)
			}
			stats := f.nat.EgressRealismStats()
			if ttl == 0 {
				if stats.InvalidTtlFlowCount != 1 || stats.PreConnectSocketCount != 0 || stats.PostConnectSocketCount != 0 || stats.TtlApplyCount != 0 {
					t.Fatalf("invalid TTL stats=%+v", stats)
				}
			} else if stats.InvalidTtlFlowCount != 0 || stats.TtlApplyCount != 1 {
				t.Fatalf("valid TTL%d stats=%+v", ttl, stats)
			}
		}
	}
}

// A host wrapper may preserve the private dial context and reach NetDialer.
// A truly opaque dial gets the observable post-connect fallback instead.
func TestProviderEgressHostWrappersAndOpaqueFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			for _, opaque := range []bool{false, true} {
				settings := DefaultLocalUserNatSettingsWithBufferSize(2)
				var connectSettings *ConnectSettings
				if ipProtocol == IpProtocolTcp {
					connectSettings = &settings.TcpBufferSettings.ConnectSettings
				} else {
					connectSettings = &settings.UdpBufferSettings.ConnectSettings
				}
				base := *connectSettings
				connectSettings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					if opaque {
						return (&net.Dialer{}).DialContext(ctx, network, address)
					}
					return base.DialContext(ctx, network, address)
				}}
				f := newProviderEgressTestFlow(t, version, ipProtocol, settings, 43, false)
				if ttl := providerEgressTestSocketTtl(t, f.socket, version); ttl != 43 {
					t.Fatalf("ipv%d %s opaque=%t TTL=%d", version, ipProtocol, opaque, ttl)
				}
				wantPre, wantPost := uint64(1), uint64(0)
				if opaque {
					wantPre, wantPost = 0, 1
				}
				stats := f.nat.EgressRealismStats()
				if stats.PreConnectSocketCount != wantPre || stats.PostConnectSocketCount != wantPost || stats.TtlApplyCount != 1 {
					t.Fatalf("ipv%d %s opaque=%t stats=%+v", version, ipProtocol, opaque, stats)
				}
			}
		}
	}
}

// Socketless wrappers must still carry a usable connection; diagnostic failure
// is not a reason to fail a flow, including an independent keepalive override.
type providerEgressOpaqueTestConn struct{ net.Conn }

// Exposes a keepalive setter that reports a platform refusal.
type providerEgressRefusedKeepAliveTestConn struct {
	net.Conn
	calls int
}

// Counts attempts so nil configuration is distinguishable from a refused one.
func (self *providerEgressRefusedKeepAliveTestConn) SetKeepAliveConfig(net.KeepAliveConfig) error {
	self.calls++
	return syscall.EPERM
}

// Missing socket access and refused keepalive remain local diagnostics while
// the returned connection continues carrying application bytes.
func TestProviderEgressOpaqueWrapperAndKeepAliveRefusal(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var counters egressRealismCounters
	target := newProviderEgressTarget(EgressTtlModeMirror, 37, ipProtocolNumberTcp, &counters)
	_, dial := target.dialContext(context.Background())
	wrapped := &providerEgressOpaqueTestConn{Conn: left}
	dial.afterConnect(wrapped)
	if counters.postConnectSocketCount.Load() != 1 || counters.socketUnavailableCount.Load() != 1 {
		t.Fatal("opaque socket failure not diagnosed")
	}
	config := &net.KeepAliveConfig{Enable: false}
	configureProviderKeepAlive(wrapped, config, &counters)
	refused := &providerEgressRefusedKeepAliveTestConn{Conn: left}
	configureProviderKeepAlive(refused, nil, &counters)
	if refused.calls != 0 {
		t.Fatal("nil provider override called keepalive setter")
	}
	configureProviderKeepAlive(refused, config, &counters)
	if refused.calls != 1 || counters.keepAliveApplyErrorCount.Load() != 2 {
		t.Fatal("keepalive refusal not diagnosed")
	}
	written := make(chan error, 1)
	go func() { _, err := wrapped.Write([]byte("ok")); written <- err }()
	buffer := make([]byte, 2)
	if _, err := right.Read(buffer); err != nil || string(buffer) != "ok" {
		t.Fatalf("wrapper stopped forwarding: %q %v", buffer, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}
