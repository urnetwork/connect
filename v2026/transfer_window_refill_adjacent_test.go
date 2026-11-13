// A physical path proof changes delivery-history eligibility, never permission
// or logical accounting. These transitions require no scheduler timing.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Peer, configured-byte, and pool limits still constrain admission after a
// path proof. Existing measured service can size a target-rate candidate but
// cannot erase retained capacity or turn that target into hard permission.
func TestWindowPacingWindowProofPreservesHardBoundsAndRetainedCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			name       string
			peer       ByteCount
			configured ByteCount
			target     ByteCount
			budget     ByteCount
			ceiling    ByteCount
			window     ByteCount
		}{
			{name: "peer", peer: kib(32), ceiling: kib(32), window: kib(32)},
			{name: "configured", configured: kib(512), ceiling: kib(512), window: kib(512)},
			{name: "target", target: 1250000, ceiling: mib(48), window: mib(2)},
			{name: "budget", budget: mib(1), ceiling: mib(1), window: mib(1)},
		} {
			sequences, service := newWindowRefillProofFixture(t, 1, true)
			sequence := sequences[0]
			if test.peer > 0 {
				sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(test.peer)})
			}
			sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = test.configured
			sequence.sendBufferSettings.TargetGoodputByteRate = test.target
			if test.budget > 0 {
				sequence.resendQueue.Clear()
				sequence.resendQueue = newResendQueue(NewTransferMemoryBudget(test.budget), sequence.sendBufferSettings.ResendQueueMinByteCount)
			}
			at := completeWindowRefillProbe(t, sequence, service, true, 1210*time.Millisecond)
			estimate := sequence.sendWindowEstimate(at)
			if estimate.Window != test.window || estimate.Ceiling != test.ceiling || estimate.LearnedWindow != estimate.Initial {
				t.Errorf("%s proof changed hard permission or learned capacity: %+v", test.name, estimate)
			}
			if estimate.Sized || !estimate.ServiceSized || estimate.TargetBound || estimate.CandidateTargetBound != (test.target > 0) {
				t.Errorf("%s proof confused cumulative and service qualification: %+v", test.name, estimate)
			}
			assertWindowRefillLogicalRate(t, estimate)
			if service.windowDeliveryStep() != at.UnixNano() || estimate.RoundTrip != 1200*time.Millisecond || estimate.WindowRoundTrip != 1210*time.Millisecond {
				t.Errorf("%s proof did not establish the exact new path: step=%d estimate=%+v", test.name, service.windowDeliveryStep(), estimate)
			}
		}
	})
}

// Repeated reads do not move the proof. Fresh slow cumulative delivery remains
// qualified without erasing the window learned from independent service.
func TestWindowPacingWindowProofAcceptsFreshSlowDeliveryWithoutShrinking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, metadata)
			sequence := sequences[0]
			at := completeWindowRefillProbe(t, sequence, service, metadata, 1210*time.Millisecond)
			step := service.windowDeliveryStep()
			before := sequence.sendWindowEstimate(at)
			wantWindow := ByteCount(30500000)
			if metadata {
				wantWindow = 30250000
			}
			if before.Sized || !before.ServiceSized || before.Window != wantWindow || before.CandidateWindow != wantWindow {
				t.Fatalf("metadata=%t stale history qualified immediately after proof: %+v", metadata, before)
			}
			assertWindowRefillLogicalRate(t, before)
			for range 8 {
				for _, estimate := range []SendWindowEstimate{sequence.sendWindowSnapshot(at), sequence.sendWindowEstimate(at)} {
					if estimate.Sized || !estimate.ServiceSized || estimate.Window != before.Window || estimate.LearnedWindow != before.LearnedWindow || service.windowDeliveryStep() != step {
						t.Fatalf("metadata=%t read moved proof or learned capacity: %+v", metadata, estimate)
					}
				}
			}
			for range 80 {
				time.Sleep(50 * time.Millisecond)
				sequence.observeAckedBytesWithServiceCredit(4096, 0, windowServiceAckCredit{}, time.Now())
			}
			estimate := sequence.sendWindowEstimate(time.Now())
			if step != at.UnixNano() || service.windowDeliveryStep() != step || !estimate.Sized || !estimate.ServiceSized || estimate.CandidateWindow != before.CandidateWindow || estimate.Window != before.Window || estimate.LearnedWindow != before.LearnedWindow {
				t.Errorf("metadata=%t proof moved, suppressed fresh delivery, or reduced learned capacity: step=%d now=%d estimate=%+v", metadata, step, service.windowDeliveryStep(), estimate)
			}
			assertWindowRefillLogicalRate(t, estimate)
		}
	})
}

