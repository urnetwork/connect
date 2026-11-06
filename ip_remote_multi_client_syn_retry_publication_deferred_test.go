// A busy paid flow must not park publication work for another real retained
// source. Deferred publication reuses the existing parent timer and custody.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Cancel the skipped source before its retry. The sibling has already made
// physical progress on the same publisher while the first leaf was held.
func TestNativeSynLatePublicationCanceledDeferredOwnerStaysCanceled(t *testing.T) {
	testSynPublicationDeferredPaidPair(t, true)
}

// The real callback remains synchronous. These optional test observations
// control only its first ping and the physical boundary of an old clock wait.
type synPublicationPaidPairInstallTestObserver struct {
	startLatePing    func(context.Context, func())
	beforeLateAdd    func(*multiClientWindow)
	beforeAwait      func(time.Time)
	beforeLegacyWait func()
	installed        func(*multiClientWindow, *multiClientChannel, func(), <-chan struct{})
}

// Two sources retain genuine native SYNs after both real providers refuse.
// The third real provider is installed with its actual callback still gated.
func newSynPublicationPaidPairTestFixture(t *testing.T, retryDelay time.Duration) (*reraceHarness, [2]*IpPath, [2]*multiClientChannelUpdate, [2]*tcpSynRetry, *multiClientChannel, *reraceExit, func(), <-chan struct{}) {
	return newSynPublicationPaidPairTestFixtureObserved(t, retryDelay, nil)
}

// Installation is an event, not a reason to settle a callback-held bubble.
func newSynPublicationPaidPairTestFixtureObserved(t *testing.T, retryDelay time.Duration, observer *synPublicationPaidPairInstallTestObserver) (*reraceHarness, [2]*IpPath, [2]*multiClientChannelUpdate, [2]*tcpSynRetry, *multiClientChannel, *reraceExit, func(), <-chan struct{}) {
	t.Helper()
	paths := [2]*IpPath{reraceFlow(4, 96, 53360), reraceFlow(4, 97, 53361)}
	answer := func(candidate *IpPath) reraceAnswer {
		if reraceSameFlow(candidate, paths[0]) || reraceSameFlow(candidate, paths[1]) {
			return reraceSignal
		}
		return reraceAccept
	}
	h := newReraceHarness(t, []reraceExitSpec{{name: "pair-a", answer: answer}, {name: "pair-b", answer: answer}}, func(settings *MultiClientSettings) {
		settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 3, WindowSizeHardMax: 3}
		settings.StrictWindowSizeHardMax, settings.StandingReserve, settings.EvaluationPoolMultiple = true, false, 1
		settings.SynRetryPublicationRetryDelay = retryDelay
	})
	h.settle()
	var updates [2]*multiClientChannelUpdate
	var owners [2]*tcpSynRetry
	for index, path := range paths {
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("paid-pair fixture refused an original native SYN")
		}
		updates[index] = h.update(path)
		owners[index] = synRetryControlOwner(t, updates[index])
		updates[index].stateLock.Lock()
		refused := updates[index].synRetryHasRemoteRefusalWithLock() && len(updates[index].dialFailedClients) == 2 &&
			updates[index].client.Load() == nil && !updates[index].receivedInbound.Load()
		updates[index].stateLock.Unlock()
		if !refused {
			t.Fatal("paid-pair fixture lacks two actual same-generation native refusals")
		}
	}
	h.multiClient.stateLock.Lock()
	indexed := h.multiClient.synPublicationOwnerHead == owners[0] && owners[0].publicationNext == owners[1] &&
		h.multiClient.synPublicationOwnerTail == owners[1] && h.multiClient.synPublicationOwnerCount == 2
	h.multiClient.stateLock.Unlock()
	if !indexed {
		t.Fatal("two real paid sources are not in their original publication index order")
	}
	window := h.multiClient.windows[WindowTypeQuality]
	if observer != nil && observer.startLatePing != nil {
		window.settings.startChannelPingForTest = func(start func()) { observer.startLatePing(h.multiClient.ctx, start) }
	}
	previous := window.clientAddCallback
	entered, release, callbackDone := make(chan *multiClientChannel, 1), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	window.clientAddCallback = func(client *multiClientChannel) {
		if observer != nil && observer.installed != nil {
			observer.installed(window, client, resume, callbackDone)
		}
		entered <- client
		select {
		case <-release:
		case <-h.multiClient.ctx.Done():
		}
		previous(client)
		close(callbackDone)
	}
	t.Cleanup(func() { synPublicationTestCloseFixture(t, h.multiClient, resume) })
	if observer != nil && observer.beforeLateAdd != nil {
		observer.beforeLateAdd(window)
	}
	lateExit := h.addExit(reraceExitSpec{name: "pair-late"})
	// This is the original waitForExits setup bound, captured at its old call
	// site. A bare generator may first see this append in a later ordinary pass.
	setupDeadline := time.Now().Add(time.Minute)
	if observer != nil && observer.beforeAwait != nil {
		observer.beforeAwait(setupDeadline)
	}
	late, installed := synPublicationTestAwaitInstalled(h.multiClient.ctx, entered, setupDeadline)
	if !installed {
		t.Fatal("SYN_PUBLICATION_PAID_PAIR_INSTALL_NOT_OBSERVED: original setup deadline elapsed")
	}
	// expand calls this callback while owning its local mutex. A resize edge
	// may wait for that mutex, so no fake-clock polling or bubble Wait belongs here.
	if late != h.channel("pair-late") || len(h.liveChannels()) != 3 || len(lateExit.syns(paths[0])) != 0 || len(lateExit.syns(paths[1])) != 0 {
		t.Fatal("paid-pair fixture exposed retained bytes before actual publication")
	}
	return h, paths, updates, owners, late, lateExit, resume, callbackDone
}

