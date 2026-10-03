// The file the ratchet tree's mlswg vector runners live in, and the accounting they share.
//
// Two families are this plan's: 10 (tree-validation) and 11 (TreeKEM). Family 9
// (tree-operations) is the group lifecycle plan's, because its vector carries a serialized
// Proposal. Both of this plan's two are INSTALLED here, each in the commit that deletes its
// number from expectedPendingFamilies, and TestTreeVectorFamiliesAreInstalledOrPending holds
// both halves of that at once. The machinery the two runners are built out of came first and
// is still the first half of this file, exercised over the real corpora rather than declared
// and left for a later task to be the first caller of.
//
// What each runner ADDS over the sweeps its own plan already landed, stated plainly because a
// runner that adds no protection of its own is better replaced by a comment. Family 10's four
// answers -- resolutions, tree hashes, the section 7.9 chain, the leaf signatures -- are each
// swept somewhere else in this package already, over the same corpus. What is new is that they
// are asked of ONE case together and reported as one verdict, that the family is offered its
// corpus by TestVectorFamiliesVerify, and that the comparator is held to REFUSING every class
// of wrong case it names, which no sweep over a corpus that agrees with everything can be. The
// word EVERY is the gate TestTheTreeComparatorsRefuseEveryClassTheyName makes true: each
// family's class is read off its own comparator and verdict, and a class no control row reaches
// and no written exemption excuses fails there. It replaces a count, and the count is what let
// two arms be deleted with the package green.
// Family 11 adds the same three plus a generate direction: a sender built here, a receiver
// that is not the sender, and a commit secret re-derived from the RFC text with crypto/hmac.
//
// Wired to the shared registry rather than to a second copy of it. The plan's own text for
// this task is explicit about the corpus half of that -- no second loader, no second hex
// decoder, no second PINS.md, because three parallel hex decoders over one corpus is how two
// of them end up disagreeing about the empty string -- and the same argument reaches the
// accounting. vectors_runner_test.go already holds the run tally, the suite filter, the
// comparator control driver and the probe every control is read through, each of them put
// there because a runner that had rediscovered it independently would have had to learn the
// same lesson at whatever point somebody noticed. So the pieces below are the ones that are
// genuinely this family pair's and nothing else:
//
//   - treeVectorFile, which reads a corpus basename off the shared manifest instead of
//     spelling it, so a family renumbered or repointed in vectors_test.go fails here rather
//     than leaving this file reading a corpus the registry no longer names.
//   - treeVectorsOfSuite, which decodes a corpus into a runner's own struct and partitions it
//     THROUGH the shared tally, so the covered and skipped halves of the run are counted by
//     the same code every other family is counted by.
//   - treeVectorPublishedSuite, the comparator this file owns. It returns the published
//     answer it read as EVIDENCE rather than returning nothing, which is the defect p4
//     shipped twice one task apart: a verifier that returns nothing lets its caller count
//     CALLS, and a call that returned is not a comparison that happened.
//
// The suite filter is the shared implementedSuite and NOT the single code point the plan's
// snippet for this task tests against. A helper that kept only 0x0003 would assume a
// singleton suite, which this slice's global constraints forbid outright, and it would do it
// invisibly: every count below would still add up, over a run that covered one of the two
// registered suites. assertRun holds the matched key set against Suites(), so the singleton
// version fails there.
//
// treeVectorsOfSuite's SIGNATURE departs from the plan's too, in two more ways, and they are
// written down here because the tasks that call it are not written yet and will hit them. The
// plan spells it `func treeVectorsOfSuite[T any](t *testing.T, file string, suiteOf func(v *T)
// CipherSuite) []T`. What is here returns `(*vectorRunTally, []treeVectorCase[T])` and takes a
// suiteOf answering `uint16`.
//
// Both are consequences of routing the partition through the shared tally rather than a second
// copy of it. The tally has to reach the caller, because the counts a family writes down are
// that family's to assert and deriving them inside the helper with the filter under test is
// how a filter matching nothing agrees with itself; and the corpus publishes a code point, not
// a CipherSuite, so a suiteOf returning CipherSuite would have to convert an unregistered code
// point into the registered type before the filter had decided whether it is one. The case
// wrapper carries the raw text alongside the decoded value, which is what lets a runner read
// every answer twice.
//
// The cost is at the call sites: the plan's Task 25 writes `vectors := treeVectorsOfSuite(t,
// "treekem.json", ...)`, a single-value assignment that does not compile against this, and a
// caller reads one.Value where the plan reads the bare T. Tasks 24 and 25 own that edit.
package mls

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// treeVectorRun is one family's whole run: the four counts assertRun holds that family's
// partition of its corpus against.
//
// A named type rather than an anonymous row inside the partition test, because the family
// NUMBERS and the family COUNTS have to be one table. They were two -- a slice of numbers here
// and a row table down in the test -- and a review measured what the second spelling costs:
// shrinking the slice to a single family took the other family out of three of the five tests
// in this file, left the partition test still covering it because its rows were the other
// spelling, and the package reported nothing at all.
type treeVectorRun struct {
	// covered and skipped are the two halves of the suite filter's partition of the file.
	covered int
	skipped int
	// comparisons is how many published answers were compared against and distinct is how
	// many of those answers differed from each other. A corpus read as one repeated value
	// compares the right number of times against the wrong number of answers.
	comparisons int
	distinct    int
}

// treeVectorRuns is the two section 4.2.1 rows this plan gates on, keyed by number.
//
// Numbers and not file names, because the number is what the registry keys on and the file
// name is what treeVectorFile derives from it. A list of two names here and a manifest over
// there is two spellings of one fact, and the second one goes stale silently.
//
// The counts are written down rather than derived, for the reason assertRun records: a count
// derived with the filter that is under test agrees with itself whatever the filter does. 14
// cases per suite over the seven published suites is tree-validation's 98, and 11 per suite is
// TreeKEM's 77; this package registers two of the seven.
var treeVectorRuns = map[int]treeVectorRun{
	10: {covered: 28, skipped: 70, comparisons: 28, distinct: 2},
	11: {covered: 22, skipped: 55, comparisons: 22, distinct: 2},
}

// treeVectorFamilies is the numbers of treeVectorRuns, ascending, and is the only list of them
// this file has.
//
// Derived and not written, so a family can leave this plan's gates only by leaving the table
// above -- and TestTreeVectorFamiliesAreEveryTreeFamilyTheRegistryNames refuses that too, by
// holding the table to the manifest rows that name a tree.
var treeVectorFamilies = slices.Sorted(maps.Keys(treeVectorRuns))

// treeVectorFamiliesElsewhere is every family whose manifest row names a tree and that this
// plan does NOT gate, with the argument for each.
//
// An exemption table and not a filter, for the same reason treeIndexWraps is one: which plan
// owns a family is a judgement about that family and not a property its manifest row carries.
// What makes an exemption table safe is that it is held in both directions --
// TestTreeVectorFamiliesAreEveryTreeFamilyTheRegistryNames refuses an exemption for a family no
// row names, refuses a family that is both exempt and gated, and refuses a tree family that is
// neither -- so the only way a family leaves this file is somebody writing down here who has it
// and why.
var treeVectorFamiliesElsewhere = map[int]string{
	1: "tree math, wave 1's; tree_math_kat_test.go installs it and it is no longer pending",
	3: "the secret tree, p4's; a different tree entirely, the key schedule's ratchets rather than the ratchet tree",
	9: "tree operations, the group lifecycle plan's, because its vector carries a serialized Proposal",
}

// treeVectorFile is one family's corpus basename, read off the shared manifest.
//
// Derived rather than written, so this file cannot name a corpus the registry does not. The
// pin relationship -- every manifest file appears in VECTORS.sha256 -- is
// TestVectorManifestIsComplete's and is deliberately not repeated here; what is checked is
// only that the row exists and names something, since a missing row would otherwise reach
// LoadVectorFile as the empty string and be reported as an unreadable file rather than as the
// registry disagreeing with this plan.
func treeVectorFile(t *testing.T, number int) string {
	t.Helper()
	family, ok := vectorManifest[number]
	if !ok {
		t.Fatalf("family %d is not in the manifest, so this file names a family the registry does not know about", number)
	}
	if family.File == "" {
		t.Fatalf("family %d (%s) names no corpus file", number, family.Name)
	}
	return family.File
}

// treeVectorHeader is the one field families 10 and 11 have in common and the only field this
// task reads: the ciphersuite the case is published at.
//
// Its json key is not typed out a second time in this file. theJsonKeyOf reads it off this
// tag, so the generic decode below and the struct decode above address the same key by
// construction -- two spellings of one key is how the two end up disagreeing about which key
// the corpus uses, and the disagreement is silent in the worst direction.
//
// In this FILE, and the distinction is not pedantry. The shared aCaseAtARegisteredSuite, which
// this file's comparator control gets its accepted case from, carries a literal tag of its
// own, so within the package there are two spellings whatever this file does.
// TestTheSharedCaseFinderAddressesTheSameCiphersuiteKey reads that literal off its source and
// holds it to this tag, which is the difference between the claim being true and the claim
// being checked.
type treeVectorHeader struct {
	CipherSuite uint16 `json:"cipher_suite"`
}

// treeVectorHeaderAsText is treeVectorHeader with the ciphersuite read as a string, so the
// control below can hand treeVectorsOfSuite a struct the vendored corpus cannot decode into.
//
// A struct rather than a corrupted corpus file, because the corpus is pinned by digest and
// the arm under test is "this runner's own struct does not fit the file", which is a property
// of the struct.
type treeVectorHeaderAsText struct {
	CipherSuite string `json:"cipher_suite"`
}

// treeVectorCase is one case of a vendored family file at a ciphersuite this package
// registers.
type treeVectorCase[T any] struct {
	// Index is the case's position in the corpus, for the failure messages.
	Index int
	// Suite is the registered suite the shared filter matched it at.
	Suite CipherSuite
	// Raw is the case's own json text, kept because a runner owes a second decode of every
	// answer it compares: the struct below is the runner's reading of the file and the raw
	// text is the file, and a struct tag pointing at a key the corpus does not publish is
	// visible only where the two are read apart.
	Raw json.RawMessage
	// Value is the case decoded into the runner's own struct.
	Value T
}

// treeVectorsOfSuite decodes every entry of a vendored family file into T, partitions the
// file through the shared run tally, and returns the entries at a ciphersuite this package
// has a provider for.
//
// The tally is returned rather than kept, because the accounting is the caller's to assert:
// the counts a family writes down are that family's, and deriving them here with the same
// filter that is under test is how a filter matching nothing ends up agreeing with itself.
// What this function does is make the partition impossible to do any other way -- every case
// takes exactly one branch of tally.filter, so covered plus skipped is the file and assertRun
// can say so.
//
// An empty in-scope half is fatal here rather than an empty slice returned. A runner handed
// nothing loops zero times, compares nothing and reports a pass, which is the single failure
// the whole vector harness is built to be unable to reach.
func treeVectorsOfSuite[T any](t *testing.T, file string, suiteOf func(v *T) uint16) (*vectorRunTally, []treeVectorCase[T]) {
	t.Helper()
	tally, entries := newVectorRunTally(t, file)
	kept := []treeVectorCase[T]{}
	for index, raw := range entries {
		var value T
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s case %d does not decode into this runner's struct: %v", file, index, err)
		}
		suite, inScope := tally.filter(suiteOf(&value))
		if !inScope {
			continue
		}
		kept = append(kept, treeVectorCase[T]{Index: index, Suite: suite, Raw: raw, Value: value})
	}
	if len(kept) == 0 {
		t.Fatalf("%s published no case at a ciphersuite this package registers, so every comparison over the result would be over nothing",
			file)
	}
	return tally, kept
}

// The three refusals treeVectorPublishedSuite makes. Three sentinels and not one, because
// assertComparatorRefuses reports a refusal for the WRONG reason, and a comparator with one
// sentinel cannot produce that report.
var (
	errTreeVectorSuiteUndecodable = errors.New("the vector case is not a json object")
	errTreeVectorSuiteUnpublished = errors.New("the vector case publishes no ciphersuite")
	errTreeVectorSuiteNotANumber  = errors.New("the vector case publishes a ciphersuite that is not a decimal number")
)

// treeVectorPublishedSuite reads a case's ciphersuite out of the case's own json text and
// returns it AS TEXT.
//
// The returned string is the point. A comparator that answers nothing lets its caller count
// the calls it made, and a call that returned is not a comparison that happened -- p4 shipped
// exactly that twice, one task apart, and the shared tally's answer() now takes the published
// value the runner read rather than a tick. This is that value: the corpus's own bytes for
// the field, addressed by the key treeVectorHeader's tag names, decoded by nothing the
// runner's own struct decode touches.
//
// The key is a parameter rather than a literal so the callers can derive it with
// theJsonKeyOf. An error is returned and never reported, because assertComparatorRefuses
// drives this function with cases that are wrong on purpose and a t.Fatalf would end the test
// doing the driving rather than the case being driven.
func treeVectorPublishedSuite(key string, raw json.RawMessage) (string, error) {
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", fmt.Errorf("%w: %v", errTreeVectorSuiteUndecodable, err)
	}
	field, found := object[key]
	if !found {
		return "", fmt.Errorf("%w: no %q", errTreeVectorSuiteUnpublished, key)
	}
	published := strings.TrimSpace(string(field))
	if _, err := strconv.ParseUint(published, 10, 16); err != nil {
		return "", fmt.Errorf("%w: %s", errTreeVectorSuiteNotANumber, published)
	}
	return published, nil
}

// rewriteTreeVectorField replaces one top level field of a corpus case, or deletes it when
// the replacement is nil, and re-encodes the case the way the corpus encodes one.
//
// One field moved and nothing else, so a refusal below is attributable to the thing that was
// made wrong. The absence of the field being rewritten is fatal: a corruption that corrupted
// nothing is a control row that drives the comparator with a case it should accept.
func rewriteTreeVectorField(t *testing.T, raw json.RawMessage, key string, replacement json.RawMessage) json.RawMessage {
	t.Helper()
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("parse the case to corrupt: %v", err)
	}
	if _, found := object[key]; !found {
		t.Fatalf("the case publishes no %q, so rewriting it would corrupt nothing", key)
	}
	if replacement == nil {
		delete(object, key)
	} else {
		object[key] = replacement
	}
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("re-encode the corrupted case: %v", err)
	}
	return body
}

// TestTreeVectorFilesAreVendored holds the two corpora this plan reads to being present and
// parsing to cases, through the harness loader and nothing of this file's own.
//
// Partly redundant, and stated as redundant rather than left to look like coverage:
// TestVectorFilesArePinned already fails when either file is absent or edited, and
// TestVectorManifestIsComplete already fails when a manifest row names a file the pin does
// not cover. What is NOT redundant is the direction -- this reads the files through
// LoadVectorFile, which is the call every runner in this file makes, so a loader that started
// answering an empty corpus for a missing one is a failure here and a green run everywhere a
// runner loops over what it returned.
func TestTreeVectorFilesAreVendored(t *testing.T) {
	if len(treeVectorFamilies) == 0 {
		t.Fatal("this plan claims no vector family, so the loop below reads nothing")
	}
	for _, number := range treeVectorFamilies {
		file := treeVectorFile(t, number)
		if raws := LoadVectorFile(t, file); len(raws) == 0 {
			t.Fatalf("%s decoded to zero entries", file)
		}
	}
}

// requireTreeVectorSuiteAgrees holds one case's two independent decodes of its ciphersuite
// equal: the suite the shared filter kept it at, and the answer read out of the case's own
// json text.
//
// A function rather than four lines inline in the loop below, and the reason is a measurement
// rather than a preference. Inline, this comparison could be DELETED with all five tests of
// this file still reporting green: nothing drove it with a case where the two decodes
// disagree, because every case of both vendored corpora agrees. An assertion no control can
// reach is an assertion whose absence looks exactly like its presence.
// TestTreeVectorSuiteAgreementFlagsADisagreement drives this one now, so deleting it fails
// there.
func requireTreeVectorSuiteAgrees(t *testing.T, file string, index int, kept CipherSuite, published string) {
	t.Helper()
	if published != strconv.Itoa(int(kept)) {
		t.Fatalf("%s case %d was kept at suite %#04x and its own text publishes %s, so the struct this runner decodes with is not reading the field it addresses",
			file, index, uint16(kept), published)
	}
}

// TestTreeVectorCorporaPartitionByTheRegistry is the run this file can hold today, and the
// only comparison it has an answer for before the ratchet tree exists: every case's
// ciphersuite, read twice.
//
// Once through treeVectorHeader, which is how treeVectorsOfSuite decides what is in scope,
// and once out of the case's own json text by treeVectorPublishedSuite, which decodes through
// nothing the first decode touches. The two are held equal per case and the published half is
// what is handed to the tally, so a comparator that answered without reading the corpus moves
// neither the comparison count nor the distinct-answer count.
//
// The families and their counts are read off treeVectorRuns, which is the whole of this
// plan's family class and the same table the other four tests of this file loop over. This
// test used to carry its own row table -- a second spelling of the family numbers -- and a
// review shrank the other spelling to one family and watched this test go on covering both.
func TestTreeVectorCorporaPartitionByTheRegistry(t *testing.T) {
	key := theJsonKeyOf(t, treeVectorHeader{}, "CipherSuite")
	for _, number := range treeVectorFamilies {
		row := treeVectorRuns[number]
		file := treeVectorFile(t, number)
		t.Run(file, func(t *testing.T) {
			tally, kept := treeVectorsOfSuite(t, file, func(v *treeVectorHeader) uint16 {
				return v.CipherSuite
			})
			for _, one := range kept {
				published, err := treeVectorPublishedSuite(key, one.Raw)
				if err != nil {
					t.Errorf("%s case %d: %v", file, one.Index, err)
				}
				// the shared refusal, handed the comparator's OWN verdict and not a tick
				// saying it was called. A comparator that declined a case the filter
				// matched is the filter and the comparator disagreeing about what runs,
				// and the counts below would then read correct over a smaller run.
				tally.requireCompared(t, one.Index, one.Suite, err == nil)
				requireTreeVectorSuiteAgrees(t, file, one.Index, one.Suite, published)
				tally.answer(published)
			}
			tally.assertRun(t, row.covered, row.skipped, row.comparisons, row.distinct)
		})
	}
}

