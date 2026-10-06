package connect

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The dns carrier's port race (EXTENDER.md L2, net_extender_dns_ports.go),
// driven through its clock and dial seams so every ordering is forced by the
// test: a port answers only when the test answers it, a port the test never
// answers is a blackhole that holds its attempt until the race cancels it,
// and the next port launches on the stagger only when the test fires it.

// One attempt the race launched on one port.
type testDnsPortLaunch struct {
	dnsPort int
	ctx     context.Context
	answer  chan testDnsPortAnswer
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
}

func (self *testDnsPortConn) Close() error {
	self.closed.Store(true)
	return nil
}

// One race under test: its launches and stagger arms as the test sees them,
// and its outcome once it returns.
type testDnsPortRace struct {
	t         *testing.T
	launches  chan *testDnsPortLaunch
	staggers  chan chan time.Time
	roundTrip *ExtenderRoundTrip
	conn      net.Conn
	response  *protocol.ExtenderResponse
	err       error
	finished  chan struct{}
}

// Starts a race over `dnsPorts` with the test's dial and stagger.
func startTestDnsPortRace(t *testing.T, dnsPorts []int) *testDnsPortRace {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	race := &testDnsPortRace{
		t:         t,
		launches:  make(chan *testDnsPortLaunch, 16),
		staggers:  make(chan chan time.Time, 16),
		roundTrip: &ExtenderRoundTrip{},
		finished:  make(chan struct{}),
	}
	stagger := func() <-chan time.Time {
		staggerC := make(chan time.Time, 1)
		race.staggers <- staggerC
		return staggerC
	}
	dialPort := func(
		ctx context.Context,
		dnsPort int,
		roundTrip *ExtenderRoundTrip,
	) (net.Conn, *protocol.ExtenderResponse, error) {
		launch := &testDnsPortLaunch{
			dnsPort: dnsPort,
			ctx:     ctx,
			answer:  make(chan testDnsPortAnswer, 1),
			done:    make(chan struct{}),
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
			return nil, nil, ctx.Err()
		}
	}
	go func() {
		defer close(race.finished)
		race.conn, race.response, race.err = raceExtenderDnsPorts(
			ctx,
			dnsPorts,
			race.roundTrip,
			stagger,
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
	case <-time.After(10 * time.Second):
		self.t.Fatalf("the race never launched port %d", dnsPort)
	}
	return nil
}

// Fires the stagger the race armed last.
func (self *testDnsPortRace) fireStagger() {
	self.t.Helper()
	select {
	case staggerC := <-self.staggers:
		staggerC <- time.Time{}
	case <-time.After(10 * time.Second):
		self.t.Fatal("the race armed no stagger")
	}
}

// Waits for the race to return.
func (self *testDnsPortRace) wait() {
	self.t.Helper()
	select {
	case <-self.finished:
	case <-time.After(10 * time.Second):
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
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
	launch := race.nextLaunch(ExtenderDnsPort)
	conn := &testDnsPortConn{dnsPort: ExtenderDnsPort}
	launch.answer <- testDnsPortAnswer{conn: conn}
	race.wait()

	if race.err != nil || race.conn != conn {
		t.Fatalf("race = %v, %v, expected the 4053 connection", race.conn, race.err)
	}
	race.assertNoLaunch()
	if conn.closed.Load() {
		t.Fatal("the race closed the connection it returned")
	}
}

// A 4053-only extender, which every app extender is, is reached without
// waiting out 53: 53 goes out one stagger after 4053 and never answers, and
// the race returns the moment 4053 does, with 53 canceled and joined.
func TestExtenderDnsPortRaceReachesA4053ExtenderWithoutWaitingOut53(t *testing.T) {
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
	launch4053 := race.nextLaunch(ExtenderDnsPort)
	race.fireStagger()
	launch53 := race.nextLaunch(DefaultDnsPort)

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
}

// A record that lists 53 is dialed on 53 first, and where the client's network
// blackholes 53, 4053 goes out after one stagger rather than after 53's whole
// attempt.
func TestExtenderDnsPortRaceTries4053OneStaggerBehindABlackholed53(t *testing.T) {
	race := startTestDnsPortRace(t, []int{DefaultDnsPort, ExtenderDnsPort})
	launch53 := race.nextLaunch(DefaultDnsPort)
	race.fireStagger()
	launch4053 := race.nextLaunch(ExtenderDnsPort)

	conn := &testDnsPortConn{dnsPort: ExtenderDnsPort}
	launch4053.answer <- testDnsPortAnswer{conn: conn}
	race.wait()

	if race.err != nil || race.conn != conn {
		t.Fatalf("race = %v, %v, expected the 4053 connection", race.conn, race.err)
	}
	assertTestDnsPortLaunchJoined(t, launch53)
}

// A port that fails launches the next at once, without the stagger.
func TestExtenderDnsPortRaceLaunchesTheNextPortAtOnceAfterAFailure(t *testing.T) {
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
	launch4053 := race.nextLaunch(ExtenderDnsPort)
	launch4053.answer <- testDnsPortAnswer{err: errors.New("no route to the port")}
	// the stagger is armed but never fired
	launch53 := race.nextLaunch(DefaultDnsPort)

	conn := &testDnsPortConn{dnsPort: DefaultDnsPort}
	launch53.answer <- testDnsPortAnswer{conn: conn}
	race.wait()
	if race.err != nil || race.conn != conn {
		t.Fatalf("race = %v, %v, expected the 53 connection", race.conn, race.err)
	}
}

// An answer from the extender ends the race: it refuses or limits the request
// on every port alike, so no other port is tried.
func TestExtenderDnsPortRaceEndsOnAnExtenderAnswer(t *testing.T) {
	answers := []error{
		&ExtenderLimitedError{RetryAfter: time.Minute},
		&ExtenderRefusedError{StatusCode: 403},
	}
	for _, answer := range answers {
		race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
		launch := race.nextLaunch(ExtenderDnsPort)
		launch.answer <- testDnsPortAnswer{err: answer}
		race.wait()
		if race.err != answer {
			t.Fatalf("race err = %v, expected the extender's answer %v", race.err, answer)
		}
		race.assertNoLaunch()
	}
}

// With every port failed the race fails with every port's error.
func TestExtenderDnsPortRaceJoinsTheErrorsOfEveryPort(t *testing.T) {
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
	err4053 := errors.New("4053 failed")
	err53 := errors.New("53 failed")
	race.nextLaunch(ExtenderDnsPort).answer <- testDnsPortAnswer{err: err4053}
	race.nextLaunch(DefaultDnsPort).answer <- testDnsPortAnswer{err: err53}
	race.wait()
	if race.conn != nil || !errors.Is(race.err, err4053) || !errors.Is(race.err, err53) {
		t.Fatalf("race = %v, %v, expected both errors", race.conn, race.err)
	}
}

// A port the memory budget refused while 4053 held the budget is not a port
// that failed: it goes again once 4053 ends.
func TestExtenderDnsPortRaceRetriesAPortTheMemoryBudgetRefused(t *testing.T) {
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
	launch4053 := race.nextLaunch(ExtenderDnsPort)
	race.fireStagger()
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
}

// A refusal that lands after an attempt that held the budget already ended
// goes again at once, since that attempt freed what it held.
func TestExtenderDnsPortRaceRetriesARefusalAfterAnAttemptEnded(t *testing.T) {
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort, 5353})
	launch4053 := race.nextLaunch(ExtenderDnsPort)
	race.fireStagger()
	launch53 := race.nextLaunch(DefaultDnsPort)
	launch4053.answer <- testDnsPortAnswer{err: errors.New("4053 failed")}
	// the failure launches the third port at once, which is how the test
	// knows the race took it before the refusal below
	race.nextLaunch(5353)
	launch53.answer <- testDnsPortAnswer{err: errExtenderMemoryBudget}
	retry53 := race.nextLaunch(DefaultDnsPort)

	conn := &testDnsPortConn{dnsPort: DefaultDnsPort}
	retry53.answer <- testDnsPortAnswer{conn: conn}
	race.wait()
	if race.err != nil || race.conn != conn {
		t.Fatalf("race = %v, %v, expected the 53 connection", race.conn, race.err)
	}
}

// A loser that connected after the race returned is closed by its own
// attempt, so no carrier outlives the race.
func TestExtenderDnsPortRaceClosesALoserThatConnected(t *testing.T) {
	race := startTestDnsPortRace(t, []int{ExtenderDnsPort, DefaultDnsPort})
	launch4053 := race.nextLaunch(ExtenderDnsPort)
	race.fireStagger()
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
