// Ledger item 242's R4 at the seam: the role a leaf held AT ONE EPOCH, read off the handle that
// epoch's records open under and off no other.
//
// R4 is OBSERVER read-only, and what it needs from this package is one question answered honestly:
// "who stood at this leaf when this record was sealed, and what were they allowed to do then?"
// Spec A rules where the answer is read from -- "from the transcript-covered group-context
// extension of the SENDING EPOCH, never from current membership" -- and item 242's ruling 17 rules
// which handle does the reading: the session's OWN handle for that epoch, through
// scheduleForOnLoop, because a door reading the same handle the open read cannot fail where the
// open succeeded. Every case below is stated over one of those two sentences.
//
// Nothing here decides what an observer may do. That is the sdk's, on the application record path;
// this file is the input and the promise that it says what its name says.
package messagegroup

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls"
)

// commitPolicyToOpener has the FOUNDER commit a policy naming one identity, the opener follow it,
// and the opener's session advance onto the epoch that opens.
//
// It is the founder that commits and the opener that receives, rather than the opener committing
// alone as advanceOpener has it, because the role the cases below read is one the opener LEARNED
// from somebody else's commit -- which is the only way a receiving client ever learns a role.
// connect/mls authorizes nothing, here or anywhere: whether this committer was allowed to make
// this change is the sdk's predicate on both arms, and a seam that refused would be a second
// authorizer nobody could hold equal to the first.
func commitPolicyToOpener(t *testing.T, pair *pastEpochPair, identity []byte, role mls.Role) uint64 {
	t.Helper()
	policy := policyOf(t, contextExtensionsOf(t, pair.chain.founder))
	policy.SetRole(identity, role)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("encoding a policy naming %x as %s: %v", identity, role, err)
	}
	commit, _, _, err := pair.chain.founder.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("the founder's CommitPolicy: %v", err)
	}
	processed, err := pair.chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the opener's Process of the policy commit: %v", err)
	}
	if err := pair.chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("the opener's ApplyCommit: %v", err)
	}
	if err := pair.chain.founder.MergePendingCommit(); err != nil {
		t.Fatalf("the founder's MergePendingCommit: %v", err)
	}
	if err := pair.opener.AdvanceEpoch(pair.chain.pqSecret); err != nil {
		t.Fatalf("the opener's AdvanceEpoch: %v", err)
	}
	epoch, err := pair.opener.Epoch()
	if err != nil {
		t.Fatalf("the opener's Epoch: %v", err)
	}
	return epoch
}

// assertRoleAt holds one (epoch, leaf) ask to an identity and a role.
func assertRoleAt(t *testing.T, session *GroupSession, epoch uint64, leaf uint32,
	wantIdentity []byte, wantRole string) {

	t.Helper()
	identityPub, role, err := session.RoleAt(epoch, leaf)
	if err != nil {
		t.Fatalf("RoleAt(epoch %d, leaf %d): %v", epoch, leaf, err)
	}
	if !bytes.Equal(identityPub, wantIdentity) {
		t.Errorf("RoleAt(epoch %d, leaf %d) names identity %x, want %x", epoch, leaf, identityPub, wantIdentity)
	}
	if role != wantRole {
		t.Errorf("RoleAt(epoch %d, leaf %d) answers role %q, want %q", epoch, leaf, role, wantRole)
	}
}

// theEpochRefusalIn is which of the session's three epoch sentinels an error carries, or a fatal
// for none of them.
//
// IT IS WHAT MAKES THE SENTINEL EQUALITY CASE AN EQUALITY. Asserting that each of two calls
// errors.Is one named sentinel would pass a build where they answered two different ones, because
// each half would be written with its own expectation; resolving both to a value and comparing the
// values is the assertion the ruling actually asks for.
func theEpochRefusalIn(t *testing.T, what string, err error) error {
	t.Helper()
	for _, sentinel := range []error{ErrEpochOutOfWindow, ErrPastEpochUnobtainable, ErrRecordNotForThisSession} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	t.Fatalf("%s answered %v, which carries none of this session's three epoch sentinels", what, err)
	return nil
}

// ---------------------------------------------------------------------------
// (a) per-epoch truth
// ---------------------------------------------------------------------------

