package connect

// Hand-rolled protobuf wire codec for the hot transfer frames.
//
// The transfer data plane marshals a `protocol.TransferFrame` carrying a
// `protocol.Pack` for every packet, and the reflection-based proto marshal —
// plus the intermediate Pack/TransferFrame/Tag/TransferPath structs and the
// `Id.Bytes()` slices they require — dominates the per-packet allocation
// profile. This file encodes that frame directly into a pooled buffer with no
// intermediate structs, no `Id.Bytes()` escapes, and no reflection.
//
// Wire compatibility is the invariant: output here must be byte-identical to
// `proto.Marshal` of the equivalent structs (verified by round-trip tests in
// frame_protobuf_test.go), so peers and the platform — and our own
// `setHead` re-marshal path, which `proto.Unmarshal`s these bytes — decode it
// unchanged. Encoding follows field-number order with proto3 presence rules
// (implicit scalars omit zero values; `optional`/message/`bytes` fields are
// emitted when set).
//
// This file also holds the allocation-free `FilteredTransferPath` reader used
// on the routing path, keeping all custom protobuf logic together.

import (
	"errors"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect/v2026/protocol"
)

// proto wire types
const (
	protoWireVarint = 0
	protoWireBytes  = 2
)

func protoSizeVarint(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n += 1
	}
	return n
}

func protoAppendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// protoTag is (fieldNumber << 3) | wireType. All field numbers used here are
// small enough that the tag is a single byte, but go through the varint helpers
// for correctness.
func protoSizeTag(fieldNumber int) int {
	return protoSizeVarint(uint64(fieldNumber) << 3)
}

func protoAppendTag(b []byte, fieldNumber int, wireType int) []byte {
	return protoAppendVarint(b, uint64(fieldNumber)<<3|uint64(wireType))
}

// --- protocol.Frame { message_type=1, message_bytes=2, raw=3 } ---

func sizeFrame(f *protocol.Frame) int {
	n := 0
	if f.MessageType != 0 {
		n += protoSizeTag(1) + protoSizeVarint(uint64(f.MessageType))
	}
	if 0 < len(f.MessageBytes) {
		n += protoSizeTag(2) + protoSizeVarint(uint64(len(f.MessageBytes))) + len(f.MessageBytes)
	}
	if f.Raw {
		n += protoSizeTag(3) + 1
	}
	return n
}

func appendFrame(b []byte, f *protocol.Frame) []byte {
	if f.MessageType != 0 {
		b = protoAppendTag(b, 1, protoWireVarint)
		b = protoAppendVarint(b, uint64(f.MessageType))
	}
	if 0 < len(f.MessageBytes) {
		b = protoAppendTag(b, 2, protoWireBytes)
		b = protoAppendVarint(b, uint64(len(f.MessageBytes)))
		b = append(b, f.MessageBytes...)
	}
	if f.Raw {
		b = protoAppendTag(b, 3, protoWireVarint)
		b = append(b, 1)
	}
	return b
}

// --- protocol.TransferPath { destination_id=1, source_id=2, stream_id=3 } ---
// each id is an optional 16-byte value, emitted only when non-zero (matching
// TransferPath.ToProtobuf).

func sizeTransferPath(path TransferPath) int {
	n := 0
	if (path.DestinationId != Id{}) {
		n += protoSizeTag(1) + protoSizeVarint(16) + 16
	}
	if (path.SourceId != Id{}) {
		n += protoSizeTag(2) + protoSizeVarint(16) + 16
	}
	if (path.StreamId != Id{}) {
		n += protoSizeTag(3) + protoSizeVarint(16) + 16
	}
	return n
}

func appendIdField(b []byte, fieldNumber int, id Id) []byte {
	b = protoAppendTag(b, fieldNumber, protoWireBytes)
	b = protoAppendVarint(b, 16)
	return append(b, id[:]...)
}

func appendTransferPath(b []byte, path TransferPath) []byte {
	if (path.DestinationId != Id{}) {
		b = appendIdField(b, 1, path.DestinationId)
	}
	if (path.SourceId != Id{}) {
		b = appendIdField(b, 2, path.SourceId)
	}
	if (path.StreamId != Id{}) {
		b = appendIdField(b, 3, path.StreamId)
	}
	return b
}

