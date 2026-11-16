// Source-custody roots use native public packets, real Transfer queues, actual
// routed Packs and peer ACKs. No channel pointer is treated as a NAT identity.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A source fence is useful only while the parent reservation has stopped new
// old-generation enrollment. These component roots isolate that one boundary.
func affinitySourcesTestCloseEnrollment(update *multiClientChannelUpdate) {
	update.stateLock.Lock()
	update.affinitySources.closed = true
	update.stateLock.Unlock()
}

func TestAffinityRenewalSourceFenceRequiresCumulativePeerCompletion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		fromPeer := make(Route, 16)
		f.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fromPeer})
		t.Cleanup(func() {
			if err := f.closeClient(context.Background()); err != nil {
				t.Error(err)
			}
			drainFlightGateRoute(fromPeer)
		})
		template := groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)
		if f.offer(template) != 1 {
			t.Fatal("initial public SYN was not admitted")
		}
		f.requireParked()
		affinitySourcesTestCloseEnrollment(f.update)
		if f.update.affinitySourceFenced(f.selected) {
			t.Fatal("enqueue without source delivery became a provider fence")
		}
		f.unpark()
		synctest.Wait()
		if len(f.route) != 1 {
			t.Fatal("fixture did not write exactly one actual SYN Pack")
		}
		wire := <-f.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		defer MessagePoolReturn(wire)
		ackFlightGatePack(t, f.client, f.peer, fromPeer, pack, true)
		synctest.Wait()
		if f.update.affinitySourceFenced(f.selected) || f.terminal.Load() != 0 {
			t.Fatal("selective acknowledgement falsely fenced an accepted old control")
		}
		ackFlightGatePack(t, f.client, f.peer, fromPeer, pack, false)
		synctest.Wait()
		if !f.update.affinitySourceFenced(f.selected) || f.terminal.Load() != 1 {
			t.Fatal("actual cumulative terminal completion did not fence the source")
		}
	})
}

func TestAffinityRenewalWrittenErrorCannotBecomeSourceFence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		f.unpark()
		if f.offer(groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)) != 1 {
			t.Fatal("initial public SYN was not admitted")
		}
		synctest.Wait()
		if len(f.route) != 1 {
			t.Fatal("error control lacks a real written Pack")
		}
		affinitySourcesTestCloseEnrollment(f.update)
		if err := f.closeClient(context.Background()); err != nil {
			t.Fatal(err)
		}
		if f.update.affinitySourceFenced(f.selected) || f.terminal.Load() != 1 {
			t.Fatal("ambiguous written error was forgotten instead of preserving source taint")
		}
	})
}

