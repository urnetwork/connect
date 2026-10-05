// Ledger item 251's ruling 40: pq_secret is a value PER EPOCH and not a scalar on the session.
//
// The line the ruling names is pastepoch.go's `root := StorageRoot(mlsSecret, self.pqSecret)`,
// and what is wrong with it is not visible in any group that exists today, because every group
// that exists today was handed the same octets at every AdvanceEpoch. The first case below is the
// reproduction: the same open, the same record, the same loader, with ONE thing changed -- the
// secret the session was handed at the epoch change -- and the root the session derives for the
// epoch it is opening moves with it.
package messagegroup

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/message"
)

// rotatedTestPqSecret is pq_secret at the epoch a rotation opens: thirty two octets that are not
// testPqSecret's and are not a shift of them, so a derivation that mixed the two up produces
// neither.
func rotatedTestPqSecret() []byte {
	secret := make([]byte, PqSecretBytes)
	for i := range secret {
		secret[i] = byte(0x5A ^ (i * 7))
	}
	return secret
}

// advanceOpenerWith is advanceOpener with the secret the caller chooses: the opener commits
// alone, merges, and installs the epoch that commit opened with THAT pq_secret.
func (self *pastEpochPair) advanceOpenerWith(t *testing.T, pqSecret []byte) {
	t.Helper()
	if _, _, _, err := self.chain.joined.Commit(nil); err != nil {
		t.Fatalf("the opener's Commit(nil): %v", err)
	}
	if err := self.chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("the opener's MergePendingCommit: %v", err)
	}
	if err := self.opener.AdvanceEpoch(pqSecret); err != nil {
		t.Fatalf("the opener's AdvanceEpoch: %v", err)
	}
}

