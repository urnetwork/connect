// The derived gate over the class the messagegroup join rests on: a field this package ERASES must
// not be filled with an array its CALLER owns.
//
// WHY THIS EXISTS BESIDE A BEHAVIOURAL PIN. connect/messagegroup holds the same property from the
// outside -- TestTheJoinLeavesThisDeviceAndItsNewHandleAbleToWork joins, then asks whether the
// device can still sign, whether its new handle can still sign, and whether that handle can still
// open a path addressed to its own leaf. That pin catches the symptom wherever it comes from and it
// cannot go stale when this package refactors, which is exactly what a pin is for. What it cannot
// do is see a FOURTH site: it observes only the fields the join path it drives happens to use, it
// fails three packages away from the mistake, and the day somebody adds a retained field that no
// join exercises the pin is green over it. This gate is the other half. It fails at the statement.
//
// THE CLASS IS DERIVED AND NOT ENUMERATED, in three steps, each of which refuses rather than
// reporting clean if it finds nothing:
//
//  1. THE ERASE HELPERS, found by what they DO. A function of this package whose body writes a
//     zero through every index of its byte-slice parameter is an erase helper. The name
//     zeroizeSecret appears nowhere in this derivation; rename it and the set is unchanged.
//  2. THE ERASED FIELDS, found by where those helpers are CALLED on a receiver's own field.
//     Every (type, field) an erase reaches is a field whose backing array this package destroys.
//  3. THE FILL SITES, found by field name over every composite literal and every assignment in
//     this package's production source. By NAME rather than by resolved type, deliberately: that
//     over-reports at the class boundary, which is the safe direction, and the sites it admits in
//     excess are printed.
//
// THE DECISION AT EACH SITE IS AN ALIAS QUESTION AND NOT A SPELLING ONE, which is the whole reason
// this gate is worth having. The three sites the coupling actually rests on are written TWO
// DIFFERENT WAYS -- cloneBytes(x) twice and append(T(nil), x...) once -- and a gate that grepped
// for either spelling would be blind to the other. So nothing here matches a spelling. The RHS of
// every fill site is resolved down to the ORIGIN of its backing array, through parentheses, slice
// expressions, dereferences, address-of, type conversions this package declares, append's
// destination, single-assignment locals, and one hop into the return of any function this package
// declares. A site is CALLER-ROOTED when that origin is a parameter of the enclosing function. A
// new spelling of a copy passes without being taught; a new spelling of an alias is caught without
// being taught.
//
// AND THEN THE PART THAT IS NOT DERIVABLE, WHICH WAS MEASURED RATHER THAN GUESSED AT. Twenty fill
// sites exist. Fifteen are not caller-rooted. FIVE ARE, AND NONE OF THE FIVE IS A DEFECT: this
// package HANDS OWNERSHIP OVER on purpose in four places and the fifth is erased by its own caller.
// (*SecretTree).newRatchet's header says so in its own words -- "taking ownership of the root
// secret: it is erased in place by the first step, so the caller must not keep it" -- while
// JoinFromWelcome must never take ownership of a device's signing key. Both are a field an erase
// reaches, filled from a parameter. NOTHING IN THE SOURCE TELLS THEM APART; it takes escape
// analysis or a stated contract.
//
// So the split is: THE CLASS IS DERIVED AND THE DISPOSITION IS ENUMERATED, in
// eraseOwnershipHandovers below, keyed by file:function.field with the evidence for each. Anything
// caller-rooted and undisposed is RED, and any disposition matching no site is RED. That is
// GATES.md's own remedy rather than a retreat from it -- "a derived class whose literal is
// invisible and silent is worth less than an enumerated one that refuses and prints" -- and the
// alternative, widening the decision rule until those five passed, is how a gate stops refusing
// anything.
//
// JUDGED BY GATES.md's TWO QUESTIONS, because "it is derived" is not the criterion this package
// stopped on:
//
//   - IS THE LITERAL AT A LEVEL WHERE BEING WRONG IS VISIBLE? The literals are the erase body
//     shape (step 1), the syntactic forms the resolver peels, and the five dispositions. Being
//     wrong about step 1 empties the class and this gate FATALS rather than passing. Being wrong
//     about a peel form lands the site in the printed "undecided" list, which is an error and
//     not a silent admit. Being wrong about a disposition is visible because every disposition
//     is PRINTED WITH ITS REASON on every passing run, next to the site it excuses.
//   - DOES IT FAIL CLOSED AND PRINT ITS COMPLEMENT? Empty helper set, empty field set and empty
//     site set each fatal. An origin the resolver cannot reach is an error, never an admit. An
//     empty reason on a disposition is an error. And three complements are printed on every run:
//     what "a field an erase reaches" removed (43 sibling fields), what "the origin is a
//     parameter" removed (15 fill sites, each with where its array came from), and every callee
//     this gate admitted WITHOUT opening -- the three names every one of those admissions rests
//     on. Each of the two "complement is empty" clauses was driven red by forcing it empty.
//
// AND THEN THE CHECK THAT JUDGES THE CLAUSES THEMSELVES, run over EVERY clause of this file: delete
// it, run the suite UNFILTERED over all three trees, and see whether anything notices. It is the
// same inversion the gate applies to the package, turned on the gate, and what it found is the
// reason this file looks the way it does now.
//
// SEVENTY-ONE clauses were driven that way and THIRTY-THREE of them could be deleted with the
// unfiltered run reading exactly what it read before. THREE are the ones the finding named, and all
// three are measured at 3e287a4 rather than taken on trust: deleting the *ast.RangeStmt and
// *ast.IncDecStmt arms together, and separately deleting the whole operator-assignment reading,
// each leaves the unfiltered run over all three trees at 7,701 pass / 0 fail / 1 skip, which is that
// copy's own baseline entry for entry. THIRTY MORE came out of the same check over the rest of the
// file once those three had drivers -- thirteen further readings of the form walk, four of its
// refusals and nine of its thirteen non-member counts, and seventeen arms of the resolver, including
// the
// type-assertion peel, the selector arm, the make arm, the builtin arm, the []byte(nil) conversion,
// the named-result reading, the range-bound local, the var declaration, the result-position reading
// of a multi-value call and the opaque answer itself. A clause nothing drives is a comment however
// correct it is.
//
// AND THAT CHECK IS NOW A STANDING ONE RATHER THAN A SWEEP SOMEBODY REMEMBERS TO RE-RUN, which is
// the recurrence rather than the instance. Twice on this line a clause was added to this gate,
// exercised by NOTHING, and shipped green; both times what found it was a hand-run deletion sweep,
// and nothing in the suite would have told the next round the same thing. So every arm that answers
// an origin or decides a binding position now REGISTERS ITSELF BY NAME, the set of names this file
// DECLARES is read out of its own SOURCE TEXT rather than out of a run, and
// TestEveryReadingOfThisGateIsDrivenBySomething asserts that DECLARED minus UNREACHED equals what
// the control corpora EXERCISE -- as an exact set, member by member, never as "non-empty". Adding an
// arm nothing drives turns that test red at the arm's own name; so does adding one that only real
// source reaches; so does deleting an arm's tag, which is what the case-clause totality half is for.
// The seven arms nothing can drive are in eraseUnreachedReadings with the reason for each, and what
// the register CANNOT see is in eraseClausesOutsideTheRegister, printed on every run.
//
// THE CAUSE WAS THE SHAPE OF THE ASSERTIONS AND NOT THE COVERAGE OF THE CORPUS. Complement 4 was
// asserted only to be NON-EMPTY, and non-empty is satisfied by twelve readings when there are
// thirteen; the copy corpus asserted only that a copy is "not the caller's", and that is satisfied
// by a copy that degraded into an opaque admission. Both are now exact: every spelling is asserted
// by the KIND it must answer, and the complement is asserted reading by reading, over corpora that
// go/types accepts as compilable Go.
//
// SEVEN ARMS ARE UNREACHED RATHER THAN DRIVEN, and the register HOLDS them there rather than
// letting them look driven: each is named in eraseUnreachedReadings with its reason, a row that is
// ever taken is red, and a row naming an arm this file no longer declares is red. SIX are argued
// UNREACHABLE from Go's own grammar or from this gate's own structure -- the bounds guard on a
// positional element past the end of a struct's field list; the refusal of an assignment whose two
// sides differ in length and whose right side is not one expression; the UNDECIDED default of the
// result-position reading; the bounds guard on append with no arguments at all; and the two arms
// that refuse receiver state answered out of a plain function, whose scope chain binds no receiver.
// THE SEVENTH SAYS THE WEAKER THING ON PURPOSE and is spelled UNREACHED rather than UNREACHABLE: the
// default of originOf, for which no compilable driver was found. "I could not reach it" is not "it
// cannot be reached", and the register refuses a row that does not say which of the two it means.
//
// AND ONE CLAUSE IS OUTSIDE THE REGISTER ALTOGETHER, in eraseClausesOutsideTheRegister: R25, the
// RESULT QUALIFICATION of originOfBody's cycle key. The register's unit is the ARM, and that is a
// sub-expression of one -- the arm itself is driven, by copy27. It is neither driven nor shown to be
// beyond compilable Go, and it is filed in those terms rather than in stronger ones.
//
// AND ONE READING WAS REMOVED rather than kept: the two that handed a struct field's declared type
// down to a nested literal, for an elision Go permits only "within a composite literal of array,
// slice, or map type".
//
// AND THE FIFTH DEFECT IS CLOSED RATHER THAN CARRIED. A local bound at a RESULT POSITION other than
// 0 of a multi-value assignment was resolved through result 0 -- an array it never receives -- and
// the round that found it left it open on purpose, reproduced and measured LATENT: with every such
// local forced to refuse, the gate answered the same twenty fill sites and stayed green, so nothing
// in connect/mls resolved through one. The repair is the one that round named and it was small for
// the reason it gave: the fill-site walk already passed the target index through originOfResult, so
// the position only had to be carried alongside each value. eraseAssignmentsTo now answers
// value-and-position pairs, originOfIdent resolves through originOfResult, and
// eraseReturnExpressionsAt carries the same pair because a NAMED result assigned by `out, err = f()`
// had the identical hole. It is driven by alias24/copy24, which differ only in WHICH result of one
// callee holds the caller's array; put the defect back and alias24 answers "a fresh array".
package mls

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// eraseGateFile is this file, by name. The register below reads this gate's own SOURCE TEXT to find
// out what it DECLARES, which is the only reading of "declared" that a run cannot make true by
// accident.
const eraseGateFile = "erased_field_alias_test.go"

// ---------------------------------------------------------------------------
// what the resolver answers
// ---------------------------------------------------------------------------

// eraseOriginKind is where a fill site's backing array came from.
type eraseOriginKind int

const (
	// the enclosing function's own parameter, or something reached from one without ever
	// leaving its backing array. THIS IS THE DEFECT.
	eraseOriginParameter eraseOriginKind = iota
	// a fresh array: make, new, a composite literal, a constant, nil, or an append onto one.
	eraseOriginFresh
	// the receiver's own state, which is this type's array and not the caller's.
	eraseOriginReceiver
	// a call this gate could not open: a method, a qualified call, or a function of another
	// package. Admitted, and NAMED on every run.
	eraseOriginOpaque
	// the resolver ran out of forms it understands. Refused, and named.
	eraseOriginUndecided
)

func (self eraseOriginKind) String() string {
	switch self {
	case eraseOriginParameter:
		return "a parameter of the enclosing function"
	case eraseOriginFresh:
		return "a fresh array"
	case eraseOriginReceiver:
		return "the receiver's own state"
	case eraseOriginOpaque:
		return "a call this gate did not open"
	}
	return "UNDECIDED"
}

type eraseOrigin struct {
	kind eraseOriginKind
	// the parameter name, the callee name, or the expression the resolver gave up on.
	what string
}

// eraseFillSite is one place this package's production source writes into a field an erase reaches.
type eraseFillSite struct {
	at string
	// the SCOPE the statement is written in: the declaration, or a function LITERAL nested inside
	// it, named the way Go names one -- BuildWelcome.func1. A site inside a literal is resolved
	// against THAT literal's parameters, so which scope it sits in is part of what names it.
	inside string
	// the outermost declaration that scope is nested in, which is the name a reader looks up.
	declaration string
	field       string
	rhs         string
	origin      eraseOrigin
	// the READING of the form walk that decided this binding position was a member. It is here
	// so that the register below can say which reading admitted which site, rather than only
	// that the reading answered.
	reading string
}

// ---------------------------------------------------------------------------
// the register every reading of this gate writes its own name into
// ---------------------------------------------------------------------------

// eraseReadings is the REGISTER, and it is what makes "is every clause of this gate driven by
// something?" a standing question rather than a sweep somebody remembers to re-run.
//
// WHY IT EXISTS. Twice on this line a clause was added to this gate, exercised by NOTHING, and
// shipped green -- the second time thirty-three of seventy-one at once. Both times the check that
// found them was a hand-run deletion sweep, which is a MEASUREMENT and not a gate: nothing in the
// suite told the next round that a newly added clause was undriven.
//
// AND THE TWO SHAPES THAT ALREADY FAILED ARE NOT REBUILT HERE:
//
//   - AN EXISTENCE ASSERTION CANNOT DETECT A MISSING MEMBER. Complement 4 was asserted only to be
//     non-empty, and non-empty is satisfied by twelve readings when there are thirteen. So this
//     register is asserted as an EXACT SET, member by member, in both directions.
//   - A REGISTER THAT ONLY RECORDS WHAT RAN IS VACUOUS. If "registered" meant "wrote its name in
//     during this run", then registered would be a subset of exercised by construction and the
//     comparison would always hold. So the DECLARED set is read out of this file's own SOURCE
//     TEXT -- every eraseReadings tag written anywhere in it -- and the EXERCISED set is what the
//     control corpora actually took. A reading present in the text and absent from the run is a
//     failing diff at its own name.
//
// The tag is part of the expression it names and never a statement beside it -- name answers its
// own argument, origin answers the origin it wraps -- so a tag cannot be deleted into a silent
// no-op, and an arm written without one is caught by the CASE-CLAUSE TOTALITY check instead.
type eraseReadings struct {
	taken map[string]int
}

func newEraseReadings() *eraseReadings {
	return &eraseReadings{taken: map[string]int{}}
}

// name records that the reading called `of` ANSWERED, and answers `of` itself.
func (self *eraseReadings) name(of string) string {
	if self != nil {
		self.taken[of] += 1
	}
	return of
}

// origin is name for an arm that answers an origin.
func (self *eraseReadings) origin(of string, answer eraseOrigin) eraseOrigin {
	self.name(of)
	return answer
}

// at is name for a reading that answers a RESULT POSITION.
func (self *eraseReadings) at(of string, position int) int {
	self.name(of)
	return position
}

