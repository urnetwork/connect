// The server attachment: the one structured field of a record the server is allowed to
// read, and the one encoding both ends of that conversation share.
//
// Spec A section 5.11 is normative and spec B section 5.4 restates it character for
// character. The two agree, and the block below is theirs:
//
//	server_attachment := u16(kind) ‖ LP(body)
//
//	  kind 0x0000  NONE            body is zero length, and no conforming encoder writes it
//	  kind 0x0001  EpochAttachment carried by, and only by, a record with is_commit = 1
//	  kind 0x0002  RecoveryTag     carried by RECOVERY_PUB records and by recovery wraps
//	  kind 0x0003  WrapTag         carried by per device epoch wraps and by the snapshot
//	  kind 0x0004  EpochComplete   carried by the wrap set complete marker record
//
//	EpochAttachment := u64(epoch) ‖ u16(alg_id) ‖ LP(write_key) ‖ LP(read_key)
//	                 ‖ u32(media_ttl_seconds) ‖ u32(durable_ttl_seconds)
//	                 ‖ LP(group_context_hash) ‖ u32(expected_wrap_count)
//	RecoveryTag     := LP(recovery_handle) ‖ LP(recovery_verify_pub) ‖ u16(alg_id)
//	WrapTag         := LP(wrap_target_handle) ‖ u64(epoch)
//	EpochComplete   := u64(epoch) ‖ u32(wrap_count)
//
// A SIXTH KIND, which section 5.11 now carries too — ruling 27 of 2026-09-22 amended it,
// and THE SIXTH KIND below says what that amendment is for:
//
//	  kind 0x0005  EpochDigest     what 0x0001 becomes: the six PUBLIC fields of an
//	                               EpochAttachment, and the DIGEST of its two keys in
//	                               place of the keys themselves. Carried by, and only by,
//	                               a record with is_commit = 1, exactly as 0x0001 is.
//
//	EpochDigest     := u64(epoch) ‖ u16(alg_id) ‖ u32(media_ttl_seconds)
//	                 ‖ u32(durable_ttl_seconds) ‖ LP(group_context_hash)
//	                 ‖ u32(expected_wrap_count) ‖ LP(H(epoch_keys))
//
//	epoch_keys      := "URmessage/v1/epochkeys" ‖ LP(group_id) ‖ u64(opens_epoch)
//	                 ‖ LP(write_key) ‖ LP(read_key)
//
// LP(group_id) IS WHERE IT IS BECAUSE THAT IS WHERE EVERY SIBLING PREIMAGE IN THIS SYSTEM
// PUTS IT: aad_body and aad_head carry LP(group_id) ahead of every number they commit to,
// write_auth carries it directly after the connection's LP(server_nonce), and section
// 4.3.4's attestation carries it directly after LP(server_id) — the scope first, the
// position inside the scope second. It is LP framed rather than raw for codec.go's rule,
// the same rule that frames the two keys.
//
// codec.go states the rule that generated a layout and it holds here too: a field whose
// width is fixed by its go type encodes raw at that width, and a field whose length varies
// encodes as LP(x). That rule is what decides the go types below rather than the other way
// round. Section 5.11 writes LP on write_key, read_key, group_context_hash,
// recovery_handle, recovery_verify_pub and wrap_target_handle even though it gives each
// one an exact width in the same line, and aad_test.go's commit vector already pins those
// four octet prefixes on the wire — so each of the six is a slice here and not an array,
// and its width is checked rather than typed. An array would have made the rule and the
// spec disagree about the bytes, and the bytes are not the negotiable half.
//
// The same choice is what makes spec B section 5.1 check 3 answerable at all. That check
// is normative and it is the server's whole static defence — "server_attachment parses via
// message.ParseServerAttachment and is well formed for its record kind" — and every clause
// of it is phrased as a question about a value that could have been otherwise: write_key
// exactly 32 bytes, a 32 byte Ed25519 pub on RecoveryTag, a 16 byte target on WrapTag. A
// [32]byte field turns those into questions no caller can ask and no attacker can fail.
//
// Three decisions in here cannot be found by reading the code, and each is a place two
// implementations would otherwise diverge silently.
//
// The first is the absent attachment. An ordinary record carries a zero length
// server_attachment — the field is empty — and NOT a kind 0x0000 with an empty body. Both
// specs say so in the same words and section 5.11's test obligation says why in one line:
// a zero length attachment and an AttachmentNone attachment must encode identically "so
// H(server_attachment) cannot differ between client and server for an ordinary record".
// So EncodeServerAttachment answers no bytes at all for AttachmentNone and for a nil
// attachment alike, aad.go hashes whatever those bytes are with no carve out, and the
// ordinary record's LP(H(server_attachment)) is LP(SHA-256("")) on both sides. The
// consequence for the parser is the part neither spec writes down: the six octet encoding
// 0x0000 followed by an empty LP body is REFUSED here rather than parsed as
// AttachmentNone. It has to be. If it parsed, one attachment would have two encodings
// whose hashes differ, the write_auth mac and both aeads are over exactly one of them, and
// a record built by a client that emitted the long form is a record the server hashes
// differently and rejects as a bad mac — the intermittent, undiagnosable failure spec B
// section 12.1 A-1 exists to prevent. Refusing it is also the only reading under which
// parsing either fails or re-encodes to the identical bytes, which is the property the
// fuzz target asserts.
//
// The second is the unknown kind. It is a decode error and never a silently ignored
// attachment. Check 3 is what stands between a record and the database, and an attachment
// the server cannot parse is one it cannot check: a record carrying kind 0x0006 that
// parsed to "nothing worth looking at" would take the epoch key install path, the recovery
// index and the wrap index with it, all of them unexamined. The same rule applies on the
// encode side, so a caller cannot build one either. (This paragraph named 0x0005 until
// ruling 27 defined that code; the point is about a code nothing defines, so it now names
// the first one that still is not defined.)
//
// The third is durable_ttl_seconds, and it is the check most likely to be added by
// somebody being careful. It has TWO wire sentinels and both are legal here: 0 means the
// group set nothing and the server applies its own advertised text default, and 0xFFFFFFFF
// means the group asked for indefinite retention, which a server with a cap clamps DOWN to
// that cap. Spec B section 7.3 case 3 forbids refusing either, in all cases, and section
// 5.1 check 3 says so again in the check that calls this function. They are resolved at
// spec B section 6.1 step (6), which is the server's arithmetic over its own advertised
// policy and nothing this layer can compute. So the range check on both retention fields
// is the u32 they are typed as, and there is deliberately no comparison against either
// sentinel anywhere in this file. A refusal here would refuse a commit, and a refused
// commit is a group that cannot rekey.
//
// One clause of check 3 is not here and cannot be. "EpochAttachment iff is_commit" is a
// question about the record's header and the attachment together, and this function is
// handed the attachment alone — the server holds both, has already parsed the header
// through ParseRecord, and asks that one itself. Everything check 3 says it will rely on
// about the attachment's own contents is answered here so the server never re-derives it,
// which is spec B section 12.1 A-2: the server "parses them with
// message.ParseServerAttachment and never reimplements them".
//
// ── THE SIXTH KIND ──────────────────────────────────────────────────────────────────
//
// Kind 0x0005 is here because spec B section 5.4's RULED EpochAttachment block was
// RE-OPENED, narrowly, by ruling 27 of 2026-09-22, and a re-opened ruling is worth the
// sentence that says which two statements could not both be true. Section 5.4 puts
// read_key[n+1] and write_key[n+1] IN THE CLEAR inside a structure the server serves back
// verbatim; section 5.3 and MASTER section 9.2 promise that a member removed at epoch n
// keeps access "until epoch n's read key ages out, and no longer". The commit that removes
// the member is sealed AT epoch n, is fetchable under read_key[n], and carries the keys of
// n+1 — so the removed member ladders every future epoch forever, and the two sentences
// are not a preference between readings but a contradiction. Measured, twice, against the
// server: a fetch under a learned read key answered REASON_OK and a forged write under a
// learned write key answered REASON_OK. A specification that contradicts itself is
// amended.
//
// What the amendment does here, and it is the whole of it: the two keys leave the served
// structure and are replaced by ONE 32 octet digest over both of them. The keys ride
// beside the record instead, as request fields, which is a later step and not this file's.
// Nothing else moves — RecordHeader, the write_auth preimage, message_id and
// format_version are all untouched — because THE BINDING IS ALREADY THERE AND IS FREE: the
// attachment's octets are hashed into AAD_head and into the write_auth preimage, so the
// mac covers the attachment, the attachment covers the digest, and the digest covers the
// keys. A server recomputes H(epoch_keys) over the fields it was handed and compares it
// against a value the mac already authenticated. No new preimage term, no new mac call
// site, no format_version bump, no flag day.
//
// THE TWO DOORS. EncodeServerAttachment and ParseServerAttachment are spec B section 5.1
// check 3's door and they serve every kind section 5.11 defines, which since ruling 27 and
// this commit is all six; EncodeEpochDigestAttachment and ParseEpochDigestAttachment are the
// sixth kind's own door and serve that one kind and no other. The codec, the body table and
// checkServerAttachment are ONE set of code behind both, so the two doors cannot come to
// disagree about what an attachment is — what differs is only which kinds each one serves,
// and that is serverAttachmentKindServed against epochDigestKindServed.
//
// THE PROPERTY THAT SPLIT BOUGHT, and it is a property of a BUILD rather than of this text,
// so read the tense. A record carrying a kind 0x0005 attachment encodes, ParseRecords back
// with is_commit set and the attachment slot byte intact, while a door that does not serve
// that kind refuses the same octets BY NAME with the kind in the message. On every build
// made before this commit, section 5.1 check 3's door was such a door, and that is what let
// the sixth kind ship ahead of the fields that carry its keys: a STALE SERVER refused the
// commit loudly at check 3 instead of installing an epoch whose keys it was never handed,
// and a STALE RECEIVER followed the commit correctly, because no receive path in connect or
// sdk reads a single field of an epoch attachment — it hashes the octets and nothing more.
// THIS BUILD IS NOT THAT BUILD. Section 5.1 check 3's door serves 0x0005 here, which is
// spec B section 5.4's acceptance window step 1, dated 2026-09-22: from that date a server
// accepts a commit carrying EITHER kind. The stale half of the window is held by the
// binaries that predate this commit and cannot be held by this one; what this package still
// holds is the MECHANISM — a door refuses a kind it does not serve by name, rather than
// parsing it into something — and it holds it over the epoch digest door, which serves one
// kind and refuses the other five. attachment_test.go's
// TestARecordCarriesAKindADoorRefusesByName is that property, written over both doors.
//
// ── WHAT serverAttachmentKindServed IS AND IS NOT: A LIBRARY VERSION GATE ────────────
//
// This paragraph replaces one that said "the rollout is then server-serves-both, then
// clients-emit-0x0005, then server-stops-serving-0x0001, and each step is that one map".
// That sentence is wrong in two independent ways, both measured, so it is corrected here
// rather than softened.
//
// FIRST, THE MAP IS NOT PER ROLE. serverAttachmentKindServed governs BOTH halves of section
// 5.1 check 3's door — ParseServerAttachment, which is the server's check 3, the server's
// serve path AND the client's own submit projection, and EncodeServerAttachment, which is
// the client sealer's only encoder by way of connect/messagegroup. One flag, both roles,
// one build: widening it makes a client emit what it makes a server accept, in the same
// library version. TestTheEncoderAndTheParserAdmitTheSameAttachments actively ENFORCES that
// the two sets are identical, and that is the property being kept rather than a coincidence
// to work around. So "servers first, then clients" is operator discipline about which
// BINARIES are deployed when — real, ordinary, and the thing that belongs in a runbook —
// and it is not a property this map can express on its own. Calling it a rollout step made
// it sound as though the library could hold the two roles apart. It cannot, and a reader
// who believed it would ship the client half early.
//
// SECOND, THE SERVER HALF IS NOT ONE LINE, AND IT LANDED FIRST. Widening this map alone
// would have yielded a server that passes the kind at check 3 and then refuses the same
// commit further in — or, worse, accepts a commit and installs no epoch keys at all. The
// message server's F3′ half was named here, by name, when none of it existed: its "an
// EpochAttachment iff is_commit" clause, in THREE copies (the api submit pass and both
// store implementations), which ADMITTED a kind 0x0005 attachment on a NON commit record
// because 0x0005 is not AttachmentEpoch and false != false passes; its founding commit
// check, which required the founding attachment to be kind 0x0001 outright; its
// wellFormedEpochAttachment, which asked for AttachmentEpoch and for two 32 octet keys on
// the body; that repository's own attachment kind enum in its store contract; and its epoch
// key INSTALL path, which is where the keys have to arrive from somewhere else now. Every
// one of those is written, and this map moves BEHIND them rather than in front: the message
// server repository holds the disjunction at section 5.1 check 3, parses 0x0005 at
// ParseEpochDigestAttachment, recomputes the digest through CheckEpochKeysDigest over the
// request's keys, installs them through the vault KEK, and carries migration 012 for the
// column. The order was the safe one — a server that accepts more than any client emits is
// a server with nothing to accept.
//
// WHAT WAS THE INTERLOCK, AND WHAT REPLACED IT. This package refused kind 0x0005 at check
// 3's door until this commit, by name, and that refusal was the interlock: a server built
// from a library that had not been widened could not be talked into installing an epoch
// whose keys it was never handed, whatever a client sent it. The interlock is gone from
// THIS build because the thing it guarded against is gone: the keys now arrive on the
// request (ruling 33) and CheckEpochKeysDigest binds them to the attachment the mac covers.
// An old binary keeps the old refusal — that is what makes the window a rollout — and the
// mechanism that made the refusal legible, a door naming the kind it will not serve rather
// than parsing it into something, is still asserted here, over the epoch digest door.
//
// 0x0001 IS FROZEN AND STAYS READABLE. Nothing above changes one octet of it, and the
// vectors that pin it are the ones that were there before this kind existed.
package message

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// The kind discriminator, u16 on the wire.
type ServerAttachmentKind uint16

