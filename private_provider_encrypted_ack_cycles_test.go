// A real signed, Required-encryption session carries repeated immediate ACK
// originals under full independent DATA pressure. Their encoded reliable
// lifetime and the actual TCP return ledger remain owned until real ACKs.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	ackOffers                             []*privateProviderAckOffer
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
	self.assertCapturedControlOriginal(frame)
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

func (self *privateProviderEncryptedAckFixture) takeAckOriginal(offer *privateProviderAckOffer) *privateProviderEncryptedAckFrame {
	self.t.Helper()
	synctest.Wait()
	for len(self.senderOut) != 0 {
		frame := self.capture(<-self.senderOut, self.receiver)
		if frame.pack != nil && receiveProviderHeadRecoveryFrames(frame.pack.Frames) {
			if self.boundAckOriginal(frame.pack) {
				continue
			}
			offer.bind(self, frame)
			return frame
		}
		self.forward(frame.windowRoundFrame, self.receiverIn)
		self.feedback()
	}
	source := privateProviderImmediateSource(self.t, self.windowRoundFixture, self.sequence.sequenceId.Bytes())
	self.t.Fatalf("actual source did not emit the requested encrypted ACK original: offered_frames=%d next=%d immediate=%d limit=%d lane=%d", len(offer.frames), source.nextSequenceNumber, source.immediateSendCount, source.immediateSendLimit, source.logicalLane)
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
func (self *privateProviderEncryptedAckFixture) enqueueAcks(count int, acknowledgement uint32) *privateProviderAckOffer {
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
	if !protocolOnlyFrames(frames) || !immediateIpAckFrames(frames) {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		self.t.Fatal("actual encrypted ACK batch does not qualify for the production protocol policy")
	}
	offer := newPrivateProviderAckOffer(frames)
	self.ackOffers = append(self.ackOffers, offer)
	if !self.sender.SendMultiWithTimeout(frames, self.receiver.ClientId(), func(err error) {
		offer.complete(self, err)
	}, time.Second, NoBudget(), DeliverImmediately()) {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		self.t.Fatal("actual encrypted source refused its ACK original")
	}
	return offer
}

// Capture is a separate transition: an accepted owner may remain queued while
// the unchanged count-one sender awaits the previous actual terminal ACK.
func (self *privateProviderEncryptedAckFixture) writeAcks(count int, acknowledgement uint32) *privateProviderEncryptedAckFrame {
	self.t.Helper()
	return self.takeAckOriginal(self.enqueueAcks(count, acknowledgement))
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
	for _, budget := range []*TransferMemoryBudget{self.root, self.parent, self.rx, self.recovery, self.natBudget} {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			self.t.Errorf("encrypted provider owner survived full join: %+v", stats)
		}
	}
}

