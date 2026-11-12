package connect

// ip_remote_multi_client_family.go — the address-family half of the window
// (IPV6.md B1–B4, B6) and the placement predicate both capability axes share
// (server FP2-PROTOCOL.md §7): each exit's category and labels as the server
// gave them, the soft v6-capable minimum's shortfall and starvation state, the
// family-aware discovery order, the admission preference, the capacity swap,
// and the v6 and udp no-route replies. IPV6.md B5, the local v6 downgrade, is
// retired (FP2-PROTOCOL.md D11.8): no client-side state changes a label.
//
// Threading: an exit's category is read on the send hot path from its
// immutable channel args, and the swap mark is an atomic. The window's
// shortfall state is guarded by the window stateLock. Nothing here takes the
// parent stateLock except through the injected flow-count seam, which is
// only reached from the resize pass with no lock held.

import (
	"slices"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// enumeratedDestination is one discovery result in the order the enumerator
// hands candidates to expand: v6-capable results first while a v6 shortfall
// is open, so the expand pass evaluates them before the main fill.
type enumeratedDestination struct {
	destination MultiHopId
	stats       DestinationStats
	// Marks a sticky window's lost exit, asked for by name ahead of discovery
	// (see enumerateStickyRedial)
	stickyRedial bool
	// Marks an exit the window lost to an outage of ours, asked for by name
	// alongside discovery (see enumerateRecentExits)
	recentExit bool
}

// --- exit category ---

// IpFamily is this exit's category: the server's label for it, never a local
// inference (server FP2-PROTOCOL.md D11.8 retired IPV6.md B5's local v6
// downgrade). In selector mode it is the provider's ip label; from an older
// server, its proven `ip_family` (see addProviderDestinations). It changes
// only when a fresh find-providers2 answer relabels the destination. A bare
// fixture without args reads as legacy, which carries v4 only.
func (self *multiClientChannel) IpFamily() IpFamily {
	if self.args == nil {
		return IpFamilyLegacy
	}
	return self.args.DestinationStats.IpFamily.Normalize()
}

// The exit's labels as placement reads them: the ip label from its category
// (IpFamily) and the protocol label the server gave it. A bare fixture
// without args reads as legacy: ipv4 and dualproto.
func (self *multiClientChannel) capabilities() ProviderCapabilities {
	capabilities := ProviderCapabilities{
		Ip:       ProviderIpCapabilityIpv4,
		Protocol: ProviderProtocolCapabilityDualproto,
	}
	if self.IpFamily() == IpFamilyDualstack {
		capabilities.Ip = ProviderIpCapabilityDualstack
	}
	if self.args != nil {
		capabilities.Protocol = self.args.DestinationStats.Capabilities.Normalize().Protocol
	}
	return capabilities
}

// 1 when the server gave this tcp exit to a dualproto request as overflow.
func (self *multiClientChannel) overflowStep() int {
	if self.args == nil {
		return 0
	}
	return self.args.DestinationStats.OverflowStep
}

// The window that admitted this exit. WindowTypeAuto, which no window is, for
// a channel built outside a window (bare fixtures).
func (self *multiClientChannel) windowType() WindowType {
	if self.args == nil {
		return WindowTypeAuto
	}
	return self.args.windowType
}

// supportsFlow is the placement predicate (server FP2-PROTOCOL.md §7.2):
// whether this exit's labels can carry a flow of the ip version and
// protocol. ipv6 needs an ipv6-capable category, udp needs dualproto, and tcp
// and icmp follow every label. A zero version or an unknown protocol needs
// nothing, for callers that gather exits without a flow in hand. A
// fixed-destination exit carries every flow: it is the user's choice, with no
// alternative to prefer (see multiClientChannelArgs.FixedDestination).
func (self *multiClientChannel) supportsFlow(ipVersion int, ipProtocol IpProtocol) bool {
	if self.args != nil && self.args.FixedDestination {
		return true
	}
	need := flowNeed{
		ipVersion:  ipVersion,
		ipProtocol: ipProtocol,
	}
	return need.carriedBy(self.IpFamily(), self.capabilities())
}

// supportsFlow for the flow on the path; a nil path needs nothing.
func (self *multiClientChannel) supportsIpPath(ipPath *IpPath) bool {
	need := flowNeedOf(ipPath)
	return self.supportsFlow(need.ipVersion, need.ipProtocol)
}

// pruneConnectSuccessesWithLock prunes the proven-connect record to the
// strike window, cutting the same prefix from the parallel versions. The
// version cut is clamped, like pruneDialStrikesWithLock, for fixtures that
// inject the times alone.
//
// must be called with stateLock
func (self *multiClientChannel) pruneConnectSuccessesWithLock(horizon time.Time) {
	i := 0
	for i < len(self.connectSuccessTimes) && self.connectSuccessTimes[i].Before(horizon) {
		i += 1
	}
	if 0 < i {
		self.connectSuccessTimes = self.connectSuccessTimes[i:]
		self.connectSuccessVersions = self.connectSuccessVersions[min(i, len(self.connectSuccessVersions)):]
	}
}

// isFamilySwapVictim reports whether the resize pass chose this exit to give
// up its slot to a v6-capable candidate (see selectFamilySwapVictim). The
// mark persists across passes so the warning it causes is not undone by the
// next classification; it is cleared when the v6 fill starves.
func (self *multiClientChannel) isFamilySwapVictim() bool {
	return self.familySwapVictim.Load()
}

func (self *multiClientChannel) markFamilySwapVictim() {
	self.familySwapVictim.Store(true)
}

func (self *multiClientChannel) clearFamilySwapVictim() {
	self.familySwapVictim.Store(false)
}

// --- window shortfall state ---

// ipv6CapableShortfall is the soft v6-capable minimum's open shortfall for
// one resize pass (IPV6.md B1): how many v6-capable exits the window wants
// and holds none of. Zero when the minimum does not apply -- a fixed
// performance profile, a fixed window size or a fixed destination pins the
// membership and a soft minimum has no slot to claim -- and zero while the
// window is still forming (no healthy exit yet): during formation the
// dualstack-first main fill is the way to get a v6-capable exit, so nothing
// extra is asked for. The shortfall is the whole minimum, not the
// difference, because it is only open while the count is zero: one
// v6-capable exit satisfies the minimum however large it is set, which is
// what keeps the extra request to one candidate.
func ipv6CapableShortfall(
	windowSize WindowSizeSettings,
	fixedProfile bool,
	fixedDestination bool,
	clientCount int,
	ipv6CapableCount int,
) int {
	if fixedProfile || 0 < windowSize.FixedWindowSize || fixedDestination {
		return 0
	}
	if windowSize.WindowSizeMinIpv6Capable <= 0 || clientCount <= 0 || 0 < ipv6CapableCount {
		return 0
	}
	return windowSize.WindowSizeMinIpv6Capable
}

// setIpv6Shortfall records the v6-capable shortfall the resize pass computed:
// how many v6-capable exits the window wants and has none of. 0 closes it.
func (self *multiClientWindow) setIpv6Shortfall(shortfall int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ipv6Shortfall = max(0, shortfall)
}

// ipv6FillWanted is how many v6-capable candidates the enumerator should ask
// for ahead of the main fill: the open shortfall, unless the family is in
// its starvation backoff.
func (self *multiClientWindow) ipv6FillWanted(now time.Time) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ipv6Shortfall <= 0 || now.Before(self.ipv6StarvedUntil) {
		return 0
	}
	return self.ipv6Shortfall
}

