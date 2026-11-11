// A reliable UDP head can precede TCP ACK tails on one public Transfer
// sequence. Its own downstream TCP growth is zero, but refusing its encoded
// owner can still strand the tails that release an actual TCP return chunk.
package connect

import (
	"bytes"
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Capture an unchanged public UDP original or its actual source-owned headed
// retry. Keep unrelated provider originals held while session controls flow.
func privateProviderReliableUdpHeadWire(t *testing.T, f *privateProviderEncryptedAckFixture, number uint64, original *privateProviderEncryptedAckFrame) *privateProviderEncryptedAckFrame {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case wire := <-f.senderOut:
			frame := f.capture(wire, f.receiver)
			if frame.pack != nil {
				if frame.pack.SequenceNumber == number && bytes.Equal(frame.pack.SequenceId, f.sequence.sequenceId.Bytes()) {
					if !frame.encrypted || frame.pack.Nack || len(frame.pack.Frames) != 1 || frame.scan.providerTcpFrameCount != 0 {
						t.Fatal("public source did not preserve the single reliable encrypted UDP head")
					}
					member := frame.pack.Frames[0]
					path, err := ParseIpPath(member.MessageBytes)
					if err != nil || member.MessageType != protocol.MessageType_IpIpPacketToProvider || !member.Raw || path.Protocol != IpProtocolUdp {
						t.Fatal("actual reliable UDP head lost its valid raw provider datagram")
					}
					if original != nil && (!frame.pack.Head || frame.pack.ContractFrame == nil && len(frame.pack.ContractId) == 0 ||
						!bytes.Equal(frame.pack.MessageId, original.pack.MessageId) ||
						!bytes.Equal(member.MessageBytes, original.pack.Frames[0].MessageBytes)) {
						t.Fatal("source retry changed the UDP original or lost its headed signed authority")
					}
					return frame
				}
				provider := false
				for _, member := range frame.pack.Frames {
					provider = provider || member.MessageType == protocol.MessageType_IpIpPacketToProvider
				}
				if provider {
					continue
				}
			}
			f.forward(frame.windowRoundFrame, f.receiverIn)
			f.feedback()
		case wire := <-f.receiverOut:
			frame := f.capture(wire, f.sender)
			f.forward(frame.windowRoundFrame, f.senderIn)
		case <-deadline.C:
			t.Fatal("actual source did not emit its reliable UDP original or retry")
		}
	}
}

