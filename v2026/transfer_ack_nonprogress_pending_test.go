// A receiver may repeat a truthful old receipt without advancing delivery.
// Such feedback must remain sendable but cannot continually preempt a due hole.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAckRepeatedHeadDoesNotBecomePendingProgress(t *testing.T) {
	testAckNonprogressPending(t, false)
}

func TestAckPastSelectiveDoesNotBecomePendingProgress(t *testing.T) {
	testAckNonprogressPending(t, true)
}

func testAckNonprogressPending(t *testing.T, selective bool) {
	t.Helper()
	window := newSequenceAckWindow()
	head := sequenceAck{sequenceNumber: 21, messageId: NewId(), unwrapped: true}
	window.Update(head)
	if !window.PendingCumulativeProgress() {
		t.Fatal("first actual cumulative head lost its progress signal")
	}
	window.Snapshot(true)
	feedback := head
	if selective {
		feedback.sequenceNumber, feedback.messageId, feedback.selective = 20, NewId(), true
	}
	window.Update(feedback)
	if window.PendingCumulativeProgress() {
		t.Errorf("ACK_NONPROGRESS_COUNTS_AS_PROGRESS selective=%t: already-applied head21 did not advance", selective)
	}
	response := window.Snapshot(true)
	if response.ackUpdateCount != 1 || response.headAck.sequenceNumber != 21 || response.headAck.messageId != head.messageId || !response.headAck.unwrapped {
		t.Fatal("excluding nonprogress must preserve the truthful repeated head response")
	}
}

func TestAckAdvancingHeadKeepsPendingProgress(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{sequenceNumber: 21, messageId: NewId()})
	window.Snapshot(true)
	window.Update(sequenceAck{sequenceNumber: 22, messageId: NewId()})
	if !window.PendingCumulativeProgress() {
		t.Fatal("new cumulative head must preempt stale due recovery before application")
	}
	window.Snapshot(true)
	if window.PendingCumulativeProgress() {
		t.Fatal("drained cumulative progress remained pending")
	}
}

func TestH1RepeatedHeadCannotStarveDuePublicRecovery(t *testing.T) {
	testH1NonprogressCannotStarveDueRecovery(t, false)
}

func TestH1PastSelectiveCannotStarveDuePublicRecovery(t *testing.T) {
	testH1NonprogressCannotStarveDueRecovery(t, true)
}

