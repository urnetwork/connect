// The gates over the one thing (*ProposalList) now decides: that its per-type views ARE its commit
// order and cannot be given anything else.
//
// WHY THIS FILE EXISTS AND WHY IT IS NOT A THIRD CHECK. ProposalList used to carry the commit order
// and four per-type buckets as independently writable fields, and the two doors of this package
// read different ones -- apply_proposals.go walked Updates and Removes while every rule of
// validate_proposals.go read the buckets and the door held the two together by a per-type COUNT.
// Two rounds of gates were written against that shape and each left a live bypass, because every
// bypass came back in count-preserving form. Four of them were verified against the counting door:
//
//	All=[remove(committer)]                    Removes=[remove(3)]     -> accepted, applies remove 3
//	All=[add colliding with the path leaf key]  Adds=[innocent]        -> accepted
//	All=[update publishing the path leaf key]   Updates=[innocent]     -> accepted
//	All=[gce installing an unlisted extension]  GCE=[the good set]     -> accepted
//
// The first is the one to sit with: the sender signed a self-remove and the receiver removed leaf
// 3. That is one member applying a different commit from the one the transcript covers, with every
// count equal.
//
// The repair is not another rule. The views are derived from the commit order at every read, so
// none of those four inputs can be BUILT: whatever the order says, the view says. This file drives
// exactly that, in three shapes -- the type keeps its proposals in one place, every view is the
// order filtered element by element, and each of the four bypasses is now refused by the aggregate
// that used to accept it -- plus the measurement that says what the derivation costs, because the
// alternative to deriving is caching and a cache is a second representation again.
package mls

