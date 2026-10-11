package connect

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func TestNatMemoryRefusesConstructionBeforeAllocating(t *testing.T) {
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget = NewTransferMemoryBudget(natMemoryFixedBytes - 1)
	if allocations := testing.AllocsPerRun(100, func() {
		nat, err := TryNewLocalUserNat(context.Background(), "refused", settings)
		if nat != nil || !errors.Is(err, ErrNatMemoryBudget) {
			panic("construction was not refused")
		}
	}); allocations != 0 {
		t.Fatalf("refused constructor allocated %g objects", allocations)
	}
	if settings.MemoryBudget.UsedByteCount() != 0 {
		t.Fatal("refusal retained a claim")
	}
}

func TestNatMemoryRefusesPacketBeforeAllocating(t *testing.T) {
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget = NewTransferMemoryBudget(natMemoryFixedBytes)
	nat, err := TryNewLocalUserNat(t.Context(), "full", settings)
	if err != nil {
		t.Fatal(err)
	}
	defer nat.CloseAndWait(context.Background())
	packet := []byte{0}
	packets := [][]byte{packet}
	for _, batch := range []bool{false, true} {
		if allocations := testing.AllocsPerRun(100, func() {
			var admitted bool
			if batch {
				admitted = nat.SendPacketsWithTimeout(TransferPath{}, protocol.ProvideMode_Network, packets, 0)
			} else {
				admitted = nat.SendPacketWithTimeout(TransferPath{}, protocol.ProvideMode_Network, packet, 0)
			}
			if admitted {
				panic("full NAT admitted data")
			}
		}); allocations != 0 {
			t.Fatalf("refused packet batch=%t allocated %g objects", batch, allocations)
		}
	}
}

func TestNatMemoryLegacyNilSettingsAndCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	nat := NewLocalUserNat(ctx, "canceled", nil)
	if nat == nil {
		t.Fatal("legacy unbudgeted constructor returned nil")
	}
	if err := nat.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if nat, err := TryNewLocalUserNat(ctx, "canceled", nil); nat != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission = %v, %v", nat, err)
	}
}

func TestNatMemoryTwoNatsAndRetiringGenerationShareCeiling(t *testing.T) {
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(mib(13))
	budget := NewTransferMemoryBudgetWithParent(mib(2), root)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var dials atomic.Int64
	newHeld := func() (*LocalUserNat, <-chan struct{}, func()) {
		settings := DefaultLocalUserNatSettings()
		settings.Log = NewNoopLogger()
		settings.MemoryBudget = budget
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		settings.TcpBufferSettings.beforeSequenceRunForTest = func() { close(entered); <-release }
		settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dials.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		nat, err := TryNewLocalUserNat(ctx, "held", settings)
		if err != nil {
			t.Fatal(err)
		}
		done := func() { once.Do(func() { close(release) }) }
		t.Cleanup(func() { done(); nat.CloseAndWait(context.Background()) })
		return nat, entered, done
	}
	first, firstEntered, releaseFirst := newHeld()
	second, secondEntered, releaseSecond := newHeld()
	if budget.UsedByteCount() != 2*natMemoryFixedBytes || root.UsedByteCount() != budget.UsedByteCount() {
		t.Fatal("two NAT graphs escaped the shared root")
	}
	sendSyn := func(nat *LocalUserNat, port int) {
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(10, 0, 0, 1), SourcePort: port,
			DestinationIp: net.IPv4(203, 0, 113, 1), DestinationPort: 443}
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
		if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
			MessagePoolReturn(packet)
			t.Fatal("initial SYN rejected")
		}
	}
	sendSyn(first, 40000)
	sendSyn(second, 40001)
	for _, entered := range []<-chan struct{}{firstEntered, secondEntered} {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Occupy remaining data headroom, then retire a NAT with a held flow.
	fill := budget.Available()
	if !budget.TryReserve(fill) {
		t.Fatal("fill refused")
	}
	first.Close()
	select {
	case <-first.runDone:
		t.Fatal("retirement released a still-running flow")
	default:
	}
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget = budget
	if nat, err := TryNewLocalUserNat(ctx, "replacement", settings); nat != nil || !errors.Is(err, ErrNatMemoryBudget) {
		t.Fatal("replacement escaped retiring ownership")
	}
	if dials.Load() != 0 {
		t.Fatal("held/refused flows opened a socket")
	}
	budget.Release(fill)
	releaseFirst()
	if err := first.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	third, err := TryNewLocalUserNat(ctx, "replacement", settings)
	if err != nil {
		t.Fatal(err)
	}
	if budget.UsedByteCount() > mib(2) || root.UsedByteCount() != budget.UsedByteCount() {
		t.Fatal("replacement exceeded shared root")
	}
	third.CloseAndWait(ctx)
	second.Close()
	releaseSecond()
	if err := second.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	stats := budget.Stats()
	if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || root.UsedByteCount() != 0 {
		t.Fatalf("teardown imbalance: %+v root=%+v", stats, root.Stats())
	}
}

