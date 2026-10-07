package connect

// The request-rate and disclosure budgets share the country admission
// transaction. Barriers and a fixed clock make every boundary observable.

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// Eleven pending admissions compete for ten identity request slots.
func TestExtenderReleaseIdentityRequestsAreAtomic(t *testing.T) {
	testExtenderReleaseAtomicRequests(t, true)
}

// Distinct identities still share the same vantage request budget.
func TestExtenderReleaseVantageRequestsAreAtomic(t *testing.T) {
	testExtenderReleaseAtomicRequests(t, false)
}

// Synchronizes requests at transaction entry and checks lock ownership after
// their count reads, which is the root-cause gap of the former split API.
func testExtenderReleaseAtomicRequests(t *testing.T, sameIdentity bool) {
	t.Helper()
	settings := DefaultExtenderReleaseSettings()
	settings.Now = func() time.Time { return time.Unix(1700000000, 0) }
	settings.IdentityRequestLimit = 0
	settings.VantageRequestLimit = 10
	wantErr := ErrExtenderReleaseVantageLimited
	if sameIdentity {
		settings.IdentityRequestLimit = 10
		settings.VantageRequestLimit = 0
		wantErr = ErrExtenderReleaseIdentityLimited
	}
	ledger := &reviewBarrierLedger{ExtenderReleaseMemoryLedger: NewExtenderReleaseMemoryLedger(settings.RequestWindow, settings.ClientWindow), proceed: make(chan struct{})}
	const requestCount = 11
	ledger.arrived.Add(requestCount)
	results := make(chan error, requestCount)
	for i := range requestCount {
		go func() {
			identity := []byte(fmt.Sprintf("test-identity-%d", i))
			if sameIdentity {
				identity = []byte("test-shared-identity")
			}
			policy := NewExtenderReleasePolicy(testPartitionSecret, ledger, nil, settings)
			_, err := policy.Release(&ExtenderReleaseRequest{Identity: identity, Vantage: "192.0.2.0/24"}, nil)
			results <- err
		}()
	}
	ledger.arrived.Wait()
	close(ledger.proceed)
	admitted := 0
	for range requestCount {
		if err := <-results; err == nil {
			admitted++
		} else if !errors.Is(err, wantErr) {
			t.Fatal(err)
		}
	}
	if admitted != 10 || ledger.unlocked.Load() {
		t.Fatalf("admitted %d requests; read/decision gap unlocked: %t", admitted, ledger.unlocked.Load())
	}
	_, vantageCount := ledger.RequestCounts(nil, "192.0.2.0/24", settings.Now().Add(-settings.RequestWindow))
	if vantageCount != requestCount {
		t.Fatalf("recorded %d requests, including refusals; want %d", vantageCount, requestCount)
	}
}

// Blocking or removing an issued record cannot refund a disclosure within
// the epoch. Unblocking returns the original record; a new epoch allows another.
func TestExtenderReleaseEpochBudgetSurvivesAvailabilityChanges(t *testing.T) {
	clock := newTestClock()
	blocked := testBlockedSource{"us": {}}
	policy := newTestReleasePolicy(t, clock, blocked, nil)
	request := &ExtenderReleaseRequest{Identity: []byte("test-identity"), CountryCode: "us", Vantage: "192.0.2.0/24"}
	first, err := policy.Release(request, []string{"first"})
	if err != nil || !slices.Equal(first.KeyHexes, []string{"first"}) {
		t.Fatalf("initial release: %v, %v", first, err)
	}
	blocked["us"]["first"] = true
	for _, keys := range [][]string{{"first", "second"}, {"second"}} {
		result, err := policy.Release(request, keys)
		if err != nil || len(result.KeyHexes) != 0 {
			t.Fatalf("availability refunded disclosure: %v, %v", result, err)
		}
	}
	delete(blocked["us"], "first")
	again, err := policy.Release(request, []string{"first", "second"})
	if err != nil || !slices.Equal(again.KeyHexes, first.KeyHexes) {
		t.Fatalf("original disclosure lost: %v, %v", again, err)
	}
	clock.advance(policy.settings.EpochTimeout)
	next, err := policy.Release(request, []string{"second"})
	if err != nil || !slices.Equal(next.KeyHexes, []string{"second"}) {
		t.Fatalf("new epoch did not renew budget: %v, %v", next, err)
	}
}

// A durable blocked source must receive one partition in one call, instead of
// one database lookup per fleet key before each admission.
func TestExtenderReleaseBatchesPartitionEligibility(t *testing.T) {
	clock := newTestClock()
	blocked := &testReleaseBatchBlockedSource{}
	policy := newTestReleasePolicy(t, clock, blocked, nil)
	keys := make([]string, 256)
	for i := range keys {
		keys[i] = fmt.Sprintf("synthetic-key-%03d", i)
	}
	identity := []byte("synthetic-identity")
	members, _, _ := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelGated, identity, keys)
	result, err := policy.Release(&ExtenderReleaseRequest{Identity: identity}, keys)
	if err != nil || len(result.KeyHexes) != 1 {
		t.Fatalf("release: %v, %v", result, err)
	}
	slices.Sort(members)
	if blocked.calls != 1 || !slices.Equal(blocked.keys, members) || blocked.singleCalls != 0 {
		t.Fatalf("blocked reads: batches=%d singles=%d candidates=%d, want one batch of %d", blocked.calls, blocked.singleCalls, len(blocked.keys), len(members))
	}
}

// Records the work offered to the durable batch seam.
type testReleaseBatchBlockedSource struct {
	calls       int
	singleCalls int
	keys        []string
}

// The scalar fallback must not run when batch lookup is available.
func (self *testReleaseBatchBlockedSource) Blocked(_ string, _ string) bool {
	self.singleCalls++
	return false
}

// Captures the exact candidate keys without retaining the caller's slice.
func (self *testReleaseBatchBlockedSource) BlockedKeys(keys []string, _ string) map[string]bool {
	self.calls++
	self.keys = slices.Clone(keys)
	return nil
}

// A full active-epoch table fails closed but still stamps the refused request.
func TestExtenderReleaseFullEpochLedgerRefusesWithoutForgetting(t *testing.T) {
	clock := newTestClock()
	settings := DefaultExtenderReleaseSettings()
	settings.Now = clock.Now
	ledger := NewExtenderReleaseMemoryLedger(settings.RequestWindow, settings.ClientWindow)
	ledger.SetBounds(1, 4, 4)
	policy := NewExtenderReleasePolicy(testPartitionSecret, ledger, nil, settings)
	first := &ExtenderReleaseRequest{Identity: []byte("synthetic-first"), Vantage: "192.0.2.0/24"}
	if _, err := policy.Release(first, []string{"first"}); err != nil {
		t.Fatal(err)
	}
	other := &ExtenderReleaseRequest{Identity: []byte("synthetic-other"), Vantage: "198.51.100.0/24"}
	if _, err := policy.Release(other, []string{"second"}); !errors.Is(err, ErrExtenderReleaseLedgerFull) {
		t.Fatalf("full ledger: %v", err)
	}
	if identityCount, _ := ledger.RequestCounts(other.Identity, other.Vantage, clock.Now().Add(-settings.RequestWindow)); identityCount != 1 {
		t.Fatal("refused request was not recorded")
	}
	if result, err := policy.Release(first, []string{"second"}); err != nil || len(result.KeyHexes) != 0 {
		t.Fatalf("active disclosure was forgotten: %v, %v", result, err)
	}
}
