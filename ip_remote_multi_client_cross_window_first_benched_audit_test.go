// Test-only audit of the first window's availability fallback across two
// permitted windows. No service history or production policy is synthesized.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The first actual provider callback identifies the selected field. The
// benched provider stays silent, so no refusal can seed service history.
func privateCrossWindowTestFirstBenchedBeforeHealthySpeed(t *testing.T, quarantine bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		firstRoute := make(chan string, 1)
		observe := func(name string, answer reraceAnswer) func(*IpPath) reraceAnswer {
			return func(*IpPath) reraceAnswer {
				select {
				case firstRoute <- name:
				default:
				}
				return answer
			}
		}
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality", answer: observe("quality", reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", answer: observe("speed", reraceAccept)}, windowType: WindowTypeSpeed},
		}, nil)
		quality := privateCrossWindowChannel(t, harness, "quality")
		speed := privateCrossWindowChannel(t, harness, "speed")
		if quarantine {
			quality.setQuarantined(blackholeNoReceiveAck)
		} else {
			quality.setWarning(true, warnUnhealthy)
		}
		qualityWindow, speedWindow := harness.multiClient.windows[WindowTypeQuality], harness.multiClient.windows[WindowTypeSpeed]
		if clients := qualityWindow.orderedClients(false, 4); len(clients) != 0 || !quality.isWarning() || quality.IsDone() {
			t.Fatal("first quality window is not live and wholly benched")
		}
		if clients := harness.multiClient.raceCandidates(qualityWindow, 4); len(clients) != 1 || clients[0] != quality {
			t.Fatal("existing per-window last-resort field is not the benched quality owner")
		}
		if clients := speedWindow.orderedClients(false, 4); len(clients) != 1 || clients[0] != speed || speed.isWarning() || speed.IsDone() {
			t.Fatal("permitted speed window does not have its ordinary healthy owner")
		}
		harness.multiClient.stateLock.Lock()
		noServiceHistory := len(harness.multiClient.destinationServiceFailures) == 0
		harness.multiClient.stateLock.Unlock()
		if !noServiceHistory {
			t.Fatal("independent-health audit inherited exact-service history")
		}
		t.Logf("first-window audit premises: quality_benched=true quarantine=%t speed_healthy=true service_history=0 windows_permitted=quality,speed", quarantine)
		path := privateCrossWindowFlow(4, 22, 45200)
		start := time.Now()
		if !harness.syn(path) {
			t.Fatal("first public source SYN was not admitted to an available field")
		}
		var first string
		select {
		case first = <-firstRoute:
		case <-time.After(5 * time.Second):
			t.Fatal("first actual provider callback did not arrive within the response bound")
		}
		harness.multiClient.stateLock.Lock()
		noServiceHistory = len(harness.multiClient.destinationServiceFailures) == 0
		harness.multiClient.stateLock.Unlock()
		if !noServiceHistory {
			t.Fatal("the selected silent provider unexpectedly created exact-service evidence")
		}
		if first != "speed" {
			t.Fatalf("SDK_CROSS_WINDOW_FIRST_BENCHED_SHADOWED_HEALTHY: first=%s quality_syns=%d speed_syns=%d service_history=0", first, len(harness.exit("quality").syns(path)), len(harness.exit("speed").syns(path)))
		}
		if !harness.waitForSynAck(path, start.Add(5*time.Second)) || harness.boundTo(path) != speed || len(harness.exit("quality").syns(path)) != 0 || quality.IsDone() {
			t.Fatal("ordinary healthy speed recovery changed first-window ownership or missed the response bound")
		}
	})
}

// Warned quality is a last resort only when no permitted healthy field exists.
func TestPrivateCrossWindowFirstWarnedQualityPrefersHealthySpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestFirstBenchedBeforeHealthySpeed(t, false)
}

// Quarantine follows the same actual offer exclusion as an ordinary warning.
func TestPrivateCrossWindowFirstQuarantinedQualityPrefersHealthySpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestFirstBenchedBeforeHealthySpeed(t, true)
}
