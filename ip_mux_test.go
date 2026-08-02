package connect

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// a recorder for sent (upstream) and received (downstream) packets
type ipMuxRecorder struct {
	mu       sync.Mutex
	sent     [][]byte
	received [][]byte
}

func (self *ipMuxRecorder) upstream(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.sent = append(self.sent, append([]byte{}, packet...))
	return true
}

func (self *ipMuxRecorder) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.received = append(self.received, append([]byte{}, packet...))
}

func (self *ipMuxRecorder) counts() (int, int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.sent), len(self.received)
}

func newIpMuxIpv4Packet(sourceIp net.IP, destinationIp net.IP) []byte {
	packet := make([]byte, Ipv4HeaderSizeWithoutExtensions)
	writeIpv4Header(packet, ipProtocolNumberUdp, sourceIp.To4(), destinationIp.To4())
	return packet
}

func TestIpMuxPassthrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rec := &ipMuxRecorder{}
	// onSend nil => pure pass-through
	mux := NewIpMux(ctx, tun, TransferPath{}, protocol.ProvideMode_Network, 0, nil, nil, rec.receive, nil)
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	// send path: not claimed => forwarded to upstream verbatim
	pkt := []byte("a-send-packet")
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, pkt, 0) {
		t.Fatal("SendPacket returned false")
	}
	if sent, _ := rec.counts(); sent != 1 {
		t.Fatalf("upstream got %d packets, want 1", sent)
	}

	// receive path: external destination => dispatched downstream
	external := &IpPath{Version: 4, Protocol: IpProtocolUdp, DestinationIp: net.ParseIP("8.8.8.8"), DestinationPort: 443}
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, external, []byte("a-receive-packet"))
	if _, received := rec.counts(); received != 1 {
		t.Fatalf("downstream got %d packets, want 1", received)
	}

	// Return callbacks carry the canonical outbound flow path, while the
	// packet itself has the reverse direction. The packet destination is the
	// authoritative mux-local identity.
	addrs := tun.LocalAddresses()
	if len(addrs) == 0 {
		t.Fatal("tun has no local address")
	}
	localIp := net.IP(addrs[0].AsSlice())
	canonicalOutbound := &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        localIp,
		SourcePort:      40000,
		DestinationIp:   net.ParseIP("1.1.1.1"),
		DestinationPort: 443,
	}
	returnPacket := newIpMuxIpv4Packet(canonicalOutbound.DestinationIp, canonicalOutbound.SourceIp)
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, canonicalOutbound, returnPacket)
	if _, received := rec.counts(); received != 1 {
		t.Fatalf("downstream got %d packets after mux-addressed receive, want still 1", received)
	}
}

func TestIpMuxReceiveDoesNotTrustMisleadingPathDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec := &ipMuxRecorder{}
	mux := NewIpMux(ctx, tun, TransferPath{}, protocol.ProvideMode_Network, 0, nil, nil, rec.receive, nil)
	defer mux.Close()

	localIp := net.IP(tun.LocalAddresses()[0].AsSlice())
	misleadingPath := &IpPath{
		Version:       4,
		Protocol:      IpProtocolUdp,
		DestinationIp: localIp,
	}
	packetForOs := newIpMuxIpv4Packet(net.ParseIP("1.1.1.1"), net.ParseIP("10.0.0.2"))
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, misleadingPath, packetForOs)
	if _, received := rec.counts(); received != 1 {
		t.Fatalf("packet bytes addressed downstream were intercepted from misleading metadata: received=%d, want 1", received)
	}
}

func TestIpMuxReceiveRoutesLocalPacketWithoutPathMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tun, err := CreateTunWithDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec := &ipMuxRecorder{}
	mux := NewIpMux(ctx, tun, TransferPath{}, protocol.ProvideMode_Network, 0, nil, nil, rec.receive, nil)
	defer mux.Close()

	localIp := net.IP(tun.LocalAddresses()[0].AsSlice())
	packet := newIpMuxIpv4Packet(net.ParseIP("1.1.1.1"), localIp)
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, nil, packet)
	if _, received := rec.counts(); received != 0 {
		t.Fatalf("mux-local packet without metadata reached downstream: received=%d, want 0", received)
	}
}

func TestIpMuxLocalPacketDestinationSupportsIpv6(t *testing.T) {
	local := netip.MustParseAddr("fd00::53")
	mux := &IpMux{localAddresses: []netip.Addr{local}}
	packet := make([]byte, 40)
	packet[0] = 0x60
	copy(packet[24:40], local.AsSlice())
	if !mux.isLocalPacketDestination(packet) {
		t.Fatal("IPv6 packet addressed to mux was not classified local")
	}
}

func TestIpMuxLocalPacketDestinationDoesNotAllocate(t *testing.T) {
	local := netip.MustParseAddr("169.254.1.2")
	mux := &IpMux{localAddresses: []netip.Addr{local}}
	packet := newIpMuxIpv4Packet(net.ParseIP("1.1.1.1"), net.IP(local.AsSlice()))
	var localDestination bool
	allocations := testing.AllocsPerRun(1000, func() {
		localDestination = mux.isLocalPacketDestination(packet)
	})
	if !localDestination {
		t.Fatal("packet addressed to mux was not classified local")
	}
	if allocations != 0 {
		t.Fatalf("local packet classification allocated %.2f objects per packet, want 0", allocations)
	}
}

func testIpMuxRejectedPumpPoolBalance(t *testing.T, installRejectingUpstream bool) {
	t.Helper()
	poolOutstanding := func() int64 {
		taken, returned, _ := MessagePoolCounts()
		return int64(taken) - int64(returned)
	}
	before := poolOutstanding()

	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultTunSettings()
	settings.DialRace = 1
	settings.DialTimeout = 100 * time.Millisecond
	tun, err := CreateTun(ctx, settings)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	mux := NewIpMux(
		ctx,
		tun,
		TransferPath{},
		protocol.ProvideMode_Network,
		0,
		nil,
		nil,
		nil,
		NewNoopLogger(),
	)
	if installRejectingUpstream {
		mux.SetUpstream(func(
			source TransferPath,
			provideMode protocol.ProvideMode,
			packet []byte,
			timeout time.Duration,
		) bool {
			return false
		})
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 250*time.Millisecond)
	conn, _ := tun.DialContext(dialCtx, "tcp", "192.0.2.1:443")
	dialCancel()
	if conn != nil {
		conn.Close()
	}
	if !waitForCondition(time.Second, func() bool {
		return 0 < mux.rejectedPumpPacketCount.Load()
	}) {
		mux.Close()
		cancel()
		t.Fatal("internal stack emitted no packet into the rejected upstream")
	}
	mux.Close()
	cancel()

	if !waitForCondition(2*time.Second, func() bool {
		return poolOutstanding() <= before
	}) {
		after := poolOutstanding()
		t.Fatalf("rejected pump packet leaked a pooled buffer: outstanding %d -> %d", before, after)
	}
}

func TestIpMuxPumpReturnsPacketRejectedByUpstreamBackpressure(t *testing.T) {
	testIpMuxRejectedPumpPoolBalance(t, true)
}

func TestIpMuxPumpReturnsPacketBeforeUpstreamIsWired(t *testing.T) {
	testIpMuxRejectedPumpPoolBalance(t, false)
}
