// The shared n-member ratchet tree every later task of this plan builds its fixtures on, and
// the check that stops a wrong fixture from being agreed with twenty times over.
//
// A builder is not an ordinary test helper. Twenty tests downstream of this one assert against
// the tree it hands them, so a builder that puts a member's key at the wrong leaf, signs a leaf
// at an index it does not occupy, or hands back a private key that is not the one the leaf
// publishes does not FAIL those twenty tests -- it makes all twenty agree with the same wrong
// tree. What that produces is not a red test, it is a green suite and an interop report, and it
// arrives one plan later at a point that has nothing to do with this file.
//
// So the builder checks its own output, on every call, against measures that are not itself:
//
//   - the shape, against the tree math of p3. NodeWidth and IsFullLeafCount decide what a
//     complete tree of this leaf width is, FullLeafCount decides how wide a tree this many
//     members belongs in, and TestTheTestTreeIsATreeTheTreeMathAgreesWith holds every leaf's
//     direct path, copath and root against the same arithmetic. None of the three reads
//     anything this file wrote. The width against the MEMBERSHIP is the half a review had to
//     add, and it is the sharpest thing in this list: a node array that is internally
//     self-consistent is self-consistent at EVERY power of two, so a builder handing back a
//     tree one doubling too wide -- every extra leaf blank, every member still at its own leaf
//     -- satisfied both other shape rules, moved the root, changed every direct path and every
//     future tree hash, and no test in this package said a word.
//   - the leaves, against section 7.3 validation. LeafNode.Validate is task 7's, written
//     before this file and tested against the RFC's own rules, so a leaf this builder produces
//     that no validator would accept is caught at the builder rather than in task 15's tree
//     operations or task 23's whole-tree validation.
//   - the private halves, by USING them. A member whose SignaturePriv is not the key its leaf
//     publishes signs an update path nobody can verify in task 18, and one whose EncryptionPriv
//     is not its leaf's key decrypts nothing in task 22 -- and neither is visible to any
//     assertion about the tree's shape, because both keys are opaque bytes that round trip and
//     compare fine while being the wrong ones. The only thing that separates them is a
//     signature made with one half and checked with the other, and a seal to one half opened
//     with the other, which is what testTreeFaults does.
//
// The faults are values rather than t.Errorf calls at the point they are found, because a
// checker nobody has watched fail is a checker nobody has evidence about.
// TestTheTestTreeCheckerFlagsEveryFaultItNames drives each clause with a tree broken exactly
// that way and requires that clause's own sentinel back, and it derives the class of clauses
// from testTreeFaults' own body so a clause added without a control row -- or deleted -- fails
// there rather than quietly widening or narrowing what the builder promises.
package mls

