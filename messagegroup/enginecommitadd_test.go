// GroupHandle.CommitAdd, the twenty seventh method, and the two properties it exists for.
//
// WHAT THE BY-REFERENCE ARM CANNOT DO. Commit names proposals by reference and a reference resolves
// against the RECEIVER's proposal cache, so a member that never received the proposal refuses the
// commit -- (*mls.ProposalCache).Resolve's "proposal reference is not cached for this epoch". The
// ProposeAdd-then-Commit pair therefore works only while every member is handed every proposal
// before the commit, which is one record per proposal per member on top of the commit itself, and
// assembling N members that way is N proposals fanned to a growing group: Θ(N²) records.
//
// SO THE TWO CASES THAT MATTER ARE THE ONES THE OLD ARM FAILS.
// TestCommitAddIsProcessedByAMemberThatNeverSawAProposal is the first, and it carries its own
// control: the same receiver, the same shape, through Commit over a proposal it was never handed,
// refused with exactly the sentence this file names. TestOneCommitAddAdmitsThreeMembers is the
// second, and it is what makes adds batchable -- one commit, one Welcome three joiners open, and
// every one of the five members exporting the same epoch secret.
package messagegroup

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// commitAddChain is a founder and a joiner already in one group at epoch 1, built through the
// seam and nothing else: NewKeyPackage, CommitAdd, MergePendingCommit, JoinFromWelcome.
type commitAddChain struct {
	founder *testEngine
	joiner  *testEngine
	founded GroupHandle
	joined  GroupHandle
}

func newCommitAddChain(t *testing.T, name string) *commitAddChain {
	t.Helper()
	founder := newTestEngine(t)
	joiner := newTestEngine(t)
	keyPackage, err := joiner.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the joiner's NewKeyPackage: %v", err)
	}
	founded := founder.createGroup(t, name)
	// THE FOUNDING ADD GOES THROUGH THE ARM UNDER TEST, so that a chain built on it is a chain
	// that never had a proposal in any cache.
	_, welcome, ratchetTree, err := founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd over one key package: %v", err)
	}
	if err := founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	joined, err := joiner.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("JoinFromWelcome over a by-value add's Welcome: %v", err)
	}
	if founded.Epoch() != 1 || joined.Epoch() != 1 {
		t.Fatalf("the chain stands at epochs %d and %d, want 1 and 1", founded.Epoch(), joined.Epoch())
	}
	chain := &commitAddChain{founder: founder, joiner: joiner, founded: founded, joined: joined}
	t.Cleanup(func() {
		joined.Close()
		founded.Close()
	})
	return chain
}

// assertSameEpochSecret is the assertion an epoch counter cannot make: two handles that agree on
// the number and disagree on the schedule are two members every peer refuses.
func assertSameEpochSecret(t *testing.T, epoch uint64, handles map[string]GroupHandle) {
	t.Helper()
	var reference []byte
	var referenceName string
	for name, handle := range handles {
		if got := handle.Epoch(); got != epoch {
			t.Fatalf("%s stands at epoch %d, want %d", name, got, epoch)
		}
		secret, err := handle.Export("URmessage/v1/storage", nil, 32)
		if err != nil {
			t.Fatalf("%s's exporter at epoch %d: %v", name, epoch, err)
		}
		if reference == nil {
			reference, referenceName = secret, name
			continue
		}
		if !bytes.Equal(reference, secret) {
			t.Fatalf("%s exports %x at epoch %d and %s exports %x; every key of the record layer is a function of this value",
				referenceName, reference, epoch, name, secret)
		}
	}
}

