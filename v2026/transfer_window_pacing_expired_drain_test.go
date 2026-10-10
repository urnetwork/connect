// Real retained H1 credit and a running pacer distinguish an expired drain
// attempt from continued no-progress flight. This is not a throughput model.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Only the original timer's dispatch is held. Repayment enters the ordinary
// ACK coalescer, and the subsequent clock writer owns a real pooled share.
func runWindowExpiredDrain(t *testing.T, event string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		fixture := newWindowReceiverCreditFixture(t, nil, start, 0)
		service, sequence := fixture.service, fixture.sequence
		pacer := &sequence.windowPacer
		pacer.rate = 1000000
		service.observeReceiverRoundTrip(1, 40*time.Millisecond, 40*time.Millisecond, 0, start.Add(-time.Second))
		service.observeReceiverRoundTrip(2, 100*time.Millisecond, 100*time.Millisecond, 0, start)
		var originals [32]*sendItem
		for number := range originals {
			originals[number] = fixture.write(uint64(number), 1000, start.Add(-100*time.Millisecond))
		}
		frame := MessagePoolGet(1000)
		resumed := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 32},
			sendTime: start, sendCount: 1, expectsAck: true, transferFrameBytes: frame,
			pacingByteCount: 1000, rttState: sendItemRttWritePending}
		sequence.resendQueue.Add(resumed)
		writer := &windowPacingClockWriter{windowPacingPolicyWriter: windowPacingPolicyWriter{
			policy: transferFlightPolicySnapshot{h1Only: true}}}
		woke, release := make(chan time.Time, 1), make(chan struct{})
		first := true
		pacer.afterWaitForTest = func() {
			if first {
				first = false
				woke <- time.Now()
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		type outcome struct {
			at, writtenAt time.Time
			err           error
			written       bool
		}
		result, joined := make(chan outcome, 1), make(chan struct{})
		go func() {
			defer close(joined)
			err := pacer.waitForServiceMessage(ctx, len(frame), false, sequence.sequenceId, resumed.messageId, resumed.sequenceNumber)
			written := false
			if err == nil {
				resumed.pacingSentAtNanos = time.Now().UnixNano()
				share := MessagePoolShareReadOnly(frame)
				var transport TransportType
				written, transport, err = writer.WriteDetailedWithTransport(ctx, share, 0)
				if !written {
					MessagePoolReturn(share)
				}
				if written && err == nil && transport == TransportTypeH1 {
					resumed.transportWriteObserved = true
					fixture.confirm(resumed)
				}
			}
			result <- outcome{at: time.Now(), writtenAt: writer.writtenAt, err: err, written: written}
		}()
		defer func() {
			cancel()
			<-joined
			pacer.close()
			if !MessagePoolReturn(frame) {
				t.Error("joined clock writer retained the original pooled frame share")
			}
		}()
		type state struct {
			started, until, check, progress, next, paid         time.Time
			flight, spent                                       float64
			sent, acked, reserved, burst, ownedSent, ownedAcked ByteCount
			reservations, pending                               int
			head, drained, epoch, probe                         bool
			minimum                                             time.Duration
		}
		snapshot := func() state {
			service.stateLock.Lock()
			defer service.stateLock.Unlock()
			return state{started: service.drainStartedAt, until: service.drainUntil, check: service.drainCheckAt,
				progress: service.drainProgressUntil, next: service.next, paid: service.burstMeter.paidUntil,
				flight: service.outstandingWithLock(), spent: service.burstMeter.spent,
				sent: service.sent, acked: service.total, reserved: service.reservedByteCount, burst: service.burstMeter.limit,
				ownedSent: pacer.serviceSent, ownedAcked: pacer.serviceAcked,
				reservations: service.pacingReservations, pending: service.pendingWrites,
				head: service.waiterHead == &pacer.waiter, drained: service.drained, epoch: service.drainServiceEpoch,
				probe: !service.roundTripProbe.sentAt.IsZero(), minimum: service.roundTripEvidenceWithLock(time.Now()).minimum}
		}
		credit := func(head int) {
			ack := fixture.ack(originals[head], time.Now(), 0)
			sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		}
		synctest.Wait()
		opening := snapshot()
		if opening.started != start || opening.until != start.Add(200*time.Millisecond) ||
			opening.check != start.Add(windowPacingDrainMinimumInterval) || opening.burst != 10000 ||
			opening.flight != 32000 || opening.acked != 0 || opening.sent != 33000 ||
			opening.ownedSent != 33000 || opening.reserved != 1000 || opening.reservations != 1 ||
			!opening.head || opening.pending != 1 || opening.drained || !opening.epoch || opening.probe {
			t.Fatalf("%s: actual pacer did not establish the original200ms drain and one charged FIFO: %+v", event, opening)
		}
		wantAt := start.Add(200 * time.Millisecond)
		acked := ByteCount(0)
		if event == "before_expiry" || event == "stale_grace" {
			time.Sleep(100 * time.Millisecond)
			credit(15)
			acked = 16000
			if event == "before_expiry" {
				// Secondary mechanism control: re-inspect the active attempt
				// before its timer expires; real repayment must not end it early.
				delay, _ := service.admitBurst(time.Now(), 1000, false, &pacer.waiter)
				if delay != 300*time.Millisecond || snapshot().until != start.Add(400*time.Millisecond) {
					t.Fatalf("before-expiry evidence changed the existing400ms bound: delay=%s state=%+v", delay, snapshot())
				}
				wantAt = start.Add(400 * time.Millisecond)
			} else {
				// The existing progress helper consumes real ACK credit here.
				// Its one-turn grace must expire rather than become a latch.
				service.stateLock.Lock()
				interval := max(4*deliverySizedWindowSampleInterval, service.roundTripEvidenceWithLock(time.Now()).residence)
				granted := service.observeDrainProgressWithLock(time.Now(), interval)
				service.stateLock.Unlock()
				if !granted || snapshot().progress != start.Add(140*time.Millisecond) {
					t.Fatalf("real partial ACK did not establish exact one-turn grace: %+v", snapshot())
				}
				wantAt = start.Add(600 * time.Millisecond)
			}
		}
		if event == "just_before" {
			time.Sleep(time.Until(opening.until.Add(-time.Nanosecond)))
			credit(15)
			acked = 16000
		}
		time.Sleep(time.Until(opening.until))
		synctest.Wait()
		select {
		case at := <-woke:
			if at != opening.until {
				t.Fatalf("%s: initial deadline dispatch moved: %s", event, at.Sub(start))
			}
		default:
			t.Fatal("actual pacer did not reach the owned expiry barrier")
		}
		switch event {
		case "at_expiry", "cancel":
			credit(15)
			acked = 16000
		case "burst_noise":
			credit(9)
			acked = 10000
			wantAt = start.Add(600 * time.Millisecond)
		case "no_progress", "stale_grace":
			// A new long-path observation without new byte repayment remains
			// eligible to extend the bounded measurement, as before this fix.
			service.observeReceiverRoundTrip(3, 300*time.Millisecond, 300*time.Millisecond, 0, time.Now())
			wantAt = start.Add(600 * time.Millisecond)
		}
		before := snapshot()
		if before.sent != opening.sent || before.acked != acked || before.ownedAcked != acked ||
			before.flight != float64(32000-acked) || before.pending != 1 || before.drained || before.probe ||
			before.reservations != 1 || before.reserved != 1000 || !before.head ||
			before.next != opening.next || before.paid != opening.paid || before.spent != opening.spent {
			t.Fatalf("%s: ACK publication duplicated credit, released ownership, or repriced debt: %+v", event, before)
		}
		if event == "cancel" {
			cancel()
		}
		close(release)
		synctest.Wait()
		var got outcome
		finished := false
		select {
		case got = <-result:
			finished = true
		default:
		}
		if event == "cancel" {
			if !finished || !errors.Is(got.err, context.Canceled) || got.written || !got.writtenAt.IsZero() || got.at != opening.until {
				t.Fatalf("canceled expired attempt dispatched or failed to finish: %+v", got)
			}
			canceled := snapshot()
			if canceled.drained || canceled.probe || canceled.pending != 1 || canceled.reserved != 0 ||
				canceled.reservations != 0 || canceled.head || canceled.next != opening.next || canceled.acked != acked {
				t.Fatalf("cancellation forged drain proof, refunded debt, or retained its FIFO: %+v", canceled)
			}
			return
		}
		if wantAt == opening.until && !finished {
			t.Errorf("expired drain extended despite complete-turn above-burst repayment: at=%s repaid=%d burst=%d state=%+v",
				time.Since(start), acked, opening.burst, snapshot())
		} else if wantAt.After(opening.until) && finished {
			t.Errorf("%s: bounded active/no-progress pause ended early: got=%s want=%s", event, got.at.Sub(start), wantAt.Sub(start))
		}
		if !finished {
			// Both preimage and candidate must finish the same unchanged
			// bounded attempt; a failed causal assertion still joins ownership.
			settleAt := wantAt
			if wantAt == opening.until {
				settleAt = start.Add(600 * time.Millisecond)
			}
			time.Sleep(time.Until(settleAt))
			synctest.Wait()
			select {
			case got = <-result:
			default:
				t.Fatalf("%s: actual pacer did not end its original residence-derived bound: %+v", event, snapshot())
			}
		}
		<-joined
		if got.err != nil || !got.written || got.writtenAt != got.at {
			t.Fatalf("%s: resumed physical clock-writer disposition=%+v", event, got)
		}
		if got.at != wantAt {
			t.Errorf("%s: resumed write at%s, want%s", event, got.at.Sub(start), wantAt.Sub(start))
		}
		after := snapshot()
		if after.started != opening.started || after.check != opening.check || !after.until.IsZero() ||
			after.reservations != 0 || after.reserved != 0 || after.head || after.pending != 1 || after.drained || after.probe ||
			after.next != opening.next || after.sent != opening.sent || after.acked != acked || after.minimum != 40*time.Millisecond {
			t.Fatalf("%s: resumed nonempty flight invented a probe/reset or lost its original ownership: %+v", event, after)
		}
		// The old tail can retire only its prefix. The newly admitted H1
		// item must remain outstanding until its own real cumulative ACK.
		time.Sleep(time.Millisecond)
		credit(31)
		partial := snapshot()
		if partial.acked != 32000 || partial.flight != 1000 || partial.pending != 1 || partial.drained || partial.probe {
			t.Fatalf("%s: older cumulative head credited the resumed writer: %+v", event, partial)
		}
		time.Sleep(time.Until(got.writtenAt.Add(50 * time.Millisecond)))
		ack := fixture.ack(resumed, time.Now(), 0)
		sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		complete := snapshot()
		if complete.acked != 33000 || complete.ownedAcked != 33000 || complete.flight != 0 ||
			complete.pending != 0 || !complete.drained || complete.probe || complete.minimum != 40*time.Millisecond {
			t.Fatalf("%s: actual final head did not reconcile exactly one complete flight: %+v", event, complete)
		}
	})
}

// Repayment before or exactly at dispatch of the old deadline is real credit,
// but is not evidence that resumed traffic will keep draining at its old rate.
func TestWindowPacingExpiredDrainRepaymentEndsAttempt(t *testing.T) {
	for _, event := range []string{"just_before", "at_expiry"} {
		runWindowExpiredDrain(t, event)
	}
}

// No byte progress and one permitted burst still retain late-path extension.
func TestWindowPacingExpiredDrainNoiseKeepsExtension(t *testing.T) {
	for _, event := range []string{"no_progress", "burst_noise"} {
		runWindowExpiredDrain(t, event)
	}
}

// Current expiry, not an old grace bit or a still-active timer, is decisive.
func TestWindowPacingExpiredDrainProgressBoundaryControls(t *testing.T) {
	for _, event := range []string{"before_expiry", "stale_grace"} {
		runWindowExpiredDrain(t, event)
	}
}

// Cancellation joins the actual pacer and cannot turn partial credit into
// an empty-flight observation or return paid nominal serialization debt.
func TestWindowPacingExpiredDrainProgressCancellation(t *testing.T) {
	runWindowExpiredDrain(t, "cancel")
}
