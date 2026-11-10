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

	"github.com/urnetwork/connect/protocol"
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
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := DefaultTcpBufferSettingsWithBufferSize(4)
	settings.Log = NewNoopLogger()
	settings.MemoryBudget = NewTransferMemoryBudget(mib(2))
	settings = boundedNatTcpMemorySettings(settings, settings.MemoryBudget)
	settings.MemoryBudget = NewTransferMemoryBudget(natTcpFlowMemoryByteCount(settings) - 1)
	var dials atomic.Int64
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected socket")
	}}
	buffer := NewTcp4Buffer(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, settings)
	defer func() { cancel(); buffer.waitForLifecycle() }()
	source := SourceId(NewId())
	for i := range 100 {
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(10, 0, 0, 1), DestinationIp: net.IPv4(203, 0, 113, 1),
			SourcePort: 40000 + i, DestinationPort: 443}
		packet := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagSyn, nil))
		_, sourceIp, destinationIp, transport, valid := parseIpv4(packet)
		var tcp parsedTcp
		if !valid || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			MessagePoolReturn(packet)
			t.Fatal("actual SYN fixture is malformed")
		}
		accepted, err := buffer.send(source, protocol.ProvideMode_Network, &tcp, 0, packet)
		if !accepted {
			MessagePoolReturn(packet)
		}
		if accepted || err != nil {
			t.Fatalf("near-full SYN admitted: %t %v", accepted, err)
		}
	}
	if len(buffer.sequences) != 0 || len(buffer.sourceSequences) != 0 || dials.Load() != 0 || settings.MemoryBudget.UsedByteCount() != 0 {
		t.Fatal("refused SYN installed idle flow state")
	}
}

