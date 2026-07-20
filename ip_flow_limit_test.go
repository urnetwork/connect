package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestLocalUserNatSettingsMemoryScaled pins the memory-budget scaling of the
// nat's memory-dominant defaults: the per flow channel depths, the tcp
// window/read buffer, and the dmca flow cache (see `SetMemoryBudget`).
func TestLocalUserNatSettingsMemoryScaled(t *testing.T) {
	defer SetMemoryBudget(0)

	// the ios packet tunnel budget (scale 24/64)
	SetMemoryBudget(24 * 1024 * 1024)

	udpSettings := DefaultUdpBufferSettings()
	AssertEqual(t, udpSettings.SequenceBufferSize, 96)
	AssertEqual(t, udpSettings.IdleTimeout, 60*time.Second)
	AssertEqual(t, udpSettings.MaxWindowSize, uint32(393216))
	AssertEqual(t, udpSettings.GlobalLimit, 768)

	tcpSettings := DefaultTcpBufferSettings()
	AssertEqual(t, tcpSettings.SequenceBufferSize, 384)
	AssertEqual(t, tcpSettings.ReadBufferByteCount, 24576)
	AssertEqual(t, tcpSettings.MinWindowSize, uint32(65536))
	// 384 KiB scaled, snapped down to a power of 2 multiple of the min window
	AssertEqual(t, tcpSettings.MaxWindowSize, uint32(262144))
	AssertEqual(t, tcpSettings.GlobalLimit, 192)

	natSettings := DefaultLocalUserNatSettings()
	AssertEqual(t, natSettings.SequenceBufferSize, 384)

	dmcaSettings := DefaultDmcaSecurityPolicySettings()
	AssertEqual(t, dmcaSettings.MaxFlows, 24576)

	// floors at a tiny budget
	SetMemoryBudget(8 * 1024 * 1024)
	udpSettings = DefaultUdpBufferSettings()
	AssertEqual(t, udpSettings.SequenceBufferSize, 32)
	AssertEqual(t, udpSettings.MaxWindowSize, uint32(262144))
	AssertEqual(t, udpSettings.GlobalLimit, 256)
	tcpSettings = DefaultTcpBufferSettings()
	AssertEqual(t, tcpSettings.SequenceBufferSize, 192)
	AssertEqual(t, tcpSettings.ReadBufferByteCount, 16384)
	AssertEqual(t, tcpSettings.MaxWindowSize, uint32(131072))
	AssertEqual(t, tcpSettings.GlobalLimit, 64)
	AssertEqual(t, DefaultDmcaSecurityPolicySettings().MaxFlows, 8192)

	// no budget identifies a server/generic caller: it keeps the unscaled
	// buffers but does not silently inherit the constrained-device flow cap.
	// Actual providers select the explicit provider profile.
	SetMemoryBudget(0)
	udpSettings = DefaultUdpBufferSettings()
	AssertEqual(t, udpSettings.SequenceBufferSize, 256)
	AssertEqual(t, udpSettings.MaxWindowSize, uint32(1048576))
	AssertEqual(t, udpSettings.GlobalLimit, 0)
	tcpSettings = DefaultTcpBufferSettings()
	AssertEqual(t, tcpSettings.SequenceBufferSize, 1024)
	AssertEqual(t, tcpSettings.ReadBufferByteCount, 65536)
	AssertEqual(t, tcpSettings.MaxWindowSize, uint32(1048576))
	AssertEqual(t, tcpSettings.GlobalLimit, 0)
	AssertEqual(t, DefaultLocalUserNatSettings().SequenceBufferSize, 1024)
	AssertEqual(t, DefaultDmcaSecurityPolicySettings().MaxFlows, 65536)
	providerSettings := DefaultProviderLocalUserNatSettings()
	AssertEqual(t, providerSettings.UdpBufferSettings.UserLimit, 0)
	AssertEqual(t, providerSettings.UdpBufferSettings.GlobalLimit, 0)
	AssertEqual(t, providerSettings.TcpBufferSettings.UserLimit, 0)
	AssertEqual(t, providerSettings.TcpBufferSettings.GlobalLimit, 0)
	// an unbudgeted provider keeps long-lived plain-udp NAT bindings alive:
	// the provider-tuned idle, longer than the general 60s reap
	AssertEqual(t, providerSettings.UdpBufferSettings.IdleTimeout, providerUdpIdleTimeout)
	if providerSettings.UdpBufferSettings.IdleTimeout <= udpSettings.IdleTimeout {
		t.Errorf("provider udp idle %s must exceed the general udp idle %s",
			providerSettings.UdpBufferSettings.IdleTimeout, udpSettings.IdleTimeout)
	}

	// a budgeted provider keeps the scaled defaults, including the short idle
	SetMemoryBudget(24 * 1024 * 1024)
	budgetedProviderSettings := DefaultProviderLocalUserNatSettings()
	AssertEqual(t, budgetedProviderSettings.UdpBufferSettings.IdleTimeout, 60*time.Second)
	AssertEqual(t, budgetedProviderSettings.UdpBufferSettings.GlobalLimit, 768)
	SetMemoryBudget(0)

	// invariants at every budget tier:
	// - the tcp channel depth must cover the max window in mtu packets, so a
	//   full window burst is never dropped (the nat implements no retransmit
	//   toward the socket)
	// - the max window stays a power of 2 multiple of the min window (the
	//   window doubling ladder must land exactly on the max)
	for _, budget := range []ByteCount{0, mib(8), mib(16), mib(24), mib(32), mib(48), mib(64), mib(128)} {
		SetMemoryBudget(budget)
		tcpSettings := DefaultTcpBufferSettings()
		if int64(tcpSettings.SequenceBufferSize)*int64(tcpSettings.Mtu) < int64(tcpSettings.MaxWindowSize) {
			t.Errorf("budget %d: tcp depth %d x mtu %d does not cover the max window %d",
				budget, tcpSettings.SequenceBufferSize, tcpSettings.Mtu, tcpSettings.MaxWindowSize)
		}
		if tcpSettings.MaxWindowSize < tcpSettings.MinWindowSize {
			t.Errorf("budget %d: max window %d below min window %d",
				budget, tcpSettings.MaxWindowSize, tcpSettings.MinWindowSize)
		}
		for w := tcpSettings.MinWindowSize; ; w *= 2 {
			if w == tcpSettings.MaxWindowSize {
				break
			}
			if w > tcpSettings.MaxWindowSize {
				t.Errorf("budget %d: max window %d is not a power of 2 multiple of the min window %d",
					budget, tcpSettings.MaxWindowSize, tcpSettings.MinWindowSize)
				break
			}
		}
	}
}

