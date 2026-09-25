package connect

import (
	"encoding/hex"
	"maps"
	mathrand "math/rand"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// The active tier of the client extender directory (EXTENDER.md E1, GEOMAP
// §2.1, D26).
//
// Every record of an active key that has an address in the directory is in
// exactly one pool of the tier index:
//
//   - kept: this extender's own record, and a record with a manual address,
//     which the cap never evicts
//   - near: a record on the continent the candidate order prefers
//   - measured: one elsewhere with a current latency sample
//   - rest: every other one, which the cap evicts first, at random
//   - held: one whose every address is held for failure, which is outside
//     the tier until a hold lapses
//
// The kept, near, measured and rest pools are the tier MaxActiveRecordCount
// bounds, and the pools a peer pinger draws its sample from, so neither reads
// the whole directory. Each pool is a slice with every record's index in it: a
// uniform draw, an add and a remove each take constant time, which is what
// keeps a directory fed a record a millisecond from doing work in proportion
// to what it holds on every one.
//
// An event -- an apply, a revocation, a dial outcome, a latency sample --
// moves the one record it concerns. What moves a record with the clock alone
// -- its expiry, a hold lapsing, a sample aging out -- is caught by a rebuild
// of the whole index at the earliest such time, so the index is exact
// whenever it is read. Everything here runs under the directory's state lock.

// The pool of the tier index a record is in.
type extenderTierPool int

const (
	// not in the index: no active record, or no address of it in the
	// directory
	extenderTierPoolNone extenderTierPool = iota
	extenderTierPoolKept
	extenderTierPoolNear
	extenderTierPoolMeasured
	extenderTierPoolRest
	extenderTierPoolHeld
	extenderTierPoolCount
)

// The pools MaxActiveRecordCount counts.
var extenderTierCountedPools = []extenderTierPool{
	extenderTierPoolKept,
	extenderTierPoolNear,
	extenderTierPoolMeasured,
	extenderTierPoolRest,
}

// Marks an identity whose record MaxActiveRecordCount never evicts: the
// extender's own, which its peers judge its pings by, its feed serves first
// and its pinger waits for (GEOMAP §2.1, D4). The record still leaves the
// directory when it is revoked or expires. A key the directory holds no
// record for is kept for when one arrives.
func (self *ExtenderDirectory) KeepPublicKey(publicKey []byte) {
	if len(publicKey) == 0 {
		return
	}
	keyHex := hex.EncodeToString(publicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.keptKeyHexes[keyHex] {
		return
	}
	self.keptKeyHexes[keyHex] = true
	self.tierUpdateWithLock(keyHex, now)
}

// Replaces MaxActiveRecordCount for the life of the directory, and evicts
// down to it at once. <= 0 keeps every record. The settings the directory was
// built with are never written.
func (self *ExtenderDirectory) SetMaxActiveRecordCount(count int) {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.maxActiveRecordCount = count
	if self.enforceActiveRecordCapWithLock(now) {
		self.changedWithLock()
	}
}

// The cap on active records in force: the setting, or what
// SetMaxActiveRecordCount replaced it with.
func (self *ExtenderDirectory) MaxActiveRecordCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.maxActiveRecordCount
}

// The records in the tier MaxActiveRecordCount bounds: the active keys with an
// address that is not held for failure, this extender's own included.
func (self *ExtenderDirectory) ActiveRecordCount() int {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.tierSweepIfDueWithLock(now)
	return self.tierActiveCountWithLock()
}

// The size of the counted pools.
func (self *ExtenderDirectory) tierActiveCountWithLock() int {
	count := 0
	for _, pool := range extenderTierCountedPools {
		count += len(self.tierPoolKeyHexes[pool])
	}
	return count
}

// The continent tier of one record under the current hint (DESIGNNOTES4.md
// §4): 0 on the hinted continent, 1 on another, 2 unknown -- no record, or one
// that predates the continent field. With no hint every record is tier 0,
// which leaves every order what it was.
func (self *ExtenderDirectory) continentTierOfRecordWithLock(keyRecord *extenderDirectoryRecord) int {
	if self.continentHint == "" {
		return 0
	}
	if keyRecord == nil || keyRecord.recordBody == nil {
		return 2
	}
	switch continentCode := strings.ToUpper(strings.TrimSpace(keyRecord.recordBody.ContinentCode)); continentCode {
	case "":
		return 2
	case self.continentHint:
		return 0
	default:
		return 1
	}
}

// Puts one key in the pool it belongs in now, and moves the next rebuild up to
// the earliest time that pool may change with the clock alone: the record
// expiring, and for a held record its first hold lapsing, for a measured one
// its first sample aging out.
func (self *ExtenderDirectory) tierUpdateWithLock(keyHex string, now time.Time) {
	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil {
		return
	}
	earliest := func(a time.Time, b time.Time) time.Time {
		if a.IsZero() || (!b.IsZero() && b.Before(a)) {
			return b
		}
		return a
	}
	poolOf := func() (extenderTierPool, time.Time) {
		if keyRecord.recordBody == nil {
			return extenderTierPoolNone, time.Time{}
		}
		if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
			return extenderTierPoolNone, time.Time{}
		}
		var expireTime time.Time
		if 0 < keyRecord.recordBody.ExpireTimeMs {
			// expired once the expiry and its skew are strictly past
			expireTime = time.UnixMilli(int64(keyRecord.recordBody.ExpireTimeMs)).
				Add(self.settings.RecordExpireSkew).
				Add(time.Nanosecond)
		}
		var holdLapseTime time.Time
		var latencyAgeTime time.Time
		addressCount := 0
		held := true
		manual := false
		measured := false
		for _, ip := range keyRecord.ips {
			address := self.ipAddresses[ip]
			if address == nil || address.publicKeyHex != keyHex {
				continue
			}
			addressCount += 1
			if address.source == ExtenderSourceManual {
				manual = true
			}
			if now.Before(address.holdUntilTime) {
				holdLapseTime = earliest(holdLapseTime, address.holdUntilTime)
			} else {
				held = false
			}
			if _, ok := self.latencyWithLock(address, now, false); ok {
				measured = true
				if 0 < self.settings.LatencyMaxAge {
					latencyAgeTime = earliest(latencyAgeTime, address.latencyTime.Add(self.settings.LatencyMaxAge))
				}
			}
		}
		switch {
		case addressCount == 0:
			return extenderTierPoolNone, time.Time{}
		case self.keptKeyHexes[keyHex] || manual:
			return extenderTierPoolKept, expireTime
		case held:
			return extenderTierPoolHeld, earliest(expireTime, holdLapseTime)
		case self.continentHint != "" && self.continentTierOfRecordWithLock(keyRecord) == 0:
			return extenderTierPoolNear, expireTime
		case measured:
			return extenderTierPoolMeasured, earliest(expireTime, latencyAgeTime)
		default:
			return extenderTierPoolRest, expireTime
		}
	}
	pool, changeTime := poolOf()
	if keyRecord.tierPool != pool {
		self.tierRemoveWithLock(keyHex, keyRecord)
		if pool != extenderTierPoolNone {
			keyRecord.tierPool = pool
			keyRecord.tierIndex = len(self.tierPoolKeyHexes[pool])
			self.tierPoolKeyHexes[pool] = append(self.tierPoolKeyHexes[pool], keyHex)
			if pool == extenderTierPoolNear {
				self.tierNearVersion += 1
			}
		}
	}
	if !changeTime.IsZero() && (self.tierSweepTime.IsZero() || changeTime.Before(self.tierSweepTime)) {
		self.tierSweepTime = changeTime
	}
}

