package connect

// ip_remote_user_nat_dial_failure.go — the single-destination client's fast
// dial failure (IPV6.md C7).
//
// A provider whose upstream TCP dial fails answers the source's SYN with an
// ICMP destination-unreachable (ipOosUnreachable). ParseIpPath drops every
// ICMP, so the local stack never heard it: a family the provider cannot dial
// cost the full race fallback delay before the other family started. This
// gate admits exactly that signal and nothing else. The packet must be a
// well-formed, bounded unreachable with the provider's dial-failure code,
// addressed back from the SYN's destination to its source, sent by the
// provider the SYN went to, and its embedded header must name an outstanding
// SYN of this client by 5-tuple. The embedded sequence number is not
// compared: providers embed their flow path, whose sequence is zero.
// Admission consumes that SYN; any reply on the flow clears it. Every other
// ICMP stays dropped. The multi client keeps its own intercept
// (ip_remote_multi_client.go).

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"time"
)

const (
	// A SYN is outstanding only until its first reply, so the table holds
	// concurrent connects. A connect beyond the bound is not tracked and keeps
	// the delayed fallback.
	remoteUserNatPendingSynLimit = 256
	// A SYN older than this is no longer a live connect, and its unreachable
	// is late. This is the default tun dial timeout.
	remoteUserNatPendingSynLifetime = 30 * time.Second
	// The provider's signal is a header, the 8-byte ICMP header and the
	// embedded header with 8 transport bytes. RFC 4443 bounds any ICMPv6
	// error by the IPv6 minimum mtu, and that bound applies to both families.
	remoteUserNatDialFailureMaxSize = 1280
)

// The flow of one outgoing SYN, source to destination.
type remoteUserNatSynKey struct {
	source      netip.AddrPort
	destination netip.AddrPort
}

type remoteUserNatPendingSyn struct {
	provider Id
	expires  time.Time
}

// Outstanding SYNs of one client. Safe for concurrent use; the zero value is
// ready.
type remoteUserNatDialFailures struct {
	stateLock sync.Mutex
	pending   map[remoteUserNatSynKey]remoteUserNatPendingSyn
}

func remoteUserNatSynKeyOf(source []byte, sourcePort int, destination []byte, destinationPort int) (remoteUserNatSynKey, bool) {
	sourceAddr, sourceOk := netip.AddrFromSlice(source)
	destinationAddr, destinationOk := netip.AddrFromSlice(destination)
	if !sourceOk || !destinationOk {
		return remoteUserNatSynKey{}, false
	}
	return remoteUserNatSynKey{
		source:      netip.AddrPortFrom(sourceAddr.Unmap(), uint16(sourcePort)),
		destination: netip.AddrPortFrom(destinationAddr.Unmap(), uint16(destinationPort)),
	}, true
}

// syn records an outgoing bare SYN sent to provider. A retransmission
// refreshes it; any other packet is ignored.
func (self *remoteUserNatDialFailures) syn(ipPath *IpPath, provider Id) {
	if ipPath.Protocol != IpProtocolTcp || !ipPath.Syn || ipPath.Ack {
		return
	}
	key, ok := remoteUserNatSynKeyOf(ipPath.SourceIp, ipPath.SourcePort, ipPath.DestinationIp, ipPath.DestinationPort)
	if !ok {
		return
	}
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pending == nil {
		self.pending = map[remoteUserNatSynKey]remoteUserNatPendingSyn{}
	}
	if _, tracked := self.pending[key]; !tracked && remoteUserNatPendingSynLimit <= len(self.pending) {
		for pendingKey, pending := range self.pending {
			if !now.Before(pending.expires) {
				delete(self.pending, pendingKey)
			}
		}
		if remoteUserNatPendingSynLimit <= len(self.pending) {
			return
		}
	}
	self.pending[key] = remoteUserNatPendingSyn{
		provider: provider,
		expires:  now.Add(remoteUserNatPendingSynLifetime),
	}
}

// reply clears the SYN that an incoming SYN-ACK or reset answers.
func (self *remoteUserNatDialFailures) reply(ipPath *IpPath) {
	if ipPath.Protocol != IpProtocolTcp || !ipPath.Syn && !ipPath.Rst {
		return
	}
	key, ok := remoteUserNatSynKeyOf(ipPath.DestinationIp, ipPath.DestinationPort, ipPath.SourceIp, ipPath.SourcePort)
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.pending, key)
}

// admit reports whether packet, sent by provider, is the dial-failure signal
// for an outstanding SYN, and consumes that SYN. The returned path is the
// failed flow in the ingress direction.
func (self *remoteUserNatDialFailures) admit(provider Id, packet []byte) (*IpPath, bool) {
	if len(packet) == 0 || remoteUserNatDialFailureMaxSize < len(packet) {
		return nil, false
	}
	outerSource, outerDestination, ok := remoteUserNatDialFailureOuter(packet)
	if !ok {
		return nil, false
	}
	egress, ok := ipParseIcmpUnreachable(packet)
	if !ok || egress.Protocol != IpProtocolTcp {
		return nil, false
	}
	key, ok := remoteUserNatSynKeyOf(egress.SourceIp, egress.SourcePort, egress.DestinationIp, egress.DestinationPort)
	if !ok || outerSource != key.destination.Addr() || outerDestination != key.source.Addr() {
		return nil, false
	}
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pending, tracked := self.pending[key]
	if !tracked || pending.provider != provider {
		return nil, false
	}
	delete(self.pending, key)
	if !now.Before(pending.expires) {
		return nil, false
	}
	return egress.Reverse(), true
}

// remoteUserNatDialFailureOuter validates the outer packet of a provider
// dial-failure signal: an exact length, no fragment, verified checksums, and
// the ICMP type and code that ipOosUnreachable writes for a failed TCP dial.
// It returns the outer addresses, which the caller matches to the embed.
func remoteUserNatDialFailureOuter(packet []byte) (source netip.Addr, destination netip.Addr, ok bool) {
	icmp, ok := ipIcmpBody(packet)
	if !ok || len(icmp) < icmpUnreachableHeaderSize {
		return netip.Addr{}, netip.Addr{}, false
	}
	switch packet[0] >> 4 {
	case 4:
		ihl := int(packet[0]&0x0f) * 4
		if int(binary.BigEndian.Uint16(packet[2:4])) != len(packet) || packet[6]&0x3f != 0 || packet[7] != 0 ||
			checksumFinish(checksumAdd(0, packet[:ihl])) != 0 ||
			icmp[0] != icmpv4TypeDestinationUnreachable || icmp[1] != icmpv4CodeHostUnreachable ||
			checksumFinish(checksumAdd(0, icmp)) != 0 {
			return netip.Addr{}, netip.Addr{}, false
		}
		source = netip.AddrFrom4([4]byte(packet[12:16]))
		destination = netip.AddrFrom4([4]byte(packet[16:20]))
	case 6:
		if Ipv6HeaderSize+int(binary.BigEndian.Uint16(packet[4:6])) != len(packet) ||
			icmp[0] != icmpv6TypeDestinationUnreachable || icmp[1] != icmpv6CodeNoRoute ||
			transportChecksum(ipProtocolNumberIcmp6, packet[8:24], packet[24:40], icmp) != 0 {
			return netip.Addr{}, netip.Addr{}, false
		}
		source = netip.AddrFrom16([16]byte(packet[8:24])).Unmap()
		destination = netip.AddrFrom16([16]byte(packet[24:40])).Unmap()
	default:
		return netip.Addr{}, netip.Addr{}, false
	}
	return source, destination, true
}
