package connect

// Release outcomes, retry timing and capability eligibility (EXTENDER-UPDATE2
// §1.1, §1.2, §3.2), and the blocked state's outage rule (§1.3). Every clock
// is the fake one and every secret is pinned, so each transition is exact.

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A trusted identity's request.
func testTrustedReleaseRequest(clock *testClock, identity string, countryCode string) *ExtenderReleaseRequest {
	return &ExtenderReleaseRequest{
		Identity:           []byte(identity),
		IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
		Vantage:            "198.51.100.0/24",
		CountryCode:        countryCode,
	}
}

// When the epoch holding `now` ends, computed independently of the policy.
func testReleaseEpochEnd(now time.Time) time.Time {
	epochMs := ExtenderReleaseEpochTimeout.Milliseconds()
	return time.UnixMilli((now.UnixMilli()/epochMs + 1) * epochMs)
}

// The identity's partition members in the epoch's order.
func testReleaseOrder(clock *testClock, identity string, keyHexes []string) []string {
	members, _, _ := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelGated, []byte(identity), keyHexes)
	epoch := ExtenderEpoch(clock.Now(), ExtenderReleaseEpochTimeout)
	return ExtenderPartitionOrder(testPartitionSecret, ExtenderChannelGated, []byte(identity), epoch, members)
}

func testWithout(keyHexes []string, removed ...string) []string {
	kept := []string{}
	for _, keyHex := range keyHexes {
		if !slices.Contains(removed, keyHex) {
			kept = append(kept, keyHex)
		}
	}
	return kept
}

// Every outcome a release can answer with. An answer outside it would be a
// value a client must treat as empty.
var testReleaseOutcomes = []string{
	ExtenderReleaseOutcomeOk,
	ExtenderReleaseOutcomeEmpty,
	ExtenderReleaseOutcomeEpochSpent,
	ExtenderReleaseOutcomeProbation,
	ExtenderReleaseOutcomeCountryCapacity,
	ExtenderReleaseOutcomeRateLimited,
	ExtenderReleaseOutcomeIncompatible,
	ExtenderReleaseOutcomeUnavailable,
}

// The wire names are contract R's; a rename strands every client that reads
// them.
func TestExtenderReleaseOutcomeWireNames(t *testing.T) {
	want := []string{"ok", "empty", "epoch_spent", "probation", "country_capacity", "rate_limited", "incompatible", "unavailable"}
	if !slices.Equal(testReleaseOutcomes, want) {
		t.Fatalf("outcomes = %v, want %v", testReleaseOutcomes, want)
	}
}

// The gated client reads the answer through its normalizer, which treats a name
// it does not know as empty (contract R). Every outcome the policy can answer
// with must reach the client as itself, or a refusal with a retry time would
// read as an empty release, and the client's counts must label each one.
func TestExtenderReleaseOutcomesReachTheGatedClient(t *testing.T) {
	labels := ExtenderGatedReleaseOutcomes()
	for _, outcome := range testReleaseOutcomes {
		if normalized := NormalizeExtenderGatedReleaseOutcome(outcome); normalized != outcome {
			t.Errorf("outcome %q reaches the client as %q", outcome, normalized)
		}
		if !slices.Contains(labels, outcome) {
			t.Errorf("outcome %q has no label in the client's counts %v", outcome, labels)
		}
	}
	if normalized := NormalizeExtenderGatedReleaseOutcome("a-newer-outcome"); normalized != ExtenderReleaseOutcomeEmpty {
		t.Errorf("an unknown outcome reaches the client as %q, want empty", normalized)
	}
}

