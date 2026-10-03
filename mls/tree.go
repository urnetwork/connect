// The ratchet tree of RFC 9420 section 7: the ParentNode structure and its codec, the Node
// union that occupies one array position, the optional<Node> that one position becomes on the
// wire, and the RatchetTree container every later task of this plan operates on.
//
// Two things in this file decide whether this implementation agrees with a peer or forks from
// it, and neither is visible to a round trip.
//
// The first is what a BLANK node is. RFC 9420 section 4.2 stores the tree as
// optional<Node> ratchet_tree<V>, so a blank position is an ABSENT entry and not a zero valued
// one: absent is a single 0x00 presence octet, present is 0x01 followed by a whole node. An
// implementation that stored a blank as a zero valued Node would round trip against itself
// perfectly, would agree with itself about every accessor, and would hash a different tree from
// every peer -- which is a fork, not a parse error. So a blank here is a nil entry in the node
// array and nothing else, NodeType has no zero valued member so a Node that was never filled in
// cannot be mistaken for either kind, and SetLeaf and SetParent refuse a nil payload rather than
// storing an occupied slot with nothing in it. TestABlankAndAZeroValuedNodeEncodeDifferently and
// TestBlankPositionsAreExactlyTheUnsetOnesAtEveryWidth are what hold it.
//
// The second is unmerged_leaves. RFC 9420 section 7.9.2 requires the vector strictly ascending,
// and the requirement is not cosmetic: the vector is hashed into the parent hash, so two
// orderings of one set are two different parent hashes over one tree. An encoder that emitted
// insertion order and a decoder that accepted it would round trip byte exact and disagree with
// every peer, and one that quietly SORTED on read would agree with itself and with nobody,
// since the bytes a signature and a tree hash were taken over are the bytes that arrived. Both
// halves of this codec therefore REFUSE a vector that is not strictly ascending, with
// ErrUnmergedLeavesNotSorted, rather than repairing it or passing it on.
package mls

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// RFC 9420 section 7. Leaf nodes sit at even node indices, parent nodes at odd ones, and the
// octet below is what a decoder reads to learn which of the two a present entry holds.
//
// Neither member is zero, deliberately. The zero value of a Node is therefore a node of no
// type, which no encoder will write and no decoder will accept, so the Go zero value cannot be
// mistaken for a leaf, for a parent, or -- the one that matters -- for a blank.
type NodeType uint8

const (
	NodeTypeLeaf   NodeType = 1
	NodeTypeParent NodeType = 2
)

// ParentNode is RFC 9420 section 7.1:
//
//	struct {
//	    HPKEPublicKey encryption_key;
//	    opaque parent_hash<V>;
//	    uint32 unmerged_leaves<V>;
//	} ParentNode;
//
// EncryptionKey and ParentHash are both opaque<V> and adjacent, which is the pair a symmetric
// field order swap hides in: swapped in both halves they round trip perfectly and produce a
// parent hash no peer computes. TestParentNodeMarshalMatchesTheHandDerivedGoldens is the only
// thing in this package that separates the two orders, because it states the encoding without
// reference to this code.
type ParentNode struct {
	EncryptionKey  HpkePublicKey
	ParentHash     []byte
	UnmergedLeaves []LeafIndex
}

// checkUnmergedLeavesSorted refuses a vector that is not STRICTLY ascending, which is both
// halves of RFC 9420 section 7.9.2's requirement at once: sorted, and free of repeats.
//
// It runs on the encode side as well as the decode side. The decode side is the obvious half --
// a peer's unsorted vector is a tree this implementation must not adopt, and adopting it after
// sorting it would compute a parent hash over bytes nobody sent. The encode side is the half
// that keeps THIS implementation from being the peer that forks: a caller that appended an
// unmerged leaf without inserting it in order has built a tree whose hash no other member
// reproduces, and the cheapest place to find that out is the encode that would have published
// it.
func checkUnmergedLeavesSorted(leaves []LeafIndex) error {
	for i := 1; i < len(leaves); i += 1 {
		if leaves[i-1] >= leaves[i] {
			return ErrUnmergedLeavesNotSorted
		}
	}
	return nil
}

// maxUnmergedLeaves is the longest unmerged_leaves vector this codec writes or accepts, in
// ELEMENTS, derived from the byte bound rather than written down: an unmerged leaf is a uint32,
// so the default vector limit divided by four is the longest list a peer running that limit
// could have sent.
const maxUnmergedLeaves = syntax.MaxVectorLength / 4

// checkUnmergedLeavesBounded holds one parent node's unmerged_leaves at the DEFAULT vector limit
// whatever limit the surrounding encode or decode happens to be running at.
//
// It has to be applied here, and not left to the writer or the reader the caller opened, because
// the syntax package inherits its limit DOWNWARDS on purpose: subReader hands the parent's limit
// to every nested read and WriteVector builds its scratch at the outer limit, which is what lets
// a ratchet_tree running at MaxRatchetTreeLength carry a structure whose fields are larger than
// one ordinary field may be. That inheritance is right for the ARRAY and wrong for this vector.
// The ratchet_tree of RFC 9420 section 12.4.3.3 is raised because it is a whole tree; a single
// parent node's unmerged list is bounded by the group's leaf count, and one past MaxVectorLength
// is one no peer running the default limit could have sent -- which matters here because those
// bytes are covered by the parent hash and the tree hash, so a vector this implementation
// accepted and a peer refused is a tree only this implementation hashes.
//
// Without the check the argument written beside this pair's entry in the codec table is true
// only until the first ratchet tree codec opens a raised writer, and it becomes false with
// nothing failing. The bound is sixteen times smaller than the one it would inherit there.
//
// The decode side checks AFTER ReadVector rather than before, because the element count is not
// something a decoder can know without reading the region; what that costs is one allocation
// already bounded by the outer limit, on an input that had to carry those bytes anyway.
func checkUnmergedLeavesBounded(leaves []LeafIndex) error {
	if len(leaves) > maxUnmergedLeaves {
		return syntax.ErrLengthExceedsMax
	}
	return nil
}

func (self *ParentNode) MarshalMLS(w *syntax.Writer) error {
	if err := checkUnmergedLeavesBounded(self.UnmergedLeaves); err != nil {
		return err
	}
	if err := checkUnmergedLeavesSorted(self.UnmergedLeaves); err != nil {
		return err
	}
	w.WriteOpaque(self.EncryptionKey)
	w.WriteOpaque(self.ParentHash)
	return syntax.WriteVector(w, self.UnmergedLeaves, writeOneUnmergedLeaf)
}

// writeOneUnmergedLeaf is WriteVector's element encoder, named rather than written as a closure
// for the reason extension.go's writeOneExtension is: the codec entry gate in
// crypto_labels_test.go pins every syntax call this package makes by its source text, and a
// closure carries its whole body into that pin.
func writeOneUnmergedLeaf(w *syntax.Writer, leaf LeafIndex) error {
	w.WriteUint32(uint32(leaf))
	return nil
}

// UnmarshalMLS decodes the parent node, STAGED and assigned whole for LeafNode.UnmarshalMLS's
// reason: the value this answers is a function of the bytes it read and of nothing the receiver
// arrived holding, so a refused decode -- a truncation, or an unmerged vector that is not
// strictly ascending -- leaves the receiver exactly as it found it rather than half written.
func (self *ParentNode) UnmarshalMLS(r *syntax.Reader) error {
	encryptionKey, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	parentHash, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	unmerged, err := syntax.ReadVector(r, readOneUnmergedLeaf)
	if err != nil {
		return err
	}
	if err := checkUnmergedLeavesBounded(unmerged); err != nil {
		return err
	}
	if err := checkUnmergedLeavesSorted(unmerged); err != nil {
		return err
	}
	self.EncryptionKey = HpkePublicKey(encryptionKey)
	self.ParentHash = parentHash
	self.UnmergedLeaves = unmerged
	return nil
}

// readOneUnmergedLeaf is ReadVector's element decoder, named for the reason its encode twin is.
func readOneUnmergedLeaf(r *syntax.Reader) (LeafIndex, error) {
	v, err := r.ReadUint32()
	return LeafIndex(v), err
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*ParentNode)(nil)

// Clone answers a parent node that shares no storage with this one, for LeafNode.Clone's
// reason: a commit computed against one epoch's tree and later rejected must leave that tree
// exactly as it found it, and a shared backing array is how a rejected commit mutates a tree
// that never accepted it.
func (self *ParentNode) Clone() *ParentNode {
	return &ParentNode{
		EncryptionKey:  HpkePublicKey(cloneBytes(self.EncryptionKey)),
		ParentHash:     cloneBytes(self.ParentHash),
		UnmergedLeaves: cloneSlice(self.UnmergedLeaves),
	}
}