import (
	"errors"
	"fmt"
	"go/ast"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// one member of a test tree, with the private halves a real client would hold.
type testTreeMember struct {
	LeafIndex      LeafIndex
	SignaturePriv  SignaturePrivateKey
	EncryptionPriv HpkePrivateKey
}

const testGroupIdString = "urmessage-test-group"

func testGroupId() []byte {
	return []byte(testGroupIdString)
}

// newTestTree answers an n-member tree whose parent nodes are all blank, at the narrowest
// complete width n members fit in.
//
// That is the SHAPE a group has immediately after every member has been added and nobody has
// committed a path. It is not that group's LEAVES, and the difference is written down here
// because the description is what a later task reads. Every leaf below carries
// LeafNodeSourceUpdate; a leaf that entered by Add carries key_package, and in this package the
// two are not interchangeable -- leaf_node.go's signature preimage splits key_package from
// update and commit, and key_package is the source that does NOT bind group_id and leaf_index
// into the signature.
//
// Update is the deliberate choice, because the builder's own sign-at-index check is only worth
// making under a source that binds the index: a key_package leaf verifies at every index, so a
// builder that signed member 3's leaf at index 5 would be invisible to it. What that costs is
// that this fixture never holds a key_package-sourced leaf, so task 23's whole tree validation
// and task 18's signing are exercised here only against the source that carries group context.
// A task that needs the other source needs a fixture of its own and must not read this one as
// covering it.
//
// It takes testing.TB rather than *testing.T so the task 28 benchmarks can build trees without
// faking a *testing.T, which is also why the well-formedness check reports through t.Errorf on
// the TB rather than through anything only a test has.
func newTestTree(t testing.TB, crypto CryptoProvider, n uint32) (*RatchetTree, []*testTreeMember) {
	t.Helper()
	tree := NewRatchetTree()
	members := make([]*testTreeMember, 0, n)
	for i := uint32(0); i < n; i += 1 {
		signaturePriv, signaturePub, err := crypto.SignatureKeyPair()
		if err != nil {
			t.Fatalf("SignatureKeyPair(%d): %v", i, err)
		}
		encryptionPriv, encryptionPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair(%d): %v", i, err)
		}
		// the X-Wing public key is opaque filler here and is never used as a key in this
		// package; what matters is that it is XwingPublicKeyLen long, which is what the
		// extension's own encoder refuses anything else for.
		leafKeys := &LeafKeysExtension{
			AlgId:          AlgIdXwing,
			DeviceXwingPub: crypto.Random(XwingPublicKeyLen),
		}
		leafKeysExt, err := leafKeys.Encode()
		if err != nil {
			t.Fatalf("LeafKeysExtension.Encode(%d): %v", i, err)
		}
		leaf := &LeafNode{
			EncryptionKey: encryptionPub,
			SignatureKey:  signaturePub,
			Credential:    BasicCredential([]byte(fmt.Sprintf("member-%d", i))),
			Capabilities: Capabilities{
				Versions:     []ProtocolVersion{ProtocolVersionMls10},
				CipherSuites: []CipherSuite{CipherSuiteX25519ChaCha20Sha256Ed25519},
				Extensions: []ExtensionType{
					ExtensionTypeUrmessageGroupPolicy,
					ExtensionTypeUrmessageLeafKeys,
					ExtensionTypeUrmessageOwnerSuccessor,
				},
				// no proposal types, and empty is the CONFORMING answer rather than a gap in
				// the fixture. Add, update and remove are RFC 9420 section 7.2 "default"
				// types, which section 7.2 forbids a leaf to list; leaf_node_test.go's
				// required capabilities table states the same rule from the other side, that
				// a row requiring ProposalTypeAdd would be asserting a conforming leaf is
				// refused. LeafNode.Validate does not enforce it today, which is exactly why
				// listing them here was free -- and why the twenty tasks reading this fixture
				// would have inherited a non-conforming leaf the day one of them did.
				Proposals:   nil,
				Credentials: []CredentialType{CredentialTypeBasic},
			},
			LeafNodeSource: LeafNodeSourceUpdate,
			Extensions:     []Extension{leafKeysExt},
		}
		if err := leaf.Sign(crypto, signaturePriv, testGroupId(), LeafIndex(i)); err != nil {
			t.Fatalf("Sign(%d): %v", i, err)
		}
		if err := tree.SetLeaf(LeafIndex(i), leaf); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
		members = append(members, &testTreeMember{
			LeafIndex:      LeafIndex(i),
			SignaturePriv:  signaturePriv,
			EncryptionPriv: encryptionPriv,
		})
	}
	assertTestTreeIsWellFormed(t, crypto, tree, members)
	return tree, members
}

// testTreeFaultPrefix is what makes the fault class DERIVABLE: every sentinel below carries it,
// and the control reads the class off testTreeFaults' own body rather than off a list somebody
// maintains beside it.
const testTreeFaultPrefix = "errTestTree"

// Every way a tree this builder produced can be wrong, one sentinel each.
//
// One per clause and never one shared sentinel with a different message: the control drives each
// clause with a tree broken exactly that way and asks errors.Is for that clause's own sentinel,
// which is the only question a deleted clause cannot answer yes to. A shared sentinel would be
// answered yes by whichever OTHER clause the broken fixture also happens to trip, and the
// control would then pass over a checker missing the clause it was written for.
var (
	errTestTreeLeafWidthNotFull          = errors.New("the leaf width is not a power of two")
	errTestTreeNodeWidthNotDerived       = errors.New("the node array is not the node width of its own leaf width")
	errTestTreeWidthNotTheMembershipsOwn = errors.New("the leaf width is not the narrowest complete tree this membership fits in")
	errTestTreeMemberNotAtItsOwnLeaf     = errors.New("member i does not carry leaf index i")
	errTestTreeMemberLeafBlank           = errors.New("a member's leaf is blank")
	errTestTreeStrayLeaf                 = errors.New("a leaf outside the membership is occupied")
	errTestTreeParentNotBlank            = errors.New("a parent node of a fresh test tree is not blank")
	errTestTreeLeafSignature             = errors.New("a member's leaf signature does not verify at its own index")
	errTestTreeLeafInvalid               = errors.New("a member's leaf does not pass section 7.3 validation")
	errTestTreeSignatureKeyPairMismatch  = errors.New("a member's signature private key is not the one its leaf publishes")
	errTestTreeEncryptionKeyPairMismatch = errors.New("a member's encryption private key is not the one its leaf publishes")
)

// testTreeExpectedWidth is the leaf width a tree holding exactly this membership must have.
//
// DERIVED from the container and the arithmetic rather than restated: FullLeafCount is this
// package's own "narrowest complete tree that holds n leaves", and NewRatchetTree's width is
// this package's own floor -- the one leaf tree every group starts as, which is what a tree
// with no members still is, and which FullLeafCount answers zero for on its own. Neither rule
// is rewritten here, so a change to either moves this check with it rather than leaving it
// agreeing with a shape the container no longer builds.
func testTreeExpectedWidth(members []*testTreeMember) LeafCount {
	width := FullLeafCount(LeafCount(len(members)))
	if floor := NewRatchetTree().LeafWidth(); width < floor {
		width = floor
	}
	return width
}

