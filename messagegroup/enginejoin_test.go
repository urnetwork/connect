// The first standing proof in either tree of "two clients, one group".
//
// It is named enginejoin_test.go and not join_test.go on purpose: another plan's file block
// already claims join_test.go in this package, and two plans creating one file is how a
// dispatched task discovers a merge.
//
// WHAT THIS FILE DOES NOT ESTABLISH IS STATED IN ITS OWN TEST, in four sentences the gate at the
// bottom asserts are present. An absence that is named is safe and an absence that looks like a
// placeholder is not, and a file that proves two clients share a group and does not say those
// four things is a file the next reader will cite as the milestone.
//
// AND THREE OF THOSE SENTENCES WERE WRONG, which is why the list is worth reading before the code.
// Two understated what this package does -- a DURABLE record DOES cross between these two engines,
// and BOTH sides lose their session at the add rather than the joiner alone -- and the third was
// missing: the welcome anchors nothing. A sentence that is wrong in the modest direction is still
// wrong, and it is the expensive kind: it makes the next planner budget for work already done.
//
// THIS FILE DOES NOT DECLARE CP3B, and the distinction is deliberate rather than cautious. What it
// establishes is written above each case and measured by it; whether that meets a milestone is the
// owner's disposition and is made in PROGRESS.md, not here. What a reader of that file should know
// is that its stated blocker for CP3b -- "JoinFromWelcome is an unconditional refusal, so no
// exported path lets two clients share one group" -- has not been true since j1 task 5.
package messagegroup

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/mls"
)

// The exporter label and length the whole claim rests on. It is MASTER section 7's mls_secret:
// the value GroupSession.installEpochOnLoop reads to build every key of the record layer, so two
// members that disagree here disagree about every record either of them will ever seal.
const engineJoinExporterLabel = "URmessage/v1/storage"

const engineJoinExporterLength = 32

// TestTwoEnginesShareOneGroupAndTheirExportersAgree is the whole claim of j1's CP3b prefix.
//
// THE CHAIN: A CreateGroup; B NewKeyPackage; A ProposeAdd(kpB); A Commit(nil) answering a
// non-empty commit, welcome and ratchet tree; A MergePendingCommit; B JoinFromWelcome.
//
// FIVE CLAUSES, AND THE EXPORTER IS THE PROPERTY WHILE THE OTHER FOUR ARE ITS PRECONDITIONS.
// Group id, epoch and member count agree between a joiner that really joined and a joiner that
// built plausible state out of a Welcome it mis-derived; the exported secret does not.
//
// Commit(nil) is what produces the Welcome: connect/mls's nil arm commits every cached proposal,
// and ProposeAdd caches locally, so no new seam method is needed for the founder to answer one.
// An empty non-nil vector commits NOTHING and is a hazard this file names rather than relies on.
func TestTwoEnginesShareOneGroupAndTheirExportersAgree(t *testing.T) {
	a := newTestEngine(t)
	b := newTestEngine(t)

	keyPackage, err := b.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("B's NewKeyPackage: %v", err)
	}
	founder := a.createGroup(t, "two-clients-one-group")
	defer founder.Close()

	if _, err := founder.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("A's ProposeAdd over B's key package: %v", err)
	}
	commit, welcome, ratchetTree, err := founder.Commit(nil)
	if err != nil {
		t.Fatalf("A's Commit(nil): %v", err)
	}
	t.Logf("the founder's commit answered commit=%d welcome=%d ratchetTree=%d octets",
		len(commit), len(welcome), len(ratchetTree))
	if len(commit) == 0 || len(welcome) == 0 || len(ratchetTree) == 0 {
		t.Fatalf("the commit answered commit=%d welcome=%d ratchetTree=%d and every one of the three has to carry octets",
			len(commit), len(welcome), len(ratchetTree))
	}
	if err := founder.MergePendingCommit(); err != nil {
		t.Fatalf("A's MergePendingCommit: %v", err)
	}

	joined, err := b.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("B's JoinFromWelcome: %v", err)
	}
	defer joined.Close()

	if !bytes.Equal(joined.GroupId(), founder.GroupId()) {
		t.Errorf("B is in group %x and A is in %x", joined.GroupId(), founder.GroupId())
	}
	if joined.Epoch() != founder.Epoch() {
		t.Errorf("B is at epoch %d and A is at %d", joined.Epoch(), founder.Epoch())
	}
	if count := founder.MemberCount(); count != 2 {
		t.Errorf("A sees %d members, want 2", count)
	}
	if count := joined.MemberCount(); count != 2 {
		t.Errorf("B sees %d members, want 2", count)
	}
	// each finds the OTHER's identity, which is what says the two handles describe one membership
	// rather than two groups that happen to agree on a count
	engineJoinAssertFinds(t, "A", founder, b.identityPub)
	engineJoinAssertFinds(t, "B", joined, a.identityPub)

	// THE CLAUSE THAT IS THE PROPERTY.
	founderSecret, err := founder.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("A's Export: %v", err)
	}
	joinedSecret, err := joined.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("B's Export: %v", err)
	}
	t.Logf("both handles export %x under %q at epoch %d with %d members",
		founderSecret, engineJoinExporterLabel, founder.Epoch(), founder.MemberCount())
	if !bytes.Equal(founderSecret, joinedSecret) {
		t.Errorf("A exports %x and B exports %x under %q; two members that disagree here disagree about every record either of them will ever seal",
			founderSecret, joinedSecret, engineJoinExporterLabel)
	}
	if len(founderSecret) != engineJoinExporterLength {
		t.Errorf("the exporter answered %d octets, want %d", len(founderSecret), engineJoinExporterLength)
	}

	// AND THE DEVICE SURVIVED ITS OWN JOIN, read through the ratchet tree and NEVER through
	// MemberAt: MemberAt answers Credential.Identity, which a destroyed signing key does not
	// touch, and this whole chain is green over an engine B whose identity was erased by its own
	// join. A second key package and a second group are the admissible observation.
	afterJoin, err := b.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("B's NewKeyPackage after the join: %v", err)
	}
	if named := engineKeyPackageLeafKeyOf(t, afterJoin); !bytes.Equal(named, b.signerPub) {
		t.Errorf("B's key package after the join names %x and B signs with %x; the join destroyed the device's identity",
			named, b.signerPub)
	}
	second := b.createGroup(t, "b-founds-after-joining")
	defer second.Close()
	if named, _ := engineLeafKeyOf(t, second, second.OwnLeafIndex()); !bytes.Equal(named, b.signerPub) {
		t.Errorf("a group B founds after the join names %x at leaf 0 and B signs with %x", named, b.signerPub)
	}
}

// engineJoinAssertFinds walks a handle's membership and requires it to carry one identity.
func engineJoinAssertFinds(t *testing.T, who string, handle GroupHandle, identityPub []byte) {
	t.Helper()
	for at := 0; at < handle.MemberCount(); at += 1 {
		_, found, _, err := handle.MemberAt(at)
		if err != nil {
			t.Fatalf("%s's MemberAt(%d): %v", who, at, err)
		}
		if bytes.Equal(found, identityPub) {
			return
		}
	}
	t.Errorf("%s's membership does not carry the identity %x", who, identityPub)
}

// TestTheTwoEnginesOfAJoinShareNoState is j1 task 6's second property, and the gate PROVES it
// rather than arranging it.
//
// Two providers, two stores, two signers, two credentials, two leaf-keys bodies. THE REASON THIS
// MUTANT IS SOUND IS NOT THE OBVIOUS ONE, and the obvious one is wrong: a shared store does NOT
// make the exporter equality a tautology. Export delegates to the group's epoch key schedule, held
// IN MEMORY by two distinct groups; the store is written by persist and read only by LoadGroup,
// which has zero callers outside connect/mls's own tests. So the first property going on passing
// under one store would prove nothing about the exporter -- and what a shared store DOES break is
// this property's own observation: that B's store holds no group state before the join and A's is
// unchanged by it.
func TestTheTwoEnginesOfAJoinShareNoState(t *testing.T) {
	a := newTestEngine(t)
	b := newTestEngine(t)
	if a.store == b.store {
		t.Fatal("the two engines were built over one store, so every observation below is about one device")
	}
	if bytes.Equal(a.signerPub, b.signerPub) || bytes.Equal(a.identityPub, b.identityPub) ||
		bytes.Equal(a.leafKeys, b.leafKeys) {
		t.Fatal("the two engines share a signer, a credential or a leaf keys body, so they are one device wearing two names")
	}

	keyPackage, err := b.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("B's NewKeyPackage: %v", err)
	}
	founder := a.createGroup(t, "no-shared-state")
	defer founder.Close()
	if _, err := founder.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("A's ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := founder.Commit(nil)
	if err != nil {
		t.Fatalf("A's Commit(nil): %v", err)
	}
	if err := founder.MergePendingCommit(); err != nil {
		t.Fatalf("A's MergePendingCommit: %v", err)
	}

	if held := len(b.store.groupStates); held != 0 {
		t.Errorf("B's store holds %d group states BEFORE the join; a joiner that read the founder's state would make this whole file a tautology", held)
	}
	founderStates := len(a.store.groupStates)
	if founderStates == 0 {
		t.Fatal("A's store holds no group state after founding and committing, so the comparison below observes nothing")
	}

	joined, err := b.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("B's JoinFromWelcome: %v", err)
	}
	defer joined.Close()

	if held := len(b.store.groupStates); held != 1 {
		t.Errorf("B's store holds %d group states after the join, want exactly 1", held)
	}
	if held := len(a.store.groupStates); held != founderStates {
		t.Errorf("A's store held %d group states before B joined and %d after; B's join wrote into the founder's store",
			founderStates, held)
	}
}

