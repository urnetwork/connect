package connect

// ip_remote_multi_client_recent_exits.go — the exits a window lost to an
// outage of ours, asked for again before discovery.
//
// A window that lost its exits in an outage used to replace every one of them
// through discovery: a find-providers round, then a contract and an evaluation
// per exit, on providers new to the window. The providers it lost usually still
// work once the connection is back, so the window remembers them for a bounded
// time, and the rounds that re-form it ask the platform for them first, by
// client id. This generalizes the user's Fixed IP re-dial
// (ip_remote_multi_client_sticky.go) to every other window, quality and speed.
// The Fixed IP window keeps its own single re-dial with no timeout, unchanged,
// and this memory never holds or asks for its exits. In-process only: the
// window identity store is what carries providers across a restart.
//
// What is remembered (classifyRecentExit): an exit the resize pass removes
// when the loss says nothing against the provider. It is classified before the
// removal and held only after it (rememberRecentExit), so a round never finds
// a fresh entry's exit still in the window.
//   - Transport loss (errTransportDownTimeout): the exit's own route set stayed
//     empty for the whole migration grace. A local fact, remembered always.
//   - A blackhole verdict collected in the dark, judged at the removal: the
//     tunnel-wide uplink gate is holding (an open uplink-stale epoch inside its
//     hold cap, or the scheduler-pause recovery hold), and nothing has arrived
//     from any provider since the verdict's evidence began
//     (blackholeEvidenceSince). The gate holds exactly while it vouches that
//     the silence is ours: two or more exits sending into a tunnel that
//     receives nothing, or the first seconds after the host was suspended.
//     It already holds every receive verdict then, so the verdict that
//     reaches this is the hard no-send-ack whose unacknowledged run began
//     after the last delivery, which is our outage timing out an innocent
//     provider. Both halves are required, which keeps it conservative: a gate
//     that holds while the evidence predates the silence would remember a
//     provider that was failing on a working uplink, and past the hold cap
//     the gate stops vouching by design ("a long outage or a window whose
//     every provider is dead"), so such a removal goes to discovery. Judging
//     at the removal rather than at the verdict errs toward discovery: the
//     ingress stamp only moves forward, so a later read of it refuses at
//     least as much, and a verdict the storm breaker deferred until the
//     uplink recovered is not remembered.
// Never remembered: a verdict on a fresh uplink, a send-stall conviction (it
// is admitted only while a sibling receives, which proves the uplink
// delivers), a stats-unhealthy removal (a soft classification whose clock the
// gate does not rebase, and which matures past the hold cap in an outage), a
// contract reliability verdict, user actions (drop_exit, shuffle and remove
// provider end a channel with a cancellation), expansion declines, capacity
// collapses, family swaps, drains, and exits closed with their window.
//
// Validity and bounds. An entry is used only within
// MultiClientSettings.RecentExitMemoryTimeout of its loss; non-positive
// disables the memory. It holds one entry per destination and at most what the
// window can hold at once (recentExitMemoryMaxCount, its hard max), keeping
// the newest.
//
// Use (enumerateRecentExits). A discovery round takes every valid entry, so
// each remembered exit is tried once. An entry whose exit is in the window
// again is dropped unasked. For each of the rest the platform is asked by
// client id, exactly as the sticky path asks (NextDestinationsForClientId),
// so the platform still applies its exclusions. A provider it returns leads
// the round's push order with the stats it was discovered with, and from there
// is an ordinary candidate: minted, evaluated with its initial ping and
// admitted by the usual pool, nothing skipped. One it does not return, or one
// that fails evaluation, is dropped, not retried. One the platform could not
// be asked about is put back for the next round, within its timeout.
// Discovery runs in the same round, excluding the remembered destinations, and
// fills whatever remains. The specs are not re-applied to a named lookup and
// need not be: a destination or spec change builds a new multi client and new
// windows, so every entry was discovered under the specs in force.
//
// Lookup timing. The lookups run in parallel, one per entry, so their number
// is bounded by the memory's cap, and each carries the generator deadline
// (WindowGeneratorTimeout), like discovery. While the platform has been
// answering, they run concurrently with discovery, so the round takes one
// platform round trip, as plain discovery does, and never more than one
// generator deadline. While the window's last generator call failed, the
// round asks discovery alone first and the lookups only once it answered: a
// failing platform is asked once per retry, as before, never once per
// remembered exit (retry amplification), at the cost of one more round trip in
// the single round that recovers.
//
// Clearing. A performance profile change, a shuffle and the window's close
// drop every entry; removing a provider drops that provider. A network change
// does not: re-forming after one is when the memory is useful.
//
// Visibility: one relEvent line per action ("recent_exit", action
// remember/try/admit/drop/expire), naming exits and providers by client id.
//
// Threading: the memory has its own small lock and never takes the window
// lock, so the resize pass, the enumerator and the profile and teardown paths
// may use it alike. A forget advances a generation, so a round whose lookups
// end after a forget neither puts back nor offers what it took.

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// Why an exit was remembered: the reason value of its log lines.
const (
	// its own route set stayed empty for the whole migration grace
	recentExitTransportLoss = "transport_loss"
	// a blackhole verdict collected while the uplink gate held the outage ours
	recentExitUplinkHold = "uplink_hold"
)

