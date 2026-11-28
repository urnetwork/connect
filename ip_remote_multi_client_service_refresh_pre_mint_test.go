package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A new refusal recorded while the actual discovery result is held must
// prevent minting a fresh local client for that physical remote.
func TestServiceRefreshSkipsRemoteRefusedAfterListing(t *testing.T) {
	testServiceRefreshHeldCandidate(t, false, false)
}

// An independently eligible answer to the same kind of held read must
// still get one mint and deliver the original retained SYN.
func TestServiceRefreshKeepsEligibleHeldCandidate(t *testing.T) {
	testServiceRefreshHeldCandidate(t, true, false)
}

// An ordinary quality read can already contain both the refused destination
// and an eligible alternative. Only the latter may spend the one mint.
func TestServiceRefreshSkipsRefusedFirstOfHeldListing(t *testing.T) {
	testServiceRefreshHeldCandidate(t, true, true)
}

// The cases add a real remote refusal to the same paid SYN while the
// generator holds one actual, already chosen discovery result.
func testServiceRefreshHeldCandidate(t *testing.T, eligible, alternate bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var adapter *serviceRefreshHeldListingGenerator
		var wrap func(*discoveryDemandTestGenerator) MultiClientGenerator
		if alternate {
			wrap = func(generator *discoveryDemandTestGenerator) MultiClientGenerator {
				adapter = &serviceRefreshHeldListingGenerator{discoveryDemandTestGenerator: generator}
				return adapter
			}
		}
		f := newEarlyIdleRefusalFixtureWithGenerator(t, 53131, false, 4, wrap)
		stale := f.siblingOwners[0]
		var staleExit *reraceExit
		for _, exit := range f.h.generator.exitList() {
			if exit.destination == stale.Destination() {
				staleExit = exit
				break
			}
		}
		if staleExit == nil {
			t.Fatal("held listing lacks the original remote contender")
		}
		listedExit := staleExit
		if eligible {
			listedExit = f.h.addExit(reraceExitSpec{name: "held-eligible"})
		}
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.resultOverrideForTest = map[MultiHopId]DestinationStats{
			listedExit.destination: {IpFamily: listedExit.family},
		}
		if alternate {
			// The stock selector sorts the one dualstack result ahead of
			// IPv4 fill while its real dualstack target is unsatisfied.
			f.generator.resultOverrideForTest[staleExit.destination] = DestinationStats{IpFamily: IpFamilyDualstack}
			f.generator.resultOverrideForTest[listedExit.destination] = DestinationStats{IpFamily: IpFamilyV4Only}
		}
		f.generator.mutex.Unlock()
		beforeReads, beforeEmpty, beforeMints, beforeRetired := f.generator.counts()
		beforeEvaluation := listedExit.evaluations.Load()
		beforeStaleEvaluation := staleExit.evaluations.Load()
		var evaluated <-chan struct{}
		if eligible {
			evaluated = discoveryDemandTestObservePublication(f.window)
		}

		f.start(t)
		deadline := f.started.Add(f.window.settings.WindowExpandTimeout)
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, deadline) {
			t.Fatal("actual nonempty listing did not reach its return barrier")
		}
		if alternate {
			adapter.mutex.Lock()
			call := adapter.heldCall
			adapter.mutex.Unlock()
			if call.count != 4 || call.ipFamily != IpFamilyFilterV4Capable ||
				call.selection.HeldDualstackCount != 0 || call.selection.DualstackTargetCount <= 0 {
				t.Fatalf("held answer lacks the ordinary Count=4 dualstack-first selection: %+v", call)
			}
		}
		waitingReads, waitingEmpty, waitingMints, waitingRetired := f.generator.counts()
		if waitingReads != beforeReads+1 || waitingEmpty != beforeEmpty ||
			waitingMints != beforeMints || waitingRetired != beforeRetired {
			t.Fatalf("held discovery does not own one unspent read: reads=%d empty=%d mints=%d retired=%d", waitingReads-beforeReads, waitingEmpty-beforeEmpty, waitingMints-beforeMints, waitingRetired-beforeRetired)
		}
		snapshot := f.window.serviceRefreshSnapshot()
		f.update.stateLock.Lock()
		lineage := f.update.currentSynRetryLineageWithLock(f.owner)
		alreadyRefused := f.update.synRetryRemoteTailRefusedWithLock(stale)
		f.update.stateLock.Unlock()
		originalSyns := staleExit.syns(f.path)
		if snapshot.owner != f.owner || snapshot.update != f.update || snapshot.stamp != f.stamp ||
			lineage != f.owner || alreadyRefused || len(originalSyns) != 1 ||
			originalSyns[0].source != stale.ClientId() || originalSyns[0].path.SequenceNumber != f.path.SequenceNumber {
			t.Fatal("missing standing original SYN at the exact newly refused remote")
		}

		original := originalSyns[0]
		staleExit.sendTo(original.source, reraceProviderSignal(t, original.path))
		f.h.settle()
		f.update.stateLock.Lock()
		stillPaid := f.update.currentSynRetryLineageWithLock(f.owner) == lineage &&
			f.owner.serial == f.serial && f.update.discoveryAttemptStamp.Load() == f.stamp &&
			f.owner.packet != nil && f.owner.credit != nil && !f.owner.credit.released.Load()
		refused := f.update.synRetryRemoteTailRefusedWithLock(stale) &&
			f.update.synRetryRemoteTailRefusedWithLock(f.victim)
		inbound := f.update.receivedInbound.Load()
		f.update.stateLock.Unlock()
		if !stillPaid || !refused || inbound || f.h.update(f.path) != f.update ||
			f.window.serviceRefreshSnapshot() != snapshot || f.h.metrics().DialFailuresIntercepted != 2 {
			t.Fatal("second real refusal did not join the same paid service refresh before listing return")
		}
		if reads, empty, mints, retired := f.generator.counts(); reads != waitingReads ||
			empty != waitingEmpty || mints != waitingMints || retired != waitingRetired {
			t.Fatal("new refusal escaped the held listing or consumed another discovery call")
		}
		resumedAt := time.Now()
		close(f.generator.availableRelease)
		f.h.settle()
		if !time.Now().Equal(resumedAt) {
			t.Fatal("held-result continuation consumed a discovery retry timer")
		}
		reads, empty, mints, retired := f.generator.counts()
		evaluations := listedExit.evaluations.Load() - beforeEvaluation
		if reads != waitingReads || empty != waitingEmpty || len(f.window.unorderedClients()) != 5 {
			t.Fatal("held-result continuation escaped its original read or normal-window ceiling")
		}
		if alternate {
			staleEvaluations := staleExit.evaluations.Load() - beforeStaleEvaluation
			_, answered := f.h.synAck(f.path)
			if staleEvaluations == 1 && evaluations == 0 && mints == beforeMints+1 &&
				retired == beforeRetired+1 && !answered && f.h.update(f.path) == f.update &&
				len(staleExit.syns(f.path)) == 1 && len(listedExit.syns(f.path)) == 0 {
				t.Fatalf("SERVICE_REFRESH_MINTED_FIRST_REFUSED_WITH_ELIGIBLE_ALTERNATE: reads=%d mints=%d stale_evaluations=%d alternate_evaluations=%d", reads-beforeReads, mints-beforeMints, staleEvaluations, evaluations)
			}
			if staleEvaluations != 0 {
				t.Fatalf("ordinary listing evaluated a refused identity: %d", staleEvaluations)
			}
		}
		if !eligible {
			_, answered := f.h.synAck(f.path)
			if answered || len(staleExit.syns(f.path)) != 1 || f.h.update(f.path) != f.update {
				t.Fatal("refused remote changed the original unestablished generation")
			}
			if mints == beforeMints+1 && evaluations == 1 && retired == beforeRetired+1 {
				t.Fatalf("SERVICE_REFRESH_MINTED_REMOTE_REFUSED_AFTER_LISTING: reads=%d mints=%d evaluations=%d retired=%d same_paid_syn=true", reads-beforeReads, mints-beforeMints, evaluations, retired-beforeRetired)
			}
			if mints != beforeMints || evaluations != 0 || retired != beforeRetired {
				t.Fatalf("refused remote escaped before mint: mints=%d evaluations=%d retired=%d", mints-beforeMints, evaluations, retired-beforeRetired)
			}
		} else {
			// The decision above owns its exact instant. Await the actual
			// evaluation callback and retained original ACK separately.
			// A contended reaper can use its existing publication retry.
			deliveryDeadline := resumedAt.Add(f.window.settings.MultiRaceSetOnResponseTimeout)
			evaluationObserved := discoveryDemandTestAwaitPublication(t, evaluated, deliveryDeadline)
			answerObserved := evaluationObserved && f.h.waitForSynAck(f.path, deliveryDeadline)
			if !answerObserved {
				t.Fatalf("eligible retained SYN did not finish within the existing race response bound: evaluation_callback=%t", evaluationObserved)
			}
			if afterReads, afterEmpty, afterMints, _ := f.generator.counts(); afterReads != reads ||
				afterEmpty != empty || afterMints != mints || len(f.window.unorderedClients()) != 5 {
				t.Fatal("eligible delivery added discovery work or exceeded its five-slot window")
			}
			late := f.h.boundTo(f.path)
			syns := listedExit.syns(f.path)
			reply, answered := f.h.synAck(f.path)
			if mints != beforeMints+1 || evaluations != 1 || !answered || late == nil ||
				late.Destination() != listedExit.destination || len(syns) != 1 ||
				syns[0].source != late.ClientId() || syns[0].path.SequenceNumber != f.path.SequenceNumber ||
				f.h.update(f.path) != f.update {
				t.Fatalf("eligible remote lost original one-mint delivery: mints=%d evaluations=%d answered=%t native_syns=%d", mints-beforeMints, evaluations, answered, len(syns))
			}
			parsed, err := ParseIpPath(reply.packet)
			if err != nil || parsed.AckSequenceNumber != f.path.SequenceNumber+1 {
				t.Fatal("eligible remote did not acknowledge the original SYN")
			}
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
		reraceAssertNoErrorDelivered(t, f.h, f.path)
		t.Logf("SERVICE_REFRESH_HELD_CANDIDATE: eligible=%t reads=%d mints=%d evaluations=%d", eligible, reads-beforeReads, mints-beforeMints, evaluations)
	})
}

