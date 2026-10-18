// Current-profile peer fixtures retain the first unexpected error through
// shutdown. This test-only state never owns or cancels a peer's lifetime.
package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// Each peer owns its shutdown announcement and sticky error queue. Recording
// and announcing are concurrent-safe; draining requires the producer join.
type iosMemoryPeerErrors struct {
	expectedShutdown atomic.Bool
	errors           chan error
}

// A full queue keeps the earlier failure; a later closure cannot erase it.
func newIosMemoryPeerErrors() *iosMemoryPeerErrors {
	return &iosMemoryPeerErrors{errors: make(chan error, 1)}
}

// Announcement changes classification only, never peer context or sockets.
func (self *iosMemoryPeerErrors) beginShutdown() {
	self.expectedShutdown.Store(true)
}

// Before explicit shutdown, even a normal closure is a fixture failure.
func (self *iosMemoryPeerErrors) shouldRecord(err error) bool {
	return err != nil && (!self.expectedShutdown.Load() || !iosMemoryExpectedPeerClosure(err))
}

// The graph cannot qualify once an unexpected error has entered this queue.
func (self *iosMemoryPeerErrors) record(err error) {
	if self.shouldRecord(err) {
		select {
		case self.errors <- err:
		default:
		}
	}
}

// Called only after every producer is joined; absence is already conclusive.
func (self *iosMemoryPeerErrors) takeAfterJoin() (error, bool) {
	select {
	case err := <-self.errors:
		return err, true
	default:
		return nil, false
	}
}

// Inspect each typed cause before following unwraps: QUIC protocol failures
// and timeouts also unwrap to net.ErrClosed. A mixed error is expected only
// when every joined cause is an expected closure; wrapping never masks one.
func iosMemoryExpectedPeerClosure(err error) bool {
	switch current := err.(type) {
	case *quic.ApplicationError:
		return current.ErrorCode == 0
	case *quic.TransportError, *quic.IdleTimeoutError, *quic.HandshakeTimeoutError,
		*quic.VersionNegotiationError, *quic.StatelessResetError, *quic.StreamError:
		return false
	case interface{ Unwrap() []error }:
		// quic-go transport close unwraps to net.ErrClosed plus an
		// optional cause. Nil is absent, not a second failure; retain
		// every nonnil unexpected cause, and reject empty/nil-only sets.
		hasCause := false
		for _, cause := range current.Unwrap() {
			if cause == nil {
				continue
			}
			hasCause = true
			if !iosMemoryExpectedPeerClosure(cause) {
				return false
			}
		}
		return hasCause
	}
	if cause := errors.Unwrap(err); cause != nil {
		return iosMemoryExpectedPeerClosure(cause)
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, quic.ErrServerClosed)
}

// These closures are ignored only after an explicit shutdown announcement.
func iosMemoryExpectedPeerErrors() []error {
	return []error{
		context.Canceled, net.ErrClosed, io.EOF, http.ErrServerClosed, quic.ErrServerClosed,
		&quic.ApplicationError{ErrorCode: 0},
		errors.Join(io.EOF, net.ErrClosed),
	}
}

// Keep protocol, framing, deadline and handler failures visible even when
// they wrap net.ErrClosed or share a joined error with an expected closure.
func iosMemoryUnexpectedPeerErrors() []struct {
	name string
	err  error
} {
	failure := errors.New("synthetic peer handler failure")
	return []struct {
		name string
		err  error
	}{
		{name: "handler", err: failure},
		{name: "truncated-frame", err: io.ErrUnexpectedEOF},
		{name: "short-write", err: io.ErrShortWrite},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "application-nonzero", err: &quic.ApplicationError{ErrorCode: 7}},
		{name: "transport-protocol", err: &quic.TransportError{ErrorCode: quic.ProtocolViolation}},
		{name: "idle-timeout", err: &quic.IdleTimeoutError{}},
		{name: "handshake-timeout", err: &quic.HandshakeTimeoutError{}},
		{name: "version-negotiation", err: &quic.VersionNegotiationError{}},
		{name: "stateless-reset", err: &quic.StatelessResetError{}},
		{name: "stream-protocol", err: &quic.StreamError{ErrorCode: 7}},
		{name: "mixed-eof-failure", err: errors.Join(io.EOF, failure)},
		{name: "mixed-zero-application-failure", err: errors.Join(&quic.ApplicationError{ErrorCode: 0}, failure)},
		{name: "closure-text-is-not-identity", err: errors.New(net.ErrClosed.Error())},
	}
}

