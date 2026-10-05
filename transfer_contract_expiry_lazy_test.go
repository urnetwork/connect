package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func expiryManagerWorkers(manager *ContractManager) int {
	manager.workers.stateLock.Lock()
	defer manager.workers.stateLock.Unlock()
	return manager.workers.activeCount
}

// A native ControlId client matches the contract-free manager inside each
// server resident. Empty polling and invalid results must not start a janitor.
func TestContractExpiryNoWorkerBeforeFirstQueuedContract(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const count = 128
		clients := make([]*Client, 0, count)
		defer func() {
			for _, client := range clients {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
		for range count {
			client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), closeWaitClientSettings())
			clients = append(clients, client)
			manager := client.ContractManager()
			key := ContractKey{Destination: DestinationId(NewId())}
			if manager.TakeContract(t.Context(), key, 0) != nil {
				t.Fatal("empty manager returned a contract")
			}
			if err := manager.addContract(key, &protocol.Contract{StoredContractBytes: []byte{0xff}}); err == nil {
				t.Fatal("invalid contract was accepted")
			}
			wrongSource := expiryContractFrame(t, NewId(), key.Destination.DestinationId, NewId())
			reportedTrust := false
			remove := manager.addContractStatusDispatchCallback(func(status *ContractStatus) {
				reportedTrust = status.Key == key && status.Error != nil && *status.Error == protocol.ContractError_Trust
			})
			err := manager.HandleControlFrame(key, wrongSource)
			remove()
			MessagePoolReturn(wrongSource.MessageBytes)
			if err != nil || !reportedTrust || expiryQueueCount(manager) != 0 {
				t.Fatal("another client's contract did not report trust rejection and release its empty queue")
			}
		}
		synctest.Wait()
		workers := 0
		for _, client := range clients {
			workers += expiryManagerWorkers(client.ContractManager())
		}
		if workers != 0 {
			t.Fatalf("%d contract-free native clients retained %d manager workers; want zero", count, workers)
		}
	})
}

func expiryContractFrame(t *testing.T, source, destination, contractID Id) *protocol.Frame {
	t.Helper()
	storedBytes, err := ProtoMarshal(&protocol.StoredContract{
		ContractId: contractID.Bytes(), SourceId: source.Bytes(), DestinationId: destination.Bytes(),
		TransferByteCount: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(storedBytes)
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{
		StoredContractBytes: storedBytes, ProvideMode: protocol.ProvideMode_Public,
	}}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func expiryQueueCount(manager *ContractManager) int {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	return len(manager.destinationContracts)
}

// This drives the actual decoded result, periodic expiry, and client join.
// The janitor is still present as soon as there is a reservation to retire.
func TestContractExpiryFirstQueuedResultStillExpires(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		settings := closeWaitClientSettings()
		settings.ContractManagerSettings.ContractQueueExpireTimeout = time.Second
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		manager := client.ContractManager()
		destination := NewId()
		frame := expiryContractFrame(t, ControlId, destination, NewId())
		defer MessagePoolReturn(frame.MessageBytes)
		if err := manager.HandleControlFrame(ContractKey{Destination: DestinationId(destination)}, frame); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := expiryManagerWorkers(manager); got != 1 {
			t.Fatalf("workers after first reservation = %d, want one expiry owner", got)
		}
		if got := expiryQueueCount(manager); got != 1 {
			t.Fatalf("queued reservations = %d, want one", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := expiryQueueCount(manager); got != 0 {
			t.Fatalf("orphan queue remained after existing expiry budget: %d", got)
		}
	})
}

type expiryHeldCloseOob struct {
	t       *testing.T
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mutex   sync.Mutex
	closed  map[Id]int
}

func (self *expiryHeldCloseOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.SendControlWithCtx(context.Background(), frames, callback)
}

func (self *expiryHeldCloseOob) SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction) {
	for _, frame := range frames {
		message, err := FromFrame(frame)
		MessagePoolReturn(frame.MessageBytes)
		if err != nil {
			self.t.Error(err)
			continue
		}
		closeContract, ok := message.(*protocol.CloseContract)
		if !ok {
			self.t.Errorf("unexpected cleanup frame %T", message)
			continue
		}
		if ctx.Err() != nil || closeContract.AckedByteCount != 0 || closeContract.UnackedByteCount != 0 || closeContract.Checkpoint {
			self.t.Error("pending reservation cleanup lost its original zero-byte, uncanceled authority")
		}
		self.mutex.Lock()
		self.closed[Id(closeContract.ContractId)]++
		self.mutex.Unlock()
	}
	self.once.Do(func() { close(self.entered) })
	<-self.release
	if callback != nil {
		callback(nil, nil)
	}
}

// Concurrent first results share one owner. Disabling periodic expiry must
// still retain that owner through every final out-of-band financial close.
func TestContractExpiryConcurrentFirstResultsJoinFinalFlush(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	for _, timeout := range []time.Duration{-time.Second, 0, time.Hour} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				oob := &expiryHeldCloseOob{t: t, entered: make(chan struct{}), release: make(chan struct{}), closed: map[Id]int{}}
				settings := closeWaitClientSettings()
				settings.ContractManagerSettings.ContractQueueExpireTimeout = timeout
				client := NewClient(t.Context(), ControlId, oob, settings)
				var release sync.Once
				defer func() {
					release.Do(func() { close(oob.release) })
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
				}()
				manager := client.ContractManager()
				const count = 64
				var publishers sync.WaitGroup
				ids := make([]Id, count)
				for index := range count {
					destination := NewId()
					ids[index] = NewId()
					frame := expiryContractFrame(t, ControlId, destination, ids[index])
					publishers.Go(func() {
						defer MessagePoolReturn(frame.MessageBytes)
						if err := manager.HandleControlFrame(ContractKey{Destination: DestinationId(destination)}, frame); err != nil {
							t.Error(err)
						}
					})
				}
				publishers.Wait()
				synctest.Wait()
				if got := expiryManagerWorkers(manager); got != 1 {
					t.Fatalf("concurrent first results launched %d workers; want one", got)
				}
				joined := make(chan error, 1)
				go func() { joined <- client.CloseAndWait(context.Background()) }()
				<-oob.entered
				synctest.Wait()
				select {
				case err := <-joined:
					t.Fatalf("client joined before the final financial close was released: %v", err)
				default:
				}
				release.Do(func() { close(oob.release) })
				if err := <-joined; err != nil {
					t.Fatal(err)
				}
				for _, id := range ids {
					if got := oob.closed[id]; got != 1 {
						t.Fatalf("reservation close count = %d, want exactly one", got)
					}
				}
				if expiryQueueCount(manager) != 0 || expiryManagerWorkers(manager) != 0 {
					t.Fatal("joined manager retained queued contracts or workers")
				}
			})
		})
	}
}