// testTreeFaults is every way this tree and this membership disagree with what newTestTree
// promises, as a list rather than as reports, so the control below can drive it and read back
// what it found.
func testTreeFaults(crypto CryptoProvider, tree *RatchetTree, members []*testTreeMember) []error {
	faults := []error{}
	width := tree.LeafWidth()
	if !IsFullLeafCount(width) {
		faults = append(faults, fmt.Errorf("%w: leaf width %d", errTestTreeLeafWidthNotFull, width))
	}
	if NodeWidth(width) != tree.NodeWidth() {
		faults = append(faults, fmt.Errorf("%w: %d nodes over %d leaves, want %d",
			errTestTreeNodeWidthNotDerived, tree.NodeWidth(), width, NodeWidth(width)))
	}
	// the width against the MEMBERSHIP, which is the question neither clause above asks. Both of
	// them are answered yes by a complete tree of ANY power-of-two width, so the tree this
	// builder returns could be one doubling too wide -- every extra leaf blank, every member
	// still at its own leaf -- and satisfy both. That tree is not a cosmetic difference: RFC 9420
	// section 7.7 keeps a group at the narrowest complete width its members fit in, a doubling
	// adds a new root ABOVE the old one, and moving the root changes every direct path, every
	// copath, every parent hash and every tree hash that twenty later tasks compute against this
	// fixture. Section 12.4.3.3 additionally refuses an exported tree that ends in a blank, which
	// such a tree always does.
	if expected := testTreeExpectedWidth(members); width != expected {
		faults = append(faults, fmt.Errorf("%w: %d leaves over %d members, want %d",
			errTestTreeWidthNotTheMembershipsOwn, width, len(members), expected))
	}

	// the membership this builder promises: member i at leaf i, and nobody anywhere else. The
	// occupied set is derived from the members rather than from the leaf count, so a builder
	// that filled one leaf too many is a stray rather than a member nobody looked for.
	member := map[LeafIndex]bool{}
	for i, one := range members {
		if one.LeafIndex != LeafIndex(i) {
			faults = append(faults, fmt.Errorf("%w: members[%d] carries leaf index %d",
				errTestTreeMemberNotAtItsOwnLeaf, i, one.LeafIndex))
		}
		member[one.LeafIndex] = true
	}
	for x := uint32(0); x < tree.NodeWidth(); x += 1 {
		node := NodeIndex(x)
		if !node.IsLeaf() {
			// asked as IsBlank and not as ParentAt(node) != nil, which is one member of the
			// class rather than the class. This sentinel's own message is about a parent
			// position that is not BLANK, and blank is the ABSENCE of a node; ParentAt answers
			// only for a parent-TYPED one, so a leaf-typed *Node stored at an odd index has a
			// nil Parent, ParentAt says nil and the clause says nothing -- while the tree's own
			// IsBlank says false and Resolution therefore EMITS that node into the list a path
			// secret is sealed to. IsBlank is the tree's own definition of the property, read in
			// the same container the resolution walk reads it from, so the clause asks the tree
			// instead of restating one of the ways a position can be occupied.
			if !tree.IsBlank(node) {
				faults = append(faults, fmt.Errorf("%w: node %d", errTestTreeParentNotBlank, x))
			}
			continue
		}
		leafIndex, ok := leafIndexOf(node)
		if !ok || member[leafIndex] {
			continue
		}
		if tree.Leaf(leafIndex) != nil {
			faults = append(faults, fmt.Errorf("%w: leaf %d", errTestTreeStrayLeaf, leafIndex))
		}
	}

	for _, one := range members {
		leaf := tree.Leaf(one.LeafIndex)
		if leaf == nil {
			faults = append(faults, fmt.Errorf("%w: leaf %d", errTestTreeMemberLeafBlank, one.LeafIndex))
			continue
		}
		if err := leaf.VerifySignature(crypto, testGroupId(), one.LeafIndex); err != nil {
			faults = append(faults, fmt.Errorf("%w: leaf %d: %v", errTestTreeLeafSignature, one.LeafIndex, err))
		}
		// task 7's validator and not a restatement of it. The rules this fixture has to meet
		// are section 7.3's, they are already written down and already tested, and a second
		// reading of them here would be a second thing to keep in step.
		if err := leaf.Validate(&LeafValidationContext{
			Crypto:         crypto,
			Suite:          crypto.Suite(),
			GroupId:        testGroupId(),
			LeafIndex:      one.LeafIndex,
			ExpectedSource: LeafNodeSourceUpdate,
		}); err != nil {
			faults = append(faults, fmt.Errorf("%w: leaf %d: %v", errTestTreeLeafInvalid, one.LeafIndex, err))
		}
		if err := testTreeSignatureKeyPairAgrees(crypto, leaf, one); err != nil {
			faults = append(faults, fmt.Errorf("%w: leaf %d: %v",
				errTestTreeSignatureKeyPairMismatch, one.LeafIndex, err))
		}
		if err := testTreeEncryptionKeyPairAgrees(crypto, leaf, one); err != nil {
			faults = append(faults, fmt.Errorf("%w: leaf %d: %v",
				errTestTreeEncryptionKeyPairMismatch, one.LeafIndex, err))
		}
	}
	return faults
}

