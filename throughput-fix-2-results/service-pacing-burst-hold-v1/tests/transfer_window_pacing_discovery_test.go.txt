// Discovery and the held pace are the two directions of the same rule: before
// a queue has been observed the pacer's own release rate is not evidence of
// capacity, and after admission has granted a pace only congestion may lower
// it. These cells prove both against the pure rate function and the closed
// loop, including the capacity cases where the pace must still fall.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// On a path that has never queued, measured service is what the pacer itself
// released one residence earlier, so following it throttles the admitted
// window to an additive crawl. The floor releases that window once per
// residence instead, and the target still caps it.
func TestWindowPacingDiscoveryReleasesRetainedWindow(t *testing.T) {
	for _, test := range []struct {
		name     string
		estimate SendWindowEstimate
		want     ByteCount
	}{
		{
			name:     "crawl",
			estimate: SendWindowEstimate{Window: 7010478, WindowRoundTrip: 410 * time.Millisecond, Initial: 524288, ServiceByteRate: 1303889, ServiceEstablished: true},
			want:     1434277,
		},
		{
			name:     "discovery",
			estimate: SendWindowEstimate{Window: 7010478, WindowRoundTrip: 410 * time.Millisecond, Initial: 524288, ServiceByteRate: 1303889, ServiceEstablished: true, PacingDiscovery: true},
			want:     ByteCount(float64(7010478) / (410 * time.Millisecond).Seconds()),
		},
		{
			// Flight-based backlog is not an observed queue. Only the service's
			// own queue evidence ends discovery.
			name:     "discovery backlogged",
			estimate: SendWindowEstimate{Window: 7010478, WindowRoundTrip: 410 * time.Millisecond, Initial: 524288, ServiceByteRate: 1303889, ServiceEstablished: true, ServiceBacklogged: true, PacingDiscovery: true},
			want:     ByteCount(float64(7010478) / (410 * time.Millisecond).Seconds()),
		},
		{
			name:     "discovery capped",
			estimate: SendWindowEstimate{Window: 48 * 1024 * 1024, WindowRoundTrip: 110 * time.Millisecond, Initial: 524288, ServiceByteRate: 12500000, ServiceEstablished: true, PacingDiscovery: true},
			want:     147928994,
		},
		{
			name:     "discovery without residence",
			estimate: SendWindowEstimate{Window: 7010478, Initial: 524288, ServiceByteRate: 1303889, ServiceEstablished: true, PacingDiscovery: true},
			want:     1434277,
		},
		{
			name:     "discovery before service",
			estimate: SendWindowEstimate{Window: 1069244, WindowRoundTrip: 410 * time.Millisecond, Initial: 524288, PacingDiscovery: true},
			want:     ByteCount(float64(1069244) / (410 * time.Millisecond).Seconds()),
		},
	} {
		t.Logf("case: %s", test.name)
		if got := windowPacingRate(test.estimate, 147928994); got != test.want {
			t.Errorf("rate=%d want=%d", got, test.want)
		}
	}
}

// A smaller peer permission or a window-limited interval lowers measured
// service without lowering the path's capacity. The pace granted by admission
// is held until a backlogged read proves a queue.
func TestWindowPacingHeldRateFallsOnlyWithCongestion(t *testing.T) {
	for _, test := range []struct {
		name     string
		estimate SendWindowEstimate
		want     ByteCount
	}{
		{
			name:     "held",
			estimate: SendWindowEstimate{WindowRoundTrip: 150 * time.Millisecond, Initial: 2 * 1024 * 1024, ServiceByteRate: 575000, ServiceEstablished: true, PacingHeldByteRate: 13750000},
			want:     13750000,
		},
		{
			name:     "backlogged",
			estimate: SendWindowEstimate{WindowRoundTrip: 150 * time.Millisecond, Initial: 2 * 1024 * 1024, ServiceByteRate: 575000, ServiceEstablished: true, ServiceBacklogged: true, PacingHeldByteRate: 13750000},
			want:     546250,
		},
		{
			name:     "held above target",
			estimate: SendWindowEstimate{WindowRoundTrip: 150 * time.Millisecond, Initial: 2 * 1024 * 1024, ServiceByteRate: 575000, ServiceEstablished: true, PacingHeldByteRate: 200000000},
			want:     147928994,
		},
		{
			name:     "held before service",
			estimate: SendWindowEstimate{WindowRoundTrip: 150 * time.Millisecond, Initial: 2 * 1024 * 1024, PacingHeldByteRate: 20000000},
			want:     20000000,
		},
		{
			// A hold below the opening bootstrap cannot slow the first window.
			name:     "held below bootstrap",
			estimate: SendWindowEstimate{WindowRoundTrip: 100 * time.Millisecond, Initial: 2 * 1024 * 1024, PacingHeldByteRate: 1000},
			want:     ByteCount(float64(2*1024*1024) / (100 * time.Millisecond).Seconds()),
		},
	} {
		t.Logf("case: %s", test.name)
		if got := windowPacingRate(test.estimate, 147928994); got != test.want {
			t.Errorf("rate=%d want=%d", got, test.want)
		}
	}
}

