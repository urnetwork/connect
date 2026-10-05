// Typed errors for the storage layer. Every one of them is fatal by construction: spec
// A section 5.9 guardrail 7 says there is no path in this package that reports one and
// carries on, and that rule is what the shape of this file enforces.
//
// The rule has a second half worth stating here, because this is the file a reader
// checks it against. Nothing in this package signals a failure with a bool. The
// verifiers of spec A section 5.7 — VerifyWriteAuth, VerifyRequestAuth and
// VerifyRecoveryProof — return a bare bool because it is the answer to a constant time
// comparison rather than a report of something having gone wrong, and each of their
// callers is asserted to return on false. A predicate that answers a question about a
// legal value is in the same category and not an exception to anything: ClassIsPrunable
// says what a class is, and there is no failure in it to report. Everything that can
// actually fail hands back one of these values, so a caller that wants to continue past
// a failure has to write the code that ignores an error, in plain sight.
//
// Sentinels rather than error structs, and wrapped with %w at each site that has a
// value worth naming, so errors.Is holds for the caller while the message still carries
// the byte or the bucket that was refused.
//
// Nine of the names below are on spec A section 12.1's published surface, which is the
// surface the allowlist test in the message server repo asserts, and spec B section 12.1
// restates it. They were not on it originally — that block listed functions and types and no
// errors — and they were added as amendments A-8 and B-8 rather than kept internal,
// because the guardrail above only means anything if the caller can name what it caught:
// an error a server cannot name is one it can only match on message text.
//
// The rule for a sentinel added later is section 12.1's own rather than this file's, and it
// is reachability and not the count: amendments A-9 and B-9 of 2026-08-25 say so, in the
// two blocks the rule governs. That block is the allowlist of what the server may reach
// and not an inventory of what this package exports — it carries no preimage builder
// either, and aad.go exports two — so a sentinel a published function can return is an
// addition to the published surface and belongs in the same commit as the amendment that
// publishes it, while a sentinel only an unpublished function can reach is not, because
// publishing it would widen the server's allowlist with a name no server can use.
//
// Seven more are of the first kind and are owed the amendment rather than already carrying
// it. ErrServerAttachmentKindUnknown, ErrServerAttachmentKindNotServed,
// ErrServerAttachmentBody,
// ErrServerAttachmentNoneEncoded, ErrServerAttachmentFieldLength, ErrServerAttachmentAlgId
// and ErrExpectedWrapCountZero are all reachable from ParseServerAttachment and
// EncodeServerAttachment, and both of those are on section 12.1's block in spec A and in
// spec B's character for character restatement of it. So by the rule in the paragraph above
// each of the seven is owed a line in both refusal blocks, and neither block carries one yet:
// those blocks list nine names, all of them the record codec's, and were written before this
// encoding existed. Spec B section 5.1 check 3 is the caller that needs them — it is the
// server's static shape check, it calls ParseServerAttachment, and a refusal it cannot name
// is one it can only match on message text, which is exactly what the nine were amended in
// to prevent. The gap is recorded here because this file is where the rule is written down,
// and it is a spec defect of the same shape as ClassIsPrunable's absence from the same block.
//
// The last four below are of that kind, in two shapes. ErrRecordHeaderNil and
// ErrServerAttachmentMismatch are the preimage builders', those builders are on no line of
// section 12.1, and the server never decrypts, so it never builds an aad at all.
// ErrServerNonceEmpty and ErrAuthKeyLength are write_auth's, and they are of that kind for
// a second reason on top of the first: no function returns them. They are the cause the
// computing half panics with and the reason the verifying half answers false, so a server
// holding this package's published surface has no error path either one can arrive on, and
// putting them on the allowlist would name something no server can match. If any of the
// four ever becomes the return of a published function it stops being of that kind, and the
// amendment that publishes it lands with the change that made it reachable.
package message

import "errors"