import (
	"errors"
	"go/ast"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// one representation
// ---------------------------------------------------------------------------

// proposalListStorageFields is every field of ProposalList that can hold cached proposals, by any
// route the type system offers.
//
// BY ROUTE AND NOT BY TYPE NAME, which is rule 5 at the one place it has to hold here. A gate that
// looked for []CachedProposal fields would pass a `map[ProposalType][]CachedProposal` index built
// beside the order -- which is exactly the cache the derivation was chosen over, and exactly the
// shape a later edit reaches for when it decides the filtering is too expensive. So the walk
// follows slices, arrays, pointers and map values down to the element type.
//
// IT DESCENDS INTO STRUCT FIELDS TOO, and that was the hole. The walk used to stop at a struct,
// on the argument that a CachedProposal carries a Proposal carrying arm pointers and a walk into
// those would recurse over the whole wire model answering nothing. That argument is about what
// lies BELOW a CachedProposal, which this walk never reaches -- the entry type is answered before
// any descent -- and it bought a route straight past the gate: `type proposalListIndex struct {
// adds []CachedProposal }` as a field on ProposalList, built in NewProposalList and kept in step in
// Resolve, passed all eight derivation gates and produced a list whose All()[1].Sender was 0x5eed
// while its Adds()[0].Sender was 0. The suite went red at
// TestEveryRuleTheCommitAggregateRunsDecidesOffASourceTheDoorEstablishes, incidentally, because a
// commit rule happens to read that view -- which is a red that says nothing about the property this
// file exists to assert.
//
// SO EVERY ROUTE THE TYPE SYSTEM OFFERS, ENUMERATED OFF reflect.Kind RATHER THAN OFF MEMORY:
// element types for slices, arrays, pointers and channels, BOTH halves of a map, the fields of a
// struct, and the parameters and results of a func -- a `views func() []CachedProposal` field is an
// index with a closure around it. An INTERFACE is answered yes without looking, because a static
// walk cannot see what an interface holds and the honest answer for a gate about
// unrepresentability is that it could hold this.
//
// The walk is bounded by the set of types it has already entered rather than by refusing to
// enter one. A negative is stable, and a positive unwinds the whole walk at the first entry type
// it reaches, so a type met twice inside one field's walk has already answered. The set is FRESH
// PER FIELD, because a type that answered yes for one field is marked entered and would otherwise
// answer no for the next.
func proposalListStorageFields(t *testing.T) []reflect.StructField {
	t.Helper()
	entry := reflect.TypeOf(CachedProposal{})
	var carries func(reflect.Type, map[reflect.Type]bool) bool
	carries = func(held reflect.Type, entered map[reflect.Type]bool) bool {
		if held == entry {
			return true
		}
		if entered[held] {
			return false
		}
		entered[held] = true
		switch held.Kind() {
		case reflect.Slice, reflect.Array, reflect.Pointer, reflect.Chan:
			return carries(held.Elem(), entered)
		case reflect.Map:
			return carries(held.Key(), entered) || carries(held.Elem(), entered)
		case reflect.Struct:
			for i := 0; i < held.NumField(); i += 1 {
				if carries(held.Field(i).Type, entered) {
					return true
				}
			}
		case reflect.Func:
			for i := 0; i < held.NumIn(); i += 1 {
				if carries(held.In(i), entered) {
					return true
				}
			}
			for i := 0; i < held.NumOut(); i += 1 {
				if carries(held.Out(i), entered) {
					return true
				}
			}
		case reflect.Interface:
			return true
		}
		return false
	}
	found := []reflect.StructField{}
	structure := reflect.TypeOf(ProposalList{})
	for i := 0; i < structure.NumField(); i += 1 {
		if carries(structure.Field(i).Type, map[reflect.Type]bool{}) {
			found = append(found, structure.Field(i))
		}
	}
	return found
}

// TestAProposalListKeepsItsProposalsInExactlyOnePlace is the unrepresentability claim itself.
//
// ONE FIELD, AND IT IS UNEXPORTED. Both halves are the property. A second field holding cached
// proposals is a second representation of one fact whatever it is called and whatever rule is
// stood over it -- an Adds bucket, or an index keyed by type -- and that is the class this type
// was rebuilt to remove. An EXPORTED field is one a caller outside this package writes, which is
// how the four bypasses above were built in the first place.
//
// AND THE COUNT IS OVER EVERY FIELD RATHER THAN OVER THE ONES THAT REACH A CachedProposal, which
// is this gate's own repair. proposalListStorageFields derives its ROUTES off reflect.Kind and
// misses none of them, and then asks the enumerated question "does this field reach the TYPE
// CachedProposal" -- while the paragraph above states the class as a second representation of one
// fact WHATEVER IT IS CALLED. An index of POSITIONS is not called anything of the sort:
// `addsAt []int`, filled in NewProposalList, kept in step in Resolve and answered by Adds, holds
// no CachedProposal, reaches none, and passed every derivation gate in this file. It is the same
// defect one indirection over -- a view that can fall behind the order it was built from -- and
// nothing here saw it.
//
// So the class is taken as the COMPLEMENT of the one field that is allowed rather than as a shape
// somebody thought of. Every fact a ProposalList can answer is a function of its commit order --
// that is the whole doctrine of the type -- so a second field is either a cache of something
// derivable, which can fall behind, or a fact about the commit the constructor was never given,
// which nothing can fill. Neither is representable, and a field of any type at all fails here.
// proposalListStorageFields stays because it says WHICH field the one field is, and because a
// build whose single field stopped carrying proposals would otherwise pass a count of one.
//
// A cache would fail this, deliberately. Filtering at every read costs something and the honest
// alternative is to index once at construction and answer the index; that index cannot diverge
// from the order for a caller, but it can diverge for an in-package edit, and it re-opens the
// exact question two rounds of gates failed to close. What it would buy is measured by
// TestDerivingTheViewsCostsLessThanTheRulesThatReadThem, which is why the measurement is in the
// suite rather than in a commit message.
func TestAProposalListKeepsItsProposalsInExactlyOnePlace(t *testing.T) {
	// the complement first: a ProposalList has ONE field, whatever it holds. An index of
	// positions, a count, a memoised length or a closure over the order are all second
	// representations of the commit order and none of them reaches a CachedProposal.
	structure := reflect.TypeOf(ProposalList{})
	if structure.NumField() != 1 {
		named := []string{}
		for i := 0; i < structure.NumField(); i += 1 {
			named = append(named, structure.Field(i).Name+" "+structure.Field(i).Type.String())
		}
		t.Fatalf("ProposalList carries %d fields, %v; every question a list answers is a function of its commit order, so a second field is either a cache of something derivable -- which can fall behind an in-package write, and an index of POSITIONS is exactly that -- or a fact about the commit nothing can fill",
			structure.NumField(), named)
	}
	fields := proposalListStorageFields(t)
	if len(fields) != 1 {
		names := []string{}
		for _, field := range fields {
			names = append(names, field.Name+" "+field.Type.String())
		}
		t.Fatalf("ProposalList holds cached proposals in %d fields, %v; one fact in two fields is two representations with no identity relation between them, which is the defect this type was rebuilt to remove rather than to check for",
			len(fields), names)
	}
	held := fields[0]
	if want := reflect.TypeOf([]CachedProposal{}); held.Type != want {
		t.Errorf("ProposalList keeps its proposals in a %s; the commit order is a sequence and section 12.1.1 places adds in it, so anything that is not a %s has lost the order",
			held.Type, want)
	}
	if unicode.IsUpper([]rune(held.Name)[0]) {
		t.Errorf("ProposalList keeps its proposals in the exported field %s; a caller outside this package can then write it, and a caller writing one field of a list is the whole of how a commit came to be validated as one thing and applied as another",
			held.Name)
	}
	// and the views really are answered by methods rather than by that field under another name
	views := proposalListViewMethods(t)
	if len(views) == 0 {
		t.Fatal("*ProposalList answers no per-type view, so the field above is the only way to read a list and every rule of section 12.2 would be reading it directly")
	}
}

// ---------------------------------------------------------------------------
// every view is the commit order filtered
// ---------------------------------------------------------------------------

// testInterleavedCommitOrder is a commit order carrying TWO proposals of every viewed type, no two
// of one type adjacent, each entry marked with a Sender nothing else in the order shares.
//
// THE MARK IS WHAT MAKES THE COMPARISON AN IDENTITY. Two removes of the same leaf are equal values,
// so a view that answered the right COUNT of the right TYPE in the wrong ORDER -- or that answered
// some other remove of the list -- would compare equal to the filter under any comparison of
// counts or of types. Sender is carried straight through by every accessor and is not read by any
// of them, so it is a serial number this test can put on each entry and find again.
//
// TWO OF EACH and interleaved, because one of each cannot tell an ordered filter from a filter that
// answers whatever it finds first, and a contiguous run cannot tell a filter from a slice of the
// order.
func testInterleavedCommitOrder(t *testing.T) []CachedProposal {
	t.Helper()
	viewed := []ProposalType{}
	for _, bucket := range proposalBucketsOf(&ProposalList{}) {
		viewed = append(viewed, bucket.carries)
	}
	if len(viewed) < 2 {
		t.Fatalf("a ProposalList answers %d views; with fewer than two, an interleaving is one type in a row and this fixture separates nothing",
			len(viewed))
	}
	order := []CachedProposal{}
	for round := 0; round < 2; round += 1 {
		for _, carries := range viewed {
			order = append(order, CachedProposal{
				Proposal: Proposal{ProposalType: carries},
				Sender:   LeafIndex(len(order)),
				ByValue:  true,
			})
		}
	}
	return order
}

// TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered is the identity relation, driven
// element by element over the derived class of views.
//
// ELEMENT BY ELEMENT AND NOT BY COUNT, which is the whole difference between this and the rule it
// replaces. validateBucketsAgreeWithTheCommitOrder compared len(bucket) with the number of entries
// of that type in the order, and every input that got past it satisfied that comparison: an entry
// swapped for another of its type, a bucket reordered, a bucket holding somebody else's proposals
// of the right type. Each of those is a different sequence with the same length, so what is
// compared here is the sequence.
//
// BOTH DIRECTIONS OF THE CLASS. The views are read off *ProposalList's method set and each is
// joined to the type it filters on through proposalBucketsOf, so a view whose accessor filters on
// the WRONG type fails here rather than answering a plausible-looking slice, and a fifth view added
// later is driven on the commit that adds it.
func TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered(t *testing.T) {
	order := testInterleavedCommitOrder(t)
	list := NewProposalList(order)
	if len(list.All()) != len(order) {
		t.Fatalf("the list answers %d entries in its commit order and was built from %d",
			len(list.All()), len(order))
	}
	carriedBy := map[string]ProposalType{}
	for _, bucket := range proposalBucketsOf(list) {
		carriedBy[bucket.accessor] = bucket.carries
	}
	views := proposalListViewMethods(t)
	if len(views) == 0 {
		t.Fatal("*ProposalList answers no per-type view, so this compared nothing")
	}
	covered := 0
	for _, method := range views {
		carries, joined := carriedBy[method.Name]
		if !joined {
			t.Errorf("*ProposalList answers %s and proposalBucketsOf says nothing about which type it filters on; a view nobody can name the type of is a view no gate can say is the right one",
				method.Name)
			continue
		}
		// the filter this view is CLAIMED to be, computed here off the commit order the list was
		// built from rather than off anything the list answers
		want := []LeafIndex{}
		for _, entry := range order {
			if entry.Proposal.ProposalType == carries {
				want = append(want, entry.Sender)
			}
		}
		if len(want) == 0 {
			t.Errorf("the fixture carries no %s, so the %s view is compared against an empty sequence and would agree with anything that answered nothing",
				proposalTypeName(carries), method.Name)
			continue
		}
		answered := proposalListViewAnswer(t, list, method.Name)
		got := []LeafIndex{}
		for _, entry := range answered {
			if entry.Proposal.ProposalType != carries {
				t.Errorf("the %s view answered a %s; the view is not the commit order filtered on %s at all",
					method.Name, proposalTypeName(entry.Proposal.ProposalType),
					proposalTypeName(carries))
			}
			got = append(got, entry.Sender)
		}
		if !slices.Equal(got, want) {
			t.Errorf("the %s view answers the entries %v of the commit order and the order's %s proposals are %v; a view that is not that sequence is a second representation of the commit, and the rules stated over it are rules about a commit nobody sent",
				method.Name, got, proposalTypeName(carries), want)
		}
		covered += 1
	}
	t.Logf("%d per-type views, each held to the commit order element by element over %d entries",
		covered, len(order))
}

// TestAViewOfAProposalListFollowsAWriteToTheCommitOrder is the half of the property a comparison of
// two sequences cannot make.
//
// The test above compares what a view answers with what the order holds AT ONE MOMENT, and an
// index built once at construction agrees with it exactly. What separates a derivation from an
// index is what happens after the order changes: a derived view answers the new order, an index
// answers the order it was built from. This is that, and it is the reason the accessors filter at
// the read rather than caching.
//
// The write goes through All(), which answers the list's own vector rather than a copy -- so this
// also holds that door open. A version of All() that cloned would make this test pass for the
// wrong reason, and the assertion below is written over what the VIEW answers rather than over
// what All answers, so a clone breaks it.
func TestAViewOfAProposalListFollowsAWriteToTheCommitOrder(t *testing.T) {
	list := NewProposalList(testInterleavedCommitOrder(t))
	removes := list.Removes()
	if len(removes) < 2 {
		t.Fatalf("the fixture's commit order carries %d removes; with fewer than two there is no entry to change that is not the first",
			len(removes))
	}
	// the SECOND remove of the order, found through the order rather than through the view: a
	// write through the view is a write to a slice the view built, which is the mistake this
	// function exists to be the counter-example of
	at := -1
	seen := 0
	order := list.All()
	for i := range order {
		if order[i].Proposal.ProposalType != ProposalTypeRemove {
			continue
		}
		if seen == 1 {
			at = i
			break
		}
		seen += 1
	}
	if at < 0 {
		t.Fatal("the commit order does not carry a second remove, so nothing below is written")
	}
	const marked = LeafIndex(0x5eed)
	order[at].Sender = marked
	if answered := list.Removes(); len(answered) < 2 || answered[1].Sender != marked {
		t.Fatalf("the commit order's second remove was changed and the removes view answers %v; a view that does not follow the order is an index built once, which is the second representation this type was rebuilt to stop having",
			answered)
	}
	// and the other views are unaffected, so what followed was the entry rather than the whole
	for _, bucket := range proposalBucketsOf(list) {
		if bucket.carries == ProposalTypeRemove {
			continue
		}
		for _, entry := range bucket.entries {
			if entry.Sender == marked {
				t.Errorf("the %s view answers the entry that was written into the removes of the commit order; a view answering another type's entries filters on nothing",
					bucket.accessor)
			}
		}
	}
}

// TestAProposalListDoesNotShareTheSliceItWasBuiltFrom holds the one thing the constructor claims
// beyond indexing nothing.
//
// NewProposalList clones what it is handed, and without that the caller's own slice header is the
// list's commit order: a caller that goes on appending to it writes past the length this list was
// built with, and every append that fits the spare capacity lands in entries the list will answer
// the moment somebody extends it. That is a list changing under a validator that has already
// judged it, which is the same fault class as the one this type was rebuilt for, one level out.
//
// The append is made to SPARE CAPACITY on purpose. A slice built with make(cap>len) and appended to
// writes into the array the list would be sharing, so a constructor that did not clone is separated
// from one that did; an append past capacity reallocates and both constructors look the same.
func TestAProposalListDoesNotShareTheSliceItWasBuiltFrom(t *testing.T) {
	order := make([]CachedProposal, 0, 4)
	order = append(order,
		CachedProposal{Proposal: Proposal{ProposalType: ProposalTypeRemove,
			Remove: &Remove{Removed: LeafIndex(1)}}, Sender: LeafIndex(1)})
	if cap(order) <= len(order) {
		t.Fatal("the fixture slice has no spare capacity, so an append below reallocates and a constructor that shared the caller's array would look like one that cloned")
	}
	list := NewProposalList(order)
	// the caller goes on using its own slice, which is the ordinary thing a caller does
	order = append(order,
		CachedProposal{Proposal: Proposal{ProposalType: ProposalTypeRemove,
			Remove: &Remove{Removed: LeafIndex(2)}}, Sender: LeafIndex(2)})
	order[0].Sender = LeafIndex(0x5eed)
	if held := list.All(); len(held) != 1 || held[0].Sender != LeafIndex(1) {
		t.Fatalf("the list answers %v after its caller wrote through the slice it was built from; the commit order is the caller's array and not the list's",
			held)
	}
	if removes := list.Removes(); len(removes) != 1 || removes[0].Sender != LeafIndex(1) {
		t.Fatalf("the removes view answers %v after the caller's write; the view filters the caller's array",
			removes)
	}
}

// TestAProposalListDoesNotShareTheProposalArmsItWasBuiltFrom is the same claim one dereference in,
// and the constructor used to fail it.
//
// slices.Clone COPIES HEADERS. It stops the caller appending into this list's commit order and it
// stops nothing else: every entry it copies carries the same *Add, *Update, *Remove and
// *GroupContextExtensions the caller still holds, and the same backing array under its ProposalRef.
// Driven before the constructor was repaired, `order[0].Proposal.Remove.Removed = LeafIndex(99)`
// after construction moved the list's remove target from leaf 3 to leaf 99. That is a list changing
// under a validator that has already judged it -- the sentence the test above names its own class
// with, while asserting only over the value fields where a shallow clone is already enough.
//
// NO PRODUCTION PATH REACHES IT TODAY and that is not the reason it is closed. Resolve builds its
// order through cloneProposal, so every list this package produces for itself already owns its
// arms; NewProposalList is EXPORTED, takes the caller's own values, and is the door a caller
// outside this package builds a list at.
//
// BOTH HALVES OF AN ENTRY, because a CachedProposal carries two things a caller can write through.
// The proposal is what every rule of section 12.2 reads. The reference is what the commit vector
// join compares a by-reference entry BY, so a caller that kept its slice and wrote a byte of it
// would be changing the identity the door matched, after the door matched it.
func TestAProposalListDoesNotShareTheProposalArmsItWasBuiltFrom(t *testing.T) {
	order := []CachedProposal{
		{
			Ref:      ProposalRef{0x01, 0x02, 0x03, 0x04},
			Proposal: Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: LeafIndex(3)}},
			Sender:   LeafIndex(1),
		},
	}
	list := NewProposalList(order)
	// the caller goes on using the values it handed over, which is the ordinary thing a caller does
	order[0].Proposal.Remove.Removed = LeafIndex(99)
	order[0].Ref[0] = 0xFF
	removes := list.Removes()
	if len(removes) != 1 {
		t.Fatalf("the list answers %d removes and was built from one", len(removes))
	}
	if removed := removes[0].Proposal.Remove.Removed; removed != LeafIndex(3) {
		t.Errorf("the list's remove names leaf %d after its caller wrote through the arm it was built from, and was built naming leaf 3; the arm is the caller's and a validator that has already judged this list judged another one",
			removed)
	}
	if held := list.All(); held[0].Ref[0] != 0x01 {
		t.Errorf("the list's reference begins %#02x after its caller wrote through the slice it was built from; a by-reference entry is joined to the commit's vector by exactly those octets",
			held[0].Ref[0])
	}
	// and the arm really is a value of this list's own rather than an equal one reached by luck
	if list.All()[0].Proposal.Remove == order[0].Proposal.Remove {
		t.Error("the list's remove arm is the caller's own pointer; the two assertions above passed on a value the caller can still reach")
	}
}