func testH1NonprogressCannotStarveDueRecovery(t *testing.T, selective bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		type dueEvent struct {
			release chan struct{}
			at      time.Time
		}
		due := make(chan dueEvent, 1)
		var fixture *windowRoundFixture
		var targetNumber uint64
		var armed atomic.Bool
		fixture = newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
				if !armed.Load() || fixture == nil || id.Destination != fixture.receiver.ClientId() || number != targetNumber {
					return
				}
				sequence := fixture.sender.sendBuffer.lookupSendSequence(id, nil)
				if sequence == nil {
					return
				}
				item := sequence.resendQueue.PeekFirst()
				if item == nil || item.sequenceNumber != number || item.recoveryKind != sendRecoveryNone {
					return
				}
				now := time.Now()
				if sequence.shouldDeferTimeoutResend(item, sequence.rttWindow.ScaledRtt()) {
					return
				}
				if service := sequence.windowPacer.service; service != nil {
					deadline := service.probeRecoveryDeadline(sequence.sequenceId, item.messageId,
						sequence.sendBufferSettings.RttScale, sequence.sendBufferSettings.MaxResendInterval)
					if interval := sequence.sharedRawRecoveryInterval(item, now); interval > 0 && item.pacingSentAtNanos != 0 {
						physical := sequence.firstPhysicalRecoveryTime(item).Add(max(interval, sequence.resendIntervalForItem(item, 1)))
						if deadline.Before(physical) {
							deadline = physical
						}
					}
					if now.Before(deadline) {
						return
					}
				}
				event := dueEvent{release: make(chan struct{}), at: now}
				select {
				case due <- event:
				case <-fixture.ctx.Done():
					return
				}
				select {
				case <-event.release:
				case <-fixture.ctx.Done():
				}
			}
		}, nil, TransportTypeH1)
		settings := fixture.sender.settings.SendBufferSettings
		defaults := DefaultSendBufferSettings()
		if settings.ReliableLaneProvenRecovery || settings.MinResendInterval != defaults.MinResendInterval ||
			settings.RttMinResendInterval != defaults.RttMinResendInterval || settings.AckTimeout != defaults.AckTimeout ||
			!settings.DeferTimeoutResendWhileCumulativeProgress {
			t.Fatal("nonprogress regression changed production recovery timers/policy")
		}
		var replay []byte
		t.Cleanup(func() {
			if replay != nil {
				MessagePoolReturn(replay)
			}
		})
		first := fixture.write(64)
		if selective {
			second := fixture.write(64)
			fixture.forward(second, fixture.receiverIn)
			acks := fixture.takeAcknowledgements()
			for _, ack := range acks {
				if ack.ack.Selective && bytes.Equal(ack.ack.MessageId, second.pack.MessageId) && replay == nil {
					replay = MessagePoolCopy(ack.bytes)
				}
				fixture.forward(ack, fixture.senderIn)
			}
			fixture.forward(first, fixture.receiverIn)
			fixture.acknowledge()
		} else {
			fixture.forward(first, fixture.receiverIn)
			acks := fixture.takeAcknowledgements()
			for _, ack := range acks {
				if !ack.ack.Selective && bytes.Equal(ack.ack.MessageId, first.pack.MessageId) && replay == nil {
					replay = MessagePoolCopy(ack.bytes)
				}
				fixture.forward(ack, fixture.senderIn)
			}
		}
		if replay == nil || fixture.ackedCount != int(fixture.nextNumber) {
			t.Fatal("nonprogress regression lacks a real receipt already covered by the applied cumulative prefix")
		}
		targetNumber = fixture.nextNumber
		missing := fixture.write(64)
		fixture.drop(missing)
		armed.Store(true)
		sequence := flightGateSendSequence(t, fixture.sender, fixture.receiver.ClientId())
		before := fixture.sender.SendRecoveryStats()
		var retry *windowRoundFrame
		var firstDue time.Time
		for attempt := range 8 {
			var event dueEvent
			select {
			case event = <-due:
			case <-time.After(10 * time.Second):
				t.Fatal("ordinary missing original never reached its actual eligible due boundary")
			}
			if firstDue.IsZero() {
				firstDue = event.at
			}
			fixture.senderIn <- MessagePoolCopy(replay)
			synctest.Wait()
			if sequence.ackWindow.PendingDispositionFor(targetNumber, RequireIdFromBytes(missing.pack.MessageId)) {
				t.Fatal("old receipt falsely covers the actual missing original")
			}
			close(event.release)
			synctest.Wait()
			if len(fixture.senderOut) != 0 {
				retry = fixture.take(fixture.senderOut)
				break
			}
			if len(due) == 0 {
				t.Fatal("nonprogress did not produce the expected observable retry or repeated due selection")
			}
			if attempt == 7 {
				after := fixture.sender.SendRecoveryStats()
				t.Fatalf("ACK_NONPROGRESS_STARVES_PUBLIC_RETRY selective=%t repeated_due=8 preemptions=%d elapsed=%s original=%d", selective,
					after.AckPendingResendPreemptCount-before.AckPendingResendPreemptCount, time.Since(firstDue), targetNumber)
			}
		}
		armed.Store(false)
		if retry == nil || retry.pack == nil || !bytes.Equal(retry.pack.MessageId, missing.pack.MessageId) ||
			fixture.sender.SendRecoveryStats().AckPendingResendPreemptCount != before.AckPendingResendPreemptCount {
			t.Fatal("nonprogress feedback postponed or changed the due public retry")
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.ackedCount != int(fixture.nextNumber) || fixture.deliveredCount != int(fixture.nextNumber) ||
			sequence.resendQueue.Len() != 0 || sequence.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatal("repaired original lost exact delivery, terminal ACK, retry custody or shared lifetime")
		}
	})
}

