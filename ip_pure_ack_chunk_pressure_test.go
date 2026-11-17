// Encrypted provider custody measures larger feedback owners under actual DATA pressure.
package connect

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Costs copy scalar capacities only; no packet, frame, owner or claim outlives
// its existing callback or custody barrier because of this observation.
type pureAckDecodedCost struct {
	frames, pointerCapacity                                      int
	carrierViewCapacitySum, frameCapacitySum, uniqueBackingBytes ByteCount
	metadataEnvelope                                             ByteCount
	legacyGraph                                                  bool
}

// Merge actual overlapping backing intervals in fixed storage, avoiding double
// counting a carrier root if a decoder lends frame slices from that same root.
func pureAckReceivedItemCost(t *testing.T, item *receiveItem) pureAckDecodedCost {
	t.Helper()
	if item == nil || !item.ack || !item.noBudget || len(item.frames) > sendPackH1GroupMaxFrames {
		t.Fatal("cost cut has no actual bounded protocol item")
	}
	owner := item.decodedOwner
	cost := pureAckDecodedCost{frames: len(item.frames), pointerCapacity: cap(item.frames)}
	if owner != nil {
		if !owner.inUse || owner.receivePack.Pack == nil {
			t.Fatal("cost cut lost its embedded decoded owner")
		}
		cost.metadataEnvelope = owner.metadataQueueByteCount()
		cost.legacyGraph = owner.receivePack.Pack != &owner.pack
	} else {
		// Legacy compatibility retains separately allocated item/Pack/frame
		// descriptors. This is a labelled component estimate, not a resident
		// heap peak: closure, allocator and transient decoder costs are separate.
		cost.legacyGraph = true
		cost.metadataEnvelope = ByteCount(unsafe.Sizeof(receiveItem{})) + ByteCount(unsafe.Sizeof(ReceivePack{})) + ByteCount(unsafe.Sizeof(protocol.Pack{})) + ByteCount(unsafe.Sizeof(protocol.Tag{})) + ByteCount(len(item.frames))*(ByteCount(unsafe.Sizeof(protocol.Frame{}))+16) + ByteCount(cap(item.frames))*ByteCount(unsafe.Sizeof((*protocol.Frame)(nil))) + 4*32
	}
	type span struct{ first, last uintptr }
	var spans [sendPackH1GroupMaxFrames + 3]span
	count := 0
	add := func(buffer []byte) {
		if cap(buffer) != 0 {
			first := uintptr(unsafe.Pointer(unsafe.SliceData(buffer)))
			spans[count] = span{first: first, last: first + uintptr(cap(buffer))}
			count++
		}
	}
	cost.carrierViewCapacitySum = ByteCount(cap(item.transferFrameBytes))
	add(item.transferFrameBytes)
	if owner != nil {
		cost.carrierViewCapacitySum += ByteCount(cap(owner.receivePack.TransferFrameBytes))
		add(owner.receivePack.TransferFrameBytes)
	}
	for _, frame := range item.frames {
		if frame != nil {
			cost.frameCapacitySum += ByteCount(cap(frame.MessageBytes))
			add(frame.MessageBytes)
		}
	}
	if item.contractFrame != nil {
		add(item.contractFrame.MessageBytes)
	}
	for index := 1; index < count; index++ {
		value := spans[index]
		position := index
		for position > 0 && value.first < spans[position-1].first {
			spans[position] = spans[position-1]
			position--
		}
		spans[position] = value
	}
	if count != 0 {
		first, last := spans[0].first, spans[0].last
		for index := 1; index < count; index++ {
			if spans[index].first <= last {
				last = max(last, spans[index].last)
			} else {
				cost.uniqueBackingBytes += ByteCount(last - first)
				first, last = spans[index].first, spans[index].last
			}
		}
		cost.uniqueBackingBytes += ByteCount(last - first)
	}
	runtime.KeepAlive(item)
	return cost
}

