package connect

// HTTP request/upgrade operation owners retain physical failure causes until their existing
// strategy budget ends. This does not change route selection or retry budgets.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
)

// HttpRequestExhaustedError distinguishes an owned unavailable request from an
// untyped timeout string. Unwrap retains original observed leaves and the actual
// caller/strategy cancellation or private budget deadline. It is not itself a
// retry verdict: every retained cause must satisfy the caller's policy.
type HttpRequestExhaustedError struct{ causes []error }

func (self *HttpRequestExhaustedError) Error() string   { return "http request attempts exhausted" }
func (self *HttpRequestExhaustedError) Unwrap() []error { return slices.Clone(self.causes) }

var errHttpExhaustionAdditionalHardCauses = errors.New("http request exhausted with additional non-transient causes")

// The hook observes a real failed physical attempt after it has been retained.
// It cannot replace an outcome or admission decision and runs outside locks.
type httpAttemptCauseObserverKey struct{}

type httpRequestCauses struct {
	stateLock sync.Mutex
	transient map[uint8]error
	hard      []error
	moreHard  bool
	observe   func(error)
}

func newHttpRequestCauses(ctx context.Context) *httpRequestCauses {
	observe, _ := ctx.Value(httpAttemptCauseObserverKey{}).(func(error))
	return &httpRequestCauses{transient: map[uint8]error{}, observe: observe}
}

// Repeated equivalent transport leaves occupy one slot. Unknown causes occupy
// at most 32 slots plus a hard overflow sentinel, so none can disappear into a
// pure transient verdict. Local path/link errors are never unwrapped into EOF.
func (self *httpRequestCauses) recordWithLock(err error) {
	if err == nil {
		return
	}
	switch err.(type) {
	case *os.PathError, *os.LinkError:
		self.recordHardWithLock(err)
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			self.recordHardWithLock(err)
			return
		}
		for _, cause := range causes {
			self.recordWithLock(cause)
		}
		return
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if cause := wrapped.Unwrap(); cause != nil {
			self.recordWithLock(cause)
			return
		}
	}
	var kind uint8
	switch err {
	case context.Canceled:
		kind = 1
	case context.DeadlineExceeded:
		kind = 2
	case io.EOF:
		kind = 3
	case io.ErrUnexpectedEOF:
		kind = 4
	case net.ErrClosed:
		kind = 5
	case syscall.ECONNRESET:
		kind = 6
	case syscall.ECONNREFUSED:
		kind = 7
	case syscall.ECONNABORTED:
		kind = 8
	case syscall.EPIPE:
		kind = 9
	case syscall.ETIMEDOUT:
		kind = 10
	}
	if kind == 0 {
		if network, ok := err.(net.Error); ok {
			if network.Timeout() {
				kind = 11
			} else if network.Temporary() {
				kind = 12
			}
		}
	}
	if kind != 0 {
		if self.transient[kind] == nil {
			self.transient[kind] = err
		}
		return
	}
	self.recordHardWithLock(err)
}

func (self *httpRequestCauses) recordHardWithLock(err error) {
	if len(self.hard) < 32 {
		self.hard = append(self.hard, err)
	} else {
		self.moreHard = true
	}
}

func (self *httpRequestCauses) record(err error) {
	if err == nil {
		return
	}
	func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); self.recordWithLock(err) }()
	if self.observe != nil {
		self.observe(err)
	}
}

// Deferred response reads happen only after route selection. Observe their
// genuine result without forcing an unselected body read or altering cleanup.
func (self *httpRequestCauses) track(result *evalResult) *evalResult {
	if result == nil {
		return nil
	}
	self.record(result.err)
	if materialize := result.materialize; materialize != nil {
		result.materialize = func() error { err := materialize(); self.record(err); return err }
	}
	return result
}

// Evaluators join all admitted workers before this immutable snapshot. The
// collector still guards reads, so actual cancellation races stay data-race free.
func (self *httpRequestCauses) exhausted(request, strategy context.Context) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	causes := []error{}
	for kind := uint8(1); kind <= 12; kind++ {
		if cause := self.transient[kind]; cause != nil {
			causes = append(causes, cause)
		}
	}
	causes = append(causes, self.hard...)
	if self.moreHard {
		causes = append(causes, errHttpExhaustionAdditionalHardCauses)
	}
	requestErr := request.Err()
	var strategyErr error
	if strategy != nil {
		strategyErr = strategy.Err()
	}
	if requestErr != nil {
		causes = append(causes, requestErr)
	}
	if strategyErr != nil {
		causes = append(causes, strategyErr)
	}
	if requestErr == nil && strategyErr == nil {
		causes = append(causes, context.DeadlineExceeded)
	}
	return &HttpRequestExhaustedError{causes: causes}
}
