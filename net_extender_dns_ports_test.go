package connect

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The dns carrier's port race (EXTENDER.md L2, net_extender_dns_ports.go), in
// synctest bubbles under the default schedule (testingWhodisRaceSchedule, which
// TestExtenderDnsPortRaceDefaultSchedule holds to DefaultConnectSettings), so
// the stagger and the stall timeout pass in the bubble's time and every launch
// and end is at an exact instant. Two harnesses drive the race through its
// dial seam. The first answers a port only when the test answers it, and a
// port the test never answers is a blackhole that holds its attempt until the
// race cancels it. The second scripts each port's extender over a real memory
// budget, which the dial claims before anything else as dialExtenderQuic
// does: how often the attempt hears the extender, and when it answers.

// One attempt the race launched on one port.
type testDnsPortLaunch struct {
	dnsPort    int
	ctx        context.Context
	launchTime time.Time
	answer     chan testDnsPortAnswer
	// closed once the attempt's dial has returned
	done chan struct{}
}

// What the test answers one attempt with.
type testDnsPortAnswer struct {
	conn net.Conn
	err  error
}

// A connection that records its close.
type testDnsPortConn struct {
	net.Conn
	dnsPort int
	closed  atomic.Bool
	onClose func()
}

func (self *testDnsPortConn) Close() error {
	if !self.closed.Swap(true) && self.onClose != nil {
		self.onClose()
	}
	return nil
}

// One race under test: its launches as the test sees them, and its outcome
// once it returns.
type testDnsPortRace struct {
	t         *testing.T
	startTime time.Time
	launches  chan *testDnsPortLaunch
	roundTrip *ExtenderRoundTrip
	conn      net.Conn
	response  *protocol.ExtenderResponse
	err       error
	finished  chan struct{}
}

// Starts a race over `dnsPorts` with the test's dial, in the test's bubble.
func startTestDnsPortRace(t *testing.T, dnsPorts []int) *testDnsPortRace {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	race := &testDnsPortRace{
		t:         t,
		startTime: time.Now(),
		launches:  make(chan *testDnsPortLaunch, 16),
		roundTrip: &ExtenderRoundTrip{},
		finished:  make(chan struct{}),
	}
	dialPort := func(
		ctx context.Context,
		dnsPort int,
		roundTrip *ExtenderRoundTrip,
	) (net.Conn, *protocol.ExtenderResponse, error) {
		launch := &testDnsPortLaunch{
			dnsPort:    dnsPort,
			ctx:        ctx,
			launchTime: time.Now(),
			answer:     make(chan testDnsPortAnswer, 1),
			done:       make(chan struct{}),
		}
		defer close(launch.done)
		race.launches <- launch
		select {
		case answer := <-launch.answer:
			if answer.err != nil {
				return answer.conn, nil, answer.err
			}
			// each port marks its own round trip, so the copy the caller
			// gets says which port answered
			roundTrip.SendTime = time.Unix(int64(dnsPort), 0)
			roundTrip.ReceiveTime = time.Unix(int64(dnsPort)+1, 0)
			return answer.conn, &protocol.ExtenderResponse{}, nil
		case <-ctx.Done():
			// a blackhole: nothing answers until the race gives up on it
			return nil, nil, context.Cause(ctx)
		}
	}
	go func() {
		defer close(race.finished)
		race.conn, race.response, race.err = raceExtenderDnsPorts(
			ctx,
			dnsPorts,
			race.roundTrip,
			testingWhodisRaceSchedule,
			dialPort,
		)
	}()
	t.Cleanup(func() {
		cancel()
		<-race.finished
	})
	return race
}

// The next attempt the race launches.
func (self *testDnsPortRace) nextLaunch(dnsPort int) *testDnsPortLaunch {
	self.t.Helper()
	select {
	case launch := <-self.launches:
		if launch.dnsPort != dnsPort {
			self.t.Fatalf("launched port %d, expected %d", launch.dnsPort, dnsPort)
		}
		return launch
	case <-self.finished:
		self.t.Fatalf("the race returned before it launched port %d: %v", dnsPort, self.err)
	case <-time.After(time.Minute):
		self.t.Fatalf("the race never launched port %d", dnsPort)
	}
	return nil
}

// How long after the race started the attempt launched.
func (self *testDnsPortRace) launchedAfter(launch *testDnsPortLaunch) time.Duration {
	return launch.launchTime.Sub(self.startTime)
}

// Waits for the race to return.
func (self *testDnsPortRace) wait() {
	self.t.Helper()
	select {
	case <-self.finished:
	case <-time.After(time.Minute):
		self.t.Fatal("the race did not return")
	}
}

