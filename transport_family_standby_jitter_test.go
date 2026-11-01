package connect

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A platform restart or outage disconnects every provider's pinned
// transports in the same instant, and the standby used to be released a fixed
// StandbyDelay later, so every provider in the fleet released its standby, and
// dialed it, in the same second. After a pinned loss the release now waits a
// draw over [0, StandbyJitter) past StandbyDelay. A group start is one
// device's own event (an app start, a provide toggle, a policy migration) and
// keeps StandbyDelay exactly.

func testingStandbyDueTime(group *FamilyPlatformTransportGroup) time.Time {
	group.stateLock.Lock()
	defer group.stateLock.Unlock()
	return group.standbyDueTime
}

// A group whose pins cannot connect soon but do resolve, so its standby waits
// out the delay, which is far beyond the test.
func newStandbyJitterTestGroup(
	t *testing.T,
	ctx context.Context,
	groupSettings *FamilyPlatformTransportGroupSettings,
) *FamilyPlatformTransportGroup {
	t.Helper()
	clientSettings := newTestingFamilyStrategySettingsOwningHost(t)
	v4Port := reserveTestPort(t, "tcp4", "127.0.0.1")
	v6Port := reserveTestPort(t, "tcp6", "::1")
	standbyPlatform := newTestingFamilyPlatformServer(t, 4, false)
	settings := testingFamilyTransportSettings()
	strategy := NewClientStrategy(ctx, clientSettings)
	t.Cleanup(strategy.Close)
	group := NewFamilyPlatformTransportGroup(
		ctx,
		clientSettings,
		strategy,
		NewRouteManager(ctx, "group"),
		standbyPlatform.dualStackURL(),
		fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v4Port),
		fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v6Port),
		testingFamilyAuth(),
		TransportModeH1,
		settings,
		groupSettings,
	)
	t.Cleanup(group.Close)
	return group
}

// A group start draws no spread: the standby is due StandbyDelay after the
// start exactly, 15 s with the defaults.
func TestFamilyStandbyAtGroupStartIsStandbyDelayExactly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defaultSettings := DefaultFamilyPlatformTransportGroupSettings()
	for _, groupSettings := range []*FamilyPlatformTransportGroupSettings{
		defaultSettings,
		{StandbyDelay: time.Hour, StandbyJitter: 5 * time.Second},
	} {
		draws := newTestingJitterDraws(4 * time.Second)
		groupSettings.standbyJitterForTest = draws.draw
		before := time.Now()
		group := newStandbyJitterTestGroup(t, ctx, groupSettings)
		after := time.Now()

		dueTime := testingStandbyDueTime(group)
		standbyDelay := groupSettings.StandbyDelay
		if dueTime.Before(before.Add(standbyDelay)) || after.Add(standbyDelay).Before(dueTime) {
			t.Fatalf("standby due %s after the start, want the delay %s exactly", dueTime.Sub(before), standbyDelay)
		}
		requireDrawWindows(t, draws, 0, groupSettings.StandbyJitter)
		if group.Status().StandbyActive {
			t.Fatalf("standby released with resolvable pins: %+v", group.Status())
		}
	}
	AssertEqual(t, defaultSettings.StandbyDelay, 15*time.Second)
}

// How long a loss test waits for a step it expects. A step takes moments; the
// bound is far past any scheduling delay under load.
const testingStandbyLossWait = 60 * time.Second

// A group whose v4 pin dials `v4PlatformUrl` and whose v6 pin cannot connect.
func newStandbyLossTestGroup(
	t *testing.T,
	ctx context.Context,
	v4PlatformUrl string,
	standbyPlatformUrl string,
	settings *PlatformTransportSettings,
	groupSettings *FamilyPlatformTransportGroupSettings,
) *FamilyPlatformTransportGroup {
	t.Helper()
	clientSettings := newTestingFamilyStrategySettings(t)
	// a dial to a platform that is not up yet is tried again within 100 ms,
	// not at a random point of the strategy's 15 s retry window, so a pin
	// reaches a platform that appears mid-test at once
	clientSettings.ReconnectTimeout = 100 * time.Millisecond
	v6Port := reserveTestPort(t, "tcp6", "::1")
	strategy := NewClientStrategy(ctx, clientSettings)
	t.Cleanup(strategy.Close)
	group := NewFamilyPlatformTransportGroup(
		ctx,
		clientSettings,
		strategy,
		NewRouteManager(ctx, "group"),
		standbyPlatformUrl,
		v4PlatformUrl,
		fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v6Port),
		testingFamilyAuth(),
		TransportModeH1,
		settings,
		groupSettings,
	)
	t.Cleanup(group.Close)
	return group
}

