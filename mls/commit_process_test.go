// RFC 9420 section 12.4.2, the receive half of a commit, held to the two members it takes to
// observe it.
//
// WHY THIS FILE IS BUILT AROUND TWO GROUPS. Every property this task is about is a property of two
// members AGREEING -- the epoch a commit opens, the secrets that epoch derives, and the transcript
// the confirmation tag is taken over -- and a single group processing its own commit agrees with
// itself under any derivation at all. Task 13 built the commits and this is what applies them, so
// the assertion that says the two halves are one protocol is the epoch authenticator: it is
// DeriveSecret(epoch_secret, "authentication"), so two members that agree on it agree on the epoch
// secret rather than on some public function of the tree.
//
// AND WHY THREE OF THE CASES EDIT AN AuthenticatedContent RATHER THAN A MESSAGE. Every octet of a
// commit's framed content is covered by the confirmed transcript hash the confirmation tag is taken
// over, and the whole message is covered by the AEAD -- so a case that damages bytes on the wire is
// refused by the tag or by the decrypt whatever else is wrong with it, and it would report the door
// it names while observing neither. The refusals of section 12.4.2 are reachable only from a
// content that was opened honestly and edited after, or from a commit built through the seams
// CommitOptions keeps unexported for exactly this.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// testTwoMemberGroup returns a committer and a joiner already in the same group at the same epoch.
func testTwoMemberGroup(t *testing.T, crypto CryptoProvider) (*Group, *Group, *testMember, *testMember) {
	t.Helper()
	group, joined, owner, bob, _ := testTwoMemberGroupNamed(t, crypto, "group-1")
	return group, joined, owner, bob
}

// testJoinMaterial is the Welcome one commit produced together with the material it is addressed
// to, kept so that a case can run the join door a SECOND time: for another view of the same group
// at the epoch that Welcome describes, or for a join whose one field the case has changed.
type testJoinMaterial struct {
	crypto  CryptoProvider
	member  *testMember
	groupId string
	welcome []byte
	tree    []byte
	keys    JoinKeyMaterial
}

// join runs the door over a COPY of the material, so a case that edits one field of it leaves the
// rest of the fixture where the successful join found it.
func (self *testJoinMaterial) join(t *testing.T, edit func(*JoinKeyMaterial)) (*Group, error) {
	t.Helper()
	keys := self.keys
	if edit != nil {
		edit(&keys)
	}
	return JoinFromWelcome(testGroupConfig(t, self.crypto, self.member, self.groupId),
		self.welcome, self.tree, &keys)
}

// testTwoMemberGroupNamed is the fixture above with the group id a case chooses, which is what two
// INDEPENDENT groups need: every group this client is a member of runs an epoch 1, so two groups
// sharing an id would let a provenance case pass on the epoch alone.
func testTwoMemberGroupNamed(t *testing.T, crypto CryptoProvider, groupId string) (
	*Group, *Group, *testMember, *testMember, *testJoinMaterial) {

	t.Helper()
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, groupId)

	bob := testIdentity(t, crypto, "bob")
	kp, initPriv, encPriv := testKeyPackage(t, crypto, bob)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal key package: %v", err)
	}
	if _, err := group.ProposeAdd(encoded); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	result, err := group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	material := &testJoinMaterial{
		crypto: crypto, member: bob, groupId: groupId,
		welcome: result.Welcome, tree: result.RatchetTree,
		keys: JoinKeyMaterial{
			KeyPackage:     *kp,
			InitPrivate:    initPriv,
			EncryptPrivate: encPriv,
			SignPrivate:    bob.SigPriv,
		},
	}
	joined, err := material.join(t, nil)
	if err != nil {
		t.Fatalf("JoinFromWelcome: %v", err)
	}
	if joined.Epoch() != group.Epoch() {
		t.Fatalf("the joiner is at epoch %d and the committer at %d, so this fixture is not two members of one epoch",
			joined.Epoch(), group.Epoch())
	}
	if !bytes.Equal(joined.EpochAuthenticator(), group.EpochAuthenticator()) {
		t.Fatal("the two groups this fixture answers disagree on the epoch authenticator, so nothing built on it observes an agreement")
	}
	return group, joined, owner, bob, material
}

// testOpenInboundCommit opens a commit message the way (*Group).ProcessMessage does, so that a case
// can edit the AuthenticatedContent the receive path works on and hand it back to
// stageInboundCommitLocked.
//
// See this file's header for why editing after the open is the only reading that separates one door
// of section 12.4.2 from the ones behind it.
func testOpenInboundCommit(t *testing.T, receiver *Group, message []byte) *AuthenticatedContent {
	t.Helper()
	parsed, err := ParseMLSMessage(message)
	if err != nil {
		t.Fatalf("parse the commit this case opens: %v", err)
	}
	if parsed.PrivateMessage == nil {
		t.Fatal("the commit this case opens is not a PrivateMessage")
	}
	groupContext, err := syntax.Marshal(receiver.context)
	if err != nil {
		t.Fatalf("encode the receiver's group context: %v", err)
	}
	authenticated, err := OpenPrivateMessage(receiver.crypto, receiver.secretTree,
		receiver.senderDataSecretLocked(), parsed.PrivateMessage,
		func(sender Sender) (SignaturePublicKey, error) {
			leaf := receiver.tree.Leaf(sender.LeafIndex)
			if leaf == nil {
				return nil, fmt.Errorf("leaf %d is blank", sender.LeafIndex)
			}
			return leaf.SignatureKey, nil
		}, groupContext)
	if err != nil {
		t.Fatalf("open the commit this case edits: %v", err)
	}
	return authenticated
}

