// MASTER section 8.4 held as cases: the forgery it closes, the two refusals it owes, the size it
// costs, and the one thing it stops working.
//
// THE PROPERTY THIS FILE EXISTS FOR IS ONE SENTENCE: a member must not be able to forge a message
// from another member. Before 2026-09-15 it could -- RecordKeyZero(class_key, leaf) needs only the
// class key every member holds and a leaf number, and messagegroup/seal.go contained no signature
// at all -- and a case in m1w1repairs_test.go asserted that as a standing property of this
// package. That case is not deleted here. It is NARROWED, to
// TestAnyMemberCanStillSquatAnotherLeafsStreamIndex, which holds the half of the finding that
// survives: a denial rather than a forgery, ledger open item 205.
//
// EVERY NUMBER IN THIS FILE IS MEASURED BY A CASE IN THIS FILE and none is transcribed from the
// ruling. The ladder below re-derives its own column by calling Protect and walking; if the ruling
// and this tree ever disagree, the disagreement is a failure here rather than a comment somebody
// reads past.
package messagegroup

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the property: one member cannot forge a message from another
// ---------------------------------------------------------------------------

// The forgery, written from the attacker's side and refused at a named error.
//
// THE THREE ROLES ARE THREE DIFFERENT DEVICES' WORTH OF STATE and they are not interchangeable:
//
//	A  the founder, whose messages are being forged. It is also the OPENER here, because the
//	   opener must not be the forger -- a forger opening its own frame meets MG-4's self-Unprotect
//	   refusal first and would observe the wrong sentence entirely.
//	B  the joiner, the FORGER. It is a full member holding everything a member holds: the same
//	   storage root, the same three class keys, the same group_handle_key, the same write key. It
//	   holds one thing A does not, which is B's own MLS signing key, and one thing it can never
//	   hold, which is A's.
//
// WHAT B DOES is every step of the seal path, by hand, out of exported symbols: derive A's record
// key from the shared DURABLE class key and A's LEAF NUMBER, compute A's sender_handle from the
// shared group_handle_key and the same leaf number, seal a body under it, hash it, seal a head,
// and mac the record under the group's write key. Every one of those inputs is group-shared by
// construction, which is why no amount of care in the record layer could ever have refused this.
//
// AND B PUTS A REAL MLS FRAME IN IT, which is the strongest form of the attack rather than the
// easiest. A body of arbitrary octets is refused by mls before either of MASTER section 8.4.3's
// refusals is reached, and a case that only did that would leave R1 untested: it would be
// asserting that a malformed body is malformed. So B calls its OWN Protect, with the aad this
// record's position produces, and gets a frame that verifies perfectly -- at leaf B.
//
// THE REFUSAL IS R1 AND ITS ERROR IS NAMED. If the sender binding is ever removed, this case goes
// red with the forged record opening as A's, which is the whole point of writing it.
func TestOneMemberCannotForgeAMessageFromAnother(t *testing.T) {
	pair := newTestPair(t, "one-member-cannot-forge")
	forger := pair.chain.joined
	forgerSession := pair.opener
	victimLeaf := pair.senderLeaf
	opener := pair.sender

	// A tracks its OWN ladder, which is what makes a record attributed to A something A's
	// opener will look at at all. It is also what MG-4 makes impossible for a genuine record,
	// and the control below is at a leaf A can receive from, precisely so this case is not
	// resting on that.
	if err := opener.TrackSender(victimLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("A tracks its own ladder: %v", err)
	}
	if err := opener.TrackSender(pair.openerLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("A tracks B's ladder: %v", err)
	}

	// THE CONTROL FIRST, so that the refusal below is a refusal of the forgery and not of the
	// fixture. Same opener, same class, same ladder machinery, a real signature -- and the one
	// thing that differs is that the record's sender_handle is the handle of the leaf that
	// signed it.
	honest, err := pair.opener.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head B wrote"), []byte("body B wrote"), 0, nil)
	if err != nil {
		t.Fatalf("B's own SealRecord: %v", err)
	}
	gotHead, gotBody, err := opener.OpenRecord(honest)
	if err != nil {
		t.Fatalf("A could not open a record B really wrote, so nothing below is about forgery: %v", err)
	}
	if !bytes.Equal(gotHead, []byte("head B wrote")) || !bytes.Equal(gotBody, []byte("body B wrote")) {
		t.Fatalf("the control record opened to %q/%q", gotHead, gotBody)
	}

	// THE FORGERY. B protects under its own credential, with the aad the record it is about to
	// build produces, and then envelopes it at A's handle on A's ladder.
	forgedHead := []byte("a head attributed to A")
	inner := forgeProtectBoundAt(t, forger, forgerSession, victimLeaf, 0, forgedHead,
		[]byte("Alice never wrote this"))
	forged := repairForgeRecord(t, forgerSession, victimLeaf, 0, forgedHead, inner)
	if forged.Header.SenderHandle != SenderHandle(forgerSession.groupHandleKey, victimLeaf) {
		t.Fatal("the forged record does not carry A's sender_handle, so it is not the record this case is about")
	}

	headPlain, bodyPlain, err := opener.OpenRecord(forged)
	if !errors.Is(err, ErrRecordSenderBinding) {
		t.Errorf("a record sealed by B, attributed to A and carrying a frame B signed opened with %v; want ErrRecordSenderBinding. If it opened, one member can forge a message from another and MASTER section 8.4 is not wired",
			err)
	}
	if headPlain != nil || bodyPlain != nil {
		t.Errorf("the refusal returned %d octets of head and %d of body beside the error",
			len(headPlain), len(bodyPlain))
	}
	t.Logf("B, a full member holding every group-shared secret, sealed a record at A's handle on A's ladder with a frame B really signed, and A refused it: %v", err)

	// AND THE CRUDE FORM, which is the same attack without the frame: the octets a forger wrote
	// before this ruling existed. It is refused one step earlier, by mls, and the error names
	// that rather than naming the sender binding.
	crude := repairForgeRecord(t, forgerSession, victimLeaf, 1,
		[]byte("a head attributed to A"), []byte("Alice never wrote this either"))
	if _, _, err := opener.OpenRecord(crude); !errors.Is(err, ErrRecordInnerFrame) {
		t.Errorf("a record whose body is not an MLS frame at all opened with %v, want ErrRecordInnerFrame", err)
	}
}

// R2's own case: a frame its signer really signed, moved to a position it was not signed for.
//
// WHY IT IS A SEPARATE CASE FROM THE ONE ABOVE. R1 and R2 do not imply each other, and a suite
// that held only the forgery above would go green with R2 deleted. Here the signature is genuine,
// the leaf is genuine and the sender_handle is the signer's own -- so R1 passes, and the only
// thing wrong with the record is WHERE it is. That is MASTER section 8.4.2's re-enveloping: a
// replay into a later conversational position, indistinguishable from the sender saying it again.
//
// WHAT IS EXERCISED HERE IS THE POSITION AND NOT THE CLASS, said rather than implied. The ruling
// argues the class arm at length -- a DURABLE message dropped into EPH(1) self-destructs within
// the hour, an EPH one promoted to PERMANENT never does -- and that arm is the SAME comparison,
// because retention_class and eph_window are inside AAD_body and AAD_body is inside the digest.
// It is not separately driven here: a class move needs a second class key's ladder at both ends,
// which is a fixture and not a property, and
// TestEveryFieldOfARecordIsAuthenticatedByTheOpen already moves every header field of a record and
// requires none of them to open.
func TestASignedFrameCannotBeReEnvelopedIntoAnotherPosition(t *testing.T) {
	pair := newTestPair(t, "re-enveloping")
	forger := pair.chain.joined
	forgerSession := pair.opener
	opener := pair.sender
	if err := opener.TrackSender(pair.openerLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("A tracks B's durable ladder: %v", err)
	}

	// the frame B signs for its own handle at index 0, under the head that record carries, which
	// is where it belongs.
	head := []byte("head")
	inner := forgeProtectBoundAt(t, forger, forgerSession, pair.openerLeaf, 0, head,
		[]byte("a message B really wrote"))

	// THE CONTROL: at the position it was signed for, the same frame opens.
	atHome := repairForgeRecord(t, forgerSession, pair.openerLeaf, 0, head, inner)
	_, gotBody, err := opener.OpenRecord(atHome)
	if err != nil {
		t.Fatalf("a frame at the position it was signed for did not open, so nothing below is about the position: %v", err)
	}
	if !bytes.Equal(gotBody, []byte("a message B really wrote")) {
		t.Fatalf("the control opened to %q", gotBody)
	}

	// MOVED ONE POSITION ALONG. Everything about the record is well formed: B's own handle, B's
	// own ladder, B's own signature, a mac under the group's write key. The frame names index 0
	// and the record is at index 1.
	moved := forgeProtectBoundAt(t, forger, forgerSession, pair.openerLeaf, 0, head,
		[]byte("a message B really wrote"))
	replayed := repairForgeRecord(t, forgerSession, pair.openerLeaf, 1, head, moved)
	if _, _, err := opener.OpenRecord(replayed); !errors.Is(err, ErrRecordPositionBinding) {
		t.Errorf("a frame signed for stream_index 0 and sealed at stream_index 1 opened with %v; want ErrRecordPositionBinding. Without it, any member can replay a message into a later conversational position",
			err)
	}
}

