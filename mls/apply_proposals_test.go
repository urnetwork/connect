package mls

import (
	"bytes"
	"errors"
	"testing"
)

func testApplyContext() *GroupContext {
	return &GroupContext{Version: ProtocolVersionMls10,
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519, GroupId: []byte("group"), Epoch: 1}
}

// TestApplyProposalsAppliesTheRfcOrderAndNotTheListOrder is the one case the whole of section
// 12.3's order is visible from, and it is built so that the two orders disagree.
//
// The commit order is add(dave), remove(bob), add(erin) over a three leaf tree. RFC 9420 section
// 12.3 applies removes before adds, so bob's leaf 1 is blank when the first add is placed: dave
// lands at 1 and erin at 3. An implementation that walked the list in the order it arrived places
// dave at 3 -- the first blank in a three leaf tree of width four -- blanks leaf 1, and puts erin
// there: the same two members, on different leaves, under a different tree hash, which is a fork
// and not an error.
//
// A fixture whose list happened to be sorted remove-then-add passes under BOTH orders, which is
// what most fixtures look like and is why this one is written backwards.
func TestApplyProposalsAppliesTheRfcOrderAndNotTheListOrder(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	dave := testIdentity(t, crypto, "dave")
	erin := testIdentity(t, crypto, "erin")
	kpDave, _, _ := testKeyPackage(t, crypto, dave)
	kpErin, _, _ := testKeyPackage(t, crypto, erin)

	list := testProposalList(t, testAddOf(kpDave), testRemoveOf(1), testAddOf(kpErin))
	result, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0), list)
	if err != nil {
		t.Fatalf("ApplyProposals: %v", err)
	}
	if len(result.AddedLeaves) != 2 {
		t.Fatalf("AddedLeaves = %v, want two entries", result.AddedLeaves)
	}
	if result.AddedLeaves[0] != 1 || result.AddedLeaves[1] != 3 {
		t.Fatalf("AddedLeaves = %v, want [1 3]: removes are applied before adds, so the first add takes the leaf the remove blanked",
			result.AddedLeaves)
	}
	placed := result.Tree.Leaf(result.AddedLeaves[0])
	if placed == nil || !bytes.Equal(placed.SignatureKey, dave.SigPub) {
		t.Fatal("the FIRST add of the commit order did not land in the leftmost blank leaf")
	}
	second := result.Tree.Leaf(result.AddedLeaves[1])
	if second == nil || !bytes.Equal(second.SignatureKey, erin.SigPub) {
		t.Fatal("the second add of the commit order did not land at the leaf after it")
	}
}

// TestApplyProposalsPlacesAddsInCommitOrderRatherThanBucketOrder is the second half of the add
// rule, one level down: which of two adds goes first.
//
// Two commits carrying the same set of adds in a different order build different trees, so the
// order has to come from the wire's own vector. The two lists here differ only in that order and
// nothing else, and each member must land where its own commit put it.
func TestApplyProposalsPlacesAddsInCommitOrderRatherThanBucketOrder(t *testing.T) {
	crypto := testCrypto(t)
	dave := testIdentity(t, crypto, "dave")
	erin := testIdentity(t, crypto, "erin")
	kpDave, _, _ := testKeyPackage(t, crypto, dave)
	kpErin, _, _ := testKeyPackage(t, crypto, erin)

	for name, row := range map[string]struct {
		list  *ProposalList
		first []byte
	}{
		"dave first": {testProposalList(t, testAddOf(kpDave), testAddOf(kpErin)), dave.SigPub},
		"erin first": {testProposalList(t, testAddOf(kpErin), testAddOf(kpDave)), erin.SigPub},
	} {
		tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
		result, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0), row.list)
		if err != nil {
			t.Fatalf("%s: ApplyProposals: %v", name, err)
		}
		if len(result.AddedLeaves) != 2 {
			t.Fatalf("%s: AddedLeaves = %v, want two entries", name, result.AddedLeaves)
		}
		landed := result.Tree.Leaf(result.AddedLeaves[0])
		if landed == nil || !bytes.Equal(landed.SignatureKey, row.first) {
			t.Errorf("%s: the leaf the first add took is not the first add's member", name)
		}
	}
}

