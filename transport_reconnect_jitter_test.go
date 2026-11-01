package connect

import (
	"context"
	"crypto/tls"
	"sync"
	"testing"
	"time"
)

// A platform restart or deploy ends every client's platform connection in the
// same instant. On 2026-11-01 every api block restarted at 05:53Z, and the
// reconnect waves that followed came with 503 storms and fleet-wide client
// drops. A connection that ran and died without a network change kick was
// re-dialed within the strategy's 250 ms fast-path jitter, and a pinned
// transport's at once, so the fleet re-dialed in lockstep. These tests pin the
// spread of that re-dial over [0, ReconnectJitter). The spread's interplay
// with the transport's holds and the strategy's pacing is in
// transport_redial_hold_test.go.

// A transport whose pacing a test reads through a recorder of its pre-dial
// waits: the strategy's own fast-path jitter is zero, so a re-dial's waits are
// the spread and a pinned transport's backoff alone, and `draw` replaces the
// spread's uniform draw. Each wait ends at once.
func newReconnectJitterTestTransport(
	t *testing.T,
	ipFamily int,
	maxJitter time.Duration,
	draw func(maxJitter time.Duration) time.Duration,
) (*PlatformTransport, *testingPacingWaits) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ReconnectFastPathMaxDelay = 0
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)
	waits := newTestingPacingWaits(strategy, false)
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectJitter = maxJitter
	settings.reconnectJitterForTest = draw
	settings.pacingAfterForTest = waits.after
	return &PlatformTransport{
		ctx:            ctx,
		log:            NewNoopLogger(),
		settings:       settings,
		clientStrategy: strategy,
		ipFamily:       ipFamily,
		pinnedBackoff:  newPinnedDialBackoff(time.Second, time.Minute),
		failBackoff:    newDialFailureBackoff(time.Second, time.Minute),
		kickMonitor:    NewMonitor(),
	}, waits
}

// The pre-dial waits of one pacing, which a recorder that ends every wait at
// once lets run to its end. The pacing's release is called, as after a dial.
func testingDialPacingWaits(
	t *testing.T,
	transport *PlatformTransport,
	waits *testingPacingWaits,
	hadConnection bool,
	afterKick bool,
) []time.Duration {
	t.Helper()
	kickGeneration := transport.kickGeneration.Load()
	pacing, releaseReconnect, _ := transport.waitDialPacing(transport.ctx, hadConnection, afterKick, &kickGeneration)
	if pacing != dialWaitElapsed {
		t.Fatalf("pacing ended with %d, want the waits to elapse", pacing)
	}
	releaseReconnect()
	delays := []time.Duration{}
	for {
		select {
		case wait := <-waits.waits:
			delays = append(delays, wait.delay)
			continue
		default:
		}
		return delays
	}
}

// A recorded sequence of spread draws. Each draw returns the next value and
// records the window it was asked for. Safe for concurrent use.
type testingJitterDraws struct {
	stateLock  sync.Mutex
	values     []time.Duration
	maxJitters []time.Duration
}

func newTestingJitterDraws(values ...time.Duration) *testingJitterDraws {
	return &testingJitterDraws{values: values}
}

func (self *testingJitterDraws) draw(maxJitter time.Duration) time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	index := len(self.maxJitters)
	self.maxJitters = append(self.maxJitters, maxJitter)
	if len(self.values) == 0 {
		return 0
	}
	return self.values[min(index, len(self.values)-1)]
}

func (self *testingJitterDraws) drawn() []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Duration{}, self.maxJitters...)
}

func requireDrawWindows(t *testing.T, draws *testingJitterDraws, count int, maxJitter time.Duration) {
	t.Helper()
	maxJitters := draws.drawn()
	if len(maxJitters) != count {
		t.Fatalf("spread drawn %d times, want %d", len(maxJitters), count)
	}
	for _, drawnMaxJitter := range maxJitters {
		if drawnMaxJitter != maxJitter {
			t.Fatalf("spread drawn over [0, %s), want [0, %s)", drawnMaxJitter, maxJitter)
		}
	}
}

