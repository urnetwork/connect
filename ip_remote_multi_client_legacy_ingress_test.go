// Legacy provider custody is exercised through real Transfer and TCP owners,
// without changing the sender's lifetime collapse policy or using a fake ACK.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Two established flows share one provider and finite in-memory routes. Only
// the pre-receipt provider delivery boundary differs between compatibility cases.
type legacyIngressFixture struct {
	t              *testing.T
	ctx            context.Context
	cancel         context.CancelFunc
	parent         *RemoteUserNatMultiClient
	client         *Client
	providerClient *Client
	provider       *RemoteUserNatProvider
	nat            *LocalUserNat
	selected       *multiClientChannel
	flows          []*legacyIngressFlow
	routes         []Route
	ackedItems     atomic.Int64
	acknowledged   chan struct{}
	holdNat        atomic.Bool
	natEntered     chan struct{}
	natRelease     chan struct{}
	holdOnce       sync.Once
	releaseOnce    sync.Once
	readers        sync.WaitGroup
}

// Each origin is a net.Pipe, not a network dial. Observations own bounded copies
// of borrowed application bytes; production owns all pooled packet buffers.
type legacyIngressFlow struct {
	path       *IpPath
	update     *multiClientChannelUpdate
	local      net.Conn
	origin     net.Conn
	upstream   chan []byte
	downstream chan []byte
	sequence   uint32
	ack        uint32
	dials      atomic.Int64
}

// Constructs current peers, then selects the historical void-callback boundary
// locally at the provider. Before c4f4e59f, callback return cumulatively ACKed
// even when the zero-wait LocalUserNat handoff had refused the packet.
func newLegacyIngressFixture(t *testing.T, version int, legacy bool) *legacyIngressFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &legacyIngressFixture{t: t, ctx: ctx, cancel: cancel,
		acknowledged: make(chan struct{}, 32), natEntered: make(chan struct{}), natRelease: make(chan struct{})}
	t.Cleanup(f.close)
	settings := func() *ClientSettings {
		settings := closeWaitClientSettings()
		settings.ReceiveBufferSettings.AckCompressTimeout = 0
		settings.SendBufferSettings.PrewarmOpeningContract = false
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
		return settings
	}
	clientSettings := settings()
	clientSettings.SendBufferSettings.afterAckSendItemForTest = func(sendSequenceId, uint64) {
		f.ackedItems.Add(1)
		select {
		case f.acknowledged <- struct{}{}:
		default:
		}
	}
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	f.providerClient = NewClient(ctx, NewId(), NewNoContractClientOob(), settings())
	f.client.ContractManager().AddNoContractPeer(f.providerClient.ClientId())
	f.providerClient.ContractManager().AddNoContractPeer(f.client.ClientId())

	for index := range 2 {
		path := &IpPath{Version: version, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, byte(92+index)),
			SourcePort: 44000 + index, DestinationPort: 443}
		if version == 6 {
			path.SourceIp = net.ParseIP("2001:db8::91")
			path.DestinationIp = net.ParseIP(fmt.Sprintf("2001:db8:1::%d", 92+index))
		}
		local, origin := net.Pipe()
		flow := &legacyIngressFlow{path: path, local: local, origin: origin,
			upstream: make(chan []byte, 8), downstream: make(chan []byte, 16), sequence: 100}
		f.flows = append(f.flows, flow)
		f.readers.Add(1)
		go func() {
			defer f.readers.Done()
			var buffer [4096]byte
			for {
				n, err := origin.Read(buffer[:])
				if n > 0 {
					select {
					case flow.upstream <- bytes.Clone(buffer[:n]):
					case <-ctx.Done():
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
	natSettings.Log = NewNoopLogger()
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			for _, flow := range f.flows {
				if address == net.JoinHostPort(flow.path.DestinationIp.String(), strconv.Itoa(flow.path.DestinationPort)) {
					if flow.dials.Add(1) == 1 {
						return flow.local, nil
					}
					return nil, fmt.Errorf("duplicate synthetic origin dial")
				}
			}
			return nil, fmt.Errorf("unexpected synthetic origin dial")
		},
	}
	f.nat = NewLocalUserNat(ctx, "legacy ingress compatibility", natSettings)
	f.nat.beforeSendPacketForTest = func(*SendPacket) {
		if f.holdNat.Load() {
			f.holdOnce.Do(func() {
				close(f.natEntered)
				select {
				case <-f.natRelease:
				case <-ctx.Done():
				}
			})
		}
	}
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	f.provider = NewRemoteUserNatProvider(f.providerClient, f.nat, providerSettings)
	f.providerClient.reliableProviderIngress.Store(!legacy)

	multiSettings := DefaultMultiClientSettings()
	multiSettings.Log = NewNoopLogger()
	multiSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	f.parent = NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{}, nil,
		protocol.ProvideMode_Network, multiSettings)
	f.selected = newPacketTransferTestChannel()
	f.selected.ctx, f.selected.client, f.selected.settings = ctx, f.client, multiSettings
	f.selected.log = NewNoopLogger()
	f.selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.providerClient.ClientId())}
	f.selected.clientReceivePacketCallback = f.parent.clientReceivePacket
	f.selected.clientReceiveUnsub = f.client.AddReceiveCallback(f.selected.clientReceive)
	f.parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		path, err := ParseIpPath(packet)
		if err != nil {
			t.Error(err)
			return
		}
		for _, flow := range f.flows {
			if path.DestinationPort == flow.path.SourcePort {
				select {
				case flow.downstream <- bytes.Clone(packet):
				default:
					t.Error("bounded synthetic application receive queue overflowed")
				}
				return
			}
		}
		t.Error("provider returned a packet for an unknown synthetic flow")
	})
	for _, flow := range f.flows {
		flow.update, _, _ = f.parent.sendUpdate(flow.path, flowPin{})
		flow.update.client.Store(f.selected)
		f.parent.bindClientFlow(flow.update, f.selected)
	}
	up, down := make(Route, 64), make(Route, 64)
	f.routes = []Route{up, down}
	f.client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.providerClient.ClientId())), []Route{up})
	f.providerClient.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{up})
	f.providerClient.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.client.ClientId())), []Route{down})
	f.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{down})
	return f
}

