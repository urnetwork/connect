// The two doors that install a staged epoch, and the two things they read before they do.
//
// commit_provenance_test.go holds the pair ApplyCommit compares -- the group and the epoch a staged
// commit was staged against -- and this file holds the two properties that file's fixtures could not
// see, both of them measured through the exported API alone:
//
//   - THE ORDER of those two refusals against the RemovesSelf arm. Both provenance fixtures commit
//     without removing anybody, so moving the two comparisons below the removal arm is a four line
//     reorder that leaves the whole suite green -- and lets a FOREIGN staged commit Close a live group
//     and erase its key material.
//   - AN ERASED staged commit. The group id, the prior epoch, the epoch, the committer and the
//     removal flag all survive Zeroize, because none of them is key material, so an erased value
//     walks the whole of the provenance door and installs an erased key schedule, an erased secret
//     tree and an erased leaf private state as the group's live epoch.
package mls

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// testRemovingStagedCommit is a Processed one member staged for a commit that removes IT, together
// with the group that staged it.
//
// The Remove proposal is delivered to the receiver before the commit that names it, for
// TestProcessCommitReportsSelfRemoval's stated reason: a commit names its proposals by reference, and
// a member that never received the Remove cannot resolve the reference at all -- so a fixture that
// skipped the delivery would observe errProposalNotCached and report a self-removal that was never
// staged.
func testRemovingStagedCommit(t *testing.T, crypto CryptoProvider, groupId string) (
	*Group, *Group, *Processed) {

	t.Helper()
	committer, receiver, _, _, _ := testTwoMemberGroupNamed(t, crypto, groupId)
	removal, err := committer.ProposeRemove(receiver.OwnLeafIndex())
	if err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	if _, err := receiver.ProcessMessage(removal); err != nil {
		t.Fatalf("the receiver could not cache the Remove naming it: %v", err)
	}
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if processed.Kind != ProcessedCommit || processed.Commit == nil {
		t.Fatalf("the receiver staged kind %d", processed.Kind)
	}
	if !processed.Commit.RemovesSelf() {
		t.Fatal("this fixture's commit does not remove the member that staged it, so the ordering it exists to observe is not reachable from it")
	}
	return committer, receiver, processed
}

