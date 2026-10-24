// Readiness describes selectable windows without discarding retained ownership.
package connect

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A zero configured minimum is not a provider. Warned fixed peers and strict
// saturation with a real healthy client retain their established semantics.
func TestWindowMinimumRequiresOwnedSelectableClient(t *testing.T) {
	cases := []struct {
		name                 string
		min, healthy, warned int
		fixed, strict        bool
		max                  int
		want                 bool
	}{
		{name: "empty public zero", min: 0},
		{name: "warned public zero", min: 0, warned: 1},
		{name: "empty fixed zero", min: 0, fixed: true},
		{name: "healthy public zero", min: 0, healthy: 1, want: true},
		{name: "warned fixed zero", min: 0, warned: 1, fixed: true, want: true},
		{name: "warned selected peer", min: 1, warned: 1, fixed: true, want: true},
		{name: "healthy public minimum", min: 3, healthy: 3, want: true},
		{name: "strict retained warning", min: 3, healthy: 2, warned: 1, strict: true, max: 3, want: true},
		{name: "strict only warnings", min: 0, warned: 3, strict: true, max: 3},
		{name: "strict replacement available", min: 3, healthy: 2, warned: 1, strict: true, max: 4},
	}
	for _, c := range cases {
		if got := windowMinSatisfied(c.min, c.healthy, c.warned, c.fixed, c.strict, c.max); got != c.want {
			t.Errorf("%s: readiness=%t, want %t", c.name, got, c.want)
		}
	}
}

// The non-family enumeration path calls NextDestinations, not its contextual
// variant. Hold that actual entry until the explicitly owned fixture cancels.
type readinessHeldGenerator struct {
	maintenanceContextTestGenerator
	ctx      context.Context
	fixed    bool
	entered  atomic.Int32
	returned atomic.Int32
}

// The fixture supplies its owner context before any constructor worker starts.
// No destination or client identity is allocated while discovery is held.
func (self *readinessHeldGenerator) NextDestinations(count int, exclude []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.entered.Add(1)
	defer self.returned.Add(1)
	return self.NextDestinationsContext(self.ctx, count, exclude, rankMode)
}

// A chosen peer still has only the quality window, regardless of profile type.
func (self *readinessHeldGenerator) FixedDestinationSize() (int, bool) {
	return 1, self.fixed
}

// Exercise the actual constructor's legacy discovery dispatch for public and
// fixed-peer windows. Quiescence proves calls are held; explicit owner cancel
// must return every call and close each real enumeration channel before exit.
func TestReadinessFixtureLegacyDiscoveryJoinsOwner(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			settings := DefaultMultiClientSettings()
			settings.Log = NewNoopLogger()
			settings.StandingReserve = false
			ctx, cancel := context.WithCancel(context.Background())
			generator := &readinessHeldGenerator{ctx: ctx, fixed: fixed}
			multi := NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Public, settings)
			defer func() {
				cancel()
				join, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer joinCancel()
				if err := multi.CloseAndWait(join); err != nil {
					t.Error(err)
				}
				synctest.Wait()
				if generator.returned.Load() != generator.entered.Load() {
					t.Errorf("fixed=%t: discovery did not join: entered=%d returned=%d", fixed, generator.entered.Load(), generator.returned.Load())
				}
				for kind, window := range multi.windows {
					select {
					case args, open := <-window.clientChannelArgs:
						if open || args != nil {
							t.Errorf("fixed=%t %s: enumeration produced an identity or remained open", fixed, kind.RankMode())
						}
					default:
						t.Errorf("fixed=%t %s: enumeration worker did not join", fixed, kind.RankMode())
					}
				}
			}()
			synctest.Wait()
			if generator.entered.Load() != int32(len(multi.windows)) || generator.returned.Load() != 0 {
				t.Fatalf("fixed=%t: actual legacy discovery not held: entered=%d returned=%d windows=%d", fixed, generator.entered.Load(), generator.returned.Load(), len(multi.windows))
			}
			if _, providers := multi.Monitor().Events(); len(providers) != 0 {
				t.Fatalf("fixed=%t: held discovery published %d providers", fixed, len(providers))
			}
		})
	}
}

// Profiles use exact three-slot windows so only the selected class can satisfy
// the requested minimum. The SDK converts min=max to this same fixed size.
func readinessProfile(kind WindowType) *PerformanceProfile {
	return &PerformanceProfile{
		WindowType: kind,
		WindowSize: WindowSizeSettings{
			WindowSizeMin:     3,
			WindowSizeMax:     3,
			WindowSizeHardMax: 3,
			FixedWindowSize:   3,
		},
	}
}

