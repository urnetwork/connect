// Public signed H1 bursts distinguish a redundant encoded-byte refusal from
// actual physical/quota/count/channel limits. This is not a performance run.
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
)

type consumerPaidPackByteLayout struct {
	label                           string
	byteLimit, leafBytes, rootBytes ByteCount
	countLimit                      int
}

// Limits reproduce the two actual source-guarded SDK cut geometries. Every
// knob and budget is assigned before NewClient; no live settings mutation.
func newConsumerPaidPackByteFixture(t *testing.T, layout consumerPaidPackByteLayout, rx, recovery, pack *TransferMemoryBudget, progress func(TransferProgressEvent), configure ...func(bool, *ClientSettings)) (*windowRoundFixture, *receiveGapTestLogger, *expirationLifecycleAuthority, *expirationLifecycleAuthority) {
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
		value.SendBufferSettings.CompactContractHead = true
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveQueueBudget = nil
		value.ReceiveBufferSettings.ProgressObserver = progress
		if receiver {
			receive := value.ReceiveBufferSettings
			receive.ReceiveQueueBudget, receive.ReceiveHeadRecoveryBudget = rx, recovery
			receive.ReceiveQueueRetainedByteAccounting = true
			receive.ReceiveQueueMinByteCount = 0
			receive.ReceiveQueueMaxByteCount = layout.leafBytes
			receive.SequenceBufferSize, receive.H1SequenceBufferSize = layout.countLimit, layout.countLimit
			receive.SequenceBufferByteCount, receive.H1SequenceBufferByteCount = layout.byteLimit, layout.byteLimit
			receive.H1SequenceBufferAdaptiveMaxSize = 0
			receive.H1SequenceBufferAdaptiveStepSize = 0
			receive.H1SequenceBufferAdaptiveSaturationThreshold = 0
			receive.H1SequenceBufferAdaptiveSaturationWindow = 0
			receive.H1SequenceBufferAdaptiveMaxByteCount = 0
			receive.H1SequenceBufferAdaptiveStepByteCount = 0
			receive.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			receive.AdvertiseReceiveWindow = false
			receive.PackQueueBudget = pack
			receive.PackQueueRetainedByteAccounting = false
			receive.ProgressObserver = progress
			receive.H1PackHandoffTimeout = -1
			receive.ReliablePackHandoffTimeout = -1
			receive.H1AckHandoffTimeout = time.Millisecond
		}
		for _, change := range configure {
			change(receiver, value)
		}
		return value
	}
	fixture := &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
		senderOut: make(Route, 128), senderIn: make(Route, 128), receiverOut: make(Route, 128), receiverIn: make(Route, 128)}
	senderId, receiverId := NewId(), NewId()
	forward := &expirationLifecycleAuthority{ctx: ctx, sourceId: senderId,
		peer: func() *Client { return fixture.receiver }, initialDeadline: time.Now().Add(time.Hour),
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	reverse := &expirationLifecycleAuthority{ctx: ctx, sourceId: receiverId,
		peer: func() *Client { return fixture.sender }, initialDeadline: time.Now().Add(time.Hour),
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	fixture.sender = NewClient(ctx, senderId, forward, settings(false))
	fixture.receiver = NewClient(ctx, receiverId, reverse, settings(true))
	fixture.sender.RouteManager().UpdateTransportWithProperties(newPrivateH1PeerGateway(receiverId, TransportTypeH1), []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	fixture.sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{fixture.senderIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	fixture.receiver.RouteManager().UpdateTransportWithProperties(newPrivateH1PeerGateway(senderId, TransportTypeH1), []Route{fixture.receiverOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	fixture.receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{fixture.receiverIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
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
	return fixture, log, forward, reverse
}

type consumerPaidPackByteWire struct {
	frame     *windowRoundFrame
	scan      receiveMemoryPackScan
	acked     *atomic.Bool
	completed *atomic.Int64
}

// Each public SendMulti emits exactly one real H1 Pack of eight 1280-byte
// raw consumer packets. Empty signed openings traverse the ordinary worker.
func consumerPaidPackByteSend(t *testing.T, fixture *windowRoundFixture, lane uint32, frameCount int, slot int, acked *atomic.Bool, completed *atomic.Int64, started time.Time) consumerPaidPackByteWire {
	t.Helper()
	frames := make([]*protocol.Frame, frameCount)
	for index := range frames {
		packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, uint32(1000+slot*8+index), bytes.Repeat([]byte{0x58}, 1240))
		if len(packet) != 1280 {
			t.Fatal("test source did not construct an actual1280-byte TCP packet")
		}
		frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: MessagePoolCopy(packet)}
	}
	accepted := fixture.sender.SendMultiWithTimeout(frames, fixture.receiver.ClientId(), func(err error) {
		if err == nil {
			completed.Store(time.Since(started).Nanoseconds())
			acked.Store(true)
		}
	}, time.Second, TransferKey{LogicalLane: lane})
	if !accepted {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		t.Fatal("public signed H1 source refused its caller-owned burst")
	}
	for range cap(fixture.senderOut) {
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil {
			t.Fatal("public H1 source published a non-Pack")
		}
		if wire.pack.LogicalLane != lane {
			continue
		}
		if len(wire.pack.Frames) == 0 {
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			continue
		}
		if len(wire.pack.Frames) != frameCount || wire.pack.Nack {
			t.Fatal("actual H1 producer fragmented or changed the reliable source group")
		}
		for _, frame := range wire.pack.Frames {
			if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider || !frame.Raw || len(frame.MessageBytes) != 1280 {
				t.Fatal("actual public H1 wire lost its consumer packet group")
			}
		}
		path, err := FilteredTransferPath(wire.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(wire.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.consumerFrameCount != frameCount || scan.messageByteCount != ByteCount(frameCount*1280) || scan.retainedByteCount < ByteCount(len(wire.bytes)) {
			t.Fatal("actual public H1 wire did not have its complete payable owned decode envelope")
		}
		return consumerPaidPackByteWire{frame: wire, scan: scan, acked: acked, completed: completed}
	}
	t.Fatal("actual H1 application group was hidden behind a full route")
	return consumerPaidPackByteWire{}
}

type consumerPaidPackByteResult struct {
	blocked                     bool
	queued                      int64
	encoded, incoming, physical ByteCount
	recovery                    time.Duration
	firstCompletion             time.Duration
}

func testConsumerPaidPackByteBurst(t *testing.T, layout consumerPaidPackByteLayout) consumerPaidPackByteResult {
	t.Helper()
	GetMessagePoolAggregateStats()
	var result consumerPaidPackByteResult
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("paid H1 burst retained shared quota after Clients joined")
				}
			}
		})
		var target atomic.Pointer[Id]
		var targetEnds atomic.Int64
		var targetAccepted atomic.Bool
		var cumulativeReceipts, targetCumulative, targetSelective atomic.Uint64
		progress := func(event TransferProgressEvent) {
			if event.Stage == "receive_ack_begin" && !event.Selective {
				cumulativeReceipts.Add(1)
			}
			if id := target.Load(); id != nil && event.MessageId == *id {
				switch event.Stage {
				case "receive_pack_end":
					targetAccepted.Store(event.Success)
					targetEnds.Add(1)
				case "receive_ack_begin":
					if event.Selective {
						targetSelective.Add(1)
					} else {
						targetCumulative.Add(1)
					}
				}
			}
		}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress)
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		var arm atomic.Bool
		var hotCompleted, siblingCompleted atomic.Int64
		var callbackInvalid atomic.Bool
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if !arm.Load() {
				return
			}
			if peer.TransferKey.LogicalLane == 0 {
				enteredOnce.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-fixture.ctx.Done():
					}
				})
				for _, frame := range frames {
					if frame == nil || !frame.Raw || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider || len(frame.MessageBytes) != 1280 {
						callbackInvalid.Store(true)
					}
				}
				hotCompleted.Add(int64(len(frames)))
			} else if peer.TransferKey.LogicalLane == 1 {
				siblingCompleted.Add(int64(len(frames)))
			}
		})
		var receipts [64]atomic.Bool
		var completions [64]atomic.Int64
		started := time.Now()
		nextSlot := 0
		send := func(lane uint32, count int) consumerPaidPackByteWire {
			if nextSlot == len(receipts) {
				t.Fatal("bounded burst source exceeded its64 ordinary public sends")
			}
			slot := nextSlot
			nextSlot++
			return consumerPaidPackByteSend(t, fixture, lane, count, slot, &receipts[slot], &completions[slot], started)
		}
		warm := send(0, 1)
		fixture.forward(warm.frame, fixture.receiverIn)
		fixture.acknowledge()
		sequence := consumerForeignHeadSequence(t, fixture, warm.frame)
		sender := consumerEstablishedWireHeadSender(t, fixture, warm.frame)
		if !warm.acked.Load() || cumulativeReceipts.Load() == 0 || sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet || len(sender.sendItems) != 0 || root.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("real signed H1 warm source did not fully ACK/refund")
		}
		if sequence.packQueueH1ByteLimit != layout.byteLimit || sequence.packQueueH1Limit != int64(layout.countLimit) || cap(sequence.packs) != layout.countLimit {
			t.Fatal("shipping logical limits were not installed before worker construction")
		}
		arm.Store(true)
		seed := send(0, 8)
		fixture.forward(seed.frame, fixture.receiverIn)
		<-entered
		if seed.acked.Load() || hotCompleted.Load() != 0 || sequence.packQueueCount.Load() != 0 || pack.Stats().QuotaByteCount != 0 || rx.UsedByteCount() != seed.scan.retainedByteCount {
			t.Fatal("real callback did not own the seed while leaving the actual H1 queue empty")
		}
		var hot []consumerPaidPackByteWire
		hot = append(hot, seed)
		var overflow consumerPaidPackByteWire
		for range layout.countLimit {
			next := send(0, 8)
			if next.frame.pack.Head || next.frame.pack.ContractFrame != nil || next.frame.pack.ContractId != nil || next.frame.pack.ContractAhead {
				t.Fatal("queued burst unexpectedly changed contract or original wire class")
			}
			if layout.byteLimit-sequence.packQueueByteCount.Load() < ByteCount(len(next.frame.bytes)) {
				overflow = next
				break
			}
			beforeCount, beforeBytes := sequence.packQueueCount.Load(), sequence.packQueueByteCount.Load()
			encoded := ByteCount(len(next.frame.bytes))
			fixture.forward(next.frame, fixture.receiverIn)
			if next.acked.Load() || sequence.packQueueCount.Load() != beforeCount+1 || sequence.packQueueByteCount.Load() != beforeBytes+encoded || len(sequence.packs) != int(sequence.packQueueCount.Load()) {
				t.Fatal("paid prefix failed to fill the real queue without ACK or anonymous budget bytes")
			}
			hot = append(hot, next)
		}
		if overflow.frame == nil {
			t.Fatal("shipping count gate was reached before the independently measured byte boundary")
		}
		incoming := ByteCount(len(overflow.frame.bytes))
		beforeCount, beforeBytes := sequence.packQueueCount.Load(), sequence.packQueueByteCount.Load()
		beforeRx, beforeRoot, beforeQuota := rx.UsedByteCount(), root.UsedByteCount(), pack.Stats().QuotaByteCount
		if beforeCount+1 >= int64(layout.countLimit) || len(sequence.packs) >= cap(sequence.packs) || beforeBytes+incoming <= layout.byteLimit || beforeQuota != beforeBytes ||
			rx.Available() < overflow.scan.retainedByteCount || root.Available() < overflow.scan.retainedByteCount || pack.receiveQuotaAvailable(root) < incoming || head.UsedByteCount() != 0 {
			t.Fatal("actual overflow was not isolated to logical bytes with count/channel/physical/shared-quota headroom")
		}
		id := overflow.scan.messageId
		target.Store(&id)
		beforeDrops := fixture.receiver.ReceiveStats().PackHandoffDropCount
		fixture.forward(overflow.frame, fixture.receiverIn)
		if targetEnds.Load() != 1 {
			t.Fatal("actual shared Client did not finish its zero-wait overflow offer")
		}
		result.blocked = !targetAccepted.Load()
		result.queued, result.encoded, result.incoming, result.physical = beforeCount, beforeBytes, incoming, overflow.scan.retainedByteCount
		wantCount, wantBytes, wantRx := beforeCount+1, beforeBytes+incoming, beforeRx+overflow.scan.retainedByteCount
		wantDrops := uint64(0)
		if result.blocked {
			wantCount, wantBytes, wantRx, wantDrops = beforeCount, beforeBytes, beforeRx, 1
		}
		if overflow.acked.Load() || targetCumulative.Load() != 0 || targetSelective.Load() != 0 || sequence.packQueueCount.Load() != wantCount || sequence.packQueueByteCount.Load() != wantBytes || len(sequence.packs) != int(wantCount) || rx.UsedByteCount() != wantRx || root.UsedByteCount() != beforeRoot+wantRx-beforeRx || pack.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != beforeQuota+wantBytes-beforeBytes || fixture.receiver.ReceiveStats().PackHandoffDropCount-beforeDrops != wantDrops {
			t.Fatal("overflow changed real custody, exceeded a hard limit, ACKed the held source or lost its exact refund")
		}
		if head.UsedByteCount() != 0 || root.UsedByteCount() > layout.rootBytes || rx.UsedByteCount() > layout.leafBytes || pack.Stats().QuotaByteCount > layout.leafBytes {
			t.Fatal("logical comparison bypass changed a hard physical/shared-quota ceiling")
		}
		// A distinct real worker must complete through the same Client while
		// the hot callback still owns its seed and all queued originals.
		beforeSiblingReceipts := cumulativeReceipts.Load()
		sibling := send(1, 1)
		fixture.forward(sibling.frame, fixture.receiverIn)
		fixture.acknowledge()
		if !sibling.acked.Load() || cumulativeReceipts.Load() <= beforeSiblingReceipts || targetCumulative.Load() != 0 || targetSelective.Load() != 0 || siblingCompleted.Load() != 1 || hotCompleted.Load() != 0 || seed.acked.Load() || overflow.acked.Load() || sequence.packQueueCount.Load() != wantCount || sequence.packQueueByteCount.Load() != wantBytes || rx.UsedByteCount() != wantRx {
			t.Fatal("healthy sibling failed progress or disturbed the held hot queue")
		}
		hot = append(hot, overflow)
		for _, wire := range hot {
			if wire.acked.Load() {
				t.Fatal("a held hot original received a false completion ACK")
			}
		}
		releasedAt := time.Now()
		releasedNs := time.Since(started).Nanoseconds()
		unblock()
		synctest.Wait()
		fixture.acknowledge()
		if result.blocked {
			number, present := sender.resendQueue.ContainsMessageId(overflow.scan.messageId)
			if overflow.acked.Load() || targetCumulative.Load() != 0 || targetSelective.Load() != 0 || !present || number != overflow.scan.sequenceNumber {
				t.Fatal("refused original lost its actual sender retry owner")
			}
			retry := pressureContractNaturalRetry(t, fixture, overflow.frame.pack)
			if !bytes.Equal(retry.pack.MessageId, overflow.frame.pack.MessageId) || !bytes.Equal(retry.pack.SequenceId, overflow.frame.pack.SequenceId) || retry.pack.SequenceNumber != overflow.frame.pack.SequenceNumber || len(retry.pack.Frames) != len(overflow.frame.pack.Frames) {
				t.Fatal("natural sender recovery changed the source identity")
			}
			for index := range retry.pack.Frames {
				if !proto.Equal(retry.pack.Frames[index], overflow.frame.pack.Frames[index]) {
					t.Fatal("natural retry changed source payload")
				}
			}
			fixture.forward(retry, fixture.receiverIn)
			fixture.acknowledge()
		}
		result.recovery = time.Since(releasedAt)
		result.firstCompletion = time.Duration(overflow.completed.Load() - releasedNs)
		if result.firstCompletion < 0 {
			t.Fatal("held overflow completed before actual callback release")
		}
		for _, wire := range hot {
			if !wire.acked.Load() {
				t.Fatal("actual retained sender original did not recover and ACK")
			}
		}
		if targetCumulative.Load() == 0 || targetSelective.Load() != 0 || hotCompleted.Load() != int64(8*len(hot)) || siblingCompleted.Load() != 1 || callbackInvalid.Load() || len(sender.sendItems) != 0 || sequence.packQueueCount.Load() != 0 || sequence.packQueueByteCount.Load() != 0 || len(sequence.packs) != 0 || sequence.receiveQueue.Len() != 0 || sequence.holdPromise.ingressHead != nil || root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 || len(log.messages()) != 0 {
			t.Fatal("actual burst recovery lost delivery/ACK, duplicated payload or retained physical/queue ownership")
		}
		t.Logf("PAID_PACK_BYTE_BURST_BEHAVIOR_DRAINED profile=%s blocked=%t queued=%d encoded=%d incoming=%d C=%d first_completion_ns=%d recovery_ns=%d sibling_completed=1", layout.label, result.blocked, result.queued, result.encoded, result.incoming, result.physical, result.firstCompletion.Nanoseconds(), result.recovery.Nanoseconds())
	})
	return result
}

