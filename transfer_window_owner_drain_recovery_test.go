// A controlled drain cannot postpone the repair needed to complete that drain.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The real worker has lost an accepted H1 tail and holds one fresh FIFO turn.
// Long observed residence starts a drain beyond that tail's next recovery time.
func runWindowOwnerDrainFixture(t *testing.T, check func(*testing.T, *windowOwnerProgressFixture, time.Time, time.Time)) {
	t.Helper()
	runWindowOwnerProgressFixture(t, false, false, -time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		sequence := fixture.sequence
		service := sequence.windowPacer.service
		now := time.Now()
		service.observeRoundTrip(time.Millisecond, 0, now.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(20256*time.Millisecond, 0, now.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		time.Sleep(time.Until(fixture.youngerDeadline))
		synctest.Wait()
		service.stateLock.Lock()
		drainDeadline, nextCheck := service.drainUntil, service.drainCheckAt
		waiting := service.waiterHead == &sequence.windowPacer.waiter && service.pacingReservations == 1
		service.stateLock.Unlock()
		if !drainDeadline.After(fixture.head.resendTime) || !waiting || len(fixture.route) != 0 {
			t.Fatalf("fresh owner did not enter a drain beyond its tail repair: drain=%s due=%s waiting=%t writes=%d", time.Until(drainDeadline), time.Until(fixture.head.resendTime), waiting, len(fixture.route))
		}
		check(t, fixture, drainDeadline, nextCheck)
	})
}

// Follow the original physical recovery clock, including legitimate residence
// deferrals. A selected retry must release only the impossible drain attempt.
func selectWindowOwnerDrainRecovery(t *testing.T, fixture *windowOwnerProgressFixture, drainDeadline time.Time) {
	t.Helper()
	sequence, head := fixture.sequence, fixture.head
	for attempts := 0; sequence.pendingRecovery == nil && attempts < 4; attempts++ {
		if !head.resendTime.After(time.Now()) || !head.resendTime.Before(drainDeadline) {
			t.Fatalf("tail has no eligible repair before drain expiry: due=%s drain=%s", time.Until(head.resendTime), time.Until(drainDeadline))
		}
		due := head.resendTime
		arrived, resume := fixture.inspection.arm(due)
		time.Sleep(time.Until(due))
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resume, due)
	}
	if sequence.pendingRecovery == nil || sequence.pendingRecovery.messageId != head.messageId {
		t.Fatal("real worker did not select the lost older tail")
	}
	if len(fixture.written) != 1 || len(fixture.route) != 1 {
		t.Fatalf("selected older repair is trapped behind its own controlled drain for %s; fresh writes=%d physical copies=%d", time.Until(drainDeadline), len(fixture.written), len(fixture.route))
	}
}

// Physical acceptance retains the fresh continuation's identity and buffer.
func takeWindowOwnerDrainOriginal(t *testing.T, fixture *windowOwnerProgressFixture) {
	t.Helper()
	if len(fixture.written) != 1 || len(fixture.route) != 1 {
		t.Fatal("fresh continuation did not reach its physical write barrier")
	}
	if number := <-fixture.written; number != fixture.younger.sequenceNumber {
		t.Fatalf("fresh FIFO continuation changed ownership: got %d want %d", number, fixture.younger.sequenceNumber)
	}
	wire := <-fixture.route
	pack := decodeSendPackLifecycleWirePack(t, wire)
	MessagePoolReturn(wire)
	if string(pack.MessageId) != string(fixture.younger.messageId[:]) {
		t.Fatal("a repair borrowed the fresh continuation's physical bytes")
	}
}

