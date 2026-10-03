// The tests for the RFC 9420 section 7.8 tree hash.
//
// The weighting here is not the usual one, and the reason is in tree_hash.go's file comment: a
// tree hash that disagrees with another implementation by one byte is a FORK rather than a
// failure, so an assertion that only holds this package to itself is worth almost nothing. An
// encoder that writes the leaf index after the leaf, or the right subtree's hash before the
// left, or drops the presence octet on a blank, is perfectly self-consistent -- it produces one
// stable answer, it round trips, every mutation of the tree still moves it, and the group it
// builds is internally coherent and permanently invisible to everybody else.
//
// So there are three kinds of assertion below and only the last two can see that defect:
//
//   - the plan's three, which say the hash moves when the tree moves. They catch a hash that
//     ignores an input. They do not catch a hash that reads every input in the wrong order.
//   - a hand-derived golden, whose PREIMAGES are written out octet by octet from the section
//     7.8 struct definitions and whose digests were computed outside this repository. It pins
//     the structure -- the node type octet, the width and position of leaf_index, the presence
//     octet on a blank, the order of optional<ParentNode>, left_hash and right_hash -- rather
//     than merely pinning that the structure is stable.
//   - the published tree-validation corpus, whose 908 answers for the two suites this package
//     registers were computed by the working group's implementations and not by this one.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// the plan's three: the hash moves when the tree moves
// ---------------------------------------------------------------------------

// treeHashObservableVariants is the family the sweep below runs over: this tree, and every one
// step change to it the container can make, DERIVED from the tree's own width rather than
// written out here.
//
// The five hand written mutations this replaced -- blank a leaf, swap two leaves, set a
// parent, add an unmerged, grow the tree -- were a list, and a list is the thing this project
// keeps finding to be narrower than the class it names. Those five blanked node 2 and no other
// node, set a parent at node 1 and no other position, and never moved a leaf that was not leaf
// 0 or leaf 2, so a hash that read one arm of the recursion and not another had nowhere to
// fail. What is derived here is every node of the tree for the blanking, every parent position
// for the parent node, every parent position crossed with every leaf for the unmerged entry,
// every PAIR of leaves for the swap, and the growth, off NodeWidth and LeafWidth rather than
// off a number typed beside them.
func treeHashObservableVariants(t *testing.T, tree *RatchetTree) map[string]*RatchetTree {
	t.Helper()
	variants := map[string]*RatchetTree{"the tree itself": tree.Clone()}
	add := func(what string, change func(clone *RatchetTree) error) {
		t.Helper()
		if _, taken := variants[what]; taken {
			t.Fatalf("two rows of the derived family are named %q, so one of them is not being hashed", what)
		}
		clone := tree.Clone()
		if err := change(clone); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		variants[what] = clone
	}
	width := uint32(tree.LeafWidth())
	parentKey := HpkePublicKey(bytes.Repeat([]byte{0x01}, 32))
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		add(fmt.Sprintf("blank node %d", x), func(clone *RatchetTree) error {
			return clone.Blank(NodeIndex(x))
		})
		if NodeIndex(x).IsLeaf() {
			continue
		}
		add(fmt.Sprintf("a parent node at %d", x), func(clone *RatchetTree) error {
			return clone.SetParent(NodeIndex(x), &ParentNode{EncryptionKey: parentKey})
		})
		for i := uint32(0); i < width; i += 1 {
			add(fmt.Sprintf("leaf %d unmerged at node %d", i, x), func(clone *RatchetTree) error {
				return clone.SetParent(NodeIndex(x), &ParentNode{
					EncryptionKey:  parentKey,
					UnmergedLeaves: []LeafIndex{LeafIndex(i)},
				})
			})
		}
	}
	for i := uint32(0); i < width; i += 1 {
		for j := i + 1; j < width; j += 1 {
			add(fmt.Sprintf("swap leaves %d and %d", i, j), func(clone *RatchetTree) error {
				at, other := tree.Leaf(LeafIndex(i)), tree.Leaf(LeafIndex(j))
				if at == nil || other == nil {
					t.Fatalf("leaf %d or leaf %d is blank in the base tree, so this row is not the change it is named for", i, j)
				}
				if err := clone.SetLeaf(LeafIndex(i), other); err != nil {
					return err
				}
				return clone.SetLeaf(LeafIndex(j), at)
			})
		}
	}
	add("a leaf past the current width", func(clone *RatchetTree) error {
		return clone.SetLeaf(LeafIndex(width), tree.Leaf(LeafIndex(0)).Clone())
	})
	return variants
}

// TestTreeHashChangesWithEveryObservableChange states the claim its name makes as INJECTIVITY
// over that derived family: two trees that are different on the wire never share a tree hash.
//
// "Observable" is read off the tree's own encoder rather than decided in this file, which is
// what makes the name honest. The converse is deliberately NOT asserted, and must not be:
// MarshalMLS truncates trailing blank nodes, so a four leaf tree whose last leaf is blank
// encodes as the three leaf array does and hashes differently, and that is a property of the
// wire format rather than a defect of the hash.
//
// What an inequality still cannot see is written here so nobody reads this test as more than
// it is. A hash that reads every input in the WRONG ORDER is injective too: left before right,
// the leaf index before the leaf node, the presence octet on a blank -- flipping any of those
// keeps every tree in this family separate and changes the group. Measured, not supposed: the
// version this replaced passed under a left/right swap in parentHashInput, under a deleted
// leaf index, under a deleted node type octet and under a swap of two fields of
// LeafNode.marshalCore. Those are held by the hand derived goldens below and by the published
// corpus at the end of this file, which is the whole weighting this file's header describes.
func TestTreeHashChangesWithEveryObservableChange(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	base, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if len(base) != crypto.HashSize() {
		t.Fatalf("tree hash length = %d, want %d", len(base), crypto.HashSize())
	}
	variants := treeHashObservableVariants(t, tree)
	// the size is the derivation restated in arithmetic: the tree itself, one row per node for
	// the blanking, one per parent position, one per parent position and leaf for the unmerged
	// entry, one per unordered pair of leaves for the swap, and the growth. a family that came
	// back smaller than this is a derivation that read the width wrong, and it would report a
	// clean sweep having compared almost nothing.
	nodes, leaves := tree.NodeWidth(), uint32(tree.LeafWidth())
	parents := nodes - leaves
	if want := 1 + nodes + parents + parents*leaves + leaves*(leaves-1)/2 + 1; uint32(len(variants)) != want {
		t.Fatalf("the derived family holds %d trees, want %d over a %d node %d leaf tree",
			len(variants), want, nodes, leaves)
	}
	byHash := map[string]string{}
	byEncoding := map[string]string{}
	encoded := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(variants)) {
		wire, err := marshalRatchetTree(variants[name])
		if err != nil {
			t.Fatalf("%s: marshalRatchetTree: %v", name, err)
		}
		hash, err := variants[name].TreeHash(crypto)
		if err != nil {
			t.Fatalf("%s: TreeHash: %v", name, err)
		}
		encoded[name] = string(wire)
		if prior, seen := byHash[string(hash)]; seen && encoded[prior] != string(wire) {
			t.Errorf("%q and %q are different trees on the wire and share the tree hash %x", name, prior, hash)
		}
		byHash[string(hash)] = name
		byEncoding[string(wire)] = name
	}
	// the pairwise check above only ever compares a collision against the LAST tree that hashed
	// to it, so the counts are the half that cannot be walked past: every distinct encoding owes
	// a distinct hash, and fewer hashes than encodings is a collision whichever pair it was.
	if len(byHash) < len(byEncoding) {
		t.Errorf("the family holds %d trees that differ on the wire and only %d distinct tree hashes",
			len(byEncoding), len(byHash))
	}
	if len(byEncoding) < 2 {
		t.Fatalf("every tree in the family encodes the same, so no pair of them could have failed above")
	}
	t.Logf("%d derived trees, %d distinct on the wire, %d distinct tree hashes",
		len(variants), len(byEncoding), len(byHash))
}

func TestTreeHashesIndexedByNode(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 5)
	hashes, err := tree.TreeHashes(crypto)
	if err != nil {
		t.Fatalf("TreeHashes: %v", err)
	}
	if uint32(len(hashes)) != tree.NodeWidth() {
		t.Fatalf("len(TreeHashes) = %d, want %d", len(hashes), tree.NodeWidth())
	}
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		one, err := tree.NodeTreeHash(crypto, NodeIndex(x))
		if err != nil {
			t.Fatalf("NodeTreeHash(%d): %v", x, err)
		}
		if !bytes.Equal(one, hashes[x]) {
			t.Fatalf("node %d: TreeHashes disagrees with NodeTreeHash", x)
		}
	}
	rootHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}
	if !bytes.Equal(rootHash, hashes[root]) {
		t.Fatalf("TreeHash is not the root entry of TreeHashes")
	}
}

// TestBlankLeafStillHashesAtItsIndex says both halves of its own name as BYTES: a blank
// position is HASHED rather than skipped -- the zero presence octet is in the preimage -- and
// what it is hashed with is ITS OWN index.
//
// It used to say only that two blanks at different indices differ, and that was measured to be
// a claim the implementation can break while keeping. With leafHashInput returning early for a
// nil leaf, so a blank writes no presence octet at all, leaf 0 hashed 01 00000000 and leaf 1
// hashed 01 00000001: still different, still passing, and a different group from every other
// implementation. An inequality cannot see what is IN a preimage.
//
// So the preimage is written out here octet by octet, the two rows the corpus of this file
// already holds digests for are checked against those digests -- computed outside this
// repository, which is what makes the spelling section 7.8's rather than this file's -- and
// the tree is asked about EVERY leaf it has rather than about two of them.
func TestBlankLeafStillHashesAtItsIndex(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		if err := tree.Blank(NodeIndex(x)); err != nil {
			t.Fatalf("Blank(%d): %v", x, err)
		}
	}
	// the goldens this file carries for leaf 0 and leaf 1 are the same two blank leaf
	// preimages, so the sweep below is anchored at both ends of a uint32 low octet rather than
	// hashing a spelling only this file has ever agreed with.
	published := map[uint32]string{
		0: blankTwoLeafTreeLeafZeroHash,
		1: blankTwoLeafTreeLeafOneHash,
	}
	anchored := 0
	seen := map[string]uint32{}
	width := uint32(tree.LeafWidth())
	if width < 2 {
		t.Fatalf("the tree is %d leaves wide, so no two indices could differ here", width)
	}
	for i := uint32(0); i < width; i += 1 {
		//	01 <leaf index as uint32> 00
		//	node_type=leaf(1), the index this position has and no other, and the presence
		//	octet that spells "there is no leaf here" rather than nothing at all
		want := crypto.Hash(treeHashTestConcat(
			[]byte{byte(NodeTypeLeaf)},
			[]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)},
			[]byte{0x00},
		))
		if golden, isPublished := published[i]; isPublished {
			if got := fmt.Sprintf("%x", want); got != golden {
				t.Fatalf("the hand written blank preimage for leaf %d hashes to %s, want %s; the preimage above and the golden no longer agree",
					i, got, golden)
			}
			anchored += 1
		}
		got, err := tree.NodeTreeHash(crypto, LeafIndex(i).NodeIndex())
		if err != nil {
			t.Fatalf("NodeTreeHash(leaf %d): %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("blank leaf %d hashes to %x, want %x -- node_type, the leaf index as a uint32, then the zero presence octet",
				i, got, want)
		}
		if prior, collided := seen[string(got)]; collided {
			t.Errorf("blank leaf %d hashes the same as blank leaf %d", i, prior)
		}
		seen[string(got)] = i
	}
	if anchored != len(published) {
		t.Errorf("%d of the %d published blank leaf digests were reached, so the sweep is hashing a spelling nothing outside this repository has checked",
			anchored, len(published))
	}
}

// ---------------------------------------------------------------------------
// the hand-derived golden
// ---------------------------------------------------------------------------

// treeHashTestOpaque is opaque<V> of RFC 9420 section 2.1.2, written here rather than reached
// through syntax.Writer.
//
// A preimage assembled with the encoder under test cannot separate two readings of that
// encoder, which is the whole point of the goldens below: if the length prefix were written at
// the wrong width, an expectation built with the same Writer would be wrong in exactly the same
// way and would still match. Only the one octet header is implemented, because every length
// these tests use is 63 or less, and a longer one is a fatal rather than a prefix this helper
// would have to be trusted about.
func treeHashTestOpaque(t *testing.T, bs []byte) []byte {
	t.Helper()
	if len(bs) > 0x3f {
		t.Fatalf("this hand written opaque<V> only spells the one octet header, and %d bytes needs more", len(bs))
	}
	return append([]byte{byte(len(bs))}, bs...)
}

// treeHashTestConcat joins the pieces of a preimage into a fresh slice, so no piece is aliased
// into the result and a later append cannot rewrite a preimage already hashed.
func treeHashTestConcat(pieces ...[]byte) []byte {
	out := []byte{}
	for _, piece := range pieces {
		out = append(out, piece...)
	}
	return out
}

// twoLeafBlankTree is a two leaf tree in which every one of the three nodes is blank.
//
// Built through the public surface -- newTestTree then Blank on each node -- rather than by
// reaching into the node array, so what is hashed is the same shape any other caller can
// produce, and so a Blank that stored a zero valued node instead of removing one would be
// visible here.
func twoLeafBlankTree(t *testing.T, crypto CryptoProvider) *RatchetTree {
	t.Helper()
	tree, _ := newTestTree(t, crypto, 2)
	if tree.NodeWidth() != 3 {
		t.Fatalf("a two member test tree is %d nodes wide, want 3", tree.NodeWidth())
	}
	for _, x := range []NodeIndex{0, 1, 2} {
		if err := tree.Blank(x); err != nil {
			t.Fatalf("Blank(%d): %v", x, err)
		}
	}
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		if !tree.IsBlank(NodeIndex(x)) {
			t.Fatalf("node %d is not blank after Blank", x)
		}
	}
	return tree
}

// The three digests of the all blank two leaf tree, computed OUTSIDE this repository -- python
// hashlib over the preimages spelled beside them -- so they are an answer this package did not
// produce. sha256 is the hash of both registered suites, so one set of digests serves both.
//
//	leaf 0   01 00000000 00
//	         node_type=leaf(1), leaf_index=0 as uint32, optional<LeafNode> absent
//	      -> 36c159d76b52e03e496363607c5940227f62f4e048df344b050159bc97bba3d7
//	leaf 1   01 00000001 00
//	      -> a90d4563c6a0ae0417ab3110f1ba68592833465954774201b0a69e8c457dc6ad
//	node 1   02 00 20||h(leaf 0) 20||h(leaf 1)
//	         node_type=parent(2), optional<ParentNode> absent, then the two child hashes each
//	         as opaque<V>: 32 bytes is 0x20 in a one octet varint header
//	      -> d7c368e06ce04517a1a9378b8bee6e886ede978a91da686b94018602db996b23
const (
	blankTwoLeafTreeLeafZeroHash = "36c159d76b52e03e496363607c5940227f62f4e048df344b050159bc97bba3d7"
	blankTwoLeafTreeLeafOneHash  = "a90d4563c6a0ae0417ab3110f1ba68592833465954774201b0a69e8c457dc6ad"
	blankTwoLeafTreeRootHash     = "d7c368e06ce04517a1a9378b8bee6e886ede978a91da686b94018602db996b23"
)