// The answer of a lookup that never completed, for a lookup goroutine that
// ended without one.
var errRecentExitLookupIncomplete = errors.New("recent exit lookup incomplete")

// One exit a window lost to an outage of ours.
type recentExit struct {
	destination MultiHopId
	// the stats it was discovered with: a named answer carries no location or
	// address family, and the egress is the provider either way
	stats DestinationStats
	// the memory timeout counts from here
	lostTime time.Time
	reason   string
}

// Reports whether an entry may still be used: strictly inside the timeout of
// its loss, so an entry exactly timeout old has expired. A non-positive
// timeout disables the memory, and nothing is valid.
func recentExitValid(exit recentExit, now time.Time, timeout time.Duration) bool {
	return 0 < timeout && now.Sub(exit.lostTime) < timeout
}

// The exits one window lost to an outage of ours, ordered by loss, oldest
// first, one per destination, at most maxCount of them. The zero value is
// ready, so bare test windows need no setup.
type recentExitMemory struct {
	stateLock sync.Mutex
	exits     []recentExit
	// the cap of the last remember, which a restore applies too
	maxCount int
	// advanced by every forget; see the file header
	generation uint64
}

// Holds a lost exit, replacing any entry for the same destination, and keeps
// only the newest maxCount. Entries past the timeout at the new loss leave
// first and come back as expired, so they never take a valid entry's place;
// the entries evicted by the cap come back as evicted. A non-positive maxCount
// holds nothing.
func (self *recentExitMemory) Remember(exit recentExit, maxCount int, timeout time.Duration) (evicted []recentExit, expired []recentExit) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.maxCount = maxCount
	self.exits = slices.DeleteFunc(self.exits, func(held recentExit) bool {
		if held.destination == exit.destination {
			return true
		}
		if !recentExitValid(held, exit.lostTime, timeout) {
			expired = append(expired, held)
			return true
		}
		return false
	})
	self.exits = append(self.exits, exit)
	self.sortWithLock()
	return self.evictWithLock(), expired
}

// Hands every valid entry to one round and clears the memory, so an exit that
// does not come back costs one platform round trip, not a retry loop. Entries
// past the timeout are returned as expired, never as taken. The generation
// identifies the take for Restore.
func (self *recentExitMemory) Take(now time.Time, timeout time.Duration) (taken []recentExit, expired []recentExit, generation uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, exit := range self.exits {
		if recentExitValid(exit, now, timeout) {
			taken = append(taken, exit)
		} else {
			expired = append(expired, exit)
		}
	}
	self.exits = nil
	return taken, expired, self.generation
}

// Puts back entries a round took but could not ask the platform about, with
// their loss times, so the timeout still bounds them. Refused after a forget
// since the take: the change that forgot them stands. An entry remembered
// again since the take is kept as remembered. Returns the entries evicted by
// the cap.
func (self *recentExitMemory) Restore(exits []recentExit, generation uint64) (evicted []recentExit) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(exits) == 0 || generation != self.generation {
		return nil
	}
	for _, exit := range exits {
		held := slices.ContainsFunc(self.exits, func(held recentExit) bool {
			return held.destination == exit.destination
		})
		if !held {
			self.exits = append(self.exits, exit)
		}
	}
	self.sortWithLock()
	return self.evictWithLock()
}

