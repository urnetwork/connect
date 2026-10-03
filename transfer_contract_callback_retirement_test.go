package connect

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Hold the real API's decoded successful response after its cancellation
// decision but before the real manager callback consumes it. Cleanup keeps
// the actual API's caller-context method through embedding.
type postCheckContractOob struct {
	*ApiOutOfBandControl
	entered chan struct{}
	release chan struct{}
}

func (self *postCheckContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.ApiOutOfBandControl.SendControl(frames, func(result []*protocol.Frame, err error) {
		if err == nil && len(result) == 1 && result[0].MessageType == protocol.MessageType_TransferCreateContractResult {
			close(self.entered)
			<-self.release
		}
		callback(result, err)
	})
}

// Mirrors the generator's actual teardown order: join the Client, then close
// and join the external OOB owner, then retire identity. Cancellation after
// the OOB's check must leave the manager callback admitted long enough to
// close its retired generation's returned contract before OOB admission ends.
func TestPrivateLocalOobPostCheckCallbackRetirementClosesContract(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flush bool
	}{{"retired_generation", true}, {"manager_shutdown", false}} {
		t.Run(tc.name, func(t *testing.T) {
			testPostCheckCallbackRetirement(t, tc.flush)
		})
	}
}

func testPostCheckCallbackRetirement(t *testing.T, flush bool) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		api, strategy := authObservationTestApi(ctx, nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("local retirement escaped to HTTP")
			return nil, errors.New("forbidden")
		}))
		defer api.Close()
		strategy.settings.RequestTimeout = 3 * time.Second
		source, destination, contract := NewId(), NewId(), NewId()
		var creates, closes atomic.Int32
		owner := NewApiOutOfBandControlWithLocalControl(ctx, strategy, lateLocalJwt(t, source), "https://local.invalid", privateLocalControl(func(callCtx context.Context, _ string, args *ConnectControlArgs) (*ConnectControlResult, error) {
			for _, message := range lateLocalMessages(t, args) {
				switch message := message.(type) {
				case *protocol.CreateContract:
					creates.Add(1)
					return lateLocalResult(t, source, destination, contract), nil
				case *protocol.CloseContract:
					closes.Add(1)
					if callCtx.Err() != nil || Id(message.ContractId) != contract || message.AckedByteCount != 0 || message.UnackedByteCount != 0 || message.Checkpoint {
						t.Error("retirement changed requester-only cleanup authority")
					}
					return &ConnectControlResult{}, nil
				default:
					t.Errorf("unexpected local control %T", message)
				}
			}
			return &ConnectControlResult{}, nil
		}))
		oob := &postCheckContractOob{ApiOutOfBandControl: owner, entered: make(chan struct{}), release: make(chan struct{})}
		var release sync.Once
		unblock := func() { release.Do(func() { close(oob.release) }) }
		defer unblock()
		settings := DefaultClientSettings()
		settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.ControlPingTimeout = 0
		settings.Log = NewNoopLogger()
		client := NewClient(ctx, source, oob, settings)
		defer client.Cancel()
		key := ContractKey{Destination: DestinationId(destination)}
		client.ContractManager().CreateContract(key, 0, 1024)
		<-oob.entered
		// The sender retires this exact generation while the successful OOB
		// response is already being handed to its manager callback.
		if flush {
			client.ContractManager().FlushContractQueue(key, true)
		}
		cancel()
		clientJoined := make(chan struct{})
		allJoined := make(chan error, 1)
		go func() {
			err := client.CloseAndWait(context.Background())
			close(clientJoined)
			allJoined <- errors.Join(err, owner.CloseAndWait(context.Background()))
		}()
		synctest.Wait()
		premature := false
		select {
		case <-clientJoined:
			premature = true
		default:
		}
		unblock()
		if err := <-allJoined; err != nil {
			t.Fatal(err)
		}
		if premature {
			t.Error("client retirement passed a callback that still owns a returned contract")
		}
		if creates.Load() != 1 || closes.Load() != 1 {
			t.Fatalf("post-check create=%d close=%d; want one create and one requester close before OOB retirement", creates.Load(), closes.Load())
		}
	})
}
