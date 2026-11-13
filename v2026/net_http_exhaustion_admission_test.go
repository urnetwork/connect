// Complete cause frames share one work allowance before any child callback.
// Real HTTP owners retain admitted originals, overflow and caller cancellation.
package connect

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
)

// A terminal callback makes admission work visible without foreign matching.
type httpAdmissionLeafTestError struct{ visits atomic.Int32 }

// Diagnostics do not change the callback census.
func (*httpAdmissionLeafTestError) Error() string { return "synthetic admitted network leaf" }

// Classification itself is the observable work that must follow admission.
func (self *httpAdmissionLeafTestError) Timeout() bool {
	self.visits.Add(1)
	return true
}

// Timeout already supplied this terminal leaf's original transport meaning.
func (*httpAdmissionLeafTestError) Temporary() bool { return false }

// A missing single child must consume work just like a joined missing child.
type httpAdmissionEmptyTestError struct{ visits int }

// Retain this original wrapper without recursively invoking diagnostics.
func (*httpAdmissionEmptyTestError) Error() string { return "synthetic absent single child" }

// Every admitted wrapper reveals exactly one absent branch.
func (self *httpAdmissionEmptyTestError) Unwrap() error {
	self.visits++
	return nil
}

// An incomplete wrapper cannot claim terminal transport authority.
func (*httpAdmissionEmptyTestError) Timeout() bool { panic("absent child reached Timeout") }

// This canary is equally forbidden after the explicit child was absent.
func (*httpAdmissionEmptyTestError) Temporary() bool { panic("absent child reached Temporary") }

// Parallel physical requests receive fresh recursive graphs. Only the maximum
// callback census is shared, so observations cannot depend on route scheduling.
type httpAdmissionRootTestError struct {
	width          int
	recursiveIndex int
	absent         error
	maximum        atomic.Int32
}

// The root's original error is finite even when its child graph is cyclic.
func (*httpAdmissionRootTestError) Error() string { return "synthetic shared cause allowance" }

// Each inspection owns its recursive child and local callback count.
func (self *httpAdmissionRootTestError) Unwrap() error {
	return &httpAdmissionRecursiveTestError{owner: self}
}

// The collector alone traverses each child; no cross-route state is borrowed.
type httpAdmissionRecursiveTestError struct {
	owner  *httpAdmissionRootTestError
	visits int32
}

// Avoid recursive formatting in both production and failing controls.
func (*httpAdmissionRecursiveTestError) Error() string { return "synthetic recursive absent frame" }

// Explicit slot order tests both nil and typed-nil admission around recursion.
// Even a control retaining a rejected cyclic original eventually terminates.
func (self *httpAdmissionRecursiveTestError) Unwrap() []error {
	self.visits++
	for {
		previous := self.owner.maximum.Load()
		if previous >= self.visits || self.owner.maximum.CompareAndSwap(previous, self.visits) {
			break
		}
	}
	if self.visits > 512 {
		return []error{io.EOF}
	}
	children := make([]error, self.owner.width)
	for index := range children {
		children[index] = self.owner.absent
	}
	children[self.owner.recursiveIndex] = self
	return children
}

// Inspect exact original identities, without calling any foreign Is/As method.
func httpAdmissionCauseCount(causes []httpRequestCause, target error) int {
	count := 0
	for _, cause := range causes {
		if cause.err == target {
			count++
		}
	}
	return count
}

// A complete frame at the original boundary is accepted. One additional
// child refuses the frame before any otherwise eligible network callback.
func TestHttpRequestCausesAdmitCompleteJoinedFrameBeforeCallbacks(t *testing.T) {
	for _, width := range []int{255, 256} {
		leaf := &httpAdmissionLeafTestError{}
		children := make([]error, width)
		for index := range children {
			children[index] = leaf
		}
		causes := flattenHttpRequestCauses(&httpCauseJoinedTestError{children: children})
		if width == 255 {
			if leaf.visits.Load() != 255 || len(causes) != 255 || httpAdmissionCauseCount(causes, leaf) != 255 {
				t.Fatal("exact cause frame lost admitted original leaves", leaf.visits.Load(), len(causes))
			}
			for _, cause := range causes {
				if cause.kind != 11 {
					t.Fatal("exact admitted terminal leaf lost its original timeout category")
				}
			}
		} else if leaf.visits.Load() != 0 || len(causes) != 1 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseTraversal) != 1 {
			t.Fatal("oversized child frame ran a prefix of foreign callbacks", leaf.visits.Load(), len(causes))
		}
	}
}

