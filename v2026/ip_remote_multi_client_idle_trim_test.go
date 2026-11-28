// The idle trim (see ip_remote_multi_client_idle_trim.go). The composed tests
// run the idle refill hold's harness: a real multi client over in-process
// providers in a synctest bubble, with a strict 5/5/5 quality window. The
// device sends nothing unless a test sends for it, so it is idle from the
// start.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The trim's evidence: the idle trim removals counted for the quality window.
func idleTrimTestRemovals(stats *WindowClientStats) uint64 {
	return stats.removals[windowStatsWindowQuality][windowRemovalIdleTrim].Load()
}

// A device no one uses keeps its five exits for the trim time, releasing
// none before it; then its held window keeps exactly one exit and refills
// nothing; the user's next packet refills it to five through the hold's wake,
// within the expansion, not at the next resize interval. Before the trim the
// held window kept all five for as long as the host held the device.
func TestIdleDeviceWindowTrimsToOneExitAndRefillsWhenTheUserReturns(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.IdleRefillHold = idleRefillTestHold
		})
		window := h.multiClient.windows[WindowTypeQuality]
		AssertEqual(t, window.idleTrimTimeout(), idleRefillTestHold)

		// inside the trim time: five exits, nothing released
		h.sleep(idleRefillTestHold - time.Minute)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		AssertEqual(t, idleTrimTestRemovals(stats), uint64(0))

		// past the trim time: one exit; the other four were released as idle
		// trims, and the held window builds nothing
		h.sleep(2 * time.Minute)
		installedCount, selectableCount = strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 1)
		AssertEqual(t, selectableCount, 1)
		AssertEqual(t, idleTrimTestRemovals(stats), uint64(4))
		reads, _, mints, _ := generator.counts()
		h.sleep(10 * time.Minute)
		readsDuring, _, mintsDuring, _ := generator.counts()
		AssertEqual(t, readsDuring, reads)
		AssertEqual(t, mintsDuring, mints)
		installedCount, _ = strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 1)
		if !window.idleRefillWake.Load() {
			t.Fatal("the trimmed window does not wait for the user's activity")
		}

		// the user comes back: their first packet refills the window within
		// one expansion
		h.syn(reraceFlow(4, 0, 40010))
		h.sleep(2 * time.Second)
		installedCount, selectableCount = strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		_, _, mintsAfter, _ := generator.counts()
		AssertEqual(t, mintsAfter-mints, 4)
		AssertEqual(t, idleTrimTestRemovals(stats), uint64(4))
	})
}

// With a tcp sequence idle timeout longer than the idle refill hold, the trim
// waits for the longer of the two: between them the window is held and keeps
// its five exits. The exit carrying a live flow is never released: upkeep the
// host does not count as user activity (as a hosted device's DoH keepalive
// pings) keeps the user's flow live past the trim time, and the window keeps
// exactly that exit, with the flow still bound to it.
func TestIdleTrimWaitsForTheLongerSequenceIdleTimeoutAndKeepsTheFlowsExit(t *testing.T) {
	const tcpSequenceIdleTimeout = 15 * time.Minute
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		host := &idleRefillTestHost{}
		h, _, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.IdleRefillHold = idleRefillTestHold
			settings.UserActivitySince = host.activitySince
			settings.TcpSequenceIdleTimeout = tcpSequenceIdleTimeout
		})
		window := h.multiClient.windows[WindowTypeQuality]
		AssertEqual(t, window.idleTrimTimeout(), tcpSequenceIdleTimeout)
		h.sleep(time.Minute)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 5 {
			t.Fatalf("the window formed with %d clients, want 5", installedCount)
		}

		// the user's last activity: one connection, bound to one exit
		host.recordActivity()
		path, owner := bindStrictHandoverTestFlow(t, h, 0)
		sequenceNumber := path.SequenceNumber + 1
		upkeep := func(until time.Duration) {
			t.Helper()
			for end := time.Now().Add(until); time.Now().Before(end); {
				payload := []byte("upkeep")
				h.send(reraceTcpPacket(path, tcpFlagAck|tcpFlagPsh, sequenceNumber, reraceServerIsn+1, payload))
				sequenceNumber += uint32(len(payload))
				h.sleep(30 * time.Second)
			}
		}

		// past the hold, inside the longer tcp timeout: held, not trimmed
		upkeep(idleRefillTestHold + 2*time.Minute)
		if !window.idleRefillHeld(time.Now()) {
			t.Fatal("the window's refill is not held past the hold")
		}
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, idleTrimTestRemovals(stats), uint64(0))

		// past the longer tcp timeout: one exit, the flow's
		upkeep(tcpSequenceIdleTimeout - idleRefillTestHold)
		installedCount, _ = strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 1)
		AssertEqual(t, idleTrimTestRemovals(stats), uint64(4))
		if !strictHandoverTestInstalled(window, owner) {
			t.Fatal("the trim released the exit carrying a live flow")
		}
		if h.boundTo(path) != owner {
			t.Fatal("the live flow lost its exit")
		}
	})
}