// Takes one key out of the index: the last of its pool takes its place.
func (self *ExtenderDirectory) tierRemoveWithLock(keyHex string, keyRecord *extenderDirectoryRecord) {
	pool := keyRecord.tierPool
	if pool == extenderTierPoolNone {
		return
	}
	keyHexes := self.tierPoolKeyHexes[pool]
	last := len(keyHexes) - 1
	if keyRecord.tierIndex < last {
		movedKeyHex := keyHexes[last]
		keyHexes[keyRecord.tierIndex] = movedKeyHex
		self.keyHexRecords[movedKeyHex].tierIndex = keyRecord.tierIndex
	}
	keyHexes[last] = ""
	self.tierPoolKeyHexes[pool] = keyHexes[:last]
	keyRecord.tierPool = extenderTierPoolNone
	keyRecord.tierIndex = 0
	if pool == extenderTierPoolNear {
		self.tierNearVersion += 1
	}
}

// Puts the key of one address in the pool it belongs in now, after the
// address changed.
func (self *ExtenderDirectory) tierUpdateAddressWithLock(address *extenderDirectoryAddress, now time.Time) {
	if address.publicKeyHex != "" {
		self.tierUpdateWithLock(address.publicKeyHex, now)
	}
}

// Rebuilds the whole index, in key order so the pools come out the same
// whatever order the map hands the keys in. An address a key no longer holds
// is forgotten by it here.
func (self *ExtenderDirectory) tierRebuildWithLock(now time.Time) {
	for pool := range self.tierPoolKeyHexes {
		clear(self.tierPoolKeyHexes[pool])
		self.tierPoolKeyHexes[pool] = self.tierPoolKeyHexes[pool][:0]
	}
	self.tierSweepTime = time.Time{}
	for _, keyHex := range slices.Sorted(maps.Keys(self.keyHexRecords)) {
		keyRecord := self.keyHexRecords[keyHex]
		keyRecord.tierPool = extenderTierPoolNone
		keyRecord.tierIndex = 0
		keyRecord.ips = slices.DeleteFunc(keyRecord.ips, func(ip netip.Addr) bool {
			address := self.ipAddresses[ip]
			return address == nil || address.publicKeyHex != keyHex
		})
		self.tierUpdateWithLock(keyHex, now)
	}
	self.tierNearVersion += 1
}

