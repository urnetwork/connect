// Physical receive cancellation may refuse admission, but must neither replace
// a healthy logical sequence nor revoke a Pack whose ownership already moved.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// A real receive buffer owns the queued Packs. Its worker-start barrier keeps
// admission separate from delivery; the output route carries actual wire ACKs.
type receiveCarrierLifecycleFixture struct {
	t             *testing.T
	ctx           context.Context
	client        *Client
	source        TransferPath
	output        Route
	delivered     chan string
	releaseRun    chan struct{}
	releaseOnce   sync.Once
	rootBudget    *TransferMemoryBudget
	packBudget    *TransferMemoryBudget
	receiveBudget *TransferMemoryBudget
}

// Installs instance-owned budgets after constant-window calibration and parks
// unrelated publishers. Cleanup unblocks workers before joining and reconciling.
func newReceiveCarrierLifecycleFixture(t *testing.T) *receiveCarrierLifecycleFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &receiveCarrierLifecycleFixture{
		t: t, ctx: ctx, source: SourceId(NewId()), output: make(Route, 8),
		delivered: make(chan string, 8), releaseRun: make(chan struct{}),
		rootBudget: NewTransferMemoryBudget(32 * 1024),
	}
	f.packBudget = NewTransferMemoryBudgetWithParent(16*1024, f.rootBudget)
	f.receiveBudget = NewTransferMemoryBudgetWithParent(16*1024, f.rootBudget)
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	settings.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
	settings.ReceiveBufferSettings.ApplyWindowSizing()
	settings.ReceiveBufferSettings.SequenceBufferSize = 2
	settings.ReceiveBufferSettings.H1SequenceBufferSize = 2
	settings.ReceiveBufferSettings.PackQueueBudget = f.packBudget
	settings.ReceiveBufferSettings.PackQueueRetainedByteAccounting = true
	settings.ReceiveBufferSettings.ReceiveQueueBudget = f.receiveBudget
	settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting = true
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	settings.ReceiveBufferSettings.ProtocolVersion = 2
	settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
		<-f.releaseRun
	}
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
	f.client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{f.output})
	f.client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		if source != f.source {
			t.Errorf("delivered source = %v, want the admitted synthetic peer", source)
		}
		for _, frame := range frames {
			select {
			case f.delivered <- string(frame.MessageBytes):
			default:
				t.Error("receive lifecycle callback exceeded the finite fixture capacity")
			}
		}
	})
	t.Cleanup(func() {
		f.release()
		cancel()
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join receive lifecycle client: %v", err)
		}
		for len(f.output) != 0 {
			MessagePoolReturn(<-f.output)
		}
		for _, budget := range []*TransferMemoryBudget{f.packBudget, f.receiveBudget, f.rootBudget} {
			reserved, released := budget.Counts()
			if budget.UsedByteCount() != 0 || reserved != released {
				t.Errorf("receive lifecycle budget after join: used=%d reserved=%d released=%d",
					budget.UsedByteCount(), reserved, released)
			}
		}
	})
	return f
}

// Opens the worker barrier exactly once, including assertion-failure cleanup.
func (self *receiveCarrierLifecycleFixture) release() {
	self.releaseOnce.Do(func() { close(self.releaseRun) })
}

// Takes the two independent pooled roots on success; refusal returns both to
// the caller. A consumed/dropped true result is not a delivery assertion.
func (self *receiveCarrierLifecycleFixture) offer(
	sequenceId Id, number uint64, carrierDone <-chan struct{}, payload string,
) (Id, bool, error) {
	self.t.Helper()
	messageId := NewId()
	pack := &protocol.Pack{
		MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(),
		SequenceNumber: number, Head: number == 0,
		Frames: []*protocol.Frame{{
			MessageType:  protocol.MessageType_IpIpPacketToProvider,
			MessageBytes: MessagePoolCopy([]byte(payload)),
		}},
	}
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: sendTransferPath(self.source.SourceId, DestinationId(self.client.ClientId())).ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		MessagePoolReturn(pack.Frames[0].MessageBytes)
		self.t.Fatalf("marshal synthetic receive Pack: %v", err)
	}
	receivePack := &ReceivePack{
		carrierDone: carrierDone, Source: self.source, SequenceId: sequenceId, Pack: pack,
		ReceiveCallback: self.client.receiveCallback, MessageByteCount: ByteCount(len(payload)),
		TransferFrameBytes: wire, TransportType: TransportTypeH1,
		Ctx: self.ctx, Unwrapped: true, EncryptionRole: sequenceTlsRoleServer,
	}
	accepted, err := self.client.receiveBuffer.Pack(receivePack, -1)
	if !accepted {
		receivePack.messagePoolReturn()
	}
	return messageId, accepted, err
}

