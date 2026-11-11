// A real signed, Required-encryption session carries repeated provider ACK
// gaps. The actual TCP return ledger retains multiple chunks each round; a
// parked socket worker isolates deterministic ACK custody from throughput.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Wire copies stay in heldFrames until transferred to a client or teardown.
// Scans contain copied scalar facts from a genuinely authenticated inner body.
type privateProviderEncryptedAckFrame struct {
	*windowRoundFrame
	scan      receiveMemoryPackScan
	encrypted bool
}

type privateProviderEncryptedAckFixture struct {
	*windowRoundFixture
	root, parent, rx, recovery, natBudget *TransferMemoryBudget
	sequence                              *ReceiveSequence
	nat                                   *LocalUserNat
	provider                              *RemoteUserNatProvider
	tcp                                   *TcpSequence
	path                                  *IpPath
	gate                                  chan struct{}
	entered                               chan *providerReliablePacket
	ackCounts                             map[uint64]int
	childHeld                             ByteCount
	closed                                bool
}

// Both peers negotiate TLS and verify identity against genuinely MAC-verified
// contract keys. Every memory policy is installed before either client starts.
func newPrivateProviderEncryptedAckFixture(t *testing.T) *privateProviderEncryptedAckFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	root := NewTransferMemoryBudget(16 * 1024 * 1024)
	parent := NewTransferMemoryBudgetWithParent(8*1024*1024, root)
	rx := NewTransferMemoryBudgetWithParent(512*1024, parent)
	recovery := NewTransferMemoryBudgetWithParent(512*1024, parent)
	f := &privateProviderEncryptedAckFixture{root: root, parent: parent, rx: rx, recovery: recovery,
		natBudget: NewTransferMemoryBudgetWithParent(4*1024*1024, root),
		path: &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 47011,
			DestinationIp: net.IPv4(203, 0, 113, 10).To4(), DestinationPort: 443},
		entered: make(chan *providerReliablePacket, 64), ackCounts: map[uint64]int{},
		windowRoundFixture: &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
			senderOut: make(Route, 256), senderIn: make(Route, 256), receiverOut: make(Route, 256), receiverIn: make(Route, 256)}}
	t.Cleanup(f.close)
	settings := func(receiver bool) *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = NewNoopLogger()
		value.ControlPingTimeout = 0
		value.EncryptionSettings.Mode = EncryptionModeRequired
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024 * 1024
		value.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = 256 * 1024 * 1024
		value.ContractManagerSettings.StandardContractTransferByteCount = 256 * 1024 * 1024
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.SequenceBufferSize = 0
		value.SendBufferSettings.ProtocolVersion = 2
		value.SendBufferSettings.ResendQueueMaxByteCount = 4 * 1024 * 1024
		value.SendBufferSettings.CompactContractHead = true
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveQueueBudget = nil
		if receiver {
			receive := value.ReceiveBufferSettings
			receive.ReceiveQueueBudget, receive.ReceiveHeadRecoveryBudget = rx, recovery
			receive.ReceiveQueueRetainedByteAccounting = true
			receive.ReceiveQueueMinByteCount = 0
			receive.ReceiveQueueMaxByteCount = 64 * 1024
			receive.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			receive.AdvertiseReceiveWindow = false
			receive.PackQueueBudget = nil
			receive.PackQueueRetainedByteAccounting = false
		}
		return value
	}
	senderId, receiverId := NewId(), NewId()
	forward := &expirationLifecycleAuthority{ctx: ctx, sourceId: senderId, peer: func() *Client { return f.receiver },
		initialDeadline: time.Now().Add(time.Hour), grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	reverse := &expirationLifecycleAuthority{ctx: ctx, sourceId: receiverId, peer: func() *Client { return f.sender },
		initialDeadline: time.Now().Add(time.Hour), grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	f.sender = NewClient(ctx, senderId, forward, settings(false))
	f.receiver = NewClient(ctx, receiverId, reverse, settings(true))
	f.sender.RouteManager().UpdateTransportWithProperties(newPrivateH1PeerGateway(receiverId, TransportTypeH1),
		[]Route{f.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	f.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.senderIn})
	f.receiver.RouteManager().UpdateTransport(newPrivateH1PeerGateway(senderId, TransportTypeUnknown), []Route{f.receiverOut})
	f.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.receiverIn})
	for _, client := range []*Client{f.sender, f.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage || frame.MessageType == protocol.MessageType_IpIpPacketToProvider {
				f.deliveredCount++
			}
		}
	})
	warmAck, admitted := make(chan struct{}, 1), make(chan bool, 1)
	warm := requiredGateFrame(t, "signed encrypted provider warm-up")
	go func() {
		accepted := f.sender.SendWithTimeout(warm, receiverId, func(err error) {
			if err == nil {
				f.ackedCount++
				warmAck <- struct{}{}
			}
		}, -1)
		if !accepted {
			MessagePoolReturn(warm.MessageBytes)
		}
		admitted <- accepted
	}()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
