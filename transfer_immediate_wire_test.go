package connect

// Reliable immediate delivery is additive on the wire. These controls exercise
// the real fast/generated codecs and promotion path without sockets or workers.
import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Every decoder must preserve the option and the reliable exact identity.
func immediateWireCheckPack(t *testing.T, body []byte, expected *protocol.Pack) {
	t.Helper()
	var generated protocol.Pack
	if err := proto.Unmarshal(body, &generated); err != nil || !proto.Equal(&generated, expected) {
		t.Fatalf("generated immediate Pack mismatch: error=%v got=%v want=%v", err, &generated, expected)
	}
	decoded, ok := decodePack(body)
	if !ok {
		t.Fatal("fast immediate Pack decode refused valid bytes")
	}
	defer returnDecodedPackMessageBytes(decoded)
	if !proto.Equal(decoded, expected) {
		t.Fatalf("fast immediate Pack mismatch: got=%v want=%v", decoded, expected)
	}
	owner, ok := decodePackOwned(body)
	if !ok {
		t.Fatal("owned immediate Pack decode refused valid bytes")
	}
	defer owner.release()
	if !proto.Equal(&owner.pack, expected) {
		t.Fatalf("owned immediate Pack mismatch: got=%v want=%v", &owner.pack, expected)
	}
}

// Whole-frame v2 and legacy wrapped v1 retain identical Pack semantics.
func TestDeliverImmediatelyPackWireRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, immediate := range []bool{false, true} {
		contractId := NewId()
		manual := &sendPackFrame{
			path:      TransferPath{SourceId: NewId(), DestinationId: NewId()},
			messageId: NewId(), sequenceId: NewId(), sequenceNumber: 73,
			frames:     []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: []byte{0x45, 0, 0, 40}}},
			contractId: &contractId, tagSendTime: 17, logicalLane: 0,
			deliverImmediately: immediate,
		}
		expected := buildEquivalentTransferFrame(manual)
		expected.Pack.DeliverImmediately = immediate
		want, err := proto.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		wire := marshalSendPackTransferFrame(manual)
		if !bytes.Equal(wire, want) || len(wire) != manual.size() {
			MessagePoolReturn(wire)
			t.Fatalf("manual immediate Pack differs from generated wire or size: immediate=%t", immediate)
		}
		// The fast outer decoder deliberately omits deprecated message_type.
		// Generated/manual wire equality above still checks its wire spelling;
		// this expectation preserves every supported path and payload field.
		ownedExpected := proto.Clone(expected).(*protocol.TransferFrame)
		ownedExpected.MessageType = nil
		decoded := inboundDecodedTransferFrames.take()
		if !unmarshalOwnedTransferFrame(wire, decoded, true) || decoded.frame.MessageType != nil || !proto.Equal(&decoded.frame, ownedExpected) {
			inboundDecodedTransferFrames.put(decoded)
			MessagePoolReturn(wire)
			t.Fatalf("owned v2 immediate frame mismatch: immediate=%t", immediate)
		}
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(wire)
		body, err := proto.Marshal(expected.Pack)
		if err != nil {
			t.Fatal(err)
		}
		immediateWireCheckPack(t, body, expected.Pack)
		legacy := &protocol.TransferFrame{TransferPath: manual.path.ToProtobuf(), Frame: &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: body}}
		legacyWire, err := proto.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		var decodedLegacy protocol.TransferFrame
		if err := proto.Unmarshal(legacyWire, &decodedLegacy); err != nil || decodedLegacy.Frame == nil || decodedLegacy.Pack != nil {
			t.Fatalf("legacy wrapped Pack envelope changed: error=%v", err)
		}
		immediateWireCheckPack(t, decodedLegacy.Frame.MessageBytes, expected.Pack)
	}
}

// The old schema sees an ordinary reliable Pack and an ordinary selective ACK.
// Unknown fields remain unknown; no old-parser behavior is invented by clearing
// a new struct field after decoding it.
func immediateWireOldMessage(t *testing.T, name protoreflect.Name) protoreflect.MessageDescriptor {
	t.Helper()
	file := protodesc.ToFileDescriptorProto(protocol.File_transfer_proto)
	for _, message := range file.MessageType {
		if message.GetName() != "Pack" && message.GetName() != "Ack" {
			continue
		}
		fields := message.Field[:0]
		for _, field := range message.Field {
			if message.GetName() == "Pack" && field.GetNumber() == 14 || message.GetName() == "Ack" && field.GetNumber() >= 13 {
				continue
			}
			fields = append(fields, field)
		}
		message.Field = fields
		oneofs := message.OneofDecl[:0]
		for oldIndex, oneof := range message.OneofDecl {
			used := false
			for _, field := range fields {
				if field.OneofIndex != nil && int(field.GetOneofIndex()) == oldIndex {
					used = true
					break
				}
			}
			if !used {
				continue
			}
			newIndex := int32(len(oneofs))
			for _, field := range fields {
				if field.OneofIndex != nil && int(field.GetOneofIndex()) == oldIndex {
					field.OneofIndex = proto.Int32(newIndex)
				}
			}
			oneofs = append(oneofs, oneof)
		}
		message.OneofDecl = oneofs
	}
	descriptor, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	message := descriptor.Messages().ByName(name)
	if message == nil {
		t.Fatalf("old schema lost %s", name)
	}
	return message
}