// The old preliminary Lock is witnessed at its physical boundary by the
// negative-only observer. The fixed owner must reach the sibling SYN-ACK first.
func testSynPublicationDeferredPaidPair(t *testing.T, cancelSkipped bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		retryDelay := DefaultMultiClientSettings().SynRetryPublicationRetryDelay
		if cancelSkipped {
			retryDelay = 7 * time.Millisecond
		}
		h, paths, updates, owners, late, lateExit, resume, callbackDone := newSynPublicationPaidPairTestFixture(t, retryDelay)
		atQueue, queueRelease := make(chan struct{}), make(chan struct{})
		blockedWait, siblingAnswered := make(chan struct{}, 1), make(chan struct{})
		waitArmed := make(chan time.Duration, 1)
		passEnded := make(chan bool, 1)
		var queueOnce, releaseOnce, answeredOnce sync.Once
		var targetSeen atomic.Bool
		resumeQueue := func() { releaseOnce.Do(func() { close(queueRelease) }) }
		t.Cleanup(resumeQueue)
		h.multiClient.beforeSynPublicationQueueForTest = func(current *multiClientChannelUpdate) {
			if current == updates[0] {
				queueOnce.Do(func() {
					targetSeen.Store(true)
					close(atQueue)
					select {
					case <-queueRelease:
					case <-h.multiClient.ctx.Done():
					}
				})
			}
		}
		h.multiClient.beforeSynPublicationBlockingWaitForTest = func(current *multiClientChannelUpdate) {
			if current == updates[0] {
				select {
				case blockedWait <- struct{}{}:
				default:
				}
			}
		}
		h.multiClient.SetReceivePacketCallback(synPublicationPaidPairReceiveObserver(h.receive, func(path *IpPath, packet []byte) {
			if path != nil && (reraceSameFlow(path, paths[1]) || reraceSameFlow(path, paths[1].Reverse())) {
				if parsed, err := ParseIpPath(packet); err == nil && parsed.Syn && parsed.Ack {
					answeredOnce.Do(func() { close(siblingAnswered) })
				}
			}
		}))
		h.multiClient.afterSynPublicationTurnForTest = func() {
			if !targetSeen.Load() {
				return
			}
			h.multiClient.stateLock.Lock()
			complete := h.multiClient.synPublicationHead == nil
			deferred := late.synPublicationDeferred && late.synPublicationQueued
			h.multiClient.stateLock.Unlock()
			if complete {
				select {
				case passEnded <- deferred:
				default:
				}
			}
		}
		h.multiClient.beforeFlowReaperWaitForTest = func(delay time.Duration, hasNext bool) {
			if !targetSeen.Load() || !hasNext {
				return
			}
			h.multiClient.stateLock.Lock()
			deferred := late.synPublicationDeferred && late.synPublicationQueued
			h.multiClient.stateLock.Unlock()
			if deferred {
				select {
				case waitArmed <- delay:
				default:
				}
			}
		}
		resume()
		<-atQueue
		synctest.Wait()
		updates[0].stateLock.Lock()
		var unlockOnce sync.Once
		unlock := func() { unlockOnce.Do(updates[0].stateLock.Unlock) }
		t.Cleanup(unlock)
		resumeQueue()
		blocked := false
		select {
		case <-blockedWait:
			blocked = true
		case <-siblingAnswered:
		}
		if blocked {
			unlock()
			synctest.Wait()
			t.Fatal("SYN_PUBLICATION_BUSY_OWNER_BLOCKED_HEALTHY_SIBLING: preliminary blocking wait stopped the actual shared reaper")
		}
		// This receipt can only follow shared-worker dispatch of the other source.
		// The first source is still physically locked and has emitted no retry.
		if len(lateExit.syns(paths[0])) != 0 || len(lateExit.syns(paths[1])) != 1 {
			unlock()
			t.Fatal("busy owner was sent or healthy sibling lacked its one actual retained SYN")
		}
		if deferred := <-passEnded; !deferred {
			unlock()
			synctest.Wait()
			t.Fatal("SYN_PUBLICATION_DEFERRED_EVENT_LOST: contended pass discarded its exact publisher revisit")
		}
		delay := <-waitArmed
		if delay <= 0 || h.multiClient.settings.SynRetryPublicationRetryDelay < delay {
			unlock()
			t.Fatalf("shared reaper did not arm the existing bounded retry timer: delay=%v", delay)
		}
		if cancelSkipped {
			updates[0].cancel()
		}
		unlock()
		h.sleep(h.multiClient.settings.SynRetryPublicationRetryDelay)
		if cancelSkipped {
			if len(lateExit.syns(paths[0])) != 0 || updates[0].receivedInbound.Load() {
				t.Fatal("canceled skipped owner emitted retained SYN bytes")
			}
		} else if _, answered := h.synAck(paths[0]); !answered || len(lateExit.syns(paths[0])) != 1 || h.update(paths[0]) != updates[0] {
			t.Fatal("SYN_PUBLICATION_DEFERRED_EVENT_LOST: released paid owner did not resume exactly once without a new native source")
		}
		<-callbackDone
		h.multiClient.stateLock.Lock()
		pending := h.multiClient.synPublicationHead != nil || h.multiClient.synPublicationDeferredHead != nil
		h.multiClient.stateLock.Unlock()
		if pending {
			t.Fatal("settled paid-pair publication retained pending client custody")
		}
		for _, owner := range owners {
			synRetryControlReleased(t, owner)
			if !owner.publicationRetired.Load() || owner.publicationIndexed {
				t.Fatal("final actual source release retained publication index custody")
			}
		}
	})
}

