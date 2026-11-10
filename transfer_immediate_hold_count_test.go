// Missing-count boundaries distinguish received protocol from real DATA holes.
package connect

import (
	"math"
	"testing"
)

func TestImmediateHoldMissingCountWithoutHistoryKeepsDenseRule(t *testing.T) {
	s := &ReceiveSequence{nextSequenceNumber: 10}
	if got := s.heldDataMissingBelow(15, 2); got != 3 {
		t.Fatalf("dense missing count = %d, want 3", got)
	}
}

func TestImmediateHoldMissingCountClipsSecuredRangesAndKeepsPending(t *testing.T) {
	id := NewId()
	h := &receiveImmediateHistory{sequenceId: id, maxRanges: receiveImmediateRangeCapacity}
	h.ranges[0] = &receiveImmediateRange{first: 1, last: 4, secured: true}
	h.ranges[1] = &receiveImmediateRange{first: 8, last: 12, secured: true}
	h.ranges[2] = &receiveImmediateRange{first: 5, last: 5, pending: true}
	s := &ReceiveSequence{sequenceId: id, nextSequenceNumber: 3, immediateHistory: h}
	// DATA at 6 is held; protocol 3,4,8 is secured; 5 and 7 are real holes.
	if got := s.heldDataMissingBelow(9, 1); got != 2 {
		t.Fatalf("missing count = %d, want exactly the two unreceived positions", got)
	}
	if h.ranges[0].first != 1 || h.ranges[1].last != 12 || !h.ranges[2].pending {
		t.Fatal("counting changed protocol receipt ownership")
	}
}

func TestImmediateHoldMissingCountRejectsAnotherGeneration(t *testing.T) {
	h := &receiveImmediateHistory{sequenceId: NewId(), maxRanges: receiveImmediateRangeCapacity}
	h.ranges[0] = &receiveImmediateRange{first: 1, last: 127, secured: true}
	s := &ReceiveSequence{sequenceId: NewId(), nextSequenceNumber: 1, immediateHistory: h}
	if got := s.heldDataMissingBelow(129, 0); got != 128 {
		t.Fatalf("foreign generation erased real holes: %d", got)
	}
}

func TestImmediateHoldMissingCountNeverUsesUnsecuredReceipt(t *testing.T) {
	id := NewId()
	h := &receiveImmediateHistory{sequenceId: id, maxRanges: receiveImmediateRangeCapacity}
	h.ranges[0] = &receiveImmediateRange{first: 1, last: 127, pending: true}
	h.ranges[1] = &receiveImmediateRange{first: 128, last: 128}
	s := &ReceiveSequence{sequenceId: id, nextSequenceNumber: 1, immediateHistory: h}
	if got := s.heldDataMissingBelow(129, 0); got != 128 {
		t.Fatalf("unsecured receipt erased real holes: %d", got)
	}
}

func TestImmediateHoldMissingCountNearOrdinalLimit(t *testing.T) {
	id := NewId()
	floor := uint64(math.MaxUint64 - 10)
	h := &receiveImmediateHistory{sequenceId: id, maxRanges: receiveImmediateRangeCapacity}
	h.ranges[0] = &receiveImmediateRange{first: floor + 2, last: floor + 7, secured: true}
	s := &ReceiveSequence{sequenceId: id, nextSequenceNumber: floor, immediateHistory: h}
	if got := s.heldDataMissingBelow(floor+9, 0); got != 3 {
		t.Fatalf("near-limit count = %d, want 3", got)
	}
}