// Node is one OCCUPIED position in the tree. Exactly one of Leaf and Parent is set, and which
// one is what NodeType says.
//
// A Node value is never how a blank is spelled. A blank is the absence of a Node, which in the
// array below is a nil entry and on the wire is a zero presence octet; see the file header.
type Node struct {
	NodeType NodeType
	Leaf     *LeafNode
	Parent   *ParentNode
}

func (self *Node) Clone() *Node {
	out := &Node{NodeType: self.NodeType}
	if self.Leaf != nil {
		out.Leaf = self.Leaf.Clone()
	}
	if self.Parent != nil {
		out.Parent = self.Parent.Clone()
	}
	return out
}

// OptionalNode is one element of the ratchet_tree vector: optional<Node>.
//
// It is a named type rather than a bare *Node so that the validation plan's codec table and
// fuzz corpus have a decodable unit for a single position, and so that PRESENCE is a field of
// its own rather than something derived from whether the node looks filled in. Derived
// presence is the conflation this file exists to prevent: an absent node and a present node
// that happens to be zero valued are different bytes, different tree hashes, and a fork.
type OptionalNode struct {
	Present bool
	Node    Node
}

// RatchetTree is the tree in RFC 9420 section 4.2 array order. A nil entry is a BLANK node.
//
// The leaf width is always a power of two, so the array is always a complete tree: RFC 9420
// section 7.7 grows and shrinks the tree by doubling and halving, never by one leaf. NOT safe
// for concurrent use.
type RatchetTree struct {
	nodes []*Node
}

// NewRatchetTree answers the one leaf tree, whose single node is blank. Not the empty tree:
// there is no such thing in the array representation, since a node width of zero is not 2n-1
// for any n, and every arithmetic in tree_math.go refuses a leaf count of zero.
func NewRatchetTree() *RatchetTree {
	return &RatchetTree{nodes: make([]*Node, 1)}
}

func (self *RatchetTree) NodeWidth() uint32 {
	return uint32(len(self.nodes))
}

// LeafWidth is a COUNT and not an index, which is why it answers LeafCount: it feeds
// NodeWidth, DirectPath, Root and Resolution, and every one of those takes a count.
func (self *RatchetTree) LeafWidth() LeafCount {
	return LeafCount((self.NodeWidth() + 1) / 2)
}

// growTo grows to at least the given leaf count, by doubling.
//
// Existing node indices are UNCHANGED by a doubling, because RFC 9420 section 7.7 adds a new
// root whose left subtree is the whole existing tree and whose right subtree is blank, and in
// array order that appends. ExtendedLeafCount is the doubling, and it is what refuses to pass
// MaxLeafCount rather than a bound re-derived here.
func (self *RatchetTree) growTo(target LeafCount) error {
	width := self.LeafWidth()
	for width < target {
		extended, err := ExtendedLeafCount(width)
		if err != nil {
			return err
		}
		width = extended
	}
	if width == self.LeafWidth() {
		return nil
	}
	grown := make([]*Node, NodeWidth(width))
	copy(grown, self.nodes)
	self.nodes = grown
	return nil
}

// setNode is the ONE store into the node array, and it carries the compiler directive for the
// reason secret_zeroize.go's file comment gives.
//
// Consolidating the store is not tidiness. The class
// TestEveryEraseHelperCarriesTheNoInlineDirective derives is "writes through storage that
// outlives the call", and a method of this container that indexes into its own node array is
// in that class whatever it stores. The blanking store is exactly the shape the argument is
// about: Blank is how a removed member's leaf, and the key material and credential it
// carries, stops being reachable from this tree, and a compiler is entitled to delete a store
// it can prove is never read again. Three stores would be three declarations each needing the
// directive and each able to lose it independently; one store is one place that can be wrong.
//
//go:noinline
func (self *RatchetTree) setNode(x NodeIndex, node *Node) {
	self.nodes[x] = node
}

// Get answers the node at x, or nil for a blank position and for an index outside the tree.
//
// One nil for both, deliberately: every caller of this is asking "is there a node here to read"
// and an index past the end answers that question the same way a blank does. The callers that
// need the two separated -- SetParent and Blank -- range check for themselves and answer
// ErrNodeIndexOutOfRange.
func (self *RatchetTree) Get(x NodeIndex) *Node {
	if uint32(x) >= self.NodeWidth() {
		return nil
	}
	return self.nodes[x]
}

// Leaf answers the leaf at i, or nil if that position is blank or outside the tree.
//
// The leaf index is range checked HERE and not left to Get's node index check, because
// LeafIndex.NodeIndex is total and wraps: leaf 2^31 answers node 0, so without this check
// Leaf(2^31) would hand back leaf 0's contents as though they were its own.
func (self *RatchetTree) Leaf(i LeafIndex) *LeafNode {
	if LeafCount(i) >= self.LeafWidth() {
		return nil
	}
	node := self.Get(i.NodeIndex())
	if node == nil {
		return nil
	}
	return node.Leaf
}

// ParentAt answers the parent node at x, or nil if that position is blank, outside the tree, or
// holds a leaf.
func (self *RatchetTree) ParentAt(x NodeIndex) *ParentNode {
	node := self.Get(x)
	if node == nil {
		return nil
	}
	return node.Parent
}

// SetLeaf installs a leaf, growing the tree if the index is past its current width.
//
// A nil leaf is REFUSED rather than stored. Storing it would make the position occupied and
// empty at once -- IsBlank would answer false, Leaf would answer nil -- which is exactly the
// blank as zero valued node conflation this file exists to prevent, and the encoder would then
// have a present node with nothing to write. Blank is how a position is emptied.
//
// What is stored is a COPY, so the tree owns every node in it and the caller keeps whatever it
// handed in. That is the same statement Clone makes, made at the other door: Clone's comment says
// nothing may alias between two epochs' trees, and an install that adopted the caller's pointer
// would let tree.SetLeaf(i, other.Leaf(j)) put one *LeafNode in two of them -- after which a
// commit computed against one epoch and later rejected has already written through the other. A
// caller that means to keep editing the node reads it back through Leaf, which is the accessor
// that hands out the tree's own pointer on purpose.
func (self *RatchetTree) SetLeaf(i LeafIndex, leaf *LeafNode) error {
	if leaf == nil {
		return ErrTreeMalformed
	}
	if LeafCount(i) >= MaxLeafCount {
		return ErrLeafIndexOutOfRange
	}
	if err := self.growTo(LeafCount(i) + 1); err != nil {
		return err
	}
	self.setNode(i.NodeIndex(), &Node{NodeType: NodeTypeLeaf, Leaf: leaf.Clone()})
	return nil
}

// SetParent installs a parent node at an odd index inside the current tree.
//
// It does NOT grow. A parent node above a leaf that is not in the tree is not a position the
// tree has, and growing on its behalf would invent a subtree nobody added a member to. A nil
// parent is refused for SetLeaf's reason, and the payload is copied for SetLeaf's reason.
//
// An unmerged leaf outside the tree is refused HERE, and that is the whole of what keeps
// Resolution's dropped error sound. RFC 9420 section 7.5 refuses a node whose unmerged list
// leaves the tree, and RatchetTree.Resolution answers the EMPTY list for every refusal --
// which is the same answer an accepted blank subtree gives. A resolution is the list a path
// secret is sealed to, so one out-of-range unmerged leaf turns "seal to everyone under this
// node" into "seal to nobody" at that node, at the ROOT as readily as at a leaf's parent, and
// no shape assertion, round trip, member count or tree hash of this container can see it. The
// codec cannot make this refusal on its own account -- checkUnmergedLeavesSorted and
// checkUnmergedLeavesBounded see one parent node and not the tree it is going into, so neither
// knows the width -- so the container is the only layer that holds the width and the list at
// the same time. Only the RANGE half of section 7.9 is decided here; the subtree half and the
// non-blank half need x's own subtree and the rest of the tree, and belong to whole tree
// validation.
func (self *RatchetTree) SetParent(x NodeIndex, parent *ParentNode) error {
	if x.IsLeaf() {
		return ErrNodeTypeMismatch
	}
	if parent == nil {
		return ErrTreeMalformed
	}
	if uint32(x) >= self.NodeWidth() {
		return ErrNodeIndexOutOfRange
	}
	for _, leaf := range parent.UnmergedLeaves {
		if LeafCount(leaf) >= self.LeafWidth() {
			return ErrLeafIndexOutOfRange
		}
	}
	self.setNode(x, &Node{NodeType: NodeTypeParent, Parent: parent.Clone()})
	return nil
}

