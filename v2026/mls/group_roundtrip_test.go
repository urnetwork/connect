// The round trip: this plan's whole group lifecycle, run over many members and many epochs, with
// every member's view of every epoch compared against every other member's.
//
// WHY THIS FILE IS NOT A LONGER VERSION OF THE OTHERS. Every other file in this package holds one
// piece against its own definition. This one holds the pieces against EACH OTHER, and the four
// defects this package has actually shipped were all agreements rather than pieces: ProposeAdd
// cached an Add its own ValidateProposalList refuses; ApplyCommit accepted another group's staged
// commit and derived byte identical authenticators from it; a restored member restarted its ratchet
// at generation 0 and reused a nonce; the Welcome's joiner pairing was checked by a count its own
// comment ruled out. Not one of the four is visible from inside the piece that holds it.
//
// HOW MANY OF THE FOUR THIS FILE REPRODUCES, MEASURED BY REINTRODUCING EACH ONE. It is now all
// four. It was ONE when the file shipped, and this paragraph replaces a sentence that said the
// opposite by implication -- "every one of them is visible to a second member reading what the
// first produced", which is a statement about what a second member COULD see and was read as a
// statement about what this file does see. Three of the four were unreachable by any arrangement
// the file built, and each needed a fixture it did not have:
//
//   - THE CACHED ADD. Every add here was a fresh valid identity, so no case could offer the group
//     a key package any list rule refuses. TestTheGeneratorRefusesAnAddThisGroupsOwnListRuleRefuses
//     offers back one the group already carries. It is also held by the generator gates in
//     group_test.go, and was before this; what this adds is the consequence -- a poisoned cache is
//     a commit every OTHER member refuses, which is a statement only n views can make.
//   - THE CROSS-GROUP STAGED COMMIT. A staged commit was only ever handed to the group whose
//     ProcessMessage produced it, because the file drove one cohort.
//     TestAStagedCommitFromAnotherGroupOrAnotherEpochInstallsNothingHere drives two.
//   - THE WELCOME'S JOINER PAIRING. Every commit here added exactly ONE member, and with one entry
//     on each side of the pairing, entry 0 of one is entry 0 of the other under every permutation.
//     TestOneCommitAdmitsTwoJoinersAndEachOpensTheEntryAddressedToIt admits two in one commit --
//     and asks for an update path, because section 12.4 lets an add-only commit omit one and a
//     Welcome with no path secret in it carries nothing a swapped pairing could misdeliver.
//     Measured: with the pairing swapped, a two-joiner case on an add-only commit passed.
//   - THE RESTORED RATCHET is the one that was already caught, by
//     TestEveryMemberSendsAndReceivesAcrossARestore, which sends after the restore.
//
// WHAT IT STILL DOES NOT REACH, so the count above stays a count of what was measured. The
// RECEIVING half of a restore is not here: the persisted state carries no peer position, and what
// that costs is held by group_restore_generation_test.go's
// TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound and by MaxGenerationSkip's own comment.
// Reaching it from a cohort would mean a case that sends a thousand messages between two views.
//
// MOST CASES HERE HAVE ONE SHAPE: n views of one group, each a DIFFERENT *Group with its own
// StateStore, its own tree private state and its own key schedule, and the assertion is that all n
// answer one epoch authenticator. That value is DeriveSecret(epoch_secret, "authentication"), so an
// agreement on it is an agreement about the tree, the transcript and the key schedule together --
// a divergence in any one of the three is reported at the commit that caused it rather than at the
// first message somebody could not open. Membership is compared beside it, because two views can
// agree on an epoch secret while disagreeing about who is in the group only if one of them derived
// the secret over a tree it did not publish, and saying both is what tells those apart.
//
// AND THE THREE ADDED ABOVE DELIBERATELY DO NOT. Two of them need something an agreement between n
// views of one group cannot contain -- a SECOND group, and a second JOINER of one commit -- and the
// header used to say every case had the one shape, which is how the file came to have no case that
// could hold three of the four defects it is named for.
//
// FOUR MEMBERS IS THE FLOOR AND SEVERAL SIZES ARE RUN. four_member_group_test.go measures why: four
// is the smallest size at which EVERY member has a copath node above its own leaf, and at two the
// only node a commit is ever sealed to for a receiver is that receiver's own leaf -- so a whole
// class of structural defect is invisible under a two member fixture. The sizes above four are run
// because the property is the member's POSITION and not the group's size: at five, leaf 4 stands
// alone under the right subtree and behaves exactly as a member of two does, so "four or more" is
// a false way to state any of it.
//
// AND THE UNMERGED LEAF. four_member_group_test.go records, measured, that NO fixture in this
// package puts a member in a resolution reached through an unmerged leaf -- its groups settle, and
// a settled tree carries none. TestACommitReachesTheNewestMemberThroughAnUnmergedLeaf builds one,
// and it asserts the SHAPE off the tree before it sends anything, so a change that stopped
// producing an unmerged leaf makes that case fail rather than quietly turning it into another
// ordinary round trip.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// ---------------------------------------------------------------------------
// the cohort
// ---------------------------------------------------------------------------

// testCohort is n views of ONE group that must stay in lockstep.
//
// It is not testSizedGroup with different field names. That fixture answers a SETTLED group and
// asserts its own invariant as it builds; this one is a live cohort a case drives -- members join,
// leave and are restored while it runs, and the assertions are the case's rather than the
// fixture's, so a case here reports the property it names instead of inheriting somebody else's.
//
// It carries the CONFIG of every member beside the group, for testSizedGroup's reason:
// testGroupConfig mints a fresh StateStore per call, so the only way to reach the states a member
// has persisted is to hold the config that member joined under, and a restore built from a second
// config for the same identity would be asking a store nothing has ever written to.
type testCohort struct {
	crypto  CryptoProvider
	groupId string
	groups  []*Group
	members []*testMember
	configs []*GroupConfig
}

// testNewCohort founds a group and admits size-1 members into it.
//
// The result is NOT settled: a group assembled out of Add commits alone has every parent of every
// added leaf blank, so every resolution walks down to the leaves and a group of n behaves like a
// group of two. A case that needs the tree's interior filled says so by committing from every
// member, which is what settle does below -- named rather than folded in here, because half the
// cases in this file are about what happens on the way to a settled tree.
func testNewCohort(t *testing.T, crypto CryptoProvider, groupId string, size int) *testCohort {
	t.Helper()
	if size < 1 {
		t.Fatalf("a cohort of %d members is not a group", size)
	}
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, groupId)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	self := &testCohort{
		crypto:  crypto,
		groupId: groupId,
		groups:  []*Group{group},
		members: []*testMember{owner},
		configs: []*GroupConfig{cfg},
	}
	for i := 1; i < size; i += 1 {
		self.addMember(t, group, testMemberName(i))
	}
	return self
}

// closeAll erases every epoch this cohort derived.
func (self *testCohort) closeAll() {
	for _, group := range self.groups {
		group.Close()
	}
}

// deliver hands one message to every view except the author's, applying it when it is a commit and
// merging the author's own last.
//
// THE AUTHOR MERGES AFTER EVERY RECEIVER HAS APPLIED, which is the order the delivery service
// imposes and not a convenience: a committer that merged first would be in the new epoch while the
// message it is about to send is sealed under the old one, and every case in this file that asserts
// an agreement would then be asserting it about a fixture that had already forked.
func (self *testCohort) deliver(t *testing.T, author *Group, message []byte, isCommit bool) {
	t.Helper()
	for _, group := range self.groups {
		if group == author {
			continue
		}
		processed, err := group.ProcessMessage(message)
		if err != nil {
			t.Fatalf("leaf %d could not process a message from leaf %d: %v",
				group.OwnLeafIndex(), author.OwnLeafIndex(), err)
		}
		if isCommit {
			if err := group.ApplyCommit(processed); err != nil {
				t.Fatalf("leaf %d could not apply the commit of leaf %d: %v",
					group.OwnLeafIndex(), author.OwnLeafIndex(), err)
			}
		}
	}
	if isCommit {
		if err := author.MergePendingCommit(); err != nil {
			t.Fatalf("MergePendingCommit at leaf %d: %v", author.OwnLeafIndex(), err)
		}
	}
}

