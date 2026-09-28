package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The operation's real context expires only after a physical read/rebuild
// failure has been retained. No short timer or callback supplies a verdict.
type httpExhaustionTestContext struct {
	context.Context
	done chan struct{}
	end  error
	once sync.Once
}

func (self *httpExhaustionTestContext) Done() <-chan struct{} { return self.done }
func (self *httpExhaustionTestContext) Err() error {
	select {
	case <-self.done:
		return self.end
	default:
		return nil
	}
}
func (self *httpExhaustionTestContext) expire() { self.once.Do(func() { close(self.done) }) }

func runHttpExhaustionTest(t *testing.T, serial bool, end error, rebuild error) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr := io.Copy(io.Discard, io.LimitReader(r.Body, 16*1024))
		closeErr := r.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Error("HTTP exhaustion fixture did not consume its finite request")
			return
		}
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Length: 128\r\nConnection: close\r\n\r\n{")
		_ = buffer.Flush()
		_ = connection.Close()
	}))
	defer server.Close()
	strategyCtx, cancelStrategy := context.WithCancel(t.Context())
	defer cancelStrategy()
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.RequestTimeout = 5 * time.Minute
	settings.ConnectTimeout = time.Minute
	strategy := NewClientStrategy(strategyCtx, settings)
	defer strategy.Close()
	owned := &httpExhaustionTestContext{Context: context.WithoutCancel(t.Context()), done: make(chan struct{}), end: end}
	defer owned.expire()
	observed := make(chan struct{})
	var observeOnce sync.Once
	ctx := context.WithValue(owned, httpAttemptCauseObserverKey{}, func(error) { observeOnce.Do(func() { close(observed) }) })
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/owned", bytes.NewReader([]byte("synthetic original request")))
	if err != nil {
		t.Fatal(err)
	}
	if rebuild != nil {
		request.GetBody = func() (io.ReadCloser, error) { return nil, rebuild }
	}
	hello, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var err error
		if serial {
			_, err = strategy.HttpSerial(request, hello)
		} else {
			_, err = strategy.HttpParallel(request)
		}
		done <- err
	}()
	select {
	case <-observed:
	case err := <-done:
		t.Fatalf("HTTP owner did not reach its retained failure boundary: %v", err)
	case <-t.Context().Done():
		owned.expire()
		<-done
		t.Fatal(t.Context().Err())
	}
	owned.expire()
	err = <-done
	if strategy.settings.RequestTimeout != 5*time.Minute || strategy.settings.ConnectTimeout != time.Minute {
		t.Fatal("HTTP cause preservation changed configured budgets")
	}
	return err
}

func TestHttpRequestExhaustionPreservesBodyFailure(t *testing.T) {
	for _, serial := range []bool{false, true} {
		err := runHttpExhaustionTest(t, serial, context.DeadlineExceeded, nil)
		var exhausted *HttpRequestExhaustedError
		if !errors.As(err, &exhausted) || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			t.Fatalf("HTTP exhaustion erased its physical body cause: serial=%v error=%v", serial, err)
		}
	}
}

func TestHttpRequestExhaustionPreservesMixedHardCause(t *testing.T) {
	for _, serial := range []bool{false, true} {
		canary := &os.PathError{Op: "read", Path: "synthetic-owned-request", Err: errors.New("synthetic custody failure")}
		err := runHttpExhaustionTest(t, serial, context.DeadlineExceeded, errors.Join(io.ErrUnexpectedEOF, canary))
		var exhausted *HttpRequestExhaustedError
		if !errors.As(err, &exhausted) || !errors.Is(err, canary) || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("HTTP exhaustion hid its original hard leaf: serial=%v error=%v", serial, err)
		}
	}
}

func TestHttpRequestExhaustionPreservesCallerCancellation(t *testing.T) {
	for _, serial := range []bool{false, true} {
		err := runHttpExhaustionTest(t, serial, context.Canceled, nil)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("HTTP cancellation was rewritten as a retry timeout: serial=%v error=%v", serial, err)
		}
	}
}

func TestHttpRequestExhaustionBoundsRepeatedCausesWithoutHidingHardFailure(t *testing.T) {
	owner := newHttpRequestCauses(t.Context())
	for range 10000 {
		owner.record(io.ErrUnexpectedEOF)
	}
	canary := errors.New("synthetic first hard failure")
	owner.record(canary)
	for i := range 64 {
		owner.record(fmt.Errorf("synthetic additional integrity failure %d", i))
	}
	err := owner.exhausted(context.Background(), context.Background())
	var exhausted *HttpRequestExhaustedError
	if !errors.As(err, &exhausted) || len(exhausted.Unwrap()) > 35 || !errors.Is(err, canary) || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, errHttpExhaustionAdditionalHardCauses) {
		t.Fatal("bounded HTTP cause owner lost a hard failure or grew per attempt")
	}
	copy := exhausted.Unwrap()
	copy[0] = nil
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("caller mutated the exhausted owner's retained causes")
	}
}

// Upgrade exhaustion must retain the actual socket failure without returning a
// failed evaluator as a winner (which has no usable dialer/connection).
func TestHttpUpgradeExhaustionPreservesSocketFailure(t *testing.T) {
	for _, custom := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, readErr := io.Copy(io.Discard, io.LimitReader(r.Body, 16*1024))
			closeErr := r.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Error("upgrade exhaustion fixture did not consume its finite request")
				return
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
		}))
		settings := DefaultClientStrategySettings()
		settings.EnableResilient = false
		settings.RequestTimeout = 5 * time.Minute
		settings.ConnectTimeout = time.Minute
		strategy := NewClientStrategy(t.Context(), settings)
		owned := &httpExhaustionTestContext{Context: context.WithoutCancel(t.Context()), done: make(chan struct{}), end: context.DeadlineExceeded}
		observed := make(chan struct{})
		var observeOnce sync.Once
		ctx := context.WithValue(owned, httpAttemptCauseObserverKey{}, func(error) { observeOnce.Do(func() { close(observed) }) })
		done := make(chan error, 1)
		address := "ws" + strings.TrimPrefix(server.URL, "http")
		go func() {
			if custom {
				connection, info, err := strategy.H1DialContextWithDialer(ctx, address, nil, 65535, true, nil)
				if connection != nil || info != nil {
					t.Error("failed upgrade fabricated a winning connection or dialer")
				}
				done <- err
			} else {
				connection, response, info, err := strategy.WsDialContextWithDialer(ctx, address, nil)
				if connection != nil || response != nil || info != nil {
					t.Error("failed websocket fabricated a winning connection or dialer")
				}
				done <- err
			}
		}()
		select {
		case <-observed:
		case err := <-done:
			owned.expire()
			strategy.Close()
			server.Close()
			t.Fatalf("upgrade did not reach actual retained failure: custom=%v error=%v", custom, err)
		case <-t.Context().Done():
			owned.expire()
			<-done
			strategy.Close()
			server.Close()
			t.Fatal(t.Context().Err())
		}
		owned.expire()
		err := <-done
		strategy.Close()
		server.Close()
		var exhausted *HttpRequestExhaustedError
		if !errors.As(err, &exhausted) || !(errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("upgrade exhaustion erased its physical socket cause: custom=%v error=%v", custom, err)
		}
	}
}
