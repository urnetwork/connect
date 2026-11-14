// Inherited placement must remain visible to idle-slot recovery before its
// first physical send and the later parent bookkeeping handoff.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Both providers, refusal evidence, healthy bodies and the held source use
// real native admission. No placement or parent membership is injected.
type serviceRefreshAffinityTestFixture struct {
	h            *reraceHarness
	failedPath   *IpPath
	failedUpdate *multiClientChannelUpdate
	donorPath    *IpPath
	donorUpdate  *multiClientChannelUpdate
	siblingPath  *IpPath
	siblingOwner *multiClientChannel
	path         *IpPath
	update       *multiClientChannelUpdate
	victim       *multiClientChannel
	payload      []byte
	resume       func()
	sendDone     <-chan struct{}
	accepted     bool
}

// The caller transfers one actual pooled native packet and checks both the
// aggregate ownership result and its individual admission receipt.
func (self *serviceRefreshAffinityTestFixture) offer(packet []byte) bool {
	accepted := [1]bool{}
	count := self.h.multiClient.SendNativePacketBatchWithResults(self.h.source, protocol.ProvideMode_Network,
		[][]byte{MessagePoolCopy(packet)}, reraceSendTimeout, accepted[:])
	self.h.settle()
	return count == 1 && accepted[0]
}

// The original donor exits through a real native RST while its actual child
// is held after inheritance and before its first physical provider admission.
func newServiceRefreshAffinityTestFixture(t *testing.T) *serviceRefreshAffinityTestFixture {
	t.Helper()
	f := &serviceRefreshAffinityTestFixture{failedPath: reraceFlow(4, 89, 53100),
		donorPath: reraceFlow(4, 90, 53101), siblingPath: reraceFlow(4, 91, 53102),
		payload: []byte("healthy original affinity body")}
	specs := make([]reraceExitSpec, 2)
	for index := range specs {
		specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
			if reraceSameFlow(candidate, f.failedPath) {
				return reraceSignal
			}
			return reraceAccept
		}}
	}
	f.h = newReraceHarness(t, specs, func(settings *MultiClientSettings) {
		settings.DestinationAffinity, settings.AffinityStickyPastCap = true, true
		settings.MaxFlowsPerExit = 1
	})
	if !privateCrossWindowNativeSyn(f.h, f.failedPath) {
		t.Fatal("affinity control's original refused source was not admitted")
	}
	f.failedUpdate = f.h.update(f.failedPath)
	if f.failedUpdate == nil {
		t.Fatal("affinity control lost its original refused source")
	}
	f.failedUpdate.stateLock.Lock()
	refused := f.failedUpdate.synRetryHasRemoteRefusalWithLock() && len(f.failedUpdate.dialFailedClients) == 2 &&
		len(f.failedUpdate.dialFailureSignaledClients) == 2 && f.failedUpdate.client.Load() == nil && !f.failedUpdate.receivedInbound.Load()
	f.failedUpdate.stateLock.Unlock()
	if !refused {
		t.Fatal("affinity control lacks two actual standing remote refusals")
	}
	if !privateCrossWindowNativeSyn(f.h, f.donorPath) || !privateCrossWindowNativeSyn(f.h, f.siblingPath) {
		t.Fatal("affinity control's original healthy bodies were refused")
	}
	f.donorUpdate = f.h.update(f.donorPath)
	f.victim, f.siblingOwner = f.h.boundTo(f.donorPath), f.h.boundTo(f.siblingPath)
	if f.victim == nil || f.siblingOwner == nil || f.victim == f.siblingOwner {
		t.Fatal("affinity control did not create one real established body on each provider")
	}
	for bucket := range 4 {
		if bucket != 0 {
			time.Sleep(f.h.multiClient.settings.StatsWindowBucketDuration + time.Nanosecond)
		}
		for _, path := range []*IpPath{f.donorPath, f.siblingPath} {
			if !f.offer(reraceSegment(path, uint32(bucket*len(f.payload)), f.payload)) {
				t.Fatal("original body stopped delivering positive affinity health")
			}
		}
	}
	for _, owner := range []*multiClientChannel{f.victim, f.siblingOwner} {
		stats, err := owner.WindowStats()
		if err != nil || !stats.healthy || f.h.multiClient.clientFlowCount(owner) != 1 || owner.IsDone() || owner.isWarning() {
			t.Fatal("affinity control lacks two positive healthy single-flow owners")
		}
	}
	next := *f.donorPath
	next.SourcePort += 10
	next.SequenceNumber += 100
	f.path = &next
	entered, release, done := make(chan *multiClientChannelUpdate, 1), make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.resume = func() { once.Do(func() { close(release) }) }
	f.sendDone = done
	f.h.multiClient.beforePacketGroupGateForTest = func(update *multiClientChannelUpdate, selected *multiClientChannel, group *parsedPacketGroup) {
		if group.isSynRetry() || !reraceSameFlow(group.ipPath, f.path) || group.ipPath.SequenceNumber != f.path.SequenceNumber {
			return
		}
		if selected != f.victim {
			t.Error("new child did not actually inherit its original donor")
		}
		entered <- update
		<-release
	}
	f.h.concurrent = true
	go func() { defer close(done); f.accepted = privateCrossWindowNativeSyn(f.h, f.path) }()
	t.Cleanup(func() { f.resume(); <-done })
	synctest.Wait()
	select {
	case f.update = <-entered:
	default:
		t.Fatal("affinity control did not reach its actual inherited pre-send boundary")
	}
	if f.update.client.Load() != f.victim || f.update.client.loadSelection() != f.donorUpdate.client.loadSelection() ||
		f.update.receivedInbound.Load() || len(f.h.exit("a").syns(f.path))+len(f.h.exit("b").syns(f.path)) != 0 {
		t.Fatal("affinity control lacks real inherited selection before first provider send")
	}
	sequence := f.donorPath.SequenceNumber + 1 + uint32(4*len(f.payload))
	if !f.offer(reraceTcpPacket(f.donorPath, tcpFlagAck|tcpFlagRst, sequence, reraceServerIsn+1, nil)) {
		t.Fatal("original donor's native RST was refused")
	}
	synctest.Wait()
	if !f.donorUpdate.IsDone() || f.h.update(f.donorPath) != nil || f.h.multiClient.clientFlowCount(f.victim) != 0 ||
		f.h.boundTo(f.siblingPath) != f.siblingOwner || f.siblingOwner.IsDone() || f.victim.IsDone() {
		t.Fatal("affinity control did not retire the last donor flow while preserving its actual inherited child and healthy sibling")
	}
	return f
}

