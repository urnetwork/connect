// Receipt state is bounded and records successful downstream custody only.
package connect

import (
	"testing"
	"time"
)

func TestImmediateFailedDownstreamClaimDoesNotRecordDelivery(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	history := &receiveImmediateHistory{sequenceId: NewId(), maxRanges: 2}
	span, token := history.reserve(1, Id{}, time.Time{}, time.Now(), false)
	if span == nil {
		t.Fatal("first actual receipt slot was unavailable")
	}
	canceled := false
	sequence := &ReceiveSequence{ackWindow: newSequenceAckWindow(), cancel: func() { canceled = true }}
	queue := newReceiveDeliveryQueue(sequence, 2, 1)
	queue.immediate = true
	item := &receiveItem{noBudget: true, ack: true, transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}}
	receipt, accepted := queue.append(item, true)
	if !accepted {
		t.Fatal("independent protocol receipt was refused before DATA admission")
	}
	receipt.immediateHistory, receipt.immediateRange, receipt.immediateToken = history, span, token
	item.immediateReceipt = receipt
	claim := receipt.hold()
	if claim == nil {
		t.Fatal("actual downstream callback could not acquire its receipt claim")
	}
	receipt.seal()
	claim.complete(false)
	if !canceled || receipt.secured || queue.pending() || history.pendingCount != 0 || history.everSecured {
		t.Fatal("refused callback became durable delivered state or stranded an owner")
	}
	if _, known, secured := history.replay(1, Id{}); known || secured || len(sequence.ackWindow.Snapshot(false).selectiveAcks) != 0 {
		t.Fatal("refused callback produced an exact terminal receipt or replay marker")
	}
}

func TestImmediateSparseCapacityNeverMergesMissingOrdinals(t *testing.T) {
	history := &receiveImmediateHistory{sequenceId: NewId(), maxRanges: 2}
	for _, number := range []uint64{1, 3} {
		span, token := history.reserve(number, Id{}, time.Time{}, time.Now(), false)
		if span == nil || !history.complete(span, token, true) {
			t.Fatal("two disjoint secured receipts did not occupy their fixed slots")
		}
	}
	if span, _ := history.reserve(5, Id{}, time.Time{}, time.Now(), false); span != nil {
		t.Fatal("sparse receipt pressure allocated beyond fixed identity capacity")
	}
	if _, known, _ := history.replay(2, Id{}); known {
		t.Fatal("range compression fabricated delivery across the missing ordinal")
	}
	if _, known, secured := history.replay(1, Id{}); !known || !secured {
		t.Fatal("sparse pressure evicted an already delivered exact receipt")
	}
}

func TestImmediateContiguousRangeKeepsInteriorReceiptAndPrunesOnlyRealPrefix(t *testing.T) {
	history := &receiveImmediateHistory{sequenceId: NewId(), maxRanges: 2}
	for number := uint64(2); number <= 10004; number++ {
		span, token := history.reserve(number, Id{}, time.Time{}, time.Now(), false)
		if span == nil || !history.complete(span, token, true) {
			t.Fatal("contiguous successful protocol train exhausted receipt state")
		}
	}
	active := 0
	for _, span := range history.ranges {
		if span != nil && span.secured {
			active++
		}
	}
	if active != 1 {
		t.Fatal("contiguous delivery retained one marker per original")
	}
	if _, known, secured := history.replay(5000, Id{}); !known || !secured {
		t.Fatal("compacted interior could not recover its lost terminal receipt")
	}
	history.noteCumulative(sequenceAck{sequenceNumber: 1, messageId: NewId()})
	if _, known, secured := history.replay(5000, Id{}); !known || !secured {
		t.Fatal("earlier DATA cumulative receipt erased live future receipt authority")
	}
	history.noteCumulative(sequenceAck{sequenceNumber: 10004, messageId: NewId()})
	for _, span := range history.ranges {
		if span != nil && (span.pending || span.secured) {
			t.Fatal("actual cumulative coverage did not release compacted future state")
		}
	}
}
