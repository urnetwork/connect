// Controls for the private TCP selector experiment use real public packet
// sends and provider replies; only declared selector-history tests seed state.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// A sole failed candidate remains a valid fallback, but its service failure
// must still be recognized when another permitted window has an untried exit.
func privateCrossWindowTestSoleQualityFailureUsesSpeed(t *testing.T, version int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(version, 1, 43200+version)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || len(harness.exit("speed").syns(path)) != 1 || len(harness.exit("quality").syns(path)) != 1 {
			t.Fatalf("sole quality failure was retried before untried speed: ipv%d quality=%d speed=%d", version, len(harness.exit("quality").syns(path)), len(harness.exit("speed").syns(path)))
		}
	})
}

// IPv4 has its own execution and cleanup boundary.
func TestPrivateCrossWindowSoleQualityFailureUsesSpeedIp4(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestSoleQualityFailureUsesSpeed(t, 4)
}

// IPv6 executes independently of an IPv4 assertion failure.
func TestPrivateCrossWindowSoleQualityFailureUsesSpeedIp6(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestSoleQualityFailureUsesSpeed(t, 6)
}

// A distinct fresh tuple observes the same service memory without replaying
// or transferring the original unestablished flow's packet ownership.
func TestPrivateCrossWindowNewFlowUsesServiceFailureMemory(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		oldPath := privateCrossWindowFlow(4, 2, 43300)
		harness.syn(oldPath)
		privateCrossWindowAssertFailed(t, harness, oldPath, "quality")
		oldUpdate := harness.update(oldPath)
		freshPath := privateCrossWindowFlow(4, 2, 43301)
		harness.syn(freshPath)
		if _, ok := harness.synAck(freshPath); !ok || len(harness.exit("speed").syns(freshPath)) != 1 || len(harness.exit("quality").syns(freshPath)) != 0 {
			t.Fatal("fresh tuple retried remembered quality failure before untried speed")
		}
		if harness.update(oldPath) != oldUpdate || oldUpdate == nil || oldUpdate.receivedInbound.Load() || harness.boundTo(oldPath) != nil || len(harness.exit("speed").syns(oldPath)) != 0 {
			t.Fatal("fresh selection changed or replayed the original unestablished tuple")
		}
	})
}

// Once every selected window has really refused, the next source retransmit
// retains the original first-window fallback and does not send synthetic work.
func TestPrivateCrossWindowAllWindowsFailedKeepsFirstFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 3, 43400)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality-a", "quality-b")
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "speed")
		harness.syn(path)
		if len(harness.exit("quality-a").syns(path)) != 2 || len(harness.exit("quality-b").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 1 {
			t.Fatal("all-failed retransmit changed the original first-window fallback")
		}
		if len(harness.delivered(path)) != 0 || len(harness.sentSyns(path)) != 3 {
			t.Fatal("all-failed policy fabricated app delivery or source retries")
		}
	})
}

// This selector-only history boundary declares distinct valid failure ages.
// Actual packet sending must preserve the first window's oldest-first order;
// the speed window's even older failure cannot displace that fallback.
func TestPrivateCrossWindowAllFailedOldestQualityOrder(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-old"}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-new"}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 1 })
		path := privateCrossWindowFlow(4, 4, 43500)
		for _, entry := range []struct {
			name string
			age  time.Duration
		}{
			{name: "quality-old", age: time.Minute},
			{name: "quality-new", age: time.Second},
			{name: "speed", age: 2 * time.Minute},
		} {
			client := privateCrossWindowChannel(t, harness, entry.name)
			key, ok := destinationServiceFailureKeyFor(path, client)
			if !ok {
				t.Fatal("malformed fixture service key")
			}
			harness.multiClient.stateLock.Lock()
			if harness.multiClient.destinationServiceFailures == nil {
				harness.multiClient.destinationServiceFailures = map[destinationServiceFailureKey]destinationServiceFailure{}
			}
			harness.multiClient.destinationServiceFailures[key] = destinationServiceFailure{time: time.Now().Add(-entry.age), count: 1}
			harness.multiClient.stateLock.Unlock()
		}
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "quality-old") ||
			len(harness.exit("quality-new").syns(path)) != 0 || len(harness.exit("speed").syns(path)) != 0 {
			t.Fatal("all-failed availability fallback lost first-window oldest-first selection")
		}
	})
}

