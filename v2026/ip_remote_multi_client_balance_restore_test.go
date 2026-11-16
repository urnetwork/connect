// The restore spread (see ip_remote_multi_client_balance_hold.go): a window
// whose balance probe finds the balance restored while its device is idle
// waits a fixed offset, a function of its key, before it refills; a device a
// user is sending on refills at once, as before, and a user packet ends the
// wait. On Main at 00:05Z on 2026-11-05 the daily free grant restored the held
// windows of every free payer that had run dry within one expansion backoff,
// each refilled in that pass, and both proxy hosts went from 16 to 31 cores in
// ten minutes.
package connect

import (
	"encoding/binary"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The test key numbered i.
func balanceRestoreTestKey(i int) Id {
	var key Id
	binary.BigEndian.PutUint64(key[8:], uint64(i))
	return key
}

// A key whose offset under the default spread falls in [low, high), and the
// offset.
func balanceRestoreTestKeyWithOffset(t testing.TB, low time.Duration, high time.Duration) (Id, time.Duration) {
	t.Helper()
	spread := DefaultMultiClientSettings().BalanceRestoreSpread
	for i := 0; i < 1<<16; i += 1 {
		key := balanceRestoreTestKey(i)
		if offset := balanceRestoreOffset(key, spread); low <= offset && offset < high {
			return key, offset
		}
	}
	t.Fatalf("no test key has an offset in [%s, %s)", low, high)
	return Id{}, 0
}

// The offset is a pure function of the key, inside the spread, and zero without
// one.
func TestBalanceRestoreOffsetIsAFunctionOfTheKey(t *testing.T) {
	spread := 5 * time.Minute
	for i := 0; i < 1000; i += 1 {
		key := balanceRestoreTestKey(i)
		offset := balanceRestoreOffset(key, spread)
		if offset < 0 || spread <= offset {
			t.Fatalf("key %d: offset %s outside [0, %s)", i, offset, spread)
		}
		AssertEqual(t, balanceRestoreOffset(key, spread), offset)
		AssertEqual(t, balanceRestoreOffset(key, 0), time.Duration(0))
		AssertEqual(t, balanceRestoreOffset(key, -time.Second), time.Duration(0))
	}
}

// A bare discovery window with the hold on, whose parent reports the device's
// latest user packet as `lastSend` (zero: none).
type balanceRestoreTestParent struct {
	lastSend time.Time
}

func (self *balanceRestoreTestParent) userSentSince(since time.Time) bool {
	return !self.lastSend.IsZero() && !self.lastSend.Before(since)
}

func newBalanceRestoreTestWindow(parent *balanceRestoreTestParent, key Id) *multiClientWindow {
	window := newBalanceHoldTestWindow(&balanceHoldFixedProbeGenerator{outcome: BalanceProbeRestored}, true)
	window.balanceRestoreKey = key
	if parent != nil {
		window.userSentSinceFunc = parent.userSentSince
	}
	return window
}

// Holds the window on a refusal for the balance and runs one expansion step,
// whose probe finds the balance restored. True when the step builds nothing.
func balanceRestoreTestStep(window *multiClientWindow) bool {
	short := protocol.ContractError_InsufficientBalance
	window.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), &short))
	return window.probeBalanceHold(NewWindowClientStats())
}

