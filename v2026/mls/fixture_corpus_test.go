// The commit and proposal fixture corpora, and the gate that measures BOTH of them.
//
// THREE CONSECUTIVE ROUNDS OF THE COMMIT DOOR HAD ONE ROOT CAUSE AND IT WAS NOT IN THE DOOR. "Every
// fixture carries exactly one Update" made every loop narrowable to element zero; "every fixture
// makes PostTree a Clone of PreTree" made three tree reads swappable; "every fixture puts the
// committer at leaf 0" made `Sender: self.Committer` indistinguishable from the constant
// `LeafIndex(0)`. Each round repaired the comparison it was handed and left the corpus that hid it,
// so the next round found the next constant.
//
// A CORPUS THAT DRIFTS BACK TO CONSTANTS IS THE DEFECT, so it is measured here rather than
// remembered. What the gate below asserts is one property stated once: no dimension of a validation
// input is the SAME VALUE across the whole corpus. A dimension that is constant is a dimension on
// which the production code and that constant are the same program, which is what "the fixtures
// cannot see it" means and is the only thing all three rounds had in common.
//
// THE FOURTH ROUND WAS THIS FILE'S OWN. The gate derived its walk and NAMED ITS SCOPE -- it walked
// for values of two types, LeafIndex and *RatchetTree, and was blind to every other type an input
// carries. Owner-verified: a collapsed `Generation uint32` added to CommitValidationInput left the
// gate passing and logging "11 fixtures, 5 leaf dimensions"; the identical field typed LeafIndex
// fired. That is ledger 21 applied to types -- derive the walk, enumerate the scope -- and the
// scope was where the survivors were. Five reads of the proposal door were measured as
// indistinguishable from build-time constants through it: `in.Context.GroupId`, both halves of
// ValSem105's suite-and-version comparison, the ciphersuite ValSem105 hands KeyPackage.Validate,
// and the clock it hands with it. So the walk below carries NO type at all. It records whatever it
// reaches, by the path that reached it, rendered canonically.
//
// AND IT MEASURES BOTH DOORS. There was no corpus for ProposalValidationInput -- zero references to
// the type in this file, across fifty call sites of testValidationInput, forty-eight of which
// passed LeafIndex(0) -- so the door that reads the group id, the ciphersuite, the version and the
// clock was the one door nothing held to being able to see them. All five survivors lived there.
// The corpora are a REGISTRY keyed by the input type and joined to the types the package's own
// source declares, in both directions, because "two tests, one of which gets deleted" is the shape
// that defect takes next.
//
// WHY THE WALK STOPS WHERE IT DOES, because a bound that is not argued for is the next round's
// finding. It descends through containers -- pointers, interfaces, slices, arrays and maps cost
// nothing -- and spends one HOP per struct field, to a depth of corpusDimensionHops. That is the
// granularity commitInputSourceClass already derives the door's own source class at ("one level of
// expansion for List and for Commit and no deeper, because that is where the granularity stops
// mattering"), and it is where the input's own fields end and the CONTENTS of the messages it
// carries begin. Measured, unbounded is not the stronger choice but an unsatisfiable one: at full
// depth the corpus is asked to separate `List[].Proposal.ExternalInit`, `.PreSharedKey`, `.ReInit`
// and the unknown-type pair, which this profile refuses to populate at all -- checkProposalProfile
// rejects every one of those types before any rule sees the list -- so the claim could never go
// green whatever anybody wrote. A constant deeper than the bound is not invisible; it is folded
// into the rendering of its ancestor, so it is caught whenever that ancestor is constant too. What
// is given up is the ability to demand a DEEP field vary while its parent already does, and for
// the one type these doors decide off -- a leaf index -- that is bought back by the unbounded scan
// the octet claim runs.
package mls

import (
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// the second value of every dimension the corpus used to hold at one
// ---------------------------------------------------------------------------

// testSecondGroupId is the group id the fixtures that are NOT in testValidationGroupId's group
// announce.
//
// A SECOND SPELLING IS THE WHOLE POINT OF IT. An update leaf's LeafNodeTBS covers the group id, so
// validate_proposals.go hands in.Context.GroupId to the leaf validator to rebuild the preimage the
// leaf signed; over a corpus that runs in one group that read and the literal []byte("group") are
// the same program, and the cross-group replay the function's own header exists to close is
// untested. A fixture in this group signs its update leaves under THIS id, so the literal rebuilds
// the wrong preimage and the signature stops verifying.
func testSecondGroupId() []byte {
	return []byte("second-group")
}

// testSecondEpoch is the epoch those fixtures run in, and it is neither the default 1 nor 0.
const testSecondEpoch = uint64(9)

// testUnimplementedProtocolVersion is a protocol version this build does not implement.
//
// PRIVATE USE RATHER THAN INVENTED. RFC 9420's protocol version registry reserves the top of the
// range for private use, so this is a code point a deployment may legitimately carry and this build
// legitimately refuses -- unlike the zero value, which is "the caller forgot" and would make every
// refusal below ambiguous between a version rule and a missing field.
//
// It exists because ProtocolVersionMls10 is the only version this build accepts, so a corpus that
// announces only that cannot tell ValSem105's `kp.Version != in.Context.Version` from
// `kp.Version != ProtocolVersionMls10`. A group context announcing this one, with an Add carrying
// an ordinary mls10 key package, tells them apart in the only direction that is decidable: the
// honest comparison refuses the Add and the literal accepts it.
const testUnimplementedProtocolVersion = ProtocolVersion(0xFFFF)

// testFixedClock is a clock that is not the wall clock, and it is far enough from it that a
// lifetime decided under one is decided the other way under the other.
//
// THE CORPUS USED TO HOLD ONE CLOCK AND IT WAS time.Now(). in.Now is read in exactly two production
// places, and validate_commit_test.go says outright that the commit aggregate reads it by nothing
// -- so the only two tests that moved the clock were on the door that ignores it, and the door that
// reads it, ValSem105's `kp.Validate(in.Crypto, in.Context.CipherSuite, in.Now)`, saw one clock
// that was always approximately time.Now(). Substituting time.Now() for the field was therefore
// invisible. A key package this package mints is valid for about ninety days from the moment it is
// built, so a clock years past that decides its lifetime the other way.
func testFixedClock() time.Time {
	return time.Date(2031, time.March, 4, 5, 6, 7, 0, time.UTC)
}

// testOtherSuite is the registered ciphersuite this provider does NOT run, read off the registry
// rather than written down.
//
// READ OFF Suites() so that a third registered suite does not leave this pointing at a code point
// that is no longer "the other one", and so that a build with one registered suite says so here
// rather than silently measuring nothing.
func testOtherSuite(t *testing.T, crypto CryptoProvider) CipherSuite {
	t.Helper()
	for _, suite := range Suites() {
		if suite != crypto.Suite() {
			return suite
		}
	}
	t.Fatalf("only %d ciphersuite is registered, so no fixture can announce a second one and every ciphersuite read of these doors is the same program as that constant",
		len(Suites()))
	return CipherSuite(0)
}

// testOtherSuiteCrypto is a provider for that suite.
//
// A SECOND PROVIDER AND NOT A SECOND CODE POINT ON THE FIRST ONE'S INPUTS. NewKeyPackage refuses to
// mint a key package for a suite its provider does not run, and (*LeafNode).Validate judges a
// leaf's capabilities against the ciphersuite it is handed, so a fixture that merely relabelled its
// context would be a group whose members cannot do its crypto -- refused for a reason that is the
// relabelling rather than the rule under test.
func testOtherSuiteCrypto(t *testing.T, crypto CryptoProvider) CryptoProvider {
	t.Helper()
	other, err := NewCryptoProvider(testOtherSuite(t, crypto))
	if err != nil {
		t.Fatalf("NewCryptoProvider for the second registered suite: %v", err)
	}
	return other
}

// testCapabilitiesNaming is testCapabilities with the ciphersuite vector named by the caller.
func testCapabilitiesNaming(suites ...CipherSuite) Capabilities {
	named := testCapabilities()
	named.CipherSuites = slices.Clone(suites)
	return named
}

// ---------------------------------------------------------------------------
// the commit fixtures the corpus was missing
// ---------------------------------------------------------------------------

// testCommitInputOverTheTreeItsProposalsBuild is the fixture whose two trees are not one tree.
//
// testCommitInput hands PostTree a CLONE of PreTree, which is what every fixture in this package
// took, and a clone is equal to what it was cloned from: a rule reading either answers the same
// thing, so three tree reads of validate_commit.go could be pointed at the other tree with the
// whole suite green. Here the post tree is what ApplyProposals actually builds from the list, so
// the two answer differently -- the removes truncate the group -- and a rule stated over the wrong
// one is a rule that reports on an epoch this commit is not in.
func testCommitInputOverTheTreeItsProposalsBuild(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	testCommitProposals(t, in, testRemoveOf(LeafIndex(3)), testRemoveOf(LeafIndex(2)))
	applied, err := ApplyProposals(in.PreTree, in.Context, in.Own, in.List)
	if err != nil {
		t.Fatalf("ApplyProposals to build the post tree: %v", err)
	}
	in.PostTree = applied.Tree
	// the path is fitted against the POST tree, which is the tree ValSem202 is stated over, so it
	// has to be re-fitted once the removes have shortened the committer's filtered direct path
	testFitCommitPath(t, crypto, in, members[in.Committer])
	if in.PostTree.LeafCount() >= in.PreTree.LeafCount() {
		t.Fatalf("the post tree is %d leaves wide and the pre tree %d; this fixture exists to make the two answer differently",
			in.PostTree.LeafCount(), in.PreTree.LeafCount())
	}
	return in
}

// testWideCommitterLeaf is the leaf the wide fixture commits from, and testWideOwnLeaf is the leaf
// judging it. Both are above what one octet holds, which is the whole point of them.
//
// A COMPARISON NARROWER THAN A LeafIndex IS EXACT OVER EVERY GROUP WHOSE LEAVES FIT IN ONE OCTET,
// and every group in this package's fixtures did: measured, the corpus held exactly one leaf index
// above 255 and it was a value a probe wrote by hand. So the Sender comparator could be truncated
// to its low octet with the entire suite green. A group whose members really do sit above 255 is
// what makes that truncation a wrong answer rather than an unexercised one.
const (
	testWideCommitterLeaf = LeafIndex(258)
	testWideOwnLeaf       = LeafIndex(257)
)

// testWideGroupLeaves is the group's width, and it is a POWER OF TWO on purpose: a ratchet tree
// grows by doubling, so a group of any other size ends in blank nodes and ValSem300 refuses the
// post tree it exports.
const testWideGroupLeaves = 512

// testWideGroupNames is that group's membership, one name per leaf.
func testWideGroupNames() []string {
	names := make([]string, 0, testWideGroupLeaves)
	for i := 0; i < testWideGroupLeaves; i += 1 {
		names = append(names, fmt.Sprintf("wide%d", i))
	}
	return names
}

// testWideCommitInput is testFullCommitInput over a group wide enough that its leaf indices do not
// fit in one octet.
func testWideCommitInput(t *testing.T, crypto CryptoProvider) *CommitValidationInput {
	t.Helper()
	tree, members := testTreeWith(t, crypto, testWideGroupNames()...)
	in := testCommitInput(t, crypto, tree, &ProposalList{}, &Commit{})
	in.Committer = testWideCommitterLeaf
	in.Own = testWideOwnLeaf
	// the inline entries are attributed to THIS fixture's committer rather than to
	// testCommitterLeaf, which is (*ProposalCache).Resolve's rule: a by-value entry of a commit's
	// vector resolves to whoever sent the commit.
	removes := []CachedProposal{testRemoveOf(LeafIndex(1)), testRemoveOf(testWideOwnLeaf - 1)}
	for i := range removes {
		removes[i].Sender = in.Committer
	}
	testCommitProposals(t, in, removes...)
	testFitCommitPath(t, crypto, in, members[in.Committer])
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the wide fixture: %v; a fixture no door accepts measures nothing about the doors",
			failure)
	}
	return in
}

// testCommitInputInASecondGroupAtALaterEpoch is the fixture that is not in the default group, not
// in the default epoch, not on the wall clock, and not empty in the fields the default leaves nil.
//
// ONE FIXTURE FOR SEVERAL DIMENSIONS, and that is deliberate rather than lazy. The claim the gate
// states is that no dimension is ONE VALUE across the corpus, which one differing fixture settles
// for each; what a fixture per dimension would buy is a smaller blast radius when a door starts
// refusing, and what it would cost is ten near-identical commits somebody has to keep in step. The
// dimensions bundled here are the ones the commit door reads by NOTHING -- validate_commit_test.go's
// establishment table says so for the clock, and the group id, the epoch and the confirmation
// triple are read only by rules the aggregate does not run -- so a refusal here can only come from
// the two that ARE read, the group extensions.
//
// THE CONFIRMATION TRIPLE IS CONSISTENT rather than three unrelated slices. ValSem205 is the one
// rule of this file the aggregate deliberately does not run, and until this fixture existed there
// was no input in the corpus it could even be asked of: every fixture left all three fields nil, so
// "the tag verifies" and "the tag is missing" were the same input.
func testCommitInputInASecondGroupAtALaterEpoch(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol", "dave")
	in := testCommitInput(t, crypto, tree, &ProposalList{}, &Commit{})
	in.Context = &GroupContext{
		Version:                 ProtocolVersionMls10,
		CipherSuite:             crypto.Suite(),
		GroupId:                 testSecondGroupId(),
		Epoch:                   testSecondEpoch,
		ConfirmedTranscriptHash: crypto.Hash([]byte("the second group's confirmed transcript")),
		Extensions:              testCommitInstalledExtensions(),
	}
	in.Extensions = slices.Clone(testCommitInstalledExtensions())
	in.Now = testFixedClock()
	in.ConfirmationKey = crypto.Random(crypto.HashSize())
	in.ConfirmedHash = crypto.Hash([]byte("the second group's confirmed hash"))
	in.ConfirmationTag = crypto.Mac(in.ConfirmationKey, in.ConfirmedHash)
	// AND ITS CACHE IS STILL BOUND TO THE EPOCH THIS GROUP LEFT, which is the epoch pair read
	// from the binding's side. It is bound to THIS group at an earlier epoch rather than to
	// another group, so the epoch is the only one of the two facts that disagrees here -- the
	// group is the only one that disagrees in testCommitWhosePendingCacheBelongsToAnotherGroup,
	// and see that fixture for what one axis per fixture does and does not buy.
	//
	// NOTHING NAMES THE CACHE, so this stays an accepted commit: entryTheCommitNames consults it
	// only for a by-reference entry and both of this fixture's proposals are carried by value.
	// A door that came to refuse a stale cache outright would fail this row rather than pass it
	// quietly, which is the right way round for a fact nobody has stated as a rule.
	stale := *in.Context
	stale.Epoch = 1
	in.Pending = testCacheAt(t, &stale)
	testCommitProposals(t, in, testRemoveOf(LeafIndex(3)), testRemoveOf(LeafIndex(2)))
	testFitCommitPath(t, crypto, in, members[in.Committer])
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the second group's commit: %v", failure)
	}
	// the triple is a triple and not three fields, so the rule the aggregate does not run has an
	// input it accepts
	if failure := ValSem205ConfirmationTag(in); failure != nil {
		t.Fatalf("ValSem205 refused this fixture's own confirmation tag: %v", failure)
	}
	return in
}