// A duplicate can still pass exact ingress validation while the one owner is
// applying an already-snapshotted prefix. Its head identity retires later in
// that same application; the duplicate cannot then prove new progress.
func TestH1RepeatedApplyingHeadDoesNotPreemptDueHole(t *testing.T) {
	testH1ApplyingPrefixNonprogress(t, false)
}

func TestH1CoveredApplyingSelectiveDoesNotPreemptDueHole(t *testing.T) {
	testH1ApplyingPrefixNonprogress(t, true)
}

func testH1ApplyingPrefixNonprogress(t *testing.T, selective bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		applying, resumeApplying := make(chan struct{}), make(chan struct{})
		applied, resumeApplied := make(chan struct{}), make(chan struct{})
		var applyingArmed, appliedArmed atomic.Bool
		var dueCount atomic.Uint64
		var fixture *windowRoundFixture
		fixture = newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.afterAckSendItemForTest = func(id sendSequenceId, number uint64) {
				if id.Destination != fixture.receiver.ClientId() || number != 1 || !applyingArmed.CompareAndSwap(true, false) {
					return
				}
				close(applying)
				select {
				case <-resumeApplying:
				case <-fixture.ctx.Done():
				}
			}
			settings.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
				if id.Destination != fixture.receiver.ClientId() || !appliedArmed.CompareAndSwap(true, false) {
					return
				}
				close(applied)
				select {
				case <-resumeApplied:
				case <-fixture.ctx.Done():
				}
			}
			settings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
				if id.Destination == fixture.receiver.ClientId() && number == 3 {
					dueCount.Add(1)
				}
			}
		}, nil, TransportTypeH1)
		settings, defaults := fixture.sender.settings.SendBufferSettings, DefaultSendBufferSettings()
		if settings.ReliableLaneProvenRecovery || settings.MinResendInterval != defaults.MinResendInterval ||
			settings.RttMinResendInterval != defaults.RttMinResendInterval || settings.AckTimeout != defaults.AckTimeout ||
			!settings.DeferTimeoutResendWhileCumulativeProgress {
			t.Fatal("applying-prefix regression changed production recovery timers/policy")
		}
		warm := fixture.write(64)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		first, head := fixture.write(64), fixture.write(64)
		fixture.forward(head, fixture.receiverIn)
		acks := fixture.takeAcknowledgements()
		var selectiveReplay []byte
		for _, ack := range acks {
			if ack.ack.Selective && bytes.Equal(ack.ack.MessageId, head.pack.MessageId) && selectiveReplay == nil {
				selectiveReplay = MessagePoolCopy(ack.bytes)
			}
			fixture.forward(ack, fixture.senderIn)
		}
		if selectiveReplay == nil {
			t.Fatal("receiver did not publish the actual selectively held head")
		}
		t.Cleanup(func() { MessagePoolReturn(selectiveReplay) })
		missing := fixture.write(64)
		fixture.drop(missing)
		fixture.forward(first, fixture.receiverIn)
		acks = fixture.takeAcknowledgements()
		var cumulative *windowRoundFrame
		for _, ack := range acks {
			if !ack.ack.Selective && bytes.Equal(ack.ack.MessageId, head.pack.MessageId) {
				cumulative = ack
			}
		}
		if cumulative == nil || missing.pack.SequenceNumber != 3 || fixture.ackedCount != 1 {
			t.Fatal("fixture did not retain the actual cumulative prefix and missing tail")
		}
		replay := MessagePoolCopy(cumulative.bytes)
		if selective {
			MessagePoolReturn(replay)
			replay = MessagePoolCopy(selectiveReplay)
		}
		t.Cleanup(func() { MessagePoolReturn(replay) })
		applyingArmed.Store(true)
		fixture.forward(cumulative, fixture.senderIn)
		select {
		case <-applying:
		default:
			t.Fatal("cumulative application never parked before retiring its retained head")
		}
		sequence := fixture.sequence()
		headId := RequireIdFromBytes(head.pack.MessageId)
		missingId := RequireIdFromBytes(missing.pack.MessageId)
		if _, retained := sequence.retainedAckSequenceNumber(headId); !retained ||
			sequence.ackWindow.Snapshot(false).ackUpdateCount != 0 || fixture.ackedCount != 2 {
			t.Fatal("prefix barrier did not separate snapshot consumption from retained-head retirement")
		}
		fixture.senderIn <- MessagePoolCopy(replay)
		synctest.Wait()
		pending := sequence.ackWindow.Snapshot(false)
		if pending.ackUpdateCount != 1 || pending.headAck.messageId != headId ||
			sequence.ackWindow.PendingDispositionFor(3, missingId) {
			t.Fatal("real duplicate ingress did not preserve its response independently of missing-tail disposition")
		}
		appliedArmed.Store(true)
		close(resumeApplying)
		synctest.Wait()
		select {
		case <-applied:
		default:
			t.Fatal("owner did not reach its post-application boundary")
		}
		if _, retained := sequence.retainedAckSequenceNumber(headId); retained || fixture.ackedCount != 3 {
			t.Fatal("the real cumulative prefix did not retire exactly once")
		}
		item := sequence.resendQueue.GetByMessageId(missingId)
		if item == nil || item.recoveryKind != sendRecoveryNone || item.sendCount != 1 ||
			!item.reliableCarrierObserved || item.unreliableCarrierObserved || item.carrierChanged ||
			!sequence.transferFlightPolicy().h1Only {
			t.Fatal("missing tail is not its unchanged ordinary H1 recovery owner")
		}
		// Model descheduling after prefix application, not a different timer.
		// The next owner pass uses the real timer and physical-residence bounds.
		dueAt := item.resendTime
		if service := sequence.windowPacer.service; service != nil {
			dueAt = ackNonprogressLaterTime(dueAt, service.probeRecoveryDeadline(sequence.sequenceId, missingId,
				settings.RttScale, settings.MaxResendInterval))
			if interval := sequence.sharedRawRecoveryInterval(item, time.Now()); interval > 0 && item.pacingSentAtNanos != 0 {
				dueAt = ackNonprogressLaterTime(dueAt, sequence.firstPhysicalRecoveryTime(item).Add(max(interval, sequence.resendIntervalForItem(item, 1))))
			}
		}
		if !dueAt.Before(item.sendTime.Add(item.ackTimeout)) {
			t.Fatal("actual due boundary is outside the original ACK lifetime")
		}
		if delay := time.Until(dueAt); delay > 0 {
			time.Sleep(delay)
		}
		before := fixture.sender.SendRecoveryStats()
		close(resumeApplied)
		synctest.Wait()
		after := fixture.sender.SendRecoveryStats()
		if dueCount.Load() == 0 {
			t.Fatal("paused owner did not inspect its actually due missing original")
		}
		if preemptions := after.AckPendingResendPreemptCount - before.AckPendingResendPreemptCount; preemptions != 0 {
			t.Errorf("ACK_NONPROGRESS_PUBLIC_PREEMPT selective=%t extra_preemptions=%d original=3: replayed head2 was already applied", selective, preemptions)
		}
		fixture.startWire(40*time.Millisecond, fixture.receiver.settings.ReceiveBufferSettings.AckCompressTimeout)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if fixture.ackedCount != 4 || fixture.deliveredCount != 4 || sequence.resendQueue.Len() != 0 ||
			sequence.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatal("nonprogress race lost terminal ACK, exact delivery, retry custody or shared lifetime")
		}
	})
}