// assertLockstep fails unless every view agrees on the epoch, on the epoch authenticator and on the
// membership.
//
// THE EPOCH AUTHENTICATOR IS THE STRONGEST ASSERTION THIS PLAN HAS. It is
// DeriveSecret(epoch_secret, "authentication"), so a match means the tree, the transcript and the
// key schedule all agree -- an agreement about a secret rather than about some public function of
// the tree, which a member that had reconstructed the tree wrongly could still reproduce.
//
// MEMBERSHIP IS COMPARED ELEMENTWISE AND NOT BY COUNT. A count is satisfied by two views that hold
// the same NUMBER of members at different leaves, which is exactly what an off-by-one in a
// resolution or an add placement produces, and it is the shape of a defect this package has already
// shipped once: the Welcome's joiner pairing was checked by a count its own comment ruled out.
func (self *testCohort) assertLockstep(t *testing.T) {
	t.Helper()
	first := self.groups[0]
	for at, group := range self.groups {
		if at == 0 {
			continue
		}
		if group.Epoch() != first.Epoch() {
			t.Fatalf("leaf %d is at epoch %d and leaf %d at %d",
				group.OwnLeafIndex(), group.Epoch(), first.OwnLeafIndex(), first.Epoch())
		}
		if !bytes.Equal(group.EpochAuthenticator(), first.EpochAuthenticator()) {
			t.Fatalf("leaf %d and leaf %d disagree on the epoch authenticator at epoch %d, so they are not in one epoch of one group",
				group.OwnLeafIndex(), first.OwnLeafIndex(), group.Epoch())
		}
		mine, theirs := group.Members(), first.Members()
		if len(mine) != len(theirs) {
			t.Fatalf("leaf %d holds %d members and leaf %d holds %d",
				group.OwnLeafIndex(), len(mine), first.OwnLeafIndex(), len(theirs))
		}
		for i := range mine {
			if mine[i].LeafIndex != theirs[i].LeafIndex {
				t.Fatalf("member %d is at leaf %d for leaf %d and at leaf %d for leaf %d",
					i, mine[i].LeafIndex, group.OwnLeafIndex(), theirs[i].LeafIndex, first.OwnLeafIndex())
			}
			if !bytes.Equal(mine[i].IdentityPub, theirs[i].IdentityPub) {
				t.Fatalf("leaf %d and leaf %d disagree about who holds leaf %d",
					group.OwnLeafIndex(), first.OwnLeafIndex(), mine[i].LeafIndex)
			}
		}
	}
}

// addMembersInOneCommit admits n new identities in ONE commit, committed by the view the caller
// names, and joins every one of them out of the single Welcome that commit produced. It hands back
// the joined views and the octets each key package was published as.
//
// The Add proposals are DELIVERED before they are committed, because a commit names its proposals
// by reference and every receiver resolves those references out of its own cache; a cohort that
// skipped the delivery would be asserting about a commit the other members cannot even read.
//
// WHY THE MULTI-JOINER FORM IS THE ONE THAT IS WRITTEN and the single add is a call to it. One
// Welcome per joiner is a message where the pairing between the leaves a commit added and the Add
// proposals it names cannot be got wrong: with one of each, entry 0 of one is entry 0 of the other
// under EVERY permutation. That pairing is one of the four defects this package has shipped -- it
// was checked by comparing two LENGTHS, which errWelcomeAddPairing's own comment says a count
// cannot do -- and the arrangement that can see it is a commit carrying two Adds and a Welcome
// carrying two entries. Making it the general form means the cohort builds it wherever a case asks
// for it rather than only where somebody wrote a second helper.
//
// The key package octets come back because a case that wants to offer the group a key package it
// ALREADY CARRIES has no other way to hold one: testPublishedKeyPackage mints a fresh identity per
// call, so a second call is a different member and not a duplicate of this one.
//
// AND THE COMMIT OPTIONS ARE THE CALLER'S, which is not a convenience either. RFC 9420 section 12.4
// lets an ADD-ONLY commit omit the update path, and this build takes it: CommitPathRequired answers
// false for a list of Adds, so the plan is nil and (*StagedCommit).welcomeMessage hands every joiner
// a GroupSecrets with NO path secret in it. Every entry of such a Welcome then carries the same one
// field, the joiner secret, and which entry a joiner opens cannot be observed at all. MEASURED: with
// the joiner pairing swapped, a two joiner case built on an add-only commit passed. A case about the
// pairing therefore has to ask for the path -- CommitOptions.Force is that ask, and it is what a
// client wanting post compromise security for its committer sends anyway.
func (self *testCohort) addMembersInOneCommit(t *testing.T, committer *Group, opts *CommitOptions,
	names ...string) ([]*Group, [][]byte) {

	t.Helper()
	if len(names) == 0 {
		t.Fatal("a commit adding nobody carries no Welcome, so this helper has nothing to join")
	}
	members := []*testMember{}
	packages := []*KeyPackage{}
	initPrivs := []HpkePrivateKey{}
	encPrivs := []HpkePrivateKey{}
	published := [][]byte{}
	for _, name := range names {
		member := testIdentity(t, self.crypto, name)
		kp, initPriv, encPriv, encoded := testPublishedKeyPackage(t, self.crypto, member)
		proposal, err := committer.ProposeAdd(encoded)
		if err != nil {
			t.Fatalf("ProposeAdd(%s): %v", name, err)
		}
		self.deliver(t, committer, proposal, false)
		members = append(members, member)
		packages = append(packages, kp)
		initPrivs = append(initPrivs, initPriv)
		encPrivs = append(encPrivs, encPriv)
		published = append(published, encoded)
	}
	result, err := committer.CreateCommit(nil, nil, opts)
	if err != nil {
		t.Fatalf("CreateCommit adding %v: %v", names, err)
	}
	if result.Welcome == nil {
		t.Fatalf("the commit adding %v carried no Welcome", names)
	}
	self.deliver(t, committer, result.Commit, true)

	joined := []*Group{}
	for i, member := range members {
		cfg := testGroupConfig(t, self.crypto, member, self.groupId)
		one, err := JoinFromWelcome(cfg, result.Welcome, result.RatchetTree, &JoinKeyMaterial{
			KeyPackage: *packages[i], InitPrivate: initPrivs[i], EncryptPrivate: encPrivs[i],
			SignPrivate: member.SigPriv,
		})
		if err != nil {
			t.Fatalf("JoinFromWelcome(%s) out of a welcome carrying %d entries: %v",
				member.Name, len(names), err)
		}
		self.groups = append(self.groups, one)
		self.members = append(self.members, member)
		self.configs = append(self.configs, cfg)
		joined = append(joined, one)
	}
	return joined, published
}

// addMember is the one-joiner case of the above, kept as its own name because most of this file
// admits one member at a time and `joined[0]` at every one of those sites says nothing.
func (self *testCohort) addMember(t *testing.T, committer *Group, name string) (*Group, []byte) {
	t.Helper()
	joined, published := self.addMembersInOneCommit(t, committer, nil, name)
	return joined[0], published[0]
}

// commitFrom builds a commit at one view, delivers it to every other and merges the committer's.
func (self *testCohort) commitFrom(t *testing.T, from int) *CommitResult {
	t.Helper()
	author := self.groups[from]
	result, err := author.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit at leaf %d: %v", author.OwnLeafIndex(), err)
	}
	self.deliver(t, author, result.Commit, true)
	return result
}