// A release with records is ok; an admitted release with nothing eligible is
// empty, and neither carries a retry time.
func TestExtenderReleaseOutcomeOkAndEmpty(t *testing.T) {
	clock := newTestClock()
	keyHexes := testGatedKeyHexes(t, 16)
	order := testReleaseOrder(clock, "identity-ok", keyHexes)
	blocked := testBlockedSource{"aa": {}}
	for _, keyHex := range testReleaseOrder(clock, "identity-blocked", keyHexes) {
		blocked["aa"][keyHex] = true
	}
	policy := newTestReleasePolicy(t, clock, blocked, nil)

	result, err := policy.Release(testTrustedReleaseRequest(clock, "identity-ok", "bb"), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ExtenderReleaseOutcomeOk || result.RetryAfter != 0 || !slices.Equal(result.KeyHexes, order[:3]) {
		t.Fatalf("release = %+v, want ok with the epoch's first three of %v", result, order)
	}

	cases := []struct {
		name     string
		keyHexes []string
		request  func() *ExtenderReleaseRequest
	}{
		{
			name:     "no gated fleet",
			keyHexes: nil,
			request:  func() *ExtenderReleaseRequest { return testTrustedReleaseRequest(clock, "identity-empty-fleet", "bb") },
		},
		{
			name:     "no record of the family",
			keyHexes: keyHexes,
			request: func() *ExtenderReleaseRequest {
				request := testTrustedReleaseRequest(clock, "identity-empty-family", "bb")
				request.Eligible = func(string) bool { return false }
				return request
			},
		},
		{
			name:     "every record blocked in the country",
			keyHexes: keyHexes,
			request:  func() *ExtenderReleaseRequest { return testTrustedReleaseRequest(clock, "identity-blocked", "aa") },
		},
	}
	for _, c := range cases {
		result, err := policy.Release(c.request(), c.keyHexes)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if result.Outcome != ExtenderReleaseOutcomeEmpty || result.RetryAfter != 0 || len(result.KeyHexes) != 0 {
			t.Errorf("%s: %+v, want empty with no retry time", c.name, result)
		}
	}
}

// Root cause guarded: a client told nothing about a spent epoch asks for new
// partitions until one succeeds (§1.2). A spent epoch whose records are all
// gone answers epoch_spent, never refunds, and names the epoch's end, after
// which the next request is dealt again.
func TestExtenderReleaseSpentEpochAnswersEpochSpentUntilItEnds(t *testing.T) {
	clock := newTestClock()
	clock.advance(36 * time.Hour)
	policy := newTestReleasePolicy(t, clock, nil, nil)
	keyHexes := testGatedKeyHexes(t, 16)
	request := testTrustedReleaseRequest(clock, "identity-spent", "aa")

	first, err := policy.Release(request, keyHexes)
	if err != nil || len(first.KeyHexes) != 3 {
		t.Fatalf("first release: %+v, %v", first, err)
	}
	// every issued record leaves the fleet: the budget stays spent
	remaining := testWithout(keyHexes, first.KeyHexes...)
	for _, advance := range []time.Duration{0, time.Hour, 5 * 24 * time.Hour} {
		clock.advance(advance)
		spent, err := policy.Release(request, remaining)
		if err != nil {
			t.Fatal(err)
		}
		if spent.Outcome != ExtenderReleaseOutcomeEpochSpent || len(spent.KeyHexes) != 0 {
			t.Fatalf("after %s: %+v, want epoch_spent with nothing released", advance, spent)
		}
		if want := testReleaseEpochEnd(clock.Now()).Sub(clock.Now()); spent.RetryAfter != want || spent.RetryAfter <= 0 {
			t.Fatalf("after %s: retry after %s, want the epoch's end in %s", advance, spent.RetryAfter, want)
		}
		if spent.Count != 3 || spent.Probation {
			t.Fatalf("a spent epoch changed the entitlement: %+v", spent)
		}
	}
	clock.advance(testReleaseEpochEnd(clock.Now()).Sub(clock.Now()))
	next, err := policy.Release(request, remaining)
	if err != nil || next.Outcome != ExtenderReleaseOutcomeOk || len(next.KeyHexes) == 0 {
		t.Fatalf("the next epoch: %+v, %v", next, err)
	}
}

// An identity in probation that spent its one record answers probation, and
// its retry time is whichever ends first: the probation, which raises its
// count within the epoch, or the epoch.
func TestExtenderReleaseProbationRetriesAtTheSoonerEnd(t *testing.T) {
	clock := newTestClock()
	clock.advance(2 * time.Hour)
	policy := newTestReleasePolicy(t, clock, nil, nil)
	keyHexes := testGatedKeyHexes(t, 16)
	epochEnd := testReleaseEpochEnd(clock.Now())

	cases := []struct {
		name       string
		createTime time.Time
		retryAfter time.Duration
	}{
		{name: "a day short of trusted", createTime: clock.Now().Add(-ExtenderReleaseProbationTimeout + 24*time.Hour), retryAfter: 24 * time.Hour},
		{name: "a day old", createTime: clock.Now().Add(-24 * time.Hour), retryAfter: epochEnd.Sub(clock.Now())},
		{name: "unknown age", createTime: time.Time{}, retryAfter: epochEnd.Sub(clock.Now())},
	}
	for _, c := range cases {
		request := &ExtenderReleaseRequest{
			Identity:           []byte("identity-" + c.name),
			IdentityCreateTime: c.createTime,
			Vantage:            "198.51.100.0/24",
			CountryCode:        "aa",
		}
		first, err := policy.Release(request, keyHexes)
		if err != nil || len(first.KeyHexes) != 1 || !first.Probation || first.Outcome != ExtenderReleaseOutcomeOk {
			t.Fatalf("%s: first release %+v, %v", c.name, first, err)
		}
		spent, err := policy.Release(request, testWithout(keyHexes, first.KeyHexes...))
		if err != nil {
			t.Fatal(err)
		}
		if spent.Outcome != ExtenderReleaseOutcomeProbation || len(spent.KeyHexes) != 0 || spent.RetryAfter != c.retryAfter {
			t.Fatalf("%s: %+v, want probation retrying after %s", c.name, spent, c.retryAfter)
		}
	}

	// the probation ends within the epoch: the count rises and the identity
	// that was told to wait for it is dealt the rest of its count
	clock.advance(24 * time.Hour)
	request := &ExtenderReleaseRequest{
		Identity:           []byte("identity-a day short of trusted"),
		IdentityCreateTime: clock.Now().Add(-ExtenderReleaseProbationTimeout),
		Vantage:            "198.51.100.0/24",
		CountryCode:        "aa",
	}
	trusted, err := policy.Release(request, keyHexes)
	if err != nil || trusted.Probation || trusted.Outcome != ExtenderReleaseOutcomeOk || len(trusted.KeyHexes) != 3 {
		t.Fatalf("after probation: %+v, %v", trusted, err)
	}
}

// Every eligible record at its per-country cap answers country_capacity in
// that country alone, with no retry time: when a slot frees depends on other
// identities, which the answer must not describe.
func TestExtenderReleaseCountryCapacityOutcome(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, func(settings *ExtenderReleaseSettings) {
		settings.MaxClientsPerExtenderPerCountry = 1
	})
	keyHexes := testGatedKeyHexes(t, 1)
	if result, err := policy.Release(testTrustedReleaseRequest(clock, "first", "aa"), keyHexes); err != nil || result.Outcome != ExtenderReleaseOutcomeOk {
		t.Fatalf("first: %+v, %v", result, err)
	}
	full, err := policy.Release(testTrustedReleaseRequest(clock, "second", "aa"), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if full.Outcome != ExtenderReleaseOutcomeCountryCapacity || full.RetryAfter != 0 || len(full.KeyHexes) != 0 {
		t.Fatalf("at capacity: %+v", full)
	}
	if other, err := policy.Release(testTrustedReleaseRequest(clock, "second", "bb"), keyHexes); err != nil || other.Outcome != ExtenderReleaseOutcomeOk {
		t.Fatalf("another country: %+v, %v", other, err)
	}
}

// A request limit is a refusal whose outcome is rate_limited and whose retry
// time is the whole window from the refused request, which counts too. The
// refusal's message is the limit's, which a client that predates outcomes
// reads, and it unwraps to the limit.
func TestExtenderReleaseRateLimitRefusalCarriesTheWindow(t *testing.T) {
	for _, limit := range []error{ErrExtenderReleaseIdentityLimited, ErrExtenderReleaseVantageLimited} {
		configure := func(settings *ExtenderReleaseSettings) {
			if limit == ErrExtenderReleaseIdentityLimited {
				settings.IdentityRequestLimit = 2
			} else {
				settings.VantageRequestLimit = 2
			}
		}
		refused := func(clock *testClock, policy *ExtenderReleasePolicy, identity string) *ExtenderReleaseRefusal {
			t.Helper()
			_, err := policy.Release(testTrustedReleaseRequest(clock, identity, "aa"), testGatedKeyHexes(t, 3))
			var refusal *ExtenderReleaseRefusal
			if !errors.As(err, &refusal) || !errors.Is(err, limit) || err.Error() != limit.Error() {
				t.Fatalf("%v: refusal = %v", limit, err)
			}
			return refusal
		}
		identity := func(i int) string {
			if limit == ErrExtenderReleaseIdentityLimited {
				return "identity-shared"
			}
			return []string{"identity-a", "identity-b", "identity-c", "identity-d"}[i]
		}

		clock := newTestClock()
		policy := newTestReleasePolicy(t, clock, nil, configure)
		for i := range 2 {
			if _, err := policy.Release(testTrustedReleaseRequest(clock, identity(i), "aa"), testGatedKeyHexes(t, 3)); err != nil {
				t.Fatal(err)
			}
		}
		refusal := refused(clock, policy, identity(2))
		if refusal.Outcome != ExtenderReleaseOutcomeRateLimited || refusal.RetryAfter != ExtenderReleaseRequestWindow+time.Second {
			t.Fatalf("%v: %+v, want rate_limited after the window", limit, refusal)
		}
		// asking at the retry time is admitted
		clock.advance(refusal.RetryAfter)
		if _, err := policy.Release(testTrustedReleaseRequest(clock, identity(3), "aa"), testGatedKeyHexes(t, 3)); err != nil {
			t.Fatalf("%v: at the retry time: %v", limit, err)
		}

		// asking a moment before the retry time is refused again, and that
		// refusal counts, so the retry time was not early
		clock = newTestClock()
		policy = newTestReleasePolicy(t, clock, nil, configure)
		for i := range 2 {
			if _, err := policy.Release(testTrustedReleaseRequest(clock, identity(i), "aa"), testGatedKeyHexes(t, 3)); err != nil {
				t.Fatal(err)
			}
		}
		refused(clock, policy, identity(2))
		clock.advance(ExtenderReleaseRequestWindow)
		refused(clock, policy, identity(3))
	}
}

// A ledger that cannot take the identity is a refusal answering unavailable,
// with no retry time and the ledger's own error.
func TestExtenderReleaseLedgerFailureAnswersUnavailable(t *testing.T) {
	clock := newTestClock()
	settings := DefaultExtenderReleaseSettings()
	settings.Now = clock.Now
	ledger := NewExtenderReleaseMemoryLedger(settings.RequestWindow, settings.ClientWindow)
	ledger.SetBounds(1, 4, 4)
	policy := NewExtenderReleasePolicy(testPartitionSecret, ledger, nil, settings)
	if _, err := policy.Release(testTrustedReleaseRequest(clock, "first", "aa"), []string{"first"}); err != nil {
		t.Fatal(err)
	}
	_, err := policy.Release(testTrustedReleaseRequest(clock, "other", "aa"), []string{"second"})
	var refusal *ExtenderReleaseRefusal
	if !errors.As(err, &refusal) || refusal.Outcome != ExtenderReleaseOutcomeUnavailable || refusal.RetryAfter != 0 || !errors.Is(err, ErrExtenderReleaseLedgerFull) {
		t.Fatalf("full ledger: %v", err)
	}
}

// Root cause guarded (§3.2): a capability must change which records are
// eligible and nothing else. An incompatible record is skipped and never
// charged; declaring or dropping a capability within an epoch never refunds a
// disclosure or adds one beyond the count; the partition, count and epoch are
// the same either way; and a client without the capability is never handed a
// record that requires it.
func TestExtenderReleaseCapabilitiesChangeEligibilityOnly(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, nil)
	keyHexes := testGatedKeyHexes(t, 64)
	identity := "identity-capable"
	order := testReleaseOrder(clock, identity, keyHexes)
	if len(order) < 6 {
		t.Fatalf("the fixture partition holds %d records, want at least 6", len(order))
	}
	// the first two of the epoch's order require a capability
	requiring := map[string]bool{order[0]: true, order[1]: true}
	request := func(capable bool) *ExtenderReleaseRequest {
		request := testTrustedReleaseRequest(clock, identity, "aa")
		request.Compatible = func(keyHex string) bool { return capable || !requiring[keyHex] }
		return request
	}
	disclosed := map[string]bool{}
	release := func(capable bool) *ExtenderReleaseResult {
		t.Helper()
		result, err := policy.Release(request(capable), keyHexes)
		if err != nil {
			t.Fatal(err)
		}
		for _, keyHex := range result.KeyHexes {
			if !capable && requiring[keyHex] {
				t.Fatalf("a client without the capability was handed %s", keyHex)
			}
			disclosed[keyHex] = true
		}
		return result
	}

	legacy := release(false)
	if !slices.Equal(legacy.KeyHexes, order[2:5]) || legacy.Outcome != ExtenderReleaseOutcomeOk {
		t.Fatalf("legacy release = %v (%s), want the first three compatible %v", legacy.KeyHexes, legacy.Outcome, order[2:5])
	}
	capable := release(true)
	if !slices.Equal(capable.KeyHexes, legacy.KeyHexes) {
		t.Fatalf("declaring the capability changed a spent epoch's records: %v, want %v", capable.KeyHexes, legacy.KeyHexes)
	}
	for _, result := range []*ExtenderReleaseResult{legacy, capable} {
		if result.Count != 3 || result.Probation || result.Partition != legacy.Partition ||
			result.PartitionCount != legacy.PartitionCount || result.Epoch != legacy.Epoch {
			t.Fatalf("a capability changed the entitlement: %+v, want that of %+v", result, legacy)
		}
	}
	release(false)
	if len(disclosed) != 3 {
		t.Fatalf("capability changes disclosed %d records in one epoch, want the count of 3", len(disclosed))
	}

	// a fresh identity that declares the capability first is dealt the
	// epoch's order as it is; dropping the capability then releases only
	// what it was already given that it can use, and no replacement
	other := "identity-capable-first"
	otherOrder := testReleaseOrder(clock, other, keyHexes)
	otherRequiring := map[string]bool{otherOrder[0]: true}
	otherRequest := func(capable bool) *ExtenderReleaseRequest {
		request := testTrustedReleaseRequest(clock, other, "aa")
		request.Compatible = func(keyHex string) bool { return capable || !otherRequiring[keyHex] }
		return request
	}
	first, err := policy.Release(otherRequest(true), keyHexes)
	if err != nil || !slices.Equal(first.KeyHexes, otherOrder[:3]) {
		t.Fatalf("capable first: %+v, %v", first, err)
	}
	dropped, err := policy.Release(otherRequest(false), keyHexes)
	if err != nil || !slices.Equal(dropped.KeyHexes, otherOrder[1:3]) || dropped.Outcome != ExtenderReleaseOutcomeOk {
		t.Fatalf("dropping the capability: %+v, %v, want only %v", dropped, err, otherOrder[1:3])
	}
}