// The negotiated encrypted session stays intact while only its registered
// writer carrier changes. No source/receiver policy, queue or budget is raised.
func pureAckEncryptedCarrier(t *testing.T, f *privateProviderEncryptedAckFixture, kind TransportType) {
	t.Helper()
	synctest.Wait()
	manager := f.sender.RouteManager()
	var original Transport
	manager.mutex.Lock()
	for transport, routes := range manager.writerMatchState.transportRoutes {
		if len(routes) == 1 && routes[0] == f.senderOut {
			if original != nil {
				t.Error("ambiguous encrypted fixture writer")
			}
			original = transport
		}
	}
	manager.mutex.Unlock()
	if original == nil {
		t.Fatal("encrypted fixture lost its actual H1 writer")
	}
	// This source is live, so keep a writer available while replacing it.
	// Distinct Route identity prevents retiring the old key from disabling
	// the new registration. Teardown owns both channels explicitly.
	oldRoute := f.senderOut
	replacementRoute := make(Route, cap(oldRoute))
	manager.UpdateTransport(newPrivateH1PeerGateway(f.receiver.ClientId(), kind), []Route{replacementRoute})
	manager.RemoveTransport(original)
	f.senderOut = replacementRoute
	t.Cleanup(func() {
		for len(oldRoute) != 0 {
			MessagePoolReturn(<-oldRoute)
		}
	})
	synctest.Wait()
	source := privateProviderImmediateSource(t, f.windowRoundFixture, f.sequence.sequenceId.Bytes())
	active := source.contractMultiRouteWriter.GetActiveRoutes()
	policy := source.transferFlightPolicy()
	if len(active) != 1 || active[0] != replacementRoute || policy.h1Only != (kind == TransportTypeH1) {
		t.Fatal("encrypted carrier replacement did not publish its exact active writer")
	}
}

