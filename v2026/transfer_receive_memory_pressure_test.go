package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

const receivePressureTestCost = ByteCount(5120)

type receivePressureTestFixture struct {
	*windowRoundFixture
	root, rx  *TransferMemoryBudget
	sequence  *ReceiveSequence
	originals map[uint64]*windowRoundFrame
}

// Real Client ingress, authorized plaintext/no-contract route, and precisely
// the historical two-cell geometry. It changes no sender timer or budget.
func newReceivePressureTestFixture(t *testing.T) *receivePressureTestFixture {
	t.Helper()
	root := NewTransferMemoryBudget(2 * receivePressureTestCost)
	rx := NewTransferMemoryBudgetWithParent(2*receivePressureTestCost, root)
	fixture := &receivePressureTestFixture{root: root, rx: rx, originals: make(map[uint64]*windowRoundFrame)}
	t.Cleanup(func() {
		for _, budget := range []*TransferMemoryBudget{root, rx} {
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || stats.QuotaByteCount != 0 {
				t.Errorf("pressure fixture leaked a physical claim or quota: %+v", stats)
			}
		}
		if sequence := fixture.sequence; sequence != nil && (sequence.memoryPressure.activeSet ||
			sequence.memoryPressure.pendingSet || sequence.memoryPressure.reservedByteCount != 0 ||
			sequence.memoryPressure.owner != nil || sequence.memoryPressure.reclaimOwner != nil || sequence.holdPromise.ingressHead != nil) {
			t.Error("pressure fixture kept a retired sequence association")
		}
	})
	fixture.windowRoundFixture = newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ProtocolVersion = 2
		settings.ResendQueueMaxByteCount = 64 * 1024
	}, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueBudget = rx
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveQueueMinByteCount = 0
		settings.ReceiveQueueMaxByteCount = 2567
		settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		settings.AdvertiseReceiveWindow = false
		settings.PackQueueBudget = nil
		settings.PackQueueRetainedByteAccounting = false
	}, TransportTypeH1)
	warm := fixture.write(1280)
	fixture.forward(warm, fixture.receiverIn)
	fixture.acknowledge()
	if fixture.deliveredCount != 1 || fixture.ackedCount != 1 || root.UsedByteCount() != 0 {
		t.Fatal("warm prefix did not retire")
	}
	for number := uint64(1); number <= 11; number++ {
		original := fixture.write(1280)
		if original.pack.SequenceNumber != number || original.pack.Nack ||
			!bytes.Equal(original.pack.SequenceId, warm.pack.SequenceId) ||
			MessageByteCount(original.pack.Frames) != 1283 || MessagePoolRootByteCount(original.bytes) != 2048 {
			t.Fatal("source changed the real two-cell geometry or sequence")
		}
		fixture.originals[number] = original
	}
	fixture.forward(fixture.originals[10], fixture.receiverIn)
	fixture.forward(fixture.originals[11], fixture.receiverIn)
	fixture.sequence = fixture.receiveSequence()
	sequence := fixture.sequence
	if root.UsedByteCount() != 2*receivePressureTestCost || rx.UsedByteCount() != 2*receivePressureTestCost ||
		sequence.nextSequenceNumber != 1 || sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 ||
		len(fixture.receiverOut) != 0 || sequence.memoryPressure.activeSet {
		t.Fatal("two tentative owners did not saturate the exact RX/root without feedback")
	}
	for _, number := range []uint64{10, 11} {
		item := sequence.receiveQueue.GetBySequenceNumber(number)
		if item == nil || item.committed || !item.ack || item.QueueByteCount() != receivePressureTestCost || item.memoryBudget != rx {
			t.Fatal("tail is not one uncommitted paid owner")
		}
	}
	return fixture
}