// testSealedApplicationNaming builds an application message this group would never send: one whose
// framed content names a group id and an epoch the caller chose, sealed under the epoch this group
// is actually in.
//
// It is the only construction that can reach ValSem002 and ValSem003 at all. A message from another
// group or another epoch cannot be OPENED by this receiver -- its sender data and its content are
// sealed under that epoch's secret tree -- so a case that simply sent a later epoch's message
// observes the message key lookup and reports the context check. The group context in the signature
// preimage here is the SENDER'S own, which is the receiver's too, so the signature verifies and the
// framed content's own fields are the only disagreement left.
func testSealedApplicationNaming(t *testing.T, sender *Group, groupId []byte, epoch uint64,
	plaintext []byte) []byte {

	t.Helper()
	sender.stateLock.Lock()
	defer sender.stateLock.Unlock()
	groupContext, err := syntax.Marshal(sender.context)
	if err != nil {
		t.Fatalf("encode the sender's group context: %v", err)
	}
	content := &FramedContent{
		GroupId:         groupId,
		Epoch:           epoch,
		Sender:          Sender{SenderType: SenderTypeMember, LeafIndex: sender.ownLeaf},
		ContentType:     ContentTypeApplication,
		ApplicationData: plaintext,
	}
	authenticated, err := SignAuthenticatedContent(sender.crypto, sender.signer,
		WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		t.Fatalf("sign the framed content this case builds: %v", err)
	}
	private, err := SealPrivateMessage(sender.crypto, sender.secretTree,
		sender.senderDataSecretLocked(), authenticated, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal the message this case builds: %v", err)
	}
	encoded, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: private,
	})
	if err != nil {
		t.Fatalf("frame the message this case builds: %v", err)
	}
	return encoded
}

// TestProcessCommitStagesRatherThanMerges is the round trip against task 13 and the staging
// contract in one: a commit generated there, processed here, lands both members on the same epoch
// secrets -- and does not move the receiver until the caller says so.
func TestProcessCommitStagesRatherThanMerges(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	// the live control: a commit naming no proposals must carry an update path (section 12.4), so
	// this case exercises the merge and the decrypt rather than the pathless arm beside them
	if staged := committer.stagedForTest(); staged == nil || !staged.hasPath {
		t.Fatal("this commit carries no update path, so the path half of section 12.4.2 is asked nothing here")
	}

	epochBefore := receiver.Epoch()
	authenticatorBefore := receiver.EpochAuthenticator()
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if processed.Kind != ProcessedCommit || processed.Commit == nil {
		t.Fatalf("Kind = %d, Commit = %v", processed.Kind, processed.Commit)
	}
	if receiver.Epoch() != epochBefore {
		t.Fatalf("ProcessMessage moved the receiver to epoch %d; a commit comes back staged so the caller can refuse it",
			receiver.Epoch())
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), authenticatorBefore) {
		t.Fatal("ProcessMessage changed the epoch the receiver derives from, with the caller having applied nothing")
	}
	if processed.Commit.Epoch() != epochBefore+1 {
		t.Fatalf("the staged commit opens epoch %d, want %d", processed.Commit.Epoch(), epochBefore+1)
	}
	if processed.Commit.Committer() != committer.OwnLeafIndex() {
		t.Fatalf("the staged commit names leaf %d as the committer, want %d",
			processed.Commit.Committer(), committer.OwnLeafIndex())
	}

	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if receiver.Epoch() != committer.Epoch() {
		t.Fatalf("receiver epoch %d, committer epoch %d", receiver.Epoch(), committer.Epoch())
	}
	authenticator := committer.EpochAuthenticator()
	if len(authenticator) != crypto.HashSize() {
		t.Fatalf("the committer's epoch authenticator is %d octets, so a comparison against it says nothing",
			len(authenticator))
	}
	if bytes.Equal(authenticator, authenticatorBefore) {
		t.Fatal("the epoch this commit opened derives the authenticator the epoch it closed did, so nothing advanced")
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), authenticator) {
		t.Fatal("the receiver and the committer disagree on the epoch authenticator, so they are in two different epochs")
	}
	// and the exporter, which is expanded from a different DeriveSecret of the same epoch secret:
	// two members agreeing on both agree on the epoch secret rather than on one derivation of it
	committerExport, err := committer.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the committer's Export: %v", err)
	}
	receiverExport, err := receiver.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("the receiver's Export: %v", err)
	}
	if !bytes.Equal(committerExport, receiverExport) {
		t.Fatal("the two members derive different storage secrets from the epoch they agree they are in")
	}
	// and the epoch that opened carries traffic in both directions, which is what says the secret
	// tree and the private tree state the merge installed are the ones the other member computed
	message, err := receiver.Protect([]byte("aad"), []byte("after the commit"))
	if err != nil {
		t.Fatalf("Protect in the new epoch: %v", err)
	}
	opened, err := committer.Unprotect(message)
	if err != nil {
		t.Fatalf("Unprotect in the new epoch: %v", err)
	}
	if string(opened.Plaintext) != "after the commit" {
		t.Fatalf("the new epoch opened %q", opened.Plaintext)
	}
}

// TestProcessCommitRejectsACommitFromAnotherEpoch is the plan's epoch case, and what it observes is
// written down rather than left to its name: a commit of the epoch AFTER this receiver's was sealed
// under that epoch's secret tree, so what refuses it is the message key lookup and not ValSem003.
// The rule itself is unreachable from an honest message and is held next door.
func TestProcessCommitRejectsACommitFromAnotherEpoch(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	first, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	second, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("second CreateCommit: %v", err)
	}
	// the receiver is still at the first epoch, so the second commit is ahead of it
	if _, err := receiver.ProcessMessage(second.Commit); err == nil {
		t.Fatal("ProcessMessage accepted a commit from a future epoch")
	}
	// and the one of its own epoch is accepted, which is the control that says the refusal above is
	// about the epoch rather than about this fixture
	processed, err := receiver.ProcessMessage(first.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage on the right epoch: %v", err)
	}
	processed.Commit.Zeroize()
}

