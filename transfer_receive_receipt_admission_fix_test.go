// Private, uncompiled/unrun P3 correction: real Pack/provider admission keeps
// refused messages retriable without writing receive heads or ACK state.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The initial real SYN fills one final TCP slot. Every TCP worker is held
// before Run, so final queue admission never implies a socket or remote ACK.
type receiptAdmissionFixture struct {
	t                *testing.T
	ctx              context.Context
	cancel           context.CancelFunc
	client           *Client
	provider         *RemoteUserNatProvider
	nat              *LocalUserNat
	source           TransferPath
	sequenceId       Id
	firstTcp         *TcpSequence
	captured         chan *TcpSequence
	route            Route
	receiveBudget    *TransferMemoryBudget
	packBudget       *TransferMemoryBudget
	witnesses        [][]byte
	genericFrames    []*protocol.Frame
	genericSequences map[*ReceiveSequence]struct{}
	acks             []*protocol.Ack
	gateway          receiptGatewayEvidence
	dials            atomic.Int64
	stateLock        sync.Mutex
	deliveryPorts    map[int]int
}

// Construction uses the production Client, ReceiveBuffer, provider and NAT.
// Count is the configured receipt bound, not a mutated ledger field.
func newReceiptAdmissionFixture(t *testing.T, count int, configure ...func(*ReceiveBufferSettings)) *receiptAdmissionFixture {
	return newReceiptAdmissionFixtureWithProvider(t, count, true, configure...)
}

// The optional provider is used to exercise real late callback publication.
func newReceiptAdmissionFixtureWithProvider(t *testing.T, count int, withProvider bool, configure ...func(*ReceiveBufferSettings)) *receiptAdmissionFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &receiptAdmissionFixture{t: t, ctx: ctx, cancel: cancel, source: SourceId(NewId()),
		sequenceId: NewId(), captured: make(chan *TcpSequence, 8), route: make(Route, 64),
		receiveBudget: NewTransferMemoryBudget(256 * 1024), packBudget: NewTransferMemoryBudget(64 * 1024),
		deliveryPorts: map[int]int{}, genericSequences: map[*ReceiveSequence]struct{}{}}
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	receive := DefaultReceiveBufferSettingsWithBufferSize(count)
	receive.IdleTimeout, receive.GapTimeout, receive.MaxPeerAuditDuration = time.Hour, time.Hour, time.Hour
	receive.AckCompressTimeout, receive.WriteTimeout = 0, 0
	receive.ReliablePackHandoffTimeout, receive.H1PackHandoffTimeout = 0, 0
	receive.ReceiveQueueBudget, receive.ReceiveQueueMinByteCount = f.receiveBudget, 0
	receive.ReceiveQueueMaxByteCount, receive.ReceiveQueueRetainedByteAccounting = 64*1024, true
	receive.PackQueueBudget, receive.PackQueueRetainedByteAccounting = f.packBudget, true
	for _, apply := range configure {
		apply(receive)
	}
	settings.ReceiveBufferSettings = receive
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
	transport := NewSendGatewayTransport()
	f.client.RouteManager().UpdateTransport(transport, []Route{f.route})
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
	natSettings.Log = NewNoopLogger()
	natSettings.TcpBufferSettings.WriteTimeout = 0
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
		f.captured <- sequence
		<-sequence.ctx.Done()
	}
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			// Teardown can consume a queued SYN after cancellation wins the
			// barrier. No socket exists, including on that canceled path.
			if ctx.Err() == nil {
				f.dials.Add(1)
			}
			return nil, context.Canceled
		},
	}
	f.nat = NewLocalUserNat(ctx, "synthetic receipt admission", natSettings)
	if withProvider {
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		f.provider = NewRemoteUserNatProvider(f.client, f.nat, providerSettings)
	}
	f.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider &&
				frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			var path IpPath
			if err != nil {
				t.Error(err)
				continue
			}
			if _, err := parseIpPathWithPayloadBorrowed(packet, &path); err != nil {
				t.Error(err)
				continue
			}
			f.stateLock.Lock()
			f.deliveryPorts[path.SourcePort]++
			f.stateLock.Unlock()
		}
	})
	t.Cleanup(func() {
		cancel()
		if f.provider != nil {
			f.provider.Close()
		}
		if err := f.nat.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		f.client.RouteManager().RemoveTransport(transport)
		for len(f.route) != 0 {
			packet := <-f.route
			_, err := f.gateway.consume(packet, f.client.ClientId(), f.source.SourceId, f.provider)
			MessagePoolReturn(packet)
			if err != nil {
				t.Error(err)
			}
		}
		if err := receiptAdmissionGenericOwnersError(f.genericFrames, f.genericSequences); err != nil {
			t.Error(err)
		}
		for index, witness := range f.witnesses {
			if !MessagePoolReturn(witness) {
				t.Errorf("packet witness %d retained another owner after all joins", index)
			}
		}
		if f.receiveBudget.UsedByteCount() != 0 || f.packBudget.UsedByteCount() != 0 {
			t.Errorf("joined receive/Pack budgets retained %d/%d bytes", f.receiveBudget.UsedByteCount(), f.packBudget.UsedByteCount())
		}
		if f.dials.Load() != 0 {
			t.Error("fixture attempted a live upstream dial")
		}
	})
	packet := MessagePoolCopy(receiptAdmissionPacket(41000, tcpFlagSyn, 100, nil))
	if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, packet, 0) {
		MessagePoolReturn(packet)
		t.Fatal("initial SYN did not enter the actual NAT queue")
	}
	synctest.Wait()
	f.firstTcp = f.takeTcp()
	if cap(f.firstTcp.sendItems) != 1 || len(f.firstTcp.sendItems) != 1 {
		t.Fatal("initial SYN did not occupy the actual one-slot final queue")
	}
	return f
}

// Synthetic data includes only documentation addresses and test-only ports.
func receiptAdmissionPacket(port int, flags byte, number uint32, payload []byte) []byte {
	return ipOosTcpPacketSequence(&IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.ParseIP("192.0.2.1").To4(), SourcePort: port,
		DestinationIp: net.ParseIP("198.51.100.2").To4(), DestinationPort: 443}, flags, number, payload)
}

