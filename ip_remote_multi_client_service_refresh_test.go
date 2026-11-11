// Full-window service recovery preserves exact source, slot and body ownership.
// Real native packets supply failure evidence; gates order competing admissions.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Four healthy held bodies leave exactly one idle slot in the full five-slot
// quality window. The late provider does not exist during source qualification.
func newFullServiceRefreshFixture(t *testing.T, port int) (*reraceHarness, *discoveryDemandTestGenerator, *multiClientWindow, *IpPath, []*IpPath, []*multiClientChannel) {
	t.Helper()
	path := unboundFlowTcpPath(port)
	specs := make([]reraceExitSpec, 5)
	for index := range specs {
		specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
			if candidate.DestinationIp.Equal(path.DestinationIp) && candidate.DestinationPort == path.DestinationPort {
				return reraceSignal
			}
			return reraceAccept
		}}
	}
	h, generator := newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) { settings.StrictWindowHandover = true })
	window, paths, owners := discoveryDemandTestCarryFour(t, h)
	synctest.Wait()
	if len(window.unorderedClients()) != 5 || window.strictWindowAdmissionAllowed(NewId(), window.settings.WindowSizes[WindowTypeQuality]) {
		t.Fatal("service control lacks the full existing strict ceiling")
	}
	return h, generator, window, path, paths, owners
}

// A delayed old bind must not return the new winner's unpublished token.
// Parent publication of the actual winner is the only successful handoff.
func TestFullServiceRefreshStaleBindKeepsCurrentWinnerReference(t *testing.T) {
	parent := &RemoteUserNatMultiClient{settings: DefaultMultiClientSettings(), clientUpdates: map[*multiClientChannel]map[*multiClientChannelUpdate]bool{}}
	first := &multiClientChannel{ctx: context.Background(), args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()}}}
	second := &multiClientChannel{ctx: context.Background(), args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()}}}
	update := newMultiClientChannelUpdate(context.Background(), unboundFlowTcpPath(53011))
	t.Cleanup(update.Close)
	if !first.tryServiceRefreshRaceReference() || !second.tryServiceRefreshRaceReference() {
		t.Fatal("healthy candidate references were refused before any idle fence")
	}
	update.stateLock.Lock()
	update.selectAffinityClient(first)
	update.transferServiceRefreshBindingWithLock(first)
	update.selectAffinityClient(second)
	update.transferServiceRefreshBindingWithLock(second)
	update.stateLock.Unlock()
	parent.bindClientFlow(update, first)
	if first.serviceRefreshRaceReferences.Load() != 0 || second.serviceRefreshRaceReferences.Load() != 1 || parent.clientFlowCount(second) != 0 {
		t.Fatal("stale bookkeeping returned an unpublished current winner's exact reference")
	}
	parent.bindClientFlow(update, second)
	parent.bindClientFlow(update, first)
	if second.serviceRefreshRaceReferences.Load() != 0 || parent.clientFlowCount(second) != 1 {
		t.Fatal("actual current winner did not transfer exactly once into steady bookkeeping")
	}
}

// Ordinary SYN observation must reject before membership snapshots, discovery
// calls or backing allocation. The missing generator makes the boundary exact.
func TestFullServiceRefreshHealthySourceAllocatesNoRefreshScratch(t *testing.T) {
	parent := &RemoteUserNatMultiClient{ctx: context.Background()}
	path := unboundFlowTcpPath(53012)
	update := newMultiClientChannelUpdate(context.Background(), path)
	t.Cleanup(update.Close)
	for _, established := range []bool{false, true} {
		update.receivedInbound.Store(established)
		if allocations := testing.AllocsPerRun(1000, func() { parent.requestServiceRefresh(update, path) }); allocations != 0 {
			t.Fatalf("healthy source allocated refresh scratch: established=%t allocations=%v", established, allocations)
		}
	}
}

// Opaque identity tokens force preemption/reclaim and swap/cancel orderings.
// Neither stale cleanup nor recovery can reopen an already committed victim.
func TestFullServiceRefreshReservationIdentitySurvivesPreemptAndReclaim(t *testing.T) {
	client := &multiClientChannel{ctx: context.Background()}
	first, second := &tcpSynRetry{}, &tcpSynRetry{}
	if !client.serviceRefreshReservation.CompareAndSwap(nil, first) || !client.preemptServiceRefresh() ||
		!client.serviceRefreshReservation.CompareAndSwap(nil, second) {
		t.Fatal("control failed to order one reservation's preemption before another source's claim")
	}
	if client.releaseServiceRefreshReservation(first) || client.serviceRefreshReservation.Load() != second {
		t.Fatal("SERVICE_REFRESH_RESERVATION_ABA: stale cleanup removed another source's exact reservation")
	}
	if !client.serviceRefreshReservation.CompareAndSwap(second, &serviceRefreshRetiringOwner) {
		t.Fatal("the actual current reservation could not commit its retirement")
	}
	if client.preemptServiceRefresh() || client.releaseServiceRefreshReservation(first) ||
		client.releaseServiceRefreshReservation(second) || client.releaseServiceRefreshReservation(&serviceRefreshRetiringOwner) ||
		client.tryServiceRefreshRaceReference() || client.serviceRefreshReservation.Load() != &serviceRefreshRetiringOwner ||
		client.serviceRefreshRaceReferences.Load() != 0 {
		t.Fatal("committed service retirement reopened a channel for new or recovered flows")
	}
}

