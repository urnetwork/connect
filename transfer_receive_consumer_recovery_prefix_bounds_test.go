// Prefix bounds use real bounded allocations and registered-owner return.
// These metadata-ledger controls grant no wire, contract or delivery authority;
// the signed Client train and failed-prefix roots cover those boundaries.
package connect

import (
	"context"
	"testing"
)

// Each descriptor pays its whole metadata envelope through the maintained
// allocator before registration. No body, borrowed loan or fake budget is used.
type receiveRecoveryPrefixBoundsFixture struct {
	root, rx, head, other *TransferMemoryBudget
	sequence              *ReceiveSequence
	receiver              *ReceiveBuffer
	owners                []*decodedPackOwner
	cancel                context.CancelFunc
}

func newReceiveRecoveryPrefixBoundsFixture(t *testing.T, floor uint64) *receiveRecoveryPrefixBoundsFixture {
	t.Helper()
	cost := decodedPackOwnerQueueByteCount
	root := NewTransferMemoryBudget(4 * 512 * cost)
	rx := NewTransferMemoryBudgetWithParent(512*cost, root)
	head := NewTransferMemoryBudgetWithParent(512*cost, root)
	other := NewTransferMemoryBudgetWithParent(512*cost, root)
	settings := DefaultReceiveBufferSettingsWithBufferSize(64)
	settings.ReceiveQueueBudget, settings.ReceiveHeadRecoveryBudget = rx, head
	settings.ReceiveQueueRetainedByteAccounting = true
	ctx, cancel := context.WithCancel(t.Context())
	sequence := &ReceiveSequence{ctx: ctx, receiveQueue: newReceiveQueue(rx, 0), receiveBufferSettings: settings}
	sequence.receiveQueue.lifetimeBudget = true
	sequence.holdPromise.ingressFloor = floor
	fixture := &receiveRecoveryPrefixBoundsFixture{root: root, rx: rx, head: head, other: other,
		sequence: sequence, receiver: &ReceiveBuffer{receiveBufferSettings: settings}, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		for _, owner := range fixture.owners {
			if owner != nil {
				owner.release()
			}
		}
		for _, budget := range []*TransferMemoryBudget{head, rx, other, root} {
			assertRetainedBudgetBalance(t, budget)
			if budget.Stats().QuotaByteCount != 0 {
				t.Error("prefix bounds retained logical quota after registered owners returned")
			}
		}
	})
	return fixture
}

// This is descriptor publication, not a claim to have verified a network Pack.
// Actual allocation, registration, class and release invariants remain enabled.
func (self *receiveRecoveryPrefixBoundsFixture) add(t *testing.T, number uint64, budget *TransferMemoryBudget) *decodedPackOwner {
	t.Helper()
	settings := DefaultReceiveBufferSettings()
	settings.ReceiveQueueBudget = budget
	allocator := &ReceiveBuffer{receiveBufferSettings: settings}
	owner := allocator.takeReceiveMemoryOwner(nil, NewId(), number, decodedPackOwnerQueueByteCount)
	if owner == nil {
		t.Fatal("bounded prefix descriptor could not obtain its exact physical allocation")
	}
	self.root.admissionLock.Lock()
	self.sequence.registerReceiveIngressWithLock(&owner.receivePack)
	owner.receivePack.receivePublishedReliable = true
	owner.receivePack.receiveConsumerData = true
	self.root.admissionLock.Unlock()
	self.owners = append(self.owners, owner)
	return owner
}

// The helper is called only while its admission-root invariant is held.
func (self *receiveRecoveryPrefixBoundsFixture) allows(number uint64) bool {
	self.root.admissionLock.Lock()
	defer self.root.admissionLock.Unlock()
	return self.sequence.receiveConsumerRecoveryPrefixWithLock(number, self.head)
}

// Actual return removes intrusive membership; no stale pointer is later used
// as a descriptor, even if its pooled address is immediately reused elsewhere.
func (self *receiveRecoveryPrefixBoundsFixture) release(owner *decodedPackOwner) {
	for index, candidate := range self.owners {
		if candidate == owner {
			self.owners[index] = nil
		}
	}
	owner.release()
}

// The full64-bit mask is valid,65positions fail closed, and registration order
// does not substitute for the exact set of consecutive sequence numbers.
func TestReceiveConsumerRecoveryPrefixBounds64And65(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	for index := range 64 {
		fixture.add(t, uint64(index*17%64), fixture.head)
	}
	if !fixture.allows(64) {
		t.Fatal("64 distinct paid positions did not fill the complete prefix mask")
	}
	fixture.add(t, 64, fixture.head)
	if fixture.allows(65) || fixture.allows(0) || fixture.allows(^uint64(0)) {
		t.Fatal("distant or equal index escaped the64-position bound")
	}
}

// Wrapped/sub-floor indices cannot borrow a nearly exhausted uint64 domain.
func TestReceiveConsumerRecoveryPrefixBoundsNearUint64Maximum(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	maximum := ^uint64(0)
	floor := maximum - 3
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, floor)
	for offset := range uint64(3) {
		fixture.add(t, floor+offset, fixture.head)
	}
	if !fixture.allows(maximum) || fixture.allows(floor) || fixture.allows(floor-1) || fixture.allows(0) {
		t.Fatal("near-maximum prefix overflowed, underflowed or lost a valid nonwrapping span")
	}
	fixture.add(t, maximum, fixture.head)
	if fixture.allows(0) {
		t.Fatal("fully paid final uint64 positions authorized a wrapped zero successor")
	}
}