// --- send-side Pack TransferFrame ---

// sendPackFrame is the set of values the per-packet send path encodes into a
// TransferFrame carrying a Pack. Passed by value (no heap alloc) to the codec.
type sendPackFrame struct {
	path           TransferPath
	messageId      Id
	sequenceId     Id
	sequenceNumber uint64
	head           bool
	nack           bool
	frames         []*protocol.Frame
	contractFrame  *protocol.Frame // nil when absent
	// tagSendTime is the Pack.tag.send_time (ms). The Tag message is always
	// emitted (matching rttWindow.OpenTag); send_time itself is omitted only
	// when zero, per implicit presence.
	tagSendTime uint64
	contractId  *Id // nil when absent (only set for nack packs)
	// sessionRole is emitted (TransferFrame field 7) only when sessionRoleSet.
	sessionRole    protocol.SequenceRole
	sessionRoleSet bool
	// companion is emitted (TransferFrame field 8) only when true.
	companion bool
}

// sizePack returns the encoded size of the Pack submessage body.
func (m *sendPackFrame) sizePack() int {
	n := 0
	// message_id=1, sequence_id=2 (always 16-byte non-optional bytes)
	n += protoSizeTag(1) + protoSizeVarint(16) + 16
	n += protoSizeTag(2) + protoSizeVarint(16) + 16
	if m.sequenceNumber != 0 {
		n += protoSizeTag(3) + protoSizeVarint(m.sequenceNumber)
	}
	if m.head {
		n += protoSizeTag(4) + 1
	}
	for _, f := range m.frames {
		fs := sizeFrame(f)
		n += protoSizeTag(5) + protoSizeVarint(uint64(fs)) + fs
	}
	if m.nack {
		n += protoSizeTag(6) + 1
	}
	if m.contractFrame != nil {
		fs := sizeFrame(m.contractFrame)
		n += protoSizeTag(7) + protoSizeVarint(uint64(fs)) + fs
	}
	// tag=8 (always present); body is send_time=1 when non-zero
	tagBody := 0
	if m.tagSendTime != 0 {
		tagBody = protoSizeTag(1) + protoSizeVarint(m.tagSendTime)
	}
	n += protoSizeTag(8) + protoSizeVarint(uint64(tagBody)) + tagBody
	if m.contractId != nil {
		n += protoSizeTag(9) + protoSizeVarint(16) + 16
	}
	return n
}

func (m *sendPackFrame) appendPack(b []byte) []byte {
	b = appendIdField(b, 1, m.messageId)
	b = appendIdField(b, 2, m.sequenceId)
	if m.sequenceNumber != 0 {
		b = protoAppendTag(b, 3, protoWireVarint)
		b = protoAppendVarint(b, m.sequenceNumber)
	}
	if m.head {
		b = protoAppendTag(b, 4, protoWireVarint)
		b = append(b, 1)
	}
	for _, f := range m.frames {
		b = protoAppendTag(b, 5, protoWireBytes)
		b = protoAppendVarint(b, uint64(sizeFrame(f)))
		b = appendFrame(b, f)
	}
	if m.nack {
		b = protoAppendTag(b, 6, protoWireVarint)
		b = append(b, 1)
	}
	if m.contractFrame != nil {
		b = protoAppendTag(b, 7, protoWireBytes)
		b = protoAppendVarint(b, uint64(sizeFrame(m.contractFrame)))
		b = appendFrame(b, m.contractFrame)
	}
	// tag=8
	tagBody := 0
	if m.tagSendTime != 0 {
		tagBody = protoSizeTag(1) + protoSizeVarint(m.tagSendTime)
	}
	b = protoAppendTag(b, 8, protoWireBytes)
	b = protoAppendVarint(b, uint64(tagBody))
	if m.tagSendTime != 0 {
		b = protoAppendTag(b, 1, protoWireVarint)
		b = protoAppendVarint(b, m.tagSendTime)
	}
	if m.contractId != nil {
		b = appendIdField(b, 9, *m.contractId)
	}
	return b
}

