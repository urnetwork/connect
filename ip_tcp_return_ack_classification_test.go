// Native return loss evidence follows valid ACK headers and their raw windows.
package connect

import (
	"context"
	"io"
	"math"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An unknown or invalid first header cannot supply duplicate loss credit.
func TestTcpReturnFirstValidAckStartsWindowHistory(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		ctx, cancel := context.WithCancel(t.Context())
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		settings.Mtu = 44
		settings.Log = NewNoopLogger()
		settings.MemoryBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
		replayed := make(chan struct{}, 1)
		sequence := NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, _ []byte) {
			select {
			case replayed <- struct{}{}:
			default:
				t.Error("the first-header control emitted more than its single bounded replay")
			}
		}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, base-1, settings)
		if sequence == nil {
			cancel()
			t.Fatal("the first-header control could not reserve its flow")
		}
		// Isolate the post-emission transition with no earlier valid ACK
		// header, rather than counting an unknown first window as a duplicate.
		sequence.established = true
		sequence.receiveSeqAck, sequence.receiveSeq = base, base+8
		sequence.receiveWindowSize, sequence.receiveWindowEnd, sequence.receiveWindowEndSet = 128, base+128, true
		if !sequence.retainReturnChunk([]byte("abcdefgh"), base, false) {
			cancel()
			sequence.memory.release()
			t.Fatal("the first-header control could not retain its original bytes")
		}
		done := make(chan struct{})
		go func() { defer close(done); sequence.runReturnRecovery() }()
		defer func() {
			cancel()
			<-done
			sequence.releaseReturnChunks()
			sequence.memory.release()
			if settings.MemoryBudget.UsedByteCount() != 0 {
				t.Error("the joined first-header control retained flow memory")
			}
		}()
		synctest.Wait()
		started := time.Now()
		acknowledgeTcpReturnFlight(t, sequence, base-1, 129)
		acknowledgeTcpReturnFlight(t, sequence, base+9, 130)
		sequence.mutex.Lock()
		unknown := !sequence.returnAckWindowSeen && sequence.returnDuplicateAcks == 0
		sequence.mutex.Unlock()
		if !unknown {
			t.Fatal("an invalid first ACK invented a raw-window baseline")
		}
		acknowledgeTcpReturnFlight(t, sequence, base, 128)
		synctest.Wait()
		sequence.mutex.Lock()
		primed := sequence.returnAckWindowSeen && sequence.returnAckWindow == 128 && sequence.returnDuplicateAcks == 0
		sequence.mutex.Unlock()
		if !primed || len(replayed) != 0 {
			t.Fatal("the first valid ACK counted as duplicate loss evidence")
		}
		for range 2 {
			acknowledgeTcpReturnFlight(t, sequence, base, 128)
			synctest.Wait()
		}
		if len(replayed) != 0 {
			t.Fatal("fewer than three genuine duplicates started return recovery")
		}
		acknowledgeTcpReturnFlight(t, sequence, base, 128)
		synctest.Wait()
		if len(replayed) != 1 || !time.Now().Equal(started) {
			t.Fatal("three genuine duplicates after the first header lost immediate return recovery")
		}
	})
}

// Window or payload/control feedback supplies no new duplicate credit. It
// does not erase the same cumulative frontier's two earlier genuine credits.
func TestTcpReturnNonDuplicateFeedbackPreservesLossEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, feedback := range []string{"window", "payload", "syn", "fin"} {
		synctest.Test(t, func(t *testing.T) {
			const base = uint32(101)
			f := newTcpReturnFlightFixture(t, 1, base, []byte("abcdefgh"), 128, nil)
			defer f.close()
			f.ack(base, 128)
			f.ack(base, 128)
			sequence := f.sequence
			sequence.mutex.Lock()
			progress, retained := sequence.returnProgressTime, sequence.returnByteCount
			sequence.mutex.Unlock()
			started := time.Now()
			tcp := parsedTcp{ack: true, ackNumber: base, windowSize: 128}
			switch feedback {
			case "window":
				tcp.windowSize = 129
			case "payload":
				tcp.payload = []byte("x")
			case "syn":
				tcp.syn = true
			case "fin":
				tcp.fin = true
			}
			// The queued payload/control path and inline pure-ACK path share
			// this validated transition. The native root below composes DATA.
			sequence.mutex.Lock()
			sequence.applySendAckWithLock(&tcp)
			sequence.mutex.Unlock()
			synctest.Wait()
			sequence.mutex.Lock()
			preserved := sequence.returnDuplicateAcks == 2 && sequence.returnAttempts == 0 &&
				sequence.receiveSeqAck == base && sequence.returnByteCount == retained &&
				sequence.returnProgressTime.Equal(progress) && sequence.returnAckWindowSeen &&
				sequence.returnAckWindow == tcp.windowSize
			sequence.mutex.Unlock()
			if !preserved || len(f.snapshot()) != 0 || !time.Now().Equal(started) {
				t.Fatalf("%s feedback changed genuine prior loss evidence or its timer", feedback)
			}
			if feedback != "window" {
				// A valid ACK-bearing payload/control header also becomes the
				// raw-window baseline when its advertised value changes.
				tcp.windowSize = 129
				sequence.mutex.Lock()
				sequence.applySendAckWithLock(&tcp)
				sequence.mutex.Unlock()
				synctest.Wait()
				sequence.mutex.Lock()
				preserved = sequence.returnDuplicateAcks == 2 && sequence.returnAttempts == 0 &&
					sequence.receiveSeqAck == base && sequence.returnByteCount == retained &&
					sequence.returnProgressTime.Equal(progress) && sequence.returnAckWindow == 129
				sequence.mutex.Unlock()
				if !preserved || len(f.snapshot()) != 0 || !time.Now().Equal(started) {
					t.Fatalf("%s feedback lost its valid changed raw-window history", feedback)
				}
			}
			// This header equals the last valid raw window, including a changed
			// window's new value. It is the third qualifying duplicate.
			f.ack(base, tcp.windowSize)
			events := requireTcpReturnFlightCount(t, f, 1)
			if events[0].seq != base || events[0].payload != "abcd" || !time.Now().Equal(started) {
				t.Fatalf("%s feedback erased the earlier two genuine duplicate credits", feedback)
			}
		})
	}
}