// Supplies a complete decoded, ACK-required Pack through real buffer admission.
// Success transfers the packet; refusal leaves it for the caller to return.
func (self *receiptAdmissionFixture) offer(sequenceId Id, lane uint32, number uint64, messageId Id, port int, flags byte, payload []byte, configure ...func(*ReceivePack)) {
	self.t.Helper()
	tcpNumber := uint32(100)
	if flags&tcpFlagSyn == 0 {
		tcpNumber++
	}
	packet := MessagePoolCopy(receiptAdmissionPacket(port, flags, tcpNumber, payload))
	self.witnesses = append(self.witnesses, MessagePoolShareReadOnly(packet))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		self.t.Fatalf("raw complete Pack fixture: err=%v frame=%+v", err, frame)
	}
	pack := &ReceivePack{Source: self.source, SequenceId: sequenceId,
		Pack: &protocol.Pack{MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(),
			SequenceNumber: number, LogicalLane: lane, Head: number == 0, Frames: []*protocol.Frame{frame}},
		ReceiveCallback: self.client.receiveCallback, Ctx: self.ctx, Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, TransportType: TransportTypeH1, MessageByteCount: ByteCount(len(packet))}
	for _, apply := range configure {
		apply(pack)
	}
	if err := self.gateway.expectPack(pack.Pack); err != nil {
		pack.messagePoolReturn()
		self.t.Fatal(err)
	}
	if accepted, err := self.client.receiveBuffer.Pack(pack, 0); !accepted || err != nil {
		pack.messagePoolReturn()
		self.t.Fatalf("zero-wait Pack handoff refused before receipt boundary: accepted=%t err=%v", accepted, err)
	}
	synctest.Wait()
	self.collectAcks()
}

// Read the constructor-owned index only after durable quiescence, without
// injecting a sequence or changing its head, hold, delivery ledger or ACKs.
func (self *receiptAdmissionFixture) sequence(sequenceId Id, lane uint32) *ReceiveSequence {
	self.client.receiveBuffer.mutex.Lock()
	defer self.client.receiveBuffer.mutex.Unlock()
	return self.client.receiveBuffer.receiveSequences[receiveSequenceId{Source: self.source,
		SequenceId: sequenceId, LogicalLane: lane, EncryptionRole: sequenceTlsRoleServer}]
}

// Record actual ACKs and the provider's independently owned diagnostic Pack.
// Neither receipt state nor a simulated peer acknowledgment is manufactured.
func (self *receiptAdmissionFixture) collectAcks() {
	for {
		select {
		case packet := <-self.route:
			ack, err := self.gateway.consume(packet, self.client.ClientId(), self.source.SourceId, self.provider)
			MessagePoolReturn(packet)
			if err != nil {
				self.t.Fatal(err)
			}
			if ack != nil {
				self.acks = append(self.acks, ack)
			}
		default:
			return
		}
	}
}

// Count dispatch independently from final TCP admission and wire ACK evidence.
func (self *receiptAdmissionFixture) delivered(port int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.deliveryPorts[port]
}

// A selective receipt is not a cumulative delivery acknowledgment.
func (self *receiptAdmissionFixture) hasAck(sequenceId, messageId Id, selective bool) bool {
	for _, ack := range self.acks {
		if bytes.Equal(ack.SequenceId, sequenceId.Bytes()) && bytes.Equal(ack.MessageId, messageId.Bytes()) && ack.Selective == selective {
			return true
		}
	}
	return false
}

// The pre-Run barrier, not a negative timer, proves each captured final owner.
func (self *receiptAdmissionFixture) takeTcp() *TcpSequence {
	self.t.Helper()
	select {
	case sequence := <-self.captured:
		return sequence
	default:
		self.t.Fatal("expected real TCP final owner was not constructed")
		return nil
	}
}

// Capacity release returns the exact final owner and publishes its real wake.
func (self *receiptAdmissionFixture) releaseTcp(sequence *TcpSequence, port int, payload []byte) {
	self.t.Helper()
	select {
	case item := <-sequence.sendItems:
		if int(item.tcp.sourcePort) != port || !bytes.Equal(item.tcp.payload, payload) {
			self.t.Errorf("final queue changed packet: port=%d payload=%q", item.tcp.sourcePort, item.tcp.payload)
		}
		item.release()
	default:
		self.t.Error("expected final TCP packet is absent")
	}
	synctest.Wait()
	self.collectAcks()
}