// testCommitInputUnderTheOtherSuite is a commit in a group that runs the registry's OTHER suite.
//
// THE PROVIDER IS THE OTHER SUITE'S TOO, which is what makes this a group rather than a relabelled
// context: every key in the tree, in the path and in the proposals is drawn through it. Until it
// existed the whole corpus ran one suite, so `in.Context.CipherSuite` and the default code point
// were the same program at every door that reads one.
func testCommitInputUnderTheOtherSuite(t *testing.T, crypto CryptoProvider) *CommitValidationInput {
	t.Helper()
	other := testOtherSuiteCrypto(t, crypto)
	tree, members := testTreeWith(t, other, "alice", "bob", "carol", "dave")
	in := testCommitInput(t, other, tree, &ProposalList{}, &Commit{})
	in.Context.CipherSuite = other.Suite()
	testFitCommitPath(t, other, in, members[in.Committer])
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the other suite's commit: %v", failure)
	}
	return in
}

// testCommitInputAnnouncingAnUnimplementedVersion is a commit whose group context announces a
// protocol version this build does not implement.
//
// AND THE COMMIT DOOR ACCEPTS IT, which is asserted here rather than left as a surprise. That is a
// finding about the door and not about this fixture: no rule ValidateCommit runs reads
// Context.Version at all, and neither does any rule of section 12.2 except ValSem105's comparison
// against an Add's key package -- so a commit arriving in a group whose context claims a version
// this build cannot speak is judged in full and accepted. welcome.go refuses exactly that shape at
// the join door (`self.GroupContext.Version != ProtocolVersionMls10`); the commit door has no
// equivalent. The day one is added this fixture fails, and the answer is to move it to a refusal
// rather than to soften the claim.
func testCommitInputAnnouncingAnUnimplementedVersion(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, _ := testFullCommitInput(t, crypto)
	in.Context.Version = testUnimplementedProtocolVersion
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused a commit whose context announces version %#04x: %v. That is a rule this door did not have when this fixture was written; move the fixture to the refusing side rather than narrowing the corpus",
			uint16(testUnimplementedProtocolVersion), failure)
	}
	return in
}

// ---------------------------------------------------------------------------
// the proposal fixtures, which is a corpus that did not exist
// ---------------------------------------------------------------------------

// testValidationInputInASecondGroupAtALaterEpoch is the section 12.2 input that is not in
// testValidationGroupId's group.
//
// ITS UPDATE LEAF IS SIGNED UNDER THIS GROUP'S ID, which is the whole of what it measures.
// validateUpdateLeafNodeIsValidForAnUpdate hands in.Context.GroupId to the leaf validator, which
// rebuilds the LeafNodeTBS the leaf signed; over a corpus in one group that read is the literal
// []byte("group"), and a leaf signed in another group -- a replayed update, which is the fault the
// binding exists to refuse -- would be admitted by the literal and is refused by the read.
//
// IT ALSO CARRIES THE INPUT'S OWN Extensions AND A TREE HASH, because the corpus held both at one
// value: nil. effectiveExtensions prefers the explicit vector over the group's own, so a corpus
// that never sets it drives only two of that function's three arms.
func testValidationInputInASecondGroupAtALaterEpoch(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol", "dave")
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("the second group's tree hash: %v", err)
	}
	leaf, _ := testUpdateLeafNode(t, crypto, members[2], testSecondGroupId(), LeafIndex(2))
	return &ProposalValidationInput{
		Crypto: crypto,
		Tree:   tree,
		Context: &GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             crypto.Suite(),
			GroupId:                 testSecondGroupId(),
			Epoch:                   testSecondEpoch,
			TreeHash:                treeHash,
			ConfirmedTranscriptHash: crypto.Hash([]byte("the second group's confirmed transcript")),
		},
		Extensions: slices.Clone(testCommitInstalledExtensions()),
		Committer:  testCommitterLeaf,
		List: testProposalList(t, testUpdateOf(LeafIndex(2), leaf),
			testRemoveOf(LeafIndex(3))),
		Now: time.Now(),
	}
}

// testValidationInputUnderTheOtherSuite is a section 12.2 input for a group running the registry's
// OTHER suite, and it is what tells three ciphersuite reads of validate_proposals.go from the
// default code point.
//
// THREE, and each needs a different half of this fixture. ValSem105 compares an Add's key package
// ciphersuite against the group's, so the Add is minted under the other suite. ValSem105 then hands
// that same field to KeyPackage.Validate, which compares it again. And
// validateUpdateLeafNodeIsValidForAnUpdate hands it to the leaf validator, which asks whether the
// UPDATE leaf's capabilities list it -- so this fixture's update leaf advertises the group's suite
// and only that one. Every other fixture leaf in this package advertises Suites(), which is both
// registered code points, and a vector that lists both answers "supported" for either constant.
func testValidationInputUnderTheOtherSuite(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	other := testOtherSuiteCrypto(t, crypto)
	tree, members := testTreeWith(t, other, "alice", "bob", "carol", "dave")
	kp, _, _ := testKeyPackage(t, other, testIdentity(t, other, "erin"))
	leaf, _ := testUpdateLeafNodeNaming(t, other, members[2], testValidationGroupId(), LeafIndex(2),
		testCapabilitiesNaming(other.Suite()))
	return &ProposalValidationInput{
		Crypto: other,
		Tree:   tree,
		Context: &GroupContext{
			Version:     ProtocolVersionMls10,
			CipherSuite: other.Suite(),
			GroupId:     testValidationGroupId(),
			Epoch:       1,
			Extensions:  testCommitInstalledExtensions(),
		},
		Committer: testCommitterLeaf,
		List:      testProposalList(t, testAddOf(kp), testUpdateOf(LeafIndex(2), leaf)),
		Now:       time.Now(),
	}
}

// testValidationInputAnnouncingAnUnimplementedVersion is the input ValSem105's VERSION clause is
// decidable over, and it is a refusal.
//
// The suite clause of that comparison is separable by an accepted input -- a group running the
// other registered suite, with an Add that names it -- but the version clause is not, because
// KeyPackage.Validate refuses every version but mls10 outright. So the only input that tells
// `kp.Version != in.Context.Version` from `kp.Version != ProtocolVersionMls10` is one where the
// GROUP announces the other version and the key package is ordinary: the read refuses it and the
// literal admits it.
func testValidationInputAnnouncingAnUnimplementedVersion(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	return &ProposalValidationInput{
		Crypto: crypto,
		Tree:   tree,
		Context: &GroupContext{
			Version:     testUnimplementedProtocolVersion,
			CipherSuite: crypto.Suite(),
			GroupId:     testValidationGroupId(),
			Epoch:       1,
		},
		Committer: testCommitterLeaf,
		List:      testProposalList(t, testAddOf(kp)),
		Now:       time.Now(),
	}
}

// testValidationInputOnAClockPastTheAddsLifetime is the input in.Now is decidable over.
//
// ValSem105 hands in.Now to KeyPackage.Validate, which clamps it to milliseconds and checks the
// added leaf's lifetime against it. Every other input of this package carries time.Now(), so the
// field and a call to time.Now() answer the same verdict for every key package the fixtures mint --
// which is what made substituting the call for the field invisible. This one is years past the
// lifetime this package's key packages carry, so the field refuses and the call accepts.
func testValidationInputOnAClockPastTheAddsLifetime(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	in := testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testAddOf(kp)))
	in.Now = testFixedClock()
	if expires := time.Unix(int64(kp.LeafNode.Lifetime.NotAfter), 0); !in.Now.After(expires) {
		t.Fatalf("this fixture's clock is %s and the added leaf is valid until %s; the fixture exists to be past it",
			in.Now, expires)
	}
	return in
}

// testWideValidationInput is the section 12.2 input over a group whose leaf indices do not fit in
// one octet, and it is also the one that carries an entry BY REFERENCE.
//
// The width is testWideCommitInput's argument, restated at the door next to it: every leaf index
// comparison of validate_proposals.go -- a remove's target, an update's sender, the committer --
// is exact over every group that fits in one octet, and every proposal fixture of this package
// fitted. The reference is a second dimension the corpus held at one value: every entry of every
// proposal list here was carried by value, so `ByValue` was the constant true and `Ref` the
// constant nil.
func testWideValidationInput(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
	t.Helper()
	tree, members := testTreeWith(t, crypto, testWideGroupNames()...)
	update, _ := testUpdateProposalOf(t, crypto, members[testWideOwnLeaf], testWideOwnLeaf)
	held := testCachedRemoveOf(t, crypto, testCache(t), testWideOwnLeaf-1)
	in := testValidationInput(t, crypto, tree, testWideCommitterLeaf,
		testProposalList(t, update, held))
	if failure := ValidateProposalList(in); failure != nil {
		t.Fatalf("ValidateProposalList refused the wide fixture: %v; a fixture no door accepts measures nothing about the doors",
			failure)
	}
	return in
}

// ---------------------------------------------------------------------------
// the fixtures the RELATION claim was missing
// ---------------------------------------------------------------------------
//
// EVERY FIXTURE BELOW EXISTS FOR A PAIR AND NOT FOR A FIELD. fixture_relations_test.go derives, off
// this package's own source, every pair of input paths a rule compares, and the corpus owes each
// pair a fixture where the two are equal and one where they are not. Ten of the twenty pairs had
// only one of the two, and two had neither because no fixture ever filled both sides.
//
// MOST OF THEM ARE REFUSALS AND THAT IS WHAT THE ROW'S SENTINEL IS FOR. A rule that refuses on
// equality -- ValSem104, ValSem111, ValSem204, ValSem206, validateCommitterIsNotRemoved,
// validateUpdateChangesTheEncryptionKey -- has no ACCEPTED input in which its two sides agree, so
// the only witness that tells it from `false` is an input the door turns away. Until this round the
// commit corpus had no way to hold one: its rows carried no verdict at all.

// testCommitTheCommitterJudgesItselfFromABlankSibling is the commit whose judge IS its committer,
// standing where the committer's filtered direct path is EMPTY.
//
// IT IS THE ONE INPUT ValSem203PathDecrypt's FIRST LINE IS DECIDABLE OVER, and the last round
// measured what its absence cost: `if in.Own == in.Committer` could be replaced by
// `if in.Own == LeafIndex(0)` -- true in every fixture, since Own was 0 and Committer 1 throughout
// -- or by `if false` outright, and the whole suite stayed green. Both are real behaviour changes
// and neither was observable, because no fixture ever put the two leaves on the same number.
//
// THE BLANK SIBLING IS WHAT MAKES THE EARLY RETURN MATTER. Own == Committer alone is not enough: in
// any group whose committer has a non-empty filtered direct path, the loop the early return skips
// would compare that path against ITSELF and find every node shared, so the mutant and the honest
// rule agree anyway. Here the group is two leaves and the committer's own commit removes the other
// one, so the sibling subtree is blank, the filtered direct path is empty, and a run that reaches
// the loop finds no shared node and REFUSES the committer's own commit. That is the difference the
// early return is there to make, and this is the input that makes it.
func testCommitTheCommitterJudgesItselfFromABlankSibling(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob")
	in := testCommitInput(t, crypto, tree, &ProposalList{}, &Commit{})
	in.Own = in.Committer
	testCommitProposals(t, in, testRemoveOf(LeafIndex(0)))
	applied, err := ApplyProposals(in.PreTree, in.Context, in.Own, in.List)
	if err != nil {
		t.Fatalf("ApplyProposals to blank the committer's sibling: %v", err)
	}
	in.PostTree = applied.Tree
	testFitCommitPath(t, crypto, in, members[in.Committer])
	filtered, err := in.PostTree.FilteredDirectPath(in.Committer)
	if err != nil {
		t.Fatalf("FilteredDirectPath(%d) over the post tree: %v", in.Committer, err)
	}
	if len(filtered) != 0 {
		t.Fatalf("the committer's filtered direct path in the post tree is %v and this fixture exists to make it empty; without that, skipping the early return of ValSem203 changes no verdict",
			filtered)
	}
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the committer's own commit: %v", failure)
	}
	return in
}

// testCommitTheCommitterJudgesItselfFromLeafZeroWithABlankSibling is that same input one leaf over,
// and it is a second fixture rather than an edit of the first for the every-constant reason.
//
// ONE FIXTURE OF THIS SHAPE IS SEPARABLE FROM ONE CONSTANT. The early return is decidable only
// where the committer's own filtered direct path is EMPTY, which is the shape above and nothing
// else in the corpus -- so with a single such fixture, `in.Own == LeafIndex(<that fixture's leaf>)`
// and `in.Own == in.Committer` answer alike over every input here. Measured on this tree: with only
// the leaf one fixture, replacing the comparison with `in.Own == LeafIndex(1)` left the whole
// ./mls/... and ./message/... run green -- which is the round before this one repeating itself at
// LeafIndex(1) instead of LeafIndex(0). Two of them, at two leaves, is what no single constant
// survives: each is accepted by the honest rule and refused by the constant that names the other.
func testCommitTheCommitterJudgesItselfFromLeafZeroWithABlankSibling(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob")
	in := testCommitInput(t, crypto, tree, &ProposalList{}, &Commit{})
	in.Committer = testCommitterAtLeafZero
	in.Own = in.Committer
	// a by-value entry resolves to whoever sent the commit, which here is leaf zero rather than
	// the leaf testRemoveOf names
	removes := []CachedProposal{testRemoveOf(testCommitterLeaf)}
	removes[0].Sender = in.Committer
	testCommitProposals(t, in, removes...)
	applied, err := ApplyProposals(in.PreTree, in.Context, in.Own, in.List)
	if err != nil {
		t.Fatalf("ApplyProposals to blank the committer's sibling: %v", err)
	}
	in.PostTree = applied.Tree
	testFitCommitPath(t, crypto, in, members[in.Committer])
	filtered, err := in.PostTree.FilteredDirectPath(in.Committer)
	if err != nil {
		t.Fatalf("FilteredDirectPath(%d) over the post tree: %v", in.Committer, err)
	}
	if len(filtered) != 0 {
		t.Fatalf("the committer's filtered direct path in the post tree is %v and this fixture exists to make it empty; without that, skipping the early return of ValSem203 changes no verdict",
			filtered)
	}
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused leaf zero's own commit: %v", failure)
	}
	return in
}

