package connect

// Verdict reasons name the rule that decided a security policy result: the
// static CFAA layer, the privileged-port skip, a positive standard, the RTP
// probation, the plaintext/budget allowances, or the encrypted heuristic.
//
// Reasons are local diagnostics only. The statistics keyed by reason use the
// port-only destination (version, protocol, port) regardless of the collector's
// includeIp setting, are bounded exactly like the result statistics, and are
// never sent off the device. Nothing on the wire changes.

type SecurityPolicyReason int

const (
	SecurityPolicyReasonUnknown SecurityPolicyReason = iota
	// same-network relationship bypass
	SecurityPolicyReasonNetwork
	// the destination is not public unicast
	SecurityPolicyReasonNotPublic
	// the CFAA ip reputation table matched
	SecurityPolicyReasonCfaaDropIp
	// the CFAA port policy dropped the destination port
	SecurityPolicyReasonCfaaDropPort
	// the CFAA layer allowed without inspection (ntp, ike, dns/udp, icmp, telegram calls)
	SecurityPolicyReasonCfaaAllow
	// a positive BitTorrent signature
	SecurityPolicyReasonBittorrent
	// the fully encrypted heuristic
	SecurityPolicyReasonDropEncrypted
	// the flow is still being inspected (allowed until decided)
	SecurityPolicyReasonInspecting
	// a privileged destination port allowed without stateful inspection
	SecurityPolicyReasonAllowPrivileged
	// a provider-scoped gaming endpoint
	SecurityPolicyReasonAllowGaming
	// web and communication standards
	SecurityPolicyReasonAllowTls
	SecurityPolicyReasonAllowDtls
	SecurityPolicyReasonAllowQuic
	SecurityPolicyReasonAllowStun
	SecurityPolicyReasonAllowTurn
	SecurityPolicyReasonAllowRtcp
	SecurityPolicyReasonAllowRtp
	// a plaintext http request line
	SecurityPolicyReasonAllowHttp
	// an unidentified plaintext protocol
	SecurityPolicyReasonAllowPlaintext
	// the inspection budget was exhausted without a decision
	SecurityPolicyReasonAllowBudget
	// payload inspection is disabled, or the transport is not tcp/udp
	SecurityPolicyReasonAllowUninspected
	// application standards (ip_security_appstandard.go)
	SecurityPolicyReasonAllowWireGuard
	SecurityPolicyReasonAllowOpenVpn
	SecurityPolicyReasonAllowRtmp
	SecurityPolicyReasonAllowLevin
	SecurityPolicyReasonAllowRakNet
	SecurityPolicyReasonAllowEthereumDiscv4
	SecurityPolicyReasonAllowEthereumRlpx

	// one past the last reason
	securityPolicyReasonEnd
)

func (self SecurityPolicyReason) String() string {
	switch self {
	case SecurityPolicyReasonNetwork:
		return "network"
	case SecurityPolicyReasonNotPublic:
		return "not-public"
	case SecurityPolicyReasonCfaaDropIp:
		return "cfaa-drop-ip"
	case SecurityPolicyReasonCfaaDropPort:
		return "cfaa-drop-port"
	case SecurityPolicyReasonCfaaAllow:
		return "cfaa-allow"
	case SecurityPolicyReasonBittorrent:
		return "bittorrent"
	case SecurityPolicyReasonDropEncrypted:
		return "drop-encrypted"
	case SecurityPolicyReasonInspecting:
		return "inspecting"
	case SecurityPolicyReasonAllowPrivileged:
		return "allow-privileged"
	case SecurityPolicyReasonAllowGaming:
		return "allow-gaming"
	case SecurityPolicyReasonAllowTls:
		return "allow-web-standard:tls"
	case SecurityPolicyReasonAllowDtls:
		return "allow-web-standard:dtls"
	case SecurityPolicyReasonAllowQuic:
		return "allow-web-standard:quic"
	case SecurityPolicyReasonAllowStun:
		return "allow-web-standard:stun"
	case SecurityPolicyReasonAllowTurn:
		return "allow-web-standard:turn"
	case SecurityPolicyReasonAllowRtcp:
		return "allow-web-standard:rtcp"
	case SecurityPolicyReasonAllowRtp:
		return "allow-rtp"
	case SecurityPolicyReasonAllowHttp:
		return "allow-http"
	case SecurityPolicyReasonAllowPlaintext:
		return "allow-plaintext"
	case SecurityPolicyReasonAllowBudget:
		return "allow-budget"
	case SecurityPolicyReasonAllowUninspected:
		return "allow-uninspected"
	case SecurityPolicyReasonAllowWireGuard:
		return "allow-app-standard:wireguard"
	case SecurityPolicyReasonAllowOpenVpn:
		return "allow-app-standard:openvpn"
	case SecurityPolicyReasonAllowRtmp:
		return "allow-app-standard:rtmp"
	case SecurityPolicyReasonAllowLevin:
		return "allow-app-standard:levin"
	case SecurityPolicyReasonAllowRakNet:
		return "allow-app-standard:raknet"
	case SecurityPolicyReasonAllowEthereumDiscv4:
		return "allow-app-standard:ethereum-discv4"
	case SecurityPolicyReasonAllowEthereumRlpx:
		return "allow-app-standard:ethereum-rlpx"
	default:
		return "unknown"
	}
}

// SecurityPolicyReasonStats maps a reason to its per-port packet counts.
type SecurityPolicyReasonStats = map[SecurityPolicyReason]map[SecurityDestination]uint64

// securityDecision carries the reason for one inspected packet and whether
// this packet moved its flow from inspecting to a terminal verdict.
type securityDecision struct {
	reason SecurityPolicyReason
	// the flow's terminal verdict was reached by this packet
	decidedNow bool
}