// Invalid ACK numbers cannot become the previous valid window, including at
// the sequence-number wrap. Two genuine credits survive both invalid inputs.
func TestTcpReturnInvalidAckCannotChangeWindowHistory(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, base := range []uint32{101, math.MaxUint32 - 3} {
		synctest.Test(t, func(t *testing.T) {
			f := newTcpReturnFlightFixture(t, 1, base, []byte("abcdefgh"), 128, nil)
			defer f.close()
			f.ack(base, 128)
			f.ack(base, 128)
			sequence := f.sequence
			sequence.mutex.Lock()
			progress, retained, edge := sequence.returnProgressTime, sequence.returnByteCount, sequence.receiveWindowEnd
			sequence.mutex.Unlock()
			started := time.Now()
			f.ack(base-1, 201)
			f.ack(base+9, 202)
			sequence.mutex.Lock()
			unchanged := sequence.returnDuplicateAcks == 2 && sequence.returnAttempts == 0 &&
				sequence.returnAckWindowSeen && sequence.returnAckWindow == 128 &&
				sequence.receiveSeqAck == base && sequence.receiveWindowEnd == edge &&
				sequence.returnByteCount == retained && sequence.returnProgressTime.Equal(progress)
			sequence.mutex.Unlock()
			if !unchanged || len(f.snapshot()) != 0 {
				t.Fatalf("invalid feedback poisoned the current window or loss frontier at base %d", base)
			}
			f.ack(base, 128)
			events := requireTcpReturnFlightCount(t, f, 1)
			if events[0].seq != base || events[0].payload != "abcd" || !time.Now().Equal(started) {
				t.Fatalf("invalid feedback prevented genuine duplicate recovery at base %d", base)
			}
		})
	}
}

// Valid window history survives an empty flight and primes its next return.
func TestTcpReturnEmptyFlightStillRecordsValidWindow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 1, base, []byte("abcd"), 128, nil)
		defer f.close()
		sequence := f.sequence
		f.ack(base+4, 128)
		f.ack(base+4, 129)
		sequence.mutex.Lock()
		primed := sequence.returnHead == len(sequence.returnChunks) && sequence.returnByteCount == 0 &&
			sequence.returnAckWindowSeen && sequence.returnAckWindow == 129 && sequence.returnDuplicateAcks == 0
		// Model the next original packet being emitted, as the socket reader
		// in Run does before its later duplicate ACKs can arrive.
		sequence.receiveSeq += 4
		sequence.mutex.Unlock()
		if !primed || !sequence.retainReturnChunk([]byte("efgh"), base+4, false) {
			t.Fatal("an empty retained flight lost a valid advertised-window baseline")
		}
		f.ack(base+4, 129)
		sequence.mutex.Lock()
		oneDuplicate := sequence.returnDuplicateAcks == 1 && sequence.returnAttempts == 0
		sequence.mutex.Unlock()
		if !oneDuplicate || len(f.snapshot()) != 0 {
			t.Fatal("the first later duplicate did not use the earlier valid empty-flight header")
		}
		f.ack(base+4, 129)
		f.ack(base+4, 129)
		events := requireTcpReturnFlightCount(t, f, 1)
		if events[0].seq != base+4 || events[0].payload != "efgh" {
			t.Fatal("genuine duplicate recovery lost the next retained flight")
		}
	})
}

