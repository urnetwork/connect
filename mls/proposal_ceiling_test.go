// What one epoch's proposal cache may hold, and the flood it used to hold instead.
//
// THE DEFECT THESE TESTS ARE ABOUT was measured rather than reasoned: 20,000 distinct well formed
// epoch 7 Remove proposals from ONE sender were all accepted, leaving len(byRef), len(order) and
// len(Pending()) at 20,000 apiece. Distinct is the load bearing word -- a ProposalRef is a hash over
// the whole AuthenticatedContent, so one byte of authenticated_data makes the same removal of the
// same leaf a new entry, and there is no semantic identity anywhere in the key. Nothing but Rebind
// empties the cache and Rebind runs at an epoch boundary, so a member who simply never commits keeps
// the epoch open and the map growing.
//
// And it was never only that member's memory. Pending answers EVERY entry, implementing RFC 9420
// section 12.4's "a committer includes all valid pending proposals" unconditionally, so an honest
// committer over a flooded cache emits a commit naming N references that every other member must
// already hold -- and anyone who missed one of them answers errProposalNotCached and cannot apply
// it. One flooding peer degrades the whole group, which is why the flood below is written as a test
// of what Pending answers and not only of what Store accepts.
//
// The scale here is the ceiling and not 20,000, because the ceiling is the number the property is
// about: at one under it every entry is one an honest member could have published, and at one over
// it the refusal is the whole of the fix. 20,000 would take longer and observe strictly less.
package mls

import (
	"encoding/binary"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// testFloodedProposalContent is one proposal of the fixture epoch made distinct from every other by
// ONE byte of authenticated_data and by nothing else.
//
// That is the reported attack verbatim. A flood built out of proposals that differ in their SUBJECT
// -- a remove of leaf 1, a remove of leaf 2 -- would be bounded by the group size whatever this
// cache did, and would let a wrong fix look right; a flood built out of one proposal re-stamped is
// bounded by nothing at all, because the reference is a hash over the octets and the octets differ.
func testFloodedProposalContent(t *testing.T, sender LeafIndex, nonce uint32,
	proposal *Proposal) *AuthenticatedContent {

	t.Helper()
	content := testProposalContentAt(t, sender, []byte("group"), 1, proposal)
	content.Content.AuthenticatedData = binary.BigEndian.AppendUint32(nil, nonce)
	return content
}

// testUpdateProposal is a well formed update carrying one member's signed leaf node.
func testUpdateProposal(t *testing.T, crypto CryptoProvider, member *testMember) *Proposal {
	t.Helper()
	leaf, _ := testLeafNode(t, crypto, member)
	return &Proposal{ProposalType: ProposalTypeUpdate, Update: &Update{LeafNode: *leaf}}
}

// testPaddedAddProposal is one add carrying a key package padded to whatever size the caller asks
// for, which is the vehicle the OCTET ceiling is observable through.
//
// An ADD and not the group_context_extensions proposal below, and the difference is the whole point
// of the per sender octet column. A gce is one per sender by section 12.2, so a sender's entire gce
// holding is ONE entry and the octets it can spend that way are bounded by the entry column already.
// An add is bounded at MaxGroupMembers from one sender and Store applies no key package validation
// at all -- that door is the validation plan's and not this one's -- so an add is the cheapest half
// megabyte a peer can spend five hundred times.
func testPaddedAddProposal(t *testing.T, keyPackage *KeyPackage, padding int) *Proposal {
	t.Helper()
	padded := *keyPackage
	padded.Extensions = append(slices.Clone(keyPackage.Extensions), Extension{
		ExtensionType: ExtensionTypeRequiredCapabilities,
		ExtensionData: make([]byte, padding),
	})
	return &Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: padded}}
}

// testProposalOfType is one well formed proposal of each type the v1 profile accepts, so a rule
// stated over the accepted set can be asked of every member of it.
//
// A fifth accepted type with no case here is FATAL rather than skipped: the gates below judge the
// accepted set, and a member of it this file cannot build is a member nothing judges. The remove
// names a leaf that is NOT the sender the gates hand it, which is what makes "the leaf it names"
// and "the leaf that sent it" two different answers.
func testProposalOfType(t *testing.T, crypto CryptoProvider, member *testMember,
	proposalType ProposalType) *Proposal {

	t.Helper()
	switch proposalType {
	case ProposalTypeAdd:
		keyPackage, _, _ := testKeyPackage(t, crypto, member)
		return &Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: *keyPackage}}
	case ProposalTypeUpdate:
		return testUpdateProposal(t, crypto, member)
	case ProposalTypeRemove:
		return testRemoveProposal(LeafIndex(4))
	case ProposalTypeGroupContextExtensions:
		return &Proposal{
			ProposalType: ProposalTypeGroupContextExtensions,
			GroupContextExtensions: &GroupContextExtensions{Extensions: []Extension{{
				ExtensionType: ExtensionTypeRequiredCapabilities,
				ExtensionData: []byte{0x00, 0x00, 0x00},
			}}},
		}
	}
	t.Fatalf("the v1 profile accepts %s and this file cannot build one, so every gate stated over the accepted set judges a smaller set than it claims",
		proposalTypeName(proposalType))
	return nil
}

// testEnormousProposal is one group_context_extensions proposal just under the largest vector this
// codec will encode, which is the cheapest way to spend a mebibyte inside one accepted entry.
//
// It is what makes the point that an entry ceiling is not an octet ceiling: this is ONE entry
// against maxCachedProposals and about a thousandth of the entry ceiling, and about an eighth of
// everything the octet ceiling allows.
func testEnormousProposal() *Proposal {
	return &Proposal{
		ProposalType: ProposalTypeGroupContextExtensions,
		GroupContextExtensions: &GroupContextExtensions{Extensions: []Extension{{
			ExtensionType: ExtensionTypeRequiredCapabilities,
			ExtensionData: make([]byte, syntax.MaxVectorLength-64),
		}}},
	}
}

// ---------------------------------------------------------------------------
// the ceilings are derived from the accepted set, in both directions
// ---------------------------------------------------------------------------

// TestEveryProposalTypeTheV1ProfileAcceptsHasACeilingOfItsOwn holds the ceiling table EQUAL to the
// profile's accepted set, which is the shape the bucket rule beside it already takes.
//
// Both directions, and the reason is rule 5's. A fifth accepted type with no ceiling row would be
// admitted by checkCacheCeiling's lookup answering "no ceiling" -- which this build refuses, but
// only because that refusal exists; a table that merely listed four would leave the fifth type as
// the one this cache held without bound if anybody made the lookup permissive. And a ceiling for a
// type the profile no longer accepts is a row that outlived what it described, which is the defect
// this project's ledger names most often.
func TestEveryProposalTypeTheV1ProfileAcceptsHasACeilingOfItsOwn(t *testing.T) {
	accepted := []string{}
	for proposalType, refusal := range proposalTypeProfile {
		if refusal == nil {
			accepted = append(accepted, proposalTypeName(proposalType))
		}
	}
	if len(accepted) == 0 {
		t.Fatal("the v1 profile accepts no proposal type at all, so this gate compared two empty sets")
	}
	ceilinged := []string{}
	for proposalType := range proposalTypeCacheCeiling {
		ceilinged = append(ceilinged, proposalTypeName(proposalType))
	}
	slices.Sort(accepted)
	slices.Sort(ceilinged)
	if !slices.Equal(accepted, ceilinged) {
		t.Fatalf("the v1 profile accepts %v and the cache states a ceiling for %v; a type with no ceiling is one this cache would hold without bound, and a ceiling with no type is a row that outlived what it described",
			accepted, ceilinged)
	}
	for _, proposalType := range slices.Sorted(maps.Keys(proposalTypeCacheCeiling)) {
		ceiling := proposalTypeCacheCeiling[proposalType]
		if ceiling.perList < 1 || ceiling.perSender < 1 {
			t.Errorf("%s is accepted by the v1 profile and its ceiling is {perList:%d perSender:%d}; a ceiling below one refuses every proposal of a type the profile says it implements",
				proposalTypeName(proposalType), ceiling.perList, ceiling.perSender)
		}
		if ceiling.perSender > ceiling.perList {
			t.Errorf("%s allows one sender %d and the whole list %d; a per sender quota above the list ceiling is a quota nothing can spend and states a rule that never binds",
				proposalTypeName(proposalType), ceiling.perSender, ceiling.perList)
		}
		if ceiling.perTarget > ceiling.perSender {
			t.Errorf("%s allows one sender %d of that type and %d of them about one leaf; a per leaf ceiling above the per sender one is a quota nothing can spend",
				proposalTypeName(proposalType), ceiling.perSender, ceiling.perTarget)
		}
		if ceiling.perTarget < 0 {
			t.Errorf("%s states a per leaf ceiling of %d; a negative one refuses every proposal of a type the profile says it implements",
				proposalTypeName(proposalType), ceiling.perTarget)
		}
	}
}