// testTreeSignatureKeyPairAgrees signs a copy of the member's own leaf with the private half the
// member is holding and verifies it with the public half the leaf publishes.
//
// USING the pair rather than comparing bytes. There is no derivation from a signature private
// key to its public key on CryptoProvider, and even if there were, a mismatch between a stored
// private key and a published public key is invisible to every assertion about either one on its
// own: both are opaque, both round trip, both are the right length. The signature the builder
// already made does not answer this question either -- it was made with whatever key the builder
// used, so it verifies whether or not the key handed BACK to the caller is that one.
func testTreeSignatureKeyPairAgrees(crypto CryptoProvider, leaf *LeafNode, member *testTreeMember) error {
	probe := leaf.Clone()
	if err := probe.Sign(crypto, member.SignaturePriv, testGroupId(), member.LeafIndex); err != nil {
		return err
	}
	return probe.VerifySignature(crypto, testGroupId(), member.LeafIndex)
}

// testTreeEncryptionKeyPairAgrees seals to the public half the leaf publishes and opens with the
// private half the member is holding, which is exactly what task 22 does with these two values
// and exactly what nothing else here would notice going wrong.
func testTreeEncryptionKeyPairAgrees(crypto CryptoProvider, leaf *LeafNode, member *testTreeMember) error {
	plaintext := []byte("urmessage test tree key pair probe")
	kemOutput, ciphertext, err := crypto.HpkeSeal(leaf.EncryptionKey, nil, nil, plaintext)
	if err != nil {
		return err
	}
	opened, err := crypto.HpkeOpen(member.EncryptionPriv, kemOutput, nil, nil, ciphertext)
	if err != nil {
		return err
	}
	if string(opened) != string(plaintext) {
		return fmt.Errorf("the seal opened to %q", opened)
	}
	return nil
}

// assertTestTreeIsWellFormed reports every fault of a tree this builder produced.
func assertTestTreeIsWellFormed(t testing.TB, crypto CryptoProvider, tree *RatchetTree, members []*testTreeMember) {
	t.Helper()
	for _, fault := range testTreeFaults(crypto, tree, members) {
		t.Errorf("the test tree is malformed: %v", fault)
	}
}

// ---------------------------------------------------------------------------
// the tests
// ---------------------------------------------------------------------------

func TestNewTestTreeShape(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{1, 2, 3, 5, 8, 9} {
		tree, members := newTestTree(t, crypto, n)
		if tree.MemberCount() != n {
			t.Fatalf("n=%d member count = %d", n, tree.MemberCount())
		}
		if uint32(len(members)) != n {
			t.Fatalf("n=%d members = %d", n, len(members))
		}
		if width := testTreeExpectedWidth(members); tree.LeafWidth() != width {
			t.Fatalf("n=%d the tree is %d leaves wide and %d members belong in %d", n, tree.LeafWidth(), n, width)
		}
		for x := uint32(1); x < tree.NodeWidth(); x += 2 {
			// !IsBlank and not ParentAt != nil, for the reason testTreeFaults' own clause gives
			if !tree.IsBlank(NodeIndex(x)) {
				t.Fatalf("n=%d parent %d is not blank in a fresh test tree", n, x)
			}
		}
		for _, member := range members {
			leaf := tree.Leaf(member.LeafIndex)
			if leaf == nil {
				t.Fatalf("n=%d leaf %d is blank", n, member.LeafIndex)
			}
			if err := leaf.VerifySignature(crypto, testGroupId(), member.LeafIndex); err != nil {
				t.Fatalf("n=%d leaf %d signature: %v", n, member.LeafIndex, err)
			}
		}
		// two members must never share a key. A builder that drew one key pair and installed it
		// at every leaf satisfies every assertion above -- every leaf occupied, every signature
		// verifying, every private half matching -- and makes every later task's "member 2
		// cannot read what member 3 was sent" test pass for the wrong reason.
		signatureKeys := map[string]LeafIndex{}
		encryptionKeys := map[string]LeafIndex{}
		for _, member := range members {
			leaf := tree.Leaf(member.LeafIndex)
			if already, seen := signatureKeys[string(leaf.SignatureKey)]; seen {
				t.Fatalf("n=%d leaves %d and %d publish the same signature key", n, already, member.LeafIndex)
			}
			if already, seen := encryptionKeys[string(leaf.EncryptionKey)]; seen {
				t.Fatalf("n=%d leaves %d and %d publish the same encryption key", n, already, member.LeafIndex)
			}
			signatureKeys[string(leaf.SignatureKey)] = member.LeafIndex
			encryptionKeys[string(leaf.EncryptionKey)] = member.LeafIndex
		}
	}
}

