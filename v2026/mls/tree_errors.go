// The typed errors the ratchet tree and TreeKEM raise that carry no ValSem number.
//
// Kept out of errors.go for the reason errors_key_schedule.go gives: every ValSem-numbered
// sentinel and every ErrProfile* is the validation plan's, declared once there, and two
// plans editing one file during parallel waves is a merge conflict rather than a design
// boundary but is a real one. The fifteen names below are reserved by this plan and
// errors.go must not redeclare any of them -- package mls is one package, so a second
// declaration is a compile error, which is the loud half; the quiet half is a shape where
// both compiled, in two packages, and an errors.Is somewhere in the commit path stopped
// matching without saying so.
//
// None of the fourteen is owed to another plan, which is the question p4's task 13 had to
// answer differently: ErrPskNonceLength, ErrPskType and ErrDuplicatePsk are ValSem401 to
// ValSem403, so psk.go carries them unexported until the validation plan lands the real
// names. Nothing here is in that position. The interface registry names thirteen of the
// fifteen as this plan's own and the validation plan's errors.go block declares none of
// them, so they are exported here and the later tasks of this plan return them directly.
//
// The fourteenth, ErrRatchetTreeExtensionTag, is the one the registry does not name, and it
// landed with task 11 rather than with this file. It is here and not in tree.go for the
// reason the paragraph below gives -- an exported error in a file with no maintained class
// is one no sweep of this package judges -- and it is exported rather than carried
// unexported because no other plan declares it: the ValSem-numbered refusal task 11 owes
// the validation plan is ValSem300, and THAT one is carried unexported in tree.go on
// psk.go's terms.
//
// The fifteenth, ErrLeafBlank, is the registry's other omission and it is a SPLIT rather
// than an addition: the plan gave one sentinel to two conditions, and task 18 shipped with a
// sender whose slot the group had removed being told its leaf index was out of range. It is
// declared here on ErrRatchetTreeExtensionTag's terms and it deliberately does not wrap
// ErrLeafIndexOutOfRange -- see its own comment.
//
// Two of them WRAP a sentinel this package already declares, and that is the only thing in
// this file worth reading twice.
//
// tree_math.go, which landed in wave 1, already answers "leaf index outside the tree" with
// ErrLeafOutOfRange and "node index outside the tree" with ErrNodeOutOfRange. The registry
// nevertheless gives this plan ErrLeafIndexOutOfRange and ErrNodeIndexOutOfRange, because
// the ratchet tree refuses an out of range index at its own boundary -- SetLeaf, Blank,
// ParentAt -- rather than by forwarding whatever the arithmetic said. Declared as two fresh
// errors.New values, that is exactly the "two sentinels for one condition" this package's own
// error file headers warn about: a caller asking "was this a leaf index out of range" would
// get yes from one and no from the other depending on which layer happened to notice, and no
// round trip or count property can see the difference.
//
// So they wrap. errors.Is holds through both names, a caller may ask either question, and the
// pair keeps its own identity for the callers that care which layer refused. The precedent is
// this package's, twice over: ErrGroupContextTrailingBytes wraps syntax.ErrTrailingBytes for
// this reason, and crypto_errors.go's header describes errors.go's ErrBadSignature wrapping
// ErrCryptoBadSignature for it. TestOnlyTheTreeIndexErrorsAnswerToATreeMathSentinel is the
// half with teeth: wrapping is useful only while it stays exclusive, and a third error
// answering to ErrLeafOutOfRange would make "was this a leaf index problem" mean nothing.
//
// A third error ANYWHERE in this package, which is worth spelling out because the sweep it
// names judges only this file and the property callers depend on is not about files. A review
// wrote that third error into a new file of its own and the whole package stayed green.
// TestEveryExportedErrorOfThisPackageIsInAMaintainedClass and
// TestOnlyTheSanctionedWrapsHoldAcrossThisPackagesErrorClasses are what close that: every
// exported error of every non-test file has to belong to a class derived from its own file,
// and no pair of any two of those classes may answer for each other outside treeIndexWraps.
// The twenty-five tasks after this one add tree.go, leaf_node.go and update_path.go, and
// nothing obliges their errors to land here.
//
// ErrNodeTypeMismatch is deliberately NOT wrapped onto tree_math's ErrNodeIsParent, though the
// two read alike. ErrNodeIsParent is arithmetic -- this index is not a leaf index -- and is
// decided from the number alone. ErrNodeTypeMismatch is about what is STORED: a leaf slot
// holding a ParentNode, or a parent slot holding a LeafNode, at an index whose parity is
// perfectly correct. A caller repairing the first re-derives an index; a caller seeing the
// second has a malformed tree. Collapsing them would hand the second the first's repair.
package mls