// Asserts the race launched nothing it was not asked to.
func (self *testDnsPortRace) assertNoLaunch() {
	self.t.Helper()
	select {
	case launch := <-self.launches:
		self.t.Fatalf("the race launched port %d", launch.dnsPort)
	default:
	}
}

// Asserts one attempt was canceled and joined before the race returned.
func assertTestDnsPortLaunchJoined(t *testing.T, launch *testDnsPortLaunch) {
	t.Helper()
	if launch.ctx.Err() == nil {
		t.Fatalf("the attempt on port %d was not canceled", launch.dnsPort)
	}
	select {
	case <-launch.done:
	default:
		t.Fatalf("the race returned before the attempt on port %d ended", launch.dnsPort)
	}
}

// An extender that answers on its first port is reached on it alone: 53 is
// launched only when 4053 has not answered within one stagger.
func TestExtenderDnsPortRaceReachesTheFirstPortWithoutTheSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		launch := race.nextLaunch(ExtenderDnsPort)
		conn := &testDnsPortConn{dnsPort: ExtenderDnsPort}
		// the extender answers just inside the stagger
		time.Sleep(testingWhodisRaceSchedule.stagger - time.Millisecond)
		launch.answer <- testDnsPortAnswer{conn: conn}
		race.wait()

		if race.err != nil || race.conn != conn {
			t.Fatalf("race = %v, %v, expected the 4053 connection", race.conn, race.err)
		}
		race.assertNoLaunch()
		if conn.closed.Load() {
			t.Fatal("the race closed the connection it returned")
		}
	})
}

// A 4053-only extender, which every app extender is, is reached without
// waiting out 53: 53 goes out one stagger after 4053 and never answers, and
// the race returns the moment 4053 does, with 53 canceled and joined.
func TestExtenderDnsPortRaceReachesA4053ExtenderWithoutWaitingOut53(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		launch4053 := race.nextLaunch(ExtenderDnsPort)
		launch53 := race.nextLaunch(DefaultDnsPort)
		if after := race.launchedAfter(launch53); after != testingWhodisRaceSchedule.stagger {
			t.Fatalf("53 launched %s after 4053, want the stagger", after)
		}

		conn := &testDnsPortConn{dnsPort: ExtenderDnsPort}
		launch4053.answer <- testDnsPortAnswer{conn: conn}
		race.wait()

		if race.err != nil || race.conn != conn {
			t.Fatalf("race = %v, %v, expected the 4053 connection", race.conn, race.err)
		}
		assertTestDnsPortLaunchJoined(t, launch53)
		// the round trip is the port that answered, not the one still out
		if race.roundTrip.SendTime != time.Unix(int64(ExtenderDnsPort), 0) {
			t.Fatalf("round trip = %+v, expected the 4053 attempt's", race.roundTrip)
		}
	})
}

// A record that lists 53 is dialed on 53 first, and where the client's network
// blackholes 53, 4053 goes out after one stagger rather than after 53's whole
// attempt.
func TestExtenderDnsPortRaceTries4053OneStaggerBehindABlackholed53(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{DefaultDnsPort, ExtenderDnsPort})
		launch53 := race.nextLaunch(DefaultDnsPort)
		launch4053 := race.nextLaunch(ExtenderDnsPort)
		if after := race.launchedAfter(launch4053); after != testingWhodisRaceSchedule.stagger {
			t.Fatalf("4053 launched %s after 53, want the stagger", after)
		}

		conn := &testDnsPortConn{dnsPort: ExtenderDnsPort}
		launch4053.answer <- testDnsPortAnswer{conn: conn}
		race.wait()

		if race.err != nil || race.conn != conn {
			t.Fatalf("race = %v, %v, expected the 4053 connection", race.conn, race.err)
		}
		assertTestDnsPortLaunchJoined(t, launch53)
	})
}

// A port that fails launches the next at once, without the stagger.
func TestExtenderDnsPortRaceLaunchesTheNextPortAtOnceAfterAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		launch4053 := race.nextLaunch(ExtenderDnsPort)
		time.Sleep(100 * time.Millisecond)
		launch4053.answer <- testDnsPortAnswer{err: errors.New("no route to the port")}
		launch53 := race.nextLaunch(DefaultDnsPort)
		if after := race.launchedAfter(launch53); after != 100*time.Millisecond {
			t.Fatalf("53 launched %s after 4053, want at 4053's failure", after)
		}

		conn := &testDnsPortConn{dnsPort: DefaultDnsPort}
		launch53.answer <- testDnsPortAnswer{conn: conn}
		race.wait()
		if race.err != nil || race.conn != conn {
			t.Fatalf("race = %v, %v, expected the 53 connection", race.conn, race.err)
		}
	})
}