// The six kinds spec A section 5.11 defines: the five it was published with, and the sixth
// ruling 27 of 2026-09-22 amended it to carry. The codes
// are the spec's; nothing here may renumber them, because they reach the write_auth mac and
// both aeads by way of H(server_attachment) and a renumbering is a record every other
// implementation refuses.
const (
	AttachmentNone        ServerAttachmentKind = 0x0000
	AttachmentEpoch       ServerAttachmentKind = 0x0001
	AttachmentRecovery    ServerAttachmentKind = 0x0002
	AttachmentWrap        ServerAttachmentKind = 0x0003
	AttachmentComplete    ServerAttachmentKind = 0x0004
	AttachmentEpochDigest ServerAttachmentKind = 0x0005
)

// The kinds this package knows, as a lookup rather than a chain of comparisons, for the
// reason record.go's classPrunable is one: a kind added later has to be given an answer
// here instead of inheriting one from whichever side of a bound it happens to fall.
var serverAttachmentKindKnown = map[ServerAttachmentKind]bool{
	AttachmentNone:        true,
	AttachmentEpoch:       true,
	AttachmentRecovery:    true,
	AttachmentWrap:        true,
	AttachmentComplete:    true,
	AttachmentEpochDigest: true,
}

// The kinds spec B section 5.1 check 3's door serves. As of this commit that is every kind
// section 5.11 defines, all six of them.
//
// It is a map of its own rather than a subtraction from serverAttachmentKindKnown, and it
// stays one now that the two sets agree: a kind added later is given an answer HERE instead
// of inheriting one, and "= the known map" would be the inheritance written down. The two
// agreeing is a fact about today, not a definition.
//
// ── WHY 0x0005 WAS HELD OUT OF THIS MAP, AND WHAT DISCHARGED THAT REASON ─────────────
//
// This paragraph used to read: "kind 0x0005 is deliberately absent — a server that accepted
// it would install an epoch whose two keys it was never handed, because the fields those
// keys ride in do not exist yet." That was step 1's author's reason, it was the right
// refusal on the day it was written, and it is not being waived or outweighed here. THE
// FACT IT RESTED ON STOPPED BEING TRUE. Ruling 33 of 2026-09-22 put the two keys on the
// REQUEST rather than in the served structure: connect/protocol's SubmitRequest.epoch_keys,
// positionally aligned with `records`, and CreateGroupRequest.epoch_keys, singular — both
// EpochKeyDelivery, both carrying write_key[n+1] and read_key[n+1], and neither of them a
// field of Record or of anything the server serves back. A server accepting a kind 0x0005
// commit today IS handed the two keys, beside the record, and CheckEpochKeysDigest below is
// the comparison that says they are the pair this attachment's digest is over — a digest
// the write_auth mac already covers by way of LP(H(server_attachment)). So the sentence
// "installs an epoch whose two keys it was never handed" no longer describes anything that
// can happen, and the reason it justified is discharged.
//
// WHAT WIDENING IT DOES, which is unchanged and is why it was never one line. Adding
// AttachmentEpochDigest here makes ParseServerAttachment serve six AND makes
// EncodeServerAttachment emit six, in ONE build, because both doors ask this one map — so
// it cannot express "the server serves both while clients still emit 0x0001", which is a
// statement about deployed binaries and belongs in a runbook. Spec B section 5.4's
// acceptance window says that in dates: from 2026-09-22 a server accepts either kind, from
// 2026-10-06 a conforming client emits only 0x0005, and from 2026-11-03 — or the day the
// Remove arm first ships, whichever is EARLIER — the server refuses 0x0001 on a new commit
// and the window closes. Steps 2 and 3 are the operator's; this map is step 1's library
// half, and the message server's half of step 1 landed before it.
var serverAttachmentKindServed = map[ServerAttachmentKind]bool{
	AttachmentNone:        true,
	AttachmentEpoch:       true,
	AttachmentRecovery:    true,
	AttachmentWrap:        true,
	AttachmentComplete:    true,
	AttachmentEpochDigest: true,
}

