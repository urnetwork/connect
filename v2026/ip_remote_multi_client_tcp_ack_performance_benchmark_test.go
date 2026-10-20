// Source-only proposed Multi CPU/contention measurements, not wire throughput.
// Use this exact file against both the pre-fix source and the recovery candidate.
package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	tcpAckBenchNetworkMode     = 1
	tcpAckBenchPayloadBytes    = 1448
	tcpAckBenchWindow          = uint16(65535)
	tcpAckBenchSourceInitial   = ^uint32(0) - 32*1024
	tcpAckBenchPeerInitial     = ^uint32(0) - 64*1024
	tcpAckBenchConcurrentDepth = 8
)

// Infers an existing public callback's mode without importing a child package.
func tcpAckBenchPacketCallback[Mode ~int32](
	_ func(TransferPath, Mode, *IpPath, []byte),
	observe func([]byte),
) func(TransferPath, Mode, *IpPath, []byte) {
	return func(_ TransferPath, _ Mode, _ *IpPath, packet []byte) { observe(packet) }
}

// The batch adapter preserves borrowed callback ownership and has no queue.
func tcpAckBenchPacketsCallback[Mode ~int32](
	_ func(TransferPath, Mode, *IpPath, [][]byte),
	observe func([]byte),
) func(TransferPath, Mode, *IpPath, [][]byte) {
	return func(_ TransferPath, _ Mode, _ *IpPath, packets [][]byte) {
		for _, packet := range packets {
			observe(packet)
		}
	}
}

// Type inference keeps the private raw-frame fixture free of child imports.
func tcpAckBenchSlice[T any](values ...T) []T {
	return values
}

// Builds checksummed, synthetic IPv4/TCP templates outside the timed loops.
func tcpAckBenchPacket(path *IpPath, sequence, ack uint32, flags byte, payloadBytes int) []byte {
	packet := ipOosTcpPacketSequence(path, flags, sequence, make([]byte, payloadBytes))
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	binary.BigEndian.PutUint16(tcp[14:16], tcpAckBenchWindow)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	return packet
}

// Reuses production incremental checksum repair; payload construction is not
// charged per packet. Each template has one writer and no asynchronous reader.
func tcpAckBenchNumbers(packet []byte, sequence, ack uint32) {
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	var previous [8]byte
	copy(previous[:], tcp[4:12])
	binary.BigEndian.PutUint32(tcp[4:8], sequence)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	checksum := incrementalChecksum(binary.BigEndian.Uint16(tcp[16:18]), previous[:], tcp[4:12])
	binary.BigEndian.PutUint16(tcp[16:18], checksum)
}

// Owns a real Multi constructor and its public callbacks. The established
// receive dispatcher is production code. Only the final outgoing Transfer
// admission uses the existing synchronous, ownership-taking test seam, except
// the explicitly native, permanently queued scan-depth cases below.
//
// One goroutine owns egress and one owns ingress; they may run concurrently.
// The atomic receive edge is the only fixture state crossing that boundary.
type tcpAckBenchFixture struct {
	t                  testing.TB
	ctx                context.Context
	cancel             context.CancelFunc
	closeOnce          sync.Once
	workers            sync.WaitGroup
	parent             *RemoteUserNatMultiClient
	channel            *multiClientChannel
	update             *multiClientChannelUpdate
	source             TransferPath
	providerSource     TransferPath
	egressPath         *IpPath
	ingressPath        *IpPath
	egressAckTemplate  []byte
	egressDataTemplate []byte
	ingressPackets     [][]byte
	receive            func(uint32)
	sourceNext         uint32
	peerNext           atomic.Uint32
	lastAck            uint32
	lastEgressAck      uint32
	outgoingPacket     []byte
	seamCalls          uint64
	delivered          atomic.Uint64
	payloadBytes       int
	batchSize          int
	nativeClient       *Client
	nativeBudget       *TransferMemoryBudget
	nativeRoute        Route
	nativeEntered      chan struct{}
	nativeCapacity     int
	nativeDepth        int
}

