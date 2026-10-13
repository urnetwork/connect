package fingerprint

// diff.go -- the drift engine. it compares a captured ClientHello fingerprint
// against a committed golden and returns every field that drifted, so a test
// that fails names the field and, through FormatDrift, the golden's Chrome
// version. it is the drift gate the owner's "prevent tcp/udp drift" asks for.
//
// two kinds of field, treated differently on purpose:
//
//   - parrot fields come from the uTLS Chrome profile: cipher suites, the
//     supported groups and their key shares (including the post-quantum
//     X25519MLKEM768), signature algorithms, supported versions, the
//     extension set and the grease and pre_shared_key structure. a drift here
//     means the parrot fell behind real Chrome, and the golden is ground
//     truth. these are compared against the golden.
//
//   - dial-path fields are connect's own, never Chrome's: the server name, the
//     alpn list, the application-settings (alps) extension and the
//     pre_shared_key are set per dial path (api offers h2+http/1.1 and alps;
//     the websocket path offers http/1.1 and no alps; a first contact offers
//     no ticket). these are compared against what the caller says the path
//     should present, not against the golden's one capture.
//
// grease (rfc 8701) is normalized, never matched literally: a client sends a
// fresh grease value per connection, so a changed grease value must not fail,
// while a grease codepoint that moved out of its slot must. for the lists
// Chrome sends in a fixed order (ciphers, groups, key shares, versions) the
// grease value is replaced by a sentinel in place, so a moved slot changes the
// normalized sequence. for the extension list, whose order Chrome shuffles per
// connection, the grease slots are checked structurally (grease opens the
// list and closes it before any pre_shared_key) and the rest is compared as a
// set.

import (
	"crypto/tls"
	"fmt"
	"slices"
	"strings"
)

// the post-quantum hybrid key exchange real Chrome and the uTLS Chrome profile
// both offer; its absence is the "parrot omits the pq key share" drift. the
// value is crypto/tls.X25519MLKEM768 (0x11ec).
const groupX25519Mlkem768 = uint16(tls.X25519MLKEM768)

// Drift is one field in which a captured hello differs from the golden (or
// from the dial path it should present). Field names the field, Want and Got
// are its golden/expected and captured forms.
type Drift struct {
	Field string
	Want  string
	Got   string
}

func (self Drift) String() string {
	return fmt.Sprintf("%s: got %s, want %s", self.Field, self.Got, self.Want)
}

// DiffOptions says what dial-path fields the captured hello should present, so
// the diff can tell a parrot drift (always a failure) from connect legitimately
// offering the path's own protocols.
type DiffOptions struct {
	// the server name the hello should carry. empty skips the check.
	ExpectedServerName string
	// the alpn protocols the path offers. nil means the path offers no alpn
	// extension (the protocol-less resilient dialer); a non-empty list means
	// the hello must offer exactly it.
	ExpectedAlpnProtocols []string
	// whether the path offers the application-settings (alps) extension (Chrome
	// does so only beside an offered h2).
	ExpectApplicationSettings bool
	// whether the hello offers a pre_shared_key (a resuming dial does, a first
	// contact does not). when true the pre_shared_key must also be the last
	// extension.
	ExpectPreSharedKey bool
	// the number of tls records the hello should arrive in, when the path
	// fixes it: 1 for the normal dialer, as Chrome sends. zero skips the check
	// (the fragmenting resilient dialer reshapes records on purpose; its
	// reassembled hello is what is compared instead).
	ExpectRecordCount int
}

// the extension types that are dial-path fields, checked on their own rather
// than as part of the parrot extension set.
var dialPathExtensionTypes = []uint16{extensionAlpn, extensionApplicationSettings, extensionPreSharedKey}

