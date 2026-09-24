package extender

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	mathrand "math/rand"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Admission limits (EXTENDER.md A12).
//
// Every action an extender does work for -- a forward, a gossip or feed
// stream, a probe -- is admitted by the source's subnet hash under two limits
// of the instance's own settings: the distinct subnets admitted in any one
// minute, which bounds the whole extender, and the actions of one subnet in
// any one minute, which stops one client flooding it. Both are sliding
// windows: token buckets refilled at the limit per minute with a burst of the
// limit, so a quiet minute banks no credit. A header signed with one of this
// extender's secrets is exempt from the per-subnet limit, the secret being the
// trust, and still counts toward the per-instance one.
//
// Over either limit the request is answered 429 with a random Retry-After and
// the connection closes. Answering costs a handshake, so a subnet refused more
// than its refusal share in the same minute is closed at accept, before any.
//
// The subnet is the /29 of a v4 source and the /56 of a v6 one, the prefixes
// the platform's address hash keys on, hashed under a pepper drawn at process
// start, so the tables hold no address. The table is bounded: a subnet whose
// buckets are full and whose admission has aged out is indistinguishable from
// one never seen and is dropped, and a table full of live subnets admits no
// new one until some age out. All state is guarded by the admission lock,
// which is taken alone.

// The window every admission limit is a rate over: the limits are per minute
// by definition, which is what their names say.
const extenderAdmissionWindow = time.Minute

// The pepper of the subnet hash, drawn once per process.
var extenderAdmissionPepper = func() []byte {
	pepper := make([]byte, 32)
	if _, err := rand.Read(pepper); err != nil {
		panic(err)
	}
	return pepper
}()

// A source subnet as the tables know it.
type extenderSubnetHash [16]byte

// The subnet hash of a source address in text form, and whether it has one: a
// source with no address, such as an in-memory pipe, has none and is admitted
// without limits. Reads only what construction set.
func (self *extenderAdmission) subnetHashOf(address string) (extenderSubnetHash, bool) {
	addr, err := netip.ParseAddr(connectionSourceAddress(address))
	if err != nil {
		return extenderSubnetHash{}, false
	}
	addr = addr.Unmap().WithZone("")
	prefixBitCount := self.ipv6PrefixBitCount
	family := byte(6)
	if addr.Is4() {
		prefixBitCount = self.ipv4PrefixBitCount
		family = 4
	}
	prefix, err := addr.Prefix(prefixBitCount)
	if err != nil {
		return extenderSubnetHash{}, false
	}
	mac := hmac.New(sha256.New, extenderAdmissionPepper)
	mac.Write([]byte{family})
	mac.Write(prefix.Addr().AsSlice())
	var subnetHash extenderSubnetHash
	copy(subnetHash[:], mac.Sum(nil))
	return subnetHash, true
}

// Which limit, if any, refused an action.
type extenderAdmissionLimit int

const (
	extenderAdmitted extenderAdmissionLimit = iota
	extenderLimitedBySubnets
	extenderLimitedBySource
)

// The reported stage of an action one limit refused.
func extenderAdmissionStage(limit extenderAdmissionLimit) string {
	switch limit {
	case extenderLimitedBySubnets:
		return "admission subnets"
	default:
		return "admission source"
	}
}

// The admission state of one server.
type extenderAdmission struct {
	// the prefix widths a source's subnet is taken at, the least the subnet
	// table holds, and the source prefixes exempt from both limits, masked:
	// all resolved from the settings at construction and never changed
	ipv4PrefixBitCount int
	ipv6PrefixBitCount int
	minSubnetCount     int
	unlimitedSources   []netip.Prefix

	stateLock sync.Mutex
	// one token per subnet admitted anew, which is the per-instance limit
	subnetBucket  tokenBucket
	subnetEntries map[extenderSubnetHash]*extenderSubnetAdmission

	limitedBySubnetsCount int64
	limitedBySourceCount  int64
	unlimitedCount        int64
}

// What the admission knows of one subnet.
type extenderSubnetAdmission struct {
	// the subnet's own actions, which is the per-subnet limit
	actionBucket tokenBucket
	// the 429s it was answered with; once empty, it is closed at accept
	refusalBucket tokenBucket
	// when it last took a token of the per-instance limit; within a window of
	// that it is not a new subnet
	admitTime time.Time
}

