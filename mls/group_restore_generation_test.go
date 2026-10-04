// The persisted epoch state carries this member's own SENDER POSITION, and the round trip that
// says why.
//
// WHAT WAS WRONG. groupStateBlob carried the ladder and no consumed-generation state at all, and
// LoadGroup rebuilt the epoch's keys as NewSecretTree(crypto, tree.LeafWidth(), ...Encryption) --
// a tree whose every ratchet starts at generation 0. So a member restored into an epoch it had
// already spoken in drew generation 0 again.
//
// WHAT THAT COSTS, measured through the exported API on a settled group of four before the repair:
// live bob Protects twice and carol opens both, then LoadGroup(...).Protect(...) and carol answers
// "mls: ratchet generation already consumed: generation 0, head 2". Two consequences, and the
// second is the one that makes this a security defect rather than a liveness one:
//
//   - every message the restored member sends is DROPPED by each peer until it burns past that
//     peer's head, and the head differs per peer, so the recovery is not even uniform
//   - two different plaintexts of one epoch are sealed under ONE (key, base nonce) pair for that
//     leaf and generation. The only thing between that and an AEAD nonce collision is the 32 bit
//     reuse_guard framing_protect.go draws per message
//
// WHY IT WAS GREEN. No test in the package Protected after a restore. The restore corpus asserted
// that a restored member could RECEIVE -- open a commit, agree on the epoch authenticator, export
// the same secret -- and every one of those questions is answered identically by a member whose
// sending ratchets have been wound back to zero.
//
// THE REPAIR IS TWO HALVES AND EITHER ALONE IS USELESS. groupStateBlob version 3 carries the
// position, and (*Group).sealAndRecordLocked writes it back before the ciphertext leaves: persist
// used to run only at an epoch boundary, where the sender position is always zero, so a field
// carried without the second half would record nothing but the zero it already assumed.
package mls

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// testConsumedAt is how many generations one leaf's ratchet of one kind has handed out, read off
// the group's own secret tree.
//
// A ratchet that does not exist has consumed nothing, and that is answered as (0, false) rather
// than as zero: "this member never sent" and "this member sent nothing since the epoch opened" are
// the same number and not the same fact, and the cases below need to tell them apart.
func testConsumedAt(t *testing.T, group *Group, leaf LeafIndex, kind RatchetType) (uint64, bool) {
	t.Helper()
	entries, err := group.secretTree.SenderRatchets(leaf)
	if err != nil {
		t.Fatalf("SenderRatchets at leaf %d: %v", leaf, err)
	}
	for _, entry := range entries {
		if entry.Kind == kind {
			return entry.Consumed, true
		}
	}
	return 0, false
}

