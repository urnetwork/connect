// The Commit wire codec.
//
// The presence octet is the whole of what is interesting here. RFC 9420's optional<T> has exactly
// two encodings, and a third would be a second encoding of one object -- which is the
// signature-bypass primitive, since a signature covers one serialization and a receiver that
// accepted two readings of the same bytes has two objects claiming one signature.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

func TestCommitRoundTripWithoutPath(t *testing.T) {
	commit := Commit{
		Proposals: []ProposalOrRef{
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0x01, 0x02}},
		},
	}
	encoded, err := syntax.Marshal(&commit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if encoded[len(encoded)-1] != 0x00 {
		t.Fatalf("absent path encoded as %02x, want 00", encoded[len(encoded)-1])
	}

	decoded := Commit{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Path != nil {
		t.Fatal("path decoded as present")
	}
	if len(decoded.Proposals) != 1 || decoded.Proposals[0].Type != ProposalOrRefTypeReference {
		t.Fatalf("proposals %+v", decoded.Proposals)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

func TestCommitRoundTripEmptyProposals(t *testing.T) {
	commit := Commit{}
	encoded, err := syntax.Marshal(&commit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := Commit{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Proposals) != 0 || decoded.Path != nil {
		t.Fatalf("decoded %+v", decoded)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// The present arm, which the two tests above never reach: an absent path and a present one
// differ by one octet at the front of the value, and a codec that wrote the presence octet and
// then forgot the path round trips the absent case perfectly.
func TestCommitRoundTripWithAPath(t *testing.T) {
	commit := Commit{
		Proposals: []ProposalOrRef{
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0x01, 0x02}},
		},
		Path: &UpdatePath{LeafNode: *testLeafNodeOfSource(LeafNodeSourceCommit)},
	}
	encoded, err := syntax.Marshal(&commit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	withoutPath := Commit{Proposals: commit.Proposals}
	absent, err := syntax.Marshal(&withoutPath)
	if err != nil {
		t.Fatalf("marshal without the path: %v", err)
	}
	if len(encoded) <= len(absent) {
		t.Fatalf("a commit carrying a path encoded to %d octets and one carrying none to %d", len(encoded), len(absent))
	}
	if encoded[len(absent)-1] != 0x01 {
		t.Fatalf("present path encoded its presence octet as %02x, want 01", encoded[len(absent)-1])
	}

	decoded := Commit{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Path == nil {
		t.Fatal("a commit carrying a path decoded with none")
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

func TestCommitRejectsInvalidOptionalPresenceByte(t *testing.T) {
	commit := Commit{}
	valid, err := syntax.Marshal(&commit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded := append([]byte(nil), valid...)
	encoded[len(encoded)-1] = 0x02

	decoded := Commit{}
	// the sentinel is the syntax package's: optional<T> presence is the codec's concern and
	// not framing's, so there is one place in the system that decides what optional<T> means
	if err := syntax.Unmarshal(encoded, &decoded); !errors.Is(err, syntax.ErrOptionalPresence) {
		t.Fatalf("got %v, want syntax.ErrOptionalPresence", err)
	}
}

// TestCommitReturnsThePresenceRefusalRatherThanLeaningOnTheStickyReader is the half of the
// presence refusal that going through syntax.Unmarshal cannot observe.
//
// Measured, not supposed: with the presence error swallowed -- `if err != nil { present = false }`
// where the codec returns it -- TestCommitRejectsInvalidOptionalPresenceByte above still passes,
// because ReadOptional LATCHES the failure on the Reader and syntax.Unmarshal's own r.Done()
// reports it afterwards. So that test states that the refusal reaches a caller who went through
// Unmarshal, and states nothing about the codec it is nominally about.
//
// This one calls UnmarshalMLS directly, which is how every enclosing codec in this package
// reaches it -- a Commit inside a FramedContent inside an AuthenticatedContent is decoded by
// method call and not by syntax.Unmarshal -- and there is no Done() at the end of those. A codec
// that swallowed the presence refusal there would hand its caller a commit with no path, out of
// bytes that said something else, with a nil error.
func TestCommitReturnsThePresenceRefusalRatherThanLeaningOnTheStickyReader(t *testing.T) {
	commit := Commit{}
	valid, err := syntax.Marshal(&commit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded := append([]byte(nil), valid...)
	encoded[len(encoded)-1] = 0x02

	decoded := Commit{}
	if err := decoded.UnmarshalMLS(syntax.NewReader(encoded)); !errors.Is(err, syntax.ErrOptionalPresence) {
		t.Fatalf("the codec answered %v to a presence octet of 0x02; every enclosing codec in this package reaches it by method call and never runs a Done() that could report the latched failure instead",
			err)
	}
}

// ---------------------------------------------------------------------------
// what a refused decode leaves behind
// ---------------------------------------------------------------------------

// testCommit is the object the sweep below is stated over, in both of its arms.
//
// Both, because which truncations can see an early publish depends on WHICH arm the encoding
// carries: a commit with a path has an UpdatePath's worth of octets after the presence octet to
// be cut inside, and one without has nothing after it at all, so a single arm chosen here would
// be a guess at where the property is observable.
func testCommit(withPath bool) Commit {
	commit := Commit{
		Proposals: []ProposalOrRef{
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0x01, 0x02}},
		},
	}
	if withPath {
		commit.Path = &UpdatePath{LeafNode: *testLeafNodeOfSource(LeafNodeSourceCommit)}
	}
	return commit
}

// updatePathOctetsAfterPresence is how far back from the end of an encoding the presence octet
// sits.
//
// An absent path is the last octet; a present one is followed by the whole UpdatePath, so the
// offset is measured off the encoder rather than counted here. A hand written offset would
// rewrite a byte of the leaf node instead, and the sweep would be asserting about a mutation it
// did not mean to make.
func updatePathOctetsAfterPresence(t *testing.T, withPath bool) int {
	t.Helper()
	if !withPath {
		return 0
	}
	present := testCommit(true)
	withOne, err := syntax.Marshal(&present)
	if err != nil {
		t.Fatalf("encode a commit carrying a path: %v", err)
	}
	absent := testCommit(false)
	withNone, err := syntax.Marshal(&absent)
	if err != nil {
		t.Fatalf("encode a commit carrying no path: %v", err)
	}
	if len(withOne) <= len(withNone) {
		t.Fatalf("a commit carrying a path encoded to %d octets and one carrying none to %d",
			len(withOne), len(withNone))
	}
	return len(withOne) - len(withNone)
}

// TestARefusedCommitDecodeLeavesTheCallersValueAlone is the property Commit.UnmarshalMLS claims
// when it decodes into locals and assigns the receiver whole at the end, and which nothing
// observed.
//
// Measured, not argued: inserting `self.Proposals = proposals` immediately before the optional
// path read -- the receiver published before the decode has succeeded -- left the whole of
// ./mls/... and ./message/... green, 6457 tests, while the identical edit in GroupInfo failed at
// once. The difference was that welcome_wire_test.go's sweep is a table of four types and Commit
// was not one of them. TestEveryDecoderInThisPackagePublishesItsReceiverWhereThisSaysItDoes is
// where that table stopped being a table; this is what the property is worth for this type.
//
// What an early publish costs here is not tidiness. A Commit is the message that ENDS AN EPOCH
// and its proposal list is what the group applies, so a caller that decoded a refused commit
// into a value it still holds is holding the sender's proposal list attached to whatever path
// was there before. Nothing in this package decodes into a caller-held Commit today --
// framing.go allocates a fresh one per FramedContent -- which makes this the codec's contract
// being held for the lifecycle that will, rather than a live defect being reproduced.
//
// The refusals are DERIVED rather than picked: every proper prefix of both arms' encodings, plus
// the two refusals that are not truncations at all -- a presence octet naming neither encoding
// of optional<T>, and a proposals vector whose declared region overruns the octets after it. A
// sweep of truncations alone would say nothing about a decoder that read every field and refused
// on the contents of the last.
func TestARefusedCommitDecodeLeavesTheCallersValueAlone(t *testing.T) {
	// the value the caller already holds, produced by this codec rather than assembled, and
	// unlike the decoded one in both fields so that a receiver clobbered in either is visible
	held := Commit{
		Proposals: []ProposalOrRef{
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0xb0, 0xb1, 0xb2}},
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0xb3}},
		},
	}
	heldBytes, err := syntax.Marshal(&held)
	if err != nil {
		t.Fatalf("encode the value the caller already holds: %v", err)
	}

	refusals := [][]byte{}
	for _, withPath := range []bool{false, true} {
		value := testCommit(withPath)
		encoded, err := syntax.Marshal(&value)
		if err != nil {
			t.Fatalf("encode the commit the truncations are cut from (path present: %v): %v",
				withPath, err)
		}
		if bytes.Equal(encoded, heldBytes) {
			t.Fatalf("the held value and the decoded one encode identically, so this sweep cannot tell an untouched receiver from a clobbered one")
		}
		for cut := 0; cut < len(encoded); cut++ {
			refusals = append(refusals, encoded[:cut])
		}
		// the presence octet rewritten to a value optional<T> has no encoding for. This is
		// the one refusal that arrives AFTER the proposals vector has been read and decoded,
		// which is exactly where an early publish would already have happened.
		presence := append([]byte(nil), encoded...)
		presence[len(presence)-1-updatePathOctetsAfterPresence(t, withPath)] = 0x02
		refusals = append(refusals, presence)
	}
	// and a refusal inside the vector rather than after it: a proposals region declaring more
	// octets than follow it, so ReadVector refuses on the region and never reaches an element
	refusals = append(refusals, []byte{0x20, 0x01, 0x02})
	if len(refusals) < 8 {
		t.Fatalf("the derivation produced %d refusable encodings, which is too few to state anything",
			len(refusals))
	}

	untouched := 0
	for _, encoded := range refusals {
		receiver := Commit{}
		if err := syntax.Unmarshal(heldBytes, &receiver); err != nil {
			t.Fatalf("the prior value did not decode into the receiver: %v", err)
		}
		// UnmarshalMLS directly rather than syntax.Unmarshal: Unmarshal's contract is about
		// the bytes and this is a statement about the receiver, and every enclosing codec in
		// this package reaches a Commit by method call with no Done() behind it
		if err := receiver.UnmarshalMLS(syntax.NewReader(encoded)); err == nil {
			t.Errorf("%x was accepted as a commit, so this row says nothing about what a refusal leaves behind",
				encoded)
			continue
		}
		after, err := syntax.Marshal(&receiver)
		if err != nil {
			t.Errorf("re-encode the receiver after the refused decode of %x: %v", encoded, err)
			continue
		}
		if !bytes.Equal(after, heldBytes) {
			t.Errorf("the refused decode of %x left the caller's value as\n  %x\nand it was\n  %x\na decoder that refuses has said these octets are not a Commit, and the sender's proposal list is now on a value somebody else built",
				encoded, after, heldBytes)
			continue
		}
		untouched++
	}
	if untouched != len(refusals) {
		t.Fatalf("%d of the %d refused decodes left the receiver alone", untouched, len(refusals))
	}
	t.Logf("%d refused Commit decodes left the caller's value untouched", untouched)
}

// ---------------------------------------------------------------------------
// the published corpus
// ---------------------------------------------------------------------------

// commitWireMessagesVector is the one field of an mlswg `messages` case this file reads. The
// other sixteen belong to other codecs.
type commitWireMessagesVector struct {
	Commit string `json:"commit"`
}

// commitWireMessagesCases is how many cases messages.json carries. Asserted rather than assumed,
// for the reason every count in this package is: a corpus that shrank, or a field that stopped
// being present, turns the sweep below into a loop that runs zero times and reports exactly what
// a complete sweep reports.
const commitWireMessagesCases = 300

// TestTheMessagesCorpusCommitsRoundTripByteExactly is the interop half this codec never had: 300
// published Commits, none of which this package produced, each decoded and re-encoded to the
// same octets.
//
// Every other assertion in this file is stated over a Commit this file built, which makes every
// one of them this codec agreeing with itself. Swap the two fields in both halves and all of
// them still pass; the field order is pinned against mlswg only TRANSITIVELY today, through the
// framing preimages three other files take over commits assembled here. Byte exactness against
// bytes this package did not write is what sees a field order the RFC and this file disagree
// about directly.
//
// The corpus is read here rather than through a registered vector family because families 8 and
// 12 are owned by other tasks and installing a runner would move a decision that belongs to them
// -- welcome_wire_test.go reads its own three fields out of the same file for that reason.
func TestTheMessagesCorpusCommitsRoundTripByteExactly(t *testing.T) {
	entries := LoadVectorFile(t, "messages.json")
	if len(entries) != commitWireMessagesCases {
		t.Fatalf("messages.json carries %d cases, want %d", len(entries), commitWireMessagesCases)
	}
	roundTripped, carryingAPath, proposals := 0, 0, 0
	for i, raw := range entries {
		vector := commitWireMessagesVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("case %d: parse: %v", i, err)
		}
		if vector.Commit == "" {
			t.Fatalf("case %d carries no commit field, so this case asserted nothing", i)
		}
		encoded := MustHex(t, vector.Commit)
		decoded := Commit{}
		// syntax.Unmarshal rather than UnmarshalMLS: this half is about the octets, and
		// Unmarshal joins the decoder's answer with Done, so a published commit this codec
		// under-consumed is refused rather than round tripping off a prefix of itself
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Errorf("messages.json case %d: the published commit was refused: %v", i, err)
			continue
		}
		reencoded, err := syntax.Marshal(&decoded)
		if err != nil {
			t.Errorf("messages.json case %d: re-encode: %v", i, err)
			continue
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("messages.json case %d: re-encoded to\n  %x\nand the published encoding is\n  %x",
				i, reencoded, encoded)
			continue
		}
		if decoded.Path != nil {
			carryingAPath++
		}
		proposals += len(decoded.Proposals)
		roundTripped++
	}
	if roundTripped != commitWireMessagesCases {
		t.Fatalf("round tripped %d published commits, want %d", roundTripped, commitWireMessagesCases)
	}
	// which arm the corpus reaches, stated rather than left to be assumed. Every published
	// commit carries a path, so the ABSENT arm -- the one an add-only commit takes -- is
	// reached by the hand written round trips above and by nothing else in the tree, and a
	// corpus that stopped carrying paths would quietly stop exercising the present arm here.
	if carryingAPath != commitWireMessagesCases {
		t.Errorf("%d of the %d published commits carry an update path, want all of them; the present arm is the one this file's hand written cases can least afford to be alone on",
			carryingAPath, commitWireMessagesCases)
	}
	if proposals == 0 {
		t.Errorf("the %d published commits carry no proposals between them, so the proposals vector round tripped empty every time",
			commitWireMessagesCases)
	}
	t.Logf("%d published commits round tripped byte exactly, %d carrying a path, %d proposals in total",
		roundTripped, carryingAPath, proposals)
}
