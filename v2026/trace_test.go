// Panic-containment tests verify both rescue isolation and lifecycle cleanup.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Records the recovery warnings a test provokes on purpose, which would
// otherwise reach the shared log as if a caller had failed. A warning from
// any other goroutine passes through unchanged.
type handleErrorTestLogger struct {
	Logger
	testFrame string
	stateLock sync.Mutex
	warnings  []string
}

// Captures this test's warnings until it ends.
func newHandleErrorTestLogger(t *testing.T) *handleErrorTestLogger {
	previousLogger := DefaultLogger()
	logger := &handleErrorTestLogger{
		Logger:    previousLogger,
		testFrame: "connect." + t.Name() + "(",
	}
	SetDefaultLogger(logger)
	t.Cleanup(func() { SetDefaultLogger(previousLogger) })
	return logger
}

// A recovered stack that runs through the test's own frame is its warning.
func (self *handleErrorTestLogger) Warningf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if !strings.Contains(message, self.testFrame) {
		self.Logger.Warningf(format, args...)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.warnings = append(self.warnings, message)
}

// One expected recovery warning: its log prefix and the recovered value its
// payload names.
type handleErrorTestWarning struct {
	prefix    string
	recovered string
}

// Asserts the captured warnings, in order, with their exact fields.
func (self *handleErrorTestLogger) assertWarnings(t *testing.T, wantWarnings ...handleErrorTestWarning) {
	t.Helper()
	self.stateLock.Lock()
	warnings := append([]string(nil), self.warnings...)
	self.stateLock.Unlock()
	if len(warnings) != len(wantWarnings) {
		t.Fatalf("warnings = %q, want %d", warnings, len(wantWarnings))
	}
	for i, warning := range warnings {
		prefix, payload, _ := strings.Cut(warning, ": ")
		var decoded struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("warning %d payload: %v", i, err)
		}
		got := handleErrorTestWarning{prefix: prefix, recovered: decoded.Error}
		if got != wantWarnings[i] {
			t.Fatalf("warning %d = %+v, want %+v", i, got, wantWarnings[i])
		}
	}
}

// a rescue handler that panics must be contained by HandleError — it must
// not escalate the recovery into a process crash, and later handlers must
// still run. Regression for the 2026-08-02 teardown crash: a caller
// registered wg.Done both as an in-closure defer and as a rescue handler,
// and the panic path double-counted into `sync: negative WaitGroup counter`
// inside the recover, killing the process.
func TestHandleErrorRescueHandlerPanicContained(t *testing.T) {
	logger := newHandleErrorTestLogger(t)
	laterHandlerRan := false
	HandleError(
		func() {
			panic(errors.New("boom"))
		},
		func() {
			panic(errors.New("rescue handler bug"))
		},
		func() {
			laterHandlerRan = true
		},
	)
	if !laterHandlerRan {
		t.Fatal("handler after a panicking handler did not run")
	}
	logger.assertWarnings(t,
		handleErrorTestWarning{prefix: "Unexpected error", recovered: "*errors.errorString=boom"},
		handleErrorTestWarning{prefix: "Rescue handler panic (contained)", recovered: "*errors.errorString=rescue handler bug"},
	)
}

// the exact caller shape that crashed: wg.Done deferred inside the wrapped
// func AND passed as a rescue handler. The double Done on the panic path
// must be contained (logged), never fatal. Note what containment does NOT
// promise: sync.WaitGroup.Add moves the counter before panicking on
// negative, so the misused WaitGroup itself stays corrupted (a later Wait
// would hang) — the loud log exists so the misuse gets fixed at the caller,
// as UpdateClientScores was. This test's misuse is deliberate, so it asserts
// that log instead of printing it beside real failures.
func TestHandleErrorDoubleDoneContained(t *testing.T) {
	logger := newHandleErrorTestLogger(t)
	boom := errors.New("boom")
	var wg sync.WaitGroup
	wg.Add(1)
	// returning at all is the first assertion: the pre-fix behavior was a
	// process-killing repanic out of HandleError's recover
	r := HandleError(
		func() {
			defer wg.Done()
			panic(boom)
		},
		wg.Done,
	)
	if r != boom {
		t.Fatalf("recovered %v, want the wrapped function's panic", r)
	}
	logger.assertWarnings(t,
		handleErrorTestWarning{prefix: "Unexpected error", recovered: "*errors.errorString=boom"},
		handleErrorTestWarning{prefix: "Rescue handler panic (contained)", recovered: "string=sync: negative WaitGroup counter"},
	)
}

// handlers must not run on a normal return.
func TestHandleErrorNoPanicNoHandlers(t *testing.T) {
	ran := false
	HandleError(
		func() {},
		func() {
			ran = true
		},
	)
	if ran {
		t.Fatal("rescue handler ran without a panic")
	}
}

// A context cancellation function retains its named dynamic type when passed
// through any, so recovery must recognize that type rather than only func().
func TestHandleErrorRunsNamedContextCancelFunc(t *testing.T) {
	logger := newHandleErrorTestLogger(t)
	ctx, cancel := context.WithCancel(context.Background())
	HandleError(
		func() {
			panic(errors.New("boom"))
		},
		cancel,
	)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("recovery did not invoke the named context cancellation function")
	}
	logger.assertWarnings(t, handleErrorTestWarning{prefix: "Unexpected error", recovered: "*errors.errorString=boom"})
}

// A cancellation-with-cause function receives the recovered error despite its
// named dynamic function type.
func TestHandleErrorRunsNamedContextCancelCauseFunc(t *testing.T) {
	logger := newHandleErrorTestLogger(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	HandleError(
		func() {
			panic(errors.New("named cancel cause"))
		},
		cancel,
	)
	if cause := context.Cause(ctx); cause == nil || cause.Error() != "named cancel cause" {
		t.Fatalf("recovery cancellation cause=%v, want named cancel cause", cause)
	}
	logger.assertWarnings(t, handleErrorTestWarning{prefix: "Unexpected error", recovered: "*errors.errorString=named cancel cause"})
}