// The grow-only window of a constrained SDK device sender must reach its
// permission and be released within a few residences. Without the discovery
// floor the pacer follows its own previous release and needs tens of round
// trips to fill a seven-megabyte window.
func TestWindowPathDiscoveryFillsRetainedWindow(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	var sender *windowPathEndpointProfile
	for i := range profiles {
		profile := &profiles[i]
		if profile.Name == "sdk-device-default" && profile.MobilePolicy && !profile.Providing {
			sender = profile
			break
		}
	}
	if sender == nil {
		t.Fatal("missing constrained default SDK device sender")
	}
	receiver := &profiles[1]
	limit := sender.windowLimit(receiver)
	for _, roundTrip := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
		residence := roundTrip + 10*time.Millisecond
		windowRate := float64(limit) / residence.Seconds()
		synctest.Test(t, func(t *testing.T) {
			reading := measureWindowPathCell(t, windowPathCell{
				SenderProfile: sender, ReceiverProfile: receiver, ProfileFixtureSha256: digest,
				Arm: "delivery", Drop: true, RoundTrip: roundTrip, Flows: 1,
				RoundRobinOffer: true, Payload: 1280, Rate: 125000000, Warmup: 8 * residence,
			}, residence)
			logWindowServiceReading(t, reading)
			t.Logf("discovery rtt=%s Mb/s=%.6f pace=%d window=%d discovering=%t drops=%d queued-bytes=%d resends=%d",
				roundTrip, reading.Mbps, reading.Window.PacingByteRate, reading.Window.Window,
				reading.Window.PacingDiscovery, reading.RelayDrops, reading.MaxRelayQueuedBytes,
				reading.Recovery.TimeoutResendWriteCount)
			if float64(reading.Window.PacingByteRate) < .9*windowRate {
				t.Errorf("discovery left the retained window unreleased: pace=%d want=%.0f", reading.Window.PacingByteRate, windowRate)
			}
			if reading.Mbps < .85*windowRate*8/1e6 {
				t.Errorf("discovery did not fill the window within eight residences: %.6f < %.6f Mb/s", reading.Mbps, .85*windowRate*8/1e6)
			}
			if reading.RelayDrops != 0 {
				t.Errorf("discovery dropped traffic: %d", reading.RelayDrops)
			}
			if reading.MaxRelayQueuedBytes > int64(limit) {
				t.Errorf("discovery queued more than one window: %d > %d", reading.MaxRelayQueuedBytes, limit)
			}
			if reading.Window.Window < limit {
				t.Errorf("window did not reach its permission: %d < %d", reading.Window.Window, limit)
			}
		})
	}
}

