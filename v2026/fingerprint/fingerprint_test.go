package fingerprint

// fingerprint_test.go -- the parser turns a real uTLS Chrome hello into the
// fields a fingerprint is judged by, and a malformed message into a named
// error, never a panic.

import (
	"slices"
	"testing"
)

// The parser reads the fields of a real uTLS Chrome 133 hello: the suites and
// groups grease-led, the post-quantum key share, the extensions opening and
// closing with grease, the session id Chrome sends and the first-contact
// absence of a pre_shared_key.
func TestParseClientHelloReadsChromeFields(t *testing.T) {
	raw, err := GenerateChromeHello(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := ParseClientHello(raw)
	if err != nil {
		t.Fatal(err)
	}

	if fingerprint.LegacyVersion != 0x0303 {
		t.Errorf("legacy_version = %04x, want 0303", fingerprint.LegacyVersion)
	}
	if fingerprint.LegacySessionIdLength != 32 {
		t.Errorf("legacy_session_id length = %d, want 32", fingerprint.LegacySessionIdLength)
	}
	if len(fingerprint.CipherSuites) == 0 || !IsGrease(fingerprint.CipherSuites[0]) {
		t.Errorf("cipher suites do not open with grease: %s", formatUint16s(fingerprint.CipherSuites))
	}
	if !slices.Equal(fingerprint.CompressionMethods, []byte{0}) {
		t.Errorf("compression methods = %x, want 00", fingerprint.CompressionMethods)
	}
	if !sliceContains(fingerprint.SupportedGroups, groupX25519Mlkem768) {
		t.Errorf("supported groups lack X25519MLKEM768: %s", formatUint16s(fingerprint.SupportedGroups))
	}
	if !sliceContains(fingerprint.KeyShareGroups, groupX25519Mlkem768) {
		t.Errorf("key shares lack X25519MLKEM768: %s", formatUint16s(fingerprint.KeyShareGroups))
	}
	if len(fingerprint.ExtensionTypes) < 2 || !IsGrease(fingerprint.ExtensionTypes[0]) || !IsGrease(fingerprint.ExtensionTypes[len(fingerprint.ExtensionTypes)-1]) {
		t.Errorf("extensions do not open and close with grease: %s", formatUint16s(fingerprint.ExtensionTypes))
	}
	if !slices.Equal(fingerprint.AlpnProtocols, []string{"h2", "http/1.1"}) {
		t.Errorf("alpn = %q, want [h2 http/1.1]", fingerprint.AlpnProtocols)
	}
	if !slices.Equal(fingerprint.AlpsProtocols, []string{"h2"}) {
		t.Errorf("alps = %q, want [h2]", fingerprint.AlpsProtocols)
	}
	if fingerprint.ServerName != ServerName {
		t.Errorf("server name = %q, want %q", fingerprint.ServerName, ServerName)
	}
	if fingerprint.HasPreSharedKey {
		t.Error("a first-contact hello carries a pre_shared_key")
	}
	if len(fingerprint.SupportedVersions) == 0 || !slices.Contains(normalizeGrease(fingerprint.SupportedVersions), uint16(0x0304)) {
		t.Errorf("supported versions do not offer tls 1.3: %s", formatUint16s(fingerprint.SupportedVersions))
	}
}

// A malformed hello is a named error, not a panic: truncation, a wrong
// handshake type, and trailing bytes each fail to parse.
func TestParseClientHelloRejectsMalformed(t *testing.T) {
	raw, err := GenerateChromeHello(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		description string
		message     []byte
	}{
		{description: "empty", message: nil},
		{description: "wrong handshake type", message: append([]byte{0x02}, raw[1:]...)},
		{description: "truncated body", message: raw[:len(raw)-16]},
		{description: "header only", message: raw[:4]},
	}
	for _, c := range cases {
		fingerprint, err := ParseClientHello(c.message)
		if err == nil || fingerprint != nil {
			t.Errorf("%s: parsed a malformed hello without error", c.description)
		}
	}
}
