// Private fixture-only gateway evidence. ACKs and the real provider's initial
// identity publication have different ownership; neither may impersonate the other.
package connect

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

type receiptGatewayAckId struct {
	sequenceId Id
	messageId  Id
}

// One serial consumer owns this evidence. Expected ACK identities are registered
// before receive admission; the benchmark freezes them before its collector starts.
type receiptGatewayEvidence struct {
	expectedAcks         map[receiptGatewayAckId]bool
	ackPackets           int
	diagnosticSequenceId Id
	diagnosticMessageId  Id
	diagnosticPackets    int
}

// Registration describes offered wire identity, not delivery or acceptance.
func (self *receiptGatewayEvidence) expectAck(sequenceId, messageId Id) {
	if self.expectedAcks == nil {
		self.expectedAcks = map[receiptGatewayAckId]bool{}
	}
	self.expectedAcks[receiptGatewayAckId{sequenceId: sequenceId, messageId: messageId}] = true
}

// Read the final configured Pack, including NoAck, before actual admission.
// Wire ACKs route by peer plus SequenceId; LogicalLaneVersion is capability,
// not a lane number, and the Ack protocol has no logical_lane field.
func (self *receiptGatewayEvidence) expectPack(pack *protocol.Pack) error {
	if pack == nil {
		return fmt.Errorf("missing offered Pack")
	}
	if pack.Nack {
		return nil
	}
	sequenceId, sequenceErr := IdFromBytes(pack.SequenceId)
	messageId, messageErr := IdFromBytes(pack.MessageId)
	if sequenceErr != nil || messageErr != nil || sequenceId == (Id{}) || messageId == (Id{}) {
		return fmt.Errorf("malformed offered ACK identity")
	}
	self.expectAck(sequenceId, messageId)
	return nil
}

// Borrows packet; proto decoding owns its ordinary fields. The caller returns
// the pooled wire root on every outcome. Only a validated ACK is returned;
// the exact initial provider diagnostic is recorded separately, never ACKed here.
func (self *receiptGatewayEvidence) consume(packet []byte, clientId, peerId Id, provider *RemoteUserNatProvider) (*protocol.Ack, error) {
	frame := &protocol.TransferFrame{}
	if err := proto.Unmarshal(packet, frame); err != nil {
		return nil, fmt.Errorf("gateway outer protobuf (%d bytes): %w", len(packet), err)
	}
	unexpected := func(reason string) (*protocol.Ack, error) {
		var types []protocol.MessageType
		if frame.Pack != nil {
			for _, inner := range frame.Pack.Frames {
				types = append(types, inner.GetMessageType())
			}
		}
		return nil, fmt.Errorf("unexpected gateway envelope: %s; bytes=%d ack=%t pack=%t legacy=%t encrypted=%d inner-types=%v",
			reason, len(packet), frame.Ack != nil, frame.Pack != nil, frame.Frame != nil, len(frame.EncryptedTransferFrame), types)
	}
	if frame.TransferPath == nil {
		return unexpected("missing path")
	}
	path, err := TransferPathFromProtobuf(frame.TransferPath)
	if err != nil || path != sendTransferPath(clientId, DestinationId(peerId)) {
		return unexpected("foreign or malformed path")
	}
	ack, pack := frame.Ack, frame.Pack
	if len(frame.EncryptedTransferFrame) != 0 || ack != nil && pack != nil ||
		frame.Frame != nil && (ack != nil || pack != nil) {
		return unexpected("encrypted or mixed bodies")
	}
	if legacy := frame.Frame; legacy != nil {
		if legacy.Raw {
			return unexpected("raw legacy envelope")
		}
		switch legacy.MessageType {
		case protocol.MessageType_TransferAck:
			ack = &protocol.Ack{}
			if err := proto.Unmarshal(legacy.MessageBytes, ack); err != nil {
				return unexpected("malformed legacy ACK")
			}
		case protocol.MessageType_TransferPack:
			pack = &protocol.Pack{}
			if err := proto.Unmarshal(legacy.MessageBytes, pack); err != nil {
				return unexpected("malformed legacy Pack")
			}
		default:
			return unexpected("unknown legacy type")
		}
	}
	if ack != nil {
		if frame.MessageType != nil && *frame.MessageType != protocol.MessageType_TransferAck {
			return unexpected("outer type is not ACK")
		}
		sequenceId, sequenceErr := IdFromBytes(ack.SequenceId)
		messageId, messageErr := IdFromBytes(ack.MessageId)
		if sequenceErr != nil || messageErr != nil || sequenceId == (Id{}) || messageId == (Id{}) ||
			len(ack.MissingContractId) != 0 ||
			!self.expectedAcks[receiptGatewayAckId{sequenceId: sequenceId, messageId: messageId}] {
			return unexpected("unknown, malformed or contract-missing ACK identity")
		}
		self.ackPackets++
		return ack, nil
	}
	if pack == nil || provider == nil {
		return unexpected("not an ACK or expected provider publication")
	}
	if frame.MessageType != nil && *frame.MessageType != protocol.MessageType_TransferPack {
		return unexpected("outer type is not Pack")
	}
	sequenceId, sequenceErr := IdFromBytes(pack.SequenceId)
	messageId, messageErr := IdFromBytes(pack.MessageId)
	if sequenceErr != nil || messageErr != nil || sequenceId == (Id{}) || messageId == (Id{}) ||
		!pack.Head || pack.SequenceNumber != 0 || pack.Nack || pack.ContractFrame != nil ||
		len(pack.ContractId) != 0 || pack.ContractAhead || pack.ForceStream ||
		pack.CompanionContract || pack.LogicalLane != 0 || len(pack.Frames) != 1 {
		return unexpected("not the exact no-contract initial diagnostic Pack")
	}
	inner := pack.Frames[0]
	if inner == nil || inner.Raw || inner.MessageType != protocol.MessageType_IpIpProviderDiagnostics {
		return unexpected("not a typed provider diagnostic")
	}
	diagnostic := &protocol.IpProviderDiagnostics{}
	if err := proto.Unmarshal(inner.MessageBytes, diagnostic); err != nil {
		return unexpected("malformed provider diagnostic")
	}
	expected := &protocol.IpProviderDiagnostics{BuildVersion: provider.buildVersion,
		SecurityPolicyHash: provider.securityPolicyHash, Sequence: 1}
	if provider.securityPolicyGeneration != 0 {
		expected.SecurityPolicyGeneration = proto.Uint64(provider.securityPolicyGeneration)
	}
	if !proto.Equal(diagnostic, expected) {
		return unexpected("diagnostic identity, generation or counters changed")
	}
	if self.diagnosticPackets != 0 &&
		(self.diagnosticSequenceId != sequenceId || self.diagnosticMessageId != messageId) {
		return unexpected("diagnostic retry changed its exact sender identity")
	}
	self.diagnosticSequenceId, self.diagnosticMessageId = sequenceId, messageId
	self.diagnosticPackets++
	return nil, nil
}

