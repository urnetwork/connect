// Public upgrade decisions and the actual dialer outcome boundary inspect
// complete synthetic graphs without executing foreign matching callbacks.
package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A foreign matcher offers local classifications without an original cause.
type httpPolicyMatcherTestError struct{ matches int }

func (self *httpPolicyMatcherTestError) Error() string { return "synthetic matching error" }
func (self *httpPolicyMatcherTestError) Is(error) bool {
	self.matches++
	return true
}
func (self *httpPolicyMatcherTestError) As(target any) bool {
	self.matches++
	switch target := target.(type) {
	case **HTTPUpgradeError:
		*target = &HTTPUpgradeError{Reason: "rejected", StatusCode: 426}
	case **ExtenderLimitedError:
		*target = &ExtenderLimitedError{RetryAfter: time.Minute}
	}
	return true
}

// An old unbounded walker eventually terminates so omission controls fail an
// assertion instead of hanging the test process.
type httpPolicyCycleTestError struct {
	visits int
	leaf   error
}

func (self *httpPolicyCycleTestError) Error() string { return "synthetic cause cycle" }
func (self *httpPolicyCycleTestError) Unwrap() error {
	self.visits++
	if self.visits > 2*httpRequestCauseNodes {
		return self.leaf
	}
	return self
}

// Nil graph members are unknown, including a typed-nil foreign wrapper whose
// method must never be invoked.
type httpPolicyJoinedTestError struct{ causes []error }

func (self *httpPolicyJoinedTestError) Error() string   { return "synthetic cause join" }
func (self *httpPolicyJoinedTestError) Unwrap() []error { return self.causes }

// Only complete actual capability refusals may affect the origin/protocol cache.
func TestHttpUpgradeFallbackRequiresCompleteOriginalCauses(t *testing.T) {
	resetH1UpgradeTestState(t)
	unsupported := &HTTPUpgradeError{Reason: "rejected", StatusCode: 426}
	terminal := &HTTPUpgradeError{Reason: "rejected", StatusCode: 403, Terminal: true}
	for index, item := range []struct {
		name     string
		err      error
		fallback bool
		cached   bool
	}{
		{name: "direct", err: unsupported, fallback: true, cached: true},
		{name: "wrapped", err: fmt.Errorf("synthetic negotiation: %w", unsupported), fallback: true, cached: true},
		{name: "complete join", err: errors.Join(unsupported, &HTTPUpgradeError{Reason: "invalid-selection"}), fallback: true, cached: true},
		{name: "transient response", err: errors.Join(unsupported, &HTTPUpgradeError{Reason: "response-io"}), fallback: true},
		{name: "canceled after refusal", err: errors.Join(unsupported, context.Canceled)},
		{name: "canceled before refusal", err: errors.Join(context.Canceled, unsupported)},
		{name: "terminal after refusal", err: errors.Join(unsupported, terminal)},
		{name: "terminal before refusal", err: errors.Join(terminal, unsupported)},
		{name: "physical read failure", err: errors.Join(unsupported, io.ErrUnexpectedEOF)},
		{name: "local custody failure", err: &os.PathError{Op: "read", Path: "synthetic-local-custody", Err: unsupported}},
		{name: "nil"},
	} {
		if HTTPUpgradeAllowsFallback(item.err) != item.fallback {
			t.Fatal("fallback ignored an original cause", item.name)
		}
		address := fmt.Sprintf("wss://upgrade-%d.example/", index)
		RecordFramedUpgradeFailure(address, H1FramerProtocol, item.err)
		if FramedUpgradePermitted(address, H1FramerProtocol) == item.cached {
			t.Fatal("capability cache ignored an original cause", item.name)
		}
	}
}

