// CP3b's defining property, held as a STANDING test: no test-only key source anywhere on the
// record path.
//
// THE CLASS IS DERIVED AND NOT LISTED. The property's subject is every octet used as an AEAD
// key, an AEAD nonce or a MAC key by SealRecord or OpenRecord. A written down list of the
// derivations that produce those octets understates the class the moment a second key source
// is added, which is the one event this file exists to catch, so the class is closed from the
// other end instead: THE WHOLE SEALED RECORD IS REBUILT, BYTE FOR BYTE, from three values and one
// opaque blob and nothing else. Every keyed octet of a record is inside a reproduction of that
// record by definition, so an octet drawn from anywhere this file is not given moves ct_body,
// ct_head, sender_handle or write_auth, and one of the four comparisons below goes red.
//
// THE THREE VALUES, and there are no others:
//
//	mls_secret    the REAL group's Export("URmessage/v1/storage", nil, 32), taken off the same
//	              mls.Group the session under test is holding
//	pq_secret     the value the constructor was injected with; the session takes it as a
//	              required argument and has no default to fall back to
//	server_nonce  the value the constructor was injected with; write_auth is a mac over it
//
// Neither of the last two is a KEY -- sessionfixture_test.go's header draws that distinction
// and CP3b's bar rests on it -- and the first IS the MLS key schedule's output. So a
// reproduction that succeeds from these three is the statement that every key of the record
// layer is the MLS key schedule expanded, and nothing else.
//
// AND THE FOURTH VALUE, WHICH ARRIVED WITH MASTER SECTION 8.4 ON 2026-09-15 AND IS NOT A KEY:
//
//	inner         the marshalled MLS PrivateMessage this record's ct_body carries, handed over
//	              as OCTETS and treated as OPAQUE. Nothing here parses it, keys anything with
//	              it, or derives anything from it; it goes into LP(inner) | 0* and is padded.
//
// WHY IT HAD TO BE INJECTED RATHER THAN DERIVED, and this is the honest statement of what moved.
// MASTER section 8.4 makes an application record's ct_body plaintext an MLS frame, and that frame
// is NOT a function of the three values above: it depends on the group's encryption_secret, on
// this leaf's ratchet GENERATION, on the device's signing key and on four octets of fresh
// reuse_guard, so two calls do not even agree with each other. A reproduction that tried to
// recompute it would be reproducing connect/mls rather than the record layer, and a reproduction
// that dropped the body would have stopped rebuilding the record. So it is injected, with exactly
// the standing server_nonce has: a value the fixture hands over, that no key is derived from.
//
// WHAT THAT COSTS, STATED RATHER THAN LEFT TO BE NOTICED. This file no longer says anything about
// the BODY PLAINTEXT's provenance -- the octets inside the frame are outside the reproduction, and
// a second key source that reached only into Protect is invisible here. What it still says, in
// full, is that EVERY RECORD LAYER KEY comes from the exporter and nowhere else, which is the CP3b
// property and is the whole of what this file has ever been for. The compensating control is
// TestFlippingAnyOctetOfTheInjectedFrameMovesTheBody: the frame is a live input, so a reproduction
// that ignored it and rebuilt the body from something else would agree with nothing.
//
// AND THE RECORDS MOVED OFF THE 256 OCTET RUNG, which is the second thing that ruling cost this
// file. The three bodies are 100, 101 and 102 octets and were the 256 rung's; the frame around
// them is 194 octets, so they are now 294, 295 and 296 and the smallest rung that fits them is
// 1 KiB. keySourceRungBytes, keySourceSizeBucketCode and the ct_body length assertion all move
// with them. THE BODIES ARE NOT SHRUNK TO 59 TO KEEP THE OLD CONSTANT: the constant is a
// transcription of where the record lands, and tuning the fixture until the old number came back
// would be the fixture lying about the ladder.
//
// NOTHING ON THE REPRODUCTION'S SIDE OF THE COMPARISON COMES FROM THE MODULE UNDER TEST, and
// that is the whole of what makes this evidence rather than a tautology. A reproduction that
// reached for the record layer's own sealer would seal under whatever key the record layer
// chose -- a key it drew from a second source included -- and would agree with it forever. What
// stands in for it is RFC 5869 written out from the RFC, chacha20poly1305.NewX, crypto/hmac and
// crypto/sha256.
//
// StorageRoot, GroupHandleKey, SenderHandle, DeriveClassKeys, RecordKeyZero, RecordKeyNext,
// RecordAeadHead, RecordAeadBody, sealRecordAead, padBody, message.AADHead, message.AADBody,
// message.WriteKey and message.ComputeWriteAuth are the fourteen names this paragraph used to
// list. THEY ARE AN ILLUSTRATION AND NOT THE CLASS. Four of them are in connect/message and
// were outside the gate that once held this claim, which is how a reviewer pointed the
// reproduction at message.WriteKey and left all three tests green. The class is now every name
// the MODULE declares, read off go.mod and the import graph, and the gate at the bottom of this
// file is where it is derived, what its scope is, and -- stated rather than implied -- what it
// still cannot see.
//
// THE REPRODUCTION CANNOT SEE THE TWO VALUES IT REPRODUCES, and that is a type rather than a
// promise. keySourceShape carries every PUBLIC field of the record: the group id, the leaf,
// the epoch, the stream index, is_commit, the retention wire byte, the size bucket, expire_at,
// the blob id, the encoded attachment and the two plaintexts. It has NO field for
// sender_handle and NO field for body_hash, which are the two header values the key schedule
// produces, so a reproduction that read the sealer's answer for either -- and therefore
// compared the sealer against itself -- does not compile.
//
// ---------------------------------------------------------------------------
// WHAT THIS FILE CANNOT SEE
// ---------------------------------------------------------------------------
//
// (1) A key source that is itself a function of these three values. What is observed is
// DEPENDENCE and EXCLUSIVITY, not the absence of a constant from the source text: a second
// derivation of the SAME material -- one rung expanded twice, one label spelled two ways that
// agree -- reproduces identically and is invisible here. What is not invisible is any value
// MIXED IN from elsewhere, which is what "a second key source" means: a constant, a second
// exporter label, an entropy draw, a per-process seed, a leftover from another epoch. Every
// one of those breaks the reproduction, and the negative control below is what proves the
// reproduction is CAPABLE of breaking: it flips each of the 256 bits of the exporter output in
// turn and requires all four outputs to move for every one of them.
//
// (2) A path this record does not take. These are three records on the DURABLE class, on the
// 256 octet rung, at the first three rungs of one sender's ladder, in EPOCH ZERO of a one
// member group, with no attachment and no blob id. A key source reached only from the
// permanent, media or eph classes is outside the observation. It became REACHABLE on 2026-09-13,
// when M1-6's ruling of 2026-09-07 was reversed and SealRecord began sealing every class -- which
// widens what this file COULD cover and not what it does, and the three records it rebuilds are
// deliberately unchanged so that the reproduction is the same reproduction. So is
// one reached only by the blob rung, or only by a receiver walking a skipped key window. What
// covers those is that they are the same four derivations under other arguments, which is a
// structural argument and not this file's measurement.
//
// (3) Anything at all about epoch > 0. group_handle_key is HKDF-Expand(storage_root[0],
// "gh/v1", 32) and this fixture's group is at epoch zero, so the root derived here IS the
// epoch zero root. The test ASSERTS that epoch rather than assuming it, which is what makes
// the reproduction legal rather than lucky. At any later epoch sender_handle is a function of
// a root this file is not given -- which is the whole reason group_handle_key is persisted
// state -- and TestTheGroupHandleKeyDoesNotMoveWhenTheEpochDoes is what holds that, not this.
//
// (4) Whether the MLS exporter is itself right. mls_secret arrives as whatever the real group
// answers, and connect/mls's own cross-implementation vectors are what hold RFC 9420 section
// 8.5. This file asks only what the record layer does with the answer.
//
// (5) The read key. message.ReadKey is on neither SealRecord's nor OpenRecord's path -- no
// record is macd under it -- so it is outside the derived class and outside the reproduction.
//
// (6) The OPEN side's derivations directly, and WHAT BINDS THEM MOVED ON 2026-09-15. It used to
// be the last assertion of the first test: the record opens, through the session, to exactly the
// two plaintexts that went in. It cannot be, because MASTER section 8.4 makes the body an MLS
// frame and a member has no receiving ratchet for its own leaf, so this one member fixture's own
// OpenRecord now REFUSES the record it sealed -- open item MG-4.
//
// What binds them now is WHERE it refuses, and it is a narrower statement of the same thing.
// ErrRecordInnerFrame is reached only after openRecordOnLoop has derived the rung, expanded both
// AEAD halves, opened ct_head, opened ct_body and unpadded the result: every open side derivation
// has already run and succeeded by the time that sentinel is produced. A second key source on the
// open side alone therefore does not reach it -- the record fails in an AEAD instead, with a
// different sentinel -- and a second key source on BOTH sides is a record whose ciphertexts this
// reproduction does not match. There is still no third case. What is LOST is the plaintext: this
// file no longer observes that the body came back as the body, because nothing here can open it.
//
// ---------------------------------------------------------------------------
// WHY THE LABELS ARE TRANSCRIBED HERE RATHER THAN READ OFF THE PACKAGE
// ---------------------------------------------------------------------------
//
// Every label, width and code point below is transcribed from MASTER and spec A. Reaching for
// durableClassInfo, recordAeadBodyInfo or mlsSecretLabel would let a label change move both
// halves together and leave this file green over a wire format no second implementation
// computes -- the same failure the KAT sets in keyschedule_test.go, recordkey_test.go and
// handle_test.go exist to close. This is the fourth transcription of them and not the first
// sharing of one.
//
// SO IF THIS FILE DISAGREES WITH THE PACKAGE, ONE OF THE TWO IS WRONG, AND WHICH ONE IS A
// RULING QUESTION RATHER THAN A FIXTURE TO RETUNE. M1-7 and M1-8 are still open over values this
// reproduction spells out, and a ruling that moves one of them moves this file too -- as an edit
// somebody makes deliberately, with the ruling in hand. M1-6 is the worked example: ruled
// 2026-09-07, REVERSED 2026-09-13, and the rulings of that date cost this file three transcribed
// blocks -- both aads and the write_auth preimage each gained a u64 term -- which is exactly the
// deliberate edit this paragraph describes, made with the ruling in hand.
//
// RFC 5869 is the one thing taken from elsewhere in the test tree rather than transcribed
// again: keyScheduleReferenceExtract and keyScheduleReferenceExpand are already written out
// from the RFC in keyschedule_test.go and are already held against pinned vectors, and a
// second spelling of HKDF in this package's tests would be the second construction of one
// thing that this project bans everywhere else.
package messagegroup

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
	"golang.org/x/crypto/chacha20poly1305"
)

// The exporter call MASTER section 7 derives mls_secret with, transcribed.
const (
	keySourceExporterLabel = "URmessage/v1/storage"
	keySourceExporterBytes = 32
)

