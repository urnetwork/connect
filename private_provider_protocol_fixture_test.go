// Real DATA callback custody supplies byte pressure independently of reliable
// protocol originals. No protocol owner is manufactured as prepaid DATA.
package connect

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The callback borrows one actual decoded DATA root until release. A second
// real DATA original is refused before handoff and remains owned by its sender.
type privateProviderProtocolDataPressure struct {
	wire           *windowRoundFixture
	rx             *TransferMemoryBudget
	gate           chan struct{}
	feedback       []*windowRoundFrame
	before         TransferMemoryBudgetStats
	oldLimit       ByteCount
	closed         bool
	callbackActive atomic.Bool
	joined         bool
}

func newPrivateProviderProtocolDataPressure(t *testing.T, rx *TransferMemoryBudget) *privateProviderProtocolDataPressure {
	t.Helper()
	if rx == nil || rx.UsedByteCount() != 0 {
		t.Fatal("independent DATA pressure requires an empty actual RX leaf")
	}
	pressure := &privateProviderProtocolDataPressure{rx: rx, gate: make(chan struct{}), oldLimit: rx.TotalByteCount()}
	pressure.wire = newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueBudget = rx
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveQueueMinByteCount = 0
		settings.ReceiveQueueMaxByteCount = pressure.oldLimit
		settings.PackQueueBudget = nil
		settings.PackQueueRetainedByteAccounting = false
	})
	t.Cleanup(pressure.close)
	constructed := false
	defer func() {
		if !constructed {
			t.Log("[provider-data-pressure-constructor-unwind] joining pressure owners before caller defers")
			pressure.close()
		}
	}()
	entered := make(chan struct{}, 1)
	pressure.wire.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		pressure.callbackActive.Store(true)
		defer pressure.callbackActive.Store(false)
		if len(frames) != 1 || frames[0].MessageType != protocol.MessageType_TestSimpleMessage {
			t.Error("DATA pressure callback lost its actual application frame")
			return
		}
		entered <- struct{}{}
		select {
		case <-pressure.gate:
		case <-pressure.wire.ctx.Done():
		}
	})
	owner := pressure.wire.write(8192)
	scan, status := scanReceiveMemoryPack(owner.bytes, TransferPath{SourceId: pressure.wire.sender.ClientId(), DestinationId: pressure.wire.receiver.ClientId()})
	if status != receiveMemoryPackScanReady || !scan.loanSafe || protocolOnlyPack(owner.pack) ||
		scan.retainedByteCount <= 0 || pressure.oldLimit < scan.retainedByteCount {
		t.Fatal("DATA pressure did not construct one bounded actual decoded DATA owner")
	}
	// Tighten only this existing leaf before admission. The root, provider
	// parent and recovery ceilings never change, and release never resizes it.
	rx.SetTotalByteCount(scan.retainedByteCount)
	pressure.wire.forward(owner, pressure.wire.receiverIn)
	select {
	case <-entered:
	default:
		t.Fatal("actual DATA callback did not reach its ownership barrier")
	}
	if rx.UsedByteCount() != scan.retainedByteCount || rx.Available() != 0 {
		t.Fatal("actual borrowed DATA root did not fill the independently configured RX leaf")
	}
	pressure.before = rx.Stats()
	refused := pressure.wire.write(2048)
	before := pressure.wire.receiver.ReceiveStats()
	pressure.wire.forward(refused, pressure.wire.receiverIn)
	after := pressure.wire.receiver.ReceiveStats()
	pressure.wire.t.Logf("[provider-data-pressure-cut] workspace_drop=%d->%d pack_drop=%d->%d rx_before=%+v rx_after=%+v delivered=%d want1 acked=%d want0 callback_active=%t head_budget_present=%t", before.ReceiveDecodePressureDropCount, after.ReceiveDecodePressureDropCount, before.PackHandoffDropCount, after.PackHandoffDropCount, pressure.before, rx.Stats(), pressure.wire.deliveredCount, pressure.wire.ackedCount, pressure.callbackActive.Load(), pressure.wire.receiver.settings.ReceiveBufferSettings.receiveHeadRecoveryBudget() != nil)
	if after.PackHandoffDropCount != before.PackHandoffDropCount+1 ||
		after.ReceiveDecodePressureDropCount != before.ReceiveDecodePressureDropCount ||
		rx.Stats() != pressure.before || pressure.wire.deliveredCount != 1 || pressure.wire.ackedCount != 0 {
		t.Fatalf("full actual DATA leaf guard: workspace_unchanged=%t pack_drop_delta=%d rx_equal=%t delivered_match=%t acked_match=%t", after.ReceiveDecodePressureDropCount == before.ReceiveDecodePressureDropCount, after.PackHandoffDropCount-before.PackHandoffDropCount, rx.Stats() == pressure.before, pressure.wire.deliveredCount == 1, pressure.wire.ackedCount == 0)
	}
	for len(pressure.wire.receiverOut) != 0 {
		ack := pressure.wire.take(pressure.wire.receiverOut)
		if ack.ack == nil || bytes.Equal(ack.ack.MessageId, refused.pack.MessageId) {
			t.Fatal("refused additional DATA acquired a false ACK")
		}
		pressure.feedback = append(pressure.feedback, ack)
	}
	privateProviderAssertOriginalPending(t, pressure.wire, refused.pack, false, false)
	constructed = true
	return pressure
}