// ---------------------------------------------------------------------------
// the four inputs the counting door accepted
// ---------------------------------------------------------------------------

// commitBypass is one of the four count-preserving disagreements the per-type COUNT rule admitted,
// as an edit to the commit order of a commit ValidateCommit accepts.
//
// EACH EDIT IS A SWAP AND NOT A DELETION. The entry at one position of the order is exchanged for
// another proposal of the same type at the same position, so the total count, every per-type count
// and the commit's own ProposalOrRef vector are all unchanged -- which is precisely the class the
// count rule could not see, and precisely the class the derivation makes unrepresentable.
//
// AND THE OFFENDER HIDES BEHIND AN INNOCENT ENTRY OF ITS OWN TYPE, which is the other half of the
// shape and the half a fixture carrying one of each type cannot make. Each of the four originals
// was built that way: the bucket held a proposal the rules were happy with while the commit order
// held the one that mattered. Over a list carrying ONE remove, a view that answered element zero
// of its own filter refuses the offender anyway and this row says nothing; over a list whose first
// remove is innocent, that view accepts the commit.
type commitBypass struct {
	// view is the per-type view the old bucket field would have carried the innocent entry in.
	view string
	// leads is the innocent proposal of that same type placed AHEAD of the offender, and false
	// for a type section 12.2 admits only one of. It takes the input because an Update is
	// carried by REFERENCE -- Resolve attributes an inline proposal to the committer, so an
	// update of somebody else's leaf is a cached one -- and what a reference names lives in the
	// input's own cache.
	leads func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
		members []*testMember) (CachedProposal, bool)
	// swaps replaces the entry that view answers at `at` with the offending one, in place, in the
	// commit order, and leaves the commit's own vector naming what the list now holds. For a
	// by-value entry that is free -- (*ProposalList).Refs copies the Proposal struct and keeps
	// its arm pointer, so an edit through the arm moves both fields at once -- and for a cached
	// one it is testRestoreCachedEntries, because a reference names whatever the CACHE holds and
	// an edit to the list alone is a list that no longer resolves this commit.
	swaps func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
		members []*testMember, at int)
	// refuses is what the aggregate must answer now that the view cannot be given anything else.
	refuses error
}