// A REFUSED RECORD MOVES NO RECEIVER RATCHET, so the ruling does not turn a forgery it defeats
// into a denial it causes.
//
// THERE ARE TWO RECEIVER RATCHETS AND THIS CASE HOLDS BOTH, which is the repair. It used to hold
// one. Its first half drives a forged body that is not an MLS frame at all, so the refusal is taken
// by mls's parser before any key is reached, and what is observed is where the refusal sits
// relative to receivers.Commit -- this package's own ladder over stream_index. That is real and it
// is HALF the property the name states. The other half is the MLS ratchet INSIDE the frame, keyed
// on the sender's leaf and a generation, and a body that never parses can no more reach it than it
// can reach the signature. Measured, on the shape that reaches it: before this repair, a record
// carrying a genuine frame moved to the wrong position was refused at R2 -- correctly -- AFTER mls
// had opened the frame and erased the generation it came at, and the true sender's message at that
// generation then never opened again at that receiver, ever. One ordinary record per message an
// attacker wanted deleted.
//
// So half one asks "was the refusal taken before receivers.Commit" and half two asks "was it taken
// before the MLS erase", and the two are reached by different inputs: half one needs a body mls
// refuses, half two needs a body mls ACCEPTS and this package refuses. A case that drove only the
// first reports a property it has only half looked at, which is worse than no case at all, because
// the name is read as coverage.
//
// THE ORDERING IS THE DIFFERENCE between "this record is refused" and "this record is refused AND
// the true sender's own next write is refused behind it" -- a forger who cannot write as Alice can
// still put an envelope at Alice's next index, or lift Alice's own frame into a position it was not
// signed for, and an opener that moved either ladder before refusing would walk past the rung
// Alice's real record needs.
func TestARecordRefusedAtTheInnerFrameMovesNoReceiverRatchet(t *testing.T) {
	pair := newTestPair(t, "refusal-moves-nothing")
	pair.trackDurable(t)

	// ------------------------------------------------------------------
	// HALF ONE: this package's ladder over stream_index.
	// ------------------------------------------------------------------

	// the index A's next record will take, reserved by nobody yet. It is read off a record the
	// sender seals and throws away rather than written down, so a reserver that started
	// somewhere else moves this case with it.
	probe, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("probe"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord to read the ladder position: %v", err)
	}
	next := probe.Header.StreamIndex + 1

	forged := repairForgeRecord(t, pair.opener, pair.senderLeaf, next,
		[]byte("a head at the sender's next index"), []byte("octets that are not an MLS frame"))
	if _, _, err := pair.opener.OpenRecord(forged); !errors.Is(err, ErrRecordInnerFrame) {
		t.Fatalf("the squatted record answered %v, want ErrRecordInnerFrame; nothing below is about the ordering", err)
	}

	// and now the TRUE sender writes at that index. It must open: the refusal above authenticated
	// nothing, so it must have moved nothing.
	if _, _, err := pair.opener.OpenRecord(probe); err != nil {
		t.Fatalf("the sender's earlier record no longer opens: %v", err)
	}
	honest, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("the real one"), 0, nil)
	if err != nil {
		t.Fatalf("the sender's SealRecord: %v", err)
	}
	if honest.Header.StreamIndex != next {
		t.Fatalf("the sender's next record is at stream_index %d and this case squatted %d",
			honest.Header.StreamIndex, next)
	}
	_, gotBody, err := pair.opener.OpenRecord(honest)
	if err != nil {
		t.Fatalf("the true sender's record at stream_index %d no longer opens after a record at that index was refused: %v. The refusals of MASTER section 8.4.3 must be taken BEFORE the receiver ratchet commits, or every refused envelope denies the sender its own next write",
			next, err)
	}
	if !bytes.Equal(gotBody, []byte("the real one")) {
		t.Errorf("the true sender's record opened to %q", gotBody)
	}

	// ------------------------------------------------------------------
	// HALF TWO: the MLS ratchet inside the frame, which the half above cannot reach.
	// ------------------------------------------------------------------
	//
	// Both of MASTER section 8.4.3's refusals are driven, because "a record that is refused" is
	// the whole class and a rule held over one member of it is a rule held over one member of it.
	// Each row takes a record the sender REALLY sealed, lifts its frame out -- which any member
	// can do, the record key is RecordKeyZero(class_key, leaf) and the class key is group shared --
	// and re-envelopes that same frame into a record the opener must refuse. The frame is genuine,
	// so mls opens it, authenticates it and would erase its generation; only this package knows it
	// is in the wrong place. Then the sender's OWN record, the one the frame was lifted from, is
	// opened. It must still open.
	rows := []struct {
		what     string
		leafOf   func() uint32
		index    func(uint64) uint64
		sentinel error
	}{
		{
			what:     "R2, the same frame re-enveloped at another stream_index",
			leafOf:   func() uint32 { return pair.senderLeaf },
			index:    func(at uint64) uint64 { return at + 4 },
			sentinel: ErrRecordPositionBinding,
		},
		{
			what:     "R1, the same frame re-enveloped under another member's sender_handle",
			leafOf:   func() uint32 { return pair.openerLeaf },
			index:    func(at uint64) uint64 { return at },
			sentinel: ErrRecordSenderBinding,
		},
	}
	for _, row := range rows {
		genuine, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), []byte("a message the sender really wrote"), 0, nil)
		if err != nil {
			t.Fatalf("%s: the sender's SealRecord: %v", row.what, err)
		}
		lifted := repairLiftFrame(t, pair.opener, pair.senderLeaf, genuine)
		leaf := row.leafOf()
		if leaf == pair.openerLeaf {
			if err := pair.opener.TrackSender(leaf, message.RetentionDurable, 0, 0, 0); err != nil {
				t.Fatalf("%s: tracking the forger's own ladder: %v", row.what, err)
			}
		}
		moved := repairForgeRecord(t, pair.opener, leaf, row.index(genuine.Header.StreamIndex),
			[]byte("a head somebody else wrote"), lifted)
		if _, _, err := pair.opener.OpenRecord(moved); !errors.Is(err, row.sentinel) {
			t.Fatalf("%s: the re-enveloped record answered %v, want %v; nothing below is about the ordering",
				row.what, err, row.sentinel)
		}
		// THE STAKE. The frame above was the sender's own, at a generation of the sender's own
		// MLS ratchet. If the refusal was taken after mls opened it, that generation is erased
		// at this receiver and the sender's genuine record is unopenable for the rest of time.
		_, gotBody, err := pair.opener.OpenRecord(genuine)
		if err != nil {
			t.Fatalf("%s: the sender's OWN record no longer opens after the refusal above: %v. MASTER section 8.4.3's refusals must be taken BEFORE mls consumes the generation, or any member can permanently delete any other member's message with one ordinary record",
				row.what, err)
		}
		if !bytes.Equal(gotBody, []byte("a message the sender really wrote")) {
			t.Errorf("%s: the sender's own record opened to %q", row.what, gotBody)
		}
	}
}

// ---------------------------------------------------------------------------
// what stops working, and it is not a cost of the AAD
// ---------------------------------------------------------------------------