// TestTreeVectorPublishedSuiteRefusesAWrongCase is the control on the comparator this file
// owns.
//
// Every case of both vendored corpora agrees with it, so a comparator that read the field and
// a comparator that returned a constant produce identical runs above; the only way to
// separate them is to hand it an answer that is wrong on purpose, once per defect class, and
// require the matching refusal. The unmodified case is required to be accepted first by
// assertComparatorRefuses, so a comparator that refused everything does not satisfy the table.
//
// Both corpora, not one. The two files are different shapes -- tree-validation publishes a
// tree and its hashes, TreeKEM publishes an epoch and a set of update paths -- and a
// comparator that happened to work on one is worth nothing to the family that reads the other.
func TestTreeVectorPublishedSuiteRefusesAWrongCase(t *testing.T) {
	key := theJsonKeyOf(t, treeVectorHeader{}, "CipherSuite")
	compare := func(_ *testing.T, raw json.RawMessage) error {
		_, err := treeVectorPublishedSuite(key, raw)
		return err
	}
	for _, number := range treeVectorFamilies {
		file := treeVectorFile(t, number)
		accepted, found := aCaseAtARegisteredSuite(t, file)
		if !found {
			t.Fatalf("%s publishes no case at a suite this package registers, so nothing drives the comparator", file)
		}
		assertComparatorRefuses(t, file, compare, accepted, []comparatorRefusal{
			{
				name:   "a case that is not a json object at all",
				vector: json.RawMessage("[]"),
				want:   errTreeVectorSuiteUndecodable,
			},
			{
				name:   "a case that publishes no ciphersuite",
				vector: rewriteTreeVectorField(t, accepted, key, nil),
				want:   errTreeVectorSuiteUnpublished,
			},
			{
				name:   "a case whose ciphersuite is published as a string",
				vector: rewriteTreeVectorField(t, accepted, key, json.RawMessage(`"3"`)),
				want:   errTreeVectorSuiteNotANumber,
			},
		})
	}
}

// TestTreeVectorsOfSuiteFlagsTheControlFixture drives the partition helper instead of
// asserting about it.
//
// Both of its refusals are the kind that look like nothing when they stop firing: a corpus
// this runner's struct cannot decode would otherwise reach the filter as a zero ciphersuite
// and be skipped, and a filter that matched nothing would return an empty slice a runner
// loops over zero times. The rows are bound to the function's own reports, so a third fatal
// cannot land here uncontrolled and a deleted one leaves a row naming nothing.
func TestTreeVectorsOfSuiteFlagsTheControlFixture(t *testing.T) {
	file := treeVectorFile(t, treeVectorFamilies[0])
	suiteOf := func(v *treeVectorHeader) uint16 { return v.CipherSuite }
	if failed, raised := probeAssertion(func(probe *testing.T) {
		treeVectorsOfSuite(probe, file, suiteOf)
	}); failed || raised != nil {
		t.Fatalf("the vendored %s was reported: failed=%v raised=%v; every row below would then pass for the wrong reason",
			file, failed, raised)
	}
	// asserted rather than assumed, because a registry that grew to hold this code point would
	// turn the row below into a run that covers everything and reports nothing.
	if _, ok := implementedSuite(unregisteredControlSuite); ok {
		t.Fatalf("suite %#04x is registered, so the filter row below does not match nothing", unregisteredControlSuite)
	}
	rows := []struct {
		// names is the substring of the report this row must provoke; the bijection against
		// treeVectorsOfSuite's own reports is asserted below.
		names string
		run   func(probe *testing.T)
	}{
		{"does not decode into this runner's struct", func(probe *testing.T) {
			treeVectorsOfSuite(probe, file, func(v *treeVectorHeaderAsText) uint16 { return 0 })
		}},
		{"published no case at a ciphersuite this package registers", func(probe *testing.T) {
			treeVectorsOfSuite(probe, file, func(v *treeVectorHeader) uint16 { return unregisteredControlSuite })
		}},
	}
	keys := []string{}
	for _, row := range rows {
		keys = append(keys, row.names)
	}
	assertEveryReportIsControlled(t, "treeVectorsOfSuite",
		theReportsOf(t, theSourceDeclaring(t, "", "treeVectorsOfSuite"), "", "treeVectorsOfSuite"), keys)
	for _, row := range rows {
		failed, raised := probeAssertion(row.run)
		if raised != nil {
			t.Errorf("%s: the helper panicked: %v", row.names, raised)
			continue
		}
		if !failed {
			t.Errorf("%s: the helper reported nothing, so a corpus it read as empty would reach a runner as a loop over no cases",
				row.names)
		}
	}
}

// TestTreeVectorFamiliesAreEveryTreeFamilyTheRegistryNames derives this plan's family class
// from the registry instead of trusting the table that spells it.
//
// treeVectorRuns is a written list, and a written list of a class is the defect this project
// has paid for fourteen times: it fails when a family is added and the list is remembered, and
// says nothing when a family is dropped. A review dropped one and measured the result -- the
// dropped family lost its vendoring check, lost its comparator control and lost the gate that
// notices it gaining a Verify, and every test in the package still passed.
//
// So the class is derived: every manifest row whose family name or corpus file names a tree.
// That set is {tree math, secret tree, tree operations, tree validation, TreeKEM} today, and
// three of the five are somebody else's -- which is a judgement about those families rather
// than anything their rows carry, so it is written down in treeVectorFamiliesElsewhere with a
// reason each. The two tables together must be exactly the derived set, in both directions.
func TestTreeVectorFamiliesAreEveryTreeFamilyTheRegistryNames(t *testing.T) {
	if len(treeVectorRuns) == 0 {
		t.Fatal("this plan gates no vector family, so every loop of this file reads nothing")
	}
	named := map[int]bool{}
	for number, family := range vectorManifest {
		if strings.Contains(strings.ToLower(family.Name), "tree") || strings.Contains(strings.ToLower(family.File), "tree") {
			named[number] = true
		}
	}
	// the positive control, and TreeKEM specifically: its row spells the word inside a longer
	// one, so a matcher looking for a WORD rather than a substring reports the same clean bill
	// a complete one reports, over a class missing the family this plan reads second.
	if !named[11] {
		t.Fatalf("the scan did not find family 11 (%s), whose row certainly names a tree, so it is matching something other than the manifest's rows",
			vectorManifest[11].Name)
	}
	if len(named) == len(vectorManifest) {
		t.Fatalf("all %d families of the manifest matched, so this rule selects nothing and the two tables below are held against everything",
			len(vectorManifest))
	}
	for _, number := range slices.Sorted(maps.Keys(named)) {
		_, gated := treeVectorRuns[number]
		reason, excused := treeVectorFamiliesElsewhere[number]
		switch {
		case gated && excused:
			t.Errorf("family %d (%s) is gated by this file and also excused to somebody else as %q; one of the two is wrong and a reader cannot tell which",
				number, vectorManifest[number].Name, reason)
		case !gated && !excused:
			t.Errorf("family %d (%s) names a tree and this file neither gates it nor says who has it; a family that left treeVectorRuns silently is a corpus three tests of this file stopped reading",
				number, vectorManifest[number].Name)
		}
	}
	for _, number := range slices.Sorted(maps.Keys(treeVectorFamiliesElsewhere)) {
		if !named[number] {
			t.Errorf("family %d is excused to another plan and no manifest row of that number names a tree, so the exemption covers a family this rule never selected",
				number)
		}
		if strings.TrimSpace(treeVectorFamiliesElsewhere[number]) == "" {
			t.Errorf("family %d is excused with no reason written down, which is the exemption reading as a filter", number)
		}
	}
	for _, number := range treeVectorFamilies {
		if !named[number] {
			t.Errorf("this file gates family %d and its manifest row (%s, %s) names no tree, so the derived class above is not the class this file runs over",
				number, vectorManifest[number].Name, vectorManifest[number].File)
		}
	}
}

// TestTreeVectorSuiteAgreementFlagsADisagreement drives the per case equality the partition
// test makes, rather than asserting about it.
//
// It is the assertion that says the struct this runner filters with and the text the tally is
// answered from are reading one field. Every case of both vendored corpora agrees with it, so
// the real run cannot separate an assertion that holds from an assertion that is not there --
// measured: deleted, all five tests of this file still passed. The row below is the case the
// corpora do not contain.
func TestTreeVectorSuiteAgreementFlagsADisagreement(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("this package registers no ciphersuite, so the agreement below is over nothing")
	}
	kept := suites[0]
	agreeing := strconv.Itoa(int(kept))
	if failed, raised := probeAssertion(func(probe *testing.T) {
		requireTreeVectorSuiteAgrees(probe, "control.json", 0, kept, agreeing)
	}); failed || raised != nil {
		t.Fatalf("two decodes that agree were reported: failed=%v raised=%v; the row below would then pass for the wrong reason",
			failed, raised)
	}
	rows := []struct {
		// names is the substring of the report this row must provoke; the bijection against
		// the function's own reports is asserted below.
		names     string
		published string
	}{
		{"is not reading the field it addresses", agreeing + "0"},
	}
	keys := []string{}
	for _, row := range rows {
		keys = append(keys, row.names)
	}
	assertEveryReportIsControlled(t, "requireTreeVectorSuiteAgrees",
		theReportsOf(t, theSourceDeclaring(t, "", "requireTreeVectorSuiteAgrees"), "", "requireTreeVectorSuiteAgrees"), keys)
	for _, row := range rows {
		failed, raised := probeAssertion(func(probe *testing.T) {
			requireTreeVectorSuiteAgrees(probe, "control.json", 0, kept, row.published)
		})
		if raised != nil {
			t.Errorf("%s: the assertion panicked: %v", row.names, raised)
			continue
		}
		if !failed {
			t.Errorf("%s: the assertion reported nothing, so it can be deleted with this file's whole run still reading green",
				row.names)
		}
	}
}

// TestTheSharedCaseFinderAddressesTheSameCiphersuiteKey holds every place outside this file that
// types the corpus's ciphersuite key out to the key this file derives.
//
// The header of this file used to claim the key "is never typed out a second time". Within this
// file that was true; within the package it was not, and the second spelling sits on the path
// this file's own comparator control runs through: aCaseAtARegisteredSuite hands back the case
// whose key TestTreeVectorPublishedSuiteRefusesAWrongCase then derives with theJsonKeyOf. Two
// spellings that disagreed would leave that control driving the comparator with a case selected
// at a field the comparator never reads, and the refusal table would go on passing.
//
// The finder decodes through vectorCaseHeader now -- p6 task 18 gave it a named type when the
// suiteless corpus derivation needed the same key a third time -- so this gate reads THAT tag by
// reflection and holds the finder to still using it. Both halves are load bearing and for
// different reasons. The reflection half is the comparison of the two spellings. The structural
// half is what says the finder has not grown a private spelling beside the shared one: an
// anonymous struct written back into its body is a key this reflection cannot see, and the
// version of this gate that only scanned the body stopped seeing anything at all the moment the
// literal moved out of it -- which is how it came to be rewritten.
func TestTheSharedCaseFinderAddressesTheSameCiphersuiteKey(t *testing.T) {
	key := theJsonKeyOf(t, treeVectorHeader{}, "CipherSuite")
	if shared := theJsonKeyOf(t, vectorCaseHeader{}, "CipherSuite"); shared != key {
		t.Errorf("%T decodes %q and %T addresses %q, so the case that drives this file's comparator control is selected at a field the comparator never reads",
			vectorCaseHeader{}, shared, treeVectorHeader{}, key)
	}
	parsed := theSourceDeclaring(t, "", "aCaseAtARegisteredSuite")
	declaration := parsed.declarationOf(t, "", "aCaseAtARegisteredSuite")
	found := []string{}
	ast.Inspect(declaration, func(node ast.Node) bool {
		field, isField := node.(*ast.Field)
		if !isField || field.Tag == nil {
			return true
		}
		text, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			t.Fatalf("unquote a struct tag of aCaseAtARegisteredSuite: %v", err)
		}
		if tag, _, _ := strings.Cut(reflect.StructTag(text).Get("json"), ","); tag != "" {
			found = append(found, tag)
		}
		return true
	})
	for _, tag := range found {
		if tag != key {
			t.Errorf("aCaseAtARegisteredSuite spells a json key %q of its own and %T addresses %q",
				tag, treeVectorHeader{}, key)
		}
	}
	if len(found) > 0 {
		return
	}
	// no key of its own, so it decodes through the shared named type -- and this is where that
	// is checked rather than assumed. A finder that had moved to some third header would leave
	// the reflection above comparing a type nothing uses.
	names := false
	ast.Inspect(declaration, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && identifier.Name == "vectorCaseHeader" {
			names = true
		}
		return true
	})
	if !names {
		t.Errorf("aCaseAtARegisteredSuite spells no json key and does not decode through %T either, so nothing here says which key it selects a case at",
			vectorCaseHeader{})
	}
}

// ---------------------------------------------------------------------------
// task 24: family 10, tree-validation
// ---------------------------------------------------------------------------

// The corpus basename family 11 reads, spelled once.
//
// Family 10's is tree_math_test.go's treeValidationVectorFile rather than a second spelling of
// the same string: three readers of that corpus already live in this package and a fourth
// constant naming the same file is how one of them ends up pointed somewhere else. treekem.json
// has no constant yet -- treekem_test.go spells it inline -- so this declares it.
//
// Neither is trusted: treeVectorFamilyFiles below is held to the manifest's own File by
// TestTreeVectorFamilyFilesAreTheRegistrysFiles, so a family renumbered or repointed in
// vectors_test.go fails there rather than leaving this file reading a corpus the registry no
// longer names.
const treekemVectorFile = "treekem.json"

// treeVectorFamilyFiles is the corpus each gated family's runner reads, keyed by family number.
var treeVectorFamilyFiles = map[int]string{
	10: treeValidationVectorFile,
	11: treekemVectorFile,
}

// The accounting that makes family 10's runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/tree-validation.json holds at the pinned mlswg
// commit, counted off the corpus text with a json reader and not off this implementation: 98
// cases, 14 at each of the seven published ciphersuites, of which the two this package
// registers account for 28. Those 28 trees hold 908 nodes between them and this family compares
// two published answers at every node -- that node's resolution and that node's tree hash --
// which is 1816 comparisons against 790 distinct published answers. The 790 is 717 distinct
// tree hashes plus 73 distinct resolutions with no collision between the halves, since a tree
// hash renders as 64 hex characters and a resolution as a bracketed list of decimals.
//
// treeValidationKatLeaves is the non-blank leaf slots those 28 trees carry, every one of which
// has its signature verified at its own group id and its own index, and treeValidationKatBound
// is how many of those carry a leaf_node_source that BINDS the two. The second is what says the
// wrong-group refusal has something to refuse: over a corpus of key_package leaves alone,
// accepting a signature at another group id is the conforming answer and a control asking for a
// refusal would be asking for a defect. Both agree with leaf_node_test.go's own transcription
// of the same corpus -- 322 signatures of which 288 are context bound -- which is two readers
// of one file arriving at one number rather than a number copied across.
const (
	treeValidationKatCovered     = 28
	treeValidationKatSkipped     = 70
	treeValidationKatNodes       = 908
	treeValidationKatComparisons = 2 * treeValidationKatNodes
	treeValidationKatDistinct    = 790
	treeValidationKatLeaves      = 322
	treeValidationKatBound       = 288
)

// The refusals compareTreeValidationVector makes, as sentinels rather than formatted strings,
// so a control can require a SPECIFIC refusal and a refusal for the wrong reason is reported.
//
// They are the only thing that makes this family's comparison observable at all. Every case of
// the vendored corpus agrees with this implementation, so a comparator that checked everything
// and one that returned an empty struct produce identical runs over it; the only way to
// separate them is to hand it an answer that is wrong on purpose, once per defect class, and
// require the matching refusal.
var (
	errTreeValidationIncomplete  = errors.New("the tree-validation comparison reports answers it cannot have computed")
	errTreeValidationUndecodable = errors.New("the published ratchet tree does not decode")
	errTreeValidationColumnWidth = errors.New("a published column of a tree-validation case is not one entry per node")
	errTreeValidationResolution  = errors.New("a node's resolution is not the published one")
	errTreeValidationTreeHash    = errors.New("a node's tree hash is not the published one")
	errTreeValidationParentHash  = errors.New("the published ratchet tree fails the section 7.9 parent hash check")
	errTreeValidationLeafRefused = errors.New("a published leaf node's signature does not verify at its own group id and index")
	errTreeValidationLeafUnbound = errors.New("a context bound leaf node's signature verified at a group id that is not the case's own")
)

// The two answers this family compares at every node, named by what produced them. The order is
// the order the comparator emits them in and incomplete() holds a run to it position by
// position, because a run that emitted two tree hashes for one node and no resolution would
// still emit the right NUMBER of answers.
var treeValidationAnswerNames = []string{"Resolution", "TreeHashes"}

// treeValidationAnswer is one answer this package computed held against one answer the corpus
// published, rendered as text so a resolution and a tree hash are one kind of comparison.
//
// AS TEXT because the two published columns are two json shapes -- tree_hashes is an array of
// hex strings and resolutions an array of arrays of numbers -- and a runner that re-read the
// first through a string and the second through a []uint32 would be re-reading half of its
// answers through its own struct decode, which is the one thing a second read exists not to be.
type treeValidationAnswer struct {
	// name is what produced the computed half and is one of treeValidationAnswerNames.
	name string
	// field is the dotted json path the published half lives at, which the runner re-reads out
	// of a generic decode of the same case.
	field string
	got   string
	want  string
}

// treeValidationComparison is what one run of compareTreeValidationVector PRODUCED, and it is
// the only thing its callers are allowed to judge it by.
//
// A comparator that returns nothing lets its caller count CALLS, and a call that returned is not
// a comparison that happened: p4 shipped exactly that twice, one task apart, with an early
// return leaving the run green over code that was never invoked. Every field here is written at
// the point the work that produces it happens, so a return that skipped the work reports the
// zero value and a caller judging the values sees it.
type treeValidationComparison struct {
	// inScope is true when the case's ciphersuite is one this package registers. False is not a
	// failure and not a skip: it is a case with no provider.
	inScope bool
	// hashSize is the suite's KDF.Nh, read off the provider rather than assumed.
	hashSize int
	// nodeWidth and leafWidth are the decoded tree's own shape, which both published columns
	// are held to.
	nodeWidth uint32
	leafWidth uint32
	// publishesGroupId is whether the case carries the key at all, read off a generic decode
	// with no struct tag in the way: an absent key and an empty value decode identically
	// through the struct, and a signature verified against a group id the corpus never gave is
	// a comparison against nothing.
	publishesGroupId bool
	groupId          []byte
	// answers is every comparison the run made, in node order, two per node.
	answers []treeValidationAnswer
	// leaves is the non-blank leaf slots whose signature was verified, blanks the empty ones,
	// and bound how many of the verified ones carry a source that binds group id and index.
	leaves int
	blanks int
	bound  int
	// parentHashError is what section 7.9's whole-tree check answered, nil where it accepted.
	parentHashError error
	// refusedLeaf is the first published leaf whose signature did not verify at its own group
	// and index, and acceptedLeaf the first CONTEXT BOUND leaf that verified at a group id one
	// octet away from the case's own. The second is the vacuity control on the first: every
	// published signature verifies, so a VerifySignature that answered nil for everything passes
	// the whole sweep and only the wrong question separates it. verdict reports acceptedLeaf
	// FIRST, and the argument for that order is written where the arms are.
	refusedLeaf  error
	acceptedLeaf error
}

