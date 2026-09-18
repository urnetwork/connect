// Mixed legacy/receiver feedback preserves arrival order and paired residence.
package connect

import (
	"testing"
	"time"
)

// A late legacy record cannot rewind the admission bound for a later paired
// sample, including after capacity replacement removes the original anchor.
func TestRttReceiverTimingRejectsMixedLegacyReordering(t *testing.T) {
	base := time.Unix(1700000000, 0)
	for _, oldCount := range []int{1, 4} {
		window := NewRttWindow(nil, 4, time.Minute, 2, time.Second, time.Millisecond, 8*time.Second)
		window.observeReceiverRoundTrip(300*time.Millisecond, 0, 0, base.Add(30*time.Millisecond))
		for range oldCount {
			window.closeSendTime(uint64(base.UnixMilli()), base.Add(10*time.Millisecond))
		}
		before := window.windowCount
		window.observeReceiverRoundTrip(time.Millisecond, 0, 0, base.Add(20*time.Millisecond))
		adjusted, _, set := window.receiverWindowEstimate(base.Add(30 * time.Millisecond))
		if window.windowCount != before || oldCount == 1 && (!set || adjusted != 300*time.Millisecond) || oldCount == 4 && set {
			t.Fatalf("legacyCount=%d admitted stale paired RTT: count=%d adjusted=%s set=%t", oldCount, window.windowCount, adjusted, set)
		}
		beforeSequence := window.nextSequence
		window.observeReceiverRoundTrip(500*time.Millisecond, 0, 0, base.Add(40*time.Millisecond))
		wantAdjusted := 500 * time.Millisecond
		if oldCount == 1 {
			wantAdjusted = 300 * time.Millisecond
		}
		if adjusted, _, set := window.receiverWindowEstimate(base.Add(40 * time.Millisecond)); !set || adjusted != wantAdjusted || window.nextSequence != beforeSequence+1 {
			t.Fatalf("legacyCount=%d suppressed newer valid evidence: adjusted=%s set=%t", oldCount, adjusted, set)
		}
	}
}

// Measured receiver residence already includes the actual wait. Sampling
// cadence and the local discovery horizon must not add it a second time.
func TestRttReceiverTimingBoundsSamplingWithPairedResidence(t *testing.T) {
	at := time.Now()
	settings := DefaultSendBufferSettings()
	settings.DeliverySizedWindowScale = 1
	sequence := &SendSequence{
		sendBufferSettings: settings,
		rttWindow:          NewRttWindow(nil, 4, time.Minute, 2, time.Second, time.Millisecond, 8*time.Second),
		deliveredBytes:     make([]deliveredBytesSample, deliveredBytesRingSize),
	}
	sequence.rttWindow.observeReceiverRoundTrip(800*time.Millisecond, 600*time.Millisecond, 500000, at)
	sequence.receiveAckCompressMicros.Store(500001)
	if got, want := sequence.deliveredBytesSampleInterval(), (1600*time.Millisecond+61)/62; got != want {
		t.Fatalf("sampling double-counted receiver delay: got=%s want=%s", got, want)
	}
	// The older peak ends 1s ago: outside actual 800ms residence and inside
	// the incorrect raw+compression 1.3s discovery horizon.
	for i, sample := range []deliveredBytesSample{
		{atNanos: at.Add(-1400 * time.Millisecond).UnixNano(), serviceTotal: 0},
		{atNanos: at.Add(-time.Second).UnixNano(), serviceTotal: 500000},
		{atNanos: at.UnixNano(), serviceTotal: 900000},
	} {
		sequence.deliveredBytes[i] = sample
	}
	sequence.deliveredBytesHead, sequence.deliveredBytesCount = 2, 3
	if rate, _, latest := sequence.deliveryServiceEstimate(2*time.Second, at, false); rate != 400000 || latest != 400000 {
		t.Fatalf("double residence retained stale peak: rate=%d latest=%d", rate, latest)
	}
	if got := sequence.rttWindow.estimate(at); got.Min != 800*time.Millisecond || got.Mean != 800*time.Millisecond {
		t.Fatalf("paired sampling changed raw recovery residence: %+v", got)
	}
}
