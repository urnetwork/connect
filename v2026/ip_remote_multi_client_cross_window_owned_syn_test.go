// Retained native syns keep the ordinary pin and health boundaries while the
// parent worker retries original source bytes without another source offer.
package connect

import (
	"testing"
	"testing/synctest"
)

// An actual source first owns quality. Only a later provider icmp starts the
// independent dispatcher, so its placement cannot be confused with source retransmission.
func privateCrossWindowTestOwnedSynPin(t *testing.T, appPin bool, firstField string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		specs := []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}
		if firstField == "untried" {
			specs = append(specs, privateCrossWindowExitSpec{
				exitSpec: reraceExitSpec{name: "quality-other", tier: 2}, windowType: WindowTypeQuality,
			})
		}
		harness := newPrivateCrossWindowHarness(t, specs, nil)
		path := privateCrossWindowFlow(4, 23, 45300)
		if appPin {
			harness.multiClient.SetFlowOwnerLookup(func(*IpPath) string { return "owned-syn-app" })
		} else {
			harness.multiClient.SetBlockActionOverrides([]*BlockActionOverride{{
				OverrideId: NewId(), Hosts: []string{path.DestinationIp.String()}, RouteOverride: &RouteOverride{Pin: true},
			}})
		}
		quality := privateCrossWindowChannel(t, harness, "quality")
		var sibling *IpPath
		var siblingUpdate *multiClientChannelUpdate
		if firstField != "empty" {
			sibling = privateCrossWindowFlow(4, 23, 45301)
			if !privateCrossWindowNativeSyn(harness, sibling) || harness.boundTo(sibling) != quality {
				t.Fatal("pinned sibling did not acquire its original quality owner")
			}
			harness.inject("quality", reraceSynAck(sibling))
			siblingUpdate = harness.update(sibling)
			if _, ok := harness.synAck(sibling); !ok || !siblingUpdate.receivedInbound.Load() {
				t.Fatal("pinned sibling did not establish before the independent failure")
			}
		}
		if !privateCrossWindowNativeSyn(harness, path) || harness.boundTo(path) != quality || len(harness.exit("speed").syns(path)) != 0 {
			t.Fatal("original native SYN did not own exactly the preferred pinned field")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		if !update.pinned || appPin && update.pinAppId != "owned-syn-app" {
			t.Fatal("actual native SYN did not carry the public pin rule")
		}
		dispatchEntered := make(chan struct{}, 1)
		var entered <-chan struct{} = dispatchEntered
		release := func() {}
		if firstField == "empty" {
			entered, release = synRetryControlHold(t, harness)
		} else {
			harness.multiClient.beforeSynRetryDispatchForTest = func(*multiClientChannelUpdate) {
				dispatchEntered <- struct{}{}
			}
		}
		harness.inject("quality", reraceProviderSignal(t, path))
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		synRetryControlEntered(t, entered)
		if firstField == "empty" {
			generator := harness.multiClient.generator.(*privateCrossWindowGenerator)
			exit := harness.exit("quality")
			generator.mutex.Lock()
			generator.unavailableExits[exit] = true
			generator.mutex.Unlock()
			quality.Cancel()
			harness.settle()
			release()
			harness.settle()
		}
		switch firstField {
		case "failed":
			if len(harness.exit("speed").syns(path)) != 0 || len(harness.exit("quality").syns(path)) != 1 ||
				len(harness.delivered(path)) != 0 || harness.boundTo(path) != nil || update.receivedInbound.Load() {
				t.Fatal("OWNED_SYN_PIN_ESCAPED_FIRST_FIELD: explicit failure promoted another window")
			}
			if synRetryControlOwner(t, update) != owner {
				t.Fatal("declined automatic placement lost original bounded SYN custody")
			}
			if !privateCrossWindowNativeSyn(harness, path) || harness.boundTo(path) != quality ||
				len(harness.exit("quality").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 0 {
				t.Fatal("actual source retransmit lost the pinned first-window availability fallback")
			}
			harness.inject("quality", reraceSynAck(path))
		case "untried", "empty":
			name := "quality-other"
			if firstField == "empty" {
				name = "speed"
			}
			if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, name) ||
				len(harness.exit(name).syns(path)) != 1 || len(harness.exit("quality").syns(path)) != 1 {
				t.Fatalf("automatic pinned retry lost its ordinary permitted field: first=%s", firstField)
			}
			if firstField == "untried" && len(harness.exit("speed").syns(path)) != 0 {
				t.Fatal("untried quality offer was bypassed for speed")
			}
		default:
			t.Fatal("unknown pinned first-field control")
		}
		if harness.update(path) != update || !update.receivedInbound.Load() {
			t.Fatal("owned retry changed the canonical tuple generation")
		}
		synRetryControlReleased(t, owner)
		if sibling != nil && (harness.update(sibling) != siblingUpdate || harness.boundTo(sibling) != quality || quality.IsDone() ||
			len(harness.delivered(sibling)) != 1 || len(harness.exit("speed").received(sibling)) != 0) {
			t.Fatal("independent pin recovery changed the established sibling")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// App pins retain the first field when only its service-failed owner remains.
func TestPrivateCrossWindowOwnedSynAppPinKeepsQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynPin(t, true, "failed")
}

// Host pins enforce the same first-field boundary with no app affinity group.
func TestPrivateCrossWindowOwnedSynSitePinKeepsQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynPin(t, false, "failed")
}

