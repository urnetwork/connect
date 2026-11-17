// What the extender's http servers leave in the process's logs: no client's address and
// nothing else a connection sent (extender_error.go). Each case is a real client on a real
// socket, doing what makes net/http, http2 or http3 write a line.

package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// What a panic's value holds in these tests, which no line may repeat.
const testPanicValue = "203.0.113.7:44444 failed"

// Everything the process's logs receive during a test: the standard log, which the http
// servers and slog write to, and the default logger. Safe for concurrent use.
type testProcessLog struct {
	connect.Logger
	stateLock sync.Mutex
	lines     bytes.Buffer
}

func (self *testProcessLog) write(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines.WriteString(line)
	self.lines.WriteString("\n")
}

func (self *testProcessLog) Write(b []byte) (int, error) {
	self.write(string(b))
	return len(b), nil
}

func (self *testProcessLog) Info(args ...any) {
	self.write(fmt.Sprint(args...))
}

func (self *testProcessLog) Infof(format string, args ...any) {
	self.write(fmt.Sprintf(format, args...))
}

func (self *testProcessLog) Warningf(format string, args ...any) {
	self.write(fmt.Sprintf(format, args...))
}

func (self *testProcessLog) Errorf(format string, args ...any) {
	self.write(fmt.Sprintf(format, args...))
}

func (self *testProcessLog) text() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lines.String()
}

// Takes the process's logs for the rest of the test.
func captureTestProcessLog(t *testing.T) *testProcessLog {
	t.Helper()
	processLog := &testProcessLog{Logger: connect.NewNoopLogger()}
	previousWriter := log.Writer()
	log.SetOutput(processLog)
	previousLogger := connect.DefaultLogger()
	connect.SetDefaultLogger(processLog)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		connect.SetDefaultLogger(previousLogger)
	})
	return processLog
}

// Fails the test if the logs hold any of the text. A line a server writes after what the
// client saw gets a moment to arrive; that wait can only find more, and never fails a log
// that holds none of it.
func testExpectLogHoldsNone(t *testing.T, processLog *testProcessLog, notHeard ...string) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		text := processLog.text()
		for _, forbidden := range notHeard {
			if strings.Contains(text, forbidden) {
				t.Fatalf("the log holds %q:\n%s", forbidden, text)
			}
		}
		if deadline.Before(time.Now()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// What a client's socket is known by, the same as the server knows it.
func testClientAddressTexts(conn net.Conn) []string {
	host, port, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		panic(err)
	}
	return []string{conn.LocalAddr().String(), port, host}
}

// A tcp carrier client past its handshake, offering h2.
func dialTestH2ExtenderConn(t *testing.T, fixture *extenderFixture) *tls.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", fixture.authority(fixture.tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         testServerName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{http2.NextProtoTLS},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tlsConn.Close()
	})
	if err := tlsConn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return tlsConn
}

// The extender request of a forward on the tcp carrier, written by hand so the client's
// socket is at hand.
func writeTestExtenderRequest(t *testing.T, conn net.Conn) {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		"https://"+testServerName+"/",
		bytes.NewReader(testExtenderHeaderBytes(t, "dest.example", 443, testSecret)),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", connect.ExtenderContentType)
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
}

// What http2 logs of a client that breaks the protocol: the line names the client, and
// the greeting's bytes are the client's too.
func TestExtenderHttp2ProtocolErrorsLogNoClientAddress(t *testing.T) {
	for _, c := range []struct {
		description string
		sent        []byte
	}{
		{
			description: "a greeting that is not the preface",
			sent:        []byte("GET / HTTP/1.1\r\nX-Y: zzz"),
		},
		{
			description: "a frame that cannot be read",
			// a ping of one byte
			sent: append([]byte(http2.ClientPreface), 0, 0, 1, 6, 0, 0, 0, 0, 0, 0),
		},
	} {
		t.Run(c.description, func(t *testing.T) {
			processLog := captureTestProcessLog(t)
			fixture := newExtenderFixture(t, "127.0.0.1", nil)
			conn := dialTestH2ExtenderConn(t, fixture)
			if _, err := conn.Write(c.sent); err != nil {
				t.Fatal(err)
			}
			// the server logs this before it closes, so the line is in by the end
			io.Copy(io.Discard, conn)
			testExpectLogHoldsNone(t, processLog, append(testClientAddressTexts(conn), "http2", "zzz")...)
		})
	}
}

// Whatever else net/http and http2 have to say goes the same way, such as a client that
// never sends its settings: their log is nowhere at all.
func TestExtenderHttpServerLogGoesNowhere(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewExtenderServer(ctx, nil, nil, nil, nil, DefaultExtenderSettings())
	defer server.CloseAndWait()
	if writer := server.httpServer.ErrorLog.Writer(); writer != io.Discard {
		t.Fatalf("the http server's log writes to %T", writer)
	}
}

