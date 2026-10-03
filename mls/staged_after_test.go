// The two facts a receiving client's authorization decision reads off a STAGED commit and can read
// nowhere else, and the one default MASTER section 11 pinned on 2026-09-21.
//
// MASTER section 11 has a bad commit "rejected by every receiving client on validation", and a
// receiving client validates BETWEEN ProcessMessage and ApplyCommit. At that moment the group's
// live tree is the PRE-commit tree, so "who is in the group afterwards" -- the identities an Add
// admits, whether an Update or the committer's own path kept its leaf's identity, the identity set
// the post-commit policy is judged against -- is a question only the staged tree can answer.
// (*StagedCommit).OccupiedLeavesAfter and LeafIdentityAfter are that answer, and the cases here
// hold them to the tree the group ENTERS rather than to the tree it is leaving.
package mls

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// TestRoleOfAnUnnamedMemberIsMemberAndNotNamed is ledger item 242's ruling 8 as a property: the
// role a policy answers for an identity it does not name is MEMBER, the bool says "unnamed", and
// (*Group).Members agrees -- the two defaults that used to disagree are one default.
//
// The control is a NAMED member, so a RoleOf that stopped reading the list would fail here rather
// than pass by answering the default for everybody.
func TestRoleOfAnUnnamedMemberIsMemberAndNotNamed(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	admin := testIdentity(t, crypto, "admin")
	member := testIdentity(t, crypto, "member")
	stranger := testIdentity(t, crypto, "stranger")
	policy := testPolicy(t, owner, admin, member)

	role, named := policy.RoleOf(stranger.IdentityPub)
	if named {
		t.Fatalf("RoleOf names an identity the policy holds no entry for; the bool is what tells a named MEMBER from an unnamed one")
	}
	if role != RoleMember {
		t.Fatalf("RoleOf(unnamed) = %s, want %s: MASTER section 11 rules that a member the policy does not name is a MEMBER, and names the OBSERVER answer as the falsifying one", role, RoleMember)
	}
	// the control: the list is still read
	if role, named := policy.RoleOf(admin.IdentityPub); !named || role != RoleAdmin {
		t.Fatalf("RoleOf(a named admin) = %s %v, want admin true", role, named)
	}

	// and the group's own membership view answers the same default for an unnamed leaf, which is
	// the disagreement item 242 measured: a joiner CommitAdd admits is named by nothing.
	group, joined, _, bob := testTwoMemberGroup(t, crypto)
	defer group.Close()
	defer joined.Close()
	groupPolicy, err := group.GroupPolicy()
	if err != nil {
		t.Fatalf("GroupPolicy: %v", err)
	}
	if _, named := groupPolicy.RoleOf(bob.IdentityPub); named {
		t.Fatal("the fixture's policy names the joiner, so this case cannot observe the unnamed default")
	}
	for _, view := range []*Group{group, joined} {
		found := false
		for _, entry := range view.Members() {
			if !bytes.Equal(entry.IdentityPub, bob.IdentityPub) {
				continue
			}
			found = true
			if entry.Role != RoleMember {
				t.Fatalf("Members() answers %s for the unnamed joiner and RoleOf answers %s; an authorizer written off either must read the same default", entry.Role, RoleMember)
			}
		}
		if !found {
			t.Fatal("the joiner is not in the membership this view answers")
		}
	}
}