var (
	// Fires when a retention class byte is outside the nine the wire table admits, and
	// when a RetentionClass tag is outside the four the package defines. Both are the
	// same mistake seen from the two sides of the same byte, and a caller that told
	// them apart would be acting on a distinction the wire does not carry.
	ErrRetentionClassUnknown = errors.New("message: retention class is not one of permanent, durable, media or eph")
	// Fires when an eph bucket is past 5. The ladder has six rungs and 0x16 is not a
	// legal wire byte, so this is a refusal rather than a truncation to the top rung:
	// a record written under a manufactured byte is one that every reader refuses,
	// including the sender's own other devices.
	ErrEphBucketOutOfRange = errors.New("message: eph bucket is outside 0..5")
	// Fires when a class other than eph is asked to carry a non zero eph bucket. The
	// bucket is only meaningful under eph, and the alternative to refusing is dropping
	// it silently — which stores the record as though the caller's belief about the
	// bucket had been right.
	ErrEphBucketOnNonEphClass = errors.New("message: a retention class other than eph carries a non zero eph bucket")
	// Fires when EncodeRecord is handed no record at all. A nil here is a caller bug
	// rather than a bad record, and it is reported rather than dereferenced because the
	// server encodes on the read path, where a panic would take a fetch for an unrelated
	// group down with it.
	ErrRecordNil = errors.New("message: no record to encode")
	// Fires when a record's leading format version byte is not the one this package
	// writes. It is its own sentinel rather than a generic decode failure because every
	// offset in the record is only meaningful under a known version, so this is the one
	// refusal a caller can act on — by asking for a newer client rather than by treating
	// the record as corrupt.
	ErrRecordFormatVersion = errors.New("message: record format version is not one this package reads")
	// Fires when the is_commit byte is neither 0 nor 1. The server acts on this field, so
	// it is authenticated, and a decoder that read it as "any non zero is true" would let
	// two implementations that disagree about the byte both believe they had parsed the
	// record while the mac over it says they had not.
	ErrIsCommitNotBoolean = errors.New("message: the is_commit byte is neither 0 nor 1")
	// Fires when a size bucket is past the top of the ladder. There are six rungs and no
	// seventh, and a record naming one has no body length, no ct_body check the server
	// could apply, and no padding rung its sender could have padded to.
	ErrSizeBucketOutOfRange = errors.New("message: size bucket is outside 0..5")
	// Fires in both directions of the blob rule: a record on the blob rung whose blob id
	// is not exactly 32 bytes, and a record off it that carries one at all. The two are
	// one sentinel because they are one rule — the presence of the blob id is the size
	// bucket restated — and a caller that told them apart would be acting on a
	// distinction the record does not carry.
	ErrBlobIdPresence = errors.New("message: blob id presence disagrees with the size bucket")
	// Fires when a ct_body is neither absent nor exactly the ciphertext length of its size
	// bucket. Absent is legal on the read path, where the body may have been erased by
	// retention or skipped by a heads_only fetch; any other length is a body that was not
	// padded to its rung, which leaks the length the padding exists to hide.
	ErrCtBodyLength = errors.New("message: ct_body length is neither absent nor the size bucket's")
	// Fires when a server attachment names a kind spec A section 5.11 does not define,
	// on either side of the codec. It is a refusal and never a silently ignored
	// attachment: spec B section 5.1 check 3 is what stands between a record and the
	// database, and an attachment the server cannot parse is one it cannot check, so a
	// kind nobody recognises would carry the epoch key install, the recovery index and
	// the wrap index past every question check 3 asks of them.
	ErrServerAttachmentKindUnknown = errors.New("message: server attachment kind is not one spec A section 5.11 defines")
	// Fires when a door is handed a kind this package DOES define and that door does not
	// serve. One sentinel because it is one rule — a door serves the kinds it serves — and
	// a caller that told one door's refusal from the other's would be acting on a
	// distinction the wire does not carry. The door is named in the message, for the reader
	// who has to know which one answered.
	//
	// WHICH PAIRS REACH IT, AND THE DATE THE ANSWER MOVED. This read "kind 0x0005 at spec B
	// section 5.1 check 3's door, and any of section 5.11's five at the epoch digest door",
	// which was both directions of the rule until 2026-09-23. On that date ruling 33 put the
	// epoch keys on the request and section 5.1 check 3's door was widened to kind 0x0005,
	// so its complement is now EMPTY and the pairs that reach this sentinel are the five
	// section 5.11 kinds at the epoch digest door alone. attachment_test.go's
	// TestARecordCarriesAKindADoorRefusesByName walks the whole (door, kind) product and
	// fails if no pair reaches here at all.
	//
	// It is NOT ErrServerAttachmentKindUnknown, and the difference is the whole of ruling
	// 27's rollout. "Nobody defines this code" is a record no conforming implementation
	// ever wrote. "This door does not serve this code" is a record a conforming
	// implementation writes on purpose. Until this package was widened, that is what made a
	// server which had not learned to carry the epoch keys beside the record refuse such a
	// commit loudly at check 3 rather than install an epoch whose keys it was never handed —
	// the stale half of spec B section 5.4's acceptance window, which is now held by
	// binaries built before that date and not by this one. A caller matches the sentinel and
	// never the message text, which is guardrail G7 of spec A section 5.9 and the reason
	// this is a sentinel at all.
	//
	// It is owed a line in spec A and spec B section 12.1's refusal blocks under the rule
	// the comment at the top of this file states, exactly as the six before it are: it is
	// reachable from ParseServerAttachment and EncodeServerAttachment, both of which are on
	// that published block. It is recorded here with them rather than quietly added.
	ErrServerAttachmentKindNotServed = errors.New("message: a server attachment door was handed a kind it does not serve")
	// Fires when an attachment's kind and the body it carries disagree, in both
	// directions: a kind with no body, a kind with the wrong body, and two bodies set at
	// once. One sentinel because it is one rule — an attachment is exactly one kind and
	// carries exactly that kind's body — and resolving the disagreement instead of
	// refusing it would encode whichever half the encoder happened to prefer, which is a
	// record whose author believed it said something else.
	ErrServerAttachmentBody = errors.New("message: a server attachment's kind and the body it carries disagree")
	// Fires when the absent attachment arrives spelled out as kind 0x0000 with an empty
	// body rather than as the empty field. Both specs say an ordinary record carries a
	// zero length server_attachment and NOT kind 0x0000, and section 5.11's test
	// obligation says why: the two must encode identically or H(server_attachment)
	// differs between client and server. Accepting the long form would give one
	// attachment two encodings with two different hashes, and the write_auth mac and both
	// aeads are over exactly one of them.
	ErrServerAttachmentNoneEncoded = errors.New("message: an absent server attachment is the empty field and not an encoded kind 0x0000")
	// Fires when one of the six length prefixed fields of an attachment is not the exact
	// width spec A section 5.11 gives it. One sentinel across all six for the reason
	// ErrBlobIdPresence is one across both directions of its rule: they are one rule, and
	// a caller that told them apart would be acting on a distinction the wire does not
	// carry. Spec B section 5.1 check 3 names four of the six outright.
	ErrServerAttachmentFieldLength = errors.New("message: a server attachment field is not the exact width spec A section 5.11 gives it")
	// Fires when an attachment carries an algorithm identifier other than the one master
	// section 7.1 registers for its kind. It is the "known alg_id" of spec B section 5.1
	// check 3, and it is per kind rather than against the registry as a whole: an epoch
	// attachment announcing Ed25519 claims its two 32 octet keys came out of a signature
	// algorithm, which is a record built by something that does not know what the field
	// is for.
	ErrServerAttachmentAlgId = errors.New("message: a server attachment carries an algorithm identifier its kind does not name")
	// Fires when an epoch attachment expects no wraps at all. Spec B section 5.1 check 3
	// requires expected_wrap_count > 0, and the epoch it opens has at least its own
	// snapshot in the wrap set, so zero is not a small fan out: it names a count the
	// EpochComplete marker can never match, which leaves the group readable and not
	// writable with nothing able to close it.
	ErrExpectedWrapCountZero = errors.New("message: an epoch attachment expects no wraps at all")
	// Fires when the two keys handed beside a record are not the ones that attachment's
	// H(epoch_keys) is over. It is the ONE check ruling 27 adds to spec B section 5.1 check
	// 3, and CheckEpochKeysDigest is the whole of it.
	//
	// It is not a shape refusal like the six above it, and the difference matters to a
	// server deciding what to log: every one of those says a record is malformed, and this
	// one says a record is well formed and does not bind the keys its submitter sent. Under
	// spec A section 4.5 both merge into REASON_REJECTED on the wire — the client is told
	// nothing it could use to probe — but they are different events on the server's side of
	// that merge, and a server that could not tell them apart would be unable to say whether
	// it was looking at an old client or at a forgery attempt.
	//
	// It is reachable from CheckEpochKeysDigest, which is on the published surface the other
	// repository calls, so it is owed a line in spec A and spec B section 12.1's refusal
	// blocks under the rule at the top of this file, beside ErrServerAttachmentKindNotServed
	// which is owed one for the same reason.
	ErrEpochKeysDigestMismatch = errors.New("message: the epoch keys handed beside a record do not match the digest its attachment carries")
	// Fires when NewEpochDigestAttachment is handed a body whose epoch_keys_digest is
	// already filled. That field is the one of the seven that is not the caller's to choose:
	// it is computed here from the Epoch of the body being built, which is the whole of why
	// that constructor exists, and a caller that arrived with one had either computed it at
	// an epoch of its own — the mismatch the constructor prevents — or is copying a body
	// that was already built, which is not a construction. Refused rather than overwritten,
	// because overwriting would discard a value its author believed in and say nothing.
	ErrEpochKeysDigestPresence = errors.New("message: an epoch digest attachment arrived with its digest already filled")
	// Fires when AADHead is handed no header at all. The same caller bug ErrRecordNil
	// names, one layer in, and reported rather than dereferenced for the reason nothing in
	// this package panics: a preimage builder runs on the seal and the open path alike, and
	// a nil there would take down a fetch of records that are every one of them well formed.
	ErrRecordHeaderNil = errors.New("message: no record header to build a preimage over")
	// Fires when AADHead's attachment argument and the header's own ServerAttachment field
	// disagree. The attachment is one value carried in two places — the argument, which is
	// the shape spec A section 12.1 publishes for the sibling WriteAuthPreimage, and the
	// header field the parser fills in — and a caller that lets them differ seals ct_head
	// under a preimage no reader will reconstruct, which surfaces as an aead failure on a
	// record that is otherwise entirely valid. Refused rather than resolved: preferring one
	// of the two would make a record's fate depend on which one this function happened to
	// pick.
	ErrServerAttachmentMismatch = errors.New("message: the server attachment argument and the header's own field disagree")
	// Fires when either authenticator of spec A section 5.7 is asked to compute against an
	// empty server nonce. That section's own sentence is the rule — this layer takes the
	// nonce as an opaque byte string and refuses to compute with an empty one — and the
	// alternative to refusing is a tag over a preimage with no connection in it, which every
	// record that lost its nonce would carry alike. The computing half panics with this
	// wrapped and the verifying half answers false on it; writeauth.go's comment argues why
	// the two halves differ.
	ErrServerNonceEmpty = errors.New("message: the server nonce is empty, and this layer does not compute with an empty one")
	// Fires when a mac key is not exactly thirty two octets. HMAC accepts a key of any
	// length, the empty one included, so without this a server that looked up a missing
	// write key and passed the nil it got back would compute a mac under the empty key — and
	// a client that had derived nothing would compute the same one, so two ends holding no
	// key would authenticate. Master section 8.3 puts both keys on the wire at exactly this
	// width, so any other length is a bug rather than a shorter key.
	ErrAuthKeyLength = errors.New("message: a mac key is not the thirty two octets both derivations produce")
)
