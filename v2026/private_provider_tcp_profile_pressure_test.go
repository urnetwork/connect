// Packet inputs transfer to the NAT only on successful admission. Provider
// controls borrow their input; callbacks retain metadata copies, never roots.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

type privateTcpProfilePressureOutput struct {
	sequenceNumber uint32
	ackNumber      uint32
	syn            bool
	ack            bool
	rst            bool
	payload        []byte
}

type privateTcpProfilePressureFixture struct {
	t                *testing.T
	cancel           context.CancelFunc
	root             *TransferMemoryBudget
	budget           *TransferMemoryBudget
	rootSiblingBytes ByteCount
	fillBytes        ByteCount
	client           *Client
	nats             []*LocalUserNat
	providers        []*RemoteUserNatProvider
	unsubscribes     []func()
	nat              *LocalUserNat
	provider         *RemoteUserNatProvider
	probeSocket      net.Conn
	origin           net.Conn
	source           TransferPath
	created          chan *TcpSequence
	processed        chan struct{}
	outputs          chan privateTcpProfilePressureOutput
	dials            atomic.Int64
	sequences        []*TcpSequence
	closed           bool
}

// The root and NAT child reproduce the SDK's 9/20 + 4/20 root and half
// provider share. The sibling reservation stands for other admitted device
// leaves, so a full NAT child also reaches the same root's exact ceiling.
func newPrivateTcpProfilePressureFixture(t *testing.T, target ByteCount, overlap bool) *privateTcpProfilePressureFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	providerShare := target * 4 / 20
	root := NewTransferMemoryBudget(target*9/20 + providerShare)
	budget := NewTransferMemoryBudgetWithParent(providerShare/2, root)
	fixture := &privateTcpProfilePressureFixture{
		t:         t,
		cancel:    cancel,
		root:      root,
		budget:    budget,
		source:    SourceId(NewId()),
		created:   make(chan *TcpSequence, 256),
		processed: make(chan struct{}, 1),
		outputs:   make(chan privateTcpProfilePressureOutput, 8),
	}
	t.Cleanup(fixture.close)
	rootSiblingBytes := root.TotalByteCount() - budget.TotalByteCount()
	if !root.TryReserve(rootSiblingBytes) {
		t.Fatal("other device leaves did not fit the shared root")
	}
	fixture.rootSiblingBytes = rootSiblingBytes
	fixture.probeSocket, fixture.origin = net.Pipe()
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	fixture.client = NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	newNat := func(primary bool) *LocalUserNat {
		settings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(providerShare)
		settings.Log = NewNoopLogger()
		settings.MemoryBudget = budget
		if primary {
			settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
				fixture.created <- sequence
			}
			settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
				DialContext: func(dialCtx context.Context, _, address string) (net.Conn, error) {
					fixture.dials.Add(1)
					if address == net.JoinHostPort("198.51.100.7", "443") {
						return fixture.probeSocket, nil
					}
					// Cold owners have consumed their SYN and reached the real
					// dial boundary. No test reservation substitutes for a flow.
					<-dialCtx.Done()
					return nil, dialCtx.Err()
				},
			}
		}
		nat, err := TryNewLocalUserNat(ctx, "synthetic profile pressure", settings)
		if err != nil {
			t.Fatal(err)
		}
		fixture.nats = append(fixture.nats, nat)
		return nat
	}
	newProvider := func(nat *LocalUserNat) *RemoteUserNatProvider {
		settings := DefaultRemoteUserNatProviderSettingsWithMemoryTarget(providerShare)
		settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		provider, unsubscribe, err := TryNewRemoteUserNatProviderWithPacketStats(fixture.client, nat, settings, func(*RemoteUserNatProvider, *PacketStats) {})
		if err != nil {
			t.Fatal(err)
		}
		fixture.providers = append(fixture.providers, provider)
		fixture.unsubscribes = append(fixture.unsubscribes, unsubscribe)
		// The callback below is the receiving inner TCP peer. This test does
		// not substitute a Transfer ACK for the origin's cumulative TCP ACK.
		provider.localUserNatUnsub()
		return provider
	}
	newNat(false) // local fallback survives the provider generation
	if overlap {
		newProvider(newNat(false)) // old generation remains fully admitted
	}
	fixture.nat = newNat(true)
	fixture.provider = newProvider(fixture.nat)
	// Both hooks are immutable before the first publication to this shard.
	fixture.nat.afterSendPacketForTest = func() { fixture.processed <- struct{}{} }
	fixture.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		kind, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || kind != ipProtocolNumberTcp || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			t.Error("NAT emitted a malformed TCP packet")
			return
		}
		if tcp.sourcePort != 443 || tcp.destinationPort != 40000 {
			t.Error("cold/refused flow emitted output")
			return
		}
		fixture.outputs <- privateTcpProfilePressureOutput{
			sequenceNumber: tcp.seq,
			ackNumber:      tcp.ackNumber,
			syn:            tcp.syn,
			ack:            tcp.ack,
			rst:            tcp.rst,
			payload:        append([]byte(nil), tcp.payload...),
		}
	})
	return fixture
}

