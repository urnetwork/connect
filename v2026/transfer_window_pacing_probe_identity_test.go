// Physical probe identity is independent of the latest tail on its sequence.
package connect

import (
	"testing"
	"time"
)

// Retry ambiguity follows the copied message. Other writes still invalidate
// tail-based drain proof, but cannot turn the original probe into two copies.
func TestWindowPacingProbeIdentitySurvivesUnrelatedRecovery(t *testing.T) {
	for _, event := range []string{"same-probe", "later-message", "sibling-message", "changed-carrier", "canceled-probe", "canceled-sibling"} {
		for _, ackBeforeConfirmation := range []bool{false, true} {
			start := time.Unix(1000, 0)
			sequence, tail, probe := NewId(), NewId(), NewId()
			service := &windowPacingService{}
			service.observeRoundTrip(time.Millisecond, 0, start)
			service.beginWrite(sequence, tail, 1, start, false)
			service.finishWrite(sequence, tail, true)
			service.observeRoundTrip(1200*time.Millisecond, 0, start.Add(20*time.Millisecond))
			if delay, _ := service.admitBurst(start.Add(40*time.Millisecond), 1000, false, &windowPacingWaiter{}); delay <= 0 {
				t.Fatalf("%s: controlled drain was not armed", event)
			}
			at := start.Add(100 * time.Millisecond)
			service.acknowledgeWrite(sequence, tail, 1, false, 0, at)
			service.beginWrite(sequence, probe, 2, at, false)
			if !ackBeforeConfirmation {
				service.finishWrite(sequence, probe, true)
			}
			if ackBeforeConfirmation {
				service.acknowledgeWrite(sequence, probe, 2, false, 0, at.Add(1200*time.Millisecond))
			}
			switch event {
			case "same-probe":
				service.invalidateMessageProbe(sequence, probe)
				service.beginWrite(sequence, probe, 2, at.Add(300*time.Millisecond), true)
				service.finishWrite(sequence, probe, true)
			case "later-message", "sibling-message", "canceled-sibling":
				nextSequence := sequence
				if event != "later-message" {
					nextSequence = NewId()
				}
				next := NewId()
				service.beginWrite(nextSequence, next, 3, at.Add(time.Millisecond), false)
				service.finishWrite(nextSequence, next, true)
				if event == "canceled-sibling" {
					pacer := windowBurstPacer{service: service, serviceSequenceId: nextSequence}
					pacer.close()
				} else {
					service.invalidateMessageProbe(nextSequence, next)
					service.beginWrite(nextSequence, next, 3, at.Add(300*time.Millisecond), true)
					service.finishWrite(nextSequence, next, true)
				}
			case "changed-carrier":
				service.finishWrite(sequence, probe, false)
			case "canceled-probe":
				pacer := windowBurstPacer{service: service, serviceSequenceId: sequence}
				pacer.close()
			}
			if ackBeforeConfirmation {
				service.finishWrite(sequence, probe, true)
			} else {
				service.acknowledgeWrite(sequence, probe, 2, false, 0, at.Add(1200*time.Millisecond))
			}
			want := time.Millisecond
			if event == "later-message" || event == "sibling-message" || event == "canceled-sibling" {
				want = 1200 * time.Millisecond
			}
			if got := service.roundTrip(); got != want {
				t.Fatalf("event=%s ACK-before-confirmation=%t: floor=%s, want=%s", event, ackBeforeConfirmation, got, want)
			}
			if event == "later-message" && service.drained {
				t.Fatal("retry of later tail manufactured a drained service")
			}
		}
	}
}
