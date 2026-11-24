package connect

// transport_family_watcher_join_test.go — a pinned platform transport's
// family hold watcher is one of the workers its close joins: Done and
// CloseAndWait wait for it, a watcher parked in its wait ends with the close,
// and a close from the watcher's own callback is not a join. The goroutine
// checks read the process's goroutine profile and count only the goroutines
// labeled for the test, so a watcher another test left running is not
// counted.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The goroutine label key under which this file's transports run.
const familyHoldWatcherTestLabelKey = "family-hold-watcher-test"

// Counts the goroutines labeled labelValue whose stack runs a family hold
// watcher, from one goroutine profile. The watcher's frame is gone once it
// has returned, even while its goroutine is still unwinding.
func familyHoldWatcherCount(t *testing.T, labelValue string) int {
	t.Helper()
	var profile bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&profile, 1); err != nil {
		t.Fatalf("goroutine profile: %v", err)
	}
	labelText := fmt.Sprintf("%q:%q", familyHoldWatcherTestLabelKey, labelValue)
	recordCountPattern := regexp.MustCompile(`(?m)^(\d+) @`)
	count := 0
	for _, record := range strings.Split(profile.String(), "\n\n") {
		if !strings.Contains(record, labelText) ||
			!strings.Contains(record, "(*PlatformTransport).runFamilyHoldWatcher") {
			continue
		}
		match := recordCountPattern.FindStringSubmatch(record)
		if match == nil {
			t.Fatalf("goroutine profile record without a count: %q", record)
		}
		recordCount, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("goroutine profile count %q: %v", match[1], err)
		}
		count += recordCount
	}
	return count
}

// A pinned H1 transport that its owner holds, so no runner dials, with every
// goroutine it starts labeled labelValue: a goroutine inherits the labels of
// the goroutine that starts it.
func newFamilyHoldWatcherTestTransport(
	t *testing.T,
	ctx context.Context,
	settings *PlatformTransportSettings,
	labelValue string,
) *PlatformTransport {
	t.Helper()
	settings.StartDisabled = true
	var transport *PlatformTransport
	pprof.Do(ctx, pprof.Labels(familyHoldWatcherTestLabelKey, labelValue), func(ctx context.Context) {
		transport = newTestingPinnedTransport(
			t,
			ctx,
			DefaultClientStrategySettings(),
			fmt.Sprintf("ws://%s:1", familyTransportTestHost),
			4,
			settings,
		)
	})
	return transport
}

// CloseAndWait on a pinned transport returns only once its family hold watcher
// has returned, and no watcher is left at that boundary. The watcher is held
// right before it returns, after it has seen the close, until the close waits
// for it, and done is still open there. Before the close joined the watcher,
// CloseAndWait returned while the watcher was held, and the check found it.
func TestPlatformTransportCloseAndWaitJoinsFamilyHoldWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// registered first, so it runs after every other cleanup: a watcher a
	// failed join left held ends with the test
	testEnd := make(chan struct{})
	t.Cleanup(func() { close(testEnd) })

	labelValue := NewId().String()
	parked := make(chan struct{})
	var parkedOnce sync.Once
	exiting := make(chan struct{})
	joining := make(chan struct{})
	var joinCount atomic.Int32
	var watcherExitingAtJoin atomic.Bool
	var doneClosedAtJoin atomic.Bool
	var joinedTransport atomic.Pointer[PlatformTransport]

	settings := testingFamilyTransportSettings()
	settings.familyHoldWatcherWaitForTest = func() {
		parkedOnce.Do(func() { close(parked) })
	}
	settings.familyHoldWatcherExitForTest = func() {
		close(exiting)
		// released only by the close waiting for this watcher, or by the
		// test's end
		select {
		case <-joining:
		case <-testEnd:
		}
	}
	settings.familyHoldWatcherJoinForTest = func() {
		joinCount.Add(1)
		// the join starts only once the watcher is held, so the close waits
		// for a watcher that is still running
		select {
		case <-exiting:
			watcherExitingAtJoin.Store(true)
		case <-testEnd:
		}
		select {
		case <-joinedTransport.Load().Done():
			doneClosedAtJoin.Store(true)
		default:
		}
		close(joining)
	}
	transport := newFamilyHoldWatcherTestTransport(t, ctx, settings, labelValue)
	joinedTransport.Store(transport)

	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the family hold watcher never waited")
	}
	if n := familyHoldWatcherCount(t, labelValue); n != 1 {
		t.Fatalf("%d family hold watchers before the close, want 1", n)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait = %v", err)
	}
	if n := familyHoldWatcherCount(t, labelValue); n != 0 {
		t.Fatalf("%d family hold watchers left once CloseAndWait returned, want 0", n)
	}
	if n := joinCount.Load(); n != 1 {
		t.Fatalf("the close waited for the family hold watcher %d times, want 1", n)
	}
	if !watcherExitingAtJoin.Load() {
		t.Fatal("the close waited without the family hold watcher having seen the close")
	}
	if doneClosedAtJoin.Load() {
		t.Fatal("done closed before the close waited for the family hold watcher")
	}
}

