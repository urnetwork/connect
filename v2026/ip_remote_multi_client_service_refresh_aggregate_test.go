// Aggregate controls compose partial authenticated refusal, native metadata
// continuity, actual provider publication and exact acquisition backoff.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Real constructors share a bounded ledger. Cleanup runs after all original,
// speculative and provider clients have joined, including negative controls.
func newAggregateServiceFixture(t *testing.T, port int, holdEmpty bool) (*earlyIdleRefusalFixture, func(*ClientSettings)) {
	t.Helper()
	root := NewTransferMemoryBudget(8 * 1024 * 1024)
	budget := NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
	t.Cleanup(func() {
		for _, ledger := range []*TransferMemoryBudget{budget, root} {
			reserved, released := ledger.Counts()
			if ledger.UsedByteCount() != 0 || reserved != released {
				t.Errorf("aggregate joined ledger retained bytes: used=%d reserved=%d released=%d", ledger.UsedByteCount(), reserved, released)
			}
		}
	})
	configure := func(settings *ClientSettings) {
		settings.SendBufferSettings.ResendQueueBudget = budget
		settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
	}
	f := newEarlyIdleRefusalFixtureWithBodyCount(t, port, holdEmpty, 4, configure)
	f.nativePacket = func() []byte { return synMetadataTestPacket(f.path, 101, 201, nil) }
	return f, configure
}

// A metadata reoffer crosses public native admission while the source's exact
// reservation is still owned. Neither its result nor cache replacement is faked.
func aggregateServiceMetadataReoffer(t *testing.T, f *earlyIdleRefusalFixture) *tcpSynRetry {
	t.Helper()
	before := f.h.multiClient.TcpCollapseDropCount()
	accepted := synRetryControlOffer(t, f.h, MessagePoolCopy(synMetadataTestPacket(f.path, 102, 202, nil)))
	f.update.stateLock.Lock()
	current := f.update.synRetry
	valid := current != nil && current.credit != nil && !current.credit.released.Load() &&
		current.retryLineage == f.owner.retryLineage && f.update.currentSynRetryLineageWithLock(f.owner) == current &&
		f.update.serviceRefreshLineage.Load() == f.owner.retryLineage &&
		f.update.synRetryRemoteTailRefusedWithLock(f.victim) && !f.update.receivedInbound.Load()
	f.update.stateLock.Unlock()
	if !valid || !accepted && f.h.multiClient.TcpCollapseDropCount() != before+1 {
		t.Fatalf("native metadata reoffer lost the exact paid partial-refusal lineage: valid=%t accepted=%t", valid, accepted)
	}
	t.Logf("AGGREGATE_METADATA_REOFFER accepted=%t immutable_successor=%t", accepted, current != f.owner)
	return current
}

// One refused idle provider can be replaced while four real contenders remain
// silent. The listing barrier orders native metadata ahead of actual publication;
// no additional native source packet may establish the fresh provider.
func TestAggregateIdleRefusalMetadataPublication(t *testing.T) {
	testAggregateIdleRefusalMetadataPublication(t, false)
}

// The same held listing publishes only after a real paid metadata successor.
func TestAggregatePaidMetadataSuccessorPublication(t *testing.T) {
	testAggregateIdleRefusalMetadataPublication(t, true)
}

