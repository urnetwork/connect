package connect

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func lateLocalJwt(t *testing.T, client Id) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{"client_id": client.String()}).SignedString([]byte("local-test-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func lateLocalResult(t *testing.T, source, destination, contract Id) *ConnectControlResult {
	t.Helper()
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: contract.Bytes(), SourceId: source.Bytes(), DestinationId: destination.Bytes(), TransferByteCount: 1024})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{StoredContractBytes: stored}}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
	if err != nil {
		t.Fatal(err)
	}
	return &ConnectControlResult{Pack: base64.StdEncoding.EncodeToString(pack)}
}

func lateLocalMessages(t *testing.T, args *ConnectControlArgs) []any {
	t.Helper()
	bytes, err := base64.StdEncoding.DecodeString(args.Pack)
	if err != nil {
		t.Fatal(err)
	}
	pack := &protocol.Pack{}
	if err = proto.Unmarshal(bytes, pack); err != nil {
		t.Fatal(err)
	}
	messages := []any{}
	for _, frame := range pack.Frames {
		message, err := FromFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}

// The server can finish a successful, detached COMMIT while its caller is
// canceled. Its returned contract was never available to a data producer.
func TestPrivateLocalOobCanceledCreateClosesCommittedResult(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("local result cleanup escaped to HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		strategy.settings.RequestTimeout = 3 * time.Second
		source, destination, contract := NewId(), NewId(), NewId()
		token := lateLocalJwt(t, source)
		creates, closes := 0, 0
		control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, token, "https://local.invalid", privateLocalControl(func(ctx context.Context, jwt string, args *ConnectControlArgs) (*ConnectControlResult, error) {
			if jwt != token {
				t.Error("cleanup changed authenticated owner")
			}
			messages := lateLocalMessages(t, args)
			if len(messages) != 1 {
				t.Fatal("unexpected pack cardinality")
			}
			switch message := messages[0].(type) {
			case *protocol.CreateContract:
				creates++
				<-ctx.Done()
				return lateLocalResult(t, source, destination, contract), nil
			case *protocol.CloseContract:
				closes++
				if ctx.Err() != nil {
					t.Error("cleanup inherited canceled request")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) != 3*time.Second {
					t.Error("cleanup lacks finite own budget")
				}
				if Id(message.ContractId) != contract || message.AckedByteCount != 0 || message.UnackedByteCount != 0 || message.Checkpoint {
					t.Error("cleanup changed contract or reported invented usage")
				}
				return &ConnectControlResult{}, nil
			default:
				t.Fatalf("unexpected control %T", message)
			}
			return nil, nil
		}))
		frame, err := ToFrame(&protocol.CreateContract{DestinationId: destination.Bytes(), TransferByteCount: 1024}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{frame}, func(frames []*protocol.Frame, err error) {
			if len(frames) != 0 {
				t.Error("canceled request published a usable contract")
			}
			done <- err
		})
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Error("cancellation identity lost", err)
		}
		if err := control.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if creates != 1 || closes != 1 {
			t.Fatalf("actual create=%d close=%d; want one create and one joined requester close", creates, closes)
		}
	})
}

// Closing request admission cannot strand cleanup belonging to an executor
// admitted before that boundary, or publish callback completion before it joins.
func TestPrivateLocalOobLateCleanupJoinsClosedAdmission(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("unexpected HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		strategy.settings.RequestTimeout = 3 * time.Second
		source, destination, contract := NewId(), NewId(), NewId()
		token := lateLocalJwt(t, source)
		entered, cleanup, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		calls := 0
		control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, token, "https://local.invalid", privateLocalControl(func(ctx context.Context, jwt string, args *ConnectControlArgs) (*ConnectControlResult, error) {
			calls++
			if jwt != token {
				t.Error("in-flight request changed token after rotation")
			}
			if _, ok := lateLocalMessages(t, args)[0].(*protocol.CreateContract); ok {
				close(entered)
				<-ctx.Done()
				return lateLocalResult(t, source, destination, contract), nil
			}
			close(cleanup)
			<-release
			return &ConnectControlResult{}, nil
		}))
		frame, err := ToFrame(&protocol.CreateContract{DestinationId: destination.Bytes()}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		control.SendControl([]*protocol.Frame{frame}, func(frames []*protocol.Frame, err error) { result <- err })
		<-entered
		control.SetByJwt(lateLocalJwt(t, NewId()))
		control.Close()
		joined := make(chan error, 1)
		go func() { joined <- control.CloseAndWait(context.Background()) }()
		synctest.Wait()
		select {
		case <-cleanup:
		default:
			t.Error("closed admission dropped known committed result")
		}
		select {
		case <-joined:
			t.Error("owner joined before cleanup returned")
		default:
		}
		select {
		case <-result:
			t.Error("callback preceded cleanup terminal return")
		default:
		}
		control.SendControl(nil, func(_ []*protocol.Frame, err error) {
			if !errors.Is(err, context.Canceled) {
				t.Error("closed owner admitted new request")
			}
		})
		close(release)
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Error("cancellation identity lost", err)
		}
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("executor calls=%d, want create plus its one cleanup", calls)
		}
	})
}

