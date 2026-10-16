package extender

// A full extender reclaims a slot (lag.dev/PLAN.md M3, M4). With max_sessions
// open, a new session takes the slot of the oldest session whose load has
// ended and whose report has gone out, else of the oldest session that has
// started no load and has been open the idle reclaim wait. That session is
// closed with 4410, so the browser keeps what it has and does not reconnect.
// When no session is either, the new session is refused as before. One source
// block, a /24 or /48, holds at most a share of the sessions.

import (
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
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
	waitFor(self.t, serverSession.finished.Load)
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

func (self *extenderLagApi) blockSessionCountsNow() map[extenderSubnetHash]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.blockSessionCounts)
}

// Makes an admitted session look admitted age ago, as one open that long, so
// a test need not wait out the idle reclaim wait (M3).
func (self *extenderLagApi) backdateAdmission(session *lagApiSession, age time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	session.admitTime = time.Now().Add(-age)
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
// on pinging. Once no open session has its report out, and none without a
// load has been open the idle reclaim wait, a new session is refused with
// 4429 as before. Every subnet count follows the open sessions.
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
// one that only pings and has been open less than the idle reclaim wait, and
// not one whose load has ended but whose minute is still open. The new
// session is refused with 4429, and both go on.
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
	if fixture.serverSession(loaded).finished.Load() {
		t.Error("a session whose report has not gone out is finished")
	}
	fixture.noReport(100 * time.Millisecond)
}

// A session as admission sees it, from a source address, built without a
// connection: only admission and the counts are exercised (M3).
func (self *lagApiFixture) admissionSession(remoteAddress string) *lagApiSession {
	self.t.Helper()
	session, err := newLagApiSession(self.api, nil, &protocol.LagTicketBody{
		TicketId:     testLagRandomBytes(16),
		ExpireTimeMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(session.cancel)
	session.serverName = testLagName
	session.setSource(remoteAddress)
	if !session.subnetLimited {
		self.t.Fatalf("%s has no subnet", remoteAddress)
	}
	return session
}

// Checks that the subnet and block counts are those of exactly these open
// sessions, each counted once under its subnet and once under its block.
func (self *lagApiFixture) expectSourceCounts(sessions ...*lagApiSession) {
	self.t.Helper()
	subnetCounts := map[extenderSubnetHash]int{}
	blockCounts := map[extenderSubnetHash]int{}
	for _, session := range sessions {
		subnetCounts[session.subnet] += 1
		blockCounts[session.block] += 1
	}
	if counts := self.api.subnetSessionCountsNow(); !maps.Equal(counts, subnetCounts) {
		self.t.Errorf("subnet counts %v, expected %v", counts, subnetCounts)
	}
	if counts := self.api.blockSessionCountsNow(); !maps.Equal(counts, blockCounts) {
		self.t.Errorf("block counts %v, expected %v", counts, blockCounts)
	}
	if count := self.api.sessionCountNow(); count != len(sessions) {
		self.t.Errorf("%d sessions, expected %d", count, len(sessions))
	}
}

// Whether a session built by admissionSession is still open, or how the
// extender ended it.
func expectAdmittedOpen(t *testing.T, name string, session *lagApiSession) {
	t.Helper()
	if session.ctx.Err() != nil {
		code, text := session.closeReason()
		t.Errorf("%s ended %d %q", name, code, text)
	}
}

func expectReclaimed(t *testing.T, name string, session *lagApiSession) {
	t.Helper()
	if session.ctx.Err() == nil {
		t.Errorf("%s was not ended", name)
	} else if code, text := session.closeReason(); code != lagApiCloseCodeReclaimed || text != lagApiCloseTextReclaimed {
		t.Errorf("%s closed %d %q", name, code, text)
	}
}

// M3: reclaiming keeps the per-subnet counts, and the per-block ones. The
// closed session's subnet and block give their counts back once, when the
// session is reclaimed and not again when it finishes, and the new session's
// take one. The count is the extender's, so the oldest session of any subnet
// is reclaimed. A session over its subnet's limit is refused before any room
// is made, so nothing is reclaimed for it.
func TestLagApiReclaimKeepsSubnetCounts(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 2
	})
	api := fixture.api

	older := fixture.admissionSession("198.51.100.7:40000")
	newer := fixture.admissionSession("203.0.113.9:40000")
	for _, session := range []*lagApiSession{older, newer} {
		if code := api.admitSession(session); code != "" {
			t.Fatalf("admission %q", code)
		}
	}
	fixture.expectSourceCounts(older, newer)

	// full, and neither has nothing left to measure
	third := fixture.admissionSession("192.0.2.1:40000")
	if code := api.admitSession(third); code != lagApiErrorSessions {
		t.Errorf("full admission %q", code)
	}
	expectAdmittedOpen(t, "older", older)
	expectAdmittedOpen(t, "newer", newer)
	fixture.expectSourceCounts(older, newer)

	// both are done; one more of newer's subnet is over that subnet's limit,
	// and is refused without closing either
	older.finished.Store(true)
	newer.finished.Store(true)
	api.stateLock.Lock()
	api.maxSessionsPerSubnet = 1
	api.stateLock.Unlock()
	sameSubnet := fixture.admissionSession("203.0.113.10:40001")
	if sameSubnet.subnet != newer.subnet {
		t.Fatal("the subnets differ")
	}
	if code := api.admitSession(sameSubnet); code != lagApiErrorSessions {
		t.Errorf("admission over the subnet's limit %q", code)
	}
	expectAdmittedOpen(t, "older", older)
	expectAdmittedOpen(t, "newer", newer)
	fixture.expectSourceCounts(older, newer)
	api.stateLock.Lock()
	api.maxSessionsPerSubnet = lagApiMaxSessionsPerSubnet
	api.stateLock.Unlock()

	// the oldest, of another subnet, makes room
	if code := api.admitSession(third); code != "" {
		t.Fatalf("admission %q", code)
	}
	expectReclaimed(t, "the oldest", older)
	expectAdmittedOpen(t, "newer", newer)
	fixture.expectSourceCounts(newer, third)

	// the reclaimed session's own end gives nothing back again
	api.removeSession(older)
	fixture.expectSourceCounts(newer, third)
	api.removeSession(newer)
	api.removeSession(third)
	fixture.expectSourceCounts()
}

