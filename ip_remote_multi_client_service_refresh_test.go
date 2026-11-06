// Full-window service recovery preserves exact source, slot and body ownership.
// Real native packets supply failure evidence; gates order competing admissions.
package connect

import (
	"context"
	"sync"
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
		aliasRemote := original[0]
		generator.mutex.Lock()
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
		if !discoveryDemandTestAwaitPublication(t, evaluated, start.Add(window.settings.WindowExpandTimeout)) {
			t.Fatal("actual alias candidate did not finish its normal evaluation")
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
