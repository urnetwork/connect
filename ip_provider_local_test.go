package connect

// The provider-local guard at the provider's real entry point:
// RemoteUserNatProvider.ClientReceive with crafted IpPacketToProvider frames,
// as TestRemoteUserNatProviderEnforcesReversedPolicy drives it. The provider's
// policy is the one DefaultProviderSecurityPolicyWithStats builds, over an
// injected snapshot of the provider host's addresses. The NAT admission hook
// runs synchronously inside ClientReceive, so whether a frame reached the exit
// LocalUserNat is known when ClientReceive returns; the exit's dialer records
// what it would have dialed and refuses, so nothing leaves this host.

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type providerLocalEntryRig struct {
	provider *RemoteUserNatProvider
	networks *providerLocalNetworks
	// every packet handed to the exit LocalUserNat, parsed
	admitted []IpPath
	// every upstream address the exit dialed
	dialed chan string
}

func newProviderLocalEntryRig(t *testing.T, cidrs ...string) *providerLocalEntryRig {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	clientSettings := DefaultClientSettings()
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettings.ForwardBufferSettings.SequenceBufferSize = 0
	providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	t.Cleanup(providerClient.Cancel)

	rig := &providerLocalEntryRig{
		dialed: make(chan string, 64),
	}
	natSettings := DefaultLocalUserNatSettings()
	refuse := &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			select {
			case rig.dialed <- address:
			default:
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		},
	}
	natSettings.TcpBufferSettings.DialContextSettings = refuse
	natSettings.UdpBufferSettings.DialContextSettings = refuse
	localUserNat := NewLocalUserNat(ctx, "test-exit", natSettings)
	t.Cleanup(localUserNat.Close)

	read := newTestProviderLocalRead(t, cidrs...)
	rig.networks, _ = newTestProviderLocalNetworks(t, read)
	settings := DefaultRemoteUserNatProviderSettings()
	settings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return newTestProviderSecurityPolicy(ctx, rig.networks, stats)
	}
	rig.provider = NewRemoteUserNatProvider(providerClient, localUserNat, settings)
	t.Cleanup(rig.provider.Close)
	rig.provider.beforeLocalUserNatAdmissionForTest = func(sourceId Id, packets [][]byte) {
		for _, packet := range packets {
			if ipPath, err := ParseIpPath(packet); err == nil {
				rig.admitted = append(rig.admitted, *ipPath)
			}
		}
	}
	return rig
}

// receive hands the provider one consumer packet, a tcp syn or a udp
// datagram, as a public peer's IpPacketToProvider frame.
func (self *providerLocalEntryRig) receive(t *testing.T, source TransferPath, flow *IpPath) {
	t.Helper()
	var packet []byte
	if flow.Protocol == IpProtocolTcp {
		packet = ipOosTcpPacketSequence(flow, tcpFlagSyn, 0x5eed, nil)
	} else {
		packet = ipOosUdpPacket(flow, []byte("query"))
	}
	frame, err := ToFrame(&protocol.IpPacketToProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: packet},
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	self.provider.ClientReceive(source, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Public})
}

func (self *providerLocalEntryRig) admittedTo(destination string) bool {
	ip := net.ParseIP(destination)
	for _, ipPath := range self.admitted {
		if ipPath.DestinationIp.Equal(ip) {
			return true
		}
	}
	return false
}

// awaitDial waits for the exit to dial address. The timeout only bounds a
// hang: the admission hook has already shown the packet reached the exit.
func (self *providerLocalEntryRig) awaitDial(t *testing.T, address string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case dialed := <-self.dialed:
			if dialed == address {
				return
			}
		case <-deadline:
			t.Fatalf("the exit never dialed %s", address)
		}
	}
}

func providerLocalEntryFlow(protocol IpProtocol, destination string, port int) *IpPath {
	destinationIp := net.ParseIP(destination)
	flow := &IpPath{
		Version:         6,
		Protocol:        protocol,
		SourceIp:        net.ParseIP("fd00::9"),
		SourcePort:      42001,
		DestinationIp:   destinationIp,
		DestinationPort: port,
	}
	if ip4 := destinationIp.To4(); ip4 != nil {
		flow.Version = 4
		flow.SourceIp = net.ParseIP("10.0.0.9").To4()
		flow.DestinationIp = ip4
	}
	return flow
}