// ---------------------------------------------------------------------------
// which accepted types apply to a leaf, read off the arms rather than listed
// ---------------------------------------------------------------------------

// testProposalArmOf answers the arm structure one proposal carries, by reflection over which arm is
// POPULATED rather than by a table keyed on the discriminant.
//
// Reflection, because the class the gate below holds the ceiling table to has to be read off the
// structures themselves. A table keyed on the proposal type is the hand written list rule 5 is
// about, and it is the list that would still say "a remove names a leaf and nothing else does"
// after somebody added an eighth arm that also names one.
func testProposalArmOf(t *testing.T, proposal *Proposal) reflect.Value {
	t.Helper()
	value := reflect.ValueOf(*proposal)
	arms := []reflect.Value{}
	for i := 0; i < value.NumField(); i += 1 {
		field := value.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() {
			continue
		}
		arms = append(arms, field.Elem())
	}
	if len(arms) != 1 {
		t.Fatalf("a %s proposal carries %d populated arms and this reads the one arm a proposal has; checkArm refuses anything else, so a fixture with two is a fixture no door would accept",
			proposalTypeName(proposal.ProposalType), len(arms))
	}
	return arms[0]
}

// testArmNamesALeaf is half of the STRUCTURAL reading of section 12.2's "proposals that apply to
// the same leaf": the arm NAMES a leaf when it carries a LeafIndex, and the leaf it applies to is
// that index. A Remove is the one accepted arm that does.
func testArmNamesALeaf(arm reflect.Value) (LeafIndex, bool) {
	names := reflect.TypeOf(LeafIndex(0))
	for i := 0; i < arm.NumField(); i += 1 {
		if arm.Field(i).Type() == names {
			return LeafIndex(arm.Field(i).Uint()), true
		}
	}
	return 0, false
}

// testArmReplacesItsSendersLeaf is the other half: an arm carrying a LeafNode is the replacement
// for a leaf, and the leaf it replaces is the SENDER's own -- which is why an update flood is one
// sender's second update rather than its five hundredth.
//
// An Add carries a KeyPackage and neither field is at its top level, which is the right answer and
// not an oversight: section 12.1.1 places an added member at a BLANK leaf, so an add is about a
// leaf that does not exist yet, and two adds from one sender are two members rather than two
// proposals about one member.
func testArmReplacesItsSendersLeaf(arm reflect.Value) bool {
	replaces := reflect.TypeOf(LeafNode{})
	for i := 0; i < arm.NumField(); i += 1 {
		if arm.Field(i).Type() == replaces {
			return true
		}
	}
	return false
}

// TestEveryAcceptedTypeThatAppliesToALeafIsCountedAgainstThatLeaf holds proposalAppliesToLeaf and
// the ceiling table's per target column to the class read off the ARMS, in both directions.
//
// THE DEFECT IT IS ABOUT was measured: one sender stored 500 distinct Removes ALL NAMING LEAF 4 --
// distinct because a reference is a hash over the whole message, so one byte of authenticated_data
// re-stamps the same removal -- and every one of them was inside its (sender, type) quota. Section
// 12.2 invalidates a list carrying multiple Update and/or Remove proposals that apply to the same
// leaf, so Pending then answered 500 references a committer could not put in one valid commit, the
// commit built from them is refused by every receiver, the epoch never advances, and the epoch
// advance is the only thing that empties this cache. The doc stated the right rule twice and the
// code counted (sender, type) with no regard to the target.
//
// BOTH DIRECTIONS AND OFF THE STRUCTURES, for rule 5's reason. A type that applies to a leaf and
// has no per leaf ceiling is the flood above. A type that does NOT apply to one and has a per leaf
// ceiling anyway is the opposite fault and is worse than useless: an add would be counted against
// whatever leaf the reflection happened to find, and a member adding two clients would be refused.
func TestEveryAcceptedTypeThatAppliesToALeafIsCountedAgainstThatLeaf(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	const sender = LeafIndex(3)
	applying := []string{}
	judged := 0
	for _, proposalType := range slices.Sorted(maps.Keys(proposalTypeProfile)) {
		if proposalTypeProfile[proposalType] != nil {
			continue
		}
		name := proposalTypeName(proposalType)
		proposal := testProposalOfType(t, crypto, member, proposalType)
		judged += 1
		arm := testProposalArmOf(t, proposal)
		named, names := testArmNamesALeaf(arm)
		applies := names || testArmReplacesItsSendersLeaf(arm)
		if applies {
			applying = append(applying, name)
		}
		leaf, targeted := proposalAppliesToLeaf(sender, proposal)
		if targeted != applies {
			t.Errorf("a %s carries the arm %s and the structures read it as applying to a leaf = %v, and proposalAppliesToLeaf answers %v; the two are the same question and the cache counts by the second",
				name, arm.Type().Name(), applies, targeted)
		}
		if ceiling := proposalTypeCacheCeiling[proposalType]; applies != (ceiling.perTarget >= 1) {
			t.Errorf("a %s applies to a leaf = %v and its ceiling states perTarget = %d; a type that applies to one and is counted against none is the 500-removes-of-one-leaf flood, and one that does not apply to a leaf and is counted against one refuses an honest member's second add",
				name, applies, ceiling.perTarget)
		}
		if !targeted {
			continue
		}
		// and the VALUE, which is the half a bool cannot state. A remove applies to the leaf
		// it NAMES and an update to the leaf that SENT it, and an implementation that answered
		// the sender for both would collapse every remove one member published into one bucket
		// -- refusing its second remove of any leaf at all.
		want := sender
		if names {
			want = named
		}
		if leaf != want {
			t.Errorf("a %s from leaf %d applies to leaf %d and the arm says %d; the arm is where the leaf is",
				name, sender, leaf, want)
		}
	}
	if judged == 0 {
		t.Fatal("the v1 profile accepts nothing, so this gate judged an empty set")
	}
	if len(applying) == 0 {
		t.Fatal("no accepted type was read as applying to a leaf, so the class this holds the table to is empty and every row would pass it")
	}
	t.Logf("%d accepted proposal types judged; %v apply to a leaf and are counted against it",
		judged, applying)
}