func TestNatMemoryNearCapacitySynDoesNotCreateFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := DefaultTcpBufferSettingsWithBufferSize(4)
	settings.Log = NewNoopLogger()
	settings.MemoryBudget = NewTransferMemoryBudget(natTcpFlowMemoryByteCount(settings))
	var dials atomic.Int64
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected socket")
	}}
	buffer := NewTcp4Buffer(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, settings)
	packet := MessagePoolGet(40)
	defer MessagePoolReturn(packet)
	source := SourceId(NewId())
	for i := range 100 {
		tcp := parsedTcp{sourceIp: net.IPv4(10, 0, 0, 1).To4(), destinationIp: net.IPv4(203, 0, 113, 1).To4(),
			sourcePort: uint16(40000 + i), destinationPort: 443, syn: true}
		if accepted, err := buffer.send(source, protocol.ProvideMode_Network, &tcp, 0, packet); accepted || err != nil {
			t.Fatalf("near-full SYN admitted: %t %v", accepted, err)
		}
	}
	if len(buffer.sequences) != 0 || len(buffer.sourceSequences) != 0 || dials.Load() != 0 || settings.MemoryBudget.UsedByteCount() != 0 {
		t.Fatal("refused SYN installed idle flow state")
	}
}

func TestNatMemoryControlPartitionIsPrechargedAndBounded(t *testing.T) {
	budget := NewTransferMemoryBudget(natMemoryFixedBytes)
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget = budget
	nat, err := TryNewLocalUserNat(t.Context(), "control", settings)
	if err != nil {
		t.Fatal(err)
	}
	defer nat.CloseAndWait(context.Background())
	if budget.Available() != 0 {
		t.Fatal("fixture did not fill the parent")
	}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(10, 0, 0, 1), SourcePort: 1234,
		DestinationIp: net.IPv4(203, 0, 113, 1), DestinationPort: 443}
	packet := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagAck, nil))
	defer MessagePoolReturn(packet)
	if !natControlPackets([][]byte{packet}) {
		t.Fatal("pure ACK not recognized")
	}
	charge := natPacketMemoryByteCount(packet) + 320
	count := 0
	for nat.controlMemory.TryReserve(charge) {
		count++
	}
	if count == 0 || nat.controlMemory.UsedByteCount() > kib(16) || budget.UsedByteCount() != natMemoryFixedBytes {
		t.Fatal("control partition added hidden root capacity")
	}
	nat.controlMemory.Release(ByteCount(count) * charge)
	packetWithHugeRoot := make([]byte, len(packet), 4096)
	copy(packetWithHugeRoot, packet)
	if natControlPackets([][]byte{packetWithHugeRoot}) {
		t.Fatal("large backing root escaped through control classification")
	}
	if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, MessagePoolShareReadOnly(packet), 0) {
		MessagePoolReturn(packet)
		t.Fatal("full data budget blocked pure ACK admission")
	}
}

func TestNatMemoryReplayMetadataAndSiblingWake(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		root := NewTransferMemoryBudget(kib(4))
		budget := NewTransferMemoryBudgetWithParent(kib(4), root)
		sequence := &TcpSequence{ctx: ctx, cancel: cancel, tcpBufferSettings: &TcpBufferSettings{MemoryBudget: budget},
			returnWake: make(chan struct{}, 1), returnCapacity: make(chan struct{}, 1), ConnectionState: ConnectionState{receiveSeq: 100}}
		for i := range 5 {
			if !sequence.retainReturnChunk([]byte{1}, uint32(i), false) {
				t.Fatal("return chunk refused")
			}
		}
		sequence.mutex.Lock()
		sequence.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: 5, windowSize: 100})
		sequence.mutex.Unlock()
		if budget.UsedByteCount() != sequence.returnMetadataByteCount || budget.UsedByteCount() == 0 {
			t.Fatal("ACK freed retained replay-slice backing")
		}
		sequence.releaseReturnChunks()
		if budget.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("metadata did not retire")
		}
	})
}

