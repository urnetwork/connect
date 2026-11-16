package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

func TestMultiClientProtocolOptionsPreserveAckAndWholeGroupIsolation(t *testing.T) {
	client := &Client{ctx: context.Background(), settings: DefaultClientSettings()}
	control := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: protocolClassTestPacket(false, 0x10, 12, 0)}
	syn := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: protocolClassTestPacket(false, 0x02, 0, 0)}
	fin := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: protocolClassTestPacket(false, 0x01, 0, 0)}
	rst := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: protocolClassTestPacket(false, 0x04, 0, 0)}
	data := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: protocolClassTestPacket(false, 0x10, 0, 1)}
	for _, ack := range []bool{false, true} {
		base := []any{TransferOptions{Ack: ack, ForceStream: true, CompanionContract: true}}
		for _, entry := range []struct {
			frames    []*protocol.Frame
			want      bool
			immediate bool
		}{
			{frames: []*protocol.Frame{control}, want: true, immediate: true},
			{frames: []*protocol.Frame{control, control}, want: true, immediate: true},
			{frames: []*protocol.Frame{syn}, want: true},
			{frames: []*protocol.Frame{fin}, want: true},
			{frames: []*protocol.Frame{rst}, want: true},
			{frames: []*protocol.Frame{control, syn}, want: true},
			{frames: []*protocol.Frame{control, data}, want: false},
			{frames: []*protocol.Frame{data, control}, want: false},
			{frames: []*protocol.Frame{data}, want: false},
			{frames: []*protocol.Frame{control, nil}, want: false},
			{frames: nil, want: false},
		} {
			actual := client.resolveSendOptions(multiClientProtocolTransferOpts(base, entry.frames)).transferOptions
			if actual.NoBudget != entry.want || actual.DeliverImmediately != entry.immediate || actual.Ack != (ack || entry.immediate) || !actual.ForceStream || !actual.CompanionContract {
				t.Fatal("IP policy lost reliable pure-ACK delivery or changed lifecycle/DATA/route policy")
			}
		}
	}
	// The generic Client API trusts explicitly supplied producer policy; this
	// IP helper must not reinterpret a caller's independent explicit option.
	explicit := client.resolveSendOptions(multiClientProtocolTransferOpts([]any{NoBudget()}, []*protocol.Frame{data})).transferOptions
	if !explicit.NoBudget {
		t.Fatal("generic explicit local policy was silently cleared")
	}
	intentional := client.resolveSendOptions(multiClientProtocolTransferOpts([]any{DeliverImmediately()}, []*protocol.Frame{data})).transferOptions
	if !intentional.Ack || !intentional.DeliverImmediately || intentional.NoBudget {
		t.Fatal("explicit generic reliable unordered DATA gained a protocol exemption or lost reliability")
	}
}