// Every label the chain from storage_root to a sealed record expands under, transcribed from
// MASTER section 7, MASTER section 8, MASTER section 8.1, MASTER section 9.2 and spec A
// section 5.3. They are separate constants here for the reason they are separate constants in
// the package: one construction with a word substituted into it is one edit away from making
// two keys equal.
const (
	keySourceGroupHandleInfo   = "gh/v1"
	keySourceSenderHandleInfo  = "sh/v1"
	keySourceDurableClassInfo  = "durable/v1"
	keySourceRecordKeyZeroInfo = "sender/v1"
	keySourceRecordKeyNextInfo = "ratchet/v1"
	keySourceAeadHeadInfo      = "rec/v1/head"
	keySourceAeadBodyInfo      = "rec/v1/body"
	keySourceHeadBindInfo      = "rec/v1/head-bind"
	keySourceWriteKeyInfo      = "write/v1"
)

// The three domain separation labels of the preimages, transcribed from MASTER section 7.1 and
// MASTER section 9.2.
const (
	keySourceAadHeadLabel   = "URmessage/v1/aad/head"
	keySourceAadBodyLabel   = "URmessage/v1/aad/body"
	keySourceWriteAuthLabel = "URmessage/v1/write"
	// MASTER section 8.4.2 v2's label, transcribed. THE LABEL IS THE VERSION: there is no wire
	// signal and format_version deliberately does not bump, so a v2 preimage built under v1's
	// label is a build that agrees with the wrong half of the world in silence.
	keySourceAadMlsLabel = "URmessage/v2/aad/mls"
)

// The widths and the two code points, transcribed.
const (
	keySourceKeyBytes       = 32
	keySourceHandleBytes    = 16
	keySourceAeadKeyBytes   = 32
	keySourceAeadNonceBytes = 24
	keySourceAeadTagBytes   = 16
	// MASTER section 7.1's registration for XChaCha20-Poly1305, carried inside both aads.
	keySourceAeadAlgId uint16 = 0x0021
	// MASTER section 8's retention table, the durable row: the CLASS tag and the wire byte it
	// joins to. BOTH are transcribed, and neither is read off the package into the shape any
	// more. Asking message.RetentionClassWire for the wire byte -- which keySourceShapeOf used
	// to do -- put one of the record layer's own derivations on the reproduction's side of the
	// comparison, and retention_wire is carried by both aads and by the write_auth preimage, so
	// it is a keyed octet by the definition this file opens with.
	keySourceDurableClassCode byte = 0x01
	keySourceDurableWire      byte = 0x01
	// MASTER section 8's eph_window on a durable record: zero, and PRESENT. Written as its
	// own transcribed constant rather than read off the header, for the reason every other
	// value in this block is: the reproduction is held against MASTER and not against what
	// the record layer put in the field.
	keySourceDurableEphWindow uint64 = 0
	// MASTER's size ladder, the 1 KiB rung: the bucket TAG and the octet count it names. The
	// count fixes octet_length(ct_body), so it is on the same side of the same line.
	//
	// IT WAS THE 256 OCTET RUNG AND 0x00 UNTIL 2026-09-15. The three bodies are unchanged at
	// 100, 101 and 102 octets; MASTER section 8.4 put a 194 octet MLS frame around each, and
	// 294 does not fit 256. The transcription follows the record rather than the record being
	// tuned to the transcription.
	keySourceSizeBucketCode byte = 0x01
	keySourceRungBytes           = 1024
)

// keySourceShape is every PUBLIC field of one record: what a server, or anybody holding the
// record, can read without a key.
//
// THERE IS NO sender_handle FIELD AND NO body_hash FIELD, and their absence is the point. Both
// are outputs of the key schedule, both sit in RecordHeader, and both are values the
// reproduction below produces; a shape that carried either would let the reproduction read the
// sealer's own answer for the thing it is supposed to be recomputing. keyschedule.go's
// ClassKeys makes the same move for eph_root and seal.go's staging chain makes it for the
// construction order: the wrong thing is not made difficult, it is made unrepresentable.
type keySourceShape struct {
	groupId     [32]byte
	leaf        uint32
	epoch       uint64
	streamIndex uint64
	isCommit    bool
	// the JOINED wire byte of MASTER section 8's table and never the go tag, which is the value
	// both aads and the write_auth preimage carry.
	retentionWire byte
	// t, the eph ladder's time slice, PLAINTEXT on the wire and carried by both aads and by
	// the write_auth preimage, immediately after the retention octet it qualifies. RULED
	// 2026-09-13 and transcribed here that day. This fixture's records are DURABLE, so the
	// value is the presence rule's zero -- and the zero is exactly why it belongs in the
	// shape rather than being left out: a builder that wrote the field only for an eph
	// record produces a preimage eight octets shorter than this one on every record this
	// reproduction sees, which is the conditional MASTER section 8 forbids, observed from
	// the outside.
	ephWindow  uint64
	sizeBucket byte
	// the octet length of the rung the padded body fills, which is what fixes
	// octet_length(ct_body) at rung + 16.
	rungBytes  int
	expireAt   uint64
	blobId     []byte
	attachment []byte
	headPlain  []byte
	// the MLS PrivateMessage ct_body carries, OPAQUE: it is length prefixed and padded and
	// nothing here reads an octet of it. MASTER section 8.4, and the file header says at
	// length why it is injected rather than derived and what that costs.
	innerFrame []byte
	// the GENERATION that frame was sealed at and the authenticated_data it carries in the
	// clear, both read off the frame by the fixture. Neither is a key and neither is secret:
	// the generation is four octets of a header sealed under a GROUP SHARED secret, and the
	// authenticated_data is a cleartext field of the frame anybody holding the record can read.
	//
	// THEY ARRIVED WITH MASTER SECTION 8.4.2 v2 ON 2026-09-17 and they are here for ledger item
	// 206: v2 adds ONE new keyed octet-producer to the record path -- head_commit, an HMAC under
	// a key expanded from this record's own rung -- and a reproduction that accepted it inside
	// the injected frame would be the one thing the CP3b property does not cover. With these two
	// the reproduction recomputes head_commit from its OWN ladder and rebuilds the whole 160
	// octet preimage, so the new key is inside the same statement as every other one.
	generation uint32
	frameAad   []byte
}

// keySourceReproduction is what the three values alone produce: the record's two ciphertexts,
// the hash that binds them, the handle the server routes on and the mac the server checks.
//
// These four are every octet of a record that is not already in the shape above, which is what
// makes "the record is reproduced" the same statement as "no key came from anywhere else".
type keySourceReproduction struct {
	senderHandle [16]byte
	ctBody       []byte
	bodyHash     [32]byte
	ctHead       []byte
	writeAuth    [32]byte
	// MASTER section 8.4.2 v2's fourth preimage term and the digest it goes into, both rebuilt
	// from this reproduction's OWN rung. They are not octets of the record -- the digest travels
	// inside the frame, which is injected -- so they are not part of the four comparisons above;
	// what they are is ledger item 206's clause, and the test that reads them compares them
	// against the frame the sealer actually emitted.
	headBind [32]byte
	innerAad [32]byte
}

// reproduceRecordFromTheExporterOutput rebuilds one whole sealed record from mls_secret,
// pq_secret and server_nonce, using RFC 5869, XChaCha20-Poly1305 and HMAC-SHA-256 directly.
//
// The order is spec A section 5.2's and MASTER section 8's, and it is that order for the reason
// seal.go's staging types are: hash the attachment, seal ct_body, hash it, seal ct_head, mac the
// record. Nothing here reads a value it has not already computed.
func reproduceRecordFromTheExporterOutput(t *testing.T, mlsSecret []byte, pqSecret []byte,
	serverNonce []byte, shape keySourceShape) keySourceReproduction {

	t.Helper()

	// storage_root[n] = HKDF-Extract(salt = mls_secret[n], ikm = pq_secret[n]). The salt is
	// FIRST, which is guardrail G1: crypto/hkdf writes the two the other way round, and a
	// transposition here would produce a root that is thirty two well formed octets and that
	// the package's own StorageRoot does not compute.
	storageRoot := keyScheduleReferenceExtract(mlsSecret, pqSecret)

	// group_handle_key = HKDF-Expand(storage_root[0], "gh/v1", 32). The caller asserts the group
	// is at epoch zero, which is what makes the root in hand the epoch ZERO root.
	groupHandleKey := keyScheduleReferenceExpand(storageRoot,
		[]byte(keySourceGroupHandleInfo), keySourceKeyBytes)
	// sender_handle = HKDF-Expand(group_handle_key, "sh/v1" | LP(leaf_index), 16), on the four
	// octet reading of LP(leaf_index) that open item M1-8 will rule.
	senderHandle := [16]byte(keyScheduleReferenceExpand(groupHandleKey,
		append([]byte(keySourceSenderHandleInfo), recordKeyReferenceLP(shape.leaf)...),
		keySourceHandleBytes))

	// the durable class key, then this sender's ladder walked to the record's own index. The
	// walk is what makes stream_index a key input rather than a label: a record at index k is
	// sealed under the k'th rung and under no other.
	classKey := keyScheduleReferenceExpand(storageRoot,
		[]byte(keySourceDurableClassInfo), keySourceKeyBytes)
	recordKey := keyScheduleReferenceExpand(classKey,
		append([]byte(keySourceRecordKeyZeroInfo), recordKeyReferenceLP(shape.leaf)...),
		keySourceKeyBytes)
	for walked := uint64(0); walked < shape.streamIndex; walked += 1 {
		recordKey = keyScheduleReferenceExpand(recordKey,
			[]byte(keySourceRecordKeyNextInfo), keySourceKeyBytes)
	}

	// key | nonce, the key FIRST, which is the order MASTER section 8.1 writes and a
	// transposition of which produces two values of the right widths that seal against
	// themselves and against nothing else. The fifty six is the sum of the aead's own two
	// widths rather than a written down number, for keyschedule.go's reason.
	bodyMaterial := keyScheduleReferenceExpand(recordKey, []byte(keySourceAeadBodyInfo),
		keySourceAeadKeyBytes+keySourceAeadNonceBytes)
	headMaterial := keyScheduleReferenceExpand(recordKey, []byte(keySourceAeadHeadInfo),
		keySourceAeadKeyBytes+keySourceAeadNonceBytes)
	writeKey := keyScheduleReferenceExpand(storageRoot,
		[]byte(keySourceWriteKeyInfo), keySourceKeyBytes)

	attachmentHash := sha256.Sum256(shape.attachment)

	// aad_body, which carries no body_hash at all: guardrail G4 is that the body's aad cannot
	// depend on the hash of the body it is sealing.
	aadBody := keySourceJoin(
		[]byte(keySourceAadBodyLabel),
		keySourceU16(keySourceAeadAlgId),
		keySourceLP(shape.groupId[:]),
		keySourceLP(senderHandle[:]),
		keySourceU64(shape.epoch),
		keySourceU64(shape.streamIndex),
		[]byte{shape.retentionWire},
		keySourceU64(shape.ephWindow),
	)

	// LP(the ct_body plaintext) into a buffer exactly the rung, tail zero. Open item M1-7's
	// scheme, whose fill is pinned octet by octet in m1w1repairs_test.go rather than guessed at
	// here. Since MASTER section 8.4 the thing being prefixed is the INNER FRAME and not the
	// application body, and this is the one line in the reproduction that touches it -- a copy,
	// with no octet of it read.
	if shape.rungBytes < len(shape.innerFrame)+4 {
		t.Fatalf("a %d octet inner frame does not fit the %d octet rung this shape names",
			len(shape.innerFrame), shape.rungBytes)
	}
	padded := make([]byte, shape.rungBytes)
	copy(padded, keySourceLP(shape.innerFrame))

	ctBody := keySourceSeal(t, bodyMaterial, aadBody, padded)
	bodyHash := sha256.Sum256(ctBody)

	// aad_head, which covers every field of the header, body_hash included, which is what makes
	// the head's authentication cover the body it belongs to.
	aadHead := keySourceJoin(
		[]byte(keySourceAadHeadLabel),
		keySourceU16(keySourceAeadAlgId),
		keySourceLP(shape.groupId[:]),
		keySourceLP(senderHandle[:]),
		keySourceU64(shape.epoch),
		keySourceU64(shape.streamIndex),
		[]byte{keySourceIsCommitByte(shape.isCommit)},
		[]byte{shape.retentionWire},
		keySourceU64(shape.ephWindow),
		[]byte{shape.sizeBucket},
		keySourceU64(shape.expireAt),
		keySourceLP(bodyHash[:]),
		// unconditional, and a nil blob id writes the four zero octets.
		keySourceLP(shape.blobId),
		keySourceLP(attachmentHash[:]),
	)
	ctHead := keySourceSeal(t, headMaterial, aadHead, shape.headPlain)

	// write_auth's preimage, MASTER section 9.2. LP(H(ct_head)) is the HASH of ct_head and never
	// ct_head, which is the field a reader is most likely to write straight.
	ctHeadHash := sha256.Sum256(ctHead)
	preimage := keySourceJoin(
		[]byte(keySourceWriteAuthLabel),
		keySourceLP(serverNonce),
		keySourceLP(shape.groupId[:]),
		keySourceLP(senderHandle[:]),
		keySourceU64(shape.epoch),
		keySourceU64(shape.streamIndex),
		[]byte{keySourceIsCommitByte(shape.isCommit)},
		[]byte{shape.retentionWire},
		keySourceU64(shape.ephWindow),
		[]byte{shape.sizeBucket},
		keySourceU64(shape.expireAt),
		keySourceLP(ctHeadHash[:]),
		keySourceLP(bodyHash[:]),
		keySourceLP(shape.blobId),
		keySourceLP(attachmentHash[:]),
	)
	mac := hmac.New(sha256.New, writeKey)
	mac.Write(preimage)

	// MASTER section 8.4.2 v2's third and fourth terms, rebuilt from THIS reproduction's own
	// rung. head_bind_key is a fourth expansion off record_key[i] -- beside key_head, nonce_head,
	// key_body and nonce_body -- and head_commit is HMAC-SHA-256 under it over the head plaintext
	// exactly as it is sealed into ct_head, with no length prefix and no re-encoding.
	headBindKey := keyScheduleReferenceExpand(recordKey, []byte(keySourceHeadBindInfo), keySourceKeyBytes)
	headBinder := hmac.New(sha256.New, headBindKey)
	headBinder.Write(shape.headPlain)
	headBound := [32]byte(headBinder.Sum(nil))
	// and the 160 octet preimage: the 20 octet label, AAD_body's own 104 octets, four BIG ENDIAN
	// octets of generation and the 32 octet commitment, with no separator and no padding.
	innerAadPreimage := keySourceJoin(
		[]byte(keySourceAadMlsLabel),
		aadBody,
		keySourceU32(shape.generation),
		headBound[:],
	)

	return keySourceReproduction{
		senderHandle: senderHandle,
		ctBody:       ctBody,
		bodyHash:     bodyHash,
		ctHead:       ctHead,
		writeAuth:    [32]byte(mac.Sum(nil)),
		headBind:     headBound,
		innerAad:     sha256.Sum256(innerAadPreimage),
	}
}

