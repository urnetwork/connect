// The whole-tree validation tests, and the one property that decides how nearly every one of
// them is built.
//
// Validate is five sweeps, and the failure this file is written against is not "a check is
// wrong" -- it is "a check runs over the first element of its set and returns nil for the rest".
// That shape passes every fixture whose single bad element happens to sit at position zero, which
// is where a fixture written by hand puts it, and this project has shipped it four times. So no
// test below breaks the first leaf, the first parent, the first entry of an unmerged_leaves
// vector or the first intermediate on a path. Each one derives the positions its set HAS from
// the tree, puts the single offender at every one of them in turn, and requires the refusal at
// each -- with a control at the same shape and no offender, so a fixture that was already broken
// cannot pass as a sweep that works.
//
// The fixtures are planted into the node array rather than installed through SetLeaf and
// SetParent, and plantNode says why: the setters refuse several of the shapes under test, and the
// tree this file is about did not come through them.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The clock every key_package sourced leaf in this file is judged against, and it is a REAL
// instant rather than a small round number.
//
// It has to be, and the reason is measurable rather than stylistic. validateLifetime widens the
// lifetime interval by the skew at both ends and guards each widening as a subtraction from the
// side that cannot wrap, so the not_after comparison is reached only when now > skew. The value
// this fixture used to carry was a clock of 1000 seconds with an hour of skew, under which
// now - skew is not a positive instant at all and NO not_after could ever be judged expired --
// so the not_after half of section 7.3's lifetime rule was unobservable through every context
// this file builds, and a validator that dropped the clock entirely passed the whole suite.
// TestValidateJudgesEveryKeyPackageLeafAgainstTheClock asserts the relation rather than trusting
// these two numbers, so a later edit that shrinks the clock back under the skew fails there.
const (
	testValidationNowMs       = 1_700_000_000_000
	testValidationClockSkewMs = 3_600_000
)

// testRequiredCapabilities is the group's required_capabilities body, and the third entry is in
// it for a reason the other two are not.
//
// urmessage_group_policy is also named by the group context extensions vector below and
// urmessage_leaf_keys is also carried by every fixture leaf's own extensions, so a leaf that
// stopped supporting either is refused by section 13.4's clause or by section 7.3's own
// extensions clause BEFORE Capabilities.Supports is reached -- which leaves the
// required_capabilities clause with no input of its own and makes it unobservable at this door.
// urmessage_owner_successor is demanded by nothing else: no fixture leaf carries the extension
// and the group context vector does not name it, so it is the one entry whose absence only this
// clause can refuse.
func testRequiredCapabilities() *RequiredCapabilities {
	return &RequiredCapabilities{
		ExtensionTypes: []ExtensionType{
			ExtensionTypeUrmessageGroupPolicy,
			ExtensionTypeUrmessageLeafKeys,
			ExtensionTypeUrmessageOwnerSuccessor,
		},
		CredentialTypes: []CredentialType{CredentialTypeBasic},
	}
}

// testTreeValidationContext is the context every tree in this file is judged against.
//
// Its extensions vector carries required_capabilities FIRST and the group policy behind it, and
// both halves of that are deliberate. required_capabilities is one of section 7.2's default
// types, so isDefaultExtensionType exempts it from the section 13.4 clause -- which means the
// clause has to step over an exempt entry to reach the one it judges, which is the exact shape
// LeafNode.Validate's own comment says a loop answering element zero would miss. And the body it
// carries is the encoding of the same structure RequiredCaps points at, because
// ValidateAgainstContext's check 0 now reconciles the two: a context whose vector did not carry
// the required capabilities it also passes separately is a caller holding one fact two ways.
func testTreeValidationContext(t testing.TB, crypto CryptoProvider) *TreeValidationContext {
	t.Helper()
	required := testRequiredCapabilities()
	body, err := syntax.Marshal(required)
	if err != nil {
		t.Fatalf("Marshal(required_capabilities): %v", err)
	}
	return &TreeValidationContext{
		Crypto:       crypto,
		Suite:        CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:      testGroupId(),
		RequiredCaps: required,
		GroupExtensions: []Extension{
			{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: body},
			{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte("policy")},
		},
		NowMs:       testValidationNowMs,
		ClockSkewMs: testValidationClockSkewMs,
	}
}

// testGroupContextFor is a GroupContext that AGREES with testTreeValidationContext about every
// fact the two structures both carry, pinning this tree's own hash.
//
// Every field it shares with the tree context is read off that context rather than restated
// beside it, which is the whole point: check 0 refuses a disagreement, so a fixture that spelled
// the group id or the suite a second time would start failing the day the context's copy moved,
// and the failure would look like a bug in the validator. The two copies are one copy here.
func testGroupContextFor(t testing.TB, crypto CryptoProvider, tree *RatchetTree) *GroupContext {
	t.Helper()
	ctx := testTreeValidationContext(t, crypto)
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	return &GroupContext{
		Version:     ProtocolVersionMls10,
		CipherSuite: ctx.Suite,
		GroupId:     cloneBytes(ctx.GroupId),
		Epoch:       1,
		TreeHash:    treeHash,
		Extensions:  slices.Clone(ctx.GroupExtensions),
	}
}

// withoutExtensionType is a capabilities vector with one code point removed.
//
// A fresh slice and never a filter in place. Every caller here narrows a leaf CLONE, whose
// capabilities vectors LeafNode.Clone has already made its own, so an in place filter would be
// correct today -- and it would stop being correct the first time a caller narrowed a vector it
// had not cloned, at which point the fixture itself would be narrowed and every later position
// of the sweep would be judging a tree that was already broken. The shape that cannot do that
// costs one allocation.
func withoutExtensionType(types []ExtensionType, drop ExtensionType) []ExtensionType {
	out := []ExtensionType{}
	for _, one := range types {
		if one != drop {
			out = append(out, one)
		}
	}
	return out
}

// plantNode installs a node at a raw index, past every refusal SetLeaf and SetParent make.
//
// The doors it bypasses are the point rather than a convenience. SetParent refuses an
// unmerged_leaves entry the tree does not have and refuses a parent body at an even index;
// SetLeaf refuses a leaf body at an odd one. Those are three of the shapes this file has to
// judge, and the tree it judges did not come through either setter -- it was DECODED, out of a
// Welcome or a ratchet_tree extension some peer chose, and the codec enforces neither rule. A
// fixture built through the setters could only ever hold the shapes the setters allow, which is
// exactly the set Validate would then be saying nothing about.
func plantNode(t testing.TB, tree *RatchetTree, x NodeIndex, node *Node) {
	t.Helper()
	if uint32(x) >= tree.NodeWidth() {
		t.Fatalf("plant at node %d: the tree holds %d nodes", x, tree.NodeWidth())
	}
	tree.nodes[x] = node
}

func plantParent(t testing.TB, tree *RatchetTree, x NodeIndex, parent *ParentNode) {
	t.Helper()
	plantNode(t, tree, x, &Node{NodeType: NodeTypeParent, Parent: parent.Clone()})
}

// fillerKey is a 32 byte encryption key for a planted parent, derived from the index it is
// planted at.
//
// Derived and not a constant, because two planted parents sharing a key would answer
// errDuplicateEncryptionKey -- and every unmerged_leaves assertion in this file would then be
// passing on a refusal that has nothing to do with unmerged leaves. It is a repeating two byte
// pattern, which no X25519 public key any fixture here generates will ever be.
// tamperParentHash answers a parent_hash field that is not the one it was given, for any field
// including the EMPTY one.
//
// Flipping a byte of the stored field is the obvious way to write this and it panics on the
// root: an UpdatePath gives the topmost node an empty parent_hash, so the root of every
// committed tree in this package carries a zero length field. A sweep that skipped the root
// rather than tampering with it would leave the one parent nothing above it can claim untested,
// which is where a chain check is likeliest to stop early.
func tamperParentHash(field []byte) []byte {
	return append(cloneBytes(field), 0x5A)
}

func fillerKey(x NodeIndex) HpkePublicKey {
	return HpkePublicKey(bytes.Repeat([]byte{0xE0, byte(x)}, 16))
}

// ---------------------------------------------------------------------------
// the accepting side
// ---------------------------------------------------------------------------

// TestValidateAcceptsAFreshAndACommittedTree is the positive control every refusal below stands
// on: a tree this package builds, before and after a commit, at five widths.
//
// It asserts the node array is untouched as well, at both. Validate is a REFUSAL surface over a
// tree a caller has not agreed to yet, and a validator that normalised a node on its way through
// would leave that caller holding a tree no peer sent -- the same atomicity task 21's merge owes,
// owed here on the accepting path too because a repair made while answering nil is the one
// nothing else in this file would notice.
func TestValidateAcceptsAFreshAndACommittedTree(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{1, 2, 3, 5, 8} {
		tree, members := newTestTree(t, crypto, n)
		before := treeSnapshot(tree)
		if err := tree.Validate(testTreeValidationContext(t, crypto)); err != nil {
			t.Fatalf("n=%d Validate on a fresh tree: %v", n, err)
		}
		if after := treeSnapshot(tree); !slices.Equal(before, after) {
			t.Errorf("n=%d a fresh tree was changed by the validator that accepted it", n)
		}
		if n < 2 {
			continue
		}
		senderTree, _, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
		before = treeSnapshot(senderTree)
		if err := senderTree.Validate(testTreeValidationContext(t, crypto)); err != nil {
			t.Fatalf("n=%d Validate after a commit: %v", n, err)
		}
		if after := treeSnapshot(senderTree); !slices.Equal(before, after) {
			t.Errorf("n=%d a committed tree was changed by the validator that accepted it", n)
		}
	}
}

// ---------------------------------------------------------------------------
// check 1: the shape of the node array
// ---------------------------------------------------------------------------

// TestValidateRefusesANodeArrayThatIsNotTwoNMinusOne drives the two structural refusals
// separately, by the detail each carries, so neither is covered only by the other.
//
// They overlap on the inputs but not on the message: a zero width and an even width fail the
// inversion AND the fullness test, while an odd width that is not 2^k+... fails only the second.
// Asserting the detail is what keeps the first branch from becoming unreachable decoration the
// day somebody reorders them.
func TestValidateRefusesANodeArrayThatIsNotTwoNMinusOne(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	full, _ := newTestTree(t, crypto, 8)
	if err := full.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("the fixture this truncates does not validate, so every refusal below could be it: %v", err)
	}
	cases := []struct {
		name   string
		width  int
		detail string
	}{
		{"no nodes at all", 0, "is not 2n-1"},
		{"an even node width", 14, "is not 2n-1"},
		{"an odd width whose leaf count is not a power of two", 11, "power of two"},
		{"one node short of a doubling", 13, "power of two"},
	}
	for _, c := range cases {
		broken := full.Clone()
		broken.nodes = broken.nodes[:c.width]
		err := broken.Validate(testTreeValidationContext(t, crypto))
		if !errors.Is(err, ErrTreeMalformed) {
			t.Errorf("%s: err = %v, want ErrTreeMalformed", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.detail) {
			t.Errorf("%s: err = %q and does not carry %q, so the two structural refusals are indistinguishable",
				c.name, err, c.detail)
		}
	}
}

// TestValidateJudgesTheNodeTypeAtEveryPosition sweeps the wrong kind of node across every slot
// the array has.
//
// Every slot and not one of each parity: the check is a loop and the class it must cover is "any
// position", so a version that judged position zero, or the leaves alone, or stopped at the first
// parent is what this is looking for. The wrong kind for a slot is derived from the slot's own
// parity rather than listed, which is the same derivation the implementation makes and the reason
// the sweep does not have to be told where the leaves are.
func TestValidateJudgesTheNodeTypeAtEveryPosition(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	if err := tree.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("the fixture does not validate, so every refusal below could be it: %v", err)
	}
	sampleLeaf := tree.Leaf(LeafIndex(0)).Clone()
	swept := 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		wrong := &Node{NodeType: NodeTypeLeaf, Leaf: sampleLeaf.Clone()}
		if NodeIndex(x).IsLeaf() {
			wrong = &Node{NodeType: NodeTypeParent, Parent: &ParentNode{EncryptionKey: fillerKey(NodeIndex(x))}}
		}
		broken := tree.Clone()
		plantNode(t, broken, NodeIndex(x), wrong)
		swept += 1
		if err := broken.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, ErrNodeTypeMismatch) {
			t.Errorf("node %d holds the body the other parity takes: err = %v, want ErrNodeTypeMismatch", x, err)
		}
	}
	if swept < 3 {
		t.Fatalf("the sweep ran over %d positions, so it says nothing about a loop that reads one", swept)
	}
}

