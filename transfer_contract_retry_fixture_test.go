// A control service and client fixture for contract request pacing. The tests
// that use it run a real Client and send sequence in a synctest bubble against
// a control service that answers synchronously, so every request time is exact
// virtual time. The bubble also keeps the process-wide degraded-backend gate
// honest: a failure stamped outside it carries a real-time clock, which reads
// as not recent inside it.
package connect

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// How the control service answers one contract request.
type contractRetryAnswer int

const (
	// a completed round trip whose result is an error, as a deadline or
	// balance refusal is
	contractRetryRefuse contractRetryAnswer = iota
	// a failed round trip with a 429, as a hosted owner's contract admission
	// bound answers
	contractRetryRateLimit
	// a valid contract
	contractRetryGrant
)

// One contract request as the control service saw it.
type contractRetryRequest struct {
	destinationId Id
	time          time.Time
}

// Answers each contract request by its arrival index and records when it
// arrived. Takes the frames, like every OutOfBandControl, and calls back
// before it returns. Other control is answered empty.
type contractRetryOob struct {
	clientId Id
	answer   func(index int) contractRetryAnswer

	stateLock sync.Mutex
	requests  []contractRetryRequest
}

func (self *contractRetryOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	var resultFrames []*protocol.Frame
	var resultErr error
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, frame := range resultFrames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		createContract, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		destinationId, err := IdFromBytes(createContract.DestinationId)
		if err != nil {
			continue
		}
		index := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.requests = append(self.requests, contractRetryRequest{
				destinationId: destinationId,
				time:          time.Now(),
			})
			return len(self.requests) - 1
		}()
		switch self.answer(index) {
		case contractRetryRateLimit:
			resultErr = &HttpStatusError{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Body:       []byte("hosted contract creation is at its concurrent bound"),
			}
		case contractRetryGrant:
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
				resultFrames = append(resultFrames, resultFrame)
			}
			MessagePoolReturn(storedContractBytes)
		default:
			contractError := protocol.ContractError_Setup
			result := &protocol.CreateContractResult{
				Error: &contractError,
			}
			if resultFrame, err := ToFrame(result, DefaultProtocolVersion); err == nil {
				resultFrames = append(resultFrames, resultFrame)
			}
		}
	}
	if resultErr != nil {
		callback(nil, resultErr)
		return
	}
	callback(resultFrames, nil)
}

// The times of the requests for `destinationId`, as offsets from `start`.
func (self *contractRetryOob) requestOffsets(destinationId Id, start time.Time) []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	offsets := []time.Duration{}
	for _, request := range self.requests {
		if request.destinationId == destinationId {
			offsets = append(offsets, request.time.Sub(start))
		}
	}
	return offsets
}

// A client whose contract pacing each test sets explicitly: the 1 to 5 s
// retry schedule in a 30 s budget, encryption and the control ping off, so
// the only control traffic is contracts.
func contractRetryTestSettings() *ClientSettings {
	settings := closeWaitClientSettings()
	// a bubble's clock starts in 2000, before contracts were required
	settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(0, 0)
	settings.SendBufferSettings.CreateContractTimeout = 30 * time.Second
	settings.SendBufferSettings.CreateContractRetryInterval = time.Second
	settings.SendBufferSettings.CreateContractRetryMaxInterval = 5 * time.Second
	return settings
}

// Starts a client on `oob` with a buffered gateway route. Cleanup closes and
// joins it, then returns what the route still holds.
func newContractRetryTestClient(t *testing.T, oob *contractRetryOob, settings *ClientSettings) *Client {
	t.Helper()
	return newContractTestClientWithOob(t, oob.clientId, oob, settings)
}

// newContractRetryTestClient for any control service.
func newContractTestClientWithOob(t *testing.T, clientId Id, oob OutOfBandControl, settings *ClientSettings) *Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, clientId, oob, settings)
	route := make(chan []byte, 1024)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{route})
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close contract retry client: %v", err)
		}
		for {
			select {
			case message := <-route:
				MessagePoolReturn(message)
			default:
				return
			}
		}
	})
	return client
}

// Queues one message for `destinationId` and returns where its ack lands.
func sendContractRetryMessage(t *testing.T, client *Client, destinationId Id) chan error {
	t.Helper()
	return sendContractRetryContent(t, client, destinationId, "contract retry")
}

// sendContractRetryMessage with the message's `content`, which sets how much of
// its contract the message uses.
func sendContractRetryContent(t *testing.T, client *Client, destinationId Id, content string) chan error {
	t.Helper()
	frame, err := ToFrame(&protocol.SimpleMessage{Content: content}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	ackErrs := make(chan error, 1)
	ack := func(err error) {
		select {
		case ackErrs <- err:
		default:
		}
	}
	if !client.SendWithTimeout(frame, destinationId, ack, -1) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("the send must enqueue")
	}
	return ackErrs
}

func requireContractRetryOffsets(t *testing.T, name string, got []time.Duration, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s requests at %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s requests at %v, want %v", name, got, want)
		}
	}
}
