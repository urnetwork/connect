// Wire timing tests force the receiver's ingress, queue and delivery order.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The real Client ingress must precede both the sequence handoff and the
// application callback. Virtual time advances only at the two barriers.
func TestReceiverAckTimingIncludesQueueAndDeliveryWait(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		synctest.Test(t, func(t *testing.T) {
			ack := receiveTimedPackTest(t, version, 20*time.Millisecond, 5*time.Millisecond)
			if ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != 25000 {
				t.Fatalf("version %d: receiver delay = %v, want exact ingress-to-encoding 25000 microseconds", version, ack.ReceiverAckDelayMicros)
			}
		})
	}
}

// Ingress at the exact client clock origin is available timing; zero delay
// must have wire presence, rather than looking like a legacy acknowledgement.
func TestReceiverAckTimingPreservesExactZeroDelay(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		synctest.Test(t, func(t *testing.T) {
			ack := receiveTimedPackTest(t, version, 0, 0)
			if ack.ReceiverAckDelayMicros == nil || *ack.ReceiverAckDelayMicros != 0 {
				t.Fatalf("version %d: zero-delay ACK did not retain explicit timing presence", version)
			}
		})
	}
}

// A synthetic peer offers one ordinary tagged Pack to the real Client route.
// No timestamp helper or added state is referenced, so the same proof runs
// against the codec-only implementation before receiver timing is integrated.
func receiveTimedPackTest(t *testing.T, version int, queueWait, deliveryWait time.Duration) *protocol.Ack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.ReceiveBufferSettings.ProtocolVersion = version
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	settings.ReceiveBufferSettings.IdleTimeout = time.Hour
	queued := make(chan struct{})
	releaseQueue := make(chan struct{})
	delivering := make(chan struct{})
	releaseDelivery := make(chan struct{})
	var queueOnce, deliveryOnce sync.Once
	settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
		close(queued)
		<-releaseQueue
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	peerId := NewId()
	client.ContractManager().AddNoContractPeer(peerId)
	client.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) {
		close(delivering)
		<-releaseDelivery
	})
	input, output := make(Route, 4), make(Route, 4)
	inTransport, outTransport := NewReceiveGatewayTransport(), NewSendGatewayTransport()
	client.RouteManager().UpdateTransport(inTransport, []Route{input})
	client.RouteManager().UpdateTransport(outTransport, []Route{output})
	t.Cleanup(func() {
		queueOnce.Do(func() { close(releaseQueue) })
		deliveryOnce.Do(func() { close(releaseDelivery) })
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join receiver timing client: %v", err)
		}
		for _, route := range []Route{input, output} {
			for {
				select {
				case wire := <-route:
					MessagePoolReturn(wire)
				default:
					goto drained
				}
			}
		drained:
		}
	})
	synctest.Wait()
	messageId := NewId()
	pack := &protocol.Pack{
		MessageId: messageId.Bytes(), SequenceId: NewId().Bytes(), SequenceNumber: 0, Head: true,
		Tag:    &protocol.Tag{SendTime: uint64(time.Now().UnixMilli())},
		Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: []byte{1}}},
	}
	wire, err := ProtoMarshal(&protocol.TransferFrame{TransferPath: sendTransferPath(peerId, DestinationId(client.ClientId())).ToProtobuf(), Pack: pack})
	if err != nil {
		t.Fatal(err)
	}
	input <- wire
	<-queued
	time.Sleep(queueWait)
	queueOnce.Do(func() { close(releaseQueue) })
	<-delivering
	time.Sleep(deliveryWait)
	deliveryOnce.Do(func() { close(releaseDelivery) })
	synctest.Wait()
	select {
	case wire := <-output:
		var frame protocol.TransferFrame
		err := ProtoUnmarshal(wire, &frame)
		MessagePoolReturn(wire)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Ack == nil && frame.Frame != nil {
			frame.Ack = &protocol.Ack{}
			if err := ProtoUnmarshal(frame.Frame.MessageBytes, frame.Ack); err != nil {
				t.Fatal(err)
			}
		}
		if frame.Ack == nil || frame.Ack.Tag == nil || frame.Ack.Tag.SendTime != pack.Tag.SendTime {
			t.Fatal("receiver lost the exact Pack's timing identity")
		}
		got, err := IdFromBytes(frame.Ack.MessageId)
		if err != nil || got != messageId {
			t.Fatal("receiver timed a different cumulative head")
		}
		return frame.Ack
	default:
		t.Fatal("immediate receiver ACK did not complete at the released barriers")
		return nil
	}
}