// The distinct send-only winner path must transfer the same candidate token.
// Its normal silence budget chooses an owner without inventing inbound proof.
func TestFullServiceRefreshSendOnlyWinnerBeforeBookkeepingIsNotIdle(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(53013)
		specs := make([]reraceExitSpec, 2)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.Protocol == IpProtocolUdp {
					return reraceSilent
				}
				if candidate.DestinationIp.Equal(path.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h := newReraceHarness(t, specs, nil)
		privateCrossWindowNativeSyn(h, path)
		update := h.update(path)
		body := reraceUdpFlow(4, 92, 53014)
		body.DestinationPort = 9000
		offer := func() bool {
			packet := batchResultTestUdpPacket(body, []byte("original send-only application payload"))
			accepted := [1]bool{}
			count := h.multiClient.SendNativePacketBatchWithResults(h.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
			h.settle()
			return count == 1 && accepted[0]
		}
		if !offer() {
			t.Fatal("initial native send-only packet did not acquire actual provider custody")
		}
		bodyUpdate := h.update(body)
		bodyUpdate.stateLock.Lock()
		wide := bodyUpdate.client.Load() == nil && bodyUpdate.race != nil && len(bodyUpdate.race.clientStates) == 2
		bodyUpdate.stateLock.Unlock()
		if !wide {
			t.Fatal("send-only control did not retain its original two-provider unanswered race")
		}
		h.sleep(h.multiClient.settings.MultiRaceSetOnNoResponseTimeout)
		entered, release := make(chan *multiClientChannel, 1), make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		t.Cleanup(finish)
		h.multiClient.beforeServiceRefreshBindForTest = func(current *multiClientChannelUpdate, client *multiClientChannel) {
			if current == bodyUpdate {
				select {
				case entered <- client:
				default:
				}
				<-release
			}
		}
		h.concurrent = true
		bodyDone := make(chan bool, 1)
		go func() { bodyDone <- offer() }()
		synctest.Wait()
		var victim *multiClientChannel
		select {
		case victim = <-entered:
		default:
			t.Fatal("normal send-only winner did not reach parent bookkeeping")
		}
		if h.multiClient.clientFlowCount(victim) != 0 || bodyUpdate.receivedInbound.Load() {
			t.Fatal("send-only control lacks an unindexed actual winner without inbound proof")
		}
		if victim.serviceRefreshRaceReferences.Load() == 0 {
			t.Fatal("SERVICE_REFRESH_SEND_ONLY_WINNER_TOKEN_LOST: actual send-only winner lost ownership before parent bookkeeping")
		}
		claimed, _ := h.multiClient.claimServiceRefreshOwner(update, path, victim, h.liveChannels())
		if claimed != nil {
			victim.releaseServiceRefreshReservation(claimed)
			update.stateLock.Lock()
			update.serviceRefreshRequested = false
			update.releaseSynRetryReferenceWithLock(claimed)
			update.stateLock.Unlock()
			update.releaseSynRetryMemory()
			t.Fatal("SERVICE_REFRESH_SEND_ONLY_WINNER_BOOKKEEPING_GAP_WAS_IDLE: a real admitted send-only flow lost its candidate token")
		}
		finish()
		synctest.Wait()
		if !<-bodyDone || h.boundTo(body) != victim || victim.IsDone() || h.multiClient.clientFlowCount(victim) != 1 || victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("send-only winner failed its exact candidate-to-map ownership handoff")
		}
	})
}

// Unavailable replacement is an honest non-recovery with bounded reads and no
// identity churn. The original idle slot and every held body remain owned.
func TestFullServiceRefreshNoAlternativeKeepsOwnersAndMintBudget(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, generator, window, path, paths, owners := newFullServiceRefreshFixture(t, 53001)
		original := window.unorderedClients()
		beforeReads, _, beforeMints, beforeRetired := generator.counts()
		start := time.Now()
		for _, offset := range reraceSynSchedule[:4] {
			h.sleep(start.Add(offset).Sub(time.Now()))
			privateCrossWindowNativeSyn(h, path)
		}
		h.sleep(start.Add(window.settings.WindowExpandTimeout).Sub(time.Now()))
		reads, _, mints, retired := generator.counts()
		if reads <= beforeReads || reads > beforeReads+1+window.settings.DiscoveryDemandListingsPerPass ||
			mints != beforeMints || retired != beforeRetired || len(window.unorderedClients()) != 5 {
			t.Fatalf("empty service refresh exceeded its unchanged acquisition budget: reads=%d mints=%d retired=%d", reads-beforeReads, mints-beforeMints, retired-beforeRetired)
		}
		for _, client := range original {
			window.stateLock.Lock()
			installed := window.clients[client.ClientId()] == client
			window.stateLock.Unlock()
			if !installed || client.IsDone() || client.isWarning() || client.serviceRefreshReservation.Load() != nil {
				t.Fatal("empty service refresh retired or stranded an original owner")
			}
		}
		if _, answered := h.synAck(path); answered || h.update(path).receivedInbound.Load() {
			t.Fatal("unavailable replacement fabricated useful navigation proof")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
	})
}

// Cancellation ends the bounded reservation before later availability can
// mint a derived client. Its window reference returns the paid source owner.
func TestFullServiceRefreshCanceledSourceCannotMintLateProvider(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, generator, window, path, paths, owners := newFullServiceRefreshFixture(t, 53002)
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("cancel control's actual original source was not admitted")
		}
		update := h.update(path)
		owner := discoveryDemandServiceRefreshOwner(t, window, update)
		credit := owner.credit
		update.Close()
		h.settle()
		_, _, beforeMints, _ := generator.counts()
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(reraceSynSchedule[2])
		_, _, mints, _ := generator.counts()
		if mints != beforeMints || len(h.exit("late").syns(path)) != 0 || owner.packet != nil || owner.credit != nil || owner.refusedRemoteIds != nil || !credit.released.Load() {
			t.Fatal("canceled source minted, retried or retained its paid service reservation")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
	})
}

// A provider ping is not service recovery. Preserve the existing expansion
// retry streak through idle rotation until an actual navigation answer exists.
func TestFullServiceRefreshPreservesExistingDiscoveryBackoff(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, generator, window, path, paths, owners := newFullServiceRefreshFixture(t, 53003)
		window.noteDiscoveryExpandUnproductive(time.Now())
		window.stateLock.Lock()
		deadline, failures := window.discoveryRetry.notBefore, window.discoveryRetry.failureCount
		window.stateLock.Unlock()
		beforeReads, _, beforeMints, _ := generator.counts()
		start := time.Now()
		for _, offset := range reraceSynSchedule[:3] {
			h.sleep(start.Add(offset).Sub(time.Now()))
			privateCrossWindowNativeSyn(h, path)
		}
		reads, _, mints, _ := generator.counts()
		window.stateLock.Lock()
		unchanged := window.discoveryRetry.failureCount == failures && window.discoveryRetry.notBefore.Equal(deadline)
		window.stateLock.Unlock()
		if !unchanged || !time.Now().Before(deadline) || reads != beforeReads || mints != beforeMints {
			t.Fatal("source refusals bypassed or reset the existing discovery deferral")
		}
		// Construction changes availability only. Its ping answers normally;
		// its new navigation remains silent, so service proof is still absent.
		h.addExit(reraceExitSpec{name: "late", answer: func(candidate *IpPath) reraceAnswer {
			if candidate.DestinationIp.Equal(path.DestinationIp) {
				return reraceSilent
			}
			return reraceAccept
		}})
		publication := discoveryDemandTestObservePublication(window)
		var next time.Time
		for _, offset := range reraceSynSchedule {
			if !start.Add(offset).Before(deadline) {
				next = start.Add(offset)
				break
			}
		}
		if next.IsZero() {
			t.Fatal("fixture's unchanged source schedule does not reach the retry edge")
		}
		h.sleep(next.Sub(time.Now()))
		privateCrossWindowNativeSyn(h, path)
		if !discoveryDemandTestAwaitPublication(t, publication, next.Add(window.settings.WindowExpandTimeout)) {
			t.Fatal("eligible source did not consume its first actual post-deferral acquisition")
		}
		h.settle()
		window.stateLock.Lock()
		kept := window.discoveryRetry.failureCount == failures+1 && time.Now().Before(window.discoveryRetry.notBefore)
		window.stateLock.Unlock()
		if !kept {
			t.Fatal("idle slot rotation or successful ping erased an unproven service's retry streak")
		}
		if _, answered := h.synAck(path); answered {
			t.Fatal("silent new destination unexpectedly supplied service proof")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
	})
}

