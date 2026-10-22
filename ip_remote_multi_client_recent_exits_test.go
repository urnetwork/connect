package connect

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The recent-exit memory (ip_remote_multi_client_recent_exits.go): an exit a
// window that is not the user's Fixed IP lost to an outage of ours is asked
// for again by client id ahead of discovery, once, within the memory timeout.

// One generator call, in the order the window made them.
type recentExitTestCall struct {
	// discover, named, mint or evaluate
	kind        string
	destination MultiHopId
}

// Answers discovery with a fixed set minus the request's exclusions, a named
// request with the named provider while it is online, mints a fresh identity
// per destination, and fails every evaluation: the client never comes up.
type recentExitTestGenerator struct {
	testingEmptyMultiClientGenerator

	stateLock              sync.Mutex
	discoveredDestinations map[MultiHopId]DestinationStats
	discoveryErr           error
	onlineClientIds        map[Id]bool
	namedErr               error
	calls                  []recentExitTestCall
	discoveryExcludes      [][]MultiHopId
	mintedDestinations     map[Id]MultiHopId
}

func (self *recentExitTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, recentExitTestCall{kind: "discover"})
	self.discoveryExcludes = append(self.discoveryExcludes, slices.Clone(excludeDestinations))
	if self.discoveryErr != nil {
		return nil, self.discoveryErr
	}
	destinations := map[MultiHopId]DestinationStats{}
	for destination, stats := range self.discoveredDestinations {
		if !slices.Contains(excludeDestinations, destination) {
			destinations[destination] = stats
		}
	}
	return destinations, nil
}

func (self *recentExitTestGenerator) NextDestinationsForClientId(clientId Id, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, recentExitTestCall{kind: "named", destination: RequireMultiHopId(clientId)})
	if self.namedErr != nil {
		return nil, self.namedErr
	}
	destinations := map[MultiHopId]DestinationStats{}
	if self.onlineClientIds[clientId] {
		// a named answer carries no discovery stats
		destinations[RequireMultiHopId(clientId)] = DestinationStats{}
	}
	return destinations, nil
}

func (self *recentExitTestGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, recentExitTestCall{kind: "mint", destination: destination})
	args := &MultiClientGeneratorClientArgs{ClientId: NewId()}
	if self.mintedDestinations == nil {
		self.mintedDestinations = map[Id]MultiHopId{}
	}
	self.mintedDestinations[args.ClientId] = destination
	return args, nil
}

func (self *recentExitTestGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, recentExitTestCall{kind: "evaluate", destination: self.mintedDestinations[args.ClientId]})
	return nil, errors.New("the provider does not answer")
}

func (self *recentExitTestGenerator) setDiscoveryErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.discoveryErr = err
}

func (self *recentExitTestGenerator) setNamedErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.namedErr = err
}

func (self *recentExitTestGenerator) recordedCalls() []recentExitTestCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

func (self *recentExitTestGenerator) recordedDiscoveryExcludes() [][]MultiHopId {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.discoveryExcludes)
}

// How many recorded calls are of kind, for destination when it is not zero.
func recentExitTestCount(calls []recentExitTestCall, kind string, destination MultiHopId) int {
	count := 0
	for _, call := range calls {
		if call.kind == kind && (destination.Len() == 0 || call.destination == destination) {
			count += 1
		}
	}
	return count
}

// The index of the first call of kind for destination at or after start, or -1.
func recentExitTestIndex(calls []recentExitTestCall, start int, kind string, destination MultiHopId) int {
	for i := max(0, start); i < len(calls); i += 1 {
		if calls[i].kind == kind && (destination.Len() == 0 || calls[i].destination == destination) {
			return i
		}
	}
	return -1
}

// A generator the window must not use the memory with: a fixed destination
// set already names its providers.
type recentExitFixedTestGenerator struct {
	recentExitTestGenerator
}

