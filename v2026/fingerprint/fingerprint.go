// Package fingerprint is the drift-conformance harness for the tls client
// hello (and, as those branches land, the extender carrier hello, the quic
// Initial and the egress syn) of the chrome tcp/udp work.
//
// It exists so a silent drift between what connect emits and what real Chrome
// emits fails a test instead of a censor's filter. The pieces here are
// impl-independent: they need real Chrome and a local endpoint, not connect's
// own dialers, so they can be built and proven on their own.
//
//   - a parser that turns a raw tls ClientHello handshake message into the
//     fields a fingerprint is judged by (fingerprint.go);
//   - a field-diff / drift engine that compares a captured hello against a
//     committed golden, names every drifted field, and normalizes grease so a
//     per-connection grease value is not a false positive while a moved grease
//     slot is a real one (diff.go);
//   - a shared local tls 1.3 endpoint that offers X25519MLKEM768 so a client
//     emits its post-quantum key share, and captures the raw first flight the
//     same way for both real Chrome and connect (endpoint.go);
//   - versioned golden fixtures and their provenance (golden.go, testdata/).
//
// The two layers the harness is split into, and why, are in README.md.
//
// A note on acronyms: this package follows connect/CODESTYLE.md and spells
// acronyms as ordinary words in identifiers (Tls, Alpn, Alps, Sni, Psk, Id,
// Mlkem), diverging from the standard-library spelling that the referenced
// crypto/tls and uTLS identifiers keep.
package fingerprint

import (
	"fmt"

	"golang.org/x/crypto/cryptobyte"
)

// the tls extension type numbers the fingerprint reads (rfc 8446 and the iana
// tls extensions registry). named rather than inlined so the parser and the
// diff read the same.
const (
	extensionServerName        uint16 = 0
	extensionSupportedGroups   uint16 = 10
	extensionSignatureAlgos    uint16 = 13
	extensionAlpn              uint16 = 16
	extensionPreSharedKey      uint16 = 41
	extensionSupportedVersions uint16 = 43
	extensionKeyShare          uint16 = 51
	// draft application settings (alps), the "new" codepoint uTLS and Chrome
	// send; Chrome offers it only beside an offered h2.
	extensionApplicationSettings uint16 = 17613
)

// the tls handshake message type of a ClientHello (rfc 8446 4).
const handshakeTypeClientHello uint8 = 1

// greaseSentinel is the single value every grease codepoint is normalized to
// before a fingerprint is compared (diff.go). rfc 8701 reserves a family of
// values for grease, and a client sends a fresh pick per connection, so a
// literal grease value is noise; its slot in a list is not.
const greaseSentinel uint16 = 0x0a0a

// IsGrease reports whether value is a reserved grease codepoint (rfc 8701):
// the two bytes equal and each a 0x?a nibble pair, i.e. 0x0a0a, 0x1a1a, ...,
// 0xfafa. this is the test connect's own hello tests use.
func IsGrease(value uint16) bool {
	return value&0x0f0f == 0x0a0a && value>>8 == value&0xff
}

// ClientHelloFingerprint is the parsed shape of one tls ClientHandshake
// message, reduced to the fields a network filter judges a client by. the raw
// random is not kept: it carries no fingerprint and changes every connection.
type ClientHelloFingerprint struct {
	// the legacy_version field of the record's hello body (0x0303 for tls 1.2+
	// hellos, which tls 1.3 keeps for middlebox compatibility).
	LegacyVersion uint16
	// the length of the legacy_session_id field. real Chrome sends a 32-byte
	// session id; the extender camouflage hello seals its session into exactly
	// this field, so the length is a fingerprint the carrier-hello comparison
	// will read (that comparison is a placeholder until its branch lands).
	LegacySessionIdLength int
	// cipher_suites in wire order, grease included in place.
	CipherSuites []uint16
	// legacy_compression_methods in wire order (real Chrome sends the single
	// null method).
	CompressionMethods []byte
	// the extension types in wire order, grease included in place. Chrome
	// shuffles the order of most extensions per connection, so the order is
	// not compared directly; the grease and pre_shared_key positions in it
	// are (diff.go).
	ExtensionTypes []uint16
	// supported_groups (named_group_list) in wire order, grease in place.
	SupportedGroups []uint16
	// the groups the key_share extension carries a share for, in wire order,
	// grease in place.
	KeyShareGroups []uint16
	// signature_algorithms in wire order (Chrome sends no grease here).
	SignatureAlgorithms []uint16
	// supported_versions in wire order, grease in place.
	SupportedVersions []uint16
	// the server_name (host_name) the hello carries, empty when absent.
	ServerName string
	// the alpn protocol list in offer order, nil when the extension is absent.
	AlpnProtocols []string
	// the application-settings (alps) protocol list, nil when absent.
	AlpsProtocols []string
	// whether a pre_shared_key extension is present, and whether it is the
	// last extension (where a resuming Chrome always puts it).
	HasPreSharedKey    bool
	PreSharedKeyIsLast bool
	// RecordCount is set by the capture, not the parse: how many tls records
	// the hello arrived in (1 for Chrome and the normal dialer; more for the
	// fragmenting resilient dialer). zero when the fingerprint was parsed from
	// a bare handshake message with no record framing.
	RecordCount int
}

