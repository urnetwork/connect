// Composed IPv6 discovery, admission and strict-window recovery regressions.
package connect

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// Two independent real provider transports with destination-aware identity
// minting. The candidate is published only by the explicit discovery round.
type ipv6AdmissionGenerator struct {
	MultiClientGenerator
	candidateGenerator      MultiClientGenerator
	candidate               *multiClientChannelArgs
	stateLock               sync.Mutex
	candidateClientIds      map[Id]bool
	extraClientIdGenerators map[Id]MultiClientGenerator
	minted                  chan struct{}
	mintedOnce              sync.Once
}

// Keep the fixture on production selector-mode and destination-aware paths.
var _ MultiClientGeneratorWithCapabilities = (*ipv6AdmissionGenerator)(nil)
var _ MultiClientGeneratorWithDestination = (*ipv6AdmissionGenerator)(nil)

// Keeps both providers' real ping/transport paths and cleanup owners.
func newIpv6AdmissionGenerator(first *multiClientExpandLifecycleFixture, firstArgs *multiClientChannelArgs, second *multiClientExpandLifecycleFixture, secondArgs *multiClientChannelArgs) *ipv6AdmissionGenerator {
	return &ipv6AdmissionGenerator{
		MultiClientGenerator: first.window.generator,
		candidateGenerator:   second.window.generator,
		candidate:            secondArgs,
		candidateClientIds:   map[Id]bool{secondArgs.ClientId: true},
		minted:               make(chan struct{}),
	}
}

// The platform supports the selectors used by the production enumerator.
func (self *ipv6AdmissionGenerator) SelectorMode() (bool, bool) { return true, true }

// Every compatible request receives the one available dualstack provider.
func (self *ipv6AdmissionGenerator) NextDestinationsWithCapabilities(count int, excluded []MultiHopId, rankMode string, family IpFamilyFilter, selection FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	destinations := map[MultiHopId]DestinationStats{}
	if !slices.Contains(excluded, self.candidate.Destination) && count > 0 {
		destinations[self.candidate.Destination] = self.candidate.DestinationStats
	}
	return destinations, true, nil
}

// The same immediate synthetic answer under the demand owner's context.
func (self *ipv6AdmissionGenerator) NextDestinationsWithCapabilitiesContext(ctx context.Context, count int, excluded []MultiHopId, rankMode string, family IpFamilyFilter, selection FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return self.NextDestinationsWithCapabilities(count, excluded, rankMode, family, selection)
}

// Named redial cannot substitute a different provider for a requested one.
func (self *ipv6AdmissionGenerator) NextDestinationsForClientIdWithCapabilities(clientId Id, excluded []MultiHopId, rankMode string, selection FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	if self.candidate.Destination.Tail() != clientId {
		return map[MultiHopId]DestinationStats{}, true, nil
	}
	return self.NextDestinationsWithCapabilities(1, excluded, rankMode, IpFamilyFilterV4Capable, selection)
}

// Allocates each identity from its provider's fixture before recording the
// routing choice. The signal follows the enumerator's candidate-ready publish.
func (self *ipv6AdmissionGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	if destination != self.candidate.Destination {
		return self.MultiClientGenerator.NewClientArgs()
	}
	args, err := self.candidateGenerator.NewClientArgs()
	if err == nil {
		self.stateLock.Lock()
		self.candidateClientIds[args.ClientId] = true
		self.stateLock.Unlock()
		self.mintedOnce.Do(func() { close(self.minted) })
	}
	return args, err
}

// Resolves fixture ownership without holding a lock across construction/close.
func (self *ipv6AdmissionGenerator) owner(clientId Id) MultiClientGenerator {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.candidateClientIds[clientId] {
		return self.candidateGenerator
	}
	if generator := self.extraClientIdGenerators[clientId]; generator != nil {
		return generator
	}
	return self.MultiClientGenerator
}

// Builds the real client against the selected provider transport.
func (self *ipv6AdmissionGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
	return self.owner(args.ClientId).NewClient(ctx, args, settings)
}

