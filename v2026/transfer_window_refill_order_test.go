// Delayed write confirmation must compare the live unloaded baseline, not a
// historical view that excludes a newer already observed short-path tuple.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The equal-old-floor row leaves the baseline timestamp unchanged until
// confirmation retires old rows; the newer equal tuple then owns that floor.
func TestWindowPacingWindowProofKeepsNewerShortObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			name     string
			newer    time.Duration
			wantStep bool
		}{
			{name: "smaller", newer: 100 * time.Microsecond},
			{name: "equal old", newer: 300 * time.Microsecond},
			{name: "equal probe", newer: 1200 * time.Millisecond, wantStep: true},
			{name: "larger", newer: 2 * time.Second, wantStep: true},
		} {
			sequences, service := newWindowRefillProofFixture(t, 1, true)
			sequence := sequences[0]
			messageId := NewId()
			service.drained = true
			service.beginWrite(sequence.sequenceId, messageId, 1, time.Now(), false)
			time.Sleep(1210 * time.Millisecond)
			ackedAt := time.Now()
			service.observeReceiverRoundTripForWrite(sequence.sequenceId, messageId, 1, 1210*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, ackedAt)
			service.acknowledgeWrite(sequence.sequenceId, messageId, 1, false, 10*time.Millisecond, ackedAt)
			time.Sleep(time.Millisecond)
			service.observeReceiverRoundTrip(2, test.newer+10*time.Millisecond, test.newer, 10*time.Millisecond, time.Now())
			time.Sleep(time.Millisecond)
			service.finishWrite(sequence.sequenceId, messageId, true)
			wantFloor := min(1200*time.Millisecond, test.newer)
			floor := service.roundTripEvidence(time.Now()).minimum
			wantStep := int64(0)
			if test.wantStep {
				wantStep = ackedAt.UnixNano()
			}
			t.Logf("%s current-floor=%s proof-step=%d want-step=%d", test.name, floor, service.windowDeliveryStep(), wantStep)
			if floor != wantFloor || service.windowDeliveryStep() != wantStep {
				t.Errorf("%s historical comparison changed current window evidence: floor=%s step=%d want=%s/%d", test.name, floor, service.windowDeliveryStep(), wantFloor, wantStep)
			}
		}
	})
}

// A first measurement from no shared floor has no prior path generation.
func TestWindowPacingWindowProofFirstUnknownSampleHasNoOldGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			service := newWindowPacingService(DefaultSendBufferSettings())
			sequence := &SendSequence{sequenceId: NewId()}
			completeWindowRefillProbe(t, sequence, service, metadata, 1210*time.Millisecond)
			if service.windowDeliveryStep() != 0 {
				t.Errorf("metadata=%t first sample invented an older path generation", metadata)
			}
		}
	})
}

// Repeated physical confirmation or ACK notification cannot apply one proof
// twice, even when later notifications carry later local arrival timestamps.
func TestWindowPacingWindowProofConsumedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequences, service := newWindowRefillProofFixture(t, 1, true)
		sequence := sequences[0]
		at := completeWindowRefillProbe(t, sequence, service, true, 1210*time.Millisecond)
		write := service.writes[sequence.sequenceId]
		for range 3 {
			time.Sleep(time.Millisecond)
			service.finishWrite(sequence.sequenceId, write.messageId, true)
			service.acknowledgeWrite(sequence.sequenceId, write.messageId, 1, false, 10*time.Millisecond, time.Now())
		}
		if service.windowDeliveryStep() != at.UnixNano() {
			t.Error("repeated confirmation moved the same physical proof")
		}
	})
}