// A consumer's frame to the provider's own global IPv6 address, its router
// or its other subnets never reaches the exit LocalUserNat, over tcp or udp,
// and is counted as a provider-local incident; an ordinary public destination
// of the same family still egresses.
func TestRemoteUserNatProviderRefusesProviderHostAtClientReceive(t *testing.T) {
	rig := newProviderLocalEntryRig(t, "2a01:4f8:c17:1234::10/64", "203.0.113.10/32")
	source := SourceId(NewId())

	blocked := []*IpPath{
		providerLocalEntryFlow(IpProtocolTcp, "2a01:4f8:c17:1234::10", 8080),
		providerLocalEntryFlow(IpProtocolTcp, "2a01:4f8:c17:1234::10", 443),
		providerLocalEntryFlow(IpProtocolTcp, "2a01:4f8:c17:1234::1", 80),
		providerLocalEntryFlow(IpProtocolTcp, "2a01:4f8:c17:12ab::5", 2375),
		providerLocalEntryFlow(IpProtocolUdp, "2a01:4f8:c17:1234::1", 53),
		providerLocalEntryFlow(IpProtocolTcp, "203.0.113.10", 8080),
	}
	for i, flow := range blocked {
		flow.SourcePort = 43000 + i
		rig.receive(t, source, flow)
		if rig.admittedTo(flow.DestinationIp.String()) {
			t.Fatalf("%s %s: reached the exit LocalUserNat", flow.Protocol, flow.DestinationHostPort())
		}
	}

	public := providerLocalEntryFlow(IpProtocolTcp, "2001:20::7", 443)
	public.SourcePort = 44000
	rig.receive(t, source, public)
	if !rig.admittedTo("2001:20::7") {
		t.Fatalf("an ordinary public destination did not reach the exit: admitted %v", rig.admitted)
	}
	rig.awaitDial(t, "[2001:20::7]:443")
	if len(rig.admitted) != 1 {
		t.Fatalf("admitted %d packets, want only the public one: %v", len(rig.admitted), rig.admitted)
	}

	reasons := rig.provider.SecurityPolicyReasons(false)
	for _, flow := range blocked {
		if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(flow)]; count == 0 {
			t.Errorf("%s %s: no provider-local count; reasons = %v", flow.Protocol, flow.DestinationHostPort(), reasons)
		}
	}
	stats := rig.provider.SecurityPolicyStats(false)
	if count := stats[SecurityPolicyResultIncident][newSecurityDestinationPort(blocked[0])]; count == 0 {
		t.Errorf("no incident for the provider's own address: stats = %v", stats)
	}
	if diagnostics := rig.provider.providerDiagnosticsMessage(source.SourceId); diagnostics == nil || diagnostics.BlockIngressPacketCount != uint64(len(blocked)) {
		t.Errorf("provider diagnostics = %+v, want %d blocked ingress packets", diagnostics, len(blocked))
	}
}

// An observed public address is refused at the entry point as soon as it is
// set, without a new read: the provider's router's public IPv4 address,
// reached by hairpin NAT.
func TestRemoteUserNatProviderRefusesObservedAddressAtClientReceive(t *testing.T) {
	rig := newProviderLocalEntryRig(t, "192.168.1.20/24")
	source := SourceId(NewId())

	before := providerLocalEntryFlow(IpProtocolTcp, "198.51.100.77", 8443)
	before.SourcePort = 45000
	rig.receive(t, source, before)
	if !rig.admittedTo("198.51.100.77") {
		t.Fatal("the router's public address reached nothing before it was observed")
	}
	rig.awaitDial(t, "198.51.100.77:8443")

	rig.networks.setObservedAddresses([]netip.Addr{netip.MustParseAddr("198.51.100.77")})
	after := providerLocalEntryFlow(IpProtocolTcp, "198.51.100.77", 8443)
	after.SourcePort = 45001
	rig.receive(t, source, after)
	if len(rig.admitted) != 1 {
		t.Fatalf("the observed address reached the exit: admitted %v", rig.admitted)
	}
	if count := rig.provider.SecurityPolicyReasons(false)[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(after)]; count != 1 {
		t.Fatalf("provider-local count = %d, want 1", count)
	}
}