// A bounded blocking read lets synctest advance the unchanged source timer.
// Every other emitted original remains owned by the fixture and cannot repair
// the gap first. No force-resend or sender-owned mutable field is used.
func (self *windowRoundFixture) naturalRetry(original *windowRoundFrame) *windowRoundFrame {
	self.t.Helper()
	for _, held := range self.heldFrames {
		if held != original && held.bytes != nil && held.pack != nil &&
			bytes.Equal(held.pack.SequenceId, original.pack.SequenceId) && bytes.Equal(held.pack.MessageId, original.pack.MessageId) {
			return held
		}
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case wire := <-self.senderOut:
			frame := &windowRoundFrame{bytes: wire}
			self.heldFrames = append(self.heldFrames, frame)
			var decoded protocol.TransferFrame
			if err := ProtoUnmarshal(wire, &decoded); err != nil {
				self.t.Fatal(err)
			}
			frame.pack, frame.ack = decoded.Pack, decoded.Ack
			if frame.pack == nil {
				self.t.Fatal("natural retry route did not carry a Pack")
			}
			if bytes.Equal(frame.pack.MessageId, original.pack.MessageId) {
				if !bytes.Equal(frame.pack.SequenceId, original.pack.SequenceId) || frame.pack.SequenceNumber != original.pack.SequenceNumber || frame.pack.Nack {
					self.t.Fatal("natural retry changed reliable identity")
				}
				synctest.Wait()
				return frame
			}
		case <-deadline.C:
			self.t.Fatalf("unchanged source timer did not retry original %d", original.pack.SequenceNumber)
		}
	}
}

func (self *receivePressureTestFixture) assertEscrow(number uint64, tailNumber uint64) {
	self.t.Helper()
	synctest.Wait()
	state := self.sequence.memoryPressure
	id, err := IdFromBytes(self.originals[number].pack.MessageId)
	if err != nil {
		self.t.Fatal(err)
	}
	if !state.activeSet || !state.ready || state.active.messageId != id || state.active.sequenceNumber != number ||
		state.reservedByteCount != receivePressureTestCost || state.owner != nil || state.borrowedByteCount != 0 || state.reclaimOwner != nil ||
		self.root.UsedByteCount() != 2*receivePressureTestCost || self.rx.UsedByteCount() != 2*receivePressureTestCost ||
		self.sequence.receiveQueue.Len() != 1 || self.sequence.receiveQueue.GetBySequenceNumber(tailNumber) == nil ||
		len(self.receiverOut) != 0 || self.deliveredCount != 1 || self.ackedCount != 1 || self.sequence.ctx.Err() != nil {
		self.t.Fatalf("worker failed to preserve exact tentative return credit: %+v", state)
	}
	foreign := NewTransferMemoryBudgetWithParent(receivePressureTestCost, self.root)
	if foreign.TryReserve(1) {
		foreign.Release(1)
		self.t.Fatal("another flow stole reserved retry capacity")
	}
}