// Diff returns every field in which got drifts from golden (a parrot field) or
// from opts (a dial-path field). an empty result is a clean match. the order of
// the result is stable: parrot fields first, then dial-path fields.
func Diff(golden *ClientHelloFingerprint, got *ClientHelloFingerprint, opts DiffOptions) []Drift {
	var drifts []Drift
	add := func(field string, want string, gotValue string) {
		drifts = append(drifts, Drift{Field: field, Want: want, Got: gotValue})
	}

	// parrot fields, grease normalized in place.
	if want, gotValue := normalizeGrease(golden.CipherSuites), normalizeGrease(got.CipherSuites); !slices.Equal(want, gotValue) {
		add("cipher_suites", formatUint16s(want), formatUint16s(gotValue))
	}
	if !slices.Equal(golden.CompressionMethods, got.CompressionMethods) {
		add("compression_methods", fmt.Sprintf("%x", golden.CompressionMethods), fmt.Sprintf("%x", got.CompressionMethods))
	}
	if want, gotValue := normalizeGrease(golden.SupportedGroups), normalizeGrease(got.SupportedGroups); !slices.Equal(want, gotValue) {
		add("supported_groups", formatUint16s(want), formatUint16s(gotValue))
	}
	if want, gotValue := normalizeGrease(golden.KeyShareGroups), normalizeGrease(got.KeyShareGroups); !slices.Equal(want, gotValue) {
		add("key_share_groups", formatUint16s(want), formatUint16s(gotValue))
	}
	// the post-quantum key share, named on its own so a drift that drops it
	// reads as exactly that rather than only as a changed group list.
	if !sliceContains(got.SupportedGroups, groupX25519Mlkem768) {
		add("supported_groups.X25519MLKEM768", "present", "absent")
	}
	if !sliceContains(got.KeyShareGroups, groupX25519Mlkem768) {
		add("key_share_groups.X25519MLKEM768", "present", "absent")
	}
	if !slices.Equal(golden.SignatureAlgorithms, got.SignatureAlgorithms) {
		add("signature_algorithms", formatUint16s(golden.SignatureAlgorithms), formatUint16s(got.SignatureAlgorithms))
	}
	if want, gotValue := normalizeGrease(golden.SupportedVersions), normalizeGrease(got.SupportedVersions); !slices.Equal(want, gotValue) {
		add("supported_versions", formatUint16s(want), formatUint16s(gotValue))
	}

	// the extension set, grease and dial-path extensions set aside. Chrome
	// shuffles the order, so the set is what carries the parrot signal.
	if want, gotValue := parrotExtensionSet(golden.ExtensionTypes), parrotExtensionSet(got.ExtensionTypes); !slices.Equal(want, gotValue) {
		add("extension_set", formatUint16s(want), formatUint16s(gotValue))
	}
	// the grease structure of the extension list: grease opens it and closes
	// it before any pre_shared_key. a moved grease slot fails here even though
	// a changed grease value does not.
	if drift := extensionGreaseStructureDrift(got); drift != nil {
		drifts = append(drifts, *drift)
	}

	// dial-path fields.
	if opts.ExpectedServerName != "" && got.ServerName != opts.ExpectedServerName {
		add("server_name", opts.ExpectedServerName, got.ServerName)
	}
	if !slices.Equal(got.AlpnProtocols, opts.ExpectedAlpnProtocols) {
		add("alpn", fmt.Sprintf("%q", opts.ExpectedAlpnProtocols), fmt.Sprintf("%q", got.AlpnProtocols))
	}
	if hasApplicationSettings := sliceContains(got.ExtensionTypes, extensionApplicationSettings); hasApplicationSettings != opts.ExpectApplicationSettings {
		add("application_settings", fmt.Sprintf("%t", opts.ExpectApplicationSettings), fmt.Sprintf("%t", hasApplicationSettings))
	}
	if got.HasPreSharedKey != opts.ExpectPreSharedKey {
		add("pre_shared_key", fmt.Sprintf("%t", opts.ExpectPreSharedKey), fmt.Sprintf("%t", got.HasPreSharedKey))
	} else if opts.ExpectPreSharedKey && !got.PreSharedKeyIsLast {
		add("pre_shared_key.last", "true", "false")
	}
	if opts.ExpectRecordCount != 0 && got.RecordCount != opts.ExpectRecordCount {
		add("record_count", fmt.Sprintf("%d", opts.ExpectRecordCount), fmt.Sprintf("%d", got.RecordCount))
	}
	return drifts
}

// extensionGreaseStructureDrift returns the drift in the extension list's
// grease structure, or nil when it is as Chrome sends it: a grease codepoint
// opens the list and another closes it, immediately before any trailing
// pre_shared_key.
func extensionGreaseStructureDrift(got *ClientHelloFingerprint) *Drift {
	types := got.ExtensionTypes
	if got.PreSharedKeyIsLast {
		types = types[:len(types)-1]
	}
	if len(types) < 2 || !IsGrease(types[0]) || !IsGrease(types[len(types)-1]) {
		return &Drift{
			Field: "extension_grease_structure",
			Want:  "grease opens the extensions and closes them before any pre_shared_key",
			Got:   formatUint16s(got.ExtensionTypes),
		}
	}
	return nil
}

// parrotExtensionSet is the sorted set of a hello's extension types with the
// grease and dial-path extensions removed, i.e. the parrot-driven extensions
// Chrome's profile fixes.
func parrotExtensionSet(extensionTypes []uint16) []uint16 {
	set := make([]uint16, 0, len(extensionTypes))
	for _, extensionType := range extensionTypes {
		if IsGrease(extensionType) || slices.Contains(dialPathExtensionTypes, extensionType) {
			continue
		}
		set = append(set, extensionType)
	}
	slices.Sort(set)
	return set
}

// normalizeGrease returns a copy of values with every grease codepoint replaced
// by the sentinel, position preserved: a changed grease value normalizes to the
// same sentinel, a moved grease slot changes the sequence.
func normalizeGrease(values []uint16) []uint16 {
	normalized := make([]uint16, len(values))
	for i, value := range values {
		if IsGrease(value) {
			normalized[i] = greaseSentinel
		} else {
			normalized[i] = value
		}
	}
	return normalized
}

// formatUint16s renders a uint16 list as space-separated 4-hex-digit values,
// the form the drift messages and the golden tests read.
func formatUint16s(values []uint16) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprintf("%04x", value)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// FormatDrift renders a set of drifts as a single message that names the
// golden's Chrome version, so a drift failure reads as "refresh the golden and
// the parrot for this version", not as a flake. an empty drift set renders as a
// clean match.
func FormatDrift(golden GoldenRef, drifts []Drift) string {
	if len(drifts) == 0 {
		return fmt.Sprintf("no drift from %s", golden.describe())
	}
	lines := make([]string, 0, len(drifts)+1)
	lines = append(lines, fmt.Sprintf("%d field(s) drifted from %s:", len(drifts), golden.describe()))
	for _, drift := range drifts {
		lines = append(lines, "  "+drift.String())
	}
	return strings.Join(lines, "\n")
}
