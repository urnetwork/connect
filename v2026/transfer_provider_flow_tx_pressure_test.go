// Negotiated provider-return lanes share exact retained admission. Independent
// controls can progress at the data ceiling; colliding flows preserve order.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The real lane-zero ACK supplies capability; no version map or reply key is
// seeded. Raw provider packets use the same local flow scheduling option.
func providerFlowTxFixture(t *testing.T) (*windowRoundFixture, *TransferMemoryBudget, *IpPath, *IpPath, *IpPath) {
	t.Helper()
	assertMessagePoolOwnership(t)
	tx := NewTransferMemoryBudget(128 * 1024)
	t.Cleanup(func() { assertRetainedBudgetBalance(t, tx) })
	fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueBudget = tx
		settings.ResendQueueRetainedByteAccounting = true
		settings.ResendQueueMinByteCount = 0
		settings.LaneFloorByteCount = 0
		settings.LogicalDataLaneCount = 8
		settings.ResendQueueMaxByteCount = tx.TotalByteCount()
	}, nil, TransportTypeH1)
	path := reliableDeliveryTestPath(4).Reverse()
	var warm protocolPressureCompletion
	wire := providerFlowTxWrite(t, fixture, path, tcpFlagSyn|tcpFlagAck, nil, true, &warm)
	if wire.pack.LogicalLane != 0 {
		t.Fatal("unadvertised provider peer bypassed lane zero")
	}
	fixture.forward(wire, fixture.receiverIn)
	fixture.acknowledge()
	synctest.Wait()
	fixture.sender.sendBuffer.mutex.Lock()
	sequence := fixture.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(wire.pack.SequenceId)]
	fixture.sender.sendBuffer.mutex.Unlock()
	version := uint32(0)
	if sequence != nil {
		if snapshot := fixture.sender.sendBuffer.logicalLaneVersionSnapshot.Load(); snapshot != nil {
			version = (*snapshot)[sequence.id().logicalLaneBase()]
		}
	}
	if warm.success.Load() != 1 || sequence == nil || version != transferLogicalLaneVersion || tx.UsedByteCount() != 0 {
		t.Fatal("actual provider SYNACK delivery/ACK did not publish live lane-zero capability")
	}
	first := *path
	first.DestinationPort = 44000
	wanted := ipSendSchedulingKey(&first).logicalLaneForCount(8)
	var sibling, collision *IpPath
	for port := 44001; port < 45000 && (sibling == nil || collision == nil); port++ {
		candidate := *path
		candidate.DestinationPort = port
		lane := ipSendSchedulingKey(&candidate).logicalLaneForCount(8)
		if lane != wanted && sibling == nil {
			copy := candidate
			sibling = &copy
		}
		if lane == wanted && collision == nil {
			copy := candidate
			collision = &copy
		}
	}
	if sibling == nil || collision == nil || wanted == 0 {
		t.Fatal("synthetic provider tuples did not exercise distinct and colliding lane hashes")
	}
	// Establish actual workers before zero-wait pressure assertions; otherwise
	// cold readiness would be a second, unrelated refusal boundary.
	for _, selected := range []*IpPath{&first, sibling} {
		var completed protocolPressureCompletion
		wire := providerFlowTxWrite(t, fixture, selected, tcpFlagAck, nil, true, &completed)
		if wire.pack.LogicalLane != ipSendSchedulingKey(selected).logicalLaneForCount(8) {
			t.Fatal("real negotiated provider offer did not select its actual flow hash")
		}
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		if completed.success.Load() != 1 || tx.UsedByteCount() != 0 {
			t.Fatal("actual provider sibling did not complete its reliable warm delivery")
		}
	}
	return fixture, tx, &first, sibling, collision
}