// keySourceSeal runs XChaCha20-Poly1305 over one of the record's two plaintexts, splitting the
// fifty six octets of expansion the way MASTER section 8.1 does.
//
// It is chacha20poly1305.NewX and never chacha20poly1305.New: the two differ by one character
// and by twelve octets of nonce, and recordaead.go's file comment argues at length that nothing
// inside one implementation can tell them apart.
func keySourceSeal(t *testing.T, material []byte, aad []byte, plaintext []byte) []byte {
	t.Helper()
	if len(material) != keySourceAeadKeyBytes+keySourceAeadNonceBytes {
		t.Fatalf("the aead material is %d octets, want %d", len(material),
			keySourceAeadKeyBytes+keySourceAeadNonceBytes)
	}
	aead, err := chacha20poly1305.NewX(material[:keySourceAeadKeyBytes])
	if err != nil {
		t.Fatalf("build the record aead over a %d octet key: %v", keySourceAeadKeyBytes, err)
	}
	return aead.Seal(nil, material[keySourceAeadKeyBytes:], plaintext, aad)
}

// LP(x): a thirty two bit big endian length followed by x, which is the record layer's one
// length prefix. It is written out here rather than taken from mls/syntax for the reason the
// labels are transcribed: a prefix width that moved would otherwise move both halves together.
func keySourceLP(x []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(x))), x...)
}

func keySourceU16(v uint16) []byte {
	return binary.BigEndian.AppendUint16(nil, v)
}

func keySourceU64(v uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, v)
}

// u32, BIG ENDIAN, which MASTER section 8.4.2's term (3) fixes and which is the width RFC 9420
// gives SenderData.generation. Written out here for the reason every other encoder in this file is.
func keySourceU32(v uint32) []byte {
	return binary.BigEndian.AppendUint32(nil, v)
}

func keySourceIsCommitByte(isCommit bool) byte {
	if isCommit {
		return 1
	}
	return 0
}

// keySourceJoin concatenates the parts of a preimage in the order they are written.
func keySourceJoin(parts ...[]byte) []byte {
	joined := []byte{}
	for _, part := range parts {
		joined = append(joined, part...)
	}
	return joined
}

// keySourceShapeOf reads the PUBLIC half of a sealed record into a shape.
//
// It takes the head plaintext and the inner frame from the CALLER, which is what they are: the two
// things the reproduction is given rather than deriving. Everything else comes off the header, because everything else is a value anybody holding the record can
// read without a key -- and the two fields that are NOT such values, sender_handle and
// body_hash, have nowhere in the shape to go.
func keySourceShapeOf(t *testing.T, record *message.Record, leaf uint32,
	headPlain []byte, innerFrame []byte, generation uint32, frameAad []byte) keySourceShape {

	t.Helper()
	header := record.Header
	// the record is held against the TRANSCRIPTIONS, and the transcriptions are what the shape
	// then carries. This function used to call message.RetentionClassWire and
	// message.SizeBucketBytes and put their answers in the shape, which is two of the record
	// layer's own derivations producing octets the reproduction compares. What the package thinks
	// those two values are is asked on the other side of the gate, by
	// TestTheTranscribedRetentionAndSizeAgreeWithThePackage, and its answer never arrives here.
	if byte(header.RetentionClass) != keySourceDurableClassCode {
		t.Fatalf("this reproduction is written for MASTER section 8's durable class %#02x and the record carries %#02x",
			keySourceDurableClassCode, byte(header.RetentionClass))
	}
	if header.EphBucket != 0 {
		t.Fatalf("the durable row of MASTER section 8's table carries eph bucket 0 and the record carries %d",
			header.EphBucket)
	}
	// MASTER section 8's presence rule: the window is zero on permanent, durable and media
	// and on eph bucket 0. These records are durable, so a non zero one is a record this
	// transcription is not written for, and saying so here is what keeps the zero below from
	// being an assumption.
	if header.EphWindow != 0 {
		t.Fatalf("MASTER section 8 puts eph_window at zero on a durable record and this one carries %d",
			header.EphWindow)
	}
	if byte(header.SizeBucket) != keySourceSizeBucketCode {
		t.Fatalf("this reproduction is written for the %#02x rung of MASTER's ladder and the record carries %#02x",
			keySourceSizeBucketCode, byte(header.SizeBucket))
	}
	return keySourceShape{
		groupId:       header.GroupId,
		leaf:          leaf,
		epoch:         header.Epoch,
		streamIndex:   header.StreamIndex,
		isCommit:      header.IsCommit,
		retentionWire: keySourceDurableWire,
		ephWindow:     keySourceDurableEphWindow,
		sizeBucket:    keySourceSizeBucketCode,
		rungBytes:     keySourceRungBytes,
		expireAt:      header.ExpireAt,
		blobId:        header.BlobId,
		attachment:    header.ServerAttachment,
		headPlain:     headPlain,
		innerFrame:    innerFrame,
		generation:    generation,
		frameAad:      frameAad,
	}
}

// keySourceSealed is one fixture's worth of evidence: the three values the reproduction is
// allowed, the records that came out of a real session over a real group, and what those records
// opened back to.
//
// IT CARRIES NO SESSION AND NO GROUP HANDLE, and that is the boundary the gate at the bottom of
// this file rests on. keySourceSealRecords is the one function that gate excludes -- it has to
// call the record layer, because CP3b's bar is that the fixture is REAL -- and this type is what
// stops the exclusion leaking: nothing that crosses it can seal, open, export or derive. Only
// octets cross, and one subject. TestTheFixtureCanHandTheReproductionNothingItCouldSealWith
// holds that field by field, through this package's test types, so a *testSession put back here
// fails on the commit that puts it back.
type keySourceSealed struct {
	// the three values of the file header, taken here so that the reproduction's own caller
	// never has to reach the fixture for one.
	mlsSecret   []byte
	pqSecret    []byte
	serverNonce []byte
	// the sender's leaf index, which is public and is a SHAPE input rather than a key: the
	// record header carries sender_handle, which is the key schedule's function of it.
	leaf    uint32
	records []*message.Record
	heads   [][]byte
	bodies  [][]byte
	// the MLS PrivateMessage each record's ct_body carries, read back off the record by the
	// fixture and handed across as OCTETS. It is the fourth injected value of the file header,
	// and it crosses this boundary for the same reason server_nonce does: the reproduction
	// cannot compute it and nothing keys anything with it.
	innerFrames [][]byte
	// what the real OpenRecord answered for each record, and whether the sentinel it answered
	// was the INNER FRAME's. Both are computed inside this fixture because the assertion bodies
	// may not name a module declaration, and ErrRecordInnerFrame is one. See (6) in the file
	// header for what this binds and what it stopped binding.
	openRefusals               []string
	openRefusedAtTheInnerFrame []bool
	// the generation each frame was sealed at and the authenticated_data each carries in the
	// clear, read off the frame by the fixture. Neither is a key; see keySourceShape's own note
	// and ledger item 206 for why v2 makes them necessary.
	frameGenerations []uint32
	frameAads        [][]byte
}

