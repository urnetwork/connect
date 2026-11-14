// Forced receiver timing samples exercise shared window decisions independently
// of ACK delivery scheduling and the separately tested wire lifecycle.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The raw per-sequence floor must not override a measured network RTT, and
// actual receiver residence must not vanish when compression is removed.
func TestWindowPacingReceiverTimingOwnsWindowResidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		raw         time.Duration
		compression time.Duration
		want        time.Duration
	}{
		{name: "compression", raw: 10300 * time.Microsecond, compression: 10 * time.Millisecond, want: 10300 * time.Microsecond},
		{name: "application", raw: 25300 * time.Microsecond, compression: 0, want: 25300 * time.Microsecond},
		{name: "combined", raw: 25300 * time.Microsecond, compression: 10 * time.Millisecond, want: 25300 * time.Microsecond},
		{name: "early head", raw: 5300 * time.Microsecond, compression: 10 * time.Millisecond, want: 10300 * time.Microsecond},
	} {
		at := time.Unix(1700000000, 0)
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
			settings.TargetGoodputByteRate = 125000000
		})
		sequence.rttWindow.closeSendTime(uint64(at.Add(-100*time.Millisecond).UnixMilli()), at)
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(mib(48)),
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: uint32(test.compression / time.Microsecond),
		})
		sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
		sequence.windowPacer.service = newWindowPacingService(sequence.sendBufferSettings)
		sequence.windowPacer.service.observeReceiverRoundTrip(1, test.raw, 300*time.Microsecond, test.compression, at)
		estimate := sequence.sendWindowEstimate(at)
		t.Logf("%s raw=%s adjusted=%s effective=%s window=%d", test.name, test.raw, estimate.RoundTrip, estimate.WindowRoundTrip, estimate.Window)
		if estimate.RoundTrip != 300*time.Microsecond || estimate.WindowRoundTrip != test.want {
			t.Errorf("%s raw floor or unrelated compression replaced paired timing: adjusted=%s effective=%s want=300us/%s", test.name, estimate.RoundTrip, estimate.WindowRoundTrip, test.want)
		}
	}
}

// Count retirement refreshes observed RTT. Raising an unloaded baseline
// separately requires the exact physically drained probe tested below.
func TestWindowPacingReceiverTimingRetiresOldMinimum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.RttWindowSize = 4
		service := newWindowPacingService(settings)
		service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, time.Now())
		for i := 1; i <= settings.RttWindowSize; i++ {
			time.Sleep(10 * time.Millisecond)
			service.observeReceiverRoundTrip(uint64(i+1), 1210*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, time.Now())
			want := 300 * time.Microsecond
			if i == settings.RttWindowSize {
				want = 1200 * time.Millisecond
			}
			if got, _, _ := service.receiverWindowEstimate(time.Now()); got != want {
				t.Errorf("after %d new samples, shared RTT=%s want=%s", i, got, want)
			}
		}
	})
}

// The receiver's wait is removed before backlog comparison. A genuine
// network delay increase still provides backlog evidence with excess flight.
func TestWindowPacingReceiverTimingSeparatesQueueEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		service := newWindowPacingService(settings)
		service.observeReceiverRoundTrip(1, 101*time.Millisecond, time.Millisecond, 0, time.Now())
		service.sent, service.total = 250000, 100000
		if service.backlogged(1000000) {
			t.Error("receiver waiting alone was classified as a network backlog")
		}
		time.Sleep(time.Millisecond)
		service.observeReceiverRoundTrip(2, 201*time.Millisecond, 101*time.Millisecond, 0, time.Now())
		if !service.backlogged(1000000) {
			t.Error("excess physical flight with network queue delay lost backlog evidence")
		}
	})
}

// A raw drained probe may update legacy fallback, but cannot replace the
// recent metadata stream or make a receiver wait become network propagation.
func TestWindowPacingReceiverTimingSurvivesRawProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeReceiverRoundTrip(1, 25*time.Millisecond, time.Millisecond, 10*time.Millisecond, time.Now())
		sequenceId, messageId := NewId(), NewId()
		service.drained = true
		service.beginWrite(sequenceId, messageId, 1, time.Now(), false)
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, time.Now())
		service.finishWrite(sequenceId, messageId, true)
		if got := service.roundTrip(); got != time.Millisecond {
			t.Fatalf("confirmed raw probe overwrote receiver network evidence: %s", got)
		}
	})
}

// An older worker cannot restore a retired small sample after a later
// receiver-timed ACK has already advanced the shared history.
func TestWindowPacingReceiverTimingRejectsStaleApplication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.RttWindowSize = 2
		service := newWindowPacingService(settings)
		start := time.Now()
		service.observeReceiverRoundTrip(1, 2*time.Millisecond, time.Millisecond, time.Millisecond, start)
		time.Sleep(2 * time.Millisecond)
		service.observeReceiverRoundTrip(2, 101*time.Millisecond, 100*time.Millisecond, time.Millisecond, time.Now())
		time.Sleep(2 * time.Millisecond)
		service.observeReceiverRoundTrip(3, 101*time.Millisecond, 100*time.Millisecond, time.Millisecond, time.Now())
		service.observeReceiverRoundTrip(1, 2*time.Millisecond, time.Millisecond, time.Millisecond, start.Add(time.Millisecond))
		if got, _, _ := service.receiverWindowEstimate(time.Now()); got != 100*time.Millisecond {
			t.Fatalf("stale sample revived the old floor: %s", got)
		}
	})
}

// New timing explicitly replaces the raw drain inference while present; the
// synthetic legacy drain/retry fixtures remain separate fallback coverage.
func TestWindowPacingReceiverTimingNeedsNoRawDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		service := newWindowPacingService(settings)
		service.observeRoundTrip(time.Millisecond, 0, time.Now())
		for i := 0; i < 6; i++ {
			time.Sleep(10 * time.Millisecond)
			service.observeReceiverRoundTrip(1, 101*time.Millisecond, time.Millisecond, 0, time.Now())
		}
		service.pendingWrites = 1
		service.burstMeter.update(time.Now(), 1000, 1000000)
		waiter := &windowPacingWaiter{}
		wait, _ := service.admitBurst(time.Now(), 100, false, waiter)
		if wait > 0 || !service.drainUntil.IsZero() {
			t.Fatalf("raw receiver wait started a controlled drain despite measured RTT: wait=%s", wait)
		}
	})
}

// An unsampled sibling belongs to the already measured logical service; its
// cold local ring cannot hide the shared endpoint's receiver timing.
func TestWindowPacingReceiverTimingReachesUnsampledSibling(t *testing.T) {
	at := time.Unix(1700000000, 0)
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
		settings.TargetGoodputByteRate = 125000000
	})
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: uint32(mib(48)),
		ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
	})
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.windowPacer.service = newWindowPacingService(sequence.sendBufferSettings)
	sequence.windowPacer.service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	if sequence.rttWindow.estimate(at).Sampled() {
		t.Fatal("fixture accidentally supplied a local RTT sample")
	}
	estimate := sequence.sendWindowEstimate(at)
	if estimate.RoundTrip != 300*time.Microsecond || estimate.WindowRoundTrip != 10300*time.Microsecond || estimate.Reason == "no round trip samples" {
		t.Fatalf("cold sibling hid live shared timing: %+v", estimate)
	}
}
