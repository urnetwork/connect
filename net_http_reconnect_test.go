package connect

import (
	"context"
	"testing"
	"time"
)

// the serialized path: every caller advances one shared timestamp, so a burst
// of N cold connects staircases at least (N-1) x MinNextConnectDelay deep.
// This is the shape the reconnect fast path exists to bypass -- pin it so the
// contrast below stays meaningful.
func TestNextConnectTimeSerializesCallers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	settings.MinNextConnectDelay = 100 * time.Millisecond
	settings.MaxNextConnectDelay = 200 * time.Millisecond
	strategy := NewClientStrategy(ctx, settings)

	start := time.Now()
	var last time.Time
	for i := range 8 {
		next, _ := strategy.NextConnectTime()
		if next.Before(last) {
			t.Fatalf("call %d went backwards: %s < %s", i, next, last)
		}
		last = next
	}
	// 8 callers: the first clamps to now, each later one advances >= Min
	if staircase := last.Sub(start); staircase < 7*settings.MinNextConnectDelay {
		t.Errorf("serialized staircase too shallow: %s < %s", staircase, 7*settings.MinNextConnectDelay)
	}
}

// the reconnect fast path: independent small jitter, capped concurrency, no
// effect on the shared serialized timestamp, and slots that recycle on
// release. All arithmetic on returned times -- no sleeps -- so the timing
// shape assertions hold on a loaded runner.
func TestNextConnectTimeReconnectFastPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	// the serialized step is made larger than the fast-path jitter so the two
	// shapes are provably disjoint
	settings.MinNextConnectDelay = 300 * time.Millisecond
	settings.MaxNextConnectDelay = 400 * time.Millisecond
	strategy := NewClientStrategy(ctx, settings)

	// prime the shared staircase well past the fast-path jitter range. start
	// is captured BEFORE priming so serialLast >= start + 2 x Min holds by
	// construction (the first call clamps to its own now >= start).
	start := time.Now()
	var serialLast time.Time
	for range 3 {
		serialLast, _ = strategy.NextConnectTime()
	}
	if serialLast.Before(start.Add(2 * settings.MinNextConnectDelay)) {
		t.Fatalf("staircase priming failed: %s", serialLast.Sub(start))
	}

	// up to the cap, reconnects are fast and independent of the staircase
	releases := []func(){}
	for i := range settings.ReconnectFastPathLimit {
		connectTime, release := strategy.NextReconnectTime()
		releases = append(releases, release)
		if !connectTime.Before(serialLast) {
			t.Errorf("fast path call %d landed on the staircase: %s >= %s", i, connectTime, serialLast)
		}
		if jitter := connectTime.Sub(start); settings.ReconnectFastPathMaxDelay+100*time.Millisecond <= jitter {
			t.Errorf("fast path call %d jitter too large: %s", i, jitter)
		}
	}

	// over the cap: the caller falls back to the serialized staircase, which
	// the fast-path callers must NOT have advanced -- one serialized step from
	// serialLast, not five
	overCapTime, overCapRelease := strategy.NextReconnectTime()
	if overCapTime.Before(serialLast) {
		t.Errorf("over-cap call bypassed the staircase: %s < %s", overCapTime, serialLast)
	}
	if step := overCapTime.Sub(serialLast); settings.MaxNextConnectDelay+100*time.Millisecond <= step {
		t.Errorf("fast-path callers advanced the shared timestamp: step %s", step)
	}
	overCapRelease()

	// releasing a slot restores the fast path
	releases[0]()
	againTime, againRelease := strategy.NextReconnectTime()
	if !againTime.Before(overCapTime) {
		t.Errorf("released slot did not restore the fast path: %s >= %s", againTime, overCapTime)
	}
	againRelease()

	// release is idempotent: double-releasing must not free a slot twice.
	// after this loop exactly ReconnectFastPathLimit slots are free again.
	releases[0]()
	for _, release := range releases[1:] {
		release()
	}
	for range settings.ReconnectFastPathLimit {
		_, release := strategy.NextReconnectTime()
		defer release()
	}
}

