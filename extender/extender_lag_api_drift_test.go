package extender

// The drift test's tickets on the measurement API (lag.dev/DRIFTTEST.md E3,
// T1): latency alone.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
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
