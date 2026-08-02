package connect

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestBusyStaleDegradedScale pins the degraded-performance easing: while the
// host reports degraded (low power / weak network), the busy-stale window
// scales by DegradedLivenessScale, so a device that answers control pings
// slowly is not misdiagnosed as a dead peer; clearing degraded restores the
// tight window immediately.
func TestBusyStaleDegradedScale(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.CPingBusyStaleTimeout = 5 * time.Second
	settings.DegradedLivenessScale = 3.0

	channel := &multiClientChannel{
		settings:    settings,
		packetStats: &clientWindowStats{},
	}

	if timeout := channel.busyStaleTimeout(); timeout != 5*time.Second {
		t.Fatalf("normal window must be the configured timeout, got %v", timeout)
	}

	settings.DegradedMode.Store(true)
	if timeout := channel.busyStaleTimeout(); timeout != 15*time.Second {
		t.Fatalf("degraded window must scale 3x, got %v", timeout)
	}

	// staleness that would trip the probe on the normal window is tolerated
	// while degraded
	now := time.Now()
	channel.packetStats.lastSendTime = now
	channel.packetStats.lastReceiveAckTime = now.Add(-8 * time.Second)
	if channel.busyStale() {
		t.Fatalf("8s stale must be tolerated on the degraded (15s) window")
	}
	settings.DegradedMode.Store(false)
	if !channel.busyStale() {
		t.Fatalf("8s stale must trip the normal (5s) window")
	}

	// scale <= 1 means no scaling; nil DegradedMode never degrades
	settings.DegradedMode.Store(true)
	settings.DegradedLivenessScale = 1.0
	if timeout := channel.busyStaleTimeout(); timeout != 5*time.Second {
		t.Fatalf("scale 1.0 must not change the window, got %v", timeout)
	}
	settings.DegradedLivenessScale = 3.0
	settings.DegradedMode = nil
	if timeout := channel.busyStaleTimeout(); timeout != 5*time.Second {
		t.Fatalf("nil DegradedMode must never degrade, got %v", timeout)
	}

	// disabled probe stays disabled regardless of degraded state
	settings.DegradedMode = &atomic.Bool{}
	settings.DegradedMode.Store(true)
	settings.CPingBusyStaleTimeout = 0
	if timeout := channel.busyStaleTimeout(); timeout != 0 {
		t.Fatalf("disabled probe must stay disabled, got %v", timeout)
	}
}