// Installs the actual public pin rule before either source SYN. Pins keep the
// existing quality fallback even after that destination refuses it.
func privateCrossWindowTestPin(t *testing.T, appPin bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 5, 43600)
		if appPin {
			harness.multiClient.SetFlowOwnerLookup(func(*IpPath) string { return "synthetic-app" })
		} else {
			harness.multiClient.SetBlockActionOverrides([]*BlockActionOverride{{
				OverrideId: NewId(), Hosts: []string{path.DestinationIp.String()}, RouteOverride: &RouteOverride{Pin: true},
			}})
		}
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		update := harness.update(path)
		if update == nil || !update.pinned || appPin && update.pinAppId != "synthetic-app" {
			t.Fatal("public pin rule was not applied to the flow")
		}
		if len(harness.exit("quality").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 0 || len(harness.delivered(path)) != 0 {
			t.Fatal("explicit pin spilled into another window")
		}
	})
}

// App-scoped stable egress remains an explicit selection policy.
func TestPrivateCrossWindowAppPinKeepsQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestPin(t, true)
}

// A site rule retains the same stable egress policy.
func TestPrivateCrossWindowSitePinKeepsQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestPin(t, false)
}

// This auto-sized fixed-type profile keeps both actual owners warm. Its
// explicit quality selection still excludes the healthy speed alternative.
func TestPrivateCrossWindowFixedProfileKeepsQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) {
			settings.DefaultPerformanceProfile = &PerformanceProfile{
				WindowType: WindowTypeQuality, WindowSize: settings.WindowSizes[WindowTypeQuality],
			}
		})
		if speed := privateCrossWindowChannel(t, harness, "speed"); speed.IsDone() {
			t.Fatal("fixed-type profile did not retain its real warm speed owner")
		}
		path := privateCrossWindowFlow(4, 6, 43700)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if len(harness.exit("quality").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 0 || len(harness.delivered(path)) != 0 {
			t.Fatal("fixed quality profile spilled into speed")
		}
	})
}

// Explicit destinations use the real constructor's quality-only window and
// its ordinary deferred-mint path, with no dynamic speed alternative.
func TestPrivateCrossWindowFixedDestinationKeepsQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil, true)
		if harness.multiClient.windows[WindowTypeSpeed] != nil {
			t.Fatal("fixed-destination constructor unexpectedly created speed")
		}
		path := privateCrossWindowFlow(4, 7, 43800)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if len(harness.exit("quality").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 0 || len(harness.delivered(path)) != 0 {
			t.Fatal("fixed destination changed its original quality fallback")
		}
	})
}

// A family-incompatible speed owner remains owned but cannot receive the
// fresh attempt; quality retains its valid-family failed fallback.
func privateCrossWindowTestIncompatibleSpeedKeepsQualityFallback(t *testing.T, version int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		family := IpFamilyV4Only
		if version == 4 {
			family = IpFamilyV6Only
		}
		specs := []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", family: family}, windowType: WindowTypeSpeed},
		}
		if version == 4 {
			// Ordinary main fill admits v4 first; its real soft-family
			// acquisition then admits the v6-only owner. End the flowless
			// setup anchor before observing the tested source attempt.
			specs = append(specs, privateCrossWindowExitSpec{
				exitSpec: reraceExitSpec{name: "speed-setup-anchor", family: IpFamilyV4Only}, windowType: WindowTypeSpeed,
			})
		}
		harness := newPrivateCrossWindowSourceRetryHarness(t, specs, func(settings *MultiClientSettings) {
			if version == 4 {
				windowSize := settings.WindowSizes[WindowTypeSpeed]
				windowSize.WindowSizeMinIpv6Capable = 1
				settings.WindowSizes[WindowTypeSpeed] = windowSize
			}
		})
		incompatible := privateCrossWindowChannel(t, harness, "speed")
		if incompatible.supportsIpVersion(version) {
			t.Fatal("declared speed owner is not family-incompatible")
		}
		if version == 4 {
			generator := harness.multiClient.generator.(*privateCrossWindowGenerator)
			anchor := harness.exit("speed-setup-anchor")
			generator.mutex.Lock()
			generator.unavailableExits[anchor] = true
			generator.mutex.Unlock()
			privateCrossWindowChannel(t, harness, "speed-setup-anchor").Cancel()
			synctest.Wait()
		}
		if incompatible.IsDone() {
			t.Fatal("incompatible owner did not survive the completed family setup")
		}
		path := privateCrossWindowFlow(version, 8, 43900+version)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if len(harness.exit("quality").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 0 || len(harness.delivered(path)) != 0 {
			t.Fatalf("ipv%d attempt escaped through an incompatible speed owner", version)
		}
		if version == 4 && len(harness.exit("speed-setup-anchor").syns(path)) != 0 {
			t.Fatal("ended family setup anchor received the tested source attempt")
		}
	})
}

