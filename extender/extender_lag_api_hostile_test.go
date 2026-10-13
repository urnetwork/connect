package extender

// Hostile clients of the measurement API (lag.dev/PLAN.md M2, M3, M4, T1): what
// an internet attacker can make the API hold, grow or sign.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// A raw tls connection to the measurement name, for requests no well-behaved
// client sends.
func (self *lagApiFixture) dialRaw(address string) *tls.Conn {
	self.t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{
		ServerName: testLagName,
		RootCAs:    self.rootCas,
		NextProtos: []string{"http/1.1"},
	})
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		conn.Close()
	})
	return conn
}

// Reads one response from a raw connection, then whether the server closed the
// connection after it rather than leaving it open.
func testLagRawResponse(t *testing.T, conn net.Conn) (*http.Response, bool) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = reader.ReadByte()
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return response, false
	}
	return response, true
}

// M2: a request that declares a body it never sends is refused at once and its
// connection closed, so it holds none of the connection slots every carrier
// shares. net/http would otherwise wait, with no deadline, to discard the body
// after the handler answered.
func TestLagApiDeclaredBodyRefused(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	address := fixture.serveExtender(nil)
	for name, head := range map[string]string{
		"chunked": "GET /lag/v1/down?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0 HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\nTransfer-Encoding: chunked\r\n\r\n",
		"length":  "OPTIONS /lag/v1/session HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\nContent-Length: 100\r\n\r\n",
		"upgrade": "GET /lag/v1/session?ticket=x HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nContent-Length: 10\r\n\r\n",
	} {
		conn := fixture.dialRaw(address)
		fmt.Fprintf(conn, head, testLagName)
		response, closed := testLagRawResponse(t, conn)
		// ReadResponse takes the Connection: close header into Close
		if response.StatusCode != http.StatusBadRequest || !response.Close ||
			response.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %v", name, response.StatusCode, response.Header)
		}
		if !closed {
			t.Errorf("%s: the connection was held open", name)
		}
		conn.Close()
	}
	waitFor(t, func() bool {
		return fixture.server.ConnectionCount() == 0
	})
}

// M2: every response that is not a success closes its connection, and a
// success keeps it for the next request.
func TestLagApiRefusalsCloseConnection(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	for name, request := range map[string]struct {
		head   string
		status int
	}{
		"path":    {head: "GET /elsewhere HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\n\r\n", status: http.StatusNotFound},
		"origin":  {head: "GET /lag/v1/down?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0 HTTP/1.1\r\nHost: %s\r\nOrigin: https://evil.example\r\n\r\n", status: http.StatusForbidden},
		"method":  {head: "DELETE /lag/v1/down HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\n\r\n", status: http.StatusMethodNotAllowed},
		"session": {head: "GET /lag/v1/down?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0 HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\n\r\n", status: http.StatusNotFound},
		"query":   {head: "GET /lag/v1/down?session=x&stream=0 HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\n\r\n", status: http.StatusBadRequest},
		"upload":  {head: "GET /lag/v1/up?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0 HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\n\r\n", status: http.StatusNotFound},
		"websocket": {
			head:   "GET /lag/v1/session?ticket=x HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\n\r\n",
			status: http.StatusBadRequest,
		},
	} {
		conn := fixture.dialRaw(fixture.address)
		fmt.Fprintf(conn, request.head, testLagName)
		response, closed := testLagRawResponse(t, conn)
		if response.StatusCode != request.status || !response.Close {
			t.Errorf("%s: %d %v", name, response.StatusCode, response.Header)
		}
		if !closed {
			t.Errorf("%s: the connection was left open", name)
		}
	}

	// a preflight succeeds and keeps its connection
	conn := fixture.dialRaw(fixture.address)
	preflight := "OPTIONS /lag/v1/down HTTP/1.1\r\nHost: %s\r\nOrigin: https://lag.dev\r\nAccess-Control-Request-Method: GET\r\n\r\n"
	fmt.Fprintf(conn, preflight, testLagName)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	for i := 0; i < 2; i += 1 {
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("preflight %d: %v", i, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent || response.Close {
			t.Errorf("preflight %d: %d %v", i, response.StatusCode, response.Header)
		}
		if i == 0 {
			fmt.Fprintf(conn, preflight, testLagName)
		}
	}
}

// M2: a read deadline bounds every request, its head included, and a download,
// which outlives it, clears it for itself rather than being cut by it.
func TestLagApiReadDeadline(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	if fixture.api.httpServer.ReadTimeout != fixture.server.settings.HeaderTimeout {
		t.Errorf("read timeout %s", fixture.api.httpServer.ReadTimeout)
	}
	// shorter, before the server starts, so the test need not wait it out
	fixture.api.httpServer.ReadTimeout = 500 * time.Millisecond
	fixture.api.httpServer.ReadHeaderTimeout = 500 * time.Millisecond

	session := fixture.directSession(1)
	phase := fixture.directPhase(session, lagApiDirectionDownload, 1500*time.Millisecond, 1<<40)
	startTime := time.Now()
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.id), testLagOrigin)
	n, _ := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if elapsed := time.Since(startTime); elapsed < 1400*time.Millisecond || n == 0 {
		t.Errorf("downloaded %d in %s", n, elapsed)
	}
	if _, _, end := phase.state(); end != lagApiPhaseEndTime {
		t.Errorf("ended %d", end)
	}

	// a head that never ends
	conn := fixture.dialRaw(fixture.address)
	fmt.Fprintf(conn, "GET /lag/v1/down HTTP/1.1\r\nHost: %s\r\n", testLagName)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	startTime = time.Now()
	io.Copy(io.Discard, conn)
	if elapsed := time.Since(startTime); 3*time.Second < elapsed {
		t.Errorf("a head that never ended was held %s", elapsed)
	}
}

