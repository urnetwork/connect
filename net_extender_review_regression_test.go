// Pins the six client-side review failures with fixed clocks, deterministic
// identities and explicit admission barriers.
package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Several policy instances share one real in-memory admission transaction.
type reviewBarrierLedger struct {
	*ExtenderReleaseMemoryLedger
	arrived  sync.WaitGroup
	proceed  chan struct{}
	unlocked atomic.Bool
}

// Every replica reaches admission before the shared ledger admits any of them.
func (self *reviewBarrierLedger) Transact(identity []byte, vantage string, country string, keyHexes []string, apply func(ExtenderReleaseLedgerTx)) {
	self.arrived.Done()
	<-self.proceed
	self.ExtenderReleaseMemoryLedger.Transact(identity, vantage, country, keyHexes, func(tx ExtenderReleaseLedgerTx) {
		apply(&reviewLockedReleaseTx{ExtenderReleaseLedgerTx: tx, ledger: self})
	})
}

// Checks the exact read/decision gap, without a negative timeout or sleep.
type reviewLockedReleaseTx struct {
	ExtenderReleaseLedgerTx
	ledger *reviewBarrierLedger
}

// The transaction must still own the shared lock after returning either count.
func (self *reviewLockedReleaseTx) checkLocked() {
	if self.ledger.stateLock.TryLock() {
		self.ledger.unlocked.Store(true)
		self.ledger.stateLock.Unlock()
	}
}

// Forces observation of lock ownership between reading and reserving capacity.
func (self *reviewLockedReleaseTx) ClientCount(key string, country string, identity []byte, since time.Time) int {
	count := self.ExtenderReleaseLedgerTx.ClientCount(key, country, identity, since)
	self.checkLocked()
	return count
}

// Request rate admission needs the same atomic read/decision boundary.
func (self *reviewLockedReleaseTx) RequestCounts(identity []byte, vantage string, since time.Time) (int, int) {
	identityCount, vantageCount := self.ExtenderReleaseLedgerTx.RequestCounts(identity, vantage, since)
	self.checkLocked()
	return identityCount, vantageCount
}

// Eleven pending callers cannot each read the same ten-slot country budget.
func TestReviewReleaseClientQuotaIsAtomic(t *testing.T) {
	settings := DefaultExtenderReleaseSettings()
	settings.Now = func() time.Time { return time.Unix(1700000000, 0) }
	ledger := &reviewBarrierLedger{
		ExtenderReleaseMemoryLedger: NewExtenderReleaseMemoryLedger(settings.RequestWindow, settings.ClientWindow),
		proceed:                     make(chan struct{}),
	}
	const requestCount = 11
	ledger.arrived.Add(requestCount)
	policy := NewExtenderReleasePolicy([]byte("review-secret"), ledger, nil, settings)
	results := make(chan *ExtenderReleaseResult, requestCount)
	errs := make(chan error, requestCount)
	for i := range requestCount {
		go func() {
			result, err := policy.Release(&ExtenderReleaseRequest{
				Identity: []byte(fmt.Sprintf("review-identity-%d", i)),
				Vantage:  "192.0.2.0/24", CountryCode: "us",
			}, []string{"review-extender-key"})
			results <- result
			errs <- err
		}()
	}
	ledger.arrived.Wait()
	close(ledger.proceed)
	admitted := 0
	for range requestCount {
		result := <-results
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		admitted += len(result.KeyHexes)
	}
	if admitted > settings.MaxClientsPerExtenderPerCountry {
		t.Fatalf("released one extender to %d identities in one country, configured maximum %d", admitted, settings.MaxClientsPerExtenderPerCountry)
	}
	if ledger.unlocked.Load() {
		t.Fatal("admission released its lock between the count and reservation")
	}
}

// Switching eligible address families cannot disclose another record this epoch.
func TestReviewReleaseEpochCapSurvivesFamilyChanges(t *testing.T) {
	settings := DefaultExtenderReleaseSettings()
	settings.Now = func() time.Time { return time.Unix(1700000000, 0) }
	policy := NewExtenderReleasePolicy([]byte("review-secret"), NewExtenderReleaseMemoryLedger(settings.RequestWindow, settings.ClientWindow), nil, settings)
	request := &ExtenderReleaseRequest{Identity: []byte("review-identity"), Vantage: "192.0.2.0/24", CountryCode: "us"}
	keys := []string{"review-v4-key", "review-v6-key"}
	seen := map[string]bool{}
	for _, requestedKey := range keys {
		request.Eligible = func(key string) bool { return key == requestedKey }
		result, err := policy.Release(request, keys)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range result.KeyHexes {
			seen[key] = true
		}
	}
	if len(seen) > settings.NewIdentityCount {
		t.Fatalf("same probation identity got %d distinct records in the same epoch by switching family; cap %d", len(seen), settings.NewIdentityCount)
	}
}

