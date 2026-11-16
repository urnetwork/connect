package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
)

// The real expand callback replaces a flowless same-identity predecessor.
// Force a new-generation failure between slot installation and old-generation
// cleanup, then close only the window while its cache-owning parent stays live.
func TestDestinationServiceFailureRemovalPreservesLiveReplacementGeneration(t *testing.T) {
	fixture := newMultiClientExpandLifecycleFixture(t)
	parent, _, forwarded, _ := rebindTestParent(t, false, nil)
	args := <-fixture.window.clientChannelArgs
	fixture.window.clientChannelArgs <- args
	oldCtx, cancelOld := context.WithCancel(fixture.window.ctx)
	defer cancelOld()
	old := &multiClientChannel{
		ctx:      oldCtx,
		cancel:   cancelOld,
		log:      DefaultLogger(),
		settings: parent.settings,
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: args.ClientId},
			Destination:                    args.Destination,
		},
	}
	path := serviceFailureRetentionTestPath(4)
	oldUpdate := rebindTestFlow(parent, old, path, false)
	oldUpdate.noteConnectAttempt(path)
	if !parent.clientDialFailureSignal(old, path) {
		t.Fatal("setup predecessor's live failure did not unbind")
	}
	oldKey, ok := destinationServiceFailureKeyFor(path, old)
	if _, recorded := parent.destinationServiceFailures[oldKey]; !ok || !recorded {
		t.Fatal("setup predecessor has no service memory")
	}
	fixture.window.clients[args.ClientId] = old
	fixture.window.flowCountFunc = parent.clientFlowCount
	fixture.window.dialFailureCallback = parent.clientDialFailureSignal
	var broadRemovals atomic.Int32
	fixture.window.clientRemoveCallback = func(*multiClientChannel) { broadRemovals.Add(1) }

	var replacement *multiClientChannel
	var replacementKey destinationServiceFailureKey
	var replacementBefore destinationServiceFailure
	var sibling *multiClientChannelUpdate
	var siblingPath *IpPath
	var hookErr string
	fixture.window.clientRetireCallback = func(retiring *multiClientChannel) {
		if retiring == old {
			fixture.window.stateLock.Lock()
			replacement = fixture.window.clients[args.ClientId]
			fixture.window.stateLock.Unlock()
			if replacement == nil || replacement == old || replacement.ClientId() != old.ClientId() || replacement.IsDone() {
				hookErr = "real admission did not install a live same-identity replacement before retirement"
				return
			}
			replacementPath := *path
			replacementPath.SourcePort++
			update := rebindTestFlow(parent, replacement, &replacementPath, false)
			update.noteConnectAttempt(&replacementPath)
			if !parent.clientDialFailureSignal(replacement, &replacementPath) {
				hookErr = "replacement's live failure did not unbind"
				return
			}
			var valid, recorded bool
			replacementKey, valid = destinationServiceFailureKeyFor(&replacementPath, replacement)
			replacementBefore, recorded = parent.destinationServiceFailures[replacementKey]
			if !valid || !recorded || oldKey == replacementKey {
				hookErr = "same-identity generations did not keep independent live service facts"
				return
			}
			siblingPath = serviceFailureRetentionTestPath(4)
			siblingPath.SourcePort += 2
			siblingPath.DestinationIp = net.ParseIP("203.0.113.91")
			siblingPath.Syn = false
			siblingPath.Ack = true
			sibling = rebindTestFlow(parent, replacement, siblingPath, true)
		}
		parent.clearDestinationServiceFailures(retiring)
	}

	expandDone := fixture.start()
	joined := false
	defer func() {
		fixture.releasePing()
		if !joined {
			fixture.result(t, expandDone)
		}
	}()
	fixture.wait(t, "held same-identity replacement ping", fixture.pingResultEntered)
	fixture.releasePing()
	added := fixture.result(t, expandDone)
	joined = true
	if added != 1 {
		t.Fatalf("real same-identity replacement admitted %d candidates, want 1", added)
	}
	if _, retained := parent.destinationServiceFailures[oldKey]; retained {
		t.Fatal("SERVICE_FAILURE_REPLACED_GENERATION_RETAINED: actual same-identity admission retained the predecessor key")
	}
	if hookErr != "" || replacement == nil || sibling == nil {
		t.Fatalf("replacement admission hook=%q replacement=%p sibling=%p", hookErr, replacement, sibling)
	}
	if after, found := parent.destinationServiceFailures[replacementKey]; !found || after != replacementBefore {
		t.Fatal("SERVICE_FAILURE_LIVE_REPLACEMENT_REMOVED: predecessor retirement changed the live same-identity generation's memory")
	}
	if !old.IsDone() || replacement.IsDone() || fixture.window.clients[args.ClientId] != replacement ||
		parent.ctx.Err() != nil || sibling.client.Load() != replacement || !sibling.receivedInbound.Load() ||
		!parent.clientUpdates[replacement][sibling] || len(*forwarded) != 0 || broadRemovals.Load() != 0 {
		t.Fatal("predecessor retirement changed replacement ownership or invoked broad removal")
	}
	if event := fixture.window.monitor.ProviderEvents()[args.ClientId]; event == nil || event.State != ProviderStateAdded {
		t.Fatal("same-identity replacement lost the new generation's Added monitor dot")
	}

	fixture.window.Close()
	if _, retained := parent.destinationServiceFailures[replacementKey]; retained {
		t.Fatal("SERVICE_FAILURE_WINDOW_CLOSE_RETAINED_GENERATION: closing only the window retained its channel key")
	}
	if !replacement.IsDone() || parent.ctx.Err() != nil || broadRemovals.Load() != 0 ||
		sibling.client.Load() != replacement || len(*forwarded) != 0 {
		t.Fatal("cache-only window close changed its surviving parent or invoked flow migration")
	}
	fixture.assertNoDirectArgsRemoval(t)
}
