// What a scrubbed stage reports (extender_error.go). The rule itself, that no address
// reaches the handler, is in extender_error_test.go.

package extender

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
)

// The list in the code, against the one written out for the tests, so an entry for a
// stage nothing reports cannot sit there unseen either.
func TestExtenderWholeErrorStagesAreExactlyTheDialStages(t *testing.T) {
	if !slices.Equal(extenderWholeErrorStages, testExtenderWholeErrorStages) {
		t.Fatalf("the stages that keep their whole error = %q, expected %q", extenderWholeErrorStages, testExtenderWholeErrorStages)
	}
}

// Each failure is reported as the one value that says what it was, which errors.Is still
// finds through a handler's own wrapping.
func TestExtenderErrorClass(t *testing.T) {
	opErr := func(err error) error {
		return &net.OpError{Op: "read", Net: "tcp", Source: testErrorServer, Addr: testErrorClient, Err: err}
	}
	failure := errors.New("203.0.113.7 failed")
	for _, c := range []struct {
		name   string
		err    error
		expect error
	}{
		{name: "no error", err: nil, expect: nil},

		{name: "a fixed error", err: extenderFixedError("the source is over its query rate"), expect: extenderFixedError("the source is over its query rate")},
		{
			name:   "a fixed error in a wrap that names the client",
			err:    fmt.Errorf("from %s: %w", testErrorClient, extenderFixedError("the source is over its query rate")),
			expect: extenderFixedError("the source is over its query rate"),
		},

		{name: "an errno", err: opErr(os.NewSyscallError("read", syscall.ECONNRESET)), expect: syscall.ECONNRESET},
		{name: "a bare errno", err: syscall.EPIPE, expect: syscall.EPIPE},
		{name: "an errno that is a timeout", err: opErr(os.NewSyscallError("connect", syscall.ETIMEDOUT)), expect: syscall.ETIMEDOUT},

		{name: "an address the socket cannot use", err: opErr(&net.AddrError{Err: "non-IPv4 address", Addr: "2001:db8::7"}), expect: extenderErrorAddress},

		{name: "a socket's deadline", err: opErr(os.ErrDeadlineExceeded), expect: os.ErrDeadlineExceeded},
		{name: "a context's deadline", err: opErr(context.DeadlineExceeded), expect: context.DeadlineExceeded},
		{name: "any other timeout", err: &testErrorTimeoutOfNoKnownType{testErrorOfNoKnownType{addr: testErrorClient}}, expect: os.ErrDeadlineExceeded},
		{name: "quic's idle timeout", err: &quic.IdleTimeoutError{}, expect: os.ErrDeadlineExceeded},
		{name: "quic's handshake timeout", err: &quic.HandshakeTimeoutError{}, expect: os.ErrDeadlineExceeded},

		// these unwrap to net.ErrClosed, and say more than that
		{name: "quic's application error", err: &quic.ApplicationError{ErrorCode: 7}, expect: extenderErrorQuicFailure},
		{name: "quic's transport error", err: &quic.TransportError{ErrorCode: quic.ProtocolViolation}, expect: extenderErrorQuicFailure},
		{name: "quic's stateless reset", err: &quic.StatelessResetError{}, expect: extenderErrorQuicFailure},
		{name: "quic's version negotiation", err: &quic.VersionNegotiationError{}, expect: extenderErrorQuicFailure},

		{name: "a quic close", err: &quic.ApplicationError{ErrorCode: 0}, expect: net.ErrClosed},
		{name: "a closed socket", err: opErr(net.ErrClosed), expect: net.ErrClosed},
		{name: "a closed pipe", err: io.ErrClosedPipe, expect: io.ErrClosedPipe},
		{name: "a closed http server", err: http.ErrServerClosed, expect: http.ErrServerClosed},
		{name: "a closed quic server", err: fmt.Errorf("serve: %w", quic.ErrServerClosed), expect: quic.ErrServerClosed},
		{name: "a cancel", err: fmt.Errorf("%s: %w", testErrorClient, context.Canceled), expect: context.Canceled},
		{name: "the end", err: io.EOF, expect: io.EOF},
		{name: "an end too soon", err: fmt.Errorf("frame: %w", io.ErrUnexpectedEOF), expect: io.ErrUnexpectedEOF},

		{name: "text", err: failure, expect: extenderErrorUnclassified},
		{name: "text that reads as a close", err: errors.New(net.ErrClosed.Error()), expect: extenderErrorUnclassified},
		{name: "a stream's error", err: &quic.StreamError{ErrorCode: 7}, expect: extenderErrorUnclassified},

		{name: "a failure joined to a close", err: errors.Join(failure, net.ErrClosed), expect: extenderErrorUnclassified},
		{name: "a close joined to a failure", err: fmt.Errorf("%w: %w", io.EOF, failure), expect: extenderErrorUnclassified},
		{name: "two closes", err: errors.Join(net.ErrClosed, net.ErrClosed), expect: net.ErrClosed},
		{name: "a timeout joined to a close", err: errors.Join(net.ErrClosed, os.ErrDeadlineExceeded), expect: os.ErrDeadlineExceeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			class := extenderErrorClass(c.err)
			if class != c.expect {
				t.Fatalf("the class = %#v, expected %#v", class, c.expect)
			}
			if c.expect != nil && !errors.Is(fmt.Errorf("a handler's wrap: %w", class), c.expect) {
				t.Fatalf("errors.Is does not find %v through %v", c.expect, class)
			}
		})
	}
}

// The iOS memory tests read this package's reports to tell a peer's close from a failure,
// and they still can. A websocket's close is left out: it is a lag stage's, and those tests'
// carriers have none.
func TestExtenderErrorClassKeepsPeerClosuresApart(t *testing.T) {
	for _, err := range extenderIosMemoryExpectedPeerErrors() {
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			continue
		}
		if class := extenderErrorClass(err); !extenderIosMemoryExpectedPeerClosure(class) {
			t.Errorf("%v reads as a failure once it is reported as %v", err, class)
		}
	}
	for _, test := range extenderIosMemoryUnexpectedPeerErrors() {
		if class := extenderErrorClass(test.err); extenderIosMemoryExpectedPeerClosure(class) {
			t.Errorf("%s reads as a close once it is reported as %v", test.name, class)
		}
	}
}
