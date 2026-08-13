package connect

import (
	"strings"
	"testing"
)

// TestLightClassifierDefaultZeroValueOff pins the actual regression risk: a
// default build must behave exactly like today (no classifier installed). If
// a future well-meaning edit ever sets LightClassifier in
// DefaultMultiClientSettings, this test catches it before it ships and
// silently opts every default build into class-aware placement.
func TestLightClassifierDefaultZeroValueOff(t *testing.T) {
	s := DefaultMultiClientSettings()
	if s.LightClassifier {
		t.Fatal("DefaultMultiClientSettings must leave LightClassifier off (false)")
	}
}

// TestLightClassifierSettingsZeroValueOff asserts LightClassifier follows the
// same zero-value-off contract as every other ReliabilitySettings field: a
// nil override (or one built from an older struct) must leave it off, and
// ReliabilitySettingsFrom must faithfully copy it through when set.
func TestLightClassifierSettingsZeroValueOff(t *testing.T) {
	z := ReliabilitySettingsFrom(nil) // nil -> zero value
	if z.LightClassifier {
		t.Fatal("LightClassifier must be zero-value-off (legacy behavior) for a nil override")
	}

	d := DefaultMultiClientSettings()
	if ReliabilitySettingsFrom(d).LightClassifier {
		t.Fatal("DefaultMultiClientSettings must leave LightClassifier zero-value-off")
	}

	src := &MultiClientSettings{LightClassifier: true}
	got := ReliabilitySettingsFrom(src)
	if !got.LightClassifier {
		t.Fatal("ReliabilitySettingsFrom must copy LightClassifier")
	}
}

// TestLightClassifierBannerDefaultZeroValue confirms the reflection-based
// session banner (relSettingsFields) renders lightclassifier=0 for a default
// build without any hand-edited banner list -- adding the field to
// ReliabilitySettings is sufficient.
func TestLightClassifierBannerDefaultZeroValue(t *testing.T) {
	settings := ReliabilitySettingsFrom(DefaultMultiClientSettings())
	lines := relSessionBannerLines("", settings, relLineMaxChars)
	if len(lines) != 1 {
		t.Fatalf("expected a single banner line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "lightclassifier=0") {
		t.Fatalf("default banner does not render lightclassifier=0: %s", lines[0])
	}
}

// TestMaybeInstallLightClassifier is Task 2's install-site test: with
// LightClassifier false (the zero value), flowClassifier must stay nil --
// nothing changes versus today. With it true, a classifier must be installed
// via the existing SetFlowClassifier seam.
func TestMaybeInstallLightClassifier(t *testing.T) {
	off := &RemoteUserNatMultiClient{settings: DefaultMultiClientSettings()}
	off.maybeInstallLightClassifier()
	if off.flowClassifier.Load() != nil {
		t.Fatal("LightClassifier off (zero value) must leave flowClassifier nil")
	}

	on := &RemoteUserNatMultiClient{settings: DefaultMultiClientSettings()}
	on.settings.LightClassifier = true
	on.maybeInstallLightClassifier()
	if on.flowClassifier.Load() == nil {
		t.Fatal("LightClassifier on must install a classifier")
	}
}

// TestLightClassifierInstalledChangesPlacementOrder is the brief's
// not-hollow proof: it asserts the LEGACY order explicitly (LightClassifier
// off, so no classifier is installed and scoredPlacementReorder must leave
// the field untouched), then asserts the SCORED order actually differs once
// LightClassifier is turned on and the classifier is installed through the
// real construction-time install site (maybeInstallLightClassifier),
// resolving a known streaming server name through the config's
// ServerNameLookup seam -- the same seam SetServerNameLookup wires to the
// mux's reverse index in production. A test that only checked "did not
// panic" or "installed non-nil" here would still pass if classification were
// silently disconnected from placement; comparing the two concrete orders is
// what catches that.
func TestLightClassifierInstalledChangesPlacementOrder(t *testing.T) {
	ipPath := testLightIpPath(IpProtocolTcp, "93.184.216.1", 443)

	// LightClassifier off: no classifier installed, legacy (unscored) order
	// stands. clients[0] carries 50 flows, clients[1] carries 1 -- if a
	// classifier were (incorrectly) active here, the less-loaded exit would
	// be promoted to the front, which is exactly what this asserts does NOT
	// happen.
	legacyParent, legacyClients := flowCapTestParent(t, 0, 50, 1)
	legacyParent.maybeInstallLightClassifier()
	if legacyParent.flowClassifier.Load() != nil {
		t.Fatal("LightClassifier off must leave flowClassifier nil")
	}
	legacy := legacyParent.scoredPlacementReorder(legacyClients, ipPath, "")
	if len(legacy) != 2 || legacy[0] != legacyClients[0] || legacy[1] != legacyClients[1] {
		t.Fatalf("legacy order = %v, want [clients[0], clients[1]] unchanged (no classifier installed)", legacy)
	}

	// LightClassifier on: the real install site wires a real LightClassifier
	// (not a fixedClassifier test double) that resolves "93.184.216.1" to
	// "netflix.com" via the config's ServerNameLookup, classifying the flow
	// as ClassStreaming. With a class now in hand, scoredPlacementReorder
	// reduces to the less-loaded tie-break and promotes the lighter exit.
	scoredParent, scoredClients := flowCapTestParent(t, 0, 50, 1)
	scoredParent.settings.LightClassifier = true
	scoredParent.config.Store(&multiClientConfig{
		serverNameLookup: stubServerNameLookup{names: []string{"netflix.com"}},
	})
	scoredParent.maybeInstallLightClassifier()
	if scoredParent.flowClassifier.Load() == nil {
		t.Fatal("LightClassifier on must install a classifier")
	}
	scored := scoredParent.scoredPlacementReorder(scoredClients, ipPath, "")
	if len(scored) != 2 || scored[0] != scoredClients[1] || scored[1] != scoredClients[0] {
		t.Fatalf("scored order = %v, want the less-loaded exit (clients[1]) promoted to front", scored)
	}

	// the whole point, stated directly: the heavy exit (index 0) leads the
	// legacy order and trails the scored one -- turning LightClassifier on
	// actually flips which candidate goes first, not just "installs
	// something".
	if legacy[0] != legacyClients[0] || scored[0] == scoredClients[0] {
		t.Fatal("LightClassifier=true did not change which candidate is promoted to front: classification is not reaching placement")
	}
}
