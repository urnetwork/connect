// Actual provider return offers carry a signed SYNACK around another flow's
// missing DATA. The desired early behavior is a new authorized sender policy.
package connect

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Scalar lifecycle copies identify every real producer original without
// borrowing worker-owned frames or treating admission as a terminal ACK.
type synAckProviderLifecycle struct {
	stateLock sync.Mutex
	events    []SendPackLifecycleObservation
}

// All three offered originals are serialized separately; this bounded trace
// covers their actual start/write/terminal callbacks, including close errors.
func (self *synAckProviderLifecycle) observe(event SendPackLifecycleObservation) {
	if event.MessageType != protocol.MessageType_IpIpPacketFromProvider {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

// Returns the stable token of the latest started original at a quiescent cut.
func (self *synAckProviderLifecycle) latest(t *testing.T, starts int) uint64 {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var token uint64
	count := 0
	for _, event := range self.events {
		if event.Phase == SendPackLifecyclePhaseStarted {
			count++
			token = event.Token
			if !event.AckRequired {
				t.Fatal("provider TCP original lost its reliable lifecycle")
			}
		}
	}
	if count != starts || token == 0 {
		t.Fatalf("provider original census: starts=%d want=%d", count, starts)
	}
	return token
}

// Counts only actual terminal receipts for this exact producer original.
func (self *synAckProviderLifecycle) terminal(t *testing.T, token uint64) int {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, event := range self.events {
		if event.Token == token && event.Phase == SendPackLifecyclePhaseTerminal {
			count++
			if event.Err != nil {
				t.Fatalf("provider original terminated without peer receipt: %v", event.Err)
			}
		}
	}
	return count
}

// Signed contract openings traverse the real receiver; only the requested
// application original is withheld. No ordinal, floor or ACK is manufactured.
func synAckProviderApplicationWire(t *testing.T, fixture *windowRoundFixture, packets ...[]byte) *windowRoundFrame {
	t.Helper()
	for range cap(fixture.senderOut) {
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil || wire.pack.Nack {
			t.Fatal("provider did not produce a reliable Pack")
		}
		if len(wire.pack.Frames) == 0 {
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			continue
		}
		if len(wire.pack.Frames) != len(packets) {
			t.Fatal("one provider original changed frame cardinality")
		}
		for index, frame := range wire.pack.Frames {
			actual, err := ipPacketFromProviderBytes(frame)
			if err != nil || !bytes.Equal(actual, packets[index]) {
				t.Fatalf("provider wire changed actual packet %d: %v", index, err)
			}
		}
		return wire
	}
	t.Fatal("provider application original hidden behind bounded opening traffic")
	return nil
}

// An old-schema decoder discards only fields it does not understand. Feeding
// its known Pack to the real receiver models ordered legacy semantics, without
// changing sender ownership or forging any returned ACK.
func synAckLegacyReceiverWire(t *testing.T, wire []byte) []byte {
	t.Helper()
	var outer protocol.TransferFrame
	if err := proto.Unmarshal(wire, &outer); err != nil || outer.Pack == nil {
		t.Fatalf("legacy interpretation requires the actual v2 Pack: %v", err)
	}
	body, err := proto.Marshal(outer.Pack)
	if err != nil {
		t.Fatal(err)
	}
	old := dynamicpb.NewMessage(immediateWireOldMessage(t, "Pack"))
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, old); err != nil {
		t.Fatal(err)
	}
	known, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	expected := proto.Clone(outer.Pack).(*protocol.Pack)
	expected.DeliverImmediately = false
	outer.Pack = &protocol.Pack{}
	if err := proto.Unmarshal(known, outer.Pack); err != nil || !proto.Equal(outer.Pack, expected) {
		t.Fatalf("old-schema interpretation changed reliable identity or signed contract: %v", err)
	}
	encoded, err := proto.Marshal(&outer)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Forward only actual receiver ACKs. Before repair, no ordinary cumulative
// receipt may cover the missing DATA or the later SYNACK.
func synAckForwardReceipts(t *testing.T, fixture *windowRoundFixture, missing, control *windowRoundFrame, beforeRepair bool) int {
	t.Helper()
	synctest.Wait()
	terminal := 0
	for len(fixture.receiverOut) != 0 {
		wire := fixture.take(fixture.receiverOut)
		ack := wire.ack
		if ack == nil || len(ack.EvictedSequenceNumbers) != 0 {
			t.Fatal("provider proof received a non-ACK or withdrew held custody")
		}
		if beforeRepair && !ack.Selective && (bytes.Equal(ack.MessageId, missing.pack.MessageId) || bytes.Equal(ack.MessageId, control.pack.MessageId)) {
			t.Fatal("receiver falsely acknowledged an undelivered cumulative prefix")
		}
		if ack.DeliveredImmediately {
			if !ack.Selective || !bytes.Equal(ack.MessageId, control.pack.MessageId) || !bytes.Equal(ack.SequenceId, control.pack.SequenceId) ||
				ack.ImmediateSequenceNumber == nil || *ack.ImmediateSequenceNumber != control.pack.SequenceNumber {
				t.Fatal("early terminal ACK did not bind the actual SYNACK identity and ordinal")
			}
			terminal++
		}
		fixture.forward(wire, fixture.senderIn)
	}
	return terminal
}

// The callback boundary is the public provider return API, with real signed
// Clients and unchanged reliable timers. It does not model a native TCP stack.
func testProviderSynAckImmediate(t *testing.T, batch, legacy bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(kib(512))
		tx := NewTransferMemoryBudgetWithParent(kib(128), root)
		rx := NewTransferMemoryBudgetWithParent(kib(128), root)
		head := NewTransferMemoryBudgetWithParent(kib(128), root)
		pack := NewTransferMemoryBudgetWithParent(kib(128), root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, rx, head, pack, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("provider SYNACK retained a quota after all owners joined")
				}
			}
		})
		lifecycle := &synAckProviderLifecycle{}
		fixture, _, authority, _ := newConsumerPaidPackByteFixture(t,
			consumerPaidPackByteLayout{label: "signed-provider-synack", byteLimit: kib(128), leafBytes: kib(128), rootBytes: kib(512), countLimit: 32},
			rx, head, pack, nil, func(receiver bool, settings *ClientSettings) {
				if !receiver {
					settings.SendBufferSettings.SequenceBufferSize = 32
					if legacy {
						settings.SendBufferSettings.SequenceBufferSize = 1
					}
					settings.SendBufferSettings.ResendQueueBudget = tx
					settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
					settings.SendBufferSettings.ResendQueueMinByteCount = 0
					settings.SendBufferSettings.SendPackLifecycleObserver = lifecycle.observe
				}
			})
		nat := NewLocalUserNatWithDefaults(fixture.ctx, "synack-test")
		settings := DefaultRemoteUserNatProviderSettings()
		settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		settings.ProtocolVersion = 2
		provider := NewRemoteUserNatProvider(fixture.sender, nat, settings)
		if provider == nil || nat == nil {
			t.Fatal("actual provider construction refused")
		}
		t.Cleanup(func() {
			provider.Close()
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := nat.CloseAndWait(closeCtx); err != nil {
				t.Error(err)
			}
		})
		path := reliableDeliveryTestPath(4)
		controlPath := *path
		controlPath.SourcePort++ // a distinct TCP flow shares this actual source
		warmPacket := ipOosTcpPacketSequence(path, tcpFlagAck, 1000, []byte("warm"))
		missingPacket := ipOosTcpPacketSequence(path, tcpFlagAck, 1004, bytes.Repeat([]byte{0x58}, 1240))
		controlPacket := ipOosTcpPacketSequence(&controlPath, tcpFlagSyn|tcpFlagAck, 7000, nil)
		ackPacket := ipOosTcpPacketSequence(&controlPath, tcpFlagAck, 7001, nil)
		var synAckVisits atomic.Int32
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				actual, err := ipPacketFromProviderBytes(frame)
				if err == nil && bytes.Equal(actual, controlPacket) {
					synAckVisits.Add(1)
				}
			}
		})
		offer := func(packetPath *IpPath, packet []byte) *windowRoundFrame {
			if batch && bytes.Equal(packet, controlPacket) {
				provider.ReceiveBatch(SourceId(fixture.receiver.ClientId()), protocol.ProvideMode_Network, packetPath, [][]byte{packet, ackPacket})
				return synAckProviderApplicationWire(t, fixture, packet, ackPacket)
			} else {
				provider.Receive(SourceId(fixture.receiver.ClientId()), protocol.ProvideMode_Network, packetPath, packet)
			}
			return synAckProviderApplicationWire(t, fixture, packet)
		}
		warm := offer(path, warmPacket)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		warmToken := lifecycle.latest(t, 1)
		if lifecycle.terminal(t, warmToken) != 1 || fixture.deliveredCount != 1 || rx.UsedByteCount() != 0 {
			t.Fatal("signed warm original did not finish through real custody and ACK")
		}
		authority.stateLock.Lock()
		grants := len(authority.grantDeadlines)
		authority.stateLock.Unlock()
		if grants == 0 {
			t.Fatal("provider did not obtain an actual signed contract")
		}
		missing := offer(path, missingPacket)
		missingToken := lifecycle.latest(t, 2)
		sequence := fixture.sequence()
		receiver := fixture.receiveSequence()
		base := missing.pack.SequenceNumber
		beforeTx := tx.UsedByteCount()
		if beforeTx <= 0 || receiver.nextSequenceNumber != base || lifecycle.terminal(t, missingToken) != 0 {
			t.Fatal("missing DATA is not a real outstanding source original at the receiver floor")
		}
		started := time.Now()
		control := offer(&controlPath, controlPacket)
		controlToken := lifecycle.latest(t, 3)
		if control.pack.SequenceNumber != base+1 || control.pack.LogicalLane != missing.pack.LogicalLane ||
			!bytes.Equal(control.pack.SequenceId, missing.pack.SequenceId) || control.pack.Head || control.pack.Nack {
			t.Fatal("SYNACK did not follow the missing DATA in the same actual source generation")
		}
		controlId, err := IdFromBytes(control.pack.MessageId)
		if err != nil {
			t.Fatal(err)
		}
		var item *sendItem
		for _, candidate := range sequence.sendItems {
			if candidate != nil && candidate.messageId == controlId {
				item = candidate
			}
		}
		if item == nil || !item.noBudget || !item.expectsAck || item.memoryBudget != nil || tx.UsedByteCount() != beforeTx {
			t.Fatal("SYNACK changed DATA permission or lost its actual reliable owner")
		}
		encoded := bytes.Clone(control.bytes)
		if legacy {
			encoded = synAckLegacyReceiverWire(t, encoded)
			MessagePoolReturn(control.bytes)
			control.bytes = MessagePoolCopy(encoded)
		}
		fixture.forward(control, fixture.receiverIn)
		earlyReceipts := synAckForwardReceipts(t, fixture, missing, control, true)
		early := synAckVisits.Load() == 1 && lifecycle.terminal(t, controlToken) == 1 && earlyReceipts == 1
		if receiver.nextSequenceNumber != base || lifecycle.terminal(t, missingToken) != 0 || tx.UsedByteCount() != beforeTx || fixture.sender.Ctx().Err() != nil || fixture.receiver.Ctx().Err() != nil {
			t.Fatal("early SYNACK changed the unrelated DATA floor, charge or shared lifetime")
		}
		if legacy && (synAckVisits.Load() != 0 || lifecycle.terminal(t, controlToken) != 0 || earlyReceipts != 0) {
			t.Fatal("old-schema receiver invented early delivery or an unearned terminal receipt")
		}
		if legacy && (sequence.immediateSendLimit != 1 || control.pack.DeliverImmediately && sequence.immediateSendCount != 1) {
			t.Fatal("legacy peer did not preserve the actual one-original immediate count until cumulative ACK")
		}
		// Replay the exact captured wire before the hole heals. A new peer may
		// repeat the receipt, but may never repeat the callback or its terminal.
		duplicate := &windowRoundFrame{bytes: MessagePoolCopy(encoded)}
		fixture.heldFrames = append(fixture.heldFrames, duplicate)
		fixture.forward(duplicate, fixture.receiverIn)
		synAckForwardReceipts(t, fixture, missing, control, true)
		if synAckVisits.Load() > 1 || lifecycle.terminal(t, controlToken) > 1 || lifecycle.terminal(t, missingToken) != 0 {
			t.Fatal("duplicate SYNACK repeated delivery or acknowledged the missing predecessor")
		}
		fixture.forward(missing, fixture.receiverIn)
		synAckForwardReceipts(t, fixture, missing, control, false)
		if synAckVisits.Load() != 1 || lifecycle.terminal(t, controlToken) != 1 || lifecycle.terminal(t, missingToken) != 1 ||
			receiver.nextSequenceNumber != base+2 || len(sequence.sendItems) != 0 || sequence.resendQueue.Len() != 0 || sequence.immediateSendCount != 0 {
			t.Fatal("actual head repair did not drain legacy/current SYNACK exactly once")
		}
		duplicate = &windowRoundFrame{bytes: MessagePoolCopy(encoded)}
		fixture.heldFrames = append(fixture.heldFrames, duplicate)
		fixture.forward(duplicate, fixture.receiverIn)
		synAckForwardReceipts(t, fixture, missing, control, false)
		wantDeliveries := 3
		if batch {
			wantDeliveries++
		}
		if synAckVisits.Load() != 1 || fixture.deliveredCount != wantDeliveries || lifecycle.terminal(t, controlToken) != 1 ||
			root.UsedByteCount() != 0 || tx.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || time.Now() != started {
			t.Fatal("post-repair replay leaked, redelivered, or required a timer to settle")
		}
		t.Logf("SYNACK_PROVIDER_CUSTODY batch=%t legacy=%t option=%t early=%t early_receipts=%d same_generation=true missing_data_acked_after_repair=true deliveries=%d terminal=3 timer_advance=0", batch, legacy, control.pack.DeliverImmediately, early, earlyReceipts, fixture.deliveredCount)
		if !legacy && (!control.pack.DeliverImmediately || !early) {
			t.Error("SYNACK_ORDERED_BEHIND_UNRELATED_DATA: actual reliable provider SYNACK waited for unrelated DATA repair")
		}
	})
}

// Actual single-packet producer, reliable early receipt and duplicate closure.
func TestProviderSynAckImmediateBeforeUnrelatedDataRepair(t *testing.T) {
	testProviderSynAckImmediate(t, false, false)
}

// Actual batch producer must choose the same policy before Client ownership.
func TestProviderSynAckBatchImmediateBeforeUnrelatedDataRepair(t *testing.T) {
	testProviderSynAckImmediate(t, true, false)
}

// Old receivers ignore the additive option, so handshake completion still
// waits for the gap. Ordinary cumulative ACKs then retire the same source owner.
func TestProviderSynAckLegacyReceiverCumulativeFallback(t *testing.T) {
	testProviderSynAckImmediate(t, false, true)
}