warmLoop:
	for {
		select {
		case wire := <-f.senderOut:
			f.receiverIn <- wire
		case wire := <-f.receiverOut:
			f.senderIn <- wire
		case <-warmAck:
			break warmLoop
		case accepted := <-admitted:
			if !accepted {
				t.Fatal("required encrypted warm-up send was refused")
			}
			admitted = nil
		case <-deadline.C:
			t.Fatal("real signed Required-encryption handshake failed to deliver")
		}
	}
	if admitted != nil && !<-admitted {
		t.Fatal("required encrypted warm-up was not admitted")
	}
	synctest.Wait()
	for len(f.senderOut) != 0 || len(f.receiverOut) != 0 {
		for len(f.senderOut) != 0 {
			f.receiverIn <- <-f.senderOut
		}
		for len(f.receiverOut) != 0 {
			f.senderIn <- <-f.receiverOut
		}
		synctest.Wait()
	}
	f.receiver.receiveBuffer.mutex.Lock()
	for key, sequence := range f.receiver.receiveBuffer.receiveSequences {
		if key.Source.SourceId == senderId && key.EncryptionRole == sequenceTlsRoleServer && !key.EncryptionCompanion &&
			key.LogicalLane == 0 && !sequence.transferKey.CompanionContract {
			f.sequence = sequence
		}
	}
	f.receiver.receiveBuffer.mutex.Unlock()
	if f.sequence == nil || f.sequence.session == nil || f.sequence.session.Cipher() == nil ||
		f.sequence.receiveContract == nil || !f.sequence.receiveContract.verifiedReceiveProofSet ||
		f.deliveredCount != 1 || f.ackedCount != 1 || rx.UsedByteCount() != 0 {
		t.Fatal("warm-up did not establish actual signed encrypted authority and drain RX")
	}
	f.nextNumber = f.sequence.nextSequenceNumber
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
	natSettings.Log, natSettings.MemoryBudget = NewNoopLogger(), f.natBudget
	natSettings.TcpBufferSettings.WriteTimeout = 0
	captured := make(chan *TcpSequence, 1)
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(tcp *TcpSequence) {
		captured <- tcp
		<-tcp.ctx.Done()
	}
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, context.Canceled
	}}
	f.nat = NewLocalUserNat(ctx, "signed encrypted ACK cycles", natSettings)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	f.provider = NewRemoteUserNatProvider(f.receiver, f.nat, providerSettings)
	processed := make(chan struct{}, 1)
	f.nat.afterSendPacketForTest = func() { processed <- struct{}{} }
	syn := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagSyn, 100, nil))
	if !f.nat.SendPacket(SourceId(senderId), protocol.ProvideMode_Network, syn, 0) {
		MessagePoolReturn(syn)
		t.Fatal("actual NAT could not initialize the held TCP flow")
	}
	f.tcp = <-captured
	<-processed
	synctest.Wait()
	f.nat.afterSendPacketForTest = nil
	f.tcp.receiveAckCondition()
	f.tcp.mutex.Lock()
	f.tcp.established, f.tcp.receiveWindowEndSet = true, true
	f.tcp.receiveSeqAck, f.tcp.receiveSeq = 500, 500
	f.tcp.receiveWindowSize, f.tcp.receiveWindowEnd = 65535, 66035
	f.tcp.mutex.Unlock()
	f.nat.beforeSendPacketForTest = func(packet *SendPacket) {
		if packet.reliable != nil {
			f.entered <- packet.reliable
			select {
			case <-f.gate:
			case <-ctx.Done():
			}
		}
	}
	return f
}