// Releases the exact transport and identity that constructed the client.
func (self *ipv6AdmissionGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.owner(args.ClientId).RemoveClientWithArgs(client, args)
}

// Returns an identity that never became a client to its original owner.
func (self *ipv6AdmissionGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.owner(args.ClientId).RemoveClientArgs(args)
}

// A full fixed-speed window and a mobile-style fixed-cap quality window use
// their actual resize need, probe before removing, then replace within the cap.
func TestResizeRecoversRequestedIpv6AtStrictCapacity(t *testing.T) {
	for _, fixedProfile := range []bool{false, true} {
		fixture := newMultiClientExpandLifecycleFixture(t)
		dualstackFixture := newMultiClientExpandLifecycleFixture(t)
		window := fixture.window
		first := <-window.clientChannelArgs
		second := <-dualstackFixture.window.clientChannelArgs
		first.IpFamily, second.IpFamily = IpFamilyV4Only, IpFamilyDualstack
		generator := newIpv6AdmissionGenerator(fixture, first, dualstackFixture, second)
		window.generator = generator
		window.settings.StandingReserve = false
		window.settings.StrictWindowSizeHardMax = true
		count := 5
		if fixedProfile {
			count = 1
		}
		size := WindowSizeSettings{WindowSizeMin: count, WindowSizeMax: count, WindowSizeHardMax: count}
		window.settings.WindowSizes[WindowTypeQuality] = size
		if fixedProfile {
			size.FixedWindowSize = 1
			window.windowType = WindowTypeSpeed
			window.performanceProfile = &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: size}
		}
		window.backendDegradedForTest = func() bool { return false }
		window.clientRemoveCallback = func(*multiClientChannel) {}
		window.clientChannelArgs = make(chan *multiClientChannelArgs, count)
		window.clientChannelArgs <- first
		generator.extraClientIdGenerators = map[Id]MultiClientGenerator{}
		for range count - 1 {
			sibling := newMultiClientExpandLifecycleFixture(t)
			args := <-sibling.window.clientChannelArgs
			args.IpFamily = IpFamilyV4Only
			generator.extraClientIdGenerators[args.ClientId] = sibling.window.generator
			window.clientChannelArgs <- args
		}
		fixture.releasePing()
		for seeded := 0; seeded < count; {
			added := window.expand(size, seeded, 0, count, count, count-seeded, 0, windowExpandPassPlan{})
			if added <= 0 {
				t.Fatal("seed expansion made no progress")
			}
			seeded += added
		}
		previous := window.unorderedClients()
		// An unbuffered offer keeps the probe candidate ready until expansion
		// owns it. The two pass barriers exclude resize-timer/scheduler luck.
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		passDone, nextPass := make(chan struct{}, 1), make(chan struct{})
		window.afterResizePassForTest = func() {
			passDone <- struct{}{}
			select {
			case <-nextPass:
			case <-window.ctx.Done():
			}
		}
		resizeDone := make(chan struct{})
		go func() { defer close(resizeDone); window.resize() }()
		fixture.wait(t, "first full-window resize", passDone)
		if selection := window.providerSelect(); selection.DualstackTargetCount != 1 || window.ipv6FillWanted(time.Now()) != 1 {
			fixture.cancelWindow()
			fixture.wait(t, "failed resize join", resizeDone)
			t.Fatalf("fixed profile %t: requested need was lost before recovery: %+v", fixedProfile, selection)
		}
		enumerateDone := make(chan struct{})
		go func() { defer close(enumerateDone); window.randomEnumerateClientArgs() }()
		fixture.wait(t, "IPv6 probe minted after discovery", generator.minted)
		if len(window.unorderedClients()) != count {
			t.Fatal("the full window gave up its provider before the probe found supply")
		}
		for _, old := range previous {
			if old.IsDone() {
				t.Fatal("a provider retired before candidate acquisition")
			}
		}
		fixture.send(t, "next resize pass", nextPass)
		fixture.wait(t, "family replacement pass", passDone)
		clients := window.unorderedClients()
		retired := 0
		for _, old := range previous {
			if old.IsDone() {
				retired++
			}
		}
		if len(clients) != count || !window.ipv6Available() || retired != 1 {
			t.Errorf("fixed profile %t: strict-cap recovery left %d clients, IPv6 %t, retired %d", fixedProfile, len(clients), window.ipv6Available(), retired)
		}
		fixture.cancelWindow()
		fixture.wait(t, "resize join", resizeDone)
		fixture.wait(t, "enumeration join", enumerateDone)
	}
}