// declined is name for an arm of the method reading that hands its call back to the opaque arm.
func (self *eraseReadings) declined(of string) (eraseOrigin, bool) {
	self.name(of)
	return eraseOrigin{}, false
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// TestEveryFieldThisPackageErasesIsFilledFromAnArrayItOwns is the gate.
//
// It is the class connect/messagegroup's join body rests on, stated from this side. That body
// assembles mls.JoinKeyMaterial over four copies and defers (*JoinKeyMaterial).Zeroize over the
// result, and its own header calls the fourth copy "a fourth instance of a discipline this path
// already spells three times". The three it names are fill sites in THIS package, and nothing here
// held them. This does.
//
// The symptom of a violation is silent in every case, which is why a gate and not a review: an
// all-zero ed25519 seed derives a perfectly valid public key, so a device whose signing key was
// erased through an alias goes on publishing leaves and founding groups under a key anybody can
// derive, with its credential still naming the real identity.
func TestEveryFieldThisPackageErasesIsFilledFromAnArrayItOwns(t *testing.T) {
	fileSet, sources := eraseSources(t)

	// ---- step 1: the erase helpers, by what they do ----
	helpers := eraseHelpersIn(sources)
	if len(helpers) == 0 {
		t.Fatal("no function of this package writes a zero through every index of a byte-slice parameter, so the erase helper derivation found nothing and every step below would report clean having read nothing")
	}
	t.Logf("step 1 -- %d erase helper(s), derived by body shape and not by name: %v",
		len(helpers), helpers)

	// ---- step 2: the fields those helpers erase ----
	erased, owners := eraseFieldsIn(fileSet, sources, helpers)
	if len(erased) == 0 {
		t.Fatal("no erase helper of this package is called on a receiver's own field, so the erased-field class is empty and this gate would admit every fill site in the tree")
	}
	fields := slices.Sorted(maps_Keys(erased))
	t.Logf("step 2 -- %d erased field name(s), derived from %d call site(s): %v",
		len(fields), len(erased), fields)

	// COMPLEMENT 1: what "a field an erase reaches" removed.
	notErased := eraseSiblingFields(sources, erased)
	t.Logf("complement 1 -- the %d field(s) of the %d type(s) that own an erased field which NO erase of this package reaches, and which this gate therefore says nothing about: %v",
		len(notErased), len(owners), notErased)
	if len(notErased) == 0 {
		t.Error("complement 1 is EMPTY: every field of every type that owns an erased field is itself erased, so 'a field an erase reaches' narrows nothing today and would begin removing real members the day one appears")
	}

	// ---- step 3: the fill sites ----
	sites, refusals, removedForms, _ := eraseFillSitesIn(fileSet, sources, erased)
	if len(sites) == 0 {
		t.Fatal("no composite literal and no assignment in this package's production source fills a field an erase reaches, so this gate read nothing")
	}

	// COMPLEMENT 4: what the FORM class removed, and it is here because its absence is the defect
	// this gate shipped with. The first version read two sub-forms of two of the four binding
	// positions Go has; the rest were not refused and not counted, they were INVISIBLE, and a
	// passing run over a tree holding a positional alias was byte for byte a passing run over a
	// tree holding none. A form nothing counts is a form nothing can miss.
	reasons := slices.Sorted(maps_Keys(removedForms))
	decided := 0
	lines := []string{}
	for _, reason := range reasons {
		decided += removedForms[reason]
		lines = append(lines, fmt.Sprintf("%4d  %s", removedForms[reason], reason))
	}
	t.Logf("complement 4 -- the %d binding position(s) this walk DECIDED were not members, under the reading that decided each:\n\t%s",
		decided, strings.Join(lines, "\n\t"))
	if decided == 0 {
		t.Error("complement 4 is EMPTY: every binding position in this package's production source is a fill site of an erased field, so the form walk removes nothing today and a form this gate CANNOT read would be indistinguishable from one it read and dismissed")
	}
	for _, refusal := range refusals {
		t.Errorf("%s: `%s` is %s. A BINDING POSITION this gate has no reading for is REFUSED and never skipped: the two forms this gate shipped blind to -- a positional composite literal and `x.Field, err = f()` -- produced no site, no complement entry and no undecided count between them",
			refusal.at, refusal.src, refusal.form)
	}

	byOrigin := map[eraseOriginKind][]eraseFillSite{}
	for _, site := range sites {
		byOrigin[site.origin.kind] = append(byOrigin[site.origin.kind], site)
	}
	t.Logf("step 3 -- %d fill site(s): %d from a parameter, %d fresh, %d from the receiver, %d through a call this gate did not open, %d undecided",
		len(sites), len(byOrigin[eraseOriginParameter]), len(byOrigin[eraseOriginFresh]),
		len(byOrigin[eraseOriginReceiver]), len(byOrigin[eraseOriginOpaque]),
		len(byOrigin[eraseOriginUndecided]))

	// COMPLEMENT 2: what "the origin is a parameter" removed.
	removed := []string{}
	for _, kind := range []eraseOriginKind{eraseOriginFresh, eraseOriginReceiver, eraseOriginOpaque} {
		for _, site := range byOrigin[kind] {
			removed = append(removed, fmt.Sprintf("%s %s.%s = %s <- %s",
				site.at, site.inside, site.field, site.rhs, site.origin.kind))
		}
	}
	slices.Sort(removed)
	t.Logf("complement 2 -- the %d fill site(s) this gate ADMITTED because the array they write did not come from the caller:\n\t%s",
		len(removed), strings.Join(removed, "\n\t"))
	if len(removed) == 0 {
		t.Error("complement 2 is EMPTY: every fill site in this package is caller-rooted, so 'the origin is a parameter' removes nothing today and the clause is doing no work")
	}

	// COMPLEMENT 3: what the admission RESTS ON. A call this gate could not open is admitted on
	// the strength of that callee copying, and this gate did not check that it does. It is named
	// on every run so that a new name appearing here is visible in the diff of a test log rather
	// than invisible inside a pass.
	opaque := map[string][]string{}
	for _, site := range byOrigin[eraseOriginOpaque] {
		opaque[site.origin.what] = append(opaque[site.origin.what], site.at)
	}
	names := slices.Sorted(maps_Keys(opaque))
	t.Logf("complement 3 -- %d callee(s) this gate admitted WITHOUT opening, and every admission above rests on each of them copying: %v",
		len(names), names)

	// ---- the refusals ----
	for _, site := range byOrigin[eraseOriginUndecided] {
		t.Errorf("%s: %s fills %s with %s and this gate cannot tell where that array came from (%s). A fill site whose origin is unknown is REFUSED rather than admitted: an erase over a caller's array is silent, so the unknown case must be the failing one",
			site.at, site.inside, site.field, site.rhs, site.origin.what)
	}
	// ---- the caller-rooted sites, against their dispositions ----
	//
	// EVERY ONE OF THEM IS NAMED, on every run, whether it is dispositioned or not -- so the set
	// is readable out of a passing log and a reader never has to run the gate to see what it
	// admitted.
	held := map[string][]eraseFillSite{}
	for _, site := range byOrigin[eraseOriginParameter] {
		key := eraseSiteKey(site)
		reason, isHandedOver := eraseOwnershipHandovers[key]
		held[key] = append(held[key], site)
		if !isHandedOver {
			t.Errorf("%s: %s fills %s with %s, whose backing array is the caller's own parameter %q. This package ERASES that field, so this statement hands a caller's array to an erase the caller does not know about -- which is silent: an all-zero seed derives a perfectly valid public key and an all-zero HPKE key opens nothing while looking like a key. Either COPY it, in whatever spelling this file already uses, or record %q in eraseOwnershipHandovers with the call site that proves the caller hands the array over",
				site.at, site.inside, site.field, site.rhs, site.origin.what, key)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is dispositioned with an EMPTY reason, which is an allow-list entry wearing a justification", key)
			continue
		}
		t.Logf("caller-rooted and dispositioned -- %s (%s): %s", key, site.at, reason)
	}
	// AND THE TABLE IS HELD TO THE TREE IN BOTH DIRECTIONS. An entry that matches no site is a
	// disposition for a statement that no longer exists, and the next caller-rooted fill site to
	// land in that function would inherit it silently.
	for key := range eraseOwnershipHandovers {
		if len(held[key]) == 0 {
			t.Errorf("eraseOwnershipHandovers disposes of %q and no fill site of this package answers to it. A stale disposition is an exemption waiting for a statement it was never written about",
				key)
		}
	}
	// AND ONE REASON MAY NOT BE EVIDENCE ABOUT TWO STATEMENTS. Even with the array in the key, two
	// statements can spell the same array the same way in the same function, and the reason below
	// was read and written about ONE of them. The second would inherit a judgement nobody made
	// about it, which is the narrower form of the defect the key itself closes.
	for key, matching := range held {
		if len(matching) < 2 {
			continue
		}
		if _, isHandedOver := eraseOwnershipHandovers[key]; !isHandedOver {
			continue
		}
		places := []string{}
		for _, site := range matching {
			places = append(places, site.at)
		}
		slices.Sort(places)
		t.Errorf("eraseOwnershipHandovers disposes of %q and %d statements answer to it (%v). The reason was read and written about one of them; a second statement inheriting it is an exemption nobody granted it",
			key, len(matching), places)
	}
}

// eraseOwnershipHandovers is the DISPOSITION of the caller-rooted fill sites, and it is
// deliberately a list while the class above is deliberately not one.
//
// WHY THE SPLIT IS HERE AND NOT ONE LEVEL UP. The class -- "a field an erase reaches, filled from
// the caller's array" -- is derivable and is derived. What is NOT derivable from source is whether
// the caller goes on OWNING that array or HANDS IT OVER, and this package does both on purpose:
// (*SecretTree).newRatchet's own header says "taking ownership of the root secret ... so the caller
// must not keep it", while mls.JoinFromWelcome must never take ownership of a device's signing key.
// Telling those two apart needs escape analysis or a stated contract, and a gate that guessed would
// be widened at the first false refusal until it refused nothing.
//
// So the gate derives the class and REFUSES anything undisposed, and the judgement that a
// particular handover is safe is written down where a reader can check it -- which is GATES.md's
// own remedy, quoted: "A derived class whose literal is invisible and silent is worth less than an
// enumerated one that refuses and prints." A sixth caller-rooted fill site cannot land green. A
// fifth that stops existing cannot leave its exemption behind.
//
// THE KEY IS file:function.field AND NOT A LINE. A line-keyed exemption goes stale on the next
// edit above it and gets "refreshed" without being re-read, which is how an allow-list stops being
// read at all.
//
// Each reason names the EVIDENCE, and every one of them was read before it was written here.
var eraseOwnershipHandovers = map[string]string{
	"key_schedule.go:newKeyScheduleFromParts.joinerSecret = joinerSecret":   "the two exported constructors are the only callers and both hand over arrays they made: NewKeySchedule passes bytes.Clone(joinerSecret) at key_schedule.go:269 and NewKeyScheduleFromEpochSecret passes nil. The copy is at the CALL site rather than at the fill site, which is a choice about where the clone lives and not an alias.",
	"key_schedule.go:newKeyScheduleFromParts.welcomeSecret = welcomeSecret": "the same two callers, and welcomeSecret is derived inside NewKeySchedule one statement earlier (crypto.DeriveSecret(memberSecret, \"welcome\")) and never read again by it.",
	"key_schedule.go:newKeyScheduleFromParts.epochSecret = epochSecret":     "the same two callers, and epochSecret is derived inside NewKeySchedule one statement earlier (crypto.ExpandWithLabel) and never read again by it.",
	"secret_tree.go:(SecretTree).newRatchet.secret = rootSecret":            "an ownership transfer the function's own header STATES: \"taking ownership of the root secret: it is erased in place by the first step, so the caller must not keep it or pass a slice it still reads.\" The erase is (*ratchet).step's forward secrecy and the handover is the contract that makes it safe.",
	"welcome.go:BuildWelcome.PathSecret = joiner.PathSecret":                "the GroupSecrets this loop builds are marshalled and sealed and then dropped; BuildWelcome erases nothing. The array belongs to the caller's WelcomeJoiner entries and the caller is the one that erases it -- group.go:2409 calls joiners[i].Zeroize() after the seal. The erase that put PathSecret in this class is on the JOIN side, over a GroupSecrets this package decoded itself (group.go:3196).",
}

// eraseSiteKey names a fill site by file, enclosing function, field AND THE ARRAY IT WRITES --
// never by line.
//
// WHY THE ARRAY IS IN THE KEY, which it was not. The key was file:function.field, and each reason
// below was read and written about ONE statement -- a particular array, reached from a particular
// parameter. A key that stops at the field name is satisfied by any statement in that function
// binding that field, so a NEW caller-rooted fill landing beside a dispositioned one inherited its
// exemption in silence and printed, on every passing run, a reason about a different array. It was
// demonstrated by planting `secrets.PathSecret = &PathSecret{PathSecret: joinerSecret}` one line
// under BuildWelcome's own fill: green, and excused by a sentence about joiner.PathSecret.
//
// STILL NEVER A LINE. A line-keyed exemption goes stale on the next edit above it and gets
// "refreshed" without being re-read, which is how an allow-list stops being read at all. The
// rendered right-hand side is not a line: it survives every edit that does not change what the
// statement writes, and changes exactly when the array does.
//
// AND THE FUNCTION HALF IS THE SCOPE, spelled (Type).Method for a method and Function.funcN for a
// statement written inside the Nth function literal of one. Two methods of the same name on two
// types are two functions, and a bare `newRatchet` was a key both of them answered to.
func eraseSiteKey(site eraseFillSite) string {
	file := site.at
	if at := strings.LastIndex(file, ":"); at >= 0 {
		file = file[:at]
	}
	return fmt.Sprintf("%s:%s.%s = %s", file, site.inside, site.field, site.rhs)
}

// eraseSpellingCorpus is the control corpus of SPELLINGS, and the acceptance test driven over it is
// TestTheErasedFieldGateSeesAnAliasHoweverItIsSpelled below. It is the clause that says the gate is
// about aliasing rather than about spelling.
//
// IT IS AT PACKAGE LEVEL so that TestEveryReadingOfThisGateIsDrivenBySomething can drive it too:
// that check asks whether every reading this file DECLARES is taken over these corpora, which it
// cannot ask about a corpus locked inside one test function.
//
// It is here because the three sites the coupling rests on are written two different ways today,
// and this project has shipped a class defect keyed to ONE spelling nine times. So the resolver is
// driven over a control corpus of SPELLINGS, each with the origin it must answer -- and the answer
// is exact. A copy is not merely "not the caller's": it is FRESH, and the difference matters,
// because "a call this gate did not open" is admitted on trust and named in complement 3 while a
// fresh array is decided. Every copy below was silently allowed to degrade into an opaque
// admission until this table demanded the kind.
//
// THE TABLE IS THE POINT, and it replaced a loop over copyN/aliasN pairs. The delete-it-and-see
// check was run over EVERY clause of this file, and what it found is that the pairs drove the
// spelling arms and nothing else: the type-assertion peel, the make arm, the builtin arm, the
// []byte(nil) conversion, the named-result reading, the range-bound local, the result-position
// reading of a multi-value call, the scope-qualified cycle key and the opaque answer itself could
// each be DELETED with the whole suite, unfiltered, still green. A clause nothing drives is a
// comment. Each row below is one clause's driver, named in its own comment.
//
// It parses a source file of its own rather than reading the tree, because a control has to be able
// to contain the defect -- and it TYPE-CHECKS it, because a driver that could not compile is not
// evidence about a form real source can hold.
const eraseSpellingCorpus = `package mls

type Held struct{ Secret []byte }

//go:noinline
func wipe(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}

func (self *Held) Zeroize() { wipe(self.Secret) }

func copyOf(bs []byte) []byte {
	out := make([]byte, len(bs))
	copy(out, bs)
	return out
}

func passThrough(bs []byte) []byte { return bs }

type Named []byte

func copy1(x []byte) *Held  { return &Held{Secret: copyOf(x)} }
func copy2(x []byte) *Held  { return &Held{Secret: append([]byte(nil), x...)} }
func copy3(x []byte) *Held  { return &Held{Secret: Named(copyOf(x))} }
func copy4(x []byte) *Held  { return &Held{Secret: make([]byte, len(x))} }
func copy5(x []byte) *Held  { h := &Held{}; h.Secret = copyOf(x); return h }
func copy6(x []byte) *Held  { out := copyOf(x); return &Held{Secret: out} }
func copy7(x []byte) *Held  { return &Held{Secret: append(copyOf(x), 0)} }
func copy8(x []byte) *Held  { return &Held{Secret: nil} }

func alias1(x []byte) *Held { return &Held{Secret: x} }
func alias2(x []byte) *Held { return &Held{Secret: Named(x)} }
func alias3(x []byte) *Held { return &Held{Secret: x[:]} }
func alias4(x []byte) *Held { return &Held{Secret: x[1:4]} }
func alias5(x []byte) *Held { h := &Held{}; h.Secret = x; return h }
func alias6(x []byte) *Held { out := x; return &Held{Secret: out} }
func alias7(x []byte) *Held { return &Held{Secret: passThrough(x)} }
func alias8(x []byte) *Held { return &Held{Secret: append(x, 0)} }

// 9 -- the POSITIONAL composite literal. The field name is not written anywhere in the statement,
// so a gate reading only *ast.KeyValueExpr elements does not see the site AT ALL.
func copy9(x []byte) *Held  { return &Held{copyOf(x)} }
func alias9(x []byte) *Held { return &Held{x} }

// 10 -- the MULTI-VALUE assignment, which is how every decode in the real package binds the octets
// it just read. The array is result 0 and the error is result 1.
func two(x []byte) ([]byte, error)       { return x, nil }
func twoCopies(x []byte) ([]byte, error) { return copyOf(x), nil }

func copy10(x []byte) *Held {
	h := &Held{}
	var err error
	h.Secret, err = twoCopies(x)
	_ = err
	return h
}

func alias10(x []byte) *Held {
	h := &Held{}
	var err error
	h.Secret, err = two(x)
	_ = err
	return h
}

// 11 -- a METHOD-CALL spelling, declared in this package on a type this package declares. The body
// is already parsed here, so admitting it as "a call this gate did not open" was a false statement
// about this gate's own reach.
type Box struct{ inner []byte }

func (self *Box) Raw() []byte    { return self.inner }
func (self *Box) Copied() []byte { return copyOf(self.inner) }

func copy11(b *Box) *Held  { return &Held{Secret: b.Copied()} }
func alias11(b *Box) *Held { return &Held{Secret: b.Raw()} }

// 12 -- the positional literal with its type ELIDED, which is the form that has to be given a type
// by the literal containing it before its fields can be named at all.
func copy12(x []byte) []Held  { return []Held{{copyOf(x)}} }
func alias12(x []byte) []Held { return []Held{{x}} }

// 13 -- a forwarded multi-value return, which answered "fresh" having read no return statement at
// all while the single-result restriction stood.
func forwardsAnAlias(x []byte) ([]byte, error) { return two(x) }
func forwardsACopy(x []byte) ([]byte, error)   { return twoCopies(x) }

func copy13(x []byte) *Held {
	h := &Held{}
	var err error
	h.Secret, err = forwardsACopy(x)
	_ = err
	return h
}

func alias13(x []byte) *Held {
	h := &Held{}
	var err error
	h.Secret, err = forwardsAnAlias(x)
	_ = err
	return h
}

// 14 -- a fill site inside a FUNCTION LITERAL, handed the caller's array through a CLOSURE
// PARAMETER whose name also exists as a fresh local one frame out. The site is seen either way;
// what was wrong is the FRAME it was resolved in, and the enclosing local is exactly what made the
// wrong frame answer "fresh" instead of refusing.
func copy14(x []byte) *Held {
	h := &Held{}
	shared := make([]byte, 4)
	_ = shared
	fill := func(shared []byte) { h.Secret = copyOf(shared) }
	fill(x)
	return h
}

func alias14(x []byte) *Held {
	h := &Held{}
	shared := make([]byte, 4)
	_ = shared
	fill := func(shared []byte) { h.Secret = shared }
	fill(x)
	return h
}

// 15 -- a method PROMOTED from an embedded struct: declared in this package, on a type this
// package declares, with its body already parsed here, and admitted unopened all the same because
// the method map was keyed to the outer type's own name.
type Inner struct{ kept []byte }

func (self *Inner) Kept() []byte     { return self.kept }
func (self *Inner) KeptCopy() []byte { return copyOf(self.kept) }

type Outer struct{ Inner }

func copy15(o *Outer) *Held  { return &Held{Secret: o.KeptCopy()} }
func alias15(o *Outer) *Held { return &Held{Secret: o.Kept()} }

// 16 -- a conversion through string, which is a COPY in Go and is decided by the builtin arm. It
// has no alias twin: there is no spelling of string(x) that keeps x's array.
func copy16(x []byte) *Held { return &Held{Secret: []byte(string(x))} }

// 17 -- a NAMED result assigned and returned bare, which is a body with no return expression to
// read at the position the caller asked about.
func namedAlias(x []byte) (out []byte) { out = x; return }
func namedCopy(x []byte) (out []byte)  { out = copyOf(x); return }

func copy17(x []byte) *Held  { return &Held{Secret: namedCopy(x)} }
func alias17(x []byte) *Held { return &Held{Secret: namedAlias(x)} }

// 18 -- a callee holding a FUNCTION LITERAL that returns the caller's array. The literal's return
// is not this function's return, and counting it made a copy look like an alias.
func hidesAnAliasInALiteral(x []byte) []byte {
	inner := func() []byte { return x }
	_ = inner
	return copyOf(x)
}

func copy18(x []byte) *Held { return &Held{Secret: hidesAnAliasInALiteral(x)} }

// 19 -- an erased field bound at RESULT POSITION 1, which is the case the result-position reading
// exists for and the one no pair above reached: every spelling above binds the field at result 0.
func pairOut(x []byte) (error, []byte)     { return nil, x }
func pairOutCopy(x []byte) (error, []byte) { return nil, copyOf(x) }

func copy19(x []byte) *Held {
	h := &Held{}
	var err error
	err, h.Secret = pairOutCopy(x)
	_ = err
	return h
}

func alias19(x []byte) *Held {
	h := &Held{}
	var err error
	err, h.Secret = pairOut(x)
	_ = err
	return h
}

// 20 -- a TYPE ASSERTION in front of the array, and a var declaration behind it.
func copy20(x []byte) *Held {
	var boxed any = copyOf(x)
	return &Held{Secret: boxed.([]byte)}
}

func alias20(x []byte) *Held {
	var boxed any = x
	return &Held{Secret: boxed.([]byte)}
}

// 21 -- two locals of the SAME NAME on one resolution chain, in two different functions. A cycle
// guard keyed to the name alone calls the second one a cycle and answers "fresh" having read
// nothing.
func handOver(x []byte) []byte     { out := x; return out }
func handOverCopy(x []byte) []byte { out := copyOf(x); return out }

func copy21(x []byte) *Held  { h := &Held{}; out := handOverCopy(x); h.Secret = out; return h }
func alias21(x []byte) *Held { h := &Held{}; out := handOver(x); h.Secret = out; return h }

// 22 -- a local bound by a RANGE over the caller's slice of slices.
func alias22(chunks [][]byte) *Held {
	h := &Held{}
	for _, piece := range chunks {
		h.Secret = piece
	}
	return h
}

// 23 -- a FIELD of a parameter, which the selector arm names in full so the message says which
// array it is rather than only which parameter it came through.
func alias23(b *Box) *Held { return &Held{Secret: b.inner} }

// 25 -- a PACKAGE-LEVEL array, which is neither a parameter, nor the receiver, nor a local of any
// scope enclosing the statement. It must be REFUSED as unknown and never admitted as fresh.
var packageLevel = make([]byte, 4)

func undecided25(x []byte) *Held { _ = x; return &Held{Secret: packageLevel} }

// 26 -- a call through a FUNCTION VALUE, which is a callee this gate cannot open however many
// files it has parsed. It is the open edge: admitted, and NAMED in complement 3.
var handler = passThrough

func opaque26(x []byte) *Held { return &Held{Secret: handler(x)} }

// 27 -- a method reached through an INTERFACE, the other half of the same edge. Reading the one
// implementation that happens to live here would be a guess wearing a derivation.
type Source interface{ Bytes() []byte }

func opaque27(s Source) *Held { return &Held{Secret: s.Bytes()} }

// 28 -- the COMMA-OK forms at result 1. The class is derived by field NAME, deliberately, so a
// same-named field of another type is a member of it -- and result 1 of a map index or a channel
// receive is a bool that carries no array at all.
type Flag struct{ Secret bool }

func commaOk28(m map[string][]byte, f *Flag) []byte {
	var v []byte
	v, f.Secret = m["k"]
	return v
}

func channelOk28(ch chan []byte, f *Flag) []byte {
	var v []byte
	v, f.Secret = <-ch
	return v
}

// 24 -- a local bound at a RESULT POSITION OTHER THAN 0, which is the fifth defect of this line and
// the only pair here that differs in WHICH result of one callee holds the caller's array. Before the
// repair in eraseAssignmentsTo, alias24 answered "a fresh array" -- the origin of result 0, an array
// that local never receives.
func twoOut(x []byte) ([]byte, []byte)     { return copyOf(x), x }
func twoOutCopy(x []byte) ([]byte, []byte) { return x, copyOf(x) }

func alias24(x []byte) *Held { h := &Held{}; _, second := twoOut(x); h.Secret = second; return h }
func copy24(x []byte) *Held  { h := &Held{}; _, second := twoOutCopy(x); h.Secret = second; return h }

// 25 -- a COMPOSITE LITERAL standing where the array goes.
func copy25(x []byte) *Held { _ = x; return &Held{Secret: []byte{1, 2, 3}} }

// 26 -- a method answering its own receiver as a BARE NAME, behind a selector whose base is a call
// rather than a name, so the selector must be peeled instead of read.
func (self *Box) itself() *Box { return self }

func alias26(b *Box) *Held { return &Held{Secret: b.itself().inner} }

// 27 -- a RECURSIVE callee. Without a cycle key on the call, resolving this never terminates.
func recurse(x []byte) []byte {
	if len(x) == 0 {
		return recurse(x)
	}
	return copyOf(x)
}

func copy27(x []byte) *Held { return &Held{Secret: recurse(x)} }

// 28 -- a LOCAL assigned from ITSELF, which is the cycle key of a local rather than of a call.
func copy28(x []byte) *Held {
	out := copyOf(x)
	out = append(out, 0)
	h := &Held{}
	h.Secret = out
	return h
}

// 29 -- an ADDRESS-OF and a DEREFERENCE standing between the fill and the array.
func copy29(x []byte) *Held {
	c := copyOf(x)
	p := &c
	return &Held{Secret: *p}
}

// 30 -- PARENTHESES, which preserve the array and are peeled before anything else is read.
func copy30(x []byte) *Held { return &Held{Secret: (copyOf(x))} }

// 31 -- a VARIADIC callee answering a parameter that has NO argument at this call site. The
// caller-side mapping must refuse rather than index past the arguments it was given.
func firstOf(a []byte, rest ...[]byte) []byte { _ = a; return rest[0] }

func undecided31(x []byte) *Held { return &Held{Secret: firstOf(x)} }

// 32 -- the same refusal reached through a FORWARDED multi-value return.
func forwardsVariadic(a []byte, rest ...[]byte) ([]byte, error) { _ = a; return two(rest[0]) }

func undecided32(x []byte) *Held {
	h := &Held{}
	s, _ := forwardsVariadic(x)
	h.Secret = s
	return h
}

// 33 -- a METHOD whose body FORWARDS a multi-value call that answers the receiver's own state, so
// the answer has to be mapped back onto the expression the method was called on.
func (self *Box) twoInner() ([]byte, error) { return two(self.inner) }

func alias33(b *Box) *Held {
	h := &Held{}
	s, _ := b.twoInner()
	h.Secret = s
	return h
}

// 34 -- a FORWARDED return into a callee this gate cannot open, which must stay opaque rather than
// fall through to "a fresh array".
var twoHandler = two

func forwardsOpaque(x []byte) ([]byte, error) { return twoHandler(x) }

func opaque34(x []byte) *Held {
	h := &Held{}
	s, _ := forwardsOpaque(x)
	h.Secret = s
	return h
}

// 35 -- a callee with SEVERAL returns, one of which this gate cannot open and none of which is
// caller-rooted. The opaque one has to win: a body this gate only half read must not answer fresh.
func maybeOpaque(x []byte) []byte {
	if len(x) == 0 {
		return handler(x)
	}
	return copyOf(x)
}

func opaque35(x []byte) *Held { return &Held{Secret: maybeOpaque(x)} }

// 36 -- a method call on a struct this package DECLARES that declares no such method and embeds no
// struct that does, because what it embeds is an INTERFACE. Promotion refuses and the call stays
// opaque, which is the line an embedded interface sits on.
type Wrapped struct{ Source }

func opaque36(w *Wrapped) *Held { return &Held{Secret: w.Bytes()} }

// 37 -- a method call on a receiver whose declared type this gate cannot name AT ALL.
var boxes = func() *Box { return &Box{} }

func opaque37(x []byte) *Held { _ = x; return &Held{Secret: boxes().Raw()} }

// 38 -- a CHANNEL RECEIVE where the array goes: the one unary operator that is neither address-of
// nor a computation over an array this function already holds.
func copy38(ch chan []byte) *Held { return &Held{Secret: <-ch} }

// 39 -- MORE PEELS THAN THE RESOLVER HAS DEPTH FOR. It must run out and REFUSE; a resolver that ran
// out and answered would answer "fresh" for an array it never reached.
func undecided39(x []byte) *Held { return &Held{Secret: ((((((((((((x)))))))))))) } }
`

// TestTheErasedFieldGateSeesAnAliasHoweverItIsSpelled drives the resolver over every spelling of
// eraseSpellingCorpus and asserts the KIND each must answer, never merely that a copy "is not the
// caller's" -- fresh and opaque are different admissions and opaque is admitted on trust.
func TestTheErasedFieldGateSeesAnAliasHoweverItIsSpelled(t *testing.T) {
	fileSet, sources := eraseControlCorpus(t, "erased_field_alias_control.go", eraseSpellingCorpus)

	helpers := eraseHelpersIn(sources)
	if !slices.Contains(helpers, "wipe") {
		t.Fatalf("the erase helper derivation did not find the control's own erase, which is spelled `wipe`: it found %v. The derivation is keyed to a NAME and this whole gate is void",
			helpers)
	}
	erased, _ := eraseFieldsIn(fileSet, sources, helpers)
	if !erased["Secret"] {
		t.Fatalf("the erased-field derivation did not find Held.Secret: it found %v",
			slices.Sorted(maps_Keys(erased)))
	}
	sites, refusals, removedForms, _ := eraseFillSitesIn(fileSet, sources, erased)
	for _, refusal := range refusals {
		t.Errorf("the control corpus holds `%s` at %s, which this gate refused as %s. Every form in THIS corpus is one this gate is supposed to READ; the forms it is supposed to refuse have a corpus of their own",
			refusal.src, refusal.at, refusal.form)
	}
	if len(removedForms) == 0 {
		t.Error("the form walk removed nothing at all over the control corpus, so complement 4 is empty here and the walk is admitting every binding position it sees")
	}

	// KEYED BY THE DECLARATION AND NOT BY THE SCOPE, because spelling 14's site is written inside
	// a function literal and answers to copy14.func1 -- which is the whole of what it is for.
	verdict := map[string]eraseFillSite{}
	for _, site := range sites {
		verdict[site.declaration] = site
	}
	// EVERY SPELLING, WITH THE KIND IT MUST ANSWER AND THE CLAUSE THAT ANSWERS IT. "not the
	// caller's" is not the assertion: the KIND is, because fresh and opaque are different
	// admissions and a clause that degraded one into the other would pass a weaker test.
	for _, spelling := range []struct {
		inside string
		expect eraseOriginKind
		what   string
		drives string
	}{
		{"copy1", eraseOriginFresh, "", "one hop into a callee this package declares"},
		{"copy2", eraseOriginFresh, "", "append onto a []byte(nil) written as a TYPE LITERAL"},
		{"copy3", eraseOriginFresh, "", "a conversion to a type this package declares"},
		{"copy4", eraseOriginFresh, "", "the make arm"},
		{"copy5", eraseOriginFresh, "", "the one-to-one assignment form"},
		{"copy6", eraseOriginFresh, "", "a local standing in front of the value"},
		{"copy7", eraseOriginFresh, "", "append's DESTINATION deciding the append"},
		{"copy8", eraseOriginFresh, "", "nil"},
		{"copy9", eraseOriginFresh, "", "the positional composite literal"},
		{"copy10", eraseOriginFresh, "", "the multi-value assignment form"},
		{"copy11", eraseOriginFresh, "", "a method call this package declares"},
		{"copy12", eraseOriginFresh, "", "a positional literal with an ELIDED type"},
		{"copy13", eraseOriginFresh, "", "a forwarded multi-value return"},
		{"copy14", eraseOriginFresh, "", "a site inside a function literal"},
		{"copy15", eraseOriginFresh, "", "a method PROMOTED from an embedded struct"},
		{"copy16", eraseOriginFresh, "", "the builtin arm, over string"},
		{"copy17", eraseOriginFresh, "", "the NAMED-result reading of a bare return"},
		{"copy18", eraseOriginFresh, "", "the return walk STOPPING at a nested function literal"},
		{"copy19", eraseOriginFresh, "", "the result-position reading, at result 1"},
		{"copy20", eraseOriginFresh, "", "the type-assertion peel and the var-declaration form"},
		{"copy21", eraseOriginFresh, "", "the SCOPE-qualified cycle key"},

		{"alias1", eraseOriginParameter, "x", "the parameter answer itself"},
		{"alias2", eraseOriginParameter, "x", "a conversion to a type this package declares"},
		{"alias3", eraseOriginParameter, "x", "the slice-expression peel"},
		{"alias4", eraseOriginParameter, "x", "the slice-expression peel"},
		{"alias5", eraseOriginParameter, "x", "the one-to-one assignment form"},
		{"alias6", eraseOriginParameter, "x", "a local standing in front of a parameter"},
		{"alias7", eraseOriginParameter, "x", "the parameter-to-argument mapping of one hop"},
		{"alias8", eraseOriginParameter, "x", "append's DESTINATION deciding the append"},
		{"alias9", eraseOriginParameter, "x", "the positional composite literal"},
		{"alias10", eraseOriginParameter, "x", "the multi-value assignment form"},
		{"alias11", eraseOriginParameter, "b", "the RECEIVER mapping of one hop into a method"},
		{"alias12", eraseOriginParameter, "x", "a positional literal with an ELIDED type"},
		{"alias13", eraseOriginParameter, "x", "a forwarded multi-value return"},
		{"alias14", eraseOriginParameter, "shared", "a function literal being a SCOPE of its own"},
		{"alias15", eraseOriginParameter, "o", "a method PROMOTED from an embedded struct"},
		{"alias17", eraseOriginParameter, "x", "the NAMED-result reading of a bare return"},
		{"alias19", eraseOriginParameter, "x", "the result-position reading, at result 1"},
		{"alias20", eraseOriginParameter, "x", "the type-assertion peel and the var-declaration form"},
		{"alias21", eraseOriginParameter, "x", "the SCOPE-qualified cycle key"},
		{"alias22", eraseOriginParameter, "chunks", "the RANGE form of a local's assignments"},
		{"alias23", eraseOriginParameter, "b.inner", "the selector arm, which names the FIELD and not only the parameter"},

		{"undecided25", eraseOriginUndecided, "", "the refusal of a name no scope of the chain binds"},
		{"opaque26", eraseOriginOpaque, "handler", "the opaque answer for a callee this package does not declare"},
		{"opaque27", eraseOriginOpaque, "s.Bytes", "the opaque answer for a method reached through an INTERFACE"},
		{"commaOk28", eraseOriginFresh, "", "the comma-ok arm of the result-position reading"},
		{"channelOk28", eraseOriginFresh, "", "the channel-receive arm of the result-position reading"},

		{"alias24", eraseOriginParameter, "x", "the MULTI-VALUE form of a local's assignments AT ITS OWN RESULT POSITION -- the fifth defect of this line, which answered `a fresh array` here"},
		{"copy24", eraseOriginFresh, "", "the same reading, at the result that does hold a copy"},
		{"copy25", eraseOriginFresh, "", "the literal arm, over a composite literal"},
		{"alias26", eraseOriginParameter, "b", "the selector PEEL onto a base that is a call, and the RECEIVER binding of a bare name"},
		{"copy27", eraseOriginFresh, "", "the RESULT-QUALIFIED cycle key of a callee"},
		{"copy28", eraseOriginFresh, "", "the SCOPE-qualified cycle key of a local"},
		{"copy29", eraseOriginFresh, "", "the dereference peel and the address-of peel"},
		{"copy30", eraseOriginFresh, "", "the parenthesis peel"},
		{"undecided31", eraseOriginUndecided, "", "the refusal of a callee's parameter this gate could not map back onto an argument, and the index peel"},
		{"undecided32", eraseOriginUndecided, "", "the same refusal on a FORWARDED return"},
		{"alias33", eraseOriginParameter, "b", "a FORWARDED return answering the callee's RECEIVER state"},
		{"opaque34", eraseOriginOpaque, "forwardsOpaque forwards: twoHandler", "a FORWARDED return answering OPAQUE, carried out under the callee's name"},
		{"opaque35", eraseOriginOpaque, "maybeOpaque: handler", "a callee answering OPAQUE on one of several returns, which must beat the fresh ones"},
		{"opaque36", eraseOriginOpaque, "w.Bytes", "the refusal of a method neither declared on the receiver's type nor promoted from a struct it embeds"},
		{"opaque37", eraseOriginOpaque, "boxes().Raw", "the refusal of a receiver whose declared type this gate could not name"},
		{"copy38", eraseOriginFresh, "", "the unary arm, over a channel receive"},
		{"undecided39", eraseOriginUndecided, "", "the resolver's own DEPTH LIMIT, which must refuse rather than answer"},
	} {
		site, seen := verdict[spelling.inside]
		if !seen {
			t.Errorf("%s fills an erased field and the gate did not see the site at all. The clause it drives is %s",
				spelling.inside, spelling.drives)
			continue
		}
		if site.origin.kind != spelling.expect {
			t.Errorf("%s must answer %q and the gate answers %q (%s). The clause it drives is %s, and a clause nothing drives is a comment: deleting it left an unfiltered run over all three trees reading exactly what it read before",
				spelling.inside, spelling.expect, site.origin.kind, site.origin.what, spelling.drives)
			continue
		}
		if spelling.what != "" && site.origin.what != spelling.what {
			t.Errorf("%s answers %q and this gate names the array %q rather than %q. The clause it drives is %s",
				spelling.inside, spelling.expect, site.origin.what, spelling.what, spelling.drives)
		}
	}
	t.Logf("the resolver decided %d spellings across every binding position, both scopes, and each of the two admissions it makes -- fresh and opaque -- by kind and not by whether the answer merely was not the caller's",
		len(sites))
}

// eraseControlCorpus parses one control corpus AND TYPE-CHECKS IT.
//
// THE TYPE CHECK IS NOT DECORATION. Every corpus below is the DRIVER for arms of a gate that reads
// real source, and an arm driven only by something that could never compile is an arm whose driver
// is a fiction -- which is the same defect as an arm driven by nothing, one step less obvious.
// go/types answers it with no importer at all, because a corpus that imports nothing is a whole
// package on its own.
func eraseControlCorpus(t *testing.T, path string, source string) (*token.FileSet, []eraseSource) {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control corpus %s: %v", path, err)
	}
	problems := []string{}
	config := &types.Config{Error: func(problem error) {
		problems = append(problems, problem.Error())
	}}
	if _, checked := config.Check("mls", fileSet, []*ast.File{parsed}, nil); checked != nil ||
		len(problems) != 0 {
		t.Fatalf("the control corpus %s is not compilable Go (%v / %v), so nothing it drives is evidence about a form real source can hold",
			path, checked, problems)
	}
	return fileSet, []eraseSource{{path: path, parsed: parsed}}
}