// The root cause after an outage: the pins lose a live platform, and the
// release is StandbyDelay plus a draw in [0, StandbyJitter) after the loss.
func TestFamilyStandbyAfterPinnedLossSpreadsOverStandbyJitter(t *testing.T) {
	const standbyDelay = time.Hour
	const standbyJitter = 5 * time.Second
	for _, spread := range []time.Duration{0, 2 * time.Second, standbyJitter - 1} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			v4Platform := newTestingFamilyPlatformServer(t, 4, false)
			standbyPlatform := newTestingFamilyPlatformServer(t, 4, false)
			settings := testingFamilyTransportSettings()
			// the v4 pin re-dials its lost connection only after the test, so
			// no pin connects once the platform closes it
			settings.ReconnectJitter = time.Hour
			settings.reconnectJitterForTest = newTestingJitterDraws(30 * time.Minute).draw
			draws := newTestingJitterDraws(spread)
			group := newStandbyLossTestGroup(t, ctx, v4Platform.dualStackURL(), standbyPlatform.dualStackURL(), settings, &FamilyPlatformTransportGroupSettings{
				StandbyDelay:         standbyDelay,
				StandbyJitter:        standbyJitter,
				standbyJitterForTest: draws.draw,
			})

			// the v4 pin connects, and the group clears the release
			if !waitForCondition(testingStandbyLossWait, func() bool {
				return group.Ipv4Transport().IsConnected() && testingStandbyDueTime(group).IsZero()
			}) {
				t.Fatalf("the v4 pin never connected: %+v", group.Status())
			}
			requireDrawWindows(t, draws, 0, standbyJitter)

			// the platform restarts: it closes the pin's connection
			closeTime := time.Now()
			v4Platform.closeConns()
			if !waitForCondition(testingStandbyLossWait, func() bool {
				return !testingStandbyDueTime(group).IsZero()
			}) {
				t.Fatalf("the group never set a release after the pin lost its connection: %+v", group.Status())
			}
			readTime := time.Now()
			dueTime := testingStandbyDueTime(group)
			if dueTime.Before(closeTime.Add(standbyDelay+spread)) || readTime.Add(standbyDelay+spread).Before(dueTime) {
				t.Fatalf("standby due %s after the loss, want the delay %s plus the draw %s", dueTime.Sub(closeTime), standbyDelay, spread)
			}
			requireDrawWindows(t, draws, 1, standbyJitter)
		}()
	}
}

