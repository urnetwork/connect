// The behavioural pin for the clone coupling the join body rests on.
//
// WHAT THE COUPLING IS. joinWithTakenKeyPackage assembles mls.JoinKeyMaterial over four copies and
// defers (*JoinKeyMaterial).Zeroize over the result, because that type OWNS every array it carries
// and its own header prescribes the erase. The copies make the erase safe from THIS side. What
// makes it safe from the OTHER side is that connect/mls, on the join path, clones every array of
// that material that it retains -- and engine.go's own header says so out loud, calling its
// defensive copy "a fourth instance of a discipline this path already spells three times".
//
// A DOCUMENTED COUPLING THAT NO TEST DEFENDS IS A COMMENT. Nothing in this package failed if
// connect/mls swapped a clone for a direct assignment, and the symptom of that is SILENT: an
// all-zero ed25519 seed derives a perfectly valid public key, a group whose signer is zeros goes on
// producing well formed signatures, and a member whose leaf HPKE private key is zeros goes on
// looking like a member right up until the next commit it cannot open.
//
// THE THREE SITES THIS PIN STANDS OVER, and it stands over them by OUTCOME rather than by naming
// them -- they are written down here so a reader can check the pin, not so the pin can read the
// list:
//
//	mls/group.go        group.signer                   <- keys.SignPrivate    (the joined group's identity)
//	mls/treekem.go      TreeKEMPrivate.EncryptionPriv  <- keys.EncryptPrivate (its leaf HPKE key)
//	mls/key_package.go  KeyPackage.signPriv            <- the caller's signer (the DEVICE's identity)
//
// The third is reached from NewKeyPackage rather than from the join, and it is the one that costs
// the most: this engine defers keyPackage.Zeroize() over a key package it minted with self.signer,
// so a key package that RETAINED the caller's array rather than cloning it would destroy the
// device's long term signing key on the first key package it ever published -- before any group
// exists to notice.
//
// WHY A BEHAVIOURAL PIN AND NOT A READING OF the mls INTERNALS. Standing rule R7: an erase or a
// copy is observable only through an ALIAS of the array in question, and wherever the far side
// copies, a property that reads the far side is measuring a photograph. Reading the joined group's
// signer proves nothing about whether it was cloned -- it reads the same octets either way. So
// every clause below either holds THIS package's own array, which the engine owns and the coupling
// would destroy, or drives a door and lets a PEER be the judge of the result.
package messagegroup

import (
	"bytes"
	"testing"
)