// TestTheFormWalkDecidesEveryBindingPositionAndRefusesTheOnesItCannotPlace is the driver for the
// other half of the walk: the COMPLEMENT.
//
// WHY IT EXISTS, and it is the finding that raised it. The form class was widened to all four
// binding positions Go has, and five of the clauses that went in with it were checked the way this
// project checks a clause -- delete it and confirm something notices. The rest were not. Run over
// every clause of this file, that check found SIXTEEN readings of this walk that could be deleted
// with an unfiltered run over all three trees reading exactly what it read before: the three whole
// arms the finding named -- *ast.RangeStmt, *ast.IncDecStmt and the operator assignment -- and then
// four of the refusals and NINE of the thirteen non-member counts. Complement 4 was asserted only
// to be NON-EMPTY, and non-empty is satisfied by twelve readings when there are thirteen.
//
// So the complement is asserted HERE, exactly, reading by reading. A count that stops being taken
// is a form this walk stopped deciding, and it turns this red at the reading's own name.
//
// THE CORPUS IS COMPILABLE GO and is type-checked to prove it, which is what makes it evidence
// about forms real source can hold. `x.Field++` and `x.Field += 2` cannot be written over a byte
// slice, so the fixture writes them over a field of a DIFFERENT type with the SAME NAME -- which is
// not a dodge: this gate's class is derived by field NAME on purpose, it says so, it over-reports
// at the boundary deliberately, and that over-report is exactly how those two arms are reachable in
// real source. The range form needs no such help: `for _, h.Secret = range chunks` is the one
// binding position of the four that can hand a caller's array to an erased field with no assignment
// operator anywhere in the statement.
//
// TWO REFUSALS ARE NOT DRIVEN HERE AND CANNOT BE, and they are named rather than left to look
// driven:
//
//   - "a positional element past the end of the field list this gate read for its type". A struct
//     literal with more elements than the struct has fields is a COMPILE ERROR, and the field list
//     this gate reads appends exactly one entry per declared field -- an embedded field included,
//     named by its type. There is no compilable Go that reaches it. It stays as the bounds guard on
//     an index, because what deleting it would produce is a panic rather than a wrong answer.
//   - "an assignment whose two sides have different lengths and whose right side is not a single
//     expression". Go has no such assignment: either the two sides have equal length or the right
//     side is one multi-valued expression. It stays as the walk's own totality guard, and it is the
//     line that makes "every sub-form is decided" true rather than approximately true.
//
// Both are stated here and in GATES.md, and the query that says they are unreached today is the
// gate's own run: the refusal list over this package's production source is empty on every pass.
const eraseFormCorpus = `package mls

type Held struct{ Secret []byte }

func wipe(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}

func (self *Held) Zeroize() { wipe(self.Secret) }

// Counter's field has the SAME NAME as the erased one and a type no erase can reach, which is the
// only shape in which Go lets ++ and += touch a member of this gate's by-name class.
type Counter struct{ Secret int }

func bumps(c *Counter) { c.Secret++ }
func drops(c *Counter) { c.Secret-- }
func adds(c *Counter)  { c.Secret += 2 }
func ors(c *Counter)   { c.Secret |= 1 }

// the RANGE form, which rebinds the field once per iteration out of a caller's slice of slices.
func ranges(h *Held, chunks [][]byte) {
	for _, h.Secret = range chunks {
	}
}

// Pair carries no field any erase reaches, which is what the "binds a field no erase reaches"
// readings are about -- keyed and positional.
type Pair struct {
	A int
	B int
}

func keyed(p *Pair)      { *p = Pair{A: 1, B: 2} }
func empty() *Pair       { return &Pair{} }
func positional() *Pair  { return &Pair{1, 2} }
func sliceOf() []Pair    { return []Pair{{1, 2}} }
func assigns(p *Pair)    { p.A = 1 }
func indexed() []int     { m := map[int]int{}; return []int{0: m[0]} }

// a literal with SOME keyed elements and some not, which Go permits for a slice, an array and a
// map -- and which the first version of this reading called a form "Go does not permit".
func mixed() []Pair { return []Pair{0: {1, 2}, {3, 4}} }

// a positional literal of a type declared INSIDE a function, which this gate's type reading only
// ever sees at package level and therefore cannot name.
func local(x []byte) []byte {
	type hidden struct{ Secret []byte }
	h := hidden{x}
	return h.Secret
}

// the multi-value forms onto targets no erase reaches.
func pairOf(x []byte) (int, error) { return len(x), nil }

func multi(p *Pair) error {
	var err error
	p.A, err = pairOf(nil)
	return err
}

func multiLocal() error {
	total, err := pairOf(nil)
	_ = total
	return err
}

// and the same three positions onto things NO erase reaches, which is what each arm's non-member
// reading counts into complement 4.
func counts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	total++
	return total
}

func walks(xs []int) int {
	var at int
	seen := 0
	for at = range xs {
		seen = at
	}
	return seen
}
`