// IPv4 has its own execution and cleanup boundary.
func TestPrivateCrossWindowIncompatibleSpeedKeepsQualityFallbackIp4(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestIncompatibleSpeedKeepsQualityFallback(t, 4)
}

// IPv6 executes independently of an IPv4 assertion failure.
func TestPrivateCrossWindowIncompatibleSpeedKeepsQualityFallbackIp6(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestIncompatibleSpeedKeepsQualityFallback(t, 6)
}

// An untried lower tier in the preferred window outranks cross-window spill;
// crossing rank remains governed by the existing exact-service logic.
func TestPrivateCrossWindowQualityCrossTierPrecedesSpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-best", tier: 0, answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-other", tier: 2}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", tier: 0}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 9, 44000)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality-best")
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "quality-other") ||
			len(harness.exit("speed").syns(path)) != 0 || len(harness.exit("quality-best").syns(path)) != 1 {
			t.Fatal("cross-window preference bypassed an eligible quality tier")
		}
	})
}

// The selected speed window still narrows to its own best eligible rank.
func TestPrivateCrossWindowSpeedKeepsRankGate(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-best", tier: 0}, windowType: WindowTypeSpeed},
			{exitSpec: reraceExitSpec{name: "speed-other", tier: 2}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 10, 44100)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed-best") || len(harness.exit("speed-other").syns(path)) != 0 {
			t.Fatal("speed spill bypassed its original rank gate")
		}
	})
}

// Existing warning/quarantine/ended exclusions still apply to the second
// window when that window has a healthy eligible alternative.
func TestPrivateCrossWindowSpeedPreservesHealthExclusions(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-warned", tier: 0}, windowType: WindowTypeSpeed},
			{exitSpec: reraceExitSpec{name: "speed-quarantined", tier: 0}, windowType: WindowTypeSpeed},
			{exitSpec: reraceExitSpec{name: "speed-ended", tier: 0}, windowType: WindowTypeSpeed},
			{exitSpec: reraceExitSpec{name: "speed-healthy", tier: 2}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 11, 44200)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		// Ended generations must not be silently replaced by the fixture.
		// Join that lifecycle before establishing the warning snapshots.
		generator := harness.multiClient.generator.(*privateCrossWindowGenerator)
		ended := harness.exit("speed-ended")
		generator.mutex.Lock()
		generator.unavailableExits[ended] = true
		generator.mutex.Unlock()
		privateCrossWindowChannel(t, harness, "speed-ended").Cancel()
		synctest.Wait()
		privateCrossWindowChannel(t, harness, "speed-warned").setWarning(true, warnUnhealthy)
		privateCrossWindowChannel(t, harness, "speed-quarantined").setQuarantined(blackholeNoReceiveAck)
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed-healthy") {
			t.Fatal("healthy speed alternative did not win after quality refusal")
		}
		for _, name := range []string{"speed-warned", "speed-quarantined", "speed-ended"} {
			if len(harness.exit(name).syns(path)) != 0 {
				t.Fatalf("excluded owner %s received the fresh flow", name)
			}
		}
	})
}

