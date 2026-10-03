// The join between a commit's ProposalOrRef vector and the list resolved from it, and the class
// that join is derived over.
//
// FOUR ROUNDS OF THIS DOOR EACH REPAIRED THE INPUT THEY WERE HANDED AND LEFT THE CLASS. The
// buckets were joined to nothing, then by a per-type count, then by a ProposalType, then the
// by-value entries by their octets -- and after each round a peer could still move a field of a
// CachedProposal that no comparison read. The class is "a join that compares a proxy for the thing
// rather than the thing", and what closes it is not a fifth comparison but a join whose coverage is
// the TYPE: joinCachedProposals walks the fields of a CachedProposal and refuses one it has no
// comparison for. The gates here are that claim in both directions, and the probes below are the
// two the owner verified against the fourth round -- both of which this join refuses without
// anything having been written for either.
package mls

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// the join covers the type
// ---------------------------------------------------------------------------

// TestEveryFieldOfACachedProposalIsJoinedToTheCommitsOwnVector is the coverage claim, in both
// directions and off the type rather than off a list.
//
// WHY THE TYPE IS THE RIGHT CLASS AND NOT MERELY A CONVENIENT ONE. What the join owes is the set of
// facts the consumers of a resolved list decide off: ApplyProposals writes an Update into the leaf
// Sender names, reads the Add, Remove and GroupContextExtensions arms under Proposal, and the rules
// of section 12.2 read ByValue and Ref. Every one of those is a field of this struct or lies under
// one, so the struct's field set is a SUPERSET of what any consumer can read and stays one however
// the consumers are edited -- which is what makes it computable once here instead of recomputed
// from the readers, where it would be wrong for exactly as long as somebody had moved a reader and
// not this gate.
//
// BOTH DIRECTIONS. A field with no comparison is the defect this whole file exists for. A
// comparison for a name the struct no longer carries is a row that outlived the field it described,
// which is the shape that leaves the next reader believing something is joined when it is not.
func TestEveryFieldOfACachedProposalIsJoinedToTheCommitsOwnVector(t *testing.T) {
	entry := reflect.TypeFor[CachedProposal]()
	declared := []string{}
	for _, field := range reflect.VisibleFields(entry) {
		declared = append(declared, field.Name)
	}
	if len(declared) == 0 {
		t.Fatal("a CachedProposal declares no field, so this gate read something other than the entry type and the coverage below is over nothing")
	}
	joined := []string{}
	for name := range cachedProposalJoin {
		joined = append(joined, name)
	}
	slices.Sort(declared)
	slices.Sort(joined)
	if !slices.Equal(declared, joined) {
		t.Errorf("a CachedProposal carries %v and the join compares %v; a field with no comparison is one a peer moves with every rule of the commit door still answering yes, and a comparison with no field is a row that outlived what it described",
			declared, joined)
	}
	// AND NO FIELD IS COMPARED THROUGH A CONVERSION THAT COULD LOSE IT, asserted over the
	// COMPARISON rather than over the type.
	//
	// WHAT STOOD HERE WAS A FACT ABOUT THE TYPE: `reflect.TypeFor[LeafIndex]().Size() > 8`. That
	// is false in every build this package will ever have -- Go has no integer wider than eight
	// octets -- so the clause could not fail, and it says nothing whatever about the function that
	// does the comparing. Measured: with the octets rewritten to AppendUint32, and again to the
	// low octet alone, the clause stayed silent and the whole suite stayed green.
	//
	// THE TWO PROPERTIES A COMPARATOR OVER AN INDEX OWES are stated instead, and both are derived.
	// It must SEPARATE every value the type can hold, which is one probe per bit of the type's own
	// width; and it must be wide enough for every width the type could be GIVEN, which is the
	// widest integer Go has rather than the width it has today. The first fails on a truncation
	// inside the current type -- the low octet alone collapses leaf 256 onto leaf 0 -- and the
	// second fails on AppendUint32, which is exact today and is the low half of a LeafIndex the
	// moment somebody widens the declaration. Both were applied and both fail here now.
	sender, compared := cachedProposalJoin["Sender"]
	if !compared {
		t.Fatal("the join has no comparison for Sender, so the width claims below are about nothing")
	}
	octetsOf := func(at LeafIndex) []byte {
		t.Helper()
		got, err := sender.octets(&CachedProposal{Sender: at})
		if err != nil {
			t.Fatalf("the Sender row cannot read leaf %d: %v", at, err)
		}
		return got
	}
	index := reflect.TypeFor[LeafIndex]()
	if kind := index.Kind(); kind < reflect.Uint || kind > reflect.Uint64 {
		t.Fatalf("a LeafIndex is a %s and the two claims below are derived from its being an unsigned integer",
			kind)
	}
	zero := octetsOf(LeafIndex(0))
	for bit := 0; bit < int(index.Size())*8; bit += 1 {
		at := LeafIndex(1) << bit
		if slices.Equal(octetsOf(at), zero) {
			t.Errorf("the join reads leaf %d and leaf 0 as the same octets; bit %d of a LeafIndex is not compared, so two leaves that differ only there join as one",
				at, bit)
		}
	}
	if widest := int(reflect.TypeFor[uint64]().Size()); len(zero) < widest {
		t.Errorf("the join reads a LeafIndex as %d octets and the widest integer a LeafIndex could be declared as is %d; the comparison is over the low %d of it the moment the declaration moves, which is this door's own defect class spelled as an integer conversion",
			len(zero), widest, len(zero))
	}
	// and the production walk really is over that set rather than over a slice built beside it
	walked := []string{}
	for _, field := range cachedProposalFields {
		walked = append(walked, field.Name)
	}
	slices.Sort(walked)
	if !slices.Equal(walked, declared) {
		t.Errorf("joinCachedProposals walks %v and a CachedProposal carries %v; the walk is the whole of what makes this join cover the type rather than a list",
			walked, declared)
	}
}