// Ordinary public delivery is the positive control. Pressure changes only RX
// and child ceilings; real committed TCP tails and source retries must still
// make progress without converting the UDP member into a TCP receipt owner.
func TestPrivateProviderReliableUdpHeadReleasesTcpTailChunks(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, pressure := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newPrivateProviderEncryptedAckFixture(t)
			t.Cleanup(func() {
				f.close()
				t.Logf("[provider-udp-head-cleanup] pressure=%t root=%d rx=%d child=%d nat=%d joined=%t", pressure,
					f.root.UsedByteCount(), f.rx.UsedByteCount(), f.recovery.UsedByteCount(), f.natBudget.UsedByteCount(), f.closed)
			})
			// No UDP flow exists yet. All family buffers use this exact settings
			// pointer, and this test never permits an external socket dial.
			f.nat.settings.UdpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, context.Canceled
			}}
			f.gate = make(chan struct{})
			rootTotal, parentTotal := f.root.TotalByteCount(), f.parent.TotalByteCount()
			end := uint32(500 + 1280)
			f.tcp.mutex.Lock()
			f.tcp.receiveSeq, f.tcp.receiveWindowEnd = end, end+65535
			f.tcp.mutex.Unlock()
			if !f.tcp.retainReturnChunk(bytes.Repeat([]byte{0x53}, 1280), 500, false) {
				t.Fatal("UDP-head fixture could not retain the actual TCP return chunk")
			}
			retained, first := f.tcp.returnByteCount, f.nextNumber
			udpPath := *f.path
			udpPath.Protocol, udpPath.SourcePort = IpProtocolUdp, f.path.SourcePort+1
			packet := MessagePoolCopy(ipOosUdpPacket(&udpPath, bytes.Repeat([]byte{0x23}, 1252)))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
			if !f.sender.SendMultiWithTimeout([]*protocol.Frame{frame}, f.receiver.ClientId(), func(err error) {
				if err == nil {
					f.ackedCount++
					f.ackCounts[first]++
				}
			}, time.Second) {
				MessagePoolReturn(packet)
				t.Fatal("public reliable SendMulti refused the ordinary UDP head")
			}
			f.nextNumber++
			head := privateProviderReliableUdpHeadWire(t, f, first, nil)
			headCost := head.scan.retainedByteCount
			headProbe := &receiveItem{transferItem: transferItem{queueByteCount: headCost}, ack: true, frames: head.pack.Frames}
			if !f.sequence.prepareReceiveDeliveryCredit(headProbe) || headProbe.deliveryPrepared || headProbe.QueueByteCount() != headCost {
				t.Fatal("reliable UDP head incorrectly acquired TCP downstream prepayment")
			}
			tails := make([]*privateProviderEncryptedAckFrame, 10)
			for index := range tails {
				tails[index] = f.writeAcks(1, end)
			}
			tailProbe := &receiveItem{transferItem: transferItem{queueByteCount: tails[0].scan.retainedByteCount}, ack: true, frames: tails[0].pack.Frames}
			if !f.sequence.prepareReceiveDeliveryCredit(tailProbe) || !tailProbe.deliveryPrepared {
				t.Fatal("actual later TCP ACK did not prepay its reliable downstream owner")
			}
			tailFull := tailProbe.QueueByteCount()
			if headCost <= tailFull {
				t.Fatalf("UDP head has no encoded cost shortfall: c=%d prefix=%d", headCost, tailFull)
			}
			repairMetadata, rxTarget := privateProviderRepairFixtureGeometry(t, f.sequence, tailFull, 10)
			if pressure {
				f.rx.SetTotalByteCount(rxTarget)
				f.recovery.SetTotalByteCount(f.rx.TotalByteCount())
			}
			owners := map[uint64]*receiveItem{}
			for _, tail := range tails {
				f.forward(tail.windowRoundFrame, f.receiverIn)
				item := f.sequence.receiveQueue.GetBySequenceNumber(tail.pack.SequenceNumber)
				if item == nil || !item.committed || item.QueueByteCount() != tailFull {
					t.Fatal("UDP-head fixture did not commit actual TCP ACK tails")
				}
				owners[item.sequenceNumber] = item
				f.feedback()
			}
			privateProviderAssertRepairFixtureCustody(t, f.sequence, repairMetadata, 10)
			if f.sequence.holdPromise.reservedByteCount != tailFull || f.sequence.nextSequenceNumber != first ||
				f.tcp.returnByteCount != retained || len(f.entered) != 0 || f.ackedCount != 1 ||
				pressure && f.rx.Available() != 0 || f.recovery.Available() < headCost || f.root.Available() < headCost {
				t.Fatal("UDP-head fixture did not isolate the exact reliable gap with free child/root capacity")
			}
			before := f.receiver.ReceiveStats()
			childBefore := f.recovery.Stats()
			f.forward(head.windowRoundFrame, f.receiverIn)
			attempts := 1
			for len(f.entered) == 0 && attempts < 4 {
				if f.sequence.ctx.Err() != nil || f.ackedCount != 1 || f.deliveredCount != 1 || f.tcp.returnByteCount != retained {
					t.Fatal("UDP-head refusal changed reliable ACK truth or live TCP return custody")
				}
				for number, owner := range owners {
					if f.sequence.receiveQueue.GetBySequenceNumber(number) != owner || !owner.committed {
						t.Fatal("UDP-head refusal evicted an actual committed TCP ACK original")
					}
				}
				retry := privateProviderReliableUdpHeadWire(t, f, first, head)
				f.forward(retry.windowRoundFrame, f.receiverIn)
				attempts++
			}
			if len(f.entered) == 0 {
				t.Fatalf("valid reliable UDP head stranded TCP ACK tails after %d actual source attempts: pressure=%t c=%d prefix=%d tcp_growth=0 rx=%d child=%d root_spare=%d retained_chunk=%d", attempts,
					pressure, headCost, tailFull, f.rx.UsedByteCount(), f.recovery.UsedByteCount(), f.root.Available(), f.tcp.returnByteCount)
			}
			if f.tcp.returnByteCount != retained || f.ackedCount < 1 || f.ackedCount > 2 || f.ackCounts[first] > 1 ||
				pressure && f.recovery.Stats().ReservedByteCount-childBefore.ReservedByteCount != headCost {
				t.Fatal("UDP head did not pay its exact decoded owner before enabling the held TCP tail")
			}
			close(f.gate)
			synctest.Wait()
			f.feedback()
			if f.ackedCount != 12 || f.deliveredCount != 12 || f.ackCounts[first] != 1 ||
				f.sequence.nextSequenceNumber != first+11 || f.sequence.receiveQueue.Len() != 0 ||
				f.rx.UsedByteCount() != 0 || f.recovery.UsedByteCount() != 0 || f.tcp.returnByteCount != 0 ||
				f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
				f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
				f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
				t.Fatal("reliable UDP head did not release all real TCP tails, return bytes and cumulative callbacks")
			}
			f.close()
			t.Logf("[provider-udp-head] pressure=%t encrypted=true signed=true head_c=%d prefix=%d head_tcp_growth=0 tcp_tails=10 retained_chunk_bytes=%d attempts=%d callbacks=12 root=0 rx=0 child=0 nat=0 joined=true", pressure,
				headCost, tailFull, retained, attempts)
		})
	}
}