// TestApplyCommitReadsProvenanceBeforeTheRemovalArm is the ordering, and the ordering alone.
//
// A staged commit that removes the member that staged it is a value ApplyCommit responds to by
// CLOSING the group -- erasing its key schedule, its secret tree, its leaf private state and its
// signing key -- and answering ErrRemovedFromGroup. So a door that read the removal flag before it
// read the provenance would let any Processed a caller crossed over from another group destroy this
// one, and there is no recovery from it: the epoch is gone from the process.
//
// BOTH HALVES OF THE PROVENANCE PAIR, because they are two comparisons and a reorder can move either.
// The group arm is a commit another group staged; the epoch arm is a commit of THIS group staged
// against an epoch it has already left.
//
// What each arm asserts is not only the sentinel but that the group is STILL ALIVE, which is the
// thing the sentinel cannot say: a body that closed the group and then returned the provenance
// refusal would answer exactly the error this case wants while having done the damage.
func TestApplyCommitReadsProvenanceBeforeTheRemovalArm(t *testing.T) {
	crypto := testCrypto(t)

	// the group arm.
	committerA, receiverA, removing := testRemovingStagedCommit(t, crypto, "removal-order-a")
	defer committerA.Close()
	defer receiverA.Close()
	committerB, receiverB, _, _, _ := testTwoMemberGroupNamed(t, crypto, "removal-order-b")
	defer committerB.Close()
	defer receiverB.Close()

	epochBefore := receiverB.Epoch()
	authenticatorBefore := bytes.Clone(receiverB.EpochAuthenticator())
	if len(authenticatorBefore) == 0 {
		t.Fatal("the group this case protects answers no epoch authenticator before the call, so a closed group and a live one read the same here")
	}
	if err := receiverB.ApplyCommit(removing); !errors.Is(err, errApplyCommitNotThisGroups) {
		t.Fatalf("group B ApplyCommit over a removing commit group A staged = %v, want errApplyCommitNotThisGroups", err)
	}
	if !bytes.Equal(receiverB.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("a foreign staged commit that removes its own stager destroyed group B's epoch; the removal arm is being read before the provenance pair")
	}
	if receiverB.Epoch() != epochBefore {
		t.Fatalf("the refused ApplyCommit moved group B from epoch %d to %d", epochBefore, receiverB.Epoch())
	}
	// alive rather than merely unmoved: an epoch authenticator is a read of the schedule, and Protect
	// is a write to the secret tree.
	if _, err := receiverB.Protect(nil, []byte("group B is still here")); err != nil {
		t.Fatalf("group B cannot protect a message after refusing a foreign removing commit: %v", err)
	}

	// the epoch arm, over ONE group id so that the group comparison cannot be what refuses it.
	//
	// The laggard is a SECOND VIEW of the same member at an epoch it has not left -- the join door
	// run again over the same Welcome, which is commit_provenance_test.go's device -- and it holds
	// the leaf the removing commit names. So the removal flag is true of it, the group id agrees, and
	// the epoch comparison is the only thing standing between it and a Close.
	committer, receiver, _, _, material := testTwoMemberGroupNamed(t, crypto, "removal-order-epoch")
	defer committer.Close()
	defer receiver.Close()
	laggard, err := material.join(t, nil)
	if err != nil {
		t.Fatalf("the second view this arm lags with: %v", err)
	}
	defer laggard.Close()

	// one plain commit, so the removing commit below is staged against an epoch the laggard is not in.
	advancing, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the plain CreateCommit: %v", err)
	}
	moved, err := receiver.ProcessMessage(advancing.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage of the plain commit: %v", err)
	}
	if err := receiver.ApplyCommit(moved); err != nil {
		t.Fatalf("ApplyCommit of the plain commit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit of the plain commit: %v", err)
	}

	removal, err := committer.ProposeRemove(receiver.OwnLeafIndex())
	if err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	if _, err := receiver.ProcessMessage(removal); err != nil {
		t.Fatalf("the receiver could not cache the Remove naming it: %v", err)
	}
	ejecting, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit that removes the receiver: %v", err)
	}
	staged, err := receiver.ProcessMessage(ejecting.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage of the removing commit: %v", err)
	}
	if !staged.Commit.RemovesSelf() {
		t.Fatal("the staged commit of the epoch arm does not remove the member that staged it, so the ordering it exists to observe is not reachable")
	}
	if laggard.Epoch() >= staged.Commit.Epoch() {
		t.Fatalf("the laggard is at epoch %d and the removing commit opens epoch %d, so this arm is not the stale one",
			laggard.Epoch(), staged.Commit.Epoch())
	}

	authenticatorBefore = bytes.Clone(laggard.EpochAuthenticator())
	if len(authenticatorBefore) == 0 {
		t.Fatal("the laggard answers no epoch authenticator before the call, so this arm cannot tell a closed group from a live one")
	}
	if err := laggard.ApplyCommit(staged); !errors.Is(err, errApplyCommitNotThisEpochs) {
		t.Fatalf("ApplyCommit of a removing commit staged against another epoch = %v, want errApplyCommitNotThisEpochs", err)
	}
	if !bytes.Equal(laggard.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("a removing commit staged against an epoch this group is not in destroyed its epoch; the removal arm is being read before the epoch comparison")
	}
	if _, err := laggard.Protect(nil, []byte("still a member")); err != nil {
		t.Fatalf("the laggard cannot protect a message after refusing a stale removing commit: %v", err)
	}
}

