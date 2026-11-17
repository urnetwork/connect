package connect

// transport_family_route_test.go — a pinned transport whose family the routing
// has no route for sleeps instead of redialing (pinnedRouteHold, IPV6.md A5).
//
// The field case: a host whose only v6 address is a ULA, with its v6 default
// routes scoped to vpn tunnels. The address probe counts the ULA, so the v6
// pin was never held, and every dial of it was refused at once with no route
// to host (EHOSTUNREACH) -- every few minutes, per mode, forever, each an
// "[t]auth error ... http request attempts exhausted" that named no cause.
// The dial seams here return what such a host's socket calls return.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// noRouteDialError is what a stream dial of a family the routing has no route
// for returns at once: connect refused with no route to host.
func noRouteDialError(network string) error {
	return &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
}

// refusedDialError is a dial that reached the host and found no listener: the
// family has a route.
func refusedDialError(network string) error {
	return &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// noRoutePacketConn is a real udp endpoint whose sends the routing refuses
// with no route, as sendmsg answers on a host without a route of the family.
// Reads, deadlines and close are the endpoint's own.
type noRoutePacketConn struct {
	net.PacketConn
}

func (self *noRoutePacketConn) WriteTo(packet []byte, addr net.Addr) (int, error) {
	return 0, &net.OpError{Op: "write", Net: "udp6", Addr: addr, Err: os.NewSyscallError("sendmsg", syscall.EHOSTUNREACH)}
}

// pinnedRouteFixtureHold is one no-route hold the hold watcher armed a timer
// for. Sending on end runs the hold out.
type pinnedRouteFixtureHold struct {
	hold time.Duration
	end  chan time.Time
}

// pinnedRouteFixture is a v6-pinned transport whose dials meet `refusal`
// until the test routes them to its v6 loopback platform. The test decides
// when a no-route hold runs out, and sees each hold armed, each park of a
// mode runner on a hold, and each wait of a runner's redial pacing.
type pinnedRouteFixture struct {
	transport *PlatformTransport
	platform  *testingFamilyPlatformServer
	routed    atomic.Bool
	dialCount atomic.Int64
	holds     chan pinnedRouteFixtureHold
	parks     chan struct{}
	pacings   chan time.Duration
}

func newPinnedRouteFixture(
	t *testing.T,
	ctx context.Context,
	targetMode TransportMode,
	refusal func(network string) error,
	adjust func(settings *PlatformTransportSettings),
) *pinnedRouteFixture {
	t.Helper()
	fixture := &pinnedRouteFixture{
		platform: newTestingFamilyPlatformServer(t, 6, false),
		holds:    make(chan pinnedRouteFixtureHold, 16),
		parks:    make(chan struct{}, 16),
		pacings:  make(chan time.Duration, 64),
	}
	clientSettings := newTestingFamilyStrategySettings(t)
	// one round of dialer attempts per dial: the strategy waits out its own
	// reconnect timeout before another round, past this request budget
	clientSettings.RequestTimeout = 200 * time.Millisecond
	platformAddr := testLoopbackHostPort(6, fixture.platform.port)
	clientSettings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			fixture.dialCount.Add(1)
			if fixture.routed.Load() {
				return (&net.Dialer{}).DialContext(ctx, network, platformAddr)
			}
			return nil, refusal(network)
		},
	}
	settings := testingFamilyTransportSettings()
	settings.PinnedNoRouteHoldTimeout = time.Hour
	settings.PinnedNoRouteHoldMaxTimeout = 4 * time.Hour
	settings.routeHoldAfterForTest = func(hold time.Duration) <-chan time.Time {
		end := make(chan time.Time, 1)
		fixture.holds <- pinnedRouteFixtureHold{hold: hold, end: end}
		return end
	}
	settings.heldParkForTest = func() {
		select {
		case fixture.parks <- struct{}{}:
		default:
		}
	}
	settings.pacingAfterForTest = func(delay time.Duration) <-chan time.Time {
		select {
		case fixture.pacings <- delay:
		default:
		}
		elapsed := make(chan time.Time, 1)
		elapsed <- time.Now()
		return elapsed
	}
	if adjust != nil {
		adjust(settings)
	}
	settings.IpFamily = 6
	strategy := NewDirectClientStrategy(ctx, clientSettings, 6)
	t.Cleanup(strategy.Close)
	fixture.transport = NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "family-route"),
		fixture.platform.dualStackURL(),
		testingFamilyAuth(),
		targetMode,
		settings,
	)
	t.Cleanup(fixture.transport.Close)
	return fixture
}

