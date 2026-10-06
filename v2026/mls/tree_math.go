// array-based ratchet-tree index arithmetic, per RFC 9420 appendix C and
// section 4.1.
//
// nothing in this file is cryptographic and nothing in it reads a node's
// contents, so it is deterministic, exhaustively testable, and safe to call
// from any goroutine. leaves are even-numbered nodes, with leaf L at node 2*L,
// and intermediate nodes are odd-numbered.
//
// the tree is always full: RFC 9420 section 7.7 states that adding or removing
// leaves doubles or halves the tree, so a valid leaf count is always a power of
// two. every function here that takes a leaf count enforces that, which is
// stricter than the appendix C pseudocode and deliberately so — appendix C with
// a non-power-of-two count silently answers for the enclosing full tree and can
// return an index past the end of the node array.
//
// no group-size policy lives here. the 500-member and 10-device caps are v1
// product rules enforced in commit.go.
package mls

import (
	"errors"
	"math/bits"
)

// the index of a leaf, counted from zero at the left. a member of the group
// occupies exactly one leaf.
type LeafIndex uint32

// the index of any node in the flat array, leaf or parent.
type NodeIndex uint32

// the number of leaves in a tree, always a power of two for a valid tree
// (RFC 9420 section 7.7).
type LeafCount uint32

// the largest representable tree. node width of this count is 2^32-1, the
// largest value a node index can hold, so every index computation in this file
// stays inside uint32 without a carry.
const MaxLeafCount LeafCount = 1 << 31

var (
	ErrLeafCountRange    = errors.New("mls: leaf count out of range")
	ErrLeafCountNotFull  = errors.New("mls: leaf count is not a power of two")
	ErrNodeOutOfRange    = errors.New("mls: node index outside the tree")
	ErrLeafOutOfRange    = errors.New("mls: leaf index outside the tree")
	ErrNodeIsParent      = errors.New("mls: node index is a parent, not a leaf")
	ErrLeafHasNoChildren = errors.New("mls: leaf node has no children")
	ErrRootHasNoParent   = errors.New("mls: root node has no parent")
	ErrRootHasNoSibling  = errors.New("mls: root node has no sibling")
	ErrNodeWidthNotOdd   = errors.New("mls: node array width is not odd")
)

// the exponent of the largest power of two not greater than x. zero for x == 0,
// matching the appendix C special case rather than being undefined there.
func log2(x uint32) uint32 {
	if x == 0 {
		return 0
	}
	return uint32(bits.Len32(x) - 1)
}

// the array position of a leaf: leaf L sits at node 2*L.
//
// total, and so wraps rather than refusing: a leaf index of 2^31 or above sits
// in no representable tree, and 2*L for it is taken modulo 2^32 — leaf 2^31
// answers node 0, indistinguishable from leaf 0. every caller range-checks its
// leaf count against MaxLeafCount before converting, so no reachable path holds
// an index that large, but a zero from this function is not an error signal.
func (self LeafIndex) NodeIndex() NodeIndex {
	return NodeIndex(2 * uint32(self))
}

// even node indices are leaves, odd ones are parents.
func (self NodeIndex) IsLeaf() bool {
	return self&0x01 == 0
}

// the inverse of LeafIndex.NodeIndex, refused for a parent rather than
// silently truncating.
func (self NodeIndex) LeafIndex() (LeafIndex, error) {
	if !self.IsLeaf() {
		return 0, ErrNodeIsParent
	}
	return LeafIndex(uint32(self) / 2), nil
}

// leaves are level zero, their parents level one, and so on. the level of an
// odd index is its count of trailing one bits.
//
// the only index whose level is 32 is 0xFFFFFFFF, which is one past the last
// node of the largest representable tree and therefore never inside one; the
// value is returned rather than special-cased so this stays a total function.
func (self NodeIndex) Level() uint32 {
	if self.IsLeaf() {
		return 0
	}
	return uint32(bits.TrailingZeros32(^uint32(self)))
}

// the number of nodes in the flat array for a tree with n leaves.
//
// deliberately total and deliberately not restricted to full leaf counts: the
// ratchet_tree extension carries an array with its trailing blank nodes
// stripped, so a width of node_width(6) = 11 is a legal thing to reason about
// even though a tree never has six leaves. a count past MaxLeafCount returns
// zero so that every downstream range check fails closed.
func NodeWidth(n LeafCount) uint32 {
	if n == 0 || n > MaxLeafCount {
		return 0
	}
	return 2*(uint32(n)-1) + 1
}