// Blank empties a position. The entry becomes ABSENT -- a nil, which is what the encoder writes
// a zero presence octet for -- and never a zero valued Node.
func (self *RatchetTree) Blank(x NodeIndex) error {
	if uint32(x) >= self.NodeWidth() {
		return ErrNodeIndexOutOfRange
	}
	self.setNode(x, nil)
	return nil
}

// BlankDirectPath blanks every parent node between the leaf and the root.
//
// The leaf ITSELF is left alone, because Update and Remove differ only in what they put back
// there: an update installs a new leaf, a remove blanks it, and both blank the same path above.
func (self *RatchetTree) BlankDirectPath(i LeafIndex) error {
	if LeafCount(i) >= self.LeafWidth() {
		return ErrLeafIndexOutOfRange
	}
	path, err := directPathOf(i.NodeIndex(), self.LeafWidth())
	if err != nil {
		return err
	}
	for _, x := range path {
		if err := self.Blank(x); err != nil {
			return err
		}
	}
	return nil
}

// Clone answers a tree that shares no storage with this one, at every depth.
//
// A blank stays a blank: a nil entry clones to a nil entry rather than to an occupied position
// holding a zero valued node, which is the same distinction the wire format draws and the one a
// deep copy is likeliest to lose.
func (self *RatchetTree) Clone() *RatchetTree {
	out := &RatchetTree{nodes: make([]*Node, len(self.nodes))}
	for i, node := range self.nodes {
		if node != nil {
			out.nodes[i] = node.Clone()
		}
	}
	return out
}

// NonBlankLeaves is every occupied leaf slot, ascending.
func (self *RatchetTree) NonBlankLeaves() []LeafIndex {
	out := []LeafIndex{}
	for i := uint32(0); i < uint32(self.LeafWidth()); i += 1 {
		if self.Leaf(LeafIndex(i)) != nil {
			out = append(out, LeafIndex(i))
		}
	}
	return out
}

// Members is NonBlankLeaves under the name the group lifecycle plan uses for it. One
// implementation and not two: a membership list and an occupied leaf list that could ever
// disagree is a group whose roster depends on which question was asked.
func (self *RatchetTree) Members() []LeafIndex {
	return self.NonBlankLeaves()
}

func (self *RatchetTree) MemberCount() uint32 {
	return uint32(len(self.NonBlankLeaves()))
}

// FindLeafBySignatureKey answers the leaf holding this signature key.
//
// The comparison is constant time even though a signature public key is not a secret, because
// what is being protected is not the key but the ANSWER: this runs over every member of a group
// on a path a peer can trigger, and a comparison that returned early would leak how far a
// probed key matched a member's.
//
// The LOWEST matching index wins, and that is a decision rather than a consequence of the loop.
// A tree is not guaranteed free of duplicate signature keys when this runs: it is how a member
// locates its own leaf in a tree a peer supplied, which happens before ValSem101's duplicate
// signature key refusal has necessarily run over that tree. Two members answering different
// positions for one key would each sign and decrypt as a different leaf, so the tie break is
// stated here and pinned by TestFindLeafBySignatureKeyAnswersTheLowestMatchingLeaf rather than
// left to whichever direction a later rewrite happens to scan in.
func (self *RatchetTree) FindLeafBySignatureKey(key SignaturePublicKey) (LeafIndex, bool) {
	for i := uint32(0); i < uint32(self.LeafWidth()); i += 1 {
		leaf := self.Leaf(LeafIndex(i))
		if leaf == nil {
			continue
		}
		if subtle.ConstantTimeCompare(leaf.SignatureKey, key) == 1 {
			return LeafIndex(i), true
		}
	}
	return 0, false
}

// EncryptionKeyInUse asks whether this HPKE public key is already at some node, leaf or parent.
// The group lifecycle plan's ValSem103 and ValSem110 ask exactly this, once per proposal.
//
// Both kinds of node are read, and that is the whole of it: a duplicate key check that looked
// only at leaves would accept an add whose key is already a parent's, which is a member that
// can decrypt a path secret it was never sent.
func (self *RatchetTree) EncryptionKeyInUse(key HpkePublicKey) bool {
	for x := uint32(0); x < self.NodeWidth(); x += 1 {
		node := self.nodes[x]
		if node == nil {
			continue
		}
		if node.Leaf != nil && subtle.ConstantTimeCompare(node.Leaf.EncryptionKey, key) == 1 {
			return true
		}
		if node.Parent != nil && subtle.ConstantTimeCompare(node.Parent.EncryptionKey, key) == 1 {
			return true
		}
	}
	return false
}

// HasTrailingBlankNodes reports whether the array ends in a blank, which RFC 9420 section
// 12.4.3.3 forbids of an exported ratchet_tree and which ValSem300 refuses.
//
// The LAST node and not a scan, because the node width of any tree is odd and the last index of
// an odd width array is even: the final position is always a leaf, so "the array ends in a
// blank" and "the rightmost leaf is blank" are one question. It is a read of the node array, so
// it lives here rather than being re-derived from the encoding in two other plans.
func (self *RatchetTree) HasTrailingBlankNodes() bool {
	return len(self.nodes) > 0 && self.nodes[len(self.nodes)-1] == nil
}

// ---- NodeShape, so the tree math plan's Resolution and FilteredDirectPath run against a real
// tree. This is what lets that plan hold no resolution algorithm of its own.

func (self *RatchetTree) LeafCount() LeafCount {
	return self.LeafWidth()
}

// IsBlank is the absence test, and it is the same nil the encoder writes a zero presence octet
// for. An index outside the tree reports blank, matching Get.
func (self *RatchetTree) IsBlank(x NodeIndex) bool {
	return self.Get(x) == nil
}

// UnmergedLeaves answers a COPY of the node's stored list, in stored order.
//
// Stored order and not sorted order: tree_math.go's NodeShape contract says so, and the reason
// is that a repair here would hide the tree that needs rejecting. Sortedness is refused at the
// codec boundary, where the bytes a tree hash is taken over are decided, and checked again by
// whole tree validation; it is not something a resolution walk silently fixes up.
//
// A copy and not the live slice, which is the half a reader has to be told because it costs an
// allocation. This is the NodeShape method the tree math walks, so its answer reaches code that
// has no idea it is holding a tree's own storage, and the ordinary way to narrow a list in go --
// kept := answer[:0] followed by append -- writes through the backing array. Measured on this
// tree that rewrote a real parent node's unmerged list in place, which is a different parent
// hash at that node and a fork at the next tree hash. The tree math only ranges over the answer
// today; a copy is what keeps the day it does something else from being a silent one. The empty
// case allocates nothing, since cloneSlice answers nil for nil.
func (self *RatchetTree) UnmergedLeaves(x NodeIndex) []LeafIndex {
	parent := self.ParentAt(x)
	if parent == nil {
		return nil
	}
	return cloneSlice(parent.UnmergedLeaves)
}

var _ NodeShape = (*RatchetTree)(nil)

// Resolution is RFC 9420 section 7.5, delegated to the tree math plan against this tree's own
// NodeShape. A non-blank node resolves to itself followed by its unmerged leaves, a blank leaf
// to nothing at all, and a blank parent to its left child's resolution followed by its right
// child's.
//
// This is a convenience and not a second algorithm. There is exactly one resolution walk in this
// implementation and it lives in tree_math.go, because the answer decides WHO a path secret is
// encrypted to: a second walk that agreed with the first about most trees would put a member who
// was removed back into the list a secret is sealed to, which no round trip and no tree hash
// notices.
//
// The error is dropped, and dropping it is a decision about the CALLERS rather than about the
// error. What comes back for a refused x is the EMPTY list rather than a nil, which is the same
// shape an accepted empty resolution has, so the two are deliberately not distinguishable here:
// a caller that needs to tell them apart is a caller that has not bounded its index, and it must
// call the free Resolution(self, x) and answer the error. Task 16's EncryptionTargets and
// FilteredDirectPath are exactly those callers.
//
// That is only sound while the one refusal a tree built through this container can produce is an
// x outside the node width, which every call site of this method has already bounded because x
// came out of a direct path, a copath or a root of this same tree. The other two refusals are
// held unreachable by this container rather than assumed unreachable, which is the correction a
// review made here: the earlier text asserted that a RatchetTree "always has a leaf count in
// range and an unmerged leaf inside it" and only the first half was enforced anywhere.
//
//   - the leaf count. The array is only ever built by NewRatchetTree and grown by growTo, and
//     growTo doubles through ExtendedLeafCount, so the width is a power of two inside
//     MaxLeafCount and checkLeafCount cannot refuse it.
//   - the unmerged leaves. SetParent refuses a list holding a leaf the tree does not have, and
//     the width only ever grows, so a list in range when it was stored stays in range. Without
//     that refusal this method answered the empty list for a NON-BLANK node, root included,
//     which reads as "seal this path secret to nobody" -- see SetParent's own comment.
//
// TestTheOnlyResolutionRefusalATreeThisContainerAcceptedCanProduceIsAnOutOfRangeIndex is what holds
// both, over every door into the node array rather than over the two somebody remembered.
func (self *RatchetTree) Resolution(x NodeIndex) []NodeIndex {
	out, err := Resolution(self, x)
	if err != nil {
		return []NodeIndex{}
	}
	return out
}

