// The vector family registry: the one place a runner for an mlswg family is installed,
// and the one loader every runner reads the corpus through.
//
// This is p8 task 7's contract, landed here because p4 task 16 is the first runner to
// register against it and a contract with no implementation cannot be registered against.
// The surface is the registry's, not this task's: VectorFamily, RegisterVectorFamily,
// LoadVectorFile, MustHex and HexOf keep the signatures the interface registry pins, so
// p8's own landing is a replacement of this file rather than a reconciliation of two.
//
// Why one loader and one hex decoder rather than one per family. Three parallel hex
// decoders over one corpus is how two of them end up disagreeing about the empty string:
// one returns nil, one returns an empty slice, and a family whose vector carries an absent
// field then verifies under one runner and not the other. There is one of each here.
//
// Where this file diverges from p8 task 7's literal text, so that landing is a REPLACEMENT
// and not a reconciliation of two half-registries. Five differences, each deliberate:
//
//  1. vectorManifest is a variable initializer over baseVectorManifest() rather than a map
//     literal populated from init(). p8's spelling is an ordering hazard: init() functions
//     run in file name order, so every *_kat_test.go that sorts before vectors_test.go
//     registers first and is then overwritten by the runnerless declarations. The registry
//     ends up with sixteen nil runners and a green gate. See the note on vectorManifest.
//  2. LoadVectorFile is split over vectorFileEntries, which returns the error instead of
//     reporting it, so the unreadable-corpus path is reachable from a test.
//  3. RegisterVectorFamily keeps the manifest's own Number, Name, File and Slice, so a
//     runner cannot rename its family or repoint it at another corpus file on the way in.
//  4. expectedPendingFamilies omits 6, which p4 task 16 installs from
//     key_schedule_kat_test.go; p8 task 7's list is 1..15.
//  5. TestVectorGenerateThenVerify is p8 task 9's and is pulled forward to here, because
//     family 6 ships a generator in the same commit that installs it and an uninvoked
//     Generate is an unexercised one.
//
// The manifest rows follow p8 rather than the p4 plan's own task 16 snippet where the two
// disagree: family 6 is Slice A3 and Name "Pre-shared keys", not Slice A2 and "psk_secret".
//
// The failure this whole file exists to make impossible is a runner that ran nothing
// reporting exactly what a runner that passed everything reports. Three things stand
// against it: a family with no Verify must be named in expectedPendingFamilies, so a
// runner cannot quietly stop being installed; LoadVectorFile is fatal rather than skipping
// on a corpus it cannot read, so an absent file is a red suite and not a green one; and
// every family's own runner counts its comparisons and asserts the count.
package mls

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// VectorFamily is one row of spec A section 4.2.1.
type VectorFamily struct {
	// Number is the section 4.2.1 row, 1 to 16.
	Number int
	// Name is the human label, "Tree math".
	Name string
	// File is the basename under testdata/vectors.
	File string
	// Slice is the slice this family must pass in: A1, A2, A3 or A4.
	Slice string
	// Verify checks one case of the pinned file. nil means not yet implemented, and the
	// number must then appear in expectedPendingFamilies.
	Verify func(t *testing.T, raw json.RawMessage)
	// Generate produces fresh cases from an implementation, which Verify is then run
	// against. nil where nothing independent of Verify can produce them -- a generator
	// that shares Verify's code path closes no loop, it only agrees with itself.
	Generate func(t *testing.T) json.RawMessage
}

// vectorManifest is every family, keyed by its section 4.2.1 number, built from the
// descriptive rows below.
//
// It is a variable initializer and NOT an init() function, and that is load bearing. Go
// runs a package's init() functions in file name order, so a manifest populated from an
// init() here is populated AFTER every runner file whose name sorts before this one --
// crypto_basics_kat_test.go, key_schedule_kat_test.go, and every other *_kat_test.go there
// will ever be -- has already registered. Their rows are then overwritten by the runnerless
// declarations, TestVectorFamiliesVerify runs nothing, and gate 1 is green with sixteen of
// sixteen families never executed, which is the exact outcome this registry exists to make
// impossible. Package level variables are fully initialized before any init() runs, so
// building the base here makes registration order irrelevant.
var vectorManifest = baseVectorManifest()