// Existing bodies cannot be traded for a new service at the strict cap. Even
// a physically available late provider cannot justify cutting any held socket.
func TestFullServiceRefreshAllSlotsCarryingKeepEveryOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, generator, window, path, paths, owners := newFullServiceRefreshFixture(t, 53004)
		last := unboundFlowTcpPath(53005)
		last.DestinationIp[3] = 99
		if !h.syn(last) || h.boundTo(last) == nil {
			t.Fatal("fifth actual body did not acquire the remaining idle slot")
		}
		lastOwner := h.boundTo(last)
		for _, owner := range owners {
			if owner == lastOwner {
				t.Fatal("fifth held body reused an already occupied fixture slot")
			}
		}
		payload := []byte("fifth original held body")
		if !h.send(reraceSegment(last, 0, payload)) {
			t.Fatal("fifth body did not make actual progress")
		}
		beforeReads, _, beforeMints, _ := generator.counts()
		privateCrossWindowNativeSyn(h, path)
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(reraceSynSchedule[1])
		privateCrossWindowNativeSyn(h, path)
		reads, _, mints, _ := generator.counts()
		if reads != beforeReads || mints != beforeMints || len(window.unorderedClients()) != 5 || h.boundTo(last) != lastOwner || lastOwner.IsDone() || lastOwner.isWarning() ||
			!h.send(reraceSegment(last, uint32(len(payload)), payload)) {
			t.Fatal("full-window recovery cut a held body or minted past its existing cap")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
	})
}

// The source has actual refusals, but this independent body winner is held
// after its leaf publication and before the parent map can count it.
func TestFullServiceRefreshWinnerBeforeBookkeepingIsNotIdle(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(53006)
		specs := make([]reraceExitSpec, 2)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h := newReraceHarness(t, specs, nil)
		privateCrossWindowNativeSyn(h, path)
		update := h.update(path)
		body := unboundFlowTcpPath(53007)
		body.DestinationIp[3] = 100
		entered, release := make(chan *multiClientChannel, 1), make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		t.Cleanup(finish)
		h.multiClient.beforeServiceRefreshBindForTest = func(current *multiClientChannelUpdate, client *multiClientChannel) {
			if reraceSameFlow(current.ipPath, body) && current.receivedInbound.Load() {
				select {
				case entered <- client:
				default:
				}
				<-release
			}
		}
		h.concurrent = true
		bodyDone := make(chan bool, 1)
		go func() { bodyDone <- privateCrossWindowNativeSyn(h, body) }()
		synctest.Wait()
		var victim *multiClientChannel
		select {
		case victim = <-entered:
		default:
			t.Fatal("actual body winner did not reach its bookkeeping gate")
		}
		if h.multiClient.clientFlowCount(victim) != 0 || victim.serviceRefreshReservation.Load() != nil {
			t.Fatal("control did not expose the actual winner-to-bookkeeping interval")
		}
		if victim.serviceRefreshRaceReferences.Load() == 0 {
			t.Fatal("SERVICE_REFRESH_WINNER_TOKEN_LOST_BEFORE_BOOKKEEPING: a real healthy winner has no pending ownership")
		}
		claimed, _ := h.multiClient.claimServiceRefreshOwner(update, path, victim, h.liveChannels())
		if claimed != nil {
			victim.releaseServiceRefreshReservation(claimed)
			update.stateLock.Lock()
			update.serviceRefreshRequested = false
			update.releaseSynRetryReferenceWithLock(claimed)
			update.stateLock.Unlock()
			update.releaseSynRetryMemory()
			t.Fatal("SERVICE_REFRESH_WINNER_BOOKKEEPING_GAP_WAS_IDLE: a real healthy body was reserved for teardown")
		}
		finish()
		synctest.Wait()
		if !<-bodyDone || h.boundTo(body) != victim || victim.IsDone() || h.multiClient.clientFlowCount(victim) != 1 || victim.serviceRefreshRaceReferences.Load() != 0 ||
			!h.send(reraceSegment(body, 0, []byte("same original body after idle refusal"))) {
			t.Fatal("refused idle reservation damaged the original winner or reference transfer")
		}
	})
}

// A source already waiting at selection must recheck the idle fence inside
// actual race registration. Unadmitted bytes remain available to a later retry.
func TestFullServiceRefreshFenceRejectsConcurrentFreshRegistration(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(53008)
		specs := make([]reraceExitSpec, 2)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h := newReraceHarness(t, specs, nil)
		privateCrossWindowNativeSyn(h, path)
		update, victim := h.update(path), h.channel("a")
		offered := h.liveChannels()
		body := unboundFlowTcpPath(53009)
		body.DestinationIp[3] = 101
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		var claimed *tcpSynRetry
		var releaseOnce sync.Once
		releaseReservation := func() {
			releaseOnce.Do(func() {
				victim.preemptServiceRefresh()
				if claimed != nil {
					update.stateLock.Lock()
					update.serviceRefreshRequested = false
					update.releaseSynRetryReferenceWithLock(claimed)
					update.stateLock.Unlock()
					update.releaseSynRetryMemory()
				}
			})
		}
		t.Cleanup(func() { finish(); releaseReservation() })
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if reraceSameFlow(current.ipPath, body) && !group.isSynRetry() {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
			}
		}
		h.multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
			if reraceSameFlow(group.ipPath, body) {
				return []*multiClientChannel{victim}
			}
			return offered
		}
		h.concurrent = true
		bodyDone := make(chan bool, 1)
		go func() { bodyDone <- privateCrossWindowNativeSyn(h, body) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("fresh native source did not reach its pre-registration gate")
		}
		claimed, _ = h.multiClient.claimServiceRefreshOwner(update, path, victim, offered)
		if claimed == nil {
			t.Fatal("exact idle victim lacked its actual paid source/refusal reservation")
		}
		finish()
		if <-bodyDone || len(h.exit("a").syns(body)) != 0 || victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("SERVICE_REFRESH_FENCED_REGISTRATION_WAS_ADMITTED: a preselected source entered the reserved idle channel")
		}
		releaseReservation()
		h.multiClient.beforePacketGroupGateForTest = nil
		if !privateCrossWindowNativeSyn(h, body) {
			t.Fatal("unadmitted native source could not retry after the optional fence ended")
		}
		synctest.Wait()
		if _, answered := h.synAck(body); !answered || h.boundTo(body) != victim || victim.IsDone() || victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("fence refusal consumed source recovery, delivery or candidate ownership")
		}
	})
}

