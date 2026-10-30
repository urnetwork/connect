// The H1 owner re-arms its socket deadlines only when less than their timeout
// is left, and paces its pings from its last write (server CPUOPT2.md §9).
// Real H1+ and WebSocket owners run against the synthetic in-memory peer, and
// virtual time places every boundary exactly. The socket counts each deadline
// installation that reaches it.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// The production deadline slack, which the shared liveness fixture pins to zero
// so that its boundary tests read the timeout itself.
func withLazyH1Deadlines(settings *PlatformTransportSettings) {
	defaults := DefaultPlatformTransportSettings()
	settings.ReadDeadlineSlackFraction = defaults.ReadDeadlineSlackFraction
	settings.WriteDeadlineSlackFraction = defaults.WriteDeadlineSlackFraction
}

// How far past ReadTimeout a re-armed read deadline reaches.
func h1ReadDeadlineSlack(settings *PlatformTransportSettings) time.Duration {
	return time.Duration(float64(settings.ReadTimeout) * settings.ReadDeadlineSlackFraction)
}

// How far past WriteTimeout a re-armed write deadline reaches.
func h1WriteDeadlineSlack(settings *PlatformTransportSettings) time.Duration {
	return time.Duration(float64(settings.WriteTimeout) * settings.WriteDeadlineSlackFraction)
}

// Selects the H1+ or the WebSocket carrier.
func withH1Plus(h1Plus bool) func(*PlatformTransportSettings) {
	return func(settings *PlatformTransportSettings) {
		settings.EnableH1Plus = h1Plus
	}
}

// A busy writer installs its write deadline once per slack, not once per write
// or, on WebSocket, once per frame. Every write still begins with at least
// WriteTimeout left and at most WriteTimeout plus the slack.
func TestH1LazyWriteDeadlineInstallsOncePerSlack(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newH1LivenessFixture(t, withLazyH1Deadlines, withH1Plus(h1Plus))
			settings := fixture.transport.settings
			fixture.client.resetDeadlineStats()
			// fifty payloads 10ms apart, all inside the first slack
			const payloadCount = 50
			for range payloadCount {
				fixture.sendMessage(t, h1PlusOwnedTestMessage(0x31, 64))
				synctest.Wait()
				time.Sleep(10 * time.Millisecond)
			}
			fixture.assertConnected(t)
			stats := fixture.client.deadlineStatsSnapshot()
			if fixture.payloads.Load() != payloadCount || stats.writeCount < payloadCount {
				t.Fatalf("h1+=%t: the peer read %d of %d payloads in %d socket writes", h1Plus, fixture.payloads.Load(), payloadCount, stats.writeCount)
			}
			if stats.writeDeadlineCount != 1 {
				t.Fatalf("h1+=%t: %d payloads installed the write deadline %d times, want once", h1Plus, payloadCount, stats.writeDeadlineCount)
			}
			if stats.writesWithoutAnyDeadline != 0 ||
				stats.leastWriteDeadlineLeft < settings.WriteTimeout ||
				settings.WriteTimeout+h1WriteDeadlineSlack(settings) < stats.mostWriteDeadlineLeft {
				t.Fatalf("h1+=%t: writes began with %s to %s left (%d without a deadline), want %s to %s",
					h1Plus, stats.leastWriteDeadlineLeft, stats.mostWriteDeadlineLeft, stats.writesWithoutAnyDeadline,
					settings.WriteTimeout, settings.WriteTimeout+h1WriteDeadlineSlack(settings))
			}
		})
	}
}

// A reader that keeps receiving frames, server pings included, installs its
// read deadline at most once per slack instead of before every read.
func TestH1LazyReadDeadlineInstallsOncePerSlack(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newH1LivenessFixture(t, withLazyH1Deadlines, withH1Plus(h1Plus))
			fixture.client.resetDeadlineStats()
			// fifty heartbeats 10ms apart, all inside the first slack
			const heartbeatCount = 50
			for range heartbeatCount {
				if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				time.Sleep(10 * time.Millisecond)
			}
			fixture.assertConnected(t)
			if readDeadlineCount := fixture.client.deadlineStatsSnapshot().readDeadlineCount; 1 < readDeadlineCount {
				t.Fatalf("h1+=%t: %d inbound frames installed the read deadline %d times, want at most once", h1Plus, heartbeatCount, readDeadlineCount)
			}
		})
	}
}

// A silent peer is cut off between ReadTimeout and ReadTimeout plus the slack
// after its last frame, never before. With the fixture's 3s ReadTimeout and 1s
// slack, the last frame lands before, at and after the point where it re-arms
// the deadline armed at the connection's start.
func TestH1LazyReadDeadlineCutsOffWithinSlackOfLastFrame(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, lastFrameAfter := range []time.Duration{0, time.Second / 2, time.Second, 3 * time.Second / 2} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newH1LivenessFixture(t, withLazyH1Deadlines)
			settings := fixture.transport.settings
			readTimeout := settings.ReadTimeout
			slack := h1ReadDeadlineSlack(settings)
			if 0 < lastFrameAfter {
				time.Sleep(lastFrameAfter)
				if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
			}
			lastFrame := time.Now()
			time.Sleep(readTimeout - time.Nanosecond)
			synctest.Wait()
			fixture.assertConnected(t)
			time.Sleep(time.Until(lastFrame.Add(readTimeout + slack)))
			synctest.Wait()
			fixture.assertWithdrawn(t)
			if fixture.client.writeTimedOut.Load() {
				t.Fatal("a writer timeout ended the silent connection")
			}
		})
	}
}