// TestTheEntryCeilingIsSummedOffTheCeilingTableRatherThanWrittenDown observes that the number and
// the table cannot drift.
//
// A written 1501 beside a table that grew a fifth row is a bound nobody updated, and it fails in the
// direction that matters: the cache would go on admitting entries a commit could not name.
//
// TWO ASSERTIONS AND THE SECOND IS THE ONE THAT HOLDS. Recomputing the sum off the same table today
// is satisfied by a body that returns the literal 1501, which is exactly the defect -- measured, and
// the reason the second half is here. So a row is ADDED to the ceiling table for the length of this
// test and the bound must move by that row. A written number does not move.
func TestTheEntryCeilingIsSummedOffTheCeilingTableRatherThanWrittenDown(t *testing.T) {
	summed := 0
	for proposalType, refusal := range proposalTypeProfile {
		if refusal != nil {
			continue
		}
		summed += proposalTypeCacheCeiling[proposalType].perList
	}
	if summed == 0 {
		t.Fatal("the accepted set summed to nothing, so this gate compared a bound against zero")
	}
	before := maxCachedProposals()
	if before != summed {
		t.Fatalf("maxCachedProposals() = %d and the per list column of the accepted set sums to %d; a bound that is not the sum of the table is one that stops describing it the moment the table changes",
			before, summed)
	}
	// and it is at least one per member, or a group at the profile's own membership cap could
	// not hold one pending proposal each and the ceiling would be refusing honest traffic
	if before < MaxGroupMembers {
		t.Errorf("maxCachedProposals() = %d and the v1 profile admits %d members; a cache that cannot hold one proposal per member refuses an honest epoch rather than a flood",
			before, MaxGroupMembers)
	}

	const widened = ProposalType(0x0D0D)
	if _, already := proposalTypeCacheCeiling[widened]; already {
		t.Fatalf("%#04x already has a ceiling, so this test is not modelling a widening at all", uint16(widened))
	}
	proposalTypeCacheCeiling[widened] = proposalCacheCeiling{perList: 7, perSender: 7}
	t.Cleanup(func() { delete(proposalTypeCacheCeiling, widened) })
	if got := maxCachedProposals(); got != before+7 {
		t.Fatalf("a ceiling row worth 7 was added to the table and maxCachedProposals() went from %d to %d, want %d; a bound written down rather than read off the table stops describing it the moment the table changes, and the direction it fails in is the permissive one",
			before, got, before+7)
	}
}

// ---------------------------------------------------------------------------
// the flood
// ---------------------------------------------------------------------------

// TestOneSenderCannotFloodOneEpochsCacheBeyondWhatOneCommitCouldName is the reported defect at the
// scale the property lives at, and it observes Pending rather than only Store.
//
// Pending is where the flood stopped being one member's problem: it answers every entry, so the
// number this asserts is the length of the reference vector an honest committer would put in a
// commit that every other member has to be able to resolve.
func TestOneSenderCannotFloodOneEpochsCacheBeyondWhatOneCommitCouldName(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ceiling := proposalTypeCacheCeiling[ProposalTypeRemove].perSender
	if ceiling < 2 {
		t.Fatalf("the remove ceiling is %d, so there is no one-under to observe", ceiling)
	}
	distinct := map[string]bool{}
	for i := 0; i < ceiling; i += 1 {
		// a DIFFERENT leaf each time, because one sender's removes OF ONE LEAF are bounded by
		// the per target column and this test is about the per type one. The flood of one leaf
		// re-stamped is TestOneSenderCannotHoldASecondProposalAboutALeafItAlreadyNamed, and it
		// is refused at the second entry rather than the five hundred and first.
		ref, err := cache.Store(crypto, testResolveContext(),
			testFloodedProposalContent(t, LeafIndex(1), uint32(i), testRemoveProposal(LeafIndex(i))))
		if err != nil {
			t.Fatalf("Store of entry %d of the %d one valid commit list could carry from one sender: %v; every entry under the ceiling is one an honest member could have published",
				i, ceiling, err)
		}
		distinct[string(ref)] = true
	}
	if len(distinct) != ceiling {
		t.Fatalf("%d stores produced %d distinct references, want %d; if they collide this test is not the flood it claims to be",
			ceiling, len(distinct), ceiling)
	}
	if held := len(cache.Pending(testResolveContext())); held != ceiling {
		t.Fatalf("Pending answers %d entries at the ceiling, want %d", held, ceiling)
	}

	// and one over, naming a leaf this sender has not named, so what refuses it is the number of
	// removes it holds and not the leaf this one is about
	_, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), uint32(ceiling), testRemoveProposal(LeafIndex(ceiling))))
	if !errors.Is(err, errProposalCacheSenderQuota) {
		t.Fatalf("the %dth remove from one sender answered %v, want errProposalCacheSenderQuota; without it this cache holds whatever a peer sends and Pending hands all of it to a committer",
			ceiling+1, err)
	}
	if errors.Is(err, errProposalCacheTargetQuota) {
		t.Errorf("the per type refusal answers to the per leaf one as well (%v), so a caller cannot tell a member holding too many removes from a member holding two about one leaf",
			err)
	}
	if held := len(cache.Pending(testResolveContext())); held != ceiling {
		t.Fatalf("Pending answers %d entries after the refusal, want %d; a refusal that still counted the entry is a ceiling that moves",
			held, ceiling)
	}
}

// TestAFloodingSenderSpendsItsOwnQuotaAndNotTheGroups is why the per sender column exists at all.
//
// A cache capped only on its total is not safe, it is differently unsafe: the first sender to reach
// the total denies every honest member its own proposal for the rest of the epoch, which is the same
// availability failure the ceiling was added to prevent and is cheaper to mount than the memory one.
// So the property is not "a flood is refused" -- it is "a flood is refused and the next member is
// not".
func TestAFloodingSenderSpendsItsOwnQuotaAndNotTheGroups(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	ceiling := proposalTypeCacheCeiling[ProposalTypeRemove].perSender
	for i := 0; i < ceiling; i += 1 {
		if _, err := cache.Store(crypto, testResolveContext(),
			testFloodedProposalContent(t, LeafIndex(1), uint32(i), testRemoveProposal(LeafIndex(i)))); err != nil {
			t.Fatalf("Store of flood entry %d: %v", i, err)
		}
	}
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), uint32(ceiling), testRemoveProposal(LeafIndex(ceiling)))); !errors.Is(err, errProposalCacheSenderQuota) {
		t.Fatalf("the flooding sender was answered %v, want errProposalCacheSenderQuota", err)
	}
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(2), 0, testRemoveProposal(LeafIndex(5)))); err != nil {
		t.Fatalf("an honest member's first proposal was refused with %v while another member sat at its own quota; a ceiling counted only over the whole cache turns a flood into a lockout of everybody else",
			err)
	}
}