func (self *recentExitFixedTestGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

// A bare window holding a profile, for one discovery round at a time.
func recentExitBareWindow(generator MultiClientGenerator, performanceProfile *PerformanceProfile) *multiClientWindow {
	window := stickyRedialTestWindow(generator, performanceProfile)
	window.clients = map[Id]*multiClientChannel{}
	return window
}

// The window size a resize pass of the default speed window computes.
func recentExitTestWindowSize() WindowSizeSettings {
	return DefaultMultiClientSettings().WindowSizes[WindowTypeSpeed]
}

// An exit the window held, discovered with stats, as the resize pass sees it
// when it is removed.
func recentExitTestChannel(stats DestinationStats) *multiClientChannel {
	client := stickyTestPastLifetimeChannel()
	client.args.DestinationStats = stats
	return client
}

// The entries a memory holds, without taking them.
func recentExitTestHeld(memory *recentExitMemory) []recentExit {
	memory.stateLock.Lock()
	defer memory.stateLock.Unlock()
	return slices.Clone(memory.exits)
}

// Classifies one removal and holds it as the resize pass does, for a bare
// window that holds no exit. Reports whether it was remembered.
func recentExitTestRemember(
	window *multiClientWindow,
	client *multiClientChannel,
	err error,
	stats *clientWindowStats,
	windowSize WindowSizeSettings,
	stickyExit bool,
) bool {
	exit, maxCount, ok := window.classifyRecentExit(client, err, stats, windowSize, stickyExit)
	if ok {
		window.rememberRecentExit(client, exit, maxCount)
	}
	return ok
}

// A live window -- resize, the enumerator and the watchdogs -- of the speed
// type with the fast passes of stickyTestWindow and the given uplink seams.
func recentExitTestWindow(
	t *testing.T,
	generator MultiClientGenerator,
	performanceProfile *PerformanceProfile,
	uplinkGateFunc func(now time.Time) (stale bool, freshSince time.Time),
	uplinkLastIngressFunc func() time.Time,
	configure ...func(settings *MultiClientSettings),
) *multiClientWindow {
	t.Helper()
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)

	settings := DefaultMultiClientSettings()
	settings.WindowResizeTimeout = 20 * time.Millisecond
	settings.WindowExpandTimeout = 20 * time.Millisecond
	// a pass that admits nothing is paced on the same compressed clock
	settings.DiscoveryExpandRetryMin = 20 * time.Millisecond
	settings.DiscoveryExpandRetryMax = 20 * time.Millisecond
	for _, configureSettings := range configure {
		configureSettings(settings)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newMultiClientWindow(
		ctx,
		cancel,
		generator,
		nil,
		nil,
		false,
		nil,
		DisableSecurityPolicy(),
		func(*multiClientChannel) {},
		WindowTypeSpeed,
		performanceProfile,
		settings,
		func() *ReliabilitySettings { return ReliabilitySettingsFrom(settings) },
		uplinkGateFunc,
		uplinkLastIngressFunc,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
}

// Waits until the recorded calls satisfy done, and returns them.
func waitRecentExitTestCalls(t *testing.T, generator *recentExitTestGenerator, what string, done func(calls []recentExitTestCall) bool) []recentExitTestCall {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		calls := generator.recordedCalls()
		if done(calls) {
			return calls
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("the window did not %s: calls %v", what, calls)
		}
		time.Sleep(time.Millisecond)
	}
}

// Waits until the window no longer holds client.
func waitRecentExitTestRemoved(t *testing.T, window *multiClientWindow, client *multiClientChannel) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for slices.Contains(window.unorderedClients(), client) {
		if deadline.Before(time.Now()) {
			t.Fatal("the lost exit was not removed")
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the memory ---

// Each remembered exit is handed to one round, in loss order, and a second
// round finds nothing.
func TestRecentExitMemoryTakesEachExitOnce(t *testing.T) {
	memory := &recentExitMemory{}
	timeout := 5 * time.Minute
	start := time.Now()
	first := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start, reason: recentExitTransportLoss}
	second := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start.Add(time.Second), reason: recentExitTransportLoss}
	memory.Remember(second, 4, timeout)
	memory.Remember(first, 4, timeout)

	taken, expired, _ := memory.Take(start.Add(2*time.Second), timeout)
	AssertEqual(t, len(expired), 0)
	AssertEqual(t, len(taken), 2)
	AssertEqual(t, taken[0].destination, first.destination)
	AssertEqual(t, taken[1].destination, second.destination)

	taken, expired, _ = memory.Take(start.Add(3*time.Second), timeout)
	AssertEqual(t, len(taken), 0)
	AssertEqual(t, len(expired), 0)
}

// An entry is valid strictly inside the timeout of its loss and never used
// after it; a disabled memory holds nothing valid, and a new loss prunes
// what has expired rather than counting it against the cap.
func TestRecentExitMemoryExpiresAtTheTimeout(t *testing.T) {
	timeout := 5 * time.Minute
	start := time.Now()
	exit := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start, reason: recentExitTransportLoss}

	memory := &recentExitMemory{}
	memory.Remember(exit, 4, timeout)
	taken, expired, _ := memory.Take(start.Add(timeout-time.Nanosecond), timeout)
	AssertEqual(t, len(taken), 1)
	AssertEqual(t, len(expired), 0)

	memory.Remember(exit, 4, timeout)
	taken, expired, _ = memory.Take(start.Add(timeout), timeout)
	AssertEqual(t, len(taken), 0)
	AssertEqual(t, len(expired), 1)
	AssertEqual(t, expired[0].destination, exit.destination)

	memory.Remember(exit, 4, timeout)
	taken, expired, _ = memory.Take(start, 0)
	AssertEqual(t, len(taken), 0)
	AssertEqual(t, len(expired), 1)

	memory.Remember(exit, 1, timeout)
	later := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start.Add(timeout), reason: recentExitTransportLoss}
	evicted, expired := memory.Remember(later, 1, timeout)
	AssertEqual(t, len(evicted), 0)
	AssertEqual(t, len(expired), 1)
	AssertEqual(t, expired[0].destination, exit.destination)
}