// Takes the source root only on the public API's complete success predicate.
func providerFlowTxOffer(t *testing.T, fixture *windowRoundFixture, path *IpPath, flags byte, payload []byte, noBudget bool, completed *protocolPressureCompletion, timeout time.Duration) (bool, error, []byte) {
	t.Helper()
	expected := ipOosTcpPacketSequence(path, flags, 7000, payload)
	packet := MessagePoolCopy(expected)
	frame, err := ipPacketFromProviderFrame(packet, 2)
	if err != nil || frame == nil || !frame.Raw || protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) != noBudget {
		MessagePoolReturn(packet)
		t.Fatal("provider source did not preserve strict data/control classification")
	}
	opts := []any{scheduleIpFlow(path)}
	if noBudget {
		opts = append(opts, NoBudget())
	}
	accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), completed.record, timeout, opts...)
	if !accepted || err != nil {
		MessagePoolReturn(packet)
	}
	return accepted, err, expected
}

// One awaited public write makes each original and actual wire identity exact.
func providerFlowTxWrite(t *testing.T, fixture *windowRoundFixture, path *IpPath, flags byte, payload []byte, noBudget bool, completed *protocolPressureCompletion) *windowRoundFrame {
	t.Helper()
	accepted, err, expected := providerFlowTxOffer(t, fixture, path, flags, payload, noBudget, completed, time.Second)
	if !accepted || err != nil {
		t.Fatalf("provider public offer admitted=%t error=%v", accepted, err)
	}
	wire := fixture.take(fixture.senderOut)
	if wire.pack == nil || wire.pack.Nack || len(wire.pack.Frames) != 1 ||
		wire.pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider ||
		!bytes.Equal(wire.pack.Frames[0].MessageBytes, expected) || protocolOnlyPack(wire.pack) != noBudget {
		t.Fatal("actual provider wire changed original bytes, reliable receipt or control class")
	}
	return wire
}

// A full shared parentless TX ceiling grants no free per-lane data floor.
// Reliable protocol originals still deliver and ACK without evicting data.
func TestProviderFlowLanesKeepProtocolIndependentOfFullExactTx(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture, tx, first, sibling, _ := providerFlowTxFixture(t)
		var resident protocolPressureCompletion
		data := providerFlowTxWrite(t, fixture, first, tcpFlagAck, bytes.Repeat([]byte{0x71}, 1200), false, &resident)
		// Derive the controlled ceiling from one actual retained original,
		// rather than filling the budget with anonymous reservations.
		tx.SetTotalByteCount(tx.UsedByteCount())
		before := tx.Stats()
		if before.UsedByteCount <= 0 || before.UsedByteCount != before.TotalByteCount || tx.floorByteCount.Load() != 0 {
			t.Fatal("actual provider data did not saturate an exact zero-floor TX ceiling")
		}
		fixture.sender.sendBuffer.mutex.Lock()
		sequence := fixture.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(data.pack.SequenceId)]
		fixture.sender.sendBuffer.mutex.Unlock()
		if sequence == nil {
			t.Fatal("actual retained provider DATA lost its live send generation")
		}
		owner := sequence.resendQueue.GetByMessageId(RequireIdFromBytes(data.pack.MessageId))
		if owner == nil || owner.noBudget || owner.memoryBudget != tx || owner.QueueByteCount() != before.UsedByteCount || !sequence.resendQueue.lifetimeBudget {
			t.Fatal("actual data original was not the exact lifetime owner of the shared TX ceiling")
		}
		// Republish the unchanged route so every live lane observes the test's
		// reduced ceiling before the zero-wait DATA probe.
		routes := fixture.sender.RouteManager()
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
			t.Fatal("provider H1 route disappeared during live custody")
		}
		routes.UpdateTransport(transport, []Route{fixture.senderOut})
		synctest.Wait()
		var refused protocolPressureCompletion
		if accepted, err, _ := providerFlowTxOffer(t, fixture, sibling, tcpFlagAck, []byte{0x72}, false, &refused, 0); accepted || err != nil {
			t.Fatalf("zero-floor DATA escaped a full exact TX ceiling: admitted=%t error=%v", accepted, err)
		}
		for _, flags := range []byte{tcpFlagSyn | tcpFlagAck, tcpFlagAck} {
			var control protocolPressureCompletion
			wire := providerFlowTxWrite(t, fixture, sibling, flags, nil, true, &control)
			if wire.pack.LogicalLane == data.pack.LogicalLane || bytes.Equal(wire.pack.SequenceId, data.pack.SequenceId) {
				t.Fatal("independent provider control failed actual negotiated flow isolation")
			}
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			synctest.Wait()
			after := tx.Stats()
			if control.success.Load() != 1 || control.calls.Load() != 1 || resident.calls.Load() != 0 || refused.calls.Load() != 0 ||
				after.UsedByteCount != before.UsedByteCount || after.TotalByteCount != before.TotalByteCount ||
				after.ReservedByteCount != before.ReservedByteCount || after.ReleasedByteCount != before.ReleasedByteCount ||
				sequence.resendQueue.GetByMessageId(owner.messageId) != owner || tx.floorByteCount.Load() != 0 {
				t.Fatal("control delivery/ACK changed unrelated full DATA custody, capacity or lifetime")
			}
		}
		fixture.forward(data, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		if resident.success.Load() != 1 || resident.calls.Load() != 1 || tx.UsedByteCount() != 0 {
			t.Fatal("real DATA delivery/ACK did not release its original retained owner")
		}
		var resumed protocolPressureCompletion
		wire := providerFlowTxWrite(t, fixture, sibling, tcpFlagAck, []byte{0x73}, false, &resumed)
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		if resumed.success.Load() != 1 || resumed.calls.Load() != 1 || tx.UsedByteCount() != 0 || tx.TotalByteCount() != before.TotalByteCount {
			t.Fatal("retiring the real owner did not restore charged sibling DATA admission under the same ceiling")
		}
	})
}