// The trim time is the longer of the hold and the sequence idle timeouts, and
// is zero without the hold or for a fixed destination.
func TestIdleTrimTimeoutIsTheLongerOfTheHoldAndTheSequenceIdleTimeouts(t *testing.T) {
	window := newBalanceHoldTestWindow(&balanceHoldFixedProbeGenerator{outcome: BalanceProbeRestored}, true)
	AssertEqual(t, window.idleTrimTimeout(), time.Duration(0))

	for _, c := range []struct {
		hold        time.Duration
		sequence    time.Duration
		tcp         time.Duration
		wantTimeout time.Duration
	}{
		// the hosted low memory profile: 5 minutes for others, 3 for tcp
		{hold: 10 * time.Minute, sequence: 5 * time.Minute, tcp: 3 * time.Minute, wantTimeout: 10 * time.Minute},
		{hold: 10 * time.Minute, sequence: 5 * time.Minute, tcp: 15 * time.Minute, wantTimeout: 15 * time.Minute},
		{hold: 10 * time.Minute, sequence: 12 * time.Minute, tcp: 0, wantTimeout: 12 * time.Minute},
	} {
		window.settings.IdleRefillHold = c.hold
		window.settings.SequenceIdleTimeout = c.sequence
		window.settings.TcpSequenceIdleTimeout = c.tcp
		if timeout := window.idleTrimTimeout(); timeout != c.wantTimeout {
			t.Errorf("hold %s, sequence %s, tcp %s: trim time %s, want %s", c.hold, c.sequence, c.tcp, timeout, c.wantTimeout)
		}
	}

	fixedWindow := newBalanceHoldTestWindow(&TestMultiClientGenerator{}, true)
	fixedWindow.settings.IdleRefillHold = 10 * time.Minute
	AssertEqual(t, fixedWindow.idleTrimTimeout(), time.Duration(0))
}

// The trim is due only for a window older than the trim time whose device has
// been idle for all of it; a window with no parent to ask reads as active.
func TestIdleTrimDueNeedsTheWholeTrimTimeIdle(t *testing.T) {
	now := time.Now()
	idle := &balanceRestoreTestParent{}
	window := newBalanceHoldTestWindow(&balanceHoldFixedProbeGenerator{outcome: BalanceProbeRestored}, true)
	window.settings.IdleRefillHold = idleRefillTestHold
	window.settings.SequenceIdleTimeout = 5 * time.Minute
	window.settings.TcpSequenceIdleTimeout = 3 * time.Minute
	window.userSentSinceFunc = idle.userSentSince
	window.idleRefillSince = now.Add(-2 * idleRefillTestHold)
	if !window.idleTrimDue(now) {
		t.Fatal("an idle device's window older than the trim time is not due")
	}

	// active inside the trim time: not due
	active := &balanceRestoreTestParent{lastSend: now.Add(-idleRefillTestHold + time.Second)}
	window.userSentSinceFunc = active.userSentSince
	if window.idleTrimDue(now) {
		t.Fatal("a window trimmed while its device was active")
	}

	// inside its first trim time: not due
	window.userSentSinceFunc = idle.userSentSince
	window.idleRefillSince = now.Add(-idleRefillTestHold + time.Second)
	if window.idleTrimDue(now) {
		t.Fatal("a new window trimmed")
	}

	// no parent: reads as active
	window.idleRefillSince = now.Add(-2 * idleRefillTestHold)
	window.userSentSinceFunc = nil
	if window.idleTrimDue(now) {
		t.Fatal("a window with no parent trimmed")
	}

	// the setting off: never due
	window.userSentSinceFunc = idle.userSentSince
	window.settings.IdleRefillHold = 0
	if window.idleTrimDue(now) {
		t.Fatal("a window trimmed with the hold off")
	}
}

// The removal cause's label.
func TestIdleTrimRemovalCauseLabel(t *testing.T) {
	AssertEqual(t, windowRemovalIdleTrim.String(), "idle_trim")
}