// TestAFieldOfACachedProposalWithNoJoinRefusesTheCommit drives the branch that makes the walk a
// refusal rather than a skip.
//
// UNREACHABLE IN THIS BUILD, WHICH IS WHY IT IS PERFORMED. The gate above holds the join equal to
// the type, so no input reaches this line; a build that grew a fifth field is what makes it
// reachable, and a branch that is only reasoned about is one the next edit deletes. The row is
// taken out for the length of this test, which is
// TestABucketlessAcceptedTypeIsRefusedRatherThanSilentlyDropped's own shape one file over.
//
// AND THE REFUSAL IS THE POINT RATHER THAN THE PANIC IT REPLACES. A join that skipped a field it
// had no comparison for would accept a commit whose two representations disagree about that field
// -- which is every round of this door's history -- so a build that adds a field and stops half way
// refuses every commit carrying a proposal until somebody finishes it.
func TestAFieldOfACachedProposalWithNoJoinRefusesTheCommit(t *testing.T) {
	crypto := testCrypto(t)
	in := testCommitCarryingOneOfEveryBucket(t, crypto)
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the commit this test takes one row out of the join over: %v", failure)
	}
	for _, field := range cachedProposalFields {
		t.Run(field.Name, func(t *testing.T) {
			restore := testJoinWithoutTheFieldRow(t, field.Name)
			defer restore()
			failure := ValidateCommit(in)
			if !errors.Is(failure, errCachedProposalFieldNotJoined) {
				t.Fatalf("with no comparison for %s the door answered %v, want errCachedProposalFieldNotJoined; a field the join cannot compare is one it must refuse over rather than walk past",
					field.Name, failure)
			}
			if !strings.Contains(failure.Error(), field.Name) {
				t.Errorf("the refusal is %v and does not name %s, so a reader is told a field is missing and not which one",
					failure, field.Name)
			}
		})
	}
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("the join was not restored: ValidateCommit now answers %v", failure)
	}
}

// ---------------------------------------------------------------------------
// the by-reference arm names a body, not just a name
// ---------------------------------------------------------------------------

// testCommitNamingACachedRemove is a commit whose SECOND entry names a remove this member holds,
// with an add carried inline ahead of it.
//
// The order is the point, and it is testCommitNamingACachedProposal's: a rule written over entry
// zero reads the inline add and never reaches the reference. It is built here rather than reused
// because these rows need the cache back to edit what it holds.
// testHeldRemoveTarget is the leaf the cached remove below names, and it is a member of the four
// member group that is neither the committer nor the member judging the commit.
//
// NAMED RATHER THAN WRITTEN AT EACH CALL SITE, which is TestNoValidationInputIsBuiltFromANumberWithNoName's
// rule: four call sites each spelling LeafIndex(2) are four decisions nobody made, and the one that
// matters here is that removing the committer or the judge would be refused by a rule that has
// nothing to do with the vector join these fixtures are about.
const testHeldRemoveTarget = LeafIndex(2)

func testCommitNamingACachedRemove(t *testing.T, crypto CryptoProvider,
	removed LeafIndex) (*CommitValidationInput, *ProposalCache, CachedProposal) {

	t.Helper()
	in, _ := testFullCommitInput(t, crypto)
	cache := testCacheAt(t, testResolveContext())
	in.Pending = cache
	kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	held := testCachedRemoveOf(t, crypto, cache, removed)
	in.List = testProposalList(t, testAddOf(kp), held)
	in.Commit.Proposals = in.List.Refs()
	return in, cache, held
}

