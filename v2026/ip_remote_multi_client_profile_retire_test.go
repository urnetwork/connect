package connect

import (
	"context"
	"testing"
)

// On 2026-11-13 an iOS tunnel's performance profile changed twice, and each
// change retired every window client: 17 working exits and 400 flows reset at
// once. A window type or size change, Fixed IP and back to auto included, and
// direct mode turned on, now leave the installed clients in place; the
// windows take the new profile and resize to it.
func TestProfileChangeDoesNotRetireWorkingExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	windows := map[WindowType]*multiClientWindow{
		WindowTypeQuality: capabilityTestWindow(ctx, generator, settings, WindowTypeQuality),
		WindowTypeSpeed:   capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed),
		WindowTypeLimited: capabilityTestWindow(ctx, generator, settings, WindowTypeLimited),
	}
	parent := capabilityTestParent(ctx, cancel, generator, settings, windows)
	capabilityTestWireLimitedWindow(parent)

	clients := map[WindowType]*multiClientChannel{}
	for windowType, window := range windows {
		capabilities := capabilityClassA
		if windowType == WindowTypeLimited {
			capabilities = capabilityClassD
		}
		client := capabilityTestChannel(t, ctx, settings, windowType, capabilities)
		client.ctx, client.cancel = context.WithCancel(ctx)
		window.stateLock.Lock()
		window.clients[client.ClientId()] = client
		window.stateLock.Unlock()
		clients[windowType] = client
	}

	steps := []struct {
		name    string
		profile *PerformanceProfile
	}{
		// unchanged, and changed in representation only: a no-op before this
		// fix too (the equal-profile guard)
		{name: "unchanged auto", profile: nil},
		{name: "explicit auto with a window size", profile: &PerformanceProfile{WindowType: WindowTypeAuto, WindowSize: WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4}}},
		// changes the installed clients carry: each retired every client
		// before this fix
		{name: "auto to quality", profile: unfixedTestProfile(WindowTypeQuality)},
		{name: "quality to fixed ip", profile: fixedIpTestProfile(WindowTypeQuality)},
		{name: "fixed ip to speed", profile: unfixedTestProfile(WindowTypeSpeed)},
		{name: "direct on", profile: &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: unfixedTestProfile(WindowTypeSpeed).WindowSize, AllowDirect: true}},
		{name: "back to auto with direct on", profile: &PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: true}},
	}
	for _, step := range steps {
		if err := parent.SetPerformanceProfile(step.profile); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if !performanceProfilesEqual(parent.config.Load().performanceProfile, step.profile) {
			t.Fatalf("%s: the profile was not installed", step.name)
		}
		for windowType, client := range clients {
			if client.IsDone() {
				t.Fatalf("%s: the %s window's working client was retired", step.name, windowStatsWindowOf(windowType))
			}
		}
		for windowType, window := range windows {
			window.stateLock.Lock()
			profile := window.performanceProfile
			window.stateLock.Unlock()
			if !performanceProfilesEqual(profile, step.profile) {
				t.Errorf("%s: the %s window kept profile %+v", step.name, windowStatsWindowOf(windowType), profile)
			}
		}
	}
}

// The changes the installed clients cannot carry: a post-quantum toggle either
// way, and direct mode turned off. Nothing else retires them.
func TestPerformanceProfileRetiresClients(t *testing.T) {
	pqe := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	direct := &PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: true}
	for _, c := range []struct {
		name     string
		previous *PerformanceProfile
		next     *PerformanceProfile
		retires  bool
	}{
		{name: "post-quantum on", previous: nil, next: pqe, retires: true},
		{name: "post-quantum off", previous: pqe, next: nil, retires: true},
		{name: "direct off", previous: direct, next: nil, retires: true},
		{name: "direct on", previous: nil, next: direct, retires: false},
		{name: "window type", previous: nil, next: unfixedTestProfile(WindowTypeQuality), retires: false},
		{name: "fixed ip", previous: unfixedTestProfile(WindowTypeQuality), next: fixedIpTestProfile(WindowTypeQuality), retires: false},
		{name: "fixed ip off", previous: fixedIpTestProfile(WindowTypeSpeed), next: nil, retires: false},
		{name: "post-quantum kept with a window change", previous: pqe, next: &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: unfixedTestProfile(WindowTypeSpeed).WindowSize, PostQuantumEncryption: true}, retires: false},
	} {
		if got := performanceProfileRetiresClients(c.previous, c.next); got != c.retires {
			t.Errorf("%s: retires=%t, want %t", c.name, got, c.retires)
		}
	}
}