// settle commits once from every member, in cohort order, so that every parent node of the tree
// carries a key somebody holds the private half of.
func (self *testCohort) settle(t *testing.T) {
	t.Helper()
	for from := range self.groups {
		self.commitFrom(t, from)
	}
}

// exchange has one member seal an application message and requires EVERY other member to open it.
//
// The AAD and the sender leaf are asserted beside the plaintext, because a receive path that opened
// the ciphertext under the wrong sender's ratchet, or that dropped the authenticated data on the
// way through the framing layer, answers the right plaintext and is wrong about the message.
func (self *testCohort) exchange(t *testing.T, from int, plaintext string) {
	t.Helper()
	author := self.groups[from]
	aad := []byte("round trip aad")
	sealed, err := author.Protect(aad, []byte(plaintext))
	if err != nil {
		t.Fatalf("Protect at leaf %d: %v", author.OwnLeafIndex(), err)
	}
	opened := 0
	for at, group := range self.groups {
		if at == from {
			continue
		}
		application, err := group.Unprotect(sealed)
		if err != nil {
			t.Fatalf("leaf %d could not open the message of leaf %d at epoch %d: %v",
				group.OwnLeafIndex(), author.OwnLeafIndex(), group.Epoch(), err)
		}
		if string(application.Plaintext) != plaintext {
			t.Fatalf("leaf %d opened %q from leaf %d, want %q",
				group.OwnLeafIndex(), application.Plaintext, author.OwnLeafIndex(), plaintext)
		}
		if !bytes.Equal(application.AuthenticatedData, aad) {
			t.Fatalf("leaf %d read the authenticated data as %q, want %q",
				group.OwnLeafIndex(), application.AuthenticatedData, aad)
		}
		if application.SenderLeaf != author.OwnLeafIndex() {
			t.Fatalf("leaf %d read the message as coming from leaf %d, want %d",
				group.OwnLeafIndex(), application.SenderLeaf, author.OwnLeafIndex())
		}
		opened += 1
	}
	if opened != len(self.groups)-1 {
		t.Fatalf("%d of %d peers opened the message of leaf %d",
			opened, len(self.groups)-1, author.OwnLeafIndex())
	}
}

// restore reloads one member out of the state it has persisted and PUTS THE RESTORED VIEW IN THE
// LIVE ONE'S PLACE.
//
// The live view is closed and replaced rather than kept beside the restored one, and that is the
// whole point of doing it here: both hold the same leaf, so two views sealing under one leaf's
// ratchet is the nonce reuse the restore path exists to prevent, and a case that kept both would be
// observing whichever of the two it happened to send from. Everything the cohort does afterwards
// goes through the restored view.
func (self *testCohort) restore(t *testing.T, at int) *Group {
	t.Helper()
	live := self.groups[at]
	leaf, epoch := live.OwnLeafIndex(), live.Epoch()
	restored, err := LoadGroup(self.configs[at], epoch, self.members[at].SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at leaf %d, epoch %d: %v", leaf, epoch, err)
	}
	if restored.OwnLeafIndex() != leaf {
		t.Fatalf("the restored view came back at leaf %d, want %d", restored.OwnLeafIndex(), leaf)
	}
	if restored.Epoch() != epoch {
		t.Fatalf("the restored view of leaf %d came back at epoch %d, want %d",
			leaf, restored.Epoch(), epoch)
	}
	if !bytes.Equal(restored.EpochAuthenticator(), live.EpochAuthenticator()) {
		t.Fatalf("the restored view of leaf %d answers a different epoch authenticator from the live one, so it is not the same epoch of the same group",
			leaf)
	}
	live.Close()
	self.groups[at] = restored
	return restored
}

// testExtensionsWith is the group's own published extension list with one entry replaced.
//
// A group_context_extensions proposal REPLACES the list wholesale (RFC 9420 section 12.1.6), so a
// caller that passes only the entry it changed drops every other extension the group carries -- for
// a group of this profile that is required_capabilities, and dropping it is a different change from
// the one a retention policy case means to make.
func testExtensionsWith(t *testing.T, group *Group, replacement Extension) []Extension {
	t.Helper()
	context := testGroupContextOf(t, group)
	out := make([]Extension, 0, len(context.Extensions)+1)
	replaced := false
	for _, extension := range context.Extensions {
		if extension.ExtensionType == replacement.ExtensionType {
			out = append(out, replacement)
			replaced = true
			continue
		}
		out = append(out, extension)
	}
	if !replaced {
		out = append(out, replacement)
	}
	return out
}

// ---------------------------------------------------------------------------
// the round trips
// ---------------------------------------------------------------------------

// TestGroupLifecycleFullCycle is this plan's gate: one group, four members, and every operation the
// profile has, each one delivered to every other member and each one followed by an agreement.
func TestGroupLifecycleFullCycle(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	cfg := testGroupConfig(t, crypto, owner, "roundtrip")
	root, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	cohort := &testCohort{
		crypto:  crypto,
		groupId: "roundtrip",
		groups:  []*Group{root},
		members: []*testMember{owner},
		configs: []*GroupConfig{cfg},
	}
	defer cohort.closeAll()

	cohort.addMember(t, root, "bob")
	cohort.assertLockstep(t)
	cohort.addMember(t, root, "carol")
	cohort.assertLockstep(t)
	cohort.addMember(t, root, "dave")
	cohort.assertLockstep(t)
	if len(root.Members()) != 4 {
		t.Fatalf("Members = %d, want 4", len(root.Members()))
	}

	// a member updates, and SOMEBODY ELSE commits it by reference. The committer never holds the
	// leaf key the update publishes, so a commit path that read the proposer's leaf out of its own
	// tree rather than out of the proposal would produce an epoch only the committer can enter.
	bob := cohort.groups[1]
	update, err := bob.ProposeUpdate()
	if err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	cohort.deliver(t, bob, update, false)
	cohort.commitFrom(t, 0)
	cohort.assertLockstep(t)

	// application traffic flows in both directions at the new epoch
	cohort.exchange(t, 1, "after the update")
	cohort.exchange(t, 3, "and back the other way")

	// a GroupContextExtensions commit changes the retention policy
	policy, err := root.GroupPolicy()
	if err != nil {
		t.Fatalf("GroupPolicy: %v", err)
	}
	policy.RetentionPolicy = RetentionPolicy{DurableMs: 31536000000, MediaMs: 2592000000}
	ext, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode policy: %v", err)
	}
	gce, err := root.ProposeGroupContextExtensions(testExtensionsWith(t, root, ext))
	if err != nil {
		t.Fatalf("ProposeGroupContextExtensions: %v", err)
	}
	cohort.deliver(t, root, gce, false)
	cohort.commitFrom(t, 0)
	cohort.assertLockstep(t)
	for at, group := range cohort.groups {
		updated, err := group.GroupPolicy()
		if err != nil {
			t.Fatalf("GroupPolicy at leaf %d: %v", group.OwnLeafIndex(), err)
		}
		if updated.RetentionPolicy.DurableMs != 31536000000 ||
			updated.RetentionPolicy.MediaMs != 2592000000 {
			t.Fatalf("the GCE commit did not reach cohort entry %d: retention is %+v",
				at, updated.RetentionPolicy)
		}
	}

	// a remove: the removed member sees ErrRemovedFromGroup, the rest stay in lockstep
	dave := cohort.groups[3]
	daveLeaf := dave.OwnLeafIndex()
	removal, err := root.ProposeRemove(daveLeaf)
	if err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	cohort.deliver(t, root, removal, false)
	result, err := root.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit with remove: %v", err)
	}
	for _, group := range cohort.groups[1:] {
		processed, err := group.ProcessMessage(result.Commit)
		if err != nil {
			t.Fatalf("leaf %d could not process the removal commit: %v", group.OwnLeafIndex(), err)
		}
		err = group.ApplyCommit(processed)
		if group == dave {
			if !errors.Is(err, ErrRemovedFromGroup) {
				t.Fatalf("removed member error = %v, want ErrRemovedFromGroup", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("leaf %d could not apply the removal commit: %v", group.OwnLeafIndex(), err)
		}
	}
	if err := root.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	cohort.groups = cohort.groups[:3]
	cohort.members = cohort.members[:3]
	cohort.configs = cohort.configs[:3]
	cohort.assertLockstep(t)
	if len(root.Members()) != 3 {
		t.Fatalf("Members after removal = %d, want 3", len(root.Members()))
	}
	for _, group := range cohort.groups {
		if _, found := group.MemberAt(daveLeaf); found {
			t.Fatalf("leaf %d still holds a member at the removed leaf %d",
				group.OwnLeafIndex(), daveLeaf)
		}
	}
	// the three that remain still talk to each other at the epoch the removal opened
	cohort.exchange(t, 2, "after the removal")

	// and the removed member can no longer read or write the new epoch. THE SENTINEL AND NOT
	// MERELY AN ERROR: a closed group holds a nil secret tree, so a Protect that had lost its own
	// refusal would still fail somewhere below and satisfy an assertion written as "err != nil".
	if _, err := dave.Protect(nil, []byte("still here?")); !errors.Is(err, errGroupClosed) {
		t.Fatalf("the removed member's Protect = %v, want errGroupClosed", err)
	}
}

// TestJoinAtAnAdvancedEpoch is the Welcome at a history the joiner was never sent.
func TestJoinAtAnAdvancedEpoch(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "advanced", 1)
	defer cohort.closeAll()
	root := cohort.groups[0]

	for i := 0; i < 5; i += 1 {
		cohort.commitFrom(t, 0)
	}
	if root.Epoch() != 5 {
		t.Fatalf("Epoch = %d, want 5", root.Epoch())
	}
	late, _ := cohort.addMember(t, root, "late")
	if late.Epoch() != root.Epoch() {
		t.Fatalf("late joiner epoch = %d, committer = %d", late.Epoch(), root.Epoch())
	}
	cohort.assertLockstep(t)
	// and the epoch it joined at is one it can use, in both directions
	cohort.exchange(t, 1, "the late joiner speaks")
	cohort.commitFrom(t, 1)
	cohort.assertLockstep(t)
}

