package connect

// The platform's ObservedAddresses (transfer_observed_addresses.go) through
// the client's real receive path: a Pack from the control id admitted to the
// receive buffer as the client's reader admits it. A receive callback the
// test registers after the client's own runs after the addresses are applied,
// so it is the barrier for each delivery.

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type observedAddressesHarness struct {
	t        *testing.T
	client   *Client
	networks *providerLocalNetworks
	// one per delivered ObservedAddresses frame, after the client applied it
	delivered chan TransferPath
	// sequence numbers per sender
	sequences map[Id]*observedAddressesSender
}

type observedAddressesSender struct {
	sequenceId     Id
	sequenceNumber uint64
}

func newObservedAddressesHarness(t *testing.T, cidrs ...string) *observedAddressesHarness {
	t.Helper()
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, cidrs...))
	settings := closeWaitClientSettings()
	settings.providerLocalNetworksForTest = networks
	self := &observedAddressesHarness{
		t:         t,
		networks:  networks,
		delivered: make(chan TransferPath, 16),
		sequences: map[Id]*observedAddressesSender{},
	}
	self.client = NewClient(t.Context(), NewId(), NewNoContractClientOob(), settings)
	self.client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TransferObservedAddresses {
				self.delivered <- source
			}
		}
	})
	t.Cleanup(func() {
		if err := self.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return self
}

// deliver admits one Pack from sourceId to the client's receive buffer and
// waits until its ObservedAddresses frame was applied.
func (self *observedAddressesHarness) deliver(sourceId Id, frames ...*protocol.Frame) {
	self.t.Helper()
	sender := self.sequences[sourceId]
	if sender == nil {
		sender = &observedAddressesSender{sequenceId: NewId()}
		self.sequences[sourceId] = sender
	}
	sequenceNumber := sender.sequenceNumber
	sender.sequenceNumber += 1
	receivePack := &ReceivePack{
		Source:     SourceId(sourceId),
		SequenceId: sender.sequenceId,
		Pack: &protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     sender.sequenceId.Bytes(),
			SequenceNumber: sequenceNumber,
			Head:           sequenceNumber == 0,
			Frames:         frames,
		},
		ReceiveCallback:  self.client.receiveCallback,
		MessageByteCount: MessageByteCount(frames),
		Unwrapped:        true,
		EncryptionRole:   sequenceTlsRoleServer,
	}
	success, err := self.client.receiveBuffer.Pack(receivePack, time.Second)
	if err != nil || !success {
		receivePack.messagePoolReturn()
		self.t.Fatalf("pack admission = (%t, %v)", success, err)
	}
	select {
	case <-self.delivered:
	case <-time.After(10 * time.Second):
		self.t.Fatal("the ObservedAddresses frame was never delivered")
	}
}

func observedAddressesFrame(addresses ...[]byte) *protocol.Frame {
	return RequireToFrameWithDefaultProtocolVersion(&protocol.ObservedAddresses{Addresses: addresses})
}

func (self *observedAddressesHarness) observed() []netip.Addr {
	self.networks.stateLock.Lock()
	defer self.networks.stateLock.Unlock()
	return slices.Clone(self.networks.observedAddresses)
}

// The frame round-trips with its own message type.
func TestObservedAddressesFrame(t *testing.T) {
	frame := observedAddressesFrame(netip.MustParseAddr("203.0.113.50").AsSlice())
	if frame.MessageType != protocol.MessageType_TransferObservedAddresses {
		t.Fatalf("message type = %s", frame.MessageType)
	}
	message, err := FromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	observedAddresses, ok := message.(*protocol.ObservedAddresses)
	if !ok || len(observedAddresses.Addresses) != 1 {
		t.Fatalf("decoded %T %v", message, message)
	}
}

// The control source's ObservedAddresses replaces the observed set: the
// public addresses are kept (a mapped one as its IPv4 address), the rest and
// a malformed entry are skipped. The same frame from a peer changes nothing,
// so no peer can add or clear the provider's addresses.
func TestClientAppliesObservedAddressesFromControlOnly(t *testing.T) {
	h := newObservedAddressesHarness(t)

	h.deliver(ControlId, observedAddressesFrame(
		netip.MustParseAddr("203.0.113.50").AsSlice(),
		netip.MustParseAddr("2a01:4f8:c17:5678::99").AsSlice(),
		netip.MustParseAddr("10.0.0.1").AsSlice(),
		netip.MustParseAddr("::ffff:198.51.100.5").AsSlice(),
		[]byte{203, 0, 113},
	))
	want := []netip.Addr{
		netip.MustParseAddr("203.0.113.50"),
		netip.MustParseAddr("2a01:4f8:c17:5678::99"),
		netip.MustParseAddr("198.51.100.5"),
	}
	if observed := h.observed(); !slices.Equal(observed, want) {
		t.Fatalf("observed = %v, want %v", observed, want)
	}

	peerId := NewId()
	h.client.ContractManager().AddNoContractPeer(peerId)
	h.deliver(peerId, observedAddressesFrame(netip.MustParseAddr("198.51.100.99").AsSlice()))
	h.deliver(peerId, observedAddressesFrame())
	if observed := h.observed(); !slices.Equal(observed, want) {
		t.Fatalf("a peer's frame changed the observed set to %v", observed)
	}

	// a later control frame replaces the set, and an empty one clears it
	h.deliver(ControlId, observedAddressesFrame(netip.MustParseAddr("198.51.100.6").AsSlice()))
	if observed := h.observed(); !slices.Equal(observed, []netip.Addr{netip.MustParseAddr("198.51.100.6")}) {
		t.Fatalf("observed after a replacement = %v", observed)
	}
	h.deliver(ControlId, observedAddressesFrame())
	if observed := h.observed(); len(observed) != 0 {
		t.Fatalf("observed after an empty frame = %v", observed)
	}
}