// The default lifetime hold and explicit compatibility hold keep separate
// proofs; both require actual cached-wire delivery and a real target answer.
func testAggregateIdleRefusalMetadataPublication(t *testing.T, materialize bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		cause := ""
		t.Cleanup(func() {
			if !t.Failed() {
				t.Log("AGGREGATE_IDLE_REFUSAL_CLEANUP_JOINED")
				if cause != "" {
					t.Fatal(cause)
				}
			}
		})
		f, configure := newAggregateServiceFixture(t, 53201, false)
		if materialize {
			reliability := *f.h.multiClient.ReliabilitySettings()
			reliability.TcpCollapseMaxHold = time.Millisecond
			f.h.multiClient.SetReliabilitySettings(&reliability)
		}
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		lateExit := f.h.addExit(reraceExitSpec{name: "early-late", clientSettings: configure})
		wire := make(chan []byte, 4)
		lateExit.provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil {
					continue
				}
				path, err := ParseIpPath(packet)
				if err == nil && reraceSameFlow(path, f.path) && path.Syn && !path.Ack {
					wire <- bytes.Clone(packet)
				}
			}
		})
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			reads, _, mints, _ := f.generator.counts()
			if reads != f.reads || mints != f.mints {
				t.Fatal("baseline failure changed acquisition custody")
			}
			cause = "AGGREGATE_IDLE_REFUSAL_WAITED_FOR_UNRELATED_CONTENDERS"
			return
		}
		f.h.sleep(f.started.Add(1109 * time.Millisecond).Sub(time.Now()))
		originalCredit := f.owner.credit
		current := aggregateServiceMetadataReoffer(t, f)
		f.update.stateLock.Lock()
		successor := !materialize || current != f.owner && current.serial != f.owner.serial && f.owner.references == 1
		currentCredit := current.credit
		paid := bytes.Clone(current.packet)
		f.update.stateLock.Unlock()
		if !successor {
			t.Fatal("publication control did not materialize its distinct paid metadata successor")
		}
		// The pass that publishes the provider can hold the flow lock when the
		// paid SYN's publication visit arrives; hold the visit until it settles.
		visit, releaseVisit := f.holdPublicationVisit(t)
		close(f.generator.availableRelease)
		if !discoveryDemandTestAwaitPublication(t, visit, f.started.Add(2500*time.Millisecond)) {
			t.Fatal("published provider never reached the paid flow's publication visit")
		}
		f.h.settle()
		releaseVisit()
		f.requireLate(t, publication, f.started.Add(2500*time.Millisecond), "AGGREGATE_IDLE_REFUSAL_PUBLICATION_STALLED")
		select {
		case actual := <-wire:
			if !bytes.Equal(actual, paid) {
				t.Fatal("publication changed its actual paid native wire image")
			}
		default:
			t.Fatal("actual fresh provider received no retained native SYN")
		}
		if len(wire) != 0 {
			t.Fatal("publication duplicated the original retained native SYN")
		}
		if !originalCredit.released.Load() || !currentCredit.released.Load() {
			t.Fatal("actual target answer retained paid source-cache permission")
		}
		if materialize {
			t.Log("AGGREGATE_PAID_METADATA_PUBLICATION_PROVEN")
		}
		t.Log("AGGREGATE_IDLE_REFUSAL_PUBLICATION_PROVEN")
	})
}

