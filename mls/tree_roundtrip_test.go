// The committed seed corpus for this plan's two decoders, and the deterministic regression net
// over it.
//
// Round-trip stability is the property that matters most here. MLS signs over serialized forms, so
// a decoder that accepts two encodings of one tree is a signature-bypass primitive rather than a
// leniency: encode(decode(x)) must be the canonical re-serialization, and decode(encode(decode(x)))
// must equal decode(x).
//
// WHY A CORPUS AT ALL. p1 measured it: uniform random bytes reach the round-trip property 14 times
// in 4096 -- 0.34 percent -- against the SIMPLEST structure in this tree, because the varint length
// prefix rejects them first, while a structured generator reaches it 4096 times in 4096. A target
// seeded only with random bytes spends its budget rediscovering the prefix. The structures here are
// far larger than the one that was measured -- a LeafNode has three source variants and a nested
// Credential, Capabilities, Lifetime and extensions vector; an UpdatePath is three levels of vector
// nesting over one of those -- so the fraction is smaller still.
//
// WHERE THE SEEDS LIVE, AND WHY NOT WHERE THE PLAN SAYS. The plan places them in
// mls/interop/testdata/corpus/<kind> for the validation plan's seedCorpus helper to read, on the
// belief that the validation plan owns targets named FuzzRatchetTreeDecode and FuzzUpdatePathDecode.
// It does not. That plan's nine Gate-4 targets are fuzzDecodeTarget over the five wire structures
// of its codec table -- extension, key package, mls message, proposal, welcome -- and its own
// TestFuzzTargetsCoverEveryKind asserts the count is nine and that each covers a table entry, so
// neither of these two can join it. Committing seeds under a target name no plan declares is
// exactly the defect p4's review found and fixed: 287 files no fuzz engine ever opened, every
// property stated over bytes nothing consumed, and a verification step that could not be run. The
// compose findings' own accounting agrees, listing "p5's two" among the Fuzz functions of this
// package outside p8's nine.
//
// So this file owns both halves, exactly as key_schedule_roundtrip_test.go does: the corpus lives
// under testdata/corpus/<Target> where addSeedCorpus looks, and the two targets that read it are
// declared at the foot of this file. TestEveryCommittedCorpusFolderIsReadByAFuzzTarget is what
// keeps that true.
//
// WHAT IS REUSED. Everything. The three properties p4's task 26 split apart -- the committed corpus
// IS the generated corpus, every committed seed re-encodes to its own bytes, every generated value
// is recovered by decoding its encoding -- plus the four derived coverage gates and the binary pin,
// are stated once over the seedCodecs table and these two codecs are entries in it. A second
// harness would be a second place for each of those properties to be weaker.
//
// WHAT HAD TO BE ADDED FOR THESE TWO. Two things, both because a ratchet tree is not shaped like a
// group context.
//
//   - RatchetTree's node array is UNEXPORTED, so the derived walks -- which fail rather than skip on
//     a field they cannot read, on purpose -- have nothing to walk. The codec entry supplies a
//     PROJECTION instead: the tree's own Get accessor, one *Node per index. It adds nothing the
//     container does not publish and it is not a second codec.
//   - A field can be single valued across the corpus because the CODEC pins it rather than because
//     the corpus is thin. Credential.CredentialType is the case: both halves of credential.go
//     refuse anything but CredentialTypeBasic, so no decodable seed can carry another value. The
//     variation gate now asks the codec rather than holding a list of exceptions -- see
//     seedFieldIsPinnedByTheCodec.
//
// WHAT THIS HARNESS CANNOT SEE, MEASURED RATHER THAN REASONED ABOUT. A wire format change that is
// regenerated into the golden corpus in the same commit is invisible to every property here. Both
// halves of UpdatePath's codec were swapped to write and to read the nodes vector before the leaf,
// all 43 update_path seeds were regenerated with URMSG_MLS_WRITE_CORPUS=1, and every corpus property
// in this file stayed green: the committed corpus is exactly the generated one, every seed
// re-encodes to its own bytes, every generated value is recovered by decoding its encoding, and all
// four derived coverage gates pass. The 14 tests that did fail are the vendored vector and hand
// derived golden ones elsewhere in the package -- TestUpdatePathMarshalMatchesTheHandDerivedGolden,
// TestVectorTreeKEM, TestVectorFamiliesVerify,
// TestEveryPublishedUpdatePathDecodesAndReEncodesExactly and ten others. So the thing that catches a
// silent format change is the KAT layer and not this harness, and a structure with no published
// vector would have none of that protection: a corpus is evidence about a DECODER against yesterday's
// bytes, and a corpus rewritten in the same commit is yesterday's bytes no longer. It is written down
// here so the next plan does not expect this file to carry a property it cannot. One thing the same
// run did show working: TestTheCommittedSeedCorpusIsExactlyTheGeneratedCorpus fails its own rewrite
// invocation rather than reporting success.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The two target names. p8's own matrix in the compose findings already lists both, which is where
// these spellings come from rather than from a choice made here.
const (
	ratchetTreeSeedTarget = "FuzzRatchetTreeDecode"
	updatePathSeedTarget  = "FuzzUpdatePathDecode"
)

// ---------------------------------------------------------------------------
// the leaf axis
// ---------------------------------------------------------------------------

// seedRegistryVector is one registry's whole declared class as a wire vector, plus one code point
// that is NOT in it.
//
// Derived from the package's own constant declarations rather than listed, for rule 5's reason and
// for a specific one here: a LeafNode's Capabilities carry five registries, and a corpus that named
// the code points somebody remembered would leave the fuzzer blind to every decoder arm added
// afterwards. The unregistered 0xffff is there for the opposite reason, and it is p4's precedent:
// the codec does not decide policy, so an unregistered code point has to survive a round trip for
// validation to be able to refuse it by name.
func seedRegistryVector[T ~uint16](t *testing.T, typeName string) []T {
	t.Helper()
	values := sortedValues(registryConstantsOfType(t, typeName))
	out := make([]T, 0, len(values)+1)
	for _, value := range values {
		out = append(out, T(value))
	}
	return append(out, T(0xffff))
}

// seedEveryDeclaredCapability lists every code point of all five registries a Capabilities carries.
func seedEveryDeclaredCapability(t *testing.T) Capabilities {
	t.Helper()
	return Capabilities{
		Versions:     seedRegistryVector[ProtocolVersion](t, "ProtocolVersion"),
		CipherSuites: seedRegistryVector[CipherSuite](t, "CipherSuite"),
		Extensions:   seedRegistryVector[ExtensionType](t, "ExtensionType"),
		Proposals:    seedRegistryVector[ProposalType](t, "ProposalType"),
		Credentials:  seedRegistryVector[CredentialType](t, "CredentialType"),
	}
}