// TestABlankSubtreeHashesToTheHandDerivedGolden is the assertion that a blank position is
// HASHED and not skipped, stated as bytes rather than as an inequality.
//
// The three plan tests above can only say that two blanks at different indices differ, which a
// preimage of nothing but the leaf index satisfies just as well as the RFC's does. What this
// says is which octets go in: the type tag, the four octet index, and the zero presence octet
// that is how "there is no leaf here" is spelled -- as opposed to writing nothing at all for a
// blank, which is a shorter preimage, a different digest and a different group.
func TestABlankSubtreeHashesToTheHandDerivedGolden(t *testing.T) {
	// the tree is built once and hashed under every registered suite. newTestTree's leaves list
	// one suite in their capabilities and section 7.3 validation refuses them under the other,
	// so the builder takes that provider. what the sweep is about is the HASH, and both
	// registered suites name sha256, so one set of goldens serves both or the two have drifted.
	tree := twoLeafBlankTree(t, mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519))
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)

		leafZero := treeHashTestConcat([]byte{byte(NodeTypeLeaf)}, []byte{0x00, 0x00, 0x00, 0x00}, []byte{0x00})
		leafOne := treeHashTestConcat([]byte{byte(NodeTypeLeaf)}, []byte{0x00, 0x00, 0x00, 0x01}, []byte{0x00})
		wantZero := crypto.Hash(leafZero)
		wantOne := crypto.Hash(leafOne)
		root := treeHashTestConcat(
			[]byte{byte(NodeTypeParent)},
			[]byte{0x00},
			treeHashTestOpaque(t, wantZero),
			treeHashTestOpaque(t, wantOne),
		)
		wantRoot := crypto.Hash(root)

		// the digests first, against the values computed outside this repository. this is what
		// makes the preimages above a statement about section 7.8 rather than a restatement of
		// whatever this package happens to write.
		for _, one := range []struct {
			what string
			got  []byte
			want string
		}{
			{"leaf 0", wantZero, blankTwoLeafTreeLeafZeroHash},
			{"leaf 1", wantOne, blankTwoLeafTreeLeafOneHash},
			{"the root", wantRoot, blankTwoLeafTreeRootHash},
		} {
			if got := fmt.Sprintf("%x", one.got); got != one.want {
				t.Fatalf("suite %04x: the hand written preimage for %s hashes to %s, want %s; the golden and the preimage beside it no longer agree",
					suite, one.what, got, one.want)
			}
		}

		for _, one := range []struct {
			x    NodeIndex
			want []byte
			what string
		}{
			{0, wantZero, "blank leaf 0"},
			{2, wantOne, "blank leaf 1"},
			{1, wantRoot, "the blank root over two blank leaves"},
		} {
			got, err := tree.NodeTreeHash(crypto, one.x)
			if err != nil {
				t.Fatalf("suite %04x: NodeTreeHash(%d): %v", suite, one.x, err)
			}
			if !bytes.Equal(got, one.want) {
				t.Errorf("suite %04x: %s hashes to %x, want %x", suite, one.what, got, one.want)
			}
		}
		whole, err := tree.TreeHash(crypto)
		if err != nil {
			t.Fatalf("suite %04x: TreeHash: %v", suite, err)
		}
		if !bytes.Equal(whole, wantRoot) {
			t.Errorf("suite %04x: TreeHash of an all blank two leaf tree is %x, want %x", suite, whole, wantRoot)
		}
	}
}

// The digest of the same two leaf tree carrying one ParentNode at the root, computed outside
// this repository over the preimage spelled here.
//
//	ParentNode  20||01*32  00  04||00000001
//	            encryption_key = 32 octets of 0x01 as opaque<V>, parent_hash empty, then
//	            unmerged_leaves as a vector whose length prefix is in BYTES: one uint32 is 4
//	node 1      02 01 <ParentNode> 20||h(leaf 0) 20||h(leaf 1)
//	            node_type=parent(2), optional<ParentNode> PRESENT, the node, then the children
//	         -> e2b9595acece8a79b68ab0df30d4fe5d6bd8f7421912e64b93981ca0ef1c981d
const parentTwoLeafTreeRootHash = "e2b9595acece8a79b68ab0df30d4fe5d6bd8f7421912e64b93981ca0ef1c981d"

// TestAParentNodeHashesInTheHandDerivedOrder pins the parent arm's field order: the optional
// parent node comes BEFORE the two child hashes, and the parent node itself is
// encryption_key, parent_hash, unmerged_leaves.
//
// Nothing else in this file separates those orders. The plan's mutation table moves the hash
// when a parent is set and when an unmerged leaf is added, which a preimage that wrote the
// parent node after the child hashes -- or wrote its three fields in any other order -- moves
// exactly as well.
func TestAParentNodeHashesInTheHandDerivedOrder(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree := twoLeafBlankTree(t, crypto)
	parent := &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x01}, 32)),
		UnmergedLeaves: []LeafIndex{1},
	}
	if err := tree.SetParent(NodeIndex(1), parent); err != nil {
		t.Fatalf("SetParent(1): %v", err)
	}
	leafZero := crypto.Hash(treeHashTestConcat([]byte{byte(NodeTypeLeaf)}, []byte{0x00, 0x00, 0x00, 0x00}, []byte{0x00}))
	leafOne := crypto.Hash(treeHashTestConcat([]byte{byte(NodeTypeLeaf)}, []byte{0x00, 0x00, 0x00, 0x01}, []byte{0x00}))
	parentBytes := treeHashTestConcat(
		treeHashTestOpaque(t, bytes.Repeat([]byte{0x01}, 32)),
		treeHashTestOpaque(t, nil),
		treeHashTestOpaque(t, []byte{0x00, 0x00, 0x00, 0x01}),
	)
	wantRoot := crypto.Hash(treeHashTestConcat(
		[]byte{byte(NodeTypeParent)},
		[]byte{0x01},
		parentBytes,
		treeHashTestOpaque(t, leafZero),
		treeHashTestOpaque(t, leafOne),
	))
	if got := fmt.Sprintf("%x", wantRoot); got != parentTwoLeafTreeRootHash {
		t.Fatalf("the hand written preimage hashes to %s, want the golden %s", got, parentTwoLeafTreeRootHash)
	}
	got, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if !bytes.Equal(got, wantRoot) {
		t.Errorf("a two leaf tree with one parent node hashes to %x, want %x", got, wantRoot)
	}
	// the same tree with the parent BLANK, so the presence octet is doing work rather than the
	// two goldens differing because the preimages have different lengths for some other reason
	blanked := tree.Clone()
	if err := blanked.Blank(NodeIndex(1)); err != nil {
		t.Fatalf("Blank(1): %v", err)
	}
	blankRoot, err := blanked.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash of the blanked tree: %v", err)
	}
	if bytes.Equal(blankRoot, got) {
		t.Errorf("a present parent node and a blank one hash the same at %x", got)
	}
	if want := blankTwoLeafTreeRootHash; fmt.Sprintf("%x", blankRoot) != want {
		t.Errorf("blanking the parent gives %x, want the blank tree golden %s", blankRoot, want)
	}
}

// ---------------------------------------------------------------------------
// the recursion order
// ---------------------------------------------------------------------------

// TestTheParentHashInputPlacesTheLeftSubtreeFirst is the left-then-right clause, over a tree
// whose two halves are DERIVED to be different rather than hoped to be.
//
// Three members in a four wide tree puts two occupied leaves under the root's left child and
// one occupied leaf plus one blank under its right, and the test refuses to proceed unless the
// two halves actually hash differently -- on a symmetric tree the two orders produce the same
// root and this assertion would pass against either reading.
//
// The recomposition is the assertion, not an inequality: the root's hash is rebuilt from the
// children Left and Right name, in the order section 7.8 states, and compared. Left and Right
// are tree_math's own and are checked against the published tree-math corpus, so this does not
// share its notion of "left" with the shim tree_hash.go reaches them through -- a leftOf that
// answered Right would pass every other test in this package and fail here.
func TestTheParentHashInputPlacesTheLeftSubtreeFirst(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 3)
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}
	left, err := Left(root)
	if err != nil {
		t.Fatalf("Left(%d): %v", root, err)
	}
	right, err := Right(root)
	if err != nil {
		t.Fatalf("Right(%d): %v", root, err)
	}
	leftHash, err := tree.NodeTreeHash(crypto, left)
	if err != nil {
		t.Fatalf("NodeTreeHash(%d): %v", left, err)
	}
	rightHash, err := tree.NodeTreeHash(crypto, right)
	if err != nil {
		t.Fatalf("NodeTreeHash(%d): %v", right, err)
	}
	if bytes.Equal(leftHash, rightHash) {
		t.Fatalf("the two halves of this tree hash the same, so no assertion here can separate left-then-right from right-then-left")
	}
	if tree.ParentAt(root) != nil {
		t.Fatalf("the root of a fresh test tree carries a parent node, which the preimage below does not spell")
	}
	inOrder := crypto.Hash(treeHashTestConcat(
		[]byte{byte(NodeTypeParent)},
		[]byte{0x00},
		treeHashTestOpaque(t, leftHash),
		treeHashTestOpaque(t, rightHash),
	))
	reversed := crypto.Hash(treeHashTestConcat(
		[]byte{byte(NodeTypeParent)},
		[]byte{0x00},
		treeHashTestOpaque(t, rightHash),
		treeHashTestOpaque(t, leftHash),
	))
	if bytes.Equal(inOrder, reversed) {
		t.Fatalf("the two orders produce the same preimage digest, so this test cannot fail")
	}
	got, err := tree.NodeTreeHash(crypto, root)
	if err != nil {
		t.Fatalf("NodeTreeHash(%d): %v", root, err)
	}
	if bytes.Equal(got, reversed) {
		t.Fatalf("the root hash is the RIGHT subtree's hash followed by the left; section 7.8 writes left_hash then right_hash, and the two are different groups")
	}
	if !bytes.Equal(got, inOrder) {
		t.Errorf("the root hash is %x, want %x -- left_hash then right_hash over the children Left(%d) and Right(%d) name",
			got, inOrder, root, root)
	}
}

// TestNodeTreeHashRefusesAnIndexOutsideTheTree separates "outside the tree" from "blank", which
// are the two answers this recursion must never conflate: a blank node is inside the tree and
// hashes, and an index past the end has no hash at all.
func TestNodeTreeHashRefusesAnIndexOutsideTheTree(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	for _, x := range []NodeIndex{NodeIndex(tree.NodeWidth()), NodeIndex(tree.NodeWidth() + 1)} {
		if _, err := tree.NodeTreeHash(crypto, x); err == nil {
			t.Errorf("NodeTreeHash(%d) on a %d node tree answered a hash", x, tree.NodeWidth())
		}
	}
	last := NodeIndex(tree.NodeWidth() - 1)
	if err := tree.Blank(last); err != nil {
		t.Fatalf("Blank(%d): %v", last, err)
	}
	if _, err := tree.NodeTreeHash(crypto, last); err != nil {
		t.Errorf("NodeTreeHash of the blank node %d: %v; a blank node is in the tree and hashes", last, err)
	}
}

// ---------------------------------------------------------------------------
// the published corpus
// ---------------------------------------------------------------------------

// treeHashValidationVector reads the two columns this file needs out of the mlswg
// tree-validation family: the ratchet tree and the published tree hash of every one of its
// nodes.
//
// A third view of the same corpus, beside tree_math_test.go's resolutions and
// leaf_node_test.go's group id, for the reason those two give: the columns are different and a
// struct carrying all of them would make a change to any one of them a change to all three.
//
// This is NOT the registered family. Family 10 is installed by Task 24, which verifies the
// resolutions, the parent hashes and the leaf signatures together and deletes 10 from
// expectedPendingFamilies in the same commit; registering it here would claim a coverage this
// task does not have. What is here is a direct read of one column, exactly as the two existing
// readers of this file are.
type treeHashValidationVector struct {
	CipherSuite uint16   `json:"cipher_suite"`
	Tree        string   `json:"tree"`
	TreeHashes  []string `json:"tree_hashes"`
}

// What the corpus holds for the two suites this package registers, so a sweep that decoded
// nothing, filtered everything out or stopped early fails here rather than reporting a clean
// run over an empty set. Measured off the vendored file and a property of it: 14 entries per
// suite over two registered suites, and 908 published node hashes between them.
const (
	treeValidationImplementedEntries = 28
	treeValidationTreeHashCount      = 908
)

// TestTreeHashesAgainstThePublishedTreeValidationCorpus is the only assertion in this package
// whose tree hashes this repository did not compute.
//
// Everything else here hashes with this implementation and compares against this
// implementation, or against a preimage one reader wrote twice. These 908 answers were produced
// by the working group's own implementations from their own reading of section 7.8, over trees
// that carry blank leaves, blank parents, occupied parents and unmerged leaves, so a preimage
// that ordered a field differently, wrote a prefix at the wrong width, skipped a blank or
// recursed right-then-left fails here and, for several of those, here alone.
func TestTreeHashesAgainstThePublishedTreeValidationCorpus(t *testing.T) {
	entries := LoadVectorFile(t, treeValidationVectorFile)
	if len(entries) != treeValidationEntryCount {
		t.Fatalf("tree-validation entries: %d, want %d", len(entries), treeValidationEntryCount)
	}
	covered := 0
	confirmed := 0
	for entry, raw := range entries {
		vector := treeHashValidationVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", entry, err)
		}
		suite, implemented := implementedSuite(vector.CipherSuite)
		if !implemented {
			continue
		}
		covered += 1
		label := fmt.Sprintf("tree-validation entry %d", entry)
		crypto := mustProvider(t, suite)
		tree, err := UnmarshalRatchetTree(mustDecodeHex(t, label+" ratchet tree", vector.Tree))
		if err != nil {
			t.Fatalf("%s: decode the published ratchet tree: %v", label, err)
		}
		hashes, err := tree.TreeHashes(crypto)
		if err != nil {
			t.Fatalf("%s: TreeHashes: %v", label, err)
		}
		if len(hashes) != len(vector.TreeHashes) {
			t.Fatalf("%s: computed %d node hashes for a tree the corpus publishes %d for",
				label, len(hashes), len(vector.TreeHashes))
		}
		for x, published := range vector.TreeHashes {
			want := mustDecodeHex(t, fmt.Sprintf("%s node %d hash", label, x), published)
			if !bytes.Equal(hashes[x], want) {
				t.Errorf("%s: node %d hashes to %x, the corpus publishes %x", label, x, hashes[x], want)
				continue
			}
			confirmed += 1
		}
		root, err := rootOf(tree.LeafWidth())
		if err != nil {
			t.Fatalf("%s: rootOf: %v", label, err)
		}
		whole, err := tree.TreeHash(crypto)
		if err != nil {
			t.Fatalf("%s: TreeHash: %v", label, err)
		}
		if !bytes.Equal(whole, hashes[root]) {
			t.Errorf("%s: TreeHash is %x and the published root entry is %x", label, whole, hashes[root])
		}
	}
	if covered != treeValidationImplementedEntries {
		t.Errorf("%d published entries were covered, want %d; the suite filter or the decode moved",
			covered, treeValidationImplementedEntries)
	}
	if confirmed != treeValidationTreeHashCount {
		t.Errorf("%d published node hashes were confirmed, want %d", confirmed, treeValidationTreeHashCount)
	}
}