// Each http server's own record of a panic in the handler names the client or repeats the
// panic's value. Neither reaches the log. What does is that a handler panicked and where,
// and the handler hears its class, so a bug in a handler does not go unseen. Each carrier
// serves the next request.
func TestExtenderHandlerPanicLogsWhereAndNoClientAddress(t *testing.T) {
	// the first call that reaches this panics, on whichever server called it
	newPanicOnce := func() func() {
		panicked := atomic.Bool{}
		return func() {
			if panicked.CompareAndSwap(false, true) {
				panic(errors.New(testPanicValue))
			}
		}
	}
	expectPanicReported := func(t *testing.T, processLog *testProcessLog, fixture *extenderFixture, notHeard ...string) {
		t.Helper()
		testExpectLogHoldsNone(t, processLog, append(notHeard, testPanicValue, "panic serving")...)
		reported := waitForFixtureError(t, fixture, "handler panic")
		testExpectErrorHoldsNone(t, errors.Unwrap(reported), testErrorNotHeard...)
		text := processLog.text()
		if !strings.Contains(text, "panicked at ") || !strings.Contains(text, "extender_") {
			t.Fatalf("the log does not say where a handler panicked:\n%s", text)
		}
		t.Logf("the log says: %s", text)
	}

	t.Run("http/1.1", func(t *testing.T) {
		processLog := captureTestProcessLog(t)
		panicOnce := newPanicOnce()
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.HeaderHandler = func(*protocol.ExtenderHeader) {
				panicOnce()
			}
		})
		conn := dialHandshakedExtenderConn(t, fixture)
		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		writeTestExtenderRequest(t, conn)
		// the server logs this before it closes, so the line is in by the end
		io.Copy(io.Discard, conn)
		expectPanicReported(t, processLog, fixture, testClientAddressTexts(conn)...)

		next := dialHandshakedExtenderConn(t, fixture)
		if err := next.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		writeTestExtenderRequest(t, next)
		responseBytes := make([]byte, 12)
		if _, err := io.ReadFull(next, responseBytes); err != nil || !strings.Contains(string(responseBytes), "200") {
			t.Fatalf("the next request got %q, %v", responseBytes, err)
		}
	})

	t.Run("h2", func(t *testing.T) {
		processLog := captureTestProcessLog(t)
		panicOnce := newPanicOnce()
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			recordError := settings.ErrorHandler
			settings.ErrorHandler = func(stage string, err error) {
				// an extender request over h2 is refused, which reports a stage
				if stage == "request" {
					panicOnce()
				}
				recordError(stage, err)
			}
		})
		conn := dialTestH2ExtenderConn(t, fixture)
		clientConn, err := (&http2.Transport{}).NewClientConn(conn)
		if err != nil {
			t.Fatal(err)
		}
		defer clientConn.Close()
		post := func() (*http.Response, error) {
			request, err := http.NewRequest(
				http.MethodPost,
				"https://"+testServerName+"/",
				bytes.NewReader(testExtenderHeaderBytes(t, "dest.example", 443, testSecret)),
			)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", connect.ExtenderContentType)
			return clientConn.RoundTrip(request)
		}
		if response, err := post(); err == nil {
			response.Body.Close()
			t.Fatalf("the request that panicked got status %d", response.StatusCode)
		}
		expectPanicReported(t, processLog, fixture, testClientAddressTexts(conn)...)

		// a panic is the stream's, so the connection serves the next one
		response, err := post()
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("the next request got status %d, expected the refusal", response.StatusCode)
		}
	})

	// the servers' own way to end a request, which is nothing to report
	t.Run("an aborted request", func(t *testing.T) {
		processLog := captureTestProcessLog(t)
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.HeaderHandler = func(*protocol.ExtenderHeader) {
				panic(http.ErrAbortHandler)
			}
		})
		conn := dialHandshakedExtenderConn(t, fixture)
		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		writeTestExtenderRequest(t, conn)
		io.Copy(io.Discard, conn)
		testExpectLogHoldsNone(t, processLog, append(testClientAddressTexts(conn), "panic")...)
		for _, err := range drainFixtureErrors(fixture) {
			if strings.HasPrefix(err.Error(), "handler panic:") {
				t.Fatalf("heard %v", err)
			}
		}
	})

	t.Run("h3", func(t *testing.T) {
		processLog := captureTestProcessLog(t)
		panicOnce := newPanicOnce()
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.HeaderHandler = func(*protocol.ExtenderHeader) {
				panicOnce()
			}
		})
		dial := func() (net.Conn, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			conn, _, err := connect.DialExtender(ctx, fixture.connectSettings(), fixture.extenderConfig(connect.ExtenderCarrierQuic), &connect.ExtenderDial{
				DestinationHost: "dest.example",
				DestinationPort: 443,
			})
			return conn, err
		}
		if conn, err := dial(); err == nil {
			conn.Close()
			t.Fatal("the request that panicked was answered")
		}
		expectPanicReported(t, processLog, fixture, "http3")

		conn, err := dial()
		if err != nil {
			t.Fatalf("the next request: %v", err)
		}
		conn.Close()
	})
}