// incomplete reports whether the evidence a compared case must carry is missing or
// inconsistent, without looking at whether any answer was right.
//
// The vacuity half, split from the correctness half on purpose: two empty strings compare equal,
// so an answer whose got and want are both empty has compared nothing whatever the comparison
// would say about it.
func (self treeValidationComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the case is out of scope and carries no comparison", errTreeValidationIncomplete)
	case self.hashSize == 0:
		return fmt.Errorf("%w: no KDF.Nh was read from the provider", errTreeValidationIncomplete)
	case self.nodeWidth == 0 || self.leafWidth == 0:
		return fmt.Errorf("%w: the decoded tree is %d nodes over %d leaves, so every column below was held against nothing",
			errTreeValidationIncomplete, self.nodeWidth, self.leafWidth)
	case !self.publishesGroupId:
		return fmt.Errorf("%w: the case publishes no group id key at all, so every leaf signature was verified against a value the corpus never gave",
			errTreeValidationIncomplete)
	case len(self.groupId) == 0:
		return fmt.Errorf("%w: the published group id is the empty octet string, which binds a leaf to nothing",
			errTreeValidationIncomplete)
	case uint32(len(self.answers)) != self.nodeWidth*uint32(len(treeValidationAnswerNames)):
		return fmt.Errorf("%w: the run made %d comparisons over a %d node tree and this family compares %d per node",
			errTreeValidationIncomplete, len(self.answers), self.nodeWidth, len(treeValidationAnswerNames))
	case uint32(self.leaves+self.blanks) != self.leafWidth:
		return fmt.Errorf("%w: %d occupied and %d blank leaf slots over a tree %d leaves wide; a slot took neither branch",
			errTreeValidationIncomplete, self.leaves, self.blanks, self.leafWidth)
	case self.leaves == 0:
		return fmt.Errorf("%w: the published tree carries no occupied leaf, so no signature was verified over it",
			errTreeValidationIncomplete)
	}
	// the emit order, position by position, and the two halves of every comparison non-empty.
	for index, answer := range self.answers {
		want := treeValidationAnswerNames[index%len(treeValidationAnswerNames)]
		if answer.name != want {
			return fmt.Errorf("%w: comparison %d is %s and this family emits %s there",
				errTreeValidationIncomplete, index, answer.name, want)
		}
		if answer.got == "" || answer.want == "" {
			return fmt.Errorf("%w: %s at %s compared %q against %q, and an empty comparison agrees with anything",
				errTreeValidationIncomplete, answer.name, answer.field, answer.got, answer.want)
		}
		if answer.field == "" {
			return fmt.Errorf("%w: %s names no published field, so nothing independent of this comparator's own decode can re-read it",
				errTreeValidationIncomplete, answer.name)
		}
	}
	return nil
}

// verdict is the whole judgement over one compared case: it must be complete, every published
// resolution and every published tree hash must agree, the tree must pass section 7.9's
// whole-tree parent hash check, and every occupied leaf must verify at its own group id and
// index while a context bound one refuses a group id that is not the case's own.
//
// The order puts the two columns ahead of the parent hash check on purpose. Section 7.9's check
// is stated over tree hashes of copath subtrees, so a defect in the tree hash breaks BOTH, and
// reporting the parent hash first would send a reader to the condition that is not the one that
// moved.
func (self treeValidationComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	for _, answer := range self.answers {
		if answer.got == answer.want {
			continue
		}
		sentinel := errTreeValidationTreeHash
		if answer.name == "Resolution" {
			sentinel = errTreeValidationResolution
		}
		return fmt.Errorf("%w: %s answered %s and the corpus publishes %s at %s",
			sentinel, answer.name, answer.got, answer.want, answer.field)
	}
	if self.parentHashError != nil {
		return fmt.Errorf("%w: %v", errTreeValidationParentHash, self.parentHashError)
	}
	// the accepted-elsewhere arm AHEAD of the refused-here one, and the order is what makes the
	// first of them reachable at all. A bound leaf carries one signature, so every case whose
	// published group id is not its leaves' produces BOTH -- the leaves fail at the case's own
	// group id and pass at the one octet flipped from it -- and reporting the refusal first left
	// errTreeValidationLeafUnbound unreachable from any comparator run, driven only over evidence
	// a control built by hand. Measured: with the wrong-group question deleted outright the whole
	// package stayed green. The defect the arm exists for -- a VerifySignature answering nil for
	// everything -- sets acceptedLeaf and leaves refusedLeaf nil, so it is reported the same way
	// under either order.
	if self.acceptedLeaf != nil {
		return fmt.Errorf("%w: %v", errTreeValidationLeafUnbound, self.acceptedLeaf)
	}
	if self.refusedLeaf != nil {
		return fmt.Errorf("%w: %v", errTreeValidationLeafRefused, self.refusedLeaf)
	}
	return nil
}

// verifyTreeValidationVector is the registry's shim: the signature RegisterVectorFamily needs,
// over the comparator that does the work and reports what it produced.
//
// Verify cannot return anything, which is exactly why the work is not done here: a runner
// counting calls to this would count a case it declined to check the same way it counts one it
// compared.
func verifyTreeValidationVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := compareTreeValidationVector(t, raw)
	if err != nil {
		t.Fatalf("tree-validation: %v", err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("tree-validation: %v", err)
	}
}

// compareTreeValidationVector runs one case of tree-validation.json and returns what the run
// produced. A case at a ciphersuite this package does not register is not a failure and not a
// skip: it comes back with inScope false and nothing else set.
//
// A corpus that will not parse or will not hex decode is fatal rather than returned, because it
// is not a verdict about this implementation -- it is the evidence itself being unreadable.
// Everything that IS a verdict about this implementation is returned, so a control can require
// a refusal instead of hoping the corpus disagrees with a defect.
func compareTreeValidationVector(t *testing.T, raw json.RawMessage) (treeValidationComparison, error) {
	t.Helper()
	// treeValidationVector is tree_math_test.go's row for this same corpus, reused rather than
	// declared again: a fourth declaration of one corpus row in one package is how two of them
	// end up disagreeing about which json key an answer lives at.
	vector := treeValidationVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse tree-validation case: %v", err)
	}
	suite, ok := implementedSuite(vector.CipherSuite)
	if !ok {
		return treeValidationComparison{}, nil
	}
	crypto := mustProvider(t, suite)

	published := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatalf("parse tree-validation case as a json object: %v", err)
	}
	_, publishesGroupId := published[theJsonKeyOf(t, treeValidationVector{}, "GroupId")]

	evidence := treeValidationComparison{
		inScope:          true,
		hashSize:         crypto.HashSize(),
		publishesGroupId: publishesGroupId,
		groupId:          MustHex(t, vector.GroupId),
	}
	tree, err := UnmarshalRatchetTree(MustHex(t, vector.Tree))
	if err != nil {
		return evidence, fmt.Errorf("%w: %v", errTreeValidationUndecodable, err)
	}
	evidence.nodeWidth = tree.NodeWidth()
	evidence.leafWidth = uint32(tree.LeafWidth())

	// both columns are held to the tree's own width BEFORE either is read, because a short
	// column would otherwise be a loop that ran fewer times and reported a clean run.
	resolutionKey := theJsonKeyOf(t, treeValidationVector{}, "Resolutions")
	hashKey := theJsonKeyOf(t, treeValidationVector{}, "TreeHashes")
	if uint32(len(vector.Resolutions)) != evidence.nodeWidth {
		return evidence, fmt.Errorf("%w: %d entries in %s for a tree of %d nodes",
			errTreeValidationColumnWidth, len(vector.Resolutions), resolutionKey, evidence.nodeWidth)
	}
	if uint32(len(vector.TreeHashes)) != evidence.nodeWidth {
		return evidence, fmt.Errorf("%w: %d entries in %s for a tree of %d nodes",
			errTreeValidationColumnWidth, len(vector.TreeHashes), hashKey, evidence.nodeWidth)
	}
	hashes, err := tree.TreeHashes(crypto)
	if err != nil {
		return evidence, fmt.Errorf("TreeHashes: %w", err)
	}
	if uint32(len(hashes)) != evidence.nodeWidth {
		return evidence, fmt.Errorf("%w: TreeHashes answered %d hashes for a tree of %d nodes",
			errTreeValidationColumnWidth, len(hashes), evidence.nodeWidth)
	}
	for x := uint32(0); x < evidence.nodeWidth; x += 1 {
		evidence.answers = append(evidence.answers,
			treeValidationAnswer{
				name:  "Resolution",
				field: fmt.Sprintf("%s.%d", resolutionKey, x),
				got:   treeResolutionText(tree.Resolution(NodeIndex(x))),
				want:  publishedNodeIndexListText(vector.Resolutions[x]),
			},
			treeValidationAnswer{
				name:  "TreeHashes",
				field: fmt.Sprintf("%s.%d", hashKey, x),
				got:   HexOf(hashes[x]),
				want:  vector.TreeHashes[x],
			})
	}

	evidence.parentHashError = tree.VerifyParentHashes(crypto)

	for x := uint32(0); x < evidence.leafWidth; x += 1 {
		leaf := tree.Leaf(LeafIndex(x))
		if leaf == nil {
			evidence.blanks += 1
			continue
		}
		evidence.leaves += 1
		if err := leaf.VerifySignature(crypto, evidence.groupId, LeafIndex(x)); err != nil && evidence.refusedLeaf == nil {
			evidence.refusedLeaf = fmt.Errorf("leaf %d: %v", x, err)
		}
		// the vacuity control, and only over the leaves the RFC binds. Section 7.2's select
		// puts the group id and the leaf index inside the LeafNodeTBS for the update and commit
		// sources and leaves them out for key_package, so a key_package leaf verifying at
		// another group is the conforming answer rather than a forgery.
		if !leafNodeSourceBindsItsPosition(leaf.LeafNodeSource) {
			continue
		}
		evidence.bound += 1
		elsewhere := bytes.Clone(evidence.groupId)
		elsewhere[0] ^= 0x01
		if err := leaf.VerifySignature(crypto, elsewhere, LeafIndex(x)); err == nil && evidence.acceptedLeaf == nil {
			evidence.acceptedLeaf = fmt.Errorf("leaf %d, whose source is %v", x, leaf.LeafNodeSource)
		}
	}

	return evidence, evidence.verdict()
}

// leafNodeSourceBindsItsPosition is RFC 9420 section 7.2's select read as the question the
// control above asks of it: does the LeafNodeTBS of a leaf with this source carry the group id
// and the leaf index.
//
// Written as the two sources the RFC names rather than as "not key_package", which is the same
// answer today and the wrong one on the day a fourth source is registered: an unrecognised
// source would inherit the unbound preimage in silence, which is the exact fall-through
// signatureContent refuses outright.
func leafNodeSourceBindsItsPosition(source LeafNodeSource) bool {
	return source == LeafNodeSourceUpdate || source == LeafNodeSourceCommit
}

// treeResolutionText renders a resolution this package computed the way the corpus spells one: a
// bracketed comma separated list of decimal node indices, with the empty resolution as [].
func treeResolutionText(resolution []NodeIndex) string {
	out := strings.Builder{}
	out.WriteByte('[')
	for at, x := range resolution {
		if at > 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.FormatUint(uint64(x), 10))
	}
	out.WriteByte(']')
	return out.String()
}

// publishedNodeIndexListText renders a resolution the corpus published, decoded through this
// runner's own struct, in the same spelling.
//
// This is the COMPARATOR's half and is deliberately not the runner's second read: the runner
// re-reads the same answer out of a generic decode of the case text through the shared
// publishedCorpusField, which touches no struct tag of this file at all, and the two are held
// equal there. A struct tag pointing at a key the corpus does not publish decodes to an empty
// column here and is a missing key there.
func publishedNodeIndexListText(published []uint32) string {
	indices := make([]NodeIndex, 0, len(published))
	for _, x := range published {
		indices = append(indices, NodeIndex(x))
	}
	return treeResolutionText(indices)
}

// Family 10 is installed here, and 10 is deleted from expectedPendingFamilies in the same
// commit. Without both halves TestVectorFamiliesVerify runs one fewer family and the manifest
// gate stays green while claiming this family is unimplemented.
//
// Generate is nil, and it is asserted as an absence rather than left unmentioned. A
// tree-validation case is a serialized ratchet tree together with the resolutions and tree
// hashes of every node of it, and this package can produce all three -- but the tree would be
// one this package built, so the generated case would be this implementation's own answers fed
// back to itself, which closes no loop the published corpus does not already close. Family 11
// next door is the one with a generate direction worth having, because there the sender and the
// receiver are two different code paths.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   10,
		Name:     "Tree validation",
		File:     treeValidationVectorFile,
		Slice:    "A2",
		Verify:   verifyTreeValidationVector,
		Generate: nil,
	})
}

// TestVectorTreeValidation is vector family 10 over the published corpus.
//
// Every assertion the tally makes after the loop exists because the loop can be made to run zero
// times without anything else in this package noticing: a filter that matched nothing, a filter
// that matched all seven published suites, a corpus that parsed to an empty array and a
// comparator that declined every case are each a green run of this test with the accounting
// removed.
//
// What the loop counts is not calls that returned. It counts comparisons whose computed half is
// re-read here against a GENERIC decode of the corpus text -- no struct tag of this file in the
// way -- so a comparator that answered without computing anything is a failure here rather than
// a number that looks right.
func TestVectorTreeValidation(t *testing.T) {
	file := treeVectorFile(t, 10)
	tally, entries := newVectorRunTally(t, file)
	nodes, leaves, bound := 0, 0, 0
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", file, index, err)
		}
		header := treeVectorHeader{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s case %d: %v", file, index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := compareTreeValidationVector(t, raw)
		if err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", file, index, header.CipherSuite, err)
		}
		// the shared refusal, handed the comparator's OWN verdict and not a tick saying it was
		// called. A comparator that declined a case the filter matched is the filter and the
		// comparator disagreeing about what runs, and the counts below would then read correct
		// over a smaller run.
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", file, index, header.CipherSuite, err)
		}
		for _, answer := range evidence.answers {
			want := publishedCorpusField(t, published, answer.field)
			if answer.got != want {
				t.Fatalf("%s case %d (suite %#04x): %s answered %s and the corpus text publishes %s at %s",
					file, index, header.CipherSuite, answer.name, answer.got, want, answer.field)
			}
			tally.answer(want)
		}
		nodes += int(evidence.nodeWidth)
		leaves += evidence.leaves
		bound += evidence.bound
	}
	tally.assertRun(t, treeValidationKatCovered, treeValidationKatSkipped,
		treeValidationKatComparisons, treeValidationKatDistinct)
	// the shape of the run under the comparison count, which the tally says nothing about: that
	// count is satisfied by 908 nodes spread any way at all, and by a sweep that verified no leaf
	// signature whatever, since a signature is a refusal rather than a published answer.
	if nodes != treeValidationKatNodes {
		t.Fatalf("%s: the covered trees carry %d nodes between them, want %d", file, nodes, treeValidationKatNodes)
	}
	if leaves != treeValidationKatLeaves {
		t.Fatalf("%s: %d published leaf signatures were verified, want %d", file, leaves, treeValidationKatLeaves)
	}
	if bound != treeValidationKatBound {
		t.Fatalf("%s: %d of those leaves bind their group id and index, want %d; without one the wrong-group control ran over nothing",
			file, bound, treeValidationKatBound)
	}
	t.Logf("%s: %d nodes over %d trees, two published answers each; %d leaf signatures verified, %d of them context bound and refused at another group",
		file, nodes, treeValidationKatCovered, leaves, bound)
}

// tvKatBaseCase answers a published tree-validation case every row of the control below can be
// built from, together with the one row that has to be built out of the tree rather than out
// of the json.
//
// Two conditions, and both are searched for rather than assumed of case zero. The tree must
// carry at least one CONTEXT BOUND leaf, because section 7.2 leaves the group id out of a
// key_package leaf's preimage and over a tree of those alone a flipped group id is accepted
// quite correctly -- a row asking for a refusal there would be asking for a defect. And it must
// hold a parent whose parent_hash field, changed in one octet, section 7.9's check refuses; the
// corpus publishes two-leaf trees whose only parent is the root, and the root's parent_hash is
// the zero-length octet string that section 7.9 gives it, so there is nothing there to spoil.
//
// The base is the corpus's own and not a fixture: the whole of what the refusals below mean is
// that this exact case is accepted and a one octet edit of it is not.
func tvKatBaseCase(t *testing.T) (treeValidationVector, json.RawMessage) {
	t.Helper()
	bound, chains := 0, 0
	for _, raw := range LoadVectorFile(t, treeVectorFile(t, 10)) {
		evidence, err := compareTreeValidationVector(t, raw)
		if err != nil {
			t.Fatalf("a published tree-validation case was refused: %v", err)
		}
		if !evidence.inScope || evidence.bound == 0 {
			continue
		}
		bound += 1
		vector := treeValidationVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("parse a tree-validation case: %v", err)
		}
		broken, found := aTreeValidationCaseWhoseParentHashChainIsBroken(t, vector)
		if !found {
			continue
		}
		chains += 1
		return vector, broken
	}
	t.Fatalf("%d published cases at a registered suite carry a context bound leaf and %d of them a parent hash this check refuses when it is changed; the rows below need one case with both",
		bound, chains)
	return treeValidationVector{}, nil
}

