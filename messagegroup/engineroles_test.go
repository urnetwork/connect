// Ledger item 242's R1 at the seam: what a receiving client's authorization decision is handed
// out of Process, the three by-value arms it has to be tested against, the discard door for a
// commit it refuses, and the P4 repair.
//
// MASTER section 11 has a bad commit "rejected by every receiving client on validation", and the
// validation runs BETWEEN Process and ApplyCommit. Every case here is therefore stated over the
// EngineProcessed a receiver holds at that moment, and anchored -- where it can be -- to what the
// same receiver holds AFTER ApplyCommit: a value that said one thing before the merge and another
// after it is a value no authorizer can act on.
//
// Nothing here decides who may do what. That predicate is the sdk's, on both arms; this file is
// the inputs, the doors, and the promise that each answers what its name says.
package messagegroup

import (
	"bytes"
	"errors"
	"go/ast"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// contextExtensionsOf is the PRE-commit extension list a caller can read through the seam with
// public mls API and nothing else: GroupContextBytes, syntax.Unmarshal into mls.GroupContext,
// and the list. It is the one route this package documents for a receiver's "before", so the
// cases below use it rather than reaching the adapter's own helper.
func contextExtensionsOf(t *testing.T, handle GroupHandle) []ExtensionBytes {
	t.Helper()
	contextBytes, err := handle.GroupContextBytes()
	if err != nil {
		t.Fatalf("GroupContextBytes: %v", err)
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		t.Fatalf("a caller cannot decode GroupContextBytes with syntax.Unmarshal into mls.GroupContext: %v", err)
	}
	return extensionBytesOf(context.Extensions)
}

// policyOf decodes the 0xF001 entry of a seam-typed list through public mls API.
func policyOf(t *testing.T, extensions []ExtensionBytes) *mls.GroupPolicyExtension {
	t.Helper()
	policy, err := mls.GroupPolicyOf(mlsExtensionsOf(extensions))
	if err != nil {
		t.Fatalf("mls.GroupPolicyOf over the seam's list: %v", err)
	}
	return policy
}

// entryOf is the one entry of a list carrying the type, or a fatal for none or two.
func entryOf(t *testing.T, extensions []ExtensionBytes, extensionType uint16) ExtensionBytes {
	t.Helper()
	entry, found, err := mls.FindExtensionEntry(mlsExtensionsOf(extensions), mls.ExtensionType(extensionType))
	if err != nil {
		t.Fatalf("FindExtensionEntry(%#04x): %v", extensionType, err)
	}
	if !found {
		t.Fatalf("the list carries no %#04x entry", extensionType)
	}
	return ExtensionBytes{Type: uint16(entry.ExtensionType), Data: entry.ExtensionData}
}

// assertSameExtensions holds two lists entry for entry, type and body.
func assertSameExtensions(t *testing.T, what string, got []ExtensionBytes, want []ExtensionBytes) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d entries, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i].Type != want[i].Type || !bytes.Equal(got[i].Data, want[i].Data) {
			t.Fatalf("%s: entry %d is type %#04x with %d octets, want type %#04x with %d octets",
				what, i, got[i].Type, len(got[i].Data), want[i].Type, len(want[i].Data))
		}
	}
}

// identityAtLeaf is a member's identity read through MemberAt by LEAF rather than by ordinal.
func identityAtLeaf(t *testing.T, handle GroupHandle, leaf uint32) []byte {
	t.Helper()
	for at := 0; at < handle.MemberCount(); at += 1 {
		memberLeaf, identity, _, err := handle.MemberAt(at)
		if err != nil {
			t.Fatalf("MemberAt(%d): %v", at, err)
		}
		if memberLeaf == leaf {
			return identity
		}
	}
	t.Fatalf("no member of this handle stands at leaf %d", leaf)
	return nil
}

// membersOf is a handle's live membership projected onto the seam's ProcessedMember, which is
// the anchor every case compares MembersAfter against once the commit is applied. MemberAt
// refuses a member whose leaf carries no urmessage_leaf_keys, so every entry it answers is one
// with HasLeafKeys true, and the anchor says so; the one case with a keyless member anchors
// against that refusal instead.
func membersOf(t *testing.T, handle GroupHandle) []ProcessedMember {
	t.Helper()
	out := []ProcessedMember{}
	for at := 0; at < handle.MemberCount(); at += 1 {
		leaf, identity, _, err := handle.MemberAt(at)
		if err != nil {
			t.Fatalf("MemberAt(%d): %v", at, err)
		}
		out = append(out, ProcessedMember{Leaf: leaf, Identity: identity, HasLeafKeys: true})
	}
	return out
}

func assertSameMembers(t *testing.T, what string, got []ProcessedMember, want []ProcessedMember) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d members, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i].Leaf != want[i].Leaf || !bytes.Equal(got[i].Identity, want[i].Identity) || got[i].HasLeafKeys != want[i].HasLeafKeys {
			t.Fatalf("%s: member %d is leaf %d identity %x leaf keys %v, want leaf %d identity %x leaf keys %v",
				what, i, got[i].Leaf, got[i].Identity, got[i].HasLeafKeys, want[i].Leaf, want[i].Identity, want[i].HasLeafKeys)
		}
	}
}

// memberAtLeaf is the MembersAfter entry standing at one leaf, or a fatal for none.
func memberAtLeaf(t *testing.T, what string, members []ProcessedMember, leaf uint32) ProcessedMember {
	t.Helper()
	for _, member := range members {
		if member.Leaf == leaf {
			return member
		}
	}
	t.Fatalf("%s: MembersAfter holds nobody at leaf %d: %v", what, leaf, members)
	return ProcessedMember{}
}

// TestThePreCommitExtensionListIsReadableThroughPublicMlsApi is item 242's R1 step 2, held as a
// property rather than asserted in prose: a caller holding only GroupContextBytes and the mls
// package reaches the extension list, the policy in it and the required_capabilities beside it.
func TestThePreCommitExtensionListIsReadableThroughPublicMlsApi(t *testing.T) {
	chain := newCommitAddChain(t, "roles-readable")
	extensions := contextExtensionsOf(t, chain.joined)
	policy := policyOf(t, extensions)
	owner, held := policy.OwnerId()
	if !held || !bytes.Equal(owner, chain.founder.identityPub) {
		t.Fatalf("the policy decoded out of GroupContextBytes names owner %x, want the founder %x", owner, chain.founder.identityPub)
	}
	if role, named := policy.RoleOf(chain.joiner.identityPub); named || role != mls.RoleMember {
		t.Fatalf("the joiner CommitAdd admitted is %s named=%v in the policy, want member false: an Add names nobody", role, named)
	}
	required := entryOf(t, extensions, uint16(mls.ExtensionTypeRequiredCapabilities))
	if len(required.Data) == 0 {
		t.Fatal("required_capabilities is present and empty")
	}
}

