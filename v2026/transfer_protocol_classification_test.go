package connect

import (
	"encoding/binary"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

func protocolClassTestPacket(ipv6 bool, flags byte, optionBytes, dataBytes int) []byte {
	header := 20
	if ipv6 {
		header = 40
	}
	packet := make([]byte, header+20+optionBytes+dataBytes)
	if ipv6 {
		packet[0], packet[6], packet[7] = 0x60, 6, 64
		binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
	} else {
		packet[0], packet[8], packet[9] = 0x45, 64, 6
		binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	}
	packet[header+12], packet[header+13] = byte((20+optionBytes)/4)<<4, flags
	return packet
}

func protocolClassTestIpv6Extension(packet []byte, kind byte) []byte {
	size := 8
	if kind == 51 {
		size = 12
	}
	result := make([]byte, len(packet)+size)
	copy(result, packet[:40])
	result[6], result[40] = kind, packet[6]
	if kind == 51 {
		result[41] = 1
	}
	copy(result[40+size:], packet[40:])
	binary.BigEndian.PutUint16(result[4:6], uint16(len(result)-40))
	return result
}

func TestProtocolClassPureTcpControlsAndPayloadIsolation(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		for _, flags := range []byte{0x10, 0x02, 0x12, 0x11, 0x04, 0xd2} {
			for _, options := range []int{0, 4, 12, 40} {
				packet := protocolClassTestPacket(ipv6, flags, options, 0)
				if !protocolClassPureTcpControl(packet) {
					t.Fatalf("complete zero-payload TCP control refused: ipv6=%t flags=%x options=%d", ipv6, flags, options)
				}
				for _, payload := range []int{1, 1280} {
					if protocolClassPureTcpControl(protocolClassTestPacket(ipv6, flags, options, payload)) {
						t.Fatalf("payload-bearing TCP flags exempted DATA: ipv6=%t flags=%x payload=%d", ipv6, flags, payload)
					}
				}
			}
		}
	}
	for _, kind := range []byte{0, 43, 60, 44, 51} {
		packet := protocolClassTestIpv6Extension(protocolClassTestPacket(true, 0x10, 12, 0), kind)
		if !protocolClassPureTcpControl(packet) {
			t.Fatalf("bounded complete IPv6 extension refused: %d", kind)
		}
	}
	chain := protocolClassTestIpv6Extension(protocolClassTestIpv6Extension(protocolClassTestPacket(true, 0x10, 0, 0), 60), 0)
	if !protocolClassPureTcpControl(chain) {
		t.Fatal("complete hop-by-hop/destination/TCP chain refused")
	}
}

func TestProtocolClassMalformedFragmentsAndOpaqueRemainData(t *testing.T) {
	base := protocolClassTestPacket(false, 0x10, 0, 0)
	cases := [][]byte{nil, {}, base[:19], base[:39]}
	mutate := func(change func([]byte)) {
		copy := append([]byte(nil), base...)
		change(copy)
		cases = append(cases, copy)
	}
	mutate(func(p []byte) { p[0] = 0x44 })
	mutate(func(p []byte) { p[2], p[3] = 0, 39 })
	mutate(func(p []byte) { p[2], p[3] = 0, 41 })
	mutate(func(p []byte) { p[6] = 0x20 })
	mutate(func(p []byte) { p[7] = 1 })
	mutate(func(p []byte) { p[9] = 17 })
	mutate(func(p []byte) { p[32] = 0x40 })
	mutate(func(p []byte) { p[32] = 0xf0 })
	mutate(func(p []byte) { p[32] |= 0x02 })
	mutate(func(p []byte) { p[33] = 0 })
	mutate(func(p []byte) { p[33] |= 0x20 })
	mutate(func(p []byte) { p[33] |= 0x08 })
	mutate(func(p []byte) { p[39] = 1 })
	v6 := protocolClassTestPacket(true, 0x10, 0, 0)
	badLength := append([]byte(nil), v6...)
	badLength[5]++
	cases = append(cases, badLength)
	for _, fragment := range []uint16{1, 8, 9} {
		p := protocolClassTestIpv6Extension(v6, 44)
		binary.BigEndian.PutUint16(p[42:44], fragment)
		cases = append(cases, p)
	}
	for _, next := range []byte{17, 50, 59, 253} {
		p := append([]byte(nil), v6...)
		p[6] = next
		cases = append(cases, p)
	}
	tooLong := protocolClassTestIpv6Extension(v6, 60)
	tooLong[41] = 255
	cases = append(cases, tooLong, tooLong[:43])
	chain := v6
	for range 9 {
		chain = protocolClassTestIpv6Extension(chain, 60)
	}
	cases = append(cases, chain)
	for index, packet := range cases {
		if protocolClassPureTcpControl(packet) {
			t.Fatalf("malformed, fragmented or opaque DATA classified protocol: %d", index)
		}
	}
}

func TestProtocolClassLegacyMixedAndUnknownFrames(t *testing.T) {
	packet := protocolClassTestPacket(false, 0x10, 12, 0)
	encoded, err := proto.Marshal(&protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}})
	if err != nil {
		t.Fatal(err)
	}
	control := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	legacy := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: encoded}
	if !protocolOnlyFrames([]*protocol.Frame{control, legacy, {MessageType: protocol.MessageType_TransferEncryptedControl}}) {
		t.Fatal("actual raw/legacy pure control group refused")
	}
	for _, frame := range []*protocol.Frame{
		nil,
		{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: protocolClassTestPacket(false, 0x10, 0, 1)},
		{MessageType: protocol.MessageType_Subprotocol, MessageBytes: packet},
		{MessageType: protocol.MessageType_TransferPack, MessageBytes: encoded},
		{MessageType: protocol.MessageType_TestSimpleMessage},
		{MessageType: protocol.MessageType(999)},
		{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: []byte{0x0a, 0xff}},
	} {
		if protocolOnlyFrames([]*protocol.Frame{control, frame}) {
			t.Fatal("mixed/unknown/nested/malformed group inherited protocol classification")
		}
	}
	// Repeated nested messages merge: a later application bytes field must win.
	data, err := proto.Marshal(&protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: protocolClassTestPacket(false, 0x10, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	if protocolOnlyFrame(legacy.MessageType, false, append(encoded, data...)) {
		t.Fatal("legacy merged application payload inherited an earlier ACK classification")
	}
	for _, frame := range []*protocol.Frame{control, legacy} {
		if allocations := testing.AllocsPerRun(100, func() { _ = protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) }); allocations != 0 {
			t.Fatalf("protocol classification allocated: %g", allocations)
		}
	}
}