// The one kind the epoch digest door serves. Written as the same shape as the map above so
// that "which kinds does this door serve" is one question with one answer per door.
//
// SINCE THIS COMMIT IT IS THE NARROW ONE. The map above serves every kind this package
// defines, so the complement it once had is empty and this one carries the whole of it:
// five defined kinds that a door refuses by name rather than parsing into something. That
// is not a leftover — it is what keeps the digest door a door. Accepting kind 0x0001 here
// would be the epoch key install path reached through the function that exists to take the
// keys out of it, which is what ParseEpochDigestAttachment's own comment says at length.
var epochDigestKindServed = map[ServerAttachmentKind]bool{
	AttachmentEpochDigest: true,
}

// What each door is called in its own refusal, so a reader of a log knows which of the two
// answered rather than only that something did.
const (
	serverAttachmentDoorName = "spec B section 5.1 check 3's door"
	epochDigestDoorName      = "the epoch digest door"
)

// The exact widths spec A section 5.11 gives the six length prefixed fields. Written as
// named constants rather than as the array widths of go types, because the fields are
// slices — see the file comment — so the width is a value this file checks and not a
// property the compiler enforces.
const (
	epochWriteKeyBytes         = 32
	epochReadKeyBytes          = 32
	epochGroupContextHashBytes = 32
	recoveryHandleBytes        = 16
	recoveryVerifyPubBytes     = 32
	wrapTargetHandleBytes      = 16
	// H is SHA-256 per master section 0's notation line, so the digest of epoch_keys is
	// this wide and no other width is a digest of anything. It is a named constant beside
	// the six above because it is the same kind of fact: a length prefixed field of an
	// attachment whose width this file checks rather than types.
	epochKeysDigestBytes = 32
)

// The domain separation label of the epoch keys digest, raw ascii and never length
// prefixed, exactly as the four labels in aad.go and writeauth.go are.
//
// It is its own constant and is never computed from another label or shares a prefix
// constant with one, for the reason those files give at length: the separation between two
// preimages rests on the bytes of their labels differing, a shared constant is one edit
// away from making two of them agree, and a preimage that agrees with another protocol's is
// a preimage that can be replayed into it. attachment_test.go holds every label this package
// declares to that as a property over the source rather than over a list.
const epochKeysLabel = "URmessage/v1/epochkeys"

// The algorithm identifier each kind that carries one names, from master section 7.1's
// registry: 0x0031 is HKDF-SHA-256, which is what derived write_key and read_key, and
// 0x0001 is Ed25519, which is what recovery_verify_pub verifies under. A table keyed by
// kind rather than a constant per body, so "known alg_id" is one question asked in one
// place and a kind that grows an alg_id later is given its answer here.
//
// The identifiers are pinned per kind and not to the registry as a whole. An
// EpochAttachment announcing 0x0001 would be claiming its two 32 octet keys came out of a
// signature algorithm, which is not a v1 record with an unusual field but a record built
// by something that does not know what the field is for.
// EpochDigest names the same 0x0031 EpochAttachment does, and it names it for the same
// reason: the field says which algorithm derived the two keys the attachment is about, and
// hashing them rather than carrying them does not change what derived them.
var attachmentAlgIds = map[ServerAttachmentKind]uint16{
	AttachmentEpoch:       0x0031,
	AttachmentRecovery:    0x0001,
	AttachmentEpochDigest: 0x0031,
}

// The epoch attachment: everything the server needs in order to verify the epoch this
// commit opens, delivered inside the commit that opens it (spec B section 5.3).
type EpochAttachment struct {
	// The epoch this attachment OPENS, which spec B section 5.1 check 3 requires to be
	// current_epoch + 1. That comparison is the server's — it is the party that holds
	// current_epoch — and is deliberately not made here.
	Epoch uint64
	AlgId uint16
	// write_key[epoch], exactly 32 octets. The server holds it and can therefore forge
	// write_auth, which spec B section 5.3 states as an accepted consequence rather than a
	// defect: it is the party enforcing write_auth in the first place.
	WriteKey []byte
	// read_key[epoch], exactly 32 octets, and a different value in every epoch. Spec B
	// section 5.1 check 3 says in as many words that it is "never compared against a
	// previously installed one"; the server installs it against this epoch and retains it
	// for ninety days.
	ReadKey []byte
	// Both retention fields are the whole u32 range, both of durable's sentinels included.
	// The file comment says why there is no check here and why adding one refuses commits.
	MediaTtlSeconds   uint32
	DurableTtlSeconds uint32
	GroupContextHash  []byte
	// Device wraps plus recovery wraps plus the one snapshot, for the epoch this opens.
	// Greater than zero, always: the epoch it opens has at least the snapshot in it, and a
	// zero would name a wrap set the EpochComplete marker can never match, leaving the
	// group readable but not writable with nothing able to close it.
	ExpectedWrapCount uint32
}