// Both supported ACK encodings preserve the offered identity and never consume
// the borrowed pooled root. This exercises the actual native ACK serializer.
func TestReceiveReceiptGatewayDecoderNativeAndLegacyAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	clientId, peerId, sequenceId, messageId := NewId(), NewId(), NewId(), NewId()
	path := sendTransferPath(clientId, DestinationId(peerId))
	want := &protocol.Ack{SequenceId: sequenceId.Bytes(), MessageId: messageId.Bytes(), Selective: true}
	for _, legacy := range []bool{false, true} {
		var packet []byte
		if legacy {
			body, err := proto.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			packet, err = ProtoMarshal(&protocol.TransferFrame{TransferPath: path.ToProtobuf(),
				Frame: &protocol.Frame{MessageType: protocol.MessageType_TransferAck, MessageBytes: body}})
			if err != nil {
				t.Fatal(err)
			}
		} else {
			packet = marshalSendAckTransferFrame(&sendAckFrame{path: path, sequenceId: sequenceId,
				messageId: messageId, selective: true})
		}
		before := bytes.Clone(packet)
		witness := MessagePoolShareReadOnly(packet)
		evidence := &receiptGatewayEvidence{}
		evidence.expectAck(sequenceId, messageId)
		ack, err := evidence.consume(packet, clientId, peerId, nil)
		unchanged := bytes.Equal(packet, before)
		returnedEarly := MessagePoolReturn(packet)
		returnedFinal := MessagePoolReturn(witness)
		if err != nil || !proto.Equal(ack, want) || evidence.diagnosticPackets != 0 ||
			!unchanged || returnedEarly || !returnedFinal {
			t.Fatalf("ACK decoder changed identity or borrowed custody: legacy=%t err=%v", legacy, err)
		}
	}
}

// One typed diagnostic may retransmit without manufacturing delivery evidence.
// A changed sender identity is not quietly accepted as the same publication.
func TestReceiveReceiptGatewayDecoderDiagnosticsAndRetries(t *testing.T) {
	clientId, peerId, sequenceId, messageId := NewId(), NewId(), NewId(), NewId()
	provider := &RemoteUserNatProvider{buildVersion: "synthetic-provider", securityPolicyHash: "synthetic-policy"}
	body, err := proto.Marshal(&protocol.IpProviderDiagnostics{BuildVersion: provider.buildVersion,
		SecurityPolicyHash: provider.securityPolicyHash, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame := &protocol.TransferFrame{TransferPath: sendTransferPath(clientId, DestinationId(peerId)).ToProtobuf(),
		Pack: &protocol.Pack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), Head: true,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpProviderDiagnostics, MessageBytes: body}}}}
	evidence := &receiptGatewayEvidence{}
	for index := 0; index < 2; index++ {
		packet, err := proto.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		ack, err := evidence.consume(packet, clientId, peerId, provider)
		if err != nil || ack != nil || evidence.diagnosticPackets != index+1 ||
			evidence.diagnosticMessageId != messageId || evidence.diagnosticSequenceId != sequenceId {
			t.Fatalf("typed diagnostic was not separately retained as evidence: %v", err)
		}
	}
	frame.Pack.MessageId = NewId().Bytes()
	packet, err := proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := evidence.consume(packet, clientId, peerId, provider); err == nil || ack != nil || evidence.diagnosticPackets != 2 {
		t.Fatal("changed diagnostic sender identity was silently accepted")
	}
}