// TestTheTestTreeIsATreeTheTreeMathAgreesWith holds the shape this builder produces against p3's
// arithmetic, which is the independent measure available to this task: the tree hash is task
// 12's and does not exist yet, and no tree of the vendored corpus can be decoded until task 11
// supplies the ratchet_tree codec.
//
// Every leaf of every width, and the widths are the ones where the answer changes: the one leaf
// tree, the exact powers of two, and the counts just past one, which are the trees carrying
// blank leaves on the right.
func TestTheTestTreeIsATreeTheTreeMathAgreesWith(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	for _, n := range []uint32{1, 2, 3, 4, 5, 7, 8, 9} {
		tree, _ := newTestTree(t, crypto, n)
		width := tree.LeafWidth()
		root, err := rootOf(width)
		if err != nil {
			t.Fatalf("n=%d rootOf: %v", n, err)
		}
		if uint32(root) >= tree.NodeWidth() {
			t.Fatalf("n=%d the root %d is outside a node array of %d", n, root, tree.NodeWidth())
		}
		for i := uint32(0); i < uint32(width); i += 1 {
			leaf := LeafIndex(i).NodeIndex()
			path, err := directPathOf(leaf, width)
			if err != nil {
				t.Fatalf("n=%d directPath(%d): %v", n, i, err)
			}
			// the one leaf tree is the case that has to be stated rather than folded in: its
			// single leaf IS its root, so its direct path is empty and a rule reading "the last
			// element is the root" would refuse the only tree every group starts as
			if leaf == root {
				if len(path) != 0 {
					t.Fatalf("n=%d leaf %d is the root and its direct path is %v", n, i, path)
				}
			} else if len(path) == 0 || path[len(path)-1] != root {
				t.Fatalf("n=%d the direct path of leaf %d is %v and does not end at the root %d", n, i, path, root)
			}
			// each step is the parent of the one below it and contains the one below it, so the
			// path is a walk up one tree rather than a list of indices that happens to end in
			// the right place
			below := leaf
			for _, step := range path {
				parent, err := Parent(below, width)
				if err != nil {
					t.Fatalf("n=%d Parent(%d): %v", n, below, err)
				}
				if step != parent {
					t.Fatalf("n=%d the direct path of leaf %d steps from %d to %d, and the parent of %d is %d",
						n, i, below, step, below, parent)
				}
				if !InSubtree(step, below) {
					t.Fatalf("n=%d node %d is not inside the subtree of %d", n, below, step)
				}
				if uint32(step) >= tree.NodeWidth() {
					t.Fatalf("n=%d the direct path of leaf %d leaves the node array at %d", n, i, step)
				}
				below = step
			}
			copath, err := Copath(leaf, width)
			if err != nil {
				t.Fatalf("n=%d Copath(%d): %v", n, i, err)
			}
			if len(copath) != len(path) {
				t.Fatalf("n=%d leaf %d has a direct path of %d and a copath of %d", n, i, len(path), len(copath))
			}
			onPath := map[NodeIndex]bool{leaf: true}
			for _, step := range path {
				onPath[step] = true
			}
			for _, step := range copath {
				if onPath[step] {
					t.Fatalf("n=%d node %d is on both the direct path and the copath of leaf %d", n, step, i)
				}
				if uint32(step) >= tree.NodeWidth() {
					t.Fatalf("n=%d the copath of leaf %d leaves the node array at %d", n, i, step)
				}
			}
		}
	}
}

// testTreeFaultRow is one clause of testTreeFaults, the sentinel it raises, and a tree broken
// exactly that way.
type testTreeFaultRow struct {
	// sentinel is the fault this row's break must produce, asked with errors.Is. A break is
	// allowed to trip other clauses too -- several of these faults are genuinely entangled --
	// but only the clause this row is about can answer yes to this question, which is what makes
	// the row a control on that clause rather than on the checker in general.
	sentinel error
	// text is the sentinel's own message, held against the declaration below, so a row cannot be
	// bound to one sentinel while naming another.
	text string
	// breaks answers a tree and a membership that violate this row's clause.
	breaks func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember)
}

