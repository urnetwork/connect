// The device wrap's crypto door: env_key[k], and the X-Wing envelope MASTER section 7 fixes.
//
// WHAT THIS FILE IS AND WHAT IT IS NOT. m1 task 14 builds the device wrap, and the device wrap is
// four things: an envelope key, a KEM door, a signature over the body, and a fan-out that emits
// two records per active device leaf and counts them. This file is the first two. The third is
// task 14 step 3 and is BLOCKED -- open item M1-52 leaves the signature preimage at 1,320 or 1,356
// octets and no document says which -- and the fourth is task 15's. So the seal here takes a
// PAYLOAD and does not build it: what goes inside aead_ct is
// secret | LP(identity_pub) | sig by MASTER section 7, and this file seals whichever octets its
// caller hands it. That is not a shortcut around the signature; it is the seam that lets the
// signature land later without this door moving, and it is also the seam m1 task 14 property 9
// requires in as many words -- "the sealing side is reachable with an envelope the caller
// chooses", because a property about what a signature covers cannot be written against a door
// that accepts only what its own sealer constructed.
//
// THREE OF wrap_key's NINE INPUTS HAVE NO CODE POINT AND THIS FILE DOES NOT INVENT THEM. MASTER
// section 7 names them itself, twice, and the second time to say the domain is not the encoding:
// target_id is "defined nowhere in the corpus" with four candidate byte strings, and
// u8(target_type) and u8(payload_type) arrived with red-team M-15 and "have no code point
// anywhere". MASTER's own sentence is that the block "is normative modulo those three" and "a
// second implementation cannot build a wrap from it alone". So every one of the three is a
// PARAMETER of the two doors below, with no default, no package constant and no fallback. A
// constant here would be this implementation choosing a wire value the spec has not, which is the
// one failure mode MASTER states for them: "a publisher and a restorer that choose differently
// produce a wrap nobody can open, with no error anywhere". Open item MG-7 carries it.
//
// WHERE THE CIRCULARITY IS BROKEN, because it is the whole reason env_key exists. A wrap is an
// ordinary record and an ordinary record's ct_body is sealed under a record_key descending from
// storage_root[n] -- which is the value the wrap exists to deliver. MASTER section 8.2 breaks that
// once per wrap kind, and the break for the device wrap is one new exporter label:
//
//	env_key[k] = MLS-Exporter("URmessage/v1/envelope", "", 32)          RFC 9420 section 8.5
//	record_key[0] = HKDF-Expand(env_key[k], "sender/v1" | LP(leaf_index), 32)
//
// It is not circular -- env_key[k] descends from the MLS key schedule and from no storage_root --
// and it is chosen for the one property nothing else has: it is the only outer key that neither
// the message server nor a member REMOVED by the commit that opened epoch k can derive, while a
// member the commit ADDED holds it the moment it joins. WrapRecordKeyZero below is that one line,
// and wrap_test.go holds the structural half: nothing in this file reaches DeriveClassKeys or
// StorageRoot, because an edge from here into either is exactly the circularity the ruling exists
// to remove.
//
// THE TWO SECRETS ARE POST-QUANTUM AND THE MLS EXPORTER IS NOT, WHICH IS WHY THE KEM IS HERE AT
// ALL. Measured at this commit rather than quoted: connect/mls's HPKE is hard-wired to X25519 --
// hpkeEncap calls X25519GenerateKey and hpkeEncapDeterministic calls X25519PrivateKey,
// X25519PublicKey and X25519DH; hpkeDecap the same -- and SuiteParams.KemId is READ at exactly
// two sites, hpke.go:58 and :69, both of them inside hpkeKemSuiteId building an RFC 9180 suite_id
// string, with its only other occurrences being its own declaration and the two suite
// registrations. So it is a registry LABEL the implementation never dispatches on, and both
// registered suites name HpkeKemX25519HkdfSha256. crypto/mlkem occurs in mls's production source
// in one comment and nowhere else. So env_key[k] carries no post-quantum contribution of any kind, and a wrap sealed
// under it alone would leave pq_secret protected by the same X25519 handshake a harvesting
// adversary already holds. The X-Wing encapsulation below is what makes the delivery
// post-quantum, and it is the reason ledger ruling 36 refused every cheaper rotation shape.
//
// THE ENVELOPE'S CACHING OBLIGATION IS REAL AND IS NOT DISCHARGED HERE, AND MASTER'S STATEMENT OF
// WHAT IT COSTS IS TOO STRONG OF THIS BUILD. env_key[k] is computable off the LIVE handle only
// while the group stands at epoch k: (*Group).Export reads the current schedule and this tree
// declares no ExportAt. MASTER section 8.2 concludes from that that a client which misses the
// window "cannot recover storage_root[k]", full stop, and Spec A section 5.11 prices the opener's
// typed failure as unrecoverable.
//
// THAT IS NOT WHAT THIS TREE DOES, and the correction is ledger item 251's: a PUBLISHED API does
// reach a past epoch's exporter. GroupEngine.LoadGroup(groupId, k) rebuilds a whole epoch k
// GroupHandle out of the state blob connect/mls persists at every merge, and Export on THAT handle
// is epoch k's exporter -- which is pastepoch.go's entire mechanism, built for the same reason one
// level down. So the cost of missing the window is bounded by PastEpochWindow rather than
// permanent, the typed failure an opener owes is "outside the retained window" and not
// "unrecoverable", and a builder reading MASTER's sentence alone would price this thirty two
// epochs too pessimistically. Neither the cache nor that failure is built here: the cache belongs
// to the session and to the sdk step, and what this file provides is the two doors it is filled
// through -- Export for an epoch the group has entered, and PendingExport for the epoch a staged
// commit opens, which ledger ruling 37 requires to be computable BEFORE the merge and to equal
// what Export answers after it.
package messagegroup

