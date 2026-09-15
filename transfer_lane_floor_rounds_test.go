// Controlled acknowledgement rounds pin reusable lane capacity independently
// of host throughput or the wall-clock duration of a propagation fixture.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Four separate light Packs must reach the receiver before any light Ack is
// released, on every round, while the heavy lane retains the entire pool.
// The write and capacity hooks establish each boundary before it is inspected.
func TestLightLaneReusesItsFloorAcrossAcknowledgements(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const floor = ByteCount(8 * 1024)
	const payloadByteCount = 1024
	const packsPerRound = 4
	const rounds = 4
	budget := NewTransferMemoryBudget(64 * 1024)
	senderId := NewId()
	receiverId := NewId()
	heavySaturated := make(chan struct{})
	var heavySaturatedOnce sync.Once
	lightQueued := make(chan uint64, packsPerRound)
	settings := func() *ClientSettings {
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.LogicalDataLaneCount = 2
		settings.SendBufferSettings.LaneFloorByteCount = floor
		settings.SendBufferSettings.ResendQueueBudget = budget
		settings.SendBufferSettings.ResendQueueMinByteCount = 0
		settings.SendBufferSettings.ResendQueueMaxByteCount = 128 * 1024
		settings.SendBufferSettings.DeliverySizedWindowScale = 0
		settings.SendBufferSettings.AckTimeout = time.Minute
		settings.SendBufferSettings.MinResendInterval = time.Minute
		settings.SendBufferSettings.RttMinResendInterval = time.Minute
		settings.SendBufferSettings.MaxResendInterval = time.Minute
		settings.SendBufferSettings.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
			if id.Destination == receiverId && id.LogicalLane == 1 {
				heavySaturatedOnce.Do(func() { close(heavySaturated) })
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == receiverId && id.LogicalLane == 2 {
				select {
				case lightQueued <- number:
				case <-ctx.Done():
				}
			}
		}
		return settings
	}
	sender := NewClient(ctx, senderId, NewNoContractClientOob(), settings())
	receiver := NewClient(ctx, receiverId, NewNoContractClientOob(), settings())
	sender.ContractManager().AddNoContractPeer(receiverId)
	receiver.ContractManager().AddNoContractPeer(senderId)
	senderOut := make(Route, 128)
	senderIn := make(Route, 128)
	receiverIn := make(Route, 128)
	receiverOut := make(Route, 128)
	lightAcks := make(Route, 128)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{senderOut})
	sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{senderIn})
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{receiverIn})
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{receiverOut})
	lightDelivered := make(chan int, packsPerRound)
	var heavyDelivered atomic.Int64
	receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if peer.TransferKey.LogicalLane == 2 {
				select {
				case lightDelivered <- len(frame.MessageBytes):
				default:
					t.Error("light delivery collector overflowed its explicit round")
				}
			} else if peer.TransferKey.LogicalLane == 1 {
				heavyDelivered.Add(int64(len(frame.MessageBytes)))
			}
		}
	})
	dataDone := startLaneAckTestPump(ctx, senderOut, receiverIn, 0)
	ackDone := make(chan struct{})
	go func() {
		defer close(ackDone)
		for {
			select {
			case frameBytes := <-receiverOut:
				var frame protocol.TransferFrame
				if err := proto.Unmarshal(frameBytes, &frame); err != nil {
					MessagePoolReturn(frameBytes)
					t.Errorf("decode acknowledgement: %v", err)
					return
				}
				if frame.Ack == nil {
					MessagePoolReturn(frameBytes)
					continue
				}
				sequenceId, err := IdFromBytes(frame.Ack.SequenceId)
				if err != nil {
					MessagePoolReturn(frameBytes)
					t.Errorf("decode acknowledgement sequence: %v", err)
					return
				}
				light := func() bool {
					sender.sendBuffer.mutex.Lock()
					defer sender.sendBuffer.mutex.Unlock()
					sequence := sender.sendBuffer.sendSequencesBySequenceId[sequenceId]
					return sequence != nil && sequence.logicalLane == 2
				}()
				if !light {
					MessagePoolReturn(frameBytes)
					continue
				}
				select {
				case lightAcks <- frameBytes:
				case <-ctx.Done():
					MessagePoolReturn(frameBytes)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	payload := string(make([]byte, payloadByteCount))
	send := func(lane uint32, ack func(error)) bool {
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: payload})
		admitted, _ := sender.SendWithTimeoutDetailed(frame, receiverId, ack, -1, TransferKey{LogicalLane: lane})
		if !admitted {
			MessagePoolReturn(frame.MessageBytes)
		}
		return admitted
	}
	heavyDone := make(chan struct{})
	go func() {
		defer close(heavyDone)
		for ctx.Err() == nil && send(1, nil) {
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-heavyDone
		<-dataDone
		<-ackDone
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := sender.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close sender: %v", err)
		}
		if err := receiver.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close receiver: %v", err)
		}
		for _, route := range []Route{senderOut, senderIn, receiverIn, receiverOut, lightAcks} {
			func() {
				for {
					select {
					case frame := <-route:
						MessagePoolReturn(frame)
					default:
						return
					}
				}
			}()
		}
	})
	select {
	case <-heavySaturated:
	case <-heavyDone:
		t.Fatal("heavy offer ended before reaching capacity")
	case <-ctx.Done():
		t.Fatal("heavy lane did not reach its capacity boundary")
	}
	if budget.Available() != 0 {
		t.Fatalf("heavy lane stopped with %d bytes still available in the shared pool", budget.Available())
	}
	for round := range rounds {
		acks := make(chan error, packsPerRound)
		delivered := 0
		for index := range packsPerRound {
			if !send(2, func(err error) {
				select {
				case acks <- err:
				default:
					t.Error("light acknowledgement collector overflowed its explicit round")
				}
			}) {
				t.Fatalf("round %d light Pack %d was not admitted", round, index)
			}
			select {
			case <-lightQueued:
			case <-ctx.Done():
				t.Fatalf("round %d light Pack %d did not reach the resend queue", round, index)
			}
			select {
			case byteCount := <-lightDelivered:
				delivered += byteCount
			case <-ctx.Done():
				t.Fatalf("round %d light Pack %d did not reach the receiver", round, index)
			}
		}
		if delivered < packsPerRound*payloadByteCount || budget.Available() != 0 {
			t.Fatalf("round %d delivered %d bytes with pool headroom %d", round, delivered, budget.Available())
		}
		// Only the completed round can release its acknowledgements. Each
		// application callback proves its resend reservation was removed.
		for acknowledged := 0; acknowledged < packsPerRound; {
			select {
			case frame := <-lightAcks:
				select {
				case senderIn <- frame:
				case <-ctx.Done():
					MessagePoolReturn(frame)
					t.Fatalf("round %d could not release an acknowledgement", round)
				}
			case err := <-acks:
				if err != nil {
					t.Fatalf("round %d acknowledgement: %v", round, err)
				}
				acknowledged += 1
			case <-ctx.Done():
				t.Fatalf("round %d did not reclaim its lane floor", round)
			}
		}
		t.Logf("round %d delivered %d light bytes before acknowledgement while the shared pool stayed full", round, delivered)
	}
	if heavyDelivered.Load() == 0 {
		t.Fatal("the heavy lane reached no receiver")
	}
	if stats := receiver.ReceiveStats(); stats.ReceiveQueueEvictionCount != 0 || stats.ReceiveQueueDropCount != 0 {
		t.Fatalf("receiver lost ordered work: evictions=%d drops=%d", stats.ReceiveQueueEvictionCount, stats.ReceiveQueueDropCount)
	}
}
