//go:build js

// The browser http path (net_http_platform_js.go) through the public strategy
// calls, with globalThis.fetch replaced by a fake: no network. A fetch that
// never settles ends only by the strategy's request timeout, so the hung
// cases need no sleeps; the watchdog only bounds a run that would otherwise
// hang. Run under tools/go_js_wasm_exec_fetch.sh: with the stock
// go_js_wasm_exec, net/http does not use fetch under node.
package connect

import (
	"context"
	"errors"
	"strings"
	"syscall/js"
	"testing"
	"time"
)

const httpPlatformJsTestUrl = "https://api.example/test"

// Bounds a pre-fix hang. A passing run settles within the request timeout.
const httpPlatformJsWatchdogTimeout = 10 * time.Second

func requireHttpPlatformJsFetch(t *testing.T) {
	t.Helper()
	process := js.Global().Get("process")
	if process.Type() == js.TypeObject && strings.HasPrefix(process.Get("argv0").String(), "node") {
		t.Skip("net/http does not use fetch when process.argv0 is node; run with -exec tools/go_js_wasm_exec_fetch.sh")
	}
}

// Replaces globalThis.fetch for the test. `handle` runs inside each fetch
// call with the fetch init object (method, signal) and the returned
// promise's resolve and reject; it must not block.
func installHttpPlatformJsFakeFetch(t *testing.T, handle func(init js.Value, resolve js.Value, reject js.Value)) {
	t.Helper()
	global := js.Global()
	original := global.Get("fetch")
	var funcs []js.Func
	fetch := js.FuncOf(func(this js.Value, args []js.Value) any {
		init := js.Undefined()
		if 1 < len(args) {
			init = args[1]
		}
		executor := js.FuncOf(func(this js.Value, promiseArgs []js.Value) any {
			handle(init, promiseArgs[0], promiseArgs[1])
			return nil
		})
		funcs = append(funcs, executor)
		return global.Get("Promise").New(executor)
	})
	funcs = append(funcs, fetch)
	global.Set("fetch", fetch)
	t.Cleanup(func() {
		global.Set("fetch", original)
		for _, f := range funcs {
			f.Release()
		}
	})
}

// Calls `onAbort` when the fetch's AbortSignal fires.
func onHttpPlatformJsAbort(t *testing.T, init js.Value, onAbort func()) {
	t.Helper()
	listener := js.FuncOf(func(this js.Value, args []js.Value) any {
		onAbort()
		return nil
	})
	t.Cleanup(listener.Release)
	init.Get("signal").Call("addEventListener", "abort", listener)
}

func newHttpPlatformJsTestStrategy(t *testing.T, requestTimeout time.Duration) *ClientStrategy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = requestTimeout
	return NewClientStrategy(ctx, settings)
}

func postHttpPlatformJs(t *testing.T, ctx context.Context, strategy *ClientStrategy) ([]byte, error) {
	t.Helper()
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := HttpPostWithStrategyRaw(ctx, strategy, httpPlatformJsTestUrl, []byte("{}"), "")
		done <- result{body: body, err: err}
	}()
	select {
	case r := <-done:
		return r.body, r.err
	case <-time.After(httpPlatformJsWatchdogTimeout):
		t.Fatalf("request did not return within %s", httpPlatformJsWatchdogTimeout)
		return nil, nil
	}
}

func TestHttpPlatformJsReturnsFetchedBody(t *testing.T) {
	requireHttpPlatformJsFetch(t)
	installHttpPlatformJsFakeFetch(t, func(init js.Value, resolve js.Value, reject js.Value) {
		resolve.Invoke(js.Global().Get("Response").New(`{"ok":true}`, map[string]any{"status": 200}))
	})
	body, err := postHttpPlatformJs(t, context.Background(), newHttpPlatformJsTestStrategy(t, 15*time.Second))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

// A fetch rejection (the browser's TypeError for an unreachable host, CORS
// or offline) keeps its message, and is neither a timeout nor a cancel.
func TestHttpPlatformJsFetchFailureKeepsCause(t *testing.T) {
	requireHttpPlatformJsFetch(t)
	installHttpPlatformJsFakeFetch(t, func(init js.Value, resolve js.Value, reject js.Value) {
		reject.Invoke(js.Global().Get("TypeError").New("Failed to fetch"))
	})
	_, err := postHttpPlatformJs(t, context.Background(), newHttpPlatformJsTestStrategy(t, 15*time.Second))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "TypeError: Failed to fetch") {
		t.Fatalf("err = %q, want the fetch rejection (TypeError: Failed to fetch)", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v reads as a timeout or cancel", err)
	}
}

// A fetch that never answers is aborted at the request timeout and reports
// context.DeadlineExceeded.
func TestHttpPlatformJsHungFetchTimesOut(t *testing.T) {
	requireHttpPlatformJsFetch(t)
	aborted := make(chan struct{})
	installHttpPlatformJsFakeFetch(t, func(init js.Value, resolve js.Value, reject js.Value) {
		onHttpPlatformJsAbort(t, init, func() {
			close(aborted)
			reject.Invoke(js.Global().Get("DOMException").New("This operation was aborted", "AbortError"))
		})
	})
	_, err := postHttpPlatformJs(t, context.Background(), newHttpPlatformJsTestStrategy(t, 50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-aborted:
	default:
		t.Fatal("the fetch was not aborted")
	}
}

// Headers arrive but the body never does: the request timeout still ends
// the read.
func TestHttpPlatformJsHungBodyTimesOut(t *testing.T) {
	requireHttpPlatformJsFetch(t)
	installHttpPlatformJsFakeFetch(t, func(init js.Value, resolve js.Value, reject js.Value) {
		source := js.Global().Get("Object").New()
		// pull is never answered, so a read stays pending until canceled
		pull := js.FuncOf(func(this js.Value, args []js.Value) any {
			return js.Global().Get("Promise").New(js.FuncOf(func(this js.Value, args []js.Value) any { return nil }))
		})
		t.Cleanup(pull.Release)
		source.Set("pull", pull)
		stream := js.Global().Get("ReadableStream").New(source)
		resolve.Invoke(js.Global().Get("Response").New(stream, map[string]any{"status": 200}))
	})
	_, err := postHttpPlatformJs(t, context.Background(), newHttpPlatformJsTestStrategy(t, 50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// The caller's own cancel is context.Canceled, not a timeout.
func TestHttpPlatformJsCallerCancelIsCanceled(t *testing.T) {
	requireHttpPlatformJsFetch(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installHttpPlatformJsFakeFetch(t, func(init js.Value, resolve js.Value, reject js.Value) {
		onHttpPlatformJsAbort(t, init, func() {
			reject.Invoke(js.Global().Get("DOMException").New("This operation was aborted", "AbortError"))
		})
		// the fetch is in flight; the caller gives up
		cancel()
	})
	_, err := postHttpPlatformJs(t, ctx, newHttpPlatformJsTestStrategy(t, 15*time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v reads as a timeout", err)
	}
}