// Repeated real return-ledger occupancy drains one actual immediate original
// at a time. A queued successor cannot serialize until the previous real ACK
// recycles the configured count one, even though both consume zero DATA bytes.
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
			pressure := newPrivateProviderProtocolDataPressure(t, f.rx)
			before, childBefore := f.receiver.ReceiveStats(), f.recovery.Stats()
			head := f.writeAcks(2, start+2*1280)
			first := head.pack.SequenceNumber
			privateProviderAssertImmediateCount(t, f.windowRoundFixture, head.pack, 1)
			f.forward(head.windowRoundFrame, f.receiverIn)
			privateProviderAssertProtocolReceiptCustody(t, f.sequence, 1)
			queued := f.enqueueAcks(1, end)
			f.assertQueuedAckOriginal(head.pack, queued)
			pressure.check(t)
			if len(f.entered) == 0 || f.tcp.returnByteCount != retained || f.ackedCount != 1+round*11 ||
				f.recovery.Stats() != childBefore || f.sequence.nextSequenceNumber != first {
				t.Fatal("encrypted count-one barrier lost actual source/NAT custody or spent DATA permission")
			}
			close(f.gate)
			synctest.Wait()
			f.tcp.mutex.Lock()
			partial := f.tcp.receiveSeqAck == start+2*1280 && f.tcp.returnByteCount > 0 && len(f.tcp.returnChunks)-f.tcp.returnHead == 2
			f.tcp.mutex.Unlock()
			if !partial {
				t.Fatal("the first actual partial inner ACK did not retire exactly two of four chunks")
			}
			privateProviderAssertOriginalPending(t, f.windowRoundFixture, head.pack, true, true)
			f.feedback()
			if f.ackCounts[first] != 1 || f.ackedCount != 2+round*11 {
				t.Fatal("encrypted immediate slot recycled without its actual head delivery ACK")
			}
			privateProviderAssertOriginalRetired(t, f.windowRoundFixture, head.pack)
			originals := []*privateProviderEncryptedAckFrame{head}
			for index := range 10 {
				var tail *privateProviderEncryptedAckFrame
				if index == 0 {
					tail = f.takeAckOriginal(queued)
				} else {
					tail = f.writeAcks(1, end)
				}
				originals = append(originals, tail)
				privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
				privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 1)
				f.forward(tail.windowRoundFrame, f.receiverIn)
				privateProviderAssertReturnFrontier(t, f.tcp, end)
				privateProviderAssertOriginalPending(t, f.windowRoundFixture, tail.pack, true, true)
				pressure.check(t)
				f.feedback()
				if f.ackCounts[tail.pack.SequenceNumber] != 1 || f.ackedCount != 3+round*11+index {
					t.Fatal("encrypted tail slot recycled without exactly one real terminal ACK")
				}
				privateProviderAssertOriginalRetired(t, f.windowRoundFixture, tail.pack)
				privateProviderAssertImmediateCount(t, f.windowRoundFixture, tail.pack, 0)
			}
			f.assertCompletedOriginals(originals)
			if f.ackedCount != 1+(round+1)*11 || f.deliveredCount != 1+(round+1)*12 ||
				f.sequence.receiveQueue.Len() != 0 ||
				f.rx.Stats() != pressure.before || f.recovery.Stats() != childBefore || len(f.tcp.sendItems) != 1 ||
				f.root.TotalByteCount() != rootTotal || f.parent.TotalByteCount() != parentTotal ||
				f.receiver.ReceiveStats().ReceiveDecodePressureDropCount != before.ReceiveDecodePressureDropCount ||
				f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
				f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
				t.Fatalf("encrypted round %d failed actual count-one ownership closure: callbacks=%d delivered=%d next=%d", round, f.ackedCount, f.deliveredCount, f.sequence.nextSequenceNumber)
			}
			for _, original := range originals {
				if f.ackCounts[original.pack.SequenceNumber] != 1 {
					t.Fatal("encrypted count-one train skipped or duplicated a terminal callback")
				}
				privateProviderAssertOriginalRetired(t, f.windowRoundFixture, original.pack)
			}
			pressure.close()
			t.Logf("[provider-encrypted-cycle] round=%d chunks=4 retained_bytes=%d protocol_originals=11 immediate_limit=1 protocol_data_bytes=0 root_cap=%d provider_cap=%d", round, retained, rootTotal, parentTotal)
			start = end
		}
		f.close()
		t.Log("[provider-encrypted-complete] cycles=3 chunks=12 cumulative_callbacks=34 rx=0 child=0 nat=0 root=0 joined=true")
	})
}

// These digests are copied before Send takes the pooled frame bytes. The offer
// has no predicted ordinal and cannot complete successfully before binding.
type privateProviderAckFrameStamp struct {
	messageType protocol.MessageType
	raw         bool
	digest      [32]byte
}

type privateProviderAckOffer struct {
	frames    []privateProviderAckFrameStamp
	pack      *protocol.Pack
	callbacks int
}

func newPrivateProviderAckOffer(frames []*protocol.Frame) *privateProviderAckOffer {
	offer := &privateProviderAckOffer{frames: make([]privateProviderAckFrameStamp, len(frames))}
	for index, frame := range frames {
		offer.frames[index] = privateProviderAckFrameStamp{messageType: frame.MessageType, raw: frame.Raw, digest: sha256.Sum256(frame.MessageBytes)}
	}
	return offer
}

// Capture precedes every delivery of this offered original to the receiver.
func (self *privateProviderAckOffer) bind(f *privateProviderEncryptedAckFixture, frame *privateProviderEncryptedAckFrame) {
	f.t.Helper()
	pack := frame.pack
	if self.pack != nil || self.callbacks != 0 || !frame.encrypted || pack == nil || pack.Nack ||
		!pack.DeliverImmediately || !protocolOnlyPack(pack) ||
		!bytes.Equal(pack.SequenceId, f.sequence.sequenceId.Bytes()) || len(pack.Frames) != len(self.frames) {
		f.t.Fatal("actual ACK original did not bind its unique offered frames and encrypted immediate policy")
	}
	for index, member := range pack.Frames {
		stamp := self.frames[index]
		if member.MessageType != stamp.messageType || member.Raw != stamp.raw || sha256.Sum256(member.MessageBytes) != stamp.digest {
			f.t.Fatal("actual encoded ACK original changed an offered frame")
		}
	}
	self.pack = pack
	f.nextNumber = pack.SequenceNumber + 1
}

