// Admission grants cannot let an obsolete control overtake its successor.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// A real queued retry receives the old holder's protocol grant while Run is
// parked. Publication cancels it, and the newer actual send may start only
// after that granted call returns; real platform delivery contains newest only.
func TestControlSyncGrantedAdmissionReturnsBeforeSuccessorAttempt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		blockerCtx, cancelBlocker := context.WithCancel(ctx)
		defer cancelBlocker()
		releaseRun := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseRun) }) }
		entered := make(chan sendSequenceId, 1)
		var runEntries atomic.Int32
		settings := terminalControlSettings()
		settings.SendBufferSettings.SequenceBufferSize = 0
		oldTerminalEntered := make(chan struct{}, 1)
		releaseOldTerminal := make(chan struct{})
		var releaseOldTerminalOnce sync.Once
		finishOldTerminal := func() { releaseOldTerminalOnce.Do(func() { close(releaseOldTerminal) }) }
		defer finishOldTerminal()
		var provideStarts atomic.Int32
		var eventOrder, oldRetryToken, oldRetryTerminal, newestAttempt atomic.Uint64
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			if event.DestinationId != ControlId || event.MessageType != protocol.MessageType_TransferProvide {
				return
			}
			order := eventOrder.Add(1)
			if event.Phase == SendPackLifecyclePhaseStarted {
				attempt := provideStarts.Add(1)
				if attempt == 2 {
					oldRetryToken.Store(event.Token)
				} else if attempt >= 3 {
					newestAttempt.CompareAndSwap(0, order)
				}
			} else if event.Phase == SendPackLifecyclePhaseTerminal && event.Token == oldRetryToken.Load() && event.Err != nil {
				oldRetryTerminal.Store(order)
				select {
				case oldTerminalEntered <- struct{}{}:
				default:
				}
				select {
				case <-releaseOldTerminal:
				case <-ctx.Done():
				}
			}
		}
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == ControlId {
				runEntries.Add(1)
				select {
				case entered <- id:
				default:
				}
				<-releaseRun
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		platform := NewClient(ctx, ControlId, NewNoContractClientOob(), terminalControlSettings())
		control := client.ContractManager().controlSyncProvide
		var callers sync.WaitGroup
		defer func() {
			cancel()
			release()
			callers.Wait()
			if err := control.closeAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, owner := range []*Client{client, platform} {
				if err := owner.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
		client.ContractManager().AddNoContractPeer(ControlId)
		platform.ContractManager().AddNoContractPeer(client.ClientId())
		outgoing, returning := make(Route), make(Route)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(ControlId)), []Route{outgoing})
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returning})
		platform.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{outgoing})
		platform.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(client.ClientId())), []Route{returning})
		var blockerDeliveries, oldDeliveries, newestDeliveries atomic.Int32
		var blockerAcks, oldAcks, newestAcks atomic.Int32
		newestAcknowledged := make(chan struct{}, 1)
		platform.AddReceiveCallback(func(path TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				message, err := FromFrame(frame)
				if err != nil || path.SourceId != client.ClientId() {
					t.Error("actual platform receiver lost the control source or encoding")
					continue
				}
				switch value := message.(type) {
				case *protocol.SimpleMessage:
					if value.MessageIndex == 7193 {
						blockerDeliveries.Add(1)
					} else {
						t.Error("unrelated message reached the controlled platform")
					}
				case *protocol.Provide:
					if len(value.Keys) != 1 || value.Keys[0] == nil {
						t.Error("actual Provide did not retain its one-mode payload")
					} else if value.Keys[0].Mode == protocol.ProvideMode_Network {
						oldDeliveries.Add(1)
					} else if value.Keys[0].Mode == protocol.ProvideMode_Public {
						newestDeliveries.Add(1)
					} else {
						t.Error("actual Provide has an unexpected mode")
					}
				default:
					t.Error("unexpected platform application control")
				}
			}
		})
		blocker, err := ToFrame(&protocol.SimpleMessage{MessageIndex: 7193}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		blockerResult := make(chan bool, 1)
		callers.Add(1)
		go func() {
			defer callers.Done()
			accepted, _ := client.SendWithTimeoutDetailed(blocker, ControlId, func(err error) {
				if err == nil {
					blockerAcks.Add(1)
				}
			}, -1, Ctx(blockerCtx), NoBudget())
			if !accepted {
				MessagePoolReturn(blocker.MessageBytes)
			}
			blockerResult <- accepted
		}()
		synctest.Wait()
		if runEntries.Load() != 1 || len(entered) != 1 {
			t.Fatal("actual ControlId source did not reach its existing Run barrier")
		}
		enteredId := <-entered
		client.sendBuffer.mutex.Lock()
		var source *SendSequence
		for _, candidate := range client.sendBuffer.sendSequencesBySequenceId {
			if candidate.destination == ControlId {
				if source != nil {
					t.Error("ambiguous real ControlId source")
				}
				source = candidate
			}
		}
		client.sendBuffer.mutex.Unlock()
		if source == nil || source.id() != enteredId || source.protocolAdmission == nil || source.immediateSendLimit != 1 {
			t.Fatal("actual rendezvous source has no bounded raw protocol owner")
		}
		client.ContractManager().SetProvideModesWithAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}, func(err error) {
			if err == nil {
				oldAcks.Add(1)
			}
		})
		synctest.Wait()
		admission := source.protocolAdmission
		admission.mutex.Lock()
		occupied, limit := admission.count, admission.capacity
		var oldWaiter *sendPackAdmissionWaiter
		if len(admission.waiters) == 1 {
			oldWaiter = admission.waiters[0]
		}
		admission.mutex.Unlock()
		if occupied != 1 || limit != 1 || oldWaiter == nil || runEntries.Load() != 1 || source.resendQueue.Len() != 0 || blockerDeliveries.Load() != 0 || oldDeliveries.Load() != 0 || client.IsDone() {
			t.Fatal("real raw holder and older public Provide retry did not reach the admission cut")
		}
		// The original caller cancels its own raw admission. Its actual
		// release grants the already-queued retry before supersession.
		cancelBlocker()
		synctest.Wait()
		admission.mutex.Lock()
		grantedBeforeCancel := oldWaiter.granted && admission.count == 1 && len(admission.waiters) == 0
		admission.mutex.Unlock()
		if !grantedBeforeCancel || provideStarts.Load() != 2 || oldRetryTerminal.Load() != 0 || blockerAcks.Load() != 0 || oldAcks.Load() != 0 {
			t.Fatal("actual old retry did not receive the released protocol grant before cancellation")
		}
		if !control.sendLock.TryLock() {
			t.Fatal("CONTROL_SYNC_RETRY_HELD_SUPERSESSION_LOCK: actual Provide retry waits for another raw owner while retaining the publication mutex")
		}
		generation := control.syncCount
		control.sendLock.Unlock()
		client.ContractManager().SetProvideModesWithAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true}, func(err error) {
			if err == nil && newestAcks.Add(1) == 1 {
				newestAcknowledged <- struct{}{}
			}
		})
		synctest.Wait()
		if len(oldTerminalEntered) != 1 || oldRetryTerminal.Load() == 0 || newestAttempt.Load() != 0 || provideStarts.Load() != 2 {
			t.Fatal("newest send overtook the exact older admission call still held at its terminal observer")
		}
		<-oldTerminalEntered
		control.sendLock.Lock()
		oldAttemptStillActive := control.attemptActive
		control.sendLock.Unlock()
		admission.mutex.Lock()
		returnedRawSlot := admission.count == 0 && len(admission.waiters) == 0
		admission.mutex.Unlock()
		if !oldAttemptStillActive || !returnedRawSlot {
			t.Fatal("held old call did not return its raw slot while retaining only the attempt turn")
		}
		finishOldTerminal()
		synctest.Wait()
		admission.mutex.Lock()
		oldStillQueued := false
		for _, waiter := range admission.waiters {
			oldStillQueued = oldStillQueued || waiter == oldWaiter
		}
		newestPending := admission.count == 1 && len(admission.waiters) == 0
		admission.mutex.Unlock()
		if oldStillQueued || !newestPending || control.syncCount != generation+1 || oldAcks.Load() != 0 || newestAcks.Load() != 0 || oldRetryTerminal.Load() == 0 || newestAttempt.Load() <= oldRetryTerminal.Load() {
			t.Fatal("supersession did not replace only the older waiting generation")
		}
		release()
		<-newestAcknowledged
		synctest.Wait()
		if <-blockerResult || blockerDeliveries.Load() != 0 || blockerAcks.Load() != 0 || oldDeliveries.Load() != 0 || oldAcks.Load() != 0 || newestDeliveries.Load() != 1 || newestAcks.Load() != 1 || source.resendQueue.Len() != 0 || client.IsDone() || platform.IsDone() {
			t.Fatal("actual platform delivery/ACK resurrected a canceled holder or old generation")
		}
	})
}

