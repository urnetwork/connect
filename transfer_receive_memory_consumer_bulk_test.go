// Consumer-only byte accounting across an actual signed eight-frame sender.
// These synthetic bounds reproduce a full RX leaf with an idle parent; they
// do not import SDK policy or assert that an aggregate Fast report identifies
// the same retained owners. Every transition is a synctest wait boundary.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const receiveConsumerBulkLeaf = ByteCount(2181038)
const receiveConsumerBulkTails = 175

// The census accounts each live physical claim once. Prefix and retry credits
// are unused paid capacity; ingress loans are already part of the live owner.
// Repair is the held-head repair escrow that every selective lease has
// reserved since 00d4f62b; a decoded repair copy is a live ingress owner.
type receiveConsumerBulkCensus struct {
	rx, recovery, root, parentSpare, packQuota               ByteCount
	committed, tentative, buffered, callback, ingress        ByteCount
	prefix, prefixBorrowed, escrow, escrowBorrowed, repair   ByteCount
	held, committedItems, owners, callbackPackets            int
	next, boundary, pressureNumber                           uint64
	promised, pressureActive, pressurePending, pressureReady bool
}

// Routes keep original wire custody until explicitly delivered. The callback
// consumes borrowed FromProvider packets synchronously and retains no bytes.
type receiveConsumerBulkFixture struct {
	*windowRoundFixture
	root, rx, recovery                            *TransferMemoryBudget
	sequence                                      *ReceiveSequence
	originals                                     []*windowRoundFrame
	first                                         uint64
	deliveredPackets, ackedPacks, callbackPackets int
	packetCounts                                  map[uint32]int
	closed                                        bool
	recoveryClaims, recoveryBuffered              int
	recoveryPhysical, recoveryRx, recoveryPrefix  ByteCount
	recoveryCallback                              bool
}

// Attribution matters even when already-paid RX bytes exceed the new owner.
// Both real worker stages and the deliberate missing-claim control use this
// predicate; a shared-root aggregate alone cannot prove this owner's custody.
type receiveConsumerRecoveryOwnerState struct {
	physical, rx, child, prefix ByteCount
	paid                        bool
}

func receiveConsumerRecoveryState(root, rx, recovery *TransferMemoryBudget, sequence *ReceiveSequence,
	owner *decodedPackOwner) receiveConsumerRecoveryOwnerState {
	state := receiveConsumerRecoveryOwnerState{}
	if sequence == nil || owner == nil || owner.receiveItem.memoryBudget != recovery || !owner.receivePack.receiveMemoryOwned {
		return state
	}
	stats := root.StatsWithDescendants(rx, recovery)
	view := owner.receivePack
	view.receiveMemoryOwned = false
	state.physical = view.receiveQueueByteCount()
	state.rx, state.child = stats[1].UsedByteCount, stats[2].UsedByteCount
	root.admissionLock.Lock()
	state.prefix = sequence.holdPromise.reservedByteCount
	loan := owner.receivePack.ingressLoanByteCount
	pressureOwner := sequence.memoryPressure.owner == owner || sequence.memoryPressure.reclaimOwner == owner
	root.admissionLock.Unlock()
	state.paid = state.physical == owner.receiveItem.QueueByteCount() && state.child == state.physical &&
		stats[0].UsedByteCount == state.rx+state.child && stats[0].UsedByteCount <= stats[0].TotalByteCount &&
		loan == 0 && !pressureOwner
	return state
}

