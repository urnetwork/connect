package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Reuses the actual native/Multi/Transfer/provider constructor hierarchy.
// The bounded NAT copies the explicit policy before it creates TcpSequence;
// pure native ACKs retain their existing independent admission partition.
func TestNativeReturnFlightUsesConfiguredProviderPolicy(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeDuplicateAckFixture(t, true, 4)
		defer f.close()
		body := "abcdefghijklmnop"
		if count, err := f.origin.Write([]byte(body)); err != nil || count != len(body) {
			t.Fatalf("real origin write: count=%d err=%v", count, err)
		}
		synctest.Wait()
		sequence := f.providerSequence.Load()
		if sequence == nil || sequence.tcpBufferSettings.ReturnRecoveryMaxPacketCount != 4 {
			t.Fatal("actual provider constructor did not copy its configured recovery bound")
		}
		f.consumer.stateLock.Lock()
		base, next, packets, pending := f.consumer.base, f.consumer.next, f.consumer.packets, len(f.consumer.pending)
		f.consumer.stateLock.Unlock()
		if next != base || packets != 4 || pending != 3 {
			t.Fatal("real post-Transfer native receiver did not retain the exact lost-head premise")
		}
		before := time.Now()
		for range 3 {
			if !f.offer(groupRecoveryPacket(f.initial+1, base, f.window, tcpFlagAck, nil)) {
				t.Fatal("ordinary native duplicate ACK was refused")
			}
			synctest.Wait()
		}
		f.consumer.stateLock.Lock()
		received, next, packets := f.consumer.received, f.consumer.next, f.consumer.packets
		f.consumer.stateLock.Unlock()
		sequence.mutex.Lock()
		attempts, retained := sequence.returnAttempts, sequence.returnByteCount
		sequence.mutex.Unlock()
		queued, queuedBytes, _ := f.receiver.ResendQueueSize(f.sender.ClientId(), MultiHopId{}, false, false)
		if received != body || next != base+uint32(len(body)) || packets != 8 || attempts != 1 || retained <= 0 ||
			queued != 0 || queuedBytes != 0 || time.Since(before) != 0 {
			t.Fatalf("actual native bounded flight: received=%q packets=%d attempts=%d retained=%d transfer=%d/%d elapsed=%s",
				received, packets, attempts, retained, queued, queuedBytes, time.Since(before))
		}
		if !f.offer(groupRecoveryPacket(f.initial+1, next, f.window, tcpFlagAck, nil)) {
			t.Fatal("final native cumulative ACK was refused")
		}
		synctest.Wait()
		sequence.mutex.Lock()
		retained = sequence.returnByteCount
		sequence.mutex.Unlock()
		if retained != 0 {
			t.Fatal("final native ACK did not release exact retained origin custody")
		}
	})
}

// Accepted native ACKs retain the greatest right edge. A zero Window header
// cannot close an outstanding DATA range before its cumulative ACK reaches it.
func TestNativeReturnFlightWindowHeaderCannotShrinkOutstandingEdge(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeDuplicateAckFixtureWithWindow(t, true, 16, 4)
		defer f.close()
		body := "abcdefghijklmnop"
		if count, err := f.origin.Write([]byte(body)); err != nil || count != len(body) {
			t.Fatalf("native window-control origin write: count=%d err=%v", count, err)
		}
		synctest.Wait()
		f.consumer.stateLock.Lock()
		base := f.consumer.base
		f.consumer.stateLock.Unlock()
		sequence := f.providerSequence.Load()
		for range 3 {
			if !f.offer(groupRecoveryPacket(f.initial+1, base, 0, tcpFlagAck, nil)) {
				t.Fatal("native zero-window-header ACK was refused")
			}
			synctest.Wait()
			sequence.mutex.Lock()
			preserved := sequence.receiveWindowEnd == base+16 && sequence.receiveWindowSize == 16 && sequence.receiveSeqAck == base
			sequence.mutex.Unlock()
			if !preserved {
				t.Fatal("an unchanged ACK shrank the greatest advertised right edge")
			}
		}
		f.consumer.stateLock.Lock()
		next, received := f.consumer.next, f.consumer.received
		f.consumer.stateLock.Unlock()
		if received != body || next != base+16 {
			t.Fatal("native zero-header feedback lost the original bounded repair")
		}
		if !f.offer(groupRecoveryPacket(f.initial+1, next, 0, tcpFlagAck, nil)) {
			t.Fatal("native ACK closing the old edge was refused")
		}
		synctest.Wait()
		sequence.mutex.Lock()
		closed := sequence.receiveWindowSize == 0 && sequence.receiveSeqAck == next && sequence.returnByteCount == 0
		sequence.mutex.Unlock()
		if !closed {
			t.Fatal("effective zero window retained already-ACKed ordinary DATA")
		}
	})
}
