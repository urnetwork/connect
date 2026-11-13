// Real signed Client ingress separates predecode pressure from final Pack
// admission while ordinary RX is full. Test geometry is not an SDK profile.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// All immutable policy and observers are installed before worker construction.
func newConsumerFinalHandoffFixture(t *testing.T, rx, recovery, pack *TransferMemoryBudget, ownerStage func(*ReceiveSequence, *decodedPackOwner, string), progress func(TransferProgressEvent)) (*windowRoundFixture, *receiveGapTestLogger) {
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
			receive.PackQueueBudget = pack
			receive.PackQueueRetainedByteAccounting = false
			receive.receiveOwnerStageForTest = ownerStage
			receive.ProgressObserver = progress
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
				t.Errorf("close signed final handoff consumer client: %v", err)
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

// Two actual sender lanes hold foreign and target tentative tails in RX.
// Only Pack's parent changes between the final-handoff controls; the sibling
// recovery retains its own complete physical owner through the real decoder.
func testReceiveConsumerFinalHandoff(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignBytes, tailBytes = ByteCount(5120), ByteCount(1536)
		const rxBytes = foreignBytes + tailBytes
		root := NewTransferMemoryBudget(2 * rxBytes)
		client := NewTransferMemoryBudgetWithParent(2*rxBytes, root)
		rx := NewTransferMemoryBudgetWithParent(rxBytes, client)
		recovery := NewTransferMemoryBudgetWithParent(rxBytes, client)
		packParent := client
		if mode == "rx-parented-pack" {
			packParent = rx
		}
		pack := NewTransferMemoryBudgetWithParent(rxBytes, packParent)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, recovery, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("final handoff retained logical quota after joined clients")
				}
			}
			t.Log("CONSUMER_FINAL_HANDOFF_CLEANUP_JOINED")
		})
		var sequence *ReceiveSequence
		var targetScan receiveMemoryPackScan
		var targetId Id
		var released, refused, begin, end int
		var paid, finalSuccess bool
		var finalError string
		var exactRoots bool
		ownerStage := func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if owner == nil || owner.receiveItem.messageId != targetId || targetId == (Id{}) {
				return
			}
			if stage != "pack_released" && stage != "handoff_refused" {
				return
			}
			if stage == "pack_released" {
				released++
			} else {
				refused++
			}
			receivePack := &owner.receivePack
			carrier := receivePack.TransferFrameBytes
			carrierPooled, _ := MessagePoolCheck(carrier)
			roots := MessagePoolRootByteCount(carrier) + owner.metadataQueueByteCount()
			exactRoots = carrierPooled && receivePack.Pack == &owner.pack && len(owner.pack.Frames) == 1
			for _, frame := range owner.pack.Frames {
				pooled, _ := MessagePoolCheck(frame.MessageBytes)
				exactRoots = exactRoots && pooled && frame.Raw && frame.MessageType == protocol.MessageType_IpIpPacketFromProvider
				if len(frame.MessageBytes) == 0 {
					exactRoots = false
				} else {
					for index := range carrier {
						if &carrier[index] == &frame.MessageBytes[0] {
							exactRoots = false
						}
					}
				}
				roots += MessagePoolRootByteCount(frame.MessageBytes)
			}
			paid = owner.inUse && receivePack.receiveMemoryOwned && owner.receiveItem.memoryBudget == recovery &&
				owner.receiveItem.QueueByteCount() == targetScan.retainedByteCount && roots == targetScan.retainedByteCount &&
				receivePack.ingressSequence == sequence && receivePack.ingressLoanByteCount == 0 &&
				recovery.UsedByteCount() == targetScan.retainedByteCount && rx.UsedByteCount() == rxBytes &&
				client.UsedByteCount() == rxBytes+targetScan.retainedByteCount &&
				root.UsedByteCount() == rxBytes+targetScan.retainedByteCount &&
				pack.UsedByteCount() == 0 && pack.Stats().QuotaByteCount == 0 &&
				!receivePack.sequenceQueueReserved && sequence.packQueueCount.Load() == 0 &&
				sequence.packQueueByteCount.Load() == 0 && sequence.ctx.Err() == nil &&
				!sequence.holdPromise.set && sequence.holdPromise.reservedByteCount == 0 && !sequence.memoryPressure.activeSet &&
				(stage == "pack_released" && actual == sequence || stage == "handoff_refused" && actual == nil)
		}
		progress := func(event TransferProgressEvent) {
			if targetId == (Id{}) || event.MessageId != targetId {
				return
			}
			switch event.Stage {
			case "receive_pack_begin":
				begin++
			case "receive_pack_end":
				end++
				finalSuccess, finalError = event.Success, event.ErrorKind
			}
		}
		configuredRecovery := recovery
		if mode == "predecode" {
			configuredRecovery = nil
		}
		fixture, log := newConsumerFinalHandoffFixture(t, rx, configuredRecovery, pack, ownerStage, progress)
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
				t.Fatalf("CONSUMER_FINAL_HANDOFF_PUBLIC_SEND_FAILED lane=%d accepted=%t error=%v", lane, accepted, err)
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					t.Fatal("CONSUMER_FINAL_HANDOFF_SENDER_BODY_MISSING")
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
			t.Fatal("CONSUMER_FINAL_HANDOFF_TYPED_SOURCE_NOT_PUBLISHED")
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
			t.Fatal("CONSUMER_FINAL_HANDOFF_REAL_LANE_PREMISE_FAILED")
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
			t.Fatal("CONSUMER_FINAL_HANDOFF_REAL_RECEIVE_SEQUENCE_MISSING")
			return nil
		}
		sequence = findSequence(head)
		foreign := findSequence(foreignHead)
		current := sequence.receiveContract
		if current == nil || !current.verifiedReceiveProofSet || sequence.nextSequenceNumber != head.pack.SequenceNumber ||
			foreign.receiveContract == nil || fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
			t.Fatal("CONSUMER_FINAL_HANDOFF_VERIFIED_OPENING_PREMISE_FAILED")
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignItem := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId))
		if foreignItem == nil || foreignItem.committed || foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignBytes ||
			rx.UsedByteCount() != foreignBytes || foreign.holdPromise.set || len(fixture.receiverOut) != 0 {
			t.Fatal("CONSUMER_FINAL_HANDOFF_FOREIGN_CUSTODY_PREMISE_FAILED")
		}
		fixture.forward(tail, fixture.receiverIn)
		tailItem := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
		if tailItem == nil || tailItem.committed || tailItem.memoryBudget != rx || tailItem.QueueByteCount() != tailBytes ||
			sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 || sequence.holdPromise.ingressFloor != head.pack.SequenceNumber ||
			sequence.receiveContract != current || rx.UsedByteCount() != rxBytes || root.UsedByteCount() != rxBytes || recovery.UsedByteCount() != 0 ||
			len(fixture.receiverOut) != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("CONSUMER_FINAL_HANDOFF_NO_PROMISE_FULL_RX_PREMISE_FAILED")
		}
		path, err := FilteredTransferPath(head.bytes)
		if err != nil {
			t.Fatal(err)
		}
		targetScan, status := scanReceiveMemoryPack(head.bytes, path)
		if status != receiveMemoryPackScanReady || !targetScan.loanSafe || targetScan.compatibility ||
			targetScan.consumerFrameCount != 1 || targetScan.frameCount != 1 ||
			targetScan.sequenceNumber != sequence.holdPromise.ingressFloor || targetScan.retainedByteCount <= tailBytes ||
			targetScan.retainedByteCount > recovery.TotalByteCount() || targetScan.retainedByteCount > root.Available() ||
			path.SourceId != fixture.sender.ClientId() || path.DestinationId != fixture.receiver.ClientId() {
			t.Fatal("CONSUMER_FINAL_HANDOFF_FUNDED_EXACT_FLOOR_PREMISE_FAILED")
		}
		targetId = targetScan.messageId
		dropBefore := fixture.receiver.receivePackHandoffDropCount.Load()
		bytesBefore := fixture.receiver.receivePackHandoffDropByteCount.Load()
		fixture.forward(head, fixture.receiverIn)
		drops := fixture.receiver.receivePackHandoffDropCount.Load() - dropBefore
		dropBytes := fixture.receiver.receivePackHandoffDropByteCount.Load() - bytesBefore
		if mode == "predecode" {
			if begin != 0 || end != 0 || released != 0 || refused != 0 || recovery.UsedByteCount() != 0 ||
				drops != 1 || dropBytes != uint64(targetScan.messageByteCount) || fixture.deliveredCount != 0 ||
				fixture.ackedCount != 0 || sequence.nextSequenceNumber != head.pack.SequenceNumber || sequence.ctx.Err() != nil {
				t.Fatalf("CONSUMER_PREDECODE_COUNTER_NOT_ISOLATED begin=%d end=%d released=%d refused=%d drops=%d", begin, end, released, refused, drops)
			}
			t.Log("CONSUMER_PREDECODE_REFUSED_BEFORE_FINAL_CALLBACK")
		} else if mode == "rx-parented-pack" {
			if begin != 1 || end != 1 || released != 0 || refused != 1 || !paid || !exactRoots ||
				finalSuccess || finalError != "refused" || drops != 1 || dropBytes != uint64(targetScan.messageByteCount) ||
				fixture.deliveredCount != 0 || fixture.ackedCount != 0 || sequence.nextSequenceNumber != head.pack.SequenceNumber ||
				sequence.receiveQueue.GetByMessageId(tailItem.messageId) != tailItem || rx.UsedByteCount() != rxBytes ||
				recovery.UsedByteCount() != 0 || client.UsedByteCount() != rxBytes || root.UsedByteCount() != rxBytes {
				t.Fatalf("CONSUMER_REAL_FINAL_CALLBACK_REFUSAL_NOT_ISOLATED begin=%d end=%d released=%d refused=%d paid=%t roots=%t drops=%d", begin, end, released, refused, paid, exactRoots, drops)
			}
			t.Log("CONSUMER_REAL_DECODED_FINAL_CALLBACK_QUOTA_REFUSED")
		} else {
			if begin != 1 || end != 1 || released != 1 || refused != 0 || !paid || !exactRoots ||
				!finalSuccess || finalError != "" || drops != 0 || dropBytes != 0 {
				t.Fatalf("CONSUMER_REAL_FINAL_CALLBACK_SIBLING_FAILED begin=%d end=%d released=%d refused=%d paid=%t roots=%t drops=%d", begin, end, released, refused, paid, exactRoots, drops)
			}
			fixture.acknowledge()
			if fixture.deliveredCount != 2 || fixture.ackedCount != 2 || sequence.nextSequenceNumber != tail.pack.SequenceNumber+1 ||
				sequence.receiveQueue.Len() != 0 || sequence.ctx.Err() != nil || len(log.messages()) != 0 {
				t.Fatal("CONSUMER_REAL_FINAL_CALLBACK_DID_NOT_DRAIN_EXACT_PREFIX")
			}
			t.Log("CONSUMER_REAL_DECODED_FINAL_CALLBACK_SIBLING_PASSED")
		}
		if foreign.ctx.Err() != nil || foreign.receiveQueue.GetByMessageId(foreignItem.messageId) != foreignItem || foreignItem.committed ||
			foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignBytes || foreign.holdPromise.set ||
			recovery.UsedByteCount() != 0 || pack.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("CONSUMER_FINAL_HANDOFF_CHANGED_FOREIGN_CUSTODY_OR_LEAKED_CHILD")
		}
	})
}

// Production-shaped siblings share the existing physical ancestor exactly once.
func TestReceiveConsumerFullRxRealFinalCallbackSiblingPack(t *testing.T) {
	testReceiveConsumerFinalHandoff(t, "sibling-pack")
}

// An intentionally RX-parented Pack retains its independent local permission:
// the fully decoded paid head must refuse at the final callback, without ACK.
func TestReceiveConsumerFullRxRealFinalCallbackRejectsRxQuota(t *testing.T) {
	testReceiveConsumerFinalHandoff(t, "rx-parented-pack")
}

// A missing recovery partition increments the same legacy drop counter before
// any final Pack callback. This prevents counter-only causal attribution.
func TestReceiveConsumerFullRxPredecodeDropDoesNotReachFinalCallback(t *testing.T) {
	testReceiveConsumerFinalHandoff(t, "predecode")
}
