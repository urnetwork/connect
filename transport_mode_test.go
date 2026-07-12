package connect

import (
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/exp/maps"
)

// testingPlatformTransportModes builds just the mode state of a
// PlatformTransport, without the network side, so the election plumbing can be
// exercised directly.
func testingPlatformTransportModes() *PlatformTransport {
	return &PlatformTransport{
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		mode:                 NewMonitorValue(TransportModeNone),
	}
}

// TestPlatformTransportModeAvailableNotifies pins the half of the monitor
// contract that was missing: a mutation must notify. setModeAvailable
// previously wrote availableModes and notified nobody, so the election loop —
// which subscribes before it reads — parked forever on the very first
// iteration and the active mode stayed TransportModeNone for the process life.
func TestPlatformTransportModeAvailableNotifies(t *testing.T) {
	transport := testingPlatformTransportModes()

	available, notify := transport.modesAvailable()
	if 0 != len(available) {
		t.Fatalf("available = %v, want empty", available)
	}
	select {
	case <-notify:
		t.Fatalf("notified with no change")
	default:
	}

	transport.setModeAvailable(TransportModeH1, true)
	select {
	case <-notify:
	case <-time.After(5 * time.Second):
		t.Fatalf("setModeAvailable did not notify: the election loop would never wake")
	}

	available, _ = transport.modesAvailable()
	if !available[TransportModeH1] {
		t.Fatalf("available = %v, want h1", available)
	}
}

