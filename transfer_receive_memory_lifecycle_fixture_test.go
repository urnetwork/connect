// Connect-owned synthetic budgets isolate decoded-owner lifetime. These values
// are a unit-test geometry, not an SDK profile or a phone-memory qualification.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// Deliberately small, independent physical and encoded limits leave room for
// one decoded original, unrelated scratch, and a fully occupied Pack leaf.
type receiveMemoryLifecycleSettings struct {
	rootByteCount      ByteCount
	receiveByteCount   ByteCount
	packByteCount      ByteCount
	sendByteCount      ByteCount
	sequenceCount      int
	sequenceByteCount  ByteCount
	ackCompressTimeout time.Duration
}

// Fixed synthetic settings need no SDK import, environment, receipt, global
// memory-profile change, or machine-dependent policy factory.
func receiveMemoryLifecycleTestSettings() receiveMemoryLifecycleSettings {
	return receiveMemoryLifecycleSettings{
		rootByteCount:      mib(1),
		receiveByteCount:   kib(128),
		packByteCount:      kib(64),
		sendByteCount:      kib(256),
		sequenceCount:      16,
		sequenceByteCount:  kib(64),
		ackCompressTimeout: 10 * time.Millisecond,
	}
}

// Separate RX and Pack leaves share the same client/root hierarchy. Exact
// balance checks run after the later-registered real client cleanup.
func receiveMemoryLifecycleBudgets(t *testing.T, settings receiveMemoryLifecycleSettings) (*TransferMemoryBudget, *TransferMemoryBudget, *TransferMemoryBudget, *TransferMemoryBudget) {
	t.Helper()
	root := NewTransferMemoryBudget(settings.rootByteCount)
	client := NewTransferMemoryBudgetWithParent(settings.rootByteCount, root)
	rx := NewTransferMemoryBudgetWithParent(settings.receiveByteCount, client)
	pack := NewTransferMemoryBudgetWithParent(settings.packByteCount, client)
	t.Cleanup(func() {
		for _, budget := range []*TransferMemoryBudget{rx, pack, client, root} {
			assertRetainedBudgetBalance(t, budget)
		}
	})
	return rx, pack, client, root
}

// Scalar retry evidence belongs to one real source sequence. The existing
// wire observer records only that original's actual retry, without holding it.
type receiveMemoryLifecycleFlow struct {
	fixture      *windowRoundFixture
	settings     receiveMemoryLifecycleSettings
	sourceBudget *TransferMemoryBudget
	admitted     int
	failed       int
	retryTarget  Id
	retryNumber  uint64
	retryWritten chan struct{}
}

