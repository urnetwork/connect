// Generator retirement must retain drain ownership and give the later
// credential request an independent, finite budget. All identities are local.
package connect

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Uses the real generator with only its credential authority replaced. Any
// public HTTP request is a failure, not a second path to retire the identity.
func newRetirementContextTestGenerator(t *testing.T, remove func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error)) *ApiMultiClientGenerator {
	t.Helper()
	api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		t.Error("private retirement contacted the public API")
		return nil, errors.New("unexpected public request")
	}))
	t.Cleanup(api.Close)
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.ClientCredentials = &generatorCredentialFixture{remove: remove}
	return NewApiMultiClientGenerator(t.Context(), nil, strategy, nil,
		"https://api.retirement.example", "synthetic-parent-token", "wss://platform.retirement.example",
		"synthetic probe", "synthetic", "test", nil, DefaultClientSettings, settings)
}

// The first real Client join consumes the old shared budget. Retirement must
// still reach the authority after both that client and its OOB work complete.
func TestApiMultiClientRetirementFreshBudgetAfterClientDrain(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		childId := NewId()
		active := true
		removeErrs := make(chan error, 1)
		generator := newRetirementContextTestGenerator(t, func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			if args.ClientId != childId {
				t.Error("retirement changed the admitted child")
			}
			if err := ctx.Err(); err != nil {
				removeErrs <- err
				return nil, err
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) != 30*time.Second {
				t.Error("retirement did not receive its existing finite request budget")
			}
			active = false
			removeErrs <- nil
			return &RemoveNetworkClientResult{}, nil
		})
		oob := &generatorCredentialDrainOob{OutOfBandControl: NewNoContractClientOob(), entered: make(chan struct{}), release: make(chan struct{})}
		client := NewClient(t.Context(), childId, oob, closeWaitClientSettings())
		clientEntered, clientRelease := make(chan struct{}), make(chan struct{})
		client.beforeRunDoneWaitForTest = func() { close(clientEntered); <-clientRelease }
		generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: childId, ClientAuth: &ClientAuth{InstanceId: NewId()}})
		client.Cancel()
		<-clientEntered
		// Virtual time crosses the old deadline while the actual join barrier
		// remains closed. No scheduler timing determines ownership or ordering.
		time.Sleep(31 * time.Second)
		select {
		case <-removeErrs:
			t.Fatal("identity revoked before the Client join completed")
		default:
		}
		close(clientRelease)
		<-oob.entered
		select {
		case <-removeErrs:
			t.Fatal("identity revoked before the OOB join completed")
		default:
		}
		close(oob.release)
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatalf("generator retirement failed after completed drains: %v", err)
		}
		if err := <-removeErrs; err != nil || active {
			t.Fatalf("completed drains left child active: active=%t retirement_context=%v", active, err)
		}
	})
}

// A timed-out caller is not permission to revoke an identity while an actual
// admitted OOB request still owns its callback. Later retirement is independent.
func TestApiMultiClientRetirementOutlivesCloseWaiter(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		childId := NewId()
		retired := make(chan struct{}, 1)
		generator := newRetirementContextTestGenerator(t, func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			if args.ClientId != childId || ctx.Err() != nil {
				t.Error("late retirement inherited its closed waiter's scope")
			}
			retired <- struct{}{}
			return &RemoveNetworkClientResult{}, nil
		})
		oob := NewApiOutOfBandControlWithApi(generator.api)
		if !oob.requests.start() {
			t.Fatal("synthetic control was not admitted")
		}
		oobEntered := make(chan struct{})
		oob.beforeCloseWaitForTest = func() { close(oobEntered) }
		client := NewClient(t.Context(), childId, oob, closeWaitClientSettings())
		generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: childId, ClientAuth: &ClientAuth{InstanceId: NewId()}})
		client.Cancel()
		<-oobEntered
		closeCtx, closeCancel := context.WithCancel(t.Context())
		closeCancel()
		if err := generator.CloseAndWait(closeCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter claimed a completed join: %v", err)
		}
		time.Sleep(31 * time.Second)
		select {
		case <-retired:
			t.Error("expired drain revoked an identity with an admitted OOB request")
		default:
		}
		oob.requests.finish()
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-retired:
		default:
			t.Fatal("caller cancellation abandoned admitted identity retirement")
		}
	})
}

// A callback or wire result may contain private detail. Final close must
// expose a finite failure, preserve cancellation classification, and not echo it.
func TestApiMultiClientRetirementErrorsAreRedactedAndJoined(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, input := range []struct {
			name      string
			result    *RemoveNetworkClientResult
			err       error
			wantCause error
		}{
			{name: "callback", err: errors.New("synthetic-private-callback-detail")},
			{name: "result", result: &RemoveNetworkClientResult{Error: &RemoveNetworkClientError{Message: "synthetic-private-response-detail"}}},
			{name: "absent"},
			{name: "timeout", err: context.DeadlineExceeded, wantCause: context.DeadlineExceeded},
			{name: "canceled", err: context.Canceled, wantCause: context.Canceled},
		} {
			generator := newRetirementContextTestGenerator(t, func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
				return input.result, input.err
			})
			generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{ClientId: NewId(), ClientAuth: &ClientAuth{InstanceId: NewId()}})
			err := generator.CloseAndWait(t.Context())
			if err == nil || strings.Contains(err.Error(), "synthetic-private") || (input.wantCause != nil && !errors.Is(err, input.wantCause)) {
				t.Errorf("%s retirement error was lost, exposed private detail, or lost classification: %v", input.name, err)
			}
		}
	})
}

// Independent generators must neither borrow cleanup time nor share errors.
func TestApiMultiClientRetirementOwnersRemainIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		first := newRetirementContextTestGenerator(t, func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			close(entered)
			<-release
			return nil, errors.New("synthetic-private-first-owner-detail")
		})
		second := newRetirementContextTestGenerator(t, func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			if ctx.Err() != nil {
				t.Error("healthy owner inherited another owner's cancellation")
			}
			return &RemoveNetworkClientResult{}, nil
		})
		first.RemoveClientArgs(&MultiClientGeneratorClientArgs{ClientId: NewId()})
		<-entered
		second.RemoveClientArgs(&MultiClientGeneratorClientArgs{ClientId: NewId()})
		if err := second.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := first.CloseAndWait(t.Context()); err == nil || strings.Contains(err.Error(), "synthetic-private") {
			t.Errorf("failed owner's finite retirement error was not isolated: %v", err)
		}
	})
}

// Restart persistence remains a deliberate exception: shutting down a stored
// identity must not turn the new cleanup budget into a remote revocation.
func TestApiMultiClientRetirementPreservesRestartIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		removals := 0
		generator := newRetirementContextTestGenerator(t, func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			removals++
			return &RemoveNetworkClientResult{}, nil
		})
		store := &fakeIdentityStore{}
		generator.SetIdentityStore(store)
		identity := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(), ByJwt: "synthetic-restart-token", Destination: RequireMultiHopId(NewId())}
		generator.identityState.Record(identity)
		synctest.Wait()
		generator.cancel()
		generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{ClientId: identity.ClientId, ClientAuth: &ClientAuth{InstanceId: identity.InstanceId}})
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		persisted := store.snapshot()
		if removals != 0 || len(persisted) != 1 || persisted[0].ClientId != identity.ClientId || persisted[0].InstanceId != identity.InstanceId {
			t.Fatal("shutdown revoked or changed a restartable identity")
		}
	})
}
