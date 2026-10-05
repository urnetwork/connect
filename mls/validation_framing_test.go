// The framing layer's ValSem refusals, and the roster that says each of them still has a test.
//
// RFC 9420 section 6 is authenticated by two things this package already holds -- a signature and
// a membership tag -- and neither of them says anything about WHICH conversation the message
// belongs to. A valid signature says some member of some group signed these bytes in some epoch.
// ValSem002, ValSem003 and ValSem004 are what turn that into a statement about this group, this
// epoch, and a leaf that is actually occupied, so they are the rules a receive path cannot verify
// its way past, and they live here beside the roster rather than beside the codecs.
//
// The roster is the other half, and it is the half that has been worth having. A refusal is one
// `if` and a `return`; a refusal whose test was deleted, renamed away, or moved into a file the
// build no longer compiles is the same one `if` with nothing watching it, and no coverage
// percentage in go reports the difference between a branch a test drives and a branch a test
// merely walks past. So the roster is DERIVED -- off this package's own refusal sites and off its
// own test declarations -- rather than written down. Fifteen times on this project a hand written
// list of a class understated the class, and the plan's own draft of this test held ten sentinel
// names in a slice literal, which is exactly the shape that goes stale the day an eleventh
// refusal lands.
//
// It answers in TWO verdicts and both are asserted, because "some test names this refusal" and
// "some code a test runs names this refusal" are different statements, and the gap between them is
// where a refusal loses its last real assertion while the roster goes on reporting it watched. And
// the scan itself is swept under every file name this package's directory holds, because a roster
// that can tell "read nothing" from "found nothing" still cannot tell either of those from "read
// all but one file".
package mls

import (
	"bytes"
	"errors"
	"go/ast"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// ValSem002 and ValSem003, the group id and the epoch
// ---------------------------------------------------------------------------

// framingWideGroupId is a group id wide enough for a position sweep to mean something.
//
// framingTestMemberContent carries a two octet group id, which is the right size for a codec
// golden and the wrong size for this rule: over two octets a comparison that reads the first octet
// and a comparison that reads the whole thing disagree in exactly one place, and one place is a
// coincidence rather than a class. Every octet is a different value, so a comparison that summed,
// sorted or hashed the two ids would still have to see a difference.
func framingWideGroupId() []byte {
	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(0xa0 + i)
	}
	return id
}

// framingNeighbouringEpochs is the epochs ValSem003 is judged against, derived from the one under
// test rather than being the +1 a rule's author pictures.
//
// The two that are not obvious are the interesting ones. epoch^(1<<32) differs from the epoch in
// its HIGH half alone, which is what a comparison narrowed to uint32 accepts -- and the epoch is a
// uint64 in RFC 9420 precisely because it is never reset, so a receiver comparing the low half
// would take a replay from four billion epochs ago as current. math.MaxUint64 is the saturating
// value a hostile peer writes when it wants a comparison to overflow rather than to differ.
func framingNeighbouringEpochs(epoch uint64) []uint64 {
	return []uint64{
		0,
		1,
		epoch - 1,
		epoch,
		epoch + 1,
		epoch ^ (1 << 32),
		math.MaxUint32,
		math.MaxUint64,
	}
}

// TestFramedContentContextRefusesWrongGroupId is ValSem002, over every octet of the group id and
// every length of it rather than over the one corruption somebody thought of.
//
// The sweep is rule 5's, and this project has the measurement behind it twice: a comparison that
// read a prefix passed a suite whose only case flipped octet zero, and a comparison written as
// bytes.HasPrefix passed a ban list that held six comparator names. Both shapes are refused here
// because the offending octet is at every position in turn, and because the length cases run past
// the end of the real id as well as short of it.
//
// The positive case is beside them, so the rule cannot be satisfied by a check that refuses
// everything -- which is the whole of what "wrong group id" would become if it did.
func TestFramedContentContextRefusesWrongGroupId(t *testing.T) {
	content := framingTestMemberContent()
	content.GroupId = framingWideGroupId()
	if err := CheckFramedContentContext(content, content.GroupId, content.Epoch); err != nil {
		t.Fatalf("the receiver's own group id and epoch were refused: %v", err)
	}
	// a copy rather than the same backing array, so what is compared is the VALUE, and a rule
	// that compared slice identity is not passing here on the argument being the same object
	if err := CheckFramedContentContext(content, bytes.Clone(content.GroupId), content.Epoch); err != nil {
		t.Fatalf("an equal group id in another array was refused: %v", err)
	}
	for at := range content.GroupId {
		for bit := 0; bit < 8; bit += 1 {
			other := bytes.Clone(content.GroupId)
			other[at] ^= 1 << uint(bit)
			err := CheckFramedContentContext(content, other, content.Epoch)
			if !errors.Is(err, errWrongGroupId) {
				t.Fatalf("bit %d of group id octet %d flipped: got %v, want errWrongGroupId", bit, at, err)
			}
			// and it is ValSem002 that refused it and not the epoch rule behind it
			if errors.Is(err, errWrongEpoch) {
				t.Fatalf("bit %d of group id octet %d flipped: answered errWrongEpoch too, and the epoch matches",
					bit, at)
			}
		}
	}
	// every truncation and every extension, which is the class a prefix comparison accepts
	for n := 0; n <= 2*len(content.GroupId); n += 1 {
		if n == len(content.GroupId) {
			continue
		}
		resized := make([]byte, n)
		copy(resized, content.GroupId)
		if err := CheckFramedContentContext(content, resized, content.Epoch); !errors.Is(err, errWrongGroupId) {
			t.Fatalf("a %d octet group id against this content's %d: got %v, want errWrongGroupId",
				n, len(content.GroupId), err)
		}
	}
	// a content wrong on BOTH counts is answered as ValSem002, which is the order this rule
	// states and the only case that can observe it. An epoch is a fact about a group, so a
	// message from another group has no epoch this receiver can compare it against, and a body
	// that answered ValSem003 here would be telling its caller that a stranger's message was one
	// of this group's own that had arrived late.
	bothWrong := bytes.Clone(content.GroupId)
	bothWrong[len(bothWrong)-1] ^= 0xff
	if err := CheckFramedContentContext(content, bothWrong, content.Epoch+1); !errors.Is(err, errWrongGroupId) ||
		errors.Is(err, errWrongEpoch) {
		t.Fatalf("a content wrong in both its group id and its epoch: got %v, want errWrongGroupId alone", err)
	}
	// every spelling of an absent group id, for emptyByteSpellings' reason: a rule written on
	// == nil accepts two of the three
	for _, empty := range emptyByteSpellings() {
		if err := CheckFramedContentContext(content, empty.value, content.Epoch); !errors.Is(err, errWrongGroupId) {
			t.Errorf("a group id that is %s: got %v, want errWrongGroupId", empty.what, err)
		}
		// and the mirror: a CONTENT carrying no group id, judged against a real one. a rule
		// that compared lengths and called two zero lengths equal accepts this pair the other
		// way round.
		blank := framingTestMemberContent()
		blank.GroupId = empty.value
		if err := CheckFramedContentContext(blank, content.GroupId, blank.Epoch); !errors.Is(err, errWrongGroupId) {
			t.Errorf("a content whose own group id is %s: got %v, want errWrongGroupId", empty.what, err)
		}
		// two absent ids ARE the same group id. whether an empty group id is legal at all is
		// the group's business and not this rule's, and a rule that answered it here would be
		// refusing a message for a reason it cannot see.
		if err := CheckFramedContentContext(blank, empty.value, blank.Epoch); err != nil {
			t.Errorf("two group ids that are both %s: got %v, want them accepted as equal", empty.what, err)
		}
	}
}

