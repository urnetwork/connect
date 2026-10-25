// Terminal Transfer admission retires one control generation without changing
// transient retry policy or consuming another owner's frame witness.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

func terminalControlSettings() *ClientSettings {
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.SkipClientKeyPublication = true
	settings.EncryptionSettings.Mode = EncryptionModeOff
	return settings
}

// Counts only this control's actual public enqueue attempts. The third event
// cancels the old implementation so its causal failure is finite, not a hang.
func terminalControlAttempts(settings *ClientSettings, control **ControlSync, count *atomic.Int32, messageType protocol.MessageType) {
	settings.SendBufferSettings.SendPackLifecycleObserver = func(observation SendPackLifecycleObservation) {
		if observation.Phase == SendPackLifecyclePhaseStarted &&
			observation.DestinationId == ControlId && observation.MessageType == messageType {
			if count.Add(1) == 3 {
				(*control).Close()
			}
		}
	}
}

func TestControlSyncClosedBufferStopsOwnedRetry(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		settings := terminalControlSettings()
		var attempts atomic.Int32
		var control *ControlSync
		terminalControlAttempts(settings, &control, &attempts, protocol.MessageType_TestSimpleMessage)
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		control = NewControlSync(ctx, client, "terminal-admission")
		defer func() {
			if err := control.closeAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		client.sendBuffer.Close()
		frame := &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: MessagePoolCopy([]byte("closed-admission-control")),
		}
		witness := MessagePoolShareReadOnly(frame.MessageBytes)
		control.Send(frame, nil, nil)
		synctest.Wait()
		if err := control.closeAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		released := MessagePoolReturn(witness)
		if client.IsDone() || !released {
			t.Fatal("terminal control changed Client lifetime or retained its frame")
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("CONTROL_RETRY_IGNORED_TERMINAL_ADMISSION: actual_attempts=%d want=2", got)
		}
	})
}

// The existing creation barrier publishes a real canceled source. Holding its
// worker before cleanup leaves that exact source indexed for the outer create
// and Pack. The real three-attempt Pack loop then reports a nonnil error while
// admission remains open. No admission result or timer is replaced.
func replacementControlCase(t *testing.T, cancelClient bool, witness string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		settings := terminalControlSettings()
		var client *Client
		var control *ControlSync
		var attempts atomic.Int32
		var retirements atomic.Int32
		settings.SendBufferSettings.SendPackLifecycleObserver = func(observation SendPackLifecycleObservation) {
			if observation.Phase != SendPackLifecyclePhaseStarted ||
				observation.DestinationId != ControlId || observation.MessageType != protocol.MessageType_TestSimpleMessage {
				return
			}
			if attempts.Add(1) == 3 {
				if cancelClient {
					client.Cancel()
				} else {
					control.Close()
				}
			}
		}
		releaseWorkers := make(chan struct{})
		var creating atomic.Bool
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == ControlId {
				<-releaseWorkers
			}
		}
		settings.SendBufferSettings.beforeCreateSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination != ControlId || !creating.CompareAndSwap(false, true) {
				return
			}
			defer creating.Store(false)
			sequence := client.sendBuffer.createSendSequence(id, &SendPack{
				TransferOptions: TransferOptions{
					CompanionContract: id.CompanionContract,
					ForceStream:       id.ForceStream,
				},
				Destination:         id.Destination,
				Ctx:                 client.ctx,
				EncryptionRole:      id.EncryptionRole,
				EncryptionCompanion: id.EncryptionCompanion,
				logicalLane:         id.LogicalLane,
				logicalLaneExplicit: true,
			})
			if sequence != nil {
				retirements.Add(1)
				sequence.Cancel()
			}
		}
		client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		control = NewControlSync(ctx, client, "replacement-error-owner")
		defer func() {
			select {
			case <-releaseWorkers:
			default:
				close(releaseWorkers)
			}
			if err := control.closeAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		base := &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: MessagePoolGet(3000),
		}
		current := &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: MessagePoolGet(6000),
		}
		baseWitness := MessagePoolShareReadOnly(base.MessageBytes)
		baseSecondWitness := MessagePoolShareReadOnly(base.MessageBytes)
		currentWitness := MessagePoolShareReadOnly(current.MessageBytes)
		var updates atomic.Int32
		control.Send(base, func() *protocol.Frame {
			updates.Add(1)
			return current
		}, nil)
		synctest.Wait()
		if err := control.closeAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		clientWasLive := !client.IsDone()
		client.sendBuffer.mutex.Lock()
		bufferWasOpen := !client.sendBuffer.closed
		client.sendBuffer.mutex.Unlock()
		close(releaseWorkers)
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Two base witnesses prevent the old wrong return from releasing a root
		// before this exact final-return observation. No invalid return is made.
		baseReleasedEarly := MessagePoolReturn(baseWitness)
		if !baseReleasedEarly {
			if !MessagePoolReturn(baseSecondWitness) {
				t.Error("base frame retained more than the two witness owners")
			}
		}
		currentReleased := MessagePoolReturn(currentWitness)
		if !currentReleased {
			// Retire the one positively witnessed leaked control owner on OLD,
			// so a causal assertion cannot contaminate another root's pool.
			if !MessagePoolReturn(current.MessageBytes) {
				t.Error("replacement frame has an unexpected extra owner")
			}
		}
		if attempts.Load() != 3 || updates.Load() != 1 || retirements.Load() < 2 || (!cancelClient && !bufferWasOpen) || clientWasLive == cancelClient {
			t.Fatalf("replacement-error ordering was not reached: attempts=%d updates=%d retirements=%d buffer_open=%t client_live=%t", attempts.Load(), updates.Load(), retirements.Load(), bufferWasOpen, clientWasLive)
		}
		if witness == "base" && baseReleasedEarly {
			t.Fatalf("CONTROL_REPLACEMENT_CONSUMED_BASE_WITNESS: cancel_client=%t", cancelClient)
		}
		if witness == "current" && !currentReleased {
			t.Fatalf("CONTROL_REPLACEMENT_RETAINED_CURRENT_OWNER: cancel_client=%t", cancelClient)
		}
	})
}

