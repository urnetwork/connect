// Ping pacing preserves the old wire schedule while avoiding a timer reset
// after every write. Native deadlines remain responsible for I/O bounds.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Selects the H1+ or the WebSocket carrier.
func withH1Plus(h1Plus bool) func(*PlatformTransportSettings) {
	return func(settings *PlatformTransportSettings) {
		settings.EnableH1Plus = h1Plus
	}
}

// Writes only note their time, and the ping timer is re-armed when it fires
// early, so a busy writer arms it about once per ping interval instead of once
// per write. The ping still goes out at the first wakeup boundary at least
// PingTimeout after the last write, the instant a re-arm per write chose.
func TestH1PingPacedFromLastWrite(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			var pingTimerArmCount atomic.Int64
			fixture := newH1LivenessFixture(t, withH1Plus(h1Plus), func(settings *PlatformTransportSettings) {
				// the reader's deadline stays out of the way
				settings.ReadTimeout = time.Minute
				settings.pingTimerArmForTest = func() {
					pingTimerArmCount.Add(1)
				}
			})
			pingTimeout := fixture.transport.settings.PingTimeout
			pingTimerArmCount.Store(0)
			// thirty payloads 100ms apart: three ping intervals of traffic
			const payloadCount = 30
			var lastWrite time.Time
			for i := range payloadCount {
				if 0 < i {
					time.Sleep(100 * time.Millisecond)
				}
				fixture.sendMessage(t, h1PlusOwnedTestMessage(0x31, 64))
				synctest.Wait()
				lastWrite = time.Now()
			}
			if fixture.payloads.Load() != payloadCount || fixture.heartbeats.Load() != 0 {
				t.Fatalf("h1+=%t: payloads=%d heartbeats=%d during traffic, want %d and none", h1Plus, fixture.payloads.Load(), fixture.heartbeats.Load(), payloadCount)
			}
			if armCount := pingTimerArmCount.Load(); 4 < armCount {
				t.Fatalf("h1+=%t: %d writes over three ping intervals armed the ping timer %d times, want at most 4", h1Plus, payloadCount, armCount)
			}
			wantPing := WakeupTime(lastWrite.Add(pingTimeout), pingTimeout)
			time.Sleep(time.Until(wantPing) - time.Nanosecond)
			synctest.Wait()
			if fixture.heartbeats.Load() != 0 {
				t.Fatalf("h1+=%t: pinged before %s", h1Plus, wantPing)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			heartbeatTimes := fixture.heartbeatTimesSnapshot()
			if len(heartbeatTimes) != 1 || !heartbeatTimes[0].Equal(wantPing) {
				t.Fatalf("h1+=%t: pings at %v, want one at %s", h1Plus, heartbeatTimes, wantPing)
			}
			fixture.assertConnected(t)
		})
	}
}

// With PingJitter a transport pings its own offset past the wall-clock ping
// boundaries, at the same cadence. The offset is drawn below the jitter.
func TestH1PingJitterOffsetsThePingBoundary(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t, func(settings *PlatformTransportSettings) {
			// the reader's deadline stays out of the way
			settings.ReadTimeout = time.Minute
			settings.PingJitter = settings.PingTimeout
		})
		pingTimeout := fixture.transport.settings.PingTimeout
		offset := fixture.transport.pingOffset
		if offset < 0 || pingTimeout <= offset {
			t.Fatalf("ping offset %s is not below the %s jitter", offset, pingTimeout)
		}
		fixture.sendMessage(t, h1PlusOwnedTestMessage(0x31, 64))
		synctest.Wait()
		lastWrite := time.Now()
		var wantPings []time.Time
		wantPing := wakeupTimeWithOffset(lastWrite.Add(pingTimeout), pingTimeout, offset)
		for range 3 {
			wantPings = append(wantPings, wantPing)
			time.Sleep(time.Until(wantPing) - time.Nanosecond)
			synctest.Wait()
			if heartbeatCount := len(fixture.heartbeatTimesSnapshot()); heartbeatCount != len(wantPings)-1 {
				t.Fatalf("%d pings before %s, want %d", heartbeatCount, wantPing, len(wantPings)-1)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			// pings complete in no time here, so the next lands one ping
			// interval later
			wantPing = wakeupTimeWithOffset(wantPing.Add(pingTimeout), pingTimeout, offset)
		}
		heartbeatTimes := fixture.heartbeatTimesSnapshot()
		for i := range wantPings {
			if len(heartbeatTimes) != len(wantPings) || !heartbeatTimes[i].Equal(wantPings[i]) {
				t.Fatalf("pings at %v, want %v", heartbeatTimes, wantPings)
			}
		}
		fixture.assertConnected(t)
	})
}

// Phones keep the wall-clock alignment: the default draws no offset.
func TestPlatformTransportDefaultPingsOnTheWallClockBoundary(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	if settings.PingJitter != 0 {
		t.Fatalf("default ping jitter=%s, want none", settings.PingJitter)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport := NewPlatformTransportWithTargetMode(ctx, NewClientStrategyWithDefaults(ctx), NewRouteManager(ctx, "default-ping-offset"),
		"ws://ping-offset.example/endpoint", &ClientAuth{ByJwt: "synthetic-test-token", InstanceId: NewId()},
		TransportModeH1, settings)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
	if transport.pingOffset != 0 {
		t.Fatalf("default transport ping offset=%s, want 0", transport.pingOffset)
	}
}
