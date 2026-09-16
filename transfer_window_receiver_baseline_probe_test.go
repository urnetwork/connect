// An exact, confirmed empty-flight reply distinguishes path changes from
// carrier queueing and receiver delay without borrowing another head's timing.
package connect

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// A real growth of unloaded propagation is certified by an empty-flight
// first write and its exact receiver-timed ACK. Raw recovery still includes
// the receiver's residence, independently of either confirmation ordering.
func TestWindowPacingReceiverTimingConfirmedProbeRefreshesBaseline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, beforeWrite := range []bool{false, true} {
			for _, succeeds := range []bool{false, true} {
				sequence, item := newReceiverTimingSendTestSequence()
				sequence.sequenceId = NewId()
				service := newWindowPacingService(DefaultSendBufferSettings())
				sequence.windowPacer.service = service
				item.pacingByteCount, item.pacingBurst = 1000, 1
				service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
				service.drained = true
				service.beginWrite(sequence.sequenceId, item.messageId, item.sequenceNumber, time.Now(), false)
				confirm := func() {
					var writeErr error
					if !succeeds {
						writeErr = errors.New("synthetic physical refusal")
					}
					sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, writeErr)
					service.finishWrite(sequence.sequenceId, item.messageId, succeeds)
				}
				if !beforeWrite {
					confirm()
				}
				time.Sleep(1210 * time.Millisecond)
				ack := receiveAckMessage{
					messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true},
					receivedAtNanos:     sequence.client.feedbackArrivalNanos(time.Now()),
					receiverAckDelaySet: true, receiverAckDelayMicros: 10000,
					ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
				}
				sequence.observeReceiverAckRtt(ack)
				service.acknowledgeWrite(sequence.sequenceId, item.messageId, item.sequenceNumber, false, 10*time.Millisecond, time.Now())
				if beforeWrite {
					confirm()
				}
				want := 300 * time.Microsecond
				if succeeds {
					want = 1200 * time.Millisecond
				}
				got := service.roundTripEvidence(time.Now()).minimum
				t.Logf("ACK-before-write=%t succeeds=%t baseline=%s want=%s", beforeWrite, succeeds, got, want)
				if got != want {
					t.Errorf("ACK-before-write=%t succeeds=%t physical proof did not own baseline: got=%s want=%s", beforeWrite, succeeds, got, want)
				}
				if succeeds && sequence.rttWindow.estimate(time.Now()).Min != 1210*time.Millisecond {
					t.Error("paired baseline changed raw recovery residence")
				}
			}
		}
	})
}

// Subtracting receiver delay does not remove a standing carrier queue.
// Measured metadata must still permit the existing bounded empty-flight test;
// the next physically confirmed probe decides whether propagation changed.
func TestWindowPacingReceiverTimingQueueStillPermitsDrainProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
		for i := range 6 {
			time.Sleep(10 * time.Millisecond)
			service.observeReceiverRoundTrip(uint64(i+2), 50300*time.Microsecond, 40300*time.Microsecond, 10*time.Millisecond, time.Now())
		}
		service.pendingWrites = 1
		service.burstMeter.update(time.Now(), 1000, 1000000)
		wait, _ := service.admitBurst(time.Now(), 100, false, &windowPacingWaiter{})
		if wait <= 0 || service.drainUntil.IsZero() {
			t.Errorf("carrier queue disabled physical baseline probe: wait=%s", wait)
		}
	})
}

