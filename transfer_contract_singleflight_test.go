// Pending contract creates belong to one queue generation and request shape.
// A slow control response must not multiply the same sequence's reservations.
package connect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Owns decoded requests and their completion callbacks until the test releases
// them. No socket, clock, or goroutine can accidentally complete a request.
type pendingContractCreates struct {
	stateLock sync.Mutex
	requests  []*protocol.CreateContract
	callbacks []OobResultFunction
}

// Takes the input frames; the synchronous completion of non-create controls
// keeps unrelated client lifecycle work outside the held-request boundary.
func (self *pendingContractCreates) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	var request *protocol.CreateContract
	for _, frame := range frames {
		message, err := FromFrame(frame)
		MessagePoolReturn(frame.MessageBytes)
		if err == nil {
			if create, ok := message.(*protocol.CreateContract); ok {
				request = create
			}
		}
	}
	if request == nil {
		callback(nil, nil)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, request)
	self.callbacks = append(self.callbacks, callback)
}

// Counts actual out-of-band requests, not attempted local admissions.
func (self *pendingContractCreates) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.requests)
}

// Completes exactly one request outside the fixture lock, including all of its
// callback-owned queue cleanup before returning to the test.
func (self *pendingContractCreates) complete(index int, err error) {
	self.completeFrames(index, nil, err)
}

// Borrows the response frames through the synchronously joined callback.
func (self *pendingContractCreates) completeFrames(index int, frames []*protocol.Frame, err error) {
	callback := func() OobResultFunction {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		callback := self.callbacks[index]
		self.callbacks[index] = nil
		return callback
	}()
	if callback != nil {
		callback(frames, err)
	}
}

// Releases every outstanding external callback after the client is canceled.
func (self *pendingContractCreates) cancelPending() {
	for index := 0; index < self.count(); index++ {
		self.complete(index, context.Canceled)
	}
}

// Uses a real client and contract manager; every owner is joined in cleanup.
func newPendingContractClient(t *testing.T, settings *ClientSettings) (*Client, *pendingContractCreates) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	oob := &pendingContractCreates{}
	if settings == nil {
		settings = DefaultClientSettings()
	}
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	client := NewClient(ctx, NewId(), oob, settings)
	t.Cleanup(func() {
		cancel()
		client.Cancel()
		oob.cancelPending()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		if err := client.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
	})
	return client, oob
}

// The real sequence's fourth queue wait occurs after its 1s, 2s, and 4s retry
// intervals. Its original OOB request is still pending at that exact barrier.
// A fresh subprocess isolates the legacy process-wide backend-health signal.
func TestSendSequenceCoalescesPendingContractCreates(t *testing.T) {
	const childEnv = "URNETWORK_TEST_CONTRACT_CREATE_FLIGHT"
	if os.Getenv(childEnv) == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.timeout=25s")
		command.Env = append(os.Environ(), childEnv+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated sequence admission: %v\n%s", err, output)
		}
		return
	}
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		oob := &pendingContractCreates{}
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.ControlPingTimeout = 0
		// Virtual time starts before production's historical activation date.
		settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
		waitEntered, releaseWait := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		destination := NewId()
		waitCount := 0
		settings.SendBufferSettings.beforeTakeContractForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				waitCount++
				if waitCount == 4 {
					close(waitEntered)
					<-releaseWait
				}
			}
		}
		client := NewClient(ctx, NewId(), oob, settings)
		route := make(chan []byte, 16)
		client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{route})
		defer func() {
			cancel()
			client.Cancel()
			releaseOnce.Do(func() { close(releaseWait) })
			oob.cancelPending()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for {
				select {
				case packet := <-route:
					MessagePoolReturn(packet)
				default:
					return
				}
			}
		}()
		frame, err := ToFrame(&protocol.SimpleMessage{Content: "pending control response"}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !client.SendWithTimeout(frame, destination, func(error) {}, -1) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("sequence did not admit its first message")
		}
		<-waitEntered
		if got := oob.count(); got != 1 {
			t.Fatalf("one pending opening request multiplied into %d OOB calls before any response", got)
		}
	})
}

// Definitive transport failures and completed empty responses release local
// admission; no local cache may suppress the caller's later retry.
func TestContractCreateCompletionReleasesSameRequest(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	key := ContractKey{Destination: DestinationId(NewId())}
	for _, result := range []error{context.DeadlineExceeded, errors.New("synthetic control failure"), nil} {
		before := oob.count()
		manager.CreateContract(key, 0, 1024)
		manager.CreateContract(key, 0, 1024)
		if got := oob.count(); got != before+1 {
			t.Fatalf("same pending request admitted %d calls, want one", got-before)
		}
		oob.complete(before, result)
	}
}

