package connect

import (
	"testing"
	"time"
)

// Under a sustained transfer every recent acknowledgement waits behind the
// standing queue, so a minimum over the last few hundred samples is the queue
// itself. The path minimum is kept by time, not by sample count.
func TestPathRoundTripMinimumHoldsLowestRecentSample(t *testing.T) {
	var minimum pathRoundTripMinimum
	origin := time.Unix(1700000000, 0)
	if got := minimum.observe(100*time.Millisecond, origin); got != 100*time.Millisecond {
		t.Fatalf("first sample = %s, want 100ms", got)
	}
	for i := range 1000 {
		minimum.observe(2*time.Second, origin.Add(time.Duration(i)*time.Millisecond))
	}
	if got := minimum.observe(3*time.Second, origin.Add(pathRoundTripMinimumHorizon/2)); got != 100*time.Millisecond {
		t.Fatalf("queued samples raised the path minimum to %s", got)
	}
	if got := minimum.observe(0, origin.Add(pathRoundTripMinimumHorizon/2)); got != 100*time.Millisecond {
		t.Fatalf("an empty observation changed the path minimum to %s", got)
	}
	if got := minimum.observe(2*time.Second, origin.Add(pathRoundTripMinimumHorizon+time.Second)); got != 2*time.Second {
		t.Fatalf("an expired sample still bounds the path minimum: %s", got)
	}
}

func TestWindowLatencyBoundClampsQueueInflatedWindow(t *testing.T) {
	origin := time.Unix(1700000000, 0)
	const deliveryRate = ByteCount(10 * 1000 * 1000)
	newSequence := func(budget time.Duration) *SendSequence {
		settings := DefaultSendBufferSettings()
		settings.WindowLatencyBudget = budget
		return &SendSequence{client: &Client{}, destination: NewId(), sendBufferSettings: settings}
	}
	inflated := func() SendWindowEstimate {
		return SendWindowEstimate{Window: mib(46), Floor: kib(256), DeliveryByteRate: deliveryRate}
	}

	t.Run("bounded to two delivery round trips plus the budget", func(t *testing.T) {
		sequence := newSequence(50 * time.Millisecond)
		sequence.observePathRoundTrip(100*time.Millisecond, origin)
		got := sequence.boundWindowByLatency(inflated(), origin)
		if want := ByteCount(3 * 1000 * 1000); got.Window != want {
			t.Fatalf("window = %d, want %d", got.Window, want)
		}
		if got.Reason != "latency bound" {
			t.Fatalf("reason = %q", got.Reason)
		}
	})
	t.Run("a window already below the bound is unchanged", func(t *testing.T) {
		sequence := newSequence(50 * time.Millisecond)
		sequence.observePathRoundTrip(100*time.Millisecond, origin)
		estimate := inflated()
		estimate.Window = mib(1)
		if got := sequence.boundWindowByLatency(estimate, origin); got.Window != mib(1) {
			t.Fatalf("window = %d, want unchanged", got.Window)
		}
	})
	t.Run("never below the floor", func(t *testing.T) {
		sequence := newSequence(50 * time.Millisecond)
		sequence.observePathRoundTrip(time.Millisecond, origin)
		estimate := inflated()
		estimate.DeliveryByteRate = 1000
		if got := sequence.boundWindowByLatency(estimate, origin); got.Window != kib(256) {
			t.Fatalf("window = %d, want the floor", got.Window)
		}
	})
	t.Run("the last delivery rate bounds an estimate without one", func(t *testing.T) {
		sequence := newSequence(50 * time.Millisecond)
		sequence.observePathRoundTrip(100*time.Millisecond, origin)
		sequence.boundWindowByLatency(inflated(), origin)
		estimate := inflated()
		estimate.DeliveryByteRate = 0
		if got := sequence.boundWindowByLatency(estimate, origin.Add(2*time.Second)); got.Window != 3*1000*1000 {
			t.Fatalf("window = %d, want the bound from the recent rate", got.Window)
		}
		if got := sequence.boundWindowByLatency(estimate, origin.Add(pathDeliveryRateLifetime+time.Second)); got.Window != mib(46) {
			t.Fatalf("a stale delivery rate still bounded the window: %d", got.Window)
		}
	})
	t.Run("no evidence leaves the window alone", func(t *testing.T) {
		sequence := newSequence(50 * time.Millisecond)
		estimate := inflated()
		if got := sequence.boundWindowByLatency(estimate, origin); got.Window != mib(46) {
			t.Fatalf("window = %d without a path round trip", got.Window)
		}
	})
	t.Run("a zero budget disables the bound", func(t *testing.T) {
		sequence := newSequence(0)
		sequence.observePathRoundTrip(100*time.Millisecond, origin)
		if got := sequence.boundWindowByLatency(inflated(), origin); got.Window != mib(46) {
			t.Fatalf("window = %d with the bound disabled", got.Window)
		}
	})
	t.Run("sequences to one destination share the path", func(t *testing.T) {
		client := &Client{}
		destination := NewId()
		settings := DefaultSendBufferSettings()
		settings.WindowLatencyBudget = 50 * time.Millisecond
		quiet := &SendSequence{client: client, destination: destination, sendBufferSettings: settings}
		busy := &SendSequence{client: client, destination: destination, sendBufferSettings: settings}
		quiet.observePathRoundTrip(100*time.Millisecond, origin)
		if got := busy.observePathRoundTrip(2*time.Second, origin.Add(time.Second)); got != 100*time.Millisecond {
			t.Fatalf("a busy sequence did not see its sibling's path minimum: %s", got)
		}
	})
}