// Solely deferred work must not create an immediate wake edge. A new client
// remains runnable during that interval, without renewing the old deadline.
func TestSynPublicationDeferredWorkWaitsWhileFreshPublisherRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultMultiClientSettings()
		parent := &RemoteUserNatMultiClient{ctx: t.Context(), settings: settings, flowReaperDone: make(chan struct{}), flowReaperWake: make(chan struct{}, 1)}
		deferred := &multiClientChannel{ctx: t.Context(), synPublicationQueued: true, synPublicationDeferred: true,
			synPublicationRetryAt: time.Now().Add(settings.SynRetryPublicationRetryDelay)}
		parent.synPublicationDeferredHead, parent.synPublicationDeferredTail = deferred, deferred
		parent.drainSynPublications()
		if len(parent.flowReaperWake) != 0 {
			t.Fatal("SYN_PUBLICATION_DEFERRED_SELF_WAKE: future-only work immediately rewoke the shared reaper")
		}
		fresh := &multiClientChannel{ctx: t.Context()}
		parent.enqueuePublishedSynRetries(fresh)
		<-parent.flowReaperWake
		parent.drainSynPublications()
		if fresh.synPublicationQueued || parent.synPublicationHead != nil || parent.synPublicationDeferredHead != deferred {
			t.Fatal("fresh publisher waited behind a deferred client or stole its custody")
		}
		deadline := deferred.synPublicationRetryAt
		delay, pending := parent.synPublicationNextDelay(time.Now())
		if !pending || delay != settings.SynRetryPublicationRetryDelay || len(parent.flowReaperWake) != 0 {
			t.Fatalf("deferred wake policy changed: pending=%t delay=%v wake=%d", pending, delay, len(parent.flowReaperWake))
		}
		time.Sleep(delay - time.Nanosecond)
		parent.drainSynPublications()
		if parent.synPublicationDeferredHead != deferred || !deferred.synPublicationRetryAt.Equal(deadline) || len(parent.flowReaperWake) != 0 {
			t.Fatal("publication retry ran early, replenished its deadline, or self-spun")
		}
		time.Sleep(time.Nanosecond)
		parent.drainSynPublications()
		if deferred.synPublicationQueued || parent.synPublicationHead != nil || parent.synPublicationDeferredHead != nil {
			t.Fatal("due publication did not promote and finish within its bounded empty-owner pass")
		}
	})
}