// requireSleep waits for the hold that a refused dial starts and for the mode
// runner to park on it. With redialFirst the runner first waits out its
// redial pacing once, as a dial after a hold or a network change does; any
// other pacing before the park is a redial of a family that has no route.
func (self *pinnedRouteFixture) requireSleep(t *testing.T, redialFirst bool) pinnedRouteFixtureHold {
	t.Helper()
	var hold pinnedRouteFixtureHold
	armed := false
	parked := false
	timeout := time.After(15 * time.Second)
	for !armed || !parked {
		select {
		case hold = <-self.holds:
			armed = true
		case <-self.parks:
			parked = true
		case delay := <-self.pacings:
			if !redialFirst || armed || parked {
				t.Fatalf("the transport paced a redial in %s of a family that has no route, instead of sleeping", delay)
			}
			redialFirst = false
		case <-timeout:
			t.Fatalf("no sleep after a dial that met no route: state %s, dials %d", self.transport.State(), self.dialCount.Load())
		}
	}
	if state := self.transport.State(); state != PlatformTransportStateSleeping {
		t.Fatalf("state = %s while parked on a no-route hold, want sleeping", state)
	}
	return hold
}

// A dial of a family that has no route puts the pinned transport to sleep at
// once, with no redial until the hold runs out; a refusal after the hold
// doubles the next one. Before the hold existed the transport redialed on its
// pinned backoff forever (the field case above).
func TestFamilyPinnedTransportSleepsWhenItsFamilyHasNoRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := newPinnedRouteFixture(t, ctx, TransportModeH1, noRouteDialError, nil)

	first := fixture.requireSleep(t, false)
	if first.hold != time.Hour {
		t.Fatalf("first hold = %s, want the %s base", first.hold, time.Hour)
	}
	// the runner is parked on the hold: nothing can dial until it changes
	roundDialCount := fixture.dialCount.Load()
	if roundDialCount == 0 {
		t.Fatal("slept before any dial")
	}
	if n := fixture.platform.connectCount.Load(); n != 0 {
		t.Fatalf("a refused family reached the platform %d times", n)
	}
	if n := fixture.dialCount.Load(); n != roundDialCount {
		t.Fatalf("dialed %d times while asleep", n-roundDialCount)
	}

	// the hold runs out: one more dial (requireSleep refuses a second redial
	// before the park), refused again, and a hold twice as long
	first.end <- time.Now()
	second := fixture.requireSleep(t, true)
	if second.hold != 2*time.Hour {
		t.Fatalf("second hold = %s, want %s", second.hold, 2*time.Hour)
	}
	if n := fixture.dialCount.Load(); n <= roundDialCount {
		t.Fatalf("no dial after the hold ran out: %d dials, %d before", n, roundDialCount)
	}
}

// A network change ends a no-route hold at once and forgets its doubling: the
// refusals were the old path's.
func TestFamilyPinnedTransportNoRouteHoldEndsOnNetworkChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := newPinnedRouteFixture(t, ctx, TransportModeH1, noRouteDialError, nil)

	first := fixture.requireSleep(t, false)
	first.end <- time.Now()
	second := fixture.requireSleep(t, true)
	if second.hold != 2*time.Hour {
		t.Fatalf("second hold = %s, want %s", second.hold, 2*time.Hour)
	}

	// the new path is still unroutable: the next refusal holds at the base,
	// without waiting out the doubled hold
	NetworkChanged()
	third := fixture.requireSleep(t, true)
	if third.hold != time.Hour {
		t.Fatalf("hold after a network change = %s, want the %s base", third.hold, time.Hour)
	}
}

// When the route of the family returns, the dial after the hold connects, and
// the connection forgets the strikes.
func TestFamilyPinnedTransportWakesWhenItsRouteReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := newPinnedRouteFixture(t, ctx, TransportModeH1, noRouteDialError, nil)

	first := fixture.requireSleep(t, false)
	fixture.routed.Store(true)
	first.end <- time.Now()

	connection := receiveFamilyConnection(t, fixture.platform, 15*time.Second)
	if connection.remoteFamily != 6 {
		t.Fatalf("connected over ipv%d, want ipv6", connection.remoteFamily)
	}
	if !waitForTransportState(fixture.transport, PlatformTransportStateConnected, 15*time.Second) {
		t.Fatalf("state = %s, want connected", fixture.transport.State())
	}
	if state := fixture.transport.routeHold.value(); state.strikes != 0 || state.hold != 0 {
		t.Fatalf("no-route hold after a connection = %+v, want none", state)
	}
}