// TestARestoredMemberSendsUnderAGenerationItsPeersHaveNotSeen is the defect, run as a test.
//
// THE LIVE SENDS ARE THE CONDITION AND NOT DECORATION. What makes a restored sender observable at
// all is a peer whose receiving ratchet for this leaf has moved, so the two Protects below are
// what put carol's head past zero -- and they are asserted to have opened, so a failure at the end
// is about the restore and not about a fixture that could not send in the first place.
//
// THE POSITION IS ASSERTED BEFORE THE MESSAGE IS SENT, which is what keeps this case from passing
// over a build that simply got lucky: a restored member whose ratchet is at generation 0 and a
// peer who happened to accept it would satisfy an assertion made only at the far end.
func TestARestoredMemberSendsUnderAGenerationItsPeersHaveNotSeen(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-generation")
	defer fixture.closeAll()

	bob, carol := fixture.at(t, 1), fixture.at(t, 2)

	sent := [][]byte{[]byte("the first thing bob says"), []byte("the second thing bob says")}
	for at, plaintext := range sent {
		sealed, err := bob.group.Protect(nil, plaintext)
		if err != nil {
			t.Fatalf("the live Protect at %d: %v", at, err)
		}
		opened, err := carol.group.Unprotect(sealed)
		if err != nil {
			t.Fatalf("leaf 2 could not open the live message at %d: %v", at, err)
		}
		if !bytes.Equal(opened.Plaintext, plaintext) {
			t.Fatalf("leaf 2 opened %q at %d, want %q", opened.Plaintext, at, plaintext)
		}
	}
	live, held := testConsumedAt(t, bob.group, bob.leaf, RatchetApplication)
	if !held || live != uint64(len(sent)) {
		t.Fatalf("the live member has consumed %d application generations (held=%v) after %d sends, so this case is not observing a ratchet that moved",
			live, held, len(sent))
	}

	restored, err := LoadGroup(bob.cfg, bob.group.Epoch(), bob.member.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at leaf 1: %v", err)
	}
	defer restored.Close()

	// the position itself, before anything is sealed under it. Without this the case is satisfied
	// by any build whose peers happen to accept a replayed generation.
	consumed, held := testConsumedAt(t, restored, bob.leaf, RatchetApplication)
	if !held {
		t.Fatal("the restored member holds no application ratchet for its own leaf at all, so its next message is generation 0 of a ratchet its peers are two generations past")
	}
	if consumed != live {
		t.Fatalf("the restored member has consumed %d application generations and the live one %d; the next message it seals is one two different plaintexts of this epoch now stand under",
			consumed, live)
	}

	// and the send, opened by a peer whose head is exactly where those two live messages left it.
	plaintext := []byte("what bob says after coming back")
	sealed, err := restored.Protect(nil, plaintext)
	if err != nil {
		t.Fatalf("Protect at the restored member: %v", err)
	}
	opened, err := carol.group.Unprotect(sealed)
	if err != nil {
		t.Fatalf("leaf 2 refused the restored member's message: %v; a restored member whose ratchet restarted draws a generation every peer has already consumed",
			err)
	}
	if !bytes.Equal(opened.Plaintext, plaintext) {
		t.Fatalf("leaf 2 opened %q from the restored member, want %q", opened.Plaintext, plaintext)
	}
	if opened.SenderLeaf != bob.leaf {
		t.Fatalf("leaf 2 read the restored member's message as coming from leaf %d, want %d",
			opened.SenderLeaf, bob.leaf)
	}
	// the OTHER two members open it as well, which is what says the restore is a member of the
	// group rather than of one conversation: the head this message has to clear differs per peer,
	// and owner and dave never received the two live messages at all.
	for _, one := range []*testGroupMember{fixture.at(t, 0), fixture.at(t, 3)} {
		also, err := one.group.Unprotect(sealed)
		if err != nil {
			t.Fatalf("leaf %d refused the restored member's message: %v", one.leaf, err)
		}
		if !bytes.Equal(also.Plaintext, plaintext) {
			t.Fatalf("leaf %d opened %q from the restored member, want %q", one.leaf, also.Plaintext, plaintext)
		}
	}
}

// TestAHandshakeMessageMovesTheRatchetTheRestoreCarries is the other ratchet, and it is here
// because a member's two ratchets are recorded by one field and spent by different doors.
//
// (*Group).Protect draws the application ratchet; a proposal and a commit draw the HANDSHAKE one,
// through the same seal. A repair that recorded only what Protect spent would leave a restored
// member republishing proposals under generations its peers have consumed -- the same defect on
// the ratchet that carries the group's own membership changes.
func TestAHandshakeMessageMovesTheRatchetTheRestoreCarries(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-generation-handshake")
	defer fixture.closeAll()

	bob := fixture.at(t, 1)
	before, _ := testConsumedAt(t, bob.group, bob.leaf, RatchetHandshake)
	if _, err := bob.group.ProposeUpdate(); err != nil {
		t.Fatalf("ProposeUpdate at leaf 1: %v", err)
	}
	after, held := testConsumedAt(t, bob.group, bob.leaf, RatchetHandshake)
	if !held || after != before+1 {
		t.Fatalf("a proposal moved the handshake ratchet from %d to %d (held=%v), so this case is not observing the door it names",
			before, after, held)
	}

	restored, err := LoadGroup(bob.cfg, bob.group.Epoch(), bob.member.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at leaf 1: %v", err)
	}
	defer restored.Close()
	consumed, held := testConsumedAt(t, restored, bob.leaf, RatchetHandshake)
	if !held || consumed != after {
		t.Fatalf("the restored member has consumed %d handshake generations (held=%v) and the live one %d",
			consumed, held, after)
	}
}