// TestProcessMessageRefusesAFramedContentNamingAnotherGroupOrEpoch is ValSem002 and ValSem003 over
// the one input that can reach them: a message sealed under THIS epoch's keys whose framed content
// names another group or another epoch.
//
// Both halves are asserted with their own sentinel and a live control in front of them, because a
// construction that was refused for some third reason would satisfy an err != nil assertion
// perfectly.
func TestProcessMessageRefusesAFramedContentNamingAnotherGroupOrEpoch(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	accepted := testSealedApplicationNaming(t, committer, committer.GroupId(), committer.Epoch(),
		[]byte("the control"))
	control, err := receiver.ProcessMessage(accepted)
	if err != nil {
		t.Fatalf("the control message this case is built on was refused: %v", err)
	}
	if control.Kind != ProcessedApplication || string(control.Application.Plaintext) != "the control" {
		t.Fatalf("the control opened as kind %d", control.Kind)
	}

	ahead := testSealedApplicationNaming(t, committer, committer.GroupId(), committer.Epoch()+1,
		[]byte("another epoch"))
	if _, err := receiver.ProcessMessage(ahead); !errors.Is(err, errWrongEpoch) {
		t.Fatalf("a framed content naming epoch %d was refused with %v, want errWrongEpoch",
			committer.Epoch()+1, err)
	}

	stranger := testSealedApplicationNaming(t, committer, []byte("another-group"), committer.Epoch(),
		[]byte("another group"))
	if _, err := receiver.ProcessMessage(stranger); !errors.Is(err, errWrongGroupId) {
		t.Fatalf("a framed content naming another group was refused with %v, want errWrongGroupId", err)
	}
}

// TestProcessCommitRejectsATamperedCommitMessage is the plan's tampering case, and what it observes
// is written down rather than left to its name: the last octet of the message is inside the
// PrivateMessage's ciphertext, so what refuses it is the AEAD. The confirmation tag's own rule is
// held by TestProcessCommitRefusesAConfirmationTagOverTheEpochTheCommitCloses, which is the only
// case that can reach it.
func TestProcessCommitRejectsATamperedCommitMessage(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	tampered := append([]byte(nil), result.Commit...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := receiver.ProcessMessage(tampered); err == nil {
		t.Fatal("ProcessMessage accepted a tampered commit")
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage on the untampered commit: %v", err)
	}
	processed.Commit.Zeroize()
}

// TestProcessCommitRefusesAConfirmationTagOverTheEpochTheCommitCloses is ValSem205 asked about the
// thing that makes it a rule: WHICH transcript the tag is taken over.
//
// A tag taken over the confirmed transcript hash of the epoch the commit CLOSES is Nh octets of
// perfectly good MAC output under the right key, and both members compute it identically -- so a
// receive path that checked the tag before advancing the transcript agrees with a sender that made
// the same mistake, and every round trip in this package stays green. The seam is CommitOptions'
// own, which is why it is unexported: nothing outside this package can build the commit below.
func TestProcessCommitRefusesAConfirmationTagOverTheEpochTheCommitCloses(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	forged, err := committer.CreateCommit(nil, nil, &CommitOptions{
		confirmationTagOverPreCommitTranscript: true,
		skipValidation:                         true,
	})
	if err != nil {
		t.Fatalf("build the commit whose tag covers the closing epoch: %v", err)
	}
	if _, err := receiver.ProcessMessage(forged.Commit); !errors.Is(err, errBadConfirmationTag) {
		t.Fatalf("a commit whose confirmation tag covers the epoch it closes was refused with %v, want errBadConfirmationTag",
			err)
	}
	// and the live control: the same commit with the tag over the epoch it OPENS is accepted, so
	// what the refusal above observes is the transcript and not the seam
	committer.ClearPendingCommit()
	honest, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(honest.Commit)
	if err != nil {
		t.Fatalf("the honest commit of the same epoch was refused: %v", err)
	}
	processed.Commit.Zeroize()
}

// TestStagingAnInboundCommitJudgesTheUpdatePathsLeaf is RFC 9420 section 7.3's commit door on the
// production path, and it is the reason this task exists in the shape it does.
//
// ValidateUpdatePathLeafNode had no caller anywhere in this package on the day it landed, and
// MergeUpdatePath says in as many words that it does not verify the leaf's signature -- so the leaf
// of a received update path was judged by NOTHING. What makes this case observe that door rather
// than the tag is that the edit is made after the open: with the door in place the staging is
// refused at step 5 with errUpdatePathLeafNodeInvalid, and without it the same content reaches step
// 7 and is refused by the confirmation tag, which is a different sentinel two steps later.
func TestStagingAnInboundCommitJudgesTheUpdatePathsLeaf(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	result, err := committer.CreateCommit(nil, nil, &CommitOptions{Force: true})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	authenticated := testOpenInboundCommit(t, receiver, result.Commit)
	if authenticated.Content.Commit == nil || authenticated.Content.Commit.Path == nil {
		t.Fatal("this commit carries no update path, so the door below is asked nothing")
	}
	if len(authenticated.Content.Commit.Path.LeafNode.Signature) == 0 {
		t.Fatal("the update path's leaf carries no signature, so the edit below changes nothing")
	}

	// the live control: unedited, this content stages
	staged, err := receiver.stageInboundCommitLocked(authenticated)
	if err != nil {
		t.Fatalf("the unedited commit does not stage, so the refusal below would say nothing: %v", err)
	}
	staged.Zeroize()

	authenticated.Content.Commit.Path.LeafNode.Signature[0] ^= 0x01
	if _, err := receiver.stageInboundCommitLocked(authenticated); !errors.Is(err, errUpdatePathLeafNodeInvalid) {
		t.Fatalf("an update path whose leaf is not validly signed was refused with %v, want errUpdatePathLeafNodeInvalid; nothing else on this path verifies that signature",
			err)
	}
}

// TestProcessCommitReportsSelfRemoval is the commit that ejects this client.
//
// The proposal is DELIVERED to the receiver before the commit that names it, which is not a detail
// of the fixture: a commit names proposals by reference, and a member that never received the
// Remove cannot resolve the reference at all -- so a case that skipped the delivery would observe
// errProposalNotCached and report a self-removal path that was never entered.
func TestProcessCommitReportsSelfRemoval(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	removal, err := committer.ProposeRemove(receiver.OwnLeafIndex())
	if err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	if _, err := receiver.ProcessMessage(removal); err != nil {
		t.Fatalf("the receiver could not process the Remove naming it: %v", err)
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
		t.Fatalf("Kind = %d", processed.Kind)
	}
	if !processed.Commit.RemovesSelf() {
		t.Fatal("a commit removing us must report RemovesSelf")
	}
	if removed := processed.Commit.RemovedLeaves(); len(removed) != 1 || removed[0] != receiver.OwnLeafIndex() {
		t.Fatalf("the staged commit reports %v as removed, want [%d]", removed, receiver.OwnLeafIndex())
	}
	// and nobody has been ejected yet: the decision is ApplyCommit's and the caller's
	if receiver.EpochAuthenticator() == nil {
		t.Fatal("ProcessMessage closed the group")
	}
	if err := receiver.ApplyCommit(processed); !errors.Is(err, ErrRemovedFromGroup) {
		t.Fatalf("ApplyCommit error = %v, want ErrRemovedFromGroup", err)
	}
	// and the group is closed with its secrets gone, rather than left running in an epoch it is
	// not a member of
	if receiver.EpochAuthenticator() != nil {
		t.Error("the removed client's group still answers an epoch authenticator")
	}
	if _, err := receiver.Protect(nil, []byte("still here")); !errors.Is(err, errGroupClosed) {
		t.Errorf("the removed client can still protect a message: %v", err)
	}
}

// TestProcessProposalCachesIt is the second arm of ProcessMessage, and the assertion that matters is
// the COMMIT at the end: a cache entry a later commit cannot name is an entry that is not doing the
// job the cache exists for.
func TestProcessProposalCachesIt(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	if held := len(committer.pendingProposalsForTest()); held != 0 {
		t.Fatalf("the committer already holds %d cached proposal(s), so the count below says nothing", held)
	}
	message, err := receiver.ProposeUpdate()
	if err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	processed, err := committer.ProcessMessage(message)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if processed.Kind != ProcessedProposal || processed.Proposal == nil {
		t.Fatalf("Kind = %d", processed.Kind)
	}
	if processed.Sender.LeafIndex != receiver.OwnLeafIndex() {
		t.Fatalf("the proposal is attributed to leaf %d, want %d",
			processed.Sender.LeafIndex, receiver.OwnLeafIndex())
	}
	if len(committer.pendingProposalsForTest()) != 1 {
		t.Fatal("an inbound proposal must be cached so a later commit can reference it")
	}
	// the committer can now commit the other member's update by reference, and the update lands
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit covering the peer update: %v", err)
	}
	staged := committer.stagedForTest()
	if updated := staged.UpdatedLeaves(); len(updated) != 1 || updated[0] != receiver.OwnLeafIndex() {
		t.Fatalf("the commit reports %v as updated, want [%d]", updated, receiver.OwnLeafIndex())
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	applied, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("the proposer could not process the commit covering its own update: %v", err)
	}
	if err := receiver.ApplyCommit(applied); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two members disagree on the epoch a commit covering an inbound proposal opened")
	}
}

