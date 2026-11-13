package connect

// A provider the generator excludes must not come back through a restored
// window identity (PROXYDRAIN1.md §3.5), on any path a window lists or dials
// destinations by, with post-quantum on and off. The exclusions are the user's
// removal (RemoteUserNatMultiClient.RemoveProvider), a reliability verdict and
// the generator's constructor exclusions. Found 2026-11-13 while landing the
// post-quantum restored-destination fix (connect a31ae0953): restored
// destinations were checked against the caller's exclusions only, never the
// generator's. Each case runs as one test with post-quantum off and one with
// it on. Every identity, token and endpoint here is synthetic.

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The platform of these tests. A named request, a confirmation, is answered
// with every named provider modern, so only an exclusion keeps a destination
// out. A discovery request is answered with `discovered`, less the providers
// it excludes, as the server's exclusions do. While echoSelect is set, a
// request that carries `select` is answered in selector mode: the selectors
// echoed and each provider labelled with the ones it asked for.
func testingExclusionPlatform(echoSelect *atomic.Bool, discovered ...MultiHopId) func(args *FindProviders2Args) (*FindProviders2Result, error) {
	return func(args *FindProviders2Args) (*FindProviders2Result, error) {
		if testingModernConfirmRequest(args) {
			return testingNamedModernAnswer(args, slices.Collect(maps.Keys(testingNamedClientIds(args)))...), nil
		}
		selectorMode := args.Select != nil && echoSelect != nil && echoSelect.Load()
		result := &FindProviders2Result{Providers: []*FindProvidersProvider{}}
		for _, destination := range discovered {
			if slices.Contains(args.ExcludeClientIds, destination.Tail()) {
				continue
			}
			provider := &FindProvidersProvider{ClientId: destination.Tail()}
			if selectorMode {
				provider.Capabilities = &ProviderCapabilities{Ip: args.Select.Ip, Protocol: args.Select.Protocol}
			}
			result.Providers = append(result.Providers, provider)
		}
		if selectorMode {
			providerSelect := *args.Select
			result.Select = &providerSelect
		}
		return result, nil
	}
}

// Every generator call a window lists destinations by, each at rank mode
// quality: plain discovery and its cancellation-bounded form, a family top-up
// (v6-capable) and a main family fill, selector mode for a normal window and
// for the limited window, and a discovery demand read.
func testingEveryDiscoveryCall(ctx context.Context, generator *ApiMultiClientGenerator, excludeDestinations []MultiHopId) []func() (map[MultiHopId]DestinationStats, error) {
	normalSelect := FindProvidersSelect{
		Ip:                   ProviderIpCapabilityDualstack,
		Protocol:             ProviderProtocolCapabilityDualproto,
		TargetCount:          6,
		DualstackTargetCount: 1,
	}
	limitedSelect := FindProvidersSelect{
		Ip:          ProviderIpCapabilityIpv4,
		Protocol:    ProviderProtocolCapabilityTcp,
		TargetCount: 1,
	}
	return []func() (map[MultiHopId]DestinationStats, error){
		func() (map[MultiHopId]DestinationStats, error) {
			return generator.NextDestinations(3, excludeDestinations, "quality")
		},
		func() (map[MultiHopId]DestinationStats, error) {
			return generator.NextDestinationsContext(ctx, 3, excludeDestinations, "quality")
		},
		func() (map[MultiHopId]DestinationStats, error) {
			return generator.NextDestinationsWithIpFamily(1, excludeDestinations, "quality", IpFamilyFilterV6Capable)
		},
		func() (map[MultiHopId]DestinationStats, error) {
			return generator.NextDestinationsWithIpFamilyContext(ctx, 3, excludeDestinations, "quality", IpFamilyFilterV4Capable)
		},
		func() (map[MultiHopId]DestinationStats, error) {
			destinations, _, err := generator.NextDestinationsWithCapabilities(3, excludeDestinations, "quality", IpFamilyFilterV4Capable, normalSelect)
			return destinations, err
		},
		func() (map[MultiHopId]DestinationStats, error) {
			destinations, _, err := generator.NextDestinationsWithCapabilities(3, excludeDestinations, "quality", IpFamilyFilterV4Capable, limitedSelect)
			return destinations, err
		},
		func() (map[MultiHopId]DestinationStats, error) {
			destinations, _, err := generator.NextDestinationsWithCapabilitiesContext(ctx, 1, excludeDestinations, "quality", IpFamilyFilterV4Capable, normalSelect)
			return destinations, err
		},
	}
}