// The waits a re-dial with the spread `spread` must make: the spread, which is
// no wait when it is zero, then `rest`.
func testingSpreadWaits(spread time.Duration, rest ...time.Duration) []time.Duration {
	waits := []time.Duration{}
	if 0 < spread {
		waits = append(waits, spread)
	}
	return append(waits, rest...)
}

func requireWaits(t *testing.T, label string, waits []time.Duration, want []time.Duration) {
	t.Helper()
	if len(waits) != len(want) {
		t.Fatalf("%s: pre-dial waits %v, want %v", label, waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("%s: pre-dial waits %v, want %v", label, waits, want)
		}
	}
}

// The root cause: a family-agnostic transport re-dialed a lost connection on
// the strategy's fast path alone. It now waits the spread's draw first, across
// the whole window.
func TestLostConnectionRedialSpreadsOverReconnectJitter(t *testing.T) {
	const maxJitter = 8 * time.Second
	values := []time.Duration{0, maxJitter / 4, maxJitter / 2, 3 * maxJitter / 4, maxJitter - 1}
	draws := newTestingJitterDraws(values...)
	transport, waits := newReconnectJitterTestTransport(t, 0, maxJitter, draws.draw)

	for _, value := range values {
		requireWaits(t, "re-dial", testingDialPacingWaits(t, transport, waits, true, false), testingSpreadWaits(value))
	}
	requireDrawWindows(t, draws, len(values), maxJitter)
}

// The root cause for a pinned transport, the provider's v4 and v6 carriers: a
// lost connection re-dialed at once, since a connection that ran reset the
// pinned backoff to zero.
func TestPinnedLostConnectionRedialSpreadsOverReconnectJitter(t *testing.T) {
	const maxJitter = 8 * time.Second
	values := []time.Duration{0, maxJitter / 3, 2 * maxJitter / 3, maxJitter - 1}
	for _, ipFamily := range []int{4, 6} {
		draws := newTestingJitterDraws(values...)
		transport, waits := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)
		for _, value := range values {
			requireWaits(t, "pinned re-dial", testingDialPacingWaits(t, transport, waits, true, false), testingSpreadWaits(value))
		}
		requireDrawWindows(t, draws, len(values), maxJitter)
	}
}

// The spread comes before a pinned transport's backoff rather than replacing
// it.
func TestPinnedLostConnectionRedialKeepsItsBackoff(t *testing.T) {
	const maxJitter = 8 * time.Second
	const spread = 3 * time.Second
	draws := newTestingJitterDraws(spread)
	transport, waits := newReconnectJitterTestTransport(t, 4, maxJitter, draws.draw)
	// one failure: a backoff in [base/2, base) of the pinned base, 1 s
	transport.pinnedBackoff.fail()

	delays := testingDialPacingWaits(t, transport, waits, true, false)
	if len(delays) != 2 || delays[0] != spread || delays[1] < 450*time.Millisecond || time.Second <= delays[1] {
		t.Fatalf("pre-dial waits %v, want the spread %s, then a backoff in [500ms, 1s)", delays, spread)
	}
}

// The cap holds: a draw outside the window is held to [0, ReconnectJitter),
// and the production draw never reaches the cap.
func TestReconnectJitterDrawHoldsItsWindow(t *testing.T) {
	const maxJitter = 8 * time.Second
	for _, c := range []struct {
		draw time.Duration
		want time.Duration
	}{
		{-maxJitter, 0},
		{maxJitter, maxJitter - 1},
		{10 * maxJitter, maxJitter - 1},
	} {
		for _, ipFamily := range []int{0, 4} {
			transport, waits := newReconnectJitterTestTransport(t, ipFamily, maxJitter, newTestingJitterDraws(c.draw).draw)
			requireWaits(t, "held draw", testingDialPacingWaits(t, transport, waits, true, false), testingSpreadWaits(c.want))
		}
	}

	for _, ipFamily := range []int{0, 4} {
		transport, waits := newReconnectJitterTestTransport(t, ipFamily, maxJitter, nil)
		for i := 0; i < 1000; i += 1 {
			for _, delay := range testingDialPacingWaits(t, transport, waits, true, false) {
				if delay < 0 || maxJitter <= delay {
					t.Fatalf("ipv%d re-dial spread %s, want in [0, %s)", ipFamily, delay, maxJitter)
				}
			}
		}
	}
}