// The cap keeps the newest losses, and a destination lost again replaces its
// entry rather than taking a second slot.
func TestRecentExitMemoryKeepsTheNewest(t *testing.T) {
	memory := &recentExitMemory{}
	timeout := 5 * time.Minute
	start := time.Now()
	exits := []recentExit{}
	for i := range 3 {
		exits = append(exits, recentExit{
			destination: RequireMultiHopId(NewId()),
			lostTime:    start.Add(time.Duration(i) * time.Second),
			reason:      recentExitTransportLoss,
		})
	}
	memory.Remember(exits[0], 2, timeout)
	memory.Remember(exits[1], 2, timeout)
	evicted, _ := memory.Remember(exits[2], 2, timeout)
	AssertEqual(t, len(evicted), 1)
	AssertEqual(t, evicted[0].destination, exits[0].destination)

	again := exits[1]
	again.lostTime = start.Add(3 * time.Second)
	again.stats = stickyRedialTestStats()
	evicted, _ = memory.Remember(again, 2, timeout)
	AssertEqual(t, len(evicted), 0)

	taken, _, _ := memory.Take(start.Add(4*time.Second), timeout)
	AssertEqual(t, len(taken), 2)
	AssertEqual(t, taken[0].destination, exits[2].destination)
	AssertEqual(t, taken[1].destination, exits[1].destination)
	AssertEqual(t, taken[1].stats.Location.CountryCode, "de")
}

// A forget drops everything, and a round that took entries before it can no
// longer put them back; a round that could not ask puts its entries back with
// their loss times. Forgetting a provider drops its entries only.
func TestRecentExitMemoryForgetAndRestore(t *testing.T) {
	memory := &recentExitMemory{}
	timeout := 5 * time.Minute
	start := time.Now()
	first := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start, reason: recentExitTransportLoss}
	second := recentExit{destination: RequireMultiHopId(NewId()), lostTime: start.Add(time.Second), reason: recentExitTransportLoss}

	memory.Remember(first, 4, timeout)
	taken, _, generation := memory.Take(start, timeout)
	AssertEqual(t, len(taken), 1)
	AssertEqual(t, memory.Current(generation), true)
	// a newer loss arrives while the round is asking
	memory.Remember(second, 4, timeout)
	memory.Restore(taken, generation)
	held := recentExitTestHeld(memory)
	AssertEqual(t, len(held), 2)
	AssertEqual(t, held[0].destination, first.destination)
	AssertEqual(t, held[0].lostTime, first.lostTime)
	AssertEqual(t, held[1].destination, second.destination)

	taken, _, generation = memory.Take(start, timeout)
	AssertEqual(t, len(taken), 2)
	AssertEqual(t, memory.Forget(), 0)
	AssertEqual(t, memory.Current(generation), false)
	memory.Restore(taken, generation)
	AssertEqual(t, len(recentExitTestHeld(memory)), 0)

	memory.Remember(first, 4, timeout)
	memory.Remember(second, 4, timeout)
	AssertEqual(t, memory.ForgetProvider(first.destination.Tail()), true)
	AssertEqual(t, memory.ForgetProvider(first.destination.Tail()), false)
	held = recentExitTestHeld(memory)
	AssertEqual(t, len(held), 1)
	AssertEqual(t, held[0].destination, second.destination)
	AssertEqual(t, memory.Forget(), 1)
	AssertEqual(t, len(recentExitTestHeld(memory)), 0)
}

// The cap is everything the window can hold at once.
func TestRecentExitMemoryMaxCount(t *testing.T) {
	settings := DefaultMultiClientSettings()
	AssertEqual(t, recentExitMemoryMaxCount(settings.WindowSizes[WindowTypeQuality]), 6)
	AssertEqual(t, recentExitMemoryMaxCount(settings.WindowSizes[WindowTypeSpeed]), 4)
	// the apps' profiles leave the hard max at 0
	AssertEqual(t, recentExitMemoryMaxCount(unfixedTestProfile(WindowTypeSpeed).WindowSize), 4)
	AssertEqual(t, recentExitMemoryMaxCount(WindowSizeSettings{}), 0)
	AssertEqual(t, settings.RecentExitMemoryTimeout, 5*time.Minute)
}

// --- what is remembered ---