func (self *privateProviderProtocolDataPressure) check(t *testing.T) {
	t.Helper()
	synctest.Wait()
	if self.closed || self.rx.Stats() != self.before || self.rx.Available() != 0 || self.wire.ackedCount != 0 {
		t.Fatalf("actual DATA custody changed: closed=%t rx=%+v expected=%+v available=%d acked=%d want0 callback_active=%t joined=%t", self.closed, self.rx.Stats(), self.before, self.rx.Available(), self.wire.ackedCount, self.callbackActive.Load(), self.joined)
	}
}

// Return the borrowed DATA owner, then join only its own producer/receiver.
// The refused second DATA original receives cancellation, never delivery ACK.
func (self *privateProviderProtocolDataPressure) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.wire.t.Logf("[provider-data-pressure-close-before] rx=%+v root=%+v callback_active=%t", self.rx.Stats(), self.rx.admissionRoot().Stats(), self.callbackActive.Load())
	close(self.gate)
	synctest.Wait()
	for _, ack := range self.feedback {
		self.wire.forward(ack, self.wire.senderIn)
	}
	self.wire.acknowledge()

	self.wire.cancel()
	joined := true
	for _, client := range []*Client{self.wire.sender, self.wire.receiver} {
		if err := client.CloseAndWait(context.Background()); err != nil {
			joined = false
			self.wire.t.Error(err)
		}
	}
	self.joined = joined
	synctest.Wait()
	after := self.rx.Stats()
	if !joined || self.callbackActive.Load() || after.UsedByteCount != 0 || after.QuotaByteCount != 0 ||
		after.ReservedByteCount != after.ReleasedByteCount || after.TotalByteCount != self.before.TotalByteCount || self.wire.ackedCount != 1 {
		self.wire.t.Errorf("actual DATA owner survived its joined cleanup: joined=%t callback_active=%t rx=%+v expected_limit=%d acked=%d want1", joined, self.callbackActive.Load(), after, self.before.TotalByteCount, self.wire.ackedCount)
	}
	self.wire.t.Logf("[provider-data-pressure-close-after-joins] joined=%t rx=%+v root=%+v callback_active=%t acked=%d", joined, self.rx.Stats(), self.rx.admissionRoot().Stats(), self.callbackActive.Load(), self.wire.ackedCount)
}

// Encoded originals are lifetime owners even when their DATA accounting is
// zero. This reads only after quiescence, before any captured real ACK is sent.
func privateProviderAssertOriginalPending(t *testing.T, fixture *windowRoundFixture, original *protocol.Pack, noBudget, immediate bool) {
	t.Helper()
	synctest.Wait()
	fixture.sender.sendBuffer.mutex.Lock()
	var source *SendSequence
	for _, sequence := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
		if sequence.sequenceId == RequireIdFromBytes(original.SequenceId) {
			source = sequence
		}
	}
	fixture.sender.sendBuffer.mutex.Unlock()
	if source == nil {
		t.Fatal("actual reliable original lost its live source generation")
	}
	item := source.resendQueue.GetByMessageId(RequireIdFromBytes(original.MessageId))
	if item == nil || !item.expectsAck || item.sequenceNumber != original.SequenceNumber ||
		item.noBudget != noBudget || item.deliverImmediately != immediate || len(item.transferFrameBytes) == 0 ||
		noBudget && (item.memoryBudget != nil || item.MessageByteCount() != 0 || item.QueueByteCount() != 0) ||
		!noBudget && item.MessageByteCount() <= 0 {
		t.Fatal("encoded reliable original was retired early, lost its policy or acquired false DATA custody")
	}
}