// expectedPendingFamilies is every family with no runner yet, ascending. It shrinks to the
// empty slice by the end of slice A4 and never grows.
//
// 4 is absent because p6 task 17 registers it from message_protection_kat_test.go,
// 12 because p6 task 18 registers it from messages_kat_test.go,
// 5 and 6 because p4 tasks 17 and 16 register them from key_schedule_kat_test.go,
// 10 and 11 because p5 tasks 24 and 25 register them from tree_kat_test.go,
// 7 because p4 task 20 registers it from transcript_kat_test.go, and 3 because p4 task 25
// registers it from secret_tree_kat_test.go. 16 is still
// here even though package syntax ships a working runner for it: that runner is not
// installed in this manifest, and a family verified somewhere this registry cannot see is,
// as far as this gate is concerned, a family nothing runs. p8 task 8 is the shim.
//
// 1 left this list when tree_math_kat_test.go landed, and the reason it was still HERE is
// worth recording, because it is the failure mode this gate has: p3's tree math shipped onto
// this branch with its corpus vendored and pinned and nothing running it, and this list --
// written when that code was elsewhere -- reported the family as pending and passed. A family
// whose implementation has landed is not pending, it is uncovered, and the two are
// indistinguishable from here. Families 2 and 16 are in that position today: crypto-basics
// covers RefHash, ExpandWithLabel, DeriveSecret, DeriveTreeSecret, SignWithLabel and
// EncryptWithLabel, all of which this package declares, and 16's runner exists in package
// syntax and is not installed here. Both are owned by tasks that are not this one, and both
// are named here so that "pending" cannot go on meaning two different things silently.
var expectedPendingFamilies = []int{2, 8, 9, 13, 14, 15, 16}

// RegisterVectorFamily installs a family runner. Registering a number twice, or a number
// outside 1 to 16, is a programming error rather than a condition to report: both mean the
// manifest below and the caller disagree about what the families are.
func RegisterVectorFamily(family VectorFamily) {
	if family.Number < 1 || family.Number > 16 {
		panic("mls: vector family number out of range")
	}
	existing, ok := vectorManifest[family.Number]
	if ok && existing.Verify != nil && family.Verify != nil {
		panic("mls: vector family " + existing.File + " registered twice")
	}
	// the row's descriptive half stays the manifest's, so a runner cannot rename its own
	// family or repoint it at another corpus file on the way in.
	if ok {
		family.Number = existing.Number
		family.Name = existing.Name
		family.File = existing.File
		family.Slice = existing.Slice
	}
	vectorManifest[family.Number] = family
}

// baseVectorManifest is the 16 families, declared with no runner. Each owning plan
// re-registers its own row with Verify, and with Generate where an independent generator
// exists. See the note on vectorManifest for why this is a function called from a variable
// initializer rather than an init().
func baseVectorManifest() map[int]VectorFamily {
	manifest := map[int]VectorFamily{}
	for _, family := range []VectorFamily{
		{Number: 1, Name: "Tree math", File: "tree-math.json", Slice: "A2"},
		{Number: 2, Name: "Crypto basics", File: "crypto-basics.json", Slice: "A2"},
		{Number: 3, Name: "Secret tree", File: "secret-tree.json", Slice: "A3"},
		{Number: 4, Name: "Message protection", File: "message-protection.json", Slice: "A3"},
		{Number: 5, Name: "Key schedule", File: "key-schedule.json", Slice: "A3"},
		{Number: 6, Name: "Pre-shared keys", File: "psk_secret.json", Slice: "A3"},
		{Number: 7, Name: "Transcript hashes", File: "transcript-hashes.json", Slice: "A3"},
		{Number: 8, Name: "Welcome", File: "welcome.json", Slice: "A4"},
		{Number: 9, Name: "Tree operations", File: "tree-operations.json", Slice: "A2"},
		{Number: 10, Name: "Tree validation", File: "tree-validation.json", Slice: "A2"},
		{Number: 11, Name: "TreeKEM", File: "treekem.json", Slice: "A4"},
		{Number: 12, Name: "Messages", File: "messages.json", Slice: "A4"},
		{Number: 13, Name: "Passive client, welcome", File: "passive-client-welcome.json", Slice: "A4"},
		{Number: 14, Name: "Passive client, handling commit", File: "passive-client-handling-commit.json", Slice: "A4"},
		{Number: 15, Name: "Passive client, random", File: "passive-client-random.json", Slice: "A4"},
		{Number: 16, Name: "Vector deserialization", File: "deserialization.json", Slice: "A1"},
	} {
		manifest[family.Number] = family
	}
	return manifest
}