// rewriteTreeValidationCase re-encodes one corpus row with one thing changed.
//
// Through the row's own struct rather than through a hand edit of the json text, which is what
// makes the accepted case below and the refused ones the same shape: assertComparatorRefuses
// requires the unmodified case to be ACCEPTED first, and a base that went through this encoder
// while the corruptions did not would make that acceptance a statement about a different
// encoding.
func rewriteTreeValidationCase(t *testing.T, base treeValidationVector,
	mutate func(corrupted *treeValidationVector)) json.RawMessage {
	t.Helper()
	corrupted := base
	corrupted.Resolutions = slices.Clone(base.Resolutions)
	corrupted.TreeHashes = slices.Clone(base.TreeHashes)
	mutate(&corrupted)
	body, err := json.Marshal(corrupted)
	if err != nil {
		t.Fatalf("re-encode the corrupted tree-validation case: %v", err)
	}
	return body
}

// aTreeValidationCaseWhoseParentHashChainIsBroken is the published tree with one octet of one
// parent's parent_hash field changed, and BOTH published columns recomputed over the result.
//
// The columns are recomputed rather than carried over, and that is what aims this row at the
// class it names. A node's parent_hash field is inside the tree hash preimage of that node, so a
// case that kept the published tree_hashes would be refused as a tree hash mismatch --
// correctly, and for a reason that says nothing about section 7.9.2.
//
// WHICH parent is found rather than chosen: the first whose flipped field the check refuses.
// A tree in which no such node exists answers false and its caller moves to the next published
// case -- a two-leaf tree has only the root, whose parent_hash is the zero-length octet string
// -- and a caller that ran out of cases fails there. A VerifyParentHashes answering nil for
// everything lands in exactly that arm: no tree of the corpus produces a refusal, and the
// search reports having found none.
func aTreeValidationCaseWhoseParentHashChainIsBroken(t *testing.T, base treeValidationVector) (json.RawMessage, bool) {
	t.Helper()
	suite, ok := implementedSuite(base.CipherSuite)
	if !ok {
		t.Fatalf("the base case is at suite %#04x, which this package does not register", base.CipherSuite)
	}
	crypto := mustProvider(t, suite)
	wire := MustHex(t, base.Tree)
	probe, err := UnmarshalRatchetTree(wire)
	if err != nil {
		t.Fatalf("decode the base case's ratchet tree: %v", err)
	}
	for x := uint32(1); x < probe.NodeWidth(); x += 2 {
		tree, err := UnmarshalRatchetTree(wire)
		if err != nil {
			t.Fatalf("decode the base case's ratchet tree: %v", err)
		}
		parent := tree.ParentAt(NodeIndex(x))
		if parent == nil || len(parent.ParentHash) == 0 {
			continue
		}
		parent.ParentHash[0] ^= 0x01
		if tree.VerifyParentHashes(crypto) == nil {
			continue
		}
		encoded, err := syntax.Marshal(tree)
		if err != nil {
			t.Fatalf("re-encode the corrupted ratchet tree: %v", err)
		}
		hashes, err := tree.TreeHashes(crypto)
		if err != nil {
			t.Fatalf("TreeHashes over the corrupted ratchet tree: %v", err)
		}
		return rewriteTreeValidationCase(t, base, func(corrupted *treeValidationVector) {
			corrupted.Tree = HexOf(encoded)
			corrupted.TreeHashes = make([]string, 0, len(hashes))
			for _, hash := range hashes {
				corrupted.TreeHashes = append(corrupted.TreeHashes, HexOf(hash))
			}
			corrupted.Resolutions = make([][]uint32, 0, tree.NodeWidth())
			for y := uint32(0); y < tree.NodeWidth(); y += 1 {
				list := []uint32{}
				for _, index := range tree.Resolution(NodeIndex(y)) {
					list = append(list, uint32(index))
				}
				corrupted.Resolutions = append(corrupted.Resolutions, list)
			}
		}), true
	}
	return nil, false
}

// TestCompareTreeValidationVectorRefusesAnAnswerItShouldNotAccept is the control the runner
// cannot be.
//
// Every case of the vendored corpus agrees with this implementation, so a comparator that
// checked all of its answers and one that returned an empty struct produce identical runs above;
// the only way to separate them is to hand it a case that is wrong on purpose, once per defect
// class, and require the matching refusal. Each row names the sentinel it owes, so a refusal for
// the WRONG reason is reported too, and assertComparatorRefuses requires the unmodified case to
// be accepted first so a comparator that refused everything does not satisfy the table.
func TestCompareTreeValidationVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	accepted, refusals := treeValidationRefusals(t)
	assertComparatorRefuses(t, treeVectorFile(t, 10), compareTreeValidationVerdict, accepted, refusals)
}

// compareTreeValidationVerdict is one run of family 10's comparator reported as a single error,
// which is the shape assertComparatorRefuses and the class gate below both drive it through.
//
// One declaration rather than a closure inside each caller: two callers building the same
// adapter is two chances for one of them to judge the evidence differently from the other, and
// the whole of what a refusal row means is that the comparator and the verdict together said no.
func compareTreeValidationVerdict(t *testing.T, raw json.RawMessage) error {
	t.Helper()
	evidence, err := compareTreeValidationVector(t, raw)
	if err != nil {
		return err
	}
	return evidence.verdict()
}

// treeValidationRefusals is family 10's control table: the unmodified published case every row
// is judged against, and one deliberately wrong case per defect class this family's comparator
// and verdict can reach from a corpus.
//
// A function rather than a table declared inside the test that drives it, because the class gate
// below has to read the sentinels these rows ACTUALLY name. A second list of those sentinels
// written beside the table would be the shape rule 5 is about: it would agree with itself while
// the table under it lost a row.
func treeValidationRefusals(t *testing.T) (json.RawMessage, []comparatorRefusal) {
	t.Helper()
	base, brokenChain := tvKatBaseCase(t)
	flipHex := func(text string) string {
		octets := MustHex(t, text)
		if len(octets) == 0 {
			t.Fatalf("nothing to flip in %q", text)
		}
		octets[0] ^= 0x01
		return HexOf(octets)
	}
	// the LAST octet, which is the one the comparator's wrong-group question does not touch.
	// That question flips octet zero, so a case whose group id differs from its leaves' THERE has
	// its original group id offered back to every bound leaf and is refused as the unbound class;
	// a case that differs anywhere else is refused as the leaf class and nothing else. The two
	// rows below are those two cases, and they are what makes both arms of the verdict reachable
	// from a corpus rather than only from evidence built by hand.
	flipLastHex := func(text string) string {
		octets := MustHex(t, text)
		if len(octets) < 2 {
			t.Fatalf("%q is %d octets and the two group id rows below need a first and a last that differ",
				text, len(octets))
		}
		octets[len(octets)-1] ^= 0x01
		return HexOf(octets)
	}
	if len(base.Resolutions) < 2 || len(base.TreeHashes) < 2 {
		t.Fatalf("the base case publishes %d resolutions and %d tree hashes, and the rows below need two of each",
			len(base.Resolutions), len(base.TreeHashes))
	}
	accepted := rewriteTreeValidationCase(t, base, func(corrupted *treeValidationVector) {})
	return accepted, []comparatorRefusal{
		{
			name:   "a case whose published ratchet tree does not decode",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) { c.Tree = "00" }),
			want:   errTreeValidationUndecodable,
		},
		{
			name: "a case publishing one resolution fewer than the tree has nodes",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) {
				c.Resolutions = c.Resolutions[:len(c.Resolutions)-1]
			}),
			want: errTreeValidationColumnWidth,
		},
		{
			name: "a case publishing one tree hash fewer than the tree has nodes",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) {
				c.TreeHashes = c.TreeHashes[:len(c.TreeHashes)-1]
			}),
			want: errTreeValidationColumnWidth,
		},
		{
			name: "a case whose first two published resolutions are transposed",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) {
				c.Resolutions[0], c.Resolutions[1] = c.Resolutions[1], c.Resolutions[0]
			}),
			want: errTreeValidationResolution,
		},
		{
			name: "a case with one octet of one published tree hash changed",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) {
				c.TreeHashes[0] = flipHex(c.TreeHashes[0])
			}),
			want: errTreeValidationTreeHash,
		},
		{
			name:   "a case whose ratchet tree fails the section 7.9 parent hash check",
			vector: brokenChain,
			want:   errTreeValidationParentHash,
		},
		{
			name: "a case whose group id is not its leaves own, in an octet the wrong-group question does not flip",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) {
				c.GroupId = flipLastHex(c.GroupId)
			}),
			want: errTreeValidationLeafRefused,
		},
		{
			// the row the wrong-group question exists for, and the only one that reaches it from
			// a corpus: this case's group id is its leaves own with octet zero flipped, so the
			// question hands every bound leaf the group id it really was signed under and the
			// signature verifies there. A comparator that stopped asking reports this case as the
			// row above's class instead, which is a refusal for the wrong reason.
			name: "a case whose group id differs from its leaves own in exactly the octet the wrong-group question flips",
			vector: rewriteTreeValidationCase(t, base, func(c *treeValidationVector) {
				c.GroupId = flipHex(c.GroupId)
			}),
			want: errTreeValidationLeafUnbound,
		},
	}
}

// treeValidationSentinels is this family's refusals addressed by the identifier they are
// declared under, so a class derived from the source can be turned back into the value a control
// row carries.
//
// A map written out here is a list, and a list is the shape rule 5 is about -- which is why the
// gate below does not read it AS the class. The class is the identifiers the verdict names; this
// only resolves one of those identifiers to its value, and an identifier missing from here is a
// failure rather than a row quietly skipped.
var treeValidationSentinels = map[string]error{
	"errTreeValidationIncomplete":  errTreeValidationIncomplete,
	"errTreeValidationUndecodable": errTreeValidationUndecodable,
	"errTreeValidationColumnWidth": errTreeValidationColumnWidth,
	"errTreeValidationResolution":  errTreeValidationResolution,
	"errTreeValidationTreeHash":    errTreeValidationTreeHash,
	"errTreeValidationParentHash":  errTreeValidationParentHash,
	"errTreeValidationLeafRefused": errTreeValidationLeafRefused,
	"errTreeValidationLeafUnbound": errTreeValidationLeafUnbound,
}

// TestTreeValidationVerdictReportsEveryClassItNames drives the verdict over evidence built here
// rather than over the corpus.
//
// Two of the classes the verdict names cannot be reached from a corpus at all. A comparison that
// is INCOMPLETE is one whose comparator returned early, and no corpus case can ask for that; a
// context bound leaf that verifies at a group id it was not signed under is a DEFECT in
// VerifySignature, and no corpus can publish one. Both are the arms that hold the sweep above
// honest -- without the first a comparator answering an empty struct passes, without the second
// a VerifySignature answering nil for everything passes -- so both are driven here.
//
// The class is DERIVED from the two methods' own source and not listed: every errTreeValidation
// sentinel either of them names owes exactly one row, so an arm added to the verdict without a
// row fails here rather than going unreached. The two the COMPARATOR names and the verdict does
// not -- the undecodable tree and the column widths -- are refused before a comparison exists to
// judge, and are the refusal table's above.
func TestTreeValidationVerdictReportsEveryClassItNames(t *testing.T) {
	complete := func() treeValidationComparison {
		return treeValidationComparison{
			inScope:          true,
			hashSize:         32,
			nodeWidth:        3,
			leafWidth:        2,
			publishesGroupId: true,
			groupId:          []byte{0x01, 0x02},
			answers: []treeValidationAnswer{
				{name: "Resolution", field: "resolutions.0", got: "[0]", want: "[0]"},
				{name: "TreeHashes", field: "tree_hashes.0", got: "aa", want: "aa"},
				{name: "Resolution", field: "resolutions.1", got: "[0,2]", want: "[0,2]"},
				{name: "TreeHashes", field: "tree_hashes.1", got: "bb", want: "bb"},
				{name: "Resolution", field: "resolutions.2", got: "[2]", want: "[2]"},
				{name: "TreeHashes", field: "tree_hashes.2", got: "cc", want: "cc"},
			},
			leaves: 2,
			blanks: 0,
			bound:  2,
		}
	}
	// the baseline, and it is what makes every row below mean anything: a verdict that refused
	// everything would satisfy the whole table.
	if err := complete().verdict(); err != nil {
		t.Fatalf("a complete and agreeing comparison was refused: %v", err)
	}
	rows := []struct {
		// names is the sentinel this row must be refused as.
		names error
		// spoil makes exactly one thing wrong in a comparison that would otherwise pass.
		spoil func(evidence *treeValidationComparison)
	}{
		{errTreeValidationIncomplete, func(e *treeValidationComparison) { e.answers = nil }},
		{errTreeValidationResolution, func(e *treeValidationComparison) { e.answers[2].got = "[2,0]" }},
		{errTreeValidationTreeHash, func(e *treeValidationComparison) { e.answers[3].got = "dd" }},
		{errTreeValidationParentHash, func(e *treeValidationComparison) {
			e.parentHashError = errors.New("a copath child claimed nothing")
		}},
		{errTreeValidationLeafRefused, func(e *treeValidationComparison) { e.refusedLeaf = errors.New("leaf 1") }},
		{errTreeValidationLeafUnbound, func(e *treeValidationComparison) { e.acceptedLeaf = errors.New("leaf 1") }},
	}
	claimed := map[string]bool{}
	for _, row := range rows {
		if row.names == nil {
			t.Fatal("a row names no sentinel, so any refusal at all would satisfy it")
		}
		found := ""
		for name, sentinel := range treeValidationSentinels {
			if sentinel == row.names {
				found = name
			}
		}
		if found == "" {
			t.Fatalf("a row names %v and treeValidationSentinels resolves no identifier to it", row.names)
		}
		if claimed[found] {
			t.Fatalf("%s is claimed by two rows, so some class this verdict names is claimed by none", found)
		}
		claimed[found] = true
	}
	// the class, read off the two methods rather than listed here.
	declared := theSourceDeclaring(t, "treeValidationComparison", "verdict")
	mentioned := map[string]bool{}
	for _, method := range []string{"verdict", "incomplete"} {
		for name := range namesMentionedIn(declared.declarationOf(t, "treeValidationComparison", method)) {
			if strings.HasPrefix(name, "errTreeValidation") {
				mentioned[name] = true
			}
		}
	}
	if len(mentioned) == 0 {
		t.Fatal("the verdict and its completeness check name no errTreeValidation sentinel at all, so the class below was derived from nothing")
	}
	if len(mentioned) != len(rows) {
		t.Fatalf("the verdict names %v and this control offers %d rows for %v",
			slices.Sorted(maps.Keys(mentioned)), len(rows), slices.Sorted(maps.Keys(claimed)))
	}
	for _, name := range slices.Sorted(maps.Keys(mentioned)) {
		if _, known := treeValidationSentinels[name]; !known {
			t.Errorf("%s is a class the verdict reports and treeValidationSentinels does not resolve it", name)
			continue
		}
		if !claimed[name] {
			t.Errorf("%s is a class the verdict reports and no row here drives it", name)
		}
	}
	for _, row := range rows {
		evidence := complete()
		row.spoil(&evidence)
		err := evidence.verdict()
		if err == nil {
			t.Errorf("%v: the spoiled comparison was accepted, so this arm of the verdict reports nothing", row.names)
			continue
		}
		if !errors.Is(err, row.names) {
			t.Errorf("the spoiled comparison was refused as %v, want %v; a refusal for the wrong reason is the verdict checking something else",
				err, row.names)
		}
	}
}

// treeVectorInstalledRunners is the Verify and the Generate this file installs, keyed by family
// number. A family this file gates and has NOT installed yet has no row here.
//
// The identity is the point: assertVectorFamilyIsInstalled holds the registered function to the
// one this table names, so a manifest row that kept its number and picked up some other family's
// runner fails rather than reading as installed. Which families are installed is deliberately
// NOT read off this table -- it is read off the manifest -- so a table that forgot a family and a
// manifest holding a runner nothing here names are two different failures with two different
// messages, rather than one table agreeing with itself.
var treeVectorInstalledRunners = map[int]struct {
	verify   func(t *testing.T, raw json.RawMessage)
	generate func(t *testing.T) json.RawMessage
}{
	10: {verify: verifyTreeValidationVector},
	11: {verify: verifyTreeKemVector, generate: generateTreeKemVectors},
}

// TestTreeVectorFamiliesAreInstalledOrPending is the registration half both of this file's
// families owe, in whichever of the two states each of them is in.
//
// It replaces the earlier assertion that NEITHER family was installed, which could only ever be
// deleted. The two halves of installing a family are two edits -- register the runner, delete the
// number from expectedPendingFamilies -- and either one alone is silent in the worst direction:
// the first without the second leaves the manifest gate failing for a reason nobody wrote down,
// and the second without the first leaves TestVectorFamiliesVerify running one family fewer with
// the gate green.
//
// So both states are held, and which state a family is IN is read off the manifest rather than
// off this file. A runner that stopped being linked -- an init() dropped, a build tag -- takes
// its family back to pending, and this fails there rather than passing over a family nothing
// runs.
func TestTreeVectorFamiliesAreInstalledOrPending(t *testing.T) {
	if len(treeVectorFamilies) == 0 {
		t.Fatal("this plan claims no vector family, so the loop below reads nothing")
	}
	installed := 0
	for _, number := range treeVectorFamilies {
		family, ok := vectorManifest[number]
		if !ok {
			t.Fatalf("family %d is not in the manifest", number)
		}
		runner, named := treeVectorInstalledRunners[number]
		switch {
		case family.Verify == nil && named:
			t.Errorf("this file names a runner for family %d (%s) and the registry holds none, so the init() that registers it is not linked",
				number, family.Name)
		case family.Verify != nil && !named:
			t.Errorf("family %d (%s) is registered with a verifier this file does not name, so nothing holds the installed function to this runner's",
				number, family.Name)
		case family.Verify == nil:
			if !slices.Contains(expectedPendingFamilies, number) {
				t.Errorf("family %d (%s) is no longer listed as pending and this file installs no runner for it",
					number, family.Name)
			}
		default:
			installed += 1
			assertVectorFamilyIsInstalled(t, number, treeVectorFamilyFiles[number], runner.verify, runner.generate)
		}
	}
	if installed != len(treeVectorInstalledRunners) {
		t.Fatalf("%d of the %d runners this file names were held to the registry; the rest were reported above",
			installed, len(treeVectorInstalledRunners))
	}
	t.Logf("%d of this plan's %d families are installed and %d are still pending",
		installed, len(treeVectorFamilies), len(treeVectorFamilies)-installed)
}

