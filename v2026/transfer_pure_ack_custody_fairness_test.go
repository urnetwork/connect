// Reliable immediate TCP feedback must not spend an ordered DATA owner's eligibility.
package connect

import (
	"bytes"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The actual default immediate limit is exhausted by peer-delivered pure ACKs.
// Only their wire receipts are held; ordinary DATA has its own eligible owner.
func TestPureTcpAckCustodyDoesNotBlockEligibleOrderedData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultClientSettings()
		limit := settings.SendBufferSettings.SequenceBufferSize
		if limit <= 0 {
			t.Fatal("default sequence limit cannot establish protocol custody")
		}
		f := newWindowRoundFixture(t, func(s *SendBufferSettings) {
			s.SequenceBufferSize = limit
		}, nil, TransportTypeH1)
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 10).To4(), SourcePort: 41000,
			DestinationIp: net.IPv4(198, 51, 100, 10).To4(), DestinationPort: 443}
		const base = uint32(1001)
		prefix := []byte("synthetic hdr")
		next := base + uint32(len(prefix))
		markerDelivered := false
		f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				message, err := FromFrame(frame)
				if err != nil {
					t.Errorf("decode actual IP delivery: %v", err)
					continue
				}
				ip, ok := message.(*protocol.IpPacketFromProvider)
				if !ok {
					continue
				}
				actual, payload, err := ParseIpPathWithPayload(ip.GetIpPacket().GetPacketBytes())
				if err == nil && actual != nil && actual.SequenceNumber == next && bytes.Equal(payload, []byte{1}) {
					markerDelivered = true
				}
			}
		})
		offer := func(flags byte, payload []byte, sequence uint32) {
			t.Helper()
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, sequence, payload))
			actual, actualPayload, err := ParseIpPathWithPayload(packet)
			if err != nil || actual == nil {
				MessagePoolReturn(packet)
				t.Fatal("synthetic TCP frame did not parse")
			}
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			options := multiClientProtocolTransferOpts([]any{scheduleIpFlow(actual)}, []*protocol.Frame{frame})
			resolved := f.sender.resolveSendOptions(options).transferOptions
			pureAck := actual.Ack && !actual.Syn && !actual.Fin && !actual.Rst && len(actualPayload) == 0
			if resolved.NoBudget != pureAck || resolved.DeliverImmediately != pureAck {
				MessagePoolReturn(packet)
				t.Fatal("actual producer policy did not distinguish pure TCP feedback from DATA")
			}
			admitted, err := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), nil, time.Second, options...)
			if !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("actual source handoff: admitted=%t err=%v", admitted, err)
			}
		}
		// A complete real ordinary wire round establishes this exact TCP flow.
		offer(tcpFlagAck, prefix, base)
		warm := f.take(f.senderOut)
		f.forward(warm, f.receiverIn)
		f.acknowledge()
		sequence := f.sequence()
		if sequence.logicalLane != 0 || sequence.immediateSendLimit != limit || sequence.flowIsolation.Load() {
			t.Fatal("fixture changed the default lane, immediate limit or FIFO scheduling policy")
		}
		heldReceipts := make([]*windowRoundFrame, 0, 2*limit)
		receiver := f.receiveSequence()
		for index := range limit {
			offer(tcpFlagAck, nil, next)
			wire := f.take(f.senderOut)
			if wire.pack == nil || !wire.pack.DeliverImmediately || wire.pack.Nack {
				t.Fatal("validated pure ACK lost its reliable immediate wire policy")
			}
			f.forward(wire, f.receiverIn)
			receipts := takePureAckCustodyReceipts(t, f, wire)
			heldReceipts = append(heldReceipts, receipts...)
			if receiver.nextSequenceNumber != wire.pack.SequenceNumber+1 || f.deliveredCount != index+2 ||
				sequence.immediateSendCount != index+1 {
				t.Fatalf("actual immediate custody premise: receiver_floor=%d want=%d delivered=%d want=%d custody=%d want=%d responses=%d",
					receiver.nextSequenceNumber, wire.pack.SequenceNumber+1, f.deliveredCount, index+2,
					sequence.immediateSendCount, index+1, len(receipts))
			}
		}
		if sequence.immediateSendCount != limit {
			t.Fatal("the exact default immediate custody was not filled")
		}
		offer(tcpFlagAck, nil, next)
		synctest.Wait()
		sequence.protocolAdmission.mutex.Lock()
		pendingProtocol := sequence.protocolAdmission.count
		sequence.protocolAdmission.mutex.Unlock()
		if pendingProtocol != 1 || sequence.immediateSendCount != limit {
			t.Fatal("the next admitted feedback owner did not wait behind full immediate custody")
		}
		started := time.Now()
		offer(tcpFlagAck, []byte{1}, next)
		synctest.Wait()
		if !time.Now().Equal(started) || sequence.immediateSendCount != limit ||
			sequence.resendCapacityUnavailable.Load() || sequence.fifoRepair.admission.set ||
			!sequence.flightController.canSendForKey(ipSendSchedulingKey(path)) {
			t.Fatal("ordinary DATA lacks the independent unchanged admission/flight/custody premise")
		}
		// All source workers have reached durable waits. Observe the real wire,
		// without a timeout, timer advance or a scheduler predicate substitution.
		var marker *windowRoundFrame
		select {
		case encoded := <-f.senderOut:
			marker = &windowRoundFrame{bytes: encoded}
			f.heldFrames = append(f.heldFrames, marker)
			var envelope protocol.TransferFrame
			if err := ProtoUnmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			marker.pack = envelope.Pack
			if marker.pack == nil && envelope.Frame != nil && envelope.Frame.MessageType == protocol.MessageType_TransferPack {
				marker.pack = &protocol.Pack{}
				if err := ProtoUnmarshal(envelope.Frame.MessageBytes, marker.pack); err != nil {
					t.Fatal(err)
				}
			}
		default:
			t.Fatal("PURE_TCP_ACK_CUSTODY_BLOCKED_ELIGIBLE_DATA: admitted next-sequence DATA waited for independent pure-feedback custody")
		}
		if marker.pack == nil || marker.pack.DeliverImmediately || marker.pack.Nack {
			t.Fatal("the admitted ordinary DATA marker did not preserve its reliable ordered wire policy")
		}
		f.forward(marker, f.receiverIn)
		if !markerDelivered || sequence.immediateSendCount != limit || !time.Now().Equal(started) {
			t.Fatal("DATA delivery spent a held immediate receipt or advanced virtual time")
		}
		// Withhold the marker's cumulative receipt too: it could otherwise
		// legitimately retire the earlier originals and conceal the bypass.
		f.forward(heldReceipts[0], f.senderIn)
		feedback := f.take(f.senderOut)
		if feedback.pack == nil || !feedback.pack.DeliverImmediately || feedback.pack.Nack {
			t.Fatal("releasing one actual receipt did not resume the queued pure ACK")
		}
		f.forward(feedback, f.receiverIn)
		f.acknowledge()
		synctest.Wait()
		if sequence.immediateSendCount != 0 || !time.Now().Equal(started) {
			t.Fatal("ordinary cumulative completion did not retire every immediate owner")
		}
	})
}

