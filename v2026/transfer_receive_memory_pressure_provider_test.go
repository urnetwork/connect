package connect

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The sender and receiver use encoded channel routes. Only the downstream TCP
// consumer is parked; its real one-slot queue already contains a paid SYN.
type receiveProviderPressureFixture struct {
	*receivePressureTestFixture
	natBudget             *TransferMemoryBudget
	foreignBudget         *TransferMemoryBudget
	nat                   *LocalUserNat
	provider              *RemoteUserNatProvider
	tcp                   *TcpSequence
	path                  *IpPath
	gate                  chan struct{}
	entered               chan *providerReliablePacket
	ackCounts             map[uint64]int
	dials                 atomic.Int64
	foreignBytes          ByteCount
	decodeBytes           ByteCount
	deliveryBytes         ByteCount
	prepaidBytes          ByteCount
	natStandingBytes      ByteCount
	smallPrefix           bool
	diagnosticsMessageId  Id
	diagnosticsSequenceId Id
	closed                bool
}

func newReceiveProviderPressureFixture(t *testing.T) *receiveProviderPressureFixture {
	return newReceiveProviderPressureFixtureWithPrefix(t, false)
}

// A small committed second original establishes a real predecessor promise
// before larger tentative arrivals consume the remaining root capacity.
func newReceiveProviderPressureFixtureWithPrefix(t *testing.T, smallPrefix bool) *receiveProviderPressureFixture {
	return newReceiveProviderPressureFixtureWithHold(t, smallPrefix, true)
}