// A publisher retiring in the deferred list must relinquish its graph before
// flow cleanup, and cannot disturb an independent queued publisher.
func TestSynPublicationDeferredRetirementPreservesOtherPublishers(t *testing.T) {
	settings := DefaultMultiClientSettings()
	parent := &RemoteUserNatMultiClient{ctx: t.Context(), settings: settings, flowReaperDone: make(chan struct{})}
	first, last := &multiClientChannel{ctx: t.Context()}, &multiClientChannel{ctx: t.Context()}
	for _, client := range []*multiClientChannel{first, last} {
		client.synPublicationQueued, client.synPublicationDeferred = true, true
		client.synPublicationRetryAt = time.Now().Add(settings.SynRetryPublicationRetryDelay)
	}
	first.synPublicationNext = last
	parent.synPublicationDeferredHead, parent.synPublicationDeferredTail = first, last
	fresh := &multiClientChannel{ctx: t.Context()}
	parent.enqueuePublishedSynRetries(fresh)
	parent.retireSynPublicationClientMetadata(first)
	if parent.synPublicationDeferredHead != last || parent.synPublicationDeferredTail != last || parent.synPublicationHead != fresh ||
		first.synPublicationQueued || first.synPublicationDeferred || first.synPublicationNext != nil || !first.synPublicationRetryAt.IsZero() {
		t.Fatal("deferred retirement retained its graph or disturbed a sibling publication")
	}
	parent.discardSynPublications()
	if last.synPublicationQueued || fresh.synPublicationQueued || last.synPublicationNext != nil || !last.synPublicationRetryAt.IsZero() {
		t.Fatal("shutdown retained runnable or deferred publication references")
	}
}

// The delay is per owner. Disabling it declines both event and paid-index
// admission, so a nonpositive value cannot create an immediate retry loop.
func TestSynPublicationNonpositiveRetryDelayDeclinesWithoutWake(t *testing.T) {
	for _, delay := range []time.Duration{0, -time.Nanosecond} {
		settings := DefaultMultiClientSettings()
		settings.SynRetryPublicationRetryDelay = delay
		parent := &RemoteUserNatMultiClient{ctx: t.Context(), settings: settings, flowReaperDone: make(chan struct{}), flowReaperWake: make(chan struct{}, 1)}
		client := &multiClientChannel{ctx: t.Context()}
		parent.enqueuePublishedSynRetries(client)
		update := &multiClientChannelUpdate{sequenceParent: parent}
		owner := &tcpSynRetry{update: update}
		update.synRetry = owner
		update.indexSynRetryPublication()
		parent.drainSynPublications()
		if client.synPublicationQueued || owner.publicationIndexed || len(parent.flowReaperWake) != 0 {
			t.Fatal("disabled retry delay admitted publication work or generated a wake edge")
		}
	}
}

// Source capture cannot attach an owner whose final reference already fenced
// index admission. The fixed backing and credit have only that one lifetime.
func TestSynPublicationIndexRejectsRetiredCapturedOwner(t *testing.T) {
	parent := &RemoteUserNatMultiClient{ctx: t.Context(), settings: DefaultMultiClientSettings(), flowReaperDone: make(chan struct{})}
	update := &multiClientChannelUpdate{sequenceParent: parent}
	owner := &tcpSynRetry{update: update}
	owner.publicationRetired.Store(true)
	update.synRetry = owner
	update.indexSynRetryPublication()
	if owner.publicationIndexed || parent.synPublicationOwnerHead != nil || parent.synPublicationOwnerCount != 0 {
		t.Fatal("SYN_PUBLICATION_RETIRED_CAPTURE_REATTACHED: final-reference fence allowed a stale index attachment")
	}
}

