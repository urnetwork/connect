// Public SendMultiWithTimeout permits one reliable Pack containing TCP and
// UDP ToProvider frames. Its TCP ACKs must still release retained return data
// when later committed Packs fill RX; the UDP member keeps ordinary semantics.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Submit the legitimate mixed Pack through the public single-Pack API. Capture
// checks actual decrypted source output without changing any emitted wire.
func privateProviderMixedAckOriginal(t *testing.T, f *privateProviderEncryptedAckFixture, udpPayload int, acknowledgement uint32) *privateProviderEncryptedAckFrame {
	t.Helper()
	frames := make([]*protocol.Frame, 0, 3)
	for range 2 {
		packet := ipOosTcpPacketSequence(f.path, tcpFlagAck, 101, nil)
		segment := packet[Ipv4HeaderSizeWithoutExtensions:]
		binary.BigEndian.PutUint32(segment[8:12], acknowledgement)
		binary.BigEndian.PutUint16(segment[16:18], 0)
		binary.BigEndian.PutUint16(segment[16:18], ipPathTransportChecksum(f.path, ipProtocolNumberTcp, segment))
		frames = append(frames, &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: MessagePoolCopy(packet)})
	}
	udpPath := *f.path
	udpPath.Protocol, udpPath.SourcePort = IpProtocolUdp, f.path.SourcePort+1
	udpPacket := ipOosUdpPacket(&udpPath, bytes.Repeat([]byte{0x67}, udpPayload))
	frames = append(frames, &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: MessagePoolCopy(udpPacket)})
	number := f.nextNumber
	if !f.sender.SendMultiWithTimeout(frames, f.receiver.ClientId(), func(err error) {
		if err == nil {
			f.ackedCount++
			f.ackCounts[number]++
		}
	}, time.Second) {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		t.Fatal("public reliable SendMulti refused the valid mixed TCP and UDP Pack")
	}
	f.nextNumber++
	synctest.Wait()
	for len(f.senderOut) != 0 {
		frame := f.capture(<-f.senderOut, f.receiver)
		if frame.pack != nil && frame.pack.SequenceNumber == number && bytes.Equal(frame.pack.SequenceId, f.sequence.sequenceId.Bytes()) {
			if !frame.encrypted || frame.pack.Nack || len(frame.pack.Frames) != 3 || frame.scan.providerTcpFrameCount != 2 {
				t.Fatal("public SendMulti did not preserve one actual reliable mixed provider Pack")
			}
			for index, member := range frame.pack.Frames {
				path, err := ParseIpPath(member.MessageBytes)
				want := IpProtocolTcp
				if index == 2 {
					want = IpProtocolUdp
				}
				if err != nil || member.MessageType != protocol.MessageType_IpIpPacketToProvider || !member.Raw || path.Protocol != want {
					t.Fatal("actual mixed original lost a valid provider protocol member")
				}
			}
			return frame
		}
		f.forward(frame.windowRoundFrame, f.receiverIn)
		f.feedback()
	}
	t.Fatal("actual source did not emit the mixed reliable original")
	return nil
}