import (
	"fmt"
	"io"

	"github.com/urnetwork/connect/mls/syntax"
)

// EnvKeyBytes is the width MASTER section 8.2 exports env_key at.
//
// It is the head of the device wrap's record ladder, so it is a class key by position and is
// refused at the class key width by RecordKeyZero below, which is why the two numbers are
// asserted equal in wrap_test.go rather than written once and hoped over.
const EnvKeyBytes = 32

// The salt and the label of MASTER section 7's wrap KDF, transcribed.
//
//	prk = HKDF-Extract(salt = "URmessage/v1/wrap-salt", ikm = ss)
//	wrap_key | wrap_nonce = HKDF-Expand(prk, info, 56)
//	info = "URmessage/v1/wrap" | LP(group_id) | u64(epoch) | u8(target_type) | LP(target_id)
//	       | u8(payload_type) | u16(alg_id) | LP(target_xwing_pub) | LP(ct_xwing)
//
// THEY ARE A PREFIX PAIR AND THE RELATION IS DISPOSITIONED RATHER THAN AVOIDED. "URmessage/v1/wrap"
// is the WHOLE of "URmessage/v1/wrap-salt", which is the relation this package's label rule
// refuses, and both strings are MASTER's -- so the pair is here because the wire is normative and
// the rule is connect's own stricter one, exactly as recordAeadHeadInfo and recordHeadBindInfo
// already are. What makes it safe is an argument about HKDF and not about care: the salt is
// Extract's SALT ARGUMENT, which is the HMAC key of HMAC(salt, ikm), and the label is the first
// seventeen octets of Expand's INFO, which is HMAC(prk, info | 0x01) -- two different functions
// with the value in two different positions, so no truncation of one produces the other's output.
// And the info label is never a complete info: WrapInfo always continues with LP(group_id), so the
// bare seventeen octets are not a string this package can ever expand under. wrap_test.go asserts
// both halves and holds the pair against a written-down disposition in both directions.
const (
	wrapSaltLabel = "URmessage/v1/wrap-salt"
	wrapInfoLabel = "URmessage/v1/wrap"
)

// The width of wrap_key | wrap_nonce, derived and never written.
//
// MASTER section 7's block gives 56 and says what the split is; 56 is the aead's key size plus the
// extended nonce XChaCha20-Poly1305 takes, which is the same sum recordAeadMaterialBytes is
// written as one file over and for the same reason: a suite change that moved either half would
// otherwise truncate the nonce and every wrap would still round trip against itself.
const wrapAeadMaterialBytes = recordAeadKeyBytes + recordAeadNonceBytes

// WrapFormatVersion is the octet MASTER section 7 puts FIRST in every wrap envelope.
//
// The version is first for the reason every offset below it is meaningful only under that
// version. WHAT AN OPENER DOES WITH AN UNRECOGNISED ONE IS NOT RULED -- m1 open item M1-54 -- and
// this door does not rule it either: OpenWrapBody carries the octet out to its caller and refuses
// nothing on it, and its signature gives a caller no parameter to ask about it with -- an opener's
// comparison over an octet no ruling reaches would be this package ruling M1-54 quietly. That
// choice is not free and it is not hidden. The version octet is the one field
// of the eleven that MASTER's nine-element info does NOT bind, so until task 14 step 3's signature
// lands there is no authority over it at all: a wrap whose version octet has been changed on the
// wire opens to exactly the payload it carried. That is measured in wrap_test.go, pinned in
// testdata/envelope-wrap-kat.txt as the failing-direction vector, and filed as open item MG-7.
const WrapFormatVersion uint8 = 0x01

// WrapEnvelopeBytes is the eleven octets MASTER section 7 fixes: one version, two type octets and
// a u64 content epoch, in that order, OUTSIDE hybrid_ct.
const WrapEnvelopeBytes = 1 + 1 + 1 + 8

// WrapEnvelope is the cleartext head of every wrap body -- the pq_secret device wrap, the eph_root
// device wrap and the recovery wrap, with no exception for any of them.
//
// IT DECLARES NO OCTETS AND HOLDS NO SECRET, which is why it owes no erase: every field is a wire
// value a parser recovers from the body it was handed, and the body travels to the message server.
//
// u32(publisher_leaf_index) IS DELIBERATELY NOT A FIELD. MASTER section 7 argues it out: a member
// resolves the publisher from sender_handle, which is already in record_bytes in the clear; a
// seed-only restorer cannot resolve a bare leaf index at all, holding no ratchet tree; and on the
// recovery wrap, whose body is under no record AEAD, four cleartext octets would convert a per-leaf
// pseudonym the server already sees into a TREE POSITION. A field here would put it back.
//
// TargetType and PayloadType HAVE NO CODE POINT IN ANY DOCUMENT and this type assigns none. They
// are u8 because MASTER says u8; which octet a device leaf takes, which a member's RECOVERY_PUB
// takes, which a pq_secret payload takes and which an eph_root payload takes are four wire
// decisions nobody has made. See the file header and open item MG-7.
type WrapEnvelope struct {
	FormatVersion uint8
	TargetType    uint8
	PayloadType   uint8
	ContentEpoch  uint64
}

