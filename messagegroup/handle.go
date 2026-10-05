// The three handles a record carries: the group handle key every member derives once at group
// creation, the sender handle the server routes on, and the wrap target handle a device wrap is
// addressed to.
//
// All three are expansions of the storage root and none of them is invertible by the server,
// which is the whole point of the family. Spec A section 5.3 and section 5.11 give two of the
// three; MASTER section 8's RECORD listing gives the third, and that asymmetry is worth naming
// rather than smoothing over:
//
//	group_handle_key   = HKDF-Expand(storage_root[0], "gh/v1", 32)
//	sender_handle  16B = HKDF-Expand(group_handle_key, "sh/v1" | LP(leaf_index), 16)
//	                     stable per group; every member computes it; the server cannot invert it
//	wrap_target_handle = HKDF-Expand(group_handle_key, "wt/v1" | u64(epoch) | u32(leaf_index), 16)
//
// sender_handle's formula is MASTER's. Spec A section 5.3 declares the function and gives no
// derivation at all, while every neighbouring handle in section 5.3 and section 5.11 has one --
// open item M1-8, and the omission matters more than most because sender_handle is in
// RecordHeader, in aad_head, in aad_body, in the write_auth preimage and is the column spec B
// keys message_sender.last_stream_index on. Two implementations choosing differently disagree on
// every aead and every mac in the system and each one's own tests stay green.
//
// LP(leaf_index) is the one place in this project where LP wraps an INTEGER. Section 5.11 defines
// LP(x) as a thirty two bit big endian length prefix followed by x, and every other use wraps a
// byte string, so "what is x" has two readings: the four octet big endian encoding, giving eight
// octets, or a minimal encoding, giving five for a small leaf. The asymmetry that makes this real
// rather than pedantic is one line down -- wrap_target_handle, in the same family, writes
// u32(leaf_index) RAW with no LP at all. This file takes the four octet reading, and it takes it
// in ONE helper, leafIndexLP, so that M1-8's ruling is a single edit and cannot leave two
// derivations disagreeing. It is wire visible and it blocks the A6 freeze.
//
// The epoch zero obligation is a persistence requirement rather than a derivation, and no
// section of any spec says where it lives. group_handle_key takes storage_root[0] specifically,
// so that -- MASTER section 8 -- "group_handle_key is what makes sender_handle and
// wrap_target_handle survive an epoch change. A member that does not hold it cannot compute its
// own handle and therefore cannot write."
//
// WHAT IS PERSISTED IS THIS KEY AND NOT THE ROOT IT CAME FROM, and the distinction is worth the
// sentence because both values are thirty two octets and the wrong one is a working program.
// MASTER's clause is about what a member HOLDS, and it names the key. storage_root[0] is strictly
// more: every class key, the write key and the read key of epoch zero expand from it, so a device
// that kept it for the life of the group would be keeping epoch zero's whole key schedule forever
// in order to recover a routing identifier every member of the group already knows. So this
// derivation is computed ONCE, at group creation, and its ANSWER is what is durably kept; the root
// it was expanded from is dropped with the rest of the epoch. GroupSession's construction is where
// the kept value lives and its parameter is named groupHandleKeyEpoch0 for exactly that reason;
// open item M1-4 records that the spec says neither. Deriving this key from the current epoch's
// root instead passes every single epoch test there is and breaks every group at its first
// commit.
package messagegroup