// TestTheFounderHalfAnswersARealWelcomeThroughTheSeam is j1 task 6's third property, and the
// CONTROL is what keeps its first clause from passing on any non-empty byte slice.
//
// Nothing in this package had ever produced a Welcome before this file: the four GroupHandle.Commit
// sites in these tests all commit a one-member group with no proposals -- the only kind either
// group can make -- and every one of them discards welcome and ratchetTree into _.
//
// THE RELATED OVERLOAD THIS FILE DOES NOT RELY ON AND DOES NOT FIX: Commit with an empty non-nil
// vector commits nothing and silently answers an empty commit with a nil Welcome, and ProposeAdd
// answers the encoded proposal MESSAGE rather than a ref, so there is no way through this seam to
// obtain a value for the by-reference vector at all. This file uses the nil arm and says why.
func TestTheFounderHalfAnswersARealWelcomeThroughTheSeam(t *testing.T) {
	a := newTestEngine(t)
	b := newTestEngine(t)
	founder := a.createGroup(t, "a-real-welcome")
	defer founder.Close()

	// THE CONTROL FIRST: a commit with no pending proposal answers a NIL welcome, which is the
	// documented shape. Without it "the welcome is non-empty" is satisfied by any byte slice this
	// seam happens to answer.
	commit, welcome, ratchetTree, err := founder.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) with no pending proposal: %v", err)
	}
	if len(commit) == 0 {
		t.Error("a commit with no pending proposal answered no commit message")
	}
	if welcome != nil {
		t.Errorf("a commit with no pending proposal answered a %d octet welcome; a group of one admits nobody and there is nothing for a welcome to be addressed to",
			len(welcome))
	}
	if len(ratchetTree) == 0 {
		t.Error("a commit with no pending proposal answered no ratchet tree")
	}
	if err := founder.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}

	// and then the real one
	keyPackage, err := b.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("B's NewKeyPackage: %v", err)
	}
	if _, err := founder.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	commit, welcome, ratchetTree, err = founder.Commit(nil)
	if err != nil {
		t.Fatalf("Commit(nil) after a ProposeAdd: %v", err)
	}
	if len(commit) == 0 || len(welcome) == 0 || len(ratchetTree) == 0 {
		t.Fatalf("a commit carrying one add answered commit=%d welcome=%d ratchetTree=%d",
			len(commit), len(welcome), len(ratchetTree))
	}
	// and it is a Welcome rather than some octets: it parses, it carries a welcome arm, and the
	// arm is addressed to exactly one joiner
	parsed, err := mls.ParseMLSMessage(welcome)
	if err != nil {
		t.Fatalf("the welcome this seam answered does not parse: %v", err)
	}
	if parsed.Welcome == nil {
		t.Fatal("the message this seam answered carries no welcome arm")
	}
	if addressed := len(parsed.Welcome.Secrets); addressed != 1 {
		t.Errorf("the welcome is addressed to %d joiners, want 1", addressed)
	}
}

// ---------------------------------------------------------------------------
// j1 task 6's fourth property, and the class it is derived over
// ---------------------------------------------------------------------------
//
// THE FINDING THIS BLOCK ANSWERS, measured before it was written. The gate that shipped at 0c14aa0
// narrowed its class to sentences NAMING one of two identifiers -- TakeKeyPackage and the removed
// sentinel -- so a fresh production impossibility sentence naming neither survived. The mutant was
// run: two comment lines added above JoinFromWelcome's header, reading "A SECOND DEVICE CANNOT JOIN
// A GROUP THIS ENGINE FOUNDS. The adapter publishes no joiner material a founder could address, so
// a welcome join is not reachable from this package." The gate reported "production sentences
// asserting a join is impossible: 0" and the whole package stayed GREEN.
//
// IT IS THE SAME SHAPE AS THE ERASE OBLIGATION ONE FILE OVER -- a class that is a NAME rather than
// the property -- and mls/GATES.md is the register of the nine times this project has shipped it.
// That file's own conclusion is what this block is built to, and it is not "derive harder": every
// derivation bottoms out in a literal, so the two questions are whether the literal is at a level
// where being WRONG IS VISIBLE, and whether it FAILS CLOSED and PRINTS ITS COMPLEMENT.
//
// WHERE THIS ONE'S LITERALS ARE. The SUBJECT has none: the vocabulary is read off the package's own
// syntax tree, twice narrowed and both narrowings printed. The PREDICATE has one -- the phrase list
// below -- and it is held to the two questions by printing, on every run, every production sentence
// about the join that carries a NEGATION and matches no phrase. That is the set a new wording lands
// in, so being wrong is visible rather than silent.

// engineJoinImpossibilityPhrases is the PREDICATE, and it is this gate's one literal.
//
// It is not derived and cannot usefully be: "asserts that the join cannot happen" is a judgement
// about a sentence, and a negation adjacent to a join word is not it -- "Rejected: taking only
// after a successful join, which this interface cannot express" is a true sentence of exactly that
// shape, and a gate that failed on it would be red over correct prose on the day it shipped.
//
// What is done about that instead is the two questions: the complement is printed on every run,
// and the set of DECLARATIONS allowed to speak about the join in the negative is pinned, so a new
// place in the package making a new claim is red even when the claim is worded in a way this list
// has never seen.
var engineJoinImpossibilityPhrases = []string{
	"cannot join",
	"does not carry it",
	"cannot be written over",
	"cannot assemble",
	"does not publish the joiner",
	"is not reachable",
	"no second device",
	"cannot be joined",
	"a join is impossible",
	"no record crosses",
}

// engineJoinNegations are the negation tokens the COMPLEMENT is read over.
//
// They are not the class and nothing fails on them. They exist so the sentences the phrase list
// above does NOT match are printed rather than silent, which is the whole of what makes that
// literal visible.
var engineJoinNegations = []string{
	" no ", " not ", " never ", "cannot", "can not", "nothing", "nobody", "none of", " nor ",
	"n't", "impossible", "unable", "refuses", "unreachable",
}

// engineJoinDispositions is the PIN: the production declarations allowed to speak about the join in
// the negative, each with the reason its sentences are TRUE.
//
// WHY A PIN EXISTS BESIDE A PHRASE LIST. The phrase list can only catch a wording somebody has
// already seen; that is exactly how the gate at 0c14aa0 failed, and widening a list is how this
// project has failed nine times at nine altitudes (mls/GATES.md). The pin fails on a different
// axis: a declaration that starts speaking about the join in the negative is RED on the commit
// that adds it, whatever words it chooses. Neither half subsumes the other -- a false sentence
// added to an already-pinned declaration is the phrase list's job, and a false sentence in a
// fresh declaration is the pin's.
//
// AN ENTRY IS A JUDGEMENT AND CANNOT BE DERIVED. What is derived is the class it is enumerated
// over. Read what the gate prints before adding one: if the sentence is false, fix the sentence.
var engineJoinDispositions = map[string]string{
	"doc.go (file header)": "the inventory's four remaining absences, each true and each now measured: pq_secret's carrier (M1-2, m1 task 14), the Welcome as a one-process value (ledger 44a), the session neither side keeps across an add, and the anchoring nothing performs (MG-1)",

	"engine.go GroupEngine": "section 6's method list is four and the note that JoinFromWelcome is not on GroupHandle, plus MG-1's obligation -- which is a statement about what a WELCOME carries and about what this package deliberately does not do, not about whether a join happens",

	"engine.go NewKeyPackage": "the init key a key package publishes is what a Welcome is sealed to, so a leaf naming a key this device cannot sign with is one no Welcome could be opened with. True of a BROKEN key package and not of the join",

	"engine.go JoinFromWelcome": "where the ref comes from (section 6's signature carries none), why exactly one ref is taken and not the first or all, why the take is destructive and puts back, and what the store interface's bare error cannot express. Every one is a statement about a CONSTRAINT the join works within, and the body below them joins",

	"engine.go joinWithTakenKeyPackage": "the ownership rule: nothing refuses a device whose signing key was erased by its own join, and mls.JoinFromWelcome exits at some fifteen places so the erase is deferred. Both are reasons the join is written the way it is",

	"errors.go ErrEngineWelcomeShape": "the sentinel for octets that are not an MLSMessage carrying a Welcome. It is a refusal of a MALFORMED input and fires on nothing a real Welcome produces",

	"errors.go ErrEngineNoKeyPackageForWelcome": "the sentinel for a Welcome addressed to somebody else, and the note that StateStore.TakeKeyPackage answers a bare error so an operator needs the counts. A Welcome this device is not in is not a join that cannot happen",
}

// engineJoinPath is the JOIN PATH, read off this package's own syntax tree.
//
// The seed is the production declaration whose body calls mls.JoinFromWelcome -- found, never
// named -- and the path is that declaration, everything that calls it, every package declaration
// those bodies name, and the section 6 method the whole thing satisfies.
func engineJoinPath(t *testing.T) (names map[string]bool, onlyTheJoinCalls []string) {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	declared := map[string]bool{}
	bodies := map[string]*ast.FuncDecl{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				declared[typed.Name.Name] = true
				bodies[typed.Name.Name] = typed
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					switch named := spec.(type) {
					case *ast.ValueSpec:
						for _, one := range named.Names {
							declared[one.Name] = true
						}
					case *ast.TypeSpec:
						declared[named.Name.Name] = true
					}
				}
			}
		}
	}
	seed := ""
	for name, function := range bodies {
		if function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector || selector.Sel.Name != engineJoinSeamMethod {
				return true
			}
			if qualifier, isIdentifier := selector.X.(*ast.Ident); isIdentifier && qualifier.Name == "mls" {
				seed = name
			}
			return true
		})
	}
	if seed == "" {
		t.Fatal("no production declaration of this package calls mls.JoinFromWelcome, so this gate derived its class off nothing and would report clean having read nothing")
	}
	names = map[string]bool{seed: true, engineJoinSeamMethod: true}
	// the seam method is in the frontier as well as in the set. It was in the set only, once, and
	// the bug that produced is the one this whole block exists about: its body names
	// ErrEngineNoKeyPackageForWelcome, so leaving it unwalked dropped "welcome" out of the derived
	// vocabulary and left the class one word wide.
	frontier := []string{seed, engineJoinSeamMethod}
	for _, function := range bodies {
		if function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && identifier.Name == seed {
				if !names[function.Name.Name] {
					names[function.Name.Name] = true
					frontier = append(frontier, function.Name.Name)
				}
			}
			return true
		})
	}
	for at := 0; at < len(frontier); at += 1 {
		function := bodies[frontier[at]]
		if function == nil || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			identifier, isIdentifier := node.(*ast.Ident)
			if !isIdentifier || !declared[identifier.Name] || names[identifier.Name] {
				return true
			}
			names[identifier.Name] = true
			frontier = append(frontier, identifier.Name)
			return true
		})
	}
	delete(names, "_")
	// THE CALLS ONLY THE JOIN MAKES, derived the same way and for the same reason the vocabulary
	// below is narrowed twice: a method the whole package calls says nothing about which sentences
	// are about the join, and a method only this path calls says everything.
	callSites := map[string]int{}
	joinCalls := map[string]bool{}
	for name, function := range bodies {
		if function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			callSites[selector.Sel.Name] += 1
			if names[name] {
				joinCalls[selector.Sel.Name] = true
			}
			return true
		})
	}
	for name := range joinCalls {
		if callSites[name] == 1 {
			onlyTheJoinCalls = append(onlyTheJoinCalls, name)
		}
	}
	slices.Sort(onlyTheJoinCalls)
	return names, onlyTheJoinCalls
}