// An answer from the extender ends the race: it refuses or limits the request
// on every port alike, so no other port is tried.
func TestExtenderDnsPortRaceEndsOnAnExtenderAnswer(t *testing.T) {
	answers := []error{
		&ExtenderLimitedError{RetryAfter: time.Minute},
		&ExtenderRefusedError{StatusCode: 403},
	}
	for _, answer := range answers {
		synctest.Test(t, func(t *testing.T) {
			race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
			launch := race.nextLaunch(ExtenderDnsPort)
			launch.answer <- testDnsPortAnswer{err: answer}
			race.wait()
			if race.err != answer {
				t.Fatalf("race err = %v, expected the extender's answer %v", race.err, answer)
			}
			race.assertNoLaunch()
		})
	}
}

// With every port failed the race fails with every port's error.
func TestExtenderDnsPortRaceJoinsTheErrorsOfEveryPort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		err4053 := errors.New("4053 failed")
		err53 := errors.New("53 failed")
		race.nextLaunch(ExtenderDnsPort).answer <- testDnsPortAnswer{err: err4053}
		race.nextLaunch(DefaultDnsPort).answer <- testDnsPortAnswer{err: err53}
		race.wait()
		if race.conn != nil || !errors.Is(race.err, err4053) || !errors.Is(race.err, err53) {
			t.Fatalf("race = %v, %v, expected both errors", race.conn, race.err)
		}
	})
}

// A port the memory budget refused while 4053 held the budget is not a port
// that failed: it goes again once 4053 ends.
func TestExtenderDnsPortRaceRetriesAPortTheMemoryBudgetRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		launch4053 := race.nextLaunch(ExtenderDnsPort)
		race.nextLaunch(DefaultDnsPort).answer <- testDnsPortAnswer{err: errExtenderMemoryBudget}
		launch4053.answer <- testDnsPortAnswer{err: errors.New("4053 failed")}
		// in either order the two land in, 53 goes again
		launch53 := race.nextLaunch(DefaultDnsPort)

		conn := &testDnsPortConn{dnsPort: DefaultDnsPort}
		launch53.answer <- testDnsPortAnswer{conn: conn}
		race.wait()
		if race.err != nil || race.conn != conn {
			t.Fatalf("race = %v, %v, expected the 53 connection", race.conn, race.err)
		}
	})
}

// A refusal that lands after an attempt that held the budget already ended
// goes again at once, since that attempt freed what it held.
func TestExtenderDnsPortRaceRetriesARefusalAfterAnAttemptEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort, 5353})
		launch4053 := race.nextLaunch(ExtenderDnsPort)
		launch53 := race.nextLaunch(DefaultDnsPort)
		launch4053.answer <- testDnsPortAnswer{err: errors.New("4053 failed")}
		// the failure launches the third port at once, which is how the test
		// knows the race took it before the refusal below
		race.nextLaunch(5353)
		launch53.answer <- testDnsPortAnswer{err: errExtenderMemoryBudget}
		retry53 := race.nextLaunch(DefaultDnsPort)
		if after := race.launchedAfter(retry53); after != testingWhodisRaceSchedule.stagger {
			t.Fatalf("53 went again %s after the start, want at its refusal", after)
		}

		conn := &testDnsPortConn{dnsPort: DefaultDnsPort}
		retry53.answer <- testDnsPortAnswer{conn: conn}
		race.wait()
		if race.err != nil || race.conn != conn {
			t.Fatalf("race = %v, %v, expected the 53 connection", race.conn, race.err)
		}
	})
}

// A loser that connected after the race returned is closed by its own
// attempt, so no carrier outlives the race.
func TestExtenderDnsPortRaceClosesALoserThatConnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		launch4053 := race.nextLaunch(ExtenderDnsPort)
		launch53 := race.nextLaunch(DefaultDnsPort)

		winner := &testDnsPortConn{dnsPort: ExtenderDnsPort}
		loser := &testDnsPortConn{dnsPort: DefaultDnsPort}
		// both answer; whichever the race takes first wins and the other is closed
		launch4053.answer <- testDnsPortAnswer{conn: winner}
		launch53.answer <- testDnsPortAnswer{conn: loser}
		race.wait()

		if race.err != nil {
			t.Fatal(race.err)
		}
		for _, conn := range []*testDnsPortConn{winner, loser} {
			returned := race.conn == net.Conn(conn)
			if returned == conn.closed.Load() {
				t.Fatalf("port %d: returned = %t, closed = %t", conn.dnsPort, returned, conn.closed.Load())
			}
		}
	})
}

