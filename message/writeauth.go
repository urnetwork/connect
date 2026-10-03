// The two message server authenticators of spec A section 5.7: the mac a record carries
// on the write path, and the mac a request carries on the read path. The keys they run
// under are derived here too, because the label is the only thing that separates them and
// a label belongs beside the thing it separates.
//
// Master section 9.2 is normative and spec A section 5.7 restates it; the two agree, and
// the block below is theirs:
//
//	write_auth = MAC(write_key, "URmessage/v1/write" ‖ LP(server_nonce) ‖ LP(group_id)
//	           ‖ LP(sender_handle) ‖ u64(epoch) ‖ u64(stream_index) ‖ u8(is_commit)
//	           ‖ u8(retention_class) ‖ u64(eph_window) ‖ u8(size_bucket)
//	           ‖ u64(expire_at) ‖ LP(H(ct_head)) ‖ LP(body_hash) ‖ LP(blob_id)
//	           ‖ LP(H(server_attachment)))
//
//	req_auth   = MAC(read_key, "URmessage/v1/req" ‖ LP(server_nonce) ‖ u8(op)
//	           ‖ LP(request_bytes))
//
// MAC is HMAC-SHA-256 and H is SHA-256, both from master section 0's notation line, and
// the tag is the full thirty two octets rather than a truncation.
//
// Most of what is easy to get wrong here is what was easy to get wrong in aad.go, and that
// file's comment argues each of them at length rather than this one repeating it: the label
// is raw ascii and never length prefixed; LP is WriteOpaqueLP, a fixed thirty two bit big
// endian length, and never WriteOpaque's mls varint; u8(retention_class) is the joined wire
// byte of master section 8's table and not the go tag, so it comes from RetentionClassWire
// and from nowhere else; LP(blob_id) is unconditional and a nil blob id writes the four
// zero octets; and LP(H(server_attachment)) is the hash of the attachment with no carve out
// for the absent case, so an ordinary record contributes LP(SHA-256("")) and not an empty
// field.
//
// u64(eph_window) was added on 2026-09-13 (ledger 152 / 183, m1 open item M1-27),
// immediately after u8(retention_class), the field it qualifies, and unconditional at its
// full width on every class exactly as in both aads. THIS IS THE LOAD BEARING ONE OF THE
// THREE PLACEMENTS, and the reason is master invariant I6: the SERVER ACTS ON THIS FIELD.
// Spec B section 5.1 check 3 and section 7.1 refuse an eph 1..5 record whose window is
// more than one window from the window its own arrival stamp falls in, and the term here
// is the whole of what makes that a check on a value the mac covers rather than on a field
// anyone in the path may rewrite. The aad terms are defence in depth and the key
// derivation is what stops a downgrade; neither substitutes for this one.
//
// Five things are this file's own.
//
// LP(H(ct_head)) is the hash of ct_head and never ct_head. It is the field a reader is most
// likely to write straight, because ct_head is right there in the record and hashing it
// looks like a step that could be skipped. It cannot: two implementations that disagree
// about it agree on every field boundary, produce preimages of different lengths, and
// discover it as a rejected write on every record either of them sends. The vector in
// writeauth_test.go pins the field at thirty six octets over a ct_head of ninety six, so a
// builder that wrote the ciphertext itself fails on the length before it fails on the bytes.
//
// record_id is deliberately absent, and it is absent from both aads as well. Spec A section
// 5.1 settles it: the id is assigned by the server after acceptance, which is after
// write_auth has been computed and verified, so there is nothing to authenticate at the
// moment the mac is taken and nothing a client could reproduce afterwards. It is pagination
// and hole detection only, it is never authenticated, and a preimage that carried it would
// be a preimage the server could not rebuild on the read path — where spec A section 2.4
// has it rebuild record_bytes from its stored columns with write_auth left zero precisely
// because the mac cannot be reconstructed at all.
//
// alg_id is deliberately absent too, and this is the one place where these preimages and
// the two aads of aad.go differ in shape rather than in content. Both aads carry u16(alg_id)
// because master section 7.1 wants the algorithm identifier inside the bytes the aead
// authenticates, where it cannot be stripped or downgraded. Neither mac block above carries
// it, in either spec text, and it is not an oversight to be tidied: the mac here is
// HMAC-SHA-256 fixed by this layer rather than negotiated, and a field written into the
// preimage that no other implementation writes is a field that fails every record. Do not
// add it.
//
// The nonce is refused when it is empty, which is spec A section 5.7's own sentence: this
// layer takes the nonce as an opaque byte string and refuses to compute with an empty one.
// How it refuses is a decision the published signatures force, and it is split. Nothing here
// hands back a zero tag: a caller that mistook one for a real authenticator would put a
// record on the wire under a mac every other record with a missing nonce also carries, which
// is guardrail G7 of spec A section 5.9 arriving as a value rather than as a log line. So
// the computing half — WriteAuthPreimage, ComputeWriteAuth, RequestAuthPreimage,
// ComputeRequestAuth — panics, with the sentinel as the panic value so a caller that
// recovers can name what it caught. The verifying half answers false and never panics.
//
// That split is not squeamishness about panics; it is which side of the wire each half is
// on. Computing is the client sealing its own record against a nonce its own connection was
// handed in HelloResponse, so an empty one is a lifecycle bug in the caller and cannot
// arrive from the network. Verifying is the server, and the server's nonce is connection
// state: a client that sends a Submit before its Hello reaches a connection whose nonce has
// not been issued, so an empty nonce on the verifying side is remotely reachable and a panic
// there would be a client that can stop the process. Spec A section 5.7 exports
// VerifyWriteAuth for spec B and says it is the only authentication the server performs on
// the write path, which is the same division read from the other end.
//
// A key that is not exactly thirty two octets is refused the same way, and for a sharper
// reason than tidiness. HMAC accepts a key of any length, the empty one included, so a
// server that looked up a missing write key and passed the nil it got back would compute a
// mac under the empty key — and a client that had also derived nothing would compute the
// same one. Two ends holding no key at all would authenticate. Master section 8.3 gives
// write_key and read_key as exactly thirty two octets on the wire and both derivations below
// produce exactly that, so a key of any other length is a bug rather than a shorter key, and
// refusing it closes the bypass rather than narrowing it.
//
// Guardrail G8 of spec A section 5.9 governs the last of it: every tag comparison goes
// through crypto/subtle.ConstantTimeCompare, and the guardrail's mechanical half bans
// bytes.Equal in this file — the file and not a kind of function. Nothing here compares two
// byte strings by any other means, the attachment check included, and that is not zeal about
// a comparison that is neither secret nor a tag: a file scoped ban with one exemption in it
// is a ban every later reader has to re-derive, and the exemption is where the next
// comparison hides. The gates in writeauth_test.go read the syntax tree for it rather than
// trusting the diff, over classes they compute from the tree — the functions they judge, and
// the comparators they ban, both — rather than over lists written down here.
package message

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// The two domain separation labels, raw ascii and never length prefixed.
//
// Unlike the pair in aad.go these are not the same length — eighteen octets against sixteen
// — and the separation still does not rest on that. It rests on the bytes: the two disagree
// at index thirteen, 'w' against 'r', which is inside the shorter of them, so no choice of
// anything that follows either label can make one preimage into the other. The test that
// tries it says so by construction rather than by arithmetic.
//
// Nothing here may ever compute one label from the other or share a prefix constant between
// them, for the reason aad.go gives at length: a shared constant is one edit away from
// making the two agree, and a preimage that agrees with the other protocol's is a preimage
// that can be replayed into it.
const (
	writeAuthLabel   = "URmessage/v1/write"
	requestAuthLabel = "URmessage/v1/req"
)

