package connect

import (
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The provider egress hook, C5 of the sidecar design's connect changes
// (sidecar design §4.6 and §17.6). A provider LocalUserNat asks its hook about
// each new flow, in the flow's own goroutine and before any upstream socket
// exists: pass it to its destination, map it to another address, or deny it.
// Generic, additive and off by default. A NAT without a hook behaves exactly as
// before, and its one cost is a nil check when a flow is created.

// What a hook decides for one flow.
type EgressVerdict int

const (
	// The zero value, so a decision a hook leaves unset refuses its flow. A
	// denied TCP flow gets a reset, a UDP flow an unreachable, and an ICMP echo
	// nothing. A deny is never a security incident and never reports abuse.
	EgressVerdictDeny EgressVerdict = iota
	// Dial the destination as sent.
	EgressVerdictPass
	// Dial `EgressDecision.Target` in place of the destination. Replies keep
	// the original destination as their source, so a map is a destination NAT
	// with no rewrite of any packet.
	EgressVerdictMap
)

func (self EgressVerdict) String() string {
	switch self {
	case EgressVerdictDeny:
		return "deny"
	case EgressVerdictPass:
		return "pass"
	case EgressVerdictMap:
		return "map"
	default:
		return "unknown(" + strconv.Itoa(int(self)) + ")"
	}
}

// A new flow as its hook sees it. The seals are the platform's statements, a
// consistency check only; `Principal` is what the hook itself verified for the
// flow's contract, through `EgressHook.VerifyPrincipal`.
type EgressFlow struct {
	// The source client, as the provider's receive callback named it.
	Source TransferPath
	// The mode the frames that opened the flow arrived under.
	ProvideMode protocol.ProvideMode
	// The contract class C13 sealed; `ContractClassUnset` when the contract
	// sealed none or the flow arrived without a contract.
	ContractClass protocol.ContractClass
	// C13's sealed source subject; zero when unsealed.
	SourceSubject Id
	// C13's sealed source network, in every mode; zero when unsealed.
	SourceNetwork Id
	// The hook's own answer for this flow's contract, from its
	// `WindowDelegation`; nil for none. Not `Peer.Principal`, the contract
	// signer's string.
	Principal *BridgePrincipal
	// The frame that opened the flow came through an end-to-end session whose
	// peer client key the session verified (`Peer.VerifiedClientPublicKey`).
	// A client without `EncryptionSettings.ExposeVerifiedPeerClientKey` never
	// sees a key, so every flow it hosts reads as unsealed.
	Sealed   bool
	Protocol IpProtocol
	// The flow's source and destination as the packet carried them. A
	// v4-mapped v6 address arrives as the v4 address it maps, so a hook judges
	// it as the security policy does (`policyAddress`). ICMP has no ports: both
	// are 0 for an echo, so a rule naming a port never admits one.
	From netip.AddrPort
	To   netip.AddrPort
}

// The verified subject of a contract, from a window client's customer-signed
// delegation (sidecar design §17.9).
type BridgePrincipal struct {
	Subject Id
	Network Id
	// Zero on a same-network link.
	ShareId Id
	// The principal lapses at this time; the hook rechecks it per flow.
	NotAfter time.Time
}

// A hook's answer for one flow.
type EgressDecision struct {
	Verdict EgressVerdict
	// Where a map dials. Port 0 keeps the destination's port. A target that is
	// not a valid address denies the flow, and so does an ICMP map that
	// changes the address family, since an echo cannot cross families.
	Target netip.AddrPort
	// Names the (subject, rule) pair the decision came from, so
	// `LocalUserNat.CloseFlows` can end that pair's flows.
	RuleTag uint64
	// The published policy generation the decision used.
	PolicyGeneration uint64
	// This flow's idle timeout in place of its NAT's. Zero keeps the NAT's.
	IdleTimeout time.Duration
}

// The policy a provider LocalUserNat consults before each new flow. Every
// method may be called from any flow's goroutine at once.
type EgressHook interface {
	// Called once per contract a source presents, in the sequence of the
	// first flow it opens, before that flow's dial. The NAT keeps the answer,
	// nil included, for that source's later flows on the same contract, and
	// asks again on a new contract. A nil answer is asked again only after
	// the published generation has moved. `peer` carries the contract seals,
	// the window delegation and the verified session key of the frame that
	// opened the flow; its roles, principal and delivery are not carried.
	VerifyPrincipal(source TransferPath, peer Peer) *BridgePrincipal
	// Decides one flow, inline: it must not block, wait for I/O or take a
	// lock that a packet path holds.
	ResolveEgress(flow *EgressFlow) EgressDecision
	// The published generation, one atomic load. A flow whose decision used
	// an older one is resolved again before its first byte.
	PolicyGeneration() uint64
	// C18: true hands FriendsAndFamily flows to private destinations to
	// `ResolveEgress` in place of the public-only rules.
	FriendsAndFamilyEgress() bool
}

// The refusal a router answers a packet it will not forward with, addressed
// back to the packet's source as if from its destination: a reset for TCP, and
// for anything else an ICMP destination unreachable, administratively
// prohibited (v4 type 3 code 13, v6 type 1 code 1), quoting the packet's start.
// Nil for a packet that must get no answer (RFC 1122 §3.2.2): a reset, an ICMP
// error, a fragment, a packet from or to an address that is not a single host,
// or anything malformed.
//
// Borrows the packet. The result is a message pool buffer the caller owns.
func AdministrativeRejectPacket(packet []byte) []byte {
	if len(packet) == 0 {
		return nil
	}
	switch packet[0] >> 4 {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		if !ok || !administrativeRejectHosts(sourceIp, destinationIp) {
			return nil
		}
		switch ipProtocol {
		case ipProtocolNumberTcp:
			return tcpRstForPolicyReject(packet)
		case ipProtocolNumberIcmp4:
			if len(transport) < IcmpHeaderSize || icmp4ErrorType(transport[0]) {
				return nil
			}
		}
		// rfc 792: the header and the first 8 bytes of what it carried
		headerByteCount := int(packet[0]&0x0f) * 4
		quoted := packet[:headerByteCount+min(len(transport), icmp4UnreachableQuotedTransport)]
		reply := MessagePoolGet(Ipv4HeaderSizeWithoutExtensions + IcmpHeaderSize + len(quoted))
		writeIpv4Header(reply, ipProtocolNumberIcmp4, destinationIp, sourceIp)
		icmp := reply[Ipv4HeaderSizeWithoutExtensions:]
		icmp[0] = icmp4TypeDestinationUnreachable
		icmp[1] = icmp4CodeAdministrativelyProhibited
		clear(icmp[2:IcmpHeaderSize])
		copy(icmp[IcmpHeaderSize:], quoted)
		binary.BigEndian.PutUint16(icmp[2:4], checksumFinish(checksumAdd(0, icmp)))
		return reply
	case 6:
		ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv6(packet)
		if !ok || !administrativeRejectHosts(sourceIp, destinationIp) {
			return nil
		}
		switch ipProtocol {
		case ipProtocolNumberTcp:
			return tcpRstForPolicyReject(packet)
		case ipProtocolNumberIcmp6:
			// rfc 4443 §2.1: types below 128 are errors
			if len(transport) < IcmpHeaderSize || transport[0] < 128 {
				return nil
			}
		}
		// rfc 4443 §2.4: as much of the packet as fits in the minimum mtu
		quotedByteCount := min(len(packet), icmp6MinMtu-Ipv6HeaderSize-IcmpHeaderSize)
		reply := MessagePoolGet(Ipv6HeaderSize + IcmpHeaderSize + quotedByteCount)
		writeIpv6Header(reply, ipProtocolNumberIcmp6, destinationIp, sourceIp)
		icmp := reply[Ipv6HeaderSize:]
		icmp[0] = icmp6TypeDestinationUnreachable
		icmp[1] = icmp6CodeAdministrativelyProhibited
		clear(icmp[2:IcmpHeaderSize])
		copy(icmp[IcmpHeaderSize:], packet[:quotedByteCount])
		binary.BigEndian.PutUint16(icmp[2:4], transportChecksum(ipProtocolNumberIcmp6, destinationIp, sourceIp, icmp))
		return reply
	default:
		return nil
	}
}

const (
	// rfc 1812 §5.2.7.1
	icmp4CodeAdministrativelyProhibited = byte(13)
	// rfc 4443 §3.1
	icmp6CodeAdministrativelyProhibited = byte(1)
)

// The ICMPv4 error types (rfc 1122 §3.2.2): destination unreachable, source
// quench, redirect, time exceeded and parameter problem. No error answers one.
func icmp4ErrorType(icmpType byte) bool {
	switch icmpType {
	case 3, 4, 5, 11, 12:
		return true
	default:
		return false
	}
}

// Whether a packet's endpoints admit an error answer: the source a single host
// to send it to, the destination a single host that could have refused it.
func administrativeRejectHosts(sourceIp net.IP, destinationIp net.IP) bool {
	single := func(ip net.IP) bool {
		return !ip.IsUnspecified() && !ip.IsMulticast() && !ip.Equal(net.IPv4bcast)
	}
	return single(sourceIp) && single(destinationIp)
}
