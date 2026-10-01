package connect

import (
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

type preparedSendMemoryTestOwner struct {
	credit *preparedSendMemory
	acks   int
	err    error
}

func TestPreparedSendMemoryLegacySequenceWithoutQueueIsUnaffected(t *testing.T) {
	// Existing admission fixtures omit resend ownership entirely. An ordinary
	// packet must never inspect or require the new prepared-flight machinery.
	sequence := &SendSequence{}
	if sequence.preparedPackFits(&SendPack{}) {
		t.Fatal("legacy packet manufactured prepared admission")
	}
}

func (self *preparedSendMemoryTestOwner) preparedSendMemory() *preparedSendMemory { return self.credit }
func (self *preparedSendMemoryTestOwner) sendAckResult(_ ByteCount, err error) {
	self.acks++
	self.err = err
	self.credit.release()
}

func TestPreparedSendMemoryTransfersAtFullParentWithoutReborrow(t *testing.T) {
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
		credit, ok := prepareSendMemory(budget, root.TotalByteCount())
		if !ok {
			t.Fatal("pre-read credit was not admitted")
		}
		defer credit.release()
		owner := &preparedSendMemoryTestOwner{credit: credit}
		frame := budgetTestFrame(1000)
		accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), nil, 0,
			TransferOptions{Ack: true}, sendAckTargetOption{target: owner}, sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true})
		if !accepted || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("prepaid Pack at full parent refused: accepted=%t err=%v", accepted, err)
		}
		wire := fixture.takePack(1)
		if wire.pack.Nack || root.UsedByteCount() != root.TotalByteCount() || owner.acks != 0 {
			t.Fatal("prepaid write lost reliable ownership or changed the root charge")
		}
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
		if owner.acks != 1 || owner.err != nil {
			t.Fatalf("prepaid completion: ACKs=%d err=%v", owner.acks, owner.err)
		}
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, root)
	})
}

func TestPreparedSendMemoryCoalescedMoveAndAbortReleaseExactlyOnce(t *testing.T) {
	root := NewTransferMemoryBudget(kib(16))
	budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
	first, ok := prepareSendMemory(budget, kib(8))
	if !ok {
		t.Fatal("first credit")
	}
	second, ok := prepareSendMemory(budget, kib(8))
	if !ok {
		t.Fatal("second credit")
	}
	item := &sendItem{}
	item.acks.add(sendAckRecord{target: &preparedSendMemoryTestOwner{credit: first}})
	item.acks.add(sendAckRecord{target: &preparedSendMemoryTestOwner{credit: second}})
	if !item.reservePreparedMemory(budget, kib(12)) || root.UsedByteCount() != kib(16) {
		t.Fatal("coalesced full-parent transfer refused or reborrowed")
	}
	first.release()
	first.release()
	second.release()
	second.release()
	if root.UsedByteCount() != kib(12) {
		t.Fatal("aborting unused credit released the live flight")
	}
	item.releaseMemory()
	item.releaseMemory()
	assertRetainedBudgetBalance(t, budget)
	assertRetainedBudgetBalance(t, root)
}

// A repeated logical-group record cannot manufacture extra bytes from one
// reservation, nor may a failed growth attempt consume any existing credit.
func TestPreparedSendMemoryDuplicateAndFailedGrowthKeepOwnership(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(8))
	credit, _ := prepareSendMemory(budget, kib(8))
	defer credit.release()
	owner := &preparedSendMemoryTestOwner{credit: credit}
	item := &sendItem{}
	item.acks.add(sendAckRecord{target: owner})
	item.acks.add(sendAckRecord{target: owner})
	if item.reservePreparedMemory(budget, kib(12)) {
		t.Fatal("duplicate records invented prepared capacity")
	}
	pack := &SendPack{ackTarget: owner, Frame: &protocol.Frame{}}
	if pack.preparedMemoryBytes(budget) != kib(8) || budget.UsedByteCount() != kib(8) {
		t.Fatal("failed growth consumed prepared credit")
	}
	if !item.reservePreparedMemory(budget, kib(8)) {
		t.Fatal("unchanged original credit became unusable")
	}
	item.releaseMemory()
	credit.release()
	assertRetainedBudgetBalance(t, budget)
}

func TestPreparedSendMemoryDetachedMetadataStaysChargedThroughAck(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(16))
	parent, _ := prepareSendMemory(budget, kib(16))
	credit, ok := parent.split(kib(16), kib(1))
	if !ok {
		t.Fatal("split full-parent pre-read claim")
	}
	parent.release()
	owner := &preparedSendMemoryTestOwner{credit: credit}
	item := &sendItem{}
	item.acks.add(sendAckRecord{target: owner})
	if item.reservePreparedMemory(budget, kib(16)) {
		t.Fatal("serialized flight consumed the detached capsule's fixed memory")
	}
	if !item.reservePreparedMemory(budget, kib(12)) {
		t.Fatal("legal flight could not reuse prepared credit")
	}
	credit.releaseUnused()
	if budget.UsedByteCount() != kib(13) {
		t.Fatalf("first-write retirement did not leave flight + capsule charged: %d", budget.UsedByteCount())
	}
	item.releaseMemory()
	if budget.UsedByteCount() != kib(1) {
		t.Fatal("flight release also released a still-live ACK capsule")
	}
	owner.sendAckResult(0, nil)
	credit.require(kib(8))
	if credit.growRequired() {
		t.Fatal("late actor retry resurrected a terminal ACK capsule")
	}
	assertRetainedBudgetBalance(t, budget)
}