// The real constructor and merged callback must not turn an empty inactive
// class's 0 <= 0 into readiness or let it veto the active class's failure.
func TestSelectedWindowEmptyDoesNotSatisfyMergedReadiness(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()
	cases := []struct {
		kind    WindowType
		profile *PerformanceProfile
		target  int
	}{
		{kind: WindowTypeQuality, profile: readinessProfile(WindowTypeQuality), target: 3},
		{kind: WindowTypeSpeed, profile: readinessProfile(WindowTypeSpeed), target: 3},
		{kind: WindowTypeQuality, target: 0},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			settings := DefaultMultiClientSettings()
			settings.Log = NewNoopLogger()
			settings.DefaultPerformanceProfile = c.profile
			settings.StandingReserve = false
			if c.profile == nil {
				for _, kind := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
					settings.WindowSizes[kind] = WindowSizeSettings{WindowSizeMax: 3, WindowSizeHardMax: 3}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			multi := NewRemoteUserNatMultiClient(ctx, &readinessHeldGenerator{ctx: ctx}, nil, protocol.ProvideMode_Public, settings)
			defer func() {
				cancel()
				join, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer joinCancel()
				if err := multi.CloseAndWait(join); err != nil {
					t.Error(err)
				}
				synctest.Wait()
			}()
			var callback *WindowExpandEvent
			unsub := multi.Monitor().AddMonitorEventCallback(func(event *WindowExpandEvent, _ map[Id]*ProviderEvent, _ bool) {
				callback = event
			})
			defer unsub()
			synctest.Wait()
			event, providers := multi.Monitor().Events()
			if event.TargetSize != c.target || event.MinSatisfied || len(providers) != 0 {
				t.Errorf("EMPTY_SELECTED_WINDOW: %s target=%d event=%+v providers=%d", c.kind.RankMode(), c.target, event, len(providers))
			}
			active := multi.windows[c.kind].monitor
			active.SetStallStatus(WindowStallProvidersUnresponsive, true)
			synctest.Wait()
			event = multi.Monitor().WindowExpandEvent()
			if !event.Failed || event.MinSatisfied || callback == nil || !callback.Failed || callback.MinSatisfied {
				t.Errorf("ACTIVE_FAILURE_MASKED: %s snapshot=%+v callback=%+v", c.kind.RankMode(), event, callback)
			}
		})
	}
}

// Constructs established synthetic channels before any resize worker starts.
// The flow-count seam keeps each channel owned during capacity collapse.
func newReadinessRetainedWindow(t *testing.T, kind WindowType, fixed bool, warned bool) (*multiClientWindow, []*multiClientChannel) {
	t.Helper()
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()
	settings.StandingReserve = false
	settings.WindowResizeTimeout = time.Hour
	settings.MaxClientLifetime = 2 * time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	window := &multiClientWindow{
		ctx:                    ctx,
		cancel:                 cancel,
		log:                    settings.Log,
		generator:              &readinessHeldGenerator{ctx: ctx, fixed: fixed},
		settings:               settings,
		windowType:             kind,
		monitor:                NewRemoteUserNatMultiClientMonitorWithDefaults(),
		resizeMonitor:          NewMonitor(),
		generatorMonitor:       NewMonitor(),
		clientChannelArgs:      make(chan *multiClientChannelArgs),
		clients:                map[Id]*multiClientChannel{},
		failures:               &windowFailureRecorder{},
		flowCountFunc:          func(*multiClientChannel) int { return 1 },
		backendDegradedForTest: func() bool { return false },
		clientRemoveCallback:   func(*multiClientChannel) { t.Error("retained flow was removed") },
	}
	window.evalEpochCtx, window.evalEpochCancel = context.WithCancel(ctx)
	clients := []*multiClientChannel{}
	count := 4
	if fixed {
		count = 1
	}
	for i := 0; i < count; i++ {
		client := stallTestChannel()
		client.ctx, client.cancel = context.WithCancel(ctx)
		client.clientReceiveUnsub = func() {}
		client.log = settings.Log
		client.args = &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
			DestinationStats:               DestinationStats{IpFamily: IpFamilyDualstack},
			FixedDestination:               fixed,
		}
		client.firstEventTime = time.Now().Add(-time.Hour)
		client.effectiveLifetime = 2 * time.Hour
		if warned {
			client.effectiveLifetime = time.Minute
		}
		window.clients[client.ClientId()] = client
		window.monitor.AddProviderEventWithExtenderIps(client.ClientId(), ProviderStateAdded, client.Destination().Tail(), nil, IpFamilyDualstack, []netip.Addr{netip.MustParseAddr("192.0.2.1")})
		clients = append(clients, client)
	}
	// The setter's activity publication is asynchronous by contract. Wait
	// for the actual resize worker to become blocked, never a short sleep.
	other := WindowTypeQuality
	if kind == other {
		other = WindowTypeSpeed
	}
	window.SetPerformanceProfile(readinessProfile(other))
	return window, clients
}

