package connect

import (
	"testing"
	"time"
)

// While connected, the ICE agent of every peer connection wakes and sends a
// consent check once per KeepAliveTimeout. Five seconds is the RFC 7675
// consent cadence, and it still fits six checks into the 30 s disconnect and
// failure windows, so a live peer that loses a few checks stays connected
// while each peer connection wakes 2.5 times less often than at 2 s.
func TestWebRtcIceKeepAliveIsTheConsentCadence(t *testing.T) {
	settings := DefaultWebRtcSettings()
	if settings.KeepAliveTimeout != 5*time.Second {
		t.Fatalf("ICE keepalive %s, expected 5 s", settings.KeepAliveTimeout)
	}
	for _, window := range []time.Duration{settings.DisconnectedTimeout, settings.FailedTimeout} {
		if window < 6*settings.KeepAliveTimeout {
			t.Fatalf("a %s window holds fewer than six %s keepalives", window, settings.KeepAliveTimeout)
		}
	}
}
