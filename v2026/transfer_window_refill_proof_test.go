// A newly proved path length invalidates older delivery candidates while
// retaining learned capacity and obeying current hard permission limits.
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

// Every logical checkpoint contributes exactly 4 KiB per 50 ms. This known
// rate remains observable even when history eligibility or shared service
// determines the selected candidate and effective window.
func assertWindowRefillLogicalRate(t *testing.T, estimate SendWindowEstimate) {
	t.Helper()
	if estimate.Interval < 2*estimate.WindowRoundTrip || estimate.Interval%(50*time.Millisecond) != 0 || estimate.DeliveredByteCount != 4096*ByteCount(estimate.Interval/(50*time.Millisecond)) {
		t.Fatalf("logical sizing lost its exact 4 KiB per 50 ms interval: %+v", estimate)
	}
}

// A confirmed longer path invalidates every sibling's older cumulative
// interval. Independent service can grow the window before fresh logical
// delivery qualifies again, without turning old delivery into current proof.
func TestWindowPacingNewPathProofRequiresFreshCumulativeDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, metadata := range []bool{false, true} {
			for _, lanes := range []int{1, 8} {
				sequences, service := newWindowRefillProofFixture(t, lanes, metadata)
				before := make([]SendWindowEstimate, lanes)
				for lane, sequence := range sequences {
					before[lane] = sequence.sendWindowEstimate(time.Now())
					if !before[lane].Sized || !before[lane].ServiceSized || before[lane].Window != before[lane].Initial {
						t.Fatalf("metadata=%t lanes=%d lane=%d fixture has no qualified old delivery: %+v", metadata, lanes, lane, before[lane])
					}
					assertWindowRefillLogicalRate(t, before[lane])
				}
				at := completeWindowRefillProbe(t, sequences[0], service, metadata, 1210*time.Millisecond)
				wantRoundTrip := 1210 * time.Millisecond
				if metadata {
					wantRoundTrip -= 10 * time.Millisecond
				}
				wantWindow := ByteCount(30500000)
				if metadata {
					wantWindow = 30250000
				}
				if service.windowDeliveryStep() != at.UnixNano() || service.roundTripEvidence(at).minimum != wantRoundTrip {
					t.Fatalf("metadata=%t lanes=%d exact physical proof did not establish the new path: step=%d timing=%+v", metadata, lanes, service.windowDeliveryStep(), service.roundTripEvidence(at))
				}
				for lane, sequence := range sequences {
					estimate := sequence.sendWindowEstimate(at)
					t.Logf("metadata=%t lanes=%d lane=%d window=%d ceiling=%d delivered=%d span=%s residence=%s reason=%q", metadata, lanes, lane, estimate.Window, estimate.Ceiling, estimate.DeliveredByteCount, estimate.Interval, estimate.WindowRoundTrip, estimate.Reason)
					if estimate.Sized || !estimate.ServiceSized || estimate.Window != wantWindow || estimate.CandidateWindow != wantWindow || estimate.LearnedWindow != wantWindow || estimate.Ceiling != before[lane].Ceiling {
						t.Errorf("metadata=%t lanes=%d lane=%d old cumulative delivery qualified or independent service growth changed: %+v", metadata, lanes, lane, estimate)
					}
					if estimate.RoundTrip != wantRoundTrip || estimate.WindowRoundTrip != wantRoundTrip+10*time.Millisecond {
						t.Errorf("metadata=%t lanes=%d lane=%d sibling lost the confirmed path: %+v", metadata, lanes, lane, estimate)
					}
					assertWindowRefillLogicalRate(t, estimate)
				}
				for range 80 {
					time.Sleep(50 * time.Millisecond)
					for _, sequence := range sequences {
						sequence.observeAckedBytesWithServiceCredit(4096, 0, windowServiceAckCredit{}, time.Now())
					}
				}
				for lane, sequence := range sequences {
					estimate := sequence.sendWindowEstimate(time.Now())
					if !estimate.Sized || !estimate.ServiceSized || estimate.CandidateWindow != wantWindow || estimate.Window != wantWindow || estimate.LearnedWindow != wantWindow || service.windowDeliveryStep() != at.UnixNano() {
						t.Errorf("metadata=%t lanes=%d lane=%d fresh delivery failed to qualify without shrinking: %+v", metadata, lanes, lane, estimate)
					}
					assertWindowRefillLogicalRate(t, estimate)
				}
			}
		}
	})
}

// Ordinary queued timing cannot change the propagation baseline, invalidate
// a qualified delivery candidate, or alter retained capacity and permission.
func TestWindowPacingQueuedPathObservationKeepsDeliveryCandidate(t *testing.T) {
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
			if !before.Sized || !after.Sized || !before.ServiceSized || !after.ServiceSized || after.CandidateWindow != before.CandidateWindow || after.Window != before.Window || after.LearnedWindow != before.LearnedWindow || after.Ceiling != before.Ceiling {
				t.Errorf("metadata=%t ordinary queue growth bypassed valid delivery: before=%+v after=%+v", metadata, before, after)
			}
			assertWindowRefillLogicalRate(t, before)
			assertWindowRefillLogicalRate(t, after)
			if service.windowDeliveryStep() != 0 || after.RoundTrip != before.RoundTrip || after.WindowRoundTrip != before.WindowRoundTrip {
				t.Errorf("metadata=%t queued observation changed the unproved path: step=%d before=%+v after=%+v", metadata, service.windowDeliveryStep(), before, after)
			}
		}
	})
}
