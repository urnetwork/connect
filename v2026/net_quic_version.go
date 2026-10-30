package connect

import (
	quic "github.com/quic-go/quic-go"
)

// net_quic_version.go -- which QUIC versions a carrier offers and accepts
// (EXTENDER.md A13).
//
// The GFW (since 2024-04) and the TSPU decrypt a QUIC version 1 Initial to read
// the sni and drop the flow on a forbidden name. Both parsers key on the
// version 1 Initial salt (RFC 9001), so a version 2 Initial (RFC 9369, its own
// salt and packet type codes) is not decrypted and the carrier survives in
// both countries as of 2026. quic-go offers `Versions[0]` in the first Initial
// and accepts every version listed, and a server that lists a version the
// client did not offer answers a Version Negotiation packet, on which the
// client re-dials with the first of its own list the server named. So
// offering version 2 first with version 1 behind it reaches a version 1 only
// peer at the cost of one round trip, and a version 1 only client still
// reaches a server that lists both.
//
// Every QUIC carrier of this package takes its offer from one policy: the
// extender udp and dns carriers and the alt api dialers from
// `ConnectSettings.QuicVersionPolicy`, the platform h3 transport from
// `PlatformTransportSettings.QuicVersionPolicy`, and the extender server's
// listener from its own `ExtenderSettings.QuicVersionPolicy`. The policy is
// a kill switch, not a carrier choice: it never changes which carriers are
// raced or their priorities.

// The versions a QUIC carrier offers (a client) or accepts (a server), in
// offer order. The zero value is the default, so a settings struct built
// without its defaults function offers version 2 first all the same. An
// unknown value takes the default as well: a misspelled kill switch must not
// silently disable the carrier.
type QuicVersionPolicy string

const (
	// Version 2 first, version 1 behind it for negotiation. The default.
	QuicVersionPolicyPreferV2 QuicVersionPolicy = "prefer-v2"
	// Version 1 only: the pre-A13 offer, for a peer or a path that cannot
	// carry version 2.
	QuicVersionPolicyV1 QuicVersionPolicy = "v1"
	// Version 2 only: no fallback to the decryptable Initial, so a version 1
	// only peer is unreachable rather than reached in the clear.
	QuicVersionPolicyV2 QuicVersionPolicy = "v2"
)

// The `quic.Config.Versions` of this policy. A client's first Initial carries
// the first entry; a server accepts every entry.
func (self QuicVersionPolicy) Versions() []quic.Version {
	switch self {
	case QuicVersionPolicyV1:
		return []quic.Version{quic.Version1}
	case QuicVersionPolicyV2:
		return []quic.Version{quic.Version2}
	default:
		return []quic.Version{quic.Version2, quic.Version1}
	}
}