// pollUntil polls `condition` to true within `timeout`, else fails the test.
func pollUntil(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", description)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startUdpSink starts a loopback udp socket that discards received datagrams,
// so nat udp flows have a stable destination.
func startUdpSink(t *testing.T) (port uint16, closeFn func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp sink listen: %v", err)
	}
	go HandleError(func() {
		buffer := make([]byte, 2048)
		for {
			if _, _, err := conn.ReadFromUDP(buffer); err != nil {
				return
			}
		}
	})
	return uint16(conn.LocalAddr().(*net.UDPAddr).Port), func() {
		conn.Close()
	}
}

// TestUdpBufferFlowLimits exercises the per source (`UserLimit`) and
// aggregate (`GlobalLimit`) udp flow caps: over-limit creates evict the
// approximately idle-most sampled flow, the newest flows survive, and eager
// removal keeps the flow map at the exact cap.
func TestUdpBufferFlowLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sinkPort, closeSink := startUdpSink(t)
	defer closeSink()

	udpBufferSettings := DefaultUdpBufferSettingsWithBufferSize(8)
	udpBufferSettings.UserLimit = 2
	udpBufferSettings.GlobalLimit = 3

	buffer := NewUdp4Buffer(ctx, func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {}, udpBufferSettings)

	send := func(source TransferPath, sourcePort uint16) {
		t.Helper()
		packet := MessagePoolGet(32)
		parsed := &parsedUdp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      sourcePort,
			destinationPort: sinkPort,
			payload:         packet[:4],
		}
		if success, err := buffer.send(source, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
			MessagePoolReturn(packet)
			t.Fatalf("udp send %d: success=%t err=%v", sourcePort, success, err)
		}
		// keep the lru order deterministic
		time.Sleep(10 * time.Millisecond)
	}

	sourcePorts := func() map[uint16]bool {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		ports := map[uint16]bool{}
		for _, sequence := range buffer.sequences {
			ports[sequence.sourcePort] = true
		}
		return ports
	}

	sourceA := SourceId(NewId())
	// two flows fill the per source limit
	send(sourceA, 40001)
	send(sourceA, 40002)
	// the third and fourth evict the idle-most flow before their insert
	send(sourceA, 40003)
	send(sourceA, 40004)
	pollUntil(t, 5*time.Second, "per source lru eviction", func() bool {
		ports := sourcePorts()
		return !ports[40001] && ports[40003] && ports[40004]
	})

	// a second source pushes the aggregate over the global limit: the
	// idle-most flows across all sources evict, the newest flows survive
	sourceB := SourceId(NewId())
	send(sourceB, 41001)
	send(sourceB, 41002)
	pollUntil(t, 5*time.Second, "global lru eviction", func() bool {
		ports := sourcePorts()
		return len(ports) <= udpBufferSettings.GlobalLimit && ports[41001] && ports[41002]
	})
}