// Transport loss is remembered with the stats the exit was discovered with.
// Nothing argued against a provider is, nor anything in a window that does not
// use the memory: the user's Fixed IP, a disabled memory, a fixed destination
// set, a generator that cannot be asked by client id.
func TestRecentExitRemembersOnlyLossesOfOurs(t *testing.T) {
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	window.log = newRecordingLogger()
	windowSize := recentExitTestWindowSize()

	client := recentExitTestChannel(stickyRedialTestStats())
	AssertEqual(t, recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, windowSize, false), true)
	held := recentExitTestHeld(&window.recentExits)
	AssertEqual(t, len(held), 1)
	AssertEqual(t, held[0].destination, client.Destination())
	AssertEqual(t, held[0].reason, recentExitTransportLoss)
	AssertEqual(t, held[0].stats.IpFamily, IpFamilyDualstack)
	AssertEqual(t, held[0].stats.Location.CountryCode, "de")
	lines := window.log.(*recordingLogger).linesWith("event=recent_exit")
	AssertEqual(t, len(lines), 1)
	if !strings.Contains(lines[0], "action=remember") || !strings.Contains(lines[0], "reason=transport_loss") {
		t.Fatalf("the remember line does not name the action and its reason: %s", lines[0])
	}
	window.recentExits.Forget()

	// with no uplink seams there is no dark to collect a verdict in
	for _, err := range []error{
		errors.New("Blackhole no-send-ack (send 0/0B recv 0/0B syn 0/0 nackAge 31s synAge - dsts=2)"),
		errors.New("Blackhole no-receive-ack quarantine expired (send 4/400B recv 0/0B syn 1/0 nackAge 0s synAge 0s dsts=2)"),
		errors.New("send stalled: no ack progress for 3s"),
		errContractReliability,
		errors.New("Done."),
		nil,
	} {
		AssertEqual(t, recentExitTestRemember(window, recentExitTestChannel(DestinationStats{}), err, &clientWindowStats{}, windowSize, false), false)
	}
	AssertEqual(t, recentExitTestRemember(window, recentExitTestChannel(DestinationStats{}), errTransportDownTimeout, &clientWindowStats{}, windowSize, true), false)
	bare := stickyTestPastLifetimeChannel()
	bare.args = nil
	AssertEqual(t, recentExitTestRemember(window, bare, errTransportDownTimeout, &clientWindowStats{}, windowSize, false), false)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)

	disabled := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	disabled.settings.RecentExitMemoryTimeout = 0
	fixed := recentExitBareWindow(&recentExitFixedTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	incapable := recentExitBareWindow(&testingEmptyMultiClientGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	for _, other := range []*multiClientWindow{disabled, fixed, incapable} {
		AssertEqual(t, recentExitTestRemember(other, recentExitTestChannel(DestinationStats{}), errTransportDownTimeout, &clientWindowStats{}, windowSize, false), false)
		AssertEqual(t, len(recentExitTestHeld(&other.recentExits)), 0)
	}
}

// A blackhole verdict is remembered only when it was collected in the dark,
// judged with the real uplink gate: the gate holds now (an open stale epoch
// inside its cap, or the scheduler-pause hold) and nothing has arrived since
// the verdict's evidence began. A fresh uplink, evidence from before the
// silence, a gate past its cap, a single sending exit and a tunnel that never
// delivered all leave the verdict to discovery, and a send-stall conviction
// never qualifies.
func TestRecentExitRemembersVerdictsOnlyInTheDark(t *testing.T) {
	noSendAck := errors.New("Blackhole no-send-ack (send 0/0B recv 0/0B syn 0/0 nackAge 31s synAge - dsts=2)")
	pendingSince := func(since time.Time) *clientWindowStats {
		return &clientWindowStats{sendProgressKnown: true, sendNackCount: 1, pendingSendTime: since}
	}
	cases := []struct {
		name string
		// builds the parent's uplink state relative to now
		parent func(now time.Time) *RemoteUserNatMultiClient
		err    error
		stats  func(now time.Time) *clientWindowStats
		want   bool
	}{
		{
			name: "evidence after the last delivery, gate holding",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-40 * time.Second).UnixNano())
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-35 * time.Second)) },
			want:  true,
		},
		{
			name: "evidence from before the silence",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-40 * time.Second).UnixNano())
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-45 * time.Second)) },
		},
		{
			name: "fresh uplink",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-time.Second).UnixNano())
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-500 * time.Millisecond)) },
		},
		{
			name: "stale epoch past its hold cap",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-100 * time.Second).UnixNano())
				mc.uplinkStaleSince = now.Add(-uplinkStalenessMaxHold - time.Second)
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-90 * time.Second)) },
		},
		{
			name: "one sending exit: the gate cannot tell us from the provider",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(1)
				mc.uplinkLastIngressNanos.Store(now.Add(-40 * time.Second).UnixNano())
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-35 * time.Second)) },
		},
		{
			name: "scheduler-pause hold",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-time.Second).UnixNano())
				mc.notifySchedulerPause(30 * time.Second)
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-500 * time.Millisecond)) },
			want:  true,
		},
		{
			name: "a tunnel that never delivered",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.notifySchedulerPause(30 * time.Second)
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-500 * time.Millisecond)) },
		},
		{
			name: "send stall in the dark",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-40 * time.Second).UnixNano())
				return mc
			},
			err:   errors.New("send stalled: no ack progress for 3s"),
			stats: func(now time.Time) *clientWindowStats { return pendingSince(now.Add(-35 * time.Second)) },
		},
		{
			name: "no running evidence",
			parent: func(now time.Time) *RemoteUserNatMultiClient {
				mc := uplinkGateTestParent(2)
				mc.uplinkLastIngressNanos.Store(now.Add(-40 * time.Second).UnixNano())
				return mc
			},
			err:   noSendAck,
			stats: func(now time.Time) *clientWindowStats { return &clientWindowStats{sendProgressKnown: true} },
		},
	}
	for _, c := range cases {
		now := time.Now()
		mc := c.parent(now)
		window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
		window.uplinkGateFunc = mc.uplinkGate
		window.uplinkLastIngressFunc = mc.uplinkLastIngress
		remembered := recentExitTestRemember(window, recentExitTestChannel(DestinationStats{}), c.err, c.stats(now), recentExitTestWindowSize(), false)
		if remembered != c.want {
			t.Errorf("%s: remembered = %t, want %t", c.name, remembered, c.want)
			continue
		}
		if remembered {
			AssertEqual(t, recentExitTestHeld(&window.recentExits)[0].reason, recentExitUplinkHold)
		}
	}

	// the earliest running clock decides, and the stamp may only precede it
	start := time.Now()
	stats := &clientWindowStats{
		sendProgressKnown:          true,
		sendNackCount:              1,
		pendingSendTime:            start.Add(2 * time.Second),
		firstSendNackTime:          start.Add(time.Second),
		firstUnansweredSendSynTime: start.Add(3 * time.Second),
	}
	AssertEqual(t, blackholeEvidenceSince(stats), start.Add(time.Second))
	AssertEqual(t, blackholeEvidenceSince(&clientWindowStats{sendProgressKnown: true}).IsZero(), true)
	AssertEqual(t, evidenceSinceLastIngress(start, start), true)
	AssertEqual(t, evidenceSinceLastIngress(start, start.Add(time.Nanosecond)), false)
	AssertEqual(t, evidenceSinceLastIngress(start, time.Time{}), false)
	AssertEqual(t, evidenceSinceLastIngress(time.Time{}, start), false)
}