// whether n is a leaf count a valid tree can actually have: non-zero, in range,
// and a power of two.
func IsFullLeafCount(n LeafCount) bool {
	return n > 0 && n <= MaxLeafCount && n&(n-1) == 0
}

// the depth of the full tree that contains n leaves, which is the length of any
// leaf's direct path in that tree. one leaf is depth zero.
func TreeDepth(n LeafCount) uint32 {
	if n <= 1 {
		return 0
	}
	return uint32(bits.Len32(uint32(n) - 1))
}

// the smallest full leaf count that contains n leaves. zero for n == 0 and for
// n past MaxLeafCount, so an out-of-range count fails closed.
func FullLeafCount(n LeafCount) LeafCount {
	if n == 0 || n > MaxLeafCount {
		return 0
	}
	return LeafCount(1) << TreeDepth(n)
}

// the leaf count an array of w nodes describes. every node array has an odd
// width, and a truncated ratchet_tree array yields a count that is not a power
// of two — pass the result through FullLeafCount to get the tree it belongs to.
func LeafCountFromNodeWidth(w uint32) (LeafCount, error) {
	if w == 0 || w%2 == 0 {
		return 0, ErrNodeWidthNotOdd
	}
	return LeafCount((uint64(w) + 1) / 2), nil
}

// the leaf count after adding a blank root whose left subtree is the existing
// tree (RFC 9420 section 7.7). an empty tree extends to one leaf.
func ExtendedLeafCount(n LeafCount) (LeafCount, error) {
	if n == 0 {
		return 1, nil
	}
	// range is tested before fullness, matching checkLeafCount, because a count
	// past MaxLeafCount is both out of range and not a power of two and the
	// order alone decides which sentinel the caller sees. Testing fullness
	// first reported MaxLeafCount+1 as ErrLeafCountNotFull here while the shared
	// check called the same value ErrLeafCountRange, so a caller switching on the
	// sentinel would have had to know which function produced it. The two halves
	// of that are asserted rather than stated: TestExtendAndTruncate drives
	// MaxLeafCount+1 through this function and TestCheckLeafCount drives the same
	// value through the shared check, and both want ErrLeafCountRange.
	if n > MaxLeafCount {
		return 0, ErrLeafCountRange
	}
	if !IsFullLeafCount(n) {
		return 0, ErrLeafCountNotFull
	}
	if n == MaxLeafCount {
		return 0, ErrLeafCountRange
	}
	return n * 2, nil
}

// the leaf count after removing right subtrees until one holds a non-blank leaf
// (RFC 9420 section 12.1.3): 2^d for the smallest d with 2^d greater than the
// index of the rightmost non-blank leaf.
//
// which leaf that is depends on node contents and is decided by the caller;
// only the arithmetic lives here.
func TruncatedLeafCount(rightmostNonBlankLeaf LeafIndex) (LeafCount, error) {
	if LeafCount(rightmostNonBlankLeaf) >= MaxLeafCount {
		return 0, ErrLeafOutOfRange
	}
	return LeafCount(1) << TreeDepth(LeafCount(rightmostNonBlankLeaf)+1), nil
}

// the shared entry check for every function that takes a leaf count and answers
// about a real tree.
func checkLeafCount(n LeafCount) error {
	if n == 0 || n > MaxLeafCount {
		return ErrLeafCountRange
	}
	if !IsFullLeafCount(n) {
		return ErrLeafCountNotFull
	}
	return nil
}

// the index of the root of a tree with n leaves.
//
// the root sits at 2^d - 1 for a tree of depth d, so it is the one index that
// is the same for every count in a doubling band — which is exactly why a
// non-power-of-two count is refused here rather than quietly answered.
func Root(n LeafCount) (NodeIndex, error) {
	if err := checkLeafCount(n); err != nil {
		return 0, err
	}
	w := NodeWidth(n)
	return NodeIndex((uint32(1) << log2(w)) - 1), nil
}

