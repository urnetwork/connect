package connect

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// A kick that lands while a dial is in flight cancels the dial and re-dials at
// once. It used to be lost: the runner fetched its notify channel only after
// the dial returned, so a dial started on the path that just changed ran to
// its deadline there and the failure then waited out the retry backoff.
func TestKickCancelsInFlightDial(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		platform, held := newTestingPlatformServerHoldingUpgrades(t, ipVersion)
		settings := testingPlatformTransportSettings()
		// without the kick a stalled dial, and then its retry, would both
		// outlast this test
		settings.ReconnectTimeout = 60 * time.Second
		connections := observeTestingTransportConnections(settings)
		transport := testingPlatformTransport(t, ctx, platform.url, settings)
		defer transport.Close()

		var stateLock sync.Mutex
		upgrades := []*testingPlatformUpgrade{}
		releaseAll := false
		upgradeCount := func() int {
			stateLock.Lock()
			defer stateLock.Unlock()
			return len(upgrades)
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case upgrade := <-held:
					stateLock.Lock()
					upgrades = append(upgrades, upgrade)
					if releaseAll {
						close(upgrade.release)
					}
					stateLock.Unlock()
				}
			}
		}()
		release := func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			releaseAll = true
			for _, upgrade := range upgrades {
				close(upgrade.release)
			}
		}

		// the first dial parks in the platform's upgrade; give every dialer it
		// races time to arrive before counting
		if !waitForCondition(15*time.Second, func() bool { return 0 < upgradeCount() }) {
			t.Fatal("the transport never dialed")
		}
		time.Sleep(300 * time.Millisecond)
		parked := upgradeCount()

		transport.Kick()

		// the kicked runner dials again without waiting for the parked dial to
		// time out (15s) or for the 60s retry backoff
		if !waitForCondition(5*time.Second, func() bool { return parked < upgradeCount() }) {
			t.Fatal("a kick during the dial did not start a new dial")
		}

		release()
		if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
			return 1 <= counts.connected
		}); !ok {
			t.Fatal("the re-dial after the kick never connected")
		}
		if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
			mode, _ := transport.activeMode()
			t.Fatalf("active mode = %q after the kicked re-dial, want h1", mode)
		}
	})
}

// A connection a kick closes re-dials at once, even when it is younger than
// ReconnectTimeout. The post-connection wait used to run the rest of the
// reconnect timer, measured from the connection's start, so a second kick
// soon after a reconnect held every exit back by up to ReconnectTimeout.
func TestKickedYoungConnectionRedialsImmediately(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		platform := newTestingPlatformServerIpVersion(t, ipVersion)
		settings := testingPlatformTransportSettings()
		settings.ReconnectTimeout = 60 * time.Second
		connections := observeTestingTransportConnections(settings)
		transport := testingPlatformTransport(t, ctx, platform.url, settings)
		defer transport.Close()

		if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
			return 1 <= counts.connected
		}); !ok {
			t.Fatal("the transport never connected")
		}

		// the connection is well under a second old: waiting out the residual
		// reconnect timer would hold the re-dial for most of a minute
		transport.Kick()

		if _, ok := waitTestingTransportConnections(connections, 5*time.Second, func(counts testingTransportConnectionCounts) bool {
			return 1 <= counts.disconnected && 2 <= counts.connected
		}); !ok {
			t.Fatal("a kicked young connection did not re-dial immediately")
		}

		// a second kick right after the reconnect is the same case again
		transport.Kick()
		if _, ok := waitTestingTransportConnections(connections, 5*time.Second, func(counts testingTransportConnectionCounts) bool {
			return 2 <= counts.disconnected && 3 <= counts.connected
		}); !ok {
			t.Fatal("a second kick shortly after the first did not re-dial immediately")
		}
	})
}

// A host network change starts a fresh backend failure streak. Dials that
// failed while the old path went away say nothing about the backend over the
// new one, and the gate they tripped would otherwise skip contract requests
// and window growth until a re-dial on the new path succeeded.
func TestNetworkChangedResetsBackendDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("precondition: degraded after the threshold of failures")
	}
	NetworkChanged()
	if isBackendDegraded() {
		t.Fatal("a network change left the backend marked degraded")
	}
	// a real outage still re-trips it
	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("fresh failures after a network change did not re-trip the gate")
	}
}

// The re-dial a kick asks for takes the fast path's jitter on slots of its
// own: a kick re-dials every transport on the strategy at once, and the fast
// path's cap would stagger the window's exits past the fourth onto the
// staircase. Its own cap (KickReconnectLimit) sits above a device's transport
// count, so a switch is never staggered today.
func TestKickReconnectTimeTakesItsOwnSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	strategy := NewClientStrategy(ctx, settings)
	// above the about 11 transports a desktop re-dials after a kick
	AssertEqual(t, settings.KickReconnectLimit, 12)

	// hold every fast-path slot
	releases := []func(){}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < settings.ReconnectFastPathLimit; i++ {
		_, release := strategy.NextReconnectTime()
		releases = append(releases, release)
	}

	for i := 0; i < settings.KickReconnectLimit; i++ {
		start := time.Now()
		next, release := strategy.NextKickReconnectTime()
		releases = append(releases, release)
		if lead := next.Sub(start); settings.ReconnectFastPathMaxDelay < lead {
			t.Fatalf("kick re-dial %d paced %s ahead, want at most the %s jitter", i, lead, settings.ReconnectFastPathMaxDelay)
		}
	}
	func() {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		AssertEqual(t, strategy.reconnectFastPathCount, settings.ReconnectFastPathLimit)
		AssertEqual(t, strategy.kickReconnectCount, settings.KickReconnectLimit)
	}()
}