// TestTheStagedTreeAnswersEveryPostCommitLeafWithItsIdentity is the accessor pair against a commit
// that both ADDS a member and carries the committer's own path: the leaves it answers are the
// pre-commit occupied leaves plus the added ones, the identity at the added leaf is the joiner's,
// the identity at the committer's leaf is unchanged across its own path, and -- the anchor -- the
// membership the receiver holds AFTER ApplyCommit is exactly what the staged value said before it.
func TestTheStagedTreeAnswersEveryPostCommitLeafWithItsIdentity(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, owner, bob := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	carol := testIdentity(t, crypto, "carol")
	kp, _, _ := testKeyPackage(t, crypto, carol)
	result, err := committer.CreateCommit([][]byte{}, []Proposal{{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *kp},
	}}, &CommitOptions{Force: true})
	if err != nil {
		t.Fatalf("CreateCommit over a by-value add with a forced path: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	staged := processed.Commit
	if staged == nil || !staged.hasPath || len(staged.AddedLeaves()) != 1 {
		t.Fatal("this commit does not both add a member and carry a path, so the case below observes half of what it claims")
	}
	before := receiver.tree.NonBlankLeaves()
	after := staged.OccupiedLeavesAfter()
	wantAfter := slices.Concat(before, staged.AddedLeaves())
	slices.Sort(wantAfter)
	if !slices.Equal(after, wantAfter) {
		t.Fatalf("OccupiedLeavesAfter = %v, want the pre-commit leaves %v plus the added %v", after, before, staged.AddedLeaves())
	}
	added := staged.AddedLeaves()[0]
	if identity, held := staged.LeafIdentityAfter(added); !held || !bytes.Equal(identity, carol.IdentityPub) {
		t.Fatalf("LeafIdentityAfter(added leaf %d) = %x %v, want the joiner's identity %x", added, identity, held, carol.IdentityPub)
	}
	if identity, held := staged.LeafIdentityAfter(staged.Committer()); !held || !bytes.Equal(identity, owner.IdentityPub) {
		t.Fatalf("LeafIdentityAfter(committer's leaf %d) = %x %v, want %x: a leaf's identity does not change across the committer's own path", staged.Committer(), identity, held, owner.IdentityPub)
	}
	if identity, held := staged.LeafIdentityAfter(receiver.OwnLeafIndex()); !held || !bytes.Equal(identity, bob.IdentityPub) {
		t.Fatalf("LeafIdentityAfter(the receiver's own leaf) = %x %v, want %x", identity, held, bob.IdentityPub)
	}
	// a blank leaf and one outside the tree are false, never a zero identity
	if identity, held := staged.LeafIdentityAfter(LeafIndex(1000)); held || identity != nil {
		t.Fatalf("LeafIdentityAfter(a leaf outside the tree) = %x %v, want nil false", identity, held)
	}
	// the copy: writing through the answer changes nothing the staged tree holds
	identity, _ := staged.LeafIdentityAfter(added)
	for i := range identity {
		identity[i] ^= 0xff
	}
	if again, _ := staged.LeafIdentityAfter(added); !bytes.Equal(again, carol.IdentityPub) {
		t.Fatal("LeafIdentityAfter answers a window onto the staged tree; a caller writing through it rewrote the leaf the new epoch's tree hash was taken over")
	}

	// THE ANCHOR: what the receiver holds after the merge is what the staged value said before it.
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	live := map[LeafIndex][]byte{}
	for _, member := range receiver.Members() {
		live[member.LeafIndex] = member.IdentityPub
	}
	if len(live) != len(after) {
		t.Fatalf("the receiver holds %d members after the merge and the staged tree answered %d leaves", len(live), len(after))
	}
	for _, leaf := range after {
		wanted, _ := staged.LeafIdentityAfter(leaf)
		if !bytes.Equal(live[leaf], wanted) {
			t.Fatalf("leaf %d holds identity %x after the merge and the staged tree answered %x before it", leaf, live[leaf], wanted)
		}
	}
}

// TestARemovedMembersReportAnswersTheStagedTreeAndSaysItsContextIsUnchanged is the accessor pair
// on the one staged value that carries no context: the report a member handed the commit that
// removes it is given. The tree is there, so the leaves and identities are answered; the context
// is not, so GroupContextExtensions answers nil for a commit carrying no GroupContextExtensions
// proposal -- "unchanged", by the accessor's own doc -- and the proposal's list when it carries
// one. Before this case that accessor dereferenced the nil context.
func TestARemovedMembersReportAnswersTheStagedTreeAndSaysItsContextIsUnchanged(t *testing.T) {
	crypto := testCrypto(t)
	for _, arm := range []struct {
		name    string
		withGce bool
	}{
		{name: "a bare remove", withGce: false},
		{name: "a remove beside a group context extensions proposal", withGce: true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			committer, removed, owner, _ := testTwoMemberGroup(t, crypto)
			defer committer.Close()
			defer removed.Close()
			byValue := []Proposal{{
				ProposalType: ProposalTypeRemove,
				Remove:       &Remove{Removed: removed.OwnLeafIndex()},
			}}
			var replaced []Extension
			if arm.withGce {
				context := testGroupContextOf(t, committer)
				policy, err := GroupPolicyOf(context.Extensions)
				if err != nil {
					t.Fatalf("GroupPolicyOf: %v", err)
				}
				policy.RetentionPolicy.MediaMs += 1
				policyExt, err := policy.Encode()
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				for _, extension := range context.Extensions {
					if extension.ExtensionType == ExtensionTypeUrmessageGroupPolicy {
						extension = policyExt
					}
					replaced = append(replaced, extension)
				}
				byValue = append(byValue, Proposal{
					ProposalType:           ProposalTypeGroupContextExtensions,
					GroupContextExtensions: &GroupContextExtensions{Extensions: replaced},
				})
			}
			result, err := committer.CreateCommit([][]byte{}, byValue, nil)
			if err != nil {
				t.Fatalf("CreateCommit: %v", err)
			}
			processed, err := removed.ProcessMessage(result.Commit)
			if err != nil {
				t.Fatalf("the removed member's ProcessMessage: %v", err)
			}
			staged := processed.Commit
			if staged == nil || !staged.RemovesSelf() || staged.context != nil {
				t.Fatal("this is not the report a removed member is handed, so the case observes nothing about it")
			}
			if leaves := staged.OccupiedLeavesAfter(); !slices.Equal(leaves, []LeafIndex{committer.OwnLeafIndex()}) {
				t.Fatalf("OccupiedLeavesAfter on the removed member's report = %v, want only the committer's leaf %d", leaves, committer.OwnLeafIndex())
			}
			if identity, held := staged.LeafIdentityAfter(committer.OwnLeafIndex()); !held || !bytes.Equal(identity, owner.IdentityPub) {
				t.Fatalf("LeafIdentityAfter(committer) on the report = %x %v, want %x", identity, held, owner.IdentityPub)
			}
			if _, held := staged.LeafIdentityAfter(removed.OwnLeafIndex()); held {
				t.Fatal("the report still answers an identity at the leaf the commit blanked")
			}
			extensions := staged.GroupContextExtensions()
			if !arm.withGce {
				if extensions != nil {
					t.Fatalf("GroupContextExtensions on a report for a commit carrying no GCE = %d entries, want nil, which the accessor documents as unchanged", len(extensions))
				}
			} else {
				if len(extensions) != len(replaced) {
					t.Fatalf("GroupContextExtensions on a report for a commit carrying a GCE = %d entries, want the proposal's %d", len(extensions), len(replaced))
				}
				for i := range replaced {
					if extensions[i].ExtensionType != replaced[i].ExtensionType || !bytes.Equal(extensions[i].ExtensionData, replaced[i].ExtensionData) {
						t.Fatalf("entry %d of the report's extensions is not the proposal's", i)
					}
				}
			}
			// and the report still does what it did: ApplyCommit closes the group and says why
			if err := removed.ApplyCommit(processed); !errors.Is(err, ErrRemovedFromGroup) {
				t.Fatalf("ApplyCommit over the report answered %v, want ErrRemovedFromGroup", err)
			}
		})
	}
}