// seedExtensions is a deterministic extensions vector of the requested arity, cycling the declared
// extension types and the varint width boundaries through the bodies.
func seedExtensions(t *testing.T, count int) []Extension {
	t.Helper()
	if count == 0 {
		return nil
	}
	types := sortedValues(registryConstantsOfType(t, "ExtensionType"))
	lengths := seedOpaqueLengths()
	out := make([]Extension, 0, count)
	for index := 0; index < count; index += 1 {
		length := lengths[index%len(lengths)]
		var body []byte
		if length > 0 {
			body = repeatByte(byte(0x30+index), length)
		}
		out = append(out, Extension{
			ExtensionType: ExtensionType(types[index%len(types)]),
			ExtensionData: body,
		})
	}
	return out
}

// seedLeafVariant is one LeafNode the corpus carries, named so a failure says which shape moved.
type seedLeafVariant struct {
	name string
	leaf func(t *testing.T) *LeafNode
}

// seedNarrowLeafVariants are the leaf shapes the cross product below cycles through.
//
// The axes they cross, and why each is here rather than being left to the fuzzer:
//
//   - leaf_node_source, all three. It is a SUM TYPE the Go struct does not express: marshalCore
//     writes lifetime under key_package, nothing under update and parent_hash under commit, so the
//     three sources are three different wire grammars and a corpus holding one seeds a fuzzer that
//     has never seen the other two.
//   - the varint width boundaries of section 2.1.2 -- absent, one octet, the last length a one
//     octet prefix expresses, the first that needs two -- spread across the opaque fields, because
//     that prefix is what p1 measured random bytes failing at.
//   - the uint64 boundaries of the lifetime, which are the values a field narrowed to 32 bits or
//     read as signed would move. A leaf's lifetime is inside its signature, so a value that moves
//     is a member whose leaf verifies for a window nobody agreed to.
//   - vector arity, from zero up, for the five capability vectors and the extensions vector. Zero
//     and one agree with every reading of a length prefix; two is where a prefix counting BYTES
//     and one counting ELEMENTS part company.
//   - every declared code point of all five capability registries, derived.
//
// The wide leaf is NOT here: it carries a 16 KiB signature, and one seed per cross product entry
// carrying it would be megabytes of repository for a property of the prefix rather than of the
// field in front of it. It is added once, per corpus, below.
func seedNarrowLeafVariants() []seedLeafVariant {
	return []seedLeafVariant{
		{
			// every opaque absent and every vector empty. The smallest thing this grammar
			// accepts, which is the seed a fuzzer mutates outward from.
			name: "update/empty",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					Credential:     Credential{CredentialType: CredentialTypeBasic},
					LeafNodeSource: LeafNodeSourceUpdate,
				}
			},
		},
		{
			name: "update/every-registry",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					EncryptionKey:  repeatByte(0xa1, 32),
					SignatureKey:   repeatByte(0xa2, 32),
					Credential:     Credential{CredentialType: CredentialTypeBasic, Identity: repeatByte(0xa3, 63)},
					Capabilities:   seedEveryDeclaredCapability(t),
					LeafNodeSource: LeafNodeSourceUpdate,
					Extensions:     seedExtensions(t, 1),
					Signature:      repeatByte(0xa4, 64),
				}
			},
		},
		{
			name: "keypackage/lifetime-low",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					EncryptionKey: repeatByte(0xb1, 1),
					SignatureKey:  repeatByte(0xb2, 63),
					Credential:    Credential{CredentialType: CredentialTypeBasic, Identity: repeatByte(0xb3, 1)},
					Capabilities: Capabilities{
						Versions: []ProtocolVersion{ProtocolVersionMls10},
					},
					LeafNodeSource: LeafNodeSourceKeyPackage,
					Lifetime:       Lifetime{NotBefore: 0, NotAfter: 1},
					Signature:      repeatByte(0xb4, 1),
				}
			},
		},
		{
			name: "keypackage/lifetime-uint32-boundary",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					EncryptionKey: repeatByte(0xc1, 64),
					SignatureKey:  repeatByte(0xc2, 64),
					Credential:    Credential{CredentialType: CredentialTypeBasic, Identity: repeatByte(0xc3, 64)},
					Capabilities: Capabilities{
						Versions:     []ProtocolVersion{ProtocolVersionMls10, 0xffff},
						CipherSuites: []CipherSuite{CipherSuiteX25519ChaCha20Sha256Ed25519},
					},
					LeafNodeSource: LeafNodeSourceKeyPackage,
					Lifetime:       Lifetime{NotBefore: math.MaxUint32, NotAfter: uint64(math.MaxUint32) + 1},
					Extensions:     seedExtensions(t, 2),
					Signature:      repeatByte(0xc4, 63),
				}
			},
		},
		{
			name: "keypackage/lifetime-sign-bit",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					EncryptionKey:  repeatByte(0xd1, 63),
					SignatureKey:   repeatByte(0xd2, 1),
					Credential:     Credential{CredentialType: CredentialTypeBasic, Identity: repeatByte(0xd3, 32)},
					LeafNodeSource: LeafNodeSourceKeyPackage,
					Lifetime:       Lifetime{NotBefore: 1 << 63, NotAfter: math.MaxUint64},
					Extensions:     seedExtensions(t, 3),
					Signature:      repeatByte(0xd4, 32),
				}
			},
		},
		{
			name: "commit/parent-hash-one-octet",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					EncryptionKey: repeatByte(0xe1, 32),
					SignatureKey:  repeatByte(0xe2, 32),
					Credential:    Credential{CredentialType: CredentialTypeBasic},
					Capabilities: Capabilities{
						Proposals:   []ProposalType{ProposalTypeReserved, ProposalTypeAdd},
						Credentials: []CredentialType{CredentialTypeBasic},
					},
					LeafNodeSource: LeafNodeSourceCommit,
					ParentHash:     repeatByte(0xe3, 1),
					Signature:      repeatByte(0xe4, 63),
				}
			},
		},
		{
			name: "commit/parent-hash-digest",
			leaf: func(t *testing.T) *LeafNode {
				return &LeafNode{
					EncryptionKey:  repeatByte(0xf1, 1),
					SignatureKey:   repeatByte(0xf2, 64),
					Credential:     Credential{CredentialType: CredentialTypeBasic, Identity: repeatByte(0xf3, 63)},
					Capabilities:   Capabilities{Extensions: []ExtensionType{ExtensionTypeRatchetTree}},
					LeafNodeSource: LeafNodeSourceCommit,
					ParentHash:     repeatByte(0xf4, 32),
					Extensions:     seedExtensions(t, 4),
					Signature:      repeatByte(0xf5, 64),
				}
			},
		},
	}
}

