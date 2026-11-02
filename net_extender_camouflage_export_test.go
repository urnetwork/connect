package connect

// Test seams for the external connect_test conformance suite
// (net_tls_hello_conformance_test.go): the exact ClientHello the tcp camouflage
// carrier puts on the wire, built through the real sealing path, so the
// drift-conformance harness can compare it against the committed goldens
// (EXTENDER-UPDATE2 §3.4).

import (
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ExtenderCamouflageCarrierHelloForTest builds the marshaled ClientHello the
// camouflaged tcp attempt sends: the faithful Chrome hello the dialer parrots
// with the authenticated tag sealed into its 32-byte legacy session id, exactly
// as extenderCamouflageClientHandshake does, but without the network handshake.
// It uses a throwaway identity, since the hello's shape — not the extender it
// addresses — is what the harness compares.
func ExtenderCamouflageCarrierHelloForTest(t *testing.T) []byte {
	t.Helper()
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	staticPrivateKey, err := ExtenderRealityStaticPrivateKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	identityPublicKey, err := ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := chromeClientHelloSpec(extenderCamouflageAlpn)
	if err != nil {
		t.Fatal(err)
	}
	// a pipe end as the carrier conn: BuildHandshakeState marshals the hello
	// without any i/o on it
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	uconn := utls.UClient(clientConn, &utls.Config{
		ServerName:             "front.example",
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
		OmitEmptyPsk:           true,
	}, utls.HelloCustom)
	defer uconn.Close()
	if err := uconn.ApplyPreset(spec); err != nil {
		t.Fatal(err)
	}
	if err := sealExtenderCamouflageHello(
		uconn,
		staticPrivateKey.PublicKey(),
		[]byte(identityPublicKey),
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	hello := uconn.HandshakeState.Hello
	return append([]byte(nil), hello.Raw...)
}

// ExtenderCamouflageCarrierAlpnForTest is the alpn the carrier hello offers, so
// the external test compares against the right dial-path value (P4).
func ExtenderCamouflageCarrierAlpnForTest() []string {
	return append([]string(nil), extenderCamouflageAlpn...)
}