// TestThePublishedTreeHashComparisonCanSayNo is the negative control for the sweep above.
//
// Every entry of a vendored corpus agrees with a correct implementation, so a comparison that
// could never report a difference passes the whole run and reports 908 confirmations. What
// separates the two is asking the same corpus about a tree it does not describe: one leaf
// blanked out of a published tree moves that leaf's own hash and every hash above it and
// nothing else, and the count of moved entries is asserted against the direct path so that a
// control which found one difference by luck is not read as a control that works.
func TestThePublishedTreeHashComparisonCanSayNo(t *testing.T) {
	entries := LoadVectorFile(t, treeValidationVectorFile)
	checked := 0
	for entry, raw := range entries {
		vector := treeHashValidationVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", entry, err)
		}
		suite, implemented := implementedSuite(vector.CipherSuite)
		if !implemented {
			continue
		}
		label := fmt.Sprintf("tree-validation entry %d", entry)
		crypto := mustProvider(t, suite)
		tree, err := UnmarshalRatchetTree(mustDecodeHex(t, label+" ratchet tree", vector.Tree))
		if err != nil {
			t.Fatalf("%s: decode the published ratchet tree: %v", label, err)
		}
		occupied := tree.NonBlankLeaves()
		if len(occupied) == 0 {
			t.Fatalf("%s: a published ratchet tree has no occupied leaf", label)
		}
		leaf := occupied[0]
		path, err := directPathOf(leaf.NodeIndex(), tree.LeafWidth())
		if err != nil {
			t.Fatalf("%s: directPathOf: %v", label, err)
		}
		if err := tree.Blank(leaf.NodeIndex()); err != nil {
			t.Fatalf("%s: Blank(%d): %v", label, leaf.NodeIndex(), err)
		}
		hashes, err := tree.TreeHashes(crypto)
		if err != nil {
			t.Fatalf("%s: TreeHashes: %v", label, err)
		}
		moved := 0
		for x, published := range vector.TreeHashes {
			want := mustDecodeHex(t, fmt.Sprintf("%s node %d hash", label, x), published)
			if !bytes.Equal(hashes[x], want) {
				moved += 1
			}
		}
		// the blanked leaf and every node on its direct path, and nothing else: a subtree that
		// does not contain the leaf cannot have moved, so a control reporting anything but this
		// is comparing the wrong entries
		if want := 1 + len(path); moved != want {
			t.Errorf("%s: blanking leaf %d moved %d of the published node hashes, want %d -- the leaf itself and the %d nodes of its direct path",
				label, leaf, moved, want, len(path))
		}
		checked += 1
	}
	if checked != treeValidationImplementedEntries {
		t.Errorf("the control ran over %d entries, want %d", checked, treeValidationImplementedEntries)
	}
}

// ---------------------------------------------------------------------------
// the original tree hash, RFC 9420 section 7.9
// ---------------------------------------------------------------------------

// originalTreeHashReference builds the tree section 7.9 DESCRIBES -- the one the excluded
// leaves were never added to -- and hashes it through the ordinary section 7.8 walk.
//
// treeHash folds that construction into the same recursion, on purpose and for the reason its
// doc gives, and the cost of folding it is that the exclude arm has no second opinion
// anywhere: a test that asked the recursion to check itself would agree with any reading of
// it. This is that second opinion, and it is worth what it is because the arm it compares
// against -- exclude nil -- is the one the 908 published node hashes at the end of this file
// check. So a defect in the shared encoder fails there, and a defect in what exclude does to
// the tree fails here.
//
// Every excluded leaf is BLANKED and every parent node's unmerged_leaves has those leaves
// struck out, both through the container's own surface, so what is hashed is a tree any other
// caller could have built.
func originalTreeHashReference(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	exclude map[LeafIndex]bool) []byte {
	t.Helper()
	without := treeWithoutTheExcludedLeaves(t, tree, exclude)
	root, err := rootOf(without.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}
	hash, err := without.treeHash(crypto, root, nil)
	if err != nil {
		t.Fatalf("the reference tree hash: %v", err)
	}
	return hash
}

// treeWithoutTheExcludedLeaves is the construction half of the reference above, split out
// because section 7.9's original tree hash is asked for over a SUBTREE and not only over the
// whole tree: ParentHash takes it of the copath child.
//
// It is the same second opinion either way -- the tree the excluded leaves were never added to,
// built through the container's own surface -- and splitting it is what stops the parent hash
// preimage test below from needing a second copy of it.
func treeWithoutTheExcludedLeaves(t *testing.T, tree *RatchetTree,
	exclude map[LeafIndex]bool) *RatchetTree {
	t.Helper()
	without := tree.Clone()
	for i := uint32(0); i < uint32(without.LeafWidth()); i += 1 {
		if !exclude[LeafIndex(i)] {
			continue
		}
		if without.Leaf(LeafIndex(i)) == nil {
			t.Fatalf("leaf %d is already blank in the tree this reference is built from, so excluding it is not a change", i)
		}
		if err := without.Blank(LeafIndex(i).NodeIndex()); err != nil {
			t.Fatalf("Blank(leaf %d): %v", i, err)
		}
	}
	for x := uint32(0); x < without.NodeWidth(); x += 1 {
		parent := without.ParentAt(NodeIndex(x))
		if parent == nil {
			continue
		}
		kept := []LeafIndex{}
		for _, leaf := range parent.UnmergedLeaves {
			if !exclude[leaf] {
				kept = append(kept, leaf)
			}
		}
		filtered := parent.Clone()
		filtered.UnmergedLeaves = kept
		if err := without.SetParent(NodeIndex(x), filtered); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	return without
}

// TestTheOriginalTreeHashIsTheTreeHashOfTheTreeWithoutThoseLeaves is section 7.9's definition,
// over EVERY subset of a four leaf tree's leaves rather than over a subset somebody picked.
//
// The subsets are derived from the width, which is what puts all three arms of the exclusion
// under assertion at once: a subset naming a leaf under the root's left child and one under
// its right says the set reaches both descendants, a subset naming a leaf that appears in a
// parent's unmerged_leaves says the list is filtered, and the EMPTY subset says a non nil
// exclusion naming nobody changes nothing -- which is the row that fails when the filter keeps
// what it should drop.
//
// All three arms shipped with no behavioural assertion at all, and three separate mutations of
// them survived the whole package: inverting the unmerged filter, dropping the blanking of an
// excluded leaf, and passing nil to the two recursive calls so the exclusion never reached a
// descendant. Each of the three fails here now.
func TestTheOriginalTreeHashIsTheTreeHashOfTheTreeWithoutThoseLeaves(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	// unmerged lists at BOTH depths and at the root: an exclusion that reached only the node it
	// was handed would still filter the root's own list, so a tree whose only unmerged entries
	// were at the root could not tell the two readings apart.
	for _, one := range []struct {
		x        NodeIndex
		unmerged []LeafIndex
	}{
		{NodeIndex(1), []LeafIndex{0, 1}},
		{NodeIndex(5), []LeafIndex{3}},
		{NodeIndex(3), []LeafIndex{1, 2, 3}},
	} {
		if err := tree.SetParent(one.x, &ParentNode{
			EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{byte(one.x)}, 32)),
			UnmergedLeaves: one.unmerged,
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", one.x, err)
		}
	}
	plain, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}
	width := uint32(tree.LeafWidth())
	if width != 4 {
		t.Fatalf("this sweep is written for a four leaf tree and the builder handed back %d leaves", width)
	}
	answers := map[string]string{}
	for bits := uint32(0); bits < 1<<width; bits += 1 {
		exclude := map[LeafIndex]bool{}
		named := []LeafIndex{}
		for i := uint32(0); i < width; i += 1 {
			if bits&(1<<i) != 0 {
				exclude[LeafIndex(i)] = true
				named = append(named, LeafIndex(i))
			}
		}
		got, err := tree.treeHash(crypto, root, exclude)
		if err != nil {
			t.Fatalf("excluding %v: %v", named, err)
		}
		want := originalTreeHashReference(t, crypto, tree, exclude)
		if !bytes.Equal(got, want) {
			t.Errorf("the original tree hash excluding %v is %x, and the tree those leaves were never added to hashes to %x",
				named, got, want)
		}
		// the two controls that stop sixteen rows from being sixteen readings of one answer: an
		// exclusion naming nobody must leave the hash where it was, and every other one must
		// move it, or the reference is agreeing with the implementation about a change neither
		// of them made.
		if len(named) == 0 {
			if !bytes.Equal(got, plain) {
				t.Errorf("an exclusion naming no leaf answers %x and the ordinary tree hash is %x", got, plain)
			}
		} else if bytes.Equal(got, plain) {
			t.Errorf("excluding %v leaves the tree hash at the ordinary %x", named, plain)
		}
		if prior, collided := answers[string(got)]; collided {
			t.Errorf("excluding %v hashes the same as excluding %s", named, prior)
		}
		answers[string(got)] = fmt.Sprintf("%v", named)
	}
	if len(answers) != 1<<width {
		t.Errorf("%d of the %d subsets of the leaves produced a distinct hash", len(answers), 1<<width)
	}
}

// TestTheOriginalTreeHashOfAnExcludedLeafIsTheHandDerivedBlankGolden pins what the exclusion
// does to a leaf as BYTES rather than as a difference.
//
// The sweep above holds the exclude arm against the nil arm, which is the right second opinion
// about what exclusion MEANS and says nothing about the octets either arm writes. This says the
// octets: an excluded leaf is hashed exactly as a blank one is -- node type, index, presence
// octet zero -- and the digest it must reach is one this repository did not compute. The tree
// is chosen so the exclusion is the only thing that can turn it into the all blank tree those
// goldens were derived for.
func TestTheOriginalTreeHashOfAnExcludedLeafIsTheHandDerivedBlankGolden(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 2)
	for _, x := range []NodeIndex{1, 2} {
		if err := tree.Blank(x); err != nil {
			t.Fatalf("Blank(%d): %v", x, err)
		}
	}
	if tree.Leaf(LeafIndex(0)) == nil {
		t.Fatalf("leaf 0 is blank before the exclusion, so nothing here could see one")
	}
	plain, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if got := fmt.Sprintf("%x", plain); got == blankTwoLeafTreeRootHash {
		t.Fatalf("the tree hashes to the all blank golden with nobody excluded, so this test cannot fail")
	}
	exclude := map[LeafIndex]bool{0: true}
	for _, one := range []struct {
		x    NodeIndex
		want string
		what string
	}{
		{LeafIndex(0).NodeIndex(), blankTwoLeafTreeLeafZeroHash, "the excluded leaf itself"},
		{NodeIndex(1), blankTwoLeafTreeRootHash, "the root above it"},
	} {
		got, err := tree.treeHash(crypto, one.x, exclude)
		if err != nil {
			t.Fatalf("%s: treeHash(%d): %v", one.what, one.x, err)
		}
		if fmt.Sprintf("%x", got) != one.want {
			t.Errorf("%s hashes to %x with leaf 0 excluded, want %s -- an excluded leaf is written as an ABSENT one: node_type, index, zero presence octet",
				one.what, got, one.want)
		}
	}
	// the walk READS the tree and never edits it: the exclusion is a question asked about a
	// live tree during a parent hash check, and a caller left holding a tree the question had
	// blanked would commit it.
	after, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash after the excluded walk: %v", err)
	}
	if !bytes.Equal(after, plain) {
		t.Errorf("the tree hash moved from %x to %x across an original tree hash walk, so the walk wrote into the tree it read", plain, after)
	}
}

// TestTheOriginalTreeHashStrikesTheExcludedLeafOutOfUnmergedLeaves is the same statement for
// the parent arm, and it is the one the sweep above cannot make in bytes: the filtered list is
// what is hashed, as the shorter vector, rather than the list the tree still holds.
//
// The golden is the one TestAParentNodeHashesInTheHandDerivedOrder spells octet by octet --
// unmerged_leaves = [1] over two blank leaves -- reached here from a tree whose list is [0, 1]
// by excluding leaf 0. So a filter that kept the excluded entry, or struck out the wrong one,
// hashes a four octet vector of the other index and fails against a digest computed outside
// this repository.
func TestTheOriginalTreeHashStrikesTheExcludedLeafOutOfUnmergedLeaves(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree := twoLeafBlankTree(t, crypto)
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x01}, 32)),
		UnmergedLeaves: []LeafIndex{0, 1},
	}); err != nil {
		t.Fatalf("SetParent(1): %v", err)
	}
	plain, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if got := fmt.Sprintf("%x", plain); got == parentTwoLeafTreeRootHash {
		t.Fatalf("the unfiltered tree already hashes to the golden the filtered one must reach")
	}
	got, err := tree.treeHash(crypto, NodeIndex(1), map[LeafIndex]bool{0: true})
	if err != nil {
		t.Fatalf("treeHash(1, {0}): %v", err)
	}
	if fmt.Sprintf("%x", got) != parentTwoLeafTreeRootHash {
		t.Errorf("with leaf 0 excluded the root hashes to %x, want %s -- unmerged_leaves is [1] and not [0], [0 1] or []",
			got, parentTwoLeafTreeRootHash)
	}
	// and the tree's own list is untouched, which is what the Clone in the filter is for: this
	// walk runs over a live tree, and a filter applied in place would leave the caller holding a
	// tree the exclusion had eaten.
	parent := tree.ParentAt(NodeIndex(1))
	if parent == nil {
		t.Fatalf("the parent node is gone after the excluded walk")
	}
	if !slices.Equal(parent.UnmergedLeaves, []LeafIndex{0, 1}) {
		t.Errorf("the tree's own unmerged_leaves is %v after the excluded walk, want [0 1]", parent.UnmergedLeaves)
	}
}

// TestTheTreeHashEntryPointsRefuseATreeWithNoLeavesAlike closes a disagreement between the two
// entry points that nothing in this package asked about.
//
// TreeHash asks rootOf, which refuses a leaf count of zero, so a zero valued RatchetTree
// answers ErrTreeMalformed. TreeHashes used to allocate a column of NodeWidth entries and never
// enter the loop, so the SAME receiver answered an empty slice and a nil error. A parent hash
// check reads that column, and a caller that trusted the error would read "this tree has no
// nodes" out of a tree that is not a tree.
func TestTheTreeHashEntryPointsRefuseATreeWithNoLeavesAlike(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	empty := &RatchetTree{}
	if empty.NodeWidth() != 0 || empty.LeafWidth() != 0 {
		t.Fatalf("a zero valued tree is %d nodes and %d leaves wide, so it is not the receiver this test is about",
			empty.NodeWidth(), empty.LeafWidth())
	}
	whole, wholeErr := empty.TreeHash(crypto)
	column, columnErr := empty.TreeHashes(crypto)
	if !errors.Is(wholeErr, ErrTreeMalformed) {
		t.Errorf("TreeHash of a tree with no leaves answered %v, want %v", wholeErr, ErrTreeMalformed)
	}
	if !errors.Is(columnErr, ErrTreeMalformed) {
		t.Errorf("TreeHashes of a tree with no leaves answered %v and the column %v, want %v",
			columnErr, column, ErrTreeMalformed)
	}
	if whole != nil || column != nil {
		t.Errorf("a refusal handed back a hash %x and a column of %d entries", whole, len(column))
	}
	// and the one leaf tree, which IS the narrowest tree there is, still answers both: a guard
	// that refused every tree would satisfy the two clauses above and nothing else.
	one := NewRatchetTree()
	if _, err := one.TreeHash(crypto); err != nil {
		t.Errorf("TreeHash of the one leaf tree: %v", err)
	}
	hashes, err := one.TreeHashes(crypto)
	if err != nil {
		t.Errorf("TreeHashes of the one leaf tree: %v", err)
	}
	if uint32(len(hashes)) != one.NodeWidth() {
		t.Errorf("TreeHashes of the one leaf tree is %d entries over a %d node tree", len(hashes), one.NodeWidth())
	}
}