// the left child of a parent node. children are computed from the index alone,
// so no leaf count is needed and the answer is the same in every tree that
// contains x.
func Left(x NodeIndex) (NodeIndex, error) {
	k := x.Level()
	if k == 0 {
		return 0, ErrLeafHasNoChildren
	}
	if k > 31 {
		return 0, ErrNodeOutOfRange
	}
	return x ^ NodeIndex(uint32(0x01)<<(k-1)), nil
}

// the right child of a parent node.
func Right(x NodeIndex) (NodeIndex, error) {
	k := x.Level()
	if k == 0 {
		return 0, ErrLeafHasNoChildren
	}
	if k > 31 {
		return 0, ErrNodeOutOfRange
	}
	return x ^ NodeIndex(uint32(0x03)<<(k-1)), nil
}

// the parent of a node in a tree with n leaves.
//
// the leaf count is used only to locate the root, exactly as in appendix C; the
// arithmetic itself is index-only. it is done in uint64 so the shift by k+1 is
// obviously in range without an argument about the maximum level of a non-root
// node.
func Parent(x NodeIndex, n LeafCount) (NodeIndex, error) {
	r, err := Root(n)
	if err != nil {
		return 0, err
	}
	if uint32(x) >= NodeWidth(n) {
		return 0, ErrNodeOutOfRange
	}
	if x == r {
		return 0, ErrRootHasNoParent
	}
	k := uint64(x.Level())
	b := (uint64(x) >> (k + 1)) & 0x01
	return NodeIndex((uint64(x) | (uint64(1) << k)) ^ (b << (k + 1))), nil
}

// the other child of the node's parent.
func Sibling(x NodeIndex, n LeafCount) (NodeIndex, error) {
	p, err := Parent(x, n)
	if err != nil {
		if errors.Is(err, ErrRootHasNoParent) {
			return 0, ErrRootHasNoSibling
		}
		return 0, err
	}
	if x < p {
		return Right(p)
	}
	return Left(p)
}

// the path from x to the root, ordered leaf to root, excluding x and including
// the root. the root's direct path is empty (RFC 9420 section 4.1.2).
//
// the result is always a slice and never nil, empty root included, so a caller
// can range over it without a nil check. a refusal carries no slice at all, so
// a caller that reads the value and drops the error walks nothing rather than
// walking a partly built path.
//
// the loop is bounded explicitly. it cannot run away for a validated index —
// every step moves to a strictly higher level and the root holds the highest —
// but a structural bound makes that a property of the code rather than of an
// argument about the code. the bound is pinned from below and not from above:
// the deepest tree walks a path of 31 nodes, so a bound of 29 or less truncates
// a real answer and the tests catch it, while every value from 30 up is
// indistinguishable from every other. measured, one version per value from 0 to
// 32: 0 through 29 fail, 30, 31 and 32 pass.
//
// the range check is a second line rather than the only one. Parent refuses an
// index past the end of the tree with the same sentinel and the same absent
// slice, so weakening this check, or skipping it at any one depth, changes
// nothing a caller can observe — measured, thirty-nine versions of it, every
// one indistinguishable from this one. it is kept because a function should
// enforce its own precondition rather than inherit it from the one it happens
// to call, and because a rewrite that walked ancestors by arithmetic instead of
// by Parent would leave nothing else guarding the entry.
func DirectPath(x NodeIndex, n LeafCount) ([]NodeIndex, error) {
	r, err := Root(n)
	if err != nil {
		return nil, err
	}
	if uint32(x) >= NodeWidth(n) {
		return nil, ErrNodeOutOfRange
	}

	pathNodes := make([]NodeIndex, 0, TreeDepth(n))
	for steps := uint32(0); x != r; steps += 1 {
		if steps > 32 {
			return nil, ErrNodeOutOfRange
		}
		x, err = Parent(x, n)
		if err != nil {
			return nil, err
		}
		pathNodes = append(pathNodes, x)
	}
	return pathNodes, nil
}