// Incompatible is the outcome only when nothing compatible is left. It
// charges nothing, so the identity's one record is still there to be dealt
// once its client can use it; a compatible record held back for another
// reason decides the outcome instead.
func TestExtenderReleaseIncompatibleOnlyWhenNothingElseRemains(t *testing.T) {
	clock := newTestClock()
	keyHexes := testGatedKeyHexes(t, 3)
	order := testReleaseOrder(clock, "identity-new", keyHexes)
	blocked := testBlockedSource{"aa": {order[1]: true}}
	policy := newTestReleasePolicy(t, clock, blocked, func(settings *ExtenderReleaseSettings) {
		settings.MaxClientsPerExtenderPerCountry = 1
	})
	newRequest := func(identity string, countryCode string, compatible func(string) bool) *ExtenderReleaseRequest {
		return &ExtenderReleaseRequest{
			Identity:    []byte(identity),
			Vantage:     "198.51.100.0/24",
			CountryCode: countryCode,
			Compatible:  compatible,
		}
	}
	none := func(string) bool { return false }

	incompatible, err := policy.Release(newRequest("identity-new", "aa", none), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if incompatible.Outcome != ExtenderReleaseOutcomeIncompatible || incompatible.RetryAfter != 0 || len(incompatible.KeyHexes) != 0 {
		t.Fatalf("nothing compatible: %+v", incompatible)
	}
	// the incompatible answer charged nothing: the same new identity, now
	// able to use the records, is dealt its one, the epoch's first eligible
	upgraded, err := policy.Release(newRequest("identity-new", "aa", nil), keyHexes)
	if err != nil || upgraded.Outcome != ExtenderReleaseOutcomeOk || !slices.Equal(upgraded.KeyHexes, order[:1]) {
		t.Fatalf("after an incompatible answer: %+v, %v, want %v", upgraded, err, order[:1])
	}

	// the one compatible record is at its country cap: that, not the
	// incompatible rest, is why the answer is empty
	onlyFirst := func(keyHex string) bool { return keyHex == order[0] }
	capacity, err := policy.Release(newRequest("identity-other", "aa", onlyFirst), keyHexes)
	if err != nil || capacity.Outcome != ExtenderReleaseOutcomeCountryCapacity {
		t.Fatalf("a compatible record at capacity: %+v, %v", capacity, err)
	}
	// blocked records are not incompatible ones: with the only record the
	// client could use blocked, the answer is empty
	onlyBlocked := func(keyHex string) bool { return keyHex == order[1] }
	// order[2] is eligible but incompatible, and order[1] is blocked
	besideBlocked, err := policy.Release(newRequest("identity-third", "aa", onlyBlocked), testWithout(keyHexes, order[0]))
	if err != nil || besideBlocked.Outcome != ExtenderReleaseOutcomeIncompatible {
		t.Fatalf("incompatible beside a blocked record: %+v, %v", besideBlocked, err)
	}
	empty, err := policy.Release(newRequest("identity-fourth", "aa", onlyBlocked), []string{order[1]})
	if err != nil || empty.Outcome != ExtenderReleaseOutcomeEmpty {
		t.Fatalf("only a blocked record: %+v, %v", empty, err)
	}
}

// Over a year of epochs, a client without a capability is never handed a
// record that requires it, while a capable one is dealt from the same
// partition.
func TestExtenderReleaseNeverHandsAnIncompatibleRecord(t *testing.T) {
	clock := newTestClock()
	policy := newTestReleasePolicy(t, clock, nil, nil)
	keyHexes := testGatedKeyHexes(t, 64)
	// half of each identity's partition requires the capability; membership
	// holds while the fleet does, whatever the epoch
	requiring := map[string]bool{}
	for _, identity := range []string{"identity-legacy", "identity-capable"} {
		members, _, _ := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelGated, []byte(identity), keyHexes)
		for i, keyHex := range members {
			if i%2 == 0 {
				requiring[keyHex] = true
			}
		}
	}
	sawRequiring := false
	for range 52 {
		for _, capable := range []bool{false, true} {
			identity := "identity-legacy"
			if capable {
				identity = "identity-capable"
			}
			request := testTrustedReleaseRequest(clock, identity, "aa")
			request.Compatible = func(keyHex string) bool { return capable || !requiring[keyHex] }
			result, err := policy.Release(request, keyHexes)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(testReleaseOutcomes, result.Outcome) {
				t.Fatalf("outcome %q is not one of the fixed set", result.Outcome)
			}
			for _, keyHex := range result.KeyHexes {
				if requiring[keyHex] {
					if !capable {
						t.Fatalf("a legacy client was handed %s, which requires a capability", keyHex)
					}
					sawRequiring = true
				}
			}
		}
		clock.advance(ExtenderReleaseEpochTimeout)
	}
	if !sawRequiring {
		t.Fatal("no capable client was ever dealt a record that requires the capability")
	}
}