// A signed ticket for a track position, which names the session's report.
func (self *lagApiFixture) positionTicket(position uint32) string {
	self.t.Helper()
	return self.ticket(func(body *protocol.LagTicketBody) {
		body.TrackPosition = position
	})
}

// Checks that a session ended to make room with 4410.
func (self *lagTestSession) expectReclaimed(name string) {
	self.t.Helper()
	if code, text := self.waitClosed(5 * time.Second); code != lagApiCloseCodeReclaimed || text != lagApiCloseTextReclaimed {
		self.t.Errorf("%s closed %d %q", name, code, text)
	}
}

// Checks that a session is refused as sessions with 4429.
func (self *lagTestSession) expectRefusedSessions(name string) {
	self.t.Helper()
	if message := self.next("error", 5*time.Second); message.Code != lagApiErrorSessions {
		self.t.Errorf("%s: %+v", name, message.raw)
	}
	if code, text := self.waitClosed(5 * time.Second); code != lagApiCloseCodeLimited || text != lagApiErrorSessions {
		self.t.Errorf("%s closed %d %q", name, code, text)
	}
}

// Checks the one report of a session that started no load (M8): its own,
// with no load, ending as expected, and with its pings.
func (self *lagApiFixture) expectNoLoadReport(position uint32, endReason protocol.LagEndReason) {
	self.t.Helper()
	report := self.nextReport(5 * time.Second)
	if report.TrackPosition != position || report.EndReason != endReason {
		self.t.Errorf("report of position %d ending %s, expected %d ending %s",
			report.TrackPosition, report.EndReason, position, endReason)
	}
	if report.DownloadMs != 0 || report.UploadMs != 0 || 0 < len(report.DownloadSentBytes) || 0 < len(report.UploadBytes) {
		self.t.Errorf("report of position %d has a load", report.TrackPosition)
	}
	if len(report.PingMicros) == 0 {
		self.t.Errorf("report of position %d has no pings", report.TrackPosition)
	}
}