// TestLosingCommitterRederivesAgainstTheWinner is MASTER section 9.3: the delivery service accepts
// one commit per (group, epoch), so the loser clears its pending commit, applies the winner's, and
// commits again at the epoch that opened.
func TestLosingCommitterRederivesAgainstTheWinner(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	winner, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("winner CreateCommit: %v", err)
	}
	if _, err := receiver.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("loser CreateCommit: %v", err)
	}
	// the loser learns the server accepted the other commit
	receiver.ClearPendingCommit()
	processed, err := receiver.ProcessMessage(winner.Commit)
	if err != nil {
		t.Fatalf("loser ProcessMessage: %v", err)
	}
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("loser ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("winner MergePendingCommit: %v", err)
	}
	if receiver.Epoch() != committer.Epoch() {
		t.Fatalf("the loser is at epoch %d and the winner at %d", receiver.Epoch(), committer.Epoch())
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the loser did not converge on the winner's epoch")
	}
	// and can commit again at the new epoch, which the winner then follows
	second, err := receiver.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("loser re-commit: %v", err)
	}
	staged, err := committer.ProcessMessage(second.Commit)
	if err != nil {
		t.Fatalf("the winner could not process the loser's next commit: %v", err)
	}
	if err := committer.ApplyCommit(staged); err != nil {
		t.Fatalf("the winner could not apply the loser's next commit: %v", err)
	}
	if err := receiver.MergePendingCommit(); err != nil {
		t.Fatalf("loser MergePendingCommit: %v", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two did not agree on the epoch the loser's next commit opened")
	}
}

// TestProfileRefusalsEndToEnd is the v1 profile's three refusals, taken at the doors a message
// actually arrives through rather than at the gate alone.
//
// The codec decodes all seven proposal arms so the messages vector family can round trip them; what
// this plan's profile is, is the refusal of the three v1 does not run.
func TestProfileRefusalsEndToEnd(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	psk := &Proposal{ProposalType: ProposalTypePreSharedKey, PreSharedKey: &PreSharedKey{}}
	if err := checkProposalProfile(defaultProfile(), psk); !errors.Is(err, errProfilePsk) {
		t.Fatalf("psk gate = %v, want errProfilePsk", err)
	}
	externalInit := &Proposal{ProposalType: ProposalTypeExternalInit, ExternalInit: &ExternalInit{}}
	if err := checkProposalProfile(defaultProfile(), externalInit); !errors.Is(err, errProfileExternalCommit) {
		t.Fatalf("external_init gate = %v, want errProfileExternalCommit", err)
	}
	// and a psk proposal that arrives on the wire never reaches the cache
	content := testProposalContentAt(t, committer.OwnLeafIndex(), committer.GroupId(),
		committer.Epoch(), psk)
	if _, err := committer.proposals.Store(crypto, committer.context, content); !errors.Is(err, errProfilePsk) {
		t.Fatalf("psk cache Store = %v, want errProfilePsk", err)
	}
	// a group context extension outside the allowed set is refused before it commits
	bad := []Extension{{ExtensionType: ExtensionTypeExternalSenders, ExtensionData: []byte{}}}
	if _, err := committer.ProposeGroupContextExtensions(bad); !errors.Is(err, errProfileExternalSender) {
		t.Fatalf("external_senders = %v, want errProfileExternalSender", err)
	}
}

// TestEveryMemberCommitsInTurnAndEveryOtherMemberFollows is the cross-member half of this file, run
// at three sizes.
//
// EVERY MEMBER COMMITS, IN TURN, AND EVERY OTHER MEMBER PROCESSES IT. A fixture where one member is
// always the committer exercises one sender's filtered direct path and one receiver's entry into
// it, and the entry point is a function of the PAIR: at four members eight of the twelve ordered
// pairs enter above the receiver's own leaf and four do not, so a single committer leaves most of
// the pairs unvisited.
//
// The sizes are 4, 5 and 6 because the property is the member's position. At five, leaf 4 stands
// alone under the right subtree and enters every sender's commit at its own leaf, and at six it no
// longer does -- so a defect that only shows for a member with a copath node above its own leaf is
// present for four of five members at one size and five of six at the next.
func TestEveryMemberCommitsInTurnAndEveryOtherMemberFollows(t *testing.T) {
	crypto := testCrypto(t)
	for _, size := range []int{4, 5, 6} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			cohort := testNewCohort(t, crypto, fmt.Sprintf("in-turn-%d", size), size)
			defer cohort.closeAll()
			cohort.assertLockstep(t)
			// the adds alone: one commit per admission, so the group stands at size-1
			if cohort.groups[0].Epoch() != uint64(size-1) {
				t.Fatalf("a cohort of %d built out of adds stands at epoch %d, want %d",
					size, cohort.groups[0].Epoch(), size-1)
			}
			for from := range cohort.groups {
				before := cohort.groups[0].Epoch()
				cohort.commitFrom(t, from)
				if cohort.groups[0].Epoch() != before+1 {
					t.Fatalf("the commit of leaf %d moved the epoch from %d to %d",
						cohort.groups[from].OwnLeafIndex(), before, cohort.groups[0].Epoch())
				}
				cohort.assertLockstep(t)
				cohort.exchange(t, from, fmt.Sprintf("epoch %d, from leaf %d",
					cohort.groups[0].Epoch(), cohort.groups[from].OwnLeafIndex()))
			}
			// every view of this cohort holds a distinct leaf, and together they hold 0..size-1
			seen := map[LeafIndex]bool{}
			for _, group := range cohort.groups {
				if seen[group.OwnLeafIndex()] {
					t.Fatalf("two views of this cohort hold leaf %d", group.OwnLeafIndex())
				}
				seen[group.OwnLeafIndex()] = true
			}
			for i := 0; i < size; i += 1 {
				if !seen[LeafIndex(i)] {
					t.Fatalf("no view of this cohort holds leaf %d", i)
				}
			}
			// and the MEMBERSHIP names those same leaves. assertLockstep compares the views
			// against each other, so a build that reported every member at leaf 0 would agree
			// with itself everywhere; this is the absolute statement an agreement cannot make.
			members := cohort.groups[0].Members()
			if len(members) != size {
				t.Fatalf("Members = %d, want %d", len(members), size)
			}
			for i, member := range members {
				if member.LeafIndex != LeafIndex(i) {
					t.Fatalf("Members()[%d] is at leaf %d, want %d", i, member.LeafIndex, i)
				}
			}
		})
	}
}