import (
	"encoding/binary"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// The three labels, raw ascii and never length prefixed, and three constants rather than one
// construction with the two letters substituted -- the same rule keyschedule.go's three class
// labels are written under, and writeauth.go's two before them. All three are the same length
// and the separation does not rest on that: they disagree at index zero, which is inside the
// shortest of them.
const (
	groupHandleKeyInfo   = "gh/v1"
	senderHandleInfo     = "sh/v1"
	wrapTargetHandleInfo = "wt/v1"
	// MASTER section 8.4.5's message_id, RULED 2026-09-15. It is a fourth member of this
	// family rather than a neighbour of it: it takes group_handle_key for the same reason
	// sender_handle does, and it is written as its own constant for the reason the three
	// above it are.
	messageIdInfo = "mid/v1"
)

// The widths MASTER section 8 gives: the group handle key is thirty two octets and both handles
// are sixteen.
//
// Sixteen is expanded to and never truncated from thirty two. HKDF-Expand at length sixteen and
// the first sixteen octets of an expansion at length thirty two are the same bytes, so the two
// are indistinguishable here today -- and they stop being the same the moment a length above the
// hash size is asked for, which is exactly the kind of difference that is discovered by a second
// implementation rather than by a test. Asking the kdf for the width the wire carries is what
// keeps this file's derivations transcriptions of the spec rather than of themselves.
const (
	groupHandleKeyBytes = 32
	handleBytes         = 16
	// MASTER section 8.4.5 gives message_id as thirty two octets.
	messageIdBytes = 32
)

// GroupHandleKey derives group_handle_key from the group's EPOCH ZERO storage root.
//
// The argument is named for the epoch it must come from because that is the only thing about
// this function a caller can get wrong, and getting it wrong is invisible until the group's
// first commit: every handle in the group changes at every epoch, no member can compute another
// member's handle, and every write is refused by a server that cannot resolve the sender.
// The width is refused here for the same reason the two handles below refuse a short group
// handle key, and the argument was missing at this derivation until it was measured: a root
// that is not thirty two octets expands to a well formed key, every member of the group
// computes a different one, and the only thing that used to refuse anything was mls's own
// expand -- which names mls's contract rather than this layer's, and only for a root SHORTER
// than the hash. A root of sixty four octets, which is the plausible shape of a value decoded
// out of durable storage, was accepted in silence.
func GroupHandleKey(storageRootEpoch0 []byte) []byte {
	refuseWrongWidthStorageRoot(storageRootEpoch0)
	return keyScheduleExpand(storageRootEpoch0, []byte(groupHandleKeyInfo), groupHandleKeyBytes)
}

// SenderHandle derives the stable per member handle the server routes a record on.
//
// It depends on the group handle key and on the leaf and on nothing else, which is what makes it
// survive an epoch change: MASTER section 8 says every member computes it and the server cannot
// invert it, and both halves of that need the key to be the group's first rather than the
// epoch's.
//
// It panics on a group handle key that is not thirty two octets, with the wrapped sentinel as
// the panic value, which is the shape writeauth.go's computing half already uses for a mac key
// of the wrong width and for the same reason: the published signature has no error to return,
// and the alternative is a well formed handle derived from a truncated key -- a handle every
// other member of the group would compute differently, on every record this member ever writes.
// Nothing here is reachable from the network: the key is this member's own persisted derivation.
func SenderHandle(groupHandleKey []byte, leaf uint32) [16]byte {
	refuseShortGroupHandleKey(groupHandleKey)
	return [16]byte(keyScheduleExpand(groupHandleKey, leafLabelledInfo(senderHandleInfo, leaf), handleBytes))
}

// MessageId derives MASTER section 8.4.5's identifier for one record.
//
//	message_id = HKDF-Expand(group_handle_key,
//	                         "mid/v1" | LP(group_id) | LP(sender_handle) | u64(stream_index), 32)
//
// RULED 2026-09-15, and it is in this file rather than in keyschedule.go because it takes the
// same key as SenderHandle one derivation up and for the same reason: group_handle_key is the
// group's one LIFETIME value, fixed at creation and never rotated, so an id expanded from it
// survives every commit. A reply to a message four epochs old still names it. An id under
// storage_root[n] would be a different id at every epoch, which is an id the thing it names
// cannot be found by.
//
// IT IS KEYED AND NOT HASHED, and that is the whole of why it takes a key at all. All three
// inputs are PLAINTEXT record header fields that the message server holds for every record it
// stores, so an unkeyed digest of them would be an identifier the server computes for free --
// a join key between any message_id that ever leaves a client and the row it names. Every
// member already holds group_handle_key or cannot compute its own sender_handle and therefore
// cannot write at all, so keying costs no distribution.
//
// THE THREE INPUTS ARE THE TRIPLE MLS SIGNS, which is the second half of the 2026-09-15 ruling
// and is what makes this an id rather than a label. (group_id, sender_handle, stream_index) sits
// inside AAD_body; AAD_body is hashed into aad_mls; aad_mls is the inner frame's
// authenticated_data, which is inside FramedContentTBS and therefore inside the sender's own
// signature. Before MASTER section 8.4.1 the same triple was covered by a group-wide key alone,
// so the id was the GROUP's claim; it is now the SENDER's.
//
// NEITHER SIDE NEEDS THE BODY. The sender has the reserved stream_index before it seals, so a
// reply can name its own parent optimistically; a receiver reads all three fields off the
// plaintext header, so an EPH row whose ct_body has been erased still has its id for the
// placeholder that must render in order. That is why the MLS generation is NOT one of the
// inputs: it lives inside the encrypted SenderData, and an id that needs the body cannot key a
// row that outlives the body.
//
// LP(x) here wraps a BYTE STRING and not an integer, so it carries none of the ambiguity open
// item M1-8 records about LP(leaf_index): group_id is thirty two octets and sender_handle is
// sixteen, so the info is 6 + (4+32) + (4+16) + 8 = 70 octets exactly.
//
// The SDK spells the answer as sixty four lowercase hex characters (spec A section 7.1). Where a
// preimage takes LP(message_id) it takes THESE THIRTY TWO OCTETS and never the spelling.
//
// It panics on a group handle key of the wrong width, which is SenderHandle's shape and
// SenderHandle's reason: the published signature has no error, and the alternative is a well
// formed id derived from a truncated key -- an id every other member of the group would compute
// differently for the same record.
func MessageId(groupHandleKey []byte, groupId [32]byte, senderHandle [16]byte,
	streamIndex uint64) [32]byte {

	refuseShortGroupHandleKey(groupHandleKey)
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(messageIdInfo))
	writer.WriteOpaqueLP(groupId[:])
	writer.WriteOpaqueLP(senderHandle[:])
	writer.WriteUint64(streamIndex)
	info, err := writer.Bytes()
	if err != nil {
		panic(fmt.Errorf("messagegroup: a message id's info could not be built: %w", err))
	}
	return [32]byte(keyScheduleExpand(groupHandleKey, info, messageIdBytes))
}

