package connect

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// The gated preference at the strategy (EXTENDER-UPDATE2.md §1.1, §1.4): which
// extenders the client strategy actually dials, observed at its eval, with the
// policy's clock the fake one. Nothing here dials a network: the eval records
// the attempt and answers for it.

// One recorded attempt: the address, and the policy clock when it was made.
type testGatedDialAttempt struct {
	ip      netip.Addr
	elapsed time.Duration
}

// Records the strategy's attempts and answers each from `answer`.
type testGatedDialObserver struct {
	clock     *testClock
	startTime time.Time
	answer    func(ctx context.Context, dialer *clientDialer) *evalResult

	stateLock sync.Mutex
	attempts  []testGatedDialAttempt
}

func newTestGatedDialObserver(
	clock *testClock,
	answer func(ctx context.Context, dialer *clientDialer) *evalResult,
) *testGatedDialObserver {
	return &testGatedDialObserver{
		clock:     clock,
		startTime: clock.Now(),
		answer:    answer,
	}
}

func (self *testGatedDialObserver) eval(ctx context.Context, dialer *clientDialer) *evalResult {
	if dialer.extenderConfig == nil {
		return &evalResult{err: errors.New("only extenders are dialed in this test")}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.attempts = append(self.attempts, testGatedDialAttempt{
			ip:      dialer.extenderConfig.Ip,
			elapsed: self.clock.Now().Sub(self.startTime),
		})
	}()
	return self.answer(ctx, dialer)
}

func (self *testGatedDialObserver) observed() []testGatedDialAttempt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.attempts)
}

