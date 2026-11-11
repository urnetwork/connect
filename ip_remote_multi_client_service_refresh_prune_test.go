// Atomic pruning distinguishes an invalid current reservation from a stale
// observation of an empty window. The wire root also proves retained replay.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// This fixture isolates cleanup ownership with real charged control credit.
// It does not claim to establish remote-refusal or wire-admission authority;
// those premises use the actual native/provider fixture in the final root.
type serviceRefreshPruneLedgerFixture struct {
	window  *multiClientWindow
	victim  *multiClientChannel
	root    *TransferMemoryBudget
	budget  *TransferMemoryBudget
	queue   *transferQueue[*sendItem]
	updates []*multiClientChannelUpdate
}

func newServiceRefreshPruneLedgerFixture(t *testing.T) *serviceRefreshPruneLedgerFixture {
	t.Helper()
	f := &serviceRefreshPruneLedgerFixture{root: NewTransferMemoryBudget(64 * 1024)}
	f.budget = NewTransferMemoryBudgetWithParent(f.root.TotalByteCount(), f.root)
	f.queue = retainedControlTestSendQueue()
	f.queue.setBudget(f.budget, 0)
	f.victim = &multiClientChannel{ctx: context.Background(), args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()}}}
	f.window = &multiClientWindow{ctx: context.Background(), clients: map[Id]*multiClientChannel{f.victim.ClientId(): f.victim}, resizeMonitor: NewMonitor(), generatorMonitor: NewMonitor()}
	t.Cleanup(func() {
		f.window.finishServiceRefresh(nil)
		for _, update := range f.updates {
			update.Close()
			update.releaseSynRetryMemory()
		}
		f.queue.Clear()
		for _, ledger := range []*TransferMemoryBudget{f.budget, f.root} {
			reserved, released := ledger.Counts()
			if ledger.UsedByteCount() != 0 || reserved == 0 || reserved != released {
				t.Errorf("prune ledger did not return exact paid ownership: used=%d reserved=%d released=%d", ledger.UsedByteCount(), reserved, released)
			}
		}
	})
	return f
}

func (f *serviceRefreshPruneLedgerFixture) install(t *testing.T, port int) (*multiClientChannelUpdate, *tcpSynRetry, *transferRetainedControlCredit) {
	t.Helper()
	path := unboundFlowTcpPath(port)
	wire := synMetadataTestPacket(path, 101, 201, nil)
	charge := retainedMessageCapacity(ByteCount(len(wire))) + retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpSynRetry{})))
	source := &SendSequence{ctx: context.Background(), resendQueue: f.queue}
	credit, ok := source.reserveRetainedControlMemory(charge)
	if !ok {
		t.Fatal("prune ledger could not acquire real bounded control credit")
	}
	update := newMultiClientChannelUpdate(context.Background(), path)
	f.updates = append(f.updates, update)
	owner := &tcpSynRetry{update: update, packet: MessagePoolCopy(wire), credit: credit, sequence: path.SequenceNumber, serial: 1, retryLineage: 1, references: 2}
	update.synRetry, update.serviceRefreshRequested = owner, true
	update.serviceRefreshLineage.Store(owner.retryLineage)
	update.discoveryAttemptStamp.Store(1)
	if !f.victim.serviceRefreshReservation.CompareAndSwap(nil, owner) || !f.window.installServiceRefresh(owner, f.victim, 1, []*multiClientChannel{f.victim}) {
		t.Fatal("prune ledger failed to install its exact current reservation")
	}
	return update, owner, credit
}

func TestServiceRefreshPruneStaleNilPreservesCurrentPaidOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	f := newServiceRefreshPruneLedgerFixture(t)
	stale := f.window.serviceRefreshSnapshot()
	if stale.owner != nil {
		t.Fatal("stale-read control did not start at an empty window")
	}
	update, owner, credit := f.install(t, 53301)
	paid := f.budget.UsedByteCount()
	// Deterministically order the original race: empty snapshot, live install,
	// then the resize loop's invalid-only pruning decision.
	f.window.pruneInvalidServiceRefresh()
	if f.window.serviceRefreshSnapshot().owner != owner || f.victim.serviceRefreshReservation.Load() != owner || owner.references != 2 || !update.serviceRefreshRequested || credit.released.Load() || f.budget.UsedByteCount() != paid || f.root.UsedByteCount() != paid {
		t.Fatal("SERVICE_REFRESH_STALE_EMPTY_PRUNED_LIVE_OWNER")
	}
	// Terminal nil cleanup must retain its distinct force-clear meaning.
	f.window.finishServiceRefresh(nil)
	if f.window.serviceRefreshOwner != nil || f.victim.serviceRefreshReservation.Load() != nil || owner.references != 1 || update.serviceRefreshRequested || credit.released.Load() {
		t.Fatal("terminal force cleanup did not release only its window reference")
	}
}