// A real CreateContract callback targets the exact queue generation that
// requested it. Only a successfully queued result starts the expiry owner.
func TestContractExpiryOwnedCreateResultStartsOnlyForLiveQueue(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	for _, retireQueue := range []bool{false, true} {
		name := "live"
		if retireQueue {
			name = "retired"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				oob := &pendingContractCreates{}
				client := NewClient(t.Context(), ControlId, oob, closeWaitClientSettings())
				defer func() {
					client.Cancel()
					oob.cancelPending()
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
				}()
				manager := client.ContractManager()
				destination := NewId()
				key := ContractKey{Destination: DestinationId(destination)}
				manager.CreateContract(key, 0, 1024)
				if oob.count() != 1 {
					t.Fatal("real create request did not reach OOB owner")
				}
				if retireQueue {
					manager.FlushContractQueue(key, true)
				}
				frame := expiryContractFrame(t, ControlId, destination, NewId())
				oob.completeFrames(0, []*protocol.Frame{frame}, nil)
				MessagePoolReturn(frame.MessageBytes)
				synctest.Wait()
				want := 1
				if retireQueue {
					want = 0
				}
				if workers, queues := expiryManagerWorkers(manager), expiryQueueCount(manager); workers != want || queues != want {
					t.Fatalf("completed create has %d workers and %d queues, want %d each", workers, queues, want)
				}
			})
		})
	}
}

// A decoded result paused before the queue lock cannot publish work after
// Close wins. This covers the initial, still-dormant expiry owner boundary.
func TestContractExpiryCloseWinsBeforeFirstResult(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		oob := &expiryHeldCloseOob{t: t, entered: make(chan struct{}), release: make(chan struct{}), closed: map[Id]int{}}
		close(oob.release)
		client := NewClient(t.Context(), ControlId, oob, closeWaitClientSettings())
		defer func() { _ = client.CloseAndWait(context.Background()) }()
		manager := client.ContractManager()
		entered, release := make(chan struct{}), make(chan struct{})
		manager.testingBeforeOpenContractQueueLock = func(ContractKey) { close(entered); <-release }
		destination := NewId()
		contractID := NewId()
		frame := expiryContractFrame(t, ControlId, destination, contractID)
		defer MessagePoolReturn(frame.MessageBytes)
		result := make(chan error, 1)
		go func() {
			result <- manager.HandleControlFrame(ContractKey{Destination: DestinationId(destination)}, frame)
		}()
		<-entered
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-result; err != nil {
			t.Fatalf("retired result cleanup failed: %v", err)
		}
		if got := oob.closed[contractID]; got != 1 {
			t.Fatalf("post-close first result retired %d times; want exactly one", got)
		}
		if expiryQueueCount(manager) != 0 || expiryManagerWorkers(manager) != 0 {
			t.Fatal("post-close first result retained a queue or owner")
		}
	})
}
