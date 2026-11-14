// First-opening lifecycle controls exercise real quota, source and parent owners.
package connect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Pending custody is paid by real queue/root permission, while wire policy
// remains protocol-only. The original source offer remains a false receipt.
func (self *firstSynPressureFixture) requirePending(t *testing.T) (*multiClientChannelUpdate, *tcpSynRetry) {
	t.Helper()
	health := self.healthCut()
	if self.offer(t) {
		t.Fatal("full raw protocol capacity changed the original false receipt")
	}
	self.requireRefusalHealth(t, health)
	update := self.harness.update(self.path)
	if update == nil {
		t.Fatal("first native offer did not create its real flow")
	}
	update.stateLock.Lock()
	owner := update.synRetry
	valid := owner != nil && owner.openingSerial != 0 && owner.references == 1 && !owner.queued &&
		owner.credit != nil && !owner.credit.noBudget && owner.credit.byteCount > 0 &&
		owner.readiness != nil && owner.readiness.credit != nil && !owner.readiness.credit.noBudget &&
		owner.readiness.credit.byteCount > 0 && owner.readiness.count > 0 &&
		update.synWaitStart.IsZero() && !update.synGenerationSeen && !owner.publicationIndexed &&
		!update.synRetryHasRemoteRefusalWithLock()
	update.stateLock.Unlock()
	if !valid || self.refusals.Load() != 1 || self.otherError.Load() != 0 {
		t.Fatalf("first refusal did not retain one paid, unmaterialized opening: valid=%t refusals=%d other=%d",
			valid, self.refusals.Load(), self.otherError.Load())
	}
	return update, owner
}

// One paid packet and readiness owner survive the actual source handoff, then
// parent cancellation returns both exact reservations and every index reference.
func TestNativeFirstSynOpeningPaidCustodyMaterializesAndJoins(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(1024 * 1024)
		fixture := newFirstSynPressureFixture(t, 1, 51410, func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		})
		update, owner := fixture.requirePending(t)
		credit, readyCredit := owner.credit, owner.readiness.credit
		if used := budget.UsedByteCount(); used != credit.byteCount+readyCredit.byteCount {
			t.Fatalf("pending opening has uncharged or duplicate backing: used=%d want=%d", used, credit.byteCount+readyCredit.byteCount)
		}
		before := time.Now()
		fixture.base.unpark()
		acks, syns := fixture.drain(t, true)
		update.stateLock.Lock()
		materialized := update.synRetry == owner && owner.openingSerial == 0 && owner.references == 1 &&
			owner.credit == credit && owner.readiness.credit == readyCredit && update.synGenerationSeen
		update.stateLock.Unlock()
		if acks != len(fixture.acks) || syns != 1 || !materialized || time.Since(before) != 0 {
			t.Fatalf("paid opening failed exact materialization: ACKs=%d SYNs=%d materialized=%t elapsed=%s", acks, syns, materialized, time.Since(before))
		}
		if err := fixture.harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synRetryControlReleased(t, owner)
		if !credit.released.Load() || !readyCredit.released.Load() || budget.UsedByteCount() != 0 {
			t.Fatal("joined pending opening retained real byte permission")
		}
		fixture.requireLaterAck(t)
	})
}