// THE CASE THAT CARRIES RULING 21. A member demoted at one epoch and promoted at the next held
// three different roles at three epochs of one group, and a session standing at the LAST of them
// answers all three -- each off the handle that epoch's own records open under.
//
// The identity is the same at all three, which is the other half of the claim: the role moved and
// the member did not, so an answer that tracked the member would be the same value three times and
// an answer that tracked the epoch is three values.
func TestARoleIsReadAtTheEpochOfTheRecordAndNotAtTheEpochTheSessionStandsAt(t *testing.T) {
	pair := newPastEpochPair(t, "roleat-per-epoch")
	alice, bob := pair.chain.a.identityPub, pair.chain.b.identityPub
	aliceLeaf, bobLeaf := pair.senderLeaf, pair.openerLeaf
	if epoch, _ := pair.opener.Epoch(); epoch != 1 {
		t.Fatalf("the chain stands at epoch %d after the founding add, want 1", epoch)
	}

	// epoch 1: bob is in the group and NOTHING names him. The control is read off the policy
	// itself, so "unnamed" is a fact about this epoch rather than an assumption about the fixture.
	if _, named := policyOf(t, contextExtensionsOf(t, pair.chain.joined)).RoleOf(bob); named {
		t.Fatal("the policy already names the joiner at epoch 1, so the demotion below observes nothing")
	}

	if epoch := commitPolicyToOpener(t, pair, bob, mls.RoleObserver); epoch != 2 {
		t.Fatalf("the demotion opened epoch %d, want 2", epoch)
	}
	if epoch := commitPolicyToOpener(t, pair, bob, mls.RoleAdmin); epoch != 3 {
		t.Fatalf("the promotion opened epoch %d, want 3", epoch)
	}

	// standing at 3, all three epochs answer, and they do not answer the same thing.
	assertRoleAt(t, pair.opener, 1, bobLeaf, bob, "member")
	assertRoleAt(t, pair.opener, 2, bobLeaf, bob, "observer")
	assertRoleAt(t, pair.opener, 3, bobLeaf, bob, "admin")
	// and the founder is the OWNER at every one of them, which is the control for "the reading
	// moved with the policy rather than with the epoch number".
	for _, epoch := range []uint64{1, 2, 3} {
		assertRoleAt(t, pair.opener, epoch, aliceLeaf, alice, "owner")
	}

	// ONCE PER EPOCH AND NOT PER ASK: the table is built on the first ask and held with the
	// schedule, so ten more asks at epoch 2 cost no second load of it.
	loadsAfterFirstAsk := pair.loads[2]
	if loadsAfterFirstAsk != 1 {
		t.Fatalf("epoch 2's schedule was loaded %d time(s) by the first ask, want 1", loadsAfterFirstAsk)
	}
	for range 10 {
		assertRoleAt(t, pair.opener, 2, bobLeaf, bob, "observer")
	}
	if pair.loads[2] != loadsAfterFirstAsk {
		t.Errorf("ten more asks at epoch 2 cost %d further load(s) of its schedule; the table is not held",
			pair.loads[2]-loadsAfterFirstAsk)
	}

	// a leaf nobody stands at, at an epoch this session DID reach, is its own refusal and not an
	// epoch refusal: the two are told apart by nothing downstream otherwise.
	if _, _, err := pair.opener.RoleAt(2, 9); !errors.Is(err, ErrEngineMemberLeaf) {
		t.Errorf("RoleAt(2, 9) in a two member group answered %v, want ErrEngineMemberLeaf", err)
	}
	// and a FUTURE epoch is refused with the sentinel an open of a future record gets.
	if _, _, err := pair.opener.RoleAt(4, bobLeaf); !errors.Is(err, ErrRecordNotForThisSession) {
		t.Errorf("RoleAt(4, ...) at a session at epoch 3 answered %v, want ErrRecordNotForThisSession", err)
	}
}

// ---------------------------------------------------------------------------
// (b) sentinel equality -- the case that makes ruling 17 true
// ---------------------------------------------------------------------------

