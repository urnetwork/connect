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
// udp has no in-band reset; see `ipOosUnreachable`
//
// The reset carries sequence 0, which the exit accepts (it tears down on any
// rst for a known bufferId, with no sequence check) but a real tcp stack does
// not. Toward the source, use `ipOosRstSequence`.
func ipOosRst(ipPath *IpPath) ([]byte, bool) {
	return ipOosRstSequence(ipPath, 0)
}

// out-of-sequence reset carrying an explicit sequence number.
//
// RFC 5961 section 3.2, and `tcp_validate_incoming` in linux, accept a reset
// only when SEG.SEQ is exactly RCV.NXT -- anything else earns a challenge ack
// and the reset is ignored. Sequence 0 essentially never matches an established
// connection, so a reset built by `ipOosRst` is silently discarded by the
// source's stack, and the flow freezes rather than resetting.
//
// The value the source needs is the sequence it expects next from the
// destination, which is the ack number it has been sending -- tracked per flow
// as `multiClientChannelUpdate.ackSequenceNumber`.
func ipOosRstSequence(ipPath *IpPath, sequenceNumber uint32) ([]byte, bool) {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		switch ipPath.Version {
		case 4, 6:
			return ipOosTcpPacketSequence(ipPath, tcpFlagRst, sequenceNumber, nil), true
		default:
			return nil, false
		}
	default:
		return nil, false
	}
}

const (
	// type, code, checksum, unused
	icmpUnreachableHeaderSize = 8

	icmpv4TypeDestinationUnreachable = 3
	// port unreachable, delivered as ECONNREFUSED.
	//
	// This was host unreachable (code 1) first, on the reasoning that the port
	// is fine and only the path died. That is semantically truer and it does
	// not work: linux maps ICMP_HOST_UNREACH to {EHOSTUNREACH, fatal=0} in
	// icmp_err_convert, and __udp4_lib_err discards a non-fatal error on any
	// socket without IP_RECVERR -- `if (!harderr || ...) goto out`. Chrome's
	// quic sockets and the android resolver do not set IP_RECVERR, so the
	// signal never reached the application on the one platform reporting the
	// freeze. Port unreachable is {ECONNREFUSED, fatal=1} and is delivered.
	//
	// The original worry -- that ECONNREFUSED would make a resolver mark the
	// tunnel's fixed dns address dead -- does not apply: IpMux.Receive
	// short-circuits isLocalDestination traffic to the internal stack, so the
	// mux-terminated resolver flows never reach this teardown path at all.
	icmpv4CodePortUnreachable = 3

	icmpv6TypeDestinationUnreachable = 1
	// the v6 equivalent, also ECONNREFUSED and also fatal. v6 has no fatal
	// EHOSTUNREACH at all, so matching errno across families and being
	// delivered on both is only possible here.
	icmpv6CodePortUnreachable = 4
)

// out-of-sequence destination-unreachable for a udp flow whose exit went away.
//
// tcp flows get `ipOosRst`, so the application learns immediately and
// reconnects. udp has no equivalent in-band signal, so a udp flow whose exit is
// removed goes silent and stalls until the application's own timeout: a dns
// query that never returns, or a quic session (udp 443) that hangs instead of
// falling back to tcp. an icmp destination-unreachable gives udp the same
// prompt "this path is gone" notice tcp already gets.
//
// the code is port unreachable, chosen for delivery over semantics -- see the
// constants above. an undelivered signal is worth nothing, and on linux only
// the fatal codes reach a socket that has not opted into IP_RECVERR.
//
// `ipPath` is the flow's own direction (source to destination). the returned
// packet is addressed back the other way, as if from the destination.
func ipOosUnreachable(ipPath *IpPath) ([]byte, bool) {
	if ipPath.Protocol != IpProtocolUdp {
		return nil, false
	}

	// check the version before building anything: ipOosUdpPacket panics on an
	// unsupported version, and this must return false the way ipOosRst does
	switch ipPath.Version {
	case 4, 6:
	default:
		return nil, false
	}

	// the datagram the error refers to: the ip header plus the 8 transport
	// bytes rfc 792 requires. rfc 4443 permits more for v6, but the same 8
	// are what the source matches against its socket.
	embedded := ipOosUdpPacket(ipPath, nil)
	reverse := ipPath.Reverse()

	writeHeader := func(icmp []byte, icmpType byte, code byte) {
		icmp[0] = icmpType
		icmp[1] = code
		// checksum, set by the caller
		icmp[2] = 0
		icmp[3] = 0
		// unused
		binary.BigEndian.PutUint32(icmp[4:8], 0)
		copy(icmp[icmpUnreachableHeaderSize:], embedded)
	}

	switch ipPath.Version {
	case 4:
		packet, icmp := ipTransportPacket(reverse, ipProtocolNumberIcmpv4, icmpUnreachableHeaderSize+len(embedded))
		writeHeader(icmp, icmpv4TypeDestinationUnreachable, icmpv4CodePortUnreachable)
		// icmpv4 checksums the message alone, with no pseudo header
		binary.BigEndian.PutUint16(icmp[2:4], checksumFinish(checksumAdd(0, icmp)))
		return packet, true
	case 6:
		packet, icmp := ipTransportPacket(reverse, ipProtocolNumberIcmpv6, icmpUnreachableHeaderSize+len(embedded))
		writeHeader(icmp, icmpv6TypeDestinationUnreachable, icmpv6CodePortUnreachable)
		// icmpv6 checksums with the ipv6 pseudo header, like tcp and udp
		binary.BigEndian.PutUint16(icmp[2:4], transportChecksum(
			ipProtocolNumberIcmpv6,
			reverse.SourceIp.To16(),
			reverse.DestinationIp.To16(),
			icmp,
		))
		return packet, true
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
	return ipOosTcpPacketSequence(ipPath, flags, 0, payload)
}

func ipOosTcpPacketSequence(ipPath *IpPath, flags byte, sequenceNumber uint32, payload []byte) []byte {
	packet, tcp := ipTransportPacket(ipPath, ipProtocolNumberTcp, TcpHeaderSizeWithoutExtensions+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], uint16(ipPath.SourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(ipPath.DestinationPort))
	// ack is not aligned with any flow state; zero like the historical builders
	binary.BigEndian.PutUint32(tcp[4:8], sequenceNumber)
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