// TestOneSenderCannotHoldASecondProposalAboutALeafItAlreadyNamed is the flood the shipped test
// mounted and the ceiling admitted: 500 distinct removes ALL NAMING ONE LEAF, every one of them
// inside its sender's (sender, type) quota.
//
// RFC 9420 section 12.2 invalidates a list carrying multiple Update and/or Remove proposals that
// apply to the same leaf, so the set that flood leaves in the cache is one NO VALID COMMIT LIST CAN
// NAME. That is not waste. Nothing removes a single entry, Pending is a committer's only accessor
// and answers all of them, and the only release is a Rebind at an epoch boundary that only a
// successful commit produces -- so the flood makes the commit invalid, the invalid commit prevents
// the epoch advance, and the epoch advance is the only thing that would clear the flood.
//
// The two re-stampings are asserted to be DISTINCT references first, because a refusal of the
// second could otherwise be the map answering "already held" rather than the ceiling answering at
// all -- and this cache exempts a re-delivery from every ceiling it has.
func TestOneSenderCannotHoldASecondProposalAboutALeafItAlreadyNamed(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	first := testFloodedProposalContent(t, LeafIndex(1), 0, testRemoveProposal(LeafIndex(4)))
	second := testFloodedProposalContent(t, LeafIndex(1), 1, testRemoveProposal(LeafIndex(4)))
	firstRef, err := first.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("ProposalRef of the first: %v", err)
	}
	secondRef, err := second.ProposalRef(crypto)
	if err != nil {
		t.Fatalf("ProposalRef of the second: %v", err)
	}
	if string(firstRef) == string(secondRef) {
		t.Fatal("the two re-stampings of one remove hash to one reference, so what refuses the second below would be the map and not the ceiling")
	}
	if _, err := cache.Store(crypto, testResolveContext(), first); err != nil {
		t.Fatalf("the first remove of leaf 4 from leaf 1 was refused with %v", err)
	}
	_, err = cache.Store(crypto, testResolveContext(), second)
	if !errors.Is(err, errProposalCacheTargetQuota) {
		t.Fatalf("a second remove of leaf 4 from the same sender answered %v, want errProposalCacheTargetQuota; section 12.2 lets no list carry both, so it is an entry no committer could ever use",
			err)
	}
	// and it is NOT the per type quota, which has 499 of its 500 left. A caller told the member
	// had published too many removes would go looking for a flood that is one message wide.
	if errors.Is(err, errProposalCacheSenderQuota) {
		t.Errorf("the per leaf refusal answers to the per type one as well: %v", err)
	}
	if held := cache.perSender[proposalCacheQuota{sender: LeafIndex(1), proposalType: ProposalTypeRemove}]; held != 1 {
		t.Errorf("leaf 1 is counted as holding %d removes after one was stored and one refused, want 1; a refusal that still counted the entry is a ceiling that moves",
			held)
	}
	// the same sender's remove of ANOTHER leaf is admitted, because the rule is one per leaf and
	// not a second rule about how many removes a member may publish
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 2, testRemoveProposal(LeafIndex(5)))); err != nil {
		t.Fatalf("the same sender's remove of a DIFFERENT leaf was refused with %v; one sender may legitimately hold one remove per member",
			err)
	}
	// and ANOTHER sender's remove of leaf 4 is admitted, which is the half a careless fix gets
	// wrong. Section 12.2's rule is stated over the LIST and holds across senders, but a cache
	// that enforced it across senders would let one member deny another's proposal by publishing
	// first -- the starvation, from the other side. Choosing among what it is offered is the
	// committer's job; holding a set one sender could have committed is this cache's.
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(2), 0, testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Fatalf("a second member's remove of leaf 4 was refused with %v while another member held one; a cache that refuses it lets the first sender deny every other member's removal of that leaf",
			err)
	}
	// and what the cache holds is then a set each sender could have committed: resolved through
	// the door a committer actually uses, no two entries FROM ONE SENDER apply to one leaf
	list, err := cache.Resolve(crypto, testResolveContext(), LeafIndex(9),
		cache.Pending(testResolveContext()))
	if err != nil {
		t.Fatalf("Resolve of everything Pending answered: %v", err)
	}
	if list.Len() != 3 {
		t.Fatalf("the resolved list carries %d proposals, want 3", list.Len())
	}
	seen := map[proposalCacheLeafQuota]string{}
	order := list.All()
	for i := range order {
		cached := order[i]
		leaf, targeted := proposalAppliesToLeaf(cached.Sender, &cached.Proposal)
		if !targeted {
			continue
		}
		key := proposalCacheLeafQuota{
			sender:       cached.Sender,
			proposalType: cached.Proposal.ProposalType,
			leaf:         leaf,
		}
		if already, twice := seen[key]; twice {
			t.Errorf("entry %d and %s both apply to leaf %d and both came from leaf %d; section 12.2 invalidates the list that carries them, and this is the list Pending handed the committer",
				i, already, leaf, cached.Sender)
		}
		seen[key] = proposalTypeName(cached.Proposal.ProposalType)
	}
}

// TestAnUpdateAndARemoveOfOneLeafAreCountedApart is the reason the per leaf key carries the TYPE.
//
// Section 12.2's rule is stated over Update and Remove TOGETHER, so a list carrying leaf 3's update
// beside leaf 3's removal is invalid -- but the two come from different senders in every case that
// matters, and refusing the second at this door would be one member silencing another. What this
// cache can state is the per sender half, and the type is in the key so that a sender's own update
// does not spend the quota its remove of somebody else needs.
func TestAnUpdateAndARemoveOfOneLeafAreCountedApart(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	cache := testCache(t)
	// leaf 1's own update, which applies to leaf 1
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 0, testUpdateProposal(t, crypto, member))); err != nil {
		t.Fatalf("the first update from leaf 1 was refused with %v", err)
	}
	// and leaf 1's remove OF ITSELF, which applies to the same leaf under a different type
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 1, testRemoveProposal(LeafIndex(1)))); err != nil {
		t.Fatalf("a remove naming the same leaf its sender's update applies to was refused with %v; the per leaf quota is counted per type, and one counted over the leaf alone would refuse it",
			err)
	}
	// what is refused is a second of the SAME type about that leaf
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 2, testRemoveProposal(LeafIndex(1)))); !errors.Is(err, errProposalCacheTargetQuota) {
		t.Fatalf("a second remove of leaf 1 from leaf 1 answered %v, want errProposalCacheTargetQuota", err)
	}
}

// TestTheSenderQuotaIsCountedPerTypeSoAnUpdateFloodStopsAtOne is section 12.2 read as a ceiling.
//
// An Update applies to its own sender's leaf, and a list carrying two proposals that apply to one
// leaf is invalid, so a sender's second update is a proposal no valid commit could carry beside its
// first however many it publishes. That makes the update flood the cheapest of the four to refuse,
// and it is refused at TWO rather than at the membership cap -- which is only visible if the quota
// is counted per type. A per sender total would let 500 updates through.
func TestTheSenderQuotaIsCountedPerTypeSoAnUpdateFloodStopsAtOne(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	cache := testCache(t)
	if got := proposalTypeCacheCeiling[ProposalTypeUpdate].perSender; got != 1 {
		t.Fatalf("the update ceiling allows one sender %d; section 12.2 invalidates a list carrying two proposals that apply to one leaf, and an update applies to its sender's own",
			got)
	}
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 0, testUpdateProposal(t, crypto, member))); err != nil {
		t.Fatalf("the first update from a member was refused with %v", err)
	}
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 1, testUpdateProposal(t, crypto, member))); !errors.Is(err, errProposalCacheSenderQuota) {
		t.Fatalf("a second update from one member answered %v, want errProposalCacheSenderQuota", err)
	}
	// the quota is that member's holding of that TYPE and not its holding of anything
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 2, testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Fatalf("a remove from a member at its update quota was refused with %v; the quota is counted per type, and one counted per sender alone would refuse it",
			err)
	}
	// and it is that MEMBER's holding and not the group's
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(2), 0, testUpdateProposal(t, crypto, member))); err != nil {
		t.Fatalf("a second member's first update was refused with %v; the quota is counted per sender, and one counted over the cache would refuse it",
			err)
	}
}