// The public classifiers reject incomplete graphs without invoking Is/As or
// dereferencing typed nils, and stop before a cyclic callback can run unbounded.
func TestHttpUpgradeFallbackBoundsForeignGraphs(t *testing.T) {
	resetH1UpgradeTestState(t)
	unsupported := &HTTPUpgradeError{Reason: "rejected", StatusCode: 426}
	matcher := &httpPolicyMatcherTestError{}
	cycle := &httpPolicyCycleTestError{leaf: unsupported}
	var typedNil *HTTPUpgradeError
	var nilWrapper *httpPolicyJoinedTestError
	var deep error = unsupported
	for range httpRequestCauseDepth + 1 {
		deep = fmt.Errorf("synthetic depth: %w", deep)
	}
	wide := make([]error, httpRequestCauseNodes+1)
	for index := range wide {
		wide[index] = unsupported
	}
	for index, failure := range []error{
		matcher, cycle, typedNil, nilWrapper, deep,
		&httpPolicyJoinedTestError{causes: wide},
		&httpPolicyJoinedTestError{causes: []error{unsupported, nil}},
		&httpPolicyJoinedTestError{causes: []error{nil, unsupported}},
	} {
		if HTTPUpgradeAllowsFallback(failure) {
			t.Fatal("foreign or incomplete graph authorized fallback", index)
		}
		address := fmt.Sprintf("wss://foreign-upgrade-%d.example/", index)
		RecordFramedUpgradeFailure(address, H1FramerProtocol, failure)
		if !FramedUpgradePermitted(address, H1FramerProtocol) {
			t.Fatal("foreign or incomplete graph poisoned capability cache", index)
		}
	}
	if matcher.matches != 0 || cycle.visits > 2*httpRequestCauseNodes {
		t.Fatal("classification invoked foreign matching or unbounded traversal", matcher.matches, cycle.visits)
	}
}

// A real public dial gets exactly one handshake attempt and returns the
// original joined failure; there are no application bytes to replay.
func TestDialH1MessagesPreservesJoinedHardUpgradeCause(t *testing.T) {
	resetH1UpgradeTestState(t)
	unsupported := &HTTPUpgradeError{Reason: "rejected", StatusCode: 426}
	for _, failure := range []error{
		errors.Join(unsupported, context.Canceled),
		errors.Join(unsupported, errors.New("synthetic integrity failure")),
		errors.Join(unsupported, &HTTPUpgradeError{Reason: "rejected", StatusCode: 401, Terminal: true}),
	} {
		dials := 0
		dialer := &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, failure
		}}
		connection, err := DialH1Messages(t.Context(), "ws://joined-upgrade.example/", nil, dialer, H1FramerProtocol, 1024, true, nil)
		if connection != nil {
			connection.Close()
			t.Fatal("failed negotiation returned a connection")
		}
		if dials != 1 || err != failure {
			t.Fatal("joined hard upgrade failure triggered fallback or lost its original causes", dials)
		}
	}
}

// One admission leaf cannot suppress a real failed dial or turn a cancellation
// into relay backoff while the actual caller context remains live.
func TestClientDialerAdmissionCannotHideOtherCauses(t *testing.T) {
	limited := &ExtenderLimitedError{RetryAfter: time.Minute}
	hard := errors.New("synthetic physical dial failure")
	for _, failure := range []error{
		errors.Join(errExtenderMemoryBudget, hard), errors.Join(hard, errExtenderMemoryBudget),
		errors.Join(limited, hard), errors.Join(hard, limited),
		errors.Join(limited, context.Canceled), errors.Join(context.Canceled, limited),
		&os.PathError{Op: "read", Path: "synthetic-local-custody", Err: limited},
	} {
		dialer := &clientDialer{extenderConfig: &ExtenderConfig{}}
		dialer.Update(t.Context(), failure)
		if dialer.errorCount != 1 || dialer.lastErrorTime.IsZero() || dialer.successCount != 0 || !dialer.LimitedUntil().IsZero() {
			t.Fatal("partial admission cause hid a failed dial or changed backoff")
		}
	}
}

// Wrapped and joined genuine admission outcomes retain the existing neutral
// reachability policy and only actual relay limits install backoff.
func TestClientDialerAdmissionAllowsCompletePressureOnly(t *testing.T) {
	limited := &ExtenderLimitedError{RetryAfter: time.Minute}
	for _, item := range []struct {
		err     error
		limited bool
	}{
		{err: errExtenderMemoryBudget},
		{err: fmt.Errorf("synthetic admission: %w", errExtenderMemoryBudget)},
		{err: limited, limited: true},
		{err: fmt.Errorf("synthetic relay: %w", limited), limited: true},
		{err: errors.Join(errExtenderMemoryBudget, limited), limited: true},
		{err: errors.Join(limited, &ExtenderLimitedError{RetryAfter: 2 * time.Minute}), limited: true},
	} {
		dialer := &clientDialer{extenderConfig: &ExtenderConfig{}}
		dialer.Update(t.Context(), item.err)
		if dialer.errorCount != 0 || !dialer.lastErrorTime.IsZero() || dialer.successCount != 0 || dialer.LimitedUntil().IsZero() == item.limited {
			t.Fatal("complete admission outcome changed reachability or lost relay backoff")
		}
	}
}