// Even a stale discovery answer can create a fresh local identity to an old
// remote. Evaluation success does not erase that source's remote-tail refusal.
func TestFullServiceRefreshRefusesFreshLocalAliasOfFailedRemote(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, generator, window, path, paths, owners := newFullServiceRefreshFixture(t, 53010)
		original := window.unorderedClients()
		var aliasRemote *multiClientChannel
		for _, candidate := range original {
			if h.multiClient.clientFlowCount(candidate) == 0 {
				aliasRemote = candidate
			}
		}
		if aliasRemote == nil {
			t.Fatal("alias control has no actual idle victim")
		}
		generator.mutex.Lock()
		generator.holdNextAvailable = true
		generator.resultOverrideForTest = map[MultiHopId]DestinationStats{
			aliasRemote.Destination(): {IpFamily: aliasRemote.IpFamily()},
		}
		generator.mutex.Unlock()
		_, _, beforeMints, beforeRetired := generator.counts()
		evaluated := discoveryDemandTestObservePublication(window)
		start := time.Now()
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("alias control's original actual source was not admitted")
		}
		if !discoveryDemandTestAwaitPublication(t, generator.availableEntered, start.Add(window.settings.WindowExpandTimeout)) {
			t.Fatal("alias listing did not reach its actual availability barrier")
		}
		update := h.update(path)
		update.stateLock.Lock()
		known := update.synRetryRemoteTailRefusedWithLock(aliasRemote)
		update.stateLock.Unlock()
		if !known {
			t.Fatal("alias control lacks actual refused remote provenance before evaluation")
		}
		finished := make(chan struct{}, 1)
		window.beforeDiscoveryRetryPublishForTest = func(owner *tcpSynRetry) {
			if owner != nil {
				select {
				case finished <- struct{}{}:
				default:
				}
			}
		}
		close(generator.availableRelease)
		if !discoveryDemandTestAwaitPublication(t, evaluated, start.Add(window.settings.WindowExpandTimeout)) {
			t.Fatal("actual alias candidate did not finish its normal evaluation")
		}
		if !discoveryDemandTestAwaitPublication(t, finished, start.Add(window.settings.WindowExpandTimeout+time.Second)) {
			t.Fatal("alias rejection did not finish its actual bounded pass")
		}
		h.settle()
		_, _, mints, retired := generator.counts()
		if mints != beforeMints+1 || retired != beforeRetired+1 || len(window.unorderedClients()) != 5 {
			t.Fatal("failed-remote alias was not returned through the existing bounded candidate owner")
		}
		for _, client := range original {
			window.stateLock.Lock()
			installed := window.clients[client.ClientId()] == client
			window.stateLock.Unlock()
			if !installed || client.IsDone() || client.isWarning() {
				t.Fatal("SERVICE_REFRESH_READMITTED_A_REFUSED_REMOTE_TAIL: a new local identity replaced a healthy original owner")
			}
		}
		if _, answered := h.synAck(path); answered {
			t.Fatal("refused same-remote alias fabricated destination proof")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
	})
}

// These cells hold four actual old contenders and four established bodies.
// Only the fifth, idle provider supplies a real target-service refusal.
type earlyIdleRefusalFixture struct {
	h              *reraceHarness
	generator      *discoveryDemandTestGenerator
	window         *multiClientWindow
	path           *IpPath
	siblingPaths   []*IpPath
	siblingOwners  []*multiClientChannel
	victim         *multiClientChannel
	ordinaryVictim *multiClientChannel
	update         *multiClientChannelUpdate
	owner          *tcpSynRetry
	stamp          uint64
	serial         uint64
	refuseIdle     atomic.Bool
	nativePacket   func() []byte
	started        time.Time
	attemptStarted time.Time
	reads          int
	empty          int
	mints          int
}

// Provider availability and answers use the real harness; no failure, race,
// source-age, reservation or generator notification is assigned by the test.
func newEarlyIdleRefusalFixture(t *testing.T, port int, holdEmpty bool) *earlyIdleRefusalFixture {
	t.Helper()
	return newEarlyIdleRefusalFixtureWithBodyCount(t, port, holdEmpty, 4)
}

// A three-body interleaving reserves a separate idle lifetime victim for an
// ordinary handover. Source generation and remote-refusal fields stay real.
func newEarlyIdleRefusalFixtureWithBodyCount(t *testing.T, port int, holdEmpty bool, bodyCount int, configureClients ...func(*ClientSettings)) *earlyIdleRefusalFixture {
	t.Helper()
	f := &earlyIdleRefusalFixture{path: unboundFlowTcpPath(port)}
	f.refuseIdle.Store(true)
	var idle atomic.Int32
	idle.Store(-1)
	specs := make([]reraceExitSpec, 5)
	for index := range specs {
		var configure func(*ClientSettings)
		if len(configureClients) != 0 {
			configure = configureClients[0]
		}
		specs[index] = reraceExitSpec{name: string(rune('a' + index)), clientSettings: configure, answer: func(path *IpPath) reraceAnswer {
			if path.DestinationIp.Equal(f.path.DestinationIp) && path.DestinationPort == f.path.DestinationPort {
				if idle.Load() == int32(index) && f.refuseIdle.Load() {
					return reraceSignal
				}
				return reraceSilent
			}
			return reraceAccept
		}}
	}
	f.h, f.generator = newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) { settings.StrictWindowHandover = true })
	if bodyCount == 4 {
		f.window, f.siblingPaths, f.siblingOwners = discoveryDemandTestCarryFour(t, f.h)
	} else if bodyCount == 3 {
		f.window = f.h.multiClient.windows[WindowTypeQuality]
		for index := range bodyCount {
			path, owner := bindStrictHandoverTestFlow(t, f.h, index)
			for _, previous := range f.siblingOwners {
				if previous == owner {
					t.Fatal("three-body control reused its original owner")
				}
			}
			f.siblingPaths = append(f.siblingPaths, path)
			f.siblingOwners = append(f.siblingOwners, owner)
		}
		payload := []byte("held original CSS sibling")
		for bucket := range 4 {
			if bucket != 0 {
				f.h.sleep(f.window.settings.StatsWindowBucketDuration + time.Nanosecond)
			}
			for _, path := range f.siblingPaths {
				if !f.h.send(reraceSegment(path, uint32(bucket*len(payload)), payload)) {
					t.Fatal("three-body owner stopped real progress")
				}
			}
		}
	} else {
		t.Fatal("unsupported early-refusal body-count control")
	}
	for _, client := range f.window.unorderedClients() {
		busy := false
		for _, owner := range f.siblingOwners {
			busy = busy || client == owner
		}
		if !busy {
			if f.victim == nil {
				f.victim = client
			} else if bodyCount == 3 && f.ordinaryVictim == nil {
				f.ordinaryVictim = client
			} else {
				t.Fatal("early idle control has extra idle slots")
			}
		}
	}
	if f.victim == nil || len(f.window.unorderedClients()) != 5 || f.h.multiClient.clientFlowCount(f.victim) != 0 {
		t.Fatal("early idle control lacks one genuine idle slot at the hard maximum")
	}
	for index := range specs {
		if f.victim.Destination() == f.h.exit(specs[index].name).destination {
			idle.Store(int32(index))
		}
	}
	if idle.Load() < 0 {
		t.Fatal("idle slot is not backed by one real provider")
	}
	reliability := *f.h.multiClient.ReliabilitySettings()
	reliability.MaxFlowsPerExit = DefaultMultiClientSettings().MaxFlowsPerExit
	f.h.multiClient.SetReliabilitySettings(&reliability)
	f.generator.mutex.Lock()
	f.generator.holdNextEmpty = holdEmpty
	f.generator.mutex.Unlock()
	f.reads, f.empty, f.mints, _ = f.generator.counts()
	return f
}