// ipv6Starved reports whether the v6-capable fill is in its retry backoff.
func (self *multiClientWindow) ipv6Starved(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return now.Before(self.ipv6StarvedUntil)
}

// noteIpv6Starved records that the platform returned nothing v6-capable:
// the family is re-polled only after IpFamilyStarvedRetryTimeout, and any
// exit warned as a swap victim on the strength of a fill that is not coming
// is released back to selection.
func (self *multiClientWindow) noteIpv6Starved(now time.Time) {
	timeout := self.settings.IpFamilyStarvedRetryTimeout
	if timeout <= 0 {
		timeout = DefaultMultiClientSettings().IpFamilyStarvedRetryTimeout
	}
	released := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ipv6StarvedUntil = now.Add(timeout)
		for _, client := range self.clients {
			if client.isFamilySwapVictim() {
				client.clearFamilySwapVictim()
				released += 1
			}
		}
	}()
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"family_starved",
		"window", self.windowType.RankMode(),
		"family", string(IpFamilyFilterV6Capable),
		"retry", timeout,
		"released", released,
	))
}

// requestIpv6Probe asks the enumerator for one probe-only round: the
// v6-capable request alone, so the platform is consulted BEFORE the window
// gives an exit up for the candidate (see selectFamilySwapVictim). Wakes the
// enumerator.
func (self *multiClientWindow) requestIpv6Probe() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ipv6ProbeRequested = true
	}()
	self.generatorMonitor.NotifyAll()
}