import (
	"errors"
	"fmt"
)

var (
	// ErrLeafIndexOutOfRange is returned for a leaf index outside the ratchet tree the
	// operation was asked about. It wraps tree_math's ErrLeafOutOfRange so a caller that
	// only knows the arithmetic's sentinel still matches it -- see the file header.
	ErrLeafIndexOutOfRange = fmt.Errorf("mls: leaf index out of range: %w", ErrLeafOutOfRange)

	// ErrNodeIndexOutOfRange is returned for a node index outside the ratchet tree's node
	// array. It wraps tree_math's ErrNodeOutOfRange for the reason above.
	ErrNodeIndexOutOfRange = fmt.Errorf("mls: node index out of range: %w", ErrNodeOutOfRange)

	// ErrLeafBlank names a leaf index that is INSIDE the tree and whose slot is empty: a
	// member operating from a position the group removed, or from one that has never been
	// filled. RatchetTree.Leaf answers nil for that and for an index past the width alike,
	// and an operation that let the nil decide answered ErrLeafIndexOutOfRange for both.
	//
	// Two sentinels because the two repairs are opposite. An index past the width is a
	// caller that computed an index wrong and fixes it by recomputing one; a blank slot is a
	// caller whose index was correct the whole time and whose position no longer exists, and
	// the only thing it can do is rejoin the group. Telling the second to check its index
	// sends it to look at the one thing that is not the problem, which is the same failure
	// tree_errors.go's header describes ErrNodeTypeMismatch avoiding.
	//
	// It deliberately does NOT wrap ErrLeafIndexOutOfRange. The two index errors wrap a tree
	// math sentinel because they are the same QUESTION asked at two layers; this is a
	// different question about the same argument, so a caller asking "was my index out of
	// range" must get no from it. TestOnlyTheSanctionedWrapsHoldAcrossThisPackagesErrorClasses
	// is what holds that.
	ErrLeafBlank = errors.New("mls: the leaf at this index is blank")

	// ErrTreeMalformed names a ratchet tree whose stored shape is not a tree: a node array
	// whose width is not 2*n-1 for any n, a decode that produced a different number of nodes
	// than its length prefix promised, or any other structural condition that has to be
	// refused before an index is computed against it. It is separate from every content
	// failure below because a caller cannot repair it by re-fetching one node.
	ErrTreeMalformed = errors.New("mls: ratchet tree is malformed")

	// ErrNodeTypeMismatch names a node whose type does not match its position: a LeafNode at
	// an odd node index, or a ParentNode at an even one. The wire format carries the type as
	// a byte, so this is the shape an attacker-chosen ratchet_tree extension arrives in, and
	// it is refused rather than coerced -- coercing would let a sender decide which of two
	// structures every later hash is computed over.
	ErrNodeTypeMismatch = errors.New("mls: node type does not match its position in the tree")

	// ErrUnmergedLeavesNotSorted names an unmerged_leaves vector that is not strictly
	// ascending. RFC 9420 section 7.9.2 requires the order, and the requirement is not
	// cosmetic: the vector is hashed into the parent hash, so two orderings of one set are
	// two different parent hashes over one tree, and an implementation that sorted on read
	// would agree with itself and with nobody else.
	ErrUnmergedLeavesNotSorted = errors.New("mls: unmerged leaves are not sorted and unique")

	// ErrUnmergedLeafInconsistent names an unmerged leaf that the rest of the tree does not
	// agree is unmerged: a leaf listed at a parent it is not in the subtree of, or a leaf
	// listed at one node of its own direct path and absent from another. Either way the
	// resolution computed at that node is not the set the sender encrypted to, and the
	// mismatch surfaces as an undecryptable commit rather than as the inconsistency it is.
	ErrUnmergedLeafInconsistent = errors.New("mls: unmerged leaf is not consistent with the path to its parent")

	// ErrParentHashMismatch names a parent node whose stored parent_hash is not the value
	// recomputed from the tree. It is the check that binds a node's public key to the subtree
	// beneath it, so it is a refusal and never a warning: a tree that fails it is one where
	// somebody other than the path's author chose a public key on that path.
	ErrParentHashMismatch = errors.New("mls: parent hash does not verify")

	// ErrTreeHashMismatch names a ratchet tree whose recomputed tree hash is not the
	// tree_hash the GroupContext it is being checked against carries. The GroupContext is
	// what every epoch secret and every signature is bound to, so a tree that hashes to
	// something else is a different group's tree, whatever else about it verifies.
	ErrTreeHashMismatch = errors.New("mls: tree hash does not match the group context")

	// ErrLeafNodeSourceMismatch names a leaf node whose leaf_node_source is not the one its
	// position requires: a key_package leaf found in the tree, an update leaf in a
	// KeyPackage, a commit leaf anywhere but at the sender of an UpdatePath. The source
	// decides which of lifetime and parent_hash is present and therefore what the signature
	// covers, so accepting the wrong one is accepting a signature over a different structure.
	ErrLeafNodeSourceMismatch = errors.New("mls: leaf node source is wrong for this context")

	// ErrLeafNodeLifetime names a key_package leaf whose lifetime does not contain the
	// current time, within the caller's permitted clock skew. Separate from every signature
	// failure because it is the one leaf refusal that can become correct or incorrect with no
	// change to the bytes, so a caller retrying it is not making the same mistake twice.
	ErrLeafNodeLifetime = errors.New("mls: leaf node lifetime is not current")

	// ErrLeafKeysExtensionInvalid names an urmessage_leaf_keys (0xF002) extension body this
	// profile cannot act on: an alg_id that is not X-Wing, a device public key that is not
	// XwingPublicKeyLen bytes, or trailing bytes after the body. The length is the part that
	// matters most -- a short X-Wing key is not a weaker key, it is a key the KEM will refuse
	// or, worse, one whose classical half was truncated away.
	//
	// It is also what ParseLeafKeysFrom answers for an extensions vector entry carrying some
	// other type's tag. One sentinel and not two, because the question every caller of that
	// entry point is asking is "is there a leaf keys body here I can wrap to", and an entry
	// tagged urmessage_group_policy answers it exactly as an unimplemented alg_id does. A
	// second sentinel would split one refusal across two names and leave every caller that
	// matched the wrong one silently accepting the other.
	ErrLeafKeysExtensionInvalid = errors.New("mls: urmessage_leaf_keys extension is invalid")

	// ErrNoPathSecret is returned when a TreeKEMPrivate holds no path secret for a node it is
	// asked to derive a private key at. It is the ordinary condition for a member off the
	// path, not a fault, which is why it is its own sentinel: a caller that treats it as a
	// decryption failure would report ValSem203 for a node it was never meant to hold.
	ErrNoPathSecret = errors.New("mls: no path secret is available for this node")

	// ErrPathSecretMismatch names a recovered path secret that does not derive the public key
	// the tree carries at that node. Distinct from ErrNoPathSecret and from the ValSem203
	// decrypt failure, because a secret that decrypted cleanly and then derives the wrong key
	// means the sender's path and the tree disagree, which no retry and no re-fetch repairs.
	ErrPathSecretMismatch = errors.New("mls: path secret does not derive the node public key in the tree")

	// ErrRatchetTreeExtensionTag names an extensions<V> entry handed to ParseRatchetTreeFrom
	// under some other extension type's code point. Extension.ExtensionData is opaque, so a
	// ratchet_tree body under the external_senders tag is a structure that encodes, signs and
	// travels, and the only thing that can refuse it is a read side that was given the tag.
	//
	// It answers for the TAG alone, which is the deliberate difference from
	// ErrLeafKeysExtensionInvalid above, where one sentinel covers the tag and every refusal
	// of the body. The leaf keys body has no refusals of its own to tell apart; the ratchet
	// tree body has four -- ValSem300's trailing blank, ErrNodeTypeMismatch, ErrTreeMalformed
	// and syntax.ErrLengthExceedsMax -- a caller repairing each does something different, and
	// folding them into one name would hand a caller who passed the wrong entry a tree's
	// repair.
	ErrRatchetTreeExtensionTag = errors.New("mls: the extension is not tagged ratchet_tree")
)