// Establishes the actual SYN cohort, provider binding, advertised receive
// window, and upload high-water through the public send and receive paths.
func newTcpAckBenchFixture(t testing.TB, payloadBytes, batchSize int, nativeDepths ...int) *tcpAckBenchFixture {
	t.Helper()
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &tcpAckBenchFixture{
		t: t, ctx: ctx, cancel: cancel, source: SourceId(NewId()),
		providerSource: SourceId(NewId()), sourceNext: tcpAckBenchSourceInitial + 1,
		lastAck: tcpAckBenchSourceInitial + 1, lastEgressAck: tcpAckBenchPeerInitial + 1,
		payloadBytes: payloadBytes, batchSize: batchSize,
		egressPath: &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 131), DestinationIp: net.IPv4(198, 51, 100, 132),
			SourcePort: 45071, DestinationPort: 443},
	}
	t.Cleanup(f.close)
	f.peerNext.Store(tcpAckBenchPeerInitial + 1)
	f.ingressPath = f.egressPath.Reverse()
	observe := func(packet []byte) {
		if len(packet) < Ipv4HeaderSizeWithoutExtensions+TcpHeaderSizeWithoutExtensions || packet[33]&tcpFlagAck == 0 {
			t.Error("benchmark public callback received a non-ack TCP packet")
		}
		f.delivered.Add(1)
	}
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()
	settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	settings.LocalDialContextSettings = &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("benchmark unexpectedly attempted local fallback")
		},
	}
	f.parent = NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{},
		tcpAckBenchPacketCallback(ReceivePacketFunction(nil), observe), tcpAckBenchNetworkMode, settings)
	if !f.parent.settings.TcpCollapsePrevention || f.parent.ReliabilitySettings().TcpCollapseMaxHold != 0 ||
		f.parent.provideMode.String() != "Network" {
		t.Fatal("benchmark requires default persistent collapse and Network mode")
	}
	f.channel = newPacketTransferTestChannel()
	f.channel.ctx, f.channel.settings, f.channel.log = ctx, settings, NewNoopLogger()
	f.channel.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
		Destination:                    RequireMultiHopId(f.providerSource.SourceId),
	}
	if len(nativeDepths) != 0 && nativeDepths[0] != 0 {
		f.prepareNativeSource(nativeDepths[0])
	}
	f.channel.clientReceivePacketCallback = f.parent.clientReceivePacket
	if batchSize != 1 {
		f.parent.SetReceivePacketsCallback(tcpAckBenchPacketsCallback(ReceivePacketsFunction(nil), observe))
		f.channel.args.ReceivePackets = f.parent.clientReceivePackets
	}
	// The one egress producer owns this slot. Completion is synchronous; the
	// seam takes and returns the exact pooled root before admitting another.
	f.channel.sendTransferForTest = func(completed AckFunction) (bool, error) {
		if f.outgoingPacket == nil {
			return false, errors.New("benchmark admission has no caller packet")
		}
		f.seamCalls++
		completed(nil)
		MessagePoolReturn(f.outgoingPacket)
		f.outgoingPacket = nil
		return true, nil
	}
	syn := tcpAckBenchPacket(f.egressPath, tcpAckBenchSourceInitial, 0, tcpFlagSyn, 0)
	path, err := ParseIpPath(syn)
	if err != nil {
		t.Fatal(err)
	}
	f.update, _, _ = f.parent.sendUpdate(path, flowPin{})
	f.update.client.Store(f.channel)
	f.parent.bindClientFlow(f.update, f.channel)
	if !f.offer(syn) {
		t.Fatal("benchmark SYN was not admitted")
	}
	synAck := tcpAckBenchPacket(f.ingressPath, tcpAckBenchPeerInitial, f.sourceNext, tcpFlagSyn|tcpFlagAck, 0)
	frame, err := ipPacketFromProviderFrame(synAck, DefaultProtocolVersion)
	if err != nil || !frame.Raw || frame.MessageType.String() != "IpIpPacketFromProvider" {
		t.Fatalf("benchmark raw return frame: %v", err)
	}
	f.channel.clientReceive(f.providerSource, tcpAckBenchSlice(frame), Peer{ProvideMode: tcpAckBenchNetworkMode})
	f.egressAckTemplate = tcpAckBenchPacket(f.egressPath, f.sourceNext, f.peerNext.Load(), tcpFlagAck, 0)
	f.egressDataTemplate = tcpAckBenchPacket(f.egressPath, f.sourceNext, f.peerNext.Load(), tcpFlagAck, tcpAckBenchPayloadBytes)
	if _, accepted := f.send(false); !accepted {
		t.Fatal("benchmark handshake ACK was not admitted")
	}
	frames := tcpAckBenchSlice(frame)[:0]
	for range batchSize {
		packet := tcpAckBenchPacket(f.ingressPath, f.peerNext.Load(), f.sourceNext, tcpFlagAck, payloadBytes)
		f.ingressPackets = append(f.ingressPackets, packet)
		frame, err := ipPacketFromProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	f.receive = func(ack uint32) {
		sequence := f.peerNext.Load()
		for _, packet := range f.ingressPackets {
			tcpAckBenchNumbers(packet, sequence, ack)
			sequence += uint32(f.payloadBytes)
		}
		f.channel.clientReceive(f.providerSource, frames, Peer{ProvideMode: tcpAckBenchNetworkMode})
		f.lastAck = ack
		f.peerNext.Store(sequence)
	}
	f.requireState(1)
	return f
}

