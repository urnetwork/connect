// The record on the wire: the one encoder and the one parser the whole system shares.
//
// No specification gives record_bytes a byte layout. Master section 8 lists the record's
// fields and their widths, spec A section 5.1 gives the go shape, and spec B section
// 4.3.3 carries the result opaquely in a protobuf bytes field and says only that
// EncodeRecord produced it and ParseRecord reads it. The layout is therefore internal to
// this package, and it is defined here. In encode order:
//
//	u8       format_version = 0x02
//	raw[32]  group_id
//	raw[16]  sender_handle
//	u64      epoch
//	u64      stream_index
//	u8       is_commit              0 or 1 only; any other value is a decode error
//	u8       retention_class_wire   the joined byte of master section 8's table
//	u64      eph_window             t, the eph ladder's time slice; always written
//	u8       size_bucket
//	u64      expire_at              unix milliseconds, 0 = unset
//	raw[32]  body_hash
//	LP       blob_id                32 bytes iff size_bucket is the blob rung, else empty
//	LP       server_attachment      empty for an ordinary record
//	LP       ct_head
//	LP       ct_body                empty, or exactly SizeBucketCtBodyBytes(size_bucket)
//	raw[32]  write_auth
//
// Sixteen elements: master section 8's fifteen RECORD fields, which is every line of that
// block except record_id, and the format version in front of them. The count is a
// consequence of the block rather than a number to keep in step with it — section 8 says
// so in as many words, and says that a reader who counts the block is the check.
//
// eph_window sits immediately after retention_class_wire because it is the field that
// qualifies it, and because master section 8's RECORD listing is normative in its ORDER
// and this table follows it. Owner decision 60 is the only normative statement of the
// octet ENCODING, and its amendment of 2026-09-13 states this position and this version
// bump. It is ALWAYS written, at its full u64 width, on every class: the presence rule is
// zero VALUED and never zero LENGTH, so no branch here and none in any preimage builder.
//
// The rule that generated the table, which is what the next person adding a field needs
// rather than the table itself: a field whose width is fixed by its go type encodes raw
// at that width, and a field whose length varies encodes as LP(x). LP is the notation
// master section 8 uses in every preimage and it means a fixed 32 bit big endian length
// then the bytes — syntax.WriteOpaqueLP, and never syntax.WriteOpaque, which is MLS's
// variable length varint prefix and a different encoding entirely. The two are never
// interchangeable, and the record layer uses LP everywhere the spec writes LP. Every
// integer is big endian, which is what syntax writes and what master section 8 says of
// expire_at explicitly. The field order is master section 8's RECORD listing, so a
// reader with the spec open meets the fields in the order they are written down there.
//
// record_id is not in this encoding and never will be. It is server assigned after
// acceptance, it appears in neither aad and in neither preimage, and spec B section
// 4.3.3 carries it as a sibling protobuf field beside record_bytes — field 13, ignored
// on submit and populated on read. EncodeRecord ignores Record.RecordId entirely, so
// encoding a record, assigning it an id and encoding it again produces identical bytes,
// and ParseRecord always answers zero for it. A record_id inside these bytes would be a
// value the write_auth mac covers, which would make it authenticated, which is exactly
// what master section 8 says it is not.
//
// Two things this file deliberately does not do. It does not enforce the submit only
// equality on ct_body length — see checkRecord — and it does not export anything beyond
// the three functions spec A section 12.1 publishes, because that block is restated
// character for character in spec B section 12.1 and a fourth name here breaks the claim
// that the two are the same list.
//
// That second one is a statement about this file and not about the package, and the
// difference matters because of what the allowlist test spec A section 12.1 describes
// asserts: the names in that block, which are what the server may reach, and not the set
// this package exports. Amendments A-9 and B-9 say so in as many words, and aad.go is the
// standing example — it exports the two preimage builders of master section 8, both
// deliberately on no line of section 12.1, because a server that never decrypts never
// builds either. The refusals below are the sentinels in errors.go and nine of those are
// on the block: section 5.9 guardrail 7 requires every failure here to be a typed error,
// and a typed error the server cannot name is one it can only match on message text. Both
// section 12.1 blocks listed functions and types and no errors, so the nine were added
// there as amendments A-8 and B-8. The two lists are still the same list; the way to keep
// them that way is to amend the block the day something on it can return a name the block
// does not carry, not to leave the name off.
package message