// Exhausted optional backing preserves the documented false/source-retry
// fallback; the protocol queue still drains healthy ACKs without extra work.
func TestNativeFirstSynOpeningBackingExhaustionKeepsSourceRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(1)
		fixture := newFirstSynPressureFixture(t, 1, 51411, func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		})
		if fixture.offer(t) {
			t.Fatal("full raw source unexpectedly accepted the native offer")
		}
		update := fixture.harness.update(fixture.path)
		update.stateLock.Lock()
		empty := update.synRetry == nil && update.synWaitStart.IsZero() && !update.synGenerationSeen
		update.stateLock.Unlock()
		if !empty || budget.UsedByteCount() != 0 || fixture.refusals.Load() != 1 {
			t.Fatal("exhausted optional quota retained custody or created a source clock")
		}
		before := time.Now()
		fixture.base.unpark()
		acks, syns := fixture.drain(t, true)
		if acks != len(fixture.acks) || syns != 0 || fixture.retries.Load() != 0 || time.Since(before) != 0 {
			t.Fatal("unpaid opening retried or blocked healthy feedback")
		}
		if !fixture.offer(t) {
			t.Fatal("reopened protocol source refused the caller's later SYN")
		}
		acks, syns = fixture.drain(t, true)
		if acks != 0 || syns != 1 || fixture.native.Load() != 2 || budget.UsedByteCount() != 0 {
			t.Fatal("ordinary protocol source retry acquired optional DATA permission")
		}
	})
}

// Parent closure retires pending custody before the independent source opens.
func TestNativeFirstSynOpeningParentCloseReturnsBacking(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, 1, 51412)
		_, owner := fixture.requirePending(t)
		credit, readyCredit := owner.credit, owner.readiness.credit
		if err := fixture.harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synRetryControlReleased(t, owner)
		if !credit.released.Load() || !readyCredit.released.Load() {
			t.Fatal("parent close retained first-opening permission")
		}
		fixture.base.unpark()
		acks, syns := fixture.drain(t, true)
		if acks != len(fixture.acks) || syns != 0 || fixture.retries.Load() != 0 {
			t.Fatal("source opening revived canceled first-SYN work")
		}
		fixture.requireLaterAck(t)
	})
}

// Closing the final watched source releases a never-materialized cache now,
// rather than keeping its byte reservation for the TCP idle lifetime.
func TestNativeFirstSynOpeningSourceCloseReturnsBacking(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, 1, 51413)
		update, owner := fixture.requirePending(t)
		credit, readyCredit := owner.credit, owner.readiness.credit
		fixture.source.Cancel()
		fixture.base.unpark()
		synctest.Wait()
		update.stateLock.Lock()
		empty := update.synRetry == nil
		update.stateLock.Unlock()
		if !empty || !credit.released.Load() || !readyCredit.released.Load() || len(fixture.base.route) != 0 {
			t.Fatal("closed last source retained or emitted its pending opening")
		}
		affinitySourceCancellationHealthySibling(t, fixture.base)
	})
}

// A real opening before watch installation still grants one event-driven turn.
func TestNativeFirstSynOpeningReadyBeforeWatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, 1, 51414)
		var once sync.Once
		fixture.harness.multiClient.beforeSynRetryReadinessArmForTest = func(*multiClientChannelUpdate) {
			once.Do(func() {
				fixture.base.unpark()
				synctest.Wait()
			})
		}
		before := time.Now()
		if fixture.offer(t) {
			t.Fatal("recovery changed the original raw refusal receipt")
		}
		acks, syns := fixture.drain(t, true)
		if acks != len(fixture.acks) || syns != 1 || fixture.native.Load() != 1 || time.Since(before) != 0 {
			t.Fatalf("ready-before-watch lost its bounded turn: ACKs=%d SYNs=%d native=%d elapsed=%s", acks, syns, fixture.native.Load(), time.Since(before))
		}
	})
}

// A later refused reset must retire the never-materialized SYN immediately.
func TestNativeFirstSynOpeningRefusedResetSupersedes(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, 1, 51415)
		update, owner := fixture.requirePending(t)
		credit, readyCredit := owner.credit, owner.readiness.credit
		reset := MessagePoolCopy(reraceTcpPacket(fixture.path, tcpFlagRst|tcpFlagAck, fixture.path.SequenceNumber+1, 1, nil))
		fixture.base.witnesses = append(fixture.base.witnesses, MessagePoolShareReadOnly(reset))
		if synRetryControlOffer(t, fixture.harness, reset) {
			t.Fatal("reset premise did not encounter full raw capacity")
		}
		update.stateLock.Lock()
		empty := update.synRetry == nil && !update.sourceReset.Load()
		update.stateLock.Unlock()
		if !empty || !credit.released.Load() || !readyCredit.released.Load() {
			t.Fatal("refused reset left stale pending SYN custody or invented source reset")
		}
		fixture.base.unpark()
		acks, syns := fixture.drain(t, true)
		if acks != len(fixture.acks) || syns != 0 || fixture.base.selected.dialFailureCount() != 0 || fixture.base.selected.IsDone() {
			t.Fatal("superseded opening emitted stale wire or penalized its provider")
		}
		fixture.requireLaterAck(t)
	})
}