// Offers one pooled root through the public API; only refusal retains caller
// ownership. No timed iteration bypasses parse, policy, lookup, or admission.
func (self *tcpAckBenchFixture) offer(template []byte) bool {
	packet := MessagePoolCopy(template)
	self.outgoingPacket = packet
	priorCalls := self.seamCalls
	accepted := self.parent.SendPacket(self.source, tcpAckBenchNetworkMode, packet, 0)
	if !accepted && self.outgoingPacket != nil {
		MessagePoolReturn(self.outgoingPacket)
	}
	self.outgoingPacket = nil
	return accepted && self.seamCalls == priorCalls+1
}

// A progressing upload or advertised receive edge keeps ordinary admission
// eligible; no recovery exception is consumed by the performance workload.
func (self *tcpAckBenchFixture) send(payload bool) (uint32, bool) {
	template := self.egressAckTemplate
	if payload {
		template = self.egressDataTemplate
	}
	ack := self.peerNext.Load()
	tcpAckBenchNumbers(template, self.sourceNext, ack)
	if !self.offer(template) {
		return self.sourceNext, false
	}
	self.lastEgressAck = ack
	if payload {
		self.sourceNext += tcpAckBenchPayloadBytes
	}
	return self.sourceNext, true
}

// Installs an optional actual source before the channel is published to the
// parent. No concurrently observed channel client field is replaced later.
func (self *tcpAckBenchFixture) prepareNativeSource(depth int) {
	self.t.Helper()
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = depth
	settings.SendBufferSettings.PrewarmOpeningContract = false
	self.nativeBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
	settings.SendBufferSettings.ResendQueueBudget = self.nativeBudget
	settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
	settings.beforeClientKeyPublishForTest = func() { <-self.ctx.Done() }
	self.nativeEntered = make(chan struct{})
	self.nativeCapacity = depth
	var once sync.Once
	peer := self.providerSource.SourceId
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == peer {
			if id.LogicalLane != 0 {
				self.t.Error("benchmark source negotiated an unexpected logical lane")
			}
			once.Do(func() { close(self.nativeEntered) })
			<-self.ctx.Done()
		}
	}
	self.nativeClient = NewClient(self.ctx, self.channel.ClientId(), NewNoContractClientOob(), settings)
	self.nativeClient.ContractManager().AddNoContractPeer(peer)
	self.nativeRoute = make(Route, depth)
	self.nativeClient.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peer)), []Route{self.nativeRoute})
	self.channel.client = self.nativeClient
}