// TestAMemberThatJoinedMidHistoryFollowsEveryOtherMember is the join at an epoch with a history
// behind it, followed by a commit from each of the members that predate it.
//
// The Welcome is committed by leaf 2 RATHER THAN BY THE GROUP'S FOUNDER, which is the arrangement a
// fixture that always commits from leaf 0 never builds: the joiner's own committer is then not the
// member whose leaf its Welcome's path secret climbs from, and the far side of the tree is a member
// that took no part in admitting it.
func TestAMemberThatJoinedMidHistoryFollowsEveryOtherMember(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "mid-history", 4)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)
	cohort.exchange(t, 1, "history the joiner will never see")
	behind := cohort.groups[0].Epoch()

	late, _ := cohort.addMember(t, cohort.groups[2], "late")
	cohort.assertLockstep(t)
	if late.Epoch() != behind+1 {
		t.Fatalf("the late joiner came up at epoch %d, want %d", late.Epoch(), behind+1)
	}
	if len(late.Members()) != 5 {
		t.Fatalf("the late joiner holds %d members, want 5", len(late.Members()))
	}

	// every member that predates the joiner commits, and the joiner follows each one -- leaf 0
	// first, which is the far side of the tree from the leaf the joiner was placed at.
	for from := 0; from < len(cohort.groups)-1; from += 1 {
		cohort.commitFrom(t, from)
		cohort.assertLockstep(t)
		cohort.exchange(t, from, fmt.Sprintf("leaf %d speaks to the joiner",
			cohort.groups[from].OwnLeafIndex()))
	}
	// and the joiner commits, and every member that predates it follows
	joiner := len(cohort.groups) - 1
	cohort.commitFrom(t, joiner)
	cohort.assertLockstep(t)
	cohort.exchange(t, joiner, "the joiner speaks back")
}

// TestARemovedMemberLeavesTheRestInLockstep removes a MIDDLE leaf, committed by that leaf's own
// sibling.
//
// The sibling is the committer because a remove blanks the removed leaf's direct path, which is the
// committer's own path from the parent they share upward -- so the committer is re-keying nodes the
// same commit has just blanked, and it is the arrangement in which a blank-then-fill ordering defect
// is reachable at all.
func TestARemovedMemberLeavesTheRestInLockstep(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "removal", 5)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)

	victim := cohort.groups[2]
	victimLeaf := victim.OwnLeafIndex()
	committer := cohort.groups[3]
	removal, err := committer.ProposeRemove(victimLeaf)
	if err != nil {
		t.Fatalf("ProposeRemove(%d): %v", victimLeaf, err)
	}
	cohort.deliver(t, committer, removal, false)
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit with the remove: %v", err)
	}
	for _, group := range cohort.groups {
		if group == committer {
			continue
		}
		processed, err := group.ProcessMessage(result.Commit)
		if err != nil {
			t.Fatalf("leaf %d could not process the removal commit: %v", group.OwnLeafIndex(), err)
		}
		err = group.ApplyCommit(processed)
		if group == victim {
			if !errors.Is(err, ErrRemovedFromGroup) {
				t.Fatalf("the removed member's ApplyCommit = %v, want ErrRemovedFromGroup", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("leaf %d could not apply the removal commit: %v", group.OwnLeafIndex(), err)
		}
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit at the committer: %v", err)
	}
	cohort.groups = slices.Delete(cohort.groups, 2, 3)
	cohort.members = slices.Delete(cohort.members, 2, 3)
	cohort.configs = slices.Delete(cohort.configs, 2, 3)
	cohort.assertLockstep(t)

	for _, group := range cohort.groups {
		if len(group.Members()) != 4 {
			t.Fatalf("leaf %d holds %d members after the removal, want 4",
				group.OwnLeafIndex(), len(group.Members()))
		}
		if _, found := group.MemberAt(victimLeaf); found {
			t.Fatalf("leaf %d still holds a member at the removed leaf %d",
				group.OwnLeafIndex(), victimLeaf)
		}
	}
	if _, err := victim.Protect(nil, []byte("still here?")); !errors.Is(err, errGroupClosed) {
		t.Fatalf("the removed member's Protect = %v, want errGroupClosed", err)
	}
	// the four that remain go on, from a member on each side of the leaf that left
	cohort.exchange(t, 0, "after the removal, from the left")
	cohort.exchange(t, 2, "after the removal, from the right")
	cohort.commitFrom(t, 2)
	cohort.assertLockstep(t)
	cohort.commitFrom(t, 0)
	cohort.assertLockstep(t)
}

// TestARemovalBlanksTheRemovedLeafsPathTheCommitDoesNotCover is section 12.1.3's blanking, held
// where it is observable.
//
// WHY IT NEEDS ITS OWN CASE, measured rather than argued. Deleting the BlankDirectPath call from
// (*RatchetTree).RemoveLeaf leaves every assertion of every other case in this file green: the
// remaining members all agree about the stale node, so no agreement can see it. And it survived an
// assertion written into TestARemovedMemberLeavesTheRestInLockstep too, for a second reason -- that
// case removes a leaf and commits from its SIBLING, and applying an update path blanks the
// committer's own direct path, which above two siblings is the very same set of nodes. So the
// removal's own blanking is unobservable there whatever assertion is written beside it.
//
// SO THE COMMITTER HERE IS ON THE OTHER SIDE OF THE REMOVED LEAF'S PARENT. The nodes below
// CommonAncestor(removed, committer) are exactly the ones the commit's own path does not touch, and
// they are the ones section 12.1.3 has to blank: each carries a key the removed member holds, so a
// node left standing is one it can go on deriving every path secret sealed to.
//
// THE CLASS IS DERIVED AND THE VACUITY IS GUARDED. The set is the removed leaf's direct path minus
// the committer's, taken off the tree math rather than written down after drawing the tree, and the
// case requires that set to be NON-EMPTY and non-blank BEFORE the removal -- a tree that had blanked
// those nodes already would satisfy the after-assertion by having nothing left in it to look at.
//
// AND WHAT THE FINAL LOOP COULD NOT BE MADE TO FAIL ON ITS OWN, recorded rather than left implied.
// Deleting BlankDirectPath from RemoveLeaf does fail this case -- but in the RECEIVE path, on the
// parent hash, before the loop is reached: the stale node's own parent is re-keyed by the same
// commit, so the leaf that used to claim the stale node no longer chains to it and section 7.9.2
// reports it as claimed by none of its descendants. That is structural and not a gap in the
// mutation. Every node this loop is about sits BELOW CommonAncestor(removed, committer), and that
// ancestor is on the committer's path and therefore always re-keyed, so a node left standing here
// always breaks the parent hash. The loop is a SECOND statement of a rule the parent hash already
// carries -- kept because it says the rule at the level the rule is about, and because it is what
// would report a build whose parent hash validation had been relaxed -- and it is not an
// independent observation of the blanking.
func TestARemovalBlanksTheRemovedLeafsPathTheCommitDoesNotCover(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "removal-blanking", 4)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)

	victim := cohort.groups[3]
	victimLeaf := victim.OwnLeafIndex()
	committer := cohort.groups[0]
	committerLeaf := committer.OwnLeafIndex()
	survivor := cohort.groups[1]

	// the class, derived off the tree BEFORE anything is removed
	width := survivor.tree.LeafWidth()
	victimPath, err := directPathOf(victimLeaf.NodeIndex(), width)
	if err != nil {
		t.Fatalf("direct path of leaf %d: %v", victimLeaf, err)
	}
	committerPath, err := directPathOf(committerLeaf.NodeIndex(), width)
	if err != nil {
		t.Fatalf("direct path of leaf %d: %v", committerLeaf, err)
	}
	covered := map[NodeIndex]bool{}
	for _, x := range committerPath {
		covered[x] = true
	}
	owed := []NodeIndex{}
	for _, x := range victimPath {
		if covered[x] {
			continue
		}
		owed = append(owed, x)
	}
	if len(owed) == 0 {
		t.Fatalf("every node above leaf %d is also above leaf %d, so the commit's own path would blank them and the removal's blanking is unobservable here",
			victimLeaf, committerLeaf)
	}
	for _, x := range owed {
		if survivor.tree.ParentAt(x) == nil {
			t.Fatalf("node %d is already blank before the removal, so requiring it blank afterwards observes nothing",
				x)
		}
	}
	t.Logf("removing leaf %d at a commit from leaf %d owes the blanking of %v",
		victimLeaf, committerLeaf, owed)

	removal, err := committer.ProposeRemove(victimLeaf)
	if err != nil {
		t.Fatalf("ProposeRemove(%d): %v", victimLeaf, err)
	}
	cohort.deliver(t, committer, removal, false)
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit with the remove: %v", err)
	}
	for _, group := range cohort.groups {
		if group == committer {
			continue
		}
		processed, err := group.ProcessMessage(result.Commit)
		if err != nil {
			t.Fatalf("leaf %d could not process the removal commit: %v", group.OwnLeafIndex(), err)
		}
		err = group.ApplyCommit(processed)
		if group == victim {
			if !errors.Is(err, ErrRemovedFromGroup) {
				t.Fatalf("the removed member's ApplyCommit = %v, want ErrRemovedFromGroup", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("leaf %d could not apply the removal commit: %v", group.OwnLeafIndex(), err)
		}
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit at the committer: %v", err)
	}
	cohort.groups = cohort.groups[:3]
	cohort.members = cohort.members[:3]
	cohort.configs = cohort.configs[:3]
	cohort.assertLockstep(t)

	// EVERY view, and not only the one the class was derived from: a member that kept the stale
	// node is a member that seals its next commit to a key the removed member holds, and which of
	// them does that is not something the derivation can say in advance.
	for _, group := range cohort.groups {
		for _, x := range owed {
			if group.tree.ParentAt(x) != nil {
				t.Fatalf("leaf %d still carries node %d, which stands above the removed leaf %d and which this commit's own path never touched: the removed member goes on deriving every path secret sealed to it",
					group.OwnLeafIndex(), x, victimLeaf)
			}
		}
	}
	// and the group goes on from the epoch the removal opened
	cohort.exchange(t, 0, "after the blanking")
	cohort.commitFrom(t, 2)
	cohort.assertLockstep(t)
}

// TestEveryMemberSendsAndReceivesAcrossARestore persists and restores each member in turn and
// requires the restored view to do both halves of being a member.
//
// SENDING IS THE HALF A RESTORE LOSES QUIETLY. A restored member whose sender ratchet restarted at
// generation 0 draws a generation its peers have already consumed: they answer
// ErrRatchetGenerationConsumed and drop the message, and two plaintexts of one epoch have gone out
// under one (key, base nonce) pair for that leaf. So each member SENDS before the restore and sends
// again after it, to a cohort whose receiving heads for that leaf have already moved.
//
// RECEIVING IS THE OTHER HALF, and the commit that follows each restore comes from a DIFFERENT
// member -- so the restored view has to open a path secret sealed to a node above its own leaf out
// of a ladder it rebuilt from storage rather than out of the commit that first gave it to it.
func TestEveryMemberSendsAndReceivesAcrossARestore(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "restore-round-trip", 4)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)

	for at := range cohort.groups {
		leaf := cohort.groups[at].OwnLeafIndex()
		cohort.exchange(t, at, fmt.Sprintf("leaf %d before the restore", leaf))
		cohort.restore(t, at)
		cohort.assertLockstep(t)
		cohort.exchange(t, at, fmt.Sprintf("leaf %d after the restore", leaf))
		// and the restored view follows a commit from somebody else
		cohort.commitFrom(t, (at+1)%len(cohort.groups))
		cohort.assertLockstep(t)
		cohort.exchange(t, at, fmt.Sprintf("leaf %d after the next epoch", leaf))
	}
}