// TestACommitWhoseReferenceNamesOneProposalWhileItsListHoldsAnother is the first of the two probes
// the owner verified against the fourth round of this join, and it was ACCEPTED.
//
// THE INPUT. The cache holds a remove of leaf 2 under one reference. The commit's vector names that
// reference. The list's entry at that position carries the same Ref, the same ByValue, the same
// Sender and the same per-type counts -- and a remove of leaf 3 instead of the remove of leaf 2 the
// reference names. Nothing a length, a count, a type or the reference itself can see has moved.
//
// WHY THE OLD ARM COULD NOT SEE IT. A ProposalRef is a hash over the framed proposal the SENDER
// published, so it identifies the CACHE'S entry; it says nothing whatever about what somebody put
// beside it in a list. Comparing cached.Ref with vector[i].Reference established that the two
// spelled the same name and stopped there. The header of the round that wrote it said "AN IDENTITY
// ON BOTH ARMS" and it was an identity on one.
//
// WHAT ACCEPTING IT COSTS is asserted here rather than described: ApplyProposals over the list
// removes leaf 3, while the transcript this member goes on to confirm covers a commit that removes
// leaf 2. One member applying a different commit from the one the group agreed to, reachable by a
// peer sending it.
func TestACommitWhoseReferenceNamesOneProposalWhileItsListHoldsAnother(t *testing.T) {
	crypto := testCrypto(t)
	const named = LeafIndex(2)
	const applies = LeafIndex(3)
	in, _, held := testCommitNamingACachedRemove(t, crypto, named)
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the commit this test is one edit away from: %v", failure)
	}
	at := -1
	for i := range in.Commit.Proposals {
		if in.Commit.Proposals[i].Type == ProposalOrRefTypeReference {
			at = i
			break
		}
	}
	if at < 1 {
		t.Fatalf("the fixture's first by-reference entry is at %d; a probe placed at element zero is one a rule narrowed to the first entry refuses anyway",
			at)
	}
	before := len(in.List.All())
	counts := map[string]int{}
	for _, bucket := range proposalBucketsOf(in.List) {
		counts[bucket.accessor] = len(bucket.entries)
	}

	// A FRESH ARM, so the cache's own entry is not moved with the list's. The list and the cache
	// hold clones of one proposal, so writing through Remove.Removed would move the entry the
	// reference names as well and leave the two agreeing about something else.
	entry := testListEntryAt(t, in.List, "Removes", 0)
	entry.Proposal.Remove = &Remove{Removed: applies}

	if in.Commit.Proposals[at].Type != ProposalOrRefTypeReference ||
		len(in.Commit.Proposals[at].Reference) == 0 {
		t.Fatal("the edit moved the commit's own vector, so there is no disagreement here to refuse")
	}
	if cached, ok := in.Pending.Cached(in.Context, held.Ref); !ok ||
		cached.Proposal.Remove.Removed != named {
		t.Fatalf("the cache no longer holds a remove of leaf %d under the reference the commit names, so the edit moved both halves",
			named)
	}
	if after := len(in.List.All()); after != before {
		t.Fatalf("the edit changed the commit order from %d entries to %d; it is not the count-preserving swap this test is about",
			before, after)
	}
	for _, bucket := range proposalBucketsOf(in.List) {
		if got := len(bucket.entries); got != counts[bucket.accessor] {
			t.Fatalf("the edit changed the %s view from %d entries to %d; a per-type count would have caught this input",
				bucket.accessor, counts[bucket.accessor], got)
		}
	}
	if got := in.List.All()[at].Ref; len(got) == 0 || !slices.Equal(got, held.Ref) {
		t.Fatalf("the list's entry no longer carries the reference the commit names, so the arm that compares the two would refuse this input and it is not the one this test is about")
	}

	// what accepting it costs, through the door that applies what this one judges
	applied, err := ApplyProposals(in.PreTree, in.Context, in.Committer, in.List)
	if err != nil {
		t.Fatalf("ApplyProposals over the edited list: %v", err)
	}
	if len(applied.RemovedLeaves) != 1 || applied.RemovedLeaves[0] != applies {
		t.Fatalf("ApplyProposals over the edited list removed %v, want [%d]; this test is not driving the divergence it is named for",
			applied.RemovedLeaves, applies)
	}

	failure := ValidateCommit(in)
	if !errors.Is(failure, errCommitProposalsNotResolved) {
		t.Fatalf("ValidateCommit over a commit whose reference names a remove of leaf %d while its list holds a remove of leaf %d = %v, want errCommitProposalsNotResolved; the apply door above removes leaf %d, so a member that accepts this applies a commit the transcript does not cover",
			named, applies, failure, applies)
	}
	if !strings.Contains(failure.Error(), "Proposal") {
		t.Errorf("the refusal is %v and does not say which field disagreed; the reference, the sender and the arm all agree here and only the body does",
			failure)
	}
}