// pastEpochClassKeysAt reads one prior epoch's class keys off the loop, so the read is not a race.
func pastEpochClassKeysAt(t *testing.T, session *GroupSession, epoch uint64) *ClassKeys {
	t.Helper()
	var keys *ClassKeys
	if err := session.do(func() {
		if past, isHeld := session.pastEpochs[epoch]; isHeld {
			keys = past.classKeys
		}
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	return keys
}

// classKeysEqual is the whole of a ClassKeys compared, so a case cannot pass on one third of it.
func classKeysEqual(a *ClassKeys, b *ClassKeys) bool {
	if a == nil || b == nil {
		return a == b
	}
	return bytes.Equal(a.Perm, b.Perm) && bytes.Equal(a.Durable, b.Durable) &&
		bytes.Equal(a.Media, b.Media)
}

// THE REPRODUCTION, AND THE PROPERTY. A past epoch's storage root is derived from THAT EPOCH'S
// pq_secret. The control is inline and it is the pre-rotation value: the same open at a session
// that was never handed a second secret must land on the same root, and the two candidate roots
// must differ, or the assertion below is a photograph of one number taken twice.
//
// Before the per-epoch table this case is RED in both halves: the epoch-one record does not open
// at all, and the class keys the session built for epoch one are the ones derived from the secret
// the ROTATION handed it -- which is the defect ruling 40 names, measured rather than argued.
func TestAPastEpochsRootIsDerivedFromThatEpochsPqSecretAndNotTodays(t *testing.T) {
	pair := newPastEpochPair(t, "pq-per-epoch-root")
	record := pair.sealDurable(t, "sealed at epoch one")

	founding := pair.chain.pqSecret
	rotated := rotatedTestPqSecret()
	if bytes.Equal(founding, rotated) {
		t.Fatal("the two secrets this case rotates between are equal, so nothing below can tell a per-epoch table from a scalar")
	}
	// mls_secret[1], taken from the handle while it is still at epoch one. Both candidate roots
	// are built from it, so the only thing that separates them is the pq_secret.
	mlsSecret, err := pair.chain.joined.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("the opener's epoch-one Export: %v", err)
	}
	wanted := DeriveClassKeys(StorageRoot(mlsSecret, founding))
	defective := DeriveClassKeys(StorageRoot(mlsSecret, rotated))
	// THE INLINE CONTROL: the two candidates are different values. Without this the equality
	// below holds for a session that ignored the rotation and for one that honoured it alike.
	if classKeysEqual(wanted, defective) {
		t.Fatal("the epoch-one class keys derived from the founding secret and from the rotated one are EQUAL, so this case cannot tell which secret the session used")
	}

	pair.advanceOpenerWith(t, rotated)
	if epoch, _ := pair.opener.Epoch(); epoch != 2 {
		t.Fatalf("the opener is at epoch %d after its own commit, want 2", epoch)
	}
	pair.trackAt(t, 1, 0)

	// the behavioural half: the record sealed at epoch one under the founding secret opens.
	head, body, err := pair.opener.OpenRecord(record)
	if err != nil {
		t.Fatalf("an epoch-one record did not open at a session that rotated its pq_secret at epoch two: %v", err)
	}
	if string(head) != "head" || string(body) != "sealed at epoch one" {
		t.Errorf("the epoch-one record opened to %q / %q", head, body)
	}

	// the structural half: the root the session built for epoch one is epoch one's.
	built := pastEpochClassKeysAt(t, pair.opener, 1)
	if built == nil {
		t.Fatal("the session holds no schedule for epoch one after opening a record at it")
	}
	if classKeysEqual(built, defective) {
		t.Errorf("the session derived epoch one's class keys from the secret the ROTATION handed it: a past epoch's root is being re-derived from today's pq_secret, which is ledger item 251 ruling 40's line")
	}
	if !classKeysEqual(built, wanted) {
		t.Errorf("the session derived epoch one's class keys from neither the founding secret nor the rotated one; the derivation has moved off StorageRoot(mls_secret[n], pq_secret[n])")
	}
}

// AND THE SESSION THAT WAS NEVER ROTATED IS UNCHANGED, which is every group that exists today.
//
// This is the same case with the SAME secret at the epoch change, asserted against the same two
// derivations -- so the pair fails apart: the case above goes red if the table is not consulted,
// and this one goes red if the compatibility path stops answering.
func TestASessionHandedOneSecretForeverDerivesExactlyWhatItDerivedBefore(t *testing.T) {
	pair := newPastEpochPair(t, "pq-per-epoch-single")
	record := pair.sealDurable(t, "sealed at epoch one")
	founding := pair.chain.pqSecret
	mlsSecret, err := pair.chain.joined.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("the opener's epoch-one Export: %v", err)
	}
	wanted := DeriveClassKeys(StorageRoot(mlsSecret, founding))
	// the inline control, the other way round: a session handed one secret forever must NOT be
	// reading some other epoch's, and the rotated value is what "some other epoch's" would be.
	other := DeriveClassKeys(StorageRoot(mlsSecret, rotatedTestPqSecret()))
	if classKeysEqual(wanted, other) {
		t.Fatal("the two candidate class key sets are equal, so this case distinguishes nothing")
	}

	pair.advanceOpenerWith(t, founding)
	pair.trackAt(t, 1, 0)
	if _, body, err := pair.opener.OpenRecord(record); err != nil || string(body) != "sealed at epoch one" {
		t.Fatalf("the epoch-one record answered %v / %q at a session that was never rotated", err, body)
	}
	built := pastEpochClassKeysAt(t, pair.opener, 1)
	if built == nil {
		t.Fatal("the session holds no schedule for epoch one after opening a record at it")
	}
	if !classKeysEqual(built, wanted) {
		t.Errorf("a session handed one secret forever no longer derives StorageRoot(mls_secret[1], pq_secret); the single-secret path has changed behaviour")
	}
	if classKeysEqual(built, other) {
		t.Errorf("a session handed one secret forever derived epoch one's keys from a secret nobody handed it")
	}
}

