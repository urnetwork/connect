// The idle refill hold (see ip_remote_multi_client_idle_refill.go). The
// composed tests run a real multi client over in-process providers in a
// synctest bubble (the balance hold harness: a strict 5/5/5 quality window and
// one spare provider). The device sends nothing unless a test sends for it, so
// it is idle from the start; the window loses an exit and must refill.
package connect

import (
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const idleRefillTestHold = 10 * time.Minute

// The host's record of user activity, as the proxy service keeps it: the time
// of the latest activity, zero for none.
type idleRefillTestHost struct {
	lastActivityNanos atomic.Int64
}

func (self *idleRefillTestHost) activitySince(since time.Time) bool {
	lastNanos := self.lastActivityNanos.Load()
	return lastNanos != 0 && since.UnixNano() <= lastNanos
}

func (self *idleRefillTestHost) recordActivity() {
	self.lastActivityNanos.Store(time.Now().UnixNano())
}

// Loses one of the window's exits, as a failed health check does.
func idleRefillTestLoseAnExit(h *reraceHarness) {
	lost := h.liveChannels()[0]
	lost.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
	lost.Cancel()
}

// The defect, as hosted devices run today with the hold off: a device no one
// has used for longer than the hold loses an exit, and its window refills it
// anyway, listing, minting and admitting a replacement for a device that may
// never carry traffic again before its host reaps it. Every below-target
// window of every idle hosted device does this on each step of its backoff.
func TestIdleDeviceWindowRefillsWithoutTheHold(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, _ := newBalanceHoldTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(idleRefillTestHold + time.Minute)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 5 {
			t.Fatalf("the window formed with %d clients, want 5", installedCount)
		}
		_, _, mintsBefore, _ := generator.counts()

		idleRefillTestLoseAnExit(h)
		h.sleep(10 * time.Minute)

		_, _, mints, _ := generator.counts()
		AssertEqual(t, mints-mintsBefore, 1)
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
	})
}

// With the hold, the window of a device idle for longer than the hold builds
// nothing after it loses an exit: nothing is listed, minted or admitted, and
// each resize pass counts as held_idle. The device's next user packet wakes
// the pass, which refills the window at once.
func TestIdleDeviceWindowHoldsItsRefillUntilTheUserReturns(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.IdleRefillHold = idleRefillTestHold
		})
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(idleRefillTestHold + time.Minute)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 5 {
			t.Fatalf("the window formed with %d clients, want 5", installedCount)
		}
		reads, _, mints, _ := generator.counts()
		heldBefore := stats.expandPasses[windowStatsWindowQuality][windowExpandPassHeldIdle].Load()

		idleRefillTestLoseAnExit(h)
		h.sleep(10 * time.Minute)

		readsDuring, _, mintsDuring, _ := generator.counts()
		AssertEqual(t, readsDuring, reads)
		AssertEqual(t, mintsDuring, mints)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("the idle device's window refilled to %d", installedCount)
		}
		if held := stats.expandPasses[windowStatsWindowQuality][windowExpandPassHeldIdle].Load() - heldBefore; held < 10 {
			t.Fatalf("%d held_idle passes in 10 minutes, want one per resize pass", held)
		}
		if !window.idleRefillWake.Load() {
			t.Fatal("the held window does not wait for the user's activity")
		}

		// the user comes back: their first packet refills the window within
		// one expansion, not at the next 15-second resize interval
		h.syn(reraceFlow(4, 0, 40002))
		h.sleep(2 * time.Second)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		_, _, mintsAfter, _ := generator.counts()
		AssertEqual(t, mintsAfter-mints, 1)
		if window.idleRefillHeld(time.Now()) {
			t.Fatal("the window is still held after the user's packet")
		}
	})
}

// A hosted device's tun sends its own upkeep through the device (DoH
// keepalive pings), so with the host's record of user activity set, a packet
// the host did not record as activity leaves the refill held, and the host's
// activity signal refills the window at once.
func TestHostActivityDecidesTheIdleRefillHold(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		host := &idleRefillTestHost{}
		h, generator, _ := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.IdleRefillHold = idleRefillTestHold
			settings.UserActivitySince = host.activitySince
		})
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(idleRefillTestHold + time.Minute)
		_, _, mints, _ := generator.counts()
		idleRefillTestLoseAnExit(h)
		h.sleep(time.Minute)

		// upkeep: a packet through the device that the host does not count
		for i := 0; i < 4; i += 1 {
			h.syn(reraceFlow(4, 0, 41000+i))
			h.sleep(30 * time.Second)
		}
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("upkeep packets refilled the idle device's window to %d", installedCount)
		}
		_, _, mintsDuring, _ := generator.counts()
		AssertEqual(t, mintsDuring, mints)

		// user activity, recorded and signaled by the host
		host.recordActivity()
		h.multiClient.NoteUserActivity()
		h.sleep(2 * time.Second)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
	})
}