// TestTcpBufferFlowLimits exercises the same caps for tcp flows: syns over
// the per source and global limits evict the idle-most established flow.
func TestTcpBufferFlowLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// accept and hold connections so the nat flows stay established
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go HandleError(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	})
	listenerPort := uint16(listener.Addr().(*net.TCPAddr).Port)

	tcpBufferSettings := DefaultTcpBufferSettingsWithBufferSize(8)
	tcpBufferSettings.UserLimit = 2
	tcpBufferSettings.GlobalLimit = 3

	buffer := NewTcp4Buffer(ctx, func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {}, tcpBufferSettings)

	sendSyn := func(source TransferPath, sourcePort uint16) {
		t.Helper()
		packet := MessagePoolGet(32)
		parsed := &parsedTcp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      sourcePort,
			destinationPort: listenerPort,
			syn:             true,
			seq:             1000,
			windowSize:      65535,
		}
		if success, err := buffer.send(source, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
			MessagePoolReturn(packet)
			t.Fatalf("tcp syn %d: success=%t err=%v", sourcePort, success, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	sourcePorts := func() map[uint16]bool {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		ports := map[uint16]bool{}
		for _, sequence := range buffer.sequences {
			ports[sequence.sourcePort] = true
		}
		return ports
	}

	sourceA := SourceId(NewId())
	sendSyn(sourceA, 40001)
	sendSyn(sourceA, 40002)
	sendSyn(sourceA, 40003)
	sendSyn(sourceA, 40004)
	pollUntil(t, 5*time.Second, "per source lru eviction", func() bool {
		ports := sourcePorts()
		return !ports[40001] && ports[40003] && ports[40004]
	})

	sourceB := SourceId(NewId())
	sendSyn(sourceB, 41001)
	sendSyn(sourceB, 41002)
	pollUntil(t, 5*time.Second, "global lru eviction", func() bool {
		ports := sourcePorts()
		return len(ports) <= tcpBufferSettings.GlobalLimit && ports[41001] && ports[41002]
	})
}

// TestUdpBufferGlobalLimitConcurrent drives GlobalLimit under concurrent
// new-flow creation at and over the cap (run with -race): the cap holds, the
// eviction loop terminates, the `sequences`/`sourceSequences` indexes stay
// exactly consistent, and dispatch is not stalled (a fresh flow still lands
// after the burst). Verifies the applyLruMapLimit rework under concurrency.
func TestUdpBufferGlobalLimitConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sinkPort, closeSink := startUdpSink(t)
	defer closeSink()

	udpBufferSettings := DefaultUdpBufferSettingsWithBufferSize(8)
	udpBufferSettings.GlobalLimit = 6

	buffer := NewUdp4Buffer(ctx, func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {}, udpBufferSettings)

	send := func(source TransferPath, sourcePort uint16) {
		packet := MessagePoolGet(32)
		parsed := &parsedUdp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      sourcePort,
			destinationPort: sinkPort,
			payload:         packet[:4],
		}
		// under concurrent eviction a create can lose the race and fail —
		// that is allowed; the invariants below are what must hold
		if success, err := buffer.send(source, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
			MessagePoolReturn(packet)
		}
	}

	// 8 sources x 8 flows: far over the cap, all created concurrently
	var wg sync.WaitGroup
	for s := 0; s < 8; s++ {
		wg.Add(1)
		source := SourceId(NewId())
		basePort := uint16(42000 + 100*s)
		go func() {
			defer wg.Done()
			for f := uint16(0); f < 8; f++ {
				send(source, basePort+f)
			}
		}()
	}
	wg.Wait()

	assertConsistent := func() int {
		t.Helper()
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		if udpBufferSettings.GlobalLimit < len(buffer.sequences) {
			t.Fatalf("global cap broken: %d sequences > limit %d", len(buffer.sequences), udpBufferSettings.GlobalLimit)
		}
		total := 0
		for source, sourceSequences := range buffer.sourceSequences {
			if len(sourceSequences) == 0 {
				t.Fatalf("index drift: empty source map retained for %s", source)
			}
			for bufferId, sequence := range sourceSequences {
				if buffer.sequences[bufferId] != sequence {
					t.Fatal("index drift: source-indexed sequence not in the flow table")
				}
				if sequence.source != source {
					t.Fatal("index drift: sequence filed under the wrong source")
				}
				total += 1
			}
		}
		if total != len(buffer.sequences) {
			t.Fatalf("index drift: %d source-indexed vs %d sequences", total, len(buffer.sequences))
		}
		return len(buffer.sequences)
	}
	if count := assertConsistent(); count == 0 {
		t.Fatal("no flows survived the burst")
	}

	// dispatch is not stalled: a fresh flow still lands at the cap
	lateSource := SourceId(NewId())
	packet := MessagePoolGet(32)
	parsed := &parsedUdp{
		sourceIp:        net.IPv4(10, 0, 0, 2).To4(),
		destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
		sourcePort:      45001,
		destinationPort: sinkPort,
		payload:         packet[:4],
	}
	if success, err := buffer.send(lateSource, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
		MessagePoolReturn(packet)
		t.Fatalf("post-burst flow create stalled: success=%t err=%v", success, err)
	}
	assertConsistent()
}