// Distinct real barriers separate pre-retention cancellation from a serialized
// error. An empty route alone establishes neither outcome.
func testAffinityRenewalCanceledRetention(t *testing.T, budgeted bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		var admittedGroup atomic.Pointer[parsedPacketGroup]
		var wireObservations, writeReturns atomic.Int32
		var terminalFailed atomic.Bool
		var budget *TransferMemoryBudget
		if budgeted {
			budget = NewTransferMemoryBudget(kib(64))
		}
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			observe := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observe(event)
				if event.Phase == SendPackLifecyclePhaseTerminal {
					terminalFailed.Store(event.Err != nil)
				}
			}
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				admittedGroup.Store(target.(*parsedPacketGroup))
			}
			if budgeted {
				settings.SendBufferSettings.beforeGroupRetainForTest = func() {
					close(entered)
					<-release
				}
			} else {
				// Serialization and disposition have completed before this
				// existing seam; the physical writer has not yet checked ctx.
				settings.SendBufferSettings.afterTransferWriteDeadlineForTest = func(time.Time) {
					close(entered)
					<-release
				}
			}
			settings.SendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
				wireObservations.Add(1)
			}
			settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(sendSequenceId, uint64) {
				writeReturns.Add(1)
			}
		})
		t.Cleanup(resume)
		if f.offer(groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)) != 1 {
			t.Fatal("initial public SYN was not admitted")
		}
		f.requireParked()
		group := admittedGroup.Load()
		if group == nil || group.collapseAdmission.update != f.update {
			t.Fatal("fixture did not capture the actual admitted group")
		}
		affinitySourcesTestCloseEnrollment(f.update)
		if f.update.affinitySourceFenced(f.selected) {
			t.Fatal("pending raw source custody was declared unwritten terminal")
		}
		f.unpark()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("actual admitted group did not reach its cancellation barrier")
		}
		f.update.stateLock.Lock()
		source := group.collapseSource
		materializedAtBarrier := group.affinitySourceState&affinitySourceMaterialized != 0
		dequeued := group.completionFlags.Load()&groupCompletionDequeued != 0
		ownsRange := group.collapseOwner != nil
		var materializedBits uint64
		if ownsRange {
			materializedBits = group.collapseOwner.materialized
		}
		f.update.stateLock.Unlock()
		wantMaterialized := !budgeted
		wantBits := uint64(0)
		if wantMaterialized {
			wantBits = 1
		}
		if source == nil || !dequeued || materializedAtBarrier != wantMaterialized || ownsRange != wantMaterialized ||
			materializedBits != wantBits || len(f.route) != 0 || wireObservations.Load() != 0 || writeReturns.Load() != 0 {
			t.Fatalf("cancellation barrier lost its exact custody: budgeted=%t materialized=%t range=%t bits=%d",
				budgeted, materializedAtBarrier, ownsRange, materializedBits)
		}
		// Cancel only this exact source, not its shared Client or fixture.
		source.Cancel()
		resume()
		synctest.Wait()
		select {
		case <-source.done:
		default:
			t.Fatal("canceled source did not join its real callback and raw owners")
		}
		f.update.stateLock.Lock()
		entry := f.update.affinitySources.first
		materialized := group.affinitySourceState&affinitySourceMaterialized != 0
		settled := group.affinitySourceState&(affinitySourceReturned|affinitySourceTerminal|affinitySourceSettled) ==
			affinitySourceReturned|affinitySourceTerminal|affinitySourceSettled
		f.update.stateLock.Unlock()
		if f.client.IsDone() || f.terminal.Load() != 1 || !terminalFailed.Load() || len(f.route) != 0 ||
			entry.pending != 0 || !settled || materialized == budgeted || entry.ambiguous == budgeted ||
			f.update.affinitySourceFenced(f.selected) != budgeted {
			t.Fatalf("source cancellation lost its exact retention outcome: budgeted=%t materialized=%t ambiguous=%t settled=%t pending=%d terminal=%d",
				budgeted, materialized, entry.ambiguous, settled, entry.pending, f.terminal.Load())
		}
		if budgeted {
			if wireObservations.Load() != 0 || writeReturns.Load() != 0 || budget.UsedByteCount() != 0 {
				t.Fatal("canceled retained-memory admission serialized or kept a budget owner")
			}
		} else if wireObservations.Load() != 1 || writeReturns.Load() != 1 {
			t.Fatal("materialized-error control did not exercise the real canceled writer")
		}
	})
}

func TestAffinityRenewalUnwrittenQueueCancellationSettlesSource(t *testing.T) {
	testAffinityRenewalCanceledRetention(t, true)
}

func TestAffinityRenewalCanceledMaterializationKeepsSourceTaint(t *testing.T) {
	testAffinityRenewalCanceledRetention(t, false)
}

func TestAffinityRenewalCumulativeCallbackBeforePublicReturnKeepsOfferLease(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(sendGroupAdmissionTarget) {
				close(entered)
				<-release
			}
		})
		t.Cleanup(resume)
		f.unpark()
		template := groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)
		result := make(chan int, 1)
		go func() { result <- f.offer(template) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("public offer did not reach the post-enqueue producer barrier")
		}
		affinitySourcesTestCloseEnrollment(f.update)
		f.acknowledgeAll(template)
		if f.update.affinitySourceFenced(f.selected) || f.terminal.Load() != 1 {
			t.Fatal("terminal callback released an old offer whose public owner had not joined")
		}
		resume()
		if <-result != 1 || !f.update.affinitySourceFenced(f.selected) {
			t.Fatal("public return failed to release the already-completed offer")
		}
	})
}

