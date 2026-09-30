package connect

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Window expansion is gated on the same process-wide backend-degraded signal
// as contract creation (https://github.com/urnetwork/connect/issues/181,
// https://github.com/urnetwork/connect/issues/175). The degradation state is
// package-level, so these must not run in parallel.

func TestDegradedWindowTargetTable(t *testing.T) {
	cases := []struct {
		name        string
		target      int
		clientCount int
		degraded    bool
		want        int
	}{
		// a healthy backend sizes exactly as before
		{"healthy grows to target", 7, 3, false, 7},
		{"healthy empty window", 7, 0, false, 7},
		{"healthy disabled window", 0, 0, false, 0},
		// degraded: the outage must not grow the window
		{"degraded holds current size", 7, 3, true, 3},
		{"degraded at target", 7, 7, true, 7},
		// the cap never raises a target, including one the window is above
		{"degraded above target", 4, 6, true, 4},
		// an empty window keeps one exit's worth of formation attempts, so
		// the outcome watchdog still has evaluations to read
		{"degraded empty window", 7, 0, true, 1},
		{"degraded empty window target one", 1, 0, true, 1},
		// a disabled window must not be re-enabled by the floor
		{"degraded disabled window", 0, 0, true, 0},
		{"degraded disabled window with clients", 0, 2, true, 0},
	}
	for _, c := range cases {
		if got := degradedWindowTarget(c.target, c.clientCount, c.degraded); got != c.want {
			t.Errorf("%s: degradedWindowTarget(%d, %d, %t) = %d, want %d",
				c.name, c.target, c.clientCount, c.degraded, got, c.want)
		}
	}
}

// The window's gate reads the real process-wide signal: below the failure
// threshold it is open, at the threshold it is closed, and a single success
// reopens it.
func TestWindowBackendDegradedFollowsProcessState(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	window := &multiClientWindow{}
	if window.backendDegraded() {
		t.Fatal("healthy backend must not gate window expansion")
	}
	for i := 0; i < backendDegradedFailThreshold-1; i++ {
		noteBackendFailure()
	}
	if window.backendDegraded() {
		t.Fatal("window expansion gated below the failure threshold")
	}
	noteBackendFailure()
	if !window.backendDegraded() {
		t.Fatal("window expansion not gated after the failure threshold was reached")
	}
	noteBackendSuccess()
	if window.backendDegraded() {
		t.Fatal("window expansion still gated after a successful round-trip")
	}

	// the test seam overrides the process state
	window.backendDegradedForTest = func() bool { return true }
	if !window.backendDegraded() {
		t.Fatal("backendDegradedForTest was not consulted")
	}
}

// resize must apply the gate to its computed target (after the standing
// reserve, so the spare is gated too), and must not start a family swap while
// degraded.
func TestDegradedWindowTargetSourceAnchor(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) resize()")
	if !ok {
		t.Fatal("could not find resize")
	}

	reserve := strings.Index(body, "standingReserveTarget(")
	gate := strings.Index(body, "targetWindowSize = degradedWindowTarget(targetWindowSize, len(clients), degraded)")
	expand := strings.Index(body, "addedCount = self.expand(")
	if gate < 0 {
		t.Fatal("resize does not gate its target on the backend-degraded state")
	}
	if reserve < 0 || gate < reserve {
		t.Error("the degraded gate must apply after the standing reserve, or the spare still grows the window during an outage")
	}
	if expand < 0 || expand < gate {
		t.Error("the degraded gate must apply before expand")
	}
	if !strings.Contains(body, "degraded := self.backendDegraded()") {
		t.Error("resize does not read the window's backend-degraded seam")
	}
	if !strings.Contains(body, "if !degraded && 0 < ipv6Shortfall") {
		t.Error("a family swap can still start while the backend is degraded")
	}
}

// End to end through a live resize loop on an empty quality window: healthy,
// the window asks for its full target; with the backend degraded it asks for
// one exit's worth of formation attempts and never more.
func TestWindowResizeTargetHeldWhileDegraded(t *testing.T) {
	healthyMax := windowResizeMaxTarget(t, false)
	degradedMax := windowResizeMaxTarget(t, true)

	qualityWindow := DefaultMultiClientSettings().WindowSizes[WindowTypeQuality]
	if healthyMax < qualityWindow.WindowSizeMin {
		t.Errorf("healthy empty window asked for %d exits, want at least %d", healthyMax, qualityWindow.WindowSizeMin)
	}
	if degradedMax != 1 {
		t.Errorf("degraded empty window asked for %d exits, want 1", degradedMax)
	}
}

func windowResizeMaxTarget(t *testing.T, degraded bool) int {
	t.Helper()
	resetBackendDegraded()
	defer resetBackendDegraded()
	if degraded {
		for i := 0; i < backendDegradedFailThreshold; i++ {
			noteBackendFailure()
		}
	}

	settings := DefaultMultiClientSettings()
	settings.WindowResizeTimeout = 20 * time.Millisecond
	settings.WindowExpandTimeout = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	window := newMultiClientWindow(
		ctx,
		cancel,
		&testingEmptyMultiClientGenerator{},
		nil,
		nil,
		true,
		nil,
		DisableSecurityPolicy(),
		nil,
		WindowTypeQuality,
		nil,
		settings,
		func() *ReliabilitySettings { return ReliabilitySettingsFrom(settings) },
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	var stateLock sync.Mutex
	maxTarget := 0
	observe := func(event *WindowExpandEvent) {
		if event == nil {
			return
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		maxTarget = max(maxTarget, event.TargetSize)
	}
	remove := window.monitor.AddMonitorEventCallback(func(event *WindowExpandEvent, _ map[Id]*ProviderEvent, _ bool) {
		observe(event)
	})
	defer remove()

	// several resize passes
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		observe(window.monitor.WindowExpandEvent())
		select {
		case <-ctx.Done():
			t.Fatal("window closed")
		case <-time.After(5 * time.Millisecond):
		}
	}

	stateLock.Lock()
	defer stateLock.Unlock()
	return maxTarget
}
