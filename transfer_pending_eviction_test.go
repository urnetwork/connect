// Deterministic fixed-owner metadata regressions. Client/wire roots live in
// the preserved feedback-cut fixture; these roots isolate bounded storage and
// intrusive lifetime transitions without a scheduler or transport mock.
package connect

import (
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Pool ownership is reacquired before inspecting a reused item. A stale
// sequence cut cannot affect new exact custody even at the same wire number.
func TestPendingEvictionPoolReuseCannotCarryOldGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	// Retain a spare so the bounded pool has room to admit this retired item.
	spare := takeSendItem()
	t.Cleanup(spare.messagePoolReturn)
	item := takeSendItem()
	*item = sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 7},
		expectsAck:   true, selectiveAcked: true,
		sendTime: time.Now(), ackTimeout: time.Minute, resendTime: time.Now().Add(time.Hour),
	}
	old := &SendSequence{sequenceId: NewId(), client: &Client{}, resendQueue: newResendQueue(nil, 0)}
	old.sendItems = []*sendItem{item}
	old.addResendItem(item)
	old.observeEvictions(pendingEvictionAckForTest(7))
	staleCut := old.takePendingEvictions()
	old.detachResendItem(item.messageId)
	old.observeEvictions(pendingEvictionAckForTest(7))
	old.addResendItem(item)
	if item.pendingEvictionPhases == 0 {
		t.Fatal("pool-reuse setup did not merge actual pending markers onto its owner")
	}
	old.ackLifetimes.clear()
	old.releaseRetainedSendItems(nil)
	if state := pendingEvictionStateForTest(old); len(state.numbers) != 0 ||
		state.pending != 0 || state.inFlight != 0 || old.detachedAck != (sendAckIdentity{}) {
		t.Fatal("old exact owner retained markers after teardown")
	}
	var reused *sendItem
	var held []*sendItem
	t.Cleanup(func() {
		for _, owner := range held {
			owner.messagePoolReturn()
		}
	})
	for range sendItemPoolCapacity + 1 {
		candidate := takeSendItem()
		if candidate == item {
			reused = candidate
			break
		}
		held = append(held, candidate)
	}
	if reused == nil {
		t.Fatal("retired owner was not reacquired from its bounded pool")
	}
	if reused.pendingEvictionPhases != 0 {
		reused.messagePoolReturn()
		t.Fatal("pool return retained an old owner's pending markers")
	}
	*reused = sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 7},
		expectsAck:   true, selectiveAcked: true,
		sendTime: time.Now(), ackTimeout: time.Minute, resendTime: time.Now().Add(time.Hour),
	}
	current := &SendSequence{sequenceId: NewId(), client: &Client{}, resendQueue: newResendQueue(nil, 0)}
	current.sendItems = []*sendItem{reused}
	current.addResendItem(reused)
	t.Cleanup(func() {
		current.ackLifetimes.clear()
		current.releaseRetainedSendItems(nil)
	})
	current.observeEvictions(pendingEvictionAckForTest(7))
	currentCut := current.takePendingEvictions()
	current.applyPendingEvictions(staleCut)
	if !reused.selectiveAcked || reused.pendingEvictionPhases == 0 ||
		current.pendingEvictionInFlight != currentCut.phase {
		t.Fatal("stale-generation cut stole a new exact owner's markers")
	}
	current.applyPendingEvictions(currentCut)
	if reused.selectiveAcked || len(pendingEvictionStateForTest(current).numbers) != 0 {
		t.Fatal("current generation could not consume its own exact cut")
	}
}

// Ordinary allocated owners contain no pooled payload; cleanup only releases
// their empty fixture queue/index. Production-style heap mutation stays real.
func newPendingEvictionOwners(t *testing.T, numbers ...uint64) (*SendSequence, []*sendItem) {
	t.Helper()
	sequence := &SendSequence{
		sequenceId: NewId(), client: &Client{},
		resendQueue: newResendQueue(nil, 0),
	}
	items := make([]*sendItem, 0, len(numbers))
	for _, number := range numbers {
		item := &sendItem{
			transferItem: transferItem{messageId: NewId(), sequenceNumber: number},
			expectsAck:   true, selectiveAcked: true,
			sendTime:   time.Unix(1700000000, 0),
			resendTime: time.Now().Add(time.Hour), ackTimeout: 30 * time.Second,
			sendCount: 1,
		}
		sequence.sendItems = append(sequence.sendItems, item)
		sequence.addResendItem(item)
		items = append(items, item)
	}
	t.Cleanup(func() {
		sequence.ackLifetimes.clear()
		sequence.clearPendingEvictions()
		sequence.resendQueue.Clear()
		sequence.sendItems = nil
	})
	return sequence, items
}

