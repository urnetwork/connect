package connect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Block the existing head-rewrite logging boundary, after recovery has selected
// its item but before it rewrites the envelope. This makes an ordinary worker
// interleaving deterministic without changing the product or its ACK lifetime.
type ackRewriteBarrierLogger struct {
	Logger
	once    sync.Once
	reached chan struct{}
	release <-chan struct{}
}

func (self *ackRewriteBarrierLogger) Infof(format string, args ...any) {
	if strings.HasPrefix(format, "[s]set head ") {
		self.once.Do(func() {
			close(self.reached)
			<-self.release
		})
	}
}

func (self *ackRewriteBarrierLogger) V(level int32) Verbose {
	return ackRewriteBarrierVerbose{logger: self, enabled: level == 1}
}

type ackRewriteBarrierVerbose struct {
	logger  *ackRewriteBarrierLogger
	enabled bool
}

func (self ackRewriteBarrierVerbose) Enabled() bool { return self.enabled }
func (self ackRewriteBarrierVerbose) Info(...any)   {}
func (self ackRewriteBarrierVerbose) Infof(format string, args ...any) {
	if self.enabled {
		self.logger.Infof(format, args...)
	}
}

// Both ends run the real wire decoder, Transfer workers and cumulative ACK
// path. Only the physical H1 legs are replaced with bounded reliable routes.
func newAckRetirementFixture(t *testing.T, carrier TransportType, version int, logger Logger, configure ...func(*ClientSettings)) (*windowRoundFixture, *sendGatewayTransport, *sendGatewayTransport) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	newSettings := func() *ClientSettings {
		settings := DefaultClientSettings()
		settings.Log = logger
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingConstant
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.AckTimeout = DefaultMultiClientSettings().AckTimeout
		settings.SendBufferSettings.ProtocolVersion = version
		settings.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		settings.ReceiveBufferSettings.ApplyWindowSizing()
		settings.ReceiveBufferSettings.AckCompressTimeout = 0
		settings.ReceiveBufferSettings.ProtocolVersion = version
		for _, apply := range configure {
			apply(settings)
		}
		return settings
	}
	fixture := &windowRoundFixture{
		t: t, ctx: ctx,
		sender:    NewClient(ctx, NewId(), NewNoContractClientOob(), newSettings()),
		receiver:  NewClient(ctx, NewId(), NewNoContractClientOob(), newSettings()),
		senderOut: make(Route, 32), senderIn: make(Route, 32),
		receiverOut: make(Route, 32), receiverIn: make(Route, 32),
	}
	dataTransport := NewSendGatewayTransportWithType(carrier)
	replyTransport := NewSendGatewayTransportWithType(carrier)
	fixture.sender.ContractManager().AddNoContractPeer(fixture.receiver.ClientId())
	fixture.receiver.ContractManager().AddNoContractPeer(fixture.sender.ClientId())
	fixture.sender.RouteManager().UpdateTransport(dataTransport, []Route{fixture.senderOut})
	fixture.receiver.RouteManager().UpdateTransport(replyTransport, []Route{fixture.receiverOut})
	properties := TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable}
	fixture.sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(carrier), []Route{fixture.senderIn}, properties)
	fixture.receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(carrier), []Route{fixture.receiverIn}, properties)
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		fixture.deliveredCount += len(frames)
	})
	t.Cleanup(func() {
		cancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("join ACK retirement client: %v", err)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			drainFlightGateRoute(route)
		}
	})
	return fixture, dataTransport, replyTransport
}

// The receiver really delivered both messages. Its second ACK reaches the
// sender before the original 30-second deadline while the sender is promoting
// that message to a retry head. Temporary recovery scheduling must not make a
// retained message look unknown to ACK validation or fail its terminal owner.
func TestTransferAckDuringHeadRewriteKeepsLifetime(t *testing.T) {
	for _, carrier := range []TransportType{TransportTypeH1, TransportTypeP2p} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/v%d", carrier, version), func(t *testing.T) {
				runTransferAckDuringHeadRewrite(t, carrier, version, "exact")
			})
		}
	}
}

func TestTransferAckDuringHeadRewriteRejectsOtherIdentity(t *testing.T) {
	for _, kind := range []string{"retired_message", "unknown_message", "wrong_sequence"} {
		t.Run(kind, func(t *testing.T) {
			runTransferAckDuringHeadRewrite(t, TransportTypeH1, 2, kind)
		})
	}
}

