package connect

// Provider-scoped exceptions for legitimate encrypted messaging traffic.
//
// WhatsApp's chat session runs Noise over TCP/5222 to Meta's own edge, and
// after its short framing header the payload is random from the first
// packets: the encrypted heuristic dropped it after three payloads, so
// messages stopped while connected (support inbox 7787). A positive Noise "WA"
// framing detector needs a packet capture first. Until then the exception is
// the Steam shape (ip_security_gaming.go): the intersection of the vendor's
// own destination prefixes, the transport, and the one port, and never any of
// the three alone.
//
// Meta sources, snapshot 2026-10-04:
//   - prefixes: the route and route6 objects for origin AS32934 in RADb, the
//     registry Meta documents for its address space
//     (`whois -h whois.radb.net -- '-i origin AS32934'`), keeping only the
//     objects Meta maintains itself (RADb MAINT-AS32934, and the RIPE objects
//     of its fb-neteng/facebook-neteng/meta-mnt maintainers), collapsed to
//     their covering prefixes. Objects that third parties registered with
//     origin AS32934 (an ISP-hosted cache, RPKI-only conversions) are left
//     out on purpose.
//   - port: TCP 5222, the WhatsApp chat port. 5223 is not included until a
//     capture shows WhatsApp using it.
//
// The exception runs after the positive BitTorrent signatures, and an
// allowed flow keeps checking them for its whole inspection budget (like an
// application standard), so a recognized BitTorrent flow to Meta address
// space on 5222 is still an incident.

import (
	"net/netip"
)

// MessagingSecurityPolicySettings controls provider-scoped messaging
// exceptions. Use DefaultMessagingSecurityPolicySettings for reasonable
// defaults.
type MessagingSecurityPolicySettings struct {
	// Enabled is the master switch for every messaging exception.
	Enabled bool

	// AllowWhatsApp permits TCP/5222 only when the destination is inside
	// Meta's own AS32934 address space.
	AllowWhatsApp bool
}

func DefaultMessagingSecurityPolicySettings() *MessagingSecurityPolicySettings {
	return &MessagingSecurityPolicySettings{
		Enabled:       true,
		AllowWhatsApp: true,
	}
}

// The WhatsApp chat port.
const whatsAppChatPort = 5222

// Masked, collapsed snapshot of the AS32934 route objects Meta maintains.
// Keep these as prefixes rather than expanding them into individual addresses.
var metaNetworkPrefixes = [...]netip.Prefix{
	// IPv4
	netip.MustParsePrefix("31.13.24.0/21"),
	netip.MustParsePrefix("31.13.64.0/18"),
	netip.MustParsePrefix("45.64.40.0/22"),
	netip.MustParsePrefix("57.141.0.0/20"),
	netip.MustParsePrefix("57.141.16.0/21"),
	netip.MustParsePrefix("57.141.24.0/23"),
	netip.MustParsePrefix("57.144.0.0/14"),
	netip.MustParsePrefix("66.220.144.0/20"),
	netip.MustParsePrefix("69.63.176.0/20"),
	netip.MustParsePrefix("69.171.224.0/19"),
	netip.MustParsePrefix("74.119.76.0/22"),
	netip.MustParsePrefix("102.132.96.0/20"),
	netip.MustParsePrefix("103.4.96.0/22"),
	netip.MustParsePrefix("129.134.0.0/16"),
	netip.MustParsePrefix("147.75.208.0/20"),
	netip.MustParsePrefix("157.240.0.0/16"),
	netip.MustParsePrefix("163.70.128.0/17"),
	netip.MustParsePrefix("163.77.128.0/17"),
	netip.MustParsePrefix("173.252.64.0/18"),
	netip.MustParsePrefix("179.60.192.0/22"),
	netip.MustParsePrefix("185.60.216.0/22"),
	netip.MustParsePrefix("185.89.216.0/22"),
	netip.MustParsePrefix("204.15.20.0/22"),

	// IPv6
	netip.MustParsePrefix("2401:db00::/32"),
	netip.MustParsePrefix("2620:0:1c00::/40"),
	netip.MustParsePrefix("2a03:2880::/31"),
	netip.MustParsePrefix("2a03:2887:ff2c::/47"),
	netip.MustParsePrefix("2a03:83e0::/32"),
}

func isSanctionedMessagingEndpoint(
	settings *MessagingSecurityPolicySettings,
	ipPath *IpPath,
) bool {
	return settings != nil && settings.Enabled && settings.AllowWhatsApp &&
		isWhatsAppMetaEndpoint(ipPath)
}

func isWhatsAppMetaEndpoint(ipPath *IpPath) bool {
	if ipPath == nil || ipPath.Protocol != IpProtocolTcp || ipPath.DestinationPort != whatsAppChatPort {
		return false
	}
	address, ok := netip.AddrFromSlice(ipPath.DestinationIp)
	if !ok {
		return false
	}
	address = address.Unmap()
	switch ipPath.Version {
	case 4:
		if !address.Is4() {
			return false
		}
	case 6:
		if !address.Is6() || address.Is4In6() {
			return false
		}
	default:
		return false
	}
	for _, prefix := range metaNetworkPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