// Reports whether no forget happened since the take of this generation.
func (self *recentExitMemory) Current(generation uint64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return generation == self.generation
}

// Drops every entry: the user changed the profile or asked for new exits, or
// the window closed. Returns how many were held.
func (self *recentExitMemory) Forget() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := len(self.exits)
	self.exits = nil
	self.generation += 1
	return count
}

// Drops the entries for one provider (a destination tail). Reports whether any
// were held. A round in flight drops its taken entries too: the generation
// cannot say which provider was forgotten.
func (self *recentExitMemory) ForgetProvider(clientId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := len(self.exits)
	self.exits = slices.DeleteFunc(self.exits, func(held recentExit) bool {
		return held.destination.Tail() == clientId
	})
	self.generation += 1
	return len(self.exits) < count
}

func (self *recentExitMemory) sortWithLock() {
	slices.SortStableFunc(self.exits, func(a recentExit, b recentExit) int {
		return a.lostTime.Compare(b.lostTime)
	})
}

// Drops the oldest entries beyond the cap. Must be called with stateLock.
func (self *recentExitMemory) evictWithLock() (evicted []recentExit) {
	if n := len(self.exits) - max(0, self.maxCount); 0 < n {
		evicted = slices.Clone(self.exits[:n])
		self.exits = slices.Delete(self.exits, 0, n)
	}
	return evicted
}

// The most entries a window sized by windowSize remembers: everything it holds
// at once, which is its hard max, else the largest of its sizes. A window never
// loses more exits than it holds, so the cap drops only the oldest of losses
// that span more than one fill.
func recentExitMemoryMaxCount(windowSize WindowSizeSettings) int {
	if 0 < windowSize.WindowSizeHardMax {
		return windowSize.WindowSizeHardMax
	}
	return max(windowSize.WindowSizeMin, windowSize.WindowSizeMax, windowSize.FixedWindowSize)
}

// The earliest clock a blackhole verdict's evidence can run on: the
// outstanding unacknowledged sends, the first unacknowledged send in the
// stats window, and the current unanswered connect. Zero when none runs.
func blackholeEvidenceSince(stats *clientWindowStats) time.Time {
	evidenceSince := time.Time{}
	for _, since := range []time.Time{
		unacknowledgedSendSince(stats),
		stats.firstSendNackTime,
		unansweredConnectSince(stats),
	} {
		if !since.IsZero() && (evidenceSince.IsZero() || since.Before(evidenceSince)) {
			evidenceSince = since
		}
	}
	return evidenceSince
}

// Reports whether a verdict's evidence began no earlier than the tunnel's last
// delivery from any provider, so all of it was collected in the dark. A tunnel
// that never delivered has no baseline and vouches for nothing. The stamp may
// trail the true last delivery by its coarseness, a fraction of a second
// against evidence that runs for tens of seconds.
func evidenceSinceLastIngress(evidenceSince time.Time, lastIngress time.Time) bool {
	return !evidenceSince.IsZero() && !lastIngress.IsZero() && !evidenceSince.Before(lastIngress)
}

// The window's memory timeout. Non-positive disables the memory.
func (self *multiClientWindow) recentExitMemoryTimeout() time.Duration {
	if self.settings == nil {
		return 0
	}
	return self.settings.RecentExitMemoryTimeout
}

// The generator to ask for remembered providers by client id, when the window
// has one. A fixed destination set already names its providers, and a
// generator without the capability is never asked.
func (self *multiClientWindow) recentExitGenerator() (MultiClientGeneratorWithClientId, bool) {
	if self.generator == nil {
		return nil, false
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return nil, false
	}
	clientIdGenerator, capable := self.generator.(MultiClientGeneratorWithClientId)
	return clientIdGenerator, capable
}

// Classifies one removal for the memory: transport loss always, a blackhole
// verdict only when it was collected in the dark, anything else never. See the
// file header for why.
func (self *multiClientWindow) recentExitLossReason(err error, stats *clientWindowStats, now time.Time) (reason string, ok bool) {
	if errors.Is(err, errTransportDownTimeout) {
		return recentExitTransportLoss, true
	}
	if blackholeVerdictErr(err) && stats != nil && self.recentExitVerdictInTheDark(blackholeEvidenceSince(stats), now) {
		return recentExitUplinkHold, true
	}
	return "", false
}