// The no-tail control isolates restored decode credit from tentative reclaim.
func newReceiveProviderPressureFixtureWithHold(t *testing.T, smallPrefix bool, holdTails bool) *receiveProviderPressureFixture {
	t.Helper()
	root := NewTransferMemoryBudget(mib(4))
	rx := NewTransferMemoryBudgetWithParent(kib(64), root)
	self := &receiveProviderPressureFixture{
		receivePressureTestFixture: &receivePressureTestFixture{root: root, rx: rx, originals: map[uint64]*windowRoundFrame{}},
		natBudget:                  NewTransferMemoryBudgetWithParent(mib(3), root),
		foreignBudget:              NewTransferMemoryBudgetWithParent(mib(4), root),
		path:                       reliableDeliveryTestPath(4), gate: make(chan struct{}), entered: make(chan *providerReliablePacket, 16), ackCounts: map[uint64]int{},
		smallPrefix: smallPrefix,
	}
	self.windowRoundFixture = newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ProtocolVersion = 2
		settings.ResendQueueMaxByteCount = kib(64)
	}, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueBudget = rx
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveQueueMinByteCount = 0
		settings.ReceiveQueueMaxByteCount = kib(64)
		settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		settings.AdvertiseReceiveWindow = false
		settings.PackQueueBudget = nil
		settings.PackQueueRetainedByteAccounting = false
	}, TransportTypeH1)
	t.Cleanup(func() { self.close() })
	warm := self.write(1280)
	self.forward(warm, self.receiverIn)
	self.acknowledge()
	self.sequence = self.receiveSequence()
	if self.ackedCount != 1 || self.deliveredCount != 1 || root.UsedByteCount() != 0 {
		t.Fatal("provider pressure warm prefix did not retire")
	}

	natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
	natSettings.Log, natSettings.MemoryBudget = NewNoopLogger(), self.natBudget
	natSettings.TcpBufferSettings.WriteTimeout = 0
	captured := make(chan *TcpSequence, 1)
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
		captured <- sequence
		// Ancestor cancellation can precede this worker's own cancellation.
		// Match the established receipt fixture: hold this exact TCP owner.
		<-sequence.ctx.Done()
	}
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			// A queued setup SYN may be selected during teardown. Canceled
			// calls return without a socket and are not live dial attempts.
			if ctx.Err() == nil {
				self.dials.Add(1)
			}
			return nil, context.Canceled
		},
	}
	self.nat = NewLocalUserNat(self.ctx, "provider pressure ownership", natSettings)
	processed := make(chan struct{}, 16)
	self.nat.afterSendPacketForTest = func() { processed <- struct{}{} }
	self.nat.beforeSendPacketForTest = func(packet *SendPacket) {
		if packet.reliable == nil {
			return
		}
		self.entered <- packet.reliable
		select {
		case <-self.gate:
		case <-self.ctx.Done():
		}
	}
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	self.provider = NewRemoteUserNatProvider(self.receiver, self.nat, providerSettings)
	syn := MessagePoolCopy(ipOosTcpPacketSequence(self.path, tcpFlagSyn, 100, nil))
	if !self.nat.SendPacket(SourceId(self.sender.ClientId()), protocol.ProvideMode_Public, syn, 0) {
		MessagePoolReturn(syn)
		t.Fatal("provider pressure initial SYN was refused")
	}
	self.tcp = <-captured
	<-processed
	synctest.Wait()
	if len(self.tcp.sendItems) != 1 || len(self.nat.sendPackets) != 0 || self.dials.Load() != 0 {
		t.Fatal("provider pressure did not isolate the real full TCP queue")
	}
	self.natStandingBytes = self.natBudget.UsedByteCount()
	if root.UsedByteCount() != self.natStandingBytes {
		t.Fatal("unclassified provider standing charge")
	}

	for number := uint64(1); number <= 11; number++ {
		payloadSize := 1240
		sequenceNumber := uint32(101 + (number-1)*1240)
		if smallPrefix && number == 2 {
			payloadSize = 1
		}
		if smallPrefix && number > 2 {
			sequenceNumber -= 1239
		}
		payload := bytes.Repeat([]byte{byte(number)}, payloadSize)
		packet := MessagePoolCopy(ipOosTcpPacketSequence(self.path, tcpFlagAck, sequenceNumber, payload))
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil || !frame.Raw {
			MessagePoolReturn(packet)
			t.Fatal("provider pressure did not create a raw TCP frame")
		}
		accepted, err := self.sender.SendWithTimeoutDetailed(frame, self.receiver.ClientId(), func(err error) {
			if err == nil {
				self.ackedCount++
				self.ackCounts[number]++
			}
		}, time.Second)
		if !accepted || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("provider pressure sender refused an original")
		}
		original := self.takePack(number)
		self.nextNumber = number + 1
		if original.pack.Nack || original.pack.SequenceNumber != number || len(original.pack.Frames) != 1 ||
			original.pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketToProvider ||
			!bytes.Equal(original.pack.SequenceId, warm.pack.SequenceId) {
			t.Fatal("provider pressure changed reliable source identity or frame type")
		}
		self.originals[number] = original
		scan, status := scanReceiveMemoryPack(original.bytes, TransferPath{SourceId: self.sender.ClientId(), DestinationId: self.receiver.ClientId()})
		if status != receiveMemoryPackScanReady || scan.nack {
			t.Fatal("provider pressure original is not owned-decode eligible")
		}
		if number == 1 {
			self.decodeBytes = scan.retainedByteCount
			// This sizes the fixture using the actual prepay path. The held
			// owners below must independently carry exactly the same charge.
			probe := &receiveItem{transferItem: transferItem{queueByteCount: self.decodeBytes}, ack: true, frames: original.pack.Frames}
			if !self.sequence.prepareReceiveDeliveryCredit(probe) {
				t.Fatal("unowned geometry probe was refused")
			}
			self.deliveryBytes, self.prepaidBytes = probe.QueueByteCount(), probe.deliveryPrepaid
		} else if !(smallPrefix && number == 2) && scan.retainedByteCount != self.decodeBytes {
			t.Fatal("provider pressure originals cross decoded-owner size classes")
		}
	}
	if self.decodeBytes <= 0 || self.deliveryBytes <= self.decodeBytes || self.prepaidBytes <= 0 ||
		2*self.deliveryBytes >= rx.TotalByteCount() || root.Available() <= 2*self.deliveryBytes {
		t.Fatal("provider pressure lacks two full TCP owners and distinct downstream credit")
	}
	if !holdTails {
		if smallPrefix {
			t.Fatal("no-tail control cannot create a committed prefix")
		}
		self.reserveForeign(root.Available())
		if rx.UsedByteCount() != 0 || self.sequence.receiveQueue.Len() != 0 || self.sequence.holdPromise.set ||
			self.sequence.memoryPressure != (receiveMemoryPressure{}) || len(self.receiverOut) != 0 || self.ackedCount != 1 {
			t.Fatal("no-tail provider control retained unexpected receive custody")
		}
		return self
	}
	if smallPrefix {
		self.forward(self.originals[2], self.receiverIn)
		prefix := self.sequence.receiveQueue.GetBySequenceNumber(2)
		if prefix == nil || !prefix.committed || prefix.QueueByteCount() >= self.decodeBytes ||
			!self.sequence.holdPromise.set || self.sequence.holdPromise.boundary != 2 ||
			self.sequence.holdPromise.reservedByteCount != prefix.QueueByteCount() {
			t.Fatal("small provider original did not establish its real smaller SACK promise")
		}
		acks := self.takeAcknowledgements()
		if len(acks) == 0 {
			t.Fatal("small committed original emitted no selective acknowledgement")
		}
		for _, ack := range acks {
			if !ack.ack.Selective || !bytes.Equal(ack.ack.MessageId, self.originals[2].pack.MessageId) || len(ack.ack.EvictedSequenceNumbers) != 0 {
				t.Fatal("small committed original changed its selective custody")
			}
			self.forward(ack, self.senderIn)
		}
		self.reserveForeign(root.Available() - self.deliveryBytes)
		self.forward(self.originals[10], self.receiverIn)
		self.releaseForeign(self.deliveryBytes)
	} else {
		self.reserveForeign(root.Available() - 2*self.deliveryBytes)
		self.forward(self.originals[10], self.receiverIn)
	}
	self.forward(self.originals[11], self.receiverIn)
	for _, number := range []uint64{10, 11} {
		item := self.sequence.receiveQueue.GetBySequenceNumber(number)
		if item == nil || item.committed || !item.ack || item.memoryBudget != rx || item.decodedOwner == nil ||
			item.QueueByteCount() != self.deliveryBytes || item.deliveryPrepaid != self.prepaidBytes || !item.deliveryPrepared {
			t.Fatal("actual provider tail is not one uncommitted, fully prepaid decoded owner")
		}
	}
	expectedBytes := 2 * self.deliveryBytes
	repairBytes := ByteCount(0)
	if smallPrefix {
		prefix := self.sequence.receiveQueue.GetBySequenceNumber(2)
		if prefix == nil || !prefix.committed || self.sequence.holdPromise.reservedByteCount != prefix.QueueByteCount() ||
			self.sequence.holdPromise.reservedByteCount >= self.decodeBytes || self.sequence.holdPromise.boundary != 2 {
			t.Fatal("larger tentative owners changed the smaller committed prefix promise")
		}
		repairBytes = assertReliableIngressRepairCustody(t, self.sequence, prefix)
		if repairBytes != 1024 {
			t.Fatal("no-contract SACK did not own exactly its independent decoder repair envelope")
		}
		expectedBytes += 2*prefix.QueueByteCount() + repairBytes
	} else if self.sequence.heldHeadRepair != (receiveHeldHeadRepair{}) {
		t.Fatal("tentative provider tails created unfunded repair authority")
	}
	if root.Available() != 0 || rx.UsedByteCount() != expectedBytes || self.sequence.holdPromise.set != smallPrefix ||
		!smallPrefix && self.sequence.holdPromise.reservedByteCount != 0 || len(self.receiverOut) != 0 || self.ackedCount != 1 {
		t.Fatalf("provider pressure changed exact tail/prefix/repair ownership: root_free=%d rx=%d want=%d repair=%d small_prefix=%t",
			root.Available(), rx.UsedByteCount(), expectedBytes, repairBytes, smallPrefix)
	}
	return self
}

