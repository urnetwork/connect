package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The pause detector and the busy probe across a host sleep
// (schedulerPauseElapsed).
//
// Both judged a wait by the monotonic clock alone, which runs on through a
// frozen process but stops while the host sleeps on darwin, linux and android.
// A closed lid therefore read as a timer that fired on time: no recovery hold
// and no rebase of the receive-verdict clocks at wake, and a busy probe armed
// before the sleep convicted its exit at the first expiry after it. These
// tests run on the model of the host clock of net_extender_sample_clock_test.go,
// whose sleep moves the wall reading alone.

// How long a wait lasted: its monotonic time, which shows a frozen process,
// plus the time the host slept, which only the wall clock shows.
func TestSchedulerPauseElapsedCountsAHostSleep(t *testing.T) {
	cases := []struct {
		name    string
		awake   time.Duration
		sleep   time.Duration
		setBack time.Duration
		elapsed time.Duration
	}{
		{name: "on time", awake: time.Second, elapsed: time.Second},
		{name: "a frozen process", awake: 31 * time.Second, elapsed: 31 * time.Second},
		{name: "a closed lid", awake: time.Second, sleep: 30 * time.Minute, elapsed: 30*time.Minute + time.Second},
		{name: "a wall clock set back", awake: time.Second, setBack: time.Hour, elapsed: time.Second},
		{name: "a sleep a set back hid", awake: time.Second, sleep: 30 * time.Minute, setBack: time.Hour, elapsed: time.Second},
	}
	for _, c := range cases {
		hostClock := newTestHostClock(t)
		start := hostClock.Now()
		hostClock.awake(c.awake)
		hostClock.sleep(c.sleep)
		hostClock.setBack(c.setBack)
		if elapsed := schedulerPauseElapsed(start, hostClock.Now()); elapsed != c.elapsed {
			t.Errorf("%s: elapsed = %s, expected %s", c.name, elapsed, c.elapsed)
		}
	}

	// readings with no monotonic part are judged by the wall clock, as before
	clock := newTestClock()
	start := clock.Now()
	clock.advance(5 * time.Second)
	if elapsed := schedulerPauseElapsed(start, clock.Now()); elapsed != 5*time.Second {
		t.Fatalf("wall readings: elapsed = %s, expected 5s", elapsed)
	}
}

// The bug: a closed lid was not a pause, so the first verdict passes after
// wake judged silence that spanned the sleep, with no hold and no rebase. One
// wait of the detector across a sleep opens the recovery hold and rebases the
// verdict clocks now, as a frozen process always did; jitter, a nap within the
// tolerance and a wall clock set back do not. By choice, a wall clock set
// forward past the tolerance reads as a pause.
func TestSchedulerPauseDetectorSeesAHostSleep(t *testing.T) {
	cases := []struct {
		name    string
		awake   time.Duration
		sleep   time.Duration
		setBack time.Duration
		held    bool
	}{
		{name: "on time", awake: time.Second},
		{name: "ordinary jitter", awake: 1200 * time.Millisecond},
		{name: "a frozen process", awake: 30 * time.Second, held: true},
		{name: "a closed lid", awake: time.Second, sleep: 30 * time.Minute, held: true},
		{name: "a nap within the tolerance", awake: time.Second, sleep: time.Second},
		{name: "a wall clock set back", awake: time.Second, setBack: time.Hour},
		{name: "a wall clock set forward", awake: time.Second, sleep: 10 * time.Second, held: true},
	}
	for _, c := range cases {
		mc := schedulerPauseTestParent()
		hostClock := newTestHostClock(t)
		armed := hostClock.Now()
		hostClock.awake(c.awake)
		hostClock.sleep(c.sleep)
		hostClock.setBack(c.setBack)

		before := time.Now()
		mc.observeSchedulerPause(armed, hostClock.Now())
		stale, freshSince := mc.uplinkGate(time.Now())
		if stale != c.held {
			t.Errorf("%s: receive verdicts held = %t, expected %t", c.name, stale, c.held)
		}
		if c.held && freshSince.Before(before) {
			t.Errorf("%s: the verdict clocks were not rebased: freshSince = %v", c.name, freshSince)
		}
		if !c.held && !freshSince.IsZero() {
			t.Errorf("%s: the verdict clocks were rebased with no pause: freshSince = %v", c.name, freshSince)
		}
	}
}

// Starts one busy probe on the host clock inside the caller's synctest bubble
// and waits until it waits for its ack.
func busyProbeTestHostClockProbe(
	t *testing.T,
	hostClock *testHostClock,
	budget time.Duration,
) (*multiClientChannel, <-chan busyProbeVerdict, func(error)) {
	t.Helper()
	var ackCallback func(error)
	client := busyProbeTestChannel(t, func(_ time.Duration, ack func(error)) (bool, error) {
		ackCallback = ack
		return true, nil
	})
	client.settings.SchedulerPauseTolerance = 10 * time.Millisecond
	client.busyProbeNowForTest = hostClock.Now
	verdicts := make(chan busyProbeVerdict, 1)
	go func() {
		verdicts <- client.busyLivenessProbe(budget)
	}()
	synctest.Wait()
	if ackCallback == nil || !client.busyProbeOutstandingNow() {
		t.Fatal("the probe did not arm")
	}
	return client, verdicts, ackCallback
}

// The bug's other half: a busy probe armed before the host slept convicted its
// exit at its first expiry after the sleep, though neither the exit's answer
// nor this waiter could arrive while the host was asleep. The sleep counts as
// a pause now, so the probe gets its one fresh budget, and the ack inside it
// acquits.
func TestBusyProbeRefreshesItsBudgetAfterAHostSleep(t *testing.T) {
	hostClock := newTestHostClock(t)
	synctest.Test(t, func(t *testing.T) {
		budget := 50 * time.Millisecond
		client, verdicts, ackCallback := busyProbeTestHostClockProbe(t, hostClock, budget)

		// an hour asleep while the probe waits; its timer, on the monotonic
		// clock, runs out its budget in awake time
		hostClock.awake(budget)
		hostClock.sleep(time.Hour)
		time.Sleep(budget)
		synctest.Wait()
		select {
		case verdict := <-verdicts:
			t.Fatalf("the probe convicted at its first expiry after the host slept: %+v", verdict)
		default:
		}
		if !client.busyProbeOutstandingNow() {
			t.Fatal("the refresh disarmed the unanswered probe")
		}

		ackCallback(nil)
		synctest.Wait()
		select {
		case verdict := <-verdicts:
			if verdict.convict || verdict.detail != "liveness probe answered" {
				t.Fatalf("the ack inside the refreshed budget did not acquit: %+v", verdict)
			}
		default:
			t.Fatal("the probe did not finish after its ack")
		}
	})
}

// With no sleep the same probe convicts at its first expiry: the host clock
// adds nothing to a wait the host was awake for.
func TestBusyProbeConvictsAtTheBudgetWithNoSleep(t *testing.T) {
	hostClock := newTestHostClock(t)
	synctest.Test(t, func(t *testing.T) {
		budget := 50 * time.Millisecond
		_, verdicts, _ := busyProbeTestHostClockProbe(t, hostClock, budget)

		hostClock.awake(budget)
		time.Sleep(budget)
		synctest.Wait()
		select {
		case verdict := <-verdicts:
			if !verdict.convict || verdict.detail != "liveness probe timed out after 50ms" {
				t.Fatalf("the awake probe did not convict at its budget: %+v", verdict)
			}
		default:
			t.Fatal("the awake probe refreshed its budget")
		}
	})
}