// TestTheWholeCacheStopsAtWhatOneValidCommitListCouldName is the other column: enough senders, each
// inside its own quota, to reach the total.
//
// The per sender quota is what a single peer spends and this is what a set of them spends together,
// and the two refusals are separate values because the remedies are: the sender quota says stop
// trusting a member, and this one says somebody has to commit.
func TestTheWholeCacheStopsAtWhatOneValidCommitListCouldName(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	total := maxCachedProposals()
	perSender := proposalTypeCacheCeiling[ProposalTypeRemove].perSender
	stored := 0
	sender := 0
	for stored < total {
		// a different leaf per entry, for the per target column's reason: this test is about
		// the number of entries the whole cache holds, and a run of removes of one leaf would
		// be stopped by a rule about what one valid list can carry rather than by the total.
		for i := 0; i < perSender && stored < total; i += 1 {
			if _, err := cache.Store(crypto, testResolveContext(), testFloodedProposalContent(
				t, LeafIndex(sender), uint32(i), testRemoveProposal(LeafIndex(i)))); err != nil {
				t.Fatalf("Store of entry %d of %d, from leaf %d: %v; every entry under the total is one a valid commit list could name",
					stored, total, sender, err)
			}
			stored += 1
		}
		sender += 1
	}
	if held := len(cache.Pending(testResolveContext())); held != total {
		t.Fatalf("Pending answers %d at the entry ceiling, want %d", held, total)
	}
	// the last sender still has quota left, so what refuses this is the total and not the quota
	if used := cache.perSender[proposalCacheQuota{sender: LeafIndex(sender - 1), proposalType: ProposalTypeRemove}]; used >= perSender {
		t.Fatalf("leaf %d has spent %d of its %d, so this next store would be refused by the quota and the total would not be observed",
			sender-1, used, perSender)
	}
	_, err := cache.Store(crypto, testResolveContext(), testFloodedProposalContent(
		t, LeafIndex(sender-1), uint32(perSender), testRemoveProposal(LeafIndex(perSender))))
	if !errors.Is(err, errProposalCacheFull) {
		t.Fatalf("the entry past the total answered %v, want errProposalCacheFull", err)
	}
	if held := len(cache.Pending(testResolveContext())); held != total {
		t.Fatalf("Pending answers %d after the refusal, want %d", held, total)
	}
}

// ---------------------------------------------------------------------------
// the octets, which the entry ceiling does not bound
// ---------------------------------------------------------------------------

// TestOneEnormousProposalDoesNotSatisfyACeilingCountedInEntriesAlone is the second dimension.
//
// syntax.MaxVectorLength caps a single field at a mebibyte and a group_context_extensions proposal
// is a vector of extensions each carrying an opaque body, so one accepted entry is worth about a
// mebibyte -- and maxCachedProposals of them are worth about a gibibyte and a half. This asserts
// that what stops the run is the OCTET ceiling and that the entry count when it stops is a tiny
// fraction of the entry ceiling, because a test that only asserted "eventually refused" would pass
// over an implementation with no octet ceiling at all.
func TestOneEnormousProposalDoesNotSatisfyACeilingCountedInEntriesAlone(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	stored := 0
	var err error
	// one per sender, because a group_context_extensions proposal is one per sender by the same
	// section 12.2 rule; the octets are what this is about and the quota must not be what fires
	for sender := 0; sender < maxCachedProposals(); sender += 1 {
		if _, err = cache.Store(crypto, testResolveContext(), testProposalContentAt(
			t, LeafIndex(sender), []byte("group"), 1, testEnormousProposal())); err != nil {
			break
		}
		stored += 1
	}
	if !errors.Is(err, errProposalCacheOctets) {
		t.Fatalf("a run of proposals worth about a mebibyte each stopped with %v after %d of them, want errProposalCacheOctets; a cache bounded only on its entry count admits maxCachedProposals of these",
			err, stored)
	}
	if stored < 2 {
		t.Fatalf("only %d of these were accepted, so the octet ceiling is refusing an honest single proposal rather than an accumulation", stored)
	}
	if stored >= maxCachedProposals() {
		t.Fatalf("%d were accepted against an entry ceiling of %d, so the entry ceiling is what stopped this and the octet one was never observed",
			stored, maxCachedProposals())
	}
	if cache.octets > maxCachedProposalOctets {
		t.Fatalf("the cache holds %d octets against a ceiling of %d; the refusal has to come before the entry lands or the ceiling is one entry wide",
			cache.octets, maxCachedProposalOctets)
	}
	// what refused the run is the SUM and not the entry offered, and both halves of that are
	// observable. The headroom left is smaller than the proposal that was refused, so the ceiling
	// is what bound; and one proposal of exactly that size into an empty cache is accepted, so the
	// ceiling is over what this cache holds for an epoch rather than over one message. A ceiling
	// written per proposal would pass the first of those and fail the second.
	if headroom := maxCachedProposalOctets - cache.octets; headroom >= syntax.MaxVectorLength {
		t.Errorf("the run stopped with %d octets of headroom under the %d ceiling, which is more than the proposal it refused; something other than the ceiling ended it",
			headroom, maxCachedProposalOctets)
	}
	if _, alone := testCache(t).Store(crypto, testResolveContext(), testProposalContentAt(
		t, LeafIndex(0), []byte("group"), 1, testEnormousProposal())); alone != nil {
		t.Fatalf("one proposal of this size into an empty cache was refused with %v; the octet ceiling is over what the cache holds for a whole epoch and not over one message",
			alone)
	}
	t.Logf("%d proposals of about a mebibyte each were accepted into an entry ceiling of %d before the %d octet ceiling refused the next",
		stored, maxCachedProposals(), maxCachedProposalOctets)
}

// TestThePerSenderOctetShareIsTheShareTheEntryColumnAlreadyGrants holds the octet ceiling's second
// column to the ceiling table rather than to a number somebody wrote down.
//
// The share one sender may spend is the share the ENTRY column already grants it: sum(perSender) of
// the sum(perList) entries one valid commit list could name. Deriving it is what keeps the two
// dimensions from stating different policies -- a table edit that changed what one sender may hold
// in entries, beside an octet constant nobody moved, is a per sender rule in one dimension and a
// free-for-all in the other, which is the defect this column closes.
func TestThePerSenderOctetShareIsTheShareTheEntryColumnAlreadyGrants(t *testing.T) {
	perSender, perList := 0, 0
	for _, ceiling := range proposalTypeCacheCeiling {
		perSender += ceiling.perSender
		perList += ceiling.perList
	}
	if perList == 0 {
		t.Fatal("the ceiling table sums to nothing, so this gate compared a share against zero")
	}
	before := maxCachedProposalOctetsPerSender()
	if want := maxCachedProposalOctets / perList * perSender; before != want {
		t.Fatalf("maxCachedProposalOctetsPerSender() = %d and the table's own columns give %d; a share that is not read off the table stops describing it the moment the table changes",
			before, want)
	}
	if before >= maxCachedProposalOctets {
		t.Fatalf("one sender may spend %d of the %d octets one epoch's cache holds; a share that is the whole total is a column that never binds, and the starvation it exists to refuse is open again",
			before, maxCachedProposalOctets)
	}
	// and it is at least one whole proposal, or the column refuses an honest member's single
	// largest message rather than an accumulation of them
	if before < syntax.MaxVectorLength {
		t.Errorf("one sender may spend %d octets and this codec encodes a single field of up to %d; a share below one proposal refuses honest traffic",
			before, syntax.MaxVectorLength)
	}

	// and it MOVES with the table, which a written constant does not. The row widens the list
	// ceiling without widening what one sender may hold, so the share every sender has must fall.
	const widened = ProposalType(0x0E0E)
	if _, already := proposalTypeCacheCeiling[widened]; already {
		t.Fatalf("%#04x already has a ceiling, so this test is not modelling a widening at all", uint16(widened))
	}
	proposalTypeCacheCeiling[widened] = proposalCacheCeiling{perList: 1000, perSender: 1, perTarget: 0}
	t.Cleanup(func() { delete(proposalTypeCacheCeiling, widened) })
	if got := maxCachedProposalOctetsPerSender(); got >= before {
		t.Fatalf("a row worth 1000 of the list and 1 of one sender was added and the per sender share went from %d to %d; a share written down rather than read off the table does not move, and the direction it fails in is the permissive one",
			before, got)
	}
}