func TestControlSyncHandleCancellationPreservesReplacedBase(t *testing.T) {
	replacementControlCase(t, false, "base")
}

func TestControlSyncHandleCancellationReleasesReplacement(t *testing.T) {
	replacementControlCase(t, false, "current")
}

func TestControlSyncClientCancellationPreservesReplacedBase(t *testing.T) {
	replacementControlCase(t, true, "base")
}

func TestControlSyncClientCancellationReleasesReplacement(t *testing.T) {
	replacementControlCase(t, true, "current")
}

func TestControlSyncLiveBufferErrorStillRetries(t *testing.T) {
	replacementControlCase(t, false, "retry")
}

func TestControlSyncClosedBufferKeepsOtherClientControlHealthy(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		settings := terminalControlSettings()
		var attempts atomic.Int32
		var closedControl *ControlSync
		terminalControlAttempts(settings, &closedControl, &attempts, protocol.MessageType_TestSimpleMessage)
		closedClient := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		closedControl = NewControlSync(ctx, closedClient, "closed-scope")
		healthy := NewClient(ctx, NewId(), NewNoContractClientOob(), terminalControlSettings())
		receiver := NewClient(ctx, ControlId, NewNoContractClientOob(), terminalControlSettings())
		healthyControl := NewControlSync(ctx, healthy, "healthy-scope")
		defer func() {
			for _, control := range []*ControlSync{closedControl, healthyControl} {
				if err := control.closeAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			for _, client := range []*Client{closedClient, healthy, receiver} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
		healthy.ContractManager().AddNoContractPeer(ControlId)
		receiver.ContractManager().AddNoContractPeer(healthy.ClientId())
		outgoing, returning := make(Route), make(Route)
		healthy.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(ControlId)), []Route{outgoing})
		healthy.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{returning})
		receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{outgoing})
		receiver.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(healthy.ClientId())), []Route{returning})
		var delivered, acked atomic.Int32
		acknowledged := make(chan struct{})
		receiver.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TestSimpleMessage {
					continue
				}
				message, err := FromFrame(frame)
				if index, ok := message.(*protocol.SimpleMessage); err == nil && ok && index.MessageIndex == 1979 {
					delivered.Add(1)
				}
			}
		})
		closedClient.sendBuffer.Close()
		closedControl.Send(&protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: MessagePoolCopy([]byte("closed-control")),
		}, nil, nil)
		healthyFrame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: 1979}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		healthyControl.Send(healthyFrame, nil, func(err error) {
			if err == nil && acked.Add(1) == 1 {
				close(acknowledged)
			}
		})
		// Wait for positive peer ACK evidence, including the unchanged normal
		// ACK compression timer; Wait alone does not advance the bubble clock.
		<-acknowledged
		synctest.Wait()
		if closedClient.IsDone() || healthy.IsDone() || receiver.IsDone() || delivered.Load() != 1 || acked.Load() != 1 {
			t.Fatalf("terminal scope changed live controls: delivered=%d acked=%d", delivered.Load(), acked.Load())
		}
	})
}