// TestApplyProposalsTakesTheAddOrderFromTheCommitOrder holds section 12.3's one ordered clause to
// the one order a list has.
//
// IT USED TO PUT TWO FIELDS IN CONFLICT and it cannot any more, which is the change rather than a
// weakening. A ProposalList once carried its adds in All and in an Adds field a caller wrote
// separately, so this test built All saying dave-then-erin beside a bucket saying
// erin-then-dave and required the placement to follow All. The views are now the commit order
// filtered, so that fixture does not exist: (*ProposalList).Adds answers dave-then-erin because
// the order does, and the conflict is unrepresentable rather than caught.
// TestTheCountPreservingBypassesOfTheBucketJoinCannotBeBuilt is where that is asserted.
//
// What is left here is still worth driving, because it is section 12.3's own clause: adds are
// placed "in the order they appear in the proposals vector", and an implementation that reversed
// the walk, or that placed them in some order of its own, puts two members on each other's
// leaves. The two adds are told apart by their signature keys, so a placement in the other order
// fails rather than reading the same.
func TestApplyProposalsTakesTheAddOrderFromTheCommitOrder(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	dave := testIdentity(t, crypto, "dave")
	erin := testIdentity(t, crypto, "erin")
	kpDave, _, _ := testKeyPackage(t, crypto, dave)
	kpErin, _, _ := testKeyPackage(t, crypto, erin)
	list := NewProposalList([]CachedProposal{testAddOf(kpDave), testAddOf(kpErin)})
	// the derived view is the same sequence, which is what makes the walk below a statement about
	// section 12.3 rather than a statement about which of two fields was read
	adds := list.Adds()
	if len(adds) != 2 || !bytes.Equal(adds[0].Proposal.Add.KeyPackage.LeafNode.SignatureKey, dave.SigPub) {
		t.Fatalf("the adds view answers %d entries and does not open with dave; it is not the commit order filtered",
			len(adds))
	}
	result, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0), list)
	if err != nil {
		t.Fatalf("ApplyProposals: %v", err)
	}
	if len(result.AddedLeaves) != 2 {
		t.Fatalf("AddedLeaves = %v, want two entries", result.AddedLeaves)
	}
	landed := result.Tree.Leaf(result.AddedLeaves[0])
	if landed == nil || !bytes.Equal(landed.SignatureKey, dave.SigPub) {
		t.Fatal("the first leaf taken is not the first add of the COMMIT order")
	}
}

// TestApplyProposalsDoesNotMutateTheInputTree is what makes a rejected commit safe.
//
// Section 12.4.2 validates the update path, the tree and the confirmation tag AGAINST the tree
// this call produces, so applying in place would leave a member's live state half-way through a
// commit it went on to reject, with no way back to the epoch it was in.
func TestApplyProposalsDoesNotMutateTheInputTree(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")
	before, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	width := tree.LeafWidth()
	leaf, _ := testLeafNode(t, crypto, members[1])
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "dave"))
	// one of each kind, so the claim covers every operation this file performs rather than the
	// one a remove happens to touch
	list := testProposalList(t, testUpdateOf(1, leaf), testRemoveOf(2), testAddOf(kp))
	result, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0), list)
	if err != nil {
		t.Fatalf("ApplyProposals: %v", err)
	}
	after, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("ApplyProposals mutated the caller's tree; a rejected commit would corrupt live state")
	}
	if tree.LeafWidth() != width {
		t.Fatalf("the caller's tree is now %d leaves wide, was %d", tree.LeafWidth(), width)
	}
	// and the result is a DIFFERENT tree rather than the same one: a version that answered the
	// caller's own pointer would satisfy the hash comparison above by doing nothing at all
	applied, err := result.Tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash of the applied tree: %v", err)
	}
	if bytes.Equal(before, applied) {
		t.Fatal("the applied tree hashes to the pre-commit tree, so the proposals were applied to nothing")
	}
}