// The real receiver retains decoded roots and downstream receipts at its
// configured count boundary, without converting those roots into DATA credit.
func privateProviderAssertProtocolReceiptCustody(t *testing.T, sequence *ReceiveSequence, expected int) {
	t.Helper()
	synctest.Wait()
	queue := sequence.immediateDeliveryQueue
	if queue == nil {
		t.Fatal("actual immediate protocol original has no delivery receipt owner")
	}
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	count := len(queue.items) + len(sequence.protocolPacks)
	if count != expected || expected > queue.maxCount || queue.bytes != 0 || queue.controlBytes != 0 {
		t.Fatalf("actual protocol count/custody changed: retained=%d expected=%d limit=%d bytes=%d", count, expected, queue.maxCount, queue.bytes)
	}
	for _, receipt := range queue.items {
		item := receipt.item
		if item == nil || !item.noBudget || !item.deliverImmediately || item.decodedOwner == nil ||
			item.memoryBudget != nil || item.MessageByteCount() != 0 || item.QueueByteCount() != 0 ||
			item.deliveryPrepared || item.deliveryPrepaid != 0 || len(item.transferFrameBytes) == 0 || receipt.bytes != 0 {
			t.Fatal("actual protocol receipt lost its decoded lifetime owner or acquired DATA prepayment")
		}
	}
}

// A real terminal receipt must remove its exact encoded source owner as well
// as invoking the public completion. Protocol bytes are never a leak oracle.
func privateProviderAssertOriginalRetired(t *testing.T, fixture *windowRoundFixture, original *protocol.Pack) {
	t.Helper()
	synctest.Wait()
	fixture.sender.sendBuffer.mutex.Lock()
	var sequence *SendSequence
	for _, source := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
		if source.sequenceId == RequireIdFromBytes(original.SequenceId) {
			sequence = source
		}
	}
	fixture.sender.sendBuffer.mutex.Unlock()
	if sequence != nil && sequence.resendQueue.GetByMessageId(RequireIdFromBytes(original.MessageId)) != nil {
		t.Fatal("actual terminal ACK completed its callback but retained the encoded reliable original")
	}
}

// Reads the exact already-published generation after worker quiescence. The
// fixture's rendezvous setting deliberately permits one serialized immediate
// owner; accepting its successor does not promise a second wire publication.
func privateProviderImmediateSource(t *testing.T, fixture *windowRoundFixture, sequenceId []byte) *SendSequence {
	t.Helper()
	synctest.Wait()
	fixture.sender.sendBuffer.mutex.Lock()
	defer fixture.sender.sendBuffer.mutex.Unlock()
	for _, source := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
		if source.sequenceId == RequireIdFromBytes(sequenceId) {
			if source.sendBufferSettings.SequenceBufferSize != 0 || source.immediateSendLimit != 1 {
				t.Fatal("fixture changed its actual rendezvous/immediate count-one constructor policy")
			}
			return source
		}
	}
	t.Fatal("actual immediate original lost its source generation")
	return nil
}

func privateProviderAssertImmediateCount(t *testing.T, fixture *windowRoundFixture, original *protocol.Pack, expected int) {
	t.Helper()
	source := privateProviderImmediateSource(t, fixture, original.SequenceId)
	if source.immediateSendCount != expected {
		t.Fatalf("actual immediate count changed without its real terminal receipt: count=%d expected=%d limit=%d", source.immediateSendCount, expected, source.immediateSendLimit)
	}
}

