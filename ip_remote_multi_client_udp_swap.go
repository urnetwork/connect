package connect

// ip_remote_multi_client_udp_swap.go — udp for the windows that ask for any
// protocol (MultiClientSettings.NormalWindowSelectors ipv4 and any, the proxy
// service's hosted devices; server FP2-PROTOCOL.md D14).
//
// Such a window takes dualproto and tcp providers in one pool, by rank alone,
// so it can end up holding only tcp exits. A udp flow then has no exit that
// carries it and is answered as unroutable (replyUdpNoRoute). Two mechanisms
// make that rare without a dualproto requirement on the window:
//
//   - The udp swap. A udp flow answered as unroutable opens the udp demand of
//     the first window it would use that asks for any protocol (quality in
//     auto, the profile's window otherwise) for UdpSwapDemandHold, every such
//     flow restarting it. While it is open and the window holds no exit that
//     carries udp, the window has a udp shortfall of one (udpSwapShortfall).
//     That is the ipv6 family swap of ip_remote_multi_client_family.go
//     (IPV6.md B2) on the protocol axis, with its structure and limits: the
//     shortfall raises the window's target within its max and leads
//     discovery with one dualproto request (udpFillWanted); at capacity the
//     resize pass first has the enumerator probe for one dualproto candidate
//     (requestUdpProbe), and only once one is minted (hasUdpCandidateReady)
//     gives a tcp-only exit's slot to it (selectUdpSwapVictim), one victim at
//     a time, and nothing while the backend is degraded. An answer with no
//     dualproto provider, or UdpSwapStarveAfterFailures failed candidates in
//     a row, starves the swap for UdpSwapStarvedRetryTimeout. The admission
//     takes a candidate that carries udp ahead of the rest while the
//     shortfall is open, and keeps the slot for one while it can still finish
//     (windowExpandPassPlan.udpShortfall). The victim returns to service once
//     the shortfall closes and the window is within its ceiling. Two limits
//     the family swap does not have: the victim is always flowless, so the
//     swap never moves or cuts a flow, and the user's Fixed IP (stickyExit)
//     never swaps, since its stable egress ip is the point. And one
//     deviation: a non-strict window warns its victim and admits one exit in
//     its place, where the family swap admits one over target, since the
//     capacity collapse that sheds an exit above the ceiling orders exits by
//     ip family and weight, not protocol, and could otherwise shed the new
//     exit.
//
//   - The single-exit preference. A window sized to one exit (a single-exit
//     fixed profile, the user's Fixed IP, or any window whose target is one)
//     fills its slot with the first provider its listing offers, and the
//     listing follows the order of the platform's answer map, a coin toss
//     between equals. Its listing puts first the first provider that carries
//     udp and ranks about as well as the best listed (singleExitListingOrder):
//     no less qualified, found at the same or a nearer location level, and a
//     server tier at most SingleExitDualprotoTierBand above. With none, the
//     listing keeps its order, so a worse provider never goes first for udp.
//
// Nothing happens until a udp flow fails or a single slot is listed, and a
// window that asks for dualproto, as every app's does, has neither: it holds
// dualproto exits first.

import (
	"time"
)

// --- channel ---

// Whether the resize pass chose this exit to give up its slot to a candidate
// that carries udp (selectUdpSwapVictim). The mark persists across passes, so
// the warning it causes survives until the collapse removes the exit or the
// swap ends (noteUdpStarved, or the shortfall closing within the ceiling).
func (self *multiClientChannel) isUdpSwapVictim() bool {
	return self.udpSwapVictim.Load()
}

func (self *multiClientChannel) markUdpSwapVictim() {
	self.udpSwapVictim.Store(true)
}

func (self *multiClientChannel) clearUdpSwapVictim() {
	self.udpSwapVictim.Store(false)
}

// --- window ---

// Whether this window's normal selectors take every protocol label. The
// limited window never does.
func (self *multiClientWindow) asksAnyProtocol() bool {
	return self.windowType != WindowTypeLimited && self.providerSelectors().Protocol == ProviderProtocolCapabilityAny
}