// Existing pending siblings retain their slots before a nested frame is
// admitted. An overflowing frame cannot consume a later cancellation leaf.
func TestHttpRequestCausesReservePendingSiblingSlots(t *testing.T) {
	for _, width := range []int{253, 254} {
		leaf := &httpAdmissionLeafTestError{}
		children := make([]error, width)
		for index := range children {
			children[index] = leaf
		}
		root := &httpCauseJoinedTestError{children: []error{&httpCauseJoinedTestError{children: children}, context.Canceled}}
		causes := flattenHttpRequestCauses(root)
		if httpAdmissionCauseCount(causes, context.Canceled) != 1 {
			t.Fatal("nested cause admission consumed an already admitted cancellation sibling", width)
		}
		if width == 253 {
			if leaf.visits.Load() != 253 || len(causes) != 254 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseTraversal) != 0 {
				t.Fatal("exact nested frame lost its reserved sibling or leaf census", leaf.visits.Load(), len(causes))
			}
		} else if leaf.visits.Load() != 0 || len(causes) != 2 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseTraversal) != 1 {
			t.Fatal("nested oversized frame borrowed its sibling's allowance", leaf.visits.Load(), len(causes))
		}
	}
}

// The root plus 127 wrappers and 127 absent children fit in 256 slots; one more
// wrapper's absent child exceeds that same allowance and must retain overflow.
func TestHttpRequestCausesChargeSingleAbsentChild(t *testing.T) {
	for _, width := range []int{127, 128} {
		empty := &httpAdmissionEmptyTestError{}
		children := make([]error, width)
		for index := range children {
			children[index] = empty
		}
		causes := flattenHttpRequestCauses(&httpCauseJoinedTestError{children: children})
		wantOverflow := 0
		if width == 128 {
			wantOverflow = 1
		}
		if empty.visits != width || httpAdmissionCauseCount(causes, empty) != width || httpAdmissionCauseCount(causes, errHttpExhaustionCauseIncomplete) != 1 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseTraversal) != wantOverflow || len(causes) != width+1+wantOverflow {
			t.Fatal("single absent children escaped the original shared node allowance", width, empty.visits, len(causes))
		}
	}
}

// A discovered absent child cannot take the last slot already reserved by a
// sibling. Its refused admission remains explicit alongside both originals.
func TestHttpRequestCausesAbsentChildPreservesReservedSibling(t *testing.T) {
	empty := &httpAdmissionEmptyTestError{}
	children := make([]error, 255)
	children[0], children[1] = empty, context.Canceled
	causes := flattenHttpRequestCauses(&httpCauseJoinedTestError{children: children})
	if empty.visits != 1 || len(causes) != 4 || httpAdmissionCauseCount(causes, empty) != 1 || httpAdmissionCauseCount(causes, context.Canceled) != 1 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseIncomplete) != 1 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseTraversal) != 1 {
		t.Fatal("absent child consumed or concealed an admitted original sibling", empty.visits, len(causes))
	}
}

