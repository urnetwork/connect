package extender

// Phase-control and degraded-state tests for the splice rollout
// (EXTENDER-UPDATE2 §3.2, §3.3): the runtime posture flip and its rollback, the
// record-settle gate, the degraded fallback-terminate that must not read as
// coverage, and the per-target load measurement.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A borrowed target that is never reachable, for the degraded-fallback test.
var errUnreachableBorrowTarget = errors.New("borrow target unreachable")

// Writes a raw unauthenticated browser hello fronting testBorrowName to a new
// prober connection and returns it open.
func writeProber(t *testing.T, fixture *extenderFixture) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write(rawUnauthenticatedHelloRecord(t, testBorrowName)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// SetCamouflageSplice flips the posture at run time: a prober before the flip is
// terminated (Phase A), one after it is spliced (Phase B), and a rollback
// terminates new probers again — all without a restart (§3.3 rollback changes
// posture for new connections).
func TestExtenderCamouflageRuntimeSplicePostureFlipAndRollback(t *testing.T) {
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	terminated := make(chan struct{}, 8)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		// start in Phase A
		settings.ExtenderCamouflageSplice = false
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
				terminated <- struct{}{}
			}
		}
	})
	if fixture.server.CamouflageSplice() {
		t.Fatal("posture started on though ExtenderCamouflageSplice was false")
	}
	// Phase A: a prober is terminated, not spliced
	writeProber(t, fixture)
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatal("the Phase A prober was not terminated")
	}
	if site.reached.get() != 0 {
		t.Fatal("a prober was spliced while the posture was off")
	}

	// flip to Phase B: a new prober is spliced
	fixture.server.SetCamouflageSplice(true)
	if !fixture.server.CamouflageSplice() {
		t.Fatal("posture did not report on after the flip")
	}
	writeProber(t, fixture)
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the Phase B prober was not spliced after the flip")
	}

	// roll back to Phase A: a new prober is terminated again
	splicedBefore := site.reached.get()
	fixture.server.SetCamouflageSplice(false)
	writeProber(t, fixture)
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatal("the prober after rollback was not terminated")
	}
	if site.reached.get() != splicedBefore {
		t.Fatal("a prober was spliced after rollback to Phase A")
	}
}

// The posture is the ExtenderCamouflageSplice setting until SetCamouflageSplice
// sets one: a setting made after the server was built and before it serves
// applies, as it did before the runtime flip existed, and a posture set at run
// time then wins over the setting either way (§3.3).
func TestExtenderCamouflageSplicePostureFollowsSettingUntilSet(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.CamouflageEnabled = true
	settings.IdentityKeySeed = mustSeed(t)
	settings.ExtenderCamouflageSplice = false
	server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
	defer server.CloseAndWait()

	// set after the server was built, before it serves
	server.settings.ExtenderCamouflageSplice = true
	if !server.CamouflageSplice() || !server.camouflageSpliceEnabled() {
		t.Fatal("a splice setting made before serving did not apply")
	}
	if status := server.CamouflageStatus(); !status.SpliceOn {
		t.Fatalf("status = %+v, expected the splice on", status)
	}
	server.settings.ExtenderCamouflageSplice = false
	if server.CamouflageSplice() || server.camouflageSpliceEnabled() {
		t.Fatal("clearing the splice setting before serving did not apply")
	}

	// a posture set at run time wins over the setting
	server.SetCamouflageSplice(true)
	server.settings.ExtenderCamouflageSplice = false
	if !server.CamouflageSplice() || !server.camouflageSpliceEnabled() {
		t.Fatal("the setting overrode a posture set at run time")
	}
	server.SetCamouflageSplice(false)
	server.settings.ExtenderCamouflageSplice = true
	if server.CamouflageSplice() || server.camouflageSpliceEnabled() {
		t.Fatal("the setting overrode a rollback set at run time")
	}
	if status := server.CamouflageStatus(); status.SpliceOn || status.Covered {
		t.Fatalf("status = %+v, expected the rollback to hold", status)
	}
}

// A working splice already relaying is not interrupted by a rollback: rollback
// changes posture for new connections and preserves working sessions (§3.3).
func TestExtenderCamouflageRollbackPreservesActiveSplice(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	site := newFakeBorrowSite(t, blockingBorrowSiteHandler(release))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	// a prober is spliced and held open by the blocking site
	prober := writeProber(t, fixture)
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the splice did not begin")
	}
	// rolling back does not tear the active splice down
	fixture.server.SetCamouflageSplice(false)
	prober.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buffer := make([]byte, 1)
	_, err := prober.Read(buffer)
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("the active splice was torn down by rollback (read err = %v), expected it to stay open", err)
	}
}