// Inspect using the actual established recipient cipher; the original sealed
// carrier remains untouched and is later consumed by the real receive pump.
func (self *privateProviderEncryptedAckFixture) capture(wire []byte, receiver *Client) *privateProviderEncryptedAckFrame {
	self.t.Helper()
	frame := &privateProviderEncryptedAckFrame{windowRoundFrame: &windowRoundFrame{bytes: wire}}
	self.heldFrames = append(self.heldFrames, frame.windowRoundFrame)
	var outer protocol.TransferFrame
	if err := ProtoUnmarshal(wire, &outer); err != nil {
		self.t.Fatal(err)
	}
	plain := wire
	if len(outer.EncryptedTransferFrame) != 0 {
		path, err := TransferPathFromProtobuf(outer.TransferPath)
		if err != nil {
			self.t.Fatal(err)
		}
		inner, _, _, err := receiver.unwrapFrame(path.SourceId, outer.GetSessionRole(), outer.SessionCompanion, outer.EncryptedTransferFrame)
		if err != nil {
			self.t.Fatalf("actual established recipient could not decrypt its carrier: %v", err)
		}
		defer MessagePoolReturn(inner)
		plain, frame.encrypted = inner, true
	}
	var decoded protocol.TransferFrame
	if err := ProtoUnmarshal(plain, &decoded); err != nil {
		self.t.Fatal(err)
	}
	frame.pack, frame.ack = decoded.Pack, decoded.Ack
	if frame.pack != nil {
		path, err := TransferPathFromProtobuf(decoded.TransferPath)
		if err != nil {
			self.t.Fatal(err)
		}
		var status receiveMemoryPackScanStatus
		frame.scan, status = scanReceiveMemoryPack(plain, path)
		if status != receiveMemoryPackScanReady || !frame.scan.loanSafe {
			self.t.Fatal("actual encrypted application body has no canonical owned scan")
		}
	}
	return frame
}

// Non-application control replies retain their normal ownership and route.
func (self *privateProviderEncryptedAckFixture) feedback() {
	self.t.Helper()
	synctest.Wait()
	for len(self.receiverOut) != 0 {
		frame := self.capture(<-self.receiverOut, self.sender)
		self.forward(frame.windowRoundFrame, self.senderIn)
	}
}

func (self *privateProviderEncryptedAckFixture) takeAckOriginal(number uint64) *privateProviderEncryptedAckFrame {
	self.t.Helper()
	synctest.Wait()
	for len(self.senderOut) != 0 {
		frame := self.capture(<-self.senderOut, self.receiver)
		if frame.pack != nil && receiveProviderHeadRecoveryFrames(frame.pack.Frames) {
			if frame.pack.SequenceNumber == number {
				if !frame.encrypted || RequireIdFromBytes(frame.pack.SequenceId) != self.sequence.sequenceId {
					self.t.Fatal("provider ACK original escaped actual encrypted source sequence")
				}
				return frame
			}
			continue
		}
		self.forward(frame.windowRoundFrame, self.receiverIn)
		self.feedback()
	}
	self.t.Fatal("actual source did not emit the requested encrypted ACK original")
	return nil
}

// The original sender owns retransmission timing and Head/proof promotion.
// Retain every unrelated copy, so only this exact retry can repair the gap.
func (self *privateProviderEncryptedAckFixture) retry(original *privateProviderEncryptedAckFrame) *privateProviderEncryptedAckFrame {
	self.t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case wire := <-self.senderOut:
			frame := self.capture(wire, self.receiver)
			if frame.pack != nil && receiveProviderHeadRecoveryFrames(frame.pack.Frames) {
				if bytes.Equal(frame.pack.MessageId, original.pack.MessageId) && bytes.Equal(frame.pack.SequenceId, original.pack.SequenceId) {
					if !frame.encrypted || !frame.pack.Head || frame.pack.ContractFrame == nil && len(frame.pack.ContractId) == 0 {
						self.t.Fatal("source retry did not retain encrypted headed contract authority")
					}
					return frame
				}
				continue
			}
			self.forward(frame.windowRoundFrame, self.receiverIn)
			self.feedback()
		case wire := <-self.receiverOut:
			frame := self.capture(wire, self.sender)
			self.forward(frame.windowRoundFrame, self.senderIn)
		case <-deadline.C:
			self.t.Fatal("actual sender failed to retry the missing encrypted head")
		}
	}
}

