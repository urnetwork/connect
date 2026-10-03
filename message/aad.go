// The two aead preimages of master section 8: the bytes ct_body is sealed against and
// the bytes ct_head is sealed against. Nothing else lives here — the keys are the key
// schedule's, the mac is write_auth's, and this file only ever produces the additional
// authenticated data those two aeads take.
//
// Master section 8 is normative and spec B section 5.4 restates aad_head character for
// character; the two agree, and the block below is theirs:
//
//	aad_body = "URmessage/v1/aad/body" ‖ u16(alg_id) ‖ LP(group_id) ‖ LP(sender_handle)
//	         ‖ u64(epoch) ‖ u64(stream_index) ‖ u8(retention_class) ‖ u64(eph_window)
//
//	aad_head = "URmessage/v1/aad/head" ‖ u16(alg_id) ‖ LP(group_id) ‖ LP(sender_handle)
//	         ‖ u64(epoch) ‖ u64(stream_index) ‖ u8(is_commit) ‖ u8(retention_class)
//	         ‖ u64(eph_window) ‖ u8(size_bucket) ‖ u64(expire_at) ‖ LP(body_hash)
//	         ‖ LP(blob_id) ‖ LP(H(server_attachment))
//
// u64(eph_window) was added to both blocks on 2026-09-13 (ledger 152 / 183, m1 open item
// M1-27), immediately after u8(retention_class) in each, which is the field it qualifies.
// It is UNCONDITIONAL and eight octets wide on every record of every class: master
// section 8's presence rule is a zero VALUE off eph 1..5 and never a zero LENGTH, for the
// reason LP(blob_id) is written on every record that has no blob — a conditional here is
// the one thing the preimage builders in this package have never had.
//
// What the aad terms buy is NOT the window's integrity, and a reader who concludes it is
// will draw the wrong boundary around the next change. The KEY DERIVATION is what stops a
// window or bucket downgrade: K_eph[n][b][t] takes both, so an altered window yields a key
// nobody holds. The write_auth term is what makes the SERVER's plus or minus one window
// check a check on macd bytes. These two terms cost no wire octets and bind the ZERO on a
// non eph record, so a header cannot be spliced across classes — which is the whole of
// what they are for.
//
// Six things about that block are easy to get wrong, and each is worth its own paragraph,
// because every one of them produces a preimage that encodes, that round trips against
// itself, and that then fails the aead against every other implementation.
//
// The label is raw ascii and is not length prefixed. It is a fixed length constant
// prefix, written with WriteRaw and never with WriteOpaqueLP, so a reader of the bytes
// meets twenty one label octets and then the first field. The two labels are the same
// length — twenty one bytes each — which means the domain separation between the two
// preimages rests entirely on the bytes of the label differing and not at all on a length
// boundary between the label and what follows. One character typed into a label so that
// the two agree removes the separation completely, and it removes it silently: both
// preimages still build, and a header with is_commit set and the durable retention byte
// then makes aad_body an exact prefix of aad_head, which is the shape a cross protocol
// attack wants. aad_test.go asserts on exactly that header.
//
// LP means WriteOpaqueLP: a fixed thirty two bit big endian length then the bytes. It is
// not WriteOpaque, which is MLS's variable length varint prefix and a different encoding
// entirely; the record layer uses LP everywhere the specs write LP and the two are never
// interchangeable. A fixed width prefix is also what keeps the field boundaries here
// independent of the lengths inside them.
//
// u8(retention_class) is the joined wire byte of master section 8's table and not the go
// enum. The two agree on three of the four classes by coincidence — permanent, durable
// and media are 0, 1 and 2 on both sides — and disagree on every eph record, where the
// byte is 0x10 | bucket and the go tag is 3. A conversion here in place of a call to
// RetentionClassWire would therefore pass every test written with a durable record and
// fail every eph one, at the aead rather than anywhere legible. It is also the crossing
// record.go's comment confines to itself, and the gate in record_test.go reads this file.
//
// LP(blob_id) is unconditional. There is no "if the size bucket is the blob rung" in
// either builder — spec A section 5.1 is explicit that there is no conditional in the
// preimage builder and no special case for ordinary records — and a nil BlobId produces
// the four zero octets by itself, because LP carries no representation for absent. An if
// written here would drop four bytes from every ordinary record's preimage, which two
// implementations can only discover as an aead failure on every record either of them
// sends.
//
// LP(H(server_attachment)) is the hash of the attachment and never the attachment, and H
// is SHA-256 per master section 0's notation line. The absent case is a real decision and
// it is stated here because getting it wrong makes every ordinary record fail the aead
// between two implementations that each believe they read the same spec. Master section 8
// writes LP(H(server_attachment)) with no carve out; spec A section 5.2 says
// "serverAttachment is nil for an ordinary record and MUST then encode zero-length", which
// a reader can take as a claim about this field of the preimage. It is not one. Section
// 5.2's sentence governs the attachment's own encoding — EncodeServerAttachment answers
// zero bytes for an absent attachment rather than a two byte kind 0x0000 — and section
// 5.11's test obligation says why in one line: a zero length attachment and an
// AttachmentNone attachment must encode identically "so H(server_attachment) cannot differ
// between client and server for an ordinary record". That sentence only means anything if
// H is applied to whatever those bytes are, with no carve out of its own. So an absent
// attachment contributes LP(SHA-256("")) — the four octets 00000020 and then
// e3b0c442…7852b855, thirty six bytes in all — and not the four zero octets a zero length
// field would give. aad_test.go pins that vector, and it is the one number in this file
// most worth checking against a second implementation before either ships.
//
// Guardrail G4 of spec A section 5.9 is the sixth, and it is why AADBody does not take a
// *RecordHeader. The comment on BodyBinding says the rest.
package message

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// The two domain separation labels, raw ascii and never length prefixed.
//
// They are the same length in the spec and so they are here, which is why nothing in this
// file may ever compute one from the other or share a prefix constant between them: the
// bytes are the whole of the separation, and a shared constant is one edit away from
// making them equal.
const (
	aadBodyLabel = "URmessage/v1/aad/body"
	aadHeadLabel = "URmessage/v1/aad/head"
)

