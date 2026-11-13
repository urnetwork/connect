package connect

// ip_remote_multi_client_limited.go — find-providers2's selector mode in the
// windows, and the limited window (server FP2-PROTOCOL.md §7.4-§7.7).
//
// Every window of a capability-aware generator asks with a `select`. A normal
// window asks for dualstack and dualproto providers with its target, its ipv6
// minimum and the clients it holds of each label; the server fills its ipv6
// need first, then its protocol's providers, and only when those cannot reach
// the target, tcp providers as overflow. The limited window asks for ipv4 and
// tcp providers: the providers whose udp failed, which the hard gate kept out
// of every window before. Its clients join the race of every flow they can
// carry (coalesceOrderedClients), so tcp traffic reaches them while udp never
// does, and the race orders them with the selected window's by the same
// weighted metrics (mergeRaceField).
//
// The limited window is sized from the normal windows: its target is
// max(1, floor(fraction x S)), S the largest target among the active normal
// windows as their last resize passes computed it before the standing reserve,
// and its hard max follows the largest active normal hard max the same way.
// The default fraction is 1, the same size as the normal window (D8). Its
// rank mode is the performance profile's, quality in auto (D10). It does
// not fill without a capability-aware generator, while the server is known
// not to echo `select`, under the user's Fixed IP (a second selectable exit
// would change the egress ip) or with a non-positive fraction; and a fixed
// destination has none at all.
//
// Threading: each window's request sizing and published sizing are guarded by
// its stateLock. The limited window reads its siblings' published sizing
// through the parent's hooks, one sibling lock at a time and with no lock of
// its own held. The hooks are stored once, after the parent has built every
// window, so a hook never reads the windows map while it is being built.

import (
	"math"
	"slices"
	"time"
)

// What a normal window's resize pass publishes for the limited window's size.
type windowSizing struct {
	// whether a resize pass has published this: the limited window sizes
	// itself only once every normal window has, so it never starts from a
	// partial view
	published bool
	// whether the profile routes flows through this window
	active bool
	// the target before the standing reserve
	targetSize int
	// the hard max; 0 is unbounded
	hardMax int
}

// What the limited window's size is computed from.
type limitedSizing struct {
	// whether the limited window may fill at all (see the file header)
	enabled  bool
	fraction float64
	// LimitedWindowSizeMax; non-positive is no cap
	sizeMax int
	// the largest target among the active normal windows; 0 while none has
	// computed one
	targetSize int
	// the largest bounded hard max among them, and whether one of them is
	// unbounded
	hardMax          int
	hardMaxUnbounded bool
}

// The parent's hooks a window reaches the limited window's sizing through.
type limitedWindowHooks struct {
	// the normal windows' published sizing and the limited window's settings
	sizing func() limitedSizing
	// wakes the limited window's resize pass
	wake func()
}

// Whether a fraction turns the limited window on: positive and finite.
func limitedWindowFractionEnabled(fraction float64) bool {
	return 0 < fraction && !math.IsInf(fraction, 0)
}

// max(1, floor(fraction x count)), bounded so a pathological fraction cannot
// overflow the conversion.
func limitedWindowShare(fraction float64, count int) int {
	share := math.Floor(fraction * float64(count))
	if math.IsNaN(share) || share < 1 {
		return 1
	}
	return int(min(share, float64(math.MaxInt32)))
}

// The limited window's size (server FP2-PROTOCOL.md §7.4): min = max =
// target, so its resize pass holds exactly its target whatever its traffic,
// and the hard max it may never exceed. A target of 0 is no limited window.
func limitedWindowSize(sizing limitedSizing) WindowSizeSettings {
	windowSize := WindowSizeSettings{
		WindowSizeReconnectScale: 1,
	}
	if !sizing.enabled || !limitedWindowFractionEnabled(sizing.fraction) || sizing.targetSize <= 0 {
		return windowSize
	}
	targetSize := limitedWindowShare(sizing.fraction, sizing.targetSize)
	hardMax := 0
	if !sizing.hardMaxUnbounded && 0 < sizing.hardMax {
		hardMax = limitedWindowShare(sizing.fraction, sizing.hardMax)
	}
	if 0 < sizing.sizeMax {
		targetSize = min(targetSize, sizing.sizeMax)
		if hardMax <= 0 {
			hardMax = sizing.sizeMax
		} else {
			hardMax = min(hardMax, sizing.sizeMax)
		}
	}
	if 0 < hardMax {
		targetSize = min(targetSize, hardMax)
	}
	windowSize.WindowSizeMin = targetSize
	windowSize.WindowSizeMax = targetSize
	windowSize.WindowSizeHardMax = hardMax
	return windowSize
}

