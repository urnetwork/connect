// IPV6.md C7: the single-destination client admits a provider's dial-failure
// signal for its own outstanding SYN and nothing else. Packets are built with
// the provider's own builder (ipOosUnreachable); nothing leaves the process.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Recomputes the checksums of an edited signal, so a case differs from the
// provider's packet only in the property it tests.
func resealDialFailureSignal(packet []byte) []byte {
	switch packet[0] >> 4 {
	case 4:
		ihl := int(packet[0]&0x0f) * 4
		binary.BigEndian.PutUint16(packet[10:12], 0)
		binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[:ihl])))
		icmp := packet[ihl:]
		binary.BigEndian.PutUint16(icmp[2:4], 0)
		binary.BigEndian.PutUint16(icmp[2:4], checksumFinish(checksumAdd(0, icmp)))
	case 6:
		icmp := packet[Ipv6HeaderSize:]
		binary.BigEndian.PutUint16(icmp[2:4], 0)
		binary.BigEndian.PutUint16(icmp[2:4], transportChecksum(ipProtocolNumberIcmp6, packet[8:24], packet[24:40], icmp))
	}
	return packet
}

func dialFailureSignal(t *testing.T, ipPath *IpPath) []byte {
	t.Helper()
	signal, ok := ipOosUnreachable(ipPath)
	if !ok {
		t.Fatal("could not build the provider dial-failure signal")
	}
	return signal
}

func dialFailureIcmpOffset(packet []byte) int {
	if packet[0]>>4 == 4 {
		return int(packet[0]&0x0f) * 4
	}
	return Ipv6HeaderSize
}

// IPV6.md C7. The provider's signal for an outstanding SYN is admitted once,
// from that provider, as the failed flow in the ingress direction.
func TestRemoteUserNatDialFailureAdmitsProviderSignalForOutstandingSyn(t *testing.T) {
	for _, version := range []int{4, 6} {
		var gate remoteUserNatDialFailures
		provider := NewId()
		syn := icmpTcpTestPath(version)
		gate.syn(syn, provider)
		signal := dialFailureSignal(t, syn)
		ingress, ok := gate.admit(provider, signal)
		if !ok || ingress.Protocol != IpProtocolTcp || !ingress.SourceIp.Equal(syn.DestinationIp) || ingress.SourcePort != syn.DestinationPort ||
			!ingress.DestinationIp.Equal(syn.SourceIp) || ingress.DestinationPort != syn.SourcePort {
			t.Fatalf("v%d: admitted=%t ingress=%+v, want the reverse of %+v", version, ok, ingress, syn)
		}
		if _, ok := gate.admit(provider, signal); ok {
			t.Fatalf("v%d: one SYN admitted two signals", version)
		}
	}
}

// IPV6.md C7. A deployed provider builds its signal from its flow path,
// whose sequence number is zero, so the gate matches the 5-tuple alone.
func TestRemoteUserNatDialFailureAdmitsDeployedProviderSignal(t *testing.T) {
	for _, version := range []int{4, 6} {
		var gate remoteUserNatDialFailures
		provider := NewId()
		syn := icmpTcpTestPath(version)
		gate.syn(syn, provider)
		flowPath := *syn
		flowPath.SequenceNumber = 0
		if _, ok := gate.admit(provider, dialFailureSignal(t, &flowPath)); !ok {
			t.Fatalf("v%d: the deployed provider's signal was not admitted", version)
		}
	}
}

// IPV6.md C7. Only the SYN's own flow is admitted: no recorded SYN, another
// port or another destination is unrelated, and none of them consumes the
// real SYN.
func TestRemoteUserNatDialFailureIgnoresUnrelatedSignal(t *testing.T) {
	for _, version := range []int{4, 6} {
		provider := NewId()
		syn := icmpTcpTestPath(version)
		var fresh remoteUserNatDialFailures
		if _, ok := fresh.admit(provider, dialFailureSignal(t, syn)); ok {
			t.Fatalf("v%d: admitted with no SYN recorded", version)
		}
		var gate remoteUserNatDialFailures
		gate.syn(syn, provider)
		otherPort, otherDestination := *syn, *syn
		otherPort.SourcePort++
		otherDestination.DestinationPort++
		for name, unrelated := range map[string]*IpPath{"port": &otherPort, "destination": &otherDestination} {
			if _, ok := gate.admit(provider, dialFailureSignal(t, unrelated)); ok {
				t.Fatalf("v%d: admitted a signal for another %s", version, name)
			}
		}
		if _, ok := gate.admit(provider, dialFailureSignal(t, syn)); !ok {
			t.Fatalf("v%d: unrelated signals consumed the outstanding SYN", version)
		}
	}
}