// The actual shared worker must take the earlier idle-flow deadline instead
// of letting an independent publication retry postpone flow retirement.
func TestSynPublicationDeferredTimerPreservesEarlierFlowExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		settings := DefaultMultiClientSettings()
		settings.SequenceIdleTimeout = 10 * time.Millisecond
		settings.SynRetryPublicationRetryDelay = 20 * time.Millisecond
		settings.UdpTeardownSignal = false
		parent := flowReaperTestParent(ctx, settings)
		parent.flowReaperDone = make(chan struct{})
		deferred := &multiClientChannel{ctx: ctx, synPublicationQueued: true, synPublicationDeferred: true,
			synPublicationRetryAt: time.Now().Add(settings.SynRetryPublicationRetryDelay)}
		parent.synPublicationDeferredHead, parent.synPublicationDeferredTail = deferred, deferred
		path := flowReaperTestPath(4, IpProtocolUdp, 53362)
		update := newMultiClientChannelUpdate(ctx, path)
		update.activityTime = time.Now()
		parent.ip4PathUpdates[path.ToIp4Path()], parent.flowUpdates[update] = update, true
		armed := make(chan time.Duration, 1)
		var once sync.Once
		parent.beforeFlowReaperWaitForTest = func(delay time.Duration, hasNext bool) {
			if hasNext {
				once.Do(func() { armed <- delay })
			}
		}
		go parent.runFlowReaper()
		defer func() {
			cancel()
			<-parent.flowReaperDone
			update.Close()
			if deferred.synPublicationQueued || parent.synPublicationDeferredHead != nil {
				t.Error("parent cancellation retained its deferred publication graph")
			}
		}()
		synctest.Wait()
		if delay := <-armed; delay != settings.SequenceIdleTimeout {
			t.Fatalf("SYN_PUBLICATION_POSTPONED_FLOW_EXPIRY: actual shared timer=%v want earlier flow=%v", delay, settings.SequenceIdleTimeout)
		}
		time.Sleep(settings.SequenceIdleTimeout - time.Nanosecond)
		if update.IsDone() {
			t.Fatal("publication changed an idle flow's just-before expiry boundary")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !update.IsDone() || !deferred.synPublicationQueued || time.Now().After(deferred.synPublicationRetryAt) {
			t.Fatal("earlier actual flow deadline was not honored independently of deferred publication")
		}
	})
}

// This metadata-only owner exercises the real capture boundary. The source
// may wait for its own held leaf while the shared parent remains available.
func TestSynPublicationIndexCaptureNeverWaitsBeneathParent(t *testing.T) {
	parent := &RemoteUserNatMultiClient{ctx: t.Context(), settings: DefaultMultiClientSettings(), flowReaperDone: make(chan struct{})}
	update := &multiClientChannelUpdate{sequenceParent: parent}
	owner := &tcpSynRetry{update: update, references: 1}
	update.synRetry = owner
	available := make(chan bool, 1)
	releaseCapture, done := make(chan struct{}), make(chan struct{})
	var releaseOnce, unlockOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCapture) }) }
	locked := false
	unlock := func() {
		if locked {
			unlockOnce.Do(update.stateLock.Unlock)
		}
	}
	parent.beforeSynPublicationIndexCaptureForTest = func(current *multiClientChannelUpdate) {
		free := parent.stateLock.TryLock()
		if free {
			parent.stateLock.Unlock()
		}
		available <- free
		<-releaseCapture
	}
	go func() { defer close(done); update.indexSynRetryPublication() }()
	defer func() {
		unlock()
		release()
		<-done
		parent.removeSynPublicationOwner(owner)
	}()
	free := <-available
	// The initial parent snapshot has finished. Hold the same source leaf
	// before resuming the preserved pre-owner-capture hook, never a fake-clock
	// Wait on a mutex that Go does not classify as durably blocked.
	update.stateLock.Lock()
	locked = true
	release()
	parentFree := parent.stateLock.TryLock()
	if parentFree {
		parent.stateLock.Unlock()
	}
	unlock()
	<-done
	if !parentFree {
		t.Fatal("SYN_PUBLICATION_INDEX_WAIT_HELD_PARENT: waiting for source capture held the shared parent")
	}
	if !free {
		t.Fatal("SYN_PUBLICATION_INDEX_WAIT_HELD_PARENT: paid-owner capture attempted its busy leaf beneath the shared parent")
	}
}