// Signed/encrypted feedback retains its actual decoded roots and per-frame
// claims with full RX callback ownership and an independent full TX DATA
// permission reservation; every native ACK still arrives once.
func TestPureTcpAckDenseChunkPreservesEncryptedPressureCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, row := range []struct {
			name    string
			batch   int
			carrier TransportType
		}{
			{name: "legacy-shared-shape", batch: 2, carrier: TransportTypeP2p},
			{name: "compact-feedback", batch: 16, carrier: TransportTypeP2p},
			{name: "existing-unknown-H1-shape", batch: 16, carrier: TransportTypeH1},
		} {
			batch := row.batch
			tx := NewTransferMemoryBudget(2 * 1024 * 1024)
			f := newPrivateProviderEncryptedAckFixture(t, func(settings *ClientSettings) {
				settings.SendBufferSettings.ResendQueueBudget = tx
				settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
				settings.SendBufferSettings.ChunkSettings.PureTcpAckMaxFrames = batch
			})
			defer f.close()
			pureAckEncryptedCarrier(t, f, row.carrier)
			pressure := newPrivateProviderProtocolDataPressure(t, f.rx)
			defer pressure.close()
			// This explicit DATA permission owner fills the enabled TX budget;
			// it is independent from the real full RX callback owner above.
			heldTx := tx.Available()
			if heldTx <= 0 || !tx.TryReserve(heldTx) {
				t.Fatal("TX DATA permission owner could not fill its existing budget")
			}
			var releaseTxOnce sync.Once
			releaseTx := func() { releaseTxOnce.Do(func() { tx.Release(heldTx) }) }
			defer releaseTx()
			probe := budgetTestFrame(64)
			probeType, probeRaw := probe.MessageType, probe.Raw
			probeBytes := bytes.Clone(probe.MessageBytes)
			var probeCallbacks, probeAcks atomic.Int64
			probeAck := func(err error) {
				probeCallbacks.Add(1)
				if err == nil {
					probeAcks.Add(1)
				}
			}
			admitted, probeErr := f.sender.SendWithTimeoutDetailed(probe, f.receiver.ClientId(), probeAck, 0)
			if !admitted {
				MessagePoolReturn(probe.MessageBytes)
			}
			// Admission transfers a bounded raw owner; exact encoded memory is
			// reserved by Run. A published open gate may legitimately queue it.
			synctest.Wait()
			source := privateProviderImmediateSource(t, f.windowRoundFixture, f.sequence.sequenceId.Bytes())
			if tx.Available() != 0 || len(f.senderOut) != 0 || source.resendQueue.Len() != 0 || !source.resendCapacityUnavailable.Load() || probeAcks.Load() != 0 || admitted && probeCallbacks.Load() != 0 {
				t.Fatalf("full TX permission did not retain unwritten DATA: admitted=%t err=%v callbacks=%d acks=%d", admitted, probeErr, probeCallbacks.Load(), probeAcks.Load())
			}
			txBefore, rxBefore, recoveryBefore := tx.Stats(), f.rx.Stats(), f.recovery.Stats()
			before := time.Now()
			var sourceRoots, sourceCapacity, carrierCapacity, decodedBacking, decodedMetadata ByteCount
			var maxDecoded, claimsEnvelope ByteCount
			var nativeCount int
			var nativeCapacity ByteCount
			previous := f.nat.beforeSendPacketForTest
			f.nat.beforeSendPacketForTest = func(packet *SendPacket) {
				if packet.reliable != nil {
					nativeCount += len(packet.packets)
					for _, raw := range packet.packets {
						nativeCapacity += ByteCount(cap(raw))
					}
				}
				previous(packet)
			}
			for round := 0; round < 64/batch; round++ {
				f.gate = make(chan struct{})
				offer := f.enqueueAcks(batch, 500)
				wire := f.takeAckOriginal(offer)
				if !wire.encrypted || len(wire.pack.Frames) != batch {
					t.Fatalf("actual encrypted partition=%d want=%d", len(wire.pack.Frames), batch)
				}
				source := privateProviderImmediateSource(t, f.windowRoundFixture, wire.pack.SequenceId)
				item := source.resendQueue.GetBySequenceNumber(wire.pack.SequenceNumber)
				if item == nil || item.acks.count != 1 || item.acks.overflow != nil || !item.noBudget {
					t.Fatal("dense same-group chunk added unrelated original records")
				}
				sourceRoots += MessagePoolRootByteCount(item.transferFrameBytes)
				sourceCapacity += ByteCount(cap(item.transferFrameBytes))
				carrierCapacity += ByteCount(cap(wire.bytes))
				f.forward(wire.windowRoundFrame, f.receiverIn)
				privateProviderAssertProtocolReceiptCustody(t, f.sequence, 1)
				pressure.check(t)
				q := f.sequence.immediateDeliveryQueue
				q.mutex.Lock()
				if len(q.items) != 1 || q.items[0].pending != batch {
					q.mutex.Unlock()
					t.Fatal("dense encrypted frame claims did not keep one exact physical receipt")
				}
				receipt := q.items[0]
				receivedItem := receipt.item
				claimBytes := ByteCount(unsafe.Sizeof(receiveDeliveryReceipt{})) + ByteCount(cap(q.items))*ByteCount(unsafe.Sizeof((*receiveDeliveryReceipt)(nil))) + ByteCount(cap(q.operations))*ByteCount(unsafe.Sizeof((*receiveDeliveryOperation)(nil))) + ByteCount(batch)*(ByteCount(unsafe.Sizeof(receiveDeliveryClaim{}))+ByteCount(unsafe.Sizeof(receiveDeliveryOperation{}))+ByteCount(unsafe.Sizeof(providerReliablePacket{}))+ByteCount(unsafe.Sizeof(TcpSendItem{})))
				q.mutex.Unlock()
				cost := pureAckReceivedItemCost(t, receivedItem)
				if cost.frames != batch || cost.pointerCapacity < batch || cost.metadataEnvelope < decodedPackOwnerQueueByteCount || cost.uniqueBackingBytes <= 0 || offer.callbacks != 0 || source.immediateSendCount != 1 {
					t.Fatal("encrypted cost cut lost actual root/descriptor/custody ownership")
				}
				decodedBacking += cost.uniqueBackingBytes
				decodedMetadata += cost.metadataEnvelope
				maxDecoded = max(maxDecoded, cost.uniqueBackingBytes+cost.metadataEnvelope)
				claimsEnvelope = max(claimsEnvelope, claimBytes)
				close(f.gate)
				synctest.Wait()
				f.feedback()
				privateProviderAssertOriginalRetired(t, f.windowRoundFixture, wire.pack)
				privateProviderAssertImmediateCount(t, f.windowRoundFixture, wire.pack, 0)
				if offer.callbacks != 1 || f.ackCounts[wire.pack.SequenceNumber] != 1 || tx.Stats() != txBefore || f.rx.Stats() != rxBefore || f.recovery.Stats() != recoveryBefore || !time.Now().Equal(before) {
					t.Fatal("encrypted receipt closure spent DATA budgets, clock, or callback ownership")
				}
			}
			if nativeCount != 64 || f.deliveredCount != 65 || f.ackedCount != 1+64/batch || tx.Available() != 0 || probeAcks.Load() != 0 || admitted && probeCallbacks.Load() != 0 {
				t.Fatalf("encrypted native events changed: native=%d delivered=%d callbacks=%d", nativeCount, f.deliveredCount, f.ackedCount)
			}
			t.Logf("feedback-cost-equal-work case=%s headers=64 batch=%d physical=%d source_root_charges=%d source_actual_capacity=%d encrypted_carrier_capacity=%d decoded_unique_backing=%d decoded_metadata_envelopes=%d max_live_decoded=%d max_claim_type_and_array_bytes=%d native_packet_capacities=%d send_item_size=%d settings_size=%d scaled_32_owner_component_estimate=%d", row.name, batch, 64/batch, sourceRoots, sourceCapacity, carrierCapacity, decodedBacking, decodedMetadata, maxDecoded, claimsEnvelope, nativeCapacity, unsafe.Sizeof(sendItem{}), unsafe.Sizeof(SendBufferChunkSettings{}), 32*(maxDecoded+claimsEnvelope))
			pressure.close()
			releaseTx()
			synctest.Wait()
			callbacksBeforeDelivery := probeCallbacks.Load()
			if !admitted {
				// A synchronous refusal retained caller ownership. Only now may
				// a fresh identical owner retry against released permission.
				retry := budgetTestFrame(64)
				accepted, err := f.sender.SendWithTimeoutDetailed(retry, f.receiver.ClientId(), probeAck, time.Second)
				if !accepted {
					MessagePoolReturn(retry.MessageBytes)
				}
				if !accepted || err != nil {
					t.Fatalf("DATA retry after TX release failed: %t %v", accepted, err)
				}
			}
			synctest.Wait()
			select {
			case encoded := <-f.senderOut:
				wire := f.capture(encoded, f.receiver)
				if wire.pack == nil || wire.pack.Nack || wire.pack.DeliverImmediately || len(wire.pack.Frames) != 1 || wire.pack.Frames[0].MessageType != probeType || wire.pack.Frames[0].Raw != probeRaw || !bytes.Equal(wire.pack.Frames[0].MessageBytes, probeBytes) {
					t.Fatal("TX release did not write the exact queued DATA owner")
				}
				owner := source.resendQueue.GetBySequenceNumber(wire.pack.SequenceNumber)
				if owner == nil || owner.memoryBudget != tx || owner.QueueByteCount() <= 0 {
					t.Fatal("written DATA did not acquire its actual TX memory owner")
				}
				f.forward(wire.windowRoundFrame, f.receiverIn)
				f.feedback()
			default:
				t.Fatal("released TX permission stranded the accepted DATA owner")
			}
			if probeAcks.Load() != 1 || probeCallbacks.Load() != callbacksBeforeDelivery+1 || f.deliveredCount != 66 || !time.Now().Equal(before) {
				t.Fatal("DATA recovery lost its exact delivery, callback or clock")
			}
			f.close()
			if tx.UsedByteCount() != 0 || f.root.UsedByteCount() != 0 || f.parent.UsedByteCount() != 0 {
				t.Fatal("joined encrypted pressure retained a memory owner")
			}
		}
	})
}