// The epoch digest attachment: everything the epoch attachment above says about the epoch
// this commit opens, with the two keys replaced by one digest over both of them.
//
// Six of its seven fields are the six PUBLIC fields of EpochAttachment, in EpochAttachment's
// own order and with their own meanings, and they are declared here rather than by embedding
// that type because embedding would put write_key and read_key one selector away from a
// structure whose whole purpose is not to have them.
type EpochDigestAttachment struct {
	// The epoch this attachment OPENS, which spec B section 5.1 check 3 requires to be
	// current_epoch + 1. That comparison is the server's — it is the party that holds
	// current_epoch — and is deliberately not made here.
	Epoch uint64
	AlgId uint16
	// Both retention fields are the whole u32 range, both of durable's sentinels included,
	// exactly as they are on EpochAttachment. The file comment says why there is no check
	// here and why adding one refuses commits.
	MediaTtlSeconds   uint32
	DurableTtlSeconds uint32
	GroupContextHash  []byte
	// Device wraps plus recovery wraps plus the one snapshot, for the epoch this opens.
	// Greater than zero, always, for EpochAttachment's reason unchanged.
	ExpectedWrapCount uint32
	// H(epoch_keys), exactly 32 octets: SHA-256 over
	//
	//	"URmessage/v1/epochkeys" ‖ LP(group_id) ‖ u64(opens_epoch)
	//	  ‖ LP(write_key) ‖ LP(read_key)
	//
	// and EpochKeysDigest is the one function in this package that computes it. The two
	// keys are LP framed inside the preimage, so no choice of one key's octets can move a
	// boundary into the other's; opens_epoch is inside it so that one epoch's pair cannot
	// be replayed as another's; and group_id is inside it — ruling 34 — so that the pair
	// is an epoch OF A GROUP and not a number. Without the group term the preimage commits
	// to an epoch index and two keys, and epoch 1 of every group in the world is the same
	// index: the same digest verifies the same two keys under any group_id a request cares
	// to name, and this structure alone can no longer say which group it is about.
	//
	// The group is NOT a field of this type, and that is the same ruling read the other
	// way. This package already has one wire-visible group_id per record, in the header,
	// where AAD_head and the write_auth preimage both commit to it; a second copy here
	// would be a second group this attachment could disagree with its own record about,
	// which is a new instance of exactly the defect NewEpochDigestAttachment exists to
	// close, in a place this package cannot see both halves of. So the group is a
	// parameter of the three functions below and a field of nothing.
	//
	// The server does not learn the keys from this. It is handed them beside the record and
	// recomputes this value, which the write_auth mac already covers by way of
	// H(server_attachment) — so the digest is a binding and never a delivery.
	EpochKeysDigest []byte
}

// NewEpochDigestAttachment builds the sixth kind's body from its six public fields and the
// two keys the epoch opens with, and is the way a committer should build one.
//
// IT EXISTS BECAUSE THE MISMATCH IT PREVENTS IS OTHERWISE REPRESENTABLE AND SILENT. An
// EpochDigestAttachment has an epoch in two places — its own Epoch field, and the
// opens_epoch term inside the preimage its digest is over — and nothing in the encoding can
// relate the two: the codec is never handed the keys, so it cannot recompute the digest, and
// a body whose Epoch is 43 and whose digest is H over opens_epoch 42 encodes, parses back
// and re-encodes byte for byte. It is not exploitable on its own, because the check it
// eventually fails is a comparison that fails closed — but a server that reached for the
// RECORD HEADER's epoch instead of the attachment's would compute the matching digest and
// accept it, and there are three epoch values live at that call site: the header's, the
// attachment's, and the server's own current_epoch + 1. A wrong choice among them type
// checks. So the epoch is taken ONCE here, from the attachment being built, and the caller
// is given no second one to disagree with it.
//
// It takes the body by value, its own declared type rather than a parallel argument list,
// for two reasons. A field added to EpochDigestAttachment later is carried through this
// constructor without a signature change and without arriving silently zero. And there is
// no run of same typed positional arguments for a caller to transpose — media_ttl_seconds
// and durable_ttl_seconds are both u32 and mean opposite things to a retention policy.
//
// public.EpochKeysDigest MUST be unset. It is the one field of the six-plus-one that is not
// the caller's to choose, and a caller that filled it in has either computed it at an epoch
// of its own — the whole defect above — or is round-tripping a body that was already built,
// which is a copy and not a construction. Refused rather than overwritten: overwriting it
// would silently discard a value its author believed in.
//
// THE GROUP GETS THE SAME TREATMENT BY A DIFFERENT ROUTE, and the route matters. Ruling 34
// put LP(group_id) in the preimage, and the way to keep a caller from naming a group that
// disagrees with something is to leave it nothing to disagree WITH: the group is not a
// field of EpochDigestAttachment, is not a second parameter of the checker, and has exactly
// one home in a record — the header's own GroupId, which AAD_head and the write_auth
// preimage already commit to. It is typed [32]byte here, that header field's own type, so
// the only value a caller can hand over is a whole 32 octet group id, at the width the
// preimage frames, taken from the one place the record keeps it. It also cannot be
// transposed with either key: [32]byte and []byte do not convert, so the swap that would
// hash a key as the group and the group as a key is a compile error rather than a digest
// nothing else reproduces.
//
// Everything it returns has been through checkEpochDigestAttachment, so a body this answers
// is a body the sixth kind's door will encode.
func NewEpochDigestAttachment(groupId [32]byte, public EpochDigestAttachment, writeKey []byte, readKey []byte) (*EpochDigestAttachment, error) {
	if 0 < len(public.EpochKeysDigest) {
		return nil, fmt.Errorf("%w: epoch_keys_digest arrived already filled, at %d octets, and it is this function's to compute from the epoch beside it",
			ErrEpochKeysDigestPresence, len(public.EpochKeysDigest))
	}
	// the ONE place the epoch is read for the preimage, and it is the field of the body this
	// is building. There is no parameter here that could name a different one. The group is
	// the opposite arrangement to the same end: it is a parameter and a field of nothing, so
	// there is no second group here either.
	digest, err := EpochKeysDigest(groupId, public.Epoch, writeKey, readKey)
	if err != nil {
		return nil, err
	}
	public.EpochKeysDigest = digest
	if err := checkEpochDigestAttachment(&public); err != nil {
		return nil, err
	}
	return &public, nil
}

