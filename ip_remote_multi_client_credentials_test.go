package connect

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

type generatorCredentialFixture struct {
	auth   func(context.Context, *AuthNetworkClientArgs) (*AuthNetworkClientResult, error)
	remove func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error)
}

type generatorCredentialDrainOob struct {
	OutOfBandControl
	entered chan struct{}
	release chan struct{}
}

func (self *generatorCredentialDrainOob) CloseAndWait(context.Context) error {
	close(self.entered)
	<-self.release
	return nil
}

func (self *generatorCredentialFixture) AuthNetworkClient(ctx context.Context, args *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
	return self.auth(ctx, args)
}

func (self *generatorCredentialFixture) RemoveNetworkClient(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
	return self.remove(ctx, args)
}

func generatorCredentialTestJwt(t *testing.T, client Id) string {
	t.Helper()
	value, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
		"client_id": client.String(),
	}).SignedString([]byte("synthetic-generator-credential-key"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// The exact public route that formerly owned every mint stalls. The custom
// authority must avoid it, without replacing the generator's identity state.
func TestApiMultiClientInternalCredentialsAvoidPublicAuthStarvation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var publicCalls atomic.Int32
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			publicCalls.Add(1)
			authObservationTestWrote(request)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		defer api.Close()
		source, child, provider := NewId(), NewId(), NewId()
		token := generatorCredentialTestJwt(t, child)
		minted, retired := 0, 0
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.ClientCredentials = &generatorCredentialFixture{
			auth: func(ctx context.Context, args *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
				minted++
				if args.SourceClientId == nil || *args.SourceClientId != source || args.ClientId != nil {
					t.Error("internal mint lost its derived identity")
				}
				return &AuthNetworkClientResult{ByClientJwt: token}, nil
			},
			remove: func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
				retired++
				if args.ClientId != child || ctx.Err() != nil {
					t.Error("retirement lost identity or inherited cancellation")
				}
				return &RemoveNetworkClientResult{}, nil
			},
		}
		generator := NewApiMultiClientGenerator(t.Context(), []*ProviderSpec{{ClientId: &provider}}, strategy, nil,
			"https://api.credentials.example", "synthetic-parent-token", "wss://platform.credentials.example",
			"synthetic probe", "synthetic", "test", &source, DefaultClientSettings, settings)
		// Changing the caller-owned settings must not switch the authority.
		settings.ClientCredentials = nil
		started := time.Now()
		destination := RequireMultiHopId(provider)
		args, err := generator.NewClientArgsForDestinationContext(t.Context(), destination)
		if err != nil {
			t.Fatalf("internal mint remained dependent on public auth: %v", err)
		}
		if time.Since(started) != 0 || publicCalls.Load() != 0 || minted != 1 || args.ClientId != child || args.ClientAuth.ByJwt != token || args.ClientAuth.InstanceId == (Id{}) {
			t.Fatal("internal mint changed credential semantics or contacted public auth")
		}
		generator.RemoveClientArgs(args)
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if retired != 1 || publicCalls.Load() != 0 {
			t.Fatal("derived retirement bypassed its joined authority")
		}
	})
}

// No HTTP retry may create a second identity after an ambiguous internal
// failure. The callback receives the same finite request budget as HTTP.
func TestApiMultiClientInternalCredentialFailureNeverFallsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		publicCalls := 0
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			publicCalls++
			return nil, errors.New("unexpected public request")
		}))
		defer api.Close()
		calls := 0
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.ClientCredentials = &generatorCredentialFixture{
			auth: func(ctx context.Context, args *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
				calls++
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) != strategy.settings.RequestTimeout {
					t.Fatal("internal mint lost its finite request budget")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		generator := NewApiMultiClientGenerator(t.Context(), nil, strategy, nil,
			"https://api.credentials.example", "synthetic-parent-token", "wss://platform.credentials.example",
			"synthetic probe", "synthetic", "test", nil, DefaultClientSettings, settings)
		started := time.Now()
		_, err := generator.NewClientArgsContext(t.Context())
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != strategy.settings.RequestTimeout || calls != 1 || publicCalls != 0 {
			t.Fatalf("ambiguous mint retried or escaped budget: calls=%d public=%d error=%v", calls, publicCalls, err)
		}
		generator.CloseAndWait(t.Context())
	})
}

// Direct model retirement must remain behind the same contract/OOB drain as
// API retirement, including cancellation and the generator's final join.
func TestApiMultiClientInternalRetirementWaitsForControlDrain(t *testing.T) {
	// The existing process-owned pool stats worker must be initialized outside
	// virtual time; it is not owned by a synthetic client or its drain.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			t.Error("internal retirement contacted public API")
			return nil, errors.New("unexpected public request")
		}))
		defer api.Close()
		child := NewId()
		retired := make(chan struct{})
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.ClientCredentials = &generatorCredentialFixture{remove: func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			if args.ClientId != child || ctx.Err() != nil {
				t.Error("retirement lost child identity or live bounded context")
			}
			close(retired)
			return &RemoveNetworkClientResult{}, nil
		}}
		generator := NewApiMultiClientGenerator(t.Context(), nil, strategy, nil,
			"https://api.credentials.example", "synthetic-parent-token", "wss://platform.credentials.example",
			"synthetic probe", "synthetic", "test", nil, DefaultClientSettings, settings)
		oob := &generatorCredentialDrainOob{OutOfBandControl: NewNoContractClientOob(), entered: make(chan struct{}), release: make(chan struct{})}
		client := NewClient(t.Context(), child, oob, closeWaitClientSettings())
		generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: child, ClientAuth: &ClientAuth{InstanceId: NewId()}})
		client.Cancel()
		<-oob.entered
		select {
		case <-retired:
			t.Fatal("internal credential revoked before control drain")
		default:
		}
		closed := make(chan error, 1)
		go func() { closed <- generator.CloseAndWait(t.Context()) }()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("generator abandoned admitted internal retirement")
		default:
		}
		close(oob.release)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		select {
		case <-retired:
		default:
			t.Fatal("joined retirement never reached the internal authority")
		}
	})
}