// Reports whether a verdict whose evidence began at evidenceSince was
// collected in the dark: nothing has arrived since, and the uplink gate holds
// now. The stamp is read first: the gate may sweep every channel. Must be
// called with no lock held, as uplinkGate takes the parent stateLock inside.
func (self *multiClientWindow) recentExitVerdictInTheDark(evidenceSince time.Time, now time.Time) bool {
	if self.uplinkGateFunc == nil || self.uplinkLastIngressFunc == nil {
		return false
	}
	if !evidenceSinceLastIngress(evidenceSince, self.uplinkLastIngressFunc()) {
		return false
	}
	holding, _ := self.uplinkGateFunc(now)
	return holding
}

// Classifies an exit the resize pass is about to remove with err: ok when the
// loss was an outage of ours (recentExitLossReason) in a window that uses the
// memory, with the entry and the cap to hold it under. The resize pass calls it
// before the removal, while the exit's flows are still bound and the uplink
// gate counts its outstanding sends, and holds the entry only after the
// removal (rememberRecentExit). Called with no lock held.
func (self *multiClientWindow) classifyRecentExit(
	client *multiClientChannel,
	err error,
	stats *clientWindowStats,
	windowSize WindowSizeSettings,
	stickyExit bool,
) (exit recentExit, maxCount int, ok bool) {
	maxCount = recentExitMemoryMaxCount(windowSize)
	if stickyExit || client.args == nil || self.recentExitMemoryTimeout() <= 0 || maxCount <= 0 {
		// the user's Fixed IP keeps its own re-dial (stickyRedial)
		return recentExit{}, 0, false
	}
	if _, capable := self.recentExitGenerator(); !capable {
		return recentExit{}, 0, false
	}
	now := time.Now()
	reason, ok := self.recentExitLossReason(err, stats, now)
	if !ok {
		return recentExit{}, 0, false
	}
	return recentExit{
		destination: client.args.Destination,
		stats:       client.args.DestinationStats,
		lostTime:    now,
		reason:      reason,
	}, maxCount, true
}

// Holds an exit classified by classifyRecentExit, once the window no longer
// holds the exit: a round takes entries at any moment, and one that found the
// exit still in the window would drop it unasked.
func (self *multiClientWindow) rememberRecentExit(client *multiClientChannel, exit recentExit, maxCount int) {
	evicted, expired := self.recentExits.Remember(exit, maxCount, self.recentExitMemoryTimeout())
	self.logRecentExitsExpired(expired, exit.lostTime)
	self.logRecentExitsDropped(evicted, "cap")
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"recent_exit",
		"exit", client.ClientId(),
		"provider", exit.destination.Tail(),
		"action", "remember",
		"reason", exit.reason,
	))
}

// Runs one discovery round with the window's remembered exits asked for by
// client id, and returns the providers the platform still offers ahead of
// discovery's (see the file header for the order of calls). A window that
// does not use the memory, or holds nothing valid, discovers as before.
func (self *multiClientWindow) enumerateRecentExits(
	excludeDestinations []MultiHopId,
	rankMode string,
	discover func(excludeDestinations []MultiHopId) ([]enumeratedDestination, error),
) ([]enumeratedDestination, error) {
	timeout := self.recentExitMemoryTimeout()
	clientIdGenerator, capable := self.recentExitGenerator()
	if timeout <= 0 || !capable || self.stickyExit() {
		return discover(excludeDestinations)
	}
	exits, generation := self.takeRecentExits(time.Now(), timeout)
	if len(exits) == 0 {
		return discover(excludeDestinations)
	}

	// discovery fills the rest of the round with other providers
	discoverExcludeDestinations := slices.Clone(excludeDestinations)
	for _, exit := range exits {
		discoverExcludeDestinations = append(discoverExcludeDestinations, exit.destination)
	}

	if !self.platformAnswering() {
		// the last generator call failed: one call per retry, as before, and the
		// lookups only once discovery answered
		discovered, err := discover(discoverExcludeDestinations)
		if err != nil {
			self.restoreRecentExits(exits, generation)
			return nil, err
		}
		recovered := self.resolveRecentExits(
			self.lookupRecentExits(clientIdGenerator, exits, excludeDestinations, rankMode)(),
			generation,
		)
		return appendEnumeratedDestinations(recovered, discovered), nil
	}

	// the lookups and discovery in flight together: one round trip
	wait := self.lookupRecentExits(clientIdGenerator, exits, excludeDestinations, rankMode)
	discovered, err := discover(discoverExcludeDestinations)
	recovered := self.resolveRecentExits(wait(), generation)
	if err != nil {
		if len(recovered) == 0 {
			return nil, err
		}
		// the platform answered for these providers: dial them now rather
		// than fail the round, as the generator prefers its fixed
		// destinations over failing a discovery call
		loggerOrDefault(self.log).Infof("[multi]window enumerate error with %d recent exits = %s\n", len(recovered), err)
		return recovered, nil
	}
	return appendEnumeratedDestinations(recovered, discovered), nil
}