// The successor has transferred one real raw owner to count admission, but no
// ordinal or encoded ACK original exists until the first actual ACK frees the
// sole slot. Ordinary session controls retain their observed ordinals.
// No timer, queue resize or fabricated receipt selects this cut.
func privateProviderAssertQueuedImmediateSuccessor(t *testing.T, fixture *windowRoundFixture, current *protocol.Pack, controls ...*protocol.Pack) {
	t.Helper()
	source := privateProviderImmediateSource(t, fixture, current.SequenceId)
	source.protocolAdmission.mutex.Lock()
	queued := source.protocolAdmission.count
	source.protocolAdmission.mutex.Unlock()
	intervening := make(map[uint64]bool, len(controls))
	for _, control := range controls {
		if control == nil || !privateProviderEncryptedControl(control) ||
			!bytes.Equal(control.SequenceId, current.SequenceId) || control.SequenceNumber <= current.SequenceNumber ||
			control.SequenceNumber >= source.nextSequenceNumber || intervening[control.SequenceNumber] {
			t.Fatal("queued immediate successor has an unknown, duplicate or foreign intervening original")
		}
		intervening[control.SequenceNumber] = true
	}
	complete := source.nextSequenceNumber == current.SequenceNumber+1+uint64(len(intervening))
	for number := current.SequenceNumber + 1; complete && number < source.nextSequenceNumber; number++ {
		complete = intervening[number]
	}
	if source.immediateSendCount != 1 || queued != 1 || !complete ||
		source.resendQueue.GetBySequenceNumber(source.nextSequenceNumber) != nil || len(fixture.senderOut) != 0 {
		t.Fatalf("accepted successor bypassed or lost the actual occupied count-one slot: immediate=%d queued=%d next=%d current=%d wires=%d", source.immediateSendCount, queued, source.nextSequenceNumber, current.SequenceNumber, len(fixture.senderOut))
	}
	privateProviderAssertOriginalPending(t, fixture, current, true, true)
}

// Actual downstream custody, including its TCP acknowledgement frontier, must
// finish before real Transfer feedback can retire the encoded source original.
func privateProviderAssertReturnFrontier(t *testing.T, tcp *TcpSequence, acknowledgement uint32) {
	t.Helper()
	tcp.mutex.Lock()
	defer tcp.mutex.Unlock()
	if tcp.receiveSeqAck != acknowledgement || tcp.returnByteCount != 0 || tcp.returnHead != len(tcp.returnChunks) {
		t.Fatalf("actual inner ACK did not retire exact return custody: ack=%d want=%d retained=%d", tcp.receiveSeqAck, acknowledgement, tcp.returnByteCount)
	}
}