// The destinations with a restored identity pending reuse.
func testingRestoredDestinationSet(generator *ApiMultiClientGenerator) map[MultiHopId]bool {
	restoredDestinations := map[MultiHopId]bool{}
	for _, destination := range generator.identityState.RestoredDestinations() {
		restoredDestinations[destination] = true
	}
	return restoredDestinations
}

// A synthetic restored identity for a destination.
func testingRestoredIdentity(destination MultiHopId, byJwt string) *WindowClientIdentity {
	return &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       byJwt,
		InstanceId:  NewId(),
		Destination: destination,
	}
}

// The root cause. Before the restart the window ran two clients to one
// provider. It dials the provider again with the first restored identity,
// and the user removes the provider (RemoveProvider), which drops it from the
// windows and excludes it from discovery. Every listing path still handed it
// straight back through the second identity, pending reuse, because a
// restored destination was checked against the caller's exclusions only, and
// the window dialed the removed provider again. Now no path returns it, no
// confirmation names it, and the pending identity is forgotten, so a later
// dial of the provider mints a fresh identity. The kept provider's identity
// stays pending, and a destination the window holds is skipped without being
// forgotten, as before.
func TestRemovedProviderNeverReturnsThroughPendingRestoredIdentity(t *testing.T) {
	testRemovedProviderNeverReturnsThroughPendingRestoredIdentity(t, false)
}

func TestRemovedProviderNeverReturnsThroughPendingRestoredIdentityUnderPostQuantum(t *testing.T) {
	testRemovedProviderNeverReturnsThroughPendingRestoredIdentity(t, true)
}

func testRemovedProviderNeverReturnsThroughPendingRestoredIdentity(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	removed := RequireMultiHopId(NewId())
	kept := RequireMultiHopId(NewId())
	discovered := RequireMultiHopId(NewId())
	removedDialed := testingRestoredIdentity(removed, "synthetic-removed-dialed")
	removedPending := testingRestoredIdentity(removed, "synthetic-removed-pending")
	keptPending := testingRestoredIdentity(kept, "synthetic-kept-pending")
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		[]*WindowClientIdentity{removedDialed, removedPending, keptPending},
		testingExclusionPlatform(nil, discovered),
	)
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	// a window that holds the provider skips it and keeps it pending
	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, []MultiHopId{removed}, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{kept: true, discovered: true})
	AssertEqual(t, testingRestoredDestinationSet(fixture.generator), map[MultiHopId]bool{removed: true, kept: true})

	// the window dials it with the first restored identity
	dialedArgs, err := fixture.generator.NewClientArgsForDestinationContext(ctx, removed)
	AssertEqual(t, err, nil)
	AssertEqual(t, dialedArgs.ClientId, removedDialed.ClientId)

	multi := &RemoteUserNatMultiClient{ctx: ctx, log: NewNoopLogger(), generator: fixture.generator}
	multi.RemoveProvider(removed.Tail())
	fixture.takeRequests()

	for _, call := range testingEveryDiscoveryCall(ctx, fixture.generator, nil) {
		destinations, err := call()
		AssertEqual(t, err, nil)
		AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{kept: true, discovered: true})
	}
	confirms, others := fixture.takeRequests()
	AssertEqual(t, len(others), 7)
	for _, request := range others {
		// discovery still carries the exclusion to the platform
		AssertEqual(t, slices.Contains(request.ExcludeClientIds, removed.Tail()), true)
	}
	if postQuantum {
		// asked about the kept provider alone, never the removed one
		AssertEqual(t, len(confirms), 7)
		for _, confirm := range confirms {
			assertModernConfirmRequest(t, confirm, "quality", kept.Tail())
		}
	} else {
		AssertEqual(t, len(confirms), 0)
	}

	// forgotten: no later expansion meets it, and its next dial mints
	AssertEqual(t, testingRestoredDestinationSet(fixture.generator), map[MultiHopId]bool{kept: true})
	redialArgs, err := fixture.generator.NewClientArgsForDestinationContext(ctx, removed)
	AssertEqual(t, err, nil)
	AssertNotEqual(t, redialArgs.ClientId, removedPending.ClientId)
	AssertEqual(t, fixture.mintCount.Load(), int32(1))
	fixture.generator.RemoveClientArgs(dialedArgs)
	fixture.generator.RemoveClientArgs(redialArgs)
}

