package extender

// A full extender reclaims a slot (lag.dev/PLAN.md M3, M4). With max_sessions
// open, a new session takes the slot of the oldest session whose load has
// ended and whose report has gone out. That session is closed with 4410, so
// the browser keeps its results and does not reconnect. When no session is
// done that far, the new session is refused as before.

import (
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The extender's side of a test session, found by its hello.
func (self *lagApiFixture) serverSession(session *lagTestSession) *lagApiSession {
	self.t.Helper()
	sessionId, ok := parseLagApiSessionId(session.hello.Session)
	if !ok {
		self.t.Fatalf("hello session %q", session.hello.Session)
	}
	serverSession := self.api.session(sessionId)
	if serverSession == nil {
		self.t.Fatalf("no open session %q", session.hello.Session)
	}
	return serverSession
}

// Gives up the session's one load before a byte moves, which ends it at once
// with its result (M4). Then waits for its report (M8), after which the
// session has nothing left to measure.
func (self *lagApiFixture) endLoadAndReport(session *lagTestSession) {
	self.t.Helper()
	serverSession := self.serverSession(session)
	session.load("download")
	session.load("done")
	if message := session.nextResult(5 * time.Second); message.Type != "loadResult" {
		self.t.Fatalf("result %+v", message.raw)
	}
	self.nextReport(5 * time.Second)
	waitFor(self.t, serverSession.reclaimable.Load)
}

func (self *extenderLagApi) sessionCountNow() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.sessions)
}

func (self *extenderLagApi) subnetSessionCountsNow() map[extenderSubnetHash]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.subnetSessionCounts)
}

// Whether a test session is still open and pinged.
func (self *lagTestSession) expectOpen(name string) {
	self.t.Helper()
	if rtts := self.rttsFor(600 * time.Millisecond); len(rtts) < 1 {
		self.t.Errorf("%s: no rtts", name)
	}
	select {
	case <-self.done:
		self.t.Errorf("%s: closed %d %q", name, self.closeCode, self.closeText)
	default:
	}
}

// M3, M4: with max_sessions open, a new session takes the slot of the oldest
// session whose load has ended and whose report has gone out. That session is
// closed with 4410 "reclaimed" and sends no second report, and the others go
// on pinging. Once no open session has its report out, a new session is
// refused with 4429 as before. Every subnet count follows the open sessions.
func TestLagApiFullReclaimsTheOldestReportedSession(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 3
	})
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.reportBefore = 100 * time.Millisecond
		timings.reportAfter = 100 * time.Millisecond
		timings.reportWindow = 5 * time.Second
	})
	older := fixture.openHelloSession(fixture.ticket(nil))
	newer := fixture.openHelloSession(fixture.ticket(nil))
	pinging := fixture.openHelloSession(fixture.ticket(nil))
	fixture.endLoadAndReport(older)
	fixture.endLoadAndReport(newer)
	// every client of the fixture is one loopback subnet
	subnetCounts := fixture.api.subnetSessionCountsNow()
	if len(subnetCounts) != 1 {
		t.Fatalf("subnet counts %v", subnetCounts)
	}
	expectSubnetCount := func(expected int) {
		t.Helper()
		for _, count := range fixture.api.subnetSessionCountsNow() {
			if count != expected {
				t.Errorf("subnet count %d, expected %d", count, expected)
			}
		}
		if count := fixture.api.sessionCountNow(); count != expected {
			t.Errorf("%d sessions, expected %d", count, expected)
		}
	}
	expectSubnetCount(3)

	// full: the oldest session with its report out makes room
	first := fixture.openHelloSession(fixture.ticket(nil))
	if code, text := older.waitClosed(5 * time.Second); code != lagApiCloseCodeReclaimed || text != lagApiCloseTextReclaimed {
		t.Errorf("oldest closed %d %q", code, text)
	}
	// its one report went out before
	fixture.noReport(300 * time.Millisecond)
	newer.expectOpen("newer")
	pinging.expectOpen("pinging")
	first.expectOpen("first")
	expectSubnetCount(3)

	// full again: the next oldest
	second := fixture.openHelloSession(fixture.ticket(nil))
	if code, text := newer.waitClosed(5 * time.Second); code != lagApiCloseCodeReclaimed || text != lagApiCloseTextReclaimed {
		t.Errorf("next oldest closed %d %q", code, text)
	}
	fixture.noReport(300 * time.Millisecond)
	expectSubnetCount(3)

	// full, and no open session has its report out: refused as before, and
	// nothing open is closed for it
	refused := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	if message := refused.next("error", 5*time.Second); message.Code != lagApiErrorSessions {
		t.Errorf("sessions: %+v", message.raw)
	}
	if code, text := refused.waitClosed(5 * time.Second); code != lagApiCloseCodeLimited || text != lagApiErrorSessions {
		t.Errorf("sessions close %d %q", code, text)
	}
	pinging.expectOpen("pinging")
	first.expectOpen("first")
	second.expectOpen("second")
	expectSubnetCount(3)

	// each close gives its subnet's count back once
	for _, session := range []*lagTestSession{pinging, first, second} {
		session.close()
		fixture.nextReport(5 * time.Second)
	}
	waitFor(t, func() bool {
		return fixture.api.sessionCountNow() == 0 && len(fixture.api.subnetSessionCountsNow()) == 0
	})
}