// Hash collisions preserve ordinary reliable ordering. Capability is not a
// promise that every distinct TCP flow gets a separate Transfer generation.
func TestProviderFlowLaneCollisionKeepsMissingDataOrderAndReceipts(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture, tx, first, _, collision := providerFlowTxFixture(t)
		var dataResult, controlResult protocolPressureCompletion
		data := providerFlowTxWrite(t, fixture, first, tcpFlagAck, []byte{0x61}, false, &dataResult)
		control := providerFlowTxWrite(t, fixture, collision, tcpFlagSyn|tcpFlagAck, nil, true, &controlResult)
		if first.DestinationPort == collision.DestinationPort || data.pack.LogicalLane == 0 || data.pack.LogicalLane != control.pack.LogicalLane ||
			!bytes.Equal(data.pack.SequenceId, control.pack.SequenceId) || control.pack.SequenceNumber != data.pack.SequenceNumber+1 {
			t.Fatal("actual distinct colliding TCP tuples did not preserve one ordered generation")
		}
		delivered := fixture.deliveredCount
		fixture.forward(control, fixture.receiverIn)
		for _, ack := range fixture.takeAcknowledgements() {
			if !ack.ack.Selective || !bytes.Equal(ack.ack.MessageId, control.pack.MessageId) || !bytes.Equal(ack.ack.SequenceId, control.pack.SequenceId) || len(ack.ack.EvictedSequenceNumbers) != 0 {
				t.Fatal("future colliding protocol emitted false cumulative, DATA or unrelated receipt")
			}
			fixture.forward(ack, fixture.senderIn)
		}
		synctest.Wait()
		if fixture.deliveredCount != delivered || dataResult.calls.Load() != 0 || controlResult.calls.Load() != 0 || tx.UsedByteCount() == 0 {
			t.Fatal("colliding future protocol bypassed missing DATA delivery or reliable ownership")
		}
		started := time.Now()
		fixture.forward(data, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		if time.Since(started) != 0 || fixture.deliveredCount != delivered+2 || dataResult.success.Load() != 1 || controlResult.success.Load() != 1 ||
			dataResult.calls.Load() != 1 || controlResult.calls.Load() != 1 || tx.UsedByteCount() != 0 {
			t.Fatal("real missing predecessor did not drain both retained originals without retry or duplicate receipt")
		}
	})
}
