package connect

import (
	"encoding/binary"
	"fmt"
)

// out-of-sequence packet builders for the security paths. these packets are
// built rarely (flow teardown and abuse edges), so they use plain (non pool)
// allocations; callers own the returned packet and never return it to the
// message pool.

// out-of-sequence packet is not aligned with the flow sequence
func ipOosPacket(ipPath *IpPath, payload []byte) []byte {
	switch ipPath.Protocol {
	case IpProtocolUdp:
		return ipOosUdpPacket(ipPath, payload)
	case IpProtocolTcp:
		// seq, ack, and flags are not aligned with any flow state
		return ipOosTcpPacket(ipPath, 0, payload)
	default:
		panic(fmt.Errorf("Bad ip protocol: %d", ipPath.Protocol))
	}
}

// out-of-sequence reset packet for the flow
// if tcp, send rst
// TODO if udp quic, look at what should happen
func ipOosRst(ipPath *IpPath) ([]byte, bool) {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		switch ipPath.Version {
		case 4, 6:
			return ipOosTcpPacket(ipPath, tcpFlagRst, nil), true
		default:
			return nil, false
		}
	default:
		return nil, false
	}
}

// builds a fresh packet in the path direction (source to destination) sized
// for the transport, normalizing the addresses to the wire form for the
// version
func ipTransportPacket(ipPath *IpPath, ipProtocol ipProtocolNumber, transportByteCount int) (packet []byte, transport []byte) {
	switch ipPath.Version {
	case 4:
		packet = make([]byte, Ipv4HeaderSizeWithoutExtensions+transportByteCount)
		writeIpv4Header(packet, ipProtocol, ipPath.SourceIp.To4(), ipPath.DestinationIp.To4())
		transport = packet[Ipv4HeaderSizeWithoutExtensions:]
	case 6:
		packet = make([]byte, Ipv6HeaderSize+transportByteCount)
		writeIpv6Header(packet, ipProtocol, ipPath.SourceIp.To16(), ipPath.DestinationIp.To16())
		transport = packet[Ipv6HeaderSize:]
	default:
		panic(fmt.Errorf("Bad ip version: %d", ipPath.Version))
	}
	return
}

// computes the transport checksum with the wire form of the path addresses
func ipPathTransportChecksum(ipPath *IpPath, ipProtocol ipProtocolNumber, transport []byte) uint16 {
	switch ipPath.Version {
	case 4:
		return transportChecksum(ipProtocol, ipPath.SourceIp.To4(), ipPath.DestinationIp.To4(), transport)
	default:
		return transportChecksum(ipProtocol, ipPath.SourceIp.To16(), ipPath.DestinationIp.To16(), transport)
	}
}

func ipOosUdpPacket(ipPath *IpPath, payload []byte) []byte {
	packet, udp := ipTransportPacket(ipPath, ipProtocolNumberUdp, UdpHeaderSize+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], uint16(ipPath.SourcePort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(ipPath.DestinationPort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	// checksum, set below
	udp[6] = 0
	udp[7] = 0
	copy(udp[UdpHeaderSize:], payload)
	checksum := ipPathTransportChecksum(ipPath, ipProtocolNumberUdp, udp)
	if checksum == 0 {
		// zero means no checksum
		checksum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], checksum)
	return packet
}

func ipOosTcpPacket(ipPath *IpPath, flags byte, payload []byte) []byte {
	packet, tcp := ipTransportPacket(ipPath, ipProtocolNumberTcp, TcpHeaderSizeWithoutExtensions+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], uint16(ipPath.SourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(ipPath.DestinationPort))
	// seq and ack are not aligned with any flow state; zero like the
	// historical builders
	binary.BigEndian.PutUint32(tcp[4:8], 0)
	binary.BigEndian.PutUint32(tcp[8:12], 0)
	// data offset, no options
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 4096)
	// checksum, set below
	tcp[16] = 0
	tcp[17] = 0
	// urgent
	tcp[18] = 0
	tcp[19] = 0
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(ipPath, ipProtocolNumberTcp, tcp))
	return packet
}