// Successful Send owns the pooled packet; refusal leaves it with this fixture.
func (self *privateProviderEncryptedAckFixture) writeAcks(count int, acknowledgement uint32) *privateProviderEncryptedAckFrame {
	self.t.Helper()
	frames := make([]*protocol.Frame, count)
	for index := range frames {
		packet := ipOosTcpPacketSequence(self.path, tcpFlagAck, 101, nil)
		segment := packet[Ipv4HeaderSizeWithoutExtensions:]
		binary.BigEndian.PutUint32(segment[8:12], acknowledgement)
		binary.BigEndian.PutUint16(segment[16:18], 0)
		binary.BigEndian.PutUint16(segment[16:18], ipPathTransportChecksum(self.path, ipProtocolNumberTcp, segment))
		frame, err := ipPacketToProviderFrame(MessagePoolCopy(packet), DefaultProtocolVersion)
		if err != nil || !frame.Raw || !reliableFrameControl(frame) {
			self.t.Fatal("actual provider ACK encoder rejected the synthetic TCP acknowledgement")
		}
		frames[index] = frame
	}
	number := self.nextNumber
	if !self.sender.SendMultiWithTimeout(frames, self.receiver.ClientId(), func(err error) {
		if err == nil {
			self.ackedCount++
			self.ackCounts[number]++
		}
	}, time.Second) {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		self.t.Fatal("actual encrypted source refused its ACK original")
	}
	self.nextNumber++
	return self.takeAckOriginal(number)
}

// Shutdown returns all route copies after joining the clients and the real NAT.
func (self *privateProviderEncryptedAckFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.cancel()
	if self.provider != nil {
		self.provider.Close()
	}
	if self.nat != nil {
		if err := self.nat.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	for _, client := range []*Client{self.sender, self.receiver} {
		if client != nil {
			if err := client.CloseAndWait(context.Background()); err != nil {
				self.t.Error(err)
			}
		}
	}
	for _, frame := range self.heldFrames {
		MessagePoolReturn(frame.bytes)
		frame.bytes = nil
	}
	for _, route := range []Route{self.senderOut, self.senderIn, self.receiverOut, self.receiverIn} {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	}
	if self.childHeld != 0 {
		self.recovery.Release(self.childHeld)
		self.childHeld = 0
	}
	for _, budget := range []*TransferMemoryBudget{self.root, self.parent, self.rx, self.recovery, self.natBudget} {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			self.t.Errorf("encrypted provider owner survived full join: %+v", stats)
		}
	}
}