// TestProcessAnswersWhoACommitAdmitsAndWhoCommittedIt is the receiving arm's inputs on an Add:
// after CommitAdd of two key packages, the member that processes it holds the two new identities
// at the AddedLeaves, every pre-commit member at its own leaf, the extension list unchanged entry
// for entry, and the committer's identity -- and all of it agrees with what the same member holds
// after ApplyCommit.
func TestProcessAnswersWhoACommitAdmitsAndWhoCommittedIt(t *testing.T) {
	chain := newCommitAddChain(t, "roles-add-two")
	third, fourth := newTestEngine(t), newTestEngine(t)
	keyPackages := [][]byte{}
	for _, engine := range []*testEngine{third, fourth} {
		keyPackage, err := engine.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("NewKeyPackage: %v", err)
		}
		keyPackages = append(keyPackages, keyPackage)
	}
	before := contextExtensionsOf(t, chain.joined)
	membersBefore := membersOf(t, chain.joined)
	committerIdentity := identityAtLeaf(t, chain.joined, chain.founded.OwnLeafIndex())

	commit, _, _, err := chain.founded.CommitAdd(keyPackages)
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if processed.Kind != EngineProcessedCommit {
		t.Fatalf("Process discriminated kind %d, want a commit", processed.Kind)
	}
	if len(processed.AddedLeaves) != 2 {
		t.Fatalf("AddedLeaves = %v, want two", processed.AddedLeaves)
	}
	// the committer, by identity and not only by leaf
	if processed.CommitterLeaf != chain.founded.OwnLeafIndex() {
		t.Fatalf("CommitterLeaf = %d, want the founder's %d", processed.CommitterLeaf, chain.founded.OwnLeafIndex())
	}
	if !bytes.Equal(processed.CommitterIdentity, committerIdentity) {
		t.Fatalf("CommitterIdentity = %x, want the identity MemberAt answers for the committer's leaf, %x", processed.CommitterIdentity, committerIdentity)
	}
	// the membership after: every pre-commit member where it was, and the two joiners at the
	// leaves the commit put them, identity for identity
	byLeaf := map[uint32][]byte{}
	for _, member := range processed.MembersAfter {
		byLeaf[member.Leaf] = member.Identity
	}
	if len(byLeaf) != len(membersBefore)+2 {
		t.Fatalf("MembersAfter holds %d leaves, want the %d before plus two", len(byLeaf), len(membersBefore))
	}
	for _, member := range membersBefore {
		if !bytes.Equal(byLeaf[member.Leaf], member.Identity) {
			t.Fatalf("MembersAfter holds %x at leaf %d and the pre-commit member there is %x", byLeaf[member.Leaf], member.Leaf, member.Identity)
		}
	}
	wantAdded := map[string]bool{string(third.identityPub): true, string(fourth.identityPub): true}
	for _, leaf := range processed.AddedLeaves {
		identity, held := byLeaf[leaf]
		if !held {
			t.Fatalf("AddedLeaves names leaf %d and MembersAfter holds nobody there", leaf)
		}
		if !wantAdded[string(identity)] {
			t.Fatalf("MembersAfter holds %x at added leaf %d, which is neither joiner's identity", identity, leaf)
		}
		delete(wantAdded, string(identity))
	}
	if len(wantAdded) != 0 {
		t.Fatalf("%d of the two admitted identities are not at any added leaf", len(wantAdded))
	}
	// in leaf order, which is the order an authorizer will zip against the pre-commit list
	for i := 1; i < len(processed.MembersAfter); i += 1 {
		if processed.MembersAfter[i-1].Leaf >= processed.MembersAfter[i].Leaf {
			t.Fatalf("MembersAfter is not in leaf order at %d: %v then %v", i, processed.MembersAfter[i-1].Leaf, processed.MembersAfter[i].Leaf)
		}
	}
	// the extension list: a commit carrying no GroupContextExtensions proposal installs the list
	// the group already had, entry for entry, with both 0x0003 and 0xF001 in it
	assertSameExtensions(t, "ContextExtensionsAfter across an add-only commit", processed.ContextExtensionsAfter, before)
	entryOf(t, processed.ContextExtensionsAfter, uint16(mls.ExtensionTypeRequiredCapabilities))
	entryOf(t, processed.ContextExtensionsAfter, uint16(mls.ExtensionTypeUrmessageGroupPolicy))
	// nothing here is a window: writing through the answer changes nothing a second reading sees
	for i := range processed.CommitterIdentity {
		processed.CommitterIdentity[i] ^= 0xff
	}
	if !bytes.Equal(identityAtLeaf(t, chain.joined, chain.founded.OwnLeafIndex()), committerIdentity) {
		t.Fatal("writing through CommitterIdentity rewrote the live tree's leaf")
	}

	// THE ANCHOR
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	assertSameMembers(t, "MembersAfter against the membership after ApplyCommit", processed.MembersAfter, membersOf(t, chain.joined))
	assertSameExtensions(t, "ContextExtensionsAfter against the context after ApplyCommit", processed.ContextExtensionsAfter, contextExtensionsOf(t, chain.joined))
}

// TestCommitPolicyReplacesThePolicyAndNothingElse is item 242's P4 inverted at the by-value arm:
// after CommitPolicy naming the joiner ADMIN, the receiver's ContextExtensionsAfter decodes to
// that policy at 0xF001 and carries 0x0003 byte-identical to before; and the commit is processed
// COLD, by a member with nothing cached.
func TestCommitPolicyReplacesThePolicyAndNothingElse(t *testing.T) {
	chain := newCommitAddChain(t, "roles-commit-policy")
	before := contextExtensionsOf(t, chain.joined)
	if _, named := policyOf(t, before).RoleOf(chain.joiner.identityPub); named {
		t.Fatal("the joiner is already named, so the change below observes nothing")
	}
	policy := policyOf(t, before)
	policy.SetRole(chain.joiner.identityPub, mls.RoleAdmin)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode the new policy: %v", err)
	}

	commit, welcome, _, err := chain.founded.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("CommitPolicy: %v", err)
	}
	if welcome != nil {
		t.Fatalf("CommitPolicy answered a %d octet welcome for a commit that adds nobody", len(welcome))
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("a member with nothing cached refused the by-value policy commit: %v", err)
	}
	if len(processed.AddedLeaves)+len(processed.RemovedLeaves) != 0 {
		t.Fatalf("a policy commit added %v and removed %v", processed.AddedLeaves, processed.RemovedLeaves)
	}
	after := processed.ContextExtensionsAfter
	if role, named := policyOf(t, after).RoleOf(chain.joiner.identityPub); !named || role != mls.RoleAdmin {
		t.Fatalf("the post-commit policy answers %s named=%v for the joiner, want admin true", role, named)
	}
	if !bytes.Equal(entryOf(t, after, uint16(mls.ExtensionTypeUrmessageGroupPolicy)).Data, encoded.ExtensionData) {
		t.Fatal("the post-commit 0xF001 body is not the one CommitPolicy was handed")
	}
	if !bytes.Equal(entryOf(t, after, uint16(mls.ExtensionTypeRequiredCapabilities)).Data,
		entryOf(t, before, uint16(mls.ExtensionTypeRequiredCapabilities)).Data) {
		t.Fatal("required_capabilities changed across a policy commit; the wholesale replacement stripped or rewrote it")
	}
	if len(after) != len(before) {
		t.Fatalf("the list has %d entries after the policy commit and %d before", len(after), len(before))
	}
	// and the members are unchanged, identity for identity, including the committer's own
	// leaf across its path
	assertSameMembers(t, "MembersAfter across a policy commit", processed.MembersAfter, membersOf(t, chain.joined))

	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	assertSameExtensions(t, "ContextExtensionsAfter against the context after ApplyCommit", after, contextExtensionsOf(t, chain.joined))
	assertSameEpochSecret(t, 2, map[string]GroupHandle{"the committer": chain.founded, "the receiver": chain.joined})
}

