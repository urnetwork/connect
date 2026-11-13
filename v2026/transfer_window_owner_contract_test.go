// Synchronous contract announcements return directly into application sending,
// so selected recovery must be consumed before that next entry owns a waiter.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The same owned call stack sends an actual announcement followed by an actual
// application Pack. No Run iteration is inserted between those two entries.
func runWindowOwnerContractContinuation(t *testing.T, acknowledge bool, beforeApplication, afterApplication func(*testing.T, *SendSequence, *sendItem, Route)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, older, _, route, observations, state := newWindowRouteAccountingFixture(t, 1)
		older.ackTimeout = time.Minute
		sequence.nextSequenceNumber = older.sequenceNumber + 1
		disposition, err := sequence.writeMaybeWrappedBytes(older.transferFrameBytes, TransferPath{}, true, older, false, false)
		if err != nil || disposition.transportType != TransportTypeH1 || len(route) != 1 {
			t.Fatalf("fixture did not physically accept the older H1 original: %v", err)
		}
		older.transportWriteObserved = true
		sequence.observeCarrierWrite(older, disposition)
		physical := time.Now()
		sequence.setResendTime(older, physical.Add(sequence.resendIntervalForItem(older, 1)))
		due, lifetime, olderId := older.resendTime, older.sendTime.Add(older.ackTimeout), older.messageId
		inspection := &windowOwnerInspection{ctx: sequence.ctx}
		arrived, resumeInspection := inspection.arm(due)
		sequence.beforeContractWriterAccessForTest = inspection.beforeWriterAccess
		// Keep the already-accepted original's real route share in the one-slot
		// carrier. The empty announcement waits for physical readiness, not DATA credit.
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		expiry := service.burst.start.Add(windowPacingBurstMaximumTime(service.burstEstimateTime))
		service.stateLock.Unlock()
		if !expiry.After(time.Now()) || !expiry.Before(due) || due != physical.Add(2*time.Second) {
			t.Fatal("fixture did not preserve the cold physical recovery boundary")
		}
		entry, acceptedAt := due.Add(-500*time.Millisecond), due.Add(250*time.Millisecond)
		if !expiry.Before(entry) || sequence.sendBufferSettings.WriteTimeout != time.Second {
			t.Fatal("fixture changed the existing burst or absolute writer budget")
		}
		time.Sleep(time.Until(entry))
		service.stateLock.Lock()
		service.next = due.Add(time.Second)
		unchangedNext, unchangedProbe := service.next, service.probeSent
		unchangedSent, unchangedTotal := service.sent, service.total
		service.stateLock.Unlock()
		var deadline time.Time
		deadlineObservations := 0
		sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
			deadline, deadlineObservations = until, deadlineObservations+1
		}
		callbackCounts, callbackErrors := [3]int{}, [3]error{}
		callback := func(index int) AckFunction {
			return func(err error) {
				callbackCounts[index]++
				callbackErrors[index] = err
			}
		}
		older.acks.add(sendAckRecord{callback: callback(0)})
		applicationStarted := false
		ahead := newContractAheadTestContract(t, sequence.client, sequence.destination)
		announced, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(finished)
			sequence.sendContractAheadAnnouncement(ahead, callback(1))
			close(announced)
			select {
			case <-sequence.ctx.Done():
				return
			case <-resume:
			}
			applicationStarted = true
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(32)}
			clear(frame.MessageBytes)
			sequence.send([]*protocol.Frame{frame}, callback(2), true, true)
		}()
		defer func() {
			sequence.cancel()
			<-finished
			sequence.pendingRecovery = nil
			sequence.ackLifetimes.clear()
			sequence.releaseRetainedSendItems(context.Canceled)
			for index := range callbackCounts {
				want := 1
				if index == 2 && !applicationStarted {
					want = 0
				}
				if callbackCounts[index] != want || want == 1 && !errors.Is(callbackErrors[index], context.Canceled) {
					t.Errorf("original %d terminal callbacks=%d want=%d error=%v", index, callbackCounts[index], want, callbackErrors[index])
				}
			}
			if count, bytes := sequence.resendQueue.QueueSize(); count != 0 || bytes != 0 || len(sequence.sendItems) != 0 || len(sequence.ackLifetimes.items) != 0 {
				t.Error("joined continuation retained an original owner")
			}
		}()
		synctest.Wait()
		service.stateLock.Lock()
		unreserved := service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.waiterHead == nil && service.waiterTail == nil &&
			service.next == unchangedNext && service.probeSent == unchangedProbe &&
			service.sent == unchangedSent && service.total == unchangedTotal
		service.stateLock.Unlock()
		if !unreserved || time.Now() != entry || deadline != entry.Add(time.Second) || deadlineObservations != 1 ||
			!acceptedAt.Before(deadline) || len(route) != 1 || len(sequence.sendItems) != 2 ||
			*observations != 2 || state.pendingLocalWrites.Load() != 1 {
			t.Fatal("real empty announcement did not wait at the bounded physical writer without DATA credit")
		}
		announcement := sequence.sendItems[1]
		if !announcement.contractControl || !announcement.noBudget || !announcement.expectsAck ||
			announcement.pacingByteCount != 0 || announcement.transportWriteObserved ||
			announcement.sendCount != 1 || announcement.sequenceNumber != older.sequenceNumber+1 || callbackCounts != [3]int{} {
			t.Fatal("blocked announcement lost its protocol classification or retained ACK ownership")
		}
		time.Sleep(time.Until(due.Add(-time.Nanosecond)))
		synctest.Wait()
		if sequence.pendingRecovery != nil || older.head || len(route) != 1 || *observations != 2 {
			t.Fatal("announcement selected or dispatched the older retry before its real due time")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resumeInspection, due)
		service.stateLock.Lock()
		kept := service.next == unchangedNext && service.probeSent == unchangedProbe &&
			service.sent == unchangedSent && service.total == unchangedTotal &&
			service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.waiterHead == nil && service.waiterTail == nil
		service.stateLock.Unlock()
		if !kept || sequence.pendingRecovery == nil || sequence.pendingRecovery.messageId != olderId ||
			!older.head || !older.promotedHead || older.sendTime.Add(older.ackTimeout) != lifetime ||
			len(route) != 1 || *observations != 2 || deadlineObservations != 1 ||
			state.pendingLocalWrites.Load() != 1 || announcement.transportWriteObserved || callbackCounts != [3]int{} {
			t.Fatal("physical announcement wait did not select real due recovery without charging protocol DATA credit")
		}
		time.Sleep(time.Until(acceptedAt))
		wire := <-route
		original := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(original.MessageId) != string(olderId[:]) || original.SequenceNumber != older.sequenceNumber || original.Head {
			t.Fatal("readiness release did not consume the original physical share")
		}
		synctest.Wait()
		select {
		case <-announced:
		default:
			t.Fatal("physically released announcement did not finish before the next synchronous entry")
		}
		if time.Now() != acceptedAt || !time.Now().Before(deadline) || !time.Now().Before(due.Add(time.Second)) ||
			len(route) != 1 || sequence.pendingRecovery == nil || deadlineObservations != 1 ||
			*observations != 2 || state.pendingLocalWrites.Load() != 0 ||
			!announcement.transportWriteObserved || announcement.pacingByteCount != 0 || callbackCounts != [3]int{} {
			t.Fatal("announcement return lost its physical result, selected recovery or original writer bound")
		}
		sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = nil
		wire = <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if !pack.ContractAhead || pack.Head || pack.Nack || pack.DeliverImmediately || pack.ContractFrame == nil ||
			string(pack.MessageId) != string(announcement.messageId[:]) ||
			pack.SequenceNumber != older.sequenceNumber+1 || len(pack.Frames) != 0 {
			t.Fatal("fixture did not dispatch a real successor contract announcement")
		}
		if acknowledge {
			if ok, err := sequence.Ack(&protocol.Ack{MessageId: olderId.Bytes(), SequenceId: sequence.sequenceId.Bytes(), Selective: true}, 0); !ok || err != nil {
				t.Fatalf("real selected-copy ACK was refused: %t %v", ok, err)
			}
			if !sequence.ackWindow.PendingDispositionFor(older.sequenceNumber, olderId) {
				t.Fatal("selected-copy ACK was not left with its ordinary owner")
			}
		}
		if beforeApplication != nil {
			beforeApplication(t, sequence, older, route)
		}
		close(resume)
		read := func() *protocol.Pack {
			var wire []byte
			select {
			case wire = <-route:
			case <-finished:
				if len(route) == 0 {
					t.Fatal("synchronous continuation ended without its physical Pack")
				}
				wire = <-route
			}
			pack := decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
			return pack
		}
		first := read()
		if !acknowledge {
			if string(first.MessageId) != string(olderId[:]) || !first.Head || first.ContractAhead {
				t.Fatal("next application entry overtook the already-selected older physical retry")
			}
			first = read()
		}
		<-finished
		if first.ContractAhead || first.SequenceNumber != older.sequenceNumber+2 || len(first.Frames) != 1 ||
			sequence.pendingRecovery != nil || len(route) != 0 || older.sendTime.Add(older.ackTimeout) != lifetime {
			t.Fatal("synchronous continuation lost application identity, selection retirement or the immutable lifetime")
		}
		wantRetries := uint64(1)
		if acknowledge {
			wantRetries = 0
		}
		if sequence.resendWriteCount.Load() != wantRetries || len(sequence.sendItems) != 3 || callbackCounts != [3]int{} {
			t.Fatal("synchronous continuation duplicated recovery or moved original ACK ownership")
		}
		if afterApplication != nil {
			afterApplication(t, sequence, older, route)
		}
	})
}

// The announcement's completed FIFO entry precedes its selected older copy,
// and that copy precedes the application entry on the same worker call stack.
func TestWindowPacingOwnerSelectionContractContinuationOrdersRecovery(t *testing.T) {
	runWindowOwnerContractContinuation(t, false, nil, nil)
}

// A real selective ACK between announcement and application invalidates the
// selected copy without consuming the ACK or suppressing the new application.
func TestWindowPacingOwnerSelectionContractContinuationAckPreempts(t *testing.T) {
	runWindowOwnerContractContinuation(t, true, nil, nil)
}