// TestTheFormWalkDecidesEveryBindingPositionAndRefusesTheOnesItCannotPlace drives the COMPLEMENT of
// the form walk over eraseFormCorpus, reading by reading and exactly.
func TestTheFormWalkDecidesEveryBindingPositionAndRefusesTheOnesItCannotPlace(t *testing.T) {
	fileSet, sources := eraseControlCorpus(t, "erased_field_form_control.go", eraseFormCorpus)

	helpers := eraseHelpersIn(sources)
	if !slices.Contains(helpers, "wipe") {
		t.Fatalf("the erase helper derivation did not find this corpus's own erase: it found %v", helpers)
	}
	erased, _ := eraseFieldsIn(fileSet, sources, helpers)
	if !erased["Secret"] {
		t.Fatalf("the erased-field derivation did not find Held.Secret: it found %v",
			slices.Sorted(maps_Keys(erased)))
	}
	sites, refusals, removedForms, _ := eraseFillSitesIn(fileSet, sources, erased)

	// ONE FILL SITE, and it is the one the refusals are measured against. `hidden{x}` inside
	// local() binds Secret positionally out of a caller's array, and the gate cannot name the type
	// -- so it must be REFUSED and must NOT appear here.
	for _, site := range sites {
		t.Errorf("%s: %s.%s = %s was recorded as a FILL SITE. Every binding position in this corpus is one no member of the class can be written in, or one this gate must refuse; a site here is an arm that stopped deciding its form and let the next arm read it",
			site.at, site.inside, site.field, site.rhs)
	}

	counted := map[string]int{}
	for _, refusal := range refusals {
		counted[refusal.form] += 1
	}
	for _, expected := range []struct {
		form   string
		times  int
		drives string
	}{
		{"an increment or decrement of Secret, a field an erase reaches", 2,
			"the *ast.IncDecStmt arm, over c.Secret++ and c.Secret--"},
		{"an operator assignment onto Secret, a field an erase reaches", 2,
			"the operator-assignment reading of *ast.AssignStmt, over c.Secret += 2 and c.Secret |= 1"},
		{"a range binding Secret, a field an erase reaches, once per iteration", 1,
			"the *ast.RangeStmt arm, over for _, h.Secret = range chunks"},
		{"an element with no key in a composite literal whose other elements have one, which this gate cannot place", 1,
			"the mixed-element refusal, over []Pair{0: {1, 2}, {3, 4}} -- a form Go DOES permit for a slice, an array and a map"},
		{"a POSITIONAL composite literal whose type this gate could not name, so it cannot say which field each element binds", 1,
			"the unnamed-type refusal, over a struct type declared inside a function"},
	} {
		if counted[expected.form] == expected.times {
			continue
		}
		t.Errorf("this corpus holds %d binding position(s) that must be refused as %q and the gate refused %d. That clause is %s, and a clause nothing drives is a comment",
			expected.times, expected.form, counted[expected.form], expected.drives)
	}
	for form, times := range counted {
		t.Logf("refused %d x %s", times, form)
	}

	// AND THE COMPLEMENT, EXACTLY. Asserting only that it is non-empty is what let six of these
	// readings stop being taken with nothing anywhere noticing.
	expectedComplement := map[string]int{
		"a keyed element whose key is not an identifier, so it keys a map or an array and binds no field": 2,
		"a keyed element binding a field no erase of this package reaches":                                2,
		"a composite literal with no elements, which binds nothing":                                       2,
		"a positional literal of a slice, array or map type, whose elements bind no field":                1,
		"a positional element binding a field no erase of this package reaches":                           8,
		"a one-to-one assignment onto something that is not a field":                                      8,
		"a one-to-one assignment onto a field no erase of this package reaches":                           1,
		"a multi-value assignment onto something that is not a field":                                     3,
		"a multi-value assignment onto a field no erase of this package reaches":                          1,
		"a range that declares its own variables or binds none, which binds no field":                     2,
		"a range binding with =, onto targets no erase of this package reaches":                           1,
		"an increment or decrement, which cannot rebind a slice field":                                    1,
		"an operator assignment, which writes through a field's array and cannot rebind it":               3,
	}
	for reason, times := range expectedComplement {
		if removedForms[reason] == times {
			continue
		}
		t.Errorf("this corpus holds %d binding position(s) that must be DECIDED not to be members under %q and the walk counted %d. A form nothing counts is a form nothing can miss, and complement 4's only other clause -- that it is not EMPTY -- is satisfied by twelve readings when there are thirteen",
			times, reason, removedForms[reason])
	}
	for reason, times := range removedForms {
		if _, expected := expectedComplement[reason]; !expected {
			t.Errorf("the walk decided %d binding position(s) over this corpus under %q, which this control says nothing about. A reading with no row here is a reading nothing drives",
				times, reason)
		}
	}
	t.Logf("every binding position Go has that cannot legally carry a member of this class was refused by name and line or counted under its own reading, over a corpus go/types accepts: %d refusal(s), %d reading(s) in the complement",
		len(refusals), len(removedForms))
}

// ---------------------------------------------------------------------------
// the standing check: is every reading of this gate driven by something?
// ---------------------------------------------------------------------------

// eraseRegisteredReadingFunctions are the functions whose arms the register covers, and the list is
// the BOUNDARY -- it is stated here rather than implied, because what a check cannot see is the only
// thing worth naming about it.
//
// WHAT IS IN: every reading that ANSWERS AN ORIGIN or DECIDES A BINDING POSITION. That is where all
// thirty-three of the undriven clauses the last round found were living.
//
// WHAT IS OUT, and this is the register's largest blind spot: the readings that answer a TYPE or a
// SET rather than an origin -- declaredTypeNameOf, eraseDeclaredTypeOfName, promotedMethod,
// eraseShapeOf, shapeOf, elementTypesOf, eraseCompositeShapes -- and the class derivations
// themselves, eraseHelpersIn, eraseFieldsIn and eraseSiblingFields, and the traversal helper
// eraseInspectScope. An arm added to any of those can be driven by nothing and this check stays
// green. They are not ungated: each is a t.Fatal on empty in the gate itself, which is a different
// and weaker guarantee, and it is stated in GATES.md as such.
var eraseRegisteredReadingFunctions = []string{
	"eraseFillSitesIn",
	"originOf",
	"originOfResult",
	"originOfIdent",
	"originOfCall",
	"originOfMethodCall",
	"originOfBody",
	"eraseAssignmentsTo",
	"eraseReturnExpressionsAt",
}

// eraseUnreachedReadings are the readings this file DECLARES that no compilable Go drives, each with
// the reason -- and the register asserts both halves: a row here that is never taken is fine, a row
// here that IS taken is red, and a row naming a reading this file no longer declares is red.
//
// THIS IS THE HONEST HALF OF THE REGISTER. Without it, "every declared reading is exercised" would
// have to be made true by deleting guards that exist to stop a panic, or by widening the corpus with
// Go that does not compile -- which is the shape of driver the last round already threw out. A
// reading kept because deleting it produces a panic rather than a wrong answer belongs here, named,
// with the reason readable beside it.
var eraseUnreachedReadings = map[string]string{
	"a positional element past the end of the field list this gate read for its type": "UNREACHABLE: a struct literal with more elements than the struct has fields is a COMPILE ERROR, and the field list this gate reads appends exactly one entry per declared field, an embedded one included and named by its type. It stays as the bounds guard on an index, because what deleting it produces is a PANIC rather than a wrong answer.",

	"an assignment whose two sides have different lengths and whose right side is not a single expression": "UNREACHABLE: Go's grammar has no such assignment -- either the two sides have equal length, or the right side is one multi-valued expression. It stays as the walk's own totality guard, and it is the line that makes \"every sub-form is decided\" true rather than approximately true.",

	"the UNDECIDED default of the result-position reading": "UNREACHABLE: only a call, a map index, a type assertion and a channel receive are multi-valued in Go; each has its own arm above, and parentheses are peeled before the switch reads the node. The default is what makes the reading TOTAL rather than approximately total.",

	"append with NO arguments at all": "UNREACHABLE: append() with no arguments does not compile -- \"not enough arguments in call to append\". It stays as the bounds guard on call.Args[0], because what deleting it produces is a PANIC rather than a wrong answer. THIS REGISTER FOUND IT; the seventy-one-clause deletion sweep of the round before never named it.",

	"a callee answering RECEIVER state that this gate called as a plain function": "UNREACHABLE: eraseOriginReceiver is answered only where eraseBindingOf finds a name bound as the RECEIVER of some scope of the chain, and the chain of a plain function declares none -- eraseScopeOf(declared).receiver() is nil for every *ast.FuncDecl with no Recv. A body this gate called with receiver == nil therefore cannot answer receiver state. It stays as the arm that REFUSES rather than resolving a nil receiver expression.",

	"a FORWARDED return answering receiver state out of a plain function": "UNREACHABLE: the same argument as the arm above, on the forwarded half. A plain function's scope chain binds no receiver, so a forwarded return out of one cannot answer receiver state, and the arm refuses rather than resolving a nil receiver expression.",

	"the REFUSAL of an expression form this gate has no peel for": "UNREACHED: no compilable driver was found, and that is a WEAKER claim than the six above rather than the same one. Every Go expression that evaluates to a slice is one of the fourteen ast.Expr kinds this switch already names. What is left is *ast.BadExpr, which only a parse error produces and which this gate fatals on before reading; the type-expression nodes, which are not values; and *ast.IndexListExpr, a generic instantiation with TWO type arguments, which is a function value and which no peel of this resolver descends into. Reaching this default needs an expression that both evaluates to an array and arrives at originOf as one of those, and the attempt did not find one. \"I COULD NOT REACH IT\" IS NOT \"IT CANNOT BE REACHED\", and it is recorded as the first.",
}

// eraseClausesOutsideTheRegister are the clauses of this gate that the register CANNOT SEE, named
// here rather than left to prose in a commit message, because the blind spot of a check is where the
// next finding on this line has come from four rounds running.
//
// The register's unit is the ARM -- a case clause, or the expression a tag wraps. A clause written
// INSIDE an arm that already has a tag is invisible to it: another name in a case's expression list,
// another || on a condition, another if nested in a tagged body, or a sub-expression of a value the
// arm computes. Each row here is such a clause, with what is known about it.
var eraseClausesOutsideTheRegister = map[string]string{
	"R25 -- the RESULT QUALIFICATION of originOfBody's cycle key": "originOfBody keys its cycle guard `call:NAME:RESULT`, and the register sees only the arm that guard protects -- driven by copy27, a recursive callee. The `:%d` half is a sub-expression of the key and has no arm of its own. NEITHER DRIVEN NOR SHOWN TO BE BEYOND COMPILABLE GO: reaching it needs one resolution chain that asks ONE callee at TWO different result positions, and originOfBody returns at the first parameter- or receiver-rooted answer, so the attempt did not get there. \"I COULD NOT REACH IT\" IS NOT \"IT CANNOT BE REACHED\", and it is recorded as the first. Removing the qualification would key two different questions about one callee to one answer; keeping it costs nothing and is not evidence of anything.",
}

// eraseDeclaredReadings reads every reading name this file DECLARES, out of its own SOURCE TEXT.
//
// THIS IS THE HALF THAT MAKES THE CHECK NON-VACUOUS, and it is the trap the first shape of this idea
// walks into. If "declared" meant "wrote its name into the register during this run", then declared
// would be a subset of exercised by construction and the comparison would hold no matter how many
// arms nothing drove. Declared has to be a property of the TEXT. So this parses the gate's own file
// and takes every string literal handed to an eraseReadings tag, whether or not anything ever calls
// the line it sits on.
func eraseDeclaredReadings(t *testing.T) map[string][]string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, eraseGateFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse this gate's own source, %s: %v", eraseGateFile, err)
	}
	declared := map[string][]string{}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || len(call.Args) == 0 {
			return true
		}
		tag, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if tag.Sel.Name != "name" && tag.Sel.Name != "origin" &&
			tag.Sel.Name != "declined" && tag.Sel.Name != "at" {
			return true
		}
		if !strings.HasSuffix(eraseRender(fileSet, tag.X), "readings") {
			return true
		}
		literal, isLiteral := call.Args[0].(*ast.BasicLit)
		if !isLiteral || literal.Kind != token.STRING {
			t.Errorf("%s:%d: a reading is tagged with something that is not a string literal (%s). The DECLARED set is read out of this file's text, so a name this gate computes at run time is a reading the register cannot see is missing",
				eraseGateFile, fileSet.Position(call.Pos()).Line, eraseRender(fileSet, call.Args[0]))
			return true
		}
		name, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Errorf("%s:%d: unquote a reading name: %v", eraseGateFile,
				fileSet.Position(call.Pos()).Line, err)
			return true
		}
		declared[name] = append(declared[name],
			fmt.Sprintf("%s:%d", eraseGateFile, fileSet.Position(call.Pos()).Line))
		return true
	})
	return declared
}

// eraseUntaggedArms answers every CASE CLAUSE of the registered functions that carries no reading
// tag, which is the half that catches an arm written WITHOUT one.
//
// WHY CASE CLAUSES AND NOT EVERY BRANCH. An arm of this resolver is a case of a type switch or of a
// switch on a callee name; a guard is an if. Requiring a tag on every if would put one on
// `if depth > eraseResolverDepth` and on `if body == nil` and would be noise; requiring one on every
// case is exact and has no false positives today. The cost is stated: a NEW CLAUSE ADDED INSIDE AN
// EXISTING CASE -- another name in `case "len", "cap", ...`, another `||` on a condition, another if
// nested in an arm that already has a tag -- is invisible here.
func eraseUntaggedArms(t *testing.T) ([]string, int, []string) {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, eraseGateFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse this gate's own source, %s: %v", eraseGateFile, err)
	}
	tagged := func(body []ast.Stmt) bool {
		found := false
		for _, statement := range body {
			ast.Inspect(statement, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if tag, isSelector := call.Fun.(*ast.SelectorExpr); isSelector &&
					strings.HasSuffix(eraseRender(fileSet, tag.X), "readings") {
					found = true
				}
				return true
			})
		}
		return found
	}
	untagged := []string{}
	seen := []string{}
	arms := 0
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		if !slices.Contains(eraseRegisteredReadingFunctions, function.Name.Name) {
			continue
		}
		seen = append(seen, function.Name.Name)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			clause, isClause := node.(*ast.CaseClause)
			if !isClause || len(clause.Body) == 0 {
				return true
			}
			arms += 1
			if tagged(clause.Body) {
				return true
			}
			untagged = append(untagged, fmt.Sprintf("%s:%d in %s -- %s",
				eraseGateFile, fileSet.Position(clause.Pos()).Line, function.Name.Name,
				eraseRender(fileSet, clause)))
			return true
		})
	}
	slices.Sort(seen)
	return untagged, arms, seen
}

// eraseReadingsOver drives one whole derivation and answers the register it filled, together with
// the fill sites it admitted -- each of which carries the MEMBER reading that decided it.
func eraseReadingsOver(t *testing.T, fileSet *token.FileSet,
	sources []eraseSource) (*eraseReadings, []eraseFillSite) {

	t.Helper()
	helpers := eraseHelpersIn(sources)
	if len(helpers) == 0 {
		t.Fatal("no erase helper was derived, so nothing below read anything")
	}
	erased, _ := eraseFieldsIn(fileSet, sources, helpers)
	if len(erased) == 0 {
		t.Fatal("no erased field was derived, so nothing below read anything")
	}
	sites, _, _, readings := eraseFillSitesIn(fileSet, sources, erased)
	return readings, sites
}

