package connect

// Remote ICE candidates. A peer's candidates name the addresses this host
// sends ICE connectivity checks to: STUN binding requests over UDP, and a TCP
// connect for a passive TCP candidate. Accepted as offered, they let any peer,
// a consumer of this provider among them, make this host send packets to
// itself or into its own LAN, the reach the provider-local guard
// (ip_security_local.go) closes for tunneled packets. So the candidates of a
// peer that is not a same-network peer are checked before Pion sees them,
// trickled ones and the candidate lines of the remote description alike: the
// address must be public unicast (ip_security_public.go) and not this host or
// on one of its links. A hostname candidate (mDNS, which the factory disables,
// or a name this host would resolve) is dropped too. A same-network peer
// (ProvideMode_Network: the user's own device) may offer any address, its LAN
// included, so a user's devices on one network still connect directly.

import (
	"net/netip"
	"strings"

	"github.com/pion/ice/v4"
)

// callerOwnsIceTopology reports whether the candidates come from a topology
// the caller built: the same-host loopback-only mode or an injected network,
// both test configurations no production caller sets. Their remote
// candidates are loopback or virtual addresses and are applied as offered.
func (self *WebRtcSettings) callerOwnsIceTopology() bool {
	return self != nil && (self.UseLoopbackOnlyIceInterfaces || self.Network != nil)
}

// remoteIceCandidateAllowed reports whether this host may check a remote
// candidate. An empty candidate is the end of candidates and names no address.
func remoteIceCandidateAllowed(candidate string, trusted bool, networks *providerLocalNetworks) bool {
	if trusted || candidate == "" {
		return true
	}
	parsed, err := ice.UnmarshalCandidate(candidate)
	if err != nil {
		// Pion would refuse it as well
		return false
	}
	addr, err := netip.ParseAddr(parsed.Address())
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return isPublicUnicastAddr(addr) && !networks.containsAddr(addr)
}

// filterRemoteSdpCandidates drops the candidate lines of a remote description
// that remoteIceCandidateAllowed refuses, and keeps every other line as it is.
func filterRemoteSdpCandidates(sdp string, trusted bool, networks *providerLocalNetworks) string {
	if trusted {
		return sdp
	}
	lines := strings.SplitAfter(sdp, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if candidate, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a="); ok &&
			strings.HasPrefix(candidate, "candidate:") &&
			!remoteIceCandidateAllowed(candidate, false, networks) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "")
}
