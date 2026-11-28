package connect

// Root-cause tests for the camouflage front-name selection and the extender's
// coverage of the names its supported clients front it with (EXTENDER-UPDATE2
// §3.1, §3.2).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"testing"
	"time"
)

func testIdentityKey(t *testing.T, seed byte) []byte {
	t.Helper()
	key := make([]byte, ed25519.PublicKeySize)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

// Every client of a list version fronts a given extender with the same name,
// and that name is one of the list (rendezvous selection, §3.1).
func TestExtenderCamouflageFrontNameIsDeterministicAndFromTheList(t *testing.T) {
	key := testIdentityKey(t, 1)
	names := []string{"a.example", "b.example", "c.example", "d.example"}
	first := ExtenderCamouflageFrontName(key, names)
	if first == "" || !slices.Contains(names, first) {
		t.Fatalf("front name %q is not one of the list", first)
	}
	// deterministic: the same key and list always select the same name
	for i := 0; i < 5; i += 1 {
		if got := ExtenderCamouflageFrontName(key, names); got != first {
			t.Fatalf("front name was not deterministic: %q then %q", first, got)
		}
	}
	// order independent: a shuffled list selects the same name
	shuffled := []string{"d.example", "c.example", "b.example", "a.example"}
	if got := ExtenderCamouflageFrontName(key, shuffled); got != first {
		t.Fatalf("front name depended on list order: %q vs %q", first, got)
	}
	// no key selects nothing, so the camouflaged attempt is dropped
	if got := ExtenderCamouflageFrontName(nil, names); got != "" {
		t.Fatalf("front name = %q with no key, expected empty", got)
	}
}

// Different extenders spread across the list rather than all choosing one name,
// so one borrowed site is not every extender's front (§3.1 stability/spread).
func TestExtenderCamouflageFrontNameSpreadsAcrossExtenders(t *testing.T) {
	names := []string{"a.example", "b.example", "c.example", "d.example", "e.example"}
	counts := map[string]int{}
	for i := 0; i < 200; i += 1 {
		key := make([]byte, ed25519.PublicKeySize)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		counts[ExtenderCamouflageFrontName(key, names)] += 1
	}
	if len(counts) != len(names) {
		t.Fatalf("front names used %d of %d borrowed sites: %v", len(counts), len(names), counts)
	}
}

// Removing a name moves only the extenders whose top name it was; every other
// extender keeps its front across the list change (§3.1 staged change).
func TestExtenderCamouflageFrontNameStableAcrossListChange(t *testing.T) {
	before := []string{"a.example", "b.example", "c.example", "d.example"}
	after := []string{"a.example", "b.example", "d.example"} // c removed
	moved := 0
	total := 300
	for i := 0; i < total; i += 1 {
		key := make([]byte, ed25519.PublicKeySize)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		if ExtenderCamouflageFrontName(key, before) != ExtenderCamouflageFrontName(key, after) {
			moved += 1
		}
	}
	// only the extenders whose top name was the removed one move; with four
	// names that is about a quarter, so well under half
	if moved*2 >= total {
		t.Fatalf("%d of %d extenders moved front on one name removal, expected few", moved, total)
	}
}

// A staged name (selectable from a later version) is not selected by current
// clients but is accepted and verified by the extender, and a retiring name is
// still selected by the versions it covers (§3.1, §3.2).
func TestExtenderBorrowListStagedAndRetiring(t *testing.T) {
	borrowList := &ExtenderBorrowList{
		Version: 5,
		Names: []ExtenderBorrowName{
			{Name: "current.example"},
			{Name: "staged.example", SelectableFromVersion: 6},
			{Name: "retiring.example", SelectableUntilVersion: 5},
		},
	}
	selectable := borrowList.SelectableNames(borrowList.Version)
	if slices.Contains(selectable, "staged.example") {
		t.Fatal("a staged name was selectable at the current version")
	}
	if slices.Contains(selectable, "retiring.example") {
		t.Fatal("a retiring name was still selectable at the version it retires in")
	}
	if !slices.Contains(selectable, "current.example") {
		t.Fatal("the current name was not selectable")
	}
	// the previous version still selects the retiring name
	if !slices.Contains(borrowList.SelectableNames(4), "retiring.example") {
		t.Fatal("a retiring name was not selectable by the previous version")
	}
	// the extender accepts and verifies all three
	accept := borrowList.AcceptNames()
	for _, name := range []string{"current.example", "staged.example", "retiring.example"} {
		if !slices.Contains(accept, name) {
			t.Fatalf("accept names missing %q", name)
		}
	}
}

// An extender covers its supported clients only when it has a current
// verification of every name they front it with, across the covered versions
// and the global and country lists (§3.1, §3.3).
func TestEvaluateExtenderCamouflageCoverage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testIdentityKey(t, 7)
	borrowLists := []*ExtenderBorrowList{
		{
			CountryCode:       "",
			Version:           4,
			Measured:          true,
			MeasuredAt:        now.Add(-time.Hour),
			MaxMeasurementAge: 30 * 24 * time.Hour,
			Names: []ExtenderBorrowName{
				{Name: "a.example"}, {Name: "b.example"}, {Name: "c.example"},
			},
		},
	}
	// the names supported clients (v3, v4, v5) front this extender with
	required := EvaluateExtenderCamouflageCoverage(key, borrowLists, map[string]time.Time{}, now).RequiredNames
	if len(required) == 0 {
		t.Fatal("no required names computed")
	}
	// verify all of them recently: coverage complete
	verified := map[string]time.Time{}
	for _, name := range borrowLists[0].AcceptNames() {
		verified[name] = now.Add(-time.Minute)
	}
	coverage := EvaluateExtenderCamouflageCoverage(key, borrowLists, verified, now)
	if !coverage.Complete() {
		t.Fatalf("coverage not complete with every name verified: %+v", coverage)
	}
	// let one required name's verification go stale: coverage is now degraded
	stale := map[string]time.Time{}
	for name, verifiedTime := range verified {
		stale[name] = verifiedTime
	}
	stale[required[0]] = now.Add(-60 * 24 * time.Hour)
	coverage = EvaluateExtenderCamouflageCoverage(key, borrowLists, stale, now)
	if coverage.Complete() {
		t.Fatal("coverage reported complete with a stale required-name verification")
	}
	if !slices.Contains(coverage.MissingNames, required[0]) {
		t.Fatalf("missing names %v did not name the stale required name %q", coverage.MissingNames, required[0])
	}
}