// A path that stays busy never shows its minimum again, so the window drains
// to its floor for a short probe once an interval passes without a sample near
// the minimum, and a sample near it postpones the next probe.
func TestWindowLatencyBoundProbesAPathThatStaysBusy(t *testing.T) {
	origin := time.Unix(1700000000, 0)
	settings := DefaultSendBufferSettings()
	settings.WindowLatencyBudget = 50 * time.Millisecond
	sequence := &SendSequence{client: &Client{}, destination: NewId(), sendBufferSettings: settings}
	busy := func() SendWindowEstimate {
		return SendWindowEstimate{Window: mib(46), Floor: kib(256), DeliveryByteRate: 10 * 1000 * 1000}
	}
	sequence.observePathRoundTrip(100*time.Millisecond, origin)
	if got := sequence.boundWindowByLatency(busy(), origin); got.Reason != "latency bound" {
		t.Fatalf("reason = %q before any probe is due", got.Reason)
	}
	// queued samples only, well above the minimum
	at := origin
	for at.Before(origin.Add(pathRoundTripProbeInterval - time.Second)) {
		at = at.Add(500 * time.Millisecond)
		sequence.observePathRoundTrip(300*time.Millisecond, at)
		if got := sequence.boundWindowByLatency(busy(), at); got.Reason != "latency bound" {
			t.Fatalf("probed early at %s", at.Sub(origin))
		}
	}
	at = origin.Add(pathRoundTripProbeInterval)
	sequence.observePathRoundTrip(300*time.Millisecond, at)
	got := sequence.boundWindowByLatency(busy(), at)
	if got.Reason != "latency probe" || got.Window != kib(256) {
		t.Fatalf("no drain after an interval without a near-minimum sample: %q %d", got.Reason, got.Window)
	}
	if got := sequence.boundWindowByLatency(busy(), at.Add(pathRoundTripProbeDuration-time.Millisecond)); got.Reason != "latency probe" {
		t.Fatalf("the drain ended early: %q", got.Reason)
	}
	after := at.Add(pathRoundTripProbeDuration + time.Millisecond)
	sequence.observePathRoundTrip(105*time.Millisecond, after)
	if got := sequence.boundWindowByLatency(busy(), after); got.Reason != "latency bound" {
		t.Fatalf("the drain did not end: %q", got.Reason)
	}
	// a near-minimum sample postpones the next probe by a full interval
	if got := sequence.boundWindowByLatency(busy(), after.Add(pathRoundTripProbeInterval-time.Second)); got.Reason != "latency bound" {
		t.Fatalf("probed again before an interval: %q", got.Reason)
	}
}

func TestWindowLatencyBoundOffByDefault(t *testing.T) {
	if budget := DefaultSendBufferSettings().WindowLatencyBudget; budget != 0 {
		t.Fatalf("default budget = %s, want off", budget)
	}
}