// A dial that reached the host and was refused proves a route: the transport
// keeps its pinned backoff and never sleeps. This is the shape of a working
// family whose platform port is closed, and of every family before this hold.
func TestFamilyPinnedTransportRefusedPortIsNoRouteHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := newPinnedRouteFixture(t, ctx, TransportModeH1, refusedDialError, nil)

	// two redials after refused dials, with no hold armed between
	for range 2 {
		select {
		case <-fixture.pacings:
		case hold := <-fixture.holds:
			t.Fatalf("a refused port started a %s no-route hold", hold.hold)
		case <-time.After(15 * time.Second):
			t.Fatalf("no redial after a refused port: state %s", fixture.transport.State())
		}
		if state := fixture.transport.State(); state != PlatformTransportStateConnecting {
			t.Fatalf("state = %s after a refused port, want connecting", state)
		}
	}
	select {
	case hold := <-fixture.holds:
		t.Fatalf("a refused port started a %s no-route hold", hold.hold)
	default:
	}
}

// The h3 modes meet no route on their first send, from the udp socket rather
// than a connect, and sleep the same way.
func TestFamilyPinnedH3TransportSleepsWhenItsFamilyHasNoRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := newPinnedRouteFixture(t, ctx, TransportModeH3, noRouteDialError, func(settings *PlatformTransportSettings) {
		settings.H3Port = 4433
		settings.resolveH3AddrsForTest = testingH3LoopbackResolver(6, settings.H3Port)
		settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
			packetConn, err := net.ListenPacket("udp6", "[::1]:0")
			if err != nil {
				return nil, err
			}
			return &noRoutePacketConn{PacketConn: packetConn}, nil
		}
	})

	first := fixture.requireSleep(t, false)
	if first.hold != time.Hour {
		t.Fatalf("first hold = %s, want the %s base", first.hold, time.Hour)
	}
}

// The "[t]auth error" line names the transport's mode and pinned family and the
// causes the exhausted strategy retained. It used to read only "http request
// attempts exhausted", which left a field log unable to say which transport
// failed, or that nothing ever left the host.
func TestPlatformTransportAuthErrorLogNamesItsCauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &captureLogger{enabled: true}
	fixture := newPinnedRouteFixture(t, ctx, TransportModeH1, noRouteDialError, func(settings *PlatformTransportSettings) {
		settings.Log = log
	})
	fixture.requireSleep(t, false)

	// throttled at INFO across the process, every attempt at V(1)
	lines := captureLoggerLinesWith(log, "[t]auth error")
	if len(lines) == 0 {
		t.Fatal("no auth error line for the refused dial")
	}
	for _, line := range lines {
		for _, expected := range []string{
			"(h1 ipv6)",
			(&HttpRequestExhaustedError{}).Error() + ": ",
			syscall.EHOSTUNREACH.Error(),
		} {
			if !strings.Contains(line, expected) {
				t.Errorf("auth error line %q does not name %q", line, expected)
			}
		}
	}
}

// Every pin held by a no-route hold releases the standby at once, as every pin
// held without an address of its family does, instead of after StandbyDelay:
// the group re-evaluates when a pin's hold changes.
func TestFamilyPlatformTransportGroupReleasesStandbyWhenPinsHaveNoRoute(t *testing.T) {
	clientSettings := newTestingFamilyStrategySettings(t)
	clientSettings.RequestTimeout = 200 * time.Millisecond
	clientSettings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			return nil, noRouteDialError(network)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingFamilyPlatformServer(t, 4, false)
	group := newTestingFamilyGroupWithLongDelay(t, ctx, clientSettings,
		platform.dualStackURL(), platform.dualStackURL(), platform.dualStackURL())
	if !waitForCondition(10*time.Second, func() bool {
		return group.Status().StandbyActive
	}) {
		t.Fatalf("standby not released with every pin asleep on no route: %+v", group.Status())
	}
	status := group.Status()
	if status.Ipv4 != PlatformTransportStateSleeping || status.Ipv6 != PlatformTransportStateSleeping {
		t.Fatalf("pin status with no route for either family = %+v", status)
	}
}