// a cancelled pacing wait gives its staircase reservation back. Before the
// release existed, a dialer torn down while waiting for its slot left its
// 100ms-1s step consumed forever, so rapid connect/disconnect cycles pushed
// the one shared timestamp arbitrarily far ahead of wall clock (2026-08-09
// field capture: 60s+ lead, cohorts of ~10 transport-downs expiring every 15s,
// 0 proven connections). All arithmetic on returned times -- no sleeps.
// Min == Max pins the serialized step to exactly 100ms so the assertions are
// deterministic.
func TestNextConnectTimeCancelReleasesReservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	settings.MinNextConnectDelay = 100 * time.Millisecond
	settings.MaxNextConnectDelay = 100 * time.Millisecond
	strategy := NewClientStrategy(ctx, settings)

	// 50 dialers reserve a step each and are cancelled before dialing -- the
	// shape of a multi-client window torn down mid-connect
	releases := []func(){}
	var deepest time.Time
	for range 50 {
		var release func()
		deepest, release = strategy.NextConnectTime()
		releases = append(releases, release)
	}
	// the staircase is genuinely deep before the releases...
	if lead := time.Until(deepest); lead < 4*time.Second {
		t.Fatalf("staircase priming failed: lead %s", lead)
	}
	for _, release := range releases {
		release()
	}
	// ...and every cancelled wait gave its step back: a fresh caller is paced
	// one step from now, not queued behind 50 dials that will never happen
	next, release := strategy.NextConnectTime()
	release()
	if lead := time.Until(next); 2*settings.MaxNextConnectDelay < lead {
		t.Errorf("cancelled waits leaked their reservations: lead %s", lead)
	}

	// release is idempotent: a double release must not free a second step.
	// reserve three steps, double-release the first, and the next reservation
	// must land one step past the third (back where the third was), not two
	// steps down.
	_, releaseA := strategy.NextConnectTime()
	strategy.NextConnectTime()
	last, _ := strategy.NextConnectTime()
	releaseA()
	releaseA()
	final, _ := strategy.NextConnectTime()
	if final.Before(last) {
		t.Errorf("double release freed a second step: %s < %s", final, last)
	}
}

// the safety clamp behind the release: even when reservations leak (a release
// never runs -- e.g. the over-cap reconnect fallback), the shared timestamp
// never runs more than nextConnectMaxLead ahead of wall clock, so no dialer is
// ever born more than that from its first dial.
func TestNextConnectTimeLeadClamp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	settings.MinNextConnectDelay = 100 * time.Millisecond
	settings.MaxNextConnectDelay = 200 * time.Millisecond
	strategy := NewClientStrategy(ctx, settings)

	// 200 leaked reservations would stack >= 20s of staircase unclamped
	var last time.Time
	for range 200 {
		last, _ = strategy.NextConnectTime()
	}
	if lead := time.Until(last); nextConnectMaxLead < lead {
		t.Errorf("staircase lead %s exceeds the clamp %s", lead, nextConnectMaxLead)
	}
	// the clamp bounds the lead but does not erase pacing: with >= 20s of
	// unclamped demand the lead must be sitting AT the bound, not below it
	if lead := time.Until(last); lead < nextConnectMaxLead-time.Second {
		t.Errorf("lead %s is far below the clamp %s: the bound is not what limited it", lead, nextConnectMaxLead)
	}
}

// the TLS side of the reconnect fast path: without a session cache every
// re-dial pays a full handshake. The cache must exist on the default config
// and be SHARED by clones, because both the resilient dialers and the h3
// transport clone their config per dial -- a cache that cloned with the
// config would never see a second use.
func TestClientStrategyTlsSessionResumptionCache(t *testing.T) {
	tlsConfig, err := DefaultTlsConfig()
	AssertEqual(t, err, nil)
	if tlsConfig.ClientSessionCache == nil {
		t.Fatal("DefaultTlsConfig has no ClientSessionCache: every reconnect pays a full handshake")
	}
	if cloned := tlsConfig.Clone(); cloned.ClientSessionCache != tlsConfig.ClientSessionCache {
		t.Error("Clone does not share the session cache: per-dial clones could never resume")
	}

	// the two production consumers both flow from DefaultTlsConfig
	connectSettings := DefaultConnectSettings()
	if connectSettings.TlsConfig.ClientSessionCache == nil {
		t.Error("ConnectSettings.TlsConfig has no session cache")
	}
	transportSettings := DefaultPlatformTransportSettings()
	if transportSettings.QuicTlsConfig.ClientSessionCache == nil {
		t.Error("QuicTlsConfig has no session cache: h3 re-dials cannot resume (or 0-RTT)")
	}
}

// The fast path's cap and jitter are settings: with a cap of one, a second
// concurrent reconnect takes the staircase instead of a slot, and with no
// jitter the fast path and a kick re-dial both dial at once.
func TestReconnectFastPathHonorsItsSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	settings.ReconnectFastPathLimit = 1
	settings.ReconnectFastPathMaxDelay = 0
	strategy := NewClientStrategy(ctx, settings)

	start := time.Now()
	first, releaseFirst := strategy.NextReconnectTime()
	defer releaseFirst()
	_, releaseSecond := strategy.NextReconnectTime()
	defer releaseSecond()
	func() {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		AssertEqual(t, strategy.reconnectFastPathCount, 1)
	}()
	if lead := first.Sub(start); 50*time.Millisecond < lead {
		t.Fatalf("fast path with no jitter dialed %s late", lead)
	}
	kick, releaseKick := strategy.NextKickReconnectTime()
	defer releaseKick()
	if lead := kick.Sub(start); 50*time.Millisecond < lead {
		t.Fatalf("kick re-dial with no jitter dialed %s late", lead)
	}
}