// MG-4's reproduction: a session cannot open its own application record, and that is MLS rather
// than this package.
//
// Protect consumes a generation of THIS leaf's sending ratchet, and RFC 9420 section 9's secret
// tree gives a member no RECEIVING ratchet for its own leaf, because a member never receives its
// own messages. Spec A section 5.2's A-27 paragraph says the ruling "does not make a working call
// stop working"; it does, and the sentence that stands here is the measurement rather than the
// claim.
//
// THE CONTROL IS THE SAME SESSION SEALING A COMMIT RECORD, which MASTER section 8.4.1's first row
// leaves alone: it carries no application frame, so the record layer opens it exactly as it always
// did. That is what makes this case a statement about the FRAME and not about a record layer that
// has stopped working. It is asked for through OpenCeremonyRecord, which is where that arm's
// records go after the second pass -- the door changed, the opening did not, and the control is
// still the same session getting its own octets back.
func TestASessionCannotOpenItsOwnApplicationRecordAndThatIsMls(t *testing.T) {
	fixture := newTestSession(t, "own-application-record")
	fixture.trackOwn(t)

	application, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("a message this device wrote"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	if _, _, err := fixture.session.OpenRecord(application); !errors.Is(err, ErrRecordInnerFrame) {
		t.Errorf("a session opened a record it sealed itself and answered %v; MASTER section 8.4 makes that body an MLS frame this leaf has no receiving ratchet for, so the refusal is ErrRecordInnerFrame and open item MG-4 is what is unruled about it",
			err)
	}

	// the control: is_commit == 1, MASTER section 8.4.1 row 1, no application frame.
	commit, err := fixture.session.SealRecord(message.RetentionDurable, 0, true,
		[]byte("head"), []byte("the octets a commit record carries"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord of a commit record: %v", err)
	}
	gotHead, gotBody, err := fixture.session.OpenCeremonyRecord(commit)
	if err != nil {
		t.Fatalf("a commit record carries no application frame and must open exactly as it always did: %v", err)
	}
	// AND THE ARM SPLIT, from both sides: the message door refuses it, and the ceremony door
	// refuses the application record above. Without the pair, one door that quietly served both
	// arms would satisfy everything else in this case.
	if _, _, err := fixture.session.OpenRecord(commit); !errors.Is(err, ErrRecordNotAnApplicationRecord) {
		t.Errorf("the message door opened a commit record with %v, want ErrRecordNotAnApplicationRecord", err)
	}
	if _, _, err := fixture.session.OpenCeremonyRecord(application); !errors.Is(err, ErrRecordNotAnApplicationRecord) {
		t.Errorf("the ceremony door opened an application record with %v, want ErrRecordNotAnApplicationRecord", err)
	}
	if !bytes.Equal(gotHead, []byte("head")) || !bytes.Equal(gotBody, []byte("the octets a commit record carries")) {
		t.Errorf("the commit record opened to %q/%q", gotHead, gotBody)
	}
}

// The predicate MASTER section 8.4.1 states as a table, held as the table.
//
// It is derived from two values the sealer already holds and never from a parameter, so this case
// walks the three rows rather than the one arm a round trip happens to take. The attachment arm is
// asked through the ENCODER's answer, which is what the production predicate reads: connect/message
// collapses a nil attachment and an explicit AttachmentNone to no bytes at all, so "the attachment
// is NONE" and "the encoding is empty" are one sentence on both sides of the wire.
func TestWhichRecordsCarryAnInnerFrameIsMasterSection841sTable(t *testing.T) {
	wrap, err := message.EncodeServerAttachment(&message.ServerAttachment{
		Kind: message.AttachmentWrap,
		Wrap: &message.WrapTag{WrapTargetHandle: make([]byte, 16), Epoch: 1},
	})
	if err != nil {
		t.Fatalf("encode a wrap attachment: %v", err)
	}
	if len(wrap) == 0 {
		t.Fatal("a wrap attachment encodes to no bytes, so the third row of the table is unreachable here")
	}
	none, err := message.EncodeServerAttachment(nil)
	if err != nil {
		t.Fatalf("encode no attachment: %v", err)
	}
	explicit, err := message.EncodeServerAttachment(&message.ServerAttachment{Kind: message.AttachmentNone})
	if err != nil {
		t.Fatalf("encode an explicit AttachmentNone: %v", err)
	}
	for _, row := range []struct {
		name       string
		isCommit   bool
		attachment []byte
		want       bool
	}{
		{"a commit record", true, none, false},
		{"a commit record carrying an attachment", true, wrap, false},
		{"an application record", false, none, true},
		{"an application record whose attachment is an explicit NONE", false, explicit, true},
		{"a device wrap", false, wrap, false},
	} {
		if got := isApplicationRecord(row.isCommit, row.attachment); got != row.want {
			t.Errorf("%s: isApplicationRecord answered %v, want %v", row.name, got, row.want)
		}
	}
}

// aad_mls held against a TRANSCRIPTION of MASTER section 8.4.2, and not against itself.
//
// WITHOUT THIS, THE AAD IS UNPINNED. Both ends of this package call one function, so a label spelled
// differently, a digest taken over the wrong preimage or an AAD_body swapped for a hash of it would
// agree with itself forever and be discovered by a second implementation. That is the exact failure
// keyschedule_test.go, recordkey_test.go and handle_test.go exist to close for their own
// derivations, and this is the fourth member of that family.
//
// The label is written out here rather than read off aadMlsLabel for the same reason every other
// transcription in this package is: reading it off the package would move both halves together.
func TestAadMlsIsMasterSection842sDigest(t *testing.T) {
	// THE FOUR TERMS, transcribed. The label carries the VERSION, which is the whole of what
	// separates a v1 opener from a v2 one -- there is no wire signal and format_version
	// deliberately does not bump, so a v2 label written as v1 is a build that interoperates with
	// the wrong half of the world in silence.
	const label = "URmessage/v2/aad/mls"
	const headBindInfo = "rec/v1/head-bind"
	referenceRecordKey := bytes.Repeat([]byte{0x5c}, 32)
	referenceHeadCommit := func(recordKey []byte, headPlain []byte) []byte {
		bindKey := keyScheduleReferenceExpand(recordKey, []byte(headBindInfo), 32)
		mac := hmac.New(sha256.New, bindKey)
		mac.Write(headPlain)
		return mac.Sum(nil)
	}
	referenceU32 := func(v uint32) []byte {
		return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	}

	for _, row := range []struct {
		binding    message.BodyBinding
		generation uint32
		head       []byte
	}{
		{binding: message.BodyBinding{RetentionClass: message.RetentionDurable}, generation: 0, head: nil},
		{
			binding: message.BodyBinding{
				GroupId:        [32]byte{0x01, 0x02},
				SenderHandle:   [16]byte{0xAA},
				Epoch:          9,
				StreamIndex:    4096,
				RetentionClass: message.RetentionEph,
				EphBucket:      3,
				EphWindow:      1 << 33,
			},
			generation: 0x01020304,
			head:       []byte("a head of nine"),
		},
	} {
		aadBody, err := message.AADBody(RecordAeadAlgId, row.binding)
		if err != nil {
			t.Fatalf("AADBody: %v", err)
		}
		head := referenceHeadCommit(referenceRecordKey, row.head)
		preimage := append([]byte(label), aadBody...)
		preimage = append(preimage, referenceU32(row.generation)...)
		preimage = append(preimage, head...)
		// the whole preimage is 160 octets: 20 + 104 + 4 + 32. Asserted rather than assumed,
		// because a term that grew a length prefix would still hash to something.
		if len(preimage) != 160 {
			t.Fatalf("the transcribed preimage is %d octets and MASTER section 8.4.2 fixes it at 160", len(preimage))
		}
		want := sha256.Sum256(preimage)
		got, err := aadMls(row.binding, row.generation, headCommit(referenceRecordKey, row.head))
		if err != nil {
			t.Fatalf("aadMls: %v", err)
		}
		if got != want {
			t.Errorf("aadMls answered %x and MASTER section 8.4.2's H(%q | AAD_body | u32(generation) | head_commit) is %x",
				got, label, want)
		}
		if len(got) != 32 {
			t.Errorf("aad_mls is %d octets and MASTER section 8.4.2 fixes it at 32", len(got))
		}
	}

	// AND IT IS NOT THE PREIMAGE ITSELF, nor an unlabelled digest, which are the two shapes an
	// edit reaches for. Both would be self-consistent across this package's own two ends.
	binding := message.BodyBinding{RetentionClass: message.RetentionDurable, StreamIndex: 7}
	aadBody, err := message.AADBody(RecordAeadAlgId, binding)
	if err != nil {
		t.Fatalf("AADBody: %v", err)
	}
	zeroHead := headCommit(referenceRecordKey, nil)
	got, err := aadMls(binding, 0, zeroHead)
	if err != nil {
		t.Fatalf("aadMls: %v", err)
	}
	if bytes.Equal(got[:], aadBody) {
		t.Error("aad_mls is AAD_body itself; MASTER section 8.4.2 hashes it, and the 256 octet rung is why")
	}
	if unlabelled := sha256.Sum256(aadBody); got == unlabelled {
		t.Error("aad_mls is an UNLABELLED digest of AAD_body; the domain separation label is what keeps this preimage out of every other digest in the system")
	}
	// THE v1 LABEL IS A DIFFERENT DIGEST, which is MASTER section 8.4.2's fail-closed direction
	// in both directions and is the only thing that separates the two versions on the wire.
	v1Preimage := append([]byte("URmessage/v1/aad/mls"), aadBody...)
	if got == sha256.Sum256(v1Preimage) {
		t.Error("aad_mls is still v1's digest; a v2 frame must not verify against a v1 opener's preimage and the label is the only thing that says so")
	}

	// and every field of AAD_body reaches it, AND SO DO THE TWO NEW TERMS -- which is what "four
	// terms" means once it is a comparison rather than a sentence.
	base, err := aadMls(message.BodyBinding{RetentionClass: message.RetentionDurable}, 0, zeroHead)
	if err != nil {
		t.Fatalf("aadMls: %v", err)
	}
	for name, moved := range map[string]struct {
		binding    message.BodyBinding
		generation uint32
		head       [32]byte
	}{
		"group_id":        {binding: message.BodyBinding{RetentionClass: message.RetentionDurable, GroupId: [32]byte{0x01}}, head: zeroHead},
		"sender_handle":   {binding: message.BodyBinding{RetentionClass: message.RetentionDurable, SenderHandle: [16]byte{0x01}}, head: zeroHead},
		"epoch":           {binding: message.BodyBinding{RetentionClass: message.RetentionDurable, Epoch: 1}, head: zeroHead},
		"stream_index":    {binding: message.BodyBinding{RetentionClass: message.RetentionDurable, StreamIndex: 1}, head: zeroHead},
		"retention_class": {binding: message.BodyBinding{RetentionClass: message.RetentionPermanent}, head: zeroHead},
		"eph_window":      {binding: message.BodyBinding{RetentionClass: message.RetentionDurable, EphWindow: 1}, head: zeroHead},
		"generation":      {binding: message.BodyBinding{RetentionClass: message.RetentionDurable}, generation: 1, head: zeroHead},
		"head_commit":     {binding: message.BodyBinding{RetentionClass: message.RetentionDurable}, head: headCommit(referenceRecordKey, []byte("x"))},
	} {
		got, err := aadMls(moved.binding, moved.generation, moved.head)
		if err != nil {
			t.Fatalf("aadMls with %s moved: %v", name, err)
		}
		if got == base {
			t.Errorf("moving %s does not move aad_mls, so a frame signed for one record's position verifies at another's", name)
		}
	}

	// THE GENERATION IS BIG ENDIAN, and that is a separate assertion because a little endian
	// encoder produces a perfectly well formed digest that no peer computes. MASTER section
	// 8.4.3's mutation (d) is exactly this, on the sealer only. 0x01000000 and 0x00000001 are
	// each other's byte reversal, so a build that wrote the four octets the other way round
	// answers this pair swapped.
	bigEndian, err := aadMls(binding, 0x01000000, zeroHead)
	if err != nil {
		t.Fatalf("aadMls: %v", err)
	}
	wantBigEndian := sha256.Sum256(append(append(append([]byte(label), aadBody...),
		0x01, 0x00, 0x00, 0x00), zeroHead[:]...))
	if bigEndian != wantBigEndian {
		t.Errorf("u32(0x01000000) is not encoded most significant octet first; MASTER section 8.4.2 fixes the order and a reversal is a preimage no MLS implementation reproduces")
	}

	// AND head_commit IS KEYED: the same head under a different record_key is a different
	// commitment. Without the key the server, which holds AAD_body and sees aad_mls in the clear,
	// could confirm a guessed sent_at in a few million tries.
	otherKey := bytes.Repeat([]byte{0x5d}, 32)
	if headCommit(referenceRecordKey, []byte("same head")) == headCommit(otherKey, []byte("same head")) {
		t.Error("head_commit does not depend on record_key[i], so it is an unkeyed commitment to the head and the server can confirm a guessed sent_at")
	}
	if headCommit(referenceRecordKey, []byte("a")) == headCommit(referenceRecordKey, []byte("b")) {
		t.Error("head_commit does not depend on the head plaintext at all")
	}
}

// ---------------------------------------------------------------------------
// the size ladder, MEASURED HERE
// ---------------------------------------------------------------------------

// The usable application body per rung once the frame is inside it. MASTER section 8.4.4's column,
// RE-MEASURED by the case below rather than transcribed: if this tree and the ruling disagree, the
// case is what says so.
//
// It is a var and not a const because seal_test.go's minimality clause reads it, and a rung's
// capacity is now a property of the frame rather than of the ladder.
var applicationBodyCapacity = []int{59, 826, 3898, 16186, 65334}

// The frame's overhead over the application plaintext, which is a STEP FUNCTION and not a
// constant: RFC 9420's varint prefix widens at 64 and again at 16,384, and the frame carries two
// of them -- one around the ciphertext and one around the application data inside it.
var applicationFrameOverhead = []struct {
	plaintext int
	overhead  int
}{
	{0, 193}, {1, 193}, {63, 193}, {64, 194}, {16383, 196}, {16384, 198}, {65000, 198},
}

// THE QUERY, published beside the numbers: Protect at each length over a real two member group and
// walk the largest plaintext whose protected form fits rung - lpPrefixBytes.
//
// It is a two member group and a thirty two octet group id because both are inputs to the frame's
// length -- group_id is carried inside the PrivateMessage under a varint, and a group id of a
// different width moves every row. The walk is a bisection rather than a scan because the 64 KiB
// rung would otherwise cost sixty five thousand Protect calls, each of which consumes a ratchet
// generation.
//
// WHAT THE COLUMN COSTS, stated by the case that measures it: the 256 octet rung falls from 252
// usable octets to 59. A text longer than about 59 ASCII characters -- fifteen to twenty CJK
// characters or emoji -- now pays the 1 KiB rung, which is 1,040 stored octets where it paid 272.
// That is 3.8x for a large fraction of real traffic and it is the honest headline of this ruling.
func TestTheSizeLadderCostOfTheInnerFrameIsMeasuredHere(t *testing.T) {
	pair := newTestPair(t, "size-ladder")
	if len(applicationBodyCapacity) != int(message.SizeBucketBlob) {
		t.Fatalf("the ladder has %d rungs below the blob rung and this column has %d entries",
			message.SizeBucketBlob, len(applicationBodyCapacity))
	}
	for bucket := message.SizeBucket(0); bucket < message.SizeBucketBlob; bucket += 1 {
		rung := message.SizeBucketBytes(bucket)
		measured := -1
		low, high := 0, rung
		for low <= high {
			middle := (low + high) / 2
			framed, err := protectLength(t, pair, middle)
			if err != nil {
				t.Fatalf("Protect over a %d octet plaintext: %v", middle, err)
			}
			if framed+lpPrefixBytes <= rung {
				measured = middle
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if measured != applicationBodyCapacity[bucket] {
			t.Errorf("rung %d (%d octets) carries %d octets of application body and this file's column says %d",
				bucket, rung, measured, applicationBodyCapacity[bucket])
		}
		t.Logf("rung %d: %d octets of ct_body plaintext, %d usable before this ruling, %d after -- %d lost",
			bucket, rung, rung-lpPrefixBytes, measured, rung-lpPrefixBytes-measured)
	}
	for _, step := range applicationFrameOverhead {
		framed, err := protectLength(t, pair, step.plaintext)
		if err != nil {
			t.Fatalf("Protect over a %d octet plaintext: %v", step.plaintext, err)
		}
		if framed-step.plaintext != step.overhead {
			t.Errorf("a %d octet plaintext frames to %d octets, an overhead of %d, and this file says %d",
				step.plaintext, framed, framed-step.plaintext, step.overhead)
		}
	}
}

// AND THE MEASUREMENT THE DIGEST WAS CHOSEN ON, which is the one number a reader is most likely to
// take on trust: carried VERBATIM rather than hashed, AAD_body's 104 octets leave the 256 octet
// rung carrying NO APPLICATION BODY AT ALL -- not a short one, not a zero length one.
//
// It is measured rather than argued because the alternative was a real candidate and the
// difference between the two columns is what decided it. A rung that carries nothing turns every
// reaction, receipt and typing indicator -- the whole of the next ruling's feature set, all of them
// tens of octets -- into a 1 KiB record.
func TestCarryingAadBodyVerbatimWouldLeaveThe256RungCarryingNothing(t *testing.T) {
	pair := newTestPair(t, "verbatim-aad")
	verbatim, err := message.AADBody(RecordAeadAlgId, message.BodyBinding{
		RetentionClass: message.RetentionDurable,
	})
	if err != nil {
		t.Fatalf("AADBody: %v", err)
	}
	if len(verbatim) != 104 {
		t.Errorf("AAD_body is %d octets and the ruling's measurement is 104", len(verbatim))
	}
	rung := message.SizeBucketBytes(message.SizeBucket256)
	empty, err := pair.chain.founder.Protect(verbatim, nil)
	if err != nil {
		t.Fatalf("Protect a zero length plaintext under a verbatim AAD_body: %v", err)
	}
	if len(empty)+lpPrefixBytes <= rung {
		t.Errorf("a zero length plaintext under a verbatim AAD_body frames to %d octets and fits the %d octet rung; the whole argument for hashing the aad is that it does not",
			len(empty), rung)
	}
	hashed, err := pair.chain.founder.Protect(make([]byte, 32), nil)
	if err != nil {
		t.Fatalf("Protect a zero length plaintext under a 32 octet aad: %v", err)
	}
	t.Logf("256 octet rung, %d octets of plaintext available: verbatim AAD_body needs %d for an EMPTY body, a 32 octet digest needs %d, so the digest buys %d usable octets and the verbatim form buys none",
		rung-lpPrefixBytes, len(empty), len(hashed), applicationBodyCapacity[0])
}

// The 198 octet band ledger open item 203 is about: bodies that fit before this ruling and do not
// fit after, whose only destination is a blob rung that is not built.
//
// It is a case rather than a note because it is the one place the ruling REMOVES a capability, and
// because the refusal a caller meets is the one it already met one octet further along -- so
// without this, the band would be invisible until somebody sent a 65 KiB message.
func TestTheNinetyEightOctetBandAtTheCeilingNoLongerFits(t *testing.T) {
	fixture := newTestSession(t, "the-64k-ceiling")
	rung := message.SizeBucketBytes(message.SizeBucket64K)
	for _, length := range []int{applicationBodyCapacity[message.SizeBucket64K] + 1, rung - lpPrefixBytes} {
		body := make([]byte, length)
		if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), body, 0, nil); !errors.Is(err, ErrBodyTooLong) {
			t.Errorf("a %d octet body answered %v, want ErrBodyTooLong; it fitted the 64 KiB rung before MASTER section 8.4 and its only destination now is the blob rung, which is ledger open item 203",
				length, err)
		}
	}
	// and the top of the band still seals, so the boundary is the boundary and not the whole rung.
	body := make([]byte, applicationBodyCapacity[message.SizeBucket64K])
	record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, []byte("head"), body, 0, nil)
	if err != nil {
		t.Fatalf("a body of exactly the 64 KiB rung's new capacity did not seal: %v", err)
	}
	if record.Header.SizeBucket != message.SizeBucket64K {
		t.Errorf("a body of the 64 KiB rung's capacity landed on rung %d", record.Header.SizeBucket)
	}
	t.Logf("bodies of %d..%d octets fitted before this ruling and do not fit after; the band is %d octets wide",
		applicationBodyCapacity[message.SizeBucket64K]+1, rung-lpPrefixBytes,
		rung-lpPrefixBytes-applicationBodyCapacity[message.SizeBucket64K])
}

// A body no rung could hold under ANY framing costs neither a stream index nor an MLS generation.
//
// THIS CASE EXISTS BECAUSE THE CLAUSE IT HOLDS DEFENDED NOTHING WITHOUT IT, measured: deleting
// newRecordBuilderOnLoop's early bucketForBody refusal left the whole of ./messagegroup/ green.
// The clause is the half of the ladder refusal that can still be taken BEFORE anything is spent --
// MASTER section 8.4 moved the real bucket behind the frame, so the rung is now chosen after the
// index is reserved and after Protect has consumed a generation, and without the early refusal a
// caller handing in a megabyte would burn one of each on every attempt.
//
// THE BAND AT THE CEILING IS COVERED NOW AND IT WAS NOT, which is MASTER section 8.4.6, RULED
// 2026-09-17, and ledger open item 203's DEFECT half. The early refusal used to run on
// len(bodyPlain), which is necessary and not sufficient, so a body of 65,335..65,532 octets passed
// it, reserved an index, spent a generation, was framed, and was only then refused. It now runs on
// framed_length(len(bodyPlain)), so a body in the band costs nothing either. The PRODUCT half of
// 203 stays open: such a body still has nowhere to go until the blob plane exists.
//
// THIS IS MASTER SECTION 8.4.6's OWN FALSIFIABLE, written as it is written there: seal an over-long
// body twice, then a legal one, and read the legal record's stream_index. Under the old rule the
// band case answered 2; under this one it answers the first index the reserver hands out.
func TestABodyNoRungCouldHoldCostsNeitherAnIndexNorAGeneration(t *testing.T) {
	// THE FIRST INDEX THIS RESERVER HANDS OUT IS 1 AND NOT 0, measured rather than assumed: the
	// fake hands out the first index above its high water and its high water starts at zero. So
	// "nothing was spent" is "the next record is still the first one".
	const firstIndex = uint64(1)
	for name, body := range map[string][]byte{
		"longer than the largest rung": make([]byte, message.SizeBucketBytes(message.SizeBucket64K)+1),
		// 65,400 octets: inside MASTER section 8.4.6's own band, which is the length that
		// ruling names and the one the old rule charged an index and a generation for.
		"in the band at the ceiling": make([]byte, 65400),
		// and both ends of the band, so the boundary is measured rather than sampled.
		"one octet over the framed ceiling": make([]byte, applicationBodyCapacity[message.SizeBucket64K]+1),
		"the last octet the rung admits": make([]byte,
			message.SizeBucketBytes(message.SizeBucket64K)-lpPrefixBytes),
	} {
		fixture := newTestSession(t, "too-long-costs-nothing")
		// TWICE, which is the shape section 8.4.6 publishes: one refusal that spends an index is
		// a defect and two are the same defect counted, and a case that sealed once could not
		// tell "the refusal spent nothing" from "the reserver starts at one".
		for attempt := 0; attempt < 2; attempt += 1 {
			if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
				[]byte("head"), body, 0, nil); !errors.Is(err, ErrBodyTooLong) {
				t.Fatalf("a body %s (%d octets), attempt %d, answered %v, want ErrBodyTooLong",
					name, len(body), attempt, err)
			}
		}
		record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), []byte("body"), 0, nil)
		if err != nil {
			t.Fatalf("SealRecord after two refusals of a body %s: %v", name, err)
		}
		if record.Header.StreamIndex != firstIndex {
			t.Errorf("after two refused bodies %s the next record is at stream_index %d, want %d; the refusal must be taken before the reservation and before the generation, or every attempt burns one of each on a call that can never succeed",
				name, record.Header.StreamIndex, firstIndex)
		}
	}
	t.Logf("a body no rung could hold under ANY framing spends neither a stream index nor an MLS generation, at both ends of the %d octet band MASTER section 8.4.6 rules (ledger item 203's defect half)",
		message.SizeBucketBytes(message.SizeBucket64K)-lpPrefixBytes-applicationBodyCapacity[message.SizeBucket64K])
}