func (self *privateTcpProfilePressureFixture) path(index int) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(192, 0, 2, 3).To4(),
		SourcePort:      40000 + index,
		DestinationIp:   net.IPv4(198, 51, 100, 7).To4(),
		DestinationPort: 443 + index,
	}
}

func (self *privateTcpProfilePressureFixture) packet(index int, flags byte, sequenceNumber, ackNumber uint32) []byte {
	path := self.path(index)
	packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, sequenceNumber, nil))
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint32(tcp[8:12], ackNumber)
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	return packet
}

// Wait observes the completed shard decision and deferred packet release.
// It never advances the protocol clock or polls for a constructor eventually.
func (self *privateTcpProfilePressureFixture) dispatched() {
	self.t.Helper()
	synctest.Wait()
	select {
	case <-self.processed:
	default:
		self.t.Fatal("admitted packet did not reach its completed shard disposition")
	}
}

func (self *privateTcpProfilePressureFixture) syn(index int, wantConstructor bool) *TcpSequence {
	self.t.Helper()
	initial := uint32(1000 + index*100)
	packet := self.packet(index, tcpFlagSyn, initial, 0)
	if !self.nat.SendPacket(self.source, protocol.ProvideMode_Network, packet, 0) {
		MessagePoolReturn(packet)
		self.t.Fatal("SYN was refused before the constructor boundary")
	}
	self.dispatched()
	select {
	case sequence := <-self.created:
		if !wantConstructor || sequence.initialSynSeq != initial || sequence.sourcePort != uint16(40000+index) {
			self.t.Fatal("refused or wrong SYN installed a constructor owner")
		}
		self.sequences = append(self.sequences, sequence)
		return sequence
	default:
		if wantConstructor {
			self.t.Fatal("funded SYN did not install its actual constructor owner")
		}
		return nil
	}
}

// ClientReceive borrows this caller's input. Its accepted copy must traverse
// the provider's prepaid control partition and the real NAT dispatcher.
func (self *privateTcpProfilePressureFixture) control(flags byte, ackNumber uint32) {
	self.t.Helper()
	packet := self.packet(0, flags, 1001, ackNumber)
	defer MessagePoolReturn(packet)
	frame, err := ipPacketToProviderFrame(packet, 2)
	if err != nil {
		self.t.Fatal(err)
	}
	before := self.provider.packetStatsCounters.remoteIngressPacketCount.Load()
	self.provider.ClientReceive(self.source, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Network})
	self.dispatched()
	if self.provider.packetStatsCounters.remoteIngressPacketCount.Load() != before+1 || self.provider.ingressControlMemory.UsedByteCount() != 0 {
		self.t.Fatal("control failed provider admission or kept its fixed workspace")
	}
}

func (self *privateTcpProfilePressureFixture) output() privateTcpProfilePressureOutput {
	self.t.Helper()
	synctest.Wait()
	select {
	case output := <-self.outputs:
		return output
	default:
		self.t.Fatal("probe did not emit its real TCP packet")
		return privateTcpProfilePressureOutput{}
	}
}

