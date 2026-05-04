package connect

import (
	"fmt"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// out-of-sequence packet is not aligned with the flow sequence
func ipOosPacket(ipPath *IpPath, payload []byte) []byte {
	switch ipPath.Version {
	case 4:
		switch ipPath.Protocol {
		case IpProtocolUdp:
			return ip4OosUdpPacket(ipPath, payload)
		case IpProtocolTcp:
			return ip4OosTcpPacket(ipPath, payload)
		default:
			panic(fmt.Errorf("Bad ip protocol: %d", ipPath.Protocol))
		}
	case 6:
		switch ipPath.Protocol {
		case IpProtocolUdp:
			return ip6OosUdpPacket(ipPath, payload)
		case IpProtocolTcp:
			return ip6OosTcpPacket(ipPath, payload)
		default:
			panic(fmt.Errorf("Bad ip protocol: %d", ipPath.Protocol))
		}
	default:
		panic(fmt.Errorf("Bad ip version: %d", ipPath.Version))
	}
}

func ip4OosUdpPacket(ipPath *IpPath, payload []byte) []byte {
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    ipPath.SourceIp.To4(),
		DstIP:    ipPath.DestinationIp.To4(),
		Protocol: layers.IPProtocolUDP,
	}

	udp := layers.UDP{
		SrcPort: layers.UDPPort(ipPath.SourcePort),
		DstPort: layers.UDPPort(ipPath.DestinationPort),
	}
	udp.SetNetworkLayerForChecksum(ip)

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	buffer := gopacket.NewSerializeBufferExpectedSize(128+len(payload), 0)
	err := gopacket.SerializeLayers(buffer, options,
		gopacket.SerializableLayer(ip),
		&udp,
		gopacket.Payload(payload),
	)
	if err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

func ip4OosTcpPacket(ipPath *IpPath, payload []byte) []byte {
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    ipPath.SourceIp.To4(),
		DstIP:    ipPath.DestinationIp.To4(),
		Protocol: layers.IPProtocolTCP,
	}

	tcp := layers.TCP{
		SrcPort: layers.TCPPort(ipPath.SourcePort),
		DstPort: layers.TCPPort(ipPath.DestinationPort),
		Seq:     0,
		Ack:     0,
		Window:  4096,
	}
	tcp.SetNetworkLayerForChecksum(ip)

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	buffer := gopacket.NewSerializeBufferExpectedSize(128+len(payload), 0)
	err := gopacket.SerializeLayers(buffer, options,
		gopacket.SerializableLayer(ip),
		&tcp,
		gopacket.Payload(payload),
	)
	if err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

func ip6OosUdpPacket(ipPath *IpPath, payload []byte) []byte {
	ip := &layers.IPv6{
		Version:    6,
		HopLimit:   64,
		SrcIP:      ipPath.SourceIp.To16(),
		DstIP:      ipPath.DestinationIp.To16(),
		NextHeader: layers.IPProtocolUDP,
	}

	udp := layers.UDP{
		SrcPort: layers.UDPPort(ipPath.SourcePort),
		DstPort: layers.UDPPort(ipPath.DestinationPort),
	}
	udp.SetNetworkLayerForChecksum(ip)

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	buffer := gopacket.NewSerializeBufferExpectedSize(128+len(payload), 0)
	err := gopacket.SerializeLayers(buffer, options,
		gopacket.SerializableLayer(ip),
		&udp,
		gopacket.Payload(payload),
	)
	if err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

func ip6OosTcpPacket(ipPath *IpPath, payload []byte) []byte {
	ip := &layers.IPv6{
		Version:    6,
		HopLimit:   64,
		SrcIP:      ipPath.SourceIp.To16(),
		DstIP:      ipPath.DestinationIp.To16(),
		NextHeader: layers.IPProtocolTCP,
	}

	tcp := layers.TCP{
		SrcPort: layers.TCPPort(ipPath.SourcePort),
		DstPort: layers.TCPPort(ipPath.DestinationPort),
		Seq:     0,
		Ack:     0,
		Window:  4096,
	}
	tcp.SetNetworkLayerForChecksum(ip)

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	buffer := gopacket.NewSerializeBufferExpectedSize(128+len(payload), 0)
	err := gopacket.SerializeLayers(buffer, options,
		gopacket.SerializableLayer(ip),
		&tcp,
		gopacket.Payload(payload),
	)
	if err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

// out-of-sequence reset packet for the flow
// if tcp, send rst
// TODO if udp quic, look at what should happen
func ipOosRst(ipPath *IpPath) ([]byte, bool) {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		switch ipPath.Version {
		case 4:
			return ip4OosTcpRst(ipPath), true
		case 6:
			return ip6OosTcpRst(ipPath), true
		default:
			return nil, false
		}
	default:
		return nil, false
	}
}

func ip4OosTcpRst(ipPath *IpPath) []byte {
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    ipPath.SourceIp.To4(),
		DstIP:    ipPath.DestinationIp.To4(),
		Protocol: layers.IPProtocolTCP,
	}

	tcp := layers.TCP{
		SrcPort: layers.TCPPort(ipPath.SourcePort),
		DstPort: layers.TCPPort(ipPath.DestinationPort),
		Seq:     0,
		Ack:     0,
		RST:     true,
		Window:  4096,
	}
	tcp.SetNetworkLayerForChecksum(ip)

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	buffer := gopacket.NewSerializeBufferExpectedSize(128, 0)
	err := gopacket.SerializeLayers(buffer, options,
		gopacket.SerializableLayer(ip),
		&tcp,
	)
	if err != nil {
		panic(err)
	}
	return buffer.Bytes()
}

func ip6OosTcpRst(ipPath *IpPath) []byte {
	ip := &layers.IPv6{
		Version:    6,
		HopLimit:   64,
		SrcIP:      ipPath.SourceIp.To16(),
		DstIP:      ipPath.DestinationIp.To16(),
		NextHeader: layers.IPProtocolTCP,
	}

	tcp := layers.TCP{
		SrcPort: layers.TCPPort(ipPath.SourcePort),
		DstPort: layers.TCPPort(ipPath.DestinationPort),
		Seq:     0,
		Ack:     0,
		RST:     true,
		Window:  4096,
	}
	tcp.SetNetworkLayerForChecksum(ip)

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	buffer := gopacket.NewSerializeBufferExpectedSize(128, 0)
	err := gopacket.SerializeLayers(buffer, options,
		gopacket.SerializableLayer(ip),
		&tcp,
	)
	if err != nil {
		panic(err)
	}
	return buffer.Bytes()
}
