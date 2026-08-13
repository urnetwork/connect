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

// TestSetReliabilitySettingsTogglesLightClassifierLive is the fix-round
// proof: SetReliabilitySettings is the developer-menu A/B seam, used to flip
// a knob on a LIVE session with no reconnect. Every other Phase 1/2 knob
// (ScoredPlacement, hysteresis, dampening, ...) is read fresh from
// reliabilitySettings() on each placement decision, so a runtime override
// "just works" for them with no extra wiring. LightClassifier is different:
// it gates whether an *object* -- the classifier -- is installed behind the
// SetFlowClassifier seam, and reflection-based settings plumbing
// (relSettingsDiffLines, the banner) cannot install an object; it can only
// report that a bool changed. Before this fix, SetReliabilitySettings would
// happily log `field=lightclassifier from=0 to=1` and the banner would
// report `lightclassifier=1` while flowClassifier stayed nil and placement
// never changed -- a confirming log line for something that did not happen.
//
// This asserts the thing that actually matters -- placement order changes --
// not just that flowClassifier becomes non-nil, so a fix that installs a
// classifier which is somehow never consulted would still be caught.
func TestSetReliabilitySettingsTogglesLightClassifierLive(t *testing.T) {
	ipPath := testLightIpPath(IpProtocolTcp, "93.184.216.1", 443)

	parent, clients := flowCapTestParent(t, 0, 50, 1)
	parent.config.Store(&multiClientConfig{
		serverNameLookup: stubServerNameLookup{names: []string{"netflix.com"}},
	})

	// before any toggle: constructed with LightClassifier off (flowCapTestParent
	// uses DefaultMultiClientSettings, which is zero-value-off), so no
	// classifier and the legacy order stands.
	if parent.flowClassifier.Load() != nil {
		t.Fatal("no classifier should be installed before any runtime toggle")
	}
	legacy := parent.scoredPlacementReorder(clients, ipPath, "")
	if len(legacy) != 2 || legacy[0] != clients[0] || legacy[1] != clients[1] {
		t.Fatalf("legacy order = %v, want [clients[0], clients[1]] unchanged", legacy)
	}

	// flip LightClassifier on at RUNTIME -- exactly what a developer menu A/B
	// does mid-session, no reconnect. This must install a classifier, not
	// just change what the banner/diff log report.
	parent.SetReliabilitySettings(&ReliabilitySettings{LightClassifier: true})
	if parent.flowClassifier.Load() == nil {
		t.Fatal("runtime toggle on must install a classifier, not merely change the reported setting")
	}
	scored := parent.scoredPlacementReorder(clients, ipPath, "")
	if len(scored) != 2 || scored[0] != clients[1] || scored[1] != clients[0] {
		t.Fatalf("scored order after runtime toggle-on = %v, want the less-loaded exit (clients[1]) promoted to front -- the toggle changed the report but not the placement", scored)
	}

	// flip back off at runtime: the classifier must be CLEARED (SetFlowClassifier(nil)),
	// not merely left installed with the setting now reading false, and
	// placement must revert to the legacy order.
	parent.SetReliabilitySettings(&ReliabilitySettings{LightClassifier: false})
	if parent.flowClassifier.Load() != nil {
		t.Fatal("runtime toggle off must clear the classifier")
	}
	reverted := parent.scoredPlacementReorder(clients, ipPath, "")
	if len(reverted) != 2 || reverted[0] != clients[0] || reverted[1] != clients[1] {
		t.Fatalf("order after runtime toggle-off = %v, want the legacy order restored", reverted)
	}
}
