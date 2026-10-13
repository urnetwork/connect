// IPV6.md C7: a provider's own dial-failure signal leaves through its return
// path, bound to the flow that produced it; nothing else new leaves. Every
// dial is a fake that fails or never runs; nothing leaves the process.
package connect

import (
	"bytes"
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A provider flow in its egress direction, as a NAT sequence reports it.
func providerSignalTestFlow(protocol IpProtocol, version int) *IpPath {
	flow := &IpPath{
		Version:         4,
		Protocol:        protocol,
		SourceIp:        net.ParseIP("10.0.0.9").To4(),
		SourcePort:      42001,
		DestinationIp:   net.ParseIP("203.0.113.7").To4(),
		DestinationPort: 8080,
	}
	if version == 6 {
		flow.Version = 6
		flow.SourceIp = net.ParseIP("fd00::9")
		flow.DestinationIp = net.ParseIP("2001:20::7")
	}
	return flow
}

func providerSignalTestCases() []*IpPath {
	cases := []*IpPath{}
	for _, protocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
		for _, version := range []int{4, 6} {
			cases = append(cases, providerSignalTestFlow(protocol, version))
		}
	}
	return cases
}

// IPV6.md C7. Only the exact signal the provider builds for a flow is
// recognized for that flow, as the flow's return direction.
func TestProviderDialFailureReturnPathMatchesOnlyItsFlowSignal(t *testing.T) {
	for _, flow := range providerSignalTestCases() {
		signal, ok := ipOosUnreachable(flow)
		if !ok {
			t.Fatal("could not build the signal")
		}
		returnPath, ok := providerDialFailureReturnPath(flow, signal)
		want := flow.Reverse()
		if !ok || returnPath.Version != want.Version || returnPath.Protocol != want.Protocol ||
			!returnPath.SourceIp.Equal(want.SourceIp) || returnPath.SourcePort != want.SourcePort ||
			!returnPath.DestinationIp.Equal(want.DestinationIp) || returnPath.DestinationPort != want.DestinationPort {
			t.Fatalf("%s v%d: own signal ok=%t path=%+v, want %+v", flow.Protocol, flow.Version, ok, returnPath, want)
		}
		otherFlow := *flow
		otherFlow.SourcePort++
		foreign, _ := ipOosUnreachable(&otherFlow)
		changed := append([]byte(nil), signal...)
		changed[len(changed)-1] ^= 0x01
		withSequence := *flow
		withSequence.SequenceNumber = 7
		sequenced, _ := ipOosUnreachable(&withSequence)
		tooBig, _ := ipOosPacketTooBig(flow, 1280)
		reset, _ := ipOosRst(flow.Reverse())
		for name, packet := range map[string][]byte{
			"another flow's signal": foreign,
			"a changed byte":        changed,
			"another embed":         sequenced,
			"packet too big":        tooBig,
			"tcp reset":             reset,
			"empty":                 {},
		} {
			if flow.Protocol == IpProtocolUdp && name == "another embed" {
				// a udp embed carries no sequence number
				continue
			}
			if _, ok := providerDialFailureReturnPath(flow, packet); ok {
				t.Fatalf("%s v%d: matched %s", flow.Protocol, flow.Version, name)
			}
		}
		if _, ok := providerDialFailureReturnPath(nil, signal); ok {
			t.Fatalf("%s v%d: matched without a flow", flow.Protocol, flow.Version)
		}
	}
}

// One provider return lane and a flow's source, as the existing return-path
// fixtures install them.
func newProviderSignalReturnFixture(t *testing.T) (*RemoteUserNatProvider, TransferPath, TransferKey, *SendSequence) {
	t.Helper()
	provider, client, _ := newProviderTransferKeyTestFixture(t)
	peerId := NewId()
	transferKey := TransferKey{
		ForceStream:         true,
		CompanionContract:   false,
		EncryptionRole:      protocol.SequenceRole_SequenceRoleServer,
		EncryptionCompanion: false,
	}
	sequence := installProviderReturnTestSequence(t, provider, client, sendSequenceId{
		Destination:         peerId,
		CompanionContract:   true,
		ForceStream:         true,
		EncryptionRole:      sequenceTlsRoleServer,
		EncryptionCompanion: false,
	})
	return provider, SourceId(peerId), transferKey, sequence
}

// IPV6.md C7. The provider's own signal for a flow leaves through the return
// path unchanged, for tcp and udp flows of both families.
func TestRemoteUserNatProviderReturnSendsItsDialFailureSignal(t *testing.T) {
	for _, flow := range providerSignalTestCases() {
		provider, source, transferKey, sequence := newProviderSignalReturnFixture(t)
		signal, _ := ipOosUnreachable(flow)
		provider.receiveTransferWithRecovery(source, transferKey, protocol.ProvideMode_Public, receiveRecoveryModeRegenerableControl, flow, MessagePoolCopy(signal))
		queued := waitProviderReturnTestPack(t, sequence)
		packet, err := ipPacketFromProviderBytes(queued.Frame)
		if err != nil || !bytes.Equal(packet, signal) {
			queued.returnFrames()
			t.Fatalf("%s v%d: returned %x err=%v, want the signal", flow.Protocol, flow.Version, packet, err)
		}
		queued.returnFrames()
	}
}

