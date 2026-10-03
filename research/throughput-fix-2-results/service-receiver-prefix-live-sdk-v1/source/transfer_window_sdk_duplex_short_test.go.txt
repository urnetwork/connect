// Isolate the retained device's short duplex cell, where reverse ACK traffic
// shares the serializer with a bounded opposing data flight.
package connect

import (
	"testing"
	"time"
)

// Each direction must retain the original SDK per-direction throughput gate;
// summing both directions cannot hide a slow device upload.
func TestWindowPathSdkRetainedDeviceDuplexShort(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	selected := 0
	for i := range profiles {
		profile := &profiles[i]
		if profile.Name != "sdk-device-h1" || !profile.MobilePolicy {
			continue
		}
		selected++
		checkWindowSdkCell(t, windowPathCell{
			SenderProfile: &profiles[1], ReceiverProfile: profile,
			ProfileFixtureSha256: digest, Bidirectional: true,
			RoundTrip: 300 * time.Microsecond, Flows: 1, RoundRobinOffer: true,
			Payload: 1280, Rate: 125000000,
		})
	}
	if selected != 2 {
		t.Fatalf("selected %d retained H1 device profiles, want both provider roles", selected)
	}
}
