// The RFC 9420 section 7.8 tree hash, which is what GroupContext.TreeHash carries and what
// the key schedule therefore binds every epoch's secrets to.
//
// A tree hash that differs by one byte between two implementations is not a validation
// failure, it is a FORK: each side's group context is internally consistent, each side signs
// and verifies against its own, and the two stop agreeing from the first commit onward with
// nothing on the wire that names the disagreement. That is why the tests beside this file are
// weighted the way they are. Self-consistency proves nothing here -- an encoder that writes
// the leaf index after the leaf instead of before it writes it that way every time and agrees
// with itself forever -- so the assertions that can see a defect are the published
// tree-validation corpus, whose hashes this repository did not compute, and a hand-derived
// golden whose preimage is written out byte by byte from the struct definitions below.
package mls

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// leafHashInput is TreeHashInput's leaf arm:
//
//	struct { uint32 leaf_index; optional<LeafNode> leaf_node; } LeafNodeHashInput;
//
// The leaf INDEX is inside the hash input, which is what makes a blank position distinguishable
// from every other blank position: without it every empty leaf of a tree hashes to the same
// value and a tree with two blanks agrees with a tree that has them elsewhere. optional<LeafNode>
// goes through WriteOptional so the presence octet has one implementation and one canonical
// spelling, rather than a second 0-or-1 written here.
func (self *RatchetTree) leafHashInput(w *syntax.Writer, i LeafIndex, leaf *LeafNode) error {
	w.WriteUint8(uint8(NodeTypeLeaf))
	w.WriteUint32(uint32(i))
	return w.WriteOptional(leaf != nil, func(w *syntax.Writer) error {
		return leaf.MarshalMLS(w)
	})
}

// parentHashInput is TreeHashInput's parent arm:
//
//	struct { optional<ParentNode> parent_node; opaque left_hash<V>; opaque right_hash<V>; }
//
// The order of the two child hashes is the whole content of this function and it is not
// recoverable from any tree that is symmetric at the point it is tested. leftHash first.
func (self *RatchetTree) parentHashInput(w *syntax.Writer, parent *ParentNode,
	leftHash, rightHash []byte) error {
	w.WriteUint8(uint8(NodeTypeParent))
	if err := w.WriteOptional(parent != nil, func(w *syntax.Writer) error {
		return parent.MarshalMLS(w)
	}); err != nil {
		return err
	}
	w.WriteOpaque(leftHash)
	w.WriteOpaque(rightHash)
	return nil
}

// treeHash is the hash of the subtree rooted at x, with the leaves in exclude treated as blank
// and every descendant parent node's unmerged_leaves filtered by the same set.
//
// exclude is nil for an ordinary tree hash and is the parent's unmerged_leaves when computing
// an ORIGINAL tree hash for a parent hash, RFC 9420 section 7.9. One recursion serves both, on
// purpose: the original tree hash is the section 7.8 hash of a tree the unmerged leaves were
// never added to, so a second walk that re-derived the same encoding would be a second place
// for the field order to be wrong, and the two would diverge silently because only one of them
// is what the published corpus checks.
//
// A blank position is still hashed, at its own index, rather than skipped -- see leafHashInput.
func (self *RatchetTree) treeHash(crypto CryptoProvider, x NodeIndex,
	exclude map[LeafIndex]bool) ([]byte, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: every node's hash input is hashed through it", ErrNilCryptoProvider)
	}
	if uint32(x) >= self.NodeWidth() {
		return nil, ErrNodeIndexOutOfRange
	}
	if x.IsLeaf() {
		i, ok := leafIndexOf(x)
		if !ok {
			return nil, ErrTreeMalformed
		}
		leaf := self.Leaf(i)
		if exclude != nil && exclude[i] {
			leaf = nil
		}
		input, err := marshalBytes(func(w *syntax.Writer) error {
			return self.leafHashInput(w, i, leaf)
		})
		if err != nil {
			return nil, err
		}
		return crypto.Hash(input), nil
	}
	left, ok := leftOf(x)
	if !ok {
		return nil, ErrTreeMalformed
	}
	right, ok := rightOf(x)
	if !ok {
		return nil, ErrTreeMalformed
	}
	leftHash, err := self.treeHash(crypto, left, exclude)
	if err != nil {
		return nil, err
	}
	rightHash, err := self.treeHash(crypto, right, exclude)
	if err != nil {
		return nil, err
	}
	parent := self.ParentAt(x)
	if parent != nil && exclude != nil && len(parent.UnmergedLeaves) > 0 {
		// the filtered copy is a Clone and never the tree's own node: this walk runs over a
		// live tree during a parent hash check, and dropping entries in place would leave the
		// caller holding a tree whose unmerged lists the exclusion had eaten.
		filtered := parent.Clone()
		kept := filtered.UnmergedLeaves[:0]
		for _, leaf := range parent.UnmergedLeaves {
			if !exclude[leaf] {
				kept = append(kept, leaf)
			}
		}
		filtered.UnmergedLeaves = kept
		parent = filtered
	}
	input, err := marshalBytes(func(w *syntax.Writer) error {
		return self.parentHashInput(w, parent, leftHash, rightHash)
	})
	if err != nil {
		return nil, err
	}
	return crypto.Hash(input), nil
}

