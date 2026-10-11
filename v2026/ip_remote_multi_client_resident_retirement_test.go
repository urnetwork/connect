package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type residentRetirementCredentialsFixture struct {
	NetworkClientCredentials
	prepare func(context.Context, Id, Id) (func(context.Context) error, error)
}

func (f *residentRetirementCredentialsFixture) PrepareResidentRetirement(ctx context.Context, clientId, instanceId Id) (func(context.Context) error, error) {
	return f.prepare(ctx, clientId, instanceId)
}

type residentRetirementCarrier struct {
	closed, entered, release chan struct{}
}

func (f *residentRetirementCarrier) ConnectedNotify() <-chan struct{} { return nil }
func (f *residentRetirementCarrier) IsConnected() bool                { return true }
func (f *residentRetirementCarrier) Close()                           { close(f.closed) }
func (f *residentRetirementCarrier) CloseAndWait(context.Context) error {
	close(f.entered)
	<-f.release
	return nil
}

func TestApiMultiClientResidentRetirementJoinsAndBindsOriginal(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		child, instance := NewId(), NewId()
		retired, committed := false, false
		carrier := &residentRetirementCarrier{make(chan struct{}), make(chan struct{}), make(chan struct{})}
		generator := newRetirementContextTestGenerator(t, func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			if args.ClientId != child || ctx.Err() != nil {
				t.Error("retirement lost captured child or fresh context")
			}
			retired = true
			return &RemoveNetworkClientResult{}, nil
		})
		generator.clientCredentials = &residentRetirementCredentialsFixture{generator.clientCredentials, func(ctx context.Context, gotChild, gotInstance Id) (func(context.Context) error, error) {
			if gotChild != child || gotInstance != instance {
				t.Error("capture lost exact owned generation")
			}
			select {
			case <-carrier.closed:
				t.Error("capture followed carrier close")
			default:
			}
			return func(ctx context.Context) error {
				if !retired || ctx.Err() != nil {
					t.Error("resident removed before successful identity retirement")
				}
				committed = true
				return nil
			}, nil
		}}
		oob := &generatorCredentialDrainOob{OutOfBandControl: NewNoContractClientOob(), entered: make(chan struct{}), release: make(chan struct{})}
		client := NewClient(t.Context(), child, oob, closeWaitClientSettings())
		clientEntered, clientRelease := make(chan struct{}), make(chan struct{})
		client.beforeRunDoneWaitForTest = func() { close(clientEntered); <-clientRelease }
		generator.transports[client] = &apiWindowClientTransport{current: carrier}
		generator.transportIdle = make(chan struct{})
		args := &MultiClientGeneratorClientArgs{ClientId: child, ClientAuth: &ClientAuth{InstanceId: instance}}
		generator.RemoveClientWithArgs(client, args)
		args.ClientId, args.ClientAuth.InstanceId = NewId(), NewId()
		client.Cancel()
		<-carrier.entered
		if retired || committed {
			t.Fatal("cleanup crossed carrier join")
		}
		close(carrier.release)
		<-clientEntered
		if retired || committed {
			t.Fatal("cleanup crossed Client join")
		}
		close(clientRelease)
		<-oob.entered
		if retired || committed {
			t.Fatal("cleanup crossed OOB join")
		}
		close(oob.release)
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !retired || !committed {
			t.Fatal("joined successful retirement omitted captured resident")
		}
	})
}

func TestApiMultiClientResidentRetirementLateCaptureDiscardedAndJoined(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		retired, committed := 0, 0
		generator := newRetirementContextTestGenerator(t, func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			retired++
			return &RemoveNetworkClientResult{}, nil
		})
		generator.clientCredentials = &residentRetirementCredentialsFixture{generator.clientCredentials, func(ctx context.Context, _, _ Id) (func(context.Context) error, error) {
			close(entered)
			<-release // deliberately models a driver outliving ctx
			return func(context.Context) error { committed++; return nil }, nil
		}}
		client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), closeWaitClientSettings())
		returned := make(chan struct{})
		go func() {
			generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: client.ClientId(), ClientAuth: &ClientAuth{InstanceId: NewId()}})
			close(returned)
		}()
		<-entered
		time.Sleep(2 * time.Second)
		<-returned
		client.Cancel()
		synctest.Wait()
		if retired != 1 || committed != 0 {
			t.Fatal("late capture blocked SQL retirement or was committed")
		}
		waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := generator.CloseAndWait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late capture lost join ownership: %v", err)
		}
		close(release)
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if committed != 0 {
			t.Fatal("late callback was resurrected after teardown")
		}
	})
}