// Rebuilds the index when a pooled record has changed pool with the clock
// alone since it was last built.
func (self *ExtenderDirectory) tierSweepIfDueWithLock(now time.Time) {
	if !self.tierSweepTime.IsZero() && !now.Before(self.tierSweepTime) {
		self.tierRebuildWithLock(now)
	}
}

// Evicts down to MaxActiveRecordCount (GEOMAP §2.1, D26): a random record of
// the rest first, then the oldest applied of the measured, then the oldest
// applied of the hinted continent. A kept record, a held one and an expired
// one are never evicted here, so a tier of kept records larger than the cap
// simply holds more than the cap. An evicted record takes its addresses with
// it; a revocation the key carried is kept, so a replayed older record cannot
// bring the key back, and a record that arrives again later is applied as a
// new one.
func (self *ExtenderDirectory) enforceActiveRecordCapWithLock(now time.Time) (changed bool) {
	if self.maxActiveRecordCount <= 0 {
		return false
	}
	self.tierSweepIfDueWithLock(now)
	victim := func() string {
		if restKeyHexes := self.tierPoolKeyHexes[extenderTierPoolRest]; 0 < len(restKeyHexes) {
			return restKeyHexes[self.randomIndexWithLock(len(restKeyHexes))]
		}
		for _, pool := range []extenderTierPool{extenderTierPoolMeasured, extenderTierPoolNear} {
			oldestKeyHex := ""
			var oldestApplySerial uint64
			for _, keyHex := range self.tierPoolKeyHexes[pool] {
				applySerial := self.keyHexRecords[keyHex].applySerial
				if oldestKeyHex == "" || applySerial < oldestApplySerial {
					oldestKeyHex = keyHex
					oldestApplySerial = applySerial
				}
			}
			if oldestKeyHex != "" {
				return oldestKeyHex
			}
		}
		return ""
	}
	for self.maxActiveRecordCount < self.tierActiveCountWithLock() {
		keyHex := victim()
		if keyHex == "" {
			break
		}
		keyRecord := self.keyHexRecords[keyHex]
		for _, ip := range keyRecord.ips {
			if address := self.ipAddresses[ip]; address != nil && address.publicKeyHex == keyHex {
				// never a manual address: a record with one is kept
				delete(self.ipAddresses, ip)
			}
		}
		if self.log.V(2).Enabled() {
			self.log.Infof("[extender]evicted record %s for the active cap\n", keyHex)
		}
		self.tierRemoveWithLock(keyHex, keyRecord)
		if keyRecord.revocation != nil {
			keyRecord.record = nil
			keyRecord.recordBody = nil
			keyRecord.ips = nil
		} else {
			delete(self.keyHexRecords, keyHex)
		}
		changed = true
	}
	return changed
}