// The defect: one grant restores many held windows in the same pass, and each
// refilled in it. Now the windows of idle devices restored at the same instant
// refill at their own offsets, spread evenly over BalanceRestoreSpread: no
// resize interval of it takes more than twice its even share, where before
// every window refilled in the first.
func TestRestoredWindowsOfIdleDevicesSpreadTheirRefill(t *testing.T) {
	settings := DefaultMultiClientSettings()
	spread := settings.BalanceRestoreSpread
	interval := settings.WindowResizeTimeout
	AssertEqual(t, spread, 5*time.Minute)
	parent := &balanceRestoreTestParent{}
	count := 600
	buckets := make([]int, int(spread/interval))
	for i := 0; i < count; i += 1 {
		window := newBalanceRestoreTestWindow(parent, balanceRestoreTestKey(i))
		restored := time.Now()
		if !balanceRestoreTestStep(window) {
			t.Fatalf("window %d of an idle device refilled in the pass that found the balance", i)
		}
		notBefore, waiting := window.balanceRestoreDeferral(restored)
		if !waiting {
			t.Fatalf("window %d does not wait after its restore", i)
		}
		offset := notBefore.Sub(restored)
		// the probe's step ran at most a moment after `restored`
		if want := balanceRestoreOffset(balanceRestoreTestKey(i), spread); offset < want || want+time.Second < offset {
			t.Fatalf("window %d waits %s, want its offset %s", i, offset, want)
		}
		buckets[min(len(buckets)-1, int(offset/interval))] += 1
	}
	even := count / len(buckets)
	for i, n := range buckets {
		if 2*even < n {
			t.Fatalf("the resize interval at %s of the spread refills %d of %d windows, more than twice its even share %d", time.Duration(i)*interval, n, count, even)
		}
	}
}

// A device that sent a user packet within BalanceRestoreSpreadIdle, a window with
// no parent to ask, a zero spread, and a probe that could not answer all expand
// in the pass that found the balance, exactly as before the spread.
func TestRestoredWindowExpandsAtOnceWhenTheSpreadDoesNotApply(t *testing.T) {
	idle := DefaultMultiClientSettings().BalanceRestoreSpreadIdle
	key, _ := balanceRestoreTestKeyWithOffset(t, time.Minute, 4*time.Minute)

	sending := newBalanceRestoreTestWindow(&balanceRestoreTestParent{lastSend: time.Now().Add(-idle + time.Second)}, key)
	if balanceRestoreTestStep(sending) {
		t.Fatal("a device a user sent on inside the idle time waits after its restore")
	}
	bare := newBalanceRestoreTestWindow(nil, key)
	if balanceRestoreTestStep(bare) {
		t.Fatal("a window with no parent waits after its restore")
	}
	off := newBalanceRestoreTestWindow(&balanceRestoreTestParent{}, key)
	off.settings.BalanceRestoreSpread = 0
	if balanceRestoreTestStep(off) {
		t.Fatal("a window waits after its restore with no spread")
	}
	unknown := newBalanceRestoreTestWindow(&balanceRestoreTestParent{}, key)
	unknown.generator = &balanceHoldFixedProbeGenerator{outcome: BalanceProbeUnknown}
	if balanceRestoreTestStep(unknown) {
		t.Fatal("a probe that could not answer waits as a restore")
	}
	for _, window := range []*multiClientWindow{sending, bare, off, unknown} {
		if _, waiting := window.balanceRestoreDeferral(time.Now()); waiting {
			t.Fatal("a window that expanded at once still waits")
		}
	}

	// a device idle just past the idle time waits
	stale := newBalanceRestoreTestWindow(&balanceRestoreTestParent{lastSend: time.Now().Add(-idle - time.Second)}, key)
	if !balanceRestoreTestStep(stale) {
		t.Fatal("a device idle past the idle time refilled at once")
	}
}