// Busy refusal supplies no retirement authority for a different silent idle
// provider. Both remain real members and every original body still delivers.
func TestAggregateBusyRefusalCannotRetireHealthyIdleProvider(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(53202)
		var refused atomic.Int32
		refused.Store(-1)
		specs := make([]reraceExitSpec, 5)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if reraceSameFlow(candidate, path) {
					if refused.Load() == int32(index) {
						return reraceSignal
					}
					return reraceSilent
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) { settings.StrictWindowHandover = true })
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		reliability := *h.multiClient.ReliabilitySettings()
		reliability.MaxFlowsPerExit = DefaultMultiClientSettings().MaxFlowsPerExit
		h.multiClient.SetReliabilitySettings(&reliability)
		var idle *multiClientChannel
		for index := range specs {
			if h.exit(specs[index].name).destination == owners[0].Destination() {
				refused.Store(int32(index))
			}
		}
		for _, client := range window.unorderedClients() {
			if h.multiClient.clientFlowCount(client) == 0 {
				idle = client
			}
		}
		if idle == nil || refused.Load() < 0 {
			t.Fatal("negative lacks genuine busy refusal and idle sibling")
		}
		reads, _, mints, _ := generator.counts()
		h.addExit(reraceExitSpec{name: "available"})
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("negative original native source was not admitted")
		}
		update := h.update(path)
		update.stateLock.Lock()
		valid := len(update.dialFailureSignaledClients) == 1 && update.synRetryRemoteTailRefusedWithLock(owners[0]) && !update.synRetryRemoteTailRefusedWithLock(idle)
		update.stateLock.Unlock()
		if !valid {
			t.Fatal("negative lacks its exact real authenticated refusal provenance")
		}
		for _, candidate := range []*multiClientChannel{owners[0], idle} {
			if claimed, _ := h.multiClient.claimServiceRefreshOwner(update, path, candidate, h.liveChannels()); claimed != nil {
				t.Fatal("busy or healthy candidate acquired retirement authority")
			}
		}
		h.settle()
		afterReads, _, afterMints, _ := generator.counts()
		window.stateLock.Lock()
		reserved := window.serviceRefreshOwner != nil
		window.stateLock.Unlock()
		if reads != afterReads || mints != afterMints || reserved || idle.IsDone() || idle.isWarning() || idle.serviceRefreshReservation.Load() != nil {
			t.Fatal("AGGREGATE_BUSY_REFUSAL_RETIRED_HEALTHY_IDLE_PROVIDER")
		}
		// The real local-inference path releases the silent race reference but
		// grants no authenticated remote-refusal authority. This isolates the
		// victim-specific predicate from the earlier busy-reference exclusion.
		h.multiClient.clientDialFailure(idle, path, false)
		h.settle()
		update.stateLock.Lock()
		unrefused := !update.synRetryRemoteTailRefusedWithLock(idle)
		update.stateLock.Unlock()
		if !unrefused || idle.serviceRefreshRaceReferences.Load() != 0 || h.multiClient.clientFlowCount(idle) != 0 {
			t.Fatal("healthy-idle negative failed to isolate absent authenticated refusal")
		}
		if claimed, _ := h.multiClient.claimServiceRefreshOwner(update, path, idle, h.liveChannels()); claimed != nil {
			t.Fatal("AGGREGATE_UNREFUSED_IDLE_VICTIM_ACQUIRED_RETIREMENT")
		}
		afterReads, _, afterMints, _ = generator.counts()
		if reads != afterReads || mints != afterMints || idle.serviceRefreshReservation.Load() != nil || idle.IsDone() || idle.isWarning() {
			t.Fatal("inferred idle verdict borrowed another provider's actual refusal")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
	})
}