// seedWideLeaf carries the first length whose varint prefix needs four octets, which is
// seedWideOpaqueLength and is the axis key_schedule_roundtrip_test.go already states the cost of.
func seedWideLeaf(t *testing.T) *LeafNode {
	t.Helper()
	return &LeafNode{
		EncryptionKey:  repeatByte(0x11, 32),
		SignatureKey:   repeatByte(0x12, 32),
		Credential:     Credential{CredentialType: CredentialTypeBasic, Identity: repeatByte(0x13, 1)},
		LeafNodeSource: LeafNodeSourceCommit,
		ParentHash:     repeatByte(0x14, 63),
		Signature:      repeatByte(0x15, seedWideOpaqueLength),
	}
}

// ---------------------------------------------------------------------------
// the parent axis
// ---------------------------------------------------------------------------

// seedParentVariants cross the parent node's three fields: the encryption key and the parent hash
// over the varint width boundaries, and unmerged_leaves over its arity.
//
// unmerged_leaves is the field with the most to say. The codec refuses a vector that is not
// STRICTLY ascending on both sides, so a corpus carrying only the empty one seeds a fuzzer that has
// never seen the ordering rule hold, let alone break; and the values are LEAF indices carried as
// uint32, which section 7.9.2 later equates against a resolution of NODE indices.
func seedParentVariants() []*ParentNode {
	return []*ParentNode{
		{},
		{
			EncryptionKey:  repeatByte(0x21, 32),
			ParentHash:     repeatByte(0x22, 32),
			UnmergedLeaves: []LeafIndex{0},
		},
		{
			EncryptionKey:  repeatByte(0x23, 1),
			ParentHash:     repeatByte(0x24, 63),
			UnmergedLeaves: []LeafIndex{0, 1, 3},
		},
		{
			EncryptionKey:  repeatByte(0x25, 64),
			ParentHash:     repeatByte(0x26, 64),
			UnmergedLeaves: []LeafIndex{2, 5},
		},
	}
}

// ---------------------------------------------------------------------------
// the tree shapes
// ---------------------------------------------------------------------------

// seedTreeShapes are the node arrays the corpus carries, one character per node index: L a leaf, P
// a parent, and a full stop a blank.
//
// Written as strings so the SHAPE is legible and so the parity rule -- leaves at even indices,
// parents at odd -- can be checked against the tree math rather than trusted, which
// seedRatchetTreeFromShape does on every build. Every width here is 2n-1 for a power-of-two n,
// because readNodeArray refuses anything else.
//
// The shapes cross what a decoder branches on: the width, whether the first leaf is blank, whether
// a parent is blank, whether the array ENDS in a blank -- that last one is ValSem300, the rule that
// the encoder strips trailing blanks and the decoder refuses an array that carries one, so the
// "LPLPLP." row is the only one whose committed bytes are shorter than its node width.
func seedTreeShapes() []string {
	return []string{
		"L",
		"LPL",
		".PL",
		"L.L",
		"LPLPLPL",
		"L..P..L",
		"LPLPLP.",
		"LPLPLPLPLPLPLPL",
		"L.L.L.LPL.L.L.L",
		"..LPL...LPLPLPL",
	}
}

// seedRatchetTreeFromShape builds one tree, taking its leaves and parents from the variant cycles
// starting at offset.
//
// The parity of every position is asserted against NodeIndex.IsLeaf rather than against the string,
// because the string is what this function is being told and the arithmetic is what the decoder
// will judge it by. A shape with a leaf at an odd index encodes fine and is refused by
// readNodeArray, which would show up as an unexplained corpus that does not decode.
func seedRatchetTreeFromShape(t *testing.T, shape string, offset int, leaves []seedLeafVariant) *RatchetTree {
	t.Helper()
	parents := seedParentVariants()
	nodes := make([]*Node, len(shape))
	leafSlot, parentSlot := 0, 0
	for index, token := range shape {
		x := NodeIndex(index)
		switch token {
		case 'L':
			if !x.IsLeaf() {
				t.Fatalf("shape %q puts a leaf at node %d, which is a parent position", shape, index)
			}
			nodes[index] = &Node{
				NodeType: NodeTypeLeaf,
				Leaf:     leaves[(offset+leafSlot)%len(leaves)].leaf(t),
			}
			leafSlot += 1
		case 'P':
			if x.IsLeaf() {
				t.Fatalf("shape %q puts a parent at node %d, which is a leaf position", shape, index)
			}
			nodes[index] = &Node{
				NodeType: NodeTypeParent,
				Parent:   parents[(offset+parentSlot)%len(parents)].Clone(),
			}
			parentSlot += 1
		case '.':
		default:
			t.Fatalf("shape %q holds %q, which is not one of L, P or .", shape, token)
		}
	}
	if _, err := LeafCountFromNodeWidth(uint32(len(nodes))); err != nil {
		t.Fatalf("shape %q is %d nodes wide, which is not 2n-1 for any n: %v", shape, len(nodes), err)
	}
	return &RatchetTree{nodes: nodes}
}

// seedRatchetTrees is the cross product: every shape against every rotation of the leaf and parent
// variant cycles, plus the one wide seed.
func seedRatchetTrees(t *testing.T) []*RatchetTree {
	t.Helper()
	leaves := seedNarrowLeafVariants()
	trees := []*RatchetTree{}
	for _, shape := range seedTreeShapes() {
		for offset := 0; offset < len(leaves); offset += 1 {
			trees = append(trees, seedRatchetTreeFromShape(t, shape, offset, leaves))
		}
	}
	// the four octet varint prefix, once. The tree around it is the smallest one that still holds a
	// parent, so the seed is the wide field and almost nothing else.
	wide := &RatchetTree{nodes: []*Node{
		{NodeType: NodeTypeLeaf, Leaf: seedWideLeaf(t)},
		{NodeType: NodeTypeParent, Parent: seedParentVariants()[1].Clone()},
		{NodeType: NodeTypeLeaf, Leaf: seedNarrowLeafVariants()[0].leaf(t)},
	}}
	return append(trees, wide)
}

// ---------------------------------------------------------------------------
// the update path shapes
// ---------------------------------------------------------------------------

// seedUpdatePathShapes are the ciphertext counts per path node: the outer vector's arity is the
// length of the row, and each entry is the arity of that node's own ciphertext vector.
//
// Three levels of nesting is the whole difficulty of this structure, and it is why the rows below
// are not all the same width: an UpdatePath holds a vector of nodes, each node holds a vector of
// HPKE ciphertexts, and each ciphertext holds two opaques. A decoder that read the inner arity as a
// byte count, or the outer one as an element count, agrees with itself on every row of arity zero
// and one and parts company at the first row that has two of anything.
func seedUpdatePathShapes() [][]int {
	return [][]int{
		{},
		{0},
		{1},
		{0, 1, 2},
		{2, 3},
		{1, 1, 1, 1},
	}
}