// The first refusal remains visible. Its actual source retry consumes escrow;
// then an even earlier head makes progress and the whole original prefix is
// delivered/cumulatively acknowledged, with every owner returned.
func TestReceiveMemoryTentativePressureRetryCompletesPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceivePressureTestFixture(t)
		fixture.forward(fixture.originals[8], fixture.receiverIn)
		fixture.assertEscrow(8, 10)
		stats := fixture.receiver.ReceiveStats()
		if stats.PackHandoffDropCount != 1 || stats.ReceiveQueueTentativeEvictionCount != 1 ||
			stats.ReceiveQueueEvictionCount != 0 || stats.ReceiveQueueCommitCount != 0 {
			t.Fatal("initial refusal or tentative-only eviction was hidden")
		}
		fixture.forward(fixture.naturalRetry(fixture.originals[8]), fixture.receiverIn)
		if fixture.sequence.memoryPressure.activeSet || fixture.sequence.receiveQueue.GetBySequenceNumber(8) == nil ||
			fixture.sequence.receiveQueue.GetBySequenceNumber(10) == nil || fixture.root.UsedByteCount() != 2*receivePressureTestCost ||
			len(fixture.receiverOut) != 0 {
			t.Fatal("exact natural retry did not consume credit once and remain tentative")
		}
		fixture.forward(fixture.originals[1], fixture.receiverIn)
		fixture.assertEscrow(1, 8)
		fixture.forward(fixture.naturalRetry(fixture.originals[1]), fixture.receiverIn)
		fixture.acknowledge()
		for number := uint64(2); number <= 11; number++ {
			if number < fixture.sequence.nextSequenceNumber {
				continue
			}
			original := fixture.originals[number]
			if original.bytes == nil {
				original = fixture.naturalRetry(original)
			}
			fixture.forward(original, fixture.receiverIn)
			fixture.acknowledge()
		}
		finalStats := fixture.receiver.ReceiveStats()
		if finalStats.ReceiveQueueEvictionCount != 0 || finalStats.ReceiveQueueCommitShortfallCount != 0 || finalStats.ReceiveQueueExitWithdrawCount != 0 ||
			fixture.deliveredCount != 12 || fixture.ackedCount != 12 || fixture.sequence.nextSequenceNumber != 12 ||
			fixture.sequence.receiveQueue.Len() != 0 || fixture.root.UsedByteCount() != 0 || fixture.rx.UsedByteCount() != 0 ||
			fixture.sequence.memoryPressure.activeSet || fixture.sequence.memoryPressure.pendingSet || fixture.sequence.ctx.Err() != nil {
			t.Fatal("ordinary retries did not complete the entire cumulative prefix with exact retirement")
		}
		if count, _ := fixture.windowRoundFixture.sequence().resendQueue.QueueSize(); count != 0 {
			t.Fatal("cumulative delivery left source originals retained")
		}
		t.Log("[rx-pressure-progress] first_attempt=refused natural_retry=true earlier_head=true delivered=12 cumulative=12 root=0 committed_evictions=0")
	})
}

// A newer request may only move toward the real head. Returning no payload
// makes retargeting free of an uncharged intermediate owner or 3C overlap.
func TestReceiveMemoryTentativePressureRetargetsUnusedCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceivePressureTestFixture(t)
		fixture.forward(fixture.originals[8], fixture.receiverIn)
		fixture.assertEscrow(8, 10)
		fixture.forward(fixture.originals[1], fixture.receiverIn)
		fixture.assertEscrow(1, 10)
		if fixture.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != 1 {
			t.Fatal("retarget unnecessarily evicted another real owner")
		}
		fixture.forward(fixture.naturalRetry(fixture.originals[1]), fixture.receiverIn)
		fixture.acknowledge()
		for number := uint64(2); number <= 11; number++ {
			if number < fixture.sequence.nextSequenceNumber {
				continue
			}
			original := fixture.originals[number]
			if original.bytes == nil {
				original = fixture.naturalRetry(original)
			}
			fixture.forward(original, fixture.receiverIn)
			fixture.acknowledge()
		}
		if fixture.deliveredCount != 12 || fixture.ackedCount != 12 || fixture.root.UsedByteCount() != 0 || fixture.sequence.memoryPressure.activeSet {
			t.Fatal("retargeted exact head did not drain the complete prefix")
		}
		t.Log("[rx-pressure-retarget] from=8 to=1 extra_evictions=0 natural_retry=true cumulative=12 root=0")
	})
}

