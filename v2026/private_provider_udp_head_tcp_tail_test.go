// A reliable UDP DATA head stays byte-budgeted and ordered. Later genuine
// TCP ACKs use immediate protocol delivery, so its real DATA refusal cannot
// strand the actual TCP return chunk or falsely acknowledge the UDP original.
package connect

import (
	"bytes"
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
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

// Ordinary public delivery and an exact full-DATA refusal use the same real
// UDP original. Immediate TCP ACK tails must retire return custody before the
// refused UDP head's unchanged natural retry can acquire DATA permission.
func TestPrivateProviderReliableUdpHeadReleasesTcpTailChunks(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, pressure := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newPrivateProviderEncryptedAckFixture(t)
			f.nat.settings.UdpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, context.Canceled
			}}
			f.gate = make(chan struct{})
			end := uint32(500 + 1280)
			f.tcp.mutex.Lock()
			f.tcp.receiveSeq, f.tcp.receiveWindowEnd = end, end+65535
			f.tcp.mutex.Unlock()
			if !f.tcp.retainReturnChunk(bytes.Repeat([]byte{0x53}, 1280), 500, false) {
				t.Fatal("UDP-head fixture could not retain the actual TCP return chunk")
			}
			first := f.nextNumber
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
			probe := &receiveItem{transferItem: transferItem{queueByteCount: head.scan.retainedByteCount}, ack: true, frames: head.pack.Frames}
			if protocolOnlyPack(head.pack) || head.pack.DeliverImmediately || immediateIpAckFrames(head.pack.Frames) ||
				!f.sequence.prepareReceiveDeliveryCredit(probe) || probe.deliveryPrepared || probe.QueueByteCount() != head.scan.retainedByteCount {
				t.Fatal("reliable UDP DATA head incorrectly inherited protocol exemption or TCP prepayment")
			}
			privateProviderDataHeadWithProtocolTails(t, f, head, end, pressure, func() *privateProviderEncryptedAckFrame {
				return privateProviderReliableUdpHeadWire(t, f, first, head)
			})
		})
	}
}