// The limited window's size this pass. Must be called with no lock held: the
// hooks take each normal window's lock in turn.
func (self *multiClientWindow) limitedWindowSize() WindowSizeSettings {
	hooks := self.limitedHooks.Load()
	if hooks == nil || self.stickyExit() {
		// the user's Fixed IP: a limited exit would be a second egress ip
		return limitedWindowSize(limitedSizing{})
	}
	return limitedWindowSize(hooks.sizing())
}

// The limited window's sizing from the normal windows' last resize passes.
// The windows map is complete and immutable by the time the hooks call this.
// Takes each normal window's lock in turn, never two at once.
func (self *RemoteUserNatMultiClient) limitedWindowSizing() limitedSizing {
	sizing := limitedSizing{
		fraction: self.LimitedWindowSizeFraction(),
		sizeMax:  self.settings.LimitedWindowSizeMax,
	}
	generator, capable := self.generator.(MultiClientGeneratorWithCapabilities)
	if !capable {
		// no labels, so no tcp provider to give
		return sizing
	}
	if selectorMode, known := generator.SelectorMode(); known && !selectorMode {
		// the server predates selector mode: the limited window does not fill
		// (FP2-PROTOCOL.md §5.4), until an answer echoes `select`
		return sizing
	}
	sizing.enabled = true
	for windowType, window := range self.windows {
		if windowType == WindowTypeLimited {
			continue
		}
		published := window.publishedSizingSnapshot()
		if !published.published {
			// a normal window has not sized itself yet: nothing to fill
			// until it has, when its publication wakes the limited window
			sizing.targetSize = 0
			return sizing
		}
		if !published.active {
			continue
		}
		sizing.targetSize = max(sizing.targetSize, published.targetSize)
		if published.hardMax <= 0 {
			sizing.hardMaxUnbounded = true
		} else {
			sizing.hardMax = max(sizing.hardMax, published.hardMax)
		}
	}
	return sizing
}

// Publishes this normal window's sizing for the limited window, and wakes it
// when the sizing changed. Must be called with no lock held.
func (self *multiClientWindow) publishSizing(sizing windowSizing) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.publishedSizing != sizing {
			self.publishedSizing = sizing
			changed = true
		}
	}()
	if changed {
		if hooks := self.limitedHooks.Load(); hooks != nil {
			hooks.wake()
		}
	}
}

// The sizing this window last published, for the limited window's sizing.
func (self *multiClientWindow) publishedSizingSnapshot() windowSizing {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.publishedSizing
}

// --- requests ---

// The ipv6 minimum a window's find-providers2 requests carry (D11.1): its
// WindowSizeMinIpv6Capable, but at least 1 for the window an ipv6 flow
// depends on -- the quality window in auto, and the profile's window when the
// profile fixes one, which is then the only window. A deviation from reading
// the setting alone: the mobile policy and every fixed profile leave the
// setting at 0, and a request without an ipv6 need no longer draws dualstack
// first (FP2-PROTOCOL.md §6.3), so those windows would lose the one dualstack
// exit today's draw gives them. The limited window has no ipv6 need.
func requestDualstackTargetCount(windowType WindowType, fixedWindowType *WindowType, windowSize WindowSizeSettings) int {
	if windowType == WindowTypeLimited {
		return 0
	}
	dualstackTargetCount := max(0, windowSize.WindowSizeMinIpv6Capable)
	if (fixedWindowType == nil && windowType == WindowTypeQuality) ||
		(fixedWindowType != nil && *fixedWindowType == windowType) {
		dualstackTargetCount = max(1, dualstackTargetCount)
	}
	return dualstackTargetCount
}

// Sets what this window's requests carry from its resize pass. Must be
// called with no lock held.
func (self *multiClientWindow) setRequestSizing(targetCount int, dualstackTargetCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requestTargetCount = targetCount
	self.requestDualstackTargetCount = dualstackTargetCount
}