// The lifetime handshake ratio must not bias a fresh network's race.
func TestReviewNetworkChangeDoesNotReuseGlobalHandshakeWeight(t *testing.T) {
	settings := DefaultClientStrategySettings()
	winner := &clientDialer{description: "normal", minimumWeight: 0.25, settings: settings}
	other := &clientDialer{description: "fragment", minimumWeight: 0.25, settings: settings}
	strategy := &ClientStrategy{settings: settings, scores: newNetworkStrategyScores(), dialers: map[*clientDialer]bool{winner: true, other: true}}
	strategy.SetNetworkId("review-network-A")
	winner.Update(context.Background(), nil)
	strategy.SetNetworkId("review-network-B")
	weights := strategy.dialerWeights(false)
	if weights[winner] != weights[other] {
		t.Fatalf("old-network handshake biases new network with no delivery evidence: normal=%v fragment=%v", weights[winner], weights[other])
	}
}

// A completion carries the network captured when its attempt began.
func TestReviewLateDeliveryDoesNotCreditNewNetwork(t *testing.T) {
	settings := DefaultClientStrategySettings()
	dialer := &clientDialer{description: "normal", minimumWeight: 0.25, settings: settings}
	strategy := &ClientStrategy{settings: settings, scores: newNetworkStrategyScores(), dialers: map[*clientDialer]bool{dialer: true}}
	strategy.SetNetworkId("review-network-A")
	info := strategy.dialerInfo(dialer)
	strategy.SetNetworkId("review-network-B")
	strategy.RecordDeliveryOutcome(info, deliveryVerifiedByteCount, false)
	weightB := strategy.scores.weight(deriveNetworkId("review-network-B"), dialer.dialerKey())
	if weightB != 1 {
		t.Fatalf("A delivery credited B: B multiplier=%v want neutral1", weightB)
	}
}

// Evicting a key's last country cannot detach the replacement entry's map.
func TestReviewBlockedStateSameKeyEvictionRetainsNewCountry(t *testing.T) {
	settings := DefaultExtenderBlockedStateSettings()
	settings.MaxEntryCount = 1
	settings.ReportThreshold = 1
	settings.Now = func() time.Time { return time.Unix(1700000000, 0) }
	state := NewExtenderBlockedState(settings)
	state.ProbeSucceeded("review-key")
	state.Report("review-key", "us", []byte("review-reporter"))
	state.Report("review-key", "ca", []byte("review-reporter"))
	if !state.Blocked("review-key", "ca") {
		t.Fatal("new country's report disappeared when eviction removed the last old country of the same key")
	}
	if state.entryCount != 1 || len(state.entries) != 1 || len(state.entries["review-key"]) != 1 || state.Blocked("review-key", "us") {
		t.Fatal("country replacement lost bounded entry accounting")
	}
}

// The server's own key remains in the pool that determines partition count.
func TestReviewFeedSampleAndStreamShareOnePartition(t *testing.T) {
	clock := newTestClock()
	directory, root := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) { settings.PartitionSecret = testPartitionSecret })
	keys := []string{}
	var own ed25519.PublicKey
	for i := range 8 {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = byte(i + 1)
		key := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		if i == 0 {
			own = key
		}
		record := signTestTierRecord(t, root, key, clock.Now(), ExtenderDirectoryTierOpen, fmt.Sprintf("198.51.100.%d", i+1))
		if _, err := directory.ApplyRecord(record, ExtenderSourceGossip); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, hex.EncodeToString(key))
	}
	vantage := ExtenderVantageKey(netip.MustParseAddr("192.0.2.1"))
	members, _, _ := ExtenderPartitionMembers(testPartitionSecret, ExtenderChannelFeed, vantage, keys)
	sampled := sampleKeyHexes(t, directory, directory.SampleRecords(8, own, vantage))
	if len(sampled) == 0 || sampled[0] != hex.EncodeToString(own) {
		t.Fatal("own record lost first position")
	}
	for _, member := range members {
		if !slices.Contains(sampled, member) {
			t.Fatal("sample omitted a member of its complete partition")
		}
	}
	for _, key := range sampled {
		if key != hex.EncodeToString(own) && !slices.Contains(members, key) {
			t.Fatalf("sample released %s outside the stream's partition%v", key, members)
		}
	}
}
