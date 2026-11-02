package extender

// An NLayer hop that splices (EXTENDER-UPDATE2 §3.2): a Phase B extender sends a
// legacy hello to its borrowed site, so the extender in front of it reaches it only
// with its camouflage key, the --nlayer-hop parameter camouflage_key.

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Builds a Phase B extender that forwards to its destination, with an extender in
// front relaying every forward to it over tcp. withCamouflageKey says whether the
// hop's config carries the Phase B extender's camouflage key.
func newTestCamouflageHopChain(t *testing.T, withCamouflageKey bool) (entry *extenderFixture, hop *extenderFixture, site *fakeBorrowSite) {
	t.Helper()
	t.Cleanup(connect.Testing_SetBorrowDomains([]string{testBorrowName}))
	seed, identityPublicKey, realityPublicKey := newCamouflageKeys(t)
	site = newFakeBorrowSite(t, tlsBorrowSiteHandler(t))
	hop = newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = seed
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{testBorrowName}
		settings.CamouflageSpliceDialContext = site.dialContext()
	})
	entry = newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		hopConfig := hop.extenderConfig(connect.ExtenderCarrierTcp)
		hopConfig.Secret = ""
		hopConfig.PublicKey = identityPublicKey
		if withCamouflageKey {
			hopConfig.RealityPublicKey = realityPublicKey
		}
		settings.NLayerHops = []*connect.ExtenderConfig{hopConfig}
	})
	return entry, hop, site
}

// With the key the front's tcp dial authenticates, and a forward reaches the
// destination behind a hop that splices every other hello.
func TestExtenderNLayerHopWithTheCamouflageKeyReachesASplicingHop(t *testing.T) {
	entry, hop, _ := newTestCamouflageHopChain(t, true)
	body, err := getThroughNLayer(entry, connect.ExtenderCarrierTcp, nlayerConnectSettings(hop))
	if err != nil {
		t.Fatalf("the forward failed: %v", err)
	}
	if !strings.Contains(body, testHelloClientAddress) {
		t.Fatalf("body = %q", body)
	}
	if count := hop.server.CamouflageStats().AuthenticatedCount; count == 0 {
		t.Fatal("the hop did not see an authenticated hello")
	}
}

// Without it the hop's legacy hello is spliced to the borrowed site, where the
// leaf check fails it: what the parameter exists for.
func TestExtenderNLayerHopWithoutTheCamouflageKeyIsSplicedAway(t *testing.T) {
	entry, hop, site := newTestCamouflageHopChain(t, false)
	if _, err := getThroughNLayer(entry, connect.ExtenderCarrierTcp, nlayerConnectSettings(hop)); err == nil {
		t.Fatal("a forward passed through a splicing hop with no camouflage key")
	}
	if count := hop.server.CamouflageStats().AuthenticatedCount; count != 0 {
		t.Fatalf("the hop authenticated %d hellos with no key", count)
	}
	if site.reached.get() == 0 {
		t.Fatal("the hop did not splice the legacy hello")
	}
}

// The server keeps its own copy of the key, as it does of the identity key, so a
// caller that changes its settings after construction cannot race a dial.
func TestExtenderNLayerHopsCopyTheCamouflageKey(t *testing.T) {
	realityPublicKey := []byte{1, 2, 3}
	hops := newExtenderNLayerHops([]*connect.ExtenderConfig{{RealityPublicKey: realityPublicKey}})
	realityPublicKey[0] = 9
	if hops[0].extenderConfig.RealityPublicKey[0] != 1 {
		t.Fatal("the hop shares the caller's key")
	}
}
