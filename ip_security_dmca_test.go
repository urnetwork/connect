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
		t.Run(c.name, func(t *testing.T) {
			ipPath := &IpPath{Protocol: c.proto}
			if got := detectBittorrentSignature(ipPath, c.b); got != c.want {
				t.Fatalf("detectBittorrentSignature = %v, want %v", got, c.want)
			}
		})
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

func TestDmcaStateMachine(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	web := newWebStandardDetector(DefaultWebStandardSettings())

	// plaintext bittorrent handshake -> incident on the first data packet
	t.Run("handshake incident", func(t *testing.T) {
		d := newDmcaDetector(settings, web)
		if r := d.inspect(dmcaPath(IpProtocolTcp, 40001, 51413, false), btHandshake()); r != SecurityPolicyResultIncident {
			t.Fatalf("handshake -> %v, want incident", r)
		}
	})

	// encrypted, non-web-standard TCP observed from SYN -> drop after the budget
	t.Run("encrypted tcp dropped", func(t *testing.T) {
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
	})

	// same bytes, but joined mid-stream (no SYN seen) -> must NOT drop
	t.Run("encrypted tcp midstream allowed", func(t *testing.T) {
		d := newDmcaDetector(settings, web)
		data := dmcaPath(IpProtocolTcp, 40003, 50000, false)
		var last SecurityPolicyResult
		for i := 0; i < settings.InspectionPacketBudget+1; i += 1 {
			last = d.inspect(data, encryptedPayload(512))
		}
		if last != SecurityPolicyResultAllow {
			t.Fatalf("encrypted tcp mid-stream -> %v, want allow", last)
		}
	})

	// encrypted UDP (first datagram is the start) -> drop
	t.Run("encrypted udp dropped", func(t *testing.T) {
		d := newDmcaDetector(settings, web)
		path := dmcaPath(IpProtocolUdp, 40004, 50000, false)
		var last SecurityPolicyResult
		for i := 0; i < settings.EncryptedDecisionPackets; i += 1 {
			last = d.inspect(path, encryptedPayload(512))
		}
		if last != SecurityPolicyResultDrop {
			t.Fatalf("encrypted udp -> %v, want drop", last)
		}
	})

	// whitelisted web standard that looks encrypted on later packets stays allowed
	t.Run("quic allowed", func(t *testing.T) {
		d := newDmcaDetector(settings, web)
		path := dmcaPath(IpProtocolUdp, 40005, 50000, false)
		if r := d.inspect(path, quicInitial()); r != SecurityPolicyResultAllow {
			t.Fatalf("quic initial -> %v, want allow", r)
		}
		// subsequent encrypted 1-RTT packets must remain allowed (terminal)
		if r := d.inspect(path, encryptedPayload(512)); r != SecurityPolicyResultAllow {
			t.Fatalf("quic post-handshake -> %v, want allow", r)
		}
	})

	// an unknown plaintext protocol is allowed (only encrypted non-web is dropped)
	t.Run("plaintext allowed", func(t *testing.T) {
		d := newDmcaDetector(settings, web)
		path := dmcaPath(IpProtocolUdp, 40006, 50000, false)
		if r := d.inspect(path, []byte("PLAINTEXT GAME PROTOCOL HELLO v1 ............")); r != SecurityPolicyResultAllow {
			t.Fatalf("plaintext -> %v, want allow", r)
		}
	})
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

	// tls on 443 -> allow
	r, _ = policy.Inspect(protocol.ProvideMode_Public, dmcaPath(IpProtocolTcp, 41002, 443, false), tlsClientHello())
	if r != SecurityPolicyResultAllow {
		t.Fatalf("tls on 443 -> %v, want allow", r)
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