// commitBypassesTheCountRuleAdmitted is the four inputs the owner verified against the counting
// door, each of which it returned nil for.
func commitBypassesTheCountRuleAdmitted() map[string]commitBypass {
	return map[string]commitBypass{
		"a remove of the committer behind an innocent remove": {
			view: "Removes",
			leads: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember) (CachedProposal, bool) {

				return testRemoveOf(LeafIndex(2)), true
			},
			swaps: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember, at int) {

				testListEntryAt(t, in.List, "Removes", at).Proposal.Remove.Removed = in.Committer
			},
			refuses: ErrRemoveCommitter,
		},
		"an add republishing the update path's leaf key behind an innocent add": {
			view: "Adds",
			leads: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember) (CachedProposal, bool) {

				kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "frank"))
				return testAddOf(kp), true
			},
			swaps: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember, at int) {

				testListEntryAt(t, in.List, "Adds", at).Proposal.Add.KeyPackage.LeafNode.EncryptionKey =
					in.Commit.Path.LeafNode.EncryptionKey
			},
			refuses: errDuplicateEncryptionKey,
		},
		"an update republishing the update path's leaf key behind an innocent update": {
			view: "Updates",
			// CACHED, like the update it hides, because that is the only shape an update in a
			// commit has: an inline one is attributed to the committer and would be the
			// committer covering its own Update.
			leads: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember) (CachedProposal, bool) {

				return testCachedUpdateOf(t, crypto, in.Pending, members[2], LeafIndex(2)), true
			},
			swaps: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember, at int) {

				testListEntryAt(t, in.List, "Updates", at).Proposal.Update.LeafNode.EncryptionKey =
					in.Commit.Path.LeafNode.EncryptionKey
				// and the cache is put back in step with the list, because this entry is
				// carried by reference: the join holds the list's entry to what the CACHE
				// holds under that name, so an edit to the list alone would be refused for
				// not resolving the commit rather than by the rule this row is about. That
				// refusal is a different test's -- see
				// TestACommitWhoseReferenceNamesOneProposalWhileItsListHoldsAnother.
				testRestoreCachedEntries(t, crypto, in)
			},
			refuses: errDuplicateEncryptionKey,
		},
		"a group_context_extensions installing an extension outside the v1 profile": {
			view: "GCE",
			// NO INNOCENT LEAD, and that is not an omission. Section 12.2 makes a list carrying
			// two GroupContextExtensions proposals invalid outright and both doors of this package
			// refuse one, so a GCE offender cannot hide behind another of its own type -- the
			// original bypass put a DIFFERENT extension set in the bucket rather than a second
			// proposal. What separates a working GCE view from a broken one over this row is
			// therefore the type it filters on rather than the position it stops at: a view
			// answering some other type answers no extension set at all,
			// (*ProposalList).Extensions falls back to the group's own, and this commit is
			// accepted.
			leads: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember) (CachedProposal, bool) {

				return CachedProposal{}, false
			},
			swaps: func(t *testing.T, crypto CryptoProvider, in *CommitValidationInput,
				members []*testMember, at int) {
				// 0xABCD, which is the code point the owner used against the counting door: a
				// type this build's extension registry does not carry, so no member could
				// evaluate it and the group would be agreeing to a state none of them can read
				installed := append(slices.Clone(testCommitInstalledExtensions()),
					Extension{ExtensionType: ExtensionType(0xABCD), ExtensionData: []byte{}})
				testListEntryAt(t, in.List, "GCE", at).Proposal.GroupContextExtensions.Extensions =
					installed
			},
			refuses: errUnregisteredGroupExtension,
		},
	}
}

