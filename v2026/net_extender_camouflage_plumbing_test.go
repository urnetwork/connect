package connect

// The camouflage key from a verified record to the tcp dial (EXTENDER.md P6, P8;
// EXTENDER-UPDATE2 §3.2). A client fronts a Phase B extender only if the record's
// key reaches the dial, and the record reaches the dial through the directory's
// candidates and the strategy's expansion: the connection draw, with the gated
// preference off and on (ConnectCandidates), the plain candidates, and the feed
// and probe configs. A dial that lost the key would run the legacy attempt alone
// and be spliced away.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func newTestCamouflageKey(t *testing.T) []byte {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey.PublicKey().Bytes()
}

// One record at `ip` on every carrier, signed by the root, with realityPublicKey
// as its camouflage key.
func signTestCamouflageRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	extenderKey ed25519.PublicKey,
	clock *testClock,
	directoryTier int,
	realityPublicKey []byte,
	ip string,
) *protocol.ExtenderRecord {
	t.Helper()
	record, err := SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:        extenderKey,
		Addresses:        []*protocol.ExtenderAddress{testExtenderAddress(ip)},
		TcpPort:          443,
		UdpPort:          443,
		DnsPort:          53,
		DnsTld:           "x.example.",
		CountryCode:      "us",
		IssueTimeMs:      uint64(clock.Now().UnixMilli()),
		ExpireTimeMs:     uint64(clock.Now().Add(24 * time.Hour).UnixMilli()),
		NetworkHost:      testExtenderNetworkHost,
		DirectoryTier:    uint32(directoryTier),
		RealityPublicKey: realityPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// A verified record's key is on the candidate from every draw a dial reads:
// the plain candidates, the verified ones, and the connection draw with the
// gated preference off and on. A record with no key, or one whose key is not an
// X25519 key's size, gives none, so that record's dial runs legacy alone; an
// unverified address has no key at all.
func TestExtenderCandidateCarriesTheRecordCamouflageKey(t *testing.T) {
	realityPublicKey := newTestCamouflageKey(t)
	for _, c := range []struct {
		name string
		key  []byte
		want []byte
	}{
		{name: "a key", key: realityPublicKey, want: realityPublicKey},
		{name: "no key", key: nil, want: nil},
		{name: "a short key", key: realityPublicKey[:31], want: nil},
		{name: "a long key", key: append(append([]byte{}, realityPublicKey...), 0), want: nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock := newTestClock()
			directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
			extenderKey := newTestExtenderKey(t)
			record := signTestCamouflageRecord(t, rootPrivateKey, extenderKey, clock, ExtenderDirectoryTierOpen, c.key, "192.0.2.10")
			if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
				t.Fatal(err)
			}
			require := func(name string, candidates []*ExtenderCandidate) {
				t.Helper()
				if len(candidates) != 1 {
					t.Fatalf("%s drew %d candidates", name, len(candidates))
				}
				if !bytes.Equal(candidates[0].RealityPublicKey, c.want) {
					t.Fatalf("%s: candidate key %x, expected %x", name, candidates[0].RealityPublicKey, c.want)
				}
			}
			require("Candidates", directory.Candidates(4, 8))
			require("VerifiedCandidates", directory.VerifiedCandidates(4, 8))
			require("the connection draw", directory.ConnectCandidates([]int{4}, 8, false, nil)[0])
			require("the verified connection draw", directory.ConnectCandidates([]int{4}, 8, true, nil)[0])

			directory.SetGatedPreference(&ExtenderGatedPreferenceSettings{
				Enabled:        true,
				AttemptTimeout: testGatedPreferenceAttemptTimeout,
			})
			applyGatedRelease := func() {
				t.Helper()
				gatedRecord := signTestCamouflageRecord(t, rootPrivateKey, newTestExtenderKey(t), clock, ExtenderDirectoryTierGated, c.key, "192.0.2.20")
				if changed, err := directory.ApplyReleaseRecord(gatedRecord, directory.ResetGeneration()); err != nil || !changed {
					t.Fatalf("the released record was not applied: changed=%t err=%v", changed, err)
				}
			}
			applyGatedRelease()
			drawn := directory.ConnectCandidates([]int{4}, 8, false, nil)[0]
			if len(drawn) == 0 {
				t.Fatal("the preferring connection draw is empty")
			}
			for _, candidate := range drawn {
				if !bytes.Equal(candidate.RealityPublicKey, c.want) {
					t.Fatalf("the preferring draw: candidate key %x, expected %x", candidate.RealityPublicKey, c.want)
				}
			}
		})
	}

	// no record verifies a manual address, so nothing names a key for it
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	directory.AddManual(netip.MustParseAddr("203.0.113.5"))
	candidates := directory.Candidates(4, 8)
	if len(candidates) != 1 || candidates[0].Verified || len(candidates[0].RealityPublicKey) != 0 {
		t.Fatalf("a manual address drew %+v", candidates)
	}
}