func TestNatMemoryPacketizationBoundsTinyMss(t *testing.T) {
	state := ConnectionState{ipVersion: 4, peerMss: 1}
	if got := state.natPacketizationByteLimit(DefaultMtu); got != natMemoryBatchCount {
		t.Fatalf("MSS=1 packetization grew beyond charged producer roots: %d", got)
	}
}

func TestNatMemoryUdpFragmentWorkingSetFitsFlowClaim(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mtu := range []int{56, ipv4MinimumPathMtu, DefaultMtu, DefaultTunnelMtu} {
		settings := DefaultUdpBufferSettings()
		settings.ReadBufferByteCount = natMemoryReadBytes
		settings.Mtu = mtu
		claim := natUdpFlowMemoryByteCount(settings)
		for _, ipVersion := range []int{4, 6} {
			path := udpTestPath(ipVersion)
			state := StreamState{ipVersion: ipVersion, sourceIp: path.SourceIp, destinationIp: path.DestinationIp}
			state.applyPathMtu(ipMinimumPathMtu(ipVersion))
			payload := make([]byte, settings.ReadBufferByteCount)
			packets, err := state.DataPackets(payload, len(payload), mtu)
			if err != nil {
				t.Fatal(err)
			}
			header := Ipv4HeaderSizeWithoutExtensions
			if ipVersion == 6 {
				header = Ipv6HeaderSize
			}
			working := ByteCount(len(payload)+cap(packets)*24) +
				retainedMessageCapacity(ByteCount(len(payload)+header+UdpHeaderSize))
			for _, packet := range packets {
				working += ByteCount(cap(packet))
				MessagePoolReturn(packet)
			}
			if claim < kib(16)+ByteCount(settings.SequenceBufferSize)*16+working {
				t.Fatalf("IPv%d mtu=%d working=%d exceeds claim=%d", ipVersion, mtu, working, claim)
			}
		}
	}
}

func TestNatMemoryUdpCallbackRetainsFlowThroughClose(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "portable", true: "shared"}[shared], func(t *testing.T) {
			assertMessagePoolOwnership(t)
			port, stopEcho := startUdpLoopbackEcho(t, 4)
			defer stopEcho()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			budget := NewTransferMemoryBudget(mib(2))
			settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(mib(4))
			settings.MemoryBudget = budget
			if !shared {
				settings.UdpBufferSettings.SocketReadShardCount = 0
				settings.UdpBufferSettings.SharedSocketLifecycle = false
			}
			nat, err := TryNewLocalUserNat(ctx, "udp-retirement", settings)
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer func() { unblock(); nat.CloseAndWait(context.Background()) }()
			nat.AddReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
				close(entered)
				<-release
			})
			path := udpTestPath(4)
			path.DestinationIp, path.DestinationPort = net.IPv4(127, 0, 0, 1), int(port)
			packet := MessagePoolCopy(ipOosUdpPacket(path, []byte("echo")))
			if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
				MessagePoolReturn(packet)
				t.Fatal("UDP rejected")
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			nat.Close()
			if budget.UsedByteCount() <= natMemoryFixedBytes {
				t.Fatal("close returned live UDP callback/flow memory")
			}
			unblock()
			if err := nat.CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
			if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatalf("UDP teardown leaked claims: %+v", stats)
			}
		})
	}
}

func TestNatMemoryTcpAckProgressAtFullDataBudget(t *testing.T) {
	testNatMemoryTcpAckProgressAtFullDataBudget(t, 0)
}

func TestNatProviderMemoryTcpAckProgressAtFullDataBudget(t *testing.T) {
	t.Run("legacy", func(t *testing.T) { testNatMemoryTcpAckProgressAtFullDataBudget(t, 1) })
	t.Run("raw", func(t *testing.T) { testNatMemoryTcpAckProgressAtFullDataBudget(t, 2) })
}