// TestEveryReadingOfThisGateIsDrivenBySomething is the STANDING check, and it exists because twice
// on this line a clause was added to this gate, driven by NOTHING, and shipped green.
//
// BOTH TIMES THE CHECK THAT FOUND IT WAS A HAND-RUN SWEEP -- delete each clause, run the suite
// unfiltered, see whether anything notices -- and a sweep is a measurement, not a gate. The round
// that ran it over seventy-one clauses and found thirty-three undriven closed its own report with
// the observation that nothing in the suite would tell the NEXT round the same thing. This is that
// something.
//
// THE SHAPE, AND WHY IT IS THIS ONE. Two were on the table: the readings register themselves and the
// test asserts the registered set equals what the corpora exercise, or the control corpus becomes
// the authority and a reading with no named row fails. Taken alone each has the same hole, and it is
// the hole the last round diagnosed in complement 4: a reading nothing takes and nothing names is
// absent from BOTH sides of the comparison, so the comparison holds. So this is both, with the
// DECLARED side read out of this file's own SOURCE TEXT rather than out of a run:
//
//	DECLARED    every eraseReadings tag written anywhere in erased_field_alias_test.go
//	EXERCISED   every reading the two control corpora actually took
//	UNREACHED   the readings named in eraseUnreachedReadings, with the reason for each
//
// and the assertion is an EXACT SET in both directions -- DECLARED minus UNREACHED equals EXERCISED,
// member by member, printed as a diff at the reading's own name. Never that either side is
// non-empty: an existence assertion is satisfied by twelve readings when there are thirteen, which
// is the precise mechanism that let this gate ship twelve-of-thirteen in silence.
//
// AND THE REAL TREE IS HELD TO THE CONTROL, which is the other carry. Complement 4 over the real
// tree is asserted only to be non-empty, and the exact reading-by-reading assertion lives in the
// control corpus -- so a reading exercised ONLY by real source and never modelled in the control was
// a gap nothing closed. It is closed here: the readings the tree takes must be a SUBSET of the
// readings the control takes, so a reading that only real source reaches is red at its own name
// until somebody writes the spelling for it.
//
// WHAT THIS CHECK CANNOT SEE, stated because every round on this line found its successor in exactly
// that blind spot:
//
//  1. A CLAUSE ADDED INSIDE AN ARM THAT ALREADY HAS A TAG. The register's unit is the arm. Another
//     name in `case "len", "cap", ...`, another `||` on a condition, another if nested inside a
//     tagged case -- none of those is a new reading as far as this check is concerned. R25, the
//     result qualification of originOfBody's cycle key, is exactly such a sub-expression, and it is
//     filed in GATES.md rather than registered here.
//  2. THE FUNCTIONS OUTSIDE eraseRegisteredReadingFunctions, listed there with their names.
//  3. WHETHER A READING WAS TAKEN ON PURPOSE. The register counts that a reading ANSWERED, not that
//     answering it decided anything. A peel reached incidentally on the way to another answer counts
//     as driven, so "driven" here is weaker than "a corpus row asserts what it answers" -- which is
//     what the two tables in the corpora tests are for, and they are the stronger half.
//  4. A READING WHOSE NAME IS COMPUTED. The declared set is read out of the text, so a tag whose
//     argument is not a string literal is refused rather than read -- but that is a refusal, not a
//     reading of it.
//  5. AN ARM DELETED OUTRIGHT. Deleting an arm takes its tag with it, so both sides of the
//     comparison shrink together and this check stays green. That direction is the CORPORA's job
//     and not this one's: deleting the slice-expression peel leaves alias3 and alias4 answering
//     something else, which is a failing row with a name on it. The division is worth stating --
//     THIS CHECK CATCHES AN ARM ADDED WITHOUT A DRIVER, THE CORPUS TABLES CATCH AN ARM REMOVED
//     FROM UNDER ONE -- and what neither catches is an arm removed together with the only row that
//     drove it, in one edit.
func TestEveryReadingOfThisGateIsDrivenBySomething(t *testing.T) {
	declared := eraseDeclaredReadings(t)
	if len(declared) == 0 {
		t.Fatal("this gate's own source declares NO reading at all, so the register read nothing and every assertion below would report clean having compared two empty sets")
	}

	// ---- one name, one arm ----
	for name, places := range declared {
		if len(places) < 2 {
			continue
		}
		t.Errorf("the reading %q is declared at %d places (%v). Two arms answering to one name is one arm's worth of evidence covering both: either could stop being taken with the register still seeing the name",
			name, len(places), places)
	}

	// ---- every arm carries a tag ----
	untagged, arms, functions := eraseUntaggedArms(t)
	if arms == 0 {
		t.Fatal("no case clause was found in any registered function, so the totality half of this check compared nothing -- which is what a rename of one of those functions looks like")
	}
	if len(functions) != len(eraseRegisteredReadingFunctions) {
		t.Errorf("the register names %d function(s) and this file declares %d of them (%v). A registered function that no longer exists under that name is a whole family of arms this check silently stopped reading",
			len(eraseRegisteredReadingFunctions), len(functions), functions)
	}
	for _, arm := range untagged {
		t.Errorf("%s\n\tthis arm carries NO reading tag, so nothing can say whether anything drives it. Every case clause of a registered function answers under a name: wrap what it returns in self.readings.origin(%q, ...) or hand its reason through resolver.readings.name(...)",
			arm, "what this arm reads")
	}
	t.Logf("totality -- %d case clause(s) across %d registered function(s), each carrying a reading tag",
		arms, len(functions))

	// ---- what the two control corpora actually take ----
	exercised := map[string]int{}
	for _, corpus := range []struct {
		path   string
		source string
	}{
		{"erased_field_alias_control.go", eraseSpellingCorpus},
		{"erased_field_form_control.go", eraseFormCorpus},
	} {
		fileSet, sources := eraseControlCorpus(t, corpus.path, corpus.source)
		taken, _ := eraseReadingsOver(t, fileSet, sources)
		for name, times := range taken.taken {
			exercised[name] += times
		}
	}

	// ---- DECLARED minus UNREACHED == EXERCISED, member by member ----
	undriven := []string{}
	for name, places := range declared {
		reason, isUnreached := eraseUnreachedReadings[name]
		if isUnreached {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("the reading %q is recorded as unreached with an EMPTY reason, which is an exemption wearing a justification", name)
			}
			if exercised[name] > 0 {
				t.Errorf("the reading %q is recorded as UNREACHED -- %q -- and the control corpora took it %d time(s). Either the reason is wrong or the corpus reached what it says nothing can",
					name, reason, exercised[name])
			}
			continue
		}
		if exercised[name] > 0 {
			continue
		}
		undriven = append(undriven, fmt.Sprintf("%s\n\t\tdeclared at %v", name, places))
	}
	slices.Sort(undriven)
	for _, name := range undriven {
		t.Errorf("this gate declares the reading %s\n\tand NOTHING in either control corpus takes it. A clause nothing drives is a comment however correct it is. Either write the spelling that drives it into eraseSpellingCorpus or eraseFormCorpus, or record it in eraseUnreachedReadings with the reason no compilable Go reaches it",
			name)
	}
	for name, times := range exercised {
		if _, isDeclared := declared[name]; isDeclared {
			continue
		}
		t.Errorf("the control corpora took a reading named %q %d time(s) and this file's SOURCE declares no such tag. The declared set is read out of the text on purpose; a name that appears only at run time is a name the register cannot notice the absence of",
			name, times)
	}
	for name := range eraseUnreachedReadings {
		if _, isDeclared := declared[name]; isDeclared {
			continue
		}
		t.Errorf("eraseUnreachedReadings names %q and this gate declares no reading by that name. A stale row is an exemption waiting for an arm it was never written about",
			name)
	}

	// ---- and the REAL TREE is held to the control ----
	fileSet, sources := eraseSources(t)
	overTree, treeSites := eraseReadingsOver(t, fileSet, sources)
	onlyTree := []string{}
	for name := range overTree.taken {
		if exercised[name] > 0 {
			continue
		}
		onlyTree = append(onlyTree, name)
	}
	slices.Sort(onlyTree)
	for _, name := range onlyTree {
		t.Errorf("the reading %q is taken over this package's production source and by NEITHER control corpus. That is the gap complement 4 still has over the real tree: the exact reading-by-reading assertion lives in the control, so a reading only real source reaches is asserted by nothing. Write the spelling for it",
			name)
	}

	// ---- THE COMPLEMENT OF THIS NARROWING, PRINTED ----
	//
	// "every declared reading is driven" removes nothing by itself; what the check NARROWS is the
	// set of readings the real tree exercises, down from the set the corpora do. That difference is
	// the complement, and it is the interesting direction: those readings exist for source this
	// package does not contain today and would otherwise be the first to rot.
	control := slices.Sorted(maps_Keys(exercised))
	notInTree := []string{}
	for _, name := range control {
		if overTree.taken[name] > 0 {
			continue
		}
		notInTree = append(notInTree, name)
	}
	t.Logf("the register -- %d reading(s) declared in this file's source, %d driven by the control corpora, %d recorded unreached with a reason, %d taken over this package's own production source",
		len(declared), len(exercised), len(eraseUnreachedReadings), len(overTree.taken))

	// AND WHICH MEMBER READING ADMITTED WHICH SITE, because "the reading answered" and "the reading
	// decided a member of this class" are different facts and only the second is what the gate is
	// for. A form walk that stopped deciding one of the four member positions would still answer
	// everywhere else; this line is where that shows.
	admitted := map[string]int{}
	for _, site := range treeSites {
		admitted[site.reading] += 1
	}
	byReading := []string{}
	for _, reading := range slices.Sorted(maps_Keys(admitted)) {
		byReading = append(byReading, fmt.Sprintf("%4d  %s", admitted[reading], reading))
	}
	t.Logf("the %d fill site(s) of this package, under the MEMBER reading that admitted each:\n\t%s",
		len(treeSites), strings.Join(byReading, "\n\t"))
	t.Logf("complement 5 -- the %d reading(s) the corpora drive that this package's own source does NOT reach today, which are the ones a corpus is for:\n\t%s",
		len(notInTree), strings.Join(notInTree, "\n\t"))
	if len(notInTree) == 0 {
		t.Error("complement 5 is EMPTY: every reading of this gate is reached by this package's own production source, so the control corpora narrow nothing and the day a reading stops being reachable from real source nothing would be modelling it")
	}
	// AND EACH UNREACHED ROW SAYS WHICH CLAIM IT IS MAKING. "No compilable Go reaches this" and "I
	// could not find compilable Go that reaches this" are different statements, and a register that
	// let them share a spelling would quietly promote the second into the first -- which is the
	// exact correction the round before this one had to make about R25.
	unreachable, unreached := 0, 0
	for name, reason := range eraseUnreachedReadings {
		switch {
		case strings.HasPrefix(reason, "UNREACHABLE: "):
			unreachable += 1
		case strings.HasPrefix(reason, "UNREACHED: "):
			unreached += 1
		default:
			t.Errorf("the unreached row for %q begins with neither UNREACHABLE: nor UNREACHED:. The first says no compilable Go reaches this arm and the second says none was FOUND; a row that does not say which is a claim nobody can check",
				name)
		}
		t.Logf("unreached -- %s\n\t%s", name, reason)
	}
	t.Logf("the unreached register -- %d arm(s) argued UNREACHABLE from Go's own grammar or from this gate's own structure, %d recorded UNREACHED because no driver was found, which is the weaker claim",
		unreachable, unreached)

	// AND THE CLAUSES THIS REGISTER CANNOT SEE, printed on every run so that the blind spot is in
	// the log next to the clean verdict rather than only in a document.
	if len(eraseClausesOutsideTheRegister) == 0 {
		t.Error("eraseClausesOutsideTheRegister is EMPTY, which claims this register sees every clause of this gate. Its unit is the arm; a clause inside an arm that already carries a tag is invisible to it, and at least R25 is one")
	}
	for name, what := range eraseClausesOutsideTheRegister {
		if strings.TrimSpace(what) == "" {
			t.Errorf("the clause %q is recorded as outside the register with an EMPTY note", name)
		}
		t.Logf("outside the register -- %s\n\t%s", name, what)
	}
}

// ---------------------------------------------------------------------------
// step 1 -- the erase helpers, by body shape
// ---------------------------------------------------------------------------

// eraseHelpersIn answers every function of these sources whose body writes a zero through every
// index of one of its byte-slice parameters.
//
// THE NAME IS NOT READ. `zeroizeSecret` does not appear in this function, and the control corpus
// above spells its erase `wipe` for exactly that reason: a derivation that matched the name would
// be the same defect this file exists to prevent, one level up.
func eraseHelpersIn(sources []eraseSource) []string {
	found := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Recv != nil {
				continue
			}
			parameters := eraseParameterNames(eraseScopeOf(function))
			erases := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				loop, isRange := node.(*ast.RangeStmt)
				if !isRange || loop.Key == nil || loop.Body == nil {
					return true
				}
				over, isIdentifier := loop.X.(*ast.Ident)
				if !isIdentifier || !parameters[over.Name] {
					return true
				}
				index, isIndexIdentifier := loop.Key.(*ast.Ident)
				if !isIndexIdentifier {
					return true
				}
				for _, statement := range loop.Body.List {
					assign, isAssign := statement.(*ast.AssignStmt)
					if !isAssign || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
						continue
					}
					target, isIndexed := assign.Lhs[0].(*ast.IndexExpr)
					if !isIndexed {
						continue
					}
					into, isInto := target.X.(*ast.Ident)
					at, isAt := target.Index.(*ast.Ident)
					if !isInto || !isAt || into.Name != over.Name || at.Name != index.Name {
						continue
					}
					if literal, isLiteral := assign.Rhs[0].(*ast.BasicLit); isLiteral && literal.Value == "0" {
						erases = true
					}
				}
				return true
			})
			if erases {
				found = append(found, function.Name.Name)
			}
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// ---------------------------------------------------------------------------
// step 2 -- the fields those helpers erase
// ---------------------------------------------------------------------------

// eraseFieldsIn answers every field name an erase helper is called on through a receiver, and the
// receiver type names that own one.
func eraseFieldsIn(fileSet *token.FileSet, sources []eraseSource,
	helpers []string) (map[string]bool, map[string]bool) {

	isHelper := map[string]bool{}
	for _, name := range helpers {
		isHelper[name] = true
	}
	fields := map[string]bool{}
	owners := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Recv == nil ||
				len(function.Recv.List) != 1 {
				continue
			}
			receiver := ""
			if len(function.Recv.List[0].Names) == 1 {
				receiver = function.Recv.List[0].Names[0].Name
			}
			if receiver == "" {
				continue
			}
			owner := eraseTypeNameOf(function.Recv.List[0].Type)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall || len(call.Args) != 1 {
					return true
				}
				callee, isIdentifier := call.Fun.(*ast.Ident)
				if !isIdentifier || !isHelper[callee.Name] {
					return true
				}
				selector, isSelector := eraseUnparen(call.Args[0]).(*ast.SelectorExpr)
				if !isSelector {
					return true
				}
				base, isBase := eraseUnparen(selector.X).(*ast.Ident)
				if !isBase || base.Name != receiver {
					return true
				}
				fields[selector.Sel.Name] = true
				if owner != "" {
					owners[owner] = true
				}
				return true
			})
		}
	}
	return fields, owners
}

// eraseSiblingFields is complement 1: every field of every struct type that owns an erased field,
// minus the erased ones.
func eraseSiblingFields(sources []eraseSource, erased map[string]bool) []string {
	found := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType {
					continue
				}
				structure, isStruct := typeSpec.Type.(*ast.StructType)
				if !isStruct || structure.Fields == nil {
					continue
				}
				owns := false
				for _, field := range structure.Fields.List {
					for _, name := range field.Names {
						if erased[name.Name] {
							owns = true
						}
					}
				}
				if !owns {
					continue
				}
				for _, field := range structure.Fields.List {
					for _, name := range field.Names {
						if !erased[name.Name] {
							found = append(found, typeSpec.Name.Name+"."+name.Name)
						}
					}
				}
			}
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// ---------------------------------------------------------------------------
// step 3 -- the fill sites and the origin of what they write
// ---------------------------------------------------------------------------

// eraseFormRefusal is a BINDING POSITION this walk found and has no reading for.
//
// It is an error and never a skip, which is the whole of what was wrong with the first version:
// the two forms it could not read produced no site, no complement entry and no undecided count, so
// a tree containing one looked exactly like a tree containing none.
type eraseFormRefusal struct {
	at   string
	form string
	src  string
}