// Sets what this window's requests carry before its first resize pass, from
// its configuration: the fixed size of an active fixed window, or the
// minimum, which is what the first pass computes for a window that holds no
// client yet. The limited window starts at 0, which holds its listing until
// its first pass sized it. Called by the constructor before the window's
// goroutines start.
func (self *multiClientWindow) initRequestSizing() {
	if self.windowType == WindowTypeLimited {
		return
	}
	fixedDestination := false
	if self.generator != nil {
		_, fixedDestination = self.generator.FixedDestinationSize()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var fixedWindowType *WindowType
	var windowSize WindowSizeSettings
	if profileWindowType, profileWindowSize, ok := self.performanceProfile.FixedWindow(); ok {
		fixedWindowType = &profileWindowType
		windowSize = profileWindowSize
	} else {
		windowSize = self.defaultWindowSizeWithLock()
	}
	targetCount := windowSize.WindowSizeMin
	if 0 < windowSize.FixedWindowSize {
		targetCount = windowSize.FixedWindowSize
		if fixedWindowType != nil && *fixedWindowType != self.windowType {
			targetCount = 0
		}
	}
	self.requestTargetCount = targetCount
	self.requestDualstackTargetCount = requestDualstackTargetCount(self.windowType, fixedWindowType, windowSize)
	if fixedDestination || targetCount <= 0 {
		self.requestDualstackTargetCount = 0
	}
	self.ipv6Shortfall = self.requestDualstackTargetCount
}

// The rank mode the limited window asks with (D10): the performance
// profile's window type, quality in auto.
func limitedWindowRankMode(performanceProfile *PerformanceProfile) string {
	if windowType, _, ok := performanceProfile.FixedWindow(); ok {
		return windowType.RankMode()
	}
	return WindowTypeQuality.RankMode()
}

// The rank mode this window's find-providers2 requests carry: a normal
// window's own, and the profile's for the limited window, read when each
// request is built.
func (self *multiClientWindow) requestRankMode() string {
	if self.windowType != WindowTypeLimited {
		return self.windowType.RankMode()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return limitedWindowRankMode(self.performanceProfile)
}

// The `select` this window's requests carry (FP2-PROTOCOL.md §7.4). The held
// counts are the window's live, unwarned clients: a warned client is being
// replaced, so it holds no slot the request should count.
func (self *multiClientWindow) providerSelect() FindProvidersSelect {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	providerSelect := FindProvidersSelect{
		Ip:                   ProviderIpCapabilityDualstack,
		Protocol:             ProviderProtocolCapabilityDualproto,
		TargetCount:          max(1, self.requestTargetCount),
		DualstackTargetCount: max(0, self.requestDualstackTargetCount),
	}
	if self.windowType == WindowTypeLimited {
		providerSelect.Ip = ProviderIpCapabilityIpv4
		providerSelect.Protocol = ProviderProtocolCapabilityTcp
		providerSelect.DualstackTargetCount = 0
	}
	for _, client := range self.clients {
		if client == nil || client.IsDone() || client.isWarning() {
			continue
		}
		capabilities := client.capabilities()
		if capabilities.Protocol == providerSelect.Protocol {
			providerSelect.HeldCount += 1
		}
		if capabilities.Ip == ProviderIpCapabilityDualstack {
			providerSelect.HeldDualstackCount += 1
		}
	}
	return providerSelect
}

// Whether the limited window lists nothing this round: its last resize pass
// sized it to nothing to fill. An expansion that asks again wakes the
// enumerator. False for every other window.
func (self *multiClientWindow) limitedListingHeld() bool {
	if self.windowType != WindowTypeLimited {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.requestTargetCount <= 0
}

// Whether a provider this window was given may join it. The limited window
// keeps only providers labelled tcp: a server that predates selector mode
// answers its request with hard-gate providers, which carry no label and so
// read dualproto, and a tcp request never receives a dualproto provider
// (FP2-PROTOCOL.md §6.3).
func (self *multiClientWindow) admitsCapabilities(capabilities ProviderCapabilities) bool {
	if self.windowType != WindowTypeLimited {
		return true
	}
	return capabilities.Normalize().Protocol == ProviderProtocolCapabilityTcp
}

// One selector-mode answer.
type capabilityDiscovery struct {
	destinations map[MultiHopId]DestinationStats
	selectorMode bool
}

// discoverDestinations with a capability-aware generator: every request
// carries the window's `select` (FP2-PROTOCOL.md §7.4).
//   - The limited window asks once, ipv4 and tcp, and keeps the tcp providers.
//   - A normal window asks once, v4-capable, and the server serves its ipv6
//     need from that request. The separate v6-capable request of IPV6.md B2 is
//     sent only to a server that does not echo `select`: ahead of the main
//     fill, as before, once the server is known; after it on the answer that
//     first shows it. A selector-mode answer without a dualstack provider
//     while the window's shortfall is open starves the family, as an empty
//     v6-capable answer does.
//   - probeOnly asks the v6-capable request alone, with the same `select`.
func (self *multiClientWindow) discoverDestinationsWithCapabilities(
	generator MultiClientGeneratorWithCapabilities,
	excludeDestinations []MultiHopId,
	probeOnly bool,
	rankMode string,
) ([]enumeratedDestination, error) {
	providerSelect := self.providerSelect()
	call := func(count int, ipFamily IpFamilyFilter) (capabilityDiscovery, error) {
		return windowGeneratorCall(
			self.ctx,
			self.settings.WindowGeneratorTimeout,
			func() (capabilityDiscovery, error) {
				destinations, selectorMode, err := generator.NextDestinationsWithCapabilities(
					count,
					excludeDestinations,
					rankMode,
					ipFamily,
					providerSelect,
				)
				return capabilityDiscovery{
					destinations: destinations,
					selectorMode: selectorMode,
				}, err
			},
			nil,
		)
	}
	ordered := []enumeratedDestination{}
	seen := map[MultiHopId]bool{}
	appendAll := func(destinations map[MultiHopId]DestinationStats) {
		for destination, stats := range destinations {
			if seen[destination] || !self.admitsCapabilities(stats.Capabilities) {
				continue
			}
			seen[destination] = true
			ordered = append(ordered, enumeratedDestination{
				destination: destination,
				stats:       stats,
			})
		}
		if providerSelect.HeldDualstackCount < providerSelect.DualstackTargetCount {
			// The API returns a map after selecting its single need candidate.
			// Keep that candidate ahead of IPv4 fill before bounded evaluation
			// and identity allocation can consume the whole pass budget.
			slices.SortStableFunc(ordered, func(a enumeratedDestination, b enumeratedDestination) int {
				if a.stats.IpFamily.SupportsIpv6() == b.stats.IpFamily.SupportsIpv6() {
					return 0
				}
				if a.stats.IpFamily.SupportsIpv6() {
					return -1
				}
				return 1
			})
		}
	}

	if self.windowType == WindowTypeLimited {
		if probeOnly {
			return ordered, nil
		}
		answer, err := call(self.settings.WindowExpandBlockCount, IpFamilyFilterV4Capable)
		if err != nil {
			return nil, err
		}
		appendAll(answer.destinations)
		return ordered, nil
	}

	now := time.Now()
	wanted := self.ipv6FillWanted(now)
	ipv6Fill := func() error {
		answer, err := call(wanted, IpFamilyFilterV6Capable)
		if err != nil {
			return err
		}
		if len(answer.destinations) == 0 {
			self.noteIpv6Starved(now)
		} else {
			appendAll(answer.destinations)
		}
		return nil
	}
	if probeOnly {
		if 0 < wanted {
			if err := ipv6Fill(); err != nil {
				return nil, err
			}
		}
		return ordered, nil
	}
	selectorMode, known := generator.SelectorMode()
	legacyServer := known && !selectorMode
	if 0 < wanted && legacyServer {
		if err := ipv6Fill(); err != nil {
			return nil, err
		}
	}
	answer, err := call(self.settings.WindowExpandBlockCount, IpFamilyFilterV4Capable)
	if err != nil {
		return nil, err
	}
	appendAll(answer.destinations)
	if 0 < wanted && !legacyServer {
		if !answer.selectorMode {
			// the answer that first shows a server without selector mode
			if err := ipv6Fill(); err != nil {
				return nil, err
			}
		} else if !destinationsCarryIpv6(answer.destinations) {
			self.noteIpv6Starved(now)
		}
	}
	return ordered, nil
}

// One by-id lookup for this window (a sticky redial, a recent exit): in
// selector mode with the window's `select` when the generator has it, so the
// named provider comes back with its labels, else as before. Must be called
// with no lock held.
func (self *multiClientWindow) lookupClientId(
	clientIdGenerator MultiClientGeneratorWithClientId,
	clientId Id,
	excludeDestinations []MultiHopId,
	rankMode string,
) (capabilityDiscovery, error) {
	if generator, ok := self.generator.(MultiClientGeneratorWithCapabilities); ok {
		destinations, selectorMode, err := generator.NextDestinationsForClientIdWithCapabilities(
			clientId,
			excludeDestinations,
			rankMode,
			self.providerSelect(),
		)
		return capabilityDiscovery{
			destinations: destinations,
			selectorMode: selectorMode,
		}, err
	}
	destinations, err := clientIdGenerator.NextDestinationsForClientId(clientId, excludeDestinations, rankMode)
	return capabilityDiscovery{
		destinations: destinations,
	}, err
}

// The stats a remembered exit is dialed again with: as it was discovered,
// with the labels of a fresh selector-mode answer, since only a fresh answer
// changes a label (FP2-PROTOCOL.md D11.8). Any other answer keeps the
// remembered labels.
func relabelledStats(remembered DestinationStats, fresh DestinationStats, selectorMode bool) DestinationStats {
	if !selectorMode {
		return remembered
	}
	remembered.IpFamily = fresh.IpFamily
	remembered.Capabilities = fresh.Capabilities
	return remembered
}

// Whether any destination is labelled to carry v6.
func destinationsCarryIpv6(destinations map[MultiHopId]DestinationStats) bool {
	for _, stats := range destinations {
		if stats.IpFamily.SupportsIpv6() {
			return true
		}
	}
	return false
}

// --- the race ---

// The field a selected window offers a flow (FP2-PROTOCOL.md §7.5): its own
// race candidates for the flow and the limited window's, merged into one
// weighted order (mergeRaceField). Each window narrows to its own best rank
// first (raceCandidates): one rank gate across both would drop the limited
// window's exits, whose overflow ranks sit below the window's. The caller
// runs every filter on the merged field, so the limited window's exits race
// the window's on equal terms. They are tcp, so they never join a udp race;
// and when the window offers nothing the flow can use, a tcp flow races them
// alone. offerChanged and limitedOfferChanged are armed for the formation
// wait, each window's under its own lock (see orderedClients).
func (self *RemoteUserNatMultiClient) raceField(
	window *multiClientWindow,
	need flowNeed,
	offerChanged *<-chan struct{},
	limitedOfferChanged *<-chan struct{},
) []*multiClientChannel {
	field := self.raceCandidates(window, need, offerChanged)
	if limitedWindow := self.windows[WindowTypeLimited]; limitedWindow != nil && limitedWindow != window {
		field = self.mergeRaceField(field, self.raceCandidates(limitedWindow, need, limitedOfferChanged))
	}
	return field
}

// raceField widened across tiers, for a flow whose service failed on the
// window's best rank: both windows' cross-tier offers, merged.
func (self *RemoteUserNatMultiClient) crossTierRaceField(window *multiClientWindow, need flowNeed) []*multiClientChannel {
	field := window.orderedClientsCrossTierForFlow(need)
	if limitedWindow := self.windows[WindowTypeLimited]; limitedWindow != nil && limitedWindow != window {
		field = self.mergeRaceField(field, limitedWindow.orderedClientsCrossTierForFlow(need))
	}
	return field
}

// raceField's benched fallback, when the cross-tier field is empty: both
// windows' last-resort offers, merged.
func (self *RemoteUserNatMultiClient) lastResortRaceField(window *multiClientWindow, need flowNeed) []*multiClientChannel {
	field := window.lastResortClientsForFlow(need)
	if limitedWindow := self.windows[WindowTypeLimited]; limitedWindow != nil && limitedWindow != window {
		field = self.mergeRaceField(field, limitedWindow.lastResortClientsForFlow(need))
	}
	return field
}

// One race order over a window's field and the limited window's (owner,
// 2026-11-13: "the windows need to merge in order based on the weighted
// metrics"). The union is ordered as one window orders its offer
// (orderedClients): a weighted shuffle by each exit's expected throughput
// (raceWeight) at StatsWindowEntropy. A strong limited exit can lead the race
// and a weak one follow, by its metrics alone. The race then puts each
// window's top first (raceWindowTopsFirst) before it bounds. An empty part
// leaves the other in its own window's order, so a flow with no limited exit
// to race keeps exactly the window's field.
//
// The weights are read without coalescing the event buckets, as
// exitMetricsSnapshot reads them on this path; the windows' own passes
// coalesce. A benched last-resort exit is weighed by the same metric.
func (self *RemoteUserNatMultiClient) mergeRaceField(field []*multiClientChannel, limitedField []*multiClientChannel) []*multiClientChannel {
	if len(limitedField) == 0 {
		return field
	}
	if len(field) == 0 {
		return limitedField
	}
	merged := make([]*multiClientChannel, 0, len(field)+len(limitedField))
	merged = append(merged, field...)
	merged = append(merged, limitedField...)
	weights := make(map[*multiClientChannel]float32, len(merged))
	for _, client := range merged {
		// an exit that ended since its pass still has its stats filled in
		stats, _ := client.windowStatsWithCoalesce(false)
		weights[client] = raceWeight(stats)
	}
	raceWeightedShuffle(self.settings, merged, weights)
	return merged
}

// The race's order over a field that merges windows (owner, 2026-11-13: "if
// it's 2, i want to make sure a member of the limited window is always tried.
// I want to combine that with the merged sort, but keep the top from each
// list to be always included"). Each window's top, its first candidate in
// the field whose contract requests are not held, comes first, the tops
// ordered between themselves by the weighted metrics (raceWeight at
// StatsWindowEntropy). Every other candidate follows in the field's own
// order, the merged weighted order (mergeRaceField). So a race bounded to two
// tries the best exit of the normal window and the best of the limited window
// together, and an unbounded race still tries every one. In a merged field
// the first of a window is that window's own weighted top: at the default
// entropy of 0, the weighted order of the union orders each window's exits
// among themselves as a weighted order of that window alone would.
//
// The send path calls it after every filter and right before the bound, so a
// top that a filter removed is replaced by its window's next. A field of one
// window, every udp field among them, comes back as it is, as does a field in
// which fewer than two windows have a candidate that is not held. Held
// clients are then moved last by contractRateLimitHeldLast, which keeps the
// tops first. Returns a reordered copy, so the caller's list keeps its order.
func raceWindowTopsFirst(clients []*multiClientChannel, settings *MultiClientSettings, now time.Time) []*multiClientChannel {
	if len(clients) < 2 {
		return clients
	}
	tops := make([]*multiClientChannel, 0, 2)
	windowTypes := make([]WindowType, 0, 2)
	for _, client := range clients {
		windowType := client.windowType()
		if slices.Contains(windowTypes, windowType) || contractRateLimitHeldChannel(client, now) {
			continue
		}
		windowTypes = append(windowTypes, windowType)
		tops = append(tops, client)
	}
	if len(tops) < 2 {
		return clients
	}
	weights := make(map[*multiClientChannel]float32, len(tops))
	for _, client := range tops {
		stats, _ := client.windowStatsWithCoalesce(false)
		weights[client] = raceWeight(stats)
	}
	raceWeightedShuffle(settings, tops, weights)
	ordered := make([]*multiClientChannel, 0, len(clients))
	ordered = append(ordered, tops...)
	for _, client := range clients {
		if !slices.Contains(tops, client) {
			ordered = append(ordered, client)
		}
	}
	return ordered
}

// --- parent ---

// The process's window client counters (WindowClientStats). Nil counts
// nothing, as in every app and bare fixture.
func (self *RemoteUserNatMultiClient) windowClientStats() *WindowClientStats {
	if self == nil || self.settings == nil {
		return nil
	}
	return self.settings.WindowClientStats
}

// The clients the limited window holds: installed and not done, warned ones
// included. 0 without a limited window.
func (self *RemoteUserNatMultiClient) LimitedWindowSize() int {
	window := self.windows[WindowTypeLimited]
	if window == nil {
		return 0
	}
	count := 0
	for _, client := range window.unorderedClients() {
		if !client.IsDone() {
			count += 1
		}
	}
	return count
}

// The limited window's target as its last resize pass computed it. 0 when it
// does not fill and without a limited window.
func (self *RemoteUserNatMultiClient) LimitedWindowTargetSize() int {
	window := self.windows[WindowTypeLimited]
	if window == nil {
		return 0
	}
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	return max(0, window.requestTargetCount)
}

// The limited window's size fraction in force:
// MultiClientSettings.LimitedWindowSizeFraction until
// SetLimitedWindowSizeFraction overrides it.
func (self *RemoteUserNatMultiClient) LimitedWindowSizeFraction() float64 {
	return math.Float64frombits(self.limitedWindowSizeFractionBits.Load())
}

// Overrides MultiClientSettings.LimitedWindowSizeFraction for this multi
// client, for the developer menu. A fraction that turns the limited window
// off retires its clients at once; any other change takes effect at the
// limited window's next resize pass, which it wakes, and a smaller hard max
// sheds the excess clients there.
func (self *RemoteUserNatMultiClient) SetLimitedWindowSizeFraction(fraction float64) {
	previous := math.Float64frombits(self.limitedWindowSizeFractionBits.Swap(math.Float64bits(fraction)))
	window := self.windows[WindowTypeLimited]
	if window == nil {
		return
	}
	if limitedWindowFractionEnabled(previous) && !limitedWindowFractionEnabled(fraction) {
		window.shuffle()
		return
	}
	window.resizeMonitor.NotifyAll()
}