func pendingEvictionAckForTest(numbers ...uint64) receiveAckMessage {
	return receiveAckMessage{evictions: &ackEvictionNotice{sequenceNumbers: numbers}}
}

// Unknown/future input cannot create a pending map entry, vector or wake.
func TestPendingEvictionUnknownNumbersRetainNoOwner(t *testing.T) {
	sequence, _ := newPendingEvictionOwners(t, 7)
	ack := pendingEvictionAckForTest(0, 8, math.MaxUint64)
	for range 1024 {
		if sequence.observeEvictions(ack) {
			t.Fatal("unknown number created receiver-withdrawal state")
		}
	}
	state := pendingEvictionStateForTest(sequence)
	if len(state.numbers) != 0 || state.pending != 0 || state.inFlight != 0 ||
		sequence.takePendingEvictions() != (pendingEvictionBatch{}) {
		t.Fatal("unknown numbers retained pending storage or a captured cut")
	}
}

// The changed representation itself allocates neither first-owner markers nor
// duplicate markers; this is not an end-to-end ACK decoding performance claim.
func TestPendingEvictionDuplicateMarkersDoNotAllocate(t *testing.T) {
	sequence, _ := newPendingEvictionOwners(t, 7)
	ack := pendingEvictionAckForTest(7, 7, math.MaxUint64)
	allocations := testing.AllocsPerRun(1024, func() {
		sequence.observeEvictions(ack)
	})
	if allocations != 0 || !slices.Equal(pendingEvictionStateForTest(sequence).numbers, []uint64{7}) {
		t.Fatalf("duplicate owner markers allocated or multiplied: allocations=%g state=%+v",
			allocations, pendingEvictionStateForTest(sequence))
	}
}

// Concurrent ingress uses the real queue mutex. Explicit wave joins force
// the pre/post-cut boundary; scheduler timing is not the assertion.
func TestPendingEvictionConcurrentIngressKeepsBothCuts(t *testing.T) {
	sequence, _ := newPendingEvictionOwners(t, 7, 8, 9)
	ack := pendingEvictionAckForTest(7, 8, 9, math.MaxUint64)
	wave := func() {
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range 16 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for range 64 {
					sequence.observeEvictions(ack)
				}
			}()
		}
		close(start)
		workers.Wait()
	}
	wave()
	first := sequence.takePendingEvictions()
	wave()
	sequence.applyPendingEvictions(first)
	if !slices.Equal(pendingEvictionStateForTest(sequence).numbers, []uint64{7, 8, 9}) {
		t.Fatal("concurrent duplicate ingress lost post-cut exact owners")
	}
	sequence.applyPendingEvictions(sequence.takePendingEvictions())
	if state := pendingEvictionStateForTest(sequence); len(state.numbers) != 0 ||
		state.pending != 0 || state.inFlight != 0 {
		t.Fatal("concurrent ingress left unbounded or unowned pending state")
	}
}

// A notice arriving after extraction must survive the earlier cut's drain.
func TestPendingEvictionCutPreservesLaterPhase(t *testing.T) {
	sequence, items := newPendingEvictionOwners(t, 7)
	ack := pendingEvictionAckForTest(7)
	sequence.observeEvictions(ack)
	first := sequence.takePendingEvictions()
	sequence.observeEvictions(ack)
	sequence.applyPendingEvictions(first)
	if items[0].selectiveAcked || items[0].recoveryKind != sendRecoveryEviction ||
		!slices.Equal(pendingEvictionStateForTest(sequence).numbers, []uint64{7}) {
		t.Fatal("first cut erased the later phase or did not schedule its own repair")
	}
	// A later genuine acceptance could re-establish a lease. Keep this isolated
	// root about cut membership, not the separate network-stale-SACK question.
	items[0].selectiveAcked = true
	sequence.setResendTime(items[0], time.Now().Add(time.Hour))
	second := sequence.takePendingEvictions()
	if second.phase == 0 || second.phase == first.phase {
		t.Fatal("later phase did not receive a distinct live cut")
	}
	sequence.applyPendingEvictions(second)
	if items[0].selectiveAcked || len(pendingEvictionStateForTest(sequence).numbers) != 0 {
		t.Fatal("later phase was lost")
	}
	sequence.applyPendingEvictions(first)
	if sequence.pendingEvictionInFlight != 0 {
		t.Fatal("already-consumed cut recreated ownership")
	}
}