// Drops one identity from the directory, taking it out of the index first so
// no pool keeps a key the map no longer holds.
func (self *ExtenderDirectory) deleteKeyRecordWithLock(keyHex string) {
	if keyRecord := self.keyHexRecords[keyHex]; keyRecord != nil {
		self.tierRemoveWithLock(keyHex, keyRecord)
	}
	delete(self.keyHexRecords, keyHex)
}

// A uniform index in [0, n) from the Random seam, or from math/rand without
// one.
func (self *ExtenderDirectory) randomIndexWithLock(n int) int {
	if self.settings.Random == nil {
		return mathrand.Intn(n)
	}
	i := int(self.settings.Random() * float64(n))
	return min(max(i, 0), n-1)
}

// The number of active peers of the extender whose key is `ownPublicKey`: the
// active verified identities with an address in the directory, held or not,
// but its own (GEOMAP §2.1).
func (self *ExtenderDirectory) peerCount(ownPublicKey []byte) int {
	ownKeyHex := hex.EncodeToString(ownPublicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.tierSweepIfDueWithLock(now)
	count := len(self.tierPoolKeyHexes[extenderTierPoolHeld]) + self.tierActiveCountWithLock()
	if ownKeyRecord := self.keyHexRecords[ownKeyHex]; ownKeyRecord != nil && ownKeyRecord.tierPool != extenderTierPoolNone {
		count -= 1
	}
	return count
}

// The version of the near pool, which changes whenever a record joins or
// leaves it: a peer pinger follows its nearest peers by it.
func (self *ExtenderDirectory) nearVersion() uint64 {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.tierSweepIfDueWithLock(now)
	return self.tierNearVersion
}

// Up to `count` keys of the hinted continent's records but `ownKeyHex`, nearest
// first -- in the candidate order of each record's best unheld address, the
// order every dial takes -- and the near pool's version they were read at.
// Nothing without a hint.
func (self *ExtenderDirectory) nearPeerKeyHexes(count int, ownKeyHex string) ([]string, uint64) {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.tierSweepIfDueWithLock(now)
	// one near record and the address it is ordered by
	type nearPeer struct {
		keyHex  string
		address *extenderDirectoryAddress
	}
	nearPeers := []nearPeer{}
	for _, keyHex := range self.tierPoolKeyHexes[extenderTierPoolNear] {
		if keyHex == ownKeyHex {
			continue
		}
		var bestAddress *extenderDirectoryAddress
		for _, ip := range self.keyHexRecords[keyHex].ips {
			address := self.ipAddresses[ip]
			if address == nil || address.publicKeyHex != keyHex || now.Before(address.holdUntilTime) {
				continue
			}
			if bestAddress == nil || self.compareCandidateWithLock(address, bestAddress, now) < 0 {
				bestAddress = address
			}
		}
		if bestAddress != nil {
			nearPeers = append(nearPeers, nearPeer{keyHex: keyHex, address: bestAddress})
		}
	}
	if 0 < count && count < len(nearPeers) {
		// only a pool larger than what is asked needs the order
		slices.SortFunc(nearPeers, func(a nearPeer, b nearPeer) int {
			if c := self.compareCandidateWithLock(a.address, b.address, now); c != 0 {
				return c
			}
			return strings.Compare(a.keyHex, b.keyHex)
		})
		nearPeers = nearPeers[:count]
	}
	keyHexes := make([]string, 0, len(nearPeers))
	for _, nearPeer := range nearPeers {
		keyHexes = append(keyHexes, nearPeer.keyHex)
	}
	return keyHexes, self.tierNearVersion
}

// Up to `count` keys drawn uniformly, by the Random seam, from the records off
// the hinted continent that are not held -- the measured and the rest --
// leaving out what `excluded` says, of which there are at most
// `excludedCount`. A pool no larger than a few times what is asked is
// shuffled; a larger one is drawn from, a repeat drawn again, so a draw from
// a large directory reads no more of it than it takes. The draws are bounded:
// a seam that keeps drawing what is excluded ends in a scan. `excluded` is
// the caller's own and is called with the directory's state lock held.
func (self *ExtenderDirectory) drawPeerKeyHexes(
	count int,
	excluded func(keyHex string) bool,
	excludedCount int,
) []string {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.tierSweepIfDueWithLock(now)
	measuredKeyHexes := self.tierPoolKeyHexes[extenderTierPoolMeasured]
	restKeyHexes := self.tierPoolKeyHexes[extenderTierPoolRest]
	total := len(measuredKeyHexes) + len(restKeyHexes)
	at := func(i int) string {
		if i < len(measuredKeyHexes) {
			return measuredKeyHexes[i]
		}
		return restKeyHexes[i-len(measuredKeyHexes)]
	}
	drawnKeyHexes := []string{}
	if count <= 0 || total == 0 {
		return drawnKeyHexes
	}
	if total <= 4*(count+excludedCount) {
		keyHexes := make([]string, 0, total)
		for i := range total {
			if keyHex := at(i); !excluded(keyHex) {
				keyHexes = append(keyHexes, keyHex)
			}
		}
		if len(keyHexes) <= count {
			return keyHexes
		}
		// a partial shuffle: each of the first `count` places takes a
		// uniform pick of what is left
		for i := range count {
			j := i + self.randomIndexWithLock(len(keyHexes)-i)
			keyHexes[i], keyHexes[j] = keyHexes[j], keyHexes[i]
		}
		return keyHexes[:count]
	}
	// at least three quarters of the pool can be drawn, so a repeat is rare
	chosenKeyHexes := make(map[string]bool, count)
	for attempt := 0; len(drawnKeyHexes) < count && attempt < 16*count; attempt += 1 {
		keyHex := at(self.randomIndexWithLock(total))
		if excluded(keyHex) || chosenKeyHexes[keyHex] {
			continue
		}
		chosenKeyHexes[keyHex] = true
		drawnKeyHexes = append(drawnKeyHexes, keyHex)
	}
	for i := 0; len(drawnKeyHexes) < count && i < total; i += 1 {
		if keyHex := at(i); !excluded(keyHex) && !chosenKeyHexes[keyHex] {
			chosenKeyHexes[keyHex] = true
			drawnKeyHexes = append(drawnKeyHexes, keyHex)
		}
	}
	return drawnKeyHexes
}

// For each key, in the order given, its addresses in address order as their
// bytes when it is an active peer now -- an active verified key with an
// address in the directory, held or not -- which is what a peer pinger judges
// an address change by, and empty when it is not one: an active peer always
// has an address.
func (self *ExtenderDirectory) peerAddressKeys(keyHexes []string) []string {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.tierSweepIfDueWithLock(now)
	addressKeys := make([]string, len(keyHexes))
	// both reused from key to key, so a key costs its one string
	ips := []netip.Addr{}
	addressKeyBytes := []byte{}
	for i, keyHex := range keyHexes {
		keyRecord := self.keyHexRecords[keyHex]
		if keyRecord == nil || keyRecord.tierPool == extenderTierPoolNone {
			continue
		}
		ips = ips[:0]
		for _, ip := range keyRecord.ips {
			if address := self.ipAddresses[ip]; address != nil && address.publicKeyHex == keyHex {
				ips = append(ips, ip)
			}
		}
		slices.SortFunc(ips, func(a netip.Addr, b netip.Addr) int {
			return a.Compare(b)
		})
		addressKeyBytes = addressKeyBytes[:0]
		for _, ip := range ips {
			ipBytes := ip.As16()
			addressKeyBytes = append(addressKeyBytes, ipBytes[:]...)
		}
		addressKeys[i] = string(addressKeyBytes)
	}
	return addressKeys
}

// The address of one key and family a peer pinger dials: the first of its
// usable addresses that is not limited, in the directory's candidate order,
// which is where Candidates would put it. Nil when it has none: every one
// held, limited, of another family, or the record no longer active.
func (self *ExtenderDirectory) peerCandidate(keyHex string, ipVersion int) *ExtenderCandidate {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil || keyRecord.recordBody == nil {
		return nil
	}
	if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
		return nil
	}
	var bestAddress *extenderDirectoryAddress
	for _, ip := range keyRecord.ips {
		address := self.ipAddresses[ip]
		if address == nil || address.publicKeyHex != keyHex {
			continue
		}
		if ipVersion != 0 && addressIpVersion(ip) != ipVersion {
			continue
		}
		if now.Before(address.holdUntilTime) || now.Before(address.limitedUntilTime) {
			continue
		}
		if bestAddress == nil || self.compareCandidateWithLock(address, bestAddress, now) < 0 {
			bestAddress = address
		}
	}
	if bestAddress == nil {
		return nil
	}
	return self.candidateWithLock(bestAddress, now)
}