// takeIpv6ProbeRequest consumes a pending probe request.
func (self *multiClientWindow) takeIpv6ProbeRequest() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	requested := self.ipv6ProbeRequested
	self.ipv6ProbeRequested = false
	return requested
}

// setIpv6CandidateReady marks whether a probe's v6-capable candidate is
// minted and waiting for expand to consume it.
func (self *multiClientWindow) setIpv6CandidateReady(ready bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ipv6CandidateReady = ready
}

func (self *multiClientWindow) hasIpv6CandidateReady() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ipv6CandidateReady
}

// ipv6FillFailureStarveCount is how many consecutive failed v6-capable
// evaluations starve the family for a retry period.
const ipv6FillFailureStarveCount = 2

// noteIpv6CandidateFailed counts a v6-capable candidate that failed its
// evaluation while the shortfall was open, and starves the family once the
// count reaches ipv6FillFailureStarveCount: a candidate the platform keeps
// offering and that keeps failing must not cost an exit per pass.
func (self *multiClientWindow) noteIpv6CandidateFailed() {
	starve := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ipv6FillFailures += 1
		if ipv6FillFailureStarveCount <= self.ipv6FillFailures {
			self.ipv6FillFailures = 0
			starve = true
		}
	}()
	if starve {
		self.noteIpv6Starved(time.Now())
	}
}

// noteIpv6CandidateAdmitted resets the failure count on a successful fill.
func (self *multiClientWindow) noteIpv6CandidateAdmitted() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ipv6FillFailures = 0
}

// ipv6Available reports whether this window holds an added, unwarned exit
// labelled to carry v6 (B6). Read live from the client set rather than from
// the last resize snapshot, so the flag follows an admission without waiting
// for the next pass.
func (self *multiClientWindow) ipv6Available() bool {
	return self.flowAvailable(flowNeed{ipVersion: 6})
}

// udpAvailable reports whether this window holds an added, unwarned exit
// labelled dualproto (server FP2-PROTOCOL.md §7.7), read live like
// ipv6Available.
func (self *multiClientWindow) udpAvailable() bool {
	return self.flowAvailable(flowNeed{ipProtocol: IpProtocolUdp})
}

// flowAvailable reports whether this window holds an added, unwarned exit
// whose labels carry the flow.
func (self *multiClientWindow) flowAvailable(need flowNeed) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range self.clients {
		if client == nil || client.IsDone() || client.isWarning() {
			continue
		}
		if client.supportsFlow(need.ipVersion, need.ipProtocol) {
			return true
		}
	}
	return false
}

// hasAddedExit reports whether this window holds any added, unwarned exit
// of any family: the window has formed.
func (self *multiClientWindow) hasAddedExit() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range self.clients {
		if client != nil && !client.IsDone() && !client.isWarning() {
			return true
		}
	}
	return false
}

// hardMinimumClientCount is the live count the hard minimum is judged
// against (see hardMinimumCount for the rule): every unwarned exit, as long
// as at least one of them can carry v4 or the destination is fixed.
func (self *multiClientWindow) hardMinimumClientCount(fixedDestination bool) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clients := []*multiClientChannel{}
	for _, client := range self.clients {
		if client == nil || client.IsDone() || client.isWarning() {
			continue
		}
		clients = append(clients, client)
	}
	return hardMinimumCount(clients, fixedDestination)
}

// hardMinimumCount is the rule the hard minimum is judged by (IPV6.md B1):
// every unwarned exit counts, as long as at least one of them can carry v4
// -- a window whose only exits are v6-only is not connected for the traffic
// that matters, so it counts as empty. A fixed destination counts whatever
// it holds: the user chose that peer whatever its family. A v6-only exit
// beside v4-capable ones counts, since it occupies a slot the soft minimum
// deliberately gave it.
func hardMinimumCount(clients []*multiClientChannel, fixedDestination bool) int {
	if fixedDestination {
		return len(clients)
	}
	for _, client := range clients {
		if client.IpFamily().SupportsIpv4() {
			return len(clients)
		}
	}
	return 0
}

