// THE RESTART pqsecret.go's FIRST DRAFT CLAIMED COULD NOT HAPPEN.
//
// That draft said, of the group-lifetime premise, that "the moment a different secret arrives the
// session is ROTATED and the fallback is gone for good", and errors.go said ErrPqSecretUnknownEpoch
// was "unreachable" while a session had not rotated. Both sentences are about ONE PROCESS and the
// thing they describe is a property of the GROUP. A device that restarts after its group has
// rotated builds a session that holds today's secret, an empty table and the premise INTACT -- it
// never witnessed the rotation and now never can -- so it answers a past epoch with today's
// secret, which is exactly the defect ledger item 251's ruling 40 exists to remove, surfacing as
// an AEAD tag failure with nothing naming the cause.
//
// FOUR CASES, AND THE FIRST ONE IS WRITTEN TO GO RED ON AN IMPROVEMENT. It pins the residual as a
// measurement rather than as a paragraph: while nothing carries rotation across a restart, this is
// what a restarted session does. The day the fact reaches a fresh session -- sdk's GroupRecord
// stops being one pq_secret scalar, which is item 243's step 4 -- that case fails and says so, and
// whoever closes it has to move this file, pqsecret.go's header and errors.go's sentinel together.
// The other three are the seam that makes closing it possible, measured in both directions.
package messagegroup

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"slices"
	"testing"

	"github.com/urnetwork/connect/message"
)

// restartOpener is a device coming back after a stop: a session built over a handle LOADED FROM
// THE STORE rather than the one the live session held, at the epoch the group has reached, handed
// the one pq_secret a durable GroupRecord carries today.
//
// The handle is loaded rather than borrowed for two reasons, and the second is the one that
// matters: a restart really does re-open its group out of storage, and a session that owns its
// own handle can be Closed in a case that measures what a closed session's doors answer. The
// price is that a loaded handle is a SNAPSHOT -- it does not move when the chain's own handle
// commits -- so a case that needs this session to advance epochs uses restartOpenerOnTheChain
// instead, and the two are separate functions rather than a flag because the difference decides
// what AdvanceEpoch does and is not a detail.
func restartOpener(t *testing.T, pair *pastEpochPair, pqSecret []byte) *GroupSession {
	t.Helper()
	handle, err := pair.chain.b.engine.LoadGroup(pair.chain.joined.GroupId(), pair.chain.joined.Epoch())
	if err != nil {
		t.Fatalf("LoadGroup at the group's current epoch %d: %v", pair.chain.joined.Epoch(), err)
	}
	session := restartOpenerOver(t, pair, handle, pqSecret)
	t.Cleanup(func() { session.Close() })
	return session
}

// restartOpenerOnTheChain is the same restart over the chain's own live handle, for the cases that
// need the session to walk forward. It is NOT closed by this helper: the handle is the chain's and
// the chain closes it, and a Close here would close the group out from under the fixture.
func restartOpenerOnTheChain(t *testing.T, pair *pastEpochPair, pqSecret []byte) *GroupSession {
	t.Helper()
	return restartOpenerOver(t, pair, pair.chain.joined, pqSecret)
}

func restartOpenerOver(t *testing.T, pair *pastEpochPair, handle GroupHandle, pqSecret []byte) *GroupSession {
	t.Helper()
	groupId := pair.chain.joined.GroupId()
	engine := pair.chain.b.engine
	session, err := NewGroupSession(handle, pqSecret, pair.chain.groupHandleKey,
		newStreamIndexMemory(), testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("the restarted session: %v", err)
	}
	if err := session.InstallPastEpochLoader(func(epoch uint64) (GroupHandle, error) {
		return engine.LoadGroup(groupId, epoch)
	}); err != nil {
		t.Fatalf("InstallPastEpochLoader: %v", err)
	}
	return session
}

// epochOneCandidateKeys is the control every case below rests on: the class keys epoch one's root
// yields under its OWN pq_secret, and the ones it yields under the secret a rotated group runs on
// today. A case that compared a session's answer against one of these without knowing the two
// differ would be photographing one number twice.
func epochOneCandidateKeys(t *testing.T, pair *pastEpochPair) (own *ClassKeys, todays *ClassKeys) {
	t.Helper()
	handle, err := pair.chain.b.engine.LoadGroup(pair.chain.joined.GroupId(), 1)
	if err != nil {
		t.Fatalf("LoadGroup(epoch 1): %v", err)
	}
	defer handle.Close()
	mlsSecret, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("epoch one's Export: %v", err)
	}
	defer zeroize(mlsSecret)
	ownRoot := StorageRoot(mlsSecret, pair.chain.pqSecret)
	defer zeroize(ownRoot)
	todaysRoot := StorageRoot(mlsSecret, rotatedTestPqSecret())
	defer zeroize(todaysRoot)
	own = DeriveClassKeys(ownRoot)
	todays = DeriveClassKeys(todaysRoot)
	if classKeysEqual(own, todays) {
		t.Fatalf("CONTROL FAILED: epoch one's class keys under its own pq_secret equal the ones under the rotated secret, so no case in this file can tell an answer apart from a wrong answer")
	}
	return own, todays
}