// Takes the entries one round may ask for: valid, and with their exit not in
// the window now, warned or not. The rest are dropped here, one line each.
func (self *multiClientWindow) takeRecentExits(now time.Time, timeout time.Duration) (exits []recentExit, generation uint64) {
	taken, expired, generation := self.recentExits.Take(now, timeout)
	self.logRecentExitsExpired(expired, now)
	if len(taken) == 0 {
		return nil, generation
	}
	log := loggerOrDefault(self.log)
	windowDestinations := map[MultiHopId]bool{}
	for _, client := range self.unorderedClients() {
		if client.args != nil {
			windowDestinations[client.Destination()] = true
		}
	}
	for _, exit := range taken {
		if windowDestinations[exit.destination] {
			// back already: never a second exit to the same destination
			log.Infof("%s\n", relEvent(
				"recent_exit",
				"provider", exit.destination.Tail(),
				"action", "drop",
				"reason", "in_window",
			))
			continue
		}
		exits = append(exits, exit)
	}
	return exits, generation
}

// Reports whether the window's last generator call succeeded: neither of its
// failure streaks (windowRetryDelay) is running.
func (self *multiClientWindow) platformAnswering() bool {
	return self.errorStreak.Load() == 0 && self.rateLimitStreak.Load() == 0
}

// One lookup's answer.
type recentExitAnswer struct {
	exit recentExit
	// the platform still offers the provider
	found bool
	// the platform could not be asked
	err error
}

// Starts one by-id lookup per entry, all at once, each under the generator
// deadline. The returned wait joins them and returns their answers in entry
// order. A lookup past its deadline is abandoned like any generator call: a
// listing carries no platform state, so its late answer is discarded.
func (self *multiClientWindow) lookupRecentExits(
	clientIdGenerator MultiClientGeneratorWithClientId,
	exits []recentExit,
	excludeDestinations []MultiHopId,
	rankMode string,
) (wait func() []recentExitAnswer) {
	answers := make([]recentExitAnswer, len(exits))
	lookups := sync.WaitGroup{}
	for i, exit := range exits {
		// a goroutine that ends without an answer reads as unanswered
		answers[i] = recentExitAnswer{exit: exit, err: errRecentExitLookupIncomplete}
		lookups.Add(1)
		go HandleError(func() {
			defer lookups.Done()
			destinations, err := windowGeneratorCall(
				self.ctx,
				self.settings.WindowGeneratorTimeout,
				func() (map[MultiHopId]DestinationStats, error) {
					return clientIdGenerator.NextDestinationsForClientId(
						exit.destination.Tail(),
						excludeDestinations,
						rankMode,
					)
				},
				nil,
			)
			found := false
			for candidate := range destinations {
				if candidate.Tail() == exit.destination.Tail() {
					found = true
					break
				}
			}
			answers[i].found = err == nil && found
			answers[i].err = err
		})
	}
	return func() []recentExitAnswer {
		lookups.Wait()
		return answers
	}
}