// Actual reservation success is the old defect. Merely observing an
// unpublished selected pointer does not establish that idle proof accepted it.
func TestFullServiceRefreshAffinityBeforeFirstSendIsNotIdle(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		claimed, _ := f.h.multiClient.claimServiceRefreshOwner(f.failedUpdate, f.failedPath, f.victim, f.h.liveChannels())
		acceptedIdleClaim := claimed != nil
		if claimed != nil {
			f.victim.releaseServiceRefreshReservation(claimed)
			f.failedUpdate.stateLock.Lock()
			f.failedUpdate.serviceRefreshRequested = false
			f.failedUpdate.releaseSynRetryReferenceWithLock(claimed)
			f.failedUpdate.stateLock.Unlock()
			f.failedUpdate.releaseSynRetryMemory()
		}
		f.resume()
		<-f.sendDone
		synctest.Wait()
		if acceptedIdleClaim {
			t.Fatal("SERVICE_REFRESH_AFFINITY_PRE_SEND_OWNER_WAS_IDLE: real inherited child survived last-donor RST but idle reservation accepted its provider before first send")
		}
		if !f.accepted || f.h.boundTo(f.path) != f.victim || f.victim.IsDone() || f.h.multiClient.clientFlowCount(f.victim) != 1 ||
			f.victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("affinity child did not hand its exact pending ownership into parent bookkeeping")
		}
		if _, answered := f.h.synAck(f.path); !answered || !f.offer(reraceSegment(f.path, 0, f.payload)) ||
			!f.offer(reraceSegment(f.siblingPath, uint32(4*len(f.payload)), f.payload)) || f.h.boundTo(f.siblingPath) != f.siblingOwner {
			t.Fatal("idle refusal disturbed the real inherited child or original healthy sibling")
		}
	})
}

