// The persisted epoch state carries the member's TreeKEM ladder, and the round trip that says why.
//
// WHAT WAS WRONG. groupStateBlob carried OwnEncPriv and RestoreSecret and no direct-path private
// state at all; LoadGroup rebuilt the private half as NewTreeKEMPrivate(ownLeaf, ...), whose
// PathSecrets map starts EMPTY; and (*RatchetTree).DecryptUpdatePath resolves the copath node at the
// common ancestor and asks NodePrivateKey, which for such a state answers only for the member's own
// leaf. So a member restored by LoadGroup could not process the next commit that came from the other
// side of the tree -- and persistence is the whole point of restoring.
//
// WHY IT WAS GREEN. Every group fixture in this package was two members. At two members the only
// node of a sender's filtered direct path that covers the receiver is the root, and the receiver's
// own leaf is the whole of that node's copath resolution, so an empty ladder answers every question
// the receive path ever asks. four_member_group_test.go measures that, over sizes two, three and
// four, and this file is what the four member fixture was built for.
package mls

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// TestARestoredMemberProcessesTheNextCommitFromTheFarSideOfTheTree is the defect, run as a test.
//
// The entry point is read BEFORE the restore and asserted to be a node that is NOT this member's own
// leaf, which is what makes the case observe the ladder rather than the leaf key. Without that
// assertion the same body passes over a two member group, over a four member group whose parents are
// all blank, and over a build that never stored a path secret at all.
//
// The live member is the CONTROL and it processes the same octets, so a failure here is about the
// restore and not about the commit.
func TestARestoredMemberProcessesTheNextCommitFromTheFarSideOfTheTree(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-copath")
	defer fixture.closeAll()

	owner, bob, carol, dave := fixture.at(t, 0), fixture.at(t, 1), fixture.at(t, 2), fixture.at(t, 3)

	entry, sealed := testLadderEntryPoint(t, crypto, bob.group.tree, bob.group.ownPriv, carol.leaf)
	if !sealed {
		t.Fatal("a commit from leaf 2 seals nothing the member at leaf 1 can open, so this fixture is not one group")
	}
	if entry == bob.leaf.NodeIndex() {
		t.Fatal("the member at leaf 1 enters the ladder at its OWN leaf for a commit from leaf 2, so this case observes the leaf key rather than the ladder")
	}

	restored, err := LoadGroup(bob.cfg, bob.group.Epoch(), bob.member.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at leaf 1: %v", err)
	}
	defer restored.Close()
	if len(restored.ownPriv.PathSecrets) == 0 {
		t.Fatal("the restored member holds no path secret at all, so it holds its leaf key and nothing above it")
	}
	restoredEntry, restoredSealed := testLadderEntryPoint(t, crypto, restored.tree, restored.ownPriv, carol.leaf)
	if !restoredSealed || restoredEntry != entry {
		t.Fatalf("the restored member enters the ladder at node %d (sealed=%v) and the live one at node %d",
			restoredEntry, restoredSealed, entry)
	}

	result, err := carol.group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit at leaf 2: %v", err)
	}
	// the control first: the live member at the same leaf, holding the same ladder, opens it.
	fixture.applyAt(t, bob.group, result.Commit)

	processed, err := restored.ProcessMessage(result.Commit)
	if err != nil {
		t.Fatalf("the restored member could not process the commit from leaf 2, which is sealed to node %d: %v",
			entry, err)
	}
	if err := restored.ApplyCommit(processed); err != nil {
		t.Fatalf("the restored member could not apply the commit from leaf 2: %v", err)
	}

	fixture.applyAt(t, owner.group, result.Commit)
	fixture.applyAt(t, dave.group, result.Commit)
	if err := carol.group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit at leaf 2: %v", err)
	}
	fixture.requireOneEpoch(t)

	// and the restored member derives the SAME epoch secrets, which is the assertion the epoch
	// authenticator is for: it is DeriveSecret(epoch_secret, "authentication"), so agreeing on it is
	// agreeing on the epoch secret rather than on some public function of the tree. The exporter is
	// a second, independent expansion of the same parent, so a build that agreed on one and not the
	// other is reported here rather than one epoch later.
	if !bytes.Equal(restored.EpochAuthenticator(), carol.group.EpochAuthenticator()) {
		t.Fatal("the restored member entered a different epoch from the member whose commit opened it")
	}
	mine, err := restored.Export("restore-copath", nil, 32)
	if err != nil {
		t.Fatalf("Export at the restored member: %v", err)
	}
	theirs, err := carol.group.Export("restore-copath", nil, 32)
	if err != nil {
		t.Fatalf("Export at leaf 2: %v", err)
	}
	if !bytes.Equal(mine, theirs) {
		t.Fatal("the restored member and the committer export different secrets from the epoch they agree the authenticator of")
	}
}