// The same for a reliability verdict, the platform's statement that the
// provider is no longer a usable route: the window excludes the provider
// before it wakes its replacement pass, and the replacement must not be the
// same provider through its pending restored identity.
func TestReliabilityVerdictNeverReturnsThroughPendingRestoredIdentity(t *testing.T) {
	testReliabilityVerdictNeverReturnsThroughPendingRestoredIdentity(t, false)
}

func TestReliabilityVerdictNeverReturnsThroughPendingRestoredIdentityUnderPostQuantum(t *testing.T) {
	testReliabilityVerdictNeverReturnsThroughPendingRestoredIdentity(t, true)
}

func testReliabilityVerdictNeverReturnsThroughPendingRestoredIdentity(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	failed := RequireMultiHopId(NewId())
	discovered := RequireMultiHopId(NewId())
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		[]*WindowClientIdentity{testingRestoredIdentity(failed, "synthetic-failed-pending")},
		testingExclusionPlatform(nil, discovered),
	)
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	window, client := newContractStatusWindowFixture(failed.Tail())
	window.generator = fixture.generator
	reliability := protocol.ContractError_Reliability
	window.contractStatusFromClient(client, &ContractStatus{
		Key:   ContractKey{Destination: DestinationId(failed.Tail())},
		Error: &reliability,
	})

	for _, call := range testingEveryDiscoveryCall(ctx, fixture.generator, nil) {
		destinations, err := call()
		AssertEqual(t, err, nil)
		AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{discovered: true})
	}
	AssertEqual(t, len(testingRestoredDestinationSet(fixture.generator)), 0)
}

// The constructor's exclusions, such as the device's own client id that the
// sdk excludes, hold for restored destinations too.
func TestConstructorExclusionsHoldForRestoredIdentities(t *testing.T) {
	testConstructorExclusionsHoldForRestoredIdentities(t, false)
}

func TestConstructorExclusionsHoldForRestoredIdentitiesUnderPostQuantum(t *testing.T) {
	testConstructorExclusionsHoldForRestoredIdentities(t, true)
}

func testConstructorExclusionsHoldForRestoredIdentities(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	selfId := NewId()
	kept := RequireMultiHopId(NewId())
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		[]*WindowClientIdentity{
			testingRestoredIdentity(RequireMultiHopId(selfId), "synthetic-self-pending"),
			testingRestoredIdentity(kept, "synthetic-kept-pending"),
		},
		testingExclusionPlatform(nil),
		selfId,
	)
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{kept: true})
	confirms, _ := fixture.takeRequests()
	if postQuantum {
		AssertEqual(t, len(confirms), 1)
		assertModernConfirmRequest(t, confirms[0], "quality", kept.Tail())
	} else {
		AssertEqual(t, len(confirms), 0)
	}
	AssertEqual(t, testingRestoredDestinationSet(fixture.generator), map[MultiHopId]bool{kept: true})
}

// A restored multi-hop destination is left out, and forgotten, when any of
// its hops is excluded, its exit or an intermediary: an excluded provider
// carries none of the generator's routes. A route with no excluded hop is
// returned as before.
func TestRestoredMultiHopDestinationWithAnExcludedHopIsNeverReturned(t *testing.T) {
	testRestoredMultiHopDestinationWithAnExcludedHopIsNeverReturned(t, false)
}

