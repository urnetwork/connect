// Tests for the ratchet tree container of RFC 9420 section 7 and for the ParentNode codec.
//
// Three properties here are not round trip properties, and they are the reason this file is
// longer than the code it holds.
//
// A hand derived golden, stated from the RFC without reference to the encoder, is the only
// thing that separates a field order from its mirror image. Two adjacent opaque<V> fields
// swapped in BOTH halves of a codec round trip perfectly and re-encode byte exact; so does a
// field dropped from both halves, while being lost. Both shapes have been found in this package
// before, and neither is visible to any symmetry property.
//
// Comparing the decoded VALUE rather than the re-encoded bytes is the other half of that: a
// dropped field is byte exact against itself and unequal as a value.
//
// And the blank node. optional<Node> makes a blank an ABSENT array entry, so a blank and a zero
// valued node are different bytes and different tree hashes. Every blank assertion below derives
// the blank set from the tree's own node width rather than naming a few indices, because "the
// blanks are where somebody remembered to look" is exactly how that conflation survives a suite.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the values the goldens and the sweeps are built from
// ---------------------------------------------------------------------------

// testParentNodeTemplate is one parent node with every field populated and every field
// different from every other field of its own width.
//
// encryption_key and parent_hash are the SAME width on purpose. They are adjacent opaque<V>
// fields, so a length difference would separate them for free and a swap would be caught by
// arithmetic that has nothing to do with the fields. Equal width means only their CONTENT
// separates them, which is the strictest form of the swap test and what the goldens rest on.
//
// The unmerged list is strictly ascending because RFC 9420 section 7.9.2 requires it and
// because this codec refuses anything else; the refusal has its own test below.
func testParentNodeTemplate() *ParentNode {
	return &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0xa1, 32)),
		ParentHash:     repeatByte(0xb2, 32),
		UnmergedLeaves: []LeafIndex{1, 2, 5},
	}
}

// testParentNodeInside is the template with its unmerged list narrowed to the leaves the given
// tree actually has.
//
// The template names leaf 5 because its own subject is the CODEC, where a strictly ascending
// list with a gap is the vector worth encoding and no tree is involved at all. SetParent refuses
// an unmerged leaf outside the tree it is installing into -- for the reason written on it, that
// such a list makes the resolution of a non-blank node the empty list, which is the list a path
// secret would be sealed to -- so a test installing the template into a narrow tree has to hand
// it a list that tree can hold.
//
// Narrowed rather than the refusal being routed around: every property the tests below measure
// through the template is about storage, aliasing and copying, and none of them is about which
// leaves the list names. Derived from the tree's own width so it stays right as those tests
// change the tree they build.
func testParentNodeInside(tree *RatchetTree) *ParentNode {
	parent := testParentNodeTemplate()
	kept := []LeafIndex{}
	for _, leaf := range parent.UnmergedLeaves {
		if LeafCount(leaf) < tree.LeafWidth() {
			kept = append(kept, leaf)
		}
	}
	parent.UnmergedLeaves = kept
	return parent
}

// testTreeLeaf is one leaf whose keys are a function of its index, so that a tree built below
// has a distinct signature key and a distinct encryption key at every occupied position. Two
// leaves carrying the same key would make FindLeafBySignatureKey and EncryptionKeyInUse pass
// against an implementation that answered the wrong position.
func testTreeLeaf(i uint32) *LeafNode {
	leaf := testLeafNodeTemplate()
	leaf.LeafNodeSource = LeafNodeSourceUpdate
	leaf.SignatureKey = SignaturePublicKey(repeatByte(byte(0x80+i), 32))
	leaf.EncryptionKey = HpkePublicKey(repeatByte(byte(0x40+i), 32))
	return leaf
}

// treeUnderTest builds a tree holding leafCount occupied leaves and a parent node at every
// LEVEL ONE index, and answers it together with the exact set of node indices it filled.
//
// The occupied set is returned rather than described, and every blank assertion below is
// derived from it and from the tree's own NodeWidth. Level one only -- the odd indices
// congruent to 1 modulo 4 -- so that the sweep sees blank parents as well as occupied ones at
// every width above two, which a rule that filled every odd index would not.
func treeUnderTest(t *testing.T, leafCount uint32) (*RatchetTree, map[NodeIndex]bool) {
	t.Helper()
	tree := NewRatchetTree()
	occupied := map[NodeIndex]bool{}
	for i := uint32(0); i < leafCount; i += 1 {
		if err := tree.SetLeaf(LeafIndex(i), testTreeLeaf(i)); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
		occupied[LeafIndex(i).NodeIndex()] = true
	}
	for x := uint32(1); x < tree.NodeWidth(); x += 4 {
		if err := tree.SetParent(NodeIndex(x), &ParentNode{
			EncryptionKey: HpkePublicKey(repeatByte(byte(0xc0+x), 32)),
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
		occupied[NodeIndex(x)] = true
	}
	return tree, occupied
}

// ---------------------------------------------------------------------------
// the hand derived goldens for ParentNode
// ---------------------------------------------------------------------------

// parentNodeGoldenCase is one parent node and the octets RFC 9420 section 7.1 says it encodes
// to, derived by hand. The size is stated separately from the derivation so that a derivation
// edited without its comment fails rather than quietly redefining what it is compared to.
type parentNodeGoldenCase struct {
	name  string
	node  *ParentNode
	bytes []byte
	size  int
}

// parentNodeGoldenCases is the RFC 9420 section 7.1 structure written out from the RFC rather
// than read back through the encoder:
//
//	struct {
//	    HPKEPublicKey encryption_key;      -- opaque<V>
//	    opaque parent_hash<V>;
//	    uint32 unmerged_leaves<V>;
//	} ParentNode;
//
// The populated case:
//
//	encryption_key<V>    32 octets    -> 20 a1*32                          33
//	parent_hash<V>       32 octets    -> 20 b2*32                          33
//	unmerged_leaves<V>   3 x uint32   -> 0c 00000001 00000002 00000005     13
//
// 33 + 33 + 13 = 79 octets.
//
// The unmerged prefix counts BYTES and not elements, which is the single easiest thing in this
// encoding to get wrong and the one a round trip cannot see: three uint32 are twelve octets, so
// it is 0c and never 03, and an encoder writing the element count agrees perfectly with a
// decoder reading one element per unit and with nothing else.
//
// The sixteen leaf case exists for the prefix WIDTH. Sixteen uint32 are 64 octets, which is the
// first length whose varint prefix is two octets rather than one, so it comes out 40 40: a
// fixed one octet prefix moves the total by one and a WriteOpaqueLP -- which would write
// 00000040 -- moves it by three. Neither is visible to a round trip, because this
// implementation would read back whatever it wrote.
//
// The empty cases pin that a zero length field is one zero octet and not an omission, which is
// what keeps a blank parent hash distinguishable from a parent node that carries none.
func parentNodeGoldenCases() []parentNodeGoldenCase {
	ascendingLeaves := func(n uint32) []LeafIndex {
		out := []LeafIndex{}
		for i := uint32(0); i < n; i += 1 {
			out = append(out, LeafIndex(i))
		}
		return out
	}
	sixteenLeafOctets := []byte{}
	for i := 0; i < 16; i += 1 {
		sixteenLeafOctets = append(sixteenLeafOctets, 0x00, 0x00, 0x00, byte(i))
	}
	return []parentNodeGoldenCase{
		{
			name: "populated",
			node: testParentNodeTemplate(),
			bytes: joinBytes(
				[]byte{0x20}, repeatByte(0xa1, 32),
				[]byte{0x20}, repeatByte(0xb2, 32),
				[]byte{0x0c},
				[]byte{0x00, 0x00, 0x00, 0x01},
				[]byte{0x00, 0x00, 0x00, 0x02},
				[]byte{0x00, 0x00, 0x00, 0x05},
			),
			// 33 + 33 + 13
			size: 79,
		},
		{
			name: "no unmerged leaves",
			node: &ParentNode{
				EncryptionKey:  HpkePublicKey(repeatByte(0xa1, 32)),
				ParentHash:     repeatByte(0xb2, 32),
				UnmergedLeaves: []LeafIndex{},
			},
			bytes: joinBytes(
				[]byte{0x20}, repeatByte(0xa1, 32),
				[]byte{0x20}, repeatByte(0xb2, 32),
				[]byte{0x00},
			),
			// 33 + 33 + 1
			size: 67,
		},
		{
			name: "empty parent hash",
			node: &ParentNode{
				EncryptionKey:  HpkePublicKey(repeatByte(0xa1, 32)),
				ParentHash:     []byte{},
				UnmergedLeaves: []LeafIndex{1, 2, 5},
			},
			bytes: joinBytes(
				[]byte{0x20}, repeatByte(0xa1, 32),
				[]byte{0x00},
				[]byte{0x0c},
				[]byte{0x00, 0x00, 0x00, 0x01},
				[]byte{0x00, 0x00, 0x00, 0x02},
				[]byte{0x00, 0x00, 0x00, 0x05},
			),
			// 33 + 1 + 13
			size: 47,
		},
		{
			name: "every field empty",
			node: &ParentNode{
				EncryptionKey:  HpkePublicKey{},
				ParentHash:     []byte{},
				UnmergedLeaves: []LeafIndex{},
			},
			bytes: []byte{0x00, 0x00, 0x00},
			// 1 + 1 + 1
			size: 3,
		},
		{
			name: "sixteen unmerged leaves",
			node: &ParentNode{
				EncryptionKey:  HpkePublicKey(repeatByte(0xa1, 32)),
				ParentHash:     repeatByte(0xb2, 32),
				UnmergedLeaves: ascendingLeaves(16),
			},
			bytes: joinBytes(
				[]byte{0x20}, repeatByte(0xa1, 32),
				[]byte{0x20}, repeatByte(0xb2, 32),
				[]byte{0x40, 0x40}, sixteenLeafOctets,
			),
			// 33 + 33 + 2 + 64
			size: 132,
		},
	}
}

// TestParentNodeMarshalMatchesTheHandDerivedGoldens is the field order and prefix width pin,
// and the one test in this file a symmetric edit cannot survive.
//
// encryption_key and parent_hash swapped in BOTH halves of the codec round trips perfectly and
// re-encodes byte exact; so does either of them dropped from both halves. What separates those
// from this codec is a statement of the encoding written without reference to the code, which
// is what parentNodeGoldenCases is.
func TestParentNodeMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	cases := parentNodeGoldenCases()
	if len(cases) == 0 {
		t.Fatal("no golden case, so this gate compared nothing")
	}
	for _, testCase := range cases {
		if len(testCase.bytes) != testCase.size {
			t.Fatalf("%s: the hand derivation is %d octets and the arithmetic in its comment says %d",
				testCase.name, len(testCase.bytes), testCase.size)
		}
		encoded, err := syntax.Marshal(testCase.node)
		if err != nil {
			t.Fatalf("%s: Marshal: %v", testCase.name, err)
		}
		if !bytes.Equal(encoded, testCase.bytes) {
			t.Errorf("%s: Marshal =\n %x\nwant\n %x", testCase.name, encoded, testCase.bytes)
		}
	}
}

// TestParentNodeGoldenDecodesToTheValueItWasBuiltFrom compares the decoded VALUE against the
// original rather than the re-encoded bytes against the golden.
//
// That is a different property and it catches a different defect. A field dropped from both
// halves of the codec is byte exact against everything, including the golden if the golden were
// generated from the encoder; what it is not is EQUAL, because the field it never wrote is not
// there when the value comes back. reflect.DeepEqual over the whole structure is what sees it,
// and over the whole structure rather than three named fields, so a field added later is
// covered without this test being edited.
//
// Every golden case is built with non nil empty vectors, because ReadOpaque and ReadVector
// never answer nil: a nil field in the original would compare unequal to a decode of its own
// bytes, which is a property of Go and not of this codec.
func TestParentNodeGoldenDecodesToTheValueItWasBuiltFrom(t *testing.T) {
	for _, testCase := range parentNodeGoldenCases() {
		decoded := &ParentNode{}
		if err := syntax.Unmarshal(testCase.bytes, decoded); err != nil {
			t.Fatalf("%s: Unmarshal the golden: %v", testCase.name, err)
		}
		if !reflect.DeepEqual(decoded, testCase.node) {
			t.Errorf("%s: the golden decoded to\n %+v\nwant\n %+v", testCase.name, decoded, testCase.node)
		}
	}
}

// ---------------------------------------------------------------------------
// unmerged_leaves ordering
// ---------------------------------------------------------------------------

// unmergedLeavesEncoding builds a ParentNode encoding by hand around a given unmerged_leaves
// body, so the decode side can be handed vectors this package's own encoder refuses to
// produce. The two opaque fields are one octet each and empty, so the whole input is the
// unmerged vector plus three prefixes.
func unmergedLeavesEncoding(leaves []uint32) []byte {
	body := []byte{}
	for _, leaf := range leaves {
		body = append(body, byte(leaf>>24), byte(leaf>>16), byte(leaf>>8), byte(leaf))
	}
	return joinBytes([]byte{0x00, 0x00, byte(len(body))}, body)
}

// TestParentNodeRefusesUnmergedLeavesThatAreNotStrictlyAscending holds RFC 9420 section 7.9.2
// on BOTH halves of the codec.
//
// Sorted and unique are one requirement, so both orderings and both repeats are refused with
// one sentinel. The decode half matters because a peer's unsorted vector is a tree whose parent
// hashes nobody else computes; the encode half matters because it is what keeps this
// implementation from being the peer that publishes one.
//
// Refused and not quietly sorted, which are different behaviours and only one of them is the
// RFC's: the bytes a parent hash and a tree hash are taken over are the bytes that arrived, so
// an implementation that sorted on read would agree with itself and with nobody. The accepted
// half of the table is what keeps this from being a decoder that refuses everything.
func TestParentNodeRefusesUnmergedLeavesThatAreNotStrictlyAscending(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		leaves  []uint32
		refused bool
	}{
		{name: "empty", leaves: []uint32{}, refused: false},
		{name: "one", leaves: []uint32{7}, refused: false},
		{name: "ascending", leaves: []uint32{0, 1, 2}, refused: false},
		{name: "ascending with gaps", leaves: []uint32{1, 2, 5}, refused: false},
		{name: "descending", leaves: []uint32{1, 0}, refused: true},
		{name: "duplicated", leaves: []uint32{1, 1}, refused: true},
		{name: "out of order in the middle", leaves: []uint32{0, 3, 2, 4}, refused: true},
		{name: "duplicated at the end", leaves: []uint32{0, 1, 2, 2}, refused: true},
	} {
		asLeaves := []LeafIndex{}
		for _, leaf := range testCase.leaves {
			asLeaves = append(asLeaves, LeafIndex(leaf))
		}

		// the encode half
		_, err := syntax.Marshal(&ParentNode{UnmergedLeaves: asLeaves})
		if testCase.refused != errors.Is(err, ErrUnmergedLeavesNotSorted) {
			t.Errorf("%s: Marshal err = %v, refused should be %v", testCase.name, err, testCase.refused)
		}

		// the decode half, over bytes built without going through the encoder, since the
		// encoder above will not produce the refused ones
		decoded := &ParentNode{ParentHash: []byte("untouched")}
		err = syntax.Unmarshal(unmergedLeavesEncoding(testCase.leaves), decoded)
		if testCase.refused != errors.Is(err, ErrUnmergedLeavesNotSorted) {
			t.Errorf("%s: Unmarshal err = %v, refused should be %v", testCase.name, err, testCase.refused)
			continue
		}
		if !testCase.refused {
			if len(decoded.UnmergedLeaves) != len(asLeaves) {
				t.Errorf("%s: decoded %d unmerged leaves, want %d", testCase.name, len(decoded.UnmergedLeaves), len(asLeaves))
			}
			continue
		}
		// refused rather than repaired: nothing was assigned, so the receiver still holds
		// what it arrived with and no sorted copy of the refused vector exists anywhere
		if !bytes.Equal(decoded.ParentHash, []byte("untouched")) {
			t.Errorf("%s: a refused decode wrote into its receiver", testCase.name)
		}
		if decoded.UnmergedLeaves != nil {
			t.Errorf("%s: a refused decode left %v behind, so it repaired the vector rather than refusing it",
				testCase.name, decoded.UnmergedLeaves)
		}
	}
}

// TestParentNodeCloneSharesNoStorage. A parent node is copied whenever a provisional tree is
// built, so a commit that is later rejected must leave the epoch it was computed against
// exactly as it found it.
func TestParentNodeCloneSharesNoStorage(t *testing.T) {
	original := testParentNodeTemplate()
	clone := original.Clone()
	if !reflect.DeepEqual(clone, original) {
		t.Fatalf("Clone = %+v, want %+v", clone, original)
	}
	clone.EncryptionKey[0] ^= 0xff
	clone.ParentHash[0] ^= 0xff
	clone.UnmergedLeaves[0] = 0xffff
	if original.EncryptionKey[0] != 0xa1 {
		t.Errorf("Clone shares the encryption key with the original")
	}
	if original.ParentHash[0] != 0xb2 {
		t.Errorf("Clone shares the parent hash with the original")
	}
	if original.UnmergedLeaves[0] != LeafIndex(1) {
		t.Errorf("Clone shares the unmerged leaf list with the original")
	}
	// nil stays nil, because an absent vector and a present empty one are different bytes and
	// a clone that changed which one a caller holds changed the value it was asked to copy
	bare := (&ParentNode{}).Clone()
	if bare.EncryptionKey != nil || bare.ParentHash != nil || bare.UnmergedLeaves != nil {
		t.Errorf("Clone of a bare parent node = %+v, want every field nil", bare)
	}
}

// ---------------------------------------------------------------------------
// the blank node
// ---------------------------------------------------------------------------

// TestABlankAndAZeroValuedNodeEncodeDifferently is the statement the whole blank/absent
// distinction rests on, made in octets.
//
// RFC 9420 section 4.2 carries the array as optional<Node>, so an absent entry is one 0x00
// presence octet and a present entry is 0x01 followed by a whole node. An implementation that
// stored a blank as a zero valued node would round trip against itself, would agree with itself
// about every accessor in this file, and would hash a different tree from every peer -- a fork
// rather than a parse error. The two encodings differ in length as well as content here, and
// both are written out by hand.
func TestABlankAndAZeroValuedNodeEncodeDifferently(t *testing.T) {
	blankWriter := syntax.NewWriter()
	if err := blankWriter.WriteOptional(false, func(w *syntax.Writer) error { return nil }); err != nil {
		t.Fatalf("WriteOptional(false): %v", err)
	}
	blank, err := blankWriter.Bytes()
	if err != nil {
		t.Fatalf("blank Bytes: %v", err)
	}

	presentWriter := syntax.NewWriter()
	if err := presentWriter.WriteOptional(true, func(w *syntax.Writer) error {
		return (&ParentNode{}).MarshalMLS(w)
	}); err != nil {
		t.Fatalf("WriteOptional(true): %v", err)
	}
	present, err := presentWriter.Bytes()
	if err != nil {
		t.Fatalf("present Bytes: %v", err)
	}

	// an absent entry is the presence octet and nothing else
	if !bytes.Equal(blank, []byte{0x00}) {
		t.Errorf("a blank entry encodes to %x, want 00", blank)
	}
	// a present zero valued parent node is the presence octet and three empty length prefixes
	if !bytes.Equal(present, []byte{0x01, 0x00, 0x00, 0x00}) {
		t.Errorf("a present zero valued parent node encodes to %x, want 01000000", present)
	}
	if bytes.Equal(blank, present) {
		t.Fatalf("a blank entry and a zero valued node encode identically (%x), which is a tree that agrees with itself and with no peer", blank)
	}
}

// TestNodeTypeHasNoZeroValuedMember is what makes a Go zero value unusable as a node.
//
// A Node that was never filled in carries NodeType zero, which is neither leaf nor parent, so
// no encoder will write it and no decoder will accept it. If either constant were zero, the
// zero value of Node would be a well formed node of that kind and the blank/absent distinction
// would have nothing but a nil pointer holding it up.
func TestNodeTypeHasNoZeroValuedMember(t *testing.T) {
	if NodeTypeLeaf == 0 || NodeTypeParent == 0 {
		t.Fatalf("NodeTypeLeaf = %d and NodeTypeParent = %d; a zero member makes the zero value of Node a well formed node",
			NodeTypeLeaf, NodeTypeParent)
	}
	if NodeTypeLeaf == NodeTypeParent {
		t.Fatalf("NodeTypeLeaf and NodeTypeParent are both %d", NodeTypeLeaf)
	}
	var zero Node
	if zero.NodeType == NodeTypeLeaf || zero.NodeType == NodeTypeParent {
		t.Fatalf("the zero value of Node names a node type")
	}
	// and presence on the wire unit is a field of its own rather than something derived from
	// whether the node looks filled in
	absent := OptionalNode{}
	if absent.Present {
		t.Fatalf("the zero value of OptionalNode reports present")
	}
	if (OptionalNode{Present: true}).Present == absent.Present {
		t.Fatalf("OptionalNode.Present does not distinguish a present entry from an absent one")
	}
}

// ---------------------------------------------------------------------------
// the trees every structural sweep judges
// ---------------------------------------------------------------------------

// treeCase is one tree a caller can be holding, together with the exact set of node indices
// occupied in it. build answers a FRESH tree per use, so a sweep that blanks or installs cannot
// reach the next assertion through a tree it shared.
type treeCase struct {
	name      string
	leafWidth uint32
	nodeWidth uint32
	occupied  map[NodeIndex]bool
	build     func(t *testing.T) *RatchetTree
}

// treeWithOccupancy answers the tree of this leaf width whose occupied positions are exactly the
// ones the predicate accepts.
//
// The width is reached by installing the rightmost leaf and blanking it again, because the
// container grows only through SetLeaf and RFC 9420 section 7.7 grows it by doubling. That is
// the only route a caller has to a wide tree whose leftmost positions are blank -- which is what
// a group whose first members left is -- and reaching in past the accessors to build one would
// put this helper's own idea of a blank into the trees the sweeps then judge.
func treeWithOccupancy(t *testing.T, leafWidth uint32, isOccupied func(NodeIndex) bool) *RatchetTree {
	t.Helper()
	tree := NewRatchetTree()
	rightmost := LeafIndex(leafWidth - 1)
	if err := tree.SetLeaf(rightmost, testTreeLeaf(leafWidth-1)); err != nil {
		t.Fatalf("SetLeaf(%d) growing to %d leaves: %v", rightmost, leafWidth, err)
	}
	if err := tree.Blank(rightmost.NodeIndex()); err != nil {
		t.Fatalf("Blank(%d): %v", rightmost.NodeIndex(), err)
	}
	if tree.LeafWidth() != LeafCount(leafWidth) {
		t.Fatalf("growing to %d leaves produced a %d leaf tree", leafWidth, tree.LeafWidth())
	}
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		index := NodeIndex(x)
		if !isOccupied(index) {
			continue
		}
		if index.IsLeaf() {
			leafIndex, err := index.LeafIndex()
			if err != nil {
				t.Fatalf("LeafIndex(%d): %v", x, err)
			}
			if err := tree.SetLeaf(leafIndex, testTreeLeaf(uint32(leafIndex))); err != nil {
				t.Fatalf("SetLeaf(%d): %v", leafIndex, err)
			}
			continue
		}
		// every occupied parent carries an unmerged leaf, so a sweep that asks a parent for
		// its unmerged list is asking one that has something to answer.
		//
		// The leaf is the first of the node's OWN subtree, and it was the node index reused as a
		// leaf index -- which is a leaf the tree does not have at every parent above level one:
		// node 5 of a four leaf tree named leaf 5, and node 13 of an eight leaf tree named leaf
		// 13. RFC 9420 section 7.9 required a leaf of the node's subtree all along and nothing
		// here refused it, so these fixtures carried a shape no validator accepts into three
		// structural sweeps; SetParent's range check is what surfaced it.
		firstLeaf, _ := SubtreeLeaves(index)
		if err := tree.SetParent(index, &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(byte(0xc0+x), 32)),
			UnmergedLeaves: []LeafIndex{firstLeaf},
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	return tree
}

// The name of the one member of the family that is the constructor's own answer, so the
// coverage gate can say it judged that tree rather than a rebuild of it.
const constructedTreeCaseName = "the constructor's answer"

// treeCasesUnderTest is every tree the structural sweeps below judge, derived from the widths the
// container can hold rather than from the handful one helper happens to build.
//
// The family this replaced was one tree per width and every one of them came out of
// treeUnderTest, which fills leaf 0 first. So no sweep in this file had ever seen a tree whose
// leaf 0 was blank, and none of them had ever seen the tree the constructor hands back: a
// NewRatchetTree answering a one leaf tree that held a zero valued *Node rather than an absent
// entry -- occupied and empty at once, the conflation tree.go's file header says SetLeaf and
// SetParent refuse to create -- passed the whole suite. The blank sweep derived its POSITIONS
// from the node width and did not derive its TREES.
//
// So the trees are derived as well, and the derivation is stated as coverage rather than as a
// list: every position of every width has to be seen blank in one member of the family and
// occupied in another, which the gate below asserts of this family before any sweep uses it.
// Exhaustively over every occupancy at the two widths small enough to afford it, and by the
// single occupied and single blank families above that, which is what makes the coverage claim
// true at fifteen positions without thirty two thousand trees.
//
// The constructor's own answer is a member in its own right, and it is the one member no builder
// here reproduces: every other tree in the family has been through SetLeaf.
func treeCasesUnderTest(t *testing.T) []treeCase {
	t.Helper()
	cases := []treeCase{{
		name:      constructedTreeCaseName,
		leafWidth: 1,
		nodeWidth: 1,
		occupied:  map[NodeIndex]bool{},
		build:     func(t *testing.T) *RatchetTree { return NewRatchetTree() },
	}}
	add := func(name string, leafWidth uint32, isOccupied func(NodeIndex) bool) {
		nodeWidth := 2*leafWidth - 1
		occupied := map[NodeIndex]bool{}
		for x := uint32(0); x < nodeWidth; x += 1 {
			if isOccupied(NodeIndex(x)) {
				occupied[NodeIndex(x)] = true
			}
		}
		cases = append(cases, treeCase{
			name:      name,
			leafWidth: leafWidth,
			nodeWidth: nodeWidth,
			occupied:  occupied,
			build: func(t *testing.T) *RatchetTree {
				return treeWithOccupancy(t, leafWidth, isOccupied)
			},
		})
	}
	// the leaf widths are the powers of two, because RFC 9420 section 7.7 grows and shrinks by
	// doubling and halving and no other width is reachable
	for _, leafWidth := range []uint32{1, 2, 4, 8} {
		nodeWidth := 2*leafWidth - 1
		if nodeWidth <= 3 {
			for mask := uint32(0); mask < 1<<nodeWidth; mask += 1 {
				occupancy := mask
				add(fmt.Sprintf("%d leaves, occupancy %0*b", leafWidth, nodeWidth, occupancy), leafWidth,
					func(x NodeIndex) bool { return occupancy&(1<<uint32(x)) != 0 })
			}
			continue
		}
		add(fmt.Sprintf("%d leaves, every position blank", leafWidth), leafWidth,
			func(x NodeIndex) bool { return false })
		add(fmt.Sprintf("%d leaves, every position occupied", leafWidth), leafWidth,
			func(x NodeIndex) bool { return true })
		for x := uint32(0); x < nodeWidth; x += 1 {
			only := NodeIndex(x)
			add(fmt.Sprintf("%d leaves, only node %d occupied", leafWidth, x), leafWidth,
				func(candidate NodeIndex) bool { return candidate == only })
			add(fmt.Sprintf("%d leaves, only node %d blank", leafWidth, x), leafWidth,
				func(candidate NodeIndex) bool { return candidate != only })
		}
	}
	return cases
}

// assertTheTreeAgreesWithItsOccupancy asks every accessor of the container about every position
// of one tree and holds the answers against the set of positions that were actually filled.
//
// One helper rather than one assertion per test, because the property is the same at every entry
// point: a position is either ABSENT -- nil from Get, blank from IsBlank, nothing from Leaf or
// ParentAt, outside Members -- or it holds a node, and there is no third state. Occupied and
// empty at once is the blank as zero valued node conflation, and it reaches a tree through
// whichever accessor nobody thought to ask.
func assertTheTreeAgreesWithItsOccupancy(t *testing.T, label string, tree *RatchetTree, occupied map[NodeIndex]bool, nodeWidth uint32) {
	t.Helper()
	if tree.NodeWidth() != nodeWidth {
		t.Errorf("%s: node width %d, want %d", label, tree.NodeWidth(), nodeWidth)
		return
	}
	members := []LeafIndex{}
	for x := uint32(0); x < nodeWidth; x += 1 {
		index := NodeIndex(x)
		wantBlank := !occupied[index]
		if tree.IsBlank(index) != wantBlank {
			t.Errorf("%s: IsBlank(%d) = %v, want %v", label, x, tree.IsBlank(index), wantBlank)
		}
		if (tree.Get(index) == nil) != wantBlank {
			t.Errorf("%s: Get(%d) == nil is %v, want %v", label, x, tree.Get(index) == nil, wantBlank)
		}
		if index.IsLeaf() {
			leafIndex, err := index.LeafIndex()
			if err != nil {
				t.Fatalf("%s: LeafIndex(%d): %v", label, x, err)
			}
			if (tree.Leaf(leafIndex) == nil) != wantBlank {
				t.Errorf("%s: Leaf(%d) == nil is %v, want %v", label, leafIndex, tree.Leaf(leafIndex) == nil, wantBlank)
			}
			if !wantBlank {
				members = append(members, leafIndex)
			}
			continue
		}
		if (tree.ParentAt(index) == nil) != wantBlank {
			t.Errorf("%s: ParentAt(%d) == nil is %v, want %v", label, x, tree.ParentAt(index) == nil, wantBlank)
		}
	}
	if tree.MemberCount() != uint32(len(members)) {
		t.Errorf("%s: MemberCount = %d, want %d", label, tree.MemberCount(), len(members))
	}
	if got := tree.Members(); !reflect.DeepEqual(got, members) {
		t.Errorf("%s: Members = %v, want %v", label, got, members)
	}
}

// assertEveryOccupiedPositionCarriesItsType derives the node type from the index parity and
// answers how many leaves and how many parents it examined, so a caller can say what it judged.
func assertEveryOccupiedPositionCarriesItsType(t *testing.T, label string, tree *RatchetTree, occupied map[NodeIndex]bool) (int, int) {
	t.Helper()
	leaves, parents := 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		index := NodeIndex(x)
		node := tree.Get(index)
		if node == nil {
			if occupied[index] {
				t.Errorf("%s: node %d was filled and reads blank", label, x)
			}
			continue
		}
		want := NodeTypeParent
		if index.IsLeaf() {
			want = NodeTypeLeaf
		}
		if node.NodeType != want {
			t.Errorf("%s: node %d carries type %d, want %d", label, x, node.NodeType, want)
		}
		if index.IsLeaf() {
			leaves += 1
			if node.Leaf == nil || node.Parent != nil {
				t.Errorf("%s: node %d is a leaf index holding leaf=%v parent=%v", label, x, node.Leaf != nil, node.Parent != nil)
			}
			continue
		}
		parents += 1
		if node.Parent == nil || node.Leaf != nil {
			t.Errorf("%s: node %d is a parent index holding leaf=%v parent=%v", label, x, node.Leaf != nil, node.Parent != nil)
		}
	}
	return leaves, parents
}

// TestTheFamilyOfTreesTheSweepsJudgeCoversEveryPositionInBothStates is the derivation's own
// gate, and it runs before any sweep uses the family.
//
// A sweep is worth exactly what it was pointed at. The family this replaced covered every
// position of every width in the OCCUPIED state and covered position 0 in the blank state at no
// width at all, which is how a constructor handing back an occupied position where the whole
// file says there is a blank one passed the package. So the claim the sweeps rest on is made
// here and made mechanically: at every width, every position appears blank in some member of
// the family and occupied in some other, some member holds nothing at all, and the constructor's
// own answer is one of the members.
func TestTheFamilyOfTreesTheSweepsJudgeCoversEveryPositionInBothStates(t *testing.T) {
	cases := treeCasesUnderTest(t)
	if len(cases) == 0 {
		t.Fatal("the family is empty, so every sweep over it judges nothing")
	}
	seenBlank := map[uint32]map[uint32]bool{}
	seenOccupied := map[uint32]map[uint32]bool{}
	widths := []uint32{}
	constructed, allBlank := 0, 0
	for _, one := range cases {
		if seenBlank[one.nodeWidth] == nil {
			seenBlank[one.nodeWidth] = map[uint32]bool{}
			seenOccupied[one.nodeWidth] = map[uint32]bool{}
			widths = append(widths, one.nodeWidth)
		}
		for x := uint32(0); x < one.nodeWidth; x += 1 {
			if one.occupied[NodeIndex(x)] {
				seenOccupied[one.nodeWidth][x] = true
			} else {
				seenBlank[one.nodeWidth][x] = true
			}
		}
		if len(one.occupied) == 0 {
			allBlank += 1
		}
		if one.name == constructedTreeCaseName {
			constructed += 1
		}
	}
	slices.Sort(widths)
	for _, nodeWidth := range widths {
		for x := uint32(0); x < nodeWidth; x += 1 {
			if !seenBlank[nodeWidth][x] {
				t.Errorf("no tree of node width %d in the family has node %d blank, so no sweep over it can tell a blank there from a node", nodeWidth, x)
			}
			if !seenOccupied[nodeWidth][x] {
				t.Errorf("no tree of node width %d in the family has node %d occupied, so no sweep over it can tell a node there from a blank", nodeWidth, x)
			}
		}
	}
	if allBlank == 0 {
		t.Error("no member of the family holds nothing at all, which is the shape a constructor hands back and the shape every sweep here used to miss")
	}
	if constructed != 1 {
		t.Errorf("%d members of the family are %q; the constructor's answer is the one tree no builder here reproduces and it has to be judged as itself", constructed, constructedTreeCaseName)
	}
	t.Logf("%d trees over %d widths, %d of them holding nothing", len(cases), len(widths), allBlank)
}