// A device a user is active on refills exactly as without the hold: the lost
// exit is replaced in the same time.
func TestActiveDeviceWindowRefillTimingUnchangedByTheHold(t *testing.T) {
	refillTime := func(t *testing.T, hold time.Duration) (elapsed time.Duration) {
		t.Helper()
		synctest.Test(t, func(t *testing.T) {
			h, _, _ := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
				settings.IdleRefillHold = hold
			})
			window := h.multiClient.windows[WindowTypeQuality]
			h.sleep(idleRefillTestHold + time.Minute)
			// the user is active now
			h.syn(reraceFlow(4, 0, 40003))
			h.sleep(time.Second)
			lost := time.Now()
			idleRefillTestLoseAnExit(h)
			for {
				if installedCount, _ := strictHandoverTestCounts(window); installedCount == 5 {
					elapsed = time.Since(lost)
					return
				}
				if 5*time.Minute < time.Since(lost) {
					t.Fatalf("the active device's window did not refill within 5 minutes (hold %s)", hold)
				}
				h.sleep(100 * time.Millisecond)
			}
		})
		return
	}
	GetMessagePoolAggregateStats()
	without := refillTime(t, 0)
	with := refillTime(t, idleRefillTestHold)
	if with != without {
		t.Fatalf("an active device refills in %s with the hold, %s without", with, without)
	}
}

// A new device forms and refills before its first packet: the window is not
// held during its first IdleRefillHold.
func TestNewDeviceWindowRefillsBeforeItsFirstPacket(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, _ := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.IdleRefillHold = idleRefillTestHold
		})
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(2 * time.Minute)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 5 {
			t.Fatalf("the new device's window formed with %d clients before its first packet, want 5", installedCount)
		}
		_, _, mints, _ := generator.counts()
		idleRefillTestLoseAnExit(h)
		h.sleep(time.Minute)
		_, _, mintsAfter, _ := generator.counts()
		AssertEqual(t, mintsAfter-mints, 1)
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
	})
}

// The hold needs the host's opt-in and a discovery window, and a window with
// no parent to ask reads as active.
func TestIdleRefillHoldNeedsTheSettingAndADiscoveryWindow(t *testing.T) {
	now := time.Now()
	idle := &balanceRestoreTestParent{}

	window := newBalanceHoldTestWindow(&balanceHoldFixedProbeGenerator{outcome: BalanceProbeRestored}, true)
	window.userSentSinceFunc = idle.userSentSince
	window.idleRefillSince = now.Add(-2 * idleRefillTestHold)
	if window.idleRefillHeld(now) {
		t.Fatal("a window held its refill with the setting off")
	}

	window.settings.IdleRefillHold = idleRefillTestHold
	if !window.idleRefillHeld(now) {
		t.Fatal("an idle device's window older than the hold did not hold its refill")
	}
	if !window.idleRefillWake.Load() {
		t.Fatal("the held window does not wait for activity")
	}
	window.wakeIdleRefill()
	if window.idleRefillWake.Load() {
		t.Fatal("the wake left the window waiting")
	}

	// active inside the hold: not held
	active := &balanceRestoreTestParent{lastSend: now.Add(-idleRefillTestHold + time.Second)}
	window.userSentSinceFunc = active.userSentSince
	if window.idleRefillHeld(now) {
		t.Fatal("a window held its refill while its device was active")
	}

	// inside its first hold: not held
	window.userSentSinceFunc = idle.userSentSince
	window.idleRefillSince = now.Add(-idleRefillTestHold + time.Second)
	if window.idleRefillHeld(now) {
		t.Fatal("a new window held its refill")
	}

	// no parent: reads as active
	window.idleRefillSince = now.Add(-2 * idleRefillTestHold)
	window.userSentSinceFunc = nil
	if window.idleRefillHeld(now) {
		t.Fatal("a window with no parent held its refill")
	}
}

// Apps leave the hold off and read their own packets.
func TestIdleRefillHoldDefaults(t *testing.T) {
	settings := DefaultMultiClientSettings()
	AssertEqual(t, settings.IdleRefillHold, time.Duration(0))
	if settings.UserActivitySince != nil {
		t.Fatal("the default settings read a host's activity")
	}
	AssertEqual(t, windowExpandPassHeldIdle.String(), "held_idle")
}