import (
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// The one format version this package writes and the only one it reads. It is first on
// the wire so that a reader meets it before it has interpreted anything sized or
// positioned by a later field, and it is checked the moment it is read rather than with
// the rest of the validation at the end: every offset below it is only meaningful under
// this version, so a v2 record parsed as a v1 record would report whichever field
// happened to land somewhere illegal instead of the one thing actually wrong with it.
//
// IT WAS 0x01 UNTIL 2026-09-13 AND THE FIELD THAT MOVED IT IS eph_window. Master section
// 14 froze this wire format before slice 2, slice 2 is this package and it has shipped,
// and the owner reopened the freeze deliberately rather than absorbing it; master section
// 0's ninth amendment carries what that cost. So the version is not decoration. A v1
// record has eight fewer octets and carries a DIFFERENT FIELD AT EVERY OFFSET PAST
// retention_class_wire, and a parser that accepted 0x01 and defaulted the window to zero
// would read size_bucket out of the first octet of expire_at and call the result a
// record. That silent zero is the failure this constant refuses; the refusal is the
// version check below and nothing else stands in for it.
//
// Whether 0x01 should instead be TOLERATED on the read path for one release is ledger
// item 182 and is the owner's, filed and not ruled. Refusal is what master section 0
// assumes and is the safe half of that question, so refusal is what is implemented; it is
// also the half a later ruling can widen without invalidating anything already written.
const recordFormatVersion uint8 = 0x02

// The version this one replaced, carried so the refusal can say WHICH old record it met
// rather than only that the octet was wrong. Nothing encodes it and nothing parses it;
// master section 0's "nothing already encoded is migrated, because nothing already
// encoded is retained" is why there is no read path for it at all.
const recordFormatVersionSuperseded uint8 = 0x01

// The blob id's exact length, from master section 8 and spec A section 5.1. It is the
// one fixed width field that is a slice rather than an array in go — it is absent on
// every record that is not on the blob rung — so its width is a constant here rather
// than a property of its type.
const blobIdBytes = 32

// EncodeRecord serialises a record into the layout at the top of this file. It refuses a
// record its own parser would refuse, through the same checkRecord the parser runs, so
// there is no record this package will write and then fail to read back.
//
// It accepts an all zero WriteAuth and a nil CtBody, and both are ordinary rather than
// exceptional: spec A section 2.4 has the server rebuild record_bytes from its stored
// columns on every read, with ct_body nil when the body was erased or when heads_only
// was set on the fetch, and with write_auth zero always, because write_auth is a mac
// over the submitting connection's server_nonce and there is nothing left to reconstruct
// it from and nobody who could verify it.
//
// Record.RecordId is not read. See the file comment.
func EncodeRecord(r *Record) ([]byte, error) {
	if r == nil {
		return nil, ErrRecordNil
	}
	header := &r.Header
	retentionWire, err := checkRecord(header, r.CtBody)
	if err != nil {
		return nil, err
	}
	writer := syntax.NewWriter()
	writer.WriteUint8(recordFormatVersion)
	writer.WriteRaw(header.GroupId[:])
	writer.WriteRaw(header.SenderHandle[:])
	writer.WriteUint64(header.Epoch)
	writer.WriteUint64(header.StreamIndex)
	writer.WriteUint8(isCommitByte(header.IsCommit))
	writer.WriteUint8(retentionWire)
	writer.WriteUint64(header.EphWindow)
	writer.WriteUint8(byte(header.SizeBucket))
	writer.WriteUint64(header.ExpireAt)
	writer.WriteRaw(header.BodyHash[:])
	writer.WriteOpaqueLP(header.BlobId)
	writer.WriteOpaqueLP(header.ServerAttachment)
	writer.WriteOpaqueLP(r.CtHead)
	writer.WriteOpaqueLP(r.CtBody)
	writer.WriteRaw(r.WriteAuth[:])
	// the writer is sticky: the first failure latches and every later call is a no op,
	// so this is the one place the encode is asked whether it worked.
	return writer.Bytes()
}

// ParseRecord deserialises the layout at the top of this file, validating as spec A
// sections 5.1 and 5.8 require: the format version, is_commit as a genuine boolean, the
// retention class byte through the one split in record.go, the size bucket against the
// ladder, the blob id's presence against the size bucket in both directions, the ct_body
// length, and full consumption of the input.
//
// The returned record's RecordId is always zero. The id is not in these bytes.
func ParseRecord(b []byte) (*Record, error) {
	return decodeRecord(b)
}

// ParseRecordHeader answers the header of a record, for a caller that has no use for the
// ciphertexts — spec B section 5.1 check 3 reads the size bucket and the blob id from
// here rather than from the request's own projection fields.
//
// It is exactly ParseRecord with the rest of the record dropped, and it is that rather
// than a second decoder that stops early, on purpose. A header parser that stopped at
// body_hash would accept a record whose ct_body length is illegal, whose blob id
// disagrees with its size bucket, or that carries trailing bytes — and the server's
// check 3 would then be reading a header out of a record that ParseRecord, one function
// call later in the same request, refuses. Two entry points that disagree about which
// records exist is the defect this shape makes unrepresentable, and codec_test.go
// asserts the agreement over every input it constructs.
func ParseRecordHeader(b []byte) (*RecordHeader, error) {
	record, err := decodeRecord(b)
	if err != nil {
		return nil, err
	}
	return &record.Header, nil
}

// The one decode routine both entry points share.
//
// The reader is sticky, so the reads below are written as a straight run and the input
// is asked whether it was well formed exactly once, at Done — which reports the first
// latched failure if there was one and otherwise refuses any trailing byte, spec A
// section 5.8's full consumption rule. Nothing is validated as a value until that
// question has been answered, so a truncated record reports that it was truncated rather
// than reporting whatever a field read off the end happens to be.
//
// No allocation happens before validation, section 5.8's second rule. The fixed width
// fields are bounded by their own widths, and every LP field's declared length is checked
// against the configured maximum and then against the bytes actually remaining before
// syntax sizes anything with it — which is why they are read through ReadOpaqueLP into
// fresh slices rather than into a buffer made here.
func decodeRecord(bs []byte) (*Record, error) {
	reader := syntax.NewReader(bs)

	version, err := reader.ReadUint8()
	if err != nil {
		return nil, err
	}
	if version != recordFormatVersion {
		if version == recordFormatVersionSuperseded {
			return nil, fmt.Errorf("%w: 0x%02x carries no eph_window and puts a different field at every offset past retention_class, want 0x%02x",
				ErrRecordFormatVersion, version, recordFormatVersion)
		}
		return nil, fmt.Errorf("%w: 0x%02x, want 0x%02x", ErrRecordFormatVersion, version, recordFormatVersion)
	}

	record := &Record{}
	header := &record.Header

	groupId, _ := reader.ReadRaw(len(header.GroupId))
	senderHandle, _ := reader.ReadRaw(len(header.SenderHandle))
	header.Epoch, _ = reader.ReadUint64()
	header.StreamIndex, _ = reader.ReadUint64()
	isCommit, _ := reader.ReadUint8()
	retentionWire, _ := reader.ReadUint8()
	header.EphWindow, _ = reader.ReadUint64()
	sizeBucket, _ := reader.ReadUint8()
	header.ExpireAt, _ = reader.ReadUint64()
	bodyHash, _ := reader.ReadRaw(len(header.BodyHash))
	blobId, _ := reader.ReadOpaqueLP()
	serverAttachment, _ := reader.ReadOpaqueLP()
	ctHead, _ := reader.ReadOpaqueLP()
	ctBody, _ := reader.ReadOpaqueLP()
	writeAuth, _ := reader.ReadRaw(len(record.WriteAuth))

	if err := reader.Done(); err != nil {
		return nil, err
	}

	copy(header.GroupId[:], groupId)
	copy(header.SenderHandle[:], senderHandle)
	copy(header.BodyHash[:], bodyHash)
	copy(record.WriteAuth[:], writeAuth)
	header.BlobId = absentIfEmpty(blobId)
	header.ServerAttachment = absentIfEmpty(serverAttachment)
	record.CtHead = absentIfEmpty(ctHead)
	record.CtBody = absentIfEmpty(ctBody)

	// a u8 decoded into a go bool with != 0 accepts 255 and calls it true, which makes
	// two encoders that disagree about the byte both appear to work while the write_auth
	// mac and both aads — all three of which cover this byte — disagree about the record.
	if 1 < isCommit {
		return nil, fmt.Errorf("%w: 0x%02x, want 0x00 or 0x01", ErrIsCommitNotBoolean, isCommit)
	}
	header.IsCommit = isCommit == 1

	// the split is record.go's, not this file's: there is exactly one place in the system
	// where the class and the eph bucket come apart, and a codec that took the byte apart
	// itself would be the second.
	class, ephBucket, err := RetentionClassOf(retentionWire)
	if err != nil {
		return nil, err
	}
	header.RetentionClass = class
	header.EphBucket = ephBucket
	header.SizeBucket = SizeBucket(sizeBucket)

	if _, err := checkRecord(header, record.CtBody); err != nil {
		return nil, err
	}
	return record, nil
}

// The structural invariants, run by both sides of the codec so that the set of records
// this package will write and the set it will read are the same set. It answers the
// retention class wire byte on the way, because deciding whether the class and the
// bucket are a legal pair and computing the byte they join to are the same question
// asked of the same function.
//
// The parser is deliberately more permissive about ct_body than the server's submit
// check is, and this is the rule in this file most likely to be "fixed" into something
// wrong. On submit, spec B section 5.1 check 3 requires octet_length(ct_body) to be
// exactly SizeBucketCtBodyBytes(b) — an equality, not a range, because master section 9.5
// pads bodies into rungs and a body that is not exactly its rung leaks its real length.
// But the parser also runs on the read path, where spec A section 2.4 has the server
// rebuild record_bytes from its stored columns with ct_body nil whenever the body was
// erased by retention or whenever heads_only was set on the fetch. A parser that enforced
// the submit equality would refuse every pruned record and every heads_only response —
// that is, it would refuse most of what a client ever fetches. So the length here is
// either the rung's or absent, and the server enforces the submit only half itself by
// calling the already exported SizeBucketCtBodyBytes. That function is on spec A section
// 12.1's published surface for exactly this reason, and no new export is needed for it.
func checkRecord(header *RecordHeader, ctBody []byte) (byte, error) {
	retentionWire, err := RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		return retentionWire, err
	}
	// the blob rung is the top of the ladder, so the range check is against it rather
	// than against a written down 5: a rung added above it moves this bound with it.
	if SizeBucketBlob < header.SizeBucket {
		return retentionWire, fmt.Errorf("%w: %d, want 0..%d", ErrSizeBucketOutOfRange, header.SizeBucket, SizeBucketBlob)
	}
	// presence in both directions. a blob record without its blob id names no object, and
	// a non blob record carrying one names an object nothing will ever fetch; both are
	// records whose write_auth preimage covers a blob id field that disagrees with the
	// size bucket beside it.
	if header.SizeBucket == SizeBucketBlob {
		if len(header.BlobId) != blobIdBytes {
			return retentionWire, fmt.Errorf("%w: the blob rung carries a %d byte blob id, want %d", ErrBlobIdPresence, len(header.BlobId), blobIdBytes)
		}
	} else if 0 < len(header.BlobId) {
		return retentionWire, fmt.Errorf("%w: size bucket %d carries a %d byte blob id, want none", ErrBlobIdPresence, header.SizeBucket, len(header.BlobId))
	}
	if 0 < len(ctBody) {
		want := SizeBucketCtBodyBytes(header.SizeBucket)
		if want < 0 || len(ctBody) != want {
			return retentionWire, fmt.Errorf("%w: size bucket %d carries a %d byte ct_body, want 0 or %d", ErrCtBodyLength, header.SizeBucket, len(ctBody), want)
		}
	}
	return retentionWire, nil
}

// The wire byte of a go bool. Written out rather than reached for through a conversion,
// because the whole point of the field is that only two of the 256 values are legal and
// the encoder should be visibly incapable of producing a third.
func isCommitByte(isCommit bool) uint8 {
	if isCommit {
		return 1
	}
	return 0
}

// What the parser answers for a zero length LP field.
//
// LP carries no representation for "absent": a nil slice and an empty slice both encode
// to the same four zero octets, so the wire cannot tell them apart and neither can a
// reader. Answering nil rather than an empty slice makes the parsed record the shape spec
// A section 5.1 describes — BlobId nil off the blob rung, CtBody nil once pruned — so a
// caller's test against nil means what it reads as, and the go value the parser hands
// back for a given byte string is canonical.
func absentIfEmpty(bs []byte) []byte {
	if len(bs) == 0 {
		return nil
	}
	return bs
}