// THE CEILING IS 65,334 AND A BODY OF EXACTLY IT STILL SEALS, which is the other half of the rule
// above: an early refusal that was merely CONSERVATIVE would also spend nothing and would refuse
// legal bodies, and no assertion about an unspent index can tell the two apart.
//
// It walks the whole ladder rather than the top rung alone, because the early refusal is arithmetic
// over a step function and MASTER section 8.4.4 records that the step function published three of
// its four steps for two days -- a build that transcribed the three step form refuses a legal
// 16,300..16,383 octet body, and rung 3's capacity is 16,186, so only a sweep at the boundary sees
// it.
func TestTheFramedEarlyRefusalAdmitsEveryBodyThatFits(t *testing.T) {
	for bucket, capacity := range applicationBodyCapacity {
		fixture := newTestSession(t, fmt.Sprintf("ceiling-rung-%d", bucket))
		record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), make([]byte, capacity), 0, nil)
		if err != nil {
			t.Fatalf("a body of exactly rung %d's capacity (%d octets) was refused: %v; the early refusal runs on a DERIVED framed length and a conservative one refuses legal bodies",
				bucket, capacity, err)
		}
		if int(record.Header.SizeBucket) != bucket {
			t.Errorf("a body of rung %d's capacity landed on rung %d", bucket, record.Header.SizeBucket)
		}
	}
	// AND THE BAND THE THREE STEP FORM GETS WRONG, driven by its own length rather than by a
	// rung's: 16,350 octets is inside 16,300..16,383, where varint(C) widens and varint(P) has
	// not. A build carrying MASTER section 8.4.4's old three step overhead computes 194 here and
	// the truth is 196.
	fixture := newTestSession(t, "the-four-step-band")
	if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), make([]byte, 16350), 0, nil); err != nil {
		t.Errorf("a 16,350 octet body was refused with %v; it is inside the band MASTER section 8.4.4's corrected step function adds and it fits the 64 KiB rung",
			err)
	}
}

// octet_length(ct_body) does not move at any rung, which is the claim MASTER section 8.4.4 makes
// about what this ruling does NOT cost and is the one a codec, a schema and a CHECK all rest on.
//
// The rung is what is sealed and the frame sits inside it, so the ladder's own arithmetic is
// untouched: message.SizeBucketCtBodyBytes is an EQUALITY the codec enforces, and a record whose
// ct_body were the frame's length plus a tag would be a record EncodeRecord refuses.
func TestTheFrameDoesNotMoveOctetLengthOfCtBody(t *testing.T) {
	fixture := newTestSession(t, "ct-body-length")
	for bucket := message.SizeBucket(0); bucket < message.SizeBucketBlob && bucket < 3; bucket += 1 {
		body := make([]byte, applicationBodyCapacity[bucket])
		record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), body, 0, nil)
		if err != nil {
			t.Fatalf("SealRecord over rung %d's capacity: %v", bucket, err)
		}
		if record.Header.SizeBucket != bucket {
			t.Errorf("a body of rung %d's measured capacity landed on rung %d", bucket, record.Header.SizeBucket)
		}
		if want := message.SizeBucketCtBodyBytes(record.Header.SizeBucket); len(record.CtBody) != want {
			t.Errorf("rung %d: ct_body is %d octets and the ladder fixes it at %d",
				bucket, len(record.CtBody), want)
		}
		if _, err := message.EncodeRecord(record); err != nil {
			t.Errorf("rung %d: the record is not one the codec accepts: %v", bucket, err)
		}
	}
}

// ---------------------------------------------------------------------------
// message_id, MASTER section 8.4.5
// ---------------------------------------------------------------------------

// The derivation held against a transcription of MASTER section 8.4.5, written out here rather
// than read off handle.go.
//
// It is the discipline every KAT in this package is written under and the reason is the same:
// reaching for messageIdInfo would let a label change move both halves together and leave this
// file green over an identifier no second implementation computes. The expansion goes through the
// reference HKDF keyschedule_test.go already writes out from RFC 5869.
func TestMessageIdIsMasterSection845sDerivation(t *testing.T) {
	groupHandleKey := make([]byte, 32)
	for i := range groupHandleKey {
		groupHandleKey[i] = byte(0x40 + i)
	}
	groupId := [32]byte{}
	for i := range groupId {
		groupId[i] = byte(i)
	}
	senderHandle := [16]byte{}
	for i := range senderHandle {
		senderHandle[i] = byte(0xB0 + i)
	}
	for _, streamIndex := range []uint64{0, 1, 4096, 1 << 40} {
		info := []byte("mid/v1")
		info = append(info, messageIdReferenceLP(groupId[:])...)
		info = append(info, messageIdReferenceLP(senderHandle[:])...)
		info = append(info, messageIdReferenceU64(streamIndex)...)
		// 6 + (4+32) + (4+16) + 8 = 70, stated by MASTER section 8.4.5 and asserted rather
		// than assumed: a prefix of the wrong width is an info of the wrong length before it
		// is an identifier of the wrong value.
		if len(info) != 70 {
			t.Fatalf("the info is %d octets and MASTER section 8.4.5 makes it 70", len(info))
		}
		want := keyScheduleReferenceExpand(groupHandleKey, info, 32)
		got := MessageId(groupHandleKey, groupId, senderHandle, streamIndex)
		if !bytes.Equal(got[:], want) {
			t.Errorf("stream index %d: MessageId answered %x and MASTER section 8.4.5 gives %x",
				streamIndex, got, want)
		}
	}
}

// Every input is live, and the key is one of them.
//
// A derivation that ignored an argument would be an identifier two different messages share, and
// the two that matter most are the two a reader is least likely to check: stream_index, because
// it is the only thing that distinguishes one sender's messages from each other, and
// group_handle_key, because an unkeyed id is one the message server computes for every record it
// stores.
func TestEveryInputToAMessageIdMovesIt(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x40 + i)
	}
	groupId := [32]byte{0x11}
	handle := [16]byte{0x22}
	base := MessageId(key, groupId, handle, 7)
	otherKey := append([]byte(nil), key...)
	otherKey[0] ^= 0x01
	otherGroup := groupId
	otherGroup[31] ^= 0x01
	otherHandle := handle
	otherHandle[15] ^= 0x01
	for name, got := range map[string][32]byte{
		"a different group_handle_key": MessageId(otherKey, groupId, handle, 7),
		"a different group_id":         MessageId(key, otherGroup, handle, 7),
		"a different sender_handle":    MessageId(key, groupId, otherHandle, 7),
		"the next stream_index":        MessageId(key, groupId, handle, 8),
	} {
		if got == base {
			t.Errorf("%s produces the same message_id, so that input is not in the derivation", name)
		}
	}
}

// BOTH SIDES DERIVE THE SAME OCTETS FROM THE SAME AUTHENTICATED INPUTS, which is the whole of what
// makes this an id rather than a label.
//
// The sender has the reserved stream_index before it seals -- so a reply can name its own parent
// optimistically -- and a receiver reads all three inputs off the PLAINTEXT header, so it needs
// neither the body nor any epoch secret. This case is the second half stated as a comparison: the
// id the sealer could compute equals the id an opener computes off the record, and the opener's
// side reaches nothing but header fields.
//
// AND IT IS NOT record_id. The server assigns that after acceptance, so a client has nothing to
// quote in the record it is sealing; the case asserts the two differ rather than leaving a reader
// to infer it.
func TestAMessageIdIsComputableBeforeTheSendAndFromTheHeaderAlone(t *testing.T) {
	pair := newTestPair(t, "message-id-both-sides")
	pair.trackDurable(t)
	record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	senderHandle, err := pair.sender.SenderHandle()
	if err != nil {
		t.Fatalf("the sender's own handle: %v", err)
	}
	// the sender's side: the three values it holds before it seals.
	fromSender := MessageId(pair.chain.groupHandleKey, record.Header.GroupId, senderHandle,
		record.Header.StreamIndex)
	// the opener's side: three PLAINTEXT header fields and the group handle key every member
	// holds. Nothing here opens anything.
	fromOpener := MessageId(pair.chain.groupHandleKey, record.Header.GroupId,
		record.Header.SenderHandle, record.Header.StreamIndex)
	if fromSender != fromOpener {
		t.Errorf("the sender computes %x and an opener reading the header computes %x", fromSender, fromOpener)
	}
	// and the record's own body is not an input, which is what lets an EPH row whose ct_body has
	// been erased keep its id.
	erased := *record
	erased.CtBody = nil
	if again := MessageId(pair.chain.groupHandleKey, erased.Header.GroupId,
		erased.Header.SenderHandle, erased.Header.StreamIndex); again != fromOpener {
		t.Error("the message_id of a record whose ct_body has been erased is not the id it had")
	}
	// it is not record_id, which the server assigns after acceptance -- so a client sealing a
	// record has nothing to quote of it, and MASTER section 8 keeps it out of every preimage for
	// exactly that reason.
	if record.RecordId != 0 {
		t.Errorf("a record this package sealed carries record_id %d; the server assigns it after acceptance", record.RecordId)
	}
	t.Logf("message_id %x, computed by the sender before the send and by an opener from three plaintext header fields",
		fromOpener)
}