// TestTheCountPreservingBypassesOfTheBucketJoinCannotBeBuilt drives all four, and drives the
// reason they are gone rather than caught.
//
// THREE ASSERTIONS PER ROW, and the middle one is the one that says the bypass is unrepresentable
// rather than refused by a third count. First the unedited commit is ACCEPTED, so what follows is
// the edit's answer and not the fixture's. Then the view the old bucket field would have carried
// the innocent entry in is required to ANSWER THE OFFENDING ENTRY: under the old type this was the
// step a caller skipped, by leaving the innocent proposal in the bucket and the offending one in
// All, and there is now no field to leave it in. Then the aggregate refuses.
//
// A rule that read the commit order and a rule that read a bucket used to be two rules about two
// commits. They are now two spellings of one rule about one commit, and that is what each row here
// measures.
func TestTheCountPreservingBypassesOfTheBucketJoinCannotBeBuilt(t *testing.T) {
	crypto := testCrypto(t)
	rows := commitBypassesTheCountRuleAdmitted()
	// one row per view, in both directions, so a fifth view is not a bypass nobody wrote a row for
	views := proposalListBucketNames(t)
	covered := []string{}
	for _, row := range rows {
		covered = append(covered, row.view)
	}
	slices.Sort(covered)
	if !slices.Equal(covered, views) {
		t.Errorf("*ProposalList answers the views %v and the bypasses here cover %v; a view with no row is one nothing says a commit cannot be judged through and applied around",
			views, covered)
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		row := rows[name]
		t.Run(name, func(t *testing.T) {
			in, members := testCommitCarryingOneOfEveryBucketAndItsMembers(t, crypto)
			at := 0
			if lead, hides := row.leads(t, crypto, in, members); hides {
				in = testCommitLedBy(t, in, lead)
				at = 1
			}
			if failure := ValidateCommit(in); failure != nil {
				t.Fatalf("ValidateCommit refused the commit this row is one swap away from: %v; every refusal below would then be that one",
					failure)
			}
			before := len(in.List.All())
			counts := map[string]int{}
			for _, bucket := range proposalBucketsOf(in.List) {
				counts[bucket.accessor] = len(bucket.entries)
			}
			row.swaps(t, crypto, in, members, at)
			// the swap preserved every count the retired rule could see, which is what makes this
			// the input that rule admitted rather than one it would have caught
			if after := len(in.List.All()); after != before {
				t.Fatalf("the swap changed the commit order from %d entries to %d; it is a deletion and not the count-preserving edit this row is about",
					before, after)
			}
			for _, bucket := range proposalBucketsOf(in.List) {
				if got := len(bucket.entries); got != counts[bucket.accessor] {
					t.Fatalf("the swap changed the %s view from %d entries to %d; the retired count rule would have caught this input, so it is not the one that got past it",
						bucket.accessor, counts[bucket.accessor], got)
				}
			}
			// and the view the offender used to be hidden from answers it AT THE POSITION IT WAS
			// PUT, which is the whole of the repair: there is no field left to hide it in and no
			// first entry of its own type to stop at
			answered := 0
			for _, bucket := range proposalBucketsOf(in.List) {
				if bucket.accessor == row.view {
					answered = len(bucket.entries)
				}
			}
			if answered <= at {
				t.Fatalf("the %s view answers %d entries and the offender was put at %d, so no rule stated over that view reads it and this row asserts a refusal that came from somewhere else",
					row.view, answered, at)
			}
			if failure := ValidateCommit(in); !errors.Is(failure, row.refuses) {
				t.Fatalf("ValidateCommit over the swapped commit answered %v, want %v; this is the input the counting door returned nil for, and a member that accepts it applies a commit the transcript does not cover",
					failure, row.refuses)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// what the derivation costs
// ---------------------------------------------------------------------------

// TestNoReaderOfAPerTypeViewFiltersItInsideItsOwnLoop is the half of the cost claim a stopwatch
// cannot make.
//
// A VIEW IS FILTERED AT EVERY READ, which is what makes divergence unrepresentable and is also the
// one way this type can be made expensive: a caller that writes `for i := range list.Removes()`
// and then `list.Removes()[i]` sweeps the whole commit order once per entry, and a list bounded
// only by what a peer may send is then quadratic in what a peer may send. Every rule in this
// package binds its view once and ranges over the binding, and this is what says so.
//
// IT IS NOT COVERED BY THE TIMING BELOW, which is why it is a separate gate rather than a clause
// in that one. That test replays the view reads it counts off the SOURCE -- call sites, not calls
// executed -- so a rule that moved its filter inside a loop would make the aggregate slower and
// the replay no bigger, and the ratio it reports would go DOWN. A measurement that moves the wrong
// way on the defect it names is worse than no measurement, so the defect is named here instead.
//
// THE CLASS IS DERIVED ON BOTH AXES. The view names come off proposalBucketsOf, and the readers
// are every function of this package's non-test source that calls one of them -- less the methods
// declared on *ProposalList itself, which ARE the derivation and cannot be written any other way.
// Reading by bare method name over-reaches if another type ever answers an Adds(), and that is the
// safe direction: a reader wrongly in the class costs a binding, and a reader missed is the
// quadratic sweep this exists to prevent.
func TestNoReaderOfAPerTypeViewFiltersItInsideItsOwnLoop(t *testing.T) {
	views := map[string]bool{}
	for _, bucket := range proposalBucketsOf(&ProposalList{}) {
		views[bucket.accessor] = true
	}
	if len(views) == 0 {
		t.Fatal("*ProposalList answers no per-type view, so this gate read nothing")
	}
	readers := []string{}
	offenders := 0
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			// the accessors themselves are the derivation and are excluded by their RECEIVER
			// rather than by their file or their name
			if function.Recv != nil && len(function.Recv.List) == 1 &&
				receiverTypeName(function.Recv.List[0].Type) == "ProposalList" {
				continue
			}
			depth := 0
			var walk func(ast.Node) bool
			walk = func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.RangeStmt:
					// the range EXPRESSION is evaluated once and is not inside the loop
					ast.Inspect(typed.X, walk)
					depth += 1
					ast.Inspect(typed.Body, walk)
					depth -= 1
					return false
				case *ast.ForStmt:
					if typed.Init != nil {
						ast.Inspect(typed.Init, walk)
					}
					depth += 1
					if typed.Cond != nil {
						ast.Inspect(typed.Cond, walk)
					}
					if typed.Post != nil {
						ast.Inspect(typed.Post, walk)
					}
					ast.Inspect(typed.Body, walk)
					depth -= 1
					return false
				case *ast.CallExpr:
					selector, isSelector := typed.Fun.(*ast.SelectorExpr)
					if !isSelector || len(typed.Args) != 0 || !views[selector.Sel.Name] {
						return true
					}
					readers = append(readers, function.Name.Name+" -> "+selector.Sel.Name)
					if depth > 0 {
						offenders += 1
						t.Errorf("%s calls %s() inside a loop of its own body; a per-type view is the commit order FILTERED at every read, so this sweeps the whole order once per iteration and is quadratic in what one peer can put in a commit. Bind it once above the loop, as every other rule of this package does",
							function.Name.Name, selector.Sel.Name)
					}
					return true
				}
				return true
			}
			ast.Inspect(function.Body, walk)
		}
	}
	// the positive control: a scan that resolved nothing reports the clean bill a complete one
	// reports, and section 12.2's committer rule certainly reads the removes
	found := false
	for _, one := range readers {
		if strings.HasPrefix(one, "validateCommitterIsNotRemoved -> Removes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the scan found the view readers %v, and validateCommitterIsNotRemoved certainly reads the removes, so it is reading something other than this package",
			readers)
	}
	t.Logf("%d reads of a per-type view across this package's non-test source, %d of them inside a loop",
		len(readers), offenders)
}

// ---------------------------------------------------------------------------
// the stopwatch
// ---------------------------------------------------------------------------

// proposalListViewReadsOfSectionTwelveTwo is how many times each per-type view is filtered during
// one run of ValidateProposalList, read off validate_proposals.go rather than counted by hand.
//
// OFF THE SOURCE, because the number is the point of the measurement: the cost of deriving is the
// cost of one filter multiplied by how many the rules ask for, and a hand-written number would be
// the thing that goes stale the moment a rule is added. Every rule binds its view once and ranges
// over the binding -- ranging over the call would refilter at every index, which is the one shape
// this package must not have -- so the calls are what to count.
func proposalListViewReadsOfSectionTwelveTwo(t *testing.T) map[string]int {
	t.Helper()
	parsed := mustParseSource(t, "validate_proposals.go")
	reads := map[string]int{}
	for _, bucket := range proposalBucketsOf(&ProposalList{}) {
		reads[bucket.accessor] = 0
	}
	ast.Inspect(parsed.file, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || len(call.Args) != 0 {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if _, named := reads[selector.Sel.Name]; !named {
			return true
		}
		base := parsed.render(selector.X)
		if base != "in.List" && base != "list" {
			return true
		}
		reads[selector.Sel.Name] += 1
		return true
	})
	return reads
}

// testFullValidationInput is a section 12.2 input carrying a list of every viewed type at the width
// this profile's fixtures can build, and one ValidateProposalList ACCEPTS.
//
// Accepted, which is what makes the timing below a timing of the whole aggregate: every rule
// returns the first failure, so a list any rule refuses is a measurement of the rules that ran
// before it. The list is a commit from leaf 0 that updates the first third of the members, removes
// the last third and adds a handful, which is a shape section 12.2 admits -- updates and removes
// apply to disjoint leaves, no leaf is named twice, and the committer is neither updated nor
// removed.
func testFullValidationInput(t *testing.T, crypto CryptoProvider,
	members int) *ProposalValidationInput {

	t.Helper()
	names := []string{}
	for at := 0; at < members; at += 1 {
		names = append(names, "member-"+strconv.Itoa(at))
	}
	tree, built := testTreeWith(t, crypto, names...)
	third := members / 3
	entries := []CachedProposal{}
	// one update per member, the COMMITTER'S OWN SKIPPED: ValSem111 refuses a list covering it,
	// and the leaf this loop used to start past was leaf zero rather than the committer
	for at := 0; at < members && len(entries) < third; at += 1 {
		if LeafIndex(at) == testCommitterLeaf {
			continue
		}
		update, _ := testUpdateProposalOf(t, crypto, built[at], LeafIndex(at))
		entries = append(entries, update)
	}
	for at := members - third; at < members; at += 1 {
		entries = append(entries, testRemoveOf(LeafIndex(at)))
	}
	for at := 0; at < third; at += 1 {
		kp, _, _ := testKeyPackage(t, crypto, testIdentity(t, crypto, "joiner-"+strconv.Itoa(at)))
		entries = append(entries, testAddOf(kp))
	}
	entries = append(entries, testGceOf(Extension{
		ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x01}}))
	return testValidationInput(t, crypto, tree, testCommitterLeaf, testProposalList(t, entries...))
}

