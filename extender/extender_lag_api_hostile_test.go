package extender

// Hostile clients of the measurement API (lag.dev/PLAN.md M2, M3, M4, T1): what
// an internet attacker can make the API hold, grow or sign.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
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
		conn := fixture.dialRaw(fixture.dialAddress())
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
	conn := fixture.dialRaw(fixture.dialAddress())
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
	conn := fixture.dialRaw(fixture.dialAddress())
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
		return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", fixture.dialAddress(), &tls.Config{
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

// The live children of a cancel context, read through its internals, under
// its own lock: what a context that is never canceled leaves behind.
func testLagContextChildCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	value := reflect.ValueOf(ctx)
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	children := value.FieldByName("children")
	lock := value.FieldByName("mu")
	if !children.IsValid() || !lock.IsValid() || !lock.CanAddr() {
		t.Fatalf("%T has no children to count", ctx)
	}
	mutex := (*sync.Mutex)(unsafe.Pointer(lock.UnsafeAddr()))
	mutex.Lock()
	defer mutex.Unlock()
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

// M1: under camouflage splicing, a name under the measurement zone that the
// API is not serving now -- another id, or any while no certificate is
// installed -- is spliced to a borrowed site exactly as an unknown name is, so
// a prober cannot tell an extender that keeps a measurement zone. With
// splicing off it is closed, and only the name being served reaches the API.
func TestLagApiZoneNamesLookUnknown(t *testing.T) {
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	for _, splice := range []bool{true, false} {
		t.Run(fmt.Sprintf("splice %t", splice), func(t *testing.T) {
			site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
			fixture := newLagApiFixture(t, nil)
			fixture.server.measurementZone = "extender.example"
			address := fixture.serveExtender(func(settings *ExtenderSettings) {
				settings.CamouflageEnabled = true
				settings.ExtenderCamouflageSplice = splice
				settings.CamouflageSpliceDialContext = site.dialContext()
				// copied from the settings when the server was built: set
				// before it serves
				fixture.server.camouflage.borrowNames = []string{testBorrowName}
			})
			// the leaf a name is answered with: its organization, or "" when
			// there was no handshake
			answer := func(serverName string) string {
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{
					ServerName:         serverName,
					InsecureSkipVerify: true,
				})
				if err != nil {
					return ""
				}
				defer conn.Close()
				leaf := conn.ConnectionState().PeerCertificates[0]
				if leaf.Equal(fixture.certificate.Leaf) {
					return "measurement"
				}
				if 0 < len(leaf.Subject.Organization) {
					return leaf.Subject.Organization[0]
				}
				return "?"
			}
			unknown := answer("www.random.example")
			if splice && unknown != "Borrow Test" {
				t.Fatalf("an unknown name: %q", unknown)
			}
			if answer(testLagName) != "measurement" {
				t.Error("the name served did not reach the api")
			}
			otherZoneName := "0192e4a1-0000-7000-8000-000000000009.extender.example"
			expected := ""
			if splice {
				expected = "Borrow Test"
			}
			if got := answer(otherZoneName); got != expected {
				t.Errorf("another zone name: %q, expected %q", got, expected)
			}
			fixture.certificates.set(nil, nil)
			for _, serverName := range []string{testLagName, otherZoneName} {
				if got := answer(serverName); got != expected {
					t.Errorf("%s with no certificate: %q, expected %q", serverName, got, expected)
				}
			}
		})
	}
}

// M4: a client cannot forge the round trips the extender signs. Each ping
// carries 16 random bytes, and only a pong echoing the ping in flight, arriving
// after it was sent, counts: a flood of pongs sent ahead of the ping they would
// answer -- predicting its payload, guessing it, or replaying an earlier
// ping's -- changes nothing, and every round trip is the one the honest pong
// takes, here at least the 200 ms it is held for.
func TestLagApiForgedPongsNeverCount(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	conn, _, err := fixture.dialWebSocket("/lag/v1/session?ticket="+url.QueryEscape(fixture.ticket(nil)), testLagOrigin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const pathDelay = 200 * time.Millisecond
	payloads := make(chan string, 1024)
	// the payload of the ping received last, and of the one before it
	var lastPayload atomic.Pointer[string]
	var earlierPayload atomic.Pointer[string]
	conn.SetPingHandler(func(appData string) error {
		payloads <- appData
		earlierPayload.Store(lastPayload.Load())
		lastPayload.Store(&appData)
		go func() {
			time.Sleep(pathDelay)
			conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		}()
		return nil
	})
	stop := make(chan struct{})
	var flood sync.WaitGroup
	flood.Add(1)
	go func() {
		defer flood.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			// a guess, an earlier ping's payload, and, were the payloads a
			// counter, the next ping's; never the ping just received, which
			// would be an honest answer
			forged := [][]byte{testLagRandomBytes(16)}
			if earlier := earlierPayload.Load(); earlier != nil {
				forged = append(forged, []byte(*earlier))
			}
			if last := lastPayload.Load(); last != nil && len(*last) == 8 {
				next := make([]byte, 8)
				binary.BigEndian.PutUint64(next, binary.BigEndian.Uint64([]byte(*last))+1)
				forged = append(forged, next)
			}
			for _, payload := range forged {
				if err := conn.WriteControl(websocket.PongMessage, payload, time.Now().Add(time.Second)); err != nil {
					return
				}
			}
		}
	}()
	rtts := []float64{}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			message := testLagMessage{}
			if err := conn.ReadJSON(&message); err != nil {
				return
			}
			if message.Type == "rtt" && message.Ms != nil {
				rtts = append(rtts, *message.Ms)
			}
		}
	}()
	time.Sleep(3 * time.Second)
	close(stop)
	flood.Wait()
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	<-readDone
	body := fixture.nextReport(10 * time.Second)

	if len(rtts) < 3 {
		t.Fatalf("%d rtts", len(rtts))
	}
	for _, ms := range rtts {
		if ms < float64(pathDelay/time.Millisecond)-1 {
			t.Errorf("a round trip of %f ms, below the path's %s", ms, pathDelay)
		}
	}
	for _, pingMicros := range body.PingMicros {
		if pingMicros != 0 && pingMicros < uint32((pathDelay-time.Millisecond)/time.Microsecond) {
			t.Errorf("a signed round trip of %d us, below the path's %s", pingMicros, pathDelay)
		}
	}
	// the payloads: 16 bytes, none repeated
	close(payloads)
	seen := map[string]bool{}
	for payload := range payloads {
		if len(payload) != lagApiPingPayloadByteCount || seen[payload] {
			t.Errorf("payload %x", payload)
		}
		seen[payload] = true
	}
}