// A peer that does not know the new fields still preserves reliable ordering.
func TestDeliverImmediatelyUnknownFieldsKeepOldPeerWire(t *testing.T) {
	messageId, sequenceId := NewId(), NewId()
	pack := &protocol.Pack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), SequenceNumber: 19, DeliverImmediately: true}
	body, err := proto.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	oldPack := dynamicpb.NewMessage(immediateWireOldMessage(t, "Pack"))
	if err := proto.Unmarshal(body, oldPack); err != nil {
		t.Fatal(err)
	}
	fields := oldPack.Descriptor().Fields()
	if fields.ByNumber(14) != nil || oldPack.Get(fields.ByName("nack")).Bool() || oldPack.Get(fields.ByName("head")).Bool() || oldPack.Get(fields.ByName("sequence_number")).Uint() != 19 || !bytes.Equal(oldPack.Get(fields.ByName("message_id")).Bytes(), messageId.Bytes()) || len(oldPack.GetUnknown()) == 0 {
		t.Fatal("old Pack decoder interpreted immediate delivery or lost reliable identity")
	}
	ack := &protocol.Ack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), Selective: true, DeliveredImmediately: true, ImmediateSequenceNumber: proto.Uint64(19)}
	body, err = proto.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	oldAck := dynamicpb.NewMessage(immediateWireOldMessage(t, "Ack"))
	if err := proto.Unmarshal(body, oldAck); err != nil {
		t.Fatal(err)
	}
	fields = oldAck.Descriptor().Fields()
	if fields.ByNumber(13) != nil || fields.ByNumber(14) != nil || !oldAck.Get(fields.ByName("selective")).Bool() || !bytes.Equal(oldAck.Get(fields.ByName("message_id")).Bytes(), messageId.Bytes()) || len(oldAck.GetUnknown()) == 0 {
		t.Fatal("old ACK decoder changed an exact selective receipt into cumulative delivery")
	}
}

// Absent and explicit-false fields both retain the ordinary wire defaults.
func TestDeliverImmediatelyOmissionAndFalseDefaults(t *testing.T) {
	assertMessagePoolOwnership(t)
	pack := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), SequenceNumber: 5}
	body, err := proto.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	immediateWireCheckPack(t, body, pack)
	explicitFalse := protowire.AppendTag(append([]byte(nil), body...), 14, protowire.VarintType)
	explicitFalse = protowire.AppendVarint(explicitFalse, 0)
	immediateWireCheckPack(t, explicitFalse, pack)
	ack := &protocol.Ack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), Selective: true}
	body, err = proto.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{body, protowire.AppendVarint(protowire.AppendTag(append([]byte(nil), body...), 13, protowire.VarintType), 0)} {
		decoded, ok := decodeAck(data)
		if !ok || decoded.DeliveredImmediately || !decoded.Selective || !proto.Equal(decoded, ack) {
			t.Fatal("absent/false immediate ACK changed existing selective semantics")
		}
	}
}