// TestACommitWhoseReferenceNamesABodyDifferingInOneOctetIsRefused is the same probe at the
// smallest difference the encoding has.
//
// A remove of leaf 2 and a remove of leaf 258 differ in exactly one octet of their wire form -- the
// leaf index is a uint32 and only its third byte moves -- so a join comparing anything coarser than
// the octets accepts this. It is separated from the row above because the row above changes a leaf
// index a reader can see; this one says the comparison is over the encoding rather than over
// whatever a reader would have thought to look at.
func TestACommitWhoseReferenceNamesABodyDifferingInOneOctetIsRefused(t *testing.T) {
	crypto := testCrypto(t)
	const named = LeafIndex(2)
	const applies = LeafIndex(258)
	in, _, held := testCommitNamingACachedRemove(t, crypto, named)
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the commit this test is one octet away from: %v", failure)
	}
	signed, err := proposalOctets(&held.Proposal)
	if err != nil {
		t.Fatalf("encode the cached remove: %v", err)
	}
	testListEntryAt(t, in.List, "Removes", 0).Proposal.Remove = &Remove{Removed: applies}
	moved, err := proposalOctets(&in.List.All()[1].Proposal)
	if err != nil {
		t.Fatalf("encode the list's remove: %v", err)
	}
	if len(signed) != len(moved) {
		t.Fatalf("the two encodings are %d and %d octets long, so this is not the one-octet difference the test is named for",
			len(signed), len(moved))
	}
	differ := 0
	for i := range signed {
		if signed[i] != moved[i] {
			differ += 1
		}
	}
	if differ != 1 {
		t.Fatalf("the two encodings differ in %d octets, want 1; the fixture is not the smallest difference this join has to see",
			differ)
	}
	if failure := ValidateCommit(in); !errors.Is(failure, errCommitProposalsNotResolved) {
		t.Fatalf("ValidateCommit over a list whose body differs from the one its reference names in one octet = %v, want errCommitProposalsNotResolved",
			failure)
	}
}

// ---------------------------------------------------------------------------
// the sender is joined, on both arms
// ---------------------------------------------------------------------------

// TestAnInlineProposalAttributedToAnotherLeafIsRefused is the second probe the owner verified, and
// it was ACCEPTED -- with the door's own fixture in the same state.
//
// THE RULE IT BREAKS IS RESOLVE'S. A by-value entry is attributed to the COMMITTER: that is what
// (*ProposalCache).Resolve does and it is the whole of what makes ValSem111 -- the committer must
// not cover its own Update -- a comparison against anything real. So a list holding an inline
// Update whose Sender is leaf 2 under a committer at leaf 0 is a list no resolution of any commit
// can produce, and neither arm of this join mentioned Sender at all.
//
// WHAT ACCEPTING IT COSTS is apply_proposals.go line 131 verbatim:
// `result.Tree.UpdateLeaf(cached.Sender, &cached.Proposal.Update.LeafNode)`. The committer's own
// leaf node is written into whatever leaf the list names, which is a member replaced by a leaf its
// owner did not publish -- asserted below rather than described.
func TestAnInlineProposalAttributedToAnotherLeafIsRefused(t *testing.T) {
	crypto := testCrypto(t)
	in, members := testFullCommitInput(t, crypto)
	const wrong = LeafIndex(2)
	if in.Committer == wrong {
		t.Fatalf("the fixture's committer is leaf %d, so attributing the update to it is not a disagreement", wrong)
	}
	// the committer's own update, carried inline, which is the only inline update a resolution
	// could produce -- and then attributed to somebody else's leaf.
	//
	// BEHIND AN INNOCENT ADD, which is this file's own header requirement and was violated here:
	// the list carried one entry, so the join's walk over the vector and a read of its head
	// answered alike and the whole rule could be narrowed to entry zero with this test green.
	joiner, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "erin"))
	update, leaf := testUpdateProposalOf(t, crypto, members[in.Committer], in.Committer)
	update.Sender = wrong
	testCommitProposals(t, in, testAddOf(joiner), update)
	if at := len(in.List.All()) - 1; at < 1 {
		t.Fatalf("the offending entry is at %d of %d; a probe at element zero is one a rule narrowed to the first entry refuses anyway",
			at, len(in.List.All()))
	}

	applied, err := ApplyProposals(in.PreTree, in.Context, in.Committer, in.List)
	if err != nil {
		t.Fatalf("ApplyProposals over the list: %v", err)
	}
	if len(applied.UpdatedLeaves) != 1 || applied.UpdatedLeaves[0] != wrong {
		t.Fatalf("ApplyProposals wrote the update into %v, want [%d]; this test is not driving the divergence it is named for",
			applied.UpdatedLeaves, wrong)
	}
	written := applied.Tree.Leaf(wrong)
	if written == nil || !slices.Equal(written.EncryptionKey, leaf.EncryptionKey) {
		t.Fatal("leaf 2 does not carry the committer's new leaf node after the apply, so the cost this test asserts is not the one it describes")
	}

	failure := ValidateCommit(in)
	if !errors.Is(failure, errCommitProposalsNotResolved) {
		t.Fatalf("ValidateCommit over a commit carrying an inline update attributed to leaf %d under a committer at leaf %d = %v, want errCommitProposalsNotResolved; the apply door above writes the committer's leaf node into leaf %d",
			wrong, in.Committer, failure, wrong)
	}
	if !strings.Contains(failure.Error(), "Sender") {
		t.Errorf("the refusal is %v and does not say which field disagreed; only the sender does here",
			failure)
	}
}

