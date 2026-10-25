package connect

import (
	"context"
	"testing"
	"time"
)

// Support inbox 8004/8005: with Fixed IP on, the public ip changed on every
// refresh of the same page. A sticky window can hold two live exits at once (a
// warned exit and its replacement). When both are benched, the window's
// ordinary offer is empty and new flows fall back to the last-resort offer,
// which handed every live exit to the race: each new flow left on whichever
// exit answered first, so the egress ip alternated between page loads. The
// last-resort offer of a sticky window must hold one exit, the same one on
// every call.
func TestStickyLastResortOffersOneStableExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newFamilyTestGenerator(nil)

	newMultiClient := func(window *multiClientWindow) *RemoteUserNatMultiClient {
		return &RemoteUserNatMultiClient{
			ctx:                ctx,
			cancel:             cancel,
			log:                NewNoopLogger(),
			generator:          generator,
			settings:           settings,
			windows:            map[WindowType]*multiClientWindow{WindowTypeQuality: window},
			ip4PathUpdates:     map[Ip4Path]*multiClientChannelUpdate{},
			ip6PathUpdates:     map[Ip6Path]*multiClientChannelUpdate{},
			affinityIp4Paths:   map[Ip4Path]map[Ip4Path]time.Time{},
			affinityIp6Paths:   map[Ip6Path]map[Ip6Path]time.Time{},
			clientUpdates:      map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
			reliabilityMetrics: newReliabilityMetrics(),
		}
	}
	benchedWindow := func(profile *PerformanceProfile) *multiClientWindow {
		window := familyTestWindow(ctx, generator, settings)
		window.performanceProfile = profile
		// the warned exit being replaced
		old := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
		old.warning = true
		window.clients[old.ClientId()] = old
		// its replacement, quarantined after a stall
		replacement := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
		replacement.quarantined = true
		window.clients[replacement.ClientId()] = replacement
		return window
	}

	window := benchedWindow(fixedIpTestProfile(WindowTypeQuality))
	multiClient := newMultiClient(window)
	if got := len(window.OrderedClients()); got != 0 {
		t.Fatalf("ordinary offer = %d exits, want 0 (both benched)", got)
	}
	var first *multiClientChannel
	for i := 0; i < 64; i += 1 {
		candidates := multiClient.raceCandidates(window, 4)
		if len(candidates) != 1 {
			t.Fatalf("sticky last-resort race = %d exits, want 1", len(candidates))
		}
		if first == nil {
			first = candidates[0]
		} else if candidates[0] != first {
			t.Fatalf("sticky last-resort exit changed between calls")
		}
	}

	// a window that is not Fixed IP still races the whole benched field
	window = benchedWindow(unfixedTestProfile(WindowTypeQuality))
	multiClient = newMultiClient(window)
	if got := len(multiClient.raceCandidates(window, 4)); got != 2 {
		t.Fatalf("unfixed last-resort race = %d exits, want 2", got)
	}
}