func (self *receiveProviderPressureFixture) reserveForeign(bytes ByteCount) {
	self.t.Helper()
	if bytes < 0 || !self.foreignBudget.TryReserve(bytes) {
		self.t.Fatal("competing reservation could not take observed capacity")
	}
	self.foreignBytes += bytes
}

func (self *receiveProviderPressureFixture) releaseForeign(bytes ByteCount) {
	self.t.Helper()
	if bytes < 0 || bytes > self.foreignBytes {
		self.t.Fatal("invalid competing reservation return")
	}
	self.foreignBudget.Release(bytes)
	self.foreignBytes -= bytes
}

// Return the actual captured source retry. No sender timer, sequence field,
// source original, or receive worker is changed to manufacture progress.
func (self *receiveProviderPressureFixture) retryHead() {
	self.t.Helper()
	original := self.originals[1]
	retry := self.naturalRetry(original)
	scan, status := scanReceiveMemoryPack(retry.bytes, TransferPath{SourceId: self.sender.ClientId(), DestinationId: self.receiver.ClientId()})
	if status != receiveMemoryPackScanReady || scan.retainedByteCount != self.decodeBytes || retry.pack.Nack ||
		len(retry.pack.Frames) != 1 || !bytes.Equal(retry.pack.Frames[0].MessageBytes, original.pack.Frames[0].MessageBytes) {
		self.t.Fatal("provider retry changed paid decode size or original TCP bytes")
	}
	self.forward(retry, self.receiverIn)
}

