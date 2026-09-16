// A newly proved path length must not inherit a window from delivery measured
// while the old flight limit starved that path. Fixed permission bounds remain.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Eight lanes share one physical service and pool; only logical delivery
// histories differ. Explicit timestamps retain three seconds of limited work.
func newWindowRefillProofFixture(t *testing.T, lanes int, metadata bool) ([]*SendSequence, *windowPacingService) {
	t.Helper()
	budget := NewTransferMemoryBudget(mib(48))
	sequences := make([]*SendSequence, lanes)
	for i := range sequences {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.ResendQueueBudget = budget
			settings.TargetGoodputByteRate = 0
		})
		sequence.sequenceId = NewId()
		sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(mib(48)),
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
		})
		sequence.rttWindow.closeSendTime(uint64(time.Now().Add(-time.Millisecond).UnixMilli()), time.Now())
		sequences[i] = sequence
		t.Cleanup(func() { sequence.resendQueue.Clear() })
	}
	service := newWindowPacingService(sequences[0].sendBufferSettings)
	for _, sequence := range sequences {
		sequence.windowPacer.service = service
	}
	if metadata {
		service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
	} else {
		service.observeRoundTrip(time.Millisecond, 10*time.Millisecond, time.Now())
	}
	// Service capacity and local window delivery are independent evidence.
	service.total, service.sent, service.serviceHoldRate = 6250000, 6250000, 12500000
	for range 61 {
		for _, sequence := range sequences {
			sequence.observeAckedBytesWithServiceCredit(4096, 0, windowServiceAckCredit{}, time.Now())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return sequences, service
}

// Only this exact physical first-write/ACK pair can certify an unloaded path;
// ordinary observations, retries, and a covering sibling do not call this.
func completeWindowRefillProbe(t *testing.T, sequence *SendSequence, service *windowPacingService, metadata bool, residence time.Duration) time.Time {
	t.Helper()
	messageId := NewId()
	service.drained = true
	service.sent += 1000
	service.beginWrite(sequence.sequenceId, messageId, 1, time.Now(), false)
	service.finishWrite(sequence.sequenceId, messageId, true)
	time.Sleep(residence)
	at := time.Now()
	if metadata {
		service.observeReceiverRoundTripForWrite(sequence.sequenceId, messageId, 1, residence, residence-10*time.Millisecond, 10*time.Millisecond, at)
	}
	service.acknowledgeWrite(sequence.sequenceId, messageId, 1, false, 10*time.Millisecond, at)
	service.total += 1000
	if !service.roundTripProbe.sentAt.IsZero() || service.roundTripEvidence(at).minimum < residence-10*time.Millisecond {
		t.Fatal("fixture failed to establish the exact long unloaded path")
	}
	return at
}

// Existing low logical delivery cannot undo a newly proved larger flight
// requirement before any complete delivery interval on that path exists.
func TestWindowPacingNewPathProofRequalifiesOldDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			for _, lanes := range []int{1, 8} {
				sequences, service := newWindowRefillProofFixture(t, lanes, metadata)
				at := completeWindowRefillProbe(t, sequences[0], service, metadata, 1210*time.Millisecond)
				for lane, sequence := range sequences {
					estimate := sequence.sendWindowEstimate(at)
					t.Logf("metadata=%t lanes=%d lane=%d window=%d ceiling=%d delivered=%d span=%s residence=%s reason=%q", metadata, lanes, lane, estimate.Window, estimate.Ceiling, estimate.DeliveredByteCount, estimate.Interval, estimate.WindowRoundTrip, estimate.Reason)
					if estimate.Window != estimate.Ceiling {
						t.Errorf("metadata=%t lanes=%d lane=%d old flight-limited delivery reduced the newly proved path: window=%d ceiling=%d", metadata, lanes, lane, estimate.Window, estimate.Ceiling)
					}
				}
			}
		}
	})
}

// A queued ordinary sample is not proof of propagation growth and must not
// discard valid low delivery evidence or bypass a fixed peer permission.
func TestWindowPacingQueuedPathObservationKeepsDeliveryClamp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			sequences, service := newWindowRefillProofFixture(t, 1, metadata)
			sequence := sequences[0]
			before := sequence.sendWindowEstimate(time.Now())
			if metadata {
				service.observeReceiverRoundTrip(2, 1210*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, time.Now())
			} else {
				service.observeRoundTrip(1210*time.Millisecond, 10*time.Millisecond, time.Now())
			}
			after := sequence.sendWindowEstimate(time.Now())
			if before.Window != before.Floor || after.Window != before.Window {
				t.Errorf("metadata=%t ordinary queue growth bypassed valid delivery: before=%+v after=%+v", metadata, before, after)
			}
		}
	})
}