// M1: no carrier gives an identity leaf to a name under the measurement zone,
// with the API off as with it on: the default zone when the extender keeps no
// measurement certificate, its lifecycle's own zone when it does. The tcp and
// QUIC carriers both take their certificate through this one choice.
func TestLagApiZoneNamesNeverGetIdentityLeaves(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(configure func(settings *ExtenderSettings)) *ExtenderServer {
		settings := DefaultExtenderSettings()
		settings.IdentityKeySeed = seed
		if configure != nil {
			configure(settings)
		}
		server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
		t.Cleanup(server.CloseAndWait)
		return server
	}
	refused := func(server *ExtenderServer, serverName string) bool {
		certificate, err := server.carrierCertificate(&tls.ClientHelloInfo{ServerName: serverName})
		return err != nil && certificate == nil
	}
	measurementName := "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f." + connect.ExtenderMeasurementZone
	for name, configure := range map[string]func(settings *ExtenderSettings){
		"api off": func(settings *ExtenderSettings) {
			settings.Measurement.Enabled = false
		},
		"api on": nil,
	} {
		server := newServer(configure)
		for _, serverName := range []string{
			measurementName,
			"0192E4A1-7B3C-7D2E-9F10-5A6B7C8D9E0F.EXTENDER.BRINGYOUR.COM",
			"anything." + connect.ExtenderMeasurementZone,
		} {
			if !refused(server, serverName) {
				t.Errorf("%s: an identity leaf for %q", name, serverName)
			}
		}
		for _, serverName := range []string{"other.example", "notextender.bringyour.com", connect.ExtenderMeasurementZone} {
			if refused(server, serverName) {
				t.Errorf("%s: %q refused", name, serverName)
			}
		}
	}

	// a lifecycle's own zone
	certificate, err := NewExtenderMeasurementCert("", "Extender.Example.")
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(func(settings *ExtenderSettings) {
		settings.LagLifecycle = &ExtenderLagLifecycleSettings{Certificate: certificate}
	})
	if server.measurementZone != "extender.example" || !refused(server, "0192e4a1-1111-7000-8000-000000000001.extender.example") {
		t.Errorf("the lifecycle's zone %q", server.measurementZone)
	}

	// through the tcp accept path with the api off: a zone name is closed,
	// and an unknown name answered as ever
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server = newServer(func(settings *ExtenderSettings) {
		settings.Measurement.Enabled = false
		settings.Listen = func(network string, address string) (net.Listener, error) {
			return listener, nil
		}
	})
	server.ports = map[int][]connect.ExtenderConnectMode{
		listener.Addr().(*net.TCPAddr).Port: {connect.ExtenderConnectModeTcpTls},
	}
	go server.ListenAndServe()
	<-server.Listening()
	dial := func(serverName string) error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", listener.Addr().String(), &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true,
		})
		if err == nil {
			conn.Close()
		}
		return err
	}
	if dial(measurementName) == nil {
		t.Error("the api off, a measurement name was answered")
	}
	if err := dial("other.example"); err != nil {
		t.Errorf("an unknown name: %v", err)
	}
}

// M4: the browser's download progress counts only inside the download's
// window: a report from past it and half an interval is ignored, and none is
// binned past the window's ceil(downloadMs / intervalMs) intervals, so a report
// never carries intervals the download did not have.
func TestLagApiReceivedStaysInTheWindow(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.DownloadMs = 500
		body.UploadMs = 500
	}))
	session.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.hello.Session), testLagOrigin)
	downloadedByteCount, _ := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if downloadedByteCount < 4000 {
		t.Fatalf("downloaded %d", downloadedByteCount)
	}
	step := uint64(downloadedByteCount / 4)
	for _, report := range []struct {
		ms    int64
		bytes uint64
	}{
		{ms: 250, bytes: step},
		{ms: 500, bytes: 2 * step},
		// late by less than half an interval: counted, in the last interval
		{ms: 600, bytes: 3 * step},
		// past the window and half an interval: ignored
		{ms: 700, bytes: 4 * step},
		{ms: 2000, bytes: 4 * step},
	} {
		session.send(map[string]any{"type": "received", "bytes": report.bytes, "ms": report.ms})
	}
	// the messages are read in order, so the close comes after them
	time.Sleep(200 * time.Millisecond)
	session.close()
	body := fixture.nextReport(5 * time.Second)
	if len(body.DownloadReceivedBytes) != 2 {
		t.Fatalf("received %v", body.DownloadReceivedBytes)
	}
	if body.DownloadReceivedBytes[0] != step || body.DownloadReceivedBytes[1] != 2*step {
		t.Errorf("received %v, steps of %d", body.DownloadReceivedBytes, step)
	}
}