// Root: one pending data receipt fills the real ledger. Another flow on the
// same logical lane must be refused without killing it or falsely ACKing retry.
func TestReceiveReceiptAdmissionRefusalKeepsExactRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		firstId, refusedId := NewId(), NewId()
		payload := []byte("synthetic pending TCP data")
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil {
			t.Fatal("actual receive/dispatch did not create the receipt ledger")
		}
		q := sequence.deliveryQueue
		if cap(sequence.packs) != 1 || q.maxCount != 1 || q.maxBytes != 64*1024 || len(q.items) != 1 || q.controlCount != 0 ||
			q.items[0].pending != 1 || q.items[0].secured || sequence.nextSequenceNumber != 1 || f.delivered(41000) != 1 {
			t.Fatal("constructor/real final-queue pressure premise was not established")
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present || len(f.acks) != 0 {
			t.Fatal("pending original already owns ACK evidence")
		}
		f.offer(f.sequenceId, 0, 1, refusedId, 41001, tcpFlagSyn, nil)
		if sequence.nextSequenceNumber != 1 || sequence.ctx.Err() != nil || q.closed ||
			len(q.items) != 1 || q.items[0].pending != 1 || f.delivered(41001) != 0 {
			t.Fatal("receipt pressure advanced or canceled the receive owner instead of refusing the complete Pack")
		}
		if f.hasAck(f.sequenceId, firstId, false) || f.hasAck(f.sequenceId, firstId, true) {
			t.Error("capacity refusal acknowledged the first flow without final admission")
		}
		f.offer(f.sequenceId, 0, 1, refusedId, 41001, tcpFlagSyn, nil)
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		if f.hasAck(f.sequenceId, refusedId, false) || f.hasAck(f.sequenceId, refusedId, true) ||
			sequence.nextSequenceNumber != 1 || f.delivered(41000) != 1 || f.delivered(41001) != 0 {
			t.Fatal("refused or still-pending duplicate acquired delivery/ACK ownership")
		}
		// Another lane of the same source/H1 classification retains its own
		// real provider and final queue; the blast radius is not the Client.
		siblingSequenceId, siblingId := NewId(), NewId()
		f.offer(siblingSequenceId, 1, 0, siblingId, 41002, tcpFlagSyn, nil)
		sibling := f.sequence(siblingSequenceId, 1)
		if sibling == nil || sibling.ctx.Err() != nil || f.client.Ctx().Err() != nil || f.provider.ctx.Err() != nil ||
			f.delivered(41002) != 1 || !f.hasAck(siblingSequenceId, siblingId, false) {
			t.Fatal("receipt refusal spread beyond the failing logical lane")
		}
		f.releaseTcp(f.takeTcp(), 41002, nil)
		f.client.receiveBuffer.mutex.Lock()
		tombstones := len(f.client.receiveBuffer.rejectedReceiveSequenceIds)
		f.client.receiveBuffer.mutex.Unlock()
		if tombstones != 0 {
			t.Fatal("capacity refusal became an authentication retransmit tombstone")
		}
		// The original pending owner survives. Capacity alone secures it;
		// only the refused second Pack needs a same-identity retransmission.
		f.releaseTcp(f.firstTcp, 41000, nil)
		if !f.hasAck(f.sequenceId, firstId, false) || f.delivered(41000) != 1 {
			t.Fatal("capacity release lost the original pending owner")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
		f.offer(f.sequenceId, 0, 1, refusedId, 41001, tcpFlagSyn, nil)
		if f.sequence(f.sequenceId, 0) != sequence || f.delivered(41001) != 1 || !f.hasAck(f.sequenceId, refusedId, false) {
			t.Fatal("same-sequence retry was ACKed without one actual dispatch/final owner")
		}
		f.releaseTcp(f.takeTcp(), 41001, nil)
		f.offer(f.sequenceId, 0, 1, refusedId, 41001, tcpFlagSyn, nil)
		if f.delivered(41001) != 1 {
			t.Error("already secured duplicate delivered the second flow twice")
		}
	})
}

// With room, the second flow owns its final slot and SACK while the first
// receipt still waits. Only securing the first can release a cumulative ACK.
func TestReceiveReceiptCapacityHealthyRoomKeepsOtherFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 2)
		firstId, secondId := NewId(), NewId()
		payload := []byte("synthetic first flow")
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		f.offer(f.sequenceId, 0, 1, secondId, 41001, tcpFlagSyn, nil)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.ctx.Err() != nil || sequence.deliveryQueue.maxCount != 2 ||
			f.delivered(41001) != 1 || !f.hasAck(f.sequenceId, secondId, true) || f.hasAck(f.sequenceId, secondId, false) {
			t.Fatal("healthy second flow did not retain selective-only final ownership")
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present {
			t.Fatal("healthy later flow cumulatively skipped pending first flow")
		}
		f.releaseTcp(f.takeTcp(), 41001, nil)
		f.releaseTcp(f.firstTcp, 41000, nil)
		if !f.hasAck(f.sequenceId, secondId, false) || len(sequence.deliveryQueue.items) != 0 || sequence.deliveryQueue.bytes != 0 {
			t.Error("capacity release did not cumulatively secure the exact prefix")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
		f.offer(f.sequenceId, 0, 1, secondId, 41001, tcpFlagSyn, nil)
		if f.delivered(41000) != 1 || f.delivered(41001) != 1 {
			t.Error("healthy duplicate changed exact-once callback custody")
		}
	})
}

// Genuine owner cancellation remains terminal and returns the pending claim,
// packet roots and exact budgets without inventing a delivery ACK.
func TestReceiveReceiptExplicitCancelReturnsOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		messageId := NewId()
		f.offer(f.sequenceId, 0, 0, messageId, 41000, tcpFlagAck, []byte("synthetic canceled data"))
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil || len(sequence.deliveryQueue.items) != 1 || f.receiveBudget.UsedByteCount() == 0 {
			t.Fatal("cancellation control did not own a real pending receipt")
		}
		sequence.Cancel()
		synctest.Wait()
		f.collectAcks()
		select {
		case <-sequence.done:
		default:
			t.Fatal("explicit cancellation did not join the actual receive owner")
		}
		q := sequence.deliveryQueue
		if len(q.items) != 0 || len(q.operations) != 0 || q.bytes != 0 || f.receiveBudget.UsedByteCount() != 0 || f.packBudget.UsedByteCount() != 0 ||
			f.hasAck(f.sequenceId, messageId, false) || f.hasAck(f.sequenceId, messageId, true) {
			t.Error("explicit cancellation leaked custody or acknowledged undelivered data")
		}
		if f.client.Ctx().Err() != nil || f.provider.ctx.Err() != nil || f.nat.ctx.Err() != nil {
			t.Error("explicit sequence cancellation closed unrelated owners")
		}
	})
}

// Constructor byte pressure, with spare receipt count, has the same retry
// boundary. The observed first charge must prove the exact byte premise.
func TestReceiveReceiptAdmissionBytePressureKeepsRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 4, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 10 * 1024
		})
		firstId, secondId := NewId(), NewId()
		payload := bytes.Repeat([]byte("b"), 1000)
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil {
			t.Fatal("first data receipt was not constructed")
		}
		q := sequence.deliveryQueue
		if len(q.items) != 1 || q.maxCount != 4 || q.bytes > q.maxBytes || q.bytes*2 <= q.maxBytes {
			t.Fatalf("actual constructor charge did not isolate byte pressure: count=%d bytes=%d limit=%d", len(q.items), q.bytes, q.maxBytes)
		}
		ownedBytes := f.receiveBudget.UsedByteCount()
		f.offer(f.sequenceId, 0, 1, secondId, 41000, tcpFlagAck, payload)
		if sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 || f.delivered(41000) != 1 ||
			f.receiveBudget.UsedByteCount() != ownedBytes || f.hasAck(f.sequenceId, secondId, false) {
			t.Fatal("receipt byte refusal changed the owner, reservation, or receive head")
		}
		f.releaseTcp(f.firstTcp, 41000, nil)
		f.offer(f.sequenceId, 0, 1, secondId, 41000, tcpFlagAck, payload)
		f.releaseTcp(f.firstTcp, 41000, payload)
		if f.delivered(41000) != 2 || !f.hasAck(f.sequenceId, secondId, false) {
			t.Fatal("byte-capacity retry did not secure exactly the second message")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
	})
}