// Construct all policy before clients start. Real OOB authority signs the
// opening contract; known H1 framing uses the unchanged 16 KiB/eight-frame
// defaults and ordinary compact recovery. No socket or private receipt is used.
func newReceiveConsumerBulkFixture(t *testing.T) *receiveConsumerBulkFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	root := NewTransferMemoryBudget(10 * receiveConsumerBulkLeaf)
	rx := NewTransferMemoryBudgetWithParent(receiveConsumerBulkLeaf, root)
	recovery := NewTransferMemoryBudgetWithParent(receiveConsumerBulkLeaf, root)
	self := &receiveConsumerBulkFixture{root: root, rx: rx, recovery: recovery, packetCounts: map[uint32]int{},
		windowRoundFixture: &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
			senderOut: make(Route, 256), senderIn: make(Route, 256),
			receiverOut: make(Route, 256), receiverIn: make(Route, 256)}}
	settings := func(receiver bool) *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = NewNoopLogger()
		value.ControlPingTimeout = 0
		value.EncryptionSettings.Mode = EncryptionModeOff
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024 * 1024
		value.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = 256 * 1024 * 1024
		value.ContractManagerSettings.StandardContractTransferByteCount = 256 * 1024 * 1024
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.SequenceBufferSize = 0
		value.SendBufferSettings.ProtocolVersion = 2
		value.SendBufferSettings.ResendQueueMaxByteCount = 4 * 1024 * 1024
		value.SendBufferSettings.CompactContractHead = true
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveQueueBudget = nil
		if receiver {
			receive := value.ReceiveBufferSettings
			receive.ReceiveQueueBudget = rx
			receive.ReceiveHeadRecoveryBudget = recovery
			receive.ReceiveQueueRetainedByteAccounting = true
			receive.ReceiveQueueMinByteCount = 0
			receive.ReceiveQueueMaxByteCount = 2796202
			receive.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			receive.AdvertiseReceiveWindow = true
			receive.AdvertisedReceiveWindowMaxByteCount = receiveConsumerBulkLeaf
			receive.EvictionNotice = true
			receive.PackQueueBudget = nil
			receive.PackQueueRetainedByteAccounting = false
			receive.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
				if owner == nil || owner.receiveItem.memoryBudget != recovery {
					return
				}
				state := receiveConsumerRecoveryState(root, rx, recovery, sequence, owner)
				if !state.paid {
					t.Error("consumer recovery child did not pay its actual live roots exactly once")
				}
				switch stage {
				case "pack_released":
					self.recoveryClaims++
					self.recoveryPhysical, self.recoveryRx, self.recoveryPrefix = state.physical, state.rx, state.prefix
				case "head_buffered":
					self.recoveryBuffered++
				}
			}
		}
		return value
	}
	senderId, receiverId := NewId(), NewId()
	forward := &expirationLifecycleAuthority{ctx: ctx, sourceId: senderId,
		peer: func() *Client { return self.receiver }, initialDeadline: time.Now().Add(time.Hour),
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	reverse := &expirationLifecycleAuthority{ctx: ctx, sourceId: receiverId,
		peer: func() *Client { return self.sender }, initialDeadline: time.Now().Add(time.Hour),
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	self.sender = NewClient(ctx, senderId, forward, settings(false))
	self.receiver = NewClient(ctx, receiverId, reverse, settings(true))
	self.sender.RouteManager().UpdateTransportWithProperties(newPrivateH1PeerGateway(receiverId, TransportTypeH1),
		[]Route{self.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	self.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.senderIn})
	self.receiver.RouteManager().UpdateTransport(newPrivateH1PeerGateway(senderId, TransportTypeUnknown), []Route{self.receiverOut})
	self.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.receiverIn})
	for _, client := range []*Client{self.sender, self.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	self.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		if self.recovery.UsedByteCount() != 0 {
			self.recoveryCallback = true
		}
		self.callbackPackets = len(frames)
		defer func() { self.callbackPackets = 0 }()
		for _, frame := range frames {
			switch frame.MessageType {
			case protocol.MessageType_TestSimpleMessage:
				self.deliveredCount++
			case protocol.MessageType_IpIpPacketFromProvider:
				packet, err := ipPacketFromProviderBytes(frame)
				if err != nil {
					t.Error("consumer callback could not borrow raw packet")
					continue
				}
				var path IpPath
				payload, err := parseIpPathWithPayloadBorrowed(packet, &path)
				if err != nil || path.Protocol != IpProtocolTcp || len(payload) == 0 {
					t.Error("consumer callback received a non-data TCP packet")
				}
				self.packetCounts[path.SequenceNumber]++
				if self.packetCounts[path.SequenceNumber] != 1 {
					t.Error("consumer callback duplicated an original packet")
				}
				self.deliveredPackets++
			default:
				t.Error("consumer data callback received an unexpected frame")
			}
		}
	})
	t.Cleanup(self.close)
	warm := pressureContractData(t, self.windowRoundFixture)
	self.forward(warm, self.receiverIn)
	self.acknowledge()
	self.sequence = self.receiveSequence()
	self.first = warm.pack.SequenceNumber + 1
	if self.sequence.receiveContract == nil || !self.sequence.receiveContract.verifiedReceiveProofSet ||
		self.sequence.nextSequenceNumber != self.first || self.rx.UsedByteCount() != 0 ||
		self.deliveredCount != 1 || self.ackedCount != 1 {
		t.Fatal("consumer bulk opening did not establish signed plaintext authority with zero retained RX")
	}
	if advertised := self.sequence.receiveWindowAdvertisement(); advertised != uint64(receiveConsumerBulkLeaf) {
		t.Fatal("consumer bulk changed the independently fixed advertised window")
	}
	return self
}

