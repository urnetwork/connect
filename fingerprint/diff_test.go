package fingerprint

// diff_test.go -- the drift engine discriminates. each failure in the task's
// root-cause table has a test here that passes on a matching hello and fails,
// naming the field, on a hello that drifted for that one reason. the mutations
// are deterministic clones, so the discrimination is proven without a capture.

import (
	"slices"
	"strings"
	"testing"
)

// chromeNavigationOptions is the dial-path shape of the golden itself -- a
// Chrome navigation: the endpoint name, h2+http/1.1 alpn, alps, and no ticket.
// a captured hello that matches the golden exactly diffs clean against it.
func chromeNavigationOptions() DiffOptions {
	return DiffOptions{
		ExpectedServerName:        ServerName,
		ExpectedAlpnProtocols:     []string{"h2", "http/1.1"},
		ExpectApplicationSettings: true,
		ExpectPreSharedKey:        false,
	}
}

// loadSyntheticGolden loads the committed golden's fingerprint for a test.
func loadSyntheticGolden(t *testing.T) *ClientHelloFingerprint {
	t.Helper()
	golden, err := LoadGolden(GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}
	return golden.Fingerprint
}

// cloneFingerprint is a deep copy a test mutates to model a drift.
func cloneFingerprint(fingerprint *ClientHelloFingerprint) *ClientHelloFingerprint {
	clone := *fingerprint
	clone.CipherSuites = slices.Clone(fingerprint.CipherSuites)
	clone.CompressionMethods = slices.Clone(fingerprint.CompressionMethods)
	clone.ExtensionTypes = slices.Clone(fingerprint.ExtensionTypes)
	clone.SupportedGroups = slices.Clone(fingerprint.SupportedGroups)
	clone.KeyShareGroups = slices.Clone(fingerprint.KeyShareGroups)
	clone.SignatureAlgorithms = slices.Clone(fingerprint.SignatureAlgorithms)
	clone.SupportedVersions = slices.Clone(fingerprint.SupportedVersions)
	clone.AlpnProtocols = slices.Clone(fingerprint.AlpnProtocols)
	clone.AlpsProtocols = slices.Clone(fingerprint.AlpsProtocols)
	return &clone
}

// driftFields is the set of field names a diff reported, for an assertion that
// names the field rather than only the count.
func driftFields(drifts []Drift) []string {
	fields := make([]string, len(drifts))
	for i, drift := range drifts {
		fields[i] = drift.Field
	}
	return fields
}

// A hello that matches the golden exactly diffs clean: the pass-after state of
// every discrimination test below.
func TestDiffPassesWhenHelloMatchesGolden(t *testing.T) {
	golden := loadSyntheticGolden(t)
	got := cloneFingerprint(golden)
	if drifts := Diff(golden, got, chromeNavigationOptions()); len(drifts) != 0 {
		t.Fatalf("a matching hello drifted: %s", FormatDrift(GoldenChrome133Synthetic, drifts))
	}
}

// Root cause: the uTLS parrot falls behind real Chrome and drops a parrot
// extension. Observable: the extension-set diff names it. (table row 1)
func TestDiffCatchesParrotExtensionDrift(t *testing.T) {
	golden := loadSyntheticGolden(t)
	// drop the first parrot (non-grease, non-dial-path) extension.
	dropped := parrotExtensionSet(golden.ExtensionTypes)[0]
	got := cloneFingerprint(golden)
	got.ExtensionTypes = slices.DeleteFunc(got.ExtensionTypes, func(extensionType uint16) bool {
		return extensionType == dropped
	})

	drifts := Diff(golden, got, chromeNavigationOptions())
	if !slices.Contains(driftFields(drifts), "extension_set") {
		t.Fatalf("dropping extension %04x did not drift the extension set: %s", dropped, FormatDrift(GoldenChrome133Synthetic, drifts))
	}
}