// The old shutdown predicate discarded all of these errors deterministically.
func TestIosMemoryPeerErrorsRetainUnexpectedAfterShutdown(t *testing.T) {
	for _, test := range iosMemoryUnexpectedPeerErrors() {
		peerErrors := newIosMemoryPeerErrors()
		peerErrors.beginShutdown()
		wrapped := fmt.Errorf("synthetic wrapped peer failure: %w", test.err)
		peerErrors.record(wrapped)
		select {
		case retained := <-peerErrors.errors:
			if retained != wrapped {
				t.Errorf("current-profile peer changed the retained failure: case=%s got=%v", test.name, retained)
			}
		default:
			t.Errorf("unexpected current-profile peer error suppressed after shutdown: case=%s", test.name)
		}
	}
}

// The same closed socket before expected shutdown must remain sticky even
// when later normal-close reports arrive, rather than being retroactively lost.
func TestIosMemoryPeerErrorsKeepPreShutdownFailures(t *testing.T) {
	for index, err := range iosMemoryExpectedPeerErrors() {
		peerErrors := newIosMemoryPeerErrors()
		wrapped := fmt.Errorf("synthetic pre-shutdown failure: %w", err)
		peerErrors.record(wrapped)
		peerErrors.beginShutdown()
		peerErrors.record(net.ErrClosed)
		select {
		case retained := <-peerErrors.errors:
			if retained != wrapped {
				t.Errorf("pre-shutdown peer failure was replaced: case=%d got=%v", index, retained)
			}
		default:
			t.Errorf("pre-shutdown peer failure was erased: case=%d", index)
		}
	}
}

// Expected typed shutdown results must not manufacture a failure, including
// wrapped and joined closures; nil remains a no-op in either phase.
func TestIosMemoryPeerErrorsIgnoreOnlyExpectedShutdown(t *testing.T) {
	for index, err := range iosMemoryExpectedPeerErrors() {
		peerErrors := newIosMemoryPeerErrors()
		peerErrors.record(nil)
		peerErrors.beginShutdown()
		peerErrors.record(fmt.Errorf("synthetic expected closure: %w", err))
		peerErrors.record(nil)
		select {
		case retained := <-peerErrors.errors:
			t.Errorf("expected shutdown became a peer failure: case=%d got=%v", index, retained)
		default:
		}
	}
}

// No sockets or timers: the test chooses the exact terminal packet-read cause.
type iosMemoryPeerErrorPacketConn struct {
	readErrors chan error
}

func (self *iosMemoryPeerErrorPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, <-self.readErrors
}

func (self *iosMemoryPeerErrorPacketConn) WriteTo([]byte, net.Addr) (int, error) {
	return 0, errors.New("synthetic peer must not write a packet")
}

func (self *iosMemoryPeerErrorPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 12345}
}

func (self *iosMemoryPeerErrorPacketConn) Close() error {
	select {
	case self.readErrors <- net.ErrClosed:
	default:
	}
	return nil
}

func (self *iosMemoryPeerErrorPacketConn) SetDeadline(deadline time.Time) error {
	return self.SetReadDeadline(deadline)
}

func (self *iosMemoryPeerErrorPacketConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		return self.Close()
	}
	return nil
}

func (self *iosMemoryPeerErrorPacketConn) SetWriteDeadline(time.Time) error {
	return nil
}

// A custom multi-cause value retains nil entries, unlike errors.Join.
type iosMemoryPeerErrorJoinedError struct {
	causes []error
}

func (self *iosMemoryPeerErrorJoinedError) Error() string {
	return "synthetic peer joined error"
}

func (self *iosMemoryPeerErrorJoinedError) Unwrap() []error {
	return self.causes
}