// TestApplyCommitRefusesAStagedCommitWhoseKeyMaterialHasBeenErased is the second measurement, and it
// is reached through the exported API alone: Processed and its Commit field are exported, Zeroize is
// exported, and connect/message is documented as holding Processed values across a policy decision.
//
// WHAT THE ERASED VALUE STILL ANSWERS is the whole reason the door needed a new flag. Zeroize erases
// the key schedule, the secret tree, the leaf private state and the update path plan; the group id,
// the prior epoch, the epoch and the removal flag are not key material and survive it untouched. So
// the provenance pair -- the only thing this door read -- agrees over an erased commit exactly as it
// agrees over a live one.
//
// AND THE SYMPTOM IS THE FORK DETECTOR. Two members that each took this path would install two erased
// key schedules and answer the same 32 zero bytes for the epoch authenticator, which is the value the
// product compares to decide whether two devices are in the same epoch. The second half of this case
// is that comparison, run over two groups that have nothing in common.
func TestApplyCommitRefusesAStagedCommitWhoseKeyMaterialHasBeenErased(t *testing.T) {
	crypto := testCrypto(t)
	committerA, receiverA, _, _, _ := testTwoMemberGroupNamed(t, crypto, "erased-staged-a")
	defer committerA.Close()
	defer receiverA.Close()
	committerB, receiverB, _, _, _ := testTwoMemberGroupNamed(t, crypto, "erased-staged-b")
	defer committerB.Close()
	defer receiverB.Close()

	erasedAt := func(t *testing.T, committer *Group, receiver *Group) *Processed {
		t.Helper()
		result, err := committer.CreateCommit(nil, nil, nil)
		if err != nil {
			t.Fatalf("CreateCommit: %v", err)
		}
		processed, err := receiver.ProcessMessage(result.Commit)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		// the control: while it is live the staged commit answers the epoch it opens.
		if live := processed.Commit.EpochAuthenticator(); len(live) != crypto.HashSize() {
			t.Fatalf("the live staged commit answers %d octets for its epoch authenticator, want %d",
				len(live), crypto.HashSize())
		}
		processed.Commit.Zeroize()
		return processed
	}

	processedA := erasedAt(t, committerA, receiverA)
	// an erased staged commit answers NOTHING rather than the zero bytes its erased schedule holds.
	// Without that, the two groups below compare equal on a value both of them made up.
	if authenticator := processedA.Commit.EpochAuthenticator(); authenticator != nil {
		t.Errorf("an erased staged commit answers %x for its epoch authenticator, want nothing", authenticator)
	}

	epochBefore := receiverA.Epoch()
	authenticatorBefore := bytes.Clone(receiverA.EpochAuthenticator())
	if err := receiverA.ApplyCommit(processedA); !errors.Is(err, errStagedCommitErased) {
		t.Fatalf("ApplyCommit of an erased staged commit = %v, want errStagedCommitErased", err)
	}
	if receiverA.Epoch() != epochBefore {
		t.Fatalf("the refused ApplyCommit moved the receiver from epoch %d to %d", epochBefore, receiverA.Epoch())
	}
	if !bytes.Equal(receiverA.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("the refused ApplyCommit changed the receiver's epoch authenticator")
	}
	// and it left no staged commit behind, which is what would block every later commit this member
	// makes with ErrPendingCommitExists.
	if _, err := receiverA.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("the receiver cannot commit after refusing an erased staged commit: %v", err)
	}
	receiverA.ClearPendingCommit()

	processedB := erasedAt(t, committerB, receiverB)
	if err := receiverB.ApplyCommit(processedB); !errors.Is(err, errStagedCommitErased) {
		t.Fatalf("group B ApplyCommit of an erased staged commit = %v, want errStagedCommitErased", err)
	}
	// the symptom, stated over the two groups rather than over the door: two members that both took
	// this path used to answer one epoch authenticator, and answering one is what the product reads
	// as "these two devices have not forked".
	if bytes.Equal(receiverA.EpochAuthenticator(), receiverB.EpochAuthenticator()) {
		t.Fatal("two members of two unrelated groups answer one epoch authenticator after each was handed an erased staged commit")
	}
}

// TestMergePendingCommitRefusesAStagedCommitWhoseKeyMaterialHasBeenErased is the SECOND door of the
// same rule, and it is here because the rule is about installing a staged epoch rather than about the
// method a caller happens to call. ApplyCommit reaches live state through this method, so a guard
// written only at ApplyCommit is a guard that holds for the callers somebody enumerated.
//
// The pending commit is reached through the group's own field because no exported API hands a caller
// its own pending StagedCommit today. That is what makes this the door rather than the case: a
// CommitResult that ever carries one -- and the type is exported, with an exported Zeroize -- turns
// this into a caller shape overnight, and the refusal is already standing there.
func TestMergePendingCommitRefusesAStagedCommitWhoseKeyMaterialHasBeenErased(t *testing.T) {
	crypto := testCrypto(t)
	group, joined, _, _, _ := testTwoMemberGroupNamed(t, crypto, "erased-pending")
	defer group.Close()
	defer joined.Close()

	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	epochBefore := group.Epoch()
	authenticatorBefore := bytes.Clone(group.EpochAuthenticator())
	group.stateLock.Lock()
	group.pending.Zeroize()
	group.stateLock.Unlock()

	if err := group.MergePendingCommit(); !errors.Is(err, errStagedCommitErased) {
		t.Fatalf("MergePendingCommit over an erased pending commit = %v, want errStagedCommitErased", err)
	}
	if group.Epoch() != epochBefore {
		t.Fatalf("the refused merge moved the group from epoch %d to %d", epochBefore, group.Epoch())
	}
	if !bytes.Equal(group.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("the refused merge changed the group's epoch authenticator, so an erased epoch was installed")
	}
	// the pending commit is LEFT WHERE IT IS rather than dropped, which is what every other refusal
	// of that method does: what to do with a staged epoch this group could not enter is the caller's
	// decision, through ClearPendingCommit.
	if _, err := group.CreateCommit(nil, nil, nil); !errors.Is(err, ErrPendingCommitExists) {
		t.Fatalf("CreateCommit after the refused merge = %v, want ErrPendingCommitExists", err)
	}
	group.ClearPendingCommit()
}