// Encode writes the eleven octets, version first.
//
// There is no error to return: every field is a fixed width integer and the writer cannot fail on
// one. The assertion that the width is eleven lives in wrap_test.go rather than here, because a
// check of a constant against itself passes against any drift at all.
func (self WrapEnvelope) Encode() []byte {
	writer := syntax.NewWriter()
	writer.WriteUint8(self.FormatVersion)
	writer.WriteUint8(self.TargetType)
	writer.WriteUint8(self.PayloadType)
	writer.WriteUint64(self.ContentEpoch)
	encoded, err := writer.Bytes()
	if err != nil {
		// unreachable: four fixed width integer writes, no vector and no length check. A panic
		// rather than a second route to a wire sentinel, because anything arriving here is this
		// process's own bug and nothing remote can reach it.
		panic(fmt.Errorf("messagegroup: a wrap envelope of four fixed width fields did not encode: %w", err))
	}
	return encoded
}

// ParseWrapEnvelope reads the eleven octets back.
//
// It refuses a short or a long input rather than reading a prefix, which is what stops a truncated
// body being read as an envelope whose content epoch is whatever followed it. The version is NOT
// judged -- see WrapFormatVersion.
func ParseWrapEnvelope(b []byte) (WrapEnvelope, error) {
	if len(b) != WrapEnvelopeBytes {
		return WrapEnvelope{}, fmt.Errorf("%w: %d octets of envelope, want %d",
			ErrWrapEnvelope, len(b), WrapEnvelopeBytes)
	}
	reader := syntax.NewReader(b)
	version, err := reader.ReadUint8()
	if err != nil {
		return WrapEnvelope{}, fmt.Errorf("%w: %w", ErrWrapEnvelope, err)
	}
	targetType, err := reader.ReadUint8()
	if err != nil {
		return WrapEnvelope{}, fmt.Errorf("%w: %w", ErrWrapEnvelope, err)
	}
	payloadType, err := reader.ReadUint8()
	if err != nil {
		return WrapEnvelope{}, fmt.Errorf("%w: %w", ErrWrapEnvelope, err)
	}
	contentEpoch, err := reader.ReadUint64()
	if err != nil {
		return WrapEnvelope{}, fmt.Errorf("%w: %w", ErrWrapEnvelope, err)
	}
	return WrapEnvelope{
		FormatVersion: version,
		TargetType:    targetType,
		PayloadType:   payloadType,
		ContentEpoch:  contentEpoch,
	}, nil
}

// WrapInfo builds MASTER section 7's nine-element info, in the order MASTER prints it.
//
// IT IS EXPORTED AND SEPARATELY CALLABLE ON PURPOSE. A second implementation cannot check a key it
// can only observe through an AEAD that either opens or does not; what it can check is these
// octets. m1 task 14 property 9 assertion 3 makes the same argument about the signature preimage
// -- "a coverage claim readable only through a signature verify is a claim two implementations can
// satisfy incompatibly" -- and the same is true one construction lower.
//
// WHAT EACH ELEMENT BUYS is MASTER section 7's own table and is not restated here, with one
// exception worth carrying beside the code: LP(target_xwing_pub) and LP(ct_xwing) are the two
// values that stand in for M-15's four, because M-15 was written against a pre-X-Wing revision
// whose ss_x25519 | ss_mlkem combiner revision 5 deleted. Pasting M-15's IKM literally would
// reinstate exactly that combiner.
//
// THE ORDER IS THE WHOLE OF IT. Every one of the nine is a length-prefixed or fixed-width field,
// so a transposition of any two changes no length, returns no error, and produces a wrap_key both
// ends of THIS implementation agree on and no second implementation ever reproduces.
func WrapInfo(envelope WrapEnvelope, groupId []byte, targetId []byte, algId uint16,
	targetXwingPub []byte, ctXwing []byte) []byte {

	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(wrapInfoLabel))
	writer.WriteOpaqueLP(groupId)
	writer.WriteUint64(envelope.ContentEpoch)
	writer.WriteUint8(envelope.TargetType)
	writer.WriteOpaqueLP(targetId)
	writer.WriteUint8(envelope.PayloadType)
	writer.WriteUint16(algId)
	writer.WriteOpaqueLP(targetXwingPub)
	writer.WriteOpaqueLP(ctXwing)
	info, err := writer.Bytes()
	if err != nil {
		panic(fmt.Errorf("messagegroup: the wrap info did not encode: %w", err))
	}
	return info
}

