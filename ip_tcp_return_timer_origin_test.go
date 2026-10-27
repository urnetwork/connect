// Return recovery starts its timer when bytes are retained, before the
// original packet reaches a later Transfer/TUN loss boundary.
package connect

import (
	"bytes"
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A positive delivery delay makes a genuine timeout's receiver hole shorter
// than the unchanged configured timeout. No acknowledgement drives this replay.
func TestTcpReturnTimerStartsBeforeDelayedReceiverDrop(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		settings.Log = NewNoopLogger()
		configuredTimeout := settings.ReturnResendTimeout
		if configuredTimeout <= 4*time.Nanosecond {
			t.Fatal("default return resend timeout cannot define the delayed-delivery control")
		}
		const base = uint32(101)
		payload := []byte("lost")
		replays := make(chan time.Time, 2)
		sequence := NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			_, source, destination, transport, valid := parseIpv4(packet)
			var tcp parsedTcp
			if !valid || !parseTcpPacket(source, destination, transport, &tcp) ||
				tcp.seq != base || !bytes.Equal(tcp.payload, payload) {
				t.Error("the timer did not replay the retained missing sequence and bytes")
			}
			replays <- time.Now()
		}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, base-1, settings)
		if sequence == nil {
			t.Fatal("timer control could not reserve a TCP flow")
		}
		defer sequence.memory.release()
		defer sequence.releaseReturnChunks()
		sequence.receiveSeqAck = base
		sequence.receiveSeq = base + uint32(len(payload))
		if !sequence.retainReturnChunk(payload, base, false) {
			t.Fatal("timer control could not retain the original response bytes")
		}
		sequence.mutex.Lock()
		retainedAt := sequence.returnProgressTime
		noFeedback := sequence.returnDuplicateAcks == 0 && sequence.returnAttempts == 0 && !sequence.returnRecoveryReady
		sequence.mutex.Unlock()
		if retainedAt.IsZero() || !noFeedback {
			t.Fatal("timer control started with an earlier recovery or ACK trigger")
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			sequence.runReturnRecovery()
		}()
		defer func() {
			cancel()
			<-done
		}()
		synctest.Wait()
		// Model elapsed delivery time after retention, then lose the original.
		// This moves no timer and sends no synthetic ACK or window update.
		deliveryDelay := configuredTimeout / 4
		time.Sleep(deliveryDelay)
		synctest.Wait()
		droppedAt := time.Now()
		deadline := retainedAt.Add(configuredTimeout)
		remaining := deadline.Sub(droppedAt)
		if remaining <= time.Nanosecond {
			t.Fatal("the original was not dropped before the retention deadline")
		}
		time.Sleep(remaining - time.Nanosecond)
		synctest.Wait()
		select {
		case <-replays:
			t.Fatal("a replay preceded the unchanged retention-based timeout without ACK feedback")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		var replayedAt time.Time
		select {
		case replayedAt = <-replays:
		default:
			t.Fatal("the retention-based timeout did not replay the missing bytes")
		}
		hole := replayedAt.Sub(droppedAt)
		if !replayedAt.Equal(deadline) || hole != configuredTimeout-deliveryDelay || hole >= configuredTimeout {
			t.Fatalf("wrong timer origin: retained=%s dropped=%s replayed=%s timer=%s hole=%s", retainedAt, droppedAt, replayedAt, configuredTimeout, hole)
		}
		t.Logf("retention_to_replay=%s delivery_before_drop=%s receiver_hole=%s configured_timeout=%s zero_feedback=true", replayedAt.Sub(retainedAt), droppedAt.Sub(retainedAt), hole, configuredTimeout)
	})
}