// Core transaction control: an actual funded owner returns before adoption,
// so the same identity gets its credit back; a foreign/duplicate owner never
// spends it. Revocation during a later in-flight copy keeps that copy funded.
func TestReceiveMemoryPressureCreditReturnsUntilAdoption(t *testing.T) {
	assertMessagePoolOwnership(t)
	root, rx, buffer, sequence := receiveMemoryTestState(t, receivePressureTestCost)
	id := NewId()
	if !rx.TryReserve(receivePressureTestCost) {
		t.Fatal("escrow setup")
	}
	sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: receivePressureTestCost},
		activeSet: true, ready: true, reservedByteCount: receivePressureTestCost}
	owner := buffer.takeReceiveMemoryOwner(sequence, id, 1, receivePressureTestCost)
	if owner == nil {
		t.Fatal("exact retry could not use full-root credit")
	}
	defer func() {
		if owner != nil {
			owner.release()
		}
	}()
	if other := buffer.takeReceiveMemoryOwner(sequence, id, 1, receivePressureTestCost); other != nil {
		other.release()
		t.Fatal("duplicate spent escrow twice")
	}
	if other := buffer.takeReceiveMemoryOwner(nil, NewId(), 1, receivePressureTestCost); other != nil {
		other.release()
		t.Fatal("foreign owner stole escrow")
	}
	owner.release()
	owner = nil
	if sequence.memoryPressure.reservedByteCount != receivePressureTestCost || root.UsedByteCount() != receivePressureTestCost || sequence.memoryPressure.owner != nil {
		t.Fatal("pre-adoption return lost exact reserved identity")
	}
	owner = buffer.takeReceiveMemoryOwner(sequence, id, 1, receivePressureTestCost)
	if owner == nil {
		t.Fatal("second exact retry could not reuse returned credit")
	}
	sequence.nextSequenceNumber = 2
	sequence.publishReceiveMemoryFloor()
	if root.UsedByteCount() != receivePressureTestCost || sequence.memoryPressure.activeSet {
		t.Fatal("head advance released a live owner or kept obsolete authority")
	}
	owner.release()
	owner = nil
	if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 {
		t.Fatal("obsolete in-flight owner did not return once")
	}
	t.Log("[rx-pressure-credit] duplicate_refused=true foreign_refused=true return_restored=true obsolete_owner_paid=true root=0")
}

// Pending proposals contain no carrier, owner or queue mutation. Repeated
// pressure cannot grow the reservation or renew a sequence generation.
func TestReceiveMemoryPressureRejectsNoAckAndClosesExactCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	root, rx, _, sequence := receiveMemoryTestState(t, receivePressureTestCost)
	sequence.closeNotify = make(chan struct{}, 1)
	sequence.receiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
	scan := receiveMemoryPackScan{loanSafe: true, messageId: NewId(), sequenceNumber: 1, retainedByteCount: receivePressureTestCost, messageByteCount: 1283}
	sequence.requestReceiveMemoryPressure(scan, false)
	first := sequence.memoryPressure
	for range 8 {
		sequence.requestReceiveMemoryPressure(scan, false)
	}
	if sequence.memoryPressure != first || root.UsedByteCount() != 0 || len(sequence.closeNotify) != 1 {
		t.Fatal("duplicate pressure grew state, memory, or wakes")
	}
	sequence.memoryPressure = receiveMemoryPressure{}
	scan.nack = true
	scan.messageId = NewId()
	sequence.requestReceiveMemoryPressure(scan, false)
	if sequence.memoryPressure != (receiveMemoryPressure{}) {
		t.Fatal("NoAck created reliable pressure authority")
	}
	if !rx.TryReserve(receivePressureTestCost) {
		t.Fatal("credit setup")
	}
	sequence.memoryPressure.active = first.pending
	sequence.memoryPressure.activeSet = true
	sequence.memoryPressure.ready = true
	sequence.memoryPressure.reservedByteCount = receivePressureTestCost
	sequence.releaseHoldPromise()
	if root.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) {
		t.Fatal("close did not release the exact unused credit and metadata")
	}
	sequence.requestReceiveMemoryPressure(first.pending.asScan(), false)
	if sequence.memoryPressure != (receiveMemoryPressure{}) {
		t.Fatal("closed generation accepted stale pressure")
	}
	t.Log("[rx-pressure-close] duplicate_bounded=true noack_ignored=true closed_generation_rejected=true root=0")
}

