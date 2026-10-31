// Ownership controls cover stale minted handoffs and reject equivalent-count
// moves into a constructed channel's lifetime.
package connect

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// Reads the current expansion body without accepting a missing mutation site.
func expandOwnershipControlBody(t *testing.T) string {
	t.Helper()
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) expand(")
	if !ok {
		t.Fatal("could not find expand")
	}
	return body
}

// Returns the exact preconstruction stale handoff and its same-slot retry.
func expandOwnershipStaleCleanup() string {
	return "\t\t\tif !self.acceptDiscoveryDemandArgs(evaluationOwner, args) {\n" +
		"\t\t\t\tself.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)\n" +
		"\t\t\t\tgoto awaitCandidate\n\t\t\t}\n"
}

// The fourth direct retirement belongs to args that never reached construction.
// The previous three-site inventory deterministically rejects this healthy body.
func TestExpandArgsCleanupAnchorAcceptsStaleDemandBeforeOwnership(t *testing.T) {
	body := expandOwnershipControlBody(t)
	if strings.Count(body, expandOwnershipStaleCleanup()) != 1 || strings.Count(body, "RemoveClientArgs(") != 4 {
		t.Fatal("preownership cleanup premises changed")
	}
	if violations := expandArgsCleanupViolations(t, body); len(violations) != 0 {
		t.Fatalf("SDK_EXPAND_PREOWNERSHIP_DEMAND_CLEANUP_UNRECOGNIZED: %v", violations)
	}
}

// Moving only the stale retirement into decline preserves the call count but
// wrongly revokes credentials still owned by a constructed channel.
func TestExpandArgsCleanupAnchorRejectsMovedStaleDemandRetirement(t *testing.T) {
	body := expandOwnershipControlBody(t)
	stale := expandOwnershipStaleCleanup()
	decline := "\n\t\t\tclient.Cancel()\n\t\t\tself.monitor.AddProviderEventWithExtenderIps("
	if strings.Count(body, stale) != 1 || strings.Count(body, decline) != 1 {
		t.Fatal("ownership mutation sites changed")
	}
	withoutCleanup := strings.Replace(stale, "\t\t\t\tself.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)\n", "", 1)
	mutated := strings.Replace(body, stale, withoutCleanup, 1)
	mutated = strings.Replace(mutated, decline, "\n\t\t\tself.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)"+decline, 1)
	if strings.Count(mutated, "RemoveClientArgs(") != strings.Count(body, "RemoveClientArgs(") {
		t.Fatal("counterexample changed the direct cleanup count")
	}
	if violations := expandArgsCleanupViolations(t, mutated); !strings.Contains(strings.Join(violations, "\n"), "outside a pre-ownership cleanup") {
		t.Fatalf("same-count stale retirement moved into decline was not rejected: %v", violations)
	}
}

// Keeping the condition does not authorize retirement after construction.
func TestExpandArgsCleanupAnchorRejectsStaleDemandAfterConstruction(t *testing.T) {
	body := expandOwnershipControlBody(t)
	stale := expandOwnershipStaleCleanup()
	constructed := "\n\t\t\tif err != nil {\n\t\t\t\tproviderFailure := self.recordChannelCreationFailure("
	if strings.Count(body, stale) != 1 || strings.Count(body, constructed) != 1 {
		t.Fatal("ownership mutation sites changed")
	}
	mutated := strings.Replace(body, stale, "", 1)
	mutated = strings.Replace(mutated, constructed, "\n"+stale+constructed, 1)
	if strings.Count(mutated, "RemoveClientArgs(") != strings.Count(body, "RemoveClientArgs(") {
		t.Fatal("counterexample changed the direct cleanup count")
	}
	if violations := expandArgsCleanupViolations(t, mutated); !strings.Contains(strings.Join(violations, "\n"), "outside a pre-ownership cleanup") {
		t.Fatalf("same-guard retirement after construction was not rejected: %v", violations)
	}
}

// A retired stale handoff must not fall through into the channel constructor.
func TestExpandArgsCleanupAnchorRejectsStaleDemandFallthrough(t *testing.T) {
	body := expandOwnershipControlBody(t)
	stale := expandOwnershipStaleCleanup()
	if strings.Count(body, stale) != 1 {
		t.Fatal("ownership mutation site changed")
	}
	mutated := strings.Replace(body, stale, strings.Replace(stale, "\t\t\t\tgoto awaitCandidate\n", "", 1), 1)
	if strings.Count(mutated, "RemoveClientArgs(") != strings.Count(body, "RemoveClientArgs(") {
		t.Fatal("counterexample changed the direct cleanup count")
	}
	if violations := expandArgsCleanupViolations(t, mutated); !strings.Contains(strings.Join(violations, "\n"), "outside a pre-ownership cleanup") {
		t.Fatalf("stale retirement falling through to construction was not rejected: %v", violations)
	}
}