// An outcome's retry time describes the requester's own epoch and probation,
// never the fleet: two fleets of different sizes answer the same spent epoch
// with the same retry time.
func TestExtenderReleaseRetryTimeIsIndependentOfTheFleet(t *testing.T) {
	retryAfters := []time.Duration{}
	for _, size := range []int{3, 64} {
		clock := newTestClock()
		clock.advance(50 * time.Hour)
		policy := newTestReleasePolicy(t, clock, nil, nil)
		keyHexes := testGatedKeyHexes(t, size)
		request := testTrustedReleaseRequest(clock, "identity-fleet", "aa")
		first, err := policy.Release(request, keyHexes)
		if err != nil {
			t.Fatal(err)
		}
		spent, err := policy.Release(request, testWithout(keyHexes, first.KeyHexes...))
		if err != nil || spent.Outcome != ExtenderReleaseOutcomeEpochSpent {
			t.Fatalf("fleet of %d: %+v, %v", size, spent, err)
		}
		retryAfters = append(retryAfters, spent.RetryAfter)
	}
	if retryAfters[0] != retryAfters[1] {
		t.Fatalf("retry times %v differ with the fleet's size", retryAfters)
	}
}

// Root cause guarded (§1.3): an outage reported from a country was a block
// there for a day, since the probe needed only to have reached the record in
// the last hour. The probe must reach it now, and the reports made before its
// last failure describe the outage, so they never count, even once the probe
// reaches it again. Reports made after that do.
func TestExtenderBlockedStateTreatsAnOutageAsNoBlock(t *testing.T) {
	clock := newTestClock()
	settings := DefaultExtenderBlockedStateSettings()
	settings.Now = clock.Now
	state := NewExtenderBlockedState(settings)
	keyHex := "ab"
	report := func(reporters ...string) {
		for _, reporter := range reporters {
			state.Report(keyHex, "aa", []byte(reporter))
		}
	}

	state.ProbeSucceeded(keyHex)
	clock.advance(time.Minute)
	report("r1", "r2", "r3")
	if !state.Blocked(keyHex, "aa") {
		t.Fatal("three reporters against a record the probe reaches are not a block")
	}
	clock.advance(time.Minute)
	state.ProbeFailed(keyHex)
	if state.Blocked(keyHex, "aa") || len(state.BlockedKeyHexes("aa")) != 0 {
		t.Fatal("a record whose latest probe failed is blocked: an outage became a block")
	}
	report("r4", "r5", "r6")
	clock.advance(time.Minute)
	state.ProbeSucceeded(keyHex)
	if state.Blocked(keyHex, "aa") {
		t.Fatal("reports made during an outage that has ended made a block")
	}
	// two of the earlier reporters report again after the recovery: still
	// short of three reports since the failure
	clock.advance(time.Minute)
	report("r1", "r4")
	if state.Blocked(keyHex, "aa") {
		t.Fatal("two reporters since the outage made a block")
	}
	report("r7")
	if !state.Blocked(keyHex, "aa") {
		t.Fatal("three reporters since the outage, with the probe reaching the record, are not a block")
	}
	// a failure at the same instant as the success is not a success after it
	clock.advance(time.Minute)
	state.ProbeSucceeded(keyHex)
	state.ProbeFailed(keyHex)
	if state.Blocked(keyHex, "aa") {
		t.Fatal("a probe that failed at its last success's instant counts as reaching the record")
	}
}

