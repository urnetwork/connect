// The ACK worker's compression wait re-arms its timer only for a new
// deadline, while every update is still acknowledged by its deadline and a
// burst's quiet head leaves exactly one quiet interval after its last arrival.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Wakes that leave the wait's deadline where it was do not re-arm the timer,
// and the quiet head still leaves at its exact deadline.
func TestReceiveSequenceAckCompressWaitKeepsAnUnchangedDeadlineArmed(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const compressTimeout = 10 * time.Millisecond
		const quiet = compressTimeout / 10
		const wakeCount = 16
		arms := atomic.Int64{}
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = compressTimeout
			settings.AckGapWakeSelectiveCount = 0
			settings.afterAckCompressTimerArmForTest = func(receiveSequenceId) {
				arms.Add(1)
			}
		})
		sequence.prime(t)

		// a burst quantum lands, and the wait arms for its quiet deadline
		want := deliverBurstTailTestHead(t, sequence, 1, 100*ackResponseEntryMaxByteCount)
		deliveredAt := time.Now()
		synctest.Wait()
		before := arms.Load()
		// quantum signals that move no deadline, as a signal raised before the
		// worker saw the quantum does
		for range wakeCount {
			sequence.receiveSequence.ackWindow.headQuietNotify <- struct{}{}
			synctest.Wait()
		}
		if rearms := arms.Load() - before; rearms != 0 {
			t.Fatalf("the compression wait re-armed its timer %d times for %d wakes that left its deadline unchanged", rearms, wakeCount)
		}

		time.Sleep(quiet)
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want || ack.Selective {
			t.Fatalf("the quiet head was not the delivered head: %v", ack)
		}
		if elapsed := time.Since(deliveredAt); elapsed != quiet {
			t.Fatalf("the quiet head left %s after its arrival, want %s", elapsed, quiet)
		}
	})
}

// Continuous H1 progress keeps the burst tail's quiet check from ever firing
// early. Each interval still ends with exactly one cumulative ACK for the
// newest head at its deadline, and the first ACK after a lull leaves at once.
func TestReceiveSequenceAckCompressWaitHotIntervalsKeepTheirDeadlines(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const compressTimeout = 10 * time.Millisecond
		const arrivalInterval = 500 * time.Microsecond
		const intervalCount = 10
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = compressTimeout
			settings.AckGapWakeSelectiveCount = 0
		})
		sequence.prime(t)

		number := uint64(0)
		for interval := range intervalCount {
			intervalStart := time.Now()
			var want Id
			// a full quantum every arrival keeps the head eligible and never quiet
			for range int(compressTimeout / arrivalInterval) {
				number++
				want = deliverBurstTailTestHead(t, sequence, number, 1024*1024)
				time.Sleep(arrivalInterval)
				synctest.Wait()
				if time.Now().Before(intervalStart.Add(compressTimeout)) && len(sequence.route) != 0 {
					t.Fatalf("interval %d: continuous progress was acknowledged before its deadline", interval)
				}
			}
			ack := readBurstTailTestAck(t, sequence)
			got, err := IdFromBytes(ack.MessageId)
			if err != nil || got != want || ack.Selective {
				t.Fatalf("interval %d: the deadline response did not carry the newest head", interval)
			}
			if len(sequence.route) != 0 {
				t.Fatalf("interval %d: more than one response at the deadline", interval)
			}
		}

		// a lull: the next ACK is immediate
		time.Sleep(2 * compressTimeout)
		number++
		want := deliverBurstTailTestHead(t, sequence, number, 1)
		ack := readBurstTailTestAck(t, sequence)
		got, err := IdFromBytes(ack.MessageId)
		if err != nil || got != want {
			t.Fatal("the first ACK after a lull waited for compression")
		}
	})
}
