// Additional held selector controls preserve first-fallback availability and
// the boundary between ordinary healthy, service-failed and benched fields.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The real provider callback records the first selected field without waiting
// for a negative timeout or requiring a benched provider to adopt a flow.
func privateHealthFirstHarness(t *testing.T) (*reraceHarness, <-chan string) {
	t.Helper()
	firstRoute := make(chan string, 1)
	observe := func(name string) func(*IpPath) reraceAnswer {
		return func(*IpPath) reraceAnswer {
			select {
			case firstRoute <- name:
			default:
			}
			return reraceSilent
		}
	}
	harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
		{exitSpec: reraceExitSpec{name: "quality", answer: observe("quality")}, windowType: WindowTypeQuality},
		{exitSpec: reraceExitSpec{name: "speed", answer: observe("speed")}, windowType: WindowTypeSpeed},
	}, nil)
	return harness, firstRoute
}

// No ordinary healthy field exists, so the first allowed benched field keeps
// availability rather than turning an existing soft fallback into a blackhole.
func TestPrivateCrossWindowAllBenchedKeepsFirstWindowAvailability(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, firstRoute := privateHealthFirstHarness(t)
		for _, name := range []string{"quality", "speed"} {
			privateCrossWindowChannel(t, harness, name).setWarning(true, warnUnhealthy)
		}
		path := privateCrossWindowFlow(4, 23, 45300)
		if !harness.syn(path) {
			t.Fatal("all-benched fallback lost ordinary admission")
		}
		select {
		case first := <-firstRoute:
			if first != "quality" {
				t.Fatal("all-benched first-window order changed: ", first)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("first actual provider callback did not arrive within the response bound")
		}
	})
}

// A declared exact-service history input isolates the requested precedence:
// ordinary healthy failure memory outranks an untried but benched first field.
// This is a selector-state control, not an observed provider refusal claim.
func TestPrivateCrossWindowHealthyFailedSpeedPrecedesBenchedQuality(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, firstRoute := privateHealthFirstHarness(t)
		quality := privateCrossWindowChannel(t, harness, "quality")
		speed := privateCrossWindowChannel(t, harness, "speed")
		quality.setWarning(true, warnUnhealthy)
		path := privateCrossWindowFlow(4, 24, 45400)
		key, ok := destinationServiceFailureKeyFor(path, speed)
		if !ok {
			t.Fatal("exact-service selector-history key is malformed")
		}
		harness.multiClient.stateLock.Lock()
		harness.multiClient.destinationServiceFailures[key] = destinationServiceFailure{time: time.Now(), count: 1}
		harness.multiClient.stateLock.Unlock()
		if _, err := speed.WindowStats(); err != nil || speed.isWarning() {
			t.Fatal("speed did not retain the ordinary healthy predicate")
		}
		if !harness.syn(path) {
			t.Fatal("healthy failed fallback lost ordinary admission")
		}
		select {
		case first := <-firstRoute:
			if first != "speed" {
				t.Fatal("healthy service-failed field lost to benched quality: ", first)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("first actual provider callback did not arrive within the response bound")
		}
	})
}

// The new health-first preference is TCP-only. UDP443 retains the original
// quality-first benched fallback even with a healthy speed field present.
func TestPrivateCrossWindowFirstBenchedUdpSelectionIsUnchanged(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, firstRoute := privateHealthFirstHarness(t)
		quality := privateCrossWindowChannel(t, harness, "quality")
		quality.setWarning(true, warnUnhealthy)
		path := privateCrossWindowFlow(4, 25, 45500)
		path.Protocol, path.Syn, path.SequenceNumber = IpProtocolUdp, false, 0
		if !harness.send(ipOosUdpPacket(path, []byte("synthetic-udp-control"))) {
			t.Fatal("unchanged UDP fallback lost admission")
		}
		select {
		case first := <-firstRoute:
			if first != "quality" {
				t.Fatal("TCP-only health preference changed UDP window order: ", first)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("first actual provider callback did not arrive within the response bound")
		}
	})
}