// A real existing speed flow fills its min-tier owner's cap. The fresh
// quality-failed attempt uses the under-cap speed tier without migrating it.
func TestPrivateCrossWindowSpeedPreservesFlowCap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-full", tier: 0}, windowType: WindowTypeSpeed},
			{exitSpec: reraceExitSpec{name: "speed-free", tier: 2}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MaxFlowsPerExit = 1 })
		existing := privateCrossWindowFlow(4, 12, 44300)
		existing.DestinationPort = 5000
		harness.syn(existing)
		owner := privateCrossWindowChannel(t, harness, "speed-full")
		if _, ok := harness.synAck(existing); !ok || harness.boundTo(existing) != owner || harness.multiClient.clientFlowCount(owner) != 1 {
			t.Fatal("public existing flow did not fill the intended speed owner")
		}
		path := privateCrossWindowFlow(4, 13, 44301)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed-free") ||
			len(harness.exit("speed-full").syns(path)) != 0 || harness.boundTo(existing) != owner || owner.IsDone() {
			t.Fatal("cross-window choice bypassed flow capacity or changed the carrying owner")
		}
	})
}

// Reputation arrives through actual discovery metadata. The flow's public
// server-name lookup makes the original second-window reputation filter act.
func TestPrivateCrossWindowSpeedPreservesReputation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed-rejected"}, windowType: WindowTypeSpeed, reputationFailures: []string{"service.example"}},
			{exitSpec: reraceExitSpec{name: "speed-clean"}, windowType: WindowTypeSpeed},
		}, nil)
		harness.multiClient.SetServerNameLookup(stubServerNameLookup{names: []string{"assets.service.example"}})
		path := privateCrossWindowFlow(4, 14, 44400)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed-clean") || len(harness.exit("speed-rejected").syns(path)) != 0 {
			t.Fatal("cross-window preference bypassed external service reputation")
		}
	})
}

// A held 12,288-byte CSS-shaped sibling stays on its established tuple and
// owner while a new connection to the same service fails quality and uses
// speed. This is synthetic public TCP data, not the natural100 HTTP fixture.
func privateCrossWindowTestKeepsEstablishedCssSibling(t *testing.T, version int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		const siblingPort = 44500
		answer := func(path *IpPath) reraceAnswer {
			if path.SourcePort == siblingPort {
				return reraceAccept
			}
			return reraceSignal
		}
		harness := newPrivateCrossWindowSourceRetryHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-original", tier: 0, answer: answer}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-other", tier: 2, answer: answer}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		sibling := privateCrossWindowFlow(version, 15, siblingPort)
		harness.syn(sibling)
		owner := privateCrossWindowChannel(t, harness, "quality-original")
		update := harness.update(sibling)
		if _, ok := harness.synAck(sibling); !ok || harness.boundTo(sibling) != owner {
			t.Fatal("CSS sibling did not establish on the original quality exit")
		}
		path := privateCrossWindowFlow(version, 15, siblingPort+1)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality-original")
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality-other")
		harness.syn(path)
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed") {
			t.Fatal("affected same-service flow never reached the eligible speed exit")
		}
		body := bytes.Repeat([]byte("synthetic-css{}\n"), 768)
		if len(body) != 12288 {
			t.Fatal("fixture CSS body length changed")
		}
		for offset := 0; offset < len(body); offset += 4096 {
			if !harness.send(reraceSegment(sibling, uint32(offset), body[offset:offset+4096])) {
				t.Fatal("original CSS owner refused its established data")
			}
		}
		var received []byte
		for _, packet := range harness.delivered(sibling) {
			parsed, payload, err := ParseIpPathWithPayload(packet.packet)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Protocol == IpProtocolTcp && !parsed.Syn {
				received = append(received, payload...)
			}
		}
		if !bytes.Equal(received, body) || harness.update(sibling) != update || harness.boundTo(sibling) != owner || owner.IsDone() ||
			len(harness.exit("speed").received(sibling)) != 0 || len(harness.exit("quality-other").received(sibling)) != 0 {
			t.Fatal("cross-window recovery changed the established CSS tuple, owner, or exact body")
		}
		reraceAssertNoErrorDelivered(t, harness, sibling)
	})
}