// TestCommitAddIsProcessedByAMemberThatNeverSawAProposal is the property: a receiver with an empty
// proposal cache processes the commit and follows it into the next epoch.
//
// THE CONTROL IS THE SAME RECEIVER UNDER THE OLD ARM, and it is not a second test because the two
// halves are one claim. Without it, a build in which Process had quietly stopped consulting the
// cache -- accepting every reference -- would pass the property while making the property
// meaningless; the control is what says the cache is still consulted and the by-value arm is what
// gets past it.
func TestCommitAddIsProcessedByAMemberThatNeverSawAProposal(t *testing.T) {
	chain := newCommitAddChain(t, "commitadd-never-saw")
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the third member's NewKeyPackage: %v", err)
	}

	// THE CONTROL, FIRST: the founder proposes and commits by reference, and the joiner -- who
	// was never handed the proposal -- refuses the commit for exactly the reason this arm exists.
	if _, err := chain.founded.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	byReference, _, _, err := chain.founded.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) over the cached add: %v", err)
	}
	if _, err := chain.joined.Process(byReference); err == nil {
		t.Fatal("a member that never saw the proposal processed a by-reference commit naming it; the cache is no longer consulted and the property below observes nothing")
	} else if !strings.Contains(err.Error(), "not cached") {
		t.Fatalf("the control refused with %v, want the proposal cache's own refusal", err)
	}
	// the founder drops the staged epoch-2 candidate, so the arm under test starts from epoch 1
	// with the same receiver. THE PROPOSAL STAYS IN THE FOUNDER'S CACHE, which is the point of
	// the empty-not-nil vector CommitAdd passes: a nil would fold that cached proposal back in
	// and the joiner would refuse this commit for the control's reason.
	chain.founded.ClearPendingCommit()

	commit, welcome, ratchetTree, err := chain.founded.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("CommitAdd: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("a member that never saw a proposal refused the by-value commit: %v", err)
	}
	if processed.Kind != EngineProcessedCommit {
		t.Fatalf("Process discriminated the commit as kind %d, want %d", processed.Kind, EngineProcessedCommit)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("the committer's MergePendingCommit: %v", err)
	}
	admitted, err := third.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the third member's JoinFromWelcome: %v", err)
	}
	defer admitted.Close()
	assertSameEpochSecret(t, 2, map[string]GroupHandle{
		"the committer":                        chain.founded,
		"the member that never saw a proposal": chain.joined,
		"the member the commit admitted":       admitted,
	})
	if n := chain.joined.MemberCount(); n != 3 {
		t.Fatalf("the receiver sees %d members after the by-value add, want 3", n)
	}
}

// TestOneCommitAddAdmitsThreeMembers is what makes adds batchable: one commit, three by-value
// Adds, one Welcome that three joiners open, and five members on one schedule.
func TestOneCommitAddAdmitsThreeMembers(t *testing.T) {
	chain := newCommitAddChain(t, "commitadd-three")
	engines := []*testEngine{newTestEngine(t), newTestEngine(t), newTestEngine(t)}
	names := []string{"the first joiner", "the second joiner", "the third joiner"}
	keyPackages := [][]byte{}
	for i, engine := range engines {
		keyPackage, err := engine.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("joiner %d's NewKeyPackage: %v", i, err)
		}
		keyPackages = append(keyPackages, keyPackage)
	}

	commit, welcome, ratchetTree, err := chain.founded.CommitAdd(keyPackages)
	if err != nil {
		t.Fatalf("CommitAdd over three key packages: %v", err)
	}
	processed, err := chain.joined.Process(commit)
	if err != nil {
		t.Fatalf("the existing member's Process over a three-add commit: %v", err)
	}
	if err := chain.joined.ApplyCommit(processed); err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	if err := chain.founded.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	handles := map[string]GroupHandle{
		"the committer":       chain.founded,
		"the existing member": chain.joined,
	}
	for i, engine := range engines {
		admitted, err := engine.engine.JoinFromWelcome(welcome, ratchetTree)
		if err != nil {
			t.Fatalf("%s's JoinFromWelcome off the one Welcome: %v", names[i], err)
		}
		defer admitted.Close()
		handles[names[i]] = admitted
	}
	assertSameEpochSecret(t, 2, handles)
	for name, handle := range handles {
		if n := handle.MemberCount(); n != 5 {
			t.Fatalf("%s sees %d members, want 5", name, n)
		}
	}
}