// Every positive case first proves the exact new initial-placement claim.
func (self *serviceRefreshAffinityTestFixture) requirePending(t *testing.T) {
	t.Helper()
	self.update.stateLock.Lock()
	pending := self.update.pendingServiceRefreshBinding == self.victim && self.update.client.Load() == self.victim
	self.update.stateLock.Unlock()
	if !pending || self.victim.serviceRefreshRaceReferences.Load() != 1 || self.h.multiClient.clientFlowCount(self.victim) != 0 {
		t.Fatal("SERVICE_REFRESH_AFFINITY_PENDING_CLAIM_MISSING: inherited child lacks its one exact pre-send claim")
	}
}

// Later send and receive bookkeeping can each arrive; both wait on the same
// event and only actual map publication may return the current pending claim.
func (self *serviceRefreshAffinityTestFixture) holdBinding(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	self.h.multiClient.beforeServiceRefreshBindForTest = func(update *multiClientChannelUpdate, client *multiClientChannel) {
		if update == self.update {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
	}
	t.Cleanup(resume)
	return entered, resume
}

// A transport error really clears the selected flow; the former donor must
// become idle without retaining the failed placement's pending reference.
func TestFullServiceRefreshAffinityFailedSendReturnsPendingClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		attempts := 0
		f.victim.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			attempts++
			return false, errors.New("synthetic selected-channel transport error")
		}
		f.resume()
		<-f.sendDone
		synctest.Wait()
		f.victim.sendGroupForTest = nil
		f.update.stateLock.Lock()
		cleared := f.update.client.Load() == nil && f.update.pendingServiceRefreshBinding == nil && f.update.sourceReset.Load()
		f.update.stateLock.Unlock()
		if f.accepted || attempts != 1 || !cleared || f.victim.serviceRefreshRaceReferences.Load() != 0 ||
			f.h.multiClient.clientFlowCount(f.victim) != 0 || f.victim.IsDone() || f.h.boundTo(f.siblingPath) != f.siblingOwner {
			t.Fatal("SERVICE_REFRESH_AFFINITY_FAILED_SEND_KEPT_CLAIM: rejected transport retained or transferred its initial selected claim")
		}
	})
}

// Local refusal keeps the same selected flow for ordinary native retry. The
// existing parent map becomes its owner even though no physical SYN left yet.
func TestFullServiceRefreshAffinityLocalRefusalPublishesCurrentClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		attempts := 0
		f.victim.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			attempts++
			return false, nil
		}
		f.resume()
		<-f.sendDone
		synctest.Wait()
		f.victim.sendGroupForTest = nil
		if f.accepted || attempts != 1 || f.update.client.Load() != f.victim || f.victim.serviceRefreshRaceReferences.Load() != 0 ||
			f.h.multiClient.clientFlowCount(f.victim) != 1 || f.update.pendingServiceRefreshBinding != nil || f.victim.IsDone() ||
			len(f.h.exit("a").syns(f.path))+len(f.h.exit("b").syns(f.path)) != 0 {
			t.Fatal("SERVICE_REFRESH_AFFINITY_LOCAL_REFUSAL_LOST_OWNER: local refusal did not transfer its still-selected flow into parent bookkeeping")
		}
		if !privateCrossWindowNativeSyn(f.h, f.path) {
			t.Fatal("native retry after local refusal was not admitted")
		}
		synctest.Wait()
		if _, answered := f.h.synAck(f.path); !answered || f.h.boundTo(f.path) != f.victim || f.victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("local admission recovery changed the inherited flow or kept a duplicate claim")
		}
	})
}

