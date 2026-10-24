// A later window's benched-only availability fallback must not displace the
// healthy first-window field solely because its exact service has failed.
package connect

import (
	"testing"
	"testing/synctest"
)

// Real provider replies establish service history before the selected live
// warning, quarantine or closed-owner state is applied to the speed field.
func privateCrossWindowTestSoleUnavailableSpeed(t *testing.T, kind string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(4, 21, 45100)
		harness.syn(path)
		privateCrossWindowAssertFailed(t, harness, path, "quality")
		quality := privateCrossWindowChannel(t, harness, "quality")
		speed := privateCrossWindowChannel(t, harness, "speed")
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
			synctest.Wait()
		default:
			t.Fatal("unowned unavailable-speed fixture state")
		}
		if _, err := quality.WindowStats(); err != nil || quality.isWarning() || quality.IsDone() {
			t.Fatal("quality is not globally healthy under the ordinary offer predicate")
		}
		if kind == "closed" {
			if !speed.IsDone() {
				t.Fatal("closed speed premise was not joined")
			}
		} else {
			if _, err := speed.WindowStats(); err != nil || !speed.isWarning() || speed.IsDone() {
				t.Fatal("live benched-only speed premise was not observed")
			}
			if got := harness.multiClient.raceCandidates(harness.multiClient.windows[WindowTypeSpeed], 4); len(got) != 1 || got[0] != speed {
				t.Fatal("existing per-window last-resort availability premise changed")
			}
		}
		t.Logf("cross-window health premises: quality_global_healthy=true quality_service_failed=true speed_state=%s speed_syns=0", kind)
		harness.syn(path)
		qualityOwners := harness.multiClient.windows[WindowTypeQuality].unorderedClients()
		if len(harness.exit("speed").syns(path)) != 0 || len(harness.exit("quality").syns(path)) != 2 || len(harness.delivered(path)) != 0 || quality.IsDone() || len(qualityOwners) != 1 || qualityOwners[0] != quality {
			t.Fatalf("SDK_CROSS_WINDOW_BENCHED_SPEED_REPLACED_HEALTHY_FALLBACK: state=%s quality_syns=%d speed_syns=%d app_packets=%d", kind, len(harness.exit("quality").syns(path)), len(harness.exit("speed").syns(path)), len(harness.delivered(path)))
		}
	})
}

// A plain warning retains its original owner but excludes it from promotion.
func TestPrivateCrossWindowSoleWarnedSpeedKeepsHealthyQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestSoleUnavailableSpeed(t, "warned")
}

// Quarantine uses the same actual isWarning predicate as ordinary ordering.
func TestPrivateCrossWindowSoleQuarantinedSpeedKeepsHealthyQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestSoleUnavailableSpeed(t, "quarantined")
}

// An ended speed owner is unlisted before cancellation so no new generation
// can silently substitute for the closed owner under test.
func TestPrivateCrossWindowSoleClosedSpeedKeepsHealthyQualityFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestSoleUnavailableSpeed(t, "closed")
}