func testTreeFaultRows() map[string]testTreeFaultRow {
	return map[string]testTreeFaultRow{
		"errTestTreeLeafWidthNotFull": {
			sentinel: errTestTreeLeafWidthNotFull,
			text:     "the leaf width is not a power of two",
			// five nodes is a leaf width of three, which is not a power of two and IS
			// NodeWidth(3), so this fixture separates the two shape clauses
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				return &RatchetTree{nodes: make([]*Node, 5)}, nil
			},
		},
		"errTestTreeNodeWidthNotDerived": {
			sentinel: errTestTreeNodeWidthNotDerived,
			text:     "the node array is not the node width of its own leaf width",
			// four nodes is a leaf width of two, which IS a power of two and is not four nodes
			// wide, so this fixture separates them the other way
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				return &RatchetTree{nodes: make([]*Node, 4)}, nil
			},
		},
		"errTestTreeWidthNotTheMembershipsOwn": {
			sentinel: errTestTreeWidthNotTheMembershipsOwn,
			text:     "the leaf width is not the narrowest complete tree this membership fits in",
			// one doubling too wide and nothing else touched. Every member is still at its own
			// leaf holding its own keys, every added leaf is blank and every added parent is
			// blank, so this fixture separates the width clause from every other clause of the
			// checker: it is the only one with anything to say about it.
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 4)
				if err := tree.growTo(tree.LeafWidth() * 2); err != nil {
					t.Fatalf("growTo: %v", err)
				}
				return tree, members
			},
		},
		"errTestTreeMemberNotAtItsOwnLeaf": {
			sentinel: errTestTreeMemberNotAtItsOwnLeaf,
			text:     "member i does not carry leaf index i",
			// the membership reversed and nothing else touched: every leaf is still occupied by
			// the member holding its own keys, so no other clause fires
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 2)
				return tree, []*testTreeMember{members[1], members[0]}
			},
		},
		"errTestTreeMemberLeafBlank": {
			sentinel: errTestTreeMemberLeafBlank,
			text:     "a member's leaf is blank",
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 4)
				if err := tree.Blank(members[1].LeafIndex.NodeIndex()); err != nil {
					t.Fatalf("Blank: %v", err)
				}
				return tree, members
			},
		},
		"errTestTreeStrayLeaf": {
			sentinel: errTestTreeStrayLeaf,
			text:     "a leaf outside the membership is occupied",
			// a three member tree is four leaves wide, so leaf 3 is a position the tree has and
			// the membership does not
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 3)
				if err := tree.SetLeaf(LeafIndex(3), tree.Leaf(LeafIndex(0))); err != nil {
					t.Fatalf("SetLeaf: %v", err)
				}
				return tree, members
			},
		},
		"errTestTreeParentNotBlank": {
			sentinel: errTestTreeParentNotBlank,
			text:     "a parent node of a fresh test tree is not blank",
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 2)
				if err := tree.SetParent(NodeIndex(1), &ParentNode{
					EncryptionKey: HpkePublicKey(repeatByte(0x33, 32)),
				}); err != nil {
					t.Fatalf("SetParent: %v", err)
				}
				return tree, members
			},
		},
		"errTestTreeLeafSignature": {
			sentinel: errTestTreeLeafSignature,
			text:     "a member's leaf signature does not verify at its own index",
			// signed with the right key over the wrong group id, so the leaf's own key pair is
			// untouched and only its binding to this group is wrong
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 2)
				leaf := tree.Leaf(LeafIndex(1)).Clone()
				if err := leaf.Sign(crypto, members[1].SignaturePriv,
					[]byte("some other group"), LeafIndex(1)); err != nil {
					t.Fatalf("Sign: %v", err)
				}
				if err := tree.SetLeaf(LeafIndex(1), leaf); err != nil {
					t.Fatalf("SetLeaf: %v", err)
				}
				return tree, members
			},
		},
		"errTestTreeLeafInvalid": {
			sentinel: errTestTreeLeafInvalid,
			text:     "a member's leaf does not pass section 7.3 validation",
			// a leaf carrying urmessage_leaf_keys and no longer claiming to support it,
			// re-signed so the signature is correct and only section 7.3 refuses it
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 2)
				leaf := tree.Leaf(LeafIndex(1)).Clone()
				leaf.Capabilities.Extensions = slices.DeleteFunc(leaf.Capabilities.Extensions,
					func(e ExtensionType) bool { return e == ExtensionTypeUrmessageLeafKeys })
				if err := leaf.Sign(crypto, members[1].SignaturePriv, testGroupId(), LeafIndex(1)); err != nil {
					t.Fatalf("Sign: %v", err)
				}
				if err := tree.SetLeaf(LeafIndex(1), leaf); err != nil {
					t.Fatalf("SetLeaf: %v", err)
				}
				return tree, members
			},
		},
		"errTestTreeSignatureKeyPairMismatch": {
			sentinel: errTestTreeSignatureKeyPairMismatch,
			text:     "a member's signature private key is not the one its leaf publishes",
			// the leaf and its stored signature are untouched, so nothing but the private half
			// handed back to the caller is wrong -- which is the whole of what this clause is
			// for
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 2)
				members[1].SignaturePriv = members[0].SignaturePriv
				return tree, members
			},
		},
		"errTestTreeEncryptionKeyPairMismatch": {
			sentinel: errTestTreeEncryptionKeyPairMismatch,
			text:     "a member's encryption private key is not the one its leaf publishes",
			breaks: func(t *testing.T, crypto CryptoProvider) (*RatchetTree, []*testTreeMember) {
				tree, members := newTestTree(t, crypto, 2)
				members[1].EncryptionPriv = members[0].EncryptionPriv
				return tree, members
			},
		},
	}
}