// --- the rounds ---

// A remembered exit is asked for by client id and leads the round as the
// destination the window held, with its discovery stats; discovery fills the
// rest with other providers in the same round. The next round discovers alone.
func TestRecentExitsLeadTheRoundAheadOfDiscovery(t *testing.T) {
	other := RequireMultiHopId(NewId())
	client := recentExitTestChannel(stickyRedialTestStats())
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}, lost: {}},
		onlineClientIds:        map[Id]bool{lost.Tail(): true},
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	AssertEqual(t, recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false), true)

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 2)
	AssertEqual(t, ordered[0].destination, lost)
	AssertEqual(t, ordered[0].recentExit, true)
	AssertEqual(t, ordered[0].stats.IpFamily, IpFamilyDualstack)
	AssertEqual(t, ordered[0].stats.Location.CountryCode, "de")
	AssertEqual(t, ordered[1].destination, other)
	AssertEqual(t, ordered[1].recentExit, false)
	calls := generator.recordedCalls()
	AssertEqual(t, recentExitTestCount(calls, "named", lost), 1)
	AssertEqual(t, recentExitTestCount(calls, "discover", MultiHopId{}), 1)
	excludes := generator.recordedDiscoveryExcludes()
	AssertEqual(t, slices.Contains(excludes[0], lost), true)
	if tries := log.linesWith("action=try"); len(tries) != 1 || !strings.Contains(tries[0], "reason=transport_loss") {
		t.Fatalf("one try line naming why the exit was remembered, got %v", tries)
	}

	// taken: the next round discovers alone, and may find the provider there
	ordered, err = window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 2)
	for _, destination := range ordered {
		AssertEqual(t, destination.recentExit, false)
	}
	calls = generator.recordedCalls()
	AssertEqual(t, recentExitTestCount(calls, "named", lost), 1)
	AssertEqual(t, recentExitTestCount(calls, "discover", MultiHopId{}), 2)
}

// An entry past the memory timeout is never asked for: the round goes
// straight to discovery and the entry is gone.
func TestRecentExitsExpiredGoStraightToDiscovery(t *testing.T) {
	other := RequireMultiHopId(NewId())
	lost := RequireMultiHopId(NewId())
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
		onlineClientIds:        map[Id]bool{lost.Tail(): true},
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	timeout := window.settings.RecentExitMemoryTimeout
	window.recentExits.Remember(recentExit{
		destination: lost,
		lostTime:    time.Now().Add(-timeout),
		reason:      recentExitTransportLoss,
	}, 4, timeout)

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	calls := generator.recordedCalls()
	AssertEqual(t, recentExitTestCount(calls, "named", MultiHopId{}), 0)
	AssertEqual(t, recentExitTestCount(calls, "discover", MultiHopId{}), 1)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
	AssertEqual(t, len(log.linesWith("action=expire")), 1)
}

// A provider the platform no longer offers costs one named request and is
// dropped, not retried; the same round discovers as usual.
func TestRecentExitsDroppedWhenThePlatformNoLongerOffersThem(t *testing.T) {
	other := RequireMultiHopId(NewId())
	client := recentExitTestChannel(DestinationStats{})
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)

	for range 2 {
		ordered, err := window.enumerateDestinations(nil, false)
		AssertEqual(t, err, nil)
		AssertEqual(t, len(ordered), 1)
		AssertEqual(t, ordered[0].destination, other)
	}
	AssertEqual(t, recentExitTestCount(generator.recordedCalls(), "named", lost), 1)
	if drops := log.linesWith("action=drop"); len(drops) != 1 || !strings.Contains(drops[0], "reason=unavailable") {
		t.Fatalf("one drop line for the unavailable provider, got %v", drops)
	}
}