// TestBlankPositionsAreExactlyTheUnsetOnesAtEveryWidth sweeps every node index of every tree in
// the derived family and requires blankness to agree, at every accessor, with the set of
// positions that were actually filled.
//
// Derived from the width AND from the family rather than from a handful of named indices in a
// handful of trees, because the defect this is about -- a blank materialised into an occupied
// position holding a zero valued node -- can hide at any index of any tree a sweep does not
// visit, and a tree that round trips against itself will not report it anywhere else. The tree
// the constructor hands back is where it hid last time.
//
// Every case is judged twice: as built, and cloned. A deep copy that turned a nil entry into an
// occupied position holding a zero valued node is the same conflation arriving through the copy
// constructor, and every provisional tree in TreeKEM is a clone.
func TestBlankPositionsAreExactlyTheUnsetOnesAtEveryWidth(t *testing.T) {
	judged := 0
	for _, one := range treeCasesUnderTest(t) {
		tree := one.build(t)
		assertTheTreeAgreesWithItsOccupancy(t, one.name, tree, one.occupied, one.nodeWidth)
		assertTheTreeAgreesWithItsOccupancy(t, one.name+", cloned", tree.Clone(), one.occupied, one.nodeWidth)
		judged += 2
	}
	if judged == 0 {
		t.Fatal("no tree was judged")
	}
	t.Logf("%d trees judged, originals and clones", judged)
}

// TestEveryOccupiedPositionCarriesTheNodeTypeItsIndexRequires derives the type from the index
// parity rather than checking a couple of positions.
//
// The wire format carries the type as an octet, so a leaf at an odd index is the shape a hostile
// ratchet_tree extension arrives in; a container that stored the wrong type would hand the tree
// hash a different structure at that position while every accessor still worked.
//
// The CLONE of every tree is walked as well as the tree, and that half is not symmetry. Every
// provisional tree in TreeKEM is built through RatchetTree.Clone, so a Node.Clone that dropped
// the node type would make every node of every provisional tree a node of no type -- the value
// TestNodeTypeHasNoZeroValuedMember exists to make unwritable and undecodable. That mutation
// passed the whole suite, because this sweep walked only the original and the clone was checked
// for blankness and for key material independence and never for the types it carried.
func TestEveryOccupiedPositionCarriesTheNodeTypeItsIndexRequires(t *testing.T) {
	leaves, parents, clonedLeaves, clonedParents := 0, 0, 0, 0
	for _, one := range treeCasesUnderTest(t) {
		tree := one.build(t)
		leafCount, parentCount := assertEveryOccupiedPositionCarriesItsType(t, one.name, tree, one.occupied)
		leaves += leafCount
		parents += parentCount
		leafCount, parentCount = assertEveryOccupiedPositionCarriesItsType(t, one.name+", cloned", tree.Clone(), one.occupied)
		clonedLeaves += leafCount
		clonedParents += parentCount
	}
	if leaves == 0 || parents == 0 {
		t.Fatalf("the sweep examined %d occupied leaves and %d occupied parents; it needs both to separate anything", leaves, parents)
	}
	if clonedLeaves == 0 || clonedParents == 0 {
		t.Fatalf("the sweep examined %d cloned leaves and %d cloned parents; a clone that carried no node type is what this half is here for", clonedLeaves, clonedParents)
	}
	t.Logf("%d leaves and %d parents examined, %d and %d of them in clones", leaves, parents, clonedLeaves, clonedParents)
}

// TestBlankingAPositionMakesItAbsentAndNotOccupiedAndEmpty walks every occupied position of a
// tree, blanks it, and requires the whole container to agree that it is gone.
//
// One position at a time against a fresh tree, so the assertions are about the position that
// was blanked and not about whatever the previous iteration left behind.
func TestBlankingAPositionMakesItAbsentAndNotOccupiedAndEmpty(t *testing.T) {
	blanked := 0
	for _, leafCount := range []uint32{1, 3, 5, 8} {
		_, occupied := treeUnderTest(t, leafCount)
		for x := uint32(0); x < uint32(len(occupied))*4; x += 1 {
			index := NodeIndex(x)
			if !occupied[index] {
				continue
			}
			tree, _ := treeUnderTest(t, leafCount)
			before := tree.MemberCount()
			if err := tree.Blank(index); err != nil {
				t.Fatalf("%d leaves: Blank(%d): %v", leafCount, x, err)
			}
			blanked += 1
			if tree.Get(index) != nil {
				t.Errorf("%d leaves: Get(%d) after Blank = %+v, want nil", leafCount, x, tree.Get(index))
			}
			if !tree.IsBlank(index) {
				t.Errorf("%d leaves: IsBlank(%d) is false after Blank", leafCount, x)
			}
			if got := tree.UnmergedLeaves(index); len(got) != 0 {
				t.Errorf("%d leaves: a blanked node reports unmerged leaves %v", leafCount, got)
			}
			if index.IsLeaf() {
				leafIndex, err := index.LeafIndex()
				if err != nil {
					t.Fatalf("LeafIndex(%d): %v", x, err)
				}
				if tree.Leaf(leafIndex) != nil {
					t.Errorf("%d leaves: Leaf(%d) after Blank is not nil", leafCount, leafIndex)
				}
				if tree.MemberCount() != before-1 {
					t.Errorf("%d leaves: blanking leaf %d moved the member count from %d to %d",
						leafCount, leafIndex, before, tree.MemberCount())
				}
				for _, member := range tree.Members() {
					if member == leafIndex {
						t.Errorf("%d leaves: leaf %d is still a member after Blank", leafCount, leafIndex)
					}
				}
				continue
			}
			if tree.ParentAt(index) != nil {
				t.Errorf("%d leaves: ParentAt(%d) after Blank is not nil", leafCount, x)
			}
			if tree.MemberCount() != before {
				t.Errorf("%d leaves: blanking parent %d moved the member count from %d to %d",
					leafCount, x, before, tree.MemberCount())
			}
		}
	}
	if blanked == 0 {
		t.Fatal("nothing was blanked, so this gate judged nothing")
	}
}

// TestSetLeafAndSetParentRefuseANilPayload. A position that is occupied and empty at once is
// the blank-as-zero-valued-node conflation arriving through the front door: IsBlank would
// answer false, Leaf would answer nil, and the encoder would have a present node with nothing
// to write. Blank is how a position is emptied.
func TestSetLeafAndSetParentRefuseANilPayload(t *testing.T) {
	tree, _ := treeUnderTest(t, 4)
	if err := tree.SetLeaf(LeafIndex(3), nil); !errors.Is(err, ErrTreeMalformed) {
		t.Errorf("SetLeaf(3, nil) err = %v, want ErrTreeMalformed", err)
	}
	if tree.Leaf(LeafIndex(3)) == nil {
		t.Errorf("the refused SetLeaf emptied a position that was occupied")
	}
	if err := tree.Blank(NodeIndex(6)); err != nil {
		t.Fatalf("Blank(6): %v", err)
	}
	if err := tree.SetLeaf(LeafIndex(3), nil); !errors.Is(err, ErrTreeMalformed) {
		t.Errorf("SetLeaf(3, nil) err = %v, want ErrTreeMalformed", err)
	}
	if !tree.IsBlank(NodeIndex(6)) {
		t.Errorf("the refused SetLeaf occupied a blank position")
	}
	if err := tree.SetParent(NodeIndex(3), nil); !errors.Is(err, ErrTreeMalformed) {
		t.Errorf("SetParent(3, nil) err = %v, want ErrTreeMalformed", err)
	}
	if !tree.IsBlank(NodeIndex(3)) {
		t.Errorf("the refused SetParent occupied a blank position")
	}
}

// ---------------------------------------------------------------------------
// the container
// ---------------------------------------------------------------------------

// TestRatchetTreeGrowsByDoubling. RFC 9420 section 7.7 grows the tree by adding a blank root
// whose left subtree is the whole existing tree, never by one leaf, so the leaf width is always
// a power of two and the widths are a function of the highest occupied index alone.
func TestRatchetTreeGrowsByDoubling(t *testing.T) {
	tree := NewRatchetTree()
	if tree.LeafWidth() != 1 || tree.NodeWidth() != 1 {
		t.Fatalf("empty tree width = (%d, %d), want (1, 1)", tree.LeafWidth(), tree.NodeWidth())
	}
	// the widths after each SetLeaf, derived from the doubling rule rather than checked only
	// at the end: a container that grew to the final width in one step would pass a test that
	// looked only after the last call
	for i, want := range []struct {
		leaves LeafCount
		nodes  uint32
	}{
		{leaves: 1, nodes: 1},
		{leaves: 2, nodes: 3},
		{leaves: 4, nodes: 7},
		{leaves: 4, nodes: 7},
		{leaves: 8, nodes: 15},
	} {
		if err := tree.SetLeaf(LeafIndex(i), testTreeLeaf(uint32(i))); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
		if tree.LeafWidth() != want.leaves || tree.NodeWidth() != want.nodes {
			t.Fatalf("after SetLeaf(%d) width = (%d, %d), want (%d, %d)",
				i, tree.LeafWidth(), tree.NodeWidth(), want.leaves, want.nodes)
		}
	}
	if tree.MemberCount() != 5 {
		t.Fatalf("member count = %d, want 5", tree.MemberCount())
	}
	members := tree.Members()
	if !reflect.DeepEqual(members, []LeafIndex{0, 1, 2, 3, 4}) {
		t.Fatalf("members = %v, want [0 1 2 3 4]", members)
	}
	if !reflect.DeepEqual(tree.NonBlankLeaves(), members) {
		t.Fatalf("NonBlankLeaves = %v and Members = %v; they are one list", tree.NonBlankLeaves(), members)
	}
}

// TestGrowingDoesNotMoveTheNodesAlreadyThere. Doubling appends, so every existing node index
// means the same position afterwards. A growth that reindexed would keep every accessor
// working and silently move every member.
func TestGrowingDoesNotMoveTheNodesAlreadyThere(t *testing.T) {
	tree := NewRatchetTree()
	for i := uint32(0); i < 2; i += 1 {
		if err := tree.SetLeaf(LeafIndex(i), testTreeLeaf(i)); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
	}
	if err := tree.SetParent(NodeIndex(1), testParentNodeInside(tree)); err != nil {
		t.Fatalf("SetParent(1): %v", err)
	}
	leafZero, leafOne, parentOne := tree.Get(NodeIndex(0)), tree.Get(NodeIndex(2)), tree.Get(NodeIndex(1))

	if err := tree.SetLeaf(LeafIndex(9), testTreeLeaf(9)); err != nil {
		t.Fatalf("SetLeaf(9): %v", err)
	}
	if tree.LeafWidth() != 16 || tree.NodeWidth() != 31 {
		t.Fatalf("after growing to hold leaf 9, width = (%d, %d), want (16, 31)", tree.LeafWidth(), tree.NodeWidth())
	}
	if tree.Get(NodeIndex(0)) != leafZero || tree.Get(NodeIndex(2)) != leafOne || tree.Get(NodeIndex(1)) != parentOne {
		t.Fatalf("growing moved the nodes that were already in the tree")
	}
	if tree.Leaf(LeafIndex(9)) == nil {
		t.Fatalf("leaf 9 is not in the grown tree")
	}
	if !reflect.DeepEqual(tree.Members(), []LeafIndex{0, 1, 9}) {
		t.Fatalf("members after growing = %v, want [0 1 9]", tree.Members())
	}
}

// TestRatchetTreeSetAndBlank covers the refusals each setter makes at its own boundary.
func TestRatchetTreeSetAndBlank(t *testing.T) {
	tree, _ := treeUnderTest(t, 4)
	parent := &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x55, 32)),
		ParentHash:     repeatByte(0x66, 32),
		UnmergedLeaves: []LeafIndex{2, 3},
	}
	if err := tree.SetParent(NodeIndex(3), parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if got := tree.ParentAt(NodeIndex(3)); got == nil || !reflect.DeepEqual(got.UnmergedLeaves, []LeafIndex{2, 3}) {
		t.Fatalf("ParentAt(3) = %+v", got)
	}
	if tree.ParentAt(NodeIndex(0)) != nil {
		t.Fatalf("ParentAt(0) on a leaf index returned a parent node")
	}
	if err := tree.SetParent(NodeIndex(2), parent); !errors.Is(err, ErrNodeTypeMismatch) {
		t.Fatalf("SetParent on an even index err = %v, want ErrNodeTypeMismatch", err)
	}
	// a parent index past the end is a different refusal from an index of the wrong parity,
	// and a caller repairing the first re-derives an index while the second means the tree is
	// not the tree it thought
	if err := tree.SetParent(NodeIndex(tree.NodeWidth()), parent); !errors.Is(err, ErrNodeIndexOutOfRange) {
		t.Fatalf("SetParent past the end err = %v, want ErrNodeIndexOutOfRange", err)
	}
	if err := tree.Blank(NodeIndex(tree.NodeWidth())); !errors.Is(err, ErrNodeIndexOutOfRange) {
		t.Fatalf("Blank past the end err = %v, want ErrNodeIndexOutOfRange", err)
	}
	// SetLeaf grows rather than refusing, which is the whole difference between the two
	if err := tree.SetLeaf(LeafIndex(99), testTreeLeaf(99)); err != nil {
		t.Fatalf("SetLeaf(99) should grow the tree: %v", err)
	}
	if tree.LeafWidth() != 128 {
		t.Fatalf("after SetLeaf(99) leaf width = %d, want 128", tree.LeafWidth())
	}
	if err := tree.SetLeaf(LeafIndex(MaxLeafCount), testTreeLeaf(0)); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("SetLeaf at MaxLeafCount err = %v, want ErrLeafIndexOutOfRange", err)
	}
	if err := tree.Blank(NodeIndex(3)); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if tree.Get(NodeIndex(3)) != nil {
		t.Fatalf("node 3 is not blank after Blank")
	}
}

// TestRatchetTreeBlankDirectPath requires exactly the direct path to be blanked: the tree math
// decides which nodes those are, and everything else the tree held must survive.
//
// Derived from DirectPath rather than from three named indices, and the survivors are checked
// as well as the casualties -- a BlankDirectPath that blanked the whole array would satisfy a
// test that only looked at the path.
func TestRatchetTreeBlankDirectPath(t *testing.T) {
	tree, occupied := treeUnderTest(t, 4)
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if occupied[NodeIndex(x)] {
			continue
		}
		if err := tree.SetParent(NodeIndex(x), &ParentNode{
			EncryptionKey: HpkePublicKey(repeatByte(byte(x), 32)),
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
		occupied[NodeIndex(x)] = true
	}

	path, err := DirectPath(LeafIndex(0).NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("DirectPath: %v", err)
	}
	if len(path) == 0 {
		t.Fatal("the direct path of leaf 0 in a four leaf tree is empty, so this test blanks nothing")
	}
	onPath := map[NodeIndex]bool{}
	for _, x := range path {
		onPath[x] = true
	}

	if err := tree.BlankDirectPath(LeafIndex(0)); err != nil {
		t.Fatalf("BlankDirectPath: %v", err)
	}
	survived := 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		index := NodeIndex(x)
		wantBlank := onPath[index] || !occupied[index]
		if tree.IsBlank(index) != wantBlank {
			t.Errorf("after BlankDirectPath(0), IsBlank(%d) = %v, want %v (on path %v, occupied %v)",
				x, tree.IsBlank(index), wantBlank, onPath[index], occupied[index])
		}
		if !wantBlank {
			survived += 1
		}
	}
	if survived == 0 {
		t.Fatal("BlankDirectPath blanked everything, so nothing separates it from Blank over the whole array")
	}
	if tree.Leaf(LeafIndex(0)) == nil {
		t.Fatal("BlankDirectPath must not blank the leaf itself")
	}
	if err := tree.BlankDirectPath(LeafIndex(tree.LeafWidth())); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("BlankDirectPath past the width err = %v, want ErrLeafIndexOutOfRange", err)
	}
}

// TestRatchetTreeCloneIsIndependent. Nothing may alias between two epochs' trees: a commit that
// is later rejected must leave the epoch it was computed against exactly as it found it, and a
// shared backing array is how a rejected commit mutates a tree that never accepted it.
func TestRatchetTreeCloneIsIndependent(t *testing.T) {
	tree, _ := treeUnderTest(t, 4)
	if err := tree.SetParent(NodeIndex(3), testParentNodeInside(tree)); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	clone := tree.Clone()
	if err := clone.Blank(NodeIndex(0)); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if tree.Leaf(LeafIndex(0)) == nil {
		t.Fatal("blanking the clone blanked the original")
	}

	clone2 := tree.Clone()
	clone2.Leaf(LeafIndex(0)).EncryptionKey[0] ^= 0xff
	if tree.Leaf(LeafIndex(0)).EncryptionKey[0] == clone2.Leaf(LeafIndex(0)).EncryptionKey[0] {
		t.Error("Clone shares leaf key material with the original")
	}
	clone2.ParentAt(NodeIndex(3)).EncryptionKey[0] ^= 0xff
	if tree.ParentAt(NodeIndex(3)).EncryptionKey[0] == clone2.ParentAt(NodeIndex(3)).EncryptionKey[0] {
		t.Error("Clone shares parent key material with the original")
	}
	clone2.ParentAt(NodeIndex(3)).UnmergedLeaves[0] = 0xffff
	if tree.ParentAt(NodeIndex(3)).UnmergedLeaves[0] == clone2.ParentAt(NodeIndex(3)).UnmergedLeaves[0] {
		t.Error("Clone shares the unmerged leaf list with the original")
	}
	// and installing into the clone does not reach the original's array
	if err := clone2.SetLeaf(LeafIndex(7), testTreeLeaf(7)); err != nil {
		t.Fatalf("SetLeaf on the clone: %v", err)
	}
	if tree.Leaf(LeafIndex(7)) != nil || tree.LeafWidth() != 4 {
		t.Errorf("growing the clone reached the original: width %d, leaf 7 present %v", tree.LeafWidth(), tree.Leaf(LeafIndex(7)) != nil)
	}
}

// TestRatchetTreeFindLeafBySignatureKey. Every leaf of the test tree carries a distinct
// signature key, so an implementation answering the wrong position fails rather than passing
// against a tree where every answer is the same.
func TestRatchetTreeFindLeafBySignatureKey(t *testing.T) {
	tree, _ := treeUnderTest(t, 3)
	for i := uint32(0); i < 3; i += 1 {
		got, ok := tree.FindLeafBySignatureKey(SignaturePublicKey(repeatByte(byte(0x80+i), 32)))
		if !ok || got != LeafIndex(i) {
			t.Errorf("FindLeafBySignatureKey for leaf %d = (%d, %v), want (%d, true)", i, got, ok, i)
		}
	}
	if _, ok := tree.FindLeafBySignatureKey(SignaturePublicKey(repeatByte(0x09, 32))); ok {
		t.Error("FindLeafBySignatureKey found an absent key")
	}
	// a prefix of a present key is not that key: the comparison is over the whole value, and a
	// length insensitive one would answer here
	if _, ok := tree.FindLeafBySignatureKey(SignaturePublicKey(repeatByte(0x80, 31))); ok {
		t.Error("FindLeafBySignatureKey accepted a prefix of a present key")
	}
	if _, ok := tree.FindLeafBySignatureKey(nil); ok {
		t.Error("FindLeafBySignatureKey accepted a nil key")
	}
	if err := tree.Blank(LeafIndex(1).NodeIndex()); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if _, ok := tree.FindLeafBySignatureKey(SignaturePublicKey(repeatByte(0x81, 32))); ok {
		t.Error("FindLeafBySignatureKey found a leaf that has been blanked")
	}
}

// TestEncryptionKeyInUseReadsLeavesAndParentsAlike. ValSem103 and ValSem110 ask this once per
// proposal, and a check that looked only at leaves would accept an add whose key is already a
// parent's -- a member that can decrypt a path secret it was never sent.
func TestEncryptionKeyInUseReadsLeavesAndParentsAlike(t *testing.T) {
	tree, _ := treeUnderTest(t, 3)
	for i := uint32(0); i < 3; i += 1 {
		if !tree.EncryptionKeyInUse(HpkePublicKey(repeatByte(byte(0x40+i), 32))) {
			t.Errorf("EncryptionKeyInUse missed leaf %d's key", i)
		}
	}
	if tree.EncryptionKeyInUse(HpkePublicKey(repeatByte(0x99, 32))) {
		t.Error("EncryptionKeyInUse found an absent key")
	}
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x50, 32)),
		UnmergedLeaves: []LeafIndex{1},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if !tree.EncryptionKeyInUse(HpkePublicKey(repeatByte(0x50, 32))) {
		t.Error("EncryptionKeyInUse ignored a parent node key")
	}
	if err := tree.Blank(NodeIndex(1)); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if tree.EncryptionKeyInUse(HpkePublicKey(repeatByte(0x50, 32))) {
		t.Error("EncryptionKeyInUse answered for a node that has been blanked")
	}
}

// TestRatchetTreeIsTheNodeShapeTheTreeMathWalks. Implementing NodeShape is what lets the tree
// math plan's Resolution and FilteredDirectPath run against a real tree, which is why this plan
// has no resolution algorithm of its own.
func TestRatchetTreeIsTheNodeShapeTheTreeMathWalks(t *testing.T) {
	tree, _ := treeUnderTest(t, 3)
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x50, 32)),
		UnmergedLeaves: []LeafIndex{1},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	var shape NodeShape = tree

	// three leaves means a width of four, stated rather than read back off the tree: a
	// comparison against tree.LeafWidth() is the same expression on both sides
	if shape.LeafCount() != LeafCount(4) {
		t.Errorf("NodeShape.LeafCount = %d, want 4", shape.LeafCount())
	}
	if shape.IsBlank(NodeIndex(0)) {
		t.Error("leaf 0 reported blank")
	}
	if !shape.IsBlank(NodeIndex(6)) {
		t.Error("leaf 3 is unoccupied and must report blank")
	}
	if got := shape.UnmergedLeaves(NodeIndex(1)); !reflect.DeepEqual(got, []LeafIndex{1}) {
		t.Errorf("NodeShape.UnmergedLeaves(1) = %v, want [1]", got)
	}
	if got := shape.UnmergedLeaves(NodeIndex(3)); len(got) != 0 {
		t.Errorf("a blank parent has no unmerged leaves, got %v", got)
	}
	if got := shape.UnmergedLeaves(NodeIndex(0)); len(got) != 0 {
		t.Errorf("a leaf has no unmerged leaves, got %v", got)
	}

	// and the resolution the tree math computes over this shape is the one RFC 9420 section
	// 4.1 defines, which is where blankness stops being a fact about this container and
	// starts deciding who a path secret is encrypted to. Node 3 is the root of a four leaf
	// tree and is blank, so it resolves to its children's resolutions: node 1 is occupied and
	// carries leaf 1 as an unmerged leaf, so it is followed immediately by node 2, and node 5
	// is occupied so it stands for the whole right subtree.
	resolution, err := Resolution(shape, NodeIndex(3))
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if !reflect.DeepEqual(resolution, []NodeIndex{1, 2, 5}) {
		t.Errorf("Resolution(3) = %v, want [1 2 5]", resolution)
	}
	// blanking node 5 pushes the walk down to its children: node 4 is leaf 2 and occupied, so
	// it stands in; node 6 is leaf 3 and blank, and a blank LEAF resolves to nothing at all,
	// which is the one place where treating a blank as a zero valued node would put a member
	// that does not exist into the list a path secret is encrypted to.
	if err := tree.Blank(NodeIndex(5)); err != nil {
		t.Fatalf("Blank(5): %v", err)
	}
	resolution, err = Resolution(shape, NodeIndex(3))
	if err != nil {
		t.Fatalf("Resolution after blanking node 5: %v", err)
	}
	if !reflect.DeepEqual(resolution, []NodeIndex{1, 2, 4}) {
		t.Errorf("Resolution(3) after blanking node 5 = %v, want [1 2 4]", resolution)
	}
}

// TestHasTrailingBlankNodesIsTheLastPositionAndNotAnyBlank. RFC 9420 section 12.4.3.3 forbids
// an exported ratchet_tree ending in a blank; it says nothing about blanks in the middle, and a
// predicate that reported "any blank" would refuse almost every real tree.
func TestHasTrailingBlankNodesIsTheLastPositionAndNotAnyBlank(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		leaves  []uint32
		trailer bool
	}{
		{name: "one leaf, full", leaves: []uint32{0}, trailer: false},
		{name: "two leaves, full", leaves: []uint32{0, 1}, trailer: false},
		{name: "three of four leaves", leaves: []uint32{0, 1, 2}, trailer: true},
		{name: "a hole in the middle", leaves: []uint32{0, 3}, trailer: false},
		{name: "only the last leaf", leaves: []uint32{1}, trailer: false},
		{name: "only the first leaf of two", leaves: []uint32{0, 1}, trailer: false},
	} {
		tree := NewRatchetTree()
		for _, i := range testCase.leaves {
			if err := tree.SetLeaf(LeafIndex(i), testTreeLeaf(i)); err != nil {
				t.Fatalf("%s: SetLeaf(%d): %v", testCase.name, i, err)
			}
		}
		if tree.HasTrailingBlankNodes() != testCase.trailer {
			t.Errorf("%s: HasTrailingBlankNodes = %v, want %v (node width %d)",
				testCase.name, tree.HasTrailingBlankNodes(), testCase.trailer, tree.NodeWidth())
		}
	}
	// and blanking the last leaf of a full tree turns it on, which is the transition ValSem300
	// is about
	tree, _ := treeUnderTest(t, 4)
	if tree.HasTrailingBlankNodes() {
		t.Fatal("a four leaf tree with every leaf occupied reports a trailing blank")
	}
	if err := tree.Blank(LeafIndex(3).NodeIndex()); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if !tree.HasTrailingBlankNodes() {
		t.Error("blanking the rightmost leaf did not produce a trailing blank")
	}
}

// TestTheAccessorsRefuseAnIndexPastTheWidthRatherThanWrapping.
//
// LeafIndex.NodeIndex is total and wraps: leaf 2^31 answers node 0. Without a leaf range check
// of its own, Leaf would hand back leaf 0's contents for it, which is a member reading another
// member's credential and believing it is their own.
func TestTheAccessorsRefuseAnIndexPastTheWidthRatherThanWrapping(t *testing.T) {
	tree, _ := treeUnderTest(t, 2)
	if tree.Leaf(LeafIndex(0)) == nil {
		t.Fatal("leaf 0 is not in the tree, so the wrap below proves nothing")
	}
	for _, i := range []LeafIndex{LeafIndex(tree.LeafWidth()), LeafIndex(1 << 31), LeafIndex(0xffffffff)} {
		if got := tree.Leaf(i); got != nil {
			t.Errorf("Leaf(%d) on a %d leaf tree = %p, want nil", i, tree.LeafWidth(), got)
		}
	}
	for _, x := range []NodeIndex{NodeIndex(tree.NodeWidth()), NodeIndex(0xffffffff)} {
		if tree.Get(x) != nil {
			t.Errorf("Get(%d) past the end is not nil", x)
		}
		if !tree.IsBlank(x) {
			t.Errorf("IsBlank(%d) past the end is false", x)
		}
		if tree.ParentAt(x) != nil {
			t.Errorf("ParentAt(%d) past the end is not nil", x)
		}
	}
}

// ---------------------------------------------------------------------------
// what the container owns and what it hands back
// ---------------------------------------------------------------------------

// TestInstallingANodeCopiesItAndTheAccessorsHandBackTheStoredOne states the ownership contract
// in the one direction it has, in both halves.
//
// It was unpinned in BOTH directions before: SetLeaf adopting the caller's pointer passed, and
// SetLeaf storing a copy passed, so a later task could flip it with nothing failing. The
// direction chosen is the one RatchetTree.Clone's own comment requires -- nothing may alias
// between two epochs' trees -- because an install that adopted would make
// tree.SetLeaf(i, other.Leaf(j)) put one *LeafNode in two of them, after which a commit computed
// against one epoch and later rejected has already written through the other.
//
// The second half is the part that makes the first usable: the accessors hand back the tree's
// OWN node, so a caller that means to keep editing reads it back rather than holding on to what
// it passed in. A container that copied on the way in and on the way out would pass the first
// half of this and leave no way to edit a tree at all.
func TestInstallingANodeCopiesItAndTheAccessorsHandBackTheStoredOne(t *testing.T) {
	tree, _ := treeUnderTest(t, 4)

	installed := testTreeLeaf(9)
	if err := tree.SetLeaf(LeafIndex(1), installed); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if stored := tree.Leaf(LeafIndex(1)); stored == installed {
		t.Error("SetLeaf stored the caller's own *LeafNode, so the caller and the tree share one node")
	}
	installed.SignatureKey[0] ^= 0xff
	if tree.Leaf(LeafIndex(1)).SignatureKey[0] == installed.SignatureKey[0] {
		t.Error("writing through the leaf that was installed reached the tree, so SetLeaf adopted it rather than copying it")
	}
	// and the accessor is the way in: this is how a caller edits a leaf it has installed
	before := tree.Leaf(LeafIndex(1)).SignatureKey[0]
	tree.Leaf(LeafIndex(1)).SignatureKey[0] ^= 0xff
	if tree.Leaf(LeafIndex(1)).SignatureKey[0] == before {
		t.Error("writing through Leaf did not reach the tree, so the tree hands out a copy and cannot be edited at all")
	}

	installedParent := testParentNodeInside(tree)
	if err := tree.SetParent(NodeIndex(1), installedParent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if stored := tree.ParentAt(NodeIndex(1)); stored == installedParent {
		t.Error("SetParent stored the caller's own *ParentNode")
	}
	installedParent.EncryptionKey[0] ^= 0xff
	if tree.ParentAt(NodeIndex(1)).EncryptionKey[0] == installedParent.EncryptionKey[0] {
		t.Error("writing through the parent node that was installed reached the tree")
	}
	installedParent.UnmergedLeaves[0] = 0xffff
	if tree.ParentAt(NodeIndex(1)).UnmergedLeaves[0] == installedParent.UnmergedLeaves[0] {
		t.Error("the installed parent node shares its unmerged leaf list with the tree")
	}

	// the aliasing this closes, spelled as the operation that would have produced it: one leaf
	// taken out of one tree and installed in another
	other := NewRatchetTree()
	if err := other.SetLeaf(LeafIndex(0), tree.Leaf(LeafIndex(1))); err != nil {
		t.Fatalf("SetLeaf on the second tree: %v", err)
	}
	other.Leaf(LeafIndex(0)).SignatureKey[0] ^= 0xff
	if tree.Leaf(LeafIndex(1)).SignatureKey[0] == other.Leaf(LeafIndex(0)).SignatureKey[0] {
		t.Error("one leaf is installed in two trees at once, which is the aliasing between two epochs' trees that Clone exists to prevent")
	}
}

// TestNodeShapeUnmergedLeavesHandsBackACopyAndNotTheTreesOwnList is the same contract at the one
// accessor whose answer leaves the package.
//
// This is the NodeShape method the tree math walks, so its answer reaches code that has no idea
// it is holding a tree's own storage. Measured before this was pinned: the ordinary go idiom for
// narrowing a list -- kept := answer[:0] followed by append, which is the spelling the tree hash
// task uses -- rewrote a real parent node's unmerged list in place, which is a different parent
// hash at that node and a different tree hash from every peer. It was unpinned in both
// directions, so a later task could have flipped it back silently.
func TestNodeShapeUnmergedLeavesHandsBackACopyAndNotTheTreesOwnList(t *testing.T) {
	tree, _ := treeUnderTest(t, 4)
	installed := testParentNodeInside(tree)
	if err := tree.SetParent(NodeIndex(3), installed); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	// read off what was installed rather than written out again, so the narrowing
	// testParentNodeInside does cannot leave this comparing against a list the tree never held
	stored := slices.Clone(installed.UnmergedLeaves)
	if len(stored) < 2 {
		t.Fatalf("the installed unmerged list is %v and what follows needs at least two entries", stored)
	}
	var shape NodeShape = tree
	if got := shape.UnmergedLeaves(NodeIndex(3)); !reflect.DeepEqual(got, stored) {
		t.Fatalf("UnmergedLeaves(3) = %v, want %v, so what follows measures the wrong list", got, stored)
	}

	// narrowing the answer in place, which is what a caller filtering the list writes
	kept := shape.UnmergedLeaves(NodeIndex(3))[:0]
	kept = append(kept, LeafIndex(9))
	if got := shape.UnmergedLeaves(NodeIndex(3)); !reflect.DeepEqual(got, stored) {
		t.Errorf("a caller that narrowed the answer in place rewrote the tree: the node now holds %v, want %v", got, stored)
	}
	// and writing through an element of it
	answer := shape.UnmergedLeaves(NodeIndex(3))
	answer[0] = 0xffff
	if got := shape.UnmergedLeaves(NodeIndex(3)); !reflect.DeepEqual(got, stored) {
		t.Errorf("a caller that wrote through the answer rewrote the tree: the node now holds %v, want %v", got, stored)
	}
	// two answers do not share storage with each other either, which is the same statement made
	// where a caller holds both
	first, second := shape.UnmergedLeaves(NodeIndex(3)), shape.UnmergedLeaves(NodeIndex(3))
	first[0] = 0xeeee
	if second[0] == first[0] {
		t.Error("two answers from UnmergedLeaves share one backing array")
	}
	// the parent node itself is still reachable and still editable through ParentAt, which is
	// the accessor that hands out the tree's own storage on purpose
	tree.ParentAt(NodeIndex(3)).UnmergedLeaves[0] = 0xabcd
	if got := shape.UnmergedLeaves(NodeIndex(3))[0]; got != 0xabcd {
		t.Errorf("an edit through ParentAt did not reach the answer: got %d", got)
	}
	// a blank position and a leaf answer nothing, which is where an implementation reaching
	// through a nil parent would panic rather than report
	if got := shape.UnmergedLeaves(NodeIndex(5)); len(got) != 0 {
		t.Errorf("a blank parent answered %v", got)
	}
	if got := shape.UnmergedLeaves(NodeIndex(0)); len(got) != 0 {
		t.Errorf("a leaf answered %v", got)
	}
}

// TestFindLeafBySignatureKeyAnswersTheLowestMatchingLeaf pins the tie break, which neither the
// code nor any case in this file stated before: every leaf of the test tree carries a distinct
// signature key, so an implementation answering the LAST match rather than the first passed.
//
// A tie is reachable. This is how a member locates its own leaf in a tree a peer supplied, which
// happens before ValSem101's duplicate signature key refusal has necessarily run over that tree,
// and two members answering different positions for one key would each sign and decrypt as a
// different leaf.
func TestFindLeafBySignatureKeyAnswersTheLowestMatchingLeaf(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		duplicate []uint32
		blank     []uint32
		want      LeafIndex
	}{
		// the answer is the first of the run, not the last
		{name: "leaves 0 and 2 share a key", duplicate: []uint32{0, 2}, want: 0},
		{name: "leaves 0, 1 and 3 share a key", duplicate: []uint32{0, 1, 3}, want: 0},
		// and it is the first MATCH and not merely leaf zero: blanking the lowest holder moves
		// the answer, which an implementation hard wired to 0 fails
		{name: "the lowest holder is blank", duplicate: []uint32{0, 1, 3}, blank: []uint32{0}, want: 1},
		{name: "the two lowest holders are blank", duplicate: []uint32{0, 1, 3}, blank: []uint32{0, 1}, want: 3},
	} {
		tree, _ := treeUnderTest(t, 4)
		shared := SignaturePublicKey(repeatByte(0x7e, 32))
		for _, i := range testCase.duplicate {
			leaf := testTreeLeaf(i)
			leaf.SignatureKey = SignaturePublicKey(repeatByte(0x7e, 32))
			if err := tree.SetLeaf(LeafIndex(i), leaf); err != nil {
				t.Fatalf("%s: SetLeaf(%d): %v", testCase.name, i, err)
			}
		}
		for _, i := range testCase.blank {
			if err := tree.Blank(LeafIndex(i).NodeIndex()); err != nil {
				t.Fatalf("%s: Blank(%d): %v", testCase.name, i, err)
			}
		}
		got, ok := tree.FindLeafBySignatureKey(shared)
		if !ok || got != testCase.want {
			t.Errorf("%s: FindLeafBySignatureKey = (%d, %v), want (%d, true)", testCase.name, got, ok, testCase.want)
		}
	}
}