// An unmeasured or stale list makes coverage incomplete even when every name is
// verified: the list itself cannot be trusted to name what clients select
// (§3.1, §3.3).
func TestEvaluateExtenderCamouflageCoverageStaleList(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testIdentityKey(t, 9)
	borrowLists := []*ExtenderBorrowList{
		{
			CountryCode:       "",
			Version:           2,
			Measured:          true,
			MeasuredAt:        now.Add(-90 * 24 * time.Hour),
			MaxMeasurementAge: 30 * 24 * time.Hour,
			Names:             []ExtenderBorrowName{{Name: "a.example"}},
		},
	}
	verified := map[string]time.Time{"a.example": now}
	coverage := EvaluateExtenderCamouflageCoverage(key, borrowLists, verified, now)
	if coverage.StaleListCount != 1 || coverage.Complete() {
		t.Fatalf("a stale list was treated as coverage: %+v", coverage)
	}
}

// A hostname the extender does not cover is a degraded outcome, not success: an
// accept name without a current verification is listed so a prober fronting it
// is known to be substituted or terminated (§3.1).
func TestEvaluateExtenderCamouflageCoverageUnverifiedAcceptName(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testIdentityKey(t, 11)
	borrowLists := []*ExtenderBorrowList{
		{
			Version:           1,
			Measured:          true,
			MeasuredAt:        now,
			MaxMeasurementAge: 30 * 24 * time.Hour,
			Names:             []ExtenderBorrowName{{Name: "a.example"}, {Name: "b.example"}},
		},
	}
	// verify only one of the two accept names
	verified := map[string]time.Time{"a.example": now}
	coverage := EvaluateExtenderCamouflageCoverage(key, borrowLists, verified, now)
	if !slices.Contains(coverage.UnverifiedAcceptNames, "b.example") {
		t.Fatalf("unverified accept names %v did not name b.example", coverage.UnverifiedAcceptNames)
	}
}

// The front-name score is domain separated and length prefixed, so no other
// construction over the identity key produces it and no two (key, name) pairs
// collide by concatenation (§3.1 conservative construction).
func TestExtenderCamouflageFrontNameScoreDomainSeparated(t *testing.T) {
	key := testIdentityKey(t, 3)
	// "ab"+"c" and "a"+"bc" must not score the same name pair identically
	if bytes.Equal(
		extenderCamouflageFrontNameScore(append(slices.Clone(key), 'a'), "bc"),
		extenderCamouflageFrontNameScore(append(slices.Clone(key), 'a', 'b'), "c"),
	) {
		t.Fatal("length-prefixing failed: two different (key,name) pairs scored identically")
	}
}