// TestProcessCommitInstallsTheLeafKeyOfAnUpdateThisClientPublished is the step the plan's task 18
// does not have and the receive path cannot do without.
//
// An Update at this client's own leaf is one this client PUBLISHED -- ValSem111 makes an Update's
// sender the leaf it updates -- so the epoch the commit opens carries a leaf key whose private half
// is in this client's store and nowhere in the epoch it is leaving. Measured before this step
// existed: a member whose Update another member commits was refused at DecryptUpdatePath on the
// very commit that carried its own proposal, with errPathDecrypt, which reads as a corrupt commit
// from a peer that did everything right.
//
// The assertion is over the key the group INSTALLED and not over the fact that it processed the
// commit: a build that carried the old leaf key forward and happened to open the path at a node
// ABOVE the leaf would process this commit perfectly and decrypt nothing afterwards.
func TestProcessCommitInstallsTheLeafKeyOfAnUpdateThisClientPublished(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	before := bytes.Clone(receiver.ownPriv.EncryptionPriv)
	if len(before) == 0 {
		t.Fatal("the receiver holds no leaf private key, so the comparison below says nothing")
	}
	update, err := receiver.ProposeUpdate()
	if err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	if _, err := committer.ProcessMessage(update); err != nil {
		t.Fatalf("the committer could not process the Update: %v", err)
	}
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("the proposer could not process the commit carrying its own update: %v", err)
	}
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}

	// the leaf the GROUP now carries at this client's index, which is the key every other member
	// will seal to from here on
	installed := receiver.tree.Leaf(receiver.ownLeaf)
	if installed == nil {
		t.Fatal("the receiver holds no leaf of its own after the commit")
	}
	filed, err := receiver.store.GetPrivateKey(installed.EncryptionKey)
	if err != nil {
		t.Fatalf("the private half of the leaf key this client published is not in its store: %v", err)
	}
	if bytes.Equal(before, filed) {
		t.Fatal("the update published the leaf key this client already held, so the swap below observes nothing")
	}
	if !bytes.Equal(receiver.ownPriv.EncryptionPriv, filed) {
		t.Fatal("the group is holding a leaf private key that is not the private half of the leaf key it now publishes; every update path sealed to that leaf from here on is one this member cannot open")
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two members disagree on the epoch the commit carrying the update opened")
	}
}