// A platform that cannot be asked keeps the entry for the next round, with
// its loss time, whether discovery answered or failed too.
func TestRecentExitsKeptWhileThePlatformCannotBeAsked(t *testing.T) {
	other := RequireMultiHopId(NewId())
	client := recentExitTestChannel(DestinationStats{})
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
		onlineClientIds:        map[Id]bool{lost.Tail(): true},
		namedErr:               errors.New("platform unreachable"),
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)
	lostTime := recentExitTestHeld(&window.recentExits)[0].lostTime

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	held := recentExitTestHeld(&window.recentExits)
	AssertEqual(t, len(held), 1)
	AssertEqual(t, held[0].destination, lost)
	AssertEqual(t, held[0].lostTime, lostTime)

	generator.setDiscoveryErr(errors.New("platform unreachable"))
	if _, err := window.enumerateDestinations(nil, false); err == nil {
		t.Fatal("a round that reached no platform did not fail for the enumerator's retry")
	}
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 1)

	// once it answers, the provider leads the round
	generator.setNamedErr(nil)
	generator.setDiscoveryErr(nil)
	window.windowRetryReset()
	ordered, err = window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, ordered[0].destination, lost)
}

// Discovery failing does not hide a provider the platform did answer for: the
// round dials it alone rather than failing.
func TestRecentExitsDialedWhenOnlyDiscoveryFails(t *testing.T) {
	client := recentExitTestChannel(DestinationStats{})
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		onlineClientIds: map[Id]bool{lost.Tail(): true},
		discoveryErr:    errors.New("discovery timed out"),
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, lost)
	AssertEqual(t, ordered[0].recentExit, true)
}

// After a failed generator call the round asks discovery alone first: a
// failing platform is asked once per retry, never once per remembered exit,
// and the lookups follow only once discovery answered.
func TestRecentExitsAfterFailuresAskDiscoveryFirst(t *testing.T) {
	other := RequireMultiHopId(NewId())
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
		onlineClientIds:        map[Id]bool{},
		discoveryErr:           errors.New("platform unreachable"),
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	lostDestinations := []MultiHopId{}
	for range 3 {
		client := recentExitTestChannel(DestinationStats{})
		generator.onlineClientIds[client.Destination().Tail()] = true
		lostDestinations = append(lostDestinations, client.Destination())
		recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)
	}
	window.errorStreak.Store(1)

	for range 2 {
		if _, err := window.enumerateDestinations(nil, false); err == nil {
			t.Fatal("a round that reached no platform did not fail")
		}
	}
	calls := generator.recordedCalls()
	AssertEqual(t, recentExitTestCount(calls, "named", MultiHopId{}), 0)
	AssertEqual(t, recentExitTestCount(calls, "discover", MultiHopId{}), 2)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 3)

	generator.setDiscoveryErr(nil)
	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 4)
	for i, destination := range lostDestinations {
		AssertEqual(t, ordered[i].destination, destination)
	}
	AssertEqual(t, ordered[3].destination, other)
	calls = generator.recordedCalls()
	discovered := recentExitTestIndex(calls, 2, "discover", MultiHopId{})
	firstNamed := recentExitTestIndex(calls, 0, "named", MultiHopId{})
	if discovered < 0 || firstNamed < discovered {
		t.Fatalf("the recovering round asked by id before discovery answered: %v", calls)
	}
	AssertEqual(t, recentExitTestCount(calls, "named", MultiHopId{}), 3)
}

// An exit that is in the window again, warned or not, is never asked for.
func TestRecentExitsNeverAskForAnExitInTheWindow(t *testing.T) {
	other := RequireMultiHopId(NewId())
	client := recentExitTestChannel(DestinationStats{})
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
		onlineClientIds:        map[Id]bool{lost.Tail(): true},
	}
	window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)

	back := stickyTestPastLifetimeChannel()
	back.args.Destination = lost
	back.setWarning(true, warnDraining)
	window.clients[back.ClientId()] = back

	ordered, err := window.enumerateDestinations([]MultiHopId{}, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	AssertEqual(t, recentExitTestCount(generator.recordedCalls(), "named", MultiHopId{}), 0)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
}

// A lost exit is classified before its removal and held only after it. Held
// first, a round that ran in between (the removal closes the channel before
// the exit leaves the window) found the exit still in the window and dropped
// the entry unasked, so the window discovered instead of asking for it.
func TestRecentExitHeldOnlyAfterItsExitLeftTheWindow(t *testing.T) {
	other := RequireMultiHopId(NewId())
	newWindow := func() (*multiClientWindow, *recentExitTestGenerator, *multiClientChannel) {
		client := recentExitTestChannel(DestinationStats{})
		generator := &recentExitTestGenerator{
			discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
			onlineClientIds:        map[Id]bool{client.Destination().Tail(): true},
		}
		window := recentExitBareWindow(generator, unfixedTestProfile(WindowTypeSpeed))
		window.clients[client.ClientId()] = client
		return window, generator, client
	}

	// held while the exit is still in the window: a round drops it
	window, generator, client := newWindow()
	exit, maxCount, ok := window.classifyRecentExit(client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)
	AssertEqual(t, ok, true)
	window.rememberRecentExit(client, exit, maxCount)
	_, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, recentExitTestCount(generator.recordedCalls(), "named", MultiHopId{}), 0)

	// classified first, held after the exit left: the round asks for it
	window, generator, client = newWindow()
	exit, maxCount, ok = window.classifyRecentExit(client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)
	AssertEqual(t, ok, true)
	// a round in between finds nothing held to drop
	_, err = window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	delete(window.clients, client.ClientId())
	window.rememberRecentExit(client, exit, maxCount)
	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, ordered[0].destination, client.Destination())
	AssertEqual(t, recentExitTestCount(generator.recordedCalls(), "named", client.Destination()), 1)

	// and the resize pass keeps that order
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) resize()")
	if !ok {
		t.Fatal("could not find resize")
	}
	classify := strings.Index(body, "self.classifyRecentExit(client, err, stats, windowSize, stickyExit)")
	if classify < 0 {
		t.Fatal("resize does not classify the exits it removes for the recent-exit memory")
	}
	removal := strings.Index(body[classify:], "removeClient(client)")
	remember := strings.Index(body[classify:], "self.rememberRecentExit(client, lostExit, lostExitMaxCount)")
	if removal < 0 || remember < 0 || remember < removal {
		t.Fatal("resize holds a lost exit before its removal: a round in between drops it as still in the window")
	}
}