// testCommitAnnouncingExtensionsItDoesNotInstall is the commit whose announced extension set and
// whose installed one agree at the ends and disagree in the middle.
//
// IT IS THE ONLY INPUT THE EXTENSION JOIN IS DECIDABLE OVER, which the last round measured:
// `if self.Extensions != nil` could be replaced by `if false` -- deleting the join outright -- with
// the whole suite green, because every fixture that filled Extensions filled it with the very
// vector the join compares it against. A join whose two sides are equal in every input is a join
// nothing can tell from its own absence.
//
// NOT THE FIRST ENTRY, for testCommitInstalledExtensions' own reason: a comparison narrowed to
// entry zero, or one that stops at the first agreement, is told apart from the whole walk only by a
// disagreement that has agreement in front of it. Entry zero therefore agrees, and this fixture is
// also the corpus's witness that the announced set and the installed set CAN agree entry by entry.
//
// THE DISAGREEMENT IS A SWAP OF THE BODIES AND THE TYPES ARE LEFT ALONE, and the argument that used
// to stand here said the opposite of what it should have. It read "rewriting one entry's body
// leaves every type equal, so the type comparison would still be indistinguishable from one of
// either side against itself; reordering the tail moves both" -- and moving both is exactly the
// shape that leaves NEITHER clause decidable. The join has a type clause and a data clause and they
// answer one sentinel, so an input whose two pairs differ TOGETHER is refused by either clause
// alone: delete the data comparison and the swap is still caught by the type comparison, delete the
// type comparison and it is still caught by the data one. What makes the DATA clause decidable is a
// fixture whose types agree and whose bodies do not, which is a swap of the tail's bodies; the type
// clause gets the fixture below. Contrast ValSem206, whose three clauses each own a fixture and
// whose mutations therefore die in this gate's own driving test.
func testCommitAnnouncingAnExtensionBodyItDoesNotInstall(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	in.Context.Extensions = testCommitInstalledExtensions()
	testCommitProposals(t, in, testGceOf(testCommitInstalledExtensions()...))
	announced := slices.Clone(testCommitInstalledExtensions())
	announced[1].ExtensionData, announced[2].ExtensionData =
		announced[2].ExtensionData, announced[1].ExtensionData
	in.Extensions = announced
	testFitCommitPath(t, crypto, in, members[in.Committer])
	return in
}

// testCommitAnnouncingAnExtensionTypeItDoesNotInstall is its mirror: the announced tail carries the
// installed BODIES under each other's types.
//
// IT IS THE HALF THE TYPE CLAUSE IS DECIDABLE OVER, and it is a second fixture rather than a second
// edit of the one above for the reason that one now records. Here every body sits opposite the body
// it is compared with and only the discriminants disagree, so deleting the type comparison accepts
// a commit this row says is refused.
//
// THE TYPES ARE SWAPPED RATHER THAN INVENTED, so the announced vector carries no extension type
// twice and no type this profile refuses. effectiveExtensions prefers a caller announced set, so an
// announced vector holding two entries of one type would be judged by the extension lookup before
// this join ran, and the fixture would wear the name of a rule it never reached.
func testCommitAnnouncingAnExtensionTypeItDoesNotInstall(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	in.Context.Extensions = testCommitInstalledExtensions()
	testCommitProposals(t, in, testGceOf(testCommitInstalledExtensions()...))
	announced := slices.Clone(testCommitInstalledExtensions())
	announced[1].ExtensionType, announced[2].ExtensionType =
		announced[2].ExtensionType, announced[1].ExtensionType
	in.Extensions = announced
	testFitCommitPath(t, crypto, in, members[in.Committer])
	return in
}

// testCommitAnnouncingOneMoreExtensionThanItInstalls is the commit whose announced set is SHORTER
// than the one it installs, and whose first entry is not the installed first entry either.
//
// EVERY ENTRY IT DOES CARRY AGREES, and that is a correction this round made rather than the shape
// it was written in. The fixture used to replace entry zero with an extension the installed set does
// not carry, so the ENTRY clause refused it before the count clause was reached -- and a count
// clause deleted outright left this row refused all the same, by the comparison one line down, with
// the whole suite green. A clause is decidable only over an input NOTHING ELSE refuses, so the
// announced vector here is the installed one with its tail cut off: same entries, same bodies, one
// fewer of them. Delete the count and the loop indexes past the end of the vector the caller
// announced.
func testCommitAnnouncingOneMoreExtensionThanItInstalls(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	in.Context.Extensions = testCommitInstalledExtensions()
	in.Extensions = slices.Clone(testCommitInstalledExtensions()[:2])
	testFitCommitPath(t, crypto, in, members[in.Committer])
	return in
}

// testCommitAnnouncingTheExtensionSetItInstalls is an ACCEPTED commit whose announced and
// installed extension sets agree, and it is two entries long rather than three.
//
// ONE AGREEING PAIR IS ONE LENGTH, which is the limit of the relation claim and was measured on
// this tree: with the corpus holding a single accepted fixture whose two sets agree at three
// entries, `len(self.Extensions) != len(installed)` and `len(self.Extensions) != 3` are the same
// program over every input here, and that mutation survived the full ./mls/... and ./message/...
// run. A relation is separable from a constant when the corpus holds a fixture where it holds and
// one where it does not; it is separable from EVERY constant only when it holds at more than one
// value. This is the second value.
func testCommitAnnouncingTheExtensionSetItInstalls(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	announced := slices.Clone(testCommitInstalledExtensions()[:2])
	in.Context.Extensions = slices.Clone(announced)
	in.Extensions = announced
	testFitCommitPath(t, crypto, in, members[in.Committer])
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused a commit announcing exactly the set it installs: %v",
			failure)
	}
	return in
}

// testCommitWhosePathLeafRepublishesAnAddedKey, ...AnAddedInitKey and ...AnUpdatedKey are the three
// inputs ValSem206PathLeafEncryptionKeyUnique's three clauses are decidable over.
//
// THE RULE REFUSES ON EQUALITY, so there is no accepted input in which any of its three
// comparisons agrees, and a corpus of accepted inputs alone cannot tell the whole rule from
// `return nil`. Three fixtures and not one because the rule reads three different fields -- an
// add's leaf key, an add's INIT key, and an update's leaf key -- and a single collision witnesses
// exactly one of them.
func testCommitWhosePathLeafRepublishesAnAddedKey(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	testCommitProposals(t, in, testAddOf(kp))
	testFitCommitPath(t, crypto, in, members[in.Committer])
	in.Commit.Path.LeafNode.EncryptionKey = slices.Clone(kp.LeafNode.EncryptionKey)
	return in
}

func testCommitWhosePathLeafRepublishesAnAddedInitKey(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	testCommitProposals(t, in, testAddOf(kp))
	testFitCommitPath(t, crypto, in, members[in.Committer])
	in.Commit.Path.LeafNode.EncryptionKey = slices.Clone(kp.InitKey)
	return in
}

func testCommitWhosePathLeafRepublishesAnUpdatedKey(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	// BY REFERENCE, which is the one shape an update inside a commit has: (*ProposalCache).Resolve
	// attributes a by-VALUE entry to the committer, so an inline update would be an update of the
	// committer's own leaf and the vector join would refuse this fixture before ValSem206 ran.
	in.Pending = testCacheAt(t, in.Context)
	update := testCachedUpdateOf(t, crypto, in.Pending, members[2], LeafIndex(2))
	testCommitProposals(t, in, update)
	testFitCommitPath(t, crypto, in, members[in.Committer])
	in.Commit.Path.LeafNode.EncryptionKey =
		slices.Clone(update.Proposal.Update.LeafNode.EncryptionKey)
	return in
}

// testCommitWhosePathLeafKeepsTheCommittersKey is the input ValSem204PathKeyMismatch is decidable
// over: the update path publishes the key the committer already had.
//
// Section 12.4.2's rule is that the two must DIFFER, so every accepted commit in the corpus has
// them differing and the comparison is indistinguishable from `false` over accepted inputs alone.
func testCommitWhosePathLeafKeepsTheCommittersKey(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, _ := testFullCommitInput(t, crypto)
	current := in.PreTree.Leaf(in.Committer)
	if current == nil {
		t.Fatalf("leaf %d of the pre tree is blank, so this fixture has no committer", in.Committer)
	}
	in.Commit.Path.LeafNode.EncryptionKey = slices.Clone(current.EncryptionKey)
	return in
}

// testCommitWhoseVectorIsShorterThanItsList is the input checkListResolvesTheCommitsVector's LENGTH
// clause is decidable over.
//
// The join holds the ProposalOrRef vector the sender signed to the list this member resolved it
// into, and every fixture of this package builds the second from the first -- testCommitProposals
// sets both from one set of entries so that a fixture cannot make them disagree by accident. That
// is the right default and it left the two lengths equal in every input, so `!=` and `false` were
// the same program.
func testCommitWhoseVectorIsShorterThanItsList(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	testCommitProposals(t, in, testRemoveOf(LeafIndex(3)), testRemoveOf(LeafIndex(2)))
	testFitCommitPath(t, crypto, in, members[in.Committer])
	// the vector the sender signed names ONE of the two proposals the list resolves, which is
	// the caller error this value exists to name
	in.Commit.Proposals = slices.Clone(in.Commit.Proposals[:1])
	return in
}

// testCommitWhosePathIsShorterThanItsFilteredDirectPath is the input ValSem202PathLength is
// decidable over.
//
// Every other fixture fits its path to the committer's filtered direct path -- testFitCommitPath is
// how they are built -- so the two lengths agreed everywhere and the comparison could have been
// stated over either side alone.
func testCommitWhosePathIsShorterThanItsFilteredDirectPath(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, _ := testFullCommitInput(t, crypto)
	if len(in.Commit.Path.Nodes) == 0 {
		t.Fatalf("this fixture's path already has no nodes, so there is nothing to shorten and the length rule stays undecidable")
	}
	in.Commit.Path.Nodes = slices.Clone(in.Commit.Path.Nodes[:len(in.Commit.Path.Nodes)-1])
	return in
}

// testValidationInputRemovingItsOwnCommitter is the input validateCommitterIsNotRemoved is
// decidable over, and testValidationInputCarryingTheCommittersOwnUpdate is ValSem111's.
//
// BOTH RULES REFUSE ON EQUALITY, so no accepted fixture can witness their comparisons agreeing --
// and the last round left ValSem111's `updates[i].Sender == in.Committer` surviving a replacement
// by `== LeafIndex(0)` for exactly that reason, dying in the full suite only to a gate about
// bucket positions rather than to anything stated about the rule.
func testValidationInputRemovingItsOwnCommitter(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	return testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testRemoveOf(testCommitterLeaf)))
}

func testValidationInputCarryingTheCommittersOwnUpdate(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")
	update, _ := testUpdateProposalOf(t, crypto, members[testCommitterLeaf], testCommitterLeaf)
	return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, update))
}

// testValidationInputAddingAKeyPackageForAnotherSuite is the input ValSem105's SUITE clause is
// decidable over in the refusing direction.
//
// testValidationInputUnderTheOtherSuite already separates the ciphersuite as a DIMENSION -- a whole
// group running the registry's other suite -- but in it the group and the add agree, as they must
// for the input to be accepted. Separation is not discrimination: the comparison and `false` stay
// the same program until some input has the two disagreeing.
func testValidationInputAddingAKeyPackageForAnotherSuite(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	other := testOtherSuiteCrypto(t, crypto)
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, other, testIdentity(t, other, "erin"))
	return testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testAddOf(kp)))
}

// testValidationInputWhoseAddReusesItsInitKey is the input ValSem104 is decidable over: an added
// key package whose init key IS its own leaf's encryption key.
func testValidationInputWhoseAddReusesItsInitKey(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	kp.InitKey = slices.Clone(kp.LeafNode.EncryptionKey)
	return testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testAddOf(kp)))
}

// testValidationInputWhoseUpdateRepublishesTheLeafKeyItReplaces is the input
// validateUpdateChangesTheEncryptionKey is decidable over.
//
// THE LEAF IS RE-SIGNED after its key is put back, because the signature covers the whole leaf: an
// update leaf edited and not re-signed is refused by validateUpdateLeafNodeIsValidForAnUpdate one
// rule earlier, which would make this a fixture about signatures wearing the name of one about
// keys.
func testValidationInputWhoseUpdateRepublishesTheLeafKeyItReplaces(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")
	replaced := tree.Leaf(LeafIndex(2))
	if replaced == nil {
		t.Fatalf("leaf 2 of this fixture's tree is blank, so there is no leaf for an update to republish")
	}
	leaf, _ := testUpdateLeafNode(t, crypto, members[2], testValidationGroupId(), LeafIndex(2))
	leaf.EncryptionKey = slices.Clone(replaced.EncryptionKey)
	return testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testResignedUpdateOf(t, crypto, members[2], LeafIndex(2), leaf)))
}

// ---------------------------------------------------------------------------
// the fixtures the EVERY CONSTANT claim was missing
// ---------------------------------------------------------------------------
//
// EVERY FIXTURE IN THIS SECTION EXISTS FOR ONE AGREEMENT VALUE. A pair witnessed equal in one
// fixture and unequal in another is separable from ONE constant -- the value it happened to agree
// at -- and the round before this one proved that is not enough by moving forty-nine call sites off
// LeafIndex(0) and landing every one of them on LeafIndex(1). So each of these either gives a pair
// a SECOND value to agree at, or carries the one value it already agrees at on a side where the two
// disagree, which is the position the constant and the comparison part company at.

// testCommitFromLeafZeroJudgedFromTheLeafItsCommitterUsuallyOccupies is the commit whose committer
// sits at leaf zero and whose judge sits at leaf one.
//
// IT IS THE SECOND POSITION ValSem203PathDecrypt is decidable from, and it is needed because the
// last repair moved the pin rather than removing it. Own and Committer agree in exactly one fixture
// and they agree there at ONE, so `in.Own == LeafIndex(1)` and `in.Own == in.Committer` answered
// alike over the whole corpus -- the same defect the round before closed at LeafIndex(0), one
// number along. Here Own IS one while the two disagree, which no constant survives.
func testCommitFromLeafZeroJudgedFromTheLeafItsCommitterUsuallyOccupies(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol", "dave")
	in := testCommitInput(t, crypto, tree, &ProposalList{}, &Commit{})
	in.Committer = testCommitterAtLeafZero
	in.Own = testCommitterLeaf
	// a by-value entry of a commit vector resolves to whoever sent the commit, which is this
	// fixture's committer and not the one testRemoveOf names
	removes := []CachedProposal{testRemoveOf(testHeldRemoveTarget)}
	removes[0].Sender = in.Committer
	testCommitProposals(t, in, removes...)
	testFitCommitPath(t, crypto, in, members[in.Committer])
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused a commit sent from leaf zero: %v; a fixture no door accepts measures nothing about the doors",
			failure)
	}
	return in
}