// TestARestoredMemberHoldsTheLadderThatWasPersisted is the round trip stated over the state itself
// rather than over what it can do, so a build that carried the ladder and then read it back under
// the wrong node indices fails here as well as at the decrypt.
//
// The comparison is over the NODE each secret belongs to and not over their order. That is the whole
// argument (*TreeKEMPrivate).PathSecrets is a map for: a member's filtered direct path gains and
// loses nodes as the group changes shape, so a ladder that round tripped by position would be a
// ladder silently renamed the first time it was read back against a differently filtered path.
func TestARestoredMemberHoldsTheLadderThatWasPersisted(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-ladder")
	defer fixture.closeAll()

	for _, one := range fixture.members {
		restored, err := LoadGroup(one.cfg, one.group.Epoch(), one.member.SigPriv)
		if err != nil {
			t.Fatalf("LoadGroup at leaf %d: %v", one.leaf, err)
		}
		live := one.group.ownPriv
		if len(live.PathSecrets) == 0 {
			t.Fatalf("the live member at leaf %d holds no path secret, so this row compares two empty ladders",
				one.leaf)
		}
		if len(restored.ownPriv.PathSecrets) != len(live.PathSecrets) {
			t.Errorf("leaf %d: the restored ladder holds %d rungs and the live one %d",
				one.leaf, len(restored.ownPriv.PathSecrets), len(live.PathSecrets))
		}
		for node, secret := range live.PathSecrets {
			got, held := restored.ownPriv.PathSecrets[node]
			if !held {
				t.Errorf("leaf %d: the restored ladder holds no secret for node %d", one.leaf, node)
				continue
			}
			if !bytes.Equal(got, secret) {
				t.Errorf("leaf %d: the restored secret for node %d is not the one that was persisted",
					one.leaf, node)
			}
		}
		// and the restored ladder is storage of its own: a restore that handed back a view of the
		// decoder's arrays would hold key material LoadGroup erases on its way out, which is a
		// ladder of zeros that derives one wrong node key per rung.
		for node, secret := range restored.ownPriv.PathSecrets {
			if len(secret) != crypto.HashSize() {
				t.Errorf("leaf %d: the restored secret for node %d is %d octets, want %d",
					one.leaf, node, len(secret), crypto.HashSize())
			}
			if bytes.Equal(secret, make([]byte, crypto.HashSize())) {
				t.Errorf("leaf %d: the restored secret for node %d is all zeros, which is what an erased array answers",
					one.leaf, node)
			}
		}
		restored.Close()
	}
}