// IPV6.md C7. A signal from another provider, or one not addressed back from
// the SYN's destination to its source, is spoofed and ignored.
func TestRemoteUserNatDialFailureIgnoresSpoofedSignal(t *testing.T) {
	for _, version := range []int{4, 6} {
		provider := NewId()
		syn := icmpTcpTestPath(version)
		var gate remoteUserNatDialFailures
		gate.syn(syn, provider)
		if _, ok := gate.admit(NewId(), dialFailureSignal(t, syn)); ok {
			t.Fatalf("v%d: admitted a signal from another provider", version)
		}
		sourceOffset, destinationOffset, size := 12, 16, 4
		if version == 6 {
			sourceOffset, destinationOffset, size = 8, 24, 16
		}
		for name, offset := range map[string]int{"source": sourceOffset, "destination": destinationOffset} {
			spoofed := dialFailureSignal(t, syn)
			spoofed[offset+size-1] ^= 0x01
			if _, ok := gate.admit(provider, resealDialFailureSignal(spoofed)); ok {
				t.Fatalf("v%d: admitted a signal with a rewritten outer %s", version, name)
			}
		}
		if _, ok := gate.admit(provider, dialFailureSignal(t, syn)); !ok {
			t.Fatalf("v%d: spoofed signals consumed the outstanding SYN", version)
		}
	}
}

// IPV6.md C7. Malformed signals are ignored: truncated, oversized, bad
// checksums, a fragment, another type or code, a UDP embed, or an embed of
// the other family.
func TestRemoteUserNatDialFailureIgnoresMalformedSignal(t *testing.T) {
	for _, version := range []int{4, 6} {
		provider := NewId()
		syn := icmpTcpTestPath(version)
		var gate remoteUserNatDialFailures
		gate.syn(syn, provider)
		cases := map[string][]byte{}
		signal := dialFailureSignal(t, syn)
		icmpOffset := dialFailureIcmpOffset(signal)
		setLength := func(packet []byte) []byte {
			if version == 4 {
				binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
			} else {
				binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-Ipv6HeaderSize))
			}
			return resealDialFailureSignal(packet)
		}
		cases["empty"] = []byte{}
		cases["truncated"] = append([]byte(nil), signal[:len(signal)-1]...)
		embeddedHeader := Ipv4HeaderSizeWithoutExtensions
		if version == 6 {
			embeddedHeader = Ipv6HeaderSize
		}
		// seven of the eight transport bytes an embed must carry
		cases["short embed"] = setLength(append([]byte(nil), signal[:icmpOffset+icmpUnreachableHeaderSize+embeddedHeader+7]...))
		cases["oversized"] = setLength(append(append([]byte(nil), signal...), make([]byte, 1280)...))
		badIcmpChecksum := append([]byte(nil), signal...)
		badIcmpChecksum[icmpOffset+2] ^= 0xff
		cases["icmp checksum"] = badIcmpChecksum
		wrongCode := append([]byte(nil), signal...)
		wrongType := append([]byte(nil), signal...)
		if version == 4 {
			badHeaderChecksum := append([]byte(nil), signal...)
			badHeaderChecksum[10] ^= 0xff
			cases["header checksum"] = badHeaderChecksum
			fragment := append([]byte(nil), signal...)
			fragment[6] |= 0x20
			cases["fragment"] = resealDialFailureSignal(fragment)
			wrongCode[icmpOffset+1] = icmpv4CodePortUnreachable
			wrongType[icmpOffset] = 0
		} else {
			wrongCode[icmpOffset+1] = icmpv6CodePortUnreachable
			wrongType[icmpOffset] = icmpv6TypePacketTooBig
		}
		cases["code"] = resealDialFailureSignal(wrongCode)
		cases["type"] = resealDialFailureSignal(wrongType)
		udp := *syn
		udp.Protocol = IpProtocolUdp
		cases["udp embed"] = dialFailureSignal(t, &udp)
		otherFamily := icmpTcpTestPath(10 - version)
		embed := dialFailureSignal(t, otherFamily)
		mixed := append(append([]byte(nil), signal[:icmpOffset+icmpUnreachableHeaderSize]...), embed[dialFailureIcmpOffset(embed)+icmpUnreachableHeaderSize:]...)
		cases["other family embed"] = setLength(mixed)
		for name, malformed := range cases {
			if _, ok := gate.admit(provider, malformed); ok {
				t.Fatalf("v%d: admitted a malformed signal (%s)", version, name)
			}
		}
		if _, ok := gate.admit(provider, signal); !ok {
			t.Fatalf("v%d: malformed signals consumed the outstanding SYN", version)
		}
	}
}