// size returns the encoded size of the whole TransferFrame.
func (m *sendPackFrame) size() int {
	n := 0
	// transfer_path=1
	pathSize := sizeTransferPath(m.path)
	n += protoSizeTag(1) + protoSizeVarint(uint64(pathSize)) + pathSize
	// message_type=3 (optional, always set to TransferPack=0 by the v2 send path)
	n += protoSizeTag(3) + protoSizeVarint(uint64(protocol.MessageType_TransferPack))
	// pack=4
	packSize := m.sizePack()
	n += protoSizeTag(4) + protoSizeVarint(uint64(packSize)) + packSize
	// session_role=7
	if m.sessionRoleSet {
		n += protoSizeTag(7) + protoSizeVarint(uint64(m.sessionRole))
	}
	// session_companion=8
	if m.companion {
		n += protoSizeTag(8) + 1
	}
	return n
}

func (m *sendPackFrame) append(b []byte) []byte {
	// transfer_path=1
	b = protoAppendTag(b, 1, protoWireBytes)
	b = protoAppendVarint(b, uint64(sizeTransferPath(m.path)))
	b = appendTransferPath(b, m.path)
	// message_type=3 = TransferPack (0)
	b = protoAppendTag(b, 3, protoWireVarint)
	b = protoAppendVarint(b, uint64(protocol.MessageType_TransferPack))
	// pack=4
	b = protoAppendTag(b, 4, protoWireBytes)
	b = protoAppendVarint(b, uint64(m.sizePack()))
	b = m.appendPack(b)
	// session_role=7
	if m.sessionRoleSet {
		b = protoAppendTag(b, 7, protoWireVarint)
		b = protoAppendVarint(b, uint64(m.sessionRole))
	}
	// session_companion=8
	if m.companion {
		b = protoAppendTag(b, 8, protoWireVarint)
		b = append(b, 1)
	}
	return b
}

// marshalSendPackTransferFrame encodes the TransferFrame into a pooled buffer.
// The returned slice is owned by the caller (MessagePoolReturn when done). It is
// byte-identical to ProtoMarshal of the equivalent structs (see tests).
func marshalSendPackTransferFrame(m *sendPackFrame) []byte {
	size := m.size()
	buf := MessagePoolGet(size)
	out := m.append(buf[:0])
	// size() is exact, so append never grows past the pooled capacity; guard
	// anyway so a sizing bug surfaces as a returned-to-pool orphan, not a leak.
	if cap(out) != cap(buf) {
		MessagePoolReturn(buf)
	}
	return out
}

// --- send-side Ack TransferFrame ---

// sendAckFrame is the set of values the ack-writer encodes into a TransferFrame
// carrying an Ack. The inner ack frame is not session-stamped (wrapping, when
// it happens, adds the role/companion hint to the outer encrypted frame).
type sendAckFrame struct {
	path       TransferPath
	messageId  Id
	sequenceId Id
	selective  bool
	tag        *protocol.Tag // nil when absent
}

func (m *sendAckFrame) sizeAck() int {
	n := 0
	// message_id=1, sequence_id=2 (always 16-byte bytes)
	n += protoSizeTag(1) + protoSizeVarint(16) + 16
	n += protoSizeTag(2) + protoSizeVarint(16) + 16
	if m.selective {
		n += protoSizeTag(3) + 1
	}
	if m.tag != nil {
		tagBody := sizeTagBody(m.tag.SendTime)
		n += protoSizeTag(4) + protoSizeVarint(uint64(tagBody)) + tagBody
	}
	return n
}

func (m *sendAckFrame) appendAck(b []byte) []byte {
	b = appendIdField(b, 1, m.messageId)
	b = appendIdField(b, 2, m.sequenceId)
	if m.selective {
		b = protoAppendTag(b, 3, protoWireVarint)
		b = append(b, 1)
	}
	if m.tag != nil {
		b = protoAppendTag(b, 4, protoWireBytes)
		b = protoAppendVarint(b, uint64(sizeTagBody(m.tag.SendTime)))
		b = appendTagBody(b, m.tag.SendTime)
	}
	return b
}

func (m *sendAckFrame) size() int {
	n := 0
	// transfer_path=1
	pathSize := sizeTransferPath(m.path)
	n += protoSizeTag(1) + protoSizeVarint(uint64(pathSize)) + pathSize
	// message_type=3 = TransferAck
	n += protoSizeTag(3) + protoSizeVarint(uint64(protocol.MessageType_TransferAck))
	// ack=5
	ackSize := m.sizeAck()
	n += protoSizeTag(5) + protoSizeVarint(uint64(ackSize)) + ackSize
	return n
}