// Runs the real resize loop and joins it before closing the fixture channels.
func readinessRetainedWindow(t *testing.T, kind WindowType, fixed bool, warned bool) (*multiClientWindow, []*multiClientChannel, func()) {
	t.Helper()
	window, clients := newReadinessRetainedWindow(t, kind, fixed, warned)
	done := make(chan struct{})
	go func() {
		defer close(done)
		window.resize()
	}()
	stop := func() {
		window.cancel()
		<-done
		window.Close()
		synctest.Wait()
	}
	synctest.Wait()
	return window, clients, stop
}

// A disabled class may still own flows and old failures. Neither its retained
// count nor a late outcome update may masquerade as an active readiness vote.
func TestInactiveWindowRetainsFlowsWithoutMaskingActiveFailure(t *testing.T) {
	for _, kind := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		for _, warned := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				window, clients, stop := readinessRetainedWindow(t, kind, false, warned)
				defer stop()
				active := NewRemoteUserNatMultiClientMonitorWithDefaults()
				active.AddWindowExpandEvent(false, 3, false)
				active.SetStallStatus(WindowStallProvidersUnresponsive, true)
				merged := NewMergedMultiClientMonitor([]MultiClientMonitor{active, window.monitor})
				for _, failed := range []bool{false, true} {
					window.monitor.SetStallStatus(WindowStallAuthFailing, failed)
					event := merged.WindowExpandEvent()
					own := window.monitor.WindowExpandEvent()
					if event.TargetSize != 3 || event.MinSatisfied || !event.Failed || event.Reason != WindowStallProvidersUnresponsive ||
						own.TargetSize != 0 || own.MinSatisfied || own.Failed || own.Reason != WindowStallEvaluating {
						t.Errorf("INACTIVE_RETAINED_WINDOW: %s warned=%t inactiveFailed=%t merged=%+v own=%+v", kind.RankMode(), warned, failed, event, own)
					}
				}
				own, providers := window.monitor.Events()
				if len(providers) != len(clients) || len(window.unorderedClients()) != len(clients) || own.Ipv6Available == warned {
					t.Errorf("retained ownership/family changed: providers=%d clients=%d ipv6=%t warned=%t", len(providers), len(window.unorderedClients()), own.Ipv6Available, warned)
				}
				for _, client := range clients {
					provider := providers[client.ClientId()]
					if client.IsDone() || window.flowCount(client) != 1 || provider == nil || provider.State != ProviderStateAdded ||
						provider.EgressClientId != client.Destination().Tail() || provider.IpFamily != IpFamilyDualstack ||
						len(provider.ExtenderIps) != 1 || provider.ExtenderIps[0] != netip.MustParseAddr("192.0.2.1") {
						t.Error("inactive profile lost an established flow or provider identity")
					}
				}
				// Reactivation is the same production setter/resize path. Healthy
				// retained exits again satisfy readiness; all-warned ones cannot.
				var notified *WindowExpandEvent
				unsub := window.monitor.AddMonitorEventCallback(func(event *WindowExpandEvent, _ map[Id]*ProviderEvent, _ bool) { notified = event })
				defer unsub()
				window.SetPerformanceProfile(readinessProfile(kind))
				synctest.Wait()
				own = window.monitor.WindowExpandEvent()
				if own.MinSatisfied == warned || own.TargetSize == 0 || notified == nil || notified.MinSatisfied != own.MinSatisfied || notified.TargetSize != own.TargetSize {
					t.Errorf("reactivated %s warned=%t: snapshot=%+v callback=%+v", kind.RankMode(), warned, own, notified)
				}
			})
		}
	}
}

// A fixed peer is always routed through quality, including an explicit speed
// profile. The private activity bit must not hide that actual selectable path.
func TestFixedPeerQualityWindowRemainsActiveUnderSpeedProfile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window, clients, stop := readinessRetainedWindow(t, WindowTypeQuality, true, true)
		defer stop()
		event := window.monitor.WindowExpandEvent()
		if !event.MinSatisfied || event.TargetSize == 0 || len(window.monitor.ProviderEvents()) != 1 || clients[0].IsDone() {
			t.Fatalf("fixed warned peer was hidden by profile selection: %+v", event)
		}
	})
}

