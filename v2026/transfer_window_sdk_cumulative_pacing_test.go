// Repeat affected constructor cells with the original SDK instrument, warmup
// and throughput gates. These runs distinguish cold pacing from service faults.
package connect

import (
	"testing"
	"time"
)

// Short default and explicit-H1 device/provider senders include both the
// observed cold-rate failures and the working eight-lane sibling controls.
func TestWindowPathRetainedSdkCumulativePacingShort(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	selected := 0
	for i := range profiles {
		profile := &profiles[i]
		if !profile.MobilePolicy || profile.Providing && profile.Name != "sdk-provider-default" && profile.Name != "sdk-provider-h1" {
			continue
		}
		selected++
		for _, flows := range []int{1, 8} {
			checkWindowSdkCell(t, windowPathCell{
				SenderProfile: profile, ReceiverProfile: &profiles[1],
				ProfileFixtureSha256: digest, RoundTrip: 300 * time.Microsecond,
				Flows: flows, RoundRobinOffer: true, Payload: 1280, Rate: 125000000,
			})
		}
	}
	if selected != 4 {
		t.Fatalf("selected %d constrained SDK senders, want four", selected)
	}
}

// The longer failures already reported positive service. Keep those exact
// cells separate so a cold-rate fix is not credited for an unverified cause.
func TestWindowPathRetainedSdkCumulativePacingLong(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	for i := range profiles {
		profile := &profiles[i]
		if profile.Name != "sdk-device-default" || !profile.MobilePolicy || profile.Providing {
			continue
		}
		for _, roundTrip := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
			checkWindowSdkCell(t, windowPathCell{
				SenderProfile: profile, ReceiverProfile: &profiles[1],
				ProfileFixtureSha256: digest, RoundTrip: roundTrip,
				Flows: 1, RoundRobinOffer: true, Payload: 1280, Rate: 125000000,
			})
		}
		return
	}
	t.Fatal("missing constrained default SDK device sender")
}
