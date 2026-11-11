// Public SendMultiWithTimeout keeps a mixed TCP+UDP DATA Pack byte-budgeted
// and ordered. Its later pure ACK originals use actual IP immediate policy,
// releasing retained TCP return data even while the DATA head is refused.
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
			if !frame.encrypted || frame.pack.Nack || frame.pack.DeliverImmediately || protocolOnlyPack(frame.pack) || len(frame.pack.Frames) != 3 || frame.scan.providerTcpFrameCount != 2 {
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

// Exercise the ordinary and full-DATA cases without turning the genuine ACK
// tails into DATA. The separate pressure owner's actual return permits the
// unchanged mixed original's natural retry; no ceiling is raised on retry.
func testPrivateProviderMixedReliableHead(t *testing.T, udpPayload int) {
	t.Helper()
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
			if !f.tcp.retainReturnChunk(bytes.Repeat([]byte{0x49}, 1280), 500, false) {
				t.Fatal("mixed provider fixture could not retain an actual return chunk")
			}
			head := privateProviderMixedAckOriginal(t, f, udpPayload, end)
			probe := &receiveItem{transferItem: transferItem{queueByteCount: head.scan.retainedByteCount}, ack: true, frames: head.pack.Frames}
			if protocolOnlyPack(head.pack) || immediateIpAckFrames(head.pack.Frames) ||
				!f.sequence.prepareReceiveDeliveryCredit(probe) || !probe.deliveryPrepared ||
				probe.deliveryPrepaid <= 0 || probe.QueueByteCount() <= head.scan.retainedByteCount {
				t.Fatal("mixed public DATA Pack did not retain its ordinary full downstream accounting")
			}
			privateProviderDataHeadWithProtocolTails(t, f, head, end, pressure, func() *privateProviderEncryptedAckFrame {
				return privateProviderMixedHeadRetry(t, f, head)
			})
		})
	}
}

// Retain the historical small mixed-head root while testing current policy:
// its genuine ACK tails progress independently of its ordinary DATA admission.
func TestPrivateProviderMixedReliableHeadFitsPromise(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderMixedReliableHead(t, 12)
}

// The maximum-MTU UDP member stays DATA, including its larger encoded owner.
// The pure ACK tails neither borrow its memory nor falsely acknowledge it.
func TestPrivateProviderMixedReliableHeadExceedsPromise(t *testing.T) {
	assertMessagePoolOwnership(t)
	testPrivateProviderMixedReliableHead(t, 1252)
}
