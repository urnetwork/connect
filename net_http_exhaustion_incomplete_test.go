// Public HTTP exhaustion must retain incomplete and hard graphs without
// dispatching foreign matching methods or trusting absent receivers.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
)

// A nil receiver can falsely claim transport authority without panicking.
type httpIncompleteNilTestError struct{}

// Keep fixture diagnostics independent of whether the receiver exists.
func (self *httpIncompleteNilTestError) Error() string { return "synthetic absent wrapped cause" }

// Admission must reject the absent receiver before consulting this method.
func (self *httpIncompleteNilTestError) Unwrap() error { return io.ErrUnexpectedEOF }

// An absent network receiver must not acquire timeout authority either.
type httpIncompleteNilNetworkTestError struct{}

// This deliberately remains callable on a nil receiver.
func (self *httpIncompleteNilNetworkTestError) Error() string {
	return "synthetic absent network cause"
}

// The old collector incorrectly trusted this result from a nil receiver.
func (self *httpIncompleteNilNetworkTestError) Timeout() bool { return true }

// No temporary result can cure the absent network receiver.
func (self *httpIncompleteNilNetworkTestError) Temporary() bool { return true }

// A present wrapper whose cause is absent is also incomplete, even if it
// advertises net.Error authority after the failed unwrap.
type httpIncompleteEmptyTestError struct{}

// This value has a stable hard diagnostic for retention.
func (self *httpIncompleteEmptyTestError) Error() string { return "synthetic missing wrapped cause" }

// The graph intentionally ends without a leaf.
func (self *httpIncompleteEmptyTestError) Unwrap() error { return nil }

// An incomplete wrapper cannot use this claim to bypass its missing child.
func (self *httpIncompleteEmptyTestError) Timeout() bool { return true }

// This is another claim, not an observed transport cause.
func (self *httpIncompleteEmptyTestError) Temporary() bool { return true }

// Foreign matching callbacks are never part of error graph admission.
type httpIncompleteForeignTestError struct{ cause error }

// Avoid invoking nested diagnostics while testing admission.
func (self *httpIncompleteForeignTestError) Error() string {
	return "synthetic foreign matching methods"
}

// The collector may inspect the explicit child within its finite allowance.
func (self *httpIncompleteForeignTestError) Unwrap() error { return self.cause }

// Any generic matching traversal would fail this deterministic canary.
func (self *httpIncompleteForeignTestError) Is(error) bool { panic("foreign Is must not execute") }

// No foreign object may manufacture typed transport authority.
func (self *httpIncompleteForeignTestError) As(any) bool { panic("foreign As must not execute") }

// Each route gets its own cyclic graph; only the observed maximum is shared.
type httpIncompleteCycleRootTestError struct{ maximum atomic.Int32 }

// Graph allocation never needs diagnostic recursion.
func (self *httpIncompleteCycleRootTestError) Error() string { return "synthetic cyclic HTTP root" }

// A fresh graph keeps inspection assertions independent of route scheduling.
func (self *httpIncompleteCycleRootTestError) Unwrap() error {
	return &httpIncompleteCycleTestError{owner: self}
}

// One collector owns each child graph's counter.
type httpIncompleteCycleTestError struct {
	owner  *httpIncompleteCycleRootTestError
	visits int32
}

// The cycle diagnostic itself does not recurse.
func (self *httpIncompleteCycleTestError) Error() string { return "synthetic cyclic HTTP cause" }

// Even a deliberately unbounded control eventually finishes with a visible
// missing traversal marker, rather than hanging its qualification process.
func (self *httpIncompleteCycleTestError) Unwrap() error {
	self.visits++
	for {
		previous := self.owner.maximum.Load()
		if previous >= self.visits || self.owner.maximum.CompareAndSwap(previous, self.visits) {
			break
		}
	}
	if self.visits > 2048 {
		return io.EOF
	}
	return self
}

// Nested absent children must consume the same node allowance as present ones.
type httpIncompleteWideTestError struct{ visits int }

// Its diagnostic remains finite even when the children point back to it.
func (self *httpIncompleteWideTestError) Error() string { return "synthetic wide absent children" }

// A control that stops charging absent children still terminates at depth64.
func (self *httpIncompleteWideTestError) Unwrap() []error {
	self.visits++
	children := make([]error, 129)
	children[0] = self
	return children
}

