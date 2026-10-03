// Client-side handling of the first security Drop on a flow. Before the fix
// the multi-client let the first packets of a flow go to a provider while the
// policy was still inspecting, then on the deciding packet either silently
// blocked the rest (kill switch on) or moved the rest of the flow to the local
// route (kill switch off). Either way a TCP connection died without a reset,
// a UDP sender got no unreachable, and the app's retry to the same destination
// repeated the cycle. These tests use only the public multi-client API with
// the default policy, so they run unchanged on the code before the fix. The
// inputs are fixed bytes; the receive callback is synchronous, so every
// assertion is made right after the SendPacket call that must cause it.
package connect

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

var (
	policyRejectSourceIp      = net.ParseIP("192.0.2.30")
	policyRejectDestinationIp = net.ParseIP("203.0.113.60")
)

const policyRejectPort = 50000

type policyRejectCapture struct {
	packets [][]byte
}

func (self *policyRejectCapture) receive(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
	self.packets = append(self.packets, append([]byte(nil), packet...))
}

func (self *policyRejectCapture) take() [][]byte {
	packets := self.packets
	self.packets = nil
	return packets
}

func newPolicyRejectMulti(t *testing.T, capture *policyRejectCapture, localSecurityBypass bool) *RemoteUserNatMultiClient {
	t.Helper()
	settings := DefaultMultiClientSettings()
	settings.EventEpoch = 10 * time.Millisecond
	settings.HeartbeatInterval = 0
	settings.ProviderProbe = false
	settings.IpAssocSettings = nil
	settings.SecurityPolicyGenerator = DefaultSecurityPolicyWithStats
	multi := NewRemoteUserNatMultiClient(
		context.Background(),
		&testingEmptyMultiClientGenerator{},
		capture.receive,
		protocol.ProvideMode_Public,
		settings,
	)
	multi.SetLocalSecurityBypass(localSecurityBypass)
	t.Cleanup(multi.Close)
	return multi
}

func policyRejectSend(multi *RemoteUserNatMultiClient, transport IpProtocol, sourcePort int, syn bool, payload []byte) {
	packet := craftSecurityPacket(transport, policyRejectSourceIp, sourcePort, policyRejectDestinationIp, policyRejectPort, syn, payload)
	multi.SendPacket(SourceId(NewId()), protocol.ProvideMode_Public, packet, 0)
}

func requireTcpReset(t *testing.T, name string, packets [][]byte, sourcePort int) {
	t.Helper()
	if len(packets) != 1 {
		t.Fatalf("%s: delivered %d packets, want one tcp reset", name, len(packets))
	}
	_, sourceIp, destinationIp, transport, ok := parseIpv4(packets[0])
	var tcp parsedTcp
	if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) || !tcp.rst {
		t.Fatalf("%s: delivered packet is not a tcp reset", name)
	}
	if !sourceIp.Equal(policyRejectDestinationIp) || int(tcp.destinationPort) != sourcePort {
		t.Fatalf("%s: reset addressed %s:%d, want the app's flow", name, destinationIp, tcp.destinationPort)
	}
}

func requireIcmpPortUnreachable(t *testing.T, name string, packets [][]byte, sourcePort int) {
	t.Helper()
	if len(packets) != 1 {
		t.Fatalf("%s: delivered %d packets, want one icmp unreachable", name, len(packets))
	}
	ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(packets[0])
	if !ok || ipProtocol != ipProtocolNumberIcmp4 || len(transport) < 8+20+8 {
		t.Fatalf("%s: delivered packet is not icmpv4", name)
	}
	if transport[0] != 3 || transport[1] != 3 {
		t.Fatalf("%s: icmp type/code = %d/%d, want 3/3", name, transport[0], transport[1])
	}
	if checksumFinish(checksumAdd(0, transport)) != 0 {
		t.Fatalf("%s: icmp checksum invalid", name)
	}
	if !sourceIp.Equal(policyRejectDestinationIp) || !destinationIp.Equal(policyRejectSourceIp) {
		t.Fatalf("%s: unreachable addressed %s -> %s", name, sourceIp, destinationIp)
	}
	// the quoted datagram identifies the app's flow
	quoted := transport[8:]
	quotedHeader := int(quoted[0]&0x0f) * 4
	if int(binary.BigEndian.Uint16(quoted[quotedHeader:quotedHeader+2])) != sourcePort {
		t.Fatalf("%s: quoted source port mismatch", name)
	}
}

