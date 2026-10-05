package connect

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Retain the second successful response after the real local API has decoded
// it and made its cancellation decision, but before the manager consumes it.
type contextGapContractOob struct {
	*ApiOutOfBandControl
	results        atomic.Int32
	firstReturned  chan struct{}
	secondEntered  chan struct{}
	releaseSecond  chan struct{}
	secondReturned chan struct{}
}

func (o *contextGapContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	o.ApiOutOfBandControl.SendControl(frames, func(result []*protocol.Frame, err error) {
		if err == nil && len(result) == 1 && result[0].MessageType == protocol.MessageType_TransferCreateContractResult {
			switch o.results.Add(1) {
			case 1:
				callback(result, err)
				close(o.firstReturned)
				return
			case 2:
				close(o.secondEntered)
				<-o.releaseSecond
				callback(result, err)
				close(o.secondReturned)
				return
			}
		}
		callback(result, err)
	})
}

func contextGapWait(t *testing.T, ctx context.Context, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
	}
}

func contextGapManagerSnapshot(manager *ContractManager) (closed bool, queues, contracts, workers int) {
	manager.mutex.Lock()
	closed = manager.closed
	queues = len(manager.destinationContracts)
	for _, queue := range manager.destinationContracts {
		queue.mutex.Lock()
		contracts += len(queue.contracts)
		queue.mutex.Unlock()
	}
	manager.mutex.Unlock()
	manager.workers.stateLock.Lock()
	workers = manager.workers.activeCount
	manager.workers.stateLock.Unlock()
	return
}

// This is an actual public Client.Close blocked on StreamBuffer.Close's real
// mutex, after client cancellation and before ContractManager.Close. No
// production hook or replacement Close method supplies the interval.
func contextGapWaitPublicCloseAtStreamLock(t *testing.T, ctx context.Context) {
	t.Helper()
	stackBytes := make([]byte, 1<<20)
	for ctx.Err() == nil {
		n := runtime.Stack(stackBytes, true)
		for _, stack := range strings.Split(string(stackBytes[:n]), "\n\n") {
			if strings.Contains(stack, "(*StreamBuffer).Close(") &&
				strings.Contains(stack, "(*Client).Close(") &&
				strings.Contains(stack, "sync.(*Mutex).Lock(") &&
				strings.Contains(stack, "testClientCloseContractPublication.func") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("public Client.Close did not wait on the stream retirement mutex")
}

func TestClientCloseContextBeforeContractManagerRetirementClosesLateReservation(t *testing.T) {
	testClientCloseContractPublication(t, 120*time.Second, true)
}

// A live callback must still publish both reservations. Expiry-disabled
// managers retain the same shutdown ownership even without a periodic tick.
func TestContractPublicationPreservesLiveResultsAndDisabledExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiry    time.Duration
		holdClose bool
	}{
		{"live_callbacks", 120 * time.Second, false},
		{"zero_expiry_close_gap", 0, true},
		{"negative_expiry_close_gap", -time.Second, true},
		{"zero_expiry_live_callbacks", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) { testClientCloseContractPublication(t, tc.expiry, tc.holdClose) })
	}
}