// The exact terminal receipt bit survives both generated and hand codecs.
func TestDeliverImmediatelyAckWireRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		immediate bool
		number    uint64
		set       bool
	}{
		{},
		{number: 0, set: true},
		{number: ^uint64(0), set: true},
		{immediate: true, number: 0, set: true},
		{immediate: true, number: 128, set: true},
		{immediate: true, number: ^uint64(0), set: true},
	} {
		manual := &sendAckFrame{path: TransferPath{SourceId: NewId(), DestinationId: NewId()}, messageId: NewId(), sequenceId: NewId(), selective: true, deliveredImmediately: test.immediate, immediateSequenceNumber: test.number, immediateSequenceNumberSet: test.set}
		messageType := protocol.MessageType_TransferAck
		expected := &protocol.TransferFrame{MessageType: &messageType, TransferPath: manual.path.ToProtobuf(), Ack: &protocol.Ack{MessageId: manual.messageId.Bytes(), SequenceId: manual.sequenceId.Bytes(), Selective: true, DeliveredImmediately: test.immediate}}
		if test.set {
			expected.Ack.ImmediateSequenceNumber = proto.Uint64(test.number)
		}
		want, err := proto.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		wire := marshalSendAckTransferFrame(manual)
		if !bytes.Equal(wire, want) || len(wire) != manual.size() {
			MessagePoolReturn(wire)
			t.Fatalf("manual immediate ACK differs from generated wire or size: %+v", test)
		}
		// The fast outer decoder deliberately omits deprecated message_type.
		// Generated/manual wire equality above still checks its wire spelling;
		// this expectation preserves every supported path and payload field.
		ownedExpected := proto.Clone(expected).(*protocol.TransferFrame)
		ownedExpected.MessageType = nil
		decoded := inboundDecodedTransferFrames.take()
		if !unmarshalOwnedTransferFrame(wire, decoded, true) || decoded.frame.MessageType != nil || !proto.Equal(&decoded.frame, ownedExpected) {
			inboundDecodedTransferFrames.put(decoded)
			MessagePoolReturn(wire)
			t.Fatal("owned immediate ACK frame mismatch")
		}
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(wire)
		body, err := proto.Marshal(expected.Ack)
		if err != nil {
			t.Fatal(err)
		}
		ack, ok := decodeAck(body)
		if !ok || !proto.Equal(ack, expected.Ack) {
			t.Fatal("fast immediate ACK body mismatch")
		}
		legacy := &protocol.TransferFrame{TransferPath: manual.path.ToProtobuf(), Frame: &protocol.Frame{MessageType: protocol.MessageType_TransferAck, MessageBytes: body}}
		legacyWire, err := proto.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		var outer protocol.TransferFrame
		if err := proto.Unmarshal(legacyWire, &outer); err != nil || outer.Frame == nil {
			t.Fatalf("legacy ACK envelope decode failed: %v", err)
		}
		var inner protocol.Ack
		if err := proto.Unmarshal(outer.Frame.MessageBytes, &inner); err != nil || !proto.Equal(&inner, expected.Ack) {
			t.Fatalf("legacy ACK body changed terminal selective receipt: %v", err)
		}
	}
}

// Malformed known bool fields are rejected by both fast Pack ownership paths.
func TestDeliverImmediatelyMalformedWireDoesNotAcquireOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	pack := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), SequenceNumber: 3}
	body, err := proto.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range [][]byte{
		protowire.AppendBytes(protowire.AppendTag(nil, 14, protowire.BytesType), []byte{1}),
		append(protowire.AppendTag(nil, 14, protowire.VarintType), 0x80),
	} {
		data := append(append([]byte(nil), body...), suffix...)
		if decoded, ok := decodePack(data); ok {
			returnDecodedPackMessageBytes(decoded)
			t.Fatal("fast Pack decoder accepted malformed immediate field")
		}
		if owner, ok := decodePackOwned(data); ok {
			owner.release()
			t.Fatal("owned Pack decoder accepted malformed immediate field")
		}
	}
}

// A real retry promotion preserves the new option, identity and ACK semantics.
func TestDeliverImmediatelyHeadPromotionPreservesWire(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, immediate := range []bool{false, true} {
			messageId, sequenceId := NewId(), NewId()
			pack := &protocol.Pack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), SequenceNumber: 11, DeliverImmediately: immediate, Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: []byte{0x45, 0, 0, 40}}}}
			outer := &protocol.TransferFrame{TransferPath: (TransferPath{SourceId: NewId(), DestinationId: NewId()}).ToProtobuf()}
			if version == 2 {
				outer.Pack = pack
			} else {
				body, err := proto.Marshal(pack)
				if err != nil {
					t.Fatal(err)
				}
				outer.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: body}
			}
			wire, err := ProtoMarshal(outer)
			if err != nil {
				t.Fatal(err)
			}
			sequence := &SendSequence{log: NewNoopLogger(), rttWindow: &RttWindow{}, sendBufferSettings: &SendBufferSettings{ProtocolVersion: version}}
			item := &sendItem{transferItem: transferItem{messageId: messageId, sequenceNumber: 11}, transferFrameBytes: wire, expectsAck: true, deliverImmediately: immediate}
			promoted, _, err := sequence.setHead(item, true)
			MessagePoolReturn(wire)
			if err != nil {
				t.Fatalf("real head promotion failed: version=%d immediate=%t error=%v", version, immediate, err)
			}
			var got protocol.TransferFrame
			err = proto.Unmarshal(promoted, &got)
			MessagePoolReturn(promoted)
			if err != nil {
				t.Fatal(err)
			}
			gotPack := got.Pack
			if version == 1 {
				gotPack = &protocol.Pack{}
				if got.Frame == nil {
					t.Fatal("v1 promotion lost its Pack envelope")
				}
				if err := proto.Unmarshal(got.Frame.MessageBytes, gotPack); err != nil {
					t.Fatal(err)
				}
			}
			if gotPack == nil || !gotPack.Head || gotPack.Nack || gotPack.DeliverImmediately != immediate || gotPack.SequenceNumber != 11 || !bytes.Equal(gotPack.MessageId, messageId.Bytes()) || !bytes.Equal(gotPack.SequenceId, sequenceId.Bytes()) {
				t.Fatalf("head promotion changed immediate identity/reliability: version=%d immediate=%t got=%v", version, immediate, gotPack)
			}
		}
	}
}