func runTransferAckDuringHeadRewrite(t *testing.T, carrier TransportType, version int, ackKind string) *SendSequence {
	t.Helper()
	assertMessagePoolOwnership(t)
	var sequence *SendSequence
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		logger := &ackRewriteBarrierLogger{Logger: NewNoopLogger(), reached: make(chan struct{}), release: release}
		fixture, _, _ := newAckRetirementFixture(t, carrier, version, logger)
		first := fixture.write(64)
		type terminalResult struct {
			err error
			at  time.Time
		}
		terminal := make(chan terminalResult, 4)
		frame := budgetTestFrame(64)
		admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
			terminal <- terminalResult{err: err, at: time.Now()}
		}, time.Second)
		if !admitted || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("second Pack admission=%t err=%v", admitted, err)
		}
		second := fixture.takePack(1)
		if second.pack.Head {
			t.Fatal("second initial Pack must need promotion after cumulative progress")
		}
		sequence = fixture.sequence()
		messageId := RequireIdFromBytes(second.pack.MessageId)
		item := sequence.resendQueue.GetByMessageId(messageId)
		started := item.sendTime
		if item.ackTimeout != 30*time.Second {
			t.Fatalf("fixture changed production ACK lifetime: %s", item.ackTimeout)
		}
		fixture.forward(fixture.receive(first), fixture.senderIn)
		secondAck := fixture.receive(second)
		if fixture.deliveredCount != 2 || fixture.ackedCount != 1 || len(terminal) != 0 {
			t.Fatal("fixture did not deliver both originals while retaining only the second ACK")
		}
		<-logger.reached
		lookupDuringRewrite := sequence.resendQueue.GetByMessageId(messageId) != nil
		time.Sleep(time.Until(started.Add(29 * time.Second)))
		if ackKind == "cancel" {
			sequence.cancel()
		} else if ackKind == "rewrite_error" {
			MessagePoolReturn(item.transferFrameBytes)
			item.transferFrameBytes = MessagePoolCopy([]byte{0xff})
		} else if ackKind == "exact" {
			fixture.forward(secondAck, fixture.senderIn)
		} else {
			ack := secondAck.ack
			switch ackKind {
			case "retired_message":
				ack.MessageId = first.pack.MessageId
			case "unknown_message":
				ack.MessageId = NewId().Bytes()
			case "wrong_sequence":
				ack.SequenceId = NewId().Bytes()
			}
			wire, marshalErr := ProtoMarshal(&protocol.TransferFrame{
				TransferPath: sendTransferPath(fixture.receiver.ClientId(), DestinationId(fixture.sender.ClientId())).ToProtobuf(),
				Ack:          ack,
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			fixture.senderIn <- wire
			synctest.Wait()
		}
		pending := sequence.ackWindow.pendingDeliveryFor(1, messageId)
		close(release)
		released = true
		synctest.Wait()
		retryWrites := len(fixture.senderOut)
		time.Sleep(time.Until(started.Add(31 * time.Second)))
		synctest.Wait()
		if len(terminal) != 1 {
			t.Fatalf("terminal callback count=%d, want one", len(terminal))
		}
		result := <-terminal
		t.Logf("lookup_during_rewrite=%t pending_ack=%t retry_writes=%d terminal_at=%s err=%v delivered=%d",
			lookupDuringRewrite, pending, retryWrites, result.at.Sub(started), result.err, fixture.deliveredCount)
		if ackKind == "exact" {
			if !pending || result.err != nil || result.at != started.Add(29*time.Second) {
				t.Error("timely ACK for a retained message was discarded during head rewrite")
			}
			if retryWrites != 0 {
				t.Error("acknowledged head rewrite emitted a redundant retry")
			}
		} else if ackKind == "cancel" || ackKind == "rewrite_error" {
			if pending || result.err == nil || result.at != started.Add(29*time.Second) || retryWrites != 0 {
				t.Error("rewrite exit retained or dispatched its canceled owner")
			}
		} else if pending || result.err == nil || result.at != started.Add(30*time.Second) {
			t.Error("another ACK identity borrowed the detached message's retained lifetime")
		}
		if fixture.deliveredCount != 2 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Error("head rewrite duplicated delivery or retained recovery ownership")
		}
	})
	return sequence
}
