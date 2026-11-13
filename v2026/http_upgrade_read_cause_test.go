// The real HTTP parser observes synthetic socket failures without losing their
// original causes or turning caller cancellation into fallback permission.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// The read boundary controls cancellation without a timer or background race.
type httpUpgradeReadFailureTestConn struct {
	*h1UpgradeScriptConn
	failure error
	cancel  context.CancelFunc
}

func (self *httpUpgradeReadFailureTestConn) Read([]byte) (int, error) {
	if self.cancel != nil {
		self.cancel()
	}
	return 0, self.failure
}

// A complete socket interruption keeps its cause and fresh negotiation policy;
// a hard original or cancellation cannot be erased by the HTTP parser.
func TestDialFramedUpgradePreservesOriginalReadCauses(t *testing.T) {
	resetH1UpgradeTestState(t)
	hard := &os.PathError{Op: "read", Path: "synthetic-original-custody", Err: io.EOF}
	for _, item := range []struct {
		name     string
		failure  error
		fallback bool
	}{
		{name: "eof", failure: io.EOF, fallback: true},
		{name: "incomplete response", failure: io.ErrUnexpectedEOF, fallback: true},
		{name: "closed network connection", failure: net.ErrClosed, fallback: true},
		{name: "unclassified closed pipe", failure: io.ErrClosedPipe},
		{name: "hard original", failure: hard},
		{name: "mixed original", failure: errors.Join(io.ErrUnexpectedEOF, hard)},
		{name: "mixed cancellation", failure: errors.Join(io.ErrUnexpectedEOF, context.Canceled)},
	} {
		connection := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: item.failure}
		result, err := DialFramedUpgrade(t.Context(), "ws://read-cause.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol)
		if result != nil {
			result.Close()
			t.Fatal("failed response returned a connection", item.name)
		}
		if !errors.Is(err, item.failure) || HTTPUpgradeAllowsFallback(err) != item.fallback || !connection.closed.Load() {
			t.Fatal("response parser lost original read custody or permission", item.name)
		}
		RecordFramedUpgradeFailure("ws://read-cause.example/", H1FramerProtocol, err)
		if !FramedUpgradePermitted("ws://read-cause.example/", H1FramerProtocol) {
			t.Fatal("physical read failure poisoned protocol capability", item.name)
		}
	}
}

// Cancellation after a physical read retains both the original failure and
// the real caller cancellation; neither is rewritten as protocol rejection.
func TestDialFramedUpgradeCancellationRetainsOriginalReadCause(t *testing.T) {
	resetH1UpgradeTestState(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hard := errors.New("synthetic owned socket integrity failure")
	connection := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: hard, cancel: cancel}
	result, err := DialFramedUpgrade(ctx, "ws://canceled-read.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol)
	if result != nil {
		result.Close()
		t.Fatal("canceled response returned a connection")
	}
	if !errors.Is(err, hard) || !errors.Is(err, context.Canceled) || HTTPUpgradeAllowsFallback(err) || !connection.closed.Load() {
		t.Fatal("cancellation erased the physical original cause or allowed fallback")
	}
}

// Foreign read graphs cross the actual parser boundary without As/Is dispatch,
// unbounded traversal or nil-receiver calls before the request collector runs.
func TestDialFramedUpgradeReadBoundsForeignCauses(t *testing.T) {
	resetH1UpgradeTestState(t)
	matcher := &httpPolicyMatcherTestError{}
	cycle := &httpPolicyCycleTestError{leaf: io.EOF}
	var nilWrapper *httpPolicyJoinedTestError
	for _, failure := range []error{
		matcher, cycle, nilWrapper,
		&httpPolicyJoinedTestError{causes: []error{io.ErrUnexpectedEOF, nil}},
	} {
		connection := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: failure}
		result, err := DialFramedUpgrade(t.Context(), "ws://foreign-read.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol)
		if result != nil {
			result.Close()
			t.Fatal("foreign failed response returned a connection")
		}
		if err == nil || HTTPUpgradeAllowsFallback(err) || !connection.closed.Load() {
			t.Fatal("foreign read graph gained fallback permission")
		}
	}
	if matcher.matches != 0 || cycle.visits > 3*httpRequestCauseNodes {
		t.Fatal("response read invoked foreign matching or unbounded traversal", matcher.matches, cycle.visits)
	}
}

// When both fresh negotiations fail, the returned graph retains the initial
// physical response failure as well as the separate WebSocket write failure.
func TestDialH1MessagesFallbackFailureRetainsBothAttempts(t *testing.T) {
	resetH1UpgradeTestState(t)
	first := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: io.ErrUnexpectedEOF}
	hard := errors.New("synthetic fallback request write failure")
	second := newH1UpgradeScriptConn(nil)
	second.writeErr = hard
	dials := 0
	dialer := &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	}}
	result, err := DialH1Messages(t.Context(), "ws://failed-fallback.example/", nil, dialer, H1FramerProtocol, 1024, true, nil)
	if result != nil {
		result.Close()
		t.Fatal("two failed negotiations returned a connection")
	}
	if dials != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, hard) || HTTPUpgradeAllowsFallback(err) {
		t.Fatal("fallback replaced an original attempt cause or authorized another downgrade", dials)
	}
	if !first.closed.Load() || !second.closed.Load() || first.writeCalls == 0 || second.writeCalls == 0 {
		t.Fatal("failed handshake ownership leaked or fixture skipped its physical writes")
	}
}