// Source and generation are part of every derived immediate identity. Ordinary
// NewId remains outside this opt-in domain; the helper supplies no authority.
func TestDeliverImmediatelyIdentityBindsSourceSequenceAndOrdinal(t *testing.T) {
	source, otherSource, sequence, otherSequence := NewId(), NewId(), NewId(), NewId()
	for _, number := range []uint64{0, 1, 127, 128, 10000, ^uint64(0)} {
		id := immediateMessageId(source, sequence, number)
		if id != immediateMessageId(source, sequence, number) || !bytes.Equal(id[:6], sequence[:6]) ||
			id == immediateMessageId(otherSource, sequence, number) || id == immediateMessageId(source, otherSequence, number) ||
			id == immediateMessageId(source, sequence, number^1) {
			t.Fatalf("immediate identity lost source/generation/ordinal binding: number=%d", number)
		}
	}
}

// Adjacent terminals compress, but no unseen hole becomes a delivered range.
func TestDeliverImmediatelyTerminalRangesBoundSparseAndLongTrains(t *testing.T) {
	var ranges []immediateOrdinalRange
	for number := uint64(0); number < 10000; number++ {
		var accepted bool
		ranges, accepted = addImmediateOrdinalRange(ranges, number)
		if !accepted || len(ranges) != 1 || ranges[0].first != 0 || ranges[0].last != number || cap(ranges) > immediateTerminalRangeLimit {
			t.Fatalf("contiguous terminal train did not stay bounded: number=%d ranges=%v capacity=%d", number, ranges, cap(ranges))
		}
	}
	ranges = nil
	for index := range immediateTerminalRangeLimit {
		var accepted bool
		ranges, accepted = addImmediateOrdinalRange(ranges, uint64(index)*3)
		if !accepted {
			t.Fatal("sparse terminal capacity was refused before its bound")
		}
	}
	before := slices.Clone(ranges)
	var accepted bool
	ranges, accepted = addImmediateOrdinalRange(ranges, uint64(immediateTerminalRangeLimit)*3)
	if accepted || !slices.Equal(ranges, before) || len(ranges) != immediateTerminalRangeLimit {
		t.Fatal("sparse saturation evicted an identity or grew the range bound")
	}
	ranges, accepted = addImmediateOrdinalRange(ranges, 1)
	if !accepted || len(ranges) != immediateTerminalRangeLimit {
		t.Fatal("adjacent growth unnecessarily refused at range capacity")
	}
	ranges, accepted = addImmediateOrdinalRange(ranges, 2)
	if !accepted || len(ranges) != immediateTerminalRangeLimit-1 || ranges[0] != (immediateOrdinalRange{first: 0, last: 3}) {
		t.Fatal("filling an actual ordinal gap did not merge only its neighbors")
	}
	ranges = nil
	for _, number := range []uint64{^uint64(0), ^uint64(0) - 1, 0, 1} {
		ranges, accepted = addImmediateOrdinalRange(ranges, number)
		if !accepted {
			t.Fatal("terminal range arithmetic refused a valid endpoint")
		}
	}
	if !slices.Equal(ranges, []immediateOrdinalRange{{first: 0, last: 1}, {first: ^uint64(0) - 1, last: ^uint64(0)}}) {
		t.Fatal("terminal ranges wrapped across the uint64 boundary")
	}
}

// Terminal wire authority is selective and has an explicit exact ordinal.
func TestDeliverImmediatelyTerminalAckRejectsMissingAuthority(t *testing.T) {
	messageId, sequenceId := NewId(), NewId()
	for _, invalid := range []*protocol.Ack{
		{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), DeliveredImmediately: true, ImmediateSequenceNumber: proto.Uint64(1)},
		{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), Selective: true, DeliveredImmediately: true},
	} {
		if _, err := receiveAckMessageFromProtocol(invalid); err == nil {
			t.Fatal("malformed terminal ACK gained selective ordinal authority")
		}
	}
	valid := &protocol.Ack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), Selective: true, DeliveredImmediately: true, ImmediateSequenceNumber: proto.Uint64(0)}
	decoded, err := receiveAckMessageFromProtocol(valid)
	if err != nil || !decoded.deliveredImmediately || !decoded.selective || !decoded.immediateSequenceNumberSet || decoded.immediateSequenceNumber != 0 {
		t.Fatalf("present zero ordinal lost exact terminal authority: %v", err)
	}
}