// A config names its dial ports in launch order, without a port out of range
// or repeated; with none it dials its one port.
func TestExtenderConfigDnsDialPorts(t *testing.T) {
	cases := []struct {
		port     int
		dnsPorts []int
		expected []int
	}{
		{port: ExtenderDnsPort, dnsPorts: nil, expected: []int{ExtenderDnsPort}},
		{port: 5353, dnsPorts: nil, expected: []int{5353}},
		{
			port:     ExtenderDnsPort,
			dnsPorts: []int{ExtenderDnsPort, DefaultDnsPort},
			expected: []int{ExtenderDnsPort, DefaultDnsPort},
		},
		{
			port:     DefaultDnsPort,
			dnsPorts: []int{DefaultDnsPort, 0, ExtenderDnsPort, 70000, DefaultDnsPort},
			expected: []int{DefaultDnsPort, ExtenderDnsPort},
		},
	}
	for _, c := range cases {
		extenderConfig := &ExtenderConfig{
			Profile:  ExtenderProfile{ConnectMode: ExtenderConnectModeDns, Port: c.port},
			DnsPorts: c.dnsPorts,
		}
		if dnsPorts := extenderConfig.dnsDialPorts(); !slices.Equal(dnsPorts, c.expected) {
			t.Errorf("port %d, dns ports %v: dial ports = %v, expected %v",
				c.port, c.dnsPorts, dnsPorts, c.expected)
		}
	}
}

// The dial races its ports as a whodis race on the connect defaults: a 1 s
// stagger and a 2 s stall timeout.
func TestExtenderDnsPortRaceDefaultSchedule(t *testing.T) {
	if schedule := DefaultConnectSettings().extenderDnsRaceSchedule(); schedule != testingWhodisRaceSchedule {
		t.Fatalf("schedule = %+v, want %+v", schedule, testingWhodisRaceSchedule)
	}
}

// What the extender does on one port of a scripted race.
type testDnsPortScript struct {
	// how often the attempt hears the extender, from its launch; zero hears
	// nothing
	hearEvery time.Duration
	// how long the attempt hears it; zero hears it until the attempt ends
	hearFor time.Duration
	// when the extender answers, after the launch; zero never answers
	answerAfter time.Duration
}

// One launch of a scripted race.
type testDnsPortScriptedLaunch struct {
	dnsPort    int
	launchTime time.Time
	// the budget refused its claim
	refused bool
	endTime time.Time
	// why its dial ended, nil for the extender's answer
	cause error
}

// A scripted race over a memory budget that admits `claims` claims.
type testDnsPortScriptedRace struct {
	budget    *PlatformTransportBudget
	startTime time.Time
	endTime   time.Time
	conn      net.Conn
	err       error

	stateLock     sync.Mutex
	launches      []*testDnsPortScriptedLaunch
	liveClaims    int
	maxLiveClaims int
}

// The bytes of one carrier claim in a scripted race.
const testDnsPortClaimByteCount = ByteCount(4096)

// Runs a race over `dnsPorts` whose extender does what `scripts` say on each
// port, under `schedule`, with a budget of `claims` claims. Each attempt claims
// its memory first and holds it until its dial returns, or for the winner,
// until its connection closes. A race still out after a minute ends at that
// deadline, so a race that would wait forever fails its test rather than
// deadlocking the bubble.
func runTestDnsPortScriptedRace(
	ctx context.Context,
	dnsPorts []int,
	claims int,
	schedule quicDialRaceSchedule,
	scripts map[int]testDnsPortScript,
) *testDnsPortScriptedRace {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	race := &testDnsPortScriptedRace{
		budget:    NewPlatformTransportBudget(testDnsPortClaimByteCount*ByteCount(claims), claims),
		startTime: time.Now(),
	}
	policy := extenderQuicMemoryPolicy{budget: race.budget, byteCount: testDnsPortClaimByteCount}
	claimed := func(delta int) {
		race.stateLock.Lock()
		defer race.stateLock.Unlock()
		race.liveClaims += delta
		race.maxLiveClaims = max(race.maxLiveClaims, race.liveClaims)
	}
	dialPort := func(
		ctx context.Context,
		dnsPort int,
		roundTrip *ExtenderRoundTrip,
	) (net.Conn, *protocol.ExtenderResponse, error) {
		launch := &testDnsPortScriptedLaunch{dnsPort: dnsPort, launchTime: time.Now()}
		end := func(refused bool, cause error) {
			race.stateLock.Lock()
			defer race.stateLock.Unlock()
			launch.refused = refused
			launch.endTime = time.Now()
			launch.cause = cause
		}
		func() {
			race.stateLock.Lock()
			defer race.stateLock.Unlock()
			race.launches = append(race.launches, launch)
		}()
		claim, err := policy.acquire(ctx)
		if err != nil {
			end(true, err)
			return nil, nil, err
		}
		claimed(1)
		script := scripts[dnsPort]
		clock := quicDialReceiveClockFromContext(ctx)
		var hearC <-chan time.Time
		if 0 < script.hearEvery {
			hearC = time.After(script.hearEvery)
		}
		var answerC <-chan time.Time
		if 0 < script.answerAfter {
			answerC = time.After(script.answerAfter)
		}
		for {
			select {
			case <-hearC:
				if clock != nil {
					clock.received()
				}
				hearC = nil
				if script.hearFor <= 0 || time.Since(launch.launchTime) < script.hearFor {
					hearC = time.After(script.hearEvery)
				}
			case <-answerC:
				end(false, nil)
				conn := &testDnsPortConn{dnsPort: dnsPort, onClose: func() {
					claimed(-1)
					claim.Release()
				}}
				return conn, &protocol.ExtenderResponse{}, nil
			case <-ctx.Done():
				cause := context.Cause(ctx)
				end(false, cause)
				claimed(-1)
				claim.Release()
				return nil, nil, cause
			}
		}
	}
	race.conn, _, race.err = raceExtenderDnsPorts(ctx, dnsPorts, nil, schedule, dialPort)
	race.endTime = time.Now()
	return race
}

