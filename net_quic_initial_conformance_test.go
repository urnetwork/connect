package connect

// net_quic_initial_conformance_test.go -- Layer A of the fingerprint-drift
// harness for the udp carriers (fingerprint/README.md, net_quic_version.go).
// it drives connect's merged QuicVersionPolicy to the wire -- the same
// `policy.Versions()` the extender udp, the alt api dialers and the platform h3
// transport hand quic-go -- against the shared QUIC endpoint, captures the
// first Initial datagram, and asserts its long-header version is the one the
// policy offers first. a silent regression of the default to a version 1
// Initial, which the GFW/TSPU can decrypt and filter by sni (A13), fails here
// and names the version.
//
// the transport parameters and the CRYPTO-frame layout the task also names live
// inside the aead-encrypted Initial; reading them needs the Initial keys
// derived from the version salt and the destination connection id. that is the
// next increment (fingerprint/README.md); the version, the packet-type code and
// the 1200-byte padding are observable here without it and are the primary udp
// drift gate.

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/fingerprint"
)

// dialAndCaptureQuicInitial dials the shared QUIC endpoint with a quic.Config
// whose version offer is exactly connect's policy, and returns the first
// Initial datagram the endpoint captured, parsed.
func dialAndCaptureQuicInitial(t *testing.T, policy QuicVersionPolicy) *fingerprint.QuicInitialFingerprint {
	t.Helper()
	endpoint, err := fingerprint.NewQuicEndpoint(fingerprint.QuicEndpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endpoint.Close)

	clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: clientConn}
	t.Cleanup(func() { transport.Close(); clientConn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// the one line that carries the merged policy to the wire, as the extender
	// udp carrier (net_extender_memory.go) and the alt dialer (net_http_alt.go)
	// set it.
	quicConfig := &quic.Config{Versions: policy.Versions()}
	tlsConfig := &tls.Config{
		ServerName: fingerprint.ServerName,
		RootCAs:    endpoint.CaCertPool(),
		NextProtos: []string{"h3"},
	}
	conn, err := transport.Dial(ctx, endpoint.Addr(), tlsConfig, quicConfig)
	if err != nil {
		t.Fatalf("%s: quic dial: %s", policy, err)
	}
	_ = conn.CloseWithError(quic.ApplicationErrorCode(0), "")

	initials := endpoint.CapturedInitials()
	if len(initials) == 0 {
		t.Fatalf("%s: no quic initial captured", policy)
	}
	fingerprintValue, err := fingerprint.ParseQuicInitial(initials[0])
	if err != nil {
		t.Fatal(err)
	}
	return fingerprintValue
}

// Each QuicVersionPolicy puts its first offered version on the wire in the
// first Initial: prefer-v2 and v2-only send a version 2 Initial, v1-only a
// version 1 Initial. the capture also drifts against the other version, so the
// gate catches a regression either way.
func TestConnectQuicPolicyOffersPolicyVersionInInitial(t *testing.T) {
	cases := []struct {
		description string
		policy      QuicVersionPolicy
		wantVersion uint32
	}{
		{description: "prefer-v2", policy: QuicVersionPolicyPreferV2, wantVersion: fingerprint.QuicVersion2},
		{description: "v2 only", policy: QuicVersionPolicyV2, wantVersion: fingerprint.QuicVersion2},
		{description: "v1 only", policy: QuicVersionPolicyV1, wantVersion: fingerprint.QuicVersion1},
	}
	for _, c := range cases {
		got := dialAndCaptureQuicInitial(t, c.policy)
		if drifts := fingerprint.DiffQuicInitial(got, fingerprint.QuicDiffOptions{ExpectedVersion: c.wantVersion}); len(drifts) != 0 {
			t.Errorf("%s: captured initial drifted from the policy's version: %v", c.description, drifts)
		}
		otherVersion := fingerprint.QuicVersion2
		if c.wantVersion == fingerprint.QuicVersion2 {
			otherVersion = fingerprint.QuicVersion1
		}
		if drifts := fingerprint.DiffQuicInitial(got, fingerprint.QuicDiffOptions{ExpectedVersion: otherVersion}); len(drifts) == 0 {
			t.Errorf("%s: captured initial did not drift against the other version, so the gate would miss a version regression", c.description)
		}
	}
}

// The shipped default (DefaultConnectSettings, prefer-v2) puts a version 2
// Initial on the wire -- the whole point of A13, so a version 1 regression the
// GFW/TSPU can decrypt is caught. this is the pass-after; the v1-only case above
// is the faithful pre-A13 revert whose Initial drifts against the expected
// version 2.
func TestConnectQuicDefaultInitialIsVersion2(t *testing.T) {
	policy := DefaultConnectSettings().QuicVersionPolicy
	if policy != QuicVersionPolicyPreferV2 {
		t.Fatalf("default quic version policy = %q, want prefer-v2", policy)
	}
	got := dialAndCaptureQuicInitial(t, policy)
	if got.Version != fingerprint.QuicVersion2 {
		t.Fatalf("the default policy's first Initial version = %08x, want version 2 (%08x)", got.Version, fingerprint.QuicVersion2)
	}
	if got.PacketType != fingerprint.QuicPacketInitial || !got.LongHeader {
		t.Fatalf("the default policy's first datagram is not a long-header Initial: %+v", got)
	}
	if got.DatagramLength < 1200 {
		t.Fatalf("the default policy's Initial is padded to %d bytes, want >= 1200", got.DatagramLength)
	}
}