// M3: a full extender closes no session whose report has not gone out: not
// one that only pings, and not one whose load has ended but whose minute is
// still open. The new session is refused with 4429, and both go on.
func TestLagApiFullRefusesWithNothingReclaimable(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 2
	})
	fixture.setTimings(func(timings *lagApiTimings) {
		// the loaded session's minute stays open through the test
		timings.reportBefore = 100 * time.Millisecond
		timings.reportAfter = 30 * time.Second
		timings.reportWindow = 60 * time.Second
	})
	pinging := fixture.openHelloSession(fixture.ticket(nil))
	loaded := fixture.openHelloSession(fixture.ticket(nil))
	loaded.load("download")
	loaded.load("done")
	if message := loaded.nextResult(5 * time.Second); message.Type != "loadResult" {
		t.Fatalf("result %+v", message.raw)
	}

	refused := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	if message := refused.next("error", 5*time.Second); message.Code != lagApiErrorSessions {
		t.Errorf("sessions: %+v", message.raw)
	}
	if code, text := refused.waitClosed(5 * time.Second); code != lagApiCloseCodeLimited || text != lagApiErrorSessions {
		t.Errorf("sessions close %d %q", code, text)
	}
	pinging.expectOpen("pinging")
	loaded.expectOpen("loaded")
	if count := fixture.api.sessionCountNow(); count != 2 {
		t.Errorf("%d sessions", count)
	}
	if fixture.serverSession(loaded).reclaimable.Load() {
		t.Error("a session whose report has not gone out is reclaimable")
	}
	fixture.noReport(100 * time.Millisecond)
}

// M3: reclaiming keeps the per-subnet counts. The closed session's subnet
// gives its count back once, when the session is reclaimed and not again when
// it finishes, and the new session's subnet takes one. The count is the
// extender's, so the oldest session of any subnet is reclaimed. A session
// over its subnet's limit is refused before any room is made, so nothing is
// reclaimed for it.
func TestLagApiReclaimKeepsSubnetCounts(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 2
	})
	api := fixture.api
	newSession := func(remoteAddress string) *lagApiSession {
		t.Helper()
		session, err := newLagApiSession(api, nil, &protocol.LagTicketBody{
			TicketId:     testLagRandomBytes(16),
			ExpireTimeMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(session.cancel)
		session.serverName = testLagName
		session.remoteAddress = remoteAddress
		session.subnet, session.subnetLimited = api.subnetOf(remoteAddress)
		if !session.subnetLimited {
			t.Fatalf("%s has no subnet", remoteAddress)
		}
		return session
	}
	expectCounts := func(expected map[extenderSubnetHash]int) {
		t.Helper()
		if counts := api.subnetSessionCountsNow(); !maps.Equal(counts, expected) {
			t.Errorf("subnet counts %v, expected %v", counts, expected)
		}
	}
	expectOpen := func(name string, session *lagApiSession) {
		t.Helper()
		if session.ctx.Err() != nil {
			code, text := session.closeReason()
			t.Errorf("%s ended %d %q", name, code, text)
		}
	}

	older := newSession("198.51.100.7:40000")
	newer := newSession("203.0.113.9:40000")
	for _, session := range []*lagApiSession{older, newer} {
		if code := api.admitSession(session); code != "" {
			t.Fatalf("admission %q", code)
		}
	}
	expectCounts(map[extenderSubnetHash]int{older.subnet: 1, newer.subnet: 1})

	// full, and neither has nothing left to measure
	third := newSession("192.0.2.1:40000")
	if code := api.admitSession(third); code != lagApiErrorSessions {
		t.Errorf("full admission %q", code)
	}
	expectOpen("older", older)
	expectOpen("newer", newer)
	expectCounts(map[extenderSubnetHash]int{older.subnet: 1, newer.subnet: 1})

	// both are done; one more of newer's subnet is over that subnet's limit,
	// and is refused without closing either
	older.reclaimable.Store(true)
	newer.reclaimable.Store(true)
	api.stateLock.Lock()
	api.maxSessionsPerSubnet = 1
	api.stateLock.Unlock()
	sameSubnet := newSession("203.0.113.10:40001")
	if sameSubnet.subnet != newer.subnet {
		t.Fatal("the subnets differ")
	}
	if code := api.admitSession(sameSubnet); code != lagApiErrorSessions {
		t.Errorf("admission over the subnet's limit %q", code)
	}
	expectOpen("older", older)
	expectOpen("newer", newer)
	expectCounts(map[extenderSubnetHash]int{older.subnet: 1, newer.subnet: 1})
	api.stateLock.Lock()
	api.maxSessionsPerSubnet = lagApiMaxSessionsPerSubnet
	api.stateLock.Unlock()

	// the oldest, of another subnet, makes room
	if code := api.admitSession(third); code != "" {
		t.Fatalf("admission %q", code)
	}
	if older.ctx.Err() == nil {
		t.Error("the oldest was not ended")
	} else if code, text := older.closeReason(); code != lagApiCloseCodeReclaimed || text != lagApiCloseTextReclaimed {
		t.Errorf("the oldest closed %d %q", code, text)
	}
	expectOpen("newer", newer)
	expectCounts(map[extenderSubnetHash]int{newer.subnet: 1, third.subnet: 1})

	// the reclaimed session's own end gives nothing back again
	api.removeSession(older)
	expectCounts(map[extenderSubnetHash]int{newer.subnet: 1, third.subnet: 1})
	if count := api.sessionCountNow(); count != 2 {
		t.Errorf("%d sessions", count)
	}
	api.removeSession(newer)
	api.removeSession(third)
	expectCounts(map[extenderSubnetHash]int{})
}