func TestPrivateLocalOobCleanupTimeoutHasOneJoinedAttempt(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("unexpected HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		strategy.settings.RequestTimeout = 3 * time.Second
		source, destination := NewId(), NewId()
		calls := 0
		control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, lateLocalJwt(t, source), "https://local.invalid", privateLocalControl(func(ctx context.Context, _ string, _ *ConnectControlArgs) (*ConnectControlResult, error) {
			calls++
			<-ctx.Done()
			if calls == 1 {
				return lateLocalResult(t, source, destination, NewId()), nil
			}
			return nil, ctx.Err()
		}))
		frame, err := ToFrame(&protocol.CreateContract{DestinationId: destination.Bytes()}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		done := make(chan error, 1)
		control.SendControl([]*protocol.Frame{frame}, func(_ []*protocol.Frame, err error) { done <- err })
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Error("timeout identity lost", err)
		}
		if err := control.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls != 2 || time.Since(began) != 6*time.Second {
			t.Fatalf("cleanup calls=%d total=%s, want exactly two finite3s phases", calls, time.Since(began))
		}
	})
}

func TestPrivateLocalOobLateCleanupRejectsUnownedResults(t *testing.T) {
	source, destination := NewId(), NewId()
	frame, err := ToFrame(&protocol.CreateContract{DestinationId: destination.Bytes()}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	bytes, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
	if err != nil {
		t.Fatal(err)
	}
	request := &ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(bytes)}
	for _, tc := range []struct {
		name, token string
		result      *ConnectControlResult
	}{
		{"other_source", lateLocalJwt(t, source), lateLocalResult(t, NewId(), destination, NewId())},
		{"other_destination", lateLocalJwt(t, source), lateLocalResult(t, source, NewId(), NewId())},
		{"zero_contract", lateLocalJwt(t, source), lateLocalResult(t, source, destination, Id{})},
		{"malformed_pack", lateLocalJwt(t, source), &ConnectControlResult{Pack: "!"}},
		{"missing_owner", "not-a-jwt", lateLocalResult(t, source, destination, NewId())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames, err := undeliveredLocalContractCloses(tc.token, request, tc.result)
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			if err == nil || len(frames) != 0 {
				t.Fatal("unowned result gained cleanup authority")
			}
		})
	}
	// A repeated response is one known reservation, and a partially failed
	// pack may still contain the successfully committed create to clean up.
	valid := lateLocalResult(t, source, destination, NewId())
	decoded, _ := base64.StdEncoding.DecodeString(valid.Pack)
	pack := &protocol.Pack{}
	if err := proto.Unmarshal(decoded, pack); err != nil {
		t.Fatal(err)
	}
	pack.Frames = append(pack.Frames, pack.Frames[0])
	encoded, _ := proto.Marshal(pack)
	valid.Pack = base64.StdEncoding.EncodeToString(encoded)
	valid.Error = &ConnectControlError{Message: "independent sibling failed"}
	frames, err := undeliveredLocalContractCloses(lateLocalJwt(t, source), request, valid)
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	if err != nil || len(frames) != 1 {
		t.Fatalf("known duplicate partial result cleanup=%d error=%v", len(frames), err)
	}
}

// An ambiguous commit without a returned identity is not cleanup authority.
// This path preserves cancellation and neither invents an ID nor replays the
// create. Durable server expiry remains responsible for any unknown outcome.
func TestPrivateLocalOobCanceledUnknownOutcomeIsNotReplayed(t *testing.T) {
	for _, mode := range []string{"absent", "error", "empty", "protocol_rejection"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
					t.Error("ambiguous local result escaped to HTTP")
					return nil, errors.New("forbidden")
				}))
				defer api.Close()
				strategy.settings.RequestTimeout = 3 * time.Second
				calls := 0
				control := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, lateLocalJwt(t, NewId()), "https://local.invalid", privateLocalControl(func(ctx context.Context, _ string, _ *ConnectControlArgs) (*ConnectControlResult, error) {
					calls++
					<-ctx.Done()
					switch mode {
					case "absent":
						return nil, nil
					case "error":
						return nil, errors.New("commit outcome unavailable")
					case "empty":
						return &ConnectControlResult{}, nil
					default:
						reason := protocol.ContractError_InsufficientBalance
						frame, err := ToFrame(&protocol.CreateContractResult{Error: &reason}, DefaultProtocolVersion)
						if err != nil {
							t.Fatal(err)
						}
						defer MessagePoolReturn(frame.MessageBytes)
						pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
						if err != nil {
							t.Fatal(err)
						}
						return &ConnectControlResult{Pack: base64.StdEncoding.EncodeToString(pack)}, nil
					}
				}))
				frame, err := ToFrame(&protocol.CreateContract{DestinationId: NewId().Bytes()}, DefaultProtocolVersion)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				control.SendControl([]*protocol.Frame{frame}, func(frames []*protocol.Frame, err error) {
					if len(frames) != 0 {
						t.Error("unknown canceled outcome became a result")
					}
					done <- err
				})
				if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
					t.Error("unknown outcome lost cancellation", err)
				}
				if err := control.CloseAndWait(context.Background()); err != nil || calls != 1 {
					t.Fatalf("ambiguous outcome was retried or guessed: calls=%d error=%v", calls, err)
				}
			})
		})
	}
}