// engineJoinExclusiveNames narrows the join path to the declarations NOTHING OUTSIDE IT NAMES.
//
// The path is a reachability closure and reachability is not aboutness: Zeroize and
// connectMlsHandle are both on it and both are the whole package's, so a sentence naming either is
// not a sentence about the join. Measured -- without this clause the class admitted SenderRatchet's
// header and ErrEngineCryptoProvider's, neither of which has anything to do with a join.
//
// The seed and the seam method are kept whatever else names them: they ARE the join, and a
// package that grew a second caller of the seam would otherwise remove the subject from its own
// gate.
func engineJoinExclusiveNames(t *testing.T) []string {
	t.Helper()
	names, _ := engineJoinPath(t)
	_, sources := messagegroupProductionSources(t)
	namedFromOutside := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || names[function.Name.Name] {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.Ident:
					namedFromOutside[typed.Name] = true
				case *ast.SelectorExpr:
					namedFromOutside[typed.Sel.Name] = true
				}
				return true
			})
		}
	}
	exclusive := []string{}
	for name := range names {
		if namedFromOutside[name] {
			continue
		}
		exclusive = append(exclusive, name)
	}
	slices.Sort(exclusive)
	if len(exclusive) == 0 {
		t.Fatal("no declaration of the join path is named only from it, so this gate has no subject identifier at all")
	}
	return exclusive
}

// The section 6 method the adapter satisfies, spelled once. It is the ONE name this derivation
// writes down, and it is at a level where being wrong is visible: a wrong spelling finds no seed
// and engineJoinPath fatals rather than deriving a smaller class.
const engineJoinSeamMethod = "JoinFromWelcome"

// engineJoinVocabulary answers the words a sentence about the join is recognised by, and PRINTS
// what each of its two narrowings removed.
//
// NARROWING 1 -- shared with the rest of the package. A word in the join path's names that also
// spells some other declaration says nothing: "key", "package", "engine", "handle", "err" are the
// package's whole vocabulary and admit every sentence in it.
//
// NARROWING 2 -- RECURRENCE, and this is the clause that separates the subject from one helper's
// spelling. A word that names what the join IS appears in more than one of the path's declaration
// names; a word incidental to how one helper was spelled appears in exactly one. Measured: without
// it the vocabulary is {join, welcome, with, taken, shape} -- "with" alone carries the class from
// 54 sentences to 266 and the negated half from 17 to 91, which is a register nobody reads.
func engineJoinVocabulary(t *testing.T) (vocabulary []string, shared []string, onceOnly []string) {
	t.Helper()
	names, _ := engineJoinPath(t)
	_, sources := messagegroupProductionSources(t)
	occurrences := map[string]int{}
	for name := range names {
		seen := map[string]bool{}
		for _, word := range engineJoinWordsOf(name) {
			if !seen[word] {
				seen[word] = true
				occurrences[word] += 1
			}
		}
	}
	elsewhere := map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			record := func(name string) {
				if names[name] {
					return
				}
				for _, word := range engineJoinWordsOf(name) {
					elsewhere[word] = true
				}
			}
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				record(typed.Name.Name)
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					switch named := spec.(type) {
					case *ast.ValueSpec:
						for _, one := range named.Names {
							record(one.Name)
						}
					case *ast.TypeSpec:
						record(named.Name.Name)
					}
				}
			}
		}
	}
	for word, count := range occurrences {
		switch {
		case elsewhere[word]:
			shared = append(shared, word)
		case count < 2:
			onceOnly = append(onceOnly, word)
		default:
			vocabulary = append(vocabulary, word)
		}
	}
	slices.Sort(vocabulary)
	slices.Sort(shared)
	slices.Sort(onceOnly)
	return vocabulary, shared, onceOnly
}

// engineJoinWordsOf splits an identifier into its lower case words.
func engineJoinWordsOf(name string) []string {
	words := []string{}
	current := []rune{}
	runes := []rune(name)
	flush := func() {
		if 0 < len(current) {
			words = append(words, strings.ToLower(string(current)))
			current = nil
		}
	}
	for at, letter := range runes {
		if unicode.IsUpper(letter) && 0 < len(current) &&
			(!unicode.IsUpper(runes[at-1]) || (at+1 < len(runes) && unicode.IsLower(runes[at+1]))) {
			flush()
		}
		if !unicode.IsLetter(letter) {
			flush()
			continue
		}
		current = append(current, letter)
	}
	flush()
	return words
}

// engineJoinSentence is one production comment line or string literal, with the declaration it
// sits in. The declaration is what the disposition register is keyed by: a line number moves every
// time somebody adds a paragraph and a declaration name does not.
type engineJoinSentence struct {
	at    string
	where string
	text  string
}

// engineJoinSentencesOf reads every production comment line and string literal of this package.
//
// The reading is over the WHOLE package and not over the file the join happens to live in, which
// is the other half of the finding: a class scoped to one file is a scope narrowing, and
// mls/GATES.md's fourth instance is exactly that.
func engineJoinSentencesOf(t *testing.T) []engineJoinSentence {
	t.Helper()
	fileSet, sources := messagegroupProductionSources(t)
	sentences := []engineJoinSentence{}
	for _, source := range sources {
		type span struct {
			from token.Pos
			to   token.Pos
			name string
		}
		spans := []span{}
		for _, declaration := range source.parsed.Decls {
			from := declaration.Pos()
			name := ""
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				name = typed.Name.Name
				if typed.Doc != nil {
					from = typed.Doc.Pos()
				}
			case *ast.GenDecl:
				if typed.Doc != nil {
					from = typed.Doc.Pos()
				}
				// A var or const BLOCK is many declarations and this package writes its whole
				// sentinel table as one. Keying every sentinel's header by the block's first name
				// would collapse the register onto ErrEngineCryptoProvider and say that one
				// declaration speaks for thirty, so each spec gets its own span.
				if 1 < len(typed.Specs) {
					for at, spec := range typed.Specs {
						specFrom := spec.Pos()
						specName := ""
						switch named := spec.(type) {
						case *ast.ValueSpec:
							if named.Doc != nil {
								specFrom = named.Doc.Pos()
							}
							if 0 < len(named.Names) {
								specName = named.Names[0].Name
							}
						case *ast.TypeSpec:
							if named.Doc != nil {
								specFrom = named.Doc.Pos()
							}
							specName = named.Name.Name
						case *ast.ImportSpec:
							specName = "imports"
						}
						if specName == "" {
							specName = "spec"
						}
						if at == 0 {
							specFrom = from
						}
						spans = append(spans, span{from: specFrom, to: spec.End(), name: specName})
					}
					continue
				}
				for _, spec := range typed.Specs {
					switch named := spec.(type) {
					case *ast.ValueSpec:
						if 0 < len(named.Names) {
							name = named.Names[0].Name
						}
					case *ast.TypeSpec:
						name = named.Name.Name
					case *ast.ImportSpec:
						name = "imports"
					}
					if name != "" {
						break
					}
				}
			}
			if name == "" {
				name = "declaration"
			}
			spans = append(spans, span{from: from, to: declaration.End(), name: name})
		}
		whereOf := func(position token.Pos) string {
			for _, one := range spans {
				if one.from <= position && position <= one.to {
					return source.path + " " + one.name
				}
			}
			// the package doc and anything above the first declaration
			return source.path + " (file header)"
		}
		record := func(position token.Pos, text string) {
			sentences = append(sentences, engineJoinSentence{
				at:    source.path + ":" + strconv.Itoa(fileSet.Position(position).Line),
				where: whereOf(position),
				text:  text,
			})
		}
		for _, group := range source.parsed.Comments {
			for _, line := range group.List {
				record(line.Pos(), line.Text)
			}
		}
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			if literal, isLiteral := node.(*ast.BasicLit); isLiteral && literal.Kind == token.STRING {
				record(literal.Pos(), literal.Value)
			}
			return true
		})
	}
	if len(sentences) == 0 {
		t.Fatal("no production sentence of this package was read at all, so this gate would report clean having read nothing")
	}
	return sentences
}