// testCommitWhosePendingCacheBelongsToAnotherGroup is the commit whose own proposal cache is bound
// to a different group at a different epoch.
//
// TWO PAIRS AND NOTHING IN THE CORPUS HELD EITHER. (*ProposalCache).bindingHolds compares the
// caller's group id and epoch against the ones the cache was bound to, and this door reaches it --
// CheckErrata8815 asks it of the whole vector and the by-reference arm of the join asks it per
// entry. Every fixture that carried a cache at all built that cache AT this input's own context, so
// both comparisons and the constant `true` were the same program here.
//
// ONLY THE GROUP MOVES, and the epoch is deliberately left where it is. One fixture per axis is
// what makes a refusal here say WHICH of the two facts was wrong, and it is what the two epoch
// fixtures beside it need anyway: the epoch pair agrees at one value across the whole corpus, so
// the claim next door asks for that value to appear on each side while the two disagree, and no
// single fixture can carry it on both.
//
// WHAT THIS DOES NOT ESTABLISH, said here because the obvious reading is that it does: bindingHolds
// is an AND of the two comparisons and NONE of these three fixtures decides between its clauses.
// Rebind empties the cache as it moves it, so the reference this commit names is missing whatever
// the binding says, and the door answers errProposalNotCached by a route that never reaches the
// AND. Measured: deleting either clause of bindingHolds leaves every claim in this file green and
// dies in proposal_list_test.go, at TestCheckEpochAnswersTheBindingAndRebindMovesIt and
// TestResolveRefusesAReferenceCachedInAnEpochThatHasClosed. What these fixtures are for is the
// relation claim -- two paths this corpus used to carry one value at.
func testCommitWhosePendingCacheBelongsToAnotherGroup(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	// STORED FIRST AND REBOUND AFTER, because a cache refuses a proposal framed in a group it
	// does not belong to -- Store asks bindingHolds too. So the entry goes in under the group
	// this fixture is in and the cache is then moved, which is the state a member reaches by
	// advancing an epoch: the commit still names what it received, and the cache no longer
	// answers to the epoch the commit is being judged in.
	cache := testCacheAt(t, testResolveContext())
	held := testCachedRemoveOf(t, crypto, cache, testHeldRemoveTarget)
	elsewhere := *in.Context
	elsewhere.GroupId = testSecondGroupId()
	if failure := cache.Rebind(testVerifiedContextAt(t, &elsewhere)); failure != nil {
		t.Fatalf("Rebind this fixture's cache to epoch %d of group %x: %v",
			elsewhere.Epoch, elsewhere.GroupId, failure)
	}
	in.Pending = cache
	in.List = testProposalList(t, held)
	in.Commit.Proposals = []ProposalOrRef{
		{Type: ProposalOrRefTypeReference, Reference: held.Ref},
	}
	testFitCommitPath(t, crypto, in, members[in.Committer])
	return in
}

// testCommitWhosePendingCacheBelongsToALaterEpoch is the third of the cache fixtures, and it is the
// epoch comparison read from the other side.
//
// A PAIR NEEDS A WITNESS IN BOTH DIRECTIONS WHEN ITS TWO SIDES ARE BOTH FIELDS. The corpus agrees
// on the epoch at ONE value -- epoch 1, which is where almost every fixture lives -- so a constant
// stands in for the comparison unless some fixture carries epoch 1 on EACH side while the two
// disagree. testCommitInputInASecondGroupAtALaterEpoch carries it on the binding, being a caller in
// epoch 9 holding a cache still at 1; this carries it on the caller, being a caller in epoch 1
// holding a cache that has been moved on to 9. Neither alone is enough, because
// `caller.epoch == 1` and `binding.epoch == 1` are two different constant rules and the claim next
// door refuses both.
func testCommitWhosePendingCacheBelongsToALaterEpoch(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	// stored first and rebound after, for the reason the group fixture above records
	cache := testCacheAt(t, testResolveContext())
	held := testCachedRemoveOf(t, crypto, cache, testHeldRemoveTarget)
	ahead := *in.Context
	ahead.Epoch = testSecondEpoch
	if failure := cache.Rebind(testVerifiedContextAt(t, &ahead)); failure != nil {
		t.Fatalf("Rebind this fixture's cache to epoch %d of group %x: %v",
			ahead.Epoch, ahead.GroupId, failure)
	}
	in.Pending = cache
	in.List = testProposalList(t, held)
	in.Commit.Proposals = []ProposalOrRef{
		{Type: ProposalOrRefTypeReference, Reference: held.Ref},
	}
	testFitCommitPath(t, crypto, in, members[in.Committer])
	return in
}

// testCommitCarryingAProposalUnderItsOwnTypeAsTheUnknownOne is the commit whose proposal names the
// same type twice: once as its arm and once as the discriminant the encoder is to write.
//
// IT IS THE ONLY INPUT checkProposalProfile's forgery clause AGREES in. That clause refuses a
// proposal whose UnknownType is set and names something other than its ProposalType, and every
// fixture in either corpus left UnknownType at the reserved zero -- so the two were unequal
// wherever they were read and `proposal.UnknownType != proposal.ProposalType` and the constant
// `true` were one program. proposal_list.go says outright that UnknownType equal to ProposalType is
// the admitted shape, because it is how proposal_wire.go makes a GREASE code point round trip, and
// until now nothing in the corpus carried it.
func testCommitCarryingAProposalUnderItsOwnTypeAsTheUnknownOne(t *testing.T,
	crypto CryptoProvider) *CommitValidationInput {

	t.Helper()
	in, members := testFullCommitInput(t, crypto)
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	testCommitProposals(t, in, testAddOf(kp), testRemoveOf(testHeldRemoveTarget))
	// THE FIELD IS SET ON THE ORDER THE LIST KEEPS and not on the entries handed to the
	// constructor. NewProposalList clones every proposal through the codec, and the decoder
	// normalises an UnknownType naming a registered type back to the reserved zero -- so a
	// fixture that set it before construction would announce nothing at all. The commit's own
	// vector is taken from the list afterwards, so the two sides of the join carry one proposal.
	//
	// TWO ENTRIES OF TWO TYPES, because one is separable from one constant only: a corpus in
	// which the clause agrees at `remove` and nowhere else cannot tell
	// `UnknownType != proposal.ProposalType` from `UnknownType != ProposalTypeRemove`. The add
	// is the second value it agrees at.
	order := in.List.All()
	for i := range order {
		order[i].Proposal.UnknownType = order[i].Proposal.ProposalType
	}
	in.Commit.Proposals = in.List.Refs()
	testFitCommitPath(t, crypto, in, members[in.Committer])
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused a proposal announcing its own type as the discriminant: %v; proposal_list.go names that the admitted shape",
			failure)
	}
	return in
}

// testValidationInputWhoseUpdateComesFromTheLeafItsCommitterUsuallyOccupies is ValSem111's second
// position, and it is the proposal door's copy of the argument above.
//
// Every fixture that reaches ValSem111 carries Committer = 1, and the one fixture where the rule's
// two sides AGREE agrees at 1 -- so `updates[i].Sender == LeafIndex(1)` and
// `updates[i].Sender == in.Committer` are one program over this corpus, which is precisely the
// state the previous round left after moving forty-nine call sites off LeafIndex(0). Here an update
// is sent FROM leaf one while the committer sits at leaf zero, so the sender carries the agreement
// value at a position the two disagree at.
func testValidationInputWhoseUpdateComesFromTheLeafItsCommitterUsuallyOccupies(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, members := testTreeWith(t, crypto, "alice", "bob", "carol")
	update, _ := testUpdateProposalOf(t, crypto, members[testCommitterLeaf], testCommitterLeaf)
	in := testValidationInput(t, crypto, tree, testCommitterAtLeafZero,
		testProposalList(t, update))
	if failure := ValidateProposalList(in); failure != nil {
		t.Fatalf("ValidateProposalList refused an update sent from leaf one under a committer at leaf zero: %v",
			failure)
	}
	return in
}

// testValidationInputAddingAKeyPackageForAnotherVersion is ValSem105's VERSION clause in the one
// direction the corpus could not reach.
//
// testValidationInputAnnouncingAnUnimplementedVersion moves the GROUP off mls10 and leaves the
// added key package on it, which separates the two as a relation but pins the group's own side:
// every fixture in which the two AGREE agrees at mls10, and no fixture carries mls10 in the group
// while the add carries something else. So `kp.Version != ProtocolVersionMls10` and
// `kp.Version != in.Context.Version` answer alike everywhere. This is the other direction, and it
// is the one a real peer sends: an ordinary mls10 group handed an Add announcing a version this
// build does not implement.
//
// THE VERSION IS EDITED AFTER THE KEY PACKAGE IS MINTED and that is safe HERE for a reason the
// rule's own order gives: ValSem105 compares the version and the suite BEFORE it calls
// KeyPackage.Validate, so the refusal this row names is reached without the signature over the
// edited field ever being checked. A fixture relying on that order is a fixture that would start
// failing loudly if the order changed, which is the right way round.
func testValidationInputAddingAKeyPackageForAnotherVersion(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	kp.Version = testUnimplementedProtocolVersion
	return testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testAddOf(kp)))
}

// testValidationInputCarryingAProposalUnderItsOwnTypeAsTheUnknownOne is the proposal door's copy of
// the commit fixture above: the one input checkProposalProfile's forgery clause agrees in.
func testValidationInputCarryingAProposalUnderItsOwnTypeAsTheUnknownOne(t *testing.T,
	crypto CryptoProvider) *ProposalValidationInput {

	t.Helper()
	tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	in := testValidationInput(t, crypto, tree, testCommitterLeaf,
		testProposalList(t, testAddOf(kp), testRemoveOf(testHeldRemoveTarget)))
	// set on the order the list keeps, and over two types, for the reasons the commit fixture
	// above records
	order := in.List.All()
	for i := range order {
		order[i].Proposal.UnknownType = order[i].Proposal.ProposalType
	}
	if failure := ValidateProposalList(in); failure != nil {
		t.Fatalf("ValidateProposalList refused a proposal announcing its own type as the discriminant: %v; proposal_list.go names that the admitted shape",
			failure)
	}
	return in
}

// ---------------------------------------------------------------------------
// the corpora
// ---------------------------------------------------------------------------

// validationFixtureRow is one entry of either corpus: the fixture, and the verdict the door it is a
// fixture FOR must give it.
//
// THE VERDICT IS THE HALF THAT KILLS THE SURVIVORS, and it is stated ONCE over both doors because
// stating it at one of them is what left three of them alive. Separating a dimension in the corpus
// makes a production read distinguishable from a constant; it does not by itself make any test
// NOTICE the difference. The proposal corpus has been driven through its door since the round that
// built it, while the commit corpus was measured by a gate that never called ValidateCommit -- so
// ten of fourteen commit fixtures were never judged by anything, and varying a corpus nothing runs
// changes nothing. A row that expects a refusal names the sentinel, so a fixture that starts being
// refused for a different reason is a failure rather than a pass.
//
// PARAMETERISED BY THE INPUT rather than written twice, for validationFixtureBuildersInSource's
// reason one section down: a second door held by a weaker structure than the first is how the
// first door's repairs stop reaching the second.
type validationFixtureRow[Input any] struct {
	build func(*testing.T, CryptoProvider) *Input
	// refuses is the value the door must answer, or nil where it must accept.
	refuses error
}

// testCommitterAtLeafZero is the one committer in either corpus that sits on leaf zero, and it is
// there ON PURPOSE.
//
// Every other call site of testValidationInput now names testCommitterLeaf, which is the repair
// this round made -- forty-nine of them used to spell LeafIndex(0) and no test said why. But a
// corpus in which the committer is NEVER leaf zero is degenerate in the other direction: leaf zero
// is exactly where `x == in.Committer` and `x == LeafIndex(0)` disagree, so one fixture keeps it
// and says so here rather than leaving a number at a call site to carry the argument.
const testCommitterAtLeafZero = LeafIndex(0)

// testFullValidationMembers is the group testFullValidationInput builds for the corpus.
//
// NINE, because that fixture divides its group into thirds -- three updates, three removes and
// three adds -- and a width that is not a multiple of three would make one of those buckets shorter
// than the others for a reason nobody chose.
const testFullValidationMembers = 9

// commitFixtureCorpus is every fixture of this package that answers a commit validation input,
// keyed by the name it is declared under.
//
// KEYED BY THE DECLARED NAME because that is what the derivation below can read. Several of these
// builders take arguments -- a tree, a list, an input to lead -- and the row supplies the ordinary
// ones, so what is measured is the fixture as its callers use it rather than a zero value of it.
func commitFixtureCorpus() map[string]validationFixtureRow[CommitValidationInput] {
	return map[string]validationFixtureRow[CommitValidationInput]{
		"testCommitInput": {build: func(t *testing.T, crypto CryptoProvider) *CommitValidationInput {
			tree, members := testTreeWith(t, crypto, "alice", "bob", "carol", "dave")
			in := testCommitInput(t, crypto, tree, &ProposalList{}, &Commit{})
			testCommitProposals(t, in, testRemoveOf(LeafIndex(2)))
			// the path is fitted because the row is DRIVEN now and not only measured: a
			// list carrying a Remove requires one, so the fixture this helper's own
			// callers build is a commit with a path and this row used to be the one
			// shape of it nothing ever asked a door about
			testFitCommitPath(t, crypto, in, members[in.Committer])
			return in
		}},
		"testFullCommitInput": {build: func(t *testing.T, crypto CryptoProvider) *CommitValidationInput {
			in, _ := testFullCommitInput(t, crypto)
			return in
		}},
		"testCommitCarryingOneOfEveryBucket": {build: testCommitCarryingOneOfEveryBucket},
		"testCommitCarryingOneOfEveryBucketAndItsMembers": {build: func(t *testing.T,
			crypto CryptoProvider) *CommitValidationInput {
			in, _ := testCommitCarryingOneOfEveryBucketAndItsMembers(t, crypto)
			return in
		}},
		"testCommitCarryingAnInnocentRemoveFirst": {build: testCommitCarryingAnInnocentRemoveFirst},
		"testCommitNamingACachedProposal":         {build: testCommitNamingACachedProposal},
		"testCommitNamingACachedRemove": {build: func(t *testing.T,
			crypto CryptoProvider) *CommitValidationInput {
			in, _, _ := testCommitNamingACachedRemove(t, crypto, testHeldRemoveTarget)
			return in
		}},
		"testCommitWideEnoughToPrice": {build: testCommitWideEnoughToPrice},
		"testCommitLedBy": {build: func(t *testing.T, crypto CryptoProvider) *CommitValidationInput {
			return testCommitLedBy(t, testCommitCarryingOneOfEveryBucket(t, crypto),
				testRemoveOf(LeafIndex(3)))
		}},
		"testCommitInputOverTheTreeItsProposalsBuild": {
			build: testCommitInputOverTheTreeItsProposalsBuild},
		"testWideCommitInput": {build: testWideCommitInput},
		"testCommitInputInASecondGroupAtALaterEpoch": {
			build: testCommitInputInASecondGroupAtALaterEpoch},
		"testCommitInputUnderTheOtherSuite": {build: testCommitInputUnderTheOtherSuite},
		"testCommitInputAnnouncingAnUnimplementedVersion": {
			build: testCommitInputAnnouncingAnUnimplementedVersion},
		"testCommitTheCommitterJudgesItselfFromABlankSibling": {
			build: testCommitTheCommitterJudgesItselfFromABlankSibling},
		"testCommitTheCommitterJudgesItselfFromLeafZeroWithABlankSibling": {
			build: testCommitTheCommitterJudgesItselfFromLeafZeroWithABlankSibling},
		"testCommitAnnouncingTheExtensionSetItInstalls": {
			build: testCommitAnnouncingTheExtensionSetItInstalls},
		"testCommitAnnouncingAnExtensionBodyItDoesNotInstall": {
			build:   testCommitAnnouncingAnExtensionBodyItDoesNotInstall,
			refuses: errCommitExtensionsNotApplied},
		"testCommitAnnouncingAnExtensionTypeItDoesNotInstall": {
			build:   testCommitAnnouncingAnExtensionTypeItDoesNotInstall,
			refuses: errCommitExtensionsNotApplied},
		"testCommitFromLeafZeroJudgedFromTheLeafItsCommitterUsuallyOccupies": {
			build: testCommitFromLeafZeroJudgedFromTheLeafItsCommitterUsuallyOccupies},
		"testCommitCarryingAProposalUnderItsOwnTypeAsTheUnknownOne": {
			build: testCommitCarryingAProposalUnderItsOwnTypeAsTheUnknownOne},
		"testCommitWhosePendingCacheBelongsToAnotherGroup": {
			build:   testCommitWhosePendingCacheBelongsToAnotherGroup,
			refuses: errProposalNotCached},
		"testCommitWhosePendingCacheBelongsToALaterEpoch": {
			build:   testCommitWhosePendingCacheBelongsToALaterEpoch,
			refuses: errProposalNotCached},
		"testCommitAnnouncingOneMoreExtensionThanItInstalls": {
			build:   testCommitAnnouncingOneMoreExtensionThanItInstalls,
			refuses: errCommitExtensionsNotApplied},
		"testCommitWhosePathLeafRepublishesAnAddedKey": {
			build:   testCommitWhosePathLeafRepublishesAnAddedKey,
			refuses: errDuplicateEncryptionKey},
		"testCommitWhosePathLeafRepublishesAnAddedInitKey": {
			build:   testCommitWhosePathLeafRepublishesAnAddedInitKey,
			refuses: errDuplicateEncryptionKey},
		"testCommitWhosePathLeafRepublishesAnUpdatedKey": {
			build:   testCommitWhosePathLeafRepublishesAnUpdatedKey,
			refuses: errDuplicateEncryptionKey},
		"testCommitWhosePathLeafKeepsTheCommittersKey": {
			build:   testCommitWhosePathLeafKeepsTheCommittersKey,
			refuses: errPathLeafKeyUnchanged},
		"testCommitWhoseVectorIsShorterThanItsList": {
			build:   testCommitWhoseVectorIsShorterThanItsList,
			refuses: errCommitProposalsNotResolved},
		"testCommitWhosePathIsShorterThanItsFilteredDirectPath": {
			build:   testCommitWhosePathIsShorterThanItsFilteredDirectPath,
			refuses: errPathLength},
	}
}