// Error completion retains its ordinary cancellation semantics. Success must
// name the captured original, not an arithmetic guess made at raw admission.
func (self *privateProviderAckOffer) complete(f *privateProviderEncryptedAckFixture, err error) {
	if err != nil {
		return
	}
	self.callbacks++
	f.ackedCount++
	if self.pack == nil || self.callbacks != 1 {
		f.t.Error("ACK callback completed before actual identity binding or completed more than once")
		return
	}
	f.ackCounts[self.pack.SequenceNumber]++
}

// Previously captured application originals stay held when they are retried.
func (self *privateProviderEncryptedAckFixture) boundAckOriginal(pack *protocol.Pack) bool {
	for _, offer := range self.ackOffers {
		if offer.pack != nil && bytes.Equal(offer.pack.SequenceId, pack.SequenceId) && bytes.Equal(offer.pack.MessageId, pack.MessageId) {
			return true
		}
	}
	for _, previous := range self.heldFrames[:len(self.heldFrames)-1] {
		if previous.pack != nil && bytes.Equal(previous.pack.SequenceId, pack.SequenceId) && bytes.Equal(previous.pack.MessageId, pack.MessageId) {
			return true
		}
	}
	return false
}

// This is an ordered reliable session control, never an immediate IP receipt.
// Authentication and generation handling remain the real receiver's work.
func privateProviderEncryptedControl(pack *protocol.Pack) bool {
	if pack == nil || pack.Nack || pack.DeliverImmediately || pack.ContractAhead || len(pack.Frames) != 1 {
		return false
	}
	frame := pack.Frames[0]
	if frame == nil || frame.Raw || frame.MessageType != protocol.MessageType_TransferEncryptedControl {
		return false
	}
	var control protocol.EncryptedControl
	if ProtoUnmarshal(frame.MessageBytes, &control) != nil {
		return false
	}
	if _, valid := sequenceTlsRoleFromProtobuf(control.SessionRole); !valid {
		return false
	}
	if len(control.EpochId) != 0 {
		if _, err := IdFromBytes(control.EpochId); err != nil {
			return false
		}
	}
	switch control.ControlType {
	case protocol.EncryptedControlType_EncryptedControlHandshake,
		protocol.EncryptedControlType_EncryptedControlIdentityProof,
		protocol.EncryptedControlType_EncryptedControlUnknownWrapNack:
		return true
	default:
		return false
	}
}

// Its first real wire cannot have received feedback: it must still own the
// exact unwrapped reliable encoding, with no data debit or immediate slot.
func (self *privateProviderEncryptedAckFixture) assertCapturedControlOriginal(frame *privateProviderEncryptedAckFrame) {
	pack := frame.pack
	if self.sequence == nil || !privateProviderEncryptedControl(pack) ||
		!bytes.Equal(pack.SequenceId, self.sequence.sequenceId.Bytes()) || self.boundAckOriginal(pack) {
		return
	}
	privateProviderAssertOriginalPending(self.t, self.windowRoundFixture, pack, true, false)
	source := privateProviderImmediateSource(self.t, self.windowRoundFixture, pack.SequenceId)
	item := source.resendQueue.GetByMessageId(RequireIdFromBytes(pack.MessageId))
	var control protocol.EncryptedControl
	if err := ProtoUnmarshal(pack.Frames[0].MessageBytes, &control); err != nil || frame.encrypted || !item.forceUnwrapped ||
		control.SessionRole != self.sequence.encryptionRole.complement().toProtobuf() || control.Companion != self.sequence.encryptionCompanion {
		self.t.Fatal("actual control original lost its source-owned unwrapped session scope")
	}
	self.t.Logf("[provider-source-control-pending] ordinal=%d kind=%d subtype=%d payload_bytes=%d no_data_debit=true immediate=false encoded_owner=true", pack.SequenceNumber, pack.Frames[0].MessageType, control.ControlType, len(control.Payload))
}