// Decoding an outer protobuf successfully is insufficient. Reject unknown,
// mixed, foreign, malformed and spoofed diagnostic envelopes without evidence.
func TestReceiveReceiptGatewayDecoderRejectsForeignAndMalformedBodies(t *testing.T) {
	clientId, peerId, sequenceId, messageId := NewId(), NewId(), NewId(), NewId()
	path := sendTransferPath(clientId, DestinationId(peerId)).ToProtobuf()
	ack := &protocol.Ack{SequenceId: sequenceId.Bytes(), MessageId: messageId.Bytes()}
	provider := &RemoteUserNatProvider{buildVersion: "synthetic-provider", securityPolicyHash: "synthetic-policy"}
	wrongDiagnostic, err := proto.Marshal(&protocol.IpProviderDiagnostics{
		BuildVersion: provider.buildVersion, SecurityPolicyHash: "foreign-synthetic-policy", Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	frames := []*protocol.TransferFrame{
		{TransferPath: path},
		{TransferPath: path, Ack: ack, Pack: &protocol.Pack{}},
		{TransferPath: path, Ack: ack, EncryptedTransferFrame: []byte{1}},
		{TransferPath: path, Ack: ack, Frame: &protocol.Frame{}},
		{TransferPath: sendTransferPath(NewId(), DestinationId(peerId)).ToProtobuf(), Ack: ack},
		{Ack: ack},
		{TransferPath: path, Ack: &protocol.Ack{SequenceId: NewId().Bytes(), MessageId: messageId.Bytes()}},
		{TransferPath: path, Ack: &protocol.Ack{SequenceId: []byte{1}, MessageId: messageId.Bytes()}},
		{TransferPath: path, Frame: &protocol.Frame{MessageType: protocol.MessageType_TransferAck, MessageBytes: []byte{0x80}}},
		{TransferPath: path, Frame: &protocol.Frame{MessageType: protocol.MessageType_TransferControlPing}},
		{TransferPath: path, Pack: &protocol.Pack{SequenceId: sequenceId.Bytes(), MessageId: messageId.Bytes(), Head: true,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPing}}}},
		{TransferPath: path, Pack: &protocol.Pack{SequenceId: sequenceId.Bytes(), MessageId: messageId.Bytes(), Head: true,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpProviderDiagnostics, MessageBytes: wrongDiagnostic}}}},
	}
	for index, frame := range frames {
		packet, err := proto.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		evidence := &receiptGatewayEvidence{}
		evidence.expectAck(sequenceId, messageId)
		if got, err := evidence.consume(packet, clientId, peerId, provider); err == nil || got != nil || evidence.diagnosticPackets != 0 {
			t.Fatalf("unexpected envelope %d escaped the strict evidence boundary", index)
		}
	}
	evidence := &receiptGatewayEvidence{}
	if got, err := evidence.consume([]byte{0x80}, clientId, peerId, provider); err == nil || got != nil {
		t.Fatal("malformed outer wire bytes were accepted")
	}
}

// The configured NoAck Pack never registers delivery authority. An ACK for
// another sequence is foreign even when it copies an offered message id.
func TestReceiveReceiptGatewayDecoderRejectsNoAckAndUnofferedIdentity(t *testing.T) {
	clientId, peerId, sequenceId, messageId := NewId(), NewId(), NewId(), NewId()
	pack := &protocol.Pack{SequenceId: sequenceId.Bytes(), MessageId: messageId.Bytes(), Nack: true}
	evidence := &receiptGatewayEvidence{}
	if err := evidence.expectPack(pack); err != nil {
		t.Fatal(err)
	}
	frame := &protocol.TransferFrame{TransferPath: sendTransferPath(clientId, DestinationId(peerId)).ToProtobuf(),
		Ack: &protocol.Ack{SequenceId: sequenceId.Bytes(), MessageId: messageId.Bytes()}}
	packet, err := proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := evidence.consume(packet, clientId, peerId, nil); err == nil || ack != nil || evidence.ackPackets != 0 {
		t.Fatal("NoAck offer acquired false wire delivery authority")
	}
	pack.Nack = false
	if err := evidence.expectPack(pack); err != nil {
		t.Fatal(err)
	}
	if ack, err := evidence.consume(packet, clientId, peerId, nil); err != nil || ack == nil || evidence.ackPackets != 1 {
		t.Fatalf("actual ACK-required offer lost its exact identity: %v", err)
	}
	frame.Ack.SequenceId = NewId().Bytes()
	packet, err = proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := evidence.consume(packet, clientId, peerId, nil); err == nil || ack != nil || evidence.ackPackets != 1 {
		t.Fatal("unoffered sibling sequence borrowed another sequence's ACK identity")
	}
}
