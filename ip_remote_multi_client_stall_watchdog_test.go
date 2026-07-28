package connect

import (
	"testing"
	"time"
)

// the whole point of the watchdog is that a stall is noticed on roughly its own
// timescale. Detecting at 3s is worth nothing if it is consulted every 15s,
// which is what made a stalled exit take 15-30s to recover on a device.
func TestSendStallPollTimeoutTracksTheStallTimeout(t *testing.T) {
	resizeTimeout := 15 * time.Second

	pollTimeout := sendStallPollTimeout(3*time.Second, resizeTimeout)

	// comfortably inside the stall timeout, so detection latency is bounded by
	// the timeout itself rather than by the resize cadence
	AssertEqual(t, pollTimeout < 3*time.Second, true)
	AssertEqual(t, pollTimeout < resizeTimeout, true)
}

// a very small timeout must not turn the watchdog into a busy loop
func TestSendStallPollTimeoutHasAFloor(t *testing.T) {
	pollTimeout := sendStallPollTimeout(1*time.Millisecond, 15*time.Second)

	AssertEqual(t, 250*time.Millisecond <= pollTimeout, true)
}

// disabled idles at the resize cadence rather than exiting, so turning stall
// detection back on from the developer menu takes effect without a reconnect
func TestSendStallPollTimeoutWhenDisabled(t *testing.T) {
	resizeTimeout := 15 * time.Second

	AssertEqual(t, sendStallPollTimeout(0, resizeTimeout), resizeTimeout)
	AssertEqual(t, sendStallPollTimeout(-1*time.Second, resizeTimeout), resizeTimeout)
}

// a bare window must be able to read its reliability settings -- the watchdog
// calls this on every pass, and the suite constructs windows without a parent
func TestWindowReliabilitySettingsBareWindow(t *testing.T) {
	window := &multiClientWindow{
		settings: &MultiClientSettings{SendStallTimeout: 3 * time.Second},
	}

	AssertEqual(t, window.reliabilitySettings().SendStallTimeout, 3*time.Second)
}

// the parent's runtime override wins, which is what makes the developer menu
// toggle able to switch the fix off against a live freeze
func TestWindowReliabilitySettingsUsesTheOverride(t *testing.T) {
	window := &multiClientWindow{
		settings: &MultiClientSettings{SendStallTimeout: 3 * time.Second},
		reliabilitySettingsFunc: func() *ReliabilitySettings {
			return &ReliabilitySettings{SendStallTimeout: 0}
		},
	}

	AssertEqual(t, window.reliabilitySettings().SendStallTimeout, time.Duration(0))
}