// Forward the existing typed callback without a new child-package dependency.
// The observer borrows the same live packet only after the actual receiver.
func synPublicationPaidPairReceiveObserver[Mode any](receive func(TransferPath, Mode, *IpPath, []byte), observe func(*IpPath, []byte)) func(TransferPath, Mode, *IpPath, []byte) {
	return func(source TransferPath, mode Mode, path *IpPath, packet []byte) {
		receive(source, mode, path, packet)
		observe(path, packet)
	}
}

// A real resize notification is delivered while the actual admission callback
// owns expand's mutex. Installation must return at that event, before any
// fake-clock poll or bubble-wide Wait; then the same callback is released/joined.
func TestNativeSynLatePublicationPaidPairInstallSurvivesResizeEdge(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		type installation struct {
			client     *multiClientChannel
			at         time.Time
			resizeEdge <-chan struct{}
			resume     func()
			done       <-chan struct{}
		}
		installed := make(chan installation, 1)
		pingRelease := make(chan struct{})
		var pingOnce sync.Once
		releasePing := func() { pingOnce.Do(func() { close(pingRelease) }) }
		t.Cleanup(releasePing)
		var oldAcquisitionDeadline, setupDeadline time.Time
		observer := &synPublicationPaidPairInstallTestObserver{
			beforeLateAdd: func(window *multiClientWindow) {
				window.stateLock.Lock()
				oldAcquisitionDeadline = window.discoveryDemand.deadline
				emptySlot := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen &&
					window.discoveryDemand.slot == 3 && len(window.clients) == 2
				window.stateLock.Unlock()
				if !emptySlot || window.cooperativeDiscoveryDemand() || !time.Now().Before(oldAcquisitionDeadline) {
					t.Fatal("paid-pair setup no longer owns the exhausted legacy third slot before late append")
				}
			},
			startLatePing: func(ctx context.Context, start func()) {
				select {
				case <-pingRelease:
					start()
				case <-ctx.Done():
				}
			},
			beforeAwait: func(deadline time.Time) {
				setupDeadline = deadline
				if !setupDeadline.Equal(time.Now().Add(time.Minute)) {
					t.Fatal("SYN_PUBLICATION_SETUP_BUDGET_CHANGED: original one-minute fixture setup bound was changed or renewed")
				}
				releasePing()
			},
			installed: func(window *multiClientWindow, client *multiClientChannel, resume func(), done <-chan struct{}) {
				edge := window.resizeMonitor.NotifyChannel()
				window.resizeMonitor.NotifyAll()
				installed <- installation{client: client, at: time.Now(), resizeEdge: edge, resume: resume, done: done}
			},
		}
		// Counterfactual observers run at the old physical wait boundary. They
		// release/join the real callback before reporting the semantic failure,
		// rather than retaining the known mutex/clock cycle until Go's timeout.
		observer.beforeLegacyWait = func() {
			releasePing()
			receipt := <-installed
			select {
			case <-receipt.resizeEdge:
			default:
				t.Error("forced real resize edge was not delivered")
			}
			receipt.resume()
			<-receipt.done
			t.Fatal("SYN_PUBLICATION_CALLBACK_HELD_CLOCK_WAIT: fixture attempted a fake-clock or bubble wait while the real admission callback still owned expand's mutex")
		}
		h, paths, updates, owners, late, lateExit, resume, callbackDone := newSynPublicationPaidPairTestFixtureObserved(t, DefaultMultiClientSettings().SynRetryPublicationRetryDelay, observer)
		receipt := <-installed
		if receipt.client != late || !receipt.at.Equal(time.Now()) {
			t.Fatal("paid-pair installation advanced the fake clock after the actual callback event")
		}
		if receipt.at.Before(oldAcquisitionDeadline) || !receipt.at.Before(setupDeadline) {
			t.Fatal("SYN_PUBLICATION_WRONG_PASS_BOUNDARY: real late installation did not cross the exhausted pass within the original setup budget")
		}
		for index, path := range paths {
			sameUpdate := h.update(path) == updates[index]
			updates[index].stateLock.Lock()
			same := sameUpdate && updates[index].synRetry == owners[index] &&
				owners[index].references == 1 && owners[index].serial != 0 && owners[index].validWithLock(updates[index])
			updates[index].stateLock.Unlock()
			if !same {
				t.Fatal("later ordinary acquisition replaced an original paid SYN owner or its one cache reference")
			}
		}
		select {
		case <-receipt.resizeEdge:
		default:
			t.Fatal("forced real resize edge was not delivered")
		}
		select {
		case <-callbackDone:
			t.Fatal("paid-pair installation released the actual publication callback prematurely")
		default:
		}
		resume()
		<-callbackDone
		h.settle()
		for index, path := range paths {
			if !h.waitForSynAck(path, setupDeadline) || len(lateExit.syns(path)) != 1 {
				t.Fatal("resize-edge control did not preserve one actual retained SYN and response per paid source")
			}
			synRetryControlReleased(t, owners[index])
		}
	})
}