// TestCommitContextExtensionsCarriesExactlyTheListItIsHanded is the wholesale arm: the list a
// caller passes is the list every receiver installs, no more and no less, and an empty list is
// refused by name before anything is staged.
func TestCommitContextExtensionsCarriesExactlyTheListItIsHanded(t *testing.T) {
	chain := newCommitAddChain(t, "roles-commit-gce")
	if _, _, _, err := chain.founded.CommitContextExtensions(nil); !errors.Is(err, ErrEngineCommitContextExtensionsEmpty) {
		t.Fatalf("CommitContextExtensions(nil) answered %v, want ErrEngineCommitContextExtensionsEmpty", err)
	}
	before := contextExtensionsOf(t, chain.joined)
	policy := policyOf(t, before)
	policy.RetentionPolicy.MediaMs += 1
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// the caller's list, in the caller's order, and the caller's buffer written over afterwards
	handed := []ExtensionBytes{}
	for _, entry := range before {
		if entry.Type == uint16(mls.ExtensionTypeUrmessageGroupPolicy) {
			entry = ExtensionBytes{Type: entry.Type, Data: append([]byte(nil), encoded.ExtensionData...)}
		}
		handed = append(handed, entry)
	}
	want := extensionBytesOf(mlsExtensionsOf(handed))
	commit, _, _, err := chain.founded.CommitContextExtensions(handed)
	if err != nil {
		t.Fatalf("CommitContextExtensions after a refusal: %v; the refusal staged something", err)
	}
	for _, entry := range handed {
		for i := range entry.Data {
			entry.Data[i] ^= 0xff
		}
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	assertSameExtensions(t, "ContextExtensionsAfter against the list the caller handed in", processed.ContextExtensionsAfter, want)
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	assertSameExtensions(t, "the receiver's context after ApplyCommit", contextExtensionsOf(t, chain.joined), want)
	assertSameExtensions(t, "the committer's context after MergePendingCommit", contextExtensionsOf(t, chain.founded), want)
}

// TestProposeGroupPolicyKeepsRequiredCapabilities is item 242's P4 inverted at the by-reference
// arm: after a policy proposal is committed, the context still carries 0x0003 byte-identical to
// before, beside the new policy. Before the repair this body handed mls a one-entry list and the
// wholesale replacement dropped everything else.
func TestProposeGroupPolicyKeepsRequiredCapabilities(t *testing.T) {
	chain := newCommitAddChain(t, "roles-propose-policy")
	before := contextExtensionsOf(t, chain.joined)
	policy := policyOf(t, before)
	policy.SetRole(chain.joiner.identityPub, mls.RoleObserver)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	proposal, err := chain.founded.ProposeGroupPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("ProposeGroupPolicy: %v", err)
	}
	if _, err := chain.joined.Process(proposal); err != nil {
		t.Fatalf("the receiver's Process over the proposal: %v", err)
	}
	commit, _, _, err := chain.founded.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) over the cached proposal: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process over the commit: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	for name, handle := range map[string]GroupHandle{"the committer": chain.founded, "the receiver": chain.joined} {
		after := contextExtensionsOf(t, handle)
		if len(after) != len(before) {
			t.Fatalf("%s's context has %d entries after the policy proposal was committed and %d before; the proposal replaced the list with fewer entries", name, len(after), len(before))
		}
		if !bytes.Equal(entryOf(t, after, uint16(mls.ExtensionTypeRequiredCapabilities)).Data,
			entryOf(t, before, uint16(mls.ExtensionTypeRequiredCapabilities)).Data) {
			t.Fatalf("%s's required_capabilities changed across a policy proposal", name)
		}
		if role, named := policyOf(t, after).RoleOf(chain.joiner.identityPub); !named || role != mls.RoleObserver {
			t.Fatalf("%s's policy answers %s named=%v for the joiner, want observer true", name, role, named)
		}
	}
	// and the one helper both doors share answers the same list, so a policy proposed and one
	// committed by value cannot leave different entries standing
	if !bytes.Equal(entryOf(t, processed.ContextExtensionsAfter, uint16(mls.ExtensionTypeUrmessageGroupPolicy)).Data, encoded.ExtensionData) {
		t.Fatal("the staged list's 0xF001 is not the body that was proposed")
	}
}

// TestANonFounderCommitterIsNamedByItsOwnIdentityAtBothReceivers is CommitterIdentity's own case,
// and its committer is deliberately NOT the founder. Every other commit in this package is the
// founder's, at leaf 0 with the owner's identity, so a CommitterIdentity that always answered the
// first member's identity -- or leaf 0's -- would have passed every one of them. Here the joiner
// at leaf 1 commits, twice: a policy commit the founder processes, and a Remove of leaf 2 that
// both the survivor and the removed member process, so the identity is read at a live receiver
// and off the report a removed member is handed.
//
// What is held at each receiver is the pair the sdk's authorizer compares: CommitterIdentity is
// the joiner's and not the founder's, and MembersAfter at CommitterLeaf carries the SAME identity,
// which is MASTER section 11's continuity rule holding over an honest commit -- and the seam's
// doc is explicit that mls holds that rule for nobody, so a receiving arm that skips the
// comparison has no other line that makes it. The dishonest side of the same pair, where the
// two reads differ, cannot be built through this seam and is pinned in mls, in
// TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt, over the
// same two reads this adapter makes.
func TestANonFounderCommitterIsNamedByItsOwnIdentityAtBothReceivers(t *testing.T) {
	chain := newCommitAddChain(t, "roles-non-founder-committer")
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	commit, welcome, ratchetTree, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	thirdHandle, err := third.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the third member's join: %v", err)
	}
	defer thirdHandle.Close()
	if chain.joined.OwnLeafIndex() != 1 || thirdHandle.OwnLeafIndex() != 2 {
		t.Fatalf("the joiner is at leaf %d and the third member at %d, want 1 and 2", chain.joined.OwnLeafIndex(), thirdHandle.OwnLeafIndex())
	}
	if bytes.Equal(chain.joiner.identityPub, chain.founder.identityPub) {
		t.Fatal("the joiner and the founder share an identity, so a committer named by the wrong one is invisible here")
	}
	// the pair the authorizer compares, at one receiver over one processed commit
	assertCommitterIsTheJoiner := func(t *testing.T, who string, processed *EngineProcessed) {
		t.Helper()
		if processed.CommitterLeaf != chain.joined.OwnLeafIndex() {
			t.Fatalf("%s: CommitterLeaf = %d, want the joiner's %d", who, processed.CommitterLeaf, chain.joined.OwnLeafIndex())
		}
		if !bytes.Equal(processed.CommitterIdentity, chain.joiner.identityPub) {
			t.Fatalf("%s: CommitterIdentity = %x, want the joiner's %x; a read that answers the founder's, or leaf 0's, names the wrong member as the one whose authority is judged", who, processed.CommitterIdentity, chain.joiner.identityPub)
		}
		atCommitterLeaf := []byte(nil)
		for _, member := range processed.MembersAfter {
			if member.Leaf == processed.CommitterLeaf {
				atCommitterLeaf = member.Identity
			}
		}
		if atCommitterLeaf == nil {
			t.Fatalf("%s: MembersAfter holds nobody at the committer's leaf %d", who, processed.CommitterLeaf)
		}
		if !bytes.Equal(atCommitterLeaf, processed.CommitterIdentity) {
			t.Fatalf("%s: MembersAfter at the committer's leaf carries %x and CommitterIdentity is %x; over an honest commit the continuity rule's two sides agree", who, atCommitterLeaf, processed.CommitterIdentity)
		}
	}

	// a policy commit by the joiner, processed by the founder
	policy := policyOf(t, contextExtensionsOf(t, chain.joined))
	policy.SetRole(chain.joiner.identityPub, mls.RoleAdmin)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	policyCommit, _, _, err := chain.joined.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("the joiner's CommitPolicy: %v", err)
	}
	atFounder, err := chain.founded.Process(policyCommit)
	if err != nil {
		t.Fatalf("the founder's Process of the joiner's policy commit: %v", err)
	}
	assertCommitterIsTheJoiner(t, "the founder over the policy commit", atFounder)
	atThird, err := thirdHandle.Process(policyCommit)
	if err != nil {
		t.Fatalf("the third member's Process of the joiner's policy commit: %v", err)
	}
	assertCommitterIsTheJoiner(t, "the third member over the policy commit", atThird)
	for who, arm := range map[string]struct {
		handle    GroupHandle
		processed *EngineProcessed
	}{"the founder": {chain.founded, atFounder}, "the third member": {thirdHandle, atThird}} {
		if err := arm.handle.ApplyCommit(arm.processed); err != nil {
			t.Fatalf("%s's ApplyCommit: %v", who, err)
		}
	}
	if err := chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("the joiner's MergePendingCommit: %v", err)
	}
	assertSameEpochSecret(t, 3, map[string]GroupHandle{"the founder": chain.founded, "the joiner": chain.joined, "the third member": thirdHandle})

	// a Remove of leaf 2 by the joiner: the founder survives it and the third member is handed
	// the report, and both name the joiner
	removal, _, _, err := chain.joined.CommitRemove([]uint32{2})
	if err != nil {
		t.Fatalf("the joiner's CommitRemove: %v", err)
	}
	survivor, err := chain.founded.Process(removal)
	if err != nil {
		t.Fatalf("the founder's Process of the joiner's remove: %v", err)
	}
	assertCommitterIsTheJoiner(t, "the survivor over the remove", survivor)
	removed, err := thirdHandle.Process(removal)
	if err != nil {
		t.Fatalf("the removed member's Process of the joiner's remove: %v", err)
	}
	assertCommitterIsTheJoiner(t, "the removed member over the remove", removed)
	if err := thirdHandle.ApplyCommit(removed); !errors.Is(err, mls.ErrRemovedFromGroup) {
		t.Fatalf("the removed member's ApplyCommit answered %v, want mls.ErrRemovedFromGroup", err)
	}
	if err := thirdHandle.DiscardProcessed(removed); err != nil {
		t.Fatalf("DiscardProcessed of the report after ErrRemovedFromGroup answered %v, want nil: the report holds no key material", err)
	}
	if err := chain.founded.ApplyCommit(survivor); err != nil {
		t.Fatalf("the survivor's ApplyCommit: %v", err)
	}
	if err := chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("the joiner's MergePendingCommit: %v", err)
	}
	assertSameEpochSecret(t, 4, map[string]GroupHandle{"the founder": chain.founded, "the joiner": chain.joined})
}

