package connect

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A window whose platform calls failed through an outage (every call on no
// path fails) had grown its error backoff toward WindowErrorBackoffMax, and
// nothing ended that wait when the path came back: the first round on the new
// network could start up to 15 s late. A network change now starts the
// backoff over and ends the wait.

// A host network change wakes every window's resize loop, so a window short of
// exits (some lost in the outage, or a tunnel that started offline) expands on
// the new path now rather than at its next WindowResizeTimeout pass.
func TestNotifyNetworkChangedWakesWindowResize(t *testing.T) {
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	changes := map[WindowType]chan struct{}{}
	for _, kind := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		parent.windows[kind] = &multiClientWindow{
			ctx: parent.ctx, log: NewNoopLogger(), settings: parent.settings,
			windowType: kind, clients: map[Id]*multiClientChannel{},
			resizeMonitor: NewMonitor(),
		}
		changes[kind] = parent.windows[kind].resizeMonitor.NotifyChannel()
	}
	parent.NotifyNetworkChanged()
	for kind, changed := range changes {
		select {
		case <-changed:
		default:
			t.Fatalf("a network change did not wake the %v window's resize loop", kind)
		}
	}
}

func newNetworkChangeTestWindow(settings *MultiClientSettings) *multiClientWindow {
	return &multiClientWindow{
		settings:             settings,
		monitor:              NewRemoteUserNatMultiClientMonitorWithDefaults(),
		clients:              map[Id]*multiClientChannel{},
		failures:             &windowFailureRecorder{},
		resizeMonitor:        NewMonitor(),
		networkChangeMonitor: NewMonitor(),
		retryJitterForTest:   func(time.Duration) time.Duration { return 0 },
	}
}

func TestNetworkChangeStartsTheWindowErrorBackoffOver(t *testing.T) {
	w := newNetworkChangeTestWindow(DefaultMultiClientSettings())
	floor := w.settings.WindowEnumerateErrorTimeout
	for range 4 {
		w.windowRetryDelay(windowFailurePlatform)
	}
	for range 3 {
		w.windowRetryDelay(windowFailureRateLimit)
	}
	w.notifyNetworkChanged()
	if d := w.windowRetryDelay(windowFailurePlatform); d != floor/2 {
		t.Fatalf("first error delay after a network change: %s, want the floor %s", d, floor/2)
	}
	// the rate-limit streak is the platform's answer, not the path's: its
	// fourth delay doubles three times from the minimum
	if want, d := w.settings.WindowRateLimitBackoffMin*8/2, w.windowRetryDelay(windowFailureRateLimit); d != want {
		t.Fatalf("a network change moved the rate-limit backoff: %s, want %s", d, want)
	}
}

func TestNetworkChangeEndsAWindowErrorWait(t *testing.T) {
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	settings := *DefaultMultiClientSettings()
	// without the network change these waits would outlast the test
	settings.WindowEnumerateErrorTimeout = time.Hour
	settings.WindowErrorBackoffMax = 2 * time.Hour
	settings.WindowRateLimitBackoffMin = time.Hour
	settings.WindowRateLimitBackoffMax = 2 * time.Hour
	w := newNetworkChangeTestWindow(&settings)
	parent.windows[WindowTypeQuality] = w

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := func(class windowFailureClass, networkChanged <-chan struct{}) <-chan bool {
		waited := make(chan bool, 1)
		go func() {
			waited <- w.waitWindowRetry(ctx, class, networkChanged)
		}()
		return waited
	}
	expectEnded := func(waited <-chan bool, what string) {
		t.Helper()
		select {
		case ok := <-waited:
			if !ok {
				t.Fatalf("%s: the wait reported its context ended", what)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: a network change did not end the wait", what)
		}
	}

	// a change while the wait runs
	errorWait := wait(windowFailurePlatform, w.networkChangeNotify())
	rateLimitWait := wait(windowFailureRateLimit, w.networkChangeNotify())
	parent.NotifyNetworkChanged()
	expectEnded(errorWait, "change during the wait")

	// a change during the failing call: the channel was fetched before it
	networkChanged := w.networkChangeNotify()
	parent.NotifyNetworkChanged()
	expectEnded(wait(windowFailurePlatform, networkChanged), "change during the call")

	// a rate limit is waited out
	select {
	case <-rateLimitWait:
		t.Fatal("a network change ended a rate-limit wait")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case ok := <-rateLimitWait:
		if ok {
			t.Fatal("a canceled rate-limit wait reported it waited out its delay")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the context did not end the rate-limit wait")
	}
}

// Every platform retry wait in the window goes through waitWindowRetry, with
// a network-change channel, so a network change can end it. A new
// time.After(self.windowRetryDelay(...)) elsewhere would bring back the late
// first round after an outage.
func TestEveryWindowRetryWaitEndsOnANetworkChange(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	retryWaits := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), "windowRetryDelay(") && !strings.Contains(string(source), "waitWindowRetry(") {
			continue
		}
		file, err := parser.ParseFile(fset, path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch selector.Sel.Name {
				case "windowRetryDelay":
					if function.Name.Name != "waitWindowRetry" {
						t.Errorf("%s: %s waits on windowRetryDelay directly; use waitWindowRetry",
							fset.Position(call.Pos()), function.Name.Name)
					}
				case "waitWindowRetry":
					retryWaits += 1
					if len(call.Args) != 3 {
						t.Errorf("%s: waitWindowRetry takes ctx, class and the network-change channel", fset.Position(call.Pos()))
					} else if ident, ok := call.Args[2].(*ast.Ident); !ok || ident.Name == "nil" {
						t.Errorf("%s: waitWindowRetry needs the channel from networkChangeNotify, fetched before the call it waits out",
							fset.Position(call.Pos()))
					}
				}
				return true
			})
		}
	}
	// the enumerator, the dynamic mint and the fixed-destination mint
	if retryWaits < 3 {
		t.Fatalf("found %d waitWindowRetry call sites, want at least 3", retryWaits)
	}
}
