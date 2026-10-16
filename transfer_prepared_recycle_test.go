// Exact recycling tests pin physical ACK ordering, immutable envelopes and
// canceled-generation ownership without introducing a retry timer or worker.
package connect

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// The callback observes credit while the actual ACK path still owns completion.
type recyclableMemoryTestOwner struct {
	credit        *preparedSendMemory
	expected      ByteCount
	acks          int
	err           error
	recycledAtAck bool
	bytesAtAck    ByteCount
	inFlightAtAck ByteCount
}

// Supplies the exact owner instance to the serializer's existing credit seam.
func (self *recyclableMemoryTestOwner) preparedSendMemory() *preparedSendMemory {
	return self.credit
}

// Captures ordering without releasing or borrowing any part of the envelope.
func (self *recyclableMemoryTestOwner) sendAckResult(_ ByteCount, err error) {
	self.acks++
	self.err = err
	root := self.credit.budget.admissionRoot()
	root.admissionLock.Lock()
	self.bytesAtAck = self.credit.bytes
	self.inFlightAtAck = self.credit.inFlight
	self.recycledAtAck = self.credit.bytes == self.expected && self.credit.inFlight == 0
	root.admissionLock.Unlock()
}

// A real cumulative Transfer ACK returns wire permission before its callback;
// the same credit then sends again while all ordinary parent capacity is held.
func TestRecyclableSendMemoryActualAckAtFullParent(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(kib(16))
		budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueBudget = budget
			settings.ResendQueueRetainedByteAccounting = true
			settings.SequenceBufferSize = 1
		}, nil)
		fixture.forward(fixture.write(100), fixture.receiverIn)
		fixture.acknowledge()
		credit, ok := prepareRecyclableSendMemory(budget, root.TotalByteCount(), kib(1))
		if !ok {
			t.Fatal("exact recovery envelope was not admitted")
		}
		defer credit.release()
		owner := &recyclableMemoryTestOwner{credit: credit, expected: root.TotalByteCount()}
		for number := uint64(1); number <= 3; number++ {
			frame := budgetTestFrame(1000)
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), nil, time.Second,
				TransferOptions{Ack: true}, sendAckTargetOption{target: owner},
				sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true})
			if !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("cycle=%d: prepaid physical send refused: accepted=%t err=%v", number, accepted, err)
			}
			wire := fixture.takePack(number)
			if wire.pack.Nack || root.UsedByteCount() != root.TotalByteCount() || budget.Available() != 0 {
				t.Fatal("physical write lost reliable custody or lent its prepaid permission")
			}
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			if owner.acks != int(number) || owner.err != nil || !owner.recycledAtAck {
				t.Fatalf("cycle=%d: ACK preceded wire-credit recycling: callbacks=%d err=%v recycled=%t", number, owner.acks, owner.err, owner.recycledAtAck)
			}
			if root.UsedByteCount() != root.TotalByteCount() || root.TryReserve(1) {
				t.Fatal("cumulative ACK released recovery permission to an unrelated borrower")
			}
		}
		credit.release()
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, root)
	})
}

