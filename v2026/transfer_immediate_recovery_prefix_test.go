// Immediate secured ordinals must not look like missing DATA in recovery trains.
package connect

import "testing"

func TestReceiveConsumerRecoveryPrefixIncludesSecuredImmediateOrdinal(t *testing.T) {
	GetMessagePoolAggregateStats()
	f := newReceiveRecoveryPrefixBoundsFixture(t, 0)
	f.sequence.sequenceId = NewId()
	f.add(t, 0, f.head)
	history := &receiveImmediateHistory{sequenceId: f.sequence.sequenceId}
	history.ranges[0] = &receiveImmediateRange{first: 1, last: 1, secured: true}
	f.sequence.immediateHistory = history
	f.sequence.immediateDeliveryHistory.Store(history)
	if !f.allows(2) {
		t.Fatal("SECURED_PROTOCOL_ORDINAL_REJECTED_AS_MISSING_DATA: paid DATA at0 plus secured protocol at1 must fund the complete predecessor prefix")
	}
}

func TestReceiveConsumerRecoveryPrefixDoesNotUseUnsecuredImmediateOrdinal(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, kind := range []string{"pending", "unsecured", "foreign-generation"} {
		t.Run(kind, func(t *testing.T) {
			f := newReceiveRecoveryPrefixBoundsFixture(t, 0)
			f.sequence.sequenceId = NewId()
			f.add(t, 0, f.head)
			history := &receiveImmediateHistory{sequenceId: f.sequence.sequenceId}
			span := &receiveImmediateRange{first: 1, last: 1, secured: true}
			switch kind {
			case "pending":
				span.pending = true
			case "unsecured":
				span.secured = false
			case "foreign-generation":
				history.sequenceId = NewId()
			}
			history.ranges[0] = span
			f.sequence.immediateHistory = history
			f.sequence.immediateDeliveryHistory.Store(history)
			if f.allows(2) {
				t.Fatal("unsecured or foreign protocol state fabricated DATA predecessor custody")
			}
		})
	}
}
