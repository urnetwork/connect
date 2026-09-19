package connect

// FLIGHTGATEFIX §20.3. Account for each retained mechanism against merged's
// exact struct sizes. Later receiver advertisements, ACK arrival timestamps
// and pacing state have explicit byte costs; unaccounted growth still fails.

import (
	"testing"
	"unsafe"
)

const (
	mergedSendItemByteCount          = 520
	mergedSequenceAckByteCount       = 88
	mergedReceiveAckMessageByteCount = 72
	// §13.5's timeoutDeferCount and timeoutDeferAckTime
	deferStateByteCount = 32
	// §34.3's laneAckedAtLastFiring: where this item's own lane had got to
	// when it last looked. §34.5 expected the rule to need no new bytes,
	// because the per-route slots already hold the sequence numbers its
	// rules read. They do not hold this one: rule 2 asks what moved on the
	// lane since this item last looked, which is per item and per position,
	// and a slot holds one number for the whole lane. The field is placed
	// against the struct's 8-byte tail, so it costs its own 8 bytes and no
	// padding.
	lanePositionStateByteCount = 8
	// THROUGHPUTFIX §37.3's receive advertisement: what the receiver said it
	// can still hold out of order, and whether it said anything at all. It is
	// a mechanism the landing keeps rather than a field left behind, and it
	// has to live on this struct because the sender clamps its window to the
	// latest advertised value and both wire paths decode into it. The count
	// is the narrow type and the flag packs against the existing bools, so
	// the pair costs one word rather than two.
	receiveAdvertisementStateByteCount = 8
	// one pointer, nil on every acknowledgement that evicted nothing, naming
	// the items a receiver removed from its hold after acknowledging them
	// (THROUGHPUTFIX §37.16)
	evictionNoticeStateByteCount = 8
	// Optional receiver compression duration plus its presence bit/padding.
	// Actual receiver ACK delay fits the same layout by grouping presence bits;
	// the compact ACK still has no additional retained word for that field.
	ackCompressionStateByteCount = 8
	// Local arrival survives ACK handoff without retaining the wire message.
	ackArrivalStateByteCount = 8
	pacingWireStateByteCount = 16
	// The actual first-release burst id survives until acknowledgement, so
	// older in-flight bursts cannot reset the newer RTT measurement ring.
	// This uint64 follows the two pacing words and adds no alignment padding.
	pacingBurstStateByteCount = 8
	// Mobile retained admission follows an item through retries and teardown.
	retainedBudgetOwnerByteCount = 8
)

func TestLandingStructsMatchMergedLessTheDeferState(t *testing.T) {
	if got, want := unsafe.Sizeof(sequenceAck{}), uintptr(mergedSequenceAckByteCount+ackArrivalStateByteCount); got != want {
		t.Errorf("sequenceAck is %d bytes, want baseline plus one arrival timestamp, %d",
			got, want)
	}
	if got, want := unsafe.Sizeof(receiveAckMessage{}),
		uintptr(mergedReceiveAckMessageByteCount+
			receiveAdvertisementStateByteCount+
			evictionNoticeStateByteCount+ackCompressionStateByteCount+ackArrivalStateByteCount); got != want {
		t.Errorf(
			"receiveAckMessage is %d bytes, want merged's %d plus %d for capacity, %d for evictions, %d for compression and 8 for arrival",
			got, mergedReceiveAckMessageByteCount,
			receiveAdvertisementStateByteCount, evictionNoticeStateByteCount, ackCompressionStateByteCount,
		)
	}
	want := uintptr(
		mergedSendItemByteCount + deferStateByteCount + lanePositionStateByteCount + pacingWireStateByteCount + pacingBurstStateByteCount + retainedBudgetOwnerByteCount)
	if got := unsafe.Sizeof(sendItem{}); got != want {
		t.Errorf(
			"sendItem is %d bytes, want merged's %d plus %d for the deferred retransmit's own state "+
				"and %d for the lane position it last looked at, plus %d for paced wire bytes and actual write time "+
				"and %d for the actual burst epoch plus %d for the lifetime budget owner; "+
				"anything else means a removed mechanism left a field behind",
			got, mergedSendItemByteCount, deferStateByteCount, lanePositionStateByteCount, pacingWireStateByteCount, pacingBurstStateByteCount, retainedBudgetOwnerByteCount,
		)
	}
}