// seedUpdatePathFromShape builds one path, cycling the opaque lengths through the ciphertexts so
// the varint width boundaries appear at every level of the nesting rather than only at the top.
func seedUpdatePathFromShape(t *testing.T, shape []int, offset int, leaves []seedLeafVariant) *UpdatePath {
	t.Helper()
	lengths := seedOpaqueLengths()
	path := &UpdatePath{LeafNode: *leaves[offset%len(leaves)].leaf(t)}
	for index, ciphertexts := range shape {
		node := UpdatePathNode{}
		if keyLength := lengths[(offset+index)%len(lengths)]; keyLength > 0 {
			node.EncryptionKey = repeatByte(byte(0x40+index), keyLength)
		}
		for which := 0; which < ciphertexts; which += 1 {
			kemLength := lengths[(offset+index+which)%len(lengths)]
			ciphertextLength := lengths[(offset+index+which+1)%len(lengths)]
			one := HpkeCiphertext{}
			if kemLength > 0 {
				one.KemOutput = repeatByte(byte(0x50+which), kemLength)
			}
			if ciphertextLength > 0 {
				one.Ciphertext = repeatByte(byte(0x60+which), ciphertextLength)
			}
			node.EncryptedPathSecret = append(node.EncryptedPathSecret, one)
		}
		path.Nodes = append(path.Nodes, node)
	}
	return path
}

func seedUpdatePaths(t *testing.T) []*UpdatePath {
	t.Helper()
	leaves := seedNarrowLeafVariants()
	paths := []*UpdatePath{}
	for _, shape := range seedUpdatePathShapes() {
		for offset := 0; offset < len(leaves); offset += 1 {
			paths = append(paths, seedUpdatePathFromShape(t, shape, offset, leaves))
		}
	}
	// the four octet varint prefix, once, on the innermost field: a ciphertext rather than the
	// leaf's signature, so the wide length is reached through all three levels of the nesting.
	wide := &UpdatePath{
		LeafNode: *seedNarrowLeafVariants()[0].leaf(t),
		Nodes: []UpdatePathNode{{
			EncryptionKey: repeatByte(0x71, 32),
			EncryptedPathSecret: []HpkeCiphertext{{
				KemOutput:  repeatByte(0x72, 32),
				Ciphertext: repeatByte(0x73, seedWideOpaqueLength),
			}},
		}},
	}
	return append(paths, wide)
}

// ---------------------------------------------------------------------------
// the two codec table entries
// ---------------------------------------------------------------------------

// ratchetTreeNodes is the PROJECTION the derived walks read a tree through: one *Node per index,
// through the container's own accessor, a typed nil for a blank.
//
// It exists because RatchetTree.nodes is unexported and the walks in
// key_schedule_roundtrip_test.go fail rather than skip on a field they cannot read -- which is the
// right behaviour, since a field a comparison cannot read is a field it reports equal whatever it
// holds. What this hands them is exactly what the container publishes and nothing more: Get is the
// same accessor every other reader of a tree uses, and it answers nil for a blank and for an index
// past the end alike.
//
// The values are the tree's OWN nodes rather than copies, which matters for
// seedFieldIsPinnedByTheCodec: that probe perturbs a field of a decoded value and re-encodes it, so
// it needs the walk to reach the storage the encoder will read.
func ratchetTreeNodes(value any) []any {
	tree := value.(*RatchetTree)
	out := make([]any, 0, tree.NodeWidth())
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := tree.Get(NodeIndex(x))
		out = append(out, node)
	}
	return out
}

// updatePathItself is the identity projection. It is spelled out rather than left nil so that the
// probe below reaches an UpdatePath through the same code path it reaches a tree through, and a
// change to one is a change to both.
func updatePathItself(value any) []any {
	return []any{value.(*UpdatePath)}
}

func describeRatchetTree(tree *RatchetTree) string {
	if tree == nil {
		return "<nil tree>"
	}
	leaves, parents, blanks := 0, 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := tree.Get(NodeIndex(x))
		switch {
		case node == nil:
			blanks += 1
		case node.Leaf != nil:
			leaves += 1
		default:
			parents += 1
		}
	}
	return fmt.Sprintf("tree of %d nodes: %d leaves, %d parents, %d blank",
		tree.NodeWidth(), leaves, parents, blanks)
}

// describeUpdatePath is treekem_test.go, and it is reused rather than restated: that one spells all
// three levels of the nesting out, which is what a corpus failure has to say.

// treeSeedCodecs is this plan's half of the shared seedCodecs table.
//
// The ratchet tree entry runs at syntax.MaxRatchetTreeLength on BOTH halves, which is not an
// optimisation and is stated in syntax.CheckRoundTripLimit's own comment: a structure that decodes
// only under the raised bound does not decode under the default one, and CheckRoundTrip's contract
// for an input that does not decode is to return nil. A ratchet tree target built on the default
// bound would be not merely mostly vacuous but entirely so, and indistinguishable from one that
// found nothing wrong.
// checkRatchetTreeRoundTrip is the round-trip property for a ratchet tree, at the raised bound, in
// the ONE place this package spells it.
//
// It was spelled twice -- once in the codec table below and once in FuzzRatchetTreeDecode -- and
// nothing kept the two in step. Both dropped to syntax.CheckRoundTrip's default bound without a
// single test moving, because the substitution has no observable behaviour to catch: for a body the
// default bound cannot decode, CheckRoundTrip's contract is to return nil, so the vacuous form and
// the working form answer the same thing on every input. What it costs is the whole large-tree
// region -- above one mebibyte the target stops decoding anything at all -- and
// TestTheRatchetTreeCodecIsHandedTheRaisedLimitAtTheProductsGroupSize in tree_test.go is where that
// region is shown to be this product's own group rather than a hypothetical one.
//
// Since no input can tell the two apart, the pin is over the SOURCE and it is
// TestEveryRatchetTreeCodecCallInThisPackageRunsAtTheRaisedBound below.
func checkRatchetTreeRoundTrip(bs []byte) error {
	return syntax.CheckRoundTripLimit[RatchetTree, *RatchetTree](bs, syntax.MaxRatchetTreeLength)
}