// The head plaintext is EIGHTEEN octets on every record, so ct_head is thirty four, and every
// body lands on the 1 KiB rung, so ct_body is one thousand and forty. Both widths are asserted
// rather than left implicit: a reproduction that agreed with a record of the wrong shape would be
// agreeing about the wrong thing.
//
// THE SECOND OF THOSE MOVED ON 2026-09-15 and the first did not, which is itself the statement
// MASTER section 8.4 makes about scope: the head is not framed and its ciphertext is what it was.
const (
	keySourceHeadPlainBytes = 18
	keySourceRecordCount    = 3
)

// keySourceSealRecords founds a group, seals three records on one ladder, and collects the three
// values the reproduction is allowed to see.
//
// THREE RECORDS AND NOT ONE, because the ladder walk is a key input. The reserver hands out the
// first index above its high water, so these land on three consecutive rungs and the walk in the
// reproduction runs a different number of times for each; a single record at the ladder's head
// would leave the walk untaken and a defect in it invisible.
func keySourceSealRecords(t *testing.T, name string) *keySourceSealed {
	t.Helper()
	fixture := newTestSession(t, name)
	fixture.trackOwn(t)
	sealed := &keySourceSealed{
		pqSecret:    testPqSecret(),
		serverNonce: testServerNonce(),
		// the leaf is read HERE rather than in the reproduction's caller, because
		// GroupHandle.OwnLeafIndex is the record layer's and the caller is inside the gate.
		leaf: fixture.handle.OwnLeafIndex(),
	}
	for i := 0; i < keySourceRecordCount; i += 1 {
		head := []byte(fmt.Sprintf("record head %06d", i))
		if len(head) != keySourceHeadPlainBytes {
			t.Fatalf("the head plaintext is %d octets and this file is written for %d",
				len(head), keySourceHeadPlainBytes)
		}
		body := make([]byte, 100+i)
		for at := range body {
			body[at] = byte(at*11 + i)
		}
		record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, head, body, 0, nil)
		if err != nil {
			t.Fatalf("SealRecord %d: %v", i, err)
		}
		// THE FOURTH INJECTED VALUE, read back off the record HERE and nowhere else. It is the
		// MLS frame MASTER section 8.4 put inside ct_body, and this fixture is the one function
		// the gate below excludes precisely so that a REAL record can be produced and read; what
		// crosses the boundary is octets. Extracting it through the record layer's own
		// derivations does not blind anything: if a second key source moved any of them, the
		// extraction would still answer the frame that was sealed and the reproduction -- which
		// derives its rung from the exporter through RFC 5869 -- would rebuild a different
		// ct_body and go red.
		inner := keySourceInnerFrameOf(t, fixture, record)
		// THE FIFTH AND SIXTH INJECTED VALUES, read off that frame here and nowhere else. The
		// peek opens the frame's sender data under a secret every member of the group holds and
		// reads the cleartext authenticated_data beside it; neither answer is a key and neither
		// is something the reproduction could compute, which is exactly server_nonce's standing.
		_, frameAad, frameGeneration, err := peekInnerFrameSender(fixture.handle, inner)
		if err != nil {
			t.Fatalf("peek the inner frame of record %d: %v", i, err)
		}
		// THE OPEN HALF OF THE CLASS, run HERE for the same reason, and it is a REFUSAL now. A
		// member has no receiving ratchet for its own leaf, so this one member fixture cannot
		// open the frame it sealed (open item MG-4). What the refusal still binds is in (6) of
		// the file header: ErrRecordInnerFrame is reached only after the rung was derived, both
		// AEAD halves expanded, ct_head opened, ct_body opened and the padding read.
		_, _, openErr := fixture.session.OpenRecord(record)
		sealed.records = append(sealed.records, record)
		sealed.heads = append(sealed.heads, head)
		sealed.bodies = append(sealed.bodies, body)
		sealed.innerFrames = append(sealed.innerFrames, inner)
		sealed.openRefusals = append(sealed.openRefusals, fmt.Sprint(openErr))
		sealed.openRefusedAtTheInnerFrame = append(sealed.openRefusedAtTheInnerFrame,
			errors.Is(openErr, ErrRecordInnerFrame))
		sealed.frameGenerations = append(sealed.frameGenerations, frameGeneration)
		sealed.frameAads = append(sealed.frameAads, frameAad)
	}
	// the group's OWN exporter, under the label MASTER section 7 names, at the width it names.
	// This is the one secret the reproduction is handed, and it comes off the real mls.Group the
	// session is holding rather than off anything this file made up.
	mlsSecret, err := fixture.handle.Export(keySourceExporterLabel, nil, keySourceExporterBytes)
	if err != nil {
		t.Fatalf("export the epoch's mls_secret: %v", err)
	}
	if len(mlsSecret) != keySourceExporterBytes {
		t.Fatalf("the exporter answered %d octets, want %d", len(mlsSecret), keySourceExporterBytes)
	}
	sealed.mlsSecret = mlsSecret
	// epoch zero is ASSERTED and not assumed: group_handle_key is expanded from storage_root[0],
	// so at any later epoch this reproduction would be deriving sender_handle from a root it was
	// never given. See (3) in the file header.
	epoch, err := fixture.session.Epoch()
	if err != nil {
		t.Fatalf("read the session's epoch: %v", err)
	}
	if epoch != 0 {
		t.Fatalf("this reproduction is only legal at epoch 0 and the session is at epoch %d", epoch)
	}
	return sealed
}

// reproduce rebuilds record i from the three values and the record's public half.
func (self *keySourceSealed) reproduce(t *testing.T, i int, mlsSecret []byte) keySourceReproduction {
	t.Helper()
	return self.reproduceOverFrame(t, i, mlsSecret, self.innerFrames[i])
}

// reproduceOverFrame is reproduce with the injected frame supplied by the caller, which is what
// the compensating control needs: the frame is an INPUT, so the only way to show it is a live one
// is to hand in a different one and require the record to move.
func (self *keySourceSealed) reproduceOverFrame(t *testing.T, i int, mlsSecret []byte,
	inner []byte) keySourceReproduction {

	t.Helper()
	shape := keySourceShapeOf(t, self.records[i], self.leaf, self.heads[i], inner,
		self.frameGenerations[i], self.frameAads[i])
	return reproduceRecordFromTheExporterOutput(t, mlsSecret, self.pqSecret, self.serverNonce, shape)
}

// keySourceInnerFrameOf reads the MLS frame out of one sealed record's ct_body.
//
// It is the record layer's own open, written out here rather than called through OpenRecord,
// because OpenRecord goes one step further and hands the frame to Unprotect -- which this one
// member fixture cannot do for its own leaf. What it needs is the step before that: the padded
// plaintext, unpadded.
//
// IT IS INSIDE THE EXCLUDED FUNCTION'S REACH AND NOWHERE ELSE. Only keySourceSealRecords calls it,
// and the gate at the bottom of this file asserts that the exclusion covers exactly one function
// and that nothing else in scope reaches into it.
func keySourceInnerFrameOf(t *testing.T, fixture *testSession, record *message.Record) []byte {
	t.Helper()
	recordKey := RecordKeyZero(fixture.session.classKeys.Durable, fixture.handle.OwnLeafIndex())
	for walked := uint64(0); walked < record.Header.StreamIndex; walked += 1 {
		recordKey = RecordKeyNext(recordKey)
	}
	defer zeroize(recordKey)
	aadBody, err := message.AADBody(RecordAeadAlgId, record.Header.BodyBinding())
	if err != nil {
		t.Fatalf("AADBody while reading the inner frame back: %v", err)
	}
	bodyKey, bodyNonce := RecordAeadBody(recordKey)
	defer zeroize(bodyKey)
	defer zeroize(bodyNonce)
	padded, err := openRecordAead(bodyKey, bodyNonce, aadBody, record.CtBody)
	if err != nil {
		t.Fatalf("open ct_body while reading the inner frame back: %v", err)
	}
	inner, err := unpadBody(record.Header.SizeBucket, padded)
	if err != nil {
		t.Fatalf("unpad ct_body while reading the inner frame back: %v", err)
	}
	if len(inner) == 0 {
		t.Fatal("the record's ct_body carries no inner frame, so the fourth injected value is empty")
	}
	return inner
}

// CP3b's bar, standing: no test-only key source anywhere on the path.
//
// This is the only test in this package that can go red when somebody introduces a key source
// that is not the MLS key schedule. Every other case here observes that the record layer agrees
// with ITSELF -- it round trips, it refuses what it should, its ladder does not repeat -- and
// all of those stay green under a second key source, because a second key source is still well
// formed, still deterministic and still round trips.
func TestEveryKeyedOctetOfARecordIsReproducibleFromTheExporterAndTheTwoInjectedValuesAlone(t *testing.T) {
	sealed := keySourceSealRecords(t, "no-second-key-source")
	for i, record := range sealed.records {
		got := sealed.reproduce(t, i, sealed.mlsSecret)
		// the two widths, so that an agreement about a record of the wrong shape is not read as
		// an agreement about this one.
		if len(record.CtHead) != keySourceHeadPlainBytes+keySourceAeadTagBytes {
			t.Errorf("record %d: ct_head is %d octets, want %d",
				i, len(record.CtHead), keySourceHeadPlainBytes+keySourceAeadTagBytes)
		}
		if len(record.CtBody) != keySourceRungBytes+keySourceAeadTagBytes {
			t.Errorf("record %d: ct_body is %d octets, want %d",
				i, len(record.CtBody), keySourceRungBytes+keySourceAeadTagBytes)
		}
		if got.senderHandle != record.Header.SenderHandle {
			t.Errorf("record %d: sender_handle rebuilt from the exporter output is %x and the record carries %x; a handle the exporter does not produce is a handle from a second key source",
				i, got.senderHandle, record.Header.SenderHandle)
		}
		if string(got.ctBody) != string(record.CtBody) {
			t.Errorf("record %d: ct_body rebuilt from the exporter output is %x and the record carries %x",
				i, got.ctBody, record.CtBody)
		}
		if got.bodyHash != record.Header.BodyHash {
			t.Errorf("record %d: body_hash rebuilt from the exporter output is %x and the record carries %x",
				i, got.bodyHash, record.Header.BodyHash)
		}
		if string(got.ctHead) != string(record.CtHead) {
			t.Errorf("record %d: ct_head rebuilt from the exporter output is %x and the record carries %x",
				i, got.ctHead, record.CtHead)
		}
		if got.writeAuth != record.WriteAuth {
			t.Errorf("record %d: write_auth rebuilt from the exporter output is %x and the record carries %x; a mac key the exporter does not produce is a mac key from a second key source",
				i, got.writeAuth, record.WriteAuth)
		}
		// and the OPEN half of the class, bound to the same reproduction and NARROWED on
		// 2026-09-15. See (6) in the file header. The open ran in the fixture, which is the one
		// function outside the gate below; what is compared here is where it refused.
		//
		// WHAT THIS STILL BINDS: ErrRecordInnerFrame is produced only after openRecordOnLoop has
		// derived the rung, expanded both AEAD halves, opened ct_head, opened ct_body and read
		// the padding, so a second key source on the open side alone cannot reach it -- the
		// record would fail in an AEAD, with a different sentinel, and this clause would be red.
		// WHAT IT NO LONGER BINDS: that the body came back as the body. Nothing in a one member
		// fixture can open the frame, and the file header says so rather than leaving it to be
		// inferred.
		if !sealed.openRefusedAtTheInnerFrame[i] {
			t.Errorf("record %d: the sealer's own OpenRecord answered %q; it must reach the inner frame and refuse there, because everything the open side derives has already run by that point and a refusal anywhere earlier is a second key source on the open side",
				i, sealed.openRefusals[i])
		}
	}
	t.Logf("%d records rebuilt byte for byte from Export(%q, nil, %d), pq_secret, server_nonce and the injected %d octet inner frame",
		len(sealed.records), keySourceExporterLabel, keySourceExporterBytes, len(sealed.innerFrames[0]))
}