// The last four packets may be short. Both shapes remain eight valid TCP data
// frames; only their actual allocator roots and wire carrier class differ.
// No test edits a wire identity, source original, receiver floor or timer.
func (self *receiveConsumerBulkFixture) writePackets(shortPacketBytes int) *windowRoundFrame {
	self.t.Helper()
	number := self.first + uint64(len(self.originals))
	frames := make([]*protocol.Frame, 8)
	path := reliableDeliveryTestPath(4)
	for index := range frames {
		packetBytes := 1280
		if index >= 4 {
			packetBytes = shortPacketBytes
		}
		payload := bytes.Repeat([]byte{byte(number + uint64(index))}, packetBytes-40)
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, uint32(100+number*10240+uint64(index*1280)), payload))
		if len(packet) != packetBytes {
			MessagePoolReturn(packet)
			self.t.Fatal("consumer packet geometry changed")
		}
		frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	}
	if !self.sender.SendMultiWithTimeout(frames, self.receiver.ClientId(), func(err error) {
		if err == nil {
			self.ackedPacks++
		}
	}, time.Second) {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		self.t.Fatal("consumer bulk public batch send refused")
	}
	original := self.takePack(number)
	if original.pack.Nack || len(original.pack.Frames) != 8 || MessageByteCount(original.pack.Frames) != ByteCount(4*(1280+shortPacketBytes)) {
		self.t.Fatal("known H1 route did not emit the intended reliable eight-frame original")
	}
	self.originals = append(self.originals, original)
	return original
}