// ---- the ratchet_tree extension body of RFC 9420 section 12.4.3.3, and ValSem300.
//
// Three things about this codec are decisions rather than transcription, and none of the three
// is visible to a round trip.
//
// The SIZE. Every other field of this package is capped at syntax.MaxVectorLength, one
// mebibyte, and this one is not: the ratchet_tree array is a whole tree rather than one MLS
// structure, and p1 raised MaxRatchetTreeLength to sixteen mebibytes for this structure and no
// other. It is not a theoretical margin. MASTER sizes the product at 500 members with two
// devices each, which is a thousand occupied leaves in a 1024 leaf tree, and every leaf of this
// profile carries an urmessage_leaf_keys extension holding a 1216 byte X-Wing key; that tree
// encodes to about 1.33 MiB, which the default limit refuses. So both halves are wired to the
// raised bound HERE, once, rather than left to a caller to remember --
// TestTheRatchetTreeCodecIsHandedTheRaisedLimitAtTheProductsGroupSize is what holds it, and it
// fails the encode and the decode separately, because either one at the default limit refuses a
// legal group and reports it as a corrupt Welcome.
//
// The TRAILING BLANK. RFC 9420 section 12.4.3.3: "the sender MUST NOT include blank nodes after
// the last non-blank node. The receiver MUST check that the last node in ratchet_tree is
// non-blank, and then extend the tree to the right until it has a length of the form
// 2^(d+1) - 1". That is ValSem300, and it is a refusal on the decode side and a truncation on
// the encode side, so this implementation neither sends nor accepts the padded form.
//
// The EMPTY array, which is the branch a trailing blank check written as
// len(nodes) > 0 && nodes[len(nodes)-1] == nil never looks at. A vector of zero entries carries
// no non-blank last node, so it fails the RFC's requirement as surely as a padded one does, and
// reading it as the one leaf blank tree would have the decoder answer with a tree that
// HasTrailingBlankNodes reports true for -- the rule refused on the line above. It is
// ErrTreeMalformed and not the ValSem300 refusal, because a zero width node array is not 2n-1
// for any n, which is the same reason NewRatchetTree's floor is one leaf and not nothing.

// errTrailingBlankNodes is ValSem300, carried unexported on psk.go's terms and for psk.go's
// reason.
//
// The validation plan's catalogue numbers this refusal ValSem300 and owns the single
// declaration site for ErrTrailingBlankNodes. Neither that name nor ValSem itself has landed in
// this package, so the refusal is carried by the unexported value below until they do, and the
// swap is then mechanical: ValSem(ValSem300, ...) with the catalogue's sentinel as the detail.
//
// Unexported is the whole point of the shape. An exported ErrTrailingBlankNodes declared here
// would be a second public declaration site for a name the validation plan also declares, the
// two would not be the same value, and a caller matching one would silently stop matching the
// other. Every caller of this refusal is inside package mls until that plan lands -- the group
// lifecycle plan's tree adoption and the validation plan's own ValSem300 entry point are both
// package mls -- so a name that cannot be reached from outside this package costs nobody
// outside it anything.
//
// The moment the swap is owed is not left to anybody's memory:
// TestValSem300sSentinelIsStillCarriedByThisPackage fails on the commit that lands the exported
// name.
var errTrailingBlankNodes = errors.New("mls: the exported ratchet tree ends in a blank node")

// MarshalMLS writes the node union of RFC 9420 section 12.4.3.3: the type octet, then the arm
// the octet names.
//
// A Node whose type and payload disagree is ErrTreeMalformed rather than a nil write, because
// the alternative is an encoding that says "leaf" and carries nothing, which a peer decodes as
// a truncated structure and this implementation would have taken a tree hash over.
func (self *Node) MarshalMLS(w *syntax.Writer) error {
	switch self.NodeType {
	case NodeTypeLeaf:
		if self.Leaf == nil {
			return ErrTreeMalformed
		}
		w.WriteUint8(uint8(self.NodeType))
		return self.Leaf.MarshalMLS(w)
	case NodeTypeParent:
		if self.Parent == nil {
			return ErrTreeMalformed
		}
		w.WriteUint8(uint8(self.NodeType))
		return self.Parent.MarshalMLS(w)
	default:
		return ErrTreeMalformed
	}
}

// UnmarshalMLS is position-agnostic on purpose: whether this node type is legal at the index it
// was read from is the TREE decoder's check, because only the tree knows the index. What is
// decided here is that the octet names an arm at all -- NodeType has no zero valued member, so
// a Node that was never filled in cannot be mistaken for either kind.
//
// STAGED and assigned whole, for ParentNode.UnmarshalMLS's reason: the value this answers is a
// function of the bytes it read and of nothing the receiver arrived holding, so a refused
// decode leaves the receiver exactly as it found it rather than half written.
func (self *Node) UnmarshalMLS(r *syntax.Reader) error {
	nodeType, err := r.ReadUint8()
	if err != nil {
		return err
	}
	switch NodeType(nodeType) {
	case NodeTypeLeaf:
		leaf := &LeafNode{}
		if err := leaf.UnmarshalMLS(r); err != nil {
			return err
		}
		self.NodeType, self.Leaf, self.Parent = NodeTypeLeaf, leaf, nil
	case NodeTypeParent:
		parent := &ParentNode{}
		if err := parent.UnmarshalMLS(r); err != nil {
			return err
		}
		self.NodeType, self.Leaf, self.Parent = NodeTypeParent, nil, parent
	default:
		return ErrTreeMalformed
	}
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*Node)(nil)

// MarshalMLS writes optional<Node>: the presence octet, then the node when present.
//
// WriteOptional and ReadOptional own the octet, so a value that is neither 0 nor 1 is
// syntax.ErrOptionalPresence and never has to be re-spelled in this package's own error set --
// which matters more here than anywhere else in this package, because "any non-zero means
// present" would give one blank many encodings and MLS signs over serialized forms.
func (self *OptionalNode) MarshalMLS(w *syntax.Writer) error {
	return w.WriteOptional(self.Present, func(w *syntax.Writer) error {
		return self.Node.MarshalMLS(w)
	})
}

// UnmarshalMLS reads optional<Node>, staged for Node.UnmarshalMLS's reason and for one of its
// own: an absent entry must leave the receiver holding NO node rather than whatever it arrived
// with, since a reused OptionalNode would otherwise report Present false while carrying the
// previous entry's payload, and every reader of this type reaches Node only after asking
// Present.
func (self *OptionalNode) UnmarshalMLS(r *syntax.Reader) error {
	node := Node{}
	present, err := r.ReadOptional(func(r *syntax.Reader) error {
		return node.UnmarshalMLS(r)
	})
	if err != nil {
		return err
	}
	self.Present, self.Node = present, node
	return nil
}

var _ syntax.Codec = (*OptionalNode)(nil)

// writeOneOptionalNode is WriteVector's element encoder, named rather than written as a closure
// for the reason writeOneUnmergedLeaf is: the codec entry gate in crypto_labels_test.go pins
// every syntax call this package makes by its source text, and a closure carries its whole body
// into that pin.
//
// A nil entry is an ABSENT optional and never a zero valued Node, which is the distinction this
// whole file exists to keep: see the file header.
func writeOneOptionalNode(w *syntax.Writer, node *Node) error {
	optional := OptionalNode{Present: node != nil}
	if node != nil {
		optional.Node = *node
	}
	return optional.MarshalMLS(w)
}