func treeSeedCodecs() []seedCodec {
	return []seedCodec{
		{
			target:    ratchetTreeSeedTarget,
			structure: func() any { return &Node{} },
			project:   ratchetTreeNodes,
			values: func(t *testing.T) []any {
				values := []any{}
				for _, value := range seedRatchetTrees(t) {
					values = append(values, value)
				}
				return values
			},
			decode: func(bs []byte) (any, error) {
				return UnmarshalRatchetTree(bs)
			},
			encode:         func(value any) ([]byte, error) { return marshalRatchetTree(value.(*RatchetTree)) },
			checkRoundTrip: checkRatchetTreeRoundTrip,
			describe:       func(value any) string { return describeRatchetTree(value.(*RatchetTree)) },
		},
		{
			target:    updatePathSeedTarget,
			structure: func() any { return &UpdatePath{} },
			project:   updatePathItself,
			values: func(t *testing.T) []any {
				values := []any{}
				for _, value := range seedUpdatePaths(t) {
					values = append(values, value)
				}
				return values
			},
			decode: func(bs []byte) (any, error) {
				parsed := &UpdatePath{}
				return parsed, syntax.Unmarshal(bs, parsed)
			},
			encode:         func(value any) ([]byte, error) { return syntax.Marshal(value.(*UpdatePath)) },
			checkRoundTrip: syntax.CheckRoundTrip[UpdatePath, *UpdatePath],
			describe:       func(value any) string { return describeUpdatePath(value.(*UpdatePath)) },
		},
	}
}

// ---------------------------------------------------------------------------
// the two properties the plan names
// ---------------------------------------------------------------------------

// TestRatchetTreeDecodeIsRoundTripStable is round-trip stability over the committed ratchet tree
// corpus, deterministically and in every go test run rather than only under -fuzz.
//
// It states the plan's own two claims -- the canonical re-encoding decodes, and encoding it again
// gives the same bytes -- and adds the one the plan's version could not make, which is a count. A
// loop whose body never ran reports exactly what a loop that checked every seed reports, and on
// this project that is not hypothetical: three CheckRoundTrip tests once passed against a helper
// that evaluated the comparison, discarded the result and returned nil. So the decodes are counted
// and zero is fatal.
//
// It overlaps TestEverySeedInTheCommittedCorpusReEncodesToItsOwnBytes deliberately and adds two
// things that property does not say: that the tree's WIDTH survives the round trip, which is the
// one field of a ratchet tree that is not written down anywhere on the wire -- readNodeArray
// derives it by extension from the entry count -- and that the SECOND encoding is stable, which is
// the difference between "encode of decode is canonical" and "decode is a function".
func TestRatchetTreeDecodeIsRoundTripStable(t *testing.T) {
	names, onDisk := readSeedCorpus(t, ratchetTreeSeedTarget)
	decoded := 0
	for _, name := range names {
		data := onDisk[name]
		tree, err := UnmarshalRatchetTree(data)
		if err != nil {
			t.Errorf("%s: the corpus holds only accepted inputs: %v", name, err)
			continue
		}
		decoded += 1
		encoded, err := marshalRatchetTree(tree)
		if err != nil {
			t.Errorf("%s: a decoded tree failed to re-encode: %v", name, err)
			continue
		}
		if !bytes.Equal(encoded, data) {
			t.Errorf("%s: the decoder accepted a non-canonical encoding of %s:\n     got %x\ncommitted %x",
				name, describeRatchetTree(tree), encoded, data)
			continue
		}
		again, err := UnmarshalRatchetTree(encoded)
		if err != nil {
			t.Errorf("%s: the canonical re-encoding failed to decode: %v", name, err)
			continue
		}
		reencoded, err := marshalRatchetTree(again)
		if err != nil {
			t.Errorf("%s: re-encode: %v", name, err)
			continue
		}
		if !bytes.Equal(encoded, reencoded) {
			t.Errorf("%s: encoding is not stable across a second round trip", name)
			continue
		}
		if again.NodeWidth() != tree.NodeWidth() {
			t.Errorf("%s: node width changed across a round trip: %d then %d",
				name, tree.NodeWidth(), again.NodeWidth())
		}
	}
	if decoded == 0 {
		t.Fatalf("not one of the %d committed seeds decoded, so this property reached nothing; a corpus of bytes no decoder accepts makes every claim over it trivially true",
			len(names))
	}
	if decoded != len(names) {
		t.Errorf("%d of %d committed seeds decoded", decoded, len(names))
	}
	t.Logf("%d ratchet tree seeds are round trip stable", decoded)
}

// TestUpdatePathDecodeIsRoundTripStable is the same property over the update path corpus, plus the
// node count, which is the arity the three levels of nesting make it possible to lose one level
// down without changing the length of anything.
func TestUpdatePathDecodeIsRoundTripStable(t *testing.T) {
	names, onDisk := readSeedCorpus(t, updatePathSeedTarget)
	decoded := 0
	for _, name := range names {
		data := onDisk[name]
		path := &UpdatePath{}
		if err := syntax.Unmarshal(data, path); err != nil {
			t.Errorf("%s: the corpus holds only accepted inputs: %v", name, err)
			continue
		}
		decoded += 1
		encoded, err := syntax.Marshal(path)
		if err != nil {
			t.Errorf("%s: a decoded update path failed to re-encode: %v", name, err)
			continue
		}
		if !bytes.Equal(encoded, data) {
			t.Errorf("%s: the decoder accepted a non-canonical encoding of %s:\n     got %s\ncommitted %s",
				name, describeUpdatePath(path), HexOf(encoded), HexOf(data))
			continue
		}
		again := &UpdatePath{}
		if err := syntax.Unmarshal(encoded, again); err != nil {
			t.Errorf("%s: the canonical re-encoding failed to decode: %v", name, err)
			continue
		}
		reencoded, err := syntax.Marshal(again)
		if err != nil {
			t.Errorf("%s: re-encode: %v", name, err)
			continue
		}
		if !bytes.Equal(encoded, reencoded) {
			t.Errorf("%s: encoding is not stable across a second round trip", name)
			continue
		}
		if len(again.Nodes) != len(path.Nodes) {
			t.Errorf("%s: node count changed across a round trip: %d then %d",
				name, len(path.Nodes), len(again.Nodes))
		}
		for index := range path.Nodes {
			if len(again.Nodes[index].EncryptedPathSecret) != len(path.Nodes[index].EncryptedPathSecret) {
				t.Errorf("%s: node %d's ciphertext count changed across a round trip: %d then %d",
					name, index, len(path.Nodes[index].EncryptedPathSecret),
					len(again.Nodes[index].EncryptedPathSecret))
			}
		}
	}
	if decoded == 0 {
		t.Fatalf("not one of the %d committed seeds decoded, so this property reached nothing", len(names))
	}
	if decoded != len(names) {
		t.Errorf("%d of %d committed seeds decoded", decoded, len(names))
	}
	t.Logf("%d update path seeds are round trip stable", decoded)
}

