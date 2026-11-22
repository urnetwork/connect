// A logical current consumer head can need its bounded sibling budget before
// any tail earned a promise. Actual signed lanes isolate foreign RX custody.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Constructor policies are immutable throughout the witness. The sender emits
// its own opening controls and full-proof retries; no sequence or proof state
// is installed by the test after startup.
func newConsumerTentativeHeadFixture(t *testing.T, rx, recovery *TransferMemoryBudget) (*windowRoundFixture, *receiveGapTestLogger) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := &receiveGapTestLogger{Logger: NewNoopLogger()}
	settings := func(receiver bool) *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = log
		value.EncryptionSettings.Mode = EncryptionModeOff
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.SequenceBufferSize = 0
		value.SendBufferSettings.ProtocolVersion = 2
		value.SendBufferSettings.CompactContractHead = false
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveQueueBudget = nil
		if receiver {
			receive := value.ReceiveBufferSettings
			receive.ReceiveQueueBudget, receive.ReceiveHeadRecoveryBudget = rx, recovery
			receive.ReceiveQueueRetainedByteAccounting = true
			receive.ReceiveQueueMinByteCount = 0
			receive.ReceiveQueueMaxByteCount = kib(64)
			receive.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			receive.AdvertiseReceiveWindow = false
			receive.PackQueueBudget = nil
			receive.ReliablePackHandoffTimeout = 0
		}
		return value
	}
	fixture := &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
		senderOut: make(Route, 128), senderIn: make(Route, 128), receiverOut: make(Route, 128), receiverIn: make(Route, 128)}
	senderId, receiverId := NewId(), NewId()
	fixture.sender = NewClient(ctx, senderId, &idleContractTestOob{sourceId: senderId, peer: func() *Client { return fixture.receiver }}, settings(false))
	fixture.receiver = NewClient(ctx, receiverId, &idleContractTestOob{sourceId: receiverId, peer: func() *Client { return fixture.sender }}, settings(true))
	fixture.sender.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.senderOut})
	fixture.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.senderIn})
	fixture.receiver.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.receiverOut})
	fixture.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.receiverIn})
	for _, client := range []*Client{fixture.sender, fixture.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider {
				fixture.deliveredCount++
			}
		}
	})
	t.Cleanup(func() {
		cancel()
		owner, release := context.WithTimeout(context.Background(), 5*time.Second)
		defer release()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(owner); err != nil {
				t.Errorf("close signed tentative consumer client: %v", err)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return fixture, log
}

// Two real lanes share fixed RX. The foreign lane keeps the large owner;
// reclaiming the target's own small tentative tail cannot fund its current head.
func testReceiveConsumerTentativeHead(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignBytes, tailBytes = ByteCount(5120), ByteCount(1536)
		const rxBytes = foreignBytes + tailBytes
		root := NewTransferMemoryBudget(2 * rxBytes)
		rx := NewTransferMemoryBudgetWithParent(rxBytes, root)
		recovery := NewTransferMemoryBudgetWithParent(rxBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, recovery, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("consumer witness retained logical quota after shutdown")
				}
			}
		})
		fixture, log := newConsumerTentativeHeadFixture(t, rx, recovery)
		lastApplication := map[uint32]uint64{}
		write := func(lane uint32, payloadBytes int) *windowRoundFrame {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 19, bytes.Repeat([]byte{0x5a}, payloadBytes))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				if err == nil {
					fixture.ackedCount++
				}
			}, time.Second, TransferKey{LogicalLane: lane})
			if !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("TENTATIVE_CONSUMER_PUBLIC_SEND_FAILED lane=%d accepted=%t error=%v", lane, accepted, err)
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					t.Fatal("TENTATIVE_CONSUMER_SENDER_BODY_MISSING")
				}
				if wire.pack.LogicalLane != lane {
					continue
				}
				for _, payload := range wire.pack.Frames {
					if payload.MessageType == protocol.MessageType_IpIpPacketFromProvider {
						if last, present := lastApplication[lane]; present && wire.pack.SequenceNumber <= last {
							break
						}
						lastApplication[lane] = wire.pack.SequenceNumber
						return wire
					}
				}
				if len(wire.pack.Frames) != 0 {
					// Keep earlier retries off the receiver while the next
					// application is selected from this actual sender lane.
					continue
				}
				// The actual sender's contract-only control precedes every new
				// application lane. Deliver it before capturing its typed data.
				fixture.forward(wire, fixture.receiverIn)
				fixture.acknowledge()
			}
			t.Fatal("TENTATIVE_CONSUMER_TYPED_SOURCE_NOT_PUBLISHED")
			return nil
		}
		foreignHead := write(1, 1280)
		head := write(0, 1280)
		foreignTail := write(1, 1280)
		tail := write(0, 32)
		if foreignHead.pack.LogicalLane != 1 || head.pack.LogicalLane != 0 || head.pack.Head || head.pack.ContractFrame != nil ||
			foreignTail.pack.Head || tail.pack.Head || foreignTail.pack.ContractFrame != nil || tail.pack.ContractFrame != nil ||
			!bytes.Equal(foreignTail.pack.SequenceId, foreignHead.pack.SequenceId) || !bytes.Equal(tail.pack.SequenceId, head.pack.SequenceId) ||
			bytes.Equal(head.pack.SequenceId, foreignHead.pack.SequenceId) || fixture.deliveredCount != 0 || fixture.ackedCount != 0 || rx.UsedByteCount() != 0 {
			t.Fatal("TENTATIVE_CONSUMER_REAL_LANE_PREMISE_FAILED")
		}
		findSequence := func(frame *windowRoundFrame) *ReceiveSequence {
			buffer := fixture.receiver.receiveBuffer
			buffer.mutex.Lock()
			defer buffer.mutex.Unlock()
			for id, sequence := range buffer.receiveSequences {
				if id.SequenceId == RequireIdFromBytes(frame.pack.SequenceId) && id.LogicalLane == frame.pack.LogicalLane {
					return sequence
				}
			}
			t.Fatal("TENTATIVE_CONSUMER_REAL_RECEIVE_SEQUENCE_MISSING")
			return nil
		}
		sequence, foreign := findSequence(head), findSequence(foreignHead)
		current := sequence.receiveContract
		if current == nil || !current.verifiedReceiveProofSet || sequence.nextSequenceNumber != head.pack.SequenceNumber ||
			foreign.receiveContract == nil || fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
			t.Fatal("TENTATIVE_CONSUMER_VERIFIED_OPENING_PREMISE_FAILED")
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignItem := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId))
		if foreignItem == nil || foreignItem.committed || foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignBytes ||
			rx.UsedByteCount() != foreignBytes || foreign.holdPromise.set || len(fixture.receiverOut) != 0 {
			t.Fatal("TENTATIVE_CONSUMER_FOREIGN_CUSTODY_PREMISE_FAILED")
		}
		fixture.forward(tail, fixture.receiverIn)
		tailItem := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
		if tailItem == nil || tailItem.committed || tailItem.memoryBudget != rx || tailItem.QueueByteCount() != tailBytes ||
			sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 || sequence.holdPromise.ingressFloor != head.pack.SequenceNumber ||
			sequence.receiveContract != current || rx.UsedByteCount() != rxBytes || root.UsedByteCount() != rxBytes || recovery.UsedByteCount() != 0 ||
			len(fixture.receiverOut) != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("TENTATIVE_CONSUMER_NO_PROMISE_FULL_RX_PREMISE_FAILED")
		}
		arrival := head
		if mode != "original" {
			fixture.drop(head)
			arrival = pressureContractNaturalRetry(t, fixture, head.pack)
			if !arrival.pack.Head || arrival.pack.ContractFrame == nil || arrival.pack.ContractId != nil ||
				sha256.Sum256(arrival.pack.ContractFrame.MessageBytes) != current.verifiedReceiveProof ||
				!proto.Equal(arrival.pack.Frames[0], head.pack.Frames[0]) {
				t.Fatal("TENTATIVE_CONSUMER_GENUINE_FULL_RETRY_PREMISE_FAILED")
			}
		}
		if mode == "bad-mac" {
			pack := proto.Clone(arrival.pack).(*protocol.Pack)
			var contract protocol.Contract
			if err := proto.Unmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil || len(contract.StoredContractHmac) == 0 {
				t.Fatal("TENTATIVE_CONSUMER_SIGNED_RETRY_MALFORMED")
			}
			contract.StoredContractHmac[0] ^= 0x80
			var err error
			pack.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
			if err != nil {
				t.Fatal(err)
			}
			rewriteHeldHeadContractWire(t, arrival, pack)
		}
		path, err := FilteredTransferPath(arrival.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(arrival.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.consumerFrameCount != 1 || scan.frameCount != 1 ||
			scan.sequenceNumber != sequence.holdPromise.ingressFloor || scan.retainedByteCount <= tailBytes ||
			scan.retainedByteCount > recovery.TotalByteCount() || scan.retainedByteCount > root.Available() ||
			path.SourceId != fixture.sender.ClientId() || path.DestinationId != fixture.receiver.ClientId() {
			t.Fatal("TENTATIVE_CONSUMER_FUNDED_EXACT_FLOOR_PREMISE_FAILED")
		}
		var ackUpdates atomic.Int32
		sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
		observed, paid := false, false
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if actual == sequence && owner != nil && stage == "pack_released" && owner.receiveItem.messageId == scan.messageId {
				observed = true
				paid = owner.receiveItem.memoryBudget == recovery && owner.receiveItem.QueueByteCount() == scan.retainedByteCount &&
					owner.receivePack.ingressLoanByteCount == 0 && recovery.UsedByteCount() == scan.retainedByteCount &&
					rx.UsedByteCount() == rxBytes && root.UsedByteCount() == rxBytes+scan.retainedByteCount &&
					root.UsedByteCount() <= root.TotalByteCount() && !sequence.holdPromise.set &&
					sequence.holdPromise.reservedByteCount == 0 && !sequence.memoryPressure.activeSet
			}
		}
		fixture.forward(arrival, fixture.receiverIn)
		if !observed || !paid {
			t.Fatalf("TENTATIVE_CONSUMER_CURRENT_HEAD_CHILD_UNUSED mode=%s observed=%t paid=%t delivered=%d acked=%d next=%d rx=%d child=%d root=%d",
				mode, observed, paid, fixture.deliveredCount, fixture.ackedCount, sequence.nextSequenceNumber,
				rx.UsedByteCount(), recovery.UsedByteCount(), root.UsedByteCount())
		}
		if mode == "bad-mac" {
			if sequence.ctx.Err() == nil || !sequence.rejectRetransmits || sequence.receiveContract != current ||
				sequence.nextSequenceNumber != head.pack.SequenceNumber || fixture.deliveredCount != 0 || fixture.ackedCount != 0 ||
				ackUpdates.Load() != 0 || len(log.messages()) == 0 {
				t.Fatal("TENTATIVE_CONSUMER_BAD_MAC_BYPASSED_ORDINARY_VERIFIER")
			}
		} else {
			fixture.acknowledge()
			if fixture.deliveredCount != 2 || fixture.ackedCount != 2 || sequence.nextSequenceNumber != tail.pack.SequenceNumber+1 ||
				sequence.receiveQueue.Len() != 0 || sequence.ctx.Err() != nil || len(log.messages()) != 0 {
				t.Fatal("TENTATIVE_CONSUMER_CURRENT_HEAD_DID_NOT_DRAIN_EXACT_PREFIX")
			}
		}
		if foreign.ctx.Err() != nil || foreign.receiveQueue.GetByMessageId(foreignItem.messageId) != foreignItem || foreignItem.committed ||
			foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignBytes || foreign.holdPromise.set ||
			rx.UsedByteCount() != foreignBytes || root.UsedByteCount() != foreignBytes || recovery.UsedByteCount() != 0 {
			t.Fatal("TENTATIVE_CONSUMER_RECOVERY_CHANGED_FOREIGN_CUSTODY_OR_LEAKED_CHILD")
		}
	})
}

// Logical currentness does not require the sender's Head bit when the already
// verified opening contract remains authoritative for the original payload.
func TestReceiveConsumerTentativeCurrentOriginalUsesPaidChild(t *testing.T) {
	testReceiveConsumerTentativeHead(t, "original")
}

// An actual source retry promotes the same current original and restores its
// full signed proof, while the receiver still owns only tentative later data.
func TestReceiveConsumerTentativeFullProofRetryUsesPaidChild(t *testing.T) {
	testReceiveConsumerTentativeHead(t, "retry")
}

// Fresh bounded storage is not proof authority. A changed MAC reaches the
// ordinary verifier and cancels only its receive lane, with no application ACK.
func TestReceiveConsumerTentativeFullProofBadMacUsesPaidChild(t *testing.T) {
	testReceiveConsumerTentativeHead(t, "bad-mac")
}