// TestOneSenderCannotSpendTheOctetsEveryOtherMemberNeeds is the octet dimension's starvation, and it
// is the defect the entry column's own argument already described.
//
// MEASURED before the column existed: leaf 1 reached 8,388,605 of the 8,388,608 octets -- headroom
// 3 -- from 27 messages and 15 of its own 500 entry add quota, and leaf 2, which had cached nothing
// at all, was then refused a six octet remove. The octets were a single int with no attribution
// while the entries had a per sender column, so the cheapest denial of service against this cache
// was the one dimension nobody had attributed -- and it needs ONE sender where the entry route
// needs two.
//
// The property is not "a flood is refused". It is "a flood is refused AND the next member is not",
// which is the shape TestAFloodingSenderSpendsItsOwnQuotaAndNotTheGroups states for the entries.
func TestOneSenderCannotSpendTheOctetsEveryOtherMemberNeeds(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "mallory")
	keyPackage, _, _ := testKeyPackage(t, crypto, member)
	cache := testCache(t)
	quota := proposalTypeCacheCeiling[ProposalTypeAdd].perSender
	const padding = 512 << 10
	stored := 0
	var err error
	for i := 0; i < quota; i += 1 {
		if _, err = cache.Store(crypto, testResolveContext(), testFloodedProposalContent(
			t, LeafIndex(1), uint32(i), testPaddedAddProposal(t, keyPackage, padding))); err != nil {
			break
		}
		stored += 1
	}
	if !errors.Is(err, errProposalCacheSenderOctets) {
		t.Fatalf("one sender storing half megabyte adds was stopped by %v after %d of them, want errProposalCacheSenderOctets; an octet ceiling with no per sender column is a ceiling one member reaches",
			err, stored)
	}
	// the two octet refusals are two rules with two remedies, exactly as the counted pair is:
	// this one names a member and the other names the epoch
	if errors.Is(err, errProposalCacheOctets) {
		t.Errorf("the per sender octet refusal answers to the total as well (%v), so a caller cannot tell one flooding member from a group that has to commit",
			err)
	}
	if stored < 2 {
		t.Fatalf("only %d of these were accepted, so the share is refusing an honest single proposal rather than an accumulation", stored)
	}
	if stored >= quota {
		t.Fatalf("%d were accepted against an entry quota of %d, so the entry column is what stopped this and the octet share was never observed",
			stored, quota)
	}
	// and the whole cache is nowhere near its total, which is the half that makes this a
	// STARVATION and not an exhaustion: before the column, this sender had spent the group's
	// last three octets
	if cache.octets >= maxCachedProposalOctets {
		t.Fatalf("one sender put the cache at %d of its %d octets", cache.octets, maxCachedProposalOctets)
	}
	// the reported failure verbatim: a member that has cached nothing at all offers six octets
	if _, honest := cache.Store(crypto, testResolveContext(),
		testProposalContent(t, crypto, LeafIndex(2), testRemoveProposal(LeafIndex(4)))); honest != nil {
		t.Fatalf("leaf 2 had cached nothing in this epoch and its remove was refused with %v while leaf 1 sat on %d octets; that is the starvation the per sender column exists to refuse, in the dimension where one message is worth half a mebibyte",
			honest, cache.octets)
	}
	// and it is not only the six octet case: an honest member's own half megabyte add is
	// admitted too, so what is left is room and not a crack
	if _, honest := cache.Store(crypto, testResolveContext(), testFloodedProposalContent(
		t, LeafIndex(2), 0, testPaddedAddProposal(t, keyPackage, padding))); honest != nil {
		t.Fatalf("an honest member's first add of the epoch was refused with %v while leaf 1 sat on %d octets",
			honest, cache.octets)
	}
	t.Logf("one sender spent %d octets over %d adds of its %d entry quota before its %d octet share refused the next, and the cache holds %d of %d",
		cache.octetsPerSender[LeafIndex(1)], stored, quota, maxCachedProposalOctetsPerSender(),
		cache.octets, maxCachedProposalOctets)
}

// ---------------------------------------------------------------------------
// what the ceilings are asked of, and what releases them
// ---------------------------------------------------------------------------

// TestACeilingIsAskedOnlyOfAnEntryTheCacheDoesNotAlreadyHold is the idempotence half.
//
// The key is a hash over the whole message, so a re-delivery is the same key and costs this cache
// nothing more. A ceiling asked of it would answer a sentence about a limit to a caller holding a
// message this cache already agreed to, and would let one duplicated message reach a quota.
func TestACeilingIsAskedOnlyOfAnEntryTheCacheDoesNotAlreadyHold(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	cache := testCache(t)
	content := testFloodedProposalContent(t, LeafIndex(1), 0, testUpdateProposal(t, crypto, member))
	first, err := cache.Store(crypto, testResolveContext(), content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	octets := cache.octets
	again, err := cache.Store(crypto, testResolveContext(), content)
	if err != nil {
		t.Fatalf("the same message delivered twice was refused with %v; the second delivery is the entry the cache already holds, and the update quota is one",
			err)
	}
	if string(again) != string(first) {
		t.Fatalf("the same message answered two references, %x and %x", first, again)
	}
	if held := len(cache.Pending(testResolveContext())); held != 1 {
		t.Fatalf("Pending answers %d after one message delivered twice, want 1", held)
	}
	if cache.octets != octets {
		t.Fatalf("the octets went from %d to %d over a re-delivery; an entry counted twice is a ceiling one duplicated message can reach",
			octets, cache.octets)
	}
	// and the quota really was spent by the first, so the exemption is about identity and not
	// about the rule being off
	if _, second := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 1, testUpdateProposal(t, crypto, member))); !errors.Is(second, errProposalCacheSenderQuota) {
		t.Fatalf("a genuinely second update answered %v, want errProposalCacheSenderQuota", second)
	}
}