// MarshalMLS writes the ratchet_tree extension body of RFC 9420 section 12.4.3.3:
// optional<Node> ratchet_tree<V>, with every trailing blank stripped.
//
// The truncation is the sender half of ValSem300 and it is not an optimisation: the section
// says the sender MUST NOT include blank nodes after the last non-blank one, so an encoder that
// wrote the full width would produce an array every conforming receiver refuses, this
// implementation's own decoder included.
//
// A tree with no non-blank node at all is refused rather than written as the empty array, for
// the reason the header above gives: the empty array is not a ratchet_tree any receiver
// accepts, so emitting it would make this the peer whose encode nobody -- itself included --
// can decode. The container cannot reach that state through SetLeaf, so the refusal is about
// the tree a later task builds by blanking every leaf rather than about anything reachable
// today.
func (self *RatchetTree) MarshalMLS(w *syntax.Writer) error {
	end := len(self.nodes)
	for end > 0 && self.nodes[end-1] == nil {
		end -= 1
	}
	if end == 0 {
		return ErrTreeMalformed
	}
	return syntax.WriteVector(w, self.nodes[:end], writeOneOptionalNode)
}

// positionedNode is one PRESENT entry of a ratchet_tree array together with the index it
// arrived at, and it is what the decode below collects instead of one slot per entry.
//
// The reason is an amplification that is a fact about the FORMAT rather than about this
// container: an absent entry is a single presence octet, so a body at p1's raised sixteen
// mebibyte bound declares up to 16,777,212 of them, and a decoder that appended a slot per
// entry as it read grew a 134 megabyte pointer array -- through every doubling on the way to
// it, and with a heap allocated OptionalNode per entry beside it -- before ANY of the refusals
// below had run. Measured on this package: 827 MB allocated to REFUSE an all-blank body of
// 16 MiB, 961 MB to accept a legal one, 49x and 57x the bytes that arrived. The path is
// reachable before any authentication, since a ratchet_tree extension arrives in a Welcome
// from a sender nothing has verified yet.
//
// Keeping only the present entries makes every refusal cost what the body actually CARRIED
// rather than what it claimed: an all-blank body of any length is now refused out of a count
// and an empty slice. The one array this decode builds is the accepted tree's own, allocated
// once at its final width, which is the tree the peer described and not a multiple of it.
// TestARefusedRatchetTreeBodyIsNotFirstMaterialised is what holds it.
type positionedNode struct {
	at   NodeIndex
	node *Node
}

// UnmarshalMLS reads the ratchet_tree extension body, refuses ValSem300's trailing blank and a
// node whose type contradicts its position, and extends the array to the next complete tree.
//
// The region is taken with ReadNested rather than with ReadSub and a Done of this decoder's
// own. Both run the region to empty; what differs is where the obligation lives, and what
// happens to the CALLER's Reader when the tree is refused.
//
// ReadSub hands the completion obligation back, so a Done spelled here is a line no test in
// this package can observe -- the loop below ends only when the region is empty, and every
// element decoder propagates the reader's latched error, so it can answer nothing but nil, and
// it duly survived being deleted. ReadNested discharges the same obligation inside syntax,
// where a sub reader CAN be left short because decodeOne is arbitrary there, and where syntax's
// own tests exercise exactly that.
//
// The half with teeth here is the LATCH. ReadSub advances the caller's Reader past the whole
// region before the first entry is read, so with the hand rolled form a refused tree left that
// Reader clean, positioned at the next field and reporting nil from Done -- and Done is exactly
// the check a caller makes instead of checking every return. ReadNested latches the refusal
// onto it, which is why every refusal of this body is decided inside readNodeArray rather than
// after ReadNested has returned. TestARefusedRatchetTreeLatchesTheReaderItWasReadFrom is what
// holds it.
func (self *RatchetTree) UnmarshalMLS(r *syntax.Reader) error {
	return r.ReadNested(self.readNodeArray)
}

// readNodeArray is ReadNested's decoder for the optional<Node> array, named rather than written
// as a closure for the reason writeOneUnmergedLeaf is, and holding every refusal of the body so
// that ReadNested latches all of them and not only the ones a read raised.
//
// Staged into locals and assigned whole at the end for the reason every other decoder in this
// file is: a refused tree leaves the receiver exactly as it found it, which for a container a
// caller may be decoding INTO over a live epoch's tree is the difference between a rejected
// Welcome and a half replaced group.
//
// The array is walked here rather than through syntax.ReadVector because the element decoder
// needs the node INDEX to check type against position and ReadVector's element callback does
// not carry one. That is the sanctioned form for a heterogeneous vector.
//
// Two of the rules below are also stated by tree_sync.go's validateStructure, and the two doors
// are NOT the same door. Both invert the width through LeafCountFromNodeWidth and IsFullLeafCount
// and both refuse a leaf typed node at an odd index, through the same shared helpers rather than
// through two derivations of the arithmetic -- so there is nothing here that can drift from the
// validator. Where they part is the half each adds, and each half is reachable only at its own
// door:
//
//   - this one adds ValSem300, the trailing blank rule, which is stated over the STRIPPED array
//     a ratchet_tree extension travels as. It has no meaning once the array has been extended to
//     its full width, which is why validateStructure drops it rather than restating it.
//   - validateStructure adds the body half -- exactly one of Leaf and Parent populated, matching
//     the declared type -- which cannot fire here because this decoder populated the body the
//     type selected. It fires for a tree that reached the package any other way: through the
//     setters, through Clone, or out of a connect/message snapshot record.
//
// So a decoded tree has been past both and every other tree has been past only the validator,
// and neither door is the one the other can be deleted in favour of.
func (self *RatchetTree) readNodeArray(body *syntax.Reader) error {
	present := []positionedNode{}
	entries := 0
	// ONE OptionalNode for the whole array rather than one per entry. Its own UnmarshalMLS is
	// staged and assigns both fields whole, so a reused receiver carries nothing forward from
	// the entry before it -- that method's comment states the property as its own, which is
	// what makes the reuse a decision here rather than a shortcut that depends on how it
	// happens to be written today.
	optional := OptionalNode{}
	for !body.Empty() {
		x := NodeIndex(entries)
		if err := optional.UnmarshalMLS(body); err != nil {
			return err
		}
		entries += 1
		if !optional.Present {
			continue
		}
		node := optional.Node
		switch node.NodeType {
		case NodeTypeLeaf:
			// RFC 9420 section 12.4.3.3: "The leaves of the tree are stored in
			// even-numbered entries in the array". A LeafNode at an odd index is refused
			// and never coerced, because coercing would let a sender decide which of two
			// structures every later tree hash and parent hash is computed over.
			if !x.IsLeaf() {
				return ErrNodeTypeMismatch
			}
		case NodeTypeParent:
			if x.IsLeaf() {
				return ErrNodeTypeMismatch
			}
		default:
			return ErrTreeMalformed
		}
		present = append(present, positionedNode{at: x, node: &node})
	}
	// the empty array, which is NOT the one leaf tree -- see the header above. Checked before
	// the trailing blank rather than folded into it, because a guard of the form
	// entries > 0 && ... reads as the same rule and skips it for exactly this input.
	if entries == 0 {
		return ErrTreeMalformed
	}
	// ValSem300: "The receiver MUST check that the last node in ratchet_tree is non-blank".
	// Asked of the last entry that CARRIED a node rather than of a materialised array, which
	// is the same question -- an array ends in a blank exactly when its last index is not the
	// index of its last present entry -- answered without having built one. An array with no
	// present entry anywhere ends in a blank by that same reading.
	if len(present) == 0 || uint32(present[len(present)-1].at) != uint32(entries-1) {
		return errTrailingBlankNodes
	}
	// "and then extend the tree to the right until it has a length of the form 2^(d+1) - 1,
	// adding the minimum number of blank values possible". ExtendedLeafCount is this package's
	// doubling and it is what refuses to run past MaxLeafCount on a hostile length, so the bound
	// is not re-derived here.
	leafWidth := LeafCount(1)
	for NodeWidth(leafWidth) < uint32(entries) {
		extended, err := ExtendedLeafCount(leafWidth)
		if err != nil {
			return err
		}
		leafWidth = extended
	}
	width := NodeWidth(leafWidth)
	// an extension is only an extension while it is WIDER than what arrived, and that is
	// asserted rather than assumed. The loop above exits with NodeWidth(leafWidth) >= entries
	// so it cannot fire today; what it replaces is a make-and-copy, and copy's answer to a
	// destination shorter than its source is to drop the tail SILENTLY -- after which
	// LeafCountFromNodeWidth and IsFullLeafCount both pass on the truncated array and the tree
	// is accepted short. A refusal costs one comparison and has no such quiet direction.
	if width < uint32(entries) {
		return ErrTreeMalformed
	}
	// the ONE array this decode builds: the accepted tree's own, at its final width and
	// allocated once. The present entries are scattered into it by index rather than copied in
	// as a block, which is what lets a blank cost nothing at all before this line.
	padded := make([]*Node, width)
	for _, one := range present {
		padded[one.at] = one.node
	}
	// the shape, DERIVED from the arithmetic rather than trusted from the loop above. A node
	// array is a tree only if its width is 2n-1 for a leaf count that is a power of two, and
	// both halves are asked because neither implies the other: a decoder that skipped the
	// extension entirely answers 2n-1 for the truncated array of a three member group -- five
	// nodes, three leaves -- and every round trip of it is byte exact, every member count
	// correct, and every direct path, copath and tree hash computed against a root that is not
	// where the group's is. TestEveryDecodedRatchetTreeIsACompleteTree is what holds it.
	shape, err := LeafCountFromNodeWidth(uint32(len(padded)))
	if err != nil {
		return err
	}
	if !IsFullLeafCount(shape) {
		return ErrTreeMalformed
	}
	self.nodes = padded
	return nil
}