// ── 1. THE RESIDUAL, PINNED ─────────────────────────────────────────────────────────────────────
//
// This case asserts a DEFECT and it is meant to. It is the difference between a residual that a
// step-4 author will find and one they will read past: pqsecret.go's header says a restarted
// session of a rotated group answers a past epoch with today's secret, and this is that sentence
// with a measurement under it, in a form that cannot quietly stop being true.
//
// WHEN THIS CASE FAILS, THE RESIDUAL IS CLOSED. Its messages say so. Do not repair it by relaxing
// the assertion; delete it, delete the residual paragraph in pqsecret.go, rewrite errors.go's
// three-ways paragraph at ErrPqSecretUnknownEpoch, and take item 243's Consumes line with it.
func TestARestartOfARotatedGroupStillHoldsThePremiseAndAnswersAPastEpochWithTodaysSecret(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-residual")
	record := pair.sealDurable(t, "sealed at epoch one")
	ownKeys, todaysKeys := epochOneCandidateKeys(t, pair)

	// THE GROUP ROTATES. The live session is the control for the residual: it lived through the
	// change, so it observed it, and it is the thing the restarted session below is not.
	pair.advanceOpenerWith(t, rotatedTestPqSecret())
	var livePremise bool
	if err := pair.opener.do(func() { livePremise = pair.opener.pqLifetime }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if livePremise {
		t.Fatalf("the session that lived through the rotation still holds the group-lifetime premise; a second, different secret arriving at AdvanceEpoch is supposed to drop it, and if that is broken the case below measures nothing")
	}

	restarted := restartOpener(t, pair, rotatedTestPqSecret())
	var restartedPremise bool
	var held int
	if err := restarted.do(func() {
		restartedPremise = restarted.pqLifetime
		held = len(restarted.pqSecrets)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !restartedPremise || held != 1 {
		t.Fatalf("THE RESIDUAL MAY BE CLOSED: a restarted session of a rotated group holds the premise: %t with %d table entry(ies), and this case was written when it was true with 1. If a constructor now carries rotation across a restart, this file, pqsecret.go's residual paragraph and errors.go's ErrPqSecretUnknownEpoch comment all have to move together",
			restartedPremise, held)
	}

	if err := restarted.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("the restarted session's TrackSenderAt(epoch 1) answered %v; the residual is that it does NOT refuse here", err)
	}
	built := pastEpochClassKeysAt(t, restarted, 1)
	if !classKeysEqual(built, todaysKeys) || classKeysEqual(built, ownKeys) {
		t.Fatalf("THE RESIDUAL MAY BE CLOSED: the restarted session built epoch one's schedule from today's secret: %t, from epoch one's own: %t; this case was written when it was true/false. Ruling 40's line is fixed inside a session and this case is the part that is not -- if it now derives epoch one's own root, say so everywhere the residual is written down",
			classKeysEqual(built, todaysKeys), classKeysEqual(built, ownKeys))
	}
	_, _, openErr := restarted.OpenRecord(record)
	if openErr == nil {
		t.Fatalf("a restarted session of a rotated group OPENED an epoch-one record; either the residual is closed or the rotation did not happen")
	}
	if errors.Is(openErr, ErrPqSecretUnknownEpoch) {
		t.Fatalf("THE RESIDUAL IS CLOSED: the restarted session refused with ErrPqSecretUnknownEpoch, which is the answer this shape should get and did not. Delete this case and the residual with it")
	}
	// and this is the whole cost of the residual in one line: the failure a field report would
	// carry is an AEAD tag, which names nothing.
	if !errors.Is(openErr, ErrRecordAeadOpen) {
		t.Errorf("the restarted session answered %v; the residual's signature is an AEAD failure with no diagnosis in it, and an answer of another shape means the path changed under this case", openErr)
	}
}

// ── 2. THE DECLARATION ──────────────────────────────────────────────────────────────────────────
//
// DeclarePqSecretRotated is how the fact reaches a fresh session, and this case fails in both
// directions off one session: before the declaration it opens the record out of the premise, after
// it refuses by name, and after the missing secret is filed it opens it out of the RIGHT root. A
// case that only showed the refusal would be satisfied by a door that refused everything.
func TestARestartedSessionToldItsGroupRotatedRefusesUntilThePastSecretIsFiled(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-declared")
	record := pair.sealDurable(t, "sealed at epoch one")
	ownKeys, todaysKeys := epochOneCandidateKeys(t, pair)
	pair.advanceOpenerWith(t, rotatedTestPqSecret())

	restarted := restartOpener(t, pair, rotatedTestPqSecret())

	// BEFORE, READ AT THE SECRET AND NOT THROUGH AN OPEN: this session would answer epoch one
	// with today's octets. It is read here rather than by opening a record because an open
	// CACHES the schedule it builds, and a cached pastEpoch answers ahead of the lookup this
	// case is about -- so an open before the declaration would be measuring the cache afterwards.
	var beforeSecret []byte
	if err := restarted.do(func() {
		secret, err := restarted.pqSecretForOnLoop(1)
		if err != nil {
			t.Errorf("before the declaration, epoch one's secret answered %v; the premise is supposed to answer it, and the declaration below has nothing to change if it does not", err)
			return
		}
		beforeSecret = append([]byte(nil), secret...)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !bytes.Equal(beforeSecret, rotatedTestPqSecret()) {
		t.Fatalf("before the declaration the restarted session answered epoch one with something other than today's secret, so this case is not measuring the premise")
	}

	if err := restarted.DeclarePqSecretRotated(); err != nil {
		t.Fatalf("DeclarePqSecretRotated: %v", err)
	}

	if err := restarted.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0); !errors.Is(err, ErrPqSecretUnknownEpoch) {
		t.Fatalf("a restarted session TOLD its group rotated answered %v when asked to track a sender at epoch one, want ErrPqSecretUnknownEpoch; the declaration is the only thing standing between this session and ruling 40's defect", err)
	}
	_, _, err := restarted.OpenRecord(record)
	if !errors.Is(err, ErrPqSecretUnknownEpoch) {
		t.Fatalf("the same session answered %v for an epoch-one record, want ErrPqSecretUnknownEpoch", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte(fmt.Sprintf("epoch %d", 1))) {
		t.Errorf("the refusal reads %v and does not name the epoch; the caller's only repair is to supply that epoch's secret", err)
	}

	// AFTER THE REPAIR THE REFUSAL NAMED: the past secret is filed and the same record opens, out
	// of epoch one's OWN root and not out of today's. Without this half the case above would be
	// held by a session that simply refuses everything.
	if err := restarted.InstallPqSecret(1, pair.chain.pqSecret); err != nil {
		t.Fatalf("InstallPqSecret(1, epoch one's own secret): %v", err)
	}
	if err := restarted.TrackSenderAt(1, pair.senderLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("TrackSenderAt(epoch 1) after the secret was filed: %v", err)
	}
	if _, body, err := restarted.OpenRecord(record); err != nil || string(body) != "sealed at epoch one" {
		t.Fatalf("after epoch one's own secret was filed the record answered %v / %q, want the body it was sealed with", err, body)
	}
	after := pastEpochClassKeysAt(t, restarted, 1)
	if !classKeysEqual(after, ownKeys) || classKeysEqual(after, todaysKeys) {
		t.Errorf("the repaired session built epoch one from its own secret: %t, from today's: %t; want true/false",
			classKeysEqual(after, ownKeys), classKeysEqual(after, todaysKeys))
	}
}

// ── 3. THE DOOR ─────────────────────────────────────────────────────────────────────────────────
//
// InstallPqSecret's own properties, each failing for its own reason: it FILES, it REFUTES the
// premise on the octets rather than on the call, it ERASES what it replaces, it refuses a width, a
// nothing, an epoch past the window and a closed session. The refutation is the one that earns the
// door its place -- a restorer that files a past secret has told this session its group rotates
// without a second call, so a restorer cannot half-repair itself into the residual above.
func TestInstallPqSecretFilesRefutesOnTheOctetsAndRefusesWhatItCannotServe(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-door")
	pair.advanceOpenerWith(t, rotatedTestPqSecret())
	restarted := restartOpener(t, pair, rotatedTestPqSecret())
	var at uint64
	if err := restarted.do(func() { at = restarted.epoch }); err != nil {
		t.Fatalf("do: %v", err)
	}

	// FILING TODAY'S OWN SECRET AGAIN REFUTES NOTHING. The control for the refutation: same door,
	// octets that match, premise intact. It files at the epoch ABOVE this session's own -- ruling
	// 37's epoch, and the one this door exists for. It used to file AT this session's own epoch,
	// and that acceptance was itself a defect, measured two blocks down.
	if err := restarted.InstallPqSecret(at+1, rotatedTestPqSecret()); err != nil {
		t.Fatalf("InstallPqSecret(%d, the secret it already holds): %v", at+1, err)
	}
	var premise bool
	if err := restarted.do(func() { premise = restarted.pqLifetime }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !premise {
		t.Errorf("filing the secret this session already holds dropped the group-lifetime premise; the refutation is on the OCTETS and a door that dropped it on the call would take the compatibility path away from every group alive")
	}

	// AND FILING A DIFFERENT ONE REFUTES IT, with the entry landing at the epoch asked for.
	if err := restarted.InstallPqSecret(1, pair.chain.pqSecret); err != nil {
		t.Fatalf("InstallPqSecret(1, a different secret): %v", err)
	}
	var filed []byte
	if err := restarted.do(func() {
		premise = restarted.pqLifetime
		filed = append([]byte(nil), restarted.pqSecrets[1]...)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if premise {
		t.Errorf("a secret differing from the one at this session's epoch was filed and the premise survived; that filing IS the observation a restart cannot make for itself")
	}
	if !bytes.Equal(filed, pair.chain.pqSecret) {
		t.Errorf("pq_secret[1] reads %d octets and is not what was filed", len(filed))
	}

	// THE REPLACE ERASES, aliased before the replacement for the reason every erase case in this
	// package aliases: "all zero" read through the map is a photograph of whatever is there now.
	var superseded []byte
	if err := restarted.do(func() { superseded = restarted.pqSecrets[1] }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !containsNonZero(superseded) {
		t.Fatalf("the entry about to be replaced is already all zero, so the reading below would say nothing")
	}
	replacement := make([]byte, PqSecretBytes)
	for i := range replacement {
		replacement[i] = byte(0xC3 ^ i)
	}
	if err := restarted.InstallPqSecret(1, replacement); err != nil {
		t.Fatalf("InstallPqSecret(1, a replacement): %v", err)
	}
	if containsNonZero(superseded) {
		t.Errorf("the superseded pq_secret[1] is still in the heap; an entry a door replaced without erasing is a retired epoch's post quantum half with no owner")
	}

	// THE REFUSALS, each measured against the accepted call above rather than against nothing.
	if err := restarted.InstallPqSecret(1, nil); !errors.Is(err, ErrNilPqSecret) {
		t.Errorf("InstallPqSecret(1, nil) answered %v, want ErrNilPqSecret; a door that took it would file thirty two zeros as an epoch's post quantum half", err)
	}
	if err := restarted.InstallPqSecret(1, make([]byte, 4)); !errors.Is(err, ErrPqSecretLength) {
		t.Errorf("InstallPqSecret(1, four octets) answered %v, want ErrPqSecretLength; four octets extract to a storage root both clients agree on and MASTER section 7 does not specify", err)
	}

	// AND AN EPOCH ABOVE THIS SESSION'S OWN IS ACCEPTED, which is not an oversight: ruling 37 has
	// the wraps for epoch n+1 submitted AT epoch n, staged and pre-merge, so the secret of an
	// epoch this session has not yet entered is a value that legitimately arrives early.
	//
	// WHAT THIS BLOCK USED TO BE was `err != nil` and nothing else, and that was the whole of the
	// measurement under a capability the ledger report called "asserted, not incidental". err ==
	// nil says the call was not refused; it says nothing about whether the value survives, and it
	// did not -- the very next AdvanceEpoch erased it and filed its own argument over the top.
	// Case 3c is that assertion, over the sequence ruling 37 actually specifies.
	if err := restarted.InstallPqSecret(at+1, replacement); err != nil {
		t.Errorf("InstallPqSecret at epoch %d, one above this session's %d, answered %v; ruling 37 has that secret arriving before the merge that opens its epoch", at+1, at, err)
	}

	// AND THE SESSION'S OWN EPOCH IS REFUSED, one epoch away from the acceptance above so the two
	// fail in opposite directions off one session. This door FILES AND DOES NOT RE-DERIVE: a
	// value accepted here would move pq_secret[at] while self.storageRoot stayed extracted from
	// the octets installEpochOnLoop was handed, and one advance later pastEpochOnLoop would
	// rebuild epoch `at` out of the TABLE and every record of it would stop opening at the AEAD
	// tag -- ruling 40's own defect arriving through the door added to close it. Reproduced
	// before it was repaired; case 6 holds the invariant the refusal protects.
	if err := restarted.InstallPqSecret(at, replacement); !errors.Is(err, ErrPqSecretEpochIsCurrent) {
		t.Errorf("InstallPqSecret at epoch %d, the epoch this session is STANDING at, answered %v, want ErrPqSecretEpochIsCurrent; this door's own doc says it files for an epoch this session did not stand at, and it files without re-deriving", at, err)
	}
	// and the refusal FILED NOTHING, which is the half a sentinel check cannot see. The value
	// offered above is the one that DIFFERS from what stands at `at`, so a door that filed it
	// would be visible here.
	var untouched []byte
	if err := restarted.do(func() { untouched = append([]byte(nil), restarted.pqSecrets[at]...) }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if bytes.Equal(untouched, replacement) || !bytes.Equal(untouched, rotatedTestPqSecret()) {
		t.Errorf("pq_secret[%d] moved under a refused call; a refusal that files is a refusal in the error only", at)
	}

	// AND A CLOSED SESSION'S DOORS ARE SHUT. Both of them, because two doors onto one field with
	// one of them still open after Close is the field surviving the erase that emptied it.
	if err := restarted.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := restarted.InstallPqSecret(1, replacement); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("InstallPqSecret on a closed session answered %v, want ErrSessionClosed", err)
	}
	if err := restarted.DeclarePqSecretRotated(); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("DeclarePqSecretRotated on a closed session answered %v, want ErrSessionClosed", err)
	}
}

// ── 3b. THE DOOR'S WINDOW BOUND, WHICH NEEDS A SESSION FAR ENOUGH ALONG TO HAVE ONE ─────────────
//
// InstallPqSecret refuses an epoch past PastEpochWindow with pastEpochOnLoop's OWN sentinel and
// files nothing, because that function refuses the epoch before it ever asks for a secret: filing
// one would hold a retired epoch's post quantum half until the next advance erased it, for
// nothing, and would answer a restorer "recovered" about history it cannot read. The control is
// the epoch exactly AT the edge, in the same pass -- one epoch apart, opposite answers -- without
// which the refusal could be a door that refuses every past epoch.
func TestInstallPqSecretRefusesAnEpochPastTheWindowAndAcceptsTheOneAtTheEdge(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-door-window")
	// the group walks to 1+PastEpochWindow with the SAME secret every time, so the premise is
	// intact and the only thing under test below is the bound.
	for epoch := uint64(2); epoch <= 1+PastEpochWindow; epoch += 1 {
		pair.advanceOpenerWith(t, pair.chain.pqSecret)
	}
	restarted := restartOpener(t, pair, pair.chain.pqSecret)
	var at uint64
	if err := restarted.do(func() { at = restarted.epoch }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if at != 1+PastEpochWindow {
		t.Fatalf("the restarted session is at epoch %d, want %d; the bound below is arithmetic on it", at, 1+PastEpochWindow)
	}
	edge := at - PastEpochWindow
	other := make([]byte, PqSecretBytes)
	for i := range other {
		other[i] = byte(0x91 ^ (i * 3))
	}
	if err := restarted.InstallPqSecret(edge, other); err != nil {
		t.Errorf("InstallPqSecret at epoch %d, exactly PastEpochWindow behind %d, answered %v; that is the last epoch the window reaches and an open there would be admitted", edge, at, err)
	}
	if err := restarted.InstallPqSecret(edge-1, other); !errors.Is(err, ErrEpochOutOfWindow) {
		t.Errorf("InstallPqSecret at epoch %d, %d behind %d, answered %v, want ErrEpochOutOfWindow", edge-1, PastEpochWindow+1, at, err)
	}
	var heldEdge, heldBelow bool
	if err := restarted.do(func() {
		_, heldEdge = restarted.pqSecrets[edge]
		_, heldBelow = restarted.pqSecrets[edge-1]
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !heldEdge {
		t.Errorf("the accepted epoch %d was not filed", edge)
	}
	if heldBelow {
		t.Errorf("the refused epoch %d was filed anyway; a refusal that files is a refusal with a side effect", edge-1)
	}
}

// candidateClassKeysAt is epochOneCandidateKeys for any epoch and any two candidates: the class
// keys one epoch yields under two different pq_secrets, ASSERTED UNEQUAL before either of them is
// compared against anything. Without it every reading below is one number photographed twice.
func candidateClassKeysAt(t *testing.T, pair *pastEpochPair, epoch uint64, a []byte, b []byte) (*ClassKeys, *ClassKeys) {
	t.Helper()
	handle, err := pair.chain.b.engine.LoadGroup(pair.chain.joined.GroupId(), epoch)
	if err != nil {
		t.Fatalf("LoadGroup(epoch %d): %v", epoch, err)
	}
	defer handle.Close()
	mlsSecret, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("Export at epoch %d: %v", epoch, err)
	}
	defer zeroize(mlsSecret)
	rootA := StorageRoot(mlsSecret, a)
	defer zeroize(rootA)
	rootB := StorageRoot(mlsSecret, b)
	defer zeroize(rootB)
	keysA := DeriveClassKeys(rootA)
	keysB := DeriveClassKeys(rootB)
	if classKeysEqual(keysA, keysB) {
		t.Fatalf("CONTROL FAILED: the two candidate class-key sets for epoch %d are equal, so nothing in this case can tell an answer from a wrong answer", epoch)
	}
	return keysA, keysB
}

// liveClassKeysOf reads the class keys the session is ACTUALLY sealing and opening under, off the
// loop, copied so nothing here aliases the schedule.
func liveClassKeysOf(t *testing.T, session *GroupSession) *ClassKeys {
	t.Helper()
	var keys *ClassKeys
	if err := session.do(func() {
		keys = &ClassKeys{
			Perm:    append([]byte(nil), session.classKeys.Perm...),
			Durable: append([]byte(nil), session.classKeys.Durable...),
			Media:   append([]byte(nil), session.classKeys.Media...),
		}
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	return keys
}

// ── 3c. RULING 37's OWN SEQUENCE, WHICH THE ERR==NIL ASSERTION DID NOT MEASURE ──────────────────
//
// InstallPqSecret(n+1) then AdvanceEpoch, which is what the wrap delivers: the wraps carrying
// pq_secret[n+1] are submitted at epoch n and opened BEFORE the merge, so the table holds that
// epoch's authority when the advance runs. What this case asserts is that the filed value SURVIVES
// the advance and is what the epoch's own schedule is derived from.
//
// IT DID NOT. The install compared the arriving value against the entry at the epoch being LEFT
// and never against the entry it was about to ERASE, so AdvanceEpoch blanked the wrap's secret,
// filed its own argument over the top and answered nil -- and both production call sites in sdk
// hand in the group lifetime scalar. The session then ran epoch n+1 on one secret while every
// other member ran it on another, and the only symptom anywhere was an AEAD tag: ruling 38's
// undiagnosable both-directions blackout, produced by the seam built to prevent it. The whole of
// the measurement under that capability was `err != nil`, which says a call was not refused and
// says nothing about whether its effect lasted one line.
//
// THE MIS-WIRED CALLER IS THE OTHER HALF, in the same pass and off the same session: an advance
// carrying a DIFFERENT secret for that epoch is ErrPqSecretEpochConflict, it files nothing, it
// erases nothing, and the wrap's value is still there afterwards. A case with only the happy arm
// would be satisfied by a session that ignored its advance parameter entirely.
func TestAWrapsSecretForTheNextEpochSurvivesTheAdvanceAndIsWhatThatEpochDerivesFrom(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-ruling37")
	wrapSecret := rotatedTestPqSecret()
	scalar := pair.chain.pqSecret

	// THE COMMIT THAT OPENS THE NEXT EPOCH, merged at the handle while the SESSION still stands
	// at epoch n -- which is exactly the window ruling 37 puts the wrap in.
	if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
		t.Fatalf("Commit(nil): %v", err)
	}
	if err := pair.chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	next := pair.chain.joined.Epoch()
	wrapKeys, scalarKeys := candidateClassKeysAt(t, pair, next, wrapSecret, scalar)

	var standing uint64
	if err := pair.opener.do(func() { standing = pair.opener.epoch }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if standing >= next {
		t.Fatalf("the session stands at epoch %d and the new epoch is %d; this case is about a secret filed for an epoch the session has NOT entered", standing, next)
	}
	if err := pair.opener.InstallPqSecret(next, wrapSecret); err != nil {
		t.Fatalf("InstallPqSecret(%d, the wrap's secret) while standing at %d: %v", next, standing, err)
	}

	// THE MIS-WIRED ADVANCE, FIRST, so that the happy arm below cannot be read as "the parameter
	// was ignored". sdk/urmessage/group.go's committer and receiver both hand in the lifetime
	// scalar today, and that is this call.
	if err := pair.opener.AdvanceEpoch(scalar); !errors.Is(err, ErrPqSecretEpochConflict) {
		t.Fatalf("AdvanceEpoch with a secret differing from the one already filed for epoch %d answered %v, want ErrPqSecretEpochConflict; silently replacing it is ruling 38's blackout and it answered nil before this refusal existed", next, err)
	}
	var survived []byte
	if err := pair.opener.do(func() { survived = append([]byte(nil), pair.opener.pqSecrets[next]...) }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !bytes.Equal(survived, wrapSecret) {
		t.Fatalf("the refused advance moved pq_secret[%d] anyway; nothing may be filed or erased on the path that refuses", next)
	}
	if !containsNonZero(survived) {
		t.Fatalf("pq_secret[%d] is all zero after the refused advance; the entry was erased by a call that filed nothing in its place", next)
	}

	// AND THE ADVANCE THAT AGREES WITH THE WRAP GOES THROUGH, and the epoch's LIVE schedule is
	// the wrap's and not the scalar's. This is the assertion `err != nil` stood in for.
	if err := pair.opener.AdvanceEpoch(wrapSecret); err != nil {
		t.Fatalf("AdvanceEpoch with the secret already filed for epoch %d: %v", next, err)
	}
	var after []byte
	var now uint64
	if err := pair.opener.do(func() {
		after = append([]byte(nil), pair.opener.pqSecrets[next]...)
		now = pair.opener.epoch
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if now != next {
		t.Fatalf("the session stands at epoch %d after an advance into %d", now, next)
	}
	if !bytes.Equal(after, wrapSecret) {
		t.Errorf("pq_secret[%d] is not the wrap's secret after the advance; the filed value is the one the epoch ran on", next)
	}
	live := liveClassKeysOf(t, pair.opener)
	if !classKeysEqual(live, wrapKeys) || classKeysEqual(live, scalarKeys) {
		t.Errorf("epoch %d's live class keys are the wrap's: %t, the scalar's: %t; want true/false. The table holding the right octets while the schedule was derived from the wrong ones is the same defect one field further in",
			next, classKeysEqual(live, wrapKeys), classKeysEqual(live, scalarKeys))
	}
	// AND THE SEALING PATH AGREES WITH THE SCHEDULE, so this is not a reading of one field: a
	// record sealed here is sealed under the root the table's octets extract to.
	if _, err := pair.opener.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("sealed at the wrap's epoch"), 0, nil); err != nil {
		t.Errorf("SealRecord at the advanced epoch: %v", err)
	}
}

// ── 3d. THE INVARIANT THE TWO REFUSALS PROTECT ──────────────────────────────────────────────────
//
// THE TABLE AND THE LIVE KEY SCHEDULE MAY NOT DISAGREE ABOUT self.epoch. self.storageRoot was
// extracted ONCE, by installEpochOnLoop, from pq_secret[self.epoch] as the table held it then;
// everything this session seals and opens at its own epoch hangs off it, and pastEpochOnLoop
// rebuilds that epoch from the TABLE the moment the session moves on. Two answers to one question
// is a session that works today and opens nothing tomorrow, with an AEAD tag for a diagnosis.
//
// IT IS ASSERTED OVER EVERY DOOR THAT COULD BREAK IT AND IT FAILS BOTH WAYS. The doors are walked
// first -- construct, install a past epoch, install the next epoch, declare, advance -- and the
// invariant is held after each; then the disagreement is PLANTED on the loop, because no door can
// produce it any more, and the same check is required to catch it. Without the planted half this
// case would be satisfied by a check that compared a value against itself.
func TestTheTableAndTheLiveScheduleCannotDisagreeAboutTheSessionsOwnEpoch(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-invariant")
	session := restartOpenerOnTheChain(t, pair, pair.chain.pqSecret)

	// agrees re-derives self.epoch's storage root from the TABLE's own octets and compares it
	// against the root the session is actually using. It answers the comparison rather than
	// asserting, so the planted half below can require it to say false.
	agrees := func() bool {
		t.Helper()
		var epoch uint64
		var fromTable []byte
		var live []byte
		if err := session.do(func() {
			epoch = session.epoch
			fromTable = append([]byte(nil), session.pqSecrets[session.epoch]...)
			live = append([]byte(nil), session.storageRoot...)
		}); err != nil {
			t.Fatalf("do: %v", err)
		}
		if len(fromTable) == 0 {
			t.Fatalf("the table holds no entry at this session's own epoch %d, so the invariant is being read over nothing", epoch)
		}
		if !containsNonZero(live) {
			t.Fatalf("this session's storage_root is all zero, so the comparison below would hold over two blanks")
		}
		handle, err := pair.chain.b.engine.LoadGroup(pair.chain.joined.GroupId(), epoch)
		if err != nil {
			t.Fatalf("LoadGroup(epoch %d): %v", epoch, err)
		}
		defer handle.Close()
		mlsSecret, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
		if err != nil {
			t.Fatalf("Export at epoch %d: %v", epoch, err)
		}
		defer zeroize(mlsSecret)
		rebuilt := StorageRoot(mlsSecret, fromTable)
		defer zeroize(rebuilt)
		return bytes.Equal(rebuilt, live)
	}

	if !agrees() {
		t.Fatalf("the invariant does not hold at construction, so nothing below is measuring a door")
	}

	other := make([]byte, PqSecretBytes)
	for i := range other {
		other[i] = byte(0x9B ^ (i * 3))
	}
	var built uint64
	if err := session.do(func() { built = session.epoch }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if built == 0 {
		t.Fatalf("this session was built at epoch 0 and the past-epoch door below has no epoch to file at")
	}
	if err := session.InstallPqSecret(built-1, other); err != nil {
		t.Fatalf("InstallPqSecret(%d, a past epoch's own secret): %v", built-1, err)
	}
	if !agrees() {
		t.Errorf("filing a PAST epoch's secret moved this session's own epoch out of agreement with the table; that door files at the epoch it was given and nowhere else")
	}
	if err := session.InstallPqSecret(built+1, other); err != nil {
		t.Fatalf("InstallPqSecret(%d, the next epoch's secret): %v", built+1, err)
	}
	if !agrees() {
		t.Errorf("filing the NEXT epoch's secret moved this session's own epoch out of agreement with the table")
	}
	if err := session.DeclarePqSecretRotated(); err != nil {
		t.Fatalf("DeclarePqSecretRotated: %v", err)
	}
	if !agrees() {
		t.Errorf("the declaration moved this session's own epoch out of agreement with the table; it states a fact and files nothing")
	}
	// and the advance, which is the one path that moves BOTH and must move them together.
	if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
		t.Fatalf("Commit(nil): %v", err)
	}
	if err := pair.chain.joined.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if err := session.AdvanceEpoch(other); err != nil {
		t.Fatalf("AdvanceEpoch into the epoch whose secret was already filed: %v", err)
	}
	if !agrees() {
		t.Errorf("an advance left the table and the live schedule disagreeing about the epoch it entered; AdvanceEpoch files AND re-derives, and that is the whole reason InstallPqSecret refuses this session's own epoch")
	}

	// AND THE PLANTED DISAGREEMENT, because a check that cannot say false is not a check. No door
	// can produce this state -- InstallPqSecret refuses self.epoch with ErrPqSecretEpochIsCurrent
	// and AdvanceEpoch refuses a conflicting value with ErrPqSecretEpochConflict -- so it is
	// written straight onto the loop, which is what those two refusals are for.
	var saved []byte
	if err := session.do(func() {
		saved = append([]byte(nil), session.pqSecrets[session.epoch]...)
		zeroize(session.pqSecrets[session.epoch])
		session.pqSecrets[session.epoch] = append([]byte(nil), pair.chain.pqSecret...)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if bytes.Equal(saved, pair.chain.pqSecret) {
		t.Fatalf("the planted value equals the one already at this session's epoch, so the disagreement below was never planted")
	}
	if agrees() {
		t.Errorf("the table was made to disagree with the live schedule about epoch's own secret and the invariant still held; this check cannot catch what the two refusals exist to prevent, so every reading above it is vacuous")
	}
	if err := session.do(func() {
		zeroize(session.pqSecrets[session.epoch])
		session.pqSecrets[session.epoch] = saved
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !agrees() {
		t.Errorf("the planted value was put back and the invariant did not return, so the reading above was not the plant")
	}
}

// ── 4. WHAT THE PREMISE IS DEFINED ON, DERIVED AND ASSERTED RATHER THAN DESCRIBED ───────────────
//
// pqsecret.go claims the premise is reached "only for epochs BELOW the one this session was
// constructed at: precisely the epochs it was not present for". That is a claim about a SET, so it
// is measured as one: every epoch from zero to the session's own is asked for, each answer is
// classified by which arm produced it, and the three sets are asserted whole -- not sampled.
//
// IT FAILS BOTH WAYS. With the premise standing, the table answers exactly the epochs the session
// stood at and the premise answers exactly the rest, with NOTHING refused; with the premise
// dropped, the premise set is empty and the refused set is exactly what it used to answer. A gate
// that printed these sets without holding them against a written-down disposition would be a gate
// that passes whatever the code happens to do.
func TestThePremiseAnswersExactlyTheEpochsBelowTheOneTheSessionWasBuiltAt(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-premise-set")
	// the group walks to epoch five with no session watching, which is what a device that was
	// offline comes back to.
	for range 4 {
		if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
			t.Fatalf("Commit(nil): %v", err)
		}
		if err := pair.chain.joined.MergePendingCommit(); err != nil {
			t.Fatalf("MergePendingCommit: %v", err)
		}
	}
	if epoch := pair.chain.joined.Epoch(); epoch != 5 {
		t.Fatalf("the group is at epoch %d after four commits, want 5", epoch)
	}
	// over the chain's LIVE handle, because this session has to walk forward and a store-loaded
	// handle is a snapshot that does not move when the chain commits.
	restarted := restartOpenerOnTheChain(t, pair, pair.chain.pqSecret)
	builtAt := uint64(5)

	// and two advances on the SAME secret, so the table holds 5, 6 and 7 and the premise is
	// still standing -- the ordinary shape of every group alive today.
	for range 2 {
		if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
			t.Fatalf("Commit(nil): %v", err)
		}
		if err := pair.chain.joined.MergePendingCommit(); err != nil {
			t.Fatalf("MergePendingCommit: %v", err)
		}
		if err := restarted.AdvanceEpoch(pair.chain.pqSecret); err != nil {
			t.Fatalf("AdvanceEpoch: %v", err)
		}
	}

	// classify is the whole measurement: which arm answered, for every epoch at or below this
	// session's own.
	//
	// THE OCTETS ARE CHECKED AND NOT ONLY THE MEMBERSHIP, which is what makes this a measurement
	// of the ORDER of the two arms rather than of the table's keys. While the premise stands both
	// arms return the same octets for every epoch in the table -- every entry is the same value --
	// so a reading that classified on the key alone would call an answer a table hit while the
	// premise produced it, and the day the entries differ that is ruling 40's defect exactly.
	// Each epoch's answer is therefore held against the entry filed AT that epoch, and a premise
	// answer against the entry at the session's own.
	classify := func() (fromTable []uint64, fromPremise []uint64, refused []uint64) {
		if err := restarted.do(func() {
			for epoch := uint64(0); epoch <= restarted.epoch; epoch += 1 {
				entry, inTable := restarted.pqSecrets[epoch]
				secret, err := restarted.pqSecretForOnLoop(epoch)
				switch {
				case err != nil:
					refused = append(refused, epoch)
				case inTable:
					if !bytes.Equal(secret, entry) {
						t.Errorf("epoch %d is in the table and the lookup answered something else; the table arm has to come FIRST or a session that has seen a rotation answers a held epoch out of the premise", epoch)
					}
					fromTable = append(fromTable, epoch)
				default:
					if !bytes.Equal(secret, restarted.pqSecrets[restarted.epoch]) {
						t.Errorf("epoch %d was answered out of neither the table nor the premise, which is a third arm this file does not know about", epoch)
					}
					fromPremise = append(fromPremise, epoch)
				}
			}
		}); err != nil {
			t.Fatalf("do: %v", err)
		}
		return fromTable, fromPremise, refused
	}
	sameSet := func(got []uint64, want []uint64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	fromTable, fromPremise, refused := classify()
	// THE DISPOSITION, WRITTEN DOWN BEFORE THE READING: the table answers exactly the epochs this
	// session stood at -- the one it was built at and the two it advanced into -- and the premise
	// answers exactly the epochs below that, which is the set it has no evidence about.
	if !sameSet(fromTable, []uint64{5, 6, 7}) {
		t.Errorf("the table answered %v, want [5 6 7]: the epoch the session was built at and the two it advanced into, and nothing else", fromTable)
	}
	if !sameSet(fromPremise, []uint64{0, 1, 2, 3, 4}) {
		t.Errorf("the premise answered %v, want [0 1 2 3 4]: every epoch below the one this session was built at, and no epoch it stood at", fromPremise)
	}
	if len(refused) != 0 {
		t.Errorf("a session holding the premise refused %v; while the premise stands it answers everything, which is the behaviour every group alive today depends on", refused)
	}
	// THE COMPLEMENT, ASSERTED AND NOT MERELY PRINTED: every epoch the premise answered is
	// strictly below the epoch the session was built at. An empty premise set here would make the
	// two assertions above pass vacuously.
	if len(fromPremise) == 0 {
		t.Fatalf("no epoch reached the premise at all, so the bound below is asserted over nothing")
	}
	for _, epoch := range fromPremise {
		if epoch >= builtAt {
			t.Errorf("the premise answered epoch %d and this session was built at epoch %d; the premise is supposed to be defined exactly on what the session was not present for", epoch, builtAt)
		}
	}

	// AND THE OTHER DIRECTION, off the same session: the declaration moves the premise's whole set
	// into the refused set and leaves the table's untouched.
	if err := restarted.DeclarePqSecretRotated(); err != nil {
		t.Fatalf("DeclarePqSecretRotated: %v", err)
	}
	fromTableAfter, fromPremiseAfter, refusedAfter := classify()
	if !sameSet(fromTableAfter, []uint64{5, 6, 7}) {
		t.Errorf("after the declaration the table answered %v, want [5 6 7]: a session told its group rotates still holds the epochs it stood at", fromTableAfter)
	}
	if len(fromPremiseAfter) != 0 {
		t.Errorf("after the declaration the premise still answered %v", fromPremiseAfter)
	}
	if !sameSet(refusedAfter, []uint64{0, 1, 2, 3, 4}) {
		t.Errorf("after the declaration the refused set is %v, want [0 1 2 3 4]: exactly what the premise used to answer, refused by name instead of answered wrongly", refusedAfter)
	}

	// AND THE ORDER OF THE ARMS ONCE THE PREMISE IS GONE: epoch five's own secret is filed and
	// has to be what epoch five is answered with, rather than the entry at this session's own
	// epoch. Case 5 is where the order is measured in the state that makes it load-bearing.
	distinct := make([]byte, PqSecretBytes)
	for i := range distinct {
		distinct[i] = byte(0x6E ^ (i * 5))
	}
	if err := restarted.InstallPqSecret(5, distinct); err != nil {
		t.Fatalf("InstallPqSecret(5, a distinguishable secret): %v", err)
	}
	if err := restarted.do(func() {
		if bytes.Equal(restarted.pqSecrets[restarted.epoch], distinct) {
			t.Fatalf("the filed secret equals the one at this session's own epoch, so the order below cannot be read")
		}
		secret, err := restarted.pqSecretForOnLoop(5)
		if err != nil {
			t.Errorf("epoch five's own secret was filed and the lookup answered %v", err)
			return
		}
		if !bytes.Equal(secret, distinct) {
			t.Errorf("the lookup answered epoch five out of something other than the entry filed at epoch five; the table arm has to come first")
		}
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
}

// ── 5. THE ORDER OF THE TWO ARMS, WHICH IS UNOBSERVABLE TODAY AND WILL NOT BE ───────────────────
//
// A mutant that puts the PREMISE arm first survives this package's whole suite, and after the
// query was checked that turned out to be a fact about the code rather than a hole in the reading.
// installPqSecretOnLoop is the ONLY writer of an ENTRY of the table -- case 5b walks every
// production source and asserts that, rather than this paragraph counting greps -- and it drops
// the premise the moment a value arrives that differs from the one already there. So WHILE THE
// PREMISE STANDS EVERY ENTRY IN THE TABLE IS THE SAME OCTETS, the two arms return equal values for
// every epoch, and no reading of the live doors can tell the order apart. The mutant is
// equivalent, today.
//
// TODAY IS THE WHOLE OF THAT SENTENCE. The invariant is exactly what item 243's step 4 removes: a
// device wrap delivers pq_secret[k] per epoch, the table starts holding different values, and the
// order stops being a style question -- a lookup that reached the premise first would answer a
// held epoch out of the current epoch's octets, which is ruling 40's own line one arm further in.
//
// So both facts are asserted rather than either being assumed: the invariant that makes the order
// unobservable, over the real doors; and the order itself, in the state step 4 makes reachable,
// which no door can produce today and which is therefore planted on the loop. A case that planted
// the state without asserting the invariant would be testing a shape the code cannot reach; one
// that asserted the invariant without the order would leave the arm swap unmeasured until the day
// it starts mattering, which is the day nobody is looking at this file.
func TestWhileThePremiseStandsTheTableHoldsOneValueAndTheTableArmStillAnswersFirst(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-arm-order")
	restarted := restartOpenerOnTheChain(t, pair, pair.chain.pqSecret)
	for range 2 {
		if _, _, _, err := pair.chain.joined.Commit(nil); err != nil {
			t.Fatalf("Commit(nil): %v", err)
		}
		if err := pair.chain.joined.MergePendingCommit(); err != nil {
			t.Fatalf("MergePendingCommit: %v", err)
		}
		if err := restarted.AdvanceEpoch(pair.chain.pqSecret); err != nil {
			t.Fatalf("AdvanceEpoch: %v", err)
		}
	}
	// and one more filing through the public door, with the SAME octets, because the invariant
	// is over every writer and not only over AdvanceEpoch.
	if err := restarted.InstallPqSecret(1, pair.chain.pqSecret); err != nil {
		t.Fatalf("InstallPqSecret(1, the same secret): %v", err)
	}

	// 1. THE INVARIANT. Every entry is one value, and the count of entries is checked with it so
	// that a table of one entry cannot satisfy "all entries agree" vacuously.
	var entries int
	var distinctValues int
	var premise bool
	if err := restarted.do(func() {
		premise = restarted.pqLifetime
		entries = len(restarted.pqSecrets)
		seen := [][]byte{}
		for _, secret := range restarted.pqSecrets {
			isNew := true
			for _, already := range seen {
				if bytes.Equal(already, secret) {
					isNew = false
					break
				}
			}
			if isNew {
				seen = append(seen, secret)
			}
		}
		distinctValues = len(seen)
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !premise {
		t.Fatalf("the premise was dropped by a run that handed in one secret throughout, so the invariant below is about some other session")
	}
	if entries < 2 {
		t.Fatalf("the table holds %d entry(ies) and the invariant is over a table of several; one entry agrees with itself", entries)
	}
	if distinctValues != 1 {
		t.Errorf("the table holds %d entries and %d distinct values while the premise stands, want 1; the premise standing IS the claim that every entry is the same octets, and it is what makes the arm order unobservable",
			entries, distinctValues)
	}

	// 2. THE ORDER, in the state step 4 makes reachable. It is planted on the loop because no
	// door can produce it: filing a differing value is what drops the premise. The control is in
	// the same pass -- the planted value differs from the one at this session's epoch -- without
	// which the assertion could be satisfied by either arm.
	var at uint64
	planted := make([]byte, PqSecretBytes)
	for i := range planted {
		planted[i] = byte(0x2D ^ (i * 11))
	}
	if err := restarted.do(func() {
		at = restarted.epoch
		if bytes.Equal(restarted.pqSecrets[at], planted) {
			t.Fatalf("the planted value equals the entry at this session's own epoch, so the two arms answer the same thing and the order cannot be read")
		}
		zeroize(restarted.pqSecrets[1])
		restarted.pqSecrets[1] = append([]byte(nil), planted...)
		// and the premise is left STANDING, which is the whole of what makes this the mutant's
		// state rather than the ordinary one.
		restarted.pqLifetime = true
		secret, err := restarted.pqSecretForOnLoop(1)
		if err != nil {
			t.Errorf("epoch one is in the table and the lookup answered %v", err)
			return
		}
		if bytes.Equal(secret, restarted.pqSecrets[at]) {
			t.Errorf("the lookup answered epoch one out of the entry at epoch %d: the PREMISE arm ran for an epoch the table holds, which is ruling 40's defect with the table already carrying the right answer", at)
		}
		if !bytes.Equal(secret, planted) {
			t.Errorf("the lookup answered epoch one out of neither the entry at epoch one nor the entry at epoch %d", at)
		}
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
}

// ── 5b. WHO MAY WRITE THE TABLE, ASSERTED OVER EVERY PRODUCTION SOURCE ──────────────────────────
//
// CASE 5's ARGUMENT RESTS ON THIS AND IT WAS A GREP IN A COMMENT. "installPqSecretOnLoop is the
// only writer, so while the premise stands every entry is the same octets, so the arm order is
// unobservable" is the sentence that decided a surviving mutant was equivalent rather than
// escaped. A sentence load-bearing enough to dispose of a mutant is load-bearing enough to be
// measured, and the same sentence is now doing the same job for the refutation against the entry
// being REPLACED: that comparison is equivalent today for exactly this reason and will stop being
// so the day a wrap files a differing value.
//
// IT IS A CLASS AND NOT A FILE. Every non-test source of this package is parsed and every write of
// the pqSecrets field is reported with the function it is in -- the entry store, the two deletes,
// the whole-field assignment and the constructor's literal -- and the set is held against a
// disposition written down here. A gate that opened pqsecret.go by name would be scoped to the
// address this table has today, which is the blindness ledger item 253 names twice.
//
// THE POSITIVE CONTROL IS INLINE AND IT FIRES FOR ITS OWN REASON: installPqSecretOnLoop's store
// must be in the reading before the reading's zero means anything, and a run that stopped finding
// the field at all reports the same clean set as a complete one.
func TestOnlyOneFunctionOfThisPackageWritesAnEntryOfThePqSecretTable(t *testing.T) {
	fileSet, sources := messagegroupProductionSources(t)

	type tableWrite struct {
		where string
		kind  string
		at    string
	}
	// isTable answers whether an expression is the pqSecrets FIELD, by shape and not by the
	// receiver's spelling, so a body that renamed its receiver is still read.
	isTable := func(node ast.Expr) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		return isSelector && selector.Sel != nil && selector.Sel.Name == "pqSecrets"
	}
	writes := []tableWrite{}
	for _, source := range sources {
		enclosing := ""
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				if typed.Name != nil {
					enclosing = typed.Name.Name
				}
			case *ast.AssignStmt:
				for _, target := range typed.Lhs {
					kind := ""
					if index, isIndex := target.(*ast.IndexExpr); isIndex && isTable(index.X) {
						kind = "entry"
					}
					if isTable(target) {
						kind = "whole field"
					}
					if kind != "" {
						writes = append(writes, tableWrite{where: enclosing, kind: kind,
							at: fileSet.Position(target.Pos()).String()})
					}
				}
			case *ast.CallExpr:
				name, isName := typed.Fun.(*ast.Ident)
				if isName && name.Name == "delete" && 0 < len(typed.Args) && isTable(typed.Args[0]) {
					writes = append(writes, tableWrite{where: enclosing, kind: "delete",
						at: fileSet.Position(typed.Pos()).String()})
				}
			case *ast.KeyValueExpr:
				key, isKey := typed.Key.(*ast.Ident)
				if isKey && key.Name == "pqSecrets" {
					writes = append(writes, tableWrite{where: enclosing, kind: "literal",
						at: fileSet.Position(typed.Pos()).String()})
				}
			}
			return true
		})
	}

	reported := []string{}
	for _, one := range writes {
		reported = append(reported, one.where+" ("+one.kind+")")
	}
	slices.Sort(reported)
	t.Logf("%d write(s) of the pq_secret table in this package's production source: %v", len(writes), reported)

	// THE CONTROL, in the same reading: the one store this whole argument is about.
	foundStore := false
	for _, one := range writes {
		if one.where == "installPqSecretOnLoop" && one.kind == "entry" {
			foundStore = true
		}
	}
	if !foundStore {
		t.Fatalf("this reading finds no entry store in installPqSecretOnLoop, so it is not reading the shape it exists for and the disposition below would be satisfied by an empty walk. It read: %v", reported)
	}

	// THE DISPOSITION, WRITTEN DOWN AS A SET RATHER THAN AS A COUNT, so a write that moves house
	// says which house it moved to.
	//
	//   installPqSecretOnLoop      entry      the ONE writer of an entry; case 5's argument is
	//                                         this row and nothing else
	//   dropPqSecretsBelowWindowOnLoop delete the window bound, which forgets and erases together
	//   zeroizeOnLoop              delete     Close, which empties the table entry by entry
	//   zeroizeOnLoop              whole field  and then drops the emptied map
	//   NewGroupSession            literal    the empty table a session starts with
	//
	// A SECOND `entry` ROW IS THE ONE THAT MATTERS. It would mean some other body can put octets
	// under an epoch, and with it case 5's "every entry is the same octets while the premise
	// stands" stops being true -- so the arm-order mutant it disposed of, and the
	// replace-refutation this file measures on planted state, both stop being equivalent and
	// start being escaped. Whoever adds one moves case 5's paragraph in the same commit.
	want := []string{
		"NewGroupSession (literal)",
		"dropPqSecretsBelowWindowOnLoop (delete)",
		"installPqSecretOnLoop (entry)",
		"zeroizeOnLoop (delete)",
		"zeroizeOnLoop (whole field)",
	}
	if !slices.Equal(reported, want) {
		t.Errorf("the writers of the pq_secret table are\n  %v\nand the disposition is\n  %v\nEvery difference is a body that can now put octets under an epoch, or one that stopped being able to; case 5's equivalence argument and the replace-refutation of installPqSecretOnLoop both rest on there being exactly one entry writer",
			reported, want)
	}
}

// ── 5c. THE REFUTATION AGAINST THE ENTRY BEING REPLACED, IN THE STATE THAT MAKES IT LOAD-BEARING ─
//
// installPqSecretOnLoop makes TWO comparisons and one of them is equivalent to the other today,
// for case 5b's reason: while the premise stands every entry is the same octets, so "differs from
// the entry at self.epoch" and "differs from the entry being replaced" fire together. Saying that
// out loud is the difference between a comparison that is redundant and one that is unmeasured --
// the previous pass through this file found a surviving mutant that turned out to be equivalent,
// and the lesson recorded was to assert BOTH the invariant and the thing it makes unobservable.
//
// SO THE STATE IS PLANTED, because no door produces it: a table holding two different values with
// the premise still standing is exactly what step 4's wrap creates and what nothing today can. In
// it, a value arriving for the epoch whose entry differs must drop the premise, and a session that
// compared only against its own epoch's entry would keep it -- and would then answer every past
// epoch out of today's octets, which is ruling 40's defect with the right answer already in hand.
//
// THE CONTROL IS IN THE SAME PASS AND IT FIRES FOR ITS OWN REASON: the arriving value EQUALS the
// entry at self.epoch, so the first comparison provably cannot be what drops the premise.
func TestTheInstallRefutesAgainstTheEntryItIsAboutToReplaceAndNotOnlyAgainstItsOwnEpoch(t *testing.T) {
	pair := newPastEpochPair(t, "pq-restart-replace-refutation")
	session := restartOpenerOnTheChain(t, pair, pair.chain.pqSecret)

	other := make([]byte, PqSecretBytes)
	for i := range other {
		other[i] = byte(0x47 ^ (i * 13))
	}
	var at uint64
	if err := session.do(func() { at = session.epoch }); err != nil {
		t.Fatalf("do: %v", err)
	}
	if at == 0 {
		t.Fatalf("this session was built at epoch 0 and the plant below needs an epoch beside its own")
	}
	below := at - 1

	var premise bool
	var standingEqualsArriving bool
	if err := session.do(func() {
		// THE PLANT: a differing entry at another epoch, premise left standing. No door can do
		// this -- filing a differing value is what drops the premise -- which is the whole reason
		// it is written here and the whole reason the comparison under test is unobservable
		// through the doors today.
		if held, isHeld := session.pqSecrets[below]; isHeld {
			zeroize(held)
		}
		session.pqSecrets[below] = append([]byte(nil), other...)
		session.pqLifetime = true
		// THE CONTROL: what arrives equals the entry at this session's OWN epoch, so the
		// comparison this file used to make cannot be what fires below.
		arriving := append([]byte(nil), session.pqSecrets[at]...)
		standingEqualsArriving = bytes.Equal(arriving, session.pqSecrets[at])
		if bytes.Equal(arriving, session.pqSecrets[below]) {
			t.Fatalf("the planted entry equals the arriving value, so nothing below differs from anything")
		}
		session.installPqSecretOnLoop(below, arriving)
		premise = session.pqLifetime
	}); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !standingEqualsArriving {
		t.Fatalf("the control did not hold: the arriving value is not the entry at this session's own epoch, so the refutation below could have come from either comparison")
	}
	if premise {
		t.Errorf("a value replaced an entry holding DIFFERENT octets and the group-lifetime premise survived; the install compared only against the entry at this session's own epoch, never against the one it was about to erase, and that is the reading under which an advance destroyed a wrap's pq_secret without a word")
	}
}