// vectorFileEntries reads a pinned family file as a json array of cases.
//
// The error is returned rather than reported, so the unreadable corpus path is reachable
// by a test. LoadVectorFile below is the only caller that turns it into a verdict, and it
// turns it into a fatal one.
func vectorFileEntries(file string) ([]json.RawMessage, error) {
	body, err := os.ReadFile(filepath.Join("testdata", "vectors", file))
	if err != nil {
		return nil, fmt.Errorf("read vector file %s: %w", file, err)
	}
	entries := []json.RawMessage{}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse vector file %s: %w", file, err)
	}
	return entries, nil
}

// LoadVectorFile reads a pinned family file as a json array of cases, and fails the test
// if it cannot.
//
// Fatal and never skipped, deliberately. A skipped gate is an absent gate: a corpus that
// was deleted, renamed, or truncated to an empty array would take every family runner in
// this package out of the suite at once, and a skip reports that as a run with nothing
// wrong. The whole argument that this package interoperates is the corpus, so failing to
// read it is the loudest thing that can happen here rather than the quietest.
func LoadVectorFile(t *testing.T, file string) []json.RawMessage {
	t.Helper()
	entries, err := vectorFileEntries(file)
	if err != nil {
		t.Fatalf("%v; the corpus is the whole of this family's evidence, so an unreadable one is a failure and not a skip", err)
	}
	return entries
}

// MustHex decodes a vector's hex field. The mlswg files use lowercase hex with no prefix
// and an empty string for an absent value, which decodes to an empty slice rather than to
// nil so a caller comparing lengths sees 0 either way.
func MustHex(t *testing.T, s string) []byte {
	t.Helper()
	if s == "" {
		return []byte{}
	}
	body, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return body
}

// HexOf is the inverse, for the generate direction and for failure messages.
func HexOf(b []byte) string {
	return hex.EncodeToString(b)
}

// TestVectorManifestIsComplete holds the manifest to spec A section 4.2.1, and holds the
// set of families with no runner to the set this tree says has no runner.
//
// The pending set is the part that matters. A runner deleted, or an init() that stopped
// being linked, removes a family from the run and leaves every other assertion in this
// package untouched; comparing the derived pending set against a written one is what makes
// that a failure rather than a quieter suite.
func TestVectorManifestIsComplete(t *testing.T) {
	if len(vectorManifest) != 16 {
		t.Fatalf("manifest holds %d families, spec A section 4.2.1 names 16", len(vectorManifest))
	}
	for number := 1; number <= 16; number++ {
		family, ok := vectorManifest[number]
		if !ok {
			t.Fatalf("family %d is not in the manifest", number)
		}
		if family.File == "" || family.Name == "" || family.Slice == "" {
			t.Errorf("family %d is under-specified: %+v", number, family)
		}
		if family.Number != number {
			t.Errorf("family %d is filed under %d", family.Number, number)
		}
	}
	// every family must name a file the corpus pin actually covers.
	pinned := map[string]bool{}
	for _, name := range vectorFiles {
		pinned[name] = true
	}
	if len(pinned) != len(vectorFiles) || len(pinned) == 0 {
		t.Fatalf("the pinned file set read %d names out of %d, so the check below would hold vacuously",
			len(pinned), len(vectorFiles))
	}
	for number, family := range vectorManifest {
		if !pinned[family.File] {
			t.Errorf("family %d names %s, which is not in VECTORS.sha256", number, family.File)
		}
	}
	pending := []int{}
	for number, family := range vectorManifest {
		if family.Verify == nil {
			pending = append(pending, number)
		}
	}
	slices.Sort(pending)
	if !slices.Equal(pending, expectedPendingFamilies) {
		t.Fatalf("pending families %v, expected %v; update expectedPendingFamilies in the same commit that lands or drops a runner",
			pending, expectedPendingFamilies)
	}
}