// TestFramedContentContextRefusesWrongEpoch is ValSem003 over framingNeighbouringEpochs, with
// which rows are expected to refuse DERIVED from the epoch rather than written beside them.
//
// Derived because that is what makes the table safe to extend: a row added to the neighbours is
// judged by the same one line that judges the others, so a case whose expectation somebody would
// have had to type cannot be typed wrong.
func TestFramedContentContextRefusesWrongEpoch(t *testing.T) {
	content := framingTestMemberContent()
	refused := 0
	accepted := 0
	for _, epoch := range framingNeighbouringEpochs(content.Epoch) {
		err := CheckFramedContentContext(content, content.GroupId, epoch)
		if epoch == content.Epoch {
			if err != nil {
				t.Fatalf("this content's own epoch %d was refused: %v", epoch, err)
			}
			accepted += 1
			continue
		}
		if !errors.Is(err, errWrongEpoch) {
			t.Fatalf("epoch %d against this content's %d: got %v, want errWrongEpoch",
				epoch, content.Epoch, err)
		}
		// the group id matches on every row, so ValSem002 must not be what answered
		if errors.Is(err, errWrongGroupId) {
			t.Fatalf("epoch %d answered errWrongGroupId, and the group id matches", epoch)
		}
		refused += 1
	}
	// counted, because a neighbour list that stopped producing rows leaves this loop running
	// zero times and reporting PASS, which is the one outcome a sweep must not be able to reach
	if refused == 0 || accepted != 1 {
		t.Fatalf("%d epochs refused and %d accepted, so this sweep states nothing about the rule",
			refused, accepted)
	}
	// the refusal names both epochs. a receiver holding a message from another epoch has to
	// decide whether to buffer it or drop it, and "wrong epoch" alone does not answer that.
	err := CheckFramedContentContext(content, content.GroupId, content.Epoch+1)
	for _, named := range []uint64{content.Epoch, content.Epoch + 1} {
		if !strings.Contains(err.Error(), strconv.FormatUint(named, 10)) {
			t.Errorf("the ValSem003 refusal reads %q and does not name epoch %d; a caller told only that the epoch was wrong has to decode the message again to find out which way",
				err, named)
		}
	}
}