// IPV6.md C7. A signal after the SYN was answered (SYN-ACK or reset) or
// after its lifetime is late and ignored; a retransmitted SYN restarts the
// lifetime.
func TestRemoteUserNatDialFailureIgnoresLateSignal(t *testing.T) {
	for _, version := range []int{4, 6} {
		syn := icmpTcpTestPath(version)
		for name, reply := range map[string]*IpPath{
			"syn-ack": {Version: version, Protocol: IpProtocolTcp, Syn: true, Ack: true},
			"reset":   {Version: version, Protocol: IpProtocolTcp, Rst: true, Ack: true},
		} {
			provider := NewId()
			var gate remoteUserNatDialFailures
			gate.syn(syn, provider)
			reverse := syn.Reverse()
			reply.SourceIp, reply.SourcePort = reverse.SourceIp, reverse.SourcePort
			reply.DestinationIp, reply.DestinationPort = reverse.DestinationIp, reverse.DestinationPort
			gate.reply(reply)
			if _, ok := gate.admit(provider, dialFailureSignal(t, syn)); ok {
				t.Fatalf("v%d: admitted a signal after a %s", version, name)
			}
		}
		synctest.Test(t, func(t *testing.T) {
			provider := NewId()
			var gate remoteUserNatDialFailures
			gate.syn(syn, provider)
			time.Sleep(remoteUserNatPendingSynLifetime)
			if _, ok := gate.admit(provider, dialFailureSignal(t, syn)); ok {
				t.Fatalf("v%d: admitted a signal after the SYN lifetime", version)
			}
			gate.syn(syn, provider)
			time.Sleep(remoteUserNatPendingSynLifetime * 2 / 3)
			gate.syn(syn, provider)
			time.Sleep(remoteUserNatPendingSynLifetime * 2 / 3)
			if _, ok := gate.admit(provider, dialFailureSignal(t, syn)); !ok {
				t.Fatalf("v%d: a retransmitted SYN did not restart its lifetime", version)
			}
		})
	}
}

// IPV6.md C7. Only bare SYNs are tracked, and the table is bounded: beyond
// the limit a new SYN is not tracked until expired ones make room.
func TestRemoteUserNatDialFailureTracksBoundedBareSyns(t *testing.T) {
	provider := NewId()
	var gate remoteUserNatDialFailures
	synAck := icmpTcpTestPath(4)
	synAck.Ack = true
	data := icmpTcpTestPath(4)
	data.Syn = false
	data.Ack = true
	udp := icmpTcpTestPath(4)
	udp.Protocol = IpProtocolUdp
	for _, ipPath := range []*IpPath{synAck, data, udp} {
		gate.syn(ipPath, provider)
	}
	if len(gate.pending) != 0 {
		t.Fatalf("tracked %d packets that are not bare SYNs", len(gate.pending))
	}
	synctest.Test(t, func(t *testing.T) {
		var gate remoteUserNatDialFailures
		withPort := func(port int) *IpPath {
			syn := icmpTcpTestPath(4)
			syn.SourcePort = port
			return syn
		}
		for port := 1; port <= remoteUserNatPendingSynLimit; port++ {
			gate.syn(withPort(port), provider)
		}
		beyond := withPort(remoteUserNatPendingSynLimit + 1)
		gate.syn(beyond, provider)
		if _, ok := gate.admit(provider, dialFailureSignal(t, beyond)); ok || len(gate.pending) != remoteUserNatPendingSynLimit {
			t.Fatalf("tracked a SYN beyond the bound: pending=%d", len(gate.pending))
		}
		time.Sleep(remoteUserNatPendingSynLifetime)
		gate.syn(beyond, provider)
		if _, ok := gate.admit(provider, dialFailureSignal(t, beyond)); !ok || len(gate.pending) != 0 {
			t.Fatalf("expired SYNs did not make room: pending=%d", len(gate.pending))
		}
	})
}

// Allows every flow, so a fixture can use test addresses.
type dialFailureAllowPolicy struct {
	stats *SecurityPolicyStatsCollector
}