// The launches on one port, in order.
func (self *testDnsPortScriptedRace) launchesOn(dnsPort int) []testDnsPortScriptedLaunch {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	launches := []testDnsPortScriptedLaunch{}
	for _, launch := range self.launches {
		if launch.dnsPort == dnsPort {
			launches = append(launches, *launch)
		}
	}
	return launches
}

// The launches the budget admitted, as port and launch offset, in order.
func (self *testDnsPortScriptedRace) admittedLaunches() []testDnsPortLaunchAt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	admitted := []testDnsPortLaunchAt{}
	for _, launch := range self.launches {
		if !launch.refused {
			admitted = append(admitted, testDnsPortLaunchAt{
				dnsPort: launch.dnsPort,
				after:   launch.launchTime.Sub(self.startTime),
			})
		}
	}
	return admitted
}

type testDnsPortLaunchAt struct {
	dnsPort int
	after   time.Duration
}

// The offset of an instant from the race's start.
func (self *testDnsPortScriptedRace) at(instant time.Time) time.Duration {
	return instant.Sub(self.startTime)
}

// The winner's port, zero for none.
func (self *testDnsPortScriptedRace) winnerPort() int {
	if conn, ok := self.conn.(*testDnsPortConn); ok {
		return conn.dnsPort
	}
	return 0
}

// Closes the winner and checks that every claim came back.
func (self *testDnsPortScriptedRace) assertReleased(t *testing.T) {
	t.Helper()
	if self.conn != nil {
		self.conn.Close()
	}
	stats := self.budget.Stats()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.liveClaims != 0 || stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("claims leaked: live=%d budget=%+v", self.liveClaims, stats)
	}
}

// An attempt on 53 that hears the extender every half second and is answered
// at 3 s, past the stagger and the stall timeout, is never ended for 4053 when
// the budget holds one claim: 4053 is refused at the stagger and waits, and 53
// wins. Ending 53 at the refusal (the direct H3 rule) or timing its silence
// from its launch instead of its last packet each fail this.
func TestExtenderDnsPortRaceKeepsAnAttemptHearingTheExtender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := runTestDnsPortScriptedRace(t.Context(), []int{DefaultDnsPort, ExtenderDnsPort}, 1,
			testingWhodisRaceSchedule, map[int]testDnsPortScript{
				DefaultDnsPort:  {hearEvery: 500 * time.Millisecond, answerAfter: 3 * time.Second},
				ExtenderDnsPort: {answerAfter: time.Millisecond},
			})
		if race.err != nil || race.winnerPort() != DefaultDnsPort || race.at(race.endTime) != 3*time.Second {
			t.Fatalf("race = port %d at %s, %v, want 53 at 3s", race.winnerPort(), race.at(race.endTime), race.err)
		}
		launches53 := race.launchesOn(DefaultDnsPort)
		if len(launches53) != 1 || launches53[0].cause != nil {
			t.Fatalf("53 launches = %+v, want one that the extender answered", launches53)
		}
		launches4053 := race.launchesOn(ExtenderDnsPort)
		if len(launches4053) == 0 || race.at(launches4053[0].launchTime) != testingWhodisRaceSchedule.stagger {
			t.Fatalf("4053 launches = %+v, want the first at the stagger", launches4053)
		}
		for _, launch := range launches4053 {
			if !launch.refused {
				t.Fatalf("4053 held a claim while 53 heard the extender on the only one: %+v", launch)
			}
		}
		if race.maxLiveClaims != 1 {
			t.Fatalf("%d claims held at once on a budget of one", race.maxLiveClaims)
		}
		race.assertReleased(t)
	})
}