// TestACachedProposalMovedToALeafOfItsOwnIsRefused is the same fault on the other arm, with the
// sender moved to a leaf that does not exist.
//
// A by-reference entry keeps the sender THE CACHE RECORDED, which is the leaf that framed and
// signed the proposal. A list that moved it names a leaf nobody published from, and the number is
// chosen outside the tree so that what accepting it costs is not a member being replaced but the
// apply door indexing past the end of the tree -- a rule reading in.List deciding off a leaf index
// no message ever carried.
//
// THE MOVE IS EXACTLY ONE OCTET UP AND NOTHING ELSE, which is the WIDTH of the comparison rather
// than the fact of one. The leaf this test used to name was the tree's width plus one -- leaf 5
// against a recorded leaf 1 -- so every octet of the two indices differed and a comparison one
// octet wide separated them just as well as a comparison eight octets wide. Measured: with the
// Sender row truncated to the low octet of a LeafIndex, the whole suite stayed green. Two leaves
// that agree in their low octet and differ above it are the pair that tells the two apart, and
// they are also the reason this corpus now carries a leaf index that does not fit in one octet.
func TestACachedProposalMovedToALeafOfItsOwnIsRefused(t *testing.T) {
	crypto := testCrypto(t)
	in, _, _ := testCommitNamingACachedRemove(t, crypto, testHeldRemoveTarget)
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the commit this test is one edit away from: %v", failure)
	}
	entry := testListEntryAt(t, in.List, "Removes", 0)
	if entry.ByValue {
		t.Fatal("the fixture's remove is carried by value, so this test is not driving the by-reference arm")
	}
	held := entry.Sender
	// one octet up, which is the smallest move a comparison narrower than the index cannot see
	const octet = LeafIndex(1) << 8
	outside := held + octet
	if outside%octet != held%octet {
		t.Fatalf("leaf %d and leaf %d differ in their low octet, so this probe does not separate a comparison one octet wide from the whole index",
			held, outside)
	}
	if outside < LeafIndex(in.PreTree.LeafWidth()) {
		t.Fatalf("leaf %d is inside a tree %d leaves wide, so what accepting this costs is not the apply door indexing past its end",
			outside, in.PreTree.LeafWidth())
	}
	entry.Sender = outside

	failure := ValidateCommit(in)
	if !errors.Is(failure, errCommitProposalsNotResolved) {
		t.Fatalf("ValidateCommit over a list that attributes a cached proposal to leaf %d while the cache recorded leaf %d = %v, want errCommitProposalsNotResolved; the two agree in their low octet, so a Sender comparison narrower than a LeafIndex accepts this",
			outside, held, failure)
	}
	if !strings.Contains(failure.Error(), "Sender") {
		t.Errorf("the refusal is %v and does not say which field disagreed; only the sender does here",
			failure)
	}
}