// ---------------------------------------------------------------------------
// the ParentNode encoding, octet by octet
// ---------------------------------------------------------------------------

// TestATruncatedOrExtendedParentNodeEncodingIsRefused is the counterpart of leaf_node_test.go's
// TestEveryOctetOfALeafNodeEncodingIsLoadBearing, which this structure had none of: the goldens
// were only ever decoded whole.
//
// Every proper prefix of every golden must be refused, and so must every golden with an octet
// added. What holds it today is syntax.Unmarshal's whole buffer requirement rather than anything
// in this file, and that is exactly why it is worth stating here: a ParentNode.UnmarshalMLS that
// stopped consuming its last field would still refuse the whole golden -- the leftover octets
// fail Done -- and would ACCEPT the prefix that ends where its reading stopped, which is a second
// decoder for a structure the parent hash and the tree hash are taken over.
func TestATruncatedOrExtendedParentNodeEncodingIsRefused(t *testing.T) {
	prefixes, extensions := 0, 0
	for _, golden := range parentNodeGoldenCases() {
		if len(golden.bytes) != golden.size {
			t.Fatalf("%s: the golden is %d octets and its stated size is %d", golden.name, len(golden.bytes), golden.size)
		}
		// the whole thing decodes, so a refusal below is the truncation and not the golden
		if err := syntax.Unmarshal(golden.bytes, &ParentNode{}); err != nil {
			t.Fatalf("%s: the complete golden was refused: %v", golden.name, err)
		}
		for cut := 0; cut < len(golden.bytes); cut += 1 {
			prefixes += 1
			if err := syntax.Unmarshal(golden.bytes[:cut], &ParentNode{}); err == nil {
				t.Errorf("%s: the first %d of %d octets decoded as a whole parent node", golden.name, cut, len(golden.bytes))
			}
		}
		for _, extra := range []byte{0x00, 0xff} {
			extensions += 1
			extended := append(bytes.Clone(golden.bytes), extra)
			if err := syntax.Unmarshal(extended, &ParentNode{}); err == nil {
				t.Errorf("%s: the golden with a trailing %#02x decoded as a whole parent node", golden.name, extra)
			}
		}
	}
	if prefixes == 0 || extensions == 0 {
		t.Fatalf("the sweep judged %d prefixes and %d extensions", prefixes, extensions)
	}
	t.Logf("%d prefixes and %d extended encodings refused", prefixes, extensions)
}

// TestOneParentNodesUnmergedLeavesStayAtTheDefaultLimitUnderARaisedOne is the mechanism behind
// the argument the codec table writes down beside this pair's entries.
//
// The syntax package inherits its vector limit downwards on purpose -- subReader hands the
// parent's limit to every nested read, WriteVector builds its scratch at the outer limit -- which
// is what lets a ratchet_tree running at MaxRatchetTreeLength carry fields larger than one
// ordinary structure may. So an entry arguing that unmerged_leaves runs at MaxVectorLength is an
// argument that becomes FALSE, with nothing failing, the moment a ratchet tree codec opens a
// raised writer: sixteen mebibytes is 4,194,304 unmerged leaves at one parent node.
//
// The bound is therefore applied by the codec itself and asserted here through a raised limit,
// which is the only place the difference is visible. Both halves, because an encoder that wrote
// what its decoder refuses is an implementation that cannot read what it sends.
func TestOneParentNodesUnmergedLeavesStayAtTheDefaultLimitUnderARaisedOne(t *testing.T) {
	// the bound is a function of the encoding rather than a number: an unmerged leaf is a
	// uint32, so a vector of exactly this many is exactly MaxVectorLength octets
	if maxUnmergedLeaves != syntax.MaxVectorLength/4 {
		t.Fatalf("maxUnmergedLeaves is %d and the default limit holds %d uint32", maxUnmergedLeaves, syntax.MaxVectorLength/4)
	}
	if syntax.MaxRatchetTreeLength <= syntax.MaxVectorLength {
		t.Fatalf("the ratchet tree limit is %d and the default is %d, so a raised limit proves nothing here",
			syntax.MaxRatchetTreeLength, syntax.MaxVectorLength)
	}
	ascending := func(n int) []LeafIndex {
		out := make([]LeafIndex, n)
		for i := range out {
			out[i] = LeafIndex(i)
		}
		return out
	}
	atTheBound := &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0xa1, 32)),
		ParentHash:     repeatByte(0xb2, 32),
		UnmergedLeaves: ascending(maxUnmergedLeaves),
	}
	pastTheBound := &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0xa1, 32)),
		ParentHash:     repeatByte(0xb2, 32),
		UnmergedLeaves: ascending(maxUnmergedLeaves + 1),
	}

	// the encode half. At the bound it encodes under the raised limit and under the default
	// one, so the refusal below is the bound and not the size of the buffer.
	if _, err := syntax.MarshalLimit(atTheBound, syntax.MaxRatchetTreeLength); err != nil {
		t.Fatalf("a vector at the bound was refused at the raised limit: %v", err)
	}
	if _, err := syntax.Marshal(atTheBound); err != nil {
		t.Fatalf("a vector at the bound was refused at the default limit: %v", err)
	}
	if _, err := syntax.MarshalLimit(pastTheBound, syntax.MaxRatchetTreeLength); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("encoding %d unmerged leaves at the raised limit answered %v, want ErrLengthExceedsMax: the ratchet tree's raised bound belongs to the ARRAY and not to one parent node's unmerged list",
			maxUnmergedLeaves+1, err)
	}
	if _, err := syntax.Marshal(pastTheBound); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("encoding %d unmerged leaves at the default limit answered %v, want ErrLengthExceedsMax", maxUnmergedLeaves+1, err)
	}

	// the decode half, over bytes built through the vector encoder directly so that a codec
	// which refuses to WRITE the over long vector can still be handed one
	overLong := func() []byte {
		writer := syntax.NewWriterLimit(syntax.MaxRatchetTreeLength)
		writer.WriteOpaque(pastTheBound.EncryptionKey)
		writer.WriteOpaque(pastTheBound.ParentHash)
		if err := syntax.WriteVector(writer, pastTheBound.UnmergedLeaves, writeOneUnmergedLeaf); err != nil {
			t.Fatalf("building the over long encoding: %v", err)
		}
		encoded, err := writer.Bytes()
		if err != nil {
			t.Fatalf("building the over long encoding: %v", err)
		}
		return encoded
	}()
	if err := syntax.UnmarshalLimit(overLong, &ParentNode{}, syntax.MaxRatchetTreeLength); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("decoding %d unmerged leaves at the raised limit answered %v, want ErrLengthExceedsMax", maxUnmergedLeaves+1, err)
	}
	// and the same decode at the bound is accepted, so the refusal above is the bound and not
	// a decoder that gave up on a large vector
	encodedAtTheBound, err := syntax.MarshalLimit(atTheBound, syntax.MaxRatchetTreeLength)
	if err != nil {
		t.Fatalf("encoding at the bound: %v", err)
	}
	decoded := &ParentNode{}
	if err := syntax.UnmarshalLimit(encodedAtTheBound, decoded, syntax.MaxRatchetTreeLength); err != nil {
		t.Fatalf("a vector at the bound was refused on the way in: %v", err)
	}
	if len(decoded.UnmergedLeaves) != maxUnmergedLeaves {
		t.Errorf("decoding a vector at the bound produced %d leaves, want %d", len(decoded.UnmergedLeaves), maxUnmergedLeaves)
	}
}

// ---------------------------------------------------------------------------
// guardrail 8 over the ratchet tree's own key comparisons
// ---------------------------------------------------------------------------

// The comparison guardrail 8 names, split into the two identifiers a call to it is written from
// so that the rules below can match it without rendering a node against a file set that is not
// its own.
const (
	sanctionedComparisonPackage = "subtle"
	sanctionedComparisonName    = "ConstantTimeCompare"
	ratchetTreeTypeName         = "RatchetTree"
)

func theSanctionedComparison() string {
	return sanctionedComparisonPackage + "." + sanctionedComparisonName
}

// keyShapedTypesOf is every named type these files declare whose underlying type is a slice of
// octets: SignaturePublicKey, HpkePublicKey and their private twins today.
//
// Read off the declarations rather than listed, so a key type a later task declares is under the
// rule the day it is declared rather than the day somebody remembers. A key is what a caller
// probes a tree with, and it is the argument whose comparison the guardrail is about.
func keyShapedTypesOf(files []parsedSource) map[string]bool {
	found := map[string]bool{}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			spec, isType := node.(*ast.TypeSpec)
			if !isType {
				return true
			}
			slice, isSlice := spec.Type.(*ast.ArrayType)
			if !isSlice || slice.Len != nil {
				return true
			}
			if element, isIdent := slice.Elt.(*ast.Ident); isIdent && (element.Name == "byte" || element.Name == "uint8") {
				found[spec.Name.Name] = true
			}
			return true
		})
	}
	return found
}

// keyHoldingTypesOf is every struct these files declare that carries a key of its OWN: a field
// whose type is one of the key shaped types above, or a pointer, slice, array or map spelled
// over one.
//
// This is the clause that was missing. The class below used to require a key PARAMETER, which
// is how a tree is asked about a key it was handed and is not how the private half of the tree
// compares one: TreeKEMPrivate.Consistent takes a provider and a tree, holds the keys it
// compares in its own fields, and sat outside a gate whose own comment said it was inside one.
// A predicate fitted to the two functions it was written for reads the third as clean.
//
// Directly declared and not transitively. A type holding a container that holds a key is not a
// key holder here, and *RatchetTree is exactly that -- its one field is a slice of nodes and the
// keys are two declarations further down. A transitive rule would put every bool or error
// answering method of the tree under the guardrail, most of them deciding an index, a width or a
// blank, and a gate carrying that many exemptions is a gate the next contributor deletes. It
// costs nothing here: the tree's own key comparisons are asked ABOUT a key and enter through the
// parameter clause, which is the clause that was already there.
func keyHoldingTypesOf(files []parsedSource, keyTypes map[string]bool) map[string]bool {
	found := map[string]bool{}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			spec, isType := node.(*ast.TypeSpec)
			if !isType {
				return true
			}
			structure, isStruct := spec.Type.(*ast.StructType)
			if !isStruct || structure.Fields == nil {
				return true
			}
			for _, field := range structure.Fields.List {
				if keyTypes[elementTypeNameOf(parsed.render(field.Type))] {
					found[spec.Name.Name] = true
				}
			}
			return true
		})
	}
	return found
}

// elementTypeNameOf is the name a spelled type carries values OF: the type itself, or the element
// of the pointer, slice, array or map written over it. A key one indirection down is held all the
// same, and the spelling is where the indirection is.
func elementTypeNameOf(rendered string) string {
	name := rendered
	if at := strings.LastIndex(name, "]"); at >= 0 {
		name = name[at+1:]
	}
	return strings.TrimPrefix(name, "*")
}

// keyQuestion is one member of the class the guardrail runs over, carrying the file it was read
// out of because every rule renders nodes back to source and a node rendered against the wrong
// file set reports the wrong text.
type keyQuestion struct {
	name     string
	host     parsedSource
	function *ast.FuncDecl
}

// qualifiedName is how a member is named in the expectations below: the receiver as it is
// written, then the method name, so a method and a plain function of the same name stay apart.
func (self parsedSource) qualifiedName(function *ast.FuncDecl) string {
	if receiver := self.receiverOf(function); receiver != "" {
		return receiver + "." + function.Name.Name
	}
	return function.Name.Name
}

// parameterTypesOf is one type per parameter, with a group like (a, b []byte) counted twice.
func parameterTypesOf(parsed parsedSource, function *ast.FuncDecl) []string {
	found := []string{}
	if function.Type.Params == nil {
		return found
	}
	for _, field := range function.Type.Params.List {
		rendered := parsed.render(field.Type)
		for range max(len(field.Names), 1) {
			found = append(found, rendered)
		}
	}
	return found
}

// keyQuestionsIn is every declaration in these files that has a key, answers rather than computes,
// and does it over a ratchet tree.
//
// Three conditions and each of them a shape rather than a name:
//
//   - a key of its own. A parameter whose type is one of the key types derived above, a receiver
//     that IS one, or a receiver whose struct declares a field of one. Three spellings of one
//     thing, and the third is the one this class was missing: a function does not have to be
//     HANDED a key to compare one, and the private half of the tree holds the keys it compares.
//   - a bool or an error somewhere in the results, which is what makes the answer a yes or no an
//     attacker can probe for rather than a value it computes. An error is the same answer spelled
//     as a refusal -- Consistent returns ErrPathSecretMismatch where FindLeafBySignatureKey
//     returns false -- and a class reading only bool judges whichever of the two happened to be
//     written first.
//   - a ratchet tree in the receiver or the parameters, which is the scope.
//
// The scope is where it is deliberately. The crypto provider answers questions about keys too and
// is guardrail 8's own subject next door, held by TestMacVerifyComparesInConstantTime and by
// TestEveryTagVerifierComparesThroughMacVerifyAndNothingElse; those verifiers reach ed25519.Verify
// and the HPKE primitives, which is what their gates sanction and what the rule below forbids.
// What had no gate at all was the container. Its comparisons are over PUBLIC keys, so nothing
// aimed at secrets reaches them, and all three survived being rewritten as ordinary go equality
// with the whole package green.
func keyQuestionsIn(files []parsedSource) []keyQuestion {
	keyTypes := keyShapedTypesOf(files)
	holders := keyHoldingTypesOf(files, keyTypes)
	found := []keyQuestion{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			receiver := elementTypeNameOf(parsed.receiverOf(function))
			hasAKey := keyTypes[receiver] || holders[receiver]
			mentionsTheTree := strings.Contains(receiver, ratchetTreeTypeName)
			for _, one := range parameterTypesOf(parsed, function) {
				if keyTypes[strings.TrimPrefix(one, "*")] {
					hasAKey = true
				}
				if strings.Contains(one, ratchetTreeTypeName) {
					mentionsTheTree = true
				}
			}
			if !hasAKey || !mentionsTheTree {
				continue
			}
			answers := false
			for _, field := range fieldsOf(function.Type.Results) {
				if rendered := parsed.render(field.Type); rendered == "bool" || rendered == "error" {
					answers = true
				}
			}
			if !answers {
				continue
			}
			found = append(found, keyQuestion{name: parsed.qualifiedName(function), host: parsed, function: function})
		}
	}
	slices.SortFunc(found, func(a keyQuestion, b keyQuestion) int { return strings.Compare(a.name, b.name) })
	return found
}

// namesOfKeyQuestions is the class as a sorted list of names, which is what the expectations
// below are stated in.
func namesOfKeyQuestions(class []keyQuestion) []string {
	names := []string{}
	for _, one := range class {
		names = append(names, one.name)
	}
	return names
}

// packageLevelConstantsOf is every name these files declare as a constant, which is what lets the
// equality rule tell a control flow decision from a decision about data.
func packageLevelConstantsOf(files []parsedSource) map[string]bool {
	found := map[string]bool{}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			declaration, isGeneric := node.(*ast.GenDecl)
			if !isGeneric || declaration.Tok != token.CONST {
				return true
			}
			for _, spec := range declaration.Specs {
				if value, isValue := spec.(*ast.ValueSpec); isValue {
					for _, name := range value.Names {
						found[name.Name] = true
					}
				}
			}
			return true
		})
	}
	return found
}

// isConstantExpression is whether one side of a comparison is a constant as far as the rule is
// concerned: a literal, nil, true, false, a composite literal, or a constant this package
// declares.
func isConstantExpression(expr ast.Expr, constants map[string]bool) bool {
	switch typed := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.CompositeLit:
		return true
	case *ast.ParenExpr:
		return isConstantExpression(typed.X, constants)
	case *ast.UnaryExpr:
		return isConstantExpression(typed.X, constants)
	case *ast.Ident:
		return typed.Name == "nil" || typed.Name == "true" || typed.Name == "false" || constants[typed.Name]
	}
	return false
}

// variableTimeEqualitiesIn is every == or != in one body with a value on both sides.
//
// This is the shape no class of function names ever sees: string(a) == string(b) mentions no
// comparator at all and is exactly the rewrite that survived here, and a [32]byte compared with
// == is a variable time comparison the language performs for free. A comparison with a constant
// on one side is a control flow decision -- err != nil, a node type against its constant, the
// == 1 that reads the answer out of the sanctioned comparison -- and is not reported.
//
// A length comparison IS reported, because len(a) != len(b) has a value on both sides. That is
// the direction this fails in on purpose: the sanctioned comparison refuses a length mismatch
// itself, so a key question that writes one has put a decision in front of the comparison the
// guardrail put there.
func variableTimeEqualitiesIn(parsed parsedSource, function *ast.FuncDecl, constants map[string]bool) []string {
	found := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		binary, isBinary := node.(*ast.BinaryExpr)
		if !isBinary || (binary.Op != token.EQL && binary.Op != token.NEQ) {
			return true
		}
		if isConstantExpression(binary.X, constants) || isConstantExpression(binary.Y, constants) {
			return true
		}
		found = append(found, parsed.render(binary))
		return true
	})
	slices.Sort(found)
	return slices.Compact(found)
}

// callsOutOfThisPackageIn is every call in one body that leaves this package for anything other
// than the sanctioned comparison.
//
// This is the comparator ban with its bounds taken off, and it is here because a class derived
// from signatures still has a shape and anything outside that shape is outside it: bytes.Cut
// answers "does this key begin with that one" through three results, and a helper in a package
// nobody has classified answers it however it likes. So for a function whose answer is about a
// key, the rule is not that no comparator is called -- it is that nothing outside this package
// is called at all, with one exception, and the exception is the comparison the guardrail names.
//
// A method on a value is not a call out of the package: the receiver had to come from somewhere,
// and everything that could produce one here is either a call this rule already sees or a value
// of this package's own. A conversion through a predeclared or package level type name is not a
// call at all.
func callsOutOfThisPackageIn(parsed parsedSource, function *ast.FuncDecl, declared declaredNames) []string {
	found := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if declared.functions[callee.Name] || declared.types[callee.Name] || types.Universe.Lookup(callee.Name) != nil {
				return true
			}
			found = append(found, callee.Name)
		case *ast.SelectorExpr:
			rendered := parsed.render(call.Fun)
			if rendered == theSanctionedComparison() {
				return true
			}
			qualifier, isIdent := callee.X.(*ast.Ident)
			if !isIdent || !declared.imports[qualifier.Name] {
				return true
			}
			found = append(found, rendered)
		}
		return true
	})
	slices.Sort(found)
	return slices.Compact(found)
}

// functionsByNameIn is every declaration these files carry, keyed by the name a call to it is
// written with -- the function name, or the method name for a method, since a call site spells
// self.Leaf and not (*RatchetTree).Leaf.
func functionsByNameIn(files []parsedSource) map[string][]*ast.FuncDecl {
	found := map[string][]*ast.FuncDecl{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			if function, isFunction := declaration.(*ast.FuncDecl); isFunction && function.Body != nil {
				found[function.Name.Name] = append(found[function.Name.Name], function)
			}
		}
	}
	return found
}

// reachesTheConstantTimeComparison walks this package's own call graph from one declaration and
// reports whether the sanctioned comparison is anywhere in it.
//
// The transitive half is what says the requirement is about the ANSWER and not about the text of
// one function: a key question that moved its comparison into a helper still satisfies this, and
// one that moved it into a helper written as a byte loop does not. Banning the wrong comparison
// is not the same as requiring the right one -- a function that compared nothing at all passes
// every ban above and fails here.
func reachesTheConstantTimeComparison(function *ast.FuncDecl, byName map[string][]*ast.FuncDecl) bool {
	pending := []*ast.FuncDecl{function}
	seen := map[*ast.FuncDecl]bool{function: true}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		sanctioned, called := false, []string{}
		ast.Inspect(current.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				called = append(called, callee.Name)
			case *ast.SelectorExpr:
				called = append(called, callee.Sel.Name)
				qualifier, isIdent := callee.X.(*ast.Ident)
				if isIdent && qualifier.Name == sanctionedComparisonPackage && callee.Sel.Name == sanctionedComparisonName {
					sanctioned = true
				}
			}
			return true
		})
		if sanctioned {
			return true
		}
		for _, name := range called {
			for _, next := range byName[name] {
				if !seen[next] {
					seen[next] = true
					pending = append(pending, next)
				}
			}
		}
	}
	return false
}

// parsedProductionSourcesOfThisPackage is every non test file of this package, parsed. The rule
// reads the directory rather than a file name, so a key question a later task puts in
// tree_sync.go is under the guardrail without an edit here.
func parsedProductionSourcesOfThisPackage(t *testing.T) []parsedSource {
	t.Helper()
	files := []parsedSource{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		files = append(files, mustParseSource(t, path))
	}
	if len(files) == 0 {
		t.Fatal("no production file was parsed, so the guardrail below judged nothing")
	}
	return files
}

// A fixture declaring one of every shape the three rules have to tell apart, so a matcher that
// stopped matching fails here rather than issuing the real source the clean bill a working one
// issues.
//
// Every member is here because some half of some rule has to be the only thing reporting it:
//
//   - ComparesWithBytesEqual and ComparesWithAPrefix are the comparators a ban list would have
//     had to think of, and only the foreign call rule reports them. bytes.HasPrefix leaks
//     strictly more than bytes.Equal does and was outside the six name list this project shipped.
//   - ComparesByConvertingToString names no comparator at all and is the rewrite that actually
//     survived in this file, so only the equality rule reports it.
//   - ComparesAfterALengthFastPath keeps the sanctioned comparison and puts a decision in front
//     of it, so only the equality rule reports it, and it is what says that rule is not merely
//     the foreign call rule spelled differently.
//   - ComparesNothingAtAll answers without comparing, so only the reachability rule reports it.
//   - ComparesThroughAHelperThatLoops and loops are the comparison moved out of the function
//     that answers: the helper carries the byte loop, so the equality rule reports the HELPER and
//     the reachability rule reports the caller, and neither reports the other.
//   - FindsInATreeItWasHanded is a plain function rather than a method, which is what says the
//     class is not "the methods of one type".
//   - FindsInConstantTime, FindsThroughAHelperOfItsOwn and holds are the clean half: one
//     comparing directly and one through a helper, which is what makes the reachability
//     requirement really transitive.
//   - the three AgreesWithTheTree methods are the HOLDER route, which is TreeKEMPrivate.Consistent
//     in the real source and which no version of this fixture had: PrivateHalf is handed no key
//     and answers no bool, it holds a key in a field and refuses with an error, and each of the
//     three commits one of the three shapes -- the sanctioned comparison, a named comparator, and
//     the conversion to string that names none. RatchetTree's own field is [][]byte and not a key
//     type on purpose, so the tree stays outside the holder clause exactly as the real one is and
//     its members go on entering through the parameter clause.
//   - TakesAKeyAndAnswersNoQuestion, AnswersAQuestionAboutNoKey, RewritesAgainstTheTree,
//     AgreesWithNoTreeAtAll and ComparesKeysWithNoTreeInSight are outside the class on one
//     condition each -- no question, no key, no question, no tree, no tree -- and every one of
//     them would be reported if it were inside, so a class that widened fails as loudly as one
//     that narrowed.
const ratchetTreeKeyComparisonControl = `package control

import (
	"bytes"
	"crypto/subtle"
	"errors"
)

var errControlMismatch = errors.New("control: mismatch")

type SignaturePublicKey []byte

type SignaturePrivateKey []byte

type RatchetTree struct {
	keys [][]byte
	at   int
}

type PrivateHalf struct {
	leafKey SignaturePrivateKey
	rungs   map[int][]byte
}

func (self *RatchetTree) FindsInConstantTime(key SignaturePublicKey) (int, bool) {
	for i, held := range self.keys {
		if subtle.ConstantTimeCompare(held, key) == 1 {
			return i, true
		}
	}
	return 0, false
}

func (self *RatchetTree) FindsThroughAHelperOfItsOwn(key SignaturePublicKey) bool {
	return self.holds(key)
}

func (self *RatchetTree) holds(key SignaturePublicKey) bool {
	return subtle.ConstantTimeCompare(self.keys[0], key) == 1
}

func (self *RatchetTree) ComparesWithBytesEqual(key SignaturePublicKey) bool {
	return bytes.Equal(self.keys[0], key)
}

func (self *RatchetTree) ComparesWithAPrefix(key SignaturePublicKey) bool {
	return bytes.HasPrefix(self.keys[0], key)
}

func (self *RatchetTree) ComparesByConvertingToString(key SignaturePublicKey) bool {
	return string(self.keys[0]) == string(key)
}

func (self *RatchetTree) ComparesAfterALengthFastPath(key SignaturePublicKey) bool {
	if len(key) != len(self.keys[0]) {
		return false
	}
	return subtle.ConstantTimeCompare(self.keys[0], key) == 1
}

func (self *RatchetTree) ComparesNothingAtAll(key SignaturePublicKey) bool {
	_ = key
	return true
}

func (self *RatchetTree) ComparesThroughAHelperThatLoops(key SignaturePublicKey) bool {
	return self.loops(key)
}

func (self *RatchetTree) loops(key SignaturePublicKey) bool {
	held := self.keys[0]
	for i := range held {
		if held[i] != key[i] {
			return false
		}
	}
	return true
}

func FindsInATreeItWasHanded(tree *RatchetTree, key SignaturePublicKey) bool {
	return bytes.Equal(tree.keys[0], key)
}

func (self *RatchetTree) BuildsAPathAndComparesNoKey(key SignaturePublicKey) error {
	_ = key
	for i := range self.keys {
		if i == self.at {
			return errControlMismatch
		}
	}
	return nil
}

func (self *RatchetTree) BuildsAPathThroughAForeignHelper(key SignaturePublicKey) error {
	_ = key
	self.keys[0] = bytes.Clone(self.keys[0])
	return nil
}

func (self *PrivateHalf) AgreesWithTheTreeInConstantTime(tree *RatchetTree) error {
	if subtle.ConstantTimeCompare(self.leafKey, tree.keys[0]) != 1 {
		return errControlMismatch
	}
	return nil
}

func (self *PrivateHalf) AgreesWithTheTreeWithBytesEqual(tree *RatchetTree) error {
	if !bytes.Equal(self.leafKey, tree.keys[0]) {
		return errControlMismatch
	}
	return nil
}

func (self *PrivateHalf) AgreesWithTheTreeByConvertingToString(tree *RatchetTree) error {
	if string(self.leafKey) != string(tree.keys[0]) {
		return errControlMismatch
	}
	return nil
}

func (self *PrivateHalf) RewritesAgainstTheTree(tree *RatchetTree) []byte {
	return bytes.Clone(tree.keys[0])
}

func (self *PrivateHalf) AgreesWithNoTreeAtAll() error {
	if !bytes.Equal(self.leafKey, self.rungs[0]) {
		return errControlMismatch
	}
	return nil
}

func (self *RatchetTree) TakesAKeyAndAnswersNoQuestion(key SignaturePublicKey) []byte {
	return bytes.Clone(key)
}

func (self *RatchetTree) AnswersAQuestionAboutNoKey(a []byte, b []byte) bool {
	return bytes.Equal(a, b)
}

func ComparesKeysWithNoTreeInSight(a SignaturePublicKey, b SignaturePublicKey) bool {
	return bytes.Equal(a, b)
}
`

// What the class must read out of the fixture, exactly rather than as a floor. A class that
// widened to take in the three members outside it, or narrowed to drop one of the bad shapes,
// would go on to read the real source the same wrong way and report the same clean bill.
var ratchetTreeKeyComparisonControlClass = []string{
	"*PrivateHalf.AgreesWithTheTreeByConvertingToString",
	"*PrivateHalf.AgreesWithTheTreeInConstantTime",
	"*PrivateHalf.AgreesWithTheTreeWithBytesEqual",
	"*RatchetTree.BuildsAPathAndComparesNoKey",
	"*RatchetTree.BuildsAPathThroughAForeignHelper",
	"*RatchetTree.ComparesAfterALengthFastPath",
	"*RatchetTree.ComparesByConvertingToString",
	"*RatchetTree.ComparesNothingAtAll",
	"*RatchetTree.ComparesThroughAHelperThatLoops",
	"*RatchetTree.ComparesWithAPrefix",
	"*RatchetTree.ComparesWithBytesEqual",
	"*RatchetTree.FindsInConstantTime",
	"*RatchetTree.FindsThroughAHelperOfItsOwn",
	"*RatchetTree.holds",
	"*RatchetTree.loops",
	"FindsInATreeItWasHanded",
}

// The five the class must be seen to LEAVE OUT, named so a reader can tell a deliberate boundary
// from an oversight. ComparesKeysWithNoTreeInSight and AgreesWithNoTreeAtAll are left out by the
// scope rather than by the shape: both compare keys in variable time and both are somebody else's
// guardrail -- the package wide ban in constant_time_test.go reports them, which is the gate that
// has no scope at all.
var ratchetTreeKeyComparisonControlOutsideTheClass = []string{
	"*PrivateHalf.AgreesWithNoTreeAtAll",
	"*PrivateHalf.RewritesAgainstTheTree",
	"*RatchetTree.AnswersAQuestionAboutNoKey",
	"*RatchetTree.TakesAKeyAndAnswersNoQuestion",
	"ComparesKeysWithNoTreeInSight",
}

var (
	// the equality rule: a value on both sides of an == or a !=
	ratchetTreeControlVariableTimeEqualities = []string{
		"*PrivateHalf.AgreesWithTheTreeByConvertingToString",
		"*RatchetTree.BuildsAPathAndComparesNoKey",
		"*RatchetTree.ComparesAfterALengthFastPath",
		"*RatchetTree.ComparesByConvertingToString",
		"*RatchetTree.loops",
	}
	// the foreign call rule: anything outside the package that is not the sanctioned comparison
	ratchetTreeControlForeignCalls = []string{
		"*PrivateHalf.AgreesWithTheTreeWithBytesEqual",
		"*RatchetTree.BuildsAPathThroughAForeignHelper",
		"*RatchetTree.ComparesWithAPrefix",
		"*RatchetTree.ComparesWithBytesEqual",
		"FindsInATreeItWasHanded",
	}
	// the reachability rule: the sanctioned comparison is nowhere in the call graph
	ratchetTreeControlUnreached = []string{
		"*PrivateHalf.AgreesWithTheTreeByConvertingToString",
		"*PrivateHalf.AgreesWithTheTreeWithBytesEqual",
		"*RatchetTree.BuildsAPathAndComparesNoKey",
		"*RatchetTree.BuildsAPathThroughAForeignHelper",
		"*RatchetTree.ComparesByConvertingToString",
		"*RatchetTree.ComparesNothingAtAll",
		"*RatchetTree.ComparesThroughAHelperThatLoops",
		"*RatchetTree.ComparesWithAPrefix",
		"*RatchetTree.ComparesWithBytesEqual",
		"*RatchetTree.loops",
		"FindsInATreeItWasHanded",
	}
)