// A competing exact owner fills the shared budget. Refusal must happen before
// the first head moves, and releasing that owner permits the original retry.
func TestReceiveReceiptAdmissionSharedBudgetKeepsRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 2)
		reserved := f.receiveBudget.Available()
		if !f.receiveBudget.TryReserve(reserved) {
			t.Fatal("independent shared-budget owner was not admitted")
		}
		t.Cleanup(func() {
			if reserved != 0 {
				f.receiveBudget.Release(reserved)
			}
		})
		messageId := NewId()
		payload := []byte("shared exact owner pressure")
		f.offer(f.sequenceId, 0, 0, messageId, 41000, tcpFlagAck, payload)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 0 ||
			f.delivered(41000) != 0 || len(f.acks) != 0 || f.receiveBudget.UsedByteCount() != reserved {
			t.Fatal("shared-budget refusal advanced/canceled the head or stole an existing reservation")
		}
		f.receiveBudget.Release(reserved)
		reserved = 0
		f.offer(f.sequenceId, 0, 0, messageId, 41000, tcpFlagAck, payload)
		f.releaseTcp(f.firstTcp, 41000, nil)
		if f.delivered(41000) != 1 || !f.hasAck(f.sequenceId, messageId, false) {
			t.Fatal("shared-budget release did not permit the exact original retry")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
	})
}

// A real out-of-order Pack owns a selective lease before reaching the receipt
// boundary. It must stay retained, then drain from a capacity wake alone.
func TestReceiveReceiptAdmissionRetainsSelectiveHeldOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		firstId, heldId := NewId(), NewId()
		f.offer(f.sequenceId, 0, 1, heldId, 41001, tcpFlagSyn, nil)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil {
			t.Fatal("out-of-order owner was not constructed")
		}
		held := sequence.receiveQueue.GetByMessageId(heldId)
		if held == nil || !held.committed || !f.hasAck(f.sequenceId, heldId, true) || held.memoryBudget == nil {
			t.Fatal("actual reorder admission did not secure a selective retained owner")
		}
		payload := []byte("pending before retained selective owner")
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		if sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 ||
			sequence.receiveQueue.GetByMessageId(heldId) != held || f.delivered(41001) != 0 || f.hasAck(f.sequenceId, heldId, false) {
			t.Fatal("receipt pressure discarded or cumulatively promoted the selectively held owner")
		}
		f.offer(f.sequenceId, 0, 1, heldId, 41001, tcpFlagSyn, nil)
		if sequence.receiveQueue.GetByMessageId(heldId) != held || f.delivered(41001) != 0 {
			t.Fatal("held duplicate replaced the exact selective owner under pressure")
		}
		f.releaseTcp(f.firstTcp, 41000, nil)
		if sequence.nextSequenceNumber != 2 || sequence.receiveQueue.Len() != 0 ||
			f.delivered(41001) != 1 || !f.hasAck(f.sequenceId, heldId, false) {
			t.Fatal("capacity-only wake did not drain the retained selective owner")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
		f.releaseTcp(f.takeTcp(), 41001, nil)
	})
}

// Terminal cancellation returns both existing owners without revoking the
// historical SACK into a false cumulative delivery acknowledgment.
func TestReceiveReceiptAdmissionSelectiveCancelReturnsOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		firstId, heldId := NewId(), NewId()
		f.offer(f.sequenceId, 0, 1, heldId, 41001, tcpFlagSyn, nil)
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, []byte("cancel retained selective owner"))
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.ctx.Err() != nil || sequence.receiveQueue.Len() != 1 ||
			sequence.deliveryQueue == nil || !f.hasAck(f.sequenceId, heldId, true) {
			t.Fatal("real pending receipt and selectively held owner were not both retained")
		}
		sequence.Cancel()
		synctest.Wait()
		f.collectAcks()
		select {
		case <-sequence.done:
		default:
			t.Fatal("selective-owner cancellation did not join the receive worker")
		}
		if sequence.receiveQueue.Len() != 0 || sequence.deliveryQueue.pending() ||
			f.receiveBudget.UsedByteCount() != 0 || f.packBudget.UsedByteCount() != 0 ||
			f.hasAck(f.sequenceId, firstId, false) || f.hasAck(f.sequenceId, heldId, false) {
			t.Fatal("selective-owner cancellation leaked memory or invented cumulative delivery")
		}
	})
}

// Sender-authorized recovery heads retain their existing jump semantics, but
// the jump itself may not happen until this complete Pack can own a receipt.
func TestReceiveReceiptAdmissionRefusedHeadDoesNotJump(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		firstId, headId := NewId(), NewId()
		payload := []byte("pending recovery-head predecessor")
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		sequence := f.sequence(f.sequenceId, 0)
		head := func(pack *ReceivePack) { pack.Pack.Head = true }
		f.offer(f.sequenceId, 0, 5, headId, 41001, tcpFlagSyn, nil, head)
		if sequence == nil || sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 ||
			f.delivered(41001) != 0 || f.hasAck(f.sequenceId, headId, false) {
			t.Fatal("refused recovery head jumped the receive sequence")
		}
		f.releaseTcp(f.firstTcp, 41000, nil)
		f.releaseTcp(f.firstTcp, 41000, payload)
		f.offer(f.sequenceId, 0, 5, headId, 41001, tcpFlagSyn, nil, head)
		if sequence.nextSequenceNumber != 6 || f.delivered(41001) != 1 || !f.hasAck(f.sequenceId, headId, false) {
			t.Fatal("admitted recovery head lost existing sender-directed reset semantics")
		}
		f.releaseTcp(f.takeTcp(), 41001, nil)
	})
}