// Real public native admissions leave a fixed number of current-provider raw
// claims before Run. Thus the pending-reset scans cannot accidentally benchmark
// an empty list or only already-dequeued groups. No packet is sent on a route.
func (self *tcpAckBenchFixture) queueNativeClaims(depth int) {
	self.t.Helper()
	if self.nativeClient == nil || self.nativeCapacity != depth {
		self.t.Fatal("native source must be constructed before the flow is published")
	}
	self.channel.sendTransferForTest = nil
	for index := range depth {
		template := tcpAckBenchPacket(self.egressPath, self.sourceNext+uint32(index), self.peerNext.Load(), tcpFlagAck, 1)
		packet := MessagePoolCopy(template)
		if !self.parent.SendPacket(self.source, tcpAckBenchNetworkMode, packet, 0) {
			MessagePoolReturn(packet)
			self.t.Fatalf("native pending claim %d/%d refused", index+1, depth)
		}
	}
	select {
	case <-self.nativeEntered:
	case <-self.ctx.Done():
		self.t.Fatal("native source did not reach its pre-Run barrier")
	}
	self.nativeDepth = depth
	self.requireClaims()
}

// Checks the actual linked groups rather than treating queue admission as proof
// of the scan shape. Held workers cannot change this list while timing runs.
func (self *tcpAckBenchFixture) requireClaims() {
	self.t.Helper()
	self.update.stateLock.Lock()
	count := 0
	valid := true
	for group := self.update.sequenceClaims; group != nil; group = group.collapseNext {
		count++
		valid = valid && group.completionClient == self.channel && group.collapseOrder() == 0 &&
			group.completionFlags.Load()&groupCompletionTracked != 0 && len(group.packets) == 1 &&
			!group.packets[0].ipPath.Syn && !group.packets[0].ipPath.Rst
	}
	self.update.stateLock.Unlock()
	if !valid || count != self.nativeDepth {
		self.t.Fatalf("pending scan shape: count=%d want=%d valid=%t", count, self.nativeDepth, valid)
	}
	if len(self.nativeRoute) != 0 {
		self.t.Fatal("pre-Run benchmark source wrote a route packet")
	}
}

// All checks run outside benchmark timing. Reflection only reads optional new
// scalar state under its lock, so the exact same source builds before the fix.
// The candidate must have accepted the final valid frontier, not just returned
// through its out-of-window or above-upload-high-water rejection paths.
func (self *tcpAckBenchFixture) requireState(delivered uint64) {
	self.t.Helper()
	if self.delivered.Load() != delivered {
		self.t.Fatalf("public callback deliveries=%d want=%d", self.delivered.Load(), delivered)
	}
	if self.parent.TcpCollapseDropCount() != 0 {
		self.t.Fatalf("healthy benchmark sends collapsed: %d", self.parent.TcpCollapseDropCount())
	}
	var stateErr string
	func() {
		self.update.stateLock.Lock()
		defer self.update.stateLock.Unlock()
		if !self.update.receivedInbound.Load() || self.update.client.Load() != self.channel ||
			!self.update.synGenerationSeen || self.update.synGenerationAwaiting ||
			self.update.synGenerationNumber != tcpAckBenchSourceInitial || self.update.sequenceNumber != self.sourceNext ||
			!self.update.ingressAckSeen || self.update.ingressAckSequence != self.lastAck {
			stateErr = "established cohort, upload high-water, or delivered ingress ACK changed"
			return
		}
		recovery := reflect.ValueOf(self.update).Elem().FieldByName("sequenceRecovery")
		if recovery.IsValid() && (!recovery.FieldByName("seen").Bool() ||
			uint32(recovery.FieldByName("ack").Uint()) != self.lastAck ||
			!recovery.FieldByName("peerSeen").Bool() ||
			uint32(recovery.FieldByName("peerSequence").Uint()) != self.lastEgressAck ||
			uint32(recovery.FieldByName("peerWindow").Uint()) != uint32(tcpAckBenchWindow) ||
			recovery.FieldByName("reserved").Bool()) {
			stateErr = "candidate did not retain the valid cumulative ACK frontier/window"
		}
	}()
	if stateErr != "" {
		self.t.Fatal(stateErr)
	}
	for _, packet := range self.ingressPackets {
		if ipPathTransportChecksum(self.ingressPath, ipProtocolNumberTcp, packet[Ipv4HeaderSizeWithoutExtensions:]) != 0 {
			self.t.Fatal("incrementally updated ingress checksum is invalid")
		}
	}
	self.requireClaims()
}