// A udp flow whose first window is this one found no exit to carry it: opens
// or extends the window's udp demand, and wakes the resize pass when it
// opens. A window that does not ask for any protocol ignores it, as does every
// window with a non-positive UdpSwapDemandHold.
func (self *multiClientWindow) noteUdpDemand(now time.Time) {
	if self.settings == nil || self.settings.UdpSwapDemandHold <= 0 || !self.asksAnyProtocol() {
		return
	}
	opened := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		opened = !now.Before(self.udpDemandUntil)
		self.udpDemandUntil = now.Add(self.settings.UdpSwapDemandHold)
	}()
	if !opened {
		return
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"udp_demand",
		"window", self.windowType.RankMode(),
		"hold", self.settings.UdpSwapDemandHold,
	))
	if self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}

// Whether the window's udp demand is open.
func (self *multiClientWindow) udpDemandOpen(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return now.Before(self.udpDemandUntil)
}

// The udp shortfall the resize pass computes: one while the window's udp
// demand is open, it asks for any protocol, and none of its udpCapableCount
// healthy exits carries udp; otherwise none. A fixed destination and a
// profile's inactive window have none, as for ipv6 (ipv6CapableShortfall),
// and so does the user's Fixed IP (stickyExit).
func (self *multiClientWindow) udpSwapShortfall(now time.Time, fixedDestination bool, inactive bool, stickyExit bool, udpCapableCount int) int {
	if fixedDestination || inactive || stickyExit || 0 < udpCapableCount || !self.asksAnyProtocol() || !self.udpDemandOpen(now) {
		return 0
	}
	return 1
}

// Records the udp shortfall the resize pass computed. 0 closes it.
func (self *multiClientWindow) setUdpShortfall(shortfall int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.udpShortfall = max(0, shortfall)
}

// How many dualproto candidates the enumerator asks for ahead of the main
// fill: the open udp shortfall, unless the swap is starved.
func (self *multiClientWindow) udpFillWanted(now time.Time) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.udpShortfall <= 0 || now.Before(self.udpStarvedUntil) {
		return 0
	}
	return self.udpShortfall
}

// Whether the udp swap is waiting out a starved retry.
func (self *multiClientWindow) udpStarved(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return now.Before(self.udpStarvedUntil)
}

// The platform offered no dualproto provider, or too many candidates failed:
// the swap asks again only after UdpSwapStarvedRetryTimeout, and a victim
// warned for a candidate that is not coming returns to selection.
func (self *multiClientWindow) noteUdpStarved(now time.Time) {
	timeout := self.settings.UdpSwapStarvedRetryTimeout
	if timeout <= 0 {
		timeout = DefaultMultiClientSettings().UdpSwapStarvedRetryTimeout
	}
	released := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.udpStarvedUntil = now.Add(timeout)
		self.udpFillFailures = 0
		for _, client := range self.clients {
			if client.isUdpSwapVictim() {
				client.clearUdpSwapVictim()
				released += 1
			}
		}
	}()
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"udp_starved",
		"window", self.windowType.RankMode(),
		"retry", timeout,
		"released", released,
	))
}

// Asks the enumerator for one probe-only round, so the platform is asked for
// a dualproto candidate before the window gives an exit up for it. Wakes the
// enumerator.
func (self *multiClientWindow) requestUdpProbe() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.udpProbeRequested = true
	}()
	self.generatorMonitor.NotifyAll()
}

// Consumes a pending udp probe request.
func (self *multiClientWindow) takeUdpProbeRequest() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	requested := self.udpProbeRequested
	self.udpProbeRequested = false
	return requested
}

// Marks whether a probe's dualproto candidate is minted and waiting for
// expansion to take it, which is what licenses the swap.
func (self *multiClientWindow) setUdpCandidateReady(ready bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.udpCandidateReady = ready
}

func (self *multiClientWindow) hasUdpCandidateReady() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.udpCandidateReady
}

// A dualproto candidate failed its evaluation while the udp shortfall was
// open; UdpSwapStarveAfterFailures in a row starve the swap.
func (self *multiClientWindow) noteUdpCandidateFailed() {
	starve := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.udpFillFailures += 1
		starve = max(1, self.settings.UdpSwapStarveAfterFailures) <= self.udpFillFailures
	}()
	if starve {
		self.noteUdpStarved(time.Now())
	}
}

// A candidate that carries udp was admitted: the failure count starts over.
func (self *multiClientWindow) noteUdpCandidateAdmitted() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.udpFillFailures = 0
}

// Whether an exit is already marked to give up its slot to a candidate that
// carries udp, so a pass never marks a second one while the first is out.
func (self *multiClientWindow) hasUdpSwapVictim() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range self.clients {
		if client != nil && client.isUdpSwapVictim() {
			return true
		}
	}
	return false
}

