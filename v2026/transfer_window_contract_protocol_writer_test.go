// Empty contract announcements retain ACK ownership but use only the physical
// writer's readiness and lifetime gates, independently of optional DATA pacing.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A real ready contract wire neither waits for nor consumes DATA credit. The
// exact accepted ACK subsequently retires both originals and their callbacks.
func TestWindowPacingReadyContractBypassesDataReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	if DefaultClientSettings().SendBufferSettings.ApplicationPacing {
		t.Fatal("fixture must preserve shipping application pacing off")
	}
	for _, applicationPacing := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			sequence, older, _, route, observations, state := newWindowRouteAccountingFixture(t, 1)
			sequence.sendBufferSettings.ApplicationPacing = applicationPacing
			sequence.nextSequenceNumber = older.sequenceNumber + 1
			callbackCounts, callbackErrors := [2]int{}, [2]error{}
			older.acks.add(sendAckRecord{callback: func(err error) {
				callbackCounts[0]++
				callbackErrors[0] = err
			}})
			defer func() {
				sequence.cancel()
				sequence.ackLifetimes.clear()
				sequence.releaseRetainedSendItems(context.Canceled)
			}()
			disposition, err := sequence.writeMaybeWrappedBytes(older.transferFrameBytes, TransferPath{}, true, older, false, false)
			if err != nil || disposition.transportType != TransportTypeH1 || len(route) != 1 {
				t.Fatalf("ready fixture lost the real older H1 original: %v", err)
			}
			older.transportWriteObserved = true
			sequence.observeCarrierWrite(older, disposition)
			MessagePoolReturn(<-route)
			start := time.Now()
			service := sequence.windowPacer.service
			service.stateLock.Lock()
			service.next = start.Add(time.Second)
			next, probe, sent, total := service.next, service.probeSent, service.sent, service.total
			service.stateLock.Unlock()
			ahead := newContractAheadTestContract(t, sequence.client, sequence.destination)
			sequence.sendContractAheadAnnouncement(ahead, func(err error) {
				callbackCounts[1]++
				callbackErrors[1] = err
			})
			if len(sequence.sendItems) != 2 || len(route) != 1 || *observations != 2 ||
				time.Now() != start || state.pendingLocalWrites.Load() != 0 || callbackCounts != [2]int{} {
				t.Fatal("ready announcement waited for DATA service or lost its real write/ACK ownership")
			}
			announcement := sequence.sendItems[1]
			service.stateLock.Lock()
			unchanged := service.next == next && service.probeSent == probe && service.sent == sent && service.total == total &&
				service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil && service.waiterTail == nil
			service.stateLock.Unlock()
			if !unchanged || !announcement.noBudget || !announcement.contractControl || !announcement.expectsAck ||
				!announcement.transportWriteObserved || announcement.pacingByteCount != 0 || announcement.sendCount != 1 ||
				sequence.pendingRecovery != nil || sequence.resendWriteCount.Load() != 0 {
				t.Fatal("empty contract consumed DATA pacing credit or lost its reliable protocol classification")
			}
			wire := <-route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
			if !pack.ContractAhead || pack.ContractFrame == nil || pack.Head || pack.Nack || pack.DeliverImmediately ||
				len(pack.Frames) != 0 || pack.SequenceNumber != announcement.sequenceNumber ||
				string(pack.MessageId) != string(announcement.messageId[:]) {
				t.Fatal("ready writer did not emit the exact retained empty contract original")
			}
			if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: sequence.sequenceId.Bytes()}, 0); !ok || err != nil {
				t.Fatalf("real contract identity ACK was refused: %t %v", ok, err)
			}
			if callbackCounts != [2]int{} || !sequence.ackWindow.PendingDispositionFor(pack.SequenceNumber, announcement.messageId) {
				t.Fatal("ACK publication bypassed its ordinary retained owner")
			}
			sequence.applyAcceptedAckSnapshot(sequence.ackWindow.Snapshot(true))
			if callbackCounts != [2]int{1, 1} || callbackErrors != [2]error{} ||
				len(sequence.sendItems) != 0 || len(sequence.ackLifetimes.items) != 0 || time.Now() != start {
				t.Fatal("accepted cumulative contract ACK did not retire both exact original callbacks once")
			}
			if count, bytes := sequence.resendQueue.QueueSize(); count != 0 || bytes != 0 {
				t.Fatal("ready contract ACK left retained ownership")
			}
		})
	}
}