// A single close outcome belongs to the same negotiation even when the
// cancellation callback and ordinary failure cleanup both need the socket.
type httpUpgradeCloseFailureTestConn struct {
	net.Conn
	failure error
	closes  atomic.Int32
}

func (self *httpUpgradeCloseFailureTestConn) Close() error {
	self.closes.Add(1)
	return errors.Join(self.Conn.Close(), self.failure)
}

// Capability evidence requires clean custody release. A valid refusal cannot
// hide a hard close error, and an ordinary nil close still permits fallback.
func TestDialFramedUpgradeRefusalRetainsOriginalCloseCause(t *testing.T) {
	resetH1UpgradeTestState(t)
	hard := errors.New("synthetic socket close integrity failure")
	for index, closeErr := range []error{nil, hard} {
		physical := newH1UpgradeScriptConn([]byte("HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\n\r\n"))
		connection := &httpUpgradeCloseFailureTestConn{Conn: physical, failure: closeErr}
		result, err := DialFramedUpgrade(t.Context(), "ws://close-cause.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol)
		if result != nil {
			result.Close()
			t.Fatal("protocol refusal returned a connection", index)
		}
		if err == nil || HTTPUpgradeAllowsFallback(err) != (closeErr == nil) || connection.closes.Load() != 1 || !physical.closed.Load() {
			t.Fatal("refusal lost its owned close outcome", index, connection.closes.Load())
		}
		if closeErr != nil && !errors.Is(err, closeErr) {
			t.Fatal("refusal erased original close cause")
		}
		if closeErr != nil {
			RecordFramedUpgradeFailure("ws://close-cause.example/", H1FramerProtocol, err)
			if !FramedUpgradePermitted("ws://close-cause.example/", H1FramerProtocol) {
				t.Fatal("close failure became negative capability evidence")
			}
		}
	}
}

// Caller cancellation and a simultaneous read/close failure preserve all three
// originals, while shared cleanup closes the owned connection exactly once.
func TestDialFramedUpgradeCanceledClosePreservesCallerCause(t *testing.T) {
	resetH1UpgradeTestState(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stopCause := errors.New("synthetic caller custody stop")
	readCause := errors.New("synthetic original read failure")
	closeCause := errors.New("synthetic original close failure")
	physical := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: readCause, cancel: func() { cancel(stopCause) }}
	connection := &httpUpgradeCloseFailureTestConn{Conn: physical, failure: closeCause}
	result, err := DialFramedUpgrade(ctx, "ws://canceled-close.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol)
	if result != nil {
		result.Close()
		t.Fatal("canceled negotiation returned a connection")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, stopCause) || !errors.Is(err, readCause) || !errors.Is(err, closeCause) ||
		HTTPUpgradeAllowsFallback(err) || connection.closes.Load() != 1 {
		t.Fatal("canceled negotiation lost an original cause or closed twice", connection.closes.Load())
	}
}

// A custom dial that returns both a connection and an error still transfers
// failed-connection cleanup to this caller, including the original close cause.
func TestDialFramedUpgradeFailedDialRetainsOriginalCloseCause(t *testing.T) {
	resetH1UpgradeTestState(t)
	dialCause := errors.New("synthetic original dial failure")
	closeCause := errors.New("synthetic failed-dial close failure")
	connection := &httpUpgradeCloseFailureTestConn{Conn: newH1UpgradeScriptConn(nil), failure: closeCause}
	dialer := &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
		return connection, dialCause
	}}
	result, err := DialFramedUpgrade(t.Context(), "ws://failed-dial-close.example/", nil, dialer, H1FramerProtocol)
	if result != nil {
		result.Close()
		t.Fatal("failed dial returned a connection")
	}
	if !errors.Is(err, dialCause) || !errors.Is(err, closeCause) || HTTPUpgradeAllowsFallback(err) || connection.closes.Load() != 1 {
		t.Fatal("failed dial lost owned close custody", connection.closes.Load())
	}
}

// Rejected local framing settings release the successfully negotiated socket
// and return a nil interface, retaining any owned cleanup failure.
func TestDialH1MessagesInvalidFramingRetainsOriginalCloseCause(t *testing.T) {
	resetH1UpgradeTestState(t)
	closeCause := errors.New("synthetic rejected-framing close failure")
	physical := newH1UpgradeScriptConn([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + H1FramerProtocol + "\r\n\r\n"))
	connection := &httpUpgradeCloseFailureTestConn{Conn: physical, failure: closeCause}
	result, err := DialH1Messages(t.Context(), "ws://invalid-framing.example/", nil, h1UpgradeScriptDialer(connection, nil), H1FramerProtocol, -1, true, nil)
	if result != nil {
		t.Fatal("invalid framing returned a typed-nil connection interface")
	}
	if !errors.Is(err, closeCause) || HTTPUpgradeAllowsFallback(err) || connection.closes.Load() != 1 || !physical.closed.Load() {
		t.Fatal("rejected local framing lost original close custody", connection.closes.Load())
	}
}