// Selection allows the fresh continuation to finish; only the outer worker
// then emits the older repair. No queue credit, drain proof or probe is invented.
func TestWindowPacingOwnerDrainReleasesSelectedRecovery(t *testing.T) {
	runWindowOwnerDrainFixture(t, func(t *testing.T, fixture *windowOwnerProgressFixture, drainDeadline, nextCheck time.Time) {
		selectWindowOwnerDrainRecovery(t, fixture, drainDeadline)
		takeWindowOwnerDrainOriginal(t, fixture)
		sequence, head := fixture.sequence, fixture.head
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		kept := !service.drained && !service.drainServiceEpoch && service.drainUntil.IsZero() &&
			service.roundTripProbe.sentAt.IsZero() && service.drainCheckAt == nextCheck &&
			service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
			service.sent == fixture.chargedSent && service.pacingReservations == 0 && service.reservedByteCount == 0
		service.stateLock.Unlock()
		if !kept {
			t.Fatal("abandoning an impossible drain changed debt or cooldown, or manufactured delivery or a probe")
		}
		close(fixture.resumeYounger)
		synctest.Wait()
		if len(fixture.route) != 1 || sequence.resendWriteCount.Load() != 1 {
			t.Fatalf("outer owner did not physically repair its selected tail: copies=%d retries=%d", len(fixture.route), sequence.resendWriteCount.Load())
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(pack.MessageId) != string(head.messageId[:]) {
			t.Fatal("physical retry changed the older retained identity")
		}
	})
}

// Selection may precede the fresh reservation's original serialization
// deadline. A later attempted drain must not hide that already-selected repair.
func TestWindowPacingOwnerDrainPreviouslySelectedRecovery(t *testing.T) {
	runWindowOwnerProgressFixture(t, false, false, time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		sequence := fixture.sequence
		service := sequence.windowPacer.service
		now := time.Now()
		service.observeRoundTrip(time.Millisecond, 0, now.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(20256*time.Millisecond, 0, now.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		time.Sleep(time.Until(fixture.due))
		synctest.Wait()
		if sequence.pendingRecovery == nil || len(fixture.route) != 0 {
			t.Fatal("older selection did not preserve the fresh write's original serialization deadline")
		}
		time.Sleep(time.Until(fixture.youngerDeadline))
		synctest.Wait()
		takeWindowOwnerDrainOriginal(t, fixture)
		service.stateLock.Lock()
		kept := service.drainStartedAt == fixture.youngerDeadline && service.drainUntil.IsZero() &&
			service.drainCheckAt.After(time.Now()) && !service.drained && service.roundTripProbe.sentAt.IsZero() &&
			service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe
		service.stateLock.Unlock()
		if !kept || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("a later drain attempt lost the selected repair, its fresh FIFO continuation or its paid clock")
		}
	})
}

// A real covering ACK wins before recovery and still proves the empty path.
func TestWindowPacingOwnerDrainAckPreservesProbe(t *testing.T) {
	runWindowOwnerDrainFixture(t, func(t *testing.T, fixture *windowOwnerProgressFixture, _, nextCheck time.Time) {
		sequence, head := fixture.sequence, fixture.head
		time.Sleep(time.Until(head.resendTime.Add(-time.Nanosecond)))
		synctest.Wait()
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, sequence.destination, &protocol.Pack{
			MessageId: head.messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
		})
		synctest.Wait()
		takeWindowOwnerDrainOriginal(t, fixture)
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		probe := service.roundTripProbe
		kept := probe.messageId == fixture.younger.messageId && probe.written && probe.resetService &&
			service.drainCheckAt == nextCheck && service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe
		service.stateLock.Unlock()
		if !kept || sequence.pendingRecovery != nil || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("covering ACK lost its real controlled probe or selected an unnecessary repair")
		}
		close(fixture.resumeYounger)
		synctest.Wait()
		if len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("ordinary ACK application repeated the covered older write")
		}
	})
}

// An ACK arriving after selection still suppresses the physical retry when
// the original continuation returns to its ordinary ACK owner.
func TestWindowPacingOwnerDrainSelectedAckPreemptsCopy(t *testing.T) {
	runWindowOwnerDrainFixture(t, func(t *testing.T, fixture *windowOwnerProgressFixture, drainDeadline, _ time.Time) {
		selectWindowOwnerDrainRecovery(t, fixture, drainDeadline)
		takeWindowOwnerDrainOriginal(t, fixture)
		sequence, head := fixture.sequence, fixture.head
		headId := head.messageId
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, sequence.destination, &protocol.Pack{
			MessageId: headId.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
		})
		close(fixture.resumeYounger)
		synctest.Wait()
		if sequence.pendingRecovery != nil || sequence.resendQueue.GetByMessageId(headId) != nil ||
			len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 {
			t.Fatal("ACK arrival did not retire the selected older copy before physical dispatch")
		}
	})
}

// Cancellation inside the real drain releases ownership without refunding the
// serialization or opening allowance and cannot manufacture a physical write.
func TestWindowPacingOwnerDrainCancellationKeepsDebt(t *testing.T) {
	runWindowOwnerDrainFixture(t, func(t *testing.T, fixture *windowOwnerProgressFixture, _, nextCheck time.Time) {
		sequence := fixture.sequence
		service := sequence.windowPacer.service
		fixture.cancel()
		if err := fixture.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		service.stateLock.Lock()
		kept := service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
			service.drainCheckAt == nextCheck && service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.waiterHead == nil && service.waiterTail == nil && !service.drained && service.roundTripProbe.sentAt.IsZero()
		service.stateLock.Unlock()
		if !kept || sequence.pendingRecovery != nil || len(fixture.route) != 0 ||
			sequence.resendWriteCount.Load() != 0 || len(sequence.sendItems) != 0 {
			t.Fatal("canceled drain leaked ownership, dispatched or manufactured service credit")
		}
	})
}