// Detach moves (not aliases) markers; new ingress lands on its exact identity.
// Applying while absent defers only that owner and survives exact reinsertion.
func TestPendingEvictionDetachedOwnerMergesBothPhases(t *testing.T) {
	sequence, items := newPendingEvictionOwners(t, 7)
	item := items[0]
	ack := pendingEvictionAckForTest(7)
	sequence.observeEvictions(ack)
	first := sequence.takePendingEvictions()
	if sequence.detachResendItem(item.messageId) != item ||
		item.pendingEvictionPhases != 0 || sequence.detachedAck.pendingEvictionPhases == 0 {
		t.Fatal("detach did not move the exact owner's pending marker")
	}
	sequence.observeEvictions(ack)
	sequence.applyPendingEvictions(first)
	state := pendingEvictionStateForTest(sequence)
	if !state.detached || !slices.Equal(state.numbers, []uint64{7}) ||
		sequence.detachedAck.pendingEvictionPhases != first.phase^uint8(3) {
		t.Fatal("detached drain lost or duplicated the next-phase exact marker")
	}
	sequence.addResendItem(item)
	if sequence.detachedAck != (sendAckIdentity{}) || item.pendingEvictionPhases == 0 {
		t.Fatal("reinsert failed to merge detached custody")
	}
	sequence.applyPendingEvictions(sequence.takePendingEvictions())
	if item.selectiveAcked || len(pendingEvictionStateForTest(sequence).numbers) != 0 {
		t.Fatal("exact detached owner did not recover after reinsert")
	}
}

// Taking away an owner also takes away its marker; a later same-number owner
// cannot inherit old local state. This is not wire-level uint64 rollover proof.
func TestPendingEvictionRetiredOwnerCannotMarkReplacement(t *testing.T) {
	sequence, items := newPendingEvictionOwners(t, 7)
	sequence.observeEvictions(pendingEvictionAckForTest(7))
	cut := sequence.takePendingEvictions()
	old := sequence.resendQueue.RemoveByMessageId(items[0].messageId)
	sequence.ackLifetimes.remove(old)
	sequence.sendItems = nil
	replacement := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 7},
		expectsAck:   true, selectiveAcked: true, resendTime: time.Now().Add(time.Hour),
	}
	sequence.sendItems = []*sendItem{replacement}
	sequence.addResendItem(replacement)
	sequence.applyPendingEvictions(cut)
	if !replacement.selectiveAcked || replacement.pendingEvictionPhases != 0 ||
		replacement.recoveryKind != sendRecoveryNone || len(pendingEvictionStateForTest(sequence).numbers) != 0 {
		t.Fatal("retired exact owner's marker affected a replacement message")
	}
}

// No wire-chunk-sized cap may silently drop the last genuine retained victim.
func TestPendingEvictionAllOwnersBeyondWireChunkRecover(t *testing.T) {
	numbers := make([]uint64, evictionNoticeAckMaxCount+1)
	for index := range numbers {
		numbers[index] = uint64(index)
	}
	sequence, items := newPendingEvictionOwners(t, numbers...)
	sequence.observeEvictions(pendingEvictionAckForTest(numbers[:evictionNoticeAckMaxCount]...))
	sequence.observeEvictions(pendingEvictionAckForTest(numbers[evictionNoticeAckMaxCount:]...))
	sequence.applyPendingEvictions(sequence.takePendingEvictions())
	for _, item := range items {
		if item.selectiveAcked || item.recoveryKind != sendRecoveryEviction {
			t.Fatalf("genuine owner %d was dropped at an unrelated wire chunk cap", item.sequenceNumber)
		}
	}
	if sequence.resendQueue.Len() != len(items) || len(pendingEvictionStateForTest(sequence).numbers) != 0 {
		t.Fatal("repair changed retained custody count or left pending markers")
	}
}

// Repeated binary cut reuse has no numeric counter to overflow. Extreme wire
// values are looked up by exact identity, never by a wrapping numeric range.
func TestPendingEvictionPhaseReuseAndMaximumNumberStayExact(t *testing.T) {
	sequence, items := newPendingEvictionOwners(t, 0, math.MaxUint64)
	for index := range 260 {
		for _, item := range items {
			item.selectiveAcked = true
			sequence.setResendTime(item, time.Now().Add(time.Hour))
		}
		sequence.observeEvictions(pendingEvictionAckForTest(math.MaxUint64, 0))
		cut := sequence.takePendingEvictions()
		want := uint8(1)
		if index%2 != 0 {
			want = 2
		}
		if cut.phase != want {
			t.Fatalf("cut %d aliased an overflowing phase: got=%d want=%d", index, cut.phase, want)
		}
		sequence.applyPendingEvictions(cut)
		if items[0].selectiveAcked || items[1].selectiveAcked ||
			len(pendingEvictionStateForTest(sequence).numbers) != 0 {
			t.Fatal("phase reuse or maximum sequence number lost exact repair")
		}
	}
}