func coldConnectFastPathCount(strategy *ClientStrategy) int {
	strategy.mutex.Lock()
	defer strategy.mutex.Unlock()
	return strategy.coldConnectFastPathCount
}

// The candidates of one window expansion each dial a new platform transport on
// the device's strategy at once. On the staircase the fourth waits three
// 100ms-1s steps behind the first; on the cold-connect fast path all four dial
// within the small jitter. All arithmetic on returned times -- no sleeps.
func TestColdConnectFastPathExpansionBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	AssertEqual(t, settings.ColdConnectFastPathLimit, 4)
	AssertEqual(t, settings.ColdConnectFastPathLimit, multiClientExpandStepMax)
	if settings.MinNextConnectDelay < settings.ColdConnectFastPathMaxDelay {
		t.Fatalf("cold jitter %s exceeds the staircase step %s", settings.ColdConnectFastPathMaxDelay, settings.MinNextConnectDelay)
	}

	staircase := NewClientStrategy(ctx, settings)
	start := time.Now()
	var staircaseLast time.Time
	for range multiClientExpandStepMax {
		staircaseLast, _ = staircase.NextConnectTime()
	}
	if lead := staircaseLast.Sub(start); lead < time.Duration(multiClientExpandStepMax-1)*settings.MinNextConnectDelay {
		t.Fatalf("staircase burst too shallow: %s", lead)
	}

	strategy := NewClientStrategy(ctx, settings)
	releases := []func(){}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := range multiClientExpandStepMax {
		start := time.Now()
		connectTime, release, cancelConnect := strategy.NextColdConnectTime()
		releases = append(releases, release)
		// a fast-path caller holds no staircase reservation
		cancelConnect()
		if lead := connectTime.Sub(start); settings.ColdConnectFastPathMaxDelay+50*time.Millisecond <= lead {
			t.Fatalf("first dial %d paced %s ahead, want within the %s jitter", i, lead, settings.ColdConnectFastPathMaxDelay)
		}
	}
	AssertEqual(t, coldConnectFastPathCount(strategy), multiClientExpandStepMax)
}

// Independent jitter, capped concurrency, no effect on the shared staircase,
// slots that recycle on release, and an idempotent release.
func TestNextColdConnectTimeCapAndRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	// the serialized step is larger than the fast-path jitter so the two
	// shapes are provably disjoint
	settings.MinNextConnectDelay = 300 * time.Millisecond
	settings.MaxNextConnectDelay = 400 * time.Millisecond
	settings.ColdConnectFastPathLimit = 3
	settings.ColdConnectFastPathMaxDelay = 50 * time.Millisecond
	strategy := NewClientStrategy(ctx, settings)

	// prime the staircase well past the fast-path jitter range
	start := time.Now()
	var serialLast time.Time
	for range 3 {
		serialLast, _ = strategy.NextConnectTime()
	}
	if serialLast.Before(start.Add(2 * settings.MinNextConnectDelay)) {
		t.Fatalf("staircase priming failed: %s", serialLast.Sub(start))
	}

	releases := []func(){}
	for i := range settings.ColdConnectFastPathLimit {
		connectTime, release, cancelConnect := strategy.NextColdConnectTime()
		cancelConnect()
		releases = append(releases, release)
		if !connectTime.Before(serialLast) {
			t.Errorf("fast path call %d landed on the staircase: %s >= %s", i, connectTime, serialLast)
		}
		if lead := connectTime.Sub(start); settings.ColdConnectFastPathMaxDelay+100*time.Millisecond <= lead {
			t.Errorf("fast path call %d jitter too large: %s", i, lead)
		}
	}
	AssertEqual(t, coldConnectFastPathCount(strategy), settings.ColdConnectFastPathLimit)

	// over the cap: the staircase, which the fast-path callers did not advance
	// -- one serialized step from serialLast
	overCapTime, overCapRelease, overCapCancel := strategy.NextColdConnectTime()
	if overCapTime.Before(serialLast) {
		t.Errorf("over-cap call bypassed the staircase: %s < %s", overCapTime, serialLast)
	}
	if step := overCapTime.Sub(serialLast); settings.MaxNextConnectDelay+100*time.Millisecond <= step {
		t.Errorf("fast-path callers advanced the shared timestamp: step %s", step)
	}
	// the over-cap caller holds no slot; its staircase reservation is returned
	overCapRelease()
	AssertEqual(t, coldConnectFastPathCount(strategy), settings.ColdConnectFastPathLimit)
	overCapCancel()
	next, nextCancel := strategy.NextConnectTime()
	nextCancel()
	if step := next.Sub(serialLast); settings.MaxNextConnectDelay+100*time.Millisecond <= step {
		t.Errorf("cancelled over-cap wait kept its staircase step: %s", step)
	}

	// releasing a slot restores the fast path
	releases[0]()
	againTime, againRelease, _ := strategy.NextColdConnectTime()
	if !againTime.Before(serialLast) {
		t.Errorf("released slot did not restore the fast path: %s >= %s", againTime, serialLast)
	}
	againRelease()

	// release is idempotent: a double release does not free a slot twice
	releases[0]()
	for _, release := range releases[1:] {
		release()
	}
	AssertEqual(t, coldConnectFastPathCount(strategy), 0)
}

