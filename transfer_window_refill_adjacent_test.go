// A physical path proof changes delivery-history eligibility, never permission
// or logical accounting. These transitions require no scheduler timing.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Peer, configured, target, and pool bounds still constrain the permission
// used while delivery on the newly proved path is being remeasured.
func TestWindowPacingWindowProofPreservesFixedBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			name       string
			peer       ByteCount
			configured ByteCount
			target     ByteCount
			budget     ByteCount
		}{
			{name: "peer", peer: kib(32)},
			{name: "configured", configured: kib(512)},
			{name: "target", target: 1250000},
			{name: "budget", budget: mib(1)},
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
			if estimate.Window != estimate.Ceiling || estimate.Ceiling >= mib(48) {
				t.Errorf("%s proof ignored fixed permission: %+v", test.name, estimate)
			}
			if test.peer > 0 && estimate.Window > test.peer || test.configured > 0 && estimate.Window > test.configured || test.budget > 0 && estimate.Window > test.budget || test.target > 0 && !estimate.TargetBound {
				t.Errorf("%s permission changed after path proof: %+v", test.name, estimate)
			}
		}
	})
}

// Repeated statistics and controller reads do not move the proof. Once a
// complete post-proof slow delivery interval exists it again lowers the window.
func TestWindowPacingWindowProofAcceptsFreshSlowDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, metadata)
			sequence := sequences[0]
			at := completeWindowRefillProbe(t, sequence, service, metadata, 1210*time.Millisecond)
			step := service.windowDeliveryStep()
			for range 8 {
				sequence.sendWindowSnapshot(at)
				sequence.sendWindowEstimate(at)
			}
			for range 80 {
				time.Sleep(50 * time.Millisecond)
				sequence.observeAckedBytesWithServiceCredit(4096, 0, windowServiceAckCredit{}, time.Now())
			}
			estimate := sequence.sendWindowEstimate(time.Now())
			if step != at.UnixNano() || service.windowDeliveryStep() != step || estimate.Window != estimate.Floor || estimate.Reason != "delivery" {
				t.Errorf("metadata=%t proof moved or suppressed fresh slower delivery: step=%d now=%d estimate=%+v", metadata, step, service.windowDeliveryStep(), estimate)
			}
		}
	})
}

// An ACK racing a writer's return cannot authorize a larger window before
// successful physical confirmation; a failed carrier never supplies proof.
func TestWindowPacingWindowProofWaitsForPhysicalConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, succeeds := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, true)
			sequence := sequences[0]
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
			service.finishWrite(sequence.sequenceId, messageId, succeeds)
			estimate := sequence.sendWindowEstimate(at)
			if succeeds && estimate.Window != estimate.Ceiling || !succeeds && service.windowDeliveryStep() != 0 {
				t.Errorf("success=%t physical confirmation did not own window proof: %+v", succeeds, estimate)
			}
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