// Native sequence numbers are identities, not ordered integers. A newer offer
// replaces only pending custody, including when its ISN is numerically smaller.
func TestNativeFirstSynOpeningRefusedNewSequenceSupersedes(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, 1, 51416)
		update, old := fixture.requirePending(t)
		credit, readyCredit := old.credit, old.readiness.credit
		fixture.syn = reraceTcpPacket(fixture.path, tcpFlagSyn, fixture.path.SequenceNumber-1, 0, nil)
		if fixture.offer(t) {
			t.Fatal("new sequence did not encounter the same full raw source")
		}
		update.stateLock.Lock()
		fresh := update.synRetry != nil && update.synRetry != old && update.synRetry.sequence == fixture.path.SequenceNumber-1 && !update.synGenerationSeen
		update.stateLock.Unlock()
		if !fresh || !credit.released.Load() || !readyCredit.released.Load() {
			t.Fatal("new offered sequence borrowed an older pending owner")
		}
		fixture.base.unpark()
		acks, syns := fixture.drain(t, true)
		if acks != len(fixture.acks) || syns != 1 || fixture.native.Load() != 2 {
			t.Fatal("source opening did not emit exactly the latest native sequence")
		}
	})
}

// A source-owned barrier pins either the admitted or dequeued opening before
// its real materialization. Cleanup releases that exact worker before joining.
func testNativeFirstSynOpeningSourceBoundary(t *testing.T, boundary string, reset bool, port int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		defer finish()
		var visits atomic.Int64
		pause := func(ctx context.Context) {
			if visits.Add(1) == 2 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		fixture := newFirstSynPressureFixture(t, 1, port, func(settings *ClientSettings) {
			if boundary == "dequeue" {
				settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) { pause(source.ctx) }
			} else {
				settings.SendBufferSettings.beforeGroupRetainForTest = func() { pause(t.Context()) }
			}
		})
		update, owner := fixture.requirePending(t)
		credit, readyCredit := owner.credit, owner.readiness.credit
		before := time.Now()
		fixture.base.unpark()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("first opening did not reach the actual source boundary")
		}
		fixture.harness.multiClient.notifyFlowReaper()
		synctest.Wait()
		update.stateLock.Lock()
		pending := update.synRetry == owner && owner.openingSerial != 0 && !owner.publicationIndexed &&
			!update.synRetryHasRemoteRefusalWithLock() && update.client.Load() == fixture.base.selected
		generationSeen := update.synGenerationSeen
		update.stateLock.Unlock()
		if !pending || credit.released.Load() || readyCredit.released.Load() || generationSeen != (boundary == "retain") {
			t.Fatalf("own admitted opening lost custody or gained materialization authority: pending=%t generation=%t boundary=%s", pending, generationSeen, boundary)
		}
		if reset {
			packet := MessagePoolCopy(reraceTcpPacket(fixture.path, tcpFlagRst|tcpFlagAck, fixture.path.SequenceNumber+1, 1, nil))
			fixture.base.witnesses = append(fixture.base.witnesses, MessagePoolShareReadOnly(packet))
			if synRetryControlOffer(t, fixture.harness, packet) {
				t.Fatal("later reset unexpectedly bypassed the held raw owner")
			}
			if !credit.released.Load() || !readyCredit.released.Load() {
				t.Fatal("later refused reset retained a pending opening")
			}
		}
		finish()
		acks, syns := fixture.drain(t, true)
		wantSyns := 1
		if reset {
			wantSyns = 0
		}
		if acks != len(fixture.acks) || syns != wantSyns || time.Since(before) != 0 || fixture.base.selected.dialFailureCount() != 0 || fixture.base.selected.IsDone() {
			t.Fatalf("source boundary changed exact custody: ACKs=%d SYNs=%d want=%d elapsed=%s", acks, syns, wantSyns, time.Since(before))
		}
		fixture.requirePeerAckIdle(t, wantSyns, before)
		fixture.requireLaterAck(t)
	})
}