// TestFramedContentContextRefusesANilContent holds the guard that keeps a receive path's own bug
// from becoming a nil dereference inside this package.
//
// nil_argument_test.go sweeps this along with every other nil refusal the package makes and is
// where that class is derived. What this adds is the pairing: the SAME call with a content present
// answers about the context rather than about the argument, so the guard cannot be passing by
// refusing everything.
func TestFramedContentContextRefusesANilContent(t *testing.T) {
	content := framingTestMemberContent()
	if err := CheckFramedContentContext(nil, content.GroupId, content.Epoch); !errors.Is(err, errNilFramedContent) {
		t.Fatalf("a nil content: got %v, want errNilFramedContent", err)
	}
	if err := CheckFramedContentContext(content, content.GroupId, content.Epoch); err != nil {
		t.Fatalf("the same call with a content present: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ValSem004, the sender's leaf
// ---------------------------------------------------------------------------

// framingLeafOccupancies is the occupancy patterns ValSem004 is swept over, and there is more
// than one of them for a reason that was measured rather than reasoned.
//
// A single pattern cannot state this rule. Blanks in the MIDDLE catch a rule that judges only the
// first leaf or only the last -- p4's ValSem401 over psks[0], p5 task 7's erratum loop over
// element zero, p5 task 15's sweep bounded at x < 2. Blanks at the ENDS catch the mirror, a rule
// that SKIPS the first leaf or the last, and that shape is not hypothetical: with this rule
// rewritten to `sender.LeafIndex != 0 && !leafOccupied(...)` -- which accepts every message
// claiming leaf zero, and leaf zero is the one a removed founder leaves blank -- the plan's draft
// of this test passed, and so did this file's own first draft, whose single pattern kept both ends
// occupied precisely to catch the other direction.
//
// So the patterns are GENERATED rather than chosen: one per leaf, blank at that leaf alone, plus a
// wholly occupied tree so the rule cannot be satisfied by refusing everything. Every position is
// then blank in one pattern and occupied in the rest, and TestSenderLeafRefusesBlankLeaf asserts
// that of the patterns rather than trusting this comment.
func framingLeafOccupancies(width int) [][]bool {
	patterns := [][]bool{}
	for blank := 0; blank < width; blank += 1 {
		pattern := make([]bool, width)
		for at := range pattern {
			pattern[at] = at != blank
		}
		patterns = append(patterns, pattern)
	}
	occupied := make([]bool, width)
	for at := range occupied {
		occupied[at] = true
	}
	return append(patterns, occupied)
}

// TestSenderLeafRefusesBlankLeaf is ValSem004 over every leaf of every framingLeafOccupancies
// pattern, with the expectation of each row read off the occupancy rather than written beside it.
func TestSenderLeafRefusesBlankLeaf(t *testing.T) {
	const width = 8
	patterns := framingLeafOccupancies(width)

	// the sweep's own completeness, derived off the patterns it is about to run. Every leaf
	// position has to be blank in at least one pattern and occupied in at least one, or a rule
	// that judged that position alone -- or skipped it -- passes the whole sweep.
	blankIn := make([]int, width)
	occupiedIn := make([]int, width)
	for _, pattern := range patterns {
		if len(pattern) != width {
			t.Fatalf("a pattern is %d leaves wide and this sweep is %d", len(pattern), width)
		}
		for at, filled := range pattern {
			if filled {
				occupiedIn[at] += 1
				continue
			}
			blankIn[at] += 1
		}
	}
	for at := 0; at < width; at += 1 {
		if blankIn[at] == 0 || occupiedIn[at] == 0 {
			t.Fatalf("leaf %d is blank in %d patterns and occupied in %d; a rule that judged only that leaf, or that skipped it, would pass this sweep",
				at, blankIn[at], occupiedIn[at])
		}
	}

	calls := 0
	refused := 0
	for which, pattern := range patterns {
		leafOccupied := func(leaf LeafIndex) bool {
			calls += 1
			return int(leaf) < len(pattern) && pattern[leaf]
		}
		for at, filled := range pattern {
			sender := Sender{SenderType: SenderTypeMember, LeafIndex: LeafIndex(at)}
			err := CheckSenderLeaf(sender, leafOccupied)
			if filled {
				if err != nil {
					t.Errorf("pattern %d: leaf %d is occupied and was refused: %v", which, at, err)
				}
				continue
			}
			if !errors.Is(err, errBlankSenderLeaf) {
				t.Errorf("pattern %d: leaf %d is blank: got %v, want errBlankSenderLeaf", which, at, err)
				continue
			}
			// the refusal names the leaf, so a receiver is not left walking its own tree to
			// work out which message it just dropped
			if !strings.Contains(err.Error(), strconv.Itoa(at)) {
				t.Errorf("pattern %d: the ValSem004 refusal for leaf %d reads %q and does not name the leaf",
					which, at, err)
			}
			refused += 1
		}
	}
	// counted, because a pattern generator that stopped producing blanks leaves every loop above
	// running over occupied leaves alone and reporting PASS
	if refused != width {
		t.Fatalf("%d leaves refused over %d patterns, want %d; every pattern but the last blanks exactly one leaf",
			refused, len(patterns), width)
	}
	// the predicate was actually consulted. a rule that answered nil without calling it accepts
	// every leaf, and every "occupied" row above passes over such a rule unchanged.
	if calls == 0 {
		t.Fatal("the occupancy predicate was never called, so whatever decided the rows above was not the tree")
	}

	full := patterns[len(patterns)-1]
	fullyOccupied := func(leaf LeafIndex) bool { return int(leaf) < len(full) && full[leaf] }
	// a leaf past the end of the tree entirely, which is what a hostile peer writes
	beyond := Sender{SenderType: SenderTypeMember, LeafIndex: LeafIndex(width + 1)}
	if err := CheckSenderLeaf(beyond, fullyOccupied); !errors.Is(err, errBlankSenderLeaf) {
		t.Errorf("a leaf past the end of the tree: got %v, want errBlankSenderLeaf", err)
	}
	widest := Sender{SenderType: SenderTypeMember, LeafIndex: math.MaxUint32}
	if err := CheckSenderLeaf(widest, fullyOccupied); !errors.Is(err, errBlankSenderLeaf) {
		t.Errorf("the widest leaf index a uint32 holds: got %v, want errBlankSenderLeaf", err)
	}

	// every sender type that is not a member, derived off the registry rather than being the one
	// somebody thought of, at every leaf position under the pattern that blanks it -- so the
	// carve out is exercised at each position this rule would otherwise have refused. RFC 9420
	// section 6 gives those three types no leaf, so a rule that judged their zero valued
	// LeafIndex would be answering about a field the message never carried.
	others := 0
	for _, senderType := range senderTypes(t) {
		if senderType == SenderTypeMember {
			continue
		}
		for at := 0; at < width; at += 1 {
			pattern := patterns[at]
			if pattern[at] {
				t.Fatalf("pattern %d was expected to blank leaf %d and does not", at, at)
			}
			occupancy := func(leaf LeafIndex) bool { return int(leaf) < len(pattern) && pattern[leaf] }
			sender := Sender{SenderType: senderType, LeafIndex: LeafIndex(at)}
			if err := CheckSenderLeaf(sender, occupancy); err != nil {
				t.Errorf("sender type %d at blank leaf %d was refused: %v; only a member carries a leaf index",
					senderType, at, err)
			}
			others += 1
		}
	}
	if others == 0 {
		t.Fatal("the sender type registry produced nothing but member, so the carve out above was never exercised")
	}
	// and the predicate itself is refused rather than dereferenced, AHEAD of the sender type: a
	// caller holding no occupancy test must not be told its non member sender was checked
	if err := CheckSenderLeaf(Sender{SenderType: SenderTypeNewMemberCommit}, nil); !errors.Is(err, errNilLeafOccupancyTest) {
		t.Errorf("a nil occupancy test under a non member sender: got %v, want errNilLeafOccupancyTest", err)
	}
}

// ---------------------------------------------------------------------------
// ValSem009, reached three ways
// ---------------------------------------------------------------------------

// TestCommitRefusesMissingConfirmationTag is ValSem009 stated as the property the two declaration
// sites' comments claim and that nothing asserted: it is ONE condition and ONE value however it is
// reached.
//
// Three layers raise it -- the verifier, the encoder and the decoder -- and each is held on its
// own next door, by TestVerifyRefusesACommitWithNoConfirmationTag and by
// TestFramedContentAuthDataRequiresAConfirmationTagOnACommit. What neither says is that the three
// answer the SAME sentinel, which is the claim errMissingConfirmationTag's comment makes and the
// one a later task splitting the condition into two values would break while leaving both of those
// tests green.
func TestCommitRefusesMissingConfirmationTag(t *testing.T) {
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	authContent, err := SignAuthenticatedContent(crypto, priv, WireFormatPrivateMessage,
		framingTestCommitContent(), groupContext)
	if err != nil {
		t.Fatalf("sign a commit: %v", err)
	}

	// the verifier, handed a commit whose confirmation tag was never set
	if err := VerifyAuthenticatedContent(crypto, pub, authContent, groupContext); !errors.Is(err, errMissingConfirmationTag) {
		t.Fatalf("verify: got %v, want errMissingConfirmationTag", err)
	}
	// the encoder, which refuses to serialize one at all
	if err := authContent.Auth.MarshalMLS(syntax.NewWriter(), ContentTypeCommit); !errors.Is(err, errMissingConfirmationTag) {
		t.Fatalf("marshal: got %v, want errMissingConfirmationTag", err)
	}
	// and the decoder, handed the wire legal zero length tag that is the encoding of "no tag".
	// the bytes are built through the Writer rather than spliced at a computed offset: an
	// opaque<V> length prefix is one octet under 64 and two at 64, so an offset written for one
	// signature scheme addresses the wrong octet under the next.
	w := syntax.NewWriter()
	w.WriteOpaque(authContent.Auth.Signature)
	w.WriteOpaque(nil)
	emptyTagged, err := w.Bytes()
	if err != nil {
		t.Fatalf("the commit auth data this case hands the decoder: %v", err)
	}
	decoded := FramedContentAuthData{
		Signature:       []byte("untouched"),
		ConfirmationTag: []byte("untouched"),
	}
	if err := decoded.UnmarshalMLS(syntax.NewReader(emptyTagged), ContentTypeCommit); !errors.Is(err, errMissingConfirmationTag) {
		t.Fatalf("unmarshal: got %v, want errMissingConfirmationTag", err)
	}
	if !bytes.Equal(decoded.Signature, []byte("untouched")) ||
		!bytes.Equal(decoded.ConfirmationTag, []byte("untouched")) {
		t.Errorf("the refused decode wrote %+v into the caller's value; a caller left holding half a message this package rejected has a signature and a tag that were never carried together",
			decoded)
	}

	// and the positive case, so none of the three is passing by refusing every commit
	authContent.Auth.ConfirmationTag = bytes.Repeat([]byte{0x01}, crypto.HashSize())
	if err := VerifyAuthenticatedContent(crypto, pub, authContent, groupContext); err != nil {
		t.Fatalf("a commit carrying a confirmation tag was refused: %v", err)
	}
	tagged := syntax.NewWriter()
	if err := authContent.Auth.MarshalMLS(tagged, ContentTypeCommit); err != nil {
		t.Fatalf("marshalling a commit that carries its tag: %v", err)
	}
	withTag, err := tagged.Bytes()
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if err := decoded.UnmarshalMLS(syntax.NewReader(withTag), ContentTypeCommit); err != nil {
		t.Fatalf("decoding a commit that carries its tag: %v", err)
	}
	if !bytes.Equal(decoded.ConfirmationTag, authContent.Auth.ConfirmationTag) {
		t.Errorf("round tripped the tag as %x, want %x", decoded.ConfirmationTag, authContent.Auth.ConfirmationTag)
	}
}

// ---------------------------------------------------------------------------
// the refusal roster
// ---------------------------------------------------------------------------

// refusalSource is one parsed file of a package together with the two facts the roster reads off
// its NAME rather than off its contents: where it came from, and whether it is part of the test
// binary. Both halves of the roster run over a slice of these, so a synthetic control goes through
// the same code the real scan uses rather than through a second copy of it.
type refusalSource struct {
	path   string
	parsed parsedSource
	isTest bool
}

// refusalScan is what one pass over a package answers.
//
// The parts are kept separate rather than reduced to a verdict, because each of them is a way the
// scan can go quiet, and a scan that has gone quiet issues the real source exactly the clean bill
// a working one issues. An empty refusals map means no refusal site was recognised; an empty
// namedBy or exercisedBy means no test was recognised; and any one of them on its own reports zero
// unnamed and zero unexercised refusals.
type refusalScan struct {
	// every package level variable of the non test source that some function answers at an
	// error result, mapped to the file declaring it. this is the roster: a refusal is a value a
	// function hands back in place of an answer.
	refusals map[string]string
	// identifiers the test binary names, mapped to the test that names them -- directly, or
	// through one package level declaration of the test binary that the test itself names.
	namedBy map[string]string
	// identifiers named from inside code a Test can reach and RUN, mapped to the declaration
	// whose body names them. the naming side above counts a mention in a table; this one counts
	// only a mention in a body, and the two disagree over three of this package's refusals.
	exercisedBy map[string]string
	// package level variables the non test source declares and never answers at an error
	// result. outside the roster by construction, and reported so the number is visible.
	declaredNotReturned []string
}

// unnamed is the roster's verdict: the refusals no test names, sorted so the message is stable.
func (self refusalScan) unnamed() []string {
	missing := []string{}
	for name := range self.refusals {
		if _, named := self.namedBy[name]; !named {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// unexercised is the roster's second verdict: the refusals that no code a test reaches names.
//
// Separate from unnamed because they are different failures, and the difference was measured
// rather than reasoned. A refusal reported unnamed has nothing in the test binary mentioning it at
// all. A refusal reported unexercised IS mentioned -- but only in data: a table that enumerates
// sentinel names, which states that a name exists and cannot state that any branch answers it,
// because there is no branch in it. Three of this package's refusals sit in exactly that position:
// ErrLeafHasNoChildren, ErrRootHasNoParent and ErrRootHasNoSibling are listed in
// treeMathOwnedErrors so the error class sweeps can run over them, and with their real assertions
// -- the errors.Is rows inside checkNodeInvariants -- replaced by `err == nil`, the naming side
// went on reporting a clean bill over all of them.
func (self refusalScan) unexercised() []string {
	missing := []string{}
	for name := range self.refusals {
		if _, exercised := self.exercisedBy[name]; !exercised {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// errorResultPositions is the indices of a signature's results that are of type error.
//
// Positional rather than "any identifier in any return", because a package level variable can be
// answered at a non error position perfectly legitimately -- a cached table, a shared empty value
// -- and a roster that swept those in would demand a test for things that are not refusals at all.
// The control commits that shape and this gate must be seen not to report it.
func errorResultPositions(function *ast.FuncDecl) []int {
	positions := []int{}
	if function.Type.Results == nil {
		return positions
	}
	at := 0
	for _, field := range function.Type.Results.List {
		names := len(field.Names)
		if names == 0 {
			names = 1
		}
		identifier, isIdentifier := field.Type.(*ast.Ident)
		for i := 0; i < names; i += 1 {
			if isIdentifier && identifier.Name == "error" {
				positions = append(positions, at)
			}
			at += 1
		}
	}
	return positions
}

// identifiersIn is every identifier a node mentions, at any depth.
//
// At any depth, because half of this package's refusals are wrapped -- fmt.Errorf("%w: %d", errX,
// code) is the house form -- and a reader matching only a bare `return errX` sees the subset whose
// sentence happened to be short enough to fit on one line, which is a class whose membership is
// decided by formatting.
func identifiersIn(node ast.Node) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(node, func(inner ast.Node) bool {
		if identifier, isIdentifier := inner.(*ast.Ident); isIdentifier {
			found[identifier.Name] = true
		}
		return true
	})
	return found
}

// packageLevelMentionsIn maps each package level declaration of one file to the identifiers it
// mentions. Functions, methods, vars, consts and types all count: a table driving a test is as
// much a naming of a sentinel as the test body is.
func packageLevelMentionsIn(parsed parsedSource, into map[string]map[string]bool) {
	for _, declaration := range parsed.file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			name := typed.Name.Name
			if typed.Recv != nil && len(typed.Recv.List) == 1 {
				name = receiverTypeName(typed.Recv.List[0].Type) + "." + name
			}
			into[name] = identifiersIn(typed)
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch typedSpec := spec.(type) {
				case *ast.ValueSpec:
					for _, ident := range typedSpec.Names {
						if ident.Name != "_" {
							into[ident.Name] = identifiersIn(typedSpec)
						}
					}
				case *ast.TypeSpec:
					into[typedSpec.Name.Name] = identifiersIn(typedSpec)
				}
			}
		}
	}
}

// executableMentionsIn maps each package level declaration of one file to the identifiers it
// mentions from inside code that RUNS -- a function's body, or the body of a function literal
// however deeply that literal sits inside a declaration's value.
//
// The data a declaration holds is deliberately not counted, and that is the whole difference
// between this and packageLevelMentionsIn above. `var owned = map[string]error{"X": X}` mentions
// X, and what the mention states is that X exists and belongs to a class; it cannot state that
// anything answers X. A table whose rows are DRIVEN still counts here, through the body that
// drives them -- nilArgumentRows is a function and its rows are written inside it, which is the
// shape a table has when a test does more with it than list it.
func executableMentionsIn(parsed parsedSource, into map[string]map[string]bool) {
	for _, declaration := range parsed.file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if typed.Body == nil {
				continue
			}
			name := typed.Name.Name
			if typed.Recv != nil && len(typed.Recv.List) == 1 {
				name = receiverTypeName(typed.Recv.List[0].Type) + "." + name
			}
			into[name] = identifiersIn(typed.Body)
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch typedSpec := spec.(type) {
				case *ast.ValueSpec:
					bodies := functionLiteralIdentifiersIn(typedSpec)
					for _, ident := range typedSpec.Names {
						if ident.Name != "_" {
							into[ident.Name] = bodies
						}
					}
				case *ast.TypeSpec:
					into[typedSpec.Name.Name] = functionLiteralIdentifiersIn(typedSpec)
				}
			}
		}
	}
}

// functionLiteralIdentifiersIn is every identifier the BODIES of a node's function literals
// mention, and nothing its data mentions.
//
// A closure written inside a table is code, and a sentinel named in one is named by something that
// runs. A sentinel named in the row beside it is not.
func functionLiteralIdentifiersIn(node ast.Node) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(node, func(inner ast.Node) bool {
		literal, isLiteral := inner.(*ast.FuncLit)
		if !isLiteral || literal.Body == nil {
			return true
		}
		for name := range identifiersIn(literal.Body) {
			found[name] = true
		}
		return true
	})
	return found
}

// refusalSitesIn collects, out of one non test file, the package level variables its functions
// answer at an error result.
func refusalSitesIn(parsed parsedSource, path string, packageVars map[string]string,
	into map[string]string) {
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		positions := errorResultPositions(function)
		if len(positions) == 0 {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			statement, isReturn := node.(*ast.ReturnStmt)
			if !isReturn {
				return true
			}
			for _, at := range positions {
				// a bare `return` inside a function with named results, and a `return f()`
				// forwarding somebody else's pair, both name nothing at this position
				if at >= len(statement.Results) {
					continue
				}
				for name := range identifiersIn(statement.Results[at]) {
					if _, isPackageVar := packageVars[name]; isPackageVar {
						into[name] = path
					}
				}
			}
			return true
		})
	}
}