// Cancellation releases the actual encrypted decoded graph and all provider
// claims, without turning an unaccepted native delivery into a successful ACK.
func TestPureTcpAckDenseChunkCancelReturnsPressuredOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		tx := NewTransferMemoryBudget(2 * 1024 * 1024)
		f := newPrivateProviderEncryptedAckFixture(t, func(settings *ClientSettings) {
			settings.SendBufferSettings.ResendQueueBudget = tx
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		})
		defer f.close()
		pureAckEncryptedCarrier(t, f, TransportTypeP2p)
		pressure := newPrivateProviderProtocolDataPressure(t, f.rx)
		defer pressure.close()
		heldTx := tx.Available()
		if heldTx <= 0 || !tx.TryReserve(heldTx) {
			t.Fatal("cancel control could not hold independent TX DATA permission")
		}
		var releaseTxOnce sync.Once
		releaseTx := func() { releaseTxOnce.Do(func() { tx.Release(heldTx) }) }
		defer releaseTx()
		before := time.Now()
		f.gate = make(chan struct{})
		offer := f.enqueueAcks(16, 500)
		wire := f.takeAckOriginal(offer)
		if len(wire.pack.Frames) != 16 {
			t.Fatal("cancel control did not hold the dense decoded shape")
		}
		f.forward(wire.windowRoundFrame, f.receiverIn)
		privateProviderAssertProtocolReceiptCustody(t, f.sequence, 1)
		pressure.check(t)
		pressure.close()
		releaseTx()
		f.close()
		synctest.Wait()
		if offer.callbacks != 0 || tx.UsedByteCount() != 0 || f.root.UsedByteCount() != 0 || f.parent.UsedByteCount() != 0 || !time.Now().Equal(before) {
			t.Fatal("cancel became delivery or left charged ownership")
		}
		for _, client := range []*Client{f.sender, f.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// The new provenance bit occupies the old pre-int padding on both word sizes;
// no per-sequence, socket, timer, queue, descriptor limit or worker is added.
func TestPureTcpAckChunkProvenanceUsesExistingPackPadding(t *testing.T) {
	if unsafe.Offsetof(SendPack{}.groupPureTcpAckPolicy)+unsafe.Sizeof(bool(false)) > unsafe.Offsetof(SendPack{}.groupChunkMaxFrames) || unsafe.Offsetof(SendPack{}.groupChunkMaxFrames)-unsafe.Offsetof(SendPack{}.serialized) > unsafe.Sizeof(int(0)) {
		t.Fatal("feedback provenance expanded the existing pre-int layout")
	}
	t.Logf("feedback-layout send_pack=%d send_sequence=%d settings=%d int=%d", unsafe.Sizeof(SendPack{}), unsafe.Sizeof(SendSequence{}), unsafe.Sizeof(SendBufferChunkSettings{}), unsafe.Sizeof(int(0)))
}