// The two hkdf info strings, which are the whole of what separates the write key from the
// read key: both are expanded from the same storage_root[n], to the same length, with the
// same hash. Swap them and every derivation still returns thirty two plausible octets, the
// write path and the read path each still agree with themselves, and the only observation
// that fails is one against a second implementation — which is why the vectors in
// writeauth_test.go pin both against RFC 5869's own arithmetic rather than against this
// package's output.
//
// They are two constants rather than a shared construction with the half word substituted,
// for the reason the labels above are: one construction is a single edit away from making
// the two keys equal.
const (
	writeKeyInfo = "write/v1"
	readKeyInfo  = "read/v1"
)

// The width of every key and every tag on this file's surface, in octets. Master section 8.3
// gives write_key and read_key as exactly this on the wire, and the mac is HMAC-SHA-256
// taken whole rather than truncated.
const (
	authKeyBytes = 32
	authTagBytes = 32
)

// WriteKey derives the group's per epoch write key, HKDF-Expand(storage_root[n], "write/v1",
// 32) of master section 9.2.
//
// It is one key per group per epoch, which is all the server needs: master section 9.2 is
// explicit that a group wide key tells the server only "a current member of this group", and
// that authenticity is mls's job at every client no matter what the server accepts. The
// server holds this key itself, delivered inside the commit's EpochAttachment, so it can
// forge a tag under it — master section 9.2 states that consequence rather than hiding it —
// and the key must therefore never be reused for any second purpose beyond write_auth.
func WriteKey(storageRoot []byte) []byte {
	return expandAuthKey(storageRoot, writeKeyInfo)
}