// reachedFromTests is every package level declaration of the test binary a Test can reach: the
// Test roots themselves, whatever they name, whatever that names, and so on to a fixed point.
//
// Transitive, where the naming side below stops at one hop, and the two depths answer different
// questions. Naming asks whether anything in the test binary still mentions the sentinel, and a
// mention arbitrarily far from anything a test runs is not evidence of that, so it stops.
// Exercising asks whether a Test RUNS code that names it, and the code that does is routinely two
// calls down: this package's tree math sentinels are asserted inside checkNodeInvariants, which no
// Test body names -- three sweep helpers do, and the Tests name those. A one hop closure does not
// reach it. The walk is also what says a declaration is reached at all, which matters in the other
// direction: over two hundred of this package's test declarations are reachable from no Test, and
// a sentinel named only in one of those is named by nothing that runs.
func reachedFromTests(roots map[string]map[string]bool,
	declarations map[string]map[string]bool) map[string]bool {
	reached := map[string]bool{}
	pending := slices.Sorted(maps.Keys(roots))
	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if reached[name] {
			continue
		}
		reached[name] = true
		for mentioned := range declarations[name] {
			if _, isDeclaration := declarations[mentioned]; isDeclaration && !reached[mentioned] {
				pending = append(pending, mentioned)
			}
		}
	}
	return reached
}

// scanRefusals runs the whole roster over one package's sources.
//
// The naming side reaches ONE hop past a Test function and no further, and the depth is measured
// rather than chosen. Every refusal this package makes is named either inside a Test body or
// inside one package level declaration of the test binary that a Test body names -- nil_argument
// _test.go's row table and tree math's boundary rows are the second shape, and a row driving a
// sentinel is as much a test of it as a body naming it directly. The full transitive closure over
// this package's test declarations would be a different thing: it accepts a mention arbitrarily
// far from anything a test runs, which is a roster that reports clean for a sentinel no test can
// reach.
func scanRefusals(sources []refusalSource) refusalScan {
	packageVars := map[string]string{}
	for _, source := range sources {
		if source.isTest {
			continue
		}
		for _, name := range packageLevelVarNamesIn(source.parsed) {
			packageVars[name] = source.path
		}
	}
	scan := refusalScan{refusals: map[string]string{}, namedBy: map[string]string{},
		exercisedBy: map[string]string{}}
	for _, source := range sources {
		if source.isTest {
			continue
		}
		refusalSitesIn(source.parsed, source.path, packageVars, scan.refusals)
	}
	for name := range packageVars {
		if _, isRefusal := scan.refusals[name]; !isRefusal {
			scan.declaredNotReturned = append(scan.declaredNotReturned, name)
		}
	}
	slices.Sort(scan.declaredNotReturned)

	roots := map[string]map[string]bool{}
	testDeclarations := map[string]map[string]bool{}
	testExecutable := map[string]map[string]bool{}
	for _, source := range sources {
		if !source.isTest {
			continue
		}
		packageLevelMentionsIn(source.parsed, testDeclarations)
		executableMentionsIn(source.parsed, testExecutable)
		for _, declaration := range source.parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}
			roots[function.Name.Name] = identifiersIn(function)
		}
	}
	// sorted so that the test CREDITED with naming a refusal is the same one on every run; the
	// verdict does not depend on it, and a message that moved between runs would read as churn
	for _, root := range slices.Sorted(maps.Keys(roots)) {
		for name := range roots[root] {
			if _, already := scan.namedBy[name]; !already {
				scan.namedBy[name] = root
			}
		}
	}
	for _, root := range slices.Sorted(maps.Keys(roots)) {
		for through := range roots[root] {
			for name := range testDeclarations[through] {
				if _, already := scan.namedBy[name]; !already {
					scan.namedBy[name] = root + " through " + through
				}
			}
		}
	}
	// the second verdict, over the closure rather than one hop, and over bodies rather than over
	// everything a declaration holds. sorted for the same reason the naming side is: the
	// declaration credited with exercising a refusal is the same one on every run.
	for _, declaration := range slices.Sorted(maps.Keys(reachedFromTests(roots, testDeclarations))) {
		for name := range testExecutable[declaration] {
			if _, already := scan.exercisedBy[name]; !already {
				scan.exercisedBy[name] = declaration
			}
		}
	}
	return scan
}

