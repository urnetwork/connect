package connect

import (
	"testing"
	"time"
)

// While connected, the ICE agent of every peer connection wakes once per
// KeepAliveTimeout to send a consent check, and only then judges the
// DisconnectedTimeout and FailedTimeout windows against the last packet from
// the peer. So the keepalive also bounds how late a lost path is noticed: up
// to one keepalive past each window, 2 s at the default. CPUOPT2 raised it to
// 5 s, which noticed a lost path up to 3 s later.
func TestWebRtcIceKeepAliveKeepsLostPathDetectionWithinTwoSeconds(t *testing.T) {
	settings := DefaultWebRtcSettings()
	if settings.KeepAliveTimeout != 2*time.Second {
		t.Fatalf("ICE keepalive %s, expected 2 s", settings.KeepAliveTimeout)
	}
	for _, window := range []time.Duration{settings.DisconnectedTimeout, settings.FailedTimeout} {
		if window < settings.KeepAliveTimeout {
			t.Fatalf("a %s window is shorter than the %s keepalive that judges it", window, settings.KeepAliveTimeout)
		}
	}
}