// CamouflageSpliceGate withholds the splice until the record settle rule is met,
// then admits it — the §3.2 safe flip: splice only once the extender's record
// carries the key and requires tcp-camouflage-v1 and legacy records have
// expired. A closed gate keeps Phase A even with the posture on; opening it
// moves to Phase B.
func TestExtenderCamouflageSpliceGate(t *testing.T) {
	var gateOpen atomic.Bool
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	terminated := make(chan struct{}, 8)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true // posture on
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
		settings.CamouflageSpliceGate = func() bool { return gateOpen.Load() }
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeTerminated {
				terminated <- struct{}{}
			}
		}
	})
	// gate closed: posture on but the record settle rule is not met, so Phase A
	writeProber(t, fixture)
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatal("a prober was not terminated while the splice gate was closed")
	}
	if site.reached.get() != 0 {
		t.Fatal("a prober was spliced while the gate was closed")
	}
	if status := fixture.server.CamouflageStatus(); status.Covered || status.DegradedReason != ExtenderCamouflageDegradedGateClosed {
		t.Fatalf("status = %+v, expected degraded splice_gate_closed", status)
	}
	// open the gate: Phase B
	gateOpen.Store(true)
	writeProber(t, fixture)
	select {
	case <-site.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("a prober was not spliced after the gate opened")
	}
}

// Splice on but no borrowed target reachable: the hello is terminated and the
// degraded fallback-terminate counter increments, apart from the Phase-A
// terminate, and the status reports degraded coverage, not success (§3.3). A
// deployment in that state must not report successful camouflage coverage.
func TestExtenderCamouflageDegradedFallbackTerminate(t *testing.T) {
	fallback := make(chan struct{}, 8)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		// every splice dial fails: no borrowed target is reachable
		settings.CamouflageSpliceDialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			return nil, errUnreachableBorrowTarget
		}
		settings.CamouflageHandler = func(outcome ExtenderCamouflageOutcome) {
			if outcome.Outcome == ExtenderCamouflageOutcomeSpliceFallbackTerminated {
				fallback <- struct{}{}
			}
		}
	})
	writeProber(t, fixture)
	select {
	case <-fallback:
	case <-time.After(10 * time.Second):
		t.Fatal("a splice with no reachable target did not report the degraded fallback terminate")
	}
	stats := fixture.server.CamouflageStats()
	if stats.SpliceFallbackTerminatedCount == 0 {
		t.Fatal("the degraded fallback-terminate counter did not increment")
	}
	if stats.TerminatedCount != 0 {
		t.Fatalf("a degraded fallback was counted as a Phase-A terminate (%d)", stats.TerminatedCount)
	}
	status := fixture.server.CamouflageStatus()
	if status.Covered {
		t.Fatal("a deployment that fell back to termination reported successful coverage")
	}
	if status.DegradedReason != ExtenderCamouflageDegradedFallbackActive {
		t.Fatalf("degraded reason = %q, expected splice_fallback_active", status.DegradedReason)
	}
}

// A splice pilot with no verified target is degraded coverage, not success
// (§3.3): substituting nothing is still not real-site behavior.
func TestExtenderCamouflageStatusNoVerifiedTarget(t *testing.T) {
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		// no borrow names verified
		settings.CamouflageBorrowNames = nil
	})
	status := fixture.server.CamouflageStatus()
	if status.Covered || status.DegradedReason != ExtenderCamouflageDegradedNoVerifiedTarget {
		t.Fatalf("status = %+v, expected degraded no_verified_target", status)
	}
	if status.Phase != ExtenderCamouflagePhaseSplicePilot {
		t.Fatalf("phase = %q, expected splice_pilot", status.Phase)
	}
}