// The wait ends at the window's offset; at the device's first user packet,
// which wakes the resize pass; or on a new refusal for the balance, which holds
// the window again. While it waits, the enumerator lists and mints nothing.
func TestBalanceRestoreWaitEnds(t *testing.T) {
	key, offset := balanceRestoreTestKeyWithOffset(t, time.Minute, 4*time.Minute)

	// at the offset
	parent := &balanceRestoreTestParent{}
	window := newBalanceRestoreTestWindow(parent, key)
	restored := time.Now()
	if !balanceRestoreTestStep(window) {
		t.Fatal("an idle device's window refilled at its restore")
	}
	if !window.balanceListingHeld() {
		t.Fatal("the enumerator may list for a window that waits")
	}
	notBefore, waiting := window.balanceRestoreDeferral(restored)
	if !waiting {
		t.Fatal("the window does not wait")
	}
	if _, waiting := window.balanceRestoreDeferral(notBefore.Add(-time.Millisecond)); !waiting {
		t.Fatal("the wait ended before the offset")
	}
	if _, waiting := window.balanceRestoreDeferral(notBefore); waiting {
		t.Fatal("the wait outlasted the offset")
	}
	if notBefore.Sub(restored) < offset {
		t.Fatalf("the window waited %s, less than its offset %s", notBefore.Sub(restored), offset)
	}
	if window.balanceListingHeld() {
		t.Fatal("the enumerator stays quiet after the wait")
	}

	// at a user packet, which wakes the pass
	parent = &balanceRestoreTestParent{}
	window = newBalanceRestoreTestWindow(parent, key)
	if !balanceRestoreTestStep(window) {
		t.Fatal("an idle device's window refilled at its restore")
	}
	if !window.balanceRestoreWake.Load() {
		t.Fatal("a waiting window does not ask the send path to wake it")
	}
	resized := window.resizeMonitor.NotifyChannel()
	parent.lastSend = time.Now()
	window.wakeBalanceRestore()
	select {
	case <-resized:
	default:
		t.Fatal("the user packet did not wake the resize pass")
	}
	if _, waiting := window.balanceRestoreDeferral(time.Now()); waiting {
		t.Fatal("the wait outlasted the device's user packet")
	}
	if window.balanceRestoreWake.Load() {
		t.Fatal("a window that no longer waits still asks to be woken")
	}

	// on a refusal for the balance, which holds it again
	window = newBalanceRestoreTestWindow(&balanceRestoreTestParent{}, key)
	if !balanceRestoreTestStep(window) {
		t.Fatal("an idle device's window refilled at its restore")
	}
	short := protocol.ContractError_InsufficientBalance
	window.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), &short))
	if _, waiting := window.balanceRestoreDeferral(time.Now()); waiting {
		t.Fatal("a refusal for the balance left the restore wait")
	}
	if _, held := window.balanceRefillHeld(); !held {
		t.Fatal("a refusal for the balance did not hold the window again")
	}
}

// A live client's granted contract also ends the hold: the balance carries
// contracts again. An idle device's window waits its offset after it, as after
// a restored probe, where it used to expand at once; a device a user is sending
// on still expands at once.
func TestGrantedContractRestoreWaitsForAnIdleDevice(t *testing.T) {
	short := protocol.ContractError_InsufficientBalance
	key, offset := balanceRestoreTestKeyWithOffset(t, time.Minute, 4*time.Minute)

	idle := newBalanceRestoreTestWindow(&balanceRestoreTestParent{}, key)
	idle.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), &short))
	granted := time.Now()
	idle.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), nil))
	if _, held := idle.balanceRefillHeld(); held {
		t.Fatal("a granted contract left the window held")
	}
	notBefore, waiting := idle.balanceRestoreDeferral(granted)
	if !waiting {
		t.Fatal("an idle device's window expands at once after a granted contract")
	}
	if wait := notBefore.Sub(granted); wait < offset || offset+time.Second < wait {
		t.Fatalf("the window waits %s after the granted contract, want its offset %s", wait, offset)
	}
	// a granted contract while nothing is held changes nothing
	idle.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), nil))
	if after, _ := idle.balanceRestoreDeferral(granted); !after.Equal(notBefore) {
		t.Fatalf("a second granted contract moved the wait from %s to %s", notBefore, after)
	}

	sending := newBalanceRestoreTestWindow(&balanceRestoreTestParent{lastSend: time.Now()}, key)
	sending.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), &short))
	sending.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), nil))
	if _, waiting := sending.balanceRestoreDeferral(time.Now()); waiting {
		t.Fatal("a device a user is sending on waits after a granted contract")
	}
}