// Compares existing deadlines without changing either recovery policy.
func ackNonprogressLaterTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return second
	}
	return first
}

// Every head-consuming edge clears only the progress signal. A duplicate
// remains a truthful response, including merged plaintext/capability metadata.
func TestAckProgressDrainsPreserveDuplicateResponse(t *testing.T) {
	for _, drain := range []string{"snapshot", "response", "quiet"} {
		t.Run(drain, func(t *testing.T) {
			window := newSequenceAckWindow()
			head := sequenceAck{sequenceNumber: 21, messageId: NewId(), transportType: TransportTypeH1,
				tag: sequenceTag{sendTime: 111, set: true}}
			window.UpdateDeliveredPrefix(func() { window.updateDeliveredWithLock(head, ackBurstTailByteCount) })
			if !window.PendingCumulativeProgress() {
				t.Fatal("actual delivered prefix did not publish cumulative progress")
			}
			var taken sequenceAck
			switch drain {
			case "snapshot":
				taken = window.Snapshot(true).headAck
			case "response":
				var scratch [ackResponseMaxCount]sequenceAck
				acks, overflow := window.takeResponse(scratch[:0], len(scratch))
				if len(acks) != 1 || overflow {
					t.Fatal("head response did not consume exactly its prefix")
				}
				taken = acks[0]
			case "quiet":
				var ok bool
				taken, ok, _ = window.takeQuietHead(time.Now(), 0)
				if !ok {
					t.Fatal("eligible quiet head did not drain")
				}
			}
			if taken.messageId != head.messageId || taken.tag != head.tag || window.PendingCumulativeProgress() {
				t.Fatal("head drain changed its exact receipt or retained a consumed progress signal")
			}
			duplicate := head
			duplicate.unwrapped, duplicate.compactContractRecoverySupported, duplicate.receiverTiming = true, true, true
			window.Update(duplicate)
			if !window.Pending() || window.PendingCumulativeProgress() {
				t.Fatal("duplicate response disappeared or acquired new cumulative progress")
			}
			repeated := window.Snapshot(true)
			if repeated.ackUpdateCount != 1 || repeated.headAck.messageId != head.messageId ||
				!repeated.headAck.unwrapped || !repeated.headAck.compactContractRecoverySupported || !repeated.headAck.receiverTiming {
				t.Fatal("duplicate response lost its exact identity or merged feedback metadata")
			}
			advanced := head
			advanced.sequenceNumber, advanced.messageId = 22, NewId()
			advanced.tag.sendTime = 222
			window.Update(advanced)
			window.Update(duplicate)
			window.Snapshot(false)
			if !window.PendingCumulativeProgress() {
				t.Fatal("late nonprogress or nondraining observation erased a pending advance")
			}
			latest := window.Snapshot(true)
			if latest.headAck.messageId != advanced.messageId || latest.headAck.tag != advanced.tag || window.PendingCumulativeProgress() {
				t.Fatal("new head lost its own tag or did not consume its pending advance")
			}
		})
	}
}