// A silent attempt on 53 holding the only claim is ended once it has heard
// nothing for the stall timeout, not at the stagger, with the stall as the
// cause its dial returns. 4053, which the budget refused at the stagger, then
// launches on the freed claim and wins.
func TestExtenderDnsPortRaceEndsASilentAttemptForARefusedPort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := runTestDnsPortScriptedRace(t.Context(), []int{DefaultDnsPort, ExtenderDnsPort}, 1,
			testingWhodisRaceSchedule, map[int]testDnsPortScript{
				DefaultDnsPort:  {},
				ExtenderDnsPort: {answerAfter: 100 * time.Millisecond},
			})
		stallTimeout := testingWhodisRaceSchedule.stallTimeout
		if race.err != nil || race.winnerPort() != ExtenderDnsPort || race.at(race.endTime) != stallTimeout+100*time.Millisecond {
			t.Fatalf("race = port %d at %s, %v, want 4053 at the stall timeout and its answer",
				race.winnerPort(), race.at(race.endTime), race.err)
		}
		launches53 := race.launchesOn(DefaultDnsPort)
		if len(launches53) != 1 || !errors.Is(launches53[0].cause, errQuicDialStalled) ||
			race.at(launches53[0].endTime) != stallTimeout {
			t.Fatalf("53 launches = %+v, want one ended for its silence at the stall timeout", launches53)
		}
		launches4053 := race.launchesOn(ExtenderDnsPort)
		if len(launches4053) != 2 || !launches4053[0].refused ||
			race.at(launches4053[0].launchTime) != testingWhodisRaceSchedule.stagger ||
			launches4053[1].refused || launches4053[1].launchTime.Before(launches53[0].endTime) {
			t.Fatalf("4053 launches = %+v, want a refusal at the stagger and a claim once 53 gave its up", launches4053)
		}
		if race.maxLiveClaims != 1 {
			t.Fatalf("%d claims held at once on a budget of one", race.maxLiveClaims)
		}
		race.assertReleased(t)
	})
}

// An attempt that heard the extender and then went silent stalls the stall
// timeout after its last packet, not after its launch.
func TestExtenderDnsPortRaceStallsFromTheLastPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		race := runTestDnsPortScriptedRace(t.Context(), []int{DefaultDnsPort, ExtenderDnsPort}, 1,
			testingWhodisRaceSchedule, map[int]testDnsPortScript{
				// heard at 0.5 s and 1 s, then nothing
				DefaultDnsPort:  {hearEvery: 500 * time.Millisecond, hearFor: time.Second},
				ExtenderDnsPort: {answerAfter: 100 * time.Millisecond},
			})
		lastPacket := time.Second
		stallTime := lastPacket + testingWhodisRaceSchedule.stallTimeout
		if race.err != nil || race.winnerPort() != ExtenderDnsPort || race.at(race.endTime) != stallTime+100*time.Millisecond {
			t.Fatalf("race = port %d at %s, %v, want 4053 after 53 stalled", race.winnerPort(), race.at(race.endTime), race.err)
		}
		launches53 := race.launchesOn(DefaultDnsPort)
		if len(launches53) != 1 || !errors.Is(launches53[0].cause, errQuicDialStalled) ||
			race.at(launches53[0].endTime) != stallTime {
			t.Fatalf("53 launches = %+v, want one ended %s after its last packet", launches53, testingWhodisRaceSchedule.stallTimeout)
		}
		race.assertReleased(t)
	})
}

// With a second claim, 4053 launches beside a silent 53 at the stagger, and 53
// is not ended for it, at the stall timeout or ever: only 4053's win ends it.
func TestExtenderDnsPortRaceLaunchesBesideWithASecondClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stagger := testingWhodisRaceSchedule.stagger
		race := runTestDnsPortScriptedRace(t.Context(), []int{DefaultDnsPort, ExtenderDnsPort}, 2,
			testingWhodisRaceSchedule, map[int]testDnsPortScript{
				DefaultDnsPort:  {},
				ExtenderDnsPort: {answerAfter: 1500 * time.Millisecond},
			})
		if race.err != nil || race.winnerPort() != ExtenderDnsPort || race.at(race.endTime) != stagger+1500*time.Millisecond {
			t.Fatalf("race = port %d at %s, %v, want 4053 a stagger after 53", race.winnerPort(), race.at(race.endTime), race.err)
		}
		admitted := race.admittedLaunches()
		want := []testDnsPortLaunchAt{{dnsPort: DefaultDnsPort}, {dnsPort: ExtenderDnsPort, after: stagger}}
		if !slices.Equal(admitted, want) {
			t.Fatalf("launches = %+v, want %+v", admitted, want)
		}
		launches53 := race.launchesOn(DefaultDnsPort)
		if errors.Is(launches53[0].cause, errQuicDialStalled) || !launches53[0].endTime.Equal(race.endTime) {
			t.Fatalf("53 = %+v, want it out until 4053 won though a claim was free", launches53[0])
		}
		if race.maxLiveClaims != 2 {
			t.Fatalf("%d claims held at once, want both", race.maxLiveClaims)
		}
		race.assertReleased(t)
	})
}