// TestRebindReleasesTheOctetsAndTheSenderQuotasWithTheEntries is the boundary half.
//
// A rebind that emptied byRef and kept the arithmetic would open the new epoch with the closed
// epoch's cache already full: every sender would arrive at its quota having cached nothing in the
// epoch it is now in. That is the permanent wedge this whole file was rewritten to remove,
// reintroduced through the accounting rather than through the binding, so it is held by a test.
func TestRebindReleasesTheOctetsAndTheSenderQuotasWithTheEntries(t *testing.T) {
	crypto := testCrypto(t)
	member := testIdentity(t, crypto, "bob")
	cache := testCache(t)
	if _, err := cache.Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 0, testUpdateProposal(t, crypto, member))); err != nil {
		t.Fatalf("Store in the first epoch: %v", err)
	}
	if cache.octets == 0 {
		t.Fatal("the cache counted no octets for an entry it holds, so the release below observes nothing")
	}
	next := testResolveContextAt([]byte("group"), 2)
	if err := cache.Rebind(testVerifiedContextAt(t, next)); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if cache.octets != 0 {
		t.Errorf("the cache holds %d octets after a rebind emptied it; an octet total carried across a boundary is a cache that is full of an epoch that has closed",
			cache.octets)
	}
	if held := len(cache.perSender); held != 0 {
		t.Errorf("%d sender quotas survived the rebind; a member arriving at its quota having cached nothing in this epoch is the wedge the binding rewrite removed",
			held)
	}
	if held := len(cache.perLeaf); held != 0 {
		t.Errorf("%d per leaf quotas survived the rebind; a member unable to name a leaf it named in the epoch that closed is that same wedge one column further in",
			held)
	}
	if held := len(cache.octetsPerSender); held != 0 {
		t.Errorf("%d sender octet holdings survived the rebind, totalling %d; a member arriving at its octet share having spent nothing in this epoch is the wedge in the dimension one message is worth half a mebibyte in",
			held, cache.octetsPerSender[LeafIndex(1)])
	}
	if _, err := cache.Store(crypto, next,
		testFloodedProposalContent2(t, LeafIndex(1), 0, 2, testUpdateProposal(t, crypto, member))); err != nil {
		t.Fatalf("the same member's first update of the NEW epoch was refused with %v; its quota belongs to the epoch that closed",
			err)
	}
}

// testFloodedProposalContent2 is testFloodedProposalContent with the epoch named, for the one test
// that stores either side of a boundary.
func testFloodedProposalContent2(t *testing.T, sender LeafIndex, nonce uint32, epoch uint64,
	proposal *Proposal) *AuthenticatedContent {

	t.Helper()
	content := testProposalContentAt(t, sender, []byte("group"), epoch, proposal)
	content.Content.AuthenticatedData = binary.BigEndian.AppendUint32(nil, nonce)
	return content
}