// A write that stalls fails between WriteTimeout and WriteTimeout plus the slack
// after it began, never before. With the fixture's 2s WriteTimeout and 1s
// slack, it begins right after the deadline was armed, with exactly
// WriteTimeout left on it, and just after and well after a re-arm was due.
func TestH1LazyWriteStallFailsWithinSlackOfItsStart(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, stallAfter := range []time.Duration{0, time.Second, time.Second + time.Nanosecond, 3 * time.Second / 2} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newH1LivenessFixtureWithBackpressure(t, true, 0, withLazyH1Deadlines, func(settings *PlatformTransportSettings) {
				// the reader's deadline and the pings stay out of the way: the
				// stalled write is the payload's
				settings.ReadTimeout = time.Minute
				settings.PingTimeout = time.Minute
			})
			settings := fixture.transport.settings
			// one written payload arms the deadline and parks the peer reader
			stopH1BackpressurePeerReads(t, fixture)
			time.Sleep(stallAfter)
			witness := blockH1BackpressurePayload(t, fixture)
			stallStart := time.Now()
			time.Sleep(settings.WriteTimeout - time.Nanosecond)
			synctest.Wait()
			fixture.assertConnected(t)
			time.Sleep(time.Until(stallStart.Add(settings.WriteTimeout + h1WriteDeadlineSlack(settings))))
			synctest.Wait()
			fixture.assertWithdrawn(t)
			if !fixture.client.writeTimedOut.Load() {
				t.Fatal("the stalled write did not end at its write deadline")
			}
			if !MessagePoolReturn(witness) {
				t.Fatal("the stalled write retained its payload")
			}
		})
	}
}

// Lazy arming installs no write deadline while enough is left, so a rejected
// installation surfaces at the next re-arm, and is terminal there.
func TestH1LazyWriteDeadlineRejectionAtRearmIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t, withLazyH1Deadlines, func(settings *PlatformTransportSettings) {
			// only the payloads write
			settings.PingTimeout = time.Minute
		})
		settings := fixture.transport.settings
		fixture.sendMessage(t, h1PlusOwnedTestMessage(0x31, 64))
		synctest.Wait()
		fixture.client.failWriteDeadline.Store(true)
		// the deadline armed for the first payload still leaves WriteTimeout
		fixture.sendMessage(t, h1PlusOwnedTestMessage(0x32, 64))
		synctest.Wait()
		fixture.assertConnected(t)
		if fixture.client.writeRejected.Load() || fixture.payloads.Load() != 2 {
			t.Fatal("a write re-armed its deadline while the armed one left WriteTimeout")
		}
		// less than WriteTimeout left: the next write re-arms
		time.Sleep(h1WriteDeadlineSlack(settings) + time.Nanosecond)
		message := h1PlusOwnedTestMessage(0x33, 64)
		witness := MessagePoolShareReadOnly(message)
		fixture.sendMessage(t, message)
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !fixture.client.writeRejected.Load() || fixture.client.writesAfterRejection.Load() != 0 {
			t.Error("the payload bypassed its rejected write deadline")
		}
		if !MessagePoolReturn(witness) {
			t.Error("the rejected re-arm retained the payload")
		}
	})
}

// The same for the reader: a frame that arrives while the armed read deadline
// leaves ReadTimeout installs nothing; once less is left, the re-arm runs and
// its rejection is terminal before another socket read begins.
func TestH1LazyReadDeadlineRejectionAtRearmIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t, withLazyH1Deadlines)
		settings := fixture.transport.settings
		fixture.client.failReadDeadline.Store(true)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		fixture.assertConnected(t)
		if fixture.client.readRejected.Load() {
			t.Fatal("a read re-armed its deadline while the armed one left ReadTimeout")
		}
		time.Sleep(h1ReadDeadlineSlack(settings) + time.Nanosecond)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if !fixture.client.readRejected.Load() {
			t.Fatal("the reader did not re-arm once less than ReadTimeout was left")
		}
		fixture.assertWithdrawn(t)
		if fixture.client.readsAfterRejection.Load() != 0 {
			t.Error("the reader began socket I/O after rejecting its deadline")
		}
	})
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

// The re-arm rule: none armed, expired, or less than the timeout left re-arms
// to the timeout plus the slack; otherwise the armed deadline stays. A zero or
// negative slack re-arms exactly the timeout before every operation.
func TestLazyDeadlineRearmsOnlyWithLessThanTimeoutLeft(t *testing.T) {
	const timeout = 10 * time.Second
	now := time.Now()
	deadline := newLazyDeadline(timeout, 1.0/2)
	expect := func(at time.Time, wantRearm bool, want time.Time) {
		t.Helper()
		got, rearm := deadline.rearm(at)
		if rearm != wantRearm || (rearm && !got.Equal(want)) {
			t.Fatalf("at %s: rearm=%t deadline=%s, want rearm=%t deadline=%s", at.Sub(now), rearm, got, wantRearm, want)
		}
	}
	expect(now, true, now.Add(15*time.Second))
	// exactly the timeout left keeps it
	expect(now.Add(5*time.Second), false, time.Time{})
	expect(now.Add(5*time.Second+1), true, now.Add(20*time.Second+1))
	// expired re-arms
	expect(now.Add(time.Minute), true, now.Add(time.Minute+15*time.Second))

	for _, slackFraction := range []float64{0, -1} {
		deadline = newLazyDeadline(timeout, slackFraction)
		expect(now, true, now.Add(timeout))
		expect(now, true, now.Add(timeout))
		expect(now.Add(time.Second), true, now.Add(time.Second+timeout))
	}
}