// CheckEpochKeysDigest answers the one question spec B section 5.1 check 3 gains under
// ruling 27: are these two keys the ones this attachment's digest is over?
//
// It is the server's half of the amendment and it lives here, beside the function that
// computes the digest, for the rule attachment.go's file comment already states of check 3 —
// everything the check asks of an attachment's own contents is answered in this package so
// the server asks rather than re-derives, which is spec B section 12.1 A-2. A server that
// wrote this itself would be choosing, on its own, which of the three epochs in scope at its
// call site goes into the preimage and whether the comparison is constant time. Neither is a
// choice another repository should be making about this construction, so neither is offered:
// the epoch is d.Epoch and the comparison is subtle.ConstantTimeCompare.
//
// THE EPOCH IS THE ATTACHMENT'S OWN AND NOT A PARAMETER. Taking one would reintroduce
// exactly what NewEpochDigestAttachment exists to prevent, one layer further on, and the
// caller with the wrong answer to hand is the same caller: the record header's epoch is the
// epoch the commit is SEALED at, and the attachment's is the epoch it OPENS, which is one
// higher. "epoch == current_epoch + 1" is still the server's own clause over its own state
// and is still not asked here; this function asks only whether the digest matches the keys,
// at the epoch the digest itself claims.
//
// THE GROUP IS A PARAMETER AND THE EPOCH IS NOT, and the asymmetry is the point rather than
// an inconsistency. The epoch has a home in the body, so taking one here would be offering a
// second answer to a question the body already answers. The group has no home in the body
// and must not get one — see EpochKeysDigest — so the only place it can come from is the
// caller, and the caller that reaches here is the server, which holds the group_id the
// request named and has already verified the record against it. It is the group of the
// group, not of the attachment: the [32]byte that came off the wire once.
//
// It answers an error rather than a bool so that a mismatch and a malformed key are
// different sentinels at the call site: the second is a caller that looked a key up and got
// nothing back, and answering "no" to that would report an attacker where there is a bug.
func CheckEpochKeysDigest(groupId [32]byte, d *EpochDigestAttachment, writeKey []byte, readKey []byte) error {
	if d == nil {
		return fmt.Errorf("%w: kind 0x%04x carries no body", ErrServerAttachmentBody, uint16(AttachmentEpochDigest))
	}
	computed, err := EpochKeysDigest(groupId, d.Epoch, writeKey, readKey)
	if err != nil {
		return err
	}
	// ConstantTimeCompare answers 0 for a length mismatch as well, so a truncated or absent
	// digest is a mismatch here and never a short comparison that happened to agree.
	if subtle.ConstantTimeCompare(computed, d.EpochKeysDigest) != 1 {
		return fmt.Errorf("%w: the two keys handed beside this record are not the ones H(epoch_keys) at opens_epoch %d is over",
			ErrEpochKeysDigestMismatch, d.Epoch)
	}
	return nil
}

// EpochKeysDigest is H(epoch_keys): the one value an EpochDigestAttachment carries about
// the two keys it does not carry.
//
//	epoch_keys := "URmessage/v1/epochkeys" ‖ LP(group_id) ‖ u64(opens_epoch)
//	                ‖ LP(write_key) ‖ LP(read_key)
//
// LP(group_id) is ruling 34 and it sits where the file comment argues it sits: ahead of
// every number, because that is where aad_body, aad_head, write_auth and section 4.3.4's
// attestation each put the group they are about.
//
// H is SHA-256, per master section 0's notation line, and the answer is its thirty two
// octets. It is exported because both ends compute it: the committer to fill the field, and
// the server to check the field against the keys it was handed. A second implementation that
// computed a different one would have every commit refused at a comparison rather than
// anywhere legible, which is why the vectors in attachment_test.go pin the preimage as well
// as the digest.
//
// It refuses a key that is not exactly thirty two octets rather than hashing it. A digest
// over a short key is a digest nothing else reproduces, and the one caller that could get
// here with one is a caller that looked a key up and got nothing back — which is the empty
// key writeauth.go's ErrAuthKeyLength exists to keep out of a mac, met one layer further out.
func EpochKeysDigest(groupId [32]byte, opensEpoch uint64, writeKey []byte, readKey []byte) ([]byte, error) {
	preimage, err := epochKeysPreimage(groupId, opensEpoch, writeKey, readKey)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(preimage)
	return digest[:], nil
}

// The bytes the digest above is taken over, in the block at the top of this file.
//
// It is a function of its own rather than four lines inside EpochKeysDigest so that the
// preimage is a value a test can pin, which is what makes "the label is these octets" and
// "opens_epoch is in here" observable at all: a digest alone moves as one opaque number
// whichever term went missing.
//
// The label is raw ascii and is NOT length prefixed, exactly as the four labels in aad.go
// and writeauth.go are, so a reader of the bytes meets twenty two label octets and then the
// first field. What separates this preimage from those four is the bytes of the label alone.
func epochKeysPreimage(groupId [32]byte, opensEpoch uint64, writeKey []byte, readKey []byte) ([]byte, error) {
	if err := checkAttachmentWidth("write_key", writeKey, epochWriteKeyBytes); err != nil {
		return nil, err
	}
	if err := checkAttachmentWidth("read_key", readKey, epochReadKeyBytes); err != nil {
		return nil, err
	}
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(epochKeysLabel))
	// the group before the epoch, which is the order of all four sibling preimages: aad.go's
	// two write LP(group_id) ahead of every number, and writeauth.go's two write it directly
	// after the scope above the group — the connection's nonce, the server's id
	writer.WriteOpaqueLP(groupId[:])
	writer.WriteUint64(opensEpoch)
	writer.WriteOpaqueLP(writeKey)
	writer.WriteOpaqueLP(readKey)
	// the writer is sticky: the first failure latches and every later call is a no op, so
	// this is the one place the build is asked whether it worked.
	return writer.Bytes()
}

// The recovery tag: the handle the server indexes recovery wraps by, and the public half
// the client — never the server — verifies the RECOVERY_PUB body signature under.
type RecoveryTag struct {
	RecoveryHandle []byte
	// Ed25519, exactly 32 octets. Spec B section 5.4 is exact about what authenticating
	// this proves: write_auth is group wide, so it proves a current member submitted the
	// record and not that the member owns the handle. The server keeps the first pub it
	// sees for a handle within one group and refuses a later differing one.
	RecoveryVerifyPub []byte
	AlgId             uint16
}

// The wrap tag: the target a per device wrap or the epoch snapshot is served to, which is
// what lets the server answer a WrapFetch in constant time without being able to invert
// the handle.
type WrapTag struct {
	WrapTargetHandle []byte
	// The epoch whose wrap or snapshot this record carries.
	Epoch uint64
}

// The marker that closes an epoch's fan out. Until it lands the group is readable but not
// writable and the server refuses every non wrap submit at the new epoch.
type EpochComplete struct {
	Epoch uint64
	// Required to equal that epoch's EpochAttachment.expected_wrap_count. The equality is
	// the server's, because it is the party holding the attachment this marker is about,
	// and it is not restated here as a bound of its own.
	WrapCount uint32
}

// One parsed attachment: the kind, and the one body that kind carries.
//
// Four pointers and a tag rather than an interface, because this is the shape spec A
// section 5.11 publishes and spec B section 12.1 restates, and the server switches on the
// tag. The rule that keeps the two halves from disagreeing is that exactly one body is set
// and it is the one the tag names — checkServerAttachment enforces it in both directions,
// so an attachment carrying an EpochAttachment under the WrapTag tag is refused rather
// than encoded as a wrap tag with the epoch attachment quietly dropped.
type ServerAttachment struct {
	Kind        ServerAttachmentKind
	Epoch       *EpochAttachment
	Recovery    *RecoveryTag
	Wrap        *WrapTag
	Complete    *EpochComplete
	EpochDigest *EpochDigestAttachment
}

