package connect

// HTTP request/upgrade operation owners retain physical failure causes until their existing
// strategy budget ends. This does not change route selection or retry budgets.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
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
var errHttpExhaustionCauseTraversal = errors.New("http request cause tree exceeds its finite inspection bound")
var errHttpExhaustionCauseIncomplete = errors.New("http request cause tree contains an absent cause")

const httpRequestCauseNodes = 256
const httpRequestCauseDepth = 64

type httpRequestCause struct {
	kind uint8
	err  error
}

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

// Unwrap and net.Error methods belong to foreign objects. Inspect a finite
// tree outside ownership locks; cycles/depth/width overflow remain explicit
// hard ambiguity. Local path/link errors cannot unwrap into transient EOF.
func flattenHttpRequestCauses(err error) []httpRequestCause {
	type pendingCause struct {
		err   error
		depth int
	}
	pending := []pendingCause{{err: err, depth: 1}}
	var result []httpRequestCause
	overflow := false
	incomplete := false
	remaining := httpRequestCauseNodes
	for len(pending) != 0 && remaining > 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		remaining--
		if item.err == nil {
			incomplete = true
			continue
		}
		value := reflect.ValueOf(item.err)
		switch value.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			if value.IsNil() {
				incomplete = true
				continue
			}
		}
		if item.depth > httpRequestCauseDepth {
			overflow = true
			continue
		}
		switch item.err.(type) {
		case *os.PathError, *os.LinkError:
			result = append(result, httpRequestCause{err: item.err})
			continue
		}
		if joined, ok := item.err.(interface{ Unwrap() []error }); ok {
			causes := joined.Unwrap()
			available := remaining - len(pending)
			if len(causes) > available {
				overflow = true
				// Admit the complete child frame before following any branch.
				// Truncating first can discard absent slots yet visit a cyclic
				// first child that the original allowance cannot cover.
				continue
			}
			before := len(pending)
			for index := len(causes) - 1; index >= 0; index-- {
				if causes[index] != nil {
					pending = append(pending, pendingCause{err: causes[index], depth: item.depth + 1})
				} else {
					remaining--
					incomplete = true
				}
			}
			if len(pending) == before {
				result = append(result, httpRequestCause{err: item.err})
			}
			continue
		}
		if dns, ok := item.err.(*net.DNSError); ok {
			// Not-found is authoritative even with a transient child. Other
			// flags classify only a leaf; actual children keep their meaning.
			if dns.IsNotFound {
				result = append(result, httpRequestCause{err: item.err})
				continue
			}
			if dns.UnwrapErr == nil {
				result = append(result, classifyHttpRequestCause(item.err))
				continue
			}
		}
		if wrapped, ok := item.err.(interface{ Unwrap() error }); ok {
			cause := wrapped.Unwrap()
			if len(pending) >= remaining {
				overflow = true
			} else if cause != nil {
				pending = append(pending, pendingCause{err: cause, depth: item.depth + 1})
			} else {
				// An absent single child consumes the same allowance as an
				// absent joined child, without taking an admitted sibling's slot.
				remaining--
			}
			if cause != nil {
				continue
			}
			// An absent wrapped cause cannot acquire transport authority from
			// the wrapper's net.Error methods. Retain its original hard cause.
			result = append(result, httpRequestCause{err: item.err})
			incomplete = true
			continue
		}
		result = append(result, classifyHttpRequestCause(item.err))
	}
	if incomplete {
		result = append(result, httpRequestCause{err: errHttpExhaustionCauseIncomplete})
	}
	if overflow || len(pending) != 0 {
		result = append(result, httpRequestCause{err: errHttpExhaustionCauseTraversal})
	}
	return result
}

// Classify only a terminal leaf, with no caller-owned lock held.
func classifyHttpRequestCause(err error) httpRequestCause {
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
	return httpRequestCause{kind: kind, err: err}
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
	causes := flattenHttpRequestCauses(err)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, cause := range causes {
			if cause.kind != 0 {
				if self.transient[cause.kind] == nil {
					self.transient[cause.kind] = cause.err
				}
			} else {
				self.recordHardWithLock(cause.err)
			}
		}
	}()
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
	// Context implementations may also reenter the owner. Copy their results
	// before locking; the immutable cause snapshot has no foreign callbacks.
	requestErr := request.Err()
	var strategyErr error
	if strategy != nil {
		strategyErr = strategy.Err()
	}
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