// A failed local ACK write returns a response, not another first delivery.
// Restoration must retain the retry and any newer head's independent progress.
func TestAckRestoreResponseDoesNotInventCumulativeProgress(t *testing.T) {
	window := newSequenceAckWindow()
	head := sequenceAck{sequenceNumber: 21, messageId: NewId(), tag: sequenceTag{sendTime: 111, set: true}}
	window.Update(head)
	var scratch [ackResponseMaxCount]sequenceAck
	acks, _ := window.takeResponse(scratch[:0], len(scratch))
	window.Restore(acks)
	if !window.Pending() || window.PendingCumulativeProgress() {
		t.Fatal("restored unwritten head lost its response or manufactured new progress")
	}
	retry, overflow := window.takeResponse(scratch[:0], len(scratch))
	if len(retry) != 1 || overflow || retry[0].messageId != head.messageId || retry[0].tag != head.tag {
		t.Fatal("nonprogress restoration changed the original head response")
	}
	advanced := head
	advanced.sequenceNumber, advanced.messageId, advanced.tag.sendTime = 22, NewId(), 222
	window.Update(advanced)
	window.Restore(retry)
	if !window.PendingCumulativeProgress() {
		t.Fatal("restoring an older response erased the independently pending advance")
	}
	latest := window.Snapshot(true)
	if latest.headAck.messageId != advanced.messageId || latest.headAck.tag != advanced.tag {
		t.Fatal("old restoration overwrote the newer exact head/tag")
	}
}