// TestApplyProposalsBlanksTheDirectPathOfAnUpdatedAndOfARemovedLeaf holds the two per-proposal
// application rules of sections 12.1.2 and 12.1.3.
//
// Both are about the nodes ABOVE the leaf and neither is visible from the leaf itself: an update
// that replaced the leaf and left the path standing lets the member who has just updated away go
// on reading the group, which is the whole of what an Update is for.
func TestApplyProposalsBlanksTheDirectPathOfAnUpdatedAndOfARemovedLeaf(t *testing.T) {
	crypto := testCrypto(t)
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol", "dave")
	// a parent node standing above leaf 1, so there is something for the update to blank
	if err := tree.SetParent(NodeIndex(1), &ParentNode{
		EncryptionKey: HpkePublicKey(crypto.Random(32)), ParentHash: []byte{}}); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if tree.IsBlank(NodeIndex(1)) {
		t.Fatal("the fixture parent was not installed, so this test observes nothing")
	}
	leaf, _ := testLeafNode(t, crypto, members[1])
	result, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0),
		testProposalList(t, testUpdateOf(1, leaf)))
	if err != nil {
		t.Fatalf("ApplyProposals: %v", err)
	}
	if !result.Tree.IsBlank(NodeIndex(1)) {
		t.Error("an applied update left the node above the updated leaf standing")
	}
	if got := result.Tree.Leaf(1); got == nil || !bytes.Equal(got.EncryptionKey, leaf.EncryptionKey) {
		t.Error("an applied update did not install the proposal's own leaf")
	}
	if got := result.UpdatedLeaves; len(got) != 1 || got[0] != 1 {
		t.Errorf("UpdatedLeaves = %v, want [1]", got)
	}

	removed, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0),
		testProposalList(t, testRemoveOf(1)))
	if err != nil {
		t.Fatalf("ApplyProposals: %v", err)
	}
	if removed.Tree.Leaf(1) != nil {
		t.Error("an applied remove left the removed leaf occupied")
	}
	if !removed.Tree.IsBlank(NodeIndex(1)) {
		t.Error("an applied remove left the node above the removed leaf standing")
	}
	if got := removed.RemovedLeaves; len(got) != 1 || got[0] != 1 {
		t.Errorf("RemovedLeaves = %v, want [1]", got)
	}
}

// TestApplyProposalsGceReplacesWholesaleAndBeforeEverythingElse is section 12.3's first step.
//
// Wholesale rather than merged, because a merge would make an extension impossible to remove: the
// pre-commit context carries an extension the proposal does not, and it must be gone. The context
// the caller handed in keeps its own vector, for the reason the tree is cloned.
func TestApplyProposalsGceReplacesWholesaleAndBeforeEverythingElse(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	replacement := testRequiredCapabilitiesExtension(t)
	ctx := testApplyContext()
	ctx.Extensions = []Extension{{ExtensionType: ExtensionType(0x00FF), ExtensionData: []byte{1}}}

	result, err := ApplyProposals(tree, ctx, LeafIndex(0),
		testProposalList(t, testGceOf(replacement)))
	if err != nil {
		t.Fatalf("ApplyProposals: %v", err)
	}
	if len(result.Extensions) != 1 || result.Extensions[0].ExtensionType != ExtensionTypeRequiredCapabilities {
		t.Fatalf("a GroupContextExtensions proposal must replace the vector wholesale, got %+v", result.Extensions)
	}
	if len(ctx.Extensions) != 1 || ctx.Extensions[0].ExtensionType != ExtensionType(0x00FF) {
		t.Fatalf("the caller's own extension vector was rewritten: %+v", ctx.Extensions)
	}
	// and with no proposal the group's own extensions are carried through unchanged
	unchanged, err := ApplyProposals(tree, ctx, LeafIndex(0), &ProposalList{})
	if err != nil {
		t.Fatalf("ApplyProposals with an empty list: %v", err)
	}
	if len(unchanged.Extensions) != 1 || unchanged.Extensions[0].ExtensionType != ExtensionType(0x00FF) {
		t.Fatalf("an empty list changed the group's extensions: %+v", unchanged.Extensions)
	}
}