// TestLoadGroupRefusesALadderThatDoesNotDeriveTheKeysItsTreeCarries holds the two doors that stand
// between a corrupted ladder and a member that derives private keys nobody's public half matches.
//
// They are needed because the interim transcript comparison LoadGroup already makes cannot see any of
// this. The interim hash is a commitment to the restore secret and to the kind that says how to use
// it; a path secret is not an input to the key schedule at all, so every edit below leaves that
// comparison agreeing. Measured: with the Consistent call removed, every derivation row here restores
// with a nil error.
//
// TWO DOORS AND NOT ONE, and the second is here because the first cannot reach it. A repeated node
// collapses into a single map entry when the ladder is rebuilt -- the restored member then holds one
// fewer rung than it persisted, and if the surviving entry is the correct one, the derivation check
// passes over a ladder with a hole in it. Measured while writing this file: the "one node twice" row
// below restored with a nil error until the decoder started refusing a vector that is not strictly
// increasing.
func TestLoadGroupRefusesALadderThatDoesNotDeriveTheKeysItsTreeCarries(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-refusal")
	defer fixture.closeAll()

	bob := fixture.at(t, 1)
	store := bob.cfg.Store.(*testStore)
	epoch := bob.group.Epoch()
	raw, err := store.GetGroupState([]byte(fixture.groupId), epoch)
	if err != nil {
		t.Fatalf("the member at leaf 1 persisted no state for epoch %d: %v", epoch, err)
	}
	raw = bytes.Clone(raw)

	// the control: the state as written restores, so every refusal below is about the one edit it
	// made rather than about a fixture that could not restore at all.
	control, err := LoadGroup(bob.cfg, epoch, bob.member.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup over the state as written: %v", err)
	}
	if len(control.ownPriv.PathSecrets) == 0 {
		t.Fatal("the control restore holds an empty ladder, so no edit below changes anything that is read")
	}
	control.Close()

	for _, row := range []struct {
		name string
		edit func(blob *groupStateBlob)
		want error
	}{
		{
			name: "one flipped bit in a rung",
			edit: func(blob *groupStateBlob) { blob.PathSecrets[0].Secret[0] ^= 0x01 },
			want: errGroupStatePathSecret,
		},
		{
			// the LAST entry, so the vector stays strictly increasing -- 4096 is above every node
			// this tree has -- and the refusal is the derivation one rather than the order one.
			name: "a rung for a node this tree does not have",
			edit: func(blob *groupStateBlob) { blob.PathSecrets[len(blob.PathSecrets)-1].Node = 4096 },
			want: errGroupStatePathSecret,
		},
		{
			// the rung filed under a node that is already in the vector. It is the row the
			// derivation check cannot see: the two entries collapse into one map entry, the
			// survivor derives its node's key correctly, and what is lost is a rung.
			name: "one node named twice",
			edit: func(blob *groupStateBlob) {
				blob.PathSecrets[0].Node = blob.PathSecrets[len(blob.PathSecrets)-1].Node
			},
			want: errGroupStateLadderOrder,
		},
		{
			// and the same door reached from the order alone, which is a state this build did not
			// write: marshalState sorts by node and the keys of a map are unique.
			name: "two rungs in descending order",
			edit: func(blob *groupStateBlob) {
				last := len(blob.PathSecrets) - 1
				blob.PathSecrets[0], blob.PathSecrets[last] = blob.PathSecrets[last], blob.PathSecrets[0]
			},
			want: errGroupStateLadderOrder,
		},
	} {
		var blob groupStateBlob
		if err := syntax.UnmarshalLimit(bytes.Clone(raw), &blob, syntax.MaxRatchetTreeLength); err != nil {
			t.Fatalf("%s: the persisted state did not decode: %v", row.name, err)
		}
		if len(blob.PathSecrets) < 2 {
			t.Fatalf("%s: the persisted state carries %d rungs and every row here needs two",
				row.name, len(blob.PathSecrets))
		}
		row.edit(&blob)
		edited, err := syntax.MarshalLimit(&blob, syntax.MaxRatchetTreeLength)
		if err != nil {
			t.Fatalf("%s: re-encoding the edited state: %v", row.name, err)
		}
		if err := store.PutGroupState([]byte(fixture.groupId), epoch, edited); err != nil {
			t.Fatalf("%s: writing the edited state: %v", row.name, err)
		}
		loaded, err := LoadGroup(bob.cfg, epoch, bob.member.SigPriv)
		if !errors.Is(err, row.want) {
			if loaded != nil {
				loaded.Close()
			}
			t.Fatalf("%s: LoadGroup = %v, want %v", row.name, err, row.want)
		}
		if loaded != nil {
			loaded.Close()
			t.Fatalf("%s: LoadGroup refused and answered a group as well", row.name)
		}
	}
	// the store is left holding the last edited state, so it is written back: a fixture that left a
	// refused state behind would make the deferred closes below report against octets this case
	// broke on purpose.
	if err := store.PutGroupState([]byte(fixture.groupId), epoch, raw); err != nil {
		t.Fatalf("restoring the state this case edited: %v", err)
	}
}