// An item whose retained roots cannot fit an empty receipt ledger is refused
// before any SACK promise. A small unrelated complete Pack still progresses.
func TestReceiveReceiptAdmissionOversizeDoesNotLease(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 2, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 6000
		})
		oversizeId, healthyId := NewId(), NewId()
		f.offer(f.sequenceId, 0, 1, oversizeId, 41000, tcpFlagAck, bytes.Repeat([]byte("x"), 4096))
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 0 ||
			sequence.receiveQueue.Len() != 0 || len(f.acks) != 0 || f.receiveBudget.UsedByteCount() != 0 {
			t.Fatal("oversized receipt acquired an undeliverable selective lease")
		}
		f.offer(f.sequenceId, 0, 0, healthyId, 41001, tcpFlagSyn, nil)
		if sequence.ctx.Err() != nil || f.delivered(41001) != 1 || !f.hasAck(f.sequenceId, healthyId, false) {
			t.Fatal("oversized refusal prevented a fitting unrelated flow")
		}
		f.releaseTcp(f.takeTcp(), 41001, nil)
	})
}

// This path deliberately uses a bare claim, not operation.complete's notify,
// so the capacity edge must come from actual receipt charge removal.
func TestReceiveReceiptAdmissionBareCompletionWakes(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	receipt, accepted := q.append(testReceiveDeliveryItem(0), true)
	if !accepted {
		t.Fatal("initial receipt was not admitted")
	}
	claim := receipt.hold()
	receipt.seal()
	select {
	case <-q.wake:
		t.Fatal("uncompleted receipt unexpectedly published capacity")
	default:
	}
	claim.complete(true)
	select {
	case <-q.wake:
	default:
		t.Fatal("bare receipt completion returned capacity without waking its receiver")
	}
	if q.pending() || q.bytes != 0 {
		t.Fatal("completed receipt retained its capacity charge")
	}
}

// A terminal earlier callback must not dispatch a later Nack from the same
// decoded batch. No callback return is reclassified as retryable pressure.
func TestReceiveReceiptAdmissionFatalBatchStopsLaterCallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, ctx := testReceiveDeliveryLedger(t, 4)
	q.maxBytes = 64 * 1024
	q.sequence.client = &Client{}
	q.sequence.client.reliableProviderIngress.Store(true)
	q.sequence.deliveryQueue = q
	first := testReceiveDeliveryItem(0)
	MessagePoolReturn(first.frames[0].MessageBytes)
	packet := MessagePoolCopy(receiptAdmissionPacket(41000, tcpFlagAck, 101, []byte("fatal synthetic claim")))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		MessagePoolReturn(packet)
		t.Fatal(err)
	}
	first.frames = []*protocol.Frame{frame}
	first.receiveCallback = func(_ TransferPath, _ []*protocol.Frame, peer Peer) {
		claim := peer.deliveryReceipt().hold()
		if claim == nil {
			t.Fatal("required callback did not receive its claim")
		}
		claim.complete(false)
	}
	later := testReceiveDeliveryItem(1)
	later.ack = false
	called := false
	later.receiveCallback = func(TransferPath, []*protocol.Frame, Peer) { called = true }
	q.sequence.flushDeliveryReceipts([]*receiveItem{first, later}, append([]*protocol.Frame{frame}, later.frames...), Peer{})
	if ctx.Err() == nil || called || first.frames != nil || later.frames != nil || q.pending() || q.sequence.ackWindow.Pending() {
		t.Fatal("fatal receipt continued later batch dispatch/ACK or retained packet roots")
	}
}

// Exact pre-admission memory must be returned on the existing fatal contract
// path, before ReceivePack's caller returns its packet roots.
func TestReceiveReceiptAdmissionContractFailureReturnsReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		f.offer(f.sequenceId, 0, 0, NewId(), 41000, tcpFlagAck, []byte("invalid synthetic contract"), func(pack *ReceivePack) {
			pack.Pack.ContractFrame = &protocol.Frame{MessageBytes: []byte{0x80}}
		})
		f.client.receiveBuffer.mutex.Lock()
		rejected := len(f.client.receiveBuffer.rejectedReceiveSequenceIds)
		f.client.receiveBuffer.mutex.Unlock()
		if f.receiveBudget.UsedByteCount() != 0 || f.packBudget.UsedByteCount() != 0 || f.delivered(41000) != 0 || len(f.acks) != 0 {
			t.Fatal("fatal contract rejection retained its pre-admission reservation or emitted delivery/ACK")
		}
		if rejected != 1 {
			t.Fatal("malformed contract did not exercise the actual terminal verification path")
		}
	})
}

// The real provider constructor is invoked at the first dispatch boundary,
// after two complete Packs were queued behind the actual worker-start gate.
// Neither the callback list nor the provider flag is modified by the test.
func TestReceiveReceiptAdmissionLateProviderRegistration(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		start := make(chan struct{})
		releaseStart := sync.OnceFunc(func() { close(start) })
		var f *receiptAdmissionFixture
		registered := false
		f = newReceiptAdmissionFixtureWithProvider(t, 2, false, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 10 * 1024
			settings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) { <-start }
			settings.ProgressObserver = func(event TransferProgressEvent) {
				if event.Stage != "deliver_begin" || registered {
					return
				}
				registered = true
				providerSettings := DefaultRemoteUserNatProviderSettings()
				providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
				f.provider = NewRemoteUserNatProvider(f.client, f.nat, providerSettings)
			}
		})
		t.Cleanup(releaseStart)
		firstId, secondId := NewId(), NewId()
		payload := bytes.Repeat([]byte("p"), 1000)
		f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
		f.offer(f.sequenceId, 0, 1, secondId, 41000, tcpFlagAck, payload)
		releaseStart()
		synctest.Wait()
		f.collectAcks()
		sequence := f.sequence(f.sequenceId, 0)
		if !registered || f.provider == nil || !f.client.reliableProviderIngress.Load() {
			t.Fatal("actual provider constructor activation did not occur")
		}
		if sequence == nil || sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 || f.delivered(41000) != 1 ||
			len(f.acks) != 0 || len(sequence.deliveryQueue.items) != 1 || sequence.deliveryQueue.items[0].pending != 1 {
			t.Fatal("late provider activation escaped pre-admission or ACKed an unsecured callback claim")
		}
		f.releaseTcp(f.firstTcp, 41000, nil)
		f.offer(f.sequenceId, 0, 1, secondId, 41000, tcpFlagAck, payload)
		f.releaseTcp(f.firstTcp, 41000, payload)
		if f.delivered(41000) != 2 || !f.hasAck(f.sequenceId, secondId, false) {
			t.Fatal("late provider failed to retain and secure the exact retry")
		}
		f.releaseTcp(f.firstTcp, 41000, payload)
	})
}