func testClientCloseContractPublication(t *testing.T, expiry time.Duration, holdClose bool) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	apiCtx, stopAPI := context.WithCancel(context.Background())
	api, strategy := authObservationTestApi(apiCtx, nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("native local contract control escaped to HTTP")
		return nil, errors.New("forbidden")
	}))
	strategy.settings.RequestTimeout = 5 * time.Second
	source, destination := ControlId, NewId()
	firstID, secondID := NewId(), NewId()
	var creates atomic.Int32
	var closeLock sync.Mutex
	closedIDs := map[Id]int{}
	firstClosed := make(chan struct{})
	var firstCloseOnce sync.Once
	owner := NewApiOutOfBandControlWithLocalControl(apiCtx, strategy, lateLocalJwt(t, source), "https://local.invalid", privateLocalControl(func(callCtx context.Context, _ string, args *ConnectControlArgs) (*ConnectControlResult, error) {
		for _, message := range lateLocalMessages(t, args) {
			switch message := message.(type) {
			case *protocol.CreateContract:
				if creates.Add(1) == 1 {
					return lateLocalResult(t, source, destination, firstID), nil
				}
				return lateLocalResult(t, source, destination, secondID), nil
			case *protocol.CloseContract:
				id, err := IdFromBytes(message.ContractId)
				if err != nil {
					return nil, err
				}
				if callCtx.Err() != nil || message.AckedByteCount != 0 || message.UnackedByteCount != 0 || message.Checkpoint {
					t.Error("shutdown financial close changed its live zero-use authority")
				}
				closeLock.Lock()
				closedIDs[id]++
				closeLock.Unlock()
				if id == firstID {
					firstCloseOnce.Do(func() { close(firstClosed) })
				}
			default:
				t.Errorf("unexpected local control %T", message)
			}
		}
		return &ConnectControlResult{}, nil
	}))
	oob := &contextGapContractOob{ApiOutOfBandControl: owner, firstReturned: make(chan struct{}), secondEntered: make(chan struct{}), releaseSecond: make(chan struct{}), secondReturned: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(oob.releaseSecond) }) }
	clientCtx, stopClient := context.WithCancel(context.Background())
	settings := DefaultClientSettingsWithBufferSize(4096)
	settings.Log = NewNoopLogger()
	settings.ContractManagerSettings.ContractQueueExpireTimeout = expiry
	settings.beforeClientKeyPublishForTest = func() { <-clientCtx.Done() }
	client := NewClient(clientCtx, source, oob, settings)
	manager := client.ContractManager()
	stream := client.streamManager.streamBuffer
	streamLocked := false
	t.Cleanup(func() {
		if streamLocked {
			stream.managementStateLock.Unlock()
			streamLocked = false
		}
		release()
		stopClient()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := client.CloseAndWait(cleanupCtx); err != nil {
			t.Error(err)
		}
		if err := owner.CloseAndWait(cleanupCtx); err != nil {
			t.Error(err)
		}
		api.Close()
		stopAPI()
	})
	key := ContractKey{Destination: DestinationId(destination)}
	manager.CreateContract(key, 0, 1024)
	contextGapWait(t, ctx, oob.firstReturned, "first real contract callback")
	closed, queues, contracts, workers := contextGapManagerSnapshot(manager)
	if closed || queues != 1 || contracts != 1 || workers != 1 {
		t.Fatalf("first reservation not queued with janitor: closed=%t queues=%d contracts=%d workers=%d", closed, queues, contracts, workers)
	}
	manager.CreateContract(key, 1, 1024)
	contextGapWait(t, ctx, oob.secondEntered, "second decoded owned OOB result")
	if _, _, _, workers = contextGapManagerSnapshot(manager); workers != 2 {
		t.Fatalf("second callback not owned alongside janitor: %d", workers)
	}

	closeReturned := make(chan struct{})
	if holdClose {
		stream.managementStateLock.Lock()
		streamLocked = true
		go func() { client.Close(); close(closeReturned) }()
		contextGapWait(t, ctx, client.Done(), "public client cancellation")
		contextGapWaitPublicCloseAtStreamLock(t, ctx)
		contextGapWait(t, ctx, firstClosed, "first reservation financial shutdown close")
		// The outstanding callback is the only remaining manager worker once
		// the real expiry loop has finished finalFlush.
		for ctx.Err() == nil {
			closed, queues, contracts, workers = contextGapManagerSnapshot(manager)
			if workers == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if closed || contracts != 0 || workers != 1 {
			t.Fatalf("did not reach context-before-manager-close boundary: closed=%t queues=%d contracts=%d workers=%d", closed, queues, contracts, workers)
		}
		t.Logf("public Close blocked after cancel; finalFlush complete; manager closed=%t queues=%d contracts=%d workers=%d", closed, queues, contracts, workers)
	}
	release()
	contextGapWait(t, ctx, oob.secondReturned, "late reservation manager callback")
	closed, queues, contracts, workers = contextGapManagerSnapshot(manager)
	t.Logf("late callback returned before manager.Close: closed=%t queues=%d contracts=%d workers=%d", closed, queues, contracts, workers)
	if holdClose {
		stream.managementStateLock.Unlock()
		streamLocked = false
	} else {
		if closed || queues != 1 || contracts != 2 || workers != 1 {
			t.Fatalf("healthy callbacks failed to publish both reservations: closed=%t queues=%d contracts=%d workers=%d", closed, queues, contracts, workers)
		}
		go func() { client.Close(); close(closeReturned) }()
	}
	contextGapWait(t, ctx, closeReturned, "public Client.Close completion")
	// The publisher fixture waits on the external parent rather than the
	// client's derived context. Release it only after public Close has finished
	// the interval under test, before the complete lifecycle join.
	stopClient()
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	// Preserve the production ownership order: Client callback ownership joins
	// before the independent external OOB's final close/ownership join.
	if err := owner.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	closed, queues, contracts, workers = contextGapManagerSnapshot(manager)
	closeLock.Lock()
	firstCloses, secondCloses, distinctCloses := closedIDs[firstID], closedIDs[secondID], len(closedIDs)
	closeLock.Unlock()
	t.Logf("joined: creates=%d first_closes=%d second_closes=%d closed=%t queues=%d contracts=%d workers=%d", creates.Load(), firstCloses, secondCloses, closed, queues, contracts, workers)
	if creates.Load() != 2 || firstCloses != 1 || secondCloses != 1 || distinctCloses != 2 {
		t.Errorf("financial closes after full joins: creates=%d first=%d second=%d distinct=%d; want 2/1/1/2", creates.Load(), firstCloses, secondCloses, distinctCloses)
	}
	if !closed || queues != 0 || contracts != 0 || workers != 0 {
		t.Errorf("retired manager retained reservation after full joins: closed=%t queues=%d contracts=%d workers=%d", closed, queues, contracts, workers)
	}
}