func (self receiveMemoryPressureRequest) asScan() receiveMemoryPackScan {
	return receiveMemoryPackScan{loanSafe: true, messageId: self.messageId, sequenceNumber: self.sequenceNumber,
		retainedByteCount: self.queueByteCount, messageByteCount: self.messageByteCount}
}

// A paid decoder can still meet a full encoded handoff. The actual Pack false,
// nil branch returns all roots, restores only its exact retry credit, and does
// not cancel the resident. A subsequent admitted handoff consumes it once.
func TestReceiveMemoryPressurePackRefusalRestoresExactRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	body, id, sequenceId := receiveMemoryScanTestBody(t, 8, 1280)
	bytesOnWire := receiveMemoryScanTestWire(path, body, false)
	probe := MessagePoolCopy(bytesOnWire)
	scan, status := scanReceiveMemoryPack(probe, path)
	MessagePoolReturn(probe)
	if status != receiveMemoryPackScanReady {
		t.Fatal("wire setup")
	}
	cost, encoded := scan.retainedByteCount, ByteCount(len(bytesOnWire))
	root := NewTransferMemoryBudget(2 * cost)
	rx := NewTransferMemoryBudgetWithParent(2*cost, root)
	quota := NewTransferMemoryBudgetWithParent(encoded, root)
	t.Cleanup(func() {
		for _, budget := range []*TransferMemoryBudget{root, rx, quota} {
			s := budget.Stats()
			if s.UsedByteCount != 0 || s.QuotaByteCount != 0 || s.ReservedByteCount != s.ReleasedByteCount {
				t.Errorf("Pack-refusal cleanup leaked: %+v", s)
			}
		}
	})
	settings := DefaultReceiveBufferSettingsWithBufferSize(4)
	settings.ReceiveQueueBudget = rx
	settings.ReceiveQueueRetainedByteAccounting = true
	settings.PackQueueBudget = quota
	settings.PackQueueRetainedByteAccounting = false
	clientSettings := DefaultClientSettings()
	clientSettings.ReceiveBufferSettings = settings
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	buffer := &ReceiveBuffer{receiveBufferSettings: settings, receiveSequences: make(map[receiveSequenceId]*ReceiveSequence)}
	client := &Client{settings: clientSettings, log: NewNoopLogger(), receiveBuffer: buffer}
	sequence := newReceiveSequence(context.Background(), client, path.SourceMask(), sequenceId,
		TransferKey{EncryptionRole: sequenceTlsRoleClient.toProtobuf(), ForceStream: scan.forceStream, CompanionContract: scan.companionContract, LogicalLane: scan.logicalLane}, settings)
	defer sequence.Close()
	defer sequence.releaseHoldPromise()
	sequence.nextSequenceNumber = scan.sequenceNumber
	sequence.holdPromise.ingressFloor = scan.sequenceNumber
	buffer.receiveSequences[receiveSequenceId{Source: path.SourceMask(), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleClient, LogicalLane: scan.logicalLane}] = sequence
	if !rx.TryReserve(cost) {
		t.Fatal("escrow setup")
	}
	sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{messageId: id, sequenceNumber: scan.sequenceNumber, queueByteCount: cost}, activeSet: true, ready: true, reservedByteCount: cost}
	foreign := buffer.takeReceiveMemoryOwner(nil, NewId(), 0, cost)
	if foreign == nil {
		t.Fatal("foreign physical owner setup")
	}
	defer func() {
		if foreign != nil {
			foreign.release()
		}
	}()
	stop := receiveMemoryQuotaStop(quota, rx)
	accepted, waiter := quota.reserveReceiveQuota(encoded, stop, false)
	if !accepted || waiter != nil {
		t.Fatal("foreign paid handoff quota setup")
	}
	foreignQuota := true
	defer func() {
		if foreignQuota {
			quota.releaseReceiveQuota(encoded, stop)
		}
	}()
	for attempt := 0; attempt < 2; attempt++ {
		wire := MessagePoolCopy(bytesOnWire)
		decoded := &decodedTransferFrame{}
		ok, pressure := client.unmarshalReceiveMemoryFrame(wire, decoded, false, path, false, sequenceTlsRoleServer, false)
		if !ok || pressure {
			MessagePoolReturn(wire)
			decoded.release()
			t.Fatal("exact retry did not reach selected paid decode")
		}
		owner := decoded.detachPackOwner()
		owner.receivePack.TransferFrameBytes = wire
		owner.receivePack.MessageByteCount = scan.messageByteCount
		owner.receivePack.TransportType = TransportTypeH1
		func() {
			defer decoded.release()
			defer func() {
				if owner != nil {
					owner.receivePack.messagePoolReturn()
				}
			}()
			accepted, err := sequence.Pack(&owner.receivePack, 0)
			if err != nil || accepted != (attempt == 1) || sequence.ctx.Err() != nil {
				t.Fatalf("attempt %d accepted=%t err=%v", attempt, accepted, err)
			}
			if accepted {
				queued := <-sequence.packs
				if queued != &owner.receivePack {
					t.Fatal("handoff changed owner")
				}
				sequence.releasePackQueue(queued)
				owner.receiveItem.decodedOwner = owner
				sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
			}
			owner.receivePack.messagePoolReturn()
			owner = nil
		}()
		if attempt == 0 {
			if sequence.memoryPressure.reservedByteCount != cost || sequence.memoryPressure.owner != nil || root.UsedByteCount() != 2*cost || quota.Stats().QuotaByteCount != encoded {
				t.Fatal("safe handoff refusal lost exact escrow, foreign quota or root custody")
			}
			quota.releaseReceiveQuota(encoded, stop)
			foreignQuota = false
			foreign.release()
			foreign = nil
		} else if sequence.memoryPressure.activeSet || root.UsedByteCount() != 0 || quota.Stats().QuotaByteCount != 0 {
			t.Fatal("adopted retry did not retire exact credit and quota")
		}
	}
	t.Log("[rx-pressure-handoff] first_refused=true resident_alive=true exact_credit_restored=true next_adopted=true root=0 quota=0")
}