// ---------------------------------------------------------------------------
// the targets that read these two corpora
// ---------------------------------------------------------------------------

// FuzzRatchetTreeDecode is Gate 4 properties 1 and 2 on the section 12.4.3.3 ratchet_tree body in
// their randomized form: no panic on adversarial input, and an encoding that decodes must re-encode
// to the bytes it came from.
//
// Through checkRatchetTreeRoundTrip, which is CheckRoundTripLimit at MaxRatchetTreeLength and is
// the whole difference between this target and a vacuous one. It is a call to the shared helper
// rather than a second spelling of the bound for the reason that helper's comment gives: this was
// the second spelling, and both copies dropped the bound with the suite still green.
func FuzzRatchetTreeDecode(f *testing.F) {
	fuzzTheCommittedSeedCorpus(f, ratchetTreeSeedTarget, func(t *testing.T, encoded []byte) bool {
		if err := checkRatchetTreeRoundTrip(encoded); err != nil {
			t.Fatalf("%d octets %x: %v", len(encoded), encoded, err)
		}
		// through UnmarshalRatchetTree and not syntax.Unmarshal, because it is the entry point that
		// raises the vector bound to MaxRatchetTreeLength. The default one refuses trees this
		// target's own corpus carries, and a reachability count taken through it would report zero
		// for a target that had reached every seed.
		_, err := UnmarshalRatchetTree(encoded)
		return err == nil
	})
}

// FuzzUpdatePathDecode is the same two properties on the section 7.6 UpdatePath. A separate target
// rather than a second case of one, for the reason p4 states: the fuzzing engine keeps its coverage
// feedback and its found corpus per target, so one target over two grammars spends half its budget
// on each while reporting one.
func FuzzUpdatePathDecode(f *testing.F) {
	fuzzTheCommittedSeedCorpus(f, updatePathSeedTarget, func(t *testing.T, encoded []byte) bool {
		if err := syntax.CheckRoundTrip[UpdatePath, *UpdatePath](encoded); err != nil {
			t.Fatalf("%d octets %x: %v", len(encoded), encoded, err)
		}
		return syntax.Unmarshal(encoded, &UpdatePath{}) == nil
	})
}

// syntaxInstantiationsAt answers every place this file instantiates a generic entry point of the
// syntax package at the given type: rendered as the whole CALL where it is called, and as the bare
// instantiation where it is used as a value.
//
// Type arguments rather than value arguments, because the type argument is what selects the codec
// and the bound is an ordinary argument -- and an ordinary argument going missing is the whole
// failure this exists to find.
//
// Both forms, and that is not thoroughness for its own sake: it is what a mutation found. The first
// version of this matcher looked only at a CallExpr's Fun, and the codec table entry below is not a
// call -- it is `checkRoundTrip: checkRatchetTreeRoundTrip`, a function VALUE -- so substituting
// syntax.CheckRoundTrip[RatchetTree, *RatchetTree] for it left this gate green over exactly the
// spelling it exists to pin. A bare instantiation can carry no bound argument at all, so it belongs
// to the class rather than being excused from it.
//
// It is a second matcher beside callsToPackage rather than a widening of it, and the reason is the
// reason these sites were never pinned: callsToPackage matches a call whose Fun is a SelectorExpr,
// while an instantiated generic's Fun is an IndexExpr or an IndexListExpr, so
// TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit -- whose list is otherwise every way this
// package enters the codec -- has never seen one.
func (self parsedSource) syntaxInstantiationsAt(typeName string) []string {
	// which identifiers this file spells the syntax package with, derived off its own imports
	// for the reason namesOfImportedPackage states: a renamed import spells the same generic
	// entry point under a different first identifier, and a matcher keyed on the literal name
	// reports it as no instantiation at all.
	names, dotImported := self.namesOfImportedPackage("syntax")
	// which instantiations are a call's function, so that a called one is reported as the call
	// that carries its bound rather than as the half of it that cannot.
	enclosing := map[ast.Node]*ast.CallExpr{}
	ast.Inspect(self.file, func(node ast.Node) bool {
		if call, isCall := node.(*ast.CallExpr); isCall {
			enclosing[call.Fun] = call
		}
		return true
	})
	found := []string{}
	if dotImported {
		found = append(found, "syntax"+packageAliasMarker)
	}
	ast.Inspect(self.file, func(node ast.Node) bool {
		var instantiated ast.Expr
		var arguments []ast.Expr
		switch generic := node.(type) {
		case *ast.IndexExpr:
			instantiated, arguments = generic.X, []ast.Expr{generic.Index}
		case *ast.IndexListExpr:
			instantiated, arguments = generic.X, generic.Indices
		default:
			return true
		}
		selector, isSelector := instantiated.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		base, isIdentifier := selector.X.(*ast.Ident)
		if !isIdentifier || !slices.Contains(names, base.Name) {
			return true
		}
		if !expressionsName(arguments, typeName) {
			return true
		}
		// rendered under the package's own name whichever identifier the file spelled it
		// with, so that the entry a gate holds against a list is the entry point and not the
		// import alias in front of it
		if call, isCalled := enclosing[node]; isCalled {
			found = append(found, "syntax"+strings.TrimPrefix(self.render(call), base.Name))
			return true
		}
		found = append(found, "syntax"+strings.TrimPrefix(self.render(node), base.Name))
		return true
	})
	slices.Sort(found)
	return found
}

// expressionsName reports whether any of these expressions mentions the named identifier, which
// asked of a type argument list is the question "is this instantiated at that type".
func expressionsName(expressions []ast.Expr, name string) bool {
	for _, expression := range expressions {
		named := false
		ast.Inspect(expression, func(inner ast.Node) bool {
			if identifier, isIdentifier := inner.(*ast.Ident); isIdentifier && identifier.Name == name {
				named = true
			}
			return true
		})
		if named {
			return true
		}
	}
	return false
}

