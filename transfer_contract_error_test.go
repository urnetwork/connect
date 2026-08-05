package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// contractErrorOob answers every CreateContract with a terminal error result
// until `errorCount` errors have been sent, then grants valid contracts. It
// models the platform answering an unsatisfiable request — no balance, or a
// companion request whose origin contract does not exist — which it does with
// one error frame per request.
type contractErrorOob struct {
	clientId Id
	// answer this many requests with an error; < 0 means all of them
	errorCount int64
	errorsSent atomic.Int64
}

func (self *contractErrorOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	var out []*protocol.Frame
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		createContract, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}

		if self.errorCount < 0 || self.errorsSent.Load() < self.errorCount {
			self.errorsSent.Add(1)
			// the platform's answer for every unsatisfiable cause (see
			// controller nextContract: "The client sees only
			// InsufficientBalance, including unrelated failures")
			contractError := protocol.ContractError_InsufficientBalance
			result := &protocol.CreateContractResult{
				Error: &contractError,
			}
			if resultFrame, err := ToFrame(result, DefaultProtocolVersion); err == nil {
				out = append(out, resultFrame)
			}
			continue
		}

		// grant: the send side verifies only that the contract's source is
		// this client (the receiver does the cryptographic verification)
		destinationId, err := IdFromBytes(createContract.DestinationId)
		if err != nil {
			continue
		}
		storedContract := &protocol.StoredContract{
			ContractId:        NewId().Bytes(),
			TransferByteCount: createContract.TransferByteCount,
			SourceId:          self.clientId.Bytes(),
			DestinationId:     destinationId.Bytes(),
		}
		storedContractBytes, err := ProtoMarshal(storedContract)
		if err != nil {
			continue
		}
		result := &protocol.CreateContractResult{
			Contract: &protocol.Contract{
				StoredContractBytes: storedContractBytes,
				ProvideMode:         protocol.ProvideMode_Network,
			},
		}
		if resultFrame, err := ToFrame(result, DefaultProtocolVersion); err == nil {
			out = append(out, resultFrame)
		}
	}
	callback(out, nil)
}

func contractErrorTestSettings() *ClientSettings {
	settings := DefaultClientSettings()
	// short retries against a much longer overall budget: the assertion is
	// about WHICH bound ends the wait, so keep them far apart
	settings.SendBufferSettings.CreateContractTimeout = 20 * time.Second
	settings.SendBufferSettings.CreateContractRetryInterval = 100 * time.Millisecond
	settings.SendBufferSettings.CreateContractRetryMaxInterval = 200 * time.Millisecond
	settings.SendBufferSettings.AckTimeout = 60 * time.Second
	return settings
}

// TestSendSequenceSurvivesTransientContractErrors pins that transient error
// results followed by a granted contract must not break the send.
//
// This is the normal path in live setups: the platform answers CreateContract
// with terminal-LOOKING errors during setup races (balance/provide
// propagation; a companion request that beats its origin contract) and then
// starts granting. The platform collapses every cause into
// ContractError_InsufficientBalance on the wire, so the client cannot tell
// hopeless from not-yet — which is precisely why a client-side fail-fast on
// repeated error results is WRONG: a 3-consecutive-errors exit was tried here
// and broke five server/proxy integration tests whose contract acquisition
// legitimately errors a few times before succeeding. The 30s blind wait it
// targeted (see the create loop's CreateContractTimeout) is instead mitigated
// at the detection layer, which reclaims a window client whose return path is
// dead regardless of cause. Do not reintroduce a fail-fast without a
// platform-side error-cause distinction on the wire.
func TestSendSequenceSurvivesTransientContractErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientId := NewId()
	oob := &contractErrorOob{
		clientId:   clientId,
		errorCount: 2,
	}
	settings := contractErrorTestSettings()
	client := NewClient(ctx, clientId, oob, settings)
	defer client.Cancel()

	out := make(chan []byte, 1024)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{out})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-out:
			}
		}
	}()

	frame, err := ToFrame(&protocol.SimpleMessage{Content: "data"}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}

	sent := make(chan struct{}, 1)
	success := client.SendWithTimeout(
		frame,
		DestinationId(NewId()),
		func(err error) {},
		-1,
	)
	if !success {
		t.Fatal("the send must enqueue")
	}

	// the sequence took a contract when the granted result lands: observe the
	// wire (the pack goes out only under a contract)
	go func() {
		// drain is in the main goroutine above; here poll for the contract
		// having been granted
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if 2 <= oob.errorsSent.Load() {
				select {
				case sent <- struct{}{}:
				default:
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the transient errors were never even requested through")
	}

	// the real assertion: the sequence must still acquire the granted
	// contract rather than having failed fast on the transient errors
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		contractKey := ContractKey{
			Destination: DestinationId(clientId),
		}
		_ = contractKey
		stats := client.ContractManager().LocalStats()
		if 0 < stats.ContractOpenCount {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("transient errors below the terminal count must be forgiven: the granted contract was never taken, so the wait failed fast on a transient")
}