// A bound-source refusal can arrive before the sender books parent membership.
// Actual standing evidence unbinds that flow and returns its initial claim.
func TestFullServiceRefreshAffinityRefusalBeforeBookkeepingReturnsClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		entered, release := f.holdBinding(t)
		retryEntered, retryRelease := make(chan struct{}), make(chan struct{})
		var retryEnteredOnce, retryReleaseOnce sync.Once
		resumeRetry := func() { retryReleaseOnce.Do(func() { close(retryRelease) }) }
		t.Cleanup(resumeRetry)
		f.h.multiClient.beforeSynRetryDispatchForTest = func(update *multiClientChannelUpdate) {
			if update != f.update {
				return
			}
			retryEnteredOnce.Do(func() { close(retryEntered) })
			select {
			case <-retryRelease:
			case <-f.h.multiClient.ctx.Done():
			}
		}
		for _, exit := range f.h.generator.exitList() {
			exit.answer = func(path *IpPath) reraceAnswer {
				if reraceSameFlow(path, f.path) {
					return reraceSignal
				}
				return reraceAccept
			}
		}
		f.resume()
		synctest.Wait()
		select {
		case <-entered:
		default:
		}
		select {
		case <-retryEntered:
		default:
			t.Fatal("actual first refusal did not reach its retained retry boundary")
		}
		f.update.stateLock.Lock()
		refused := f.update.client.Load() == nil && f.update.pendingServiceRefreshBinding == nil &&
			len(f.update.dialFailedClients) == 1 && len(f.update.dialFailureSignaledClients) == 1 && f.update.synRetryHasRemoteRefusalWithLock()
		f.update.stateLock.Unlock()
		release()
		resumeRetry()
		<-f.sendDone
		synctest.Wait()
		if !refused || f.victim.serviceRefreshRaceReferences.Load() != 0 || f.h.multiClient.clientFlowCount(f.victim) != 0 ||
			f.victim.IsDone() || f.siblingOwner.IsDone() || f.h.boundTo(f.siblingPath) != f.siblingOwner {
			t.Fatal("SERVICE_REFRESH_AFFINITY_REFUSAL_KEPT_CLAIM: actual standing pre-bookkeeping refusal did not return its unbound initial placement")
		}
	})
}

// Shared shutdown closes every flow even while a native caller is held before
// its first send. Releasing that caller cannot resurrect the old placement.
func TestFullServiceRefreshAffinityParentJoinReturnsPendingClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		if err := f.h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !f.update.IsDone() || f.update.pendingServiceRefreshBinding != nil || f.victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("SERVICE_REFRESH_AFFINITY_PARENT_JOIN_KEPT_CLAIM: shutdown retained an inherited pre-send owner")
		}
		f.resume()
		<-f.sendDone
		synctest.Wait()
		if f.accepted || f.h.multiClient.clientFlowCount(f.victim) != 0 || f.victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("a released old native caller resurrected its joined initial placement")
		}
	})
}

// The actual rebind helper publishes the replacement map before returning
// the original initial-placement claim. Old late bookkeeping stays harmless.
func TestFullServiceRefreshAffinityRebindTransfersPendingClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		f.h.multiClient.stateLock.Lock()
		placed, replacements, unplaced := f.h.multiClient.rebindFlowsWithLock(f.victim, []*multiClientChannelUpdate{f.update}, []*multiClientChannel{f.siblingOwner})
		f.h.multiClient.stateLock.Unlock()
		if len(placed) != 1 || replacements != 1 || len(unplaced) != 0 || f.update.client.Load() != f.siblingOwner ||
			f.victim.serviceRefreshRaceReferences.Load() != 0 || f.update.pendingServiceRefreshBinding != nil || f.h.multiClient.clientFlowCount(f.siblingOwner) != 2 {
			t.Fatal("SERVICE_REFRESH_AFFINITY_REBIND_KEPT_OLD_CLAIM: actual replacement map did not consume the previous pending placement")
		}
		f.h.multiClient.bindClientFlow(f.update, f.victim)
		if f.h.multiClient.clientFlowCount(f.siblingOwner) != 2 || f.victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("stale initial-owner bookkeeping disturbed the actual replacement")
		}
		f.resume()
		<-f.sendDone
		synctest.Wait()
		if !f.accepted || f.h.boundTo(f.path) != f.siblingOwner || f.h.boundTo(f.siblingPath) != f.siblingOwner {
			t.Fatal("released first offer lost its actual current rebind or original sibling")
		}
	})
}