// hasFamilySwapVictim reports whether an exit is already marked to give up
// its slot, so a pass never marks a second one while the first is draining.
func (self *multiClientWindow) hasFamilySwapVictim() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range self.clients {
		if client != nil && client.isFamilySwapVictim() {
			return true
		}
	}
	return false
}

// selectFamilySwapVictim picks the exit that gives up its slot to a
// v6-capable candidate when the window is at capacity (B2's "v4-only exits
// only top up when dualstack runs out", applied after the fact): among the
// healthy exits that cannot carry v6, a flowless one first -- it can be
// removed at once -- else the one carrying the least traffic, which is
// drain-warned. Lowest weight wins within each class. nil when none
// qualifies. Must be called with no lock held: the flow count takes the
// parent stateLock inside.
func (self *multiClientWindow) selectFamilySwapVictim(
	clients []*multiClientChannel,
	weights map[*multiClientChannel]float32,
) (victim *multiClientChannel, flowless bool) {
	var loaded *multiClientChannel
	for _, client := range clients {
		// the ip label decides: an exit labelled dualstack holds the slot
		if client.supportsFlow(6, IpProtocolUnknown) {
			continue
		}
		if self.flowCount(client) == 0 {
			if victim == nil || weights[client] < weights[victim] {
				victim = client
			}
			continue
		}
		if loaded == nil || weights[client] < weights[loaded] {
			loaded = client
		}
	}
	if victim != nil {
		return victim, true
	}
	return loaded, false
}

// --- discovery order ---

// enumerateDestinations is one discovery round in push order. Ahead of
// discovery, a sticky window's lost exit is asked for by name, and a round that
// finds it is that one destination alone (see enumerateStickyRedial). Any
// other window's recently lost exits are asked for by name alongside
// discovery and lead the push order, discovery filling the rest (see
// enumerateRecentExits). probeOnly is the resize pass's probe
// (requestIpv6Probe): discovery's v6-capable request alone, with neither.
func (self *multiClientWindow) enumerateDestinations(excludeDestinations []MultiHopId, probeOnly bool) ([]enumeratedDestination, error) {
	rankMode := self.requestRankMode()
	discover := func(excludeDestinations []MultiHopId) ([]enumeratedDestination, error) {
		return self.discoverDestinations(excludeDestinations, probeOnly, rankMode)
	}
	if probeOnly {
		return discover(excludeDestinations)
	}

	redial, ok, err := self.enumerateStickyRedial(excludeDestinations, rankMode)
	if err != nil {
		return nil, err
	}
	if ok {
		return []enumeratedDestination{redial}, nil
	}
	return self.enumerateRecentExits(excludeDestinations, rankMode, discover)
}

// discoverDestinations is one discovery request set in push order. A
// capability-aware generator asks in selector mode (see
// discoverDestinationsWithCapabilities). With a family-aware generator it is:
// the v6-capable request first while a v6 shortfall is open and not starved
// (sized to the shortfall), then the main v4-capable fill, which the platform
// draws dualstack-first. Duplicates across the two are dropped. probeOnly asks
// the v6-capable request alone, and nothing when no shortfall is open. A
// generator without either capability is called exactly as before and its
// destinations read as legacy. The limited window asks only a
// capability-aware generator: no other has a tcp provider to give. Every call
// carries the generator deadline, so a hung platform surfaces as an error on
// the caller's ordinary retry cadence.
func (self *multiClientWindow) discoverDestinations(excludeDestinations []MultiHopId, probeOnly bool, rankMode string) ([]enumeratedDestination, error) {
	if capabilityGenerator, ok := self.generator.(MultiClientGeneratorWithCapabilities); ok {
		return self.discoverDestinationsWithCapabilities(capabilityGenerator, excludeDestinations, probeOnly, rankMode)
	}
	if self.windowType == WindowTypeLimited {
		return []enumeratedDestination{}, nil
	}
	call := func(f func() (map[MultiHopId]DestinationStats, error)) (map[MultiHopId]DestinationStats, error) {
		return windowGeneratorCall(
			self.ctx,
			self.settings.WindowGeneratorTimeout,
			f,
			nil,
		)
	}
	ordered := []enumeratedDestination{}
	seen := map[MultiHopId]bool{}
	appendAll := func(destinations map[MultiHopId]DestinationStats) {
		for destination, stats := range destinations {
			if seen[destination] {
				continue
			}
			seen[destination] = true
			ordered = append(ordered, enumeratedDestination{
				destination: destination,
				stats:       stats,
			})
		}
	}

	familyGenerator, familyOk := self.generator.(MultiClientGeneratorWithIpFamily)
	if !familyOk {
		if probeOnly {
			// a legacy generator has no family to ask for
			return ordered, nil
		}
		destinations, err := call(func() (map[MultiHopId]DestinationStats, error) {
			return self.generator.NextDestinations(
				self.settings.WindowExpandBlockCount,
				excludeDestinations,
				rankMode,
			)
		})
		if err != nil {
			return nil, err
		}
		appendAll(destinations)
		return ordered, nil
	}

	now := time.Now()
	if wanted := self.ipv6FillWanted(now); 0 < wanted {
		destinations, err := call(func() (map[MultiHopId]DestinationStats, error) {
			return familyGenerator.NextDestinationsWithIpFamily(
				wanted,
				excludeDestinations,
				rankMode,
				IpFamilyFilterV6Capable,
			)
		})
		if err != nil {
			return nil, err
		}
		if len(destinations) == 0 {
			self.noteIpv6Starved(now)
		} else {
			appendAll(destinations)
		}
	}
	if probeOnly {
		return ordered, nil
	}
	destinations, err := call(func() (map[MultiHopId]DestinationStats, error) {
		return familyGenerator.NextDestinationsWithIpFamily(
			self.settings.WindowExpandBlockCount,
			excludeDestinations,
			rankMode,
			IpFamilyFilterV4Capable,
		)
	})
	if err != nil {
		return nil, err
	}
	appendAll(destinations)
	return ordered, nil
}