// TestAMemberThatHasNotSentRestoresWithNoRatchetOfItsOwn is the empty half of the record, and it
// is the one an "always carry both ratchets" repair would get wrong.
//
// A leaf's two ratchets are built together out of its node secret, and building them CONSUMES that
// secret. So a member that has sent nothing in this epoch has no ratchet at all and its leaf secret
// is still in the tree -- and a restore that materialised two ratchets on its behalf would take
// that secret out of the tree of a member the live group had not taken it for. Nothing downstream
// reports that; it is only visible as this assertion.
func TestAMemberThatHasNotSentRestoresWithNoRatchetOfItsOwn(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-generation-silent")
	defer fixture.closeAll()

	bob := fixture.at(t, 1)
	for _, kind := range []RatchetType{RatchetHandshake, RatchetApplication} {
		if _, held := testConsumedAt(t, bob.group, bob.leaf, kind); held {
			t.Fatalf("the live member already holds a ratchet of kind %d in this epoch, so this case cannot observe the empty record",
				kind)
		}
	}
	restored, err := LoadGroup(bob.cfg, bob.group.Epoch(), bob.member.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at leaf 1: %v", err)
	}
	defer restored.Close()
	for _, kind := range []RatchetType{RatchetHandshake, RatchetApplication} {
		if _, held := testConsumedAt(t, restored, bob.leaf, kind); held {
			t.Fatalf("the restored member holds a ratchet of kind %d that the live one does not, so its leaf node secret has been taken out of the tree on behalf of a member that never sent",
				kind)
		}
	}
	// and it can still send, which is what says the empty record restored a working sender rather
	// than one whose leaf secret went missing.
	sealed, err := restored.Protect(nil, []byte("the first thing this member ever says"))
	if err != nil {
		t.Fatalf("Protect at the restored member: %v", err)
	}
	if _, err := fixture.at(t, 2).group.Unprotect(sealed); err != nil {
		t.Fatalf("leaf 2 refused the first message the restored member sent: %v", err)
	}
}