// TestNoProductionSentenceOfThisPackageSaysAJoinIsImpossible is j1 task 6's fourth property, with
// its class derived from the property rather than from two identifiers.
//
// THE PROPERTY: no production sentence of this package asserts that the join cannot happen. It is
// not a claim about prose for its own sake -- the package's own code refutes such a sentence, and a
// reader who believes one re-derives the wrong fix and budgets for work already done.
//
// FOUR SETS ARE PRINTED ON EVERY RUN and only the first can fail on its own:
//
//   - the sentences that assert the join is impossible, which must be empty;
//   - what the SUBJECT narrowing removed: sentences carrying an impossibility phrase that are not
//     about the join at all;
//   - what the PREDICATE narrowing removed: sentences about the join carrying a NEGATION that no
//     phrase matched -- the set a new wording lands in;
//   - the executable call sites, because a package that deleted the join body would make every
//     sentence above false in the other direction.
//
// EVERY ONE OF THEM FAILS CLOSED ON EMPTY. An empty complement is this project's standing tell: it
// is what a gate that removed the SUBJECT rather than the claim looks like from the inside, and it
// reports clean having read nothing.
func TestNoProductionSentenceOfThisPackageSaysAJoinIsImpossible(t *testing.T) {
	names, onlyTheJoinCalls := engineJoinPath(t)
	vocabulary, shared, onceOnly := engineJoinVocabulary(t)
	t.Logf("the join path, derived: %d declaration(s) %v, and %d call(s) only it makes %v",
		len(names), slices.Sorted(maps.Keys(names)), len(onlyTheJoinCalls), onlyTheJoinCalls)
	t.Logf("the vocabulary, derived: %v -- narrowed by %d word(s) shared with the rest of the package %v and %d that spell exactly one declaration %v",
		vocabulary, len(shared), shared, len(onceOnly), onceOnly)
	if len(vocabulary) == 0 {
		t.Fatal("the derived vocabulary is empty, so no sentence is about the join and this gate judges nothing")
	}
	if len(shared) == 0 || len(onceOnly) == 0 {
		t.Error("one of the vocabulary's two narrowings removed nothing; a narrowing with an empty complement is the reading that cannot tell a correct derivation from one that read the wrong tree")
	}

	exclusive := engineJoinExclusiveNames(t)
	identifiers := append(slices.Clone(exclusive), onlyTheJoinCalls...)
	t.Logf("the subject identifiers, derived: %d named only from the join path %v, plus %d call(s) only it makes; %d path declaration(s) removed as the whole package's %v",
		len(exclusive), exclusive, len(onlyTheJoinCalls), len(names)-len(exclusive),
		engineJoinRemoved(slices.Sorted(maps.Keys(names)), exclusive))
	impossible := []string{}
	subjectRemoved := []string{}
	negatedButUnmatched := []string{}
	aboutTheJoin := 0
	total := 0
	speaking := map[string]bool{}
	for _, sentence := range engineJoinSentencesOf(t) {
		total += 1
		isAboutTheJoin := engineJoinIsAboutTheJoin(sentence.text, vocabulary, identifiers)
		assertsImpossibility := engineJoinAssertsImpossibility(sentence.text)
		switch {
		case isAboutTheJoin && assertsImpossibility:
			impossible = append(impossible, sentence.at+" "+strings.TrimSpace(sentence.text))
		case assertsImpossibility:
			subjectRemoved = append(subjectRemoved, sentence.at)
		case isAboutTheJoin && engineJoinCarriesANegation(sentence.text):
			negatedButUnmatched = append(negatedButUnmatched, sentence.at)
			speaking[sentence.where] = true
			// PRINTED ONE BY ONE and not merely counted. This is the set a new wording lands
			// in, and a count of it is a number rather than a query: a reader who has to decide
			// whether the phrase list missed something has to be able to read the sentences.
			t.Logf("  [%s] %s %s", sentence.where, sentence.at, strings.TrimSpace(sentence.text))
		}
		if isAboutTheJoin {
			aboutTheJoin += 1
		}
	}
	executable := engineJoinExecutableCallSites(t)

	t.Logf("production sentences of this package asserting the join is impossible: %d %v",
		len(impossible), impossible)
	t.Logf("the SUBJECT narrowing's complement, printed: %d of %d production sentence(s) are about the join, so the narrowing removed %d; %d sentence(s) carry an impossibility phrase and are not about the join %v",
		aboutTheJoin, total, total-aboutTheJoin, len(subjectRemoved), subjectRemoved)
	t.Logf("the PREDICATE narrowing's complement, printed: %d of the %d sentence(s) about the join carry a negation no phrase matched, in %d declaration(s) %v",
		len(negatedButUnmatched), aboutTheJoin, len(speaking), slices.Sorted(maps.Keys(speaking)))
	t.Logf("executable call site(s) of the join path: %d %v", len(executable), executable)

	if len(impossible) != 0 {
		t.Errorf("%d production statement(s) of this package say the join cannot happen: %v. It can and it does: this engine mints under its own signer, assembles the material a welcome join takes, and enginejoin_test.go drives two engines through one",
			len(impossible), impossible)
	}
	// fails closed, four ways, and each empty set is a different way of having read nothing
	if aboutTheJoin == 0 {
		t.Error("no production sentence of this package is about the join at all, so this gate cannot tell a package that removed the claim from one that removed the subject")
	}
	// THE SUBJECT NARROWING'S COMPLEMENT IS PRINTED AND IS NOT FAILED ON, and the distinction was
	// measured rather than assumed. It was a failure clause first, on this project's standing rule
	// that an empty complement is the dangerous reading -- and it went RED over correct source the
	// moment doc.go was corrected, because the phrase list here is DELIBERATELY join-specific and
	// the set "asserts an impossibility about something else" is expected to be small or empty.
	// The clause was testing the phrase list's breadth and calling it a class boundary.
	//
	// WHAT THAT CLAUSE WAS REACHING FOR is the danger below, and this is the check that actually
	// holds it: a vocabulary that admitted EVERY sentence would make every complement empty and
	// this gate universal, which is the "reported clean having read nothing" failure with the sign
	// flipped. So the assertion is that the subject narrowing REMOVES sentences.
	if aboutTheJoin >= total {
		t.Errorf("all %d production sentence(s) of this package are about the join, so the derived vocabulary admits everything and narrows nothing",
			total)
	}
	if len(negatedButUnmatched) == 0 {
		t.Error("the predicate narrowing removed nothing: no sentence about the join carries a negation the phrase list did not match, so the one literal in this gate is invisible and a new wording would be silent")
	}
	if len(executable) == 0 {
		t.Error("no production call site of the join path was found; the join body is what makes those sentences false, and without it this gate is green over a package that simply deleted its comments")
	}
	// THE PIN, and it is what a phrase list can never hold: a NEW place in this package that starts
	// speaking about the join in the negative is red on the commit that adds it, whatever words it
	// chooses. The disposition is a judgement and is enumerated; the class it is enumerated over is
	// derived above. mls/GATES.md's closing rule after nine rounds of this defect: a derived class
	// whose literal is invisible and silent is worth less than an enumerated one that refuses and
	// prints.
	for where := range speaking {
		if _, isDispositioned := engineJoinDispositions[where]; !isDispositioned {
			t.Errorf("%s carries a negated sentence about the join and no disposition says why it is true. Either the sentence is wrong -- this package joins, and a durable record crosses the join -- or it is right and engineJoinDispositions owes the reason",
				where)
		}
	}
	for where, reason := range engineJoinDispositions {
		if !speaking[where] {
			t.Errorf("engineJoinDispositions still disposes of %s (%q) and that declaration no longer carries a negated sentence about the join; a disposition that outlives its subject is an allow list nobody is reading",
				where, reason)
		}
	}
}

// engineJoinExecutableCallSites answers where this package's production source CALLS the join path.
func engineJoinExecutableCallSites(t *testing.T) []string {
	t.Helper()
	names, onlyTheJoinCalls := engineJoinPath(t)
	wanted := map[string]bool{}
	for _, name := range onlyTheJoinCalls {
		wanted[name] = true
	}
	for name := range names {
		wanted[name] = true
	}
	fileSet, sources := messagegroupProductionSources(t)
	found := []string{}
	for _, source := range sources {
		ast.Inspect(source.parsed, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			name := ""
			switch callee := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = callee.Sel.Name
			case *ast.Ident:
				name = callee.Name
			}
			if wanted[name] {
				found = append(found, source.path+":"+strconv.Itoa(fileSet.Position(call.Pos()).Line)+" "+name)
			}
			return true
		})
	}
	slices.Sort(found)
	return found
}

// engineJoinRemoved answers what a narrowing took out, which is the only form this project accepts
// a narrowing in: an empty complement is the dangerous reading.
func engineJoinRemoved(whole []string, kept []string) []string {
	held := map[string]bool{}
	for _, one := range kept {
		held[one] = true
	}
	removed := []string{}
	for _, one := range whole {
		if !held[one] {
			removed = append(removed, one)
		}
	}
	return removed
}

func engineJoinIsAboutTheJoin(text string, vocabulary []string, identifiers []string) bool {
	lowered := strings.ToLower(text)
	for _, word := range vocabulary {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	for _, name := range identifiers {
		if strings.Contains(text, name) {
			return true
		}
	}
	return false
}

func engineJoinAssertsImpossibility(text string) bool {
	lowered := strings.ToLower(text)
	for _, phrase := range engineJoinImpossibilityPhrases {
		if strings.Contains(lowered, phrase) {
			return true
		}
	}
	return false
}

func engineJoinCarriesANegation(text string) bool {
	lowered := strings.ToLower(text)
	for _, negation := range engineJoinNegations {
		if strings.Contains(lowered, negation) {
			return true
		}
	}
	return false
}

func engineJoinLineOf(fileSet *token.FileSet, node ast.Node) string {
	return strconv.Itoa(fileSet.Position(node.Pos()).Line)
}

// The four sentences j1 task 6's fifth property requires this file to carry, each keyed by a
// marker a gate can find.
//
// THREE OF THESE WERE WRONG AT 0c14aa0 AND TWO OF THEM WERE WRONG IN THE MODEST DIRECTION, which
// is the kind that costs the most: a sentence that understates what the code does makes the next
// planner budget for work that is already finished. The corrections are below, each beside the
// case that now holds it.
//
//   - engineJoinDoesNotEstablish1: a DURABLE record DOES cross between these two engines, and the
//     old sentence said it does not. What is genuinely absent is a DELIVERY CHANNEL for pq_secret,
//     which is a different claim: NewGroupSession needs a VALUE and not a channel, and every other
//     key on the seal/open path is derived by a production function out of material both engines
//     already hold. TestADurableRecordSealedByTheFounderOpensAtTheJoiner. S2-3, M1-20.
//   - engineJoinDoesNotEstablish2: NEITHER side keeps a session across the add, and the old
//     sentence named the joiner alone. TestBothSidesOfTheAddLoseTheirSessionAndNotOnlyTheJoiner.
//     The carrier is M1-2.
//   - engineJoinDoesNotEstablish3: the welcome here is handed over as a VALUE IN ONE PROCESS,
//     which is ledger 44a's named, gated, test-only hand-off and not a delivery channel. Unchanged
//     and still true.
//   - engineJoinDoesNotEstablish4: the welcome ANCHORS NOTHING, which was in no sentence at all.
//     TestAWelcomeFromAnAttackerJoinsAndTheOnlyThingItGetsWrongIsWhoTheGroupIs. Open item MG-1.
const engineJoinDoesNotEstablish1 = "a DURABLE record DOES cross between these two engines, every key on it derived by a production function; what does not exist is a delivery channel for pq_secret, whose value is handed over in one process (S2-3, M1-20)"

const engineJoinDoesNotEstablish2 = "NEITHER side keeps a session across the add: the founder's own handle moves to epoch 1 on MergePendingCommit and hits ErrEpochZeroHandleKeyMissing exactly as the joiner does. The founder held group_handle_key at epoch zero and can hand it back; the joiner never held it and the carrier is deferred (M1-2)"

const engineJoinDoesNotEstablish3 = "the welcome here is handed over as a value in one process, which is ledger 44a's named, gated, test-only hand-off and not a delivery channel"

const engineJoinDoesNotEstablish4 = "the welcome anchors nothing: a device holding a key package this device published can join it to a group of that device's own, and every value the handle answers agrees because the group is real (MG-1)"

// TestThisFileSaysWhatItDoesNotEstablish is j1 task 6's fifth property, held mechanically.
//
// A file that proves two clients share a group and does not say those three things is a file the
// next reader will cite as the milestone. CP3B IS NOT REACHED BY THIS TASK: three filed blockers
// stand after it and none of them is this plan's.
//
// It is a documentation property held by a gate for ledger 44a's own reason: an absence that is
// named is safe and an absence that looks like a placeholder is not.
func TestThisFileSaysWhatItDoesNotEstablish(t *testing.T) {
	source, err := os.ReadFile(engineJoinThisFile)
	if err != nil {
		t.Fatalf("read %s, which is the subject of this gate: %v", engineJoinThisFile, err)
	}
	text := string(source)
	// the gate reads the FILE and not the constants, because a constant this gate compared
	// against itself is a gate no deletion can make red
	// EVERY NEEDLE IS ASSEMBLED AND NOT WRITTEN, for the reason the one below it is: this gate
	// reads the file it is written in, so a literal needle is a needle that MATCHES ITSELF and the
	// gate stays green over a file whose three sentences have all been rewritten. Measured -- a
	// first version of this gate survived exactly that mutation.
	// EACH NEEDLE IS ASKED TWICE, of the FILE and of the CONSTANT, and the second half is here
	// because the first alone was measured passing over a mutant. engineJoinDoesNotEstablish2 was
	// rewritten back to its one-sided wording and this gate stayed GREEN: the needle still matched
	// the PARAGRAPH ABOVE the constant, which describes it. A gate satisfied by prose about a value
	// is not holding the value.
	//
	// Asking the constant is not the self-comparison this file warns about two paragraphs up. That
	// warning is about comparing a constant to itself; the needle below is assembled in this gate,
	// independently of the constant, so a constant rewritten to say something else fails.
	for _, owed := range []struct {
		what     string
		phrase   string
		constant string
	}{
		{what: "(1) a DURABLE record DOES cross, and what is absent is a delivery channel for pq_secret -- S2-3, M1-20",
			phrase: "a DURABLE record DOES " + "cross between these two engines", constant: engineJoinDoesNotEstablish1},
		{what: "(2) NEITHER side keeps a session across the add, and not the joiner alone -- M1-2",
			phrase: "NEITHER side keeps a " + "session across the add", constant: engineJoinDoesNotEstablish2},
		{what: "(3) the welcome is handed over as a value in one process -- ledger 44a",
			phrase: "handed over as a " + "value in one process", constant: engineJoinDoesNotEstablish3},
		{what: "(4) the welcome anchors nothing -- MG-1",
			phrase: "the welcome " + "anchors nothing", constant: engineJoinDoesNotEstablish4},
		{what: "and that this file does not DECLARE the milestone, whatever it establishes",
			phrase: "THIS FILE DOES NOT " + "DECLARE CP3B"},
	} {
		if strings.Count(text, owed.phrase) == 0 {
			t.Errorf("%s says %s nowhere. This file proves two clients share a group, and a file that does that without saying what it does not establish is the file the next reader cites as the milestone",
				engineJoinThisFile, owed.what)
		}
		if owed.constant == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(owed.constant), strings.ToLower(owed.phrase)) {
			t.Errorf("the CONSTANT for %s reads %q and does not carry %q. The file still says it in a paragraph, which is how this clause was measured passing over a constant rewritten to its old, one-sided wording",
				owed.what, owed.constant, owed.phrase)
		}
	}
	// AND THE ONE CLAUSE THAT REPLACED A REFUSAL. This gate used to forbid the file to name
	// NewGroupSession at all, on the reason that a session over a joined handle was impossible --
	// which was the same wrong sentence engineJoinDoesNotEstablish1 carried, enforced. The file
	// builds sessions now, and what it owes instead is the CONTROL that says the group_handle_key
	// those sessions are handed is the value a session derives for itself rather than a number
	// this file made up. Without that control every case built on the chain runs on an invented
	// key and proves nothing about the product.
	//
	// The needle is ASSEMBLED rather than written, because this gate reads the file it is written
	// in: a literal here would match itself and report the check as the defect.
	if strings.Count(text, "does not reproduce the "+"sender_handle a session derives for itself") == 0 {
		t.Errorf("%s hands its sessions a group_handle_key and nothing in it holds that value against the one installEpochOnLoop derives on its epoch zero arm. A fixture that invented thirty two octets of the right width would pass every case in this file",
			engineJoinThisFile)
	}
	t.Logf("what this file does NOT establish: (1) %s; (2) %s; (3) %s; (4) %s",
		engineJoinDoesNotEstablish1, engineJoinDoesNotEstablish2, engineJoinDoesNotEstablish3,
		engineJoinDoesNotEstablish4)
}