// Cancellation releases test barriers before joining every production owner.
func (self *legacyIngressFixture) close() {
	self.cancel()
	self.releaseNat()
	for _, flow := range self.flows {
		flow.origin.Close()
		flow.local.Close()
	}
	if self.provider != nil {
		self.provider.Close()
	}
	if self.parent != nil {
		if err := self.parent.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	if self.nat != nil {
		if err := self.nat.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	for _, client := range []*Client{self.client, self.providerClient} {
		if client != nil {
			if err := client.CloseAndWait(context.Background()); err != nil {
				self.t.Error(err)
			}
		}
	}
	self.readers.Wait()
	for _, route := range self.routes {
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
	}
}

// The checksum includes the exact inner ACK/window used by retransmissions.
func legacyIngressPacket(flow *legacyIngressFlow, flags byte, payload []byte) []byte {
	packet := ipOosTcpPacketSequence(flow.path, flags, flow.sequence, payload)
	header := Ipv4HeaderSizeWithoutExtensions
	if flow.path.Version == 6 {
		header = Ipv6HeaderSize
	}
	tcp := packet[header:]
	binary.BigEndian.PutUint32(tcp[8:12], flow.ack)
	binary.BigEndian.PutUint16(tcp[14:16], 4096)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(flow.path, ipProtocolNumberTcp, tcp))
	return packet
}

// Both public entry points retain their normal caller-buffer ownership rules.
func (self *legacyIngressFixture) send(packet []byte, batch bool) bool {
	packet = MessagePoolCopy(packet)
	if batch {
		return self.parent.SendPacketBatch(SourceId(self.client.ClientId()), protocol.ProvideMode_Network,
			[][]byte{packet}, 0) == 1
	}
	accepted := self.parent.SendPacket(SourceId(self.client.ClientId()), protocol.ProvideMode_Network, packet, 0)
	if !accepted {
		MessagePoolReturn(packet)
	}
	return accepted
}

// Completes the actual provider/socket handshake before any pressure injection.
func (self *legacyIngressFixture) establish(flow *legacyIngressFlow, batch bool) {
	self.t.Helper()
	ackedBefore := self.ackedItems.Load()
	if !self.send(legacyIngressPacket(flow, tcpFlagSyn, nil), batch) {
		self.t.Fatal("initial SYN was refused")
	}
	var packet []byte
	select {
	case packet = <-flow.downstream:
	case <-time.After(5 * time.Second):
		self.t.Fatal("synthetic provider did not return its SYN-ACK")
	}
	path, err := ParseIpPath(packet)
	if err != nil || !path.Syn || !path.Ack || path.AckSequenceNumber != flow.sequence+1 {
		self.t.Fatalf("invalid provider handshake: path=%+v err=%v", path, err)
	}
	flow.sequence++
	flow.ack = path.SequenceNumber + 1
	if !self.send(legacyIngressPacket(flow, tcpFlagAck, nil), batch) {
		self.t.Fatal("handshake ACK was refused")
	}
	self.waitCumulative(ackedBefore + 2)
	synctest.Wait()
	if !flow.update.receivedInbound.Load() || flow.update.client.Load() != self.selected || flow.dials.Load() != 1 {
		self.t.Fatal("fixture did not establish the real selected-provider TCP flow")
	}
}

// The actual cumulative-ACK worker signals after invoking completion and
// returning the resend item. A selective ACK never calls this hook.
func (self *legacyIngressFixture) waitCumulative(want int64) {
	self.t.Helper()
	deadline := time.After(5 * time.Second)
	for self.ackedItems.Load() < want {
		select {
		case <-self.acknowledged:
		case <-deadline:
			self.t.Fatalf("real cumulative ACK worker reached %d items, want %d", self.ackedItems.Load(), want)
		}
	}
}

// A held real NAT worker and one accepted filler occupy its one-slot channel.
// No callback result or capacity predicate is replaced by the fixture.
func (self *legacyIngressFixture) fillNat() {
	self.t.Helper()
	self.holdNat.Store(true)
	offer := func() bool {
		packet := MessagePoolCopy(legacyIngressPacket(self.flows[0], tcpFlagAck, nil))
		accepted := self.nat.SendPacket(SourceId(self.client.ClientId()), protocol.ProvideMode_Network, packet, 0)
		if !accepted {
			MessagePoolReturn(packet)
		}
		return accepted
	}
	if !offer() {
		self.t.Fatal("NAT barrier packet was refused")
	}
	synctest.Wait()
	select {
	case <-self.natEntered:
	default:
		self.t.Fatal("NAT worker did not reach its owned barrier")
	}
	if !offer() || offer() {
		self.t.Fatal("NAT channel did not expose exactly one remaining slot")
	}
}

// A one-shot release is also safe during early fatal cleanup.
func (self *legacyIngressFixture) releaseNat() {
	self.releaseOnce.Do(func() { close(self.natRelease) })
}

// Independent real upstream and return bytes make peer health observable,
// without incrementing any liveness counter directly.
func (self *legacyIngressFixture) healthyRoundTrip(batch bool) {
	self.t.Helper()
	flow := self.flows[1]
	payload := []byte("unrelated healthy upload")
	if !self.send(legacyIngressPacket(flow, tcpFlagAck, payload), batch) {
		self.t.Fatal("healthy flow upload was refused")
	}
	select {
	case got := <-flow.upstream:
		if !bytes.Equal(got, payload) {
			self.t.Fatal("healthy origin received different upload bytes")
		}
	case <-time.After(5 * time.Second):
		self.t.Fatal("unrelated established flow did not reach its origin")
	}
	flow.sequence += uint32(len(payload))
	reply := []byte("unrelated healthy return")
	if err := flow.origin.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		self.t.Fatal(err)
	}
	if _, err := flow.origin.Write(reply); err != nil {
		self.t.Fatal(err)
	}
	for {
		select {
		case packet := <-flow.downstream:
			path, payload, err := ParseIpPathWithPayload(packet)
			if err != nil {
				self.t.Fatal(err)
			}
			if len(payload) == 0 {
				continue
			}
			if !bytes.Equal(payload, reply) {
				self.t.Fatal("healthy application received different return bytes")
			}
			flow.ack = path.SequenceNumber + uint32(len(payload))
			if !self.send(legacyIngressPacket(flow, tcpFlagAck, nil), batch) {
				self.t.Fatal("healthy return acknowledgement was refused")
			}
			synctest.Wait()
			return
		case <-time.After(5 * time.Second):
			self.t.Fatal("healthy origin return did not reach the application")
		}
	}
}

