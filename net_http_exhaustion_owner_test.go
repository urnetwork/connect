package connect

// Error/context methods are foreign callbacks. The real collector must allow
// bounded reentry and limit adversarial graphs without rewriting them as EOF.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

type httpCauseUnwrapReentryTestError struct{ callback func() }

func (self *httpCauseUnwrapReentryTestError) Error() string { return "synthetic wrapped read" }
func (self *httpCauseUnwrapReentryTestError) Unwrap() error {
	self.callback()
	return io.ErrUnexpectedEOF
}

type httpCauseNetworkReentryTestError struct {
	callback func()
	timeout  bool
}

func (self *httpCauseNetworkReentryTestError) Error() string { return "synthetic network read" }
func (self *httpCauseNetworkReentryTestError) Timeout() bool {
	self.callback()
	return self.timeout
}
func (self *httpCauseNetworkReentryTestError) Temporary() bool {
	self.callback()
	return true
}

type httpCauseReentryTestContext struct {
	context.Context
	callback func()
}

func (self *httpCauseReentryTestContext) Err() error {
	self.callback()
	return context.DeadlineExceeded
}

func TestHttpRequestCausesInvokeForeignMethodsOutsideOwnershipLock(t *testing.T) {
	for _, method := range []string{"unwrap", "timeout", "temporary", "request-context", "strategy-context"} {
		owner := newHttpRequestCauses(t.Context())
		callbacks, locked := 0, false
		callback := func() {
			callbacks++
			// Observe lock ownership before real reentry so the original-body
			// control fails an assertion rather than hanging on a deadlock.
			if !owner.stateLock.TryLock() {
				locked = true
				return
			}
			owner.stateLock.Unlock()
			owner.record(io.EOF)
		}
		var request, strategy context.Context = t.Context(), t.Context()
		switch method {
		case "unwrap":
			owner.record(&httpCauseUnwrapReentryTestError{callback: callback})
		case "timeout", "temporary":
			owner.record(&httpCauseNetworkReentryTestError{callback: callback, timeout: method == "timeout"})
		case "request-context":
			request = &httpCauseReentryTestContext{Context: request, callback: callback}
		case "strategy-context":
			strategy = &httpCauseReentryTestContext{Context: strategy, callback: callback}
		}
		err := owner.exhausted(request, strategy)
		if callbacks == 0 || locked || !errors.Is(err, io.EOF) {
			t.Fatalf("cause owner invoked foreign %s method under its lock or lost reentry: callbacks=%d locked=%v error=%v", method, callbacks, locked, err)
		}
	}
}

// This graph stays cyclic well past the admission limit, then terminates so an
// intentionally restored unbounded control reports an assertion, not a hang.
type httpCauseCycleTestError struct{ visits int }

func (self *httpCauseCycleTestError) Error() string { return "synthetic cyclic read cause" }
func (self *httpCauseCycleTestError) Unwrap() error {
	self.visits++
	if self.visits > 512 {
		return io.EOF
	}
	return self
}

type httpCauseJoinedTestError struct{ children []error }

func (self *httpCauseJoinedTestError) Error() string   { return "synthetic joined read causes" }
func (self *httpCauseJoinedTestError) Unwrap() []error { return self.children }

func TestHttpRequestCausesBoundCyclicDeepAndWideGraphs(t *testing.T) {
	cycle := &httpCauseCycleTestError{}
	var deep error = io.EOF
	for range 1024 {
		deep = fmt.Errorf("synthetic wrapper: %w", deep)
	}
	wide := make([]error, 4096)
	for index := range wide {
		wide[index] = io.EOF
	}
	for _, item := range []struct {
		name string
		err  error
	}{
		{name: "cycle", err: cycle},
		{name: "deep", err: deep},
		{name: "wide", err: &httpCauseJoinedTestError{children: wide}},
	} {
		owner := newHttpRequestCauses(t.Context())
		owner.record(item.err)
		err := owner.exhausted(t.Context(), t.Context())
		var exhausted *HttpRequestExhaustedError
		if !errors.As(err, &exhausted) || !errors.Is(err, errHttpExhaustionCauseTraversal) || len(exhausted.Unwrap()) > 46 {
			t.Fatalf("%s error graph escaped finite inspection or became pure transient: %v", item.name, err)
		}
	}
	if cycle.visits > 256 {
		t.Fatalf("cyclic error invoked unbounded foreign callbacks: %d", cycle.visits)
	}
	owner := newHttpRequestCauses(t.Context())
	empty := &httpCauseJoinedTestError{children: []error{nil, nil}}
	owner.record(empty)
	if !errors.Is(owner.exhausted(t.Context(), t.Context()), empty) {
		t.Fatal("empty foreign joined error disappeared into a budget timeout")
	}
}
