package extender

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/v2026"
)

// What an ErrorHandler hears of a failed connection (EXTENDER.md G2).
//
// No client's address reaches an ErrorHandler, which is what an embedder would log. An
// error can hold one in more ways than anything could look for: a *net.OpError prints
// both ends of its socket, a *url.Error its url, and a site can build whatever text it
// likes. So nothing here looks for an address. Every stage reports one value from a
// closed set of classes, whatever the stage is and whatever the error says, and a new
// stage or a new kind of error is scrubbed with nothing further done. The handler gets
// that value and no chain behind it, so it cannot unwrap its way back to the address
// either.
//
// The exceptions are the stages below, listed by name, which dial something the
// operator named. Their whole error is what an operator needs, and each has been
// read for what it can hold. Connection errors are observable only when a caller
// installs ErrorHandler, and reportError is the one way to it.

// The stages that keep their whole error. An embedder that logs dial failures matches
// on these; the other stage names are diagnostics that may change.
//
// The two forward stages dial the destination a client asked for, which the whitelist
// confines to the operator's hosts (A5, A7), so the error names an operator host, under
// a label a `*.` pattern leaves to the client, which can hold only what a host name can.
//
// The two nlayer stages dial a hop the operator configured (A11), so the error names
// the hop and what connect.DialExtender found there: the status of a refusal, the
// backoff of a limit, a failure of its carrier or of this host's memory budget.
//
// In all four the client's address only chooses the family of the dial
// (forwardNetwork). It is not in the dial's arguments, and a hop is never told it: the
// header a hop is sent names the destination and the pinger.
const (
	ExtenderErrorStageForwardDial         = "forward dial"
	ExtenderErrorStageDatagramForwardDial = "datagram forward dial"
	ExtenderErrorStageNLayerHopDial       = "nlayer hop dial"
	ExtenderErrorStageNLayerProbeDial     = "nlayer probe dial"
)

// Every stage reportError leaves whole, which is a list to read before adding to.
var extenderWholeErrorStages = []string{
	ExtenderErrorStageForwardDial,
	ExtenderErrorStageDatagramForwardDial,
	ExtenderErrorStageNLayerHopDial,
	ExtenderErrorStageNLayerProbeDial,
}

// A failure a site names itself. A constant holds nothing a connection sent, so it
// is a class of its own and reaches the handler as it is. Only ever convert a string
// literal to this (TestExtenderErrorSources).
type extenderFixedError string

func (self extenderFixedError) Error() string {
	return string(self)
}

// The classes that say what happened to a socket or a connection.
const (
	// the socket was asked to use an address it cannot
	extenderErrorAddress = extenderFixedError("the socket cannot use the address")
	// a quic connection that ended with an error rather than a plain close
	extenderErrorQuicFailure = extenderFixedError("the quic connection ended with an error")
	// every failure no class here describes
	extenderErrorUnclassified = extenderFixedError("unclassified")
)

// The most places the log line of a handler's panic names.
const extenderPanicLocationCount = 8

// The closes and ends a failure is reported as when it is one of them, kept as the
// standard values they are so errors.Is finds them through a handler's own wrapping.
var extenderEndErrors = []error{
	context.Canceled,
	io.EOF,
	io.ErrUnexpectedEOF,
	io.ErrClosedPipe,
	http.ErrServerClosed,
	// ahead of the net.ErrClosed it unwraps to
	quic.ErrServerClosed,
	net.ErrClosed,
}

// Reports one failed stage. Safe for concurrent use, as ErrorHandler must be: it runs on
// the goroutine of whatever failed.
func (self *ExtenderServer) reportError(stage string, err error) {
	if self.settings.ErrorHandler == nil {
		return
	}
	if !slices.Contains(extenderWholeErrorStages, stage) {
		err = extenderErrorClass(err)
	}
	self.settings.ErrorHandler(stage, err)
}

// What is left of a panic in a request's handler: where it happened, which comes from
// the binary and holds nothing of the request or the panic's value, and its class. A
// handler that panics is a bug, which an operator should be able to see and report.
func (self *ExtenderServer) reportHandlerPanic(r any) {
	if r == http.ErrAbortHandler || connect.IsDoneError(r) {
		return
	}
	// past this function and the deferred call that called it
	programCounters := make([]uintptr, 32)
	frames := runtime.CallersFrames(programCounters[:runtime.Callers(3, programCounters)])
	locations := []string{}
	for more := true; more && len(locations) < extenderPanicLocationCount; {
		var frame runtime.Frame
		frame, more = frames.Next()
		if !strings.HasPrefix(frame.Function, "runtime.") {
			locations = append(locations, fmt.Sprintf("%s:%d", path.Base(frame.File), frame.Line))
		}
	}
	connect.DefaultLogger().Errorf("[extender] a request's handler panicked at %s\n", strings.Join(locations, " < "))
	self.reportError("handler panic", extenderFixedError("the handler panicked"))
}

// The class of a failure, which is the only thing about it that leaves here. Each
// result is a fixed value, an errno or one of the standard errors, whose text comes
// from the system or a constant and never from the error it was found in. The order is
// what tells causes apart: the most specific first, then a timeout or a quic failure
// ahead of the net.ErrClosed each unwraps to, and an end only when all of the error
// is one, so a failure joined to a close is not reported as the close.
func extenderErrorClass(err error) error {
	if err == nil {
		return nil
	}
	var fixedErr extenderFixedError
	if errors.As(err, &fixedErr) {
		return fixedErr
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) {
		return extenderErrorAddress
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return os.ErrDeadlineExceeded
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return os.ErrDeadlineExceeded
	}
	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.ErrorCode != 0 {
		return extenderErrorQuicFailure
	}
	var transportErr *quic.TransportError
	var statelessResetErr *quic.StatelessResetError
	var versionNegotiationErr *quic.VersionNegotiationError
	if errors.As(err, &transportErr) || errors.As(err, &statelessResetErr) || errors.As(err, &versionNegotiationErr) {
		return extenderErrorQuicFailure
	}
	var joinedErr interface{ Unwrap() []error }
	if errors.As(err, &joinedErr) {
		for _, causeErr := range joinedErr.Unwrap() {
			if causeErr != nil && extenderErrorClass(causeErr) == extenderErrorUnclassified {
				return extenderErrorUnclassified
			}
		}
	}
	for _, endErr := range extenderEndErrors {
		if errors.Is(err, endErr) {
			return endErr
		}
	}
	return extenderErrorUnclassified
}