// Unrelated lanes, actual successor indices, and a larger wire reservation
// remain independent. Admission never changes the contract's requested bytes.
func TestContractCreateAdmissionPreservesIndependentRequests(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	key := ContractKey{Destination: DestinationId(NewId())}
	manager.CreateContract(key, 0, 1024)
	manager.CreateContract(key, 1, 1024)
	larger := manager.settings.InitialContractTransferByteCount + 1024
	manager.CreateContract(key, 0, larger)
	for _, other := range []ContractKey{
		{Destination: key.Destination, LogicalLane: 1},
		{Destination: key.Destination, EncryptionRole: sequenceTlsRoleServer},
		{Destination: key.Destination, EncryptionCompanion: true},
		{Destination: key.Destination, CompanionContract: true},
		{Destination: key.Destination, ForceStream: true},
		{Destination: DestinationId(NewId())},
	} {
		manager.CreateContract(other, 0, 1024)
	}
	if got := oob.count(); got != 9 {
		t.Fatalf("independent requests = %d, want nine", got)
	}
	if got := oob.requests[2].TransferByteCount; got != uint64(larger) {
		t.Fatalf("larger reservation = %d, want %d", got, larger)
	}
}

// A flushed queue's callback owns only that retired generation. It cannot
// reopen admission for a still-pending replacement at the same key.
func TestContractCreateOldCompletionCannotReleaseReplacement(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	key := ContractKey{Destination: DestinationId(NewId())}
	manager.CreateContract(key, 0, 1024)
	manager.FlushContractQueue(key, true)
	manager.CreateContract(key, 0, 1024)
	if got := oob.count(); got != 2 {
		t.Fatalf("replacement inherited old admission: requests=%d", got)
	}
	oob.complete(0, nil)
	manager.CreateContract(key, 0, 1024)
	if got := oob.count(); got != 2 {
		t.Fatalf("old completion released replacement admission: requests=%d", got)
	}
	oob.complete(1, nil)
	manager.CreateContract(key, 0, 1024)
	if got := oob.count(); got != 3 {
		t.Fatalf("replacement completion did not release admission: requests=%d", got)
	}
}

// Concurrent retry callers linearize at the queue that owns the pending RPC.
func TestContractCreateAdmissionIsAtomic(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	key := ContractKey{Destination: DestinationId(NewId())}
	var callers sync.WaitGroup
	for range 32 {
		callers.Go(func() { client.ContractManager().CreateContract(key, 0, 1024) })
	}
	callers.Wait()
	if got := oob.count(); got != 1 {
		t.Fatalf("concurrent same-request admissions=%d, want one", got)
	}
}

// Legacy queue normalization may combine lanes, but it must never combine
// different request identities within that shared generation.
func TestContractCreateLegacyQueuePreservesRequestLanes(t *testing.T) {
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.LegacyCreateContract = true
	client, oob := newPendingContractClient(t, settings)
	key := ContractKey{Destination: DestinationId(NewId())}
	other := key
	other.EncryptionRole = sequenceTlsRoleServer
	for range 2 {
		client.ContractManager().CreateContract(key, 0, 1024)
		client.ContractManager().CreateContract(other, 0, 1024)
	}
	if got := oob.count(); got != 2 {
		t.Fatalf("legacy queue request lanes admitted=%d, want two", got)
	}
}

// A contract can wake its consumer before its OOB callback returns. The
// successor prefetch must still run while identical retries remain coalesced.
func TestContractCreateCallbackKeepsAdmissionWithoutBlockingSuccessor(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	key := ContractKey{Destination: DestinationId(NewId())}
	manager.CreateContract(key, 0, 1024)
	storedBytes, err := ProtoMarshal(&protocol.StoredContract{
		ContractId:        NewId().Bytes(),
		SourceId:          client.ClientId().Bytes(),
		DestinationId:     key.Destination.DestinationId.Bytes(),
		TransferByteCount: uint64(manager.settings.InitialContractTransferByteCount),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(storedBytes)
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{
		StoredContractBytes: storedBytes,
		ProvideMode:         protocol.ProvideMode_Network,
	}}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	observed := false
	unsubscribe := manager.addContractStatusDispatchCallback(func(status *ContractStatus) {
		if status.Error != nil {
			t.Error("synthetic successful contract was rejected")
			return
		}
		observed = true
		if manager.TakeContract(t.Context(), key, 0) == nil {
			t.Error("response did not publish its contract before callback completion")
		}
		manager.CreateContract(key, 0, 1024)
		manager.CreateContract(key, 1, 1024)
		if got := oob.count(); got != 2 {
			t.Errorf("response callback admitted=%d, want original plus successor", got)
		}
	})
	defer unsubscribe()
	oob.completeFrames(0, []*protocol.Frame{frame}, nil)
	if !observed {
		t.Fatal("successful response did not reach the callback-owned boundary")
	}
}