// Metadata survives an actual failed acquisition. A later original contender's
// useful answer clears this pass, including after its cached owner was released.
func TestAggregateMetadataLateAnswerClearsExactFailedPass(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, _ := newAggregateServiceFixture(t, 53203, true)
		// The explicit compatibility hold forces a real materialized successor;
		// default-policy acquisition and replay remain in the separate root.
		reliability := *f.h.multiClient.ReliabilitySettings()
		reliability.TcpCollapseMaxHold = time.Millisecond
		f.h.multiClient.SetReliabilitySettings(&reliability)
		f.start(t)
		if !discoveryDemandTestAwaitPublication(t, f.generator.emptyEntered, f.started.Add(2*time.Second)) {
			masterServiceRefreshFailureCensus(t, f)
			t.Fatal("held empty acquisition never began")
		}
		f.h.sleep(2 * time.Millisecond)
		current := aggregateServiceMetadataReoffer(t, f)
		f.update.stateLock.Lock()
		successor := current != f.owner && current.serial != f.owner.serial &&
			current.retryLineage == f.owner.retryLineage && f.owner.references == 1 &&
			bytes.Equal(current.packet, synMetadataTestPacket(f.path, 102, 202, nil))
		f.update.stateLock.Unlock()
		if !successor {
			t.Fatal("aggregate metadata control did not create a real paid immutable successor")
		}
		t.Log("AGGREGATE_PAID_METADATA_SUCCESSOR_PROVEN")
		close(f.generator.emptyRelease)
		f.h.sleep(f.started.Add(f.window.settings.WindowExpandTimeout + time.Second).Sub(time.Now()))
		f.window.stateLock.Lock()
		failed := f.window.discoveryRetry.failureCount == 1 && f.window.discoveryRetry.serviceLineage == f.owner.retryLineage && f.window.serviceRefreshOwner == nil
		f.window.stateLock.Unlock()
		if !failed {
			t.Fatal("metadata late answer lacks its actual completed failed pass")
		}
		f.answerOld(t)
		f.window.stateLock.Lock()
		cleared := f.window.discoveryRetry.failureCount == 0 && f.window.discoveryRetry.notBefore.IsZero()
		f.window.stateLock.Unlock()
		if !cleared {
			t.Fatal("AGGREGATE_METADATA_ANSWER_LEFT_OWN_BACKOFF")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// Strict swap retires only its own refused victim without invalidating the
// paid source. A delayed real target answer clears only its own failed pass.
func TestAggregateRetiredVictimLateTargetAnswerClearsOwnBackoff(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, configure := newAggregateServiceFixture(t, 53204, false)
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		f.h.sleep(f.started.Add(733 * time.Millisecond).Sub(time.Now()))
		late := f.h.addExit(reraceExitSpec{name: "early-late", clientSettings: configure, answer: func(candidate *IpPath) reraceAnswer {
			if reraceSameFlow(candidate, f.path) {
				return reraceSilent
			}
			return reraceAccept
		}})
		f.h.sleep(f.started.Add(1109 * time.Millisecond).Sub(time.Now()))
		beforeCollapse := f.h.multiClient.TcpCollapseDropCount()
		accepted := synRetryControlOffer(t, f.h, MessagePoolCopy(f.nativePacket()))
		if accepted || f.h.multiClient.TcpCollapseDropCount() != beforeCollapse+1 {
			t.Fatal("delayed target control did not reassert through the unchanged native collapse boundary")
		}
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			reads, empty, mints, _ := f.generator.counts()
			f.window.stateLock.Lock()
			windowOwner, attempted, deferred := f.window.serviceRefreshOwner != nil, f.window.serviceRefreshAttempted, f.window.discoveryRetry.notBefore
			f.window.stateLock.Unlock()
			f.update.stateLock.Lock()
			requested, lineage, stamp, bound := f.update.serviceRefreshRequested, f.update.serviceRefreshLineage.Load(), f.update.discoveryAttemptStamp.Load(), f.update.client.Load() != nil
			f.update.stateLock.Unlock()
			t.Fatalf("delayed target acquisition never reached held listing: reads=%d empty=%d mints=%d window_owner=%t attempted=%t deferred=%s requested=%t lineage=%d stamp=%d bound=%t", reads-f.reads, empty-f.empty, mints-f.mints, windowOwner, attempted, deferred, requested, lineage, stamp, bound)
		}
		close(f.generator.availableRelease)
		if !discoveryDemandTestAwaitPublication(t, publication, f.started.Add(2*time.Second)) {
			t.Fatal("delayed answer control never published its actual replacement")
		}
		f.h.settle()
		syns := late.syns(f.path)
		f.window.stateLock.Lock()
		failed := f.window.discoveryRetry.failureCount == 1 && f.window.discoveryRetry.serviceLineage == f.owner.retryLineage && f.window.serviceRefreshOwner == nil
		f.window.stateLock.Unlock()
		if !failed || len(syns) != 1 || !f.victim.IsDone() {
			t.Fatalf("delayed answer lacks actual swap/stamp/failure boundaries: failed=%t syns=%d victim_done=%t", failed, len(syns), f.victim.IsDone())
		}
		late.sendTo(syns[0].source, reraceSynAck(syns[0].path))
		f.h.settle()
		f.window.stateLock.Lock()
		cleared := f.window.discoveryRetry.failureCount == 0 && f.window.discoveryRetry.notBefore.IsZero()
		f.window.stateLock.Unlock()
		if _, answered := f.h.synAck(f.path); !answered || !cleared || f.h.boundTo(f.path) != f.h.channel("early-late") {
			t.Fatal("AGGREGATE_RETIRED_VICTIM_ANSWER_LEFT_OWN_BACKOFF")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// The explicit one-flow placement policy fills a four-provider retry race.
// Publication cannot invent a fifth slot; a real later refusal releases one
// and then the same paid source can reach the already-published alternative.
func TestAggregateFullRacePublicationKeepsBoundUntilActualRefusal(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, configure := newAggregateServiceFixture(t, 53205, false)
		reliability := *f.h.multiClient.ReliabilitySettings()
		reliability.MaxFlowsPerExit = 1
		f.h.multiClient.SetReliabilitySettings(&reliability)
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		late := f.h.addExit(reraceExitSpec{name: "early-late", clientSettings: configure})
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			t.Fatal("full-race control did not reach its actual held acquisition")
		}
		f.h.sleep(f.started.Add(1109 * time.Millisecond).Sub(time.Now()))
		aggregateServiceMetadataReoffer(t, f)
		close(f.generator.availableRelease)
		if !discoveryDemandTestAwaitPublication(t, publication, f.started.Add(2500*time.Millisecond)) {
			t.Fatal("full-race control did not actually publish its alternative")
		}
		f.h.settle()
		f.update.stateLock.Lock()
		race := f.update.race
		limit, candidates, slots := 0, 0, f.h.multiClient.synRetryRaceSlotsWithLock(f.update, nil)
		if race != nil {
			limit, candidates = race.clientLimit, len(race.clientStates)
		}
		paid := f.update.synRetry != nil && f.update.synRetry.validWithLock(f.update) && f.update.synRetry.credit != nil && !f.update.synRetry.credit.released.Load()
		stamp := f.update.discoveryAttemptStamp.Load()
		f.update.stateLock.Unlock()
		_, answered := f.h.synAck(f.path)
		t.Logf("AGGREGATE_FULL_RACE_CENSUS limit=%d candidates=%d free_slots=%d paid=%t wire_syns=%d answered=%t original_stamp=%d current_stamp=%d", limit, candidates, slots, paid, len(late.syns(f.path)), answered, f.stamp, stamp)
		if limit != 4 || candidates != 4 || slots != 0 || !paid || len(late.syns(f.path)) != 0 || answered {
			t.Fatal("publication changed the existing full race's paid capacity or fabricated useful proof")
		}
		old := f.siblingOwners[0]
		var refuser *reraceExit
		for _, exit := range f.h.generator.exitList() {
			if exit.destination == old.Destination() {
				refuser = exit
			}
		}
		if refuser == nil || len(refuser.syns(f.path)) == 0 {
			t.Fatal("full-race control lacks an actual original unanswered contender")
		}
		wire := refuser.syns(f.path)[0]
		refuser.sendTo(wire.source, reraceProviderSignal(t, wire.path))
		f.h.settle()
		if _, answered := f.h.synAck(f.path); !answered || len(late.syns(f.path)) != 1 || f.h.boundTo(f.path) != f.h.channel("early-late") {
			t.Fatal("actual refusal did not release exactly one existing slot for retained-source recovery")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// One actual first empty listing, partial refusal, materialized metadata
// successor, own-victim retirement and delayed target answer share one source.
// Two native offers precede publication; retained delivery needs no third.
func TestAggregateEmptyListingMetadataRetirementLateAnswer(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, configure := newAggregateServiceFixture(t, 53206, true)
		reliability := *f.h.multiClient.ReliabilitySettings()
		reliability.TcpCollapseMaxHold = time.Millisecond
		f.h.multiClient.SetReliabilitySettings(&reliability)
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		if !discoveryDemandTestAwaitPublication(t, f.generator.emptyEntered, f.started.Add(2*time.Second)) {
			t.Fatal("combined control never entered its actual initial empty listing")
		}
		f.h.sleep(f.started.Add(733 * time.Millisecond).Sub(time.Now()))
		late := f.h.addExit(reraceExitSpec{name: "early-late", clientSettings: configure, answer: func(path *IpPath) reraceAnswer {
			if reraceSameFlow(path, f.path) {
				return reraceSilent
			}
			return reraceAccept
		}})
		wirePackets := make(chan []byte, 2)
		late.provider.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil {
					continue
				}
				path, err := ParseIpPath(packet)
				if err == nil && reraceSameFlow(path, f.path) && path.Syn && !path.Ack {
					select {
					case wirePackets <- bytes.Clone(packet):
					default:
						t.Error("combined publication duplicated bounded target delivery")
					}
				}
			}
		})
		close(f.generator.emptyRelease)
		f.h.settle()
		f.h.sleep(f.started.Add(1109 * time.Millisecond).Sub(time.Now()))
		originalCredit := f.owner.credit
		current := aggregateServiceMetadataReoffer(t, f)
		f.update.stateLock.Lock()
		successor := current != f.owner && current.serial != f.owner.serial && current.retryLineage == f.owner.retryLineage && f.owner.references == 1
		currentCredit, paid := current.credit, bytes.Clone(current.packet)
		f.update.stateLock.Unlock()
		if !successor || !bytes.Equal(paid, synMetadataTestPacket(f.path, 102, 202, nil)) {
			t.Fatal("combined control did not materialize its real metadata successor")
		}
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			t.Fatal("metadata-native reassertion did not relist inside the same bounded acquisition")
		}
		close(f.generator.availableRelease)
		if !discoveryDemandTestAwaitPublication(t, publication, f.started.Add(2500*time.Millisecond)) {
			t.Fatal("combined control did not publish its actual replacement")
		}
		f.h.settle()
		reads, empty, mints, _ := f.generator.counts()
		wire := late.syns(f.path)
		f.window.stateLock.Lock()
		failed := f.window.serviceRefreshOwner == nil && f.window.discoveryRetry.failureCount == 1 && f.window.discoveryRetry.serviceLineage == f.owner.retryLineage
		f.window.stateLock.Unlock()
		t.Logf("AGGREGATE_COMBINED_STAMP before=%d after=%d", f.stamp, f.update.discoveryAttemptStamp.Load())
		if reads != f.reads+2 || empty != f.empty+1 || mints != f.mints+1 || len(f.window.unorderedClients()) != 5 ||
			len(wire) != 1 || !failed || !f.victim.IsDone() ||
			!originalCredit.released.Load() || currentCredit.released.Load() {
			t.Fatalf("combined empty/metadata/retirement boundary failed: reads=%d empty=%d mints=%d wire_syns=%d failed=%t", reads-f.reads, empty-f.empty, mints-f.mints, len(wire), failed)
		}
		select {
		case actual := <-wirePackets:
			if !bytes.Equal(actual, paid) {
				t.Fatal("combined publication did not send its latest paid metadata image")
			}
		default:
			t.Fatal("combined target has no actual retained wire packet")
		}
		if len(wirePackets) != 0 {
			t.Fatal("combined retained source was duplicated")
		}
		if _, answered := f.h.synAck(f.path); answered {
			t.Fatal("silent target fabricated useful response before its real answer")
		}
		late.sendTo(wire[0].source, reraceSynAck(wire[0].path))
		f.h.settle()
		f.window.stateLock.Lock()
		cleared := f.window.discoveryRetry.failureCount == 0 && f.window.discoveryRetry.notBefore.IsZero()
		f.window.stateLock.Unlock()
		if _, answered := f.h.synAck(f.path); !answered || !cleared || !currentCredit.released.Load() || f.h.boundTo(f.path) != f.h.channel("early-late") {
			t.Fatal("combined real target answer did not settle its source, credit and exact backoff")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
		t.Log("AGGREGATE_EMPTY_METADATA_RETIREMENT_LATE_ANSWER_PROVEN")
	})
}