// M3: a full extender with no finished session closes its oldest session that
// has started no load, once that has been open the idle reclaim wait of 15
// minutes, with 4410, and admits the new one. A session whose load was
// refused has started none. The session closed sends its one report as it
// ends: its last minute with no load, or busy when its load was refused. The
// younger sessions go on, and once only sessions younger than the wait are
// left, a new session is refused with 4429 as before.
func TestLagApiFullReclaimsAnOldIdleSession(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 3
	})
	if fixture.api.timings.idleReclaim != 15*time.Minute {
		t.Errorf("idle reclaim wait %s", fixture.api.timings.idleReclaim)
	}
	pinging := fixture.openHelloSession(fixture.positionTicket(0))
	refused := fixture.openHelloSession(fixture.positionTicket(1))
	// a load running on a third session gets refused's own load refused:
	// every client here is one subnet, which runs one load at a time
	loader := fixture.openHelloSession(fixture.positionTicket(2))
	loader.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	refused.load("download")
	if message := refused.next("error", 5*time.Second); message.Code != lagApiErrorBusy {
		t.Fatalf("refused load: %+v", message.raw)
	}
	loader.close()
	if report := fixture.nextReport(5 * time.Second); report.TrackPosition != 2 {
		t.Fatalf("report of position %d", report.TrackPosition)
	}
	young := fixture.openHelloSession(fixture.positionTicket(3))
	// each has a round trip for its report to hold
	pinging.expectOpen("pinging")
	refused.expectOpen("refused")
	pingingServer := fixture.serverSession(pinging)
	refusedServer := fixture.serverSession(refused)
	youngServer := fixture.serverSession(young)
	fixture.expectSourceCounts(pingingServer, refusedServer, youngServer)
	// the two older have been open past the wait, in the order admitted
	fixture.api.backdateAdmission(pingingServer, lagApiIdleReclaim+2*time.Minute)
	fixture.api.backdateAdmission(refusedServer, lagApiIdleReclaim+time.Minute)

	// full: the oldest that has started no load makes room, and sends its one
	// report as it ends
	first := fixture.openHelloSession(fixture.positionTicket(4))
	pinging.expectReclaimed("the oldest")
	fixture.expectNoLoadReport(0, protocol.LagEndReason_LAG_END_NO_LOAD)
	fixture.noReport(300 * time.Millisecond)
	refused.expectOpen("refused")
	young.expectOpen("young")
	first.expectOpen("first")
	firstServer := fixture.serverSession(first)
	fixture.expectSourceCounts(refusedServer, youngServer, firstServer)

	// full again: the next oldest, whose load was refused
	second := fixture.openHelloSession(fixture.positionTicket(5))
	refused.expectReclaimed("the next oldest")
	fixture.expectNoLoadReport(1, protocol.LagEndReason_LAG_END_BUSY)
	fixture.noReport(300 * time.Millisecond)
	secondServer := fixture.serverSession(second)
	fixture.expectSourceCounts(youngServer, firstServer, secondServer)

	// full, and every session younger than the wait: refused, and nothing
	// open is closed for it
	fixture.openSession(fixture.positionTicket(6), testLagOrigin).expectRefusedSessions("past the young")
	young.expectOpen("young")
	first.expectOpen("first")
	second.expectOpen("second")
	fixture.noReport(100 * time.Millisecond)
	fixture.expectSourceCounts(youngServer, firstServer, secondServer)
}

// M3: a session that has started no load is never closed before it has been
// open the idle reclaim wait, however full the extender is: the dots of a
// track in view wait for their loads a few minutes at most. A minute short of
// the wait, a new session is refused with 4429 and nothing is closed; at the
// wait, the oldest makes room.
func TestLagApiFullRefusesWhileIdleSessionsAreYoung(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 2
	})
	older := fixture.openHelloSession(fixture.positionTicket(0))
	newer := fixture.openHelloSession(fixture.positionTicket(1))
	olderServer := fixture.serverSession(older)
	newerServer := fixture.serverSession(newer)
	fixture.api.backdateAdmission(olderServer, lagApiIdleReclaim-time.Minute)
	fixture.api.backdateAdmission(newerServer, lagApiIdleReclaim-2*time.Minute)

	fixture.openSession(fixture.positionTicket(2), testLagOrigin).expectRefusedSessions("full of young")
	older.expectOpen("older")
	newer.expectOpen("newer")
	fixture.noReport(100 * time.Millisecond)
	fixture.expectSourceCounts(olderServer, newerServer)

	fixture.api.backdateAdmission(olderServer, lagApiIdleReclaim)
	admitted := fixture.openHelloSession(fixture.positionTicket(3))
	older.expectReclaimed("older at the wait")
	fixture.expectNoLoadReport(0, protocol.LagEndReason_LAG_END_NO_LOAD)
	newer.expectOpen("newer")
	admitted.expectOpen("admitted")
	fixture.expectSourceCounts(newerServer, fixture.serverSession(admitted))
}