// Producer commit precedes dequeue deterministically, so an own pending SYN
// cannot be mistaken for a conflicting reset in the source-consumer guard.
func TestNativeFirstSynOpeningOwnAdmittedClaimSurvivesDequeue(t *testing.T) {
	testNativeFirstSynOpeningSourceBoundary(t, "dequeue", false, 51417)
}

// Dequeue can advance the source epoch before materialization. That legitimate
// transition must neither retire the owner nor grant remote-failure authority.
func TestNativeFirstSynOpeningOwnDequeuePreservesPendingAuthority(t *testing.T) {
	testNativeFirstSynOpeningSourceBoundary(t, "retain", false, 51418)
}

// Supersession after dequeue but before ordinal assignment disposes only the
// unwritten retry, without false peer receipts or an innocent provider strike.
func TestNativeFirstSynOpeningResetBeforeOrdinalDisposesLocally(t *testing.T) {
	testNativeFirstSynOpeningSourceBoundary(t, "retain", true, 51419)
}

// A competitor can steal a real granted opening. The pending SYN re-arms its
// exact gate and progresses on the competitor's release, without timer polling.
func TestNativeFirstSynOpeningStolenVacancyRearms(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		readyEntered, readyRelease := make(chan struct{}), make(chan struct{})
		sourceEntered, sourceRelease := make(chan struct{}), make(chan struct{})
		var readyOnce, sourceOnce sync.Once
		finishReady := func() { readyOnce.Do(func() { close(readyRelease) }) }
		finishSource := func() { sourceOnce.Do(func() { close(sourceRelease) }) }
		defer finishReady()
		defer finishSource()
		var dequeued atomic.Int64
		fixture := newFirstSynPressureFixture(t, 1, 51420, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if dequeued.Add(1) == 2 {
					close(sourceEntered)
					select {
					case <-sourceRelease:
					case <-source.ctx.Done():
					}
				}
			}
		})
		var held atomic.Bool
		fixture.harness.multiClient.beforeSynRetryReadinessForTest = func(*multiClientChannelUpdate) {
			if held.CompareAndSwap(false, true) {
				close(readyEntered)
				select {
				case <-readyRelease:
				case <-fixture.harness.multiClient.ctx.Done():
				}
			}
		}
		update, owner := fixture.requirePending(t)
		before := time.Now()
		fixture.base.unpark()
		synctest.Wait()
		select {
		case <-readyEntered:
		default:
			t.Fatal("first opening did not reach its actual granted readiness turn")
		}
		competitor := groupRecoveryPacket(100, 601, 80, tcpFlagAck, nil)
		fixture.acks = append(fixture.acks, competitor)
		if fixture.base.offer(competitor) != 1 {
			t.Fatal("real competitor did not take the granted opening")
		}
		synctest.Wait()
		select {
		case <-sourceEntered:
		default:
			t.Fatal("competitor did not reach its actual source dequeue")
		}
		gate := fixture.requireRawProtocolHeld(t)
		// The first ACK still owns the sole serialized immediate slot. Its
		// actual receipt permits the competitor to write after the raw hold.
		acks, syns := fixture.drain(t, true)
		if acks != 1 || syns != 0 {
			t.Fatalf("held competitor changed initial peer receipt: ACKs=%d SYNs=%d", acks, syns)
		}
		finishReady()
		synctest.Wait()
		update.stateLock.Lock()
		rearmed := update.synRetry == owner && owner.openingSerial != 0 && owner.readiness.indexed.Load()
		update.stateLock.Unlock()
		if !rearmed || gate.waiterCount.Load() != 1 || fixture.retries.Load() != 1 || fixture.refusals.Load() != 2 {
			t.Fatalf("stolen real opening did not re-arm once: rearmed=%t waiters=%d retries=%d refusals=%d", rearmed, gate.waiterCount.Load(), fixture.retries.Load(), fixture.refusals.Load())
		}
		for range 4 {
			fixture.harness.multiClient.notifyFlowReaper()
			synctest.Wait()
		}
		if fixture.retries.Load() != 1 || fixture.refusals.Load() != 2 || !gate.Load() {
			t.Fatal("closed rearmed raw protocol capacity spun another retry")
		}
		finishSource()
		acks, syns = fixture.drain(t, true)
		if acks != len(fixture.acks)-1 || syns != 1 || fixture.retries.Load() != 2 || fixture.native.Load() != 1 || time.Since(before) != 0 {
			t.Fatal("competitor release lost pending setup or required another source timer")
		}
		fixture.requirePeerAckIdle(t, 1, before)
		fixture.requireLaterAck(t)
		fixture.requirePeerAckIdle(t, 1, before)
	})
}

