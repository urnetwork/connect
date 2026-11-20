package connect

import (
	"testing"
)

// The windows of an iOS connection as the 32 MiB tier sizes them: a quality
// window of five exits, a speed window of one, and the limited window of two
// that adds its availability only. Every monitor is trying (a non-zero target).
func mergedStallTestMonitors() (merged *MergedMultiClientMonitor, quality *RemoteUserNatMultiClientMonitor, speed *RemoteUserNatMultiClientMonitor, limited *RemoteUserNatMultiClientMonitor) {
	quality = NewRemoteUserNatMultiClientMonitorWithDefaults()
	speed = NewRemoteUserNatMultiClientMonitorWithDefaults()
	limited = NewRemoteUserNatMultiClientMonitorWithDefaults()
	quality.AddWindowExpandEvent(true, 5, false)
	speed.AddWindowExpandEvent(true, 1, false)
	limited.AddWindowExpandEvent(true, 2, false)
	merged = newMergedMultiClientMonitorWithAvailability(
		[]MultiClientMonitor{quality, speed},
		[]MultiClientMonitor{limited},
	)
	return
}

// The speed window's one exit loses its carrier while the quality window's
// five carry traffic: the connection is not platform-unreachable, and no app
// may offer its "Can't reach URnetwork" prompt for it. The merged reason took
// the sharpest window's, so that one exit turned the whole connection.
func TestLoneSpeedExitDropDoesNotTurnConnectionPlatformUnreachable(t *testing.T) {
	merged, quality, speed, _ := mergedStallTestMonitors()
	quality.SetStallStatus(WindowStallEvaluating, false)
	speed.SetStallStatus(WindowStallPlatformUnreachable, false)
	if reason := merged.WindowExpandEvent().Reason; reason == WindowStallPlatformUnreachable {
		t.Fatalf("one trying window's platform-unreachable turned the merged reason %q while the quality window was trying", reason)
	}

	// every trying window unreachable is the connection unreachable
	quality.SetStallStatus(WindowStallPlatformUnreachable, false)
	if reason := merged.WindowExpandEvent().Reason; reason != WindowStallPlatformUnreachable {
		t.Fatalf("every trying window unreachable merged to %q", reason)
	}
}

// The limited window's two exits add availability only: their diagnosis never
// reaches the merged reason, alone or with one normal window.
func TestLimitedWindowDoesNotTurnConnectionPlatformUnreachable(t *testing.T) {
	merged, quality, speed, limited := mergedStallTestMonitors()
	quality.SetStallStatus(WindowStallEvaluating, false)
	speed.SetStallStatus(WindowStallEvaluating, false)
	limited.SetStallStatus(WindowStallPlatformUnreachable, false)
	if reason := merged.WindowExpandEvent().Reason; reason == WindowStallPlatformUnreachable {
		t.Fatalf("the limited window alone turned the merged reason %q", reason)
	}
	speed.SetStallStatus(WindowStallPlatformUnreachable, false)
	if reason := merged.WindowExpandEvent().Reason; reason == WindowStallPlatformUnreachable {
		t.Fatalf("the limited window and the speed window turned the merged reason %q while the quality window was trying", reason)
	}
}

// A disabled window (target 0 under a fixed window profile) neither vetoes the
// turn nor makes it: a Fixed IP quality window that is unreachable is the
// connection unreachable.
func TestDisabledWindowDoesNotVetoPlatformUnreachable(t *testing.T) {
	quality := NewRemoteUserNatMultiClientMonitorWithDefaults()
	speed := NewRemoteUserNatMultiClientMonitorWithDefaults()
	quality.AddWindowExpandEvent(false, 1, false)
	speed.AddWindowExpandEvent(false, 0, false)
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{quality, speed})
	quality.SetStallStatus(WindowStallPlatformUnreachable, false)
	speed.SetStallStatus(WindowStallEvaluating, false)
	if reason := merged.WindowExpandEvent().Reason; reason != WindowStallPlatformUnreachable {
		t.Fatalf("the only trying window unreachable merged to %q", reason)
	}
}