// Root cause: the parrot omits the post-quantum key share. Observable: both the
// supported_groups and the key_share X25519MLKEM768 checks name it. (table row 2)
func TestDiffCatchesMissingPostQuantumKeyShare(t *testing.T) {
	golden := loadSyntheticGolden(t)
	got := cloneFingerprint(golden)
	got.SupportedGroups = slices.DeleteFunc(got.SupportedGroups, func(group uint16) bool { return group == groupX25519Mlkem768 })
	got.KeyShareGroups = slices.DeleteFunc(got.KeyShareGroups, func(group uint16) bool { return group == groupX25519Mlkem768 })

	fields := driftFields(Diff(golden, got, chromeNavigationOptions()))
	for _, want := range []string{"supported_groups.X25519MLKEM768", "key_share_groups.X25519MLKEM768"} {
		if !slices.Contains(fields, want) {
			t.Errorf("dropping the pq key share did not name %s; named %v", want, fields)
		}
	}
}

// Root cause: a brittle grease comparison. Observable: a changed grease value
// does NOT drift (it is masked), a moved grease slot DOES. (table row 3)
func TestDiffMasksGreaseValuesButNotPositions(t *testing.T) {
	golden := loadSyntheticGolden(t)

	// a fresh grease pick in every list, positions unchanged: no drift.
	changedValues := cloneFingerprint(golden)
	regreased := false
	for _, values := range [][]uint16{changedValues.CipherSuites, changedValues.SupportedGroups, changedValues.KeyShareGroups, changedValues.SupportedVersions, changedValues.ExtensionTypes} {
		for i, value := range values {
			if IsGrease(value) {
				// a different, still-valid grease codepoint.
				if value == 0x0a0a {
					values[i] = 0x1a1a
				} else {
					values[i] = 0x0a0a
				}
				regreased = true
			}
		}
	}
	if !regreased {
		t.Fatal("the golden carried no grease to re-pick")
	}
	if drifts := Diff(golden, changedValues, chromeNavigationOptions()); len(drifts) != 0 {
		t.Fatalf("a changed grease value drifted: %s", FormatDrift(GoldenChrome133Synthetic, drifts))
	}

	// grease moved out of the leading cipher-suite slot: a drift.
	movedCipherGrease := cloneFingerprint(golden)
	if len(movedCipherGrease.CipherSuites) < 2 || !IsGrease(movedCipherGrease.CipherSuites[0]) {
		t.Fatal("the golden's cipher suites do not open with grease")
	}
	movedCipherGrease.CipherSuites[0], movedCipherGrease.CipherSuites[1] = movedCipherGrease.CipherSuites[1], movedCipherGrease.CipherSuites[0]
	if !slices.Contains(driftFields(Diff(golden, movedCipherGrease, chromeNavigationOptions())), "cipher_suites") {
		t.Error("moving the cipher-suite grease slot did not drift cipher_suites")
	}

	// grease moved out of the leading extension slot: a structural drift.
	movedExtensionGrease := cloneFingerprint(golden)
	if len(movedExtensionGrease.ExtensionTypes) < 2 || !IsGrease(movedExtensionGrease.ExtensionTypes[0]) {
		t.Fatal("the golden's extensions do not open with grease")
	}
	movedExtensionGrease.ExtensionTypes[0], movedExtensionGrease.ExtensionTypes[1] = movedExtensionGrease.ExtensionTypes[1], movedExtensionGrease.ExtensionTypes[0]
	if !slices.Contains(driftFields(Diff(golden, movedExtensionGrease, chromeNavigationOptions())), "extension_grease_structure") {
		t.Error("moving the extension grease slot did not drift extension_grease_structure")
	}
}