// The five original channels advertise IPv4. The held selector response can
// advertise an improved label on the stale remote, so the stock sort chooses
// it first without depending on Go map traversal. No selection is applied by
// this generator. Its inherited counters observe actual reads and mints.
type serviceRefreshHeldListingGenerator struct {
	*discoveryDemandTestGenerator
	heldCall struct {
		count     int
		ipFamily  IpFamilyFilter
		selection FindProvidersSelect
	}
}

// The fixture echoes real selector requests from the normal quality window.
func (self *serviceRefreshHeldListingGenerator) SelectorMode() (bool, bool) {
	return true, true
}

// Both the normal listing and the bounded discovery path use the same fixture.
func (self *serviceRefreshHeldListingGenerator) NextDestinationsWithCapabilities(count int, excluded []MultiHopId, rankMode string, family IpFamilyFilter, selection FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return self.NextDestinationsWithCapabilitiesContext(context.Background(), count, excluded, rankMode, family, selection)
}

// The limited window has no eligible tcp-only provider in this fixture.
func (self *serviceRefreshHeldListingGenerator) NextDestinationsWithCapabilitiesContext(ctx context.Context, count int, excluded []MultiHopId, rankMode string, family IpFamilyFilter, selection FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	if selection.Protocol == ProviderProtocolCapabilityTcp {
		return nil, true, nil
	}
	self.mutex.Lock()
	held := rankMode == WindowTypeQuality.RankMode() && self.holdNextAvailable
	if held {
		self.heldCall.count = count
		self.heldCall.ipFamily = family
		self.heldCall.selection = selection
	}
	self.mutex.Unlock()
	destinations, err := self.NextDestinationsWithIpFamilyContext(ctx, count, excluded, rankMode, family)
	for destination, stats := range destinations {
		if !held {
			stats.IpFamily = IpFamilyV4Only
		}
		stats.Capabilities = providerCapabilitiesFromIpFamily(stats.IpFamily)
		destinations[destination] = stats
	}
	return destinations, true, err
}

// The fixture supplies only recorded pool answers.
func (self *serviceRefreshHeldListingGenerator) NextDestinationsForClientIdWithCapabilities(clientId Id, excluded []MultiHopId, rankMode string, selection FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return nil, true, nil
}