// Mirrors the public AddReceiveCallback-before-flag publication window with
// an explicit claim. There is no test write to reliableProviderIngress.
func TestReceiveReceiptAdmissionOptionalClaimBeforeProviderFlag(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixtureWithProvider(t, 1, false)
		var claim *receiveDeliveryClaim
		remove := f.client.AddReceiveCallback(func(_ TransferPath, _ []*protocol.Frame, peer Peer) {
			if receipt := peer.deliveryReceipt(); receipt != nil {
				claim = receipt.hold()
			}
		})
		t.Cleanup(func() {
			remove()
			if claim != nil {
				claim.complete(false)
			}
		})
		messageId := NewId()
		f.offer(f.sequenceId, 0, 0, messageId, 41000, tcpFlagAck, []byte("optional publication-window claim"))
		if f.client.reliableProviderIngress.Load() || claim == nil || len(f.acks) != 0 {
			t.Fatal("published TCP callback lacked an optional receipt before the provider flag")
		}
		claim.complete(true)
		synctest.Wait()
		f.collectAcks()
		if !f.hasAck(f.sequenceId, messageId, false) || f.receiveBudget.UsedByteCount() != 0 {
			t.Fatal("secured optional callback did not publish its ACK and return memory")
		}
	})
}

// With no provider and no optional claimant, ordinary callback completion
// remains successful; merely supplying a receipt does not require a claim.
func TestReceiveReceiptAdmissionNoProviderKeepsOrdinaryCallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixtureWithProvider(t, 1, false)
		messageId := NewId()
		f.offer(f.sequenceId, 0, 0, messageId, 41000, tcpFlagAck, []byte("ordinary synthetic consumer"))
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.ctx.Err() != nil || f.client.reliableProviderIngress.Load() ||
			f.delivered(41000) != 1 || !f.hasAck(f.sequenceId, messageId, false) || f.receiveBudget.UsedByteCount() != 0 {
			t.Fatal("optional receipt changed the ordinary no-provider callback contract")
		}
	})
}

// These independent packet classes must not acquire TCP-provider prepayment
// or receipts. No provider means the UDP control cannot open a real socket.
func TestReceiveReceiptAdmissionUnrelatedPacketClassesStayOrdinary(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, kind := range []string{"client-tcp", "provider-udp", "provider-nack"} {
			f := newReceiptAdmissionFixtureWithProvider(t, 2, false, func(settings *ReceiveBufferSettings) {
				settings.AllowLegacyNack = true
			})
			sequenceId, messageId := NewId(), NewId()
			f.offer(sequenceId, 0, 0, messageId, 41000, tcpFlagAck, []byte("ordinary class"), func(pack *ReceivePack) {
				switch kind {
				case "client-tcp":
					pack.Pack.Frames[0].MessageType = protocol.MessageType_IpIpPacketFromProvider
				case "provider-udp":
					MessagePoolReturn(pack.Pack.Frames[0].MessageBytes)
					packet := poolBalanceUdp4Packet(net.ParseIP("192.0.2.1"), 41000, net.ParseIP("198.51.100.2"), 443, []byte("synthetic UDP"))
					f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(packet))
					frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
					if err != nil {
						MessagePoolReturn(packet)
						t.Fatal(err)
					}
					pack.Pack.Frames = []*protocol.Frame{frame}
					pack.MessageByteCount = ByteCount(len(packet))
				case "provider-nack":
					pack.Pack.Nack = true
				}
			})
			sequence := f.sequence(sequenceId, 0)
			if sequence == nil || sequence.ctx.Err() != nil || sequence.deliveryQueue != nil || f.receiveBudget.UsedByteCount() != 0 {
				t.Errorf("%s acquired TCP receipt/prepayment state", kind)
			}
			if f.hasAck(sequenceId, messageId, false) != (kind != "provider-nack") {
				t.Errorf("%s changed its ordinary ACK contract", kind)
			}
		}
	})
}

// Supplies an ordinary protobuf message, not a TCP/provider-classified frame.
// Pooled roots keep a final-share witness. Larger protobuf roots are not
// pooled: observe their actual frame and joined receive owners instead.
func (self *receiptAdmissionFixture) offerGeneric(number uint64, messageId Id, size int) {
	self.t.Helper()
	self.offer(self.sequenceId, 0, number, messageId, 41000, tcpFlagAck, nil, func(pack *ReceivePack) {
		content := bytes.Repeat([]byte("g"), size-3)
		frame, err := ToFrame(&protocol.SimpleMessage{Content: string(content)}, DefaultProtocolVersion)
		if err != nil {
			self.t.Fatal(err)
		}
		if len(frame.MessageBytes) != size {
			MessagePoolReturn(frame.MessageBytes)
			self.t.Fatal("synthetic protobuf size did not match the constructor boundary")
		}
		MessagePoolReturn(pack.Pack.Frames[0].MessageBytes)
		if pooled, _ := MessagePoolCheck(frame.MessageBytes); pooled {
			self.witnesses = append(self.witnesses, MessagePoolShareReadOnly(frame.MessageBytes))
		}
		self.genericFrames = append(self.genericFrames, frame)
		pack.Pack.Frames = []*protocol.Frame{frame}
		pack.MessageByteCount = ByteCount(size)
	})
	sequence := self.sequence(self.sequenceId, 0)
	if sequence == nil {
		self.t.Fatal("generic fixture lost its actual receive owner before closure evidence")
	}
	self.genericSequences[sequence] = struct{}{}
}