// A later head proves the earlier flight delivered, but its receiver delay
// belongs to a different physical write. It cannot certify the probe's RTT.
func TestWindowPacingReceiverTimingCoveringHeadCannotSupplyProbeDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, first := newReceiverTimingSendTestSequence()
		sequence.sequenceId = NewId()
		service := newWindowPacingService(DefaultSendBufferSettings())
		sequence.windowPacer.service = service
		first.pacingByteCount, first.pacingBurst = 1000, 1
		service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
		service.drained = true
		service.beginWrite(sequence.sequenceId, first.messageId, first.sequenceNumber, time.Now(), false)
		sequence.finishReceiverRttWrite(first, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
		service.finishWrite(sequence.sequenceId, first.messageId, true)
		time.Sleep(10 * time.Millisecond)
		second := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2}, sendTime: time.Now(), sendCount: 1, expectsAck: true, pacingByteCount: 1000, pacingBurst: 2}
		sequence.resendQueue.Add(second)
		sequence.beginReceiverRttWrite(second, false)
		service.beginWrite(sequence.sequenceId, second.messageId, second.sequenceNumber, time.Now(), false)
		sequence.finishReceiverRttWrite(second, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
		service.finishWrite(sequence.sequenceId, second.messageId, true)
		time.Sleep(1210 * time.Millisecond)
		ack := receiveAckMessage{messageId: second.messageId, tag: sequenceTag{sendTime: uint64(second.sendTime.UnixMilli()), set: true}, receivedAtNanos: sequence.client.feedbackArrivalNanos(time.Now()), receiverAckDelaySet: true, receiverAckDelayMicros: 10000, ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000}
		sequence.observeReceiverAckRtt(ack)
		service.acknowledgeWrite(sequence.sequenceId, second.messageId, second.sequenceNumber, false, 10*time.Millisecond, time.Now())
		if got := service.roundTripEvidence(time.Now()).minimum; got != 300*time.Microsecond {
			t.Errorf("different head's delay certified the old probe: %s", got)
		}
		if !service.roundTripProbe.sentAt.IsZero() {
			t.Error("covering head failed to complete ordinary raw drain proof")
		}
	})
}

// Count or age retirement changes observed residence, not the unloaded path
// baseline. A larger window may cover that residence, but must not expand the
// byte burst or remove the queue's pacing drain margin.
func TestWindowPacingReceiverTimingQueuedExpiryDoesNotGrowBurst(t *testing.T) {
	for _, byAge := range []bool{false, true} {
		at := time.Unix(1700000000, 0)
		settings := DefaultSendBufferSettings()
		service := newWindowPacingService(settings)
		service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
		now := at.Add(time.Second)
		if byAge {
			now = at.Add(settings.RttWindowTimeout + time.Second)
		}
		count := 1
		if !byAge {
			count = settings.RttWindowSize
		}
		for i := range count {
			service.observeReceiverRoundTrip(uint64(i+1), 660300*time.Microsecond, 650300*time.Microsecond, 10*time.Millisecond, now)
		}
		for i := range 101 {
			bytes := []ByteCount{125000, 125000, 250000, 0}[i%4]
			service.observe(bytes, now.Add(time.Duration(i-100)*10*time.Millisecond))
		}
		service.sent = service.total + 8128750
		rate, _, _ := service.measured(time.Second, now)
		backlog := service.backloggedAt(rate, now)
		paced := windowPacingRate(SendWindowEstimate{ServiceByteRate: rate, ServiceEstablished: true, ServiceBacklogged: backlog}, 125000000)
		waiter := &windowPacingWaiter{}
		service.reserve(now, 1000, paced, rate, 125000000, 0, false, waiter)
		observed, residence, set := service.receiverWindowEstimate(now)
		baseline := service.roundTripEvidence(now).minimum
		t.Logf("age=%t baseline=%s observed=%s residence=%s rate=%d paced=%d burst=%d interval=%s", byAge, baseline, observed, residence, rate, paced, service.burstMeter.limit, service.burstEstimateTime)
		if !set || observed != 650300*time.Microsecond || residence != 660300*time.Microsecond {
			t.Errorf("age=%t did not retain actual observed receiver tuple", byAge)
		}
		if baseline != 300*time.Microsecond || rate != 12500000 || !backlog || paced > 12500000 || service.burstMeter.limit > 125000 || service.burstEstimateTime != 10*time.Millisecond {
			t.Errorf("age=%t queued observations grew physical pacing allowance", byAge)
		}
	}
}