// A prior rejected frame does not erase a later admitted empty joined error.
// The global overflow marker is independent of that original hard cause.
func TestHttpRequestCausesRetainEmptyOriginalAfterOtherOverflow(t *testing.T) {
	for _, overflowFirst := range []bool{false, true} {
		wide := &httpCauseJoinedTestError{children: make([]error, 256)}
		empty := &httpCauseJoinedTestError{}
		children := []error{empty, wide, context.Canceled}
		if overflowFirst {
			children[0], children[1] = children[1], children[0]
		}
		causes := flattenHttpRequestCauses(&httpCauseJoinedTestError{children: children})
		if len(causes) != 3 || httpAdmissionCauseCount(causes, empty) != 1 || httpAdmissionCauseCount(causes, context.Canceled) != 1 || httpAdmissionCauseCount(causes, errHttpExhaustionCauseTraversal) != 1 {
			t.Fatal("unrelated overflow erased an admitted empty original", overflowFirst, len(causes))
		}
	}
}

// Both real request strategies retain the finite graph refusal and their
// actual end cause. Slot order and typed nils cannot extend callback work.
func TestHttpRequestExhaustionPublicBoundsNestedAbsentFrames(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, end := range []error{context.DeadlineExceeded, context.Canceled} {
			for _, recursiveIndex := range []int{0, 128} {
				for _, absent := range []error{nil, (*httpIncompleteNilNetworkTestError)(nil)} {
					root := &httpAdmissionRootTestError{width: 129, recursiveIndex: recursiveIndex, absent: absent}
					err := runHttpExhaustionTest(t, serial, end, root)
					exhausted, ok := err.(*HttpRequestExhaustedError)
					if !ok || root.maximum.Load() != 2 || !errors.Is(err, errHttpExhaustionCauseIncomplete) || !errors.Is(err, errHttpExhaustionCauseTraversal) || !errors.Is(err, end) || len(exhausted.causes) > 46 {
						t.Fatalf("public cause frames escaped original shared admission: serial=%t end=%v index=%d absent=%T visits=%d err=%v", serial, end, recursiveIndex, absent, root.maximum.Load(), err)
					}
					if end == context.Canceled && errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("shared cause admission replaced caller cancellation with timeout")
					}
				}
			}
		}
	}
}

// Actual exhaustion retains admitted hard identities after an earlier frame
// overflow, without broadening transport permission or losing caller stop.
func TestHttpRequestExhaustionPublicRetainsOriginalAfterOverflow(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, end := range []error{context.DeadlineExceeded, context.Canceled} {
			wide := &httpCauseJoinedTestError{children: make([]error, 256)}
			empty := &httpCauseJoinedTestError{}
			hard := &os.PathError{Op: "read", Path: "synthetic-admitted-custody", Err: errors.New("synthetic original custody failure")}
			root := &httpCauseJoinedTestError{children: []error{wide, empty, hard}}
			err := runHttpExhaustionTest(t, serial, end, root)
			if !errors.Is(err, empty) || !errors.Is(err, hard) || !errors.Is(err, errHttpExhaustionCauseTraversal) || !errors.Is(err, end) {
				t.Fatal("public exhaustion erased an admitted original after unrelated overflow", serial, end, err)
			}
			if end == context.Canceled && errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("original hard retention fabricated a deadline on caller cancellation")
			}
		}
	}
}

// Negotiation policies consume the same finite original graph. Incomplete or
// overflowing siblings cannot authorize fallback or a refusal-only exemption.
func TestHttpUpgradePoliciesShareCauseFrameAdmission(t *testing.T) {
	root := &httpAdmissionRootTestError{width: 129, recursiveIndex: 0}
	refusal := &HTTPUpgradeError{Reason: "response-io"}
	if fallback := httpUpgradeFallbackCauses(errors.Join(refusal, io.ErrUnexpectedEOF, root)); len(fallback) != 0 || root.maximum.Load() != 2 {
		t.Fatal("upgrade fallback escaped the shared cause frame bound", len(fallback), root.maximum.Load())
	}
	refusal = &HTTPUpgradeError{StatusCode: 403, Reason: "rejected", Terminal: true}
	terminal, refusalOnly := httpUpgradeTerminalCauses(errors.Join(refusal, root))
	if !terminal || refusalOnly || root.maximum.Load() != 2 {
		t.Fatal("terminal upgrade policy lost its admitted refusal or overflow sibling", terminal, refusalOnly, root.maximum.Load())
	}
}