// Root cause guarded (§1.3): canary attribution alone must not blacklist
// unrelated records. A blocked canary names its one place and nothing else;
// a release in the blocked country skips the canary alone and deals the rest
// of its partition in the epoch's order, as if the canary were absent.
func TestExtenderReleaseBlockedCanaryBlocksNoOtherRecord(t *testing.T) {
	clock := newTestClock()
	keyHexes := testGatedKeyHexes(t, 16)
	identity := "identity-canary"
	order := testReleaseOrder(clock, identity, keyHexes)
	canary := &ExtenderCanary{KeyHex: order[0], Channel: ExtenderChannelGated}
	blockedKeyHexes := []string{canary.KeyHex}
	_, partition, partitionCount := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelGated, []byte(identity), keyHexes)

	placements := ExtenderAttributeBlockedCanaries(testPartitionSecret, []*ExtenderCanary{canary}, blockedKeyHexes, partitionCount)
	if len(placements) != 1 || placements[0].Channel != ExtenderChannelGated || placements[0].Partition != partition {
		t.Fatalf("placements = %v, want the canary's one gated partition %d", placements, partition)
	}
	// blocked ordinary records attribute nothing
	if placements := ExtenderAttributeBlockedCanaries(testPartitionSecret, []*ExtenderCanary{canary}, order[1:], partitionCount); len(placements) != 0 {
		t.Fatalf("ordinary blocked records attributed %v", placements)
	}

	blocked := testBlockedSource{"aa": {canary.KeyHex: true}}
	policy := newTestReleasePolicy(t, clock, blocked, nil)
	result, err := policy.Release(testTrustedReleaseRequest(clock, identity, "aa"), keyHexes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.KeyHexes, order[1:4]) {
		t.Fatalf("release beside a blocked canary = %v, want the partition's order without the canary %v", result.KeyHexes, order[1:4])
	}
	// elsewhere the canary is an ordinary member of the order
	elsewhere, err := policy.Release(testTrustedReleaseRequest(clock, identity+"-elsewhere", "bb"), keyHexes)
	if err != nil || !slices.Equal(elsewhere.KeyHexes, testReleaseOrder(clock, identity+"-elsewhere", keyHexes)[:3]) {
		t.Fatalf("release in another country: %+v, %v", elsewhere, err)
	}
}

