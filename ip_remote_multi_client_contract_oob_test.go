// Contract fixtures preserve their response behavior with optional request
// notifications, including repeated requests from independent send sequences.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// Omitting the request observer must preserve both silent and granted replies.
func TestEvaluationContractOobRequestNotificationIsOptional(t *testing.T) {
	for _, ready := range []bool{false, true} {
		oob := &evaluationContractOob{
			ready: ready,
			grant: &contractErrorOob{clientId: NewId()},
		}
		callbackCount := 0
		for range 2 {
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.CreateContract{
				DestinationId:     NewId().Bytes(),
				TransferByteCount: 1024,
			})
			oob.SendControl([]*protocol.Frame{frame}, func(frames []*protocol.Frame, err error) {
				callbackCount++
				if err != nil {
					t.Fatalf("ready=%t: control error=%v", ready, err)
				}
				if !ready {
					if len(frames) != 0 {
						t.Fatalf("silent service returned %d frames", len(frames))
					}
					return
				}
				if len(frames) != 1 {
					t.Fatalf("grant service returned %d frames", len(frames))
				}
				message, err := FromFrame(frames[0])
				if err != nil {
					t.Fatalf("decode grant: %v", err)
				}
				result, ok := message.(*protocol.CreateContractResult)
				if !ok || result.Error != nil || result.Contract == nil {
					t.Fatalf("grant service returned %T without a contract", message)
				}
			})
		}
		if got := oob.requests.Load(); got != 2 || callbackCount != 2 {
			t.Fatalf("ready=%t: requests=%d callbacks=%d", ready, got, callbackCount)
		}
	}
}

// Only contract requests notify, and simultaneous requests close the edge once.
func TestEvaluationContractOobRequestNotificationIsOneShot(t *testing.T) {
	requested := make(chan struct{})
	oob := &evaluationContractOob{requested: requested}
	var callbackCount atomic.Int64
	callback := func(frames []*protocol.Frame, err error) {
		callbackCount.Add(1)
		if len(frames) != 0 || err != nil {
			t.Errorf("silent service returned frames=%d error=%v", len(frames), err)
		}
	}
	ping := RequireToFrameWithDefaultProtocolVersion(&protocol.IpPing{})
	oob.SendControl([]*protocol.Frame{ping}, callback)
	if got := oob.requests.Load(); got != 0 {
		t.Fatalf("unrelated frame counted as %d contract requests", got)
	}
	select {
	case <-requested:
		t.Fatal("unrelated frame notified a contract request")
	default:
	}

	const requestCount = 8
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range requestCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.CreateContract{
				DestinationId:     NewId().Bytes(),
				TransferByteCount: 1024,
			})
			<-start
			oob.SendControl([]*protocol.Frame{frame}, callback)
		}()
	}
	close(start)
	workers.Wait()
	select {
	case <-requested:
	default:
		t.Fatal("contract requests did not notify")
	}
	if got := oob.requests.Load(); got != requestCount || callbackCount.Load() != requestCount+1 {
		t.Fatalf("requests=%d callbacks=%d", got, callbackCount.Load())
	}
}