// Cumulative coverage can name an already delivered immediate boundary, but
// its wire flag must remain cumulative even when an older terminal SACK exists.
func TestDeliverImmediatelyCumulativeCompressionKeepsReceiptClass(t *testing.T) {
	for _, restore := range []bool{false, true} {
		messageId, sequenceId := NewId(), NewId()
		terminal := sequenceAck{messageId: messageId, sequenceNumber: 11, selective: true,
			deliveredImmediately: true, immediateSequenceNumber: 11, immediateSequenceNumberSet: true}
		cumulative := sequenceAck{messageId: messageId, sequenceNumber: 11,
			immediateSequenceNumber: 11, immediateSequenceNumberSet: true}
		window := newSequenceAckWindow()
		window.Update(terminal)
		if restore {
			window.Snapshot(true)
		}
		window.Update(cumulative)
		if restore {
			window.Restore([]sequenceAck{terminal})
		}
		snapshot := window.Snapshot(false)
		ack := snapshot.headAck
		if snapshot.ackUpdateCount == 0 || ack.selective || ack.deliveredImmediately ||
			!ack.immediateSequenceNumberSet || ack.immediateSequenceNumber != 11 || ack.messageId != messageId {
			t.Fatalf("cumulative compression inherited terminal-only status: restore=%t ack=%+v", restore, ack)
		}
		wire := &protocol.Ack{MessageId: ack.messageId.Bytes(), SequenceId: sequenceId.Bytes(),
			Selective: ack.selective, DeliveredImmediately: ack.deliveredImmediately,
			ImmediateSequenceNumber: proto.Uint64(ack.immediateSequenceNumber)}
		decoded, err := receiveAckMessageFromProtocol(wire)
		if err != nil || decoded.selective || decoded.deliveredImmediately || !decoded.immediateSequenceNumberSet {
			t.Fatalf("actual ACK parser rejected compressed cumulative authority: restore=%t error=%v", restore, err)
		}
	}
}

// Directly drive the existing sender with real serialized/pooled owners. No
// receive authority is fabricated here; the signed NAT witness covers delivery.
func immediateWireSenderHarness(t *testing.T) (*noAckFastPathHarness, *TransferMemoryBudget) {
	t.Helper()
	return immediateWireSenderHarnessWithRetainedCapacity(t, 0)
}

// A sparse-range control must reach its range bound before an unrelated byte
// bound. Size only that fixture from the real retained reservation formula;
// ordinary controls retain their original 256 KiB capacity.
func immediateWireSenderHarnessWithRetainedCapacity(t *testing.T, retainedItems int) (*noAckFastPathHarness, *TransferMemoryBudget) {
	t.Helper()
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(t.Context())
	var client *Client
	t.Cleanup(func() {
		cancel()
		if client != nil {
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer joinCancel()
			if err := client.CloseAndWait(joinCtx); err != nil {
				t.Error(err)
			}
		}
	})
	harness := newNoAckBudgetHarness(t, ctx)
	client = harness.client
	sequence := harness.sequence
	budgetBytes := ByteCount(256 * 1024)
	if retainedItems > 0 {
		frame := budgetTestFrame(64)
		messageBytes := MessageByteCount([]*protocol.Frame{frame})
		MessagePoolReturn(frame.MessageBytes)
		ownerBytes := (&sendItem{}).retainedMemoryByteCount(
			sequence.retainedSendFrameByteCount(messageBytes, 1), 1,
			sequence.sendBufferSettings.ProtocolVersion < 2)
		budgetBytes = ByteCount(retainedItems) * ownerBytes
		if ownerBytes <= 0 || budgetBytes <= 0 {
			t.Fatal("sparse immediate fixture has no bounded retained capacity")
		}
	}
	budget := NewTransferMemoryBudget(budgetBytes)
	sequence.sendBufferSettings.ResendQueueBudget = budget
	sequence.sendBufferSettings.ResendQueueRetainedByteAccounting = true
	sequence.resendQueue.setBudget(budget, 0)
	sequence.resendQueue.setLifetimeBudget()
	t.Cleanup(func() {
		sequence.releaseRetainedSendItems(context.Canceled)
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("immediate sender retained byte ownership after join: %+v", stats)
		}
	})
	return harness, budget
}