// Observe ordinary callback completion independently from IP/provider work.
func (self *receiptAdmissionFixture) observeGeneric() *atomic.Int64 {
	count := &atomic.Int64{}
	remove := self.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage {
				continue
			}
			message := &protocol.SimpleMessage{}
			if err := proto.Unmarshal(frame.MessageBytes, message); err != nil || len(message.Content) == 0 {
				self.t.Errorf("ordinary synthetic protobuf dispatch: %v", err)
			}
			if peer.deliveryReceipt() != nil {
				self.t.Error("ordinary callback acquired a TCP-provider asynchronous claim")
			}
			count.Add(1)
		}
	})
	self.t.Cleanup(remove)
	return count
}

// A logical reorder window is not a maximum legal message size. Preserve
// the legacy empty-hold exception both just below and just above that window.
func TestReceiveReceiptAdmissionOrdinaryOversizeKeepsEmptyHold(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, size := range []int{1023, 1025} {
			f := newReceiptAdmissionFixtureWithProvider(t, 2, false, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = 1024
				settings.ReceiveQueueBudget = nil
				settings.ReceiveQueueRetainedByteAccounting = false
				settings.ReceiveHoldPolicy = ReceiveHoldRefuse
			})
			delivered := f.observeGeneric()
			heldId, headId := NewId(), NewId()
			f.offerGeneric(1, heldId, size)
			sequence := f.sequence(f.sequenceId, 0)
			if sequence == nil || sequence.ctx.Err() != nil || sequence.receiveQueue.lifetimeBudget ||
				sequence.receiveQueue.Len() != 1 || sequence.deliveryQueue != nil || delivered.Load() != 0 ||
				!f.hasAck(f.sequenceId, heldId, true) || f.hasAck(f.sequenceId, heldId, false) {
				t.Fatalf("ordinary %d-byte message lost the empty-hold compatibility owner", size)
			}
			f.offerGeneric(0, headId, 200)
			if sequence.nextSequenceNumber != 2 || sequence.receiveQueue.Len() != 0 ||
				delivered.Load() != 2 || !f.hasAck(f.sequenceId, heldId, false) || sequence.deliveryQueue != nil {
				t.Fatalf("ordinary %d-byte retained message did not deliver after its predecessor", size)
			}
			f.offerGeneric(1, heldId, size)
			if delivered.Load() != 2 {
				t.Fatal("ordinary compatibility duplicate dispatched twice")
			}
		}
	})
}

// A generic owner can precede provider activation. Its existing selective
// custody must survive a later TCP receipt, then drain on capacity alone.
func TestReceiveReceiptAdmissionLateTcpKeepsOversizeGenericOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, retained := range []bool{false, true} {
			size := 12 * 1024
			if retained {
				// Payload fits the logical window; pooled root plus receipt
				// exceeds it. The actual retained reservation already exists.
				size = 10*1024 - 1
			}
			f := newReceiptAdmissionFixtureWithProvider(t, 1, false, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = 10 * 1024
				settings.ReceiveQueueRetainedByteAccounting = retained
				settings.ReceiveHoldPolicy = ReceiveHoldRefuse
				if !retained {
					settings.ReceiveQueueBudget = nil
				}
			})
			delivered := f.observeGeneric()
			heldId, firstId := NewId(), NewId()
			f.offerGeneric(1, heldId, size)
			sequence := f.sequence(f.sequenceId, 0)
			if sequence == nil {
				t.Fatal("actual ordinary held sequence was not constructed")
			}
			held := sequence.receiveQueue.GetByMessageId(heldId)
			if held == nil || !held.committed || !f.hasAck(f.sequenceId, heldId, true) ||
				(held.memoryBudget != nil) != retained || sequence.deliveryQueue != nil {
				t.Fatal("ordinary selective custody did not predate provider activation")
			}
			providerSettings := DefaultRemoteUserNatProviderSettings()
			providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
			f.provider = NewRemoteUserNatProvider(f.client, f.nat, providerSettings)
			sawOneOversize := false
			remove := f.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
				for _, frame := range frames {
					if frame.MessageType != protocol.MessageType_TestSimpleMessage {
						continue
					}
					q := sequence.deliveryQueue
					if q == nil {
						t.Error("late TCP did not install its actual receipt ledger")
						continue
					}
					q.mutex.Lock()
					sawOneOversize = len(q.items) == 1 && q.maxCount == 1 && q.bytes > q.maxBytes &&
						q.controlCount == 0 && q.items[0].pending == 0 && !q.items[0].required
					q.mutex.Unlock()
					if peer.deliveryReceipt() != nil || retained &&
						(held.memoryBudget != f.receiveBudget || f.receiveBudget.UsedByteCount() < held.QueueByteCount()) {
						t.Error("ordinary oversize bypassed its synchronous or exact-reservation boundary")
					}
				}
			})
			t.Cleanup(remove)
			payload := []byte("TCP predecessor after ordinary selective ownership")
			f.offer(f.sequenceId, 0, 0, firstId, 41000, tcpFlagAck, payload)
			if sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 ||
				sequence.receiveQueue.GetByMessageId(heldId) != held || delivered.Load() != 0 ||
				f.hasAck(f.sequenceId, heldId, false) {
				t.Fatal("late TCP activation removed or cumulatively promoted the generic selective owner")
			}
			f.releaseTcp(f.firstTcp, 41000, nil)
			if sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 2 || sequence.receiveQueue.Len() != 0 ||
				delivered.Load() != 1 || !sawOneOversize || !f.hasAck(f.sequenceId, heldId, false) {
				t.Fatal("capacity-only wake failed to deliver exactly one retained generic oversize owner")
			}
			f.releaseTcp(f.firstTcp, 41000, payload)
		}
	})
}

