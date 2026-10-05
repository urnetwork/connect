// The two private adapters the rest of this plan's files are written against: a preimage
// encoder that runs a caller's encode function over a fresh Writer, and the (value, ok)
// shims over tree_math's error returning arithmetic.
//
// Nothing here is exported and nothing here is owed to another plan. The file exists so
// that the shape of these five calls is decided once, with its argument written down,
// rather than at each of the couple of dozen call sites the tasks after this one add.
package mls

import "github.com/urnetwork/connect/mls/syntax"

// marshalBytes runs an encoder against a fresh Writer and yields its bytes, surfacing the
// Writer's sticky error.
//
// It is for the preimages that are NOT themselves syntax.Codec values: the LeafNodeTBS and
// KeyPackageTBS signature contents and the three section 7.8 and 7.9 hash inputs. Every wire
// type goes through syntax.Marshal instead, and the distinction is the point of having a
// separate name for this -- a structure that gains a MarshalMLS should stop coming through
// here, because syntax.Marshal is where the trailing byte contract lives.
//
// The Writer it opens carries the default vector limit, which is the decision
// TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit pins for every codec entry this
// package makes: none of the preimages above is a ratchet tree.
func marshalBytes(encode func(w *syntax.Writer) error) ([]byte, error) {
	w := syntax.NewWriter()
	if err := encode(w); err != nil {
		return nil, err
	}
	return w.Bytes()
}

// The tree math plan returns an error from every arithmetic that can be out of range. Inside a
// RatchetTree the leaf width is at least one and every node index this package forms is
// already in range, so the error arm is unreachable and a (value, ok) shape reads better at
// those call sites.
//
// They are internal to this plan and exported to nobody, because a shim that turns an error
// into false is how a trailing blank tree gets silently accepted somewhere that has no such
// invariant. Every call site in the tasks after this one turns false into ErrTreeMalformed, so
// no error is swallowed -- it is renamed to the condition that is actually true when the
// arithmetic refuses, which is that the tree the index was formed against is not a tree.
func leftOf(x NodeIndex) (NodeIndex, bool) {
	y, err := Left(x)
	return y, err == nil
}

func rightOf(x NodeIndex) (NodeIndex, bool) {
	y, err := Right(x)
	return y, err == nil
}

func leafIndexOf(x NodeIndex) (LeafIndex, bool) {
	i, err := x.LeafIndex()
	return i, err == nil
}

// rootOf and directPathOf keep the error rather than taking the shape above, because they are
// the pair a future change to the width invariant would otherwise break by returning node
// zero: Root(0) and DirectPath(x, 0) are the two arithmetics whose refusal a bool would spend,
// and a caller handed node zero for the root of an empty tree computes a whole tree hash over
// a leaf that is not there. The explicit zero check in front of each is not redundant with the
// arithmetic's own -- it names this package's invariant at this package's boundary, so the
// refusal says "the tree is malformed" rather than repeating whatever tree_math said about a
// leaf count it was never meant to see.
func rootOf(n LeafCount) (NodeIndex, error) {
	if n == 0 {
		return 0, ErrTreeMalformed
	}
	return Root(n)
}

func directPathOf(x NodeIndex, n LeafCount) ([]NodeIndex, error) {
	if n == 0 {
		return nil, ErrTreeMalformed
	}
	return DirectPath(x, n)
}