// Capture only immutable scalar identities before any terminal return pools the
// sender item. Completion counters count successful delivery, not close errors.
func immediateWireSend(t *testing.T, harness *noAckFastPathHarness, immediate bool, completed *int) (Id, uint64, ByteCount, ByteCount) {
	t.Helper()
	frame := budgetTestFrame(64)
	messageBytes := MessageByteCount([]*protocol.Frame{frame})
	required := (&sendItem{}).retainedMemoryByteCount(
		harness.sequence.retainedSendFrameByteCount(messageBytes, 1), 1,
		harness.sequence.sendBufferSettings.ProtocolVersion < 2)
	if available := harness.sequence.resendQueue.budget.Available(); available < required {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("immediate sender fixture exhausted byte capacity before range assertion: required=%d available=%d retained=%d", required, available, harness.sequence.resendQueue.Len())
	}
	if !harness.contract.update(messageBytes) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("immediate sender fixture contract debit refused")
	}
	harness.sequence.sendRecord([]*protocol.Frame{frame}, sendAckRecord{deliverImmediately: immediate, callback: func(err error) {
		if err == nil {
			*completed++
		}
	}}, noAckSendRecord{}, true, false)
	pack := harness.takeWrittenPack(t)
	id, err := IdFromBytes(pack.MessageId)
	if err != nil {
		t.Fatal(err)
	}
	item := harness.sequence.resendQueue.GetByMessageId(id)
	if item == nil || !item.expectsAck || item.deliverImmediately != immediate || pack.Nack || pack.DeliverImmediately != immediate || item.memoryBudget == nil {
		t.Fatal("actual immediate wire lost reliable retained ownership")
	}
	if immediate && id != immediateMessageId(harness.client.ClientId(), harness.sequence.sequenceId, item.sequenceNumber) {
		t.Fatal("actual immediate sender did not use its bound identity")
	}
	return id, item.sequenceNumber, item.queueByteCount, messageBytes
}

// Removing a future original must never free its missing predecessor, while
// duplicate terminal receipts must not invoke callbacks or debit twice.
func TestDeliverImmediatelySenderRetiresOnlyExactOriginal(t *testing.T) {
	harness, budget := immediateWireSenderHarness(t)
	sequence := harness.sequence
	ordinaryDone, immediateDone := 0, 0
	ordinaryId, ordinaryNumber, _, ordinaryBytes := immediateWireSend(t, harness, false, &ordinaryDone)
	immediateId, immediateNumber, immediateCharge, immediateBytes := immediateWireSend(t, harness, true, &immediateDone)
	before := budget.UsedByteCount()
	ack := sequenceAck{messageId: immediateId, sequenceNumber: immediateNumber, selective: true, deliveredImmediately: true, immediateSequenceNumber: immediateNumber, immediateSequenceNumberSet: true}
	if !sequence.receiveImmediateAck(ack) || immediateDone != 1 || ordinaryDone != 0 || sequence.resendQueue.GetByMessageId(immediateId) != nil || sequence.resendQueue.GetByMessageId(ordinaryId) == nil || budget.UsedByteCount() != before-immediateCharge || harness.contract.ackedByteCount != immediateBytes || harness.contract.unackedByteCount != ordinaryBytes {
		t.Fatal("terminal receipt did not retire only its exact reliable original")
	}
	if !sequence.receiveImmediateAck(ack) || immediateDone != 1 || ordinaryDone != 0 || budget.UsedByteCount() != before-immediateCharge {
		t.Fatal("duplicate terminal receipt released or invoked ownership twice")
	}
	sequence.receiveAck(ordinaryId, false, sequenceTag{}, false)
	if ordinaryDone != 1 || immediateDone != 1 || len(sequence.sendItems) != 0 || sequence.resendQueue.Len() != 0 || budget.UsedByteCount() != 0 || ordinaryNumber >= immediateNumber {
		t.Fatal("ordinary predecessor failed its independent cumulative retirement")
	}
	sequence.receiveImmediateAck(ack)
	if ordinaryDone != 1 || immediateDone != 1 || harness.contract.ackedByteCount != ordinaryBytes+immediateBytes || harness.contract.unackedByteCount != 0 {
		t.Fatal("late terminal receipt changed callbacks or contract accounting")
	}
}

