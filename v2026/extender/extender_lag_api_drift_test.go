package extender

// The drift test's tickets on the measurement API (lag.dev/DRIFTTEST.md E3,
// E4, T1): latency alone, a report posted at once, and the demo's no report.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Leaves every load cap of a ticket at zero, as a drift test's tickets do
// (lag.dev/DRIFTTEST.md E3).
func testLagLatencyOnly(body *protocol.LagTicketBody) {
	body.DownloadMs = 0
	body.UploadMs = 0
	body.MaxStreams = 0
	body.MaxDownloadBytes = 0
	body.MaxUploadBytes = 0
}

// lag.dev/DRIFTTEST.md E3: only a ticket that leaves every load cap at zero is
// for latency alone. One that leaves only some unset takes the defaults for
// those, and admits its load, as before.
func TestLagApiLatencyOnly(t *testing.T) {
	body := &protocol.LagTicketBody{}
	testLagLatencyOnly(body)
	if !lagApiLatencyOnly(body) {
		t.Fatal("a ticket with every cap at zero is not for latency alone")
	}
	for name, set := range map[string]func(body *protocol.LagTicketBody){
		"DownloadMs":       func(body *protocol.LagTicketBody) { body.DownloadMs = 1 },
		"UploadMs":         func(body *protocol.LagTicketBody) { body.UploadMs = 1 },
		"MaxStreams":       func(body *protocol.LagTicketBody) { body.MaxStreams = 1 },
		"MaxDownloadBytes": func(body *protocol.LagTicketBody) { body.MaxDownloadBytes = 1 },
		"MaxUploadBytes":   func(body *protocol.LagTicketBody) { body.MaxUploadBytes = 1 },
	} {
		body := &protocol.LagTicketBody{}
		set(body)
		if lagApiLatencyOnly(body) {
			t.Errorf("a ticket with only %s set is for latency alone", name)
		}
	}
}

// lag.dev/DRIFTTEST.md E3, T1: a ticket that leaves every load cap at zero, a
// drift test's, admits a session that pings as any other and refuses any load.
// The load is refused as busy with retryAfterMs 0, as a second load is, which
// the browser's client takes without ending the session; a stream is refused
// at once, without waiting for a phase that never comes. Nothing is admitted,
// and the report is the session's last minute of pings with the load refused
// (M8).
func TestLagApiLatencyOnlyTicketRefusesLoad(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	// a stream that waited for its phase would take this long to be refused
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.streamWait = 10 * time.Second
	})
	session := fixture.openHelloSession(fixture.ticket(testLagLatencyOnly))
	// the hello names the defaults, a load the browser's client parses (S5)
	if session.hello.DownloadMs != 5000 || session.hello.UploadMs != 5000 || session.hello.MaxStreams != 4 {
		t.Errorf("hello %+v", session.hello.raw)
	}
	if rtts := session.rttsFor(600 * time.Millisecond); len(rtts) < 2 {
		t.Fatalf("%d rtts before the load", len(rtts))
	}

	// asked for twice, and refused each time
	for range 2 {
		session.load("download")
		message := session.next("error", 5*time.Second)
		if message.Code != lagApiErrorBusy || message.RetryAfterMs == nil || *message.RetryAfterMs != 0 {
			t.Errorf("refusal %+v", message.raw)
		}
	}
	// the rest of a load has nothing to move on
	session.load("upload")
	session.load("done")
	if count := fixture.api.loadCountNow(); count != 0 {
		t.Errorf("%d loads", count)
	}

	startTime := time.Now()
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.hello.Session), testLagOrigin)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Errorf("download stream %d", response.StatusCode)
	}
	conn, response, err := fixture.dialWebSocket(fmt.Sprintf("/lag/v1/up?session=%s&stream=0", session.hello.Session), testLagOrigin)
	if err == nil {
		conn.Close()
		t.Error("an upload stream opened")
	} else if response == nil || response.StatusCode != http.StatusConflict {
		t.Errorf("upload stream %v", err)
	}
	if elapsed := time.Since(startTime); 5*time.Second <= elapsed {
		t.Errorf("the streams waited %s for a phase", elapsed)
	}

	// the pings go on: what came before is passed over, then a second's worth
	session.rttsFor(300 * time.Millisecond)
	if rtts := session.rttsFor(time.Second); len(rtts) < 2 {
		t.Errorf("%d rtts after the refusals", len(rtts))
	}
	// and the upload and done drew nothing
	select {
	case message := <-session.messages:
		t.Errorf("unexpected %s %+v", message.Type, message.raw)
	default:
	}
	session.close()
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_BUSY ||
		body.DownloadMs != 0 || body.UploadMs != 0 || body.DownloadStreams != 0 || body.UploadStreams != 0 ||
		len(body.DownloadSentBytes) != 0 || len(body.DownloadReceivedBytes) != 0 || len(body.UploadBytes) != 0 {
		t.Errorf("report %v", body)
	}
	if body.WindowStartTimeMs != body.SessionStartTimeMs || len(body.PingMicros) < 4 {
		t.Errorf("window %d session %d pings %d", body.WindowStartTimeMs, body.SessionStartTimeMs, len(body.PingMicros))
	}
	fixture.noReport(100 * time.Millisecond)
}