// Runs the composed held window to its top-up. Returns the window and the step
// at which its probe will find the balance restored.
func balanceRestoreTestTopUp(t *testing.T, h *reraceHarness, generator *balanceHoldTestGenerator, key Id) (*multiClientWindow, time.Time) {
	t.Helper()
	window := h.multiClient.windows[WindowTypeQuality]
	window.balanceRestoreKey = key
	balanceHoldTestRunOutAndLoseAnExit(t, h, generator)
	h.sleep(5 * time.Minute)
	if _, ok := window.balanceRefillHeld(); !ok {
		t.Fatal("the window was not held after its refill's contracts were refused")
	}
	generator.setShort(false)
	window.stateLock.Lock()
	nextStep := window.discoveryRetry.notBefore
	window.stateLock.Unlock()
	if !nextStep.After(time.Now()) {
		t.Fatalf("the held window has no next expansion step after the top-up")
	}
	return window, nextStep
}

// No user has sent on the device for the hold's five minutes: its probe finds
// the balance restored at the step and the window refills exactly its offset
// later, listing and minting nothing in between. Before the spread it refilled
// at the step.
func TestIdleDeviceRefillWaitsItsOffsetAfterATopUp(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.OutOfBalanceRefillProbe = true
		})
		key, offset := balanceRestoreTestKeyWithOffset(t, 2*time.Minute, 3*time.Minute)
		window, nextStep := balanceRestoreTestTopUp(t, h, generator, key)

		h.sleep(nextStep.Sub(time.Now()) + time.Second)
		AssertEqual(t, stats.balanceProbes[windowStatsWindowQuality][BalanceProbeRestored].Load(), uint64(1))
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("the idle device's window refilled to %d at the step that found the balance", installedCount)
		}
		window.stateLock.Lock()
		restore := window.balanceRestore
		window.stateLock.Unlock()
		AssertEqual(t, restore.notBefore.Sub(restore.since), offset)
		if restore.since.Before(nextStep) || nextStep.Add(time.Second).Before(restore.since) {
			t.Fatalf("the restore was found at %s, not at the step %s", restore.since, nextStep)
		}
		reads, _, mints, _ := generator.counts()

		// nothing is listed, minted or built before the offset
		h.sleep(restore.notBefore.Sub(time.Now()) - time.Second)
		readsDuring, _, mintsDuring, _ := generator.counts()
		AssertEqual(t, readsDuring, reads)
		AssertEqual(t, mintsDuring, mints)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("the window refilled to %d before its offset", installedCount)
		}

		// and the offset refills it
		h.sleep(3 * time.Second)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		if _, waiting := window.balanceRestoreDeferral(time.Now()); waiting {
			t.Fatal("the window still waits after its offset")
		}
	})
}

// A user comes back to the idle device while its window waits: the first
// connection attempt ends the wait and the window refills at once, long before
// its offset.
func TestUserPacketEndsTheRestoreWait(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, _ := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.OutOfBalanceRefillProbe = true
		})
		key, _ := balanceRestoreTestKeyWithOffset(t, 4*time.Minute, 5*time.Minute)
		window, nextStep := balanceRestoreTestTopUp(t, h, generator, key)

		h.sleep(nextStep.Sub(time.Now()) + 30*time.Second)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("the idle device's window refilled to %d before the user came back", installedCount)
		}
		window.stateLock.Lock()
		notBefore := window.balanceRestore.notBefore
		window.stateLock.Unlock()
		if notBefore.Sub(time.Now()) < 3*time.Minute {
			t.Fatalf("the window waits only until %s", notBefore)
		}

		h.syn(reraceFlow(4, 0, 40002))
		h.sleep(2 * time.Second)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		if _, waiting := window.balanceRestoreDeferral(time.Now()); waiting {
			t.Fatal("the window still waits after the user's packet")
		}
	})
}

// the settings a hosted device gets: the spread is on by default
func TestBalanceRestoreSpreadDefaults(t *testing.T) {
	settings := DefaultMultiClientSettings()
	AssertEqual(t, settings.BalanceRestoreSpread, 5*time.Minute)
	AssertEqual(t, settings.BalanceRestoreSpreadIdle, time.Minute)
	// apps leave the hold off, so nothing of theirs is ever spread
	AssertEqual(t, settings.OutOfBalanceRefillProbe, false)
}