// The release end to end: a standby that a pin's connection stood down, then
// released after the pin lost the platform, dials at its release with no
// ReconnectJitter on top. Its last connection was closed by the hold, and the
// dial after the re-enable is no lost connection's re-dial
// (transport_redial_hold_test.go), so the release bound holds through the
// dial. Before that, the re-enable drew the spread too, up to 5 s more.
func TestFamilyStandbyReleaseAfterPinnedLossDialsWithoutReconnectJitter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = 300 * time.Millisecond
	const standbyJitter = time.Second
	const spread = 400 * time.Millisecond
	v4Port := reserveTestPort(t, "tcp4", "127.0.0.1")
	standbyPlatform := newTestingFamilyPlatformServer(t, 4, false)
	settings := testingFamilyTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	settings.PinnedReconnectMaxTimeout = 100 * time.Millisecond
	// a pin re-dials its lost connection only after the test
	settings.ReconnectJitter = time.Hour
	reconnectDraws := newTestingJitterDraws(30 * time.Minute)
	settings.reconnectJitterForTest = reconnectDraws.draw
	standbyDraws := newTestingJitterDraws(spread)
	group := newStandbyLossTestGroup(t, ctx, fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v4Port), standbyPlatform.dualStackURL(), settings, &FamilyPlatformTransportGroupSettings{
		StandbyDelay:         standbyDelay,
		StandbyJitter:        standbyJitter,
		standbyJitterForTest: standbyDraws.draw,
	})

	// no pin connects: the standby is released at the start's exact delay
	// and connects
	receiveFamilyConnection(t, standbyPlatform, testingStandbyLossWait)
	if !waitForCondition(testingStandbyLossWait, func() bool { return group.StandbyTransport().IsConnected() }) {
		t.Fatalf("the standby never connected: %+v", group.Status())
	}
	requireDrawWindows(t, standbyDraws, 0, standbyJitter)

	// the v4 platform appears: the pin connects and the standby stands down,
	// its connection closed by the hold
	v4Platform := newTestingFamilyPlatformServerOnPort(t, 4, v4Port, false)
	receiveFamilyConnection(t, v4Platform, testingStandbyLossWait)
	if !waitForCondition(testingStandbyLossWait, func() bool {
		status := group.Status()
		return status.Ipv4 == PlatformTransportStateConnected && !status.StandbyActive && !group.StandbyTransport().IsConnected()
	}) {
		t.Fatalf("the standby did not stand down: %+v", group.Status())
	}

	// the platform restarts: the pin loses its connection and stays down
	// through its own re-dial spread
	closeTime := time.Now()
	v4Platform.closeConns()
	if !waitForCondition(testingStandbyLossWait, func() bool { return group.StandbyTransport().IsConnected() }) {
		t.Fatalf("the standby never connected after the pin's loss: %+v", group.Status())
	}
	if elapsed := time.Since(closeTime); elapsed < standbyDelay+spread {
		t.Fatalf("standby connected %s after the loss, before the delay %s plus the draw %s", elapsed, standbyDelay, spread)
	}
	requireDrawWindows(t, standbyDraws, 1, standbyJitter)
	// the one spread is the pin's own re-dial: the standby's dial drew none
	requireDrawWindows(t, reconnectDraws, 1, settings.ReconnectJitter)
}

// The cap holds: a draw outside the window is held to [0, StandbyJitter), and
// the production draw never reaches the cap.
func TestFamilyStandbyJitterDrawHoldsItsWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = time.Hour
	const standbyJitter = 5 * time.Second
	for _, c := range []struct {
		draw time.Duration
		want time.Duration
	}{
		{-standbyJitter, 0},
		{standbyJitter, standbyJitter - 1},
		{10 * standbyJitter, standbyJitter - 1},
	} {
		group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
			StandbyDelay:         standbyDelay,
			StandbyJitter:        standbyJitter,
			standbyJitterForTest: newTestingJitterDraws(c.draw).draw,
		})
		AssertEqual(t, group.standbyDelayAfterLoss(), standbyDelay+c.want)
	}

	group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
		StandbyDelay:  standbyDelay,
		StandbyJitter: standbyJitter,
	})
	for i := 0; i < 1000; i += 1 {
		if delay := group.standbyDelayAfterLoss(); delay < standbyDelay || standbyDelay+standbyJitter <= delay {
			t.Fatalf("standby delay after a loss %s, want in [%s, %s)", delay, standbyDelay, standbyDelay+standbyJitter)
		}
	}
}

// A non-positive StandbyJitter releases at StandbyDelay exactly after a loss.
func TestFamilyStandbyJitterNonPositiveReleasesAtStandbyDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = time.Hour
	for _, standbyJitter := range []time.Duration{0, -time.Minute} {
		draws := newTestingJitterDraws(30 * time.Minute)
		group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
			StandbyDelay:         standbyDelay,
			StandbyJitter:        standbyJitter,
			standbyJitterForTest: draws.draw,
		})
		AssertEqual(t, group.standbyDelayAfterLoss(), standbyDelay)
		requireDrawWindows(t, draws, 0, standbyJitter)
	}
}

func TestDefaultFamilyStandbyJitter(t *testing.T) {
	settings := DefaultFamilyPlatformTransportGroupSettings()
	AssertEqual(t, settings.StandbyDelay, 15*time.Second)
	AssertEqual(t, settings.StandbyJitter, 5*time.Second)
}
