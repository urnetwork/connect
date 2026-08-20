package connect

import (
	"testing"
)

func TestWebStandardDetection(t *testing.T) {
	d := newWebStandardDetector(DefaultWebStandardSettings())
	cases := []struct {
		name  string
		proto IpProtocol
		b     []byte
		want  bool
	}{
		{"tls", IpProtocolTcp, tlsClientHello(), true},
		{"dtls", IpProtocolUdp, dtlsClientHello(), true},
		{"quic", IpProtocolUdp, quicInitial(), true},
		{"stun", IpProtocolUdp, stunBinding(), true},
		{"random not web", IpProtocolUdp, encryptedPayload(256), false},
		{"bittorrent handshake not web", IpProtocolTcp, btHandshake(), false},
		{"tls bytes over udp not web", IpProtocolUdp, tlsClientHello(), false},
	}
	for _, c := range cases {
		if got := d.match(&IpPath{Protocol: c.proto}, c.b); got != c.want {
			t.Errorf("%s: match = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWebStandardToggles(t *testing.T) {
	// disabling one protocol stops it matching but leaves the others
	s := DefaultWebStandardSettings()
	s.Quic = false
	d := newWebStandardDetector(s)
	if d.match(&IpPath{Protocol: IpProtocolUdp}, quicInitial()) {
		t.Fatal("quic disabled but still matched")
	}
	if !d.match(&IpPath{Protocol: IpProtocolUdp}, stunBinding()) {
		t.Fatal("stun still enabled should match")
	}

	// master switch off => nothing matches
	off := DefaultWebStandardSettings()
	off.Enabled = false
	d2 := newWebStandardDetector(off)
	if d2.match(&IpPath{Protocol: IpProtocolTcp}, tlsClientHello()) {
		t.Fatal("detector disabled but tls matched")
	}
}