func TestApiMultiClientResidentRetirementCaptureMissKeepsIdentityCleanup(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, scenario := range []string{"error", "panic", "absent", "no-instance"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				retired, prepared, committed := 0, 0, 0
				generator := newRetirementContextTestGenerator(t, func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
					retired++
					return &RemoveNetworkClientResult{}, nil
				})
				generator.clientCredentials = &residentRetirementCredentialsFixture{generator.clientCredentials, func(context.Context, Id, Id) (func(context.Context) error, error) {
					prepared++
					switch scenario {
					case "panic":
						panic("synthetic private capture panic")
					case "error":
						return func(context.Context) error { committed++; return nil }, errors.New("capture failed")
					default:
						return nil, nil
					}
				}}
				child := NewId()
				args := &MultiClientGeneratorClientArgs{ClientId: child, ClientAuth: &ClientAuth{InstanceId: NewId()}}
				if scenario == "no-instance" {
					args.ClientAuth = nil
				}
				client := NewClient(t.Context(), child, NewNoContractClientOob(), closeWaitClientSettings())
				generator.RemoveClientWithArgs(client, args)
				client.Cancel()
				if err := generator.CloseAndWait(t.Context()); err != nil {
					t.Fatal(err)
				}
				if retired != 1 || committed != 0 || (scenario == "no-instance" && prepared != 0) {
					t.Fatal("optional capture changed identity cleanup")
				}
			})
		})
	}
}

func TestApiMultiClientResidentRetirementRequiresActualAuthoritySuccess(t *testing.T) {
	for _, scenario := range []string{"error", "absent", "rejected", "replacement", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, commits := 0, 0
				generator := newRetirementContextTestGenerator(t, func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
					calls++
					switch scenario {
					case "error":
						return nil, errors.New("failed SQL")
					case "absent":
						return nil, nil
					case "rejected":
						return &RemoveNetworkClientResult{Error: &RemoveNetworkClientError{Message: "refused"}}, nil
					default:
						return &RemoveNetworkClientResult{}, nil
					}
				})
				args := &MultiClientGeneratorClientArgs{ClientId: NewId(), ClientAuth: &ClientAuth{InstanceId: NewId()}}
				if scenario == "replacement" {
					generator.identityState.Record(&WindowClientIdentity{ClientId: args.ClientId, InstanceId: NewId(), Destination: RequireMultiHopId(NewId())})
				}
				if scenario == "restart" {
					generator.SetIdentityStore(&fakeIdentityStore{})
					generator.cancel()
				}
				err := generator.removeClientArgsAndWait(t.Context(), args, func(context.Context) error { commits++; return nil })
				if commits != 0 || ((scenario == "replacement" || scenario == "restart") && calls != 0) {
					t.Fatal("non-retirement removed resident")
				}
				if scenario != "replacement" && scenario != "restart" && err == nil {
					t.Fatal("authority rejection was swallowed")
				}
				if err := generator.CloseAndWait(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestApiMultiClientResidentRetirementCommitErrorIsRedactedAndJoined(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		generator := newRetirementContextTestGenerator(t, func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			return &RemoveNetworkClientResult{}, nil
		})
		generator.clientCredentials = &residentRetirementCredentialsFixture{generator.clientCredentials, func(context.Context, Id, Id) (func(context.Context) error, error) {
			return func(context.Context) error { return errors.New("synthetic private resident address") }, nil
		}}
		child := NewId()
		client := NewClient(t.Context(), child, NewNoContractClientOob(), closeWaitClientSettings())
		generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: child, ClientAuth: &ClientAuth{InstanceId: NewId()}})
		client.Cancel()
		err := generator.CloseAndWait(t.Context())
		if err == nil || strings.Contains(err.Error(), "synthetic private") {
			t.Fatalf("resident cleanup error was lost or exposed: %v", err)
		}
	})
}