// TestTheWrongLeafKeyStopsOnlyTheCommitsSealedToThatLeaf is one of the session's earlier findings
// re-run against the four member fixture, and it is the one that WAS hiding there.
//
// TestAJoinerThatHeldTheWrongLeafKeyWouldDecryptNothing states, over two members, that a member
// holding another pair's private half in its leaf slot "would decrypt nothing". That is true of a
// group of two and FALSE of a group of four, for the reason this whole file is about: at four members
// a commit from the far side of the tree is sealed to the receiver's sibling subtree root, and a
// member that holds a path secret for that node opens the commit without touching its leaf key at
// all. The name states a property of TreeKEM; what it observes is a property of the fixture's size.
//
// What is true at every size is the narrower sentence this case holds, in both directions: the leaf
// key is what opens the commits sealed TO THAT LEAF -- the ones from the member's own sibling -- and
// nothing else depends on it.
func TestTheWrongLeafKeyStopsOnlyTheCommitsSealedToThatLeaf(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "wrong-leaf-key")
	defer fixture.closeAll()

	carol, dave, owner := fixture.at(t, 2), fixture.at(t, 3), fixture.at(t, 0)

	// the two senders this case needs, named by the entry point they seal to rather than by their
	// leaf number, so a change in the tree's shape is reported here instead of being assumed.
	atOwnLeaf, sealed := testLadderEntryPoint(t, crypto, carol.group.tree, carol.group.ownPriv, dave.leaf)
	if !sealed || atOwnLeaf != carol.leaf.NodeIndex() {
		t.Fatalf("a commit from leaf 3 enters the member at leaf 2 at node %d (sealed=%v), want its own leaf %d",
			atOwnLeaf, sealed, carol.leaf.NodeIndex())
	}
	aboveOwnLeaf, sealed := testLadderEntryPoint(t, crypto, carol.group.tree, carol.group.ownPriv, owner.leaf)
	if !sealed || aboveOwnLeaf == carol.leaf.NodeIndex() {
		t.Fatalf("a commit from leaf 0 enters the member at leaf 2 at node %d (sealed=%v), want a node above its own leaf",
			aboveOwnLeaf, sealed)
	}

	other, _, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("draw the key pair this case swaps in: %v", err)
	}
	carol.group.stateLock.Lock()
	carol.group.ownPriv.EncryptionPriv = HpkePrivateKey(bytes.Clone(other))
	carol.group.stateLock.Unlock()

	// the far side first: the leaf key is wrong and the commit opens anyway, because it was never
	// sealed to that leaf. This is the arm the two member case cannot reach.
	fromOwner, err := owner.group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit at leaf 0: %v", err)
	}
	if _, err := carol.group.ProcessMessage(fromOwner.Commit); err != nil {
		t.Fatalf("a member with the wrong leaf key could not open a commit sealed to node %d, which its ladder covers: %v",
			aboveOwnLeaf, err)
	}
	carol.group.ClearPendingCommit()
	owner.group.ClearPendingCommit()

	// and the sibling: the same wrong leaf key, and a commit whose ciphertext for this member stands
	// at that leaf. It does not open, which is the half the two member case was really observing.
	fromDave, err := dave.group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit at leaf 3: %v", err)
	}
	if _, err := carol.group.ProcessMessage(fromDave.Commit); err == nil {
		t.Fatal("a member holding another pair's private half opened a commit sealed to its own published leaf key")
	}
}