// refusalSourcesOfThisPackage parses every go file of the package directory, test files included.
// Test files are not skipped here, for the reason key_schedule_deps_test.go records next door: a
// declaration in a test file of package mls is a declaration of package mls, and the naming half
// of this roster is entirely about them.
func refusalSourcesOfThisPackage(t *testing.T) []refusalSource {
	t.Helper()
	sources := []refusalSource{}
	for _, path := range packageSourcePaths(t) {
		sources = append(sources, refusalSource{
			path:   path,
			parsed: mustParseSource(t, path),
			isTest: strings.HasSuffix(path, "_test.go"),
		})
	}
	return sources
}

// The control's production half. Five refusals and two values that are not refusals, so both
// directions of the class are committed: what the roster must include, and what it must leave out.
//
// The five refusals are one per shape the two verdicts can disagree about. errNamedDirectly is
// named and exercised; errNamedByNothing is neither; errNamedOnlyByAnUncalledHelper is named by a
// body no test reaches, which is neither; and the two that cross are errNamedThroughATable, named
// by a table and exercised by nothing, and errNamedByADeepHelper, exercised two calls below a Test
// and named by nothing within one hop of it. A control holding only the agreeing shapes would let
// either verdict be quietly reduced to the other.
const refusalRosterProductionControl = `package control

import (
	"errors"
	"fmt"
)

var (
	errNamedDirectly               = errors.New("control: a test body names this")
	errNamedThroughATable          = errors.New("control: a table a test names names this")
	errNamedByADeepHelper          = errors.New("control: a helper two calls below a test names this")
	errNamedOnlyByAnUncalledHelper = errors.New("control: only a helper no test names names this")
	errNamedByNothing              = errors.New("control: nothing names this")
	errNeverReturned               = errors.New("control: no error result ever answers this")
	cachedTable                    = []byte("control: not an error and not a refusal")
)

func refuse(which int) ([]byte, error) {
	switch which {
	case 0:
		return nil, errNamedDirectly
	case 1:
		return nil, fmt.Errorf("%w: wrapped, which a bare identifier match does not see", errNamedThroughATable)
	case 2:
		return nil, errNamedOnlyByAnUncalledHelper
	case 3:
		return nil, errNamedByNothing
	case 4:
		return nil, errNamedByADeepHelper
	}
	return cachedTable, nil
}

func reads() bool {
	return errNeverReturned != nil
}
`

// The control's test half: one Test naming a refusal directly and a table naming a second, a chain
// of two helpers naming a third, and one helper naming a fourth that no Test reaches at all.
const refusalRosterTestControl = `package control

import "testing"

var controlTable = []error{errNamedThroughATable}

func TestControlNamesOneDirectlyAndOneThroughATable(t *testing.T) {
	if errNamedDirectly == nil || controlTable == nil {
		t.Fatal("control")
	}
	if controlCallsAHelper() == nil {
		t.Fatal("control")
	}
}

// two calls below the Test, which is where this package's real assertions on the tree math
// sentinels live and is one hop further than the naming side reaches.
func controlCallsAHelper() error {
	return controlHelperTheFirstOneCalls()
}

func controlHelperTheFirstOneCalls() error {
	return errNamedByADeepHelper
}

func helperNoTestNames() error {
	return errNamedOnlyByAnUncalledHelper
}
`

// The whole answer the control commits, written out because this is the control's own expected
// answer and not a class read off anything: a control whose expectation was derived from the same
// code it is controlling states nothing.
//
// Three lists rather than one, because the roster answers in more than one way and each of them is
// a way it can go quiet. The refusals are what the production scan must recognise; a scan that
// recognised fewer would report every missing one as watched by nobody's test, which is the same
// clean bill a working scan gives.
var refusalRosterControlRefusals = []string{
	"errNamedByADeepHelper", "errNamedByNothing", "errNamedDirectly",
	"errNamedOnlyByAnUncalledHelper", "errNamedThroughATable",
}

// The refusals of the control that no test names within one hop of a Test body.
var refusalRosterControlUnnamed = []string{
	"errNamedByADeepHelper", "errNamedByNothing", "errNamedOnlyByAnUncalledHelper",
}

// The refusals of the control that no body a Test reaches names. It crosses the list above in both
// directions on purpose: errNamedByADeepHelper is unnamed and exercised, errNamedThroughATable is
// named and unexercised, and a roster that answered one verdict twice fails on both.
var refusalRosterControlUnexercised = []string{
	"errNamedByNothing", "errNamedOnlyByAnUncalledHelper", "errNamedThroughATable",
}

// assertRefusalRosterControl runs the control through the same scanRefusals the real package goes
// through, under the file names it is handed, and holds it to that whole answer.
//
// The names are parameters because a scan can be narrowed per FILE, and a control that only ever
// ran under "control.go" could not see that: it is the same file every time, so a predicate over
// the path either excludes the control always -- which every assertion here catches -- or never.
// TestTheRefusalRosterReadsAFileWhateverItIsNamed is what runs it under the names that matter.
func assertRefusalRosterControl(t *testing.T, productionPath string, testPath string) {
	t.Helper()
	control := scanRefusals([]refusalSource{
		{path: productionPath, parsed: mustParseText(t, productionPath, refusalRosterProductionControl)},
		{path: testPath, parsed: mustParseText(t, testPath, refusalRosterTestControl), isTest: true},
	})
	if got := slices.Sorted(maps.Keys(control.refusals)); !slices.Equal(got, refusalRosterControlRefusals) {
		t.Errorf("with the control's production half named %q the roster read the refusals %v, want %v; a refusal site it stops recognising is a refusal of the real source that nothing is watching",
			productionPath, got, refusalRosterControlRefusals)
	}
	if got := control.unnamed(); !slices.Equal(got, refusalRosterControlUnnamed) {
		t.Errorf("with the control named %q and %q the roster read %v as unnamed, want %v; a shape it lets through is a shape the real source can be written in, and a shape it reports is a refusal the real source would be failed for having",
			productionPath, testPath, got, refusalRosterControlUnnamed)
	}
	if got := control.unexercised(); !slices.Equal(got, refusalRosterControlUnexercised) {
		t.Errorf("with the control named %q and %q the roster read %v as unexercised, want %v; the two verdicts cross in this control, so a roster answering one of them twice fails here",
			productionPath, testPath, got, refusalRosterControlUnexercised)
	}
	if _, sweptIn := control.refusals["cachedTable"]; sweptIn {
		t.Errorf("named %q the roster read the control's non error package variable as a refusal, so it is matching a return position rather than an error result",
			productionPath)
	}
	if !slices.Contains(control.declaredNotReturned, "errNeverReturned") {
		t.Errorf("named %q the control declares errNeverReturned and no error result answers it, and the roster did not put it outside the class: %v",
			productionPath, control.declaredNotReturned)
	}
}

