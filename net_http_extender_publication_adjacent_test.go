package connect

// Overlapping expansions must revalidate strategy-owned country and route
// membership at publication, after external candidate construction completes.

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

// Pauses only the first discovery snapshot, allowing a newer expansion to
// publish before it resumes. Cleanup releases and joins the suspended worker.
func blockFirstExtenderPublication(t *testing.T, strategy *ClientStrategy) func() []*clientDialer {
	t.Helper()
	entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredFirst atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	restore := swapControlFamilyProbe(func(family int) bool {
		if family == 4 && enteredFirst.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return family == 4
	})
	t.Cleanup(restore)
	t.Cleanup(func() { unblock(); waitCustomExtenderPublication(t, stopped) })
	result := make(chan []*clientDialer, 1)
	go func() {
		defer close(stopped)
		result <- strategy.expandExtenderDialers()
	}()
	waitCustomExtenderPublication(t, entered)
	return func() []*clientDialer {
		unblock()
		waitCustomExtenderPublication(t, stopped)
		return <-result
	}
}

// Both snapshots see an empty strategy, but only the first publication may
// own and return the one available address/profile pair.
func TestClientStrategyExtenderPublicationKeepsConcurrentProfileUnique(t *testing.T) {
	strategy, ip := newCustomExtenderPublicationStrategy(t, false)
	finish := blockFirstExtenderPublication(t, strategy)
	current := strategy.expandExtenderDialers()
	assertCustomExtenderPublicationCurrent(t, strategy, current, ip, "")
	if len(current) != 1 {
		t.Fatalf("first publication returned %d dialers, want one", len(current))
	}
	if stale := finish(); len(stale) != 0 {
		t.Errorf("overlapping expansion returned %d duplicate profiles", len(stale))
	}
	if dialers := testExtenderDialers(strategy); len(dialers) != 1 {
		t.Errorf("overlapping expansions published %d dialers for one address/profile", len(dialers))
	}
	if weights := strategy.dialerWeights(false); len(weights) != 1 {
		t.Errorf("overlapping expansions left %d selectable owners for one profile", len(weights))
	}
}

// New membership alone does not invalidate an expansion. If discovery now
// offers another address, the older snapshot can still publish that new pair.
func TestClientStrategyExtenderPublicationPreservesDisjointCandidates(t *testing.T) {
	strategy, firstIp := newCustomExtenderPublicationStrategy(t, false)
	finish := blockFirstExtenderPublication(t, strategy)
	first := strategy.expandExtenderDialers()
	if len(first) != 1 || first[0].extenderConfig.Ip != firstIp {
		t.Fatal("newer expansion did not publish the first candidate")
	}
	directory := strategy.ExtenderDirectory()
	directory.RecordFailure(firstIp, ExtenderConnectModeQuic)
	nextIp := netip.MustParseAddr("192.0.2.42")
	directory.AddManual(nextIp)
	next := finish()
	if len(next) == 0 {
		t.Fatal("unrelated membership starved the available disjoint candidate")
	}
	for _, dialer := range next {
		if dialer.extenderConfig.Ip != nextIp {
			t.Errorf("disjoint expansion returned %s, want %s", dialer.extenderConfig.Ip, nextIp)
		}
	}
	if dialers := testExtenderDialers(strategy); len(dialers) != len(first)+len(next) {
		t.Errorf("disjoint expansion did not retain both candidate owners")
	}
}

// A country-A snapshot must not repopulate old outer names after a newer
// expansion has switched the strategy to country B. Otherwise later B passes
// see the current country marker and never sweep the resurrected A profiles.
func TestClientStrategyExtenderPublicationRejectsSupersededCountry(t *testing.T) {
	strategy, ip := newCustomExtenderPublicationStrategy(t, false)
	installTestCountrySpoofLists(t, map[string][]string{
		"aa": {"one.aa.example"},
		"bb": {"one.bb.example"},
	})
	directory := strategy.ExtenderDirectory()
	directory.SetCountryHint("aa")
	finish := blockFirstExtenderPublication(t, strategy)
	directory.SetCountryHint("bb")
	current := strategy.expandExtenderDialers()
	assertCustomExtenderPublicationCurrent(t, strategy, current, ip, "")
	if len(current) != 1 || current[0].extenderConfig.Profile.ServerName != "one.bb.example" {
		t.Fatal("newer expansion did not publish the current country profile")
	}
	if stale := finish(); len(stale) != 0 {
		t.Errorf("superseded country expansion returned %d selectable profiles", len(stale))
	}
	strategy.expandExtenderDialers()
	if dialers := testExtenderDialers(strategy); len(dialers) != 1 {
		t.Errorf("country switch left %d published profiles, want only the newer country", len(dialers))
	}
	for dialer := range strategy.dialerWeights(false) {
		if dialer.extenderConfig.Profile.ServerName != "one.bb.example" {
			t.Errorf("superseded country profile %q remained selectable after another expansion", dialer.extenderConfig.Profile.ServerName)
		}
	}
}