func (self *receiveProviderPressureFixture) atNat() bool {
	synctest.Wait()
	return len(self.entered) != 0
}

// Two newly contiguous callbacks may publish the provider's one source
// diagnostic on its reverse sequence. Keep only that precisely classified
// telemetry in fixture custody; a pending head still forbids every real ACK.
func (self *receiveProviderPressureFixture) captureProviderFeedback(allowAck bool) {
	self.t.Helper()
	synctest.Wait()
	for len(self.receiverOut) != 0 {
		frame := self.take(self.receiverOut)
		if frame.ack != nil {
			if !allowAck {
				self.t.Fatal("provider published an ACK before final TCP admission")
			}
			self.forward(frame, self.senderIn)
			continue
		}
		pack := frame.pack
		var outer protocol.TransferFrame
		decodeErr := ProtoUnmarshal(frame.bytes, &outer)
		path, pathErr := TransferPathFromProtobuf(outer.TransferPath)
		if decodeErr != nil || pathErr != nil || path.SourceId != self.receiver.ClientId() || path.DestinationId != self.sender.ClientId() ||
			path.IsStream() || len(outer.EncryptedTransferFrame) != 0 || pack == nil || !pack.Head || pack.Nack ||
			pack.SequenceNumber != 0 || pack.ForceStream || pack.CompanionContract || pack.LogicalLane != 0 || pack.ContractAhead ||
			len(pack.ContractId) != 0 || pack.ContractFrame != nil || len(pack.Frames) != 1 || pack.Frames[0] == nil ||
			pack.Frames[0].MessageType != protocol.MessageType_IpIpProviderDiagnostics || pack.Frames[0].Raw ||
			bytes.Equal(pack.SequenceId, self.originals[1].pack.SequenceId) || bytes.Equal(pack.MessageId, self.originals[1].pack.MessageId) {
			self.t.Fatal("unexpected reverse provider Pack at the withheld-head boundary")
		}
		var diagnostics protocol.IpProviderDiagnostics
		if err := ProtoUnmarshal(pack.Frames[0].MessageBytes, &diagnostics); err != nil || diagnostics.Sequence != 1 ||
			diagnostics.BlockIngressPacketCount != 0 || diagnostics.BlockIngressByteCount != 0 ||
			diagnostics.BlockEgressPacketCount != 0 || diagnostics.BlockEgressByteCount != 0 || len(diagnostics.SecurityPolicyHash) != 64 {
			self.t.Fatal("reverse diagnostic changed the source generation or reported application refusal")
		}
		messageId, messageErr := IdFromBytes(pack.MessageId)
		sequenceId, sequenceErr := IdFromBytes(pack.SequenceId)
		if messageErr != nil || sequenceErr != nil {
			self.t.Fatal("reverse diagnostic has invalid identity")
		}
		if self.diagnosticsMessageId == (Id{}) {
			self.diagnosticsMessageId, self.diagnosticsSequenceId = messageId, sequenceId
			self.t.Logf("[rx-prefix-provider-diagnostics-captured] source_sequence=1 policy_generation=%d blocked_packets=0 forwarded=false", diagnostics.GetSecurityPolicyGeneration())
		} else if self.diagnosticsMessageId != messageId || self.diagnosticsSequenceId != sequenceId {
			self.t.Fatal("reverse diagnostic changed its retained original identity")
		}
	}
}