// wrapKeyMaterial is MASTER section 7's wrap KDF: Extract-then-Expand, fifty six octets, split
// key | nonce.
//
// THE EXTRACTION IS THE SECOND ONE THIS PACKAGE HAS AND IT IS NAMED IN THE GATE THAT COUNTS THEM.
// keyschedule.go's guardrail G1 makes keyScheduleExtract the one extraction of this package, and
// keyschedule_test.go holds the class of its callers against a written-down table; this function
// is the second row of that table and arrived with the commit that added the call, which is the
// shape the gate's own comment asks for. The delegation is what keeps the salt first: crypto/hkdf
// takes the ikm first and every spec text in this project writes HKDF-Extract(salt, ikm), and a
// transposition here returns thirty two well formed octets that no second implementation computes.
//
// WHAT THE EXTRACT BUYS, stated as MASTER states it rather than claimed as more: X-Wing's ss is
// already a uniform thirty two octet KDF output, so the named salt buys DOMAIN SEPARATION and not
// entropy extraction.
//
// The two halves are cut with their capacity pinned to their own length, for recordAeadMaterial's
// reason: without that an append to the key would write into the nonce's octets, which is a defect
// that shows up as a wrap that does not open on the OTHER side of a wire and never here.
//
// The noinline directive is this package's erase helper class: prk is thirty two octets of key
// material and the erase below is what keeps it out of the heap the collector moves around.
//
//go:noinline
func wrapKeyMaterial(shared []byte, info []byte) (key []byte, nonce []byte) {
	prk := keyScheduleExtract([]byte(wrapSaltLabel), shared)
	defer zeroize(prk)
	material := keyScheduleExpand(prk, info, wrapAeadMaterialBytes)
	return material[:recordAeadKeyBytes:recordAeadKeyBytes],
		material[recordAeadKeyBytes:wrapAeadMaterialBytes:wrapAeadMaterialBytes]
}

// EnvKey is env_key[k] for the epoch the handle STANDS AT.
//
// It is the exporter and nothing else: no extraction, no expansion and no storage root anywhere in
// the call, which is what makes the device wrap's outer seal acyclic. A handle whose epoch has
// aged out answers mls's own erasure sentinel and it travels, because an env_key computed over an
// empty exporter output is thirty two well formed octets that no other member reproduces.
func EnvKey(handle GroupHandle) ([]byte, error) {
	if handle == nil {
		return nil, fmt.Errorf("%w: env_key is an exporter output and there is nothing to export from", ErrNilGroupHandle)
	}
	return handle.Export(envKeyLabel, nil, EnvKeyBytes)
}

// PendingEnvKey is env_key[n+1] read off the handle's OWN STAGED COMMIT, before the merge.
//
// LEDGER RULING 37 IS WHY THIS EXISTS AND IT IS NOT A CONVENIENCE. The epoch fan-out used to be
// published after AdvanceEpoch, so the wrap rows carried record epoch n+1 -- and item 246's F0
// ceiling serves a reader standing at epoch n only rows with epoch <= n, so read_key[n+1] needed
// pq_secret[n+1] needed the wrap needed read_key[n+1]. The ruling breaks it by submitting the
// wraps at epoch n, staged and pre-merge, still sealed under env_key[n+1] exactly as MASTER
// section 8.2 says. That is only buildable if the committer can compute env_key[n+1] BEFORE it
// merges, which is this door, and only correct if what it answers is what Export answers after
// ApplyCommit on the receiver -- which enginepending_test.go already holds for the storage
// exporter and wrap_test.go holds for this one.
//
// It answers mls.ErrNoPendingCommit when nothing is staged, so a fan-out can never be built out of
// the epoch the group is already in.
func PendingEnvKey(handle GroupHandle) ([]byte, error) {
	if handle == nil {
		return nil, fmt.Errorf("%w: env_key is an exporter output and there is nothing to export from", ErrNilGroupHandle)
	}
	return handle.PendingExport(envKeyLabel, nil, EnvKeyBytes)
}

// WrapRecordKeyZero is the head of a device wrap's record ladder: MASTER section 8.2's ruling of
// 2026-09-13, which puts env_key[k] where the class key stands for every other record.
//
//	record_key[0] = HKDF-Expand(env_key[k], "sender/v1" | LP(leaf_index), 32)
//
// NO NEW LADDER AND NO NEW LABEL BELOW THE ROOT, which is the ruling's own wording and is why this
// is a call into RecordKeyZero rather than a second expansion beside it. What it changes is the
// ROOT and nothing else, so every rung, every AEAD label and every width below it is the one the
// rest of the record layer already uses and already has a known answer for.
//
// THE HAZARD THIS SIGNATURE CANNOT EXPRESS, written here because spec A section 5.11 sharpened it
// into a break rather than a note. env_key[k] has NO RETENTION CLASS IN IT, and the 2026-09-13
// split puts a PERMANENT record (pq_secret[k]) and an EPH(5) record (eph_root[k]) on this one
// root. An implementer that instantiates one ratchet per (sender, class) as section 5.5 directs
// gets TWO ratchets with byte-identical roots, both starting at i = 0 -- so the two device wraps
// for one leaf are sealed under the same (key_head, nonce_head) and the same (key_body,
// nonce_body) with different plaintexts, and the message server recovers
// pq_secret[k] XOR eph_root[k] for every leaf of every epoch. The repair is ruling A1 of
// 2026-09-07: i = stream_index in every ladder, over one class-blind counter per
// (group_id, sender_handle), which streamindex.go already carries. Nothing in THIS function can
// hold that -- it answers rung zero and the position is the fan-out's -- so it is stated here and
// held where the fan-out is built, which is task 15.
func WrapRecordKeyZero(envKey []byte, leaf uint32) []byte {
	return RecordKeyZero(envKey, leaf)
}