// testUnmergedCopathReach answers the copath node through which a commit from sender reaches
// target's leaf AS AN UNMERGED LEAF.
//
// The node it looks for is NOT BLANK -- so RFC 9420 section 7.4's first rule would make its
// resolution itself alone -- and carries target on its unmerged list anyway, which is the second
// rule and the only way a resolution reaches a leaf the node's own key does not cover.
//
// DERIVED OFF THE TREE AND NOT NAMED. A case that wrote the node index down would go on passing over
// a build that stopped producing an unmerged leaf there, which is exactly the failure mode
// four_member_group_test.go records for the fixtures that have none.
func testUnmergedCopathReach(t *testing.T, tree *RatchetTree, sender LeafIndex,
	target LeafIndex) (NodeIndex, bool) {

	t.Helper()
	steps, err := tree.filteredPathSteps(sender)
	if err != nil {
		t.Fatalf("filtered path of leaf %d: %v", sender, err)
	}
	for _, step := range steps {
		if tree.ParentAt(step.CopathChild) == nil {
			continue
		}
		if !slices.Contains(tree.UnmergedLeaves(step.CopathChild), target) {
			continue
		}
		resolution := tree.Resolution(step.CopathChild)
		if !slices.Contains(resolution, target.NodeIndex()) {
			t.Fatalf("node %d lists leaf %d unmerged and its resolution %v does not reach it",
				step.CopathChild, target, resolution)
		}
		if len(resolution) < 2 || resolution[0] != step.CopathChild {
			t.Fatalf("node %d is not blank and resolves to %v, which is not itself followed by its unmerged leaves",
				step.CopathChild, resolution)
		}
		return step.CopathChild, true
	}
	return 0, false
}