func testNatMemoryTcpAckProgressAtFullDataBudget(t *testing.T, providerProtocolVersion int) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	budget := NewTransferMemoryBudget(mib(2))
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget = budget
	settings.Log = NewNoopLogger()
	local, origin := net.Pipe()
	defer origin.Close()
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
	nat, err := TryNewLocalUserNat(ctx, "tcp-full", settings)
	if err != nil {
		t.Fatal(err)
	}
	var provider *RemoteUserNatProvider
	if providerProtocolVersion != 0 {
		provider = newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(settings *RemoteUserNatProviderSettings) {
			settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		})
		// The callback below is the receiving TCP peer. Exercise real provider
		// ingress and NAT replay ownership without an unrelated Transfer rig.
		provider.localUserNatUnsub()
	}
	source := SourceId(NewId())
	sendPacket := func(packet []byte) bool {
		if provider == nil {
			return nat.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
		}
		frame, err := ipPacketToProviderFrame(packet, providerProtocolVersion)
		if err != nil {
			t.Error(err)
			return false
		}
		before := provider.packetStatsCounters.remoteIngressPacketCount.Load()
		provider.ClientReceive(source, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Network})
		if !frame.Raw {
			MessagePoolReturn(frame.MessageBytes)
		}
		accepted := before < provider.packetStatsCounters.remoteIngressPacketCount.Load()
		if accepted {
			MessagePoolReturn(packet) // ClientReceive borrowed the caller's root.
		}
		return accepted
	}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
		DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
	handshake, firstData, allowAck, complete := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var allowAckOnce sync.Once
	unblockAck := func() { allowAckOnce.Do(func() { close(allowAck) }) }
	defer func() {
		// A setup failure must release the callback barrier before joining the
		// NAT, rather than waiting for the test deadline and reporting teardown
		// cancellation as additional ACK-admission failures.
		cancel()
		unblockAck()
		nat.CloseAndWait(context.Background())
	}()
	var handshakeOnce sync.Once
	var dataStarted atomic.Bool
	var received atomic.Int64
	const total = 32 * 1024
	nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		if ctx.Err() != nil {
			return
		}
		_, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) || (!tcp.syn && len(tcp.payload) == 0) {
			return
		}
		if len(tcp.payload) != 0 && dataStarted.CompareAndSwap(false, true) {
			close(firstData)
			select {
			case <-allowAck:
			case <-ctx.Done():
				return
			}
		}
		ackNumber := tcp.seq + uint32(len(tcp.payload))
		if tcp.syn {
			ackNumber++
		}
		state := ConnectionState{ipVersion: 4, sourceIp: path.DestinationIp, sourcePort: uint16(path.DestinationPort),
			destinationIp: path.SourceIp, destinationPort: uint16(path.SourcePort), sendSeq: ackNumber, windowSize: 65535}
		ack := state.tcpPacket(tcpFlagAck, 101, nil)
		if !sendPacket(ack) {
			MessagePoolReturn(ack)
			if ctx.Err() == nil {
				t.Error("full data budget refused a releasing TCP ACK")
			}
		}
		if tcp.syn {
			handshakeOnce.Do(func() { close(handshake) })
		}
		if received.Add(int64(len(tcp.payload))) >= total {
			select {
			case complete <- struct{}{}:
			default:
			}
		}
	})
	syn := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
	if !sendPacket(syn) {
		MessagePoolReturn(syn)
		t.Fatal("SYN rejected")
	}
	select {
	case <-handshake:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	written := make(chan error, 1)
	go func() { _, err := origin.Write(bytes.Repeat([]byte{1}, total)); written <- err }()
	select {
	case <-firstData:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The callback barrier does not stop socket read-ahead from retaining its
	// next replay chunk. Fill atomically with respect to those legitimate
	// claims; Available followed by TryReserve is only an advisory snapshot
	// and can correctly fail under contention before this test starts.
	admissionRoot := budget.admissionRoot()
	admissionRoot.admissionLock.Lock()
	fill := budget.Available()
	filled := budget.tryReserveWithLock(fill)
	full := budget.UsedByteCount() == budget.TotalByteCount()
	admissionRoot.admissionLock.Unlock()
	if !filled {
		t.Fatal("could not fill data budget")
	}
	defer budget.Release(fill)
	if !full {
		t.Fatal("data budget was not full at the pressure boundary")
	}
	unblockAck()
	select {
	case <-complete:
	case <-ctx.Done():
		t.Fatalf("ACKs failed to release replay pressure: %v, bytes=%d", ctx.Err(), received.Load())
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if budget.UsedByteCount() > budget.TotalByteCount() {
		t.Fatal("TCP progress overdrew its budget")
	}
}
