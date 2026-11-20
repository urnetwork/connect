// A finite loss train keeps distinct protocol originals observable. A byte
// exemption does not erase their wire bytes or collapse repeated TCP state.
package connect

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Public writes repeat one ACK state, lose every physical copy through three
// retry ceilings, and cancel the retained child without refunding sibling data.
func TestProtocolControlNoBudgetRepeatedAckLossRetainsActualOriginals(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newProtocolDataPressureFixture(t, 4, false, func(settings *SendBufferSettings) {
			settings.LiveDiagnostics = true
		})
		const originalCount = 16
		results := make([]protocolPressureCompletion, originalCount)
		type wireOriginal struct {
			number          uint64
			encoded, pooled ByteCount
		}
		identities := make(map[Id]wireOriginal, originalCount)
		var sequence *SendSequence
		var tcpState []byte
		for index := range results {
			if admitted, err := fixture.send(t, 2, tcpFlagAck, 0, true, &results[index], time.Second); !admitted || err != nil {
				t.Fatalf("public repeated ACK %d admission=%t error=%v", index, admitted, err)
			}
			wire := fixture.take(t, 2, uint64(index+1))
			messageId := RequireIdFromBytes(wire.pack.MessageId)
			if _, duplicate := identities[messageId]; duplicate {
				t.Fatal("separate reliable originals reused one transfer message identity")
			}
			identities[messageId] = wireOriginal{
				number: wire.pack.SequenceNumber, encoded: ByteCount(len(wire.bytes)), pooled: MessagePoolRootByteCount(wire.bytes),
			}
			packet := wire.pack.Frames[0]
			if !protocolOnlyFrame(packet.MessageType, packet.Raw, packet.MessageBytes) {
				t.Fatal("loss train emitted a payload-bearing or malformed packet")
			}
			if index == 0 {
				tcpState = bytes.Clone(packet.MessageBytes)
				fixture.wire.sender.sendBuffer.mutex.Lock()
				sequence = fixture.wire.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(wire.pack.SequenceId)]
				fixture.wire.sender.sendBuffer.mutex.Unlock()
			} else if !bytes.Equal(tcpState, packet.MessageBytes) {
				t.Fatal("repeated public ACKs did not represent the same TCP state")
			}
			fixture.wire.drop(wire)
		}
		if sequence == nil {
			t.Fatal("public loss train has no retained send owner")
		}
		retryCopies := 0
		dropRetries := func() {
			t.Helper()
			for len(fixture.wire.senderOut) != 0 {
				wire := fixture.wire.take(fixture.wire.senderOut)
				if wire.pack != nil && wire.pack.LogicalLane == 2 {
					messageId := RequireIdFromBytes(wire.pack.MessageId)
					original, known := identities[messageId]
					if !known || wire.pack.Nack || wire.pack.SequenceNumber != original.number ||
						RequireIdFromBytes(wire.pack.SequenceId) != sequence.sequenceId ||
						len(wire.pack.Frames) != 1 || !bytes.Equal(wire.pack.Frames[0].MessageBytes, tcpState) {
						t.Fatal("actual retry invented or collapsed a reliable original")
					}
					original.encoded = ByteCount(len(wire.bytes))
					original.pooled = MessagePoolRootByteCount(wire.bytes)
					identities[messageId] = original
					retryCopies++
				}
				fixture.wire.drop(wire)
			}
		}
		requireOriginals := func() {
			t.Helper()
			sequence.readSendLiveSnapshot()
			synctest.Wait()
			dropRetries()
			// Only the owner reads its mutable retained items. Loading its
			// immutable publication does not request another owner pass.
			snapshot := sequence.liveDiagnostics.latest.Load()
			var encoded, pooled ByteCount
			for _, original := range identities {
				encoded += original.encoded
				pooled += original.pooled
			}
			if len(identities) != originalCount || encoded == 0 || pooled < encoded || snapshot == nil ||
				!snapshot.Sampled || !snapshot.SampledAt.Equal(time.Now()) || snapshot.Closed ||
				snapshot.SequenceId != sequence.sequenceId || snapshot.LogicalLane != 2 ||
				snapshot.OriginalItemCount != originalCount || snapshot.UnacknowledgedItemCount != originalCount ||
				snapshot.OriginalEncodedByteCount != encoded || snapshot.UnacknowledgedEncodedByteCount != encoded ||
				snapshot.OriginalRetainedChargeByteCount != 0 {
				t.Fatalf("retained protocol diagnostics hid actual originals: count=%d encoded=%d pooled=%d snapshot=%+v", len(identities), encoded, pooled, snapshot)
			}
			for index := range results {
				if results[index].calls.Load() != 0 {
					t.Fatal("lost unacknowledged original completed before child cancellation")
				}
			}
			fixture.requireDataPressure(t)
			t.Logf("retained repeated ACK state: originals=%d encoded_bytes=%d observed_serialized_pool_bytes=%d data_charge=%d age=%s", len(identities), encoded, pooled, snapshot.OriginalRetainedChargeByteCount, snapshot.OldestFirstWriteAge)
		}
		requireOriginals()
		for range 3 {
			time.Sleep(fixture.wire.sender.settings.SendBufferSettings.MaxResendInterval)
			synctest.Wait()
			requireOriginals()
		}
		if retryCopies == 0 || fixture.wire.deliveredCount != 2 {
			t.Fatal("loss train did not exercise actual retries without peer delivery")
		}
		sequence.cancel()
		<-sequence.done
		synctest.Wait()
		if !errors.Is(sequence.ctx.Err(), context.Canceled) {
			t.Fatal("retained protocol child did not join its requested cancellation")
		}
		for index := range results {
			result := &results[index]
			if result.calls.Load() != 1 || result.canceled.Load() != 1 || result.success.Load() != 0 || result.other.Load() != 0 {
				t.Fatalf("original %d did not finish exactly once at cancellation", index)
			}
		}
		terminal := sequence.readSendLiveSnapshot()
		if !terminal.Closed || terminal.OriginalItemCount != 0 || terminal.OriginalEncodedByteCount != 0 || terminal.UnacknowledgedItemCount != 0 {
			t.Fatal("canceled protocol child still reports retained originals")
		}
		fixture.requireDataPressure(t)
		fixture.cancelData(t)
		t.Logf("loss train closed: distinct_originals=%d observed_retry_copies=%d", originalCount, retryCopies)
	})
}