// Each harness owns fresh settings/window callbacks. Closing its parent does
// not join window constructors, so abandoned test hooks are never restored.
func synPublicationTestCloseFixture(t testing.TB, parent *RemoteUserNatMultiClient, resume func()) {
	t.Helper()
	resume()
	if err := parent.CloseAndWait(context.Background()); err != nil {
		t.Error(err)
	}
}

// A real constructor owns its result even if parent teardown completed while
// the legacy NewClient call was held. The test later releases and joins it.
type synPublicationCleanupTestGenerator struct {
	MultiClientGenerator
	entered chan struct{}
	release <-chan struct{}
}

func (self *synPublicationCleanupTestGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
	client, err := self.MultiClientGenerator.NewClient(ctx, args, settings)
	if err != nil {
		return nil, err
	}
	close(self.entered)
	<-self.release
	return client, nil
}

// Cleanup must not mutate construction-shared test settings. The constructor
// is physically held before its actual hook read while fixture shutdown ends.
func TestNativeSynPublicationFixtureCleanupKeepsHeldConstructorHooks(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var hookReads atomic.Uint64
		h := newReraceHarness(t, []reraceExitSpec{{name: "cleanup-owner"}}, func(settings *MultiClientSettings) {
			settings.startChannelPingForTest = func(start func()) { hookReads.Add(1); start() }
		})
		h.settle()
		window := h.multiClient.windows[WindowTypeQuality]
		// The fresh harness owns this callback. Install it before starting the
		// separately owned constructor, which never publishes into a window.
		var callbackReads atomic.Uint64
		window.clientAddCallback = func(*multiClientChannel) { callbackReads.Add(1) }
		destination := h.exit("cleanup-owner").destination
		args, err := h.generator.NewClientArgsForDestination(destination)
		if err != nil {
			t.Fatal(err)
		}
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		generator := &synPublicationCleanupTestGenerator{MultiClientGenerator: h.generator, entered: entered, release: release}
		var channel *multiClientChannel
		var constructorErr error
		t.Cleanup(func() {
			finish()
			<-done
			if channel != nil {
				channel.Close()
				if err := channel.client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
		beforeReads := hookReads.Load()
		go func() {
			defer close(done)
			channel, constructorErr = newMultiClientChannel(
				h.multiClient.ctx,
				&multiClientChannelArgs{MultiClientGeneratorClientArgs: *args, Destination: destination},
				generator, nil, nil, DefaultSecurityPolicy(h.ctx), func(*ContractStatus) {}, func([]*ContractStatsEvent) {}, func() {}, nil, window.settings,
				nil, nil, nil, nil, nil, nil, nil, nil, nil,
			)
		}()
		<-entered
		var resumed bool
		synPublicationTestCloseFixture(t, h.multiClient, func() { resumed = true })
		select {
		case <-done:
			t.Fatal("fixture cleanup unexpectedly joined the separately owned held constructor")
		default:
		}
		hookRetained := window.settings.startChannelPingForTest != nil
		// Initial admission settled before this marker was installed; the held
		// constructor is direct and never publishes into a window. This borrowed
		// marker takes no channel ownership and records only its own invocation.
		window.clientAddCallback(nil)
		callbackRetained := callbackReads.Load() == 1
		finish()
		<-done
		if !resumed || !hookRetained || !callbackRetained || hookReads.Load() != beforeReads+1 {
			t.Fatalf("SYN_PUBLICATION_FIXTURE_RESTORED_LIVE_CONSTRUCTOR_HOOK: resumed=%t hook_retained=%t callback_retained=%t actual_hook_read=%t", resumed, hookRetained, callbackRetained, hookReads.Load() == beforeReads+1)
		}
		if constructorErr != nil || channel == nil {
			t.Fatalf("held real constructor did not return its independently owned channel: %v", constructorErr)
		}
	})
}
