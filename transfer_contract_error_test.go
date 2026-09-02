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
// models the platform answering a retryable account/setup request with the
// legacy InsufficientBalance result, one error frame per request.
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
			// Legacy and account failures use InsufficientBalance. A distinct
			// Reliability result is tested at the multi-client window boundary;
			// it is intentionally terminal for that selected route.
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
// This remains a normal path in live setups: balance and legacy setup failures
// can look terminal for a few attempts and then start granting. That is why a
// generic client-side fail-fast on repeated error results is wrong: a
// 3-consecutive-errors exit was tried here and broke five server/proxy
// integration tests whose contract acquisition legitimately errors before
// succeeding. Only the platform's explicit ContractError_Reliability verdict
// is allowed to retire a multi-client route.
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
		NewId(),
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