// A bounded acquisition end releases a reserved last slot when no IPv6
// candidate arrives. A known empty discovery answer releases it immediately.
func TestExpandIpv6ReservationFallsBackWhenSupplyEnds(t *testing.T) {
	for _, starved := range []bool{false, true} {
		fixture := newMultiClientExpandLifecycleFixture(t)
		window := fixture.window
		window.settings.EvaluationPoolMultiple = 2
		first := <-window.clientChannelArgs
		first.IpFamily = IpFamilyV4Only
		window.clientChannelArgs <- first
		if starved {
			window.noteIpv6Starved(time.Now())
		}
		finish := make(chan struct{})
		window.finishExpandRequestsForTest = finish
		done := make(chan int, 1)
		go func() {
			done <- window.expand(WindowSizeSettings{FixedWindowSize: 1, WindowSizeHardMax: 1}, 0, 0, 1, 1, 1, 1, windowExpandPassPlan{})
		}()
		fixture.wait(t, "IPv4 ping", fixture.pingResultEntered)
		fixture.releasePing()
		fixture.wait(t, "IPv4 ping processed", fixture.pingResultDone)
		if (len(window.unorderedClients()) == 1) != starved {
			t.Errorf("known empty supply %t: admission did not match the reservation", starved)
		}
		close(finish)
		if admitted := fixture.result(t, done); admitted != 1 || len(window.unorderedClients()) != 1 {
			t.Fatalf("known empty supply %t: bounded completion admitted %d", starved, admitted)
		}
	}
}

// The initial resize supplies the requested minimum itself, even when a
// mobile/fixed profile leaves the explicit IPv6 setting at zero.
func TestResizeFormationReservesRequestedIpv6(t *testing.T) {
	for _, fixedProfile := range []bool{false, true} {
		fixture := newMultiClientExpandLifecycleFixture(t)
		dualstackFixture := newMultiClientExpandLifecycleFixture(t)
		window := fixture.window
		first := <-window.clientChannelArgs
		second := <-dualstackFixture.window.clientChannelArgs
		first.IpFamily, second.IpFamily = IpFamilyV4Only, IpFamilyDualstack
		window.generator = newIpv6AdmissionGenerator(fixture, first, dualstackFixture, second)
		window.settings.StandingReserve = false
		window.settings.EvaluationPoolMultiple = 2
		size := WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1}
		window.settings.WindowSizes[WindowTypeQuality] = size
		if fixedProfile {
			size.FixedWindowSize = 1
			window.windowType = WindowTypeSpeed
			window.performanceProfile = &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: size}
		}
		window.backendDegradedForTest = func() bool { return false }
		window.clientChannelArgs <- first
		window.afterResizePassForTest = fixture.cancelWindow
		done := make(chan struct{})
		go func() { defer close(done); window.resize() }()
		fixture.wait(t, "formation IPv4 ping", fixture.pingResultEntered)
		fixture.releasePing()
		fixture.wait(t, "formation IPv4 result", fixture.pingResultDone)
		window.clientChannelArgs <- second
		fixture.wait(t, "formation pass", done)
		clients := window.unorderedClients()
		if len(clients) != 1 || clients[0].IpFamily() != IpFamilyDualstack {
			t.Errorf("fixed profile %t: formation lost the requested family; clients %d", fixedProfile, len(clients))
		}
	}
}