// eraseFillSitesIn answers every place this package's production source BINDS a field an erase
// reaches, where the backing array of what it binds came from, the binding positions it decided
// were NOT members, and the forms it REFUSES.
//
// THE FORM CLASS IS THE LITERAL OF THIS STEP, and the first version of this gate got it wrong in
// the shape GATES.md records nine times. It read *ast.KeyValueExpr elements of a composite literal
// and assignments whose two sides have equal length, and nothing else -- so &PathSecret{p} was
// invisible, and so was x.Field, err = f(), of which this package's production source holds
// twenty-two instances today because it is how every decode binds the octets it just read.
//
// THE BOUNDARY IS STATED AND THE WALK FAILS CLOSED, which is the remedy gatesDeriveDoors took for
// the same defect one gate over. Go REBINDS a struct field in exactly four syntactic positions:
//
//	*ast.CompositeLit  an element, keyed or POSITIONAL, at any depth of elision
//	*ast.AssignStmt    the one-to-one form, the MULTI-VALUE form, and the operator form
//	*ast.RangeStmt     for k, x.Field = range v, when the range binds with = rather than :=
//	*ast.IncDecStmt    x.Field++
//
// and each of the four is decided here in every one of its sub-forms, with a refusal where a
// sub-form has no reading. WHAT IS OUTSIDE THE BOUNDARY IS STATED RATHER THAN IMPLIED, because a
// boundary nobody wrote down is the same thing as no boundary:
//
//   - a write THROUGH the field's existing array, copy(x.Field, p), and a write through a pointer
//     taken at &x.Field. Neither rebinds the field; this gate does not see them and does not claim
//     to. Twenty-seven &field expressions and one copy-into are in this package's source today.
//   - a binding at PACKAGE LEVEL, outside any function body. The walk below is over function
//     bodies, and that is sound rather than lucky: this class is "filled from an array the CALLER
//     owns", a package-level initialiser has no caller and no parameters, so no member of the class
//     can live in one.
//
// AND THE NARROWING PRINTS WHAT IT REMOVED. Every binding position decided NOT to be a member is
// counted under the reading that decided it, and the gate errors if that complement is empty --
// because a form nothing counts is a form nothing can miss.
func eraseFillSitesIn(fileSet *token.FileSet, sources []eraseSource,
	erased map[string]bool) ([]eraseFillSite, []eraseFormRefusal, map[string]int, *eraseReadings) {

	resolver := newEraseResolver(sources)
	sites := []eraseFillSite{}
	refusals := []eraseFormRefusal{}
	removed := map[string]int{}
	for _, source := range sources {
		shapes := eraseCompositeShapes(source.parsed, resolver)
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			// THE SCOPE, and a site's scope is part of what names it. `ast.Inspect` over a
			// declaration's body walks straight into every FUNCTION LITERAL written in it, and
			// the first version of this walk resolved a site found in there against the
			// DECLARATION's parameters and the DECLARATION's locals. So an alias handed to a
			// CLOSURE PARAMETER whose name also exists as a fresh local one level out was
			// resolved to the local and ADMITTED -- this gate's own defect, spelled in its own
			// resolver, and the exact shape it exists to refuse. A literal is a scope: it is
			// walked with a scope of its own chained to the one enclosing it, and a name is bound
			// by the INNERMOST scope of that chain which declares it.
			declaration := eraseScopeOf(function)
			where := func(node ast.Node) string {
				return fmt.Sprintf("%s:%d", source.path, fileSet.Position(node.Pos()).Line)
			}
			// result is the POSITION in the value the binding takes its array from: 0 for every
			// form but the multi-value assignment, where x.Field is the nth thing one call
			// answers and the nth result is the only one whose origin is the field's.
			// reading is the FORM-WALK reading that decided this binding position was a
			// member. It is passed in rather than inferred so that the register records it by
			// being called: a member reading that stops being taken is a diff at its own name.
			record := func(reading string, scope *eraseScope, field string, value ast.Expr, result int) {
				sites = append(sites, eraseFillSite{
					at:          where(value),
					inside:      scope.name,
					declaration: declaration.name,
					field:       field,
					rhs:         eraseRender(fileSet, value),
					origin:      resolver.originOfResult(value, result, scope, 0, map[string]bool{}),
					reading:     reading,
				})
			}
			refuse := func(node ast.Node, form string) {
				refusals = append(refusals, eraseFormRefusal{
					at: where(node), form: form, src: eraseRender(fileSet, node),
				})
			}
			// fieldOf answers the field name an assignment target binds, and whether an erase of
			// this package reaches it.
			fieldOf := func(target ast.Expr) (string, bool) {
				selector, isSelector := eraseUnparen(target).(*ast.SelectorExpr)
				if !isSelector {
					return "", false
				}
				return selector.Sel.Name, erased[selector.Sel.Name]
			}
			var walk func(scope *eraseScope, body ast.Node)
			walk = func(scope *eraseScope, body ast.Node) {
				ast.Inspect(body, func(node ast.Node) bool {
					if literal, isLiteral := node.(*ast.FuncLit); isLiteral {
						// A LITERAL IS ITS OWN SCOPE and is walked as one, so nothing written
						// inside it is ever resolved against the enclosing frame. Returning
						// false is what stops this walk descending into it a second time.
						walk(scope.inner(literal), literal.Body)
						return false
					}
					switch shaped := node.(type) {
					case *ast.CompositeLit:
						keyed := false
						for _, element := range shaped.Elts {
							if _, isPair := element.(*ast.KeyValueExpr); isPair {
								keyed = true
							}
						}
						if keyed {
							for _, element := range shaped.Elts {
								pair, isPair := element.(*ast.KeyValueExpr)
								if !isPair {
									// NOT "which Go does not permit", which is what this said and is
									// false: Go permits mixed keyed and unkeyed elements in a SLICE,
									// an ARRAY and a MAP literal and forbids it only for a struct. So
									// this is a REACHABLE form and not a dead arm, and what it is
									// refused for is that nothing in the outer literal says which
									// position the element takes.
									refuse(element, resolver.readings.name("an element with no key in a composite literal whose other elements have one, which this gate cannot place"))
									continue
								}
								key, isKey := pair.Key.(*ast.Ident)
								if !isKey {
									removed[resolver.readings.name("a keyed element whose key is not an identifier, so it keys a map or an array and binds no field")] += 1
									continue
								}
								if !erased[key.Name] {
									removed[resolver.readings.name("a keyed element binding a field no erase of this package reaches")] += 1
									continue
								}
								record(resolver.readings.name("a KEYED element binding a field an erase reaches"),
									scope, key.Name, pair.Value, 0)
							}
							return true
						}
						if len(shaped.Elts) == 0 {
							removed[resolver.readings.name("a composite literal with no elements, which binds nothing")] += 1
							return true
						}
						// POSITIONAL. Nothing in the literal names the fields, so they are read
						// off the TYPE -- which is why this arm needs a type reading and the
						// keyed arm does not, and why a gate with no type reading could not see
						// the form.
						shape, isNamed := shapes[shaped]
						if !isNamed {
							refuse(shaped, resolver.readings.name("a POSITIONAL composite literal whose type this gate could not name, so it cannot say which field each element binds"))
							return true
						}
						if !shape.isStruct {
							removed[resolver.readings.name("a positional literal of a slice, array or map type, whose elements bind no field")] += 1
							return true
						}
						for index, element := range shaped.Elts {
							if index >= len(shape.fields) {
								refuse(element, resolver.readings.name("a positional element past the end of the field list this gate read for its type"))
								continue
							}
							if !erased[shape.fields[index]] {
								removed[resolver.readings.name("a positional element binding a field no erase of this package reaches")] += 1
								continue
							}
							record(resolver.readings.name("a POSITIONAL element binding a field an erase reaches"),
								scope, shape.fields[index], element, 0)
						}
					case *ast.AssignStmt:
						if shaped.Tok != token.ASSIGN && shaped.Tok != token.DEFINE {
							// AN OPERATOR ASSIGNMENT. No operator Go has rebinds a slice, so this
							// cannot be a member -- but it IS a binding position, so it is decided
							// here rather than walked past, and an erased field on its left is
							// refused rather than assumed harmless.
							for _, target := range shaped.Lhs {
								if field, isErased := fieldOf(target); isErased {
									refuse(shaped, fmt.Sprintf(resolver.readings.name("an operator assignment onto %s, a field an erase reaches"), field))
								}
							}
							removed[resolver.readings.name("an operator assignment, which writes through a field's array and cannot rebind it")] += 1
							return true
						}
						if len(shaped.Lhs) == len(shaped.Rhs) {
							for index, target := range shaped.Lhs {
								field, isErased := fieldOf(target)
								if field == "" {
									removed[resolver.readings.name("a one-to-one assignment onto something that is not a field")] += 1
									continue
								}
								if !isErased {
									removed[resolver.readings.name("a one-to-one assignment onto a field no erase of this package reaches")] += 1
									continue
								}
								record(resolver.readings.name("a ONE-TO-ONE assignment onto a field an erase reaches"),
									scope, field, shaped.Rhs[index], 0)
							}
							return true
						}
						if len(shaped.Rhs) == 1 {
							// THE MULTI-VALUE FORM. The first version returned at this shape, so
							// every one of these was invisible. The array reaching a field comes
							// out of RESULT POSITION index of the one expression on the right,
							// and that is what its origin is resolved through -- not the call as
							// a whole, which would read result 0 for a field bound at result 1.
							for index, target := range shaped.Lhs {
								field, isErased := fieldOf(target)
								if field == "" {
									removed[resolver.readings.name("a multi-value assignment onto something that is not a field")] += 1
									continue
								}
								if !isErased {
									removed[resolver.readings.name("a multi-value assignment onto a field no erase of this package reaches")] += 1
									continue
								}
								record(resolver.readings.name("a MULTI-VALUE assignment onto a field an erase reaches, at its own result position"),
									scope, field, shaped.Rhs[0], index)
							}
							return true
						}
						refuse(shaped, resolver.readings.name("an assignment whose two sides have different lengths and whose right side is not a single expression"))
					case *ast.RangeStmt:
						if shaped.Tok == token.DEFINE || shaped.Tok == token.ILLEGAL {
							removed[resolver.readings.name("a range that declares its own variables or binds none, which binds no field")] += 1
							return true
						}
						bound := false
						for _, target := range []ast.Expr{shaped.Key, shaped.Value} {
							if target == nil {
								continue
							}
							if field, isErased := fieldOf(target); isErased {
								bound = true
								refuse(shaped, fmt.Sprintf(resolver.readings.name("a range binding %s, a field an erase reaches, once per iteration"), field))
							}
						}
						if !bound {
							removed[resolver.readings.name("a range binding with =, onto targets no erase of this package reaches")] += 1
						}
					case *ast.IncDecStmt:
						if field, isErased := fieldOf(shaped.X); isErased {
							refuse(shaped, fmt.Sprintf(resolver.readings.name("an increment or decrement of %s, a field an erase reaches"), field))
							return true
						}
						removed[resolver.readings.name("an increment or decrement, which cannot rebind a slice field")] += 1
					}
					return true
				})
			}
			walk(declaration, function.Body)
		}
	}
	slices.SortFunc(sites, func(a, b eraseFillSite) int { return strings.Compare(a.at, b.at) })
	slices.SortFunc(refusals, func(a, b eraseFormRefusal) int { return strings.Compare(a.at, b.at) })
	return sites, refusals, removed, resolver.readings
}

// eraseShape is what one type NAME is, as far as a composite literal is concerned: whether it is a
// struct, and if it is, the fields it declares IN ORDER together with their written types.
//
// The ORDER is the point. A keyed element says which field it binds; a POSITIONAL one does not, and
// nothing but the declaration order can say. The first version of this gate had no type reading at
// all, which is why it could not see a positional element and had no way to know that it could not.
type eraseShape struct {
	isStruct bool
	fields   []string
	byName   map[string]ast.Expr
	// the type names of the EMBEDDED fields, which is what a promoted method is looked up
	// through. An embedded field is in fields and byName too -- Go names it by its type -- but
	// promotion needs to know which of them are embeddings and which are ordinary fields that
	// happen to be named after a type.
	embedded []string
}

// eraseResolver answers where the backing array an expression evaluates to came from.
type eraseResolver struct {
	// every type name this package declares, so a one-argument call can be told from a
	// conversion without a type checker.
	types map[string]bool
	// every package-level function this package declares, so the resolver can take one hop into
	// a callee's return.
	functions map[string]*ast.FuncDecl
	// what each declared type name is UNDERNEATH, so a composite literal's type can be told apart
	// as a struct, a slice, a map or an interface -- and so the element type of a named slice can
	// be reached, which is what gives an ELIDED literal a type at all.
	underlying map[string]ast.Expr
	// the shape of every struct this package declares, by name.
	structs map[string]*eraseShape
	// every METHOD this package declares, by the type it is on and then by its own name. A call
	// spelled x.M() whose receiver is a concrete type of this package is a callee whose body is
	// already parsed here, so admitting it unopened was a false statement about this gate's reach.
	methods map[string]map[string]*ast.FuncDecl
	// the register every reading below writes its own name into as it answers.
	readings *eraseReadings
}

const eraseResolverDepth = 8

func newEraseResolver(sources []eraseSource) *eraseResolver {
	self := &eraseResolver{
		types:      map[string]bool{},
		functions:  map[string]*ast.FuncDecl{},
		underlying: map[string]ast.Expr{},
		structs:    map[string]*eraseShape{},
		methods:    map[string]map[string]*ast.FuncDecl{},
		readings:   newEraseReadings(),
	}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			switch shaped := declaration.(type) {
			case *ast.GenDecl:
				if shaped.Tok != token.TYPE {
					continue
				}
				for _, spec := range shaped.Specs {
					typeSpec, isType := spec.(*ast.TypeSpec)
					if !isType {
						continue
					}
					self.types[typeSpec.Name.Name] = true
					self.underlying[typeSpec.Name.Name] = typeSpec.Type
					if structure, isStructure := typeSpec.Type.(*ast.StructType); isStructure {
						self.structs[typeSpec.Name.Name] = eraseShapeOf(structure)
					}
				}
			case *ast.FuncDecl:
				if shaped.Recv == nil {
					self.functions[shaped.Name.Name] = shaped
					continue
				}
				if len(shaped.Recv.List) != 1 {
					continue
				}
				owner := eraseTypeNameOf(shaped.Recv.List[0].Type)
				if owner == "" {
					continue
				}
				if self.methods[owner] == nil {
					self.methods[owner] = map[string]*ast.FuncDecl{}
				}
				self.methods[owner][shaped.Name.Name] = shaped
			}
		}
	}
	return self
}

// eraseShapeOf reads one struct declaration into the ordered field list a positional element is
// resolved through. An EMBEDDED field is named by its own type, which is how Go names it.
func eraseShapeOf(structure *ast.StructType) *eraseShape {
	shape := &eraseShape{isStruct: true, byName: map[string]ast.Expr{}}
	if structure.Fields == nil {
		return shape
	}
	for _, field := range structure.Fields.List {
		if len(field.Names) == 0 {
			name := eraseTypeNameOf(field.Type)
			shape.fields = append(shape.fields, name)
			if name != "" {
				shape.byName[name] = field.Type
				shape.embedded = append(shape.embedded, name)
			}
			continue
		}
		for _, declared := range field.Names {
			shape.fields = append(shape.fields, declared.Name)
			shape.byName[declared.Name] = field.Type
		}
	}
	return shape
}

// shapeOf answers what a written type IS, following the names this package declares. nil means this
// reading cannot say, and nil keeps a positional literal REFUSED rather than guessed at.
func (self *eraseResolver) shapeOf(written ast.Expr, depth int) *eraseShape {
	if written == nil || depth > eraseResolverDepth {
		return nil
	}
	switch shaped := written.(type) {
	case *ast.ParenExpr:
		return self.shapeOf(shaped.X, depth+1)
	case *ast.StarExpr:
		return self.shapeOf(shaped.X, depth+1)
	case *ast.StructType:
		return eraseShapeOf(shaped)
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.InterfaceType, *ast.FuncType:
		return &eraseShape{}
	case *ast.IndexExpr:
		return self.shapeOf(shaped.X, depth+1)
	case *ast.IndexListExpr:
		return self.shapeOf(shaped.X, depth+1)
	case *ast.Ident:
		if shape, isStruct := self.structs[shaped.Name]; isStruct {
			return shape
		}
		if under, isDeclared := self.underlying[shaped.Name]; isDeclared {
			return self.shapeOf(under, depth+1)
		}
	}
	return nil
}

// elementTypesOf answers the element and key types of a written container type, which is what gives
// an ELIDED composite literal -- {left, right} inside a [2][2]NodeIndex -- a type at all.
func (self *eraseResolver) elementTypesOf(written ast.Expr, depth int) (ast.Expr, ast.Expr) {
	if written == nil || depth > eraseResolverDepth {
		return nil, nil
	}
	switch shaped := written.(type) {
	case *ast.ParenExpr:
		return self.elementTypesOf(shaped.X, depth+1)
	case *ast.StarExpr:
		return self.elementTypesOf(shaped.X, depth+1)
	case *ast.ArrayType:
		return shaped.Elt, nil
	case *ast.MapType:
		return shaped.Value, shaped.Key
	case *ast.IndexExpr:
		return self.elementTypesOf(shaped.X, depth+1)
	case *ast.Ident:
		if under, isDeclared := self.underlying[shaped.Name]; isDeclared {
			return self.elementTypesOf(under, depth+1)
		}
	}
	return nil, nil
}

// eraseCompositeShapes answers the shape of EVERY composite literal in one file, including the ones
// whose type is written only on a literal that contains them.
//
// A literal missing from this map is one this reading could not name, and the walk refuses it rather
// than skipping it -- but only when it is POSITIONAL, because a keyed element names its own field
// and needs no type at all.
func eraseCompositeShapes(file *ast.File, resolver *eraseResolver) map[*ast.CompositeLit]*eraseShape {
	shapes := map[*ast.CompositeLit]*eraseShape{}
	var walk func(literal *ast.CompositeLit, expected ast.Expr)
	walk = func(literal *ast.CompositeLit, expected ast.Expr) {
		written := literal.Type
		if written == nil {
			written = expected
		}
		shape := resolver.shapeOf(written, 0)
		if shape != nil {
			shapes[literal] = shape
		}
		// THE EXPECTATION IS ONLY EVER THE ELEMENT OR THE KEY TYPE OF A CONTAINER, and the two
		// readings that used to hand a STRUCT FIELD's declared type down to a nested literal are
		// gone. They were unreachable: Go permits a composite literal to elide its type only
		// "within a composite literal of array, slice, or map type", so `C{H: {x}}` and `C{{x}}`
		// are not Go, go/types refuses both, and deleting the two readings left an unfiltered run
		// over all three trees reading exactly what it read before. An arm kept because it is
		// theoretically correct is a comment with a cost.
		element, key := resolver.elementTypesOf(written, 0)
		for _, entry := range literal.Elts {
			value := entry
			expectation := element
			if pair, isPair := entry.(*ast.KeyValueExpr); isPair {
				value = pair.Value
				if inner, isInner := pair.Key.(*ast.CompositeLit); isInner {
					walk(inner, key)
				}
			}
			if inner, isInner := value.(*ast.CompositeLit); isInner {
				walk(inner, expectation)
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, isLiteral := node.(*ast.CompositeLit)
		if !isLiteral || literal.Type == nil {
			return true
		}
		if _, alreadyWalked := shapes[literal]; alreadyWalked {
			return true
		}
		walk(literal, nil)
		return true
	})
	return shapes
}

// ---------------------------------------------------------------------------
// the scope a name is resolved in
// ---------------------------------------------------------------------------

// eraseScope is the function body a NAME is resolved in, and the chain it is resolved OUTWARD
// through.
//
// WHY THIS EXISTS, and it is the gate's own defect one level down. Every reading below used to take
// an *ast.FuncDecl, while the walk that feeds it descends into every FUNCTION LITERAL in that
// declaration's body. So a fill site written inside a closure was resolved against the enclosing
// DECLARATION's parameters and the enclosing declaration's locals: an alias handed to a closure
// parameter whose name also existed as a fresh local one frame out resolved to THE LOCAL and was
// admitted. That is a spelling-blind gate reading the wrong frame, which is the same class of
// mistake as reading the wrong spelling.
//
// A name is bound by the INNERMOST scope of the chain that declares it -- as a parameter, as the
// receiver, or as a local something in that scope assigns. That is Go's own rule and it is the
// whole of what eraseBindingOf below does.
type eraseScope struct {
	// the scope enclosing this one, nil at a declaration.
	outer *eraseScope
	// exactly one of these is set: decl at the outermost scope, lit inside a function literal.
	decl *ast.FuncDecl
	lit  *ast.FuncLit
	// how this scope is named in a site, a key and a cycle guard: (Type).Method or Function for a
	// declaration, and that name plus .funcN for the Nth literal written directly in it, which is
	// how Go itself names one.
	name string
	// how many literals of this scope have been named so far.
	literals int
}

func eraseScopeOf(declared *ast.FuncDecl) *eraseScope {
	if declared == nil {
		return nil
	}
	return &eraseScope{decl: declared, name: eraseFunctionKey(declared)}
}

func (self *eraseScope) inner(literal *ast.FuncLit) *eraseScope {
	self.literals += 1
	return &eraseScope{
		outer: self,
		lit:   literal,
		name:  fmt.Sprintf("%s.func%d", self.name, self.literals),
	}
}

func (self *eraseScope) signature() *ast.FuncType {
	if self == nil {
		return nil
	}
	if self.decl != nil {
		return self.decl.Type
	}
	if self.lit != nil {
		return self.lit.Type
	}
	return nil
}

func (self *eraseScope) body() *ast.BlockStmt {
	if self == nil {
		return nil
	}
	if self.decl != nil {
		return self.decl.Body
	}
	if self.lit != nil {
		return self.lit.Body
	}
	return nil
}

// receiver is the receiver field list of the DECLARATION this scope is, and nil for a literal. A
// literal written inside a method still sees the receiver name -- through the chain, at the scope
// that actually declares it.
func (self *eraseScope) receiver() *ast.FieldList {
	if self == nil || self.decl == nil {
		return nil
	}
	return self.decl.Recv
}

// eraseBinding is which scope of a chain binds one name, and how.
type eraseBinding struct {
	scope *eraseScope
	// eraseOriginParameter or eraseOriginReceiver when local is false.
	kind eraseOriginKind
	// a LOCAL of scope, together with every value that scope ever assigns it AND THE RESULT
	// POSITION each of those values was taken from.
	local  bool
	values []eraseAssignment
}

// eraseBindingOf walks the scope chain from the INSIDE OUT and answers the first scope that binds
// this name.
//
// The order is the whole point: a closure parameter shadows a same-named local of the function
// enclosing it, and resolving the outer one instead is how an alias got admitted.
func eraseBindingOf(readings *eraseReadings, inside *eraseScope, name string) (eraseBinding, bool) {
	for scope := inside; scope != nil; scope = scope.outer {
		if eraseParameterNames(scope)[name] {
			return eraseBinding{scope: scope, kind: eraseOriginParameter}, true
		}
		if eraseReceiverName(scope) == name {
			return eraseBinding{scope: scope, kind: eraseOriginReceiver}, true
		}
		if values := eraseAssignmentsTo(readings, scope, name); len(values) > 0 {
			return eraseBinding{scope: scope, local: true, values: values}, true
		}
	}
	return eraseBinding{}, false
}

// eraseInspectScope is ast.Inspect that STOPS at a nested function literal, for the readings that
// are about one scope's own statements rather than about everything written inside it.
func eraseInspectScope(body ast.Node, visit func(ast.Node) bool) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(node ast.Node) bool {
		if _, isLiteral := node.(*ast.FuncLit); isLiteral {
			return false
		}
		return visit(node)
	})
}