// proposalFixtureCorpus is every fixture of this package that answers a section 12.2 validation
// input, keyed by the name it is declared under.
func proposalFixtureCorpus() map[string]validationFixtureRow[ProposalValidationInput] {
	return map[string]validationFixtureRow[ProposalValidationInput]{
		"testValidationInput": {build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
			tree, _ := testTreeWith(t, crypto, "alice", "bob", "carol")
			// THE REMOVED LEAF IS testCommitterLeaf and not the ordinary held
			// target, which is this row carrying its share of the every-constant
			// claim rather than a second fixture doing it. validateCommitterIsNotRemoved
			// compares Removed against in.Committer, the two agree in exactly one
			// fixture and they agree there at ONE -- so `Removed == LeafIndex(1)` and
			// the honest comparison answer alike unless some fixture carries leaf one
			// on the removed side while the committer sits elsewhere. This is that
			// fixture: the committer is at leaf zero and the remove names leaf one.
			return testValidationInput(t, crypto, tree, testCommitterAtLeafZero,
				testProposalList(t, testRemoveOf(testCommitterLeaf)))
		}},
		"updateSweepFixture": {build: updateSweepFixture},
		"testFullValidationInput": {build: func(t *testing.T, crypto CryptoProvider) *ProposalValidationInput {
			return testFullValidationInput(t, crypto, testFullValidationMembers)
		}},
		"testValidationInputInASecondGroupAtALaterEpoch": {
			build: testValidationInputInASecondGroupAtALaterEpoch},
		"testValidationInputUnderTheOtherSuite": {build: testValidationInputUnderTheOtherSuite},
		"testWideValidationInput":               {build: testWideValidationInput},
		"testValidationInputAnnouncingAnUnimplementedVersion": {
			build:   testValidationInputAnnouncingAnUnimplementedVersion,
			refuses: ErrSuiteMismatch},
		"testValidationInputOnAClockPastTheAddsLifetime": {
			build:   testValidationInputOnAClockPastTheAddsLifetime,
			refuses: ErrLeafNodeLifetime},
		"testValidationInputRemovingItsOwnCommitter": {
			build:   testValidationInputRemovingItsOwnCommitter,
			refuses: ErrRemoveCommitter},
		"testValidationInputCarryingTheCommittersOwnUpdate": {
			build:   testValidationInputCarryingTheCommittersOwnUpdate,
			refuses: ErrSelfUpdateInCommit},
		"testValidationInputAddingAKeyPackageForAnotherSuite": {
			build:   testValidationInputAddingAKeyPackageForAnotherSuite,
			refuses: ErrSuiteMismatch},
		"testValidationInputWhoseAddReusesItsInitKey": {
			build:   testValidationInputWhoseAddReusesItsInitKey,
			refuses: ErrInitEqualsEncryptionKey},
		"testValidationInputWhoseUpdateRepublishesTheLeafKeyItReplaces": {
			build:   testValidationInputWhoseUpdateRepublishesTheLeafKeyItReplaces,
			refuses: ErrUpdateEncryptionKeyUnchanged},
		"testValidationInputWhoseUpdateComesFromTheLeafItsCommitterUsuallyOccupies": {
			build: testValidationInputWhoseUpdateComesFromTheLeafItsCommitterUsuallyOccupies},
		"testValidationInputCarryingAProposalUnderItsOwnTypeAsTheUnknownOne": {
			build: testValidationInputCarryingAProposalUnderItsOwnTypeAsTheUnknownOne},
		"testValidationInputAddingAKeyPackageForAnotherVersion": {
			build:   testValidationInputAddingAKeyPackageForAnotherVersion,
			refuses: ErrSuiteMismatch},
	}
}

// ---------------------------------------------------------------------------
// the corpora, held to the package that declares the fixtures
// ---------------------------------------------------------------------------

// validationFixtureBuildersInSource is every function this package's test files declare that
// ANSWERS a validation input of the named type.
//
// THE DERIVATION IS OVER THE RESULT TYPE and not over a naming convention, because the result type
// is what makes a function a fixture for a door: a helper that takes an input and edits it --
// testCommitProposals, testFitCommitPath, testRestoreCachedEntries -- is not a corpus entry, it is
// something a corpus entry is built out of, and none of those answers one.
//
// WHAT IT BUYS is the direction the three rounds kept losing: a NEW fixture is in the corpus the
// moment it is declared, so the next person who adds one that puts the committer back on leaf 0
// finds out here rather than three rounds later. The reverse direction is worth as much -- a row
// naming a builder the package no longer declares is a row measuring nothing.
//
// PARAMETERISED BY THE TYPE NAME rather than written twice, so that the second door's corpus is
// held to the package by the same walk as the first one's and cannot come to be held by a weaker.
func validationFixtureBuildersInSource(t *testing.T, named string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	fileSet := token.NewFileSet()
	found := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declared := range file.Decls {
			function, isFunction := declared.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || function.Type.Results == nil {
				continue
			}
			for _, result := range function.Type.Results.List {
				pointer, isPointer := result.Type.(*ast.StarExpr)
				if !isPointer {
					continue
				}
				identifier, isNamed := pointer.X.(*ast.Ident)
				if isNamed && identifier.Name == named {
					found = append(found, function.Name.Name)
					break
				}
			}
		}
	}
	slices.Sort(found)
	return found
}

// validationDoorMethod is the method every validation input of this package carries, and it is what
// makes such a type identifiable STRUCTURALLY rather than by how it was named.
//
// Both inputs declare `func (self *X) check() error`, and both headers say why it is a method
// rather than a guard repeated in each rule -- "eleven copies of a guard is eleven chances for one
// of them to be the copy that was not updated". So a type that carries one IS a door's input,
// whatever it is called.
const validationDoorMethod = "check"

// validationInputTypesInSource is every validation input type this package's own source declares.
//
// DERIVED SO THAT A THIRD DOOR CANNOT ARRIVE WITHOUT A CORPUS, and so that the gate cannot be
// pointed at one door and left there. This file used to measure CommitValidationInput and nothing
// else while ProposalValidationInput -- the door that reads the group id, the ciphersuite, the
// version and the clock -- had no corpus at all, and nothing anywhere said so.
//
// TWO MARKS, UNIONED, because one of them is a naming convention and a convention is an
// enumeration with extra steps: a third input called AppliedStateInput would carry no corpus and
// nothing would say so. The structural mark is the one that generalises -- a struct that declares
// validationDoorMethod is an input some door refuses arguments at -- and the suffix stays beside it
// so that an input whose door has not grown its guard yet is still measured. Either alone is
// weaker than both; a type is in the class if it answers to either.
func validationInputTypesInSource(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	fileSet := token.NewFileSet()
	structs := map[string]bool{}
	named, guarded := map[string]bool{}, map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declared := range file.Decls {
			if general, isGeneral := declared.(*ast.GenDecl); isGeneral && general.Tok == token.TYPE {
				for _, spec := range general.Specs {
					typed, isTyped := spec.(*ast.TypeSpec)
					if !isTyped {
						continue
					}
					if _, isStruct := typed.Type.(*ast.StructType); !isStruct {
						continue
					}
					structs[typed.Name.Name] = true
					if strings.HasSuffix(typed.Name.Name, "ValidationInput") {
						named[typed.Name.Name] = true
					}
				}
				continue
			}
			function, isFunction := declared.(*ast.FuncDecl)
			if !isFunction || function.Recv == nil || function.Name.Name != validationDoorMethod {
				continue
			}
			if function.Type.Params != nil && len(function.Type.Params.List) != 0 {
				continue
			}
			if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
				continue
			}
			result, isNamed := function.Type.Results.List[0].Type.(*ast.Ident)
			if !isNamed || result.Name != "error" || len(function.Recv.List) != 1 {
				continue
			}
			guarded[receiverTypeName(function.Recv.List[0].Type)] = true
		}
	}
	found := []string{}
	for name := range structs {
		if named[name] || guarded[name] {
			found = append(found, name)
		}
	}
	slices.Sort(found)
	return found
}

// TestEveryCommitFixtureThisPackageDeclaresIsInTheCorpus holds the corpus to the package, in both
// directions.
func TestEveryCommitFixtureThisPackageDeclaresIsInTheCorpus(t *testing.T) {
	assertFixtureCorpusIsThePackages(t, "CommitValidationInput",
		slices.Sorted(maps.Keys(commitFixtureCorpus())))
}

// TestEveryProposalFixtureThisPackageDeclaresIsInTheCorpus is the same claim at the other door.
func TestEveryProposalFixtureThisPackageDeclaresIsInTheCorpus(t *testing.T) {
	assertFixtureCorpusIsThePackages(t, "ProposalValidationInput",
		slices.Sorted(maps.Keys(proposalFixtureCorpus())))
}

// assertFixtureCorpusIsThePackages is that claim written once.
func assertFixtureCorpusIsThePackages(t *testing.T, named string, held []string) {
	t.Helper()
	declared := validationFixtureBuildersInSource(t, named)
	if len(declared) == 0 {
		t.Fatalf("no function in this package's test files answers a *%s, so the derivation read something other than the package and the corpus is a list of names",
			named)
	}
	if !slices.Equal(declared, held) {
		t.Errorf("this package declares the %s fixtures %v and the corpus measures %v; a fixture with no row is one nothing holds to being able to separate a value from a constant, and a row naming a fixture the package no longer declares is a row measuring nothing",
			named, declared, held)
	}
}

// ---------------------------------------------------------------------------
// what a fixture is measured on
// ---------------------------------------------------------------------------

// corpusDimensionHops is how many STRUCT FIELDS deep a dimension is. See this file's header for
// why it is a bound at all and why it is this one.
const corpusDimensionHops = 2

// corpusRenderBudget bounds the canonical rendering below, which follows pointers and would
// otherwise be as deep as the value it is handed.
const corpusRenderBudget = 24

// corpusDimensionsOf answers every dimension one validation input carries, keyed by the PATH that
// reached it and holding the canonical rendering of what was there.
//
// WALKED RATHER THAN LISTED, which is this file's whole argument, and CARRYING NO TYPE, which is
// the correction this round made: the fields a fixture can be degenerate in are whatever fields
// exist, of whatever type, and a walk that names its types is blind to every dimension outside them.
//
// SLICES AGGREGATE UNDER ONE PATH -- "List[].Sender" and not "List[2].Sender" -- because the
// dimension is the FIELD and not the position. A corpus in which every list's second entry is sent
// from leaf 1 while its first varies is not degenerate in the sender; a corpus in which no entry
// anywhere carries a second value is. The slice ITSELF is also recorded, at its own path, so a
// vector that is nil in every fixture is caught rather than answered by its absent elements.
//
// A *RatchetTree IS RENDERED AS ITS TREE HASH. A tree carries a value for every member it has, so
// rendering one literally would make the group's own arithmetic the dimension; its hash is the one
// identity a ratchet tree has -- the value a GroupContext carries and a transcript covers -- and it
// changes if anything in the tree does. Nothing else is special cased and nothing at all is skipped.
//
// AN UNEXPORTED FIELD GETS NO PATH OF ITS OWN AND IS NOT LOST. The walk descends through exported
// fields, because those are what a path can be spelled from; the rendering beside it reads
// unexported state too, so a constant behind one is caught at the nearest exported ancestor rather
// than reported by its own name. That is why a ProposalCache, whose whole state is unexported, is a
// dimension this can tell two of apart.
func corpusDimensionsOf(crypto CryptoProvider, root any) map[string][]string {
	return corpusDimensionsFrom(crypto, root, "", 0)
}

// corpusDimensionsFrom is that walk started somewhere other than the root, which is what the
// accessor below needs: a value reached through a method rather than a field still sits at the
// depth its field sits at, and a walk that restarted the count there would demand separation one
// level deeper on that branch than on every other -- which is how the five proposal arms this
// profile refuses to populate came to be asked for.
func corpusDimensionsFrom(crypto CryptoProvider, root any, from string, at int) map[string][]string {
	out := map[string][]string{}
	record := func(path string, value string) {
		if path == "" {
			return
		}
		out[path] = append(out[path], value)
	}
	var walk func(value reflect.Value, path string, hops int)
	walk = func(value reflect.Value, path string, hops int) {
		if !value.IsValid() {
			record(path, "nil")
			return
		}
		record(path, corpusRenderOf(crypto, value, corpusRenderBudget))
		if hops >= corpusDimensionHops {
			return
		}
		switch value.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !value.IsNil() {
				walk(value.Elem(), path, hops)
			}
		case reflect.Slice, reflect.Array:
			if value.Kind() == reflect.Slice && value.IsNil() {
				return
			}
			// an octet string is ONE dimension, recorded whole a moment ago. Walked byte
			// by byte it would answer a value per octet under "GroupId[]" and pass the
			// claim vacuously, which is the exact shape this gate exists to refuse
			if value.Type().Elem().Kind() == reflect.Uint8 {
				return
			}
			for i := 0; i < value.Len(); i += 1 {
				walk(value.Index(i), path+"[]", hops)
			}
		case reflect.Map:
			if value.IsNil() {
				return
			}
			iterator := value.MapRange()
			for iterator.Next() {
				walk(iterator.Key(), path+"{}key", hops)
				walk(iterator.Value(), path+"{}", hops)
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i += 1 {
				field := value.Type().Field(i)
				if !field.IsExported() {
					continue
				}
				under := field.Name
				if path != "" {
					under = path + "." + field.Name
				}
				walk(value.Field(i), under, hops+1)
			}
		}
	}
	walk(reflect.ValueOf(root), from, at)
	return out
}