// The kind the bodies actually set say this is, and how many of them are set.
//
// It is the presence rule computed rather than asserted: a fifth kind added later with a
// fifth pointer that nobody wires in here is an attachment whose bodies say
// AttachmentNone while its tag says otherwise, which checkServerAttachment refuses. The
// alternative — a presence check written out per kind — is the one that lets a new kind
// through unchecked, which is the shape this package refuses to have.
func (self *ServerAttachment) bodyKind() (ServerAttachmentKind, int) {
	kind := AttachmentNone
	set := 0
	if self.Epoch != nil {
		kind, set = AttachmentEpoch, set+1
	}
	if self.Recovery != nil {
		kind, set = AttachmentRecovery, set+1
	}
	if self.Wrap != nil {
		kind, set = AttachmentWrap, set+1
	}
	if self.Complete != nil {
		kind, set = AttachmentComplete, set+1
	}
	if self.EpochDigest != nil {
		kind, set = AttachmentEpochDigest, set+1
	}
	return kind, set
}

// EncodeServerAttachment serialises an attachment into the layout at the top of this file.
//
// A nil attachment and an AttachmentNone attachment both answer no bytes at all. That is
// the absent/empty equivalence spec A section 5.11's test obligation names, and it is why
// this function is the one place in the package where nil is an ordinary argument rather
// than the caller bug ErrRecordNil reports: an ordinary record has no attachment, and the
// bytes it contributes to LP(H(server_attachment)) have to be the same bytes a client
// holding an explicit AttachmentNone contributes.
//
// It refuses everything its own parser refuses, through the same checkServerAttachment, so
// there is no attachment this package will write and then fail to read back.
func EncodeServerAttachment(a *ServerAttachment) ([]byte, error) {
	if a == nil {
		return nil, nil
	}
	if err := checkServerAttachment(a); err != nil {
		return nil, err
	}
	if err := checkAttachmentKindServed(a.Kind, serverAttachmentKindServed, serverAttachmentDoorName); err != nil {
		return nil, err
	}
	return encodeAttachmentBytes(a)
}

// EncodeEpochDigestAttachment serialises the sixth kind, and is the committer's whole
// interface to it.
//
// It takes the body rather than a ServerAttachment because a door that serves one kind has
// no discriminator left for a caller to get wrong. (It also used to be the only way to
// encode this kind at all, and the mistake it prevented was building a ServerAttachment,
// setting Kind to the digest kind, and reaching EncodeServerAttachment's refusal. That
// refusal is gone — the served map moved with ruling 33 — so EncodeServerAttachment now
// writes this kind too, and the two produce THE SAME OCTETS because the framing and the
// body table are one set of code behind both. What this door still buys is the typed one:
// the caller with a body and no discriminator to get wrong.)
//
// THE SAME-OCTETS SENTENCE IS A CLAIM, and it is measured rather than argued from the shared
// code: TestAKindTwoDoorsServeHasOneEncodingAtBoth writes every entry of this kind's corpus
// at both doors, compares the octets, and reads each door's octets back at both. It was true
// and unasserted from the day the served map moved, and an encode arm that wrote two of this
// body's fields differently at one door passed this package's, messagegroup's and protocol's
// tests under `go test -run 'Test'`.
//
// Everything it refuses, it refuses through the same checkServerAttachment the other door
// runs, so there is no attachment one door will write and the other will fail to read.
func EncodeEpochDigestAttachment(d *EpochDigestAttachment) ([]byte, error) {
	a := &ServerAttachment{Kind: AttachmentEpochDigest, EpochDigest: d}
	if err := checkServerAttachment(a); err != nil {
		return nil, err
	}
	return encodeAttachmentBytes(a)
}

// The framing both doors write, over an attachment that has already been checked.
//
// It is one function rather than one per door because the layout is one layout: two doors
// with two writers is two encodings of one attachment the day one of them is edited, and
// H(server_attachment) is over exactly one of them.
func encodeAttachmentBytes(a *ServerAttachment) ([]byte, error) {
	if a.Kind == AttachmentNone {
		return nil, nil
	}
	writer := syntax.NewWriter()
	writer.WriteUint16(uint16(a.Kind))
	// LP(body), through the one nesting form that frames a structure inside the record
	// layer's fixed 32 bit prefix. The region is built and then framed by its own length,
	// so the prefix cannot drift from the bytes it counts.
	if err := writer.WriteNestedLP(func(body *syntax.Writer) error {
		writeAttachmentBody(body, a)
		return nil
	}); err != nil {
		return nil, err
	}
	// the writer is sticky: the first failure latches and every later call is a no op, so
	// this is the one place the encode is asked whether it worked.
	return writer.Bytes()
}

// ParseServerAttachment deserialises the layout at the top of this file and validates
// everything spec B section 5.1 check 3 says it will rely on.
//
// Empty input is the absent attachment and answers AttachmentNone with no body, which is
// what every ordinary record carries. The six octet spelling of the same thing is refused;
// the file comment argues it.
//
// The returned attachment has exactly one body set, and it is the one Kind names.
func ParseServerAttachment(b []byte) (*ServerAttachment, error) {
	attachment, err := parseAttachmentBytes(b)
	if err != nil {
		return nil, err
	}
	if err := checkAttachmentKindServed(attachment.Kind, serverAttachmentKindServed, serverAttachmentDoorName); err != nil {
		return nil, err
	}
	return attachment, nil
}

// ParseEpochDigestAttachment deserialises the sixth kind, and is the only way to read one.
//
// It answers the body rather than a ServerAttachment for the reason its encoding sibling
// takes one: a door that serves one kind hands back the one thing it can have parsed, and a
// caller that had to test a discriminator afterwards would be a caller that could forget to.
//
// It refuses every other kind by name, the five section 5.11 defines included, because the
// question this door answers is "is this the digest attachment" and a yes for kind 0x0001
// would be the epoch key install path reached through the function that exists to take the
// keys out of it.
func ParseEpochDigestAttachment(b []byte) (*EpochDigestAttachment, error) {
	attachment, err := parseAttachmentBytes(b)
	if err != nil {
		return nil, err
	}
	if err := checkAttachmentKindServed(attachment.Kind, epochDigestKindServed, epochDigestDoorName); err != nil {
		return nil, err
	}
	return attachment.EpochDigest, nil
}

// The parse both doors run, over every kind this package defines and before either door
// asks whether it serves the one it found.
//
// The split is what makes the refusal above say "a kind this door does not serve" rather
// than "these octets are not an attachment": the octets ARE an attachment, they are checked
// as one, and only then is the kind weighed against the door. A parser that refused on the
// kind alone would be unable to tell a caller which of the two it had met.
func parseAttachmentBytes(b []byte) (*ServerAttachment, error) {
	if len(b) == 0 {
		return &ServerAttachment{Kind: AttachmentNone}, nil
	}
	reader := syntax.NewReader(b)
	// the reader is sticky, so the kind's own failure latches and is reported by the
	// nesting below rather than here: a one octet input reports that it was truncated
	// instead of reporting whatever half a kind happens to be.
	kind, _ := reader.ReadUint16()
	attachment := &ServerAttachment{Kind: ServerAttachmentKind(kind)}
	// ReadNestedLP runs the body's field list against a reader bounded by the declared
	// region and then runs that reader to empty, so a body region longer than the fields
	// inside it is a refusal rather than a second encoding of one attachment.
	if err := reader.ReadNestedLP(func(body *syntax.Reader) error {
		return readAttachmentBody(body, attachment)
	}); err != nil {
		return nil, err
	}
	if err := reader.Done(); err != nil {
		return nil, err
	}
	if err := checkServerAttachment(attachment); err != nil {
		return nil, err
	}
	return attachment, nil
}

