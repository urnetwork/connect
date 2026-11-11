package connect

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Two real receiver evictions occupy the two existing phases while Run owns
// the first cut. Natural unchanged ACK expiry then exits before that cut's
// ordinary apply site. Closing must reuse the cut rather than take it twice.
func TestTransferOwnedWithdrawalCutSurvivesAckExpiryClose(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var destination Id
		var sequence *SendSequence
		var armed atomic.Bool
		var terminals, successes atomic.Int32
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.afterTakePendingEvictionsForTest = func(id sendSequenceId) {
				if !armed.Load() || id.Destination != destination {
					return
				}
				sequence.resendQueue.stateLock.Lock()
				owned := sequence.pendingEvictionInFlight != 0
				sequence.resendQueue.stateLock.Unlock()
				if owned {
					barrier.wait()
				}
			}
			settings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				if event.DestinationId == destination && event.Phase == SendPackLifecyclePhaseTerminal {
					terminals.Add(1)
					if event.Err == nil {
						successes.Add(1)
					}
				}
			}
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 150
			settings.ReceiveHoldPolicy = ReceiveHoldEvict
			settings.EvictionNotice = true
		}, TransportTypeH1)
		destination = fixture.receiver.ClientId()
		var originals [5]*windowRoundFrame
		for index := range originals {
			originals[index] = fixture.write(64)
		}
		sequence = fixture.sequence()
		accept := func(reply *windowRoundFrame) {
			t.Helper()
			if reply.ack == nil || !reply.ack.Selective || !fixture.sender.sendBuffer.Ack(destination, reply.ack, 0) {
				t.Fatal("real future receiver feedback was not accepted selectively")
			}
			fixture.drop(reply)
		}
		accept(fixture.receive(originals[3]))
		accept(fixture.receive(originals[4]))
		synctest.Wait()
		if fixture.deliveredCount != 0 || sequence.ackTimeoutDisposition.Load() != sendAckTimeoutOrdinary {
			t.Fatal("withdrawal fixture advanced the missing prefix or changed ordinary ACK lifetime")
		}
		armed.Store(true)
		withdraw := func(original *windowRoundFrame, wanted uint64) {
			t.Helper()
			fixture.forward(original, fixture.receiverIn)
			replies := fixture.takeAcknowledgements()
			found := false
			for _, reply := range replies {
				for _, number := range reply.ack.EvictedSequenceNumbers {
					if number != wanted {
						t.Fatal("actual eviction withdrew another owner")
					}
					found = true
				}
				accept(reply)
			}
			if !found {
				t.Fatal("earlier real DATA arrival did not withdraw the captured tail")
			}
		}
		withdraw(originals[2], originals[4].pack.SequenceNumber)
		barrier.requireReached(t)
		// The actual source owner is durably paused. Change only its test
		// logger so its existing abnormal-exit report is retained after join.
		logger := newRecordingLogger()
		sequence.log = logger
		withdraw(originals[1], originals[3].pack.SequenceNumber)
		sequence.resendQueue.stateLock.Lock()
		owned := sequence.pendingEvictionInFlight
		pending := sequence.pendingEvictionPhases
		next := sequence.pendingEvictionPhaseWithLock()
		sequence.resendQueue.stateLock.Unlock()
		if owned == 0 || pending&next == 0 || next == owned || fixture.receiver.ReceiveStats().ReceiveQueueEvictionCount != 2 {
			t.Fatal("two actual withdrawals did not occupy an owned cut and the following phase")
		}
		if sequence.sendBufferSettings.AckTimeout <= 0 {
			t.Fatal("unchanged ACK lifetime is not finite")
		}
		time.Sleep(sequence.sendBufferSettings.AckTimeout + time.Nanosecond)
		barrier.close()
		<-sequence.done
		synctest.Wait()
		if successes.Load() != 0 || terminals.Load() != 5 || fixture.ackedCount != 0 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("expiry close claimed delivery or retained an original/lifetime")
		}
		fixture.cancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		for _, line := range logger.lines() {
			if strings.Contains(line, "another pending eviction cut is still owned") {
				t.Error("ACK_CLOSE_RETOOK_OWNED_WITHDRAWAL_CUT: natural owner expiry panicked after two actual receiver withdrawals")
			}
		}
		t.Log("ACK_CLOSE_OWNED_CUT_JOINED: real_withdrawals=2 terminals=5 delivered=0 all_clients_joined=true")
	})
}