// declaredTypeNameOf answers the name of the type an expression HAS, read off the declaration that
// introduced it: a parameter, the receiver, a local with a written type, a local assigned a
// composite literal, a field of a struct this package declares, or the first result of a function
// it declares.
//
// "" means this gate cannot say, and "" keeps a method call OPAQUE. That is exactly the line between
// the hole closed here and the open edge this gate discloses: h.Bytes() where h is declared
// *probeHolder is a body already parsed here, while crypto.DeriveSecret(...) where crypto is
// declared CryptoProvider is an INTERFACE, and picking the one implementation that happens to live
// in this package would be a guess wearing a derivation.
func (self *eraseResolver) declaredTypeNameOf(value ast.Expr, inside *eraseScope, depth int) string {
	if inside == nil || depth > eraseResolverDepth {
		return ""
	}
	switch shaped := eraseUnparen(value).(type) {
	case *ast.StarExpr:
		return self.declaredTypeNameOf(shaped.X, inside, depth+1)
	case *ast.CompositeLit:
		return eraseTypeNameOf(shaped.Type)
	case *ast.UnaryExpr:
		if shaped.Op == token.AND {
			return self.declaredTypeNameOf(shaped.X, inside, depth+1)
		}
	case *ast.Ident:
		// INNERMOST SCOPE FIRST, so a closure parameter is never given the declared type of a
		// same-named local one frame out.
		for scope := inside; scope != nil; scope = scope.outer {
			if written := eraseDeclaredTypeOfName(scope, shaped.Name); written != nil {
				return eraseTypeNameOf(written)
			}
			for _, assigned := range eraseAssignmentsTo(self.readings, scope, shaped.Name) {
				if name := self.declaredTypeNameOf(assigned.value, scope, depth+1); name != "" {
					return name
				}
			}
		}
	case *ast.SelectorExpr:
		owner := self.declaredTypeNameOf(shaped.X, inside, depth+1)
		if shape, isStruct := self.structs[owner]; isStruct {
			if written, isField := shape.byName[shaped.Sel.Name]; isField {
				return eraseTypeNameOf(written)
			}
		}
	case *ast.CallExpr:
		callee, isIdentifier := eraseUnparen(shaped.Fun).(*ast.Ident)
		if !isIdentifier {
			return ""
		}
		declared, isDeclared := self.functions[callee.Name]
		if isDeclared && declared.Type != nil && declared.Type.Results != nil &&
			len(declared.Type.Results.List) >= 1 {
			return eraseTypeNameOf(declared.Type.Results.List[0].Type)
		}
	}
	return ""
}

// eraseDeclaredTypeOfName answers the WRITTEN type of one name in ONE SCOPE: its receiver (a
// declaration only), its parameters, its results, or a var declaration in its own body.
//
// SCOPE-LOCAL ON PURPOSE. `var x T` written inside a function literal is not the enclosing
// function's x, so this reading stops at a nested literal and declaredTypeNameOf's chain walk moves
// outward instead of this reading reaching inward.
func eraseDeclaredTypeOfName(scope *eraseScope, name string) ast.Expr {
	if scope == nil {
		return nil
	}
	lists := []*ast.FieldList{scope.receiver()}
	if signature := scope.signature(); signature != nil {
		lists = append(lists, signature.Params, signature.Results)
	}
	for _, list := range lists {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, declared := range field.Names {
				if declared.Name == name {
					return field.Type
				}
			}
		}
	}
	var found ast.Expr
	eraseInspectScope(scope.body(), func(node ast.Node) bool {
		spec, isSpec := node.(*ast.ValueSpec)
		if !isSpec || spec.Type == nil {
			return true
		}
		for _, declared := range spec.Names {
			if declared.Name == name {
				found = spec.Type
			}
		}
		return true
	})
	return found
}

// originOf is the whole decision. It peels every form that PRESERVES a backing array and stops at
// the first thing that makes a new one.
func (self *eraseResolver) originOf(value ast.Expr, inside *eraseScope, depth int,
	seen map[string]bool) eraseOrigin {

	if depth > eraseResolverDepth {
		return self.readings.origin("the resolver's own DEPTH LIMIT, which refuses rather than answering",
			eraseOrigin{kind: eraseOriginUndecided, what: "the resolver ran out of depth"})
	}
	switch shaped := value.(type) {
	case *ast.ParenExpr:
		return self.readings.origin("the parenthesis peel",
			self.originOf(shaped.X, inside, depth+1, seen))
	case *ast.StarExpr:
		return self.readings.origin("the dereference peel",
			self.originOf(shaped.X, inside, depth+1, seen))
	case *ast.TypeAssertExpr:
		return self.readings.origin("the type-assertion peel",
			self.originOf(shaped.X, inside, depth+1, seen))
	case *ast.SliceExpr:
		// a reslice is the SAME array. This is one of the four forms a spelling-keyed gate
		// walks straight past.
		return self.readings.origin("the slice-expression peel",
			self.originOf(shaped.X, inside, depth+1, seen))
	case *ast.IndexExpr:
		return self.readings.origin("the index peel",
			self.originOf(shaped.X, inside, depth+1, seen))
	case *ast.UnaryExpr:
		if shaped.Op == token.AND {
			return self.readings.origin("the address-of peel",
				self.originOf(shaped.X, inside, depth+1, seen))
		}
		return self.readings.origin("a unary operator that is not address-of, which computes a value of its own",
			eraseOrigin{kind: eraseOriginFresh, what: "a computed value"})
	case *ast.BasicLit, *ast.CompositeLit, *ast.FuncLit, *ast.BinaryExpr:
		return self.readings.origin("a literal, a composite literal, a function literal or a binary expression",
			eraseOrigin{kind: eraseOriginFresh, what: "a literal"})
	case *ast.SelectorExpr:
		if base, isIdentifier := eraseUnparen(shaped.X).(*ast.Ident); isIdentifier {
			// THE INNERMOST SCOPE THAT BINDS THE BASE decides, not the declaration the statement
			// happens to be written inside.
			if binding, isBound := eraseBindingOf(self.readings, inside, base.Name); isBound && !binding.local {
				return self.readings.origin("the SELECTOR arm, which answers the base's own binding and names the field",
					eraseOrigin{kind: binding.kind, what: base.Name + "." + shaped.Sel.Name})
			}
		}
		return self.readings.origin("the selector PEEL, onto a base no scope of the chain binds as a parameter or the receiver",
			self.originOf(shaped.X, inside, depth+1, seen))
	case *ast.Ident:
		return self.readings.origin("the identifier arm",
			self.originOfIdent(shaped, inside, depth, seen))
	case *ast.CallExpr:
		return self.readings.origin("the call arm",
			self.originOfCall(shaped, 0, inside, depth, seen))
	}
	return self.readings.origin("the REFUSAL of an expression form this gate has no peel for",
		eraseOrigin{kind: eraseOriginUndecided, what: fmt.Sprintf("%T", value)})
}

// originOfResult is originOf at a RESULT POSITION, which is what the multi-value assignment form
// needs and what the first version of this gate had no way to ask for.
//
// x.Field, err = f() binds the field from result 0 and err from result 1. Resolving the call as a
// whole would answer result 0 for every target, so a field bound at position 1 would be decided by
// the origin of a value it never receives.
func (self *eraseResolver) originOfResult(value ast.Expr, result int, inside *eraseScope,
	depth int, seen map[string]bool) eraseOrigin {

	if result == 0 {
		return self.readings.origin("the result-position reading AT RESULT 0, which is originOf itself",
			self.originOf(value, inside, depth, seen))
	}
	switch shaped := eraseUnparen(value).(type) {
	case *ast.CallExpr:
		return self.readings.origin("the result-position reading of a CALL",
			self.originOfCall(shaped, result, inside, depth, seen))
	case *ast.IndexExpr, *ast.TypeAssertExpr:
		// the comma-ok forms, v, ok := m[k] and v, ok := x.(T). Result 1 is a bool and carries
		// no array at all.
		return self.readings.origin("the ok of a COMMA-OK form",
			eraseOrigin{kind: eraseOriginFresh, what: "the ok of a comma-ok form"})
	case *ast.UnaryExpr:
		if shaped.Op == token.ARROW {
			return self.readings.origin("the ok of a CHANNEL RECEIVE",
				eraseOrigin{kind: eraseOriginFresh, what: "the ok of a channel receive"})
		}
	}
	return self.readings.origin("the UNDECIDED default of the result-position reading",
		eraseOrigin{kind: eraseOriginUndecided,
			what: fmt.Sprintf("result %d of a %T, a form this gate has no reading for", result, value)})
}

func (self *eraseResolver) originOfIdent(name *ast.Ident, inside *eraseScope, depth int,
	seen map[string]bool) eraseOrigin {

	if name.Name == "nil" {
		return self.readings.origin("nil, which is no array at all",
			eraseOrigin{kind: eraseOriginFresh, what: "nil"})
	}
	// THE INNERMOST SCOPE THAT BINDS IT, which is Go's own rule and was this gate's own hole: a
	// site inside a closure used to be resolved against the enclosing declaration, so a closure
	// PARAMETER holding the caller's array was answered by whatever same-named local the frame
	// outside happened to have.
	binding, isBound := eraseBindingOf(self.readings, inside, name.Name)
	if !isBound {
		return self.readings.origin("the REFUSAL of a name no scope of the chain binds",
			eraseOrigin{kind: eraseOriginUndecided,
				what: fmt.Sprintf("%q is neither a parameter, the receiver, nor a local this gate found an assignment for, in any scope enclosing the statement", name.Name)})
	}
	if binding.kind == eraseOriginReceiver && !binding.local {
		return self.readings.origin("a name bound as the RECEIVER of some scope of the chain",
			eraseOrigin{kind: binding.kind, what: name.Name})
	}
	if !binding.local {
		return self.readings.origin("a name bound as a PARAMETER of some scope of the chain",
			eraseOrigin{kind: binding.kind, what: name.Name})
	}
	// KEYED BY THE SCOPE AND NOT BY THE FUNCTION NAME ALONE. Two methods of the same name on two
	// types are two bodies, and now that a method call is opened they can both be on one
	// resolution chain; a key that named only the method would call the second a cycle and answer
	// "fresh" having read nothing. A literal's scope carries its own name for the same reason.
	key := binding.scope.name + "." + name.Name
	if seen[key] {
		return self.readings.origin("the SCOPE-qualified cycle key of a local",
			eraseOrigin{kind: eraseOriginFresh, what: "a cycle, already resolved"})
	}
	seen[key] = true
	// a LOCAL: every value it is ever assigned, resolved IN THE SCOPE THAT DECLARES IT. The most
	// alias-y answer wins, because a local that holds the caller's array on ONE path holds it.
	worst := eraseOrigin{kind: eraseOriginFresh, what: "a fresh array"}
	for _, assigned := range binding.values {
		// AT THE RESULT POSITION THE LOCAL WAS BOUND AT, which is the fifth defect this line
		// carried open for a round: `first, second := twoOut(x)` hands `second` result 1, and
		// resolving the call as a whole answered result 0 -- an array that local never receives.
		origin := self.originOfResult(assigned.value, assigned.result, binding.scope, depth+1, seen)
		if origin.kind == eraseOriginParameter {
			return self.readings.origin("a LOCAL whose assignment reaches a parameter, which is the alias-y answer winning", origin)
		}
		if origin.kind == eraseOriginUndecided || origin.kind == eraseOriginOpaque {
			worst = origin
		}
	}
	return self.readings.origin("a LOCAL answered by every value its own scope assigns it", worst)
}

func (self *eraseResolver) originOfCall(call *ast.CallExpr, result int, inside *eraseScope,
	depth int, seen map[string]bool) eraseOrigin {

	if result == 0 {
		switch eraseUnparen(call.Fun).(type) {
		case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.InterfaceType, *ast.StructType:
			// a conversion written as a TYPE LITERAL, which is how `[]byte(nil)` is spelled --
			// the destination of one of the two copy spellings this coupling actually uses. It
			// is a conversion like any other: peel it.
			if len(call.Args) == 1 {
				return self.readings.origin("a conversion written as a TYPE LITERAL, which is how []byte(nil) is spelled",
					self.originOf(call.Args[0], inside, depth+1, seen))
			}
		}
	}
	callee, isIdentifier := eraseUnparen(call.Fun).(*ast.Ident)
	if !isIdentifier {
		if selector, isSelector := eraseUnparen(call.Fun).(*ast.SelectorExpr); isSelector {
			if origin, decided := self.originOfMethodCall(call, selector, result, inside, depth, seen); decided {
				return origin
			}
		}
		// a qualified call, or a method reached through an INTERFACE or a foreign type. This is
		// the open edge this gate discloses: admitted, and named in complement 3.
		return self.readings.origin("the OPAQUE answer for a callee whose Fun is not a bare identifier and whose body this gate does not have",
			eraseOrigin{kind: eraseOriginOpaque, what: eraseRender(token.NewFileSet(), call.Fun)})
	}
	if result == 0 {
		switch callee.Name {
		case "append":
			// append writes into its DESTINATION's array whenever that array has room, so the
			// origin of an append is the origin of its first argument. append(T(nil), x...) is
			// fresh; append(callersSlice, x...) is the caller's.
			if len(call.Args) == 0 {
				return self.readings.origin("append with NO arguments at all",
					eraseOrigin{kind: eraseOriginFresh, what: "append of nothing"})
			}
			return self.readings.origin("append's DESTINATION deciding the append",
				self.originOf(call.Args[0], inside, depth+1, seen))
		case "make", "new":
			return self.readings.origin("the make and new arm",
				eraseOrigin{kind: eraseOriginFresh, what: callee.Name})
		case "len", "cap", "copy", "int", "uint", "byte", "string", "uint16", "uint32", "uint64":
			return self.readings.origin("the BUILTIN arm, over the builtins that answer a value rather than an array of their argument",
				eraseOrigin{kind: eraseOriginFresh, what: callee.Name})
		}
		if self.types[callee.Name] && len(call.Args) == 1 {
			// a conversion this package declares. It renames the type and keeps the array,
			// which is the second of the four forms a spelling-keyed gate walks past.
			return self.readings.origin("a conversion to a type THIS PACKAGE DECLARES",
				self.originOf(call.Args[0], inside, depth+1, seen))
		}
	}
	declared, isDeclared := self.functions[callee.Name]
	if !isDeclared || declared.Body == nil {
		return self.readings.origin("the OPAQUE answer for a function this package does not declare",
			eraseOrigin{kind: eraseOriginOpaque, what: callee.Name})
	}
	// ONE HOP INTO THE CALLEE. Whatever it returns at this result position is resolved against
	// ITS parameters, and a return rooted at one of them is mapped back onto the matching
	// argument here. cloneBytes is decided by this arm and not by its name: it returns `out`, a
	// local assigned from make, so the array it answers is fresh.
	return self.readings.origin("ONE HOP into a function this package declares",
		self.originOfBody(declared, callee.Name, result, call.Args, nil, inside, depth, seen))
}