// the post-commit list a live receiver's staged commit answers is the context's, byte for byte,
// which is what a receiving client compares against its pre-commit list to see that nothing but
// the policy moved. Held here because the seam one package up states it as its contract.
func TestAStagedCommitsExtensionsAreThePostCommitContextsByteForByte(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()
	before := testGroupContextOf(t, receiver).Extensions
	result, err := committer.CreateCommit([][]byte{}, nil, &CommitOptions{Force: true})
	if err != nil {
		t.Fatalf("CreateCommit over an empty list with a path: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	after := processed.Commit.GroupContextExtensions()
	if len(after) != len(before) || len(before) < 2 {
		t.Fatalf("the staged commit answers %d extensions and the pre-commit context carries %d (want at least the policy and required_capabilities)", len(after), len(before))
	}
	for i := range before {
		if after[i].ExtensionType != before[i].ExtensionType || !bytes.Equal(after[i].ExtensionData, before[i].ExtensionData) {
			t.Fatalf("entry %d differs across a commit carrying no GroupContextExtensions proposal", i)
		}
	}
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	encoded, err := receiver.GroupContext()
	if err != nil {
		t.Fatalf("GroupContext: %v", err)
	}
	entered := &GroupContext{}
	if err := syntax.Unmarshal(encoded, entered); err != nil {
		t.Fatalf("unmarshal the entered context: %v", err)
	}
	for i := range after {
		if entered.Extensions[i].ExtensionType != after[i].ExtensionType || !bytes.Equal(entered.Extensions[i].ExtensionData, after[i].ExtensionData) {
			t.Fatalf("entry %d of the entered context is not what the staged commit answered before the merge", i)
		}
	}
}

// TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt is the
// fact the seam's CommitterIdentity rests on, pinned where it can be built: a committer's own
// path leaf is its current leaf cloned and re-signed (treekem.go, CreateUpdatePathSecrets), and
// NOTHING in this package's commit validation compares the credential identity on that leaf
// against the one the leaf carried before -- grep validate_commit.go for Credential.Identity and
// find none. So a committer that rewrites its own leaf's identity to another member's before it
// commits produces a commit every honest receiver ACCEPTS, and after the merge the victim's
// identity stands on two leaves and the committer's on none. Ledger item 242's M7 names this
// hole and rules where it is closed: by the AUTHORIZER, on both arms, with the rule that a
// leaf's identity does not change across the committer's own path -- a rule that can only be
// written by comparing the committer's identity BEFORE the commit against its leaf AFTER it.
//
// What is held is therefore the two reads that rule needs, read exactly as the seam's adapter
// reads them, over the swapped commit while it is staged: (*Group).MemberAt on the LIVE group at
// Committer() -- the adapter's CommitterIdentity -- still carries the original identity, and
// (*StagedCommit).LeafIdentityAfter at the same index -- the adapter's MembersAfter entry --
// carries the swap. A CommitterIdentity read off the staged tree would name the victim here and
// the authorizer's continuity rule would compare the swap against itself. The honest control
// runs first in the same group: over a path commit with no swap the two reads agree.
//
// AND THE ACCEPTANCE IS PINNED ON PURPOSE. If this package ever grows the continuity rule, the
// ProcessMessage below refuses and this case is the line to move; until then the rule lives
// above this package, and a case that quietly stopped building the swap would leave the seam's
// doc resting on a fact nothing holds.
func TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, owner, bob := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	// the two reads, spelled as the adapter spells them
	liveRead := func(t *testing.T, staged *StagedCommit) []byte {
		t.Helper()
		member, isMember := receiver.MemberAt(staged.Committer())
		if !isMember {
			t.Fatalf("the live tree holds nobody at the committer's leaf %d", staged.Committer())
		}
		return member.IdentityPub
	}
	stagedRead := func(t *testing.T, staged *StagedCommit) []byte {
		t.Helper()
		identity, held := staged.LeafIdentityAfter(staged.Committer())
		if !held {
			t.Fatalf("the staged tree holds nobody at the committer's leaf %d", staged.Committer())
		}
		return identity
	}
	pathCommitAt := func(t *testing.T) *Processed {
		t.Helper()
		result, err := committer.CreateCommit([][]byte{}, nil, &CommitOptions{Force: true})
		if err != nil {
			t.Fatalf("CreateCommit with a forced path: %v", err)
		}
		processed, err := receiver.ProcessMessage(result.Commit)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if processed.Commit == nil || !processed.Commit.hasPath {
			t.Fatal("this commit carries no path, so it rewrites no leaf and the case observes nothing")
		}
		return processed
	}

	// THE CONTROL: an honest path commit, over which the two reads agree
	honest := pathCommitAt(t)
	if live, after := liveRead(t, honest.Commit), stagedRead(t, honest.Commit); !bytes.Equal(live, owner.IdentityPub) || !bytes.Equal(after, owner.IdentityPub) {
		t.Fatalf("over an honest path commit the live read is %x and the staged read %x, want the owner's %x for both", live, after, owner.IdentityPub)
	}
	if err := receiver.ApplyCommit(honest); err != nil {
		t.Fatalf("ApplyCommit of the honest commit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the honest commit: %v", err)
	}

	// THE SWAP, made exactly as a dishonest committer would make it: its own leaf's credential
	// now claims the receiver's identity, and so does the credential it signs its next leaf
	// over. The signature key is untouched, so the re-signed leaf verifies.
	own := committer.OwnLeafIndex()
	committer.tree.Leaf(own).Credential.Identity = cloneBytes(bob.IdentityPub)
	committer.cred.Identity = cloneBytes(bob.IdentityPub)
	swapped := pathCommitAt(t)
	live, after := liveRead(t, swapped.Commit), stagedRead(t, swapped.Commit)
	if !bytes.Equal(live, owner.IdentityPub) {
		t.Fatalf("the live read at the committer's leaf answers %x while the swap is staged, want the owner's %x: the pre-commit tree is what still names the committer", live, owner.IdentityPub)
	}
	if !bytes.Equal(after, bob.IdentityPub) {
		t.Fatalf("the staged read at the committer's leaf answers %x while the swap is staged, want the receiver's %x: the path leaf carries the swap", after, bob.IdentityPub)
	}
	if bytes.Equal(live, after) {
		t.Fatal("the two reads agree over the swapped commit, so nothing about CommitterIdentity's pre-commit reading is observed here")
	}
	// and the acceptance, with what it leaves behind: the merge installs the swap, the
	// receiver's identity stands on both leaves and the owner's on none
	if err := receiver.ApplyCommit(swapped); err != nil {
		t.Fatalf("ApplyCommit of the swapped commit: %v; this package has grown an identity continuity rule, and this case is the line to move", err)
	}
	ownerOnALeaf := false
	bobLeaves := 0
	for _, member := range receiver.Members() {
		if bytes.Equal(member.IdentityPub, owner.IdentityPub) {
			ownerOnALeaf = true
		}
		if bytes.Equal(member.IdentityPub, bob.IdentityPub) {
			bobLeaves += 1
		}
	}
	if ownerOnALeaf || bobLeaves != 2 {
		t.Fatalf("after the swap is installed the owner's identity is on a leaf: %v, and the receiver's is on %d leaves; want none and two, which is the state item 242's M7 describes", ownerOnALeaf, bobLeaves)
	}
}

// TestTheStagedTreeAnswersWhetherEachLeafCarriesLeafKeysAndMlsAdmitsALeafWithout is the third
// fact a receiving client reads off the staged tree, and the acceptance it rests on, pinned
// together for the swap case's reason.
//
// THE ACCEPTANCE FIRST, because it is the hole. Both send doors refuse a key package whose leaf
// carries no urmessage_leaf_keys -- ProposeAdd and messagegroup's CommitAdd both ask LeafKeysOf
// before anything is staged -- and nothing on the receive side asks it: ValSem106 requires an
// added leaf to LIST the type in its capabilities and never to carry one, and a hostile build
// that skips its own send door admits a leaf no epoch wrap addresses. So an Add built here by
// value, through CreateCommit and past ProposeAdd, of a key package without the extension is
// processed by an honest receiver with no error, and this case says so on purpose: if this
// package ever grows the rule the ProcessMessage below refuses and this is the line to move.
// Until then the rule is the authorizer's, one layer up, and what it needs from here is the
// FACT.
//
// THE FACT: LeafHasKeysAfter answers false at the leaf the keyless Add landed and true at
// every other occupied leaf, off the staged tree and before the merge. The control is the
// same commit shape over a key package that carries the extension, which answers true at
// every leaf including the added one -- so an accessor that answered false for every added
// leaf, or false for everybody, fails here rather than passing by agreeing with the hole. And
// the anchor is the group after the merge: (*Group).Members answers a nil LeafKeys for exactly
// that member, which is the first symptom the receiving-side fact exists to pre-empt.
func TestTheStagedTreeAnswersWhetherEachLeafCarriesLeafKeysAndMlsAdmitsALeafWithout(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	// a key package signed by a real signer over a real credential, minus the one extension:
	// LeafKeysOf refuses it, which is the send doors' refusal, and it is the control that the
	// package is keyless for the reason this case names
	carol := testIdentity(t, crypto, "carol")
	keyless, _, _, err := NewKeyPackageWithSigner(crypto, crypto.Suite(), carol.SigPriv,
		BasicCredential(carol.IdentityPub), testCapabilities(), nil)
	if err != nil {
		t.Fatalf("a key package with no leaf keys: %v", err)
	}
	if _, err := LeafKeysOf(&keyless.LeafNode); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("LeafKeysOf over the keyless package = %v, want ErrMalformedExtension: the package this case adds is not keyless", err)
	}
	// and the send door refuses it, so the by-value commit below is the only way in
	encodedKeyless, err := syntax.Marshal(keyless)
	if err != nil {
		t.Fatalf("encode the keyless package: %v", err)
	}
	if _, err := committer.ProposeAdd(encodedKeyless); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("ProposeAdd over the keyless package = %v, want ErrMalformedExtension: the send door has stopped asking, and this case is then not about a hole the receive side alone has", err)
	}

	result, err := committer.CreateCommit([][]byte{}, []Proposal{{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *keyless},
	}}, nil)
	if err != nil {
		t.Fatalf("CreateCommit over a by-value Add of a keyless package: %v; this package has grown a receive-side leaf keys rule at the committer, and this case is the line to move", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage of a commit adding a keyless leaf: %v; this package has grown a receive-side leaf keys rule, and this case is the line to move", err)
	}
	staged := processed.Commit
	if staged == nil || len(staged.AddedLeaves()) != 1 {
		t.Fatal("the commit did not stage exactly one Add, so nothing below is about the keyless leaf")
	}
	added := staged.AddedLeaves()[0]
	if staged.LeafHasKeysAfter(added) {
		t.Fatalf("LeafHasKeysAfter(the keyless leaf %d) = true, want false", added)
	}
	occupied := staged.OccupiedLeavesAfter()
	if !slices.Contains(occupied, added) {
		t.Fatalf("the added leaf %d is not among the post-commit leaves %v", added, occupied)
	}
	for _, leaf := range occupied {
		if leaf == added {
			continue
		}
		if !staged.LeafHasKeysAfter(leaf) {
			t.Fatalf("LeafHasKeysAfter(leaf %d) = false for a leaf that carries the extension; the accessor answers false for everybody", leaf)
		}
	}
	// blank and outside the tree, which are the two shapes every leaf-taking accessor of this
	// type answers false for
	if staged.LeafHasKeysAfter(occupied[len(occupied)-1]+1) || staged.LeafHasKeysAfter(LeafIndex(1<<20)) {
		t.Fatal("LeafHasKeysAfter answers true for a blank leaf or one outside the tree")
	}

	// THE ANCHOR: after the merge, the group's own membership view carries no leaf keys for
	// exactly that member and leaf keys for every other
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	for _, member := range receiver.Members() {
		if (member.LeafKeys == nil) != (member.LeafIndex == added) {
			t.Fatalf("after the merge the member at leaf %d carries leaf keys: %v; the staged answer said %v for that leaf",
				member.LeafIndex, member.LeafKeys != nil, staged.LeafHasKeysAfter(member.LeafIndex))
		}
	}

	// THE CONTROL: the same shape over a package that carries the extension answers true at the
	// added leaf too
	dave := testIdentity(t, crypto, "dave")
	keyed, _, _ := testKeyPackage(t, crypto, dave)
	control, err := committer.CreateCommit([][]byte{}, []Proposal{{
		ProposalType: ProposalTypeAdd,
		Add:          &Add{KeyPackage: *keyed},
	}}, nil)
	if err != nil {
		t.Fatalf("the control CreateCommit: %v", err)
	}
	controlProcessed, err := receiver.ProcessMessage(control.Commit)
	if err != nil {
		t.Fatalf("the control ProcessMessage: %v", err)
	}
	controlStaged := controlProcessed.Commit
	if controlStaged == nil || len(controlStaged.AddedLeaves()) != 1 {
		t.Fatal("the control commit did not stage exactly one Add")
	}
	if !controlStaged.LeafHasKeysAfter(controlStaged.AddedLeaves()[0]) {
		t.Fatal("LeafHasKeysAfter(a keyed added leaf) = false; the accessor answers false for every added leaf and the keyless reading above agrees with it for the wrong reason")
	}
	if controlStaged.LeafHasKeysAfter(added) {
		t.Fatal("the keyless member reads as keyed one commit later; the accessor is not reading the leaf")
	}
	controlStaged.Zeroize()
	committer.ClearPendingCommit()
}
