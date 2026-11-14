package extender

// An NLayer hop that splices (EXTENDER-UPDATE2 §3.2): a Phase B extender sends a
// legacy hello to its borrowed site, so the extender in front of it reaches it only
// with its camouflage key, the --nlayer-hop parameter camouflage_key.

import (
	"crypto/ed25519"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
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

// The api's chain is what makes its Date authenticated, so the clock reads it on a
// route through an extender as on a direct one. The extender in the middle cannot
// move it: the chain it would have to present is the api's.
func TestClientStrategyReadsThePlatformClockThroughAnExtender(t *testing.T) {
	const skew = 10 * time.Minute
	t.Cleanup(connect.Testing_SetExtenderCamouflageLocalClock(func() time.Time {
		return time.Now().Add(-skew)
	}))
	fixture := newExtenderFixture(t, "127.0.0.1", nil)
	strategy := func(tlsConfig *tls.Config) *connect.ClientStrategy {
		settings := connect.DefaultClientStrategySettings()
		settings.EnableNormal = false
		settings.EnableResilient = false
		settings.ExtenderConfigs = []*connect.ExtenderConfig{fixture.extenderConfig(connect.ExtenderCarrierTcp)}
		settings.ConnectSettings = *fixture.connectSettings()
		settings.TlsConfig = tlsConfig
		settings.PlatformApiHosts = []string{"dest.example"}
		strategy := connect.NewClientStrategy(t.Context(), settings)
		t.Cleanup(strategy.Close)
		return strategy
	}
	offset := func() time.Duration {
		return connect.Testing_ExtenderCamouflageClockOffset(make([]byte, ed25519.PublicKeySize))
	}

	// a route the client cannot verify the api over says nothing
	unverified := strategy(&tls.Config{InsecureSkipVerify: true})
	if _, err := connect.HttpGetWithStrategyRaw(t.Context(), unverified, "https://dest.example/hello", ""); err != nil {
		t.Fatal(err)
	}
	if got := offset(); got != 0 {
		t.Fatalf("offset = %s after a response of no verified chain", got)
	}

	verified := strategy(&tls.Config{RootCAs: fixture.destination.rootCas})
	if _, err := connect.HttpGetWithStrategyRaw(t.Context(), verified, "https://dest.example/hello", ""); err != nil {
		t.Fatal(err)
	}
	if got := offset(); got < skew-time.Minute || skew+time.Minute < got {
		t.Fatalf("offset = %s, expected about +%s to cancel the skew", got, skew)
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