// --- admission ---

// poolAdmitOrderWithShortfall is poolAdmitOrder with one more rank above
// qualification: candidates that close the window's open family shortfall
// come first, then qualified candidates, then the rest in arrival order.
// With no shortfall it is exactly poolAdmitOrder.
func poolAdmitOrderWithShortfall(closesShortfall []bool, qualified []bool, count int) []int {
	if count <= 0 || len(qualified) == 0 {
		return nil
	}
	order := []int{}
	taken := make([]bool, len(qualified))
	pass := func(want func(i int) bool) {
		for i := range qualified {
			if !taken[i] && want(i) {
				taken[i] = true
				order = append(order, i)
			}
		}
	}
	pass(func(i int) bool { return i < len(closesShortfall) && closesShortfall[i] })
	pass(func(i int) bool { return qualified[i] })
	pass(func(i int) bool { return true })
	if count < len(order) {
		order = order[:count]
	}
	return order
}

// collapseFamilyOrder is the family key of the capacity collapse: dualstack
// exits sort first (kept), single-family exits last (shed first). 0 when the
// two are the same class.
func collapseFamilyOrder(a *multiClientChannel, b *multiClientChannel) int {
	aDualstack := a.IpFamily() == IpFamilyDualstack
	bDualstack := b.IpFamily() == IpFamilyDualstack
	switch {
	case aDualstack == bDualstack:
		return 0
	case aDualstack:
		return -1
	default:
		return 1
	}
}

// --- parent ---

// Ipv6Available reports whether any window, the limited one included, holds
// an added, unwarned exit labelled to carry v6 (B6): a limited exit labelled
// dualstack carries tcp over v6. This is the raw flag the window status
// carries.
func (self *RemoteUserNatMultiClient) Ipv6Available() bool {
	for _, window := range self.windows {
		if window.ipv6Available() {
			return true
		}
	}
	return false
}

// UdpAvailable reports whether any window holds an added, unwarned exit
// labelled dualproto (server FP2-PROTOCOL.md §7.7). The limited window's exits
// are tcp, so only the normal windows can make it true.
func (self *RemoteUserNatMultiClient) UdpAvailable() bool {
	for _, window := range self.windows {
		if window.udpAvailable() {
			return true
		}
	}
	return false
}

// Whether the normal windows have formed: one of them holds an added,
// unwarned exit. The limited window never counts: until a normal window holds
// an exit, its fill may still land one that carries any flow.
func (self *RemoteUserNatMultiClient) windowsFormed() bool {
	for windowType, window := range self.windows {
		if windowType != WindowTypeLimited && window.hasAddedExit() {
			return true
		}
	}
	return false
}