// TestEveryRatchetTreeCodecCallInThisPackageRunsAtTheRaisedBound is the pin on the one fact about
// this plan's ratchet tree target that no input can observe.
//
// A ratchet tree is the only structure in this package whose codec runs above MaxVectorLength, and
// this product's own group is in the region that raise buys: a thousand leaves each carrying a 1216
// byte X-Wing key encode to about 1.33 MiB. Below that boundary the raised bound and the default one
// agree on every byte string, and above it CheckRoundTrip does not decode and so -- by its own
// documented contract for an input that does not decode -- returns nil. So the difference between
// this plan's target and one that checks nothing at all is invisible to every behavioural test that
// could be written, which is exactly why the bound was dropped from both of its spellings with 6242
// tests still passing.
//
// The class is DERIVED and the derivation is over type arguments: every call anywhere in this
// package, test source included, to a generic syntax entry point instantiated at RatchetTree. A
// list of the two call sites that exist today is the enumeration rule 5 forbids -- the two that
// existed were exactly the two nobody kept in step -- and a third spelled tomorrow joins this gate
// in the commit that writes it.
func TestEveryRatchetTreeCodecCallInThisPackageRunsAtTheRaisedBound(t *testing.T) {
	found, unbounded := []string{}, []string{}
	for _, path := range packageSourcePaths(t) {
		for _, entry := range mustParseSource(t, path).syntaxInstantiationsAt("RatchetTree") {
			found = append(found, path+": "+entry)
			if !strings.Contains(entry, "syntax.MaxRatchetTreeLength") {
				unbounded = append(unbounded, path+": "+entry)
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("this package instantiates no generic syntax entry point at RatchetTree at all, so this gate read nothing; the codec table entry and the round trip helper are both such instantiations, and a gate that has stopped finding its subject must fail rather than report it clean")
	}
	if len(unbounded) > 0 {
		t.Errorf("%d of %d ratchet tree codec instantiations do not name syntax.MaxRatchetTreeLength (%s); at the default bound a body larger than MaxVectorLength does not decode, and the round trip property's answer for an input that does not decode is nil, so what that site states is nothing",
			len(unbounded), len(found), unbounded[0])
	}

	// the matcher on a control, because a matcher that had stopped matching reports the real
	// source clean. Every direction it has to get right: the raised call accepted, the default
	// call refused, the default VALUE refused -- that is the form the codec table uses and the
	// form the first version of this matcher could not see -- and an instantiation over another
	// structure left alone, since it is not this gate's business.
	control := mustParseText(t, "the ratchet tree bound control", ratchetTreeBoundControl)
	entries := control.syntaxInstantiationsAt("RatchetTree")
	if len(entries) != 3 {
		t.Fatalf("the matcher read %v out of a control holding one raised ratchet tree call, one default call, one default value and one instantiation over another structure", entries)
	}
	raised := 0
	for _, entry := range entries {
		if strings.Contains(entry, "syntax.MaxRatchetTreeLength") {
			raised += 1
		}
	}
	if raised != 1 {
		t.Errorf("the matcher read %d of the control's three ratchet tree sites as carrying the raised bound, want 1: %v", raised, entries)
	}
	// and a ratchet tree entry point reached through a RENAMED import is read as the entry point
	// it is, under the package's own name, rather than as no instantiation at all.
	//
	// This matcher keyed on the literal identifier `syntax` for the same reason callsToPackage
	// did, and it is the worse of the two places to have it: a generic instantiation is where the
	// BOUND lives, so an aliased CheckRoundTrip[RatchetTree, ...] with no bound at all is a
	// ratchet tree codec call running at the default limit that this gate would have reported as
	// nothing. The alias survives on the bound argument, which is what makes the renamed raised
	// call below still count as unbounded here -- the safe direction, since it forces the rename
	// to be written down rather than passing silently.
	renamed := mustParseText(t, "the renamed import control", renamedRatchetTreeBoundControl)
	if entries := renamed.syntaxInstantiationsAt("RatchetTree"); !slices.Equal(entries, []string{
		"syntax.CheckRoundTripLimit[RatchetTree, *RatchetTree](bs, sx.MaxRatchetTreeLength)",
	}) {
		t.Errorf("the matcher read %v out of a control entering the ratchet tree codec through a renamed import", entries)
	}
	// and a DOT imported codec is reported as the hole it is rather than as a file that
	// instantiates nothing: a bare CheckRoundTripLimit[RatchetTree, ...] has no package selector
	// for this matcher to match.
	dotted := mustParseText(t, "the dot import control", dottedRatchetTreeBoundControl)
	if entries := dotted.syntaxInstantiationsAt("RatchetTree"); !slices.Equal(entries, []string{
		"syntax" + packageAliasMarker,
	}) {
		t.Errorf("the matcher read %v out of a control that dot imported the codec", entries)
	}
	// the count of raised ones rather than the word "all", because this line prints on the way out
	// of a failure too and a log that contradicts the error above it is worse than no log.
	t.Logf("%d ratchet tree codec instantiations, %d of them at the raised bound: %v",
		len(found), len(found)-len(unbounded), found)
}

// The ratchet tree codec entered through a rename, which is the spelling a matcher keyed on the
// literal package name reads as no instantiation at all.
const renamedRatchetTreeBoundControl = `package mls

import (
	sx "github.com/urnetwork/connect/mls/syntax"
)

func checkRaised(bs []byte) error {
	return sx.CheckRoundTripLimit[RatchetTree, *RatchetTree](bs, sx.MaxRatchetTreeLength)
}
`

// The same entered through a dot import, where there is no selector to match at all.
const dottedRatchetTreeBoundControl = `package mls

import (
	. "github.com/urnetwork/connect/mls/syntax"
)

func checkRaised(bs []byte) error {
	return CheckRoundTripLimit[RatchetTree, *RatchetTree](bs, MaxRatchetTreeLength)
}
`

// A file entering the ratchet tree codec at both bounds, plus one generic syntax call over another
// structure. Every matcher above runs on this as well as on the real source.
const ratchetTreeBoundControl = `package mls

func checkRaised(bs []byte) error {
	return syntax.CheckRoundTripLimit[RatchetTree, *RatchetTree](bs, syntax.MaxRatchetTreeLength)
}

func checkDefault(bs []byte) error {
	return syntax.CheckRoundTrip[RatchetTree, *RatchetTree](bs)
}

func checkAnother(bs []byte) error {
	return syntax.CheckRoundTrip[UpdatePath, *UpdatePath](bs)
}

var checkAsAValue = syntax.CheckRoundTrip[RatchetTree, *RatchetTree]
`

// ---------------------------------------------------------------------------
// the controls on the two things this plan added to the shared harness
// ---------------------------------------------------------------------------

// errSeedProbePinned is the refusal a codec that pins a field answers with.
var errSeedProbePinned = errors.New("mls: this probe codec carries only one code point")

// pinnedSeedProbe is a structure whose first field the codec refuses to carry as anything but 1,
// and whose second it carries freely. Credential is that shape and so is nothing else in this
// package, which is why the control below needs a structure of its own.
type pinnedSeedProbe struct {
	Code uint16
	Body []byte
}

func (self *pinnedSeedProbe) MarshalMLS(w *syntax.Writer) error {
	if self.Code != 1 {
		return errSeedProbePinned
	}
	w.WriteUint16(self.Code)
	w.WriteOpaque(self.Body)
	return nil
}

func (self *pinnedSeedProbe) UnmarshalMLS(r *syntax.Reader) error {
	code, err := r.ReadUint16()
	if err != nil {
		return err
	}
	if code != 1 {
		return errSeedProbePinned
	}
	body, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.Code, self.Body = code, body
	return nil
}

// freeSeedProbe is the same two fields with nothing pinned. It is the half that makes the control a
// control: a probe helper that answered "pinned" unconditionally passes every assertion about
// pinnedSeedProbe and fails every one about this.
type freeSeedProbe struct {
	Code uint16
	Body []byte
}

func (self *freeSeedProbe) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint16(self.Code)
	w.WriteOpaque(self.Body)
	return nil
}

func (self *freeSeedProbe) UnmarshalMLS(r *syntax.Reader) error {
	code, err := r.ReadUint16()
	if err != nil {
		return err
	}
	body, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.Code, self.Body = code, body
	return nil
}

func seedProbeCodec[T any, PT interface {
	*T
	syntax.Codec
}](name string) seedCodec {
	return seedCodec{
		target:    name,
		structure: func() any { return PT(new(T)) },
		decode: func(bs []byte) (any, error) {
			parsed := PT(new(T))
			return parsed, syntax.Unmarshal(bs, parsed)
		},
		encode:   func(value any) ([]byte, error) { return syntax.Marshal(value.(PT)) },
		describe: func(value any) string { return fmt.Sprintf("%+v", value) },
	}
}

// TestTheFieldPinProbeSeparatesAPinnedFieldFromAFreeOne is the positive control on
// seedFieldIsPinnedByTheCodec, and it is here because that helper is an EXEMPTION: it decides which
// single valued fields TestEveryFieldOfBothStructuresVariesAcrossTheCommittedCorpus stops
// reporting, so a version of it that answered "pinned" to everything would retire that gate
// silently.
//
// Nothing else can see that. The gate consults the probe only for fields that are ALREADY single
// valued, and there is exactly one of those per codec today, so a probe stuck at yes agrees with a
// working one on every question the gate asks it -- which is precisely what mutation testing found:
// making the helper return true unconditionally left the whole package green.
//
// So the control drives it against two structures built to differ in the one thing it claims to
// detect, and asserts all four corners: the pinned field of the pinned codec, the free field of the
// SAME codec -- so "pinned" is a statement about the field and not about the structure -- and both
// fields of a codec that pins nothing.
func TestTheFieldPinProbeSeparatesAPinnedFieldFromAFreeOne(t *testing.T) {
	pinnedCodec := seedProbeCodec[pinnedSeedProbe]("pinnedSeedProbe")
	freeCodec := seedProbeCodec[freeSeedProbe]("freeSeedProbe")

	pinnedSeed, err := pinnedCodec.encode(&pinnedSeedProbe{Code: 1, Body: []byte{0x0a}})
	if err != nil {
		t.Fatalf("encode the pinned probe: %v", err)
	}
	freeSeed, err := freeCodec.encode(&freeSeedProbe{Code: 1, Body: []byte{0x0a}})
	if err != nil {
		t.Fatalf("encode the free probe: %v", err)
	}
	if !bytes.Equal(pinnedSeed, freeSeed) {
		t.Fatalf("the two probes encode differently (%x against %x), so they are not the same structure with and without the pin",
			pinnedSeed, freeSeed)
	}

	for _, row := range []struct {
		name      string
		codec     seedCodec
		seed      []byte
		fieldPath string
		want      bool
	}{
		{"the pinned field of the pinned codec", pinnedCodec, pinnedSeed, "pinnedSeedProbe.Code", true},
		{"the free field of the pinned codec", pinnedCodec, pinnedSeed, "pinnedSeedProbe.Body", false},
		{"the code point of the codec that pins nothing", freeCodec, freeSeed, "freeSeedProbe.Code", false},
		{"the body of the codec that pins nothing", freeCodec, freeSeed, "freeSeedProbe.Body", false},
	} {
		got, why := seedFieldIsPinnedOverSeeds(row.codec, [][]byte{row.seed}, row.fieldPath)
		if got != row.want {
			t.Errorf("%s: the probe answered pinned=%v (%s), want %v", row.name, got, why, row.want)
		}
	}
}

// TestTheProjectedComparisonRefusesTwoTreesOfDifferentWidth is the control on the length check in
// seedCodecValuesAgree.
//
// The node width is the one property of a ratchet tree the wire does not carry: readNodeArray
// derives it by extending the entry count to the next complete tree, so a decoder that stopped
// short answers a tree with a different root, a different direct path for every leaf and a
// different tree hash -- and every node it did decode is identical. An elementwise comparison over
// the shorter of the two reports them equal, which is why the length is compared first.
//
// Nothing in the corpus exercises that branch, because no seed round trips to a different width;
// mutation confirmed it, by deleting the check and leaving the package green. The two trees below
// are built from the same builder at the same offset, so they agree node for node over the narrow
// one's whole width and differ in nothing but how many nodes there are.
func TestTheProjectedComparisonRefusesTwoTreesOfDifferentWidth(t *testing.T) {
	codec := treeSeedCodecs()[0]
	if codec.target != ratchetTreeSeedTarget {
		t.Fatalf("this control reads the ratchet tree codec off the table by position and found %s", codec.target)
	}
	leaves := seedNarrowLeafVariants()
	narrow := seedRatchetTreeFromShape(t, "LPL", 0, leaves)
	wide := seedRatchetTreeFromShape(t, "LPLPLPL", 0, leaves)

	if narrow.NodeWidth() >= wide.NodeWidth() {
		t.Fatalf("the two trees are %d and %d nodes wide, so this control has no width difference to see",
			narrow.NodeWidth(), wide.NodeWidth())
	}
	// the premise: over the narrow tree's own width the two agree node for node, so the only thing
	// left for the comparison to notice is the count.
	for x := uint32(0); x < narrow.NodeWidth(); x += 1 {
		at := fmt.Sprintf("control[%d]", x)
		if !seedValuesAgree(t, at, reflect.ValueOf(narrow.Get(NodeIndex(x))), reflect.ValueOf(wide.Get(NodeIndex(x)))) {
			t.Fatalf("node %d already differs between the two trees, so a refusal below would say nothing about the width", x)
		}
	}

	if !seedCodecValuesAgree(t, codec, narrow, narrow.Clone()) {
		t.Fatal("the comparison refuses a tree against its own clone, so its answer below is not about the width either")
	}
	if seedCodecValuesAgree(t, codec, narrow, wide) {
		t.Fatalf("a %d node tree compared equal to a %d node one that agrees with it everywhere it reaches; the node width is the one property of a ratchet tree the wire does not carry, so a comparison that does not check it cannot see a decoder that stopped short",
			narrow.NodeWidth(), wide.NodeWidth())
	}
}