// The floor releases the admitted window, not more than the path can carry.
// A link slower than that window must observe its queue, end discovery, and
// settle at the serialization rate without dropping.
func TestWindowPathDiscoveryStopsAtCapacity(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	var sender *windowPathEndpointProfile
	for i := range profiles {
		profile := &profiles[i]
		if profile.Name == "sdk-device-default" && profile.MobilePolicy && !profile.Providing {
			sender = profile
			break
		}
	}
	if sender == nil {
		t.Fatal("missing constrained default SDK device sender")
	}
	receiver := &profiles[1]
	limit := sender.windowLimit(receiver)
	roundTrip := 400 * time.Millisecond
	residence := roundTrip + 10*time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		reading := measureWindowPathCell(t, windowPathCell{
			SenderProfile: sender, ReceiverProfile: receiver, ProfileFixtureSha256: digest,
			Arm: "delivery", Drop: true, RoundTrip: roundTrip, Flows: 1,
			RoundRobinOffer: true, Payload: 1280, Rate: 4000000, Warmup: 20 * residence,
		}, 2*residence)
		logWindowServiceReading(t, reading)
		t.Logf("capacity rtt=%s Mb/s=%.6f pace=%d window=%d discovering=%t drops=%d/%d queued-bytes=%d resends=%d",
			roundTrip, reading.Mbps, reading.Window.PacingByteRate, reading.Window.Window,
			reading.Window.PacingDiscovery, reading.RelayDrops, reading.MeasurementRelayDrops,
			reading.MaxRelayQueuedBytes, reading.Recovery.TimeoutResendWriteCount)
		if reading.MeasurementRelayDrops != 0 || reading.RelayDrops != 0 {
			t.Errorf("capacity discovery dropped traffic: %d/%d", reading.RelayDrops, reading.MeasurementRelayDrops)
		}
		if reading.Window.PacingDiscovery {
			t.Error("capacity was reached without observing a queue")
		}
		if float64(reading.Window.PacingByteRate) < .9*4000000 || float64(reading.Window.PacingByteRate) > 1.15*4000000 {
			t.Errorf("pace left the physical service rate: %d", reading.Window.PacingByteRate)
		}
		if reading.Mbps < .85*4000000*8/1e6 {
			t.Errorf("capacity lost goodput: %.6f Mb/s", reading.Mbps)
		}
		if float64(reading.MaxRelayQueuedBytes) > 2*4000000*.41+float64(limit) {
			t.Errorf("capacity queued beyond one window plus two residences: %d", reading.MaxRelayQueuedBytes)
		}
	})
}

// A shrinking peer permission lowers measured service because the sender is
// now the limit. Following it down trickles the small window and the
// receiver's quiet-head early ACK never fires; the held pace keeps it moving.
func TestWindowPathPermissionShrinkKeepsPace(t *testing.T) {
	assertMessagePoolOwnership(t)
	checkWindowMismatchCell(t, windowPathCell{
		SendWindow: mib(48), ReceiveWindow: mib(2), ReceiveWindowAfter: kib(64),
		WindowChangeAfter: 2 * time.Second, Warmup: 4 * time.Second,
		RoundTrip: 100 * time.Millisecond, Compression: 50 * time.Millisecond,
		Flows: 8, Rate: 12500000,
	})
}

// The held pace is not a floor against the path itself. When the link rate
// falls the read is backlogged, the hold is released, and pacing follows the
// new capacity down.
func TestWindowPathCapacityDropStillLowersPace(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		reading := measureWindowPathCell(t, windowPathCell{
			Arm: "delivery", Drop: true, RoundTrip: 100 * time.Millisecond,
			Compression: 10 * time.Millisecond, Flows: 1, Payload: 1280, Budget: mib(48),
			Rate: 12500000, RateAfter: 2000000, RateChangeAfter: 2 * time.Second,
			Warmup: 2*time.Second + 15*110*time.Millisecond,
		}, time.Second)
		logWindowServiceReading(t, reading)
		t.Logf("capacity-drop Mb/s=%.6f pace=%d window=%d discovering=%t drops=%d/%d queued-bytes=%d",
			reading.Mbps, reading.Window.PacingByteRate, reading.Window.Window,
			reading.Window.PacingDiscovery, reading.RelayDrops, reading.MeasurementRelayDrops,
			reading.MaxRelayQueuedBytes)
		if reading.MeasurementRelayDrops != 0 {
			t.Errorf("the slowed link dropped traffic: %d", reading.MeasurementRelayDrops)
		}
		if float64(reading.Window.PacingByteRate) > 1.15*2000000 {
			t.Errorf("pace stayed above the slowed link: %d", reading.Window.PacingByteRate)
		}
		if reading.Mbps < .85*2000000*8/1e6 {
			t.Errorf("the slowed link lost goodput: %.6f Mb/s", reading.Mbps)
		}
	})
}