// SealWrapBody is the door: one payload, sealed to one target leaf's X-Wing public half.
//
// It answers wrap_body -- wrap_envelope | hybrid_ct, MASTER section 7's grammar -- and NOT a
// record. Padding it to its rung with the LP32 prefix is the record layer's padBody, which already
// writes LP32(len) | body | zeros, and building the record around it is task 15's.
//
// THE RANDOMNESS IS THE CALLER'S AND HALF THE STORY. random supplies X-Wing's x25519 ephemeral and
// nothing else: crypto/mlkem's Encapsulate takes no randomness, so this call cannot be
// derandomized whatever reader it is handed, and xwing_test.go asserts that rather than leaving it
// as a sentence. That is why the known answers in testdata/envelope-wrap-kat.txt drive
// sealWrapBodyWith from a fixed (ss, ct_xwing) pair and not this function.
//
// AND IT IS WHY A REPUBLISHED WRAP MUST RE-ENCAPSULATE. wrap_key | wrap_nonce is a function of ss
// and of ct_xwing and of nothing the record carries, so a republisher that rebuilt a record around
// a ct_xwing its outbox kept would seal a second, different AAD_head under a byte-identical
// (key, nonce). Re-encapsulating gives a fresh pair unconditionally, which is the escape, and it
// means an outbox keeps the wrap PLAINTEXT rather than the sealed bytes. Ledger open item 142.
//
// targetType, targetId and payloadType are the three MASTER section 7 leaves without a code point.
// They are parameters and this file supplies no default for any of them.
func SealWrapBody(random io.Reader, target *XwingPublicKey, envelope WrapEnvelope,
	groupId []byte, targetId []byte, payload []byte) ([]byte, error) {

	if target == nil {
		return nil, fmt.Errorf("%w: a wrap is sealed to a leaf's published X-Wing key and none was given", ErrWrapTargetKey)
	}
	ctXwing, shared, err := XwingEncapsulate(random, target)
	if err != nil {
		return nil, err
	}
	defer zeroize(shared)
	return sealWrapBodyWith(envelope, groupId, targetId, target.Bytes(), ctXwing, shared, payload)
}

// sealWrapBodyWith is everything past the encapsulation, split out so the known answers can drive
// the deterministic half of this door over a fixed shared secret and a fixed ciphertext.
//
//	hybrid_ct = u16(alg_id) | LP(ct_xwing) | LP(aead_ct)
//	wrap_body = wrap_envelope | hybrid_ct
//
// THE AEAD TAKES NO ADDITIONAL AUTHENTICATED DATA, and that is the spec rather than an omission
// here: spec A section 5.11 records in as many words that "aead_ct still has no stated AAD", and
// everything that would be in one is already bound into wrap_key by MASTER's nine-element info --
// the group, the content epoch, both type octets, the target, the suite, the target's public key
// and the ciphertext that carried the secret. So it is written as an explicit nil rather than
// reached through sealRecordAead, which refuses an empty aad because a RECORD's two ciphertexts
// have one and must never be sealed against nothing.
//
// The noinline directive is this package's erase helper class: wrap_key and wrap_nonce are one
// backing array of key material and the erase below is what keeps it off the heap.
//
//go:noinline
func sealWrapBodyWith(envelope WrapEnvelope, groupId []byte, targetId []byte,
	targetXwingPub []byte, ctXwing []byte, shared []byte, payload []byte) ([]byte, error) {

	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: a wrap with no payload delivers nothing", ErrWrapPayload)
	}
	info := WrapInfo(envelope, groupId, targetId, XwingAlgId, targetXwingPub, ctXwing)
	key, nonce := wrapKeyMaterial(shared, info)
	defer zeroize(key)
	defer zeroize(nonce)
	aead, err := newRecordAead(key, nonce)
	if err != nil {
		return nil, err
	}
	aeadCt := aead.Seal(nil, nonce, payload, nil)
	writer := syntax.NewWriter()
	writer.WriteRaw(envelope.Encode())
	writer.WriteUint16(XwingAlgId)
	writer.WriteOpaqueLP(ctXwing)
	writer.WriteOpaqueLP(aeadCt)
	body, err := writer.Bytes()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWrapBody, err)
	}
	return body, nil
}

// wrapEnvelopeDisagreement names the first envelope field that is not the one the opener said it
// was honouring, and answers false when the carried envelope IS the wrap the opener asked for.
//
// IT TAKES THE THREE VALUES ONE BY ONE AND NOT AS A STRUCT, which is OpenWrapBody's paragraph and
// is the whole of what this signature is for.
//
// IT ANSWERS A BOOL BESIDE THE STRING RATHER THAN AN EMPTY STRING, because a caller deciding on
// the string would be comparing octets with ordinary go equality -- which mls's
// framing_guard_test.go refuses across this tree and refused this function's first draft by name.
// Nothing here is secret; the envelope is cleartext on the wire. The rule is mechanical for the
// reason it exists, which is that the next comparison spelled that way is the one over a tag.
//
// It names one field rather than all three because the string is a diagnostic and not a verdict:
// the verdict is ErrWrapEnvelopeMismatch, which is one sentinel however many fields disagree. The
// three values it prints are cleartext octets of the body it was handed.
func wrapEnvelopeDisagreement(envelope WrapEnvelope, contentEpoch uint64,
	targetType uint8, payloadType uint8) (string, bool) {

	switch {
	case envelope.ContentEpoch != contentEpoch:
		return fmt.Sprintf("content epoch %d and its opener is honouring epoch %d",
			envelope.ContentEpoch, contentEpoch), true
	case envelope.TargetType != targetType:
		return fmt.Sprintf("target_type %#02x and its opener asked for %#02x",
			envelope.TargetType, targetType), true
	case envelope.PayloadType != payloadType:
		return fmt.Sprintf("payload_type %#02x and its opener asked for %#02x",
			envelope.PayloadType, payloadType), true
	}
	return "", false
}