// TestTcpBufferGlobalLimitConcurrent is the tcp shape of
// TestUdpBufferGlobalLimitConcurrent: concurrent SYNs at/over GlobalLimit
// (run with -race).
func TestTcpBufferGlobalLimitConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go HandleError(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	})
	listenerPort := uint16(listener.Addr().(*net.TCPAddr).Port)

	tcpBufferSettings := DefaultTcpBufferSettingsWithBufferSize(8)
	tcpBufferSettings.GlobalLimit = 6

	buffer := NewTcp4Buffer(ctx, func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {}, tcpBufferSettings)

	sendSyn := func(source TransferPath, sourcePort uint16) {
		packet := MessagePoolGet(32)
		parsed := &parsedTcp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      sourcePort,
			destinationPort: listenerPort,
			syn:             true,
			seq:             1000,
			windowSize:      65535,
		}
		if success, err := buffer.send(source, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
			MessagePoolReturn(packet)
		}
	}

	var wg sync.WaitGroup
	for s := 0; s < 8; s++ {
		wg.Add(1)
		source := SourceId(NewId())
		basePort := uint16(43000 + 100*s)
		go func() {
			defer wg.Done()
			for f := uint16(0); f < 8; f++ {
				sendSyn(source, basePort+f)
			}
		}()
	}
	wg.Wait()

	assertConsistent := func() int {
		t.Helper()
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		if tcpBufferSettings.GlobalLimit < len(buffer.sequences) {
			t.Fatalf("global cap broken: %d sequences > limit %d", len(buffer.sequences), tcpBufferSettings.GlobalLimit)
		}
		total := 0
		for source, sourceSequences := range buffer.sourceSequences {
			if len(sourceSequences) == 0 {
				t.Fatalf("index drift: empty source map retained for %s", source)
			}
			for bufferId, sequence := range sourceSequences {
				if buffer.sequences[bufferId] != sequence {
					t.Fatal("index drift: source-indexed sequence not in the flow table")
				}
				if sequence.source != source {
					t.Fatal("index drift: sequence filed under the wrong source")
				}
				total += 1
			}
		}
		if total != len(buffer.sequences) {
			t.Fatalf("index drift: %d source-indexed vs %d sequences", total, len(buffer.sequences))
		}
		return len(buffer.sequences)
	}
	if count := assertConsistent(); count == 0 {
		t.Fatal("no flows survived the burst")
	}

	// dispatch is not stalled: a fresh SYN still lands at the cap
	lateSource := SourceId(NewId())
	packet := MessagePoolGet(32)
	parsed := &parsedTcp{
		sourceIp:        net.IPv4(10, 0, 0, 2).To4(),
		destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
		sourcePort:      45002,
		destinationPort: listenerPort,
		syn:             true,
		seq:             1000,
		windowSize:      65535,
	}
	if success, err := buffer.send(lateSource, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
		MessagePoolReturn(packet)
		t.Fatalf("post-burst SYN create stalled: success=%t err=%v", success, err)
	}
	assertConsistent()
}