// Even the synchronous oversize exception must acquire its actual shared
// memory. A different owner cannot lose its bytes to an empty-ledger probe.
func TestReceiveReceiptAdmissionOrdinaryOversizeHonorsSharedBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 10 * 1024
		})
		delivered := f.observeGeneric()
		firstId, genericId := NewId(), NewId()
		f.offer(f.sequenceId, 0, 0, firstId, 41001, tcpFlagSyn, nil)
		f.releaseTcp(f.takeTcp(), 41001, nil)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil || sequence.deliveryQueue.pending() {
			t.Fatal("completed TCP receipt did not leave an empty actual ledger")
		}
		reserved := f.receiveBudget.Available()
		if !f.receiveBudget.TryReserve(reserved) {
			t.Fatal("independent shared owner could not acquire the remaining budget")
		}
		t.Cleanup(func() {
			if reserved != 0 {
				f.receiveBudget.Release(reserved)
			}
		})
		f.offerGeneric(1, genericId, 10*1024-1)
		if sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 || delivered.Load() != 0 ||
			f.hasAck(f.sequenceId, genericId, false) || f.receiveBudget.UsedByteCount() != reserved {
			t.Fatal("synchronous oversize admission escaped the competing root reservation")
		}
		f.receiveBudget.Release(reserved)
		reserved = 0
		f.offerGeneric(1, genericId, 10*1024-1)
		if sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 2 || delivered.Load() != 1 ||
			!f.hasAck(f.sequenceId, genericId, false) || f.receiveBudget.UsedByteCount() != 0 {
			t.Fatal("exact generic retry did not complete after shared owner release")
		}
	})
}

// The actual H1 established group limit is three complete tunnel-MTU packets.
// All three use one existing TCP flow and its one-slot final admission queue.
func (self *receiptAdmissionFixture) offerMaximumTcpGroup(messageId Id) []byte {
	self.t.Helper()
	payload := bytes.Repeat([]byte("m"), DefaultMtu-Ipv4HeaderSizeWithoutExtensions-TcpHeaderSizeWithoutExtensions)
	self.offer(self.sequenceId, 0, 0, messageId, 41000, tcpFlagAck, payload, func(pack *ReceivePack) {
		for index := 1; index < 3; index++ {
			packet := MessagePoolCopy(receiptAdmissionPacket(41000, tcpFlagAck, uint32(101+index*len(payload)), payload))
			self.witnesses = append(self.witnesses, MessagePoolShareReadOnly(packet))
			frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
			if err != nil {
				MessagePoolReturn(packet)
				self.t.Fatal(err)
			}
			pack.Pack.Frames = append(pack.Pack.Frames, frame)
			pack.MessageByteCount += ByteCount(len(packet))
		}
		if pack.MessageByteCount != sendPackH1EstablishedMaxMessageByteCount {
			self.t.Fatal("synthetic TCP group does not reach the actual established H1 message bound")
		}
	})
	return payload
}

// Measure the actual owned charge without mutating it, then construct an
// independent receiver whose ledger ceiling equals exactly that charge.
func TestReceiveReceiptAdmissionMaximumTcpGroupFitsExactCap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		probe := newReceiptAdmissionFixture(t, 1)
		probeId := NewId()
		payload := probe.offerMaximumTcpGroup(probeId)
		sequence := probe.sequence(probe.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil || len(sequence.deliveryQueue.items) != 1 {
			t.Fatal("maximum TCP group did not construct one real pending receipt")
		}
		charge := sequence.deliveryQueue.bytes
		if charge <= sendPackH1EstablishedMaxMessageByteCount || charge > 64*1024 ||
			charge > DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount {
			t.Fatal("actual maximum TCP group does not fit the normal/default receipt profile")
		}
		probe.releaseTcp(probe.firstTcp, 41000, nil)
		for index := 0; index < 3; index++ {
			probe.releaseTcp(probe.firstTcp, 41000, payload)
		}
		f := newReceiptAdmissionFixture(t, 1, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = charge
		})
		messageId := NewId()
		payload = f.offerMaximumTcpGroup(messageId)
		sequence = f.sequence(f.sequenceId, 0)
		if sequence == nil || sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 ||
			sequence.deliveryQueue.bytes != charge || f.delivered(41000) != 3 || len(f.acks) != 0 {
			t.Fatal("valid maximum TCP group was refused at an exact-fit constructor boundary")
		}
		f.releaseTcp(f.firstTcp, 41000, nil)
		for index := 0; index < 3; index++ {
			f.releaseTcp(f.firstTcp, 41000, payload)
		}
		if !f.hasAck(f.sequenceId, messageId, false) || sequence.deliveryQueue.pending() ||
			f.receiveBudget.UsedByteCount() != 0 {
			t.Fatal("exact-fit maximum TCP group failed to secure all three final owners")
		}
	})
}

// The real provider publishes identity on first ingress even while the incoming
// data receipt is pending. Reading it is not a peer ACK: its actual send owner
// retains the exact message until fixture cancellation joins and drains it.
func TestReceiveReceiptGatewayActualDiagnosticsRetainSenderOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixture(t, 1)
		f.offer(f.sequenceId, 0, 0, NewId(), 41000, tcpFlagAck, []byte("synthetic pending ingress"))
		if f.gateway.diagnosticPackets != 1 || len(f.acks) != 0 {
			t.Fatal("initial provider diagnostics were absent or mistaken for incoming delivery ACK")
		}
		receive := f.sequence(f.sequenceId, 0)
		if receive == nil || receive.deliveryQueue == nil || !receive.deliveryQueue.pending() {
			t.Fatal("actual pending receive premise was not established")
		}
		f.client.sendBuffer.mutex.Lock()
		sender := f.client.sendBuffer.sendSequencesBySequenceId[f.gateway.diagnosticSequenceId]
		f.client.sendBuffer.mutex.Unlock()
		if sender == nil || sender.ctx.Err() != nil || len(sender.sendItems) != 1 ||
			sender.sendItems[0].messageId != f.gateway.diagnosticMessageId ||
			!sender.sendItems[0].expectsAck || sender.resendQueue.Len() != 1 {
			t.Fatal("decoded diagnostic lost its actual unacknowledged sender owner")
		}
		f.cancel()
		f.provider.Close()
		if err := f.nat.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		select {
		case <-sender.done:
		default:
			t.Fatal("diagnostic sender was not joined by actual Client cleanup")
		}
		if len(sender.sendItems) != 0 || sender.resendQueue.Len() != 0 {
			t.Fatal("joined diagnostic sender retained its unacknowledged wire owner")
		}
	})
}