// ---------------------------------------------------------------------------
// RFC 9420 section 7.9, the ParentHashInput preimage and the field a node carries
// ---------------------------------------------------------------------------

// TestTheParentHashIsTheProvidersHashOfTheHandDerivedParentHashInput writes section 7.9's
// preimage out field by field and takes its digest through the provider, which are two claims
// nothing else in this file makes and the corpus makes only together.
//
// The ORDER first. ParentHashInput is encryption_key, then parent_hash, then
// original_sibling_tree_hash, and an implementation that wrote the sibling hash before the
// parent hash agrees with itself forever, chains forever, and forks from everybody else. What
// sees that today is the published tree-validation corpus alone, and the corpus reaches this
// preimage through this package's own decoder; octets written out here do not.
//
// The DIGEST second, and that half is the one the provider gates cannot reach.
// TestEveryMethodHandedAProviderRoutesThroughIt compares ParentHash's answer over the real
// provider against its answer over one that flips every byte, and those two differ whatever the
// final hash call is, because the preimage already carries a sibling tree hash that moved. So a
// body that hashed this preimage with crypto/sha256 directly passes that gate -- measured, not
// supposed: the final crypto.Hash call replaced by sha256.Sum256 left the routing gate green.
// Under the tagging provider the expected value below is THAT provider's hash of the same
// octets, and sha256 of them is not it.
//
// The original sibling tree hash is built by treeWithoutTheExcludedLeaves and hashed through
// NodeTreeHash -- the entry point the 908 published node hashes check -- so the preimage is
// assembled out of a construction the corpus agrees with, rather than out of the exclusion arm
// this test would otherwise be checking against itself.
func TestTheParentHashIsTheProvidersHashOfTheHandDerivedParentHashInput(t *testing.T) {
	plain := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, plain, 4)
	const p = NodeIndex(3)
	// all three fields of the preimage non-empty, and an unmerged leaf under EACH child, so
	// neither copath child's ORIGINAL tree hash is the plain tree hash of the same subtree and
	// an implementation that skipped the blanking fails on both rows rather than on neither
	if err := tree.SetParent(p, &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0xB3}, 32)),
		ParentHash:     bytes.Repeat([]byte{0xB4}, 32),
		UnmergedLeaves: []LeafIndex{1, 2},
	}); err != nil {
		t.Fatalf("SetParent(%d): %v", p, err)
	}
	node := tree.ParentAt(p)
	exclude := map[LeafIndex]bool{}
	for _, leaf := range node.UnmergedLeaves {
		exclude[leaf] = true
	}
	without := treeWithoutTheExcludedLeaves(t, tree, exclude)
	// the two copath children are DERIVED from the index rather than typed, for the reason
	// errCopathChildIsNotAChildOfTheParent gives: a hash taken over the wrong subtree is a well
	// formed digest of the right width, and nothing else in this test would see it
	left, leftOk := leftOf(p)
	right, rightOk := rightOf(p)
	if !leftOk || !rightOk {
		t.Fatalf("node %d has no children, so it is not the parent this test is about", p)
	}
	for _, one := range []struct {
		name   string
		crypto CryptoProvider
	}{
		{"the registered suite", plain},
		// a provider whose Hash is not the suite's, which is what a digest taken outside the
		// provider fails against and what the routing differential cannot supply here
		{"a provider whose hash is not sha256", &taggingCryptoProvider{inner: plain}},
	} {
		t.Run(one.name, func(t *testing.T) {
			for _, copath := range []NodeIndex{left, right} {
				sibling, err := without.NodeTreeHash(one.crypto, copath)
				if err != nil {
					t.Fatalf("NodeTreeHash(%d) over the tree the unmerged leaves were never added to: %v",
						copath, err)
				}
				want := one.crypto.Hash(treeHashTestConcat(
					treeHashTestOpaque(t, node.EncryptionKey),
					treeHashTestOpaque(t, node.ParentHash),
					treeHashTestOpaque(t, sibling)))
				got, err := tree.ParentHash(one.crypto, p, copath)
				if err != nil {
					t.Fatalf("ParentHash(%d, %d): %v", p, copath, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("the parent hash of node %d with copath child %d is %x, and this provider's hash of the hand written ParentHashInput is %x",
						p, copath, got, want)
				}
			}
		})
	}
}

// TestOnlyACommitSourcedLeafCarriesAParentHashField is section 7.9.1's "a LeafNode has a
// parent_hash field only when its leaf_node_source is commit", asked of nodeParentHashField.
//
// The class is derived at both ends and typed at neither. leafNodeSources reads every
// LeafNodeSource this package declares off the declarations, and leafNodeVariantPaths -- the
// table leaf_node_test.go holds against the struct AND against those same constants -- is what
// says which source carries the field. A fourth source therefore arrives here already swept and
// already carrying its own expectation.
//
// Every leaf below is handed a parent_hash that is present and 32 octets wide, which is what
// makes this an assertion rather than a restatement of the zero value: the answer has to come
// from the SOURCE. That is not a hypothetical shape. Clone copies the field across a source
// change, so a leaf built in memory out of one that had committed keeps bytes its new source
// does not cover -- and under a non-commit source those bytes are covered by no signature at
// all, because marshalCore writes parent_hash into the signed preimage under the commit arm
// alone.
func TestOnlyACommitSourcedLeafCarriesAParentHashField(t *testing.T) {
	sources := leafNodeSources(t)
	if len(sources) < 2 {
		t.Fatalf("%d leaf node source is declared, and a sweep over fewer than two cannot separate a function that reads the source from one that ignores it",
			len(sources))
	}
	carrying := []LeafNodeSource{}
	for _, source := range sources {
		leaf := &LeafNode{LeafNodeSource: source, ParentHash: bytes.Repeat([]byte{0x7C}, 32)}
		got, carries := nodeParentHashField(&Node{NodeType: NodeTypeLeaf, Leaf: leaf})
		want := slices.Contains(leafNodeVariantPaths[source], "ParentHash")
		if carries != want {
			t.Errorf("a leaf of source %d holding %x is reported as carrying a parent_hash field: %v, want %v",
				uint8(source), leaf.ParentHash, carries, want)
			continue
		}
		if !carries {
			if got != nil {
				t.Errorf("a leaf of source %d carries no parent_hash field and %x came back beside the bool",
					uint8(source), got)
			}
			continue
		}
		carrying = append(carrying, source)
		if !bytes.Equal(got, leaf.ParentHash) {
			t.Errorf("a leaf of source %d answered %x, want its own field %x",
				uint8(source), got, leaf.ParentHash)
		}
	}
	// the derived table held against the RFC rather than against itself: section 7.9.1 gives the
	// field to commit and to no other source, so a table that grew a second carrier would be
	// wrong about the protocol and the sweep above would agree with it.
	if len(carrying) != 1 || carrying[0] != LeafNodeSourceCommit {
		t.Errorf("the sources carrying a parent_hash field are %v of the %d declared, and section 7.9.1 gives it to commit alone",
			carrying, len(sources))
	}
	// a ParentNode always carries the field, the root included, where it is the zero-length
	// octet string. "present and empty" and "absent" are different answers and this is the one
	// place both are spelled.
	for _, one := range []struct {
		name  string
		value []byte
	}{
		{"a parent node below the root", bytes.Repeat([]byte{0x7D}, 32)},
		{"the root, whose parent_hash is the zero-length string", []byte{}},
	} {
		got, carries := nodeParentHashField(&Node{
			NodeType: NodeTypeParent,
			Parent:   &ParentNode{ParentHash: one.value},
		})
		if !carries {
			t.Errorf("%s is reported as carrying no parent_hash field", one.name)
			continue
		}
		if !bytes.Equal(got, one.value) {
			t.Errorf("%s answered %x, want %x", one.name, got, one.value)
		}
	}
	// and a blank position carries nothing, which is what keeps a blank out of the claimant loop
	// rather than into it holding an empty field.
	if got, carries := nodeParentHashField(nil); carries || got != nil {
		t.Errorf("a blank node answered %x and %v, want no field at all", got, carries)
	}
}

// ---------------------------------------------------------------------------
// RFC 9420 section 7.9.2, parent hash validity over the whole tree
// ---------------------------------------------------------------------------
//
// What this check defends is worth stating, because it decides how the tests below are
// weighted. A joiner adopts a ratchet tree somebody else handed it. Every parent node in that
// tree publishes an encryption key that the next commit will seal a path secret to, and the
// only thing that makes such a key trustworthy is that a member signed a leaf whose parent_hash
// chains up to it. A member who can get a forged parent node accepted has substituted their own
// key for somebody else's and reads the group from then on.
//
// So this is a refusal surface, and on this project a refusal surface fails by ACCEPTING
// because it never looked -- p4's ValSem401/402 applied to psks[0] instead of psks[i], p5's own
// erratum 8745 loop applied to element zero. The two sweeps below are aimed at exactly that:
// TestVerifyParentHashesVisitsEveryNonBlankParent invalidates each non-blank parent in turn, at
// positions derived from the tree rather than chosen, so a first-only or last-only loop fails on
// the parents in between; and the corpus test puts 290 non-blank parents from the working
// group's own trees through the accepting direction, so a check that refuses everything fails
// too.
//
// TestVerifyParentHashesRefusesACoTenantInTheResolutionOfTheClaimant is the one that separates
// the RFC's rule from the weaker restatement this task's plan supplied. See tree_hash.go's
// section 7.9.2 comment: the plan asks only whether SOMETHING in the child's resolution carries
// the right hash, and section 7.9.2 additionally requires that the child's resolution hold that
// claimant and NOTHING else beyond the leaves added since the parent was last set. The two trees
// in that test differ in one unmerged_leaves entry and nothing else, one is legal and one is
// forged, and the plan's version accepts both.

// siblingUnder is the child of parent that is NOT the given one -- section 7.9's S, the copath
// child. It refuses rather than guessing, because a wrong answer here would silently build a
// fixture whose "valid" chain is a hash over the wrong subtree, which every assertion below
// would then agree with.
func siblingUnder(t *testing.T, parent NodeIndex, child NodeIndex) NodeIndex {
	t.Helper()
	left, leftOk := leftOf(parent)
	right, rightOk := rightOf(parent)
	if !leftOk || !rightOk {
		t.Fatalf("node %d has no children, so it is not a parent", parent)
	}
	switch child {
	case left:
		return right
	case right:
		return left
	}
	t.Fatalf("node %d is not a child of node %d", child, parent)
	return 0
}

// chainedTestTree answers an n leaf tree carrying one valid section 7.9.1 parent hash chain,
// from the committer's leaf all the way to the root, and the chain's parent nodes leaf-first.
//
// This is the shape a tree has right after one member has committed with an UpdatePath, built by
// hand because UpdatePath itself has not landed. Every parent on the committer's direct path is
// occupied and every other parent stays blank, which is what makes it a fixture rather than a
// re-implementation: the chain is written from section 7.9.1's sentence -- "the value of the
// parent_hash field is the parent hash of the next non-blank parent node above the node in
// question" -- and not from tree_hash.go's verifier.
//
// The fill is TOP DOWN and has to be. The parent hash of a node is taken over that node's own
// parent_hash field, so the root's must be final (the zero-length octet string, section 7.9)
// before the node below it can be computed, and so on down. Filling bottom up would hash
// placeholder fields and produce a chain that is stable, self-consistent and invalid.
func chainedTestTree(t *testing.T, crypto CryptoProvider, n uint32,
	committer LeafIndex) (*RatchetTree, []*testTreeMember, []NodeIndex) {
	t.Helper()
	tree, members := newTestTree(t, crypto, n)
	leafNode := committer.NodeIndex()
	chain, err := directPathOf(leafNode, tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf(%d): %v", leafNode, err)
	}
	if len(chain) == 0 {
		t.Fatalf("leaf %d has no parents in a %d leaf tree, so there is no chain to build", committer, n)
	}
	// every parent on the path gets a key of its own first, because a parent hash reads the
	// encryption_key of the node above and that node has to be final before it is hashed.
	for _, p := range chain {
		parent := &ParentNode{
			EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{byte(p)}, 32)),
			ParentHash:    []byte{},
		}
		if err := tree.SetParent(p, parent); err != nil {
			t.Fatalf("SetParent(%d): %v", p, err)
		}
	}
	// then the parent_hash fields, root downward. chain[len-1] is the root and keeps the
	// zero-length field it was installed with.
	below := append([]NodeIndex{leafNode}, chain[:len(chain)-1]...)
	for i := len(chain) - 1; i >= 1; i -= 1 {
		hash, err := tree.ParentHash(crypto, chain[i], siblingUnder(t, chain[i], below[i]))
		if err != nil {
			t.Fatalf("ParentHash(%d, sibling of %d): %v", chain[i], below[i], err)
		}
		child := tree.ParentAt(chain[i-1]).Clone()
		child.ParentHash = hash
		if err := tree.SetParent(chain[i-1], child); err != nil {
			t.Fatalf("SetParent(%d): %v", chain[i-1], err)
		}
	}
	// and the committer's own leaf, which is the bottom of the chain and the only signed link
	// in it.
	hash, err := tree.ParentHash(crypto, chain[0], siblingUnder(t, chain[0], leafNode))
	if err != nil {
		t.Fatalf("ParentHash(%d, sibling of %d): %v", chain[0], leafNode, err)
	}
	leaf := tree.Leaf(committer).Clone()
	leaf.LeafNodeSource = LeafNodeSourceCommit
	leaf.ParentHash = hash
	if err := leaf.Sign(crypto, members[committer].SignaturePriv, testGroupId(), committer); err != nil {
		t.Fatalf("Sign(%d): %v", committer, err)
	}
	if err := tree.SetLeaf(committer, leaf); err != nil {
		t.Fatalf("SetLeaf(%d): %v", committer, err)
	}
	return tree, members, chain
}

// nonBlankParentsOf reads the class the verifier must sweep off the TREE, at every odd index,
// rather than trusting the caller's idea of where it put parents. A fixture that quietly stopped
// installing one would otherwise shrink the sweep below and still report a clean run.
func nonBlankParentsOf(tree *RatchetTree) []NodeIndex {
	found := []NodeIndex{}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if tree.ParentAt(NodeIndex(x)) != nil {
			found = append(found, NodeIndex(x))
		}
	}
	return found
}