// TestTreeVectorFamilyFilesAreTheRegistrysFiles holds the two corpus basenames this file spells
// to the ones the manifest names.
//
// A constant is needed at all because init() runs before any test does and cannot ask a
// *testing.T for the manifest row. That constant is then a second spelling of a fact the
// registry already holds, and the failure a second spelling produces is the quiet one: a family
// repointed at another corpus in vectors_test.go would leave this file's runner reading the old
// file, comparing its answers perfectly, and reporting a clean run for a family that reads
// something else.
func TestTreeVectorFamilyFilesAreTheRegistrysFiles(t *testing.T) {
	if len(treeVectorFamilyFiles) != len(treeVectorFamilies) {
		t.Fatalf("this file spells %d corpus names and gates %d families", len(treeVectorFamilyFiles), len(treeVectorFamilies))
	}
	for _, number := range treeVectorFamilies {
		spelled, named := treeVectorFamilyFiles[number]
		if !named {
			t.Errorf("family %d is gated by this file and no corpus name is spelled for it", number)
			continue
		}
		if got := treeVectorFile(t, number); got != spelled {
			t.Errorf("family %d is registered against %s and this file spells %s", number, got, spelled)
		}
	}
}

// ---------------------------------------------------------------------------
// task 25: family 11, TreeKEM, both directions
// ---------------------------------------------------------------------------

// The accounting that makes family 11's runner unable to pass having compared nothing.
//
// Transcriptions of what testdata/vectors/treekem.json holds at the pinned mlswg commit,
// counted off the corpus text with a json reader and not off this implementation: 77 cases, 11
// at each of the seven published ciphersuites, of which the two this package registers account
// for 22. Those 22 cases publish 124 update paths between them, and 656 (path, leaf) pairs where
// the file says that leaf can decrypt.
//
// Three answers are compared: the tree hash after the path is merged, once per path; and the
// path secret the leaf recovers and the commit secret the epoch reaches, once per decrypting
// leaf. So 124 + 2*656 = 1436 comparisons against 94 distinct published answers.
//
// 94 is a small number against 1436 and it is the corpus's own property rather than a defect in
// this run: the two registered suites are both HKDF-SHA256 over X25519 and Ed25519 and the mlswg
// generator seeds them identically, so every answer at suite 0x0001 is the same octet string as
// the one at 0x0003 -- 62 distinct tree hashes for 124 paths, exactly half. The generator also
// reuses one group's secrets across the entries of growing size, which is why 62 paths carry
// only 19 distinct commit secrets and 656 published path secrets only 24 distinct values; and 11
// of the 24 are also published as a commit secret somewhere, which is why the union is 94 rather
// than 105. The number is checked because a corpus read as ONE repeated value compares the right
// number of times against one answer, and 94 separates that from this.
const (
	treeKemKatCovered     = 22
	treeKemKatSkipped     = 55
	treeKemKatPaths       = 124
	treeKemKatDecrypts    = 656
	treeKemKatComparisons = treeKemKatPaths + 2*treeKemKatDecrypts
	treeKemKatDistinct    = 94
	// the published leaf private states those 22 cases carry, one per member of each epoch.
	treeKemKatPrivateStates = 124
	// how many of the 656 decrypts enter the sender's path ABOVE the receiver's own leaf, so
	// that the receiver opens a ciphertext addressed to an ancestor and ratchets down to it.
	// A structural property of the corpus's tree shapes and not of any secret, so it is stable
	// across runs -- and it is pinned rather than merely required non-zero, because a receiver
	// that had lost the held-path-secret arm entirely would still enter above its own leaf for
	// SOME case and satisfy a floor of one.
	treeKemKatDeep = 476
)

// The refusals compareTreeKemVector makes. Sentinels rather than formatted strings, so a control
// can require a SPECIFIC refusal and a refusal for the wrong reason is reported too.
var (
	errTreeKemIncomplete     = errors.New("the treekem comparison reports answers it cannot have computed")
	errTreeKemTreeUndecoded  = errors.New("the published ratchet tree does not decode")
	errTreeKemPathUndecoded  = errors.New("a published update path does not decode")
	errTreeKemPrivateState   = errors.New("a published leaf private state does not agree with the published tree")
	errTreeKemMerge          = errors.New("a published update path does not merge into the published tree")
	errTreeKemTreeHash       = errors.New("the merged tree's hash is not the published tree_hash_after")
	errTreeKemParentHash     = errors.New("the merged tree fails the section 7.9 parent hash check")
	errTreeKemMissingPrivate = errors.New("the case publishes a path secret for a leaf it publishes no private state for")
	errTreeKemDecrypt        = errors.New("a leaf the case says can decrypt did not")
	errTreeKemPathSecret     = errors.New("the recovered path secret is not the published one")
	errTreeKemCommitSecret   = errors.New("the recovered commit secret is not the published one")
	errTreeKemContextIgnored = errors.New("an update path opened under a group context that is not the case's own")
)

// The three answers this family compares, named by what the corpus publishes them as. The order
// is the order the comparator emits them in and incomplete() rebuilds that order from the run's
// own shape and holds it position by position.
const (
	treeKemAnswerTreeHash     = "tree_hash_after"
	treeKemAnswerPathSecret   = "path_secret"
	treeKemAnswerCommitSecret = "commit_secret"
)

// treeKemAnswer is one answer this package computed held against one answer the corpus published,
// as hex, together with the dotted json path the runner re-reads the published half at.
type treeKemAnswer struct {
	name  string
	field string
	got   string
	want  string
}

// treeKemComparison is what one run of compareTreeKemVector PRODUCED.
//
// The same shape family 10 next door has and for the same reason: a comparator that returns
// nothing lets its caller count calls, and a call that returned is not a comparison that
// happened. Every field is written where the work that produces it happens.
type treeKemComparison struct {
	// inScope is true when the case's ciphersuite is one this package registers.
	inScope bool
	// hashSize is the suite's KDF.Nh, read off the provider.
	hashSize int
	// leafWidth is the decoded tree's own width.
	leafWidth uint32
	// privateStates is how many published leaf private states were checked against the
	// published tree before any path was touched.
	privateStates int
	// perPath is how many leaves decrypted each path, in path order. The SHAPE of the run, from
	// which incomplete() rebuilds the answer order: a comparison dropped from the middle and
	// another made twice leave the total untouched.
	perPath []int
	// deep counts the decrypts whose entry point into the path is ABOVE the receiver's own
	// leaf, which is the arm a receiver that only ever opened its own leaf's ciphertext never
	// reaches.
	deep int
	// refusals is how many decrypts were also offered the same path under a group context one
	// octet different and refused it. One per decrypt, or the wrong-context control ran over
	// fewer cases than the run.
	refusals int
	// answers is every comparison the run made, in emit order.
	answers []treeKemAnswer
	// parentHashError is what section 7.9's whole-tree check answered over the merged tree.
	parentHashError error
}

// incomplete reports whether the evidence a compared case must carry is missing or inconsistent,
// without looking at whether any answer was right.
func (self treeKemComparison) incomplete() error {
	switch {
	case !self.inScope:
		return fmt.Errorf("%w: the case is out of scope and carries no comparison", errTreeKemIncomplete)
	case self.hashSize == 0:
		return fmt.Errorf("%w: no KDF.Nh was read from the provider", errTreeKemIncomplete)
	case self.leafWidth == 0:
		return fmt.Errorf("%w: the decoded tree is %d leaves wide", errTreeKemIncomplete, self.leafWidth)
	case self.privateStates == 0:
		return fmt.Errorf("%w: the case publishes no leaf private state, so nothing decrypted anything",
			errTreeKemIncomplete)
	case len(self.perPath) == 0:
		return fmt.Errorf("%w: the case publishes no update path", errTreeKemIncomplete)
	}
	// the answer order, rebuilt from the run's own shape: one tree hash per path, then a path
	// secret and a commit secret per leaf that decrypted it.
	expected := []string{}
	decrypts := 0
	for at, opened := range self.perPath {
		if opened == 0 {
			return fmt.Errorf("%w: no leaf decrypted path %d, so this path contributed one tree hash and nothing else",
				errTreeKemIncomplete, at)
		}
		decrypts += opened
		expected = append(expected, treeKemAnswerTreeHash)
		for i := 0; i < opened; i += 1 {
			expected = append(expected, treeKemAnswerPathSecret, treeKemAnswerCommitSecret)
		}
	}
	if len(self.answers) != len(expected) {
		return fmt.Errorf("%w: the run made %d comparisons over %d paths and %d decrypts, which owe %d",
			errTreeKemIncomplete, len(self.answers), len(self.perPath), decrypts, len(expected))
	}
	for at, answer := range self.answers {
		if answer.name != expected[at] {
			return fmt.Errorf("%w: comparison %d is %s and the shape of this run puts %s there",
				errTreeKemIncomplete, at, answer.name, expected[at])
		}
		if answer.got == "" || answer.want == "" {
			return fmt.Errorf("%w: %s at %s compared %q against %q, and an empty comparison agrees with anything",
				errTreeKemIncomplete, answer.name, answer.field, answer.got, answer.want)
		}
		if answer.field == "" {
			return fmt.Errorf("%w: %s names no published field, so nothing independent of this comparator's own decode can re-read it",
				errTreeKemIncomplete, answer.name)
		}
	}
	if self.refusals != decrypts {
		return fmt.Errorf("%w: %d of %d decrypts were also offered the path under a group context one octet different",
			errTreeKemIncomplete, self.refusals, decrypts)
	}
	return nil
}

// verdict is the whole judgement over one compared case.
//
// The tree hash comes ahead of the secrets because it is the input the secrets were sealed
// under: the group context every ciphertext of a path is opened with carries the tree hash of the
// tree AFTER that path is merged, so a merge that landed anywhere else makes every decrypt below
// fail, and reporting those failures first would hide the one thing that moved.
func (self treeKemComparison) verdict() error {
	if err := self.incomplete(); err != nil {
		return err
	}
	for _, answer := range self.answers {
		if answer.got == answer.want {
			continue
		}
		sentinel := errTreeKemTreeHash
		switch answer.name {
		case treeKemAnswerPathSecret:
			sentinel = errTreeKemPathSecret
		case treeKemAnswerCommitSecret:
			sentinel = errTreeKemCommitSecret
		}
		return fmt.Errorf("%w: %s answered %s and the corpus publishes %s at %s",
			sentinel, answer.name, answer.got, answer.want, answer.field)
	}
	if self.parentHashError != nil {
		return fmt.Errorf("%w: %v", errTreeKemParentHash, self.parentHashError)
	}
	return nil
}

// verifyTreeKemVector is the registry's shim: the signature RegisterVectorFamily needs, over the
// comparator that does the work and reports what it produced.
func verifyTreeKemVector(t *testing.T, raw json.RawMessage) {
	t.Helper()
	evidence, err := compareTreeKemVector(t, raw)
	if err != nil {
		t.Fatalf("treekem: %v", err)
	}
	if !evidence.inScope {
		return
	}
	if err := evidence.verdict(); err != nil {
		t.Fatalf("treekem: %v", err)
	}
}

// compareTreeKemVector runs one case of treekem.json and returns what the run produced. A case at
// a ciphersuite this package does not register comes back with inScope false and nothing else
// set, which is not a failure and not a skip.
//
// The receiver is driven exactly as a member processing a commit drives it and in that order:
// merge the path into the tree, take the tree hash of the RESULT, build the group context over
// that hash, and only then open. A caller that decrypted first would be checking a path against
// the epoch it closed, and a context built over the tree hash the epoch STARTED from is one every
// other implementation computes differently -- which nothing in a self consistent seal and open
// can see, and is why the corpus is where it has to be said.
func compareTreeKemVector(t *testing.T, raw json.RawMessage) (treeKemComparison, error) {
	t.Helper()
	// treekemReceiverVector is treekem_test.go's row for this same corpus, with its groupContext
	// and private helpers, reused rather than declared again: task 21 landed that decode and a
	// second one here would be two readings of one file, silent in the worst direction when they
	// disagree about a json key.
	vector := treekemReceiverVector{}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parse treekem case: %v", err)
	}
	suite, ok := implementedSuite(vector.CipherSuite)
	if !ok {
		return treeKemComparison{}, nil
	}
	crypto := mustProvider(t, suite)
	evidence := treeKemComparison{inScope: true, hashSize: crypto.HashSize()}

	base, err := UnmarshalRatchetTree(MustHex(t, vector.RatchetTree))
	if err != nil {
		return evidence, fmt.Errorf("%w: %v", errTreeKemTreeUndecoded, err)
	}
	evidence.leafWidth = uint32(base.LeafWidth())

	// the supplied private state must already agree with the supplied tree, before any path is
	// merged. Without this a case whose private half was replaced wholesale would be reported as
	// a decrypt that failed, which sends a reader to the ladder rather than to the state.
	for _, entry := range vector.LeavesPrivate {
		// a HARNESS invariant and not a verdict about the case, which is why it is fatal rather
		// than a refusal. private searches the very slice this loop walks, so the miss it reports
		// is unreachable here; the class errTreeKemMissingPrivate names -- a path secret published
		// for a leaf the case carries no private state for -- is reachable only in the decrypt
		// loop below, where the leaf index comes off the update path instead. Written as a refusal
		// this read as a checked condition and was not one.
		priv, found := vector.private(t, entry.Index)
		if !found {
			t.Fatalf("leaf %d is published in the case's own private state and the lookup over that same slice does not find it",
				entry.Index)
		}
		if err := priv.Consistent(crypto, base); err != nil {
			return evidence, fmt.Errorf("%w: leaf %d: %v", errTreeKemPrivateState, entry.Index, err)
		}
		evidence.privateStates += 1
	}

	pathsKey := theJsonKeyOf(t, treekemReceiverVector{}, "UpdatePaths")
	hashKey := theJsonKeyOf(t, treekemReceivedUpdatePath{}, "TreeHashAfter")
	secretsKey := theJsonKeyOf(t, treekemReceivedUpdatePath{}, "PathSecrets")
	commitKey := theJsonKeyOf(t, treekemReceivedUpdatePath{}, "CommitSecret")

	for at, update := range vector.UpdatePaths {
		path := &UpdatePath{}
		if err := syntax.Unmarshal(MustHex(t, update.UpdatePath), path); err != nil {
			return evidence, fmt.Errorf("%w: path %d: %v", errTreeKemPathUndecoded, at, err)
		}
		merged := base.Clone()
		if err := merged.MergeUpdatePath(crypto, LeafIndex(update.Sender), path); err != nil {
			return evidence, fmt.Errorf("%w: path %d from leaf %d: %v", errTreeKemMerge, at, update.Sender, err)
		}
		treeHash, err := merged.TreeHash(crypto)
		if err != nil {
			return evidence, fmt.Errorf("path %d TreeHash: %w", at, err)
		}
		evidence.answers = append(evidence.answers, treeKemAnswer{
			name:  treeKemAnswerTreeHash,
			field: fmt.Sprintf("%s.%d.%s", pathsKey, at, hashKey),
			got:   HexOf(treeHash),
			want:  update.TreeHashAfter,
		})
		if evidence.parentHashError == nil {
			evidence.parentHashError = merged.VerifyParentHashes(crypto)
		}
		groupContext := vector.groupContext(t, treeHash)
		opened := 0
		for leafIndex, wantSecret := range update.PathSecrets {
			if wantSecret == nil {
				continue
			}
			priv, found := vector.private(t, uint32(leafIndex))
			if !found {
				return evidence, fmt.Errorf("%w: path %d, leaf %d", errTreeKemMissingPrivate, at, leafIndex)
			}
			// WHERE this member enters the path, taken from the tree math before the decrypt
			// rather than from the decrypt's own answer. It is the classification the deep
			// counter is about, and it is not the common ancestor: the common ancestor of two
			// distinct leaves is always a parent, so a counter written over that is true for
			// every case and counts nothing. updatePathEntryFor is treekem_test.go's, reused
			// rather than copied.
			_, entry, _, entered := updatePathEntryFor(t, crypto, merged, LeafIndex(update.Sender), priv, nil)
			if !entered {
				return evidence, fmt.Errorf("%w: path %d, leaf %d: the case says this leaf decrypts and the tree math finds it no entry point",
					errTreeKemDecrypt, at, leafIndex)
			}
			if entry != LeafIndex(leafIndex).NodeIndex() {
				evidence.deep += 1
			}
			// the control, per decrypt rather than once, and AHEAD of the decrypt that is meant to
			// succeed. Every published case opens, so a DecryptUpdatePath that checked the context
			// and one that ignored it produce identical runs, and only an input that must be
			// refused separates them. The ORDER is what makes that input expressible: a case whose
			// path was sealed under some other context refuses the real decrypt too, so a probe
			// that ran second would never be reached and this class would be reportable only over
			// evidence built by hand. Measured: with the probe's refusal deleted outright the whole
			// package stayed green.
			wrongContext := bytes.Clone(groupContext)
			wrongContext[0] ^= 0x01
			fresh, _ := vector.private(t, uint32(leafIndex))
			if _, err := merged.DecryptUpdatePath(crypto, LeafIndex(update.Sender), path,
				wrongContext, fresh, nil); err == nil {
				return evidence, fmt.Errorf("%w: path %d, leaf %d", errTreeKemContextIgnored, at, leafIndex)
			}
			evidence.refusals += 1

			result, err := merged.DecryptUpdatePath(crypto, LeafIndex(update.Sender), path,
				groupContext, priv, nil)
			if err != nil {
				return evidence, fmt.Errorf("%w: path %d, leaf %d: %v", errTreeKemDecrypt, at, leafIndex, err)
			}
			// the node the recovered secret belongs at is COMPUTED and not searched for: it is
			// the lowest node of the sender's filtered direct path that covers this leaf, which
			// is the common ancestor of the two. A run that took whatever secret the state ended
			// up holding would agree with a receiver that entered the path at the wrong rung.
			lowest := CommonAncestor(LeafIndex(update.Sender).NodeIndex(), LeafIndex(leafIndex).NodeIndex())
			evidence.answers = append(evidence.answers,
				treeKemAnswer{
					name:  treeKemAnswerPathSecret,
					field: fmt.Sprintf("%s.%d.%s.%d", pathsKey, at, secretsKey, leafIndex),
					got:   HexOf(result.Private.PathSecrets[lowest]),
					want:  *wantSecret,
				},
				treeKemAnswer{
					name:  treeKemAnswerCommitSecret,
					field: fmt.Sprintf("%s.%d.%s", pathsKey, at, commitKey),
					got:   HexOf(result.CommitSecret),
					want:  update.CommitSecret,
				})
			opened += 1
		}
		evidence.perPath = append(evidence.perPath, opened)
	}

	return evidence, evidence.verdict()
}

