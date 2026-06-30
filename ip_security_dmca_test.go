package connect

import (
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func btHandshake() []byte {
	b := make([]byte, 68)
	copy(b, bittorrentHandshakePrefix)
	return b
}

// a deterministic high-entropy payload: every byte value once => uniform
// distribution, popcount exactly 0.5, ~37% printable. Looks like ciphertext.
func encryptedPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}

func tlsClientHello() []byte {
	return []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01, 0x00, 0x00, 0x26}
}

func dtlsClientHello() []byte {
	// 13-byte record header (type, version, epoch, seq, length) then ClientHello(0x01)
	return []byte{0x16, 0xfe, 0xfd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x01}
}

func quicInitial() []byte {
	return []byte{0xc0, 0x00, 0x00, 0x00, 0x01, 0x08, 0xde, 0xad, 0xbe, 0xef}
}

func stunBinding() []byte {
	b := make([]byte, 20)
	b[0], b[1] = 0x00, 0x01 // binding request
	b[2], b[3] = 0x00, 0x00 // length 0
	b[4], b[5], b[6], b[7] = 0x21, 0x12, 0xa4, 0x42
	return b
}

func TestDmcaBittorrentSignatures(t *testing.T) {
	dhtPing := []byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe")
	tracker := append(append([]byte{}, udpTrackerConnectMagic...), 0, 0, 0, 0, 0x12, 0x34, 0x56, 0x78)
	httpTracker := []byte("GET /announce?info_hash=%01%02%03&peer_id=x HTTP/1.1\r\nHost: t\r\n\r\n")
	utp := append(append([]byte{0x01, 0x00}, make([]byte, 18)...), btHandshake()...)

	cases := []struct {
		name  string
		proto IpProtocol
		b     []byte
		want  bool
	}{
		{"tcp handshake", IpProtocolTcp, btHandshake(), true},
		{"tcp http tracker", IpProtocolTcp, httpTracker, true},
		{"udp dht ping", IpProtocolUdp, dhtPing, true},
		{"udp tracker connect", IpProtocolUdp, tracker, true},
		{"udp utp+handshake", IpProtocolUdp, utp, true},
		{"tcp tls not bt", IpProtocolTcp, tlsClientHello(), false},
		{"udp dns not bt", IpProtocolUdp, []byte("\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00"), false},
		{"udp random not bt", IpProtocolUdp, encryptedPayload(256), false},
	}
	for _, c := range cases {
		ipPath := &IpPath{Protocol: c.proto}
		if got := detectBittorrentSignature(ipPath, c.b); got != c.want {
			t.Errorf("%s: detectBittorrentSignature = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDmcaEncryptedHeuristic(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	if !payloadLooksEncrypted(encryptedPayload(256), settings) {
		t.Fatal("uniform random payload should look encrypted")
	}
	text := []byte("GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: test\r\n\r\n")
	if payloadLooksEncrypted(text, settings) {
		t.Fatal("plaintext http should not look encrypted")
	}
	if payloadLooksEncrypted(encryptedPayload(8), settings) {
		t.Fatal("payload below min length should be inconclusive")
	}
}

func dmcaPath(proto IpProtocol, sport int, dport int, syn bool) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        proto,
		SourceIp:        net.ParseIP("10.0.0.2"),
		SourcePort:      sport,
		DestinationIp:   net.ParseIP("8.8.8.8"),
		DestinationPort: dport,
		Syn:             syn,
	}
}

// plaintext bittorrent handshake -> incident on the first data packet
func TestDmcaStateMachineHandshakeIncident(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	d := newDmcaDetector(settings, web)
	if r := d.inspect(dmcaPath(IpProtocolTcp, 40001, 51413, false), btHandshake()); r != SecurityPolicyResultIncident {
		t.Fatalf("handshake -> %v, want incident", r)
	}
}