// Requires real admission; every later progress assertion also checks payload
// and the cumulative ACK produced by the receiving sequence.
func (self *receiveCarrierLifecycleFixture) admit(
	sequenceId Id, number uint64, carrierDone <-chan struct{}, payload string,
) Id {
	self.t.Helper()
	messageId, accepted, err := self.offer(sequenceId, number, carrierDone, payload)
	if !accepted || err != nil {
		self.t.Fatalf("live receive admission: accepted=%t err=%v", accepted, err)
	}
	return messageId
}

// Reads the actual constructor's head and queue ownership at the held worker
// barrier, rather than assuming the fixture's settings reached the sequence.
func (self *receiveCarrierLifecycleFixture) requireQueued(sequenceId Id) *ReceiveSequence {
	self.t.Helper()
	synctest.Wait()
	buffer := self.client.receiveBuffer
	key := receiveSequenceHeadKey{Source: self.source, EncryptionRole: sequenceTlsRoleServer}
	buffer.mutex.Lock()
	head := buffer.headReceiveSequenceIds[key]
	sequence := buffer.receiveSequences[head]
	headCount, sequenceCount, activeCount := len(buffer.headReceiveSequenceIds), len(buffer.receiveSequences), len(buffer.activeReceiveSequences)
	buffer.mutex.Unlock()
	if sequence == nil || head.SequenceId != sequenceId || headCount != 1 || sequenceCount != 1 || activeCount != 1 {
		self.t.Fatalf("held receive head not constructed exactly once: head=%v heads=%d sequences=%d active=%d",
			head.SequenceId, headCount, sequenceCount, activeCount)
	}
	if sequence.ctx.Err() != nil || sequence.packQueueCount.Load() != 1 ||
		sequence.receiveBufferSettings.PackQueueBudget != self.packBudget ||
		sequence.receiveBufferSettings.ReceiveQueueBudget != self.receiveBudget ||
		self.packBudget.UsedByteCount() <= 0 || self.rootBudget.UsedByteCount() != self.packBudget.UsedByteCount() {
		self.t.Fatal("accepted Pack did not retain the actual constructor's live queue and ancestor budget")
	}
	if len(self.delivered) != 0 || len(self.output) != 0 {
		self.t.Fatal("held receive worker delivered or acknowledged before its barrier opened")
	}
	return sequence
}

// Virtual quiescence establishes readiness without a timing oracle. Both the
// borrowed callback and the real wire identity must match, exactly once.
func (self *receiveCarrierLifecycleFixture) requireDelivery(sequenceId, messageId Id, payload string) {
	self.t.Helper()
	synctest.Wait()
	select {
	case got := <-self.delivered:
		if got != payload {
			self.t.Fatalf("receive payload = %q, want %q", got, payload)
		}
	default:
		self.t.Fatalf("accepted receive Pack lost payload %q after physical carrier retirement", payload)
	}
	select {
	case wire := <-self.output:
		var frame protocol.TransferFrame
		err := ProtoUnmarshal(wire, &frame)
		MessagePoolReturn(wire)
		if err != nil || frame.Ack == nil {
			self.t.Fatalf("delivered Pack has no actual wire ACK: ack=%v err=%v", frame.Ack, err)
		}
		gotMessageId, messageErr := IdFromBytes(frame.Ack.MessageId)
		gotSequenceId, sequenceErr := IdFromBytes(frame.Ack.SequenceId)
		if messageErr != nil || sequenceErr != nil || gotMessageId != messageId ||
			gotSequenceId != sequenceId || frame.Ack.Selective || len(frame.Ack.MissingContractId) != 0 {
			self.t.Fatalf("wire ACK does not cumulatively acknowledge the delivered Pack: %+v", frame.Ack)
		}
	default:
		self.t.Fatal("delivered Pack lost its actual cumulative ACK after physical carrier retirement")
	}
	if len(self.delivered) != 0 || len(self.output) != 0 {
		self.t.Fatal("physical carrier retirement duplicated payload delivery or its ACK")
	}
	assertRetainedBudgetBalance(self.t, self.packBudget)
	assertRetainedBudgetBalance(self.t, self.receiveBudget)
	assertRetainedBudgetBalance(self.t, self.rootBudget)
}