// The file this gate reads, spelled once. It is read off disk rather than embedded, so the gate
// reads what a reviewer will read.
const engineJoinThisFile = "enginejoin_test.go"

// ---------------------------------------------------------------------------
// what this file establishes that the plan did not claim, and what it still does not
// ---------------------------------------------------------------------------

// TestADurableRecordSealedByTheFounderOpensAtTheJoiner is the clause the honesty sentence used to
// deny, and it is here because that sentence was WRONG IN THE MODEST DIRECTION.
//
// doc.go and this file both said "no record crosses between these two engines, because a session
// refuses an empty pq_secret and there is no delivery channel for one". The first half of that is
// true about pq_secret and the conclusion drawn from it is false: a session does not need a
// DELIVERY channel to be constructed, it needs a VALUE, and every other key on the seal/open path
// is derived by a production function from material both engines already hold. An honesty sentence
// that understates is still wrong, and this one made the next planner budget for work that was
// already done.
//
// THE CHAIN, AND EVERY KEY ON IT IS DERIVED BY A PRODUCTION FUNCTION:
//
//   - group_handle_key is GroupHandleKey(StorageRoot(mls_secret[0], pq_secret)) -- two exported
//     functions of this package over the founder's own epoch-zero exporter. It is not invented and
//     it is not a constant; installEpochOnLoop derives the identical value on the epoch-zero arm,
//     which is what the first clause below asserts rather than assumes.
//   - storage_root[1], the three class keys, write_key, read_key, record_key[0] and its ladder,
//     sender_handle and both AEAD keys are derived INSIDE the two sessions, by the production
//     bodies, out of each engine's own epoch-one exporter.
//   - the two exporters agree because the two engines are in one real MLS group, which is
//     TestTwoEnginesShareOneGroupAndTheirExportersAgree's property.
//
// WHAT IS HANDED OVER IN ONE PROCESS, named rather than left for a reader to find, because the
// distinction is CP3b's own: pq_secret, whose value the test draws and whose delivery is M1-20 and
// m1 task 14; group_handle_key, whose value a PRODUCTION function computed and whose carrier is
// M1-2; and the Welcome, which is ledger 44a's named, gated, test-only hand-off. Three hand-offs,
// no test-only key SOURCE: nothing on this path mints a key some other way than the product would.
//
// THE PROPERTY IS THE ROUND TRIP AND THE OCTETS, not that no error came back. A record that sealed
// and opened to different plaintext is the failure this is about.
func TestADurableRecordSealedByTheFounderOpensAtTheJoiner(t *testing.T) {
	chain := newTwoEngineChain(t, "a-durable-record-crosses")
	defer chain.close()

	head := []byte("head the founder wrote")
	body := []byte("the first durable record either of these devices has ever exchanged")
	record, err := chain.founderSession.SealRecord(message.RetentionDurable, 0, false, head, body, 0, nil)
	if err != nil {
		t.Fatalf("the founder's SealRecord: %v", err)
	}
	if record == nil {
		t.Fatal("the founder's SealRecord answered no record and no error")
	}
	if err := chain.joinerSession.TrackSender(chain.founder.OwnLeafIndex(), message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("the joiner's TrackSender over the founder's leaf: %v", err)
	}
	gotHead, gotBody, err := chain.joinerSession.OpenRecord(record)
	if err != nil {
		t.Fatalf("the joiner could not open the founder's record: %v", err)
	}
	t.Logf("a DURABLE record crossed the join: head %d octets, body %d octets, sealed at leaf %d and opened at leaf %d",
		len(gotHead), len(gotBody), chain.founder.OwnLeafIndex(), chain.joined.OwnLeafIndex())
	if !bytes.Equal(gotHead, head) {
		t.Errorf("the head opened as %q and was sealed as %q", gotHead, head)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("the body opened as %q and was sealed as %q", gotBody, body)
	}

	// AND IT GOES THE OTHER WAY, which is the half that says the joiner is a member rather than a
	// reader: the device that joined from a Welcome seals under its OWN leaf and the founder opens
	// it.
	back := []byte("and the joiner answers")
	answer, err := chain.joinerSession.SealRecord(message.RetentionDurable, 0, false, head, back, 0, nil)
	if err != nil {
		t.Fatalf("the joiner's SealRecord: %v", err)
	}
	if err := chain.founderSession.TrackSender(chain.joined.OwnLeafIndex(), message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("the founder's TrackSender over the joiner's leaf: %v", err)
	}
	_, gotBack, err := chain.founderSession.OpenRecord(answer)
	if err != nil {
		t.Fatalf("the founder could not open the joiner's record: %v", err)
	}
	if !bytes.Equal(gotBack, back) {
		t.Errorf("the joiner's body opened as %q and was sealed as %q", gotBack, back)
	}
}

// TestBothSidesOfTheAddLoseTheirSessionAndNotOnlyTheJoiner is finding 4, and it is the sentence
// this file's framing string used to get one-sided.
//
// engineJoinDoesNotEstablish2 named the JOINER and only the joiner. The founder's own handle moves
// to epoch 1 the moment MergePendingCommit returns, and a session constructed over it with no
// epoch-zero group handle key hits the SAME refusal, for the same reason, in the same line of
// installEpochOnLoop. A reader who took the one-sided sentence at face value would look for a
// carrier that delivers a key TO A JOINER, when what is missing is a session that SURVIVES A
// COMMIT on either side.
//
// The refusal is correct and this case does not argue with it: recomputing group_handle_key from
// the current epoch's root would give every epoch a different sender_handle and end every member's
// stream at every commit. What the case holds is the SYMMETRY, so the framing string cannot go
// one-sided again without failing.
func TestBothSidesOfTheAddLoseTheirSessionAndNotOnlyTheJoiner(t *testing.T) {
	chain := newTwoEngineChain(t, "both-sides-lose-the-session")
	defer chain.close()

	for _, side := range []struct {
		who    string
		handle GroupHandle
	}{
		{who: "the FOUNDER, whose own commit moved it", handle: chain.founder},
		{who: "the JOINER, which was never at epoch zero", handle: chain.joined},
	} {
		if epoch := side.handle.Epoch(); epoch == 0 {
			t.Fatalf("%s is at epoch %d, so this case observes the branch that is not the subject",
				side.who, epoch)
		}
		refused, err := NewGroupSession(side.handle, testPqSecret(), nil, newStreamIndexMemory(),
			testClock(), testServerNonce())
		if refused != nil {
			defer refused.Close()
			t.Errorf("%s got a session at epoch %d with no epoch zero group handle key", side.who, side.handle.Epoch())
		}
		if !errorIs(err, ErrEpochZeroHandleKeyMissing) {
			t.Errorf("%s answered %v, want ErrEpochZeroHandleKeyMissing", side.who, err)
		}
		t.Logf("%s: a session at epoch %d with no epoch zero group handle key is refused -- %v",
			side.who, side.handle.Epoch(), err)
	}

	// and the ASYMMETRY that is real, so this case does not overcorrect: the founder HELD the value
	// at epoch zero and can hand it back, and the joiner never held it and has no production route
	// to one. That is M1-2 and it is the carrier, not the refusal.
	if _, err := NewGroupSession(chain.founder, testPqSecret(), chain.groupHandleKey,
		newStreamIndexMemory(), testClock(), testServerNonce()); err != nil {
		t.Errorf("the founder's own epoch zero group handle key was refused at epoch 1: %v", err)
	}
}