func (m *sendAckFrame) append(b []byte) []byte {
	// transfer_path=1
	b = protoAppendTag(b, 1, protoWireBytes)
	b = protoAppendVarint(b, uint64(sizeTransferPath(m.path)))
	b = appendTransferPath(b, m.path)
	// message_type=3 = TransferAck
	b = protoAppendTag(b, 3, protoWireVarint)
	b = protoAppendVarint(b, uint64(protocol.MessageType_TransferAck))
	// ack=5
	b = protoAppendTag(b, 5, protoWireBytes)
	b = protoAppendVarint(b, uint64(m.sizeAck()))
	b = m.appendAck(b)
	return b
}

// marshalSendAckTransferFrame encodes the Ack TransferFrame into a pooled
// buffer (caller owns; MessagePoolReturn when done). Byte-identical to
// ProtoMarshal of the equivalent structs (see tests).
func marshalSendAckTransferFrame(m *sendAckFrame) []byte {
	size := m.size()
	buf := MessagePoolGet(size)
	out := m.append(buf[:0])
	if cap(out) != cap(buf) {
		MessagePoolReturn(buf)
	}
	return out
}

// sizeTagBody / appendTagBody encode the body of a Tag submessage
// (send_time=1, omitted when zero per implicit presence).
func sizeTagBody(sendTime uint64) int {
	if sendTime != 0 {
		return protoSizeTag(1) + protoSizeVarint(sendTime)
	}
	return 0
}

func appendTagBody(b []byte, sendTime uint64) []byte {
	if sendTime != 0 {
		b = protoAppendTag(b, 1, protoWireVarint)
		b = protoAppendVarint(b, sendTime)
	}
	return b
}

// --- receive-side decode (copy-safe) ---
//
// Hand-rolled decode of an inbound TransferFrame into the existing protocol
// structs, using the official protowire primitives for parsing but allocating
// messages directly (no reflection) and copying bytes fields exactly as the
// proto library does — so downstream lifetime semantics are unchanged (this is
// the copy-safe variant; it does not alias the receive buffer). Two deviations
// from a faithful decode, both allocation trims that are safe because nothing
// reads the dropped fields on the decode paths this is used for:
//   - the deprecated TransferFrame.message_type (field 3) is always skipped.
//   - the outer transfer_path is skipped unless decodePath; routing parses the
//     path separately via FilteredTransferPath, and only the inner/unwrapped
//     frame reads its path (for the tamper check).
//
// Returns false on malformed input, which the caller treats as a bad message —
// matching ProtoUnmarshal's error. setHead still uses ProtoUnmarshal: it
// re-marshals the frame and must preserve every field, including the path.

func copyProtoBytes(v []byte) []byte {
	return append([]byte(nil), v...)
}

func decodeTag(b []byte) (*protocol.Tag, bool) {
	tag := &protocol.Tag{}
	for 0 < len(b) {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
		if num == 1 { // send_time
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			tag.SendTime = v
			continue
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			return nil, false
		}
		b = b[fn:]
	}
	return tag, true
}

func decodeFrame(b []byte) (*protocol.Frame, bool) {
	f := &protocol.Frame{}
	for 0 < len(b) {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
		switch num {
		case 1: // message_type
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			f.MessageType = protocol.MessageType(v)
		case 2: // message_bytes
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			f.MessageBytes = copyProtoBytes(v)
		case 3: // raw
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			f.Raw = protowire.DecodeBool(v)
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return nil, false
			}
			b = b[fn:]
		}
	}
	return f, true
}

func decodeTransferPathProto(b []byte) (*protocol.TransferPath, bool) {
	path := &protocol.TransferPath{}
	for 0 < len(b) {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
		switch num {
		case 1, 2, 3:
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			switch num {
			case 1:
				path.DestinationId = copyProtoBytes(v)
			case 2:
				path.SourceId = copyProtoBytes(v)
			case 3:
				path.StreamId = copyProtoBytes(v)
			}
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return nil, false
			}
			b = b[fn:]
		}
	}
	return path, true
}