// TestValidateRefusesANodeThatIsBothKindsOrNeither is the half of "matches its position" that a
// parity test alone does not reach: the declared NodeType and the body present can disagree, and
// a node can carry both bodies or neither.
//
// A node carrying a leaf body and a parent body is the sharpest of the four. Every reader of the
// tree picks one of the two fields, the tree hash picks one and Resolution picks the other, and a
// node that answers both makes those two readers disagree about a tree they both verified.
func TestValidateRefusesANodeThatIsBothKindsOrNeither(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	leaf := tree.Leaf(LeafIndex(0)).Clone()
	parent := &ParentNode{EncryptionKey: fillerKey(NodeIndex(1))}
	cases := []struct {
		name string
		at   NodeIndex
		node *Node
	}{
		{"a leaf slot holding both bodies", 4, &Node{NodeType: NodeTypeLeaf, Leaf: leaf.Clone(), Parent: parent.Clone()}},
		{"a parent slot holding both bodies", 5, &Node{NodeType: NodeTypeParent, Parent: parent.Clone(), Leaf: leaf.Clone()}},
		{"a leaf slot holding neither body", 4, &Node{NodeType: NodeTypeLeaf}},
		{"a parent slot holding neither body", 5, &Node{NodeType: NodeTypeParent}},
		{"a leaf body under the parent type", 4, &Node{NodeType: NodeTypeParent, Leaf: leaf.Clone()}},
		{"a parent body under the leaf type", 5, &Node{NodeType: NodeTypeLeaf, Parent: parent.Clone()}},
	}
	for _, c := range cases {
		broken := tree.Clone()
		plantNode(t, broken, c.at, c.node)
		if err := broken.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, ErrNodeTypeMismatch) {
			t.Errorf("%s: err = %v, want ErrNodeTypeMismatch", c.name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// check 2: every non-blank leaf
// ---------------------------------------------------------------------------

// TestValidateJudgesEveryNonBlankLeafAndNotOnlyTheFirst breaks one leaf's signature at a time,
// at every occupied leaf the tree has.
//
// The occupied set is read off the tree with NonBlankLeaves rather than assumed to be 0..n-1, so
// the sweep covers the positions this fixture actually has and would keep covering them if the
// builder's shape moved.
func TestValidateJudgesEveryNonBlankLeafAndNotOnlyTheFirst(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	if err := tree.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("the fixture does not validate, so every refusal below could be it: %v", err)
	}
	leaves := tree.NonBlankLeaves()
	if len(leaves) < 2 {
		t.Fatalf("the fixture has %d occupied leaves, so a sweep over it says nothing about a loop that reads one",
			len(leaves))
	}
	for _, i := range leaves {
		broken := tree.Clone()
		leaf := broken.Leaf(i)
		leaf.Signature = cloneBytes(leaf.Signature)
		leaf.Signature[0] ^= 0xFF
		err := broken.Validate(testTreeValidationContext(t, crypto))
		if !errors.Is(err, errBadSignature) {
			t.Errorf("leaf %d carries a broken signature: err = %v, want errBadSignature", i, err)
			continue
		}
		if want := fmt.Sprintf("leaf %d:", i); !strings.Contains(err.Error(), want) {
			t.Errorf("leaf %d was refused as %q, which does not name the leaf that failed", i, err)
		}
	}
}

// TestValidateJudgesALeafAtItsOwnIndexAndNotAtSomeOther holds the binding the inferred source
// leaves in place.
//
// validateLeaves does not demand a leaf_node_source, because a settled tree legally holds all
// three. What it must still enforce is that an update or commit sourced leaf verifies AT THE
// INDEX IT SITS AT -- otherwise a member's own signed leaf, lifted from one position of the tree
// to another, is a valid leaf twice and the tree it makes is one nobody signed.
func TestValidateJudgesALeafAtItsOwnIndexAndNotAtSomeOther(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	for i := uint32(1); i < uint32(len(members)); i += 1 {
		broken := tree.Clone()
		// member i's own leaf, signed by member i, at member i-1's position. Only the index
		// moved, so a validator that checked the signature without the position accepts it.
		moved := tree.Leaf(LeafIndex(i)).Clone()
		if err := broken.SetLeaf(LeafIndex(i-1), moved); err != nil {
			t.Fatalf("SetLeaf: %v", err)
		}
		if err := broken.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, errBadSignature) {
			t.Errorf("leaf %d moved to index %d: err = %v, want errBadSignature", i, i-1, err)
		}
	}
}

// TestValidateJudgesEveryLeafAgainstTheGroupsRequiredCapabilities is section 7.3's
// required_capabilities clause at this door, at every occupied leaf.
//
// It exists because of what a signature test cannot see. validateLeaves decides two of section
// 7.3's rules itself and PASSES THE OTHER SIX THROUGH, one field of the tree context each, and a
// passthrough is invisible to a test that breaks a signature: RequiredCaps could be replaced by
// nil in validateLeaves' literal and the whole of ./mls and ./message stayed green, which is the
// state this test and the two below it were written to end. Two of the eight -- Suite and GroupId
// -- were already observed, because a leaf that fails either fails its signature too.
//
// The offender is a leaf that fails NOTHING ELSE: correctly signed, at its own index, carrying
// its own extensions, with exactly one code point removed from its capabilities. The code point
// is urmessage_owner_successor for testRequiredCapabilities' reason, and both halves of that
// reason are asserted here rather than trusted, from the context itself -- so a later task that
// adds owner_successor to the group's extensions vector, or drops it from the requirement, fails
// here instead of quietly turning this into a second copy of the test below.
//
// The control is the same narrowed tree judged with RequiredCaps nil. If that refuses, the leaf
// was broken in some other way and every refusal above it is unattributable; if it accepts, the
// refusal came from the requirement and from nothing else.
func TestValidateJudgesEveryLeafAgainstTheGroupsRequiredCapabilities(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	ctx := testTreeValidationContext(t, crypto)
	if err := tree.Validate(ctx); err != nil {
		t.Fatalf("the fixture does not validate, so every refusal below could be it: %v", err)
	}
	const dropped = ExtensionTypeUrmessageOwnerSuccessor
	if ctx.RequiredCaps == nil || !slices.Contains(ctx.RequiredCaps.ExtensionTypes, dropped) {
		t.Fatalf("the group no longer requires %#04x, so a leaf that drops it is refused by nothing and this sweep says nothing",
			uint16(dropped))
	}
	for i := range ctx.GroupExtensions {
		if ctx.GroupExtensions[i].ExtensionType == dropped {
			t.Fatalf("the group context now carries %#04x as well, so a leaf that drops it is refused by the section 13.4 clause before this one is reached",
				uint16(dropped))
		}
	}
	leaves := tree.NonBlankLeaves()
	if len(leaves) < 2 {
		t.Fatalf("the fixture has %d occupied leaves, so a sweep over it says nothing about a loop that reads one",
			len(leaves))
	}
	for _, i := range leaves {
		narrowed := tree.Leaf(i).Clone()
		narrowed.Capabilities.Extensions = withoutExtensionType(narrowed.Capabilities.Extensions, dropped)
		if err := narrowed.Sign(crypto, members[int(i)].SignaturePriv, testGroupId(), i); err != nil {
			t.Fatalf("Sign: %v", err)
		}
		broken := tree.Clone()
		if err := broken.SetLeaf(i, narrowed); err != nil {
			t.Fatalf("SetLeaf: %v", err)
		}
		err := broken.Validate(testTreeValidationContext(t, crypto))
		if !errors.Is(err, errMissingRequiredCapability) {
			t.Errorf("leaf %d no longer supports a required capability: err = %v, want errMissingRequiredCapability", i, err)
			continue
		}
		// the two wrapped spellings of the same sentinel answer errors.Is as well, and either
		// of them here would mean the leaf was refused by a DIFFERENT clause that happens to
		// share a base error -- which is the whole failure mode this file's derived fixtures
		// are built to avoid
		if errors.Is(err, errLeafExtensionNotListed) || errors.Is(err, errGroupContextExtensionNotListed) {
			t.Errorf("leaf %d was refused as %q, which is one of the extension clauses rather than the required capabilities one", i, err)
		}
		if want := fmt.Sprintf("leaf %d:", i); !strings.Contains(err.Error(), want) {
			t.Errorf("leaf %d was refused as %q, which does not name the leaf that failed", i, err)
		}
		if want := fmt.Sprintf("%#04x", uint16(dropped)); !strings.Contains(err.Error(), want) {
			t.Errorf("leaf %d was refused as %q, which does not name the capability that is missing", i, err)
		}
		unrequired := testTreeValidationContext(t, crypto)
		unrequired.RequiredCaps = nil
		if err := broken.Validate(unrequired); err != nil {
			t.Errorf("leaf %d narrowed and judged against no requirement at all: err = %v, want nil -- the refusal above was not the requirement's",
				i, err)
		}
	}
}

// TestValidateJudgesEveryLeafAgainstTheGroupContextExtensions is RFC 9420 section 13.4 as
// corrected by erratum 8745 at this door: every member's capabilities must indicate support for
// every extension the GroupContext is using, whichever source the member's leaf carries.
//
// Second of the three passthroughs. GroupExtensions could be replaced by nil in validateLeaves'
// literal and nothing failed, which meant the erratum's clause -- the one LeafNode.Validate's own
// comment argues for at length -- was applied to no tree this file judges.
//
// The offending type is DERIVED from the context rather than named: the vector legally holds
// default types, which section 7.2 exempts and this clause steps over, so the type to drop is
// the first one the clause can actually judge. Both sweeps run, every judged type against every
// occupied leaf, because either loop alone is the shape this file exists to refuse.
//
// The refusal asserted is the group context clause's OWN sentinel and not the base one it wraps.
// That distinction is what makes this test observe its own property: the fixture's required
// capabilities name the same code point, so a validator that stopped passing GroupExtensions
// through would still refuse these leaves -- one clause later, under errMissingRequiredCapability
// unwrapped -- and a test asking only "was it refused" would pass over exactly the deletion it
// was written for.
func TestValidateJudgesEveryLeafAgainstTheGroupContextExtensions(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	ctx := testTreeValidationContext(t, crypto)
	if err := tree.Validate(ctx); err != nil {
		t.Fatalf("the fixture does not validate, so every refusal below could be it: %v", err)
	}
	judged := []ExtensionType{}
	for i := range ctx.GroupExtensions {
		if !isDefaultExtensionType(ctx.GroupExtensions[i].ExtensionType) {
			judged = append(judged, ctx.GroupExtensions[i].ExtensionType)
		}
	}
	if len(judged) == 0 {
		t.Fatal("every entry of the group context's extensions vector is a default type, which section 7.2 exempts from this clause, so the clause has nothing to judge and the sweep below says nothing")
	}
	leaves := tree.NonBlankLeaves()
	if len(leaves) < 2 {
		t.Fatalf("the fixture has %d occupied leaves, so a sweep over it says nothing about a loop that reads one",
			len(leaves))
	}
	for _, dropped := range judged {
		for _, i := range leaves {
			narrowed := tree.Leaf(i).Clone()
			narrowed.Capabilities.Extensions = withoutExtensionType(narrowed.Capabilities.Extensions, dropped)
			if err := narrowed.Sign(crypto, members[int(i)].SignaturePriv, testGroupId(), i); err != nil {
				t.Fatalf("Sign: %v", err)
			}
			broken := tree.Clone()
			if err := broken.SetLeaf(i, narrowed); err != nil {
				t.Fatalf("SetLeaf: %v", err)
			}
			err := broken.Validate(testTreeValidationContext(t, crypto))
			if !errors.Is(err, errGroupContextExtensionNotListed) {
				t.Errorf("leaf %d does not support the group's extension %#04x: err = %v, want errGroupContextExtensionNotListed",
					i, uint16(dropped), err)
				continue
			}
			if want := fmt.Sprintf("leaf %d:", i); !strings.Contains(err.Error(), want) {
				t.Errorf("leaf %d was refused as %q, which does not name the leaf that failed", i, err)
			}
			// the same narrowed tree with neither the group's extensions nor its requirement
			// in hand, which is the only judgement left that can accept it. If this refuses,
			// the narrowing broke something else and the refusal above is unattributable.
			unjudged := testTreeValidationContext(t, crypto)
			unjudged.GroupExtensions = nil
			unjudged.RequiredCaps = nil
			if err := broken.Validate(unjudged); err != nil {
				t.Errorf("leaf %d narrowed and judged against no group extensions at all: err = %v, want nil", i, err)
			}
		}
	}
}

// TestValidateJudgesEveryKeyPackageLeafAgainstTheClock is the third passthrough and the one whose
// absence was invisible for a reason worth writing down.
//
// NowMs and ClockSkewMs could both be replaced by zero in validateLeaves' literal -- which is
// LeafValidationContext's documented opt out of the lifetime check, so the whole rule stops
// applying -- and nothing failed. Two things were true at once: no fixture in this file carried a
// key_package sourced leaf, because newTestTree signs every leaf under update on purpose, and the
// clock the fixture context carried could not have judged a not_after even if one had. Both are
// fixed here, and the second is asserted rather than assumed: the guard below is the one that
// would have caught the old clock.
//
// The lifetime is a variant field of the key_package arm alone, so every offender here is a leaf
// re-sourced to key_package and re-signed. That is also the first key_package leaf this file
// builds, and it is the fixture the binding test below reads.
//
// Both ends of the interval are swept, at every occupied leaf, with the endpoints DERIVED from
// the context's own clock rather than written as constants -- an expired leaf and a leaf that is
// not current yet are separately reachable failures of validateLifetime's two comparisons, and a
// fixture that named its instants would stop driving either the day the clock moved. The control
// is the same re-sourced leaf inside its lifetime: if that refuses, the refusals are about the
// source swap rather than about the clock.
func TestValidateJudgesEveryKeyPackageLeafAgainstTheClock(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	ctx := testTreeValidationContext(t, crypto)
	if err := tree.Validate(ctx); err != nil {
		t.Fatalf("the fixture does not validate, so every refusal below could be it: %v", err)
	}
	// section 7.2's endpoints are seconds and this context carries milliseconds, which is the
	// division validateLifetime makes; the relation between the two is what decides whether the
	// not_after comparison is reachable at all
	nowSeconds := ctx.NowMs / 1000
	skewSeconds := ctx.ClockSkewMs / 1000
	if nowSeconds <= skewSeconds {
		t.Fatalf("the fixture clock is %d s with %d s of skew, so now - skew is not a positive instant and validateLifetime's not_after comparison is unreachable: no leaf this file can build is ever expired",
			nowSeconds, skewSeconds)
	}
	leaves := tree.NonBlankLeaves()
	if len(leaves) < 2 {
		t.Fatalf("the fixture has %d occupied leaves, so a sweep over it says nothing about a loop that reads one",
			len(leaves))
	}
	for _, c := range []struct {
		name     string
		lifetime Lifetime
		refused  bool
	}{
		{"a leaf whose not_after is behind now even at full skew",
			Lifetime{NotBefore: 0, NotAfter: nowSeconds - skewSeconds - 1}, true},
		{"a leaf whose not_before is ahead of now even at full skew",
			Lifetime{NotBefore: nowSeconds + skewSeconds + 1, NotAfter: nowSeconds + skewSeconds + 10_000}, true},
		{"a leaf comfortably inside its lifetime",
			Lifetime{NotBefore: 0, NotAfter: nowSeconds + skewSeconds + 10_000}, false},
	} {
		for _, i := range leaves {
			dated := tree.Leaf(i).Clone()
			dated.LeafNodeSource = LeafNodeSourceKeyPackage
			dated.Lifetime = c.lifetime
			if err := dated.Sign(crypto, members[int(i)].SignaturePriv, testGroupId(), i); err != nil {
				t.Fatalf("Sign: %v", err)
			}
			judged := tree.Clone()
			if err := judged.SetLeaf(i, dated); err != nil {
				t.Fatalf("SetLeaf: %v", err)
			}
			err := judged.Validate(testTreeValidationContext(t, crypto))
			if !c.refused {
				if err != nil {
					t.Errorf("%s at leaf %d: err = %v, want nil", c.name, i, err)
				}
				continue
			}
			if !errors.Is(err, ErrLeafNodeLifetime) {
				t.Errorf("%s at leaf %d: err = %v, want ErrLeafNodeLifetime", c.name, i, err)
				continue
			}
			if want := fmt.Sprintf("leaf %d:", i); !strings.Contains(err.Error(), want) {
				t.Errorf("%s at leaf %d was refused as %q, which does not name the leaf that failed", c.name, i, err)
			}
		}
	}
}

// TestTheIndexAndGroupBindingIsUpdateAndCommitsAndNotKeyPackages measures the sentence
// validateLeaves' doc used to get wrong, in both directions.
//
// That doc claimed a leaf lifted from another group, or from another index of this one, is
// refused here "whichever source it claims", and it is the whole argument for why inferring the
// expected source is safe rather than a hole. It is true of update and commit and FALSE of
// key_package: signatureContent's key_package arm is the empty struct of the section 7.2 select,
// carrying neither the group id nor the leaf index, so a key_package leaf verifies at every index
// of every group. That is RFC 9420 as written -- a KeyPackage is minted before its author knows
// where it will land -- and this test asserts it rather than a refusal, so the day somebody adds
// the binding, this fails and the doc is corrected with it instead of drifting again.
//
// The lifted leaf's own position is BLANKED before the copy is planted, because the two would
// otherwise repeat a signature key and check 3 would refuse the tree for a reason that has
// nothing to do with the binding -- which would read exactly like the binding holding.
func TestTheIndexAndGroupBindingIsUpdateAndCommitsAndNotKeyPackages(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	ctx := testTreeValidationContext(t, crypto)
	current := Lifetime{NotBefore: 0, NotAfter: ctx.NowMs/1000 + ctx.ClockSkewMs/1000 + 10_000}
	elsewhere := testTreeValidationContext(t, crypto)
	elsewhere.GroupId = []byte("a completely different group")

	// the update sourced half, which is where the binding does hold. Both statements of it: the
	// same tree under another group id, and one member's leaf at another member's index.
	if err := tree.Validate(elsewhere); !errors.Is(err, errBadSignature) {
		t.Errorf("an update sourced tree judged under another group id: err = %v, want errBadSignature", err)
	}
	moved := tree.Clone()
	if err := moved.Blank(LeafIndex(1).NodeIndex()); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if err := moved.SetLeaf(LeafIndex(2), tree.Leaf(LeafIndex(1)).Clone()); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if err := moved.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, errBadSignature) {
		t.Errorf("member 1's update sourced leaf at index 2: err = %v, want errBadSignature", err)
	}

	// and the key_package half, where neither binding exists at all
	keyPackaged := tree.Clone()
	for _, i := range tree.NonBlankLeaves() {
		minted := tree.Leaf(i).Clone()
		minted.LeafNodeSource = LeafNodeSourceKeyPackage
		minted.Lifetime = current
		if err := minted.Sign(crypto, members[int(i)].SignaturePriv, testGroupId(), i); err != nil {
			t.Fatalf("Sign: %v", err)
		}
		if err := keyPackaged.SetLeaf(i, minted); err != nil {
			t.Fatalf("SetLeaf: %v", err)
		}
	}
	if err := keyPackaged.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("a key_package sourced tree under its own group id: err = %v, want nil", err)
	}
	if err := keyPackaged.Validate(elsewhere); err != nil {
		t.Errorf("a key_package sourced tree under ANOTHER group id: err = %v, want nil -- if this now refuses, the key_package preimage has gained a group id binding and validateLeaves' doc must stop saying it has none",
			err)
	}
	lifted := keyPackaged.Clone()
	if err := lifted.Blank(LeafIndex(1).NodeIndex()); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	if err := lifted.SetLeaf(LeafIndex(2), keyPackaged.Leaf(LeafIndex(1)).Clone()); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if err := lifted.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Errorf("member 1's key_package sourced leaf at index 2: err = %v, want nil -- if this now refuses, the key_package preimage has gained a leaf index binding and validateLeaves' doc must stop saying it has none",
			err)
	}
}