// An ACK racing a writer's return cannot replace the RTT/history baseline
// before successful physical confirmation. Confirmed timing can then combine
// with independently established service to grow the required flight.
func TestWindowPacingWindowProofWaitsForPhysicalConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, succeeds := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, true)
			sequence := sequences[0]
			before := sequence.sendWindowEstimate(time.Now())
			messageId := NewId()
			service.drained = true
			service.beginWrite(sequence.sequenceId, messageId, 1, time.Now(), false)
			time.Sleep(1210 * time.Millisecond)
			at := time.Now()
			service.observeReceiverRoundTripForWrite(sequence.sequenceId, messageId, 1, 1210*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, at)
			service.acknowledgeWrite(sequence.sequenceId, messageId, 1, false, 10*time.Millisecond, at)
			if service.windowDeliveryStep() != 0 {
				t.Error("unconfirmed physical write changed window history")
			}
			if unconfirmed := sequence.sendWindowEstimate(at); !unconfirmed.Sized || unconfirmed.RoundTrip != before.RoundTrip || unconfirmed.CandidateWindow != before.CandidateWindow || unconfirmed.Window != before.Window {
				t.Errorf("unconfirmed physical write changed sizing evidence: before=%+v after=%+v", before, unconfirmed)
			}
			service.finishWrite(sequence.sequenceId, messageId, succeeds)
			estimate := sequence.sendWindowEstimate(at)
			if estimate.Ceiling != before.Ceiling || !estimate.ServiceSized {
				t.Errorf("success=%t physical confirmation changed permission or lost valid service: %+v", succeeds, estimate)
			}
			if succeeds {
				if service.windowDeliveryStep() != at.UnixNano() || estimate.RoundTrip != 1200*time.Millisecond || estimate.WindowRoundTrip != 1210*time.Millisecond || estimate.Sized || estimate.Window != 30250000 || estimate.LearnedWindow != estimate.Window || estimate.CandidateWindow != estimate.Window {
					t.Errorf("confirmed physical write failed to replace old sizing evidence: step=%d estimate=%+v", service.windowDeliveryStep(), estimate)
				}
			} else if service.windowDeliveryStep() != 0 || !estimate.Sized || estimate.RoundTrip != before.RoundTrip || estimate.CandidateWindow != before.CandidateWindow || estimate.Window != before.Window || estimate.LearnedWindow != before.LearnedWindow {
				t.Errorf("failed physical write changed sizing evidence: %+v", estimate)
			}
			assertWindowRefillLogicalRate(t, estimate)
		}
	})
}

// A lower proved path has no larger flight requirement and cannot repeatedly
// requalify delivery merely because a new empty-flight reply exists.
func TestWindowPacingWindowProofIgnoresLowerPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, metadata)
			sequence := sequences[0]
			completeWindowRefillProbe(t, sequence, service, metadata, 1210*time.Millisecond)
			step := service.windowDeliveryStep()
			completeWindowRefillProbe(t, sequence, service, metadata, 210*time.Millisecond)
			if service.windowDeliveryStep() != step {
				t.Errorf("metadata=%t lower path restarted history: before=%d after=%d", metadata, step, service.windowDeliveryStep())
			}
		}
	})
}

// Delayed older ACK application after newer ordinary timing cannot move the
// proof clock backward or certify a larger floor from superseded feedback.
func TestWindowPacingWindowProofRejectsDelayedOldAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequences, service := newWindowRefillProofFixture(t, 1, true)
		sequence := sequences[0]
		completeWindowRefillProbe(t, sequence, service, true, 1210*time.Millisecond)
		step := service.windowDeliveryStep()
		messageId := NewId()
		service.drained = true
		service.beginWrite(sequence.sequenceId, messageId, 2, time.Now(), false)
		service.finishWrite(sequence.sequenceId, messageId, true)
		time.Sleep(2 * time.Second)
		service.observeReceiverRoundTrip(3, 1300*time.Millisecond, 1290*time.Millisecond, 10*time.Millisecond, time.Now())
		older := time.Now().Add(-time.Millisecond)
		service.observeReceiverRoundTripForWrite(sequence.sequenceId, messageId, 2, 1999*time.Millisecond, 1989*time.Millisecond, 10*time.Millisecond, older)
		service.acknowledgeWrite(sequence.sequenceId, messageId, 2, false, 10*time.Millisecond, older)
		if service.windowDeliveryStep() != step || service.roundTripEvidence(time.Now()).minimum != 1200*time.Millisecond {
			t.Error("older delayed feedback changed confirmed window generation")
		}
	})
}