// Queued/coalesced listener snapshots use the same projection as synchronous
// reads, including a provider overflow reset and later identity/IP updates.
func TestInactiveWindowCallbacksKeepProviderSnapshotAndFamily(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window, clients, stop := readinessRetainedWindow(t, WindowTypeSpeed, false, false)
		defer stop()
		window.monitor.settings.CallbackPendingProviderEventMaxCount = 1
		entered := make(chan struct{})
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		var delivered []*WindowExpandEvent
		var lastProviders map[Id]*ProviderEvent
		var lastReset bool
		unsub := window.monitor.AddMonitorEventCallback(func(event *WindowExpandEvent, providers map[Id]*ProviderEvent, reset bool) {
			if len(delivered) == 0 {
				close(entered)
				<-release
			}
			delivered = append(delivered, event)
			lastProviders, lastReset = providers, reset
		})
		defer unsub()
		window.monitor.SetStallStatus(WindowStallAuthFailing, true)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("first actual monitor callback did not start")
		}
		// More than the pending cap forces self.Events(), not just a raw copy.
		window.monitor.SetProviderExtenderIps(clients[0].ClientId(), []netip.Addr{netip.MustParseAddr("192.0.2.2")})
		window.monitor.SetProviderIpFamily(clients[1].ClientId(), IpFamilyV4Only)
		window.monitor.SetStallStatus(WindowStallPlatformUnreachable, true)
		synctest.Wait()
		close(release)
		released = true
		synctest.Wait()
		if len(delivered) != 2 || !lastReset || len(lastProviders) != len(clients) {
			t.Fatalf("bounded reset delivery changed: callbacks=%d reset=%t providers=%d", len(delivered), lastReset, len(lastProviders))
		}
		for _, event := range delivered {
			if event.TargetSize != 0 || event.MinSatisfied || event.Failed || event.Reason != WindowStallEvaluating || !event.Ipv6Available {
				t.Errorf("inactive callback exposed admission or lost retained family: %+v", event)
			}
		}
		if lastProviders[clients[0].ClientId()].ExtenderIps[0] != netip.MustParseAddr("192.0.2.2") ||
			lastProviders[clients[1].ClientId()].IpFamily != IpFamilyV4Only {
			t.Fatal("reset lost current retained provider/IP updates")
		}
	})
}

// Public callers have no activity parameter. Their zero-value/default events
// remain active, especially a terminal failure with a zero desired target.
func TestLegacyWindowPublisherKeepsZeroTargetFailure(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	monitor.SetStallStatus(WindowStallPlatformUnreachable, true)
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{monitor})
	if !merged.WindowExpandEvent().Failed {
		t.Fatal("legacy zero-target terminal failure was treated as inactive")
	}
	monitor.AddWindowExpandEvent(true, 1, true)
	if event := merged.WindowExpandEvent(); !event.MinSatisfied || event.Failed || !event.Ipv6Available {
		t.Fatalf("legacy positive publisher lost compatibility: %+v", event)
	}
}

// A cancelled owner plus an already-ready resize edge may enter the loop again.
// Force that body-entry state directly, without depending on select ordering.
func TestCanceledWindowResizeDoesNotStartMaintenancePass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window, clients := newReadinessRetainedWindow(t, WindowTypeQuality, false, false)
		defer window.Close()
		removals := 0
		retainedCallback := window.clientRemoveCallback
		window.clientRemoveCallback = func(client *multiClientChannel) {
			removals++
			retainedCallback(client)
		}
		window.cancel()
		<-window.ctx.Done()
		for _, client := range clients {
			if !client.IsDone() {
				t.Fatal("owner cancellation did not reach an established child")
			}
		}
		window.resize()
		if removals != 0 || len(window.unorderedClients()) != len(clients) {
			t.Fatalf("cancelled owner started maintenance: removals=%d retained=%d want=%d",
				removals, len(window.unorderedClients()), len(clients))
		}
		if providers := window.monitor.ProviderEvents(); len(providers) != len(clients) {
			t.Fatalf("cancelled maintenance published provider removals: providers=%d want=%d", len(providers), len(clients))
		}
		window.Close()
		if len(window.unorderedClients()) != 0 {
			t.Fatal("explicit window close left owned channels")
		}
	})
}

// The loop-head gate must not shelter a cancelled child of a live owner.
// Its actual removal callback ends the owner only after recording the reap.
func TestLiveWindowResizeReapsExplicitlyCanceledChannel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window, clients := newReadinessRetainedWindow(t, WindowTypeQuality, true, false)
		defer window.Close()
		defer window.cancel()
		client := clients[0]
		client.Cancel()
		removals := 0
		window.clientRemoveCallback = func(removed *multiClientChannel) {
			if window.ctx.Err() != nil || removed != client || !removed.IsDone() {
				t.Error("explicit child retirement lost its live owner or exact channel")
			}
			removals++
			window.cancel()
		}
		window.resize()
		if removals != 1 || len(window.unorderedClients()) != 0 {
			t.Fatalf("live owner did not reap cancelled child: removals=%d retained=%d",
				removals, len(window.unorderedClients()))
		}
		if len(window.monitor.ProviderEvents()) != 0 {
			t.Fatal("explicit child retirement left its provider published")
		}
	})
}
