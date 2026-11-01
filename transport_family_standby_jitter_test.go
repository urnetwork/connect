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
// dialed it, in the same second. These tests pin the spread of the release
// over [StandbyDelay, StandbyDelay+StandbyJitter).

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

// The root cause at the group's start: the release was StandbyDelay after the
// start exactly. It is now further by the draw.
func TestFamilyStandbyDueTimeSpreadsOverStandbyJitter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = time.Hour
	const standbyJitter = time.Hour
	for _, spread := range []time.Duration{0, 20 * time.Minute, standbyJitter - 1} {
		draws := newTestingJitterDraws(spread)
		before := time.Now()
		group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
			StandbyDelay:         standbyDelay,
			StandbyJitter:        standbyJitter,
			standbyJitterForTest: draws.draw,
		})
		after := time.Now()

		dueTime := testingStandbyDueTime(group)
		if dueTime.Before(before.Add(standbyDelay+spread)) || after.Add(standbyDelay+spread).Before(dueTime) {
			t.Fatalf("standby due %s after the start, want the delay %s plus the draw %s", dueTime.Sub(before), standbyDelay, spread)
		}
		requireDrawWindows(t, draws, 1, standbyJitter)
		if group.Status().StandbyActive {
			t.Fatalf("standby released with resolvable pins: %+v", group.Status())
		}
	}
}

// The root cause after an outage: the pins lose a live platform, and the
// release is StandbyDelay plus a new draw after the loss, not the start's.
func TestFamilyStandbyDueTimeAfterPinnedDisconnectSpreadsOverStandbyJitter(t *testing.T) {
	clientSettings := newTestingFamilyStrategySettings(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = time.Hour
	const standbyJitter = time.Hour
	const spread = 40 * time.Minute
	v4Platform := newTestingFamilyPlatformServer(t, 4, false)
	v6Port := reserveTestPort(t, "tcp6", "::1")
	standbyPlatform := newTestingFamilyPlatformServer(t, 4, false)

	settings := testingFamilyTransportSettings()
	// the v4 pin re-dials its lost connection only after the test, so no pin
	// connects once the platform closes it
	settings.ReconnectJitter = time.Hour
	settings.reconnectJitterForTest = newTestingJitterDraws(30 * time.Minute).draw
	// the start's draw, then the loss's
	draws := newTestingJitterDraws(0, spread)
	strategy := NewClientStrategy(ctx, clientSettings)
	defer strategy.Close()
	group := NewFamilyPlatformTransportGroup(
		ctx,
		clientSettings,
		strategy,
		NewRouteManager(ctx, "group"),
		standbyPlatform.dualStackURL(),
		v4Platform.dualStackURL(),
		fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v6Port),
		testingFamilyAuth(),
		TransportModeH1,
		settings,
		&FamilyPlatformTransportGroupSettings{
			StandbyDelay:         standbyDelay,
			StandbyJitter:        standbyJitter,
			standbyJitterForTest: draws.draw,
		},
	)
	defer group.Close()

	// the v4 pin connects, and the group clears the release
	if !waitForCondition(15*time.Second, func() bool {
		return group.Ipv4Transport().IsConnected() && testingStandbyDueTime(group).IsZero()
	}) {
		t.Fatalf("the v4 pin never connected: %+v", group.Status())
	}
	requireDrawWindows(t, draws, 1, standbyJitter)

	// the platform restarts: it closes the pin's connection
	closeTime := time.Now()
	v4Platform.closeConns()
	if !waitForCondition(15*time.Second, func() bool {
		return !testingStandbyDueTime(group).IsZero()
	}) {
		t.Fatalf("the group never set a release after the pin lost its connection: %+v", group.Status())
	}
	readTime := time.Now()
	dueTime := testingStandbyDueTime(group)
	if dueTime.Before(closeTime.Add(standbyDelay+spread)) || readTime.Add(standbyDelay+spread).Before(dueTime) {
		t.Fatalf("standby due %s after the loss, want the delay %s plus the draw %s", dueTime.Sub(closeTime), standbyDelay, spread)
	}
	requireDrawWindows(t, draws, 2, standbyJitter)
}

// The cap holds: a draw outside the window is held to [0, StandbyJitter).
func TestFamilyStandbyJitterDrawHoldsItsWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = time.Hour
	const standbyJitter = time.Hour
	for _, c := range []struct {
		draw time.Duration
		want time.Duration
	}{
		{-standbyJitter, 0},
		{standbyJitter, standbyJitter - 1},
		{10 * standbyJitter, standbyJitter - 1},
	} {
		before := time.Now()
		group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
			StandbyDelay:         standbyDelay,
			StandbyJitter:        standbyJitter,
			standbyJitterForTest: newTestingJitterDraws(c.draw).draw,
		})
		after := time.Now()
		dueTime := testingStandbyDueTime(group)
		if dueTime.Before(before.Add(standbyDelay+c.want)) || after.Add(standbyDelay+c.want).Before(dueTime) {
			t.Fatalf("draw %s: standby due %s after the start, want %s", c.draw, dueTime.Sub(before), standbyDelay+c.want)
		}
	}

	// the production draw never reaches the cap
	for i := 0; i < 8; i += 1 {
		before := time.Now()
		group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
			StandbyDelay:  standbyDelay,
			StandbyJitter: standbyJitter,
		})
		after := time.Now()
		dueTime := testingStandbyDueTime(group)
		if dueTime.Before(before.Add(standbyDelay)) || !dueTime.Before(after.Add(standbyDelay+standbyJitter)) {
			t.Fatalf("standby due %s after the start, want in [%s, %s)", dueTime.Sub(before), standbyDelay, standbyDelay+standbyJitter)
		}
	}
}

// A non-positive StandbyJitter releases at StandbyDelay exactly.
func TestFamilyStandbyJitterNonPositiveReleasesAtStandbyDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const standbyDelay = time.Hour
	for _, standbyJitter := range []time.Duration{0, -time.Minute} {
		draws := newTestingJitterDraws(30 * time.Minute)
		before := time.Now()
		group := newStandbyJitterTestGroup(t, ctx, &FamilyPlatformTransportGroupSettings{
			StandbyDelay:         standbyDelay,
			StandbyJitter:        standbyJitter,
			standbyJitterForTest: draws.draw,
		})
		after := time.Now()
		dueTime := testingStandbyDueTime(group)
		if dueTime.Before(before.Add(standbyDelay)) || after.Add(standbyDelay).Before(dueTime) {
			t.Fatalf("StandbyJitter %s: standby due %s after the start, want %s", standbyJitter, dueTime.Sub(before), standbyDelay)
		}
		requireDrawWindows(t, draws, 0, standbyJitter)
	}
}

func TestDefaultFamilyStandbyJitter(t *testing.T) {
	settings := DefaultFamilyPlatformTransportGroupSettings()
	AssertEqual(t, settings.StandbyDelay, 15*time.Second)
	AssertEqual(t, settings.StandbyJitter, 15*time.Second)
}
