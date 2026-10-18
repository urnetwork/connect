package connect

// Provider-scoped exceptions for legitimate encrypted messaging traffic.
//
// WhatsApp's chat session runs Noise over TCP/5222 to Meta's own edge, and
// after its short framing header the payload is random from the first
// packets: the encrypted heuristic dropped it after three payloads, so
// messages stopped while connected (support inbox 7787). The positive Noise
// detector (whatsAppStream, ip_security_appstandard.go) now recognizes the
// stream's opening bytes on any address. This exception stays as its
// backstop on Meta's own address space, for WhatsApp flows the detector does
// not recognize (a flow first seen mid-stream, an opening that differs from
// the public clients' layout). It has the Steam shape (ip_security_gaming.go):
// the intersection of the vendor's own destination prefixes, the transport,
// and the one port, and never any of the three alone.
//
// Meta sources:
//   - prefixes: metaNetworkPrefixes, generated into
//     ip_security_messaging_meta.go by security/main.go from the route and
//     route6 objects for origin AS32934 in RADb, the registry Meta documents
//     for its address space (`whois -h whois.radb.net -- '-i origin
//     AS32934'`), keeping only the objects Meta maintains itself (RADb
//     MAINT-AS32934, and the RIPE objects of its fb-neteng/facebook-neteng/
//     meta-mnt maintainers), collapsed to their covering prefixes. Objects
//     that third parties registered with origin AS32934 (an ISP-hosted cache,
//     RPKI-only conversions) are left out on purpose. Every release build
//     refreshes it with the CFAA tables, so like them it is identified by
//     SecurityPolicyHash, not by SecurityPolicyRulesGeneration.
//   - port: TCP 5222, the WhatsApp chat port. 5223 is not included until a
//     capture shows WhatsApp using it.
//
// The exception runs after the positive BitTorrent signatures and the
// application standards, and an allowed flow keeps checking the signatures
// for its whole inspection budget (like an application standard), so a
// recognized BitTorrent flow to Meta address space on 5222 is still an
// incident.

import (
	"net/netip"
)

// Provider-scoped messaging exceptions. Use
// DefaultMessagingSecurityPolicySettings for reasonable defaults.
type MessagingSecurityPolicySettings struct {
	// The master switch for every messaging exception.
	Enabled bool

	// Permits TCP/5222 only when the destination is inside Meta's own AS32934
	// address space.
	AllowWhatsApp bool
}

// Every messaging exception enabled, which today is WhatsApp on Meta's own
// address space.
func DefaultMessagingSecurityPolicySettings() *MessagingSecurityPolicySettings {
	return &MessagingSecurityPolicySettings{
		Enabled:       true,
		AllowWhatsApp: true,
	}
}

// The WhatsApp chat port.
const whatsAppChatPort = 5222

// Reports whether the settings admit a flow as a sanctioned messaging
// endpoint, which today is WhatsApp's chat port on Meta's own address space.
// Nil settings admit nothing.
func isSanctionedMessagingEndpoint(
	settings *MessagingSecurityPolicySettings,
	ipPath *IpPath,
) bool {
	return settings != nil && settings.Enabled && settings.AllowWhatsApp &&
		isWhatsAppMetaEndpoint(ipPath)
}

// Reports whether a flow is TCP to the WhatsApp chat port at an address
// inside metaNetworkPrefixes, in the family its IP version names: an
// IPv4-mapped address under version 6 is not one. It does not allocate.
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