// A kick re-dial keeps the fast path: a network change is this device's own
// event. A cold dial keeps the staircase. Neither draws the spread.
func TestKickAndColdDialsTakeNoReconnectJitter(t *testing.T) {
	const maxJitter = 8 * time.Second
	for _, ipFamily := range []int{0, 4, 6} {
		draws := newTestingJitterDraws(maxJitter - 1)
		transport, waits := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)

		requireWaits(t, "kick re-dial", testingDialPacingWaits(t, transport, waits, true, true), nil)
		// the first cold dial clamps the staircase to now
		requireWaits(t, "cold dial", testingDialPacingWaits(t, transport, waits, false, false), nil)
		requireDrawWindows(t, draws, 0, maxJitter)
	}
}

// A kick during the spread ends it, and the re-dial takes the kick's path at
// once rather than waiting out the rest of the spread.
func TestKickDuringRedialSpreadRedialsOnTheKickPath(t *testing.T) {
	const maxJitter = 8 * time.Second
	transport, waits := newReconnectJitterTestTransport(t, 0, maxJitter, newTestingJitterDraws(maxJitter-1).draw)
	waits.hold.Store(true)

	kickGeneration := transport.kickGeneration.Load()
	type result struct {
		pacing  dialWait
		release func()
	}
	done := make(chan result, 1)
	go func() {
		pacing, releaseReconnect, _ := transport.waitDialPacing(transport.ctx, true, false, &kickGeneration)
		done <- result{pacing: pacing, release: releaseReconnect}
	}()
	spreadWait := waits.next(t)
	AssertEqual(t, spreadWait.delay, maxJitter-1)
	transport.Kick()
	select {
	case r := <-done:
		if r.pacing == dialWaitHeld || r.pacing == dialWaitClosed {
			t.Fatalf("pacing ended with %d after a kick, want a dial", r.pacing)
		}
		r.release()
	case <-time.After(15 * time.Second):
		t.Fatal("a kick did not end the re-dial spread")
	}
	if kickGeneration != transport.kickGeneration.Load() {
		t.Fatal("the pacing did not take the kick it ended on")
	}
}

// A non-positive ReconnectJitter re-dials on the other pacing alone.
func TestReconnectJitterNonPositiveRedialsOnPacingAlone(t *testing.T) {
	for _, maxJitter := range []time.Duration{0, -time.Second} {
		for _, ipFamily := range []int{0, 4} {
			draws := newTestingJitterDraws(time.Hour)
			transport, waits := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)
			requireWaits(t, "re-dial", testingDialPacingWaits(t, transport, waits, true, false), nil)
			requireDrawWindows(t, draws, 0, maxJitter)
		}
	}
}

func TestDefaultReconnectJitter(t *testing.T) {
	AssertEqual(t, DefaultPlatformTransportSettings().ReconnectJitter, 5*time.Second)
}