// The admission state of a server with these settings: a width outside its
// family's range or a table bound that is not positive takes the default, and
// the unlimited sources are masked and copied. The settings are never
// written.
func newExtenderAdmission(settings *ExtenderSettings) *extenderAdmission {
	defaults := DefaultExtenderSettings()
	ipv4PrefixBitCount := settings.AdmissionIpv4PrefixBitCount
	if ipv4PrefixBitCount <= 0 || 32 < ipv4PrefixBitCount {
		ipv4PrefixBitCount = defaults.AdmissionIpv4PrefixBitCount
	}
	ipv6PrefixBitCount := settings.AdmissionIpv6PrefixBitCount
	if ipv6PrefixBitCount <= 0 || 128 < ipv6PrefixBitCount {
		ipv6PrefixBitCount = defaults.AdmissionIpv6PrefixBitCount
	}
	minSubnetCount := settings.AdmissionMinSubnetCount
	if minSubnetCount <= 0 {
		minSubnetCount = defaults.AdmissionMinSubnetCount
	}
	maskedSources := []netip.Prefix{}
	for _, unlimitedSource := range settings.AdmissionUnlimitedSources {
		if !unlimitedSource.IsValid() {
			continue
		}
		// a source is judged unmapped, so a v4 prefix written in its mapped
		// v6 form is judged as the v4 prefix it names
		if addr := unlimitedSource.Addr(); addr.Is4In6() && 96 <= unlimitedSource.Bits() {
			unlimitedSource = netip.PrefixFrom(addr.Unmap(), unlimitedSource.Bits()-96)
		}
		maskedSources = append(maskedSources, unlimitedSource.Masked())
	}
	return &extenderAdmission{
		ipv4PrefixBitCount: ipv4PrefixBitCount,
		ipv6PrefixBitCount: ipv6PrefixBitCount,
		minSubnetCount:     minSubnetCount,
		unlimitedSources:   maskedSources,
		subnetEntries:      map[extenderSubnetHash]*extenderSubnetAdmission{},
	}
}

// Whether a source address in text form is inside one of the unlimited
// prefixes. The address is judged here and kept nowhere.
func (self *extenderAdmission) unlimitedSource(address string) bool {
	if len(self.unlimitedSources) == 0 {
		return false
	}
	addr, err := netip.ParseAddr(connectionSourceAddress(address))
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, unlimitedSource := range self.unlimitedSources {
		if unlimitedSource.Contains(addr) {
			return true
		}
	}
	return false
}

// What the admission limits of one extender have refused (A12), by the limit that refused it, cumulative for the life of the
// server. A connection closed at accept is not counted: it was refused before.
type ExtenderAdmissionStats struct {
	// refused by the per-instance limit, AdmissionSubnetsPerMinute
	LimitedBySubnetsCount int64
	// refused by the per-subnet limit, AdmissionActionsPerSubnetPerMinute
	LimitedBySourceCount int64
	// admitted past both limits from an AdmissionUnlimitedSources prefix
	UnlimitedCount int64
}

// A snapshot of what the admission limits refused, and of what the unlimited
// sources were admitted past them (A12).
func (self *ExtenderServer) AdmissionStats() ExtenderAdmissionStats {
	self.admission.stateLock.Lock()
	defer self.admission.stateLock.Unlock()
	return ExtenderAdmissionStats{
		LimitedBySubnetsCount: self.admission.limitedBySubnetsCount,
		LimitedBySourceCount:  self.admission.limitedBySourceCount,
		UnlimitedCount:        self.admission.unlimitedCount,
	}
}

// The clock the admission limits read.
func (self *ExtenderServer) admissionNow() time.Time {
	if self.settings.AdmissionNow != nil {
		return self.settings.AdmissionNow()
	}
	return time.Now()
}

// A rate per minute as a token bucket reads it: per second, with the burst of
// the rate itself. <= 0 is a disabled bucket.
func extenderAdmissionRate(perMinute int) (float64, float64) {
	if perMinute <= 0 {
		return 0, 0
	}
	return float64(perMinute) / extenderAdmissionWindow.Seconds(), float64(perMinute)
}