// The NAT barrier is after the real partial move. Its receipt must keep the
// decoded owner paid while only explicit downstream prepayment changed child.
func (self *receiveProviderPressureFixture) proveNatCustody() {
	self.t.Helper()
	synctest.Wait()
	if self.smallPrefix {
		self.captureProviderFeedback(false)
	}
	var owner *providerReliablePacket
	select {
	case owner = <-self.entered:
	default:
		self.t.Fatal("provider original never reached the typed NAT owner")
	}
	receipt := owner.peer.deliveryReceipt()
	if receipt == nil || owner.operation == nil {
		self.t.Fatal("NAT packet bypassed a reliable delivery receipt")
	}
	receipt.queue.mutex.Lock()
	item := receipt.item
	physicalRoots := ByteCount(0)
	if item != nil {
		physicalRoots = MessagePoolRootByteCount(item.transferFrameBytes)
		for _, frame := range item.frames {
			physicalRoots += MessagePoolRootByteCount(frame.MessageBytes)
		}
	}
	valid := item != nil && item.decodedOwner != nil && item.decodedOwner.receivePack.receiveMemoryOwned &&
		item.memoryBudget == self.rx && item.QueueByteCount() >= self.decodeBytes && physicalRoots > 0 &&
		item.QueueByteCount() >= physicalRoots+decodedPackOwnerQueueByteCount &&
		owner.memory.budget == self.natBudget && owner.memory.bytes > 0 &&
		item.QueueByteCount()+owner.memory.bytes == self.deliveryBytes &&
		item.deliveryPrepaid+owner.memory.bytes == self.prepaidBytes &&
		item.messageId == RequireIdFromBytes(self.originals[1].pack.MessageId) && !receipt.secured
	receipt.queue.mutex.Unlock()
	if !valid || self.root.UsedByteCount() != self.rx.UsedByteCount()+self.natBudget.UsedByteCount()+self.foreignBytes ||
		self.ackedCount != 1 || len(self.receiverOut) != 0 || len(self.tcp.sendItems) != 1 || self.dials.Load() != 0 {
		self.t.Fatal("partial provider handoff released decoded roots, double charged the root, or ACKed before final TCP admission")
	}
	self.t.Logf("[rx-provider-pressure-custody] decode=%d full=%d moved=%d retained=%d physical_roots=%d root=%d ack=1", self.decodeBytes, self.deliveryBytes, owner.memory.bytes, item.QueueByteCount(), physicalRoots, self.root.UsedByteCount())
}

