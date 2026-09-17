// A constrained opening must be able to grow from independently measured
// serialization before its own flight limit supplies several RTTs of delivery.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// One real pacer admits a 2 MiB opening, confirmed physical writes establish
// its ownership, and explicit serialized replies measure a 125 MB/s service.
func newRetainedServiceGrowthFixture(t *testing.T, replies int) (*SendSequence, time.Time) {
	t.Helper()
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueMaxByteCount = 2 * 1024 * 1024
		settings.ResendQueueMinByteCount = 256 * 1024
		settings.ResendQueueBudget = NewTransferMemoryBudget(48 * 1024 * 1024)
		settings.TargetGoodputByteRate = 125000000
	})
	sequence.sequenceId = NewId()
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	service := newWindowPacingService(sequence.sendBufferSettings)
	pacer := &sequence.windowPacer
	pacer.service, pacer.serviceSequenceId = service, sequence.sequenceId
	pacer.rate = ByteCount(float64(sequence.sendBufferSettings.TargetGoodputByteRate) / goodputFactor)
	t.Cleanup(func() { pacer.close(); sequence.resendQueue.Clear() })
	start := time.Now()
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: 48 * 1024 * 1024,
		ackCompressTimeoutSet: true,
	})
	var messages [512]Id
	for i := range messages {
		messages[i] = NewId()
		if err := pacer.waitForServiceMessage(context.Background(), 4096, false, sequence.sequenceId, messages[i], uint64(i)); err != nil {
			t.Fatal(err)
		}
		service.finishWrite(sequence.sequenceId, messages[i], true)
	}
	if time.Since(start) >= 400*time.Millisecond || service.sent != 2*1024*1024 {
		t.Fatal("opening did not finish its paced writes before the first reply")
	}
	for i := range replies {
		at := start.Add(400*time.Millisecond + time.Duration(i+1)*32768*time.Nanosecond)
		time.Sleep(time.Until(at))
		sequence.rttWindow.closeSendTime(uint64(start.UnixMilli()), at)
		service.observeRoundTrip(time.Since(start), 0, at)
		service.acknowledgeWrite(sequence.sequenceId, messages[i], uint64(i), false, 0, at)
		service.observe(4096, at)
		pacer.serviceAcked += 4096
		// The coalescer has already published physical credit. The worker
		// separately retires cumulative logical bytes without double credit.
		sequence.observeAckedBytesWithServiceCredit(4096, 0, windowServiceAckCredit{}, at)
	}
	return sequence, time.Now()
}

// The physical serializer has been measured, but the 16.8 ms ACK train cannot
// fill an 800 ms cumulative-rate horizon while the opening limits flight.
func TestWindowRetainedMeasuredServiceCanGrowConstrainedFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, at := newRetainedServiceGrowthFixture(t, 512)
		estimate := sequence.sendWindowEstimate(at)
		if estimate.ServiceByteRate < 124000000 || !estimate.ServiceEstablished {
			t.Fatalf("fixture did not measure its physical service: %+v", estimate)
		}
		if estimate.Window < 40000000 || estimate.Window > 48*1024*1024 {
			t.Fatalf("healthy service could not grow the 2 MiB flight on a 400 ms path: %+v", estimate)
		}
	})
}

// One partial reply establishes no serialization pair and cannot authorize
// learning the peer's much larger receive permission.
func TestWindowRetainedSingleReplyCannotAuthorizeServiceGrowth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, at := newRetainedServiceGrowthFixture(t, 1)
		if estimate := sequence.sendWindowEstimate(at); estimate.Window != 2*1024*1024 || estimate.ServiceByteRate != 0 {
			t.Fatalf("one reply supplied invented capacity: %+v", estimate)
		}
	})
}

// Keep the affected actual-worker cells and original fixed warmup/throughput
// gates. Synthetic sampler evidence alone cannot establish timely convergence.
func TestWindowPathRetainedGrowthLongRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, compression := range []time.Duration{0, 10 * time.Millisecond} {
		for _, flows := range []int{1, 8} {
			checkWindowMismatchCell(t, windowPathCell{
				SendWindow: 48 * 1024 * 1024, ReceiveWindow: 48 * 1024 * 1024,
				RoundTrip: 400 * time.Millisecond, Compression: compression,
				Flows: flows, Rate: 125000000,
			})
		}
	}
}