var _ syntax.Codec = (*RatchetTree)(nil)

// marshalRatchetTree encodes the tree at the sixteen mebibyte ratchet_tree bound rather than at
// the one mebibyte default every other field of this package gets.
//
// It exists because the decode side alone is not enough. syntax.Marshal caps the vector at
// MaxVectorLength, and the tree this product is sized for exceeds that: a thousand leaves each
// carrying a 1216 byte X-Wing key encode to about 1.33 MiB, so syntax.Marshal(tree) refuses a
// legal group at syntax.ErrLengthExceedsMax and reports it as though the tree were malformed.
// The interface registry resolves the group lifecycle plan's retired Encode() to
// syntax.Marshal, and that resolution is right only for a group smaller than this product's
// own sizing.
//
// UNEXPORTED, which is the half worth reading twice, because the obvious shape is the exported
// twin of UnmarshalRatchetTree. A ratchet tree is an extension BODY, and
// TestNoExportedSymbolOfThisPackageHandsOutAnExtensionBodyOnItsOwn says this package does not
// hand one out loose: a byte run leaving here is a tag choice handed back to the caller, and
// 0x0005 rather than 0x0002 encodes, is covered by the GroupInfo signature, and travels. Encode
// is the way out, tag and body together. A caller inside this package that wants the bytes at
// the raised bound for something that is NOT an extension -- local group state, say -- calls
// this; one outside it has no business with a loose body.
//
// RatchetTree still implements syntax.Codec, so it stays a CheckRoundTrip target and an entry
// in the validation plan's codec table, and the plain MarshalMLS simply carries whatever limit
// the caller's Writer was opened at.
func marshalRatchetTree(tree *RatchetTree) ([]byte, error) {
	return syntax.MarshalLimit(tree, syntax.MaxRatchetTreeLength)
}

// UnmarshalRatchetTree decodes a ratchet_tree extension body at the sixteen mebibyte bound.
//
// This and marshalRatchetTree are the only two places in this package that raise the vector
// limit. Decoding through plain syntax.Unmarshal would refuse a large group at
// syntax.ErrLengthExceedsMax, which reads as a corrupt Welcome rather than as a limit, so the
// raised limit is wired here once and no caller has to remember it. syntax.UnmarshalLimit also
// enforces full consumption, so there is no separate trailing-bytes check: a body with anything
// after the vector is syntax.ErrTrailingBytes.
func UnmarshalRatchetTree(data []byte) (*RatchetTree, error) {
	tree := &RatchetTree{}
	if err := syntax.UnmarshalLimit(data, tree, syntax.MaxRatchetTreeLength); err != nil {
		return nil, err
	}
	return tree, nil
}

// Encode answers the whole extensions<V> entry, tagged ratchet_tree, rather than a body a
// caller then has to tag.
//
// It is spelled Encode() (Extension, error) and NOT Encode() ([]byte, error), which is the
// signature the interface registry retired on this type in favour of syntax.Marshal --
// slice1-interface-registry's dead name table, "Encode (p7) -> syntax.Marshal". Re-animating a
// retired name is a decision and it is recorded here rather than left to be rediscovered: the
// reconciliation owed to those plans is that syntax.Marshal is the resolution for a group
// SMALLER than this product's own sizing and refuses this one, that p8's text still instructs
// "encoding is syntax.Marshal(tree)" and must become syntax.MarshalLimit or this method, and
// that ParseRatchetTreeFrom and marshalRatchetTree are surfaces neither the registry nor p5's
// own Produces list names. The different SIGNATURE is what makes this the loud direction: a
// caller written against the retired spelling gets a compile error rather than bytes refused
// at the wrong limit.
//
// The two are not the same method: a []byte answer hands the tag choice back to the caller, and
// extension_test.go's extensionBodyTypesIn derives the sanctioned extension body class off
// exactly this signature, so this spelling is what makes ParseRatchetTreeFrom a sanctioned
// read side rather than a second codec. A caller that wanted the retired method wants
// marshalRatchetTree.
//
// This is the Encode/Parse pair extension.go describes for urmessage_leaf_keys, and it is here
// for the same reason: Extension.ExtensionData is opaque, so nothing in the type system stops a
// call site pairing this body with some other extension's code point, and an extensions vector
// carrying a ratchet_tree body under the external_senders tag is a structure that encodes,
// signs and travels. The guarantee is that no call site can build the pairing wrong, and the
// only spelling that holds it is one where the tag and the body are produced together --
// TestEveryRatchetTreeExtensionThisPackageBuildsCarriesItsOwnTag is the half with teeth, and
// ParseRatchetTreeFrom is the read side that refuses the pairing this side cannot produce.
//
// The body is encoded at the raised bound, so a group at this product's sizing has an extension
// to put in a GroupInfo. Whether the GroupInfo ENCODE around it is also running at that bound
// is the caller's decision and not one this call can make: Extension.MarshalMLS writes
// ExtensionData through the caller's Writer, which caps it at whatever limit that Writer was
// opened with.
func (self *RatchetTree) Encode() (Extension, error) {
	data, err := marshalRatchetTree(self)
	if err != nil {
		return Extension{}, err
	}
	return Extension{
		ExtensionType: ExtensionTypeRatchetTree,
		ExtensionData: data,
	}, nil
}

// ParseRatchetTreeFrom decodes one extensions<V> entry as a ratchet_tree body, refusing any
// entry that is not tagged ExtensionTypeRatchetTree.
//
// This is the half of the pair that has a tag to check, and it is the one a caller holding an
// entry out of an extensions vector wants. UnmarshalRatchetTree is the half with no tag -- it
// parses whatever it is handed -- and a caller who reaches for it with an Extension in hand,
// pulling .ExtensionData out to pass it, has taken the guarantee off: it will decode an
// external_senders body as a tree if the bytes happen to fit.
//
// Only the TAG answers ErrRatchetTreeExtensionTag. Every refusal of the body itself keeps its
// own sentinel -- errTrailingBlankNodes for ValSem300, ErrNodeTypeMismatch, ErrTreeMalformed,
// syntax.ErrLengthExceedsMax -- which is the deliberate difference from
// ErrLeafKeysExtensionInvalid, where one sentinel covers both. The leaf keys body had no
// refusals of its own to tell apart; this one has four, a caller repairing each does something
// different, and collapsing them into one name would hand a wrong-tag caller a tree's repair.
func ParseRatchetTreeFrom(ext Extension) (*RatchetTree, error) {
	if ext.ExtensionType != ExtensionTypeRatchetTree {
		return nil, fmt.Errorf("%w: extension type 0x%04x",
			ErrRatchetTreeExtensionTag, uint16(ext.ExtensionType))
	}
	return UnmarshalRatchetTree(ext.ExtensionData)
}