// THE COMPENSATING CONTROL the 2026-09-15 repair owes, and it is what stops the fourth injected
// value from being decoration.
//
// The frame is an INPUT the reproduction copies and never reads, which is exactly the shape a
// reader should be suspicious of: a reproduction that ignored it and rebuilt the body some other
// way would agree with the record forever. So every octet of it is moved in turn and all three of
// the record's body-derived outputs are required to move with it -- ct_body because the frame is
// what is sealed, body_hash because it is H(ct_body), and write_auth because its preimage carries
// body_hash.
//
// ct_head is NOT required to move and that is the point of listing which three are: the head is
// sealed under its own half of the same rung over a preimage that carries body_hash, so it moves
// too -- and requiring it here would be requiring the same fact twice. The three named are the
// ones whose dependence on the injected value is direct.
func TestFlippingAnyOctetOfTheInjectedFrameMovesTheBody(t *testing.T) {
	sealed := keySourceSealRecords(t, "the-injected-frame-is-live")
	base := sealed.reproduce(t, 0, sealed.mlsSecret)
	record := sealed.records[0]
	if string(base.ctBody) != string(record.CtBody) || base.bodyHash != record.Header.BodyHash ||
		base.writeAuth != record.WriteAuth {
		t.Fatal("the unflipped reproduction is not the record, so nothing this control observes is about the record")
	}
	frame := sealed.innerFrames[0]
	if len(frame) == 0 {
		t.Fatal("the injected frame is empty, so this control flips nothing")
	}
	moved := 0
	for octet := range frame {
		flipped := append([]byte(nil), frame...)
		flipped[octet] ^= 0x01
		got := sealed.reproduceOverFrame(t, 0, sealed.mlsSecret, flipped)
		if string(got.ctBody) == string(base.ctBody) {
			t.Errorf("octet %d of the injected frame does not reach ct_body", octet)
		}
		if got.bodyHash == base.bodyHash {
			t.Errorf("octet %d of the injected frame does not reach body_hash", octet)
		}
		if got.writeAuth == base.writeAuth {
			t.Errorf("octet %d of the injected frame does not reach write_auth", octet)
		}
		moved += 1
	}
	if moved != len(frame) {
		t.Errorf("%d octets were flipped and the frame is %d octets", moved, len(frame))
	}
	t.Logf("every one of the %d octets of the injected frame moves ct_body, body_hash and write_auth", len(frame))
}

// The negative control, and it matters as much as the reproduction does.
//
// A reproduction that could not FAIL would certify anything, and this project has shipped nine
// consecutive tasks' worth of tests that could not. So every one of the 256 bits of the exporter
// output is flipped in turn and all four outputs are required to move: if any bit of mls_secret
// could be changed without moving ct_body, ct_head, sender_handle and write_auth, then that part
// of the record is not a function of the exporter output at all, which is the same defect stated
// from the other side.
//
// It flips the EXPORTER OUTPUT specifically, because that is the value CP3b is about. That
// pq_secret and server_nonce are also live inputs is held by
// TestTheStorageRootDependsOnTheInjectedPqSecret and by message/writeauth_test.go's nonce cases.
func TestFlippingAnyBitOfTheExporterOutputChangesEveryKeyedOctetOfARecord(t *testing.T) {
	sealed := keySourceSealRecords(t, "one-bit-of-the-exporter")
	// the control is anchored to reality first: the unflipped reproduction IS the record, so a
	// difference below is a difference from the record and not from some third thing.
	base := sealed.reproduce(t, 0, sealed.mlsSecret)
	record := sealed.records[0]
	if base.senderHandle != record.Header.SenderHandle || string(base.ctBody) != string(record.CtBody) ||
		string(base.ctHead) != string(record.CtHead) || base.writeAuth != record.WriteAuth {
		t.Fatal("the unflipped reproduction is not the record, so nothing this control observes is about the record")
	}
	flips := 0
	for octet := range sealed.mlsSecret {
		for bit := 0; bit < 8; bit += 1 {
			flipped := append([]byte(nil), sealed.mlsSecret...)
			flipped[octet] ^= 1 << bit
			got := sealed.reproduce(t, 0, flipped)
			if got.senderHandle == base.senderHandle {
				t.Errorf("bit %d of octet %d of the exporter output does not reach sender_handle", bit, octet)
			}
			if string(got.ctBody) == string(base.ctBody) {
				t.Errorf("bit %d of octet %d of the exporter output does not reach ct_body", bit, octet)
			}
			if string(got.ctHead) == string(base.ctHead) {
				t.Errorf("bit %d of octet %d of the exporter output does not reach ct_head", bit, octet)
			}
			if got.writeAuth == base.writeAuth {
				t.Errorf("bit %d of octet %d of the exporter output does not reach write_auth", bit, octet)
			}
			flips += 1
		}
	}
	if flips != len(sealed.mlsSecret)*8 {
		t.Errorf("%d bit flips were tried and the exporter output is %d octets", flips, len(sealed.mlsSecret))
	}
}

// The two table values keySourceShapeOf transcribes, held against the package that ships them.
//
// IT IS ON THE OTHER SIDE OF THE GATE ON PURPOSE. This is the one place the record layer is
// asked what it thinks the durable wire byte and the 1 KiB rung are, and its answer is
// compared against a transcription and thrown away -- it reaches no shape and no preimage. Until
// this test existed the comparison happened inside keySourceShapeOf and the package's answer WAS
// the shape's, which put two derivations of the record layer on the reproduction's side.
//
// The retention class tag and the size bucket tag are pinned too, because they are what
// keySourceShapeOf holds the record against: a tag that moved without this test would make every
// record fail the shape check with no statement about which of the two was wrong.
func TestTheTranscribedRetentionAndSizeAgreeWithThePackage(t *testing.T) {
	if byte(message.RetentionDurable) != keySourceDurableClassCode {
		t.Errorf("message.RetentionDurable is %#02x and MASTER section 8's durable row is transcribed here as %#02x",
			byte(message.RetentionDurable), keySourceDurableClassCode)
	}
	wire, err := message.RetentionClassWire(message.RetentionDurable, 0)
	if err != nil {
		t.Fatalf("join the durable class and bucket 0: %v", err)
	}
	if wire != keySourceDurableWire {
		t.Errorf("the durable class joins to wire byte %#02x and MASTER section 8's table is transcribed here as %#02x",
			wire, keySourceDurableWire)
	}
	if byte(message.SizeBucket1K) != keySourceSizeBucketCode {
		t.Errorf("message.SizeBucket1K is %#02x and this file transcribes the rung's tag as %#02x",
			byte(message.SizeBucket1K), keySourceSizeBucketCode)
	}
	if rung := message.SizeBucketBytes(message.SizeBucket(keySourceSizeBucketCode)); rung != keySourceRungBytes {
		t.Errorf("size bucket %#02x is %d octets on the ladder and this file transcribes it as %d",
			keySourceSizeBucketCode, rung, keySourceRungBytes)
	}
}

// ---------------------------------------------------------------------------
// the gate that holds the reproduction's independence, DERIVED FROM THE PROPERTY IT NAMES
// ---------------------------------------------------------------------------

// The whole of what makes the reproduction above evidence rather than a tautology is that no
// code of the module under test produces any octet it compares. That claim was a paragraph; then
// it was a gate over the wrong class; this is the third version, and the difference between the
// second and the third is the whole subject of this comment.
//
// WHAT THE SECOND VERSION GOT WRONG, because it is the defect this project keeps repeating: its
// class was "identifiers declared in connect/messagegroup" -- the DIRECTORY the file happens to
// sit in -- and the property is "code that produces the record's keyed octets". Those are not
// the same set, and a reviewer walked through the gap three times:
//
//	(1) message.WriteKey, message.ComputeWriteAuth, message.AADHead and message.AADBody are four
//	    of the fourteen names the file header bans by name, all on SealRecord's path, all in the
//	    OTHER package of the record layer -- and all outside a class derived from this directory.
//	    A second key source planted in message.WriteKey went red on the reproduction; pointing
//	    the reproduction at message.WriteKey turned all three tests green with the gate still
//	    green.
//	(2) The matcher read *ast.CallExpr nodes and nothing else, so a production package level
//	    const or var was invisible to it. A hard coded second key source planted as a package
//	    constant, read by the reproduction, passed everything.
//	(3) The scope was the closure seeded at reproduceRecordFromTheExporterOutput, which EXCLUDES
//	    the two test bodies that carry the comparisons -- so a production derivation called from
//	    the assertion side could make a comparison tautological and stay invisible.
//
// All three are one error at three altitudes: the artefact was derived from the INSTANCE the
// property was first seen in rather than from the property. So all three are re-derived.
//
// THE CLASS IS EVERY NAME THE MODULE DECLARES. The module path is read out of go.mod, the
// package set is walked from this package and from its test source's imports through every
// module internal import transitively, and the class is every function, every method, every
// package level constant, every package level variable and every type their production source
// declares. connect/message, connect/mls and connect/mls/syntax are in it today because this
// package's production source imports them, and a package the next import adds is in it with no
// edit here. message.WriteKey is a member. So is a constant nobody has written yet.
//
// THE REACH IS CALLS AND REFERENCES. A name is reached whether it is called, read as a constant,
// taken as a value or named as a type. That is (2), and it is why the matcher walks identifiers
// rather than call expressions.
//
// THE SCOPE IS THE WHOLE COMPARISON. It is walked in BOTH directions from
// reproduceRecordFromTheExporterOutput: backwards to every test function that transitively
// reaches it -- which is what the two assertion bodies are -- and forwards from each of those
// through everything they reach. That is (3), and the gate asserts the backward walk found
// something, because a backward walk that found nothing reports the clean run a complete one
// reports.
//
// ONE FUNCTION IS OUTSIDE THE SCOPE, AND IT IS DERIVED RATHER THAN NAMED. CP3b's bar is that the
// fixture is REAL: a real mls.Group, the real sealer, the real open. So the function that founds
// the group, seals the records, opens them and takes the exporter's answer must call the record
// layer, and a gate that banned that would ban the evidence. It is found by its RESULT TYPE, as
// the one test function that answers a *keySourceSealed, and the gate fails if there is not
// exactly one -- a second one would be a second door, and writing this one's name in a list
// would be the same enumeration this section exists to undo. The gate also fails if anything
// else in the scope reaches into that function's closure other than through the function itself,
// because an exclusion that had grown to swallow an assertion body would clear it in silence.
//
// AND WHAT IT HANDS ACROSS IS A TYPE. keySourceSealed carries the three values, the leaf, the
// records and what they opened to, and no live session and no group handle -- so the excluded
// function cannot pass its reach along. TestTheFixtureCanHandTheReproductionNothingItCouldSealWith
// holds that field by field and THROUGH this package's test structs, so a *testSession put back
// on the boundary fails even though *testSession is a test type.
//
// ---------------------------------------------------------------------------
// WHAT THIS GATE STILL CANNOT SEE, stated rather than implied
// ---------------------------------------------------------------------------
//
// (a) THE SUBJECT PRODUCER ITSELF, which is the price of the exclusion above. A second key
// source MIRRORED in it -- a fixture that perturbs the exporter's answer the same way a
// perturbed production side does -- reproduces and passes. Two things stand against that and
// neither is this gate: the boundary type, which is why nothing but octets crosses, and that
// such an edit is a deliberate change to the fixture rather than to a derivation.
//
// (b) RESOLUTION IS SYNTACTIC. Names are resolved by their qualifier and not by go/types, which
// this module cannot reach: golang.org/x/tools is not a dependency and this gate is not worth
// adding one for. A selector rooted at an imported non module package, or at a local whose value
// came from one, is read as that package's -- which is what keeps aead.Seal from being read as
// connect/mls's Seal, since connect/mls really does declare a Seal. A selector the walk cannot
// root falls back to matching the selected name against the module's declarations, which OVER
// reports rather than under: a standard library method that one day shares a name with a module
// function fails here, naming the call and the file. That is the safe direction for a ban, and
// it is the direction mls/crypto_forbidden_test.go argues for its own matcher. A FIELD read off
// an unrooted value is not matched, because the record's public half is read exactly that way
// and is one of the reproduction's declared inputs rather than a derivation.
//
// (c) DYNAMIC REACH. A module function reached through a value this walk cannot follow -- a func
// typed struct field, a method value in a map -- is outside the name matching above. Nothing in
// the scope does that today and nothing here would say so if it started.
//
// (d) A SECOND DERIVATION OF THE SAME MATERIAL, unchanged from (1) in the file header. This gate
// holds that no module code produced these octets. It does not hold that the octets could not
// have been produced twice.

