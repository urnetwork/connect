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
		next := strategy.NextConnectTime()
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
		serialLast = strategy.NextConnectTime()
	}
	if serialLast.Before(start.Add(2 * settings.MinNextConnectDelay)) {
		t.Fatalf("staircase priming failed: %s", serialLast.Sub(start))
	}

	// up to the cap, reconnects are fast and independent of the staircase
	releases := []func(){}
	for i := range reconnectFastPathLimit {
		connectTime, release := strategy.NextReconnectTime()
		releases = append(releases, release)
		if !connectTime.Before(serialLast) {
			t.Errorf("fast path call %d landed on the staircase: %s >= %s", i, connectTime, serialLast)
		}
		if jitter := connectTime.Sub(start); reconnectFastPathMaxDelay+100*time.Millisecond <= jitter {
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
	// after this loop exactly reconnectFastPathLimit slots are free again.
	releases[0]()
	for _, release := range releases[1:] {
		release()
	}
	for range reconnectFastPathLimit {
		_, release := strategy.NextReconnectTime()
		defer release()
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