// ---- RFC 9420 section 7.7, the three membership changes a commit applies to the tree.
//
// Add, Update and Remove are the only operations that change WHO is in the tree, and section
// 12.3 applies them in the order GroupContextExtensions, Update, Remove, Add. That order belongs
// to the caller, but it is why Add's rule below is written against the tree as it stands AFTER
// the removes: the leftmost blank leaf Add fills may be one a Remove in the same commit made.
//
// What separates the three is what happens to the DIRECT PATH, and that difference is TreeKEM's
// forward secrecy story rather than bookkeeping:
//
//   - Update and Remove replace or destroy the key at a leaf, so every node above that leaf is
//     holding a key its old occupant could derive. Those nodes are blanked.
//   - Add introduces a member who has never held any of them, so those nodes STAY: the existing
//     members keep their path secrets and the group does not re-key on every join. The price is
//     that the new member cannot yet decrypt to them, and unmerged_leaves is how the tree records
//     the debt -- section 4.2 puts every unmerged leaf of a node into that node's resolution, so
//     the next sender seals to the new leaf separately until a commit through the node merges it.
//
// So Add NEVER blanks. An implementation that blanked the new leaf's direct path would be secure,
// would be interoperable with nobody, and would be indistinguishable from this one by every test
// that does not compute a parent hash.

// AddLeaf places a new member at the leftmost blank leaf, growing the tree when there is none,
// and records the new leaf as unmerged on every non-blank node above it. It answers the leaf the
// member was placed at.
//
// LEFTMOST and not "the next index past the last member", because the two differ exactly when
// somebody has been removed: section 7.7 refills the gaps a Remove left before it widens the
// tree, and a version that appended instead would grow without bound over a group that churns
// while never exceeding its size. The scan reads the node array rather than a remembered free
// list, for the reason this container has no free list at all -- a second answer to "which leaves
// are occupied" is a second thing that can disagree with the leaves.
//
// The unmerged marking is a LOOP over the whole path and each of its clauses earns its place. It
// skips blank nodes, because a blank node publishes no key and so owes no debt. It marks every
// LEVEL and not just the first, because section 7.9.2 condition 3 reads the resolution of one
// child of each parent against that parent's unmerged list, so a level that was missed leaves
// that parent with a resolution the tree cannot explain -- which surfaces as a parent hash
// failure at the next join and not as anything an assertion about the level that WAS marked can
// see. And it keeps the vector strictly ascending, which is the one clause an append is not.
func (self *RatchetTree) AddLeaf(leaf *LeafNode) (LeafIndex, error) {
	// one past the last leaf is the answer when nothing is blank, and SetLeaf is what turns that
	// into a doubling: the container already knows how to grow and already knows the bound, so
	// neither is re-derived here.
	target := LeafIndex(self.LeafWidth())
	for i := uint32(0); i < uint32(self.LeafWidth()); i += 1 {
		if self.Leaf(LeafIndex(i)) == nil {
			target = LeafIndex(i)
			break
		}
	}
	if err := self.SetLeaf(target, leaf); err != nil {
		return 0, err
	}
	// the width is re-read AFTER the install, because a full tree has just doubled and the new
	// leaf's direct path runs to the NEW root.
	path, err := directPathOf(target.NodeIndex(), self.LeafWidth())
	if err != nil {
		return 0, err
	}
	for _, x := range path {
		parent := self.ParentAt(x)
		if parent == nil {
			continue
		}
		parent.UnmergedLeaves = insertUnmergedLeaf(parent.UnmergedLeaves, target)
	}
	return target, nil
}

// insertUnmergedLeaf puts one leaf into an unmerged_leaves vector and keeps the vector strictly
// ascending.
//
// An append is what the operation reads like, and it is not enough. Section 7.9.2 requires the
// vector ascending, this package's own encoder refuses anything else, and the requirement is not
// cosmetic: the vector is hashed into the parent hash, so two orderings of one set are two parent
// hashes over one tree. The arrangement that makes an append wrong -- a blank leaf sitting to the
// LEFT of a leaf already listed -- is the arrangement Add looks for first, so it is not a corner
// case of this operation but its main line.
//
// A leaf already listed is left alone rather than listed twice, because condition 3 matches the
// list against a resolution and a leaf appears in a resolution once.
//
// The result is a fresh slice rather than an insert through the backing array, for the reason
// RatchetTree.UnmergedLeaves gives at the other door: this list is a tree's own storage, and a
// caller that read it through ParentAt is holding the same array.
func insertUnmergedLeaf(leaves []LeafIndex, leaf LeafIndex) []LeafIndex {
	at := 0
	for at < len(leaves) && leaves[at] < leaf {
		at += 1
	}
	if at < len(leaves) && leaves[at] == leaf {
		return leaves
	}
	out := make([]LeafIndex, 0, len(leaves)+1)
	out = append(out, leaves[:at]...)
	out = append(out, leaf)
	out = append(out, leaves[at:]...)
	return out
}

// UpdateLeaf replaces the member at i and blanks the whole path from that leaf to the root.
//
// The blanking is the point, and not the replacement. Every node above the leaf carries a key the
// OLD leaf's owner could derive, so leaving any one of them standing lets a member who has just
// updated away keep reading the group -- which is the whole of what an Update proposal is for. It
// is a loop over every level for that reason: the topmost node is as reachable from a stale path
// secret as the bottom one.
//
// A BLANK leaf is refused rather than filled in, because installing a member where there is no
// member is Add's job and Add is the operation that leaves the path alone.
func (self *RatchetTree) UpdateLeaf(i LeafIndex, leaf *LeafNode) error {
	// two clauses where one would do -- Leaf answers nil past the width as readily as at a blank
	// -- because the width clause is what the sentinel is named after, and a reader should not
	// have to derive it from an accessor's nil.
	if LeafCount(i) >= self.LeafWidth() || self.Leaf(i) == nil {
		return ErrLeafIndexOutOfRange
	}
	if err := self.SetLeaf(i, leaf); err != nil {
		return err
	}
	return self.BlankDirectPath(i)
}

// RemoveLeaf blanks the departing member's leaf and the whole path above it, then shrinks the
// tree if the members that are left fit in a narrower one.
//
// Three clauses, each with its own failure mode. The path blanking is Update's, for Update's
// reason, and it carries a second job that section 12.4.3.2 never has to state: every node whose
// unmerged_leaves could name the departing leaf is by definition ABOVE that leaf, so blanking the
// path is also what takes the departing member out of every unmerged list a well formed tree has.
// The truncation is section 12.1.3, and a tree that never truncates is still CORRECT -- every
// hash, path and resolution over it agrees with itself -- while growing without bound over a
// group that churns. The sweep at the end is what keeps this container's own invariant true
// across the shrink.
func (self *RatchetTree) RemoveLeaf(i LeafIndex) error {
	if LeafCount(i) >= self.LeafWidth() || self.Leaf(i) == nil {
		return ErrLeafIndexOutOfRange
	}
	if err := self.BlankDirectPath(i); err != nil {
		return err
	}
	if err := self.Blank(i.NodeIndex()); err != nil {
		return err
	}
	if err := self.truncate(); err != nil {
		return err
	}
	self.dropUnmergedLeavesNamingABlankLeaf()
	return nil
}

// truncate shrinks the tree to the narrowest complete width that still holds every member.
//
// TruncatedLeafCount is that computation, and this does NOT re-derive it by halving. A halving
// loop here and the tree math next door would be two answers to one question; the tree hash
// agrees with exactly one of them, and the disagreement stays invisible until the first group
// that churns down across a power of two.
func (self *RatchetTree) truncate() error {
	leaves := self.NonBlankLeaves()
	if len(leaves) == 0 {
		// the one leaf tree, which is NewRatchetTree's floor and the narrowest shape the
		// arithmetic in tree_math.go will speak about at all. There is no empty tree.
		self.nodes = make([]*Node, 1)
		return nil
	}
	// the RIGHTMOST member and not the member COUNT. The two agree on every tree with no gaps in
	// it and part company on the first tree with one: three members at leaves 0, 1 and 5 fit a
	// count of four and need a width of eight, and a truncation to four does not report an error,
	// it drops leaf 5 out of the group.
	target, err := TruncatedLeafCount(leaves[len(leaves)-1])
	if err != nil {
		return err
	}
	if target >= self.LeafWidth() {
		return nil
	}
	// a fresh array rather than a reslice. self.nodes[:w] keeps the whole old backing array alive
	// behind a shorter length, so the departing member's LeafNode -- their credential and their
	// public keys -- stays reachable from this tree for as long as it lives, and a later append
	// through that slice hands it back.
	shrunk := make([]*Node, NodeWidth(target))
	copy(shrunk, self.nodes)
	self.nodes = shrunk
	return nil
}

