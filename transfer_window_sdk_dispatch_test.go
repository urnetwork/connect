// Hold the actual SDK duplex startup at fixed ACK phases, not until a lucky
// scheduler replay passes. All original direction, memory and loss gates apply.
package connect

import (
	"fmt"
	"testing"
	"time"
)

func TestWindowPathSdkDuplexShortDispatchPhases(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	if profiles[6].Name != "sdk-device-h1" || profiles[6].Providing || profiles[10].Name != "sdk-provider-h1" {
		t.Fatal("unexpected SDK constructor fixture layout")
	}
	for _, profile := range []struct {
		index int
		flows int
		first time.Duration
	}{
		{6, 1, 205 * time.Millisecond},
		{10, 8, 50100 * time.Microsecond},
	} {
		for _, offset := range []time.Duration{-1, 0, 2500 * time.Microsecond, 5 * time.Millisecond, 7500 * time.Microsecond} {
			release := time.Duration(0)
			if offset >= 0 {
				release = profile.first + offset
			}
			t.Run(fmt.Sprintf("%s/flows=%d/initial-ack=%s", profiles[profile.index].Name, profile.flows, release), func(t *testing.T) {
				checkWindowSdkCell(t, windowPathCell{
					SenderProfile: &profiles[1], ReceiverProfile: &profiles[profile.index], ProfileFixtureSha256: digest,
					Bidirectional: true, Upload: profile.flows == 8, RoundTrip: 300 * time.Microsecond,
					Flows: profile.flows, RoundRobinOffer: true, Payload: 1280, Rate: 125000000,
					InitialLogicalAckRelease: release,
				})
			})
		}
	}
}