// Close is idempotent for fatal-path cleanup. Shared cancellation releases
// real dial waits, then every graph joins before its final accounting check.
func (self *privateTcpProfilePressureFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.cancel()
	if self.origin != nil {
		self.origin.Close()
	}
	if self.probeSocket != nil {
		self.probeSocket.Close()
	}
	for index := len(self.providers) - 1; index >= 0; index-- {
		self.providers[index].Close()
		self.unsubscribes[index]()
	}
	for index := len(self.nats) - 1; index >= 0; index-- {
		if err := self.nats[index].CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	if self.client != nil {
		if err := self.client.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	for _, sequence := range self.sequences {
		select {
		case <-sequence.retirementDone:
		default:
			self.t.Error("joined NAT retained an unjoined flow")
		}
	}
	if self.budget.UsedByteCount() != self.fillBytes || self.root.UsedByteCount() != self.fillBytes+self.rootSiblingBytes {
		self.t.Errorf("closed graphs retained claims: nat=%+v root=%+v", self.budget.Stats(), self.root.Stats())
	}
	self.budget.Release(self.fillBytes)
	self.fillBytes = 0
	self.root.Release(self.rootSiblingBytes)
	self.rootSiblingBytes = 0
	for _, budget := range []*TransferMemoryBudget{self.budget, self.root} {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			self.t.Errorf("closed profile budget did not balance: %+v", stats)
		}
	}
}

func runPrivateTcpProfilePressure(t *testing.T, target ByteCount, overlap bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newPrivateTcpProfilePressureFixture(t, target, overlap)
		settings := fixture.nat.settings.TcpBufferSettings
		if DefaultMtu != 1280 || settings.Mtu != 1280 || settings.SequenceBufferSize != 16 || settings.ReadBufferByteCount != 4096 || settings.WriteBatchSize != privateTcpProfilePressureExpectedMaxBatchCount {
			t.Fatalf("constructor profile differs from sealed arm: mtu=%d queue=%d read=%d maximum_batch=%d want_maximum_batch=%d", settings.Mtu, settings.SequenceBufferSize, settings.ReadBufferByteCount, settings.WriteBatchSize, privateTcpProfilePressureExpectedMaxBatchCount)
		}
		packetRoot := ByteCount(2048 + MessagePoolMetaByteCount)
		baseClaim := kib(24) + ByteCount(unsafe.Sizeof(tcpReturnRecoveryFlight{})) + 4096 + 9*packetRoot + 16*128
		flowClaim := baseClaim + privateTcpProfilePressureExpectedColdWorkspaceBytes + ByteCount(2*(privateTcpProfilePressureExpectedColdBatchCount-2))*packetRoot
		if natTcpFlowMemoryByteCount(settings) != flowClaim {
			t.Fatal("actual constructor charge differs from independently pinned queue/held-root/workspace ledger")
		}
		fixed := 2*natMemoryFixedBytes + natProviderMemoryByteCount(natProviderSourceLimit) + kib(1)
		if overlap {
			fixed += natMemoryFixedBytes + natProviderMemoryByteCount(natProviderSourceLimit) + kib(1)
		}
		if fixture.budget.UsedByteCount() != fixed || fixture.root.UsedByteCount() != fixed+fixture.rootSiblingBytes {
			t.Fatal("actual fallback/provider generation topology differs from pinned fixed claims")
		}
		fanout := int((fixture.budget.TotalByteCount() - fixed) / flowClaim)
		baselineFanout := int((fixture.budget.TotalByteCount() - fixed) / baseClaim)
		if fanout < 2 || settings.UserLimit < fanout || settings.GlobalLimit < fanout {
			t.Fatal("FD cap or fixture prevented a memory-bound multi-flow census")
		}
		probe := fixture.syn(0, true)
		handshake := fixture.output()
		if !handshake.syn || !handshake.ack || handshake.rst || handshake.ackNumber != 1001 || len(handshake.payload) != 0 {
			t.Fatalf("probe did not emit its real SYN-ACK: %+v", handshake)
		}
		fixture.control(tcpFlagAck, handshake.sequenceNumber+1)
		for index := 1; index < fanout; index++ {
			fixture.syn(index, true)
			if used := fixture.budget.UsedByteCount(); used != fixed+ByteCount(index+1)*flowClaim {
				t.Fatalf("cold constructor claim escaped or retained its consumed SYN: index=%d used=%d", index, used)
			}
		}
		beforeRefusal := fixture.budget.Stats()
		fixture.syn(fanout, false)
		if fixture.dials.Load() != int64(fanout) || fixture.budget.UsedByteCount() != beforeRefusal.UsedByteCount || len(fixture.sequences) != fanout {
			t.Fatal("near-full constructor refusal created a dial/flow owner or retained a packet claim")
		}
		if privateTcpProfilePressureExpectedColdBatchCount == 2 && fanout != baselineFanout || privateTcpProfilePressureExpectedColdBatchCount == 8 && fanout >= baselineFanout {
			t.Fatal("fixed-cap arm did not expose the expected cold fanout difference")
		}
		payload := bytes.Repeat([]byte{0x5a}, 64)
		if written, err := fixture.origin.Write(payload); written != len(payload) || err != nil {
			t.Fatalf("real origin write = %d, %v", written, err)
		}
		data := fixture.output()
		if data.syn || data.rst || !bytes.Equal(data.payload, payload) {
			t.Fatal("probe emitted different origin bytes")
		}
		probe.mutex.Lock()
		returnBytes, metadataBytes := probe.returnByteCount, probe.returnMetadataByteCount
		probe.mutex.Unlock()
		if returnBytes == 0 || metadataBytes == 0 || fixture.budget.UsedByteCount() != fixed+ByteCount(fanout)*flowClaim+returnBytes+metadataBytes {
			t.Fatal("real emitted data did not retain its exact origin replay claim")
		}
		fillBytes := fixture.budget.Available()
		if !fixture.budget.TryReserve(fillBytes) {
			t.Fatal("sibling witness could not reserve the remaining NAT capacity")
		}
		fixture.fillBytes = fillBytes
		if fixture.budget.Available() != 0 || fixture.root.Available() != 0 {
			t.Fatal("sibling witness did not fill the unchanged NAT and root ceilings")
		}
		// Bare SYN admission is protocol-only, but its new flow still requires
		// the ordinary constructor claim. Its accepted packet retires at refusal.
		fullStats := fixture.budget.Stats()
		fixture.syn(fanout+1, false)
		if fixture.dials.Load() != int64(fanout) || fixture.budget.UsedByteCount() != fullStats.UsedByteCount ||
			len(fixture.sequences) != fanout || fixture.root.Available() != 0 {
			t.Fatal("full-budget protocol SYN created a dial/flow or retained packet ownership")
		}
		// One real TFO byte keeps the adjacent DATA admission refusal explicit.
		packet := MessagePoolCopy(ipOosTcpPacketSequence(fixture.path(fanout+1), tcpFlagSyn, 50000, []byte{0x64}))
		if protocolClassPureTcpControl(packet) {
			MessagePoolReturn(packet)
			t.Fatal("TFO refusal control did not contain DATA")
		}
		if fixture.nat.SendPacket(fixture.source, protocol.ProvideMode_Network, packet, 0) {
			t.Fatal("full DATA budget admitted a SYN with early DATA")
		}
		MessagePoolReturn(packet)
		synctest.Wait()
		if fixture.dials.Load() != int64(fanout) || fixture.budget.UsedByteCount() != fullStats.UsedByteCount {
			t.Fatal("refused TFO reached the socket constructor or retained packet credit")
		}
		// The ACK names only bytes observed on the real socket-return packet.
		fixture.control(tcpFlagAck, data.sequenceNumber+uint32(len(data.payload)))
		probe.mutex.Lock()
		remaining, acknowledged := probe.returnByteCount, probe.receiveSeqAck
		probe.mutex.Unlock()
		if remaining != 0 || acknowledged != data.sequenceNumber+uint32(len(data.payload)) || fixture.budget.Available() != returnBytes || fixture.root.Available() != returnBytes {
			t.Fatal("full-budget ACK failed to release exactly its observed origin owner")
		}
		if !fixture.budget.TryReserve(returnBytes) {
			t.Fatal("could not restore full pressure before terminal control")
		}
		fixture.fillBytes += returnBytes
		fixture.control(tcpFlagRst|tcpFlagAck, acknowledged)
		synctest.Wait()
		select {
		case <-probe.retirementDone:
		default:
			t.Fatal("full-budget reset did not join the exact probe flow")
		}
		if fixture.budget.Available() != flowClaim+metadataBytes || fixture.root.Available() != flowClaim+metadataBytes {
			t.Fatal("terminal flow close failed to return its full constructor and metadata claims")
		}
		for _, sibling := range fixture.sequences[1:] {
			if sibling.ctx.Err() != nil {
				t.Fatal("control progress retired a healthy cold sibling")
			}
		}
		if fixture.nat.ctx.Err() != nil || fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil {
			t.Fatal("flow-only pressure canceled a shared parent")
		}
		fixture.budget.Release(fixture.fillBytes)
		fixture.fillBytes = 0
		fixture.syn(fanout+2, true)
		if fixture.budget.UsedByteCount() != fixed+ByteCount(fanout)*flowClaim || fixture.dials.Load() != int64(fanout+1) {
			t.Fatal("reclaimed capacity did not fund a replacement under unchanged ceilings")
		}
		t.Logf("TCP_PROFILE_PRESSURE target=%d overlap=%t maximum_batch=%d cold_batch=%d fixed=%d flow_claim=%d cold_fanout=%d cap2_fanout=%d lost=%d constructor_refused=true full_ack_released=%d full_reset_released=%d", target, overlap, settings.WriteBatchSize, privateTcpProfilePressureExpectedColdBatchCount, fixed, flowClaim, fanout, baselineFanout, baselineFanout-fanout, returnBytes, flowClaim+metadataBytes)
		fixture.close()
	})
}

func TestPrivateTcpProfilePressure32Steady(t *testing.T) {
	runPrivateTcpProfilePressure(t, mib(32), false)
}

func TestPrivateTcpProfilePressure64Steady(t *testing.T) {
	runPrivateTcpProfilePressure(t, mib(64), false)
}

func TestPrivateTcpProfilePressure32GenerationOverlap(t *testing.T) {
	runPrivateTcpProfilePressure(t, mib(32), true)
}

func TestPrivateTcpProfilePressure64GenerationOverlap(t *testing.T) {
	runPrivateTcpProfilePressure(t, mib(64), true)
}