// TestLoadGroupRefusesAStateWrittenAtTheLayoutBeforeThisOne is the POSITION of the version check, and
// it is here because TestLoadGroupRefusesAStateThisBuildDoesNotRead cannot see it: that case edits the
// version field of a blob that is otherwise this build's own layout, so it decodes to the last octet
// whatever order the checks are in.
//
// This one hands LoadGroup the layout IMMEDIATELY BEFORE this build's -- every field of it, and no
// sender ratchet vector -- which is the state a previous build of this client actually wrote to its
// own store. The version field is written FIRST precisely so that such a state is refused as a state
// to migrate or discard; a check made after the whole decode had succeeded would answer a truncation
// error instead, which says nothing about why, and that is what this build did until the ladder was
// appended.
//
// THE LAYOUT IT WRITES IS DERIVED FROM THE VERSION CONSTANT, and that is a repair. It wrote version 1
// verbatim while this build wrote 2; version 3 then appended the sender ratchet vector and this case
// went on passing -- 1 is not 3 either -- while observing a layout two steps back. The one whose
// refusal is about a real migration is the layout a running client is actually holding, which is the
// one before this build's.
func TestLoadGroupRefusesAStateWrittenAtTheLayoutBeforeThisOne(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-version")
	defer fixture.closeAll()

	bob := fixture.at(t, 1)
	store := bob.cfg.Store.(*testStore)
	epoch := bob.group.Epoch()
	raw, err := store.GetGroupState([]byte(fixture.groupId), epoch)
	if err != nil {
		t.Fatalf("the member at leaf 1 persisted no state for epoch %d: %v", epoch, err)
	}
	raw = bytes.Clone(raw)

	var blob groupStateBlob
	if err := syntax.UnmarshalLimit(bytes.Clone(raw), &blob, syntax.MaxRatchetTreeLength); err != nil {
		t.Fatalf("the persisted state did not decode: %v", err)
	}
	// the version 1 layout, written out here rather than taken from a fixture file: it is this
	// build's own encoder minus the field version 2 appended, which is exactly what the previous
	// build wrote.
	writer := syntax.NewWriterLimit(syntax.MaxRatchetTreeLength)
	writer.WriteUint16(1)
	writer.WriteOpaque(blob.Context)
	writer.WriteOpaque(blob.Tree)
	writer.WriteOpaque(blob.Confirmed)
	writer.WriteOpaque(blob.Interim)
	writer.WriteUint32(blob.OwnLeaf)
	writer.WriteOpaque(blob.OwnEncPriv)
	writer.WriteUint8(blob.RestoreKind)
	writer.WriteOpaque(blob.RestoreSecret)
	older, err := writer.Bytes()
	if err != nil {
		t.Fatalf("encode the version 1 layout: %v", err)
	}
	if len(older) >= len(raw) {
		t.Fatalf("the version 1 layout is %d octets and this build's is %d, so it is not the shorter one and this case is not about the layout",
			len(older), len(raw))
	}
	if err := store.PutGroupState([]byte(fixture.groupId), epoch, older); err != nil {
		t.Fatalf("writing the version 1 state: %v", err)
	}
	loaded, err := LoadGroup(bob.cfg, epoch, bob.member.SigPriv)
	if loaded != nil {
		loaded.Close()
	}
	if !errors.Is(err, errGroupStateBlobVersion) {
		t.Fatalf("LoadGroup over the version 1 layout = %v, want errGroupStateBlobVersion", err)
	}
	if err := store.PutGroupState([]byte(fixture.groupId), epoch, raw); err != nil {
		t.Fatalf("restoring the state this case replaced: %v", err)
	}
}