// This is the native source and the standing remote refusal, with all other
// original contenders still unproven. The source remains the same paid owner.
func (self *earlyIdleRefusalFixture) start(t *testing.T) {
	t.Helper()
	self.started = time.Now()
	accepted := false
	if self.nativePacket == nil {
		accepted = privateCrossWindowNativeSyn(self.h, self.path)
	} else {
		accepted = synRetryControlOffer(t, self.h, MessagePoolCopy(self.nativePacket()))
	}
	if !accepted {
		t.Fatal("early idle original native SYN was not admitted")
	}
	self.update = self.h.update(self.path)
	if self.update == nil {
		t.Fatal("early idle source has no canonical update")
	}
	self.owner = discoveryDemandServiceRefreshOwner(t, self.window, self.update)
	self.update.stateLock.Lock()
	refusals := len(self.update.dialFailedClients)
	valid := self.update.client.Load() == nil && !self.update.receivedInbound.Load() && self.update.race != nil &&
		refusals == 1 && self.update.synRetryRemoteTailRefusedWithLock(self.victim)
	for _, old := range self.siblingOwners {
		valid = valid && !self.update.synRetryRemoteTailRefusedWithLock(old)
	}
	self.stamp, self.serial = self.update.discoveryAttemptStamp.Load(), self.owner.serial
	self.attemptStarted = self.update.discoveryAttemptStarted
	self.update.stateLock.Unlock()
	if !valid || self.h.metrics().DialFailuresIntercepted != 1 || self.victim.serviceRefreshRaceReferences.Load() != 0 {
		t.Fatalf("early idle premise lacks one idle actual refusal plus a live unexhausted race: valid=%t failures=%d victim_refs=%d", valid, refusals, self.victim.serviceRefreshRaceReferences.Load())
	}
	for _, old := range self.siblingOwners {
		if self.h.multiClient.clientFlowCount(old) != 1 || old.serviceRefreshRaceReferences.Load() == 0 {
			t.Fatal("early idle control did not retain its actual busy old contender")
		}
	}
}

// An unchanged native reoffer reasserts existing work; no test timer wake is sent.
func (self *earlyIdleRefusalFixture) offerAt(t *testing.T, offset time.Duration) {
	t.Helper()
	self.h.sleep(self.started.Add(offset).Sub(time.Now()))
	beforeCollapse := self.h.multiClient.TcpCollapseDropCount()
	accepted := privateCrossWindowNativeSyn(self.h, self.path)
	collapseDelta := self.h.multiClient.TcpCollapseDropCount() - beforeCollapse
	self.update.stateLock.Lock()
	inbound := self.update.receivedInbound.Load()
	original := self.update.synRetry == self.owner && self.owner.packet != nil && self.owner.credit != nil &&
		!self.owner.credit.released.Load() && self.owner.serial == self.serial &&
		self.update.discoveryAttemptStamp.Load() == self.stamp && self.update.discoveryAttemptStarted.Equal(self.attemptStarted) &&
		self.update.dialFailedSyn == self.path.SequenceNumber
	self.update.stateLock.Unlock()
	if !accepted && collapseDelta != 1 {
		t.Fatalf("native reoffer did not reach the actual post-observation collapse guard: collapse_delta=%d", collapseDelta)
	}
	if !inbound && (accepted || !original || self.h.update(self.path) != self.update) {
		t.Fatalf("same-SYN collapse lost its original paid source: accepted=%t retained=%t", accepted, original)
	}
	t.Logf("early native reoffer: age=%s accepted=%t collapse_delta=%d retained=%t inbound=%t", time.Since(self.started), accepted, collapseDelta, original, inbound)
}

// Only actual reads/mints/admission qualify. Bodies, strict capacity and the
// original update remain independently checked after the target observation.
func (self *earlyIdleRefusalFixture) requireLate(t *testing.T, publication <-chan struct{}, deadline time.Time, marker string) {
	t.Helper()
	published := discoveryDemandTestAwaitPublication(t, publication, deadline)
	self.h.settle()
	reads, empty, mints, _ := self.generator.counts()
	installed := len(self.window.unorderedClients())
	if !published || reads == self.reads {
		t.Fatalf("%s: published=%t reads=%d empty=%d mints=%d installed=%d age=%s", marker, published, reads-self.reads, empty-self.empty, mints-self.mints, installed, time.Since(self.started))
	}
	if reads > self.reads+3 || mints != self.mints+1 || installed != 5 || self.window.strictWindowAdmissionAllowed(NewId(), self.window.settings.WindowSizes[WindowTypeQuality]) {
		t.Fatal("early idle refresh escaped the existing window/read/mint ceiling")
	}
	if self.h.update(self.path) != self.update || self.owner.serial != self.serial {
		t.Fatal("early refresh replaced the original source update or paid owner generation")
	}
	late := self.h.channel("early-late")
	if late.IsDone() || late.isWarning() {
		t.Fatal("early listing did not publish an actual healthy provider")
	}
	if len(self.h.exit("early-late").syns(self.path)) != 1 {
		t.Fatalf("EARLY_IDLE_REFUSAL_MISSING_TARGET_SYN: actual=%d", len(self.h.exit("early-late").syns(self.path)))
	}
	if reply, answered := self.h.synAck(self.path); !answered || self.h.boundTo(self.path) != late {
		t.Fatal("EARLY_IDLE_REFUSAL_MISSING_TARGET_SYN_ACK")
	} else if parsed, err := ParseIpPath(reply.packet); err != nil || parsed.AckSequenceNumber != self.path.SequenceNumber+1 {
		t.Fatal("early target answer does not acknowledge the original retained SYN")
	}
	for _, syn := range self.h.exit("early-late").syns(self.path) {
		if syn.path.SequenceNumber != self.path.SequenceNumber {
			t.Fatal("early publication changed the retained original SYN")
		}
	}
	discoveryDemandFullWindowAssertSiblings(t, self.h, self.siblingPaths, self.siblingOwners)
	reraceAssertNoErrorDelivered(t, self.h, self.path)
}

