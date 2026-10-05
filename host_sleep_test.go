package connect

import (
	"testing"
	"time"
)

// The shared sleep detection (host_sleep.go), on the model of the host clock
// of net_extender_sample_clock_test.go, whose sleep moves the wall reading
// alone.

// The time slept is the wall clock's lead over the monotonic one: none across
// awake time, all of a sleep, a set back as a negative, and none at all for
// readings with no monotonic part.
func TestHostSleptIsTheWallClocksLead(t *testing.T) {
	cases := []struct {
		name    string
		awake   time.Duration
		sleep   time.Duration
		setBack time.Duration
		slept   time.Duration
	}{
		{name: "awake", awake: 8 * time.Hour, slept: 0},
		{name: "a sleep", awake: time.Minute, sleep: 8 * time.Hour, slept: 8 * time.Hour},
		{name: "a wall clock set back", awake: time.Minute, setBack: time.Hour, slept: -time.Hour},
		{name: "a sleep and a set back", sleep: 2 * time.Hour, setBack: time.Hour, slept: time.Hour},
	}
	for _, c := range cases {
		hostClock := newTestHostClock(t)
		start := hostClock.Now()
		hostClock.awake(c.awake)
		hostClock.sleep(c.sleep)
		hostClock.setBack(c.setBack)
		if slept := hostSlept(hostClock.Now(), start); slept != c.slept {
			t.Errorf("%s: slept = %s, expected %s", c.name, slept, c.slept)
		}
	}

	clock := newTestClock()
	start := clock.Now()
	clock.advance(8 * time.Hour)
	if slept := hostSlept(clock.Now(), start); slept != 0 {
		t.Fatalf("wall readings alone slept %s", slept)
	}
}

// The defaults are the ones argued at defaultResumeMinSleep, and the loops
// that watch for a resume take them.
func TestHostResumeDefaults(t *testing.T) {
	if defaultResumeMinSleep != 15*time.Minute || defaultResumeCheckTimeout != time.Minute {
		t.Fatalf("defaults = %s and %s, expected 15m and 1m", defaultResumeMinSleep, defaultResumeCheckTimeout)
	}
	networkClientSettings := DefaultExtenderNetworkClientSettings()
	if networkClientSettings.ResumeMinSleep != defaultResumeMinSleep ||
		networkClientSettings.ResumeCheckTimeout != defaultResumeCheckTimeout {
		t.Fatalf(
			"network client = %s and %s, expected the defaults",
			networkClientSettings.ResumeMinSleep,
			networkClientSettings.ResumeCheckTimeout,
		)
	}
}

// The wall clock is set forward by `d`, and the monotonic part stays. To the
// readings this is a sleep of `d`, and that is the point: nothing tells them
// apart.
func (self *testHostClock) setForward(d time.Duration) {
	self.sleep(d)
}

// One step of a resume watch case: the host clock moves, then the loop checks.
type testResumeWatchStep struct {
	awake      time.Duration
	sleep      time.Duration
	setBack    time.Duration
	setForward time.Duration
	// what the check after the moves tells
	resumed bool
	slept   time.Duration
}

