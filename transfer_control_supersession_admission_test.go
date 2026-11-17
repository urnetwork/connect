// An in-band latest-generation control cannot retain its publication mutex
// while one actual bounded protocol admission slot belongs to another sender.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// A real source is parked at the existing Run barrier, with one raw protocol
// owner and an older public Provide retry queued for its same bounded slot.
// TryLock makes the pre-fix failure finite without blocking another goroutine
// on sync.Mutex or advancing a deadline to stand in for supersession.
func TestControlSyncPendingAdmissionKeepsSupersessionAvailable(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		releaseRun := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseRun) }) }
		entered := make(chan sendSequenceId, 1)
		var runEntries atomic.Int32
		settings := terminalControlSettings()
		settings.SendBufferSettings.SequenceBufferSize = 0
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
			}, -1, NoBudget())
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
		admission.mutex.Lock()
		oldStillQueued := false
		for _, waiter := range admission.waiters {
			oldStillQueued = oldStillQueued || waiter == oldWaiter
		}
		newestPending := admission.count == 1 && len(admission.waiters) == 1
		admission.mutex.Unlock()
		if oldStillQueued || !newestPending || control.syncCount != generation+1 || oldAcks.Load() != 0 || newestAcks.Load() != 0 {
			t.Fatal("supersession did not replace only the older waiting generation")
		}
		release()
		<-newestAcknowledged
		synctest.Wait()
		if !<-blockerResult || blockerDeliveries.Load() != 1 || blockerAcks.Load() != 1 || oldDeliveries.Load() != 0 || oldAcks.Load() != 0 || newestDeliveries.Load() != 1 || newestAcks.Load() != 1 || source.resendQueue.Len() != 0 || client.IsDone() || platform.IsDone() {
			t.Fatal("actual platform delivery/ACK lost the healthy owner or newest generation")
		}
	})
}