// Captures every response from one settled in-order immediate delivery. Its
// selective terminal receipt may be absorbed by an exact cumulative boundary;
// both forms name this same original, and every wire share remains withheld.
func takePureAckCustodyReceipts(t *testing.T, f *windowRoundFixture, wire *windowRoundFrame) []*windowRoundFrame {
	t.Helper()
	if wire.pack == nil || !wire.pack.DeliverImmediately || wire.pack.Nack {
		t.Fatal("receipt matching requires an actual reliable immediate wire original")
	}
	receipts := f.takeAcknowledgements()
	for _, receipt := range receipts {
		ack := receipt.ack
		if ack == nil || !bytes.Equal(ack.SequenceId, wire.pack.SequenceId) ||
			!bytes.Equal(ack.MessageId, wire.pack.MessageId) || ack.ImmediateSequenceNumber == nil ||
			ack.GetImmediateSequenceNumber() != wire.pack.SequenceNumber ||
			ack.Selective != ack.DeliveredImmediately || len(ack.MissingContractId) != 0 || len(ack.EvictedSequenceNumbers) != 0 {
			t.Fatalf("actual immediate receipt identity/shape: pack_sequence=%x pack_message=%x pack_ordinal=%d ack=%+v responses=%d",
				wire.pack.SequenceId, wire.pack.MessageId, wire.pack.SequenceNumber, ack, len(receipts))
		}
	}
	return receipts
}