// The ports launch in the order the dial names them, which is the record's
// (dnsDialPorts), one stagger apart while the budget admits each.
func TestExtenderDnsPortRaceLaunchesInTheGivenOrder(t *testing.T) {
	stagger := testingWhodisRaceSchedule.stagger
	for _, dnsPorts := range [][]int{
		{DefaultDnsPort, ExtenderDnsPort},
		{ExtenderDnsPort, DefaultDnsPort},
		{ExtenderDnsPort, DefaultDnsPort, 5353},
	} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			race := runTestDnsPortScriptedRace(ctx, dnsPorts, len(dnsPorts), testingWhodisRaceSchedule,
				map[int]testDnsPortScript{})
			want := []testDnsPortLaunchAt{}
			for i, dnsPort := range dnsPorts {
				want = append(want, testDnsPortLaunchAt{dnsPort: dnsPort, after: time.Duration(i) * stagger})
			}
			if admitted := race.admittedLaunches(); !slices.Equal(admitted, want) {
				t.Fatalf("ports %v: launches = %+v, want %+v", dnsPorts, admitted, want)
			}
			race.assertReleased(t)
		})
	}
}

// Whatever the budget admits, the race never holds more claims than that, and
// every port gets its turn: a refused port waits for a claim to free, by an
// attempt failing or stalling, and every attempt is silent here. The dial
// ends at the caller's deadline with every claim released.
func TestExtenderDnsPortRaceNeverHoldsMoreClaimsThanTheBudget(t *testing.T) {
	dnsPorts := []int{DefaultDnsPort, ExtenderDnsPort, 5353}
	for _, claims := range []int{1, 2, 3} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			race := runTestDnsPortScriptedRace(ctx, dnsPorts, claims, testingWhodisRaceSchedule,
				map[int]testDnsPortScript{})
			if !errors.Is(race.err, context.DeadlineExceeded) || race.at(race.endTime) != 20*time.Second {
				t.Fatalf("claims %d: race = %v at %s, want the caller's deadline", claims, race.err, race.at(race.endTime))
			}
			if race.maxLiveClaims != claims {
				t.Fatalf("claims %d: %d claims held at once", claims, race.maxLiveClaims)
			}
			for _, dnsPort := range dnsPorts {
				admitted := false
				for _, launch := range race.launchesOn(dnsPort) {
					admitted = admitted || !launch.refused
				}
				if !admitted {
					t.Fatalf("claims %d: port %d never held a claim", claims, dnsPort)
				}
			}
			race.assertReleased(t)
		})
	}
}

// An attempt that keeps hearing the extender and is never answered keeps the
// only claim to the caller's deadline, which ends the dial: a healthy attempt
// keeps its whole budget, and the dial its bound.
func TestExtenderDnsPortRaceEndsAtTheCallerDeadlineWhileHearing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		race := runTestDnsPortScriptedRace(ctx, []int{DefaultDnsPort, ExtenderDnsPort}, 1,
			testingWhodisRaceSchedule, map[int]testDnsPortScript{
				DefaultDnsPort:  {hearEvery: 500 * time.Millisecond},
				ExtenderDnsPort: {answerAfter: time.Millisecond},
			})
		if !errors.Is(race.err, context.DeadlineExceeded) || race.at(race.endTime) != 10*time.Second {
			t.Fatalf("race = %v at %s, want the caller's deadline", race.err, race.at(race.endTime))
		}
		launches53 := race.launchesOn(DefaultDnsPort)
		if len(launches53) != 1 || errors.Is(launches53[0].cause, errQuicDialStalled) ||
			race.at(launches53[0].endTime) != 10*time.Second {
			t.Fatalf("53 launches = %+v, want one that kept the claim to the deadline", launches53)
		}
		for _, launch := range race.launchesOn(ExtenderDnsPort) {
			if !launch.refused {
				t.Fatalf("4053 held a claim while 53 heard the extender: %+v", launch)
			}
		}
		race.assertReleased(t)
	})
}