// A kick re-dial past KickReconnectLimit takes the ordinary reconnect path, and
// a released kick slot is taken again by the next kick.
func TestKickReconnectPastItsCapTakesTheReconnectPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	settings.KickReconnectLimit = 2
	strategy := NewClientStrategy(ctx, settings)
	counts := func() (kick int, fastPath int) {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		return strategy.kickReconnectCount, strategy.reconnectFastPathCount
	}

	_, releaseFirst := strategy.NextKickReconnectTime()
	_, releaseSecond := strategy.NextKickReconnectTime()
	_, releaseThird := strategy.NextKickReconnectTime()
	kick, fastPath := counts()
	AssertEqual(t, kick, 2)
	AssertEqual(t, fastPath, 1)

	// the third held a fast-path slot, and gives back that one
	releaseThird()
	releaseThird()
	kick, fastPath = counts()
	AssertEqual(t, kick, 2)
	AssertEqual(t, fastPath, 0)

	releaseFirst()
	_, releaseFourth := strategy.NextKickReconnectTime()
	kick, fastPath = counts()
	AssertEqual(t, kick, 2)
	AssertEqual(t, fastPath, 0)
	releaseSecond()
	releaseFourth()
	kick, _ = counts()
	AssertEqual(t, kick, 0)
}

// A non-positive KickReconnectLimit removes the cap.
func TestKickReconnectLimitNonPositiveIsUncapped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	settings.KickReconnectLimit = 0
	strategy := NewClientStrategy(ctx, settings)
	releases := []func(){}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < 64; i++ {
		start := time.Now()
		next, release := strategy.NextKickReconnectTime()
		releases = append(releases, release)
		if lead := next.Sub(start); settings.ReconnectFastPathMaxDelay < lead {
			t.Fatalf("uncapped kick re-dial %d paced %s ahead", i, lead)
		}
	}
	func() {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		AssertEqual(t, strategy.reconnectFastPathCount, 0)
	}()
}

// After a kick, a lower Auto mode waits out its ModeInitialDelay before it
// dials, so the preferred mode gets the new path first; the preferred mode and
// an explicit mode never wait.
func TestHoldAfterKickParksLowerAutoModes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newTransport := func(targetMode TransportMode) *PlatformTransport {
		settings := DefaultPlatformTransportSettings()
		settings.StartDisabled = true
		settings.ModeInitialDelay = 300 * time.Millisecond
		transport := NewPlatformTransportWithTargetMode(
			ctx,
			NewClientStrategyWithDefaults(ctx),
			NewRouteManager(ctx, "test"),
			"wss://127.0.0.1:1",
			&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
			targetMode,
			settings,
		)
		t.Cleanup(transport.Close)
		return transport
	}
	held := func(transport *PlatformTransport, mode TransportMode) time.Duration {
		start := time.Now()
		if !transport.holdAfterKick(ctx, mode) {
			t.Fatal("hold ended with the context")
		}
		return time.Since(start)
	}

	auto := newTransport(TransportModeAuto)
	if d := held(auto, TransportModeH3); 50*time.Millisecond < d {
		t.Fatalf("a transport that was never kicked held h3 for %s", d)
	}
	auto.Kick()
	if d := held(auto, TransportModeH1); 50*time.Millisecond < d {
		t.Fatalf("the preferred mode held for %s after a kick", d)
	}
	auto.Kick()
	if d := held(auto, TransportModeH3); d < 200*time.Millisecond {
		t.Fatalf("h3 held for only %s after a kick, want its 300ms mode delay", d)
	}

	explicit := newTransport(TransportModeH3)
	explicit.Kick()
	if d := held(explicit, TransportModeH3); 50*time.Millisecond < d {
		t.Fatalf("an explicit h3 transport held for %s after a kick", d)
	}
}

// Every place a runner waits on a kick reads the iteration's generation, and
// a kick in the pre-dial wait clears the backoff the old path earned: the
// first dial on a new path often fails once while the link finishes coming
// up, and its retry must not inherit the old network's grown backoff.
func TestRunnerKickSourceAnchor(t *testing.T) {
	source, err := readSource("transport.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, signature := range []string{
		"func (self *PlatformTransport) runH1(",
		"func (self *PlatformTransport) runH3(",
	} {
		body, ok := functionBody(source, signature)
		if !ok {
			t.Fatalf("could not find %s", signature)
		}
		if strings.Contains(body, "self.kickMonitor.NotifyChannel()") {
			t.Errorf("%s waits on a notify channel fetched without the iteration's kick generation", signature)
		}
		preDial := strings.Index(body, "network changed while waiting to dial")
		if preDial < 0 {
			t.Fatalf("%s: could not find the pre-dial kick arm", signature)
		}
		arm := body[preDial:]
		if end := strings.Index(arm, "case <-time.After(connectDelay):"); 0 <= end {
			arm = arm[:end]
		}
		if !strings.Contains(arm, "self.noteKick()") {
			t.Errorf("%s: a kick in the pre-dial wait does not reset the dial backoff", signature)
		}
		if !strings.Contains(body, "self.cancelOnKick(dialCtx, kickGeneration, cancelDial)") {
			t.Errorf("%s: a kick does not cancel the in-flight dial", signature)
		}
	}
}