// the sibling of x followed by the sibling of every node on x's direct path
// except the root, ordered leaf to root (RFC 9420 section 4.1.2).
//
// always the same length as the direct path, and every entry is the child of
// the direct-path entry at the same position that x does not descend from. the
// root has an empty copath, and as with the direct path the empty result is a
// slice rather than nil and a refusal carries no slice.
//
// the range check here stands to DirectPath's as DirectPath's stands to
// Parent's, and is unobservable for the same reason and by the same
// measurement: thirty-nine versions of it, none of them distinguishable. the
// root case below is a shortcut rather than a correction — an empty direct path
// would yield an empty copath through the loop anyway — and is likewise
// indistinguishable from its own absence.
func Copath(x NodeIndex, n LeafCount) ([]NodeIndex, error) {
	r, err := Root(n)
	if err != nil {
		return nil, err
	}
	if uint32(x) >= NodeWidth(n) {
		return nil, ErrNodeOutOfRange
	}
	if x == r {
		return []NodeIndex{}, nil
	}

	pathNodes, err := DirectPath(x, n)
	if err != nil {
		return nil, err
	}

	// the siblings wanted are those of x and of every direct-path node below
	// the root, which is the direct path shifted down by one with x in front.
	// the root is never the argument to Sibling, which is why no refusal from
	// it can reach a caller here.
	copathNodes := make([]NodeIndex, 0, len(pathNodes))
	child := x
	for _, pathNode := range pathNodes {
		sibling, err := Sibling(child, n)
		if err != nil {
			return nil, err
		}
		copathNodes = append(copathNodes, sibling)
		child = pathNode
	}
	return copathNodes, nil
}

// the lowest node that is an ancestor of both x and y, where a node counts as
// an ancestor of itself (RFC 9420 appendix C).
//
// the answer does not depend on the leaf count: any tree containing both nodes
// contains this node at this index, which is why no count is taken. that is
// asserted and not only stated — the test file asks the semantic definition the
// same question in every tree from the smallest that holds the pair up to the
// largest there is, and requires one answer throughout.
//
// the appendix publishes two definitions of this relation, this arithmetic one
// and a semantic one built out of direct paths, and the test file runs the
// second against the first rather than restating either.
//
// total, and deliberately: an index past the end of every representable tree
// gets an answer rather than a refusal, because the relation is defined on
// indices and not inside a tree. 0xFFFFFFFF is the one such index, it reads as
// a level-32 node as Level describes, and it is its own answer against
// anything.
//
// the arithmetic runs in uint64 because the shift counts reach 33, which is
// past the width of what is being shifted and reads as a mistake in uint32 even
// though Go defines it. it buys nothing else: measured, the same body in uint32
// is indistinguishable from this one for every input, so the width is a
// statement to a reader rather than a guard.
//
// the loop runs at most 32 times, since two distinct 32-bit values agree once
// both have been shifted to zero, so it needs no explicit bound of the kind
// DirectPath carries. that maximum is pinned from below rather than argued:
// measured, one version per stopping count from 0 to 33, every count from 0 to
// 31 fails and 32 and 33 pass.
func CommonAncestor(x NodeIndex, y NodeIndex) NodeIndex {
	// one may be an ancestor of the other, in which case it is the answer.
	// these two cases are not a shortcut: the loop below descends from the
	// value the two indices agree on, which for a node and one of its own
	// descendants is not that node. measured, either one removed and every
	// version fails.
	//
	// their level tests are another matter, and the enumeration says which
	// part of each is load-bearing. dropping the first one's is caught: the
	// shift test on its own also fires for an index that merely falls inside
	// y's index range rather than inside y's subtree. dropping the second
	// one's is not, because the first runs ahead of it and has already
	// answered every pair it would get wrong — measured, that test dropped,
	// narrowed to a strict comparison or to an inequality, and either test
	// skipped when its own operand is a leaf, all indistinguishable from this
	// code for every input. they are kept because a condition that is only
	// correct in the presence of the block above it is a trap for whoever
	// reorders them.
	levelOfX := uint64(x.Level()) + 1
	levelOfY := uint64(y.Level()) + 1
	if levelOfX <= levelOfY && uint64(x)>>levelOfY == uint64(y)>>levelOfY {
		return y
	}
	if levelOfY <= levelOfX && uint64(x)>>levelOfX == uint64(y)>>levelOfX {
		return x
	}

	// otherwise shift both right until they agree; the number of shifts is one
	// past the level of the node where the two subtrees join, and the value
	// they agree on is that node's own index shifted down by the same amount.
	shiftedX, shiftedY := uint64(x), uint64(y)
	shifts := uint64(0)
	for shiftedX != shiftedY {
		shiftedX >>= 1
		shiftedY >>= 1
		shifts += 1
	}
	return NodeIndex((shiftedX << shifts) + (uint64(1) << (shifts - 1)) - 1)
}

