package connect

import (
	mathrand "math/rand"
	"sort"
	"strings"
	"testing"
)

// synthetic high-collateral pool; never the real production hostnames.
var testStunPoolUrls = []string{
	"stun:stun1.example:3478",
	"stun:stun2.example:3478",
	"stun:stun3.example:3478",
	"stun:stun4.example:3478",
	"stun:stun5.example:3478",
	"stun:stun6.example:3478",
	"stun:stun7.example:3478",
	"stun:stun8.example:3478",
}

func selectionSignature(urls []string) string {
	sorted := append([]string{}, urls...)
	sort.Strings(sorted)
	return strings.Join(sorted, "|")
}

// Root cause: a non-randomized selection (the old fixed pair) always offers the
// same servers. The fix draws a random distinct subset from the whole pool.
// Observable: the url list the factory offers. Discriminator: over many seeds a
// randomized draw produces more than one distinct subset and eventually touches
// every pool member; a fixed/first-k selection produces exactly one subset that
// never covers the pool.
func TestStunServerSampleIsRandomSubsetOfPool(t *testing.T) {
	const sampleCount = 3
	distinctSelections := map[string]struct{}{}
	coveredUrls := map[string]struct{}{}
	for seed := int64(1); seed <= 200; seed += 1 {
		rng := mathrand.New(mathrand.NewSource(seed))
		selectedUrls := sampleStunServerUrls(testStunPoolUrls, sampleCount, rng)
		if len(selectedUrls) != sampleCount {
			t.Fatalf("seed %d: got %d urls, want %d", seed, len(selectedUrls), sampleCount)
		}
		seen := map[string]struct{}{}
		for _, url := range selectedUrls {
			if _, ok := seen[url]; ok {
				t.Fatalf("seed %d: duplicate url %s in selection %v", seed, url, selectedUrls)
			}
			seen[url] = struct{}{}
			found := false
			for _, poolUrl := range testStunPoolUrls {
				if poolUrl == url {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("seed %d: selected url %s not in pool", seed, url)
			}
			coveredUrls[url] = struct{}{}
		}
		distinctSelections[selectionSignature(selectedUrls)] = struct{}{}
	}
	if len(distinctSelections) < 2 {
		t.Fatalf("selection is not randomized across sessions: only %d distinct subset(s)", len(distinctSelections))
	}
	if len(coveredUrls) != len(testStunPoolUrls) {
		t.Fatalf("randomized draw did not cover the whole pool: covered %d of %d", len(coveredUrls), len(testStunPoolUrls))
	}
}

// Sampling more than the pool size (or 0) offers the whole pool, still randomly
// ordered, without panicking or duplicating.
func TestStunServerSampleCountClampedToPool(t *testing.T) {
	rng := mathrand.New(mathrand.NewSource(7))
	for _, sampleCount := range []int{0, len(testStunPoolUrls), len(testStunPoolUrls) + 5} {
		selectedUrls := sampleStunServerUrls(testStunPoolUrls, sampleCount, rng)
		if len(selectedUrls) != len(testStunPoolUrls) {
			t.Fatalf("sampleCount %d: got %d urls, want whole pool %d", sampleCount, len(selectedUrls), len(testStunPoolUrls))
		}
		if selectionSignature(selectedUrls) != selectionSignature(testStunPoolUrls) {
			t.Fatalf("sampleCount %d: selection is not the whole pool: %v", sampleCount, selectedUrls)
		}
	}
	if got := sampleStunServerUrls(nil, 3, rng); got != nil {
		t.Fatalf("empty pool should yield nil, got %v", got)
	}
}

// Observable from the failure table: the ICE server list used per session is a
// randomized subset of the pool. Two sessions (two seeds) must be able to offer
// different subsets. A fixed pair would make both sessions identical and the
// wrong length.
func TestSelectIceServerUrlsVariesPerSession(t *testing.T) {
	const sampleCount = 3
	distinctSelections := map[string]struct{}{}
	for seed := int64(1); seed <= 50; seed += 1 {
		settings := &WebRtcSettings{
			IceServerPoolUrls:    testStunPoolUrls,
			IceServerSampleCount: sampleCount,
			iceServerRandForTest: mathrand.New(mathrand.NewSource(seed)),
		}
		selectedUrls := settings.selectIceServerUrls()
		if len(selectedUrls) != sampleCount {
			t.Fatalf("seed %d: session offered %d servers, want %d", seed, len(selectedUrls), sampleCount)
		}
		distinctSelections[selectionSignature(selectedUrls)] = struct{}{}
	}
	if len(distinctSelections) < 2 {
		t.Fatalf("per-session ICE list is fixed, not randomized: %d distinct subset(s) over 50 sessions", len(distinctSelections))
	}
}

// The explicit override pins servers and bypasses the pool, so operators and
// tests can force an exact set.
func TestSelectIceServerUrlsOverrideWins(t *testing.T) {
	overrideUrls := []string{"stun:pinned.example:3478"}
	settings := &WebRtcSettings{
		IceServerUrls:        overrideUrls,
		IceServerPoolUrls:    testStunPoolUrls,
		IceServerSampleCount: 3,
		iceServerRandForTest: mathrand.New(mathrand.NewSource(1)),
	}
	selectedUrls := settings.selectIceServerUrls()
	if selectionSignature(selectedUrls) != selectionSignature(overrideUrls) {
		t.Fatalf("override not honored: got %v, want %v", selectedUrls, overrideUrls)
	}
	// the returned slice must be a copy, not the caller's backing array
	selectedUrls[0] = "stun:mutated.example:3478"
	if overrideUrls[0] != "stun:pinned.example:3478" {
		t.Fatalf("selectIceServerUrls aliased the override slice")
	}
}

// The two hardcoded servers were replaced by a pool + sample count. The default
// no longer pins a fixed pair, and a default session offers a sized subset of
// the default pool.
func TestDefaultWebRtcSettingsReplacesFixedStunPair(t *testing.T) {
	settings := DefaultWebRtcSettings()
	if len(settings.IceServerUrls) != 0 {
		t.Fatalf("default pins a fixed ICE list (%v); it should draw from the pool", settings.IceServerUrls)
	}
	if settings.IceServerSampleCount <= 0 {
		t.Fatalf("default sample count must be positive, got %d", settings.IceServerSampleCount)
	}
	if len(settings.IceServerPoolUrls) <= 2 {
		t.Fatalf("default pool must be larger than the old fixed pair, got %d entries", len(settings.IceServerPoolUrls))
	}
	settings.iceServerRandForTest = mathrand.New(mathrand.NewSource(1))
	selectedUrls := settings.selectIceServerUrls()
	if len(selectedUrls) != settings.IceServerSampleCount {
		t.Fatalf("default session offered %d servers, want %d", len(selectedUrls), settings.IceServerSampleCount)
	}
	for _, url := range selectedUrls {
		found := false
		for _, poolUrl := range settings.IceServerPoolUrls {
			if poolUrl == url {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default session offered %s, which is not in the default pool", url)
		}
	}
}