// The strategy's and the feed's configs carry the key and the borrow country on
// the tcp carrier and only there, which is the one carrier the camouflage rides.
func TestExtenderDialConfigsCarryTheCamouflageKeyOnTcpOnly(t *testing.T) {
	realityPublicKey := newTestCamouflageKey(t)
	candidate := &ExtenderCandidate{
		Ip:               netip.MustParseAddr("192.0.2.10"),
		Verified:         true,
		PublicKey:        newTestExtenderKey(t),
		RealityPublicKey: realityPublicKey,
		Carriers:         []string{ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns},
		TcpPort:          443,
		UdpPort:          443,
		DnsPort:          53,
		DnsTld:           "x.example.",
	}
	require := func(t *testing.T, name string, extenderConfig *ExtenderConfig) {
		t.Helper()
		if extenderConfig.Profile.ConnectMode == ExtenderConnectModeTcpTls {
			if !bytes.Equal(extenderConfig.RealityPublicKey, realityPublicKey) || extenderConfig.CamouflageCountryCode != "ir" {
				t.Fatalf("%s tcp: key %x country %q", name, extenderConfig.RealityPublicKey, extenderConfig.CamouflageCountryCode)
			}
			return
		}
		if len(extenderConfig.RealityPublicKey) != 0 || extenderConfig.CamouflageCountryCode != "" {
			t.Fatalf("%s %s: key %x country %q, expected neither", name, extenderConfig.Profile.ConnectMode, extenderConfig.RealityPublicKey, extenderConfig.CamouflageCountryCode)
		}
	}

	extenderConfigs := extenderConfigsForCandidate(candidate, "", []string{"spoof.example"}, "ir")
	sawTcp := false
	for _, extenderConfig := range extenderConfigs {
		sawTcp = sawTcp || extenderConfig.Profile.ConnectMode == ExtenderConnectModeTcpTls
		require(t, "the strategy config", extenderConfig)
	}
	if !sawTcp {
		t.Fatal("the strategy drew no tcp config")
	}
	for _, connectMode := range []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic, ExtenderConnectModeDns} {
		extenderConfig := extenderFeedConfig(candidate, connectMode, []string{"spoof.example"}, "ir")
		if extenderConfig == nil {
			t.Fatalf("no feed config for %s", connectMode)
		}
		require(t, "the feed config", extenderConfig)
	}
}

// End to end through the strategy's expansion, the path a production dial takes:
// a record's key and the directory's country reach the tcp dialer's config, with
// the gated preference off and on, and with a measured list the dial then fronts
// the camouflaged attempt with the name of that country's list that the extender
// covers.
func TestClientStrategyExpansionCarriesTheCamouflageKeyToTheDial(t *testing.T) {
	const countryBorrowName = "country.borrow.example"
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{
		{Version: 1, Measured: true, Names: []ExtenderBorrowName{{Name: "global.borrow.example"}}},
		{CountryCode: "ir", Version: 1, Measured: true, Names: []ExtenderBorrowName{{Name: countryBorrowName}}},
	})
	defer restore()

	for _, preferGated := range []bool{false, true} {
		clock := newTestClock()
		clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
		directory.SetCountryHint("ir")
		realityPublicKey := newTestCamouflageKey(t)
		extenderKey := newTestExtenderKey(t)
		if preferGated {
			directory.SetGatedPreference(&ExtenderGatedPreferenceSettings{
				Enabled:        true,
				AttemptTimeout: testGatedPreferenceAttemptTimeout,
			})
			record := signTestCamouflageRecord(t, rootPrivateKey, extenderKey, clock, ExtenderDirectoryTierGated, realityPublicKey, "192.0.2.30")
			if changed, err := directory.ApplyReleaseRecord(record, directory.ResetGeneration()); err != nil || !changed {
				t.Fatalf("the released record was not applied: changed=%t err=%v", changed, err)
			}
		} else {
			record := signTestCamouflageRecord(t, rootPrivateKey, extenderKey, clock, ExtenderDirectoryTierOpen, realityPublicKey, "192.0.2.30")
			if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
				t.Fatal(err)
			}
		}

		tcpDialers := 0
		for _, dialer := range clientStrategy.expandExtenderDialers() {
			extenderConfig := dialer.extenderConfig
			if extenderConfig == nil || extenderConfig.Profile.ConnectMode != ExtenderConnectModeTcpTls {
				continue
			}
			tcpDialers += 1
			if !bytes.Equal(extenderConfig.RealityPublicKey, realityPublicKey) {
				t.Fatalf("preferGated=%t: the dialer's camouflage key is %x, expected %x", preferGated, extenderConfig.RealityPublicKey, realityPublicKey)
			}
			if extenderConfig.CamouflageCountryCode != "ir" {
				t.Fatalf("preferGated=%t: the dialer's borrow country is %q", preferGated, extenderConfig.CamouflageCountryCode)
			}
			frontName, legacyReason := extenderCamouflageDecision(DefaultConnectSettings(), extenderConfig)
			if frontName != countryBorrowName {
				t.Fatalf("preferGated=%t: the dial fronts %q (legacy reason %q), expected %q", preferGated, frontName, legacyReason, countryBorrowName)
			}
		}
		if tcpDialers == 0 {
			t.Fatalf("preferGated=%t: the expansion drew no tcp dialer", preferGated)
		}
	}
}
