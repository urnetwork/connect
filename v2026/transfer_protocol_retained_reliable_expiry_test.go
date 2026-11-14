// Protocol exemption does not collapse separately assigned reliable originals.
// Their actual retained wire remains visible until the ordinary ACK lifetime.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

func TestProtocolControlNoBudgetReliableCarrierLossExpiresActualOriginals(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newProtocolDataPressureFixture(t, 4, false, func(settings *SendBufferSettings) {
			settings.LiveDiagnostics = true
		})
		// The parent fixture establishes its pressure geometry using a modeled
		// unreliable carrier. Reclassify this same owned H1 route before any
		// new protocol original exists; no per-item lifetime is shortened.
		routes := fixture.wire.sender.RouteManager()
		routes.mutex.Lock()
		var transport Transport
		for candidate := range routes.writerMatchState.transportRoutes {
			if transportTypeOf(candidate) == TransportTypeH1 {
				transport = candidate
				break
			}
		}
		routes.mutex.Unlock()
		if transport == nil {
			t.Fatal("ordinary reliable H1 companion has no owned transport")
		}
		routes.UpdateTransportWithProperties(transport, []Route{fixture.wire.senderOut}, TransferCarrierProperties{})
		synctest.Wait()
		const originalCount = 16
		var results [originalCount]protocolPressureCompletion
		type wireOriginal struct {
			number          uint64
			encoded, pooled ByteCount
		}
		originals := make(map[Id]wireOriginal, originalCount)
		var sequence *SendSequence
		var packet []byte
		for slot := range results {
			accepted, err := fixture.send(t, 2, tcpFlagAck, 0, true, &results[slot], time.Second)
			if !accepted || err != nil {
				t.Fatalf("public expiring protocol source refused original=%d accepted=%t error=%v", slot, accepted, err)
			}
			wire := fixture.take(t, 2, uint64(slot+1))
			id := RequireIdFromBytes(wire.pack.MessageId)
			if _, exists := originals[id]; exists || !protocolOnlyPack(wire.pack) || wire.pack.Nack {
				t.Fatal("public expiring protocol originals lost distinct identity or pure-control classification")
			}
			originals[id] = wireOriginal{number: wire.pack.SequenceNumber, encoded: ByteCount(len(wire.bytes)), pooled: MessagePoolRootByteCount(wire.bytes)}
			if slot == 0 {
				packet = bytes.Clone(wire.pack.Frames[0].MessageBytes)
				fixture.wire.sender.sendBuffer.mutex.Lock()
				sequence = fixture.wire.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(wire.pack.SequenceId)]
				fixture.wire.sender.sendBuffer.mutex.Unlock()
			} else if !bytes.Equal(packet, wire.pack.Frames[0].MessageBytes) {
				t.Fatal("repeated expiring public originals changed their TCP state")
			}
			fixture.wire.drop(wire)
		}
		if sequence == nil {
			t.Fatal("public expiring protocol originals have no source owner")
		}
		retries := 0
		dropRetries := func() {
			for len(fixture.wire.senderOut) != 0 {
				wire := fixture.wire.take(fixture.wire.senderOut)
				if wire.pack != nil && wire.pack.LogicalLane == 2 {
					id := RequireIdFromBytes(wire.pack.MessageId)
					original, known := originals[id]
					if !known || wire.pack.Nack || wire.pack.SequenceNumber != original.number ||
						RequireIdFromBytes(wire.pack.SequenceId) != sequence.sequenceId || len(wire.pack.Frames) != 1 ||
						!bytes.Equal(wire.pack.Frames[0].MessageBytes, packet) || !protocolOnlyPack(wire.pack) {
						t.Fatal("natural expiring protocol retry changed identity or TCP state")
					}
					original.encoded, original.pooled = ByteCount(len(wire.bytes)), MessagePoolRootByteCount(wire.bytes)
					originals[id] = original
					retries++
				}
				fixture.wire.drop(wire)
			}
		}
		sequence.readSendLiveSnapshot()
		synctest.Wait()
		dropRetries()
		sample := sequence.liveDiagnostics.latest.Load()
		var encoded, pooled ByteCount
		for _, original := range originals {
			encoded += original.encoded
			pooled += original.pooled
		}
		if sample == nil || !sample.Sampled || sample.Closed || sample.SequenceId != sequence.sequenceId ||
			sample.OriginalItemCount != originalCount || sample.UnacknowledgedItemCount != originalCount ||
			sample.OriginalEncodedByteCount != encoded || sample.UnacknowledgedEncodedByteCount != encoded ||
			sample.OriginalRetainedChargeByteCount != 0 || encoded == 0 || pooled < encoded {
			t.Fatal("expiring protocol characterization hid retained originals, serialized backing or DATA charge")
		}
		fixture.requireDataPressure(t)
		// The same configured lifetime also applies to the deliberately lost
		// DATA siblings. Cancel only those children now, so their retirement
		// cannot masquerade as or block this protocol child's natural expiry.
		fixture.cancelData(t)
		if sequence.ctx.Err() != nil || fixture.wire.sender.ctx.Err() != nil || fixture.wire.receiver.ctx.Err() != nil {
			t.Fatal("canceling DATA pressure also canceled the independent protocol owner")
		}
		policy := sequence.transferFlightPolicy()
		if policy.limited || policy.flowIsolation || !sample.OldestFirstWriteAgeSampled ||
			sample.OldestFirstWriteAge != 0 || !sample.SampledAt.Equal(time.Now()) {
			t.Fatal("reliable H1 companion did not pin a zero-age reliable carrier before all retained originals")
		}
		lifetime := sequence.sendBufferSettings.AckTimeout
		if lifetime != 60*time.Second || sequence.sendBufferSettings.UnreliableAckTimeout <= lifetime {
			t.Fatal("reliable H1 companion did not retain the distinct unchanged 60s/90s policy settings")
		}
		step := min(sequence.sendBufferSettings.MaxResendInterval, lifetime/4)
		if lifetime <= 0 || step <= 0 {
			t.Fatal("ordinary protocol ACK lifetime or retry interval is unavailable")
		}
		began, deadline := time.Now(), time.Now().Add(lifetime)
		for time.Now().Before(deadline) {
			select {
			case <-sequence.done:
				t.Fatal("protocol owner retired before its unchanged configured ACK lifetime")
			default:
			}
			time.Sleep(min(step, time.Until(deadline)))
			synctest.Wait()
			dropRetries()
		}
		// Wake the actual deadline when its boundary and a retry timer share a
		// timestamp. This advances one virtual nanosecond, not a new policy.
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		dropRetries()
		select {
		case <-sequence.done:
		default:
			t.Fatal("protocol originals outlived their configured ACK lifetime without any peer feedback")
		}
		terminal := sequence.readSendLiveSnapshot()
		if !terminal.Closed || terminal.OriginalItemCount != 0 || terminal.UnacknowledgedItemCount != 0 ||
			terminal.OriginalEncodedByteCount != 0 || terminal.UnacknowledgedEncodedByteCount != 0 ||
			terminal.OriginalRetainedChargeByteCount != 0 || retries == 0 || time.Since(began) < lifetime ||
			fixture.wire.sender.ctx.Err() != nil || fixture.wire.receiver.ctx.Err() != nil {
			t.Fatal("ordinary protocol expiry did not retire all originals while leaving shared clients live")
		}
		for slot := range results {
			if results[slot].calls.Load() != 1 || results[slot].success.Load() != 0 ||
				results[slot].canceled.Load() != 1 || results[slot].other.Load() != 0 {
				t.Fatalf("protocol expiry lost or repeated a terminal original callback: slot=%d", slot)
			}
		}
		for _, snapshot := range fixture.budgets {
			assertRetainedBudgetBalance(t, snapshot.budget)
		}
		t.Logf("PROTOCOL_RELIABLE_H1_RETAINED_EXPIRY_JOINED originals=%d serialized_bytes=%d observed_serialized_pool_bytes=%d data_charge=0 retries=%d elapsed=%s configured_ack_lifetime=%s total_go_allocation_not_measured=true", originalCount, encoded, pooled, retries, time.Since(began), lifetime)
	})
}