// Update uses no foreign matching methods and treats incomplete graphs as
// failed outcomes, with the same finite traversal budget as the request owner.
func TestClientDialerAdmissionBoundsForeignGraphs(t *testing.T) {
	limited := &ExtenderLimitedError{RetryAfter: time.Minute}
	matcher := &httpPolicyMatcherTestError{}
	cycle := &httpPolicyCycleTestError{leaf: errExtenderMemoryBudget}
	var typedNil *ExtenderLimitedError
	var nilWrapper *httpPolicyJoinedTestError
	for _, failure := range []error{
		matcher, cycle, typedNil, nilWrapper,
		&httpPolicyJoinedTestError{causes: []error{limited, nil}},
		&httpPolicyJoinedTestError{causes: []error{nil, errExtenderMemoryBudget}},
	} {
		dialer := &clientDialer{extenderConfig: &ExtenderConfig{}}
		dialer.Update(t.Context(), failure)
		if dialer.errorCount != 1 || !dialer.LimitedUntil().IsZero() {
			t.Fatal("foreign or incomplete admission graph bypassed dial failure")
		}
	}
	if matcher.matches != 0 || cycle.visits > httpRequestCauseNodes {
		t.Fatal("dial outcome invoked foreign matching or unbounded traversal", matcher.matches, cycle.visits)
	}
}

// Actual caller cancellation makes a failed attempt neutral before it can
// impose relay backoff; successful outcomes keep their existing accounting.
func TestClientDialerCanceledAdmissionDoesNotLimitHealthyRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dialer := &clientDialer{extenderConfig: &ExtenderConfig{}}
	dialer.Update(ctx, &ExtenderLimitedError{RetryAfter: time.Minute})
	if dialer.errorCount != 0 || !dialer.LimitedUntil().IsZero() {
		t.Fatal("canceled attempt imposed relay failure or admission backoff")
	}
	dialer.Update(ctx, nil)
	if dialer.successCount != 1 || dialer.lastSuccessTime.IsZero() {
		t.Fatal("completed successful dial was erased by later caller cancellation")
	}
}

// The public strategy stops on actual authorization even when another cause
// is joined, while only complete refusal outcomes retain the health exemption.
func TestHttpStrategyTerminalUpgradePreservesOtherOriginalCauses(t *testing.T) {
	resetH1UpgradeTestState(t)
	terminal := &HTTPUpgradeError{Reason: "rejected", StatusCode: 403, Terminal: true}
	hard := errors.New("synthetic physical route failure")
	for _, item := range []struct {
		err      error
		failures uint64
	}{
		{err: terminal},
		{err: errors.Join(terminal, &HTTPUpgradeError{Reason: "rejected", StatusCode: 401, Terminal: true})},
		{err: errors.Join(terminal, hard), failures: 1},
		{err: errors.Join(hard, terminal), failures: 1},
	} {
		settings := DefaultClientStrategySettings()
		settings.EnableResilient = false
		var dials atomic.Int32
		settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, item.err
		}}
		strategy := NewClientStrategy(t.Context(), settings)
		connection, _, err := strategy.H1DialContextWithDialer(t.Context(), "ws://terminal-upgrade.example/", nil, 1024, true, nil)
		strategy.Close()
		if connection != nil {
			connection.Close()
			t.Fatal("refused negotiation returned a connection")
		}
		if dials.Load() != 1 || err != item.err {
			t.Fatal("terminal negotiation retried or lost original causes", dials.Load())
		}
		strategy.mutex.Lock()
		count := len(strategy.dialers)
		var failures uint64
		for dialer := range strategy.dialers {
			dialer.mutex.Lock()
			failures += dialer.errorCount
			dialer.mutex.Unlock()
		}
		strategy.mutex.Unlock()
		if count != 1 || failures != item.failures {
			t.Fatal("authorization exemption hid another original cause", count, failures)
		}
	}
}