// Cancel before joining any fixture worker, then join the actual native owner
// before closing the Multi flow that its completion metadata references.
func (self *tcpAckBenchFixture) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		self.workers.Wait()
		if self.nativeClient != nil {
			if err := self.nativeClient.CloseAndWait(context.Background()); err != nil {
				self.t.Errorf("join benchmark native source: %v", err)
			}
		}
		if self.parent != nil {
			if err := self.parent.CloseAndWait(context.Background()); err != nil {
				self.t.Errorf("join benchmark Multi: %v", err)
			}
		}
		for len(self.nativeRoute) != 0 {
			MessagePoolReturn(<-self.nativeRoute)
		}
		if self.nativeBudget != nil && self.nativeBudget.UsedByteCount() != 0 {
			self.t.Errorf("joined native benchmark retained %d bytes", self.nativeBudget.UsedByteCount())
		}
	})
}

// Proves fixture flags, constructor/public delivery, checksums, wraparound and
// native claim shape on both source versions before any benchmark comparison.
func TestTcpRecoveryAckPerformanceFixture(t *testing.T) {
	for _, payloadBytes := range []int{0, tcpAckBenchPayloadBytes} {
		for _, batchSize := range []int{1, 8} {
			f := newTcpAckBenchFixture(t, payloadBytes, batchSize)
			for index := range 96 {
				ack, accepted := f.send(true)
				if !accepted {
					t.Fatalf("fixture payload=%d batch=%d iteration=%d admission failed", payloadBytes, batchSize, index)
				}
				f.receive(ack)
				f.requireState(1 + uint64(index+1)*uint64(batchSize))
			}
			if payloadBytes != 0 {
				if _, accepted := f.send(false); !accepted {
					t.Fatal("download-only cumulative ACK did not advance ordinary admission")
				}
				f.requireState(1 + 96*uint64(batchSize))
			}
			f.close()
		}
	}
	for _, depth := range []int{32, 64} {
		f := newTcpAckBenchFixture(t, 0, 8, depth)
		f.queueNativeClaims(depth)
		f.receive(f.sourceNext)
		f.requireState(9)
		f.close()
	}
}

// Duplicate pure ACKs are valid established controls. Each candidate packet
// takes the new ingress lock; queued native groups expose its extra reset scan.
func BenchmarkTcpRecoveryEstablishedPureAckIngress(b *testing.B) {
	for _, batchSize := range []int{1, 8} {
		for _, depth := range []int{0, 32, 64} {
			b.Run(fmt.Sprintf("batch=%d/pending=%d", batchSize, depth), func(b *testing.B) {
				f := newTcpAckBenchFixture(b, 0, batchSize, depth)
				if depth != 0 {
					f.queueNativeClaims(depth)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					f.receive(f.sourceNext)
				}
				b.StopTimer()
				b.ReportMetric(float64(batchSize), "ingress-packets/op")
				f.requireState(1 + uint64(b.N)*uint64(batchSize))
			})
		}
	}
}

// One real public upload admission precedes each valid advancing pure ACK.
func BenchmarkTcpRecoveryEstablishedAckUpload(b *testing.B) {
	f := newTcpAckBenchFixture(b, 0, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ack, accepted := f.send(true)
		if !accepted {
			b.Fatal("healthy upload admission failed")
		}
		f.receive(ack)
	}
	b.StopTimer()
	b.ReportMetric(1, "ingress-packets/op")
	b.ReportMetric(1, "egress-packets/op")
	f.requireState(1 + uint64(b.N))
}