// TestAWelcomeFromAnAttackerJoinsAndTheOnlyThingItGetsWrongIsWhoTheGroupIs is open item MG-1's
// reproduction, and it is the case that fails the day the behaviour changes.
//
// A WELCOME AUTHENTICATES NOBODY. The attacker below has never met the victim and holds exactly one
// thing: a key package the victim PUBLISHED, which in a running system has gone to the delivery
// service and to every member of every group that ever added that device. With it the attacker
// founds a group of its own, adds the victim and hands over the Welcome, and the victim's
// JoinFromWelcome succeeds.
//
// THE POINT IS NOT THAT SOMETHING FAILED. Nothing fails, and nothing should: the group is real, the
// attacker founded it, and every value the victim can read off the handle is a true value about
// that group. Group id, epoch, member count and the exporter all agree with the attacker's,
// because there is nothing for them to disagree about. What is false is the only thing no octet on
// this path carries -- that it is the group the user meant to be in.
//
// WHAT A CALLER WOULD HAVE TO DO ABOUT IT IS SHOWN AND NOT PERFORMED. The last clause reads the
// membership the way an anchoring caller would, and finds an identity the victim has never seen.
// That is the check this package hands its caller and does not make; which identity to expect and
// what to do when it is absent are open item MG-1.
func TestAWelcomeFromAnAttackerJoinsAndTheOnlyThingItGetsWrongIsWhoTheGroupIs(t *testing.T) {
	victim := newTestEngine(t)
	attacker := newTestEngine(t)
	stranger := newTestEngine(t)

	// the ONE thing the attacker holds, and it is a PUBLISHED value
	published, err := victim.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("the victim's NewKeyPackage: %v", err)
	}

	hostile := attacker.createGroup(t, "a-group-the-victim-never-asked-for")
	defer hostile.Close()
	if _, err := hostile.ProposeAdd(published); err != nil {
		t.Fatalf("the attacker's ProposeAdd over the victim's published key package: %v", err)
	}
	_, welcome, ratchetTree, err := hostile.Commit(nil)
	if err != nil {
		t.Fatalf("the attacker's Commit: %v", err)
	}
	if err := hostile.MergePendingCommit(); err != nil {
		t.Fatalf("the attacker's MergePendingCommit: %v", err)
	}

	joined, err := victim.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("MG-1's reproduction depends on this join SUCCEEDING and it answered %v. If an anchoring mechanism has landed, this case is what says so: rewrite it around the new refusal and close MG-1 in OPENITEMS.md",
			err)
	}
	defer joined.Close()

	// every observable agrees, because the group is real
	if !bytes.Equal(joined.GroupId(), hostile.GroupId()) {
		t.Errorf("the victim is in group %x and the attacker is in %x", joined.GroupId(), hostile.GroupId())
	}
	if joined.Epoch() != hostile.Epoch() {
		t.Errorf("the victim is at epoch %d and the attacker is at %d", joined.Epoch(), hostile.Epoch())
	}
	if count := joined.MemberCount(); count != 2 {
		t.Errorf("the victim sees %d members, want 2", count)
	}
	victimSecret, err := joined.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the victim's Export: %v", err)
	}
	attackerSecret, err := hostile.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the attacker's Export: %v", err)
	}
	if !bytes.Equal(victimSecret, attackerSecret) {
		t.Errorf("the victim and the attacker export different secrets, so this is not one group and the reproduction observes something else")
	}
	t.Logf("MG-1 reproduced: a welcome built by a device the victim has never met joined it to a group of %d at epoch %d, and the two exporters agree",
		joined.MemberCount(), joined.Epoch())

	// THE CHECK THIS PACKAGE HANDS ITS CALLER AND DOES NOT MAKE.
	held := [][]byte{}
	for at := 0; at < joined.MemberCount(); at += 1 {
		_, identityPub, _, err := joined.MemberAt(at)
		if err != nil {
			t.Fatalf("the victim's MemberAt(%d): %v", at, err)
		}
		held = append(held, identityPub)
	}
	carries := func(identityPub []byte) bool {
		for _, one := range held {
			if bytes.Equal(one, identityPub) {
				return true
			}
		}
		return false
	}
	if !carries(attacker.identityPub) {
		t.Error("the group the victim joined does not carry the attacker's identity, so the membership read below is not reading what built this welcome")
	}
	if carries(stranger.identityPub) {
		t.Error("the group carries an identity of a device that had nothing to do with it, so this clause distinguishes nothing")
	}
	t.Logf("and the anchor a caller would read is right there and is WRONG: the membership carries %x, which the victim has never seen. MG-1 is which identity it should have expected instead",
		attacker.identityPub)
}

// ---------------------------------------------------------------------------
// the two-engine chain, built once
// ---------------------------------------------------------------------------

// twoEngineChain is A and B in one real group at epoch one, each with its own session.
//
// It is a fixture rather than three copies of the same twenty lines, and the group_handle_key it
// carries is derived ONCE, at epoch zero, through the same two exported functions installEpochOnLoop
// uses -- which is what makes it a production value that was handed over rather than a test value
// that was invented.
type twoEngineChain struct {
	a, b                          *testEngine
	founder, joined               GroupHandle
	founderSession, joinerSession *GroupSession
	groupHandleKey                []byte
	pqSecret                      []byte
}

func newTwoEngineChain(t *testing.T, name string) *twoEngineChain {
	t.Helper()
	return newTwoEngineChainAtClock(t, name, testClock())
}

// newTwoEngineChainAtClock is newTwoEngineChain with the injected clock supplied by the caller.
//
// IT IS NOT A TIMING SENSITIVE SEAM AND MUST NOT BECOME ONE, which is newTestSessionAtClock's own
// paragraph and its own reason: a caller handing in a closure over a variable it sets itself is
// fully deterministic, and what it buys is the one property a fixed clock cannot observe -- that
// an opener takes the record's own eph_window off the wire and never recomputes one. BOTH sessions
// take the same clock, so a case that wants the two ends in different windows moves the shared
// variable between the seal and the open.
func newTwoEngineChainAtClock(t *testing.T, name string, nowMs func() int64) *twoEngineChain {
	t.Helper()
	a := newTestEngine(t)
	b := newTestEngine(t)
	keyPackage, err := b.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("B's NewKeyPackage: %v", err)
	}
	founder := a.createGroup(t, name)

	// AT EPOCH ZERO AND NOWHERE ELSE. group_handle_key is the epoch zero storage root's expansion
	// and it never moves; a value recomputed from a later root would give every epoch a different
	// sender_handle. Both functions are exported production functions of this package.
	if epoch := founder.Epoch(); epoch != 0 {
		t.Fatalf("the founder is at epoch %d before its first commit", epoch)
	}
	mlsSecret, err := founder.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the founder's epoch zero Export: %v", err)
	}
	pqSecret := testPqSecret()
	groupHandleKey := GroupHandleKey(StorageRoot(mlsSecret, pqSecret))

	if _, err := founder.ProposeAdd(keyPackage); err != nil {
		t.Fatalf("A's ProposeAdd: %v", err)
	}
	_, welcome, ratchetTree, err := founder.Commit(nil)
	if err != nil {
		t.Fatalf("A's Commit(nil): %v", err)
	}
	if err := founder.MergePendingCommit(); err != nil {
		t.Fatalf("A's MergePendingCommit: %v", err)
	}
	joined, err := b.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("B's JoinFromWelcome: %v", err)
	}
	if founder.Epoch() != joined.Epoch() {
		t.Fatalf("A is at epoch %d and B is at %d", founder.Epoch(), joined.Epoch())
	}

	// THREE CONTROLS, AND THE FIRST VERSION OF THIS FIXTURE HAD ONLY THE FIRST -- which was
	// measured surviving the mutation it exists to catch. `groupHandleKey` was rewritten as
	// `make([]byte, 32)`, thirty two zero octets, and every case built on this chain stayed GREEN:
	// the DURABLE round trip passes over ANY value both sides agree on, so the round trip is no
	// control at all, and clause (a) below was asserting that the FORMULA is right while never
	// looking at the VALUE this fixture actually hands over.
	//
	// (a) THE FORMULA IS THE PRODUCTION ONE. A session founded at epoch zero derives
	// group_handle_key itself, on installEpochOnLoop's one expansion arm, and SenderHandle is the
	// only door that value is observable through. Held over a CONTROL group, because a session
	// opened over the chain's own founder would close that handle when it closed.
	derived := a.createGroup(t, name+"-control")
	defer derived.Close()
	control, err := NewGroupSession(derived, pqSecret, nil, newStreamIndexMemory(), nowMs,
		testServerNonce())
	if err != nil {
		t.Fatalf("the control session at epoch zero: %v", err)
	}
	defer control.Close()
	controlSecret, err := derived.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the control group's Export: %v", err)
	}
	wanted, err := control.SenderHandle()
	if err != nil {
		t.Fatalf("the control session's SenderHandle: %v", err)
	}
	if got := SenderHandle(GroupHandleKey(StorageRoot(controlSecret, pqSecret)),
		derived.OwnLeafIndex()); got != wanted {
		t.Fatalf("GroupHandleKey(StorageRoot(mls_secret, pq_secret)) does not reproduce the sender_handle a session derives for itself: %x against %x. The value this fixture hands over is not the production one",
			got, wanted)
	}

	// (b) THE VALUE HANDED OVER IS THAT FORMULA APPLIED TO THIS CHAIN'S OWN EPOCH ZERO EXPORTER,
	// which is the clause the mutation walked through. It is not circular: it holds the VARIABLE
	// against the derivation, and a fixture that substituted anything -- zeros, a draw, a constant
	// of the right width -- fails here while every round trip built on it goes on passing.
	if recomputed := GroupHandleKey(StorageRoot(mlsSecret, pqSecret)); !bytes.Equal(groupHandleKey, recomputed) {
		t.Fatalf("this chain hands its sessions %x and the epoch zero derivation answers %x. A DURABLE record crosses under ANY value both sides agree on, so the round trip is not a control and this clause is",
			groupHandleKey, recomputed)
	}

	// (c) AND IT IS THE EPOCH ZERO ONE. group_handle_key never moves: a value re-expanded from the
	// current epoch's root would give every epoch a different sender_handle and end every member's
	// stream at every commit. Both sides would still agree on it, and every case here would still
	// pass, which is exactly why it is asserted rather than assumed.
	afterSecret, err := founder.Export(engineJoinExporterLabel, nil, engineJoinExporterLength)
	if err != nil {
		t.Fatalf("the founder's epoch one Export: %v", err)
	}
	if bytes.Equal(mlsSecret, afterSecret) {
		t.Fatal("the founder exports the same mls_secret at epoch 0 and epoch 1, so clause (c) cannot tell the two apart and this fixture is not observing an epoch change at all")
	}
	if moved := GroupHandleKey(StorageRoot(afterSecret, pqSecret)); bytes.Equal(groupHandleKey, moved) {
		t.Fatalf("the group_handle_key this chain hands over is the one derived from the CURRENT epoch's root, not from epoch zero's. It must not move: %x", moved)
	}

	founderSession, err := NewGroupSession(founder, pqSecret, groupHandleKey, newStreamIndexMemory(),
		nowMs, testServerNonce())
	if err != nil {
		t.Fatalf("the founder's session at epoch 1: %v", err)
	}
	joinerSession, err := NewGroupSession(joined, pqSecret, groupHandleKey, newStreamIndexMemory(),
		nowMs, testServerNonce())
	if err != nil {
		t.Fatalf("the joiner's session at epoch 1: %v", err)
	}
	return &twoEngineChain{
		a: a, b: b, founder: founder, joined: joined,
		founderSession: founderSession, joinerSession: joinerSession,
		groupHandleKey: groupHandleKey, pqSecret: pqSecret,
	}
}

