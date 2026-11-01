package connect

import (
	"context"
	"strings"
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
// spread of that re-dial over [0, ReconnectJitter).

// A transport whose re-dial pacing is the spread alone: the strategy's own
// fast-path jitter is zero, and `draw` replaces the spread's uniform draw.
func newReconnectJitterTestTransport(
	t *testing.T,
	ipFamily int,
	maxJitter time.Duration,
	draw func(maxJitter time.Duration) time.Duration,
) *PlatformTransport {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ReconnectFastPathMaxDelay = 0
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectJitter = maxJitter
	settings.reconnectJitterForTest = draw
	return &PlatformTransport{
		log:            NewNoopLogger(),
		settings:       settings,
		clientStrategy: strategy,
		ipFamily:       ipFamily,
		pinnedBackoff:  newPinnedDialBackoff(time.Second, time.Minute),
		failBackoff:    newDialFailureBackoff(time.Second, time.Minute),
	}
}

// The lead of one dial's pacing over the nextDialTime call that drew it: the
// dial time less the instant the call returned, and less the instant it
// began. The pacing the call drew lies between the two.
func testingDialLead(transport *PlatformTransport, hadConnection bool, afterKick bool) (minLead time.Duration, maxLead time.Duration) {
	before := time.Now()
	connectTime, releaseReconnect, cancelConnect := transport.nextDialTime(hadConnection, afterKick)
	after := time.Now()
	releaseReconnect()
	cancelConnect()
	return connectTime.Sub(after), connectTime.Sub(before)
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

// The root cause: a family-agnostic transport re-dialed a lost connection on
// the strategy's fast path alone. Its lead is now the spread's draw, across the
// whole window.
func TestLostConnectionRedialSpreadsOverReconnectJitter(t *testing.T) {
	const maxJitter = 8 * time.Second
	values := []time.Duration{0, maxJitter / 4, maxJitter / 2, 3 * maxJitter / 4, maxJitter - 1}
	draws := newTestingJitterDraws(values...)
	transport := newReconnectJitterTestTransport(t, 0, maxJitter, draws.draw)

	for _, value := range values {
		minLead, maxLead := testingDialLead(transport, true, false)
		if value < minLead || maxLead < value {
			t.Fatalf("re-dial lead in [%s, %s], want the spread's draw %s", minLead, maxLead, value)
		}
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
		transport := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)
		for _, value := range values {
			minLead, maxLead := testingDialLead(transport, true, false)
			if value < minLead || maxLead < value {
				t.Fatalf("ipv%d re-dial lead in [%s, %s], want the spread's draw %s", ipFamily, minLead, maxLead, value)
			}
		}
		requireDrawWindows(t, draws, len(values), maxJitter)
	}
}

// The spread adds to a pinned transport's backoff rather than replacing it.
func TestPinnedLostConnectionRedialKeepsItsBackoff(t *testing.T) {
	const maxJitter = 8 * time.Second
	const spread = 3 * time.Second
	draws := newTestingJitterDraws(spread)
	transport := newReconnectJitterTestTransport(t, 4, maxJitter, draws.draw)
	// one failure: a backoff in [base/2, base) of the pinned base, 1 s
	transport.pinnedBackoff.fail()

	minLead, maxLead := testingDialLead(transport, true, false)
	if maxLead < spread+500*time.Millisecond || spread+time.Second <= minLead {
		t.Fatalf("re-dial lead in [%s, %s], want the spread %s plus a backoff in [500ms, 1s)", minLead, maxLead, spread)
	}
}

// The cap holds: a draw outside the window is held to [0, ReconnectJitter).
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
		draws := newTestingJitterDraws(c.draw)
		for _, ipFamily := range []int{0, 4} {
			transport := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)
			minLead, maxLead := testingDialLead(transport, true, false)
			if c.want < minLead || maxLead < c.want {
				t.Fatalf("ipv%d draw %s: re-dial lead in [%s, %s], want %s", ipFamily, c.draw, minLead, maxLead, c.want)
			}
		}
	}

	// the production draw never reaches the cap
	for _, ipFamily := range []int{0, 4} {
		transport := newReconnectJitterTestTransport(t, ipFamily, maxJitter, nil)
		for i := 0; i < 1000; i += 1 {
			minLead, maxLead := testingDialLead(transport, true, false)
			if maxJitter <= minLead || maxLead < 0 {
				t.Fatalf("ipv%d re-dial lead in [%s, %s], want a draw in [0, %s)", ipFamily, minLead, maxLead, maxJitter)
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
		transport := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)

		// a drawn spread would lead by maxJitter-1; none leads by about zero
		if _, maxLead := testingDialLead(transport, true, true); maxJitter/2 <= maxLead {
			t.Fatalf("ipv%d kick re-dial lead %s, want the fast path", ipFamily, maxLead)
		}
		// the first cold dial clamps the staircase to now
		if _, maxLead := testingDialLead(transport, false, false); maxJitter/2 <= maxLead {
			t.Fatalf("ipv%d cold dial lead %s, want the staircase", ipFamily, maxLead)
		}
		requireDrawWindows(t, draws, 0, maxJitter)
	}
}