// IPv4 has its own execution and cleanup boundary.
func TestPrivateCrossWindowKeepsEstablishedCssSiblingIp4(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestKeepsEstablishedCssSibling(t, 4)
}

// IPv6 executes independently of an IPv4 assertion failure.
func TestPrivateCrossWindowKeepsEstablishedCssSiblingIp6(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestKeepsEstablishedCssSibling(t, 6)
}

// The experiment is scoped to unestablished TCP SYNs. Existing UDP refusal
// selection remains unchanged, including swallowed provider failure signals.
func TestPrivateCrossWindowUdpSelectionIsUnchanged(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 16, 44600)
		path.Protocol = IpProtocolUdp
		path.Syn = false
		path.SequenceNumber = 0
		harness.send(ipOosUdpPacket(path, []byte("synthetic-quic-probe")))
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		harness.send(ipOosUdpPacket(path, []byte("synthetic-quic-probe")))
		if len(harness.exit("quality").received(path)) != 2 || len(harness.exit("speed").received(path)) != 0 || len(harness.delivered(path)) != 0 {
			t.Fatal("TCP selector experiment changed UDP refusal selection")
		}
	})
}

// The new predicate reads the existing bounded service memory. Its sole-exit
// answer differs from filtering, while exact port/family/expiry rules do not.
func TestPrivateCrossWindowExhaustionHistoryIsExactAndExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultMultiClientSettings()
		failed := &multiClientChannel{ctx: t.Context(), settings: settings}
		untried := &multiClientChannel{ctx: t.Context(), settings: settings}
		parent := &RemoteUserNatMultiClient{settings: settings}
		path := privateCrossWindowFlow(4, 17, 44700)
		parent.stateLock.Lock()
		parent.recordDestinationServiceFailureWithLock(failed, path)
		parent.stateLock.Unlock()
		if !parent.destinationServiceFailuresExhausted([]*multiClientChannel{failed}, path) {
			t.Fatal("sole failed candidate was not recognized as exhausted")
		}
		if filtered := parent.filterDestinationServiceFailures([]*multiClientChannel{failed}, path); len(filtered) != 1 || filtered[0] != failed {
			t.Fatal("existing sole-candidate availability fallback was changed")
		}
		otherPort := *path
		otherPort.DestinationPort++
		otherFamily := privateCrossWindowFlow(6, 17, 44700)
		for _, entry := range []struct {
			name       string
			candidates []*multiClientChannel
			path       *IpPath
		}{
			{name: "empty", path: path},
			{name: "untried", candidates: []*multiClientChannel{failed, untried}, path: path},
			{name: "other-port", candidates: []*multiClientChannel{failed}, path: &otherPort},
			{name: "other-family", candidates: []*multiClientChannel{failed}, path: otherFamily},
			{name: "nil-path", candidates: []*multiClientChannel{failed}},
			{name: "nil-owner", candidates: []*multiClientChannel{nil}, path: path},
		} {
			if parent.destinationServiceFailuresExhausted(entry.candidates, entry.path) {
				t.Fatalf("%s supplied false exhaustion evidence", entry.name)
			}
		}
		key, _ := destinationServiceFailureKeyFor(path, failed)
		parent.stateLock.Lock()
		parent.destinationServiceFailures[key] = destinationServiceFailure{time: time.Now().Add(-destinationServiceFailureTtl), count: 1}
		parent.stateLock.Unlock()
		if parent.destinationServiceFailuresExhausted([]*multiClientChannel{failed}, path) {
			t.Fatal("expired history suppressed an untried service opportunity")
		}
		parent.stateLock.Lock()
		_, retained := parent.destinationServiceFailures[key]
		parent.stateLock.Unlock()
		if retained {
			t.Fatal("predicate failed to preserve existing lazy-expiry cleanup")
		}
	})
}
