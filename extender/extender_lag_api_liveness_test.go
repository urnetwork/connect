package extender

// The page's liveness (lag.dev/PLAN.md M4). A session ends as the page gone
// when no pong has answered a ping for the pong timeout. Any pong that echoes
// a ping sent within that timeout counts as the page being there, including
// one that arrives after its ping was counted lost. Only a pong for the ping
// in flight, within the loss timeout, counts as a round trip.

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// M4: a page that answers every ping, each later than the loss timeout, is
// still there. Every ping counts as lost, and no late pong counts as a round
// trip, but the late pongs keep the session open past the pong timeout. Once
// the pongs stop, the session ends as the page gone.
func TestLagApiLatePongsKeepTheSession(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.pingLost = 300 * time.Millisecond
		timings.pongTimeout = 1200 * time.Millisecond
	})
	session := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	// every pong arrives 400 ms after its ping, past the loss timeout. The
	// session is already open, so a first ping may be answered at once before
	// this applies, and only a round trip within the loss timeout may count.
	session.pongDelay.Store(int64(400 * time.Millisecond))
	session.hello = session.next("hello", 5*time.Second)
	lateRtt := func(ms float64) bool {
		return 300 <= ms
	}

	// three pong timeouts
	rtts := session.rttsFor(3600 * time.Millisecond)
	select {
	case <-session.done:
		t.Fatalf("closed %d %q with every ping answered", session.closeCode, session.closeText)
	default:
	}
	lostCount := 0
	for _, rtt := range rtts {
		switch {
		case rtt.Lost:
			lostCount += 1
		case rtt.Ms == nil || lateRtt(*rtt.Ms):
			t.Errorf("a late pong counted as a round trip: %+v", rtt.raw)
		}
	}
	if lostCount < 6 {
		t.Fatalf("%d of %d rtts lost", lostCount, len(rtts))
	}

	session.dropPongs.Store(true)
	if code, text := session.waitClosed(5 * time.Second); code != lagApiCloseCodeTimeout || text != "timeout" {
		t.Errorf("close %d %q", code, text)
	}
	body := fixture.nextReport(5 * time.Second)
	for _, pingMicros := range body.PingMicros {
		if lateRtt(float64(pingMicros) / 1000) {
			t.Errorf("a late pong of %d us in the report", pingMicros)
		}
	}
}

// M4: a late pong counts for liveness only while its ping was sent within the
// pong timeout. A client that never answers a ping, and replays the pong of
// one lost ping over and over, keeps the session open until that ping is a
// pong timeout old, and is then ended as the page gone. No replay counts as a
// round trip.
func TestLagApiReplayedLatePongExpires(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.pingLost = 300 * time.Millisecond
		timings.pongTimeout = 1500 * time.Millisecond
	})
	conn, _, err := fixture.dialWebSocket(
		"/lag/v1/session?ticket="+url.QueryEscape(fixture.ticket(nil)),
		testLagOrigin,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pings := make(chan string, 64)
	conn.SetPingHandler(func(appData string) error {
		select {
		case pings <- appData:
		default:
		}
		return nil
	})
	messages := make(chan testLagMessage, 64)
	closeCodes := make(chan int, 1)
	go func() {
		for {
			_, messageBytes, err := conn.ReadMessage()
			if err != nil {
				closeError := &websocket.CloseError{}
				if errors.As(err, &closeError) {
					closeCodes <- closeError.Code
				} else {
					closeCodes <- 0
				}
				return
			}
			message := testLagMessage{}
			if json.Unmarshal(messageBytes, &message) == nil {
				json.Unmarshal(messageBytes, &message.raw)
				messages <- message
			}
		}
	}()
	nextMessage := func(messageType string) testLagMessage {
		t.Helper()
		for {
			select {
			case message := <-messages:
				if message.Type == messageType {
					return message
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s message", messageType)
				return testLagMessage{}
			}
		}
	}
	hello := nextMessage("hello")
	sessionId, ok := parseLagApiSessionId(hello.Session)
	if !ok {
		t.Fatalf("hello %+v", hello.raw)
	}
	serverSession := fixture.api.session(sessionId)
	if serverSession == nil {
		t.Fatal("no open session")
	}

	var firstPayload string
	select {
	case firstPayload = <-pings:
	case <-time.After(5 * time.Second):
		t.Fatal("no ping")
	}
	if rtt := nextMessage("rtt"); !rtt.Lost {
		t.Fatalf("the first ping was answered: %+v", rtt.raw)
	}
	// the first ping is lost: replay its pong until the session ends
	replayDone := make(chan struct{})
	defer close(replayDone)
	go func() {
		for {
			select {
			case <-replayDone:
				return
			case <-time.After(50 * time.Millisecond):
				conn.WriteControl(websocket.PongMessage, []byte(firstPayload), time.Now().Add(time.Second))
			}
		}
	}()
	// the replays count for liveness at first
	waitFor(t, func() bool {
		return 0 < serverSession.lastPongSinceStart.Load()
	})

	select {
	case code := <-closeCodes:
		if code != lagApiCloseCodeTimeout {
			t.Errorf("close %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("replaying one lost ping's pong kept the session open")
	}
	if lastPongTime := serverSession.lastPongTime(); serverSession.startTime.Add(2 * time.Second).Before(lastPongTime) {
		t.Errorf("a replay counted %s after the session started", lastPongTime.Sub(serverSession.startTime))
	}
	for {
		select {
		case message := <-messages:
			if message.Type == "rtt" && (!message.Lost || message.Ms != nil) {
				t.Errorf("a replay counted as a round trip: %+v", message.raw)
			}
			continue
		default:
		}
		break
	}
	fixture.nextReport(5 * time.Second)
}