// TestAListThatRecordsAnotherNameForTheProposalItHoldsIsRefused is the Ref row, which nothing
// observed at all.
//
// MEASURED: emptying that row's comparator to `return nil, nil` left the whole suite green, and the
// reason it is invisible rather than merely uncovered is subtle.ConstantTimeCompare(nil, nil) == 1
// -- a comparator that reads nothing reports every pair as equal, so the row goes on being walked
// and goes on answering yes.
//
// WHAT THE Ref FIELD IS is the name this member RESOLVED the entry under, and it is not the same
// fact as what that name points at: the Proposal row holds the body, and this row holds the
// provenance. The two are told apart here by giving the list a name the cache really holds -- for a
// DIFFERENT remove -- while leaving the body, the sender and the arm exactly as the commit names
// them, so the Ref row is the only one that can refuse.
//
// WHAT ACCEPTING IT COSTS is asserted rather than described, and it is (*ProposalList).Refs. That
// method rebuilds a commit's ProposalOrRef vector out of the list's own Ref fields, and it is what
// every committer in this package uses to publish one. A member that accepted this list and went on
// to re-derive the vector from it would publish a commit naming a remove of another leaf than the
// one it validated -- the transcript covering one proposal and the list applying another, which is
// this door's whole subject.
func TestAListThatRecordsAnotherNameForTheProposalItHoldsIsRefused(t *testing.T) {
	crypto := testCrypto(t)
	in, cache, held := testCommitNamingACachedRemove(t, crypto, testHeldRemoveTarget)
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the commit this test is one edit away from: %v", failure)
	}
	// a second remove this member also holds, so the name the list records is a real one
	other := testCachedRemoveOf(t, crypto, cache, LeafIndex(3))
	if slices.Equal(other.Ref, held.Ref) {
		t.Fatal("the cache answered one reference for two different removes, so there is no second name to record here")
	}
	at := -1
	for i := range in.Commit.Proposals {
		if in.Commit.Proposals[i].Type == ProposalOrRefTypeReference {
			at = i
			break
		}
	}
	if at < 1 {
		t.Fatalf("the fixture's first by-reference entry is at %d; a probe placed at element zero is one a rule narrowed to the first entry refuses anyway",
			at)
	}
	entry := testListEntryAt(t, in.List, "Removes", 0)
	entry.Ref = other.Ref

	// the body, the sender and the arm still agree, so the Ref row is the only one that can refuse
	if entry.Proposal.Remove.Removed != held.Proposal.Remove.Removed ||
		entry.Sender != held.Sender || entry.ByValue != held.ByValue {
		t.Fatal("the edit moved something besides the reference, so a refusal here could come from another row")
	}
	// what accepting it costs, through the method a committer publishes a vector with
	rebuilt := in.List.Refs()
	if slices.Equal(rebuilt[at].Reference, in.Commit.Proposals[at].Reference) {
		t.Fatal("the vector rebuilt from the list names what the commit names, so this edit costs nothing and is not the one this test is about")
	}
	republished, names := cache.Cached(in.Context, rebuilt[at].Reference)
	if !names || republished.Proposal.Remove.Removed == held.Proposal.Remove.Removed {
		t.Fatal("the name the list now records does not resolve to a different remove, so the cost this test asserts is not the one it describes")
	}

	failure := ValidateCommit(in)
	if !errors.Is(failure, errCommitProposalsNotResolved) {
		t.Fatalf("ValidateCommit over a list that records the name of a remove of leaf %d beside the body of a remove of leaf %d = %v, want errCommitProposalsNotResolved; Refs rebuilds the published vector out of that field, so a member accepting this republishes a commit naming a proposal it never judged",
			republished.Proposal.Remove.Removed, held.Proposal.Remove.Removed, failure)
	}
	if !strings.Contains(failure.Error(), "Ref") {
		t.Errorf("the refusal is %v and does not say which field disagreed; the body, the sender and the arm all agree here and only the name does",
			failure)
	}
}

// TestTheJoinRefusesTwoProposalsThatEncodeAlikeUnderDifferentTypes is the claim the header of
// checkListResolvesTheCommitsVector used to make the other way round.
//
// THE SENTENCE WAS "the discriminant is the first field of a proposal's encoding, so a type
// disagreement is an octet disagreement". It is false in this build and the pair below is the
// measurement: proposal_wire.go selects the ARM by ProposalType and writes UnknownType as the wire
// discriminant whenever one is set, so a remove of leaf 0x03bbccdd and an external_init carrying
// bb cc dd under UnknownType remove encode to the same six octets.
//
// DRIVEN THROUGH THE JOIN AND NOT THROUGH THE DOOR, deliberately: the v1 profile refuses
// external_init and the structural rule at the door refuses it before any join runs, so this pair
// cannot be posted to ValidateCommit. That is the whole reason the row is worth writing -- the
// safety of the old sentence rested on normalisation in another file plus a profile refusal in a
// third, with nothing tying either to this comparison, and NewProposalList keeps an un-normalised
// value verbatim on its decode-error path. The comparison now carries the type itself.
func TestTheJoinRefusesTwoProposalsThatEncodeAlikeUnderDifferentTypes(t *testing.T) {
	asRemove := CachedProposal{Proposal: Proposal{ProposalType: ProposalTypeRemove,
		Remove: &Remove{Removed: LeafIndex(0x03bbccdd)}}}
	asExternalInit := CachedProposal{Proposal: Proposal{ProposalType: ProposalTypeExternalInit,
		UnknownType:  ProposalTypeRemove,
		ExternalInit: &ExternalInit{KemOutput: []byte{0xbb, 0xcc, 0xdd}}}}

	// the premise, measured here rather than asserted: the two encode alike
	first, err := proposalOctets(&asRemove.Proposal)
	if err != nil {
		t.Fatalf("encode the remove: %v", err)
	}
	second, err := proposalOctets(&asExternalInit.Proposal)
	if err != nil {
		t.Fatalf("encode the external_init: %v", err)
	}
	if slices.Equal(first, second) {
		t.Logf("a %s and a %s both encode to %x, which is the premise this row exists for",
			proposalTypeName(asRemove.Proposal.ProposalType),
			proposalTypeName(asExternalInit.Proposal.ProposalType), first)
	} else {
		// NOT A SKIP. The refusal below is owed whether or not the two encode alike -- a join
		// over two different types must answer no either way -- and a row that opted out of its
		// own assertion when the premise stopped holding would be a row nothing drives on the
		// build where somebody had just changed the encoder.
		t.Logf("a %s encodes to %x and a %s to %x, so this build no longer writes UnknownType as the wire discriminant and the Proposal row's type prefix is no longer the only thing separating them",
			proposalTypeName(asRemove.Proposal.ProposalType), first,
			proposalTypeName(asExternalInit.Proposal.ProposalType), second)
	}
	if asRemove.Proposal.ProposalType == asExternalInit.Proposal.ProposalType {
		t.Fatal("both halves carry one ProposalType, so there is no type disagreement here to refuse")
	}

	failure := joinCachedProposals(0, &asRemove, &asExternalInit)
	if !errors.Is(failure, errCommitProposalsNotResolved) {
		t.Fatalf("the join over a %s and an %s that encode to the same %x = %v, want errCommitProposalsNotResolved; the two are one proposal to a comparison of the octets alone",
			proposalTypeName(asRemove.Proposal.ProposalType),
			proposalTypeName(asExternalInit.Proposal.ProposalType), first, failure)
	}
	if !strings.Contains(failure.Error(), "Proposal") {
		t.Errorf("the refusal is %v and does not name the field that disagreed", failure)
	}
	// and the accepting half, so the row is not "the join refuses everything"
	if failure := joinCachedProposals(0, &asRemove, &asRemove); failure != nil {
		t.Fatalf("the join refused a proposal against itself: %v", failure)
	}
}