// M3: a finished session makes room before one that has started no load,
// however much older that one is. A session that has started no load may
// still ask for it, as a page put away does once it is back, while a finished
// one has nothing left to measure. Once no session has finished, the oldest
// that has started no load in the wait makes room, and then a new session is
// refused.
func TestLagApiFullReclaimsFinishedBeforeIdle(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 3
	})
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.reportBefore = 100 * time.Millisecond
		timings.reportAfter = 100 * time.Millisecond
		timings.reportWindow = 5 * time.Second
	})
	idle := fixture.openHelloSession(fixture.positionTicket(0))
	finished := fixture.openHelloSession(fixture.positionTicket(1))
	young := fixture.openHelloSession(fixture.positionTicket(2))
	idleServer := fixture.serverSession(idle)
	youngServer := fixture.serverSession(young)
	fixture.endLoadAndReport(finished)
	fixture.api.backdateAdmission(idleServer, lagApiIdleReclaim+time.Hour)

	// full: the finished session, though the idle one is older and past the
	// wait; its one report went out before
	first := fixture.openHelloSession(fixture.positionTicket(3))
	finished.expectReclaimed("finished")
	fixture.noReport(300 * time.Millisecond)
	idle.expectOpen("idle")
	young.expectOpen("young")
	first.expectOpen("first")
	firstServer := fixture.serverSession(first)
	fixture.expectSourceCounts(idleServer, youngServer, firstServer)

	// full again, and none has finished: the idle one past the wait
	second := fixture.openHelloSession(fixture.positionTicket(4))
	idle.expectReclaimed("idle")
	fixture.expectNoLoadReport(0, protocol.LagEndReason_LAG_END_NO_LOAD)
	fixture.noReport(300 * time.Millisecond)
	young.expectOpen("young")
	first.expectOpen("first")
	second.expectOpen("second")
	secondServer := fixture.serverSession(second)
	fixture.expectSourceCounts(youngServer, firstServer, secondServer)

	// full, and nothing finished or past the wait
	fixture.openSession(fixture.positionTicket(5), testLagOrigin).expectRefusedSessions("nothing to reclaim")
	fixture.expectSourceCounts(youngServer, firstServer, secondServer)
}

// M3: the reclaim and a load are each decided under the state lock, so they
// never cross. A session whose load has been admitted is never closed as one
// that has started none, however long it has been open, and nor while its
// report is still to go out. A session closed for having started none can
// start no load after: asked as it goes, the API counts no load and has
// nothing to tell the browser, whose close is on its way.
func TestLagApiReclaimedIdleSessionStartsNoLoad(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 1
	})
	api := fixture.api

	loading := fixture.admissionSession("198.51.100.7:40000")
	if code := api.admitSession(loading); code != "" {
		t.Fatalf("admission %q", code)
	}
	api.backdateAdmission(loading, lagApiIdleReclaim+time.Hour)
	if code, _ := api.beginLoad(time.Now(), loading); code != "" {
		t.Fatalf("load refused %q", code)
	}
	knocking := fixture.admissionSession("203.0.113.9:40000")
	if code := api.admitSession(knocking); code != lagApiErrorSessions {
		t.Errorf("admission beside a running load %q", code)
	}
	// its load over, its report not yet out
	api.endLoad(loading)
	if code := api.admitSession(knocking); code != lagApiErrorSessions {
		t.Errorf("admission beside a load not yet reported %q", code)
	}
	expectAdmittedOpen(t, "loading", loading)
	fixture.expectSourceCounts(loading)
	api.removeSession(loading)

	idle := fixture.admissionSession("192.0.2.1:40000")
	if code := api.admitSession(idle); code != "" {
		t.Fatalf("admission %q", code)
	}
	api.backdateAdmission(idle, lagApiIdleReclaim)
	if code := api.admitSession(knocking); code != "" {
		t.Fatalf("admission %q", code)
	}
	expectReclaimed(t, "idle", idle)
	if code, _ := api.beginLoad(time.Now(), idle); code != lagApiLoadReclaimed {
		t.Errorf("load of a reclaimed session %q", code)
	}
	if count := api.loadCountNow(); count != 0 {
		t.Errorf("%d loads", count)
	}
	fixture.expectSourceCounts(knocking)
	// its own end gives nothing back again
	api.removeSession(idle)
	fixture.expectSourceCounts(knocking)
	api.removeSession(knocking)
	fixture.expectSourceCounts()
}