// A different SYN on the same still-selected update retains the placement
// claim until a real current-generation sender books that selected provider.
func TestFullServiceRefreshAffinityChangedSynKeepsCurrentPendingClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		next := *f.path
		next.SequenceNumber += 100
		accepted := privateCrossWindowNativeSyn(f.h, &next)
		synctest.Wait()
		currentUpdate := f.h.update(f.path)
		f.update.stateLock.Lock()
		current := currentUpdate == f.update && f.update.dialFailedSynSeen && f.update.dialFailedSyn == next.SequenceNumber &&
			f.update.synGenerationSeen && f.update.synGenerationNumber == next.SequenceNumber &&
			f.update.pendingServiceRefreshBinding == f.victim && f.update.client.Load() == f.victim
		f.update.stateLock.Unlock()
		actual := append(f.h.exit("a").syns(f.path), f.h.exit("b").syns(f.path)...)
		if !accepted || !current || f.victim.serviceRefreshRaceReferences.Load() != 1 || f.h.multiClient.clientFlowCount(f.victim) != 0 ||
			len(actual) != 1 || actual[0].path.SequenceNumber != next.SequenceNumber {
			t.Fatal("SERVICE_REFRESH_AFFINITY_NEW_SYN_LOST_CLAIM: an actual new source sequence discarded its still-unpublished current placement")
		}
		// A sender which found the same selected client does not rebind it.
		// The initial creator still owns this first parent-map handoff.
		f.resume()
		<-f.sendDone
		synctest.Wait()
		if f.h.update(f.path) != f.update || f.update.client.Load() != f.victim || f.victim.serviceRefreshRaceReferences.Load() != 0 ||
			f.update.pendingServiceRefreshBinding != nil || f.h.multiClient.clientFlowCount(f.victim) != 1 {
			t.Fatal("the original creator did not transfer its pending placement after an actual newer source attempt")
		}
		original, changed := 0, 0
		for _, packet := range append(f.h.exit("a").syns(f.path), f.h.exit("b").syns(f.path)...) {
			switch packet.path.SequenceNumber {
			case f.path.SequenceNumber:
				original++
			case next.SequenceNumber:
				changed++
			default:
				t.Fatal("actual source acquired an invented generation")
			}
		}
		if changed != 1 || original > 1 || f.h.boundTo(f.siblingPath) != f.siblingOwner || f.siblingOwner.IsDone() {
			t.Fatal("changed source attempt duplicated native offers or disturbed the original healthy sibling")
		}
	})
}