func decodePack(b []byte) (*protocol.Pack, bool) {
	pack := &protocol.Pack{}
	for 0 < len(b) {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
		switch num {
		case 1, 2, 9: // message_id, sequence_id, contract_id (bytes)
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			switch num {
			case 1:
				pack.MessageId = copyProtoBytes(v)
			case 2:
				pack.SequenceId = copyProtoBytes(v)
			case 9:
				pack.ContractId = copyProtoBytes(v)
			}
		case 3: // sequence_number
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			pack.SequenceNumber = v
		case 4: // head
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			pack.Head = protowire.DecodeBool(v)
		case 6: // nack
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			pack.Nack = protowire.DecodeBool(v)
		case 5, 7: // frames (repeated), contract_frame (Frame)
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			f, ok := decodeFrame(v)
			if !ok {
				return nil, false
			}
			if num == 5 {
				pack.Frames = append(pack.Frames, f)
			} else {
				pack.ContractFrame = f
			}
		case 8: // tag (Tag)
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			tg, ok := decodeTag(v)
			if !ok {
				return nil, false
			}
			pack.Tag = tg
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return nil, false
			}
			b = b[fn:]
		}
	}
	return pack, true
}

func decodeAck(b []byte) (*protocol.Ack, bool) {
	ack := &protocol.Ack{}
	for 0 < len(b) {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
		switch num {
		case 1, 2: // message_id, sequence_id (bytes)
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			if num == 1 {
				ack.MessageId = copyProtoBytes(v)
			} else {
				ack.SequenceId = copyProtoBytes(v)
			}
		case 3: // selective
			if typ != protowire.VarintType {
				return nil, false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			ack.Selective = protowire.DecodeBool(v)
		case 4: // tag (Tag)
			if typ != protowire.BytesType {
				return nil, false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, false
			}
			b = b[vn:]
			tg, ok := decodeTag(v)
			if !ok {
				return nil, false
			}
			ack.Tag = tg
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return nil, false
			}
			b = b[fn:]
		}
	}
	return ack, true
}