func TestNatMemoryProtocolAdmissionNeedsNoByteReserve(t *testing.T) {
	assertMessagePoolOwnership(t)
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
	before := budget.Stats()
	memory, admitted := nat.reserveSendPacketsMemory([][]byte{packet})
	if !admitted || memory.budget != nil || memory.bytes != 0 || budget.Stats() != before {
		t.Fatal("protocol admission acquired DATA or reserved protocol bytes")
	}
	packetWithHugeRoot := make([]byte, len(packet), 4096)
	copy(packetWithHugeRoot, packet)
	if natControlPackets([][]byte{packetWithHugeRoot}) {
		t.Fatal("large backing root escaped through control classification")
	}
	owned := MessagePoolShareReadOnly(packet)
	if !nat.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, owned, 0) {
		MessagePoolReturn(owned)
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
	maximum := DefaultTcpBufferSettings().MemoryColdWriteBatchSize
	if got := state.natPacketizationByteLimit(DefaultMtu, maximum); got != maximum {
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
	created := make(chan *TcpSequence, 1)
	settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) { created <- sequence }
	local, origin := net.Pipe()
	defer origin.Close()
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
	nat, err := TryNewLocalUserNat(ctx, "tcp-full", settings)
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan struct{}, 1)
	nat.afterSendPacketForTest = func() {
		select {
		case processed <- struct{}{}:
		default:
		}
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
	var receiveLock sync.Mutex
	var receiveBase, receiveNext uint32
	var receiveStarted bool
	var queueRefusals, ackAdmissions atomic.Int64
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
		// A replay is not a second delivery. This peer acknowledges only its
		// contiguous frontier and never counts duplicate payload toward total.
		receiveLock.Lock()
		if tcp.syn && !receiveStarted {
			receiveBase = tcp.seq + 1
			receiveNext = receiveBase
			receiveStarted = true
		}
		end := tcp.seq + uint32(len(tcp.payload))
		if receiveStarted && !tcp.syn && int32(tcp.seq-receiveNext) <= 0 && 0 < int32(end-receiveNext) {
			receiveNext = end
		}
		ackNumber := receiveNext
		uniqueReceived := int64(uint32(receiveNext - receiveBase))
		received.Store(uniqueReceived)
		receiveLock.Unlock()
		state := ConnectionState{ipVersion: 4, sourceIp: path.DestinationIp, sourcePort: uint16(path.DestinationPort),
			destinationIp: path.SourceIp, destinationPort: uint16(path.SourcePort), sendSeq: ackNumber, windowSize: 65535}
		ack := state.tcpPacket(tcpFlagAck, 101, nil)
		memory, permitted := nat.reserveSendPacketsMemory([][]byte{ack})
		if !protocolOnlyFrame(protocol.MessageType_IpIpPacketToProvider, true, ack) || !permitted || memory.budget != nil || memory.bytes != 0 {
			memory.release()
			MessagePoolReturn(ack)
			t.Error("actual cumulative TCP ACK acquired or required packet-byte credit")
			return
		}
		if !sendPacket(ack) {
			MessagePoolReturn(ack)
			if ctx.Err() == nil {
				// This is a shared, nonblocking callback. With byte permission
				// proved above and the lifecycle open, false is queue capacity;
				// the next cumulative ACK/replay can regenerate this feedback.
				nat.sendLock.Lock()
				closing := nat.sendClosed
				nat.sendLock.Unlock()
				if closing {
					t.Error("live ACK fixture unexpectedly closed NAT admission")
				} else {
					queueRefusals.Add(1)
				}
			}
		} else {
			ackAdmissions.Add(1)
		}
		if tcp.syn {
			handshakeOnce.Do(func() { close(handshake) })
		}
		if uniqueReceived == total {
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
	var flow *TcpSequence
	select {
	case flow = <-created:
	case <-ctx.Done():
		t.Fatal("actual TCP flow was not published")
	}
	written := make(chan error, 1)
	writerJoined := false
	defer func() {
		if !writerJoined {
			cancel()
			origin.Close()
			select {
			case <-written:
			case <-time.After(5 * time.Second):
				t.Error("origin writer did not join after cancellation")
			}
		}
	}()
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
	writeErr := <-written
	writerJoined = true
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	receiveLock.Lock()
	wantAck := receiveNext
	uniqueReceived := uint32(receiveNext - receiveBase)
	receiveLock.Unlock()
	if uniqueReceived != total || ackAdmissions.Load() == 0 {
		t.Fatalf("unique cumulative progress=%d, admitted ACKs=%d", uniqueReceived, ackAdmissions.Load())
	}
	for {
		flow.mutex.Lock()
		actualAck, retained := flow.receiveSeqAck, flow.returnByteCount
		flow.mutex.Unlock()
		if actualAck == wantAck && retained == 0 {
			if flow.ctx.Err() != nil {
				t.Fatal("flow teardown cannot stand in for cumulative ACK progress")
			}
			break
		}
		select {
		case <-processed:
		case <-ctx.Done():
			t.Fatalf("final cumulative ACK did not retire actual return chunks: ack=%d want=%d retained=%d queue_refusals=%d", actualAck, wantAck, retained, queueRefusals.Load())
		}
	}
	if budget.UsedByteCount() > budget.TotalByteCount() {
		t.Fatal("TCP progress overdrew its budget")
	}
	t.Logf("unique_bytes=%d final_ack=%d retained_return_bytes=0 ack_admissions=%d zero_wait_queue_refusals=%d packet_byte_claim=0 ingress_count_cap=%d", uniqueReceived, wantAck, ackAdmissions.Load(), queueRefusals.Load(), cap(nat.sendPackets))
}

// A shared callback may regenerate an ACK refused by the existing count
// boundary. Refusal must leave its exact root with the caller; clearing that
// queue makes the same owner admissible without releasing DATA budget.
func TestNatMemoryProtocolQueueFullRefusalKeepsOwnerAndRecovers(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	budget := NewTransferMemoryBudget(mib(2))
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget = budget
	settings.Log = NewNoopLogger()
	settings.TcpBufferSettings.EnableOrphanRst = false
	nat, err := TryNewLocalUserNat(ctx, "protocol-count-full", settings)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var held atomic.Bool
	processed := make(chan struct{}, providerReturnBatchMaxFrames+2)
	nat.beforeSendPacketForTest = func(*SendPacket) {
		if held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	nat.afterSendPacketForTest = func() { processed <- struct{}{} }
	var fill ByteCount
	defer func() {
		cancel()
		unblock()
		if err := nat.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		budget.Release(fill)
		if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("protocol queue cleanup leaked roots or DATA claims: %+v", stats)
		}
	}()
	available := budget.Available()
	if !budget.TryReserve(available) {
		t.Fatal("could not fill DATA before protocol admission")
	}
	fill = available
	if cap(nat.sendPackets) != providerReturnBatchMaxFrames || budget.Available() != 0 {
		t.Fatal("fixture lost the real16-entry ingress queue or full DATA budget")
	}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(10, 0, 0, 1), SourcePort: 40000,
		DestinationIp: net.IPv4(203, 0, 113, 1), DestinationPort: 443}
	template := ipOosTcpPacket(path, tcpFlagAck, nil)
	source := SourceId(NewId())
	offer := func() {
		packet := MessagePoolCopy(template)
		if !nat.SendPacket(source, protocol.ProvideMode_Network, packet, 0) {
			MessagePoolReturn(packet)
			t.Fatal("protocol ACK was refused before the fixed queue filled")
		}
	}
	offer()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first actual shard owner did not reach its barrier")
	}
	for range providerReturnBatchMaxFrames {
		offer()
	}
	if len(nat.sendPackets) != providerReturnBatchMaxFrames {
		t.Fatal("real shard barrier did not saturate exact ingress count")
	}
	refused := MessagePoolCopy(template)
	defer func() { MessagePoolReturn(refused) }()
	memory, permitted := nat.reserveSendPacketsMemory([][]byte{refused})
	if !permitted || memory.budget != nil || memory.bytes != 0 {
		memory.release()
		t.Fatal("protocol ACK did not have zero-byte permission at the count boundary")
	}
	before := budget.Stats()
	if nat.SendPacket(source, protocol.ProvideMode_Network, refused, 0) {
		refused = nil
		t.Fatal("zero-wait protocol send bypassed the full ingress queue")
	}
	if !bytes.Equal(refused, template) || budget.Stats() != before || len(nat.sendPackets) != providerReturnBatchMaxFrames {
		t.Fatal("count refusal consumed caller ownership or modified byte admission")
	}
	unblock()
	for range providerReturnBatchMaxFrames + 1 {
		select {
		case <-processed:
		case <-ctx.Done():
			t.Fatal("admitted protocol owners did not drain after the shard barrier")
		}
	}
	if !nat.SendPacket(source, protocol.ProvideMode_Network, refused, 0) {
		t.Fatal("same retained ACK owner did not recover after queue capacity returned")
	}
	refused = nil
	select {
	case <-processed:
	case <-ctx.Done():
		t.Fatal("recovered exact ACK owner did not reach final NAT disposition")
	}
	if budget.Stats() != before || budget.Available() != 0 {
		t.Fatal("protocol count recovery required DATA capacity or a byte reserve")
	}
	t.Logf("queue_count_cap=%d forced_zero_wait_refusals=1 same_owner_recovered=true packet_byte_claim=0", cap(nat.sendPackets))
}