// ---------------------------------------------------------------------------
// a cache that does not exist
// ---------------------------------------------------------------------------

// proposalCacheCallArguments is a value for every parameter type the exported methods of
// *ProposalCache take, keyed by the TYPE rather than by the method.
//
// KEYED BY TYPE BECAUSE THE METHOD SET IS DERIVED. The sweep below walks whatever *ProposalCache
// exports, so a table keyed by method name would be a list that goes stale the moment a seventh
// method is declared -- and the failure would be silence, because a method with no row is a method
// nobody drove. Keyed by type, a new method whose parameters are already understood is swept for
// free and one taking something new fails loudly with the sentence that says what to write.
//
// THE VALUES ARE THE ONES THE METHODS ACCEPT, not zero values, and that is what makes the sweep
// mean anything. A nil receiver is easy to survive when every argument is nil as well: Store
// refuses a nil provider before it touches the receiver, Rebind refuses a nil context, and a sweep
// built out of zero values would have reported all six methods clean while three of them took the
// caller's process on the first real call. Each value below is the one a caller in the ordinary
// case holds.
func proposalCacheCallArguments(t *testing.T, crypto CryptoProvider) map[reflect.Type]reflect.Value {
	t.Helper()
	context := testResolveContext()
	stored := testProposalContent(t, crypto, LeafIndex(1), testRemoveProposal(LeafIndex(2)))
	return map[reflect.Type]reflect.Value{
		reflect.TypeFor[CryptoProvider]():        reflect.ValueOf(crypto),
		reflect.TypeFor[*GroupContext]():         reflect.ValueOf(context),
		reflect.TypeFor[*VerifiedGroupContext](): reflect.ValueOf(testVerifiedContextAt(t, context)),
		reflect.TypeFor[*AuthenticatedContent](): reflect.ValueOf(stored),
		reflect.TypeFor[ProposalRef]():           reflect.ValueOf(ProposalRef("a reference this cache does not hold")),
		reflect.TypeFor[LeafIndex]():             reflect.ValueOf(LeafIndex(0)),
		reflect.TypeFor[[]ProposalOrRef](): reflect.ValueOf([]ProposalOrRef{
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef("a reference this cache does not hold")}}),
	}
}

