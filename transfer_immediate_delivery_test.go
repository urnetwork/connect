// Real public reliable originals cross one DATA hole without borrowing DATA
// credit. Compact receipt evidence survives loss, drain and worker recreation.
package connect

import (
	"bytes"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestImmediatePublicTenThousandReceiptsKeepLostMiddleAndExactOnce(t *testing.T) {
	testImmediatePublicReceiptTrain(t, false)
}

func TestImmediatePublicReceiptHistorySurvivesSameGenerationWorkerRecreation(t *testing.T) {
	testImmediatePublicReceiptTrain(t, true)
}

func testImmediatePublicReceiptTrain(t *testing.T, recreate bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(2 * 1024 * 1024)
		tx := NewTransferMemoryBudgetWithParent(1024*1024, root)
		rx := NewTransferMemoryBudgetWithParent(1024*1024, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, rx, root} {
				assertRetainedBudgetBalance(t, budget)
			}
		})
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.SequenceBufferSize = 64
			settings.ResendQueueBudget = tx
			settings.ResendQueueRetainedByteAccounting = true
			settings.ResendQueueMinByteCount = 0
			settings.LiveDiagnostics = true
		}, func(settings *ReceiveBufferSettings) {
			settings.SequenceBufferSize = 64
			settings.ReceiveQueueBudget = rx
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueMinByteCount = 0
		}, TransportTypeH1)
		const count = 10003
		const firstTcpSequence = 500000
		visits := make([]atomic.Uint32, count)
		completions := make([]protocolPressureCompletion, count)
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					continue
				}
				tcp := protocolClassTcpPacket(frame.MessageBytes)
				if tcp == nil || len(tcp) < 20 {
					t.Error("immediate callback lost its validated TCP header")
					continue
				}
				number := binary.BigEndian.Uint32(tcp[4:8])
				if number < firstTcpSequence || firstTcpSequence+count <= number {
					t.Error("immediate callback contains another source packet")
					continue
				}
				visits[number-firstTcpSequence].Add(1)
			}
		})
		warm := fixture.write(16)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		missing := fixture.write(1240)
		base := missing.pack.SequenceNumber
		sequence := fixture.sequence()
		receiver := fixture.receiveSequence()
		if base != 1 || receiver.nextSequenceNumber != base || sequence.immediateSendLimit != 64 ||
			tx.UsedByteCount() <= 0 || rx.UsedByteCount() != 0 {
			t.Fatal("real DATA owner did not establish the intended live single-generation hole")
		}
		originalTx := tx.Stats()
		started := time.Now()
		middle := count / 2
		var middleWire []byte
		var middleId Id
		var middleNumber uint64
		terminalAcks := 0
		for index := range count {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck,
				uint32(firstTcpSequence+index), nil))
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil || !frame.Raw || !immediateIpAckFrames([]*protocol.Frame{frame}) {
				MessagePoolReturn(packet)
				t.Fatal("public immediate train did not construct one actual pure ACK")
			}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(),
				completions[index].record, time.Second, NoBudget(), DeliverImmediately())
			if !accepted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("public immediate original %d refused: accepted=%t err=%v", index, accepted, err)
			}
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || wire.pack.Nack || !wire.pack.DeliverImmediately || wire.pack.Head ||
				wire.pack.SequenceNumber != base+1+uint64(index) ||
				!bytes.Equal(wire.pack.SequenceId, missing.pack.SequenceId) {
				t.Fatal("public immediate original lost actual reliable ordinal/generation")
			}
			if index == middle {
				middleWire = bytes.Clone(wire.bytes)
				middleId, _ = IdFromBytes(wire.pack.MessageId)
				middleNumber = wire.pack.SequenceNumber
			}
			fixture.forward(wire, fixture.receiverIn)
			ack := fixture.take(fixture.receiverOut)
			if ack.ack == nil || !ack.ack.Selective || !ack.ack.DeliveredImmediately ||
				ack.ack.ImmediateSequenceNumber == nil || *ack.ack.ImmediateSequenceNumber != wire.pack.SequenceNumber ||
				!bytes.Equal(ack.ack.MessageId, wire.pack.MessageId) || len(ack.ack.EvictedSequenceNumbers) != 0 {
				t.Fatal("early secured callback did not produce only its actual exact terminal receipt")
			}
			if index == middle {
				MessagePoolReturn(ack.bytes)
				ack.bytes = nil
			} else {
				fixture.forward(ack, fixture.senderIn)
				terminalAcks++
			}
			if visits[index].Load() != 1 || index != middle && completions[index].success.Load() != 1 ||
				completions[middle].calls.Load() != 0 {
				t.Fatal("public receipt train duplicated delivery or treated the deliberately lost receipt as received")
			}
			// The fixture may discard decoded wire projections once transferred;
			// only the one exact duplicate source remains as test-owned bytes.
			wire.pack = nil
			ack.ack = nil
			if receiver.nextSequenceNumber != base || fixture.ackedCount != 1 ||
				tx.Stats().UsedByteCount != originalTx.UsedByteCount || rx.UsedByteCount() != 0 {
				t.Fatal("immediate train advanced or funded the missing DATA predecessor")
			}
		}
		if receiver.immediateHistory == nil || time.Now() != started || terminalAcks != count-1 {
			t.Fatal("immediate train did not keep bounded replay authority without timer advancement")
		}
		history := receiver.immediateHistory
		history.stateLock.Lock()
		spans := 0
		for _, span := range history.ranges {
			if span != nil && (span.pending || span.secured) {
				spans++
			}
		}
		pending := history.pendingCount
		history.stateLock.Unlock()
		if spans != 1 || pending != 0 || receiver.receiveQueue.Len() != 0 {
			t.Fatal("contiguous successful protocol train retained per-original receiver state")
		}
		sequence.readSendLiveSnapshot()
		synctest.Wait()
		before := sequence.liveDiagnostics.latest.Load()
		if before == nil || before.OriginalItemCount != 2 || sequence.immediateSendCount != 1 ||
			len(sequence.immediateTerminals) > 2 {
			t.Fatal("terminal receipt train did not bound live sender originals and compressed evidence")
		}
		if recreate {
			receiver.cancel()
			<-receiver.done
			synctest.Wait()
		}
		duplicate := &windowRoundFrame{bytes: MessagePoolCopy(middleWire)}
		fixture.heldFrames = append(fixture.heldFrames, duplicate)
		fixture.forward(duplicate, fixture.receiverIn)
		retryAck := fixture.take(fixture.receiverOut)
		if retryAck.ack == nil || !retryAck.ack.Selective || !retryAck.ack.DeliveredImmediately ||
			!bytes.Equal(retryAck.ack.MessageId, middleId[:]) || retryAck.ack.ImmediateSequenceNumber == nil ||
			*retryAck.ack.ImmediateSequenceNumber != middleNumber {
			t.Fatal("lost middle receipt could not be recovered before the DATA hole healed")
		}
		fixture.forward(retryAck, fixture.senderIn)
		if visits[middle].Load() != 1 || completions[middle].calls.Load() != 1 || completions[middle].success.Load() != 1 {
			t.Fatal("lost exact receipt recovery redelivered payload or lost reliable terminal completion")
		}
		receiver = fixture.receiveSequence()
		if receiver.nextSequenceNumber != base || receiver.immediateHistory != history ||
			sequence.resendQueue.Len() != 1 || sequence.immediateSendCount != 0 {
			t.Fatal("receipt recovery changed DATA custody, replay generation or written-original count")
		}
		fixture.forward(missing, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		if receiver.nextSequenceNumber != base+1+count || fixture.ackedCount != 2 ||
			sequence.resendQueue.Len() != 0 || len(sequence.sendItems) != 0 || root.UsedByteCount() != 0 {
			t.Fatal("actual DATA release did not cumulatively drain only already-delivered immediate markers")
		}
		duplicate = &windowRoundFrame{bytes: MessagePoolCopy(middleWire)}
		fixture.heldFrames = append(fixture.heldFrames, duplicate)
		fixture.forward(duplicate, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		for index := range count {
			if visits[index].Load() != 1 || completions[index].calls.Load() != 1 || completions[index].success.Load() != 1 {
				t.Fatalf("original %d repeated callback, debit or terminal fate after the real head advanced", index)
			}
		}
		t.Logf("IMMEDIATE_TRAIN originals=%d receiver_spans=%d sender_ranges=%d lost_middle_recovered=true recreate=%t timer_advance=0", count, spans, len(sequence.immediateTerminals), recreate)
	})
}

// Pure controls cannot inherit ordering intent from a mixed/lifecycle sibling.
func TestImmediateIpAckPolicyKeepsPayloadAndLifecycleOrdered(t *testing.T) {
	for _, flags := range []byte{tcpFlagAck, tcpFlagSyn, tcpFlagFin, tcpFlagRst, tcpFlagSyn | tcpFlagAck} {
		for _, payload := range [][]byte{nil, {0x31}} {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), flags, 4000, payload)
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
			expected := (flags == tcpFlagAck || flags == tcpFlagSyn|tcpFlagAck) && len(payload) == 0
			if immediateIpAckFrames([]*protocol.Frame{frame}) != expected {
				t.Fatalf("automatic immediate policy changed flags=%d payload=%d", flags, len(payload))
			}
			mixed := []*protocol.Frame{frame, {MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: []byte{1}}}
			if immediateIpAckFrames(mixed) {
				t.Fatal("one pure ACK granted unordered delivery to an application sibling")
			}
		}
	}
}