// A cumulative ACK may name an immediate original whose payload was already
// returned. Keep exact ordinal authority so a lost predecessor ACK still heals.
func TestDeliverImmediatelyCumulativeAckUsesRetiredExactBoundary(t *testing.T) {
	harness, budget := immediateWireSenderHarness(t)
	sequence := harness.sequence
	ordinaryDone, immediateDone := 0, 0
	ordinaryId, _, _, ordinaryBytes := immediateWireSend(t, harness, false, &ordinaryDone)
	immediateId, number, _, immediateBytes := immediateWireSend(t, harness, true, &immediateDone)
	terminal := sequenceAck{messageId: immediateId, sequenceNumber: number, selective: true, deliveredImmediately: true, immediateSequenceNumber: number, immediateSequenceNumberSet: true}
	if !sequence.receiveImmediateAck(terminal) || sequence.resendQueue.GetByMessageId(ordinaryId) == nil {
		t.Fatal("cumulative boundary fixture did not retain its predecessor")
	}
	cumulative := receiveAckMessage{sequenceId: sequence.sequenceId, messageId: immediateId, immediateSequenceNumber: number, immediateSequenceNumberSet: true}
	for _, bad := range []receiveAckMessage{
		{sequenceId: NewId(), messageId: immediateId, immediateSequenceNumber: number, immediateSequenceNumberSet: true},
		{sequenceId: sequence.sequenceId, messageId: immediateMessageId(NewId(), sequence.sequenceId, number), immediateSequenceNumber: number, immediateSequenceNumberSet: true},
		{sequenceId: sequence.sequenceId, messageId: immediateId, immediateSequenceNumber: number + 1, immediateSequenceNumberSet: true},
		{sequenceId: sequence.sequenceId, messageId: immediateId, immediateSequenceNumber: number, immediateSequenceNumberSet: false},
		{sequenceId: sequence.sequenceId, messageId: immediateId, selective: true, immediateSequenceNumber: number, immediateSequenceNumberSet: true},
	} {
		if _, ok := sequence.resolveImmediateAckSequenceNumber(bad); ok {
			t.Fatal("unknown source/generation/ordinal borrowed a retired cumulative boundary")
		}
	}
	window := newSequenceAckWindow()
	sequence.coalesceReceivedAck(window, cumulative)
	snapshot := window.Snapshot(true)
	if snapshot.ackUpdateCount == 0 || snapshot.headAck.sequenceNumber != number || snapshot.headAck.messageId != immediateId {
		t.Fatal("actual ACK coalescer discarded the retired exact boundary")
	}
	sequence.applyAcceptedAckSnapshot(snapshot)
	if ordinaryDone != 1 || immediateDone != 1 || sequence.resendQueue.Len() != 0 || len(sequence.sendItems) != 0 || budget.UsedByteCount() != 0 || harness.contract.ackedByteCount != ordinaryBytes+immediateBytes || harness.contract.unackedByteCount != 0 || len(sequence.immediateTerminals) != 0 {
		t.Fatal("retired boundary failed to release its real preceding original exactly once")
	}
	sequence.receiveImmediateAck(terminal)
	sequence.coalesceReceivedAck(window, cumulative)
	sequence.applyAcceptedAckSnapshot(window.Snapshot(true))
	if ordinaryDone != 1 || immediateDone != 1 || budget.UsedByteCount() != 0 || harness.contract.ackedByteCount != ordinaryBytes+immediateBytes {
		t.Fatal("stale terminal/cumulative duplicate repeated release or debit")
	}
}

// An unflagged original and a mismatched ordinal cannot borrow terminal status.
func TestDeliverImmediatelySenderRejectsWrongOriginalOrOrdinal(t *testing.T) {
	harness, budget := immediateWireSenderHarness(t)
	sequence := harness.sequence
	ordinaryDone, immediateDone := 0, 0
	ordinaryId, ordinaryNumber, _, _ := immediateWireSend(t, harness, false, &ordinaryDone)
	immediateId, immediateNumber, _, _ := immediateWireSend(t, harness, true, &immediateDone)
	before := budget.UsedByteCount()
	for _, bad := range []sequenceAck{
		{messageId: ordinaryId, sequenceNumber: ordinaryNumber, selective: true, deliveredImmediately: true, immediateSequenceNumber: ordinaryNumber, immediateSequenceNumberSet: true},
		{messageId: immediateId, sequenceNumber: immediateNumber, selective: true, deliveredImmediately: true, immediateSequenceNumber: immediateNumber + 1, immediateSequenceNumberSet: true},
		{messageId: immediateId, sequenceNumber: immediateNumber, selective: true, deliveredImmediately: true},
		{messageId: immediateId, sequenceNumber: immediateNumber, deliveredImmediately: true, immediateSequenceNumber: immediateNumber, immediateSequenceNumberSet: true},
	} {
		sequence.receiveImmediateAck(bad)
		if ordinaryDone != 0 || immediateDone != 0 || sequence.resendQueue.Len() != 2 || budget.UsedByteCount() != before || len(sequence.immediateTerminals) != 0 {
			t.Fatal("invalid terminal receipt changed retained ownership")
		}
	}
	sequence.receiveAck(immediateId, false, sequenceTag{}, false)
	if ordinaryDone != 1 || immediateDone != 1 || budget.UsedByteCount() != 0 {
		t.Fatal("ordinary cumulative fallback failed after invalid terminal feedback")
	}
}

