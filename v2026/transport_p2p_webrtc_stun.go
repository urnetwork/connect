package connect

// Per-session STUN/ICE server selection. Two fixed STUN servers are a cheap
// dns/stun prefilter signal: a censor can watch for every client contacting
// exactly the same pair. Drawing a fresh random subset from a larger pool of
// high-collateral servers per session removes that fixed shape while keeping
// the collateral cost of blocking any one server high. This mirrors the
// Snowflake design (a random subset of a high-collateral pool per rendezvous).

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	mathrand "math/rand"
)

// defaultStunServerUrls is the shipped high-collateral STUN pool. It keeps the
// two anycast servers connect used before (cloudflare, google — both live and
// very high collateral) and adds the public high-collateral set documented for
// Snowflake, so a per-session subset is unlikely to be all-dead.
//
// A dead or slow server in the pool costs at most one StunGatherTimeout per
// draw (see DefaultWebRtcSettings), and only IceServerSampleCount of them are
// drawn per session, so the gather cost stays bounded even if some entries rot.
// Callers that need pinned servers set IceServerUrls, which overrides the pool.
var defaultStunServerUrls = []string{
	"stun:stun.cloudflare.com:3478",
	"stun:stun.l.google.com:19302",
	"stun:stun.epygi.com:3478",
	"stun:stun.uls.co.za:3478",
	"stun:stun.voipgate.com:3478",
	"stun:stun.mixvoip.com:3478",
	"stun:stun.telnyx.com:3478",
	"stun:stun.hot-chilli.net:3478",
	"stun:stun.fitauto.ru:3478",
	"stun:stun.m-online.net:3478",
}

// defaultIceServerSampleCount is how many pool servers are offered per session.
// A few (not all) keeps candidate gathering fast under the 2s gather timeout
// while still breaking the fixed-pair signal.
const defaultIceServerSampleCount = 3

// sampleStunServerUrls returns up to sampleCount distinct urls drawn uniformly
// at random (and in randomized order) from poolUrls. It is deterministic for a
// given rng, which the tests seed. A sampleCount of 0 or more than the pool
// size yields the whole pool in randomized order. An empty pool yields nil.
func sampleStunServerUrls(poolUrls []string, sampleCount int, rng *mathrand.Rand) []string {
	if len(poolUrls) == 0 {
		return nil
	}
	selectedCount := sampleCount
	if selectedCount <= 0 || len(poolUrls) < selectedCount {
		selectedCount = len(poolUrls)
	}
	// Perm gives a random permutation; its first selectedCount indices are a
	// uniform random distinct subset in randomized order.
	permutedIndexes := rng.Perm(len(poolUrls))
	selectedUrls := make([]string, selectedCount)
	for i := 0; i < selectedCount; i += 1 {
		selectedUrls[i] = poolUrls[permutedIndexes[i]]
	}
	return selectedUrls
}

// cryptoSeededRand builds a math/rand source seeded from crypto/rand, so the
// per-session STUN subset is not predictable from wall-clock time. A crypto
// read failure falls back to a fixed seed rather than panicking; the selection
// is a traffic-shaping choice, not a security boundary.
func cryptoSeededRand() *mathrand.Rand {
	var seedBytes [8]byte
	if _, err := cryptorand.Read(seedBytes[:]); err != nil {
		return mathrand.New(mathrand.NewSource(1))
	}
	return mathrand.New(mathrand.NewSource(int64(binary.LittleEndian.Uint64(seedBytes[:]))))
}

// selectIceServerUrls resolves the per-session ICE/STUN server list the peer
// connection factory offers. An explicit IceServerUrls override wins so callers
// and tests can pin exact servers; otherwise a random IceServerSampleCount
// subset is drawn from IceServerPoolUrls. The factory is manager scoped and
// calls this once, so every manager (session) offers its own subset.
func (self *WebRtcSettings) selectIceServerUrls() []string {
	if 0 < len(self.IceServerUrls) {
		return append([]string{}, self.IceServerUrls...)
	}
	rng := self.iceServerRandForTest
	if rng == nil {
		rng = cryptoSeededRand()
	}
	return sampleStunServerUrls(self.IceServerPoolUrls, self.IceServerSampleCount, rng)
}