// A public mixed/UDP DATA head remains byte-budgeted and ordered. Its pure
// ACK successors use actual IP policy and can release TCP return custody while
// DATA is full, without acknowledging the refused predecessor. Returning the
// separate DATA callback then permits only the original sender's real retry.
func privateProviderDataHeadWithProtocolTails(t *testing.T, f *privateProviderEncryptedAckFixture,
	head *privateProviderEncryptedAckFrame, acknowledgement uint32, pressureEnabled bool,
	retry func() *privateProviderEncryptedAckFrame,
) {
	t.Helper()
	first := head.pack.SequenceNumber
	rootTotal, parentTotal := f.root.TotalByteCount(), f.parent.TotalByteCount()
	retained := f.tcp.returnByteCount
	if retained <= 0 || head.pack.DeliverImmediately || protocolOnlyPack(head.pack) || head.pack.Nack {
		t.Fatal("public DATA head lost its ordered reliable policy or actual retained TCP return chunk")
	}
	var pressure *privateProviderProtocolDataPressure
	if pressureEnabled {
		pressure = newPrivateProviderProtocolDataPressure(t, f.rx)
		defer pressure.close()
	}
	rxLimit := f.rx.TotalByteCount()
	before, childBefore := f.receiver.ReceiveStats(), f.recovery.Stats()
	tail := f.writeAcks(1, acknowledgement)
	if !tail.pack.DeliverImmediately || !protocolOnlyPack(tail.pack) {
		t.Fatal("actual TCP ACK tail did not use immediate protocol policy")
	}
	privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
	privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 1)
	f.forward(tail.windowRoundFrame, f.receiverIn)
	privateProviderAssertProtocolReceiptCustody(t, f.sequence, 1)
	queued := f.enqueueAcks(1, acknowledgement)
	f.assertQueuedAckOriginal(tail.pack, queued)
	f.forward(head.windowRoundFrame, f.receiverIn)
	privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, false, false)
	expectedNext := first
	if !pressureEnabled {
		expectedNext = first + 1
	}
	t.Logf("[provider-data-head-cut] pressure=%t entered=%d return_bytes=%d want%d acked=%d want1 head_acks=%d want0 next=%d first=%d expected_next=%d floor=%d rx=%+v child=%+v ctx_done=%t", pressureEnabled, len(f.entered), f.tcp.returnByteCount, retained, f.ackedCount, f.ackCounts[first], f.sequence.nextSequenceNumber, first, expectedNext, f.sequence.holdPromise.ingressFloor, f.rx.Stats(), f.recovery.Stats(), f.sequence.ctx.Err() != nil)
	if len(f.entered) == 0 || f.tcp.returnByteCount != retained || f.ackedCount != 1 ||
		f.ackCounts[first] != 0 || f.sequence.nextSequenceNumber != expectedNext {
		t.Fatalf("held actual ACK guard: entered_match=%t retained_match=%t acked_match=%t head_ack_match=%t next_expected=%d next_match=%t", len(f.entered) != 0, f.tcp.returnByteCount == retained, f.ackedCount == 1, f.ackCounts[first] == 0, expectedNext, f.sequence.nextSequenceNumber == expectedNext)
	}
	if pressure != nil {
		pressure.check(t)
		if f.receiver.ReceiveStats().PackHandoffDropCount != before.PackHandoffDropCount+1 ||
			f.receiver.ReceiveStats().ReceiveDecodePressureDropCount != before.ReceiveDecodePressureDropCount || f.recovery.Stats() != childBefore {
			t.Fatalf("full-RX ordinary head refusal changed: pack_drop=%d->%d workspace_drop=%d->%d child_before=%+v child_after=%+v", before.PackHandoffDropCount, f.receiver.ReceiveStats().PackHandoffDropCount, before.ReceiveDecodePressureDropCount, f.receiver.ReceiveStats().ReceiveDecodePressureDropCount, childBefore, f.recovery.Stats())
		}
	}
	close(f.gate)
	synctest.Wait()
	privateProviderAssertReturnFrontier(t, f.tcp, acknowledgement)
	privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
	f.feedback()
	if f.ackCounts[tail.pack.SequenceNumber] != 1 {
		t.Fatal("the immediate successor serialized without the first tail's actual terminal ACK")
	}
	privateProviderAssertOriginalRetired(t, f.windowRoundFixture, tail.pack)
	if pressure != nil {
		pressure.check(t)
		if f.ackedCount != 2 || f.deliveredCount != 2 || f.ackCounts[first] != 0 || f.sequence.nextSequenceNumber != first ||
			f.sequence.receiveQueue.Len() != 0 || f.recovery.Stats() != childBefore {
			t.Fatal("actual immediate ACK did not retire only itself across the refused DATA head")
		}
		privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, false, false)
	}
	tails := []*privateProviderEncryptedAckFrame{tail}
	for index := 1; index < 10; index++ {
		if index == 1 {
			tail = f.takeAckOriginal(queued)
		} else {
			tail = f.writeAcks(1, acknowledgement)
		}
		tails = append(tails, tail)
		privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
		privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 1)
		f.forward(tail.windowRoundFrame, f.receiverIn)
		privateProviderAssertReturnFrontier(t, f.tcp, acknowledgement)
		privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
		if pressure != nil {
			pressure.check(t)
		}
		f.feedback()
		if f.ackCounts[tail.pack.SequenceNumber] != 1 {
			t.Fatal("actual count-one ACK tail skipped or repeated its terminal callback")
		}
		privateProviderAssertOriginalRetired(t, f.windowRoundFixture, tail.pack)
		privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 0)
	}
	if pressure != nil {
		pressure.check(t)
		if f.ackedCount != 11 || f.deliveredCount != 11 || f.ackCounts[first] != 0 || f.sequence.nextSequenceNumber != first {
			t.Fatal("ten actual immediate receipts falsely completed the refused DATA original")
		}
		privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, false, false)
		// Returning the actual DATA callback changes no ceiling. Only the
		// original sender may retry its still-owned DATA identity afterward.
		pressure.close()
		ownedRetry := retry()
		if !bytes.Equal(ownedRetry.pack.MessageId, head.pack.MessageId) || !bytes.Equal(ownedRetry.pack.SequenceId, head.pack.SequenceId) ||
			ownedRetry.pack.SequenceNumber != first || ownedRetry.pack.DeliverImmediately || protocolOnlyPack(ownedRetry.pack) {
			t.Fatal("actual DATA retry changed its identity, class or ordering policy")
		}
		privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, false, false)
		f.forward(ownedRetry.windowRoundFrame, f.receiverIn)
		f.feedback()
	}
	originals := append([]*privateProviderEncryptedAckFrame{head}, tails...)
	f.assertCompletedOriginals(originals)
	if f.ackedCount != 12 || f.deliveredCount != 11+len(head.pack.Frames) || f.ackCounts[first] != 1 ||
		f.sequence.receiveQueue.Len() != 0 ||
		f.rx.UsedByteCount() != 0 || f.recovery.UsedByteCount() != 0 ||
		f.rx.TotalByteCount() != rxLimit || f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
		f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
		f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
		t.Fatal("actual DATA head/retry and count-one ACK train did not close all reliable ownership")
	}
	for _, original := range originals {
		if f.ackCounts[original.pack.SequenceNumber] != 1 {
			t.Fatal("public DATA/protocol sequence skipped or repeated a real terminal callback")
		}
	}
	privateProviderAssertOriginalRetired(t, f.windowRoundFixture, head.pack)
	for _, original := range tails {
		privateProviderAssertOriginalRetired(t, f.windowRoundFixture, original.pack)
	}
	f.close()
	t.Logf("[provider-data-head-protocol-tails] pressure=%t data_head_frames=%d protocol_tails=10 immediate_limit=1 retained_chunk_bytes=%d callbacks=12 root=0 rx=0 child=0 nat=0 joined=true", pressureEnabled, len(head.pack.Frames), retained)
}