func TestRestoredMultiHopDestinationWithAnExcludedHopIsNeverReturnedUnderPostQuantum(t *testing.T) {
	testRestoredMultiHopDestinationWithAnExcludedHopIsNeverReturned(t, true)
}

func testRestoredMultiHopDestinationWithAnExcludedHopIsNeverReturned(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	excludedIntermediaryId := NewId()
	excludedExitId := NewId()
	throughExcludedIntermediary := RequireMultiHopId(excludedIntermediaryId, NewId())
	toExcludedExit := RequireMultiHopId(NewId(), excludedExitId)
	kept := RequireMultiHopId(NewId(), NewId())
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		[]*WindowClientIdentity{
			testingRestoredIdentity(throughExcludedIntermediary, "synthetic-intermediary-pending"),
			testingRestoredIdentity(toExcludedExit, "synthetic-exit-pending"),
			testingRestoredIdentity(kept, "synthetic-kept-pending"),
		},
		testingExclusionPlatform(nil),
	)
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	multi := &RemoteUserNatMultiClient{ctx: ctx, log: NewNoopLogger(), generator: fixture.generator}
	multi.RemoveProvider(excludedIntermediaryId)
	multi.RemoveProvider(excludedExitId)

	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{kept: true})
	confirms, _ := fixture.takeRequests()
	if postQuantum {
		AssertEqual(t, len(confirms), 1)
		assertModernConfirmRequest(t, confirms[0], "quality", kept.Ids()...)
	} else {
		AssertEqual(t, len(confirms), 0)
	}
	AssertEqual(t, testingRestoredDestinationSet(fixture.generator), map[MultiHopId]bool{kept: true})
}

// An exclusion that lands while the platform requests are out, as a user's
// removal can, holds for that answer too: a removed fixed spec, restored
// destination or discovered provider is not handed back by the call that was
// already asking, and the restored identity is forgotten. When discovery then
// fails, no fixed destination is left to prefer, and the call fails as
// discovery did.
func TestProviderRemovedWhileDiscoveryIsOutIsNotReturned(t *testing.T) {
	testProviderRemovedWhileDiscoveryIsOutIsNotReturned(t, false)
}

func TestProviderRemovedWhileDiscoveryIsOutIsNotReturnedUnderPostQuantum(t *testing.T) {
	testProviderRemovedWhileDiscoveryIsOutIsNotReturned(t, true)
}

func testProviderRemovedWhileDiscoveryIsOutIsNotReturned(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	removedFixedId := NewId()
	keptFixedId := NewId()
	removedRestored := RequireMultiHopId(NewId())
	removedDiscovered := RequireMultiHopId(NewId())
	keptDiscovered := RequireMultiHopId(NewId())
	platform := testingExclusionPlatform(nil, removedDiscovered, keptDiscovered)
	var multi *RemoteUserNatMultiClient
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{ClientId: &removedFixedId}, {ClientId: &keptFixedId}, {BestAvailable: true}},
		[]*WindowClientIdentity{testingRestoredIdentity(removedRestored, "synthetic-removed-pending")},
		func(args *FindProviders2Args) (*FindProviders2Result, error) {
			if testingModernConfirmRequest(args) {
				return platform(args)
			}
			// the platform answers as the request found it, and the user
			// removes three providers before the answer is read
			result, err := platform(args)
			multi.RemoveProvider(removedFixedId)
			multi.RemoveProvider(removedRestored.Tail())
			multi.RemoveProvider(removedDiscovered.Tail())
			return result, err
		},
	)
	multi = &RemoteUserNatMultiClient{ctx: ctx, log: NewNoopLogger(), generator: fixture.generator}
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{
		RequireMultiHopId(keptFixedId): true,
		keptDiscovered:                 true,
	})
	AssertEqual(t, len(testingRestoredDestinationSet(fixture.generator)), 0)

	failedFixedId := NewId()
	var failingMulti *RemoteUserNatMultiClient
	failing := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{ClientId: &failedFixedId}, {BestAvailable: true}},
		nil,
		func(args *FindProviders2Args) (*FindProviders2Result, error) {
			if testingModernConfirmRequest(args) {
				return platform(args)
			}
			failingMulti.RemoveProvider(failedFixedId)
			return nil, errors.New("synthetic discovery failure")
		},
	)
	failingMulti = &RemoteUserNatMultiClient{ctx: ctx, log: NewNoopLogger(), generator: failing.generator}
	failing.generator.SetPostQuantumEncryption(postQuantum)
	destinations, err = failing.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertNotEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)
}