// OpenWrapBody is the other half of the door: one wrap body, opened with the target leaf's own
// X-Wing private half, for an opener that says which wrap it is opening.
//
// THE DECAPSULATION'S "YES" IS NOT THE ANSWER, and this is the property the whole door turns on.
// ML-KEM-768 uses implicit rejection: a ciphertext that was not produced for this key
// decapsulates SUCCESSFULLY, to a pseudorandom secret, rather than failing -- which is what leaves
// no success flag to leak and no oracle to query, and is exactly why XwingDecapsulate answers a
// shared secret and no verdict. So every wrap addressed to some other leaf reaches this function's
// KDF and its AEAD with thirty two perfectly well formed octets, and the ONLY thing that separates
// "this wrap is mine" from "this wrap is not" is the Poly1305 tag below. A door that reported the
// decapsulation's error and stopped there would report success on every wrap in the epoch.
// testdata/envelope-wrap-kat.txt carries that as its failing-direction vector.
//
// THE KEY IS DERIVED FROM THE ENVELOPE'S CARRIED VALUES and not from the opener's own epoch. That
// is unruled -- m1 open item M1-55, and ledger item 178's first residual files the sentence as
// owed -- and it is M1-55's first reading, which MASTER section 7's own rationale argues for: "a
// receiver that derives the key from the envelope's own values and finds aead_ct does not open has
// detected the disagreement fail-closed".
//
// THAT SENTENCE IS ABOUT A DISAGREEMENT, AND A GENUINE WRAP OF ANOTHER EPOCH IS NOT ONE. It holds
// where the octets on the wire and the octets the sealer used DIFFER: the opener then derives a
// key the sealer never used and the tag fails. It says nothing about a wrap nobody edited. A wrap
// sealed at another epoch, or carrying the other payload kind, is self-consistent -- its key
// matches its own envelope -- and the AEAD opens it, which is measured in wrap_test.go. So the ten
// info-bound octets are authenticated by the AEAD against TAMPERING and against nothing else, the
// eleventh -- the version octet -- is authenticated by nothing at all until task 14 step 3's
// signature lands, and WHICH WRAP THIS IS is a question the AEAD was never asked. Open item MG-7;
// wrap_test.go measures both authorities and reports what each covers rather than asserting a
// table.
//
// WHICH IS WHY contentEpoch, targetType AND payloadType ARE ARGUMENTS AND NOT A COMMENT. The
// opener states the wrap it is honouring, and a body carrying any other envelope is refused with
// ErrWrapEnvelopeMismatch -- BEFORE the decapsulation and before the AEAD, so the plaintext of a
// wrap nobody asked for is never recovered in this process. M1-55 names taking that comparison
// itself, as the third behaviour neither document describes -- an opener "may or may not compare
// the carried values against its own and refuse a mismatch" -- and leaves it to the
// implementation; what is ruled nowhere is left nowhere by taking it, because the wire does not
// move. What moves is who is obliged. Task 15's fan-out is the consumer that INSTALLS
// pq_secret[k] and eph_root[k] into a session, which is MASTER section 5.3's "honour", and a
// residual obligation written in a comment is one that consumer meets or does not.
//
// THE THREE ARE SEPARATE PARAMETERS AND NOT A STRUCT, AND THAT SHAPE IS A REPAIR OF A MEASURED
// DEFECT. They were one argument of type WrapExpectation, and the claim written over it was that
// an obligation carried in a type is one a caller cannot consume the door without meeting. That
// claim was false, for a reason that is Go's and not this package's: a struct's zero value is a
// complete value of it, so WrapExpectation{} compiled, stated nothing, and opened a GENUINE wrap
// whose envelope was {target_type 0x00, payload_type 0x00, content epoch 0} -- measured, payload
// byte for byte, err nil -- while WrapExpectation{ContentEpoch: k} named the epoch and silently
// expected 0x00 for the two octets MG-7 itself says have no code point anywhere. A caller could
// consume the door while declining the obligation, and no gate and no open item recorded it.
//
// As three parameters there is no such call, because Go supplies no argument a caller did not
// write: each of the three is on the page at every call site. THAT IS THE WHOLE OF WHAT THE SHAPE
// BUYS and it is not the paragraph below. 0x00 and epoch 0 stay perfectly reachable expectations
// -- 0 is the founding epoch and neither type octet has a code point to be unlike -- and what is
// gone is stating them by omission. wrap_test.go holds the structural half over this signature,
// so the three arriving through an aggregate again fails on the day it lands.
//
// IT IS THE SHAPE SealDeviceWraps ALREADY HAS for the same three under-determined values, one
// door over, and for the file header's reason: no default, no package constant and no fallback,
// so this package cannot be the one that chose. THE SEALER'S ENVELOPE ARGUMENT IS NOT THE SAME
// CASE and is deliberately left a struct: it is the wire record being WRITTEN rather than an
// authority being stated, and m1 task 14 property 9 requires in as many words that "the sealing
// side is reachable with an envelope the caller chooses" -- its assertion 1 needs a consistent
// control for each of the ELEVEN envelope octets, the version octet included, and an opener may
// state an expectation over only ten.
//
// THE PARAMETER ORDER IS MASTER SECTION 7's info ORDER -- LP(group_id) | u64(epoch) |
// u8(target_type) | LP(target_id) | u8(payload_type) -- so that a call site reads as the line it
// is checked against, and so that the two u8s are not adjacent. Two uint8 parameters side by side
// are two the compiler cannot tell apart. It is not a preference and it is not only written here:
// wrap_test.go reads WrapInfo's own sequence of writes and this signature's own parameter list and
// compares them, so the claim is held against the encoder the known answers reproduce bytewise
// rather than against a list copied out of MASTER.
//
// u8(wrap_format_version) IS DELIBERATELY NOT ONE OF THEM. What an opener does with an
// unrecognised version is m1 open item M1-54 and is unruled, so this door gives a caller no way to
// express an expectation over it and refuses nothing on it. The octet stays exactly as
// unauthenticated as it was -- measured, printed and filed as MG-7 -- rather than being hidden
// behind a comparison no ruling has authorised. wrap_test.go holds the complement of this
// signature against WrapEnvelope's fields, and fails in both directions.
//
// WHAT NONE OF IT ESTABLISHES, because the door cannot see it: that the values came from anywhere.
// This compares what the caller was handed against what the body carries; it cannot tell an
// expectation read off the caller's own authority -- the epoch it is restoring, the record kind it
// asked for -- from one copied out of the body in front of it. There is deliberately no exported
// helper that reads the three off a WrapEnvelope, so that copying is something a caller has to
// write out on purpose; nothing here stops it. Open item MG-7.
//
// target_xwing_pub comes from the opener's OWN key and is never an argument: it is one of the nine
// inputs to wrap_key, and taking it from a caller would let a wrap be opened under a public key
// that is not the one this private half belongs to.
//
// The noinline directive is this package's erase helper class, for sealWrapBodyWith's reason.
//
//go:noinline
func OpenWrapBody(priv *XwingPrivateKey, groupId []byte, contentEpoch uint64,
	targetType uint8, targetId []byte, payloadType uint8, body []byte) (WrapEnvelope, []byte, error) {

	if priv == nil {
		return WrapEnvelope{}, nil, fmt.Errorf("%w: a wrap is opened with the target leaf's own X-Wing private half and none was given", ErrWrapTargetKey)
	}
	if len(body) < WrapEnvelopeBytes {
		return WrapEnvelope{}, nil, fmt.Errorf("%w: %d octets, and the envelope alone is %d",
			ErrWrapBody, len(body), WrapEnvelopeBytes)
	}
	envelope, err := ParseWrapEnvelope(body[:WrapEnvelopeBytes])
	if err != nil {
		return WrapEnvelope{}, nil, err
	}
	if disagreement, disagrees := wrapEnvelopeDisagreement(envelope, contentEpoch, targetType, payloadType); disagrees {
		// AHEAD OF THE KEM AND AHEAD OF THE AEAD, so that a wrap this opener did not ask for is
		// refused without its payload ever being recovered here -- and so that the refusal does
		// not depend on the rest of the body parsing at all. Nothing is carried out with it:
		// returning the envelope of a wrap that was refused is handing a caller the very value
		// it just failed to match.
		return WrapEnvelope{}, nil, fmt.Errorf("%w: this wrap carries %s",
			ErrWrapEnvelopeMismatch, disagreement)
	}
	algId, ctXwing, aeadCt, err := parseHybridCt(body[WrapEnvelopeBytes:])
	if err != nil {
		return WrapEnvelope{}, nil, err
	}
	shared, err := XwingDecapsulate(priv, ctXwing)
	if err != nil {
		return WrapEnvelope{}, nil, err
	}
	defer zeroize(shared)
	info := WrapInfo(envelope, groupId, targetId, algId, priv.Public().Bytes(), ctXwing)
	key, nonce := wrapKeyMaterial(shared, info)
	defer zeroize(key)
	defer zeroize(nonce)
	aead, err := newRecordAead(key, nonce)
	if err != nil {
		return WrapEnvelope{}, nil, err
	}
	payload, err := aead.Open(nil, nonce, aeadCt, nil)
	if err != nil {
		// no plaintext, and the underlying error is not wrapped: it says only that
		// authentication failed, it is the same for every cause, and a caller separating causes
		// here would be separating what an attacker chose.
		return WrapEnvelope{}, nil, fmt.Errorf("%w: %d octets of aead_ct at content epoch %d",
			ErrWrapOpen, len(aeadCt), envelope.ContentEpoch)
	}
	return envelope, payload, nil
}