// Family 11 is installed here with BOTH directions, and 11 is deleted from
// expectedPendingFamilies in the same commit.
//
// Generate is not nil for this family and is not decoration. A treekem case is a tree, an update
// path over it and the secrets that path delivers, and the sender and the receiver are two
// different code paths in this package -- CreateUpdatePathSecrets and EncryptUpdatePath on one
// side, MergeUpdatePath and DecryptUpdatePath on the other -- so a generated case fed back
// through the installed verifier closes a loop the published corpus cannot: the corpus never
// passes through this package's ENCODER, so an encoder and a decoder wrong in the same direction
// verify perfectly against it.
//
// A generate/consume pair sharing a code path would prove only that the code agrees with itself,
// so the generator carries one answer that came from neither side: the commit secret is
// re-derived from RFC 9420 section 7.4 with crypto/hmac through independentDeriveSecret, which is
// the single hand written expander this package's tests have.
func init() {
	RegisterVectorFamily(VectorFamily{
		Number:   11,
		Name:     "TreeKEM",
		File:     treekemVectorFile,
		Slice:    "A4",
		Verify:   verifyTreeKemVector,
		Generate: generateTreeKemVectors,
	})
}

// TestVectorTreeKEM is vector family 11 over the published corpus, in the verify direction.
//
// The accounting after the loop is what stops a run that compared nothing from reporting what a
// run that compared everything reports, and every one of its assertions is reachable: a filter
// that matched nothing, a corpus that parsed to an empty array, a comparator that declined every
// case and a comparator that answered without computing are each a green run of this test with it
// removed.
//
// The published half of every comparison is re-read here out of a GENERIC decode of the case
// text, addressed by the dotted path the comparator recorded, so a struct tag of treekem_test.go
// pointing at a key this corpus does not publish is a failure here rather than an empty string
// compared against an empty string.
func TestVectorTreeKEM(t *testing.T) {
	file := treeVectorFile(t, 11)
	tally, entries := newVectorRunTally(t, file)
	paths, decrypts, deep, states := 0, 0, 0, 0
	for index, raw := range entries {
		published := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &published); err != nil {
			t.Fatalf("%s case %d: %v", file, index, err)
		}
		header := treeVectorHeader{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s case %d: %v", file, index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		evidence, err := compareTreeKemVector(t, raw)
		if err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", file, index, header.CipherSuite, err)
		}
		tally.requireCompared(t, index, suite, evidence.inScope)
		if err := evidence.verdict(); err != nil {
			t.Fatalf("%s case %d (suite %#04x): %v", file, index, header.CipherSuite, err)
		}
		for _, answer := range evidence.answers {
			want := publishedCorpusField(t, published, answer.field)
			if answer.got != want {
				t.Fatalf("%s case %d (suite %#04x): %s answered %s and the corpus text publishes %s at %s",
					file, index, header.CipherSuite, answer.name, answer.got, want, answer.field)
			}
			tally.answer(want)
		}
		paths += len(evidence.perPath)
		for _, opened := range evidence.perPath {
			decrypts += opened
		}
		deep += evidence.deep
		states += evidence.privateStates
	}
	tally.assertRun(t, treeKemKatCovered, treeKemKatSkipped, treeKemKatComparisons, treeKemKatDistinct)
	if paths != treeKemKatPaths || decrypts != treeKemKatDecrypts {
		t.Fatalf("%s: %d published paths merged and %d leaves decrypted them, want %d and %d",
			file, paths, decrypts, treeKemKatPaths, treeKemKatDecrypts)
	}
	if states != treeKemKatPrivateStates {
		t.Fatalf("%s: %d published leaf private states were checked against their tree, want %d",
			file, states, treeKemKatPrivateStates)
	}
	// the deep arm, which the comparison count says nothing about: a receiver that only ever
	// opened the ciphertext standing at its own leaf answers every case where the sender is its
	// sibling and no other, and the count above would be short rather than wrong.
	if deep != treeKemKatDeep {
		t.Fatalf("%s: %d of %d decrypts entered the sender's path above the receiver's own leaf, want %d",
			file, deep, decrypts, treeKemKatDeep)
	}
	t.Logf("%s: %d published update paths merged and hashed, %d leaf decrypts reproducing a published path secret and commit secret, %d of them entering above the receiver's own leaf; %d private states checked against their tree",
		file, paths, decrypts, deep, states)
}

// The shape of the generate direction over the published corpus, transcribed from the file: 22
// cases at a registered suite publishing 124 leaf private states between them, so 124 senders,
// and every OTHER published member of the same case receives what that sender publishes -- which
// is 656 receipts, since the entries carry 2, 3, 4, 5, 6, 7, 8, 7, 5, 8 and 7 members and n
// members make n*(n-1) ordered pairs.
//
// The generated corpus the registry is handed is smaller on purpose: TestVectorGenerateThenVerify
// runs the installed verifier over every case of it and the verifier decrypts, so one base per
// registered suite is the loop-closing that costs a fixed amount rather than a quadratic one. The
// sweep below is where the quadratic coverage lives.
const (
	treeKemGeneratedSenders   = 124
	treeKemGeneratedReceipts  = 656
	treeKemGeneratedBaseCases = 2
)

// treeKemGeneratorBases is the published case at each registered suite that the generated corpus
// is built over: the one with the most published members, so the generated paths are the deepest
// the corpus can supply and every generated case has a receiver that enters the path above its
// own leaf.
//
// Derived off the corpus rather than an index written down here, so a corpus update that reorders
// the file cannot leave this reading a two-member case and reporting a clean generate direction
// over paths one node long.
func treeKemGeneratorBases(t *testing.T) []treekemReceiverVector {
	t.Helper()
	widest := map[CipherSuite]treekemReceiverVector{}
	for _, raw := range LoadVectorFile(t, treeVectorFile(t, 11)) {
		vector := treekemReceiverVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("parse a treekem case: %v", err)
		}
		suite, ok := implementedSuite(vector.CipherSuite)
		if !ok {
			continue
		}
		if held, found := widest[suite]; found && len(held.LeavesPrivate) >= len(vector.LeavesPrivate) {
			continue
		}
		widest[suite] = vector
	}
	bases := []treekemReceiverVector{}
	for _, suite := range Suites() {
		base, found := widest[suite]
		if !found {
			t.Fatalf("the corpus publishes no case at suite %#04x, which this package registers", uint16(suite))
		}
		if len(base.LeavesPrivate) < 3 {
			t.Fatalf("the widest case at suite %#04x publishes %d members, and a generated path needs three before any receiver enters it above its own leaf",
				uint16(suite), len(base.LeavesPrivate))
		}
		bases = append(bases, base)
	}
	if len(bases) != treeKemGeneratedBaseCases {
		t.Fatalf("%d generator bases were found and this package registers %d suites", len(bases), treeKemGeneratedBaseCases)
	}
	return bases
}

// generateTreeKemVectors is family 11's generate direction: for every member of a published
// epoch, the update path that member would publish committing over it, written back out in the
// corpus's own format so the installed verifier can be run over it.
//
// The tree, the group id, the epoch and the confirmed transcript hash are the published case's;
// what is generated is the path, the secrets it delivers and the tree hash of the epoch it opens.
// The ratchet_tree of a generated case is therefore the tree the path is a commit OVER, which is
// what the verifier merges into -- and the published private states stay valid against it,
// because CreateUpdatePathSecrets mutates a clone.
func generateTreeKemVectors(t *testing.T) json.RawMessage {
	t.Helper()
	generated := []treekemReceiverVector{}
	for _, base := range treeKemGeneratorBases(t) {
		suite, ok := implementedSuite(base.CipherSuite)
		if !ok {
			t.Fatalf("a generator base is at suite %#04x, which this package does not register", base.CipherSuite)
		}
		crypto := mustProvider(t, suite)
		tree, err := UnmarshalRatchetTree(MustHex(t, base.RatchetTree))
		if err != nil {
			t.Fatalf("decode a generator base's ratchet tree: %v", err)
		}
		for _, sender := range base.LeavesPrivate {
			generated = append(generated, generateOneTreeKemCase(t, crypto, base, tree, sender))
		}
	}
	if len(generated) == 0 {
		t.Fatal("the generate direction produced no case at all")
	}
	body, err := json.Marshal(generated)
	if err != nil {
		t.Fatalf("marshal the generated treekem cases: %v", err)
	}
	return body
}

// generateOneTreeKemCase is one member's commit over one published epoch, in the corpus's format.
//
// The two calls are separate here for the reason EncryptUpdatePath's own comment gives and it is
// the whole reason this family has a generate direction worth having: the group context every
// ciphertext is sealed under carries the tree hash of the tree AFTER this path is installed, so
// the context does not exist until the secrets have been created and the tree mutated. A
// generator that built the context from the tree hash it started with produces a path that opens
// for nobody, and a seal-and-open that shared that context would not notice.
func generateOneTreeKemCase(t *testing.T, crypto CryptoProvider, base treekemReceiverVector,
	tree *RatchetTree, sender treekemLeafPrivateVector) treekemReceiverVector {
	t.Helper()
	return generateOneTreeKemCaseSealedUnder(t, crypto, base, tree, sender, nil)
}

// generateOneTreeKemCaseSealedUnder is the same commit with the group context every ciphertext
// is sealed under handed to a caller, and the whole reason it is a parameter is the control that
// uses it.
//
// sealUnder nil is the case's own context, which is every generated case the registry or the
// generate sweep ever asks for. A control that answers something else produces the one case
// family 11's wrong-context refusal can be driven by: a path that opens under a context this
// case does not publish. That case cannot be written by editing a published one, because every
// corpus field the context is built out of also feeds the decrypt that must succeed -- edit any
// of them and the case is refused as a decrypt that did not open, which is a true statement
// about a class this arm is not.
func generateOneTreeKemCaseSealedUnder(t *testing.T, crypto CryptoProvider, base treekemReceiverVector,
	tree *RatchetTree, sender treekemLeafPrivateVector,
	sealUnder func(groupContext []byte) []byte) treekemReceiverVector {
	t.Helper()
	senderTree := tree.Clone()
	plan, err := senderTree.CreateUpdatePathSecrets(crypto, LeafIndex(sender.Index),
		SignaturePrivateKey(MustHex(t, sender.SignaturePriv)), MustHex(t, base.GroupId))
	if err != nil {
		t.Fatalf("sender %d CreateUpdatePathSecrets: %v", sender.Index, err)
	}
	treeHash, err := senderTree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("sender %d TreeHash: %v", sender.Index, err)
	}
	groupContext := base.groupContext(t, treeHash)
	if sealUnder != nil {
		groupContext = sealUnder(groupContext)
	}
	path, err := senderTree.EncryptUpdatePath(crypto, plan, LeafIndex(sender.Index), groupContext, nil)
	if err != nil {
		t.Fatalf("sender %d EncryptUpdatePath: %v", sender.Index, err)
	}
	if err := senderTree.VerifyParentHashes(crypto); err != nil {
		t.Fatalf("sender %d published a tree its own section 7.9 chain refuses: %v", sender.Index, err)
	}
	assertGeneratedCommitSecretIsTheRungPastTheRoot(t, plan)
	encoded, err := syntax.Marshal(path)
	if err != nil {
		t.Fatalf("sender %d Marshal(UpdatePath): %v", sender.Index, err)
	}

	// one entry per LEAF INDEX and null where that leaf cannot decrypt, which is the corpus's own
	// shape: the sender's own index, and every leaf whose private state the case does not carry.
	secrets := make([]*string, int(tree.LeafWidth()))
	for _, other := range base.LeavesPrivate {
		if other.Index == sender.Index {
			continue
		}
		lowest := CommonAncestor(LeafIndex(sender.Index).NodeIndex(), LeafIndex(other.Index).NodeIndex())
		secret, held := plan.Private.PathSecrets[lowest]
		if !held {
			t.Fatalf("sender %d holds no path secret at node %d, which is where leaf %d enters its path",
				sender.Index, lowest, other.Index)
		}
		text := HexOf(secret)
		secrets[other.Index] = &text
	}

	return treekemReceiverVector{
		CipherSuite:             base.CipherSuite,
		GroupId:                 base.GroupId,
		Epoch:                   base.Epoch,
		ConfirmedTranscriptHash: base.ConfirmedTranscriptHash,
		RatchetTree:             base.RatchetTree,
		LeavesPrivate:           base.LeavesPrivate,
		UpdatePaths: []treekemReceivedUpdatePath{{
			Sender:        sender.Index,
			UpdatePath:    HexOf(encoded),
			PathSecrets:   secrets,
			CommitSecret:  HexOf(plan.CommitSecret),
			TreeHashAfter: HexOf(treeHash),
		}},
	}
}

// assertGeneratedCommitSecretIsTheRungPastTheRoot is the one answer of the generate direction
// that came from neither side of this package.
//
// RFC 9420 section 7.4 makes the ladder path_secret[n+1] = DeriveSecret(path_secret[n], "path"),
// and section 8.1 makes the commit secret the rung PAST the root -- so the commit secret is
// DeriveSecret of the secret at the last node of the filtered direct path and not that secret
// itself. Both spellings are 32 octets of apparent random and both are stable, so a sender and a
// receiver that made the same mistake agree with each other perfectly; p4 task 18 shipped an
// "independent" verifier that called the production provider and could not have seen the
// analogous transposition at all.
//
// So it is re-derived here through independentDeriveSecret, which is written from the RFC text
// with crypto/hmac and reaches nothing this package declares --
// TestTheGenerateDirectionSharesNoCodePathWithVerify is the gate that keeps it that way. sha256
// is written into that derivation rather than read off a provider for the same reason, and
// TestBothRegisteredSuitesAreSha256AtThisWidth is what fails on the day that stops being true.
//
// A group of one has an empty filtered direct path and no last node; that case is answered as
// "nothing to check" rather than as a failure, and treeKemGeneratorBases requires three members,
// so no base this generator runs over is in it.
func assertGeneratedCommitSecretIsTheRungPastTheRoot(t *testing.T, plan *UpdatePathPlan) {
	t.Helper()
	if len(plan.Path) == 0 {
		return
	}
	top := plan.Path[len(plan.Path)-1]
	secret, held := plan.Private.PathSecrets[top]
	if !held {
		t.Fatalf("the plan holds no path secret at node %d, the last node of its own filtered direct path", top)
	}
	want := independentDeriveSecret(t, secret, "path")
	if !bytes.Equal(want, plan.CommitSecret) {
		t.Fatalf("the commit secret is %s and DeriveSecret(path_secret[last], \"path\") written from the RFC is %s",
			HexOf(plan.CommitSecret), HexOf(want))
	}
}

// TestVectorTreeKEMGenerate is family 11's generate direction over every published epoch, rather
// than over the two the registry is handed.
//
// Every published member of every in-scope case commits, and every OTHER published member of the
// same case receives it: 124 senders and 656 receipts. The receiver is not the sender -- it holds
// only the private state the corpus published and the octets the sender emitted -- so a commit
// secret both of them reach is a statement about two code paths agreeing rather than about one
// agreeing with itself, and the independent derivation inside the generator is what says the
// value they agree on is the one the RFC names.
//
// The tree the receiver merges into is the corpus's, which no part of this package built.
func TestVectorTreeKEMGenerate(t *testing.T) {
	file := treeVectorFile(t, 11)
	tally, entries := newVectorRunTally(t, file)
	senders, receipts, deep := 0, 0, 0
	for index, raw := range entries {
		header := treeVectorHeader{}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s case %d: %v", file, index, err)
		}
		suite, inScope := tally.filter(header.CipherSuite)
		if !inScope {
			continue
		}
		vector := treekemReceiverVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("%s case %d: %v", file, index, err)
		}
		crypto := mustProvider(t, suite)
		base, err := UnmarshalRatchetTree(MustHex(t, vector.RatchetTree))
		if err != nil {
			t.Fatalf("%s case %d: UnmarshalRatchetTree: %v", file, index, err)
		}
		compared := 0
		for _, sender := range vector.LeavesPrivate {
			senders += 1
			generated := generateOneTreeKemCase(t, crypto, vector, base, sender)
			update := generated.UpdatePaths[0]
			path := &UpdatePath{}
			if err := syntax.Unmarshal(MustHex(t, update.UpdatePath), path); err != nil {
				t.Fatalf("%s case %d sender %d: the generated update path does not decode: %v",
					file, index, sender.Index, err)
			}
			groupContext := vector.groupContext(t, MustHex(t, update.TreeHashAfter))
			for _, other := range vector.LeavesPrivate {
				if other.Index == sender.Index {
					continue
				}
				receiverTree := base.Clone()
				if err := receiverTree.MergeUpdatePath(crypto, LeafIndex(sender.Index), path); err != nil {
					t.Fatalf("%s case %d sender %d receiver %d: MergeUpdatePath: %v",
						file, index, sender.Index, other.Index, err)
				}
				// the receiver recomputes the tree hash rather than taking the sender's, because
				// the sender's is the one value both ends would agree on for the wrong reason if
				// the merge and the create disagreed about what the epoch looks like.
				treeHash, err := receiverTree.TreeHash(crypto)
				if err != nil {
					t.Fatalf("%s case %d sender %d receiver %d: TreeHash: %v",
						file, index, sender.Index, other.Index, err)
				}
				if got := HexOf(treeHash); got != update.TreeHashAfter {
					t.Fatalf("%s case %d sender %d receiver %d: the merged tree hashes to %s and the sender published %s",
						file, index, sender.Index, other.Index, got, update.TreeHashAfter)
				}
				priv, found := vector.private(t, other.Index)
				if !found {
					t.Fatalf("%s case %d: leaf %d publishes no private state", file, index, other.Index)
				}
				result, err := receiverTree.DecryptUpdatePath(crypto, LeafIndex(sender.Index), path,
					groupContext, priv, nil)
				if err != nil {
					t.Fatalf("%s case %d sender %d receiver %d: DecryptUpdatePath: %v",
						file, index, sender.Index, other.Index, err)
				}
				if got := HexOf(result.CommitSecret); got != update.CommitSecret {
					t.Fatalf("%s case %d sender %d receiver %d: the receiver reached commit secret %s and the sender published %s",
						file, index, sender.Index, other.Index, got, update.CommitSecret)
				}
				lowest := CommonAncestor(LeafIndex(sender.Index).NodeIndex(), LeafIndex(other.Index).NodeIndex())
				// where this member entered, and not the common ancestor: the common ancestor of
				// two distinct leaves is always a parent, so a counter over that is true for
				// every case and counts nothing.
				if _, entry, _, entered := updatePathEntryFor(t, crypto, receiverTree,
					LeafIndex(sender.Index), priv, nil); !entered {
					t.Fatalf("%s case %d sender %d receiver %d: the decrypt succeeded and the tree math finds it no entry point",
						file, index, sender.Index, other.Index)
				} else if entry != LeafIndex(other.Index).NodeIndex() {
					deep += 1
				}
				if got := HexOf(result.Private.PathSecrets[lowest]); update.PathSecrets[other.Index] == nil ||
					got != *update.PathSecrets[other.Index] {
					t.Fatalf("%s case %d sender %d receiver %d: the receiver recovered %s at node %d and the sender published %v",
						file, index, sender.Index, other.Index, got, lowest, update.PathSecrets[other.Index])
				}
				// the commit secret is one value the whole run agrees on, so it is also the one
				// value a sender and a receiver that are wrong in the same direction agree on.
				// tally.answer is fed the SENDER's published text and the comparison above is
				// what earns it, so a run that stopped generating moves this count.
				tally.answer(update.CommitSecret)
				receipts += 1
				compared += 1
			}
		}
		tally.requireCompared(t, index, suite, compared > 0)
	}
	// the tally's own accounting, over a corpus whose answers this run produced rather than read:
	// covered and skipped are the same partition the verify direction makes, and the comparisons
	// are the receipts.
	// the distinct count is the SENDERS and not the receipts: every sender samples a fresh ladder,
	// so the 656 answers are 124 distinct commit secrets, each agreed on by every member that
	// received that sender's path. A generator answering one value for every sender would make the
	// same 656 comparisons against one answer, which is what this separates.
	tally.assertRun(t, treeKemKatCovered, treeKemKatSkipped, treeKemGeneratedReceipts, treeKemGeneratedSenders)
	if senders != treeKemGeneratedSenders || receipts != treeKemGeneratedReceipts {
		t.Fatalf("%d members committed and %d members received, want %d and %d",
			senders, receipts, treeKemGeneratedSenders, treeKemGeneratedReceipts)
	}
	// the same structural count the verify direction pins, over the same trees: a generated path
	// installs the sender's own direct path and touches no copath child, so which node a receiver
	// enters at is the shape of the published tree and nothing the generator sampled.
	if deep != treeKemKatDeep {
		t.Fatalf("%d of %d generated receipts entered the path above the receiver's own leaf, want %d",
			deep, receipts, treeKemKatDeep)
	}
	t.Logf("%s: %d generated commits received by %d other members, %d of them entering the path above their own leaf",
		file, senders, receipts, deep)
}