// The four controls, one per shape the matcher has to recognise, and nothing calls any of them.
//
// They exist so that a matcher which stopped matching fails HERE rather than reporting the
// reproduction clean having recognised nothing -- the same shape mls/crypto_forbidden_test.go's
// testdata/forbidden fixture has. Two of them are the reviewer's escapes written down as code:
// the constant read is (2), and the connect/message pair is (1). The gate asserts each is
// OUTSIDE the scope before it believes what the matcher said about it, because a control that
// had drifted into the scope would be a control reporting on itself.
func keySourceControlThatCallsThisPackagesDerivation(recordKey []byte) ([]byte, []byte) {
	return RecordAeadBody(recordKey)
}

func keySourceControlThatReadsThisPackagesConstant() uint16 {
	return RecordAeadAlgId
}

func keySourceControlThatCallsAnotherModulePackage(storageRoot []byte) []byte {
	return message.WriteKey(storageRoot)
}

func keySourceControlThatReadsAnotherModulePackage() message.RetentionClass {
	return message.RetentionDurable
}

// keySourceModulePackage is one package of the module under test, with every name its production
// source declares.
type keySourceModulePackage struct {
	importPath string
	funcs      map[string]bool
	values     map[string]bool
	types      map[string]bool
}

// keySourceTestFile is one of this package's test files, with the imports a name written in it
// resolves through. Resolution is per FILE because import names are.
type keySourceTestFile struct {
	path          string
	moduleImports map[string]string
	otherImports  map[string]bool
	dotImports    []string
}

// keySourceTestFunction is one function of this package's test source, with the file it was
// written in.
type keySourceTestFunction struct {
	name string
	decl *ast.FuncDecl
	file *keySourceTestFile
}

// keySourceTestStruct is one struct type of this package's test source, with the file it was
// written in -- which is the file its field types resolve through, and which is read off the
// declaration rather than guessed at by name.
type keySourceTestStruct struct {
	decl *ast.StructType
	file *keySourceTestFile
}

// keySourceReach is one name of the class that a scanned body reaches, with how it reached it.
type keySourceReach struct {
	from  string
	file  string
	name  string
	owner string
	kind  string
}

func (self keySourceReach) String() string {
	return fmt.Sprintf("%s (%s) %s %s, which %s declares", self.from, self.file, self.kind, self.name, self.owner)
}

// keySourceModuleRoot walks up from this package to the go.mod that declares the module, and
// answers the directory holding it and the module path it declares.
//
// Both are READ rather than written down, which is what makes "the module under test" a fact of
// the tree: a module rename carries this gate's class with it instead of emptying it.
func keySourceModuleRoot(t *testing.T) (string, string) {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package's directory: %v", err)
	}
	for {
		source, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(source), "\n") {
				if path, isModule := strings.CutPrefix(strings.TrimSpace(line), "module "); isModule {
					return dir, strings.TrimSpace(path)
				}
			}
			t.Fatalf("%s declares no module path, so this gate has no module to derive a class from",
				filepath.Join(dir, "go.mod"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package, so this gate cannot derive the module under test")
		}
		dir = parent
	}
}

// keySourceReadModulePackage reads one package's production declarations, and answers the module
// internal imports it holds, which is how the package set below grows.
func keySourceReadModulePackage(t *testing.T, importPath string, dir string,
	modulePath string) (*keySourceModulePackage, []string) {

	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s, which %s names: %v", dir, importPath, err)
	}
	read := &keySourceModulePackage{
		importPath: importPath,
		funcs:      map[string]bool{},
		values:     map[string]bool{},
		types:      map[string]bool{},
	}
	imports := []string{}
	fileSet := token.NewFileSet()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, name))
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files += 1
		for _, imported := range parsed.Imports {
			held := strings.Trim(imported.Path.Value, `"`)
			if held == modulePath || strings.HasPrefix(held, modulePath+"/") {
				imports = append(imports, held)
			}
		}
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				read.funcs[declaration.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							// the blank identifier names nothing. `var _ GroupHandle = ...` is an
							// interface satisfaction assertion, and reading it into the class
							// makes every `for _, x := range` in the scope a reach.
							if name.Name != "_" {
								read.values[name.Name] = true
							}
						}
					case *ast.TypeSpec:
						read.types[spec.Name.Name] = true
					}
				}
			}
		}
	}
	if files == 0 {
		t.Fatalf("%s holds no production go file, so this gate read an empty class out of %s", dir, importPath)
	}
	return read, imports
}

// keySourceModuleClass is THE CLASS: every name the production source of every module package
// this package or its test source can reach declares.
//
// The seeds are this package itself and every module package its TEST source imports, so a test
// file reaching for a package the production source does not import still hands the gate the
// class it would need to see it. Growth from there is transitive and needs no edit here.
func keySourceModuleClass(t *testing.T, root string, modulePath string,
	files []*keySourceTestFile) (map[string]*keySourceModulePackage, string) {

	t.Helper()
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package's directory: %v", err)
	}
	relative, err := filepath.Rel(root, here)
	if err != nil {
		t.Fatalf("place this package inside %s: %v", root, err)
	}
	selfImport := modulePath
	if relative != "." {
		selfImport = modulePath + "/" + filepath.ToSlash(relative)
	}
	frontier := []string{selfImport}
	for _, file := range files {
		for _, path := range file.moduleImports {
			frontier = append(frontier, path)
		}
	}
	class := map[string]*keySourceModulePackage{}
	for 0 < len(frontier) {
		importPath := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if _, isRead := class[importPath]; isRead {
			continue
		}
		dir := root
		if importPath != modulePath {
			dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(importPath, modulePath+"/")))
		}
		read, imports := keySourceReadModulePackage(t, importPath, dir, modulePath)
		class[importPath] = read
		frontier = append(frontier, imports...)
	}
	if _, isRead := class[selfImport]; !isRead {
		t.Fatalf("%s was not read into the class, so the gate banned nothing this package declares", selfImport)
	}
	return class, selfImport
}

// keySourceTestSource reads this package's test source: every function by name, every file's
// import resolution, and every struct type, which the boundary check below walks.
func keySourceTestSource(t *testing.T, modulePath string) (map[string]*keySourceTestFunction,
	[]*keySourceTestFile, map[string]*keySourceTestStruct) {

	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	fileSet := token.NewFileSet()
	functions := map[string]*keySourceTestFunction{}
	files := []*keySourceTestFile{}
	structs := map[string]*keySourceTestStruct{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(".", name))
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		file := &keySourceTestFile{
			path:          path,
			moduleImports: map[string]string{},
			otherImports:  map[string]bool{},
		}
		for _, imported := range parsed.Imports {
			held := strings.Trim(imported.Path.Value, `"`)
			local := held[strings.LastIndex(held, "/")+1:]
			if imported.Name != nil {
				local = imported.Name.Name
			}
			if local == "." {
				file.dotImports = append(file.dotImports, held)
				continue
			}
			if held == modulePath || strings.HasPrefix(held, modulePath+"/") {
				file.moduleImports[local] = held
				continue
			}
			file.otherImports[local] = true
		}
		files = append(files, file)
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Body == nil {
					continue
				}
				// a method and a function of one name collapse to one entry, which is the safe
				// direction: it pulls MORE source into the walk rather than less.
				functions[declaration.Name.Name] = &keySourceTestFunction{
					name: declaration.Name.Name, decl: declaration, file: file,
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					typed, isType := spec.(*ast.TypeSpec)
					if !isType {
						continue
					}
					if structure, isStruct := typed.Type.(*ast.StructType); isStruct {
						structs[typed.Name.Name] = &keySourceTestStruct{decl: structure, file: file}
					}
				}
			}
		}
	}
	if len(functions) == 0 {
		t.Fatal("no test function was read out of this package, so this gate scoped itself to nothing")
	}
	return functions, files, structs
}

// keySourceExprRoot is the leftmost identifier of an expression: the thing a selector chain, a
// call or a type is written on.
func keySourceExprRoot(node ast.Expr) string {
	for {
		switch typed := node.(type) {
		case nil:
			return ""
		case *ast.Ident:
			return typed.Name
		case *ast.SelectorExpr:
			node = typed.X
		case *ast.CallExpr:
			node = typed.Fun
		case *ast.IndexExpr:
			node = typed.X
		case *ast.StarExpr:
			node = typed.X
		case *ast.ParenExpr:
			node = typed.X
		case *ast.UnaryExpr:
			node = typed.X
		case *ast.SliceExpr:
			node = typed.X
		case *ast.TypeAssertExpr:
			node = typed.X
		case *ast.CompositeLit:
			node = typed.Type
		case *ast.ArrayType:
			node = typed.Elt
		case *ast.Ellipsis:
			node = typed.Elt
		default:
			return ""
		}
	}
}