// the first and last node indices of the subtree headed by x, inclusive.
//
// the subtree of a level-k node is the 2^(k+1)-1 array slots centred on that
// node (RFC 9420 appendix C figure 32), so the ends are x - (2^k - 1) and
// x + (2^k - 1). a node at level k has exactly k trailing one bits, so it is
// never smaller than its own half-span and the subtraction cannot underflow;
// the rightmost node of level k sits at 2^32 - 2^k - 1, so the addition cannot
// overflow either.
//
// no leaf count is taken, for the reason CommonAncestor takes none: the span is
// a property of the array layout and is the same in every tree that holds the
// node. a caller that needs the span clipped to a tree range-checks x against
// NodeWidth first, which every caller in this package already does.
//
// 0xFFFFFFFF is the one index no tree holds — it is one past the last node of
// the largest representable tree — and it reads as a level-32 node as Level
// describes. a level-32 subtree is 2^33-1 slots and is not representable, so it
// answers itself alone. the alternative is not a smaller mistake: without this
// guard the half-span is 2^32-1, which truncates to 0xFFFFFFFF, and the answer
// is [0, 0xFFFFFFFE] — the whole array of the largest tree, which would make an
// index inside no tree the head of every node of the largest one.
//
// that guard is held by exactly one test and it is worth knowing which.
// measured: with the guard removed, and again with the guarded answer written
// as the whole array, every test in this package passes except
// TestSubtreeSpanArms, which asks for the span of 0xFFFFFFFF. a sweep of nodes
// inside a tree never reaches the index that is inside none.
//
// measured rather than argued, in a scratch copy. a grammar over the three
// bodies — the level each is driven by, the guard's threshold and its form, the
// guard's answer, the half-span, each of the two endpoints, each of the two
// halvings, each of the two comparisons and the connective between them, and
// every perturbation that can be confined to one level or to one node, crossed
// with all 32 levels — enumerates 622 versions. the test file kills 572. of the
// 50 it does not, 38 are this function written differently: the threshold as
// k >= 32 or k == 32, the guard switched off at any level below 32, the
// half-span computed in uint32, Level's leaf shortcut dropped, level zero
// collapsed to itself, and the guarded answer written as x, 0xFFFFFFFF. that
// they are this function was established by walking all 2^32 indices, not
// inferred from the sweep.
//
// the other 12 were called real and named one node at a time, and the naming
// was the error. the class was not twelve nodes: it was every version that
// agreed with this one wherever the tests looked, and the tests looked at
// 0.4879% of the domain. measured since, by counting the distinct arguments a
// whole package run passes here — 20,955,304 of the 4,294,967,296 indices, with
// levels 0 to 7 at 0.09766% each — and versions wrong over runs of hundreds of
// millions of nodes at those levels passed the file. the sweep walks every
// index a tree can hold now, so that class is closed by construction rather
// than counted down: of 447 versions enumerated over the three bodies and the
// conditions that confine them to a level, a node, a run of blocks or a run of
// one span, 229 lived before and 14 live after — 11 of them a run inside one
// node's subtree at a level whose spans are probed rather than walked, and 3 a
// slot in the block past one node's subtree below level 5. both are keyed on a
// node, and the test file says what closing them would cost.
func SubtreeSpan(x NodeIndex) (firstNode NodeIndex, lastNode NodeIndex) {
	k := x.Level()
	if k > 31 {
		return x, x
	}
	halfSpan := NodeIndex((uint64(1) << k) - 1)
	return x - halfSpan, x + halfSpan
}