// TestTheKeyComparisonGateFlagsItsControlFixture is the matcher's own control, and it runs before
// the gate over the real source so that a rule which stopped matching fails here rather than
// issuing this package a clean bill.
func TestTheKeyComparisonGateFlagsItsControlFixture(t *testing.T) {
	control := mustParseText(t, "the ratchet tree key comparison control", ratchetTreeKeyComparisonControl)
	files := []parsedSource{control}
	class := keyQuestionsIn(files)
	if got := namesOfKeyQuestions(class); !slices.Equal(got, ratchetTreeKeyComparisonControlClass) {
		t.Fatalf("the class read %v out of the control, want %v", got, ratchetTreeKeyComparisonControlClass)
	}
	// the members outside it are outside it, said by name rather than left implied
	for _, outside := range ratchetTreeKeyComparisonControlOutsideTheClass {
		if slices.Contains(ratchetTreeKeyComparisonControlClass, outside) {
			t.Errorf("%s is named as being outside the class and is inside it", outside)
		}
	}
	declared := namesTheseFilesDeclare(files)
	constants := packageLevelConstantsOf(files)
	byName := functionsByNameIn(files)
	equalities, foreign, unreached := []string{}, []string{}, []string{}
	for _, one := range class {
		if len(variableTimeEqualitiesIn(one.host, one.function, constants)) != 0 {
			equalities = append(equalities, one.name)
		}
		if len(callsOutOfThisPackageIn(one.host, one.function, declared)) != 0 {
			foreign = append(foreign, one.name)
		}
		if !reachesTheConstantTimeComparison(one.function, byName) {
			unreached = append(unreached, one.name)
		}
	}
	if !slices.Equal(equalities, ratchetTreeControlVariableTimeEqualities) {
		t.Errorf("the equality rule reported %v out of the control, want %v", equalities, ratchetTreeControlVariableTimeEqualities)
	}
	if !slices.Equal(foreign, ratchetTreeControlForeignCalls) {
		t.Errorf("the foreign call rule reported %v out of the control, want %v", foreign, ratchetTreeControlForeignCalls)
	}
	if !slices.Equal(unreached, ratchetTreeControlUnreached) {
		t.Errorf("the reachability rule reported %v out of the control, want %v", unreached, ratchetTreeControlUnreached)
	}
}

// keyQuestionsThatCompareNoKey names a declaration the class's SHAPE takes in and whose body has
// no key comparison for the reachability rule to hold, with the reason it has none.
//
// The class is three shapes and not a list -- a key of its own, an answer spelled as a bool or an
// error, and a ratchet tree in scope -- and that is what makes it worth having: it found
// TreeKEMPrivate.Consistent, which a hand written list had missed while a comment claimed it was
// covered. A shape wide enough to find that one also takes in a declaration that is HANDED a key
// and answers an error without ever comparing anything, and task 18's path generation is the
// first of those: it is given the committer's signature private key so it can sign a leaf, and
// every key it touches it DERIVES.
//
// The entry is a narrower rule rather than a pass, which is the whole of why it is safe. The
// equality rule and the foreign call rule still run over an excused member, so a comparison
// written into it in either of those spellings fails exactly as it would anywhere else in the
// class; and an excused member that DOES reach the sanctioned comparison fails too, because that
// is an excuse which has outlived what it named. Every name is held against the derived class, so
// an entry cannot survive the declaration it excuses.
//
// That paragraph used to be a claim and is now a test. Nothing verified that the two surviving
// rules survive -- the control fixture had no excused member at all -- so the sentence that says
// the excuse is safe was exactly the shape this file's own header keeps citing: a comment
// claiming a coverage nothing provided. TestTheExcuseForAKeyQuestionThatComparesNoKeyLiftsOnly-
// Reachability excuses every member of the control and requires the equality and foreign call
// rules to go on reporting the same members they report unexcused.
//
// What that costs the members of this class is worth writing down, because it is a real
// constraint on the tasks that will join it and not a formality. The equality rule reads a body
// for == and != with a value on both sides and cannot tell a NodeIndex from a key -- the gate is
// over source and there is no type information in it -- so a path builder that needs to compare
// two indices, two lengths or two positions cannot write the comparison inline. It puts the
// decision behind a function that is not itself a member of this class, which any helper over
// indices alone is: the class needs a key AND a tree AND a bool or an error, and a comparison of
// two node indices has none of them. That route is not an escape from the guardrail, because a
// helper that compared KEYS that way would leave its caller reaching no sanctioned comparison,
// which is the reachability rule, and would be reported by the package wide comparator ban next
// door in either case. The control fixture holds both halves: BuildsAPathAndComparesNoKey writes
// exactly that inline index comparison and is reported, and it is reported while excused.
var keyQuestionsThatCompareNoKey = map[string]string{
	"*RatchetTree.CreateUpdatePathSecrets": "is handed the committer's signature private key so it can sign a leaf and answers an error, which is the class's shape; it derives a fresh key pair for every node of the path and compares none of them",
}

// TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime is guardrail 8 over the key
// comparisons made against the ratchet tree, which no gate in this package reached.
//
// The tree's two were rewritten as ordinary go equality -- string(leaf.SignatureKey) == string(key)
// at one and both comparisons at the other -- and the whole package stayed green. The comparison
// is constant time for the reason FindLeafBySignatureKey's own comment gives, and what says so
// today is that comment; no behavioural test in go can see the difference, and a timing
// measurement over a 32 octet comparison on this machine is noise. So what is asserted is what
// was actually verified: mechanically, over the source, so it survives an edit nobody reruns this
// for.
//
// The name says OVER the tree rather than what the tree answers, because the class is no longer
// the tree's own methods. TreeKEMPrivate.Consistent compares a derived public key against the one
// the tree carries and is not a method of the tree at all; the predicate that missed it required
// a key parameter and a bool, and it missed it while treekem.go's own comment said this gate held
// it. Measured: subtle.ConstantTimeCompare there rewritten as bytes.Equal left every one of this
// package's tests passing.
//
// The blind spot is worth naming, because it is the same one the tag verifier gate next door
// documents: the equality rule reads each member's own body, so a byte loop written inside a
// helper is caught by the reachability rule -- the helper is not the sanctioned comparison -- and
// not by the equality rule, unless the helper is itself a member of the class.
func TestEveryKeyQuestionOverTheRatchetTreeIsAnsweredInConstantTime(t *testing.T) {
	files := parsedProductionSourcesOfThisPackage(t)
	class := keyQuestionsIn(files)
	names := namesOfKeyQuestions(class)
	t.Logf("%d key questions under the gate: %v", len(names), names)
	if len(class) == 0 {
		t.Fatal("the gate found no key question at all, so it is reporting clean having read nothing")
	}
	// the coverage claim, checked rather than assumed: the three comparisons this gate is about
	// have to be among the ones being judged. A derived class can go quiet, and a quiet class
	// reports the clean bill a working one reports -- which is exactly what happened to
	// Consistent, so it is named here beside the two that were.
	for _, want := range []string{"*RatchetTree.FindLeafBySignatureKey", "*RatchetTree.EncryptionKeyInUse", "*TreeKEMPrivate.Consistent"} {
		if !slices.Contains(names, want) {
			t.Fatalf("the gate is judging %v, which does not include %s", names, want)
		}
	}
	declared := namesTheseFilesDeclare(files)
	constants := packageLevelConstantsOf(files)
	byName := functionsByNameIn(files)
	for _, one := range class {
		verdict := judgeKeyQuestion(one, declared, constants, byName, keyQuestionsThatCompareNoKey)
		for _, equality := range verdict.equalities {
			t.Errorf("%s decides %s, which is a comparison of two values in variable time; every comparison of a key here goes through %s. An entry in keyQuestionsThatCompareNoKey does not lift this rule and is not meant to: a member of this class with an index, a length or a position to compare puts that decision behind a function that is not itself a key question, which any helper over indices alone is",
				one.name, equality, theSanctionedComparison())
		}
		for _, foreign := range verdict.foreign {
			t.Errorf("%s calls %s; a key question's answer is decided by %s and by nothing else, and what it needs from elsewhere belongs behind a function of this package",
				one.name, foreign, theSanctionedComparison())
		}
		if verdict.excused {
			if verdict.reaches {
				t.Errorf("%s is excused from the reachability rule as a member that compares no key, and it reaches %s; the excuse has outlived what it named",
					one.name, theSanctionedComparison())
			}
			continue
		}
		if !verdict.reaches {
			t.Errorf("%s reaches no %s; a function that answers a question about a key without comparing it in constant time is not answering it safely",
				one.name, theSanctionedComparison())
		}
	}
	// and the excuse is held to the derived class in the other direction, so an entry cannot
	// outlive the declaration it names
	for name, reason := range keyQuestionsThatCompareNoKey {
		if !slices.Contains(names, name) {
			t.Errorf("the gate excuses %s as %q, and no key question of this package is called that",
				name, reason)
		}
	}
}

// keyQuestionVerdict is everything the three rules decide about one member of the class, in one
// value, so the gate over the real source and the control below read the rules through one body
// rather than through two copies of it.
//
// A copy is what this was, and the copy is why the excuse's own claim went unchecked: the gate
// applied the excuse and the control test ran the three rules with no excuse in sight, so the
// sentence saying an excuse lifts only reachability was asserted by neither.
type keyQuestionVerdict struct {
	equalities []string
	foreign    []string
	reaches    bool
	excused    bool
}

// judgeKeyQuestion runs the three rules over one member, with the excuse table as an argument
// rather than as a global, which is what lets the control fixture be judged under an excuse.
func judgeKeyQuestion(one keyQuestion, declared declaredNames, constants map[string]bool,
	byName map[string][]*ast.FuncDecl, excuses map[string]string) keyQuestionVerdict {
	_, excused := excuses[one.name]
	return keyQuestionVerdict{
		equalities: variableTimeEqualitiesIn(one.host, one.function, constants),
		foreign:    callsOutOfThisPackageIn(one.host, one.function, declared),
		reaches:    reachesTheConstantTimeComparison(one.function, byName),
		excused:    excused,
	}
}

// TestTheExcuseForAKeyQuestionThatComparesNoKeyLiftsOnlyReachability is the claim
// keyQuestionsThatCompareNoKey makes about itself, made into an assertion.
//
// The excuse exists because the class is a SHAPE, and a shape wide enough to find
// TreeKEMPrivate.Consistent also takes in a declaration that is handed a key so it can sign
// with it and derives every other key it touches -- task 18's path generation was the first,
// and tasks 20 to 22 add more path builders to the same class. What makes an entry safe is that
// it lifts the reachability rule alone: a comparison written into an excused member still fails
// the equality rule and a call out of the package still fails the foreign call rule. Nothing
// checked that. The control fixture had no excused member, so both rules were only ever run
// unexcused, and an excuse that had grown into a pass would have read exactly the same.
//
// So the control is judged with EVERY one of its members excused, which is the strongest form
// of the question, and the two surviving rules are required to report exactly the members they
// report with no excuse at all. The two shapes a path builder actually writes are named on top
// of that: an inline comparison of two indices, and a call to a helper outside the package.
// Both are things a member of this class may not write, excused or not, and both are in the
// fixture so that the constraint tasks 20 to 22 inherit is a test rather than a paragraph.
func TestTheExcuseForAKeyQuestionThatComparesNoKeyLiftsOnlyReachability(t *testing.T) {
	control := mustParseText(t, "the ratchet tree key comparison control", ratchetTreeKeyComparisonControl)
	files := []parsedSource{control}
	class := keyQuestionsIn(files)
	if len(class) == 0 {
		t.Fatal("the class read nothing out of the control, so this test judges an empty set")
	}
	declared := namesTheseFilesDeclare(files)
	constants := packageLevelConstantsOf(files)
	byName := functionsByNameIn(files)
	// every member excused, which is the excuse at its widest
	excuses := map[string]string{}
	for _, one := range class {
		excuses[one.name] = "the control excuses every member, so what the two surviving rules report here is what they report under an excuse"
	}
	equalities, foreign, lifted := []string{}, []string{}, []string{}
	for _, one := range class {
		verdict := judgeKeyQuestion(one, declared, constants, byName, excuses)
		if !verdict.excused {
			t.Fatalf("%s was excused and the verdict says it was not, so this test is not reading the excuse at all", one.name)
		}
		if len(verdict.equalities) != 0 {
			equalities = append(equalities, one.name)
		}
		if len(verdict.foreign) != 0 {
			foreign = append(foreign, one.name)
		}
		if !verdict.reaches {
			lifted = append(lifted, one.name)
		}
	}
	// the two rules an excuse does not touch report exactly what they report unexcused
	if !slices.Equal(equalities, ratchetTreeControlVariableTimeEqualities) {
		t.Errorf("under an excuse the equality rule reported %v, and unexcused it reports %v; the excuse is lifting a rule it is documented not to lift",
			equalities, ratchetTreeControlVariableTimeEqualities)
	}
	if !slices.Equal(foreign, ratchetTreeControlForeignCalls) {
		t.Errorf("under an excuse the foreign call rule reported %v, and unexcused it reports %v; the excuse is lifting a rule it is documented not to lift",
			foreign, ratchetTreeControlForeignCalls)
	}
	// and the rule it does lift is the one that had something to lift
	if !slices.Equal(lifted, ratchetTreeControlUnreached) {
		t.Errorf("the reachability rule found %v not reaching %s, and the control expects %v",
			lifted, theSanctionedComparison(), ratchetTreeControlUnreached)
	}
	if len(lifted) == 0 {
		t.Error("no member of the control fails the reachability rule, so this test cannot see that an excuse lifts it")
	}
	// the two shapes a path builder writes, named rather than left to the lists above. A member
	// of this class may write neither, whatever it is excused for.
	for _, want := range []string{"*RatchetTree.BuildsAPathAndComparesNoKey"} {
		if !slices.Contains(equalities, want) {
			t.Errorf("%s compares two indices inline and the equality rule did not report it under an excuse; a path builder joining this class would be told that is allowed",
				want)
		}
	}
	for _, want := range []string{"*RatchetTree.BuildsAPathThroughAForeignHelper"} {
		if !slices.Contains(foreign, want) {
			t.Errorf("%s calls out of the package and the foreign call rule did not report it under an excuse; a path builder joining this class would be told that is allowed",
				want)
		}
	}
	t.Logf("%d members judged under an excuse: %d still reported by the equality rule, %d by the foreign call rule, %d lifted from the reachability rule",
		len(class), len(equalities), len(foreign), len(lifted))
}

// ---------------------------------------------------------------------------
// node resolution -- RFC 9420 section 7.5
// ---------------------------------------------------------------------------
//
// Resolution decides WHO a path secret is encrypted to, so the two directions it can be wrong in
// are not symmetric. Too small and a member who should have been sent the secret is not, which
// is loud -- that member cannot decrypt the next commit and says so. Too large and a member who
// should NOT have been sent it is, which is silent, and the member reading it is one the group
// removed. The clause that produces the silent direction is the unmerged-leaf clause, and the
// clause that produces the quiet-until-interop direction is the ORDER: TreeKEM pairs the entries
// of a resolution positionally with the ciphertexts of an UpdatePath, so a permuted resolution
// seals every secret to the wrong member while having exactly the right members in it.
//
// So nothing below compares two resolutions as sets, and nothing sorts one before comparing.
// equalNodeIndices is elementwise and length-first, and the sweeps compare against a recursion
// written from the RFC's own words rather than against a second reading of the code under test.

// equalNodeIndices is elementwise and never a set comparison, for the reason above: the order of
// a resolution is the contract and not a detail of it, and reflect.DeepEqual over a sorted copy
// -- or a subset test -- passes every permutation there is.
func equalNodeIndices(a, b []NodeIndex) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rfcResolution is RFC 9420 section 7.5 written as the recursion the RFC states, as the
// independent second reading every sweep below compares against.
//
// A different SHAPE and not a paraphrase. The implementation under test walks an explicit stack
// so a deep tree cannot become deep Go stack, and the subtle part of that version is the push
// order -- right child pushed first so the left is popped first -- which a recursion states
// directly and cannot get wrong the same way. A reference transcribed from the implementation
// would agree with it about a reversed descent, a dropped unmerged list and an inverted blank
// test alike, which is the whole of what these sweeps are for.
//
// The unreachable arms panic rather than answering an empty list. A node that is not a leaf has
// both children at every representable index, so a refusal from Left or Right here is this
// helper being asked something it was never given -- and an empty answer would be a resolution
// that silently lost a whole subtree, which is the exact defect the sweep is looking for.
func rfcResolution(shape NodeShape, x NodeIndex) []NodeIndex {
	if !shape.IsBlank(x) {
		out := []NodeIndex{x}
		for _, leaf := range shape.UnmergedLeaves(x) {
			out = append(out, leaf.NodeIndex())
		}
		return out
	}
	if x.IsLeaf() {
		return []NodeIndex{}
	}
	left, err := Left(x)
	if err != nil {
		panic("rfcResolution: a parent node with no left child: " + err.Error())
	}
	right, err := Right(x)
	if err != nil {
		panic("rfcResolution: a parent node with no right child: " + err.Error())
	}
	return append(rfcResolution(shape, left), rfcResolution(shape, right)...)
}

func TestResolutionRules(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}

	// all parents blank: the root resolves to the four leaves, left to right.
	got := tree.Resolution(root)
	want := []NodeIndex{0, 2, 4, 6}
	if !equalNodeIndices(got, want) {
		t.Fatalf("blank-parent root resolution = %v, want %v", got, want)
	}

	// a blank leaf contributes nothing.
	if err := tree.Blank(NodeIndex(2)); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	got = tree.Resolution(root)
	want = []NodeIndex{0, 4, 6}
	if !equalNodeIndices(got, want) {
		t.Fatalf("with leaf 1 blank, root resolution = %v, want %v", got, want)
	}
	if len(tree.Resolution(NodeIndex(2))) != 0 {
		t.Fatalf("a blank leaf must resolve to the empty list")
	}

	// a non-blank parent resolves to itself, then its unmerged leaves in order.
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x77}, 32)),
		UnmergedLeaves: []LeafIndex{0},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	got = tree.Resolution(NodeIndex(1))
	want = []NodeIndex{1, 0}
	if !equalNodeIndices(got, want) {
		t.Fatalf("non-blank parent resolution = %v, want %v", got, want)
	}
	got = tree.Resolution(root)
	want = []NodeIndex{1, 0, 4, 6}
	if !equalNodeIndices(got, want) {
		t.Fatalf("root resolution = %v, want %v", got, want)
	}

	// the unmerged half again with a list of more than one, and with a stored order that is not
	// the ascending one. RFC 9420 section 7.9.2 requires the vector ascending and both halves of
	// the parent node codec refuse anything else, but the resolution walk reads STORED order and
	// must not be the place that quietly repairs it: a walk that sorted here would answer a
	// resolution no peer computes, over a tree every peer would have rejected outright.
	if err := tree.SetParent(NodeIndex(5), &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x88}, 32)),
		UnmergedLeaves: []LeafIndex{3, 2},
	}); err != nil {
		t.Fatalf("SetParent(5): %v", err)
	}
	got = tree.Resolution(NodeIndex(5))
	want = []NodeIndex{5, 6, 4}
	if !equalNodeIndices(got, want) {
		t.Fatalf("a parent carrying two unmerged leaves resolved to %v, want %v", got, want)
	}

	// the method and the free function the tree math plan owns agree, and the free
	// one is where an out-of-range node index is an error rather than an empty list.
	got = tree.Resolution(root)
	free, err := Resolution(tree, root)
	if err != nil {
		t.Fatalf("Resolution(tree, root): %v", err)
	}
	if !equalNodeIndices(free, got) {
		t.Fatalf("the method and the free function disagree: %v vs %v", got, free)
	}
	if _, err := Resolution(tree, NodeIndex(tree.NodeWidth())); err == nil {
		t.Fatalf("Resolution past the node width returned no error")
	}
}

// TestTheResolutionMethodDropsTheErrorOnlyWhereTheFreeFunctionRefuses pins the one decision this
// task's method makes.
//
// The method answers the empty list for an out-of-range index, and an accepted empty resolution
// is also the empty list, so the two are not distinguishable through it -- that is the whole of
// what dropping the error costs, and it is only sound if the method and the free function agree
// everywhere the free function accepts. A method that had quietly grown a second opinion about
// any in-range node would be a second resolution algorithm, which is the thing this section's
// header says must not exist.
func TestTheResolutionMethodDropsTheErrorOnlyWhereTheFreeFunctionRefuses(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 5)
	if err := tree.SetParent(NodeIndex(3), &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x91, 32)),
		UnmergedLeaves: []LeafIndex{1, 3},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	refused := 0
	// past the width as well as inside it, so both arms of the method are taken
	for x := uint32(0); x < tree.NodeWidth()+4; x += 1 {
		free, err := Resolution(tree, NodeIndex(x))
		method := tree.Resolution(NodeIndex(x))
		if err != nil {
			refused += 1
			if method == nil {
				t.Errorf("the method answered nil rather than the empty list for the refused index %d", x)
			}
			if len(method) != 0 {
				t.Errorf("the free function refused index %d and the method answered %v", x, method)
			}
			continue
		}
		if !equalNodeIndices(method, free) {
			t.Errorf("at node %d the method answered %v and the free function %v", x, method, free)
		}
	}
	if refused != 4 {
		t.Fatalf("the free function refused %d of the indices past the width, want 4", refused)
	}

	// the free function's OTHER refusal, and the reason this test's name is a statement about the
	// whole method rather than about its range arm. An unmerged leaf outside the tree makes the
	// free function refuse a node that is perfectly in range, and the method would then answer
	// the empty list for a NON-BLANK node -- the root as readily as any other -- which reads as
	// sealing that node's path secret to nobody. The loop above never reaches that arm: its
	// fixture's list is [1 3] on an eight leaf tree, so every index it walks is in range. The arm
	// is unreachable because the container refuses the list at the door, and this is where that
	// is observed rather than assumed.
	if err := tree.SetParent(NodeIndex(3), &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x91, 32)),
		UnmergedLeaves: []LeafIndex{LeafIndex(tree.LeafWidth())},
	}); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("a parent carrying a leaf one past the width was accepted with err = %v", err)
	}
}

// TestSetParentRefusesAnUnmergedLeafTheTreeDoesNotHave is the door RatchetTree.Resolution's
// dropped error rests on.
//
// RFC 9420 section 7.5 refuses a node whose unmerged list reaches past the tree, and the method
// answers the EMPTY list for every refusal -- which is the answer an accepted blank subtree
// gives, so the two are not distinguishable through it. A tree holding one out-of-range unmerged
// leaf therefore turns "seal this path secret to everyone under this node" into "seal it to
// nobody", and no shape assertion, member count, round trip or tree hash of this container can
// see the difference. Before this refusal existed SetParent accepted such a list on a four leaf
// tree without a word, and the comment on Resolution asserted it could not happen.
//
// Every parent index of every width, and the boundary from BOTH sides: the widest list the tree
// can hold is accepted at the same position that refuses the next leaf along, so what is pinned
// here is a range check rather than a door that has stopped working.
func TestSetParentRefusesAnUnmergedLeafTheTreeDoesNotHave(t *testing.T) {
	for _, leafCount := range []uint32{1, 2, 3, 4, 5, 8} {
		tree, _ := treeUnderTest(t, leafCount)
		width := tree.LeafWidth()
		for x := uint32(1); x < tree.NodeWidth(); x += 2 {
			node := NodeIndex(x)
			if err := tree.Blank(node); err != nil {
				t.Fatalf("Blank(%d): %v", x, err)
			}
			// one past the width, two past it, and the largest value a leaf index holds, which
			// is what a truncated or hostile ratchet_tree is likeliest to carry
			for _, outside := range []LeafIndex{LeafIndex(width), LeafIndex(width) + 1, LeafIndex(0xffffffff)} {
				parent := &ParentNode{
					EncryptionKey:  HpkePublicKey(repeatByte(0x71, 32)),
					UnmergedLeaves: []LeafIndex{outside},
				}
				if err := tree.SetParent(node, parent); !errors.Is(err, ErrLeafIndexOutOfRange) {
					t.Fatalf("%d leaves: SetParent(%d, unmerged %d) err = %v, want ErrLeafIndexOutOfRange",
						leafCount, x, outside, err)
				}
				// and the tree math's own sentinel underneath it, because tree_errors.go's header
				// makes the wrap the thing a caller may ask either way about
				if err := tree.SetParent(node, parent); !errors.Is(err, ErrLeafOutOfRange) {
					t.Fatalf("%d leaves: SetParent(%d, unmerged %d) does not answer the tree math sentinel: %v",
						leafCount, x, outside, err)
				}
				if !tree.IsBlank(node) {
					t.Fatalf("%d leaves: node %d was occupied by a SetParent that reported a refusal", leafCount, x)
				}
			}
			// and every leaf the tree HAS, at the same position, is accepted
			inside := []LeafIndex{}
			for leaf := uint32(0); leaf < uint32(width); leaf += 1 {
				inside = append(inside, LeafIndex(leaf))
			}
			if err := tree.SetParent(node, &ParentNode{
				EncryptionKey:  HpkePublicKey(repeatByte(0x72, 32)),
				UnmergedLeaves: inside,
			}); err != nil {
				t.Fatalf("%d leaves: SetParent(%d) refused a list of every leaf the tree has: %v", leafCount, x, err)
			}
		}
		// the check is against the CURRENT width and the array only ever grows, so a leaf refused
		// before a doubling is accepted after it. That is the direction that keeps a stored list
		// from going out of range behind the check's back, and it is observed rather than argued.
		refused := LeafIndex(width)
		if err := tree.SetLeaf(LeafIndex(uint32(width)*2-1), testTreeLeaf(0)); err != nil {
			t.Fatalf("%d leaves: SetLeaf to grow: %v", leafCount, err)
		}
		if err := tree.SetParent(NodeIndex(1), &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(0x73, 32)),
			UnmergedLeaves: []LeafIndex{refused},
		}); err != nil {
			t.Fatalf("%d leaves: leaf %d is inside the grown tree and SetParent refused it: %v", leafCount, refused, err)
		}
	}
}

// ratchetTreeParentNodeDoorRow is one door a ParentNode reaches this container through, and what
// the container can and cannot promise at it.
type ratchetTreeParentNodeDoorRow struct {
	// copies says whether this door takes a *ParentNode the container copies on the way in,
	// which is the only kind of door a refusal can live on. A door that hands out the tree's OWN
	// node is documented to do exactly that, so a caller writing an out-of-range leaf through it
	// has built a tree this container never accepted -- which is the boundary Resolution's
	// comment draws, and the reason it draws it there instead of claiming a guarantee that is
	// not there to claim.
	copies bool
	// drive puts an unmerged leaf the tree does not have through this door and answers whether
	// the door refused it.
	drive func(t *testing.T, tree *RatchetTree, outside LeafIndex) bool
}

func ratchetTreeParentNodeDoorRows() map[string]ratchetTreeParentNodeDoorRow {
	return map[string]ratchetTreeParentNodeDoorRow{
		"SetParent": {
			copies: true,
			drive: func(t *testing.T, tree *RatchetTree, outside LeafIndex) bool {
				err := tree.SetParent(NodeIndex(3), &ParentNode{
					EncryptionKey:  HpkePublicKey(repeatByte(0x81, 32)),
					UnmergedLeaves: []LeafIndex{outside},
				})
				if err != nil && !errors.Is(err, ErrLeafIndexOutOfRange) {
					t.Fatalf("SetParent refused with %v, which is not the unmerged range refusal", err)
				}
				return err != nil
			},
		},
		"ParentAt": {
			copies: false,
			drive: func(t *testing.T, tree *RatchetTree, outside LeafIndex) bool {
				installLegalParent(t, tree)
				tree.ParentAt(NodeIndex(3)).UnmergedLeaves = []LeafIndex{outside}
				return false
			},
		},
		"Get": {
			copies: false,
			drive: func(t *testing.T, tree *RatchetTree, outside LeafIndex) bool {
				installLegalParent(t, tree)
				tree.Get(NodeIndex(3)).Parent.UnmergedLeaves = []LeafIndex{outside}
				return false
			},
		},
	}
}

// installLegalParent puts a parent node the container accepts at node 3, so that a door which
// EDITS the tree's own storage has something of the tree's own to edit.
func installLegalParent(t *testing.T, tree *RatchetTree) {
	t.Helper()
	if err := tree.SetParent(NodeIndex(3), &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x82, 32)),
		UnmergedLeaves: []LeafIndex{0},
	}); err != nil {
		t.Fatalf("installing a legal parent at node 3: %v", err)
	}
}

// ratchetTreeParentNodeDoorsInSource is every exported method of *RatchetTree a ParentNode -- and
// therefore an unmerged list -- can enter or be edited through, read off this package's source.
//
// Derived by SIGNATURE rather than listed, for guardrail 5's reason and for one specific to what
// is being claimed: RatchetTree.Resolution's comment now makes a statement about every tree this
// container ACCEPTED, and a table of the doors somebody remembered is a statement about those
// doors. A method is a door when the Node union or a ParentNode appears among its parameters or
// its results -- the first is how a list is installed, the second is how a caller reaches the
// tree's own list and writes through it -- so a later task adding either kind of method has to
// answer for it here rather than quietly widening the surface the claim is made over.
func ratchetTreeParentNodeDoorsInSource(t *testing.T) []string {
	t.Helper()
	found := []string{}
	for _, path := range packageSourcePaths(t) {
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || parsed.receiverOf(function) != "*RatchetTree" || !function.Name.IsExported() {
				continue
			}
			carries := false
			ast.Inspect(function.Type, func(node ast.Node) bool {
				ident, isIdent := node.(*ast.Ident)
				if isIdent && (ident.Name == "Node" || ident.Name == "ParentNode") {
					carries = true
				}
				return true
			})
			if carries {
				found = append(found, function.Name.Name)
			}
		}
	}
	if len(found) == 0 {
		t.Fatalf("no exported method of *RatchetTree carries a Node or a ParentNode, so the table above controls nothing")
	}
	slices.Sort(found)
	return found
}

// TestEveryDoorAParentNodeReachesTheRatchetTreeThroughIsHeldToTheUnmergedRange is the whole of
// what makes RatchetTree.Resolution's dropped error sound, said over the derived class of doors
// rather than over the one the fix was written at.
//
// The comment this replaced claimed a RatchetTree "always has ... an unmerged leaf inside it",
// and nothing anywhere enforced it. Two things are true instead, and both are stated here: every
// door that COPIES what it is handed refuses a leaf the tree does not have, and every door that
// hands out the tree's own node cannot -- it is documented to hand out live storage so a caller
// can edit what it installed. For the second kind this test shows the consequence rather than
// describing it, so the boundary Resolution's comment draws is a thing a reader can see.
func TestEveryDoorAParentNodeReachesTheRatchetTreeThroughIsHeldToTheUnmergedRange(t *testing.T) {
	rows := ratchetTreeParentNodeDoorRows()
	declared := ratchetTreeParentNodeDoorsInSource(t)
	controlled := slices.Sorted(maps.Keys(rows))
	if !slices.Equal(declared, controlled) {
		t.Fatalf("*RatchetTree carries a parent node through %v and this table drives %v", declared, controlled)
	}
	for _, name := range declared {
		row := rows[name]
		t.Run(name, func(t *testing.T) {
			tree, _ := treeUnderTest(t, 4)
			outside := LeafIndex(tree.LeafWidth())
			if row.copies != row.drive(t, tree, outside) {
				t.Fatalf("%s copies=%v and a copying door must refuse an unmerged leaf outside the tree while a door handing out the tree's own node cannot",
					name, row.copies)
			}
			if row.copies {
				if !tree.IsBlank(NodeIndex(3)) {
					t.Fatalf("%s reported a refusal and stored the node anyway", name)
				}
				return
			}
			// the door that cannot refuse, and the consequence spelled where it can be watched:
			// the tree now holds a node that is NOT blank whose resolution the free function
			// refuses, and the method answers the empty list for it -- which is the list a path
			// secret would be sealed to. That is the tree this container never accepted, and it
			// is why Resolution's comment draws its guarantee at the trees it did.
			if tree.IsBlank(NodeIndex(3)) {
				t.Fatalf("writing through %s did not reach the tree, so nothing below is about that door", name)
			}
			if _, err := Resolution(tree, NodeIndex(3)); !errors.Is(err, ErrLeafOutOfRange) {
				t.Fatalf("after writing through %s the free Resolution answered err = %v", name, err)
			}
			if got := tree.Resolution(NodeIndex(3)); len(got) != 0 {
				t.Fatalf("after writing through %s the method answered %v", name, got)
			}
		})
	}
}