// tkKatBaseCase answers a published treekem case at a registered suite that every row of the
// control below can be built out of: it must publish at least two update paths, and at least one
// leaf that decrypts a path it is not the sender of, or the rows that corrupt a secret would
// corrupt nothing.
//
// The base is the corpus's own and not a fixture: the whole of what the refusals below mean is
// that this exact case is accepted and a one octet edit of it is not.
func tkKatBaseCase(t *testing.T) treekemReceiverVector {
	t.Helper()
	for _, raw := range LoadVectorFile(t, treeVectorFile(t, 11)) {
		vector := treekemReceiverVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("parse a treekem case: %v", err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok {
			continue
		}
		if len(vector.UpdatePaths) < 2 || len(vector.LeavesPrivate) < 3 {
			continue
		}
		if _, found := tkFirstDecryptingLeaf(vector, 0); !found {
			continue
		}
		if _, err := compareTreeKemVector(t, tkRewrite(t, vector, func(*treekemReceiverVector) {})); err != nil {
			t.Fatalf("a published treekem case was refused: %v", err)
		}
		return vector
	}
	t.Fatal("no published case at a registered suite publishes two update paths, three members and a leaf that decrypts the first path; the rows below need all three")
	return treekemReceiverVector{}
}

// tkFirstDecryptingLeaf is the lowest leaf index one published path delivers a secret to, which
// is the entry the secret-corrupting rows below move.
func tkFirstDecryptingLeaf(vector treekemReceiverVector, path int) (int, bool) {
	if path >= len(vector.UpdatePaths) {
		return 0, false
	}
	for leaf, secret := range vector.UpdatePaths[path].PathSecrets {
		if secret != nil {
			return leaf, true
		}
	}
	return 0, false
}

// tkRewrite re-encodes one corpus row with one thing changed, through the row's own struct.
//
// The nested halves are cloned before the mutation runs, because a row that edited
// UpdatePaths[0] in place would edit the base every later row is built from, and the rows would
// then be a sequence of corruptions rather than independent ones.
func tkRewrite(t *testing.T, base treekemReceiverVector,
	mutate func(corrupted *treekemReceiverVector)) json.RawMessage {
	t.Helper()
	corrupted := base
	corrupted.LeavesPrivate = slices.Clone(base.LeavesPrivate)
	for at := range corrupted.LeavesPrivate {
		corrupted.LeavesPrivate[at].PathSecrets = slices.Clone(base.LeavesPrivate[at].PathSecrets)
	}
	corrupted.UpdatePaths = slices.Clone(base.UpdatePaths)
	for at := range corrupted.UpdatePaths {
		corrupted.UpdatePaths[at].PathSecrets = slices.Clone(base.UpdatePaths[at].PathSecrets)
	}
	mutate(&corrupted)
	body, err := json.Marshal(corrupted)
	if err != nil {
		t.Fatalf("re-encode the corrupted treekem case: %v", err)
	}
	return body
}

// TestCompareTreeKemVectorRefusesAnAnswerItShouldNotAccept is the control the runner cannot be.
//
// Every case of the vendored corpus agrees with this implementation, so a comparator that
// reproduced all three of its answers and one that returned an empty struct produce identical
// runs; only a case that is wrong on purpose separates them. Each row names the sentinel it owes,
// so a refusal for the WRONG reason is reported too.
func TestCompareTreeKemVectorRefusesAnAnswerItShouldNotAccept(t *testing.T) {
	accepted, refusals := treeKemRefusals(t)
	assertComparatorRefuses(t, treeVectorFile(t, 11), compareTreeKemVerdict, accepted, refusals)
}

// compareTreeKemVerdict is one run of family 11's comparator reported as a single error, which
// is the shape assertComparatorRefuses and the class gate below both drive it through. See
// compareTreeValidationVerdict next door for why it is one declaration and not two closures.
func compareTreeKemVerdict(t *testing.T, raw json.RawMessage) error {
	t.Helper()
	evidence, err := compareTreeKemVector(t, raw)
	if err != nil {
		return err
	}
	return evidence.verdict()
}

// treeKemRefusals is family 11's control table: the unmodified published case every row is
// judged against, and one deliberately wrong case per defect class this family's comparator and
// verdict can reach from a corpus.
//
// A function for treeValidationRefusals' reason: the class gate below reads the sentinels these
// rows actually name, rather than a second list of them that would agree with itself while the
// table under it lost a row.
func treeKemRefusals(t *testing.T) (json.RawMessage, []comparatorRefusal) {
	t.Helper()
	base := tkKatBaseCase(t)
	// the octet that is flipped is the SECOND and not the first, and that is not arbitrary. An
	// X25519 private key is clamped before use -- the low three bits of its first octet are
	// cleared -- so a private key whose first octet was flipped in bit 0 derives the very same
	// public key, and the row that means to corrupt a private state would corrupt nothing.
	flipSecond := func(text string) string {
		octets := MustHex(t, text)
		if len(octets) < 2 {
			t.Fatalf("%q is %d octets and this flip needs two", text, len(octets))
		}
		octets[1] ^= 0x01
		return HexOf(octets)
	}
	leaf, found := tkFirstDecryptingLeaf(base, 0)
	if !found {
		t.Fatal("the base case's first path delivers no secret, so the secret rows below corrupt nothing")
	}
	rung := -1
	for at, entry := range base.LeavesPrivate {
		if len(entry.PathSecrets) > 0 {
			rung = at
			break
		}
	}
	if rung < 0 {
		t.Fatal("no published member of the base case holds a path secret, so the private-state row corrupts nothing")
	}
	return tkRewrite(t, base, func(*treekemReceiverVector) {}),
		[]comparatorRefusal{
			{
				name:   "a case whose published ratchet tree does not decode",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) { c.RatchetTree = "00" }),
				want:   errTreeKemTreeUndecoded,
			},
			{
				name:   "a case whose published update path does not decode",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) { c.UpdatePaths[0].UpdatePath = "00" }),
				want:   errTreeKemPathUndecoded,
			},
			{
				// a PATH SECRET and not the leaf key. Consistent deliberately does not re-derive
				// the leaf public half -- its scope is the path secrets, and DecryptUpdatePath is
				// where both halves of the leaf key exist -- so a flipped encryption_priv is
				// refused a path later as a decrypt that did not open, which is a true refusal for
				// a class this row does not name. (The derivation itself exists: hpkePublicKeyOf,
				// outside CryptoProvider, is what the join door holds a joiner's own pair with.)
				name: "a case whose published path secret does not derive the key its tree announces",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) {
					c.LeavesPrivate[rung].PathSecrets[0].PathSecret =
						flipSecond(c.LeavesPrivate[rung].PathSecrets[0].PathSecret)
				}),
				want: errTreeKemPrivateState,
			},
			{
				name: "a case whose published tree_hash_after is not the merged tree's",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) {
					c.UpdatePaths[0].TreeHashAfter = flipSecond(c.UpdatePaths[0].TreeHashAfter)
				}),
				want: errTreeKemTreeHash,
			},
			{
				name: "a case whose published path secret is not the one the leaf recovers",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) {
					moved := flipSecond(*c.UpdatePaths[0].PathSecrets[leaf])
					c.UpdatePaths[0].PathSecrets[leaf] = &moved
				}),
				want: errTreeKemPathSecret,
			},
			{
				name: "a case whose published commit secret is not the one the epoch reaches",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) {
					c.UpdatePaths[0].CommitSecret = flipSecond(c.UpdatePaths[0].CommitSecret)
				}),
				want: errTreeKemCommitSecret,
			},
			{
				name: "a case whose confirmed transcript hash is not the one the path was sealed under",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) {
					c.ConfirmedTranscriptHash = flipSecond(c.ConfirmedTranscriptHash)
				}),
				want: errTreeKemDecrypt,
			},
			{
				name: "a case publishing a path secret for a leaf it publishes no private state for",
				vector: tkRewrite(t, base, func(c *treekemReceiverVector) {
					kept := []treekemLeafPrivateVector{}
					for _, entry := range c.LeavesPrivate {
						if int(entry.Index) == leaf {
							continue
						}
						kept = append(kept, entry)
					}
					c.LeavesPrivate = kept
				}),
				want: errTreeKemMissingPrivate,
			},
			{
				// a path that still DECODES and no longer fits the tree it is a commit over, which
				// is the class between those two: a node dropped leaves the update path one rung
				// short of the sender's filtered direct path and ValSem202 refuses it.
				name:   "a case whose published update path decodes and does not fit the tree it commits over",
				vector: aTreeKemCaseWhoseUpdatePathDoesNotFitTheTree(t, base),
				want:   errTreeKemMerge,
			},
			{
				// the row the per-decrypt wrong-context question exists for, and the only one that
				// reaches it: the path is sealed under the case's own context with the octet that
				// question flips already flipped, so the question hands it the context it really was
				// sealed under and it opens. A comparator that stopped asking reports this case as a
				// decrypt that did not open, which is a refusal for the wrong reason.
				name:   "a case whose published update path opens under a group context the case does not publish",
				vector: aTreeKemCaseSealedUnderAnotherGroupContext(t, base),
				want:   errTreeKemContextIgnored,
			},
		}
}

// aTreeKemCaseWhoseUpdatePathDoesNotFitTheTree is the base case with one node dropped from its
// first published update path and the path re-encoded.
//
// Re-encoded through this package's own marshaller rather than by cutting the hex, so the case
// is refused for the length and not for the decode: a truncated hex string is refused a step
// earlier as a path that does not decode, which is a true refusal for the class the row above
// this one owns.
func aTreeKemCaseWhoseUpdatePathDoesNotFitTheTree(t *testing.T, base treekemReceiverVector) json.RawMessage {
	t.Helper()
	path := &UpdatePath{}
	if err := syntax.Unmarshal(MustHex(t, base.UpdatePaths[0].UpdatePath), path); err != nil {
		t.Fatalf("decode the base case's first update path: %v", err)
	}
	if len(path.Nodes) == 0 {
		t.Fatal("the base case's first update path carries no node, so dropping one changes nothing")
	}
	path.Nodes = path.Nodes[:len(path.Nodes)-1]
	encoded, err := syntax.Marshal(path)
	if err != nil {
		t.Fatalf("re-encode the shortened update path: %v", err)
	}
	return tkRewrite(t, base, func(c *treekemReceiverVector) {
		c.UpdatePaths[0].UpdatePath = HexOf(encoded)
	})
}

// aTreeKemCaseSealedUnderAnotherGroupContext is one member's commit over the base case's epoch,
// sealed under the base case's own group context with the octet the comparator's wrong-context
// question flips already flipped.
//
// Generated rather than edited, and that is the whole point. The context is built out of the
// case's group id, epoch, confirmed transcript hash and the tree hash of the MERGED tree, and
// every one of those either is a published field the decrypt also depends on or is computed by
// the comparator itself -- so there is no edit of a published case that leaves the real decrypt
// working and the wrong-context probe opening. A path sealed under the probe's own context is
// the only case that separates a DecryptUpdatePath which checks the context from one which does
// not, which is the whole reason the arm is there.
func aTreeKemCaseSealedUnderAnotherGroupContext(t *testing.T, base treekemReceiverVector) json.RawMessage {
	t.Helper()
	suite, ok := implementedSuite(base.CipherSuite)
	if !ok {
		t.Fatalf("the base case is at suite %#04x, which this package does not register", base.CipherSuite)
	}
	crypto := mustProvider(t, suite)
	tree, err := UnmarshalRatchetTree(MustHex(t, base.RatchetTree))
	if err != nil {
		t.Fatalf("decode the base case's ratchet tree: %v", err)
	}
	if len(base.LeavesPrivate) == 0 {
		t.Fatal("the base case publishes no member, so nothing here can commit")
	}
	generated := generateOneTreeKemCaseSealedUnder(t, crypto, base, tree, base.LeavesPrivate[0],
		func(groupContext []byte) []byte {
			// the same octet compareTreeKemVector's own question flips, so the two contexts are
			// each other's. Written here as the flip of the context the comparator will compute
			// rather than as a constant, because what has to hold is that the two agree.
			sealed := bytes.Clone(groupContext)
			sealed[0] ^= 0x01
			return sealed
		})
	body, err := json.Marshal(generated)
	if err != nil {
		t.Fatalf("marshal the case sealed under another group context: %v", err)
	}
	return body
}

// treeKemSentinels is this family's refusals addressed by the identifier they are declared under.
// See treeValidationSentinels next door for why the gate below does not read this AS the class.
var treeKemSentinels = map[string]error{
	"errTreeKemIncomplete":     errTreeKemIncomplete,
	"errTreeKemTreeUndecoded":  errTreeKemTreeUndecoded,
	"errTreeKemPathUndecoded":  errTreeKemPathUndecoded,
	"errTreeKemPrivateState":   errTreeKemPrivateState,
	"errTreeKemMerge":          errTreeKemMerge,
	"errTreeKemTreeHash":       errTreeKemTreeHash,
	"errTreeKemParentHash":     errTreeKemParentHash,
	"errTreeKemMissingPrivate": errTreeKemMissingPrivate,
	"errTreeKemDecrypt":        errTreeKemDecrypt,
	"errTreeKemPathSecret":     errTreeKemPathSecret,
	"errTreeKemCommitSecret":   errTreeKemCommitSecret,
	"errTreeKemContextIgnored": errTreeKemContextIgnored,
}

// TestTreeKemVerdictReportsEveryClassItNames drives family 11's verdict over evidence built here.
//
// Two of the classes it names cannot be reached from a corpus. A comparison that is INCOMPLETE is
// one whose comparator returned early or emitted its answers in the wrong order, and no corpus
// case asks for that; a merged tree that fails section 7.9 while every published answer still
// agrees is a DEFECT in the merge, and no corpus publishes one. Both are the arms that hold the
// sweep honest, so both are driven here.
//
// The class is derived from the two methods' own source: every errTreeKem sentinel either of them
// names owes exactly one row.
func TestTreeKemVerdictReportsEveryClassItNames(t *testing.T) {
	complete := func() treeKemComparison {
		return treeKemComparison{
			inScope:       true,
			hashSize:      32,
			leafWidth:     4,
			privateStates: 2,
			perPath:       []int{1},
			refusals:      1,
			answers: []treeKemAnswer{
				{name: treeKemAnswerTreeHash, field: "update_paths.0.tree_hash_after", got: "aa", want: "aa"},
				{name: treeKemAnswerPathSecret, field: "update_paths.0.path_secrets.1", got: "bb", want: "bb"},
				{name: treeKemAnswerCommitSecret, field: "update_paths.0.commit_secret", got: "cc", want: "cc"},
			},
		}
	}
	if err := complete().verdict(); err != nil {
		t.Fatalf("a complete and agreeing comparison was refused: %v", err)
	}
	rows := []struct {
		names error
		spoil func(evidence *treeKemComparison)
	}{
		{errTreeKemIncomplete, func(e *treeKemComparison) { e.refusals = 0 }},
		{errTreeKemTreeHash, func(e *treeKemComparison) { e.answers[0].got = "dd" }},
		{errTreeKemPathSecret, func(e *treeKemComparison) { e.answers[1].got = "dd" }},
		{errTreeKemCommitSecret, func(e *treeKemComparison) { e.answers[2].got = "dd" }},
		{errTreeKemParentHash, func(e *treeKemComparison) {
			e.parentHashError = errors.New("a copath child claimed nothing")
		}},
	}
	claimed := map[string]bool{}
	for _, row := range rows {
		if row.names == nil {
			t.Fatal("a row names no sentinel, so any refusal at all would satisfy it")
		}
		found := ""
		for name, sentinel := range treeKemSentinels {
			if sentinel == row.names {
				found = name
			}
		}
		if found == "" {
			t.Fatalf("a row names %v and treeKemSentinels resolves no identifier to it", row.names)
		}
		if claimed[found] {
			t.Fatalf("%s is claimed by two rows, so some class this verdict names is claimed by none", found)
		}
		claimed[found] = true
	}
	declared := theSourceDeclaring(t, "treeKemComparison", "verdict")
	mentioned := map[string]bool{}
	for _, method := range []string{"verdict", "incomplete"} {
		for name := range namesMentionedIn(declared.declarationOf(t, "treeKemComparison", method)) {
			if strings.HasPrefix(name, "errTreeKem") {
				mentioned[name] = true
			}
		}
	}
	if len(mentioned) == 0 {
		t.Fatal("the verdict and its completeness check name no errTreeKem sentinel at all, so the class below was derived from nothing")
	}
	if len(mentioned) != len(rows) {
		t.Fatalf("the verdict names %v and this control offers %d rows for %v",
			slices.Sorted(maps.Keys(mentioned)), len(rows), slices.Sorted(maps.Keys(claimed)))
	}
	for _, name := range slices.Sorted(maps.Keys(mentioned)) {
		if _, known := treeKemSentinels[name]; !known {
			t.Errorf("%s is a class the verdict reports and treeKemSentinels does not resolve it", name)
			continue
		}
		if !claimed[name] {
			t.Errorf("%s is a class the verdict reports and no row here drives it", name)
		}
	}
	for _, row := range rows {
		evidence := complete()
		row.spoil(&evidence)
		err := evidence.verdict()
		if err == nil {
			t.Errorf("%v: the spoiled comparison was accepted, so this arm of the verdict reports nothing", row.names)
			continue
		}
		if !errors.Is(err, row.names) {
			t.Errorf("the spoiled comparison was refused as %v, want %v; a refusal for the wrong reason is the verdict checking something else",
				err, row.names)
		}
	}
}