// The part of a record header aad_body covers, and the whole of what AADBody is given.
//
// This type exists for guardrail G4 of spec A section 5.9, and the guardrail is worth
// restating in full because the type reads as duplication until it is understood.
// body_hash placed in aad_body is circular — the body's own ciphertext hashed into the
// aad the body is sealed under — and the defence spec A names for it is not a comment and
// not a review habit but a signature: "aad_body is built by a function that does not take
// a hash argument". A builder handed the whole header is one line away from the defect at
// every future edit, and the only thing between it and the wire is whoever reads the
// diff. A builder handed these six fields cannot commit the defect at all, because there
// is no hash within its reach to commit it with.
//
// It costs the shape a reader expects. Every other function in this package that takes a
// record's header takes *RecordHeader, and someone arriving at AADBody will wonder why
// this one is different. That difference is the guardrail: the answer to "why does this
// one not take the header" is "because taking the header is the defect", and a reader who
// asks the question has already been told the thing this comment exists to tell them.
//
// The fields are named rather than positional for a smaller reason of the same shape.
// Epoch and StreamIndex are adjacent uint64s, and a builder taking six loose scalars would
// let a call site swap them silently — a swap the record's own encoding cannot see, since
// both fields are in it, and which surfaces only when a second implementation refuses the
// aead.
//
// RecordHeader.BodyBinding is the projection from a full header, and it is the only thing
// in the package that builds one of these from anything wider than these six values.
type BodyBinding struct {
	GroupId      [32]byte
	SenderHandle [16]byte
	Epoch        uint64
	// Monotonic per (group_id, sender_handle) and write once. It is in the aad because a
	// reused index under a reused key is the nonce reuse of guardrail G5, and an aad that
	// did not cover it would leave two records at the same index told apart by nothing.
	StreamIndex uint64
	// The go tag and the eph bucket, which the wire joins into one byte. They are two
	// fields here for the reason they are two fields in RecordHeader, and they are joined
	// for the preimage by the one function in the system that joins them.
	RetentionClass RetentionClass
	EphBucket      uint8
	// t, the eph ladder's time slice, plaintext on the wire and covered here. It is in
	// aad_body as well as aad_head because binding the ZERO a non eph record carries is
	// what stops a body being spliced onto a header of another class, and because the
	// body is the half whose key the window selects.
	EphWindow uint64
}

// BodyBinding projects the six fields aad_body covers out of a full record header.
//
// It is the narrowing G4 asks for, written once here rather than at each call site, so a
// sealer holding a header reaches AADBody through a value with no hash in it rather than
// by picking six fields out by hand — which would leave the property resting on exactly
// the discipline the guardrail says it must not rest on.
func (self *RecordHeader) BodyBinding() BodyBinding {
	return BodyBinding{
		GroupId:        self.GroupId,
		SenderHandle:   self.SenderHandle,
		Epoch:          self.Epoch,
		StreamIndex:    self.StreamIndex,
		RetentionClass: self.RetentionClass,
		EphBucket:      self.EphBucket,
		EphWindow:      self.EphWindow,
	}
}