// TestApplyProposalsAnswersAnExtensionVectorNothingElseWritesThrough is the extension half of the
// "a candidate the caller did not own" claim.
//
// The tree half is held next door by a hash comparison. This half needs a write, because both
// vectors have the length they were built with: a result sharing the caller's backing array
// hashes, prints and compares identically to one that does not, and the difference only appears
// when somebody assigns through it. Task 22 builds the next epoch's GroupContext out of
// result.Extensions, so the somebody is real.
//
// Both sources are covered: the group's own vector when no proposal replaces it, and the
// proposal's vector when one does.
func TestApplyProposalsAnswersAnExtensionVectorNothingElseWritesThrough(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	carried := Extension{ExtensionType: ExtensionType(0x00FF), ExtensionData: []byte{1}}
	replacement := Extension{ExtensionType: ExtensionType(0x00FE), ExtensionData: []byte{2}}
	proposal := testGceOf(replacement)

	for name, row := range map[string]struct {
		list  *ProposalList
		owner func(ctx *GroupContext) []Extension
	}{
		"the group's own vector": {&ProposalList{},
			func(ctx *GroupContext) []Extension { return ctx.Extensions }},
		"the proposal's vector": {testProposalList(t, proposal),
			func(ctx *GroupContext) []Extension {
				return proposal.Proposal.GroupContextExtensions.Extensions
			}},
	} {
		ctx := testApplyContext()
		ctx.Extensions = []Extension{carried}
		result, err := ApplyProposals(tree, ctx, LeafIndex(0), row.list)
		if err != nil {
			t.Fatalf("%s: ApplyProposals: %v", name, err)
		}
		source := row.owner(ctx)
		if len(result.Extensions) != 1 || len(source) != 1 {
			t.Fatalf("%s: the fixture holds %d applied and %d source entries, want one of each",
				name, len(result.Extensions), len(source))
		}
		before := source[0].ExtensionType
		result.Extensions[0] = Extension{ExtensionType: ExtensionType(0x0BAD)}
		if source[0].ExtensionType != before {
			t.Errorf("%s: writing through the applied vector rewrote it, so the two share storage",
				name)
		}
	}
}

// TestApplyProposalsReportsSelfRemovalAndOnlyForOurOwnLeaf is the flag the group state machine
// reads to know it has been evicted.
//
// Both halves, because a version that answered true for every remove would satisfy the positive
// case exactly as the correct one does.
func TestApplyProposalsReportsSelfRemovalAndOnlyForOurOwnLeaf(t *testing.T) {
	crypto := testCrypto(t)
	for name, row := range map[string]struct {
		own  LeafIndex
		want bool
	}{
		"our own leaf":    {LeafIndex(1), true},
		"somebody else's": {LeafIndex(0), false},
	} {
		tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
		result, err := ApplyProposals(tree, testApplyContext(), row.own,
			testProposalList(t, testRemoveOf(1)))
		if err != nil {
			t.Fatalf("%s: ApplyProposals: %v", name, err)
		}
		if result.SelfRemoved != row.want {
			t.Errorf("%s: SelfRemoved = %v, want %v", name, result.SelfRemoved, row.want)
		}
	}
}

// TestApplyProposalsRefusesAListItWouldOtherwiseDereference holds the structural precondition at
// the application door.
//
// A caller applies before it validates -- section 12.4.2 validates the tree this produces -- so a
// proposal whose named arm is not the one populated, or one of a type this build does not
// implement, reaches here first, and each is a nil dereference rather than a wrong answer.
//
// TWO OF THE THREE ROWS THIS TABLE CARRIED ARE GONE WITH THE FIELDS THEY DESCRIBED. "A remove in
// the adds bucket" and "an add the commit order does not carry" were shapes a caller could build
// while ProposalList carried its views as writable fields; the views are now the commit order
// filtered, so neither can be constructed and neither has a refusal left to assert. The row that
// stays is the one about the ARM, which is a fault of the proposal rather than of the list, and it
// is joined by a type this profile refuses -- the other half of what ValSem113 answers at this
// door.
func TestApplyProposalsRefusesAListItWouldOtherwiseDereference(t *testing.T) {
	crypto := testCrypto(t)
	armless := CachedProposal{Proposal: Proposal{ProposalType: ProposalTypeRemove}}
	psk := CachedProposal{Proposal: Proposal{
		ProposalType: ProposalTypePreSharedKey, PreSharedKey: &PreSharedKey{}}}
	for name, row := range map[string]struct {
		list     *ProposalList
		sentinel error
	}{
		"a remove with no remove arm": {
			NewProposalList([]CachedProposal{armless}),
			ErrContentArmMismatch},
		"a proposal of a type this profile does not implement": {
			NewProposalList([]CachedProposal{psk}),
			errProfilePsk},
	} {
		tree, _ := testTreeWith(t, crypto, "alice", "bob")
		_, err := applyProposalsRefusalOf(t, name, tree, testApplyContext(), LeafIndex(0), row.list)
		if !errors.Is(err, row.sentinel) {
			t.Errorf("%s: ApplyProposals answered %v, want %v", name, err, row.sentinel)
		}
	}
}

