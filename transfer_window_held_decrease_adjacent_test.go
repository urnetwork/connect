// Independent queue residence still authorizes real decreases, while both
// estimator paths and RTT-less siblings retain a healthy shared pace.
package connect

import (
	"testing"
	"time"
)

// The same real preoffered credit crosses the existing queue margin exactly.
// Permission changes never supply either numerator bytes or queue evidence.
func TestWindowPacingHeldDecreaseRequiresIndependentQueue(t *testing.T) {
	const frameBytes ByteCount = 2671
	for _, test := range []struct {
		name                          string
		budgeted, sibling             bool
		initialPermission, permission ByteCount
		wait, extra                   time.Duration
		queued                        bool
	}{
		{name: "exact-margin", initialPermission: kib(64), permission: kib(64), wait: 48 * time.Millisecond},
		{name: "one-nanosecond-queue", initialPermission: kib(64), permission: kib(64), wait: 48 * time.Millisecond, extra: time.Nanosecond, queued: true},
		{name: "real-slow-prefix", initialPermission: kib(64), permission: kib(64), queued: true},
		{name: "sized-growth", budgeted: true, initialPermission: kib(64), permission: mib(2), wait: 49 * time.Millisecond},
		{name: "sized-shrink", budgeted: true, initialPermission: mib(2), permission: kib(64), wait: 49 * time.Millisecond},
		{name: "fixed-sibling", sibling: true, initialPermission: kib(64), permission: kib(64), wait: 49 * time.Millisecond},
		{name: "sized-sibling", budgeted: true, sibling: true, initialPermission: mib(2), permission: kib(64), wait: 49 * time.Millisecond},
	} {
		service, at := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
		service.observeReceiverRoundTrip(0, 180*time.Millisecond, 130*time.Millisecond, 50*time.Millisecond, at.Add(time.Millisecond))
		at = at.Add(2 * time.Millisecond)
		service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
		fixture := newWindowReceiverCreditFixture(t, service, at, 50*time.Millisecond)
		consumer := fixture.sequence
		if test.sibling {
			consumer = newEstimatorFixture(t, nil)
			consumer.windowPacer.service = service
		}
		consumer.sendBufferSettings = DefaultSendBufferSettings()
		consumer.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
		if test.budgeted {
			consumer.sendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
			consumer.resendQueue = newResendQueue(consumer.sendBufferSettings.ResendQueueBudget, consumer.sendBufferSettings.ResendQueueMinByteCount)
		}
		consumer.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
		consumer.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(test.initialPermission)})
		if test.sibling {
			localRoundTrip := consumer.rttWindow.estimate(at)
			_, _, localReceiverSampled := consumer.rttWindow.receiverWindowEstimate(at)
			if localRoundTrip.Sampled() || localReceiverSampled || consumer.deliveredSampleCount() != 0 ||
				consumer.deliveredByteTotal != 0 {
				t.Fatalf("%s: sibling started with local timing or delivery: rtt=%+v receiver=%t bytes=%d", test.name, localRoundTrip, localReceiverSampled, consumer.deliveredByteTotal)
			}
		}
		t.Cleanup(func() {
			consumer.windowPacer.close()
			fixture.sequence.windowPacer.close()
			consumer.resendQueue.Clear()
			fixture.sequence.resendQueue.Clear()
			if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
				t.Errorf("%s: closing both owners retained physical flight", test.name)
			}
		})
		before := consumer.sendWindowEstimate(at)
		if before.ServiceByteRate != 12500000 || before.PacingByteRate != 13750000 || before.ServiceBacklogged || before.PacingDiscovery {
			t.Fatalf("%s: ordinary healthy admission did not grant fast pacing: %+v", test.name, before)
		}
		priorTotal := service.total
		offerAt := at.Add(time.Second)
		var originals [24]*sendItem
		for number := range originals {
			originals[number] = fixture.write(uint64(number), frameBytes, offerAt)
		}
		leftAt := offerAt.Add(100 * time.Millisecond)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[0], leftAt, 0))
		refill := fixture.write(24, frameBytes, leftAt)
		at = leftAt.Add(50*time.Millisecond + test.extra)
		ack := fixture.ack(originals[5], at, test.wait)
		ack.receiveWindowSet, ack.receiveWindowByteCount = true, uint32(test.permission)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		if test.sibling {
			consumer.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(test.permission)})
		}
		timing := service.roundTripEvidence(at)
		wantQueue := 50*time.Millisecond + test.extra - test.wait
		if timing.latest-timing.minimum != wantQueue || (wantQueue > 2*time.Millisecond) != test.queued ||
			service.total-priorTotal != 6*frameBytes || service.sent-service.total != 19*frameBytes ||
			service.drained || service.feedbackPending || service.receiverHeldPrefixAtNanos != 0 || refill.serviceCreditObserved {
			t.Fatalf("%s: independent physical queue, exact credit or refill provenance changed: timing=%+v", test.name, timing)
		}
		if test.wait > 0 && service.heldPacingRate != before.PacingByteRate {
			t.Fatalf("%s: small excess residence bypassed the intended held-rate gate", test.name)
		}
		if test.wait == 0 && service.heldPacingRate != 0 {
			t.Fatal("a real 50 ms queue failed to end the old held pace")
		}
		wantRate := ByteCount(float64(5*frameBytes) * float64(time.Second) / float64(50*time.Millisecond+test.extra))
		wantPace := before.PacingByteRate
		if test.queued {
			wantPace = ByteCount(.95 * float64(wantRate))
		}
		held := service.heldPacingRate
		snapshot := consumer.sendWindowSnapshot(at)
		if snapshot.ServiceByteRate != wantRate || snapshot.ServiceBacklogged != test.queued || snapshot.PacingByteRate != wantPace ||
			service.heldPacingRate != held {
			t.Errorf("%s: statistics changed the queue boundary or published a hold: %+v", test.name, snapshot)
		}
		after := consumer.sendWindowEstimate(at)
		if after.ServiceByteRate != wantRate || after.ServiceBacklogged != test.queued || after.PacingByteRate != wantPace ||
			after.Window != test.permission || service.heldPacingRate != wantPace || service.serviceHoldRate != wantRate {
			t.Errorf("%s: permission or caller path changed decrease qualification: %+v", test.name, after)
		}
		if test.budgeted && !test.sibling && after.WindowRoundTrip <= 0 {
			t.Errorf("%s: the owner did not reach the normal residence-backed sizing path", test.name)
		}
		if test.sibling {
			localRoundTrip := consumer.rttWindow.estimate(at)
			_, _, localReceiverSampled := consumer.rttWindow.receiverWindowEstimate(at)
			if localRoundTrip.Sampled() || localReceiverSampled || consumer.deliveredSampleCount() != 0 || consumer.deliveredByteTotal != 0 ||
				after.SampleCount != 0 || after.Sized || after.DeliveredByteCount != 0 || after.DeliveryByteRate != 0 {
				t.Errorf("%s: shared evidence manufactured local history: rtt=%+v receiver=%t estimate=%+v", test.name, localRoundTrip, localReceiverSampled, after)
			}
			if test.budgeted {
				_, sharedResidence, sharedSampled := service.receiverWindowEstimate(at)
				if !sharedSampled || after.WindowRoundTrip != sharedResidence || !after.ServiceSized {
					t.Errorf("%s: budgeted sibling lost qualified shared residence=%s sampled=%t estimate=%+v", test.name, sharedResidence, sharedSampled, after)
				}
			} else if after.WindowRoundTrip != 0 || after.ServiceSized {
				t.Errorf("%s: fixed sibling bypassed its early no-budget boundary: %+v", test.name, after)
			}
		}
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		if service.total != priorTotal+6*frameBytes || fixture.sequence.windowPacer.serviceAcked != 6*frameBytes || refill.serviceCreditObserved {
			t.Errorf("%s: repeated publication changed exact owner credit", test.name)
		}
	}
}