func (self *receiveProviderPressureFixture) drain() {
	self.t.Helper()
	close(self.gate)
	synctest.Wait()
	if self.smallPrefix {
		self.captureProviderFeedback(false)
	}
	if self.ackedCount != 1 || len(self.receiverOut) != 0 {
		self.t.Fatal("full TCP queue was cumulatively ACKed")
	}
	(<-self.tcp.sendItems).release()
	self.releaseForeign(self.foreignBytes)
	for number := uint64(1); number <= 11; number++ {
		if number > 1 && self.sequence.nextSequenceNumber <= number {
			original := self.originals[number]
			if original.bytes == nil {
				original = self.naturalRetry(original)
			}
			self.forward(original, self.receiverIn)
		}
		synctest.Wait()
		select {
		case item := <-self.tcp.sendItems:
			payloadSize := 1240
			sequenceNumber := uint32(101 + (number-1)*1240)
			if self.smallPrefix && number == 2 {
				payloadSize = 1
			}
			if self.smallPrefix && number > 2 {
				sequenceNumber -= 1239
			}
			valid := item.memory.budget == self.natBudget && item.tcp.seq == sequenceNumber &&
				bytes.Equal(item.tcp.payload, bytes.Repeat([]byte{byte(number)}, payloadSize))
			item.release()
			if !valid {
				self.t.Fatal("final TCP queue changed original bytes, ordering or NAT claim")
			}
		default:
			self.t.Fatalf("original %d did not reach the available final TCP queue", number)
		}
		if self.smallPrefix {
			self.captureProviderFeedback(true)
		} else {
			self.acknowledge()
		}
		if number == 1 && self.ackCounts[number] != 1 {
			self.t.Fatal("final TCP admission did not ACK the original provider head exactly once")
		}
	}
	synctest.Wait()
	if self.deliveredCount != 12 || self.ackedCount != 12 || self.sequence.nextSequenceNumber != 12 ||
		self.sequence.receiveQueue.Len() != 0 || self.sequence.memoryPressure.activeSet || self.sequence.memoryPressure.pendingSet ||
		self.rx.UsedByteCount() != 0 || self.sequence.ctx.Err() != nil || self.dials.Load() != 0 ||
		self.receiver.ReceiveStats().ReceiveQueueEvictionCount != 0 {
		self.t.Fatal("provider recovery failed full-prefix delivery, ownership retirement or resident continuity")
	}
	if count, _ := self.windowRoundFixture.sequence().resendQueue.QueueSize(); count != 0 {
		self.t.Fatal("provider recovery left acknowledged originals retained")
	}
	for number := uint64(1); number <= 11; number++ {
		if self.ackCounts[number] != 1 {
			self.t.Fatal("provider recovery skipped or duplicated an original ACK callback")
		}
	}
	self.t.Log("[rx-provider-pressure-recovered] natural_same_size_retry=true delivered=12 cumulative=12 rx=0 committed_evictions=0 dials=0")
}

func (self *receiveProviderPressureFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	clean := true
	self.cancel()
	if self.provider != nil {
		self.provider.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if self.nat != nil {
		if err := self.nat.CloseAndWait(ctx); err != nil {
			clean = false
			self.t.Errorf("provider pressure NAT join: %v", err)
		}
	}
	for _, client := range []*Client{self.sender, self.receiver} {
		if err := client.CloseAndWait(ctx); err != nil {
			clean = false
			self.t.Errorf("provider pressure Client join: %v", err)
		}
	}
	for _, frame := range self.heldFrames {
		if frame.bytes != nil {
			MessagePoolReturn(frame.bytes)
			frame.bytes = nil
		}
	}
	for _, route := range []Route{self.senderOut, self.senderIn, self.receiverOut, self.receiverIn} {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	}
	for len(self.entered) != 0 {
		<-self.entered
	}
	if self.foreignBytes != 0 {
		self.releaseForeign(self.foreignBytes)
	}
	for _, budget := range []*TransferMemoryBudget{self.root, self.rx, self.natBudget, self.foreignBudget} {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			clean = false
			self.t.Errorf("provider pressure cleanup imbalance: %+v", stats)
		}
	}
	if self.sequence != nil && self.sequence.heldHeadRepair != (receiveHeldHeadRepair{}) {
		clean = false
		self.t.Error("provider cleanup retained a repair owner or escrow after join")
	}
	if clean {
		self.t.Log("[rx-provider-pressure-cleanup] root=0 rx=0 nat=0 foreign=0 quota=0 clients_joined=true nat_joined=true")
	}
}