// Captured Pack projections are ordinary copies. Forwarded wire shares still
// belong to the real receive pump; held recovery copies return only at cleanup.
func (self *privateProviderEncryptedAckFixture) sourceOriginals(first, next uint64) map[uint64]*protocol.Pack {
	self.t.Helper()
	originals := map[uint64]*protocol.Pack{}
	messageNumbers := map[Id]uint64{}
	for _, frame := range self.heldFrames {
		pack := frame.pack
		if pack == nil || !bytes.Equal(pack.SequenceId, self.sequence.sequenceId.Bytes()) || pack.SequenceNumber < first {
			continue
		}
		if pack.Nack || pack.SequenceNumber >= next {
			self.t.Fatal("captured original escaped the actual reliable source frontier")
		}
		if previous := originals[pack.SequenceNumber]; previous != nil {
			if !bytes.Equal(previous.MessageId, pack.MessageId) || previous.DeliverImmediately != pack.DeliverImmediately ||
				len(previous.Frames) != len(pack.Frames) {
				self.t.Fatal("actual source reused an ordinal for a different original")
			}
			for index, member := range pack.Frames {
				old := previous.Frames[index]
				if member.MessageType != old.MessageType || member.Raw != old.Raw || !bytes.Equal(member.MessageBytes, old.MessageBytes) {
					self.t.Fatal("actual source retry changed its original application or control frames")
				}
			}
			continue
		}
		id := RequireIdFromBytes(pack.MessageId)
		if number, exists := messageNumbers[id]; exists && number != pack.SequenceNumber {
			self.t.Fatal("actual source reused a reliable message identity at a different ordinal")
		}
		messageNumbers[id] = pack.SequenceNumber
		originals[pack.SequenceNumber] = pack
	}
	return originals
}

// Drain only already-published wires. Real provider retries remain withheld;
// session controls and reverse feedback take their existing ordinary paths.
func (self *privateProviderEncryptedAckFixture) forwardSourceControls() {
	self.t.Helper()
	synctest.Wait()
	for count := 0; len(self.senderOut) != 0 || len(self.receiverOut) != 0; count++ {
		if count == cap(self.senderOut)+cap(self.receiverOut) {
			self.t.Fatal("bounded source-control wire drain did not quiesce")
		}
		if len(self.senderOut) != 0 {
			frame := self.capture(<-self.senderOut, self.receiver)
			provider := false
			if frame.pack != nil {
				for _, member := range frame.pack.Frames {
					provider = provider || member != nil && member.MessageType == protocol.MessageType_IpIpPacketToProvider
				}
				if !provider && !privateProviderEncryptedControl(frame.pack) {
					self.t.Fatal("unclassified actual source original cannot explain an ordinal gap")
				}
			}
			if !provider {
				self.forward(frame.windowRoundFrame, self.receiverIn)
			}
		}
		self.feedback()
	}
}

// The current immediate owner keeps the only slot. Any intervening ordinal
// must belong to an observed ordinary session control, not the queued offer.
func (self *privateProviderEncryptedAckFixture) assertQueuedAckOriginal(current *protocol.Pack, offer *privateProviderAckOffer) {
	self.t.Helper()
	self.forwardSourceControls()
	source := privateProviderImmediateSource(self.t, self.windowRoundFixture, current.SequenceId)
	observed := self.sourceOriginals(current.SequenceNumber+1, source.nextSequenceNumber)
	controls := make([]*protocol.Pack, 0, len(observed))
	for _, pack := range observed {
		if !privateProviderEncryptedControl(pack) {
			self.t.Fatal("accepted raw ACK successor published an encoded original before its predecessor ACK")
		}
		controls = append(controls, pack)
	}
	if offer.pack != nil || offer.callbacks != 0 {
		self.t.Fatal("queued ACK offer bound or completed before the actual immediate predecessor ACK")
	}
	privateProviderAssertQueuedImmediateSuccessor(self.t, self.windowRoundFixture, current, controls...)
}