// The requirement is field 18, a repeated string a reader that predates it
// skips, signed with the body: tampering with it breaks the root signature,
// and a record without it requires nothing.
func TestExtenderRecordRequiredCapabilitiesIsSignedFieldEighteen(t *testing.T) {
	body := &protocol.ExtenderRecordBody{RequiredCapabilities: []string{"tcp-camouflage-v1"}}
	descriptor := body.ProtoReflect().Descriptor().Fields().ByName("RequiredCapabilities")
	if descriptor == nil || descriptor.Number() != 18 || !descriptor.IsList() {
		t.Fatal("RequiredCapabilities is not the repeated field 18")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	expected := protowire.AppendString(protowire.AppendTag(nil, 18, protowire.BytesType), "tcp-camouflage-v1")
	if !bytes.Equal(encoded, expected) {
		t.Fatalf("encoded = %x, want %x", encoded, expected)
	}

	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	rootKeys := NewExtenderRootKeySet(rootPublicKey)
	record, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:            bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize),
		DirectoryTier:        ExtenderDirectoryTierGated,
		RequiredCapabilities: []string{"tcp-camouflage-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := rootKeys.VerifyRecord(record)
	if err != nil || !slices.Equal(verified.RequiredCapabilities, []string{"tcp-camouflage-v1"}) {
		t.Fatalf("signed requirement: %v, %v", verified, err)
	}
	// dropping the requirement from the signed bytes is a forgery
	stripped := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.Body, stripped); err != nil {
		t.Fatal(err)
	}
	stripped.RequiredCapabilities = nil
	strippedBytes, err := proto.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	forged := proto.Clone(record).(*protocol.ExtenderRecord)
	forged.Body = strippedBytes
	if _, err := rootKeys.VerifyRecord(forged); err == nil {
		t.Fatal("a record with its requirement removed still verifies")
	}
	// a record that predates the field requires nothing
	old, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{PublicKey: bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize)})
	if err != nil {
		t.Fatal(err)
	}
	if verified, err := rootKeys.VerifyRecord(old); err != nil || len(verified.GetRequiredCapabilities()) != 0 {
		t.Fatalf("a record without the field: %v, %v", verified, err)
	}
}