// TestEveryExportedMethodOfAProposalCacheRefusesANilCacheRatherThanPanicking is the receiver half
// of this package's nil-argument doctrine, derived off the method set.
//
// IT IS HERE BECAUSE THREE OF THEM DID NOT. bindingHolds guarded self.binding and not self, so
// Cached, Pending and CheckEpoch dereferenced a nil receiver -- and CommitValidationInput.Pending
// is legitimately nil in this package's own fixtures, so the join that now reads the cache would
// have taken the test process rather than refusing the commit. holds() next door already had the
// guard and states the doctrine: a nil cache holds nothing, which is what makes a rule over it fail
// closed under one instead of guarding for it at every call.
//
// WHAT EACH ANSWER HAS TO BE IS READ OFF THE RESULT TYPES rather than written per method: an error
// result must be non-nil, a bool must be false, and a slice must be empty. Those are the three
// shapes "this cache holds nothing and belongs to no epoch" takes in this type's signatures, and a
// seventh method answering one of them is swept without anybody adding a row.
func TestEveryExportedMethodOfAProposalCacheRefusesANilCacheRatherThanPanicking(t *testing.T) {
	crypto := testCrypto(t)
	arguments := proposalCacheCallArguments(t, crypto)
	cache := reflect.TypeFor[*ProposalCache]()
	swept := []string{}
	for i := 0; i < cache.NumMethod(); i += 1 {
		method := cache.Method(i)
		t.Run(method.Name, func(t *testing.T) {
			call := []reflect.Value{reflect.ValueOf((*ProposalCache)(nil))}
			for at := 1; at < method.Type.NumIn(); at += 1 {
				parameter := method.Type.In(at)
				value, known := arguments[parameter]
				if !known {
					t.Fatalf("(*ProposalCache).%s takes a %s at argument %d and this sweep has no value of that type, so the method would be driven with a zero value and a nil receiver survives most of those for the wrong reason",
						method.Name, parameter, at)
				}
				call = append(call, value)
			}
			defer func() {
				if taken := recover(); taken != nil {
					t.Fatalf("(*ProposalCache).%s panicked on a nil cache: %v; a cache that does not exist belongs to no epoch and holds nothing, which is an answer rather than the caller's process",
						method.Name, taken)
				}
			}()
			answered := method.Func.Call(call)
			for at, result := range answered {
				switch method.Type.Out(at).Kind() {
				case reflect.Bool:
					if result.Bool() {
						t.Errorf("(*ProposalCache).%s answered true from a nil cache; a cache that does not exist holds nothing",
							method.Name)
					}
				case reflect.Slice:
					if result.Len() != 0 {
						t.Errorf("(*ProposalCache).%s answered %d entries from a nil cache",
							method.Name, result.Len())
					}
				case reflect.Interface:
					if method.Type.Out(at) != reflect.TypeFor[error]() {
						continue
					}
					if result.IsNil() {
						t.Errorf("(*ProposalCache).%s answered no error from a nil cache; a cache that does not exist belongs to no epoch, and a caller told nothing goes on to act in one",
							method.Name)
					}
				}
			}
		})
		swept = append(swept, method.Name)
	}
	if len(swept) == 0 {
		t.Fatal("*ProposalCache exports no method, so this sweep read something other than the cache")
	}
	// the three the guard was missing on, named so that a rename that took one out of the derived
	// set fails here rather than quietly shrinking the sweep
	for _, owed := range []string{"Cached", "CheckEpoch", "Pending"} {
		if !slices.Contains(swept, owed) {
			t.Errorf("*ProposalCache exports %v and this sweep is written after %s dereferenced a nil receiver; a method that has been renamed out of the set is one nothing drives",
				swept, owed)
		}
	}
}

// TestACommitNamingAReferenceUnderNoCacheIsRefusedRatherThanCrashing is the blocker this join
// stood behind, driven through the door rather than through the method.
//
// CommitValidationInput.Pending is nil in most of this package's fixtures and in every caller that
// has not built a cache yet, and the join reads it for every by-reference entry. Before the
// receiver guard that is a panic inside ValidateCommit -- the whole suite taken by one commit
// naming a proposal, which is a peer-reachable input. The answer is erratum 8815's own value,
// because "this member has no record of what it received" and "this member did not receive that
// proposal" are one fact to the caller.
func TestACommitNamingAReferenceUnderNoCacheIsRefusedRatherThanCrashing(t *testing.T) {
	crypto := testCrypto(t)
	in, _, _ := testCommitNamingACachedRemove(t, crypto, testHeldRemoveTarget)
	if failure := ValidateCommit(in); failure != nil {
		t.Fatalf("ValidateCommit refused the commit this test takes the cache away from: %v", failure)
	}
	in.Pending = nil
	if failure := ValidateCommit(in); !errors.Is(failure, errProposalNotCached) {
		t.Fatalf("ValidateCommit over a commit naming a reference under no cache = %v, want errProposalNotCached",
			failure)
	}
	// and a commit carrying everything inline is not refused for the state of a cache it never
	// reads, which is CheckErrata8815's own position and has to hold here too
	inline, _ := testFullCommitInput(t, crypto)
	inline.Pending = nil
	testCommitProposals(t, inline, testRemoveOf(LeafIndex(2)))
	if failure := ValidateCommit(inline); failure != nil {
		t.Fatalf("ValidateCommit over a by-value only commit under no cache = %v, want nil; a commit that names no reference asks this member's cache nothing",
			failure)
	}
}