// Generic Transfer calls keep their ordinary source policy unless the IP
// producer supplies its local options. Receive independently validates the
// packet class, but cannot invent unordered delivery or a terminal source ACK.
func TestPrivateProviderEncodedAckDefaultKeepsOrderedSourcePolicy(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		rx := NewTransferMemoryBudget(64 * 1024)
		t.Cleanup(func() { assertRetainedBudgetBalance(t, rx) })
		fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueBudget = rx
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueMinByteCount = 0
			settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			settings.AdvertiseReceiveWindow = false
		})
		warm := fixture.write(16)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		missing := fixture.write(16)
		packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7000, nil))
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil || !frame.Raw || !protocolOnlyFrames([]*protocol.Frame{frame}) {
			MessagePoolReturn(packet)
			t.Fatal("default Transfer policy control did not construct an actual pure ACK")
		}
		if !fixture.sender.SendMultiWithTimeout([]*protocol.Frame{frame}, fixture.receiver.ClientId(), func(err error) {
			if err == nil {
				fixture.ackedCount++
			}
		}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("ordinary public Transfer caller refused its actual ACK original")
		}
		original := fixture.takePack(2)
		if original.pack.DeliverImmediately || original.pack.Nack || !protocolOnlyPack(original.pack) {
			t.Fatal("ordinary public Transfer call silently inherited IP producer delivery policy")
		}
		privateProviderAssertOriginalPending(t, fixture, original.pack, false, false)
		fixture.forward(original, fixture.receiverIn)
		sequence := fixture.receiveSequence()
		held := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(original.pack.MessageId))
		if held == nil || !held.noBudget || held.deliverImmediately || held.memoryBudget != nil ||
			held.MessageByteCount() != 0 || held.QueueByteCount() != 0 || held.deliveryPrepared || held.deliveryPrepaid != 0 ||
			sequence.nextSequenceNumber != missing.pack.SequenceNumber || fixture.deliveredCount != 1 || fixture.ackedCount != 1 ||
			rx.UsedByteCount() != sequence.holdPromise.reservedByteCount {
			t.Fatal("default ordered pure ACK acquired DATA custody, bypassed its predecessor or falsely completed")
		}
		privateProviderAssertOriginalPending(t, fixture, original.pack, false, false)
		fixture.forward(missing, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.ackedCount != 3 || fixture.deliveredCount != 3 || sequence.nextSequenceNumber != 3 ||
			sequence.receiveQueue.Len() != 0 || rx.UsedByteCount() != 0 {
			t.Fatal("actual default-policy DATA predecessor and ordered ACK did not drain through real feedback")
		}
		privateProviderAssertOriginalRetired(t, fixture, original.pack)
	})
}