// Split chunks recycle independently; aggregate completion cannot publish
// eligibility while a later original physical owner still holds its credit.
func TestRecyclableSendMemorySplitCompletionOutOfOrder(t *testing.T) {
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(kib(16))
	budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
	credit, ok := prepareRecyclableSendMemory(budget, root.TotalByteCount(), 0)
	if !ok {
		t.Fatal("recovery envelope admission")
	}
	defer credit.release()
	owner := &recyclableMemoryTestOwner{credit: credit, expected: root.TotalByteCount()}
	completion := newSendGroupCompletion(&SendPack{ackTarget: owner}, 2)
	items := [2]*sendItem{{}, {}}
	defer func() {
		for _, item := range items {
			if item != nil {
				item.messagePoolReturn()
			}
		}
	}()
	for _, item := range items {
		item.acks.add(completion.chunkAckRecord())
		if !item.reservePreparedMemory(budget, kib(4)) {
			t.Fatal("split physical credit refused")
		}
		item.transferFrameBytes = MessagePoolGet(100)
		item.acks.firstRouteWrite(nil)
	}
	sequence := &SendSequence{}
	sequence.ackItem(items[1])
	items[1] = nil
	if owner.acks != 0 || credit.inFlight != kib(4) || credit.bytes != kib(12) || root.UsedByteCount() != kib(16) {
		t.Fatalf("partial out-of-order completion lost custody: callbacks=%d flight=%d reusable=%d root=%d", owner.acks, credit.inFlight, credit.bytes, root.UsedByteCount())
	}
	sequence.ackItem(items[0])
	items[0] = nil
	if owner.acks != 1 || owner.err != nil || !owner.recycledAtAck || root.UsedByteCount() != kib(16) {
		t.Fatal("last chunk callback did not observe the complete recycled envelope")
	}
	credit.release()
	assertRetainedBudgetBalance(t, budget)
	assertRetainedBudgetBalance(t, root)
}

// A closed generation retains its compact/capsule charge until every old
// physical flight joins; its late return cannot refill a replacement owner.
func TestRecyclableSendMemoryCancelKeepsOldFlightOwned(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, retained := range []ByteCount{0, kib(1)} {
		for _, terminalErr := range []error{context.Canceled, errors.New("synthetic terminal route failure")} {
			for _, indexed := range []bool{false, true} {
				func() {
					root := NewTransferMemoryBudget(kib(16))
					budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
					old, ok := prepareRecyclableSendMemory(budget, kib(8), retained)
					if !ok {
						t.Fatal("old envelope admission")
					}
					defer old.release()
					owner := &recyclableMemoryTestOwner{credit: old, expected: kib(8)}
					item := &sendItem{transferItem: transferItem{messageId: NewId()}}
					var sequence *SendSequence
					defer func() {
						if sequence != nil {
							sequence.releaseRetainedSendItems(terminalErr)
						} else {
							item.messagePoolReturn()
						}
					}()
					item.acks.add(sendAckRecord{target: owner})
					if !item.reservePreparedMemory(budget, kib(4)) {
						t.Fatal("old physical flight admission")
					}
					item.transferFrameBytes = MessagePoolGet(100)
					queue := newResendQueue(budget, 0)
					queue.setLifetimeBudget()
					queue.Add(item)
					sequence = &SendSequence{resendQueue: queue}
					if indexed {
						sequence.sendItems = []*sendItem{item}
					}
					old.release()
					old.release()
					if root.UsedByteCount() != kib(4)+retained || old.bytes != retained || old.inFlight != kib(4) {
						t.Fatalf("cancel released live metadata: retained=%d indexed=%t root=%d capsule=%d flight=%d", retained, indexed, root.UsedByteCount(), old.bytes, old.inFlight)
					}
					current, ok := prepareRecyclableSendMemory(budget, kib(8), kib(1))
					if !ok {
						t.Fatal("independent replacement envelope admission")
					}
					defer current.release()
					probe := &sendItem{}
					probe.acks.add(sendAckRecord{target: owner})
					if probe.reservePreparedMemory(budget, 1) {
						probe.releaseMemory()
						t.Fatal("closed generation reacquired serializer ownership")
					}
					sequence.releaseRetainedSendItems(terminalErr)
					sequence.releaseRetainedSendItems(terminalErr)
					if owner.acks != 1 || owner.err != terminalErr || owner.bytesAtAck != retained || owner.inFlightAtAck != kib(4) {
						t.Fatal("terminal callback did not precede its still-owned physical return exactly once")
					}
					if old.bytes != 0 || old.inFlight != 0 || current.bytes != kib(8) || root.UsedByteCount() != kib(8) {
						t.Fatal("late canceled-flight return changed replacement credit or leaked the old capsule")
					}
					current.release()
					assertRetainedBudgetBalance(t, budget)
					assertRetainedBudgetBalance(t, root)
				}()
			}
		}
	}
}