// A preclosed lane cannot consume a logical head slot, even though a lower
// Pack refusal alone would otherwise happen after ReceiveBuffer construction.
func TestReceiveBufferPreclosedCarrierDoesNotCreateHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveCarrierLifecycleFixture(t)
		closedCarrier := make(chan struct{})
		close(closedCarrier)
		_, accepted, err := f.offer(NewId(), 0, closedCarrier, "synthetic retired cold head")
		synctest.Wait()
		buffer := f.client.receiveBuffer
		buffer.mutex.Lock()
		heads, sequences, active := len(buffer.headReceiveSequenceIds), len(buffer.receiveSequences), len(buffer.activeReceiveSequences)
		buffer.mutex.Unlock()
		if heads != 0 || sequences != 0 || active != 0 {
			t.Fatalf("preclosed physical carrier created receive state: heads=%d sequences=%d active=%d", heads, sequences, active)
		}
		if accepted || err == nil {
			t.Fatalf("preclosed cold Pack admission = (%t, %v), want caller-owned refusal", accepted, err)
		}
		assertRetainedBudgetBalance(t, f.rootBudget)
		sequenceId := NewId()
		messageId := f.admit(sequenceId, 0, make(chan struct{}), "synthetic live cold head")
		f.requireQueued(sequenceId)
		f.release()
		f.requireDelivery(sequenceId, messageId, "synthetic live cold head")
	})
}

// A newer id from a preclosed lane must not cancel already accepted healthy
// work. Missing payload is the failure oracle, not only the Pack return value.
func TestReceiveBufferPreclosedCarrierCannotSupersedeAcceptedHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveCarrierLifecycleFixture(t)
		liveCarrier := make(chan struct{})
		sequenceId := NewId()
		messageId := f.admit(sequenceId, 0, liveCarrier, "synthetic healthy accepted head")
		sequence := f.requireQueued(sequenceId)
		closedCarrier := make(chan struct{})
		close(closedCarrier)
		_, accepted, err := f.offer(NewId(), 0, closedCarrier, "synthetic retired newer head")
		f.release()
		f.requireDelivery(sequenceId, messageId, "synthetic healthy accepted head")
		if accepted || err == nil || sequence.ctx.Err() != nil {
			t.Fatalf("retired newer head changed healthy ownership: admitted=%t err=%v sequenceErr=%v", accepted, err, sequence.ctx.Err())
		}
		buffer := f.client.receiveBuffer
		buffer.mutex.Lock()
		head := buffer.headReceiveSequenceIds[receiveSequenceHeadKey{Source: f.source, EncryptionRole: sequenceTlsRoleServer}]
		sameSequence := buffer.receiveSequences[head] == sequence
		buffer.mutex.Unlock()
		if head.SequenceId != sequenceId || !sameSequence {
			t.Fatal("retired newer head replaced the healthy sequence's actual buffer indices")
		}
		messageId = f.admit(sequenceId, 1, liveCarrier, "synthetic healthy continuation")
		f.requireDelivery(sequenceId, messageId, "synthetic healthy continuation")
	})
}