// encrypted, non-web-standard TCP observed from SYN -> drop after the budget
func TestDmcaStateMachineEncryptedTcpDropped(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	d := newDmcaDetector(settings, web)
	path := dmcaPath(IpProtocolTcp, 40002, 50000, true)
	if r := d.inspect(path, nil); r != SecurityPolicyResultAllow {
		t.Fatalf("syn -> %v, want allow (inspecting)", r)
	}
	data := dmcaPath(IpProtocolTcp, 40002, 50000, false)
	var last SecurityPolicyResult
	for i := 0; i < settings.EncryptedDecisionPackets; i += 1 {
		last = d.inspect(data, encryptedPayload(512))
	}
	if last != SecurityPolicyResultDrop {
		t.Fatalf("encrypted tcp from syn -> %v, want drop", last)
	}
}

// same bytes, but joined mid-stream (no SYN seen) -> must NOT drop
func TestDmcaStateMachineEncryptedTcpMidstreamAllowed(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	d := newDmcaDetector(settings, web)
	data := dmcaPath(IpProtocolTcp, 40003, 50000, false)
	var last SecurityPolicyResult
	for i := 0; i < settings.InspectionPacketBudget+1; i += 1 {
		last = d.inspect(data, encryptedPayload(512))
	}
	if last != SecurityPolicyResultAllow {
		t.Fatalf("encrypted tcp mid-stream -> %v, want allow", last)
	}
}

// encrypted UDP (first datagram is the start) -> drop
func TestDmcaStateMachineEncryptedUdpDropped(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	d := newDmcaDetector(settings, web)
	path := dmcaPath(IpProtocolUdp, 40004, 50000, false)
	var last SecurityPolicyResult
	for i := 0; i < settings.EncryptedDecisionPackets; i += 1 {
		last = d.inspect(path, encryptedPayload(512))
	}
	if last != SecurityPolicyResultDrop {
		t.Fatalf("encrypted udp -> %v, want drop", last)
	}
}

// whitelisted web standard that looks encrypted on later packets stays allowed
func TestDmcaStateMachineQuicAllowed(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	d := newDmcaDetector(settings, web)
	path := dmcaPath(IpProtocolUdp, 40005, 50000, false)
	if r := d.inspect(path, quicInitial()); r != SecurityPolicyResultAllow {
		t.Fatalf("quic initial -> %v, want allow", r)
	}
	// subsequent encrypted 1-RTT packets must remain allowed (terminal)
	if r := d.inspect(path, encryptedPayload(512)); r != SecurityPolicyResultAllow {
		t.Fatalf("quic post-handshake -> %v, want allow", r)
	}
}

// an unknown plaintext protocol is allowed (only encrypted non-web is dropped)
func TestDmcaStateMachinePlaintextAllowed(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	d := newDmcaDetector(settings, web)
	path := dmcaPath(IpProtocolUdp, 40006, 50000, false)
	if r := d.inspect(path, []byte("PLAINTEXT GAME PROTOCOL HELLO v1 ............")); r != SecurityPolicyResultAllow {
		t.Fatalf("plaintext -> %v, want allow", r)
	}
}

// TestDmcaRawHttpShortRequestAllowed: a short HTTP request line (below MinEncryptedPayload)
// observed from SYN is allowed immediately and terminally — so a later high-entropy body on
// the same flow cannot trip the encrypted-traffic heuristic. Without the explicit HTTP
// detection the short request would stay inspecting and be dropped once the high-entropy
// packets arrived.
func TestDmcaRawHttpShortRequestAllowed(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	d := newDmcaDetector(settings, newWebStandardDetector(DefaultWebStandardSettings()))
	if r := d.inspect(dmcaPath(IpProtocolTcp, 40010, 40000, true), nil); r != SecurityPolicyResultAllow {
		t.Fatalf("syn -> %v, want allow", r)
	}
	data := dmcaPath(IpProtocolTcp, 40010, 40000, false)
	if r := d.inspect(data, []byte("GET /a HTTP/1.1\r\n\r\n")); r != SecurityPolicyResultAllow {
		t.Fatalf("short http get -> %v, want allow", r)
	}
	for i := 0; i < settings.EncryptedDecisionPackets+1; i += 1 {
		if r := d.inspect(data, encryptedPayload(512)); r != SecurityPolicyResultAllow {
			t.Fatalf("post-http high-entropy -> %v, want allow (terminal)", r)
		}
	}
}