// keySourceNonModuleRoots is every name inside one function that resolves to a package OUTSIDE
// this module, so a selector written on it is that package's rather than a name to match.
//
// It is what stops aead.Seal from being read as connect/mls's Seal: aead came out of
// chacha20poly1305.NewX, and t came in as a *testing.T. Without it the widened class goes red on
// the standard library, which is a gate that has to be turned off rather than one that holds.
func keySourceNonModuleRoots(function *keySourceTestFunction) map[string]bool {
	rooted := map[string]bool{}
	for _, list := range []*ast.FieldList{function.decl.Recv, function.decl.Type.Params, function.decl.Type.Results} {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			if !function.file.otherImports[keySourceExprRoot(field.Type)] {
				continue
			}
			for _, name := range field.Names {
				rooted[name.Name] = true
			}
		}
	}
	note := func(targets []ast.Expr, values []ast.Expr) bool {
		grew := false
		for at, value := range values {
			root := keySourceExprRoot(value)
			if !function.file.otherImports[root] && !rooted[root] {
				continue
			}
			taking := targets
			if len(values) == len(targets) {
				taking = targets[at : at+1]
			}
			for _, target := range taking {
				name, isIdent := target.(*ast.Ident)
				if isIdent && !rooted[name.Name] {
					rooted[name.Name] = true
					grew = true
				}
			}
		}
		return grew
	}
	// to a fixpoint, because one local's package can arrive through another's.
	for pass := 0; pass < 8; pass += 1 {
		grew := false
		ast.Inspect(function.decl.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				grew = note(typed.Lhs, typed.Rhs) || grew
			case *ast.ValueSpec:
				targets := []ast.Expr{}
				for _, name := range typed.Names {
					targets = append(targets, name)
				}
				if function.file.otherImports[keySourceExprRoot(typed.Type)] {
					grew = note(targets, []ast.Expr{typed.Type}) || grew
				}
				grew = note(targets, typed.Values) || grew
			}
			return true
		})
		if !grew {
			break
		}
	}
	return rooted
}

// keySourceOwnerOf is the module package that declares a name, or the empty string. Packages are
// consulted in a fixed order, so a name two of them declare reports the same one every run.
func keySourceOwnerOf(name string, class map[string]*keySourceModulePackage, functionsOnly bool) string {
	for _, importPath := range keySourceSortedKeys(class) {
		read := class[importPath]
		if read.funcs[name] {
			return importPath
		}
		if !functionsOnly && (read.values[name] || read.types[name]) {
			return importPath
		}
	}
	return ""
}

// keySourceModuleReaches is THE MATCHER: every name of the class one function's body reaches,
// through a call, a constant read, a value or a type.
func keySourceModuleReaches(function *keySourceTestFunction, class map[string]*keySourceModulePackage,
	selfImport string) []keySourceReach {

	body := function.decl.Body
	rooted := keySourceNonModuleRoots(function)
	selected := map[*ast.Ident]bool{}
	keyed := map[*ast.Ident]bool{}
	called := map[ast.Expr]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			selected[typed.Sel] = true
		case *ast.KeyValueExpr:
			if name, isIdent := typed.Key.(*ast.Ident); isIdent {
				keyed[name] = true
			}
		case *ast.CallExpr:
			called[typed.Fun] = true
		}
		return true
	})
	found := []keySourceReach{}
	report := func(name string, owner string, kind string) {
		found = append(found, keySourceReach{
			from: function.name, file: function.file.path, name: name, owner: owner, kind: kind,
		})
	}
	kindOf := func(node ast.Expr) string {
		if called[node] {
			return "calls"
		}
		return "reads"
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			root := keySourceExprRoot(typed.X)
			if path, isModulePackage := function.file.moduleImports[root]; isModulePackage {
				report(root+"."+typed.Sel.Name, path, kindOf(typed))
				return true
			}
			if function.file.otherImports[root] || rooted[root] {
				return true
			}
			// a selector on a value this walk cannot root. Only a CALL is matched: a field read
			// off an unrooted value is how the record's public half is read, and that is one of
			// the reproduction's declared inputs rather than a derivation.
			if called[typed] {
				if owner := keySourceOwnerOf(typed.Sel.Name, class, true); owner != "" {
					report(typed.Sel.Name, owner, "calls")
				}
			}
		case *ast.Ident:
			if selected[typed] || keyed[typed] || typed.Name == "_" {
				return true
			}
			// unqualified, so it can only be this package's own production source: every other
			// package of the module has to be reached through an import name, and the gate
			// refuses a dot import for exactly that reason.
			own := class[selfImport]
			if own.funcs[typed.Name] || own.values[typed.Name] || own.types[typed.Name] {
				report(typed.Name, selfImport, kindOf(typed))
			}
		}
		return true
	})
	lines := []string{}
	seen := map[string]bool{}
	compacted := []keySourceReach{}
	for _, reach := range found {
		line := reach.String()
		if seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	slices.Sort(lines)
	for _, line := range lines {
		for _, reach := range found {
			if reach.String() == line {
				compacted = append(compacted, reach)
				break
			}
		}
	}
	return compacted
}

// keySourceMentionedNames is every name a body mentions: a bare identifier by its own name, a
// selector by the name it selects.
//
// The call graph below is built out of this rather than out of call expressions, because a
// function reached as a VALUE is still reached.
func keySourceMentionedNames(function *ast.FuncDecl) []string {
	names := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			names = append(names, typed.Name)
		case *ast.SelectorExpr:
			names = append(names, typed.Sel.Name)
		}
		return true
	})
	return names
}

// keySourceForwardClosure is every test function reachable from the seeds, the seeds included.
func keySourceForwardClosure(seeds []string, functions map[string]*keySourceTestFunction) map[string]bool {
	closure := map[string]bool{}
	frontier := []string{}
	for _, seed := range seeds {
		closure[seed] = true
		frontier = append(frontier, seed)
	}
	for 0 < len(frontier) {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		function, isDeclared := functions[name]
		if !isDeclared {
			continue
		}
		for _, mentioned := range keySourceMentionedNames(function.decl) {
			if closure[mentioned] {
				continue
			}
			if _, isTestFunction := functions[mentioned]; !isTestFunction {
				continue
			}
			closure[mentioned] = true
			frontier = append(frontier, mentioned)
		}
	}
	return closure
}

// keySourceBackwardClosure is every test function that transitively REACHES the seed, the seed
// included. It is what puts the assertion bodies inside this gate.
func keySourceBackwardClosure(seed string, functions map[string]*keySourceTestFunction) map[string]bool {
	callers := map[string][]string{}
	for name, function := range functions {
		for _, mentioned := range keySourceMentionedNames(function.decl) {
			if _, isTestFunction := functions[mentioned]; isTestFunction {
				callers[mentioned] = append(callers[mentioned], name)
			}
		}
	}
	closure := map[string]bool{seed: true}
	frontier := []string{seed}
	for 0 < len(frontier) {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, caller := range callers[name] {
			if closure[caller] {
				continue
			}
			closure[caller] = true
			frontier = append(frontier, caller)
		}
	}
	return closure
}