// TestCommitAddRefusesWhatItCannotAdmitBeforeAnythingIsStaged is the adapter's refusals, each
// carrying its sentinel, followed by the control that says the group was left where it stood: a
// CommitAdd that succeeds afterwards, which ErrPendingCommitExists would refuse had any refusal
// staged something.
func TestCommitAddRefusesWhatItCannotAdmitBeforeAnythingIsStaged(t *testing.T) {
	chain := newCommitAddChain(t, "commitadd-refuses")
	third := newTestEngine(t)
	keyPackage, err := third.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("NewKeyPackage: %v", err)
	}

	if _, _, _, err := chain.founded.CommitAdd(nil); !errors.Is(err, ErrEngineCommitAddEmpty) {
		t.Errorf("CommitAdd(nil) answered %v, want ErrEngineCommitAddEmpty", err)
	}
	if _, _, _, err := chain.founded.CommitAdd([][]byte{}); !errors.Is(err, ErrEngineCommitAddEmpty) {
		t.Errorf("CommitAdd of an empty vector answered %v, want ErrEngineCommitAddEmpty", err)
	}
	// a package that does not decode, IN SECOND POSITION, so the refusal has an index to name
	// and a first package it had already accepted.
	_, _, _, err = chain.founded.CommitAdd([][]byte{keyPackage, []byte("not a key package")})
	if !errors.Is(err, ErrEngineCommitAddKeyPackage) {
		t.Errorf("CommitAdd over octets that do not decode answered %v, want ErrEngineCommitAddKeyPackage", err)
	} else if !strings.Contains(err.Error(), "key package 1 of 2") {
		t.Errorf("the refusal does not name which package: %v", err)
	}
	// a package whose leaf carries no urmessage_leaf_keys: a real key package, signed by a real
	// device signer, that section 10.1 accepts and this profile's ProposeAdd refuses.
	withoutLeafKeys := commitAddKeyPackageWithoutLeafKeys(t, third)
	_, _, _, err = chain.founded.CommitAdd([][]byte{withoutLeafKeys})
	if !errors.Is(err, ErrEngineCommitAddKeyPackage) || !errors.Is(err, mls.ErrMalformedExtension) {
		t.Errorf("CommitAdd over a leaf with no urmessage_leaf_keys answered %v, want ErrEngineCommitAddKeyPackage wrapping mls.ErrMalformedExtension", err)
	}

	// THE CONTROL: nothing above staged a commit.
	if _, _, _, err := chain.founded.CommitAdd([][]byte{keyPackage}); err != nil {
		t.Fatalf("CommitAdd after four refusals: %v; a refusal left a staged commit behind", err)
	}
	chain.founded.ClearPendingCommit()
}

// commitAddKeyPackageWithoutLeafKeys is a key package signed by the device's own signer whose leaf
// carries NO urmessage_leaf_keys extension. It is built the way the engine builds one, minus the
// extension, so the only thing the adapter can refuse it for is the thing under test.
func commitAddKeyPackageWithoutLeafKeys(t *testing.T, device *testEngine) []byte {
	t.Helper()
	keyPackage, initPrivate, encryptPrivate, err := mls.NewKeyPackageWithSigner(device.crypto,
		device.crypto.Suite(), device.signer, mls.BasicCredential(device.identityPub),
		engineCapabilities(), nil)
	if err != nil {
		t.Fatalf("a key package with no leaf keys: %v", err)
	}
	defer keyPackage.Zeroize()
	defer zeroize(initPrivate)
	defer zeroize(encryptPrivate)
	encoded, err := syntax.Marshal(keyPackage)
	if err != nil {
		t.Fatalf("encode it: %v", err)
	}
	return encoded
}