// What an older client does with this message type: a control Pack that also
// carries a message type the client does not know is applied for the frames it
// knows, and the unknown frame is ignored without error. A client from before
// TransferObservedAddresses sees exactly this for the platform's new frame.
func TestClientIgnoresUnknownControlMessageType(t *testing.T) {
	h := newObservedAddressesHarness(t)
	unknown := &protocol.Frame{
		MessageType:  protocol.MessageType(protocol.MessageType_TransferObservedAddresses + 100),
		MessageBytes: []byte{0x0a, 0x04, 203, 0, 113, 51},
	}
	if _, err := FromFrame(unknown); err == nil {
		t.Fatal("the future message type decoded")
	}
	h.deliver(ControlId, unknown, observedAddressesFrame(netip.MustParseAddr("203.0.113.50").AsSlice()))
	if observed := h.observed(); !slices.Equal(observed, []netip.Addr{netip.MustParseAddr("203.0.113.50")}) {
		t.Fatalf("observed = %v, want the known frame applied", observed)
	}
	if h.client.ctx.Err() != nil {
		t.Fatal("the client ended on an unknown control message type")
	}
}

// End to end in connect: the platform's ObservedAddresses reaches the client,
// and the client's provider then refuses a consumer packet to that address at
// RemoteUserNatProvider.ClientReceive, while another public destination still
// reaches the exit.
func TestProviderRefusesPlatformObservedAddressAtClientReceive(t *testing.T) {
	h := newObservedAddressesHarness(t, "192.168.1.20/24")
	natSettings := DefaultLocalUserNatSettings()
	refuse := &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		},
	}
	natSettings.TcpBufferSettings.DialContextSettings = refuse
	natSettings.UdpBufferSettings.DialContextSettings = refuse
	nat := NewLocalUserNat(h.client.Ctx(), "observed-addresses-exit", natSettings)
	t.Cleanup(nat.Close)
	settings := DefaultRemoteUserNatProviderSettings()
	settings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return newTestProviderSecurityPolicy(ctx, h.networks, stats)
	}
	provider := NewRemoteUserNatProvider(h.client, nat, settings)
	t.Cleanup(provider.Close)
	admitted := []string{}
	provider.beforeLocalUserNatAdmissionForTest = func(sourceId Id, packets [][]byte) {
		for _, packet := range packets {
			if ipPath, err := ParseIpPath(packet); err == nil {
				admitted = append(admitted, ipPath.DestinationIp.String())
			}
		}
	}
	consumer := SourceId(NewId())
	sourcePort := 47000
	receive := func(destination string) {
		t.Helper()
		sourcePort += 1
		flow := providerLocalEntryFlow(IpProtocolTcp, destination, 8443)
		flow.SourcePort = sourcePort
		frame, err := ToFrame(&protocol.IpPacketToProvider{
			IpPacket: &protocol.IpPacket{PacketBytes: ipOosTcpPacketSequence(flow, tcpFlagSyn, 0x5eed, nil)},
		}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		provider.ClientReceive(consumer, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Public})
	}

	receive("203.0.113.50")
	if !slices.Contains(admitted, "203.0.113.50") {
		t.Fatal("the router's public address was refused before the platform observed it")
	}
	h.deliver(ControlId, observedAddressesFrame(netip.MustParseAddr("203.0.113.50").AsSlice()))
	admitted = nil
	receive("203.0.113.50")
	receive("198.51.100.77")
	if !slices.Equal(admitted, []string{"198.51.100.77"}) {
		t.Fatalf("admitted %v, want only the unobserved public destination", admitted)
	}
	if count := provider.SecurityPolicyReasons(false)[SecurityPolicyReasonProviderLocal][SecurityDestination{Version: 4, Protocol: IpProtocolTcp, Port: 8443}]; count != 1 {
		t.Fatalf("provider-local count = %d, want 1", count)
	}
}