// A destination change, a profile change and a shuffle drop every entry, and
// removing a provider drops its own; a round in flight across a forget offers
// nothing it took. A destination change builds a new multi client, closing
// every window, and its new windows start empty.
func TestRecentExitsClearedOnDestinationAndProfileChange(t *testing.T) {
	newWindow := func() (*multiClientWindow, []*multiClientChannel) {
		window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
		clients := []*multiClientChannel{}
		for range 2 {
			client := recentExitTestChannel(DestinationStats{})
			recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)
			clients = append(clients, client)
		}
		AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 2)
		return window, clients
	}

	window, _ := newWindow()
	window.Close()
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
	fresh := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	AssertEqual(t, len(recentExitTestHeld(&fresh.recentExits)), 0)

	window, _ = newWindow()
	window.SetPerformanceProfile(unfixedTestProfile(WindowTypeSpeed))
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 2)
	window.SetPerformanceProfile(unfixedTestProfile(WindowTypeQuality))
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)

	window, _ = newWindow()
	window.shuffle()
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)

	window, clients := newWindow()
	window.removeProvider(clients[0].Destination().Tail())
	held := recentExitTestHeld(&window.recentExits)
	AssertEqual(t, len(held), 1)
	AssertEqual(t, held[0].destination, clients[1].Destination())

	window, _ = newWindow()
	exits, generation := window.takeRecentExits(time.Now(), window.settings.RecentExitMemoryTimeout)
	AssertEqual(t, len(exits), 2)
	window.shuffle()
	answers := []recentExitAnswer{{exit: exits[0], found: true}, {exit: exits[1], err: errors.New("unreachable")}}
	AssertEqual(t, len(window.resolveRecentExits(answers, generation)), 0)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
}

// A network change keeps the memory: re-forming after one is when it is
// useful.
func TestRecentExitsKeptAcrossANetworkChange(t *testing.T) {
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	window.ctx = parent.ctx
	parent.windows[WindowTypeSpeed] = window
	recentExitTestRemember(window, recentExitTestChannel(DestinationStats{}), errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), false)

	parent.NotifyNetworkChanged()
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 1)
}

// The user's Fixed IP keeps its own re-dial: its transport loss goes to the
// sticky re-dial only, and its rounds never read this memory.
func TestRecentExitsNotUsedByTheFixedIpWindow(t *testing.T) {
	other := RequireMultiHopId(NewId())
	planted := RequireMultiHopId(NewId())
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
		onlineClientIds:        map[Id]bool{planted.Tail(): true},
	}
	window := recentExitBareWindow(generator, fixedIpTestProfile(WindowTypeSpeed))
	client := recentExitTestChannel(DestinationStats{})
	AssertEqual(t, window.rememberStickyRedial(client, errTransportDownTimeout, true), true)
	AssertEqual(t, recentExitTestRemember(window, client, errTransportDownTimeout, &clientWindowStats{}, recentExitTestWindowSize(), true), false)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
	window.stickyRedial.Forget()

	window.recentExits.Remember(recentExit{
		destination: planted,
		lostTime:    time.Now(),
		reason:      recentExitTransportLoss,
	}, 4, window.settings.RecentExitMemoryTimeout)
	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	AssertEqual(t, recentExitTestCount(generator.recordedCalls(), "named", MultiHopId{}), 0)
}

// --- end to end through the live window ---