// Two genuine SACKed tails plus their predecessor promise fill three cells.
// A genuinely larger missing original cannot turn pressure into a withdrawal
// of either committed owner, even when EvictionNotice is enabled.
func TestReceiveMemoryPressureNeverPreemptsCommittedOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		// Three physical cells and the exact metadata-only promotion escrow.
		capacity := 3*receivePressureTestCost + decodedPackOwnerQueueByteCount
		root := NewTransferMemoryBudget(capacity)
		rx := NewTransferMemoryBudgetWithParent(capacity, root)
		t.Cleanup(func() {
			if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 {
				t.Error("committed control leaked owners or promise")
			}
		})
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ProtocolVersion = 2
			settings.ResendQueueMaxByteCount = 64 * 1024
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueBudget = rx
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueMinByteCount = 0
			settings.ReceiveQueueMaxByteCount = 4*1283 + 1
			settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			settings.AdvertiseReceiveWindow = false
			settings.EvictionNotice = true
			settings.PackQueueBudget = nil
		}, TransportTypeH1)
		warm := fixture.write(1280)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		missing := fixture.write(2100)
		tail2, tail3 := fixture.write(1280), fixture.write(1280)
		fixture.forward(tail2, fixture.receiverIn)
		fixture.forward(tail3, fixture.receiverIn)
		sequence := fixture.receiveSequence()
		for _, number := range []uint64{2, 3} {
			item := sequence.receiveQueue.GetBySequenceNumber(number)
			if item == nil || !item.committed || item.QueueByteCount() != receivePressureTestCost {
				t.Fatal("tail never acquired actual SACK custody")
			}
		}
		acks := fixture.takeAcknowledgements()
		for _, ack := range acks {
			if !ack.ack.Selective || len(ack.ack.EvictedSequenceNumbers) != 0 {
				t.Fatal("premise did not publish genuine selective acknowledgements")
			}
			fixture.forward(ack, fixture.senderIn)
		}
		if root.UsedByteCount() != capacity || sequence.holdPromise.reservedByteCount != receivePressureTestCost ||
			sequence.heldHeadRepair.reservedByteCount != decodedPackOwnerQueueByteCount {
			t.Fatal("SACK predecessor promise did not saturate three cells")
		}
		before := fixture.receiver.ReceiveStats()
		fixture.forward(missing, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		if root.UsedByteCount() != capacity || sequence.holdPromise.reservedByteCount != receivePressureTestCost ||
			sequence.receiveQueue.Len() != 2 || sequence.memoryPressure.activeSet ||
			after.ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount || after.ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount ||
			len(fixture.receiverOut) != 0 || sequence.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatal("predecode pressure pruned committed custody, spent the promise or closed a resident")
		}
		t.Log("[rx-pressure-committed] genuine_sack=2 owners_preserved=2 promise_preserved=true evictions=0 resident_alive=true")
	})
}