// corpusListDimensionsInto adds the commit order's own dimensions to a fixture's map.
//
// The order is behind an unexported field with one accessor, which is what makes a ProposalList
// hold ONE representation -- so it is walked through the accessor. The list itself is already
// recorded by the walk above, under "List"; this adds the per-entry paths beneath it, ENTERED AT
// THE DEPTH THE LIST SITS AT so that an entry's own fields are the same distance from the root as
// any other field's.
func corpusListDimensionsInto(crypto CryptoProvider, into map[string][]string, list *ProposalList) {
	if list == nil {
		return
	}
	order := list.All()
	for i := range order {
		for path, values := range corpusDimensionsFrom(crypto, order[i], corpusOrderPath, 1) {
			into[path] = append(into[path], values...)
		}
	}
}

// corpusOrderPath is the path this accessor writes its entries under, and it is a constant because
// the gate reads it back: a corpus whose lists carry entries and whose measurement holds no
// dimension beneath them is one where this call has been dropped, and dropping it would take the
// per-entry dimensions -- the sender the last round fought for among them -- with nothing else
// noticing. See the claim in assertCorpusSeparatesItsDimensions.
const corpusOrderPath = "List[]"

// corpusRenderOf is the canonical rendering of one value.
//
// IT NEVER PRINTS AN ADDRESS, which is why it is written out rather than left to %v. A pointer
// formatted by the fmt package prints where it points, which differs between two fixtures holding
// equal values and between two runs holding the same one -- so a dimension rendered that way is
// separated by construction and measures nothing. This follows pointers instead. It also reads
// UNEXPORTED fields, through the kind specific accessors reflect allows without Interface(), so a
// type whose whole state is unexported -- a ProposalCache, a crypto provider -- is a value this can
// tell two of apart rather than an opaque one every fixture shares.
func corpusRenderOf(crypto CryptoProvider, value reflect.Value, budget int) string {
	if !value.IsValid() {
		return "nil"
	}
	if budget <= 0 {
		return "..."
	}
	if tree, isTree := corpusTreeIdentity(crypto, value); isTree {
		return tree
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return "nil"
		}
		return corpusRenderOf(crypto, value.Elem(), budget-1)
	case reflect.Bool:
		return strconv.FormatBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'g', -1, 64)
	case reflect.Complex64, reflect.Complex128:
		return fmt.Sprint(value.Complex())
	case reflect.String:
		return strconv.Quote(value.String())
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return "nil"
		}
		// an octet string is ONE value and not a value per byte, for the walk's own reason
		if value.Type().Elem().Kind() == reflect.Uint8 {
			raw := make([]byte, value.Len())
			for i := 0; i < value.Len(); i += 1 {
				raw[i] = byte(value.Index(i).Uint())
			}
			return hex.EncodeToString(raw)
		}
		parts := make([]string, 0, value.Len())
		for i := 0; i < value.Len(); i += 1 {
			parts = append(parts, corpusRenderOf(crypto, value.Index(i), budget-1))
		}
		return "[" + strings.Join(parts, " ") + "]"
	case reflect.Map:
		if value.IsNil() {
			return "nil"
		}
		// sorted, because map iteration order is randomised and an unsorted rendering would
		// separate a dimension from itself
		parts := make([]string, 0, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			parts = append(parts, corpusRenderOf(crypto, iterator.Key(), budget-1)+"="+
				corpusRenderOf(crypto, iterator.Value(), budget-1))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, " ") + "}"
	case reflect.Struct:
		if value.Type() == reflect.TypeFor[time.Time]() && value.CanInterface() {
			// an instant, in one spelling, rather than the wall clock and monotonic halves
			// the struct keeps it in
			return value.Interface().(time.Time).UTC().Format(time.RFC3339Nano)
		}
		parts := make([]string, 0, value.NumField())
		for i := 0; i < value.NumField(); i += 1 {
			parts = append(parts, value.Type().Field(i).Name+":"+
				corpusRenderOf(crypto, value.Field(i), budget-1))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		// a behaviour rather than a value: two of these are distinguishable by nothing a
		// fixture could vary on purpose
		if value.IsNil() {
			return "nil"
		}
		return "set:" + value.Type().String()
	}
	return "?" + value.Type().String()
}

// corpusTreeIdentity answers a ratchet tree's identity, and whether the value was one.
func corpusTreeIdentity(crypto CryptoProvider, value reflect.Value) (string, bool) {
	if value.Type() != reflect.TypeFor[*RatchetTree]() || !value.CanInterface() {
		return "", false
	}
	tree, isTree := value.Interface().(*RatchetTree)
	if !isTree || tree == nil {
		return "nil", true
	}
	hash, err := tree.TreeHash(crypto)
	if err != nil {
		return "unhashable:" + err.Error(), true
	}
	return "tree:" + hex.EncodeToString(hash), true
}