func TestServiceRefreshPruneInvalidOwnerReleasesExactPaidCredit(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	f := newServiceRefreshPruneLedgerFixture(t)
	update, owner, credit := f.install(t, 53302)
	update.Close()
	if f.window.serviceRefreshSnapshot().owner != nil || owner.references != 1 || credit.released.Load() {
		t.Fatal("invalid-owner control lost its still-paid window-only reference")
	}
	f.window.pruneInvalidServiceRefresh()
	f.window.pruneInvalidServiceRefresh()
	if f.window.serviceRefreshOwner != nil || f.victim.serviceRefreshReservation.Load() != nil || owner.references != 0 || owner.packet != nil || owner.credit != nil || !credit.released.Load() || f.budget.UsedByteCount() != 0 || f.root.UsedByteCount() != 0 {
		t.Fatal("invalid pruning failed exact-once packet/control-credit return")
	}
}

func TestServiceRefreshPruneStalePassCannotClearReplacement(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	f := newServiceRefreshPruneLedgerFixture(t)
	_, first, firstCredit := f.install(t, 53303)
	f.window.finishServiceRefresh(first)
	update, replacement, secondCredit := f.install(t, 53304)
	paid := f.root.UsedByteCount()
	f.window.finishServiceRefresh(first)
	f.window.pruneInvalidServiceRefresh()
	if f.window.serviceRefreshSnapshot().owner != replacement || f.victim.serviceRefreshReservation.Load() != replacement || replacement.references != 2 || first.references != 1 || !update.serviceRefreshRequested || firstCredit.released.Load() || secondCredit.released.Load() || f.root.UsedByteCount() != paid {
		t.Fatal("stale exact-pass cleanup cleared its replacement owner")
	}
}

func TestServiceRefreshPrunePreservesActualListingAdmissionAndReplay(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, configure := newAggregateServiceFixture(t, 53305, false)
		stale := f.window.serviceRefreshSnapshot()
		if stale.owner != nil {
			t.Fatal("wire control did not capture the initial empty window")
		}
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		late := f.h.addExit(reraceExitSpec{name: "early-late", clientSettings: configure})
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			masterServiceRefreshFailureCensus(t, f)
			t.Fatal("actual live reservation never reached held available listing")
		}
		credit := f.owner.credit
		f.window.pruneInvalidServiceRefresh()
		f.update.stateLock.Lock()
		paid := f.update.synRetry == f.owner && f.owner.references == 2 && f.owner.credit == credit && !credit.released.Load() && f.update.serviceRefreshRequested
		f.update.stateLock.Unlock()
		if !paid || f.window.serviceRefreshSnapshot().owner != f.owner || f.victim.serviceRefreshReservation.Load() != f.owner {
			t.Fatal("stale empty observation cleared actual paid provider acquisition")
		}
		close(f.generator.availableRelease)
		f.requireLate(t, publication, f.started.Add(2500*time.Millisecond), "SERVICE_REFRESH_PRUNED_ACTUAL_REPLAY")
		if len(late.syns(f.path)) != 1 || !credit.released.Load() {
			t.Fatal("preserved listing failed exact retained replay and final credit return")
		}
		// requireLate already checked actual progress on every original sibling.
	})
}