// Repeated real return-ledger occupancy survives the first child-pressure
// refusal, then drains through an unchanged encrypted headed source retry.
func TestPrivateProviderEncryptedAckRecoveryRetiresSustainedChunks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPrivateProviderEncryptedAckFixture(t)
		rootTotal, parentTotal := f.root.TotalByteCount(), f.parent.TotalByteCount()
		start := uint32(500)
		for round := range 3 {
			f.gate = make(chan struct{})
			for len(f.entered) != 0 {
				<-f.entered
			}
			end := start + 4*1280
			f.tcp.mutex.Lock()
			f.tcp.receiveSeq, f.tcp.receiveWindowEnd = end, end+65535
			f.tcp.mutex.Unlock()
			for chunk := range 4 {
				if !f.tcp.retainReturnChunk(bytes.Repeat([]byte{byte(17 + round*4 + chunk)}, 1280), start+uint32(chunk*1280), false) {
					t.Fatal("actual TCP return ledger could not retain its sustained chunk")
				}
			}
			retained := f.tcp.returnByteCount
			if retained <= 0 || len(f.tcp.returnChunks)-f.tcp.returnHead != 4 || len(f.tcp.sendItems) != 1 {
				t.Fatal("sustained round did not retain four actual paid origin chunks")
			}
			first := f.nextNumber
			head := f.writeAcks(2, start+2*1280)
			tails := make([]*privateProviderEncryptedAckFrame, 10)
			for index := range tails {
				tails[index] = f.writeAcks(1, end)
			}
			cost := func(frame *privateProviderEncryptedAckFrame) ByteCount {
				probe := &receiveItem{transferItem: transferItem{queueByteCount: frame.scan.retainedByteCount}, ack: true, frames: frame.pack.Frames}
				if !f.sequence.prepareReceiveDeliveryCredit(probe) || !probe.deliveryPrepared {
					t.Fatal("encrypted inner ACK has no actual downstream ownership estimate")
				}
				return probe.QueueByteCount()
			}
			headFull, tailFull := cost(head), cost(tails[0])
			if head.scan.retainedByteCount > tailFull || tailFull >= headFull || f.rx.UsedByteCount() != 0 {
				t.Fatal("encrypted ACK cycle did not isolate C <= prefix < actual D")
			}
			repairMetadata, rxTarget := privateProviderRepairFixtureGeometry(t, f.sequence, tailFull, 10)
			f.rx.SetTotalByteCount(rxTarget)
			f.recovery.SetTotalByteCount(f.rx.TotalByteCount())
			owners := make(map[uint64]*receiveItem, len(tails))
			for _, tail := range tails {
				f.forward(tail.windowRoundFrame, f.receiverIn)
				item := f.sequence.receiveQueue.GetBySequenceNumber(tail.pack.SequenceNumber)
				if item == nil || !item.committed || item.QueueByteCount() != tailFull || !item.deliveryPrepared {
					t.Fatal("encrypted later ACK did not acquire actual committed custody")
				}
				owners[item.sequenceNumber] = item
				f.feedback()
			}
			privateProviderAssertRepairFixtureCustody(t, f.sequence, repairMetadata, 10)
			if f.rx.UsedByteCount() != f.rx.TotalByteCount() || f.sequence.holdPromise.reservedByteCount != tailFull ||
				f.sequence.nextSequenceNumber != first || f.tcp.returnByteCount != retained || len(f.entered) != 0 {
				t.Fatal("encrypted committed ACK tails did not preserve the exact missing head and return chunks")
			}
			f.childHeld = f.recovery.Available()
			if !f.recovery.TryReserve(f.childHeld) {
				t.Fatal("could not establish bounded independent child pressure")
			}
			before := f.receiver.ReceiveStats()
			f.forward(head.windowRoundFrame, f.receiverIn)
			if len(f.entered) != 0 || f.tcp.returnByteCount != retained || f.ackedCount != 1+round*11 ||
				f.sequence.ctx.Err() != nil || f.sequence.holdPromise.ingressLoanByteCount != 0 {
				t.Fatal("first encrypted child refusal changed ACK truth or retained chunk custody")
			}
			for number, owner := range owners {
				if f.sequence.receiveQueue.GetBySequenceNumber(number) != owner || !owner.committed {
					t.Fatal("encrypted child refusal evicted or replaced a committed original")
				}
			}
			f.recovery.Release(f.childHeld)
			f.childHeld = 0
			retry := f.retry(head)
			f.forward(retry.windowRoundFrame, f.receiverIn)
			if len(f.entered) == 0 || f.tcp.returnByteCount != retained || f.recovery.UsedByteCount() <= 0 ||
				f.ackedCount != 1+round*11 || f.sequence.ctx.Err() != nil {
				t.Fatal("released child capacity did not admit the unchanged encrypted headed retry before ACK")
			}
			close(f.gate)
			synctest.Wait()
			f.feedback()
			if f.ackedCount != 1+(round+1)*11 || f.deliveredCount != 1+(round+1)*12 ||
				f.sequence.nextSequenceNumber != first+11 || f.sequence.receiveQueue.Len() != 0 ||
				f.rx.UsedByteCount() != 0 || f.recovery.UsedByteCount() != 0 || f.tcp.returnByteCount != 0 ||
				f.tcp.returnHead != len(f.tcp.returnChunks) || len(f.tcp.sendItems) != 1 ||
				f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
				f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
				f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
				t.Fatalf("encrypted round %d failed cumulative/inner ownership closure: callbacks=%d delivered=%d next=%d rx=%d child=%d chunks=%d", round, f.ackedCount, f.deliveredCount, f.sequence.nextSequenceNumber, f.rx.UsedByteCount(), f.recovery.UsedByteCount(), f.tcp.returnByteCount)
			}
			for number := first; number < first+11; number++ {
				if f.ackCounts[number] != 1 {
					t.Fatal("encrypted headed repair skipped or duplicated an actual cumulative callback")
				}
			}
			t.Logf("[provider-encrypted-cycle] round=%d chunks=4 retained_bytes=%d head_c=%d head_d=%d prefix=%d actual_headed_retry=true signed_contract=true root_cap=%d provider_cap=%d", round, retained, head.scan.retainedByteCount, headFull, tailFull, rootTotal, parentTotal)
			start = end
		}
		f.close()
		t.Log("[provider-encrypted-complete] cycles=3 chunks=12 cumulative_callbacks=34 rx=0 child=0 nat=0 root=0 joined=true")
	})
}