// TestDmcaHttpTrackerIsBittorrent: an HTTP-tracker GET (info_hash + /announce) is still
// BitTorrent, not allowed as raw HTTP — the HTTP check runs after the BitTorrent signatures.
func TestDmcaHttpTrackerIsBittorrent(t *testing.T) {
	d := newDmcaDetector(DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
	tracker := []byte("GET /announce?info_hash=%01%02&peer_id=x HTTP/1.1\r\nHost: t\r\n\r\n")
	if r := d.inspect(dmcaPath(IpProtocolTcp, 40012, 40000, false), tracker); r != SecurityPolicyResultIncident {
		t.Fatalf("http tracker -> %v, want incident", r)
	}
}

// TestEgressAllowsRawHttpNonStandardPort: end-to-end through the egress policy, CFAA passes
// the ephemeral port to DPI and DPI allows the raw HTTP request.
func TestEgressAllowsRawHttpNonStandardPort(t *testing.T) {
	policy := DefaultEgressSecurityPolicy()
	get := []byte("GET /stream/audio.mp3 HTTP/1.1\r\nHost: stream.example.com\r\n\r\n")
	r, err := policy.Inspect(protocol.ProvideMode_Public, dmcaPath(IpProtocolTcp, 41010, 40000, false), get)
	if err != nil {
		t.Fatal(err)
	}
	if r != SecurityPolicyResultAllow {
		t.Fatalf("raw http on :40000 -> %v, want allow", r)
	}
}

func TestIsHttpRequest(t *testing.T) {
	yes := [][]byte{
		[]byte("GET / HTTP/1.1\r\n"),
		[]byte("POST /x HTTP/1.0\r\n"),
		[]byte("HEAD /y HTTP/1.1\r\nHost: a\r\n"),
	}
	no := [][]byte{
		[]byte("GET /no-version\r\n"),
		[]byte("CONNECT host:443 HTTP/1.1\r\n"), // opaque tunnel, not allowed as raw http
		tlsClientHello(),
		[]byte("random bytes without a method"),
	}
	for _, b := range yes {
		if !isHttpRequest(b) {
			t.Errorf("isHttpRequest(%q) = false, want true", b)
		}
	}
	for _, b := range no {
		if isHttpRequest(b) {
			t.Errorf("isHttpRequest(%q) = true, want false", b)
		}
	}
}

func TestEgressSecurityPolicyDpi(t *testing.T) {
	policy := DefaultEgressSecurityPolicy()

	// bittorrent handshake on a non-bittorrent port (all-ports coverage) -> incident
	r, err := policy.Inspect(protocol.ProvideMode_Public, dmcaPath(IpProtocolTcp, 41001, 51413, false), btHandshake())
	if err != nil {
		t.Fatal(err)
	}
	if r != SecurityPolicyResultIncident {
		t.Fatalf("handshake on 51413 -> %v, want incident", r)
	}

	// tls on a non-standard (>=1024) port -> allow via web-standard detection
	r, _ = policy.Inspect(protocol.ProvideMode_Public, dmcaPath(IpProtocolTcp, 41002, 8443, false), tlsClientHello())
	if r != SecurityPolicyResultAllow {
		t.Fatalf("tls on 8443 -> %v, want allow", r)
	}

	// a privileged destination port (<1024) is allowed without inspection; even a BitTorrent
	// handshake (an incident on a high port) passes
	r, _ = policy.Inspect(protocol.ProvideMode_Public, dmcaPath(IpProtocolTcp, 41005, 443, false), btHandshake())
	if r != SecurityPolicyResultAllow {
		t.Fatalf("bittorrent handshake on privileged port 443 -> %v, want allow", r)
	}

	// known bittorrent port -> drop without payload
	r, _ = policy.Inspect(protocol.ProvideMode_Public, dmcaPath(IpProtocolTcp, 41003, 6881, false), nil)
	if r != SecurityPolicyResultDrop {
		t.Fatalf("port 6881 -> %v, want drop", r)
	}

	// network-mode traffic bypasses inspection entirely
	r, _ = policy.Inspect(protocol.ProvideMode_Network, dmcaPath(IpProtocolTcp, 41004, 51413, false), btHandshake())
	if r != SecurityPolicyResultAllow {
		t.Fatalf("network-mode handshake -> %v, want allow", r)
	}
}
