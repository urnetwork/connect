// Receiver timing remains optional, bounded and independent of decoded owners.
package connect

import (
	"math"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Both wire decoders preserve absent versus immediate feedback, and the
// compact handoff retains its value after the pooled decoder is reused.
func TestAckReceiverDelayCodecPreservesPresenceAndLifetime(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, present := range []bool{false, true} {
		for _, micros := range []uint32{0, 1, 127, 128, 16383, 16384, math.MaxUint32} {
			frame := sendAckFrame{
				messageId: NewId(), sequenceId: NewId(), tagSet: true, tagSendTime: 1,
				receiverAckDelaySet: present, receiverAckDelayMicros: micros,
			}
			assertAckCodecMatches(t, &frame)
			encoded := marshalSendAckTransferFrame(&frame)
			var decoded protocol.TransferFrame
			if !unmarshalTransferFrame(encoded, &decoded, true) {
				t.Fatal("compatibility decoder rejected receiver timing")
			}
			if got := decoded.GetAck(); got == nil || (got.ReceiverAckDelayMicros != nil) != present ||
				present && got.GetReceiverAckDelayMicros() != micros {
				t.Fatalf("present=%t micros=%d: compatibility decoder changed timing", present, micros)
			}
			owner := inboundDecodedTransferFrames.take()
			if !unmarshalOwnedTransferFrame(encoded, owner, true) {
				t.Fatal("owned decoder rejected receiver timing")
			}
			compact, err := receiveAckMessageFromProtocol(owner.frame.Ack)
			// Model immediate owner reuse without depending on pool shard order.
			owner.ackReceiverDelayMicros = micros ^ math.MaxUint32
			inboundDecodedTransferFrames.put(owner)
			MessagePoolReturn(encoded)
			if err != nil || compact.receiverAckDelaySet != present ||
				present && compact.receiverAckDelayMicros != micros {
				t.Fatalf("present=%t micros=%d: compact timing changed after owner reuse", present, micros)
			}
		}
	}
}

// A descriptor with the pre-extension Ack fields models a protobuf legacy
// receiver. It still obtains the same cumulative/selective delivery evidence.
func TestAckReceiverDelayIsOptionalForLegacyPeer(t *testing.T) {
	file := protodesc.ToFileDescriptorProto(protocol.File_transfer_proto)
	for _, message := range file.MessageType {
		if message.GetName() != "Ack" {
			continue
		}
		removedOneof := int32(-1)
		fields := message.Field[:0]
		for _, field := range message.Field {
			if field.GetNumber() == 12 {
				removedOneof = field.GetOneofIndex()
				continue
			}
			fields = append(fields, field)
		}
		message.Field = fields
		if removedOneof < 0 {
			t.Fatal("receiver timing field is missing from the current descriptor")
		}
		message.OneofDecl = append(message.OneofDecl[:removedOneof], message.OneofDecl[removedOneof+1:]...)
		for _, field := range message.Field {
			if field.OneofIndex != nil && *field.OneofIndex > removedOneof {
				field.OneofIndex = proto.Int32(*field.OneofIndex - 1)
			}
		}
	}
	legacyFile, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	legacyAck := legacyFile.Messages().ByName("Ack")
	if legacyAck.Fields().ByNumber(12) != nil {
		t.Fatal("legacy descriptor still recognizes receiver timing")
	}
	for _, selective := range []bool{false, true} {
		frame := sendAckFrame{
			messageId: NewId(), sequenceId: NewId(), selective: selective,
			tagSet: true, tagSendTime: 42,
			receiverAckDelaySet: true, receiverAckDelayMicros: math.MaxUint32,
		}
		wire := frame.appendAck(nil)
		legacy := dynamicpb.NewMessage(legacyAck)
		if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(wire, legacy); err != nil {
			t.Fatal(err)
		}
		frame.receiverAckDelaySet = false
		want := dynamicpb.NewMessage(legacyAck)
		if err := proto.Unmarshal(frame.appendAck(nil), want); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(legacy, want) {
			t.Fatalf("selective=%t: optional timing changed legacy delivery evidence", selective)
		}
	}
}

// Invalid timing cannot turn a truncated or wrongly typed Ack into valid
// feedback. A duplicate optional scalar keeps protobuf's last-value rule.
func TestAckReceiverDelayRejectsMalformedAndPreservesLastValue(t *testing.T) {
	frame := sendAckFrame{messageId: NewId(), sequenceId: NewId(), receiverAckDelaySet: true, receiverAckDelayMicros: 100}
	base := frame.appendAck(nil)
	for _, suffix := range [][]byte{
		append(protowire.AppendTag(nil, 12, protowire.VarintType), 0x80),
		protowire.AppendBytes(protowire.AppendTag(nil, 12, protowire.BytesType), []byte{1}),
	} {
		wire := append(append([]byte{}, base...), suffix...)
		if _, ok := decodeAck(wire); ok {
			t.Fatal("compatibility decoder accepted invalid receiver timing")
		}
		owner := inboundDecodedTransferFrames.take()
		accepted := decodeAckOwned(wire, owner)
		inboundDecodedTransferFrames.put(owner)
		if accepted {
			t.Fatal("owned decoder accepted invalid receiver timing")
		}
	}
	wire := protowire.AppendVarint(protowire.AppendTag(base, 12, protowire.VarintType), 0)
	ack, ok := decodeAck(wire)
	if !ok || ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != 0 {
		t.Fatal("duplicate timing field did not preserve explicit final zero")
	}
	owner := inboundDecodedTransferFrames.take()
	defer inboundDecodedTransferFrames.put(owner)
	if !decodeAckOwned(wire, owner) || owner.ack.ReceiverAckDelayMicros == nil || *owner.ack.ReceiverAckDelayMicros != 0 {
		t.Fatal("owned duplicate timing field did not preserve explicit final zero")
	}
}

// Maximal timing and existing varints fit the unchanged per-entry reservation,
// including legacy framing and the encrypted carrier's complete outer frame.
func TestAckReceiverDelayFitsEncodedEntryReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, legacy := range []bool{false, true} {
		for _, selective := range []bool{false, true} {
			missing := NewId()
			frame := sendAckFrame{
				path:      TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()},
				messageId: NewId(), sequenceId: NewId(), selective: selective,
				tagSet: true, tagSendTime: math.MaxUint64, missingContractId: &missing,
				compactContractRecovery: true, logicalLaneVersion: math.MaxUint32,
				receiveWindowSet: true, receiveWindowByteCount: math.MaxUint64,
				ackCompressTimeoutSet: true, ackCompressTimeoutMicros: math.MaxUint32,
				receiverAckDelaySet: true, receiverAckDelayMicros: math.MaxUint32,
				contractAhead: true,
			}
			wire := marshalSendAckTransferFrame(&frame)
			if legacy {
				MessagePoolReturn(wire)
				ackBytes, err := proto.Marshal(buildEquivalentAckFrame(&frame).Ack)
				if err != nil {
					t.Fatal(err)
				}
				wire, err = ProtoMarshal(&protocol.TransferFrame{
					TransferPath: frame.path.ToProtobuf(),
					Frame:        &protocol.Frame{MessageType: protocol.MessageType_TransferAck, MessageBytes: ackBytes},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			plainSize := len(wire)
			wrapped, err := newFrameCodecTestSequenceCipher(t).SealOuterFrame(frame.path, wire, protocol.SequenceRole_SequenceRoleServer, true)
			MessagePoolReturn(wire)
			if err != nil {
				t.Fatal(err)
			}
			wrappedSize := len(wrapped)
			MessagePoolReturn(wrapped)
			if plainSize > ackResponseEntryMaxByteCount || wrappedSize > ackResponseEntryMaxByteCount {
				t.Fatalf("legacy=%t selective=%t: encoded entry %d/%d exceeds %d", legacy, selective, plainSize, wrappedSize, ackResponseEntryMaxByteCount)
			}
		}
	}
}