// AADBody builds the additional authenticated data ct_body is sealed against, from master
// section 8's aad_body block.
//
// It takes a BodyBinding and not a header, and that is guardrail G4 rather than a
// preference; the comment on BodyBinding says why at length. It takes the binding by value
// rather than by pointer for a much smaller reason: there is then no nil to refuse, and
// one fewer failure mode in a function whose whole job is to be byte exact.
//
// The only refusal it can reach is the join's. A class and bucket pair the wire has no
// byte for has no aad either, and manufacturing one would seal a record under a preimage
// no reader — the sender's own other devices included — will ever reconstruct.
func AADBody(algId uint16, binding BodyBinding) ([]byte, error) {
	retentionWire, err := RetentionClassWire(binding.RetentionClass, binding.EphBucket)
	if err != nil {
		return nil, err
	}
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(aadBodyLabel))
	writer.WriteUint16(algId)
	writer.WriteOpaqueLP(binding.GroupId[:])
	writer.WriteOpaqueLP(binding.SenderHandle[:])
	writer.WriteUint64(binding.Epoch)
	writer.WriteUint64(binding.StreamIndex)
	writer.WriteUint8(retentionWire)
	// unconditional on every class, and a non eph record writes eight zero octets. Master
	// section 8 forbids the conditional a reader's instinct puts here; the file comment
	// says what the conditional costs.
	writer.WriteUint64(binding.EphWindow)
	// the writer is sticky: the first failure latches and every later call is a no op, so
	// this is the one place the build is asked whether it worked.
	return writer.Bytes()
}

// AADHead builds the additional authenticated data ct_head is sealed against, from master
// section 8's aad_head block as amended by the server attachment ruling of spec B section
// 5.4.
//
// It covers every field of RecordHeader, which is master invariant I6 — the server acts
// only on values it can verify, and every header field is one the server can see — and
// aad_test.go holds it to that by walking the struct rather than by listing the fields.
//
// The attachment arrives as its own argument and also sits on the header, and the two must
// agree. Two arguments beside the header is the shape spec A section 12.1 publishes for
// the sibling preimage WriteAuthPreimage, so the two preimages take the attachment from
// the same place and cannot disagree about one record. But a second source for one value
// is a second thing to get wrong: a caller that passes nil while the header carries an
// attachment seals ct_head under a preimage nothing will reproduce, and it is a plausible
// call, because Record.Header.ServerAttachment is right there and forgetting it costs
// nothing at compile time. So the disagreement is refused here rather than resolved, and
// it is refused in both directions, with a nil and an empty attachment treated as the one
// value LP cannot tell apart anyway.
//
// H(server_attachment) is SHA-256 of the attachment's encoded bytes, and of the empty
// string when there is no attachment. The file comment states that resolution and the
// ambiguity behind it in full; it is the one decision here that cannot be found by reading
// this function.
func AADHead(algId uint16, h *RecordHeader, serverAttachment []byte) ([]byte, error) {
	if h == nil {
		return nil, ErrRecordHeaderNil
	}
	// subtle.ConstantTimeCompare rather than bytes.Equal, for the reason writeauth.go's
	// sibling check gives: guardrail G8's mechanical half is a file scoped ban on bytes.Equal
	// and a ban with one exemption written into it is a ban with a judgement call in front of
	// it. The answer is the same one — equal lengths compared whole, so a nil and an empty
	// attachment remain the one value LP cannot tell apart.
	if subtle.ConstantTimeCompare(h.ServerAttachment, serverAttachment) != 1 {
		return nil, fmt.Errorf("%w: the header carries %d bytes and the argument carries %d",
			ErrServerAttachmentMismatch, len(h.ServerAttachment), len(serverAttachment))
	}
	retentionWire, err := RetentionClassWire(h.RetentionClass, h.EphBucket)
	if err != nil {
		return nil, err
	}
	// the hash of the attachment and never the attachment. sha256.Sum256 of a nil slice is
	// the hash of the empty string, which is exactly the absent case the file comment
	// resolves, so the ordinary record needs no branch of its own here either.
	attachmentHash := sha256.Sum256(serverAttachment)
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(aadHeadLabel))
	writer.WriteUint16(algId)
	writer.WriteOpaqueLP(h.GroupId[:])
	writer.WriteOpaqueLP(h.SenderHandle[:])
	writer.WriteUint64(h.Epoch)
	writer.WriteUint64(h.StreamIndex)
	writer.WriteUint8(isCommitByte(h.IsCommit))
	writer.WriteUint8(retentionWire)
	// unconditional, as in aad_body and for the same reason.
	writer.WriteUint64(h.EphWindow)
	writer.WriteUint8(byte(h.SizeBucket))
	writer.WriteUint64(h.ExpireAt)
	writer.WriteOpaqueLP(h.BodyHash[:])
	// unconditional, and a nil BlobId writes the four zero octets. spec A section 5.1
	// forbids the conditional a reader's instinct puts here; the file comment says what
	// the conditional costs.
	writer.WriteOpaqueLP(h.BlobId)
	writer.WriteOpaqueLP(attachmentHash[:])
	return writer.Bytes()
}