// A non-positive ReconnectJitter re-dials on the other pacing alone.
func TestReconnectJitterNonPositiveRedialsOnPacingAlone(t *testing.T) {
	for _, maxJitter := range []time.Duration{0, -time.Second} {
		for _, ipFamily := range []int{0, 4} {
			draws := newTestingJitterDraws(time.Hour)
			transport := newReconnectJitterTestTransport(t, ipFamily, maxJitter, draws.draw)
			if _, maxLead := testingDialLead(transport, true, false); time.Minute <= maxLead {
				t.Fatalf("ipv%d ReconnectJitter %s: re-dial lead %s, want none", ipFamily, maxJitter, maxLead)
			}
			requireDrawWindows(t, draws, 0, maxJitter)
		}
	}
}

func TestDefaultReconnectJitter(t *testing.T) {
	AssertEqual(t, DefaultPlatformTransportSettings().ReconnectJitter, 5*time.Second)
}

// Both mode runners take their dial pacing from nextDialTime, so the H3 modes
// spread a lost connection's re-dial as H1 does. The H1 runner is driven end
// to end below.
func TestRunnersPaceDialsThroughNextDialTime(t *testing.T) {
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
		if !strings.Contains(body, "self.nextDialTime(hadConnection, afterKick)") {
			t.Errorf("%s does not pace its dials through nextDialTime", signature)
		}
	}
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

	if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
		return 1 <= counts.connected
	}); !ok {
		t.Fatal("the transport never connected")
	}
	// the first dial is cold: no spread
	requireDrawWindows(t, draws, 0, maxJitter)

	// the platform restarts: it closes every live connection
	closeTime := time.Now()
	platform.closeConns()
	if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
		return 2 <= counts.connected
	}); !ok {
		t.Fatal("the transport never re-dialed after the platform closed its connection")
	}
	if elapsed := time.Since(closeTime); elapsed < spread {
		t.Fatalf("re-dialed %s after the platform closed the connection, want at least the %s spread", elapsed, spread)
	}
	requireDrawWindows(t, draws, 1, maxJitter)

	// a kick is this device's own event: its re-dial draws no spread
	transport.Kick()
	if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
		return 3 <= counts.connected
	}); !ok {
		t.Fatal("the transport never re-dialed after a kick")
	}
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
	if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
		return 1 <= counts.connected
	}); !ok {
		t.Fatal("the pinned transport never connected")
	}
	requireDrawWindows(t, draws, 0, maxJitter)

	closeTime := time.Now()
	platform.closeConns()
	if _, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
		return 2 <= counts.connected
	}); !ok {
		t.Fatal("the pinned transport never re-dialed after the platform closed its connection")
	}
	if elapsed := time.Since(closeTime); elapsed < spread {
		t.Fatalf("re-dialed %s after the platform closed the connection, want at least the %s spread", elapsed, spread)
	}
	requireDrawWindows(t, draws, 1, maxJitter)
	if transport.IpFamily() != 4 {
		t.Fatalf("IpFamily = %d, want 4", transport.IpFamily())
	}
}