// assertResolutionRefusesOnlyPastTheNodeWidth holds the free function's refusals over the WHOLE
// node array rather than at the one index a caller happened to ask about, and holds the method
// to the free function everywhere the free function accepts.
func assertResolutionRefusesOnlyPastTheNodeWidth(t *testing.T, tree *RatchetTree, where string) {
	t.Helper()
	for x := uint32(0); x < tree.NodeWidth()+4; x += 1 {
		free, err := Resolution(tree, NodeIndex(x))
		if outside := x >= tree.NodeWidth(); outside != (err != nil) {
			t.Fatalf("%s: Resolution(%d) of a %d node tree answered err = %v", where, x, tree.NodeWidth(), err)
		}
		method := tree.Resolution(NodeIndex(x))
		if err != nil {
			if !errors.Is(err, ErrNodeOutOfRange) {
				t.Fatalf("%s: Resolution(%d) refused with %v, and an out of range index is the only refusal a tree this container accepted may produce",
					where, x, err)
			}
			if method == nil || len(method) != 0 {
				t.Fatalf("%s: the free function refused index %d and the method answered %v", where, x, method)
			}
			continue
		}
		if !equalNodeIndices(method, free) {
			t.Fatalf("%s: at node %d the method answered %v and the free function %v", where, x, method, free)
		}
	}
}

// TestTheOnlyResolutionRefusalATreeThisContainerAcceptedCanProduceIsAnOutOfRangeIndex is the
// property RatchetTree.Resolution's dropped error is sound under, driven rather than argued.
//
// Every copying door is exercised with the widest argument it accepts and the invariant is
// re-read after each one, because the two failures worth catching are asymmetric: storing a list
// the width does not cover is what SetParent now refuses, and WIDENING the tree under a list
// already stored is a thing SetLeaf does on purpose and no refusal defends against. The second
// is sound only because the array grows and never shrinks, which is an argument worth watching
// hold rather than believing.
func TestTheOnlyResolutionRefusalATreeThisContainerAcceptedCanProduceIsAnOutOfRangeIndex(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{1, 2, 3, 4, 5, 8} {
		tree, members := newTestTree(t, crypto, n)
		assertResolutionRefusesOnlyPastTheNodeWidth(t, tree, fmt.Sprintf("n=%d as built", n))

		every := []LeafIndex{}
		for leaf := uint32(0); leaf < uint32(tree.LeafWidth()); leaf += 1 {
			every = append(every, LeafIndex(leaf))
		}
		for x := uint32(1); x < tree.NodeWidth(); x += 2 {
			if err := tree.SetParent(NodeIndex(x), &ParentNode{
				EncryptionKey:  HpkePublicKey(repeatByte(byte(0x90+x), 32)),
				UnmergedLeaves: every,
			}); err != nil {
				t.Fatalf("n=%d SetParent(%d) refused every leaf of the tree: %v", n, x, err)
			}
		}
		assertResolutionRefusesOnlyPastTheNodeWidth(t, tree, fmt.Sprintf("n=%d every parent full", n))

		// the door that changes the width UNDER every list already stored
		if err := tree.SetLeaf(LeafIndex(uint32(tree.LeafWidth())+1), testTreeLeaf(1)); err != nil {
			t.Fatalf("n=%d SetLeaf past the width: %v", n, err)
		}
		assertResolutionRefusesOnlyPastTheNodeWidth(t, tree, fmt.Sprintf("n=%d after growing", n))

		for _, member := range members {
			if err := tree.BlankDirectPath(member.LeafIndex); err != nil {
				t.Fatalf("n=%d BlankDirectPath(%d): %v", n, member.LeafIndex, err)
			}
		}
		if err := tree.Blank(LeafIndex(0).NodeIndex()); err != nil {
			t.Fatalf("n=%d Blank(0): %v", n, err)
		}
		assertResolutionRefusesOnlyPastTheNodeWidth(t, tree, fmt.Sprintf("n=%d after blanking", n))
	}
}

// resolutionUnmergedRule is one way of hanging an unmerged list on every parent of a sweep tree.
//
// The rules are DERIVED from each node's own subtree rather than written out as literal lists,
// so the same rule applies at every node of every width and the sweep does not depend on somebody
// having remembered which nodes to decorate. RFC 9420 section 7.9 requires a node's unmerged
// leaves to be non-blank leaves inside its own subtree; the sweep respects the subtree half,
// since a leaf outside it would be a tree no validator accepts, and deliberately does not
// respect the non-blank half, because whether an unmerged leaf happens to be blank changes
// nothing about section 7.5's rule and a resolution walk that started caring would be a walk
// doing section 7.9's job in the wrong place.
type resolutionUnmergedRule struct {
	name  string
	apply func(x NodeIndex) []LeafIndex
}

func resolutionUnmergedRules() []resolutionUnmergedRule {
	return []resolutionUnmergedRule{
		{
			// the half of the class every "nobody has been added since the last commit" test
			// lives in, and the half a walk that forgot unmerged leaves entirely still passes
			name:  "no unmerged leaves",
			apply: func(x NodeIndex) []LeafIndex { return nil },
		},
		{
			// the widest list a node can carry, so the resolution of a non-blank parent is
			// longer than the resolution of the whole subtree below it
			name:  "every leaf of the subtree",
			apply: func(x NodeIndex) []LeafIndex { return subtreeLeavesWhere(x, 1, 0) },
		},
		{
			// a list that is neither empty nor everything, and whose entries interleave with
			// the nodes a blank descent would have produced
			name:  "every other leaf of the subtree",
			apply: func(x NodeIndex) []LeafIndex { return subtreeLeavesWhere(x, 2, 0) },
		},
		{
			// the one entry list, and the one entry chosen so the resolution is NOT in
			// ascending node index order: the first leaf of a subtree sits below the node
			// heading it, so a node carrying it resolves to [x, something smaller]. That is
			// figure 10 of RFC 9420, whose [X, B] is [3, 2], and it is the shape a comparison
			// that sorted before comparing would stop being able to see. The LAST leaf of a
			// subtree has a node index above its head and would have made every answer here
			// ascending, which is worth writing down because it is the version this rule was
			// first written as.
			name: "the first leaf of the subtree",
			apply: func(x NodeIndex) []LeafIndex {
				first, _ := SubtreeLeaves(x)
				return []LeafIndex{first}
			},
		},
	}
}

// subtreeLeavesWhere is every leaf under x whose offset within the subtree is congruent to
// offset modulo step, ascending -- which is what RFC 9420 section 7.9.2 requires an
// unmerged_leaves vector to be.
func subtreeLeavesWhere(x NodeIndex, step uint32, offset uint32) []LeafIndex {
	first, last := SubtreeLeaves(x)
	out := []LeafIndex{}
	for leaf := uint32(first); leaf <= uint32(last); leaf += 1 {
		if (leaf-uint32(first))%step == offset {
			out = append(out, LeafIndex(leaf))
		}
	}
	return out
}

// resolutionSweep builds one tree of the given leaf width and walks every blanking pattern of
// its node array, handing each pattern to visit.
//
// EVERY pattern and not a sample: a resolution defect lives in the relationship between a node's
// blankness and its children's, so which positions are blank is the input, and a sweep that
// picked a few trees would be picking a few of exactly the thing under test. Two to the node
// width is 32,768 at eight leaves, which is the whole space at every width this runs at.
//
// The node array is written directly rather than through SetLeaf and SetParent. Those copy the
// node they are handed, deliberately and for a reason recorded on them, and this sweep would
// spend two million clones on it; what the resolution walk reads is IsBlank, LeafCount and
// UnmergedLeaves, and all three read this array. The nodes are built once per rule.
func resolutionSweep(t *testing.T, leafWidth uint32, rule resolutionUnmergedRule,
	visit func(tree *RatchetTree, pattern uint32)) {
	t.Helper()
	nodeWidth := NodeWidth(LeafCount(leafWidth))
	if nodeWidth == 0 || nodeWidth > 20 {
		t.Fatalf("a sweep over %d leaves is %d nodes, which is not a space to enumerate", leafWidth, nodeWidth)
	}
	filled := make([]*Node, nodeWidth)
	for x := uint32(0); x < nodeWidth; x += 1 {
		node := NodeIndex(x)
		if node.IsLeaf() {
			filled[x] = &Node{NodeType: NodeTypeLeaf, Leaf: testTreeLeaf(x / 2)}
			continue
		}
		filled[x] = &Node{NodeType: NodeTypeParent, Parent: &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(byte(0xd0+x), 32)),
			UnmergedLeaves: rule.apply(node),
		}}
	}
	tree := &RatchetTree{nodes: make([]*Node, nodeWidth)}
	for pattern := uint32(0); pattern < uint32(1)<<nodeWidth; pattern += 1 {
		for x := uint32(0); x < nodeWidth; x += 1 {
			if pattern>>x&1 == 1 {
				tree.nodes[x] = filled[x]
			} else {
				tree.nodes[x] = nil
			}
		}
		visit(tree, pattern)
	}
}

// TestResolutionAgreesWithTheRfcRecursionOverEveryBlankingPattern is the derived half of this
// task: every blanking pattern of every node of every small tree, under four rules for the
// unmerged lists, against the recursion RFC 9420 section 7.5 states.
//
// Derived over the blank positions rather than sampled, because the blank positions ARE the
// input to a resolution: which node is blank decides whether the walk emits it or descends past
// it, and a test that blanked three positions somebody chose is a test of those three. At one,
// two and four leaves this is the entire space; at eight leaves it is the entire space of the
// two rules that bracket the unmerged clause -- none at all, and the widest list a node can
// carry -- which is what keeps the run inside a few seconds while still covering every shape.
//
// A resolution longer than one is counted, and required, so this cannot pass over a run in which
// every answer was the empty list.
func TestResolutionAgreesWithTheRfcRecursionOverEveryBlankingPattern(t *testing.T) {
	rules := resolutionUnmergedRules()
	// eight leaves is 32,768 patterns per rule, so two of the four run there rather than all
	// four: the rule with no unmerged list at all and the rule with the widest one, which are the
	// pair that brackets the unmerged clause.
	//
	// Chosen by NAME and refused if a name is gone, which is the correction a review made here.
	// This was written as "ruleIndex != 0 && ruleIndex != 1" under a comment calling those two
	// "the first and the last rule" -- rule 1 is the second of four -- and an index pair is a
	// description of the table's current ORDER rather than of the two rules this is about, so
	// reordering resolutionUnmergedRules would have swept eight leaves under two rules nobody
	// chose while the comment went on naming the two it was written for.
	wideSweepRules := []string{"no unmerged leaves", "every leaf of the subtree"}
	for _, name := range wideSweepRules {
		if !slices.ContainsFunc(rules, func(rule resolutionUnmergedRule) bool { return rule.name == name }) {
			t.Fatalf("the eight leaf sweep runs the rule %q and resolutionUnmergedRules declares no rule of that name", name)
		}
	}
	compared := 0
	nonEmpty := 0
	for _, leafWidth := range []uint32{1, 2, 4, 8} {
		for _, rule := range rules {
			if leafWidth == 8 && !slices.Contains(wideSweepRules, rule.name) {
				continue
			}
			nodeWidth := NodeWidth(LeafCount(leafWidth))
			resolutionSweep(t, leafWidth, rule, func(tree *RatchetTree, pattern uint32) {
				for x := uint32(0); x < nodeWidth; x += 1 {
					got := tree.Resolution(NodeIndex(x))
					want := rfcResolution(tree, NodeIndex(x))
					compared += 1
					if len(got) > 0 {
						nonEmpty += 1
					}
					if !equalNodeIndices(got, want) {
						t.Fatalf("%d leaves, %s, pattern %0*b: Resolution(%d) = %v, and the RFC recursion says %v",
							leafWidth, rule.name, int(nodeWidth), pattern, x, got, want)
					}
				}
			})
		}
	}
	// the space, written down rather than derived from the loop that walked it: one leaf is 1
	// node over 2 patterns, two leaves 3 over 8 and four leaves 7 over 128, each under all four
	// rules, which is 3,688; eight leaves is 15 nodes over 32,768 patterns under two rules,
	// which is 983,040.
	if compared != 986728 {
		t.Fatalf("the sweep made %d comparisons and the space it walks is 986728", compared)
	}
	if nonEmpty*4 < compared {
		t.Fatalf("only %d of %d resolutions were non-empty, so most of this sweep compared nothing against nothing",
			nonEmpty, compared)
	}
}

// TestAResolutionIsOftenNotInAscendingNodeOrder is the guard that makes every comparison in this
// file worth making.
//
// A resolution ascends by node index everywhere EXCEPT at an unmerged leaf, which follows
// immediately behind the node carrying it and is often below it -- figure 10 of RFC 9420 has
// [X, B], which is [3, 2]. If that never happened, a test comparing two resolutions as sets, or
// sorting them before comparing, would be indistinguishable from one comparing them elementwise,
// and the sweep above would pass every permutation there is. So the case is counted rather than
// assumed, and a sweep that stopped producing it fails here.
func TestAResolutionIsOftenNotInAscendingNodeOrder(t *testing.T) {
	rules := resolutionUnmergedRules()
	rule := rules[len(rules)-1]
	if rule.name != "the first leaf of the subtree" {
		t.Fatalf("this test is written against the one entry rule and the last rule is %q", rule.name)
	}
	nodeWidth := NodeWidth(LeafCount(4))
	longEnough := 0
	outOfOrder := 0
	resolutionSweep(t, 4, rule, func(tree *RatchetTree, pattern uint32) {
		for x := uint32(0); x < nodeWidth; x += 1 {
			got := tree.Resolution(NodeIndex(x))
			if len(got) < 2 {
				continue
			}
			longEnough += 1
			if slices.IsSorted(got) {
				continue
			}
			outOfOrder += 1
			// and the consequence stated rather than implied: the sorted copy is a DIFFERENT
			// list, so a comparison that sorted first would have accepted an answer that seals
			// each path secret to the wrong member
			sorted := slices.Clone(got)
			slices.Sort(sorted)
			if equalNodeIndices(sorted, got) {
				t.Fatalf("pattern %0*b: %v is reported out of order and equals its own sorted form",
					int(nodeWidth), pattern, got)
			}
		}
	})
	if longEnough == 0 {
		t.Fatal("no resolution in the sweep held more than one node, so order was never observable")
	}
	if outOfOrder == 0 {
		t.Fatal("every resolution in the sweep was in ascending node order, so a set comparison would pass this file")
	}
}

// corpusTreeOfShape builds a RatchetTree that is blank exactly where a decoded corpus tree is
// blank and carries exactly the unmerged lists it carries.
//
// Through the shape the tree math plan's decoder already produced, and NOT through a second
// reading of the corpus bytes. There is one decoder of a published ratchet_tree in this package's
// tests -- decodeRatchetTreeShape in tree_math_test.go, which walks the presentation language by
// hand and is independent of this package's own codecs -- and task 11 will add the production
// one. A third would be two of them able to disagree about a truncation, and the disagreement
// would show up as this container failing a corpus it actually reproduces.
//
// The node CONTENTS are placeholders, because resolution reads three things about a tree and
// none of them is a key: whether a position is blank, what a parent's unmerged_leaves holds, and
// how many leaves there are. What this therefore compares is the container's NodeShape against
// the corpus, which is the seam this task adds and the one seam
// TestResolutionAgainstPublishedTreeValidationVectors does not cross -- that test runs the same
// corpus against the decoder's own shape struct, so a RatchetTree whose IsBlank or UnmergedLeaves
// answered wrongly would pass it and fail here.
func corpusTreeOfShape(t *testing.T, label string, shape *ratchetTreeShape, width int) *RatchetTree {
	t.Helper()
	tree := &RatchetTree{nodes: make([]*Node, width)}
	for x := uint32(0); x < uint32(width); x += 1 {
		node := NodeIndex(x)
		if shape.IsBlank(node) {
			continue
		}
		if node.IsLeaf() {
			tree.nodes[x] = &Node{NodeType: NodeTypeLeaf, Leaf: testTreeLeaf(x / 2)}
			continue
		}
		tree.nodes[x] = &Node{NodeType: NodeTypeParent, Parent: &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(byte(0xe0+x), 32)),
			UnmergedLeaves: shape.UnmergedLeaves(node),
		}}
	}
	// the container derives its leaf count from the array it was given and the decoder derived
	// its own from the wire form, so this is two independent readings of one tree's width rather
	// than one value compared against itself
	if tree.LeafCount() != shape.LeafCount() {
		t.Fatalf("%s: the container reads %d leaves out of a %d node array and the decoder read %d",
			label, tree.LeafCount(), width, shape.LeafCount())
	}
	for x := uint32(0); x < uint32(width); x += 1 {
		node := NodeIndex(x)
		if tree.IsBlank(node) != shape.IsBlank(node) {
			t.Fatalf("%s: node %d is blank=%v in the container and blank=%v in the decoded shape",
				label, x, tree.IsBlank(node), shape.IsBlank(node))
		}
		if !slices.Equal(tree.UnmergedLeaves(node), shape.UnmergedLeaves(node)) {
			t.Fatalf("%s: node %d carries %v in the container and %v in the decoded shape",
				label, x, tree.UnmergedLeaves(node), shape.UnmergedLeaves(node))
		}
	}
	return tree
}

// TestTheRatchetTreeReproducesEveryPublishedResolution is the independent half of this task: the
// resolution of every node of every tree the mlswg publishes, computed through the real container
// and compared against the answer the mlswg publishes for it.
//
// Independent in the way the sweeps are not. This implementation and the recursion the sweeps
// compare against were both written from one reading of section 7.5, so a misreading shared by
// the two of them survives every sweep in this file; the corpus was produced by implementations
// that never saw either. Twenty-one of its published resolutions are a node followed by leaves
// merged into it, so the unmerged clause is exercised by the corpus and not only by fixtures of
// this file's own making, and seven of them are not in ascending node index order, so an
// implementation that sorted its answer fails here as well as in the sweep above.
//
// Every entry and not only the ciphersuites this package registers, because nothing here does any
// crypto: a resolution is a function of the tree's SHAPE, so the corpus's seven suites are seven
// more trees rather than seven key formats, and skipping five of them would be throwing evidence
// away for no reason. The counts are written down rather than derived from the loop that produced
// them, and three of the four are the constants the tree math plan already pinned this corpus
// with, so a corpus update moves one number in one place.
func TestTheRatchetTreeReproducesEveryPublishedResolution(t *testing.T) {
	entries := LoadVectorFile(t, treeValidationVectorFile)
	if len(entries) != treeValidationEntryCount {
		t.Fatalf("tree-validation entries: %d, want %d", len(entries), treeValidationEntryCount)
	}
	compared := 0
	withUnmerged := 0
	notAscending := 0
	for entry, raw := range entries {
		vector := treeValidationVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("entry %d: %v", entry, err)
		}
		label := fmt.Sprintf("tree-validation entry %d", entry)
		shape, width := decodeRatchetTreeShape(t, label, MustHex(t, vector.Tree))
		if width != len(vector.Resolutions) {
			t.Fatalf("%s: node width %d from the tree, %d published resolutions",
				label, width, len(vector.Resolutions))
		}
		tree := corpusTreeOfShape(t, label, shape, width)
		for x, published := range vector.Resolutions {
			want := make([]NodeIndex, 0, len(published))
			for _, node := range published {
				want = append(want, NodeIndex(node))
			}
			got := tree.Resolution(NodeIndex(x))
			if !equalNodeIndices(got, want) {
				t.Fatalf("%s: Resolution(%d) = %v, and the corpus publishes %v", label, x, got, want)
			}
			compared += 1
			if len(want) > 1 && want[0] == NodeIndex(x) {
				withUnmerged += 1
			}
			if !slices.IsSorted(want) {
				notAscending += 1
			}
		}
	}
	if compared != treeValidationResolutionCount {
		t.Errorf("compared %d published resolutions, want %d", compared, treeValidationResolutionCount)
	}
	// the unmerged half of section 7.5, counted rather than assumed. An implementation that
	// appended no unmerged leaves at all produces a resolution that is a strict SUBSET of the
	// right one, and every case where nobody has been added since the last commit still passes
	// -- so a run of this corpus that reached none of these would be reporting a clean bill over
	// the one clause that fails silently. The count is the tree math plan's own pin on this
	// corpus, so the two readings of it have to agree.
	if withUnmerged != treeValidationUnmergedCount {
		t.Errorf("%d published resolutions carry a node's unmerged leaves, want %d",
			withUnmerged, treeValidationUnmergedCount)
	}
	if notAscending != 7 {
		t.Errorf("%d published resolutions are not in ascending node order, want 7; an implementation that sorted its answer would pass a run with none",
			notAscending)
	}
}

// ---------------------------------------------------------------------------
// the ratchet_tree extension codec of RFC 9420 section 12.4.3.3, and ValSem300
// ---------------------------------------------------------------------------

// productGroupLeafCount is the group MASTER sizes this product for: 500 members with two
// devices each, one leaf per device.
//
// It is here as a named constant rather than inline because the size test below is the only
// thing in this package that can tell MaxVectorLength from MaxRatchetTreeLength, and it can do
// that only while the fixture is big enough to exceed the first. A number that quietly shrank
// would leave that test passing against either limit.
const productGroupLeafCount = 1000

// the same encoding as syntax.Marshal(tree) but with one absent node appended, which is exactly
// what ValSem300 forbids.
//
// Rebuilt through the codec rather than patched into the bytes, because the vector's length
// prefix moves when the body grows and a hand patched prefix would produce a truncation rather
// than the padded array this is meant to be.
func marshalRatchetTreeWithTrailingBlank(tree *RatchetTree) ([]byte, error) {
	canonical, err := syntax.Marshal(tree)
	if err != nil {
		return nil, err
	}
	body, err := syntax.NewReader(canonical).ReadSub()
	if err != nil {
		return nil, err
	}
	inner := syntax.NewWriter()
	for !body.Empty() {
		node := &OptionalNode{}
		if err := node.UnmarshalMLS(body); err != nil {
			return nil, err
		}
		if err := node.MarshalMLS(inner); err != nil {
			return nil, err
		}
	}
	if err := (&OptionalNode{}).MarshalMLS(inner); err != nil {
		return nil, err
	}
	payload, err := inner.Bytes()
	if err != nil {
		return nil, err
	}
	w := syntax.NewWriter()
	w.WriteOpaque(payload)
	return w.Bytes()
}

// handWrittenVarint is the RFC 9420 section 2.1.2 length prefix, written out here rather than
// taken from the codec, so the golden below states the framing instead of agreeing with it.
func handWrittenVarint(t *testing.T, n int) []byte {
	t.Helper()
	switch {
	case n < 1<<6:
		return []byte{byte(n)}
	case n < 1<<14:
		return []byte{byte(n>>8) | 0x40, byte(n)}
	case n < 1<<30:
		return []byte{byte(n>>24) | 0x80, byte(n >> 16), byte(n >> 8), byte(n)}
	}
	t.Fatalf("no RFC 9420 varint encodes %d", n)
	return nil
}

// TestRatchetTreeMarshalMatchesAHandDerivedGolden states the array encoding from the RFC
// without reference to the encoder, which is the only thing in this file that separates it
// from its mirror images.
//
// Four of them, and none is visible to a round trip: the presence octet written AFTER the node
// type rather than before, the node type octet dropped from both halves, the parents emitted
// before the leaves rather than interleaved in array order, and the trailing blanks left in.
// Each of those encodes, decodes, re-encodes byte exact against itself and hashes a tree no
// peer computes.
//
// The leaf and parent bodies come from their own codecs, deliberately: what this golden pins is
// THIS layer -- the vector prefix, the presence octet, the type octet, the array order and the
// truncation -- and those two structures have hand derived goldens of their own above.
func TestRatchetTreeMarshalMatchesAHandDerivedGolden(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 3)
	parent := &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x88}, 32)),
		ParentHash:     bytes.Repeat([]byte{0x99}, 32),
		UnmergedLeaves: []LeafIndex{1},
	}
	if err := tree.SetParent(NodeIndex(1), parent); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	// the array of a three member group in a four leaf tree: leaves at 0, 2, 4, the parent
	// this test installed at 1, a blank at 3, and nodes 5 and 6 -- the right hand parent and
	// the fourth leaf -- stripped because they are trailing blanks.
	body := []byte{}
	for _, entry := range []struct {
		present bool
		kind    NodeType
		value   syntax.Marshaler
	}{
		{present: true, kind: NodeTypeLeaf, value: tree.Leaf(LeafIndex(0))},
		{present: true, kind: NodeTypeParent, value: parent},
		{present: true, kind: NodeTypeLeaf, value: tree.Leaf(LeafIndex(1))},
		{present: false},
		{present: true, kind: NodeTypeLeaf, value: tree.Leaf(LeafIndex(2))},
	} {
		if !entry.present {
			body = append(body, 0x00)
			continue
		}
		encoded, err := syntax.Marshal(entry.value)
		if err != nil {
			t.Fatalf("marshal a golden node: %v", err)
		}
		body = append(body, 0x01, byte(entry.kind))
		body = append(body, encoded...)
	}
	want := append(handWrittenVarint(t, len(body)), body...)

	got, err := syntax.Marshal(tree)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the ratchet_tree encoding is %d bytes beginning %x, and the hand derived array is %d bytes beginning %x",
			len(got), got[:min(len(got), 24)], len(want), want[:min(len(want), 24)])
	}
	// and the truncation is a fact about the ARRAY rather than about these bytes: a four leaf
	// tree has seven nodes and this encoding carries five entries.
	if tree.NodeWidth() != 7 {
		t.Fatalf("the fixture is %d nodes wide, and this golden is written for the seven node tree a three member group sits in", tree.NodeWidth())
	}
	entries := 0
	scan, err := syntax.NewReader(got).ReadSub()
	if err != nil {
		t.Fatalf("ReadSub: %v", err)
	}
	for !scan.Empty() {
		if err := (&OptionalNode{}).UnmarshalMLS(scan); err != nil {
			t.Fatalf("scan entry %d: %v", entries, err)
		}
		entries += 1
	}
	if entries != 5 {
		t.Fatalf("the encoding carries %d entries over a seven node tree, want the five the trailing blank rule leaves", entries)
	}
}

func TestRatchetTreeMarshalRoundTrip(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{1, 2, 3, 5, 8} {
		tree, _ := newTestTree(t, crypto, n)
		if err := tree.SetParent(NodeIndex(1), &ParentNode{
			EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x88}, 32)),
			ParentHash:     bytes.Repeat([]byte{0x99}, 32),
			UnmergedLeaves: []LeafIndex{1},
		}); n >= 2 && err != nil {
			t.Fatalf("n=%d SetParent: %v", n, err)
		}
		encoded, err := syntax.Marshal(tree)
		if err != nil {
			t.Fatalf("n=%d Marshal: %v", n, err)
		}
		out, err := UnmarshalRatchetTree(encoded)
		if err != nil {
			t.Fatalf("n=%d UnmarshalRatchetTree: %v", n, err)
		}
		if out.MemberCount() != n {
			t.Fatalf("n=%d decoded member count = %d", n, out.MemberCount())
		}
		reencoded, err := syntax.Marshal(out)
		if err != nil {
			t.Fatalf("n=%d re-Marshal: %v", n, err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("n=%d re-encode differs", n)
		}
		// the decoded VALUE and not only the bytes, which is the half a dropped field is
		// invisible to: a codec that lost the parent node from both halves re-encodes byte
		// exact and hands back a tree with a blank where the parent stood.
		if n >= 2 {
			decodedParent := out.ParentAt(NodeIndex(1))
			if decodedParent == nil {
				t.Fatalf("n=%d the parent at node 1 did not survive the round trip", n)
			}
			if !bytes.Equal(decodedParent.EncryptionKey, bytes.Repeat([]byte{0x88}, 32)) ||
				!bytes.Equal(decodedParent.ParentHash, bytes.Repeat([]byte{0x99}, 32)) ||
				!slices.Equal(decodedParent.UnmergedLeaves, []LeafIndex{1}) {
				t.Fatalf("n=%d the decoded parent is %+v", n, decodedParent)
			}
		}
		for i := uint32(0); i < n; i += 1 {
			before, after := tree.Leaf(LeafIndex(i)), out.Leaf(LeafIndex(i))
			if after == nil {
				t.Fatalf("n=%d leaf %d is blank after the round trip", n, i)
			}
			if !bytes.Equal(after.SignatureKey, before.SignatureKey) ||
				!bytes.Equal(after.EncryptionKey, before.EncryptionKey) {
				t.Fatalf("n=%d leaf %d came back holding another leaf's keys", n, i)
			}
		}
		// and the tree the decoder built is the same shape, not merely the same members
		if out.LeafWidth() != tree.LeafWidth() || out.NodeWidth() != tree.NodeWidth() {
			t.Fatalf("n=%d decoded a %d leaf / %d node tree out of a %d leaf / %d node one",
				n, out.LeafWidth(), out.NodeWidth(), tree.LeafWidth(), tree.NodeWidth())
		}
	}
}

// TestEveryDecodedRatchetTreeIsACompleteTree is the half of the decode the round trip cannot
// see.
//
// RFC 9420 section 12.4.3.3 has the receiver "extend the tree to the right until it has a
// length of the form 2^(d+1) - 1", and a decoder that skipped that step is invisible to every
// symmetry property this file holds: the truncated array of a three member group is five nodes,
// which is 2n-1 for n=3, so it round trips byte exact, reports three members, and puts the root
// at node 3 where the group's root is node 3 of a SEVEN node tree. Every direct path, copath,
// parent hash and tree hash computed against it is then taken over a different tree.
//
// So the property is the shape and not the bytes, asked through the arithmetic the container
// itself is built on rather than through a table of widths: the leaf count must be one
// IsFullLeafCount accepts, the node width must be NodeWidth of that count, and the width must
// be the one the encoder's own tree had.
func TestEveryDecodedRatchetTreeIsACompleteTree(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 17} {
		tree, _ := newTestTree(t, crypto, n)
		encoded, err := syntax.Marshal(tree)
		if err != nil {
			t.Fatalf("n=%d Marshal: %v", n, err)
		}
		out, err := UnmarshalRatchetTree(encoded)
		if err != nil {
			t.Fatalf("n=%d UnmarshalRatchetTree: %v", n, err)
		}
		if !IsFullLeafCount(out.LeafWidth()) {
			t.Errorf("n=%d decoded a tree of %d leaves, which is not a complete tree", n, out.LeafWidth())
		}
		if got := NodeWidth(out.LeafWidth()); got != out.NodeWidth() {
			t.Errorf("n=%d decoded %d nodes over %d leaves, and a complete tree of that many leaves is %d nodes",
				n, out.NodeWidth(), out.LeafWidth(), got)
		}
		if _, err := LeafCountFromNodeWidth(out.NodeWidth()); err != nil {
			t.Errorf("n=%d decoded a %d node array, which is not 2n-1 for any n: %v", n, out.NodeWidth(), err)
		}
		if out.LeafWidth() != tree.LeafWidth() {
			t.Errorf("n=%d decoded a %d leaf tree out of a %d leaf one, so the root moved",
				n, out.LeafWidth(), tree.LeafWidth())
		}
		// the padding is BLANK and not something the decoder invented, and it is where the
		// encoder's own tree had blanks
		for x := uint32(0); x < out.NodeWidth(); x += 1 {
			if out.IsBlank(NodeIndex(x)) != tree.IsBlank(NodeIndex(x)) {
				t.Errorf("n=%d node %d is blank=%v after the round trip and blank=%v before it",
					n, x, out.IsBlank(NodeIndex(x)), tree.IsBlank(NodeIndex(x)))
			}
		}
	}
}

// TestTheRatchetTreeCodecIsHandedTheRaisedLimitAtTheProductsGroupSize is the only test in this
// package that can tell MaxVectorLength from MaxRatchetTreeLength.
//
// p1 caps every vector at MaxVectorLength, one mebibyte, EXCEPT the ratchet tree, which gets
// MaxRatchetTreeLength, sixteen. That exception exists for this structure, and it is not a
// margin: MASTER sizes the product at 500 members with two devices each, and every leaf of this
// profile carries a 1216 byte X-Wing key in an urmessage_leaf_keys extension, so the thousand
// leaf tree that group sits in encodes to about 1.33 MiB. A codec handed the default limit
// refuses that tree -- at ErrLengthExceedsMax, which reads as a corrupt Welcome rather than as
// a limit.
//
// An eight leaf tree cannot see any of this, which is why the size is asserted before anything
// else: a fixture that shrank below the default limit would leave every assertion below passing
// against either bound, and the test would go on reporting a clean bill over the one decision it
// exists to make. The two directions are then failed SEPARATELY, because a raise wired into the
// decode alone still refuses to publish this product's own group.
func TestTheRatchetTreeCodecIsHandedTheRaisedLimitAtTheProductsGroupSize(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, productGroupLeafCount)
	encoded, err := marshalRatchetTree(tree)
	if err != nil {
		t.Fatalf("marshalRatchetTree at %d leaves: %v; this is the encode the raised bound exists for",
			productGroupLeafCount, err)
	}
	t.Logf("a %d leaf tree (%d members x 2 devices) encodes to %d bytes; MaxVectorLength is %d and MaxRatchetTreeLength is %d",
		productGroupLeafCount, productGroupLeafCount/2, len(encoded), syntax.MaxVectorLength, syntax.MaxRatchetTreeLength)
	if len(encoded) <= syntax.MaxVectorLength {
		t.Fatalf("the fixture encodes to %d bytes, which the default limit of %d accepts, so nothing below can tell the two limits apart",
			len(encoded), syntax.MaxVectorLength)
	}
	if len(encoded) > syntax.MaxRatchetTreeLength {
		t.Fatalf("the fixture encodes to %d bytes and the ratchet tree bound is %d, so this product's own group does not fit the limit p1 raised for it",
			len(encoded), syntax.MaxRatchetTreeLength)
	}

	// the ENCODE at the default limit refuses it, which is what makes marshalRatchetTree's
	// existence a decision rather than a convenience wrapper
	if _, err := syntax.Marshal(tree); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("syntax.Marshal of a %d leaf tree answered %v, want syntax.ErrLengthExceedsMax; if the default limit encodes this tree the raised one is not load bearing",
			productGroupLeafCount, err)
	}
	// and the DECODE at the default limit refuses it
	if err := syntax.Unmarshal(encoded, &RatchetTree{}); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("syntax.Unmarshal of the same bytes answered %v, want syntax.ErrLengthExceedsMax", err)
	}

	out, err := UnmarshalRatchetTree(encoded)
	if err != nil {
		t.Fatalf("UnmarshalRatchetTree at %d leaves: %v; this is the decode the raised bound exists for",
			productGroupLeafCount, err)
	}
	if out.MemberCount() != productGroupLeafCount {
		t.Fatalf("decoded %d members out of a %d member tree", out.MemberCount(), productGroupLeafCount)
	}
	reencoded, err := marshalRatchetTree(out)
	if err != nil {
		t.Fatalf("re-marshalRatchetTree: %v", err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatalf("the re-encoding of a %d leaf tree differs from its encoding", productGroupLeafCount)
	}
	// the extension a GroupInfo carries is these same bytes under this extension's own tag
	ext, err := tree.Encode()
	if err != nil {
		t.Fatalf("Encode at %d leaves: %v", productGroupLeafCount, err)
	}
	if !bytes.Equal(ext.ExtensionData, encoded) {
		t.Errorf("the extension body is not the tree encoding")
	}
}