// ---------------------------------------------------------------------------
// check 3: key uniqueness
// ---------------------------------------------------------------------------

// TestValidateRejectsDuplicateKeys is the plan's own pair, one repeat of each kind.
func TestValidateRejectsDuplicateKeys(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	duplicate := tree.Leaf(LeafIndex(1)).Clone()
	duplicate.EncryptionKey = cloneBytes(tree.Leaf(LeafIndex(0)).EncryptionKey)
	if err := duplicate.Sign(crypto, members[1].SignaturePriv, testGroupId(), LeafIndex(1)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := tree.SetLeaf(LeafIndex(1), duplicate); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if err := tree.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, errDuplicateEncryptionKey) {
		t.Fatalf("err = %v, want errDuplicateEncryptionKey", err)
	}

	tree, members = newTestTree(t, crypto, 4)
	duplicate = tree.Leaf(LeafIndex(1)).Clone()
	duplicate.SignatureKey = cloneBytes(tree.Leaf(LeafIndex(0)).SignatureKey)
	if err := duplicate.Sign(crypto, members[0].SignaturePriv, testGroupId(), LeafIndex(1)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := tree.SetLeaf(LeafIndex(1), duplicate); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	if err := tree.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, errDuplicateSignatureKey) {
		t.Fatalf("err = %v, want errDuplicateSignatureKey", err)
	}
}

// TestValidateFindsADuplicateKeyWhereverThePairSits moves the repeated pair down the tree one
// position at a time.
//
// A uniqueness check written over the first element of the array, or over the first two, accepts
// every tree whose repeat is further along, and a fixture that always duplicates leaf zero cannot
// tell the difference. Each iteration repeats the key of the leaf IMMEDIATELY BEFORE the one it
// plants, so the pair walks to the far end of the array rather than always having one foot at
// index zero.
func TestValidateFindsADuplicateKeyWhereverThePairSits(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	const memberCount = 8
	swept := 0
	for j := uint32(1); j < memberCount; j += 1 {
		for _, kind := range []string{"encryption", "signature"} {
			tree, members := newTestTree(t, crypto, memberCount)
			duplicate := tree.Leaf(LeafIndex(j)).Clone()
			signer := members[j].SignaturePriv
			want := errDuplicateEncryptionKey
			if kind == "signature" {
				duplicate.SignatureKey = cloneBytes(tree.Leaf(LeafIndex(j - 1)).SignatureKey)
				// signed by the member whose key it now publishes, so the leaf still verifies
				// and the only thing wrong with the tree is the repeat
				signer = members[j-1].SignaturePriv
				want = errDuplicateSignatureKey
			} else {
				duplicate.EncryptionKey = cloneBytes(tree.Leaf(LeafIndex(j - 1)).EncryptionKey)
			}
			if err := duplicate.Sign(crypto, signer, testGroupId(), LeafIndex(j)); err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if err := tree.SetLeaf(LeafIndex(j), duplicate); err != nil {
				t.Fatalf("SetLeaf: %v", err)
			}
			swept += 1
			if err := tree.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, want) {
				t.Errorf("leaves %d and %d share a %s key: err = %v, want %v", j-1, j, kind, err, want)
			}
		}
	}
	if swept < 4 {
		t.Fatalf("the sweep ran %d fixtures, so it says nothing about a loop that reads the front of the array", swept)
	}
}

// TestValidateFindsADuplicateEncryptionKeyAmongTheParentNodes is the half of check 3 that a leaf
// only fixture cannot reach: the rule is over EVERY node, and a version written over the leaves
// alone passes every test above.
//
// It runs every ordered pair of the non-blank parents a commit leaves behind, and then one pair
// across the two kinds -- a parent publishing a leaf's key -- because "leaf and parent alike" is
// two statements and a map keyed per kind satisfies only the first.
func TestValidateFindsADuplicateEncryptionKeyAmongTheParentNodes(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	senderTree, _, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	parents := []NodeIndex{}
	for x := uint32(1); x < senderTree.NodeWidth(); x += 2 {
		if senderTree.ParentAt(NodeIndex(x)) != nil {
			parents = append(parents, NodeIndex(x))
		}
	}
	if len(parents) < 2 {
		t.Fatalf("a commit left %d non-blank parents, so there is no pair to repeat a key across", len(parents))
	}
	for _, from := range parents {
		for _, to := range parents {
			if from == to {
				continue
			}
			broken := senderTree.Clone()
			repeat := broken.ParentAt(to).Clone()
			repeat.EncryptionKey = HpkePublicKey(cloneBytes(broken.ParentAt(from).EncryptionKey))
			plantParent(t, broken, to, repeat)
			if err := broken.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, errDuplicateEncryptionKey) {
				t.Errorf("parents %d and %d share an encryption key: err = %v, want errDuplicateEncryptionKey",
					from, to, err)
			}
		}
	}
	for _, leaf := range senderTree.NonBlankLeaves() {
		at := parents[len(parents)-1]
		broken := senderTree.Clone()
		repeat := broken.ParentAt(at).Clone()
		repeat.EncryptionKey = HpkePublicKey(cloneBytes(broken.Leaf(leaf).EncryptionKey))
		plantParent(t, broken, at, repeat)
		if err := broken.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, errDuplicateEncryptionKey) {
			t.Errorf("parent %d publishes leaf %d's encryption key: err = %v, want errDuplicateEncryptionKey",
				at, leaf, err)
		}
	}
}

// ---------------------------------------------------------------------------
// check 4: unmerged leaves
// ---------------------------------------------------------------------------

// TestValidateRejectsBadUnmergedLeaves is the plan's table, one refusal per way a single entry
// can be wrong.
//
// The parents are PLANTED rather than installed: SetParent refuses an out of range entry itself,
// so the plan's own version of this table never reaches Validate at all for that row -- it stops
// at the setter and reports a fixture failure. The tree this rule is stated over arrives decoded,
// and the codec checks the order of the vector and nothing about its contents.
func TestValidateRejectsBadUnmergedLeaves(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	cases := []struct {
		name     string
		unmerged []LeafIndex
		want     error
	}{
		{"descending", []LeafIndex{1, 0}, ErrUnmergedLeavesNotSorted},
		{"duplicated", []LeafIndex{1, 1}, ErrUnmergedLeavesNotSorted},
		{"out of range", []LeafIndex{99}, ErrUnmergedLeafInconsistent},
		{"not a descendant", []LeafIndex{3}, ErrUnmergedLeafInconsistent},
	}
	for _, c := range cases {
		tree, _ := newTestTree(t, crypto, 4)
		plantParent(t, tree, NodeIndex(1), &ParentNode{
			EncryptionKey:  fillerKey(NodeIndex(1)),
			UnmergedLeaves: c.unmerged,
		})
		if err := tree.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// a blank leaf, which is a member who is gone and a node that still names them
	tree, _ := newTestTree(t, crypto, 4)
	if err := tree.Blank(LeafIndex(1).NodeIndex()); err != nil {
		t.Fatalf("Blank: %v", err)
	}
	plantParent(t, tree, NodeIndex(1), &ParentNode{
		EncryptionKey:  fillerKey(NodeIndex(1)),
		UnmergedLeaves: []LeafIndex{1},
	})
	if err := tree.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, ErrUnmergedLeafInconsistent) {
		t.Errorf("a node listing a blank leaf: err = %v, want ErrUnmergedLeafInconsistent", err)
	}
}