// ReadKey derives the group's per epoch read key, HKDF-Expand(storage_root[n], "read/v1",
// 32) of master section 9.2.
//
// It is deliberately not the epoch's write key. The server keeps the current epoch's write
// key and one sixty second predecessor and nothing older, so a member offline across a
// single commit holds a write key the server cannot resolve — and every route out of that
// condition is itself a read: GroupStatus to learn the epoch, Fetch to obtain the commits,
// WrapFetch to obtain its own wrap. The read key breaks that cycle, which is why its
// retention window is measured in months rather than seconds, and it is why nothing in this
// package has a path that macs a request under a write key.
func ReadKey(storageRootEpoch []byte) []byte {
	return expandAuthKey(storageRootEpoch, readKeyInfo)
}

// The one expansion both derivations go through.
//
// hkdf.Expand's info argument is typed string and is not text; the conversion at the two
// call sites above is byte preserving. The error it can return comes from the fips140-only
// mode check, which refuses an unapproved hash or a short secret; sha256 is approved, so
// under this package's build only a storage root shorter than fourteen octets can reach it,
// and a storage root that short is a caller that derived every group key from nothing. It is
// a panic rather than an ignored error because the published signature has no error to
// return and the alternative is handing back a key derived from a secret the caller does not
// have.
//
// It is hkdf.Expand and never hkdf.Extract. Guardrail G1 of spec A section 5.9 confines
// Extract to one function because its argument order is the reverse of every spec text in
// this project, and the gate in mls/crypto_forbidden_test.go reads this package for it.
func expandAuthKey(storageRoot []byte, info string) []byte {
	key, err := hkdf.Expand(sha256.New, storageRoot, info, authKeyBytes)
	if err != nil {
		panic(fmt.Errorf("message: hkdf expand of an auth key failed with a compiled-in sha256: %w", err))
	}
	return key
}