func TestRatchetTreeRefusesTrailingBlankNodes(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 3)
	encoded, err := syntax.Marshal(tree)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// append one more optional<Node> that is absent. the length prefix moves, so rebuild it
	// rather than patching bytes.
	padded, err := marshalRatchetTreeWithTrailingBlank(tree)
	if err != nil {
		t.Fatalf("marshalRatchetTreeWithTrailingBlank: %v", err)
	}
	if bytes.Equal(padded, encoded) {
		t.Fatalf("the padded encoding is identical to the canonical one")
	}
	// the padded array is a legal ENCODING that this decoder must refuse, and not merely one
	// that fails to parse: everything up to the last entry decodes, which is what makes the
	// refusal a rule rather than a syntax error.
	if _, err := UnmarshalRatchetTree(encoded); err != nil {
		t.Fatalf("the canonical encoding of the same tree does not decode: %v", err)
	}
	if _, err := UnmarshalRatchetTree(padded); !errors.Is(err, errTrailingBlankNodes) {
		t.Fatalf("err = %v, want the ValSem300 refusal", err)
	}
	// an array that is nothing BUT blanks is the same refusal, which is the case a check
	// written over the first entry rather than the last would report differently
	allBlank := syntax.NewWriter()
	allBlank.WriteOpaque([]byte{0x00, 0x00, 0x00})
	blanks, err := allBlank.Bytes()
	if err != nil {
		t.Fatalf("build the all blank array: %v", err)
	}
	if _, err := UnmarshalRatchetTree(blanks); !errors.Is(err, errTrailingBlankNodes) {
		t.Fatalf("an array of three absent nodes answered %v, want the ValSem300 refusal", err)
	}
	// the same fact through the accessor the group lifecycle and validation plans call, so a
	// tree that was built rather than decoded is caught too.
	if !tree.HasTrailingBlankNodes() {
		t.Fatalf("a width-4 tree holding three leaves has trailing blank nodes")
	}
}

// TestRatchetTreeRefusesAnEmptyNodeArray is the case ValSem300 accepts without ever looking.
//
// The rule is naturally written "if the array is non-empty and its last entry is blank, refuse",
// and the guard is what makes it wrong: a vector of ZERO entries carries no non-blank last node,
// so RFC 9420 section 12.4.3.3's "the receiver MUST check that the last node in ratchet_tree is
// non-blank" is failed by it exactly as it is failed by a padded one -- and the guarded form
// skips the whole rule for it. What the earlier reading produced was the one leaf blank tree,
// which is a tree HasTrailingBlankNodes reports true for: the decoder would have answered with
// a tree the rule it had just applied forbids.
//
// The refusal is ErrTreeMalformed rather than the ValSem300 one, because a node array of width
// zero is not 2n-1 for any n, which is the same reason NewRatchetTree's floor is one leaf and
// not nothing.
func TestRatchetTreeRefusesAnEmptyNodeArray(t *testing.T) {
	// one zero length prefix octet: a legal syntax encoding of a ratchet_tree carrying no node
	empty := syntax.NewWriter()
	empty.WriteOpaque(nil)
	encoded, err := empty.Bytes()
	if err != nil {
		t.Fatalf("build the empty array: %v", err)
	}
	if !bytes.Equal(encoded, []byte{0x00}) {
		t.Fatalf("the empty ratchet_tree is %x, want a single zero length prefix", encoded)
	}
	out, err := UnmarshalRatchetTree(encoded)
	if !errors.Is(err, ErrTreeMalformed) {
		t.Fatalf("the empty array answered (%v, %v), want ErrTreeMalformed", out, err)
	}
	// and what the unchecked reading would have handed back is a tree that fails the rule
	if !NewRatchetTree().HasTrailingBlankNodes() {
		t.Fatalf("the one leaf blank tree does not report a trailing blank, so the case above is not the one this test is about")
	}
	// the encode half of the same fact: a tree with no non-blank node is refused rather than
	// written as the empty array, so this implementation never sends what it will not accept
	if _, err := marshalRatchetTree(NewRatchetTree()); !errors.Is(err, ErrTreeMalformed) {
		t.Errorf("encoding a wholly blank tree answered %v, want ErrTreeMalformed", err)
	}
}

func TestRatchetTreeRejectsNodeTypeInWrongPosition(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 2)
	// put a parent node at node index 0, which is a leaf position.
	tree.nodes[0] = &Node{NodeType: NodeTypeParent, Parent: &ParentNode{
		EncryptionKey: HpkePublicKey(bytes.Repeat([]byte{0xAA}, 32)),
	}}
	encoded, err := syntax.Marshal(tree)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := UnmarshalRatchetTree(encoded); !errors.Is(err, ErrNodeTypeMismatch) {
		t.Fatalf("err = %v, want ErrNodeTypeMismatch", err)
	}
	// the mirror image, which a check written for one parity alone accepts: a LeafNode at an
	// odd index. Node 1 is the parent position of a two leaf tree.
	other, _ := newTestTree(t, crypto, 2)
	other.nodes[1] = &Node{NodeType: NodeTypeLeaf, Leaf: other.Leaf(LeafIndex(0))}
	encoded, err = syntax.Marshal(other)
	if err != nil {
		t.Fatalf("Marshal a leaf at a parent index: %v", err)
	}
	if _, err := UnmarshalRatchetTree(encoded); !errors.Is(err, ErrNodeTypeMismatch) {
		t.Fatalf("a LeafNode at node index 1 answered %v, want ErrNodeTypeMismatch", err)
	}
	// and an octet naming neither arm is refused rather than defaulted to one of them
	third, _ := newTestTree(t, crypto, 2)
	third.nodes[0] = &Node{NodeType: NodeType(7), Leaf: third.Leaf(LeafIndex(0))}
	if _, err := syntax.Marshal(third); !errors.Is(err, ErrTreeMalformed) {
		t.Fatalf("encoding a node of no type answered %v, want ErrTreeMalformed", err)
	}
}

func TestRatchetTreeRejectsABadPresenceOctet(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 2)
	encoded, err := syntax.Marshal(tree)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// the first octet of the vector body is the first node's presence octet. the length prefix
	// is variable-width, so find its length by decoding rather than by assuming an offset.
	body, err := syntax.NewReader(encoded).ReadSub()
	if err != nil {
		t.Fatalf("ReadSub: %v", err)
	}
	prefixLen := len(encoded) - body.Remaining()
	if encoded[prefixLen] != 0x01 {
		t.Fatalf("the octet at %d is %#x and the first entry of this tree is present, so the offset is not the presence octet",
			prefixLen, encoded[prefixLen])
	}
	mutated := append([]byte{}, encoded...)
	mutated[prefixLen] = 0x02
	if _, err := UnmarshalRatchetTree(mutated); !errors.Is(err, syntax.ErrOptionalPresence) {
		t.Fatalf("err = %v, want ErrOptionalPresence", err)
	}
}

// TestUnmarshalRatchetTreeRefusesBytesAfterTheVector is the full consumption half, which
// syntax.UnmarshalLimit carries and a hand rolled reader would not.
//
// A ratchet_tree extension body with anything after the array is a body whose sender and
// receiver disagree about where the structure ends, and every byte of it is covered by the
// GroupInfo signature the extension travels under.
func TestUnmarshalRatchetTreeRefusesBytesAfterTheVector(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 3)
	encoded, err := syntax.Marshal(tree)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := UnmarshalRatchetTree(encoded); err != nil {
		t.Fatalf("the encoding without a tail does not decode: %v", err)
	}
	for _, tail := range [][]byte{{0x00}, {0x01}, bytes.Repeat([]byte{0xff}, 16)} {
		trailing := append(append([]byte{}, encoded...), tail...)
		if _, err := UnmarshalRatchetTree(trailing); !errors.Is(err, syntax.ErrTrailingBytes) {
			t.Errorf("a %d byte tail answered %v, want syntax.ErrTrailingBytes", len(tail), err)
		}
	}
	// and a truncated array is refused rather than read as a shorter tree
	if _, err := UnmarshalRatchetTree(encoded[:len(encoded)-1]); err == nil {
		t.Errorf("an encoding one byte short decoded without complaint")
	}
}

// TestEveryRatchetTreeExtensionThisPackageBuildsCarriesItsOwnTag is the guarantee the
// Encode/Parse pair exists for: no call site can pair a ratchet_tree body with another
// extension's code point.
//
// Extension.ExtensionData is opaque, so nothing in the type system holds that, and an
// extensions vector carrying a tree body under some other tag is a structure that encodes,
// signs and travels. Task 4's review found this guarantee claimed and not held, and the fix was
// a read side that checks the tag rather than a restated claim -- so both halves are asserted
// here: the write side produces only the right tag, and the read side refuses every other one.
//
// The refusal is swept over the WHOLE uint16 tag space rather than over the code points this
// package happens to declare. A list of seven names is the shape that understates the class: an
// extension registered later, or a private code point a peer chose, is exactly the tag nobody
// would have written down.
func TestEveryRatchetTreeExtensionThisPackageBuildsCarriesItsOwnTag(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 3)
	ext, err := tree.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if ext.ExtensionType != ExtensionTypeRatchetTree {
		t.Fatalf("Encode tagged the body 0x%04x, want ratchet_tree 0x%04x",
			uint16(ext.ExtensionType), uint16(ExtensionTypeRatchetTree))
	}
	back, err := ParseRatchetTreeFrom(ext)
	if err != nil {
		t.Fatalf("ParseRatchetTreeFrom its own Encode: %v", err)
	}
	if back.MemberCount() != 3 || back.NodeWidth() != tree.NodeWidth() {
		t.Fatalf("the extension carried a %d member / %d node tree out of a 3 member / %d node one",
			back.MemberCount(), back.NodeWidth(), tree.NodeWidth())
	}

	// the whole tag space, with a body short enough that 65536 refusals cost nothing. Exactly
	// one code point may get past the tag check, and every other must be refused BEFORE the
	// body is looked at, which is what the sentinel separates.
	probe := []byte{0x01, 0x01}
	past := []ExtensionType{}
	for tag := 0; tag < 1<<16; tag += 1 {
		_, err := ParseRatchetTreeFrom(Extension{
			ExtensionType: ExtensionType(tag),
			ExtensionData: probe,
		})
		if err == nil {
			t.Fatalf("tag 0x%04x parsed %x as a ratchet tree", tag, probe)
		}
		if !errors.Is(err, ErrRatchetTreeExtensionTag) {
			past = append(past, ExtensionType(tag))
		}
	}
	if !slices.Equal(past, []ExtensionType{ExtensionTypeRatchetTree}) {
		t.Fatalf("the tag check let %v past, want exactly ratchet_tree 0x%04x",
			past, uint16(ExtensionTypeRatchetTree))
	}

	// a refusal of the BODY keeps its own sentinel rather than being folded into the tag one,
	// which is what lets a caller tell "you handed me the wrong entry" from "this peer's tree
	// is not one I may adopt"
	padded, err := marshalRatchetTreeWithTrailingBlank(tree)
	if err != nil {
		t.Fatalf("marshalRatchetTreeWithTrailingBlank: %v", err)
	}
	_, err = ParseRatchetTreeFrom(Extension{
		ExtensionType: ExtensionTypeRatchetTree,
		ExtensionData: padded,
	})
	if !errors.Is(err, errTrailingBlankNodes) {
		t.Errorf("a padded body under the right tag answered %v, want the ValSem300 refusal", err)
	}
	if errors.Is(err, ErrRatchetTreeExtensionTag) {
		t.Errorf("a body refusal answers to the tag sentinel, so the two conditions are no longer distinguishable")
	}

	// and the entry FindExtension hands a caller out of a real extensions vector is the one
	// this pair accepts, which is the shape the group lifecycle plan uses
	body, found, err := FindExtension([]Extension{ext}, ExtensionTypeRatchetTree)
	if err != nil || !found {
		t.Fatalf("FindExtension did not find the entry Encode built: %v", err)
	}
	if !bytes.Equal(body, ext.ExtensionData) {
		t.Fatalf("FindExtension answered a different body")
	}
}

// TestValSem300sSentinelIsStillCarriedByThisPackage is the swap gate psk.go's header describes,
// written for ValSem300.
//
// The validation plan owns the single declaration site for ErrTrailingBlankNodes and for
// ValSem itself; until that plan lands, tree.go carries the refusal as the unexported
// errTrailingBlankNodes. The moment the exported name arrives, this fails and names what is
// owed: wrap the detail in ValSem(ValSem300, ...) with the catalogue's sentinel and delete the
// unexported one.
//
// A scan that read nothing would report every name as still pending and pass, so the positive
// and negative controls below are what separate "not landed" from "not looked".
func TestValSem300sSentinelIsStillCarriedByThisPackage(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	if _, ok := declared["ErrTreeMalformed"]; !ok {
		t.Fatal("the scan did not find ErrTreeMalformed, which this package certainly declares, so it is reporting every name below as pending having read nothing")
	}
	if _, ok := declared["ThisSymbolDoesNotExistAnywhereInPackageMls"]; ok {
		t.Fatal("the scan reports a symbol that cannot exist, so it is matching text rather than declarations")
	}
	for _, name := range []string{"ErrTrailingBlankNodes", "ValSem", "ValSem300"} {
		if file, ok := declared[name]; ok {
			t.Errorf("%s has landed in %s, so the unexported errTrailingBlankNodes in tree.go is now a second declaration site for one refusal; wrap the detail in ValSem(ValSem300, ...) and delete it",
				name, file)
		}
	}
	if errTrailingBlankNodes == nil || !strings.HasPrefix(errTrailingBlankNodes.Error(), "mls: ") {
		t.Fatalf("errTrailingBlankNodes reads %v; every typed error of this package names the package it came from", errTrailingBlankNodes)
	}
	// and it is its own condition rather than an alias of one of the tree's structural refusals
	for name, other := range map[string]error{
		"ErrTreeMalformed":    ErrTreeMalformed,
		"ErrNodeTypeMismatch": ErrNodeTypeMismatch,
	} {
		if errors.Is(errTrailingBlankNodes, other) || errors.Is(other, errTrailingBlankNodes) {
			t.Errorf("the ValSem300 refusal and %s answer for each other, so a caller branching on the pair reads one as the other", name)
		}
	}
}

// ---------------------------------------------------------------------------
// what a hostile ratchet_tree body costs to refuse, and what a legal one costs to accept
// ---------------------------------------------------------------------------

// ratchetTreeBodyOf builds a ratchet_tree extension body by hand: the RFC 9420 section 2.1.2
// varint length prefix, then absent entries, then whatever tail is handed in.
//
// By hand and not through MarshalMLS, because every shape below is one this package's own
// encoder refuses to produce -- an array of nothing but blanks, an array whose only node is a
// parent -- and those are exactly the shapes a peer puts on the wire. A fixture that can only
// be built by the encoder under test measures the encoder rather than the decoder.
func ratchetTreeBodyOf(t testing.TB, absent int, tail []byte) []byte {
	t.Helper()
	w := syntax.NewWriterLimit(syntax.MaxRatchetTreeLength)
	w.WriteVarint(uint32(absent + len(tail)))
	prefix, err := w.Bytes()
	if err != nil {
		t.Fatalf("the length prefix for %d entries: %v", absent+len(tail), err)
	}
	body := make([]byte, 0, len(prefix)+absent+len(tail))
	body = append(body, prefix...)
	body = append(body, make([]byte, absent)...)
	return append(body, tail...)
}

// oneParentNodeEntry is a single PRESENT optional<Node> carrying the smallest ParentNode this
// codec writes, which is what turns an all blank array into a legal one: ValSem300 asks that
// the LAST entry be non-blank and asks nothing else of the rest.
func oneParentNodeEntry(t testing.TB) []byte {
	t.Helper()
	w := syntax.NewWriterLimit(syntax.MaxRatchetTreeLength)
	if err := writeOneOptionalNode(w, &Node{
		NodeType: NodeTypeParent,
		Parent:   &ParentNode{EncryptionKey: HpkePublicKey{0x01}, ParentHash: []byte{0x02}},
	}); err != nil {
		t.Fatalf("encoding one present parent node: %v", err)
	}
	entry, err := w.Bytes()
	if err != nil {
		t.Fatalf("one present parent node: %v", err)
	}
	return entry
}

// bytesAllocatedBy is everything f asked the allocator for while it ran.
//
// TotalAlloc and not HeapAlloc, which is the half worth reading twice: what an attacker makes
// the process pay is every byte handed out, including the buffers a doubling slice abandoned
// on the way and which a later collection would have reclaimed. A peak-heap measurement reads
// a decoder that allocated a gigabyte in eight steps as though it had allocated the last one.
func bytesAllocatedBy(t testing.TB, f func()) uint64 {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestARefusedRatchetTreeBodyIsNotFirstMaterialised is the decode side of the amplification
// bound, over the shape that costs the most to refuse: an array of nothing but blanks, at the
// full sixteen mebibyte bound p1 raised for this structure and no other.
//
// ValSem300 refuses it -- the last node of an exported ratchet_tree must be non-blank and every
// node of this one is blank -- and the property here is WHEN, not whether. A decoder that grew
// one array slot per entry as it read, with a heap allocated OptionalNode beside each, reached
// that refusal having already asked the allocator for 827 MB against a 16 MiB input: 49 times
// the bytes that arrived, on a path that runs before anything has authenticated the sender,
// since a ratchet_tree extension travels in a Welcome and in a GroupInfo.
//
// The bound is stated against the BODY rather than as a byte count, because that is the
// statement that survives the limit being changed: refusing a body must not cost more than the
// body itself is long. What the decoder actually spends is the entry count and an empty slice,
// measured at 208 bytes for every length below.
func TestARefusedRatchetTreeBodyIsNotFirstMaterialised(t *testing.T) {
	for _, entries := range []int{1 << 20, syntax.MaxRatchetTreeLength - 4} {
		body := ratchetTreeBodyOf(t, entries, nil)
		var err error
		allocated := bytesAllocatedBy(t, func() {
			_, err = UnmarshalRatchetTree(body)
		})
		if !errors.Is(err, errTrailingBlankNodes) {
			t.Fatalf("an all blank array of %d entries answered %v, want the ValSem300 refusal; the bound below is about the refusal path and nothing else",
				entries, err)
		}
		t.Logf("refusing %d blank entries (%d wire bytes) allocated %d bytes", entries, len(body), allocated)
		if allocated >= uint64(len(body)) {
			t.Errorf("refusing a %d byte all blank ratchet_tree allocated %d bytes, want less than the body itself; a refusal that costs more than its input is an amplifier a Welcome hands to an unauthenticated sender",
				len(body), allocated)
		}
	}
}

// TestAnAcceptedRatchetTreeCostsOnePointerPerNodeOfTheTreeItDescribes is the other half of the
// same bound, over the array a hostile body can make this decoder ACCEPT.
//
// The shape is legal and the RFC says to take it: absent entries up to an odd index, then one
// real ParentNode, which ValSem300 accepts because the last entry is non-blank and which
// section 12.4.3.3 then extends to the enclosing complete tree. So the tree really is that
// wide, and holding it really does cost one pointer per slot. What is NOT owed is a multiple
// of that -- the earlier decoder spent 961 MB to build a 134 MB array, because it grew the
// array by doubling and allocated an OptionalNode per entry on the way.
//
// The floor is measured rather than written down, by allocating the same array this tree IS.
// A stated byte count would be a second copy of unsafe.Sizeof that goes stale on a target with
// a different pointer width, and the comparison is a ratio in any case.
func TestAnAcceptedRatchetTreeCostsOnePointerPerNodeOfTheTreeItDescribes(t *testing.T) {
	entry := oneParentNodeEntry(t)
	// the present node has to land on an ODD index: a ParentNode at an even one is
	// ErrNodeTypeMismatch, which is a different path from the one being measured
	absent := (4 << 20) - len(entry)
	if absent%2 == 0 {
		absent -= 1
	}
	body := ratchetTreeBodyOf(t, absent, entry)
	var tree *RatchetTree
	var err error
	allocated := bytesAllocatedBy(t, func() {
		tree, err = UnmarshalRatchetTree(body)
	})
	if err != nil {
		t.Fatalf("a legal truncated array of %d entries: %v; RFC 9420 section 12.4.3.3 accepts it and extends it",
			absent+1, err)
	}
	var array []*Node
	floor := bytesAllocatedBy(t, func() {
		array = make([]*Node, tree.NodeWidth())
	})
	if len(array) != int(tree.NodeWidth()) {
		t.Fatalf("the floor measurement built %d slots for a %d node tree", len(array), tree.NodeWidth())
	}
	t.Logf("accepting %d wire bytes built a %d node tree and allocated %d bytes; the array alone is %d",
		len(body), tree.NodeWidth(), allocated, floor)
	if allocated > floor+floor/2 {
		t.Errorf("decoding a %d byte ratchet_tree into a %d node tree allocated %d bytes, want no more than half again the %d the array itself costs; the excess is the decoder's own bookkeeping multiplying an attacker's input",
			len(body), tree.NodeWidth(), allocated, floor)
	}
}

// TestARefusedRatchetTreeLatchesTheReaderItWasReadFrom is the obligation the hand rolled
// ReadSub-and-Done form did not discharge.
//
// ReadSub advances the CALLER's Reader past the whole region before the element decode begins,
// so a refusal inside the region is invisible to it: the caller is left holding a Reader
// positioned at the next field of the enclosing structure, reporting nil from Done, having
// skipped a ratchet tree nothing accepted. A caller that dropped this error -- and Done is
// exactly the check a caller uses instead of checking every return -- would go on to decode the
// rest of a GroupInfo as though the tree in it had been read. ReadNested latches, so it cannot.
func TestARefusedRatchetTreeLatchesTheReaderItWasReadFrom(t *testing.T) {
	body := ratchetTreeBodyOf(t, 4, nil)
	r := syntax.NewReaderLimit(body, syntax.MaxRatchetTreeLength)
	tree := &RatchetTree{}
	if err := tree.UnmarshalMLS(r); !errors.Is(err, errTrailingBlankNodes) {
		t.Fatalf("an all blank array answered %v, want the ValSem300 refusal", err)
	}
	if err := r.Done(); !errors.Is(err, errTrailingBlankNodes) {
		t.Errorf("the Reader the refused tree was read from reports %v from Done, want the refusal latched onto it; without the latch a caller that checks only Done is told the region it skipped was fine",
			err)
	}
}

// TestTheExtensionToACompleteTreeMovesNoNodeAndDropsNone pins where the nodes of a truncated
// array land after RFC 9420 section 12.4.3.3's extension.
//
// It is here because the extension used to be a make-and-copy, and copy's answer to a
// destination shorter than its source is to drop the tail SILENTLY: the array would be
// accepted short, LeafCountFromNodeWidth and IsFullLeafCount would both pass on what was left,
// and the tree would be a complete tree of the wrong width with a member missing. The widths
// below straddle a power of two in both directions, so a scatter that put a node anywhere but
// at the index it arrived at fails here rather than in a tree hash three tasks later.
func TestTheExtensionToACompleteTreeMovesNoNodeAndDropsNone(t *testing.T) {
	entry := oneParentNodeEntry(t)
	// odd indices only: a ParentNode at an even one is refused by position
	for _, at := range []int{1, 3, 5, 7, 9, 15, 17, 31, 33, 63, 65} {
		body := ratchetTreeBodyOf(t, at, entry)
		tree, err := UnmarshalRatchetTree(body)
		if err != nil {
			t.Fatalf("a %d entry array whose last node sits at index %d: %v", at+1, at, err)
		}
		occupied := []NodeIndex{}
		for x := uint32(0); x < tree.NodeWidth(); x += 1 {
			if !tree.IsBlank(NodeIndex(x)) {
				occupied = append(occupied, NodeIndex(x))
			}
		}
		if !slices.Equal(occupied, []NodeIndex{NodeIndex(at)}) {
			t.Errorf("an array carrying one node at index %d decoded to a %d node tree occupied at %v, want exactly [%d]",
				at, tree.NodeWidth(), occupied, at)
		}
		if uint32(at) >= tree.NodeWidth() {
			t.Errorf("the node at index %d is outside the %d node tree it decoded to", at, tree.NodeWidth())
		}
	}
}

// TestTheRatchetTreeExtensionNeedsTheRaisedLimitAroundItToo is the caller obligation Encode's
// own doc comment states, measured rather than left as prose.
//
// Encode answers an Extension whose body is written at MaxRatchetTreeLength, and that is as far
// as this package's decision reaches: Extension.MarshalMLS writes ExtensionData through
// whichever Writer the caller opened, and WriteExtensions and ReadExtensions do the same. So at
// this product's own group size -- 500 members, two devices each, a 1216 byte X-Wing key per
// leaf -- the entry Encode produces cannot be put into an extensions<V> vector by any caller
// running the default limit, and the failure is syntax.ErrLengthExceedsMax, which reads as a
// corrupt structure rather than as a limit.
//
// Both directions are asserted separately, because a caller can get either half wrong on its
// own: a GroupInfo written at the raised bound and read back at the default one refuses a tree
// it just wrote.
func TestTheRatchetTreeExtensionNeedsTheRaisedLimitAroundItToo(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, productGroupLeafCount)
	ext, err := tree.Encode()
	if err != nil {
		t.Fatalf("Encode at %d leaves: %v", productGroupLeafCount, err)
	}
	if len(ext.ExtensionData) <= syntax.MaxVectorLength {
		t.Fatalf("the extension body is %d bytes, which the default limit of %d accepts, so nothing below can tell the two limits apart",
			len(ext.ExtensionData), syntax.MaxVectorLength)
	}

	// the entry on its own, and the vector around it, both refused at the default limit
	if _, err := syntax.Marshal(&ext); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("syntax.Marshal of the ratchet_tree extension answered %v, want syntax.ErrLengthExceedsMax", err)
	}
	if err := WriteExtensions(syntax.NewWriter(), []Extension{ext}); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("WriteExtensions at the default limit answered %v, want syntax.ErrLengthExceedsMax; a caller that writes a GroupInfo through an ordinary Writer cannot carry this product's own tree",
			err)
	}

	// and carried by a Writer opened at the bound the body was encoded under
	w := syntax.NewWriterLimit(syntax.MaxRatchetTreeLength)
	if err := WriteExtensions(w, []Extension{ext}); err != nil {
		t.Fatalf("WriteExtensions at MaxRatchetTreeLength: %v", err)
	}
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("the extensions vector: %v", err)
	}
	t.Logf("a %d leaf tree is a %d byte extension body and a %d byte extensions vector; MaxVectorLength is %d",
		productGroupLeafCount, len(ext.ExtensionData), len(encoded), syntax.MaxVectorLength)

	// the read side fails on its own, at the default limit, over bytes this package just wrote
	if _, err := ReadExtensions(syntax.NewReader(encoded)); !errors.Is(err, syntax.ErrLengthExceedsMax) {
		t.Errorf("ReadExtensions at the default limit answered %v over an extensions vector this package wrote, want syntax.ErrLengthExceedsMax",
			err)
	}
	back, err := ReadExtensions(syntax.NewReaderLimit(encoded, syntax.MaxRatchetTreeLength))
	if err != nil {
		t.Fatalf("ReadExtensions at MaxRatchetTreeLength: %v", err)
	}
	if len(back) != 1 || back[0].ExtensionType != ExtensionTypeRatchetTree {
		t.Fatalf("read back %d entries, first tagged 0x%04x, want one ratchet_tree entry",
			len(back), uint16(back[0].ExtensionType))
	}
	out, err := ParseRatchetTreeFrom(back[0])
	if err != nil {
		t.Fatalf("ParseRatchetTreeFrom the entry that survived the round trip: %v", err)
	}
	if out.MemberCount() != productGroupLeafCount {
		t.Errorf("the tree that came back out of the extensions vector has %d members, want %d",
			out.MemberCount(), productGroupLeafCount)
	}
}

// ---------------------------------------------------------------------------
// RFC 9420 section 7.7: Add, Update and Remove on the tree
// ---------------------------------------------------------------------------

// treeWithOccupiedLeaves is the cheap fixture the shape questions below sweep with: a complete
// tree of the given leaf width, a distinguishable leaf wherever the occupancy says so, a
// distinguishable parent node at EVERY odd index, and no cryptography anywhere.
//
// Every parent occupied is the half that makes a blanking observable at all. On a tree whose
// parents are already blank -- which is what newTestTree hands back, and what three of the four
// tests the plan supplied for this task ran against -- an operation that blanks the whole direct
// path and one that blanks nothing above the leaf leave exactly the same tree, and no assertion
// about a blank can tell them apart. The fixture asserts its own occupancy below for that reason.
//
// The leaves are filled and then blanked back rather than skipped, because a pattern with a blank
// on the right would otherwise never reach the width it was asked for: the array grows to hold
// the widest leaf INSTALLED, so leaving leaf 7 out of an eight leaf tree builds a four leaf one.
func treeWithOccupiedLeaves(t *testing.T, width uint32, occupied func(LeafIndex) bool) *RatchetTree {
	t.Helper()
	tree := NewRatchetTree()
	for i := uint32(0); i < width; i += 1 {
		if err := tree.SetLeaf(LeafIndex(i), testTreeLeaf(i)); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
	}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if err := tree.SetParent(NodeIndex(x), &ParentNode{
			EncryptionKey: HpkePublicKey(repeatByte(byte(0xc0+x), 32)),
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	for i := uint32(0); i < width; i += 1 {
		if occupied(LeafIndex(i)) {
			continue
		}
		if err := tree.Blank(LeafIndex(i).NodeIndex()); err != nil {
			t.Fatalf("Blank(leaf %d): %v", i, err)
		}
	}
	// the fixture's own claims, asked rather than assumed, because everything below is a
	// statement about a change to this shape.
	if tree.LeafWidth() != LeafCount(width) {
		t.Fatalf("the fixture is %d leaves wide and was asked for %d", tree.LeafWidth(), width)
	}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if tree.IsBlank(NodeIndex(x)) {
			t.Fatalf("node %d is blank in a fixture that fills every parent, so blanking it is not observable", x)
		}
	}
	for i := uint32(0); i < width; i += 1 {
		if occupied(LeafIndex(i)) != (tree.Leaf(LeafIndex(i)) != nil) {
			t.Fatalf("leaf %d does not match the occupancy the fixture was asked for", i)
		}
	}
	return tree
}

// assertStillATree holds every structural rule this package already owns over whatever an
// operation left behind.
//
// The cheapest strong property available here, and the reason it is worth its length: an
// operation that leaves a MALFORMED tree does not fail at the operation. It fails at a tree hash,
// a resolution or a join three tasks later, against code that has nothing to do with the loop
// that was wrong. So each rule below is read out of something that is not this file -- the tree
// math's own width predicates, the container's own type-per-position rule, the codec's own
// unmerged ordering check, the free Resolution's own refusal boundary -- and none of them is
// restated here in a form that could drift from what the package actually enforces.
func assertStillATree(t *testing.T, where string, tree *RatchetTree) {
	t.Helper()
	if !IsFullLeafCount(tree.LeafWidth()) {
		t.Fatalf("%s: the leaf width is %d, which is not a width any tree has", where, tree.LeafWidth())
	}
	if NodeWidth(tree.LeafWidth()) != tree.NodeWidth() {
		t.Fatalf("%s: %d nodes over %d leaves, want %d",
			where, tree.NodeWidth(), tree.LeafWidth(), NodeWidth(tree.LeafWidth()))
	}
	if _, err := rootOf(tree.LeafWidth()); err != nil {
		t.Fatalf("%s: the tree has no root: %v", where, err)
	}
	occupied := 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := tree.Get(NodeIndex(x))
		if node == nil {
			continue
		}
		occupied += 1
		if NodeIndex(x).IsLeaf() != (node.NodeType == NodeTypeLeaf) {
			t.Fatalf("%s: node %d carries node type %d, which is not the one its index requires", where, x, node.NodeType)
		}
	}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		parent := tree.ParentAt(NodeIndex(x))
		if parent == nil {
			continue
		}
		// the codec's own rule and not a second reading of it: an unsorted vector is a tree only
		// this implementation hashes.
		if err := checkUnmergedLeavesSorted(parent.UnmergedLeaves); err != nil {
			t.Fatalf("%s: node %d unmerged = %v: %v", where, x, parent.UnmergedLeaves, err)
		}
		first, last := SubtreeLeaves(NodeIndex(x))
		for _, leaf := range parent.UnmergedLeaves {
			if LeafCount(leaf) >= tree.LeafWidth() {
				t.Fatalf("%s: node %d lists leaf %d unmerged over a %d leaf tree, and SetParent would refuse that node",
					where, x, leaf, tree.LeafWidth())
			}
			if leaf < first || leaf > last {
				t.Fatalf("%s: node %d lists leaf %d unmerged and covers only leaves %d..%d", where, x, leaf, first, last)
			}
			if tree.Leaf(leaf) == nil {
				t.Fatalf("%s: node %d lists blank leaf %d unmerged", where, x, leaf)
			}
		}
	}
	assertResolutionRefusesOnlyPastTheNodeWidth(t, tree, where)
	if occupied == 0 {
		// a tree with nothing in it anywhere has no encoding at all: section 12.4.3.3's array may
		// not end in a blank and every entry of this one is. That is the state the container calls
		// the one leaf tree and the codec calls malformed, and both are right.
		return
	}
	encoded, err := marshalRatchetTree(tree)
	if err != nil {
		t.Fatalf("%s: the tree no longer encodes: %v", where, err)
	}
	back, err := UnmarshalRatchetTree(encoded)
	if err != nil {
		t.Fatalf("%s: the tree no longer decodes: %v", where, err)
	}
	again, err := marshalRatchetTree(back)
	if err != nil {
		t.Fatalf("%s: the decoded tree does not re-encode: %v", where, err)
	}
	if !bytes.Equal(encoded, again) {
		t.Fatalf("%s: the tree does not round trip through its own codec", where)
	}
}

