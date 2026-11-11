package connect

// Remote ICE candidates (transport_p2p_webrtc_candidate.go): what an
// untrusted peer may make this host check, what a same-network peer may, and
// the candidate lines of a remote description.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRemoteIceCandidateAllowed(t *testing.T) {
	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "203.0.113.10/32")
	networks, _ := newTestProviderLocalNetworks(t, read)
	// the snapshot is read by the first guard
	testProviderLocalGuard(t.Context(), networks, 56)

	for _, c := range []struct {
		name      string
		candidate string
		untrusted bool
	}{
		{"a public host candidate", "candidate:1 1 udp 2130706431 198.51.100.7 50000 typ host", true},
		{"a public server-reflexive candidate", "candidate:2 1 udp 1694498815 198.51.100.8 41000 typ srflx raddr 10.0.0.2 rport 50000", true},
		{"a public ipv6 host candidate", "candidate:3 1 udp 2130706431 2001:4860:4860::8888 50000 typ host", true},
		{"a public relay candidate", "candidate:4 1 udp 16777215 198.51.100.9 3478 typ relay raddr 198.51.100.8 rport 41000", true},
		{"end of candidates", "", true},
		{"a lan host candidate", "candidate:5 1 udp 2130706431 192.168.1.1 80 typ host", false},
		{"a lan passive tcp candidate", "candidate:6 1 tcp 1518280447 192.168.1.1 22 typ host tcptype passive", false},
		{"loopback", "candidate:7 1 udp 2130706431 127.0.0.1 6379 typ host", false},
		{"cgnat and tailscale", "candidate:8 1 udp 2130706431 100.100.100.100 53 typ host", false},
		{"link-local ipv6", "candidate:9 1 udp 2130706431 fe80::1 50000 typ host", false},
		{"unique-local ipv6", "candidate:10 1 udp 2130706431 fd00::1 50000 typ host", false},
		{"the cloud metadata endpoint", "candidate:11 1 tcp 1518280447 169.254.169.254 80 typ host tcptype passive", false},
		{"this host's own ipv6", "candidate:12 1 udp 2130706431 2a01:4f8:c17:1234::10 50000 typ host", false},
		{"this host's router", "candidate:13 1 udp 2130706431 2a01:4f8:c17:1234::1 53 typ host", false},
		{"another subnet of this host's /56", "candidate:14 1 udp 2130706431 2a01:4f8:c17:12ab::5 50000 typ host", false},
		{"this host's public ipv4", "candidate:15 1 udp 2130706431 203.0.113.10 8080 typ host", false},
		{"an mdns name", "candidate:16 1 udp 2130706431 0c9f6e5f-3c66-4b46-9d8e-1f2a3b4c5d6e.local 50000 typ host", false},
		{"a host name", "candidate:17 1 udp 2130706431 router.example 50000 typ host", false},
		{"malformed", "candidate:garbage", false},
	} {
		if got := remoteIceCandidateAllowed(c.candidate, false, networks); got != c.untrusted {
			t.Errorf("%s from an untrusted peer: allowed = %t, want %t", c.name, got, c.untrusted)
		}
		// a same-network peer may offer anything, its lan included
		if !remoteIceCandidateAllowed(c.candidate, true, networks) {
			t.Errorf("%s from a same-network peer: refused", c.name)
		}
	}
}

// The candidate lines of a remote description are filtered and every other
// line is kept byte for byte; a same-network peer's description is kept whole.
func TestFilterRemoteSdpCandidates(t *testing.T) {
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t))
	testProviderLocalGuard(t.Context(), networks, 56)

	sdp := strings.Join([]string{
		"v=0",
		"o=- 4611731400430051336 2 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"c=IN IP4 0.0.0.0",
		"a=candidate:1 1 udp 2130706431 192.168.1.1 80 typ host",
		"a=candidate:2 1 udp 1694498815 198.51.100.8 41000 typ srflx raddr 192.168.1.20 rport 50000",
		"a=candidate:3 1 tcp 1518280447 10.0.0.1 22 typ host tcptype passive",
		"a=candidate:4 1 udp 2130706431 2001:4860:4860::8888 50000 typ host",
		"a=end-of-candidates",
		"a=ice-ufrag:abcd",
		"",
	}, "\r\n")
	want := strings.Join([]string{
		"v=0",
		"o=- 4611731400430051336 2 IN IP4 127.0.0.1",
		"s=-",
		"t=0 0",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"c=IN IP4 0.0.0.0",
		"a=candidate:2 1 udp 1694498815 198.51.100.8 41000 typ srflx raddr 192.168.1.20 rport 50000",
		"a=candidate:4 1 udp 2130706431 2001:4860:4860::8888 50000 typ host",
		"a=end-of-candidates",
		"a=ice-ufrag:abcd",
		"",
	}, "\r\n")
	if got := filterRemoteSdpCandidates(sdp, false, networks); got != want {
		t.Fatalf("filtered description:\n%q\nwant\n%q", got, want)
	}
	if got := filterRemoteSdpCandidates(sdp, true, networks); got != sdp {
		t.Fatal("a same-network peer's description changed")
	}
}

// The trust that lets a same-network peer's candidates through is recorded on
// every association of the peer when the manager remembers it, and a later
// association of a remembered peer starts trusted; another peer's never is.
func TestWebRtcManagerTrustsRememberedNetworkPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &WebRtcManager{peerConns: map[peerConnKey]*peerConn{}}
	networkPeerId := NewId()
	publicPeerId := NewId()
	networkConn := &peerConn{ctx: ctx, settings: DefaultWebRtcSettings()}
	publicConn := &peerConn{ctx: ctx, settings: DefaultWebRtcSettings()}
	manager.peerConns[peerConnKey{PeerId: networkPeerId, StreamId: NewId()}] = networkConn
	manager.peerConns[peerConnKey{PeerId: publicPeerId, StreamId: NewId()}] = publicConn

	if manager.trustedNetworkPeerLocked(networkPeerId) || networkConn.remoteCandidatesTrusted() {
		t.Fatal("a peer is trusted before it is remembered")
	}
	manager.rememberNetworkPeerLocked(networkPeerId, time.Now())
	if !networkConn.remoteCandidatesTrusted() {
		t.Fatal("the remembered peer's live association is not trusted")
	}
	if publicConn.remoteCandidatesTrusted() || manager.trustedNetworkPeerLocked(publicPeerId) {
		t.Fatal("another peer became trusted")
	}
	if !manager.trustedNetworkPeerLocked(networkPeerId) {
		t.Fatal("a new association of the remembered peer would not start trusted")
	}
	// a test topology is trusted whatever the peer
	loopback := DefaultWebRtcSettings()
	loopback.UseLoopbackOnlyIceInterfaces = true
	if !(&peerConn{ctx: ctx, settings: loopback}).remoteCandidatesTrusted() {
		t.Fatal("the loopback-only test topology is filtered")
	}
}