// TestVectorFamiliesVerify offers every installed family every case of its pinned file.
//
// The number of families run is asserted rather than left implicit: this loop over a
// manifest of sixteen nil Verify funcs completes instantly and reports PASS, which is the
// shape gate 1 has to be unable to reach.
//
// What this loop counts is cases OFFERED, and the log line says so. It cannot count cases
// compared: Verify returns nothing, so a family that declined every case it was handed --
// because the case is at a ciphersuite it does not implement, which is the normal
// condition for five of the seven suites the mlswg files publish -- is indistinguishable
// here from one that checked all of them. Family 6 is offered 77 cases and compares 22 of
// them. The honest number is each family's own, asserted in each family's own runner
// against a written count; this number is an upper bound and reading it as coverage
// overstates the run by whatever the suite filter dropped.
func TestVectorFamiliesVerify(t *testing.T) {
	families, offered := 0, 0
	for number := 1; number <= 16; number++ {
		family := vectorManifest[number]
		if family.Verify == nil {
			continue
		}
		families++
		entries := LoadVectorFile(t, family.File)
		if len(entries) == 0 {
			t.Fatalf("family %d (%s) has no cases", number, family.File)
		}
		for index, raw := range entries {
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("family %d case %d panicked: %v", number, index, recovered)
					}
				}()
				family.Verify(t, raw)
			}()
			offered++
		}
	}
	if want := 16 - len(expectedPendingFamilies); families != want {
		t.Fatalf("ran %d families and the manifest says %d are installed, so this loop and the manifest disagree about what runs",
			families, want)
	}
	if families == 0 {
		t.Fatal("no family is installed, so gate 1 is green with nothing behind it")
	}
	t.Logf("%d families verified; %d published cases offered to them, of which each family's own runner asserts how many it compared",
		families, offered)
}

// TestVectorGenerateThenVerify closes the loop verification alone cannot: a pinned vector
// never passes through our encoder, so an encoder and a decoder that are wrong in the same
// direction verify perfectly. Generating a fresh case and feeding it back through the
// verifier sees that -- but only if the generator is not the verifier wearing a hat, which
// is the property each family owes and which family 6 asserts in key_schedule_kat_test.go.
func TestVectorGenerateThenVerify(t *testing.T) {
	generated := 0
	for number := 1; number <= 16; number++ {
		family := vectorManifest[number]
		if family.Generate == nil || family.Verify == nil {
			continue
		}
		generated++
		raw := family.Generate(t)
		entries := []json.RawMessage{}
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatalf("family %d generated a value that is not an array of cases: %v", number, err)
		}
		if len(entries) == 0 {
			t.Fatalf("family %d generated no cases", number)
		}
		for _, generatedCase := range entries {
			family.Verify(t, generatedCase)
		}
		t.Logf("family %d (%s): %d generated cases verified", number, family.Name, len(entries))
	}
	if generated == 0 {
		t.Fatal("no installed family supports generation, so the generate direction of spec A section 4.2.1 is unexercised")
	}
}

// TestLoadVectorFileIsFatalOnACorpusItCannotRead is the control on the paragraph above
// LoadVectorFile.
//
// The reachable half of the claim is checked here: an absent file, and a file that is not
// an array of cases, both produce an error rather than an empty result that reads as a
// corpus with nothing in it. That the error becomes a fatal verdict and never a skip is
// the other half, and it is asserted structurally by TestNoVectorRunnerCanSkip in
// key_schedule_kat_test.go, because a t.Fatalf cannot be observed from inside the test it
// terminates.
func TestLoadVectorFileIsFatalOnACorpusItCannotRead(t *testing.T) {
	if _, err := vectorFileEntries("this-family-was-never-vendored.json"); err == nil {
		t.Fatal("an absent corpus file loaded without error, so a deleted corpus would take every family out of the suite silently")
	}
	if _, err := vectorFileEntries("VECTORS.sha256"); err == nil {
		t.Fatal("a file that is not a json array of cases loaded without error")
	}
	// and the positive half, or the two refusals above are just everything failing.
	entries, err := vectorFileEntries("psk_secret.json")
	if err != nil {
		t.Fatalf("the vendored psk_secret corpus did not load: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the vendored psk_secret corpus loaded as zero cases")
	}
}

// TestMustHexAndHexOfAreInverses pins the one decoder against the one encoder, including
// over the empty string, which is the case the interface registry says three parallel
// decoders end up disagreeing about.
func TestMustHexAndHexOfAreInverses(t *testing.T) {
	for _, text := range []string{"", "00", "ff00", "aa6ee4e7ac86bec0a39c185ad88995e9"} {
		if got := HexOf(MustHex(t, text)); got != text {
			t.Errorf("HexOf(MustHex(%q)) = %q", text, got)
		}
	}
	decoded := MustHex(t, "")
	if decoded == nil || len(decoded) != 0 {
		t.Errorf("MustHex of the empty string is %#v, want a non-nil empty slice", decoded)
	}
}