// Decoder size classes and downstream prepayment can differ on a retry. Both
// portions still use the one exact reservation, and a failed deficit does not
// partially spend it or release the old owner's physical claim.
func TestReceiveMemoryPressureRetryCostAndGrowthStayConserved(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = ByteCount(4096)
	root, rx, buffer, sequence := receiveMemoryTestState(t, cost)
	id := NewId()
	if !rx.TryReserve(cost) {
		t.Fatal("credit setup")
	}
	sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: cost},
		activeSet: true, ready: true, reservedByteCount: cost}
	before := root.Stats()
	if excess := buffer.takeReceiveMemoryOwner(sequence, id, 1, cost+1); excess != nil {
		excess.release()
		t.Fatal("unfunded larger retry acquired an owner")
	}
	if root.Stats() != before || sequence.memoryPressure.reservedByteCount != cost || sequence.memoryPressure.owner != nil {
		t.Fatal("failed retry deficit partially consumed credit")
	}
	owner := buffer.takeReceiveMemoryOwner(sequence, id, 1, cost/2)
	if owner == nil {
		t.Fatal("smaller exact retry failed")
	}
	defer func() {
		if owner != nil {
			owner.release()
		}
	}()
	owner.receiveItem.decodedOwner = owner
	if !owner.receiveItem.growReceiveMemory(cost/2) || owner.receiveItem.QueueByteCount() != cost ||
		sequence.memoryPressure.borrowedByteCount != cost || sequence.memoryPressure.reservedByteCount != 0 || root.Stats() != before {
		t.Fatal("same pending owner's downstream prepay could not use its remaining escrow")
	}
	if owner.receiveItem.growReceiveMemory(1) || owner.receiveItem.QueueByteCount() != cost || root.Stats() != before {
		t.Fatal("failed growth changed an existing physical claim")
	}
	owner.release()
	owner = nil
	if sequence.memoryPressure.reservedByteCount != cost || sequence.memoryPressure.borrowedByteCount != 0 || sequence.memoryPressure.owner != nil || root.Stats() != before {
		t.Fatal("decode/prepay return did not restore exact combined credit")
	}
	owner = buffer.takeReceiveMemoryOwner(sequence, id, 1, cost/2)
	if owner == nil {
		t.Fatal("smaller adoption retry failed")
	}
	owner.receiveItem.decodedOwner = owner
	sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	if root.UsedByteCount() != cost/2 || sequence.memoryPressure.activeSet {
		t.Fatal("adoption retained unused credit or released the live owner")
	}
	owner.release()
	owner = nil
	if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 {
		t.Fatal("smaller adopted owner did not return once")
	}
	t.Log("[rx-pressure-cost] larger_deficit_refused=true smaller_growth_paid=true failed_growth_unchanged=true unused_credit_returned=true root=0")
}