// TestACommitReachesTheNewestMemberThroughAnUnmergedLeaf builds the arrangement no fixture in this
// package has: a member a commit reaches through a NON-BLANK copath node's unmerged list.
//
// WHY SEVEN AND THEN AN EIGHTH. An Add lists the new leaf as unmerged at every non-blank node above
// it, and in every other fixture here there are none -- a group assembled out of adds has all those
// parents blank, and one that has settled has had them merged. The node above a new leaf is
// non-blank only when somebody committed through it WHILE the leaf slot was empty, and a filtered
// direct path drops any node whose copath child resolves to nothing, so the empty slot's own parent
// is never it. The nearest node that can be is the GRANDparent, which needs the sibling subtree
// occupied: seven members leaves leaf 7 empty with leaves 4 and 6 in place, node 13 blank because
// leaf 6's filtered path drops it, and node 11 filled because leaf 4's does not.
//
// THE SHAPE IS ASSERTED BEFORE ANYTHING IS SENT. Without that this case is satisfied by a tree with
// no unmerged leaf in it at all -- it would simply be an eight member round trip, green, and
// reported as covering the resolution rule it no longer reaches.
func TestACommitReachesTheNewestMemberThroughAnUnmergedLeaf(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "unmerged", 7)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)

	newest, _ := cohort.addMember(t, cohort.groups[0], "newest")
	cohort.assertLockstep(t)

	sender := cohort.groups[0]
	through, held := testUnmergedCopathReach(t, sender.tree, sender.OwnLeafIndex(),
		newest.OwnLeafIndex())
	if !held {
		t.Fatalf("no copath node of leaf %d reaches leaf %d through an unmerged list, so this case is an ordinary round trip and observes nothing about section 7.4's second rule",
			sender.OwnLeafIndex(), newest.OwnLeafIndex())
	}
	t.Logf("leaf %d reaches leaf %d through the unmerged list of node %d, whose resolution is %v",
		sender.OwnLeafIndex(), newest.OwnLeafIndex(), through, sender.tree.Resolution(through))

	// the commit that has to pair its ciphertexts with that resolution. The newest member is not
	// the FIRST entry of it, so a pairing that walked the resolution wrongly hands it the
	// ciphertext sealed to the node itself -- which every other member of that subtree opens.
	cohort.commitFrom(t, 0)
	cohort.assertLockstep(t)
	cohort.exchange(t, len(cohort.groups)-1, "the unmerged member speaks")

	// AND THE ENTRY IS STILL THERE, which is the half that says leaf 0's commit did not merge it.
	// An update path re-keys the COMMITTER'S filtered direct path, and the node this case is about
	// is on leaf 0's COPATH -- so a build that cleared an unmerged list it had only encrypted to
	// would leave every later resolution unable to reach the leaf it dropped.
	if !slices.Contains(sender.tree.UnmergedLeaves(through), newest.OwnLeafIndex()) {
		t.Fatalf("node %d stopped listing leaf %d unmerged after a commit that only sealed to it",
			through, newest.OwnLeafIndex())
	}

	// the merge is the newest member's own commit: that node IS on its own filtered direct path, so
	// this is the commit that re-keys the node and takes the leaf off its unmerged list.
	cohort.commitFrom(t, len(cohort.groups)-1)
	cohort.assertLockstep(t)
	if remaining := sender.tree.UnmergedLeaves(through); len(remaining) != 0 {
		t.Fatalf("node %d still lists %v unmerged after a commit from the leaf it listed",
			through, remaining)
	}
	// and the group goes on from there, with the tree it merged into
	cohort.exchange(t, 0, "after the merge")
	cohort.commitFrom(t, 3)
	cohort.assertLockstep(t)
}

// ---------------------------------------------------------------------------
// the shipped defects this file could not see, each in the arrangement that sees it
// ---------------------------------------------------------------------------

// TestTheGeneratorRefusesAnAddThisGroupsOwnListRuleRefuses is the FIRST of the four defects this
// package has shipped, reached from the cohort for the first time.
//
// THE DEFECT was (*Group).ProposeAdd caching an Add that (*Group).ValidateProposalList refuses.
// What makes it a cross-member fault rather than a generator one is what happens next: the entry
// sits in this member's cache, its own next commit names it by reference, and every OTHER member
// runs section 12.2 over the resolved list and refuses the commit. The proposer is then a client
// whose every commit is rejected by a group that is behaving correctly, and nothing at the point of
// the mistake says so.
//
// WHY THIS FILE COULD NOT SEE IT UNTIL NOW, measured: every add the cohort makes is a FRESH valid
// identity, so no arrangement it built could offer the group a key package any list rule refuses.
// The header of this file nonetheless named the cached Add among the defects it is written for.
// This is the arrangement that reaches it -- a key package the group ALREADY CARRIES, which is
// ValSem101 and ValSem103 at once and is the shape a client re-fetching a stale directory entry
// produces in the field.
//
// AND THE SECOND HALF IS THE CACHE. A refusal that had happened after the entry was stored would
// satisfy an assertion written as "ProposeAdd returned an error", so the cohort commits afterwards
// and requires every member to follow: a poisoned cache is a commit nobody accepts, and that is a
// statement only n views can make.
func TestTheGeneratorRefusesAnAddThisGroupsOwnListRuleRefuses(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "cached-add", 4)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)

	// a member admitted through the cohort's own door, and the octets its key package was
	// published as. The group now carries that leaf's signature key and its encryption key.
	admitted, published := cohort.addMember(t, cohort.groups[0], "already here")
	cohort.assertLockstep(t)
	if _, found := cohort.groups[0].MemberAt(admitted.OwnLeafIndex()); !found {
		t.Fatalf("the group holds no member at leaf %d, so the key package below is not one it carries",
			admitted.OwnLeafIndex())
	}

	// offered back to the group that already carries it, at a DIFFERENT member from the one that
	// admitted it -- so what refuses is a rule about the tree and not a memory of the call.
	proposer := cohort.groups[2]
	if _, err := proposer.ProposeAdd(published); !errors.Is(err, ErrAddDuplicateSignatureKey) {
		t.Fatalf("ProposeAdd of a key package this group already carries = %v, want ErrAddDuplicateSignatureKey; an Add this group's own ValidateProposalList refuses is one every OTHER member refuses the committing member's next commit for",
			err)
	}

	// and the cache is clean, which is the half the refusal alone does not say: the proposer's
	// next commit is one every member follows.
	cohort.commitFrom(t, 2)
	cohort.assertLockstep(t)
	cohort.exchange(t, 2, "the proposer still commits for this group")
}