func testReceiveMemoryProviderSameSizeRetry(t *testing.T, compete bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceiveProviderPressureFixture(t)
		before := fixture.receiver.ReceiveStats()
		fixture.forward(fixture.originals[1], fixture.receiverIn)
		state := fixture.sequence.memoryPressure
		surplus := fixture.root.Available()
		if !state.activeSet || !state.ready || state.owner != nil || state.reservedByteCount != fixture.decodeBytes ||
			surplus != fixture.deliveryBytes-fixture.decodeBytes || fixture.sequence.receiveQueue.Len() != 1 ||
			fixture.sequence.receiveQueue.GetBySequenceNumber(10) == nil || fixture.sequence.receiveQueue.GetBySequenceNumber(11) != nil ||
			fixture.receiver.ReceiveStats().PackHandoffDropCount != before.PackHandoffDropCount+1 || len(fixture.receiverOut) != 0 {
			t.Fatal("provider first refusal did not reclaim one actual tentative TCP owner into exact decoded credit")
		}
		if compete {
			fixture.reserveForeign(surplus)
		}
		stalled := false
		stalledAttempts := 0
		for attempt := 0; attempt < 3; attempt++ {
			fixture.retryHead()
			if fixture.atNat() {
				break
			}
			state = fixture.sequence.memoryPressure
			if fixture.sequence.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil || fixture.ackedCount != 1 ||
				len(fixture.receiverOut) != 0 || fixture.sequence.nextSequenceNumber != 1 ||
				fixture.root.UsedByteCount() > fixture.root.TotalByteCount() ||
				fixture.root.UsedByteCount() != fixture.rx.UsedByteCount()+fixture.natBudget.UsedByteCount()+fixture.foreignBytes ||
				!state.activeSet || state.owner != nil || state.reservedByteCount < fixture.decodeBytes ||
				state.active.messageId != RequireIdFromBytes(fixture.originals[1].pack.MessageId) {
				t.Fatal("same-size downstream refusal changed the original, claim, ACK, or resident premise")
			}
			if fixture.root.Available() == 0 && state.reservedByteCount == fixture.decodeBytes &&
				fixture.sequence.receiveQueue.GetBySequenceNumber(10) != nil &&
				fixture.receiver.ReceiveStats().ReceiveQueueDropCount == before.ReceiveQueueDropCount+uint64(attempt+1) {
				stalledAttempts++
			}
			stalled = attempt == 2 && stalledAttempts == 3
		}
		if stalled {
			if !compete {
				t.Fatal("uncontended downstream credit failed to progress")
			}
			t.Logf("[rx-provider-pressure-stalled] retries=3 decode=%d downstream=%d reclaimable_tail=%d root_full=true cumulative=1 resident_alive=true", fixture.decodeBytes, fixture.deliveryBytes-fixture.decodeBytes, fixture.deliveryBytes)
			// A labeled fixture-validity counterfactual: the failure above is
			// retained even though returning the stolen surplus repairs it.
			fixture.releaseForeign(surplus)
			fixture.retryHead()
		}
		fixture.proveNatCustody()
		fixture.drain()
		fixture.close()
		if stalled {
			t.Error("RX_PROVIDER_SAME_SIZE_RETRY_STALLED: three unchanged source retries could not prepay downstream custody while a later uncommitted owner remained reclaimable")
		}
	})
}

func TestReceiveMemoryProviderSameSizeRetryUsesReleasedDownstreamCapacity(t *testing.T) {
	testReceiveMemoryProviderSameSizeRetry(t, false)
}

func TestReceiveMemoryProviderSameSizeRetryReclaimsDownstreamCredit(t *testing.T) {
	testReceiveMemoryProviderSameSizeRetry(t, true)
}