// A non-positive stagger launches every port at once, and the budget admits as
// many as it holds, in the order their dials reach it; the port it refuses
// waits for a claim as at any stagger, here until the silent ones stall.
func TestExtenderDnsPortRaceWithoutAStaggerLaunchesEveryAdmittedPort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		schedule := testingWhodisRaceSchedule
		schedule.stagger = 0
		dnsPorts := []int{DefaultDnsPort, ExtenderDnsPort, 5353}
		race := runTestDnsPortScriptedRace(ctx, dnsPorts, 2, schedule, map[int]testDnsPortScript{})
		admitted := race.admittedLaunches()
		if len(admitted) != 3 || admitted[0].after != 0 || admitted[1].after != 0 ||
			admitted[2].after != schedule.stallTimeout {
			t.Fatalf("launches = %+v, want two at once and the third at the stall timeout", admitted)
		}
		admittedPorts := []int{admitted[0].dnsPort, admitted[1].dnsPort, admitted[2].dnsPort}
		slices.Sort(admittedPorts)
		if !slices.Equal(admittedPorts, []int{DefaultDnsPort, ExtenderDnsPort, 5353}) {
			t.Fatalf("launches = %+v, want each port once", admitted)
		}
		for _, dnsPort := range dnsPorts {
			if launches := race.launchesOn(dnsPort); race.at(launches[0].launchTime) != 0 {
				t.Fatalf("port %d first launched at %s, want at once", dnsPort, race.at(launches[0].launchTime))
			}
		}
		if race.maxLiveClaims != 2 {
			t.Fatalf("%d claims held at once on a budget of two", race.maxLiveClaims)
		}
		race.assertReleased(t)
	})
}

// A non-positive stall timeout never ends an attempt for its silence: a port
// the budget refused waits for an attempt to fail.
func TestExtenderDnsPortRaceWithoutAStallTimeoutWaitsForAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		schedule := testingWhodisRaceSchedule
		schedule.stallTimeout = 0
		race := runTestDnsPortScriptedRace(ctx, []int{DefaultDnsPort, ExtenderDnsPort}, 1, schedule,
			map[int]testDnsPortScript{ExtenderDnsPort: {answerAfter: time.Millisecond}})
		if !errors.Is(race.err, context.DeadlineExceeded) {
			t.Fatalf("race = port %d, %v, want the caller's deadline", race.winnerPort(), race.err)
		}
		if admitted := race.admittedLaunches(); !slices.Equal(admitted, []testDnsPortLaunchAt{{dnsPort: DefaultDnsPort}}) {
			t.Fatalf("launches = %+v, want 53 alone", admitted)
		}
		race.assertReleased(t)
	})
}

// The race's clocks are its own: each attempt's context carries a clock of
// its own under no other, so what the attempt hears never moves a clock the
// caller's context carries, and a dial of one port carries none.
func TestExtenderDnsPortRaceClocksAreItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		callerClock := newQuicDialReceiveClock(nil)
		ctx, cancel := context.WithTimeout(withQuicDialReceiveClock(t.Context(), callerClock), time.Minute)
		defer cancel()
		attemptClocks := []*quicDialReceiveClock{}
		var clocksLock sync.Mutex
		dialPort := func(
			ctx context.Context,
			dnsPort int,
			roundTrip *ExtenderRoundTrip,
		) (net.Conn, *protocol.ExtenderResponse, error) {
			clock := quicDialReceiveClockFromContext(ctx)
			func() {
				clocksLock.Lock()
				defer clocksLock.Unlock()
				attemptClocks = append(attemptClocks, clock)
			}()
			if clock != nil {
				time.Sleep(100 * time.Millisecond)
				clock.received()
			}
			if dnsPort == ExtenderDnsPort {
				return &testDnsPortConn{dnsPort: dnsPort}, &protocol.ExtenderResponse{}, nil
			}
			<-ctx.Done()
			return nil, nil, context.Cause(ctx)
		}
		conn, _, err := raceExtenderDnsPorts(ctx, []int{DefaultDnsPort, ExtenderDnsPort}, nil, testingWhodisRaceSchedule, dialPort)
		if err != nil || conn.(*testDnsPortConn).dnsPort != ExtenderDnsPort {
			t.Fatalf("race = %v, %v", conn, err)
		}
		if len(attemptClocks) != 2 {
			t.Fatalf("%d attempts", len(attemptClocks))
		}
		for _, clock := range attemptClocks {
			if clock == nil || clock == callerClock || clock.parent != nil {
				t.Fatal("an attempt's clock is not its own")
			}
			if !clock.silentSince().After(clock.startTime) {
				t.Fatal("an attempt's clock did not hear its attempt")
			}
		}
		if !callerClock.silentSince().Equal(callerClock.startTime) {
			t.Fatal("an attempt moved the caller's clock")
		}

		attemptClocks = nil
		if _, _, err := raceExtenderDnsPorts(ctx, []int{ExtenderDnsPort}, nil, testingWhodisRaceSchedule, dialPort); err != nil {
			t.Fatal(err)
		}
		if len(attemptClocks) != 1 || attemptClocks[0] != nil {
			t.Fatal("a dial of one port carries a clock")
		}
	})
}