// TestAnAcceptedTypeWithNoCeilingIsRefusedRatherThanAdmittedWithoutBound performs the commit that
// makes checkCacheCeiling's first branch reachable, for the length of one test.
//
// It is the shape TestABucketlessAcceptedTypeIsRefusedRatherThanSilentlyDropped takes one rule over,
// and it exists for the same reason. Nothing this build can assemble reaches that branch today --
// every value arriving at it has been through the profile gate, and the four types the profile
// accepts are exactly the four the ceiling table has rows for. What makes it reachable is the commit
// that widens the accepted set, and a fifth accepted type admitted with no ceiling would be the one
// type this cache held without bound: counted by nothing, refused by nothing, and answered whole by
// Pending. So the branch refuses rather than defaulting, and this performs the widening so the line
// is executed rather than reasoned about.
//
// The row is removed by the cleanup whether this passes or fails, because every other test in this
// file and the next derives its class off the same table.
func TestAnAcceptedTypeWithNoCeilingIsRefusedRatherThanAdmittedWithoutBound(t *testing.T) {
	crypto := testCrypto(t)
	const widened = ProposalType(0x0C0C)
	if _, already := proposalTypeProfile[widened]; already {
		t.Fatalf("%#04x is already classified, so this test is not modelling a widening at all", uint16(widened))
	}
	if _, already := proposalTypeCacheCeiling[widened]; already {
		t.Fatalf("%#04x already has a ceiling, so the branch this test is about cannot be reached through it",
			uint16(widened))
	}
	proposalTypeProfile[widened] = nil
	t.Cleanup(func() { delete(proposalTypeProfile, widened) })

	accepted := &Proposal{ProposalType: widened, UnknownBody: []byte{0xc0, 0xff, 0xee}}
	if err := checkProposalProfile(defaultProfile(), accepted); err != nil {
		t.Fatalf("the widened profile refused %#04x with %v, so this never reaches the ceiling lookup and the branch is still unobserved",
			uint16(widened), err)
	}
	_, err := testCache(t).Store(crypto, testResolveContext(),
		testFloodedProposalContent(t, LeafIndex(1), 0, accepted))
	if !errors.Is(err, errAcceptedTypeHasNoCeiling) {
		t.Fatalf("an accepted type with no ceiling stored with err = %v, want errAcceptedTypeHasNoCeiling; a type admitted with no ceiling is the one type this cache holds without bound",
			err)
	}
	// and it is not the refusal an unsupported type gets. The two are opposite: this type IS
	// supported -- the profile was just widened to accept it -- and what is missing is a ceiling
	// on THIS side. A caller told the type was unsupported would go and look at the peer.
	if errors.Is(err, errUnregisteredProposalType) || errors.Is(err, errReservedProposalType) {
		t.Errorf("the ceilingless refusal answers to a type refusal as well: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the cache's accounting is a view of what it holds, package-wide rule 11a
// ---------------------------------------------------------------------------

// proposalCacheAccountingDerivedFrom recomputes every accounting field of a cache from the entries
// it holds, so the fields can be compared with what they describe.
//
// THE SAME CLASS ProposalList WAS REBUILT FOR, one type over, and it is here because that class is
// the defect rather than the file it was found in. A ProposalCache keeps byRef -- the entries --
// and then FIVE derived views of that map as state beside it: order, octets, octetsPerSender,
// perSender and perLeaf. Store's own comment says they "move together ... because they describe the
// same set", and the octet field's says "never decremented: nothing removes a single entry, so
// there is no path on which this can drift". Both are arguments, and until this gate there was no
// identity relation between the five fields and the map: a Store that incremented one of them twice,
// or that attributed a remove to the wrong leaf, left every ceiling test green, because the ceiling
// tests read the counters the flood built rather than the entries the cache holds.
//
// It is a CHECK and not a derivation, which is the weaker answer and is chosen with a reason. The
// per-type views of a ProposalList could be derived because a view is a pure function of the order
// and is read a bounded number of times per commit; these counters are read on the hot path of
// every Store and deriving them would make one store O(n) in the cache and a full epoch O(n^2) in
// what an attacker can send. So the identity relation is asserted here rather than made structural,
// and the difference is written down rather than left for the next reader to discover.
func proposalCacheAccountingDerivedFrom(t *testing.T, cache *ProposalCache) (int,
	map[LeafIndex]int, map[proposalCacheQuota]int, map[proposalCacheLeafQuota]int) {

	t.Helper()
	octets := 0
	octetsPerSender := map[LeafIndex]int{}
	perSender := map[proposalCacheQuota]int{}
	perLeaf := map[proposalCacheLeafQuota]int{}
	for _, key := range slices.Sorted(maps.Keys(cache.byRef)) {
		entry := cache.byRef[key]
		// the same encode Store counted, reached through the same function, so this measures
		// the accounting rather than a second opinion about how big a proposal is
		_, size, err := cloneProposal(&entry.Proposal)
		if err != nil {
			t.Fatalf("re-encoding a cached %s to price it: %v",
				proposalTypeName(entry.Proposal.ProposalType), err)
		}
		octets += size
		octetsPerSender[entry.Sender] += size
		perSender[proposalCacheQuota{sender: entry.Sender,
			proposalType: entry.Proposal.ProposalType}] += 1
		if leaf, targeted := proposalAppliesToLeaf(entry.Sender, &entry.Proposal); targeted {
			perLeaf[proposalCacheLeafQuota{sender: entry.Sender,
				proposalType: entry.Proposal.ProposalType, leaf: leaf}] += 1
		}
	}
	return octets, octetsPerSender, perSender, perLeaf
}

// TestTheCachesAccountingIsAlwaysAViewOfTheEntriesItHolds is that identity relation, driven over a
// sequence of stores chosen to reach every branch of Store's accounting.
//
// THE SEQUENCE IS THE FIXTURE. Two senders, so the per sender columns are not one column; three
// types, so the per type key is not the sender's; an update and a remove from one sender, so the
// per leaf key is not the per sender key; a RE-DELIVERY of a proposal already held, which is the
// branch that must count nothing and is the one an over-counting Store fails on; and an add, which
// applies to no existing leaf and must therefore appear in every column except perLeaf.
//
// AND A REBIND AT THE END, because releasing the entries and releasing their accounting are two
// statements and the whole point of this gate is that two statements about one fact are what drift.
func TestTheCachesAccountingIsAlwaysAViewOfTheEntriesItHolds(t *testing.T) {
	crypto := testCrypto(t)
	cache := testCache(t)
	alice := testIdentity(t, crypto, "alice")
	bob := testIdentity(t, crypto, "bob")
	keyPackage, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "carol"))

	stores := []struct {
		what     string
		sender   LeafIndex
		proposal *Proposal
	}{
		{"a remove of leaf 4 from leaf 0", LeafIndex(0), testRemoveProposal(LeafIndex(4))},
		{"a remove of leaf 5 from leaf 0", LeafIndex(0), testRemoveProposal(LeafIndex(5))},
		{"a remove of leaf 4 from leaf 1", LeafIndex(1), testRemoveProposal(LeafIndex(4))},
		{"an update from leaf 0", LeafIndex(0), testUpdateProposal(t, crypto, alice)},
		{"an update from leaf 1", LeafIndex(1), testUpdateProposal(t, crypto, bob)},
		{"an add from leaf 1", LeafIndex(1), testPaddedAddProposal(t, keyPackage, 0)},
	}
	for _, store := range stores {
		if _, err := cache.Store(crypto, testResolveContext(),
			testProposalContent(t, crypto, store.sender, store.proposal)); err != nil {
			t.Fatalf("Store %s: %v", store.what, err)
		}
	}
	// the re-delivery, which must change nothing at all: the same proposal from the same sender
	// hashes to the key already held
	if _, err := cache.Store(crypto, testResolveContext(),
		testProposalContent(t, crypto, LeafIndex(0), testRemoveProposal(LeafIndex(4)))); err != nil {
		t.Fatalf("re-storing a proposal the cache already holds: %v", err)
	}
	if len(cache.byRef) != len(stores) {
		t.Fatalf("the cache holds %d entries after %d distinct stores and one re-delivery; the fixture is not the shape this gate is written over",
			len(cache.byRef), len(stores))
	}

	// the order is the key set of the map, once each and in reception order
	if len(cache.order) != len(cache.byRef) {
		t.Errorf("the cache holds %d entries and its order names %d; a name with no entry answers a reference Resolve cannot find, and an entry with no name is one Pending never offers",
			len(cache.byRef), len(cache.order))
	}
	named := map[string]bool{}
	for at, key := range cache.order {
		if named[key] {
			t.Errorf("the cache's order names the same entry twice, at %d; one entry offered twice is a commit naming a duplicate reference",
				at)
		}
		named[key] = true
		if _, holds := cache.byRef[key]; !holds {
			t.Errorf("the cache's order names an entry at %d that byRef does not hold", at)
		}
	}

	octets, octetsPerSender, perSender, perLeaf := proposalCacheAccountingDerivedFrom(t, cache)
	if cache.octets != octets {
		t.Errorf("the cache accounts %d octets and the entries it holds are %d; a total that is not a view of what it counts is a ceiling reached by re-delivering one message, or one nothing reaches",
			cache.octets, octets)
	}
	if !maps.Equal(cache.octetsPerSender, octetsPerSender) {
		t.Errorf("the cache attributes octets %v and the entries it holds are %v",
			cache.octetsPerSender, octetsPerSender)
	}
	if !maps.Equal(cache.perSender, perSender) {
		t.Errorf("the cache counts %v per sender and type and the entries it holds are %v",
			cache.perSender, perSender)
	}
	if !maps.Equal(cache.perLeaf, perLeaf) {
		t.Errorf("the cache counts %v per sender, type and leaf and the entries it holds are %v",
			cache.perLeaf, perLeaf)
	}
	// the positive control: a derivation that found nothing would agree with a cache that counted
	// nothing, and this fixture certainly stores an add, which is counted everywhere but perLeaf
	if len(perSender) < 2 || len(perLeaf) == 0 || octets == 0 {
		t.Fatalf("the derivation answered %d per-sender rows, %d per-leaf rows and %d octets; it read something other than the entries",
			len(perSender), len(perLeaf), octets)
	}
	if len(perLeaf) >= len(perSender)+len(stores) {
		t.Errorf("every entry landed in a per-leaf row and the fixture stores an add, which applies to no existing leaf; the per-leaf column is counting something other than what applies to a leaf")
	}

	// AND THE CLASS OF ACCOUNTING FIELDS IS THE STRUCT'S, in both directions, because a gate over
	// five names written down here is the shape that understates its class the moment a sixth
	// column is added -- and a sixth column is exactly what the last two ceiling repairs each
	// were. Two fields are exempt and each is exempt for a reason rather than by name: byRef IS
	// the entries every column above is derived from, and binding names the epoch rather than
	// counting anything.
	entries := map[string]string{
		"byRef":   "the entries themselves, which every column above is derived FROM",
		"binding": "the epoch this cache belongs to, which counts nothing",
	}
	compared := map[string]bool{"order": true, "octets": true, "octetsPerSender": true,
		"perSender": true, "perLeaf": true}
	structure := reflect.TypeOf(ProposalCache{})
	if structure.NumField() == 0 {
		t.Fatal("reflection found no field on ProposalCache, so the class below is empty")
	}
	for i := 0; i < structure.NumField(); i += 1 {
		name := structure.Field(i).Name
		if _, exempt := entries[name]; exempt || compared[name] {
			continue
		}
		t.Errorf("a ProposalCache carries %s and nothing here holds it to the entries the cache holds; a column derived from byRef and kept beside it is a second representation of one fact, and the ceiling tests read the column rather than the entries",
			name)
	}
	for _, name := range slices.Sorted(maps.Keys(compared)) {
		if _, found := structure.FieldByName(name); !found {
			t.Errorf("this gate compares %s and a ProposalCache declares no field of that name; the comparison has outlived what it described",
				name)
		}
	}

	// and Rebind releases the entries and their accounting together
	next := testResolveContext()
	next.Epoch += 1
	if err := cache.Rebind(testVerifiedContextAt(t, next)); err != nil {
		t.Fatalf("Rebind to epoch %d: %v", next.Epoch, err)
	}
	octets, octetsPerSender, perSender, perLeaf = proposalCacheAccountingDerivedFrom(t, cache)
	if cache.octets != octets || !maps.Equal(cache.octetsPerSender, octetsPerSender) ||
		!maps.Equal(cache.perSender, perSender) || !maps.Equal(cache.perLeaf, perLeaf) ||
		len(cache.order) != len(cache.byRef) {

		t.Errorf("after Rebind the cache accounts %d octets, %v per sender, %v per type, %v per leaf and %d names over %d entries; accounting an epoch has released is a quota the next epoch's senders are still paying",
			cache.octets, cache.octetsPerSender, cache.perSender, cache.perLeaf,
			len(cache.order), len(cache.byRef))
	}
}
