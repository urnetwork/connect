// The legacy fixture confused selected window class with owned window count.
// Force actual speed publication before checking that fixture premise.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// This is a fixture-readiness regression, not a new production selector bug.
// The same real speed-publication barrier runs on legacy and corrected
// fixtures; only the fixture's computed readiness target differs.
func TestPrivateCrossWindowFixedProfileReadinessAfterSpeedPublication(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		published := false
		harness := newPrivateCrossWindowHarnessWithOptions(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality"}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) {
			settings.DefaultPerformanceProfile = &PerformanceProfile{
				WindowType: WindowTypeQuality, WindowSize: settings.WindowSizes[WindowTypeQuality],
			}
		}, privateCrossWindowHarnessOptions{
			afterConstruct: func(harness *reraceHarness) {
				window := harness.multiClient.windows[WindowTypeSpeed]
				deadline := time.After(time.Minute)
				for {
					var changed <-chan struct{}
					if clients := window.orderedClients(true, 0, &changed); len(clients) == 1 {
						published = true
						t.Log("fixture publication barrier: actual speed owner=1, fixed profile size=0")
						return
					}
					select {
					case <-changed:
					case <-deadline:
						t.Fatal("actual speed owner never reached the fixture publication barrier")
					}
				}
			},
			inspectExpectedCounts: func(expectedCounts map[WindowType]int) {
				if !published {
					t.Fatal("fixture readiness inspection preceded actual speed publication")
				}
				if expectedCounts[WindowTypeSpeed] != 1 {
					t.Fatalf("causal fixture readiness: auto-sized fixed-type profile computed speed target=%d after actual publication; want 1", expectedCounts[WindowTypeSpeed])
				}
				if expectedCounts[WindowTypeQuality] != 1 {
					t.Fatal("fixed-type fixture changed its quality ownership target")
				}
			},
		})
		if !published || privateCrossWindowChannel(t, harness, "quality").IsDone() || privateCrossWindowChannel(t, harness, "speed").IsDone() {
			t.Fatal("corrected fixed-type fixture returned before both actual warm owners were ready")
		}
	})
}
