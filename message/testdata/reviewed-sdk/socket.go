//go:build ignore

// Reviewed IPv4/IPv6 version and destination-address decoding.
package sdk

import "net/netip"

func socketPacketDestination(packet []byte) netip.Addr {
	if len(packet) >= 20 && packet[0]>>4 == 4 {
		return netip.AddrFrom4([4]byte(packet[16:20]))
	}
	if len(packet) >= 40 && packet[0]>>4 == 6 {
		return netip.AddrFrom16([16]byte(packet[24:40]))
	}
	return netip.Addr{}
}