// TestApplyCommitRefusesToOverwriteTheEpochThisClientStagedAndClearErasesIt is MASTER section
// 9.3's lost-commit race, over the discipline (*Group).CreateCommit states at the other end of the
// same file.
//
// The race is the ordinary case and not an edge one: the delivery service accepts at most one
// commit per (group, epoch), so a client whose commit lost it is holding a fully derived epoch --
// its own key schedule, its own secret tree and the leaf key it drew -- at the moment the peer's
// commit arrives. What this body used to assert is that ApplyCommit ERASED that epoch and installed
// the peer's over it, and that is the behaviour this task changed: a caller that had merely handed
// the wrong Processed value lost a derived epoch to a call that answered nil, and CreateCommit
// refuses the same collision rather than resolving it. So the refusal is asserted here, and the
// erase is asserted at the drop site it actually happens on -- ClearPendingCommit, which is the
// caller's one-call repair.
//
// THE REFUSAL DESTROYS NOTHING, which is the half a refusal-only assertion would miss: the staged
// epoch is still whole after it, so a caller that refused wrongly has lost nothing, and the erase
// below is therefore about the CLEAR rather than about the ApplyCommit that ran before it.
func TestApplyCommitRefusesToOverwriteTheEpochThisClientStagedAndClearErasesIt(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	if _, err := receiver.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("the receiver's own CreateCommit: %v", err)
	}
	lost := receiver.stagedForTest()
	held := stagedEpochStorage(t, lost)

	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if err := receiver.ApplyCommit(processed); !errors.Is(err, ErrPendingCommitExists) {
		t.Fatalf("ApplyCommit over a live pending commit = %v, want ErrPendingCommitExists", err)
	}
	if receiver.stagedForTest() != lost {
		t.Fatal("the refused ApplyCommit replaced the staged commit anyway")
	}
	for name, secret := range held {
		if len(secret) != 0 && allZero(secret) {
			t.Fatalf("the refused ApplyCommit erased %s; a refusal must leave the caller's epoch whole", name)
		}
	}

	// the caller's repair, which is the drop site the erase discipline is stated at
	receiver.ClearPendingCommit()
	requireErased(t, "ClearPendingCommit", held)
	if !lost.secretTree.erased {
		t.Error("the lost epoch's secret tree is not marked erased, so every method of it still answers out of zeros")
	}

	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit after the clear: %v", err)
	}
	// and the epoch the receiver actually entered is the peer's, which is the control that says the
	// erase above did not take the live one with it
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the receiver did not enter the epoch the commit it applied opened")
	}
}

// allZero is the reading requireErased makes, answered rather than reported, for the one case that
// needs the NEGATIVE of it: storage a refusal must have left alone.
func allZero(secret []byte) bool {
	for _, b := range secret {
		if b != 0 {
			return false
		}
	}
	return true
}

// TestApplyCommitRefusesAResultThatIsNotACommit is the caller's own mistake, answered rather than
// dereferenced: every arm of Processed is a pointer, and a body that read Commit without asking Kind
// would be a nil dereference inside a library.
func TestApplyCommitRefusesAResultThatIsNotACommit(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	message, err := committer.Protect(nil, []byte("an application message"))
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	processed, err := receiver.ProcessMessage(message)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	refused := map[string]*Processed{
		"nil":                   nil,
		"an application":        processed,
		"a kind with no commit": {Kind: ProcessedCommit},
	}
	for name, result := range refused {
		if err := receiver.ApplyCommit(result); !errors.Is(err, errApplyCommitNotACommit) {
			t.Errorf("ApplyCommit(%s) = %v, want errApplyCommitNotACommit", name, err)
		}
	}
	if receiver.Epoch() != committer.Epoch() {
		t.Fatal("a refused ApplyCommit moved the group")
	}
}

// TestProcessMessageRefusesEveryWireFormatButPrivateMessage is the profile door on the inbound path,
// over messages this package's own encoder really produces rather than over one somebody picked.
func TestProcessMessageRefusesEveryWireFormatButPrivateMessage(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	// a Welcome is a message this build really produces, and it is not one a group at an epoch
	// ingests: JoinFromWelcome takes it, and a receive path that accepted it here would be a second
	// door onto joining
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "carol"))
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal key package: %v", err)
	}
	add, err := committer.ProposeAdd(encoded)
	if err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	// delivered to the receiver, because the commit below names it by reference and the control at
	// the end of this case is the receiver processing that commit
	if _, err := receiver.ProcessMessage(add); err != nil {
		t.Fatalf("the receiver could not process the Add: %v", err)
	}
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if len(result.Welcome) == 0 {
		t.Fatal("this commit produced no Welcome, so the case below has nothing to hand over")
	}
	if _, err := receiver.ProcessMessage(result.Welcome); !errors.Is(err, errProcessWireFormat) {
		t.Fatalf("ProcessMessage(a Welcome) = %v, want errProcessWireFormat", err)
	}
	keyPackageMessage, err := MarshalMLSMessage(&MLSMessage{
		Version:    ProtocolVersionMls10,
		WireFormat: WireFormatKeyPackage,
		KeyPackage: kp,
	})
	if err != nil {
		t.Fatalf("frame a key package: %v", err)
	}
	if _, err := receiver.ProcessMessage(keyPackageMessage); !errors.Is(err, errProcessWireFormat) {
		t.Fatalf("ProcessMessage(a KeyPackage) = %v, want errProcessWireFormat", err)
	}
	// the live control: the commit of the same call, which IS a PrivateMessage, is accepted
	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage on the commit of the same call: %v", err)
	}
	processed.Commit.Zeroize()
}