// TestCommitRemoveOfLeafTwoInAFourMemberGroup is the Remove arm at every receiver that matters:
// a survivor holds RemovedLeaves == [2] and a MembersAfter without leaf 2, the removed member
// holds the report and is told ErrRemovedFromGroup, and both hold the extension list unchanged.
func TestCommitRemoveOfLeafTwoInAFourMemberGroup(t *testing.T) {
	chain := newCommitAddChain(t, "roles-remove-two")
	if _, _, _, err := chain.founded.CommitRemove(nil); !errors.Is(err, ErrEngineCommitRemoveEmpty) {
		t.Fatalf("CommitRemove(nil) answered %v, want ErrEngineCommitRemoveEmpty", err)
	}
	// two more, so the group holds leaves 0..3
	third, fourth := newTestEngine(t), newTestEngine(t)
	keyPackages := [][]byte{}
	for _, engine := range []*testEngine{third, fourth} {
		keyPackage, err := engine.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("NewKeyPackage: %v", err)
		}
		keyPackages = append(keyPackages, keyPackage)
	}
	commit, welcome, ratchetTree, err := chain.founded.CommitAdd(keyPackages)
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	thirdHandle, err := third.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the third member's join: %v", err)
	}
	defer thirdHandle.Close()
	fourthHandle, err := fourth.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the fourth member's join: %v", err)
	}
	defer fourthHandle.Close()
	if thirdHandle.OwnLeafIndex() != 2 || fourthHandle.OwnLeafIndex() != 3 || chain.joined.MemberCount() != 4 {
		t.Fatalf("the group is not four members at leaves 0..3 (third at %d, fourth at %d, %d members)", thirdHandle.OwnLeafIndex(), fourthHandle.OwnLeafIndex(), chain.joined.MemberCount())
	}
	before := contextExtensionsOf(t, chain.joined)
	membersBefore := membersOf(t, chain.joined)

	removal, welcome, _, err := chain.founded.CommitRemove([]uint32{2})
	if err != nil {
		t.Fatalf("CommitRemove: %v", err)
	}
	if welcome != nil {
		t.Fatal("a remove answered a welcome")
	}
	// a survivor with nothing cached
	survivor, err := chain.joined.Process(removal)
	if err != nil {
		t.Fatalf("the survivor's Process: %v", err)
	}
	if len(survivor.RemovedLeaves) != 1 || survivor.RemovedLeaves[0] != 2 {
		t.Fatalf("RemovedLeaves = %v, want [2]", survivor.RemovedLeaves)
	}
	wantAfter := []ProcessedMember{}
	for _, member := range membersBefore {
		if member.Leaf != 2 {
			wantAfter = append(wantAfter, member)
		}
	}
	assertSameMembers(t, "the survivor's MembersAfter", survivor.MembersAfter, wantAfter)
	assertSameExtensions(t, "the survivor's ContextExtensionsAfter", survivor.ContextExtensionsAfter, before)
	if !bytes.Equal(survivor.CommitterIdentity, chain.founder.identityPub) {
		t.Fatalf("CommitterIdentity = %x, want the founder's %x", survivor.CommitterIdentity, chain.founder.identityPub)
	}
	// the removed member holds the report: the same facts, off a staged value with no context
	removed, err := thirdHandle.Process(removal)
	if err != nil {
		t.Fatalf("the removed member's Process: %v", err)
	}
	if len(removed.RemovedLeaves) != 1 || removed.RemovedLeaves[0] != 2 {
		t.Fatalf("the removed member's RemovedLeaves = %v, want [2]", removed.RemovedLeaves)
	}
	assertSameMembers(t, "the removed member's MembersAfter", removed.MembersAfter, wantAfter)
	assertSameExtensions(t, "the removed member's ContextExtensionsAfter, which the adapter makes the pre-commit list", removed.ContextExtensionsAfter, contextExtensionsOf(t, thirdHandle))
	if err := thirdHandle.ApplyCommit(removed); !errors.Is(err, mls.ErrRemovedFromGroup) {
		t.Fatalf("the removed member's ApplyCommit answered %v, want mls.ErrRemovedFromGroup", err)
	}

	// THE ANCHOR, at the survivor and at the fourth member
	if err := chain.joined.ApplyCommit(survivor); err != nil {
		t.Fatalf("the survivor's ApplyCommit: %v", err)
	}
	fourthProcessed, err := fourthHandle.Process(removal)
	if err != nil {
		t.Fatalf("the fourth member's Process: %v", err)
	}
	if err := fourthHandle.ApplyCommit(fourthProcessed); err != nil {
		t.Fatalf("the fourth member's ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	assertSameMembers(t, "MembersAfter against the survivor's membership after ApplyCommit", survivor.MembersAfter, membersOf(t, chain.joined))
	assertSameEpochSecret(t, 3, map[string]GroupHandle{
		"the committer":     chain.founded,
		"the survivor":      chain.joined,
		"the fourth member": fourthHandle,
	})
}

// TestDiscardProcessedErasesTheStagedEpochAndApplyCommitRefusesIt is the discard door: after
// DiscardProcessed, ApplyCommit of the same value refuses, the handle's epoch is unchanged, and
// the staged epoch's key material is gone -- read through mls's own erased-answers-nil door on the
// value this package put in stagedRef. The three foreign shapes ApplyCommit refuses are refused
// here too, and an application message discards to nil.
func TestDiscardProcessedErasesTheStagedEpochAndApplyCommitRefusesIt(t *testing.T) {
	chain := newCommitAddChain(t, "roles-discard")
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	commit, _, _, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	staged := processed.stagedRef.(*stagedProcessed).processed.Commit
	if staged.EpochAuthenticator() == nil {
		t.Fatal("the staged epoch answers no authenticator before the discard, so the erase below observes nothing")
	}
	epochBefore := chain.joined.Epoch()

	// the foreign shapes, each refused and none erasing anything
	if err := chain.joined.DiscardProcessed(nil); !errors.Is(err, ErrEngineProcessedForeign) {
		t.Fatalf("DiscardProcessed(nil) answered %v, want ErrEngineProcessedForeign", err)
	}
	if err := chain.joined.DiscardProcessed(&EngineProcessed{Kind: EngineProcessedCommit}); !errors.Is(err, ErrEngineProcessedForeign) {
		t.Fatalf("DiscardProcessed over a keyed literal answered %v, want ErrEngineProcessedForeign", err)
	}
	if err := chain.founded.DiscardProcessed(processed); !errors.Is(err, ErrEngineProcessedForeign) {
		t.Fatalf("another handle's DiscardProcessed answered %v, want ErrEngineProcessedForeign", err)
	}
	if staged.EpochAuthenticator() == nil {
		t.Fatal("a refused discard erased the staged epoch")
	}

	if err := chain.joined.DiscardProcessed(processed); err != nil {
		t.Fatalf("DiscardProcessed: %v", err)
	}
	if staged.EpochAuthenticator() != nil {
		t.Fatal("the staged epoch still answers an authenticator after DiscardProcessed; its schedule was not erased")
	}
	err = chain.joined.ApplyCommit(processed)
	if err == nil {
		t.Fatal("ApplyCommit installed a discarded commit")
	}
	if !strings.Contains(err.Error(), "erased") {
		t.Fatalf("ApplyCommit after the discard refused with %v, want mls's erased refusal", err)
	}
	if got := chain.joined.Epoch(); got != epochBefore {
		t.Fatalf("the handle stands at epoch %d after a refused ApplyCommit, want %d", got, epochBefore)
	}
	// a second discard is a no-op, for every erase's reason
	if err := chain.joined.DiscardProcessed(processed); err != nil {
		t.Fatalf("a second DiscardProcessed answered %v", err)
	}
	// and the handle is still a member of the epoch it refused to leave: it exports
	if _, err := chain.joined.Export("URmessage/v1/storage", nil, 32); err != nil {
		t.Fatalf("the handle cannot export after discarding a commit: %v", err)
	}

	// an application message stages no epoch and discards to nil
	chain.founded.ClearPendingCommit()
	frame, err := chain.founded.Protect([]byte("aad"), []byte("hello"))
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	application, err := chain.joined.Process(frame)
	if err != nil {
		t.Fatalf("Process over an application message: %v", err)
	}
	if application.Kind != EngineProcessedApplication {
		t.Fatalf("kind %d, want an application message", application.Kind)
	}
	if err := chain.joined.DiscardProcessed(application); err != nil {
		t.Fatalf("DiscardProcessed over an application message answered %v, want nil", err)
	}
}

// TestDiscardProcessedAfterASuccessfulApplyCommitErasesNothing is the OTHER order of the discard
// door, and it is the order every receiving arm will write: `defer handle.DiscardProcessed(
// processed)` beside an ApplyCommit that then succeeds. The case above holds discard-then-apply;
// this one holds apply-then-discard, and before 2026-09-21's repair it failed at every line after
// the discard -- the discard answered nil and erased the epoch the handle had just ENTERED, so
// Export refused, Protect refused, and the next commit did not decrypt. A member bricked by its
// own cleanup, with no error at the line that did it.
//
// What is held: the discard answers nil, the handle still exports the epoch it entered and
// agrees with the committer on it, it still seals, and the NEXT commit still opens. The last is
// the one an authorizer's caller would notice first and the one that says the secret tree is
// intact and not only the exporter.
func TestDiscardProcessedAfterASuccessfulApplyCommitErasesNothing(t *testing.T) {
	chain := newCommitAddChain(t, "roles-discard-after-apply")
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	commit, _, _, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	// the control: the handle is in the epoch it entered before the discard runs
	assertSameEpochSecret(t, 2, map[string]GroupHandle{"the committer": chain.founded, "the receiver": chain.joined})

	// the cleanup every receiving arm writes
	if err := chain.joined.DiscardProcessed(processed); err != nil {
		t.Fatalf("DiscardProcessed after a successful ApplyCommit answered %v, want nil: the value holds no epoch the handle has not already taken", err)
	}
	if err := chain.joined.DiscardProcessed(processed); err != nil {
		t.Fatalf("a second DiscardProcessed after a successful ApplyCommit answered %v, want nil", err)
	}
	// and the value cannot be installed twice: refused by name, with the epoch unmoved
	if err := chain.joined.ApplyCommit(processed); !errors.Is(err, ErrEngineProcessedApplied) {
		t.Fatalf("a second ApplyCommit of an installed value answered %v, want ErrEngineProcessedApplied", err)
	}
	assertSameEpochSecret(t, 2, map[string]GroupHandle{"the committer": chain.founded, "the receiver": chain.joined})
	if _, err := chain.joined.Protect([]byte("aad"), []byte("still here")); err != nil {
		t.Fatalf("Protect after discarding an applied commit: %v; the discard erased the epoch the handle is running", err)
	}

	// and the NEXT commit opens, which is what says the secret tree survived and not only the
	// exporter: a policy commit, which is the commit the role model's receiving arm exists for
	policy := policyOf(t, contextExtensionsOf(t, chain.founded))
	policy.SetRole(chain.joiner.identityPub, mls.RoleAdmin)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	next, _, _, err := chain.founded.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("CommitPolicy: %v", err)
	}
	nextProcessed, err := chain.joined.Process(next)
	if err != nil {
		t.Fatalf("the receiver's Process of the NEXT commit after the discard: %v; the discard erased the epoch it was sealed under", err)
	}
	if err := chain.joined.ApplyCommit(nextProcessed); err != nil {
		t.Fatalf("ApplyCommit of the next commit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the next commit: %v", err)
	}
	if err := chain.joined.DiscardProcessed(nextProcessed); err != nil {
		t.Fatalf("DiscardProcessed after the next ApplyCommit answered %v, want nil", err)
	}
	assertSameEpochSecret(t, 3, map[string]GroupHandle{"the committer": chain.founded, "the receiver": chain.joined})
}