// WriteAuthPreimage builds the bytes write_auth is taken over, from master section 9.2's
// block.
//
// It covers every field of RecordHeader, which is master invariant I6 — the server acts only
// on values it can verify — and writeauth_test.go holds it to that by walking the struct
// rather than by listing the fields.
//
// The attachment arrives as its own argument and also sits on the header, exactly as it does
// for AADHead, and the two must agree for the same reason: a caller that passes nil while
// the header carries an attachment macs a record under a preimage the server will not
// reproduce, and it is a plausible call because Record.Header.ServerAttachment is right
// there. The disagreement is refused rather than resolved, in both directions, with a nil
// and an empty attachment treated as the one value LP cannot tell apart anyway.
//
// It panics rather than reporting, because the published signature of spec A section 12.1
// has no error to report through and the alternative is worse than a panic. A builder that
// answered nil on a refusal would hand ComputeWriteAuth an empty preimage, and every record
// that took that path — every record with an empty nonce, or with a class and bucket pair
// the wire has no byte for — would carry the same mac under the same key. The file comment
// argues the division between this half and the verifying half at length.
//
// What it panics on, since neither spec says and a published function that panics owes its
// caller the list: ErrRecordHeaderNil for no header, ErrServerNonceEmpty for a nonce of no
// octets, ErrServerAttachmentMismatch for an attachment argument that disagrees with the
// header's, and the join's own ErrEphBucketOnNonEphClass, ErrEphBucketOutOfRange or
// ErrRetentionClassUnknown for a class and bucket pair the wire has no octet for. The panic
// value is the wrapped sentinel in every case, so a caller that recovers names what it caught
// with errors.Is instead of matching on message text — which is guardrail G7 of spec A section
// 5.9 read through a panic rather than through a return.
func WriteAuthPreimage(serverNonce []byte, h *RecordHeader, ctHead []byte, serverAttachment []byte) []byte {
	preimage, err := writeAuthPreimage(serverNonce, h, ctHead, serverAttachment)
	if err != nil {
		panic(fmt.Errorf("message: the write_auth preimage cannot be built: %w", err))
	}
	return preimage
}

// The reporting half of the builder above, and the one every verifier uses.
//
// It exists because VerifyWriteAuth is reachable from the network and must answer false
// rather than stop the process. Everything that can refuse refuses here, once, so the two
// halves cannot disagree about which inputs have a preimage at all.
func writeAuthPreimage(serverNonce []byte, h *RecordHeader, ctHead []byte, serverAttachment []byte) ([]byte, error) {
	if h == nil {
		return nil, ErrRecordHeaderNil
	}
	if len(serverNonce) == 0 {
		return nil, ErrServerNonceEmpty
	}
	// subtle.ConstantTimeCompare rather than bytes.Equal, and the attachment is not a tag.
	// Guardrail G8 of spec A section 5.9 bans bytes.Equal in this file rather than in a kind
	// of function, and a file scoped ban that the file itself breaks once is a ban with a
	// judgement call in front of it — every later reader has to decide whether this
	// comparison is the exempt kind. It answers the same question here: the lengths are
	// public and equal length inputs are compared whole, so nil and empty stay the one value
	// LP cannot tell apart, which is what bytes.Equal answered too.
	if subtle.ConstantTimeCompare(h.ServerAttachment, serverAttachment) != 1 {
		return nil, fmt.Errorf("%w: the header carries %d bytes and the argument carries %d",
			ErrServerAttachmentMismatch, len(h.ServerAttachment), len(serverAttachment))
	}
	retentionWire, err := RetentionClassWire(h.RetentionClass, h.EphBucket)
	if err != nil {
		return nil, err
	}
	// the hash of ct_head and never ct_head, and the hash of the attachment and never the
	// attachment. sha256.Sum256 of a nil slice is the hash of the empty string, which is the
	// absent attachment aad.go's comment resolves, so the ordinary record needs no branch.
	ctHeadHash := sha256.Sum256(ctHead)
	attachmentHash := sha256.Sum256(serverAttachment)
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(writeAuthLabel))
	writer.WriteOpaqueLP(serverNonce)
	writer.WriteOpaqueLP(h.GroupId[:])
	writer.WriteOpaqueLP(h.SenderHandle[:])
	writer.WriteUint64(h.Epoch)
	writer.WriteUint64(h.StreamIndex)
	writer.WriteUint8(isCommitByte(h.IsCommit))
	writer.WriteUint8(retentionWire)
	// unconditional on every class, and a non eph record writes eight zero octets, for the
	// reason LP(blob_id) below is written on a record that has no blob.
	writer.WriteUint64(h.EphWindow)
	writer.WriteUint8(byte(h.SizeBucket))
	writer.WriteUint64(h.ExpireAt)
	writer.WriteOpaqueLP(ctHeadHash[:])
	writer.WriteOpaqueLP(h.BodyHash[:])
	// unconditional, and a nil BlobId writes the four zero octets. spec A section 5.1 forbids
	// the conditional a reader's instinct puts here.
	writer.WriteOpaqueLP(h.BlobId)
	writer.WriteOpaqueLP(attachmentHash[:])
	// the writer is sticky: the first failure latches and every later call is a no op, so
	// this is the one place the build is asked whether it worked.
	return writer.Bytes()
}