// The named redial of a lost exit, a sticky window's or a recent exit's,
// reads the exclusions again once its answer is back: a provider removed
// while the request was out is answered empty, as one removed before it is,
// which makes no request at all.
func TestNamedRedialOfProviderRemovedWhileTheRequestIsOutIsEmpty(t *testing.T) {
	testNamedRedialOfProviderRemovedWhileTheRequestIsOutIsEmpty(t, false)
}

func TestNamedRedialOfProviderRemovedWhileTheRequestIsOutIsEmptyUnderPostQuantum(t *testing.T) {
	testNamedRedialOfProviderRemovedWhileTheRequestIsOutIsEmpty(t, true)
}

func testNamedRedialOfProviderRemovedWhileTheRequestIsOutIsEmpty(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var multi *RemoteUserNatMultiClient
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		nil,
		func(args *FindProviders2Args) (*FindProviders2Result, error) {
			// the platform answers the named provider, and the user removes
			// it before the answer is read
			namedClientIds := slices.Collect(maps.Keys(testingNamedClientIds(args)))
			for _, clientId := range namedClientIds {
				multi.RemoveProvider(clientId)
			}
			return testingNamedModernAnswer(args, namedClientIds...), nil
		},
	)
	multi = &RemoteUserNatMultiClient{ctx: ctx, log: NewNoopLogger(), generator: fixture.generator}
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	lostId := NewId()
	destinations, err := fixture.generator.NextDestinationsForClientId(lostId, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)

	otherLostId := NewId()
	destinations, _, err = fixture.generator.NextDestinationsForClientIdWithCapabilities(otherLostId, nil, "quality", FindProvidersSelect{
		Ip:          ProviderIpCapabilityDualstack,
		Protocol:    ProviderProtocolCapabilityDualproto,
		TargetCount: 1,
	})
	AssertEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)

	fixture.takeRequests()
	destinations, err = fixture.generator.NextDestinationsForClientId(lostId, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)
	confirms, others := fixture.takeRequests()
	AssertEqual(t, len(confirms)+len(others), 0)
}

// Every window lists through the generator, so none lists the removed
// provider: the normal windows in selector mode, a family top-up and the
// resize pass's ipv6 probe from a server without selector mode, and the
// limited window. The limited window admits only providers labelled tcp, and
// a restored destination carries no label, so it never listed one; the
// normal windows listed the removed provider before the fix.
func TestWindowsNeverListRemovedProviderThroughPendingRestoredIdentity(t *testing.T) {
	testWindowsNeverListRemovedProviderThroughPendingRestoredIdentity(t, false)
}

func TestWindowsNeverListRemovedProviderThroughPendingRestoredIdentityUnderPostQuantum(t *testing.T) {
	testWindowsNeverListRemovedProviderThroughPendingRestoredIdentity(t, true)
}