// The spread reaches the real H1 runner: after the platform closes a live
// connection, the re-dial draws the spread once and waits it out. A lower
// bound only, which a timer cannot undercut. A kick's re-dial draws nothing.
func TestPlatformTransportRedialAfterPlatformCloseWaitsItsReconnectJitter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const maxJitter = 10 * time.Second
	const spread = 700 * time.Millisecond
	platform := newTestingPlatformServer(t)
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = maxJitter
	draws := newTestingJitterDraws(spread)
	settings.reconnectJitterForTest = draws.draw
	connections := observeTestingTransportConnections(settings)
	transport := testingPlatformTransport(t, ctx, platform.url, settings)

	waitTestingConnected(t, connections, 1)
	// the first dial is cold: no spread
	requireDrawWindows(t, draws, 0, maxJitter)

	// the platform restarts: it closes every live connection
	closeTime := time.Now()
	platform.closeConns()
	waitTestingConnected(t, connections, 2)
	if elapsed := time.Since(closeTime); elapsed < spread {
		t.Fatalf("re-dialed %s after the platform closed the connection, want at least the %s spread", elapsed, spread)
	}
	requireDrawWindows(t, draws, 1, maxJitter)

	// a kick is this device's own event: its re-dial draws no spread
	transport.Kick()
	waitTestingConnected(t, connections, 3)
	requireDrawWindows(t, draws, 1, maxJitter)
}

// The same for a pinned transport, the shape of a provider's v4 and v6
// carriers, which re-dialed a closed connection at once.
func TestPinnedTransportRedialAfterPlatformCloseWaitsItsReconnectJitter(t *testing.T) {
	clientSettings := newTestingFamilyStrategySettings(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const maxJitter = 10 * time.Second
	const spread = 700 * time.Millisecond
	platform := newTestingFamilyPlatformServer(t, 4, false)
	settings := testingFamilyTransportSettings()
	settings.ReconnectJitter = maxJitter
	draws := newTestingJitterDraws(spread)
	settings.reconnectJitterForTest = draws.draw
	connections := observeTestingTransportConnections(settings)
	transport := newTestingPinnedTransport(t, ctx, clientSettings, platform.dualStackURL(), 4, settings)

	receiveFamilyConnection(t, platform, 15*time.Second)
	waitTestingConnected(t, connections, 1)
	requireDrawWindows(t, draws, 0, maxJitter)

	closeTime := time.Now()
	platform.closeConns()
	waitTestingConnected(t, connections, 2)
	if elapsed := time.Since(closeTime); elapsed < spread {
		t.Fatalf("re-dialed %s after the platform closed the connection, want at least the %s spread", elapsed, spread)
	}
	requireDrawWindows(t, draws, 1, maxJitter)
	if transport.IpFamily() != 4 {
		t.Fatalf("IpFamily = %d, want 4", transport.IpFamily())
	}
}

// The same for the H3 runner: the platform ends the QUIC connection with an
// ordinary close, and the re-dial waits out the spread it drew.
func TestPlatformTransportH3RedialAfterPlatformCloseWaitsItsReconnectJitter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const maxJitter = 10 * time.Second
	const spread = 700 * time.Millisecond
	platform := newTestingClientLimitH3Platform(t)
	settings := testingFamilyTransportSettings()
	settings.H3Port = platform.port
	settings.QuicConnectTimeout = 2 * time.Second
	settings.QuicHandshakeTimeout = 2 * time.Second
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{platform.nextProto},
	}
	settings.FramerSettings = platform.framerSets
	settings.ReconnectJitter = maxJitter
	draws := newTestingJitterDraws(spread)
	settings.reconnectJitterForTest = draws.draw
	connections := observeTestingTransportConnections(settings)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "redial-h3"),
		"https://"+testLoopbackHost(4),
		&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
		TransportModeH3,
		settings,
	)
	defer transport.Close()

	conn := platform.nextConnection(t)
	waitTestingConnected(t, connections, 1)
	requireDrawWindows(t, draws, 0, maxJitter)

	closeTime := time.Now()
	conn.CloseWithError(0, "platform restart")
	platform.nextConnection(t)
	waitTestingConnected(t, connections, 2)
	if elapsed := time.Since(closeTime); elapsed < spread {
		t.Fatalf("re-dialed %s after the platform closed the connection, want at least the %s spread", elapsed, spread)
	}
	requireDrawWindows(t, draws, 1, maxJitter)
}