// ComputeWriteAuth takes the record's mac under the group's epoch write key.
//
// It is the last step of the construction order spec A section 5.2 makes a type: the body is
// sealed, body_hash is taken, the header is complete, ct_head is sealed, and only then is
// there a preimage to mac. Every dependency is acyclic and this is the end of the chain.
//
// It panics on everything WriteAuthPreimage panics on, which its comment lists, and on one
// more of its own: ErrAuthKeyLength for a write key that is not thirty two octets, which a
// lookup that missed and answered nil arrives at. The panic value is the wrapped sentinel.
func ComputeWriteAuth(writeKey []byte, serverNonce []byte, h *RecordHeader, ctHead []byte, serverAttachment []byte) [32]byte {
	return mustAuthTag(writeKey, WriteAuthPreimage(serverNonce, h, ctHead, serverAttachment))
}

// VerifyWriteAuth answers whether a record's write_auth is the mac the group's epoch write
// key takes over it, in constant time.
//
// It is exported for spec B and it is the only authentication the server performs on the
// write path. Per master invariant I5 it is access control and never authenticity: a forged
// record fails mls verification at every client no matter what the server accepts.
//
// It answers false rather than panicking on every refusal the builder can reach, and false
// rather than true on a record whose write_auth is all zero. That last case is not
// hypothetical and it is not a corrupt record: spec A section 2.4 has the server rebuild
// record_bytes from its stored columns on every read with write_auth left zero always,
// because the mac is over the submitting connection's nonce and there is nothing left to
// reconstruct it from. A client that treated the zero it reads back as evidence of anything
// would be treating "the server did not keep this" as "the server checked this". There is no
// special case for it below and there must not be one: the zero fails because the mac of a
// real preimage under a real key is not zero, which is the same reason every other wrong tag
// fails, and a special case would be a second rule a reader has to trust.
func VerifyWriteAuth(writeKey []byte, serverNonce []byte, r *Record) bool {
	if r == nil {
		return false
	}
	preimage, err := writeAuthPreimage(serverNonce, &r.Header, r.CtHead, r.Header.ServerAttachment)
	if err != nil {
		return false
	}
	tag, err := authTag(writeKey, preimage)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(tag[:], r.WriteAuth[:]) == 1
}

// RequestAuthPreimage builds the bytes req_auth is taken over, from master section 9.2's
// block.
//
// op is the field number of the selected oneof body arm in MessageServerRequest as a u8 —
// 13, 14, 16, 17 and 19 are the five arms that carry a req_auth — and requestBytes is the
// deterministically marshaled request body with its own req_auth field set to zero length.
// Both are the caller's to produce: this package encodes no protobuf and knows nothing about
// the request beyond its octets.
//
// The epoch whose read key computed the mac is not an argument here. It travels in the
// request's own read_epoch field, which is inside requestBytes and therefore inside the mac,
// so the server selects a key by an authenticated value rather than by a hint beside one.
//
// It panics on a refusal for the reason WriteAuthPreimage does, and the file comment argues
// it. There is one refusal here — ErrServerNonceEmpty, for a nonce of no octets — and the panic
// value is the wrapped sentinel, so a caller that recovers can name it with errors.Is.
func RequestAuthPreimage(serverNonce []byte, op uint8, requestBytes []byte) []byte {
	preimage, err := requestAuthPreimage(serverNonce, op, requestBytes)
	if err != nil {
		panic(fmt.Errorf("message: the req_auth preimage cannot be built: %w", err))
	}
	return preimage
}