// All policies precede worker construction. Existing channel routes carry
// actual Send/Client ingress without a socket or SDK policy factory.
func newReceiveMemoryLifecycleFlow(t *testing.T, settings receiveMemoryLifecycleSettings, rx, pack *TransferMemoryBudget) *receiveMemoryLifecycleFlow {
	t.Helper()
	flow := &receiveMemoryLifecycleFlow{
		settings:     settings,
		sourceBudget: NewTransferMemoryBudget(settings.sendByteCount),
		retryWritten: make(chan struct{}),
	}
	t.Cleanup(func() {
		if flow.fixture != nil && flow.fixture.ackedCount+flow.failed != flow.admitted {
			t.Errorf("terminal callbacks differ from admitted groups: success=%d failed=%d admitted=%d", flow.fixture.ackedCount, flow.failed, flow.admitted)
		}
		assertRetainedBudgetBalance(t, flow.sourceBudget)
	})
	flow.fixture = newWindowRoundFixture(t, func(send *SendBufferSettings) {
		send.WindowSizing = WindowSizingFromDelivery
		send.ApplyWindowSizing()
		send.ChunkSettings = DefaultSendBufferChunkSettings()
		send.ResendQueueBudget = flow.sourceBudget
		send.ResendQueueRetainedByteAccounting = true
		send.ResendQueueMaxByteCount = settings.sendByteCount / 2
		send.ResendQueueMinByteCount = settings.sendByteCount / 8
		send.SequenceBufferSize = settings.sequenceCount
		send.LogicalDataLaneCount = 1
		send.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
			if event.Resend && flow.retryTarget != (Id{}) && event.MessageId == flow.retryTarget && event.SequenceNumber == flow.retryNumber {
				select {
				case <-flow.retryWritten:
				default:
					close(flow.retryWritten)
				}
			}
		}
	}, func(receive *ReceiveBufferSettings) {
		receive.ReceiveQueueBudget = rx
		receive.PackQueueBudget = pack
		receive.ReceiveQueueRetainedByteAccounting = true
		receive.PackQueueRetainedByteAccounting = false
		receive.ReceiveQueueMinByteCount = 0
		receive.ReceiveQueueMaxByteCount = settings.receiveByteCount
		receive.SequenceBufferSize = settings.sequenceCount
		receive.H1SequenceBufferSize = settings.sequenceCount
		receive.SequenceBufferByteCount = settings.sequenceByteCount
		receive.H1SequenceBufferByteCount = settings.sequenceByteCount
		receive.ReliablePackHandoffTimeout = -1
		receive.H1PackHandoffTimeout = -1
		receive.H1AckHandoffTimeout = time.Millisecond
		receive.AckCompressTimeout = settings.ackCompressTimeout
		receive.AdvertiseReceiveWindow = true
		receive.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		receive.EvictionNotice = true
	}, TransportTypeH1)
	transport := privateH1FixtureSendTransport(t, flow.fixture)
	flow.fixture.sender.RouteManager().UpdateTransportWithProperties(transport, []Route{flow.fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	manager := flow.fixture.receiver.RouteManager()
	manager.mutex.Lock()
	var readers []Transport
	for transport, routes := range manager.readerMatchState.transportRoutes {
		for _, route := range routes {
			if route == flow.fixture.receiverIn {
				readers = append(readers, transport)
				break
			}
		}
	}
	manager.mutex.Unlock()
	if len(readers) != 1 {
		t.Fatal("controlled receive lane has no unique transport owner")
	}
	manager.RemoveTransport(readers[0])
	manager.UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{flow.fixture.receiverIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	synctest.Wait()
	return flow
}

// Eight standard-MTU raw frames share one known-limit H1 original. The actual
// sender chooses its encoding and owns completion through real ACK retirement.
func (self *receiveMemoryLifecycleFlow) group(t *testing.T, frameCount int) *windowRoundFrame {
	t.Helper()
	frames := (&SendPack{}).Frames
	for range frameCount {
		packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 101, bytes.Repeat([]byte{0x42}, 1280-Ipv4HeaderSizeWithoutExtensions-TcpHeaderSizeWithoutExtensions)))
		frame, err := ipPacketFromProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		if !frame.Raw {
			MessagePoolReturn(packet)
		}
		frames = append(frames, frame)
	}
	accepted, err := self.fixture.sender.sendGroupWithTimeoutDetailed(frames, self.fixture.receiver.ClientId(), func(err error) {
		if err != nil {
			self.failed++
		} else {
			self.fixture.ackedCount++
		}
	}, time.Second, TransferKey{LogicalLane: 1})
	if !accepted || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		t.Fatalf("controlled logical group refused: accepted=%t error=%v", accepted, err)
	}
	self.admitted++
	wire := self.fixture.takePack(self.fixture.nextNumber)
	self.fixture.nextNumber++
	if len(wire.pack.Frames) != frameCount || len(wire.bytes) > 16*1024 || receiveDeliveryHasTcp(wire.pack.Frames) {
		t.Fatal("controlled raw-frame family changed its chunk or wire envelope")
	}
	return wire
}

// Advance only the synctest clock through the actual compression interval.
// A real cumulative ACK and empty resend queue establish the source handoff.
func (self *receiveMemoryLifecycleFlow) warm(t *testing.T) {
	t.Helper()
	wire := self.group(t, 8)
	self.fixture.forward(wire, self.fixture.receiverIn)
	time.Sleep(2 * self.settings.ackCompressTimeout)
	self.fixture.acknowledge()
	sequence := self.fixture.sequence()
	grant, ok := sequence.receivedWindowAdvertisement()
	if !ok || grant != self.settings.receiveByteCount || sequence.resendQueue.Len() != 0 || self.fixture.ackedCount != 1 || self.fixture.deliveredCount != 8 {
		t.Fatal("controlled warm ACK did not retire its actual source original")
	}
}
