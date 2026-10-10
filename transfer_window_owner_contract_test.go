// Synchronous contract announcements return directly into application sending,
// so selected recovery must be consumed before that next entry owns a waiter.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The same owned call stack sends an actual announcement followed by an actual
// application Pack. No Run iteration is inserted between those two entries.
func runWindowOwnerContractContinuation(t *testing.T, acknowledge bool, beforeApplication, afterApplication func(*testing.T, *SendSequence, *sendItem, Route)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, older, _, route, _, _ := newWindowRouteAccountingFixture(t, 1)
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
		MessagePoolReturn(<-route)
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		expiry := service.burst.start.Add(windowPacingBurstMaximumTime(service.burstEstimateTime))
		service.stateLock.Unlock()
		if !expiry.After(time.Now()) || !expiry.Before(due) || due != physical.Add(2*time.Second) {
			t.Fatal("fixture did not preserve the cold physical recovery boundary")
		}
		time.Sleep(time.Until(expiry))
		service.stateLock.Lock()
		service.next = due.Add(time.Second)
		service.stateLock.Unlock()
		ahead := newContractAheadTestContract(t, sequence.client, sequence.destination)
		announced, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(finished)
			sequence.sendContractAheadAnnouncement(ahead, func(error) {})
			close(announced)
			select {
			case <-sequence.ctx.Done():
				return
			case <-resume:
			}
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(32)}
			clear(frame.MessageBytes)
			sequence.send([]*protocol.Frame{frame}, func(error) {}, true, true)
		}()
		defer func() {
			sequence.cancel()
			<-finished
			sequence.pendingRecovery = nil
			sequence.ackLifetimes.clear()
			sequence.releaseRetainedSendItems(context.Canceled)
		}()
		synctest.Wait()
		deadline := sequence.windowPacer.waiter.deadline
		service.stateLock.Lock()
		chargedNext, chargedProbe := service.next, service.probeSent
		owned := service.pacingReservations == 1 && service.waiterHead == &sequence.windowPacer.waiter
		service.stateLock.Unlock()
		if !owned || !deadline.After(due) || len(route) != 0 || len(sequence.sendItems) != 2 {
			t.Fatal("actual announcement did not retain one charged original reservation")
		}
		time.Sleep(time.Until(due.Add(-time.Nanosecond)))
		synctest.Wait()
		if sequence.pendingRecovery != nil || older.head || len(route) != 0 {
			t.Fatal("announcement selected or dispatched the older retry before its real due time")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resumeInspection, due)
		service.stateLock.Lock()
		kept := service.next == chargedNext && service.probeSent == chargedProbe && service.pacingReservations == 1
		service.stateLock.Unlock()
		if !kept || sequence.pendingRecovery == nil || sequence.pendingRecovery.messageId != olderId ||
			!older.head || !older.promotedHead || older.sendTime.Add(older.ackTimeout) != lifetime || len(route) != 0 {
			t.Fatal("announcement wait did not perform real due preparation while preserving its charged owner")
		}
		time.Sleep(time.Until(deadline))
		synctest.Wait()
		select {
		case <-announced:
		default:
			t.Fatal("paid announcement did not finish before the next synchronous entry")
		}
		if len(route) != 1 || sequence.pendingRecovery == nil {
			t.Fatal("announcement return lost either its physical result or selected recovery")
		}
		wire := <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if !pack.ContractAhead || pack.SequenceNumber != older.sequenceNumber+1 || len(pack.Frames) != 0 {
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
		if sequence.resendWriteCount.Load() != wantRetries || len(sequence.sendItems) != 3 {
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