// Holds the reaper's publication visit to this source until release. The visit
// only tries the flow lock and defers a contended visit by
// SynRetryPublicationRetryDelay, and the pass that publishes the provider then
// takes that lock in finishServiceRefresh. Settle while the visit waits.
func (self *earlyIdleRefusalFixture) holdPublicationVisit(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseVisit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseVisit)
	self.h.multiClient.beforeSynPublicationQueueForTest = func(current *multiClientChannelUpdate) {
		if current == self.update {
			enteredOnce.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-self.h.multiClient.ctx.Done():
				}
			})
		}
	}
	return entered, releaseVisit
}

// The production OLD guard cannot use the one actual-refused idle tail while
// four old contenders remain silent. Availability0.733 and reoffer1.109 precede
// the unchanged3s age gate and complete field exhaustion.
func TestEarlyIdleServiceRefusalListsBeforeUnfinishedContenders(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53120, false)
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		f.h.sleep(f.started.Add(733 * time.Millisecond).Sub(time.Now()))
		// A listing inside the reoffer can publish the provider there: that
		// retires the refused idle victim, which renews the attempt stamp, while
		// the paid SYN's visit can still wait behind finishServiceRefresh. The
		// reoffer then sees an unanswered source it cannot call retained. Hold
		// the listing until the reoffer completes, and the visit until it settles.
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		f.h.addExit(reraceExitSpec{name: "early-late"})
		f.offerAt(t, 1109*time.Millisecond)
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2*time.Second)) {
			t.Fatal("EARLY_IDLE_REFUSAL_DID_NOT_RELIST: the available provider was never listed")
		}
		visit, releaseVisit := f.holdPublicationVisit(t)
		close(f.generator.availableRelease)
		if !discoveryDemandTestAwaitPublication(t, visit, f.started.Add(2*time.Second)) {
			t.Fatal("published provider never reached the paid source's publication visit")
		}
		f.h.settle()
		releaseVisit()
		f.requireLate(t, publication, f.started.Add(2*time.Second), "EARLY_IDLE_REFUSAL_DID_NOT_RELIST")
	})
}

// An early empty response owns no identity and must leave the same pass able
// to read again after ordinary native demand. The existing one-second listing
// cadence and three-read budget remain; no5--10s expansion retry is bypassed.
func TestEarlyIdleServiceRefusalReassertsAfterEmptyListing(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53121, true)
		publication := discoveryDemandTestObservePublication(f.window)
		f.start(t)
		f.offerAt(t, 500*time.Millisecond)
		select {
		case <-f.generator.emptyEntered:
		default:
			t.Fatal("EARLY_IDLE_REFUSAL_FIRST_EMPTY_NOT_REACHED")
		}
		f.h.sleep(f.started.Add(733 * time.Millisecond).Sub(time.Now()))
		f.h.addExit(reraceExitSpec{name: "early-late"})
		close(f.generator.emptyRelease)
		f.h.settle()
		f.offerAt(t, 1109*time.Millisecond)
		f.requireLate(t, publication, f.started.Add(2500*time.Millisecond), "EARLY_IDLE_REFUSAL_EMPTY_REASSERT_STALLED")
		_, empty, _, _ := f.generator.counts()
		if empty != f.empty+1 {
			t.Fatal("early-empty control did not preserve its one actual empty listing")
		}
	})
}