// WrapTargetHandle derives the handle one device wrap of one epoch is addressed to.
//
// It depends on the epoch AND on the leaf, so the same device at two epochs is two different
// targets -- which is what stops the server from following one device across a group's life by
// watching which wrap it fetches.
//
// WHICH epoch is the one thing about this function a caller can get wrong, so the argument is
// named for it. It is the CONTENT epoch -- the epoch whose secrets the wrap carries -- and it is
// deliberately NOT the record's own epoch field. Spec A section 5.11 annotates the sibling wrap
// info block in exactly those words, and sets it against an AAD_head block annotated "the
// RECORD's epoch and the RECORD's stream index"; MASTER section 7's info table says the same of
// u64(epoch), and MASTER section 8.3's WrapTag carries the content epoch. A caller reaching for
// RecordHeader.Epoch here produces a well formed sixteen octet handle that no fetcher resolves,
// with no error anywhere -- which is why GroupHandleKey's argument is named storageRootEpoch0
// one derivation up, and this one had to carry the same care.
//
// leaf_index 0xFFFFFFFF is the snapshot's, per section 5.11, and it is COMPUTED here rather than
// special cased. A branch for it would be a branch a second implementation might not have, and
// the value is an ordinary leaf index to every line of this derivation.
//
// The leaf index is written RAW, four octets big endian, with no length prefix. That is not an
// oversight and it is not consistency with sender_handle: MASTER writes the two differently and
// this file follows MASTER. Open item M1-8 carries the asymmetry.
func WrapTargetHandle(groupHandleKey []byte, contentEpoch uint64, leafIndex uint32) [16]byte {
	refuseShortGroupHandleKey(groupHandleKey)
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(wrapTargetHandleInfo))
	writer.WriteUint64(contentEpoch)
	writer.WriteUint32(leafIndex)
	info, err := writer.Bytes()
	if err != nil {
		panic(fmt.Errorf("messagegroup: the wrap target handle's info could not be built: %w", err))
	}
	return [16]byte(keyScheduleExpand(groupHandleKey, info, handleBytes))
}