// A non-positive limit disables the fast path: every first dial takes the
// staircase and no slot is ever held. A strategy without connect pacing adds
// no jitter on the fast path either.
func TestNextColdConnectTimeHonorsItsSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	disabledSettings := DefaultClientStrategySettings()
	disabledSettings.MinNextConnectDelay = 100 * time.Millisecond
	disabledSettings.MaxNextConnectDelay = 100 * time.Millisecond
	disabledSettings.ColdConnectFastPathLimit = 0
	disabled := NewClientStrategy(ctx, disabledSettings)
	start := time.Now()
	var last time.Time
	for range 4 {
		var release func()
		last, release, _ = disabled.NextColdConnectTime()
		release()
		AssertEqual(t, coldConnectFastPathCount(disabled), 0)
	}
	if lead := last.Sub(start); lead < 3*disabledSettings.MinNextConnectDelay {
		t.Errorf("disabled fast path did not staircase: %s", lead)
	}

	unpacedSettings := DefaultClientStrategySettings()
	unpacedSettings.MinNextConnectDelay = 0
	unpacedSettings.MaxNextConnectDelay = 0
	unpaced := NewClientStrategy(ctx, unpacedSettings)
	for i := range 64 {
		start := time.Now()
		connectTime, release, _ := unpaced.NextColdConnectTime()
		release()
		if lead := connectTime.Sub(start); 10*time.Millisecond < lead {
			t.Fatalf("unpaced first dial %d waited %s", i, lead)
		}
	}
}

// The transport's pacing: only a runner's first dial takes the cold-connect
// fast path; a retry takes the staircase, a re-dial after a connection the
// reconnect fast path, and a pinned transport its own backoff.
func TestNextDialTimeFirstDialTakesColdConnectFastPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientStrategySettings()
	settings.MinNextConnectDelay = 300 * time.Millisecond
	settings.MaxNextConnectDelay = 300 * time.Millisecond
	strategy := NewClientStrategy(ctx, settings)
	transport := newDialBackoffTestTransport(time.Hour, 4*time.Hour)
	transport.clientStrategy = strategy

	// prime the staircase so a staircase dial is provably later than a cold one
	start := time.Now()
	serialLast, _ := strategy.NextConnectTime()
	serialLast, _ = strategy.NextConnectTime()
	if serialLast.Before(start.Add(settings.MinNextConnectDelay)) {
		t.Fatalf("staircase priming failed: %s", serialLast.Sub(start))
	}

	first, releaseFirst, cancelFirst := transport.nextDialTime(false, false, true)
	cancelFirst()
	if !first.Before(serialLast) {
		t.Fatalf("first dial took the staircase: %s >= %s", first, serialLast)
	}
	AssertEqual(t, coldConnectFastPathCount(strategy), 1)
	releaseFirst()
	AssertEqual(t, coldConnectFastPathCount(strategy), 0)

	retry, releaseRetry, cancelRetry := transport.nextDialTime(false, false, false)
	releaseRetry()
	if retry.Before(serialLast) {
		t.Fatalf("a retry bypassed the staircase: %s < %s", retry, serialLast)
	}
	AssertEqual(t, coldConnectFastPathCount(strategy), 0)
	cancelRetry()

	_, releaseReconnect, _ := transport.nextDialTime(true, false, false)
	func() {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		AssertEqual(t, strategy.reconnectFastPathCount, 1)
		AssertEqual(t, strategy.coldConnectFastPathCount, 0)
	}()
	releaseReconnect()

	transport.ipFamily = 4
	_, releasePinned, cancelPinned := transport.nextDialTime(false, false, true)
	releasePinned()
	cancelPinned()
	AssertEqual(t, coldConnectFastPathCount(strategy), 0)
}