// TestDerivingTheViewsCostsLessThanTheRulesThatReadThem is the measurement the choice between
// deriving and caching turns on, run in the suite rather than quoted from a commit message.
//
// THE QUESTION. A per-type view is the commit order filtered at every read, and section 12.2 reads
// the views a fixed number of times per validation -- counted off the source above, not here. The
// honest alternative is an index built once at construction, which is a second representation of
// the same proposals: unwritable by a caller, but able to fall behind an in-package write to the
// order it was built from, which is the question two rounds of gates already failed to close. It is
// worth having only if the filtering is a material share of what a commit costs.
//
// THE ANSWER, on this tree at this width, is that it is not: the filtering is a small fraction of
// the aggregate that asks for it, and the aggregate is itself run once per epoch. So the bound
// here is loose on purpose -- what it is protecting against is not a few percent of drift but a
// change of ORDER, an accessor that came to be called inside a loop, or a view built by something
// quadratic. Either of those would put the filtering above the rules and fail this.
//
// The two halves are timed over the same list in the same process, so the number compared is a
// ratio rather than a wall clock somebody would have to normalise.
func TestDerivingTheViewsCostsLessThanTheRulesThatReadThem(t *testing.T) {
	crypto := testCrypto(t)
	const members = 96
	in := testFullValidationInput(t, crypto, members)
	if err := ValidateProposalList(in); err != nil {
		t.Fatalf("the fixture list is refused with %v, so the timing below would be a timing of the rules that ran before the refusal",
			err)
	}
	reads := proposalListViewReadsOfSectionTwelveTwo(t)
	asked := 0
	for _, count := range reads {
		asked += count
	}
	if asked == 0 {
		t.Fatal("the scan found no view read in validate_proposals.go, so the derived half below replays nothing")
	}
	if entries := len(in.List.All()); entries < members/2 {
		t.Fatalf("the fixture's commit order carries %d entries over %d members; a filter measured over a short list is a measurement of the call and not of the walk",
			entries, members)
	}

	const rounds = 50
	// AND THE VIEW HALF IS RUN MANY TIMES OVER, which is not padding. The filtering is so much
	// cheaper than the rules that read it that on a clock ticking in milliseconds -- Windows'
	// does -- fifty replays finish inside one tick and the ratio comes out as a flat zero. A zero
	// that means "below the timer" reads exactly like a zero that means "free", and this test
	// would then be reporting a measurement it had not made. So the cheap half is run `scale`
	// times as often and its time divided back down, and both halves are required to be above
	// the tick before anything is concluded from them.
	const scale = 400
	const tick = 5 * time.Millisecond

	// the whole of section 12.2, which is what the views are read BY
	whole := time.Now()
	for round := 0; round < rounds; round += 1 {
		if err := ValidateProposalList(in); err != nil {
			t.Fatalf("the fixture stopped being accepted mid-measurement: %v", err)
		}
	}
	wholeTook := time.Since(whole)

	// and exactly the view reads that aggregate makes, replayed off the counted class.
	//
	// THROUGH THE ACCESSORS AND NOT THROUGH proposalBucketsOf's ENTRIES, which is a distinction
	// this test got wrong once and would have gone on reporting a number for. Ranging over
	// proposalBucketsOf calls each accessor ONCE and hands back the slices; a replay that then
	// read len() off those slices was timing four filters per round and reporting them as twenty.
	// So the accessors are held as functions and each is CALLED as many times as
	// validate_proposals.go calls it.
	answering := []struct {
		name   string
		answer func() []CachedProposal
	}{
		{"Adds", in.List.Adds},
		{"Updates", in.List.Updates},
		{"Removes", in.List.Removes},
		{"GCE", in.List.GCE},
	}
	// and that hand-written four is held to the derived class in both directions, so a fifth view
	// is replayed rather than silently left out of the number this test reports
	replayed := []string{}
	for _, one := range answering {
		replayed = append(replayed, one.name)
	}
	slices.Sort(replayed)
	if !slices.Equal(replayed, proposalListBucketNames(t)) {
		t.Fatalf("this replay calls %v and *ProposalList answers %v; a view left out of the replay is filtering the aggregate pays for and this measurement does not count",
			replayed, proposalListBucketNames(t))
	}

	views := time.Now()
	filtered := 0
	for round := 0; round < rounds*scale; round += 1 {
		for _, one := range answering {
			for at := 0; at < reads[one.name]; at += 1 {
				filtered += len(one.answer())
			}
		}
	}
	viewsTook := time.Since(views) / scale
	if want := rounds * scale * len(in.List.All()); filtered < want {
		t.Fatalf("the replay walked %d entries and the %d view reads over a %d entry order are at least %d; it is not filtering as many times as the aggregate does",
			filtered, asked, len(in.List.All()), want)
	}
	if wholeTook < tick || time.Since(views) < tick {
		t.Fatalf("the aggregate took %v over %d rounds and the view replay took %v over %d; one of them is inside the clock's own granularity, so the ratio below is not a measurement",
			wholeTook, rounds, time.Since(views), rounds*scale)
	}

	share := float64(viewsTook) / float64(wholeTook)
	t.Logf("section 12.2 over %d proposals: %v per run of the whole aggregate, %v per run of the %d view reads it makes (%v), %.2f%% of it",
		len(in.List.All()), wholeTook/rounds, viewsTook/rounds, asked, reads, share*100)
	// A TENTH, against a measured one hundredth, and what it bounds is the DECISION rather than
	// the order of growth. Ten times the headroom is loose enough that nothing about a slower
	// machine reaches it -- both halves are loops over the same data in the same process, so what
	// is compared is a ratio and not a wall clock -- and it is reached by a filtering that stopped
	// being a rounding error beside the rules that ask for it, which is the whole of what the
	// choice between deriving and indexing turns on.
	//
	// IT DOES NOT CATCH A SUPERLINEAR ACCESSOR AND THIS COMMENT USED TO SAY IT DID -- "tight enough
	// to be reached by ... an accessor that became more than linear in the commit order, which is
	// an order of magnitude at this width". Measured: a genuinely quadratic viewOf, with its inner
	// sweep accumulated into a package level sink so that nothing is eliminated, moves this share
	// from about 1.1% to between 3.4% and 4.5%. Three or four times, not ten, and the suite stays
	// green. The cause is arithmetic and not a badly chosen bound: at 97 entries a read is dominated
	// by the append and copy of the matched values rather than by the type scan, so squaring the
	// scan moves the total by a small factor however this bound is set, and a bound tight enough to
	// catch it -- a fortieth -- would sit twice above the linear measurement rather than ten times
	// above it. A share is a PROXY for the order of growth, AND NOTHING HERE MEASURES THE ORDER OF
	// GROWTH ITSELF. TestAPerTypeViewIsLinearInTheCommitOrder stood below this case in this file
	// until 2026-09-05 and did: it timed each accessor against the same filter written out beside
	// it and bounded the ratio at four. It was deleted rather than repaired, because a wall clock
	// could not carry the property on this machine. It skipped one full run in five here and two in
	// fifteen in the round before, so every count this package reports was a sample of a two valued
	// distribution; and the guard that was there for an accessor whose call had been optimised away
	// could not fire, because a block that measures nothing is SHORT and the skip beneath the floor
	// is reached before the fatal above it.
	//
	// So the quadratic accessor is refused by nothing. Measured on 2026-09-05 with viewOf rewritten
	// as a sweep of the order per entry, accumulated so that nothing is eliminated: the whole of
	// ./mls/... and ./message/... comes back 7,485 passing and 0 failing, and this share reads
	// 4.27% run on its own and 1.37% inside that full run -- both under the tenth below, and the
	// pair is what a share is worth as a proxy. mls/UNOBSERVED.md carries the mutation and what
	// closing it would take.
	//
	// It says nothing about a rule that started filtering inside its own loop either, and it must
	// not be read as though it did: the replay is sized off the CALL SITES this file's scan counts,
	// so a rule that moved its filter into a loop would grow the aggregate and not the replay, and
	// this ratio would fall. TestNoReaderOfAPerTypeViewFiltersItInsideItsOwnLoop is that half.
	//
	// A bound of a half would be a bound nothing can fail, which is a logger with an assertion
	// painted on it.
	if share > 0.10 {
		t.Errorf("filtering the views costs %.2f%% of what the rules that read them cost, and it was measured at about 1%% when the derivation was chosen over an index; the filtering is no longer the rounding error the choice to derive rather than index was made on",
			share*100)
	}
}