// The ONE reading of LP(leaf_index) in this package, and the one place open item M1-8's ruling
// will land.
//
// x is the four octet big endian encoding of the leaf index, so LP(leaf_index) is eight octets:
// the length 00 00 00 04 followed by the index. The alternative reading -- a minimal encoding,
// five octets for a small leaf -- produces a handle of exactly the same width from exactly the
// same inputs, so no length check, no round trip and no test inside one implementation can tell
// the two apart.
//
// The prefix is written by mls/syntax's WriteOpaqueLP, which is the tree's one LP implementation
// and is deliberately not WriteOpaque, mls's varint: codec.go's header comment and
// mls/syntax/encode.go both say the two are never interchangeable.
func leafIndexLP(leaf uint32) []byte {
	writer := syntax.NewWriter()
	writer.WriteOpaqueLP(binary.BigEndian.AppendUint32(nil, leaf))
	prefixed, err := writer.Bytes()
	if err != nil {
		panic(fmt.Errorf("messagegroup: the length prefix around a leaf index could not be built: %w", err))
	}
	return prefixed
}

// The ONE assembly of a label followed by a length prefixed leaf index, so that every
// derivation MASTER writes as label | LP(leaf_index) is built by one body.
//
// It exists because this file used to hold two assembly styles for one family of preimages:
// sender_handle appended to a converted string while wrap_target_handle wrote through
// mls/syntax's writer. There is no behavioural difference between them today -- both KAT sets
// reproduce either way -- and that is the point: a second assembly of one shape is a second
// place for a conversion, an order or a prefix to drift, and this project's rule is one
// assembly per preimage. record_key[0] is the second member of the family and it lands on this
// helper rather than on a third spelling of it.
//
// It is a WriteRaw of the label and a WriteRaw of the prefix leafIndexLP already built, and not
// a WriteOpaqueLP here: the length prefix belongs to the leaf index and lives in the one place
// open item M1-8's ruling will land.
func leafLabelledInfo(label string, leaf uint32) []byte {
	writer := syntax.NewWriter()
	writer.WriteRaw([]byte(label))
	writer.WriteRaw(leafIndexLP(leaf))
	info, err := writer.Bytes()
	if err != nil {
		panic(fmt.Errorf("messagegroup: the info around a labelled leaf index could not be built: %w", err))
	}
	return info
}

// The width refusal both handles make, in one place so the two cannot disagree about it.
func refuseShortGroupHandleKey(groupHandleKey []byte) {
	if len(groupHandleKey) != groupHandleKeyBytes {
		panic(fmt.Errorf("%w: %d octets, want %d", ErrGroupHandleKeyLength, len(groupHandleKey), groupHandleKeyBytes))
	}
}

// The width refusal every derivation taking a storage root makes.
//
// It is one body for the reason the one above is: GroupHandleKey and DeriveClassKeys both hang
// a whole epoch off this value and a disagreement between them about what a root is would be a
// disagreement about which of the two a wrong width is caught by.
func refuseWrongWidthStorageRoot(storageRoot []byte) {
	if len(storageRoot) != classKeyBytes {
		panic(fmt.Errorf("%w: %d octets, want %d", ErrStorageRootLength, len(storageRoot), classKeyBytes))
	}
}