// Read only after all workers quiesce. Distinct budget locking does not stand
// in for worker ownership: synctest is the ordering barrier for queue fields.
func (self *receiveConsumerBulkFixture) census() receiveConsumerBulkCensus {
	self.t.Helper()
	synctest.Wait()
	sequence := self.sequence
	result := receiveConsumerBulkCensus{rx: self.rx.UsedByteCount(), root: self.root.UsedByteCount(),
		recovery: self.recovery.UsedByteCount(), parentSpare: self.root.Available(), next: sequence.nextSequenceNumber, callbackPackets: self.callbackPackets}
	held := map[*decodedPackOwner]bool{}
	for _, item := range sequence.receiveQueue.UnorderedItems(nil) {
		result.held++
		held[item.decodedOwner] = true
		if item.committed {
			result.committed += item.QueueByteCount()
			result.committedItems++
		} else {
			result.tentative += item.QueueByteCount()
		}
	}
	buffered := map[*decodedPackOwner]bool{}
	for _, item := range sequence.deliverItems {
		buffered[item.decodedOwner] = true
	}
	root := self.rx.admissionRoot()
	root.admissionLock.Lock()
	promise, pressure := &sequence.holdPromise, &sequence.memoryPressure
	result.promised, result.boundary, result.prefix = promise.set, promise.boundary, promise.reservedByteCount
	result.prefixBorrowed = promise.ingressLoanByteCount
	result.pressureActive, result.pressurePending, result.pressureReady = pressure.activeSet, pressure.pendingSet, pressure.ready
	result.escrow, result.escrowBorrowed, result.pressureNumber = pressure.reservedByteCount, pressure.borrowedByteCount, pressure.active.sequenceNumber
	result.repair = sequence.heldHeadRepair.reservedByteCount
	for pack := promise.ingressHead; pack != nil; pack = pack.ingressNext {
		owner := pack.decodedOwner
		result.owners++
		if owner == nil || owner.receiveItem.memoryBudget != self.rx && owner.receiveItem.memoryBudget != self.recovery {
			root.admissionLock.Unlock()
			self.t.Fatal("consumer census found an unbacked live owner")
		}
		if held[owner] {
			continue
		}
		if buffered[owner] {
			result.buffered += owner.receiveItem.QueueByteCount()
		} else if self.callbackPackets != 0 {
			result.callback += owner.receiveItem.QueueByteCount()
		} else {
			result.ingress += owner.receiveItem.QueueByteCount()
		}
	}
	root.admissionLock.Unlock()
	if budget := sequence.receiveBufferSettings.PackQueueBudget; budget != nil {
		result.packQuota = budget.Stats().QuotaByteCount
	}
	classified := result.committed + result.tentative + result.buffered + result.callback + result.ingress + result.prefix + result.escrow + result.repair
	if result.root != classified || result.root != result.rx+result.recovery || result.parentSpare <= 0 || result.packQuota != 0 ||
		self.rx.TotalByteCount() != receiveConsumerBulkLeaf || self.root.TotalByteCount() != 10*receiveConsumerBulkLeaf ||
		self.recovery.TotalByteCount() != receiveConsumerBulkLeaf || result.recovery > receiveConsumerBulkLeaf ||
		result.rx > receiveConsumerBulkLeaf || result.root > 10*receiveConsumerBulkLeaf ||
		sequence.receiveWindowAdvertisement() != uint64(receiveConsumerBulkLeaf) ||
		sequence.receiveBufferSettings.ReceiveQueueMaxByteCount != 2796202 ||
		self.receiver.ReceiveStats().ReceiveDecodeReservedByteCount != 0 || sequence.deliveryQueue != nil {
		self.t.Fatalf("consumer physical census did not conserve exact owners and unused credits: %+v classified=%d", result, classified)
	}
	return result
}

// Only the source's normal retry can reuse its original identity. Other writes
// remain captured, so they cannot accidentally close the selected gap first.
func (self *receiveConsumerBulkFixture) retry(original *windowRoundFrame) *windowRoundFrame {
	self.t.Helper()
	for _, held := range self.heldFrames {
		if held != original && held.bytes != nil && held.pack != nil && bytes.Equal(held.pack.MessageId, original.pack.MessageId) {
			return held
		}
	}
	limit, cancel := context.WithTimeout(self.ctx, self.sender.settings.SendBufferSettings.AckTimeout)
	defer cancel()
	for {
		select {
		case wire := <-self.senderOut:
			frame := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(self.t, wire)}
			self.heldFrames = append(self.heldFrames, frame)
			if bytes.Equal(frame.pack.MessageId, original.pack.MessageId) {
				if !bytes.Equal(frame.pack.SequenceId, original.pack.SequenceId) || frame.pack.SequenceNumber != original.pack.SequenceNumber || frame.pack.Nack ||
					len(frame.pack.Frames) != len(original.pack.Frames) || MessageByteCount(frame.pack.Frames) != MessageByteCount(original.pack.Frames) {
					self.t.Fatal("natural consumer retry changed reliable source identity or payload size")
				}
				return frame
			}
		case <-limit.Done():
			self.t.Fatal("natural consumer source retry did not reach its route before the unchanged ACK deadline")
		}
	}
}