// Every final ordinal is an actual application original or a captured session
// control. Both receiver progress and encoded retirement must reach that exact
// source frontier before cancellation or cleanup can make the assertion pass.
func (self *privateProviderEncryptedAckFixture) assertCompletedOriginals(applications []*privateProviderEncryptedAckFrame) {
	self.t.Helper()
	self.forwardSourceControls()
	if len(applications) == 0 {
		self.t.Fatal("source closure has no actual application originals")
	}
	first := applications[0].pack.SequenceNumber
	source := privateProviderImmediateSource(self.t, self.windowRoundFixture, self.sequence.sequenceId.Bytes())
	observed := self.sourceOriginals(first, source.nextSequenceNumber)
	applicationIds := map[uint64]Id{}
	for _, original := range applications {
		pack := original.pack
		if !bytes.Equal(pack.SequenceId, self.sequence.sequenceId.Bytes()) || applicationIds[pack.SequenceNumber] != (Id{}) ||
			self.ackCounts[pack.SequenceNumber] != 1 {
			self.t.Fatal("actual application original lost its exact unique terminal callback")
		}
		applicationIds[pack.SequenceNumber] = RequireIdFromBytes(pack.MessageId)
	}
	if source.nextSequenceNumber < first || source.nextSequenceNumber-first != uint64(len(observed)) {
		self.t.Fatal("actual source frontier contains an uncaptured reliable original")
	}
	terminalAcks := map[uint64]bool{}
	for _, frame := range self.heldFrames {
		ack := frame.ack
		if ack == nil || !bytes.Equal(ack.SequenceId, self.sequence.sequenceId.Bytes()) || len(ack.MissingContractId) != 0 {
			continue
		}
		for number, original := range observed {
			if !bytes.Equal(original.MessageId, ack.MessageId) {
				continue
			}
			if ack.Selective {
				if ack.DeliveredImmediately && original.DeliverImmediately && ack.ImmediateSequenceNumber != nil && *ack.ImmediateSequenceNumber == number {
					terminalAcks[number] = true
				}
			} else {
				for candidate := range observed {
					if candidate <= number {
						terminalAcks[candidate] = true
					}
				}
			}
			break
		}
	}
	controlCount := 0
	for number := first; number < source.nextSequenceNumber; number++ {
		pack := observed[number]
		if pack == nil || !terminalAcks[number] {
			self.t.Fatalf("actual source frontier lacks an observed original or real terminal ACK: ordinal=%d original=%t terminal_ack=%t", number, pack != nil, terminalAcks[number])
		}
		if id, application := applicationIds[number]; application {
			if id != RequireIdFromBytes(pack.MessageId) {
				self.t.Fatal("captured original does not match the application's actual identity")
			}
		} else {
			if !privateProviderEncryptedControl(pack) {
				self.t.Fatal("non-application ordinal is not an actual ordinary encryption control")
			}
			var control protocol.EncryptedControl
			if err := ProtoUnmarshal(pack.Frames[0].MessageBytes, &control); err != nil ||
				control.SessionRole != self.sequence.encryptionRole.complement().toProtobuf() ||
				control.Companion != self.sequence.encryptionCompanion {
				self.t.Fatal("actual encryption control escaped its application sequence's role or companion")
			}
			controlCount++
			self.t.Logf("[provider-source-control] ordinal=%d kind=%d subtype=%d payload_bytes=%d immediate=false actual_terminal_retirement=true", number, pack.Frames[0].MessageType, control.ControlType, len(control.Payload))
		}
		privateProviderAssertOriginalRetired(self.t, self.windowRoundFixture, pack)
	}
	source.protocolAdmission.mutex.Lock()
	queued := source.protocolAdmission.count
	source.protocolAdmission.mutex.Unlock()
	if self.sequence.nextSequenceNumber != source.nextSequenceNumber || self.sequence.holdPromise.ingressFloor != source.nextSequenceNumber ||
		self.sequence.receiveQueue.Len() != 0 || source.resendQueue.Len() != 0 || len(source.sendItems) != 0 || source.immediateSendCount != 0 || queued != 0 {
		self.t.Fatalf("actual original closure: receiver_next=%d receiver_floor=%d source_next=%d applications=%d controls=%d receive_queue=%d retained=%d items=%d immediate=%d queued=%d", self.sequence.nextSequenceNumber, self.sequence.holdPromise.ingressFloor, source.nextSequenceNumber, len(applications), controlCount, self.sequence.receiveQueue.Len(), source.resendQueue.Len(), len(source.sendItems), source.immediateSendCount, queued)
	}
	self.nextNumber = source.nextSequenceNumber
	self.t.Logf("[provider-source-originals-complete] first=%d source_next=%d receiver_next=%d applications=%d controls=%d all_actual_originals_retired=true", first, source.nextSequenceNumber, self.sequence.nextSequenceNumber, len(applications), controlCount)
}