// TestUdpBufferIdleReap pins the udp idle reap: an idle flow releases its
// sequence (channels, read buffer, goroutines, socket) after `IdleTimeout`,
// without any close signal from the source.
func TestUdpBufferIdleReap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sinkPort, closeSink := startUdpSink(t)
	defer closeSink()

	udpBufferSettings := DefaultUdpBufferSettingsWithBufferSize(8)
	udpBufferSettings.IdleTimeout = 250 * time.Millisecond

	buffer := NewUdp4Buffer(ctx, func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {}, udpBufferSettings)

	source := SourceId(NewId())
	for s := 0; s < 3; s++ {
		packet := MessagePoolGet(32)
		parsed := &parsedUdp{
			sourceIp:        net.IPv4(10, 0, 0, 1).To4(),
			destinationIp:   net.IPv4(127, 0, 0, 1).To4(),
			sourcePort:      uint16(42001 + s),
			destinationPort: sinkPort,
			payload:         packet[:4],
		}
		if success, err := buffer.send(source, protocol.ProvideMode_Network, parsed, -1, packet); err != nil || !success {
			MessagePoolReturn(packet)
			t.Fatalf("udp send %d: success=%t err=%v", s, success, err)
		}
	}

	sequenceCount := func() int {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		return len(buffer.sequences)
	}
	AssertEqual(t, sequenceCount(), 3)

	pollUntil(t, 10*time.Second, "idle flows reaped", func() bool {
		return sequenceCount() == 0
	})
}

// TestMemoryScaledCaps pins the memory-budget scaling of the remaining
// bounded-by-default caps: the webrtc peer connection count and the
// provider's return provide mode source map.
func TestMemoryScaledCaps(t *testing.T) {
	defer SetMemoryBudget(0)

	SetMemoryBudget(24 * 1024 * 1024)
	AssertEqual(t, DefaultWebRtcSettings().MaxPeerConnectionCount, 12)
	AssertEqual(t, DefaultRemoteUserNatProviderSettings().MaxSourceCount, 3072)

	SetMemoryBudget(8 * 1024 * 1024)
	AssertEqual(t, DefaultWebRtcSettings().MaxPeerConnectionCount, 8)
	AssertEqual(t, DefaultRemoteUserNatProviderSettings().MaxSourceCount, 1024)

	SetMemoryBudget(0)
	AssertEqual(t, DefaultWebRtcSettings().MaxPeerConnectionCount, 32)
	AssertEqual(t, DefaultRemoteUserNatProviderSettings().MaxSourceCount, 8192)
}

type testing_noopSignalSender struct {
}

func (self *testing_noopSignalSender) SendSignal(path TransferPath, signal *protocol.Frame, opts ...any) {
}

// TestWebRtcManagerPeerConnCap exercises the peer connection cap: creates at
// the cap are refused (the stream stays on the platform transport), while a
// create for an existing key replaces that connection and is allowed.
func TestWebRtcManagerPeerConnCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	// no stun: the connections never need to gather beyond host candidates
	settings.IceServerUrls = nil
	settings.MaxPeerConnectionCount = 2

	manager := NewWebRtcManager(ctx, &testing_noopSignalSender{}, settings)

	sourceId := NewId()
	newPath := func() TransferPath {
		return TransferPath{
			SourceId:      sourceId,
			DestinationId: NewId(),
			StreamId:      NewId(),
		}
	}

	pathA := newPath()
	if _, err := manager.NewP2pConnActive(ctx, pathA); err != nil {
		t.Fatalf("conn a: %v", err)
	}
	pathB := newPath()
	if _, err := manager.NewP2pConnActive(ctx, pathB); err != nil {
		t.Fatalf("conn b: %v", err)
	}

	// at the cap a new key is refused
	if _, err := manager.NewP2pConnActive(ctx, newPath()); err == nil {
		t.Fatal("expected the peer connection cap to refuse a new key")
	}

	// a create for an existing key replaces in place and is allowed
	if _, err := manager.NewP2pConnActive(ctx, pathA); err != nil {
		t.Fatalf("replacement for an existing key must be allowed: %v", err)
	}

	peerConnCount := func() int {
		manager.stateLock.Lock()
		defer manager.stateLock.Unlock()
		return len(manager.peerConns)
	}
	AssertEqual(t, peerConnCount(), 2)
}