// An actual peer receipt may publish the next generation through the same
// ControlSync. Both originals keep their own callback and ordered delivery.
func TestControlSyncPeerAckCanPublishSuccessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), terminalControlSettings())
		platform := NewClient(ctx, ControlId, NewNoContractClientOob(), terminalControlSettings())
		control := NewControlSync(ctx, client, "ack-reentrant-successor")
		defer func() {
			cancel()
			if err := control.closeAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, owner := range []*Client{client, platform} {
				if err := owner.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
		client.ContractManager().AddNoContractPeer(ControlId)
		platform.ContractManager().AddNoContractPeer(client.ClientId())
		outgoing, returning := make(Route), make(Route)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(ControlId)), []Route{outgoing})
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returning})
		platform.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{outgoing})
		platform.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(client.ClientId())), []Route{returning})
		observed := make(chan int, 2)
		platform.AddReceiveCallback(func(path TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				message, err := FromFrame(frame)
				value, ok := message.(*protocol.SimpleMessage)
				if err != nil || !ok || path.SourceId != client.ClientId() {
					t.Error("reentrant control lost its actual peer encoding")
					continue
				}
				select {
				case observed <- int(value.MessageIndex):
				default:
					t.Error("reentrant control delivered extra originals")
				}
			}
		})
		first, err := ToFrame(&protocol.SimpleMessage{MessageIndex: 7194}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		second, err := ToFrame(&protocol.SimpleMessage{MessageIndex: 7195}, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(first.MessageBytes)
			t.Fatal(err)
		}
		var firstAcks, secondAcks atomic.Int32
		done := make(chan struct{}, 1)
		control.Send(first, nil, func(err error) {
			if err == nil && firstAcks.Add(1) == 1 {
				control.Send(second, nil, func(err error) {
					if err == nil && secondAcks.Add(1) == 1 {
						done <- struct{}{}
					}
				})
			}
		})
		<-done
		synctest.Wait()
		if firstAcks.Load() != 1 || secondAcks.Load() != 1 || len(observed) != 2 || <-observed != 7194 || <-observed != 7195 || client.IsDone() || platform.IsDone() {
			t.Fatal("reentrant publication changed actual receipt ownership or delivery order")
		}
		control.sendLock.Lock()
		active := control.attemptActive
		control.sendLock.Unlock()
		if active {
			t.Fatal("completed actual ACKs retained an admission attempt turn")
		}
	})
}
