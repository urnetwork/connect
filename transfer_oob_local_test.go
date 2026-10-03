package connect

import (
	"context"
	"errors"
	"github.com/urnetwork/connect/protocol"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type privateLocalControl func(context.Context, string, *ConnectControlArgs) (*ConnectControlResult, error)

func (f privateLocalControl) ConnectControl(ctx context.Context, jwt string, args *ConnectControlArgs) (*ConnectControlResult, error) {
	return f(ctx, jwt, args)
}

func TestPrivateLocalOobDeadlineAndTerminalJoin(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("local control escaped to HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		strategy.settings.RequestTimeout = 3 * time.Second
		entered, release := make(chan struct{}), make(chan struct{})
		deadlineObserved := make(chan error, 1)
		calls := atomic.Int32{}
		control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, "old", "https://local.invalid", privateLocalControl(func(ctx context.Context, jwt string, args *ConnectControlArgs) (*ConnectControlResult, error) {
			calls.Add(1)
			if jwt != "current" {
				t.Error("current token not used")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 3*time.Second {
				t.Error("request timeout changed")
			}
			close(entered)
			<-ctx.Done()
			deadlineObserved <- ctx.Err()
			<-release
			return &ConnectControlResult{Pack: ""}, nil
		}))
		control.SetByJwt("current")
		result := make(chan error, 1)
		started := time.Now()
		control.SendControl(nil, func(_ []*protocol.Frame, e error) { result <- e })
		<-entered
		// Sleeping exactly to the deadline races the timeout callback against
		// Close's parent cancellation. Wait for the executor to observe the
		// actual deadline first; fake time advances while this receive blocks.
		if e := <-deadlineObserved; !errors.Is(e, context.DeadlineExceeded) {
			close(release)
			t.Fatalf("executor did not observe its deadline: %v", e)
		}
		if elapsed := time.Since(started); elapsed != 3*time.Second {
			close(release)
			t.Fatalf("deadline elapsed %s, want exactly 3s", elapsed)
		}
		joined := make(chan error, 1)
		go func() { joined <- control.CloseAndWait(context.Background()) }()
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal("close did not retain canceled executor")
		default:
		}
		select {
		case <-result:
			t.Fatal("callback preceded executor terminal return")
		default:
		}
		close(release)
		if e := <-result; !errors.Is(e, context.DeadlineExceeded) {
			t.Fatalf("deadline lost: %v", e)
		}
		if e := <-joined; e != nil {
			t.Fatal(e)
		}
		if calls.Load() != 1 {
			t.Fatal("local control retried")
		}
		control.SendControl(nil, func(_ []*protocol.Frame, e error) {
			if !errors.Is(e, context.Canceled) {
				t.Error("closed owner admitted work")
			}
		})
	})
}

func TestPrivateLocalOobCleanupAndProcessedFailure(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("local escaped to HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		lifecycle, cancel := context.WithCancel(t.Context())
		count := 0
		control := NewApiOutOfBandControlWithLocalControl(lifecycle, strategy, "current", "https://local.invalid", privateLocalControl(func(ctx context.Context, _ string, _ *ConnectControlArgs) (*ConnectControlResult, error) {
			if ctx.Err() != nil {
				t.Error("cleanup inherited lifecycle cancellation")
			}
			count++
			switch count {
			case 1:
				return &ConnectControlResult{Pack: "", Error: &ConnectControlError{Message: "unprocessed"}}, nil
			case 2:
				panic("synthetic controller panic")
			default:
				return &ConnectControlResult{Pack: ""}, nil
			}
		}))
		cancel()
		for range 3 {
			done := make(chan error, 1)
			control.SendControlWithCtx(context.Background(), nil, func(_ []*protocol.Frame, e error) { done <- e })
			e := <-done
			if (count < 3) != (e != nil) {
				t.Fatalf("processed boundary count=%d error=%v", count, e)
			}
		}
		if err := control.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPrivateLocalOobCapturedPerGenerator(t *testing.T) {
	api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected HTTP")
		return nil, errors.New("forbidden")
	}))
	defer api.Close()
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.ClientControl = privateLocalControl(func(context.Context, string, *ConnectControlArgs) (*ConnectControlResult, error) {
		return &ConnectControlResult{Pack: ""}, nil
	})
	generator := NewApiMultiClientGenerator(t.Context(), nil, strategy, nil, "https://local.invalid", "parent", "wss://local.invalid", "test", "test", "test", nil, DefaultClientSettings, settings)
	defer generator.CloseAndWait(t.Context())
	settings.ClientControl = nil
	control := generator.newClientOob(t.Context(), "child")
	defer control.CloseAndWait(t.Context())
	if control.localControl == nil || control.api == nil {
		t.Fatal("generator did not capture actual local API OOB owner")
	}
	clientSettings := DefaultClientSettings()
	clientSettings.ClientKeyRegistrationRequired = true
	client := NewClient(t.Context(), NewId(), control, clientSettings)
	defer client.CloseAndWait(t.Context())
	if err := client.ClientKeyManager().WaitForRegistration(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateLocalOobFrameOwnershipAndResultValidation(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("unexpected HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		for _, test := range []struct {
			name      string
			result    *ConnectControlResult
			wantError bool
		}{
			{"absent", nil, true},
			{"controller_error", &ConnectControlResult{Pack: "", Error: &ConnectControlError{Message: "failure"}}, true},
			{"malformed_base64", &ConnectControlResult{Pack: "!"}, true},
			{"malformed_protobuf", &ConnectControlResult{Pack: "gA=="}, true},
			{"processed_empty", &ConnectControlResult{Pack: ""}, false},
		} {
			control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, "token", "https://local.invalid", privateLocalControl(func(context.Context, string, *ConnectControlArgs) (*ConnectControlResult, error) {
				return test.result, nil
			}))
			frame, err := ToFrame(&protocol.ClientKey{PublicKey: make([]byte, 32)}, DefaultProtocolVersion)
			if err != nil {
				t.Fatal(err)
			}
			witness := MessagePoolShareReadOnly(frame.MessageBytes)
			done := make(chan error, 1)
			calls := 0
			control.SendControl([]*protocol.Frame{frame}, func(_ []*protocol.Frame, err error) { calls++; done <- err })
			if !MessagePoolReturn(witness) {
				t.Fatalf("%s input ownership survived SendControl", test.name)
			}
			err = <-done
			if (err != nil) != test.wantError {
				t.Fatalf("%s result validation: %v", test.name, err)
			}
			if err := control.CloseAndWait(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("%s callback count=%d", test.name, calls)
			}
		}
	})
}

func TestPrivateLocalOobCloseCancelsNormalRequest(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("unexpected HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		entered := make(chan struct{})
		returned := false
		control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, "token", "https://local.invalid", privateLocalControl(func(ctx context.Context, _ string, _ *ConnectControlArgs) (*ConnectControlResult, error) {
			close(entered)
			<-ctx.Done()
			returned = true
			return nil, ctx.Err()
		}))
		done := make(chan error, 1)
		control.SendControl(nil, func(_ []*protocol.Frame, e error) { done <- e })
		<-entered
		if err := control.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !returned {
			t.Fatal("normal executor survived join")
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("normal request did not inherit close cancellation", err)
		}
	})
}