// the first and last leaf indices under x, inclusive.
//
// the span of a node any tree holds runs from that node's leftmost leaf to its
// rightmost, so both ends are even and both convert to a leaf exactly; for a
// leaf the pair is that leaf twice.
//
// 0xFFFFFFFF is the exception and it is named rather than left to be found. its
// span is the single odd slot 0xFFFFFFFF, so the halving truncates and the
// answer is leaf 0x7FFFFFFF twice — which sits at node 0xFFFFFFFE and is not
// inside the span above it, and which is also the exact pair the last leaf of
// the largest tree answers. no signature without an error can say "no leaves
// under this", so the answer is a plausible in-range one rather than a refusal,
// and a caller holding an index it did not choose range-checks it against
// NodeWidth first. every index that survives such a check is even at both ends
// of its span.
//
// that one row is doing more work than it looks. rounding either halving up
// instead of down is this same function at every index a tree holds, since both
// ends of such a span are even, and differs only here, where the addition wraps
// to zero. measured, once per end: every test in this package passes both
// versions except TestSubtreeSpanArms.
func SubtreeLeaves(x NodeIndex) (firstLeaf LeafIndex, lastLeaf LeafIndex) {
	firstNode, lastNode := SubtreeSpan(x)
	return LeafIndex(uint32(firstNode) / 2), LeafIndex(uint32(lastNode) / 2)
}

// whether x is head or a descendant of head.
//
// the subtree of a node is a contiguous run of array slots, so this is the
// range test on the span rather than a walk up from x. the two give the same
// answer (RFC 9420 appendix C figure 32) and the range test is the one that
// terminates for an index no tree holds, since it asks nothing about a parent.
//
// which slots this is asked about is worth recording, because for a while it
// was asked too narrowly to see a whole shape of mistake. the sweeps used to
// probe a subtree at its two ends, at the head, and at the leftmost and
// rightmost descendant on a direct path, and every one of those is a
// power-of-two offset into the span: measured on that file, level 10 was asked
// about 21 of the 2,047 offsets a span has, level 20 about 41 of 2,097,151 and
// level 31 about 63 of 4,294,967,295. a version answering false for the quarter
// of every subtree between 1/4 and 1/2 of its span, at every head above level
// 9, passed the whole package — and section 7.9 has this function intersecting
// unmerged leaves with the leaves under a child, where being wrong over a
// quarter of a large subtree is a parent hash that verifies when it should not.
// it is asked now about every slot of a subtree up to level 4, about a ladder
// and offsets that move with the block above that, about slots nowhere near the
// span, and about every ordered pair of a 2^14-leaf tree.
func InSubtree(head NodeIndex, x NodeIndex) bool {
	firstNode, lastNode := SubtreeSpan(head)
	return firstNode <= x && x <= lastNode
}

// the minimal view of node contents the two shape rules need.
//
// tree.go implements this over the real ratchet tree, which keeps every public
// key and credential out of this file. UnmergedLeaves returns the node's stored
// list in stored order: that the list is sorted and that its entries are
// non-blank leaves inside the node's subtree are RFC 9420 section 7.9
// tree-validation checks and belong to tree_sync.go, not here.
type NodeShape interface {
	LeafCount() LeafCount
	IsBlank(x NodeIndex) bool
	UnmergedLeaves(x NodeIndex) []LeafIndex
}