// Pressure credit and a committed-prefix promise are different paid portions
// of the same root. Returning an unadopted copy restores each once; closing its
// generation removes both return associations while keeping the copy funded.
func TestReceiveMemoryPressureAndPrefixLoansRemainDisjoint(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = ByteCount(4096)
	root, rx, buffer, sequence := receiveMemoryTestState(t, 3*cost)
	id := NewId()
	if !rx.TryReserve(cost) || !sequence.reserveHoldPromise(cost) {
		t.Fatal("dual credit setup")
	}
	sequence.settleHoldPromise(true, 3, cost)
	sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: cost}, activeSet: true, ready: true, reservedByteCount: cost}
	foreign := buffer.takeReceiveMemoryOwner(nil, NewId(), 9, cost)
	if foreign == nil {
		t.Fatal("competing owner setup")
	}
	defer func() {
		if foreign != nil {
			foreign.release()
		}
	}()
	before := root.Stats()
	owner := buffer.takeReceiveMemoryOwner(sequence, id, 1, cost+cost/2)
	if owner == nil {
		t.Fatal("mixed-credit original could not enter the full root")
	}
	defer func() {
		if owner != nil {
			owner.release()
		}
	}()
	owner.receiveItem.decodedOwner = owner
	if sequence.memoryPressure.borrowedByteCount != cost || owner.receivePack.ingressLoanByteCount != cost/2 ||
		!owner.receiveItem.growReceiveMemory(cost/2) || owner.receiveItem.QueueByteCount() != 2*cost ||
		owner.receivePack.ingressLoanByteCount != cost || root.Stats() != before {
		t.Fatal("mixed physical prepayment was not one conserved claim")
	}
	owner.release()
	owner = nil
	if sequence.memoryPressure.reservedByteCount != cost || sequence.memoryPressure.borrowedByteCount != 0 ||
		sequence.holdPromise.reservedByteCount != cost || sequence.holdPromise.ingressLoanByteCount != 0 || root.Stats() != before {
		t.Fatal("mixed pre-adoption return did not restore both disjoint portions once")
	}
	owner = buffer.takeReceiveMemoryOwner(sequence, id, 1, 2*cost)
	if owner == nil {
		t.Fatal("restored mixed credit could not retry")
	}
	sequence.releaseHoldPromise()
	if root.Stats() != before || sequence.memoryPressure.activeSet || owner.receivePack.ingressLoanByteCount != 0 || owner.receiveItem.QueueByteCount() != 2*cost {
		t.Fatal("generation close released a live owner or retained restoration authority")
	}
	queue := newReceiveQueue(rx, 0)
	queue.setLifetimeBudget()
	replacement := &ReceiveSequence{ctx: context.Background(), receiveQueue: queue, receiveBufferSettings: sequence.receiveBufferSettings, nextSequenceNumber: 1}
	defer replacement.releaseHoldPromise()
	if replacement.reserveHoldPromise(1) {
		t.Fatal("replacement generation stole a live old owner's claim")
	}
	owner.release()
	owner = nil
	if root.UsedByteCount() != cost || !replacement.reserveHoldPromise(2*cost) {
		t.Fatal("actual old return did not free exact capacity")
	}
	replacement.settleHoldPromise(true, 3, 2*cost)
	sequence.releaseHoldPromise()
	if replacement.holdPromise.reservedByteCount != 2*cost || root.UsedByteCount() != 3*cost {
		t.Fatal("retired generation changed replacement credit")
	}
	replacement.releaseHoldPromise()
	foreign.release()
	foreign = nil
	if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 {
		t.Fatal("dual-credit generations did not retire exactly")
	}
	t.Log("[rx-pressure-prefix] disjoint_restore=true live_close_paid=true replacement_isolated=true competing_owner_paid=true root=0")
}