// A ROLE ASK CANNOT FAIL WHERE THE OPEN SUCCEEDED, AND WHERE THE OPEN FAILS IT FAILS THE SAME WAY.
// Both halves are asserted over the SAME record at the SAME epoch in the same test, because that
// is the only shape in which "the same handle" is a claim about a run rather than about the source.
//
// BOTH REASONS AN EPOCH IS UNOBTAINABLE ARE COVERED, and they are different reasons with different
// sentinels: below the window is arithmetic every device takes the same way, and below ADMISSION
// is a fact about ONE DEVICE'S disk -- a joiner's store holds no epoch beneath its Welcome, so its
// LoadGroup answers "this state store holds no such value" for an epoch that is well inside the
// window and that other members hold. A case that covered only the first would pass a build that
// routed the second anywhere at all.
func TestARoleAskAndAnOpenAtTheSameEpochAnswerTheSameSentinel(t *testing.T) {
	t.Run("below the window", func(t *testing.T) {
		pair := newPastEpochPair(t, "roleat-below-window")
		record := pair.sealDurable(t, "sealed at epoch one")
		if record.Header.Epoch != 1 {
			t.Fatalf("the sender sealed at epoch %d, want 1", record.Header.Epoch)
		}
		// one epoch further than the window reaches: at 2+PastEpochWindow, epoch 1 is
		// PastEpochWindow+1 behind.
		for range PastEpochWindow + 1 {
			pair.advanceOpener(t)
		}
		if epoch, _ := pair.opener.Epoch(); epoch != 2+PastEpochWindow {
			t.Fatalf("the opener is at epoch %d, want %d", epoch, 2+PastEpochWindow)
		}
		_, _, openErr := pair.opener.OpenRecord(record)
		_, _, roleErr := pair.opener.RoleAt(record.Header.Epoch, pair.senderLeaf)
		opened := theEpochRefusalIn(t, "OpenRecord at an epoch below the window", openErr)
		asked := theEpochRefusalIn(t, "RoleAt at an epoch below the window", roleErr)
		if opened != asked {
			t.Fatalf("the open refused with %v and the role ask refused with %v; a door reading the same handle the open reads cannot answer a different sentinel",
				opened, asked)
		}
		if opened != ErrEpochOutOfWindow {
			t.Errorf("both answered %v for an epoch below the window, want ErrEpochOutOfWindow", opened)
		}
	})

	t.Run("below admission", func(t *testing.T) {
		pair := newPastEpochPair(t, "roleat-below-admission")
		record := pair.sealDurable(t, "sealed before the third member existed")

		// the opener adds a third member, which is the commit that opens epoch 2.
		c := newTestEngine(t)
		keyPackage, err := c.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("C's NewKeyPackage: %v", err)
		}
		_, welcome, ratchetTree, err := pair.chain.joined.CommitAdd([][]byte{keyPackage})
		if err != nil {
			t.Fatalf("the opener's CommitAdd: %v", err)
		}
		if err := pair.chain.joined.MergePendingCommit(); err != nil {
			t.Fatalf("the opener's MergePendingCommit: %v", err)
		}
		if err := pair.opener.AdvanceEpoch(pair.chain.pqSecret); err != nil {
			t.Fatalf("the opener's AdvanceEpoch: %v", err)
		}
		admitted, err := c.engine.JoinFromWelcome(welcome, ratchetTree)
		if err != nil {
			t.Fatalf("C's JoinFromWelcome: %v", err)
		}
		defer admitted.Close()
		if admitted.Epoch() != 2 {
			t.Fatalf("C was admitted at epoch %d, want 2", admitted.Epoch())
		}
		cSession, err := NewGroupSession(admitted, pair.chain.pqSecret, pair.chain.groupHandleKey,
			newStreamIndexMemory(), testClock(), testServerNonce())
		if err != nil {
			t.Fatalf("C's session: %v", err)
		}
		defer cSession.Close()
		groupId := admitted.GroupId()
		if err := cSession.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
			return c.engine.LoadGroup(groupId, epoch)
		}); err != nil {
			t.Fatalf("C's InstallPastEpochLoader: %v", err)
		}

		// THE CONTROL, FIRST: a member that WAS there answers both, so the refusals below are
		// about C's disk and not about the epoch.
		pair.trackAt(t, 1, 0)
		if _, body, err := pair.opener.OpenRecord(record); err != nil || string(body) != "sealed before the third member existed" {
			t.Fatalf("the opener, a member at epoch 1, answered %v / %q", err, body)
		}
		assertRoleAt(t, pair.opener, 1, pair.senderLeaf, pair.chain.a.identityPub, "owner")

		_, _, openErr := cSession.OpenRecord(record)
		_, _, roleErr := cSession.RoleAt(record.Header.Epoch, pair.senderLeaf)
		opened := theEpochRefusalIn(t, "C's OpenRecord below its admission", openErr)
		asked := theEpochRefusalIn(t, "C's RoleAt below its admission", roleErr)
		if opened != asked {
			t.Fatalf("C's open refused with %v and C's role ask refused with %v; both reach the epoch through one lookup and must answer one sentinel",
				opened, asked)
		}
		if opened != ErrPastEpochUnobtainable {
			t.Errorf("both answered %v for an epoch beneath C's Welcome, want ErrPastEpochUnobtainable", opened)
		}
	})
}