// The verdict is no route on every attempt, of either kind, and nothing else.
// The deadline or cancellation that closes an exhausted budget counts neither
// way.
func TestDialRouteRefused(t *testing.T) {
	exhausted := func(errs ...error) error {
		causes := newHttpRequestCauses(context.Background())
		for _, err := range errs {
			causes.record(err)
		}
		return causes.exhausted(context.Background(), nil)
	}
	attemptUrl := "ws://platform.space.example:443"
	noRoute := &url.Error{Op: "Get", URL: attemptUrl, Err: noRouteDialError("tcp6")}
	noNetwork := &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	noRouteSend := &net.OpError{Op: "write", Net: "udp6", Err: os.NewSyscallError("sendmsg", syscall.EHOSTUNREACH)}
	refused := &url.Error{Op: "Get", URL: attemptUrl, Err: refusedDialError("tcp6")}
	timedOut := &net.OpError{Op: "dial", Net: "tcp6", Err: os.ErrDeadlineExceeded}
	notFound := &net.DNSError{Err: "no such host", Name: "platform.space.example", IsNotFound: true}

	for _, c := range []struct {
		name    string
		err     error
		refused bool
	}{
		{name: "no error", err: nil, refused: false},
		{name: "no route to host", err: noRouteDialError("tcp6"), refused: true},
		{name: "no route to network", err: noNetwork, refused: true},
		{name: "every address of an attempt", err: errors.Join(noRouteDialError("tcp6"), noNetwork), refused: true},
		{name: "exhausted with no route on every attempt", err: exhausted(noRoute, noRoute, noRoute), refused: true},
		{name: "h3 send", err: fmt.Errorf("h3 handshake failed: %w", noRouteSend), refused: true},
		{name: "exhausted with one refused port", err: exhausted(noRoute, refused), refused: false},
		{name: "one address refused", err: errors.Join(noRouteDialError("tcp6"), refusedDialError("tcp6")), refused: false},
		{name: "exhausted with no attempt", err: exhausted(), refused: false},
		{name: "canceled", err: context.Canceled, refused: false},
		{name: "refused port", err: refusedDialError("tcp6"), refused: false},
		{name: "timed out", err: timedOut, refused: false},
		{name: "not found", err: notFound, refused: false},
		{name: "tls", err: errors.New("tls: handshake failure"), refused: false},
	} {
		if refused := dialRouteRefused(c.err); refused != c.refused {
			t.Errorf("%s: dialRouteRefused = %t, want %t", c.name, refused, c.refused)
		}
	}
}

// A hold doubles per refusal after the one before ran out, up to the cap; a
// refusal while a hold is in force adds no strike; a reset forgets the strikes;
// and a timer armed for an ended hold cannot end the next one.
func TestPinnedRouteHoldDoublesToItsCap(t *testing.T) {
	hold := newPinnedRouteHold(5*time.Minute, time.Hour)
	for _, expected := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour,
	} {
		started := hold.refuse()
		if started != expected {
			t.Fatalf("hold = %s, want %s", started, expected)
		}
		if again := hold.refuse(); again != 0 {
			t.Fatalf("a refusal during a %s hold started another of %s", started, again)
		}
		hold.end(hold.value().generation)
	}

	hold.reset()
	if started := hold.refuse(); started != 5*time.Minute {
		t.Fatalf("hold after a reset = %s, want the base", started)
	}
	stale := hold.value().generation
	hold.reset()
	hold.refuse()
	hold.end(stale)
	if state := hold.value(); state.hold != 5*time.Minute {
		t.Fatalf("a stale timer ended the hold in force: %+v", state)
	}

	// a cap at or below the base does not grow; a non-positive base holds
	// nothing; a family-agnostic transport has no hold at all
	flat := newPinnedRouteHold(time.Hour, time.Minute)
	for range 3 {
		if started := flat.refuse(); started != time.Hour {
			t.Fatalf("hold under a cap below its base = %s, want %s", started, time.Hour)
		}
		flat.end(flat.value().generation)
	}
	if started := newPinnedRouteHold(0, time.Hour).refuse(); started != 0 {
		t.Fatalf("a disabled hold started %s", started)
	}
	var none *pinnedRouteHold
	if started := none.refuse(); started != 0 {
		t.Fatalf("a nil hold started %s", started)
	}
	none.reset()
	none.end(0)
}