// Absent children neither multiply retained marker storage nor bypass finite
// callback work through successive otherwise empty joined wrappers.
func TestHttpRequestCausesChargeAbsentChildrenWithinOriginalBudget(t *testing.T) {
	cause := &httpIncompleteWideTestError{}
	retained := flattenHttpRequestCauses(cause)
	incomplete, overflow := 0, false
	for _, item := range retained {
		if item.err == errHttpExhaustionCauseIncomplete {
			incomplete++
		}
		overflow = overflow || item.err == errHttpExhaustionCauseTraversal
	}
	if cause.visits > 2 || len(retained) > 258 || incomplete != 1 || !overflow {
		t.Fatalf("absent branches escaped shared admission: visits=%d retained=%d incomplete=%d overflow=%t", cause.visits, len(retained), incomplete, overflow)
	}
}

// Direct admission must reject standard nil receivers before their methods can
// panic. No test supplies a replacement classification result.
func TestHttpRequestCausesRejectTypedNilBeforeForeignMethods(t *testing.T) {
	for _, cause := range []error{(*url.Error)(nil), (*net.OpError)(nil), (*os.PathError)(nil), (*os.LinkError)(nil), (*HttpRequestExhaustedError)(nil), (*httpCauseJoinedTestError)(nil), (*httpIncompleteNilNetworkTestError)(nil)} {
		owner := newHttpRequestCauses(t.Context())
		owner.record(cause)
		exhausted := owner.exhausted(t.Context(), t.Context()).(*HttpRequestExhaustedError)
		if !errors.Is(exhausted, errHttpExhaustionCauseIncomplete) || len(exhausted.causes) != 2 || len(owner.transient) != 0 {
			t.Fatalf("%T absent receiver became retry authority: retained=%d transient=%d", cause, len(exhausted.causes), len(owner.transient))
		}
	}
}

// Actual public serial/parallel request owners retain the absent-cause refusal
// through both their original deadline and explicit caller cancellation.
func TestHttpRequestExhaustionPublicRetainsIncompleteGraphs(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, end := range []error{context.DeadlineExceeded, context.Canceled} {
			for _, cause := range []error{
				(*httpIncompleteNilTestError)(nil),
				(*httpIncompleteNilNetworkTestError)(nil),
				&httpIncompleteEmptyTestError{},
				&httpCauseJoinedTestError{children: []error{io.ErrUnexpectedEOF, nil}},
				&httpCauseJoinedTestError{children: []error{nil, io.ErrUnexpectedEOF}},
			} {
				err := runHttpExhaustionTest(t, serial, end, cause)
				exhausted, ok := err.(*HttpRequestExhaustedError)
				if !ok || !errors.Is(err, errHttpExhaustionCauseIncomplete) || !errors.Is(err, end) || len(exhausted.causes) > 46 {
					t.Fatalf("serial=%t end=%v cause=%T lost hard incomplete graph: %v", serial, end, cause, err)
				}
				if end == context.Canceled && errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("incomplete cause replaced real caller cancellation with a retry deadline")
				}
			}
		}
	}
}

// A public request's explicit graph supplies authority, never its Is/As
// callbacks. Original local custody and cancellation siblings stay inspectable.
func TestHttpRequestExhaustionPublicKeepsHardSiblingsWithoutForeignMatching(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, end := range []error{context.DeadlineExceeded, context.Canceled} {
			hard := &os.PathError{Op: "read", Path: "synthetic-request-custody", Err: context.DeadlineExceeded}
			cause := &httpIncompleteForeignTestError{cause: errors.Join(io.ErrUnexpectedEOF, hard)}
			err := runHttpExhaustionTest(t, serial, end, cause)
			exhausted, ok := err.(*HttpRequestExhaustedError)
			if !ok {
				t.Fatalf("public HTTP owner lost its structured exhaustion: %T", err)
			}
			foundHard, foundEof, foundEnd := false, false, false
			for _, actual := range exhausted.causes {
				foundHard = foundHard || actual == hard
				foundEof = foundEof || actual == io.ErrUnexpectedEOF
				foundEnd = foundEnd || actual == end
			}
			if !foundHard || !foundEof || !foundEnd {
				t.Fatalf("serial=%t end=%v lost a physical, hard or cancellation leaf", serial, end)
			}
		}
	}
}

// Public owners also retain the existing depth/work refusal through the new
// nil admission gate; a cyclic graph cannot become an ordinary transport EOF.
func TestHttpRequestExhaustionPublicBoundsForeignCycle(t *testing.T) {
	for _, serial := range []bool{false, true} {
		cycle := &httpIncompleteCycleRootTestError{}
		err := runHttpExhaustionTest(t, serial, context.Canceled, cycle)
		if !errors.Is(err, errHttpExhaustionCauseTraversal) || !errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || cycle.maximum.Load() > 64 {
			t.Fatalf("serial=%t public graph inspection escaped its bound: maximum=%d err=%v", serial, cycle.maximum.Load(), err)
		}
	}
}