// TestAddLeafFillsTheLeftmostBlankAndMarksUnmerged is the plan's test for this clause with the
// two things it could not observe put back.
//
// The plan's version blanked ONE leaf and then asserted Add filled it. With a single blank in the
// tree the leftmost blank, the rightmost blank and any blank at all are the same index, so no
// version of Add can fail that assertion -- the first mutation this task names is invisible to
// it. And it ran on a four leaf tree, whose direct path is two nodes long, so there is no node on
// it that is neither the first element nor the last: the interior of the marking loop is
// unobserved. Eight leaves and two blanks fix both.
func TestAddLeafFillsTheLeftmostBlankAndMarksUnmerged(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 8)
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if err := tree.SetParent(NodeIndex(x), &ParentNode{
			EncryptionKey: HpkePublicKey(repeatByte(byte(0x60+x), 32)),
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	for _, blank := range []LeafIndex{2, 5} {
		if err := tree.Blank(blank.NodeIndex()); err != nil {
			t.Fatalf("Blank(leaf %d): %v", blank, err)
		}
	}
	newLeaf := tree.Leaf(LeafIndex(0)).Clone()
	got, err := tree.AddLeaf(newLeaf)
	if err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	if got != LeafIndex(2) {
		t.Fatalf("AddLeaf = %d, want the leftmost blank leaf 2 and not the rightmost blank leaf 5", got)
	}
	path, err := directPathOf(got.NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf(%d): %v", got.NodeIndex(), err)
	}
	if len(path) < 3 {
		t.Fatalf("leaf %d's direct path is %v, and a path with no interior node observes no interior clause", got, path)
	}
	onPath := map[NodeIndex]bool{}
	for _, x := range path {
		onPath[x] = true
		parent := tree.ParentAt(x)
		if parent == nil {
			t.Fatalf("AddLeaf blanked node %d; Add must never blank", x)
		}
		if len(parent.UnmergedLeaves) != 1 || parent.UnmergedLeaves[0] != got {
			t.Fatalf("node %d of the direct path %v has unmerged = %v, want [%d]", x, path, parent.UnmergedLeaves, got)
		}
	}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if onPath[NodeIndex(x)] {
			continue
		}
		parent := tree.ParentAt(NodeIndex(x))
		if parent == nil {
			t.Fatalf("AddLeaf blanked node %d, which is not even on the new leaf's path", x)
		}
		if len(parent.UnmergedLeaves) != 0 {
			t.Fatalf("node %d is off leaf %d's direct path %v and must not be marked unmerged, and carries %v",
				x, got, path, parent.UnmergedLeaves)
		}
	}
	// the second blank next, because Add refills every gap before it widens anything.
	second, err := tree.AddLeaf(newLeaf.Clone())
	if err != nil {
		t.Fatalf("AddLeaf into the second gap: %v", err)
	}
	if second != LeafIndex(5) {
		t.Fatalf("AddLeaf = %d, want the remaining blank leaf 5", second)
	}
	if tree.LeafWidth() != 8 {
		t.Fatalf("leaf width = %d: Add widened a tree that still had a blank leaf", tree.LeafWidth())
	}
	// and with no blank left, Add grows the tree.
	if _, err := tree.AddLeaf(newLeaf.Clone()); err != nil {
		t.Fatalf("AddLeaf into a full tree: %v", err)
	}
	if tree.LeafWidth() != 16 {
		t.Fatalf("leaf width = %d, want 16", tree.LeafWidth())
	}
	assertStillATree(t, "after three adds", tree)
}

// TestAddLeafPicksTheLeftmostBlankOfEveryBlankSetATreeCanHold says "leftmost" over the whole
// family of blank sets rather than over one arrangement.
//
// Which is the point: leftmost, rightmost, lowest-numbered-scanning-down and "the first one the
// loop happens to see" all agree on a tree with one blank in it, and the family is what separates
// them. The expected answer is derived from the pattern -- and the rightmost blank derived beside
// it, with a count that refuses to let this sweep run without a case where the two differ.
func TestAddLeafPicksTheLeftmostBlankOfEveryBlankSetATreeCanHold(t *testing.T) {
	separating := 0
	for _, width := range []uint32{2, 4, 8} {
		for pattern := uint32(0); pattern < (uint32(1) << width); pattern += 1 {
			isBlank := func(i LeafIndex) bool { return pattern&(uint32(1)<<uint32(i)) != 0 }
			// no blank at all means the tree grows and the new leaf lands one past the last, which
			// is what LeafIndex(width) says here.
			leftmost, rightmost := LeafIndex(width), LeafIndex(width)
			for i := uint32(0); i < width; i += 1 {
				if !isBlank(LeafIndex(i)) {
					continue
				}
				if leftmost == LeafIndex(width) {
					leftmost = LeafIndex(i)
				}
				rightmost = LeafIndex(i)
			}
			if leftmost != rightmost {
				separating += 1
			}
			tree := treeWithOccupiedLeaves(t, width, func(i LeafIndex) bool { return !isBlank(i) })
			got, err := tree.AddLeaf(testTreeLeaf(0x20))
			if err != nil {
				t.Fatalf("width %d blanks %b: AddLeaf: %v", width, pattern, err)
			}
			if got != leftmost {
				t.Fatalf("width %d blanks %b: AddLeaf = %d, want the leftmost blank %d (the rightmost blank is %d)",
					width, pattern, got, leftmost, rightmost)
			}
			if tree.Leaf(got) == nil {
				t.Fatalf("width %d blanks %b: AddLeaf answered %d and put no leaf there", width, pattern, got)
			}
		}
	}
	if separating == 0 {
		t.Fatalf("no pattern in this sweep holds two blanks, so nothing here separates the leftmost blank from the rightmost")
	}
}

// TestAddLeafKeepsUnmergedLeavesAscendingSoTheTreeStillEncodes is the clause an append gets wrong.
//
// Section 7.9.2 requires unmerged_leaves strictly ascending and this package's own encoder refuses
// anything else, so the consequence of an append is not a cosmetic difference: it is a tree this
// implementation cannot publish and, if it could, one whose parent hashes no peer reproduces. The
// arrangement that produces it is Add's main line rather than a corner of it -- a blank leaf to
// the LEFT of a leaf a node already lists is exactly what a Remove followed by an Add leaves.
func TestAddLeafKeepsUnmergedLeavesAscendingSoTheTreeStillEncodes(t *testing.T) {
	tree := treeWithOccupiedLeaves(t, 8, func(i LeafIndex) bool { return i != LeafIndex(2) })
	path, err := directPathOf(LeafIndex(2).NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	// one entry per path node, each naming an occupied leaf under that node that sits to the RIGHT
	// of the blank, derived from the node's own subtree rather than picked off a drawing.
	already := map[NodeIndex]LeafIndex{}
	for _, x := range path {
		_, last := SubtreeLeaves(x)
		if last <= LeafIndex(2) {
			t.Fatalf("node %d covers up to leaf %d, so nothing under it sits right of the blank", x, last)
		}
		already[x] = last
		if err := tree.SetParent(x, &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(byte(0x30+x), 32)),
			UnmergedLeaves: []LeafIndex{last},
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	got, err := tree.AddLeaf(testTreeLeaf(0x21))
	if err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	if got != LeafIndex(2) {
		t.Fatalf("AddLeaf = %d, want 2", got)
	}
	for _, x := range path {
		unmerged := tree.UnmergedLeaves(x)
		if err := checkUnmergedLeavesSorted(unmerged); err != nil {
			t.Fatalf("node %d unmerged = %v after adding leaf %d beside %d: %v",
				x, unmerged, got, already[x], err)
		}
		if !slices.Equal(unmerged, []LeafIndex{got, already[x]}) {
			t.Fatalf("node %d unmerged = %v, want %v", x, unmerged, []LeafIndex{got, already[x]})
		}
	}
	// the consequence, and the reason the ordering is not a matter of taste.
	if _, err := marshalRatchetTree(tree); err != nil {
		t.Fatalf("the tree AddLeaf produced no longer encodes: %v", err)
	}
	assertStillATree(t, "after an add to the left of an unmerged leaf", tree)
	// and a leaf already listed is not listed twice.
	if err := tree.RemoveLeaf(got); err != nil {
		t.Fatalf("RemoveLeaf: %v", err)
	}
	surviving := path[len(path)-1]
	if err := tree.SetParent(surviving, &ParentNode{
		EncryptionKey:  HpkePublicKey(repeatByte(0x39, 32)),
		UnmergedLeaves: []LeafIndex{LeafIndex(2), already[surviving]},
	}); err != nil {
		t.Fatalf("SetParent(%d): %v", surviving, err)
	}
	if _, err := tree.AddLeaf(testTreeLeaf(0x23)); err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	if unmerged := tree.UnmergedLeaves(surviving); !slices.Equal(unmerged, []LeafIndex{LeafIndex(2), already[surviving]}) {
		t.Fatalf("node %d unmerged = %v after re-adding a leaf it already lists, want %v",
			surviving, unmerged, []LeafIndex{LeafIndex(2), already[surviving]})
	}
}

// TestAddLeafIntoACommittedTreeLeavesEveryParentHashValid is the strongest single statement
// available about the marking loop, and it is not an assertion about unmerged_leaves at all.
//
// Section 7.9.1 hashes the ORIGINAL tree hash of the sibling subtree -- the tree hash taken with
// the parent's unmerged leaves blanked out -- and section 7.9.2 condition 3 compares the
// resolution of the other child against the same list. Between them, a parent that was not marked
// is a parent that no descendant can claim. So this asks nothing about the list and everything
// about the tree: a marking loop that skips the unmerged update, that runs only its first element,
// only its last, or only its interior, each produces a tree VerifyParentHashes refuses -- at a
// different node in each case, none of which is where the loop is.
//
// The shape is chosen so that every level is load bearing. Five members in an eight leaf tree
// committed from leaf 4 gives a chain of [9, 11, 7], and the leftmost blank leaf is 5, whose
// direct path is the same three nodes. Three levels is the shortest path with an interior.
func TestAddLeafIntoACommittedTreeLeavesEveryParentHashValid(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _, chain := chainedTestTree(t, crypto, 5, LeafIndex(4))
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("the fixture does not verify before the add, so nothing below is about the add: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("the chain is %v, and a chain with no interior node observes no interior clause", chain)
	}
	joiner := tree.Leaf(LeafIndex(0)).Clone()
	added, err := tree.AddLeaf(joiner)
	if err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	if added != LeafIndex(5) {
		t.Fatalf("AddLeaf = %d, want the leftmost blank leaf 5", added)
	}
	path, err := directPathOf(added.NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	if !slices.Equal(path, chain) {
		t.Fatalf("the new leaf's direct path is %v and the committed chain is %v; this fixture only says what it claims when they are the same nodes",
			path, chain)
	}
	// the marking read straight off the tree first, so a failure below can be told from a failure
	// in the parent hash machinery next door.
	for _, x := range chain {
		if !slices.Contains(tree.UnmergedLeaves(x), added) {
			t.Fatalf("node %d of the chain %v does not list the new leaf %d unmerged", x, chain, added)
		}
	}
	if err := tree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("VerifyParentHashes after the add: %v", err)
	}
	assertStillATree(t, "after an add into a committed tree", tree)
}

// TestUpdateLeafBlanksTheDirectPath is the plan's test on a tree deep enough for its own claim:
// a four leaf tree has a two node direct path, so "blanks the direct path" there is two
// assertions about the two ends of a loop and says nothing about anything between them.
func TestUpdateLeafBlanksTheDirectPath(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 8)
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if err := tree.SetParent(NodeIndex(x), &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(byte(0x60+x), 32)),
			UnmergedLeaves: []LeafIndex{2},
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	replacement := tree.Leaf(LeafIndex(0)).Clone()
	replacement.EncryptionKey = HpkePublicKey(repeatByte(0xAB, 32))
	if err := tree.UpdateLeaf(LeafIndex(0), replacement); err != nil {
		t.Fatalf("UpdateLeaf: %v", err)
	}
	if !bytes.Equal(tree.Leaf(LeafIndex(0)).EncryptionKey, replacement.EncryptionKey) {
		t.Fatalf("UpdateLeaf did not install the replacement")
	}
	path, err := directPathOf(LeafIndex(0).NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	if len(path) < 3 {
		t.Fatalf("the direct path is %v and has no interior node", path)
	}
	onPath := map[NodeIndex]bool{}
	for _, x := range path {
		onPath[x] = true
		if tree.ParentAt(x) != nil {
			t.Fatalf("node %d of the direct path %v survived UpdateLeaf", x, path)
		}
	}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if onPath[NodeIndex(x)] {
			continue
		}
		if tree.ParentAt(NodeIndex(x)) == nil {
			t.Fatalf("node %d is off leaf 0's direct path %v and must survive", x, path)
		}
	}
	// the two refusals, which is what keeps Update from being a way to install a member.
	if err := tree.UpdateLeaf(LeafIndex(tree.LeafWidth()), replacement); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("UpdateLeaf past the width err = %v, want ErrLeafIndexOutOfRange", err)
	}
	if err := tree.Blank(LeafIndex(1).NodeIndex()); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if err := tree.UpdateLeaf(LeafIndex(1), replacement); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("UpdateLeaf at a blank leaf err = %v, want ErrLeafIndexOutOfRange", err)
	}
	if tree.Leaf(LeafIndex(1)) != nil {
		t.Fatalf("UpdateLeaf reported a refusal and installed the member anyway")
	}
}

// TestRemoveLeafBlanksAndTruncates keeps the plan's scenario and its numbers, with the clause the
// plan's version could not see put in front of them.
//
// That version ran on a fixture whose parents are all blank, so "blanks the direct path" was
// asserted by nothing: a Remove that touched only the leaf left the same tree. And it could not
// simply be asserted afterwards either, because leaf 4's whole direct path is OUTSIDE the
// truncated array, where IsBlank answers yes for an index that is merely absent. So the blanking
// is observed first, on a removal that does not truncate.
func TestRemoveLeafBlanksAndTruncates(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)

	// the blanking, on leaf 3 of a five member tree: the rightmost member is still leaf 4, so the
	// width does not move and every node of the path stays inside the array to be asked about.
	observable := treeWithOccupiedLeaves(t, 8, func(i LeafIndex) bool { return i < LeafIndex(5) })
	path, err := directPathOf(LeafIndex(3).NodeIndex(), observable.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	if len(path) < 3 {
		t.Fatalf("the direct path is %v and has no interior node", path)
	}
	if err := observable.RemoveLeaf(LeafIndex(3)); err != nil {
		t.Fatalf("RemoveLeaf(3): %v", err)
	}
	if observable.LeafWidth() != 8 {
		t.Fatalf("leaf width = %d after removing an interior leaf, want 8", observable.LeafWidth())
	}
	onPath := map[NodeIndex]bool{}
	for _, x := range path {
		onPath[x] = true
		if !observable.IsBlank(x) {
			t.Fatalf("RemoveLeaf left node %d of leaf 3's direct path %v standing", x, path)
		}
	}
	for x := uint32(1); x < observable.NodeWidth(); x += 2 {
		if onPath[NodeIndex(x)] {
			continue
		}
		if observable.IsBlank(NodeIndex(x)) {
			t.Fatalf("RemoveLeaf blanked node %d, which is not on leaf 3's direct path %v", x, path)
		}
	}

	// the plan's scenario, unchanged.
	tree, _ := newTestTree(t, crypto, 5)
	if tree.LeafWidth() != 8 {
		t.Fatalf("leaf width = %d, want 8", tree.LeafWidth())
	}
	if err := tree.RemoveLeaf(LeafIndex(4)); err != nil {
		t.Fatalf("RemoveLeaf: %v", err)
	}
	if tree.Leaf(LeafIndex(4)) != nil {
		t.Fatalf("leaf 4 is still present")
	}
	// the whole right half is blank now, so the tree halves.
	if tree.LeafWidth() != 4 {
		t.Fatalf("leaf width after remove = %d, want 4", tree.LeafWidth())
	}
	if tree.MemberCount() != 4 {
		t.Fatalf("member count = %d, want 4", tree.MemberCount())
	}
	if err := tree.RemoveLeaf(LeafIndex(4)); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("removing past the width err = %v, want ErrLeafIndexOutOfRange", err)
	}
	// removing an interior leaf leaves the width alone.
	if err := tree.RemoveLeaf(LeafIndex(1)); err != nil {
		t.Fatalf("RemoveLeaf(1): %v", err)
	}
	if tree.LeafWidth() != 4 {
		t.Fatalf("leaf width = %d, want 4 after an interior removal", tree.LeafWidth())
	}
	// and a leaf that is blank rather than absent is refused with the same sentinel.
	if err := tree.RemoveLeaf(LeafIndex(1)); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("removing a blank leaf err = %v, want ErrLeafIndexOutOfRange", err)
	}
	assertStillATree(t, "after two removals", tree)
}

// TestRemoveLeafDropsItFromUnmergedLeaves is the plan's test with the stale entries moved off the
// first parent the sweep visits.
//
// The plan's version put its one stale entry at node 1, which is the FIRST odd index, so a sweep
// that ran a single iteration and stopped passed it. The entries are spread over every off-path
// parent here, including the last one the sweep reaches, and each node also carries an entry that
// must SURVIVE -- otherwise "drops the removed leaf" and "clears the list" are the same test.
func TestRemoveLeafDropsItFromUnmergedLeaves(t *testing.T) {
	tree := treeWithOccupiedLeaves(t, 8, func(LeafIndex) bool { return true })
	path, err := directPathOf(LeafIndex(7).NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	onPath := map[NodeIndex]bool{}
	for _, x := range path {
		onPath[x] = true
	}
	// every parent that is NOT on leaf 7's direct path, derived from the path rather than listed:
	// the nodes on it are blanked by the removal and so can say nothing about a sweep.
	keep := map[NodeIndex]LeafIndex{}
	off := []NodeIndex{}
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if onPath[NodeIndex(x)] {
			continue
		}
		first, _ := SubtreeLeaves(NodeIndex(x))
		keep[NodeIndex(x)] = first
		off = append(off, NodeIndex(x))
		if err := tree.SetParent(NodeIndex(x), &ParentNode{
			EncryptionKey:  HpkePublicKey(repeatByte(byte(0x90+x), 32)),
			UnmergedLeaves: []LeafIndex{first, LeafIndex(7)},
		}); err != nil {
			t.Fatalf("SetParent(%d): %v", x, err)
		}
	}
	if len(off) < 2 {
		t.Fatalf("only %v are off the path, so nothing here separates a sweep from its first iteration", off)
	}
	if err := tree.RemoveLeaf(LeafIndex(7)); err != nil {
		t.Fatalf("RemoveLeaf: %v", err)
	}
	if tree.LeafWidth() != 8 {
		t.Fatalf("leaf width = %d: this fixture only says what it claims while every off-path parent is still in the array",
			tree.LeafWidth())
	}
	for _, x := range off {
		parent := tree.ParentAt(x)
		if parent == nil {
			t.Fatalf("node %d is off leaf 7's direct path %v and must survive", x, path)
		}
		if slices.Contains(parent.UnmergedLeaves, LeafIndex(7)) {
			t.Fatalf("removed leaf 7 is still listed unmerged on node %d: %v", x, parent.UnmergedLeaves)
		}
		if !slices.Contains(parent.UnmergedLeaves, keep[x]) {
			t.Fatalf("node %d lost leaf %d, which is still a member: %v", x, keep[x], parent.UnmergedLeaves)
		}
	}
}

// TestUpdateAndRemoveBlankEveryNodeOfTheDirectPathAndNothingElse is the property both operations
// share, said over a derived family of shapes rather than over one tree.
//
// Every leaf of every width, both operations, the path read from the tree math and the nodes off
// it read from the tree's own width. The counters at the end are what stop the sweep going quiet:
// a family in which no path has an interior, or in which nothing off a path survives, would report
// the same clean run a complete one does.
func TestUpdateAndRemoveBlankEveryNodeOfTheDirectPathAndNothingElse(t *testing.T) {
	operations := []struct {
		name  string
		apply func(t *testing.T, tree *RatchetTree, i LeafIndex)
	}{
		{"UpdateLeaf", func(t *testing.T, tree *RatchetTree, i LeafIndex) {
			t.Helper()
			if err := tree.UpdateLeaf(i, testTreeLeaf(uint32(i)+0x10)); err != nil {
				t.Fatalf("UpdateLeaf(%d): %v", i, err)
			}
		}},
		{"RemoveLeaf", func(t *testing.T, tree *RatchetTree, i LeafIndex) {
			t.Helper()
			if err := tree.RemoveLeaf(i); err != nil {
				t.Fatalf("RemoveLeaf(%d): %v", i, err)
			}
		}},
	}
	blanked, survived, interior := 0, 0, 0
	for _, width := range []uint32{2, 4, 8, 16} {
		for i := uint32(0); i < width; i += 1 {
			for _, operation := range operations {
				tree := treeWithOccupiedLeaves(t, width, func(LeafIndex) bool { return true })
				path, err := directPathOf(LeafIndex(i).NodeIndex(), tree.LeafWidth())
				if err != nil {
					t.Fatalf("width %d leaf %d: directPathOf: %v", width, i, err)
				}
				onPath := map[NodeIndex]bool{}
				for _, x := range path {
					onPath[x] = true
				}
				operation.apply(t, tree, LeafIndex(i))
				for at, x := range path {
					if !tree.IsBlank(x) {
						t.Fatalf("width %d leaf %d: %s left node %d -- element %d of a %d element direct path %v -- standing",
							width, i, operation.name, x, at, len(path), path)
					}
					blanked += 1
					if at > 0 && at < len(path)-1 {
						interior += 1
					}
				}
				// asked over the width the tree has NOW, because a truncation is not "leaving a
				// node alone" -- a node that went away with the right half was dropped.
				for x := uint32(1); x < tree.NodeWidth(); x += 2 {
					if onPath[NodeIndex(x)] {
						continue
					}
					if tree.IsBlank(NodeIndex(x)) {
						t.Fatalf("width %d leaf %d: %s blanked node %d, which is not on the direct path %v",
							width, i, operation.name, x, path)
					}
					survived += 1
				}
				assertStillATree(t, fmt.Sprintf("width %d leaf %d after %s", width, i, operation.name), tree)
			}
		}
	}
	if blanked == 0 || survived == 0 || interior == 0 {
		t.Fatalf("the sweep asserted %d blanked path nodes, %d surviving off-path nodes and %d interior path nodes, and a zero in any of the three is a sweep that observed nothing",
			blanked, survived, interior)
	}
}

// TestBlankingTheDirectPathReachesTheInteriorNodesNeitherEndOfALoopWouldTouch names the failure
// this project has shipped three times -- a loop that runs its first element and stops -- and asks
// about exactly the nodes such a loop, and its last-element-only twin, both miss.
//
// The interior is DERIVED from the path rather than written down, so the statement follows the
// shape instead of a pair of indices somebody read off one drawing of one tree, and the sweep
// refuses to run at a width whose paths have no interior at all.
func TestBlankingTheDirectPathReachesTheInteriorNodesNeitherEndOfALoopWouldTouch(t *testing.T) {
	const width = 16
	operations := []struct {
		name  string
		apply func(*RatchetTree, LeafIndex) error
	}{
		{"UpdateLeaf", func(tree *RatchetTree, i LeafIndex) error { return tree.UpdateLeaf(i, testTreeLeaf(0x11)) }},
		{"RemoveLeaf", func(tree *RatchetTree, i LeafIndex) error { return tree.RemoveLeaf(i) }},
	}
	asserted := 0
	for i := uint32(0); i < width; i += 1 {
		for _, operation := range operations {
			tree := treeWithOccupiedLeaves(t, width, func(LeafIndex) bool { return true })
			path, err := directPathOf(LeafIndex(i).NodeIndex(), tree.LeafWidth())
			if err != nil {
				t.Fatalf("directPathOf(%d): %v", i, err)
			}
			if len(path) < 4 {
				t.Fatalf("leaf %d's direct path is %v; with fewer than two interior nodes this test is about one node and not about the interior",
					i, path)
			}
			interior := path[1 : len(path)-1]
			if err := operation.apply(tree, LeafIndex(i)); err != nil {
				t.Fatalf("%s(%d): %v", operation.name, i, err)
			}
			for _, x := range interior {
				if !tree.IsBlank(x) {
					t.Fatalf("%s left interior node %d of leaf %d's direct path %v standing; a loop over path[0] alone and a loop over path[len-1] alone both leave exactly %v",
						operation.name, x, i, path, interior)
				}
				asserted += 1
			}
		}
	}
	if asserted == 0 {
		t.Fatalf("no interior node was asserted about")
	}
}

// TestRemoveLeafTruncatesToTheRightmostMemberAndNotToTheMemberCount separates the two rules that
// agree on every tree with no gaps in it.
//
// A truncation derived from the member COUNT is the natural wrong answer -- three members, so four
// leaves -- and it does not report an error when it is wrong. It drops a member out of the group.
func TestRemoveLeafTruncatesToTheRightmostMemberAndNotToTheMemberCount(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	for _, gone := range []LeafIndex{1, 2} {
		if err := tree.RemoveLeaf(gone); err != nil {
			t.Fatalf("RemoveLeaf(%d): %v", gone, err)
		}
	}
	if tree.MemberCount() != 2 {
		t.Fatalf("member count = %d, want 2", tree.MemberCount())
	}
	if got := FullLeafCount(LeafCount(tree.MemberCount())); got >= tree.LeafWidth() {
		t.Fatalf("the members fit a width of %d, which is not narrower than the tree's %d, so this fixture separates nothing",
			got, tree.LeafWidth())
	}
	if tree.LeafWidth() != 4 {
		t.Fatalf("leaf width = %d, want 4: two members at leaves 0 and 3 do not fit in two leaves", tree.LeafWidth())
	}
	if tree.Leaf(LeafIndex(3)) == nil {
		t.Fatalf("the truncation dropped leaf 3, which is a member")
	}
	assertStillATree(t, "after removing into a gapped membership", tree)
}

// TestRemoveLeafShrinksToExactlyWhatTheTreeMathSays sweeps every occupancy an eight leaf tree can
// have and holds the width after a removal against TruncatedLeafCount.
//
// Against the tree math and not against a halving written out here: a halving loop in the test
// would only say that the test and the code halve the same way, and the property is that the
// container and the arithmetic every tree hash is computed against give one answer. The two
// counters refuse a sweep in which nothing shrinks, or in which nothing holds its width.
func TestRemoveLeafShrinksToExactlyWhatTheTreeMathSays(t *testing.T) {
	const width = 8
	shrank, held := 0, 0
	for pattern := uint32(1); pattern < (uint32(1) << width); pattern += 1 {
		for removed := uint32(0); removed < width; removed += 1 {
			if pattern&(uint32(1)<<removed) == 0 {
				continue
			}
			tree := treeWithOccupiedLeaves(t, width, func(i LeafIndex) bool {
				return pattern&(uint32(1)<<uint32(i)) != 0
			})
			if err := tree.RemoveLeaf(LeafIndex(removed)); err != nil {
				t.Fatalf("occupancy %b: RemoveLeaf(%d): %v", pattern, removed, err)
			}
			left := pattern &^ (uint32(1) << removed)
			// the one leaf tree is the floor, and it is what an empty membership shrinks to.
			want := LeafCount(1)
			if left != 0 {
				rightmost := LeafIndex(0)
				for i := uint32(0); i < width; i += 1 {
					if left&(uint32(1)<<i) != 0 {
						rightmost = LeafIndex(i)
					}
				}
				truncated, err := TruncatedLeafCount(rightmost)
				if err != nil {
					t.Fatalf("TruncatedLeafCount(%d): %v", rightmost, err)
				}
				want = truncated
			}
			if tree.LeafWidth() != want {
				t.Fatalf("occupancy %b minus leaf %d: leaf width = %d, want %d",
					pattern, removed, tree.LeafWidth(), want)
			}
			if want < LeafCount(width) {
				shrank += 1
			} else {
				held += 1
			}
			// and nothing that was a member left with the right half.
			for i := uint32(0); i < width; i += 1 {
				if left&(uint32(1)<<i) == 0 {
					continue
				}
				if tree.Leaf(LeafIndex(i)) == nil {
					t.Fatalf("occupancy %b minus leaf %d: leaf %d was a member and is gone", pattern, removed, i)
				}
			}
		}
	}
	if shrank == 0 || held == 0 {
		t.Fatalf("the sweep saw %d removals that shrank the tree and %d that did not, and a zero in either is a sweep that cannot tell a truncation from its absence",
			shrank, held)
	}
}