// Window-only progress cannot continually defer the independent lost-tail
// timer. Virtual time checks the original deadline on both sides of its edge.
func TestTcpReturnWindowProgressKeepsOriginalResendDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const base = uint32(101)
		f := newTcpReturnFlightFixture(t, 1, base, []byte("abcd"), 128, nil)
		defer f.close()
		sequence := f.sequence
		sequence.mutex.Lock()
		progress := sequence.returnProgressTime
		sequence.mutex.Unlock()
		timeout := sequence.tcpBufferSettings.ReturnResendTimeout
		time.Sleep(timeout / 4)
		f.ack(base, 129)
		time.Sleep(timeout / 4)
		f.ack(base, 130)
		sequence.mutex.Lock()
		unchanged := sequence.returnProgressTime.Equal(progress) && sequence.returnDuplicateAcks == 0 && sequence.returnAttempts == 0
		sequence.mutex.Unlock()
		if !unchanged {
			t.Fatal("window-only feedback changed the native return progress clock or loss credit")
		}
		deadline := progress.Add(timeout)
		time.Sleep(deadline.Sub(time.Now()) - time.Nanosecond)
		requireTcpReturnFlightCount(t, f, 0)
		time.Sleep(time.Nanosecond)
		events := requireTcpReturnFlightCount(t, f, 1)
		if !time.Now().Equal(deadline) || events[0].seq != base || events[0].payload != "abcd" {
			t.Fatal("window progress postponed or changed the independent native lost-tail replay")
		}
	})
}

// ACK-bearing upload DATA goes through Multi, Transfer, the provider queue and
// the actual origin writer. Its ACK half cannot supply duplicate loss credit.
func TestNativePayloadAcksDoNotCountAsDuplicateAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeDuplicateAckFixture(t, true)
		uploadDone := make(chan struct{})
		uploadStarted := false
		defer func() {
			f.close()
			if uploadStarted {
				<-uploadDone
			}
		}()
		body := "abcdefghijklmnop"
		if count, err := f.origin.Write([]byte(body)); err != nil || count != len(body) {
			t.Fatalf("actual native origin flight: count=%d err=%v", count, err)
		}
		synctest.Wait()
		sequence := f.providerSequence.Load()
		f.consumer.stateLock.Lock()
		base, packets, pending := f.consumer.base, f.consumer.packets, len(f.consumer.pending)
		f.consumer.stateLock.Unlock()
		sequence.mutex.Lock()
		retained, progress := sequence.returnByteCount, sequence.returnProgressTime
		sequence.mutex.Unlock()
		if packets != 4 || pending != 3 || retained <= 0 {
			t.Fatal("the actual native return did not retain its exact post-Transfer lost-head premise")
		}
		type uploadResult struct {
			payload string
			err     error
		}
		uploaded := make(chan uploadResult, 1)
		uploadStarted = true
		go func() {
			defer close(uploadDone)
			payload := make([]byte, 3)
			_, err := io.ReadFull(f.origin, payload)
			uploaded <- uploadResult{payload: string(payload), err: err}
		}()
		started := time.Now()
		for index, value := range []byte("xyz") {
			if !f.offer(groupRecoveryPacket(f.initial+1+uint32(index), base, f.window, tcpFlagAck, []byte{value})) {
				t.Fatal("actual native ACK-bearing upload DATA was refused")
			}
			synctest.Wait()
		}
		select {
		case result := <-uploaded:
			if result.err != nil || result.payload != "xyz" {
				t.Fatalf("actual queued DATA upload: payload=%q err=%v", result.payload, result.err)
			}
		default:
			t.Fatal("the actual origin did not consume all three queued ACK-bearing DATA packets")
		}
		sequence.mutex.Lock()
		unchanged := sequence.returnDuplicateAcks == 0 && sequence.returnAttempts == 0 &&
			sequence.receiveSeqAck == base && sequence.returnByteCount == retained && sequence.returnProgressTime.Equal(progress)
		sequence.mutex.Unlock()
		f.consumer.stateLock.Lock()
		replays := f.consumer.replays
		f.consumer.stateLock.Unlock()
		if !unchanged || replays != 0 || !time.Now().Equal(started) {
			t.Fatal("three actual queued payload ACKs became native duplicate-ACK loss evidence")
		}
		for range 3 {
			if !f.offer(groupRecoveryPacket(f.initial+4, base, f.window, tcpFlagAck, nil)) {
				t.Fatal("actual pure duplicate ACK after upload was refused")
			}
			synctest.Wait()
		}
		f.consumer.stateLock.Lock()
		received, replays := f.consumer.received, f.consumer.replays
		f.consumer.stateLock.Unlock()
		if received != body || replays != 1 || !time.Now().Equal(started) {
			t.Fatal("three genuine native duplicate ACKs lost fast repair after payload feedback")
		}
	})
}