func (self *twoEngineChain) close() {
	self.founderSession.Close()
	self.joinerSession.Close()
	self.joined.Close()
	self.founder.Close()
}

// ---------------------------------------------------------------------------
// the inventory, held against what the package proves
// ---------------------------------------------------------------------------

// engineJoinInventoryClaim is one thing this package PROVES, the case that proves it, the sentence
// doc.go owes about it, and the wordings that would deny it.
//
// WHY THIS EXISTS SEPARATELY FROM THE IMPOSSIBILITY GATE ABOVE, and it is a measurement rather than
// a preference. The false sentence this whole round is about --  "no record crosses between those
// two engines" -- was put back into doc.go as a mutation after that gate was rewritten, and the
// gate stayed GREEN. It was right to: the sentence names neither the join nor a welcome, so the
// derived join vocabulary does not admit it, and it landed in that gate's printed complement
// exactly as it should have. A class derived for one property does not hold a different property,
// and widening the join vocabulary until it swallowed this one is how a class stops meaning
// anything.
//
// So the class here is the OTHER one: the claims this package has a case for. It is enumerated,
// because "what this package proves" is a judgement; every entry NAMES the case, and the case is
// required to exist, so an entry cannot be asserted without a test behind it.
type engineJoinInventoryClaim struct {
	// what the package proves, in a sentence.
	proves string
	// the case that proves it. It must exist in this package's test source or the entry fails.
	heldBy string
	// the sentence doc.go owes, assembled rather than written: this gate reads production source
	// and a literal needle in a test file cannot match itself, but the halves are kept for the
	// same discipline the framing gate uses one file up.
	owes []string
	// wordings that DENY it. Any of these in production source is the defect this gate exists for.
	denials []string
}

var engineJoinInventoryClaims = []engineJoinInventoryClaim{
	{
		proves: "a DURABLE record sealed by the founder opens at the joiner, and back again",
		heldBy: "TestADurableRecordSealedByTheFounderOpensAtTheJoiner",
		owes:   []string{"DURABLE RECORD " + "CROSSES BETWEEN THEM"},
		denials: []string{
			"no record " + "crosses between", "no record " + "can cross",
			"record crosses " + "between those two engines", "records cross " + "between them",
		},
	},
	{
		proves: "neither side keeps a session across an add, and not the joiner alone",
		heldBy: "TestBothSidesOfTheAddLoseTheirSessionAndNotOnlyTheJoiner",
		owes:   []string{"NEITHER SIDE KEEPS A " + "SESSION ACROSS AN ADD"},
		denials: []string{
			"the joiner cannot compute a " + "sender_handle, because",
			"only the joiner " + "loses", "the joiner alone " + "loses",
		},
	},
	{
		proves: "a welcome built by a device holding a published key package joins this device to a group of that device's own",
		heldBy: "TestAWelcomeFromAnAttackerJoinsAndTheOnlyThingItGetsWrongIsWhoTheGroupIs",
		owes:   []string{"THE WELCOME " + "ANCHORS NOTHING"},
		denials: []string{
			"the welcome " + "authenticates the", "the welcome is " + "anchored",
			"this package " + "anchors the welcome",
		},
	},
	{
		proves: "pq_secret is a key value on the seal and open path and has no production driver",
		heldBy: "TestNoProductionDeclarationOfThisPackageDrawsAPqSecret",
		owes:   []string{"it is a KEY on the seal and " + "open path"},
		denials: []string{
			"none of the four " + "is a KEY", "pq_secret is " + "not a key",
		},
	},
	{
		// THE CLAIM MASTER SECTION 8.4's RULING OF 2026-09-15 CREATED, and the one whose
		// denial stood in doc.go as a true sentence for as long as it was true. It is the
		// sharpest case for this gate existing: the paragraph was honest, carefully argued
		// and correct, and a ruling made it false in one commit. The denials below are the
		// shapes it took, so a rewrite that restored any of them is red rather than stale.
		proves: "a member cannot forge a message from another member: the leaf that signed the body's MLS frame must be the leaf the record's sender_handle names",
		heldBy: "TestOneMemberCannotForgeAMessageFromAnother",
		owes:   []string{"A MEMBER CANNOT FORGE A " + "MESSAGE FROM ANOTHER MEMBER"},
		denials: []string{
			"no sender " + "authentication at all",
			"attributed to any other " + "leaf and it opens",
			"opens cleanly at " + "every other member",
			"the record layer has " + "no sender authentication",
		},
	},
	{
		// the claim ledger item 152's ruling of 2026-09-13 created, and the one whose
		// DENIAL was sitting in doc.go unchallenged until this commit. The seal lift is
		// exactly the kind of widening an inventory sentence goes stale under: the code
		// starts doing more, and the paragraph that says what it does not do is the last
		// thing anybody edits. The denials below are the shapes the retracted sentence
		// took, normalised the way this gate normalises, so a rewrite that reintroduced
		// any of them is red rather than merely wrong.
		proves: "this package seals and opens every retention class the wire admits, the six eph buckets included",
		heldBy: "TestEverySealableClassRoundTripsAndTheWrapItemOneEightyFiveRefusesDoesNot",
		owes:   []string{"IT SEALS AND OPENS " + "EVERY RETENTION CLASS THE WIRE ADMITS"},
		denials: []string{
			"it seals only the " + "durable", "only the durable " + "retention class is sealed",
			"the permanent, media " + "and eph classes are refused",
			"seals the durable class " + "alone",
		},
	},
}

// TestTheInventoryDoesNotDenyWhatThisPackageProves is finding 3's property, and it is the reason an
// honesty sentence is not finished by being corrected.
//
// A sentence nothing holds is a sentence the next commit can unwrite for free, and this one had
// already been wrong once. Three clauses per claim, and each catches a different regression:
//
//   - the CASE must exist, so an entry cannot claim a proof that was deleted;
//   - the SENTENCE must be present in production source, so a rewrite of the inventory that drops
//     it is red rather than silent;
//   - no production sentence may DENY it, which is the mutation that was measured surviving.
func TestTheInventoryDoesNotDenyWhatThisPackageProves(t *testing.T) {
	cases := engineJoinTestFunctionNames(t)
	prose := engineJoinProductionProse(t)
	if len(engineJoinInventoryClaims) == 0 {
		t.Fatal("the inventory claims nothing, so this gate reports clean having read nothing")
	}
	if len(prose) == 0 {
		t.Fatal("no production prose was read, so every claim below is held against nothing")
	}
	for _, claim := range engineJoinInventoryClaims {
		if !cases[claim.heldBy] {
			t.Errorf("the inventory claims %q and names %s as the case that proves it, and this package declares no such test. An inventory entry without a case is the sentence this gate exists to prevent",
				claim.proves, claim.heldBy)
		}
		for _, owed := range claim.owes {
			found := ""
			for path, text := range prose {
				if strings.Contains(text, engineJoinNormalise(owed)) {
					found = path
					break
				}
			}
			if found == "" {
				t.Errorf("no production prose of this package says %q, and %s proves it. The inventory owes what the code does, not only what it does not",
					owed, claim.heldBy)
			}
		}
		for _, denial := range claim.denials {
			for path, text := range prose {
				if strings.Contains(text, engineJoinNormalise(denial)) {
					t.Errorf("%s says %q, and %s proves the opposite: %s. A sentence that understates what the code does is still wrong, and it is the kind that makes the next planner budget for work already finished",
						path, denial, claim.heldBy, claim.proves)
				}
			}
		}
	}
	if t.Failed() {
		return
	}
	t.Logf("%d inventory claim(s) held over %d production file(s), each by a named case, each with its sentence present and no production prose denying it",
		len(engineJoinInventoryClaims), len(prose))
}

// engineJoinProductionProse answers each production file's comment text as ONE normalised string.
//
// PER FILE AND NOT PER LINE, which is not a convenience. This package wraps its prose at a hundred
// columns, so a claim worth making is almost always split across two comment lines and a per-line
// reading finds none of them -- measured: the first version of the gate above reported that no
// production sentence says a sentence doc.go carries in full, because the needle straddled a
// newline. A gate that cannot see the sentence it is about is the empty-complement failure wearing
// different clothes.
//
// Normalisation is lower case, comment markers removed, and every run of whitespace collapsed to
// one space, so a needle matches however the paragraph happens to be wrapped today.
func engineJoinProductionProse(t *testing.T) map[string]string {
	t.Helper()
	_, sources := messagegroupProductionSources(t)
	prose := map[string]string{}
	for _, source := range sources {
		joined := []string{}
		for _, group := range source.parsed.Comments {
			for _, line := range group.List {
				joined = append(joined, line.Text)
			}
		}
		prose[source.path] = engineJoinNormalise(strings.Join(joined, " "))
	}
	return prose
}

// engineJoinNormalise lower cases, drops comment markers and collapses whitespace.
func engineJoinNormalise(text string) string {
	lowered := strings.ToLower(text)
	for _, marker := range []string{"//", "/*", "*/"} {
		lowered = strings.ReplaceAll(lowered, marker, " ")
	}
	return strings.Join(strings.Fields(lowered), " ")
}

