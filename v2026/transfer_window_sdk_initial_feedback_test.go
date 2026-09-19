// Force the first feedback ordering behind the intermittent constrained-SDK
// collapse. All arms keep the original serializer, budgets and capacity gates.
package connect

import (
	"testing"
	"time"
)

// One application flow crosses the SDK's shared H1 lane configuration. Only
// the first receiver ACK is held; later feedback uses ordinary compression.
func checkWindowSdkInitialFeedback(t *testing.T, release time.Duration) {
	t.Helper()
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	for i := range profiles {
		profile := &profiles[i]
		if profile.Name == "sdk-device-h1" && profile.MobilePolicy && !profile.Providing {
			checkWindowSdkCell(t, windowPathCell{
				SenderProfile: profile, ReceiverProfile: &profiles[1],
				ProfileFixtureSha256: digest, RoundTrip: 400 * time.Millisecond,
				Flows: 1, RoundRobinOffer: true, Payload: 1280, Rate: 125000000,
				InitialLogicalAckRelease: release,
			})
			return
		}
	}
	t.Fatal("missing constrained SDK H1 constructor profile")
}

// Preserve the exact original cell independently of the broader SDK sweep.
func TestWindowPathSdkConstrainedLongWindow(t *testing.T) {
	checkWindowSdkInitialFeedback(t, 0)
}

// Release five milliseconds after forward propagation. The forced ACK phase
// later replaces measured service with a small reply across a feedback gap.
func TestWindowPathSdkConstrainedInitialFeedback(t *testing.T) {
	checkWindowSdkInitialFeedback(t, 205*time.Millisecond)
}

// A later first ACK is a control for the same serializer and lifetime policy.
func TestWindowPathSdkConstrainedLaterFeedbackControl(t *testing.T) {
	checkWindowSdkInitialFeedback(t, 220*time.Millisecond)
}