// TestRemoteUserNatProviderSourceCap bounds the provider's per-source return
// provide mode map: at the cap an arbitrary entry evicts to admit the new
// source (the return path falls back to the packet's carried provide mode).
func TestRemoteUserNatProviderSourceCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer providerClient.Cancel()

	localUserNat := NewLocalUserNatWithDefaults(ctx, "test-source-cap")
	defer localUserNat.Close()

	settings := DefaultRemoteUserNatProviderSettings()
	settings.MaxSourceCount = 4
	provider := NewRemoteUserNatProvider(providerClient, localUserNat, settings)
	defer provider.Close()

	sourceCount := func() int {
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		return len(provider.sourceProvideMode)
	}

	var lastSourceId Id
	for i := 0; i < 10; i++ {
		lastSourceId = NewId()
		provider.recordSourceProvideMode(lastSourceId, protocol.ProvideMode_Public)
	}
	AssertEqual(t, sourceCount(), 4)
	// the newest source is always admitted
	AssertEqual(t, provider.sourceReturnProvideMode(lastSourceId, protocol.ProvideMode_Network), protocol.ProvideMode_Public)

	// updating an existing entry does not evict
	provider.recordSourceProvideMode(lastSourceId, protocol.ProvideMode_Network)
	AssertEqual(t, sourceCount(), 4)
	AssertEqual(t, provider.sourceReturnProvideMode(lastSourceId, protocol.ProvideMode_Public), protocol.ProvideMode_Network)
}

// TestIpEgressTcp4MemoryBudget runs real tcp echo flows through the nat with
// the ios packet tunnel budget applied, so the scaled per flow depths, read
// buffer, and snapped window carry real traffic end to end.
func TestIpEgressTcp4MemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping egress memory budget test in short mode")
	}

	defer SetMemoryBudget(0)
	SetMemoryBudget(24 * 1024 * 1024)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	echoListener, err := net.Listen("tcp4", "127.0.0.1:0")
	AssertEqual(t, err, nil)
	defer echoListener.Close()
	go HandleError(func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go HandleError(func() {
				defer conn.Close()
				io.Copy(conn, conn)
			})
		}
	})

	tun, err := CreateTunWithDefaults(ctx)
	AssertEqual(t, err, nil)
	defer tun.Close()

	// the scaled defaults under the budget
	localUserNat := NewLocalUserNat(ctx, "testEgressBudget", DefaultLocalUserNatSettings())
	defer localUserNat.Close()

	removeReceiveCallback := bridgeTunToLocalUserNat(tun, localUserNat, SourceId(NewId()))
	defer removeReceiveCallback()

	payloadSizes := []int{1, 1381, 16384, 1 << 20}

	parallelCount := 2
	flowErrs := make(chan error, parallelCount)
	for p := 0; p < parallelCount; p += 1 {
		go HandleError(func() {
			flowErrs <- func() error {
				conn, err := tun.DialContext(ctx, "tcp", echoListener.Addr().String())
				if err != nil {
					return fmt.Errorf("dial: %w", err)
				}
				defer conn.Close()

				for _, payloadSize := range payloadSizes {
					payload := testingEgressPayload(p, payloadSize)

					readErr := make(chan error, 1)
					go HandleError(func() {
						readErr <- func() error {
							echoPayload := make([]byte, payloadSize)
							conn.SetReadDeadline(time.Now().Add(60 * time.Second))
							if _, err := io.ReadFull(conn, echoPayload); err != nil {
								return fmt.Errorf("read size=%d: %w", payloadSize, err)
							}
							if !bytes.Equal(payload, echoPayload) {
								return fmt.Errorf("echo mismatch size=%d", payloadSize)
							}
							return nil
						}()
					})

					conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
					if _, err := conn.Write(payload); err != nil {
						return fmt.Errorf("write size=%d: %w", payloadSize, err)
					}
					if err := <-readErr; err != nil {
						return err
					}
				}
				return nil
			}()
		})
	}
	for p := 0; p < parallelCount; p += 1 {
		select {
		case err := <-flowErrs:
			AssertEqual(t, err, nil)
		case <-ctx.Done():
			t.Fatal("timeout")
		}
	}
}