func TestAffinityRenewalClosedEnrollmentPreservesCallerPacket(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		group := groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 0, 64, tcpFlagSyn))
		callerOwns := true
		defer func() {
			if callerOwns {
				MessagePoolReturn(group.packets[0].packet)
			}
		}()
		affinitySourcesTestCloseEnrollment(f.update)
		if accepted, err := f.selected.SendGroupDetailedWithAck(group, 0, true); accepted || err != nil {
			t.Fatalf("closed old generation changed bounded refusal: accepted=%v err=%v", accepted, err)
		}
		if group.affinitySourceRegistration != 0 || f.started.Load() != 0 || len(f.route) != 0 || f.client.IsDone() {
			t.Fatal("refused old offer acquired a source owner or tore down the shared client")
		}
		// The caller can retry the same original after all-new admission refuses.
		f.update.stateLock.Lock()
		f.update.affinitySources.closed = false
		f.update.stateLock.Unlock()
		if accepted, err := f.selected.SendGroupDetailedWithAck(group, 0, true); !accepted || err != nil {
			t.Fatalf("reopened old owner could not retry its exact packet: %v", err)
		}
		callerOwns = false
	})
}

func TestAffinityRenewalSourceSlotsAreBoundedAndEpochExact(t *testing.T) {
	parent := affinityTimeTestParent(t)
	update, _ := affinityTimeTestDonor(t, parent, 4)
	key := multiClientAffinitySourceKey{source: NewId(), destination: NewId()}
	first := &parsedPacketGroup{collapseAdmission: tcpCollapseAdmission{update: update}}
	first.affinitySourceRegistration = update.affinitySources.enrollWithLock(key)
	first.affinitySourceState = affinitySourceAdmitted | affinitySourceReturned | affinitySourceTerminal
	first.settleAffinitySourceWithLock()
	second := update.affinitySources.enrollWithLock(key)
	if second == first.affinitySourceRegistration || second == 0 {
		t.Fatal("reused source slot did not issue a new exact epoch")
	}
	first.settleAffinitySourceWithLock()
	if update.affinitySources.fencedWithLock(key) || update.affinitySources.first.pending != 1 {
		t.Fatal("duplicate old completion settled a newer same-source owner")
	}
	for index := 1; index < affinitySourceMaxCount; index++ {
		if update.affinitySources.enrollWithLock(multiClientAffinitySourceKey{source: NewId(), destination: NewId()}) == 0 {
			t.Fatalf("bounded source table refused slot %d", index)
		}
	}
	if update.affinitySources.enrollWithLock(multiClientAffinitySourceKey{source: NewId(), destination: NewId()}) != 0 ||
		1+len(update.affinitySources.spill) != affinitySourceMaxCount || cap(update.affinitySources.spill) > affinitySourceMaxCount-1 {
		t.Fatal("source history grew beyond the declared per-flow scalar bound")
	}
}

// The ordinary default races all six providers, not merely the winner. Real
// source queues remain parked, so every one is an unresolved provider owner.
func TestAffinityRenewalPublicSixSourceFanoutKeepsEveryAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixtures := make([]*groupDispositionQueueFixture, 6)
		clients := make([]*multiClientChannel, len(fixtures))
		for index := range fixtures {
			fixtures[index] = newGroupDispositionQueueFixture(t, nil, nil)
			clients[index] = fixtures[index].selected
			clients[index].healthy = true
		}
		parent := fixtures[0].parent
		parent.sendClientPathForTest = nil
		parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
		parent.ip6PathUpdates = map[Ip6Path]*multiClientChannelUpdate{}
		parent.affinityIp4Paths = map[Ip4Path]map[Ip4Path]time.Time{}
		parent.affinityIp6Paths = map[Ip6Path]map[Ip6Path]time.Time{}
		parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
		parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return clients }
		t.Cleanup(func() {
			for update := range parent.flowUpdates {
				update.Close()
			}
		})
		path := affinityTimeTestPath(4, 44101)
		template := ipOosTcpPacketSequence(path, tcpFlagSyn, path.SequenceNumber, nil)
		if fixtures[0].offer(template) != 1 {
			t.Fatal("public six-candidate SYN was not admitted")
		}
		for _, fixture := range fixtures {
			fixture.requireParked()
		}
		update := parent.receiveUpdate(path)
		if update == nil || update.client.Load() != nil {
			t.Fatal("six-way race unexpectedly committed without an inbound response")
		}
		affinitySourcesTestCloseEnrollment(update)
		for index, client := range clients {
			if update.affinitySourceFenced(client) {
				t.Fatalf("admitted losing candidate %d disappeared from source custody", index)
			}
		}
		fixtures[0].unpark()
		fixtures[0].acknowledgeAll(template)
		for index, client := range clients {
			if update.affinitySourceFenced(client) != (index == 0) {
				t.Fatalf("one cumulative fence changed another candidate's custody: %d", index)
			}
		}
	})
}