// What the watch tells at each check, with the defaults: a sleep of at least
// fifteen minutes, and a minute awake after it.
func TestHostResumeWatchTellsAResumeFromTheHostClock(t *testing.T) {
	minSleep := defaultResumeMinSleep
	awakeTimeout := defaultResumeCheckTimeout
	cases := []struct {
		name  string
		steps []testResumeWatchStep
	}{
		{
			name: "awake",
			steps: []testResumeWatchStep{
				{awake: time.Minute},
				{awake: time.Minute},
				{awake: time.Hour},
				{awake: 24 * time.Hour},
			},
		},
		{
			// told once the host has been awake a minute since the check
			// that saw the sleep, and once only
			name: "a night's sleep",
			steps: []testResumeWatchStep{
				{awake: time.Minute},
				{sleep: 8 * time.Hour},
				{awake: 30 * time.Second},
				{awake: 30 * time.Second, resumed: true, slept: 8 * time.Hour},
				{awake: time.Minute},
				{awake: time.Minute},
			},
		},
		{
			name: "a nap",
			steps: []testResumeWatchStep{
				{sleep: minSleep - time.Second},
				{awake: time.Minute},
				{awake: time.Minute},
			},
		},
		{
			// a host that wakes for upkeep and sleeps again tells nothing
			// until it stays awake, and then the whole time it slept
			name: "dark wakes",
			steps: []testResumeWatchStep{
				{sleep: 2 * time.Hour},
				{awake: 30 * time.Second, sleep: 2 * time.Hour},
				{awake: 30 * time.Second, sleep: 2 * time.Hour},
				{awake: 30 * time.Second},
				{awake: 30 * time.Second, resumed: true, slept: 6 * time.Hour},
			},
		},
		{
			// a nap while the host comes back neither starts the wait over
			// nor counts as awake time
			name: "a nap while waking",
			steps: []testResumeWatchStep{
				{sleep: time.Hour},
				{awake: 30 * time.Second, sleep: 5 * time.Minute},
				{awake: 30 * time.Second, resumed: true, slept: time.Hour},
			},
		},
		{
			name: "a wall clock set back",
			steps: []testResumeWatchStep{
				{setBack: 24 * time.Hour},
				{awake: time.Minute},
				{awake: time.Minute},
			},
		},
		{
			// the awake time is the monotonic clock's
			name: "a wall clock set back while waking",
			steps: []testResumeWatchStep{
				{sleep: 8 * time.Hour},
				{awake: time.Minute, setBack: 2 * time.Hour, resumed: true, slept: 8 * time.Hour},
			},
		},
		{
			// by choice: a set back hides as much sleep from its check
			name: "a sleep hidden by a wall clock set back",
			steps: []testResumeWatchStep{
				{setBack: time.Hour, sleep: 50 * time.Minute},
				{awake: time.Minute},
				{awake: time.Minute},
			},
		},
		{
			// by choice: a set forward reads as a sleep, which costs one
			// probe pass
			name: "a wall clock set forward",
			steps: []testResumeWatchStep{
				{setForward: 20 * time.Minute},
				{awake: time.Minute, resumed: true, slept: 20 * time.Minute},
			},
		},
		{
			name: "a wall clock set forward less than the minimum",
			steps: []testResumeWatchStep{
				{setForward: minSleep - time.Second},
				{awake: time.Minute},
			},
		},
	}
	for _, c := range cases {
		hostClock := newTestHostClock(t)
		resumeWatch := newHostResumeWatch(minSleep, awakeTimeout, hostClock.Now())
		if !resumeWatch.Watching() {
			t.Fatalf("%s: the watch is not watching", c.name)
		}
		for i, step := range c.steps {
			hostClock.awake(step.awake)
			hostClock.sleep(step.sleep)
			hostClock.setBack(step.setBack)
			hostClock.setForward(step.setForward)
			slept, resumed := resumeWatch.Check(hostClock.Now())
			if resumed != step.resumed || slept != step.slept {
				t.Errorf("%s: check %d told resumed = %t after %s, expected %t after %s", c.name, i, resumed, slept, step.resumed, step.slept)
			}
		}
	}

	// readings with no monotonic part, as a fake clock's, tell nothing
	clock := newTestClock()
	resumeWatch := newHostResumeWatch(minSleep, awakeTimeout, clock.Now())
	for range 3 {
		clock.advance(8 * time.Hour)
		if _, resumed := resumeWatch.Check(clock.Now()); resumed {
			t.Fatal("wall readings alone told a resume")
		}
	}

	// either setting <= 0 watches nothing
	for _, durations := range [][2]time.Duration{{0, awakeTimeout}, {minSleep, 0}} {
		hostClock := newTestHostClock(t)
		resumeWatch := newHostResumeWatch(durations[0], durations[1], hostClock.Now())
		if resumeWatch.Watching() {
			t.Fatalf("watching with %s and %s", durations[0], durations[1])
		}
		for range 3 {
			hostClock.sleep(8 * time.Hour)
			hostClock.awake(time.Hour)
			if _, resumed := resumeWatch.Check(hostClock.Now()); resumed {
				t.Fatalf("a resume told with %s and %s", durations[0], durations[1])
			}
		}
	}
}