// Return every route share after joining the real clients. Failure is reported
// only after this explicit cleanup, preserving the stalled state separately.
func (self *receiveConsumerBulkFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, client := range []*Client{self.sender, self.receiver} {
		if err := client.CloseAndWait(ctx); err != nil {
			self.t.Errorf("consumer bulk client join: %v", err)
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
	for _, budget := range []*TransferMemoryBudget{self.rx, self.recovery, self.root} {
		state := budget.Stats()
		if state.UsedByteCount != 0 || state.QuotaByteCount != 0 || state.ReservedByteCount != state.ReleasedByteCount {
			self.t.Errorf("consumer bulk cleanup retained a physical claim: %+v", state)
		}
	}
	if self.sequence != nil && (self.sequence.holdPromise.ingressHead != nil || self.sequence.holdPromise.reservedByteCount != 0 ||
		self.sequence.memoryPressure.activeSet || self.sequence.memoryPressure.pendingSet || self.sequence.memoryPressure.reservedByteCount != 0) {
		self.t.Error("consumer bulk cleanup retained a sequence credit or owner")
	}
	self.t.Log("[rx-consumer-bulk-cleanup] clients_joined=true routes_returned=true rx=0 root=0")
}

// The exact-next head used to reach direct delivery after decode. Its larger
// physical cost can exceed a promise estimated from smaller tails; unlike an
// earlier middle-gap arrival, it never enters the old withdrawal branch.
// Recovery must not admit an unpaid owner, falsely ACK, or close the resident.
func testReceiveConsumerBulkGap(t *testing.T, tailShortPacketBytes int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceiveConsumerBulkFixture(t)
		head := fixture.writePackets(1280)
		for range receiveConsumerBulkTails {
			fixture.writePackets(tailShortPacketBytes)
		}
		path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
		headScan, headStatus := scanReceiveMemoryPack(head.bytes, path)
		tailScan, tailStatus := scanReceiveMemoryPack(fixture.originals[1].bytes, path)
		if headStatus != receiveMemoryPackScanReady || tailStatus != receiveMemoryPackScanReady || !headScan.loanSafe || !tailScan.loanSafe ||
			headScan.hasEncryptedControl || tailScan.hasEncryptedControl || headScan.frameCount != 8 || tailScan.frameCount != 8 {
			t.Fatal("consumer original geometry is not a normal loan-eligible eight-frame Pack")
		}
		for _, tail := range fixture.originals[1:] {
			fixture.forward(tail, fixture.receiverIn)
			fixture.acknowledge()
		}
		before := fixture.census()
		if before.next != fixture.first || before.held == 0 || before.owners != before.held ||
			before.ingress != 0 || before.buffered != 0 || before.callback != 0 || before.escrow != 0 ||
			before.prefixBorrowed != 0 || before.escrowBorrowed != 0 || fixture.ackedPacks != 0 || fixture.deliveredPackets != 0 ||
			before.committedItems != before.held || before.tentative != 0 || !before.promised ||
			before.prefix != tailScan.retainedByteCount || before.pressureActive || before.pressurePending || before.recovery != 0 ||
			before.repair != fixture.sequence.receiveBufferSettings.receiveHeldHeadRepairByteCount(false) {
			t.Fatalf("consumer bulk did not retain owned tails behind the withheld head: %+v", before)
		}
		free := fixture.rx.TotalByteCount() - before.rx
		if headScan.retainedByteCount <= free {
			t.Fatal("consumer bulk did not force the head to need paid credit or tentative recovery")
		}
		if tailShortPacketBytes < 1280 && headScan.retainedByteCount <= tailScan.retainedByteCount {
			t.Fatal("mixed packet shape does not make the actual head more costly than a tail")
		}
		if tailShortPacketBytes < 1280 && headScan.retainedByteCount <= before.prefix+free {
			t.Fatal("mixed packet shape did not cross the actual paid RX predecessor promise")
		}
		t.Logf("[rx-consumer-bulk-before] tail_short_packet=%d head_cost=%d tail_cost=%d free=%d census=%+v", tailShortPacketBytes,
			headScan.retainedByteCount, tailScan.retainedByteCount, free, before)
		headStarted := time.Now()
		fixture.forward(head, fixture.receiverIn)
		fixture.acknowledge()
		retries := 0
		for ; fixture.sequence.nextSequenceNumber == fixture.first && retries < 3; retries++ {
			retry := fixture.retry(head)
			scan, status := scanReceiveMemoryPack(retry.bytes, path)
			if status != receiveMemoryPackScanReady || !scan.loanSafe {
				t.Fatal("source retry lost canonical loan eligibility")
			}
			fixture.forward(retry, fixture.receiverIn)
			fixture.acknowledge()
			t.Logf("[rx-consumer-bulk-retry] attempt=%d cost=%d census=%+v", retries+1, scan.retainedByteCount, fixture.census())
		}
		headDelay := time.Since(headStarted)
		t.Logf("[rx-consumer-head-delay] simulated_elapsed_ns=%d natural_retries=%d", headDelay.Nanoseconds(), retries)
		stalled := fixture.sequence.nextSequenceNumber == fixture.first
		if stalled {
			state := fixture.census()
			stats := fixture.receiver.ReceiveStats()
			if headScan.retainedByteCount <= before.prefix+free || before.committedItems == 0 || before.tentative != 0 ||
				!before.promised || before.prefix == 0 || fixture.sequence.ctx.Err() != nil || fixture.deliveredPackets != 0 || fixture.ackedPacks != 0 ||
				state.committed != before.committed || state.prefix != before.prefix || state.ingress != 0 ||
				state.callback != 0 || stats.ReceiveQueueCommitShortfallCount != 0 || stats.ReceiveQueueEvictionCount != 0 ||
				stats.ReceiveQueueDropCount != 0 || stats.PackHandoffDropCount < 4 {
				t.Fatalf("consumer larger-head stall changed ownership, ACK truth, or generation: %+v stats=%+v", state, stats)
			}
		} else {
			if retries != 0 || headDelay != 0 {
				t.Error("paid consumer head waited for a natural retry or timer")
			}
			for _, original := range fixture.originals {
				if original.pack.SequenceNumber < fixture.sequence.nextSequenceNumber {
					continue
				}
				fixture.forward(fixture.retry(original), fixture.receiverIn)
				fixture.acknowledge()
			}
			final := fixture.census()
			finalStats := fixture.receiver.ReceiveStats()
			if fixture.sequence.nextSequenceNumber != fixture.first+uint64(len(fixture.originals)) ||
				fixture.deliveredPackets != 8*len(fixture.originals) || fixture.ackedPacks != len(fixture.originals) ||
				final.rx != 0 || final.held != 0 || final.prefix != 0 || final.escrow != 0 || final.repair != 0 || fixture.sequence.ctx.Err() != nil ||
				finalStats.ReceiveQueueEvictionCount != 0 || finalStats.ReceiveQueueCommitShortfallCount != 0 || finalStats.ReceiveQueueExitWithdrawCount != 0 {
				t.Fatalf("consumer prefix did not recover every original exactly once: %+v delivered=%d acked=%d", final, fixture.deliveredPackets, fixture.ackedPacks)
			}
			if tailShortPacketBytes < 1280 {
				if fixture.recoveryClaims != 1 || fixture.recoveryBuffered != 1 || !fixture.recoveryCallback ||
					fixture.recoveryPhysical != headScan.retainedByteCount || fixture.recoveryRx != before.rx ||
					fixture.recoveryPrefix != before.prefix || retries != 0 || finalStats.ReceiveQueueTentativeEvictionCount != 0 {
					t.Fatal("larger consumer head did not use one fully paid child without changing committed RX custody")
				}
			} else if fixture.recoveryClaims != 0 || fixture.recovery.Stats().ReservedByteCount != 0 {
				t.Fatal("same-size promised head bypassed its ordinary RX loan")
			}
			t.Logf("[rx-consumer-recovery-child] claims=%d physical=%d rx_at_decode=%d prefix_at_decode=%d root_at_decode=%d child_final=%d",
				fixture.recoveryClaims, fixture.recoveryPhysical, fixture.recoveryRx, fixture.recoveryPrefix,
				fixture.recoveryRx+fixture.recoveryPhysical, final.recovery)
			t.Log("[rx-consumer-bulk-progress] all_originals_delivered=true cumulative_ack=true rx=0 root=0")
		}
		fixture.close()
		if stalled {
			t.Error("RX_CONSUMER_LARGER_HEAD_STALLED: physical predecode refused the exact-next original whose cost exceeded the committed-prefix estimate")
		}
	})
}