// A held actual source dequeue must retain its raw admission and close the
// same readiness gate; no test writes either count or availability state.
func (self *firstSynPressureFixture) requireRawProtocolHeld(t *testing.T) *resendCapacityGate {
	t.Helper()
	self.source.protocolAdmission.mutex.Lock()
	count, capacity := self.source.protocolAdmission.count, self.source.protocolAdmission.capacity
	gate := self.source.protocolAdmission.protocolReadiness
	self.source.protocolAdmission.mutex.Unlock()
	if count != 1 || capacity != 1 || gate == nil || !gate.Load() {
		t.Fatalf("actual competitor lost raw protocol custody: count=%d capacity=%d gate=%p", count, capacity, gate)
	}
	return gate
}

// Parallel candidates share one tentative request. A real refused candidate
// pays first, then a different candidate admits; overall success releases the
// tentative backing and cannot publish a second autonomous original.
func TestNativeFirstSynOpeningWideAdmissionDiscardsTentativeOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(1024 * 1024)
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		var recovered atomic.Int64
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		defer finish()
		first := newFirstSynPressureFixture(t, 1, 51421, func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = budget
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			previous := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				previous(event)
				var admission *SendPackAdmissionError
				if event.Phase == SendPackLifecyclePhaseTerminal && errors.As(event.Err, &admission) &&
					admission.Boundary == "pack-admission" && admission.RecoveredByOwner {
					recovered.Add(1)
				}
			}
		})
		second := newFirstSynPressureFixture(t, 1, 51422)
		second.syn = first.syn
		second.base.unpark()
		acks, syns := second.drain(t, true)
		if acks != len(second.acks) || syns != 0 || budget.UsedByteCount() != 0 {
			t.Fatal("second real candidate did not establish vacant protocol capacity")
		}
		second.base.selected.beforeGroupCompletionForTest = func() {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-second.base.selected.ctx.Done():
			}
		}
		first.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.base.selected, second.base.selected}
		}
		before := time.Now()
		packet := MessagePoolCopy(first.syn)
		first.base.witnesses = append(first.base.witnesses, MessagePoolShareReadOnly(packet))
		accepted, count := [1]bool{}, 0
		done := make(chan struct{})
		go func() {
			defer close(done)
			count = first.harness.multiClient.SendNativePacketBatchWithResults(first.harness.source,
				first.harness.multiClient.provideMode, [][]byte{packet}, 0, accepted[:])
		}()
		defer func() {
			finish()
			if t.Failed() {
				first.harness.multiClient.cancel()
			}
			<-done
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("healthy wide candidate did not reach its independent admission barrier")
		}
		// The first source remains parked with only its original raw ACK.
		// Its initially empty retained budget can now belong only to the
		// refused candidate's tentative opening. Typed terminal diagnostics
		// are intentionally deferred until both candidate calls return.
		first.base.requireParked()
		peak := budget.UsedByteCount()
		update := first.harness.update(first.path)
		if update == nil {
			t.Fatal("wide original did not create its actual flow")
		}
		update.stateLock.Lock()
		unpublished := update.synRetry == nil && !update.synGenerationSeen && update.synWaitStart.IsZero()
		update.stateLock.Unlock()
		if peak <= 0 || !unpublished || first.refusals.Load() != 0 || first.retries.Load() != 0 {
			t.Fatalf("held wide attempt lacks unpublished real backing: peak=%d unpublished=%t refused=%d retry=%d", peak, unpublished, first.refusals.Load(), first.retries.Load())
		}
		finish()
		<-done
		synctest.Wait()
		if count != 1 || !accepted[0] || first.native.Load() != 1 {
			t.Fatalf("real wide original lost public custody: count=%d accepted=%t native=%d", count, accepted[0], first.native.Load())
		}
		if budget.UsedByteCount() != 0 || first.refusals.Load() != 1 || recovered.Load() != 1 || first.retries.Load() != 0 {
			t.Fatalf("wide success leaked tentative custody or its deferred result: peak=%d remaining=%d refused=%d recovered=%d retry=%d", peak, budget.UsedByteCount(), first.refusals.Load(), recovered.Load(), first.retries.Load())
		}
		acks, syns = second.drain(t, true)
		if acks != 0 || syns != 1 || time.Since(before) != 0 {
			t.Fatal("wide admitted candidate did not own exactly its physical original")
		}
		first.base.unpark()
		acks, syns = first.drain(t, true)
		if acks != len(first.acks) || syns != 0 || first.retries.Load() != 0 {
			t.Fatal("successful wide original published a tentative first-SYN retry")
		}
	})
}