// ---------------------------------------------------------------------------
// (c) the unnamed and policy-less readings
// ---------------------------------------------------------------------------

// ITEM 242's RULINGS 8 AND 20, AT THIS SEAM. An identity the policy does not name is a MEMBER, and
// an epoch whose context carries no urmessage_group_policy at all reads EVERY member as a MEMBER
// -- not as an error, and not as an OBSERVER.
//
// The second half is worth a real commit rather than a hand-built context: item 242's P3 measured
// that a by-value GroupContextExtensions omitting 0xF001 is ACCEPTED and that the group afterwards
// has no policy, so this is a state a hostile committer can put an honest receiver into, and what
// R4 does about it decides whether that committer can silence the whole group by dropping one
// extension. The founder is the assertion that matters: it is the OWNER in the policy that has
// just stopped existing, and it reads "member" -- the default, applied to everybody, rather than a
// remembered role or a refusal.
func TestAnUnnamedIdentityAndAnEpochWithNoPolicyBothReadAsMember(t *testing.T) {
	chain := newCommitAddChain(t, "roleat-unnamed")
	founderLeaf, joinerLeaf := chain.founded.OwnLeafIndex(), chain.joined.OwnLeafIndex()
	before := contextExtensionsOf(t, chain.joined)
	if _, named := policyOf(t, before).RoleOf(chain.joiner.identityPub); named {
		t.Fatal("the joiner is named at epoch 1, so the unnamed reading below observes nothing")
	}
	if role, named := policyOf(t, before).RoleOf(chain.founder.identityPub); !named || role != mls.RoleOwner {
		t.Fatalf("the founder is %s named=%v at epoch 1, want owner true; the case below needs a role to lose", role, named)
	}

	identityPub, role, err := chain.joined.RoleAt(joinerLeaf)
	if err != nil {
		t.Fatalf("RoleAt over an unnamed member: %v", err)
	}
	if !bytes.Equal(identityPub, chain.joiner.identityPub) || role != "member" {
		t.Errorf("an unnamed identity reads %x / %q, want %x / \"member\"", identityPub, role, chain.joiner.identityPub)
	}

	// AND NOW THE POLICY GOES AWAY. The list is the group's own with the 0xF001 entry dropped and
	// nothing else touched, committed by value through the wholesale arm.
	withoutPolicy := []ExtensionBytes{}
	for _, entry := range before {
		if entry.Type == uint16(mls.ExtensionTypeUrmessageGroupPolicy) {
			continue
		}
		withoutPolicy = append(withoutPolicy, entry)
	}
	if len(withoutPolicy) != len(before)-1 {
		t.Fatalf("the list without 0xF001 has %d entries and the list with it has %d", len(withoutPolicy), len(before))
	}
	commit, _, _, err := chain.founded.CommitContextExtensions(withoutPolicy)
	if err != nil {
		t.Fatalf("CommitContextExtensions over a list with no policy: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process of a commit that drops the policy: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	// the control: the epoch really has no policy, read the way a caller would read it.
	if _, _, found, err := findExtensionEntryOf(t, contextExtensionsOf(t, chain.joined),
		uint16(mls.ExtensionTypeUrmessageGroupPolicy)); err != nil || found {
		t.Fatalf("after the commit the context still carries a 0xF001 entry (found=%v, err=%v)", found, err)
	}

	for who, want := range map[uint32][]byte{founderLeaf: chain.founder.identityPub, joinerLeaf: chain.joiner.identityPub} {
		for name, handle := range map[string]GroupHandle{"the committer": chain.founded, "the receiver": chain.joined} {
			identityPub, role, err := handle.RoleAt(who)
			if err != nil {
				t.Fatalf("%s's RoleAt(%d) at an epoch with no policy: %v", name, who, err)
			}
			if !bytes.Equal(identityPub, want) {
				t.Errorf("%s's RoleAt(%d) names %x, want %x", name, who, identityPub, want)
			}
			if role != "member" {
				t.Errorf("%s's RoleAt(%d) answers %q at an epoch carrying no policy, want \"member\"", name, who, role)
			}
		}
	}
}

// findExtensionEntryOf is mls.FindExtensionEntry over the seam's list, answering the entry, its
// presence and the error apart so a case can assert ABSENCE rather than fataling on it.
func findExtensionEntryOf(t *testing.T, extensions []ExtensionBytes, extensionType uint16) (uint16, []byte, bool, error) {
	t.Helper()
	entry, found, err := mls.FindExtensionEntry(mlsExtensionsOf(extensions), mls.ExtensionType(extensionType))
	if err != nil || !found {
		return 0, nil, found, err
	}
	return uint16(entry.ExtensionType), entry.ExtensionData, true, nil
}

// ---------------------------------------------------------------------------
// (c2) the key: a leaf index and not an ordinal
// ---------------------------------------------------------------------------

// A LEAF INDEX IS NOT AN ORDINAL THE MOMENT ANYTHING IS REMOVED, and this is the case that says
// so: three members at leaves 0, 1 and 2, then leaf 1 removed. The member at LEAF 2 now stands at
// ORDINAL 1, so a projection keyed on the ordinal answers the wrong member for leaf 1 and refuses
// leaf 2 -- both of them a role attached to somebody who did not send the record.
//
// Every two-member case in this file would pass under that projection, because in a group nobody
// has ever left the two numbers are equal. This one is the only place in the package where they
// are not.
func TestRoleAtIsKeyedByLeafIndexAndNotByOrdinal(t *testing.T) {
	chain := newCommitAddChain(t, "roleat-leaf-not-ordinal")
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the third member's NewKeyPackage: %v", err)
	}
	commit, welcome, ratchetTree, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("Process of the add: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit of the add: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the add: %v", err)
	}
	thirdHandle, err := third.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the third member's join: %v", err)
	}
	defer thirdHandle.Close()
	if thirdHandle.OwnLeafIndex() != 2 || chain.joined.MemberCount() != 3 {
		t.Fatalf("the group is not three members at leaves 0..2 (third at %d, %d members)",
			thirdHandle.OwnLeafIndex(), chain.joined.MemberCount())
	}

	// name the THIRD member admin, so the answer this case reads is one nobody else holds.
	policy := policyOf(t, contextExtensionsOf(t, chain.founded))
	policy.SetRole(third.identityPub, mls.RoleAdmin)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("encoding the policy: %v", err)
	}
	policyCommit, _, _, err := chain.founded.CommitPolicy(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("CommitPolicy: %v", err)
	}
	policyProcessed, err := thirdHandle.Process(policyCommit)
	if err != nil {
		t.Fatalf("the third member's Process of the policy commit: %v", err)
	}
	if err := thirdHandle.ApplyCommit(policyProcessed); err != nil {
		t.Fatalf("the third member's ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the policy: %v", err)
	}

	// and now leaf 1 leaves. The committer is the founder, so the removed leaf is neither end of
	// the ask below.
	removeCommit, _, _, err := chain.founded.CommitRemove([]uint32{1})
	if err != nil {
		t.Fatalf("CommitRemove(leaf 1): %v", err)
	}
	removeProcessed, err := thirdHandle.Process(removeCommit)
	if err != nil {
		t.Fatalf("the third member's Process of the remove: %v", err)
	}
	if err := thirdHandle.ApplyCommit(removeProcessed); err != nil {
		t.Fatalf("the third member's ApplyCommit of the remove: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the remove: %v", err)
	}

	// THE CONTROL, PRINTED: the ordinal and the leaf index have come apart, and here is where.
	if chain.founded.MemberCount() != 2 {
		t.Fatalf("the group is %d members after the removal, want 2", chain.founded.MemberCount())
	}
	ordinals := map[int]uint32{}
	for at := 0; at < chain.founded.MemberCount(); at += 1 {
		leaf, _, _, err := chain.founded.MemberAt(at)
		if err != nil {
			t.Fatalf("MemberAt(%d): %v", at, err)
		}
		ordinals[at] = leaf
	}
	if ordinals[1] != 2 {
		t.Fatalf("ordinal 1 is leaf %d after the removal; this case needs a group where the two disagree, and got %v",
			ordinals[1], ordinals)
	}

	for name, handle := range map[string]GroupHandle{"the committer": chain.founded, "the third member": thirdHandle} {
		identityPub, role, err := handle.RoleAt(2)
		if err != nil {
			t.Fatalf("%s's RoleAt(leaf 2): %v", name, err)
		}
		if !bytes.Equal(identityPub, third.identityPub) || role != "admin" {
			t.Errorf("%s's RoleAt(leaf 2) answers %x / %q, want %x / \"admin\" -- ordinal 1 is leaf 2 here, so an ordinal-keyed projection answers this at the wrong key",
				name, identityPub, role, third.identityPub)
		}
		if _, _, err := handle.RoleAt(1); !errors.Is(err, ErrEngineMemberLeaf) {
			t.Errorf("%s's RoleAt(leaf 1), which nobody stands at, answered %v, want ErrEngineMemberLeaf", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// (d) the table's lifetime
// ---------------------------------------------------------------------------

// THE CACHED TABLE DOES NOT SURVIVE AN EPOCH INSTALL -- item 242's ruling 18, held by the only
// observation that can tell a dropped table from a kept one: ASK, CHANGE THE ROLE, INSTALL, ASK
// AGAIN, and get the NEW role.
//
// The key of the table is the LEAF INDEX, and an epoch install moves nothing about a leaf except
// exactly the thing being asked for, so a table that outlived installEpochOnLoop would answer the
// new epoch's question out of the old epoch's entry with no error anywhere. The second half of the
// case is the control that the answer is not simply "always recomputed": the OLD epoch still
// answers the OLD role, which is what says the drop replaced the table rather than the reading.
func TestTheRoleTableDoesNotSurviveAnEpochInstall(t *testing.T) {
	pair := newPastEpochPair(t, "roleat-table-lifetime")
	bob, bobLeaf := pair.chain.b.identityPub, pair.openerLeaf

	// (1) ASK AT THE LIVE EPOCH, which is what puts an entry for this leaf in the session's own
	// table.
	assertRoleAt(t, pair.opener, 1, bobLeaf, bob, "member")

	// (2) change the role and install the epoch that carries the change.
	if epoch := commitPolicyToOpener(t, pair, bob, mls.RoleAdmin); epoch != 2 {
		t.Fatalf("the promotion opened epoch %d, want 2", epoch)
	}

	// (3) ASK AGAIN AT THE NEW LIVE EPOCH, at the SAME LEAF. A kept table answers "member" here.
	assertRoleAt(t, pair.opener, 2, bobLeaf, bob, "admin")

	// (4) and the control: epoch 1 still answers epoch 1's role, so what the install did was drop
	// the table rather than stop this door from caching at all.
	assertRoleAt(t, pair.opener, 1, bobLeaf, bob, "member")

	// AND THE PRIOR EPOCHS' TABLES DIE THE SAME DEATH, which is measurable through the loader:
	// their table is a field of the schedule, installEpochOnLoop re-makes self.pastEpochs
	// wholesale, so the next ask at epoch 1 rebuilds the schedule out of the store. The count
	// above is one load for step (4); one more install and one more ask is one more load.
	loads := pair.loads[1]
	if loads != 1 {
		t.Fatalf("epoch 1's schedule was loaded %d time(s) by the ask at step 4, want 1", loads)
	}
	pair.advanceOpener(t)
	assertRoleAt(t, pair.opener, 1, bobLeaf, bob, "member")
	if pair.loads[1] != loads+1 {
		t.Errorf("after a second epoch install the ask at epoch 1 cost %d further load(s), want 1; a schedule that survived the install would have carried its role table with it",
			pair.loads[1]-loads)
	}
}

// ---------------------------------------------------------------------------
// and the door is closed with the session
// ---------------------------------------------------------------------------

// A CLOSED SESSION ANSWERS ErrSessionClosed AND NOT A ROLE, which is every other door of this type
// and is stated because the table this one holds is the first field of a session that a caller can
// read without touching a key.
func TestRoleAtOnAClosedSessionIsRefused(t *testing.T) {
	pair := newPastEpochPair(t, "roleat-closed")
	assertRoleAt(t, pair.opener, 1, pair.openerLeaf, pair.chain.b.identityPub, "member")
	if err := pair.opener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := pair.opener.RoleAt(1, pair.openerLeaf); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("RoleAt on a closed session answered %v, want ErrSessionClosed", err)
	}
}