// The reporting half, for VerifyRequestAuth, which must answer false rather than stop the
// process on anything a connection can arrive in.
func requestAuthPreimage(serverNonce []byte, op uint8, requestBytes []byte) ([]byte, error) {
	if len(serverNonce) == 0 {
		return nil, ErrServerNonceEmpty
	}
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(requestAuthLabel))
	writer.WriteOpaqueLP(serverNonce)
	writer.WriteUint8(op)
	// a request body that marshals to nothing writes the four zero octets, which is a legal
	// request rather than a missing one: a protobuf message holding only defaults has no
	// bytes, and LP has no representation for absent to tell it apart from one.
	writer.WriteOpaqueLP(requestBytes)
	return writer.Bytes()
}

// ComputeRequestAuth takes a request's mac under the epoch's read key.
//
// The key is the argument and this function derives nothing. There is no path from here to
// WriteKey and there must never be one — a request macd under an epoch write key is a
// request the server cannot resolve for any member that was offline across a commit for more
// than sixty seconds, which is every member the read key exists for.
// TestReadAuthNeverUsesWriteKey walks the call graph from here and asserts it.
//
// It panics on an empty nonce with ErrServerNonceEmpty and on a read key that is not thirty two
// octets with ErrAuthKeyLength, in both cases with the wrapped sentinel as the panic value.
func ComputeRequestAuth(readKey []byte, serverNonce []byte, op uint8, requestBytes []byte) [32]byte {
	return mustAuthTag(readKey, RequestAuthPreimage(serverNonce, op, requestBytes))
}

// VerifyRequestAuth answers whether a request's req_auth is the mac the epoch's read key
// takes over it, in constant time.
//
// auth is the tag as it arrived, of whatever length it arrived in.
// crypto/subtle.ConstantTimeCompare answers zero for a length mismatch, so a truncated tag is
// refused by the comparison itself rather than by a length check in front of it — and the
// lengths are public, so answering on them early leaks nothing.
func VerifyRequestAuth(readKey []byte, serverNonce []byte, op uint8, requestBytes []byte, auth []byte) bool {
	preimage, err := requestAuthPreimage(serverNonce, op, requestBytes)
	if err != nil {
		return false
	}
	tag, err := authTag(readKey, preimage)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(tag[:], auth) == 1
}

// The mac itself: HMAC-SHA-256 over the preimage, taken whole.
//
// The key length is checked here rather than at each call site, and it is checked at all for
// the reason the file comment gives: HMAC takes a key of any length including the empty one,
// so two ends that both hold nothing would otherwise agree.
//
// The conversion of the sum to an array rather than a copy into one is deliberate. A copy
// into a destination of the wrong width is silent; the conversion panics on a sum that is not
// thirty two octets, which can only happen if the hash under it changed.
func authTag(key []byte, preimage []byte) ([authTagBytes]byte, error) {
	if len(key) != authKeyBytes {
		return [authTagBytes]byte{}, fmt.Errorf("%w: %d octets, want %d", ErrAuthKeyLength, len(key), authKeyBytes)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(preimage)
	return [authTagBytes]byte(mac.Sum(nil)), nil
}

// The computing half's mac, which panics on the refusal the verifying half answers false to.
// The panic value is the wrapped sentinel rather than a string, so a caller that recovers can
// name what it caught with errors.Is instead of matching on message text.
func mustAuthTag(key []byte, preimage []byte) [authTagBytes]byte {
	tag, err := authTag(key, preimage)
	if err != nil {
		panic(fmt.Errorf("message: the authenticator cannot be computed: %w", err))
	}
	return tag
}