// Both real candidates refuse, and only the second source opens. All observed
// gates must remain represented; the first source cannot block the other's turn.
func TestNativeFirstSynOpeningWideSecondSourceOpens(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first := newFirstSynPressureFixture(t, 1, 51423)
		second := newFirstSynPressureFixture(t, 1, 51424)
		second.syn = first.syn
		first.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.base.selected, second.base.selected}
		}
		update, owner := first.requirePending(t)
		credit, readyCredit := owner.credit, owner.readiness.credit
		update.stateLock.Lock()
		ready := owner.readiness
		both := ready.count == 2 && len(ready.slots) >= 2 &&
			((ready.slots[0].source == first.source && ready.slots[1].source == second.source) ||
				(ready.slots[0].source == second.source && ready.slots[1].source == first.source))
		update.stateLock.Unlock()
		if !both || second.refusals.Load() != 1 {
			t.Fatal("wide first refusal lost one actual source gate")
		}
		before := time.Now()
		second.base.unpark()
		acks, syns := second.drain(t, true)
		if acks != len(second.acks) || syns != 1 || len(first.base.route) != 0 || first.native.Load() != 1 || first.retries.Load() != 1 || time.Since(before) != 0 {
			t.Fatalf("second source opening waited for the first or a timer: ACKs=%d SYNs=%d firstWire=%d native=%d retry=%d elapsed=%s", acks, syns, len(first.base.route), first.native.Load(), first.retries.Load(), time.Since(before))
		}
		if err := first.harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !credit.released.Load() || !readyCredit.released.Load() {
			t.Fatal("joined wide opening retained paid packet or gate state")
		}
		first.base.unpark()
		acks, syns = first.drain(t, true)
		if acks != len(first.acks) || syns != 0 {
			t.Fatal("closed wide attempt revived through its formerly full first source")
		}
	})
}

