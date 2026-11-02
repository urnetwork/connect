package fingerprint

// real_golden_test.go — the committed real-browser golden is a maintained
// fixture, exercised offline (EXTENDER-UPDATE2 §3.4). These tests prove the
// fixture is a genuine modern-Chrome hello and record its measured gap from the
// uTLS parrot the carrier emits, so a maintainer sees when to refresh the
// parrot. They are deterministic and need no network.

import (
	"slices"
	"testing"
)

// The committed real-Chrome golden parses and is a genuine modern-Chrome hello:
// a 32-byte legacy session id (the field the camouflage carrier seals its tag
// into), the post-quantum X25519MLKEM768 key share, grease opening the
// extensions, and an ALPN offer. This is what makes it real-browser evidence
// rather than a synthetic reference (§3.4).
func TestRealChromeGoldenIsAModernBrowserHello(t *testing.T) {
	golden, err := LoadGolden(GoldenChrome156Real)
	if err != nil {
		t.Fatal(err)
	}
	got := golden.Fingerprint
	if got.LegacySessionIdLength != 32 {
		t.Fatalf("legacy session id is %d bytes, want 32 (the carrier seals its tag into this field)", got.LegacySessionIdLength)
	}
	if !sliceContains(got.SupportedGroups, groupX25519Mlkem768) {
		t.Fatal("real Chrome golden does not offer X25519MLKEM768 in supported_groups")
	}
	if !sliceContains(got.KeyShareGroups, groupX25519Mlkem768) {
		t.Fatal("real Chrome golden does not carry an X25519MLKEM768 key share")
	}
	if len(got.ExtensionTypes) < 2 || !IsGrease(got.ExtensionTypes[0]) {
		t.Fatal("real Chrome golden does not open its extensions with grease")
	}
	if !slices.Contains(got.AlpnProtocols, "h2") || !slices.Contains(got.AlpnProtocols, "http/1.1") {
		t.Fatalf("real Chrome golden alpn = %v, want it to offer h2 and http/1.1", got.AlpnProtocols)
	}
}

// Real Chrome opens signature_algorithms with a grease value it picks per
// connection, so two captures of one Chrome differ there. A fresh grease pick
// in that slot is not drift, which is what lets a maintainer diff a refresh
// capture against the committed golden; a grease value moved out of its slot
// still is.
func TestDiffMasksRealChromeSignatureAlgorithmGrease(t *testing.T) {
	golden, err := LoadGolden(GoldenChrome156Real)
	if err != nil {
		t.Fatal(err)
	}
	signatureAlgorithms := golden.Fingerprint.SignatureAlgorithms
	if len(signatureAlgorithms) < 2 || !IsGrease(signatureAlgorithms[0]) {
		t.Fatalf("real Chrome golden signature_algorithms = %v, expected a leading grease value", signatureAlgorithms)
	}
	opts := DiffOptions{
		ExpectedServerName:        golden.Fingerprint.ServerName,
		ExpectedAlpnProtocols:     golden.Fingerprint.AlpnProtocols,
		ExpectApplicationSettings: len(golden.Fingerprint.AlpsProtocols) != 0,
	}

	// another capture of the same Chrome: a different grease pick
	recaptured := cloneFingerprint(golden.Fingerprint)
	if recaptured.SignatureAlgorithms[0] == 0x3a3a {
		recaptured.SignatureAlgorithms[0] = 0x1a1a
	} else {
		recaptured.SignatureAlgorithms[0] = 0x3a3a
	}
	if drifts := Diff(golden.Fingerprint, recaptured, opts); len(drifts) != 0 {
		t.Fatalf("a fresh signature_algorithms grease pick drifted: %s", FormatDrift(GoldenChrome156Real, drifts))
	}

	// the grease moved out of its leading slot
	moved := cloneFingerprint(golden.Fingerprint)
	moved.SignatureAlgorithms[0], moved.SignatureAlgorithms[1] = moved.SignatureAlgorithms[1], moved.SignatureAlgorithms[0]
	if !slices.Contains(driftFields(Diff(golden.Fingerprint, moved, opts)), "signature_algorithms") {
		t.Fatal("moving the signature_algorithms grease slot did not drift signature_algorithms")
	}
}

// The synthetic Chrome_133 parrot is NOT real-browser equivalent: diffing it
// against the real Chrome 156 golden reports drift, which is why the synthetic
// golden is regression coverage only and the real golden is the equivalence
// evidence (§3.4). The core parrot fields a censor keys on first — cipher
// suites, the group list and the post-quantum key share, supported versions,
// and the grease structure — still match, so the gap is confined to
// signature_algorithms and the extension set; this test fails loudly if the
// gap widens into those core fields, which would be a real divergence.
func TestSyntheticParrotIsNotRealBrowserEquivalent(t *testing.T) {
	synthetic, err := LoadGolden(GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}
	real, err := LoadGolden(GoldenChrome156Real)
	if err != nil {
		t.Fatal(err)
	}
	// compare parrot fields only: the real golden's own alpn and name are the
	// dial-path fields, not the parrot's business here
	opts := DiffOptions{
		ExpectedServerName:        real.Fingerprint.ServerName,
		ExpectedAlpnProtocols:     real.Fingerprint.AlpnProtocols,
		ExpectApplicationSettings: len(real.Fingerprint.AlpsProtocols) != 0,
	}
	drifts := Diff(synthetic.Fingerprint, real.Fingerprint, opts)
	if len(drifts) == 0 {
		t.Fatal("the synthetic Chrome_133 parrot matched real Chrome 156 exactly; either the parrot was refreshed (update this test and the carrier parrot) or the golden is stale")
	}
	driftFields := map[string]bool{}
	for _, drift := range drifts {
		driftFields[drift.Field] = true
	}
	// the gap must stay confined to these fields; a drift in a core parrot
	// field is a real divergence that must fail here
	allowed := map[string]bool{"signature_algorithms": true, "extension_set": true}
	for field := range driftFields {
		if !allowed[field] {
			t.Errorf("real-vs-parrot drift reached core field %q: %s", field, FormatDrift(GoldenChrome156Real, drifts))
		}
	}
	t.Logf("measured parrot gap (uTLS Chrome_133 vs real Chrome 156): %s", FormatDrift(GoldenChrome156Real, drifts))
}