// Picks the exit that gives up its slot to a candidate that carries udp: the
// flowless healthy exit with the least weight among those that cannot carry
// udp, nil when there is none. Must be called with no lock held: the flow
// count takes the parent stateLock.
func (self *multiClientWindow) selectUdpSwapVictim(
	clients []*multiClientChannel,
	weights map[*multiClientChannel]float32,
) *multiClientChannel {
	var victim *multiClientChannel
	for _, client := range clients {
		// the protocol label decides: an exit labelled dualproto holds the slot
		if client.supportsFlow(0, IpProtocolUdp) || 0 < self.flowCount(client) {
			continue
		}
		if victim == nil || weights[client] < weights[victim] {
			victim = client
		}
	}
	return victim
}

// --- parent ---

// A udp flow found no exit to carry it in windowTypes, the windows it could
// use in the order it would use them: the first of them that asks for any
// protocol opens its udp demand. One window's exit is enough for the flow.
func (self *RemoteUserNatMultiClient) noteUdpDemand(windowTypes []WindowType) {
	for _, windowType := range windowTypes {
		if window := self.windows[windowType]; window != nil && window.asksAnyProtocol() {
			window.noteUdpDemand(time.Now())
			return
		}
	}
}

// --- the single-exit preference ---

// Whether this window's listing is ordered for its one exit: it asks for any
// protocol and its request target is one (a single-exit fixed profile, the
// user's Fixed IP, or any window sized to one exit).
func (self *multiClientWindow) singleExitListingPreferred(providerSelect FindProvidersSelect) bool {
	return self.asksAnyProtocol() && providerSelect.TargetCount <= 1
}

// What the single-exit preference compares a listed provider by: whether it
// is qualified (providerQualifiedFunc, the order the admission already
// prefers), the location level the platform found it at, and its server tier
// (DestinationStats.Tier, lower is better).
type singleExitRank struct {
	qualified     bool
	locationLevel int
	tier          int
}

// The rank of a listed provider.
func (self *multiClientWindow) singleExitRankOf(enumerated enumeratedDestination) singleExitRank {
	return singleExitRank{
		qualified:     self.providerQualifiedFunc != nil && self.providerQualifiedFunc(enumerated.destination),
		locationLevel: locationMatchLevel(enumerated.stats.LocationMatch),
		tier:          enumerated.stats.Tier,
	}
}

// How far from the requested location the platform found a provider: exact
// 0, region 1, country 2, and a level this build does not know 3.
func locationMatchLevel(locationMatch LocationMatch) int {
	switch locationMatch.Normalize() {
	case LocationMatchExact:
		return 0
	case LocationMatchRegion:
		return 1
	case LocationMatchCountry:
		return 2
	default:
		return 3
	}
}

// Whether the rank is better than other: qualified first, then the nearer
// location level, then the lower tier.
func (self singleExitRank) better(other singleExitRank) bool {
	if self.qualified != other.qualified {
		return self.qualified
	}
	if self.locationLevel != other.locationLevel {
		return self.locationLevel < other.locationLevel
	}
	return self.tier < other.tier
}

// Whether the rank is about as good as best: no less qualified, found at the
// same or a nearer location level, and a tier at most tierBand above best's.
// This is the preference's tie band.
func (self singleExitRank) rankedWith(best singleExitRank, tierBand int) bool {
	if best.qualified && !self.qualified {
		return false
	}
	return self.locationLevel <= best.locationLevel && self.tier <= best.tier+max(0, tierBand)
}

// The listing of a window sized to one exit, whose first provider takes the
// slot: the first listed provider that carries udp and ranks with the best
// listed one moves to the front, and the rest keep their order. With none,
// or one already first, the listing is unchanged.
func singleExitListingOrder(listed []enumeratedDestination, ranks []singleExitRank, tierBand int) []enumeratedDestination {
	if len(listed) < 2 || listed[0].stats.Capabilities.CarriesUdp() {
		return listed
	}
	best := ranks[0]
	for _, rank := range ranks[1:] {
		if rank.better(best) {
			best = rank
		}
	}
	for i, enumerated := range listed {
		if enumerated.stats.Capabilities.CarriesUdp() && ranks[i].rankedWith(best, tierBand) {
			ordered := make([]enumeratedDestination, 0, len(listed))
			ordered = append(ordered, enumerated)
			ordered = append(ordered, listed[:i]...)
			return append(ordered, listed[i+1:]...)
		}
	}
	return listed
}