// TestTheJoinLeavesThisDeviceAndItsNewHandleAbleToWork is the clone coupling pin.
//
// Four clauses, and each is here because it is the only one of the four that catches its own site:
//
//	(1) the DEVICE's signing key survives every door this engine has been driven through. The
//	    array is this package's own, held on the engine, and it is read DIRECTLY -- it is the
//	    alias R7 asks for and not a photograph of one. Two readings, after two doors, so a
//	    failure names which door destroyed it.
//	(2) the device can still sign, judged by a door driven AFTER the join: the next key package
//	    it publishes must name the key it signed with before any of this started.
//	(3) the JOINED HANDLE can still sign, judged by a PEER. A handle whose signer is zeros still
//	    produces a perfectly well formed signature, so the sender is not allowed to be the judge:
//	    the founder opens the record, and the founder verifies against the signature_key the
//	    joiner's own leaf names.
//	(4) the JOINED HANDLE can still open a path addressed to its own leaf. Its leaf HPKE private
//	    key is not on the signing path at all and clause 3 is green over a destroyed one, so the
//	    founder commits again -- a commit with no proposals still carries an UpdatePath -- and
//	    the joiner has to decrypt the path secret with the key the join handed it.
//
// THE CONTROL IS THE AT-CONSTRUCTION COPY AND IT IS NOT OPTIONAL. "Non-zero afterwards" is
// satisfied by an array that was non-zero for some other reason, and "all zero afterwards" is
// satisfied vacuously by an empty one, so clause 1 refuses an empty or already-zero control before
// it reads anything.
func TestTheJoinLeavesThisDeviceAndItsNewHandleAbleToWork(t *testing.T) {
	// the fixture publishes the joiner's key package and commits the add, so by the time it
	// returns this engine has already been through NewKeyPackage once.
	fixture := newEngineJoinFixture(t, "the-clone-coupling-pin", true)

	device, isTheAdapter := fixture.joiner.engine.(*connectMlsEngine)
	if !isTheAdapter {
		t.Fatalf("the joiner's engine is %T and not the connect/mls adapter, so clause 1 has no array to hold",
			fixture.joiner.engine)
	}
	control := fixture.joiner.signer
	if len(control) == 0 {
		t.Fatal("the fixture kept no copy of this device's signing key, so every clause below would compare against nothing")
	}
	if isAllZero(control) {
		t.Fatal("this device's signing key was all zero when it was handed to the engine, so 'it is not all zero afterwards' cannot fail and clause 1 is vacuous")
	}

	// ---- clause 1a: the door the FIXTURE drove, which is NewKeyPackage ----
	//
	// It is read BEFORE the join so that a failure names the right site. A key package constructor
	// that retained the caller's seed instead of cloning it erases device_sig here, under this
	// engine's own deferred keyPackage.Zeroize(), and the join that follows never happens: the
	// joiner's published leaf names the real key and the material it would assemble names zeros.
	if len(device.signer) == 0 {
		t.Fatal("the engine's signing key is EMPTY before the join, so every reading of it below is vacuous")
	}
	if !bytes.Equal(device.signer, control) {
		t.Fatalf("this device signed with %x when its engine was built and the engine holds %x before it has joined anything. The only door driven in between is NewKeyPackage, and this engine defers keyPackage.Zeroize() over a key package it minted with that very array: the key package retained it instead of cloning it",
			control, device.signer)
	}

	joined, err := fixture.joiner.engine.JoinFromWelcome(fixture.welcome, fixture.ratchetTree)
	if err != nil {
		t.Fatalf("the joiner's JoinFromWelcome: %v", err)
	}
	defer joined.Close()

	// ---- clause 1b: the join ----
	if len(device.signer) == 0 {
		t.Fatal("the engine's signing key is EMPTY after the join, so every reading of it below is vacuous")
	}
	if isAllZero(device.signer) {
		t.Errorf("this device's long term signing key reads all zero after ONE join. It signed with %x before it; the join's material was assembled over that array rather than over a copy of it, and nothing anywhere refuses afterwards -- an all-zero seed derives a perfectly valid public key",
			control)
	}
	if !bytes.Equal(device.signer, control) {
		t.Errorf("this device signed with %x before the join and its engine holds %x after it",
			control, device.signer)
	}

	// ---- clause 2: the device can still sign, through a door driven after the join ----
	published, err := fixture.joiner.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the engine's NewKeyPackage after the join: %v", err)
	}
	if named := engineKeyPackageLeafKeyOf(t, published); !bytes.Equal(named, fixture.joiner.signerPub) {
		t.Errorf("the key package this device publishes AFTER the join names %x as its leaf signature_key and this device signs with %x",
			named, fixture.joiner.signerPub)
	}

	// ---- clause 3: the joined handle can still sign, and the FOUNDER is the judge ----
	//
	// Protect signs the framed content and Unprotect verifies that signature against the
	// signature_key the sender's leaf names. A joined group holding zeros signs just as happily as
	// one holding the device's key, so a clause that asked the joiner whether its own message was
	// well formed would be green over a destroyed group.
	aad := []byte("the-clone-coupling-pin-aad")
	plaintext := []byte("a record the joiner seals with the key its join handed it")
	protected, err := joined.Protect(aad, plaintext)
	if err != nil {
		t.Fatalf("the joined handle's Protect: %v", err)
	}
	aadBack, plainBack, senderLeaf, _, err := fixture.handle.Unprotect(protected)
	if err != nil {
		t.Fatalf("the founder opening a record the joiner sealed: %v. The joiner's group signed it with a key the founder cannot verify against the signature_key the joiner's own leaf names -- the join retained the material's SignPrivate rather than cloning it, and the deferred erase reached the group",
			err)
	}
	if !bytes.Equal(aadBack, aad) || !bytes.Equal(plainBack, plaintext) {
		t.Errorf("the founder opened %q/%q and the joiner sealed %q/%q", aadBack, plainBack, aad, plaintext)
	}
	if senderLeaf != joined.OwnLeafIndex() {
		t.Errorf("the founder reads the sender as leaf %d and the joiner is at leaf %d",
			senderLeaf, joined.OwnLeafIndex())
	}

	// ---- clause 4: the joined handle can still open a path addressed to its own leaf ----
	//
	// A commit with no proposals still carries an UpdatePath, and the node on that path over the
	// joiner's subtree is encrypted to the joiner's LEAF HPKE public key. The private half is the
	// one NewTreeKEMPrivate was handed out of the material, so a join that retained that array
	// instead of cloning it hands the group a leaf key the deferred erase then wipes -- on every
	// successful join, and with no symptom at all until this commit arrives.
	beforeEpoch := joined.Epoch()
	commit, _, _, err := fixture.handle.Commit(nil)
	if err != nil {
		t.Fatalf("the founder's second Commit(nil): %v", err)
	}
	if err := fixture.handle.MergePendingCommit(); err != nil {
		t.Fatalf("the founder's MergePendingCommit: %v", err)
	}
	processed, err := joined.Process(commit)
	if err != nil {
		t.Fatalf("the joined handle processing the founder's next commit: %v. The path secret over this member's subtree is encrypted to its leaf HPKE public key, and the private half the join handed the group was the material's own array rather than a copy of it",
			err)
	}
	if processed.Kind != EngineProcessedCommit {
		t.Fatalf("the founder's commit ingested as kind %d and not a commit", processed.Kind)
	}
	if err := joined.ApplyCommit(processed); err != nil {
		t.Fatalf("the joined handle applying the founder's next commit: %v", err)
	}
	if joined.Epoch() == beforeEpoch {
		t.Fatalf("the joiner is still at epoch %d, so clause 4 ingested nothing and reports clean having read nothing",
			beforeEpoch)
	}
	if joined.Epoch() != fixture.handle.Epoch() {
		t.Errorf("the joiner is at epoch %d and the founder is at %d after the commit",
			joined.Epoch(), fixture.handle.Epoch())
	}
	founderSecret, err := fixture.handle.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the founder's Export at the new epoch: %v", err)
	}
	joinerSecret, err := joined.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the joiner's Export at the new epoch: %v", err)
	}
	if !bytes.Equal(founderSecret, joinerSecret) {
		t.Error("the two sides export different secrets after the commit, so the joiner derived the new epoch from a path secret it opened with the wrong key")
	}
	t.Logf("the device signs with %x after a join and a second key package; its joined handle sealed a record the founder verified, and opened the founder's next commit into epoch %d",
		fixture.joiner.signerPub, joined.Epoch())
}