// NodeTreeHash is the tree hash of the subtree rooted at x. ErrNodeIndexOutOfRange for an index
// outside this tree, which is a different answer from the hash of a blank node -- the blank is
// inside the tree and hashes.
func (self *RatchetTree) NodeTreeHash(crypto CryptoProvider, x NodeIndex) ([]byte, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: the subtree's hash is taken through it", ErrNilCryptoProvider)
	}
	return self.treeHash(crypto, x, nil)
}

// TreeHash is the whole tree's hash, which is what GroupContext.TreeHash is set from.
func (self *RatchetTree) TreeHash(crypto CryptoProvider) ([]byte, error) {
	// before rootOf and not after it: a zero valued tree answers ErrTreeMalformed from the
	// arithmetic, and a caller who passed no provider would be sent to look at its tree.
	if crypto == nil {
		return nil, fmt.Errorf("%w: the whole tree's hash is taken through it", ErrNilCryptoProvider)
	}
	root, err := rootOf(self.LeafWidth())
	if err != nil {
		return nil, err
	}
	return self.treeHash(crypto, root, nil)
}

// TreeHashes is every node's tree hash, indexed by node index; the tree-validation vector
// publishes exactly this column and checks all of it.
//
// It re-walks each subtree rather than memoising, so it costs about 2n*log(n) hash calls for an
// n leaf tree rather than the 2n-1 a single bottom up pass would. That is deliberate and it is
// not a placeholder: the alternative is a second encoder for the same structure, and the one
// thing this file must not have is two ways to compute the same hash. It is called once per
// parent-hash check, over trees of a few hundred nodes, and TestTreeHashesIndexedByNode is what
// holds it equal to NodeTreeHash entry for entry.
func (self *RatchetTree) TreeHashes(crypto CryptoProvider) ([][]byte, error) {
	// before the loop, which a tree of no nodes never enters: without this, no provider and an
	// empty tree would answer an empty slice and a nil error rather than a refusal.
	if crypto == nil {
		return nil, fmt.Errorf("%w: every node's hash is taken through it", ErrNilCryptoProvider)
	}
	// the width is refused through the SAME arithmetic TreeHash asks, and asked before the loop
	// for the same reason: a tree with no leaves never enters the loop, so without this the two
	// entry points answer one receiver differently -- TreeHash refuses it as malformed and
	// TreeHashes hands back an empty column and a nil error. A caller that checks the column it
	// got rather than the error would then read "this tree has no nodes" out of a tree that is
	// not a tree, at a parent hash check, which is where an empty answer is indistinguishable
	// from a passing one.
	if _, err := rootOf(self.LeafWidth()); err != nil {
		return nil, err
	}
	out := make([][]byte, self.NodeWidth())
	for x := uint32(0); x < self.NodeWidth(); x += 1 {
		hash, err := self.treeHash(crypto, NodeIndex(x), nil)
		if err != nil {
			return nil, err
		}
		out[x] = hash
	}
	return out, nil
}