// TestNoProductionDeclarationOfThisPackageDrawsAPqSecret is finding 5, recorded rather than closed.
//
// pq_secret is the one test-only KEY VALUE on the seal and open path: StorageRoot takes it as the
// ikm of every storage_root a session extracts, so it is key material by the only definition that
// matters. What makes it different from every other key on that path is not that a test supplies
// it -- NewPqSecret is a production function and is what draws the real one, so this is not a
// test-only key SOURCE -- but that NOTHING IN PRODUCTION CALLS IT. There is no driver.
//
// DO NOT BUILD ONE HERE. Its delivery channel is m1 task 14, whose blocker ledger item 152 was
// RULED 2026-09-13 and whose remaining blocker is M1-52, which is an owner ruling and is not this
// package's. This case records the absence so it stays visible, and it goes RED the
// day a production driver lands -- at which point the right move is to delete this case and say
// where the value comes from, not to widen it.
func TestNoProductionDeclarationOfThisPackageDrawsAPqSecret(t *testing.T) {
	fileSet, sources := messagegroupProductionSources(t)
	drivers := []string{}
	declared := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction {
				continue
			}
			if function.Name.Name == engineJoinPqSecretSampler {
				declared = true
			}
			if function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				callee, isIdentifier := call.Fun.(*ast.Ident)
				if isIdentifier && callee.Name == engineJoinPqSecretSampler {
					drivers = append(drivers, source.path+":"+
						strconv.Itoa(fileSet.Position(call.Pos()).Line)+" in "+function.Name.Name)
				}
				return true
			})
		}
	}
	if !declared {
		t.Fatalf("this package declares no %s, so this gate read nothing: the sampler it is about is gone and the sentence in doc.go is about a function that does not exist",
			engineJoinPqSecretSampler)
	}
	t.Logf("production call site(s) of %s: %d %v -- the one test-only key VALUE on the seal and open path has no production driver, and its delivery is m1 task 14, whose ledger item 152 was ruled 2026-09-13 and whose remaining blocker is M1-52",
		engineJoinPqSecretSampler, len(drivers), drivers)
	if len(drivers) != 0 {
		t.Errorf("%s is now called from production at %v. That is not a failure of this package -- it is the carrier landing -- and what it means is that this case and doc.go's hand-carried paragraph are both stale. Say where the value comes from and delete this case",
			engineJoinPqSecretSampler, drivers)
	}
}

// The sampler, spelled once. A wrong spelling finds no declaration and the gate fatals rather than
// reporting a clean absence, which is the reading that would be worth nothing.
const engineJoinPqSecretSampler = "NewPqSecret"

// engineJoinTestFunctionNames answers every Test function this package declares.
func engineJoinTestFunctionNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	names := map[string]bool{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range parsed.Decls {
			if function, isFunction := declaration.(*ast.FuncDecl); isFunction {
				names[function.Name.Name] = true
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no test declaration was read out of this package, so every claim below is held against nothing")
	}
	return names
}

// TestTheOpenItemThisPackageCitesIsFiledWhereItSaysItIs holds the citation MG-1 rests on.
//
// engine.go states an obligation at GroupEngine and hands the MECHANISM to a numbered item. A
// number that names nothing is worse than no number: it reads as though somebody decided
// something. This gate is what keeps the citation from becoming one.
//
// WHY THE ITEM IS IN THIS DIRECTORY AND NOT IN THE SPEC LEDGER, which is where every other number
// this package cites lives -- M1-4, M1-15, M1-20, S2-3, J1-1, and M1-6, which was ruled
// 2026-09-07 and reversed 2026-09-13. It was raised in a commit
// to THIS repository, and the ledger is in another one; a number minted here against that register
// would collide with whatever it assigns next. So the row is filed where the code that cites it
// lives, it says in its own text that a ledger number is owed, and it migrates when one exists.
//
// The reading normalises line endings. .gitattributes pins *.go to LF and does not pin *.md, so a
// checkout on a box with autocrlf=true writes this file with CRLF -- and an anchor that matches on
// the text of a file using the other ending matches nothing at all, which is this tree's most
// expensive failure mode and has already cost it 84 anchors once.
func TestTheOpenItemThisPackageCitesIsFiledWhereItSaysItIs(t *testing.T) {
	source, err := os.ReadFile(engineJoinOpenItemsFile)
	if err != nil {
		t.Fatalf("read %s, which engine.go cites for the mechanism MG-1 files: %v. A citation to a document that is not there reads as a decision somebody took",
			engineJoinOpenItemsFile, err)
	}
	register := strings.ReplaceAll(string(source), "\r\n", "\n")

	// THE ROWS, DERIVED OFF THE FILE'S OWN HEADINGS rather than listed here. A row added to the
	// register and cited from nowhere, and a number cited from production source with no row,
	// are the two halves of the same defect and this reads both off the subject.
	filed := []string{}
	for _, line := range strings.Split(register, "\n") {
		heading, isHeading := strings.CutPrefix(strings.TrimSpace(line), "## "+engineJoinOpenItemPrefix)
		if !isHeading {
			continue
		}
		number, _, _ := strings.Cut(heading, " ")
		if number = strings.TrimSpace(number); number != "" {
			filed = append(filed, engineJoinOpenItemPrefix+number)
		}
	}
	slices.Sort(filed)
	if len(filed) == 0 {
		t.Fatalf("%s carries no row at all, so both directions below are held over nothing. The heading shape this reads is \"## %sN\"",
			engineJoinOpenItemsFile, engineJoinOpenItemPrefix)
	}

	// WHAT EACH ROW OWES, which is the file's own rule: the item, the surface it is about, the
	// case that reproduces it, and the register that owes it a number.
	for _, row := range engineJoinOpenItemRows {
		if !slices.Contains(filed, row.item) {
			t.Errorf("%s has no row for %s, and the table here says what that row must carry. A row deleted without its obligation being closed is the failure this gate is for",
				engineJoinOpenItemsFile, row.item)
			continue
		}
		for _, owed := range row.owes {
			if !strings.Contains(register, owed) {
				t.Errorf("%s's %s row does not mention %q. The row has to carry the item, the surface it is about, the case that reproduces it, and the register that owes it a number",
					engineJoinOpenItemsFile, row.item, owed)
			}
		}
	}

	// AND THE POINTER SECTION, which is the OTHER state a debt of this package can be in: the
	// ledger has already numbered it, so there is no row here and there must still be something
	// that names the symbols. doc.go says this file points at that number; this is what makes
	// that sentence true rather than decorative.
	for _, owed := range engineJoinLedgerPointer {
		if !strings.Contains(register, owed) {
			t.Errorf("%s does not mention %q. Four symbols of this package are declared by no document and are filed in the spec ledger rather than here; doc.go says this file points at them, and a pointer that names neither the symbol nor the number points at nothing",
				engineJoinOpenItemsFile, owed)
		}
	}

	// AND THE PRODUCTION SOURCE HAS TO CITE EVERY ROW, which is the half that goes red if somebody
	// removes the obligation from the interface and leaves the document behind. An open item
	// nothing cites is a file nobody opens.
	cited := map[string][]string{}
	for path, prose := range engineJoinProductionProse(t) {
		for _, item := range engineJoinItemsNamedIn(prose) {
			if !slices.Contains(cited[item], path) {
				cited[item] = append(cited[item], path)
			}
		}
	}
	citedItems := []string{}
	for item := range cited {
		citedItems = append(citedItems, item)
		slices.Sort(cited[item])
	}
	slices.Sort(citedItems)
	uncited := []string{}
	for _, item := range filed {
		if len(cited[item]) == 0 {
			uncited = append(uncited, item)
		}
	}
	t.Logf("register: %d row(s) %v; production source cites %d %v", len(filed), filed, len(citedItems), citedItems)
	t.Logf("complement: %d filed row(s) that no production file cites, %v", len(uncited), uncited)
	for _, item := range uncited {
		t.Errorf("no production prose of this package cites %s, which %s files. The obligation belongs at the surface a caller uses, and a register nothing points at is a register nobody reads",
			item, engineJoinOpenItemsFile)
	}
	for _, item := range citedItems {
		if !slices.Contains(filed, item) {
			t.Errorf("production source cites %s at %v and %s has no row for it. A number that names nothing is worse than no number: it reads as though somebody decided something",
				item, cited[item], engineJoinOpenItemsFile)
		}
	}
}

// engineJoinItemsNamedIn answers every item of this register named in one file's prose.
//
// Read off the text rather than matched against a list, so a number cited before its row exists is
// found by the half of the gate above that looks for one.
func engineJoinItemsNamedIn(prose string) []string {
	prefix := strings.ToLower(engineJoinOpenItemPrefix)
	named := []string{}
	for index := 0; index+len(prefix) <= len(prose); index += 1 {
		if !strings.HasPrefix(prose[index:], prefix) {
			continue
		}
		end := index + len(prefix)
		for end < len(prose) && '0' <= prose[end] && prose[end] <= '9' {
			end += 1
		}
		if end == index+len(prefix) {
			continue
		}
		if item := strings.ToUpper(prose[index:end]); !slices.Contains(named, item) {
			named = append(named, item)
		}
	}
	return named
}

// The register, the shape of an item in it, and what each row owes, each spelled once. A wrong
// spelling fails the read above rather than quietly holding nothing.
const (
	engineJoinOpenItemPrefix = "MG-"
	engineJoinOpenItemsFile  = "OPENITEMS.md"
)

// What each row of the register must carry, by the file's own rule.
var engineJoinOpenItemRows = []struct {
	item string
	owes []string
}{
	{
		item: "MG-1",
		owes: []string{
			"GroupEngine.JoinFromWelcome",
			"TestAWelcomeFromAnAttackerJoinsAndTheOnlyThingItGetsWrongIsWhoTheGroupIs",
			"SPEC-LEDGER.md",
		},
	},
	{
		item: "MG-2",
		owes: []string{
			"SealRecord",
			"eph_window",
			"TestEverySealableClassRoundTripsAndTheWrapItemOneEightyFiveRefusesDoesNot",
			"SPEC-LEDGER.md",
		},
	},
	{
		item: "MG-3",
		owes: []string{
			"EphKey",
			"TestEphKeyIsMasterSection81sDerivationAndNotThisPackagesOpinionOfIt",
			"TestEphKeyReachesNoClockSourceInThisPackage",
			"SPEC-LEDGER.md",
		},
	},
}

// The symbols the seal lift invented, which the spec ledger has already numbered and which this
// directory therefore points at instead of re-filing.
var engineJoinLedgerPointer = []string{
	"InstallEphRoot",
	"senderLadderKey",
	"TrackSender",
	"ephRoot",
	"item 188",
}