// TestTheFounderSurvivesFoundingAndClosingItsOwnGroup is the FOURTH member of the class the pin
// above stands over three of, and it is a clause rather than a copy because the measurement said so.
//
// HOW THE FOURTH WAS FOUND, and it was not by reading the join path again. The class was enumerated
// from THIS side: every connect/mls entry point reached from this package's fourteen non-test files,
// intersected with the destinations connect/mls retains and with the twelve erased field names
// mls/erased_field_alias_test.go derives. Four members came out of that, and the pin above stands
// over three -- all three on the JOIN path, because the join is where the coupling was noticed.
// CreateGroup is the fourth: it hands self.signer to mls.NewGroup with no defensive copy of its own,
// and nothing in either package failed if the far side stopped cloning it.
//
// MEASURED BEFORE ANYTHING WAS CHANGED, because "there is no copy at the call site" and "the array
// is retained" are different claims and only the second is a defect. mls/group.go:668 fills the
// founded group's signer with SignaturePrivateKey(cloneBytes(signer)). THE FOUNDER PATH IS SAFE
// TODAY -- by the same clone discipline this pin exists to defend, which is exactly why what it
// needs is a clause and not a copy. A defensive copy added here would let this device survive a
// NewGroup that had stopped cloning, and surviving that is the one thing this clause is for.
//
// WHAT DRIVES THE ERASE, so that this observes something rather than asserting into the air.
// (*Group).Close calls zeroizeSecret(self.signer) at mls/group.go:946, over a field its own comment
// describes as "storage this group DECLARES rather than storage it points at ... NewGroup clones the
// caller's signing key ... so the erase reaches nothing the caller is still holding". That sentence
// is the coupling written down in the far side's own words, and until this test existed nothing held
// it.
//
// AND "NOTHING HELD IT" IS A MEASUREMENT HERE, which is not what this comment used to say. It said
// every fixture in this package that founds a group closes it inside a t.Cleanup, which runs after
// the last assertion of the case that registered it. The conclusion was right and was measured
// independently; the universal is FALSE as written, and not marginally. The query is
// `grep -rnE "^[[:space:]]*t\.Cleanup\(" --include=*_test.go` over this package and it answers TWO
// statements -- engine_test.go:1493, which closes a group handle, and sessionfixture_test.go:507,
// which closes a session -- against FORTY-TWO `defer x.Close()` statements across eight files, with
// engine_test.go:744 closing one inline. A universal written by hand is a list wearing a quantifier.
//
// The measurement it was standing in for: run UNFILTERED over ./mls/... ./message/...
// ./messagegroup/..., the mls/group.go:668 mutation -- filling the founded group's signer with the
// caller's own array instead of cloneBytes(signer) -- reddens NINE pre-existing top-level mls cases
// and ZERO in messagegroup, and all four clauses of the pin above are green over it. This case is
// the one that goes red.
//
// R7, WHICH IS WHY THE ORDER IS FOUND, THEN CLOSE, THEN READ. An erase is observable only through an
// ALIAS of the array in question. Clause 1 holds the engine's own signer array and reads it
// directly, which is that alias. Clause 2 drives a door AFTER the close and lets the comparison be a
// public half captured at construction -- a value no later erase can move, and therefore not a
// photograph of the thing under test.
func TestTheFounderSurvivesFoundingAndClosingItsOwnGroup(t *testing.T) {
	founder := newTestEngine(t)
	device, isTheAdapter := founder.engine.(*connectMlsEngine)
	if !isTheAdapter {
		t.Fatalf("the founder's engine is %T and not the connect/mls adapter, so clause 1 has no array to hold",
			founder.engine)
	}

	// THE CONTROL, and it is not optional for the same reason it is not optional above. "Non-zero
	// afterwards" is satisfied by an array that was non-zero for some other reason, and "all zero
	// afterwards" is satisfied vacuously by an empty one.
	control := founder.signer
	if len(control) == 0 {
		t.Fatal("the fixture kept no copy of this device's signing key, so every clause below would compare against nothing")
	}
	if isAllZero(control) {
		t.Fatal("this device's signing key was all zero when it was handed to the engine, so 'it is not all zero afterwards' cannot fail and clause 1 is vacuous")
	}
	if len(device.signer) == 0 {
		t.Fatal("the engine's signing key is EMPTY before it has founded anything, so every reading of it below is vacuous")
	}
	if !bytes.Equal(device.signer, control) {
		t.Fatalf("this device signed with %x when its engine was built and the engine holds %x before it has founded anything. Nothing has been driven yet, so this is a fixture fault and not a finding",
			control, device.signer)
	}

	handle := founder.createGroup(t, "the-founder-clone-coupling-pin")

	// READ BEFORE THE CLOSE, so a failure names the right site. A NewGroup that retained the
	// caller's array has not destroyed anything yet -- the erase is on Close -- so this reading
	// separates "founding it broke the device" from "closing it did".
	if !bytes.Equal(device.signer, control) {
		t.Fatalf("this device signed with %x before it founded a group and its engine holds %x straight after CreateGroup, with nothing closed yet",
			control, device.signer)
	}

	// THE DOOR THAT DRIVES THE ERASE. GroupHandle.Close is connectMlsHandle.Close is
	// (*Group).Close, and (*Group).Close zeroizes the group's signer field.
	if err := handle.Close(); err != nil {
		t.Fatalf("closing the group this device founded: %v", err)
	}

	// ---- clause 1: the DEVICE's own signing array survived its own group being closed ----
	if len(device.signer) == 0 {
		t.Fatal("the engine's signing key is EMPTY after its group was closed, so every reading below is vacuous")
	}
	if isAllZero(device.signer) {
		t.Errorf("this device's long term signing key reads all zero after it founded ONE group and closed it. It signed with %x before any of this; mls.NewGroup retained the array CreateGroup handed it rather than cloning it, and (*Group).Close zeroized the device through the group. Nothing anywhere refuses afterwards -- an all-zero ed25519 seed derives a perfectly valid public key, so this device goes on publishing leaves and founding groups under a key anybody can derive, with its credential still naming the real identity",
			control)
	}
	if !bytes.Equal(device.signer, control) {
		t.Errorf("this device signed with %x before it founded a group and its engine holds %x after that group was closed",
			control, device.signer)
	}

	// ---- clause 2: the device can still sign, judged by a door driven AFTER the close ----
	//
	// The comparison is against the public half this fixture captured at construction, which is
	// the half of R7 that keeps this from being a photograph: a device whose seed is now zeros
	// mints a perfectly well formed key package naming the public key of the all-zero seed, and a
	// clause that asked the key package whether it agreed with itself would be green over it.
	published, err := founder.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the engine's NewKeyPackage after its own group was closed: %v", err)
	}
	if named := engineKeyPackageLeafKeyOf(t, published); !bytes.Equal(named, founder.signerPub) {
		t.Errorf("the key package this device publishes after founding and closing a group names %x as its leaf signature_key and this device signs with %x. The founded group was handed this device's own array, and closing it erased the key the device signs with",
			named, founder.signerPub)
	}

	// ---- clause 3: and a second group founded afterwards is one a PEER can still verify ----
	//
	// Clause 2 is green over a device whose key was destroyed and whose engine then rebuilt a
	// consistent one, which is not a case this code has -- but "the leaf names the right key" is a
	// statement about one message, and the thing the coupling protects is the device's ability to
	// go on being itself to somebody else. The founder seals a record in a NEW group and the
	// JOINER opens it, verifying the signature against the signature_key the founder's own leaf
	// names.
	second := founder.createGroup(t, "the-founder-clone-coupling-pin-second")
	defer second.Close()
	if founding, _ := engineLeafKeyOf(t, second, second.OwnLeafIndex()); !bytes.Equal(founding, founder.signerPub) {
		t.Fatalf("the group this device founded after the close names %x at its own leaf and this device signs with %x",
			founding, founder.signerPub)
	}
	peer := newTestEngine(t)
	peerKeyPackage, err := peer.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the peer's NewKeyPackage: %v", err)
	}
	if _, err := second.ProposeAdd(peerKeyPackage); err != nil {
		t.Fatalf("ProposeAdd over the peer's key package: %v", err)
	}
	_, welcome, ratchetTree, err := second.Commit(nil)
	if err != nil {
		t.Fatalf("the founder's Commit over one add: %v", err)
	}
	if err := second.MergePendingCommit(); err != nil {
		t.Fatalf("the founder's MergePendingCommit: %v", err)
	}
	joined, err := peer.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("the peer joining the group this device founded after the close: %v", err)
	}
	defer joined.Close()

	aad := []byte("the-founder-clone-coupling-pin-aad")
	plaintext := []byte("a record the founder seals with the key it still has")
	protected, err := second.Protect(aad, plaintext)
	if err != nil {
		t.Fatalf("the founder's Protect in the group it founded after the close: %v", err)
	}
	aadBack, plainBack, senderLeaf, _, err := joined.Unprotect(protected)
	if err != nil {
		t.Fatalf("the peer opening a record this device sealed after founding and closing a group: %v. The peer verifies the signature against the signature_key the sender's own leaf names, so this is the founder having stopped being able to sign as itself",
			err)
	}
	if !bytes.Equal(aadBack, aad) || !bytes.Equal(plainBack, plaintext) {
		t.Errorf("the peer opened %q/%q and the founder sealed %q/%q", aadBack, plainBack, aad, plaintext)
	}
	if senderLeaf != second.OwnLeafIndex() {
		t.Errorf("the peer reads the sender as leaf %d and the founder is at leaf %d",
			senderLeaf, second.OwnLeafIndex())
	}
	t.Logf("this device signs with %x after founding a group, closing it, publishing a key package and founding a second group a peer verified it in",
		founder.signerPub)
}