// The body of one attachment, in the field order at the top of this file. Total by
// construction: the caller has already been through checkServerAttachment, so the kind is
// known and the body it names is set, and the switch has no case left to fall out of.
func writeAttachmentBody(w *syntax.Writer, a *ServerAttachment) {
	switch a.Kind {
	case AttachmentEpoch:
		w.WriteUint64(a.Epoch.Epoch)
		w.WriteUint16(a.Epoch.AlgId)
		w.WriteOpaqueLP(a.Epoch.WriteKey)
		w.WriteOpaqueLP(a.Epoch.ReadKey)
		w.WriteUint32(a.Epoch.MediaTtlSeconds)
		w.WriteUint32(a.Epoch.DurableTtlSeconds)
		w.WriteOpaqueLP(a.Epoch.GroupContextHash)
		w.WriteUint32(a.Epoch.ExpectedWrapCount)
	case AttachmentRecovery:
		w.WriteOpaqueLP(a.Recovery.RecoveryHandle)
		w.WriteOpaqueLP(a.Recovery.RecoveryVerifyPub)
		w.WriteUint16(a.Recovery.AlgId)
	case AttachmentWrap:
		w.WriteOpaqueLP(a.Wrap.WrapTargetHandle)
		w.WriteUint64(a.Wrap.Epoch)
	case AttachmentComplete:
		w.WriteUint64(a.Complete.Epoch)
		w.WriteUint32(a.Complete.WrapCount)
	case AttachmentEpochDigest:
		w.WriteUint64(a.EpochDigest.Epoch)
		w.WriteUint16(a.EpochDigest.AlgId)
		w.WriteUint32(a.EpochDigest.MediaTtlSeconds)
		w.WriteUint32(a.EpochDigest.DurableTtlSeconds)
		w.WriteOpaqueLP(a.EpochDigest.GroupContextHash)
		w.WriteUint32(a.EpochDigest.ExpectedWrapCount)
		w.WriteOpaqueLP(a.EpochDigest.EpochKeysDigest)
	}
}

// The body of one attachment, read back.
//
// The reads are a straight run because the reader is sticky: the first failure latches,
// every later read is a no op, and the bounded region is asked whether it was well formed
// exactly once, by the Done that ReadNestedLP runs on it. Nothing here is validated as a
// value — that is checkServerAttachment's, once, for both sides of the codec — so a
// truncated body reports that it was truncated rather than reporting whichever field read
// off the end happened to land somewhere illegal.
func readAttachmentBody(r *syntax.Reader, a *ServerAttachment) error {
	switch a.Kind {
	case AttachmentNone:
		return fmt.Errorf("%w: an absent attachment is the empty field and not %d octets under kind 0x%04x",
			ErrServerAttachmentNoneEncoded, r.Remaining(), uint16(AttachmentNone))
	case AttachmentEpoch:
		epoch := &EpochAttachment{}
		epoch.Epoch, _ = r.ReadUint64()
		epoch.AlgId, _ = r.ReadUint16()
		epoch.WriteKey, _ = r.ReadOpaqueLP()
		epoch.ReadKey, _ = r.ReadOpaqueLP()
		epoch.MediaTtlSeconds, _ = r.ReadUint32()
		epoch.DurableTtlSeconds, _ = r.ReadUint32()
		epoch.GroupContextHash, _ = r.ReadOpaqueLP()
		epoch.ExpectedWrapCount, _ = r.ReadUint32()
		a.Epoch = epoch
		return nil
	case AttachmentRecovery:
		recovery := &RecoveryTag{}
		recovery.RecoveryHandle, _ = r.ReadOpaqueLP()
		recovery.RecoveryVerifyPub, _ = r.ReadOpaqueLP()
		recovery.AlgId, _ = r.ReadUint16()
		a.Recovery = recovery
		return nil
	case AttachmentWrap:
		wrap := &WrapTag{}
		wrap.WrapTargetHandle, _ = r.ReadOpaqueLP()
		wrap.Epoch, _ = r.ReadUint64()
		a.Wrap = wrap
		return nil
	case AttachmentComplete:
		complete := &EpochComplete{}
		complete.Epoch, _ = r.ReadUint64()
		complete.WrapCount, _ = r.ReadUint32()
		a.Complete = complete
		return nil
	case AttachmentEpochDigest:
		digest := &EpochDigestAttachment{}
		digest.Epoch, _ = r.ReadUint64()
		digest.AlgId, _ = r.ReadUint16()
		digest.MediaTtlSeconds, _ = r.ReadUint32()
		digest.DurableTtlSeconds, _ = r.ReadUint32()
		digest.GroupContextHash, _ = r.ReadOpaqueLP()
		digest.ExpectedWrapCount, _ = r.ReadUint32()
		digest.EpochKeysDigest, _ = r.ReadOpaqueLP()
		a.EpochDigest = digest
		return nil
	}
	return fmt.Errorf("%w: 0x%04x, and an attachment this layer cannot parse is one the server cannot check",
		ErrServerAttachmentKindUnknown, uint16(a.Kind))
}

// The structural invariants, run by both sides of the codec so that the set of attachments
// this package will write and the set it will read are the same set.
//
// Everything spec B section 5.1 check 3 asks of an attachment's own contents is here and
// nowhere else, so the server asks rather than re-derives. What is deliberately absent is
// as load bearing as what is present: no comparison against either durable_ttl_seconds
// sentinel, no bound on media_ttl_seconds, no epoch arithmetic, and no wrap_count equality
// — each of those is either the server's own policy or a fact about state this layer never
// sees, and a refusal invented here would refuse a commit the spec calls valid.
func checkServerAttachment(a *ServerAttachment) error {
	if !serverAttachmentKindKnown[a.Kind] {
		return fmt.Errorf("%w: 0x%04x, and an attachment this layer cannot parse is one the server cannot check",
			ErrServerAttachmentKindUnknown, uint16(a.Kind))
	}
	carried, set := a.bodyKind()
	if 1 < set || carried != a.Kind {
		return fmt.Errorf("%w: kind 0x%04x carries %d bodies, the last of them kind 0x%04x",
			ErrServerAttachmentBody, uint16(a.Kind), set, uint16(carried))
	}
	switch a.Kind {
	case AttachmentEpoch:
		return checkEpochAttachment(a.Epoch)
	case AttachmentRecovery:
		return checkRecoveryTag(a.Recovery)
	case AttachmentWrap:
		return checkWrapTag(a.Wrap)
	case AttachmentComplete:
		return checkEpochComplete(a.Complete)
	case AttachmentEpochDigest:
		return checkEpochDigestAttachment(a.EpochDigest)
	}
	return nil
}

