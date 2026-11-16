// The pure-ACK worker arms its compression timer only to coalesce data that
// arrives inside the interval after an acknowledgement, with the same
// acknowledgement schedule. Every event runs in virtual time.
package connect

import (
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Data from the source more than AckCompressTimeout apart is acknowledged on
// arrival, and the worker takes no timer wake for it: the compression timer
// used to run out after every acknowledgement whether or not data followed.
func TestTcpFlowAcknowledgesSparseDataWithoutATimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		compressTimeout := settings.AckCompressTimeout
		if compressTimeout <= 0 {
			t.Fatal("the default flow does not compress acknowledgements")
		}
		wakes := atomic.Int64{}
		f := newTcpReapTestFlowWithSetup(t, settings, func(sequence *TcpSequence) {
			sequence.afterAckWaitWakeForTest = func() {
				wakes.Add(1)
			}
		})
		go io.Copy(io.Discard, f.upstream)
		f.takeAcks()

		const segmentCount = 8
		for index := range segmentCount {
			time.Sleep(4 * compressTimeout)
			sentAt := time.Now()
			f.send(f.newSendItem([]byte{byte(index)}))
			synctest.Wait()
			acks := f.takeAcks()
			if len(acks) != 1 || !acks[0].at.Equal(sentAt) || acks[0].ackNumber != f.sourceSeq {
				t.Fatalf("segment %d: acknowledgements %+v, want one at once for %d", index, acks, f.sourceSeq)
			}
		}
		time.Sleep(4 * compressTimeout)
		synctest.Wait()
		if got := wakes.Load(); got != 0 {
			t.Errorf("%d sparse segments woke the acknowledgement worker's timer %d times, want none", segmentCount, got)
		}
	})
}

// Data inside the interval after an acknowledgement is acknowledged together,
// exactly AckCompressTimeout after that acknowledgement.
func TestTcpFlowCoalescesDataInsideTheCompressionInterval(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		compressTimeout := settings.AckCompressTimeout
		f := newTcpReapTestFlow(t, settings)
		go io.Copy(io.Discard, f.upstream)
		f.takeAcks()

		time.Sleep(4 * compressTimeout)
		f.send(f.newSendItem([]byte("a")))
		synctest.Wait()
		acks := f.takeAcks()
		if len(acks) != 1 {
			t.Fatalf("the first segment drew %d acknowledgements, want one at once", len(acks))
		}
		firstAckAt := acks[0].at

		time.Sleep(compressTimeout / 5)
		f.send(f.newSendItem([]byte("b")))
		time.Sleep(compressTimeout / 5)
		f.send(f.newSendItem([]byte("c")))
		synctest.Wait()
		time.Sleep(firstAckAt.Add(compressTimeout).Sub(time.Now()) - time.Nanosecond)
		synctest.Wait()
		if acks := f.takeAcks(); len(acks) != 0 {
			t.Fatalf("data inside the interval was acknowledged %s after the last acknowledgement", acks[0].at.Sub(firstAckAt))
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		acks = f.takeAcks()
		if len(acks) != 1 || !acks[0].at.Equal(firstAckAt.Add(compressTimeout)) || acks[0].ackNumber != f.sourceSeq {
			t.Fatalf("coalesced acknowledgements %+v, want one for %d at %s after the last", acks, f.sourceSeq, compressTimeout)
		}
	})
}
