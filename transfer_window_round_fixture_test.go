// Controlled wire rounds exercise real client workers without using host speed
// to choose a receive ordering or a sampled send-window regime.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The fixture owns captured wire shares until forward transfers them to a
// client. Decoded messages are ordinary copies and remain readable afterwards.
type windowRoundFrame struct {
	bytes []byte
	pack  *protocol.Pack
	ack   *protocol.Ack
}

// Used only inside a synctest bubble. Quiescence barriers join each transition;
// the fixed routes and captured shares are drained after both clients stop.
type windowRoundFixture struct {
	t              *testing.T
	ctx            context.Context
	sender         *Client
	receiver       *Client
	senderOut      Route
	senderIn       Route
	receiverOut    Route
	receiverIn     Route
	heldFrames     []*windowRoundFrame
	workerDones    []<-chan struct{}
	nextNumber     uint64
	deliveredCount int
	ackedCount     int
}

// Configures every policy before workers start. Background key publication is
// parked so only the test's application sequence can consume its wire rounds.
func newWindowRoundFixture(
	t *testing.T,
	configureSend func(*SendBufferSettings),
	configureReceive func(*ReceiveBufferSettings),
) *windowRoundFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	newSettings := func() *ClientSettings {
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingConstant
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.SequenceBufferSize = 0
		settings.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		settings.ReceiveBufferSettings.ApplyWindowSizing()
		settings.ReceiveBufferSettings.ReceiveQueueBudget = nil
		settings.ReceiveBufferSettings.AckCompressTimeout = 0
		return settings
	}
	senderSettings := newSettings()
	receiverSettings := newSettings()
	if configureSend != nil {
		configureSend(senderSettings.SendBufferSettings)
	}
	if configureReceive != nil {
		configureReceive(receiverSettings.ReceiveBufferSettings)
	}
	fixture := &windowRoundFixture{
		t:           t,
		ctx:         ctx,
		sender:      NewClient(ctx, NewId(), NewNoContractClientOob(), senderSettings),
		receiver:    NewClient(ctx, NewId(), NewNoContractClientOob(), receiverSettings),
		senderOut:   make(Route, 128),
		senderIn:    make(Route, 128),
		receiverOut: make(Route, 128),
		receiverIn:  make(Route, 128),
	}
	fixture.sender.ContractManager().AddNoContractPeer(fixture.receiver.ClientId())
	fixture.receiver.ContractManager().AddNoContractPeer(fixture.sender.ClientId())
	fixture.sender.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{fixture.senderOut})
	fixture.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.senderIn})
	fixture.receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{fixture.receiverOut})
	fixture.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.receiverIn})
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		fixture.deliveredCount += len(frames)
	})
	t.Cleanup(func() {
		cancel()
		for _, done := range fixture.workerDones {
			<-done
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close controlled window client: %v", err)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return fixture
}

// Captures one already-published frame after all workers reach a wait boundary.
func (self *windowRoundFixture) take(route Route) *windowRoundFrame {
	self.t.Helper()
	synctest.Wait()
	select {
	case frameBytes := <-route:
		frame := &windowRoundFrame{bytes: frameBytes}
		self.heldFrames = append(self.heldFrames, frame)
		var transferFrame protocol.TransferFrame
		if err := ProtoUnmarshal(frameBytes, &transferFrame); err != nil {
			self.t.Fatalf("decode controlled wire frame: %v", err)
		}
		frame.pack = transferFrame.Pack
		frame.ack = transferFrame.Ack
		if legacy := transferFrame.Frame; legacy != nil {
			switch legacy.MessageType {
			case protocol.MessageType_TransferPack:
				frame.pack = &protocol.Pack{}
				if err := ProtoUnmarshal(legacy.MessageBytes, frame.pack); err != nil {
					self.t.Fatalf("decode controlled legacy Pack: %v", err)
				}
			case protocol.MessageType_TransferAck:
				frame.ack = &protocol.Ack{}
				if err := ProtoUnmarshal(legacy.MessageBytes, frame.ack); err != nil {
					self.t.Fatalf("decode controlled legacy Ack: %v", err)
				}
			}
		}
		if frame.pack == nil && frame.ack == nil {
			self.t.Fatalf("controlled wire frame contains neither Pack nor Ack")
		}
		return frame
	default:
		self.t.Fatal("controlled transition published no wire frame")
		return nil
	}
}

// Transfers an owned wire share, then waits until the client has processed it.
func (self *windowRoundFixture) forward(frame *windowRoundFrame, route Route) {
	self.t.Helper()
	if frame.bytes == nil {
		self.t.Fatal("controlled wire share was already transferred")
	}
	route <- frame.bytes
	frame.bytes = nil
	synctest.Wait()
}

// Captures the requested Pack, retaining any earlier recovery writes. Holding
// every copy of a gap prevents another recovery path from choosing its release.
func (self *windowRoundFixture) takePack(number uint64) *windowRoundFrame {
	self.t.Helper()
	for range cap(self.senderOut) {
		frame := self.take(self.senderOut)
		if frame.pack == nil || number < frame.pack.SequenceNumber {
			self.t.Fatalf("wanted Pack %d, got %+v", number, frame.pack)
		}
		if frame.pack.SequenceNumber == number {
			return frame
		}
	}
	self.t.Fatalf("Pack %d was hidden behind a full route of recovery writes", number)
	return nil
}

// One completed initial write per call prevents coalescing from changing the
// chosen sequence positions. Successful Send takes the application buffer.
func (self *windowRoundFixture) write(payloadByteCount int) *windowRoundFrame {
	self.t.Helper()
	if admitted, err := self.send(payloadByteCount); !admitted {
		self.t.Fatalf("controlled Pack %d was not admitted: %v", self.nextNumber, err)
	}
	pack := self.takePack(self.nextNumber)
	self.nextNumber += 1
	return pack
}

// Sends one application message and keeps caller ownership on refusal. The
// acknowledgement callback is counted only after successful terminal delivery.
func (self *windowRoundFixture) send(payloadByteCount int) (bool, error) {
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{
		Content: string(make([]byte, payloadByteCount)),
	})
	admitted, err := self.sender.SendWithTimeoutDetailed(frame, self.receiver.ClientId(), func(err error) {
		if err == nil {
			self.ackedCount += 1
		}
	}, time.Second)
	if !admitted {
		MessagePoolReturn(frame.MessageBytes)
	}
	return admitted, err
}

// Drops exactly this captured physical write. Its decoded identity remains
// available to prove that a later actual resend repairs the intended gap.
func (self *windowRoundFixture) drop(frame *windowRoundFrame) {
	self.t.Helper()
	if frame.bytes == nil {
		self.t.Fatal("controlled drop no longer owns its frame")
	}
	MessagePoolReturn(frame.bytes)
	frame.bytes = nil
}

// Returns a recovery copy retained while later original Packs were selected.
// The original must have been dropped, so it cannot satisfy this recovery.
func (self *windowRoundFixture) recovery(frame *windowRoundFrame) *windowRoundFrame {
	self.t.Helper()
	if frame.bytes != nil {
		self.t.Fatal("the original Pack must be dropped before selecting its recovery")
	}
	var recovered *windowRoundFrame
	for _, held := range self.heldFrames {
		if held.bytes != nil && held.pack != nil && held.pack.SequenceNumber == frame.pack.SequenceNumber {
			recovered = held
			break
		}
	}
	if recovered == nil {
		recovered = self.takePack(frame.pack.SequenceNumber)
	}
	if !bytes.Equal(recovered.pack.SequenceId, frame.pack.SequenceId) ||
		!bytes.Equal(recovered.pack.MessageId, frame.pack.MessageId) {
		self.t.Fatal("recovery changed the dropped Pack's sequence or message identity")
	}
	return recovered
}

// Applies all acknowledgements from a completed receive transition. Tentative
// admissions and refusals are allowed to publish none.
func (self *windowRoundFixture) acknowledge() {
	self.t.Helper()
	synctest.Wait()
	for len(self.receiverOut) != 0 {
		self.forward(self.take(self.receiverOut), self.senderIn)
	}
}

// Releases subsequent traffic onto FIFO lanes with concurrent propagation and
// the caller's normal Ack compression. Quiescence orders the setting change
// before any new receive work; cleanup joins both bounded forwarding workers.
func (self *windowRoundFixture) startWire(ackDelay time.Duration, ackCompressTimeout time.Duration) {
	synctest.Wait()
	self.receiver.settings.ReceiveBufferSettings.AckCompressTimeout = ackCompressTimeout
	self.workerDones = append(self.workerDones,
		startLaneAckTestPump(self.ctx, self.senderOut, self.receiverIn, 0),
		startLaneAckTestPump(self.ctx, self.receiverOut, self.senderIn, ackDelay),
	)
}

// Offers a bounded remaining workload. It retains the original one-second
// admission deadline; shutdown joins the producer before closing the clients.
func (self *windowRoundFixture) offer(messageCount int, payloadByteCount int) {
	done := make(chan struct{})
	self.workerDones = append(self.workerDones, done)
	go func() {
		defer close(done)
		for index := range messageCount {
			if admitted, err := self.send(payloadByteCount); !admitted {
				if self.ctx.Err() == nil {
					self.t.Errorf("remaining message %d of %d was not admitted: %v", index, messageCount, err)
				}
				return
			}
		}
	}()
}

// Delivers one Pack and captures the latest cumulative or selective Ack. A
// head release can drain several held items and coalesce their acknowledgements.
func (self *windowRoundFixture) receive(frame *windowRoundFrame) *windowRoundFrame {
	self.t.Helper()
	self.forward(frame, self.receiverIn)
	ack := self.take(self.receiverOut)
	for len(self.receiverOut) != 0 {
		ack = self.take(self.receiverOut)
	}
	if ack.ack == nil {
		self.t.Fatal("receiver published a Pack in place of its acknowledgement")
	}
	return ack
}

// Looks up the only application sender after its initial wire write. Mutable
// sequence fields may be read only after a synctest quiescence barrier.
func (self *windowRoundFixture) sequence() *SendSequence {
	self.t.Helper()
	synctest.Wait()
	self.sender.sendBuffer.mutex.Lock()
	defer self.sender.sendBuffer.mutex.Unlock()
	for _, sequence := range self.sender.sendBuffer.sendSequencesBySequenceId {
		if sequence.destination == self.receiver.ClientId() {
			return sequence
		}
	}
	self.t.Fatal("controlled sender has no application sequence")
	return nil
}

// Reads the receiver corresponding to the controlled sender after its workers
// quiesce. Callers may then inspect the actual held set without polling it.
func (self *windowRoundFixture) receiveSequence() *ReceiveSequence {
	self.t.Helper()
	synctest.Wait()
	self.receiver.receiveBuffer.mutex.Lock()
	defer self.receiver.receiveBuffer.mutex.Unlock()
	for _, sequence := range self.receiver.receiveBuffer.receiveSequences {
		if sequence.source.SourceId == self.sender.ClientId() {
			return sequence
		}
	}
	self.t.Fatal("controlled receiver has no application sequence")
	return nil
}
