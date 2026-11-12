package connect

import (
	"slices"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestIpPacketBytesRejectMissingLegacyPacket(t *testing.T) {
	for _, direction := range []struct {
		name string
		kind protocol.MessageType
		read func(*protocol.Frame) ([]byte, error)
	}{
		{"to provider", protocol.MessageType_IpIpPacketToProvider, ipPacketToProviderBytes},
		{"from provider", protocol.MessageType_IpIpPacketFromProvider, ipPacketFromProviderBytes},
	} {
		t.Run(direction.name, func(t *testing.T) {
			for _, message := range [][]byte{nil, {}, {0x10, 0x01}, {0x0a, 0xff}} {
				packet, err := direction.read(&protocol.Frame{MessageType: direction.kind, MessageBytes: message})
				if err == nil || packet != nil {
					t.Fatalf("legacy bytes %x: packet = %x, err = %v; want rejection", message, packet, err)
				}
			}
		})
	}
}

func TestIpPacketBytesPreserveLegacyAndRawPackets(t *testing.T) {
	assertMessagePoolOwnership(t)
	packet := []byte{0x45, 0, 0, 20}
	for _, version := range []int{1, 2} {
		for _, direction := range []struct {
			build func([]byte, int) (*protocol.Frame, error)
			read  func(*protocol.Frame) ([]byte, error)
		}{
			{ipPacketToProviderFrame, ipPacketToProviderBytes},
			{ipPacketFromProviderFrame, ipPacketFromProviderBytes},
		} {
			frame, err := direction.build(packet, version)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := direction.read(frame)
			MessagePoolReturn(frame.MessageBytes)
			if err != nil || !slices.Equal(decoded, packet) {
				t.Fatalf("version %d: packet = %x, err = %v", version, decoded, err)
			}
		}
	}
}