// TestAStagedCommitFromAnotherGroupOrAnotherEpochInstallsNothingHere is the SECOND of the four,
// and it needs a thing this file did not have: a second group.
//
// THE DEFECT was (*Group).ApplyCommit accepting a staged commit that ANOTHER GROUP derived, and
// deriving byte identical authenticators from it. A member that entered a stranger's epoch is a
// member whose every later message is sealed under keys the group it thinks it is in has never
// heard of, and whose epoch authenticator agrees with the stranger rather than with its peers.
//
// WHY THIS FILE COULD NOT SEE IT UNTIL NOW, measured: every case here drove ONE cohort, and a
// staged commit is only ever handed to the group whose ProcessMessage produced it, so no
// arrangement it built could offer a group a commit that came from anywhere else.
//
// BOTH HALVES ARE RUN AND NEITHER ALONE IS THE RULE, which is (*Group).ApplyCommit's own comment:
// every group this client is in runs an epoch 7, so the epoch is not an identity; and the group id
// alone would admit a staged epoch of this group that some other epoch of it derived.
//
// AND BOTH HALVES CARRY THEIR CONTROL. A build that refused every staged commit would satisfy the
// two refusals perfectly, so each refused value is afterwards installed WHERE IT BELONGS and the
// cohort it belongs to is required to converge on it. That is what says the refusal is about
// provenance rather than about the commit.
func TestAStagedCommitFromAnotherGroupOrAnotherEpochInstallsNothingHere(t *testing.T) {
	crypto := testCrypto(t)
	here := testNewCohort(t, crypto, "provenance-here", 4)
	defer here.closeAll()
	here.settle(t)
	here.assertLockstep(t)

	elsewhere := testNewCohort(t, crypto, "provenance-elsewhere", 4)
	defer elsewhere.closeAll()
	elsewhere.settle(t)
	elsewhere.assertLockstep(t)

	// a perfectly good commit of the OTHER group, staged by a member of the other group
	foreign, err := elsewhere.groups[0].CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit in the other group: %v", err)
	}
	foreignStaged, err := elsewhere.groups[1].ProcessMessage(foreign.Commit)
	if err != nil {
		t.Fatalf("the other group could not process its own commit: %v", err)
	}

	subject := here.groups[1]
	beforeEpoch := subject.Epoch()
	beforeAuthenticator := bytes.Clone(subject.EpochAuthenticator())
	if err := subject.ApplyCommit(foreignStaged); !errors.Is(err, errApplyCommitNotThisGroups) {
		t.Fatalf("ApplyCommit of another group's staged commit = %v, want errApplyCommitNotThisGroups",
			err)
	}
	if subject.Epoch() != beforeEpoch {
		t.Fatalf("the refused apply moved leaf %d from epoch %d to %d",
			subject.OwnLeafIndex(), beforeEpoch, subject.Epoch())
	}
	if !bytes.Equal(subject.EpochAuthenticator(), beforeAuthenticator) {
		t.Fatalf("the refused apply changed leaf %d's epoch authenticator, so something of the stranger's epoch reached this one",
			subject.OwnLeafIndex())
	}
	here.assertLockstep(t)

	// the control: that same staged commit installs in the group that staged it, and its cohort
	// converges on the epoch it opened.
	if err := elsewhere.groups[1].ApplyCommit(foreignStaged); err != nil {
		t.Fatalf("the other group could not apply its own staged commit: %v; this case would then be observing a build that refuses every staged commit",
			err)
	}
	for _, group := range elsewhere.groups[2:] {
		processed, err := group.ProcessMessage(foreign.Commit)
		if err != nil {
			t.Fatalf("leaf %d could not process the other group's commit: %v", group.OwnLeafIndex(), err)
		}
		if err := group.ApplyCommit(processed); err != nil {
			t.Fatalf("leaf %d could not apply the other group's commit: %v", group.OwnLeafIndex(), err)
		}
	}
	if err := elsewhere.groups[0].MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit in the other group: %v", err)
	}
	elsewhere.assertLockstep(t)

	// and the epoch half, in ONE group: a staged commit of this group derived against the epoch
	// that has just closed. Two views stage the same commit; one of them applies it and has moved
	// on, and the other's staged value is then a value of this group and of no epoch it is in.
	author := here.groups[0]
	result, err := author.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	stagedAt := []*Processed{}
	for _, group := range here.groups[1:] {
		processed, err := group.ProcessMessage(result.Commit)
		if err != nil {
			t.Fatalf("leaf %d could not process the commit: %v", group.OwnLeafIndex(), err)
		}
		stagedAt = append(stagedAt, processed)
	}
	if err := here.groups[1].ApplyCommit(stagedAt[0]); err != nil {
		t.Fatalf("leaf %d could not apply the commit: %v", here.groups[1].OwnLeafIndex(), err)
	}
	if err := here.groups[1].ApplyCommit(stagedAt[1]); !errors.Is(err, errApplyCommitNotThisEpochs) {
		t.Fatalf("ApplyCommit of a staged commit of this group's CLOSED epoch = %v, want errApplyCommitNotThisEpochs",
			err)
	}
	// and its control: the same value installs at the view that staged it and has not moved.
	for at, processed := range stagedAt[1:] {
		group := here.groups[2+at]
		if err := group.ApplyCommit(processed); err != nil {
			t.Fatalf("leaf %d could not apply its own staged commit after another view refused it: %v",
				group.OwnLeafIndex(), err)
		}
	}
	if err := author.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	here.assertLockstep(t)
	here.exchange(t, 3, "after both refusals the group is one group")
}

// TestOneCommitAdmitsTwoJoinersAndEachOpensTheEntryAddressedToIt is the THIRD of the four, and it
// needs the thing the cohort could not build either: a second joiner.
//
// THE DEFECT was the Welcome's joiner pairing checked by a COUNT. (*StagedCommit).welcomeMessage
// pairs the leaves a commit added with the Add proposals it names, entry i with entry i, and what
// held those two readings together was len(adds) != len(self.added). errWelcomeAddPairing's own
// comment says why that is not enough -- "a COUNT is what a dual representation gets held together
// by and a count is what let four forks through" -- and a build where the two parted company seals
// each joiner's group secrets to some OTHER joiner's material, silently, with every length equal
// and every seal well formed.
//
// WHY THIS FILE COULD NOT SEE IT UNTIL NOW, measured: every commit it built added exactly ONE
// member, and with one entry on each side, entry 0 of one is entry 0 of the other under every
// permutation there is. A pairing defect is unreachable from a one joiner Welcome however the
// pairing is written.
//
// THREE MEMBERS AND THEN TWO, AND THE SIZE IS THE POINT. A joiner's Welcome entry carries the path
// secret for the LOWEST NODE ITS LEAF AND THE COMMITTER'S SHARE, so two joiners whose leaves share
// the SAME node with the committer are handed the same secret and a swapped pairing is invisible.
// A settled cohort of three leaves leaf 3 empty in a width four tree: the first Add refills leaf 3,
// under the committer's own subtree, and the second doubles the tree and lands at leaf 4, under the
// other one. Their common ancestors with leaf 0 are then different nodes -- which this case DERIVES
// off the tree and requires, rather than drawing it and hoping.
func TestOneCommitAdmitsTwoJoinersAndEachOpensTheEntryAddressedToIt(t *testing.T) {
	crypto := testCrypto(t)
	cohort := testNewCohort(t, crypto, "two-joiners", 3)
	defer cohort.closeAll()
	cohort.settle(t)
	cohort.assertLockstep(t)

	committer := cohort.groups[0]
	committerLeaf := committer.OwnLeafIndex()
	before := len(committer.Members())
	// Force, for addMembersInOneCommit's stated reason: an add-only commit carries no update path,
	// so its Welcome entries carry no path secret and there is nothing in them a swapped pairing
	// could put in the wrong joiner's hands.
	joined, _ := cohort.addMembersInOneCommit(t, committer, &CommitOptions{Force: true},
		"first", "second")
	if len(joined) != 2 {
		t.Fatalf("one commit admitted %d joiner(s), want 2", len(joined))
	}
	cohort.assertLockstep(t)
	if now := len(committer.Members()); now != before+2 {
		t.Fatalf("the group holds %d members after a commit adding two, want %d", now, before+2)
	}

	// the arrangement, derived and required BEFORE anything is claimed about the pairing. Two
	// joiners that share one node with the committer are handed one path secret, and a swapped
	// pairing between them is then a swap of two equal values.
	first := CommonAncestor(joined[0].OwnLeafIndex().NodeIndex(), committerLeaf.NodeIndex())
	second := CommonAncestor(joined[1].OwnLeafIndex().NodeIndex(), committerLeaf.NodeIndex())
	if joined[0].OwnLeafIndex() == joined[1].OwnLeafIndex() {
		t.Fatalf("both joiners came up at leaf %d", joined[0].OwnLeafIndex())
	}
	if first == second {
		t.Fatalf("leaf %d and leaf %d both share node %d with the committer at leaf %d, so their Welcome entries carry the same path secret and this case observes nothing about which entry went to which joiner",
			joined[0].OwnLeafIndex(), joined[1].OwnLeafIndex(), first, committerLeaf)
	}
	t.Logf("leaf %d shares node %d with the committer and leaf %d shares node %d",
		joined[0].OwnLeafIndex(), first, joined[1].OwnLeafIndex(), second)

	// each joiner is the member the GROUP holds at the leaf that joiner thinks is its own, which
	// is the statement a count cannot make. The identities are the cohort's own, in the order
	// they were admitted.
	identities := cohort.members[len(cohort.members)-2:]
	for at, one := range joined {
		member, found := committer.MemberAt(one.OwnLeafIndex())
		if !found {
			t.Fatalf("the group holds no member at leaf %d, which joiner %d came up at",
				one.OwnLeafIndex(), at)
		}
		if !bytes.Equal(member.IdentityPub, identities[at].IdentityPub) {
			t.Fatalf("joiner %d (%s) came up at leaf %d, and the group holds a different identity there: the Welcome entry it opened was addressed to somebody else",
				at, identities[at].Name, one.OwnLeafIndex())
		}
	}

	// and both halves of being a member, for each of them, at the epoch they joined at
	cohort.exchange(t, len(cohort.groups)-2, "the first joiner speaks")
	cohort.exchange(t, len(cohort.groups)-1, "the second joiner speaks")
	cohort.commitFrom(t, 1)
	cohort.assertLockstep(t)
	cohort.commitFrom(t, len(cohort.groups)-1)
	cohort.assertLockstep(t)
}