// driftingHandle is a GroupHandle whose Unprotect answers a DIFFERENT sender leaf, or a different
// aad, from the one the pre-ratchet peek reads off the same octets.
//
// IT EXISTS TO MAKE THE SECOND READING SEPARABLE. unframeBodyOnLoop takes MASTER section 8.4.3's
// two refusals twice: once on the peek, which moves no ratchet and is therefore where a refusal is
// free, and once on what Unprotect answers, which is the reading the signature covers and is the
// one that decides. Over the real engine the two agree by construction -- mls's
// TestThePeekAgreesWithTheOpenOnEveryMessageThatOpens is that, swept -- so over the real engine,
// deleting the second reading turns nothing red, and a clause nothing can turn red is a clause the
// suite does not hold. This is the input that separates them: an engine whose two answers differ.
//
// It is not a hypothetical about a hostile engine. It is the shape of the bug the pre-filter could
// introduce -- a peek that drifted from the open would silently become the whole rule -- and the
// refusal below is what says the open's answer is still the one being judged.
type driftingHandle struct {
	GroupHandle
	leafDrift uint32
	aadDrift  bool
	// generationDrift is the THIRD value v2 made R2 a function of, and it is the one MASTER
	// section 8.4.3 predicts no octets can move: the content AEAD's key is derived from the
	// generation the sender data named, so over the real engine a frame that opens at all opened
	// at exactly the generation the peek read. An ENGINE can disagree with itself, which is what
	// this field is: the seam, not the wire.
	generationDrift uint32
}

// Close is a NO-OP, because three sessions in this case share one handle and a GroupSession closes
// the handle it was built over. Without it the first row's cleanup would close the group the second
// row is about, and the second row would report "the group is closed" -- a true sentence about the
// fixture standing where the property should be.
func (self *driftingHandle) Close() error { return nil }

func (self *driftingHandle) Unprotect(frame []byte) ([]byte, []byte, uint32, uint32, error) {
	aad, plaintext, senderLeaf, generation, err := self.GroupHandle.Unprotect(frame)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if self.aadDrift && 0 < len(aad) {
		aad = append([]byte(nil), aad...)
		aad[0] ^= 0xff
	}
	return aad, plaintext, senderLeaf + self.leafDrift, generation + self.generationDrift, err
}

// The second reading is the one that decides, and an engine whose two readings disagree is refused.
//
// The control comes first and it is the whole reason the case is readable: the SAME session over
// the SAME handle with no drift opens the record. So the refusals below are about the drift and not
// about a fixture that never worked.
func TestTheReadingThatDecidesIsTheOneTheSignatureCovers(t *testing.T) {
	chain := newTwoEngineChain(t, "the-reading-that-decides")
	t.Cleanup(chain.close)
	senderLeaf := chain.founder.OwnLeafIndex()

	open := func(t *testing.T, drift *driftingHandle, record *message.Record) ([]byte, error) {
		t.Helper()
		session, err := NewGroupSession(drift, chain.pqSecret, chain.groupHandleKey,
			newStreamIndexMemory(), testClock(), testServerNonce())
		if err != nil {
			t.Fatalf("a session over the drifting handle: %v", err)
		}
		defer session.Close()
		if err := session.TrackSender(senderLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
			t.Fatalf("TrackSender: %v", err)
		}
		_, bodyPlain, err := session.OpenRecord(record)
		return bodyPlain, err
	}

	rows := []struct {
		what     string
		drift    *driftingHandle
		sentinel error
	}{
		{what: "no drift, the control", drift: &driftingHandle{GroupHandle: chain.joined}, sentinel: nil},
		{
			what:     "Unprotect answers another leaf than the peek read",
			drift:    &driftingHandle{GroupHandle: chain.joined, leafDrift: 1},
			sentinel: ErrRecordSenderBinding,
		},
		{
			what:     "Unprotect answers another aad than the peek read",
			drift:    &driftingHandle{GroupHandle: chain.joined, aadDrift: true},
			sentinel: ErrRecordPositionBinding,
		},
		// THE THIRD ROW IS THE ONE MASTER SECTION 8.4.3 PREDICTS NO OCTETS CAN REACH, and it is
		// written anyway because this fixture can reach it. Over the real engine the content
		// AEAD's key is derived from the generation the sender data named, so a frame that opens
		// at all opened at exactly the generation the peek read -- and the owed deletion measured
		// that directly: with the second reading's generation replaced by the peek's, the whole of
		// ./messagegroup/ stayed green. What this row separates is an ENGINE whose two answers
		// disagree, which is the seam rather than the wire, and it is what keeps the clause from
		// being a sentence nothing can turn red.
		{
			what:     "Unprotect answers another generation than the peek read",
			drift:    &driftingHandle{GroupHandle: chain.joined, generationDrift: 1},
			sentinel: ErrRecordPositionBinding,
		},
	}
	for _, row := range rows {
		record, err := chain.founderSession.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), []byte("a message the sender really wrote"), 0, nil)
		if err != nil {
			t.Fatalf("%s: SealRecord: %v", row.what, err)
		}
		bodyPlain, err := open(t, row.drift, record)
		if row.sentinel == nil {
			if err != nil {
				t.Fatalf("%s: %v", row.what, err)
			}
			if !bytes.Equal(bodyPlain, []byte("a message the sender really wrote")) {
				t.Fatalf("%s: the control opened to %q", row.what, bodyPlain)
			}
			continue
		}
		if !errors.Is(err, row.sentinel) {
			t.Errorf("%s: OpenRecord answered %v, want %v. The pre-ratchet peek is a filter and never the answer; the reading the signature covers is what MASTER section 8.4.3 is taken on",
				row.what, err, row.sentinel)
		}
		if bodyPlain != nil {
			t.Errorf("%s: the refusal returned %d octets of body", row.what, len(bodyPlain))
		}
	}
}

// ---------------------------------------------------------------------------
// the arm of MASTER section 8.4.1's table, and who picks it
// ---------------------------------------------------------------------------

// The arm is chosen by two fields no signature covers, so the refusals of MASTER section 8.4.3 were
// OPT-OUT until the door split. This is that, measured from the attacker's side.
//
// isApplicationRecord reads is_commit and the encoded server attachment. Both live in AAD_head,
// AAD_head is sealed under record_key[n], and record_key[0] is RecordKeyZero(class_key, leaf) --
// a class key every member holds and a leaf NUMBER. So the member that seals a record decides
// which row of the table it takes, and before the split a member that did not want to be
// signature-checked simply set is_commit: OpenRecord answered that member's own octets under
// whatever sender_handle it liked, with no signature anywhere on the path.
//
// WHAT IS ASSERTED IS BOTH HALVES, and the second is the uncomfortable one. The message door now
// refuses both ceremony arms by name -- that is the repair. The ceremony door still answers the
// forged octets, because the ceremony arm carries no signature and cannot be authenticated here;
// that is MEASURED and printed rather than left to a reader, because a case that only showed the
// refusal would read as though the arm had been closed. Open item MG-5 is what a ruling owes it.
func TestTheArmOfTheTableIsChosenBySomethingNoSignatureCovers(t *testing.T) {
	pair := newTestPair(t, "the-arm-is-chosen")
	forgerSession := pair.opener
	victimLeaf := pair.senderLeaf
	opener := pair.sender
	if err := opener.TrackSender(victimLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("A tracks its own ladder: %v", err)
	}

	wrap, err := message.EncodeServerAttachment(&message.ServerAttachment{
		Kind: message.AttachmentWrap,
		Wrap: &message.WrapTag{WrapTargetHandle: make([]byte, 16), Epoch: 1},
	})
	if err != nil {
		t.Fatalf("EncodeServerAttachment: %v", err)
	}

	rows := []struct {
		what       string
		isCommit   bool
		attachment []byte
		index      uint64
	}{
		{what: "is_commit set", isCommit: true, attachment: nil, index: 0},
		{what: "a server attachment set", isCommit: false, attachment: wrap, index: 1},
	}
	for _, row := range rows {
		body := []byte("OCTETS B CHOSE, ATTRIBUTED TO A")
		forged := repairForgeRecordArm(t, forgerSession, victimLeaf, row.index,
			row.isCommit, row.attachment, []byte("a head attributed to A"), body)
		if forged.Header.SenderHandle != SenderHandle(forgerSession.groupHandleKey, victimLeaf) {
			t.Fatalf("%s: the forged record does not carry A's sender_handle", row.what)
		}

		// THE REPAIR. The door that returns a message refuses this record, so no call named for
		// opening a message can be made to answer octets no member signed.
		headPlain, bodyPlain, err := opener.OpenRecord(forged)
		if !errors.Is(err, ErrRecordNotAnApplicationRecord) {
			t.Errorf("%s: a record B forged at A's handle with no signature anywhere opened at the MESSAGE door with %v; want ErrRecordNotAnApplicationRecord. A rule an attacker can opt out of is not a rule",
				row.what, err)
		}
		if headPlain != nil || bodyPlain != nil {
			t.Errorf("%s: the refusal returned %d octets of head and %d of body",
				row.what, len(headPlain), len(bodyPlain))
		}

		// AND THE PART THAT IS NOT CLOSED, measured rather than described. The ceremony arm has
		// no signature to check -- Spec A section 5.11 step 5 -- so the ceremony door does answer
		// the attacker's octets under the victim's handle. Its name is the whole of what says so,
		// and nothing that renders a message may call it.
		_, ceremonyBody, err := opener.OpenCeremonyRecord(forged)
		if err != nil {
			t.Fatalf("%s: the ceremony door refused a well formed ceremony record: %v", row.what, err)
		}
		if !bytes.Equal(ceremonyBody, body) {
			t.Fatalf("%s: the ceremony door answered %q", row.what, ceremonyBody)
		}
		t.Logf("%s: the message door refuses it, and the ceremony door answers %q under a sender_handle B does not own. The ceremony arm is authenticated by nothing and open item MG-5 is what a ruling owes it",
			row.what, ceremonyBody)
	}
}

// ---------------------------------------------------------------------------
// MASTER section 8.4.5's message_id, and the door it had none of
// ---------------------------------------------------------------------------