// TestApplyProposalsRefusesEveryMissingArgumentRatherThanDereferencingIt is the nil rule at this
// file's one door, for every pointer it reads.
func TestApplyProposalsRefusesEveryMissingArgumentRatherThanDereferencingIt(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice")
	ctx := testApplyContext()
	list := &ProposalList{}
	for name, row := range map[string]struct {
		tree     *RatchetTree
		ctx      *GroupContext
		list     *ProposalList
		sentinel error
	}{
		"no tree":          {nil, ctx, list, errNilRatchetTree},
		"no group context": {tree, nil, list, ErrNilGroupContext},
		"no list":          {tree, ctx, nil, errNilProposalList},
	} {
		_, err := applyProposalsRefusalOf(t, name, row.tree, row.ctx, LeafIndex(0), row.list)
		if !errors.Is(err, row.sentinel) {
			t.Errorf("%s: ApplyProposals answered %v, want %v", name, err, row.sentinel)
		}
	}
}

// applyProposalsRefusalOf turns a panic into the error it should have been, so one row
// dereferencing its argument is a failure of that row rather than the end of the sweep.
func applyProposalsRefusalOf(t *testing.T, what string, tree *RatchetTree,
	ctx *GroupContext, own LeafIndex, list *ProposalList) (result *ApplyResult, answered error) {
	t.Helper()
	defer func() {
		if panicked := recover(); panicked != nil {
			t.Errorf("%s panicked with %v; a panic out of a library takes the caller's process rather than its call",
				what, panicked)
			result, answered = nil, nil
		}
	}()
	return ApplyProposals(tree, ctx, own, list)
}

// TestApplyProposalsRefusesAListCarryingTwoGroupContextExtensions holds the source this door
// decides the next epoch's whole extension set off to a rule that says which entry it is.
//
// (*ProposalList).Extensions answers GCE[0] and says so, and what makes that index EXACT is that
// (*ProposalCache).Resolve refuses the second one as it buckets. That says nothing about a list a
// caller assembled field by field, and this door takes one: over such a list step 1 installed one
// of two extension sets with nothing at all saying which, while both validators refused it
// outright. It is the same fault the three structural rules above close one field lower down -- a
// step decided off a source the door has not established -- so the rule that already states it is
// asked here rather than restated.
//
// THE SECOND PROPOSAL IS THE OFFENDER and it sits behind an agreeing one, so a rule that answered
// element zero cannot refuse this list at all.
func TestApplyProposalsRefusesAListCarryingTwoGroupContextExtensions(t *testing.T) {
	crypto := testCrypto(t)
	tree, _ := testTreeWith(t, crypto, "alice", "bob")
	first := testGceOf(Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy,
		ExtensionData: []byte{0x01}})
	second := testGceOf(Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy,
		ExtensionData: []byte{0x02}})
	_, failure := ApplyProposals(tree, testApplyContext(), LeafIndex(0),
		testProposalList(t, first, second))
	if !errors.Is(failure, errMultipleGroupContextExtensions) {
		t.Fatalf("ApplyProposals over a list carrying two group_context_extensions proposals = %v, want errMultipleGroupContextExtensions; the applied extension set is otherwise one of the two and nothing says which",
			failure)
	}
	// the control: one of them alone is applied, so the refusal above is about the second and not
	// about a GroupContextExtensions proposal this door cannot handle
	applied, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0),
		testProposalList(t, first))
	if err != nil {
		t.Fatalf("ApplyProposals over a list carrying one: %v", err)
	}
	if len(applied.Extensions) != 1 || !bytes.Equal(applied.Extensions[0].ExtensionData, []byte{0x01}) {
		t.Fatalf("the applied extension set is %v, want the one the single proposal installs",
			applied.Extensions)
	}
}