// The pin boundary still allows an untried healthy tier in its first window.
func TestPrivateCrossWindowOwnedSynAppPinUsesUntriedQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynPin(t, true, "untried")
}

// A host rule allows the same exact-service widening inside quality.
func TestPrivateCrossWindowOwnedSynSitePinUsesUntriedQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynPin(t, false, "untried")
}

// An empty first window never owned the pin's availability fallback.
func TestPrivateCrossWindowOwnedSynAppPinAllowsEmptyQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynPin(t, true, "empty")
}

// Closing the first field allows a host-pinned retry to find the next field.
func TestPrivateCrossWindowOwnedSynSitePinAllowsEmptyQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynPin(t, false, "empty")
}

// An unpinned retained syn crosses exhausted quality using the real public
// native entry point; no source retransmission or altered timer supplies it.
func privateCrossWindowTestOwnedSynUsesSpeed(t *testing.T, version int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(version, 24, 45400+version)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("original native SYN was not admitted")
		}
		privateCrossWindowAssertFailed(t, harness, path, "quality-a", "quality-b")
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed") ||
			len(harness.exit("speed").syns(path)) != 1 || len(harness.exit("quality-a").syns(path)) != 1 || len(harness.exit("quality-b").syns(path)) != 1 {
			t.Fatalf("retained ipv%d SYN did not cross exhausted quality exactly once", version)
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// The first source admission alone drives IPv4 recovery across the windows.
func TestPrivateCrossWindowOwnedSynUsesSpeedIp4(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynUsesSpeed(t, 4)
}

// IPv6 retains the same source custody through its own public native parse.
func TestPrivateCrossWindowOwnedSynUsesSpeedIp6(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynUsesSpeed(t, 6)
}

// Health state precedes the explicit failure that queues automatic work. This
// excludes a speed reply from an earlier completed continuation as evidence.
func privateCrossWindowTestOwnedSynUnavailableSpeed(t *testing.T, kind string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 25, 45500)
		quality := privateCrossWindowChannel(t, harness, "quality")
		speed := privateCrossWindowChannel(t, harness, "speed")
		if !privateCrossWindowNativeSyn(harness, path) || harness.boundTo(path) != quality || len(harness.exit("speed").syns(path)) != 0 {
			t.Fatal("original native SYN did not acquire only quality")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		switch kind {
		case "warned":
			speed.setWarning(true, warnUnhealthy)
		case "quarantined":
			speed.setQuarantined(blackholeNoReceiveAck)
		case "closed":
			generator := harness.multiClient.generator.(*privateCrossWindowGenerator)
			exit := harness.exit("speed")
			generator.mutex.Lock()
			generator.unavailableExits[exit] = true
			generator.mutex.Unlock()
			speed.Cancel()
			harness.settle()
		default:
			t.Fatal("unknown unavailable-speed control")
		}
		entered := make(chan struct{}, 1)
		harness.multiClient.beforeSynRetryDispatchForTest = func(*multiClientChannelUpdate) {
			entered <- struct{}{}
		}
		harness.inject("quality", reraceProviderSignal(t, path))
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		synRetryControlEntered(t, entered)
		if len(harness.exit("speed").syns(path)) != 0 || len(harness.exit("quality").syns(path)) != 1 || len(harness.delivered(path)) != 0 ||
			harness.boundTo(path) != nil || synRetryControlOwner(t, update) != owner {
			t.Fatalf("automatic retry promoted unavailable speed or lost original custody: state=%s", kind)
		}
		if !privateCrossWindowNativeSyn(harness, path) || harness.boundTo(path) != quality ||
			len(harness.exit("quality").syns(path)) != 2 || len(harness.exit("speed").syns(path)) != 0 {
			t.Fatal("actual source retry lost healthy first-window availability")
		}
		harness.inject("quality", reraceSynAck(path))
		synRetryControlReleased(t, owner)
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// A live warning excludes automatic promotion before provider refusal lands.
func TestPrivateCrossWindowOwnedSynDeclinesWarnedSpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynUnavailableSpeed(t, "warned")
}

// Quarantine uses the ordinary offer predicate for automatic retries too.
func TestPrivateCrossWindowOwnedSynDeclinesQuarantinedSpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynUnavailableSpeed(t, "quarantined")
}

// The closed generation is unavailable before the retry's candidate snapshot.
func TestPrivateCrossWindowOwnedSynDeclinesClosedSpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestOwnedSynUnavailableSpeed(t, "closed")
}