// Zero feedback returns before a queue access/walk, even in an inert owner.
func TestPendingEvictionEmptyApplyDoesNotInspectFlight(t *testing.T) {
	sequence := &SendSequence{}
	sequence.applyPendingEvictions(pendingEvictionBatch{})
}

// Source and sequence-generation gates stay at the actual SendBuffer entry;
// an old ACK carrier is not permission for a foreign peer/current-number match.
func TestPendingEvictionForeignPeerAndGenerationCannotMark(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEvictionFeedbackFixture(t, true)
		stale := proto.Clone(f.notice.ack).(*protocol.Ack)
		stale.SequenceId = NewId().Bytes()
		if f.wire.sender.sendBuffer.Ack(f.wire.receiver.ClientId(), stale, 0) ||
			f.wire.sender.sendBuffer.Ack(NewId(), f.notice.ack, 0) ||
			f.pendingCount() != 0 {
			t.Fatal("foreign peer or stale generation created a current exact-owner marker")
		}
		f.wire.forward(f.sack, f.wire.senderIn)
		f.wire.forward(f.notice, f.wire.senderIn)
		f.afterInitial.release()
		f.requireRepairAndFinish(t, "validated retired carrier after foreign controls")
	})
}

// Real constructor/admission accounting must cover the actual owner layout;
// a saturated retained budget still needs no auxiliary reservation for notices.
func TestPendingEvictionLayoutIsCoveredByActualRetainedBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(128 * 1024)
		f := newEvictionFeedbackFixture(t, false, func(settings *SendBufferSettings) {
			settings.ResendQueueBudget = budget
			settings.ResendQueueMinByteCount = 0
			settings.ResendQueueRetainedByteAccounting = true
		})
		f.wire.forward(f.sack, f.wire.senderIn)
		f.stopAfterSnapshot(t)
		var charged ByteCount
		f.sequence.resendQueue.stateLock.Lock()
		for _, item := range f.sequence.resendQueue.messageIdItems {
			if item.memoryBudget != budget || item.QueueByteCount() < ByteCount(unsafe.Sizeof(sendItem{})) {
				f.sequence.resendQueue.stateLock.Unlock()
				t.Fatal("actual retained admission omitted the pending marker's owner layout")
			}
			charged += item.QueueByteCount()
		}
		f.sequence.resendQueue.stateLock.Unlock()
		if charged == 0 || budget.UsedByteCount() != charged {
			t.Fatalf("actual retained budget did not match live owners: used=%d charged=%d", budget.UsedByteCount(), charged)
		}
		budget.SetTotalByteCount(charged)
		beforeReserved, beforeReleased := budget.Counts()
		for range 1024 {
			duplicate := &windowRoundFrame{bytes: MessagePoolShareReadOnly(f.notice.bytes), ack: f.notice.ack}
			f.wire.heldFrames = append(f.wire.heldFrames, duplicate)
			f.wire.forward(duplicate, f.wire.senderIn)
		}
		afterReserved, afterReleased := budget.Counts()
		if f.pendingCount() != 1 || budget.Available() != 0 || budget.UsedByteCount() != charged ||
			beforeReserved != afterReserved || beforeReleased != afterReleased {
			t.Fatal("duplicate notices multiplied owners or escaped/borrowed retained budget")
		}
		if unsafe.Offsetof(sendItem{}.pendingEvictionPhases)+unsafe.Sizeof(uint8(0)) >
			unsafe.Offsetof(sendItem{}.schedulingKey) {
			t.Fatal("pending marker was not kept in the intended bounded owner field run")
		}
		t.Logf("layout sendItem=%d detachedIdentity=%d sendSequence=%d actual_live_charge=%d",
			unsafe.Sizeof(sendItem{}), unsafe.Sizeof(sendAckIdentity{}), unsafe.Sizeof(SendSequence{}), charged)
		f.afterSnapshot.release()
		f.requireRepairAndFinish(t, "retained budget at exact live charge")
		if budget.UsedByteCount() != 0 {
			t.Fatal("completed real delivery retained owner memory budget")
		}
		reserved, released := budget.Counts()
		if reserved != released {
			t.Fatalf("retained owner charges did not reconcile: reserved=%d released=%d", reserved, released)
		}
	})
}