// A family hold watcher parked in its wait, with no network change or policy
// change to wake it, still ends with its transport, whether the owner calls
// CloseAndWait or cancels the context the transport was built with, and none
// is left once the transport is done.
func TestPlatformTransportFamilyHoldWatcherParkedInItsWaitEnds(t *testing.T) {
	closers := []struct {
		name  string
		close func(transport *PlatformTransport, cancel context.CancelFunc) error
	}{
		{
			name: "close and wait",
			close: func(transport *PlatformTransport, cancel context.CancelFunc) error {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer closeCancel()
				return transport.CloseAndWait(closeCtx)
			},
		},
		{
			name: "owner cancel",
			close: func(transport *PlatformTransport, cancel context.CancelFunc) error {
				cancel()
				select {
				case <-transport.Done():
					return nil
				case <-time.After(30 * time.Second):
					return errors.New("done did not close")
				}
			},
		},
	}
	for _, closer := range closers {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			labelValue := NewId().String()
			parked := make(chan struct{})
			var parkedOnce sync.Once
			settings := testingFamilyTransportSettings()
			settings.familyHoldWatcherWaitForTest = func() {
				parkedOnce.Do(func() { close(parked) })
			}
			transport := newFamilyHoldWatcherTestTransport(t, ctx, settings, labelValue)

			select {
			case <-parked:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s: the family hold watcher never waited", closer.name)
			}
			if err := closer.close(transport, cancel); err != nil {
				t.Fatalf("%s: %v", closer.name, err)
			}
			if n := familyHoldWatcherCount(t, labelValue); n != 0 {
				t.Fatalf("%s: %d family hold watchers left once the transport was done, want 0", closer.name, n)
			}
		}()
	}
}

// Closes its transport, with the nonblocking Close, from the family hold
// watcher's log line of a hold change, a callback the watcher runs.
type familyHoldChangeCloseLogger struct {
	Logger
	transport atomic.Pointer[PlatformTransport]
	closed    atomic.Bool
}

func (self *familyHoldChangeCloseLogger) Infof(format string, args ...any) {
	if strings.HasPrefix(format, "[t]ipv%d transport ") {
		if transport := self.transport.Load(); transport != nil && self.closed.CompareAndSwap(false, true) {
			transport.Close()
		}
	}
	self.Logger.Infof(format, args...)
}

// A callback the family hold watcher runs may close its transport: that close
// is not a join, so the watcher goes on, sees the close and returns, and the
// transport's done closes with no watcher left. The callback is the watcher's
// log line of a hold change, here its family going away on a kick.
func TestPlatformTransportClosedFromFamilyHoldWatcherCallbackEnds(t *testing.T) {
	var supported atomic.Bool
	supported.Store(true)
	restore := swapControlFamilyProbe(func(family int) bool {
		return family != 4 || supported.Load()
	})
	t.Cleanup(restore)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := &familyHoldChangeCloseLogger{Logger: DefaultLogger()}
	settings := testingFamilyTransportSettings()
	settings.Log = logger
	labelValue := NewId().String()
	transport := newFamilyHoldWatcherTestTransport(t, ctx, settings, labelValue)
	logger.transport.Store(transport)

	// the constructor's evaluation found the family; the watcher's next one
	// finds it gone
	supported.Store(false)
	transport.Kick()

	select {
	case <-transport.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("a transport closed from its family hold watcher's callback did not end")
	}
	if !logger.closed.Load() {
		t.Fatal("the family hold watcher's callback did not close the transport")
	}
	if n := familyHoldWatcherCount(t, labelValue); n != 0 {
		t.Fatalf("%d family hold watchers left once the transport was done, want 0", n)
	}
}

// A family-agnostic transport runs no family hold watcher, so its close has
// none to wait for: CloseAndWait returns and no watcher hook runs.
func TestPlatformTransportFamilyAgnosticCloseWaitsForNoFamilyHoldWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var hookCount atomic.Int32
	settings := testingPlatformTransportSettings()
	settings.StartDisabled = true
	settings.familyHoldWatcherWaitForTest = func() { hookCount.Add(1) }
	settings.familyHoldWatcherExitForTest = func() { hookCount.Add(1) }
	settings.familyHoldWatcherJoinForTest = func() { hookCount.Add(1) }
	transport := testingPlatformTransport(t, ctx, fmt.Sprintf("ws://%s:1", familyTransportTestHost), settings)

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait = %v", err)
	}
	if n := hookCount.Load(); n != 0 {
		t.Fatalf("a family-agnostic transport ran %d family hold watcher hooks, want 0", n)
	}
}