// Closing after the outer admission check must still precede head creation.
// The existing constructor hook orders that race without a timer or worker.
func TestReceiveBufferCarrierCloseBeforeHeadInitDoesNotCreateHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveCarrierLifecycleFixture(t)
		carrierDone := make(chan struct{})
		retiredSequenceId := NewId()
		constructorCalls := 0
		buffer := f.client.receiveBuffer
		buffer.beforeCreateReceiveSequenceForTest = func(id receiveSequenceId) {
			if id.SequenceId == retiredSequenceId {
				constructorCalls += 1
				if constructorCalls == 1 {
					close(carrierDone)
				}
			}
		}
		// The locked refusal may consume and return the Pack. Only actual
		// head ownership and later healthy progress establish the contract.
		f.offer(retiredSequenceId, 0, carrierDone, "synthetic carrier closed before cold head init")
		synctest.Wait()
		if constructorCalls != 1 {
			t.Fatalf("cold constructor boundary reached %d times, want 1", constructorCalls)
		}
		buffer.mutex.Lock()
		heads, sequences, active := len(buffer.headReceiveSequenceIds), len(buffer.receiveSequences), len(buffer.activeReceiveSequences)
		buffer.mutex.Unlock()
		if heads != 0 || sequences != 0 || active != 0 {
			t.Fatalf("carrier closed at constructor boundary created receive state: heads=%d sequences=%d active=%d", heads, sequences, active)
		}
		assertRetainedBudgetBalance(t, f.rootBudget)
		sequenceId := NewId()
		messageId := f.admit(sequenceId, 0, make(chan struct{}), "synthetic healthy head after constructor refusal")
		f.requireQueued(sequenceId)
		f.release()
		f.requireDelivery(sequenceId, messageId, "synthetic healthy head after constructor refusal")
	})
}

// A carrier that closes at the locked constructor boundary cannot supersede
// queued healthy work; its original payload and cumulative ACK must survive.
func TestReceiveBufferCarrierCloseBeforeHeadInitPreservesAcceptedHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveCarrierLifecycleFixture(t)
		liveCarrier := make(chan struct{})
		sequenceId := NewId()
		messageId := f.admit(sequenceId, 0, liveCarrier, "synthetic accepted head before constructor refusal")
		sequence := f.requireQueued(sequenceId)
		carrierDone := make(chan struct{})
		retiredSequenceId := NewId()
		constructorCalls := 0
		buffer := f.client.receiveBuffer
		buffer.beforeCreateReceiveSequenceForTest = func(id receiveSequenceId) {
			if id.SequenceId == retiredSequenceId {
				constructorCalls += 1
				if constructorCalls == 1 {
					close(carrierDone)
				}
			}
		}
		f.offer(retiredSequenceId, 0, carrierDone, "synthetic newer carrier closed before head init")
		if constructorCalls != 1 {
			t.Fatalf("warm constructor boundary reached %d times, want 1", constructorCalls)
		}
		f.release()
		f.requireDelivery(sequenceId, messageId, "synthetic accepted head before constructor refusal")
		buffer.mutex.Lock()
		head := buffer.headReceiveSequenceIds[receiveSequenceHeadKey{Source: f.source, EncryptionRole: sequenceTlsRoleServer}]
		sameSequence := buffer.receiveSequences[head] == sequence
		buffer.mutex.Unlock()
		if sequence.ctx.Err() != nil || head.SequenceId != sequenceId || !sameSequence {
			t.Fatal("carrier closed at constructor boundary canceled or replaced the accepted healthy head")
		}
		messageId = f.admit(sequenceId, 1, liveCarrier, "synthetic healthy continuation after constructor refusal")
		f.requireDelivery(sequenceId, messageId, "synthetic healthy continuation after constructor refusal")
	})
}

// Once admitted, physical retirement cannot revoke queue ownership. A live
// carrier can continue the same logical sequence and later replace its head.
func TestReceiveBufferAcceptedPackSurvivesCarrierClose(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiveCarrierLifecycleFixture(t)
		carrierDone := make(chan struct{})
		sequenceId := NewId()
		messageId := f.admit(sequenceId, 0, carrierDone, "synthetic accepted before retirement")
		sequence := f.requireQueued(sequenceId)
		used := f.packBudget.UsedByteCount()
		close(carrierDone)
		synctest.Wait()
		if sequence.ctx.Err() != nil || sequence.packQueueCount.Load() != 1 || f.packBudget.UsedByteCount() != used {
			t.Fatal("physical retirement revoked an already accepted logical Pack owner")
		}
		f.release()
		f.requireDelivery(sequenceId, messageId, "synthetic accepted before retirement")
		liveCarrier := make(chan struct{})
		messageId = f.admit(sequenceId, 1, liveCarrier, "synthetic replacement carrier continuation")
		f.requireDelivery(sequenceId, messageId, "synthetic replacement carrier continuation")
		nextSequenceId := NewId()
		messageId = f.admit(nextSequenceId, 0, liveCarrier, "synthetic live successor sequence")
		f.requireDelivery(nextSequenceId, messageId, "synthetic live successor sequence")
	})
}