// THE RESTART, which is the shape the compatibility path exists for and the one no round trip in
// this package had ever driven: a session constructed AT A LATER EPOCH, which therefore holds no
// entry for the epoch the record it is opening was sealed at.
//
// A device that restarts opens its group out of durable storage at whatever epoch the group has
// reached; before this change the scalar answered every epoch in the window, and a table keyed by
// epoch answers none of them. The rule that keeps the restart working is stated in pqsecret.go
// and it is narrow: a session that has never been handed a SECOND secret answers every epoch with
// the one it has. A session that HAS been rotated refuses instead, because the alternative is the
// defect above wearing a different hat.
func TestARestartedSessionOpensAnEpochItNeverStoodAtWhileItHoldsOneSecret(t *testing.T) {
	pair := newPastEpochPair(t, "pq-per-epoch-restart")
	record := pair.sealDurable(t, "sealed at epoch one")
	groupId := pair.chain.joined.GroupId()
	engine := pair.chain.b.engine

	// the opener's own MLS group walks forward four epochs with NO session watching, which is
	// what a device that was offline comes back to.
	for range 4 {
		if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
			t.Fatalf("the opener's Commit(nil): %v", err)
		}
		if err := pair.chain.joined.MergePendingCommit(); err != nil {
			t.Fatalf("the opener's MergePendingCommit: %v", err)
		}
	}
	if epoch := pair.chain.joined.Epoch(); epoch != 5 {
		t.Fatalf("the opener's handle is at epoch %d after four commits, want 5", epoch)
	}

	restarted, err := NewGroupSession(pair.chain.joined, pair.chain.pqSecret, pair.chain.groupHandleKey,
		newStreamIndexMemory(), testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("the restarted session at epoch 5: %v", err)
	}
	loads := map[uint64]int{}
	if err := restarted.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		loads[epoch] += 1
		return engine.LoadGroup(groupId, epoch)
	}); err != nil {
		t.Fatalf("InstallPastEpochLoader: %v", err)
	}

	// AND THEN IT ADVANCES TWICE WITH THE SAME SECRET, which is what makes this case tell the
	// rule apart from a cheaper one. A session that decided "have I rotated" by COUNTING its
	// advances rather than by comparing the octets would be rotated here -- two AdvanceEpoch
	// calls -- and would then refuse epoch one, which is every group in the world losing its
	// history at its second commit. The rule is on the VALUE, and this is where that is
	// observable: without these two lines the mutant passes.
	for range 2 {
		if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
			t.Fatalf("the restarted session's Commit(nil): %v", err)
		}
		if err := pair.chain.joined.MergePendingCommit(); err != nil {
			t.Fatalf("the restarted session's MergePendingCommit: %v", err)
		}
		if err := restarted.AdvanceEpoch(pair.chain.pqSecret); err != nil {
			t.Fatalf("the restarted session's AdvanceEpoch: %v", err)
		}
	}
	if epoch, _ := restarted.Epoch(); epoch != 7 {
		t.Fatalf("the restarted session is at epoch %d after two advances, want 7", epoch)
	}
	// and it holds three entries -- 5, 6 and 7 -- and none for epoch one, which is the state the
	// compatibility path has to answer out of.
	var heldEpochs int
	var holdsEpochOne bool
	if err := restarted.do(func() {
		heldEpochs = len(restarted.pqSecrets)
		_, holdsEpochOne = restarted.pqSecrets[1]
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if heldEpochs != 3 || holdsEpochOne {
		t.Fatalf("the restarted session holds %d pq_secret(s) and an entry for epoch one: %t; want 3 and false, or the open below is not going through the compatibility path at all",
			heldEpochs, holdsEpochOne)
	}

	if err := restarted.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("the restarted session's TrackSenderAt(epoch 1): %v", err)
	}
	if _, body, err := restarted.OpenRecord(record); err != nil || string(body) != "sealed at epoch one" {
		t.Fatalf("a restarted session at epoch 7 answered %v / %q for a record sealed at epoch 1; the single-secret compatibility path is not answering an epoch the session never stood at", err, body)
	}
	if loads[1] != 1 {
		t.Errorf("epoch one was loaded %d time(s), want 1", loads[1])
	}
	// the session is NOT closed here: the handle it holds is the chain's, and the chain closes it.
}