// parseHybridCt reads u16(alg_id) | LP(ct_xwing) | LP(aead_ct) and refuses anything after it.
//
// THE TRAILING REFUSAL IS THE POINT AND NOT THE PARSE. hybrid_ct is self-delimiting, so octets
// past it are octets no field of this grammar names -- and a wrap body's tail is INSIDE the record
// body AEAD, whose key descends from env_key[k], which EVERY member of the epoch holds. MASTER
// section 8.2 states what an unchecked tail is worth: a ~2.8 KB member-writable channel inside
// every wrap that the body signature does not cover. The ZERO-tail refusal over the padded body is
// a second, separate obligation and belongs with the padded body rather than with this reader;
// this one refuses the octets that are inside wrap_body and outside its grammar.
//
// alg_id is refused against X-Wing here rather than carried out, because it is one of wrap_key's
// nine inputs: a body naming a suite this build does not have is a body whose key this build would
// derive under the wrong two octets and then blame on the tag.
func parseHybridCt(b []byte) (algId uint16, ctXwing []byte, aeadCt []byte, err error) {
	reader := syntax.NewReader(b)
	algId, err = reader.ReadUint16()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %w", ErrWrapBody, err)
	}
	if algId != XwingAlgId {
		return 0, nil, nil, fmt.Errorf("%w: alg_id %#04x is not X-Wing", ErrWrapAlgId, algId)
	}
	ctXwing, err = reader.ReadOpaqueLP()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %w", ErrWrapBody, err)
	}
	aeadCt, err = reader.ReadOpaqueLP()
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %w", ErrWrapBody, err)
	}
	if !reader.Empty() {
		return 0, nil, nil, fmt.Errorf("%w: %d octets follow hybrid_ct and no field of a wrap body names them",
			ErrWrapBody, reader.Remaining())
	}
	return algId, ctXwing, aeadCt, nil
}