// The sender alone chooses retry time and Head promotion. Unrelated provider
// copies stay held while ordinary session/control traffic keeps its route.
func privateProviderMixedHeadRetry(t *testing.T, f *privateProviderEncryptedAckFixture, original *privateProviderEncryptedAckFrame) *privateProviderEncryptedAckFrame {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case wire := <-f.senderOut:
			frame := f.capture(wire, f.receiver)
			if frame.pack != nil {
				if bytes.Equal(frame.pack.MessageId, original.pack.MessageId) && bytes.Equal(frame.pack.SequenceId, original.pack.SequenceId) {
					if !frame.encrypted || !frame.pack.Head || frame.pack.Nack || frame.scan.providerTcpFrameCount != 2 ||
						len(frame.pack.Frames) != 3 || frame.pack.ContractFrame == nil && len(frame.pack.ContractId) == 0 {
						t.Fatal("source-owned mixed retry lost its headed encrypted recovery authority")
					}
					for index, member := range frame.pack.Frames {
						if member.MessageType != original.pack.Frames[index].MessageType || member.Raw != original.pack.Frames[index].Raw ||
							!bytes.Equal(member.MessageBytes, original.pack.Frames[index].MessageBytes) {
							t.Fatal("source-owned mixed retry changed an original TCP or UDP payload")
						}
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
			t.Fatal("actual source did not retry its unchanged mixed reliable head")
		}
	}
}

// The first control proves ordinary delivery of the exact mixed protocol
// shape. The pressure case changes only the RX/child ceilings, then requires
// progress without permission increases, tail eviction or wire replacement.
func testPrivateProviderMixedReliableHead(t *testing.T, udpPayload int, exceedsPrefix bool) {
	t.Helper()
	for _, pressure := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newPrivateProviderEncryptedAckFixture(t)
			// No UDP flow exists at this quiescent boundary. Both family buffers
			// use this exact settings pointer; no external socket can be opened.
			f.nat.settings.UdpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, context.Canceled
			}}
			f.gate = make(chan struct{})
			rootTotal, parentTotal := f.root.TotalByteCount(), f.parent.TotalByteCount()
			end := uint32(500 + 1280)
			f.tcp.mutex.Lock()
			f.tcp.receiveSeq, f.tcp.receiveWindowEnd = end, end+65535
			f.tcp.mutex.Unlock()
			if !f.tcp.retainReturnChunk(bytes.Repeat([]byte{0x49}, 1280), 500, false) {
				t.Fatal("mixed provider fixture could not retain an actual return chunk")
			}
			retained, first := f.tcp.returnByteCount, f.nextNumber
			head := privateProviderMixedAckOriginal(t, f, udpPayload, end)
			tails := make([]*privateProviderEncryptedAckFrame, 10)
			for index := range tails {
				tails[index] = f.writeAcks(1, end)
			}
			cost := func(frame *privateProviderEncryptedAckFrame) ByteCount {
				probe := &receiveItem{transferItem: transferItem{queueByteCount: frame.scan.retainedByteCount}, ack: true, frames: frame.pack.Frames}
				if !f.sequence.prepareReceiveDeliveryCredit(probe) || !probe.deliveryPrepared {
					t.Fatal("mixed reliable head did not prepay its actual TCP downstream owner")
				}
				return probe.QueueByteCount()
			}
			full, tailFull := cost(head), cost(tails[0])
			if (tailFull < head.scan.retainedByteCount) != exceedsPrefix || full <= tailFull {
				t.Fatalf("mixed control has wrong geometry: c=%d prefix=%d d=%d exceeds=%t", head.scan.retainedByteCount, tailFull, full, exceedsPrefix)
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
					t.Fatal("mixed control did not commit actual later ACK originals")
				}
				owners[item.sequenceNumber] = item
				f.feedback()
			}
			privateProviderAssertRepairFixtureCustody(t, f.sequence, repairMetadata, 10)
			if f.sequence.holdPromise.reservedByteCount != tailFull || f.sequence.nextSequenceNumber != first ||
				f.tcp.returnByteCount != retained || len(f.entered) != 0 || f.ackedCount != 1 ||
				pressure && f.rx.Available() != 0 || f.recovery.Available() < full || f.root.Available() < full {
				t.Fatal("mixed control did not preserve the exact gap with independently available recovery capacity")
			}
			before := f.receiver.ReceiveStats()
			f.forward(head.windowRoundFrame, f.receiverIn)
			attempts := 1
			for len(f.entered) == 0 && attempts < 4 {
				if f.sequence.ctx.Err() != nil || f.ackedCount != 1 || f.deliveredCount != 1 || f.tcp.returnByteCount != retained {
					t.Fatal("mixed head refusal changed live sequence, ACK truth or return custody")
				}
				for number, owner := range owners {
					if f.sequence.receiveQueue.GetBySequenceNumber(number) != owner || !owner.committed {
						t.Fatal("mixed head refusal evicted an actual committed original")
					}
				}
				retry := privateProviderMixedHeadRetry(t, f, head)
				f.forward(retry.windowRoundFrame, f.receiverIn)
				attempts++
			}
			if len(f.entered) == 0 {
				t.Fatalf("valid mixed reliable head remained stranded after %d actual source attempts: pressure=%t c=%d prefix=%d d=%d rx=%d child=%d root_spare=%d retained_chunk=%d", attempts, pressure, head.scan.retainedByteCount, tailFull, full, f.rx.UsedByteCount(), f.recovery.UsedByteCount(), f.root.Available(), f.tcp.returnByteCount)
			}
			if f.tcp.returnByteCount != retained || f.ackedCount != 1 || f.ackCounts[first] != 0 ||
				pressure && f.recovery.UsedByteCount() <= 0 {
				t.Fatal("mixed head crossed the NAT barrier without exact paid custody or truthful ACK state")
			}
			close(f.gate)
			synctest.Wait()
			f.feedback()
			if f.ackedCount != 12 || f.deliveredCount != 14 || f.ackCounts[first] != 1 ||
				f.sequence.nextSequenceNumber != first+11 || f.sequence.receiveQueue.Len() != 0 ||
				f.rx.UsedByteCount() != 0 || f.recovery.UsedByteCount() != 0 || f.tcp.returnByteCount != 0 ||
				f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
				f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
				f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
				t.Fatal("valid mixed head did not drain all actual reliable owners and the retained return chunk")
			}
			f.close()
			t.Logf("[provider-mixed-reliable] pressure=%t tcp_frames=2 udp_frames=1 udp_payload=%d head_c=%d prefix=%d head_d=%d attempts=%d callbacks=12 delivered=14 root=0 rx=0 child=0 nat=0 joined=true", pressure, udpPayload, head.scan.retainedByteCount, tailFull, full, attempts)
		})
	}
}

// C can borrow the existing promise while actual TCP downstream D needs the
// child. A valid UDP member cannot erase the TCP ACK's recovery requirement.
func TestPrivateProviderMixedReliableHeadFitsPromise(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderMixedReliableHead(t, 12, false)
}

// The same legitimate mixed Pack may need the child before decoding when one
// UDP payload makes C larger than the estimate derived from smaller ACK tails.
func TestPrivateProviderMixedReliableHeadExceedsPromise(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderMixedReliableHead(t, 1252, true)
}