// A strategy with extender dialers alone, a fast reconnect pace and the
// gated preference on.
func newTestGatedStrategy(t *testing.T, clock *testClock) (*ClientStrategy, *ExtenderDirectory, func(ip string, gated bool, revoke bool)) {
	t.Helper()
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, func(settings *ClientStrategySettings) {
		settings.EnableNormal, settings.EnableResilient = false, false
		settings.ReconnectTimeout = 10 * time.Millisecond
	})
	directory.SetGatedPreference(&ExtenderGatedPreferenceSettings{
		Enabled:        true,
		AttemptTimeout: testGatedPreferenceAttemptTimeout,
	})
	apply := func(ip string, gated bool, revoke bool) {
		key := newTestExtenderKey(t)
		if gated {
			applyTestGatedRelease(t, directory, rootPrivateKey, clock, key, ip)
		} else {
			record := signTestTierRecord(t, rootPrivateKey, key, clock.Now(), ExtenderDirectoryTierOpen, ip)
			if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
				t.Fatal(err)
			}
		}
		if revoke {
			if _, err := directory.ApplyRevocation(signTestRevocation(t, rootPrivateKey, key, clock.Now().Add(time.Second))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return clientStrategy, directory, apply
}

// Runs one request through the strategy and returns the address it selected.
func testGatedRequest(t *testing.T, clientStrategy *ClientStrategy, observer *testGatedDialObserver) netip.Addr {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := clientStrategy.parallelEval(ctx, false, observer.eval)
	if result == nil || result.err != nil || result.dialer == nil || result.dialer.extenderConfig == nil {
		t.Fatalf("the request selected no extender: %v", observer.observed())
	}
	return result.dialer.extenderConfig.Ip
}

// A usable gated record is what the strategy dials; the open record is never
// tried (§1.1 rule 1).
func TestClientStrategyGatedPreferenceDialsTheGatedRecord(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, apply := newTestGatedStrategy(t, clock)
	apply("198.51.100.1", false, false)
	apply("192.0.2.10", true, false)
	gatedIp := netip.MustParseAddr("192.0.2.10")

	observer := newTestGatedDialObserver(clock, func(ctx context.Context, dialer *clientDialer) *evalResult {
		dialer.Update(ctx, nil)
		return &evalResult{}
	})
	if selected := testGatedRequest(t, clientStrategy, observer); selected != gatedIp {
		t.Fatalf("the request went through %s", selected)
	}
	for _, attempt := range observer.observed() {
		if attempt.ip != gatedIp {
			t.Fatalf("the strategy dialed %s while a gated record was usable", attempt.ip)
		}
	}
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeGated {
		t.Fatalf("mode %s after the gated record connected", mode)
	}
}

// A gated record that never answers holds the open record back for the
// attempt's budget and no longer: the first open dial is made once the budget
// has passed, and every dial before it was gated (§1.1 rule 3).
func TestClientStrategyGatedPreferenceFallsBackWithinTheBudget(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, apply := newTestGatedStrategy(t, clock)
	apply("192.0.2.10", true, false)
	apply("198.51.100.1", false, false)
	gatedIp := netip.MustParseAddr("192.0.2.10")
	openIp := netip.MustParseAddr("198.51.100.1")

	observer := newTestGatedDialObserver(clock, func(ctx context.Context, dialer *clientDialer) *evalResult {
		if dialer.extenderConfig.Ip == gatedIp {
			// a dial that hangs: the time passes and nothing is reported
			// against the address
			clock.advance(4 * time.Second)
			return &evalResult{err: errors.New("no answer")}
		}
		dialer.Update(ctx, nil)
		return &evalResult{}
	})
	if selected := testGatedRequest(t, clientStrategy, observer); selected != openIp {
		t.Fatalf("the request went through %s, expected the open fallback", selected)
	}
	attempts := observer.observed()
	firstOpen := slices.IndexFunc(attempts, func(attempt testGatedDialAttempt) bool {
		return attempt.ip == openIp
	})
	if firstOpen <= 0 {
		t.Fatalf("the open record was dialed first or never: %v", attempts)
	}
	for _, attempt := range attempts[:firstOpen] {
		if attempt.ip != gatedIp {
			t.Fatalf("%s was dialed before the fallback", attempt.ip)
		}
	}
	if elapsed := attempts[firstOpen].elapsed; elapsed < testGatedPreferenceAttemptTimeout {
		t.Fatalf("the open record was dialed %s into a %s budget", elapsed, testGatedPreferenceAttemptTimeout)
	}
	if reason := directory.GatedPreference().FallbackReason; reason != ExtenderGatedFallbackGatedTimeout {
		t.Fatalf("reason %q, expected the budget", reason)
	}
}

// With no usable gated record -- a cold client, or a release that came back
// empty, refused or failed -- the first dial is the verified open record: the
// fallback costs no budget at all, and an empty release route does not hold
// it off (§1.1 rules 3 and 4).
func TestClientStrategyGatedPreferenceFallsBackAtOnceWithoutAGatedRecord(t *testing.T) {
	for _, outcome := range []string{"", "empty", "rate_limited", "unavailable", "failed"} {
		clock := newTestClock()
		clientStrategy, directory, apply := newTestGatedStrategy(t, clock)
		apply("198.51.100.1", false, false)
		if outcome != "" {
			directory.SetGatedReleaseOutcome(outcome)
		}
		observer := newTestGatedDialObserver(clock, func(ctx context.Context, dialer *clientDialer) *evalResult {
			dialer.Update(ctx, nil)
			return &evalResult{}
		})
		if selected := testGatedRequest(t, clientStrategy, observer); selected != netip.MustParseAddr("198.51.100.1") {
			t.Fatalf("outcome %q: the request went through %s", outcome, selected)
		}
		if attempts := observer.observed(); len(attempts) == 0 || attempts[0].elapsed != 0 {
			t.Fatalf("outcome %q: the open record waited: %v", outcome, attempts)
		}
	}
}

// A revoked gated record is never dialed, however the policy stands
// (§1.4).
func TestClientStrategyGatedPreferenceNeverDialsARevokedRecord(t *testing.T) {
	clock := newTestClock()
	clientStrategy, _, apply := newTestGatedStrategy(t, clock)
	apply("192.0.2.10", true, true)
	apply("198.51.100.1", false, false)

	observer := newTestGatedDialObserver(clock, func(ctx context.Context, dialer *clientDialer) *evalResult {
		dialer.Update(ctx, nil)
		return &evalResult{}
	})
	testGatedRequest(t, clientStrategy, observer)
	for _, attempt := range observer.observed() {
		if attempt.ip == netip.MustParseAddr("192.0.2.10") {
			t.Fatal("the strategy dialed a revoked gated record")
		}
	}
}

// Once a gated record connects again the open dialers drain: the collapse
// drops them, successful as they were, while the gated dialers stay (§1.1
// rule 5). Before that -- while gated is only being tried again -- the working
// open dialers keep serving beside the new gated ones. Dropping a dialer
// retires its pool for new requests; it closes no live connection made
// through it.
func TestClientStrategyGatedPreferenceDrainsTheOpenDialersOnRecovery(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, apply := newTestGatedStrategy(t, clock)
	apply("192.0.2.10", true, false)
	apply("198.51.100.1", false, false)
	gatedIp := netip.MustParseAddr("192.0.2.10")
	openIp := netip.MustParseAddr("198.51.100.1")
	ctx := context.Background()

	// the gated record fails; the fallback expands the open one, which works
	directory.ConnectCandidates([]int{4}, 1, false, nil)
	directory.RecordFailure(gatedIp, ExtenderConnectModeTcpTls)
	clientStrategy.expandExtenderDialers()
	for _, dialer := range testExtenderDialers(clientStrategy) {
		if dialer.extenderConfig.Ip != openIp {
			t.Fatalf("the fallback expanded %s", dialer.extenderConfig.Ip)
		}
		dialer.Update(ctx, nil)
	}
	clientStrategy.collapseExtenderDialers()
	if count := len(testExtenderDialers(clientStrategy)); count != 3 {
		t.Fatalf("%d dialers in the fallback, expected the open record's three", count)
	}

	// the hold lapses: gated is tried again, and the working open dialers
	// keep serving beside the gated ones the attempt expanded
	clock.advance(11 * time.Minute)
	clientStrategy.expandExtenderDialers()
	clientStrategy.collapseExtenderDialers()
	counts := map[netip.Addr]int{}
	for _, dialer := range testExtenderDialers(clientStrategy) {
		counts[dialer.extenderConfig.Ip] += 1
	}
	if counts[openIp] != 3 || counts[gatedIp] != 3 || len(counts) != 2 {
		t.Fatalf("the attempt left the dialers %v, expected the open record's three beside the gated record's three", counts)
	}
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeAttempt {
		t.Fatalf("mode %s while gated was tried again", mode)
	}

	// a gated dialer connects: the open dialers drain, the gated ones stay
	for _, dialer := range testExtenderDialers(clientStrategy) {
		if dialer.extenderConfig.Ip == gatedIp {
			dialer.Update(ctx, nil)
			break
		}
	}
	clientStrategy.collapseExtenderDialers()
	for _, dialer := range testExtenderDialers(clientStrategy) {
		if dialer.extenderConfig.Ip != gatedIp {
			t.Fatalf("%s kept a dialer once gated was preferred again", dialer.extenderConfig.Ip)
		}
	}
	if count := len(testExtenderDialers(clientStrategy)); count != 3 {
		t.Fatalf("%d dialers once gated was preferred again, expected the gated record's three", count)
	}
}

// A gated record that is tried again and never answers does not strand the
// client: the requests made during the attempt are served by the open dialer
// that carried the client, which the collapse keeps, and the attempt ends in
// the fallback at its budget (§1.1 rules 3 and 5). Were the open dialers
// drained when the attempt began, these requests would have only the
// black-holed gated dialers to wait on.
func TestClientStrategyGatedPreferenceAttemptKeepsTheWorkingOpenDialer(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, apply := newTestGatedStrategy(t, clock)
	apply("192.0.2.10", true, false)
	apply("198.51.100.1", false, false)
	gatedIp := netip.MustParseAddr("192.0.2.10")
	openIp := netip.MustParseAddr("198.51.100.1")
	ctx := context.Background()

	// the gated record fails and the open fallback carries the client
	directory.ConnectCandidates([]int{4}, 1, false, nil)
	directory.RecordFailure(gatedIp, ExtenderConnectModeTcpTls)
	clientStrategy.expandExtenderDialers()
	for _, dialer := range testExtenderDialers(clientStrategy) {
		dialer.Update(ctx, nil)
	}

	// the hold lapses and an expansion tries gated again
	clock.advance(11 * time.Minute)
	clientStrategy.expandExtenderDialers()
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeAttempt {
		t.Fatalf("mode %s, expected gated to be tried again", mode)
	}

	// the gated record black-holes: its dials end only with their request
	observer := newTestGatedDialObserver(clock, func(ctx context.Context, dialer *clientDialer) *evalResult {
		if dialer.extenderConfig.Ip == gatedIp {
			<-ctx.Done()
			return &evalResult{err: ctx.Err()}
		}
		dialer.Update(ctx, nil)
		return &evalResult{}
	})
	for i := range 3 {
		if selected := testGatedRequest(t, clientStrategy, observer); selected != openIp {
			t.Fatalf("request %d during the attempt went through %s", i, selected)
		}
	}
	if mode := directory.GatedPreference().Mode; mode != ExtenderGatedModeAttempt {
		t.Fatalf("mode %s after the requests, expected the attempt still", mode)
	}

	// the attempt's budget passes with no gated record connected: the next
	// expansion falls back
	clock.advance(testGatedPreferenceAttemptTimeout)
	clientStrategy.expandExtenderDialers()
	if status := directory.GatedPreference(); status.Mode != ExtenderGatedModeFallback || status.FallbackReason != ExtenderGatedFallbackGatedTimeout {
		t.Fatalf("mode %s reason %q after the budget", status.Mode, status.FallbackReason)
	}
}