// TestEveryTreeOperationLeavesATreeThisPackageStillAgreesWith runs all three operations over every
// occupancy of every small width and asks, after each one, whether what is left is still a tree.
//
// An operation that leaves a malformed tree does not fail at the operation. It fails at a tree
// hash, a resolution or a join in a later task, against code that has nothing to do with it -- so
// the invariants are asserted where the damage is done rather than where it surfaces. The refusals
// are swept alongside the successes because a refusal is a claim too: a tree the operation
// declined to change must be exactly the tree it was handed.
func TestEveryTreeOperationLeavesATreeThisPackageStillAgreesWith(t *testing.T) {
	operations := []struct {
		name  string
		apply func(*RatchetTree, LeafIndex) error
	}{
		{"AddLeaf", func(tree *RatchetTree, i LeafIndex) error {
			_, err := tree.AddLeaf(testTreeLeaf(uint32(i) + 0x30))
			return err
		}},
		{"UpdateLeaf", func(tree *RatchetTree, i LeafIndex) error {
			return tree.UpdateLeaf(i, testTreeLeaf(uint32(i)+0x30))
		}},
		{"RemoveLeaf", func(tree *RatchetTree, i LeafIndex) error { return tree.RemoveLeaf(i) }},
	}
	accepted, refused := 0, 0
	for _, width := range []uint32{1, 2, 4, 8} {
		for pattern := uint32(0); pattern < (uint32(1) << width); pattern += 1 {
			for i := uint32(0); i < width; i += 1 {
				for _, operation := range operations {
					tree := treeWithOccupiedLeaves(t, width, func(j LeafIndex) bool {
						return pattern&(uint32(1)<<uint32(j)) != 0
					})
					where := fmt.Sprintf("width %d occupancy %b leaf %d after %s", width, pattern, i, operation.name)
					// the bytes before, so a refusal can be held to changing nothing without a
					// deep equality over structures a Clone is entitled to normalise.
					beforeBytes, beforeErr := marshalRatchetTree(tree)
					err := operation.apply(tree, LeafIndex(i))
					switch {
					case err == nil:
						accepted += 1
					case errors.Is(err, ErrLeafIndexOutOfRange):
						refused += 1
						afterBytes, afterErr := marshalRatchetTree(tree)
						if (beforeErr == nil) != (afterErr == nil) {
							t.Fatalf("%s: the operation reported a refusal and left a tree that encodes differently", where)
						}
						if beforeErr == nil && !bytes.Equal(beforeBytes, afterBytes) {
							t.Fatalf("%s: the operation reported a refusal and changed the tree anyway", where)
						}
					default:
						t.Fatalf("%s: %v", where, err)
					}
					assertStillATree(t, where, tree)
				}
			}
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("the sweep saw %d operations accepted and %d refused, and a zero in either is a sweep that only exercised one branch",
			accepted, refused)
	}
}

// TestAddLeafMarksEveryNonBlankParentAboveABlankOne is the clause of the marking loop that a
// stop-at-the-first-blank gets wrong, and that nothing else in this package can see.
//
// The loop SKIPS a blank parent rather than stopping at one, and the two are the same loop on
// every fixture whose direct path is either wholly occupied or wholly blank -- which is every
// fixture here before this one. The arrangement that separates them is a blank node LOW on the
// path with an occupied node ABOVE it, and that is not a corner case: it is the ordinary state a
// Remove leaves behind. RemoveLeaf(2) on an eight leaf tree blanks 5, 3 and 7; a later commit
// from leaf 0 repopulates 1, 3 and 7 and leaves 5 standing blank; and the next Add lands on leaf
// 2 with the path [5, 3, 7]. So the blank set is swept over the whole family of patterns the path
// can hold rather than drawn once, and what is expected at each level is read off the pattern.
//
// The consequence of stopping is not a short list. Section 4.2 builds a node's resolution from
// its unmerged_leaves, so an ancestor that never recorded the joiner seals no path secret to
// them, and section 7.9.2 condition 3 then fails at the NEXT join -- at nodes the loop is not
// even in.
func TestAddLeafMarksEveryNonBlankParentAboveABlankOne(t *testing.T) {
	separating := 0
	for _, width := range []uint32{4, 8, 16} {
		for target := uint32(0); target < width; target += 1 {
			occupied := func(i LeafIndex) bool { return uint32(i) != target }
			shape := treeWithOccupiedLeaves(t, width, occupied)
			path, err := directPathOf(LeafIndex(target).NodeIndex(), shape.LeafWidth())
			if err != nil {
				t.Fatalf("width %d leaf %d: directPathOf: %v", width, target, err)
			}
			for pattern := uint32(0); pattern < (uint32(1) << uint32(len(path))); pattern += 1 {
				blankAt := func(level int) bool { return pattern&(uint32(1)<<uint32(level)) != 0 }
				// derived from the pattern rather than listed: a blank at one level with an
				// occupied node anywhere above it is the only arrangement in which skipping and
				// stopping part company, and the counter at the end refuses a sweep that never
				// reaches one.
				for low := 0; low < len(path); low += 1 {
					if !blankAt(low) {
						continue
					}
					reached := false
					for high := low + 1; high < len(path); high += 1 {
						if !blankAt(high) {
							reached = true
						}
					}
					if reached {
						separating += 1
					}
					break
				}
				tree := treeWithOccupiedLeaves(t, width, occupied)
				for level, x := range path {
					if !blankAt(level) {
						continue
					}
					if err := tree.Blank(x); err != nil {
						t.Fatalf("width %d leaf %d: Blank(%d): %v", width, target, x, err)
					}
				}
				where := fmt.Sprintf("width %d, leaf %d, path %v with %b blanked", width, target, path, pattern)
				got, err := tree.AddLeaf(testTreeLeaf(target + 0x50))
				if err != nil {
					t.Fatalf("%s: AddLeaf: %v", where, err)
				}
				if got != LeafIndex(target) {
					t.Fatalf("%s: AddLeaf = %d, want the only blank leaf %d", where, got, target)
				}
				for level, x := range path {
					parent := tree.ParentAt(x)
					if blankAt(level) {
						// a blank node publishes no key and so owes no debt, and Add must not
						// fill it either: filling it is a commit's job and not an add's.
						if parent != nil {
							t.Fatalf("%s: AddLeaf put a node at %d, which was blank", where, x)
						}
						continue
					}
					if parent == nil {
						t.Fatalf("%s: AddLeaf blanked node %d, and Add never blanks", where, x)
					}
					if !slices.Contains(parent.UnmergedLeaves, got) {
						t.Fatalf("%s: node %d is a non-blank node of leaf %d's direct path and does not list it unmerged: %v",
							where, x, got, parent.UnmergedLeaves)
					}
				}
				assertStillATree(t, where, tree)
			}
		}
	}
	if separating == 0 {
		t.Fatalf("no pattern in this sweep puts a blank node below an occupied one, so nothing here separates a loop that skips a blank from one that stops at it")
	}
}

// TestATruncationLeavesNothingOfTheOldTreeReachable is the half of the shrink that is not about
// width at all.
//
// A slice reaches its CAPACITY and not its length, so self.nodes[:w] leaves the whole old array
// alive behind a shorter tree: the parent keys of the half that went away, and every node the
// removal did not itself empty, stay hanging off this container for as long as it lives. Every
// width, member count, hash and round trip property in this file agrees with a reslice, because
// none of them can look past the length -- which is exactly why the statement has to be made in
// the terms that can be false, over the tree's own storage.
//
// The two counters are what stop it going quiet: a sweep in which nothing shrinks, or in which
// every dropped node had already been blanked by the removal itself, has nothing to leak and
// would report the same clean run a complete one does.
func TestATruncationLeavesNothingOfTheOldTreeReachable(t *testing.T) {
	const width = 8
	shrank, carrying := 0, 0
	for pattern := uint32(1); pattern < (uint32(1) << width); pattern += 1 {
		occupied := func(i LeafIndex) bool { return pattern&(uint32(1)<<uint32(i)) != 0 }
		for removed := uint32(0); removed < width; removed += 1 {
			if !occupied(LeafIndex(removed)) {
				continue
			}
			tree := treeWithOccupiedLeaves(t, width, occupied)
			before := tree.NodeWidth()
			// what the removal empties on its own account, derived from the tree math rather
			// than assumed, so the non-vacuity counter below cannot be satisfied by a node the
			// blanking had already cleared.
			emptied := map[NodeIndex]bool{LeafIndex(removed).NodeIndex(): true}
			path, err := directPathOf(LeafIndex(removed).NodeIndex(), tree.LeafWidth())
			if err != nil {
				t.Fatalf("occupancy %b: directPathOf(%d): %v", pattern, removed, err)
			}
			for _, x := range path {
				emptied[x] = true
			}
			was := make([]*Node, before)
			copy(was, tree.nodes)
			if err := tree.RemoveLeaf(LeafIndex(removed)); err != nil {
				t.Fatalf("occupancy %b: RemoveLeaf(%d): %v", pattern, removed, err)
			}
			if tree.NodeWidth() >= before {
				continue
			}
			shrank += 1
			where := fmt.Sprintf("occupancy %b minus leaf %d", pattern, removed)
			reachable := tree.nodes[:cap(tree.nodes)]
			for x := tree.NodeWidth(); x < uint32(len(reachable)); x += 1 {
				if reachable[x] != nil {
					t.Fatalf("%s: the tree is %d nodes wide and node %d of the tree it used to be is still reachable through its own storage",
						where, tree.NodeWidth(), x)
				}
			}
			if cap(tree.nodes) != len(tree.nodes) {
				t.Fatalf("%s: the node array is %d long and %d wide, so the array the old tree lived in is still alive behind it",
					where, len(tree.nodes), cap(tree.nodes))
			}
			for x := tree.NodeWidth(); x < before; x += 1 {
				if was[x] != nil && !emptied[NodeIndex(x)] {
					carrying += 1
					break
				}
			}
		}
	}
	if shrank == 0 || carrying == 0 {
		t.Fatalf("the sweep saw %d truncations, %d of which dropped a node the removal had not already emptied, and a zero in either is a sweep with nothing to leak",
			shrank, carrying)
	}
}

// TestRemoveLeafDropsEveryUnmergedEntryNamingABlankLeafAndNotOnlyTheRemovedOne is the second
// condition the sweep's derived predicate is wider by.
//
// "The leaf this entry names is blank" and "the leaf that was just removed" pick out the same set
// on a well formed tree, so the difference is only visible on a tree that is already wrong -- and
// a tree that is already wrong is precisely what this sweep is for, because the entries it plants
// are the ones no operation of this package would have written and every operation of it will
// later read. Each parent carries the whole blank set AND every member of its own subtree that is
// not the one leaving, so "drops the entries naming a blank" and "clears the list" are not the
// same assertion.
//
// The third counter is the shrink flavour, and it is the one the ordering of RemoveLeaf's two
// closing steps is argued from: an entry left standing on a node the truncation KEEPS, naming a
// leaf the truncation puts outside the new width, is a node SetParent would have refused and
// Resolution answers the empty list for -- "seal to everyone under this node" quietly becoming
// "seal to nobody". What is pinned is that POSTCONDITION and not the order the two steps run in:
// swapping them leaves this sweep green, because a truncation never puts an occupied leaf outside
// the tree and so every entry a shrink moves out of range was already naming a blank before it.
// The reason that is a coincidence rather than a hole is recorded at
// dropUnmergedLeavesNamingABlankLeaf, where it belongs.
func TestRemoveLeafDropsEveryUnmergedEntryNamingABlankLeafAndNotOnlyTheRemovedOne(t *testing.T) {
	const width = 8
	surviving, withASurvivor, pastTheNewWidth := 0, 0, 0
	for pattern := uint32(0); pattern < (uint32(1) << width); pattern += 1 {
		occupied := func(i LeafIndex) bool { return pattern&(uint32(1)<<uint32(i)) != 0 }
		blanks := []LeafIndex{}
		for i := uint32(0); i < width; i += 1 {
			if !occupied(LeafIndex(i)) {
				blanks = append(blanks, LeafIndex(i))
			}
		}
		if len(blanks) == 0 {
			continue
		}
		for removed := uint32(0); removed < width; removed += 1 {
			if !occupied(LeafIndex(removed)) {
				continue
			}
			tree := treeWithOccupiedLeaves(t, width, occupied)
			want := map[NodeIndex][]LeafIndex{}
			for x := uint32(1); x < tree.NodeWidth(); x += 2 {
				first, last := SubtreeLeaves(NodeIndex(x))
				keep := []LeafIndex{}
				for i := first; i <= last; i += 1 {
					if occupied(i) && uint32(i) != removed {
						keep = append(keep, i)
					}
				}
				entries := append(append([]LeafIndex{}, keep...), blanks...)
				slices.Sort(entries)
				if err := tree.SetParent(NodeIndex(x), &ParentNode{
					EncryptionKey:  HpkePublicKey(repeatByte(byte(0xa0+x), 32)),
					UnmergedLeaves: entries,
				}); err != nil {
					t.Fatalf("occupancy %b: SetParent(%d) with %v: %v", pattern, x, entries, err)
				}
				want[NodeIndex(x)] = keep
			}
			if err := tree.RemoveLeaf(LeafIndex(removed)); err != nil {
				t.Fatalf("occupancy %b: RemoveLeaf(%d): %v", pattern, removed, err)
			}
			where := fmt.Sprintf("occupancy %b minus leaf %d", pattern, removed)
			for x := uint32(1); x < tree.NodeWidth(); x += 2 {
				parent := tree.ParentAt(NodeIndex(x))
				if parent == nil {
					continue
				}
				surviving += 1
				if !slices.Equal(parent.UnmergedLeaves, want[NodeIndex(x)]) {
					t.Fatalf("%s: node %d unmerged = %v, want %v: every entry naming a leaf the tree does not have goes, and every entry naming a member stays",
						where, x, parent.UnmergedLeaves, want[NodeIndex(x)])
				}
				if len(want[NodeIndex(x)]) > 0 {
					withASurvivor += 1
				}
				for _, blank := range blanks {
					if LeafCount(blank) >= tree.LeafWidth() {
						pastTheNewWidth += 1
						break
					}
				}
			}
			assertStillATree(t, where, tree)
		}
	}
	if surviving == 0 || withASurvivor == 0 || pastTheNewWidth == 0 {
		t.Fatalf("the sweep reached %d surviving parents, %d of them carrying an entry that had to stay and %d of them carrying one the shrink put outside the tree, and a zero in any of the three is a sweep that says less than it claims",
			surviving, withASurvivor, pastTheNewWidth)
	}
}

// ---------------------------------------------------------------------------
// section 7.6: the filtered direct path and the copath encryption targets
// ---------------------------------------------------------------------------
//
// What these tests are built against is the SILENT direction. A target list that is too short
// locks a member out and somebody reports it within an epoch; a target list that is too long
// hands the path secret to a member who must not have it -- a removed member whose leaf is
// blank, or a leaf this same commit is adding -- and produces no symptom at all. Everybody
// still receives a ciphertext, everybody still decrypts, and the group carries a reader.
//
// So the sweep below checks both directions of the set at once, against a reference that is not
// this package's resolution walk: over a tree whose parents are all blank -- which is exactly
// the shape newTestTree hands back and exactly the shape a Remove leaves behind -- the
// resolution of a node is the ascending list of its subtree's occupied leaves, and that is
// derivable from SubtreeLeaves and RatchetTree.Leaf alone. The filtering rule is re-derived the
// same way: a step survives when its copath child's subtree holds an occupied leaf.
//
// And the ORDER, elementwise, never as sets. The ciphertexts of an UpdatePath are paired
// positionally with these lists, so a permuted list seals each secret to the wrong member while
// holding exactly the right members -- and every member still gets a ciphertext of the right
// shape, so the first thing that notices is a decrypt failure one task later. The positions are
// DERIVED from the tree in every sweep rather than picked, because an index somebody chose stops
// meaning what it meant the moment the fixture width changes.

// occupiedLeavesUnder is the reference resolution for a tree whose parents are all blank: the
// occupied leaves of x's subtree, ascending.
//
// It reads SubtreeLeaves and RatchetTree.Leaf and nothing else -- not Resolution, not
// FilteredDirectPath -- so a defect in the walk under test cannot be transcribed into the answer
// it is compared against. It is only a resolution on an all-blank-parent tree, which is why
// every sweep using it asserts that shape rather than assuming it.
func occupiedLeavesUnder(tree *RatchetTree, x NodeIndex) []LeafIndex {
	first, last := SubtreeLeaves(x)
	out := []LeafIndex{}
	for i := first; i <= last; i += 1 {
		if LeafCount(i) >= tree.LeafWidth() {
			break
		}
		if tree.Leaf(i) != nil {
			out = append(out, i)
		}
	}
	return out
}

// assertEveryParentIsBlank is the precondition occupiedLeavesUnder rests on, stated rather than
// assumed: a non-blank parent resolves to itself followed by its unmerged leaves, which is not
// the list above.
func assertEveryParentIsBlank(t *testing.T, where string, tree *RatchetTree) {
	t.Helper()
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		if !tree.IsBlank(NodeIndex(x)) {
			t.Fatalf("%s: node %d is not blank, so the reference resolution below does not apply to this tree", where, x)
		}
	}
}

// referenceFilteredPath is the filtered direct path and the encryption targets of leaf sender,
// derived from the tree's occupancy rather than from the code under test.
//
// The FILTER is re-derived here -- a step survives when its copath child's subtree holds an
// occupied leaf -- so a filter that kept an empty copath child or dropped a populated one
// disagrees with this. The EXCLUSION is applied after the filter and not before it, which is the
// RFC's order and is observable: a step whose copath child holds exactly one occupied leaf and
// that leaf is excluded still contributes a node to the path, with an EMPTY target list, because
// the node still needs a fresh key even though nobody is sealed to it.
func referenceFilteredPath(t *testing.T, tree *RatchetTree, sender LeafIndex,
	exclude []LeafIndex) (path []NodeIndex, targets [][]NodeIndex) {
	t.Helper()
	width := tree.LeafWidth()
	pathNodes, err := DirectPath(sender.NodeIndex(), width)
	if err != nil {
		t.Fatalf("DirectPath(%d, %d): %v", sender.NodeIndex(), width, err)
	}
	path = []NodeIndex{}
	targets = [][]NodeIndex{}
	child := sender.NodeIndex()
	for _, node := range pathNodes {
		copathChild, err := Sibling(child, width)
		if err != nil {
			t.Fatalf("Sibling(%d, %d): %v", child, width, err)
		}
		child = node
		under := occupiedLeavesUnder(tree, copathChild)
		if len(under) == 0 {
			continue
		}
		kept := []NodeIndex{}
		for _, leaf := range under {
			if slices.Contains(exclude, leaf) {
				continue
			}
			kept = append(kept, leaf.NodeIndex())
		}
		path = append(path, node)
		targets = append(targets, kept)
	}
	return path, targets
}

// TestFilteredDirectPathAndTargetsAgreeWithTheTreesOwnOccupancy sweeps every member of every
// width this fixture builds, with every single leaf removed in turn, and holds both answers
// against the reference above.
//
// The removal is a real RemoveLeaf and not a Blank, because that is the operation the silent
// direction arrives through: after a Remove the member's leaf is blank, its direct path is
// blank, and the tree may have TRUNCATED, so every index the assertion is written against has to
// be re-derived from the tree afterwards rather than remembered from before.
//
// The counters at the end are what stop this from being a sweep that says less than it claims. A
// run in which no step was ever filtered out proves nothing about filtering; one in which no
// target list ever held two entries cannot see a permutation inside a list; one in which no two
// adjacent lists ever differed cannot see a permutation of the lists themselves.
func TestFilteredDirectPathAndTargetsAgreeWithTheTreesOwnOccupancy(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	dropped, twoDeep, orderable, senders, emptied := 0, 0, 0, 0, 0
	for _, width := range []uint32{1, 2, 3, 4, 5, 6, 7, 8} {
		for removed := uint32(0); removed <= width; removed += 1 {
			tree, _ := newTestTree(t, crypto, width)
			where := fmt.Sprintf("%d members", width)
			if removed < width {
				if err := tree.RemoveLeaf(LeafIndex(removed)); err != nil {
					t.Fatalf("%s: RemoveLeaf(%d): %v", where, removed, err)
				}
				where = fmt.Sprintf("%s minus leaf %d", where, removed)
			}
			assertEveryParentIsBlank(t, where, tree)
			for _, sender := range tree.NonBlankLeaves() {
				senders += 1
				// the excluded leaf is derived from the tree rather than chosen: the
				// last occupied leaf that is not the sender stands in for one this
				// commit is adding. A one member tree has none, which is the case
				// that has to be allowed to have none.
				exclude := []LeafIndex{}
				for _, other := range tree.NonBlankLeaves() {
					if other != sender {
						exclude = []LeafIndex{other}
					}
				}
				for _, excluded := range [][]LeafIndex{nil, exclude} {
					wantPath, wantTargets := referenceFilteredPath(t, tree, sender, excluded)
					gotPath, err := tree.FilteredDirectPath(sender)
					if err != nil {
						t.Fatalf("%s: FilteredDirectPath(%d): %v", where, sender, err)
					}
					if !equalNodeIndices(gotPath, wantPath) {
						t.Fatalf("%s: FilteredDirectPath(%d) = %v, want %v",
							where, sender, gotPath, wantPath)
					}
					gotTargets, err := tree.EncryptionTargets(sender, excluded)
					if err != nil {
						t.Fatalf("%s: EncryptionTargets(%d, %v): %v", where, sender, excluded, err)
					}
					if len(gotTargets) != len(wantTargets) {
						t.Fatalf("%s: EncryptionTargets(%d, %v) = %v, want %v: one entry per filtered path node",
							where, sender, excluded, gotTargets, wantTargets)
					}
					for at := range wantTargets {
						if !equalNodeIndices(gotTargets[at], wantTargets[at]) {
							t.Fatalf("%s: EncryptionTargets(%d, %v)[%d] = %v, want %v",
								where, sender, excluded, at, gotTargets[at], wantTargets[at])
						}
						if len(wantTargets[at]) == 0 {
							emptied += 1
						}
					}
					fullPath, err := DirectPath(sender.NodeIndex(), tree.LeafWidth())
					if err != nil {
						t.Fatalf("%s: DirectPath(%d): %v", where, sender, err)
					}
					dropped += len(fullPath) - len(gotPath)
					for at, list := range gotTargets {
						if len(list) >= 2 {
							twoDeep += 1
						}
						if at > 0 && !equalNodeIndices(list, gotTargets[at-1]) {
							orderable += 1
						}
					}
				}
			}
		}
	}
	if dropped == 0 || twoDeep == 0 || orderable == 0 || senders == 0 || emptied == 0 {
		t.Fatalf("the sweep ran %d senders, dropped %d path nodes, saw %d target lists of two or more, %d adjacent pairs of distinct lists and %d lists the exclusion emptied; a zero in any of the five is a sweep that cannot see filtering, order, or the exclusion running after the filter",
			senders, dropped, twoDeep, orderable, emptied)
	}
	t.Logf("%d senders, %d dropped path nodes, %d multi-entry target lists, %d distinguishable adjacent pairs, %d lists emptied by the exclusion",
		senders, dropped, twoDeep, orderable, emptied)
}

// TestEncryptionTargetsNeverNameALeafThisCommitAdds is the unmerged half, which the sweep above
// cannot reach: it runs over trees whose parents are all blank, and there an added leaf reaches
// a resolution only as the nearest non-blank node under the copath child.
//
// In a committed tree it reaches one by a second route as well. RFC 9420 section 7.5 appends a
// non-blank node's unmerged leaves to the resolution behind it, so the added leaf's node index
// is emitted by every non-blank ancestor that lists it, at a position the walk would otherwise
// never descend to. An exclusion that removed only the first route -- by blanking the leaf, say,
// which is the shape that reads correct -- leaves the second in place: Resolution never asks
// whether an unmerged leaf is blank, so the entry survives and the new member is sealed to
// anyway. It decrypts cleanly, and it was never supposed to be able to.
//
// Both routes are counted and both are required, and what the exclusion removes is held to
// EXACTLY the added leaf's own node index: the ancestor that lists it must stay, because the
// other members under that ancestor still have to receive.
func TestEncryptionTargetsNeverNameALeafThisCommitAdds(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _, chain := chainedTestTree(t, crypto, 5, LeafIndex(4))
	added, err := tree.AddLeaf(tree.Leaf(LeafIndex(0)).Clone())
	if err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	listing := []NodeIndex{}
	for _, x := range chain {
		if slices.Contains(tree.UnmergedLeaves(x), added) {
			listing = append(listing, x)
		}
	}
	if len(listing) == 0 {
		t.Fatalf("no node of the committed chain %v lists the added leaf %d unmerged, so this fixture never built the unmerged route",
			chain, added)
	}
	direct, unmerged, removals := 0, 0, 0
	for _, sender := range tree.NonBlankLeaves() {
		if sender == added {
			continue
		}
		included, err := tree.EncryptionTargets(sender, nil)
		if err != nil {
			t.Fatalf("EncryptionTargets(%d, nil): %v", sender, err)
		}
		excluded, err := tree.EncryptionTargets(sender, []LeafIndex{added})
		if err != nil {
			t.Fatalf("EncryptionTargets(%d, [%d]): %v", sender, added, err)
		}
		if len(included) != len(excluded) {
			t.Fatalf("excluding leaf %d changed the number of path steps for sender %d from %d to %d; the exclusion is applied to a resolution, never to the filter",
				added, sender, len(included), len(excluded))
		}
		for at := range included {
			want := []NodeIndex{}
			for position, y := range included[at] {
				if y != added.NodeIndex() {
					want = append(want, y)
					continue
				}
				removals += 1
				if position > 0 && slices.Contains(tree.UnmergedLeaves(included[at][position-1]), added) {
					unmerged += 1
				} else {
					direct += 1
				}
			}
			if !equalNodeIndices(excluded[at], want) {
				t.Fatalf("sender %d targets[%d] with leaf %d excluded = %v, want %v: the added leaf's own node index goes and nothing else does",
					sender, at, added, excluded[at], want)
			}
		}
	}
	if direct == 0 || unmerged == 0 || removals == 0 {
		t.Fatalf("the added leaf %d was named %d times across the members' target lists, %d of them as the nearest non-blank node and %d of them behind an ancestor that lists it unmerged; a zero in any of the three leaves one of the two routes untested",
			added, removals, direct, unmerged)
	}
	t.Logf("the added leaf %d was named %d times: %d directly, %d through an unmerged list", added, removals, direct, unmerged)
}

// TestEveryCommittedTreeExcludesTheLeafItJustAddedFromEveryTargetList is the same statement as the
// test above, swept.
//
// The occupancy sweep next door cannot reach the unmerged route at all -- it asserts every parent
// is blank, and in such a tree an added leaf reaches a resolution only as the nearest non-blank
// node under a copath child -- and the test above holds the route on ONE tree: five members, one
// committer, one added leaf. The route it holds is the silent direction of the pair, so it is the
// one that most needs more than a fixture: a resolution that is too small is a member who cannot
// decrypt and says so, and one that is too large is a leaf the group did not mean to seal to,
// reading the commit cleanly and saying nothing.
//
// Every width and every committer, with both routes counted across the whole sweep rather than
// per tree. Which trees build the unmerged route is a property of the shape and not of this
// test's choices: a full tree doubles on Add and the new leaf's whole direct path is fresh and
// blank, so nothing lists it unmerged, while a tree with a blank leaf inside it takes the new
// member under parents that are already occupied and every one of them lists it. Both kinds are
// in the sweep, and the counter that says how many trees actually built the unmerged route is
// what stops a shape change from quietly leaving the sweep holding one route.
//
// The leaf cloned to make the new member is never the committer's. The committer's leaf carries
// LeafNodeSourceCommit and a parent hash bound to its own index, so cloning it would add a member
// whose leaf is meaningless at the index it lands on, which is a fixture defect rather than a
// case.
func TestEveryCommittedTreeExcludesTheLeafItJustAddedFromEveryTargetList(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	trees, listing, senders, removals, direct, unmerged := 0, 0, 0, 0, 0, 0
	for width := uint32(2); width <= 8; width += 1 {
		for committer := uint32(0); committer < width; committer += 1 {
			tree, _, chain := chainedTestTree(t, crypto, width, LeafIndex(committer))
			where := fmt.Sprintf("%d members, committer %d", width, committer)
			donor := LeafIndex((committer + 1) % width)
			added, err := tree.AddLeaf(tree.Leaf(donor).Clone())
			if err != nil {
				t.Fatalf("%s: AddLeaf(a clone of leaf %d): %v", where, donor, err)
			}
			trees += 1
			for _, x := range chain {
				if slices.Contains(tree.UnmergedLeaves(x), added) {
					listing += 1
				}
			}
			for _, sender := range tree.NonBlankLeaves() {
				if sender == added {
					continue
				}
				senders += 1
				included, err := tree.EncryptionTargets(sender, nil)
				if err != nil {
					t.Fatalf("%s: EncryptionTargets(%d, nil): %v", where, sender, err)
				}
				excluded, err := tree.EncryptionTargets(sender, []LeafIndex{added})
				if err != nil {
					t.Fatalf("%s: EncryptionTargets(%d, [%d]): %v", where, sender, added, err)
				}
				if len(included) != len(excluded) {
					t.Fatalf("%s: excluding leaf %d changed the number of path steps for sender %d from %d to %d; the exclusion is applied to a resolution, never to the filter",
						where, added, sender, len(included), len(excluded))
				}
				for at := range included {
					want := []NodeIndex{}
					for position, y := range included[at] {
						if y != added.NodeIndex() {
							want = append(want, y)
							continue
						}
						removals += 1
						if position > 0 && slices.Contains(tree.UnmergedLeaves(included[at][position-1]), added) {
							unmerged += 1
						} else {
							direct += 1
						}
					}
					if !equalNodeIndices(excluded[at], want) {
						t.Fatalf("%s: sender %d targets[%d] with leaf %d excluded = %v, want %v: the added leaf's own node index goes and nothing else does",
							where, sender, at, added, excluded[at], want)
					}
				}
			}
		}
	}
	if trees == 0 || listing == 0 || senders == 0 || direct == 0 || unmerged == 0 {
		t.Fatalf("the sweep built %d committed trees, %d chain nodes listing the added leaf unmerged, %d senders, and named the added leaf %d times: %d as the nearest non-blank node and %d behind an ancestor that lists it; a zero in any of them is a sweep holding fewer routes than it states",
			trees, listing, senders, removals, direct, unmerged)
	}
	t.Logf("%d committed trees over widths 2 to 8, %d chain nodes listing the added leaf unmerged, %d senders, %d namings of the added leaf: %d directly, %d through an unmerged list",
		trees, listing, senders, removals, direct, unmerged)
}

// TestFilteredPathEntryPointsRefuseALeafOutsideTheTree holds the container's own range check.
//
// ErrLeafIndexOutOfRange and not merely "an error": tree math's ErrLeafOutOfRange is what comes
// back if this package's guard is removed and the arithmetic's own is left to fire, and the wrap
// in tree_errors.go runs one way only -- the container's sentinel answers for the arithmetic's,
// never the reverse. So a body that dropped its guard fails this while still returning an error.
func TestFilteredPathEntryPointsRefuseALeafOutsideTheTree(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, _ := newTestTree(t, crypto, 4)
	outside := LeafIndex(tree.LeafWidth())
	if _, err := tree.FilteredDirectPath(outside); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("FilteredDirectPath(%d) err = %v, want ErrLeafIndexOutOfRange", outside, err)
	}
	if _, err := tree.EncryptionTargets(outside, nil); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("EncryptionTargets(%d, nil) err = %v, want ErrLeafIndexOutOfRange", outside, err)
	}
	if _, err := tree.filteredPathSteps(outside); !errors.Is(err, ErrLeafIndexOutOfRange) {
		t.Fatalf("filteredPathSteps(%d) err = %v, want ErrLeafIndexOutOfRange", outside, err)
	}
}

// TestFilteredPathStepsCarryTheCopathChildAndNotTheOtherOne is the private half, and it is here
// because tasks 18, 20, 21 and 22 all read PathStep.CopathChild rather than re-deriving it.
//
// Both fields are node indices of one tree, so a body that filled them the other way round type
// checks, round trips and produces a path of exactly the right length -- and then the commit
// encrypts every secret to the subtree the sender's own leaf is in, which already holds it. The
// check is structural: the sender descends from the step's node and does NOT descend from its
// copath child, and the two are a parent and one of its children.
func TestFilteredPathStepsCarryTheCopathChildAndNotTheOtherOne(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	steps := 0
	for _, width := range []uint32{2, 3, 4, 5, 8} {
		tree, _ := newTestTree(t, crypto, width)
		for _, sender := range tree.NonBlankLeaves() {
			got, err := tree.filteredPathSteps(sender)
			if err != nil {
				t.Fatalf("%d members: filteredPathSteps(%d): %v", width, sender, err)
			}
			for _, step := range got {
				steps += 1
				if !InSubtree(step.Node, sender.NodeIndex()) {
					t.Fatalf("%d members: step %v of leaf %d has a node the sender does not descend from",
						width, step, sender)
				}
				if InSubtree(step.CopathChild, sender.NodeIndex()) {
					t.Fatalf("%d members: step %v of leaf %d names the child the sender DOES descend from, which already holds the secret",
						width, step, sender)
				}
				left, err := Left(step.Node)
				if err != nil {
					t.Fatalf("Left(%d): %v", step.Node, err)
				}
				right, err := Right(step.Node)
				if err != nil {
					t.Fatalf("Right(%d): %v", step.Node, err)
				}
				if step.CopathChild != left && step.CopathChild != right {
					t.Fatalf("%d members: step %v of leaf %d names a copath child that is neither child of its node (%d, %d)",
						width, step, sender, left, right)
				}
			}
		}
	}
	if steps == 0 {
		t.Fatal("the sweep read no steps at all")
	}
}

// TestFilteredDirectPathSkipsEmptyCopathResolutions is the plan's own golden, kept because a
// hand written expectation over a named width is the one thing a derived sweep cannot be: it
// states what the answer IS rather than that two derivations agree about it.
func TestFilteredDirectPathSkipsEmptyCopathResolutions(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	full, err := tree.FilteredDirectPath(LeafIndex(0))
	if err != nil {
		t.Fatalf("FilteredDirectPath: %v", err)
	}
	if !equalNodeIndices(full, []NodeIndex{1, 3}) {
		t.Fatalf("filtered direct path = %v, want [1 3]", full)
	}
	// blank leaf 1: node 1's copath child (node 2) now resolves to nothing, so node 1
	// drops out of the filtered path.
	if err := tree.Blank(NodeIndex(2)); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	got, err := tree.FilteredDirectPath(LeafIndex(0))
	if err != nil {
		t.Fatalf("FilteredDirectPath: %v", err)
	}
	if !equalNodeIndices(got, []NodeIndex{3}) {
		t.Fatalf("filtered direct path = %v, want [3]", got)
	}
	// an only member has an empty filtered direct path.
	lone, _ := newTestTree(t, crypto, 1)
	got, err = lone.FilteredDirectPath(LeafIndex(0))
	if err != nil {
		t.Fatalf("FilteredDirectPath: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("single-member filtered direct path = %v, want empty", got)
	}
}

// TestEncryptionTargetsExcludeNewlyAddedLeaves is the plan's golden for the targets.
func TestEncryptionTargetsExcludeNewlyAddedLeaves(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	targets, err := tree.EncryptionTargets(LeafIndex(0), nil)
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want one entry per filtered path node", targets)
	}
	if !equalNodeIndices(targets[0], []NodeIndex{2}) {
		t.Fatalf("targets[0] = %v, want [2]", targets[0])
	}
	if !equalNodeIndices(targets[1], []NodeIndex{4, 6}) {
		t.Fatalf("targets[1] = %v, want [4 6]", targets[1])
	}
	// leaf 3 was added by this commit: it gets the path secret in the Welcome, so it
	// is not an encryption target here.
	targets, err = tree.EncryptionTargets(LeafIndex(0), []LeafIndex{3})
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	if !equalNodeIndices(targets[1], []NodeIndex{4}) {
		t.Fatalf("targets[1] with leaf 3 excluded = %v, want [4]", targets[1])
	}
}

// TestEncryptionTargetsExcludeUnmergedNewLeaves is the plan's golden for the unmerged route.
func TestEncryptionTargetsExcludeUnmergedNewLeaves(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	// node 5 is non-blank and lists leaf 3 unmerged, so leaf 3 appears in the
	// resolution both as itself and via node 5's unmerged list.
	if err := tree.SetParent(NodeIndex(5), &ParentNode{
		EncryptionKey:  HpkePublicKey(bytes.Repeat([]byte{0x05}, 32)),
		UnmergedLeaves: []LeafIndex{3},
	}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	targets, err := tree.EncryptionTargets(LeafIndex(0), []LeafIndex{3})
	if err != nil {
		t.Fatalf("EncryptionTargets: %v", err)
	}
	if !equalNodeIndices(targets[1], []NodeIndex{5}) {
		t.Fatalf("targets[1] = %v, want [5] with the unmerged new leaf removed", targets[1])
	}
}