// TestProtectAndUnprotectRoundTrip is the application arm of the receive path.
func TestProtectAndUnprotectRoundTrip(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	message, err := committer.Protect([]byte("aad"), []byte("hello"))
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	application, err := receiver.Unprotect(message)
	if err != nil {
		t.Fatalf("Unprotect: %v", err)
	}
	if string(application.Plaintext) != "hello" || string(application.AuthenticatedData) != "aad" {
		t.Fatalf("application = %+v", application)
	}
	if application.SenderLeaf != committer.OwnLeafIndex() {
		t.Fatalf("SenderLeaf = %d, want %d", application.SenderLeaf, committer.OwnLeafIndex())
	}
	// the plaintext is not on the wire in the clear, which is the one thing a round trip alone
	// agrees with under a Protect that sealed nothing
	if bytes.Contains(message, []byte("hello")) {
		t.Fatal("the protected message carries its plaintext")
	}
	// and Unprotect refuses a handshake message rather than answering an empty application
	handshake, err := committer.ProposeUpdate()
	if err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	if _, err := receiver.Unprotect(handshake); !errors.Is(err, errApplicationMustBeCiphertext) {
		t.Fatalf("Unprotect(a proposal) = %v, want errApplicationMustBeCiphertext", err)
	}
}

// TestTheSignatureKeyResolverAnswersOnlyForAMemberSenderAtAnOccupiedLeaf drives the door every
// inbound message's signature is verified against, in all three directions.
//
// The resolver is a DECLARATION for this case's sake: OpenPrivateMessage builds the Sender it
// resolves out of section 6.3's sender data, which carries a leaf index and nothing else, so every
// sender that reaches it through the one wire format this profile ingests is a member sender. The
// non-member arm is therefore unreachable from any octets, and it is kept because it is the
// fail-CLOSED half -- CheckSenderLeaf answers nil for a sender type that carries no leaf index at
// all, so a resolver without this arm would hand back leaf 0's key for an external sender.
func TestTheSignatureKeyResolverAnswersOnlyForAMemberSenderAtAnOccupiedLeaf(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()
	resolve := receiver.signatureKeyResolverLocked()

	// the live control: the committer's own leaf answers the committer's own signature key
	key, err := resolve(Sender{SenderType: SenderTypeMember, LeafIndex: committer.OwnLeafIndex()})
	if err != nil {
		t.Fatalf("the resolver refused a member sender at an occupied leaf: %v", err)
	}
	own := committer.OwnLeafNodeCopy()
	if own == nil {
		t.Fatal("the committer holds no leaf of its own")
	}
	if !bytes.Equal(key, own.SignatureKey) {
		t.Fatal("the resolver answered a key that is not the one standing at that leaf")
	}
	// and it is a COPY: a caller writing through it would be writing into the tree this epoch's
	// tree hash was taken over
	if len(key) != 0 && &key[0] == &receiver.tree.Leaf(committer.OwnLeafIndex()).SignatureKey[0] {
		t.Fatal("the resolver hands out the ratchet tree's own array")
	}

	for _, senderType := range []SenderType{SenderTypeExternal, SenderTypeNewMemberProposal,
		SenderTypeNewMemberCommit} {
		if _, err := resolve(Sender{SenderType: senderType}); !errors.Is(err, errProcessSenderType) {
			t.Errorf("the resolver answered sender type %d with %v, want errProcessSenderType",
				senderType, err)
		}
	}
	// and ValSem004: a member sender naming a leaf this tree holds nobody at
	beyond := LeafIndex(len(receiver.Members()) + 4)
	if _, err := resolve(Sender{SenderType: SenderTypeMember, LeafIndex: beyond}); !errors.Is(err, errBlankSenderLeaf) {
		t.Errorf("the resolver answered a member sender at leaf %d with %v, want errBlankSenderLeaf",
			beyond, err)
	}
}

// TestProcessingAContentTypeOutsideTheRegistryIsRefused is the arm of the receive path's select
// that an octet no decoder of this package would produce reaches.
//
// It is asked of the DECLARATION rather than through ProcessMessage, because the codec refuses
// every content_type outside RFC 9420 section 6's three before a framed content exists at all --
// so this refusal cannot be reached from any message, and a test that tried would be observing the
// decoder. framing_test.go's code point gate sweeps the same body over every undeclared value of
// the registry; this names the sentinel.
func TestProcessingAContentTypeOutsideTheRegistryIsRefused(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	content := &AuthenticatedContent{
		Content: FramedContent{
			GroupId:     receiver.GroupId(),
			Epoch:       receiver.Epoch(),
			Sender:      Sender{SenderType: SenderTypeMember, LeafIndex: receiver.OwnLeafIndex()},
			ContentType: ContentTypeApplication,
		},
	}
	// the live control: the same value under a content type the registry DOES declare is processed
	if _, err := receiver.processAuthenticatedLocked(content, 0); err != nil {
		t.Fatalf("the control content was refused: %v", err)
	}
	content.Content.ContentType = ContentType(0xfe)
	if _, err := receiver.processAuthenticatedLocked(content, 0); !errors.Is(err, errProcessContentType) {
		t.Fatalf("a framed content of type 0xfe was refused with %v, want errProcessContentType", err)
	}
}

// TestStagingACommitCarryingNoCommitIsRefused is the same shape one door in: a content type of
// commit whose commit arm is empty.
//
// The codec pairs the two, so this is a value no message can carry and the refusal is this build
// disagreeing with itself. It is stated rather than dereferenced because the alternative is a nil
// dereference inside a library's receive path, and it is named here so that the refusal is one a
// test has read.
func TestStagingACommitCarryingNoCommitIsRefused(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	// the live control: a real commit content stages
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	authenticated := testOpenInboundCommit(t, receiver, result.Commit)
	staged, err := receiver.stageInboundCommitLocked(authenticated)
	if err != nil {
		t.Fatalf("the control commit does not stage, so the refusal below would say nothing: %v", err)
	}
	staged.Zeroize()

	authenticated.Content.Commit = nil
	if _, err := receiver.stageInboundCommitLocked(authenticated); !errors.Is(err, errCommitContentCarriesNoCommit) {
		t.Fatalf("a commit content with no commit was refused with %v, want errCommitContentCarriesNoCommit", err)
	}
}