// TestValidateJudgesTheUnmergedLeavesOfEveryParentAndNotOnlyTheFirst plants the same offending
// vector at every parent slot in turn.
//
// The slots are derived from the width -- every odd index, which is what the parents are -- so
// the sweep covers the last one and every one in between, and a check that stopped after the
// first non-blank parent it found is a failure at every position but that one.
func TestValidateJudgesTheUnmergedLeavesOfEveryParentAndNotOnlyTheFirst(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	if err := tree.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("the fixture does not validate, so every refusal below could be it: %v", err)
	}
	// the first leaf index this tree does not have, so the entry is wrong at EVERY parent
	// including the root, which "not a descendant" cannot be
	beyond := LeafIndex(tree.LeafWidth())
	swept := 0
	for x := uint32(1); x < tree.NodeWidth(); x += 2 {
		broken := tree.Clone()
		plantParent(t, broken, NodeIndex(x), &ParentNode{
			EncryptionKey:  fillerKey(NodeIndex(x)),
			UnmergedLeaves: []LeafIndex{beyond},
		})
		swept += 1
		err := broken.Validate(testTreeValidationContext(t, crypto))
		if !errors.Is(err, ErrUnmergedLeafInconsistent) {
			t.Errorf("parent %d lists leaf %d and the tree has %d leaves: err = %v, want ErrUnmergedLeafInconsistent",
				x, beyond, tree.LeafWidth(), err)
			continue
		}
		// the RANGE branch and not the blank leaf branch, which the same sentinel would also
		// answer: an index the tree does not have reaches Leaf as a nil too, so without this the
		// range check could be deleted and this sweep would keep passing on the other one.
		if want := "and the tree has"; !strings.Contains(err.Error(), want) {
			t.Errorf("parent %d: err = %q, which does not report an out of range index", x, err)
		}
	}
	if swept < 2 {
		t.Fatalf("the sweep ran over %d parent slots, so it says nothing about a loop that reads one", swept)
	}
}

// TestValidateJudgesEveryEntryOfAnUnmergedLeavesVector moves the single bad entry along the
// vector without disturbing the vector.
//
// The vector is the same at every iteration -- every leaf the tree has, ascending -- and what
// moves is which leaf is BLANK. So entry k is the only one that offends, the order is never in
// question, and a check applied to element zero of the list passes for every k but zero. The
// control at the top plants the same vector over a tree with no blank leaf and requires that this
// refusal does not fire, which is what stops the sweep from passing on a vector that was wrong to
// begin with.
func TestValidateJudgesEveryEntryOfAnUnmergedLeavesVector(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}
	everyLeaf := []LeafIndex{}
	for i := uint32(0); i < uint32(tree.LeafWidth()); i += 1 {
		everyLeaf = append(everyLeaf, LeafIndex(i))
	}
	if len(everyLeaf) < 3 {
		t.Fatalf("the vector under test has %d entries, so moving the offender along it proves little", len(everyLeaf))
	}
	control := tree.Clone()
	plantParent(t, control, root, &ParentNode{EncryptionKey: fillerKey(root), UnmergedLeaves: everyLeaf})
	if err := control.Validate(testTreeValidationContext(t, crypto)); errors.Is(err, ErrUnmergedLeafInconsistent) {
		t.Fatalf("the control vector is already inconsistent, so the sweep below proves nothing: %v", err)
	}
	for k := range everyLeaf {
		broken := tree.Clone()
		if err := broken.Blank(everyLeaf[k].NodeIndex()); err != nil {
			t.Fatalf("Blank(%d): %v", everyLeaf[k], err)
		}
		plantParent(t, broken, root, &ParentNode{EncryptionKey: fillerKey(root), UnmergedLeaves: everyLeaf})
		err := broken.Validate(testTreeValidationContext(t, crypto))
		if !errors.Is(err, ErrUnmergedLeafInconsistent) {
			t.Errorf("entry %d of %d names leaf %d, which is blank: err = %v, want ErrUnmergedLeafInconsistent",
				k, len(everyLeaf), everyLeaf[k], err)
			continue
		}
		if want := fmt.Sprintf("leaf %d, which is blank", everyLeaf[k]); !strings.Contains(err.Error(), want) {
			t.Errorf("entry %d was refused as %q, which does not name the entry that offends", k, err)
		}
	}
}

// TestValidateJudgesEveryIntermediateBetweenAnUnmergedLeafAndTheNodeListingIt walks the offending
// intermediate up the path.
//
// This is the clause the plan's own test reaches at position zero only: it puts the leaf's list
// at node 3 of an eight leaf tree, where exactly one node stands between, so a check that read
// the first intermediate and stopped passes it. Here the list sits at the ROOT, the intermediates
// are derived from the leaf's own direct path, and each of them is the offender in turn.
//
// Both arrangements of the innocent intermediates are run. With them blank the offender is the
// only node the walk can look at, so a walk that skipped blanks and then stopped is caught; with
// them non-blank and carrying the leaf, the walk has to pass over a legitimate entry to reach the
// offender, which is the arrangement a real tree is in.
func TestValidateJudgesEveryIntermediateBetweenAnUnmergedLeafAndTheNodeListingIt(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	const unmerged = LeafIndex(0)
	root, err := rootOf(tree.LeafWidth())
	if err != nil {
		t.Fatalf("rootOf: %v", err)
	}
	path, err := directPathOf(unmerged.NodeIndex(), tree.LeafWidth())
	if err != nil {
		t.Fatalf("directPathOf: %v", err)
	}
	intermediates := []NodeIndex{}
	for _, x := range path {
		if x == root {
			break
		}
		intermediates = append(intermediates, x)
	}
	if len(intermediates) < 2 {
		t.Fatalf("leaf %d has %d nodes between it and the root, so moving the offender along them proves nothing",
			unmerged, len(intermediates))
	}
	// offender is an index into intermediates, or -1 for the control with no offender at all
	build := func(offender int, othersCarry bool) *RatchetTree {
		out := tree.Clone()
		plantParent(t, out, root, &ParentNode{
			EncryptionKey:  fillerKey(root),
			UnmergedLeaves: []LeafIndex{unmerged},
		})
		for j, x := range intermediates {
			switch {
			case j == offender:
				plantParent(t, out, x, &ParentNode{EncryptionKey: fillerKey(x)})
			case othersCarry:
				plantParent(t, out, x, &ParentNode{
					EncryptionKey:  fillerKey(x),
					UnmergedLeaves: []LeafIndex{unmerged},
				})
			}
		}
		return out
	}
	for _, othersCarry := range []bool{false, true} {
		control := build(-1, othersCarry)
		if err := control.Validate(testTreeValidationContext(t, crypto)); errors.Is(err, ErrUnmergedLeafInconsistent) {
			t.Fatalf("othersCarry=%v: the control is already inconsistent, so the sweep proves nothing: %v",
				othersCarry, err)
		}
		for k, offender := range intermediates {
			broken := build(k, othersCarry)
			err := broken.Validate(testTreeValidationContext(t, crypto))
			if !errors.Is(err, ErrUnmergedLeafInconsistent) {
				t.Errorf("othersCarry=%v: node %d between leaf %d and node %d does not list it: err = %v, want ErrUnmergedLeafInconsistent",
					othersCarry, offender, unmerged, root, err)
				continue
			}
			if want := fmt.Sprintf("node %d between them does not", offender); !strings.Contains(err.Error(), want) {
				t.Errorf("othersCarry=%v: the refusal is %q and does not name node %d, so the walk stopped somewhere else",
					othersCarry, err, offender)
			}
		}
	}
}

// TestValidateRejectsAnUnmergedLeafThatAnIntermediateDoesNotCarry is the plan's own fixture,
// kept because it is the one shape a published tree is likeliest to have -- a list at a node two
// levels up with one node in between.
func TestValidateRejectsAnUnmergedLeafThatAnIntermediateDoesNotCarry(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 8)
	// the node at 3 lists leaf 0, and node 1 between them does not.
	plantParent(t, tree, NodeIndex(1), &ParentNode{EncryptionKey: fillerKey(NodeIndex(1))})
	plantParent(t, tree, NodeIndex(3), &ParentNode{
		EncryptionKey:  fillerKey(NodeIndex(3)),
		UnmergedLeaves: []LeafIndex{0},
	})
	if err := tree.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, ErrUnmergedLeafInconsistent) {
		t.Fatalf("err = %v, want ErrUnmergedLeafInconsistent", err)
	}
}

// ---------------------------------------------------------------------------
// check 5: the parent hashes, which are VerifyParentHashes' and not restated here
// ---------------------------------------------------------------------------

// TestValidateVerifiesTheParentHashOfEveryNonBlankParent breaks the chain at each parent a commit
// left behind, one at a time.
//
// The whole point of the check is that Validate CALLS it: a version that ran the four cheaper
// sweeps and returned nil accepts every tree in this test, and accepts every tree in every other
// test in this file too, because nothing else here depends on the chain.
func TestValidateVerifiesTheParentHashOfEveryNonBlankParent(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	senderTree, _, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	if err := senderTree.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("the committed fixture does not validate, so every refusal below could be it: %v", err)
	}
	swept := 0
	for x := uint32(1); x < senderTree.NodeWidth(); x += 2 {
		parent := senderTree.ParentAt(NodeIndex(x))
		if parent == nil {
			continue
		}
		broken := senderTree.Clone()
		tampered := broken.ParentAt(NodeIndex(x)).Clone()
		tampered.ParentHash = tamperParentHash(tampered.ParentHash)
		plantParent(t, broken, NodeIndex(x), tampered)
		swept += 1
		if err := broken.Validate(testTreeValidationContext(t, crypto)); !errors.Is(err, ErrParentHashMismatch) {
			t.Errorf("parent %d carries a tampered parent_hash: err = %v, want ErrParentHashMismatch", x, err)
		}
	}
	if swept < 2 {
		t.Fatalf("a commit left %d non-blank parents to tamper with, so the sweep says little", swept)
	}
}

// TestValidateRefusesASplicedSubtreeThatKeepsTheHashChain is the reason this file calls
// VerifyParentHashes instead of restating RFC 9420 section 7.9.2.
//
// The plan this task came from states that rule with ONE condition where the RFC states three,
// and owner decision 68 records it. The condition it drops is the one that constrains the child's
// RESOLUTION to be exactly the claimant plus the parent's unmerged leaves under it -- and the
// resolution is the set a commit encrypts path secrets to. The tree built here is what dropping
// it costs: every parent_hash chains, every leaf verifies at its own index, every key is unique,
// every unmerged_leaves vector is empty, and node 1 is spliced into the resolution of the root's
// left child beside nothing at all, so the next honest commit would seal a path secret to a key
// the splicer chose.
//
// The four cheaper checks are driven directly and required to pass, which is what makes the
// refusal attributable: a fixture that failed for any other reason would report the same sentinel
// through a different door.
func TestValidateRefusesASplicedSubtreeThatKeepsTheHashChain(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 8)
	spliced, _, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	if err := spliced.Validate(testTreeValidationContext(t, crypto)); err != nil {
		t.Fatalf("the committed fixture does not validate, so the refusal below could be it: %v", err)
	}
	// blanking node 3 widens the resolution of the root's left child from the single node 3 to
	// node 1 and the two leaves under node 5, which is the room the splice needs: a resolution of
	// one entry cannot hold a claimant and a stranger at once.
	if err := spliced.Blank(NodeIndex(3)); err != nil {
		t.Fatalf("Blank(3): %v", err)
	}
	// node 1 is given the parent hash the root's left child is supposed to carry, so it satisfies
	// conditions 1 and 2 of section 7.9.2 exactly.
	rootHash, err := spliced.ParentHash(crypto, NodeIndex(7), NodeIndex(11))
	if err != nil {
		t.Fatalf("ParentHash(7, 11): %v", err)
	}
	claimant := spliced.ParentAt(NodeIndex(1)).Clone()
	claimant.ParentHash = rootHash
	plantParent(t, spliced, NodeIndex(1), claimant)
	// and the committer's own leaf is re-chained onto node 1's new field, so node 1 itself is
	// still claimed by exactly one descendant. Without this the tree would be refused at node 1
	// and the assertion below would hold against a version that never checked the root.
	leafHash, err := spliced.ParentHash(crypto, NodeIndex(1), NodeIndex(2))
	if err != nil {
		t.Fatalf("ParentHash(1, 2): %v", err)
	}
	committer := spliced.Leaf(LeafIndex(0))
	committer.ParentHash = leafHash
	if err := committer.Sign(crypto, members[0].SignaturePriv, testGroupId(), LeafIndex(0)); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	ctx := testTreeValidationContext(t, crypto)
	if err := spliced.validateStructure(); err != nil {
		t.Fatalf("the spliced tree fails the structure check, so the refusal is not the splice: %v", err)
	}
	if err := spliced.validateLeaves(ctx); err != nil {
		t.Fatalf("the spliced tree fails the leaf check, so the refusal is not the splice: %v", err)
	}
	if err := spliced.validateKeyUniqueness(); err != nil {
		t.Fatalf("the spliced tree fails the key uniqueness check, so the refusal is not the splice: %v", err)
	}
	if err := spliced.validateUnmergedLeaves(); err != nil {
		t.Fatalf("the spliced tree fails the unmerged leaves check, so the refusal is not the splice: %v", err)
	}
	// the claimant really is in the resolution of the root's left child, and it is not alone --
	// which is the whole of condition 3 and the whole of what the plan's version does not ask.
	resolution := spliced.Resolution(NodeIndex(3))
	if !slices.Contains(resolution, NodeIndex(1)) || len(resolution) < 2 {
		t.Fatalf("the resolution of node 3 is %v, which is not a claimant spliced in beside others; the fixture does not pose the question",
			resolution)
	}
	if err := spliced.Validate(ctx); !errors.Is(err, ErrParentHashMismatch) {
		t.Fatalf("a spliced subtree with an intact hash chain: err = %v, want ErrParentHashMismatch", err)
	}
}

// ---------------------------------------------------------------------------
// check 6: the binding to the epoch that pinned the tree
// ---------------------------------------------------------------------------