// Source controls arrive after real SYN admission and before its first bind.
// A reset latches close while the held native SYN offer still owns the flow;
// finishing that offer must retire it and return every placement claim.
func testFullServiceRefreshAffinityControlBeforeFirstBind(t *testing.T, reset bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		entered, release := f.holdBinding(t)
		f.resume()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("original real SYN did not reach first parent binding")
		}
		if len(f.h.exit("a").syns(f.path))+len(f.h.exit("b").syns(f.path)) != 1 {
			t.Fatal("first-binding control lacks its one actual provider SYN")
		}
		flags := byte(tcpFlagAck | tcpFlagFin)
		if reset {
			flags = tcpFlagAck | tcpFlagRst
		}
		done, controlResult := make(chan struct{}), make(chan bool, 1)
		go func() {
			defer close(done)
			controlResult <- f.offer(reraceTcpPacket(f.path, flags, f.path.SequenceNumber+1, reraceServerIsn+1, nil))
		}()
		t.Cleanup(func() { release(); <-done })
		synctest.Wait()
		controlAccepted := false
		if reset {
			select {
			case controlAccepted = <-controlResult:
			default:
				t.Fatal("reset did not complete its actual native admission")
			}
			f.update.stateLock.Lock()
			deferredClose := f.update.sequenceCloseReady.Load() && f.update.sequenceSynOffers == 1 &&
				!f.update.pendingCollapseCloseWithLock() && f.update.synRetry == nil &&
				f.update.pendingServiceRefreshBinding == f.victim && f.update.client.Load() == f.victim
			f.update.stateLock.Unlock()
			physicalResets := 0
			for _, exit := range f.h.generator.exitList() {
				for _, packet := range exit.received(f.path) {
					if packet.path.Rst {
						physicalResets++
					}
				}
			}
			creatorReturned := false
			select {
			case <-f.sendDone:
				creatorReturned = true
			default:
			}
			if !controlAccepted || physicalResets != 1 || !deferredClose || creatorReturned || f.update.IsDone() ||
				f.h.update(f.path) != f.update || f.victim.serviceRefreshRaceReferences.Load() != 1 || f.h.multiClient.clientFlowCount(f.victim) != 0 {
				t.Fatal("SERVICE_REFRESH_AFFINITY_RST_PENDING_CUSTODY_LOST: admitted reset did not retain its exact current placement while the original native SYN offer remained unfinished")
			}
		} else {
			f.update.stateLock.Lock()
			halfClosed := f.update.egressFinSeen && !f.update.ingressFinSeen && f.update.pendingServiceRefreshBinding == f.victim
			f.update.stateLock.Unlock()
			if !halfClosed || f.update.IsDone() || f.victim.serviceRefreshRaceReferences.Load() != 1 || f.h.multiClient.clientFlowCount(f.victim) != 0 {
				t.Fatal("SERVICE_REFRESH_AFFINITY_FIN_LOST_CLAIM: half-close before first map publication discarded its live initial placement")
			}
		}
		release()
		<-done
		<-f.sendDone
		synctest.Wait()
		if !reset {
			controlAccepted = <-controlResult
		}
		if !controlAccepted {
			t.Fatal("source control did not retain its actual native admission receipt")
		}
		wanted := 1
		if reset {
			wanted = 0
			f.update.stateLock.Lock()
			returned := f.update.pendingServiceRefreshBinding == nil && f.update.sequenceSynOffers == 0 && f.update.synRetry == nil
			f.update.stateLock.Unlock()
			if !f.update.IsDone() || f.h.update(f.path) != nil || !returned || f.victim.serviceRefreshRaceReferences.Load() != 0 ||
				f.h.multiClient.clientFlowCount(f.victim) != 0 {
				t.Fatal("SERVICE_REFRESH_AFFINITY_RST_KEPT_CLAIM: completed reset and original native caller retained or resurrected the old initial placement")
			}
		}
		if f.victim.serviceRefreshRaceReferences.Load() != 0 || f.h.multiClient.clientFlowCount(f.victim) != wanted ||
			f.h.boundTo(f.siblingPath) != f.siblingOwner || f.siblingOwner.IsDone() {
			t.Fatal("source control changed its exact final claim/map custody or unrelated healthy sibling")
		}
		if reset {
			next := *f.path
			next.SequenceNumber += 100
			if !privateCrossWindowNativeSyn(f.h, &next) {
				t.Fatal("actual fresh source after completed reset was refused")
			}
			synctest.Wait()
			current := f.h.update(&next)
			actual := append(f.h.exit("a").syns(f.path), f.h.exit("b").syns(f.path)...)
			if current == nil || current == f.update || current.IsDone() || f.h.boundTo(&next) != f.victim ||
				current.pendingServiceRefreshBinding != nil || f.victim.serviceRefreshRaceReferences.Load() != 0 ||
				f.h.multiClient.clientFlowCount(f.victim) != 1 || len(actual) != 2 || actual[0].path.SequenceNumber != f.path.SequenceNumber ||
				actual[1].path.SequenceNumber != next.SequenceNumber {
				t.Fatal("SERVICE_REFRESH_AFFINITY_RST_OLD_OWNER_REOPENED: fresh actual generation inherited reset ownership or duplicated old source emission")
			}
			freshAnswer := false
			for _, packet := range f.h.delivered(&next) {
				path, err := ParseIpPath(packet.packet)
				if err == nil && path.Protocol == IpProtocolTcp && path.Syn && path.Ack && path.AckSequenceNumber == next.SequenceNumber+1 {
					freshAnswer = true
				}
			}
			if !freshAnswer || !current.receivedInbound.Load() ||
				!f.offer(reraceSegment(f.siblingPath, uint32(4*len(f.payload)), f.payload)) || f.h.boundTo(f.siblingPath) != f.siblingOwner {
				t.Fatal("completed source reset disturbed fresh generation response or the original healthy sibling")
			}
			if !f.offer(reraceSegment(&next, 0, f.payload)) {
				t.Fatal("fresh generation refused its actual native body")
			}
			synctest.Wait()
			freshBodies := 0
			for _, packet := range f.h.delivered(&next) {
				path, payload, err := ParseIpPathWithPayload(packet.packet)
				if err == nil && path.Protocol == IpProtocolTcp && path.Ack && !path.Syn &&
					path.AckSequenceNumber == next.SequenceNumber+1+uint32(len(f.payload)) && bytes.Equal(payload, f.payload) {
					freshBodies++
				}
			}
			if freshBodies != 1 || f.h.update(&next) != current || current.IsDone() || f.h.boundTo(&next) != f.victim ||
				f.h.multiClient.clientFlowCount(f.victim) != 1 || f.victim.serviceRefreshRaceReferences.Load() != 0 {
				t.Fatal("SERVICE_REFRESH_AFFINITY_RST_NEW_GENERATION_BODY_FAILED: reset retirement did not preserve exact fresh-generation body delivery and ownership")
			}
		}
	})
}