// ParseClientHello parses one tls ClientHello handshake message -- the bytes
// after the 5-byte tls record header, i.e. the handshake type, its 24-bit
// length, and the hello body -- into the fields a fingerprint is judged by. a
// malformed message is an error naming the part that would not parse, never a
// panic.
func ParseClientHello(handshakeMessage []byte) (*ClientHelloFingerprint, error) {
	malformed := func(part string) (*ClientHelloFingerprint, error) {
		return nil, fmt.Errorf("malformed client hello: %s", part)
	}

	input := cryptobyte.String(handshakeMessage)
	var messageType uint8
	var body cryptobyte.String
	if !input.ReadUint8(&messageType) || messageType != handshakeTypeClientHello {
		return malformed("handshake type")
	}
	if !input.ReadUint24LengthPrefixed(&body) || !input.Empty() {
		return malformed("handshake length")
	}

	fingerprint := &ClientHelloFingerprint{}
	var legacyVersion uint16
	var sessionId, cipherSuites, compressionMethods, extensions cryptobyte.String
	if !body.ReadUint16(&legacyVersion) ||
		!body.Skip(32) ||
		!body.ReadUint8LengthPrefixed(&sessionId) ||
		!body.ReadUint16LengthPrefixed(&cipherSuites) ||
		!body.ReadUint8LengthPrefixed(&compressionMethods) ||
		!body.ReadUint16LengthPrefixed(&extensions) ||
		!body.Empty() {
		return malformed("body")
	}
	fingerprint.LegacyVersion = legacyVersion
	fingerprint.LegacySessionIdLength = len(sessionId)
	fingerprint.CompressionMethods = []byte(compressionMethods)

	var err error
	if fingerprint.CipherSuites, err = readUint16List(cipherSuites); err != nil {
		return malformed("cipher suites")
	}

	for !extensions.Empty() {
		var extensionType uint16
		var data cryptobyte.String
		if !extensions.ReadUint16(&extensionType) || !extensions.ReadUint16LengthPrefixed(&data) {
			return malformed("extension header")
		}
		fingerprint.ExtensionTypes = append(fingerprint.ExtensionTypes, extensionType)
		if err := fingerprint.readExtension(extensionType, data); err != nil {
			return nil, err
		}
	}
	if count := len(fingerprint.ExtensionTypes); 0 < count {
		lastType := fingerprint.ExtensionTypes[count-1]
		fingerprint.HasPreSharedKey = sliceContains(fingerprint.ExtensionTypes, extensionPreSharedKey)
		fingerprint.PreSharedKeyIsLast = lastType == extensionPreSharedKey
	}
	return fingerprint, nil
}

// readExtension reads the one extension the fingerprint keeps a field for. an
// extension with no field of its own is recorded by type alone (above) and its
// data ignored here.
func (self *ClientHelloFingerprint) readExtension(extensionType uint16, data cryptobyte.String) error {
	malformed := func(part string) error {
		return fmt.Errorf("malformed client hello: %s", part)
	}
	switch extensionType {
	case extensionServerName:
		var names cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&names) {
			return malformed("server_name list")
		}
		for !names.Empty() {
			var nameType uint8
			var name cryptobyte.String
			if !names.ReadUint8(&nameType) || !names.ReadUint16LengthPrefixed(&name) {
				return malformed("server_name")
			}
			if nameType == 0 {
				self.ServerName = string(name)
			}
		}
	case extensionSupportedGroups:
		var groups cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&groups) {
			return malformed("supported_groups")
		}
		groupValues, err := readUint16List(groups)
		if err != nil {
			return malformed("supported_groups")
		}
		self.SupportedGroups = groupValues
	case extensionKeyShare:
		var shares cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&shares) {
			return malformed("key_share")
		}
		for !shares.Empty() {
			var group uint16
			var key cryptobyte.String
			if !shares.ReadUint16(&group) || !shares.ReadUint16LengthPrefixed(&key) {
				return malformed("key_share entry")
			}
			self.KeyShareGroups = append(self.KeyShareGroups, group)
		}
	case extensionSignatureAlgos:
		var algos cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&algos) {
			return malformed("signature_algorithms")
		}
		algoValues, err := readUint16List(algos)
		if err != nil {
			return malformed("signature_algorithms")
		}
		self.SignatureAlgorithms = algoValues
	case extensionSupportedVersions:
		var versions cryptobyte.String
		if !data.ReadUint8LengthPrefixed(&versions) {
			return malformed("supported_versions")
		}
		versionValues, err := readUint16List(versions)
		if err != nil {
			return malformed("supported_versions")
		}
		self.SupportedVersions = versionValues
	case extensionAlpn:
		protocols, err := readProtocolList(data)
		if err != nil {
			return malformed("alpn")
		}
		self.AlpnProtocols = protocols
	case extensionApplicationSettings:
		protocols, err := readProtocolList(data)
		if err != nil {
			return malformed("application_settings")
		}
		self.AlpsProtocols = protocols
	}
	return nil
}

// readUint16List reads a cryptobyte string as a sequence of uint16s to its
// end, failing on a trailing partial value.
func readUint16List(list cryptobyte.String) ([]uint16, error) {
	var values []uint16
	for !list.Empty() {
		var value uint16
		if !list.ReadUint16(&value) {
			return nil, fmt.Errorf("uint16 list")
		}
		values = append(values, value)
	}
	return values, nil
}

// readProtocolList reads an alpn/alps-shaped list: a uint16-length-prefixed
// list of uint8-length-prefixed protocol names.
func readProtocolList(data cryptobyte.String) ([]string, error) {
	var list cryptobyte.String
	if !data.ReadUint16LengthPrefixed(&list) || !data.Empty() {
		return nil, fmt.Errorf("protocol list")
	}
	protocols := []string{}
	for !list.Empty() {
		var protocol cryptobyte.String
		if !list.ReadUint8LengthPrefixed(&protocol) {
			return nil, fmt.Errorf("protocol")
		}
		protocols = append(protocols, string(protocol))
	}
	return protocols, nil
}

// sliceContains reports whether values holds value. a local helper so the
// parser does not pull in a generic dependency for one membership test.
func sliceContains(values []uint16, value uint16) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