func testWindowsNeverListRemovedProviderThroughPendingRestoredIdentity(t *testing.T, postQuantum bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	removed := RequireMultiHopId(NewId())
	discovered := RequireMultiHopId(NewId())
	var echoSelect atomic.Bool
	echoSelect.Store(true)
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		[]*WindowClientIdentity{testingRestoredIdentity(removed, "synthetic-removed-pending")},
		testingExclusionPlatform(&echoSelect, discovered),
	)
	fixture.generator.SetPostQuantumEncryption(postQuantum)

	settings := DefaultMultiClientSettings()
	windows := map[WindowType]*multiClientWindow{}
	for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed, WindowTypeLimited} {
		window := capabilityTestWindow(ctx, fixture.generator, settings, windowType)
		window.setRequestSizing(2, 0)
		windows[windowType] = window
	}
	multi := &RemoteUserNatMultiClient{ctx: ctx, log: NewNoopLogger(), generator: fixture.generator, windows: windows}
	multi.RemoveProvider(removed.Tail())

	listed := func(window *multiClientWindow, probeOnly bool) map[MultiHopId]bool {
		t.Helper()
		enumerated, err := window.enumerateDestinations(nil, probeOnly)
		AssertEqual(t, err, nil)
		destinations := map[MultiHopId]bool{}
		for _, destination := range enumerated {
			destinations[destination.destination] = true
		}
		return destinations
	}
	for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed, WindowTypeLimited} {
		AssertEqual(t, listed(windows[windowType], false), map[MultiHopId]bool{discovered: true})
	}

	// a server without selector mode: the family top-up and the probe ask
	// v6-capable
	echoSelect.Store(false)
	quality := windows[WindowTypeQuality]
	quality.setRequestSizing(2, 1)
	quality.setIpv6Shortfall(1)
	fixture.takeRequests()
	AssertEqual(t, listed(quality, false), map[MultiHopId]bool{discovered: true})
	AssertEqual(t, listed(quality, true), map[MultiHopId]bool{discovered: true})
	_, others := fixture.takeRequests()
	familyTopUps := 0
	for _, request := range others {
		if request.IpFamily == IpFamilyFilterV6Capable {
			familyTopUps += 1
		}
	}
	AssertEqual(t, familyTopUps, 2)
	AssertEqual(t, len(testingRestoredDestinationSet(fixture.generator)), 0)
}

// A candidate listed before the user removed its provider waits for the
// expansion with its identity already minted, up to WindowExpandArgsTimeout.
// The expansion refuses it, as it refuses a candidate listed before its
// provider cooled off, so the removed provider is not dialed from a listing
// that predates the removal. A candidate of a provider still allowed is
// accepted as before.
func TestExpansionRefusesCandidateListedBeforeItsProviderWasRemoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	removed := RequireMultiHopId(NewId())
	kept := RequireMultiHopId(NewId())
	fixture := newModernConfirmFixture(
		t,
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		nil,
		testingExclusionPlatform(nil, removed, kept),
	)
	window := capabilityTestWindow(ctx, fixture.generator, DefaultMultiClientSettings(), WindowTypeQuality)
	multi := &RemoteUserNatMultiClient{
		ctx:       ctx,
		log:       NewNoopLogger(),
		generator: fixture.generator,
		windows:   map[WindowType]*multiClientWindow{WindowTypeQuality: window},
	}

	enumerated, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	candidates := map[MultiHopId]*multiClientChannelArgs{}
	for _, destination := range enumerated {
		args, err := fixture.generator.NewClientArgsForDestinationContext(ctx, destination.destination)
		AssertEqual(t, err, nil)
		candidates[destination.destination] = &multiClientChannelArgs{
			Destination:                    destination.destination,
			DestinationStats:               destination.stats,
			MultiClientGeneratorClientArgs: *args,
		}
	}
	AssertEqual(t, len(candidates), 2)

	multi.RemoveProvider(removed.Tail())
	AssertEqual(t, window.acceptDiscoveryDemandArgs(nil, candidates[removed]), false)
	AssertEqual(t, window.acceptDiscoveryDemandArgs(nil, candidates[kept]), true)
	for _, candidate := range candidates {
		fixture.generator.RemoveClientArgs(&candidate.MultiClientGeneratorClientArgs)
	}
}