// Uniform bulk recovers under the same exact caps, preserving every original.
func TestReceiveMemoryConsumerBulkSameSizeHeadProgress(t *testing.T) {
	testReceiveConsumerBulkGap(t, 1280)
}

// A legal eight-frame head can cross the retained-tail estimate while the
// advertised encoded window and shared physical root both have spare capacity.
func TestReceiveMemoryConsumerBulkLargerHeadProgress(t *testing.T) {
	testReceiveConsumerBulkGap(t, 256)
}

// Preserve an earlier paid commitment at a quiescent worker boundary. Its
// actual SACK reaches the sender before a later arrival recomputes the prefix;
// the original must retain both its owner and its promise.
func TestReceiveMemoryConsumerBulkPreservesExistingCommitment(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceiveConsumerBulkFixture(t)
		head := fixture.writePackets(1280)
		older := fixture.writePackets(1280)
		later := fixture.writePackets(1280)
		fixture.forward(older, fixture.receiverIn)
		sequence := fixture.sequence
		held := sequence.receiveQueue.GetBySequenceNumber(older.pack.SequenceNumber)
		if held == nil || held.decodedOwner == nil || held.memoryBudget != fixture.rx {
			t.Fatal("existing-commitment control lacks its real retained owner")
		}
		owner, cost := held.decodedOwner, held.QueueByteCount()
		if !held.committed {
			if !sequence.reserveHoldPromise(cost) {
				t.Fatal("existing-commitment control could not fund its predecessor")
			}
			sequence.commitHeldItem(held)
			sequence.settleHoldPromise(true, held.sequenceNumber, cost)
		}
		fixture.acknowledge()
		sent := fixture.windowRoundFixture.sequence().resendQueue.GetByMessageId(held.messageId)
		before := fixture.census()
		if sent == nil || !sent.selectiveAcked || before.committedItems != 1 || before.committed != cost ||
			before.prefix != cost || before.boundary != held.sequenceNumber || fixture.ackedPacks != 0 ||
			before.repair != sequence.receiveBufferSettings.receiveHeldHeadRepairByteCount(false) {
			t.Fatal("existing-commitment control did not publish a genuine paid selective acknowledgement")
		}
		fixture.forward(later, fixture.receiverIn)
		fixture.acknowledge()
		after := fixture.census()
		if current := sequence.receiveQueue.GetBySequenceNumber(held.sequenceNumber); current != held ||
			!current.committed || current.decodedOwner != owner || current.memoryBudget != fixture.rx ||
			current.QueueByteCount() != cost || after.prefix < cost || after.boundary < held.sequenceNumber ||
			fixture.ackedPacks != 0 || !sent.selectiveAcked || sequence.ctx.Err() != nil {
			t.Fatal("recomputing the prefix revoked a prior owner, SACK or paid predecessor promise")
		}
		fixture.forward(head, fixture.receiverIn)
		fixture.acknowledge()
		final := fixture.census()
		stats := fixture.receiver.ReceiveStats()
		if fixture.deliveredPackets != 24 || fixture.ackedPacks != 3 || sequence.nextSequenceNumber != fixture.first+3 ||
			final.rx != 0 || final.held != 0 || final.prefix != 0 || final.escrow != 0 || final.repair != 0 ||
			stats.ReceiveQueueEvictionCount != 0 || stats.ReceiveQueueCommitShortfallCount != 0 || stats.ReceiveQueueExitWithdrawCount != 0 {
			t.Fatal("existing commitment did not drain its three real originals without withdrawal")
		}
		fixture.close()
		t.Log("[rx-consumer-existing-commitment] genuine_sack=true owner_preserved=true all_originals_delivered=true root=0")
	})
}