// M2: one source subnet holds a bounded share of the measurement connections,
// so keep-alive and idle connections cannot pin the slots every carrier
// shares.
func TestLagApiSubnetConnectionCap(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	if fixture.api.maxConnectionsPerSubnet != 32 {
		t.Errorf("cap %d", fixture.api.maxConnectionsPerSubnet)
	}
	fixture.api.maxConnectionsPerSubnet = 3
	dial := func() (*tls.Conn, error) {
		return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", fixture.address, &tls.Config{
			ServerName: testLagName,
			RootCAs:    fixture.rootCas,
		})
	}
	conns := []*tls.Conn{}
	for i := 0; i < 3; i += 1 {
		conn, err := dial()
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	if conn, err := dial(); err == nil {
		conn.Close()
		t.Error("a connection past the subnet's share")
	}
	conns[0].Close()
	waitFor(t, func() bool {
		conn, err := dial()
		if err != nil {
			return false
		}
		conns[0] = conn
		return true
	})
	for _, conn := range conns {
		conn.Close()
	}
}

// The live children of a cancel context, read through its internals: what a
// context that is never canceled leaves behind.
func testLagContextChildCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	value := reflect.ValueOf(ctx)
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	children := value.FieldByName("children")
	if !children.IsValid() {
		t.Fatalf("%T has no children", ctx)
	}
	return children.Len()
}

// M3: a refused admission releases everything it built, so replaying one
// ticket, or knocking while the sessions are full, grows nothing, however many
// times it is refused.
func TestLagApiRefusedAdmissionsGrowNothing(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 1
	})
	ticketText := fixture.ticket(nil)
	fixture.openHelloSession(ticketText)
	childCount := testLagContextChildCount(t, fixture.api.ctx)

	refuse := func(ticketText string, code string) {
		conn, _, err := fixture.dialWebSocket("/lag/v1/session?ticket="+url.QueryEscape(ticketText), testLagOrigin)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		message := testLagMessage{}
		if err := conn.ReadJSON(&message); err != nil || message.Type != "error" || message.Code != code {
			t.Fatalf("refused as %+v: %v", message, err)
		}
	}
	for i := 0; i < 100; i += 1 {
		// the used ticket, replayed
		refuse(ticketText, lagApiErrorReused)
		// a fresh ticket, while the sessions are full
		refuse(fixture.ticket(nil), lagApiErrorSessions)
	}
	waitFor(t, func() bool {
		return testLagContextChildCount(t, fixture.api.ctx) <= childCount
	})
	if usedCount := fixture.api.usedTicketOrder.Len(); usedCount != 1 {
		t.Errorf("%d used tickets", usedCount)
	}
}

// M3: one source subnet holds at most 16 open sessions and 1 load at a time,
// and each admission and each load is one action under the extender's
// per-subnet admission limit (A12).
func TestLagApiPerSubnetLimits(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	if fixture.api.maxSessionsPerSubnet != 16 || fixture.api.maxLoadsPerSubnet != 1 {
		t.Errorf("per subnet %d sessions, %d loads", fixture.api.maxSessionsPerSubnet, fixture.api.maxLoadsPerSubnet)
	}

	// 16 sessions, and the 17th is refused
	sessions := []*lagTestSession{}
	for i := 0; i < 16; i += 1 {
		sessions = append(sessions, fixture.openHelloSession(fixture.ticket(nil)))
	}
	refused := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	if message := refused.next("error", 5*time.Second); message.Code != lagApiErrorSessions {
		t.Errorf("17th session: %+v", message.raw)
	}
	if code, _ := refused.waitClosed(5 * time.Second); code != lagApiCloseCodeLimited {
		t.Errorf("17th session closed %d", code)
	}

	// one load at a time, though the extender runs two
	sessions[0].load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	sessions[1].load("download")
	if message := sessions[1].next("error", 5*time.Second); message.Code != lagApiErrorBusy ||
		message.RetryAfterMs == nil || *message.RetryAfterMs != 5000 {
		t.Errorf("second load of the subnet: %+v", message.raw)
	}
	// a session that closes frees its place
	sessions[15].close()
	fixture.nextReport(5 * time.Second)
	fixture.openHelloSession(fixture.ticket(nil))
}

// M3, A12: each admission and each load is one action of its subnet: past the
// subnet's actions a minute, a session is refused as sessions and a load as
// busy, with the admission limit's wait.
func TestLagApiPerSubnetActions(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.server.settings.AdmissionActionsPerSubnetPerMinute = 3
	first := fixture.openHelloSession(fixture.ticket(nil))
	second := fixture.openHelloSession(fixture.ticket(nil))
	// the third action: a load
	first.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	first.load("done")
	if message := first.nextResult(5 * time.Second); message.Type != "loadResult" {
		t.Fatalf("result %+v", message.raw)
	}
	// the fourth: refused, a load as busy with the admission's wait
	second.load("download")
	message := second.next("error", 5*time.Second)
	if message.Code != lagApiErrorBusy || message.RetryAfterMs == nil || *message.RetryAfterMs < 15000 {
		t.Errorf("load past the subnet's actions: %+v", message.raw)
	}
	// and a session as sessions
	refused := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	if message := refused.next("error", 5*time.Second); message.Code != lagApiErrorSessions {
		t.Errorf("session past the subnet's actions: %+v", message.raw)
	}
}