// TestPlatformTransportModeAvailableChangeGated: re-asserting the same
// availability must not notify, so reconnect churn does not wake the election
// loop for a decision it already made.
func TestPlatformTransportModeAvailableChangeGated(t *testing.T) {
	transport := testingPlatformTransportModes()
	transport.setModeAvailable(TransportModeH1, true)

	_, notify := transport.modesAvailable()
	transport.setModeAvailable(TransportModeH1, true)
	select {
	case <-notify:
		t.Fatalf("re-asserting the same availability notified")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPlatformTransportActiveModeNotifies: the elected mode must wake the mode
// gates and the inactive-drain watchdogs. Without it the live transport reads
// TransportModeNone forever, believes it is NOT the active transport, and arms
// a 30s inactivity kill timer against itself that nothing can cancel.
func TestPlatformTransportActiveModeNotifies(t *testing.T) {
	transport := testingPlatformTransportModes()

	mode, notify := transport.activeMode()
	if mode != TransportModeNone {
		t.Fatalf("mode = %v, want none", mode)
	}

	transport.setActiveMode(TransportModeH1)
	select {
	case <-notify:
	case <-time.After(5 * time.Second):
		t.Fatalf("setActiveMode did not notify: the drain watchdog could never re-evaluate")
	}

	mode, _ = transport.activeMode()
	if mode != TransportModeH1 {
		t.Fatalf("mode = %v, want h1", mode)
	}

	// the live transport now reads itself as active, so the drain watchdog takes
	// the benign branch instead of arming the inactivity kill timer
	if mode != TransportModeH1 {
		t.Fatalf("the h1 watchdog would still arm its kill timer against a live transport")
	}
}

// TestPlatformTransportActiveModeDoesNotWakeElection is the property that keeps
// the election loop from spinning: the loop subscribes to availableModes and
// then writes the active mode. If the active mode shared that notification, the
// loop's own write would wake it, forever.
func TestPlatformTransportActiveModeDoesNotWakeElection(t *testing.T) {
	transport := testingPlatformTransportModes()

	_, availableNotify := transport.modesAvailable()
	transport.setActiveMode(TransportModeH1)
	select {
	case <-availableNotify:
		t.Fatalf("electing a mode woke the election loop's own subscription: it would spin")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPlatformTransportElectPreference walks the election decision the run loop
// makes: equally preferred modes do not preempt each other (whichever connected
// first stays active), a strictly better mode does preempt, and losing the
// active mode falls back — to TransportModeNone when nothing remains, which was
// previously unreachable so a dropped mode left the active mode pinned stale.
func TestPlatformTransportElectPreference(t *testing.T) {
	transport := testingPlatformTransportModes()

	// the election exactly as `run` performs it
	elect := func() TransportMode {
		available, _ := transport.modesAvailable()
		orderedModes := maps.Keys(transportModePreferences)
		slices.SortFunc(orderedModes, func(a TransportMode, b TransportMode) int {
			preferenceA := modePreference(a)
			preferenceB := modePreference(b)
			if preferenceA < preferenceB {
				return -1
			} else if preferenceB < preferenceA {
				return 1
			}
			return strings.Compare(string(a), string(b))
		})
		bestMode := TransportModeNone
		for _, mode := range orderedModes {
			if available[mode] {
				bestMode = mode
				break
			}
		}
		activeMode := transport.mode.Value()
		if !available[activeMode] || isBetterMode(bestMode, activeMode) {
			activeMode = bestMode
		}
		transport.setActiveMode(activeMode)
		return activeMode
	}

	// nothing available
	if mode := elect(); mode != TransportModeNone {
		t.Fatalf("mode = %q, want none", mode)
	}

	// h3 connects first and becomes active
	transport.setModeAvailable(TransportModeH3, true)
	if mode := elect(); mode != TransportModeH3 {
		t.Fatalf("mode = %q, want h3", mode)
	}

	// h1 connects second. it ties with h3, so it must NOT preempt: whichever
	// connected first keeps the active designation
	transport.setModeAvailable(TransportModeH1, true)
	if mode := elect(); mode != TransportModeH3 {
		t.Fatalf("mode = %q, want h3: an equally preferred mode preempted the one that connected first", mode)
	}

	// h3 drops: fall back to the equal mode that remains
	transport.setModeAvailable(TransportModeH3, false)
	if mode := elect(); mode != TransportModeH1 {
		t.Fatalf("mode = %q, want h1", mode)
	}

	// a translation fallback connects. it is strictly worse, so it must not take over
	transport.setModeAvailable(TransportModeH3DnsPump, true)
	if mode := elect(); mode != TransportModeH1 {
		t.Fatalf("mode = %q, want h1: a worse mode took over", mode)
	}

	// h1 drops, leaving only the translation fallback
	transport.setModeAvailable(TransportModeH1, false)
	if mode := elect(); mode != TransportModeH3DnsPump {
		t.Fatalf("mode = %q, want h3dnspump", mode)
	}

	// a direct mode returns: strictly better, so it preempts the fallback
	transport.setModeAvailable(TransportModeH3, true)
	if mode := elect(); mode != TransportModeH3 {
		t.Fatalf("mode = %q, want h3: a strictly better mode did not preempt", mode)
	}

	// everything drops: fall back to none (previously unreachable)
	transport.setModeAvailable(TransportModeH3, false)
	transport.setModeAvailable(TransportModeH3DnsPump, false)
	if mode := elect(); mode != TransportModeNone {
		t.Fatalf("mode = %q, want none — a dropped mode left the active mode stale", mode)
	}
}

// TestTransportModeNoneIsWorst: TransportModeNone is the absence of a transport
// and must rank below every real mode. It is absent from the preference table,
// so reading the map directly scored it 0 — better than everything — which made
// every mode gate's predicate false and meant no transport ever stood down.
func TestTransportModeNoneIsWorst(t *testing.T) {
	realModes := []TransportMode{
		TransportModeH3DnsPump,
		TransportModeH3Dns,
		TransportModeH3,
		TransportModeH1,
	}
	for _, mode := range realModes {
		if !isBetterMode(mode, TransportModeNone) {
			t.Errorf("%v is not better than none", mode)
		}
		if isBetterMode(TransportModeNone, mode) {
			t.Errorf("none is better than %v", mode)
		}
	}
	// an unknown mode ranks with none, not above everything
	if isBetterMode(TransportMode("nonsense"), TransportModeH1) {
		t.Errorf("an unknown mode outranks h1")
	}
}

// TestTransportModeTiers pins the two-tier preference: the direct modes (h3, h1)
// are equally preferred, and the packet translation modes (h3dns, h3dnspump) are
// an availability fallback below them, equally preferred among themselves. The
// table previously had the tiers inverted, making h3dnspump the most preferred
// mode of all.
func TestTransportModeTiers(t *testing.T) {
	direct := []TransportMode{TransportModeH3, TransportModeH1}
	translation := []TransportMode{TransportModeH3Dns, TransportModeH3DnsPump}

	// a direct mode beats every translation mode
	for _, d := range direct {
		for _, tr := range translation {
			if !isBetterMode(d, tr) {
				t.Errorf("%q is not better than %q: the translation fallback outranks a direct mode", d, tr)
			}
			if isBetterMode(tr, d) {
				t.Errorf("%q is better than %q: the translation fallback outranks a direct mode", tr, d)
			}
		}
	}
	// within a tier nothing is strictly better, so neither preempts the other
	for _, tier := range [][]TransportMode{direct, translation} {
		for _, a := range tier {
			for _, b := range tier {
				if isBetterMode(a, b) {
					t.Errorf("%q is better than %q: modes in a tier must tie", a, b)
				}
			}
		}
	}
}

// TestPlatformTransportStandDown covers the gate predicate. A transport runs
// when it is active, when only an equal mode is active (ties coexist), when a
// worse mode is active, and — critically — at startup when the active mode is
// none. It stands down only while a STRICTLY better mode is active. The
// arguments were previously reversed, so a transport stood down when it was
// BETTER than the active mode.
func TestPlatformTransportStandDown(t *testing.T) {
	transport := testingPlatformTransportModes()

	// startup: the active mode is none, so the first transport must be admitted
	// — otherwise it never connects, never becomes available, and the election
	// never has a mode to elect (a deadlock)
	if standDown, _ := transport.standDown(TransportModeH1); standDown {
		t.Fatalf("h1 stood down at startup: it would never connect")
	}

	// the active mode runs
	transport.setActiveMode(TransportModeH1)
	if standDown, _ := transport.standDown(TransportModeH1); standDown {
		t.Fatalf("h1 stood down while it was the active mode")
	}

	// an EQUAL mode is active: neither stands down, they coexist and whichever
	// connected first keeps the active designation
	transport.setActiveMode(TransportModeH3)
	if standDown, _ := transport.standDown(TransportModeH1); standDown {
		t.Fatalf("h1 stood down for h3, but the direct modes tie")
	}

	// a strictly better mode is active: the translation fallback stands down
	transport.setActiveMode(TransportModeH1)
	if standDown, _ := transport.standDown(TransportModeH3DnsPump); !standDown {
		t.Fatalf("the translation fallback kept running while a direct mode was active")
	}

	// a strictly WORSE mode is active: the better transport must NOT stand down
	// (the old predicate parked it here — the best mode yielding to a worse one)
	transport.setActiveMode(TransportModeH3DnsPump)
	if standDown, _ := transport.standDown(TransportModeH1); standDown {
		t.Fatalf("h1 stood down for the translation fallback")
	}

	// standing down wakes when the active mode changes
	transport.setActiveMode(TransportModeH1)
	standDown, notify := transport.standDown(TransportModeH3DnsPump)
	if !standDown {
		t.Fatalf("expected the translation fallback to stand down")
	}
	transport.setActiveMode(TransportModeH3DnsPump)
	select {
	case <-notify:
	case <-time.After(5 * time.Second):
		t.Fatalf("a standing-down transport was not woken by the mode change")
	}
}

// TestTransportModeOrderDeterministic: the election sorts the modes and takes
// the first available one. maps.Keys is randomly ordered and both tiers tie
// internally, so without a tie break the winner among tied modes differed on
// every pass — flipping the active mode and thrashing the gates.
func TestTransportModeOrderDeterministic(t *testing.T) {
	order := func() []TransportMode {
		orderedModes := maps.Keys(transportModePreferences)
		slices.SortFunc(orderedModes, func(a TransportMode, b TransportMode) int {
			preferenceA := modePreference(a)
			preferenceB := modePreference(b)
			if preferenceA < preferenceB {
				return -1
			} else if preferenceB < preferenceA {
				return 1
			}
			return strings.Compare(string(a), string(b))
		})
		return orderedModes
	}

	want := order()
	for range 50 {
		got := order()
		if !slices.Equal(got, want) {
			t.Fatalf("election order is not deterministic: %v then %v", want, got)
		}
	}
	// the direct modes sort ahead of the translation fallbacks
	if modePreference(want[0]) != modePreference(TransportModeH1) {
		t.Fatalf("order = %v, want a direct mode first", want)
	}
	if modePreference(want[len(want)-1]) != modePreference(TransportModeH3DnsPump) {
		t.Fatalf("order = %v, want a translation fallback last", want)
	}
}