// TestApplyCommitReadsTheEraseFlagBeforeTheRemovalArm is the ORDERING of the refusal above, and it is
// here because the case above cannot see it: both of its staged commits remove nobody, so an erase
// check moved below the RemovesSelf arm leaves it green.
//
// It is the same defect this file's first case is about, one refusal along, and it was found by
// running that defect's own class over this change. A staged commit that removes this client and has
// then been ERASED is a value whose removal flag is intact -- selfRemoved is not key material and
// survives Zeroize -- so a door that read the flag first would Close the group, erase its schedule,
// its secret tree, its leaf state and its signing key, and answer ErrRemovedFromGroup out of a value
// that carries no key material at all. What the caller learns is that it was removed; what actually
// happened is that it handed back a value it had already erased.
func TestApplyCommitReadsTheEraseFlagBeforeTheRemovalArm(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, processed := testRemovingStagedCommit(t, crypto, "erased-removal-order")
	defer committer.Close()
	defer receiver.Close()

	processed.Commit.Zeroize()
	if !processed.Commit.RemovesSelf() {
		t.Fatal("the erase cleared the removal flag, so this case can no longer tell the two arms apart")
	}
	authenticatorBefore := bytes.Clone(receiver.EpochAuthenticator())
	if len(authenticatorBefore) == 0 {
		t.Fatal("the receiver answers no epoch authenticator before the call, so this case cannot tell a closed group from a live one")
	}
	if err := receiver.ApplyCommit(processed); !errors.Is(err, errStagedCommitErased) {
		t.Fatalf("ApplyCommit of an erased removing commit = %v, want errStagedCommitErased", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("an ERASED staged commit closed the group; the removal arm is being read before the erase flag")
	}
	if _, err := receiver.Protect(nil, []byte("not removed by an erased value")); err != nil {
		t.Fatalf("the receiver cannot protect a message after refusing an erased removing commit: %v", err)
	}
}

// TestAnInstalledStagedCommitIsAShellWhoseEraseTouchesNothingLive is the third property of the
// install doors, and the one the header's two could not see: WHAT THE DOORS LEAVE IN THE VALUE. A
// merge moves the schedule, the secret tree and the leaf private state into the group by pointer,
// and until 2026-09-21 it left the staged value pointing at all three -- so the value a caller
// holds after ApplyCommit named the epoch the group had just entered, and the erase every
// caller's cleanup runs on a value it has finished with, (*StagedCommit).Zeroize, erased the live
// epoch. Measured through messagegroup's discard door: Export and Protect refused with the
// erased-epoch error and the next commit did not decrypt, with no error at the line that did it.
//
// Held at BOTH installs, the receiver's ApplyCommit and the committer's MergePendingCommit, over
// the storage the epoch itself uses and not copies of it: after the install the value holds no
// key material and no authenticator, it is NOT flagged erased -- it was installed -- and the
// erase run on it afterwards leaves every secret the group is running on where it was. The plan
// is the one entry the merge itself erases, so its ladder is read as the merge's own erase and
// excluded from the "still live" reading rather than counted against it.
func TestAnInstalledStagedCommitIsAShellWhoseEraseTouchesNothingLive(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	own := committer.stagedForTest()
	if own == nil || processed.Commit == nil {
		t.Fatal("one of the two installs has nothing staged, so this case observes half of what it claims")
	}
	// the controls: both values hold an epoch before the install, and the storage snapshotted
	// here is the storage the group will be RUNNING ON afterwards, because the merge moves it
	// by pointer. Every entry is checked non-zero by the helper.
	live := map[string]map[string][]byte{}
	for name, staged := range map[string]*StagedCommit{"the receiver's": processed.Commit, "the committer's": own} {
		if staged.EpochAuthenticator() == nil {
			t.Fatalf("%s staged value answers no authenticator before the install, so the shell below observes nothing", name)
		}
		live[name] = map[string][]byte{}
		for entry, secret := range stagedEpochStorage(t, staged) {
			if strings.HasPrefix(entry, "plan.") {
				continue
			}
			live[name][entry] = secret
		}
		if len(live[name]) == 0 {
			t.Fatalf("%s staged value holds no key material outside the plan, so the erase below is over nothing", name)
		}
	}

	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	authenticator := bytes.Clone(receiver.EpochAuthenticator())
	if len(authenticator) == 0 || !bytes.Equal(authenticator, committer.EpochAuthenticator()) {
		t.Fatal("the two installs did not enter one epoch, so nothing below is about an installed value")
	}

	for name, staged := range map[string]*StagedCommit{"the receiver's": processed.Commit, "the committer's": own} {
		// the shell: the public facts survive, the key material does not, and the value is
		// not flagged erased because it was not erased
		if staged.schedule != nil || staged.secretTree != nil || staged.ownPriv != nil || staged.plan != nil {
			t.Errorf("%s staged value still holds key material after its install (schedule=%v secretTree=%v ownPriv=%v plan=%v); the merge moved it by pointer and left the value naming the live epoch",
				name, staged.schedule != nil, staged.secretTree != nil, staged.ownPriv != nil, staged.plan != nil)
		}
		if staged.erased {
			t.Errorf("%s staged value is flagged erased after its install; it was installed, and a second ApplyCommit of it is the provenance pair's refusal and not the erase's", name)
		}
		if got := staged.EpochAuthenticator(); got != nil {
			t.Errorf("%s staged value answers a %d octet authenticator after its install, want nothing: the epoch's authenticator is the group's to answer now", name, len(got))
		}
		if leaves := staged.OccupiedLeavesAfter(); len(leaves) != 2 {
			t.Errorf("%s staged value answers %v for the post-commit leaves after its install; the public facts about the commit are what a shell keeps", name, leaves)
		}
		// THE PROPERTY: the erase every caller's cleanup runs on a value it has finished with
		staged.Zeroize()
		for entry, secret := range live[name] {
			if !slices.ContainsFunc(secret, func(b byte) bool { return b != 0 }) {
				t.Errorf("erasing %s installed staged value zeroed %s, which is storage the group is running on", name, entry)
			}
		}
	}
	// and read through the doors a caller has: the epoch is still there, at both members, and
	// the next commit still opens
	if !bytes.Equal(receiver.EpochAuthenticator(), authenticator) || !bytes.Equal(committer.EpochAuthenticator(), authenticator) {
		t.Fatal("erasing an installed staged value changed a member's epoch authenticator")
	}
	if _, err := receiver.Export("test", nil, 32); err != nil {
		t.Fatalf("the receiver cannot export after erasing the value it applied: %v", err)
	}
	if _, err := committer.Protect(nil, []byte("still in the epoch")); err != nil {
		t.Fatalf("the committer cannot protect after erasing the value it merged: %v", err)
	}
	next, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the next CreateCommit: %v", err)
	}
	nextProcessed, err := receiver.ProcessMessage(next.Commit)
	if err != nil {
		t.Fatalf("the receiver cannot open the next commit after erasing the value it applied: %v", err)
	}
	if err := receiver.ApplyCommit(nextProcessed); err != nil {
		t.Fatalf("the next ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("the next MergePendingCommit: %v", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two members disagree on the epoch after the next commit")
	}
}