// corpusLeafIndicesOf answers every LeafIndex a value carries, at any depth.
//
// UNBOUNDED, unlike the dimension walk, and that is the one thing this adds to it. The octet claim
// below is about the WIDEST leaf index anywhere in the corpus, so it has to see the ones a
// proposal's own body carries; a leaf index deeper than corpusDimensionHops is folded into an
// ancestor's rendering for the purpose of the constancy claim, which is the right answer there and
// the wrong one here.
//
// A tree is skipped, because a tree carries a leaf index for every member it has and the claim is
// about what the INPUT names.
func corpusLeafIndicesOf(value any) []LeafIndex {
	found := []LeafIndex{}
	leafIndex := reflect.TypeFor[LeafIndex]()
	tree := reflect.TypeFor[*RatchetTree]()
	var walk func(value reflect.Value, depth int)
	walk = func(value reflect.Value, depth int) {
		// a bound rather than a visited set: nothing under a validation input is self
		// referential, and a bound cannot silently drop a path a pointer set would
		if depth > 12 || !value.IsValid() || value.Type() == tree {
			return
		}
		if value.Type() == leafIndex {
			found = append(found, LeafIndex(value.Uint()))
			return
		}
		switch value.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !value.IsNil() {
				walk(value.Elem(), depth+1)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < value.Len(); i += 1 {
				walk(value.Index(i), depth+1)
			}
		case reflect.Map:
			iterator := value.MapRange()
			for iterator.Next() {
				walk(iterator.Key(), depth+1)
				walk(iterator.Value(), depth+1)
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i += 1 {
				if !value.Type().Field(i).IsExported() {
					continue
				}
				walk(value.Field(i), depth+1)
			}
		}
	}
	walk(reflect.ValueOf(value), 0)
	return found
}

// corpusShapesOf answers, for one built input, the LENGTH of every vector dimension it carries and
// the value of every TIMESTAMP it carries, each keyed by the path that reached it.
//
// TWO AGGREGATE CLAIMS USED TO NAME THEIR OWN FIELD. "no fixture carries more than one proposal"
// read in.List and nothing else, and "one fixture's clock is not the wall clock" read in.Now and
// nothing else -- so "no other vector is always one long" and "no other timestamp is time.Now()"
// were sentences nobody had checked. Both are the ledger-21 shape one level up from where this file
// already fixed it: the walk was derived and the SCOPE of the claim stated over it was written
// down. This is that scope derived. What a vector is, and what a timestamp is, are read off the
// values.
//
// IT DESCENDS THE WAY corpusDimensionsFrom DESCENDS, to the same bound and by the same rules, so a
// vector the dimension claim measures is a vector this measures and there is no path that is one
// claim's business and not the other's. An octet string is not a vector here for that walk's reason:
// it is ONE value, and its length is a property of the value rather than a dimension of the input.
func corpusShapesOf(root any) (map[string]int, map[string]time.Time) {
	vectors, clocks := map[string]int{}, map[string]time.Time{}
	var walk func(value reflect.Value, path string, hops int)
	walk = func(value reflect.Value, path string, hops int) {
		if !value.IsValid() {
			return
		}
		if value.Type() == reflect.TypeFor[time.Time]() && value.CanInterface() && path != "" {
			clocks[path] = value.Interface().(time.Time)
			return
		}
		// RECORDED BEFORE THE BOUND IS CHECKED, exactly as corpusDimensionsFrom records a
		// value before it decides whether to descend past it. A vector sitting AT the bound
		// is a vector this claim is about; what the bound stops is walking INTO it.
		if path != "" && (value.Kind() == reflect.Slice || value.Kind() == reflect.Array ||
			value.Kind() == reflect.Map) {
			countable := value.Kind() == reflect.Array ||
				(!value.IsNil() && value.Type() != reflect.TypeFor[[]byte]())
			if value.Kind() != reflect.Map && value.Type().Elem().Kind() == reflect.Uint8 {
				countable = false
			}
			if countable && value.Len() > vectors[path] {
				vectors[path] = value.Len()
			}
		}
		if hops >= corpusDimensionHops {
			return
		}
		switch value.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !value.IsNil() {
				walk(value.Elem(), path, hops)
			}
		case reflect.Slice, reflect.Array:
			if value.Kind() == reflect.Slice && value.IsNil() {
				return
			}
			if value.Type().Elem().Kind() == reflect.Uint8 {
				return
			}
			for i := 0; i < value.Len(); i += 1 {
				walk(value.Index(i), path+"[]", hops)
			}
		case reflect.Map:
			if value.IsNil() {
				return
			}
			iterator := value.MapRange()
			for iterator.Next() {
				walk(iterator.Value(), path+"{}", hops)
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i += 1 {
				field := value.Type().Field(i)
				if !field.IsExported() {
					continue
				}
				under := field.Name
				if path != "" {
					under = path + "." + field.Name
				}
				walk(value.Field(i), under, hops+1)
			}
		}
	}
	walk(reflect.ValueOf(root), "", 0)
	return vectors, clocks
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// corpusClockSeparation is how far from the wall clock a fixture's clock has to be before it
// separates the field from a call. A key package this package mints is valid for about ninety days,
// so anything inside that window decides every lifetime the same way whichever of the two is read.
const corpusClockSeparation = 365 * 24 * time.Hour

// corpusFixtureUnderTest is one built fixture reduced to what the claims below are stated over.
type corpusFixtureUnderTest struct {
	name       string
	dimensions map[string][]string
	// relations is what this fixture witnesses about every PAIR of paths its door compares,
	// keyed by the pair as fixture_relations_test.go derives it. A dimension claim cannot see a
	// corpus whose dimensions are separately varied and JOINTLY degenerate, and both survivors
	// of the last round were exactly that shape.
	relations map[string]corpusPairVerdict
	leaves    []LeafIndex
	// vectors is the length of every vector dimension and clocks is every timestamp, each
	// keyed by path. They are what the length claim and the clock claim are stated over, and
	// they exist because those two claims used to read in.List and in.Now by name.
	vectors map[string]int
	clocks  map[string]time.Time
	order   []CachedProposal
}

// corpusFixtureReducedTo is one built fixture reduced to everything the claims below read, written
// once so that the two doors' loops cannot come to measure different things.
//
// IT TAKES THE INPUT AS an empty interface ON PURPOSE. A helper whose PARAMETER were a
// *CommitValidationInput would be fine; one whose RESULT were is a fixture builder as far as
// validationFixtureBuildersInSource is concerned -- that walk is over the result type, which is the
// whole of what makes it unfoolable -- so nothing here answers an input and only the reduction
// leaves the loop.
func corpusFixtureReducedTo(crypto CryptoProvider, name string, in any, again any,
	list *ProposalList, pairs []comparisonPair) corpusFixtureUnderTest {

	dimensions := corpusDimensionsOf(crypto, in)
	corpusListDimensionsInto(crypto, dimensions, list)
	vectors, clocks := corpusShapesOf(in)
	// the commit order is behind an unexported field with one accessor, so the vector it IS
	// does not appear in the walk above any more than its per-entry dimensions do
	vectors[corpusOrderPath] = len(list.All())
	relations := corpusRelationVerdictsOf(crypto, in, pairs)
	// the SECOND build of the same fixture, which is what says whether a value the two agreed
	// at is a value a constant in the source could have been. See corpusStableAgreementsIn.
	corpusStableAgreementsIn(relations, corpusRelationVerdictsOf(crypto, again, pairs))
	return corpusFixtureUnderTest{name: name, dimensions: dimensions, relations: relations,
		leaves: corpusLeafIndicesOf(in), vectors: vectors, clocks: clocks,
		order: list.All()}
}

// fixtureCorporaUnderMeasurement is the corpus of every door, keyed by the validation input it is a
// corpus of, each reduced to what the claims are stated over.
//
// A REGISTRY AND NOT TWO TESTS, because "the gate is pointed at one door" is the defect this round
// was sent to close and two tests is exactly the shape it takes: deleting one of them is a silent
// weakening that nothing else notices. Here the set of keys is joined to the set of validation
// input types the package's own source declares, in both directions, so a door with no entry fails
// and an entry naming no door fails.
//
// EACH ANSWERS WHAT IT ATTEMPTED as well as what it built, so a corpus half of which failed its own
// preconditions reports that rather than quietly stating every claim below over the remainder.
func fixtureCorporaUnderMeasurement() map[string]func(*testing.T,
	CryptoProvider) ([]corpusFixtureUnderTest, int) {

	return map[string]func(*testing.T, CryptoProvider) ([]corpusFixtureUnderTest, int){
		// KEYED BY THE TYPE ITSELF and not by its name spelled out, so that the join to
		// validationInputTypesInSource cannot be satisfied by a string that has drifted from
		// the type it names
		reflect.TypeFor[CommitValidationInput]().Name(): func(t *testing.T,
			crypto CryptoProvider) ([]corpusFixtureUnderTest, int) {

			measured := measureCommitCorpus(t, crypto)
			return measured.fixtures, measured.expected
		},
		reflect.TypeFor[ProposalValidationInput]().Name(): func(t *testing.T,
			crypto CryptoProvider) ([]corpusFixtureUnderTest, int) {

			corpus := proposalFixtureCorpus()
			pairs, _ := doorComparisonPairs(t)
			compared := pairs[reflect.TypeFor[ProposalValidationInput]().Name()]
			built := []corpusFixtureUnderTest{}
			for _, name := range slices.Sorted(maps.Keys(corpus)) {
				// EACH FIXTURE IS BUILT INSIDE ITS OWN SUBTEST, and that is not
				// tidiness. A builder answers t.Fatalf when its own precondition
				// stops holding, and a Fatalf in the middle of this loop takes the
				// whole test with it -- so the claims would report nothing about the
				// other fixtures, and the claim a corpus regression actually broke
				// would never be printed. One red row per fixture, and the
				// measurement carries on over the ones that built.
				//
				// TWICE, in the one subtest, because the every-constant claim
				// asks whether a value the two agreed at is one a constant could
				// be -- and the answer is whether building the same fixture again
				// reaches it again.
				var in, again *ProposalValidationInput
				t.Run(name, func(t *testing.T) {
					in = corpus[name].build(t, crypto)
					again = corpus[name].build(t, crypto)
				})
				if in == nil || again == nil {
					continue
				}
				built = append(built, corpusFixtureReducedTo(crypto, name, in, again,
					in.List, compared))
			}
			return built, len(corpus)
		},
	}
}

// commitCorpusMeasurement is the commit corpus built once and reduced to what the two claims over
// it read.
//
// ONE PASS FOR BOTH CLAIMS, and the reason is the derivation next door rather than the cost: a
// helper that answered a *CommitValidationInput would BE a fixture builder as far as
// validationFixtureBuildersInSource is concerned -- that walk is over the result type, which is the
// whole of what makes it unfoolable -- and the corpus would then owe itself a row. So the built
// inputs never leave this function.
type commitCorpusMeasurement struct {
	fixtures    []corpusFixtureUnderTest
	treesDiffer []string
	expected    int
}

// measureCommitCorpus builds every commit fixture, each INSIDE ITS OWN SUBTEST for the reason the
// proposal loop above gives, and reduces it.
func measureCommitCorpus(t *testing.T, crypto CryptoProvider) commitCorpusMeasurement {
	t.Helper()
	corpus := commitFixtureCorpus()
	pairs, _ := doorComparisonPairs(t)
	compared := pairs[reflect.TypeFor[CommitValidationInput]().Name()]
	measured := commitCorpusMeasurement{expected: len(corpus)}
	for _, name := range slices.Sorted(maps.Keys(corpus)) {
		// twice, for corpusStableAgreementsIn's reason, and in the one subtest
		var in, again *CommitValidationInput
		t.Run(name, func(t *testing.T) {
			in = corpus[name].build(t, crypto)
			again = corpus[name].build(t, crypto)
		})
		if in == nil || again == nil {
			continue
		}
		measured.fixtures = append(measured.fixtures,
			corpusFixtureReducedTo(crypto, name, in, again, in.List, compared))
		same, err := testTreesHashAlike(t, crypto, in.PreTree, in.PostTree)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !same {
			measured.treesDiffer = append(measured.treesDiffer, name)
		}
	}
	return measured
}

// assertCorpusSeparatesItsDimensions is the gate, written once and stated at both doors.
//
// SIX CLAIMS, AND FIVE OF THEM ARE CLASSES.
//
//	no dimension is one value across the corpus     -- the class itself
//	no pair the doors compare is jointly degenerate -- the second class, and see below
//	no vector dimension is always at most one long  -- a loop against a read of its head
//	no timestamp dimension is the wall clock        -- a field against a call to time.Now()
//	no LEAF INDEX anywhere fits in one octet        -- a comparator truncated to its low octet
//	the commit order is measured at all             -- the accessor call being dropped
//
// THE THIRD AND FOURTH USED TO NAME THEIR OWN FIELD, which is the same defect as a walk that names
// its types, one level up: "no fixture carries more than one proposal" read in.List and said
// nothing about any other vector, and "one fixture's clock is not the wall clock" read in.Now and
// said nothing about any other timestamp. Both are now stated over every vector and every timestamp
// corpusShapesOf reaches.
//
// THE FIFTH IS NOT WIDENED AND THE WORDING SAYS SO. "No other integer dimension fits in one octet"
// is not a claim this corpus could satisfy or should: ProtocolVersionMls10 is 1 and a registered
// ciphersuite code point is 1 or 2, so demanding every integer dimension exceed 255 would demand
// fixtures announcing values no build accepts. A LEAF INDEX is different in kind -- it is the only
// integer these doors decide IDENTITY by, it is the one whose comparator a truncation to its low
// octet leaves exact over every group that fits in one octet, and that truncation was measured
// green on this tree. So the claim is about leaf indices, by that argument, rather than about
// integers by an omission.
//
// THE RELATION CLAIM IS NOT AN INSTANCE OF THE DIMENSION CLAIM and this file's last round is the
// proof: Own took four values and Committer took four, so both were separated, and every fixture
// still had Own == 0 with Committer != 0 -- so ValSem203PathDecrypt's `in.Own == in.Committer` and
// the constant `in.Own == LeafIndex(0)` were the same program. Separation is not discrimination.
// Which pairs are demanded is derived off this package's own source; see
// fixture_relations_test.go.
//
// THE LAST FOUR ARE INSTANCES OF NEITHER OF THE FIRST TWO. A vector's length is not a value at any
// path -- two lists of different lengths are two different renderings and the dimension claim is
// satisfied by either -- the widest leaf index lives below the hop bound, and a corpus whose clocks
// are eleven distinct calls to time.Now() separates the Now dimension while leaving the field and
// the call the same program. Each is named with the mutation it exists to make fail, which is what
// the two classes give every other dimension and every other pair for free.
func assertCorpusSeparatesItsDimensions(t *testing.T, label string, expected int,
	fixtures []corpusFixtureUnderTest, compared []comparisonPair) {

	t.Helper()
	if len(fixtures) < expected {
		t.Errorf("%d of the %d fixtures in the %s corpus could not be built, so the claims below are stated over the rest of it",
			expected-len(fixtures), expected, label)
	}
	if len(fixtures) == 0 {
		t.Fatalf("the %s corpus is empty, so every claim below holds vacuously", label)
	}

	dimensions := map[string]map[string][]string{}
	longest := map[string]int{}
	furthestFromNow := map[string]time.Duration{}
	widest, widestIn := LeafIndex(0), ""
	carriesEntries := false
	for _, fixture := range fixtures {
		for path, length := range fixture.vectors {
			if length > longest[path] {
				longest[path] = length
			}
		}
		for path, at := range fixture.clocks {
			if apart := time.Since(at).Abs(); apart > furthestFromNow[path] {
				furthestFromNow[path] = apart
			}
		}
		for path, values := range fixture.dimensions {
			if dimensions[path] == nil {
				dimensions[path] = map[string][]string{}
			}
			for _, value := range values {
				dimensions[path][value] = append(dimensions[path][value], fixture.name)
			}
		}
		for _, at := range fixture.leaves {
			if at > widest {
				widest, widestIn = at, fixture.name
			}
		}
		carriesEntries = carriesEntries || len(fixture.order) > 0
	}

	if len(dimensions) == 0 {
		t.Fatalf("the walk found no dimension anywhere in the %s corpus, so it read something other than these inputs",
			label)
	}
	for _, path := range slices.Sorted(maps.Keys(dimensions)) {
		if len(dimensions[path]) > 1 {
			continue
		}
		only := slices.Sorted(maps.Keys(dimensions[path]))[0]
		if len(only) > 160 {
			only = only[:160] + "..."
		}
		t.Errorf("every fixture in the %s corpus carries %s = %s, so that field and that constant are the same program and no test here can tell a rule reading the field from a rule reading the constant. Give one fixture a different value for it",
			label, path, only)
	}
	t.Logf("%s: %d fixtures, %d dimensions, widest leaf index %d (in %s), %d vector dimension(s), %d timestamp dimension(s)",
		label, len(fixtures), len(dimensions), widest, widestIn, len(longest),
		len(furthestFromNow))

	// the narrowest integer width there is, which is what a truncated comparator collapses to
	if octet := LeafIndex(math.MaxUint8); widest <= octet {
		t.Errorf("the widest leaf index in the %s corpus is %d and one octet holds %d, so a comparison of leaf indices one octet wide is exact over every input here. Measured: the join's Sender comparator truncated to its low octet left the whole suite green",
			label, widest, octet)
	}
	if len(longest) == 0 {
		t.Errorf("the walk found no vector dimension anywhere in the %s corpus, so the length claim below holds vacuously",
			label)
	}
	for _, path := range slices.Sorted(maps.Keys(longest)) {
		if longest[path] > 1 {
			continue
		}
		t.Errorf("no fixture in the %s corpus carries more than one entry at %s -- the longest is %d -- so a loop over it and a read of its head answer alike, which is the shape four bypasses of the commit door took",
			label, path, longest[path])
	}
	if len(furthestFromNow) == 0 {
		t.Errorf("the walk found no timestamp anywhere in the %s corpus, so the clock claim below holds vacuously",
			label)
	}
	for _, path := range slices.Sorted(maps.Keys(furthestFromNow)) {
		if furthestFromNow[path] > corpusClockSeparation {
			continue
		}
		t.Errorf("every fixture in the %s corpus carries a %s within %s of now -- the furthest is %s away -- so that field and a call to time.Now() answer every lifetime alike and the field is the call. Give one fixture a clock a lifetime is decided differently under",
			label, path, corpusClockSeparation, furthestFromNow[path])
	}
	// corpusOrderPath is ONE NAME USED TWICE -- corpusListDimensionsInto writes the per-entry
	// dimensions under it and this reads them back -- rather than a class stated by
	// enumeration. There is nothing here to derive: the claim is that the accessor call still
	// happens, and the only evidence of that is dimensions appearing under the path the call
	// writes them to.
	measuresEntries := false
	for path := range dimensions {
		measuresEntries = measuresEntries || strings.HasPrefix(path, corpusOrderPath)
	}
	if carriesEntries && !measuresEntries {
		t.Errorf("fixtures in the %s corpus carry proposals and the measurement holds no dimension beneath %s, so the commit order is reached by nothing here. Its entries are behind an unexported field with one accessor, so losing that call loses every per-entry dimension -- the sender among them -- and no other claim would notice",
			label, corpusOrderPath)
	}
	assertCorpusSeparatesEveryRelationItsDoorDecidesBy(t, label, fixtures, compared)
}

// assertCorpusSeparatesEveryRelationItsDoorDecidesBy is the second class: for every pair of paths
// this door's rules compare, the corpus holds a fixture where the two are equal and one where they
// are not.
//
// WHY BOTH WITNESSES. A pair that is equal in every fixture cannot tell `a == b` from `a == a`, and
// one that is equal in none cannot tell it from `false`; either way the comparison and a constant
// are the same program over this corpus, which is the whole of what "the fixtures cannot see it"
// means. Requiring both is what makes the comparison a comparison.
//
// A PAIR THE CORPUS NEVER REACHES IS A FAILURE AND NOT A PASS. The alternative -- skipping a pair
// no fixture populates -- is a claim that goes green by the corpus getting emptier, which is the
// direction every regression here has taken. A rule reading a field no fixture fills is a rule
// nothing measures, and the derivation is what says the rule exists.
//
// AND SEPARABILITY FROM ONE CONSTANT IS NOT SEPARABILITY FROM EVERY CONSTANT, which is the third
// round of this same defect and the reason the two witnesses are no longer enough on their own. A
// pair that agrees at ONE value v and disagrees elsewhere tells `a == b` from `a == a` and from
// `false`, and tells it from nothing else: `a == v` answers true wherever the two agree and false
// wherever they do not, so the comparison and that constant are one program over this corpus.
// Measured on this tree: the previous round moved forty-nine call sites off LeafIndex(0) so that
// ValSem111 -- `updates[i].Sender == in.Committer` -- would stop being `== LeafIndex(0)`, and every
// fixture reaching that rule then carried Committer = 1. So it became `== LeafIndex(1)` instead,
// and this log read "6 of 6 pairs witnessed both equal and unequal" either way.
//
// TWO WAYS OUT AND THE SECOND IS THE CHEAP ONE. Either the corpus witnesses the two agreeing at
// more than one value -- then no single constant can stand in for either side -- or some fixture
// carries the agreement value on one side WHILE the two disagree, which is exactly the position at
// which `a == v` and `a == b` answer differently. The second is asked of each side separately,
// because `a == v` and `b == v` are two constant rules and a corpus can refuse one while admitting
// the other.
func assertCorpusSeparatesEveryRelationItsDoorDecidesBy(t *testing.T, label string,
	fixtures []corpusFixtureUnderTest, compared []comparisonPair) {

	t.Helper()
	if len(compared) == 0 {
		t.Errorf("no rule of this package was derived as comparing two paths of a %s, so the relation claim over its corpus holds vacuously",
			label)
		return
	}
	separated := 0
	// what says the SECOND build is measuring: agreement values it reached again and values it
	// did not. All of one and none of the other means corpusStableAgreementsIn compared a
	// fixture with itself, or with something unrelated, and the claim below is then either
	// never stated or stated over freshly generated keys.
	steady, moved := 0, 0
	for _, pair := range compared {
		equalIn, differIn, reachedIn := []string{}, []string{}, []string{}
		agreed, differLeft, differRight := []string{}, []string{}, []string{}
		for _, fixture := range fixtures {
			verdict := fixture.relations[pair.String()]
			if !verdict.reached {
				continue
			}
			reachedIn = append(reachedIn, fixture.name)
			if len(verdict.agreed) != 0 {
				equalIn = append(equalIn, fixture.name)
			}
			if len(verdict.differLeft) != 0 {
				differIn = append(differIn, fixture.name)
			}
			for _, value := range verdict.agreed {
				agreed = corpusWithValue(agreed, value)
			}
			for _, value := range verdict.differLeft {
				differLeft = corpusWithValue(differLeft, value)
			}
			for _, value := range verdict.differRight {
				differRight = corpusWithValue(differRight, value)
			}
		}
		// the side a single agreement value pins, or none where the corpus carries that
		// value on both sides while the two disagree.
		//
		// THE VALUE HAS TO BE ONE A CONSTANT COULD BE, which is what stable says and why
		// each fixture is built twice: two keys that collide in a fixture collide at
		// different octets every run, so no constant in the source is that value and there
		// is nothing to be separable from. See corpusStableAgreementsIn.
		stable := []string{}
		for _, fixture := range fixtures {
			for _, value := range fixture.relations[pair.String()].stable {
				stable = corpusWithValue(stable, value)
			}
		}
		for _, value := range agreed {
			if slices.Contains(stable, value) {
				steady += 1
			} else {
				moved += 1
			}
		}
		pinned := ""
		if len(agreed) == 1 && slices.Contains(stable, agreed[0]) {
			if !slices.Contains(differLeft, agreed[0]) {
				pinned = pair.left.String()
			} else if !slices.Contains(differRight, agreed[0]) {
				pinned = pair.right.String()
			}
		}
		switch {
		case len(reachedIn) == 0:
			t.Errorf("%s: %s is compared by %s and no fixture in the corpus reaches both of its sides, so that rule is stated over values nothing here carries",
				label, pair, pair.in)
		case len(equalIn) == 0:
			t.Errorf("%s: %s is compared by %s and the two are unequal in every fixture that reaches them (%v), so that comparison and the constant `false` are the same program here. Give the corpus a fixture where they agree -- if agreeing is what the rule refuses, the fixture's row owes the sentinel it is refused by",
				label, pair, pair.in, reachedIn)
		case len(differIn) == 0:
			t.Errorf("%s: %s is compared by %s and the two are equal in every fixture that reaches them (%v), so that comparison and a comparison of either side against itself are the same program here. Give the corpus a fixture where they disagree",
				label, pair, pair.in, reachedIn)
		case pinned != "":
			t.Errorf("%s: %s is compared by %s and the two agree at ONE value across the corpus -- %s -- with no fixture carrying that value at %s while they disagree. So that comparison and `%s == <that value>` are the same program here, exactly as the last round left ValSem111 the same program as `== LeafIndex(1)`. Give the corpus a fixture where the two agree at a SECOND value, or one where %s carries this one and the other side does not",
				label, pair, pair.in, corpusShortly(agreed[0]), pinned, pinned, pinned)
		default:
			separated += 1
		}
	}
	if steady == 0 || moved == 0 {
		t.Errorf("the %s corpus witnesses %d agreement value(s) a second build reached again and %d it did not, and the claim above needs both: with none of the first it is never stated, and with none of the second it is stated over encryption keys that are different octets every run. Building each fixture twice is what tells those apart",
			label, steady, moved)
	}
	t.Logf("%s: %d of %d compared pairs are witnessed equal, unequal, and separably from every constant; %d agreement value(s) survive a second build and %d do not",
		label, separated, len(compared), steady, moved)
}

// TestEveryValidationInputCorpusSeparatesEveryDimensionItDecidesOff is the gate the four rounds
// needed, stated over every door this package has rather than over the one it was first written
// for.
func TestEveryValidationInputCorpusSeparatesEveryDimensionItDecidesOff(t *testing.T) {
	crypto := testCrypto(t)
	declared := validationInputTypesInSource(t)
	if len(declared) < 2 {
		t.Fatalf("this package's source declares %v validation input types; the derivation read something other than the package, and a gate that measures one door is what this round was sent to close",
			declared)
	}
	corpora := fixtureCorporaUnderMeasurement()
	pairs, _ := doorComparisonPairs(t)
	for _, name := range declared {
		measure, measured := corpora[name]
		if !measured {
			t.Errorf("this package declares %s and no corpus in this file measures one, so every field of that input is a field nothing holds to being separable from a constant",
				name)
			continue
		}
		fixtures, expected := measure(t, crypto)
		assertCorpusSeparatesItsDimensions(t, name, expected, fixtures, pairs[name])
	}
	for _, name := range slices.Sorted(maps.Keys(corpora)) {
		if !slices.Contains(declared, name) {
			t.Errorf("this file measures a corpus of %s and this package declares no such type", name)
		}
	}
}

// TestTheCommitCorpusIsJudgedBetweenTwoTreesThatDiffer is the one claim only the commit door can
// make: a commit is judged BETWEEN two trees, and a corpus in which those two always agree is one
// where a rule stated over either answers the same thing.
func TestTheCommitCorpusIsJudgedBetweenTwoTreesThatDiffer(t *testing.T) {
	crypto := testCrypto(t)
	if len(measureCommitCorpus(t, crypto).treesDiffer) == 0 {
		t.Errorf("every fixture in this corpus hands PostTree a tree that hashes the same as PreTree, so a rule stated over either answers the same thing and the two fields are one. Measured: three tree reads of validate_commit.go could be swapped with the whole suite green")
	}
}

// ---------------------------------------------------------------------------
// the corpora, driven through the doors they are corpora for
// ---------------------------------------------------------------------------

// fixtureCorporaUnderTheirDoors is every corpus DRIVEN, keyed by the validation input it is a
// corpus of, and it is a registry for fixtureCorporaUnderMeasurement's reason: two tests, one of
// which gets deleted, is the shape this defect takes, and the set of keys is joined below to the
// set of input types this package's own source declares.
//
// IT IS A SEPARATE REGISTRY FROM THE MEASUREMENT because the two answer different questions and one
// of them used to be asked at one door only. Measuring a corpus says a value is separable from a
// constant; DRIVING it is what makes some test notice the difference. Until this round the commit
// corpus was measured and never driven -- ten of its fourteen fixtures were built by nothing but
// the dimension walk -- so varying it changed no verdict anywhere, which is why all three of the
// last round's survivors were commit-door reads.
func fixtureCorporaUnderTheirDoors() map[string]func(*testing.T, CryptoProvider) (int, int) {
	return map[string]func(*testing.T, CryptoProvider) (int, int){
		reflect.TypeFor[CommitValidationInput]().Name(): func(t *testing.T,
			crypto CryptoProvider) (int, int) {

			return assertCorpusIsJudgedTheWayItsRowsSay(t, crypto, commitFixtureCorpus(),
				ValidateCommit)
		},
		reflect.TypeFor[ProposalValidationInput]().Name(): func(t *testing.T,
			crypto CryptoProvider) (int, int) {

			return assertCorpusIsJudgedTheWayItsRowsSay(t, crypto, proposalFixtureCorpus(),
				ValidateProposalList)
		},
	}
}

// assertCorpusIsJudgedTheWayItsRowsSay drives one corpus through its door and answers how many
// fixtures it drove and how many of them the door had to refuse.
//
// SEPARATING A DIMENSION DOES NOT BY ITSELF MAKE ANY TEST NOTICE IT. The gate above measures
// fixtures and calls no door; a read becomes decidable only when some test drives an input whose
// value differs from the constant AND asserts what comes back. This is that test, and the corpus is
// exactly the set of inputs it is stated over -- so a fixture added to separate a dimension or a
// relation is also a fixture the door is held to, and a fixture that quietly stops being accepted
// is a failure here rather than a silent weakening of the corpus.
//
// EACH FIXTURE IN ITS OWN SUBTEST for the reason the measurement loop gives: a builder answers
// t.Fatalf when its own precondition stops holding, and one such failure must not take the other
// rows' verdicts with it.
func assertCorpusIsJudgedTheWayItsRowsSay[Input any](t *testing.T, crypto CryptoProvider,
	corpus map[string]validationFixtureRow[Input], door func(*Input) error) (int, int) {

	t.Helper()
	driven, refused := 0, 0
	for _, name := range slices.Sorted(maps.Keys(corpus)) {
		row := corpus[name]
		if row.refuses != nil {
			refused += 1
		}
		var in *Input
		t.Run(name, func(t *testing.T) { in = row.build(t, crypto) })
		if in == nil {
			continue
		}
		driven += 1
		t.Run(name+"/verdict", func(t *testing.T) {
			failure := door(in)
			switch {
			case row.refuses == nil && failure != nil:
				t.Fatalf("the door refused this fixture: %v. A fixture no door accepts measures nothing about the doors; if the refusal is correct the row owes a sentinel, and if it is not the fixture is wrong",
					failure)
			case row.refuses != nil && failure == nil:
				t.Fatalf("the door accepted this fixture and its row says it must answer %v",
					row.refuses)
			case row.refuses != nil && !errors.Is(failure, row.refuses):
				t.Fatalf("the door answered %v and this fixture's row says %v; a fixture refused for another reason is one that no longer reaches the rule it was built for",
					failure, row.refuses)
			}
		})
	}
	return driven, refused
}

// TestEveryValidationInputCorpusIsJudgedTheWayItsRowsSay drives every corpus through its own door.
//
// AND HOLDS EACH ONE TO CARRYING A REFUSAL. A corpus of accepted inputs alone cannot witness the
// agreeing side of any rule that refuses on equality -- ValSem104, ValSem111, ValSem204, ValSem206,
// validateCommitterIsNotRemoved and validateUpdateChangesTheEncryptionKey are six of them -- so a
// corpus with no refusing row is one whose relation claims can only ever be half stated. That is
// what the commit corpus was until this round: rows with no verdict field at all.
func TestEveryValidationInputCorpusIsJudgedTheWayItsRowsSay(t *testing.T) {
	crypto := testCrypto(t)
	declared := validationInputTypesInSource(t)
	if len(declared) < 2 {
		t.Fatalf("this package's source declares %v validation input types; the derivation read something other than the package",
			declared)
	}
	driven := fixtureCorporaUnderTheirDoors()
	for _, name := range declared {
		drive, drives := driven[name]
		if !drives {
			t.Errorf("this package declares %s and no corpus in this file is driven through its door, so every fixture of that door is measured and judged by nothing",
				name)
			continue
		}
		built, refused := drive(t, crypto)
		if built == 0 {
			t.Errorf("no fixture of the %s corpus could be built, so its door was asked nothing", name)
		}
		if refused == 0 {
			t.Errorf("no row of the %s corpus names a refusal, so no fixture of it can witness a rule that refuses on equality agreeing, and every such rule stays indistinguishable from `return nil`",
				name)
		}
		t.Logf("%s: %d fixtures driven, %d of them refused by their row", name, built, refused)
	}
	for _, name := range slices.Sorted(maps.Keys(driven)) {
		if !slices.Contains(declared, name) {
			t.Errorf("this file drives a corpus of %s and this package declares no such type", name)
		}
	}
}

// ---------------------------------------------------------------------------
// the POPULATION the rules are actually driven over
// ---------------------------------------------------------------------------
//
// A CORPUS IS NOT A POPULATION. Everything above is stated over the registry rows, and the inputs
// this package's rules are actually driven with are built at call sites -- fifty-two of
// testValidationInput alone, of which forty-nine passed the committer as `LeafIndex(0)`. A perfect
// corpus and a door whose own per-rule tests are every one of them judged at leaf zero coexist
// happily: measured, ValSem111's `updates[i].Sender == in.Committer` could be replaced by
// `== LeafIndex(0)` and survive every test written against that rule, dying only in a gate about
// bucket positions that had no interest in the committer at all.
//
// SO THE CALL SITES ARE HELD TOO, and the claim over them is stated as a property of the SPELLING
// rather than as a count. A number written into a call site says which leaf, which width or which
// epoch the fixture uses and says nothing about WHY; a named constant is a place to put the why,
// and -- this is the half that matters here -- it is a single place to change when the answer
// stops being right. `LeafIndex(0)` at forty-nine sites was forty-nine independent decisions
// nobody had made; testCommitterLeaf at forty-nine sites is one.

// validationInputBuildersInSource is every function this package's test files declare that answers
// a validation input of ANY type this package declares, which is the population's own class.
func validationInputBuildersInSource(t *testing.T) []string {
	t.Helper()
	found := []string{}
	for _, named := range validationInputTypesInSource(t) {
		found = append(found, validationFixtureBuildersInSource(t, named)...)
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// packageDeclaredTypeNames is every type name a conversion in this package can be spelled with:
// the types the package declares, and the numeric builtins.
//
// IT IS WHAT TELLS A CONVERSION FROM A CALL. `LeafIndex(0)` and `testRemoveOf(0)` are the same AST
// shape, and only one of them is a number wearing a type; a walk that could not tell them apart
// would either miss every typed literal or refuse every fixture helper that takes one.
func packageDeclaredTypeNames(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, name := range []string{"int", "int8", "int16", "int32", "int64", "uint", "uint8",
		"uint16", "uint32", "uint64", "uintptr", "byte", "rune", "float32", "float64"} {
		found[name] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declared := range file.Decls {
			general, isGeneral := declared.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				if typed, isTyped := spec.(*ast.TypeSpec); isTyped {
					found[typed.Name.Name] = true
				}
			}
		}
	}
	return found
}

// isBareNumber answers whether an argument is A NUMBER WITH NO NAME: a numeric literal, or a
// conversion of one to a type this package declares.
func isBareNumber(expr ast.Expr, types map[string]bool) bool {
	switch node := expr.(type) {
	case *ast.BasicLit:
		return node.Kind == token.INT || node.Kind == token.FLOAT
	case *ast.ParenExpr:
		return isBareNumber(node.X, types)
	case *ast.CallExpr:
		ident, isIdent := node.Fun.(*ast.Ident)
		if !isIdent || !types[ident.Name] || len(node.Args) != 1 {
			return false
		}
		return isBareNumber(node.Args[0], types)
	}
	return false
}

// TestNoValidationInputIsBuiltFromANumberWithNoName holds the population.
//
// DERIVED IN BOTH DIRECTIONS: the builders are read off the result type, the type names that make a
// conversion a conversion are read off the package's own declarations, and the call sites are read
// off every test file. A builder written tomorrow is measured on the run after it is written.
//
// WHAT IT REFUSES is a literal at a fixture's call site, whatever the literal means. That is wider
// than the leaf indices this round was sent for, and deliberately: the epoch a fixture runs in, the
// width of the group it builds and the position it puts an offender at are the same kind of
// decision, and each of them has been a pinned dimension in this package at some point.
func TestNoValidationInputIsBuiltFromANumberWithNoName(t *testing.T) {
	builders := validationInputBuildersInSource(t)
	if len(builders) == 0 {
		t.Fatalf("no function in this package's test files answers a validation input, so this claim is stated over nothing")
	}
	types := packageDeclaredTypeNames(t)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	fileSet := token.NewFileSet()
	called := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			ident, isIdent := call.Fun.(*ast.Ident)
			if !isIdent || !slices.Contains(builders, ident.Name) {
				return true
			}
			called += 1
			for at, argument := range call.Args {
				if !isBareNumber(argument, types) {
					continue
				}
				t.Errorf("%s:%d: %s is handed a number with no name at argument %d. A literal at a fixture call site is a decision about which leaf, which epoch or which width this input runs at that nobody wrote down, and it is the shape the corpus kept drifting back into: the committer was spelled LeafIndex(0) at forty-nine of fifty-two call sites of testValidationInput and no test anywhere said why. Name it",
					entry.Name(), fileSet.Position(argument.Pos()).Line, ident.Name, at)
			}
			return true
		})
	}
	if called == 0 {
		t.Errorf("no call to any of the %d validation input builders %v was found in this package's test files, so the walk read something other than the call sites",
			len(builders), builders)
	}
	t.Logf("%d builders, %d call sites", len(builders), called)
	assertBareNumbersAreRecognised(t, types)
}