// TestEveryRefusalThisPackageShipsIsNamedByATest is the framing refusal roster, widened to the
// package because every way of fencing "framing" off is a list.
//
// The plan's draft of this test held the ten framing sentinels in a slice literal. That is the
// shape rule 5 exists for: it reports clean for the eleventh refusal, and this package has already
// grown from five validation stand ins to eight while nobody edited such a list. The two
// alternatives considered were a file name prefix, which is the join gate that exempted a file by
// base name, and a type list, which is the same enumeration one indirection out. So the class is
// every refusal the package's non test source can hand a caller, and the framing ten are a subset
// of it that no edit can drop out of.
//
// The property is that some test NAMES the refusal. Deliberately not "some test executes this
// branch": go's coverage cannot tell a branch a test drives from one it walks past, and a sentinel
// no test mentions anywhere is the failure this catches -- a refusal whose test was deleted,
// renamed away, or moved into a file the build no longer compiles.
//
// Two things keep it from reporting clean once it has stopped reading. The control is run through
// the same scanRefusals the real package goes through and must produce the whole answer it commits;
// and the real scan must find this task's own three sentinels among the refusals it read, which is
// what says it read framing_protect.go rather than an empty directory. Neither of those sees a scan
// that read all but ONE file, which is what TestTheRefusalRosterReadsAFileWhateverItIsNamed holds,
// and neither sees a refusal whose last remaining mention is in a list, which is what
// TestEveryRefusalThisPackageShipsIsNamedByCodeATestRuns holds.
func TestEveryRefusalThisPackageShipsIsNamedByATest(t *testing.T) {
	assertRefusalRosterControl(t, "control.go", "control_test.go")

	scan := scanRefusals(refusalSourcesOfThisPackage(t))
	if len(scan.refusals) == 0 || len(scan.namedBy) == 0 {
		t.Fatalf("the scan read %d refusals and %d named identifiers out of this package, so whatever it reports below, it read nothing",
			len(scan.refusals), len(scan.namedBy))
	}
	// this task's own three, which is what says the scan reached framing_protect.go. named
	// rather than derived on purpose: it is a guard on the SCAN and not the class, and a guard
	// derived from the thing it guards guards nothing.
	for _, anchor := range []string{"errWrongGroupId", "errWrongEpoch", "errBlankSenderLeaf"} {
		if _, read := scan.refusals[anchor]; !read {
			t.Fatalf("framing_protect.go answers %s at an error result and the scan did not read it, so it read something other than this package",
				anchor)
		}
	}
	for _, name := range scan.unnamed() {
		t.Errorf("%s is a refusal %s answers and no test in this package names it; the refusal has lost its test",
			name, scan.refusals[name])
	}
	t.Logf("%d refusals watched, %d package variables outside the class because no error result answers them",
		len(scan.refusals), len(scan.declaredNotReturned))
}

// TestEveryRefusalThisPackageShipsIsNamedByCodeATestRuns is the roster's second verdict, and it is
// here because the first one can be satisfied by a mention that runs nothing.
//
// The naming side reaches one hop past a Test body, and a package level TABLE is a legal hop. That
// is deliberate and it is right for a table that DRIVES its rows -- nil_argument_test.go's do --
// but a table can also merely enumerate, and an enumeration credits a refusal while asserting
// nothing whatever about it. Three of this package's refusals were in exactly that position when
// this was written: ErrLeafHasNoChildren, ErrRootHasNoParent and ErrRootHasNoSibling are listed in
// treeMathOwnedErrors so that the error class sweeps have a class to run over, and their real
// assertions are the errors.Is rows inside checkNodeInvariants -- a helper no Test body names, two
// calls below the Tests that walk the tree, which one hop does not reach. Replacing those rows
// with `err == nil`, so that two of the three lose their only real assertion, left the naming side
// reporting a clean bill over all 103 refusals and the full suite passing. Measured, not supposed.
//
// So this verdict asks the other question: is the refusal named by code a Test RUNS. Mentions in
// data do not count, for the reason executableMentionsIn gives. Mentions arbitrarily deep DO
// count, over the closure of what the Tests reach rather than one hop, because the body that
// actually asserts a refusal is routinely a helper or two below the Test that runs it. The two
// verdicts are different classes in both directions and the control commits both directions.
func TestEveryRefusalThisPackageShipsIsNamedByCodeATestRuns(t *testing.T) {
	assertRefusalRosterControl(t, "control.go", "control_test.go")

	scan := scanRefusals(refusalSourcesOfThisPackage(t))
	if len(scan.refusals) == 0 || len(scan.exercisedBy) == 0 {
		t.Fatalf("the scan read %d refusals and %d identifiers named from a body a test reaches, so whatever it reports below, it read nothing",
			len(scan.refusals), len(scan.exercisedBy))
	}
	// the same three anchors the naming side uses, and here for the same reason: a guard on the
	// SCAN rather than on the class, because a guard derived from the thing it guards guards
	// nothing.
	for _, anchor := range []string{"errWrongGroupId", "errWrongEpoch", "errBlankSenderLeaf"} {
		if _, read := scan.refusals[anchor]; !read {
			t.Fatalf("framing_protect.go answers %s at an error result and the scan did not read it, so it read something other than this package",
				anchor)
		}
	}
	unexercised := scan.unexercised()
	for _, name := range unexercised {
		t.Errorf("%s is a refusal %s answers, and no body any test in this package reaches names it; whatever still mentions it does so the way a list does, so the assertion that held this refusal is gone and has to be written back into code a test runs",
			name, scan.refusals[name])
	}
	t.Logf("%d refusals watched, %d of them named from a body a test reaches",
		len(scan.refusals), len(scan.refusals)-len(unexercised))
}

// TestTheRefusalRosterReadsAFileWhateverItIsNamed is the half that says the roster read the whole
// package rather than most of it.
//
// The roster can already tell "read nothing" from "found nothing": the control commits both
// directions of the class, and three anchors say the real scan reached framing_protect.go. Neither
// of those sees a scan that read all but ONE file. A base name exemption at the top of
// refusalSitesIn -- `if strings.HasSuffix(path, "tree.go") { return }`, which is the exact shape
// rule 5 exists for and which this project has already shipped once in a join gate -- dropped nine
// refusal sites out of the watched class, moved them quietly into declaredNotReturned, and the
// roster issued a clean bill with the full suite green. Measured.
//
// So the property here is the one a narrowing of any shape breaks: what the roster answers about a
// file does not depend on what the file is CALLED. The control is run through it under every name
// this package's own directory holds, in the production position and in the test position at once,
// and must produce the same whole answer under each. A predicate over the path -- suffix, prefix,
// equality, extension, directory -- fails at the first name it excludes, whatever it is keyed on,
// because the names are the package's rather than this test's. That is the derived form of the
// rule: the class of file names is read off the package, so a file added tomorrow is swept without
// anybody editing a list.
func TestTheRefusalRosterReadsAFileWhateverItIsNamed(t *testing.T) {
	paths := packageSourcePaths(t)
	// counted, because a path list that came back empty leaves the loop below running zero times
	// and reporting PASS, which is the one outcome a sweep must not be able to reach
	if len(paths) < 2 {
		t.Fatalf("the package listed %d source files, so this sweep ran the control under nothing",
			len(paths))
	}
	if !slices.Contains(paths, "framing_protect.go") {
		t.Fatalf("the path list holds %d names and framing_protect.go is not among them, so it is not this package's directory that was read",
			len(paths))
	}
	for _, path := range paths {
		assertRefusalRosterControl(t, path, path)
	}
	t.Logf("the control produced its whole answer under each of %d file names", len(paths))
}

// ---------------------------------------------------------------------------
// the rules this package exports and nothing applies
// ---------------------------------------------------------------------------