// M3: one source block, a /24 or a /48, holds at most an eighth of
// max_sessions however many subnets it spans: 64 of the default 512, and
// never fewer than one subnet's 16. A session over its block's limit is
// refused as sessions before any room is made, so nothing is reclaimed for
// it, and a session's end gives its block's count back.
func TestLagApiPerBlockSessionLimit(t *testing.T) {
	for maxSessions, expected := range map[int]int{0: 64, 512: 64, 1024: 128, 64: 16, 2: 16} {
		fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
			settings.MaxSessions = maxSessions
		})
		if fixture.api.maxSessionsPerBlock != expected {
			t.Errorf("max_sessions %d: %d a block, expected %d", maxSessions, fixture.api.maxSessionsPerBlock, expected)
		}
	}

	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 5
	})
	api := fixture.api
	// a block of 2, so it fills across subnets under their own limit of 16
	api.stateLock.Lock()
	api.maxSessionsPerBlock = 2
	api.stateLock.Unlock()

	// three /56s of one /48, and three /29s of one /24
	v6 := []*lagApiSession{
		fixture.admissionSession("[2001:db8:1:100::1]:40000"),
		fixture.admissionSession("[2001:db8:1:200::1]:40000"),
		fixture.admissionSession("[2001:db8:1:3ff::1]:40000"),
	}
	v4 := []*lagApiSession{
		fixture.admissionSession("198.51.100.1:40000"),
		fixture.admissionSession("198.51.100.9:40000"),
		fixture.admissionSession("198.51.100.250:40000"),
	}
	for name, sessions := range map[string][]*lagApiSession{"v6": v6, "v4": v4} {
		if sessions[0].subnet == sessions[1].subnet || sessions[1].subnet == sessions[2].subnet || sessions[0].subnet == sessions[2].subnet {
			t.Errorf("%s: subnets in common", name)
		}
		if sessions[0].block != sessions[1].block || sessions[1].block != sessions[2].block {
			t.Errorf("%s: blocks differ", name)
		}
	}
	otherBlock := fixture.admissionSession("[2001:db8:2:100::1]:40000")
	if otherBlock.block == v6[0].block {
		t.Error("two /48s are one block")
	}

	for _, session := range []*lagApiSession{v6[0], v6[1], otherBlock, v4[0], v4[1]} {
		if code := api.admitSession(session); code != "" {
			t.Fatalf("admission %q", code)
		}
	}
	fixture.expectSourceCounts(v6[0], v6[1], otherBlock, v4[0], v4[1])

	// full, with a finished session to close, but each block at its limit:
	// refused before any room is made
	otherBlock.finished.Store(true)
	for _, session := range []*lagApiSession{v6[2], v4[2]} {
		if code := api.admitSession(session); code != lagApiErrorSessions {
			t.Errorf("admission over the block's limit %q", code)
		}
	}
	expectAdmittedOpen(t, "the finished session", otherBlock)
	fixture.expectSourceCounts(v6[0], v6[1], otherBlock, v4[0], v4[1])

	// a session's end gives its block's count back; full, the block under its
	// limit makes room as any session does
	api.removeSession(v6[0])
	if code := api.admitSession(v6[2]); code != "" {
		t.Fatalf("admission %q", code)
	}
	fixture.expectSourceCounts(v6[1], v6[2], otherBlock, v4[0], v4[1])
	elsewhere := fixture.admissionSession("192.0.2.1:40000")
	if code := api.admitSession(elsewhere); code != "" {
		t.Fatalf("admission %q", code)
	}
	expectReclaimed(t, "the finished session", otherBlock)
	fixture.expectSourceCounts(v6[1], v6[2], v4[0], v4[1], elsewhere)

	for _, session := range []*lagApiSession{v6[1], v6[2], v4[0], v4[1], elsewhere} {
		api.removeSession(session)
	}
	fixture.expectSourceCounts()
}