// An old granted turn may finish after a newer native sequence published its
// own pending owner. Cleanup must name the old readiness identity, never retire
// the current owner merely because that old registration is no longer valid.
func TestNativeFirstSynOpeningOldGrantCannotRetireNewPendingOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		oldEntered, oldRelease := make(chan struct{}), make(chan struct{})
		sourceEntered, sourceRelease := make(chan struct{}), make(chan struct{})
		var oldOnce, sourceOnce sync.Once
		finishOld := func() { oldOnce.Do(func() { close(oldRelease) }) }
		finishSource := func() { sourceOnce.Do(func() { close(sourceRelease) }) }
		defer finishOld()
		defer finishSource()
		var dequeued atomic.Int64
		fixture := newFirstSynPressureFixture(t, 1, 51425, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if dequeued.Add(1) == 2 {
					close(sourceEntered)
					select {
					case <-sourceRelease:
					case <-source.ctx.Done():
					}
				}
			}
		})
		var held atomic.Bool
		fixture.harness.multiClient.beforeSynRetryReadinessForTest = func(*multiClientChannelUpdate) {
			if held.CompareAndSwap(false, true) {
				close(oldEntered)
				select {
				case <-oldRelease:
				case <-fixture.harness.multiClient.ctx.Done():
				}
			}
		}
		update, old := fixture.requirePending(t)
		oldCredit, oldReadyCredit := old.credit, old.readiness.credit
		before := time.Now()
		fixture.base.unpark()
		synctest.Wait()
		select {
		case <-oldEntered:
		default:
			t.Fatal("old readiness did not reach its actual granted-turn barrier")
		}
		competitor := groupRecoveryPacket(100, 601, 80, tcpFlagAck, nil)
		fixture.acks = append(fixture.acks, competitor)
		if fixture.base.offer(competitor) != 1 {
			t.Fatal("real competitor could not occupy the old granted opening")
		}
		synctest.Wait()
		select {
		case <-sourceEntered:
		default:
			t.Fatal("competitor did not hold its actual raw source custody")
		}
		fixture.requireRawProtocolHeld(t)
		acks, syns := fixture.drain(t, true)
		if acks != 1 || syns != 0 {
			t.Fatalf("held competitor changed initial peer receipt: ACKs=%d SYNs=%d", acks, syns)
		}
		fixture.syn = reraceTcpPacket(fixture.path, tcpFlagSyn, fixture.path.SequenceNumber-1, 0, nil)
		if fixture.offer(t) {
			t.Fatal("new native sequence unexpectedly bypassed the real competitor")
		}
		update.stateLock.Lock()
		fresh := update.synRetry
		valid := fresh != nil && fresh != old && fresh.openingSerial != 0 && fresh.readiness.indexed.Load()
		update.stateLock.Unlock()
		if !valid || !oldCredit.released.Load() || oldReadyCredit.released.Load() {
			t.Fatal("new pending owner failed to coexist with the active old readiness reference")
		}
		finishOld()
		synctest.Wait()
		update.stateLock.Lock()
		survived := update.synRetry == fresh && fresh.openingSerial != 0 && fresh.readiness.indexed.Load() &&
			!fresh.credit.released.Load() && !fresh.readiness.credit.released.Load()
		update.stateLock.Unlock()
		if !survived || !oldReadyCredit.released.Load() {
			t.Fatal("old grant cleanup retired the newer pending opening")
		}
		finishSource()
		acks, syns = fixture.drain(t, true)
		if acks != len(fixture.acks)-1 || syns != 1 || fixture.native.Load() != 2 || fixture.retries.Load() != 1 || time.Since(before) != 0 {
			t.Fatal("new pending owner failed to consume its own later source opening")
		}
		fixture.requirePeerAckIdle(t, 1, before)
	})
}