// Reusable custody has an explicit finite envelope, never a nil-budget or
// post-admission rewrite escape. Ordinary single-use credit is unchanged.
func TestRecyclableSendMemoryRejectsUnfundedEnvelope(t *testing.T) {
	for _, budget := range []*TransferMemoryBudget{nil, NewTransferMemoryBudget(0), NewTransferMemoryBudget(-1)} {
		if credit, ok := prepareRecyclableSendMemory(budget, kib(8), kib(1)); ok || credit != nil {
			t.Fatal("missing or disabled budget created unbounded recovery")
		}
	}
	budget := NewTransferMemoryBudget(kib(16))
	credit, ok := prepareRecyclableSendMemory(budget, kib(8), kib(1))
	if !ok {
		t.Fatal("bounded envelope admission")
	}
	defer credit.release()
	owner := &recyclableMemoryTestOwner{credit: credit}
	item := &sendItem{}
	defer item.releaseMemory()
	item.acks.add(sendAckRecord{target: owner})
	for _, bytes := range []ByteCount{-1, 0} {
		if item.reservePreparedMemory(budget, bytes) || credit.bytes != kib(8) || credit.inFlight != 0 {
			t.Fatal("nonpositive physical claim created an unaccounted live owner")
		}
	}
	if item.reservePreparedMemory(budget, kib(8)) || credit.bytes != kib(8) || credit.inFlight != 0 {
		t.Fatal("wire reservation consumed retained original/capsule permission")
	}
	credit.require(kib(8))
	if credit.growRequired() || budget.UsedByteCount() != kib(8) {
		t.Fatal("recovery silently enlarged its immutable envelope")
	}
	credit.require(ByteCount(math.MaxInt64))
	if credit.growRequired() || credit.required != ByteCount(math.MaxInt64) || budget.UsedByteCount() != kib(8) {
		t.Fatal("oversized envelope arithmetic wrapped into available permission")
	}
	if !item.reservePreparedMemory(budget, kib(4)) {
		t.Fatal("funded original flight refused")
	}
	if item.reserveFrameRewrite(kib(8), 1, false) || budget.UsedByteCount() != kib(8) {
		t.Fatal("rewrite stole new root capacity after original admission")
	}
	item.releaseMemory()
	credit.release()
	assertRetainedBudgetBalance(t, budget)
}

// A physical item cannot silently combine recyclable ownership with another
// owner; collection must keep this boundary before recovery is integrated.
func TestRecyclableSendMemoryRejectsMixedPhysicalOwners(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(16))
	first, ok := prepareRecyclableSendMemory(budget, kib(8), kib(1))
	if !ok {
		t.Fatal("first envelope admission")
	}
	defer first.release()
	second, ok := prepareRecyclableSendMemory(budget, kib(8), kib(1))
	if !ok {
		t.Fatal("second envelope admission")
	}
	defer second.release()
	firstOwner := &recyclableMemoryTestOwner{credit: first}
	secondOwner := &recyclableMemoryTestOwner{credit: second}
	for _, extra := range []sendAckRecord{{target: firstOwner}, {target: secondOwner}, {callback: func(error) {}}} {
		item := &sendItem{}
		item.acks.add(sendAckRecord{target: firstOwner})
		item.acks.add(extra)
		if item.reservePreparedMemory(budget, kib(4)) {
			t.Fatal("mixed physical ownership acquired unattributable recycling credit")
		}
		if item.memoryBudget != nil || first.bytes != kib(8) || first.inFlight != 0 || second.bytes != kib(8) || second.inFlight != 0 || budget.UsedByteCount() != kib(16) {
			t.Fatal("refused mixed physical ownership changed either envelope")
		}
	}
	first.release()
	second.release()
	assertRetainedBudgetBalance(t, budget)
}