// The actual receiver retires one stale minted identity without constructing a
// channel or consuming its live slot; its ordinary successor owns its cleanup.
func TestExpandStaleDemandArgsRetiredBeforeConstruction(t *testing.T) {
	fixture := newMultiClientExpandLifecycleFixture(t)
	ordinary := <-fixture.window.clientChannelArgs
	fixture.window.clientChannelArgs = make(chan *multiClientChannelArgs)
	generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
	staleClientArgs, err := generator.NewClientArgs()
	if err != nil {
		t.Fatal(err)
	}
	stale := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *staleClientArgs,
		Destination:                    ordinary.Destination,
		discoveryDemandOwner:           &multiClientEvaluationOwner{},
		discoveryDemandSlot:            1,
	}
	var stateLock sync.Mutex
	constructedClientIds := map[Id]int{}
	removedArgsClientIds := map[Id]int{}
	removedOwnedClientIds := map[Id]int{}
	staleRetired := make(chan struct{})
	var retiredOnce sync.Once
	constructorEntered, releaseConstructor := make(chan struct{}), make(chan struct{})
	var constructorOnce, releaseConstructorOnce sync.Once
	finishConstructor := func() { releaseConstructorOnce.Do(func() { close(releaseConstructor) }) }
	t.Cleanup(finishConstructor)
	originalNewClient := generator.newClient
	generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
		stateLock.Lock()
		constructedClientIds[args.ClientId]++
		stateLock.Unlock()
		if args.ClientId == ordinary.ClientId {
			constructorOnce.Do(func() { close(constructorEntered) })
			select {
			case <-releaseConstructor:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return originalNewClient(ctx, args, settings)
	}
	originalRemoveArgs := generator.removeClientArgs
	generator.removeClientArgs = func(args *MultiClientGeneratorClientArgs) {
		stateLock.Lock()
		removedArgsClientIds[args.ClientId]++
		stateLock.Unlock()
		originalRemoveArgs(args)
		if args.ClientId == stale.ClientId {
			retiredOnce.Do(func() { close(staleRetired) })
		}
	}
	originalRemoveOwned := generator.removeClientWithArgs
	generator.removeClientWithArgs = func(client *Client, args *MultiClientGeneratorClientArgs) {
		stateLock.Lock()
		removedOwnedClientIds[args.ClientId]++
		stateLock.Unlock()
		originalRemoveOwned(client, args)
	}
	expandDone := fixture.start()
	select {
	case fixture.window.clientChannelArgs <- stale:
	case <-fixture.waitCtx.Done():
		t.Fatalf("deliver stale args: %v", fixture.waitCtx.Err())
	}
	fixture.wait(t, "stale minted args retirement", staleRetired)
	fixture.window.stateLock.Lock()
	current := fixture.window.discoveryDemand
	generatorNotify := fixture.window.generatorMonitor.NotifyChannel()
	liveSlot := current.owner != nil && current.owner != stale.discoveryDemandOwner && current.slot == 1 &&
		current.slotOpen && current.slotCtx != nil && current.slotCtx.Err() == nil && len(fixture.window.clients) == 0
	fixture.window.stateLock.Unlock()
	stateLock.Lock()
	beforeConstruction := len(constructedClientIds) == 0 && removedArgsClientIds[stale.ClientId] == 1 && len(removedOwnedClientIds) == 0
	stateLock.Unlock()
	if !liveSlot || !beforeConstruction {
		t.Fatal("stale receipt constructed a channel, lost its current slot, or did not retire exactly once")
	}
	select {
	case <-generatorNotify:
		t.Fatal("stale rejection created another ordinary generator opportunity")
	default:
	}
	select {
	case fixture.window.clientChannelArgs <- ordinary:
	case <-fixture.waitCtx.Done():
		t.Fatalf("deliver ordinary successor: %v", fixture.waitCtx.Err())
	}
	// Acquisition may end before the pending ping answers. Observe the
	// consumed slot while its synchronous constructor still owns this request.
	fixture.wait(t, "ordinary successor constructor entry", constructorEntered)
	fixture.window.stateLock.Lock()
	acceptedSameSlot := fixture.window.discoveryDemand.owner == current.owner && fixture.window.discoveryDemand.slot == current.slot &&
		!fixture.window.discoveryDemand.slotOpen && current.slotCtx.Err() == context.Canceled
	fixture.window.stateLock.Unlock()
	if !acceptedSameSlot {
		t.Fatal("ordinary successor did not consume the original live request slot")
	}
	finishConstructor()
	fixture.wait(t, "ordinary successor initial ping", fixture.pingResultEntered)
	fixture.releasePing()
	if added := fixture.result(t, expandDone); added != 1 || fixture.clientCount() != 1 {
		t.Fatalf("ordinary successor admissions=%d, installed=%d, want one", added, fixture.clientCount())
	}
	fixture.cancelWindow()
	fixture.wait(t, "channel-owned successor cleanup", fixture.clientRemoved)
	stateLock.Lock()
	defer stateLock.Unlock()
	if constructedClientIds[stale.ClientId] != 0 || constructedClientIds[ordinary.ClientId] != 1 || len(constructedClientIds) != 1 ||
		removedArgsClientIds[stale.ClientId] != 1 || len(removedArgsClientIds) != 1 ||
		removedOwnedClientIds[ordinary.ClientId] != 1 || len(removedOwnedClientIds) != 1 {
		t.Fatalf("unbalanced ownership: constructed=%v unused=%v channel_owned=%v", constructedClientIds, removedArgsClientIds, removedOwnedClientIds)
	}
}