func TestValidateAgainstContextChecksTheTreeHash(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	gc := testGroupContextFor(t, crypto, tree)
	treeHash := cloneBytes(gc.TreeHash)
	if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc); err != nil {
		t.Fatalf("ValidateAgainstContext: %v", err)
	}
	// every byte of the digest and not the first one alone, so a comparison over a prefix is a
	// failure here rather than a thing nothing asks about
	for i := range treeHash {
		gc.TreeHash = cloneBytes(treeHash)
		gc.TreeHash[i] ^= 0xFF
		if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc); !errors.Is(err, ErrTreeHashMismatch) {
			t.Fatalf("byte %d of the pinned tree hash flipped: err = %v, want ErrTreeHashMismatch", i, err)
		}
	}
	for _, c := range []struct {
		name string
		hash []byte
	}{
		{"an absent tree hash", nil},
		{"an empty tree hash", []byte{}},
		{"a truncated tree hash", cloneBytes(treeHash)[:len(treeHash)-1]},
		{"a lengthened tree hash", append(cloneBytes(treeHash), 0)},
	} {
		gc.TreeHash = c.hash
		if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc); !errors.Is(err, ErrTreeHashMismatch) {
			t.Errorf("%s: err = %v, want ErrTreeHashMismatch", c.name, err)
		}
	}
}

// TestValidateAgainstContextRunsEveryCheckValidateRuns is the half a tree hash test cannot see:
// the context binding is an ADDITION to Validate and never a replacement for it, and a version
// that compared the hash and returned would accept every broken tree above as long as its digest
// was pinned honestly.
func TestValidateAgainstContextRunsEveryCheckValidateRuns(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	broken := tree.Clone()
	duplicate := broken.Leaf(LeafIndex(1)).Clone()
	duplicate.EncryptionKey = cloneBytes(broken.Leaf(LeafIndex(0)).EncryptionKey)
	if err := duplicate.Sign(crypto, members[1].SignaturePriv, testGroupId(), LeafIndex(1)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := broken.SetLeaf(LeafIndex(1), duplicate); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}
	// the group context pins the BROKEN tree honestly and agrees with the tree context about
	// every fact check 0 reconciles, so checks 0 and 6 are both satisfied and only the checks
	// Validate makes can refuse it
	gc := testGroupContextFor(t, crypto, broken)
	if err := broken.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc); !errors.Is(err, errDuplicateEncryptionKey) {
		t.Fatalf("err = %v, want errDuplicateEncryptionKey", err)
	}
}

// ---------------------------------------------------------------------------
// check 0: the facts the caller holds twice
// ---------------------------------------------------------------------------

// The pairs that share a Go type across the two structures and do NOT share a fact, each with
// the reason it is not something ValidateAgainstContext could reconcile.
//
// An exemption table beside a derived class, on this package's own terms: the class below is
// computed from the two struct types and cannot be understated by anybody's memory, and the
// entries here are the places where the computation over-approximates. Each is expired by the
// gate itself -- an entry naming a field the derivation no longer reaches fails there rather than
// sitting in the table covering nothing.
var groupContextFactsTheTreeContextDoesNotCarry = map[string]string{
	"Epoch": "a uint64 like the two clocks, and nothing else. The tree carries no epoch and no rule " +
		"of section 7.3 or 7.8 reads one; the epoch binding is the confirmation tag's, one door along",
	"ConfirmedTranscriptHash": "a []byte like the group id, and nothing else. Nothing in a ratchet " +
		"tree is derived from the transcript, and a tree validator that refused a mismatch here would " +
		"be enforcing the key schedule's binding at the wrong door",
}

// TestEveryFactBothContextsCarryIsReconciled is check 0's class, computed rather than listed.
//
// ValidateAgainstContext is handed the group twice, and the two copies were compared nowhere: a
// tree whose leaves were validated under one group id, ciphersuite or extensions vector while the
// GroupContext pinned another was accepted with every check answering nil, because the tree hash
// covers none of those fields and so check 6 cannot see the disagreement either.
//
// The class is DERIVED as every field of GroupContext whose Go type also occurs among
// TreeValidationContext's fields, which is the widest reading of "a fact both structures carry"
// that a program can take without being told what any field means. That over-approximates -- it
// pairs the epoch with a clock and the transcript hash with the group id, on type alone -- and
// the over-approximation is where the table above earns its place rather than where the gate
// gets narrowed: every other member is DRIVEN, by moving the group context's copy away from the
// one the leaves are judged against and requiring the tree to be refused.
//
// A hand written list would have held the three fields the review found and not the fourth this
// derivation reaches; that is the failure this project has now shipped fourteen times, and it is
// the reason the class is not written down here.
func TestEveryFactBothContextsCarryIsReconciled(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	pinned := reflect.TypeOf(GroupContext{})
	judged := reflect.TypeOf(TreeValidationContext{})
	carried := map[reflect.Type]bool{}
	for i := 0; i < judged.NumField(); i += 1 {
		carried[judged.Field(i).Type] = true
	}
	class := []string{}
	for i := 0; i < pinned.NumField(); i += 1 {
		if carried[pinned.Field(i).Type] {
			class = append(class, pinned.Field(i).Name)
		}
	}
	t.Logf("%d group context field(s) whose type the tree context also carries: %v", len(class), class)
	if len(class) == 0 {
		t.Fatal("the class derived from the two struct types is empty, so nothing below is driven and this gate is reporting clean having read nothing")
	}
	// the expiry half, which is what keeps an exemption from outliving the thing it excuses
	for name, why := range groupContextFactsTheTreeContextDoesNotCarry {
		if !slices.Contains(class, name) {
			t.Errorf("GroupContext.%s is exempted as %q and the derivation no longer reaches it; delete the entry", name, why)
		}
	}
	if len(class) == len(groupContextFactsTheTreeContextDoesNotCarry) {
		t.Fatal("every field of the derived class is exempted, so the sweep below drives nothing")
	}
	for _, name := range class {
		if _, exempt := groupContextFactsTheTreeContextDoesNotCarry[name]; exempt {
			continue
		}
		tree, _ := newTestTree(t, crypto, 4)
		ctx := testTreeValidationContext(t, crypto)
		gc := testGroupContextFor(t, crypto, tree)
		if err := tree.ValidateAgainstContext(ctx, gc); err != nil {
			t.Fatalf("the agreeing pair does not validate, so every refusal below could be it: %v", err)
		}
		field := reflect.ValueOf(gc).Elem().FieldByName(name)
		other, ok := aValueOtherThan(field)
		if !ok {
			t.Errorf("GroupContext.%s is a %s and this test has no other value of that kind to move it to, so the field is being swept over rather than driven",
				name, field.Type())
			continue
		}
		field.Set(other)
		err := tree.ValidateAgainstContext(ctx, gc)
		if err == nil {
			t.Errorf("the group context's %s is no longer the one the leaves were judged against and the tree was accepted anyway", name)
			continue
		}
		// the two refusals this door can make about a fact the epoch does not pin. A third
		// would mean the disagreement was reported as something else -- a leaf failure, a
		// malformed tree -- which is a caller sent looking in the wrong place.
		if !errors.Is(err, errGroupContextDisagreement) && !errors.Is(err, ErrTreeHashMismatch) {
			t.Errorf("the group context's %s was moved and the tree was refused as %v, which is neither check 0's refusal nor check 6's",
				name, err)
		}
	}
}

// aValueOtherThan is some value of the same type that is not the one it was handed.
//
// Generic over the kind rather than over the field, because naming the fields is what the gate
// above is written not to do. A number moves by one and a vector grows by a zero element, and
// both are enough to make two copies of one fact disagree -- what is NOT enough is a zero value,
// which for the group id and the extensions vector is a shape a caller can legitimately hold and
// would test the empty case instead of the disagreement.
func aValueOtherThan(v reflect.Value) (reflect.Value, bool) {
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		out := reflect.New(v.Type()).Elem()
		out.SetUint(v.Uint() + 1)
		return out, true
	case reflect.Slice:
		// append rather than truncate, so a nil or empty vector moves too, and to a fresh
		// backing array, so the copy the leaves are judged against is untouched
		return reflect.Append(v, reflect.Zero(v.Type().Elem())), true
	}
	return reflect.Value{}, false
}

// TestValidateAgainstContextComparesEveryEntryOfTheExtensionsVectorAndBothHalvesOfEach is the
// one member of check 0's class that the gate above cannot drive far enough.
//
// aValueOtherThan moves a vector by growing it, which is enough to make two copies disagree and
// is caught by the LENGTH clause alone -- so the gate proves the extensions vector is reconciled
// somehow, and says nothing about how deeply. Measured rather than reasoned: with the body
// comparison deleted, the derived gate and every other test of this package still passed. A group
// context whose policy body had been swapped for another of the same length, under the same code
// point, was the tree's own vector as far as this door could tell.
//
// So the sweep is over every ENTRY and both HALVES of each entry, with the positions read off the
// vector the fixture actually has rather than assumed to be one. Both halves are separately
// reachable failures and the first entry cannot stand for the rest: entry zero is
// required_capabilities, whose disagreement reconcileRequiredCapabilities would refuse on its own
// account, so a test that judged only the first position would pass over a deleted type clause
// and a deleted body clause together.
//
// The length halves are here too, in both directions, because they are the clause the loop stands
// on: with the length guard gone, a vector shorter than the one the leaves were judged against is
// walked to its own end and the entries past it are never compared at all.
func TestValidateAgainstContextComparesEveryEntryOfTheExtensionsVectorAndBothHalvesOfEach(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), testGroupContextFor(t, crypto, tree)); err != nil {
		t.Fatalf("the agreeing pair does not validate, so every refusal below could be it: %v", err)
	}
	entries := len(testTreeValidationContext(t, crypto).GroupExtensions)
	if entries < 2 {
		t.Fatalf("the fixture's group context carries %d extension(s), so a sweep over it says nothing about a comparison that reads one",
			entries)
	}
	type move struct {
		name  string
		moves func(gc *GroupContext)
	}
	cases := []move{
		{"a vector one entry shorter than the one the leaves were judged against", func(gc *GroupContext) {
			gc.Extensions = gc.Extensions[:len(gc.Extensions)-1]
		}},
		{"a vector one entry longer than the one the leaves were judged against", func(gc *GroupContext) {
			gc.Extensions = append(slices.Clone(gc.Extensions),
				Extension{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte("extra")})
		}},
	}
	for at := 0; at < entries; at += 1 {
		cases = append(cases,
			move{fmt.Sprintf("entry %d carrying a different code point", at), func(gc *GroupContext) {
				gc.Extensions[at].ExtensionType += 1
			}},
			move{fmt.Sprintf("entry %d carrying a different body", at), func(gc *GroupContext) {
				// a fresh slice rather than a write through the one it points at, so the move
				// cannot reach the copy the leaves are judged against
				gc.Extensions[at].ExtensionData = append(cloneBytes(gc.Extensions[at].ExtensionData), 0x5A)
			}})
	}
	for _, c := range cases {
		gc := testGroupContextFor(t, crypto, tree)
		c.moves(gc)
		err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc)
		if !errors.Is(err, errGroupContextDisagreement) {
			t.Errorf("%s: err = %v, want errGroupContextDisagreement", c.name, err)
		}
	}
}

// TestValidateAgainstContextReconcilesTheRequiredCapabilitiesBody is the fourth fact both
// structures carry and the only one that is not a field of either.
//
// required_capabilities is an entry INSIDE the extensions vector, and its body is what every leaf
// of the group is held to -- ctx.RequiredCaps is the structure Capabilities.Supports is handed.
// Pinning the vector byte for byte makes the two agree about the bytes and says nothing about the
// structure the caller parsed them into, so the derived gate above cannot reach this one: there
// is no GroupContext FIELD to move.
//
// Three ways two copies of one fact disagree, and all three are refusals a real caller can make:
// requiring what the epoch does not carry, carrying what the leaves are not held to, and holding
// a parse of the body that is not the body. The control is the agreeing pair, so a fixture that
// had stopped validating cannot pass as three refusals that work.
func TestValidateAgainstContextReconcilesTheRequiredCapabilitiesBody(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), testGroupContextFor(t, crypto, tree)); err != nil {
		t.Fatalf("the agreeing pair does not validate, so every refusal below could be it: %v", err)
	}
	// the parse of the body that is not the body: one code point fewer, which is a requirement
	// every leaf of this fixture still satisfies, so nothing but check 0 can refuse it
	narrowed := testRequiredCapabilities()
	narrowed.ExtensionTypes = withoutExtensionType(narrowed.ExtensionTypes, ExtensionTypeUrmessageOwnerSuccessor)
	for _, c := range []struct {
		name   string
		breaks func(ctx *TreeValidationContext, gc *GroupContext)
	}{
		{"the leaves are held to a requirement the epoch does not carry", func(ctx *TreeValidationContext, gc *GroupContext) {
			ctx.GroupExtensions = ctx.GroupExtensions[1:]
			gc.Extensions = gc.Extensions[1:]
		}},
		{"the epoch carries a requirement the leaves are held to none of", func(ctx *TreeValidationContext, gc *GroupContext) {
			ctx.RequiredCaps = nil
		}},
		{"the leaves are held to a parse of the body that is not the body", func(ctx *TreeValidationContext, gc *GroupContext) {
			ctx.RequiredCaps = narrowed
		}},
	} {
		ctx := testTreeValidationContext(t, crypto)
		gc := testGroupContextFor(t, crypto, tree)
		c.breaks(ctx, gc)
		if err := tree.ValidateAgainstContext(ctx, gc); !errors.Is(err, errGroupContextDisagreement) {
			t.Errorf("%s: err = %v, want errGroupContextDisagreement", c.name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// the refusals that are about the arguments rather than the tree
// ---------------------------------------------------------------------------

// TestValidateRefusesAMissingProviderAndAMissingContext holds the three arguments that cannot be
// absent.
//
// A nil context and a context with a nil provider must answer the same thing about the same
// missing thing, which is LeafNode.Validate's rule one layer down; and a nil GroupContext is
// refused rather than dereferenced, because "there was nothing to compare against" must not
// arrive at a caller as a match.
func TestValidateRefusesAMissingProviderAndAMissingContext(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, _ := newTestTree(t, crypto, 4)
	if err := tree.Validate(nil); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("Validate(nil): err = %v, want ErrNilCryptoProvider", err)
	}
	if err := tree.Validate(&TreeValidationContext{}); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("Validate with no provider: err = %v, want ErrNilCryptoProvider", err)
	}
	if err := tree.ValidateAgainstContext(nil, &GroupContext{}); !errors.Is(err, ErrNilCryptoProvider) {
		t.Errorf("ValidateAgainstContext(nil, gc): err = %v, want ErrNilCryptoProvider", err)
	}
	if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), nil); !errors.Is(err, ErrTreeHashMismatch) {
		t.Errorf("ValidateAgainstContext with no group context: err = %v, want ErrTreeHashMismatch", err)
	}
}