func TestFullServiceRefreshAffinityFinBeforeFirstBindKeepsHalfCloseClaim(t *testing.T) {
	testFullServiceRefreshAffinityControlBeforeFirstBind(t, false)
}

func TestFullServiceRefreshAffinityRstBeforeFirstBindReturnsClaim(t *testing.T) {
	testFullServiceRefreshAffinityControlBeforeFirstBind(t, true)
}

// Cancellation ends the old placement before an actual distinct update can
// reuse its tuple. Releasing the canceled first caller cannot bind it again.
func TestFullServiceRefreshAffinityNewUpdateReturnsOldPendingClaim(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		f.requirePending(t)
		f.update.Close()
		f.h.multiClient.notifyFlowReaper()
		synctest.Wait()
		if f.update.pendingServiceRefreshBinding != nil || f.victim.serviceRefreshRaceReferences.Load() != 0 || f.h.update(f.path) != nil {
			t.Fatal("SERVICE_REFRESH_AFFINITY_CANCELED_UPDATE_KEPT_CLAIM: real closed generation retained its initial placement")
		}
		next := *f.path
		next.SequenceNumber += 100
		if !privateCrossWindowNativeSyn(f.h, &next) {
			t.Fatal("distinct source generation failed actual native admission")
		}
		synctest.Wait()
		current := f.h.update(&next)
		if current == nil || current == f.update || current.IsDone() || current.client.Load() != f.victim ||
			current.pendingServiceRefreshBinding != nil || f.victim.serviceRefreshRaceReferences.Load() != 0 || f.h.multiClient.clientFlowCount(f.victim) != 1 {
			t.Fatal("new actual update inherited canceled pending ownership or failed its own race-to-map handoff")
		}
		f.resume()
		<-f.sendDone
		synctest.Wait()
		actual := append(f.h.exit("a").syns(f.path), f.h.exit("b").syns(f.path)...)
		if f.h.update(&next) != current || len(actual) != 1 || actual[0].path.SequenceNumber != next.SequenceNumber ||
			f.h.multiClient.clientFlowCount(f.victim) != 1 || f.victim.serviceRefreshRaceReferences.Load() != 0 {
			t.Fatal("canceled old caller revived its selection or damaged the distinct current source")
		}
	})
}