// installs a hand-built two-node chain: parent 1 with a child leaf 0 whose parent_hash field is
// the parent hash of node 1 with copath child 2. Node 1 is the root of a two leaf tree, so its
// own parent_hash field is the zero-length string.
func buildParentHashChain(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
	t.Helper()
	tree, members := newTestTree(t, crypto, 2)
	parent := &ParentNode{
		EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{0xE0}, 32)),
		ParentHash:    []byte{},
	}
	if err := tree.SetParent(NodeIndex(1), parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	hash, err := tree.ParentHash(crypto, NodeIndex(1), NodeIndex(2))
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	leaf := tree.Leaf(LeafIndex(0)).Clone()
	leaf.LeafNodeSource = LeafNodeSourceCommit
	leaf.ParentHash = hash
	if err := leaf.Sign(crypto, members[0].SignaturePriv, testGroupId(), LeafIndex(0)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := tree.SetLeaf(LeafIndex(0), leaf); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	return tree, members
}

func TestVerifyParentHashesAcceptsAllBlankParents(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	for _, n := range []uint32{1, 2, 3, 7, 8} {
		tree, _ := newTestTree(t, crypto, n)
		// the fixture's claim, asked rather than assumed: this really is the all-blank shape, so
		// a pass below is the trivially-valid rule and not a chain that happened to be built.
		if parents := nonBlankParentsOf(tree); len(parents) != 0 {
			t.Fatalf("n=%d: the builder left parents %v occupied", n, parents)
		}
		if err := tree.VerifyParentHashes(crypto); err != nil {
			t.Fatalf("n=%d VerifyParentHashes: %v", n, err)
		}
	}
}

func TestVerifyParentHashesAcceptsAValidChain(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := buildParentHashChain(t, crypto)
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("VerifyParentHashes: %v", err)
	}
}

func TestVerifyParentHashesRejectsATamperedChain(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := buildParentHashChain(t, crypto)
	parent := tree.ParentAt(NodeIndex(1)).Clone()
	parent.EncryptionKey[0] ^= 0xFF
	if err := tree.SetParent(NodeIndex(1), parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := tree.VerifyParentHashes(crypto); !errors.Is(err, ErrParentHashMismatch) {
		t.Fatalf("err = %v, want ErrParentHashMismatch", err)
	}
}

// giveTheLeafACommitParentHash rewrites one leaf of a fixture as a commit-sourced leaf carrying
// the given parent_hash, signed at its own index, which is what the tests below vary.
//
// It takes the leaf INDEX rather than being written against leaf 1, because the same edit is
// what makes a second claimant, a stale claimant and no claimant at all, and those tests sit on
// different leaves of different fixtures.
func giveTheLeafACommitParentHash(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	members []*testTreeMember, i LeafIndex, hash []byte) {
	t.Helper()
	leaf := tree.Leaf(i).Clone()
	leaf.LeafNodeSource = LeafNodeSourceCommit
	leaf.ParentHash = hash
	if err := leaf.Sign(crypto, members[i].SignaturePriv, testGroupId(), i); err != nil {
		t.Fatalf("Sign(%d): %v", i, err)
	}
	if err := tree.SetLeaf(i, leaf); err != nil {
		t.Fatalf("SetLeaf(%d): %v", i, err)
	}
}

// TestVerifyParentHashesFollowsTheClaimToWhicheverChildHoldsIt replaces the plan's
// "rejects both children claiming the same parent", which asserts an outcome no implementation
// of section 7.9.2 can produce.
//
// The plan's fixture gives leaf 0 the parent hash of node 1 with copath child 2, then gives
// leaf 1 the mirrored one and expects a refusal for two claimants. But writing leaf 1 CHANGES
// the tree hash of node 2's subtree, which is the sibling hash inside the value leaf 0 is
// holding, so leaf 0's claim dies in the same edit that creates leaf 1's. The tree that results
// has exactly one claimant, and the correct answer for it is to accept -- which is also what the
// plan's own two-armed implementation answers, so the plan's test fails against the code the
// plan supplies.
//
// It is not a fixture that needs more care, it is unconstructible. Two claimants require the
// left one to store a hash over the right subtree while the right one stores a hash over the
// left subtree, and each claimant's stored value is inside the subtree the other one's value
// is a hash of. Section 7.9.2's "exactly one" therefore has a reachable lower half and an
// upper half that a second preimage would be needed to reach; the lower half is
// TestVerifyParentHashesRefusesAParentNoDescendantClaims below, and this test holds the part of
// the plan's fixture that IS a real property -- that the check follows the live claim to
// whichever side of the tree now holds it, rather than insisting on the side it used to be on.
func TestVerifyParentHashesFollowsTheClaimToWhicheverChildHoldsIt(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := buildParentHashChain(t, crypto)
	stale, carries := nodeParentHashField(tree.Get(LeafIndex(0).NodeIndex()))
	if !carries {
		t.Fatal("the fixture's leaf 0 carries no parent_hash field")
	}
	mirrored, err := tree.ParentHash(crypto, NodeIndex(1), NodeIndex(0))
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	giveTheLeafACommitParentHash(t, crypto, tree, members, LeafIndex(1), mirrored)

	// the claim really has moved, observed on both sides rather than assumed: leaf 1 now holds
	// the live value and leaf 0's is stale, because the sibling hash it was taken over moved.
	live, err := tree.ParentHash(crypto, NodeIndex(1), NodeIndex(2))
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	if bytes.Equal(stale, live) {
		t.Fatal("leaf 0's parent hash survived leaf 1 being rewritten, so this tree has two claimants and the test below is not the one that is needed")
	}
	held, carries := nodeParentHashField(tree.Get(LeafIndex(1).NodeIndex()))
	if !carries || !bytes.Equal(held, mirrored) {
		t.Fatalf("leaf 1 carries %x (present: %v), want the mirrored parent hash %x", held, carries, mirrored)
	}
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("VerifyParentHashes: %v", err)
	}
}

// TestVerifyParentHashesRefusesAParentNoDescendantClaims is the reachable half of section
// 7.9.2's "exactly one descendant".
//
// A parent node no descendant claims is one that was never written by an UpdatePath -- it is a
// key somebody inserted into the tree, which is the whole thing this check exists to refuse.
// Here leaf 1 is rewritten as a commit leaf carrying a parent hash of nothing, which both kills
// leaf 0's claim (the sibling hash under it moved) and fails to create one of its own.
func TestVerifyParentHashesRefusesAParentNoDescendantClaims(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := buildParentHashChain(t, crypto)
	giveTheLeafACommitParentHash(t, crypto, tree, members, LeafIndex(1),
		bytes.Repeat([]byte{0xAB}, crypto.HashSize()))

	// neither child holds the value its side would have to hold, asked of the tree before the
	// refusal is asked for, so a pass cannot be some other refusal wearing this test's name.
	for _, one := range []struct {
		claimant LeafIndex
		copath   NodeIndex
	}{
		{LeafIndex(0), NodeIndex(2)},
		{LeafIndex(1), NodeIndex(0)},
	} {
		want, err := tree.ParentHash(crypto, NodeIndex(1), one.copath)
		if err != nil {
			t.Fatalf("ParentHash: %v", err)
		}
		held, carries := nodeParentHashField(tree.Get(one.claimant.NodeIndex()))
		if carries && bytes.Equal(held, want) {
			t.Fatalf("leaf %d still claims node 1, so this tree has a claimant and is not the one this test is about", one.claimant)
		}
	}
	if err := tree.VerifyParentHashes(crypto); !errors.Is(err, ErrParentHashMismatch) {
		t.Fatalf("err = %v, want ErrParentHashMismatch", err)
	}
}

// TestVerifyParentHashesVisitsEveryNonBlankParent is the sweep this task exists for.
//
// The failure mode being tested for is a loop that evaluates its check against one element and
// reports on the whole collection -- which has landed twice on this plan already, both times
// against element zero, both times with the suite green. So the assertion is not "an invalid
// tree is refused" but "an invalid tree is refused NO MATTER WHICH parent is the invalid one",
// over positions read out of the tree rather than chosen here.
//
// Corrupting one parent's encryption_key invalidates that parent ALONE: the node's own child
// carries the parent hash taken over the old key and stops matching, while the node's stored
// parent_hash field is untouched, so the parent above it still has its one claimant. That is
// what makes the error's node number a sound observation of which parent the sweep was looking
// at when it refused -- and it is asserted rather than argued, because a corruption that
// happened to break a second parent would let a first-only loop pass this test by reporting the
// wrong node.
func TestVerifyParentHashesVisitsEveryNonBlankParent(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _, chain := chainedTestTree(t, crypto, 16, LeafIndex(0))
	parents := nonBlankParentsOf(tree)
	if !slices.Equal(parents, chain) {
		t.Fatalf("the tree carries parents %v and the chain built was %v", parents, chain)
	}
	// three is the floor at which "first only" and "last only" are both distinguishable from
	// "every", since a middle position has to exist for them to miss.
	if len(parents) < 3 {
		t.Fatalf("the fixture has %d non-blank parents, and a sweep over fewer than 3 cannot tell a whole-collection loop from a first-only or last-only one", len(parents))
	}
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("the untampered chain: %v", err)
	}
	for _, p := range parents {
		sound := tree.ParentAt(p).Clone()
		tampered := sound.Clone()
		tampered.EncryptionKey[0] ^= 0xFF
		if err := tree.SetParent(p, tampered); err != nil {
			t.Fatalf("SetParent(%d): %v", p, err)
		}
		err := tree.VerifyParentHashes(crypto)
		if !errors.Is(err, ErrParentHashMismatch) {
			t.Errorf("node %d was given a key nothing in the tree chains to and VerifyParentHashes answered %v, want ErrParentHashMismatch",
				p, err)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("node %d ", p)) {
			// the refusal has to be ABOUT this node. without this, a sweep that always reported
			// the first parent would pass every row above.
			t.Errorf("node %d was tampered with and the refusal names another node: %v", p, err)
		}
		if err := tree.SetParent(p, sound); err != nil {
			t.Fatalf("SetParent(%d) restoring: %v", p, err)
		}
		if err := tree.VerifyParentHashes(crypto); err != nil {
			t.Fatalf("restoring node %d did not restore the tree: %v", p, err)
		}
	}
}

// TestVerifyParentHashesAcceptsAChainDescendingThroughEitherChild holds the disjunction in
// section 7.9.2.
//
// C is "the child that is on the direct path of D", so which of a parent's two children carries
// the claimant depends on where the committer sits, and a verifier that looked at one child
// refuses every tree committed from the other side of the group -- silently, since it refuses
// with the same error a forgery gets. The leftmost leaf's direct path enters every parent from
// the left and the rightmost leaf's from the right, and that structural claim is asserted here
// rather than assumed, so the test cannot go quiet by building two chains on the same side.
func TestVerifyParentHashesAcceptsAChainDescendingThroughEitherChild(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	const leaves = 16
	for _, one := range []struct {
		name      string
		committer LeafIndex
		wantArm   func(NodeIndex) (NodeIndex, bool)
	}{
		{"the leftmost leaf enters every parent from the left", LeafIndex(0), leftOf},
		{"the rightmost leaf enters every parent from the right", LeafIndex(leaves - 1), rightOf},
	} {
		t.Run(one.name, func(t *testing.T) {
			tree, _, chain := chainedTestTree(t, crypto, leaves, one.committer)
			below := append([]NodeIndex{one.committer.NodeIndex()}, chain[:len(chain)-1]...)
			for i, p := range chain {
				arm, ok := one.wantArm(p)
				if !ok {
					t.Fatalf("node %d has no children", p)
				}
				if arm != below[i] {
					t.Fatalf("the claimant under node %d is %d, which is not the arm this row is about (%d)",
						p, below[i], arm)
				}
			}
			if err := tree.VerifyParentHashes(crypto); err != nil {
				t.Fatalf("VerifyParentHashes: %v", err)
			}
		})
	}
}

// TestVerifyParentHashesRefusesACoTenantInTheResolutionOfTheClaimant is section 7.9.2's third
// condition, and the assertion that separates the RFC's rule from this task's plan.
//
// Two four-leaf trees that differ in ONE unmerged_leaves entry and in nothing else at all -- the
// same keys, the same blanks, the same signed leaf, the same parent hash bytes. Node 3 is the
// root and node 1 is blank, so the resolution of node 3's left child is {leaf 0, leaf 1}. Leaf 0
// is the claimant and carries the right hash in both trees. Whether leaf 1 is ALLOWED to be
// sitting there beside it is exactly the question condition 3 asks: it is allowed if it was
// added since node 3 was last set, which is what node 3 recording it in unmerged_leaves means,
// and it is a forgery otherwise.
//
// The plan's version of this check accepts both of these trees, because it asks only whether
// something in the resolution carries the hash. The bytes of leaf 0's parent_hash field are
// asserted equal across the pair so that the refusal below cannot be coming from condition 2.
func TestVerifyParentHashesRefusesACoTenantInTheResolutionOfTheClaimant(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	const root, blankChild, copath = NodeIndex(3), NodeIndex(1), NodeIndex(5)

	// one tree, then a CLONE of it with a single field changed. newTestTree draws fresh keys on
	// every call, so a pair built by calling it twice would differ in every leaf and the
	// comparison below would be measuring the fixture rather than the rule.
	legal, members := newTestTree(t, crypto, 4)
	parent := &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0xC0}, 32)),
		ParentHash:     []byte{},
		UnmergedLeaves: []LeafIndex{1},
	}
	if err := legal.SetParent(root, parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	hash, err := legal.ParentHash(crypto, root, copath)
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	leaf := legal.Leaf(LeafIndex(0)).Clone()
	leaf.LeafNodeSource = LeafNodeSourceCommit
	leaf.ParentHash = hash
	if err := leaf.Sign(crypto, members[0].SignaturePriv, testGroupId(), LeafIndex(0)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := legal.SetLeaf(LeafIndex(0), leaf); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	// the shape the whole test rests on, asked of the tree rather than assumed of it: node 1
	// stays blank, so the claimant is not alone in the resolution below node 3.
	if !legal.IsBlank(blankChild) {
		t.Fatalf("node %d is occupied, so the resolution below node %d is just that node", blankChild, root)
	}
	if resolution := legal.Resolution(blankChild); len(resolution) != 2 {
		t.Fatalf("the resolution below node %d is %v, and this test needs a claimant and one co-tenant",
			blankChild, resolution)
	}
	// leaf 1 recorded as added since node 3 was last set: legal.
	if err := legal.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("the tree that records its unmerged leaf: %v", err)
	}

	// the same tree with that record removed, and nothing else touched: leaf 1 is now an
	// unaccounted-for node sharing the resolution with the claimant, which is the splice
	// condition 3 exists to refuse.
	forged := legal.Clone()
	stripped := forged.ParentAt(root).Clone()
	stripped.UnmergedLeaves = nil
	if err := forged.SetParent(root, stripped); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	forgedHash, err := forged.ParentHash(crypto, root, copath)
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	// unmerged_leaves reaches a parent hash only through the sibling's ORIGINAL tree hash, and
	// leaf 1 is not under the sibling, so the value the claimant has to hold is untouched. Said
	// as an assertion because it is what makes the refusal below condition 3's and not
	// condition 2's.
	if !bytes.Equal(hash, forgedHash) {
		t.Fatalf("the pair does not differ in unmerged_leaves alone: the claimed parent hash moved from %x to %x",
			hash, forgedHash)
	}
	claimant, carries := nodeParentHashField(forged.Get(LeafIndex(0).NodeIndex()))
	if !carries || !bytes.Equal(claimant, forgedHash) {
		t.Fatalf("the forged tree's claimant carries %x (present: %v) and the parent hash is %x, so a refusal would be condition 2's and not condition 3's",
			claimant, carries, forgedHash)
	}
	if err := forged.VerifyParentHashes(crypto); !errors.Is(err, ErrParentHashMismatch) {
		t.Fatalf("err = %v, want ErrParentHashMismatch: the resolution below node %d holds a node that node %d does not account for",
			err, root, root)
	}
}