// ---------------------------------------------------------------------------
// atomicity
// ---------------------------------------------------------------------------

// TestValidateLeavesTheTreeExactlyAsItFoundIt is task 21's obligation at this door.
//
// A refused tree is a tree the caller never adopted, and a validator that repaired, sorted or
// blanked anything on its way to a refusal hands that caller a structure no peer sent -- which
// the caller then keeps, because the refusal told them the tree was somebody else's problem. The
// whole node array is compared, not the tree hash: a parent_hash field written back is invisible
// to the hash of a tree whose leaves are update sourced, which is exactly the fixture family
// above.
func TestValidateLeavesTheTreeExactlyAsItFoundIt(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	sound, members := newTestTree(t, crypto, 8)
	committed, _, _, _ := createAndEncryptPath(t, crypto, sound, members[0], nil)

	badType := sound.Clone()
	plantNode(t, badType, NodeIndex(5), &Node{NodeType: NodeTypeLeaf, Leaf: sound.Leaf(LeafIndex(0)).Clone()})

	badLeaf := sound.Clone()
	badLeaf.Leaf(LeafIndex(6)).Signature[0] ^= 0xFF

	badKeys := sound.Clone()
	repeat := badKeys.Leaf(LeafIndex(7)).Clone()
	repeat.EncryptionKey = cloneBytes(badKeys.Leaf(LeafIndex(6)).EncryptionKey)
	if err := repeat.Sign(crypto, members[7].SignaturePriv, testGroupId(), LeafIndex(7)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := badKeys.SetLeaf(LeafIndex(7), repeat); err != nil {
		t.Fatalf("SetLeaf: %v", err)
	}

	badUnmerged := sound.Clone()
	plantParent(t, badUnmerged, NodeIndex(13), &ParentNode{
		EncryptionKey:  fillerKey(NodeIndex(13)),
		UnmergedLeaves: []LeafIndex{0},
	})

	badChain := committed.Clone()
	tampered := badChain.ParentAt(NodeIndex(7)).Clone()
	tampered.ParentHash = tamperParentHash(tampered.ParentHash)
	plantParent(t, badChain, NodeIndex(7), tampered)

	cases := []struct {
		name    string
		tree    *RatchetTree
		refused bool
	}{
		{"a sound fresh tree", sound, false},
		{"a sound committed tree", committed, false},
		{"a node of the wrong type", badType, true},
		{"a leaf whose signature does not verify", badLeaf, true},
		{"a repeated encryption key", badKeys, true},
		{"an unmerged leaf outside the subtree", badUnmerged, true},
		{"a broken parent hash chain", badChain, true},
	}
	for _, c := range cases {
		before := treeSnapshot(c.tree)
		err := c.tree.Validate(testTreeValidationContext(t, crypto))
		if refused := err != nil; refused != c.refused {
			t.Errorf("%s: err = %v, refused = %v, want refused = %v", c.name, err, refused, c.refused)
		}
		if after := treeSnapshot(c.tree); !slices.Equal(before, after) {
			t.Errorf("%s: the validator changed the caller's tree", c.name)
			for i := range before {
				if i < len(after) && before[i] != after[i] {
					t.Errorf("  %s -> %s", before[i], after[i])
				}
			}
		}
		// and the same through the context door, which hashes the tree as well as walking it.
		// The context AGREES with the tree context about every fact check 0 reconciles and
		// pins a hash no tree has, so the refusal below is check 6's rather than check 0's --
		// which is the refusal this test is about, and a disagreement here would have made
		// every row of the table pass without the tree ever being hashed.
		gc := testGroupContextFor(t, crypto, sound)
		gc.TreeHash = bytes.Repeat([]byte{0x00}, 32)
		before = treeSnapshot(c.tree)
		if err := c.tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc); err == nil {
			t.Errorf("%s: ValidateAgainstContext accepted a tree the group context does not pin", c.name)
		}
		if after := treeSnapshot(c.tree); !slices.Equal(before, after) {
			t.Errorf("%s: ValidateAgainstContext changed the caller's tree", c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// the tree with nothing in it
// ---------------------------------------------------------------------------

// blankTreeOfWidth is a node array of the right shape with nothing in it.
//
// Built by poking the array for plantNode's reason and one more: NewRatchetTree is the one leaf
// tree and there is no constructor for a wider EMPTY one, because no caller has a use for it --
// which is exactly the input this test is about. Growing a real tree and then blanking every leaf
// would go through Blank, which drops unmerged entries and is a different input.
func blankTreeOfWidth(t testing.TB, leaves LeafCount) *RatchetTree {
	t.Helper()
	tree := &RatchetTree{nodes: make([]*Node, NodeWidth(leaves))}
	if tree.LeafWidth() != leaves {
		t.Fatalf("a %d node array is %d leaves wide, want %d", tree.NodeWidth(), tree.LeafWidth(), leaves)
	}
	if occupied := tree.NonBlankLeaves(); len(occupied) != 0 {
		t.Fatalf("a freshly made blank array holds %v", occupied)
	}
	return tree
}

// TestValidateAcceptsATreeWithNoMembersAtEveryWidth records a decision rather than catching a bug.
//
// A tree with no non-blank leaf at all passes every one of the five checks, at every width, and
// passes check 6 too against its own hash -- vacuously, each of them: the width still inverts,
// the leaf sweep has nothing to judge, both key maps stay empty, no parent slot is occupied and
// VerifyParentHashes has no claimant to find. Nothing in the repository said so. The commit that
// added this file named the acceptance in its message and the message is not somewhere a later
// task reads, so the only record of it was prose that had already scrolled past.
//
// It is asserted as ACCEPTANCE and not fixed as a refusal, deliberately. "A group has at least
// one member" is a rule about a group and not about a node array; the tree a removal empties is a
// legal intermediate, and a refusal here would surface as a decode failure at whichever door
// reached it first. The door that owes the membership refusal is the one that turns a tree into a
// group -- group.go's Welcome and snapshot paths -- and it does not exist yet. When it does, this
// test is what tells whoever writes it that the tree layer will not have refused this for them.
func TestValidateAcceptsATreeWithNoMembersAtEveryWidth(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, leaves := range []LeafCount{1, 2, 4, 8} {
		tree := blankTreeOfWidth(t, leaves)
		if err := tree.Validate(testTreeValidationContext(t, crypto)); err != nil {
			t.Errorf("%d leaves, none of them occupied: Validate = %v, want nil -- if the tree layer has taken on the membership rule, group.go must stop being told it owns it",
				leaves, err)
			continue
		}
		gc := testGroupContextFor(t, crypto, tree)
		if err := tree.ValidateAgainstContext(testTreeValidationContext(t, crypto), gc); err != nil {
			t.Errorf("%d leaves, none of them occupied, against a group context pinning its own hash: err = %v, want nil",
				leaves, err)
		}
		// and the hash it is pinned by is a real one rather than an accident of the empty
		// array, so the acceptance above is check 6 agreeing and not check 6 comparing two
		// absent values
		if len(gc.TreeHash) == 0 {
			t.Errorf("%d leaves: the empty tree hashes to nothing at all, so check 6 above compared two absent values", leaves)
		}
	}
}

// ---------------------------------------------------------------------------
// ValSem206 and ValSem207: the keys an UpdatePath introduces
// ---------------------------------------------------------------------------
//
// The ValSem-numbered names are the validation plan's -- TestValSem206_PathLeafDuplicateEncryptionKey
// and TestValSem207_PathNodeDuplicateEncryptionKey -- and two functions of one name in one go
// package do not compile, so everything here is named for the behaviour it holds. What this file
// owes that plan is the production surface those two drive, and the property sweeps a
// table-driven ValSem row cannot make: that both loops read every element of their set.

// updatePathUniquenessTree is an eight member tree with real PARENT nodes in it, before any path
// has been taken from it.
//
// The parents are the reason this helper exists. CheckUpdatePathKeyUniqueness reads leaves and
// parents through two different arms of one switch, and newTestTree's parents are all blank -- so
// a fixture built straight from it exercises the leaf arm and nothing else, and a walk that never
// looked at a parent would pass every assertion made over it while accepting a path that steals a
// parent's key. Two commits from members on the far side of the tree fill four parent positions
// with real keys and leave three blank, so every sweep below runs over occupied leaves, occupied
// parents and blank positions in one array.
func updatePathUniquenessTree(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
	t.Helper()
	tree, members := newTestTree(t, crypto, 8)
	for _, committer := range []int{7, 4} {
		committed, _, _, _ := createAndEncryptPath(t, crypto, tree, members[committer], nil)
		tree = committed
	}
	leaves, parents, blanks := 0, 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		switch node := tree.Get(NodeIndex(x)); {
		case node == nil:
			blanks += 1
		case node.Leaf != nil:
			leaves += 1
		default:
			parents += 1
		}
	}
	// the fixture's own claim about itself, checked rather than assumed: a later change to
	// newTestTree or to the filtered path could quietly empty one of the three populations, and
	// every sweep below would go on passing over whatever was left.
	if leaves < 4 || parents < 2 || blanks < 1 {
		t.Fatalf("the fixture holds %d occupied leaves, %d occupied parents and %d blanks; a sweep over it states nothing about the arm with nothing in it",
			leaves, parents, blanks)
	}
	return tree, members
}

// updatePathFrom is one member's freshly published path over a tree, with the two claims every
// sweep below rests on checked once here rather than at each of them: the path has a middle to
// put an offender in, and untampered it is accepted.
func updatePathFrom(t *testing.T, crypto CryptoProvider, tree *RatchetTree, member *testTreeMember) *UpdatePath {
	t.Helper()
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, member, nil)
	if len(path.Nodes) < 3 {
		t.Fatalf("the path from leaf %d publishes %d nodes; the position sweeps below need a middle to put an offender in",
			member.LeafIndex, len(path.Nodes))
	}
	if err := CheckUpdatePathKeyUniqueness(tree, path); err != nil {
		t.Fatalf("the untampered path from leaf %d was refused: %v", member.LeafIndex, err)
	}
	return path
}

// updatePathUniquenessFixture is that tree, one member's path over it, and the member who
// published it.
//
// The committer is NOT the group's first member, and that is this fixture's second claim after
// the parents. Leaf L lives at node 2L, so leaf 0 is the single leaf whose leaf index and node
// index are the same number: a fixture committing from it cannot tell an exemption derived
// through LeafIndex.NodeIndex from one that used the leaf index raw, nor from one hardcoded to
// node 0, and both of those point at somebody ELSE's leaf the moment a real group commits from
// anywhere but its first member. Every test in this family drove members[0] once, and both of
// those mutations survived the whole suite.
//
// The committer is DERIVED from that property rather than named, so it stays off the diagonal if
// newTestTree's ordering ever changes.
func updatePathUniquenessFixture(t *testing.T, crypto CryptoProvider) (*RatchetTree, *UpdatePath, *testTreeMember) {
	t.Helper()
	tree, members := updatePathUniquenessTree(t, crypto)
	var sender *testTreeMember
	for _, member := range members {
		if NodeIndex(uint32(member.LeafIndex)) != member.LeafIndex.NodeIndex() {
			sender = member
			break
		}
	}
	if sender == nil {
		t.Fatal("no member of the fixture sits at a leaf whose index differs from its node index, so nothing below can tell 2L from L")
	}
	return tree, updatePathFrom(t, crypto, tree, sender), sender
}

// pathWithLeafKey and pathWithNodeKey are the two tamperings, each over a COPY, so a sweep cannot
// carry one iteration's offender into the next. The nodes slice is cloned because UpdatePathNode
// is a value type in it, and assigning through the original slice would rewrite the fixture every
// later iteration reads.
func pathWithLeafKey(path *UpdatePath, key HpkePublicKey) *UpdatePath {
	tampered := &UpdatePath{LeafNode: *path.LeafNode.Clone(), Nodes: slices.Clone(path.Nodes)}
	tampered.LeafNode.EncryptionKey = cloneBytes(key)
	return tampered
}

func pathWithNodeKey(path *UpdatePath, at int, key HpkePublicKey) *UpdatePath {
	tampered := &UpdatePath{LeafNode: *path.LeafNode.Clone(), Nodes: slices.Clone(path.Nodes)}
	tampered.Nodes[at].EncryptionKey = cloneBytes(key)
	return tampered
}

// TestUpdatePathLeafKeyUniqueness is ValSem206's shape: the path's own leaf node may not publish
// a key the tree already has.
func TestUpdatePathLeafKeyUniqueness(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	if err := CheckUpdatePathKeyUniqueness(tree, path); err != nil {
		t.Fatalf("a fresh path must be unique: %v", err)
	}
	tampered := pathWithLeafKey(path, tree.Leaf(LeafIndex(2)).EncryptionKey)
	if err := CheckUpdatePathKeyUniqueness(tree, tampered); !errors.Is(err, errDuplicateEncryptionKey) {
		t.Fatalf("err = %v, want errDuplicateEncryptionKey", err)
	}
}

// TestUpdatePathNodeKeyUniqueness is ValSem207's shape: a path node may not publish a key the
// tree already has, nor one another node of the same path publishes.
func TestUpdatePathNodeKeyUniqueness(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)

	// a path node reusing a leaf's key that is already in the tree.
	tampered := pathWithNodeKey(path, 0, tree.Leaf(LeafIndex(2)).EncryptionKey)
	if err := CheckUpdatePathKeyUniqueness(tree, tampered); !errors.Is(err, errDuplicateEncryptionKey) {
		t.Fatalf("reused tree key: err = %v, want errDuplicateEncryptionKey", err)
	}

	// two nodes of the same path sharing a key.
	tampered = pathWithNodeKey(path, 1, path.Nodes[0].EncryptionKey)
	if err := CheckUpdatePathKeyUniqueness(tree, tampered); !errors.Is(err, errDuplicateEncryptionKey) {
		t.Fatalf("repeated path key: err = %v, want errDuplicateEncryptionKey", err)
	}

	// the sender's own outgoing leaf key is being replaced, so it does not count. This is the
	// case that fails if the sender is guessed rather than recovered from the path's own
	// signature key.
	reused := pathWithLeafKey(path, tree.Leaf(members[0].LeafIndex).EncryptionKey)
	if err := CheckUpdatePathKeyUniqueness(tree, reused); err != nil {
		t.Fatalf("the sender's own leaf key must not collide with itself: %v", err)
	}
}

// TestUpdatePathKeyUniquenessWithAnUnknownSender: a path whose leaf signature key is in no leaf
// of the tree is not from a member, so nothing is being replaced and every key in it must be new
// -- the one it copied from the member it is impersonating included.
func TestUpdatePathKeyUniquenessWithAnUnknownSender(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	stranger := pathWithLeafKey(path, tree.Leaf(members[0].LeafIndex).EncryptionKey)
	stranger.LeafNode.SignatureKey = SignaturePublicKey(bytes.Repeat([]byte{0x7E}, 32))
	if err := CheckUpdatePathKeyUniqueness(tree, stranger); !errors.Is(err, errDuplicateEncryptionKey) {
		t.Fatalf("err = %v, want errDuplicateEncryptionKey", err)
	}
}

// TestUpdatePathKeyUniquenessReadsEveryOccupiedNodeOfTheTree is the sweep this file's header
// argues for, run over the tree side of the check.
//
// The set is every occupied position of the node array, and the positions are DERIVED from the
// width rather than listed, so a fixture that grows a node or loses one is swept as it stands.
// Every one of them has its key stolen -- once into the path's leaf node, and once into each
// position of the path -- and each theft must be refused. A build of the existing-key map bounded
// to the first element, or to the leaves, or to the parents, accepts every theft from outside
// whatever it read, and this is the assertion that sees it.
//
// The single accepted pair in the whole product is the committer's own leaf key at the path's
// LEAF node, which is the exemption and has two tests of its own. It is skipped at that one
// position and stolen like any other node's key at the rest, because a path NODE carrying the
// committer's outgoing key replaces nothing and is refused.
func TestUpdatePathKeyUniquenessReadsEveryOccupiedNodeOfTheTree(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, path, sender := updatePathUniquenessFixture(t, crypto)
	before := treeSnapshot(tree)
	senderNode := sender.LeafIndex.NodeIndex()
	stolenFromLeaves, stolenFromParents := 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := tree.Get(NodeIndex(x))
		if node == nil {
			continue
		}
		// treekem_test.go's reader, which is fatal on a blank and on a body-less node; the
		// sweep has already skipped the blanks and the fixture has no body-less node.
		key := encryptionKeyAt(t, tree, NodeIndex(x))
		if NodeIndex(x) != senderNode {
			if err := CheckUpdatePathKeyUniqueness(tree, pathWithLeafKey(path, key)); !errors.Is(err, errDuplicateEncryptionKey) {
				t.Errorf("the path's leaf node publishing node %d's key: err = %v, want errDuplicateEncryptionKey",
					x, err)
			}
		}
		for i := range path.Nodes {
			if err := CheckUpdatePathKeyUniqueness(tree, pathWithNodeKey(path, i, key)); !errors.Is(err, errDuplicateEncryptionKey) {
				t.Errorf("path node %d of %d publishing node %d's key: err = %v, want errDuplicateEncryptionKey",
					i, len(path.Nodes), x, err)
			}
		}
		if node.Leaf != nil {
			stolenFromLeaves += 1
		} else {
			stolenFromParents += 1
		}
	}
	// the coverage claim, checked rather than assumed: both arms of the switch had something
	// taken from them, so a walk reading only one of them is inside this sweep rather than beside
	// it.
	if stolenFromLeaves < 3 || stolenFromParents < 2 {
		t.Fatalf("the sweep stole from %d leaves and %d parents; an arm nothing was stolen from is an arm this states nothing about",
			stolenFromLeaves, stolenFromParents)
	}
	if after := treeSnapshot(tree); !slices.Equal(before, after) {
		t.Error("CheckUpdatePathKeyUniqueness changed the caller's tree; it is a refusal surface over a tree nobody has agreed to yet")
	}
	t.Logf("%d occupied leaves and %d occupied parents swept, at %d path positions each",
		stolenFromLeaves, stolenFromParents, len(path.Nodes)+1)
}