// TestAMemberWhoseOwnUpdateACommitCarriesOpensItWithoutTheLadderItJustLost is the OTHER production
// site that hands a member an empty ladder, run at the size where an empty ladder is dangerous.
//
// (*Group).updatedOwnLeafPrivateLocked answers NewTreeKEMPrivate(ownLeaf, stored) -- a fresh state
// with the new leaf key and no rung above it -- for a commit whose proposals replaced this client's
// own leaf, and its comment argues the case: an Update BLANKS the direct path of the leaf it
// replaces, so every rung the client held above that leaf is a secret for a node the commit blanked
// and carrying them forward is what (*TreeKEMPrivate).Consistent refuses.
//
// That argument is sound and it was, until this case, unmeasured above two members -- where it is
// unfalsifiable, because at two members every ciphertext stands at the member's own leaf whatever
// the ladder holds. What makes it true at four is that the blanking runs the other way too: the
// blanked parents resolve DOWN to the leaves under them, so the committer seals this member's rung
// to its leaf rather than to the subtree root it would otherwise have used. The assertions below are
// that pair -- the member did hold rungs, the commit is from the far side of the tree, and it opens
// anyway -- so a change that stopped blanking, or that started carrying the rungs forward, is
// reported here.
func TestAMemberWhoseOwnUpdateACommitCarriesOpensItWithoutTheLadderItJustLost(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "own-update-ladder")
	defer fixture.closeAll()

	owner, carol := fixture.at(t, 0), fixture.at(t, 2)

	// the far side, asserted rather than assumed: without this the case runs at two members in a
	// four member tree.
	entry, sealed := testLadderEntryPoint(t, crypto, carol.group.tree, carol.group.ownPriv, owner.leaf)
	if !sealed || entry == carol.leaf.NodeIndex() {
		t.Fatalf("a commit from leaf 0 enters the member at leaf 2 at node %d (sealed=%v), want a node above its own leaf",
			entry, sealed)
	}
	held := len(carol.group.ownPriv.PathSecrets)
	if held == 0 {
		t.Fatal("the member at leaf 2 holds no rung before its Update, so this case cannot observe one being dropped")
	}

	proposal, err := carol.group.ProposeUpdate()
	if err != nil {
		t.Fatalf("ProposeUpdate at leaf 2: %v", err)
	}
	for _, one := range fixture.members {
		if one.leaf == carol.leaf {
			continue
		}
		if _, err := one.group.ProcessMessage(proposal); err != nil {
			t.Fatalf("leaf %d could not cache the Update from leaf 2: %v", one.leaf, err)
		}
	}
	result, err := owner.group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateCommit at leaf 0 over the Update from leaf 2: %v", err)
	}
	for _, one := range fixture.members {
		if one.leaf == owner.leaf {
			continue
		}
		fixture.applyAt(t, one.group, result.Commit)
	}
	if err := owner.group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit at leaf 0: %v", err)
	}
	fixture.requireOneEpoch(t)

	// and the member is a working member of the epoch its own Update opened: it holds the rungs the
	// commit's path gave it, rather than the fresh empty state the Update left behind.
	if len(carol.group.ownPriv.PathSecrets) == 0 {
		t.Fatal("the member whose Update this commit carried came out of it holding no rung at all, so the next commit from the far side of the tree has nothing to open")
	}
	if err := carol.group.ownPriv.Consistent(crypto, carol.group.tree); err != nil {
		t.Fatalf("the ladder the member at leaf 2 came out with does not derive the keys its tree carries: %v", err)
	}
	// the next commit from the far side, which is the property the paragraph above is really about.
	next, err := owner.group.CreateCommit(nil, nil, nil)
	if err != nil {
		t.Fatalf("the second CreateCommit at leaf 0: %v", err)
	}
	if _, err := carol.group.ProcessMessage(next.Commit); err != nil {
		t.Fatalf("the member whose Update the previous commit carried cannot process the next commit from leaf 0: %v", err)
	}
	carol.group.ClearPendingCommit()
	owner.group.ClearPendingCommit()
}