// What the corpus holds for the two suites this package registers, measured off the vendored
// file: every non-blank parent of every one of those trees, all of which are parent-hash valid.
// Stated so a sweep that decoded nothing, filtered everything out or stopped early fails here
// rather than reporting a clean run over an empty set.
const treeValidationNonBlankParents = 290

// TestVerifyParentHashesAcceptsThePublishedTreeValidationCorpus is the accepting direction, over
// trees this repository did not build.
//
// A refusal surface has two ways to be useless and the tests above only see one of them. These
// 28 trees carry blank parents, occupied parents, blank leaves and unmerged leaves, and every
// one of them is parent-hash valid by construction, so a check that is too STRICT -- one arm
// only, an elementwise comparison where section 7.9.2 states set equality, a condition 3 that
// forgot the unmerged leaves are allowed to be there -- fails here and nowhere else in this
// file, since every fixture above was built by this repository from its own reading.
func TestVerifyParentHashesAcceptsThePublishedTreeValidationCorpus(t *testing.T) {
	entries := LoadVectorFile(t, treeValidationVectorFile)
	if len(entries) != treeValidationEntryCount {
		t.Fatalf("tree-validation entries: %d, want %d", len(entries), treeValidationEntryCount)
	}
	covered := 0
	parents := 0
	for entry, raw := range entries {
		vector := treeHashValidationVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", entry, err)
		}
		suite, implemented := implementedSuite(vector.CipherSuite)
		if !implemented {
			continue
		}
		covered += 1
		label := fmt.Sprintf("tree-validation entry %d", entry)
		crypto := mustProvider(t, suite)
		tree, err := UnmarshalRatchetTree(mustDecodeHex(t, label+" ratchet tree", vector.Tree))
		if err != nil {
			t.Fatalf("%s: decode the published ratchet tree: %v", label, err)
		}
		parents += len(nonBlankParentsOf(tree))
		if err := tree.VerifyParentHashes(crypto); err != nil {
			t.Errorf("%s: VerifyParentHashes: %v", label, err)
		}
	}
	if covered != treeValidationImplementedEntries {
		t.Errorf("%d published entries were covered, want %d; the suite filter or the decode moved",
			covered, treeValidationImplementedEntries)
	}
	if parents != treeValidationNonBlankParents {
		t.Errorf("%d non-blank parents were put through the check, want %d", parents, treeValidationNonBlankParents)
	}
}

// TestVerifyParentHashesRefusesATreeThatIsNotATree keeps the empty receiver out of the accepting
// answer.
//
// A sweep over a tree with no nodes never runs its body, so the natural spelling of this method
// answers nil for a zero valued RatchetTree -- and nil here means "every parent of this tree is
// parent-hash valid", which is the same answer a sound tree gets. TreeHash and TreeHashes were
// given the same refusal at the same spot and for the same reason; this is the third entry point
// and it is the one where an empty answer is read as an authentication having succeeded.
func TestVerifyParentHashesRefusesATreeThatIsNotATree(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	empty := &RatchetTree{}
	if empty.NodeWidth() != 0 || empty.LeafWidth() != 0 {
		t.Fatalf("a zero valued tree is %d nodes and %d leaves wide, so it is not the receiver this test is about",
			empty.NodeWidth(), empty.LeafWidth())
	}
	if err := empty.VerifyParentHashes(crypto); !errors.Is(err, ErrTreeMalformed) {
		t.Errorf("VerifyParentHashes of a tree with no leaves answered %v, want %v", err, ErrTreeMalformed)
	}
	// no provider is its own refusal and not the malformed one, so a caller that passed nil is
	// not sent to look at a tree that is fine.
	one := NewRatchetTree()
	if err := one.VerifyParentHashes(nil); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("VerifyParentHashes without a provider answered %v, want %v", err, ErrNilCryptoProvider)
	}
	// and the one leaf tree still passes: a guard that refused every tree would satisfy both
	// clauses above and nothing else.
	if err := one.VerifyParentHashes(crypto); err != nil {
		t.Errorf("VerifyParentHashes of the one leaf tree: %v", err)
	}
}

// TestVerifyParentHashesRefusesAClaimantWhoseSourceDoesNotCarryAParentHash is section 7.9.1 read
// as a refusal: a leaf that did not enter the tree by committing has no parent_hash field, so it
// cannot be the descendant that claims a parent node.
//
// The forgery this refuses is a real one rather than a tidiness. marshalCore writes parent_hash
// into the bytes a leaf is signed over under the commit arm ALONE, so the field on a
// key_package or update leaf is covered by no signature at all: anybody holding a member's
// signed update leaf can hang the parent hash of their own forged node on it without a signing
// key, and that leaf still verifies at its own index in its own group. Every one of those
// facts is asserted below rather than argued, so the refusal cannot be some other refusal
// wearing this test's name -- the rewritten leaf verifies, and it holds the exact bytes node 1
// answers with copath child 2.
//
// The sources are DERIVED, and which of them is the accepting direction is derived too, off the
// same leafNodeVariantPaths table TestOnlyACommitSourcedLeafCarriesAParentHashField reads. The
// mutation this exists for is nodeParentHashField dropping the source test altogether, which
// left twenty tests green.
func TestVerifyParentHashesRefusesAClaimantWhoseSourceDoesNotCarryAParentHash(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	swept := 0
	for _, source := range leafNodeSources(t) {
		if slices.Contains(leafNodeVariantPaths[source], "ParentHash") {
			// the carrying source is the ACCEPTING direction, which
			// TestVerifyParentHashesAcceptsAValidChain holds over this same fixture
			continue
		}
		swept += 1
		t.Run(fmt.Sprintf("leaf_node_source %d", uint8(source)), func(t *testing.T) {
			tree, members := buildParentHashChain(t, crypto)
			if err := tree.VerifyParentHashes(crypto); err != nil {
				t.Fatalf("the chain before its claimant's source is moved: %v", err)
			}
			claimed, carries := nodeParentHashField(tree.Get(LeafIndex(0).NodeIndex()))
			if !carries {
				t.Fatal("the fixture's claimant carries no parent_hash field, so there is nothing to move the source of")
			}
			// the SOURCE moves and the bytes stay, which is the whole of the edit
			leaf := tree.Leaf(LeafIndex(0)).Clone()
			leaf.LeafNodeSource = source
			leaf.ParentHash = claimed
			if err := leaf.Sign(crypto, members[0].SignaturePriv, testGroupId(), LeafIndex(0)); err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if err := tree.SetLeaf(LeafIndex(0), leaf); err != nil {
				t.Fatalf("SetLeaf: %v", err)
			}
			written := tree.Leaf(LeafIndex(0))
			if err := written.VerifySignature(crypto, testGroupId(), LeafIndex(0)); err != nil {
				t.Fatalf("the rewritten leaf does not verify at its own index, so a refusal below need not be about its source: %v", err)
			}
			live, err := tree.ParentHash(crypto, NodeIndex(1), NodeIndex(2))
			if err != nil {
				t.Fatalf("ParentHash: %v", err)
			}
			if !bytes.Equal(written.ParentHash, live) {
				t.Fatalf("the rewritten leaf holds %x and the parent hash of node 1 with copath child 2 is %x, so a refusal would be condition 2's rather than section 7.9.1's",
					written.ParentHash, live)
			}
			if err := tree.VerifyParentHashes(crypto); !errors.Is(err, ErrParentHashMismatch) {
				t.Fatalf("err = %v, want ErrParentHashMismatch: leaf 0's source is %d, which carries no parent_hash field for it to claim node 1 with",
					err, uint8(source))
			}
		})
	}
	if swept == 0 {
		t.Fatal("every declared leaf node source carries a parent_hash field, so this test swept nothing")
	}
}

// TestConditionThreeRefusesAClaimantThatIsNotInTheResolution drives
// resolutionIsTheClaimantAndTheUnmergedLeaves directly, because the sweep cannot drive it.
//
// Condition 3 is two clauses -- "D is in the resolution of C", and "the intersection of P's
// unmerged_leaves with the subtree under C is equal to the resolution of C with D removed" --
// and the helper implements both. Only the second is reachable from parentHashClaimsUnder,
// which draws its claimant out of the very resolution it then hands in, so the first clause has
// no call site that can make it fire: replacing it with a constant false left the whole package
// green.
//
// The clause is still the helper's contract and the next caller will not be that loop, so what
// holds it is a direct call rather than a deletion. The shape below is the one that separates
// the two clauses: every entry of the resolution is accounted for by an unmerged leaf and
// nothing is left over, so the arithmetic clause has nothing to say and only the claimant clause
// can refuse it. The outsiders are swept off the tree's own width rather than chosen, so the
// claimant clause has to hold for every position that is not in the resolution and not for one
// somebody picked.
func TestConditionThreeRefusesAClaimantThatIsNotInTheResolution(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	const child = NodeIndex(1)
	resolution := tree.Resolution(child)
	if want := []NodeIndex{LeafIndex(0).NodeIndex(), LeafIndex(1).NodeIndex()}; !slices.Equal(resolution, want) {
		t.Fatalf("the resolution below node %d is %v, want %v", child, resolution, want)
	}
	// the accepting control, so a refusal below is the claimant clause rather than a helper that
	// refuses everything: leaf 0 claims, and leaf 1 is recorded as added since the parent was set.
	accepted := &ParentNode{UnmergedLeaves: []LeafIndex{1}}
	if !tree.resolutionIsTheClaimantAndTheUnmergedLeaves(accepted, child, resolution, LeafIndex(0).NodeIndex()) {
		t.Fatal("condition 3 refuses a claimant that IS in the resolution, so nothing below separates its two clauses")
	}
	// and the same arithmetic with the claimant outside it: BOTH entries of the resolution are
	// unmerged leaves now, so the loop consumes the whole set and leaves nothing over.
	whole := &ParentNode{UnmergedLeaves: []LeafIndex{0, 1}}
	outsiders := 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		outsider := NodeIndex(x)
		if slices.Contains(resolution, outsider) {
			continue
		}
		outsiders += 1
		if tree.resolutionIsTheClaimantAndTheUnmergedLeaves(whole, child, resolution, outsider) {
			t.Errorf("node %d is not in the resolution %v of node %d, and condition 3 accepted it as the claimant",
				outsider, resolution, child)
		}
	}
	if outsiders == 0 {
		t.Fatalf("every node of a %d node tree is in the resolution %v, so this sweep ran over nothing",
			tree.NodeWidth(), resolution)
	}
}

// TestVerifyParentHashesRefusesAnUnmergedLeafThatIsNotInTheResolutionOfItsChild is the OTHER
// containment of condition 3's set equality.
//
// Section 7.9.2 says the intersection of P's unmerged_leaves with the subtree under C "is equal
// to" the resolution of C with the claimant removed, and equality is two inclusions.
// TestVerifyParentHashesRefusesACoTenantInTheResolutionOfTheClaimant reaches one of them -- a
// node in the resolution that the unmerged set does not account for -- and nothing reached the
// other: neutering the leftover loop that reports an unmerged leaf with no resolution entry to
// match left twenty tests green.
//
// The pair below differs in one unmerged_leaves entry and nothing else, as that test's pair
// does, and the entry names a leaf that is BLANK. A blank leaf contributes nothing to a
// resolution, so naming it puts it in the unmerged set and out of the resolution at once, which
// is the surplus this half refuses. The claimed parent hash is asserted equal across the pair,
// because unmerged_leaves reaches a parent hash only through the SIBLING's original tree hash
// and this leaf is not under the sibling -- so the refusal is condition 3's and not condition
// 2's.
//
// What a tree like this actually is: a parent whose unmerged list names somebody who is not
// there. It carries no forgery leverage on its own -- padding the list moves no hash the
// claimant has to match -- and it is the same tree the plan's task 22 refuses from the other
// direction, by requiring every unmerged leaf to point at a non-blank descendant. It is refused
// here because half of a stated RFC condition being enforced with nothing observing it is how
// the other half comes to be deleted.
func TestVerifyParentHashesRefusesAnUnmergedLeafThatIsNotInTheResolutionOfItsChild(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	const root, child, copath = NodeIndex(3), NodeIndex(1), NodeIndex(5)
	const absent = LeafIndex(1)

	legal, members := newTestTree(t, crypto, 4)
	if err := legal.Blank(absent.NodeIndex()); err != nil {
		t.Fatalf("Blank(leaf %d): %v", absent, err)
	}
	if err := legal.SetParent(root, &ParentNode{
		EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{0xD0}, 32)),
		ParentHash:    []byte{},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	hash, err := legal.ParentHash(crypto, root, copath)
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	giveTheLeafACommitParentHash(t, crypto, legal, members, LeafIndex(0), hash)
	// the shape, asked of the tree rather than assumed of it: the claimant is alone in the
	// resolution below the child, and the leaf the forgery will name is inside that child's
	// subtree and is not in that resolution.
	if resolution := legal.Resolution(child); !slices.Equal(resolution, []NodeIndex{LeafIndex(0).NodeIndex()}) {
		t.Fatalf("the resolution below node %d is %v, and this test needs the claimant alone in it",
			child, resolution)
	}
	if first, last := SubtreeLeaves(child); absent < first || absent > last {
		t.Fatalf("leaf %d is not under node %d, so naming it in unmerged_leaves is not the surplus this test is about",
			absent, child)
	}
	if err := legal.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("the tree whose unmerged list is empty: %v", err)
	}

	// the same tree with one unmerged entry ADDED, naming a leaf that is not in the resolution.
	forged := legal.Clone()
	padded := forged.ParentAt(root).Clone()
	padded.UnmergedLeaves = []LeafIndex{absent}
	if err := forged.SetParent(root, padded); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	forgedHash, err := forged.ParentHash(crypto, root, copath)
	if err != nil {
		t.Fatalf("ParentHash: %v", err)
	}
	if !bytes.Equal(hash, forgedHash) {
		t.Fatalf("the pair does not differ in unmerged_leaves alone: the claimed parent hash moved from %x to %x",
			hash, forgedHash)
	}
	claimant, carries := nodeParentHashField(forged.Get(LeafIndex(0).NodeIndex()))
	if !carries || !bytes.Equal(claimant, forgedHash) {
		t.Fatalf("the forged tree's claimant carries %x (present: %v) and the parent hash is %x, so a refusal would be condition 2's and not condition 3's",
			claimant, carries, forgedHash)
	}
	if err := forged.VerifyParentHashes(crypto); !errors.Is(err, ErrParentHashMismatch) {
		t.Fatalf("err = %v, want ErrParentHashMismatch: node %d names leaf %d as unmerged and the resolution below node %d does not hold it",
			err, root, absent, child)
	}
}