// Duplicate source offers and distinct rapid navigations share the window's
// one acquisition authority; an unavailable provider set mints no new identity.
func TestEarlyIdleServiceRefusalBurstKeepsOneWindowBudget(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53122, false)
		f.start(t)
		f.offerAt(t, 500*time.Millisecond)
		for index := range 32 {
			privateCrossWindowNativeSyn(f.h, f.path)
			if index < 8 {
				page := *f.path
				page.SourcePort += 100 + index
				page.SequenceNumber += uint32(100 + index)
				privateCrossWindowNativeSyn(f.h, &page)
			}
		}
		f.h.sleep(f.started.Add(2900 * time.Millisecond).Sub(time.Now()))
		reads, _, mints, _ := f.generator.counts()
		if reads == f.reads {
			t.Fatal("EARLY_IDLE_REFUSAL_BURST_NEVER_ACQUIRED")
		}
		if reads > f.reads+1+min(f.window.settings.DiscoveryDemandListingsPerPass, 3) || mints != f.mints || len(f.window.unorderedClients()) != 5 {
			t.Fatalf("early native/page burst exceeded one window's existing budget: reads=%d mints=%d", reads-f.reads, mints-f.mints)
		}
		f.window.stateLock.Lock()
		owner := f.window.serviceRefreshOwner
		f.window.stateLock.Unlock()
		if owner != nil && owner != f.owner {
			t.Fatal("rapid pages replaced the original live refresh owner")
		}
		// Continue actual same-source pressure through acquisition expiry.
		for offset := 4 * time.Second; offset < f.window.settings.WindowExpandTimeout; offset += time.Second {
			f.offerAt(t, offset)
		}
		f.h.sleep(f.started.Add(f.window.settings.WindowExpandTimeout + time.Second).Sub(time.Now()))
		readsAtExpiry, _, mintsAtExpiry, _ := f.generator.counts()
		f.window.stateLock.Lock()
		deferred := time.Now().Before(f.window.discoveryRetry.notBefore)
		deadline := f.window.discoveryRetry.notBefore
		failures := f.window.discoveryRetry.failureCount
		owner = f.window.serviceRefreshOwner
		f.window.stateLock.Unlock()
		if readsAtExpiry > f.reads+1+min(f.window.settings.DiscoveryDemandListingsPerPass, 3) || mintsAtExpiry != f.mints || owner != nil || failures != 1 || !deferred {
			t.Fatalf("early outage exceeded one full acquisition: reads=%d mints=%d owner=%t failures=%d", readsAtExpiry-f.reads, mintsAtExpiry-f.mints, owner != nil, failures)
		}
		for range 16 {
			privateCrossWindowNativeSyn(f.h, f.path)
		}
		f.h.sleep(min(time.Second, time.Until(deadline)/2))
		readsAfter, _, mintsAfter, _ := f.generator.counts()
		f.window.stateLock.Lock()
		unchanged := f.window.discoveryRetry.failureCount == failures && f.window.discoveryRetry.notBefore.Equal(deadline)
		f.window.stateLock.Unlock()
		if !unchanged || readsAfter != readsAtExpiry || mintsAfter != mintsAtExpiry {
			t.Fatal("EARLY_IDLE_REFUSAL_OUTAGE_BYPASSED_BACKOFF")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// A real reply to one held original SYN wins while the available late listing
// is blocked. The fresh speculative identity must never be minted afterward.
func TestEarlyIdleServiceRefusalOldRaceAnswerCancelsSpeculation(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53123, false)
		f.start(t)
		credit := f.owner.credit
		f.generator.mutex.Lock()
		f.generator.holdNextAvailable = true
		f.generator.mutex.Unlock()
		f.h.sleep(f.started.Add(733 * time.Millisecond).Sub(time.Now()))
		f.h.addExit(reraceExitSpec{name: "early-late"})
		f.offerAt(t, 1109*time.Millisecond)
		if !discoveryDemandTestAwaitPublication(t, f.generator.availableEntered, f.started.Add(2500*time.Millisecond)) {
			t.Fatal("EARLY_IDLE_REFUSAL_OLD_ANSWER_DID_NOT_REACH_LISTING")
		}
		f.answerOld(t)
		close(f.generator.availableRelease)
		f.h.settle()
		_, _, mints, _ := f.generator.counts()
		f.window.stateLock.Lock()
		owned, failures := f.window.serviceRefreshOwner, f.window.discoveryRetry.failureCount
		f.window.stateLock.Unlock()
		if mints != f.mints || len(f.h.exit("early-late").syns(f.path)) != 0 || owned != nil || f.owner.packet != nil || !credit.released.Load() || failures != 0 {
			t.Fatalf("old answer failed to retire unneeded early speculation: mints=%d owner=%t retained=%t failures=%d", mints-f.mints, owned != nil, f.owner.packet != nil, failures)
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// Actual native source cancellation withdraws the borrowed window reference
// before a later provider may be consumed. The failed body/update is not reused.
func TestEarlyIdleServiceRefusalCanceledSourceCannotSpendLateListing(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53124, true)
		f.start(t)
		f.offerAt(t, 500*time.Millisecond)
		select {
		case <-f.generator.emptyEntered:
		default:
			t.Fatal("EARLY_IDLE_REFUSAL_CANCEL_NEVER_ACQUIRED")
		}
		credit := f.owner.credit
		f.update.Close()
		f.h.settle()
		f.h.addExit(reraceExitSpec{name: "early-late"})
		close(f.generator.emptyRelease)
		f.h.settle()
		_, _, mints, _ := f.generator.counts()
		if mints != f.mints || len(f.h.exit("early-late").syns(f.path)) != 0 || f.owner.packet != nil || !credit.released.Load() {
			t.Fatal("canceled early refresh spent a later listing or retained its paid owner")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// The same tuple's real new SYN changes its stamp and remote-refusal ledger.
// Completing the old empty response cannot lend the new source early authority.
func TestEarlyIdleServiceRefusalNewSynCannotBorrowOldReservation(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53125, true)
		f.start(t)
		f.offerAt(t, 500*time.Millisecond)
		select {
		case <-f.generator.emptyEntered:
		default:
			t.Fatal("EARLY_IDLE_REFUSAL_NEW_SYN_NEVER_ACQUIRED")
		}
		f.refuseIdle.Store(false)
		changed := *f.path
		changed.SequenceNumber++
		if !privateCrossWindowNativeSyn(f.h, &changed) {
			t.Fatal("changed native SYN was not admitted")
		}
		current := f.h.update(&changed)
		current.stateLock.Lock()
		fresh := current.discoveryAttemptStamp.Load() != f.stamp && current.synRetry != f.owner &&
			!current.synRetryHasRemoteRefusalWithLock()
		current.stateLock.Unlock()
		if !fresh {
			t.Fatal("new native generation inherited old early-refusal history")
		}
		f.h.addExit(reraceExitSpec{name: "early-late"})
		close(f.generator.emptyRelease)
		f.h.settle()
		_, _, mints, _ := f.generator.counts()
		if mints != f.mints || len(f.h.exit("early-late").syns(&changed)) != 0 {
			t.Fatal("old listing crossed a changed native source generation")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// Answers the real already-recorded original SYN from an old provider through
// its actual transfer channel; no race winner or ownership flag is assigned.
func (self *earlyIdleRefusalFixture) answerOld(t *testing.T) {
	t.Helper()
	old := self.siblingOwners[0]
	var provider *reraceExit
	for _, exit := range self.h.generator.exitList() {
		if exit.destination == old.Destination() {
			provider = exit
		}
	}
	if provider == nil {
		t.Fatal("held old contender has no actual provider")
	}
	syns := provider.syns(self.path)
	if len(syns) == 0 {
		t.Fatal("old race answer has no real original SYN")
	}
	provider.sendTo(syns[0].source, reraceSynAck(syns[0].path))
	self.h.settle()
	if _, answered := self.h.synAck(self.path); !answered || self.h.boundTo(self.path) != old {
		t.Fatal("actual old race answer did not establish the unchanged original navigation")
	}
}

// The evaluated alternative is a real minted client whose actual ping result
// is held. An old contender's useful answer must cancel it before admission.
func TestEarlyIdleServiceRefusalOldAnswerCancelsPendingCandidate(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53126, false)
		f.start(t)
		credit := f.owner.credit
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(finish)
		f.window.beforeExpandPingResultForTest = func() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
		_, _, _, beforeRetired := f.generator.counts()
		f.h.sleep(f.started.Add(733 * time.Millisecond).Sub(time.Now()))
		f.h.addExit(reraceExitSpec{name: "early-late"})
		f.offerAt(t, 1109*time.Millisecond)
		if !discoveryDemandTestAwaitPublication(t, entered, f.started.Add(2500*time.Millisecond)) {
			t.Fatal("EARLY_IDLE_REFUSAL_OLD_ANSWER_NEVER_REACHED_ACTUAL_PING")
		}
		_, _, mints, _ := f.generator.counts()
		if mints != f.mints+1 || len(f.window.unorderedClients()) != 5 {
			t.Fatal("pending-ping control lacks one actual bounded speculative identity")
		}
		f.answerOld(t)
		finish()
		f.h.settle()
		_, _, afterMints, retired := f.generator.counts()
		f.window.stateLock.Lock()
		owned, failures := f.window.serviceRefreshOwner, f.window.discoveryRetry.failureCount
		f.window.stateLock.Unlock()
		if afterMints != mints || retired != beforeRetired+1 || len(f.window.unorderedClients()) != 5 || owned != nil || failures != 0 || !credit.released.Load() {
			t.Fatalf("late old answer did not cancel/join the pending candidate: mints=%d retired=%d owner=%t failures=%d", afterMints-f.mints, retired-beforeRetired, owned != nil, failures)
		}
		for _, client := range f.window.unorderedClients() {
			if client.Destination() == f.h.exit("early-late").destination {
				t.Fatal("canceled speculative candidate displaced an original owner")
			}
		}
		// The useful reply is one proof, not permission for every later body
		// packet to erase a future window's genuine failed-expansion backoff.
		f.window.noteDiscoveryExpandUnproductive(time.Now())
		f.window.stateLock.Lock()
		deadline, count := f.window.discoveryRetry.notBefore, f.window.discoveryRetry.failureCount
		f.window.stateLock.Unlock()
		if count != 1 || !time.Now().Before(deadline) || !f.h.send(reraceSegment(f.path, 0, []byte("old winner body"))) {
			t.Fatal("answer replay control lacks new deferral and real body progress")
		}
		f.window.stateLock.Lock()
		unchanged := f.window.discoveryRetry.failureCount == count && f.window.discoveryRetry.notBefore.Equal(deadline)
		f.window.stateLock.Unlock()
		if !unchanged {
			t.Fatal("EARLY_IDLE_REFUSAL_ANSWER_REPLAY_RESET_BACKOFF")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// An ordinary handover has already minted its own real candidate and holds
// the initial-ping result. A distinct pending early owner then gets its real
// old-race answer; that answer must not forgive the ordinary ping's failure.
func TestEarlyIdleServiceRefusalPendingAnswerCannotEraseOrdinaryPassBackoff(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixtureWithBodyCount(t, 53127, false, 3)
		if f.ordinaryVictim == nil || f.ordinaryVictim == f.victim || f.h.multiClient.clientFlowCount(f.ordinaryVictim) != 0 {
			t.Fatal("ordinary/early interleaving lacks two distinct real idle owners")
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(finish)
		f.window.beforeExpandPingResultForTest = func() { enteredOnce.Do(func() { close(entered) }); <-release }
		expirePing := make(chan struct{}, 1)
		f.window.expireExpandPingForTest = expirePing
		f.h.addExit(reraceExitSpec{name: "ordinary-spare"})
		ageWindowClientForTest(f.ordinaryVictim, time.Minute, 2*time.Minute)
		if !discoveryDemandTestAwaitPublication(t, entered, time.Now().Add(f.window.settings.WindowResizeTimeout+time.Second)) {
			t.Fatal("ordinary handover never reached its actual held ping")
		}
		f.h.settle()
		f.window.stateLock.Lock()
		ordinaryPass := f.window.strictPass
		ordinary := ordinaryPass != nil && ordinaryPass.handover != nil && ordinaryPass.handover.victim == f.ordinaryVictim && ordinaryPass.handover.serviceRefresh == nil
		noRefresh := f.window.serviceRefreshOwner == nil
		f.window.stateLock.Unlock()
		_, _, beforeMints, beforeRetired := f.generator.counts()
		if !ordinary || !noRefresh || beforeMints != f.mints+1 {
			t.Fatal("ordinary interleaving lacks one real owned non-refresh candidate")
		}
		f.start(t)
		f.offerAt(t, 500*time.Millisecond)
		f.window.stateLock.Lock()
		pending := f.window.serviceRefreshOwner == f.owner && !f.window.serviceRefreshAttempted && f.window.strictPass == ordinaryPass
		f.window.stateLock.Unlock()
		if !pending {
			t.Fatal("ordinary pass did not retain its distinct pending early owner")
		}
		f.answerOld(t)
		expirePing <- struct{}{}
		f.h.settle()
		finish()
		f.h.settle()
		f.window.stateLock.Lock()
		failures, deadline := f.window.discoveryRetry.failureCount, f.window.discoveryRetry.notBefore
		owned := f.window.serviceRefreshOwner
		f.window.stateLock.Unlock()
		_, _, afterMints, retired := f.generator.counts()
		if failures != 1 || !time.Now().Before(deadline) || owned != nil || afterMints != beforeMints || retired != beforeRetired+1 {
			t.Fatalf("EARLY_IDLE_REFUSAL_ORDINARY_BACKOFF_WAS_CLEARED: failures=%d deferred=%t pending=%t mints=%d retired=%d", failures, time.Now().Before(deadline), owned != nil, afterMints-beforeMints, retired-beforeRetired)
		}
		if !strictHandoverTestInstalled(f.window, f.ordinaryVictim) || f.ordinaryVictim.IsDone() || len(f.window.unorderedClients()) != 5 {
			t.Fatal("failed ordinary candidate displaced a retained original owner")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

// The old target reply arrives after this source's attempted acquisition has
// ended. It may clear that exact failure, but never a newer ordinary failure.
func testEarlyIdleServiceRefusalLateBackoffAnswer(t *testing.T, ordinaryFailure bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newEarlyIdleRefusalFixture(t, 53128, false)
		f.start(t)
		f.offerAt(t, 500*time.Millisecond)
		f.h.sleep(f.started.Add(f.window.settings.WindowExpandTimeout + time.Second).Sub(time.Now()))
		f.window.stateLock.Lock()
		failed := f.window.discoveryRetry.failureCount == 1 && time.Now().Before(f.window.discoveryRetry.notBefore) && f.window.serviceRefreshOwner == nil
		f.window.stateLock.Unlock()
		if !failed {
			t.Fatal("late-answer control lacks its actual completed failed refresh pass")
		}
		if ordinaryFailure {
			f.window.noteDiscoveryExpandUnproductive(time.Now())
		}
		f.window.stateLock.Lock()
		deadline, count := f.window.discoveryRetry.notBefore, f.window.discoveryRetry.failureCount
		f.window.stateLock.Unlock()
		f.answerOld(t)
		f.window.stateLock.Lock()
		remaining, afterDeadline := f.window.discoveryRetry.failureCount, f.window.discoveryRetry.notBefore
		f.window.stateLock.Unlock()
		if ordinaryFailure {
			if count != 2 || remaining != count || !afterDeadline.Equal(deadline) {
				t.Fatal("EARLY_IDLE_REFUSAL_LATE_ANSWER_CLEARED_ORDINARY_BACKOFF")
			}
		} else if remaining != 0 || !afterDeadline.IsZero() {
			t.Fatal("EARLY_IDLE_REFUSAL_LATE_ANSWER_LEFT_OWN_BACKOFF")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
	})
}

func TestEarlyIdleServiceRefusalLateAnswerClearsOnlyOwnBackoff(t *testing.T) {
	testEarlyIdleServiceRefusalLateBackoffAnswer(t, false)
}

func TestEarlyIdleServiceRefusalLateAnswerKeepsFutureOrdinaryBackoff(t *testing.T) {
	testEarlyIdleServiceRefusalLateBackoffAnswer(t, true)
}