// originOfMethodCall opens a call spelled x.M(...) when M is a method THIS PACKAGE DECLARES on a
// CONCRETE type this package declares, and x's own declaration says which one.
//
// THIS IS NOT THE OPEN EDGE THIS GATE DISCLOSES, which is why it is closed here rather than added
// to complement 3. That edge is about callees whose body this gate does not have -- a function of
// another package, or a method reached through an interface, where the source says nothing about
// which implementation runs. A method on a struct of this package is neither: its body is in the
// files this gate already parsed, so admitting it as "a call this gate did not open" was a false
// statement about this gate's own reach, and it admitted h.Bytes() -- a plain alias -- in silence.
//
// AN INTERFACE RECEIVER STAYS OPAQUE and stays named in complement 3. crypto.DeriveSecret could be
// any implementation of CryptoProvider, and reading the one that happens to live in this package
// would be a guess wearing a derivation.
//
// AND A METHOD PROMOTED FROM AN EMBEDDED STRUCT IS OPENED TOO, because it satisfies every word of
// the condition above and the first version of this arm did not open it. `o.Held()` where Held is
// declared on a struct o embeds is declared in this package, on a type this package declares, with
// its body already parsed here -- the only thing standing between it and this reading was a map
// lookup keyed to the outer type name. A condition that describes more than the code does is the
// defect this whole line is about, so the code was widened to the condition rather than the
// condition narrowed to the code.
//
// The second bool says whether this reading DECIDED. False hands the call back to the opaque arm.
func (self *eraseResolver) originOfMethodCall(call *ast.CallExpr, selector *ast.SelectorExpr,
	result int, inside *eraseScope, depth int, seen map[string]bool) (eraseOrigin, bool) {

	owner := self.declaredTypeNameOf(selector.X, inside, 0)
	if owner == "" {
		return self.readings.declined("a receiver whose declared type this gate could not name")
	}
	shape, isStruct := self.structs[owner]
	if !isStruct || !shape.isStruct {
		return self.readings.declined("a receiver whose declared type is not a struct this package declares")
	}
	name := fmt.Sprintf("(%s).%s", owner, selector.Sel.Name)
	declared, isDeclared := self.methods[owner][selector.Sel.Name]
	if !isDeclared || declared.Body == nil {
		promoted, from, isPromoted := self.promotedMethod(owner, selector.Sel.Name)
		if !isPromoted {
			return self.readings.declined("a method neither declared on the receiver's own type nor promoted from a struct it embeds")
		}
		declared = promoted
		name = fmt.Sprintf("(%s).%s promoted from (%s)", owner, selector.Sel.Name, from)
		return self.readings.origin("ONE HOP into a method PROMOTED from an embedded struct",
			self.originOfBody(declared, name, result, call.Args, selector.X, inside, depth, seen)), true
	}
	return self.readings.origin("ONE HOP into a method this package declares on a concrete type of its own",
		self.originOfBody(declared, name, result, call.Args, selector.X, inside, depth, seen)), true
}

// promotedMethod answers the method one type gets from a struct it EMBEDS, and the type that
// declares it.
//
// GO'S OWN PROMOTION RULE, which is why this is breadth-first and why an ambiguity refuses. A
// method at embedding depth 1 shadows one at depth 2; two at the SAME depth promote neither, and Go
// rejects the call outright. Refusing there hands the call back to the opaque arm, which is the
// safe direction: an admission this reading is not sure of would be a guess wearing a derivation,
// and an opaque call is named in complement 3 on every run.
//
// An embedded INTERFACE is not in structs, so it is skipped and the call stays opaque -- the same
// line the interface receiver sits on, for the same reason.
func (self *eraseResolver) promotedMethod(owner string, method string) (*ast.FuncDecl, string, bool) {
	frontier := []string{owner}
	seen := map[string]bool{owner: true}
	for depth := 0; depth < eraseResolverDepth && len(frontier) > 0; depth += 1 {
		next := []string{}
		found := []*ast.FuncDecl{}
		from := []string{}
		for _, at := range frontier {
			shape, isStruct := self.structs[at]
			if !isStruct {
				continue
			}
			for _, embedded := range shape.embedded {
				if seen[embedded] {
					continue
				}
				seen[embedded] = true
				next = append(next, embedded)
				declared, isDeclared := self.methods[embedded][method]
				if isDeclared && declared.Body != nil {
					found = append(found, declared)
					from = append(from, embedded)
				}
			}
		}
		if len(found) == 1 {
			return found[0], from[0], true
		}
		if len(found) > 1 {
			return nil, "", false
		}
		frontier = next
	}
	return nil, "", false
}

// originOfBody is the one hop, shared by the function arm and the method arm.
//
// receiver is the expression the method was called ON, or nil for a plain function. A body that
// answers its RECEIVER's own state is answering an array that belongs to whatever the receiver
// expression is rooted at in the CALLER's frame -- which is how h.Bytes(), where h is a parameter,
// is the caller's array spelled as a method call.
func (self *eraseResolver) originOfBody(declared *ast.FuncDecl, name string, result int,
	arguments []ast.Expr, receiver ast.Expr, inside *eraseScope, depth int,
	seen map[string]bool) eraseOrigin {

	key := fmt.Sprintf("call:%s:%d", name, result)
	if seen[key] {
		return self.readings.origin("the RESULT-QUALIFIED cycle key of a callee",
			eraseOrigin{kind: eraseOriginFresh, what: "a recursive callee, already resolved"})
	}
	seen[key] = true
	callee := eraseScopeOf(declared)
	worst := eraseOrigin{kind: eraseOriginFresh, what: name + " answers a fresh array"}
	returned, forwarded := eraseReturnExpressionsAt(self.readings, declared, result)
	for _, returnedAt := range returned {
		// AT THE RESULT POSITION THE RETURNED VALUE WAS BOUND AT. A plain `return a, b` binds
		// each result from its own expression and carries position 0; a NAMED result assigned by
		// `out, err = f()` carries the position it was assigned at, which is the same defect
		// eraseAssignmentsTo carried and is closed in the same place.
		origin := self.originOfResult(returnedAt.value, returnedAt.result, callee, depth+1, seen)
		switch origin.kind {
		case eraseOriginReceiver:
			if receiver == nil {
				// a plain function has no receiver, so a receiver-rooted return out of one
				// is a reading this gate cannot map back onto anything here.
				return self.readings.origin("a callee answering RECEIVER state that this gate called as a plain function",
					eraseOrigin{kind: eraseOriginUndecided,
						what: fmt.Sprintf("%s answers receiver state and this gate called it as a function", name)})
			}
			return self.readings.origin("a callee answering its RECEIVER's own state, mapped back onto the receiver expression",
				self.originOf(receiver, inside, depth+1, seen))
		case eraseOriginParameter:
			at := eraseParameterIndex(declared, strings.Split(origin.what, ".")[0])
			if at < 0 || at >= len(arguments) {
				return self.readings.origin("a callee answering a parameter this gate could not map back onto an argument",
					eraseOrigin{kind: eraseOriginUndecided,
						what: fmt.Sprintf("%s answers its own parameter %q and this gate could not map it back onto an argument",
							name, origin.what)})
			}
			return self.readings.origin("a callee answering its OWN PARAMETER, mapped back onto the matching argument",
				self.originOf(arguments[at], inside, depth+1, seen))
		case eraseOriginUndecided, eraseOriginOpaque:
			worst = self.readings.origin("a callee answering UNDECIDED or OPAQUE, carried out under the callee's name",
				eraseOrigin{kind: origin.kind, what: name + ": " + origin.what})
		}
	}
	// A FORWARDED MULTI-VALUE RETURN, `return g()` out of a function with several results. The
	// results cannot be split apart syntactically, so the same result position is asked of g.
	// Without this arm a forwarding body answered "fresh" having read nothing, which is the
	// silent admit this gate exists to refuse.
	for _, forwardedAt := range forwarded {
		origin := self.originOfResult(forwardedAt.value, forwardedAt.result, callee, depth+1, seen)
		switch origin.kind {
		case eraseOriginParameter:
			at := eraseParameterIndex(declared, strings.Split(origin.what, ".")[0])
			if at < 0 || at >= len(arguments) {
				return self.readings.origin("a FORWARDED return answering a parameter this gate could not map back onto an argument",
					eraseOrigin{kind: eraseOriginUndecided,
						what: fmt.Sprintf("%s forwards its own parameter %q and this gate could not map it back onto an argument",
							name, origin.what)})
			}
			return self.readings.origin("a FORWARDED return answering the callee's own parameter, mapped back onto the matching argument",
				self.originOf(arguments[at], inside, depth+1, seen))
		case eraseOriginReceiver:
			if receiver == nil {
				return self.readings.origin("a FORWARDED return answering receiver state out of a plain function",
					eraseOrigin{kind: eraseOriginUndecided,
						what: fmt.Sprintf("%s forwards receiver state and this gate called it as a function", name)})
			}
			return self.readings.origin("a FORWARDED return answering the callee's RECEIVER state, mapped back onto the receiver expression",
				self.originOf(receiver, inside, depth+1, seen))
		case eraseOriginUndecided, eraseOriginOpaque:
			worst = self.readings.origin("a FORWARDED return answering UNDECIDED or OPAQUE, carried out under the callee's name",
				eraseOrigin{kind: origin.kind, what: name + " forwards: " + origin.what})
		}
	}
	return self.readings.origin("a callee this gate opened and found no caller-rooted answer in", worst)
}

// ---------------------------------------------------------------------------
// the small readings
// ---------------------------------------------------------------------------

type eraseSource struct {
	path   string
	parsed *ast.File
}

func eraseSources(t *testing.T) (*token.FileSet, []eraseSource) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	fileSet := token.NewFileSet()
	sources := []eraseSource{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		sources = append(sources, eraseSource{path: name, parsed: parsed})
	}
	if len(sources) == 0 {
		t.Fatal("no production go file was read out of this package, so this gate scanned nothing")
	}
	return fileSet, sources
}

// eraseParameterNames answers the parameters ONE SCOPE declares -- a declaration's, or a function
// literal's own, which is the pair a name is told apart by.
func eraseParameterNames(scope *eraseScope) map[string]bool {
	names := map[string]bool{}
	signature := scope.signature()
	if signature == nil || signature.Params == nil {
		return names
	}
	for _, field := range signature.Params.List {
		for _, name := range field.Names {
			names[name.Name] = true
		}
	}
	return names
}

func eraseParameterIndex(function *ast.FuncDecl, name string) int {
	at := 0
	if function.Type == nil || function.Type.Params == nil {
		return -1
	}
	for _, field := range function.Type.Params.List {
		for _, declared := range field.Names {
			if declared.Name == name {
				return at
			}
			at += 1
		}
	}
	return -1
}

// eraseFunctionKey names one declaration uniquely across this package: (Type).Method for a method,
// and the bare name for a function.
func eraseFunctionKey(function *ast.FuncDecl) string {
	if function == nil {
		return ""
	}
	if function.Recv != nil && len(function.Recv.List) == 1 {
		if owner := eraseTypeNameOf(function.Recv.List[0].Type); owner != "" {
			return "(" + owner + ")." + function.Name.Name
		}
	}
	return function.Name.Name
}

// eraseReceiverName answers the receiver name a DECLARATION binds, and "" for a function literal. A
// literal written inside a method still reaches the receiver -- through the chain, at the scope
// that declares it.
func eraseReceiverName(scope *eraseScope) string {
	receiver := scope.receiver()
	if receiver == nil || len(receiver.List) != 1 || len(receiver.List[0].Names) != 1 {
		return ""
	}
	return receiver.List[0].Names[0].Name
}

func eraseTypeNameOf(value ast.Expr) string {
	switch shaped := value.(type) {
	case *ast.StarExpr:
		return eraseTypeNameOf(shaped.X)
	case *ast.ParenExpr:
		return eraseTypeNameOf(shaped.X)
	case *ast.Ident:
		return shaped.Name
	case *ast.IndexExpr:
		return eraseTypeNameOf(shaped.X)
	case *ast.IndexListExpr:
		return eraseTypeNameOf(shaped.X)
	}
	return ""
}

// eraseAssignment is one value a local -- or a NAMED RESULT -- is assigned, together with the RESULT
// POSITION it takes its array from.
//
// THE POSITION IS THE WHOLE POINT. `first, second := twoOut(x)` gives BOTH targets the same one
// expression on the right, and only the position tells second's array from first's. A reading that
// dropped it decided a local by an array it never receives, which is the fifth defect of this line.
type eraseAssignment struct {
	value  ast.Expr
	result int
}

// eraseAssignmentsTo answers every value a local is ever assigned inside one scope, including the
// range and the type-switch forms, so that a local standing in front of a parameter is not a hole
// in the derivation.
//
// IT DOES DESCEND INTO NESTED LITERALS, unlike the two readings above it, and on purpose: a closure
// that writes to a local it CAPTURED is writing to that local, so a reading that stopped at the
// literal would miss an array arriving from inside one. The cost is stated rather than hidden --
// where a nested literal shadows the name with a parameter of its own, an assignment to the
// literal's parameter is attributed to the outer local. That is the ALIAS-Y direction, which is the
// safe one here; and when such a value is then resolved against the outer scope, the shadowing name
// is not bound there and the site comes out UNDECIDED, which is refused rather than admitted.
//
// AND THE FIFTH DEFECT OF THIS LINE WAS THE MULTI-VALUE ARM OF THIS READING, AND IT IS CLOSED HERE.
//
// It recorded the ONE expression on the right for EVERY target on the left, so the RESULT POSITION
// was lost: a local bound at result 1 of a two-result call was decided by the origin of result 0 --
// an array it never receives.
//
//	func twoOut(x []byte) ([]byte, []byte) { return copyOf(x), x }
//
//	func probe(x []byte) *Held {
//		h := &Held{}
//		first, second := twoOut(x)
//		_ = first
//		h.Secret = second                 // <- the caller's array
//		return h
//	}
//
// Driven through this resolver over exactly that corpus, `h.Secret = second` came out "a fresh
// array" -- an alias admitted in silence, which is the thing this gate exists to refuse. It was
// measured LATENT rather than live: with every local bound at a result position other than 0 forced
// to have no assignment at all, the gate over this package answered the SAME twenty fill sites and
// stayed green, so nothing in connect/mls resolved through such a local.
//
// THE REPAIR IS THE ONE THE ROUND THAT FOUND IT NAMED, and it was small for the reason it gave: the
// FILL-SITE WALK already had this right -- it passes the target INDEX and resolves through
// originOfResult -- so the position only had to be carried alongside each value here. Every
// assignment is now a value AND the result position it takes its array from, and originOfIdent
// resolves through originOfResult rather than originOf. eraseReturnExpressionsAt carries the same
// pair for the same reason: a NAMED result assigned by `out, err = f()` had the identical hole.
//
// It is driven by alias24/copy24 of the spelling corpus, which differ only in WHICH result of a
// two-result callee holds the caller's array; before the repair alias24 answered "a fresh array".
func eraseAssignmentsTo(readings *eraseReadings, scope *eraseScope, name string) []eraseAssignment {
	values := []eraseAssignment{}
	body := scope.body()
	if body == nil {
		return values
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch shaped := node.(type) {
		case *ast.AssignStmt:
			// the one-to-one form.
			if len(shaped.Lhs) == len(shaped.Rhs) {
				for at, target := range shaped.Lhs {
					if identifier, isIdentifier := target.(*ast.Ident); isIdentifier && identifier.Name == name {
						values = append(values, eraseAssignment{
							value: shaped.Rhs[at],
							result: readings.at(
								"the ONE-TO-ONE form of a local's assignments, where each target takes its own expression whole", 0),
						})
					}
				}
				return true
			}
			// AND THE MULTI-VALUE FORM, `a, err := f()`, which is not a corner: it is how
			// every decode in this package binds the bytes it just read, and a resolver that
			// did not know it reported those locals UNDECIDED. The array comes out of the one
			// call on the right AT THIS TARGET'S OWN POSITION, which is what the fifth defect
			// of this line lost.
			if len(shaped.Rhs) != 1 {
				return true
			}
			for at, target := range shaped.Lhs {
				if identifier, isIdentifier := target.(*ast.Ident); isIdentifier && identifier.Name == name {
					values = append(values, eraseAssignment{
						value: shaped.Rhs[0],
						result: readings.at(
							"the MULTI-VALUE form of a local's assignments, at THIS target's own result position", at),
					})
				}
			}
		case *ast.ValueSpec:
			if len(shaped.Values) != len(shaped.Names) {
				return true
			}
			for at, declared := range shaped.Names {
				if declared.Name == name {
					values = append(values, eraseAssignment{
						value:  shaped.Values[at],
						result: readings.at("a VAR DECLARATION binding the local", 0),
					})
				}
			}
		case *ast.RangeStmt:
			if identifier, isIdentifier := shaped.Value.(*ast.Ident); isIdentifier && identifier.Name == name {
				values = append(values, eraseAssignment{
					value:  shaped.X,
					result: readings.at("a RANGE binding the local, once per iteration, out of the ranged array", 0),
				})
			}
		}
		return true
	})
	return values
}

// eraseReturnExpressionsAt answers every value one function can return AT ONE RESULT POSITION, and
// separately the calls it FORWARDS whole.
//
// The single-result restriction the first version carried was not a corner either: a function
// answering (T, error) -- which is most of this package -- was opened, matched nothing, and
// answered "fresh" having read no return at all. A body this gate cannot read must not look like a
// body that makes its own array.
//
// THE RETURN WALK STOPS AT A NESTED LITERAL, and this is the same scoping correction as the one in
// the fill-site walk. `return x` written inside a closure is the CLOSURE's return, not this
// function's; counting it here both invented a return this function never makes and handed the
// caller-side mapping below a name that is a parameter of the literal rather than of this
// function. The NAMED-result reading below is the deliberate exception: a deferred literal that
// assigns a named result really does decide what this function returns.
func eraseReturnExpressionsAt(readings *eraseReadings, function *ast.FuncDecl,
	at int) ([]eraseAssignment, []eraseAssignment) {

	values := []eraseAssignment{}
	forwarded := []eraseAssignment{}
	if function == nil || function.Body == nil || function.Type == nil ||
		function.Type.Results == nil {
		return values, forwarded
	}
	names := []string{}
	for _, field := range function.Type.Results.List {
		if len(field.Names) == 0 {
			names = append(names, "")
			continue
		}
		for _, declared := range field.Names {
			names = append(names, declared.Name)
		}
	}
	if at < 0 || at >= len(names) {
		return values, forwarded
	}
	// a NAMED result is resolved through its name, so a body that assigns it and returns bare is
	// not a hole.
	if names[at] != "" {
		for _, assigned := range eraseAssignmentsTo(readings, eraseScopeOf(function), names[at]) {
			values = append(values, eraseAssignment{
				value:  assigned.value,
				result: readings.at("the NAMED-result reading of a bare return", assigned.result),
			})
		}
	}
	eraseInspectScope(function.Body, func(node ast.Node) bool {
		statement, isReturn := node.(*ast.ReturnStmt)
		if !isReturn {
			return true
		}
		if len(statement.Results) == len(names) {
			values = append(values, eraseAssignment{
				value: statement.Results[at],
				result: readings.at(
					"a return whose expressions match the callee's result list one for one", 0),
			})
			return true
		}
		if len(statement.Results) == 1 && len(names) > 1 {
			forwarded = append(forwarded, eraseAssignment{
				value:  statement.Results[0],
				result: readings.at("a return that FORWARDS a multi-value call whole", at),
			})
		}
		return true
	})
	return values, forwarded
}

func eraseUnparen(value ast.Expr) ast.Expr {
	for {
		parenthesised, isParen := value.(*ast.ParenExpr)
		if !isParen {
			return value
		}
		value = parenthesised.X
	}
}

func eraseRender(fileSet *token.FileSet, value ast.Node) string {
	buffer := &bytes.Buffer{}
	if err := printer.Fprint(buffer, fileSet, value); err != nil {
		return fmt.Sprintf("%T", value)
	}
	return strings.Join(strings.Fields(buffer.String()), " ")
}

// maps_Keys is maps.Keys, spelled locally so this file adds no import its neighbours do not carry.
func maps_Keys[V any](of map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range of {
			if !yield(key) {
				return
			}
		}
	}
}
