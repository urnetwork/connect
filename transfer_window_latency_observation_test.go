package connect

import (
	"testing"
	"time"
)

// A window read is not a new path measurement. A quiet sibling's only ACK
// predates the path horizon and must not pin a busy sibling's fresh estimate.
func TestWindowLatencyMinimumDoesNotRenewExpiredAck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot bool
	}{
		{name: "admission"},
		{name: "snapshot", snapshot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := time.Unix(1700000000, 0)
			client, destination := &Client{}, NewId()
			quiet := newEstimatorFixture(t, func(settings *SendBufferSettings) {
				settings.DeliverySizedWindowScale = 2
				settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
				settings.WindowLatencyBudget = 50 * time.Millisecond
			})
			t.Cleanup(func() { quiet.resendQueue.Clear() })
			quiet.client, quiet.destination = client, destination
			quiet.rttWindow.observeRoundTrip(100*time.Millisecond, 0, 0, origin, false)
			quiet.sendWindowEstimate(origin)

			at := origin.Add(pathRoundTripMinimumHorizon + time.Second)
			if tc.snapshot {
				quiet.sendWindowSnapshot(at)
			} else {
				quiet.sendWindowEstimate(at)
			}
			busy := &SendSequence{client: client, destination: destination, sendBufferSettings: quiet.sendBufferSettings}
			if got := busy.observePathRoundTrip(500*time.Millisecond, at); got != 500*time.Millisecond {
				t.Fatalf("window read renewed an ACK from %s ago: minimum=%s, want 500ms", at.Sub(origin), got)
			}
		})
	}
}

// Polling must not spend a drain probe before the controller next admits data.
func TestWindowLatencySnapshotDoesNotStartProbe(t *testing.T) {
	origin := time.Unix(1700000000, 0)
	var windows [2]ByteCount
	for poll := range 2 {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueMaxByteCount = mib(46)
			settings.WindowLatencyBudget = 50 * time.Millisecond
		})
		t.Cleanup(func() { sequence.resendQueue.Clear() })
		sequence.client, sequence.destination = &Client{}, NewId()
		sequence.observePathRoundTrip(100*time.Millisecond, origin)
		busy := SendWindowEstimate{Window: mib(46), Floor: kib(256), DeliveryByteRate: 10000000}
		sequence.boundWindowByLatency(busy, origin)
		if poll != 0 {
			sequence.sendWindowSnapshot(origin.Add(pathRoundTripProbeInterval))
			if until := sequence.client.pathLatency(sequence.destination).probeUntil; !until.IsZero() {
				t.Errorf("a statistics snapshot started a probe until %s", until)
			}
		}
		at := origin.Add(pathRoundTripProbeInterval + pathRoundTripProbeDuration + time.Millisecond)
		windows[poll] = sequence.boundWindowByLatency(busy, at).Window
	}
	if windows[0] != kib(256) || windows[1] != windows[0] {
		t.Fatalf("snapshot changed next admission: unpolled=%d polled=%d, want floor=%d", windows[0], windows[1], kib(256))
	}
}