// the ordered list of non-blank nodes that collectively cover every non-blank
// descendant of x: a depth-first, left-first enumeration of the nearest
// non-blank nodes below it (RFC 9420 section 4.1).
//
// a non-blank node resolves to itself followed by its unmerged leaves, a blank
// leaf to nothing, and a blank parent to its left child's resolution followed
// by its right child's. the traversal uses an explicit stack so a deep tree
// cannot become deep go stack. the stack gains one entry for each level it
// descends and nothing else, so 32 is its width in the deepest tree there is —
// measured over a whole package run, 32 at the peak, which is the capacity it
// is made with.
//
// the order is the contract and not a detail of it. TreeKEM encrypts a path
// secret to the entries of a resolution one position at a time, so a list with
// the right members in the wrong order hands a member someone else's secret and
// never hands it its own. the answer ascends by node index everywhere except at
// an unmerged leaf, which follows immediately behind the node carrying it and
// is usually below it — figure 10's [X, B] is [3, 2] — which is exactly why a
// test that compares two resolutions as sets, or sorts them before comparing,
// passes every wrong order there is.
//
// the two entry checks are ordered and the order is observable: a shape whose
// leaf count is no tree is refused as that whatever node it was asked about, so
// a caller switching on the sentinel can tell a bad tree from a bad request. a
// refusal carries no slice at all and an accepted empty resolution is an empty
// slice, the same pair of promises DirectPath and Copath make.
//
// what this file checks about an unmerged list is that its entries are inside
// the tree, and nothing else. sorted, free of repeats, and confined to the
// node's own subtree are the section 7.9 tree-validation rules and tree_sync.go
// owns them; a list that breaks any of them comes back through here in stored
// order rather than being quietly repaired, because a repair here would hide
// the tree that needs rejecting.
//
// the answer is as large as the caller's own tree and no ceiling is put on it
// here: the root of a 2^31-leaf tree with every parent blank resolves to 2^31
// nodes. the shape belongs to a ratchet tree the caller already holds, so that
// tree is the bound, and the 500-member policy that keeps it small in practice
// lives in commit.go.
//
// the refusals from Left and Right are unreachable. x is range checked against
// the node width, every node reached from it is inside the same tree, and the
// only index whose level is past 31 is 0xFFFFFFFF, which that check excludes at
// every leaf count. they are kept for the reason DirectPath keeps its own range
// check: a function should enforce its own precondition rather than inherit it
// from whatever it happens to call.
//
// measured rather than argued, in a scratch copy. a grammar over the body — the
// two entry checks and their order, the comparison and the bound of the range
// check, the blank test, the node emit, every clause of the unmerged handling,
// the position in the list and the node at which its bound is enforced, the
// blank-leaf stop, the pop end, the push order, the child pair, six allocation
// sizes and seven reworkings of the finished list — enumerates versions of this
// function, and each is run against the test file. the counts are dated, so
// they live in the task report where being superseded is expected rather than
// here where nothing checks them.
//
// what lives at every index at once is this function written differently, and
// each was checked by running it against this code over more than a million
// shapes rather than argued: the six make capacities, since a slice of length
// zero is non-nil whatever it was made with and append grows it either way;
// and x > w-1 for x >= w, which is the same test at every width, because a leaf
// count that passed the check above makes the width at least one.
//
// what is not covered is a class and not nothing. reaching a node and being
// able to see a defect at it are different things, and an earlier version of
// this comment ran them together: it argued from the levels the suite reaches
// that no defect above level 18 could hide, and a push order swapped at one
// block of level 19 disproved it at a node the suite does visit. the sweeps ask
// about every block of every level from 19 up as the head of a resolution, in
// every representable tree, and a defect confined to one block of one of those
// levels has nowhere to sit — 180 such versions were run, none survived. level
// 18 is the boundary: every tree but the deepest walks it whole, and of 60
// single-block versions there 11 survived, nine of them in the half of the
// level that exists only in a 2^31-leaf tree. from level 17 down the blocks of
// a level are sampled by a stride and the surviving class is real: of 90
// versions at level 17, 52 lived, and every one aimed at a block the suite
// never reaches lived by construction. closing that is a walk of 2^32 nodes per
// shape, which is why the line is drawn where it is rather than argued away.
func Resolution(shape NodeShape, x NodeIndex) ([]NodeIndex, error) {
	n := shape.LeafCount()
	if err := checkLeafCount(n); err != nil {
		return nil, err
	}
	if uint32(x) >= NodeWidth(n) {
		return nil, ErrNodeOutOfRange
	}

	resolvedNodes := make([]NodeIndex, 0, 8)
	pendingNodes := make([]NodeIndex, 0, 32)
	pendingNodes = append(pendingNodes, x)
	for len(pendingNodes) > 0 {
		node := pendingNodes[len(pendingNodes)-1]
		pendingNodes = pendingNodes[:len(pendingNodes)-1]

		if !shape.IsBlank(node) {
			resolvedNodes = append(resolvedNodes, node)
			for _, leaf := range shape.UnmergedLeaves(node) {
				if LeafCount(leaf) >= n {
					return nil, ErrLeafOutOfRange
				}
				resolvedNodes = append(resolvedNodes, leaf.NodeIndex())
			}
			continue
		}
		if node.IsLeaf() {
			continue
		}

		leftChild, err := Left(node)
		if err != nil {
			return nil, err
		}
		rightChild, err := Right(node)
		if err != nil {
			return nil, err
		}
		// pushed right first so the left child is popped first
		pendingNodes = append(pendingNodes, rightChild, leftChild)
	}
	return resolvedNodes, nil
}