// SealDeviceWraps is MASTER section 8.2's TWO RECORDS PER TARGET, as two bodies.
//
// It is two and not one, and that is a 2026-09-13 ruling rather than a shape: until then the
// device wrap was ONE record carrying pq_secret[n] and eph_root[n] together, and every existing
// draft, every option write-up and the sizing arithmetic in three documents described it that way,
// so it is the mistake a reader arrives holding. The split is what makes MASTER section 8.1's
// disappearing-message promise CRYPTOGRAPHIC rather than behavioural: pq_secret[n] rides a
// PERMANENT record and eph_root[n] an EPH(5) one, at the four-week rung, so the second is destroyed
// by the retention ladder while the first is not. A builder that emitted one record carrying both,
// or two records at one retention class, would round trip perfectly and break exactly that.
//
// WHAT IT REFUSES, and it is the only judgement this function makes: the two wraps MUST NOT take
// one payload_type. Both records land at the SAME wrap_target_handle -- that derivation is
// unchanged and no ruling of 2026-09-13 changes it -- so two records at one handle is the normal
// case, and the only thing in MASTER's nine-element info that separates the two keys is
// u8(payload_type). MASTER section 7's own table gives that octet exactly this job: "separates
// payload kinds sent to one target at one epoch -- which ruling 3 of 2026-09-13 turns into a live
// case rather than a hypothetical, because a device leaf now receives two wrap records at one
// epoch". A caller passing one octet twice has a fan-out whose two records are separable by
// nothing a key binds.
//
// AND SEPARATING THE TWO KEYS IS NOT SEPARATING THE TWO RECORDS AT AN OPENER, which is the half
// this function cannot hold and OpenWrapBody's payloadType argument does. Each body's key is
// derived from the payload_type that body CARRIES, so before that argument existed both of these
// opened at one door under identical arguments and the octet was carried out to a caller who was
// under no obligation to look at it. Two distinct octets are what make an opener's refusal
// possible; an opener that states which kind it is honouring is what makes it happen -- and it
// states it the way this function's caller states it, as a parameter with no default, because an
// aggregate a caller can leave half written is an obligation a caller can leave half met.
//
// WHICH TWO OCTETS THEY ARE IS NOT THIS FILE'S TO SAY. u8(payload_type) has no code point in any
// document, so the caller supplies both and this function supplies neither. See MG-7.
//
// WHAT IT DOES NOT DO: it does not count. MASTER section 8.2 fixes
// expected_wrap_count = 2 x (active device leaves) + 1, the +1 being the epoch snapshot, and that
// count is the marker's and therefore task 15's. It also does not enumerate the leaves: m1 task 14
// property 1 requires the enumeration to come from the GROUP, through GroupHandle.MemberAt, and
// never from a list a caller passes, because a fan-out over a caller-supplied list is a fan-out
// that silently omits -- and that enumeration belongs with the fan-out that consumes it.
func SealDeviceWraps(random io.Reader, target *XwingPublicKey, contentEpoch uint64,
	targetType uint8, groupId []byte, targetId []byte,
	pqPayloadType uint8, pqPayload []byte,
	ephPayloadType uint8, ephPayload []byte) (pqBody []byte, ephBody []byte, err error) {

	if pqPayloadType == ephPayloadType {
		return nil, nil, fmt.Errorf("%w: both records at this target take payload_type %#02x, and it is the only element of wrap_key's info that separates them",
			ErrWrapPayloadTypeCollision, pqPayloadType)
	}
	pqBody, err = SealWrapBody(random, target, WrapEnvelope{
		FormatVersion: WrapFormatVersion,
		TargetType:    targetType,
		PayloadType:   pqPayloadType,
		ContentEpoch:  contentEpoch,
	}, groupId, targetId, pqPayload)
	if err != nil {
		return nil, nil, err
	}
	ephBody, err = SealWrapBody(random, target, WrapEnvelope{
		FormatVersion: WrapFormatVersion,
		TargetType:    targetType,
		PayloadType:   ephPayloadType,
		ContentEpoch:  contentEpoch,
	}, groupId, targetId, ephPayload)
	if err != nil {
		// the first body is sealed ciphertext and not key material, so it is dropped rather
		// than erased -- but it is dropped rather than returned, because half a device wrap is
		// a fan-out that breaks property 1's "exactly two" at the first leaf.
		return nil, nil, err
	}
	return pqBody, ephBody, nil
}