// testTreeFaultNamesInSource is every fault sentinel testTreeFaults can raise, read off its own
// body.
//
// Derived and not listed, for guardrail 5's reason: a written list of a class understates it the
// moment a clause lands beside the ones it was written against, and the control table then
// reports full coverage of a checker it covers less of than it did before. A clause added
// without a row, a row for a clause that was deleted, and a row aimed at a sentinel the checker
// never raises all fail in TestTheTestTreeCheckerFlagsEveryFaultItNames.
func testTreeFaultNamesInSource(t *testing.T) []string {
	t.Helper()
	parsed := theSourceDeclaring(t, "", "testTreeFaults")
	found := map[string]bool{}
	ast.Inspect(parsed.declarationOf(t, "", "testTreeFaults"), func(node ast.Node) bool {
		ident, isIdent := node.(*ast.Ident)
		if isIdent && strings.HasPrefix(ident.Name, testTreeFaultPrefix) {
			found[ident.Name] = true
		}
		return true
	})
	if len(found) == 0 {
		t.Fatalf("testTreeFaults raises no %s sentinel at all, so the table below controls nothing",
			testTreeFaultPrefix)
	}
	return slices.Sorted(maps.Keys(found))
}

// testTreeFaultTextsInSource is the message every fault sentinel of this package was declared
// with, read off the declaration.
//
// This is what binds a control row to the sentinel it NAMES rather than to whichever sentinel
// somebody typed into it. Without it a row keyed "errTestTreeStrayLeaf" holding
// errTestTreeMemberLeafBlank still satisfies a bijection over the key set, and the clause the row
// was supposed to control has no row at all.
func testTreeFaultTextsInSource(t *testing.T) map[string]string {
	t.Helper()
	texts := map[string]string{}
	for _, path := range packageSourcePaths(t) {
		parsed := mustParseSource(t, path)
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			spec, isSpec := node.(*ast.ValueSpec)
			if !isSpec {
				return true
			}
			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, testTreeFaultPrefix) || i >= len(spec.Values) {
					continue
				}
				call, isCall := spec.Values[i].(*ast.CallExpr)
				if !isCall || len(call.Args) != 1 {
					t.Fatalf("%s is not declared as a call taking one argument, so its message cannot be read",
						name.Name)
				}
				literal, isLiteral := call.Args[0].(*ast.BasicLit)
				if !isLiteral {
					t.Fatalf("%s is not declared over a string literal, so its message cannot be read", name.Name)
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote the message of %s: %v", name.Name, err)
				}
				texts[name.Name] = text
			}
			return true
		})
	}
	return texts
}

// TestTheTestTreeCheckerFlagsEveryFaultItNames is the control on the builder's own check.
//
// A checker nobody has watched fail is decoration, and a fixture builder whose check is
// decoration is worse than one with no check at all, because the twenty tests downstream read
// its silence as evidence. So every clause is driven with a tree broken exactly that way, and
// the table of breaks is held to the class of clauses the checker's source declares -- in both
// directions, and by message rather than by name alone.
func TestTheTestTreeCheckerFlagsEveryFaultItNames(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	rows := testTreeFaultRows()
	declared := testTreeFaultNamesInSource(t)
	controlled := slices.Sorted(maps.Keys(rows))
	if !slices.Equal(declared, controlled) {
		t.Fatalf("testTreeFaults raises %v and this table controls %v", declared, controlled)
	}
	texts := testTreeFaultTextsInSource(t)
	for _, name := range declared {
		row := rows[name]
		if row.sentinel == nil {
			t.Fatalf("the row for %s carries no sentinel", name)
		}
		if row.sentinel.Error() != texts[name] {
			t.Fatalf("the row keyed %s carries the sentinel whose message is %q, and %s was declared as %q",
				name, row.sentinel.Error(), name, texts[name])
		}
		if row.text != texts[name] {
			t.Fatalf("the row keyed %s states the message %q and %s was declared as %q",
				name, row.text, name, texts[name])
		}
	}

	// the positive half first: a tree straight out of the builder has no fault at all, or every
	// refusal below is just everything failing
	for _, n := range []uint32{1, 2, 3, 4, 5, 8} {
		tree, members := newTestTree(t, crypto, n)
		if faults := testTreeFaults(crypto, tree, members); len(faults) != 0 {
			t.Fatalf("n=%d: the builder's own tree reports %v", n, faults)
		}
	}

	for _, name := range declared {
		row := rows[name]
		t.Run(name, func(t *testing.T) {
			tree, members := row.breaks(t, crypto)
			faults := testTreeFaults(crypto, tree, members)
			if len(faults) == 0 {
				t.Fatalf("the tree broken for %s reports no fault at all", name)
			}
			for _, fault := range faults {
				if errors.Is(fault, row.sentinel) {
					return
				}
			}
			t.Fatalf("the tree broken for %s reports %v and none of them is %v", name, faults, row.sentinel)
		})
	}
}

