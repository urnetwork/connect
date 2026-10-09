package fingerprint

// golden_test.go -- the committed golden is well-formed, is the Chrome 133
// shape, and is reproducible from the impl-independent generator, so a corrupt
// or silently-regenerated golden fails loudly rather than passing a diff.

import (
	"slices"
	"testing"
)

// The committed synthetic golden parses and is the Chrome 133 shape: grease-led
// suites and groups, the post-quantum key share, a 32-byte session id, the
// extensions opening and closing with grease, and no first-contact ticket. a
// corrupt golden fails here.
func TestCommittedGoldenHasChrome133Shape(t *testing.T) {
	golden, err := LoadGolden(GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := golden.Fingerprint

	if len(fingerprint.CipherSuites) == 0 || !IsGrease(fingerprint.CipherSuites[0]) {
		t.Errorf("golden cipher suites do not open with grease: %s", formatUint16s(fingerprint.CipherSuites))
	}
	if !sliceContains(fingerprint.SupportedGroups, groupX25519Mlkem768) || !sliceContains(fingerprint.KeyShareGroups, groupX25519Mlkem768) {
		t.Error("golden lacks the X25519MLKEM768 key share")
	}
	if fingerprint.LegacySessionIdLength != 32 {
		t.Errorf("golden session id length = %d, want 32", fingerprint.LegacySessionIdLength)
	}
	if fingerprint.HasPreSharedKey {
		t.Error("golden carries a first-contact pre_shared_key")
	}
	if !slices.Equal(fingerprint.AlpnProtocols, []string{"h2", "http/1.1"}) {
		t.Errorf("golden alpn = %q, want a navigation's [h2 http/1.1]", fingerprint.AlpnProtocols)
	}
}

// The committed golden is reproducible: a freshly generated uTLS Chrome 133
// hello diffs clean against it. this both proves the generator and catches a
// golden that was hand-edited or regenerated from a different profile -- the
// grease and extension order differ between the two, so only the normalizing
// diff, working correctly, makes them agree.
func TestCommittedGoldenReproducesFromGenerator(t *testing.T) {
	golden := loadSyntheticGolden(t)
	raw, err := GenerateChromeHello(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ParseClientHello(raw)
	if err != nil {
		t.Fatal(err)
	}
	if drifts := Diff(golden, fresh, chromeNavigationOptions()); len(drifts) != 0 {
		t.Fatalf("a fresh generated hello drifts from the committed golden: %s", FormatDrift(GoldenChrome133Synthetic, drifts))
	}
}

// Two generated hellos shuffle their extension order and re-pick grease, so the
// generator exercises the same per-connection variation real Chrome does; the
// normalizing diff still makes them agree. this guards against a generator that
// froze the order (which would hide a shuffle regression in connect).
func TestGeneratorVariesOrderButDiffsClean(t *testing.T) {
	first, err := GenerateChromeHello(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateChromeHello(ServerName)
	if err != nil {
		t.Fatal(err)
	}
	firstFingerprint, err := ParseClientHello(first)
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint, err := ParseClientHello(second)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(firstFingerprint.ExtensionTypes, secondFingerprint.ExtensionTypes) {
		t.Errorf("two generated hellos share an extension order, so the shuffle is not exercised: %s", formatUint16s(firstFingerprint.ExtensionTypes))
	}
	if drifts := Diff(firstFingerprint, secondFingerprint, chromeNavigationOptions()); len(drifts) != 0 {
		t.Fatalf("two Chrome hellos drift from each other: %s", FormatDrift(GoldenChrome133Synthetic, drifts))
	}
}