// the third encrypted segment decides the flow; the first two pass while the
// policy is inspecting
func policyRejectEncryptedTcpFlow(t *testing.T, multi *RemoteUserNatMultiClient, capture *policyRejectCapture, sourcePort int) {
	t.Helper()
	policyRejectSend(multi, IpProtocolTcp, sourcePort, true, nil)
	policyRejectSend(multi, IpProtocolTcp, sourcePort, false, encryptedPayload(512))
	policyRejectSend(multi, IpProtocolTcp, sourcePort, false, encryptedPayload(512))
	if packets := capture.take(); len(packets) != 0 {
		t.Fatalf("inspecting packets delivered %d packets to the app", len(packets))
	}
	before := multi.PacketStats()
	policyRejectSend(multi, IpProtocolTcp, sourcePort, false, encryptedPayload(512))
	requireTcpReset(t, "deciding segment", capture.take(), sourcePort)
	after := multi.PacketStats()
	if after.LocalEgressPacketCount != before.LocalEgressPacketCount {
		t.Fatal("deciding segment of a provider-routed flow was moved to the local route")
	}
}

func TestRootCauseFirstDropResetsTcpWithKillSwitch(t *testing.T) {
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, false)
	policyRejectEncryptedTcpFlow(t, multi, capture, 46001)

	// the app's retry to the same destination is refused at once
	policyRejectSend(multi, IpProtocolTcp, 46002, true, nil)
	requireTcpReset(t, "retry syn", capture.take(), 46002)
}

func TestRootCauseFirstDropResetsTcpAndRetryRoutesLocally(t *testing.T) {
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, true)
	policyRejectEncryptedTcpFlow(t, multi, capture, 46011)

	// the retry goes to the local route from its first packet, so the whole
	// connection has one source address
	before := multi.PacketStats()
	policyRejectSend(multi, IpProtocolTcp, 46012, true, nil)
	policyRejectSend(multi, IpProtocolTcp, 46012, false, encryptedPayload(512))
	after := multi.PacketStats()
	if after.LocalEgressPacketCount-before.LocalEgressPacketCount != 2 {
		t.Fatalf("retry local packets = %d, want 2", after.LocalEgressPacketCount-before.LocalEgressPacketCount)
	}
	if packets := capture.take(); len(packets) != 0 {
		t.Fatalf("locally routed retry delivered %d packets to the app", len(packets))
	}
}

func TestRootCauseFirstDropSendsIcmpForUdp(t *testing.T) {
	for _, localSecurityBypass := range []bool{false, true} {
		capture := &policyRejectCapture{}
		multi := newPolicyRejectMulti(t, capture, localSecurityBypass)
		policyRejectSend(multi, IpProtocolUdp, 46021, false, encryptedPayload(512))
		policyRejectSend(multi, IpProtocolUdp, 46021, false, encryptedPayload(512))
		if packets := capture.take(); len(packets) != 0 {
			t.Fatalf("bypass=%t: inspecting datagrams delivered %d packets", localSecurityBypass, len(packets))
		}
		policyRejectSend(multi, IpProtocolUdp, 46021, false, encryptedPayload(512))
		requireIcmpPortUnreachable(t, "deciding datagram", capture.take(), 46021)
	}
}

func TestRootCauseIncidentNeverRoutesRetryLocally(t *testing.T) {
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, true)
	policyRejectSend(multi, IpProtocolTcp, 46031, true, nil)
	policyRejectSend(multi, IpProtocolTcp, 46031, false, btHandshake())
	before := multi.PacketStats()
	policyRejectSend(multi, IpProtocolTcp, 46032, true, nil)
	after := multi.PacketStats()
	if after.LocalEgressPacketCount != before.LocalEgressPacketCount {
		t.Fatal("a bittorrent incident moved a later flow to the local route")
	}
	capture.take()
}