// Close is joined before Dial, forcing the real fresh QUIC wrapper whose
// causes are net.ErrClosed and nil, not merely the exported sentinel pointer.
func TestIosMemoryPeerErrorsAcceptActualTransportCloseOnlyAfterShutdown(t *testing.T) {
	socket := &iosMemoryPeerErrorPacketConn{readErrors: make(chan error, 1)}
	transport := &quic.Transport{Conn: socket}
	t.Cleanup(func() { _ = transport.Close() })
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	_, closedErr := transport.Dial(context.Background(), socket.LocalAddr(), &tls.Config{}, nil)
	if !errors.Is(closedErr, quic.ErrTransportClosed) || closedErr == quic.ErrTransportClosed {
		t.Fatalf("real QUIC close did not return its fresh typed wrapper: %T %v", closedErr, closedErr)
	}
	unwrapper, ok := closedErr.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("real QUIC close has no multi-cause unwrap: %T", closedErr)
	}
	causes := unwrapper.Unwrap()
	if len(causes) != 2 || causes[0] != net.ErrClosed || causes[1] != nil {
		t.Fatalf("real QUIC close no longer exposes expected plus nil causes: %v", causes)
	}
	for index, err := range []error{closedErr, fmt.Errorf("synthetic wrapped close: %w", closedErr), errors.Join(closedErr, io.EOF)} {
		early := newIosMemoryPeerErrors()
		early.record(err)
		early.beginShutdown()
		if retained, ok := early.takeAfterJoin(); !ok || retained != err {
			t.Fatalf("pre-shutdown QUIC transport closure was lost: case=%d got=%v", index, retained)
		}
		late := newIosMemoryPeerErrors()
		late.beginShutdown()
		late.record(err)
		if retained, ok := late.takeAfterJoin(); ok {
			t.Fatalf("announced normal QUIC transport close became a peer failure: case=%d type=%T error=%v", index, retained, retained)
		}
	}
}

// A real transport read failure uses the same QUIC error type and Is result.
// The listener's terminal result is the barrier; no sleep decides the order.
func TestIosMemoryPeerErrorsRetainActualTransportReadFailureAfterShutdown(t *testing.T) {
	socket := &iosMemoryPeerErrorPacketConn{readErrors: make(chan error, 1)}
	transport := &quic.Transport{Conn: socket}
	t.Cleanup(func() { _ = transport.Close() })
	listener, err := transport.Listen(&tls.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	failure := errors.New("synthetic unexpected transport packet read")
	socket.readErrors <- failure
	_, closedErr := listener.Accept(t.Context())
	if !errors.Is(closedErr, quic.ErrTransportClosed) || !errors.Is(closedErr, failure) {
		t.Fatalf("real QUIC failure lost its typed wrapper or underlying cause: %T %v", closedErr, closedErr)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	peerErrors := newIosMemoryPeerErrors()
	peerErrors.beginShutdown()
	wrapped := fmt.Errorf("synthetic wrapped read failure: %w", closedErr)
	peerErrors.record(wrapped)
	if retained, ok := peerErrors.takeAfterJoin(); !ok || retained != wrapped {
		t.Fatalf("QUIC transport error hid its unexpected packet-read cause: %v", retained)
	}
}

// Nil-only causes, lookalike text, framing failures and typed protocol errors
// must stay failures even beside a valid transport-close cause.
func TestIosMemoryPeerErrorsRejectNilOnlyAndMixedTransportClosure(t *testing.T) {
	failure := errors.New("synthetic peer failure")
	for index, err := range []error{
		&iosMemoryPeerErrorJoinedError{},
		&iosMemoryPeerErrorJoinedError{causes: []error{nil, nil}},
		&iosMemoryPeerErrorJoinedError{causes: []error{net.ErrClosed, nil, failure}},
		&iosMemoryPeerErrorJoinedError{causes: []error{nil, net.ErrClosed, io.ErrUnexpectedEOF}},
		errors.New(quic.ErrTransportClosed.Error()),
		errors.Join(quic.ErrTransportClosed, failure),
		errors.Join(quic.ErrTransportClosed, &quic.TransportError{ErrorCode: quic.ProtocolViolation}),
		errors.Join(quic.ErrTransportClosed, &quic.ApplicationError{ErrorCode: 7}),
	} {
		peerErrors := newIosMemoryPeerErrors()
		peerErrors.beginShutdown()
		wrapped := fmt.Errorf("synthetic invalid transport closure: %w", err)
		peerErrors.record(wrapped)
		if retained, ok := peerErrors.takeAfterJoin(); !ok || retained != wrapped {
			t.Errorf("unexpected transport closure was suppressed: case=%d got=%v", index, retained)
		}
	}
}