// Invoked only after an unchanged acquisition deadline fails. No diagnostic
// work, hook, extra packet or synchronization is added to a successful path.
func masterServiceRefreshFailureCensus(t *testing.T, f *earlyIdleRefusalFixture) {
	t.Helper()
	reads, empty, mints, retired := f.generator.counts()
	f.window.stateLock.Lock()
	size := f.window.windowSizeWithLock()
	windowState := map[string]any{
		"owner_present":     f.window.serviceRefreshOwner != nil,
		"owner_is_original": f.window.serviceRefreshOwner == f.owner,
		"owner_live":        f.window.serviceRefreshLiveWithLock(),
		"attempted":         f.window.serviceRefreshAttempted,
		"stamp":             f.window.serviceRefreshStamp,
		"clients":           len(f.window.clients), "hard_max": size.WindowSizeHardMax,
		"backoff_failures":          f.window.discoveryRetry.failureCount,
		"backoff_remaining":         f.window.discoveryRetry.notBefore.Sub(time.Now()),
		"demand_owner_present":      f.window.discoveryDemand.owner != nil,
		"demand_slot_open":          f.window.discoveryDemand.slotOpen,
		"demand_pending":            f.window.discoveryDemand.pending,
		"demand_reads":              f.window.discoveryDemand.reads,
		"demand_target":             f.window.discoveryDemand.targetSize,
		"demand_mint_claimed":       f.window.discoveryDemand.mintClaimed,
		"demand_deadline_remaining": f.window.discoveryDemand.deadline.Sub(time.Now()),
	}
	f.window.stateLock.Unlock()
	f.update.stateLock.Lock()
	owner := f.update.synRetry
	updateState := map[string]any{
		"requested":      f.update.serviceRefreshRequested,
		"lineage":        f.update.serviceRefreshLineage.Load(),
		"stamp":          f.update.discoveryAttemptStamp.Load(),
		"cached_present": owner != nil, "cached_original": owner == f.owner,
		"original_refs":     f.owner.references,
		"victim_refused":    f.update.synRetryRemoteTailRefusedWithLock(f.victim),
		"standing_refusals": len(f.update.dialFailureSignaledClients),
		"failed_clients":    len(f.update.dialFailedClients),
		"failed_syn":        f.update.dialFailedSyn,
		"failed_syn_seen":   f.update.dialFailedSynSeen,
		"bound":             f.update.client.Load() != nil,
		"inbound":           f.update.receivedInbound.Load(),
		"source_reset":      f.update.sourceReset.Load(),
		"sequence_close":    f.update.sequenceCloseReady.Load(),
		"canceled":          f.update.ctx.Err() != nil,
	}
	if owner != nil && owner.references > 0 {
		updateState["owner_refs"], updateState["owner_serial"], updateState["owner_lineage"] = owner.references, owner.serial, owner.retryLineage
		updateState["owner_sequence"], updateState["owner_valid"] = owner.sequence, owner.validWithLock(f.update)
		updateState["pending_generation"] = owner.pendingGenerationChangeWithLock(f.update)
		updateState["remote_refusal_count"], updateState["remote_overflow"] = len(owner.refusedRemoteIds), owner.remoteRefusalOverflow
		updateState["credit_present"] = owner.credit != nil
		if owner.credit != nil {
			updateState["credit_released"], updateState["credit_bytes"] = owner.credit.released.Load(), owner.credit.byteCount
			if budget := owner.credit.budget; budget != nil {
				updateState["budget_used"], updateState["budget_total"] = budget.UsedByteCount(), budget.TotalByteCount()
			}
		}
		updateState["readiness_present"] = owner.readiness != nil
		if owner.readiness != nil {
			updateState["readiness_closed"], updateState["readiness_credit_present"] = owner.readiness.closed.Load(), owner.readiness.credit != nil
			if owner.readiness.credit != nil {
				updateState["readiness_credit_released"], updateState["readiness_bytes"] = owner.readiness.credit.released.Load(), owner.readiness.credit.byteCount
			}
		}
	}
	f.update.stateLock.Unlock()
	f.generator.mutex.Lock()
	holdEmpty, holdAvailable := f.generator.holdNextEmpty, f.generator.holdNextAvailable
	f.generator.mutex.Unlock()
	_, fixed := f.generator.FixedDestinationSize()
	t.Logf("MASTER_ACQUISITION_FAILURE age=%s reads_delta=%d empty_delta=%d mints_delta=%d retired=%d hold_empty=%t hold_available=%t victim_reserved_original=%t victim_race_refs=%d victim_flow_count=%d victim_done=%t victim_warning=%t strict=%t cooperative=%t degraded=%t fixed=%t capacity_now=%t window=%v update=%v", time.Since(f.started), reads-f.reads, empty-f.empty, mints-f.mints, retired, holdEmpty, holdAvailable, f.victim.serviceRefreshReservation.Load() == f.owner, f.victim.serviceRefreshRaceReferences.Load(), f.h.multiClient.clientFlowCount(f.victim), f.victim.IsDone(), f.victim.isWarning(), f.window.strictWindowHandoverEnabled(), f.window.cooperativeDiscoveryDemand(), f.window.backendDegraded(), fixed, f.window.serviceRefreshCapacity(f.window.unorderedClients()), windowState, updateState)
}
