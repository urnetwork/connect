package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestApiOutOfBandControlUsesRefreshedJwt(t *testing.T) {
	authorizations := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		var args ConnectControlArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := NewApiOutOfBandControl(ctx, NewClientStrategyWithDefaults(ctx), "old-jwt", server.URL)

	send := func() {
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) {
			done <- err
		})
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("out-of-band control request timed out")
		}
	}

	send()
	control.SetByJwt("new-jwt")
	send()

	for i, want := range []string{"Bearer old-jwt", "Bearer new-jwt"} {
		select {
		case got := <-authorizations:
			if got != want {
				t.Fatalf("request %d authorization = %q, want %q", i+1, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("missing authorization for request %d", i+1)
		}
	}
}

// A cleanup request launched after Client.CloseAndWait must retain lifecycle
// ownership through its callback and decoded response Pack's final pool return.
func TestApiOutOfBandControlCloseAndWaitJoinsPostClientClosePoolOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args ConnectControlArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	control := NewApiOutOfBandControl(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		"test-jwt",
		server.URL,
	)
	client := NewClient(ctx, NewId(), control, DefaultClientSettings())
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatalf("close client before cleanup request: %v", err)
	}
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseCallback) })
	joinEntered := make(chan struct{})
	control.beforeCloseWaitForTest = func() { close(joinEntered) }

	poolTakenBefore, poolReturnedBefore, _ := MessagePoolCounts()
	control.SendControlWithCtx(context.Background(), []*protocol.Frame{}, func(_ []*protocol.Frame, err error) {
		if err != nil {
			t.Errorf("control callback: %v", err)
		}
		// A callback may perform the non-joining close. Its external fixture
		// owner below performs CloseAndWait after observing this barrier.
		control.Close()
		close(callbackEntered)
		<-releaseCallback
	})
	select {
	case <-callbackEntered:
	case <-ctx.Done():
		t.Fatalf("control callback did not enter: %v", ctx.Err())
	}
	poolTakenDuring, poolReturnedDuring, _ := MessagePoolCounts()
	poolOutstandingBefore := int64(poolTakenBefore) - int64(poolReturnedBefore)
	poolOutstandingDuring := int64(poolTakenDuring) - int64(poolReturnedDuring)
	if poolOutstandingDuring != poolOutstandingBefore+1 {
		t.Fatalf(
			"callback pool ownership=%d, want %d",
			poolOutstandingDuring,
			poolOutstandingBefore+1,
		)
	}

	joinResult := make(chan error, 1)
	go func() {
		joinResult <- control.CloseAndWait(ctx)
	}()
	select {
	case <-joinEntered:
	case <-ctx.Done():
		t.Fatalf("control join did not enter: %v", ctx.Err())
	}
	select {
	case err := <-joinResult:
		t.Fatalf("control join returned before callback release: %v", err)
	default:
	}

	releaseOnce.Do(func() { close(releaseCallback) })
	select {
	case err := <-joinResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("control join did not complete: %v", ctx.Err())
	}
	poolTakenAfter, poolReturnedAfter, _ := MessagePoolCounts()
	poolOutstandingAfter := int64(poolTakenAfter) - int64(poolReturnedAfter)
	if poolOutstandingAfter != poolOutstandingBefore {
		t.Fatalf(
			"joined callback pool ownership=%d, want %d",
			poolOutstandingAfter,
			poolOutstandingBefore,
		)
	}
}

// Closing an owned API cancels a normal lifecycle-bound request but still
// waits for its callback to finish before publishing completion.
func TestApiOutOfBandControlCloseAndWaitJoinsCanceledSendControlCallback(t *testing.T) {
	requestEntered := make(chan struct{})
	releaseServer := make(chan struct{})
	var requestEnteredOnce sync.Once
	var releaseServerOnce sync.Once
	defer releaseServerOnce.Do(func() { close(releaseServer) })
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/hello" {
			response.WriteHeader(http.StatusOK)
			return
		}
		requestEnteredOnce.Do(func() { close(requestEntered) })
		<-releaseServer
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	control := NewApiOutOfBandControl(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		"test-jwt",
		server.URL,
	)
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseCallback) })
	control.SendControl([]*protocol.Frame{}, func(_ []*protocol.Frame, err error) {
		if err == nil {
			t.Error("canceled control callback succeeded")
		}
		close(callbackEntered)
		<-releaseCallback
	})
	select {
	case <-requestEntered:
	case <-ctx.Done():
		t.Fatalf("control request did not enter: %v", ctx.Err())
	}

	joinResult := make(chan error, 1)
	go func() {
		joinResult <- control.CloseAndWait(ctx)
	}()
	select {
	case <-callbackEntered:
	case <-ctx.Done():
		t.Fatalf("canceled control callback did not enter: %v", ctx.Err())
	}
	select {
	case err := <-joinResult:
		t.Fatalf("control join returned before canceled callback release: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(releaseCallback) })
	select {
	case err := <-joinResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("control join did not complete: %v", ctx.Err())
	}
	releaseServerOnce.Do(func() { close(releaseServer) })
}

// A control wrapper must not close the API supplied and owned by its caller.
func TestApiOutOfBandControlWithApiClosePreservesSharedApi(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args ConnectControlArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	api := NewBringYourApi(ctx, NewClientStrategyWithDefaults(ctx), server.URL)
	control := NewApiOutOfBandControlWithApi(api)
	if err := control.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	callback, resultChannel := NewBlockingApiCallback[*ConnectControlResult](ctx)
	api.ConnectControl(&ConnectControlArgs{}, callback)
	select {
	case result := <-resultChannel:
		if result.Error != nil {
			t.Fatalf("shared API closed with wrapper: %v", result.Error)
		}
	case <-ctx.Done():
		t.Fatalf("shared API request did not complete: %v", ctx.Err())
	}
}