// TestProcessCommitRefusesAnUpdateWhosePrivateHalfTheStoreLost is the other half of the leaf key
// swap: the commit installs a leaf key at this client's own position and the client cannot produce
// its private half.
//
// A REFUSAL AND NOT A CARRY-FORWARD, which is the whole point of the case. Keeping the leaf key of
// the epoch that just closed would let the merge succeed and leave a member that decrypts NOTHING
// for the rest of the group's life -- every update path sealed to the key it published, opened with
// a key that is not its private half, reported at the far end as a corrupt commit from a member
// that did everything right.
func TestProcessCommitRefusesAnUpdateWhosePrivateHalfTheStoreLost(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	update, err := receiver.ProposeUpdate()
	if err != nil {
		t.Fatalf("ProposeUpdate: %v", err)
	}
	cached := receiver.pendingProposalsForTest()
	if len(cached) != 1 || cached[0].Proposal.Update == nil {
		t.Fatalf("the proposer holds %d cached proposal(s) and this case needs its own update", len(cached))
	}
	published := cached[0].Proposal.Update.LeafNode.EncryptionKey
	if _, err := receiver.store.GetPrivateKey(published); err != nil {
		t.Fatalf("the private half of the key this update published is not in the store to begin with: %v", err)
	}
	if _, err := committer.ProcessMessage(update); err != nil {
		t.Fatalf("the committer could not process the Update: %v", err)
	}
	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}

	// the device's keyring loses the entry between the proposal and the commit that carries it
	if err := receiver.store.DeletePrivateKey(published); err != nil {
		t.Fatalf("DeletePrivateKey: %v", err)
	}
	if _, err := receiver.ProcessMessage(result.Commit); !errors.Is(err, errUpdatedLeafPrivateKey) {
		t.Fatalf("a commit installing a leaf key this client cannot open was refused with %v, want errUpdatedLeafPrivateKey",
			err)
	}
	// and the group is where it was: a refused commit moves nothing
	if receiver.Epoch() != committer.Epoch() {
		t.Fatalf("the refused commit left the receiver at epoch %d and the committer at %d",
			receiver.Epoch(), committer.Epoch())
	}
}

// TestTwoConsecutiveCommitsKeepBothMembersOnOneTranscript is the assertion one commit cannot make.
//
// The confirmed transcript hash of an epoch is taken over the INTERIM hash of the epoch before it,
// so a receiver that derived the right epoch and then failed to advance its own transcript agrees
// with the committer about everything -- the tree, the key schedule, the epoch authenticator -- and
// disagrees about the NEXT commit, at which point its confirmation tag no longer verifies and the
// group is forked with no way back. A single commit is satisfied by a receive path that never
// touches the transcript at all.
//
// It also runs the commit in BOTH directions, which is the second thing one commit cannot show: a
// receiver that staged an epoch correctly and installed the wrong secret tree or the wrong leaf
// private state opens the next commit from its peer and cannot make one of its own.
func TestTwoConsecutiveCommitsKeepBothMembersOnOneTranscript(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	first, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the first CreateCommit: %v", err)
	}
	processed, err := receiver.ProcessMessage(first.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage on the first commit: %v", err)
	}
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit on the first commit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit on the first commit: %v", err)
	}
	// the live control: after one commit the two agree, which is what makes a disagreement after
	// the second a statement about the transcript rather than about the first commit
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two members disagree after the first commit, so the second observes nothing")
	}

	// and the SECOND commit comes from the other member, so the epoch the first one opened is
	// exercised as a sending epoch and not only as a receiving one
	second, err := receiver.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the second CreateCommit, from the member that received the first: %v", err)
	}
	back, err := committer.ProcessMessage(second.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage on the second commit: %v; the receiver's transcript and the committer's have parted company", err)
	}
	if err := committer.ApplyCommit(back); err != nil {
		t.Fatalf("ApplyCommit on the second commit: %v", err)
	}
	if err := receiver.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit on the second commit: %v", err)
	}
	if receiver.Epoch() != committer.Epoch() {
		t.Fatalf("receiver epoch %d, committer epoch %d", receiver.Epoch(), committer.Epoch())
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two members disagree on the epoch the second commit opened")
	}
	// and the epoch two commits produced carries traffic, which is what says the secret tree and
	// the leaf private state each of them installed are the ones the other computed
	message, err := committer.Protect([]byte("aad"), []byte("two commits on"))
	if err != nil {
		t.Fatalf("Protect two commits on: %v", err)
	}
	opened, err := receiver.Unprotect(message)
	if err != nil {
		t.Fatalf("Unprotect two commits on: %v", err)
	}
	if string(opened.Plaintext) != "two commits on" {
		t.Fatalf("the epoch two commits opened carried %q", opened.Plaintext)
	}
}

// TestProcessCommitThatAddsAMemberAndCarriesAPath is the receiving half of RFC 9420 section
// 12.4.1's add exclusion, and it is the one shape that can observe it.
//
// A member this commit ADDS receives the path secret in its Welcome and is sealed to nowhere in the
// update path, so the committer takes the added leaves out of every target resolution before it
// encrypts. A receiver that did not take the same leaves out computes a different resolution for at
// least one node of the path, which shifts the POSITIONAL pairing between a node's ciphertext
// vector and the members it addresses -- and every ciphertext is still well formed, still the right
// length, and opens to the wrong subtree or to nothing.
//
// An add-only commit is not enough: section 12.4 does not require a path for one, so the decrypt
// this case is about never runs. Force is what makes the commit carry the path AND the add
// together, which is the only commit in which the exclusion is a decision at all.
func TestProcessCommitThatAddsAMemberAndCarriesAPath(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	carol := testIdentity(t, crypto, "carol")
	kp, _, _ := testKeyPackage(t, crypto, carol)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal key package: %v", err)
	}
	add, err := committer.ProposeAdd(encoded)
	if err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	if _, err := receiver.ProcessMessage(add); err != nil {
		t.Fatalf("the receiver could not process the Add: %v", err)
	}
	result, err := committer.CreateCommit(nil, nil, &CommitOptions{Force: true})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	// the two live controls, without either of which this case observes nothing: the commit must
	// carry a path, and it must add somebody for there to be an exclusion to make
	staged := committer.stagedForTest()
	if staged == nil || !staged.hasPath {
		t.Fatal("this commit carries no update path, so the exclusion below is not a decision")
	}
	if len(staged.AddedLeaves()) != 1 {
		t.Fatalf("this commit adds %d leaves, want 1", len(staged.AddedLeaves()))
	}

	processed, err := receiver.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("ProcessMessage on a commit that adds a member and carries a path: %v", err)
	}
	if err := receiver.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := committer.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if !bytes.Equal(receiver.EpochAuthenticator(), committer.EpochAuthenticator()) {
		t.Fatal("the two members disagree on the epoch a commit that added a member and carried a path opened")
	}
	if len(receiver.Members()) != 3 {
		t.Fatalf("the receiver sees %d members, want 3", len(receiver.Members()))
	}
}