// Ipv6Unroutable is the B6 verdict: the windows have formed -- some added,
// unwarned exit exists -- and none of them can carry v6. While true a v6
// flow with no candidate is answered with no-route and the in-tunnel
// resolver should answer AAAA empty, so applications fall back to v4
// instead of blackholing. It is deliberately false while the windows are
// still forming, when the dualstack-first fill may land a v6-capable exit
// at any moment.
func (self *RemoteUserNatMultiClient) Ipv6Unroutable() bool {
	return self.windowsFormed() && !self.Ipv6Available()
}

// UdpUnroutable mirrors Ipv6Unroutable for udp (server FP2-PROTOCOL.md §7.7):
// the windows have formed and no window holds an exit labelled dualproto.
func (self *RemoteUserNatMultiClient) UdpUnroutable() bool {
	return self.windowsFormed() && !self.UdpAvailable()
}

// flowUnroutable reports whether a flow can be answered as having no route:
// the windows have formed and no window holds an added, unwarned exit whose
// labels carry it. A udp flow over v6 needs an exit labelled dualstack and
// dualproto at once, so it can be unroutable while UdpAvailable and
// Ipv6Available are both true.
func (self *RemoteUserNatMultiClient) flowUnroutable(need flowNeed) bool {
	if !self.windowsFormed() {
		return false
	}
	for _, window := range self.windows {
		if window.flowAvailable(need) {
			return false
		}
	}
	return true
}

// replyIpv6NoRoute answers a v6 flow that has no v6-capable exit anywhere
// with an icmpv6 destination-unreachable as if from the destination, so the
// application fails fast (and its own happy-eyeballs falls back to v4)
// instead of hanging until its connect timeout. Delivered on the receive path
// the provider dial-failure signal uses. Returns whether a reply went out.
func (self *RemoteUserNatMultiClient) replyIpv6NoRoute(ipPath *IpPath) bool {
	if ipPath == nil || ipPath.Version != 6 {
		return false
	}
	packet, ok := ipOosUnreachable(ipPath)
	if !ok {
		return false
	}
	self.ipv6NoRouteCount.Add(1)
	self.windowClientStats().addNoCompatible(ipPath)
	if log := loggerOrDefault(self.log); log.V(1).Enabled() {
		log.Infof("[multi]ipv6 no route %s: no v6-capable exit\n", ipPath.DestinationHostPort())
	}
	self.deliverReceivePacket(TransferPath{}, protocol.ProvideMode_Network, ipPath, packet)
	return true
}

// Ipv6NoRouteCount is how many v6 flows were answered with no-route because
// no v6-capable exit existed, for diagnostics.
func (self *RemoteUserNatMultiClient) Ipv6NoRouteCount() uint64 {
	return self.ipv6NoRouteCount.Load()
}

// replyUdpNoRoute answers a udp flow that no window holds an exit labelled to
// carry with an icmp port unreachable as if from the destination (server
// FP2-PROTOCOL.md §7.7), so a quic stack falls back to tcp at once instead of
// waiting out its handshake timeout. Delivered like replyIpv6NoRoute. Returns
// whether a reply went out.
func (self *RemoteUserNatMultiClient) replyUdpNoRoute(ipPath *IpPath) bool {
	if ipPath == nil || ipPath.Protocol != IpProtocolUdp {
		return false
	}
	packet, ok := ipOosUnreachable(ipPath)
	if !ok {
		return false
	}
	self.udpNoRouteCount.Add(1)
	self.windowClientStats().addNoCompatible(ipPath)
	if log := loggerOrDefault(self.log); log.V(1).Enabled() {
		log.Infof("[multi]udp no route %s: no exit labelled dualproto\n", ipPath.DestinationHostPort())
	}
	self.deliverReceivePacket(TransferPath{}, protocol.ProvideMode_Network, ipPath, packet)
	return true
}

// UdpNoRouteCount is how many udp flows were answered with port unreachable
// because no window held an exit labelled to carry them, for diagnostics.
func (self *RemoteUserNatMultiClient) UdpNoRouteCount() uint64 {
	return self.udpNoRouteCount.Load()
}

// candidatesForFlow narrows a candidate list to exits whose labels carry the
// flow, preserving order. The zero need returns the list unchanged.
func candidatesForFlow(clients []*multiClientChannel, need flowNeed) []*multiClientChannel {
	if need == (flowNeed{}) {
		return clients
	}
	return slices.DeleteFunc(slices.Clone(clients), func(client *multiClientChannel) bool {
		return !client.supportsFlow(need.ipVersion, need.ipProtocol)
	})
}