// one node of a filtered direct path together with the child of that node the
// source leaf does not descend from.
//
// the pair is carried rather than the node alone because every caller needs
// both, and deriving the second from the first is the step that gets written
// backwards: RFC 9420 section 7.4 encrypts the path secret of the node to the
// resolution of the copath child and never the other way about. both fields
// are indices of the same tree, so a version that swaps them type-checks and
// round-trips, which is why the tests compare steps and not node lists.
type PathStep struct {
	Node        NodeIndex
	CopathChild NodeIndex
}

// the direct path of a leaf with every node removed whose child on that leaf's
// copath resolves to nothing, ordered leaf to root, each surviving node paired
// with that copath child (RFC 9420 section 4.1.2).
//
// a removed node needs no key pair of its own, because encrypting to it would
// be encrypting to its non-copath child, which is the child the leaf descends
// from and already holds the secret. the length of the result is the number of
// nodes an UpdatePath has to carry, which is what ValSem202 checks, and the
// order is the order they appear in it.
//
// that order is the contract and not a detail of it, as it is for a
// resolution. each step's secret is encrypted to the resolution of that step's
// copath child, so a path holding the right steps in the wrong order hands
// every member below the first difference someone else's secret. a test that
// compares two filtered paths as sets, or by length, passes every wrong order
// there is — and length is the one property of this answer a wrong filter
// still gets right, since filtering is what changes it.
//
// what decides a step is whether the copath child's resolution is empty and
// nothing else about it. an unmerged leaf is appended behind a node that is
// already in the list, so it moves the length of a resolution and never
// whether it is empty: the parenthetical in section 4.1.2 warning that
// unmerged leaves count toward the copath child's resolution guards against
// reading a resolution as the blank rule alone, and is not a case that can
// move this answer. the test file establishes that by asking one tree twice,
// with and without a list hung on every node, rather than restating it.
//
// the whole resolution is built rather than a search stopped at the first
// non-blank node, and the refusal that comes with it is inherited on purpose:
// a copath child carrying an unmerged leaf outside the tree is a malformed
// shape, and a filtered path over one would be a path for a tree that cannot
// exist. the leaf's own side is not read, so the same malformed list on the
// direct path passes unseen; rejecting that is section 7.9 tree validation and
// tree_sync.go owns it.
//
// the cost of a dropped node is not incidental. certifying that a subtree
// resolves to nothing means finding no non-blank node anywhere in it, and the
// shape answers one node at a time, so a drop at level k costs 2^(k+1)-1
// queries here or in any other implementation of this interface, where a kept
// node costs one. that is the bound on how far up the tests observe a drop,
// and the level they reach is dated so it lives in the task report.
//
// the refusals from Sibling are unreachable. the node it is asked about is the
// leaf on the first pass and the previous path node on every later one, and
// the root is only ever the last path node, so it is never the argument. the
// two entry checks are ordered as Resolution's are — a shape that is no tree
// is refused as that whatever leaf it was asked about — and a refusal carries
// no slice at all while an accepted empty path is an empty slice, which is the
// pair of promises DirectPath, Copath and Resolution all make.
func FilteredDirectPath(shape NodeShape, leaf LeafIndex) ([]PathStep, error) {
	n := shape.LeafCount()
	if err := checkLeafCount(n); err != nil {
		return nil, err
	}
	if LeafCount(leaf) >= n {
		return nil, ErrLeafOutOfRange
	}

	leafNode := leaf.NodeIndex()
	pathNodes, err := DirectPath(leafNode, n)
	if err != nil {
		return nil, err
	}

	pathSteps := make([]PathStep, 0, len(pathNodes))
	child := leafNode
	for _, pathNode := range pathNodes {
		copathChild, err := Sibling(child, n)
		if err != nil {
			return nil, err
		}
		copathResolution, err := Resolution(shape, copathChild)
		if err != nil {
			return nil, err
		}
		if len(copathResolution) > 0 {
			pathSteps = append(pathSteps, PathStep{
				Node:        pathNode,
				CopathChild: copathChild,
			})
		}
		child = pathNode
	}
	return pathSteps, nil
}