// assertBareNumbersAreRecognised is the control the claim above needs to mean anything.
//
// A MATCHER THAT RECOGNISES NOTHING PASSES EVERY CALL SITE, and it does so silently: measured, the
// literal arm of isBareNumber replaced by `false` left the gate above green over the whole of
// ./mls/... and ./message/..., because a gate whose only output is "no offenders" cannot tell a
// clean package from a walk that read nothing. So the matcher is driven over text that is known to
// hold one of each kind, and over text that is known to hold none.
//
// THE NEGATIVE HALF IS THE OTHER ERROR. A matcher that answered true for everything would also
// pass no call site -- it would fail all of them -- but the failure a reviewer would then reach for
// is to loosen the claim rather than to fix the matcher, so the shapes a fixture legitimately hands
// a builder are held to being accepted: a named constant, a helper call carrying a number, and an
// ordinary local.
func assertBareNumbersAreRecognised(t *testing.T, types map[string]bool) {
	t.Helper()
	for _, one := range []struct {
		source string
		bare   bool
	}{
		{source: "0", bare: true},
		{source: "9", bare: true},
		{source: "LeafIndex(0)", bare: true},
		{source: "(LeafIndex(258))", bare: true},
		{source: "uint32(3)", bare: true},
		{source: "testCommitterLeaf", bare: false},
		{source: "testWideCommitterLeaf", bare: false},
		{source: "testRemoveOf(LeafIndex(3))", bare: false},
		{source: "tree", bare: false},
		{source: "testProposalList(t, held)", bare: false},
	} {
		parsed, err := parser.ParseExpr(one.source)
		if err != nil {
			t.Fatalf("parse the control %q: %v", one.source, err)
		}
		if answered := isBareNumber(parsed, types); answered != one.bare {
			t.Errorf("the matcher reads %s as bare=%v and it is bare=%v; a matcher that recognises nothing passes every call site of every builder and says so in exactly the words a clean package says",
				one.source, answered, one.bare)
		}
	}
}

// testTreesHashAlike answers whether two trees are the same tree by the one identity a ratchet tree
// has, which is its tree hash -- the value a GroupContext carries and a transcript covers.
func testTreesHashAlike(t *testing.T, crypto CryptoProvider, first *RatchetTree,
	second *RatchetTree) (bool, error) {

	t.Helper()
	if first == nil || second == nil {
		return false, fmt.Errorf("a fixture carries no tree in one of its two tree fields")
	}
	before, err := first.TreeHash(crypto)
	if err != nil {
		return false, fmt.Errorf("the pre tree's hash: %w", err)
	}
	after, err := second.TreeHash(crypto)
	if err != nil {
		return false, fmt.Errorf("the post tree's hash: %w", err)
	}
	return slices.Equal(before, after), nil
}
