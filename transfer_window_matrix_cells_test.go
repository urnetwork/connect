// Matrix construction is separate from traffic so coverage and total work can
// be checked without running a second copy of every physical path.
package connect

import (
	"testing"
	"time"
)

// Keep all feedback scales, including both short and deep-flight boundaries.
func windowDeterministicRoundTrips() []time.Duration {
	return []time.Duration{300 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
}

// A parity cover pairs each interior round trip with both flow counts and
// compression states. Both boundaries and explicit diagnostics keep full grids.
func windowDeterministicPerformanceCells(roundTrips []time.Duration, exhaustive bool) []windowPathCell {
	var cells []windowPathCell
	for roundTripIndex, roundTrip := range roundTrips {
		for flowIndex, flows := range []int{1, 8} {
			for compressionIndex, compression := range []time.Duration{0, 10 * time.Millisecond} {
				boundary := roundTrip == 300*time.Microsecond || roundTrip == 400*time.Millisecond
				if !exhaustive && !boundary && compressionIndex != (roundTripIndex+flowIndex)%2 {
					continue
				}
				cells = append(cells, windowPathCell{RoundTrip: roundTrip, Compression: compression,
					Flows: flows, Payload: 1280, Budget: mib(48), Rate: 125000000})
			}
		}
	}
	return cells
}

// All three-way profile/direction/round-trip/flow interactions fit a parity
// cover. Add every explicit h1 sender row and the constrained startup corners.
func windowSdkProfileCells(profiles []windowPathEndpointProfile, digest string) []windowPathCell {
	var cells []windowPathCell
	for i := range profiles {
		for direction, reverse := range []bool{false, true} {
			for roundTripIndex, roundTrip := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond, 400 * time.Millisecond} {
				for flowIndex, flows := range []int{1, 8} {
					profile := &profiles[i]
					startup := profile.MobilePolicy && (profile.Name == "sdk-provider-default" || !profile.Providing)
					boundary := reverse && (profile.ExplicitH1 || (startup && roundTripIndex != 1) ||
						(profile.MobilePolicy && profile.Name == "sdk-device-default" && !profile.Providing && flows == 1))
					if !boundary && flowIndex != (i+direction+roundTripIndex)%2 {
						continue
					}
					sender, receiver := &profiles[1], &profiles[i]
					if reverse {
						sender, receiver = receiver, sender
					}
					cells = append(cells, windowPathCell{SenderProfile: sender, ReceiverProfile: receiver,
						ProfileFixtureSha256: digest, RoundTrip: roundTrip, Flows: flows,
						RoundRobinOffer: true, Payload: 1280, Rate: 125000000})
				}
			}
		}
	}
	return cells
}

// Cover every three-way service interaction plus every four-way extreme
// corner. Every row keeps its original startup drain and measured interval.
func windowServicePerformanceCells() []windowPathCell {
	var cells []windowPathCell
	for rateIndex, rate := range []ByteCount{125000, 1250000, 12500000} {
		for roundTripIndex, roundTrip := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond, 400 * time.Millisecond} {
			for flowIndex, flows := range []int{1, 8} {
				for compressionIndex, compression := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond} {
					corner := rateIndex != 1 && roundTripIndex != 1 && compressionIndex != 1
					if !corner && flowIndex != (rateIndex+roundTripIndex+compressionIndex)%2 {
						continue
					}
					warmup := max(300*time.Millisecond+5*roundTrip, time.Duration(2*int64(mib(2))*int64(time.Second)/int64(rate))+5*roundTrip)
					cells = append(cells, windowPathCell{RoundTrip: roundTrip, Compression: compression,
						Flows: flows, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: rate, Warmup: warmup})
				}
			}
		}
	}
	return cells
}

// Bound expensive worker pairs directly, never by a host-dependent deadline.
func TestWindowPathMatrixWorkBounds(t *testing.T) {
	profiles, digest := windowSdkProfiles(t)
	for _, matrix := range []struct {
		name  string
		count int
		limit int
	}{
		{name: "deterministic", count: len(windowDeterministicPerformanceCells(windowDeterministicRoundTrips(), false)), limit: 22},
		{name: "sdk", count: len(windowSdkProfileCells(profiles, digest)), limit: 80},
		{name: "service", count: len(windowServicePerformanceCells()), limit: 35},
	} {
		if matrix.count > matrix.limit {
			t.Errorf("%s matrix has %d worker pairs, limit %d", matrix.name, matrix.count, matrix.limit)
		}
		t.Logf("%s matrix: %d worker pairs, limit %d", matrix.name, matrix.count, matrix.limit)
	}
}