// TestLoadGroupRefusesASenderRatchetVectorThisBuildDidNotWrite is the corrupted store, over the
// field version 3 added.
//
// Every row is a state this build cannot have produced, and each names the door that catches it.
// The two ORDER rows are the ones with no other door: two entries naming one ratchet type do not
// collapse the way two ladder entries naming one node do -- they OVERWRITE, so the restored member
// stands wherever the entry that happened to come last says, and when that is the earlier of the
// two it has been handed back generations it has already sent under.
func TestLoadGroupRefusesASenderRatchetVectorThisBuildDidNotWrite(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-generation-refusals")
	defer fixture.closeAll()

	bob := fixture.at(t, 1)
	store := bob.cfg.Store.(*testStore)
	// an application message and a proposal, so the persisted state carries BOTH ratchets: every
	// row below edits one entry against another, and a one entry vector has no order to break.
	if _, err := bob.group.Protect(nil, []byte("something to move the application ratchet")); err != nil {
		t.Fatalf("Protect at leaf 1: %v", err)
	}
	if _, err := bob.group.ProposeUpdate(); err != nil {
		t.Fatalf("ProposeUpdate at leaf 1: %v", err)
	}
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
	if len(control.secretTree.ratchets) == 0 {
		t.Fatal("the control restore holds no sender ratchet, so no edit below changes anything that is read")
	}
	control.Close()

	for _, row := range []struct {
		name string
		edit func(blob *groupStateBlob)
		want error
	}{
		{
			name: "one ratchet type named twice",
			edit: func(blob *groupStateBlob) {
				blob.SenderRatchets[0].Kind = blob.SenderRatchets[len(blob.SenderRatchets)-1].Kind
			},
			want: errGroupStateSenderRatchetOrder,
		},
		{
			name: "two ratchets in descending order",
			edit: func(blob *groupStateBlob) {
				last := len(blob.SenderRatchets) - 1
				blob.SenderRatchets[0], blob.SenderRatchets[last] =
					blob.SenderRatchets[last], blob.SenderRatchets[0]
			},
			want: errGroupStateSenderRatchetOrder,
		},
		{
			// a count past the end of the counter. Narrowed into a uint32 head it would park the
			// ratchet somewhere in the middle of the epoch it claims to have finished, which is a
			// restored member sending under generations it has already used -- this field's own
			// defect, reached through the arithmetic.
			name: "more generations consumed than a ratchet has",
			edit: func(blob *groupStateBlob) {
				blob.SenderRatchets[0].Consumed = uint64(1)<<32 + 1
			},
			want: errGroupStateSenderRatchet,
		},
		{
			name: "a ratchet secret of the wrong length",
			edit: func(blob *groupStateBlob) {
				blob.SenderRatchets[0].Secret = blob.SenderRatchets[0].Secret[1:]
			},
			want: errGroupStateSenderRatchet,
		},
	} {
		var blob groupStateBlob
		if err := syntax.UnmarshalLimit(bytes.Clone(raw), &blob, syntax.MaxRatchetTreeLength); err != nil {
			t.Fatalf("%s: the persisted state did not decode: %v", row.name, err)
		}
		if len(blob.SenderRatchets) < 2 {
			t.Fatalf("%s: the persisted state carries %d sender ratchets and every row here needs two",
				row.name, len(blob.SenderRatchets))
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

// TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound is the RECEIVING half of the restore,
// and it is here because the persisted state has nothing in it about that half at all.
//
// WHAT THE BLOB CARRIES IS THIS MEMBER'S OWN SENDER POSITION. Where its receiving ratchets for its
// PEERS stood is not in it, so every peer's head comes back at 0. The earlier disclosure of that
// called it a lost replay guard rather than key reuse, which is true and is not the whole of it:
// MEASURED on this settled four, alice Protects 1026 times, live bob opens all of them, bob is
// restored, alice Protects once more, and bob answers "generation too far ahead: generation 1026,
// head 0, bound 1024" -- and answers it again for every later message alice sends in that epoch,
// because nothing in this package moves a receiving head except a message it ACCEPTS.
//
// THIS CASE USED TO ASSERT THE OPPOSITE OF ITS LAST HALF, and the change is a deliberate loss of
// behaviour rather than a broken fixture. (*ratchet).peekFor answered a distance past the bound by
// walking the head forward by MaxGenerationSkip and THEN refusing, so restored bob lost one message
// and read alice again. That walk was driven by a generation number arriving in sender data sealed
// under a group shared secret: any member could name any other member's leaf at head+1024 and push
// that member's head forward, and every generation below a head is classified consumed. One header
// destroyed the victim's next message, two destroyed 1,025 of them, and it was pre-emptive and
// repeatable. The resynchronisation and the deletion channel were one walk, so closing the second
// closed the first. See (*ratchet).classify and MaxGenerationSkip.
//
// SO WHAT IS ASSERTED NOW IS THE COST, in full: the message past the bound is refused, the next one
// is refused, and so is a message a long way further on. A case that stopped at the first refusal
// could not tell this bound from an off-by-one. What is asserted BESIDE it is that the fixture is
// otherwise healthy -- live bob, which never fell behind, opens every one of the same messages --
// so what is being measured is the restore and not something the fixture did to alice.
//
// The distance is derived from MaxGenerationSkip rather than typed, so a build that changed the
// bound moves this fixture with it instead of quietly bringing the gap back inside it.
func TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound(t *testing.T) {
	crypto := testCrypto(t)
	fixture := testFourMemberGroup(t, crypto, "restore-behind")
	defer fixture.closeAll()
	alice, bob := fixture.at(t, 0), fixture.at(t, 1)

	// alice runs past the bound and LIVE bob opens every one of them, which is what puts live
	// bob's receiving head where the restore is about to lose it.
	ahead := int(MaxGenerationSkip) + 2
	for i := 0; i < ahead; i += 1 {
		plaintext := fmt.Appendf(nil, "alice %d", i)
		sealed, err := alice.group.Protect(nil, plaintext)
		if err != nil {
			t.Fatalf("alice's Protect at %d: %v", i, err)
		}
		opened, err := bob.group.Unprotect(sealed)
		if err != nil {
			t.Fatalf("live bob could not open alice's message at %d: %v", i, err)
		}
		if !bytes.Equal(opened.Plaintext, plaintext) {
			t.Fatalf("live bob opened %q at %d, want %q", opened.Plaintext, i, plaintext)
		}
	}

	restored, err := LoadGroup(bob.cfg, bob.group.Epoch(), bob.member.SigPriv)
	if err != nil {
		t.Fatalf("LoadGroup at leaf 1: %v", err)
	}
	defer restored.Close()

	// the message that lands past the bound. The refusal is the SENTINEL and not merely an error:
	// a restored member that had lost something else about this epoch would fail here too, and the
	// two are repaired differently.
	behind, err := alice.group.Protect(nil, []byte("the one restored bob cannot reach"))
	if err != nil {
		t.Fatalf("alice's Protect past the bound: %v", err)
	}
	if _, err := restored.Unprotect(behind); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("the restored member answered %v for a peer %d generations ahead, want ErrRatchetGenerationTooFarAhead",
			err, ahead)
	}

	// and the ones after it, which is the disclosed cost rather than a second reading of the
	// same refusal. Nothing moves that head, so alice stays unreadable at this member for the
	// rest of the epoch.
	for round := 0; round < 3; round += 1 {
		plaintext := fmt.Appendf(nil, "and the one it still cannot reach %d", round)
		next, err := alice.group.Protect(nil, plaintext)
		if err != nil {
			t.Fatalf("alice's Protect at round %d: %v", round, err)
		}
		if _, err := restored.Unprotect(next); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
			t.Fatalf("the restored member answered %v at round %d, want ErrRatchetGenerationTooFarAhead: a head that moved on one of these moved on a generation number nobody signed",
				err, round)
		}
		// and live bob, which never fell behind, opens every one of them -- so what is being
		// observed is the restore and not something the fixture did to alice.
		opened, err := bob.group.Unprotect(next)
		if err != nil {
			t.Fatalf("live bob could not open the same message at round %d: %v", round, err)
		}
		if !bytes.Equal(opened.Plaintext, plaintext) {
			t.Fatalf("live bob opened %q at round %d, want %q", opened.Plaintext, round, plaintext)
		}
		if opened.SenderLeaf != alice.leaf {
			t.Fatalf("live bob read the message as coming from leaf %d, want %d",
				opened.SenderLeaf, alice.leaf)
		}
	}
	// AND THE RESTORED MEMBER IS NOT SIMPLY BROKEN. It agrees with everybody about the epoch and
	// it can still SPEAK: its own sender position is what the blob carries, and that half of the
	// restore is unaffected by any of this.
	spoken := []byte("the restored member can still speak")
	sealed, err := restored.Protect(nil, spoken)
	if err != nil {
		t.Fatalf("the restored member could not Protect: %v", err)
	}
	heard, err := alice.group.Unprotect(sealed)
	if err != nil {
		t.Fatalf("alice could not open the restored member's message: %v", err)
	}
	if !bytes.Equal(heard.Plaintext, spoken) {
		t.Fatalf("alice opened %q, want %q", heard.Plaintext, spoken)
	}
}
