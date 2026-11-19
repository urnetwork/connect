// Protocol readiness controls preserve the existing native retry lifecycle.
// Real source writes release slots; hooks control only the disputed ordering.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type nativeSynProtocolReadyFixture struct {
	base    *groupDispositionQueueFixture
	source  *SendSequence
	control *synRetryReadyControl
	ack     []byte
}

// One real pure ACK occupies the constructor's count slot before Run. The
// native original is separately retained by the ordinary two-provider harness.
func newNativeSynProtocolReadyFixture(t *testing.T, port int, configure func(*ClientSettings)) *nativeSynProtocolReadyFixture {
	t.Helper()
	f := &nativeSynProtocolReadyFixture{}
	f.base = newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
		settings.SendBufferSettings.SequenceBufferSize = 1
		if configure != nil {
			configure(settings)
		}
	})
	f.base.selected.settings = f.base.parent.settings
	f.ack = groupRecoveryPacket(100, 500, 64, tcpFlagAck, nil)
	if f.base.offer(f.ack) != 1 {
		t.Fatal("protocol fixture failed to admit its real ACK")
	}
	f.base.requireParked()
	f.source = f.base.update.sequenceClaims.collapseSource
	if f.source == nil {
		t.Fatal("real ACK did not retain its source")
	}
	f.source.protocolAdmission.mutex.Lock()
	full := f.source.protocolAdmission.count == f.source.protocolAdmission.capacity && f.source.protocolAdmission.count == 1
	f.source.protocolAdmission.mutex.Unlock()
	if !full || f.source.resendCapacityUnavailable.Load() || f.source.resendQueue.Len() != 0 {
		t.Fatal("protocol count fixture borrowed paid resend pressure")
	}
	f.control = newSynRetryReadyControl(t, port)
	if !f.control.owner.credit.noBudget || !protocolClassPureTcpControl(f.control.original) {
		t.Fatal("native protocol control unexpectedly contains DATA")
	}
	f.control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
		return []*multiClientChannel{f.base.selected}
	}
	return f
}

// Inspect only after quiescence; lifecycle hooks never fabricate a gate state.
func (f *nativeSynProtocolReadyFixture) refused(t *testing.T) *resendCapacityGate {
	t.Helper()
	f.control.harness.inject("a", reraceProviderSignal(t, f.control.path))
	synctest.Wait()
	f.source.protocolAdmission.mutex.Lock()
	gate := f.source.protocolAdmission.protocolReadiness
	full := f.source.protocolAdmission.count == f.source.protocolAdmission.capacity
	f.source.protocolAdmission.mutex.Unlock()
	f.control.update.stateLock.Lock()
	ready := f.control.owner.readiness
	valid := f.control.update.synRetry == f.control.owner && f.control.owner.references == 1 &&
		!f.control.owner.queued && f.control.update.client.Load() == nil && ready != nil &&
		ready.indexed.Load() && ready.count == 1 && ready.references.Load() == 2
	f.control.update.stateLock.Unlock()
	if gate == nil || !full || !gate.Load() || gate.waiterCount.Load() != 1 || !valid {
		t.Fatal("actual pure SYN refusal did not retain one quiet exact protocol readiness watch")
	}
	return gate
}

// The public RED already checks exact packet custody. These adjacent controls
// also pin all separately prepaid readiness references at final retirement.
func (f *nativeSynProtocolReadyFixture) close(t *testing.T, ready *tcpSynRetryReadiness, paid *transferRetainedControlCredit, gate *resendCapacityGate) {
	t.Helper()
	f.control.close(t)
	requireSynRetryReadyRetired(t, ready, paid)
	if gate != nil {
		testingCapacityEmpty(t, gate)
	}
}

func TestNativeSynRetryProtocolReleaseBeforeArmKeepsOpening(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynProtocolReadyFixture(t, 51331, nil)
		ready, paid := f.control.owner.readiness, f.control.owner.readiness.credit
		entered, release := synRetryReadyHold(t, f.control.harness.multiClient, true)
		started, before := f.base.started.Load(), time.Now()
		f.control.harness.inject("a", reraceProviderSignal(t, f.control.path))
		synRetryControlEntered(t, entered)
		gate := f.source.protocolAdmission.protocolReadinessGate()
		if gate.waiterCount.Load() != 0 || f.base.started.Load() != started+1 {
			t.Fatal("arm barrier did not follow exactly one real protocol refusal")
		}
		f.base.unpark()
		f.base.acknowledgeAll(f.ack)
		if gate.Load() {
			t.Fatal("real source did not publish vacancy before watch registration")
		}
		release()
		synctest.Wait()
		if f.base.started.Load() != started+2 || f.control.harness.boundTo(f.control.path) != f.base.selected {
			t.Fatal("protocol opening before arm lost the only retained retry")
		}
		f.base.acknowledgeAll(f.control.original)
		if time.Since(before) != 0 {
			t.Fatal("late registration depended on a native retransmission clock")
		}
		f.close(t, ready, paid, gate)
	})
}