// TestAnInstalledStagedCommitIsRefusedByNameAtEveryDoorAndAtEveryView is the fourth property
// of the install doors and the one the shell case above could not see: WHAT THE DOORS DO WITH
// THE SHELL WHEN IT COMES BACK. A shell keeps the public facts about the commit, and two of
// those facts are the group id and the prior epoch -- the whole of what the provenance pair
// reads -- so a shell handed to a SECOND live instance of the same member at the epoch the
// commit was staged against clears the pair, is not flagged erased, removes nobody, and reaches
// the merge. Measured before the refusal below existed, through the exported API alone: the
// merge erased that instance's live schedule, assigned the shell's nil one over it, and the
// persist dereferenced it -- a panic inside ApplyCommit and an instance that panicked on every
// Protect after it.
//
// Held at three places, because the refusal is one rule and each place is a way of not having
// written it:
//
//   - THE SECOND VIEW, which is the measured shape: refused by name, alive, and -- the control
//     -- able to process and apply the same commit itself afterwards.
//   - THE SAME INSTANCE, which has moved on: refused by name and NOT by the epoch pair, which is
//     the ordering the header asks for -- a value the caller has finished with is named as such
//     wherever it is handed. commit_provenance_test.go's backward arm used to expect the pair
//     here and now expects this.
//   - THE MERGE DOOR, reached with a shell filed as pending the way the erased case reaches it:
//     refused before the erase of the epoch the merge would close, group unmoved.
//
// AND THE REPORT A REMOVED MEMBER IS HANDED IS THE CONTROL IN THE OTHER DIRECTION: it holds no
// schedule either, it is the one legitimate value of this type that does not, and the door
// still answers it on the removal arm. A predicate that read the schedule alone would refuse
// every removal, and that is a mutant this case kills together with the removal cases above.
//
// Reachable only through raw mls. messagegroup's seam refuses a value staged by another handle
// by name before mls is asked anything -- (*connectMlsHandle).stagedBy -- and releases the staged
// half on its own install, so no EngineProcessed reaches this door as a shell; the case is here
// because the door is here, and a door that holds only for the callers somebody enumerated is
// the shape this file's header names.
func TestAnInstalledStagedCommitIsRefusedByNameAtEveryDoorAndAtEveryView(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _, material := testTwoMemberGroupNamed(t, crypto, "installed-shell")
	defer committer.Close()
	defer receiver.Close()
	// the second view: the join door run again over the same Welcome, which is
	// commit_provenance_test.go's device and puts this instance at the same epoch as the receiver
	second, err := material.join(t, nil)
	if err != nil {
		t.Fatalf("the second view this case hands the shell to: %v", err)
	}
	defer second.Close()
	if second.Epoch() != receiver.Epoch() || !bytes.Equal(second.EpochAuthenticator(), receiver.EpochAuthenticator()) {
		t.Fatal("the two views are not one member at one epoch, so the provenance pair would refuse the shell below for a reason that is not this case's")
	}

	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	own := committer.stagedForTest()
	if own == nil || processed.Commit == nil {
		t.Fatal("one of the two installs has nothing staged")
	}
	// the controls: neither value is a shell before its install, and the predicate reads the
	// same thing the doors read
	if processed.Commit.installed() || own.installed() {
		t.Fatal("a staged value reads as installed before anything installed it, so every refusal below is over the wrong condition")
	}
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if !processed.Commit.installed() || !own.installed() {
		t.Fatal("an installed staged value does not read as installed, so the doors below refuse nothing")
	}
	if processed.Commit.erased || own.erased {
		t.Fatal("the install flagged a value erased; this case is about the value the erase flag cannot see")
	}

	// THE SECOND VIEW: refused by name, and alive
	epochBefore := second.Epoch()
	authenticatorBefore := bytes.Clone(second.EpochAuthenticator())
	if len(authenticatorBefore) == 0 {
		t.Fatal("the second view answers no epoch authenticator before the call, so a bricked instance and a live one read the same here")
	}
	if err := second.ApplyCommit(processed); !errors.Is(err, errStagedCommitInstalled) {
		t.Fatalf("a second view's ApplyCommit over a shell the first view installed = %v, want errStagedCommitInstalled", err)
	}
	if second.Epoch() != epochBefore {
		t.Fatalf("the refused ApplyCommit moved the second view from epoch %d to %d", epochBefore, second.Epoch())
	}
	if !bytes.Equal(second.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("the refused ApplyCommit changed the second view's epoch authenticator: the shell's nil schedule was installed over the live one")
	}
	// alive rather than merely unmoved, through the door that panicked before the refusal existed
	if _, err := second.Protect(nil, []byte("the second view is still here")); err != nil {
		t.Fatalf("the second view cannot protect a message after refusing a shell: %v", err)
	}
	// and it left nothing pending, which is what would block the control below
	if _, err := second.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("the second view cannot commit after refusing a shell: %v; the refusal filed the shell as pending", err)
	}
	second.ClearPendingCommit()
	// THE CONTROL: the same commit, processed by this view itself, installs
	ownProcessed, err := second.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("the second view's own ProcessMessage of the commit it was handed a shell of: %v", err)
	}
	if err := second.ApplyCommit(ownProcessed); err != nil {
		t.Fatalf("the second view's own ApplyCommit: %v; the refusal above is then a refusal of every value and not of a shell", err)
	}
	if !bytes.Equal(second.EpochAuthenticator(), receiver.EpochAuthenticator()) {
		t.Fatal("the two views disagree on the epoch after the second applied its own staged value")
	}

	// THE SAME INSTANCE, which has moved on: named as a shell and not as a value of another
	// epoch, which is the ordering against the provenance pair
	replayed := receiver.ApplyCommit(processed)
	if !errors.Is(replayed, errStagedCommitInstalled) {
		t.Fatalf("the receiver's second ApplyCommit of the value it installed = %v, want errStagedCommitInstalled: the shell is read before the epoch pair", replayed)
	}
	if errors.Is(replayed, errApplyCommitNotThisEpochs) {
		t.Fatal("the shell refusal also carries the epoch pair's sentinel; the two are two facts and this one is the one the caller acts on")
	}
	if receiver.Epoch() != committer.Epoch() {
		t.Fatalf("the refused ApplyCommit moved the receiver to epoch %d, committer at %d", receiver.Epoch(), committer.Epoch())
	}

	// THE MERGE DOOR, with the committer's own shell filed as pending the way the erased case
	// files an erased one. What is held is that the refusal comes BEFORE the erase of the epoch
	// this merge would close: the authenticator is the schedule's, and a merge that erased first
	// and refused second leaves a group whose authenticator is nothing.
	epochBefore = committer.Epoch()
	authenticatorBefore = bytes.Clone(committer.EpochAuthenticator())
	committer.stateLock.Lock()
	committer.pending = own
	committer.stateLock.Unlock()
	if err := committer.MergePendingCommit(); !errors.Is(err, errStagedCommitInstalled) {
		t.Fatalf("MergePendingCommit over a shell filed as pending = %v, want errStagedCommitInstalled", err)
	}
	if committer.Epoch() != epochBefore {
		t.Fatalf("the refused merge moved the committer from epoch %d to %d", epochBefore, committer.Epoch())
	}
	if !bytes.Equal(committer.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("the refused merge changed the committer's epoch authenticator: the live epoch was erased before the shell was refused")
	}
	if _, err := committer.Protect(nil, []byte("the committer is still here")); err != nil {
		t.Fatalf("the committer cannot protect a message after the refused merge: %v", err)
	}
	// the pending shell is left where it is, which is that door's discipline at every refusal,
	// and it is dropped through the ordinary door -- whose erase, on a shell, erases nothing live
	committer.ClearPendingCommit()
	if !bytes.Equal(committer.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("clearing the refused shell erased the committer's live epoch")
	}

	// THE OTHER DIRECTION: the report a removed member is handed holds no schedule and is NOT a
	// shell, and the door still answers it on the removal arm
	removingCommitter, removed, report := testRemovingStagedCommit(t, crypto, "installed-shell-report")
	defer removingCommitter.Close()
	defer removed.Close()
	if report.Commit.installed() {
		t.Fatal("the report a removed member is handed reads as an installed shell; it holds no schedule because it derived no epoch, and the removal arm is its door")
	}
	if err := removed.ApplyCommit(report); !errors.Is(err, ErrRemovedFromGroup) {
		t.Fatalf("ApplyCommit of a removed member's report = %v, want ErrRemovedFromGroup: a shell predicate that reads the schedule alone refuses every removal", err)
	}

	// and the three instances that are left agree on the next epoch, so nothing above touched
	// what the group runs on
	next, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the next CreateCommit: %v", err)
	}
	for name, view := range map[string]*Group{"the receiver": receiver, "the second view": second} {
		nextProcessed, err := view.ProcessMessage(next.Commit)
		if err != nil {
			t.Fatalf("%s cannot open the next commit: %v", name, err)
		}
		if err := view.ApplyCommit(nextProcessed); err != nil {
			t.Fatalf("%s's next ApplyCommit: %v", name, err)
		}
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("the next MergePendingCommit: %v", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) || !bytes.Equal(second.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the three instances disagree on the epoch after the next commit")
	}
}