// dropUnmergedLeavesNamingABlankLeaf takes every unmerged entry that names a leaf which is not
// there out of every surviving parent.
//
// The predicate is DERIVED from the tree -- "the leaf this entry names is blank" -- rather than
// written as "the leaf that was just removed", and the difference is a second condition wide. The
// removed leaf is one way an entry comes to name a blank, and on a well formed tree it is the
// only one, because every node that could list it sits on its direct path and has just been
// blanked. The other way is the SHRINK. SetParent refuses an unmerged leaf outside the tree, and
// that refusal is the whole of what makes RatchetTree.Resolution's dropped error sound, but it is
// made against the width at INSTALL time; a truncation moves the width underneath entries already
// stored, so a shrink that left them standing would leave this container holding a tree it would
// itself have refused.
//
// The ORDER is what that second condition is argued from, and the two orders are in fact the same
// sweep on this path -- which is worth saying out loud rather than leaving as a claim no test can
// hold. TruncatedLeafCount answers 1 << TreeDepth(rightmost+1), which is strictly greater than the
// rightmost occupied leaf, so a truncation never puts an OCCUPIED leaf outside the tree: every
// entry the shrink moves out of range was already naming a blank before the shrink, and the
// derived predicate was already true of it. Running AFTER the truncation is still the order to
// keep, because it is the one that measures the entries against the width the tree ends up with
// instead of the one it started with, and the coincidence is a property of today's truncation
// rather than of this sweep.
//
// It repairs nothing an interoperable peer computes differently. On any tree whose every unmerged
// entry names an occupied leaf inside its own node's subtree -- which is every tree section 7.9.2
// accepts -- the predicate is false at every entry and this changes nothing at all.
func (self *RatchetTree) dropUnmergedLeavesNamingABlankLeaf() {
	// every odd index, which is exactly the parent positions (appendix C), so the sweep is
	// derived from the tree's own width rather than from the path anybody expected to matter.
	for x := uint32(1); x < self.NodeWidth(); x += 2 {
		parent := self.ParentAt(NodeIndex(x))
		if parent == nil {
			continue
		}
		drop := false
		for _, leaf := range parent.UnmergedLeaves {
			if self.Leaf(leaf) == nil {
				drop = true
				break
			}
		}
		if !drop {
			// the stored slice is left exactly as it is rather than swapped for an equal copy,
			// because a reallocation here moves storage a caller that read this node through
			// ParentAt is holding.
			continue
		}
		kept := make([]LeafIndex, 0, len(parent.UnmergedLeaves))
		for _, leaf := range parent.UnmergedLeaves {
			if self.Leaf(leaf) != nil {
				kept = append(kept, leaf)
			}
		}
		parent.UnmergedLeaves = kept
	}
}

// ---- RFC 9420 section 7.6, the filtered direct path and who a commit's path secrets are
// sealed to.
//
// Three thin wrappers over the tree math plan's FilteredDirectPath and Resolution, and the
// thinness is the design. The filtering rule is structure and lives once, in tree_math.go,
// beside the resolution walk it is written in terms of; what this file adds is the container's
// own leaf-range guard and the exclusion of the leaves a commit adds, neither of which the
// arithmetic can see.
//
// Both directions of a wrong answer here are wrong, and they are not symmetric. A target list
// that is too SHORT locks a member out of the next epoch: it receives no ciphertext it can
// open, its next decrypt fails, and somebody reports it. A target list that is too LONG hands
// the path secret to somebody who must not have it -- a removed member whose leaf is blank, or
// a leaf this commit is adding that is not supposed to learn the path from the UpdatePath at
// all -- and NOTHING reports that. Every member still receives a ciphertext, every decrypt
// still succeeds, and the group carries on with a reader in it. So the tests below are built
// against the second direction: they derive the positions that must be absent from the tree's
// own state after a Remove and after an Add rather than naming indices, because an index
// somebody picked is an index that stops meaning what it meant the moment the fixture changes.

// filteredPathSteps is the tree math plan's []PathStep for a leaf of THIS tree, with the leaf
// range checked at this container's boundary.
//
// The step and not the node alone, because tasks 18, 20, 21 and 22 all need the copath child
// as well: the path secret of a step's node is encrypted to the resolution of that step's
// copath child, and the parent hash of that node is taken over the same child. Re-deriving the
// child from CommonAncestor at each of those four call sites would be four chances to derive
// it differently -- and a copath child derived backwards names the child the sender's own leaf
// descends from, which resolves to a set that already holds the secret, so the commit encrypts
// to the wrong half of the tree and still produces a full-length, well-formed UpdatePath.
//
// The range check is this package's own and duplicates the arithmetic's, which is the same
// choice rootOf and directPathOf make in tree_adapt.go: a function enforces its own
// precondition rather than inheriting one from whatever it happens to call, and the sentinel a
// caller of a RatchetTree gets back is the container's ErrLeafIndexOutOfRange rather than
// tree_math's ErrLeafOutOfRange. The two answer for each other through the wrap declared in
// tree_errors.go, so a caller matching either still matches.
func (self *RatchetTree) filteredPathSteps(i LeafIndex) ([]PathStep, error) {
	if LeafCount(i) >= self.LeafWidth() {
		return nil, ErrLeafIndexOutOfRange
	}
	return FilteredDirectPath(self, i)
}

// FilteredDirectPath is the direct path of leaf i, bottom-up, with every node dropped whose
// copath child resolves to nothing.
//
// A dropped node needs no key pair: encrypting to it would be encrypting to the child the
// sender's own leaf descends from, which already holds the secret, so the node stays blank and
// carries nothing. The length of this answer is the number of nodes an UpdatePath has to
// carry, which is what ValSem202 checks, and the ORDER is the order they appear in it.
//
// The order is the contract and not a detail of it. The ciphertexts of an UpdatePath are paired
// positionally with these nodes, so a permuted path seals every secret from the first
// difference upward to the wrong subtree -- and each of those members still receives a
// ciphertext of the right shape, so nothing looks wrong until a decrypt fails one task later.
// That is why the tests compare elementwise through equalNodeIndices and never as sets.
func (self *RatchetTree) FilteredDirectPath(i LeafIndex) ([]NodeIndex, error) {
	steps, err := self.filteredPathSteps(i)
	if err != nil {
		return nil, err
	}
	out := make([]NodeIndex, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Node)
	}
	return out, nil
}

// EncryptionTargets is one resolution per filtered-direct-path node, in the same order, each
// already stripped of the leaves this commit adds.
//
// RFC 9420 section 7.6: a member added by the same commit receives the path secret in its
// Welcome and never in the UpdatePath, so its leaf is removed from every target list here. The
// exclusion is by NODE index and it is applied to the resolution rather than to the tree,
// because an added leaf reaches a resolution by two different routes -- as itself, when its own
// leaf node is the nearest non-blank node under the copath child, and as an unmerged leaf
// appended behind whatever non-blank ancestor lists it. An exclusion that only removed the
// first would leave the second in place and seal the secret to the new member anyway, which is
// the failure that shows up nowhere: the member decrypts fine, and it was never supposed to be
// able to.
//
// The free Resolution and not the method, and the difference is the error. RatchetTree.
// Resolution answers the EMPTY list for a refusal, which is the same answer an accepted empty
// resolution gives -- fine for a caller whose index came out of a direct path of this same
// tree, and not fine here: an empty target list reads as "seal this secret to nobody", and this
// is one of the two entry points whose index is a caller's leaf rather than one this package
// already bounded. So the refusal is returned.
func (self *RatchetTree) EncryptionTargets(sender LeafIndex, exclude []LeafIndex) ([][]NodeIndex, error) {
	steps, err := self.filteredPathSteps(sender)
	if err != nil {
		return nil, err
	}
	excluded := map[NodeIndex]bool{}
	for _, leaf := range exclude {
		excluded[leaf.NodeIndex()] = true
	}
	out := make([][]NodeIndex, 0, len(steps))
	for _, step := range steps {
		resolution, err := Resolution(self, step.CopathChild)
		if err != nil {
			return nil, err
		}
		// a fresh slice rather than resolution[:0], because narrowing in place writes
		// through the backing array the walk just built and would leave the kept entries
		// followed by whatever the excluded ones were.
		kept := make([]NodeIndex, 0, len(resolution))
		for _, y := range resolution {
			if excluded[y] {
				continue
			}
			kept = append(kept, y)
		}
		out = append(out, kept)
	}
	return out, nil
}