// unmarshalTransferFrame decodes b into tf (see the section comment for the
// message_type/transfer_path deviations). decodePath controls whether the
// transfer_path is materialized (true for the inner/unwrapped frame, which is
// tamper-checked against the routing path; false for the outer frame).
func unmarshalTransferFrame(b []byte, tf *protocol.TransferFrame, decodePath bool) bool {
	for 0 < len(b) {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false
		}
		b = b[n:]
		switch num {
		case 1: // transfer_path
			if typ != protowire.BytesType {
				return false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			if decodePath {
				p, ok := decodeTransferPathProto(v)
				if !ok {
					return false
				}
				tf.TransferPath = p
			}
			// else skip: the outer path is unused (routing uses FilteredTransferPath)
		case 2: // frame (deprecated v1 carrier)
			if typ != protowire.BytesType {
				return false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			f, ok := decodeFrame(v)
			if !ok {
				return false
			}
			tf.Frame = f
		case 3: // message_type (deprecated) — skip, nothing reads it
			if typ != protowire.VarintType {
				return false
			}
			_, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
		case 4: // pack
			if typ != protowire.BytesType {
				return false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			p, ok := decodePack(v)
			if !ok {
				return false
			}
			tf.Pack = p
		case 5: // ack
			if typ != protowire.BytesType {
				return false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			a, ok := decodeAck(v)
			if !ok {
				return false
			}
			tf.Ack = a
		case 6: // encryptedTransferFrame (bytes)
			if typ != protowire.BytesType {
				return false
			}
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			tf.EncryptedTransferFrame = copyProtoBytes(v)
		case 7: // session_role (enum)
			if typ != protowire.VarintType {
				return false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			sr := protocol.SequenceRole(v)
			tf.SessionRole = &sr
		case 8: // session_companion (bool)
			if typ != protowire.VarintType {
				return false
			}
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return false
			}
			b = b[vn:]
			c := protowire.DecodeBool(v)
			tf.SessionCompanion = &c
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return false
			}
			b = b[fn:]
		}
	}
	return true
}

// parseFilteredTransferPath extracts the transfer path from an encoded
// `protocol.TransferFrame` without reflection or allocation. This runs for
// every frame a client routes (see `Client.run` and `Client.Forward`),
// where the reflection-based `FilteredTransferFrame` unmarshal dominates
// the allocation profile. Unknown fields are skipped the same as the
// protobuf decoder. `ok` is false on any unexpected encoding, in which
// case the caller must fall back to the full unmarshal.
func parseFilteredTransferPath(b []byte) (path TransferPath, exists bool, ok bool) {
	i := 0
	n := len(b)

	readVarint := func(limit int) (uint64, bool) {
		var v uint64
		var shift uint
		for {
			if limit <= i || 63 < shift {
				return 0, false
			}
			c := b[i]
			i += 1
			v |= uint64(c&0x7f) << shift
			if c < 0x80 {
				return v, true
			}
			shift += 7
		}
	}

	skipField := func(wireType uint64, limit int) bool {
		switch wireType {
		case 0:
			// varint
			_, varintOk := readVarint(limit)
			return varintOk
		case 1:
			// fixed 64
			if limit < i+8 {
				return false
			}
			i += 8
			return true
		case 2:
			// length delimited
			length, lengthOk := readVarint(limit)
			if !lengthOk || uint64(limit-i) < length {
				return false
			}
			i += int(length)
			return true
		case 5:
			// fixed 32
			if limit < i+4 {
				return false
			}
			i += 4
			return true
		default:
			// group types are not used by the protocol
			return false
		}
	}

	for i < n {
		tag, tagOk := readVarint(n)
		if !tagOk {
			return TransferPath{}, false, false
		}
		fieldNumber := tag >> 3
		wireType := tag & 0x7

		// TransferFrame { TransferPath transfer_path = 1; ... }
		if fieldNumber != 1 {
			if !skipField(wireType, n) {
				return TransferPath{}, false, false
			}
			continue
		}
		if wireType != 2 {
			return TransferPath{}, false, false
		}
		length, lengthOk := readVarint(n)
		if !lengthOk || uint64(n-i) < length {
			return TransferPath{}, false, false
		}
		end := i + int(length)
		exists = true

		// TransferPath {
		//     optional bytes destination_id = 1;
		//     optional bytes source_id = 2;
		//     optional bytes stream_id = 3;
		// }
		for i < end {
			pathTag, pathTagOk := readVarint(end)
			if !pathTagOk {
				return TransferPath{}, false, false
			}
			pathFieldNumber := pathTag >> 3
			pathWireType := pathTag & 0x7

			switch pathFieldNumber {
			case 1, 2, 3:
				if pathWireType != 2 {
					return TransferPath{}, false, false
				}
				idLength, idLengthOk := readVarint(end)
				if !idLengthOk || uint64(end-i) < idLength {
					return TransferPath{}, false, false
				}
				if idLength != 16 {
					// ids are always 16 bytes
					return TransferPath{}, false, false
				}
				var id Id
				copy(id[:], b[i:i+16])
				i += 16
				switch pathFieldNumber {
				case 1:
					path.DestinationId = id
				case 2:
					path.SourceId = id
				case 3:
					path.StreamId = id
				}
			default:
				if !skipField(pathWireType, end) {
					return TransferPath{}, false, false
				}
			}
		}
	}
	return path, exists, true
}

// FilteredTransferPath parses the transfer path of an encoded
// `protocol.TransferFrame`, using the allocation-free fast path with a
// full `FilteredTransferFrame` unmarshal fallback
func FilteredTransferPath(transferFrameBytes []byte) (TransferPath, error) {
	if path, exists, ok := parseFilteredTransferPath(transferFrameBytes); ok {
		if !exists {
			return TransferPath{}, errors.New("Missing transfer path")
		}
		return path, nil
	}
	// fall back to the full unmarshal
	filteredTransferFrame := &protocol.FilteredTransferFrame{}
	if err := ProtoUnmarshal(transferFrameBytes, filteredTransferFrame); err != nil {
		return TransferPath{}, err
	}
	if filteredTransferFrame.TransferPath == nil {
		return TransferPath{}, errors.New("Missing transfer path")
	}
	return TransferPathFromProtobuf(filteredTransferFrame.TransferPath)
}