// lag.dev/DRIFTTEST.md E4, T1, from the ticket to the operator through the
// lifecycle's queue on the test clock. The report of a session whose ticket
// says ReportPromptly is posted as soon as the session closes, with no wait
// for its batch. A demo's session, whose ticket says NoReport, signs and posts
// nothing. A session whose ticket says neither, as every ticket reads to an
// extender that predates the flags, waits for its batch as before.
func TestLagApiDriftTicketReports(t *testing.T) {
	clock := newTestLagClock()
	operator := newTestLagApi(t)
	lifecycle := newTestLagLifecycle(t, operator, clock, nil, nil)
	fixture := newLagApiFixture(t, nil)
	// the lifecycle's queue in place of the sink, before any session opens
	fixture.api.reports = lifecycle

	// the tickets of the reports in the operator's next post, each report
	// verified as this extender's
	postedTicketIds := func() [][]byte {
		t.Helper()
		ticketIds := [][]byte{}
		for _, postedReport := range operator.waitPost() {
			reportBytes, err := base64.StdEncoding.DecodeString(postedReport)
			if err != nil {
				t.Fatal(err)
			}
			report := &protocol.LagReport{}
			if err := proto.Unmarshal(reportBytes, report); err != nil {
				t.Fatal(err)
			}
			body, err := connect.VerifyLagReport(report, fixture.identityPublicKey)
			if err != nil {
				t.Fatal(err)
			}
			ticketIds = append(ticketIds, body.TicketId)
		}
		return ticketIds
	}
	driftTicket := func(ticketId []byte, configure func(body *protocol.LagTicketBody)) string {
		return fixture.ticket(func(body *protocol.LagTicketBody) {
			body.TicketId = ticketId
			testLagLatencyOnly(body)
			configure(body)
		})
	}

	demoTicketId := testLagRandomBytes(16)
	demo := fixture.openHelloSession(driftTicket(demoTicketId, func(body *protocol.LagTicketBody) {
		body.NoReport = true
	}))
	promptTicketId := testLagRandomBytes(16)
	prompt := fixture.openHelloSession(driftTicket(promptTicketId, func(body *protocol.LagTicketBody) {
		body.ReportPromptly = true
	}))
	for _, session := range []*lagTestSession{demo, prompt} {
		if rtts := session.rttsFor(600 * time.Millisecond); len(rtts) < 2 {
			t.Fatalf("%d rtts", len(rtts))
		}
	}

	// the demo's session is gone, report and all, before the prompt one
	// closes, so a report of the demo's would be queued first and posted with
	// the prompt one
	demo.close()
	waitFor(t, func() bool {
		return fixture.api.sessionCountNow() == 1
	})
	prompt.close()
	if ticketIds := postedTicketIds(); len(ticketIds) != 1 || !bytes.Equal(ticketIds[0], promptTicketId) {
		t.Fatalf("posted %x, expected the prompt session's report alone", ticketIds)
	}
	operator.expectQuiet()

	plainTicketId := testLagRandomBytes(16)
	plain := fixture.openHelloSession(driftTicket(plainTicketId, func(body *protocol.LagTicketBody) {}))
	plain.rttsFor(600 * time.Millisecond)
	plain.close()
	if wait := clock.nextWait(t); wait != 10*time.Second {
		t.Fatalf("flush wait = %s, expected the flush timeout", wait)
	}
	operator.expectQuiet()
	clock.advance(10 * time.Second)
	if ticketIds := postedTicketIds(); len(ticketIds) != 1 || !bytes.Equal(ticketIds[0], plainTicketId) {
		t.Fatalf("posted %x, expected the plain session's report", ticketIds)
	}
	operator.expectQuiet()
}

// lag.dev/DRIFTTEST.md E4, W8: a session whose ticket says NoReport signs and
// sends no report however it ends: closed by the browser, or by the extender
// when its page stops answering, and after a load it asked for was refused.
func TestLagApiNoReport(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.pingLost = 100 * time.Millisecond
		timings.pongTimeout = 400 * time.Millisecond
	})
	demoTicket := func() string {
		return fixture.ticket(func(body *protocol.LagTicketBody) {
			testLagLatencyOnly(body)
			body.NoReport = true
		})
	}

	closed := fixture.openHelloSession(demoTicket())
	if rtts := closed.rttsFor(300 * time.Millisecond); len(rtts) < 1 {
		t.Fatalf("%d rtts", len(rtts))
	}
	closed.load("download")
	closed.next("error", 5*time.Second)
	closed.close()

	timedOut := fixture.openHelloSession(demoTicket())
	timedOut.dropPongs.Store(true)
	if code, _ := timedOut.waitClosed(5 * time.Second); code != lagApiCloseCodeTimeout {
		t.Errorf("close %d, expected the pong timeout's", code)
	}

	waitFor(t, func() bool {
		return fixture.api.sessionCountNow() == 0
	})
	fixture.noReport(200 * time.Millisecond)

	// the same ticket without the flag reports, so the quiet above is the flag's
	reported := fixture.openHelloSession(fixture.ticket(testLagLatencyOnly))
	reported.close()
	fixture.nextReport(5 * time.Second)
}