// IPV6.md C7. No other icmp leaves through the return path: a signal naming
// another flow, a changed signal, a packet-too-big or a truncated packet is
// dropped exactly as before.
func TestRemoteUserNatProviderReturnDropsForeignIcmp(t *testing.T) {
	for _, flow := range providerSignalTestCases() {
		otherFlow := *flow
		otherFlow.DestinationPort++
		foreign, _ := ipOosUnreachable(&otherFlow)
		signal, _ := ipOosUnreachable(flow)
		changed := append([]byte(nil), signal...)
		changed[len(changed)-2] ^= 0x10
		tooBig, _ := ipOosPacketTooBig(flow, 1280)
		for name, packet := range map[string][]byte{
			"another flow's signal": foreign,
			"a changed signal":      changed,
			"packet too big":        tooBig,
			"truncated signal":      signal[:len(signal)-9],
		} {
			provider, source, transferKey, sequence := newProviderSignalReturnFixture(t)
			provider.receiveTransferWithRecovery(source, transferKey, protocol.ProvideMode_Public, receiveRecoveryModeRegenerableControl, flow, MessagePoolCopy(packet))
			select {
			case queued := <-sequence.packs:
				queued.returnFrames()
				t.Fatalf("%s v%d: %s left the provider", flow.Protocol, flow.Version, name)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

// IPV6.md C7. A real provider whose upstream dial fails sends its signal to the
// client, which receives it as a provider packet; the single-destination
// client hands a tcp signal to its local stack.
func TestRemoteUserNatProviderDialFailureSignalReachesClient(t *testing.T) {
	for _, flow := range []*IpPath{providerSignalTestFlow(IpProtocolTcp, 4), providerSignalTestFlow(IpProtocolTcp, 6), providerSignalTestFlow(IpProtocolUdp, 4), providerSignalTestFlow(IpProtocolUdp, 6)} {
		ctx, cancel := context.WithCancel(t.Context())
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
		providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
		send, receive := make(chan []byte), make(chan []byte)
		client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{send})
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{receive})
		client.ContractManager().AddNoContractPeer(providerClient.ClientId())
		providerClient.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{send})
		providerClient.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(client.ClientId())), []Route{receive})
		providerClient.ContractManager().AddNoContractPeer(client.ClientId())
		natSettings := DefaultLocalUserNatSettings()
		refuse := &DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ENETUNREACH}
			},
		}
		natSettings.TcpBufferSettings.DialContextSettings = refuse
		natSettings.UdpBufferSettings.DialContextSettings = refuse
		nat := NewLocalUserNat(ctx, "signal provider", natSettings)
		provider := NewRemoteUserNatProvider(providerClient, nat, DefaultRemoteUserNatProviderSettings())

		signals := make(chan []byte, 4)
		unsub := client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					continue
				}
				if packet, err := ipPacketFromProviderBytes(frame); err == nil {
					if _, ok := ipParseIcmpUnreachable(packet); ok {
						signals <- append([]byte(nil), packet...)
					}
				}
			}
		})
		delivered := make(chan []byte, 4)
		remote := NewRemoteUserNatClient(client, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			delivered <- append([]byte(nil), packet...)
		}, []MultiHopId{RequireMultiHopId(providerClient.ClientId())}, protocol.ProvideMode_Network)
		remote.securityPolicy = &dialFailureAllowPolicy{stats: DefaultSecurityPolicyStatsCollector()}

		var opening []byte
		if flow.Protocol == IpProtocolTcp {
			opening = ipOosTcpPacketSequence(flow, tcpFlagSyn, 0x5eed, nil)
		} else {
			opening = ipOosUdpPacket(flow, []byte("query"))
		}
		packet := MessagePoolCopy(opening)
		if !remote.SendPacket(SourceId(client.ClientId()), protocol.ProvideMode_Network, packet, time.Second) {
			MessagePoolReturn(packet)
			t.Fatalf("%s v%d: the opening packet was not sent", flow.Protocol, flow.Version)
		}
		flowPath := *flow
		flowPath.SequenceNumber = 0
		want, _ := ipOosUnreachable(&flowPath)
		select {
		case signal := <-signals:
			if !bytes.Equal(signal, want) {
				t.Fatalf("%s v%d: signal %x, want %x", flow.Protocol, flow.Version, signal, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s v%d: the provider's dial-failure signal never reached the client", flow.Protocol, flow.Version)
		}
		if flow.Protocol == IpProtocolTcp {
			select {
			case packet := <-delivered:
				if !bytes.Equal(packet, want) {
					t.Fatalf("v%d: delivered %x, want the signal", flow.Version, packet)
				}
			case <-time.After(time.Second):
				t.Fatalf("v%d: the single-destination client did not deliver the tcp signal", flow.Version)
			}
		}
		unsub()
		remote.Close()
		provider.Close()
		nat.Close()
		providerClient.Cancel()
		client.Cancel()
		cancel()
	}
}