// Duplicate descriptor positions are an index-corruption control: every one
// remains paid, but duplicate counts never stand in for one absent position.
func TestReceiveConsumerRecoveryPrefixBoundsDuplicateBitsCannotFillGap(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 10)
	for offset := range uint64(8) {
		if offset != 3 {
			fixture.add(t, 10+offset, fixture.head)
		}
	}
	for range 8 {
		fixture.add(t, 10, fixture.head)
	}
	if fixture.allows(18) {
		t.Fatal("duplicate physical descriptors fabricated the missing sequence position")
	}
	gap := fixture.add(t, 13, fixture.head)
	if !fixture.allows(18) {
		t.Fatal("actual paid missing position did not complete the prefix")
	}
	fixture.release(gap)
	if fixture.allows(18) {
		t.Fatal("returned missing position continued to authorize its old prefix")
	}
}

// The visit ceiling applies to all descriptors, including irrelevant tails.
// Finding the final required position at256is valid;257must remain unknown.
func TestReceiveConsumerRecoveryPrefixBoundsVisit256FailsClosed(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	fixture.add(t, 0, fixture.head)
	for index := range 255 {
		fixture.add(t, uint64(100+index), fixture.head)
	}
	if !fixture.allows(1) {
		t.Fatal("complete prefix at the256th visit was not recognized")
	}
	extra := fixture.add(t, 1000, fixture.head)
	if fixture.allows(1) {
		t.Fatal("prefix beyond the bounded owner census was treated as known")
	}
	fixture.release(extra)
	if !fixture.allows(1) {
		t.Fatal("returning the excess descriptor did not restore the bounded exact census")
	}
}

// These mutations isolate published metadata fences under their actual lock;
// each is restored before terminal return, so no loan or class is relabeled.
func TestReceiveConsumerRecoveryPrefixBoundsRequiresPublishedUnloanedOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	owner := fixture.add(t, 0, fixture.head)
	fixture.root.admissionLock.Lock()
	published := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receivePublishedReliable = false
	unpublished := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receivePublishedReliable = true
	owner.receivePack.receiveConsumerData = false
	nonconsumer := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receiveConsumerData = true
	owner.receivePack.ingressLoanByteCount = 1
	loaned := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.ingressLoanByteCount = 0
	owner.receivePack.receiveMemoryOwned = false
	unowned := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.receiveMemoryOwned = true
	owner.receivePack.ingressSequence = &ReceiveSequence{}
	foreignGeneration := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	owner.receivePack.ingressSequence = fixture.sequence
	final := fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head)
	fixture.root.admissionLock.Unlock()
	if !published || unpublished || nonconsumer || loaned || unowned || foreignGeneration || !final {
		t.Fatal("unpublished, unowned, nonconsumer, loaned or foreign-generation metadata authorized a prefix")
	}
}

// A real separately charged child cannot impersonate the configured recovery
// child, even when every other descriptor property is identical.
func TestReceiveConsumerRecoveryPrefixBoundsRejectsOtherPaidChild(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	other := fixture.add(t, 0, fixture.other)
	if fixture.allows(1) || fixture.other.UsedByteCount() != decodedPackOwnerQueueByteCount || fixture.head.UsedByteCount() != 0 {
		t.Fatal("different physical child supplied recovery-prefix membership")
	}
	fixture.release(other)
	fixture.add(t, 0, fixture.head)
	if !fixture.allows(1) {
		t.Fatal("actual configured-child ownership did not supply the exact prefix")
	}
}

// Cancellation/closed-generation checks belong to the real allocating caller,
// not the bitmap utility; neither path may allocate from still-paid old roots.
func TestReceiveConsumerRecoveryPrefixBoundsAllocationKeepsLifetimeFence(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, closed := range []bool{false, true} {
		fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
		fixture.add(t, 0, fixture.head)
		if !fixture.allows(1) {
			t.Fatal("lifetime fence has no valid paid prefix premise")
		}
		if closed {
			fixture.root.admissionLock.Lock()
			fixture.sequence.holdPromise.closed = true
			fixture.root.admissionLock.Unlock()
		} else {
			fixture.cancel()
		}
		before := fixture.head.UsedByteCount()
		owner := fixture.receiver.takeReceiveHeadRecoveryOwner(fixture.sequence, receiveMemoryPackScan{
			messageId: NewId(), sequenceNumber: 1, loanSafe: true, frameCount: 1, consumerFrameCount: 1,
			retainedByteCount: decodedPackOwnerQueueByteCount,
		})
		if owner != nil {
			owner.release()
			t.Fatal("canceled or closed generation acquired a new child from its old paid prefix")
		}
		if fixture.head.UsedByteCount() != before {
			t.Fatal("retired prefix changed physical ownership")
		}
	}
}

// A real terminal caller races a root-owned census, then its completion
// proves exact unlink/refund. This does not force the old pre-lock nil-write
// interleave in normal mode; the race suite checks synchronization separately.
func TestReceiveConsumerRecoveryPrefixBoundsReleaseUnlinkIsRootLocked(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	owner := fixture.add(t, 0, fixture.head)
	fixture.root.admissionLock.Lock()
	started, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		<-proceed
		owner.release()
		close(done)
	}()
	<-started
	close(proceed)
	stable := owner.receiveItem.memoryBudget == fixture.head &&
		fixture.sequence.receiveConsumerRecoveryPrefixWithLock(1, fixture.head) &&
		fixture.head.UsedByteCount() == decodedPackOwnerQueueByteCount &&
		fixture.root.UsedByteCount() == decodedPackOwnerQueueByteCount
	fixture.root.admissionLock.Unlock()
	<-done
	fixture.owners[0] = nil
	if !stable || fixture.allows(1) || fixture.sequence.holdPromise.ingressHead != nil ||
		fixture.head.UsedByteCount() != 0 || fixture.root.UsedByteCount() != 0 {
		t.Fatal("terminal return changed a root-locked class or left a released prefix member")
	}
}