// Real alternating originals fill every terminal range. One additional sparse
// receipt falls back to retained SACK rather than silently evicting evidence.
func TestDeliverImmediatelySparseTerminalCapacityKeepsReliableFallback(t *testing.T) {
	// At the last offer there are 33 ordered predecessors plus its immediate
	// original; completed immediate owners from prior pairs have retired.
	harness, budget := immediateWireSenderHarnessWithRetainedCapacity(t, immediateTerminalRangeLimit+2)
	sequence := harness.sequence
	ordinaryDone, immediateDone := 0, 0
	var last sequenceAck
	for index := 0; index <= immediateTerminalRangeLimit; index++ {
		immediateWireSend(t, harness, false, &ordinaryDone)
		id, number, _, _ := immediateWireSend(t, harness, true, &immediateDone)
		last = sequenceAck{messageId: id, sequenceNumber: number, selective: true, deliveredImmediately: true, immediateSequenceNumber: number, immediateSequenceNumberSet: true}
		window := sequenceAckWindowSnapshot{selectiveAcks: map[Id]sequenceAck{id: last}}
		sequence.applyAcceptedAckSnapshot(window)
		if index < immediateTerminalRangeLimit {
			if immediateDone != index+1 || sequence.resendQueue.GetByMessageId(id) != nil {
				t.Fatal("terminal range refused before its exact sparse bound")
			}
		} else {
			item := sequence.resendQueue.GetByMessageId(id)
			if item == nil || !item.selectiveAcked || immediateDone != immediateTerminalRangeLimit || len(sequence.immediateTerminals) != immediateTerminalRangeLimit {
				t.Fatal("full terminal range set discarded reliable fallback custody")
			}
		}
	}
	sequence.receiveAck(last.messageId, false, sequenceTag{}, false)
	if ordinaryDone != immediateTerminalRangeLimit+1 || immediateDone != immediateTerminalRangeLimit+1 || sequence.resendQueue.Len() != 0 || budget.UsedByteCount() != 0 || len(sequence.immediateTerminals) != 0 {
		t.Fatal("sparse fallback did not drain all original ownership on a real cumulative receipt")
	}
}

// A legacy selective receipt may precede its stronger exact terminal receipt.
// Both observations share service credit and return lifetime ownership once.
func TestDeliverImmediatelyTerminalUpgradesRetainedSelectiveReceipt(t *testing.T) {
	harness, budget := immediateWireSenderHarness(t)
	sequence := harness.sequence
	ordinaryDone, immediateDone := 0, 0
	ordinaryId, _, _, _ := immediateWireSend(t, harness, false, &ordinaryDone)
	immediateId, number, _, _ := immediateWireSend(t, harness, true, &immediateDone)
	before := budget.UsedByteCount()
	sequence.receiveAck(immediateId, true, sequenceTag{}, false)
	item := sequence.resendQueue.GetByMessageId(immediateId)
	if item == nil || !item.selectiveAcked || !item.deliveryObserved || immediateDone != 0 || budget.UsedByteCount() != before {
		t.Fatal("ordinary SACK failed to retain its exact original")
	}
	terminal := sequenceAck{messageId: immediateId, sequenceNumber: number, selective: true,
		deliveredImmediately: true, immediateSequenceNumber: number, immediateSequenceNumberSet: true}
	sequence.applyAcceptedAckSnapshot(sequenceAckWindowSnapshot{selectiveAcks: map[Id]sequenceAck{immediateId: terminal}})
	if immediateDone != 1 || ordinaryDone != 0 || sequence.resendQueue.GetByMessageId(immediateId) != nil || sequence.resendQueue.GetByMessageId(ordinaryId) == nil || budget.UsedByteCount() >= before {
		t.Fatal("terminal upgrade failed exact selective lifetime retirement")
	}
	sequence.receiveAck(ordinaryId, false, sequenceTag{}, false)
	if immediateDone != 1 || ordinaryDone != 1 || budget.UsedByteCount() != 0 {
		t.Fatal("terminal upgrade changed predecessor or duplicate accounting")
	}
}

// Feedback already accepted before cancellation still completes its exact
// original; cancellation owns the predecessor and cannot turn it into delivery.
func TestDeliverImmediatelyAcceptedTerminalSurvivesSenderCancelCut(t *testing.T) {
	harness, budget := immediateWireSenderHarness(t)
	sequence := harness.sequence
	ordinaryDone, immediateDone := 0, 0
	immediateWireSend(t, harness, false, &ordinaryDone)
	immediateId, number, _, _ := immediateWireSend(t, harness, true, &immediateDone)
	window := newSequenceAckWindow()
	sequence.coalesceReceivedAck(window, receiveAckMessage{sequenceId: sequence.sequenceId, messageId: immediateId,
		selective: true, deliveredImmediately: true, immediateSequenceNumber: number, immediateSequenceNumberSet: true})
	sequence.cancel()
	sequence.finishAcceptedAcknowledgements(window)
	if immediateDone != 1 || ordinaryDone != 0 || sequence.resendQueue.Len() != 1 {
		t.Fatal("accepted terminal receipt was lost or acknowledged a missing predecessor at cancel")
	}
	sequence.releaseRetainedSendItems(context.Canceled)
	if immediateDone != 1 || ordinaryDone != 0 || sequence.resendQueue.Len() != 0 || budget.UsedByteCount() != 0 || len(sequence.immediateTerminals) != 0 {
		t.Fatal("canceled sender retained or redelivered immediate ownership")
	}
}