// A restored identity was seen by its provider before the restart. Handed
// back unused, as a refused or expired candidate's identity is, or by a
// client whose transport never registered, it must retire, never park for the
// next candidate of another destination: that would carry an identity one
// provider saw to another, which the discovery park exists never to do
// (ip_remote_multi_client_identity_reuse.go). An identity no provider saw
// still parks and is reused.
func TestRestoredIdentityNeverParksForAnotherDestination(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), nil)
		argsRestored := testingRestoredIdentity(RequireMultiHopId(NewId()), "synthetic-args-restored")
		clientRestored := testingRestoredIdentity(RequireMultiHopId(NewId()), "synthetic-client-restored")
		fixture.generator.SetIdentityStore(&fakeIdentityStore{
			persisted: []*WindowClientIdentity{argsRestored, clientRestored},
		})

		// a candidate's args handed back before any client
		args, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), argsRestored.Destination)
		AssertEqual(t, err, nil)
		AssertEqual(t, args.ClientId, argsRestored.ClientId)
		fixture.generator.RemoveClientArgs(args)
		synctest.Wait()

		// a client whose platform transport never registered
		args, err = fixture.generator.NewClientArgsForDestinationContext(t.Context(), clientRestored.Destination)
		AssertEqual(t, err, nil)
		AssertEqual(t, args.ClientId, clientRestored.ClientId)
		client := NewClient(t.Context(), args.ClientId, NewNoContractClientOob(), closeWaitClientSettings())
		fixture.generator.transportLock.Lock()
		if len(fixture.generator.transports) == 0 {
			fixture.generator.transportIdle = make(chan struct{})
		}
		fixture.generator.transports[client] = &apiWindowClientTransport{
			createTime: time.Now(),
			current:    &registrationWindowPlatformTransport{everConnected: false},
		}
		fixture.generator.transportLock.Unlock()
		fixture.generator.RemoveClientWithArgs(client, args)
		client.Cancel()
		synctest.Wait()

		AssertEqual(t, fixture.retireCount.Load(), int32(2))
		AssertEqual(t, fixture.mintCount.Load(), int32(0))

		// the next candidate of another destination gets a fresh identity
		args, err = fixture.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
		AssertEqual(t, err, nil)
		AssertNotEqual(t, args.ClientId, argsRestored.ClientId)
		AssertNotEqual(t, args.ClientId, clientRestored.ClientId)
		AssertEqual(t, fixture.mintCount.Load(), int32(1))

		// which no provider saw: handed back, it parks for the next candidate
		freshClientId := args.ClientId
		fixture.generator.RemoveClientArgs(args)
		synctest.Wait()
		args, err = fixture.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
		AssertEqual(t, err, nil)
		AssertEqual(t, args.ClientId, freshClientId)
		AssertEqual(t, fixture.mintCount.Load(), int32(1))
		AssertEqual(t, fixture.retireCount.Load(), int32(2))
		fixture.generator.RemoveClientArgs(args)
		synctest.Wait()
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A fixed generator dials one destination per spec, so a restored identity
// handed back unused still parks for its own destination's next dial, as
// before: the provider that saw it is the one it returns to.
func TestFixedGeneratorRestoredIdentityParksForItsOwnDestination(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, nil)
		restored := testingRestoredIdentity(fixture.destination, "synthetic-fixed-restored")
		fixture.generator.SetIdentityStore(&fakeIdentityStore{
			persisted: []*WindowClientIdentity{restored},
		})

		args, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), fixture.destination)
		AssertEqual(t, err, nil)
		AssertEqual(t, args.ClientId, restored.ClientId)
		fixture.generator.RemoveClientArgs(args)
		synctest.Wait()

		args, err = fixture.generator.NewClientArgsForDestinationContext(t.Context(), fixture.destination)
		AssertEqual(t, err, nil)
		AssertEqual(t, args.ClientId, restored.ClientId)
		AssertEqual(t, fixture.mintCount.Load(), int32(0))
		AssertEqual(t, fixture.retireCount.Load(), int32(0))
		fixture.generator.RemoveClientArgs(args)
		synctest.Wait()
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