// testTreeNodeKinds is one *Node of every kind this package's Node union can hold, keyed by the
// NodeType constant it carries, each ready to be dropped at a parent position.
//
// A table rather than two cases written out, so the control below can be held to the class the
// package DECLARES: a third NodeType landing beside these two fails
// TestTheParentClauseFlagsAParentPositionWhateverKindOfNodeOccupiesIt rather than quietly
// leaving the checker with a kind of occupied node it does not name.
func testTreeNodeKinds() map[string]func(tree *RatchetTree) *Node {
	return map[string]func(tree *RatchetTree) *Node{
		"NodeTypeLeaf": func(tree *RatchetTree) *Node {
			return &Node{NodeType: NodeTypeLeaf, Leaf: tree.Leaf(LeafIndex(0))}
		},
		"NodeTypeParent": func(tree *RatchetTree) *Node {
			return &Node{NodeType: NodeTypeParent, Parent: &ParentNode{
				EncryptionKey: HpkePublicKey(repeatByte(0x33, 32)),
			}}
		},
	}
}

// testTreeNodeTypeNamesInSource is every NodeType constant this package declares, read off the
// declaration rather than listed, for the reason testTreeFaultNamesInSource gives.
func testTreeNodeTypeNamesInSource(t *testing.T) []string {
	t.Helper()
	found := []string{}
	for _, path := range packageSourcePaths(t) {
		parsed := mustParseSource(t, path)
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			spec, isSpec := node.(*ast.ValueSpec)
			if !isSpec {
				return true
			}
			ident, isIdent := spec.Type.(*ast.Ident)
			if !isIdent || ident.Name != "NodeType" {
				return true
			}
			for _, name := range spec.Names {
				found = append(found, name.Name)
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatalf("this package declares no NodeType constant, so the table above controls nothing")
	}
	slices.Sort(found)
	return found
}

// TestTheParentClauseFlagsAParentPositionWhateverKindOfNodeOccupiesIt is the control on the one
// clause of this checker that is about a POSITION rather than about a node.
//
// errTestTreeParentNotBlank says "a parent node of a fresh test tree is not blank", and blank is
// the absence of a node -- so every kind of node the union can hold, at an odd index, violates
// it. The clause was written as ParentAt(node) != nil, which answers nil for a leaf-typed node
// stored at a parent index and reported nothing, while the tree's own IsBlank said false and
// Resolution emitted that node; the row below for NodeTypeLeaf is that exact tree, and it is
// what the clause is now asked with. The kinds are held against the NodeType constants the
// package declares, so the class cannot narrow without failing here.
func TestTheParentClauseFlagsAParentPositionWhateverKindOfNodeOccupiesIt(t *testing.T) {
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	kinds := testTreeNodeKinds()
	declared := testTreeNodeTypeNamesInSource(t)
	controlled := slices.Sorted(maps.Keys(kinds))
	if !slices.Equal(declared, controlled) {
		t.Fatalf("this package declares the node types %v and this table occupies a parent position with %v",
			declared, controlled)
	}
	for _, name := range declared {
		t.Run(name, func(t *testing.T) {
			tree, members := newTestTree(t, crypto, 4)
			// written into the node array and not through SetLeaf or SetParent, because both
			// of those refuse a node whose type does not match its position -- and a fixture
			// the container refuses proves nothing about the checker. What this is about is a
			// tree that ALREADY holds such a node, which is what task 11's ratchet_tree
			// decoder can hand this package from a peer's bytes.
			tree.nodes[1] = kinds[name](tree)
			if tree.IsBlank(NodeIndex(1)) {
				t.Fatalf("node 1 holding a %s still reports blank, so this row breaks nothing", name)
			}
			// and the consequence, stated where it can be seen rather than asserted in a
			// comment: the occupied parent is emitted into the resolution of its own subtree,
			// so a fixture carrying one seals every path secret at that node to a different
			// set of nodes than the tree the membership describes.
			if got := tree.Resolution(NodeIndex(1)); len(got) == 0 || got[0] != NodeIndex(1) {
				t.Fatalf("node 1 holding a %s resolves to %v, so the fault is not observable here", name, got)
			}
			faults := testTreeFaults(crypto, tree, members)
			for _, fault := range faults {
				if errors.Is(fault, errTestTreeParentNotBlank) {
					return
				}
			}
			t.Fatalf("a parent position holding a %s reports %v and none of them is %v",
				name, faults, errTestTreeParentNotBlank)
		})
	}
}
