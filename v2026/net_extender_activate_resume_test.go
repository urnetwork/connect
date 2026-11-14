package connect

import (
	"testing"
	"time"
)

// The activator after the host resumes from a sleep (hostResumeWatch).
//
// Its address check (an hour) and its re-activation (a day) waited on
// monotonic timers, which stop while the host sleeps, so after a night's sleep
// on the same network both came due only once that much awake time had
// passed: a host whose public address changed in the night kept a record for
// the old one for up to an hour awake, and one that slept through the day
// re-activated up to a day of awake time late. These tests run the activator
// on the model of the host clock (net_extender_sample_clock_test.go), whose
// sleep moves the wall reading alone, and fire its resume checks through its
// wait seam, so no test sleeps.

// An activator fixture on the host clock, with the hour address check of
// production. Its waits for a pass never end; every resume check it arms is
// handed to the returned channel, unbuffered, so a fired check is one the loop
// has taken.
func newTestActivatorResumeFixture(
	t *testing.T,
	hostClock *testHostClock,
) (*testActivatorFixture, <-chan chan time.Time) {
	t.Helper()
	armedChecks := make(chan chan time.Time)
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.Now = hostClock.Now
		settings.AddressCheckTimeout = time.Hour
		settings.PassAfter = func(wait time.Duration) <-chan time.Time {
			if wait != settings.ResumeCheckTimeout {
				return nil
			}
			check := make(chan time.Time)
			select {
			case armedChecks <- check:
			case <-t.Context().Done():
			}
			return check
		}
	})
	return fixture, armedChecks
}

// Fires the check the loop waits on and returns the one it arms next, which it
// does once it has acted on the check. A wake of its own -- a directory change
// from its own activation -- can have moved the loop on to a newer check; that
// one is fired instead.
func fireTestActivatorCheck(
	t *testing.T,
	check chan time.Time,
	armedChecks <-chan chan time.Time,
) chan time.Time {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case check <- time.Time{}:
			return nextTestResumeCheck(t, armedChecks, "the check")
		case check = <-armedChecks:
		case <-timeout:
			t.Fatal("the activator took no resume check")
			return nil
		}
	}
}

// Fails when the activator checked its address or posted an activation since
// the last one read.
func assertNoTestActivatorPass(t *testing.T, fixture *testActivatorFixture, what string) {
	t.Helper()
	select {
	case <-fixture.operator.hellos:
		t.Fatalf("%s: the activator checked its address", what)
	case <-fixture.operator.posts:
		t.Fatalf("%s: the activator posted an activation", what)
	default:
	}
}

// The first pass: an address check and an activation of each family. Returns
// the check the loop then waits on.
func waitForTestActivatorFirstPass(
	t *testing.T,
	fixture *testActivatorFixture,
	armedChecks <-chan chan time.Time,
) chan time.Time {
	t.Helper()
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
	return nextTestResumeCheck(t, armedChecks, "the first pass")
}

// The bug: after two hours asleep the address check, due an hour after the
// last, waited out the rest of its hour in awake time. It comes due at the
// resume now, once, and the re-activation, a day out, does not.
func TestExtenderActivatorChecksTheAddressAfterTheHostResumes(t *testing.T) {
	hostClock := newTestHostClock(t)
	fixture, armedChecks := newTestActivatorResumeFixture(t, hostClock)
	check := waitForTestActivatorFirstPass(t, fixture, armedChecks)

	hostClock.awake(30 * time.Minute)
	check = fireTestActivatorCheck(t, check, armedChecks)
	assertNoTestActivatorPass(t, fixture, "half an hour awake")

	// past the hour by the wall clock; the check that sees the sleep waits for
	// the host to stay awake
	hostClock.sleep(2 * time.Hour)
	check = fireTestActivatorCheck(t, check, armedChecks)
	assertNoTestActivatorPass(t, fixture, "at the check that saw the sleep")

	hostClock.awake(time.Minute)
	check = fireTestActivatorCheck(t, check, armedChecks)
	select {
	case <-fixture.operator.hellos:
	default:
		t.Fatal("the resume did not bring the address check due")
	}
	assertNoTestActivatorPass(t, fixture, "after the resume")

	// once per resume
	for range 3 {
		hostClock.awake(time.Minute)
		check = fireTestActivatorCheck(t, check, armedChecks)
	}
	assertNoTestActivatorPass(t, fixture, "the checks after the resume")
}

// A sleep past the day re-activates every family at the resume, where the
// re-activation waited out the rest of its day in awake time.
func TestExtenderActivatorReactivatesAfterASleepPastTheDay(t *testing.T) {
	hostClock := newTestHostClock(t)
	fixture, armedChecks := newTestActivatorResumeFixture(t, hostClock)
	check := waitForTestActivatorFirstPass(t, fixture, armedChecks)

	hostClock.awake(time.Minute)
	hostClock.sleep(25 * time.Hour)
	check = fireTestActivatorCheck(t, check, armedChecks)
	assertNoTestActivatorPass(t, fixture, "at the check that saw the sleep")

	hostClock.awake(time.Minute)
	fireTestActivatorCheck(t, check, armedChecks)
	fixture.waitPass()
	ipVersions := map[int]bool{}
	for range 2 {
		ipVersions[fixture.waitPost().ipVersion] = true
	}
	if !ipVersions[4] || !ipVersions[6] {
		t.Fatalf("re-activated families = %v, expected 4 and 6", ipVersions)
	}
}

// No sleep, no early pass: awake time, a nap under the minimum and a wall
// clock set back a day leave both deadlines where the monotonic clock put
// them. The activator watches with the shared defaults.
func TestExtenderActivatorTellsNoResumeWithoutASleep(t *testing.T) {
	hostClock := newTestHostClock(t)
	fixture, armedChecks := newTestActivatorResumeFixture(t, hostClock)
	if settings := fixture.activator.settings; settings.ResumeMinSleep != defaultResumeMinSleep ||
		settings.ResumeCheckTimeout != defaultResumeCheckTimeout {
		t.Fatalf("resume settings = %s and %s, expected the defaults", settings.ResumeMinSleep, settings.ResumeCheckTimeout)
	}
	check := waitForTestActivatorFirstPass(t, fixture, armedChecks)

	for range 10 {
		hostClock.awake(time.Minute)
		check = fireTestActivatorCheck(t, check, armedChecks)
	}
	hostClock.sleep(defaultResumeMinSleep - time.Second)
	check = fireTestActivatorCheck(t, check, armedChecks)
	for range 2 {
		hostClock.awake(time.Minute)
		check = fireTestActivatorCheck(t, check, armedChecks)
	}
	hostClock.setBack(24 * time.Hour)
	check = fireTestActivatorCheck(t, check, armedChecks)
	for range 2 {
		hostClock.awake(time.Minute)
		check = fireTestActivatorCheck(t, check, armedChecks)
	}
	assertNoTestActivatorPass(t, fixture, "without a sleep")
}