// Whether a door serves the kind it has been handed.
//
// It answers yes for a kind this package does not define at all, and that is the whole of
// how it composes with the refusal above rather than shadowing it: an undefined kind is
// checkServerAttachment's to refuse, with ErrServerAttachmentKindUnknown and the sentence
// about an attachment the server cannot check, and a gate here that got there first would
// re-label every one of the 65530 codes nothing defines as a kind that merely went to the
// wrong window. The two refusals mean different things to a caller — "nobody defines this"
// against "this door does not serve it yet" — so they are two sentinels and this one is
// reached only for a kind that is well formed and defined.
//
// EACH OF THE THREE CALLS TO THIS FUNCTION HAS A WITNESS, and it is written down here
// because for one commit two of them had none. When section 5.1 check 3's door was
// widened to every kind this package defines, its complement went empty — and every test
// that could see a served check was written over the kinds a door REFUSES, so it visited no
// pair at that door. Both of its calls, in EncodeServerAttachment and ParseServerAttachment,
// could then be deleted outright with this package, messagegroup and protocol green. What
// holds them now is TestEachDoorsServedMapIsConsultedByTheEntryPointsWrittenDownForIt, which
// does not read the complement: it deletes one entry from a door's map, in process, and
// requires the door to refuse that kind and to go on serving the rest. A door that stopped
// asking its map — including one that answered correctly from a hardcoded switch — fails it.
func checkAttachmentKindServed(kind ServerAttachmentKind, served map[ServerAttachmentKind]bool, door string) error {
	if !serverAttachmentKindKnown[kind] || served[kind] {
		return nil
	}
	return fmt.Errorf("%w: kind 0x%04x at %s", ErrServerAttachmentKindNotServed, uint16(kind), door)
}

// The epoch attachment's own checks: three exact widths, the algorithm identifier its kind
// names, and a wrap set with something in it.
func checkEpochAttachment(e *EpochAttachment) error {
	if err := checkAttachmentAlgId(AttachmentEpoch, e.AlgId); err != nil {
		return err
	}
	if err := checkAttachmentWidth("write_key", e.WriteKey, epochWriteKeyBytes); err != nil {
		return err
	}
	if err := checkAttachmentWidth("read_key", e.ReadKey, epochReadKeyBytes); err != nil {
		return err
	}
	if err := checkAttachmentWidth("group_context_hash", e.GroupContextHash, epochGroupContextHashBytes); err != nil {
		return err
	}
	// spec B section 5.1 check 3 names this one outright. An epoch opens with at least its
	// own snapshot in the wrap set, so zero is not a small fan out but a marker condition
	// no EpochComplete can ever satisfy: the group would stay readable and unwritable with
	// nothing able to close it.
	if e.ExpectedWrapCount == 0 {
		return fmt.Errorf("%w: the epoch it opens expects no wraps at all, and its own snapshot is one",
			ErrExpectedWrapCountZero)
	}
	return nil
}

// The epoch digest attachment's own checks: the algorithm identifier its kind names, two
// exact widths, and a wrap set with something in it.
//
// It is checkEpochAttachment's list with write_key and read_key struck and the digest put
// in their place, which is what the amendment is, and it is DELIBERATELY the same list in
// every other respect. What is absent is as load bearing here as it is there: no comparison
// against either durable_ttl_seconds sentinel, no bound on media_ttl_seconds, and no epoch
// arithmetic.
//
// THE THREE CLAUSES OF CHECK 3 THAT ARE NOT HERE ARE NOT HERE FOR KIND 0x0005 EITHER, and
// the reason is the one spec B section 5.1 check 3 already gives of kind 0x0001:
// "epoch == current_epoch + 1" and the marker's matching wrap_count both need group state
// the attachment does not carry, and "an epoch attachment iff is_commit" needs the record
// header beside it. A codec that reached for current_epoch would be a codec with a database.
// The fourth question this kind adds — does the digest equal H over the keys the submitter
// handed us — is the server's for exactly the same reason: this layer is never handed the
// keys, which is the point of the amendment.
func checkEpochDigestAttachment(e *EpochDigestAttachment) error {
	if err := checkAttachmentAlgId(AttachmentEpochDigest, e.AlgId); err != nil {
		return err
	}
	if err := checkAttachmentWidth("group_context_hash", e.GroupContextHash, epochGroupContextHashBytes); err != nil {
		return err
	}
	// the width IS the check this layer can make of the digest. It cannot recompute the
	// value — it holds neither key — so what it can say is that the field is a SHA-256
	// output's worth of octets and not a truncation somebody would compare a prefix of.
	if err := checkAttachmentWidth("epoch_keys_digest", e.EpochKeysDigest, epochKeysDigestBytes); err != nil {
		return err
	}
	if e.ExpectedWrapCount == 0 {
		return fmt.Errorf("%w: the epoch it opens expects no wraps at all, and its own snapshot is one",
			ErrExpectedWrapCountZero)
	}
	return nil
}

// The recovery tag's own checks: the handle the server indexes by, the Ed25519 public half
// the client verifies under, and the algorithm identifier that says which signature scheme
// that is.
func checkRecoveryTag(t *RecoveryTag) error {
	if err := checkAttachmentAlgId(AttachmentRecovery, t.AlgId); err != nil {
		return err
	}
	if err := checkAttachmentWidth("recovery_handle", t.RecoveryHandle, recoveryHandleBytes); err != nil {
		return err
	}
	return checkAttachmentWidth("recovery_verify_pub", t.RecoveryVerifyPub, recoveryVerifyPubBytes)
}

// The wrap tag's own check: the 16 octet target the server serves a wrap by.
func checkWrapTag(t *WrapTag) error {
	return checkAttachmentWidth("wrap_target_handle", t.WrapTargetHandle, wrapTargetHandleBytes)
}

// The marker carries a u64 and a u32 and no field with a width to get wrong. Its one rule
// — that wrap_count equals the epoch's expected_wrap_count — is an equality against an
// attachment this layer is never handed, so it belongs to the server and is not restated
// here as a bound this function could only guess at.
//
// Which leaves the nil guard as the whole body, and it reads as the odd one out beside
// checkEpochAttachment, checkRecoveryTag and checkWrapTag, all three of which dereference
// the body they are handed without one. No input reaches it: checkServerAttachment runs the
// presence rule first, and bodyKind names AttachmentComplete only when Complete is set, so
// the marker's body is never nil by the time this is called. It stays for two reasons that
// outrank the symmetry. It fails closed if the presence rule is ever loosened, which is the
// only direction that edit goes. And it is the one thing this body does with the argument it
// is handed — take it out and the function ignores its own parameter, which is the
// placeholder shape mls/crypto_test.go's TestNoStubShapesRemainInSource refuses across this
// package and that one. The three siblings read their body because they have a width to
// check against; this one reads its body because a body with nothing to check is what is
// left to read.
func checkEpochComplete(c *EpochComplete) error {
	if c == nil {
		return fmt.Errorf("%w: kind 0x%04x carries no body", ErrServerAttachmentBody, uint16(AttachmentComplete))
	}
	return nil
}

// One length prefixed field against the exact width spec A section 5.11 gives it.
//
// One sentinel across all six, wrapped with the field's name and both counts, for the
// reason ErrBlobIdPresence is one sentinel across both directions of its rule: they are one
// rule — a field of this attachment is the width the spec gives it — and a caller that told
// them apart would be acting on a distinction the wire does not carry.
func checkAttachmentWidth(name string, field []byte, want int) error {
	if len(field) != want {
		return fmt.Errorf("%w: %s is %d octets, want exactly %d", ErrServerAttachmentFieldLength, name, len(field), want)
	}
	return nil
}

// One kind's algorithm identifier against the one master section 7.1 registers for it.
func checkAttachmentAlgId(kind ServerAttachmentKind, algId uint16) error {
	want, named := attachmentAlgIds[kind]
	if !named {
		return fmt.Errorf("%w: kind 0x%04x names no algorithm identifier and was asked about 0x%04x",
			ErrServerAttachmentAlgId, uint16(kind), algId)
	}
	if algId != want {
		return fmt.Errorf("%w: kind 0x%04x carries 0x%04x, want 0x%04x", ErrServerAttachmentAlgId, uint16(kind), algId, want)
	}
	return nil
}