// ---------------------------------------------------------------------------
// the joints: each arm of a comparator held to a control that reaches it
// ---------------------------------------------------------------------------

// The refusals of each tree family that NO corpus case can express, with the argument for each.
//
// An exemption table and not a filter, for treeVectorFamiliesElsewhere's reason: whether a class
// is reachable from a corpus is a judgement about that class and not a property its declaration
// carries. What makes an exemption table safe is that it is held in BOTH directions -- the gate
// below refuses an exemption for a sentinel no declaration of the family names, refuses a
// sentinel that is both exempt and claimed by a control row, and refuses a sentinel that is
// neither -- so the only way a class leaves the corpus tables is somebody writing down here why
// no corpus case can put it there.
var treeValidationClassElsewhere = map[string]string{
	"errTreeValidationIncomplete": "an incomplete comparison is one whose comparator returned early or emitted its answers out of order, and no corpus case can ask for that; TestTreeValidationVerdictReportsEveryClassItNames drives it over evidence built by hand",
}

var treeKemClassElsewhere = map[string]string{
	"errTreeKemIncomplete": "an incomplete comparison is one whose comparator returned early or emitted its answers out of order, and no corpus case can ask for that; TestTreeKemVerdictReportsEveryClassItNames drives it over evidence built by hand",
	"errTreeKemParentHash": "the group context every ciphertext of a path is sealed under carries the tree hash of the MERGED tree, so any edit leaving that tree's section 7.9 chain broken also moves the hash the context is built over and the case is refused as a decrypt that did not open, a rung before the chain is judged; TestTreeKemVerdictReportsEveryClassItNames drives it over evidence built by hand",
}

// TestTheTreeComparatorsRefuseEveryClassTheyName holds each family's CORPUS control table to the
// class its own comparator and verdict name, derived from their source.
//
// The gap this closes was measured twice. Family 10's wrong-group question and family 11's
// per-decrypt wrong-context question could each be deleted outright with all 1181 tests of this
// package still passing, because the sentinel each of them raises was named by no row of any
// control table and reachable from no corpus case at all. The two verdict gates next door derive
// the same class and satisfy it over evidence built BY HAND, which says the verdict reports the
// class and says nothing whatever about the comparator ever producing it -- so the join between
// the two halves was missing, and both arms were deletable in silence.
//
// The class is DERIVED from three declarations per family and not listed: the comparator, and
// the two methods that judge what it produced. A sentinel any of them names owes either a row of
// that family's corpus table or a written exemption, and the exemption tables above are held in
// both directions, so neither an arm added without a control nor an exemption for an arm that no
// longer exists can pass here.
func TestTheTreeComparatorsRefuseEveryClassTheyName(t *testing.T) {
	acceptedValidation, validationRows := treeValidationRefusals(t)
	acceptedKem, kemRows := treeKemRefusals(t)
	if len(acceptedValidation) == 0 || len(acceptedKem) == 0 {
		t.Fatal("a control table answered no accepted case, so the rows it carries were never judged against anything")
	}
	for _, family := range []struct {
		number int
		// prefix is how this family spells its refusals, which is what the derivation filters
		// the names of each declaration by.
		prefix string
		// comparator is the plain function that runs one case, and receiver the type whose
		// verdict and incomplete judge what it produced.
		comparator string
		receiver   string
		// sentinels resolves an identifier the source names back to the value a row carries.
		sentinels map[string]error
		exempt    map[string]string
		rows      []comparatorRefusal
	}{
		{10, "errTreeValidation", "compareTreeValidationVector", "treeValidationComparison",
			treeValidationSentinels, treeValidationClassElsewhere, validationRows},
		{11, "errTreeKem", "compareTreeKemVector", "treeKemComparison",
			treeKemSentinels, treeKemClassElsewhere, kemRows},
	} {
		mentioned := map[string]bool{}
		collect := func(declaration *ast.FuncDecl) {
			for name := range namesMentionedIn(declaration) {
				if strings.HasPrefix(name, family.prefix) {
					mentioned[name] = true
				}
			}
		}
		comparatorSource := theSourceDeclaring(t, "", family.comparator)
		collect(comparatorSource.declarationOf(t, "", family.comparator))
		verdictSource := theSourceDeclaring(t, family.receiver, "verdict")
		for _, method := range []string{"verdict", "incomplete"} {
			collect(verdictSource.declarationOf(t, family.receiver, method))
		}
		if len(mentioned) == 0 {
			t.Fatalf("family %d: %s and %s's verdict name no %s sentinel at all, so the class below was derived from nothing",
				family.number, family.comparator, family.receiver, family.prefix)
		}
		if len(family.rows) == 0 {
			t.Fatalf("family %d: the corpus control table is empty, so every class below would be reported unclaimed for one reason", family.number)
		}
		claimed := map[string]bool{}
		for _, row := range family.rows {
			if row.want == nil {
				t.Fatalf("family %d: the row %q names no sentinel, so any refusal at all would satisfy it", family.number, row.name)
			}
			found := ""
			for name, sentinel := range family.sentinels {
				if sentinel == row.want {
					found = name
				}
			}
			if found == "" {
				t.Fatalf("family %d: the row %q owes %v and this file's sentinel table resolves no identifier to it",
					family.number, row.name, row.want)
			}
			if !mentioned[found] {
				t.Errorf("family %d: the row %q owes %s and neither %s nor %s's verdict names that class, so the row is aimed at an arm that no longer exists",
					family.number, row.name, found, family.comparator, family.receiver)
			}
			claimed[found] = true
		}
		for _, name := range slices.Sorted(maps.Keys(family.exempt)) {
			if family.exempt[name] == "" {
				t.Errorf("family %d: %s is exempted from the corpus table with no argument written down", family.number, name)
			}
			if !mentioned[name] {
				t.Errorf("family %d: %s is exempted from the corpus table and no declaration of this family names it", family.number, name)
			}
			if claimed[name] {
				t.Errorf("family %d: %s is exempted from the corpus table as unreachable and a row of that table is refused as it; one of the two is wrong",
					family.number, name)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(mentioned)) {
			if _, known := family.sentinels[name]; !known {
				t.Errorf("family %d: %s is a class this family names and its sentinel table resolves no value for it", family.number, name)
				continue
			}
			if claimed[name] {
				continue
			}
			if _, exempt := family.exempt[name]; exempt {
				continue
			}
			t.Errorf("family %d: %s is a class %s or %s's verdict names, no row of the corpus control table is refused as it, and no exemption says why no corpus case can express it; an arm nothing reaches can be deleted with this package green",
				family.number, name, family.comparator, family.receiver)
		}
		t.Logf("family %d: %d classes derived from %s and %s's verdict, %d claimed by corpus control rows, %d exempted as unreachable from a corpus",
			family.number, len(mentioned), family.comparator, family.receiver, len(claimed), len(family.exempt))
	}
}

// theCallsReachableFrom is every function name reached from one declaration by CALLS alone,
// following a call into a function the same set declares.
//
// Calls alone, and that is the whole difference from reachableNames in key_schedule_kat_test.go,
// which collects every identifier a body names in ANY position because the question it answers
// -- does this reach the code under test -- is one that over-approximating costs nothing on. The
// question here is the opposite shape and a mention is exactly what has to fail it: a call
// replaced by a discard of the same identifier still names it, so the wider walk would go on
// reporting a derivation as reached over a generator that no longer runs it. That edit was
// measured on this file's generator and left the whole package green.
func theCallsReachableFrom(t *testing.T, declared map[string]*ast.FuncDecl, root string) map[string]bool {
	t.Helper()
	start, ok := declared[root]
	if !ok {
		t.Fatalf("%s is not declared in the files scanned, so its call closure would be empty", root)
	}
	reached := map[string]bool{}
	pending := []*ast.FuncDecl{start}
	visited := map[string]bool{root: true}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		ast.Inspect(current, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			name := ""
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				name = callee.Name
			case *ast.SelectorExpr:
				name = callee.Sel.Name
			}
			if name == "" {
				return true
			}
			reached[name] = true
			if next, isLocal := declared[name]; isLocal && !visited[name] {
				visited[name] = true
				pending = append(pending, next)
			}
			return true
		})
	}
	return reached
}

// TestTheTreeKemGeneratorReachesAnIndependentDerivation is the other half of the claim family
// 11's generate direction makes about itself.
//
// TestTheGenerateDirectionSharesNoCodePathWithVerify holds every function whose name claims
// independence to touching no production code, which says the derivation IS independent. Nothing
// said the generator runs one. Measured: replacing the single call to
// assertGeneratedCommitSecretIsTheRungPastTheRoot with a discard of its name left all 1181 tests
// passing, because the generator's own answers are then compared only against a receiver in the
// same package -- and a sender and a receiver that made the same section 8.1 transposition agree
// with each other perfectly.
//
// The class of derivations is DERIVED, the same way that gate derives it: every function these
// test files declare whose name claims independence. A list would name the ones that existed
// when it was written.
func TestTheTreeKemGeneratorReachesAnIndependentDerivation(t *testing.T) {
	control := func(source string) map[string]*ast.FuncDecl {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, "control_test.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse the control: %v", err)
		}
		return testFileFunctions(map[string]*ast.File{"control_test.go": parsed})
	}
	// the walk must separate a CALL from a MENTION, or the gate below is satisfied by the very
	// edit it exists to refuse.
	mentions := control("package control\n\nfunc independentControl() {}\n\nfunc generateControl() { _ = independentControl }\n")
	if theCallsReachableFrom(t, mentions, "generateControl")["independentControl"] {
		t.Fatal("the walk reports a derivation a control only MENTIONS, so it would pass over a generator whose call was replaced by a discard")
	}
	calls := control("package control\n\nfunc independentControl() {}\n\nfunc generateControl() { independentControl() }\n")
	if !theCallsReachableFrom(t, calls, "generateControl")["independentControl"] {
		t.Fatal("the walk does not report a derivation a control CALLS, so the gate below would be reporting on nothing")
	}
	// and it must follow a call through a hop, because the real generator reaches its derivation
	// through the assertion that owns it rather than directly.
	hops := control("package control\n\nfunc independentControl() {}\n\nfunc assertControl() { independentControl() }\n\nfunc generateControl() { assertControl() }\n")
	if !theCallsReachableFrom(t, hops, "generateControl")["independentControl"] {
		t.Fatal("the walk does not follow a call through a second function, so the gate below reads one hop of a generator that takes two")
	}

	declared := testFileFunctions(packageTestFiles(t))
	roots := []string{}
	for name := range declared {
		if strings.HasPrefix(name, "independent") {
			roots = append(roots, name)
		}
	}
	slices.Sort(roots)
	if len(roots) == 0 {
		t.Fatalf("no function this package's %d declared test functions name claims independence, so the derivation below is over an empty class", len(declared))
	}
	// the generator the registry installs has to reach the case builder, or what is held below
	// is a function nothing runs.
	if !theCallsReachableFrom(t, declared, "generateTreeKemVectors")["generateOneTreeKemCase"] {
		t.Fatal("generateTreeKemVectors does not call generateOneTreeKemCase, so the derivation held below is not on the registered generator's path")
	}
	reached := theCallsReachableFrom(t, declared, "generateOneTreeKemCase")
	found := []string{}
	for _, root := range roots {
		if reached[root] {
			found = append(found, root)
		}
	}
	if len(found) == 0 {
		t.Fatalf("generateOneTreeKemCase calls none of the %d independent derivations these test files declare (%v); family 11's generate direction then answers with this package's own code on both sides, and a sender and a receiver wrong in the same direction agree perfectly",
			len(roots), roots)
	}
	t.Logf("generateOneTreeKemCase reaches %v of the %d independent derivations declared across this package's test files", found, len(roots))
}

// aPublishedLeafNode is the first occupied leaf of the first tree-validation case at a suite this
// package registers.
//
// The corpus's own leaf and not one built here, because what the gate below asks of it is
// whether production's preimage MOVES, and a leaf assembled by this test would answer that
// question about a structure no other implementation ever signed.
func aPublishedLeafNode(t *testing.T) *LeafNode {
	t.Helper()
	for _, raw := range LoadVectorFile(t, treeVectorFile(t, 10)) {
		vector := treeValidationVector{}
		if err := json.Unmarshal(raw, &vector); err != nil {
			t.Fatalf("parse a tree-validation case: %v", err)
		}
		if _, ok := implementedSuite(vector.CipherSuite); !ok {
			continue
		}
		tree, err := UnmarshalRatchetTree(MustHex(t, vector.Tree))
		if err != nil {
			t.Fatalf("decode a tree-validation case's ratchet tree: %v", err)
		}
		for x := uint32(0); x < uint32(tree.LeafWidth()); x += 1 {
			if leaf := tree.Leaf(LeafIndex(x)); leaf != nil {
				return leaf
			}
		}
	}
	t.Fatal("no published tree-validation case at a registered suite carries an occupied leaf")
	return nil
}

// TestLeafNodeSourceBindsItsPositionIsWhatSignatureContentActuallyBinds derives the predicate's
// class from production instead of holding it to a second reading of the RFC.
//
// leafNodeSourceBindsItsPosition is a hand written second spelling of section 7.2's select, and
// the corpus pins it in one direction only. Widening it IS caught: add key_package and
// TestVectorTreeValidation refuses a leaf on the spot. Narrowing it is not -- measured, dropping
// its update arm left the whole package green, because every one of the 288 context bound leaves
// those 28 trees publish is commit-source and treeValidationKatBound did not move. So the pinned
// count says the wrong-group control ran over something and says nothing about the predicate
// that chose what it ran over.
//
// The class is therefore asked of signatureContent itself, over every octet a source can be
// rather than the three the RFC names today: a source binds its position when the preimage moves
// as the group id and the leaf index move, and a source production refuses outright has no
// preimage at all and binds nothing. Both halves of the select are required to be non-empty, so
// a signatureContent that answered one arm for everything fails here rather than agreeing with
// whatever the predicate says.
func TestLeafNodeSourceBindsItsPositionIsWhatSignatureContentActuallyBinds(t *testing.T) {
	leaf := aPublishedLeafNode(t)
	here := []byte{0x01, 0x02, 0x03, 0x04}
	elsewhere := []byte{0x05, 0x06, 0x07, 0x08}
	bound, unbound, refused := []int{}, []int{}, []int{}
	for source := 0; source <= 0xff; source += 1 {
		probe := *leaf
		probe.LeafNodeSource = LeafNodeSource(source)
		signed, err := probe.signatureContent(here, LeafIndex(0))
		if err != nil {
			refused = append(refused, source)
			if leafNodeSourceBindsItsPosition(LeafNodeSource(source)) {
				t.Errorf("signatureContent refuses source %d outright, so it has no LeafNodeTBS to bind anything, and the predicate calls it context bound",
					source)
			}
			continue
		}
		atAnotherGroup, err := probe.signatureContent(elsewhere, LeafIndex(0))
		if err != nil {
			t.Fatalf("source %d: signatureContent answered for one group id and refused another: %v", source, err)
		}
		atAnotherIndex, err := probe.signatureContent(here, LeafIndex(1))
		if err != nil {
			t.Fatalf("source %d: signatureContent answered at one leaf index and refused another: %v", source, err)
		}
		movesWithGroupId := !bytes.Equal(signed, atAnotherGroup)
		movesWithLeafIndex := !bytes.Equal(signed, atAnotherIndex)
		// both or neither, because section 7.2's select puts the group id and the leaf index in
		// one arm: a preimage carrying one of them is a structure the RFC does not describe.
		if movesWithGroupId != movesWithLeafIndex {
			t.Errorf("source %d: the preimage moves with the group id (%v) and with the leaf index (%v), and section 7.2 puts both in one arm of the select",
				source, movesWithGroupId, movesWithLeafIndex)
		}
		if movesWithGroupId {
			bound = append(bound, source)
		} else {
			unbound = append(unbound, source)
		}
		if got := leafNodeSourceBindsItsPosition(LeafNodeSource(source)); got != movesWithGroupId {
			t.Errorf("signatureContent binds the position for source %d (%v) and leafNodeSourceBindsItsPosition answers %v",
				source, movesWithGroupId, got)
		}
	}
	if len(bound) == 0 || len(unbound) == 0 || len(refused) == 0 {
		t.Fatalf("the derivation found %d sources binding their position, %d not binding it and %d production refuses; a predicate held against a class with an empty half is held against nothing",
			len(bound), len(unbound), len(refused))
	}
	t.Logf("signatureContent binds the position for source(s) %v, ignores it for %v, and refuses %d of the 256 octets a source can be",
		bound, unbound, len(refused))
}