// rulesThisPackageExportsAndNothingApplies is every exported rule of this package that nothing in
// its own non test source runs, written down with the reason each one is there.
//
// The class is DERIVED below; this is the answer that derivation must produce, which is the same
// shape as refusalRosterControlUnnamed above and as crypto_test.go's
// packageDeclarationsAwaitingTheirFirstCaller. It is held in BOTH directions, which is what makes
// an entry expire by failing: a rule that appears and is not written here fails until somebody
// gives the reason, and a rule written here that has since been wired fails until somebody takes
// it off. The excuse survives exactly as long as the condition it names.
//
// CheckFramedContentContext and CheckSenderLeaf WERE the reason this list was written and are not
// on it any more. The entry said p6 produces the two signatures and p8 puts them on the receive
// paths, so OpenPublicMessage and OpenPrivateMessage verified a signature and a membership tag and
// did not yet ask which group or which epoch the content names, or whether the sender's leaf is
// occupied -- and that nothing in this package said so, which is what a p8 that slipped would have
// turned into a receive path accepting another group's message with nothing anywhere failing. The
// caller arrived one plan earlier than the entry expected: p7 task 18's (*Group).ProcessMessage
// runs the sender leaf rule inside its signature key resolver, so a member message naming a blanked
// leaf never reaches a key, and runs the context rule the moment the framed content exists. This
// list failed until both entries came off, which is how the excuse was built to expire.
//
// CheckUpdatePathKeyUniqueness WAS here for that reason and is not any more, which is the half of
// this list that is easiest to forget: p7 task 10's ValSem207PathEncryptionKeysUnique is the funnel
// tree_sync.go's comment named, so the rule has the caller it was waiting for and this list failed
// until the entry came off.
//
// ValidateCommit is p7 task 10's aggregate, and it is here for ValidateProposalList's reason rather
// than because anything about it is unfinished. Its twelve rules are all applied -- ValidateCommit
// runs every one of them -- but the commit paths that call the aggregate are tasks 18 and 22, which
// have not landed. Until they do, this package ships a section 12.4.2 validator that its own commit
// construction does not run.
//
// ValSem205ConfirmationTag is the one rule of that file the aggregate deliberately does not run,
// and it is unwired for a reason that is a fact about the protocol rather than about the plan's
// order: the confirmation key belongs to the epoch the commit OPENS, so it does not exist until the
// new key schedule has been derived. The caller that has one is p7 task 18.
//
// ValidateUpdatePathLeafNode WAS here and is not any more, and it is the sharpest instance this
// list has produced. The entry said it is RFC 9420 section 7.3's commit door -- the third of the
// validator's three expectations, the one that had no caller at all while three comments in this
// package said it was already built -- and that the client which must call it is the one PROCESSING
// a commit. That client is p7 task 18's (*Group).ProcessMessage, it landed, and this list failed
// until the entry came off: the leaf of a received update path is now judged at the sender's
// position before MergeUpdatePath is allowed to compare a chain against the parent_hash that leaf
// carries. It is the entry that named its caller exactly right and had to wait five tasks for it.
//
// ValSem300NoTrailingBlankNodes ARRIVED on p7 task 13, and what put it here is a repair rather than
// a gap. Section 12.4.3.3's trailing blank rule is stated over the ARRAY a ratchet_tree extension
// travels as: the encoder writes it with the blanks stripped and (*RatchetTree).UnmarshalMLS refuses
// a padded one INLINE, which is where the rule fires today. The two production callers of the
// exported door were asking it of an IN-MEMORY tree, which is held at the full width 2^(d+1)-1 --
// so every group whose size is not a power of two was refused, by its own commit door and by its
// own RatchetTree accessor, over a tree that encodes and decodes perfectly. Both callers now state
// the half that is true of a tree at full width, and this door is left for the caller that has a
// DECODED array and no decode of its own to have run: task 16's welcome path, which is handed a
// ratchet tree out of band.
//
// ValidateProposalList WAS here on the same reasoning and is not any more, which is this list
// expiring by failing rather than by anybody remembering it. The entry said the commit paths that
// call the aggregate are tasks 22 and 25 and that until they land this package ships a section 12.2
// validator its own construction does not run -- and the second half of that stopped being true
// before the first half did. (*Group).propose puts every proposal this package GENERATES through the
// aggregate as a one entry list, which is the sending side of the same rules, so the caller arrived
// from a direction the entry did not anticipate. This list failed until the entry came off, exactly
// as CheckUpdatePathKeyUniqueness's did.
// ValidateCommit and ValSem205ConfirmationTag WERE here and are not any more, which is this list
// expiring by failing for the second time. Both entries said the caller was p7 task 18, the client
// PROCESSING a commit, and both were right about the receiver and wrong about the class: task 13's
// (*Group).CreateCommit puts every commit this package GENERATES through the aggregate and then
// through ValSem205 with the new epoch's confirmation key, which is the sending side of the same
// rules. A commit this build emits and this build refuses is a client publishing a message its own
// package will not process, so the generator running its own door is not a courtesy -- and the
// caller arrived from a direction both entries named the wrong task for.
var rulesThisPackageExportsAndNothingApplies = []string{
	"ValSem300NoTrailingBlankNodes",
}

// exportedRulesOfThisPackage is every exported function of the non test source whose whole answer
// is an error, mapped to the file declaring it.
//
// Derived off the SHAPE and not off the spelling. A function whose only result is an error exists
// to refuse and to do nothing else, which is what a validation rule is; a list of "Check" prefixed
// names would be the enumeration rule 5 is about, and the next rule spelled Verify, Assert or
// Require falls straight out of it. A fence around framing_protect.go would be that same
// enumeration one indirection out, which is the argument the roster above already makes.
func exportedRulesOfThisPackage(sources []refusalSource) map[string]string {
	rules := map[string]string{}
	for _, source := range sources {
		if source.isTest {
			continue
		}
		for _, declaration := range source.parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || !ast.IsExported(function.Name.Name) {
				continue
			}
			if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
				continue
			}
			result := function.Type.Results.List[0]
			if len(result.Names) > 1 {
				continue
			}
			identifier, isIdentifier := result.Type.(*ast.Ident)
			if isIdentifier && identifier.Name == "error" {
				rules[function.Name.Name] = source.path
			}
		}
	}
	return rules
}

// rulesAppliedIn is the rules some other declaration of the non test source mentions, mapped to
// the file that mentions them.
//
// A mention rather than a call, deliberately: a rule handed to something as a value is applied as
// much as one called outright, and under-reporting the unapplied is the safe direction for a list
// that has to be written by hand. A declaration's own name is not among its mentions -- the walk
// reads a function's signature and its body and not its Name -- so a recursive rule does not
// apply itself.
func rulesAppliedIn(sources []refusalSource, rules map[string]string) map[string]string {
	applied := map[string]string{}
	for _, source := range sources {
		if source.isTest {
			continue
		}
		for _, declaration := range source.parsed.file.Decls {
			mentions := map[string]bool{}
			if function, isFunction := declaration.(*ast.FuncDecl); isFunction {
				maps.Copy(mentions, identifiersIn(function.Type))
				if function.Body != nil {
					maps.Copy(mentions, identifiersIn(function.Body))
				}
			} else {
				mentions = identifiersIn(declaration)
			}
			for name := range mentions {
				if _, isRule := rules[name]; !isRule {
					continue
				}
				if _, already := applied[name]; !already {
					applied[name] = source.path
				}
			}
		}
	}
	return applied
}

// TestEveryExportedRuleIsAppliedByThisPackageOrPinnedAsUnwired holds this package to knowing which
// of its own rules nothing runs.
//
// CheckFramedContentContext and CheckSenderLeaf are the reason it exists. They are ValSem002,
// ValSem003 and ValSem004; they are the refusals a receive path cannot verify its way past; and on
// the day they landed nothing in this package called either. The tests above drive them directly,
// every gate in the package passed, and the two receive paths went on accepting a framed content
// from another group, from another epoch, and a member message naming a blanked leaf. The dead code
// gate cannot see it: crypto_test.go's call graph half skips exported names on purpose, because an
// exported declaration with no caller in its own package is what an API entry point looks like --
// and a rule is exactly the exported declaration for which that is not true. A rule nobody runs
// refuses nothing.
//
// So the gap becomes a fact this package asserts rather than one a reader has to notice, and it is
// asserted in both directions, which is the half that matters on the day p8 lands.
func TestEveryExportedRuleIsAppliedByThisPackageOrPinnedAsUnwired(t *testing.T) {
	sources := refusalSourcesOfThisPackage(t)
	rules := exportedRulesOfThisPackage(sources)
	if len(rules) == 0 {
		t.Fatal("the scan derived no exported rule out of this package, so whatever it reports below, it read nothing")
	}
	// an anchor on each half, for the roster's reason: a guard derived from the thing it guards
	// guards nothing. psk.go exports CheckNoDuplicatePsks, whose whole answer is an error, and
	// calls it from its own proposal list check.
	if _, derived := rules["CheckNoDuplicatePsks"]; !derived {
		t.Fatal("psk.go exports CheckNoDuplicatePsks and its whole answer is an error, and the scan did not derive it as a rule, so the class below is not this package's rules")
	}
	applied := rulesAppliedIn(sources, rules)
	if _, isApplied := applied["CheckNoDuplicatePsks"]; !isApplied {
		t.Fatal("psk.go calls CheckNoDuplicatePsks from its own source and the scan did not see it applied, so what it reports below is about the scan and not about this package")
	}
	unapplied := []string{}
	for name := range rules {
		if _, isApplied := applied[name]; !isApplied {
			unapplied = append(unapplied, name)
		}
	}
	slices.Sort(unapplied)
	if !slices.Equal(unapplied, rulesThisPackageExportsAndNothingApplies) {
		t.Errorf("this package exports %d rules and applies none of %v itself; the list it is pinned to is %v. A name that has appeared is a rule this package ships and never runs -- write it there with the reason and the task that wires it, or wire it. A name that has gone is a rule something applies now -- take it off, so the next unwired one cannot hide behind it.",
			len(rules), unapplied, rulesThisPackageExportsAndNothingApplies)
	}
	t.Logf("%d exported rules, %d of them applied by this package's own source",
		len(rules), len(rules)-len(unapplied))
}