// A real capacity-one carrier retains its accepted share across the older
// recovery boundary and the empty announcement's own termination boundary.
func runWindowPacingBlockedContractTermination(t *testing.T, cancelBeforeDeadline bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, older, _, route, observations, state := newWindowRouteAccountingFixture(t, 1)
		older.ackTimeout = time.Minute
		sequence.nextSequenceNumber = older.sequenceNumber + 1
		disposition, err := sequence.writeMaybeWrappedBytes(older.transferFrameBytes, TransferPath{}, true, older, false, false)
		if err != nil || disposition.transportType != TransportTypeH1 || len(route) != 1 {
			t.Fatalf("cancel fixture lost its real older physical write: %v", err)
		}
		older.transportWriteObserved = true
		sequence.observeCarrierWrite(older, disposition)
		physical, olderId, olderNumber := time.Now(), older.messageId, older.sequenceNumber
		sequence.setResendTime(older, physical.Add(sequence.resendIntervalForItem(older, 1)))
		due, lifetime := older.resendTime, older.sendTime.Add(older.ackTimeout)
		if due != physical.Add(2*time.Second) || sequence.sendBufferSettings.WriteTimeout != time.Second {
			t.Fatal("cancel fixture changed ordinary recovery or physical writer timing")
		}
		inspection := &windowOwnerInspection{ctx: sequence.ctx}
		arrived, resume := inspection.arm(due)
		sequence.beforeContractWriterAccessForTest = inspection.beforeWriterAccess
		time.Sleep(time.Until(due.Add(-500 * time.Millisecond)))
		entry := time.Now()
		var deadline time.Time
		deadlineObservations := 0
		sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
			deadline, deadlineObservations = until, deadlineObservations+1
		}
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = due.Add(time.Second)
		next, probe, sent, total := service.next, service.probeSent, service.sent, service.total
		service.stateLock.Unlock()
		callbackCounts, callbackErrors := [2]int{}, [2]error{}
		older.acks.add(sendAckRecord{callback: func(err error) {
			callbackCounts[0]++
			callbackErrors[0] = err
		}})
		ahead := newContractAheadTestContract(t, sequence.client, sequence.destination)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			sequence.sendContractAheadAnnouncement(ahead, func(err error) {
				callbackCounts[1]++
				callbackErrors[1] = err
			})
		}()
		defer func() {
			sequence.cancel()
			<-finished
			sequence.pendingRecovery = nil
			sequence.ackLifetimes.clear()
			sequence.releaseRetainedSendItems(context.Canceled)
		}()
		synctest.Wait()
		if *observations != 2 || state.pendingLocalWrites.Load() != 1 || len(sequence.sendItems) != 2 || len(route) != 1 ||
			deadlineObservations != 1 || deadline != entry.Add(time.Second) {
			t.Fatal("cancellation control did not reach the real blocked protocol writer")
		}
		announcement := sequence.sendItems[1]
		if !announcement.noBudget || !announcement.contractControl || !announcement.expectsAck || announcement.pacingByteCount != 0 {
			t.Fatal("cancellation control changed the actual empty announcement classification")
		}
		time.Sleep(time.Until(due))
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resume, due)
		if sequence.pendingRecovery == nil || sequence.pendingRecovery.messageId != olderId ||
			!older.head || !older.promotedHead || older.sendTime.Add(older.ackTimeout) != lifetime ||
			announcement.transportWriteObserved || callbackCounts != [2]int{} {
			t.Fatal("blocked announcement did not retain exact original/recovery ownership before cancel")
		}
		endedAt := due
		if cancelBeforeDeadline {
			sequence.cancel()
		} else {
			endedAt = deadline
			time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
			synctest.Wait()
			select {
			case <-finished:
				t.Fatal("due recovery wake shortened the empty contract's absolute writer budget")
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case <-finished:
			default:
				t.Fatal("due recovery wake restarted the empty contract's absolute writer budget")
			}
		}
		<-finished
		synctest.Wait()
		service.stateLock.Lock()
		unchanged := service.next == next && service.probeSent == probe && service.sent == sent && service.total == total &&
			service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil && service.waiterTail == nil
		service.stateLock.Unlock()
		if !unchanged || time.Now() != endedAt || deadlineObservations != 1 || deadline != entry.Add(time.Second) ||
			(sequence.ctx.Err() != nil) != cancelBeforeDeadline || len(route) != 1 || *observations != 2 ||
			state.pendingLocalWrites.Load() != 0 || announcement.transportWriteObserved ||
			sequence.resendWriteCount.Load() != 0 || callbackCounts != [2]int{} || older.sendTime.Add(older.ackTimeout) != lifetime {
			t.Fatal("cancellation fabricated physical acceptance, DATA debt, retry or terminal ACK")
		}
		wire := <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(pack.MessageId) != string(olderId[:]) || pack.SequenceNumber != olderNumber || pack.Head {
			t.Fatal("cancellation returned something other than the original accepted carrier share")
		}
		sequence.cancel()
		sequence.pendingRecovery = nil
		sequence.ackLifetimes.clear()
		sequence.releaseRetainedSendItems(context.Canceled)
		if callbackCounts != [2]int{1, 1} || !errors.Is(callbackErrors[0], context.Canceled) || !errors.Is(callbackErrors[1], context.Canceled) ||
			len(sequence.sendItems) != 0 || len(sequence.ackLifetimes.items) != 0 || len(route) != 0 {
			t.Fatal("joined blocked contract failed exact once cancellation and original ownership cleanup")
		}
		if count, bytes := sequence.resendQueue.QueueSize(); count != 0 || bytes != 0 {
			t.Fatal("joined blocked contract retained a resend owner")
		}
	})
}

// Cancellation at the real older deadline joins the blocked protocol writer,
// without accepting its announcement or consuming its selected physical retry.
func TestWindowPacingBlockedContractCancellationKeepsProtocolOwnership(t *testing.T) {
	runWindowPacingBlockedContractTermination(t, true)
}

// A recovery wake cannot shorten or restart the same empty contract write's
// existing one-second budget; the retained reliable owner survives that refusal.
func TestWindowPacingBlockedContractKeepsAbsoluteWriterDeadline(t *testing.T) {
	runWindowPacingBlockedContractTermination(t, false)
}