// Root cause: a cipher list reordered (a different Chrome). Observable: the
// cipher diff names it, and a changed grease value in the reordered list is
// still masked. this is the "silent drift from Chrome" of table row 1 at the
// cipher layer.
func TestDiffCatchesCipherOrderDrift(t *testing.T) {
	golden := loadSyntheticGolden(t)
	got := cloneFingerprint(golden)
	// swap two real (non-grease) suites.
	i, j := -1, -1
	for index, value := range got.CipherSuites {
		if IsGrease(value) {
			continue
		}
		if i < 0 {
			i = index
		} else {
			j = index
			break
		}
	}
	if i < 0 || j < 0 {
		t.Fatal("fewer than two real cipher suites to reorder")
	}
	got.CipherSuites[i], got.CipherSuites[j] = got.CipherSuites[j], got.CipherSuites[i]
	if !slices.Contains(driftFields(Diff(golden, got, chromeNavigationOptions())), "cipher_suites") {
		t.Error("reordering cipher suites did not drift cipher_suites")
	}
}

// The dial-path fields are compared against the options, not the golden: a
// wrong server name, alpn, alps presence, or an unexpected ticket each drift.
func TestDiffReportsDialPathDrift(t *testing.T) {
	golden := loadSyntheticGolden(t)

	cases := []struct {
		description string
		opts        DiffOptions
		wantField   string
	}{
		{
			description: "wrong server name",
			opts:        DiffOptions{ExpectedServerName: "other.example", ExpectedAlpnProtocols: []string{"h2", "http/1.1"}, ExpectApplicationSettings: true},
			wantField:   "server_name",
		},
		{
			description: "wrong alpn",
			opts:        DiffOptions{ExpectedServerName: ServerName, ExpectedAlpnProtocols: []string{"http/1.1"}, ExpectApplicationSettings: true},
			wantField:   "alpn",
		},
		{
			description: "alps not expected",
			opts:        DiffOptions{ExpectedServerName: ServerName, ExpectedAlpnProtocols: []string{"h2", "http/1.1"}, ExpectApplicationSettings: false},
			wantField:   "application_settings",
		},
		{
			description: "ticket expected but absent",
			opts:        DiffOptions{ExpectedServerName: ServerName, ExpectedAlpnProtocols: []string{"h2", "http/1.1"}, ExpectApplicationSettings: true, ExpectPreSharedKey: true},
			wantField:   "pre_shared_key",
		},
	}
	for _, c := range cases {
		got := cloneFingerprint(golden)
		if !slices.Contains(driftFields(Diff(golden, got, c.opts)), c.wantField) {
			t.Errorf("%s: did not drift %s", c.description, c.wantField)
		}
	}
}

// The record-count check fires only when the option fixes it (the normal
// dialer and Chrome send one record), and is skipped otherwise (the fragmenting
// dialer reshapes records on purpose).
func TestDiffChecksRecordCountOnlyWhenFixed(t *testing.T) {
	golden := loadSyntheticGolden(t)
	got := cloneFingerprint(golden)
	got.RecordCount = 7

	opts := chromeNavigationOptions()
	if slices.Contains(driftFields(Diff(golden, got, opts)), "record_count") {
		t.Error("record_count drifted with no expected count set")
	}
	opts.ExpectRecordCount = 1
	if !slices.Contains(driftFields(Diff(golden, got, opts)), "record_count") {
		t.Error("record_count did not drift when a single record was expected")
	}
}

// A drift message names the golden's Chrome version and the drifted field, so a
// failure reads as "refresh the 133 golden and the parrot", not as a flake.
func TestFormatDriftNamesGoldenVersionAndField(t *testing.T) {
	golden := loadSyntheticGolden(t)
	got := cloneFingerprint(golden)
	got.SupportedGroups = slices.DeleteFunc(got.SupportedGroups, func(group uint16) bool { return group == groupX25519Mlkem768 })

	message := FormatDrift(GoldenChrome133Synthetic, Diff(golden, got, chromeNavigationOptions()))
	if !strings.Contains(message, "chrome-133") {
		t.Errorf("drift message does not name the golden version: %s", message)
	}
	if !strings.Contains(message, "X25519MLKEM768") {
		t.Errorf("drift message does not name the drifted field: %s", message)
	}
}