// collidingHashProvider is the registered suite with its Hash collapsed onto one answer.
//
// It exists for one assertion and is documented rather than reached for casually: a hash that is
// not collision resistant is the ONLY input that reaches the upper half of section 7.9.2's
// "exactly one descendant". Two claimants under ONE child are impossible by arithmetic --
// condition 3 gives a claimant D the equation resolution(C) minus D equals U, and two distinct
// claimants in the same resolution would each have to appear in the other's U, which forces
// resolution(C) to equal U and contradicts |U| = |resolution(C)| - 1. Two claimants across the
// two ARMS need each side's stored parent_hash to be a hash of the subtree the other one sits
// in, which is a cycle in the hash. This provider is that cycle, supplied.
//
// Only Hash is overridden; everything else is the registered suite's, so nothing driven over it
// fails for a reason belonging to a fake.
type collidingHashProvider struct {
	CryptoProvider
	answer []byte
}

// Cloned rather than handed out, for the reason the tagging provider gives: the real provider
// hands every caller storage nobody else writes into, and a wrapper that returned the same array
// to all of them would break that invariant from the test side.
func (self *collidingHashProvider) Hash(data []byte) []byte {
	return bytes.Clone(self.answer)
}

// TestVerifyParentHashesRefusesAParentTwoDescendantsClaim is the UPPER half of section 7.9.2's
// "exactly one descendant", which no tree over a real hash can reach.
//
// TestVerifyParentHashesFollowsTheClaimToWhicheverChildHoldsIt records why the fixture is not
// constructible: writing the second claimant kills the first, because each claimant's stored
// value is a hash of the subtree the other one sits in. The consequence is that "claims != 1"
// and "claims < 1" are the same function over every fixture in this file and over the published
// corpus -- measured, not supposed, the two are indistinguishable under the whole package. The
// clause is a real requirement of the RFC, and the only way to observe it is to supply the
// collision the impossibility argument turns on.
//
// What this buys is not a defence against a broken SHA-256. It is that the count cannot be
// weakened to "at least one" -- or to a bool per arm, which is the same weakening spelled
// differently -- without a test going red, on the line whose whole content is that two update
// paths spliced into one parent are refused.
func TestVerifyParentHashesRefusesAParentTwoDescendantsClaim(t *testing.T) {
	suite := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	colliding := &collidingHashProvider{
		CryptoProvider: suite,
		answer:         bytes.Repeat([]byte{0x9C}, suite.HashSize()),
	}
	tree, members := newTestTree(t, suite, 2)
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{0xE1}, 32)),
		ParentHash:    []byte{},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	// the fixture's own claim, both ways round. Under the registered suite the two arms of node 1
	// answer different parent hashes, which is what makes two claimants unconstructible; under
	// this provider they answer the same one, which is what makes them constructible.
	realRight, err := tree.ParentHash(suite, NodeIndex(1), NodeIndex(2))
	if err != nil {
		t.Fatalf("ParentHash(1, 2): %v", err)
	}
	realLeft, err := tree.ParentHash(suite, NodeIndex(1), NodeIndex(0))
	if err != nil {
		t.Fatalf("ParentHash(1, 0): %v", err)
	}
	if bytes.Equal(realRight, realLeft) {
		t.Fatalf("the two arms of node 1 already collide under the registered suite, so this test is not about the provider it installs")
	}
	withRight, err := tree.ParentHash(colliding, NodeIndex(1), NodeIndex(2))
	if err != nil {
		t.Fatalf("ParentHash(1, 2) under the colliding provider: %v", err)
	}
	withLeft, err := tree.ParentHash(colliding, NodeIndex(1), NodeIndex(0))
	if err != nil {
		t.Fatalf("ParentHash(1, 0) under the colliding provider: %v", err)
	}
	if !bytes.Equal(withRight, withLeft) {
		t.Fatalf("the two arms of node 1 answer %x and %x under the colliding provider, so this fixture cannot hold two claimants",
			withRight, withLeft)
	}

	// one claimant under that same provider is ACCEPTED. Without this the refusal below could be
	// the colliding provider breaking the check in some way that has nothing to do with a count.
	giveTheLeafACommitParentHash(t, suite, tree, members, LeafIndex(0), withRight)
	if err := tree.VerifyParentHashes(colliding); err != nil {
		t.Fatalf("one claimant under the colliding provider: %v", err)
	}
	// and the second, on the other arm: two, which is neither zero nor one.
	giveTheLeafACommitParentHash(t, suite, tree, members, LeafIndex(1), withLeft)
	err = tree.VerifyParentHashes(colliding)
	if !errors.Is(err, ErrParentHashMismatch) {
		t.Fatalf("err = %v, want ErrParentHashMismatch: both children of node 1 claim it", err)
	}
	// the refusal is about the COUNT and names it, so a check that read "at least one" and
	// refused for some other reason fails here rather than passing.
	if !strings.Contains(err.Error(), "claimed by 2") {
		t.Errorf("node 1 is claimed by both of its children and the refusal does not say so: %v", err)
	}
}

// ---------------------------------------------------------------------------
// task 13's debt: what ParentHash refuses, and the RFC's own worked example
// ---------------------------------------------------------------------------

