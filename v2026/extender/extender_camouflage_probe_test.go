package extender

// The operator's probes of an extender that publishes a camouflage key
// (EXTENDER-UPDATE2 §3.2, connect net_extender_camouflage_probe.go): a Phase B
// extender splices the legacy probe away, so the probe needs the extender's
// key, while in Phase A the key changes nothing the probe proves.

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A probe of the fixture's tcp carrier, with realityPublicKey when it is set.
func probeCamouflageCarrier(
	t *testing.T,
	fixture *extenderFixture,
	identityPublicKey []byte,
	realityPublicKey []byte,
) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	response, err := connect.ProbeExtenderCarrierWithCamouflageKey(
		ctx,
		fixture.connectSettings(),
		fixture.ip,
		connect.ExtenderConnectModeTcpTls,
		fixture.tcpPort,
		"",
		testServerName,
		identityPublicKey,
		realityPublicKey,
		"dest.example",
		443,
	)
	if err == nil && string(response.PublicKey) != string(identityPublicKey) {
		t.Fatal("the probe passed with another identity key")
	}
	return err
}

// A forward probe through the fixture's tcp carrier, with realityPublicKey
// when it is set.
func probeCamouflageForward(
	t *testing.T,
	fixture *extenderFixture,
	identityPublicKey []byte,
	realityPublicKey []byte,
) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return connect.ProbeExtenderForwardWithCamouflageKey(
		ctx,
		fixture.connectSettings(),
		fixture.ip,
		fixture.tcpPort,
		testServerName,
		identityPublicKey,
		realityPublicKey,
		"https://dest.example",
		&tls.Config{RootCAs: fixture.destination.rootCas},
	)
}

// A Phase B extender splices the operator's legacy probe to the borrowed site,
// where the B3 leaf check fails it; with the extender's camouflage key the
// carrier and forward probes authenticate and pass. A key that is not the
// extender's fails them, since the legacy attempt is spliced away too.
func TestProbeExtenderPhaseBNeedsTheCamouflageKey(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	_, _, otherRealityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	// an operator activated extender is open, so a probe carries no secret
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})

	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, nil); err == nil {
		t.Fatal("the legacy probe passed though Phase B splices it away")
	}
	if site.reached.get() == 0 {
		t.Fatal("the legacy probe was not spliced to the borrowed site")
	}
	if _, err := probeCamouflageForward(t, fixture, identityPublicKey, nil); err == nil {
		t.Fatal("the legacy forward probe passed though Phase B splices it away")
	}

	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, realityPublicKey); err != nil {
		t.Fatalf("the carrier probe with the camouflage key failed: %v", err)
	}
	if count := fixture.server.CamouflageStats().AuthenticatedCount; count == 0 {
		t.Fatal("the carrier probe with the camouflage key did not authenticate")
	}
	clientAddress, err := probeCamouflageForward(t, fixture, identityPublicKey, realityPublicKey)
	if err != nil {
		t.Fatalf("the forward probe with the camouflage key failed: %v", err)
	}
	if clientAddress != testHelloClientAddress {
		t.Fatalf("client address = %q, expected %q", clientAddress, testHelloClientAddress)
	}

	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, otherRealityPublicKey); err == nil {
		t.Fatal("the carrier probe passed with another extender's camouflage key in Phase B")
	}
}

// In Phase A the key changes nothing a probe proves (no proof of possession):
// the extender's key authenticates, another key falls to the legacy attempt
// and still passes, and the legacy probe passes as it always has.
func TestProbeExtenderPhaseACamouflageKeyClaimsNothing(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	_, _, otherRealityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
	})

	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, realityPublicKey); err != nil {
		t.Fatalf("the probe with the camouflage key failed in Phase A: %v", err)
	}
	authenticatedCount := fixture.server.CamouflageStats().AuthenticatedCount
	if authenticatedCount == 0 {
		t.Fatal("the probe with the camouflage key did not authenticate in Phase A")
	}
	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, otherRealityPublicKey); err != nil {
		t.Fatalf("the probe with another camouflage key failed in Phase A: %v", err)
	}
	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, nil); err != nil {
		t.Fatalf("the legacy probe failed in Phase A: %v", err)
	}
	if count := fixture.server.CamouflageStats().AuthenticatedCount; count != authenticatedCount {
		t.Fatalf("authenticated count = %d after the other key and the legacy probe, expected %d", count, authenticatedCount)
	}
}

// The key rides the tcp carrier only: a quic or dns probe given one probes as
// ProbeExtenderCarrier does, on a Phase B extender too.
func TestProbeExtenderCamouflageKeyIgnoredOffTcp(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{testBorrowName})
	defer restore()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	for _, carrier := range []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns} {
		port, dnsTld := testProbeEndpoint(fixture, carrier)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := connect.ProbeExtenderCarrierWithCamouflageKey(
			ctx,
			fixture.connectSettings(),
			fixture.ip,
			testProbeConnectMode(carrier),
			port,
			dnsTld,
			testServerName,
			identityPublicKey,
			realityPublicKey,
			"dest.example",
			443,
		)
		cancel()
		if err != nil {
			t.Fatalf("%s probe with a camouflage key: %v", carrier, err)
		}
	}
	if count := site.reached.get(); count != 0 {
		t.Fatalf("a non-tcp probe reached the borrowed site %d times", count)
	}
}

// Without a bundled borrow name the operator has no front name, so the
// camouflaged attempt is dropped and the probe is the legacy attempt alone,
// which Phase B splices away: Phase B needs the operator's own build to carry
// the measured list (EXTENDER-SPLICE-ROLLOUT.md).
func TestProbeExtenderPhaseBWithoutABorrowListIsLegacy(t *testing.T) {
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	restore := connect.Testing_SetBorrowDomains([]string{})
	defer restore()
	site := newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	if err := probeCamouflageCarrier(t, fixture, identityPublicKey, realityPublicKey); err == nil {
		t.Fatal("the probe passed in Phase B with no front name to authenticate with")
	}
	if count := fixture.server.CamouflageStats().AuthenticatedCount; count != 0 {
		t.Fatalf("authenticated count = %d with no front name, expected 0", count)
	}
}