func TestReceivePaidH1Burst32KeepsCanonicalPhysicalCustody(t *testing.T) {
	result := testConsumerPaidPackByteBurst(t, consumerPaidPackByteLayout{label: "32", byteLimit: 174762, leafBytes: 2181038, rootBytes: 21810380, countLimit: 85})
	if t.Failed() {
		return
	}
	t.Log("PAID_PACK_BYTE_BURST_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("PAID_H1_ORIGINAL_REFUSED_BY_REDUNDANT_LOGICAL_BYTES: profile=32 queued=%d encoded=%d incoming=%d C=%d natural_recovery_ns=%d", result.queued, result.encoded, result.incoming, result.physical, result.recovery.Nanoseconds())
	}
}

func TestReceivePaidH1Burst64KeepsCanonicalPhysicalCustody(t *testing.T) {
	result := testConsumerPaidPackByteBurst(t, consumerPaidPackByteLayout{label: "64", byteLimit: 262144, leafBytes: 4362076, rootBytes: 43620760, countLimit: 170})
	if t.Failed() {
		return
	}
	t.Log("PAID_PACK_BYTE_BURST_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("PAID_H1_ORIGINAL_REFUSED_BY_REDUNDANT_LOGICAL_BYTES: profile=64 queued=%d encoded=%d incoming=%d C=%d natural_recovery_ns=%d", result.queued, result.encoded, result.incoming, result.physical, result.recovery.Nanoseconds())
	}
}