// Admits one action from a source, and when it is refused, by which limit and
// with what Retry-After. `exempt` spares it the per-subnet limit: its header is
// signed with one of this extender's secrets. A source inside an unlimited
// prefix is spared both, judged on its address before any hash is taken.
func (self *ExtenderServer) admitAction(remoteAddress string, exempt bool) (extenderAdmissionLimit, time.Duration) {
	if self.admission.unlimitedSource(remoteAddress) {
		self.admission.stateLock.Lock()
		self.admission.unlimitedCount += 1
		self.admission.stateLock.Unlock()
		return extenderAdmitted, 0
	}
	subnetHash, ok := self.admission.subnetHashOf(remoteAddress)
	if !ok {
		return extenderAdmitted, 0
	}
	now := self.admissionNow()
	subnetRate, subnetBurst := extenderAdmissionRate(self.settings.AdmissionSubnetsPerMinute)
	actionRate, actionBurst := extenderAdmissionRate(self.settings.AdmissionActionsPerSubnetPerMinute)
	refusalRate, refusalBurst := extenderAdmissionRate(self.settings.AdmissionRefusalsPerSubnetPerMinute)

	admission := self.admission
	limit := func() extenderAdmissionLimit {
		admission.stateLock.Lock()
		defer admission.stateLock.Unlock()

		entry := admission.subnetEntries[subnetHash]
		if entry == nil {
			maxSubnetCount := max(admission.minSubnetCount, 4*self.settings.AdmissionSubnetsPerMinute)
			if maxSubnetCount <= len(admission.subnetEntries) {
				admission.pruneWithLock(now, actionRate, actionBurst, refusalRate, refusalBurst)
			}
			if maxSubnetCount <= len(admission.subnetEntries) {
				// every entry is a subnet of the last minute: there is no
				// room for another until some age out
				admission.limitedBySubnetsCount += 1
				return extenderLimitedBySubnets
			}
			entry = &extenderSubnetAdmission{}
			admission.subnetEntries[subnetHash] = entry
		}

		limit := extenderAdmitted
		if 0 < subnetRate && (entry.admitTime.IsZero() || extenderAdmissionWindow <= now.Sub(entry.admitTime)) {
			// a subnet not admitted within the window takes one of the
			// instance's tokens, and no more than one per window
			if admission.subnetBucket.refill(now, subnetRate, subnetBurst) {
				admission.subnetBucket.take()
				entry.admitTime = now
			} else {
				limit = extenderLimitedBySubnets
			}
		}
		if limit == extenderAdmitted && !exempt {
			if entry.actionBucket.refill(now, actionRate, actionBurst) {
				entry.actionBucket.take()
			} else {
				limit = extenderLimitedBySource
			}
		}
		switch limit {
		case extenderLimitedBySubnets:
			admission.limitedBySubnetsCount += 1
		case extenderLimitedBySource:
			admission.limitedBySourceCount += 1
		default:
			return limit
		}
		// every 429 spends one of the subnet's refusals, which is what closes
		// it at accept once they run out
		entry.refusalBucket.refill(now, refusalRate, refusalBurst)
		entry.refusalBucket.take()
		return limit
	}()
	if limit == extenderAdmitted {
		return limit, 0
	}
	return limit, self.admissionRetryAfter()
}

// Whether a new connection from this source is closed at accept, before its
// handshake: its subnet has spent every refusal of the window (A12).
func (self *ExtenderServer) closedAtAccept(remoteAddr net.Addr) bool {
	refusalRate, refusalBurst := extenderAdmissionRate(self.settings.AdmissionRefusalsPerSubnetPerMinute)
	if refusalRate <= 0 {
		return false
	}
	// an unlimited source is never refused, so it has no refusals to spend
	if self.admission.unlimitedSource(remoteAddressString(remoteAddr)) {
		return false
	}
	subnetHash, ok := self.admission.subnetHashOf(remoteAddressString(remoteAddr))
	if !ok {
		return false
	}
	now := self.admissionNow()

	self.admission.stateLock.Lock()
	defer self.admission.stateLock.Unlock()
	entry := self.admission.subnetEntries[subnetHash]
	if entry == nil || entry.refusalBucket.updateTime.IsZero() {
		// never refused
		return false
	}
	return !entry.refusalBucket.refill(now, refusalRate, refusalBurst)
}

// Drops every subnet that is indistinguishable from one never seen: its
// admission aged out of the window and its buckets are full again.
func (self *extenderAdmission) pruneWithLock(
	now time.Time,
	actionRate float64,
	actionBurst float64,
	refusalRate float64,
	refusalBurst float64,
) {
	full := func(bucket *tokenBucket, rate float64, burst float64) bool {
		if rate <= 0 || bucket.updateTime.IsZero() {
			return true
		}
		bucket.refill(now, rate, burst)
		return burst <= bucket.tokens
	}
	for subnetHash, entry := range self.subnetEntries {
		if !entry.admitTime.IsZero() && now.Sub(entry.admitTime) < extenderAdmissionWindow {
			continue
		}
		if full(&entry.actionBucket, actionRate, actionBurst) && full(&entry.refusalBucket, refusalRate, refusalBurst) {
			delete(self.subnetEntries, subnetHash)
		}
	}
}

// A Retry-After drawn uniformly in whole seconds between the bounds (A12), so
// the clients one minute turned away do not return together.
func (self *ExtenderServer) admissionRetryAfter() time.Duration {
	minSeconds := int64((self.settings.AdmissionRetryAfterMin + time.Second - 1) / time.Second)
	maxSeconds := max(minSeconds, int64(self.settings.AdmissionRetryAfterMax/time.Second))
	if maxSeconds <= 0 {
		return 0
	}
	return time.Duration(minSeconds+mathrand.Int63n(maxSeconds-minSeconds+1)) * time.Second
}