// TestUpdatePathKeyUniquenessReadsEveryPositionOfThePath is the same sweep over the other set:
// the keys the path itself introduces, which are the leaf node's and every path node's.
//
// Every ORDERED PAIR of positions is tried, with the source's key written into the target, so a
// loop bounded at the first position and a loop that stops before its last are both refused by
// some pair. The positions come from the path's own length; nothing here names an index.
func TestUpdatePathKeyUniquenessReadsEveryPositionOfThePath(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, path, _ := updatePathUniquenessFixture(t, crypto)
	// position 0 is the path's leaf node and position i+1 is path.Nodes[i], which is the order
	// the encoder writes them in and the order a refusal names them in.
	positions := len(path.Nodes) + 1
	keyAt := func(at int) HpkePublicKey {
		if at == 0 {
			return path.LeafNode.EncryptionKey
		}
		return path.Nodes[at-1].EncryptionKey
	}
	withKeyAt := func(at int, key HpkePublicKey) *UpdatePath {
		if at == 0 {
			return pathWithLeafKey(path, key)
		}
		return pathWithNodeKey(path, at-1, key)
	}
	pairs := 0
	for source := 0; source < positions; source += 1 {
		for target := 0; target < positions; target += 1 {
			if source == target {
				continue
			}
			tampered := withKeyAt(target, keyAt(source))
			if err := CheckUpdatePathKeyUniqueness(tree, tampered); !errors.Is(err, errDuplicateEncryptionKey) {
				t.Errorf("position %d of %d carrying position %d's key: err = %v, want errDuplicateEncryptionKey",
					target, positions, source, err)
			}
			pairs += 1
		}
	}
	if want := positions * (positions - 1); pairs != want {
		t.Fatalf("the sweep tried %d ordered pairs and the path has %d positions, so it should have tried %d",
			pairs, positions, want)
	}
	t.Logf("%d ordered pairs over %d path positions", pairs, positions)
}

// TestUpdatePathKeyUniquenessExemptsTheCommittersLeafAndNoOther holds the one exemption to its
// exact width on the KEY axis, with the committer derived from the tree rather than fixed at its
// first member.
//
// Three ways to get this wrong, and each is silent. Exempt nothing and every honest commit is
// refused, which at least fails the first time a group commits. Exempt every LEAF -- which is
// what dropping the index comparison and keeping the arm does -- and a commit may publish any
// member's encryption key as its own, which is a member reading path secrets sealed to somebody
// else and nothing anywhere reporting it. Exempt the WRONG leaf -- which is what comparing a node
// index against a raw LEAF index does, since leaf L lives at node 2L -- and both of those
// failures happen at once, for every committer except leaf 0.
//
// That third one is why this commits from every occupied leaf in turn instead of from one. Leaf 0
// is the only leaf where 2L and L are the same number, so the version of this test that drove one
// path from members[0] passed with the exemption pointed at the leaf index raw AND with it
// hardcoded to node 0. The committers come from the tree; nothing here names a leaf.
func TestUpdatePathKeyUniquenessExemptsTheCommittersLeafAndNoOther(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := updatePathUniquenessTree(t, crypto)
	committers, offDiagonal := 0, 0
	for _, sender := range members {
		path := updatePathFrom(t, crypto, tree, sender)
		outgoing := tree.Leaf(sender.LeafIndex).EncryptionKey
		if err := CheckUpdatePathKeyUniqueness(tree, pathWithLeafKey(path, outgoing)); err != nil {
			t.Errorf("leaf %d committing its own outgoing leaf key, which is the key its path replaces: err = %v, want nil",
				sender.LeafIndex, err)
		}
		refused := 0
		for _, i := range tree.NonBlankLeaves() {
			if i == sender.LeafIndex {
				continue
			}
			tampered := pathWithLeafKey(path, tree.Leaf(i).EncryptionKey)
			if err := CheckUpdatePathKeyUniqueness(tree, tampered); !errors.Is(err, errDuplicateEncryptionKey) {
				t.Errorf("leaf %d committing and publishing leaf %d's encryption key: err = %v, want errDuplicateEncryptionKey",
					sender.LeafIndex, i, err)
				continue
			}
			refused += 1
		}
		if refused < 3 {
			t.Fatalf("leaf %d committing: only %d other leaves were refused; an exemption widened to every leaf has to be visible at more than one of them",
				sender.LeafIndex, refused)
		}
		// the exemption is the COMMITTER'S and follows the signature key rather than the
		// position. A path signed by a stranger replaces nothing, so the same key is a duplicate
		// again -- which is TestUpdatePathKeyUniquenessWithAnUnknownSender's claim, restated here
		// against the fixture that has parents in it so the two cannot drift.
		stranger := pathWithLeafKey(path, outgoing)
		stranger.LeafNode.SignatureKey = SignaturePublicKey(bytes.Repeat([]byte{0x7E}, 32))
		if err := CheckUpdatePathKeyUniqueness(tree, stranger); !errors.Is(err, errDuplicateEncryptionKey) {
			t.Errorf("leaf %d: a stranger publishing that committer's outgoing key: err = %v, want errDuplicateEncryptionKey",
				sender.LeafIndex, err)
		}
		committers += 1
		if NodeIndex(uint32(sender.LeafIndex)) != sender.LeafIndex.NodeIndex() {
			offDiagonal += 1
		}
	}
	// the coverage claim, checked rather than assumed. A sweep that only ever reached leaf 0 --
	// the one leaf whose leaf index and node index are equal -- states nothing about which of the
	// two numbers the exemption is derived from, which is the shape this test replaced.
	if committers < 4 || offDiagonal < 3 {
		t.Fatalf("the sweep committed from %d leaves, %d of them at a leaf index that is not also its node index; the second number is what says the exemption converts",
			committers, offDiagonal)
	}
	t.Logf("%d committers swept, %d of them off the leaf-index-equals-node-index diagonal",
		committers, offDiagonal)
}

// TestUpdatePathKeyUniquenessExemptsOnlyThePathsLeafPosition is the exemption's other axis: not
// which KEY is exempt, but at which POSITION of the path it is exempt.
//
// The committer's outgoing leaf key is exempt because the path's leaf node REPLACES it. A path
// node is a parent position and replaces nothing at the committer's leaf, so that key appearing
// there is a retired key republished somewhere it never lived, and it is refused like any other
// collision.
//
// The distinction is invisible to every errors.Is assertion in this file unless something states
// it, because the two readings differ only in WHERE the exemption is written. Taken out of the
// tree sweep, the key leaves the set entirely and the path's other len(Nodes) positions are
// exempted along with the leaf, silently. Taken at the comparison, only the pair it is an
// exemption for is covered. The positions come from the path's own length.
func TestUpdatePathKeyUniquenessExemptsOnlyThePathsLeafPosition(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, path, sender := updatePathUniquenessFixture(t, crypto)
	outgoing := tree.Leaf(sender.LeafIndex).EncryptionKey
	if err := CheckUpdatePathKeyUniqueness(tree, pathWithLeafKey(path, outgoing)); err != nil {
		t.Fatalf("the path's leaf node is the one position that replaces the committer's outgoing key: err = %v, want nil", err)
	}
	refused := 0
	for i := range path.Nodes {
		tampered := pathWithNodeKey(path, i, outgoing)
		if err := CheckUpdatePathKeyUniqueness(tree, tampered); !errors.Is(err, errDuplicateEncryptionKey) {
			t.Errorf("path node %d of %d publishing the committer's outgoing leaf key: err = %v, want errDuplicateEncryptionKey",
				i, len(path.Nodes), err)
			continue
		}
		refused += 1
	}
	if refused != len(path.Nodes) || refused < 3 {
		t.Fatalf("%d of the path's %d node positions were refused the committer's outgoing key; an exemption widened to the whole path has to be visible at more than one of them",
			refused, len(path.Nodes))
	}
	t.Logf("the path's leaf node exempt, %d path node positions refused", refused)
}