// Zero-time ACK producers retain their existing immediate offer. Stealing a
// vacancy after a directed grant must rearm, not spin or lose the next opening.
func TestNativeSynRetryProtocolStolenVacancyRearmsWithoutSpin(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		secondEntered, secondRelease := make(chan struct{}, 1), make(chan struct{})
		var secondOnce sync.Once
		unparkSecond := func() { secondOnce.Do(func() { close(secondRelease) }) }
		t.Cleanup(unparkSecond)
		var dequeued atomic.Int32
		f := newNativeSynProtocolReadyFixture(t, 51332, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if dequeued.Add(1) != 2 {
					return
				}
				secondEntered <- struct{}{}
				select {
				case <-secondRelease:
				case <-source.ctx.Done():
				}
			}
		})
		gate := f.refused(t)
		ready, paid := f.control.owner.readiness, f.control.owner.readiness.credit
		entered, release := synRetryReadyHold(t, f.control.harness.multiClient, false)
		before := time.Now()
		f.base.unpark()
		synctest.Wait()
		synRetryControlEntered(t, entered)
		second := groupRecoveryPacket(100, 501, 65, tcpFlagAck, nil)
		if f.base.offer(second) != 1 {
			t.Fatal("real unrelated ACK did not take the granted retry's unreserved vacancy")
		}
		synctest.Wait()
		select {
		case <-secondEntered:
		default:
			t.Fatal("second real ACK was not held at source dequeue")
		}
		if !gate.Load() {
			t.Fatal("actual second ACK did not reclose protocol count capacity")
		}
		// The one-immediate source still owns the first physical ACK. Feed its
		// exact peer receipt while the second dequeue is held; after release,
		// that real receipt permits the second ACK's write and slot retirement.
		f.base.acknowledgeAll(f.ack)
		started := f.base.started.Load()
		release()
		synctest.Wait()
		if f.base.started.Load() != started+1 || !ready.indexed.Load() || gate.waiterCount.Load() != 1 ||
			f.control.harness.boundTo(f.control.path) != nil {
			t.Fatal("stolen vacancy failed to refuse once and rearm exact protocol readiness")
		}
		for range 4 {
			f.control.harness.multiClient.notifyFlowReaper()
			synctest.Wait()
		}
		if f.base.started.Load() != started+1 {
			t.Fatal("closed rearmed protocol readiness spun another native retry")
		}
		unparkSecond()
		synctest.Wait()
		if f.base.started.Load() != started+2 || f.control.harness.boundTo(f.control.path) != f.base.selected {
			t.Fatal("next actual vacancy did not reoffer the original without native retransmission")
		}
		f.base.acknowledgeAll(second, f.control.original)
		if time.Since(before) != 0 || f.base.selected.dialFailureCount() != 0 {
			t.Fatal("stolen vacancy changed source health or depended on a clock")
		}
		f.close(t, ready, paid, gate)
	})
}

func TestNativeSynRetryProtocolSourceCloseWithdrawsWatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynProtocolReadyFixture(t, 51333, nil)
		gate := f.refused(t)
		ready, paid := f.control.owner.readiness, f.control.owner.readiness.credit
		started := f.base.started.Load()
		f.source.Cancel()
		f.base.unpark()
		synctest.Wait()
		if f.base.started.Load() != started || !gate.closed.Load() || ready.indexed.Load() || ready.count != 0 {
			t.Fatal("closed protocol source retried or retained a directed watch")
		}
		testingCapacityEmpty(t, gate)
		f.close(t, ready, paid, gate)
	})
}

func TestNativeSynRetryProtocolGrantedTurnRejectsNewGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynProtocolReadyFixture(t, 51334, nil)
		gate := f.refused(t)
		ready, paid, packetCredit := f.control.owner.readiness, f.control.owner.readiness.credit, f.control.owner.credit
		entered, release := synRetryReadyHold(t, f.control.harness.multiClient, false)
		f.base.unpark()
		synctest.Wait()
		synRetryControlEntered(t, entered)
		fresh := *f.control.path
		fresh.SequenceNumber++
		if !privateStaleBoundNativeSyn(t, f.control.harness, &fresh) {
			t.Fatal("fresh native generation did not take actual protocol capacity")
		}
		if !packetCredit.released.Load() || paid.released.Load() || ready.references.Load() != 1 {
			t.Fatal("superseded ready worker failed to retain only its independent metadata reference")
		}
		started := f.base.started.Load()
		release()
		synctest.Wait()
		if f.base.started.Load() != started {
			t.Fatal("old protocol ready turn wrote across the actual native generation change")
		}
		f.base.acknowledgeAll(f.ack, reraceTcpPacket(&fresh, tcpFlagSyn, fresh.SequenceNumber, 0, nil))
		requireSynRetryReadyRetired(t, ready, paid)
		if err := f.control.harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		testingCapacityEmpty(t, gate)
	})
}

func TestNativeSynRetryProtocolGrantedTurnRejectsOtherWinner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeSynProtocolReadyFixture(t, 51335, nil)
		gate := f.refused(t)
		ready, paid := f.control.owner.readiness, f.control.owner.readiness.credit
		entered, release := synRetryReadyHold(t, f.control.harness.multiClient, false)
		f.base.unpark()
		synctest.Wait()
		synRetryControlEntered(t, entered)
		other := f.control.harness.channel("b")
		f.control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{other}
		}
		if !privateStaleBoundNativeSyn(t, f.control.harness, f.control.path) {
			t.Fatal("actual native offer failed to reach the other healthy provider")
		}
		f.control.harness.inject("b", reraceSynAck(f.control.path))
		if f.control.harness.boundTo(f.control.path) != other || !f.control.update.receivedInbound.Load() {
			t.Fatal("real other-provider response did not publish its winning flow")
		}
		started := f.base.started.Load()
		release()
		synctest.Wait()
		if f.base.started.Load() != started {
			t.Fatal("late protocol grant reopened a flow already answered by another provider")
		}
		f.base.acknowledgeAll(f.ack)
		requireSynRetryReadyRetired(t, ready, paid)
		f.control.close(t)
		testingCapacityEmpty(t, gate)
	})
}