// errCopathChildIsNotAChildOfTheParent names the one argument mistake this method could not
// otherwise report.
//
// ParentHash(P, S) is defined only for S a CHILD of P: RFC 9420 section 7.9 states it as "the
// parent hash of P with copath child S", S being the child of P that is not on the path being
// updated. Handed any other node, the walk below would hash that node's subtree quite happily
// and answer a well formed digest of the right width over the wrong tree, which no length
// check, round trip or comparison against this implementation can see. So the two children are
// DERIVED from the index here and the argument is held against them, rather than the caller
// being trusted to have picked one of them -- the whole security of the chain is that the hash
// covers the OTHER subtree, and a hash that covered something else would still chain.
//
// Unexported because it is a caller's bug and not a protocol condition: nothing arriving off
// the wire reaches it, so no caller outside this package has a branch to write for it. A blank
// parent node is the other refusal and answers ErrParentHashMismatch instead, because that one
// IS a question about a tree somebody else sent.
var errCopathChildIsNotAChildOfTheParent = errors.New(
	"mls: the copath child is not a child of the node whose parent hash was asked for")

// ParentHash is RFC 9420 section 7.9: the parent hash of the node at parent, taken with respect
// to the copath child -- the child of parent that is NOT on the path being updated.
//
//	struct { HPKEPublicKey encryption_key;
//	         opaque parent_hash<V>;
//	         opaque original_sibling_tree_hash<V>; } ParentHashInput;
//
// "With respect to" is the entire mechanism, and it is why the copath child is an argument
// rather than something this method chooses. The hash covers the OTHER subtree, so a member
// cannot rewrite its own side of the tree without moving the parent hash of every node above
// it -- which is what the signature a leaf makes over its parent_hash field then pins.
//
// original_sibling_tree_hash is the ORIGINAL tree hash of that subtree, not its tree hash:
// section 7.9 says to blank every leaf in parent.unmerged_leaves and strike those leaves out of
// every descendant's unmerged_leaves before hashing it. That is treeHash's exclude arm, and it
// is the half of this function that is invisible until somebody has been added since the last
// commit -- over a parent with no unmerged leaves the original tree hash and the plain tree
// hash of the same subtree are the same bytes, so every fixture without an unmerged leaf agrees
// with an implementation that never heard of the exclusion.
// What holds the exclusion arm is the section 7.8 pair beside it --
// TestTheOriginalTreeHashIsTheTreeHashOfTheTreeWithoutThoseLeaves and
// TestTheOriginalTreeHashStrikesTheExcludedLeafOutOfUnmergedLeaves -- and, for this
// function's own bytes, TestVerifyParentHashesAcceptsThePublishedTreeValidationCorpus:
// the 290 non-blank parents of the working group's own trees chain through this
// preimage, and several of those parents carry unmerged leaves inside the sibling
// subtree, so a version that skipped the blanking fails there. Since task 18 it is also
// held against the RFC's own notation, spelled out by hand:
// TestTheParentHashChainOfRfc9420AppendixBsWorkedExample runs appendix B's four steps and
// TestEveryParentHashOverATreeWithUnmergedLeavesTakesTheOriginalSiblingTreeHash runs the
// same notation over a tree where a member joined after the last commit, which is the one
// state appendix B's own example never reaches and the only one where the original hash
// and the plain one differ.
func (self *RatchetTree) ParentHash(crypto CryptoProvider,
	parent, copathChild NodeIndex) ([]byte, error) {
	// before anything is read off the receiver, for TreeHash's reason: a caller that passed no
	// provider is told that, rather than being sent to look at a tree that is not the problem.
	if crypto == nil {
		return nil, fmt.Errorf("%w: the parent hash input is hashed through it", ErrNilCryptoProvider)
	}
	if uint32(parent) >= self.NodeWidth() {
		return nil, ErrNodeIndexOutOfRange
	}
	left, leftOk := leftOf(parent)
	right, rightOk := rightOf(parent)
	if !leftOk || !rightOk {
		// an even index is a leaf, and a leaf has no children for a copath child to be one of.
		// SetParent answers the same sentinel to the same mistake.
		return nil, ErrNodeTypeMismatch
	}
	if copathChild != left && copathChild != right {
		return nil, errCopathChildIsNotAChildOfTheParent
	}
	node := self.ParentAt(parent)
	if node == nil {
		return nil, fmt.Errorf("%w: node %d is blank, so it publishes no encryption_key and carries no parent_hash",
			ErrParentHashMismatch, parent)
	}
	exclude := map[LeafIndex]bool{}
	for _, leaf := range node.UnmergedLeaves {
		exclude[leaf] = true
	}
	siblingHash, err := self.treeHash(crypto, copathChild, exclude)
	if err != nil {
		return nil, err
	}
	input, err := marshalBytes(func(w *syntax.Writer) error {
		w.WriteOpaque(node.EncryptionKey)
		w.WriteOpaque(node.ParentHash)
		w.WriteOpaque(siblingHash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return crypto.Hash(input), nil
}

// nodeParentHashField is the parent_hash a node CARRIES, whatever kind of node it is, and a
// bool for whether it carries one at all.
//
// RFC 9420 section 7.9.1: a ParentNode always has the field, and a LeafNode has it only under
// leaf_node_source = commit. What is read is the SOURCE and not the field, and the reason is the
// SIGNATURE rather than the comparison. marshalCore writes parent_hash into the bytes a leaf is
// signed over under the commit arm alone, so on a key_package or an update leaf the Go field is
// covered by nothing its member signed: an attacker holding somebody else's signed update leaf
// can hang any parent hash on it, that leaf still verifies at its own index in its own group,
// and a chain check that read the field regardless would take it as the one descendant claiming
// a parent node the attacker wrote. So "no field" is the bool and never an empty slice, and the
// two are not the same answer.
//
// It is NOT that an empty field would compare equal to a parent hash. subtle.ConstantTimeCompare
// answers 0 on a length mismatch, so a zero valued field never matches a 32 octet hash, and an
// earlier version of this comment justified the predicate with a mechanism this package does not
// have. The predicate is right and the reason it was given for was wrong, which is the more
// dangerous of the two to leave standing: a later reader who checked the stated reason would
// find it false and delete the line.
//
// Off the wire the predicate is defence in depth today. unmarshalCore stages a fresh LeafNode and
// assigns ParentHash under the commit arm alone, so a leaf decoded from bytes cannot arrive
// carrying one under another source. It is load bearing for the leaves this package CONSTRUCTS
// in memory, where the field outlives the source that put it there -- an update written over a
// leaf that had committed keeps the bytes unless somebody clears them, and nothing in the type
// makes that impossible. TestOnlyACommitSourcedLeafCarriesAParentHashField holds this function
// over the sources the package declares, and
// TestVerifyParentHashesRefusesAClaimantWhoseSourceDoesNotCarryAParentHash holds the tree.
func nodeParentHashField(node *Node) ([]byte, bool) {
	if node == nil {
		return nil, false
	}
	if node.Parent != nil {
		return node.Parent.ParentHash, true
	}
	if node.Leaf != nil && node.Leaf.LeafNodeSource == LeafNodeSourceCommit {
		return node.Leaf.ParentHash, true
	}
	return nil, false
}

// ---- RFC 9420 section 7.9.2, parent hash validity over the whole tree.
//
// This is the check that stops a member handing a joiner a tree in which they have substituted
// themselves for somebody else. A parent node's encryption key is only trustworthy because some
// member signed a leaf whose parent_hash chains up to it; drop the check and a forged tree is
// adopted and its author reads the group.
//
// The rule is stated in section 7.9.2 and it is worth being exact about which reading this
// implements, because the plan this task came from restates it as a two armed disjunction while
// the RFC's own text is a three condition conjunction inside a disjunction. Section 7.9.2 in
// full: the parent hash in a node D is valid with respect to a parent P, whose children are C
// (the one on D's direct path) and S (the other), when
//
//  1. D is a descendant of P,
//  2. D's parent_hash field equals the parent hash of P with copath child S, and
//  3. D is in the resolution of C, and the intersection of P's unmerged_leaves with the subtree
//     under C equals the resolution of C with D removed.
//
// and then: "the new member MUST authenticate that each non-blank parent node P is parent-hash
// valid ... top down by verifying that there is EXACTLY ONE descendant of each non-blank parent
// node for which the parent node is parent-hash valid".
//
// Condition 3 is the half a restatement drops, and dropping it is not conservative. Without it
// the question is only "does SOMETHING under this child carry the right hash", which a forger
// satisfies by leaving the legitimate chain in place and splicing an extra subtree in beside it
// -- the spliced nodes are then in the resolution, so the next commit seals a path secret to
// keys the forger chose, while every parent still chains. Condition 3 is what says the
// resolution of C holds the one claimant and NOTHING else except the leaves added since P was
// last set, which is the only set allowed to be there.
// TestVerifyParentHashesRefusesACoTenantInTheResolutionOfTheClaimant is the pair of trees that
// differ in exactly that and nothing else.
//
// The count is over BOTH arms together and not one per arm. "Exactly one descendant" is a single
// number for the node, so neither zero -- P was never legitimately written -- nor two -- two
// update paths spliced together, or a claimant manufactured on each side -- is accepted.

// unmergedLeavesUnder is the intersection of the parent's unmerged_leaves with the subtree under
// child, as the NODE indices a resolution is made of, counted rather than set-flagged so a
// repeated entry cannot be matched twice.
//
// The conversion is the point. unmerged_leaves is a list of LEAF indices and a resolution is a
// list of NODE indices, and condition 3 equates the two, so one of them has to be carried into
// the other's space. Carrying the leaves up is the direction that cannot lose information, since
// every leaf has a node index and not every node index is a leaf.
func (self *RatchetTree) unmergedLeavesUnder(parent *ParentNode, child NodeIndex) map[NodeIndex]int {
	firstLeaf, lastLeaf := SubtreeLeaves(child)
	under := map[NodeIndex]int{}
	for _, leaf := range parent.UnmergedLeaves {
		if leaf >= firstLeaf && leaf <= lastLeaf {
			under[leaf.NodeIndex()] += 1
		}
	}
	return under
}

// resolutionIsTheClaimantAndTheUnmergedLeaves is condition 3: the resolution of the child, with
// the claimant struck out, is exactly the parent's unmerged leaves under that child.
//
// Equality of SETS and not of ordered lists, which departs from how every other comparison of a
// resolution in this package is written and so is stated here rather than left to be noticed.
// Resolution order is a contract where two resolutions are compared to EACH OTHER, because
// TreeKEM pairs a resolution positionally with an UpdatePath's ciphertexts. Here a resolution is
// compared against an unmerged_leaves vector, a different object with an order of its own --
// ascending, which the codec enforces -- and section 7.9.2 relates the two with "is equal to"
// over an intersection, which is a statement about membership. An elementwise comparison would
// additionally demand that a non-blank child's own unmerged leaves sort after the child itself,
// which holds for a child sitting left of its unmerged leaves and fails for one sitting right of
// them, so it would refuse legal trees on one side of the tree only.
func (self *RatchetTree) resolutionIsTheClaimantAndTheUnmergedLeaves(parent *ParentNode,
	child NodeIndex, resolution []NodeIndex, claimant NodeIndex) bool {
	under := self.unmergedLeavesUnder(parent, child)
	struck := false
	for _, node := range resolution {
		if node == claimant && !struck {
			struck = true
			continue
		}
		if under[node] == 0 {
			return false
		}
		under[node] -= 1
	}
	// the claimant has to have BEEN in the resolution, which is the first clause of condition 3
	// and is not implied by the arithmetic above: a claimant absent from the resolution leaves
	// it one entry longer than the unmerged set, which the loop only reports if that surplus
	// entry is also absent from the set.
	//
	// No CALLER reaches this line, and saying so is the point of writing it down.
	// parentHashClaimsUnder draws the claimant out of the very resolution it then hands in, so
	// struck is true on every call this package makes and condition 3's first clause is carried
	// by that loop rather than by this clause. What this holds is the HELPER's own contract, for
	// the next caller that asks about a claimant it did not take out of the resolution -- and it
	// is held by TestConditionThreeRefusesAClaimantThatIsNotInTheResolution, which drives the
	// helper directly, because driving it through the sweep is not possible.
	if !struck {
		return false
	}
	for _, remaining := range under {
		if remaining != 0 {
			return false
		}
	}
	return true
}

// parentHashClaimsUnder counts the descendants under child that are parent-hash valid with
// respect to the parent at p, over the arm whose copath child is sibling.
//
// It counts rather than answering a bool, because the rule above it is "exactly one" and a bool
// per arm cannot tell one claimant from two on the same side. Two claimants under one child is
// the shape a forger produces who kept the real chain and added a node of their own beside it.
func (self *RatchetTree) parentHashClaimsUnder(crypto CryptoProvider, parent *ParentNode,
	p NodeIndex, child NodeIndex, sibling NodeIndex) (int, error) {
	hash, err := self.ParentHash(crypto, p, sibling)
	if err != nil {
		return 0, err
	}
	// condition 1 comes free of the walk: every entry of the resolution of a child of p is a
	// descendant of p, so there is no separate descendant test here to get wrong.
	resolution := self.Resolution(child)
	claims := 0
	for _, node := range resolution {
		field, carries := nodeParentHashField(self.Get(node))
		if !carries {
			continue
		}
		// condition 2, through subtle rather than bytes.Equal for guardrail 8's reason: a parent
		// hash is a public value, but every comparison in this package that decides whether a
		// tree is adopted is written the one way, so no later reader has to work out which of
		// them were the safe ones.
		if subtle.ConstantTimeCompare(field, hash) != 1 {
			continue
		}
		// condition 3.
		if !self.resolutionIsTheClaimantAndTheUnmergedLeaves(parent, child, resolution, node) {
			continue
		}
		claims += 1
	}
	return claims, nil
}

// VerifyParentHashes is RFC 9420 section 7.9.2's join-time obligation: every non-blank parent
// node of this tree is parent-hash valid.
//
// A tree whose parent nodes are all blank passes, and that is the rule rather than a hole in it,
// because such a tree publishes no parent key for anybody to have forged.
func (self *RatchetTree) VerifyParentHashes(crypto CryptoProvider) error {
	// both refusals ahead of the sweep and not inside it, for the reason TreeHashes states at
	// the same spot: a tree with no leaves never enters the loop, so without this the answer for
	// a receiver that is not a tree would be nil -- "no parent failed" -- which at a parent hash
	// check is the same answer a sound tree gets.
	if crypto == nil {
		return fmt.Errorf("%w: every parent hash on this tree is taken through it", ErrNilCryptoProvider)
	}
	if _, err := rootOf(self.LeafWidth()); err != nil {
		return err
	}
	// every odd index and no even one: the odd slots are exactly the parents (RFC 9420 appendix
	// C), so which nodes are swept is derived from the tree's own width rather than from a list
	// of the parents somebody expected to find non-blank.
	for x := uint32(1); x < self.NodeWidth(); x += 2 {
		p := NodeIndex(x)
		parent := self.ParentAt(p)
		if parent == nil {
			continue
		}
		left, leftOk := leftOf(p)
		right, rightOk := rightOf(p)
		if !leftOk || !rightOk {
			return ErrTreeMalformed
		}
		claims := 0
		// both arms, because section 7.9.2's C is "the child that is on the direct path of D"
		// and which child that is depends on where the committer's leaf sits. A version that
		// looked at one of them refuses every tree committed from the other side of it.
		for _, arm := range [2][2]NodeIndex{{left, right}, {right, left}} {
			armClaims, err := self.parentHashClaimsUnder(crypto, parent, p, arm[0], arm[1])
			if err != nil {
				return err
			}
			claims += armClaims
		}
		if claims != 1 {
			return fmt.Errorf("%w: node %d is claimed by %d of its descendants and RFC 9420 section 7.9.2 requires exactly one",
				ErrParentHashMismatch, p, claims)
		}
	}
	return nil
}