// keySourceSortedKeys is the keys of a map in one order, so every list this gate prints and every
// package order it resolves through is the same on every run.
func keySourceSortedKeys[V any](held map[string]V) []string {
	keys := []string{}
	for key := range held {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// keySourceNamesTheFixture answers whether a result list names keySourceSealed, which is how the
// subject producer is found without writing its name down.
func keySourceNamesTheFixture(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	named := false
	for _, field := range results.List {
		ast.Inspect(field.Type, func(node ast.Node) bool {
			if name, isIdent := node.(*ast.Ident); isIdent && name.Name == "keySourceSealed" {
				named = true
			}
			return true
		})
	}
	return named
}

// The reproduction's whole side of the comparison reaches nothing the module under test ships.
//
// A reproduction that reached for the record layer's own derivations would rebuild the record
// under whatever key the record layer chose -- one drawn from a second source included -- and
// would agree with it forever. So would an assertion body that recomputed an expected octet the
// same way. That is the failure the two tests above exist to be immune to, and this is what
// keeps them immune to it as the module grows.
func TestNothingOnTheReproductionsSideOfTheComparisonComesFromTheModule(t *testing.T) {
	const seed = "reproduceRecordFromTheExporterOutput"
	root, modulePath := keySourceModuleRoot(t)
	functions, files, _ := keySourceTestSource(t, modulePath)
	class, selfImport := keySourceModuleClass(t, root, modulePath, files)
	if _, isDeclared := functions[seed]; !isDeclared {
		t.Fatalf("%s was not read out of this package's test source, so this gate walked nothing", seed)
	}
	// the class has to leave this DIRECTORY, which is the whole of escape (1): message.WriteKey
	// is on SealRecord's path and is not declared here.
	beyondThisPackage := []string{}
	names := 0
	for _, importPath := range keySourceSortedKeys(class) {
		read := class[importPath]
		names += len(read.funcs) + len(read.values) + len(read.types)
		if importPath != selfImport {
			beyondThisPackage = append(beyondThisPackage, importPath)
		}
	}
	if len(beyondThisPackage) == 0 {
		t.Fatalf("the class is %s and nothing else, so it is this directory again and message.WriteKey is outside it",
			selfImport)
	}
	// a dot import would put a module name in reach unqualified, which is the one thing this
	// matcher's per file resolution cannot see.
	for _, file := range files {
		if 0 < len(file.dotImports) {
			t.Errorf("%s dot imports %v, so a name of the module can be written in it unqualified and this gate resolves by qualifier",
				file.path, file.dotImports)
		}
	}
	// THE SCOPE, walked in both directions.
	backward := keySourceBackwardClosure(seed, functions)
	if len(backward) < 2 {
		t.Fatalf("nothing in this package's test source reaches %s, so the bodies that carry the comparisons are outside this gate",
			seed)
	}
	comparison := keySourceForwardClosure(keySourceSortedKeys(backward), functions)
	// the ONE exclusion, derived by result type: the function that produces the subject.
	producers := []string{}
	for _, name := range keySourceSortedKeys(functions) {
		if keySourceNamesTheFixture(functions[name].decl.Type.Results) {
			producers = append(producers, name)
		}
	}
	if len(producers) != 1 {
		t.Fatalf("%d test function(s) answer a *keySourceSealed and this gate excludes exactly one of them: %v",
			len(producers), producers)
	}
	producer := producers[0]
	if !comparison[producer] {
		t.Fatalf("%s produces the fixture and nothing in the comparison reaches it, so the scope this gate excluded is not the one it walked",
			producer)
	}
	excluded := keySourceForwardClosure([]string{producer}, functions)
	scope := map[string]bool{}
	for name := range comparison {
		if !excluded[name] {
			scope[name] = true
		}
	}
	// the exclusion may be entered ONLY through the producer. An exclusion that had grown to
	// swallow an assertion body would clear it in silence, which is this tree's most expensive
	// failure mode.
	for _, name := range keySourceSortedKeys(scope) {
		for _, mentioned := range keySourceMentionedNames(functions[name].decl) {
			if mentioned == producer || !excluded[mentioned] {
				continue
			}
			t.Errorf("%s is inside this gate and reaches %s, which %s's closure excluded: the exclusion is swallowing scope rather than bounding the subject",
				name, mentioned, producer)
		}
	}
	// the scope holds the ASSERTION BODIES, which is escape (3).
	inScope := []string{}
	spanned := []string{}
	for _, name := range keySourceSortedKeys(scope) {
		if strings.HasPrefix(name, "Test") {
			inScope = append(inScope, name)
		}
		if path := functions[name].file.path; !slices.Contains(spanned, path) {
			spanned = append(spanned, path)
		}
	}
	slices.Sort(spanned)
	if len(inScope) < 2 {
		t.Errorf("the scope holds %v, so the bodies that carry the four comparisons are outside this gate", inScope)
	}
	// and it holds something the forward walk alone cannot reach, which is what says the backward
	// walk ran at all rather than reporting the forward one's answer over again.
	forward := keySourceForwardClosure([]string{seed}, functions)
	backwardOnly := []string{}
	for _, name := range keySourceSortedKeys(scope) {
		if !forward[name] {
			backwardOnly = append(backwardOnly, name)
		}
	}
	if len(backwardOnly) == 0 {
		t.Errorf("every function in the scope is reachable forwards from %s, so the backward walk added nothing and this is the gate that missed the assertion bodies",
			seed)
	}
	// and it leaves this file, which is the reach claim: keyschedule_test.go's RFC 5869 reference
	// is what the reproduction expands through.
	if len(spanned) < 2 {
		t.Errorf("the scope stays inside %v, so nothing outside this file is gated by it", spanned)
	}
	// the four controls, one per shape, each outside the scope and each recognised.
	controls := map[string]string{
		"keySourceControlThatCallsThisPackagesDerivation": "a call to a function this package's production source declares",
		"keySourceControlThatReadsThisPackagesConstant":   "a READ of a package level constant, which a matcher walking call expressions cannot see",
		"keySourceControlThatCallsAnotherModulePackage":   "a call into connect/message, which a class derived from this DIRECTORY does not hold",
		"keySourceControlThatReadsAnotherModulePackage":   "a read of a connect/message constant, which is both escapes at once",
	}
	for _, name := range keySourceSortedKeys(controls) {
		control, isDeclared := functions[name]
		if !isDeclared {
			t.Fatalf("the control %s was not read out of this package's test source", name)
		}
		if comparison[name] {
			t.Fatalf("%s is inside the comparison's closure, so it is no longer a control", name)
		}
		if reaches := keySourceModuleReaches(control, class, selfImport); len(reaches) == 0 {
			t.Errorf("the matcher recognised nothing in %s, which is %s, so its clean reading of the reproduction means nothing",
				name, controls[name])
		}
	}
	// and the finding.
	found := []string{}
	for _, name := range keySourceSortedKeys(scope) {
		for _, reach := range keySourceModuleReaches(functions[name], class, selfImport) {
			found = append(found, reach.String())
		}
	}
	if 0 < len(found) {
		t.Errorf("the reproduction's side of the comparison reaches the module under test, so an octet it compares was produced by the code it is evidence about: %v",
			found)
	}
	t.Logf("%d names across %d module package(s) %v banned; the scope is %d function(s) across %v, %d of them reached only backwards from %s; %s and its %d function closure are the subject",
		names, len(class), keySourceSortedKeys(class), len(scope), spanned, len(backwardOnly), seed,
		producer, len(excluded))
}

// keySourceTypeReachesTheModule answers the first module name a type expression can reach,
// following this package's own TEST structs transitively.
//
// The transitivity is the point: *testSession is a test type and looks harmless, and it holds a
// *GroupSession, which is the record layer itself. A check that stopped at the first hop would
// certify exactly the field this boundary exists to keep out.
func keySourceTypeReachesTheModule(node ast.Expr, file *keySourceTestFile,
	class map[string]*keySourceModulePackage, selfImport string,
	structs map[string]*keySourceTestStruct, seen map[string]bool) string {

	reached := ""
	ast.Inspect(node, func(inner ast.Node) bool {
		if reached != "" {
			return false
		}
		switch typed := inner.(type) {
		case *ast.SelectorExpr:
			if path, isModulePackage := file.moduleImports[keySourceExprRoot(typed.X)]; isModulePackage {
				reached = keySourceExprRoot(typed.X) + "." + typed.Sel.Name + " (" + path + ")"
				return false
			}
		case *ast.Ident:
			own := class[selfImport]
			if own.types[typed.Name] {
				reached = typed.Name + " (" + selfImport + ")"
				return false
			}
			structure, isTestStruct := structs[typed.Name]
			if !isTestStruct || seen[typed.Name] {
				return true
			}
			seen[typed.Name] = true
			// and it resolves through the file the deeper struct was written in, not this one.
			for _, field := range structure.decl.Fields.List {
				if deeper := keySourceTypeReachesTheModule(field.Type, structure.file, class,
					selfImport, structs, seen); deeper != "" {
					reached = typed.Name + " -> " + deeper
					return false
				}
			}
		}
		return true
	})
	return reached
}

// The boundary the excluded function hands across carries no door back into the record layer.
//
// keySourceSealRecords is the one function the gate above excludes from its scope, and the whole
// reason that exclusion is safe is that what it returns is octets and one subject. A *testSession
// field here -- which is what this type carried until the gate was widened -- would put a live
// GroupSession on the reproduction's side of the boundary, and everything the gate refuses to let
// the assertion bodies call would be two selectors away.
func TestTheFixtureCanHandTheReproductionNothingItCouldSealWith(t *testing.T) {
	root, modulePath := keySourceModuleRoot(t)
	_, files, structs := keySourceTestSource(t, modulePath)
	class, selfImport := keySourceModuleClass(t, root, modulePath, files)
	boundary, isDeclared := structs["keySourceSealed"]
	if !isDeclared {
		t.Fatal("keySourceSealed is not a struct of this package's test source, so this check read nothing")
	}
	carrying := []string{}
	fields := 0
	for _, field := range boundary.decl.Fields.List {
		for _, name := range field.Names {
			fields += 1
			if reached := keySourceTypeReachesTheModule(field.Type, boundary.file, class, selfImport,
				structs, map[string]bool{"keySourceSealed": true}); reached != "" {
				carrying = append(carrying, name.Name+" reaches "+reached)
			}
		}
	}
	if fields == 0 {
		t.Fatal("keySourceSealed has no fields, so this check certified an empty boundary")
	}
	slices.Sort(carrying)
	// WHAT MAY CROSS IS JUDGED BY THE TYPE REACHED, NOT BY THE FIELD'S NAME. The name is an
	// instance and the type is the property: the one thing the fixture may hand the
	// reproduction's side is the SUBJECT, and the subject is a record. It is transcribed here
	// for the reason every label in this file is -- a check that read the answer off the
	// boundary it is judging would agree with whatever the boundary carried -- and it must be
	// reached DIRECTLY, since a record arriving through a test struct means that struct is on
	// the boundary too and whatever else it holds came with it.
	const subject = "message.Record"
	crossing := 0
	for _, held := range carrying {
		if strings.Contains(held, subject+" (") && !strings.Contains(held, " -> ") {
			crossing += 1
			continue
		}
		t.Errorf("keySourceSealed's %s, and %s reached directly is the only module type the fixture may hand across; everything else it hands the reproduction's side must be octets",
			held, subject)
	}
	if crossing == 0 {
		t.Errorf("no field of keySourceSealed reaches %s, so the fixture hands the comparison no subject at all and this check read nothing",
			subject)
	}
	t.Logf("%d fields on the boundary, %d of them reaching the module: %v", fields, len(carrying), carrying)
}

// TestTheReproductionRecomputesHeadCommitFromItsOwnLadder is ledger item 206's owed clause, and it
// is the one thing v2 adds that the four comparisons above cannot see.
//
// WHAT v2 ADDED, and why it is exactly one thing. MASTER section 8.4.2's term (4) is
// head_commit = HMAC-SHA-256(HKDF-Expand(record_key[i], "rec/v1/head-bind", 32), head_plain) -- a
// FIFTH expansion off the same rung that already produces key_head, nonce_head, key_body and
// nonce_body, and the only new keyed octet-producer on the record path since this file was
// written. It travels inside the aad_mls digest, which travels inside the FRAME, and the frame is
// an injected opaque blob here -- so a reproduction that accepted it as it stands would be
// accepting the one key it is supposed to be reproducing.
//
// SO IT IS RECOMPUTED. The reproduction derives its own storage root, its own class key and its own
// rung from the exporter output through RFC 5869 written out, expands head_bind_key under the
// transcribed label, macs the head plaintext, and rebuilds the whole 160 octet preimage: the 20
// octet label, AAD_body's own 104 octets, four big endian octets of generation and the 32 octet
// commitment. What it is compared against is the authenticated_data the SEALER actually put on the
// wire, read off the frame by the fixture.
//
// THAT COMPARISON IS THE STATEMENT. If head_bind_key came from anywhere but record_key[i] -- a
// constant, a second exporter label, an entropy draw, a leftover rung -- the digest the sealer
// signed would not be the digest this rebuilds, and this goes red. The negative control beside it
// is the same one the rest of this file uses: flipping a bit of the exporter output must move it.
func TestTheReproductionRecomputesHeadCommitFromItsOwnLadder(t *testing.T) {
	sealed := keySourceSealRecords(t, "head-commit-from-the-ladder")
	for i := range sealed.records {
		got := sealed.reproduce(t, i, sealed.mlsSecret)
		if len(sealed.frameAads[i]) != 32 {
			t.Fatalf("record %d's frame carries %d octets of authenticated_data and aad_mls is 32",
				i, len(sealed.frameAads[i]))
		}
		if !bytes.Equal(got.innerAad[:], sealed.frameAads[i]) {
			t.Errorf("record %d: the reproduction rebuilt aad_mls as %x and the frame the sealer emitted carries %x; head_commit is the only term of that preimage this file derives a KEY for, so a disagreement is a key that did not come from the exporter",
				i, got.innerAad, sealed.frameAads[i])
		}
	}

	// THE NEGATIVE CONTROL, which is what says the comparison is capable of failing: a single bit
	// of the exporter output moves the rung, so it moves head_bind_key, so it moves head_commit,
	// so it moves the digest. Every bit, for the reason the sweep next door gives.
	base := sealed.reproduce(t, 0, sealed.mlsSecret)
	moved := 0
	for bit := 0; bit < len(sealed.mlsSecret)*8; bit += 1 {
		flipped := append([]byte(nil), sealed.mlsSecret...)
		flipped[bit/8] ^= 1 << (bit % 8)
		got := sealed.reproduce(t, 0, flipped)
		if got.headBind == base.headBind {
			t.Errorf("bit %d of the exporter output does not reach head_commit", bit)
			continue
		}
		if got.innerAad == base.innerAad {
			t.Errorf("bit %d of the exporter output does not reach aad_mls", bit)
			continue
		}
		moved += 1
	}
	if moved != len(sealed.mlsSecret)*8 {
		t.Errorf("%d of %d bits of the exporter output move head_commit", moved, len(sealed.mlsSecret)*8)
	}
	t.Logf("aad_mls rebuilt from the exporter output for %d records, and all %d bits of it move head_commit",
		len(sealed.records), moved)
}