// TestCommitterIdentityIsReadOffTheLiveGroupAndMembersAfterOffTheStagedValue pins WHERE Process
// reads the two halves of MASTER section 11's continuity pair, because no commit built through
// this seam can tell the two readings apart: an honest committer's leaf carries one identity
// before and after its own path, so a CommitterIdentity read off the STAGED tree passes every
// behavioral case in this file -- and over the swap mls pins in
// TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt it names the
// victim, and the authorizer's comparison of CommitterIdentity against MembersAfter at
// CommitterLeaf compares the swap against itself. The dishonest commit needs the committer's
// tree, which this package cannot reach, so the reading is held off the source instead: the value
// assigned to answer.CommitterIdentity is drawn from a call on self.group, the live *mls.Group
// and the pre-commit tree, and the value assigned to answer.MembersAfter is drawn from a call
// handed processed.Commit, the staged value and the post-commit tree. One local binding between
// the call and the assignment is followed, which is the shape the body has.
func TestCommitterIdentityIsReadOffTheLiveGroupAndMembersAfterOffTheStagedValue(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	var process *ast.FuncDecl
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != "Process" || function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			star, isStar := function.Recv.List[0].Type.(*ast.StarExpr)
			if !isStar {
				continue
			}
			if named, isNamed := star.X.(*ast.Ident); isNamed && named.Name == "connectMlsHandle" {
				process = function
			}
		}
	}
	if process == nil {
		t.Fatal("(*connectMlsHandle).Process is not declared in this package's production sources")
	}
	// every assignment in the body, by the spelling of its left side and of a defined local
	assigned := map[string]ast.Expr{}
	ast.Inspect(process.Body, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign || len(assign.Lhs) != len(assign.Rhs) && len(assign.Rhs) != 1 {
			return true
		}
		for i, left := range assign.Lhs {
			right := assign.Rhs[0]
			if len(assign.Rhs) == len(assign.Lhs) {
				right = assign.Rhs[i]
			}
			assigned[spellingOf(left)] = right
		}
		return true
	})
	// the call an expression is drawn from: itself when it is a call, and otherwise the call the
	// one local it selects off was defined from
	sourceCallOf := func(t *testing.T, what string, expression ast.Expr) *ast.CallExpr {
		t.Helper()
		if call, isCall := expression.(*ast.CallExpr); isCall {
			return call
		}
		root := expression
		if selector, isSelector := expression.(*ast.SelectorExpr); isSelector {
			root = selector.X
		}
		local, isLocal := root.(*ast.Ident)
		if !isLocal {
			t.Fatalf("%s is assigned %s, which is neither a call nor a field of a local this reading follows", what, spellingOf(expression))
		}
		defined, wasDefined := assigned[local.Name]
		if !wasDefined {
			t.Fatalf("%s is assigned off %s, which Process never defines", what, local.Name)
		}
		call, isCall := defined.(*ast.CallExpr)
		if !isCall {
			t.Fatalf("%s is assigned off %s, which is defined from %s and not from a call", what, local.Name, spellingOf(defined))
		}
		return call
	}
	committerIdentity, assignsIdentity := assigned["answer.CommitterIdentity"]
	membersAfter, assignsMembers := assigned["answer.MembersAfter"]
	if !assignsIdentity || !assignsMembers {
		t.Fatalf("Process assigns answer.CommitterIdentity: %v and answer.MembersAfter: %v; both are the commit arm's and this gate reads where each is drawn from", assignsIdentity, assignsMembers)
	}
	identityCall := sourceCallOf(t, "answer.CommitterIdentity", committerIdentity)
	identityMethod, isMethod := identityCall.Fun.(*ast.SelectorExpr)
	if !isMethod || spellingOf(identityMethod.X) != "self.group" {
		t.Errorf("answer.CommitterIdentity is drawn from %s, want a method of self.group: the committer's identity is the PRE-COMMIT tree's, which only the live group holds while a commit is staged, and a read off the staged value names whatever the committer's own path put there", spellingOf(identityCall.Fun))
	}
	membersCall := sourceCallOf(t, "answer.MembersAfter", membersAfter)
	handedTheStagedValue := false
	for _, argument := range membersCall.Args {
		if spellingOf(argument) == "processed.Commit" {
			handedTheStagedValue = true
		}
	}
	if !handedTheStagedValue {
		t.Errorf("answer.MembersAfter is drawn from %s, which is not handed processed.Commit: the membership after is the STAGED tree's, and a read off the live group answers the tree the commit is leaving", spellingOf(membersCall.Fun))
	}
	t.Logf("CommitterIdentity is drawn from %s and MembersAfter from %s", spellingOf(identityCall.Fun), spellingOf(membersCall.Fun))
}