// An exit lost to transport loss is removed, and the round that re-forms the
// window asks for that provider by client id and offers it first: its
// identity is minted ahead of discovery's provider, which follows in the same
// round. Before the memory, the window went straight to discovery.
func TestRecentExitTransportLossAsksByIdBeforeDiscovery(t *testing.T) {
	other := RequireMultiHopId(NewId())
	client := stickyTestLostChannel(errTransportDownTimeout)
	client.args.DestinationStats = stickyRedialTestStats()
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{other: {}},
		onlineClientIds:        map[Id]bool{lost.Tail(): true},
	}
	window := recentExitTestWindow(t, generator, unfixedTestProfile(WindowTypeSpeed), nil, nil)
	installStickyTestChannel(window, client)

	calls := waitRecentExitTestCalls(t, generator, "offer its lost provider and then discovery's", func(calls []recentExitTestCall) bool {
		named := recentExitTestIndex(calls, 0, "named", lost)
		mint := recentExitTestIndex(calls, named, "mint", MultiHopId{})
		return 0 <= named && 0 <= mint && 0 <= recentExitTestIndex(calls, mint, "mint", other)
	})
	named := recentExitTestIndex(calls, 0, "named", lost)
	mint := recentExitTestIndex(calls, named, "mint", MultiHopId{})
	AssertEqual(t, calls[mint].destination, lost)
	excluded := false
	for _, excludes := range generator.recordedDiscoveryExcludes() {
		excluded = excluded || slices.Contains(excludes, lost)
	}
	AssertEqual(t, excluded, true)
	waitRecentExitTestRemoved(t, window, client)
}

// A blackhole verdict on a fresh uplink is evidence against the provider: the
// exit is removed and replaced by discovery, never asked for by name.
func TestRecentExitVerdictRemovalIsNotRemembered(t *testing.T) {
	client := stickyTestLostChannel(errors.New("Blackhole no-send-ack (send 0/0B recv 0/0B syn 0/0 nackAge 31s synAge - dsts=2)"))
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{},
		onlineClientIds:        map[Id]bool{client.Destination().Tail(): true},
	}
	fresh := func(now time.Time) (bool, time.Time) { return false, time.Time{} }
	window := recentExitTestWindow(t, generator, unfixedTestProfile(WindowTypeSpeed), fresh, time.Now)
	installStickyTestChannel(window, client)

	waitRecentExitTestRemoved(t, window, client)
	start := recentExitTestCount(generator.recordedCalls(), "discover", MultiHopId{})
	calls := waitRecentExitTestCalls(t, generator, "discover twice more", func(calls []recentExitTestCall) bool {
		return start+2 <= recentExitTestCount(calls, "discover", MultiHopId{})
	})
	AssertEqual(t, recentExitTestCount(calls, "named", MultiHopId{}), 0)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
}

// A remembered exit that fails its evaluation is dropped, not retried: the
// window's later rounds discover without asking for it again.
func TestRecentExitThatFailsEvaluationIsNotRetried(t *testing.T) {
	client := stickyTestLostChannel(errTransportDownTimeout)
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{},
		onlineClientIds:        map[Id]bool{lost.Tail(): true},
	}
	log := newRecordingLogger()
	window := recentExitTestWindow(t, generator, unfixedTestProfile(WindowTypeSpeed), nil, nil, func(settings *MultiClientSettings) {
		settings.Log = log
	})
	installStickyTestChannel(window, client)

	calls := waitRecentExitTestCalls(t, generator, "evaluate its lost provider", func(calls []recentExitTestCall) bool {
		return 0 <= recentExitTestIndex(calls, 0, "evaluate", lost)
	})
	start := recentExitTestCount(calls, "discover", MultiHopId{})
	calls = waitRecentExitTestCalls(t, generator, "discover three times more", func(calls []recentExitTestCall) bool {
		return start+3 <= recentExitTestCount(calls, "discover", MultiHopId{})
	})
	AssertEqual(t, recentExitTestCount(calls, "named", lost), 1)
	AssertEqual(t, recentExitTestCount(calls, "evaluate", lost), 1)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
	deadline := time.Now().Add(30 * time.Second)
	for len(log.linesWith("action=drop")) == 0 {
		if deadline.Before(time.Now()) {
			t.Fatalf("no drop line for the failed evaluation: %v", log.linesWith("event=recent_exit"))
		}
		time.Sleep(time.Millisecond)
	}
	if drops := log.linesWith("action=drop"); !strings.Contains(drops[0], "provider="+relExitId(lost.Tail())) {
		t.Fatalf("the drop line does not name the provider: %s", drops[0])
	}
}

// With the memory timeout passed by the next round, the live window goes
// straight to discovery.
func TestRecentExitExpiredMemoryGoesStraightToDiscovery(t *testing.T) {
	client := stickyTestLostChannel(errTransportDownTimeout)
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{},
		onlineClientIds:        map[Id]bool{client.Destination().Tail(): true},
	}
	window := recentExitTestWindow(t, generator, unfixedTestProfile(WindowTypeSpeed), nil, nil, func(settings *MultiClientSettings) {
		settings.RecentExitMemoryTimeout = time.Nanosecond
	})
	installStickyTestChannel(window, client)

	waitRecentExitTestRemoved(t, window, client)
	start := recentExitTestCount(generator.recordedCalls(), "discover", MultiHopId{})
	calls := waitRecentExitTestCalls(t, generator, "discover twice more", func(calls []recentExitTestCall) bool {
		return start+2 <= recentExitTestCount(calls, "discover", MultiHopId{})
	})
	AssertEqual(t, recentExitTestCount(calls, "named", MultiHopId{}), 0)
	AssertEqual(t, len(recentExitTestHeld(&window.recentExits)), 0)
}