// Each payload burst is followed by a public source ACK, keeping the moving
// receive edge/window valid across every wrap instead of rejecting late loops.
func BenchmarkTcpRecoveryEstablishedPayloadAckDownload(b *testing.B) {
	for _, batchSize := range []int{1, 8} {
		b.Run(fmt.Sprintf("batch=%d", batchSize), func(b *testing.B) {
			f := newTcpAckBenchFixture(b, tcpAckBenchPayloadBytes, batchSize)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				f.receive(f.sourceNext)
				if _, accepted := f.send(false); !accepted {
					b.Fatal("healthy download ACK admission failed")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(batchSize), "ingress-packets/op")
			b.ReportMetric(1, "egress-packets/op")
			f.requireState(1 + uint64(b.N)*uint64(batchSize))
		})
	}
}

// Measures sequential public upload and payload-bearing return on one flow.
func BenchmarkTcpRecoveryEstablishedPayloadAckFullDuplex(b *testing.B) {
	f := newTcpAckBenchFixture(b, tcpAckBenchPayloadBytes, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ack, accepted := f.send(true)
		if !accepted {
			b.Fatal("healthy full-duplex admission failed")
		}
		f.receive(ack)
	}
	b.StopTimer()
	b.ReportMetric(1, "ingress-packets/op")
	b.ReportMetric(1, "egress-packets/op")
	f.requireState(1 + uint64(b.N))
}

// One producer and one receiver overlap on the same real flow mutex. The
// bounded scalar channel allows at most nine uploads ahead of ingress, keeping
// every payload's peer sequence inside the last advertised 65535-byte window.
// No callback waits or sends: only the independent benchmark producer may wait.
func BenchmarkTcpRecoveryContendedPayloadAckFullDuplex(b *testing.B) {
	f := newTcpAckBenchFixture(b, tcpAckBenchPayloadBytes, 1)
	acks := make(chan uint32, tcpAckBenchConcurrentDepth)
	start := make(chan struct{})
	var admitted atomic.Uint64
	f.workers.Add(1)
	go func() {
		defer f.workers.Done()
		defer close(acks)
		select {
		case <-start:
		case <-f.ctx.Done():
			return
		}
		for range b.N {
			ack, accepted := f.send(true)
			if !accepted {
				return
			}
			admitted.Add(1)
			select {
			case acks <- ack:
			case <-f.ctx.Done():
				return
			}
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	close(start)
	for range b.N {
		ack, ok := <-acks
		if !ok {
			b.Fatal("contended upload admission failed")
		}
		f.receive(ack)
	}
	f.workers.Wait()
	b.StopTimer()
	b.ReportMetric(1, "ingress-packets/op")
	b.ReportMetric(1, "egress-packets/op")
	if admitted.Load() != uint64(b.N) {
		b.Fatalf("contended admissions=%d want=%d", admitted.Load(), b.N)
	}
	f.requireState(1 + uint64(b.N))
}

// Secondary diagnostic only: isolates the changed pre-delivery observer from
// parsing, channel stats, public callback, and the existing post-delivery lock.
// The native-depth fixtures are identical to the primary receive benchmarks.
func BenchmarkTcpRecoveryAckObservation(b *testing.B) {
	for _, depth := range []int{0, 32, 64} {
		b.Run(fmt.Sprintf("pending=%d", depth), func(b *testing.B) {
			f := newTcpAckBenchFixture(b, 0, 1, depth)
			if depth != 0 {
				f.queueNativeClaims(depth)
			}
			path, err := ParseIpPath(f.ingressPackets[0])
			if err != nil || !path.Ack || path.Syn {
				b.Fatalf("observer fixture is not an ordinary ACK: %v", err)
			}
			control := tcpControlFromIpPath(path)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				f.update.markReceivedInbound(f.channel, control)
			}
			b.StopTimer()
			f.requireState(1)
		})
	}
}