// TestCommitRemoveOfTwoLeavesInAFiveMemberGroup is the Remove arm over MORE THAN ONE leaf, which
// no other case in this package builds: TestCommitRemoveOfLeafTwoInAFourMemberGroup removes one,
// so a CommitRemove that built a proposal for leaves[0] alone -- or a RemovedLeaves that
// answered the first entry alone -- passed every case there was. Leaves 1 and 3 of five, chosen
// so that neither is the first nor the last leaf and the survivors straddle both: a cold
// receiver at leaf 2, which never saw a proposal, holds RemovedLeaves == [1, 3] and a
// MembersAfter of exactly leaves 0, 2 and 4; both removed members are handed the report and
// told ErrRemovedFromGroup; and the anchor is the survivor's membership after ApplyCommit.
func TestCommitRemoveOfTwoLeavesInAFiveMemberGroup(t *testing.T) {
	chain := newCommitAddChain(t, "roles-remove-two-of-five")
	third, fourth, fifth := newTestEngine(t), newTestEngine(t), newTestEngine(t)
	keyPackages := [][]byte{}
	for _, engine := range []*testEngine{third, fourth, fifth} {
		keyPackage, err := engine.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("NewKeyPackage: %v", err)
		}
		keyPackages = append(keyPackages, keyPackage)
	}
	commit, welcome, ratchetTree, err := chain.founded.CommitAdd(keyPackages)
	if err != nil {
		t.Fatalf("CommitAdd of three: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	handles := map[uint32]GroupHandle{0: chain.founded, 1: chain.joined}
	for _, engine := range []*testEngine{third, fourth, fifth} {
		handle, err := engine.engine.JoinFromWelcome(welcome, ratchetTree)
		if err != nil {
			t.Fatalf("a join: %v", err)
		}
		defer handle.Close()
		handles[handle.OwnLeafIndex()] = handle
	}
	for leaf := uint32(0); leaf < 5; leaf += 1 {
		if _, held := handles[leaf]; !held {
			t.Fatalf("no member stands at leaf %d; the group is not five members at leaves 0..4", leaf)
		}
	}
	if chain.founded.MemberCount() != 5 {
		t.Fatalf("the founder sees %d members, want 5", chain.founded.MemberCount())
	}
	membersBefore := membersOf(t, handles[2])
	before := contextExtensionsOf(t, handles[2])

	removal, welcome, _, err := chain.founded.CommitRemove([]uint32{1, 3})
	if err != nil {
		t.Fatalf("CommitRemove([1, 3]): %v", err)
	}
	if welcome != nil {
		t.Fatal("a remove answered a welcome")
	}
	wantAfter := []ProcessedMember{}
	for _, member := range membersBefore {
		if member.Leaf != 1 && member.Leaf != 3 {
			wantAfter = append(wantAfter, member)
		}
	}
	if len(wantAfter) != 3 {
		t.Fatalf("the expected survivors are %v, want three", wantAfter)
	}
	// the cold survivor at leaf 2
	survivor, err := handles[2].Process(removal)
	if err != nil {
		t.Fatalf("the survivor's Process: %v", err)
	}
	if len(survivor.RemovedLeaves) != 2 || survivor.RemovedLeaves[0] != 1 || survivor.RemovedLeaves[1] != 3 {
		t.Fatalf("RemovedLeaves = %v, want [1 3]: a Remove of more than one leaf carries every one of them", survivor.RemovedLeaves)
	}
	if len(survivor.AddedLeaves) != 0 || len(survivor.UpdatedLeaves) != 0 {
		t.Fatalf("a remove-only commit reports AddedLeaves %v and UpdatedLeaves %v", survivor.AddedLeaves, survivor.UpdatedLeaves)
	}
	assertSameMembers(t, "the survivor's MembersAfter", survivor.MembersAfter, wantAfter)
	for _, member := range survivor.MembersAfter {
		if member.Leaf == 1 || member.Leaf == 3 {
			t.Fatalf("MembersAfter still holds leaf %d, which this commit removes", member.Leaf)
		}
	}
	assertSameExtensions(t, "the survivor's ContextExtensionsAfter", survivor.ContextExtensionsAfter, before)
	if !bytes.Equal(survivor.CommitterIdentity, chain.founder.identityPub) {
		t.Fatalf("CommitterIdentity = %x, want the founder's %x", survivor.CommitterIdentity, chain.founder.identityPub)
	}
	// both removed members hold the report, with the same two leaves and the same three survivors
	for _, leaf := range []uint32{1, 3} {
		removed, err := handles[leaf].Process(removal)
		if err != nil {
			t.Fatalf("the removed member at leaf %d: Process: %v", leaf, err)
		}
		if len(removed.RemovedLeaves) != 2 || removed.RemovedLeaves[0] != 1 || removed.RemovedLeaves[1] != 3 {
			t.Fatalf("the removed member at leaf %d holds RemovedLeaves = %v, want [1 3]", leaf, removed.RemovedLeaves)
		}
		assertSameMembers(t, "the removed member's MembersAfter", removed.MembersAfter, wantAfter)
		if err := handles[leaf].ApplyCommit(removed); !errors.Is(err, mls.ErrRemovedFromGroup) {
			t.Fatalf("the removed member at leaf %d: ApplyCommit answered %v, want mls.ErrRemovedFromGroup", leaf, err)
		}
	}

	// THE ANCHOR, at every survivor
	if err := handles[2].ApplyCommit(survivor); err != nil {
		t.Fatalf("the survivor's ApplyCommit: %v", err)
	}
	fifthProcessed, err := handles[4].Process(removal)
	if err != nil {
		t.Fatalf("the fifth member's Process: %v", err)
	}
	if err := handles[4].ApplyCommit(fifthProcessed); err != nil {
		t.Fatalf("the fifth member's ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	assertSameMembers(t, "MembersAfter against the survivor's membership after ApplyCommit", survivor.MembersAfter, membersOf(t, handles[2]))
	assertSameEpochSecret(t, 3, map[string]GroupHandle{
		"the committer":    chain.founded,
		"the survivor":     handles[2],
		"the fifth member": handles[4],
	})
}

// liveTreeOf is the ratchet tree the *mls.Group behind a handle of this package is RUNNING ON,
// reached through the field mls keeps unexported, so that a case can build the one commit the
// identity-continuity rule exists for: one whose path leaf carries an identity the pre-commit
// tree does not. The path leaf is the committer's live leaf cloned and re-signed
// (mls/treekem.go, CreateUpdatePathSecrets), so the write below is exactly the write mls's own
// TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt makes one
// package over -- committer.tree.Leaf(own).Credential.Identity = ... -- spelled from outside
// that package.
//
// unsafe IS IN A TEST AND NOWHERE ELSE, and it is here because nothing weaker reaches the
// fixture: no exported door of mls changes a leaf's credential, a doctored persisted state is
// refused by the restore's tree hash, and a hostile committer is by definition one that did not
// walk this seam's doors. The alternative was to leave MembersAfter's identity source pinned by
// the source reading alone, and that reading is passed by a projection that takes identities
// off the live group for every leaf that already exists -- which is the mutant this fixture is
// for. The field is found by name and checked by type, so a rename or a retype in mls fails
// here with a sentence rather than reading past the end of the struct.
func liveTreeOf(t *testing.T, handle GroupHandle) *mls.RatchetTree {
	t.Helper()
	adapter, isAdapter := handle.(*connectMlsHandle)
	if !isAdapter || adapter.group == nil {
		t.Fatal("the handle is not this package's adapter over a live *mls.Group")
	}
	field, found := reflect.TypeOf((*mls.Group)(nil)).Elem().FieldByName("tree")
	if !found {
		t.Fatal("mls.Group declares no field named tree; the swap fixture writes the leaf the committer's path is cloned from, and this is the line to move")
	}
	if field.Type != reflect.TypeOf((*mls.RatchetTree)(nil)) {
		t.Fatalf("mls.Group's tree field is a %s, want *mls.RatchetTree; the swap fixture is written over the wrong storage", field.Type)
	}
	tree := *(**mls.RatchetTree)(unsafe.Add(unsafe.Pointer(adapter.group), field.Offset))
	if tree == nil {
		t.Fatal("the live group holds no ratchet tree")
	}
	return tree
}

// TestMembersAfterNamesTheIdentityTheCommitLeavesAtTheCommittersLeaf is the behavioural half
// of what TestCommitterIdentityIsReadOffTheLiveGroupAndMembersAfterOffTheStagedValue pins off
// the source, over the one commit that tells the two readings apart: the committer's own path
// leaf carries an identity its pre-commit leaf did not, mls accepts it, and the seam must answer
// CommitterIdentity as the PRE-commit identity and MembersAfter at CommitterLeaf as the
// POST-commit one. The authorizer's continuity rule is the comparison of those two, and it is
// vacuous the moment either is read off the other's tree.
//
// WHAT THIS KILLS THAT THE SOURCE READING DOES NOT: a projection that reads identities off the
// live group for every leaf that exists there and off the staged value only for added leaves.
// That projection is handed processed.Commit, so the source reading passes it; over every
// honest commit in this package the two trees agree at every surviving leaf, so every
// behavioural case passes it; and over the swap it hands the authorizer the pre-commit identity
// at exactly the leaf whose change the rule must see. The commit here carries no Add and no
// Update, so the committer's leaf is a leaf that exists live, and the entry for it can only be
// right by being read off the staged tree.
//
// The control runs first, over an honest path commit in the same group: the two reads agree.
// The anchor is the membership the receiver holds after ApplyCommit, which is the swap.
func TestMembersAfterNamesTheIdentityTheCommitLeavesAtTheCommittersLeaf(t *testing.T) {
	chain := newCommitAddChain(t, "roles-identity-swap")
	if bytes.Equal(chain.founder.identityPub, chain.joiner.identityPub) {
		t.Fatal("the founder and the joiner share an identity, so a swap between them is invisible")
	}
	own := chain.founded.OwnLeafIndex()

	// a bare commit through the seam carries a path, because RFC 9420 requires one of a commit
	// with no proposals -- so the committer's leaf is rewritten and nothing is added or updated
	pathCommitAt := func(t *testing.T) *EngineProcessed {
		t.Helper()
		commit, _, _, err := chain.founded.Commit([][]byte{})
		if err != nil {
			t.Fatalf("the founder's bare Commit: %v", err)
		}
		processed, err := chain.joined.Process(commit)
		if err != nil {
			t.Fatalf("the joiner's Process: %v; mls has grown an identity continuity rule, and this case is the line to move", err)
		}
		if processed.CommitterLeaf != own || len(processed.AddedLeaves) != 0 || len(processed.UpdatedLeaves) != 0 {
			t.Fatalf("the commit is by leaf %d with adds %v and updates %v; this case needs the founder's bare path commit", processed.CommitterLeaf, processed.AddedLeaves, processed.UpdatedLeaves)
		}
		return processed
	}

	// THE CONTROL: over an honest path commit the two reads agree, at the founder's identity
	honest := pathCommitAt(t)
	if !bytes.Equal(honest.CommitterIdentity, chain.founder.identityPub) {
		t.Fatalf("over an honest commit CommitterIdentity = %x, want the founder's %x", honest.CommitterIdentity, chain.founder.identityPub)
	}
	if at := memberAtLeaf(t, "the honest commit", honest.MembersAfter, own); !bytes.Equal(at.Identity, chain.founder.identityPub) {
		t.Fatalf("over an honest commit MembersAfter at the committer's leaf = %x, want the founder's %x", at.Identity, chain.founder.identityPub)
	}
	if err := chain.joined.ApplyCommit(honest); err != nil {
		t.Fatalf("ApplyCommit of the honest commit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the honest commit: %v", err)
	}

	// THE SWAP, made as a dishonest committer makes it: its own live leaf now claims the
	// joiner's identity, the next path leaf is cloned from it and re-signed with the untouched
	// signature key, and every honest receiver accepts the commit
	liveTreeOf(t, chain.founded).Leaf(mls.LeafIndex(own)).Credential.Identity = bytes.Clone(chain.joiner.identityPub)
	swapped := pathCommitAt(t)
	if !bytes.Equal(swapped.CommitterIdentity, chain.founder.identityPub) {
		t.Fatalf("over the swap CommitterIdentity = %x, want the founder's pre-commit %x: the committer is named by the tree the commit leaves, and the continuity comparison is made against itself", swapped.CommitterIdentity, chain.founder.identityPub)
	}
	atCommitter := memberAtLeaf(t, "the swapped commit", swapped.MembersAfter, own)
	if !bytes.Equal(atCommitter.Identity, chain.joiner.identityPub) {
		t.Fatalf("over the swap MembersAfter at the committer's leaf = %x, want the joiner's %x, which the path put there: the entry is being read off the live group because the leaf exists there, and the authorizer is handed the identity the commit REPLACED", atCommitter.Identity, chain.joiner.identityPub)
	}
	if bytes.Equal(swapped.CommitterIdentity, atCommitter.Identity) {
		t.Fatal("the two reads agree over the swap, so the comparison the authorizer makes cannot see it")
	}
	if !atCommitter.HasLeafKeys {
		t.Fatal("the swapped path leaf reads as keyless; the clone carries the leaf keys the live leaf did")
	}
	// the joiner's own entry is untouched by the swap
	if at := memberAtLeaf(t, "the swapped commit", swapped.MembersAfter, chain.joined.OwnLeafIndex()); !bytes.Equal(at.Identity, chain.joiner.identityPub) {
		t.Fatalf("the joiner's own entry = %x, want %x", at.Identity, chain.joiner.identityPub)
	}

	// THE ANCHOR: the merge installs the swap, and the membership the receiver holds afterwards
	// is what MembersAfter said before it -- the joiner's identity on both leaves
	if err := chain.joined.ApplyCommit(swapped); err != nil {
		t.Fatalf("ApplyCommit of the swapped commit: %v; mls has grown an identity continuity rule, and this case is the line to move", err)
	}
	assertSameMembers(t, "MembersAfter against the receiver's membership after the swap is installed", swapped.MembersAfter, membersOf(t, chain.joined))
	if !bytes.Equal(identityAtLeaf(t, chain.joined, own), chain.joiner.identityPub) {
		t.Fatal("the swap was not installed at the receiver, so the anchor above compared two honest readings")
	}
}

// TestMembersAfterReportsALeafAdmittedWithoutLeafKeys is HasLeafKeys, over the one commit that
// can put a keyless leaf in a group: an Add of a key package with no urmessage_leaf_keys, built
// through mls directly and past both send doors, which is what a hostile mls build does. mls
// accepts it -- mls's own TestTheStagedTreeAnswersWhetherEachLeafCarriesLeafKeysAndMlsAdmitsALeaf
// Without pins that acceptance and is the line to move if it changes -- and an honest receiver's
// Process answers HasLeafKeys == false at the added leaf and true at every other. The anchor is
// the first symptom the fact exists to pre-empt: after ApplyCommit, MemberAt at that member's
// ordinal refuses with ErrEngineMemberLeafKeys. The control is the same shape over a key package
// that carries the extension, which answers true at the added leaf too.
func TestMembersAfterReportsALeafAdmittedWithoutLeafKeys(t *testing.T) {
	chain := newCommitAddChain(t, "roles-keyless-leaf")
	third := newTestEngine(t)
	// the send doors refuse it, which is the control that the package is keyless for the reason
	// this case names, and the reason the commit below is built past them
	encoded := commitAddKeyPackageWithoutLeafKeys(t, third)
	if _, _, _, err := chain.founded.CommitAdd([][]byte{encoded}); !errors.Is(err, ErrEngineCommitAddKeyPackage) {
		t.Fatalf("CommitAdd over the keyless package answered %v, want ErrEngineCommitAddKeyPackage: the send door has stopped asking", err)
	}
	if _, err := chain.founded.ProposeAdd(encoded); !errors.Is(err, mls.ErrMalformedExtension) {
		t.Fatalf("ProposeAdd over the keyless package answered %v, want mls.ErrMalformedExtension: the send door has stopped asking", err)
	}
	var keyless mls.KeyPackage
	if err := syntax.Unmarshal(encoded, &keyless); err != nil {
		t.Fatalf("decode the keyless package: %v", err)
	}
	adapter, isAdapter := chain.founded.(*connectMlsHandle)
	if !isAdapter {
		t.Fatal("the founder's handle is not this package's adapter")
	}
	result, err := adapter.group.CreateCommit([][]byte{}, []mls.Proposal{{
		ProposalType: mls.ProposalTypeAdd,
		Add:          &mls.Add{KeyPackage: keyless},
	}}, nil)
	if err != nil {
		t.Fatalf("mls's CreateCommit over a by-value Add of the keyless package: %v; mls has grown a receive-side leaf keys rule, and this case is the line to move", err)
	}

	processed, err := chain.joined.Process(result.Commit)
	if err != nil {
		t.Fatalf("the joiner's Process of a commit adding a keyless leaf: %v; mls has grown a receive-side leaf keys rule, and this case is the line to move", err)
	}
	if len(processed.AddedLeaves) != 1 {
		t.Fatalf("AddedLeaves = %v, want one leaf", processed.AddedLeaves)
	}
	added := processed.AddedLeaves[0]
	if len(processed.MembersAfter) != 3 {
		t.Fatalf("MembersAfter holds %d members, want 3", len(processed.MembersAfter))
	}
	for _, member := range processed.MembersAfter {
		if member.Leaf == added {
			if member.HasLeafKeys {
				t.Fatalf("the keyless leaf %d reads HasLeafKeys == true", added)
			}
			if !bytes.Equal(member.Identity, third.identityPub) {
				t.Fatalf("the added leaf carries identity %x, want the third device's %x", member.Identity, third.identityPub)
			}
			continue
		}
		if !member.HasLeafKeys {
			t.Fatalf("leaf %d, which carries the extension, reads HasLeafKeys == false; the fact answers false for everybody", member.Leaf)
		}
	}

	// THE ANCHOR: after the merge the seam's own membership door refuses that member by name
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	refused, answered := 0, 0
	for at := 0; at < chain.joined.MemberCount(); at += 1 {
		leaf, _, _, err := chain.joined.MemberAt(at)
		if errors.Is(err, ErrEngineMemberLeafKeys) {
			refused += 1
			continue
		}
		if err != nil {
			t.Fatalf("MemberAt(%d): %v", at, err)
		}
		if leaf == added {
			t.Fatalf("MemberAt answers the keyless member at leaf %d with no refusal", added)
		}
		answered += 1
	}
	if refused != 1 || answered != 2 {
		t.Fatalf("after the merge MemberAt refused %d members and answered %d, want 1 and 2: the symptom HasLeafKeys pre-empts is not what it was measured to be", refused, answered)
	}

	// THE CONTROL: the same shape over a package that carries the extension answers true at
	// the added leaf, and the keyless member still reads false one commit later
	fourth := newTestEngine(t)
	keyed, err := fourth.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}
	commit, _, _, err := chain.founded.CommitAdd([][]byte{keyed})
	if err != nil {
		t.Fatalf("the control CommitAdd: %v", err)
	}
	control, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the control Process: %v", err)
	}
	if len(control.AddedLeaves) != 1 {
		t.Fatalf("the control's AddedLeaves = %v, want one leaf", control.AddedLeaves)
	}
	if at := memberAtLeaf(t, "the control", control.MembersAfter, control.AddedLeaves[0]); !at.HasLeafKeys {
		t.Fatal("a keyed added leaf reads HasLeafKeys == false; the fact answers false for every added leaf and the keyless reading above agrees with it for the wrong reason")
	}
	if at := memberAtLeaf(t, "the control", control.MembersAfter, added); at.HasLeafKeys {
		t.Fatal("the keyless member reads as keyed one commit later; the fact is not being read off the leaf")
	}
	if err := chain.joined.DiscardProcessed(control); err != nil {
		t.Fatalf("DiscardProcessed of the control: %v", err)
	}
	chain.founded.ClearPendingCommit()
}

// spellingOf is a selector chain or identifier as the source spells it, and a marker for any
// other expression so a comparison against a spelling cannot match one by accident.
func spellingOf(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return spellingOf(typed.X) + "." + typed.Sel.Name
	case *ast.CallExpr:
		return spellingOf(typed.Fun) + "(...)"
	case *ast.IndexExpr:
		return spellingOf(typed.X) + "[...]"
	}
	return "<expression>"
}
