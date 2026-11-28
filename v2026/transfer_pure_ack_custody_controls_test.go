// Reliable immediate options alone do not make TCP lifecycle or DATA independent feedback.
package connect

import (
	"bytes"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Even an explicitly immediate lifecycle control must precede its next DATA.
func TestPureTcpAckCustodyKeepsSynFinResetBarriers(t *testing.T) {
	for _, flags := range []byte{tcpFlagSyn | tcpFlagAck, tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
		testPureAckCustodyBarrier(t, flags, nil, false)
	}
}

// An explicitly immediate payload remains DATA, preserving its ordinary order.
func TestPureTcpAckCustodyKeepsPayloadBarrier(t *testing.T) {
	testPureAckCustodyBarrier(t, tcpFlagAck, []byte{2}, false)
}

// An immediate mixed ACK plus DATA group cannot inherit its first frame's exemption.
func TestPureTcpAckCustodyKeepsMixedGroupBarrier(t *testing.T) {
	testPureAckCustodyBarrier(t, tcpFlagAck, []byte{2}, true)
}

// A one-slot control isolates the same custody predicate as the default-limit root.
// Both directions use real workers, wire identities and receiver-created receipts.
func testPureAckCustodyBarrier(t *testing.T, barrierFlags byte, barrierPayload []byte, mixed bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newWindowRoundFixture(t, func(s *SendBufferSettings) { s.SequenceBufferSize = 1 }, nil, TransportTypeH1)
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 11).To4(), SourcePort: 41001,
			DestinationIp: net.IPv4(198, 51, 100, 11).To4(), DestinationPort: 443}
		const next = uint32(1014)
		offer := func(flags byte, payload []byte, sequence uint32, immediate, mixedGroup bool) {
			t.Helper()
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, sequence, payload))
			actual, _, err := ParseIpPathWithPayload(packet)
			if err != nil || actual == nil {
				MessagePoolReturn(packet)
				t.Fatal("control TCP packet failed parsing")
			}
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			options := []any{scheduleIpFlow(actual)}
			if immediate {
				options = append(options, NoBudget(), DeliverImmediately())
			}
			frames := []*protocol.Frame{frame}
			if mixedGroup {
				control := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, sequence, nil))
				controlFrame, controlErr := ipPacketFromProviderFrame(control, 2)
				if controlErr != nil {
					MessagePoolReturn(control)
					MessagePoolReturn(packet)
					t.Fatal(controlErr)
				}
				frames = []*protocol.Frame{controlFrame, frame}
			}
			if !f.sender.SendMultiWithTimeout(frames, f.receiver.ClientId(), nil, time.Second, options...) {
				for _, refused := range frames {
					MessagePoolReturn(refused.MessageBytes)
				}
				t.Fatal("actual control group was not admitted")
			}
		}
		offer(tcpFlagAck, []byte("synthetic hdr"), next-13, false, false)
		f.forward(f.take(f.senderOut), f.receiverIn)
		f.acknowledge()
		sequence := f.sequence()
		offer(tcpFlagAck, nil, next, true, false)
		original := f.take(f.senderOut)
		f.forward(original, f.receiverIn)
		receipts := takePureAckCustodyReceipts(t, f, original)
		held := receipts[0]
		receiver := f.receiveSequence()
		if receiver.nextSequenceNumber != original.pack.SequenceNumber+1 || f.deliveredCount != 2 || sequence.immediateSendCount != 1 {
			t.Fatalf("control immediate custody premise: receiver_floor=%d want=%d delivered=%d want=2 custody=%d want=1 responses=%d",
				receiver.nextSequenceNumber, original.pack.SequenceNumber+1, f.deliveredCount, sequence.immediateSendCount, len(receipts))
		}
		offer(barrierFlags, barrierPayload, next, true, mixed)
		synctest.Wait()
		sequence.protocolAdmission.mutex.Lock()
		barrierPending := sequence.protocolAdmission.count
		sequence.protocolAdmission.mutex.Unlock()
		if barrierPending != 1 {
			t.Fatal("the actual lifecycle/payload owner did not reach the blocked source")
		}
		started := time.Now()
		markerSequence := next + uint32(len(barrierPayload))
		if barrierFlags&(tcpFlagSyn|tcpFlagFin) != 0 {
			markerSequence++
		}
		offer(tcpFlagAck, []byte{1}, markerSequence, false, false)
		synctest.Wait()
		select {
		case packet := <-f.senderOut:
			MessagePoolReturn(packet)
			t.Fatal("ordered DATA passed an actual earlier TCP lifecycle or payload barrier")
		default:
		}
		if !time.Now().Equal(started) {
			t.Fatal("control advanced virtual time")
		}
		f.forward(held, f.senderIn)
		wire := f.take(f.senderOut)
		expectedFrames := 1
		if mixed {
			expectedFrames++
		}
		if wire.pack == nil || !wire.pack.DeliverImmediately || wire.pack.Nack || len(wire.pack.Frames) != expectedFrames {
			t.Fatal("releasing protocol custody did not publish the actual earlier barrier first")
		}
		message, err := FromFrame(wire.pack.Frames[len(wire.pack.Frames)-1])
		if err != nil {
			t.Fatal(err)
		}
		ip, ok := message.(*protocol.IpPacketFromProvider)
		if !ok {
			t.Fatal("control published another frame type")
		}
		actual, payload, err := ParseIpPathWithPayload(ip.GetIpPacket().GetPacketBytes())
		if err != nil || actual == nil || actual.SequenceNumber != next ||
			!actual.SourceIp.Equal(path.SourceIp) || actual.SourcePort != path.SourcePort ||
			!actual.DestinationIp.Equal(path.DestinationIp) || actual.DestinationPort != path.DestinationPort ||
			actual.Syn != (barrierFlags&tcpFlagSyn != 0) || actual.Fin != (barrierFlags&tcpFlagFin != 0) ||
			actual.Rst != (barrierFlags&tcpFlagRst != 0) || !bytes.Equal(payload, barrierPayload) {
			t.Fatal("a different lifecycle/payload owner passed the earlier queued control")
		}
		f.forward(wire, f.receiverIn)
		f.acknowledge()
	})
}