// ---------------------------------------------------------------------------
// ValSem005, ValSem007, ValSem008 and ValSem010, moved off the operations they refuse
// ---------------------------------------------------------------------------
//
// Tasks 5 to 11 wrote these four beside the codecs and the operations they refuse, which is
// where each belonged while it was the only statement of its rule. They are here now because
// the roster below is what watches all ten framing refusals at once, and a roster whose subjects
// are spread over three files is a roster whose next reader has to be told where to look. Not one
// line of any of them changed in the move: what a moved test asserts is what it asserted before,
// and a move that also edited would be a rewrite nobody reviewed as one.

func TestAuthenticatedContentRefusesForgedSignature(t *testing.T) {
	signed := framingSignedMemberMessage(t)

	tampered := *signed.authContent
	tampered.Auth.Signature = append([]byte(nil), signed.authContent.Auth.Signature...)
	tampered.Auth.Signature[0] ^= 0x01
	if err := VerifyAuthenticatedContent(signed.crypto, signed.pub, &tampered, signed.groupContext); !errors.Is(err, errBadSignature) {
		t.Fatalf("flipped signature: got %v, want the ValSem010 sentinel", err)
	}

	empty := *signed.authContent
	empty.Auth.Signature = nil
	if err := VerifyAuthenticatedContent(signed.crypto, signed.pub, &empty, signed.groupContext); !errors.Is(err, errBadSignature) {
		t.Fatalf("empty signature: got %v, want the ValSem010 sentinel", err)
	}

	otherContext := append([]byte(nil), signed.groupContext...)
	otherContext[0] ^= 0xff
	if err := VerifyAuthenticatedContent(signed.crypto, signed.pub, signed.authContent, otherContext); !errors.Is(err, errBadSignature) {
		t.Fatalf("wrong group context: got %v, want the ValSem010 sentinel", err)
	}

	rewired := *signed.authContent
	rewired.WireFormat = WireFormatPublicMessage
	if err := VerifyAuthenticatedContent(signed.crypto, signed.pub, &rewired, signed.groupContext); !errors.Is(err, errBadSignature) {
		t.Fatalf("rewired wire format: got %v, want the ValSem010 sentinel", err)
	}

	// and the wrong key, which is the one refusal above that is not about the message
	_, otherPub, err := signed.crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("a second key pair: %v", err)
	}
	if err := VerifyAuthenticatedContent(signed.crypto, otherPub, signed.authContent, signed.groupContext); !errors.Is(err, errBadSignature) {
		t.Fatalf("another member's key: got %v, want the ValSem010 sentinel", err)
	}
}

// TestPublicMessageRefusesApplicationContent is ValSem005, in both directions.
//
// The receive half is the one that has to exist. A sender's guard protects nobody from a peer that
// does not run this code, and the message it refuses is the user's own plaintext travelling in an
// authenticated but unencrypted frame -- so a receiver that accepted one would be handing an
// application message up to the caller having only checked that somebody signed it.
func TestPublicMessageRefusesApplicationContent(t *testing.T) {
	crypto := newTestCrypto(t)
	priv, pub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	groupContext := framingTestGroupContext(t)
	membershipKey := bytes.Repeat([]byte{0x5a}, crypto.HashSize())

	authContent, err := SignAuthenticatedContent(crypto, priv, WireFormatPublicMessage,
		framingTestMemberContent(), groupContext)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err = SealPublicMessage(crypto, membershipKey, authContent, groupContext); !errors.Is(err, errApplicationMustBeCiphertext) {
		t.Fatalf("seal: got %v, want errApplicationMustBeCiphertext", err)
	}

	// a hostile peer that hands us one anyway is refused on receipt, and it is refused with a
	// tag that WOULD have verified -- so what refuses it is ValSem005 and not the tag rule
	// standing in front of it.
	hostile := &PublicMessage{Content: *framingTestMemberContent(), Auth: authContent.Auth}
	tag, err := ComputeMembershipTag(crypto, membershipKey, hostile.AuthenticatedContent(), groupContext)
	if err != nil {
		t.Fatalf("the tag the hostile message carries: %v", err)
	}
	hostile.MembershipTag = tag
	_, err = OpenPublicMessage(crypto, membershipKey, hostile, StaticSignatureKey(pub), groupContext)
	if !errors.Is(err, errApplicationMustBeCiphertext) {
		t.Fatalf("open: got %v, want errApplicationMustBeCiphertext", err)
	}
}

// TestPublicMessageRefusesMissingMembershipTag is ValSem007 on the open path, over every spelling
// of an empty byte run for emptyByteSpellings' reason.
//
// It is what says the open CHECKS the tag at all for a member sender. An open that never reached
// verifyMembershipTag would answer nil here, having verified the signature of a message whose only
// statement that its sender is inside this group is absent.
func TestPublicMessageRefusesMissingMembershipTag(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	for _, empty := range emptyByteSpellings() {
		stripped := *sealed.message
		stripped.MembershipTag = empty.value
		_, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &stripped,
			StaticSignatureKey(sealed.pub), sealed.groupContext)
		if !errors.Is(err, errMissingMembershipTag) {
			t.Errorf("a tag that is %s: got %v, want errMissingMembershipTag", empty.what, err)
		}
	}
}

// TestPublicMessageRefusesForgedMembershipTag is ValSem008 on the open path, over every flipped bit
// of the tag and every key but the right one, rather than over the one corruption somebody thought
// of.
//
// The derivation is rule 5's and this project has the measurement behind it: a tag verifier that
// read the first byte of a thirty two byte tag passed a test that flipped bit zero of byte zero,
// and a verifier that accepted every truncation passed a suite with no length case in it.
func TestPublicMessageRefusesForgedMembershipTag(t *testing.T) {
	sealed := framingSealedMemberProposal(t)
	if len(sealed.message.MembershipTag) != sealed.crypto.HashSize() {
		t.Fatalf("the sealed tag is %d bytes, want the provider's %d",
			len(sealed.message.MembershipTag), sealed.crypto.HashSize())
	}
	for at := 0; at < len(sealed.message.MembershipTag); at += 1 {
		for bit := 0; bit < 8; bit += 1 {
			tampered := *sealed.message
			tampered.MembershipTag = append([]byte(nil), sealed.message.MembershipTag...)
			tampered.MembershipTag[at] ^= 1 << uint(bit)
			_, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &tampered,
				StaticSignatureKey(sealed.pub), sealed.groupContext)
			if !errors.Is(err, errBadMembershipTag) {
				t.Fatalf("bit %d of tag octet %d flipped: got %v, want errBadMembershipTag", bit, at, err)
			}
		}
	}
	// and every truncation and extension of it, which is the class a prefix comparison accepts
	for n := 0; n <= 2*len(sealed.message.MembershipTag); n += 1 {
		if n == len(sealed.message.MembershipTag) {
			continue
		}
		tampered := *sealed.message
		resized := make([]byte, n)
		copy(resized, sealed.message.MembershipTag)
		tampered.MembershipTag = resized
		want := errBadMembershipTag
		if n == 0 {
			want = errMissingMembershipTag
		}
		_, err := OpenPublicMessage(sealed.crypto, sealed.membershipKey, &tampered,
			StaticSignatureKey(sealed.pub), sealed.groupContext)
		if !errors.Is(err, want) {
			t.Fatalf("a %d byte tag: got %v, want %v", n, err, want)
		}
	}
	// and the tag taken under a key that is live and is not this epoch's
	wrongKey := bytes.Repeat([]byte{0x5b}, sealed.crypto.HashSize())
	_, err := OpenPublicMessage(sealed.crypto, wrongKey, sealed.message,
		StaticSignatureKey(sealed.pub), sealed.groupContext)
	if !errors.Is(err, errBadMembershipTag) {
		t.Fatalf("wrong key: got %v, want errBadMembershipTag", err)
	}
}