// TestParentHashRefusesEveryArgumentItCannotAnswerFor is the half of task 13 that never landed.
//
// Task 13's implementer was lost mid-response and ParentHash arrived with no behavioural test of
// its own; task 14 committed it because task 14 does not compile without it. What held it after
// that was TestVerifyParentHashesAcceptsThePublishedTreeValidationCorpus, which is real coverage
// of the PREIMAGE -- 290 non-blank parents of the working group's own trees, several with
// unmerged leaves inside the sibling subtree -- and no coverage at all of what the method does
// with an argument it cannot answer for. Every one of those refusals is a caller's bug rather
// than a protocol condition, so nothing off the wire reaches them and no corpus can.
//
// Each of the three argument classes is DERIVED from the tree rather than sampled, because each
// of them has the same failure mode: a hash taken over the wrong subtree is a well formed digest
// of the right width, it chains, and nothing a length check or a comparison against this
// implementation can see separates it from the right one.
//
//   - the copath child class is every node index of the tree that is not one of the parent's two
//     children, plus two past the end. A sampled version would take the grandparent and the
//     sibling and miss the parent's own index, or take those and miss a node in another subtree.
//   - the node type class is every EVEN index of the tree, swept against every copath child, so
//     a body that judged the child before the parity is visible.
//   - the out of range class is taken at BOTH parities, which is what pins the ORDER of the two
//     checks: with the range check removed an odd index past the end still has arithmetic
//     children and answers the copath refusal, and an even one answers the type mismatch. Both
//     send a caller to look at an argument that is not the problem.
//
// The blank parent is the fourth refusal and the one that is NOT a caller's bug -- it is a
// question about a tree somebody else sent -- so it answers ErrParentHashMismatch and is
// asserted separately.
func TestParentHashRefusesEveryArgumentItCannotAnswerFor(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	// every parent position occupied, so no refusal swept below is the blank node's refusal
	// wearing another name
	for _, x := range []NodeIndex{1, 3, 5} {
		if err := tree.SetParent(x, &ParentNode{
			EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{0x40 + byte(x)}, 32)),
			ParentHash:    bytes.Repeat([]byte{0x50 + byte(x)}, crypto.HashSize()),
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	width := tree.NodeWidth()
	if width != 7 {
		t.Fatalf("the fixture is %d nodes wide, and the sweeps below are written for the 7 node tree of four members", width)
	}
	accepted, notAChild, typeMismatch, outOfRange := 0, 0, 0, 0
	for x := uint32(0); x < width; x += 1 {
		p := NodeIndex(x)
		left, leftOk := leftOf(p)
		right, rightOk := rightOf(p)
		if !leftOk || !rightOk {
			// an even index is a leaf position and a leaf has no children, so there is no
			// copath child it could be handed. every one is swept anyway: the parity is what
			// decides this refusal and the child must not be able to change it.
			for y := uint32(0); y < width; y += 1 {
				if _, err := tree.ParentHash(crypto, p, NodeIndex(y)); !errors.Is(err, ErrNodeTypeMismatch) {
					t.Errorf("ParentHash(%d, %d) over a leaf position answered %v, want %v",
						p, y, err, ErrNodeTypeMismatch)
				}
				typeMismatch += 1
			}
			continue
		}
		// the positive control, at both children. Without it every assertion below is
		// satisfiable by a method that refuses everything.
		for _, child := range []NodeIndex{left, right} {
			hash, err := tree.ParentHash(crypto, p, child)
			if err != nil {
				t.Fatalf("ParentHash(%d, %d) over a child of that node: %v", p, child, err)
			}
			if len(hash) != crypto.HashSize() {
				t.Errorf("ParentHash(%d, %d) answered %d bytes, want the provider's %d",
					p, child, len(hash), crypto.HashSize())
			}
			accepted += 1
		}
		// and the refusal class, every node of this tree that is not one of the two children
		for y := uint32(0); y < width; y += 1 {
			if NodeIndex(y) == left || NodeIndex(y) == right {
				continue
			}
			if _, err := tree.ParentHash(crypto, p, NodeIndex(y)); !errors.Is(err, errCopathChildIsNotAChildOfTheParent) {
				t.Errorf("ParentHash(%d, %d) answered %v, and node %d is not a child of node %d; want %v",
					p, y, err, y, p, errCopathChildIsNotAChildOfTheParent)
			}
			notAChild += 1
		}
		// plus the two past the end, which no enumeration over the tree's own indices reaches
		for _, y := range []NodeIndex{NodeIndex(width), NodeIndex(width + 1)} {
			if _, err := tree.ParentHash(crypto, p, y); !errors.Is(err, errCopathChildIsNotAChildOfTheParent) {
				t.Errorf("ParentHash(%d, %d) answered %v for a copath child outside the tree, want %v",
					p, y, err, errCopathChildIsNotAChildOfTheParent)
			}
			notAChild += 1
		}
	}
	// the parent index outside the tree, at both parities and with both a valid looking and an
	// out of range copath child. The range check runs before the parity check and before the
	// copath check, and this is what says so.
	for _, p := range []NodeIndex{NodeIndex(width), NodeIndex(width + 1), NodeIndex(width + 8)} {
		for _, child := range []NodeIndex{NodeIndex(0), NodeIndex(width)} {
			if _, err := tree.ParentHash(crypto, p, child); !errors.Is(err, ErrNodeIndexOutOfRange) {
				t.Errorf("ParentHash(%d, %d) over a %d node tree answered %v, want %v",
					p, child, width, err, ErrNodeIndexOutOfRange)
			}
			outOfRange += 1
		}
	}
	// and the one refusal that is about the tree rather than about the arguments
	blanked := tree.Clone()
	if err := blanked.Blank(NodeIndex(1)); err != nil {
		t.Fatalf("Blank(1): %v", err)
	}
	blankRefusals := 0
	for _, child := range []NodeIndex{NodeIndex(0), NodeIndex(2)} {
		if _, err := blanked.ParentHash(crypto, NodeIndex(1), child); !errors.Is(err, ErrParentHashMismatch) {
			t.Errorf("ParentHash(1, %d) over a blank node 1 answered %v, want %v",
				child, err, ErrParentHashMismatch)
		}
		blankRefusals += 1
	}
	if accepted == 0 || notAChild == 0 || typeMismatch == 0 || outOfRange == 0 || blankRefusals == 0 {
		t.Fatalf("the sweep made %d accepted calls, %d copath refusals, %d type refusals, %d range refusals and %d blank refusals; a zero in any of them is a class this test did not reach",
			accepted, notAChild, typeMismatch, outOfRange, blankRefusals)
	}
	t.Logf("%d accepted, %d copath refusals, %d type refusals, %d range refusals, %d blank refusals",
		accepted, notAChild, typeMismatch, outOfRange, blankRefusals)
}

// appendixBCommit is one member sending a full Commit over the tree, which is the operation
// appendix B's four steps are written in terms of.
func appendixBCommit(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	sender LeafIndex, signer SignaturePrivateKey) {
	t.Helper()
	if _, err := tree.CreateUpdatePathSecrets(crypto, sender, signer, testGroupId()); err != nil {
		t.Fatalf("CreateUpdatePathSecrets(leaf %d): %v", sender, err)
	}
}

// appendixBOriginalTreeHash is the osth of appendix B's H(P, ph, osth): RFC 9420 section 7.9's
// ORIGINAL tree hash of the subtree at sibling, taken with respect to the unmerged leaves of the
// parent whose hash is being written.
//
// Section 7.9's sentence, done to a copy of the tree rather than passed as a parameter, which is
// what makes this a separate reading from tree_hash.go's. That file computes the original hash
// by threading an exclude set through the one section 7.8 recursion -- deliberately, so the two
// hashes cannot encode a node differently -- and a helper that called the same recursion with
// the same set would be checking that function against itself. Here the leaves are BLANKED and
// struck out for real and then the ordinary tree hash is taken, so the two agree only if the
// exclude arm means what the RFC says it means.
//
// The striking is over every parent of the tree and not only the ones under the sibling. An
// unmerged leaf is listed at every non-blank node of its own direct path, so a leaf blanked out
// of the subtree while still named in some descendant's unmerged_leaves vector is a tree that
// hashes a member it does not have.
func appendixBOriginalTreeHash(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	parent *ParentNode, sibling NodeIndex) []byte {
	t.Helper()
	without := tree.Clone()
	excluded := map[LeafIndex]bool{}
	for _, leaf := range parent.UnmergedLeaves {
		excluded[leaf] = true
		if LeafCount(leaf) >= without.LeafWidth() {
			t.Fatalf("node's unmerged_leaves names leaf %d and the tree has %d leaves", leaf, without.LeafWidth())
		}
		if err := without.Blank(leaf.NodeIndex()); err != nil {
			t.Fatalf("Blank(leaf %d): %v", leaf, err)
		}
	}
	for x := uint32(0); x < without.NodeWidth(); x += 1 {
		node := without.ParentAt(NodeIndex(x))
		if node == nil {
			continue
		}
		kept := []LeafIndex{}
		for _, leaf := range node.UnmergedLeaves {
			if !excluded[leaf] {
				kept = append(kept, leaf)
			}
		}
		node.UnmergedLeaves = kept
	}
	hash, err := without.NodeTreeHash(crypto, sibling)
	if err != nil {
		t.Fatalf("NodeTreeHash(%d) over the tree without the unmerged leaves: %v", sibling, err)
	}
	return hash
}

// appendixBParentHash writes out appendix B's own H(P, ph=..., osth=th(S)) from the RFC's
// notation rather than from this package's ParentHash, so the two are separate readings.
//
// It used to REFUSE a parent carrying unmerged leaves, on the grounds that appendix B's osth is
// the original sibling tree hash and this helper took the plain one. That was honest about the
// helper and left the example weaker than it read: the four steps of appendix B never produce a
// parent with an unmerged leaf -- A adds B before X exists, and B adds C and D while both of
// their direct paths are blank -- so the refusal never fired and every osth in the worked
// example was a plain tree hash, which is a value an implementation that had never heard of
// section 7.9's blanking computes too. The exclusion is written out here now, and
// TestEveryParentHashOverATreeWithUnmergedLeavesTakesTheOriginalSiblingTreeHash drives this same
// notation over a tree that has one.
func appendixBParentHash(t *testing.T, crypto CryptoProvider, tree *RatchetTree,
	p NodeIndex, ph []byte, sibling NodeIndex) []byte {
	t.Helper()
	node := tree.ParentAt(p)
	if node == nil {
		t.Fatalf("node %d is blank, so appendix B's H(P, ...) has no encryption_key to write", p)
	}
	return crypto.Hash(treeHashTestConcat(
		treeHashTestOpaque(t, node.EncryptionKey),
		treeHashTestOpaque(t, ph),
		treeHashTestOpaque(t, appendixBOriginalTreeHash(t, crypto, tree, node, sibling))))
}

// TestTheParentHashChainOfRfc9420AppendixBsWorkedExample is the RFC's own worked example, run.
//
// This is the strongest anchor available for both halves of the chain -- the values task 18
// assigns and the values task 14 verifies -- and before this test it was held nowhere. Appendix
// B is a worked example rather than a vector file: it gives no octets, so what it pins is the
// SHAPE of every parent hash in a four member tree after a stated sequence of commits, plus
// table 15's verdict on each. That shape is what an implementation gets wrong. A chain computed
// leaf-upward, a leaf that carries the wrong node's hash, a root that carries something other
// than the zero-length octet string and an osth taken over the wrong child all produce a full
// set of well formed digests, and only a comparison against a preimage written from the RFC's
// own notation separates them.
//
// The tree GROWS through the example rather than starting at four leaves, because that is where
// the example's content is. X is the root of a two leaf tree and X' is a node of a four leaf
// one; the whole point of table 15's "No, Y and Z changed" is that X' still carries a hash taken
// against a Y that no longer exists. A fixture that started at four leaves would compute a
// different X, agree with itself, and hold none of it.
//
// The preimages come from appendixBParentHash, which spells opaque<V> by hand for the reason
// TestABlankSubtreeHashesToTheHandDerivedGolden gives: a preimage assembled with the encoder
// under test is wrong in the same way the encoder is.
func TestTheParentHashChainOfRfc9420AppendixBsWorkedExample(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	// four well formed members borrowed from the shared builder, re-installed one at a time
	// below so the tree grows the way figure 30's does
	full, members := newTestTree(t, crypto, 4)
	leaves := [4]*LeafNode{}
	for i := range leaves {
		leaves[i] = full.Leaf(LeafIndex(i)).Clone()
	}
	const (
		a = LeafIndex(0)
		b = LeafIndex(1)
		c = LeafIndex(2)
		d = LeafIndex(3)
	)
	// figure 30's node labels. X and X' are one position, as are Y and Y', and as are the blank
	// Z and Z'; the primes are epochs of the same node and not different nodes.
	const (
		x = NodeIndex(1)
		y = NodeIndex(3)
		z = NodeIndex(5)
	)

	// 1. A initializes a new group.
	tree := NewRatchetTree()
	if err := tree.SetLeaf(a, leaves[0]); err != nil {
		t.Fatalf("SetLeaf(A): %v", err)
	}

	// 2. A adds B to the group with a full Commit: set X.
	if at, err := tree.AddLeaf(leaves[1]); err != nil || at != b {
		t.Fatalf("AddLeaf(B) = %d, %v; want leaf %d and no error", at, err, b)
	}
	appendixBCommit(t, crypto, tree, a, members[0].SignaturePriv)
	xNode := tree.ParentAt(x)
	if xNode == nil {
		t.Fatalf("A's full commit did not set X")
	}
	if len(xNode.ParentHash) != 0 {
		t.Errorf("X is the root of the two leaf tree, so section 7.9 gives its parent_hash field the zero-length octet string; it carries %d bytes",
			len(xNode.ParentHash))
	}
	// A.parent_hash = ph(X) = H(X, ph="", osth=th(B))
	aField := cloneBytes(tree.Leaf(a).ParentHash)
	wantA := appendixBParentHash(t, crypto, tree, x, nil, b.NodeIndex())
	if !bytes.Equal(aField, wantA) {
		t.Errorf("after step 2 A.parent_hash is %x, and appendix B's H(X, ph=\"\", osth=th(B)) is %x", aField, wantA)
	}

	// 3. B adds C and D to the group with a full Commit: set B', X' and Y.
	if at, err := tree.AddLeaf(leaves[2]); err != nil || at != c {
		t.Fatalf("AddLeaf(C) = %d, %v; want leaf %d and no error", at, err, c)
	}
	if at, err := tree.AddLeaf(leaves[3]); err != nil || at != d {
		t.Fatalf("AddLeaf(D) = %d, %v; want leaf %d and no error", at, err, d)
	}
	appendixBCommit(t, crypto, tree, b, members[1].SignaturePriv)
	if tree.ParentAt(z) != nil {
		t.Errorf("figure 30 has Z blank at this step -- th(Z) covers (C, _, D) -- and node %d is occupied", z)
	}
	if yNode := tree.ParentAt(y); yNode == nil {
		t.Fatalf("B's full commit did not set Y")
	} else if len(yNode.ParentHash) != 0 {
		t.Errorf("Y is the root, so its parent_hash field is the zero-length octet string; it carries %d bytes",
			len(yNode.ParentHash))
	}
	// X'.parent_hash = ph(Y) = H(Y, ph="", osth=th(Z))
	xPrimeField := cloneBytes(tree.ParentAt(x).ParentHash)
	wantXPrime := appendixBParentHash(t, crypto, tree, y, nil, z)
	if !bytes.Equal(xPrimeField, wantXPrime) {
		t.Errorf("after step 3 X'.parent_hash is %x, and appendix B's H(Y, ph=\"\", osth=th(Z)) is %x",
			xPrimeField, wantXPrime)
	}
	// B'.parent_hash = ph(X') = H(X', ph=X'.parent_hash, osth=th(A))
	bField := cloneBytes(tree.Leaf(b).ParentHash)
	wantB := appendixBParentHash(t, crypto, tree, x, xPrimeField, a.NodeIndex())
	if !bytes.Equal(bField, wantB) {
		t.Errorf("after step 3 B'.parent_hash is %x, and appendix B's H(X', ph=X'.parent_hash, osth=th(A)) is %x",
			bField, wantB)
	}

	// 4. C sends an empty Commit: set C', Z' and Y'.
	appendixBCommit(t, crypto, tree, c, members[2].SignaturePriv)
	if yPrime := tree.ParentAt(y); yPrime == nil {
		t.Fatalf("C's commit did not set Y'")
	} else if len(yPrime.ParentHash) != 0 {
		t.Errorf("Y' is the root, so its parent_hash field is the zero-length octet string; it carries %d bytes",
			len(yPrime.ParentHash))
	}
	// Z'.parent_hash = ph(Y') = H(Y', ph="", osth=th(X'))
	zPrimeField := cloneBytes(tree.ParentAt(z).ParentHash)
	wantZPrime := appendixBParentHash(t, crypto, tree, y, nil, x)
	if !bytes.Equal(zPrimeField, wantZPrime) {
		t.Errorf("after step 4 Z'.parent_hash is %x, and appendix B's H(Y', ph=\"\", osth=th(X')) is %x",
			zPrimeField, wantZPrime)
	}
	// C'.parent_hash = ph(Z') = H(Z', ph=Z'.parent_hash, osth=th(D))
	cField := cloneBytes(tree.Leaf(c).ParentHash)
	wantC := appendixBParentHash(t, crypto, tree, z, zPrimeField, d.NodeIndex())
	if !bytes.Equal(cField, wantC) {
		t.Errorf("after step 4 C'.parent_hash is %x, and appendix B's H(Z', ph=Z'.parent_hash, osth=th(D)) is %x",
			cField, wantC)
	}
	// the two values set higher up the tree in an earlier epoch are still the ones their epoch
	// wrote. That is what "the part of the chain closer to the root can be overwritten" means
	// from the other side: A and X' were not touched by steps 3 and 4, and it is precisely
	// because they were not that table 15 reads them as invalid.
	if held := tree.Leaf(a).ParentHash; !bytes.Equal(held, aField) {
		t.Errorf("A.parent_hash moved from %x to %x across two commits that did not traverse A", aField, held)
	}
	if held := tree.ParentAt(x).ParentHash; !bytes.Equal(held, xPrimeField) {
		t.Errorf("X'.parent_hash moved from %x to %x across a commit that did not traverse X'", xPrimeField, held)
	}

	// table 15, recomputed over the final tree. Each row names the node whose field is read,
	// the parent that field is read against, and the copath child of that parent.
	valid, invalid, absent := 0, 0, 0
	for _, row := range []struct {
		name    string
		node    NodeIndex
		parent  NodeIndex
		copath  NodeIndex
		carries bool
		want    bool
		why     string
	}{
		{name: "A", node: a.NodeIndex(), parent: x, copath: b.NodeIndex(), carries: true, want: false, why: "B changed"},
		{name: "B'", node: b.NodeIndex(), parent: x, copath: a.NodeIndex(), carries: true, want: true},
		{name: "C'", node: c.NodeIndex(), parent: z, copath: d.NodeIndex(), carries: true, want: true},
		{name: "D", node: d.NodeIndex(), carries: false, why: "never sent an UpdatePath"},
		{name: "X'", node: x, parent: y, copath: z, carries: true, want: false, why: "Y and Z changed"},
		{name: "Z'", node: z, parent: y, copath: x, carries: true, want: true},
	} {
		field, carries := nodeParentHashField(tree.Get(row.node))
		if carries != row.carries {
			t.Errorf("table 15 has %s carrying a parent hash = %v, and this tree answers %v (%s)",
				row.name, row.carries, carries, row.why)
			continue
		}
		if !carries {
			absent += 1
			continue
		}
		live, err := tree.ParentHash(crypto, row.parent, row.copath)
		if err != nil {
			t.Fatalf("ParentHash(%d, %d) for table 15's %s row: %v", row.parent, row.copath, row.name, err)
		}
		if got := bytes.Equal(field, live); got != row.want {
			t.Errorf("table 15 says %s's parent hash is valid = %v (%s), and this tree answers %v: the field is %x and the parent hash of node %d with copath child %d is %x",
				row.name, row.want, row.why, got, field, row.parent, row.copath, live)
		}
		if row.want {
			valid += 1
		} else {
			invalid += 1
		}
	}
	if valid != 3 || invalid != 2 || absent != 1 {
		t.Fatalf("table 15 has three valid rows, two invalid ones and one node carrying no field; this run read %d, %d and %d",
			valid, invalid, absent)
	}
	// and the sentence the table is there to support: those chains cover every non-blank parent,
	// so the tree is parent-hash valid even though it holds two invalid links.
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("appendix B states this tree is parent-hash valid, and this package refuses it: %v", err)
	}
}

// TestEveryParentHashOverATreeWithUnmergedLeavesTakesTheOriginalSiblingTreeHash is the arm
// appendix B's worked example cannot reach, driven through the same hand written preimage.
//
// Appendix B's four steps never leave a parent carrying an unmerged leaf: A adds B while X does
// not exist yet, and B adds C and D while both of their direct paths are blank, so every osth in
// that example is a plain tree hash and the example agrees with an implementation that never
// heard of section 7.9's blanking. That is a property of the example rather than an omission in
// it, and the way to hold the other arm through the same reading is a tree where a member was
// ADDED since the last commit -- which is exactly when the two hashes differ, and exactly the
// state a joiner validates.
//
// The fixture makes one: a commit sets a node, and a later Add lands under it and is listed
// there as unmerged. Then the sweep is over the tree rather than over the node the fixture
// happens to have produced. Every non-blank parent, with each of its two children as the
// sibling, is compared against appendixBParentHash -- and the run is required to contain both
// kinds of case, a parent whose unmerged leaves fall inside the sibling subtree, where the
// blanking moves the hash, and one where it does not and the two readings must agree. A fixture
// that stopped producing an unmerged leaf would fail the count rather than quietly going back
// to hashing only the arm appendix B already covers.
func TestEveryParentHashOverATreeWithUnmergedLeavesTakesTheOriginalSiblingTreeHash(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 8)
	// a member leaves, which blanks its own direct path up to the root
	if err := tree.RemoveLeaf(LeafIndex(4)); err != nil {
		t.Fatalf("RemoveLeaf(4): %v", err)
	}
	// and a commit from the other side of the tree puts the root and the nodes below it back
	if _, err := tree.CreateUpdatePathSecrets(crypto, members[0].LeafIndex,
		members[0].SignaturePriv, testGroupId()); err != nil {
		t.Fatalf("CreateUpdatePathSecrets(leaf 0): %v", err)
	}
	// so the next member to join lands under a node that already carries a key, and is listed
	// there as unmerged: it has not been given that node's secret and is not in its subtree as
	// far as every hash taken before this Add is concerned
	joiner, _ := newTestTree(t, crypto, 1)
	at, err := tree.AddLeaf(joiner.Leaf(LeafIndex(0)).Clone())
	if err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	carriers := 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		if node := tree.ParentAt(NodeIndex(x)); node != nil && len(node.UnmergedLeaves) != 0 {
			carriers += 1
		}
	}
	if carriers == 0 {
		t.Fatalf("adding a member at leaf %d left no parent carrying an unmerged leaf, so this test is running over the arm appendix B already covers",
			at)
	}

	// the sweep: every non-blank parent, each of its children as the sibling
	agreed, moved, held := 0, 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		p := NodeIndex(x)
		node := tree.ParentAt(p)
		if node == nil {
			continue
		}
		for _, child := range []func(NodeIndex) (NodeIndex, bool){leftOf, rightOf} {
			sibling, isParent := child(p)
			if !isParent {
				t.Fatalf("node %d holds a parent node and tree math says it has no children", p)
			}
			want := appendixBParentHash(t, crypto, tree, p, node.ParentHash, sibling)
			got, err := tree.ParentHash(crypto, p, sibling)
			if err != nil {
				t.Fatalf("ParentHash(%d, %d): %v", p, sibling, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("ParentHash(%d, %d) is %x and section 7.9's own notation gives %x",
					p, sibling, got, want)
				continue
			}
			agreed += 1
			// which arm this case exercised, decided by the bytes rather than by the shape:
			// the same preimage with the PLAIN tree hash of the sibling in place of the
			// original one. Equal means the blanking had nothing to do here; different means
			// this case is one an implementation that skipped section 7.9's exclusion fails.
			plain, err := tree.NodeTreeHash(crypto, sibling)
			if err != nil {
				t.Fatalf("NodeTreeHash(%d): %v", sibling, err)
			}
			naive := crypto.Hash(treeHashTestConcat(
				treeHashTestOpaque(t, node.EncryptionKey),
				treeHashTestOpaque(t, node.ParentHash),
				treeHashTestOpaque(t, plain)))
			if bytes.Equal(naive, want) {
				held += 1
				continue
			}
			moved += 1
			if bytes.Equal(got, naive) {
				t.Errorf("ParentHash(%d, %d) is the hash taken over the PLAIN tree hash of node %d; section 7.9 takes it over the original tree hash, with the parent's unmerged leaves blanked out",
					p, sibling, sibling)
			}
		}
	}
	if agreed == 0 || moved == 0 || held == 0 {
		t.Fatalf("the sweep agreed on %d parent hashes, %d of them over a sibling subtree the blanking changes and %d over one it does not; a zero in any of the three is an arm this test did not reach",
			agreed, moved, held)
	}
	t.Logf("%d parent hashes agreed with section 7.9's own notation, %d of them over a sibling subtree the unmerged-leaf blanking moves, %d over one it leaves alone",
		agreed, moved, held)
}