// No verified name is what the status says in every posture of an enabled
// extender, since it stands between the extender and coverage whatever the posture
// and the gate say; only the phase differs. With a name, the posture and the gate
// speak as they did. Off is off, name or no name.
func TestExtenderCamouflageStatusNamesNoVerifiedTargetInEveryPosture(t *testing.T) {
	closedGate := func() bool { return false }
	openGate := func() bool { return true }
	cases := []struct {
		name           string
		enabled        bool
		splice         bool
		gate           func() bool
		borrowNames    []string
		expectedPhase  string
		expectedReason string
	}{
		{
			name:           "phase a with no name",
			enabled:        true,
			expectedPhase:  ExtenderCamouflagePhaseAuthenticatedRollout,
			expectedReason: ExtenderCamouflageDegradedNoVerifiedTarget,
		},
		{
			name:           "gate closed with no name",
			enabled:        true,
			splice:         true,
			gate:           closedGate,
			expectedPhase:  ExtenderCamouflagePhaseAuthenticatedRollout,
			expectedReason: ExtenderCamouflageDegradedNoVerifiedTarget,
		},
		{
			name:           "gate open with no name",
			enabled:        true,
			splice:         true,
			gate:           openGate,
			expectedPhase:  ExtenderCamouflagePhaseSplicePilot,
			expectedReason: ExtenderCamouflageDegradedNoVerifiedTarget,
		},
		{
			name:           "phase a with a name",
			enabled:        true,
			borrowNames:    []string{testBorrowName},
			expectedPhase:  ExtenderCamouflagePhaseAuthenticatedRollout,
			expectedReason: ExtenderCamouflageDegradedSpliceOff,
		},
		{
			name:           "gate closed with a name",
			enabled:        true,
			splice:         true,
			gate:           closedGate,
			borrowNames:    []string{testBorrowName},
			expectedPhase:  ExtenderCamouflagePhaseAuthenticatedRollout,
			expectedReason: ExtenderCamouflageDegradedGateClosed,
		},
		{
			name:           "off with no name",
			expectedPhase:  ExtenderCamouflagePhaseOff,
			expectedReason: ExtenderCamouflageDegradedCamouflageOff,
		},
		{
			name:           "off with a name",
			borrowNames:    []string{testBorrowName},
			expectedPhase:  ExtenderCamouflagePhaseOff,
			expectedReason: ExtenderCamouflageDegradedCamouflageOff,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
				settings.CamouflageEnabled = c.enabled
				settings.IdentityKeySeed = mustSeed(t)
				settings.ExtenderCamouflageSplice = c.splice
				settings.CamouflageSpliceGate = c.gate
				settings.CamouflageBorrowNames = c.borrowNames
			})
			status := fixture.server.CamouflageStatus()
			if status.Covered || status.Phase != c.expectedPhase || status.DegradedReason != c.expectedReason {
				t.Fatalf("status = %+v, expected %s with %s", status, c.expectedPhase, c.expectedReason)
			}
		})
	}
}

// A splice pilot with verified targets, the gate open and no fallback reports
// coverage (§3.3): the one honest success state.
func TestExtenderCamouflageStatusCovered(t *testing.T) {
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	status := fixture.server.CamouflageStatus()
	if !status.Covered {
		t.Fatalf("status = %+v, expected covered", status)
	}
	if status.Phase != ExtenderCamouflagePhaseSplicePilot {
		t.Fatalf("phase = %q, expected splice_pilot", status.Phase)
	}
}

// Camouflage off is the off phase, never coverage, whatever the posture (§3.3).
func TestExtenderCamouflageStatusOff(t *testing.T) {
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = false
		settings.IdentityKeySeed = mustSeed(t)
	})
	status := fixture.server.CamouflageStatus()
	if status.Covered || status.Phase != ExtenderCamouflagePhaseOff {
		t.Fatalf("status = %+v, expected off and not covered", status)
	}
}

// The per-target load accumulates begins and relayed bytes for one borrowed
// site, so a fleet telemetry can sum one target's load across extenders (§3.3):
// per-extender caps alone do not bound a whole fleet's traffic to one site. The
// bytes are counted when the splice ends, and the prober sees its stream end
// before that, so the site's side is held closed past the prober's end and the
// count is read once the close has joined the splice.
func TestExtenderCamouflageTargetLoadMeasured(t *testing.T) {
	floodByteCount := 4096
	site := newFakeBorrowSite(t, floodBorrowSiteHandler(floodByteCount))
	siteCloseRelease := make(chan struct{})
	releaseSiteClose := sync.OnceFunc(func() { close(siteCloseRelease) })
	defer releaseSiteClose()
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = mustSeed(t)
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContextHoldingClose(siteCloseRelease)
	})
	receivedByteCount := spliceProberReceiveByteCount(t, fixture)
	releaseSiteClose()
	fixture.server.CloseAndWait()

	loads := fixture.server.CamouflageStats().TargetLoads
	load, ok := loads[testBorrowName]
	if !ok {
		t.Fatalf("no per-target load recorded for %s: %v", testBorrowName, loads)
	}
	if load.SpliceCount == 0 {
		t.Fatal("per-target splice count did not increment")
	}
	if load.RelayedByteCount == 0 {
		t.Fatal("per-target relayed-byte count did not increment")
	}
	if load.RelayedByteCount != int64(receivedByteCount) || receivedByteCount != floodByteCount {
		t.Fatalf("per-target relayed bytes = %d, the prober received %d of %d", load.RelayedByteCount, receivedByteCount, floodByteCount)
	}
}