// TestProcessCommitRunsSection122sProposalRulesOverAnInboundList is the receive path's own copy of
// the door (*Group).propose runs at generation.
//
// The commit below is one this package will not build through its ordinary surface: ValSem101
// refuses an Add whose key package publishes a signature key the group already holds, and every
// generator here asks that rule before it signs. CommitOptions' skipValidation seam is what makes a
// commit that carries one, and it is unexported so that only this package can.
//
// ValSem101 is the isolating choice and not the first rule to hand: it is a rule of section 12.2
// and it is NOT one of ValidateCommit's twelve, so a receive path that ran the commit validator and
// skipped the proposal list validator accepts this commit -- an epoch in which one member's
// signature key stands at two leaves, which is a group nobody can attribute a message in.
func TestProcessCommitRunsSection122sProposalRulesOverAnInboundList(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, bob := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	// a SECOND key package for the member that is already at leaf 1: fresh HPKE keys, the same
	// signature key, which is exactly what ValSem101 is stated over
	duplicate, _, _ := testKeyPackage(t, crypto, bob)
	forged, err := committer.CreateCommit(nil,
		[]Proposal{{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *duplicate}}},
		&CommitOptions{skipValidation: true})
	if err != nil {
		t.Fatalf("build the commit whose list this package would not validate: %v", err)
	}
	if _, err := receiver.ProcessMessage(forged.Commit); !errors.Is(err, ErrAddDuplicateSignatureKey) {
		t.Fatalf("a commit adding a signature key the group already holds was refused with %v, want ErrAddDuplicateSignatureKey",
			err)
	}
	// and the live control: the same seam with a member the group does NOT hold is accepted, so
	// what the refusal above observes is the rule and not the seam
	committer.ClearPendingCommit()
	carol := testIdentity(t, crypto, "carol")
	fresh, _, _ := testKeyPackage(t, crypto, carol)
	honest, err := committer.CreateCommit(nil,
		[]Proposal{{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *fresh}}},
		&CommitOptions{skipValidation: true})
	if err != nil {
		t.Fatalf("build the control commit: %v", err)
	}
	processed, err := receiver.ProcessMessage(honest.Commit)
	if err != nil {
		t.Fatalf("the control commit was refused: %v", err)
	}
	processed.Commit.Zeroize()
}

// TestStagingAnInboundCommitAsksSection124sOwnRules is ValidateCommit on the receive path, asked
// through the one rule of its twelve that no other door of this path repeats.
//
// EVERY OTHER RULE IT STATES HAS A SECOND ENFORCER HERE, which is why this case exists in the shape
// it does rather than damaging any field and asserting a refusal. A path leaf of the wrong source
// or with a broken signature is refused by ValidateUpdatePathLeafNode; a path of the wrong length or
// republishing a key already in the tree is refused by MergeUpdatePath's own length check and by its
// parent hash chain; a list that section 12.2 forbids is refused by ValidateProposalList. So a
// commit damaged in any of those ways is refused whether or not this call is made, and a case built
// on one would report a validator that is not running.
//
// ValSem201 is the exception: a commit naming NO proposals must carry an update path, and a receive
// path without this call simply derives the epoch with a zero commit secret -- which is refused two
// steps later by the confirmation tag, under a different sentinel and after a whole epoch has been
// derived over a secret every member of the previous epoch already holds. The assertion is on the
// SENTINEL for exactly that reason: err != nil is satisfied by the tag.
func TestStagingAnInboundCommitAsksSection124sOwnRules(t *testing.T) {
	crypto := testCrypto(t)
	committer, receiver, _, _ := testTwoMemberGroup(t, crypto)
	defer committer.Close()
	defer receiver.Close()

	result, err := committer.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	authenticated := testOpenInboundCommit(t, receiver, result.Commit)
	if authenticated.Content.Commit == nil || authenticated.Content.Commit.Path == nil {
		t.Fatal("this commit carries no update path, so taking one away below changes nothing")
	}
	if len(authenticated.Content.Commit.Proposals) != 0 {
		t.Fatalf("this commit names %d proposals; ValSem201's empty clause is what this case is stated over",
			len(authenticated.Content.Commit.Proposals))
	}

	// the live control: unedited, this content stages
	staged, err := receiver.stageInboundCommitLocked(authenticated)
	if err != nil {
		t.Fatalf("the unedited commit does not stage, so the refusal below would say nothing: %v", err)
	}
	staged.Zeroize()

	authenticated.Content.Commit.Path = nil
	if _, err := receiver.stageInboundCommitLocked(authenticated); !errors.Is(err, errMissingPath) {
		t.Fatalf("a commit naming no proposals and carrying no update path was refused with %v, want errMissingPath; without section 12.4's own validator the refusal is the confirmation tag's, two steps and one derived epoch later",
			err)
	}
}