// Turns the lookups' answers into the round's leading candidates: a provider
// the platform still offers is tried as the destination the window held, with
// the stats it was discovered with; one it no longer offers is dropped; one it
// could not be asked about is put back for the next round. Nothing is offered
// after a forget since the take.
func (self *multiClientWindow) resolveRecentExits(answers []recentExitAnswer, generation uint64) []enumeratedDestination {
	if !self.recentExits.Current(generation) {
		return nil
	}
	log := loggerOrDefault(self.log)
	now := time.Now()
	recovered := []enumeratedDestination{}
	unanswered := []recentExit{}
	for _, answer := range answers {
		exit := answer.exit
		switch {
		case answer.err != nil:
			unanswered = append(unanswered, exit)
		case answer.found:
			log.Infof("%s\n", relEvent(
				"recent_exit",
				"provider", exit.destination.Tail(),
				"action", "try",
				"reason", exit.reason,
				"age", now.Sub(exit.lostTime),
			))
			recovered = append(recovered, enumeratedDestination{
				destination: exit.destination,
				stats:       exit.stats,
				recentExit:  true,
			})
		default:
			log.Infof("%s\n", relEvent(
				"recent_exit",
				"provider", exit.destination.Tail(),
				"action", "drop",
				"reason", "unavailable",
			))
		}
	}
	self.restoreRecentExits(unanswered, generation)
	return recovered
}

// Puts unasked entries back for the next round. See recentExitMemory.Restore.
func (self *multiClientWindow) restoreRecentExits(exits []recentExit, generation uint64) {
	self.logRecentExitsDropped(self.recentExits.Restore(exits, generation), "cap")
}

// The leading destinations, then the rest, each destination once.
func appendEnumeratedDestinations(leading []enumeratedDestination, rest []enumeratedDestination) []enumeratedDestination {
	ordered := make([]enumeratedDestination, 0, len(leading)+len(rest))
	seen := map[MultiHopId]bool{}
	for _, destinations := range [][]enumeratedDestination{leading, rest} {
		for _, destination := range destinations {
			if seen[destination.destination] {
				continue
			}
			seen[destination.destination] = true
			ordered = append(ordered, destination)
		}
	}
	return ordered
}

// --- outcomes ---

// The drop reason of a remembered exit's candidate that ended in evaluation:
// "evaluation" when the provider failed it, "not_admitted" when the pass or
// the window ended it.
func recentExitEvaluationReason(providerFailure bool) string {
	if providerFailure {
		return "evaluation"
	}
	return "not_admitted"
}

// Logs that a remembered exit's candidate was admitted to the window. Silent
// for any other candidate.
func (self *multiClientWindow) noteRecentExitAdmitted(args *multiClientChannelArgs) {
	if args == nil || !args.recentExit {
		return
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"recent_exit",
		"exit", args.ClientId,
		"provider", args.Destination.Tail(),
		"action", "admit",
	))
}

// Logs that a remembered exit's candidate ended without admission. It is not
// retried: the round that offered it took it from the memory. Silent for any
// other candidate.
func (self *multiClientWindow) noteRecentExitDropped(args *multiClientChannelArgs, reason string) {
	if args == nil || !args.recentExit {
		return
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"recent_exit",
		"exit", args.ClientId,
		"provider", args.Destination.Tail(),
		"action", "drop",
		"reason", reason,
	))
}

// One line per entry that expired, with its age at now.
func (self *multiClientWindow) logRecentExitsExpired(exits []recentExit, now time.Time) {
	for _, exit := range exits {
		loggerOrDefault(self.log).Infof("%s\n", relEvent(
			"recent_exit",
			"provider", exit.destination.Tail(),
			"action", "expire",
			"age", now.Sub(exit.lostTime),
		))
	}
}

// One line per entry dropped from the memory for reason.
func (self *multiClientWindow) logRecentExitsDropped(exits []recentExit, reason string) {
	for _, exit := range exits {
		loggerOrDefault(self.log).Infof("%s\n", relEvent(
			"recent_exit",
			"provider", exit.destination.Tail(),
			"action", "drop",
			"reason", reason,
		))
	}
}

// One line for a forget that dropped count entries, none for an empty memory.
func (self *multiClientWindow) logRecentExitsForgotten(count int, reason string) {
	if count <= 0 {
		return
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"recent_exit",
		"action", "drop",
		"reason", reason,
		"count", count,
	))
}