// Both provider generations share the same observable origin-progress oracle.
// The legacy case must not pass by expiring collapse, re-racing an established
// flow, or accepting a duplicate after a successful Transfer acknowledgement.
func checkLegacyIngressProgress(t *testing.T, legacy bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, batch := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				f := newLegacyIngressFixture(t, version, legacy)
				for _, flow := range f.flows {
					f.establish(flow, batch)
				}
				if f.parent.ReliabilitySettings().TcpCollapseMaxHold != 0 {
					t.Fatal("compatibility fixture weakened lifetime collapse")
				}
				f.fillNat()
				ackedBefore := f.ackedItems.Load()
				writesBefore := f.client.DestinationSendStats(f.providerClient.ClientId()).WriteCount
				payload := []byte("established TCP bytes lost after legacy Transfer ACK")
				packet := legacyIngressPacket(f.flows[0], tcpFlagAck, payload)
				if !f.send(packet, batch) {
					t.Fatal("original data did not acquire real Transfer ownership")
				}
				if legacy {
					f.waitCumulative(ackedBefore + 1)
				}
				synctest.Wait()
				count, _, _ := f.client.ResendQueueSize(f.providerClient.ClientId(), MultiHopId{}, false, false)
				acked := f.ackedItems.Load() - ackedBefore
				drops := f.provider.CongestionDropStats().IngressNatPacketCount
				if f.client.DestinationSendStats(f.providerClient.ClientId()).WriteCount != writesBefore+1 {
					t.Fatal("original data did not cross exactly one actual Transfer wire write")
				}
				if legacy {
					if acked != 1 || count != 0 || drops != 1 {
						t.Fatalf("legacy boundary not reproduced: cumulative ACKs=%d resend owners=%d NAT refusals=%d", acked, count, drops)
					}
				} else if acked != 0 || count != 1 || drops != 0 {
					t.Fatalf("current provider lost pending custody: cumulative ACKs=%d resend owners=%d NAT refusals=%d", acked, count, drops)
				}
				f.releaseNat()
				synctest.Wait()
				f.healthyRoundTrip(batch)
				stats, err := f.selected.WindowStats()
				if err != nil || stats.receiveAckCount == 0 || stats.sendNackCount != 0 {
					t.Fatalf("healthy peer evidence missing: stats=%+v err=%v", stats, err)
				}
				reason, held := blackholeReasonFromStats(time.Now(), stats,
					time.Nanosecond, time.Nanosecond, time.Nanosecond, blackholeGates{})
				if reason != blackholeNone || held != blackholeNone {
					t.Fatalf("unrelated working traffic was classified blackholed: reason=%s held=%s", reason, held)
				}
				if f.send(packet, batch) {
					t.Error("successful Transfer ownership was bypassed by an identical inner TCP retry")
				}
				// The bounded recovery check uses virtual time, not scheduler sleeps.
				// Packet admission is not the oracle: the real origin must see bytes.
				select {
				case got := <-f.flows[0].upstream:
					if !bytes.Equal(got, payload) {
						t.Error("recovery changed the missing origin bytes")
					}
				case <-time.After(5 * time.Second):
					t.Errorf("established TCP made no origin progress after cumulative Transfer ACK and NAT refusal: ipv%d batch=%t legacy=%t; lifetime collapse=%s, resend owners=%d, healthy peer received=%d",
						version, batch, legacy, f.parent.ReliabilitySettings().TcpCollapseMaxHold, count, stats.receiveAckCount)
				}
				synctest.Wait()
				if f.flows[0].update.client.Load() != f.selected || !f.flows[0].update.receivedInbound.Load() || f.selected.IsDone() {
					t.Error("recovery relied on clearing or replacing the established flow")
				}
			})
		}
	}
}

// An older provider can acknowledge receipt after refusing downstream custody.
// The current sender must preserve origin progress without reopening collapse.
func TestTcpCollapseLegacyProviderPostAckRefusalProgress(t *testing.T) {
	checkLegacyIngressProgress(t, true)
}

// Current providers retain the original and withhold cumulative ACK while full;
// releasing capacity delivers those exact bytes without an inner retransmission.
func TestTcpCollapseCurrentProviderBackpressureProgress(t *testing.T) {
	checkLegacyIngressProgress(t, false)
}