// TestUpdatePathKeyUniquenessRefusesTheCommittersOutgoingKeyWhereverElseItSits is the exemption's
// third axis, and the one that decides how the tree side has to be INDEXED.
//
// The exemption says one node's key is not a collision. It does not say the KEY is not a
// collision, and the difference shows up only in a tree that already carries that key twice: the
// committer's leaf, and somewhere else. Then the path's leaf node republishing it collides with
// the somewhere-else, which the exemption does not cover, and it has to be refused.
//
// That tree is reachable rather than hypothetical. This function is called over a POST-PROPOSAL
// tree that has not been re-validated -- the same reason its switch answers a body-less node
// instead of dereferencing it -- so a proposal that installed a duplicate key is upstream of
// this call, not downstream. A tree-side map keyed by bytes and holding ONE index per key hides
// one of the two occurrences behind the other, and which one it hides is decided by nothing more
// than the sweep's direction. So the offender is planted at every blank position of the tree in
// turn, above the committer's leaf and below it, and the positions are derived from the tree.
func TestUpdatePathKeyUniquenessRefusesTheCommittersOutgoingKeyWhereverElseItSits(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, path, sender := updatePathUniquenessFixture(t, crypto)
	senderNode := sender.LeafIndex.NodeIndex()
	outgoing := tree.Leaf(sender.LeafIndex).EncryptionKey
	republished := pathWithLeafKey(path, outgoing)
	// the control at the same shape and no second occurrence, so a fixture that was already
	// refusing this cannot pass as a sweep that works.
	if err := CheckUpdatePathKeyUniqueness(tree, republished); err != nil {
		t.Fatalf("the control: with the key at the committer's leaf and nowhere else it is the key the path replaces: err = %v, want nil", err)
	}
	below, above := 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		if tree.Get(NodeIndex(x)) != nil {
			continue
		}
		// a COPY per position, so one iteration's plant cannot ride into the next.
		doubled := tree.Clone()
		plantParent(t, doubled, NodeIndex(x), &ParentNode{EncryptionKey: cloneBytes(outgoing)})
		if err := CheckUpdatePathKeyUniqueness(doubled, republished); !errors.Is(err, errDuplicateEncryptionKey) {
			t.Errorf("the committer's outgoing key also at node %d, its own leaf being node %d: err = %v, want errDuplicateEncryptionKey",
				x, senderNode, err)
			continue
		}
		if NodeIndex(x) < senderNode {
			below += 1
		} else {
			above += 1
		}
	}
	// the coverage claim. A map holding only the FIRST index it saw for a key and one holding
	// only the LAST are each right at one side of the committer's leaf and wrong at the other, so
	// a sweep that reached only one side states nothing about which was written.
	if below < 1 || above < 1 {
		t.Fatalf("the offender was planted at %d blank positions below the committer's leaf and %d above it; one of those sides being empty is the side this states nothing about",
			below, above)
	}
	t.Logf("the committer's outgoing key doubled at %d blanks below its leaf and %d above, refused at every one",
		below, above)
}

// TestUpdatePathKeyUniquenessNamesTheCollisionItRefused reads the refusal TEXT, which every other
// assertion about this function takes on trust.
//
// errors.Is is satisfied by any refusal carrying the sentinel, so a loop reporting position 0 for
// every path node, or one naming node 0 as the collider whatever collided, passes every sweep in
// this file -- and building the introduced list in published order was done precisely so that the
// position a refusal names is the position the encoder wrote. extension_test.go's
// required-capability gate reads its message for the same reason: a refusal naming a different
// entry is a loop reading the wrong index, and the sentinel cannot tell you that.
//
// Both halves of both messages are DERIVED: the path positions from the path's length, the tree
// positions from the tree's width.
func TestUpdatePathKeyUniquenessNamesTheCollisionItRefused(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, path, sender := updatePathUniquenessFixture(t, crypto)
	senderNode := sender.LeafIndex.NodeIndex()
	// position 0 is the path's leaf node and position i+1 is path.Nodes[i], which is the order
	// the encoder writes them in and the order a refusal names them in.
	positions := len(path.Nodes) + 1
	where := func(at int) string {
		if at == 0 {
			return "the path's leaf node"
		}
		return fmt.Sprintf("path node %d", at-1)
	}
	keyAt := func(at int) HpkePublicKey {
		if at == 0 {
			return path.LeafNode.EncryptionKey
		}
		return path.Nodes[at-1].EncryptionKey
	}
	withKeyAt := func(at int, key HpkePublicKey) *UpdatePath {
		if at == 0 {
			return pathWithLeafKey(path, key)
		}
		return pathWithNodeKey(path, at-1, key)
	}
	// the tree-collision message names the path position that published the key and the node the
	// key was already at, so both indices are read back at every pair of the two sets.
	occupied, named := 0, 0
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		if tree.Get(NodeIndex(x)) == nil {
			continue
		}
		occupied += 1
		key := encryptionKeyAt(t, tree, NodeIndex(x))
		for at := 0; at < positions; at += 1 {
			// the exemption's one accepted pair, which refuses nothing and so names nothing.
			if at == 0 && NodeIndex(x) == senderNode {
				continue
			}
			err := CheckUpdatePathKeyUniqueness(tree, withKeyAt(at, key))
			want := fmt.Sprintf("%s publishes the key already at node %d", where(at), x)
			if !errors.Is(err, errDuplicateEncryptionKey) || !strings.Contains(err.Error(), want) {
				t.Errorf("position %d of %d carrying node %d's key: err = %v, want a refusal saying %q",
					at, positions, x, err, want)
				continue
			}
			named += 1
		}
	}
	// the within-path message names both positions, and which of the two it calls the offender is
	// decided by publication order rather than by which one was tampered with.
	pairs := 0
	for source := 0; source < positions; source += 1 {
		for target := 0; target < positions; target += 1 {
			if source == target {
				continue
			}
			err := CheckUpdatePathKeyUniqueness(tree, withKeyAt(target, keyAt(source)))
			first, second := source, target
			if target < source {
				first, second = target, source
			}
			want := fmt.Sprintf("%s publishes the same key as %s", where(second), where(first))
			if !errors.Is(err, errDuplicateEncryptionKey) || !strings.Contains(err.Error(), want) {
				t.Errorf("position %d of %d carrying position %d's key: err = %v, want a refusal saying %q",
					target, positions, source, err, want)
				continue
			}
			pairs += 1
		}
	}
	// the coverage claim, derived from the two sets rather than picked: every occupied node at
	// every path position, less the single exempt pair, and every ordered pair of positions.
	wantNamed := occupied*positions - 1
	wantPairs := positions * (positions - 1)
	if named != wantNamed || pairs != wantPairs {
		t.Fatalf("the gate read %d of %d tree collisions and %d of %d within-path collisions by text",
			named, wantNamed, pairs, wantPairs)
	}
	t.Logf("%d tree-collision refusals and %d within-path refusals read by text", named, pairs)
}

// TestUpdatePathKeyUniquenessKeepsItsRefusalsDistinct pins the function's error SURFACE, which is
// several sentinels rather than one.
//
// A missing tree and an occupied node with no body are faults of the TREE; a collision is a fault
// of the PATH. A consumer that reports all of them under one ValSem name tells the group that a
// commit duplicated an encryption key when what actually happened is that the tree the commit was
// judged against was malformed -- and the group lifecycle plan's funnel does exactly that today,
// because it wraps this error with %v instead of %w and so flattens the chain. That funnel is
// that plan's line to fix; what is owed HERE is that the refusals are distinguishable at all,
// which nothing stated before this test.
//
// The set of sentinels is DERIVED from the function's own body rather than listed, because a
// listed set goes stale on the commit that adds a refusal -- and the added refusal is exactly the
// one nobody would think to write a row for. Every error-named identifier the body mentions has
// to have a row here, and every row has to actually produce its sentinel and none of the others.
func TestUpdatePathKeyUniquenessKeepsItsRefusalsDistinct(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, path, _ := updatePathUniquenessFixture(t, crypto)
	// planted on a COPY, so the collision row below still runs against a well formed tree.
	bodiless := tree.Clone()
	plantNode(t, bodiless, NodeIndex(bodiless.NodeWidth()-2), &Node{NodeType: NodeTypeParent})
	stolen := pathWithLeafKey(path, encryptionKeyAt(t, tree, NodeIndex(tree.NodeWidth()-2)))
	table := []struct {
		name     string
		named    string
		sentinel error
		run      func() error
	}{
		{
			name:     "no tree at all",
			named:    "ErrTreeMalformed",
			sentinel: ErrTreeMalformed,
			run:      func() error { return CheckUpdatePathKeyUniqueness(nil, path) },
		},
		{
			name:     "no path at all",
			named:    "errNilUpdatePath",
			sentinel: errNilUpdatePath,
			run:      func() error { return CheckUpdatePathKeyUniqueness(tree, nil) },
		},
		{
			name:     "an occupied node carrying neither body",
			named:    "ErrNodeTypeMismatch",
			sentinel: ErrNodeTypeMismatch,
			run:      func() error { return CheckUpdatePathKeyUniqueness(bodiless, path) },
		},
		{
			name:     "a path publishing a key the tree already holds",
			named:    "errDuplicateEncryptionKey",
			sentinel: errDuplicateEncryptionKey,
			run:      func() error { return CheckUpdatePathKeyUniqueness(tree, stolen) },
		},
	}
	covered := map[string]bool{}
	for _, one := range table {
		covered[one.named] = true
		err := one.run()
		if !errors.Is(err, one.sentinel) {
			t.Errorf("%s: err = %v, want %s", one.name, err, one.named)
			continue
		}
		// and NOT any of the others, which is the half that says the surface is several answers
		// rather than one value wearing several messages.
		for _, other := range table {
			if other.named == one.named {
				continue
			}
			if errors.Is(err, other.sentinel) {
				t.Errorf("%s answered %s as well as %s; a caller cannot tell a malformed tree from a duplicated key",
					one.name, other.named, one.named)
			}
		}
	}
	// the class, read off the source rather than trusted to the table above.
	for _, name := range errorNamesMentionedIn(t, "tree_sync.go", "CheckUpdatePathKeyUniqueness") {
		if !covered[name] {
			t.Errorf("CheckUpdatePathKeyUniqueness can answer %s and no row above drives it; a refusal nothing distinguishes is a refusal a caller reports under somebody else's name",
				name)
		}
	}
	t.Logf("%d refusals held apart", len(table))
}

// errorNamesMentionedIn is every error-named identifier one function's body mentions, read off
// the parse tree.
//
// The class this derives is "the sentinels this function can answer", and it is READ rather than
// listed for the reason this file's header gives: a hand written list of them understates the
// real set the day somebody adds a refusal, and the added refusal is precisely the one the list
// was never updated for. The name test is this package's own spelling for a sentinel -- err or
// Err followed by a capital -- which is what every one of them here is called, and which excludes
// the local err variable by that capital.
func errorNamesMentionedIn(t *testing.T, file string, function string) []string {
	t.Helper()
	source := mustParseSource(t, file)
	found := map[string]bool{}
	ast.Inspect(source.declarationOf(t, "", function).Body, func(node ast.Node) bool {
		identifier, isIdentifier := node.(*ast.Ident)
		if !isIdentifier {
			return true
		}
		for _, prefix := range []string{"err", "Err"} {
			rest, cut := strings.CutPrefix(identifier.Name, prefix)
			if cut && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z' {
				found[identifier.Name] = true
			}
		}
		return true
	})
	if len(found) == 0 {
		t.Fatalf("no error name was found in %s, so the coverage check reading this judged nothing", function)
	}
	names := []string{}
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestUpdatePathKeyUniquenessRefusesAnAbsentArgument: a missing tree or a missing path is a check
// that was SKIPPED, and answering nil for it is the accept-because-it-never-looked shape every
// ValSem on this project has failed as.
func TestUpdatePathKeyUniquenessRefusesAnAbsentArgument(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	if err := CheckUpdatePathKeyUniqueness(nil, path); !errors.Is(err, ErrTreeMalformed) {
		t.Errorf("no tree: err = %v, want ErrTreeMalformed", err)
	}
	if err := CheckUpdatePathKeyUniqueness(tree, nil); !errors.Is(err, errNilUpdatePath) {
		t.Errorf("no path: err = %v, want errNilUpdatePath", err)
	}
}

// TestUpdatePathKeyUniquenessRefusesAnOccupiedNodeWithNoBody covers the switch's last arm.
//
// validateStructure refuses this shape ahead of every caller that has validated its tree, and
// this function is reachable from callers that have not -- the group lifecycle plan calls it over
// a tree it has just applied proposals to, rather than over one it has just decoded. So the arm is
// a refusal rather than a nil dereference, and the difference between those two is a panic in a
// message handler.
func TestUpdatePathKeyUniquenessRefusesAnOccupiedNodeWithNoBody(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	tree, members := newTestTree(t, crypto, 4)
	_, path, _, _ := createAndEncryptPath(t, crypto, tree, members[0], nil)
	// planted at the last parent position rather than the first, so a walk that stopped early
	// would answer nil here instead of the refusal.
	plantNode(t, tree, NodeIndex(tree.NodeWidth()-2), &Node{NodeType: NodeTypeParent})
	if err := CheckUpdatePathKeyUniqueness(tree, path); !errors.Is(err, ErrNodeTypeMismatch) {
		t.Errorf("err = %v, want ErrNodeTypeMismatch", err)
	}
}