func (self *dialFailureAllowPolicy) Stats() *SecurityPolicyStatsCollector { return self.stats }

func (self *dialFailureAllowPolicy) InspectEgress(protocol.ProvideMode, *IpPath, []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

func (self *dialFailureAllowPolicy) InspectIngress(protocol.ProvideMode, *IpPath, []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

func (self *dialFailureAllowPolicy) RefreshEgress(*IpPath)  {}
func (self *dialFailureAllowPolicy) RefreshIngress(*IpPath) {}

// A single-destination client on an unrouted Client: its sends go nowhere,
// and the test plays the provider through ClientReceive.
func newDialFailureRemoteClient(t *testing.T, provider Id, receive ReceivePacketFunction) *RemoteUserNatClient {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	remote := NewRemoteUserNatClient(client, receive, []MultiHopId{RequireMultiHopId(provider)}, protocol.ProvideMode_Network)
	remote.securityPolicy = &dialFailureAllowPolicy{stats: DefaultSecurityPolicyStatsCollector()}
	t.Cleanup(func() {
		remote.Close()
		client.Cancel()
		cancel()
	})
	return remote
}

func dialFailureProviderFrames(t *testing.T, packet []byte) []*protocol.Frame {
	t.Helper()
	frame, err := ipPacketFromProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return []*protocol.Frame{frame}
}

// IPV6.md C7. Through RemoteUserNatClient: the provider's signal for a SYN the
// client sent reaches the local stack once; the same signal from another
// provider does not; ordinary provider packets still pass.
func TestRemoteUserNatClientDeliversProviderDialFailure(t *testing.T) {
	for _, version := range []int{4, 6} {
		provider := NewId()
		received := make(chan []byte, 4)
		remote := newDialFailureRemoteClient(t, provider, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			received <- append([]byte(nil), packet...)
		})
		syn := icmpTcpTestPath(version)
		// The SYN is recorded before the unrouted send, whatever its outcome.
		synPacket := MessagePoolCopy(ipOosTcpPacketSequence(syn, tcpFlagSyn, syn.SequenceNumber, nil))
		if !remote.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, synPacket, 0) {
			MessagePoolReturn(synPacket)
		}
		signal := dialFailureSignal(t, syn)
		peer := Peer{ProvideMode: protocol.ProvideMode_Public}
		remote.ClientReceive(SourceId(NewId()), dialFailureProviderFrames(t, signal), peer)
		remote.ClientReceive(SourceId(provider), dialFailureProviderFrames(t, signal), peer)
		remote.ClientReceive(SourceId(provider), dialFailureProviderFrames(t, signal), peer)
		ordinary, ok := ipOosRst(syn.Reverse())
		if !ok {
			t.Fatal("could not build an ordinary provider packet")
		}
		remote.ClientReceive(SourceId(provider), dialFailureProviderFrames(t, ordinary), peer)
		for index, want := range [][]byte{signal, ordinary} {
			select {
			case packet := <-received:
				if !bytes.Equal(packet, want) {
					t.Fatalf("v%d: delivery %d = %x, want %x", version, index, packet, want)
				}
			default:
				t.Fatalf("v%d: delivery %d missing", version, index)
			}
		}
		if extra := len(received); extra != 0 {
			t.Fatalf("v%d: %d unexpected deliveries (a spoofed or repeated signal)", version, extra)
		}
	}
}

// IPV6.md C7. The multi client keeps its own intercept: the signal goes to
// its dial-failure callback, never to the packet callback.
func TestMultiClientDialFailureSignalStaysOutOfPacketCallback(t *testing.T) {
	for _, version := range []int{4, 6} {
		egress := icmpTcpTestPath(version)
		packets, failures := 0, 0
		channel := &multiClientChannel{
			ctx:         context.Background(),
			log:         DefaultLogger(),
			args:        &multiClientChannelArgs{},
			settings:    DefaultMultiClientSettings(),
			packetStats: &clientWindowStats{log: DefaultLogger()},
			clientReceivePacketCallback: func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {
				packets++
			},
			dialFailureCallback: func(*multiClientChannel, *IpPath) bool {
				failures++
				return true
			},
		}
		channel.clientReceive(TransferPath{}, dialFailureProviderFrames(t, dialFailureSignal(t, egress)), Peer{ProvideMode: protocol.ProvideMode_Public})
		if packets != 0 || failures != 1 {
			t.Fatalf("v%d: packet callbacks=%d dial failures=%d, want 0 and 1", version, packets, failures)
		}
	}
}