// The sender and an opener compute one id for one record THROUGH THE SESSION, which is the surface
// the derivation had none of.
//
// messagegroup.MessageId has been exported and correct since the ruling and had NO CALLER anywhere
// in connect or in sdk -- so "both sides derive the same value" was a property of a function rather
// than of the build, and a reply, a reaction or a read cursor had nothing to name. What is held
// here is the id coming out of the two sessions that actually have the record: the sender, from the
// record it just sealed and before any submit, and the opener, from the header it parsed.
//
// AND IT IS THE FREE FUNCTION'S ANSWER AND NOT A SECOND DERIVATION. The door supplies the key and
// the header supplies the other three inputs; a door that expanded anything of its own would be a
// second implementation of a formula three documents already state.
func TestTheMessageIdDoorAnswersOneIdAtTheSenderAndAtTheOpener(t *testing.T) {
	pair := newTestPair(t, "message-id-door")
	pair.trackDurable(t)

	record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("a message with an id"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	fromSender, err := pair.sender.MessageIdOf(&record.Header)
	if err != nil {
		t.Fatalf("the sender's MessageIdOf: %v", err)
	}
	fromOpener, err := pair.opener.MessageIdOf(&record.Header)
	if err != nil {
		t.Fatalf("the opener's MessageIdOf: %v", err)
	}
	if fromSender != fromOpener {
		t.Fatalf("the sender derives %x and the opener derives %x for one record", fromSender, fromOpener)
	}
	want := MessageId(pair.chain.groupHandleKey, record.Header.GroupId,
		record.Header.SenderHandle, record.Header.StreamIndex)
	if fromSender != want {
		t.Fatalf("the door answers %x and MASTER section 8.4.5's derivation answers %x; the door must be that formula and not a second one",
			fromSender, want)
	}
	// and the record OPENS, which is what makes the id worth having: an id names a message, and
	// what says the message is that member's is R1 and R2.
	if _, gotBody, err := pair.opener.OpenRecord(record); err != nil {
		t.Fatalf("the record the id names does not open: %v", err)
	} else if !bytes.Equal(gotBody, []byte("a message with an id")) {
		t.Fatalf("the record opened to %q", gotBody)
	}
	t.Logf("message_id %x, taken through the session at both ends rather than through a formula neither end calls", fromSender)

	// THE INDEX IS AN INPUT, which one id cannot say. Two positions of one sender are two ids.
	moved := record.Header
	moved.StreamIndex += 1
	atNext, err := pair.sender.MessageIdOf(&moved)
	if err != nil {
		t.Fatalf("MessageIdOf at the next index: %v", err)
	}
	if atNext == fromSender {
		t.Fatal("two stream indices of one sender answer one id, so the index is not an input")
	}

	// AND A HEADER FROM ANOTHER GROUP IS REFUSED rather than answered. The formula takes the
	// group id as an input, so a foreign header would produce a perfectly well formed id under
	// THIS group's key -- a value no member of either group computes, with no error anywhere.
	foreign := record.Header
	foreign.GroupId[0] ^= 0xff
	if _, err := pair.sender.MessageIdOf(&foreign); !errors.Is(err, ErrRecordNotForThisSession) {
		t.Errorf("a header naming another group answered %v, want ErrRecordNotForThisSession", err)
	}
	if _, err := pair.sender.MessageIdOf(nil); !errors.Is(err, message.ErrRecordNil) {
		t.Errorf("a nil header answered %v, want message.ErrRecordNil", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// forgeBodyBinding is the BodyBinding repairForgeRecord's header produces, built here so a case
// that hands Protect an aad and a case that builds the record cannot disagree about which
// position that record is at.
func forgeBodyBinding(session *GroupSession, leaf uint32, streamIndex uint64) message.BodyBinding {
	return message.BodyBinding{
		GroupId:        session.groupId,
		SenderHandle:   SenderHandle(session.groupHandleKey, leaf),
		Epoch:          session.epoch,
		StreamIndex:    streamIndex,
		RetentionClass: message.RetentionDurable,
		EphBucket:      0,
		EphWindow:      0,
	}
}

// forgeRecordKeyAt is the DURABLE rung of one leaf's ladder at one stream index, out of symbols
// every member of the group holds: RecordKeyZero(class_key, leaf) walked to the index, where the
// class key is group shared by construction. It is repairForgeRecord's own derivation reached by a
// second name, so a case that needs the rung WITHOUT sealing a record can have it.
func forgeRecordKeyAt(session *GroupSession, leaf uint32, streamIndex uint64) []byte {
	recordKey := RecordKeyZero(append([]byte(nil), session.classKeys.Durable...), leaf)
	for walked := uint64(0); walked < streamIndex; walked += 1 {
		recordKey = stepRecordKey(recordKey)
	}
	return recordKey
}

// forgeAadAt is the v2 aad_mls that a DURABLE record at (leaf, streamIndex), carrying this head
// plaintext and framed at this generation, produces.
//
// IT IS WHAT A FORGER HAS TO BE ABLE TO COMPUTE, and the fact that it can is the finding rather
// than the fixture: every input is group shared. What v2 changes is not that a member can build the
// aad -- it always could -- but that the aad the member builds is now pinned to the generation the
// seal actually spends and to the head the record actually carries, so a frame lifted out of one
// record cannot be put under another generation or another head.
func forgeAadAt(t *testing.T, session *GroupSession, leaf uint32, streamIndex uint64,
	generation uint32, headPlain []byte) [32]byte {

	t.Helper()
	recordKey := forgeRecordKeyAt(session, leaf, streamIndex)
	defer zeroize(recordKey)
	aad, err := aadMls(forgeBodyBinding(session, leaf, streamIndex), generation,
		headCommit(recordKey, headPlain))
	if err != nil {
		t.Fatalf("aadMls over the forged position: %v", err)
	}
	return aad
}

// forgeProtectBoundAt is ProtectBound over forgeAadAt: a member seals a frame whose aad names the
// position, the head and the generation of the record it is about to build.
func forgeProtectBoundAt(t *testing.T, handle GroupHandle, session *GroupSession, leaf uint32,
	streamIndex uint64, headPlain []byte, bodyPlain []byte) []byte {

	t.Helper()
	frame, err := handle.ProtectBound(func(generation uint32) ([]byte, error) {
		aad := forgeAadAt(t, session, leaf, streamIndex, generation, headPlain)
		return aad[:], nil
	}, bodyPlain)
	if err != nil {
		t.Fatalf("ProtectBound over the forged position: %v", err)
	}
	return frame
}

// protectLength answers how many octets one plaintext frames to, over the pair's founder handle.
//
// It takes a fresh aad of the width aad_mls is at every call, because the aad's LENGTH is inside
// the frame and a case measuring the ladder under a shorter one would be measuring a different
// ladder.
func protectLength(t *testing.T, pair *testPair, plaintext int) (int, error) {
	t.Helper()
	// the aad's VALUE cannot change any length -- MASTER section 8.4.4 measured that at twelve
	// lengths against three different 32 octet values -- so the generation and the head commit
	// here are whatever is cheapest. Its WIDTH is what matters and is 32 at v1 and v2 alike.
	aad, err := aadMls(message.BodyBinding{RetentionClass: message.RetentionDurable}, 0, [32]byte{})
	if err != nil {
		return 0, err
	}
	framed, err := pair.chain.founder.Protect(aad[:], make([]byte, plaintext))
	if err != nil {
		return 0, err
	}
	return len(framed), nil
}

// LP(x) and u64, written out here for the reason every label in a KAT is: a prefix width read off
// the package would move both halves of the comparison together.
func messageIdReferenceLP(x []byte) []byte {
	return append([]byte{byte(len(x) >> 24), byte(len(x) >> 16), byte(len(x) >> 8), byte(len(x))}, x...)
}

func messageIdReferenceU64(v uint64) []byte {
	out := make([]byte, 8)
	for i := range out {
		out[i] = byte(v >> (56 - 8*i))
	}
	return out
}

// ---------------------------------------------------------------------------
// the two denial channels a record's ACCEPTANCE and its REFUSAL used to open,
// and the one that is filed rather than closed
// ---------------------------------------------------------------------------

// censorshipKeySource lets a member choose the LEAF and the GENERATION an inner frame's sender data
// names, which is the whole of what the attack below needs.
//
// EVERY INPUT IT USES IS GROUP SHARED and that is the finding rather than the fixture. sender data
// is sealed under the epoch's sender_data_secret, which comes off the GroupHandle and which every
// member of the group holds, so the leaf and the generation inside it are values ANY member writes.
// The content is sealed under a key this source invents, so the frame will never open -- which is
// the point: the refusal has to land AFTER mls has been asked for the victim's key at the forged
// generation, because that ask is what used to move the victim's ratchet.
type censorshipKeySource struct {
	crypto     mls.CryptoProvider
	generation uint32
}

func (self *censorshipKeySource) NextMessageKey(mls.ContentType, mls.LeafIndex) ([]byte, []byte, uint32, error) {
	return bytes.Repeat([]byte{0x5a}, self.crypto.KeySize()),
		bytes.Repeat([]byte{0x5a}, self.crypto.NonceSize()), self.generation, nil
}

func (self *censorshipKeySource) MessageKey(mls.ContentType, mls.LeafIndex, uint32) ([]byte, []byte, error) {
	return nil, nil, errors.New("messagegroup: the forging source is a sender side source only")
}

func (self *censorshipKeySource) CommitMessageKey(mls.ContentType, mls.LeafIndex, uint32) error {
	return errors.New("messagegroup: the forging source is a sender side source only")
}

func (self *censorshipKeySource) EraseMessageKey(mls.ContentType, mls.LeafIndex, uint32) {}

// forgeFrameAtGeneration builds an inner MLS frame whose sender data names leaf and generation of
// the caller's choosing and whose cleartext authenticated_data is aad.
func forgeFrameAtGeneration(t *testing.T, handle GroupHandle, leaf uint32, generation uint32, aad []byte) []byte {
	t.Helper()
	contextBytes, err := handle.GroupContextBytes()
	if err != nil {
		t.Fatalf("GroupContextBytes: %v", err)
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		t.Fatalf("unmarshal the group context: %v", err)
	}
	crypto, err := mls.NewCryptoProvider(context.CipherSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	senderDataSecret, err := handle.SenderDataSecret()
	if err != nil {
		t.Fatalf("SenderDataSecret: %v", err)
	}
	authContent := &mls.AuthenticatedContent{
		WireFormat: mls.WireFormatPrivateMessage,
		Content: mls.FramedContent{
			GroupId:           context.GroupId,
			Epoch:             context.Epoch,
			Sender:            mls.Sender{SenderType: mls.SenderTypeMember, LeafIndex: mls.LeafIndex(leaf)},
			AuthenticatedData: append([]byte(nil), aad...),
			ContentType:       mls.ContentTypeApplication,
			ApplicationData:   []byte("octets that will never open"),
		},
		Auth: mls.FramedContentAuthData{Signature: bytes.Repeat([]byte{0x11}, 64)},
	}
	private, err := mls.SealPrivateMessage(crypto, &censorshipKeySource{crypto: crypto, generation: generation},
		senderDataSecret, authContent, 0)
	if err != nil {
		t.Fatalf("SealPrivateMessage: %v", err)
	}
	frame, err := mls.MarshalMLSMessage(&mls.MLSMessage{
		Version: mls.ProtocolVersionMls10, WireFormat: mls.WireFormatPrivateMessage,
		PrivateMessage: private,
	})
	if err != nil {
		t.Fatalf("MarshalMLSMessage: %v", err)
	}
	return frame
}

// TestAForgedGenerationInTheFrameHeaderCostsTheTrueSenderNothing is the CRITICAL channel of the
// third pass over MASTER section 8.4, driven end to end through OpenRecord.
//
// WHY THE PRE-RATCHET PEEK CANNOT STOP IT, which is the sentence that makes this case necessary
// rather than a second reading of the two refusals next door. R1 is a function of the frame's
// sender leaf and R2 is a function of its cleartext authenticated_data, and the FORGER writes both.
// A member that sets the sender data to the victim's leaf, puts the record at the victim's
// sender_handle, and sets the frame's aad to the aad of the position it is putting the record at
// passes both refusals BY CONSTRUCTION. There is a third field in that sender data -- the
// GENERATION -- that nothing authenticates and nothing above mls can check, and mls reaches it
// before the content AEAD and before the signature.
//
// WHAT IT USED TO BUY. The victim's receiving ratchet stepped to the forged generation, retained
// the run it passed and pruned it; the content AEAD then failed and the record was refused with
// "mls: message does not decrypt". Nothing was ERASED and the keys were EVICTED, which is the same
// outcome for the victim: every generation below a head is consumed, so one forged header cost the
// victim its next message, two cost it 1,025 of them, and the refusal is taken inside
// unframeBodyOnLoop -- before this package's own Commit -- so the same record could be sent again
// at the SAME stream index forever. Measured before the repair: three forged records at one index
// cost the true sender four of its next four messages, and it never had to have written anything.
//
// THE VACUITY GUARD IS THE REFUSAL'S OWN SENTINEL. If the forged record were refused at R1 or R2
// then mls never saw the generation, this case would be driving the channel that was already
// closed, and it would pass on a build with the defect. So the refusal is required to be
// ErrRecordInnerFrame -- mls refusing the frame itself, which is refused BELOW the peek.
func TestAForgedGenerationInTheFrameHeaderCostsTheTrueSenderNothing(t *testing.T) {
	pair := newTestPair(t, "forged-generation")
	pair.trackDurable(t)

	// the true sender writes, and nobody has opened it yet -- a message in flight, the ordinary
	// case and the one with something to lose.
	plaintext := []byte("the message being censored")
	genuine, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), plaintext, 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	at := genuine.Header.StreamIndex + 1

	// the aad is built AT the forged generation and under the attacker's own head, which is what
	// v2 obliges the attacker to do: an aad naming any other generation or any other head is
	// refused at R2 by the PEEK, and this case needs the refusal to land at mls instead -- see the
	// vacuity guard below.
	attackerHead := []byte("a head the attacker chose")
	aad := forgeAadAt(t, pair.opener, pair.senderLeaf, at, mls.MaxGenerationSkip, attackerHead)
	frame := forgeFrameAtGeneration(t, pair.opener.handle, pair.senderLeaf, mls.MaxGenerationSkip, aad[:])
	forged := repairForgeRecord(t, pair.opener, pair.senderLeaf, at, attackerHead, frame)

	if _, _, err := pair.opener.OpenRecord(forged); err == nil {
		t.Fatal("the forged record OPENED, which is a different and worse finding")
	} else if !errors.Is(err, ErrRecordInnerFrame) {
		t.Fatalf("the forged record was refused with %v, want ErrRecordInnerFrame: a refusal at R1 or R2 means mls never saw the forged generation and this case is not driving the channel it names",
			err)
	}

	// THE STAKE. The true sender's own message, written before any of this.
	_, body, err := pair.opener.OpenRecord(genuine)
	if err != nil {
		t.Fatalf("the true sender's genuine message is unopenable after ONE forged header: %v. No input an unauthenticated party controls may advance or prune another member's receiving ratchet",
			err)
	}
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("the genuine message opened to %q, want %q", body, plaintext)
	}
}

// TestForgedGenerationsAreRepeatableAtOneStreamIndexAndCostNothing is the amplified form, and it is
// the one that says the repair holds against a member that keeps going rather than against one
// header.
//
// The refusal is taken before this package's own Commit, so the attacker's stream index is never
// spent and the same record shape can be re-sent at index 0 forever. On the build this closes each
// round advanced the victim's MLS receiving head by another MaxGenerationSkip; here each round has
// to cost nothing, and the victim then writes for the FIRST time -- so the case is pre-emptive,
// which the lift-and-re-envelope channel could not be.
func TestForgedGenerationsAreRepeatableAtOneStreamIndexAndCostNothing(t *testing.T) {
	pair := newTestPair(t, "forged-generation-rounds")
	pair.trackDurable(t)

	// the attacker moves FIRST. The victim has written nothing at all.
	const at = uint64(0)
	attackerHead := []byte("a head the attacker chose")
	const rounds = 3
	for round := 1; round <= rounds; round += 1 {
		// each round's aad names that round's own forged generation, which is what v2 makes the
		// attacker do to get past the peek at all.
		generation := uint32(round) * mls.MaxGenerationSkip
		aad := forgeAadAt(t, pair.opener, pair.senderLeaf, at, generation, attackerHead)
		frame := forgeFrameAtGeneration(t, pair.opener.handle, pair.senderLeaf, generation, aad[:])
		forged := repairForgeRecord(t, pair.opener, pair.senderLeaf, at, attackerHead, frame)
		if _, _, err := pair.opener.OpenRecord(forged); err == nil {
			t.Fatalf("round %d: the forged record OPENED", round)
		} else if !errors.Is(err, ErrRecordInnerFrame) {
			t.Fatalf("round %d was refused with %v, want ErrRecordInnerFrame", round, err)
		}
	}

	// and NOW the victim writes, for the first time.
	for wrote := 0; wrote < 4; wrote += 1 {
		plaintext := fmt.Appendf(nil, "a message written after %d forged records", rounds)
		record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), plaintext, 0, nil)
		if err != nil {
			t.Fatalf("SealRecord %d: %v", wrote, err)
		}
		_, body, err := pair.opener.OpenRecord(record)
		if err != nil {
			t.Fatalf("message %d of the victim's is unopenable after %d forged records at one stream index: %v",
				wrote, rounds, err)
		}
		if !bytes.Equal(body, plaintext) {
			t.Fatalf("message %d opened to %q, want %q", wrote, body, plaintext)
		}
	}
}

// ---------------------------------------------------------------------------
// the generation bind: MASTER section 8.4.2 v2 term (3), driven end to end
// ---------------------------------------------------------------------------

// reEnvelopeKeySource seals the CONTENT of a re-enveloped frame under the victim leaf's REAL
// message key at a generation of the attacker's choosing.
//
// EVERY INPUT IT USES IS GROUP SHARED and that is the finding rather than the fixture. RFC 9420
// section 9 derives every leaf's ratchet from the epoch's encryption_secret, which the GroupHandle
// hands over because MASTER section 8.2 requires it for archive_secret -- so a member holds any
// other member's message key at any generation it likes. Combined with sender_data_secret, also
// group shared, a member can therefore re-seal another member's frame at any generation.
//
// It is a SENDER side source only: the framing layer calls NextMessageKey once per seal, and the
// three other methods are refusals so a misuse is loud.
type reEnvelopeKeySource struct {
	tree       *mls.SecretTree
	generation uint32
}

func (self *reEnvelopeKeySource) NextMessageKey(contentType mls.ContentType,
	leaf mls.LeafIndex) ([]byte, []byte, uint32, error) {

	key, nonce, err := self.tree.MessageKey(contentType, leaf, self.generation)
	if err != nil {
		return nil, nil, 0, err
	}
	return key, nonce, self.generation, nil
}

func (self *reEnvelopeKeySource) MessageKey(mls.ContentType, mls.LeafIndex, uint32) ([]byte, []byte, error) {
	return nil, nil, errors.New("messagegroup: the re-envelope source is a sender side source only")
}

func (self *reEnvelopeKeySource) CommitMessageKey(mls.ContentType, mls.LeafIndex, uint32) error {
	return errors.New("messagegroup: the re-envelope source is a sender side source only")
}

func (self *reEnvelopeKeySource) EraseMessageKey(mls.ContentType, mls.LeafIndex, uint32) {}

// reEnvelopeAtGeneration takes a genuine frame and re-seals it AT ANOTHER GENERATION, keeping the
// victim's FramedContent and the victim's SIGNATURE OCTETS byte for byte.
//
// This is the attack MASTER section 8.4.2's term (3) exists for, assembled out of what any member
// holds: open the frame with a secret tree built from the group's own encryption_secret, then seal
// the very same AuthenticatedContent under a source that names whatever generation the attacker
// wants. Nothing about the signature, the leaf, the aad or the position changes -- the ONE variable
// is the generation in the sender data.
func reEnvelopeAtGeneration(t *testing.T, handle GroupHandle, victimLeaf uint32,
	generation uint32, frame []byte) []byte {

	t.Helper()
	contextBytes, err := handle.GroupContextBytes()
	if err != nil {
		t.Fatalf("GroupContextBytes: %v", err)
	}
	context := &mls.GroupContext{}
	if err := syntax.Unmarshal(contextBytes, context); err != nil {
		t.Fatalf("unmarshal the group context: %v", err)
	}
	crypto, err := mls.NewCryptoProvider(context.CipherSuite)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	senderDataSecret, err := handle.SenderDataSecret()
	if err != nil {
		t.Fatalf("SenderDataSecret: %v", err)
	}
	encryptionSecret, err := handle.EncryptionSecret()
	if err != nil {
		t.Fatalf("EncryptionSecret: %v", err)
	}
	snapshot, err := handle.RatchetTreeSnapshot()
	if err != nil {
		t.Fatalf("RatchetTreeSnapshot: %v", err)
	}
	tree, err := mls.UnmarshalRatchetTree(snapshot)
	if err != nil {
		t.Fatalf("UnmarshalRatchetTree: %v", err)
	}
	victim := tree.Leaf(mls.LeafIndex(victimLeaf))
	if victim == nil {
		t.Fatalf("leaf %d is blank in the ratchet tree", victimLeaf)
	}
	// two trees out of one secret: the first opens the genuine frame, which COMMITS the genuine
	// generation on that copy, and the second is what the re-seal draws from. Two copies because
	// an attacker running this twice would build two; nothing about the victim's own tree moves.
	opening, err := mls.NewSecretTree(crypto, tree.LeafCount(), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	parsed, err := mls.ParseMLSMessage(frame)
	if err != nil {
		t.Fatalf("ParseMLSMessage: %v", err)
	}
	authContent, err := mls.OpenPrivateMessage(crypto, opening, senderDataSecret,
		parsed.PrivateMessage, mls.StaticSignatureKey(victim.SignatureKey), contextBytes)
	if err != nil {
		t.Fatalf("a member could not open the frame it is re-enveloping: %v", err)
	}
	sealing, err := mls.NewSecretTree(crypto, tree.LeafCount(), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	private, err := mls.SealPrivateMessage(crypto,
		&reEnvelopeKeySource{tree: sealing, generation: generation},
		senderDataSecret, authContent, 0)
	if err != nil {
		t.Fatalf("re-seal at generation %d: %v", generation, err)
	}
	reEnveloped, err := mls.MarshalMLSMessage(&mls.MLSMessage{
		Version: mls.ProtocolVersionMls10, WireFormat: mls.WireFormatPrivateMessage,
		PrivateMessage: private,
	})
	if err != nil {
		t.Fatalf("MarshalMLSMessage: %v", err)
	}
	return reEnveloped
}

// TestAReEnvelopedGenerationIsRefusedAndTheVictimsRecordsSurvive is MASTER section 8.4.2 v2's
// CRITICAL channel, driven end to end through GroupSession.OpenRecord.
//
// THE ATTACK, and it is not the one the two forged-generation cases above drive. Those two build a
// frame whose CONTENT never opens, so mls refuses them at the AEAD and what they measure is that
// the refusal costs the victim nothing. This one keeps the victim's FramedContent and the victim's
// SIGNATURE OCTETS byte for byte and changes only the generation in the sender data -- so under v1
// the record passed R1 (the frame really is the victim's), passed R2 (the position really is that
// record's), OPENED, and committed the attacker's generation at the receiver. Every one of the
// victim's own frames at or below that generation was then refused for ever, because every
// generation below a receiving head is consumed.
//
// MEASURED ON THIS TREE, with the query being this case: with term (3) removed from the preimage on
// both sides, 3 of 3 substitutes were ACCEPTED and 7 of 7 of the victim's genuine records were then
// dead. With it, 0 of 3 are accepted and 0 of 7 are dead. Three substitutes kill seven records
// because a commit at generation g consumes every generation below g.
//
// THE REFUSAL MUST BE R2 AND NOT SOMETHING EARLIER, and that is the vacuity guard: if the substitute
// were refused by mls instead, this case would be driving the channel the two cases above already
// close and would pass on a build with the defect.
func TestAReEnvelopedGenerationIsRefusedAndTheVictimsRecordsSurvive(t *testing.T) {
	pair := newTestPair(t, "re-enveloped-generation")
	pair.trackDurable(t)

	// the victim writes seven records and NONE of them has been delivered yet -- messages in
	// flight, which is the ordinary case and the one with something to lose.
	const wrote = 7
	heads := make([][]byte, 0, wrote)
	bodies := make([][]byte, 0, wrote)
	genuine := make([]*message.Record, 0, wrote)
	for i := 0; i < wrote; i += 1 {
		head := fmt.Appendf(nil, "head %d", i)
		body := fmt.Appendf(nil, "the message being censored, number %d", i)
		record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, head, body, 0, nil)
		if err != nil {
			t.Fatalf("SealRecord %d: %v", i, err)
		}
		heads = append(heads, head)
		bodies = append(bodies, body)
		genuine = append(genuine, record)
	}

	// THE SUBSTITUTES. Each takes one of the victim's own frames and re-seals it at a LATER
	// generation, at the victim's own handle, at that record's own stream index, under that
	// record's own head. Nothing but the generation differs, so R1 and R2's other three terms
	// pass by construction.
	substitutes := []uint32{3, 5, 7}
	accepted := 0
	for at, generation := range substitutes {
		frame := repairLiftFrame(t, pair.opener, pair.senderLeaf, genuine[at])
		reEnveloped := reEnvelopeAtGeneration(t, pair.opener.handle, pair.senderLeaf, generation, frame)
		substitute := repairForgeRecord(t, pair.opener, pair.senderLeaf,
			genuine[at].Header.StreamIndex, heads[at], reEnveloped)
		_, _, err := pair.opener.OpenRecord(substitute)
		if err == nil {
			accepted += 1
			continue
		}
		if !errors.Is(err, ErrRecordPositionBinding) {
			t.Errorf("the substitute at generation %d was refused with %v, want ErrRecordPositionBinding: a refusal anywhere else means this case is not driving the channel it names",
				generation, err)
		}
	}
	// an ERROR and not a fatal, so a build with the defect reports BOTH halves of it: the
	// substitutes that were accepted and the victim's records they killed. A case that stopped
	// here would make its own repair a guess about what the acceptance cost.
	if accepted != 0 {
		t.Errorf("%d of %d re-enveloped substitutes were ACCEPTED; MASTER section 8.4.2 term (3) binds the generation and this is the channel it closes",
			accepted, len(substitutes))
	}

	// THE STAKE. Every one of the victim's seven genuine records, written before any of this.
	dead := 0
	for i, record := range genuine {
		gotHead, gotBody, err := pair.opener.OpenRecord(record)
		if err != nil {
			dead += 1
			t.Errorf("the victim's record %d is unopenable after %d refused substitutes: %v",
				i, len(substitutes), err)
			continue
		}
		if !bytes.Equal(gotHead, heads[i]) || !bytes.Equal(gotBody, bodies[i]) {
			t.Errorf("the victim's record %d opened to %q/%q", i, gotHead, gotBody)
		}
	}
	if dead != 0 {
		t.Errorf("%d of %d of the victim's genuine records are dead", dead, wrote)
	}
	t.Logf("%d of %d substitutes accepted, %d of %d of the victim's genuine records dead",
		accepted, len(substitutes), dead, wrote)
}

// TestTheCeremonyDoorSpendsNoneOfTheHandleItNames is MG-5's DENIAL half, which that item filed as
// an attribution hole and measured only as attributed octets.
//
// WHAT THE OTHER HALF IS. openRecordOnLoop ran receivers.Commit for BOTH doors, and the ceremony arm
// takes no frame check at all -- unframeBodyOnLoop returns a ceremony body unchanged. Every key the
// two record AEADs use is group shared, so a member can seal a ceremony record at any other member's
// sender_handle at any index. Accepted, it committed the victim's ladder past the rung the victim's
// own next record needed: one squatted record per message, no lift, no genuine frame, no race, and
// the victim's record at that index answering ErrOutOfWindow forever.
//
// THE CONTROL IS THE ACCEPTANCE ITSELF. The ceremony record still opens, and to the attacker's own
// octets -- that is the attribution residual MG-5 files and this case does not repair it. What it
// requires is that the acceptance spends nothing of the handle it named.
func TestTheCeremonyDoorSpendsNoneOfTheHandleItNames(t *testing.T) {
	pair := newTestPair(t, "ceremony-ladder")
	pair.trackDurable(t)

	// the victim writes first so the index it is about to need is read off its own record rather
	// than assumed; the record is held back and opened at the end, which is what makes the squat
	// below a squat rather than a race.
	plaintext := []byte("the victim's own first message")
	genuine, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), plaintext, 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	at := genuine.Header.StreamIndex

	// that index, squatted by a ceremony record the attacker builds. is_commit puts it on the arm
	// OpenRecord refuses and OpenCeremonyRecord serves.
	chosen := []byte("octets the attacker chose")
	squat := repairForgeRecordArm(t, pair.opener, pair.senderLeaf, at, true, nil,
		[]byte("a head the attacker chose"), chosen)
	_, body, err := pair.opener.OpenCeremonyRecord(squat)
	if err != nil {
		t.Fatalf("the ceremony door refused the squatted record: %v; this case needs the ACCEPTANCE, because it is the acceptance that used to walk the ladder",
			err)
	}
	if !bytes.Equal(body, chosen) {
		t.Fatalf("the ceremony door answered %q, want %q", body, chosen)
	}

	// THE STAKE. The victim's own record at that same index.
	_, got, err := pair.opener.OpenRecord(genuine)
	if err != nil {
		t.Fatalf("the victim's own record at stream index %d no longer opens after a ceremony record was ACCEPTED there: %v. A door that authenticates nothing may not spend anything either",
			at, err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("the victim's record opened to %q, want %q", got, plaintext)
	}
}

// TestTheApplicationDoorStillSpendsTheRungItOpens is the other side of the case above, and without
// it that one is satisfied by a build whose ladder never moves at all.
//
// A ladder that committed nothing would have no replay guard and no ordering: the same record would
// open forever. So the arm that IS checked is required to spend its rung, which is what makes "the
// ceremony arm spends nothing" a statement about the arm rather than about the ladder.
func TestTheApplicationDoorStillSpendsTheRungItOpens(t *testing.T) {
	pair := newTestPair(t, "application-ladder")
	pair.trackDurable(t)

	record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("a message that spends its rung"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	if _, _, err := pair.opener.OpenRecord(record); err != nil {
		t.Fatalf("OpenRecord: %v", err)
	}
	if _, _, err := pair.opener.OpenRecord(record); !errors.Is(err, ErrOutOfWindow) {
		t.Fatalf("a replay of an accepted application record answered %v, want ErrOutOfWindow: the application arm's acceptance must spend the rung it opened",
			err)
	}
}

// TestASubstitutedHeadIsRefusedAndTheGenuineRecordStillOpens is MG-6 CLOSED, driven end to end
// through OpenRecord, and it is the inversion of the case that stood here.
//
// WHAT STOOD HERE UNTIL 2026-09-17, because a case that is inverted without saying what it used to
// measure leaves the next reader unable to reconstruct the finding. It was
// TestTheHeadPlaintextIsNotBoundByTheFrame, a FILED RESIDUAL written as a case: it required the
// substitute to OPEN, logged the attacker's head and the true sender's body as evidence, and said
// in its own header "this case goes red if somebody binds it, and that is the intended way for it
// to end". MASTER section 8.4.2 v2's fourth term is the bind, and this is the person holding the
// failure closing MG-6.
//
// THE ATTACK, unchanged. ct_head is sealed under the same record_key EVERY member derives, so a
// member can lift another member's GENUINE body -- frame, signature and all -- and re-issue it at
// the SAME position under a head of its own. R1 passed because the frame really is that member's,
// R2 passed because the position really is that record's, and the record opened to the true sender's
// plaintext under an attacker's head. The substitute also landed at the true sender's own stream
// index, so ACCEPTING it walked the ladder past the genuine record and that record then never opened
// again. Measured on this tree before the bind: the substitute opened to head "A HEAD THE ATTACKER
// CHOSE" with the true sender's body, and the genuine record then answered ErrOutOfWindow.
//
// WHY IT IS REFUSED NOW. head_commit is HMAC-SHA-256 under a key expanded from this record's own
// rung, over the head plaintext ct_head actually opened to, and it is the fourth term of the aad_mls
// the sender signed. The substitute's head produces a different commitment, so the digest the opener
// rebuilds is not the one in the frame, and R2 refuses -- by name, and BEFORE any ratchet moves,
// which is the second half of what this case asserts.
//
// THE SINGLE VARIABLE IS THE HEAD. The frame, the signature, the leaf, the handle, the stream index
// and the generation are all the genuine record's; only the head plaintext differs. So a refusal
// here cannot be a refusal of the position or of the sender.
func TestASubstitutedHeadIsRefusedAndTheGenuineRecordStillOpens(t *testing.T) {
	pair := newTestPair(t, "head-bound")
	pair.trackDurable(t)

	plaintext := []byte("the body the sender really wrote")
	genuineHead := []byte("the head the sender wrote")
	genuine, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
		genuineHead, plaintext, 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	// the attacker lifts the genuine frame out of the genuine record. The record key is
	// RecordKeyZero(class_key, leaf) walked to this index, and the class key is group shared.
	frame := repairLiftFrame(t, pair.opener, pair.senderLeaf, genuine)

	// THE CONTROL FIRST: the same lift, re-issued under the SAME head at the SAME index, opens.
	// Without it a refusal below could be a refusal of the lift rather than of the head, and the
	// whole case would be about a fixture. It is taken on a separate opener so that accepting it
	// does not spend the rung the substitute needs.
	control := newTestPair(t, "head-bound-control")
	control.trackDurable(t)
	controlGenuine, err := control.sender.SealRecord(message.RetentionDurable, 0, false,
		genuineHead, plaintext, 0, nil)
	if err != nil {
		t.Fatalf("the control's SealRecord: %v", err)
	}
	controlFrame := repairLiftFrame(t, control.opener, control.senderLeaf, controlGenuine)
	reissued := repairForgeRecord(t, control.opener, control.senderLeaf,
		controlGenuine.Header.StreamIndex, genuineHead, controlFrame)
	gotHead, gotBody, err := control.opener.OpenRecord(reissued)
	if err != nil {
		t.Fatalf("a lifted frame re-issued under its OWN head at its OWN index was refused with %v, so nothing below is about the head",
			err)
	}
	if !bytes.Equal(gotHead, genuineHead) || !bytes.Equal(gotBody, plaintext) {
		t.Fatalf("the control opened to head %q body %q", gotHead, gotBody)
	}

	// THE SUBSTITUTE: one variable moved.
	substituted := []byte("A HEAD THE ATTACKER CHOSE")
	substitute := repairForgeRecord(t, pair.opener, pair.senderLeaf,
		genuine.Header.StreamIndex, substituted, frame)
	head, body, refusal := pair.opener.OpenRecord(substitute)
	if !errors.Is(refusal, ErrRecordPositionBinding) {
		t.Fatalf("a genuine frame re-issued at its own position under another member's head opened with %v; want ErrRecordPositionBinding. MG-6 is the case where it opened, and head_commit is what closes it",
			refusal)
	}
	if head != nil || body != nil {
		t.Errorf("the refusal returned %d octets of head and %d of body beside the error",
			len(head), len(body))
	}

	// AND THE GENUINE RECORD STILL OPENS, which is the half that says the refusal spent nothing.
	// Under the old build the substitute was ACCEPTED here, so the rung was gone and this call
	// answered ErrOutOfWindow.
	gotHead, gotBody, err = pair.opener.OpenRecord(genuine)
	if err != nil {
		t.Fatalf("the true sender's own record no longer opens after the substitute was refused at its index: %v. A refused record must move no receiver ratchet",
			err)
	}
	if !bytes.Equal(gotHead, genuineHead) || !bytes.Equal(gotBody, plaintext) {
		t.Fatalf("the genuine record opened to head %q body %q, want %q / %q",
			gotHead, gotBody, genuineHead, plaintext)
	}
	t.Logf("MG-6 CLOSED: the substitute was refused with %v and the genuine record at the same index still opens", refusal)
}

// TestTheDerivedFramedLengthIsTheLengthTheSealEmits is what makes MASTER section 8.4.6's early
// refusal honest rather than approximately right.
//
// THE RULE it stands under: an application record's early size refusal is taken over
// framed_length(len(bodyPlain)), BEFORE the stream index is reserved and BEFORE the generation is
// spent. That is only a rule if framed_length is the length the seal actually produces. One octet
// short and the check admits a body the seal must then refuse late, after spending both; one octet
// long and it refuses a legal body at the boundary.
//
// THE LENGTHS ARE THE STEP FUNCTION'S OWN BOUNDARIES AND NOT A SAMPLE. MASTER section 8.4.4 records
// that the overhead has FOUR steps and that this corpus published three of them for two days,
// because the ladder was measured by walking and the step function was derived by hand. So both
// sides of each boundary are driven, including 16,300 -- the one the three step form omits, where
// varint(C) widens and varint(P) has not.
//
// THE QUERY: mls.FramedApplicationLength over the pair's own suite and group id width against
// len(Protect(aad, make([]byte, P))) on a real two member group, at each length below. It is the
// same query MASTER section 8.4.4 publishes, run here rather than transcribed.
func TestTheDerivedFramedLengthIsTheLengthTheSealEmits(t *testing.T) {
	pair := newTestPair(t, "derived-framed-length")
	overheads := map[int]int{}
	for _, plaintext := range []int{
		0, 1, 63, 64, 65,
		applicationBodyCapacity[0], applicationBodyCapacity[1], applicationBodyCapacity[2],
		16186, 16299, 16300, 16350, 16383, 16384,
		applicationBodyCapacity[4],
	} {
		derived, err := framedApplicationLength(pair.chain.founder, plaintext)
		if err != nil {
			t.Fatalf("framedApplicationLength(%d): %v", plaintext, err)
		}
		sealed, err := protectLength(t, pair, plaintext)
		if err != nil {
			t.Fatalf("Protect(%d): %v", plaintext, err)
		}
		if derived != sealed {
			t.Errorf("the derived framed length of a %d octet body is %d and the seal emitted %d; MASTER section 8.4.6's early refusal is arithmetic over the derived number and a disagreement is a body admitted or refused at the wrong boundary",
				plaintext, derived, sealed)
		}
		overheads[plaintext] = sealed - plaintext
	}
	// AND THE FOUR STEPS ARE THE ANSWER MASTER SECTION 8.4.4 PUBLISHES, which is the second half:
	// agreement between two wrong numbers is still agreement. These four are this document's
	// expected answer at ciphersuite 0x0003 with a 32 octet group id, and nothing in this
	// package's production source carries them.
	for plaintext, want := range map[int]int{
		0: 193, 63: 193, 64: 194, 16299: 194, 16300: 196, 16383: 196, 16384: 198,
	} {
		if got := overheads[plaintext]; got != want {
			t.Errorf("the frame's overhead at a %d octet plaintext is %d and MASTER section 8.4.4's corrected step function gives %d",
				plaintext, got, want)
		}
	}
	t.Logf("overheads by plaintext length: %v", overheads)
}