// THE WINDOW MOVES AND THE ENTRIES BELOW IT ARE ERASED, not merely dropped.
//
// The array is ALIASED before the drop, for the reason every erase case in this package aliases:
// "all zero afterwards" read through the map the entry has already left is a photograph of
// nothing. The control is the entry exactly AT the window edge, read in the same pass: it is
// still held and still non-zero, so a case that erased everything would fail here.
func TestAPqSecretBelowTheWindowIsDroppedAndErasedAndTheOneAtTheEdgeIsNot(t *testing.T) {
	pair := newPastEpochPair(t, "pq-per-epoch-window")
	// a DIFFERENT secret at every epoch, so the entries are told apart by their octets.
	secretAt := func(epoch uint64) []byte {
		secret := make([]byte, PqSecretBytes)
		for i := range secret {
			secret[i] = byte(int(epoch)*13 + i + 1)
		}
		return secret
	}
	for epoch := uint64(2); epoch <= 1+PastEpochWindow; epoch += 1 {
		pair.advanceOpenerWith(t, secretAt(epoch))
	}
	if epoch, _ := pair.opener.Epoch(); epoch != 1+PastEpochWindow {
		t.Fatalf("the opener is at epoch %d, want %d", epoch, 1+PastEpochWindow)
	}

	// the aliases, read off the loop: epoch one's entry, which the NEXT advance puts below the
	// window, and epoch two's, which it does not.
	var below, edge []byte
	var held int
	if err := pair.opener.do(func() {
		below = pair.opener.pqSecrets[1]
		edge = pair.opener.pqSecrets[2]
		held = len(pair.opener.pqSecrets)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !containsNonZero(below) || !containsNonZero(edge) {
		t.Fatalf("epoch one's secret (%d octets) or epoch two's (%d octets) is empty or all zero before the drop, so an all-zero reading afterwards would say nothing",
			len(below), len(edge))
	}
	if held != int(1+PastEpochWindow) {
		t.Fatalf("the session holds %d pq_secret(s) at epoch %d, want %d -- one per epoch it has stood at, epochs 1..%d",
			held, 1+PastEpochWindow, 1+PastEpochWindow, 1+PastEpochWindow)
	}

	// one more epoch: epoch one is now PastEpochWindow+1 behind, which is below the line
	// pastEpochOnLoop refuses at, so the entry has nothing left to serve.
	pair.advanceOpenerWith(t, secretAt(2+PastEpochWindow))
	var stillHeld bool
	var edgeStillHeld bool
	if err := pair.opener.do(func() {
		_, stillHeld = pair.opener.pqSecrets[1]
		_, edgeStillHeld = pair.opener.pqSecrets[2]
		held = len(pair.opener.pqSecrets)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if stillHeld {
		t.Errorf("the session still holds pq_secret[1] at epoch %d, which is %d epochs behind and outside the window of %d",
			2+PastEpochWindow, 1+PastEpochWindow, PastEpochWindow)
	}
	if containsNonZero(below) {
		t.Errorf("pq_secret[1] was dropped from the table and its octets are still in the heap: a drop that does not erase leaves the post-quantum contribution to a retired epoch's storage root lying around")
	}
	// THE CONTROL, in the same pass: the entry one epoch newer is exactly at the window edge and
	// is neither dropped nor erased.
	if !edgeStillHeld {
		t.Errorf("the session dropped pq_secret[2] at epoch %d, and epoch two is exactly %d epochs behind -- the last epoch the window reaches",
			2+PastEpochWindow, PastEpochWindow)
	}
	if !containsNonZero(edge) {
		t.Errorf("pq_secret[2] was erased although it is exactly at the window edge; the bound is one epoch too tight")
	}
	if held != int(1+PastEpochWindow) {
		t.Errorf("the session holds %d pq_secret(s) at epoch %d, want %d: the window is a moving bound and not a growing table",
			held, 2+PastEpochWindow, 1+PastEpochWindow)
	}
}

// AND A ROTATED SESSION REFUSES AN EPOCH IT HOLDS NO SECRET FOR rather than reaching for the one
// it has. It is the compatibility path's complement, and it is the branch that must not be taken
// once a second secret has arrived.
func TestARotatedSessionRefusesAnEpochItHoldsNoSecretFor(t *testing.T) {
	pair := newPastEpochPair(t, "pq-per-epoch-refusal")
	record := pair.sealDurable(t, "sealed at epoch one")
	pair.advanceOpenerWith(t, rotatedTestPqSecret())

	// the entry for epoch one is removed by hand, which is the state a session reaches when it
	// was constructed above the epoch a record names and has since been rotated -- the one
	// arrangement in which neither the table nor the compatibility path can answer.
	if err := pair.opener.do(func() {
		zeroize(pair.opener.pqSecrets[1])
		delete(pair.opener.pqSecrets, 1)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if err := pair.opener.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0); !errors.Is(err, ErrPqSecretUnknownEpoch) {
		t.Errorf("a rotated session with no entry for epoch one answered %v, want ErrPqSecretUnknownEpoch; a session that answered with the secret it HAS would derive the wrong root and report nothing", err)
	}
	if _, _, err := pair.opener.OpenRecord(record); !errors.Is(err, ErrPqSecretUnknownEpoch) {
		t.Errorf("OpenRecord at an epoch this rotated session holds no secret for answered %v, want ErrPqSecretUnknownEpoch", err)
	}
	// and the error names the epoch, because the caller's only repair is to supply that epoch's
	// secret and a refusal that does not say which epoch is a refusal nobody can act on.
	_, _, err := pair.opener.OpenRecord(record)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(fmt.Sprintf("epoch %d", 1))) {
		t.Errorf("the refusal reads %v and does not name the epoch it is about", err)
	}
}
