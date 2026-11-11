package connect

import (
	"context"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Counts actual generator calls and can gate one empty or available response.
// Availability changes only the provider set; no fixture notification is sent.
type discoveryDemandTestGenerator struct {
	*reraceGenerator
	mutex                 sync.Mutex
	qualityReads          int
	resultOverrideForTest map[MultiHopId]DestinationStats
	emptyReads            int
	mints                 int
	retired               map[Id]int
	holdNextEmpty         bool
	emptyEntered          chan struct{}
	emptyRelease          chan struct{}
	emptyOnce             sync.Once
	holdNextAvailable     bool
	availableEntered      chan struct{}
	availableRelease      chan struct{}
	configureNewClient    func(*MultiClientGeneratorClientArgs, *ClientSettings)
	// the platform's strict window handover override each listing answers
	// with, nil for none, which the generator takes from each answer as the
	// api generator takes it from find-providers2
	platformStrictWindowHandover atomic.Pointer[bool]
	strictWindowHandoverOverride strictWindowHandoverOverride
}

// Implements MultiClientGeneratorWithStrictWindowHandoverOverride.
func (self *discoveryDemandTestGenerator) StrictWindowHandoverOverride() (enabled bool, set bool) {
	return self.strictWindowHandoverOverride.get()
}

func (self *discoveryDemandTestGenerator) NextDestinationsWithIpFamily(count int, exclude []MultiHopId, rank string, family IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	return self.NextDestinationsWithIpFamilyContext(context.Background(), count, exclude, rank, family)
}

func (self *discoveryDemandTestGenerator) NextDestinationsWithIpFamilyContext(ctx context.Context, count int, exclude []MultiHopId, rank string, family IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := self.reraceGenerator.NextDestinationsWithIpFamily(count, exclude, rank, family)
	if err == nil {
		self.strictWindowHandoverOverride.observe(self.platformStrictWindowHandover.Load())
	}
	self.mutex.Lock()
	if rank == "quality" && self.resultOverrideForTest != nil {
		result = make(map[MultiHopId]DestinationStats, len(self.resultOverrideForTest))
		for destination, stats := range self.resultOverrideForTest {
			result[destination] = stats
		}
	}
	if rank == "quality" {
		self.qualityReads++
		if err == nil && len(result) == 0 {
			self.emptyReads++
		}
	}
	hold := rank == "quality" && err == nil && len(result) == 0 && self.holdNextEmpty
	if hold {
		self.holdNextEmpty = false
	}
	holdAvailable := rank == "quality" && err == nil && len(result) != 0 && self.holdNextAvailable
	if holdAvailable {
		self.holdNextAvailable = false
	}
	self.mutex.Unlock()
	if hold {
		self.emptyOnce.Do(func() { close(self.emptyEntered) })
		select {
		case <-self.emptyRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if holdAvailable {
		close(self.availableEntered)
		select {
		case <-self.availableRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result, err
}

// An optional test configurator runs before the actual client starts workers.
func (self *discoveryDemandTestGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
	self.mutex.Lock()
	configure := self.configureNewClient
	self.mutex.Unlock()
	if configure != nil {
		configure(args, settings)
	}
	return self.reraceGenerator.NewClient(ctx, args, settings)
}

func (self *discoveryDemandTestGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	self.mutex.Lock()
	self.mints++
	self.mutex.Unlock()
	return self.reraceGenerator.NewClientArgsForDestination(destination)
}

func (self *discoveryDemandTestGenerator) NewClientArgsForDestinationContext(ctx context.Context, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return self.NewClientArgsForDestination(destination)
}

func (self *discoveryDemandTestGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.mutex.Lock()
	self.retired[args.ClientId]++
	self.mutex.Unlock()
	self.reraceGenerator.RemoveClientArgs(args)
}

func (self *discoveryDemandTestGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.RemoveClientArgs(args)
}

func (self *discoveryDemandTestGenerator) counts() (reads, empty, mints, retired int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, count := range self.retired {
		retired += count
	}
	return self.qualityReads, self.emptyReads, self.mints, retired
}

// Reuses the canonical in-process providers and native public source path. Only
// the counting wrapper differs from newReraceHarness; every worker joins.
func newDiscoveryDemandTestHarness(t *testing.T, specs []reraceExitSpec, configure ...func(*MultiClientSettings)) (*reraceHarness, *discoveryDemandTestGenerator) {
	t.Helper()
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)
	ctx, cancel := context.WithCancel(t.Context())
	h := &reraceHarness{t: t, ctx: ctx,
		generator: &reraceGenerator{clientExits: map[Id]*reraceExit{}, unsubs: map[Id]func(){}},
		log:       newRecordingLogger(), source: SourceId(NewId()), synTimes: map[string][]time.Time{}}
	for _, spec := range specs {
		h.addExit(spec)
	}
	g := &discoveryDemandTestGenerator{reraceGenerator: h.generator, retired: map[Id]int{},
		emptyEntered: make(chan struct{}), emptyRelease: make(chan struct{}),
		availableEntered: make(chan struct{}), availableRelease: make(chan struct{})}
	t.Cleanup(func() {
		// Channel and enumerator retirement may finish after CloseAndWait.
		h.settle()
		_, _, mints, retired := g.counts()
		if mints != retired {
			t.Errorf("joined public fixture retired %d of %d actual minted identities", retired, mints)
		}
		g.mutex.Lock()
		defer g.mutex.Unlock()
		for _, count := range g.retired {
			if count != 1 {
				t.Errorf("an actual minted identity retired %d times", count)
			}
		}
	})
	settings := DefaultMultiClientSettings()
	settings.Log, settings.ProviderProbe, settings.HeartbeatInterval = h.log, false, 0
	settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 5, WindowSizeMax: 5, WindowSizeHardMax: 5}
	settings.StrictWindowSizeHardMax, settings.StandingReserve, settings.EvaluationPoolMultiple = true, false, 1
	settings.MaxFlowsPerExit = 1
	for _, configureSettings := range configure {
		configureSettings(settings)
	}
	h.multiClient = NewRemoteUserNatMultiClient(ctx, g, h.receive, protocol.ProvideMode_Network, settings)
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range h.log.lines() {
				t.Log(line)
			}
		}
		for _, release := range []chan struct{}{g.emptyRelease, g.availableRelease} {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		if err := h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		cancel()
		for _, client := range h.generator.clientList() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		for _, exit := range h.generator.exitList() {
			if err := exit.provider.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		h.exitWorkers.Wait()
	})
	h.waitForExits(len(specs))
	h.start = time.Now()
	return h, g
}

func discoveryDemandTestCarryFour(t *testing.T, h *reraceHarness) (*multiClientWindow, []*IpPath, []*multiClientChannel) {
	t.Helper()
	window := h.multiClient.windows[WindowTypeQuality]
	paths, owners := make([]*IpPath, 4), make([]*multiClientChannel, 4)
	seen := map[*multiClientChannel]bool{}
	for index := range paths {
		path := unboundFlowTcpPath(52810 + index)
		path.DestinationIp = net.IPv4(203, 0, 113, byte(70+index)).To4()
		paths[index] = path
		if !h.syn(path) {
			t.Fatal("healthy original sibling refused its SYN")
		}
		owners[index] = h.boundTo(path)
		if _, ok := h.synAck(path); !ok || owners[index] == nil || seen[owners[index]] {
			t.Fatal("healthy siblings lack four distinct established owners")
		}
		seen[owners[index]] = true
	}
	payload := []byte("held original CSS sibling")
	for bucket := range 4 {
		if bucket != 0 {
			time.Sleep(window.settings.StatsWindowBucketDuration + time.Nanosecond)
		}
		for _, path := range paths {
			if !h.send(reraceSegment(path, uint32(bucket*len(payload)), payload)) {
				t.Fatal("original sibling stopped delivering real health traffic")
			}
		}
	}
	for _, owner := range owners {
		stats, err := owner.WindowStats()
		if err != nil {
			t.Fatal("sibling window stats failed: ", err)
		}
		sent, received := stats.EffectiveByteCount()
		if !stats.healthy || sent <= 0 || received <= 0 || owner.IsDone() || owner.isWarning() || h.multiClient.clientFlowCount(owner) != 1 {
			t.Fatal("control premise lacks four positive healthy carrying exits")
		}
	}
	return window, paths, owners
}

func TestDiscoveryDemandFindsProviderAfterEmptyRead(t *testing.T) {
	testDiscoveryDemandLateVisibility(t, false)
}

func TestDiscoveryDemandReassertsDuringInflightEmptyRead(t *testing.T) {
	testDiscoveryDemandLateVisibility(t, true)
}

// Discovery may observe an unanswered wide race without redefining it as an
// inferred provider failure, a committed winner, or useful receive evidence.
func TestDiscoveryDemandObservesSilentWideRace(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		failedPath := unboundFlowTcpPath(52840)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(path *IpPath) reraceAnswer {
				if path.DestinationIp.Equal(failedPath.DestinationIp) {
					return reraceSilent
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs)
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		publication := discoveryDemandTestObservePublication(window)
		window.stateLock.Lock()
		deadline := window.discoveryDemand.deadline
		live := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen
		window.stateLock.Unlock()
		if !live {
			t.Fatal("silent observation lacks the ordinary vacant live acquisition")
		}
		beforeReads, _, beforeMints, _ := generator.counts()
		start := time.Now()
		if !h.syn(failedPath) {
			t.Fatal("silent original SYN was not source-admitted")
		}
		initial := h.update(failedPath)
		initial.stateLock.Lock()
		initialWide := initial.client.Load() == nil && initial.race != nil && len(initial.race.clientStates) == 4
		initial.stateLock.Unlock()
		if !initialWide {
			t.Fatal("silent control did not form the actual four-way cold race")
		}
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		h.syn(failedPath)
		if reads, _, _, _ := generator.counts(); reads != beforeReads {
			t.Fatal("silent demand preceded its actual source age")
		}
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		h.syn(failedPath)
		synctest.Wait()
		publicationBound := discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[3]))
		publicationReady := discoveryDemandTestAwaitPublication(t, publication, publicationBound)
		reads, _, mints, _ := generator.counts()
		update := h.update(failedPath)
		update.stateLock.Lock()
		failed, bound, inbound, width := len(update.dialFailedClients), update.client.Load(), update.receivedInbound.Load(), 0
		if update.race != nil {
			width = len(update.race.clientStates)
		}
		update.stateLock.Unlock()
		if !publicationReady || !time.Now().Before(publicationBound) || len(h.liveChannels()) != 5 || reads != beforeReads+1 || mints != beforeMints+1 || failed != 0 || h.metrics().DialFailuresIntercepted != 0 || h.metrics().FlowsReraced != 0 {
			t.Fatalf("silent observation changed failure/race authority: owned=%d reads=%d mints=%d failed=%d width=%d", len(h.liveChannels()), reads-beforeReads, mints-beforeMints, failed, width)
		}
		if bound != nil || inbound {
			if _, answered := h.synAck(failedPath); !answered || bound != h.channel("late") || len(h.exit("late").syns(failedPath)) == 0 {
				t.Fatal("silent demand fabricated a winner without real source/provider traffic")
			}
		} else if width != 4 {
			t.Fatal("unanswered original race lost its actual four-way cohort")
		}
		h.multiClient.stateLock.Lock()
		serviceEntries := len(h.multiClient.destinationServiceFailures)
		h.multiClient.stateLock.Unlock()
		if serviceEntries != 0 {
			t.Fatal("silent discovery observation wrote negative service memory")
		}
		for index, path := range paths {
			if h.boundTo(path) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() {
				t.Fatal("silent demand changed an original healthy owner")
			}
		}
	})
}

func testDiscoveryDemandLateVisibility(t *testing.T, holdEmpty bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		failedPath := unboundFlowTcpPath(52800)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(path *IpPath) reraceAnswer {
				if path.DestinationIp.Equal(failedPath.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs)
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		publication := discoveryDemandTestObservePublication(window)
		payload := []byte("held original CSS sibling")
		synctest.Wait()
		window.stateLock.Lock()
		premise := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen && time.Until(window.discoveryDemand.deadline) > 4*time.Second
		deadline := window.discoveryDemand.deadline
		window.stateLock.Unlock()
		if !premise || len(h.liveChannels()) != 4 || h.multiClient.backendDegraded() {
			t.Fatal("control premise lacks the ordinary vacant live acquisition")
		}
		readsBefore, emptyBefore, mintsBefore, retiredBefore := generator.counts()
		generator.mutex.Lock()
		generator.holdNextEmpty = holdEmpty
		generator.mutex.Unlock()
		start := time.Now()
		if !h.syn(failedPath) {
			t.Fatal("original source SYN was not admitted")
		}
		update := h.update(failedPath)
		update.stateLock.Lock()
		failed, inbound, bound := len(update.dialFailedClients), update.receivedInbound.Load(), update.client.Load()
		update.stateLock.Unlock()
		readsFirst, emptyFirst, _, _ := generator.counts()
		if failed != 4 || inbound || bound != nil || len(h.liveChannels()) != 4 || h.metrics().DialFailuresIntercepted != 4 {
			t.Fatal("control premise lacks the actual same-SYN four-provider refusal")
		}
		if readsFirst != readsBefore+1 || emptyFirst != emptyBefore+1 {
			t.Fatalf("control premise: first exhausted demand was not a real empty API read: before=%d/%d after=%d/%d", readsBefore, emptyBefore, readsFirst, emptyFirst)
		}
		if holdEmpty {
			select {
			case <-generator.emptyEntered:
			default:
				t.Fatal("in-flight empty read did not reach its owned barrier")
			}
		} else {
			generator.mutex.Lock()
			generator.holdNextAvailable = true
			generator.mutex.Unlock()
		}
		update.stateLock.Lock()
		originalOwner := update.synRetry
		var originalSerial uint64
		var originalCredit *transferRetainedControlCredit
		if originalOwner != nil {
			originalSerial, originalCredit = originalOwner.serial, originalOwner.credit
		}
		update.stateLock.Unlock()
		collapseBeforeLate := h.multiClient.TcpCollapseDropCount()
		sourceFirst := discoveryDemandTestHoldAutomaticRetry(t, h, failedPath, update)
		// Strictly after the empty snapshot, construction changes availability
		// only. The next real source retransmit must reassert discovery demand.
		time.Sleep(100 * time.Millisecond)
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		if !h.syn(failedPath) {
			t.Fatal("same-SYN retransmit was refused at source")
		}
		// Keep this control's publication after the reasserting offer. A
		// separately covered order lets that same offer use the late provider.
		if len(h.liveChannels()) != 4 || h.boundTo(failedPath) != nil || update.receivedInbound.Load() || len(h.exit("late").syns(failedPath)) != 0 {
			t.Fatal("late provider published before the reasserting source offer completed")
		}
		if holdEmpty {
			close(generator.emptyRelease)
		} else {
			select {
			case <-generator.availableEntered:
			default:
				t.Fatal("available demand read did not reach its owned barrier")
			}
			close(generator.availableRelease)
		}
		synctest.Wait()
		publicationBound := discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[2]))
		publicationReady := discoveryDemandTestAwaitPublication(t, publication, publicationBound)
		reads, empty, mints, retired := generator.counts()
		owned := len(h.liveChannels())
		t.Logf("late-visibility before_deadline=%t elapsed=%s owned=%d failed_first=%d explicit_signals=%d reads=%d->%d empty=%d->%d mints=%d->%d retired=%d->%d held_empty=%t", time.Now().Before(deadline), time.Since(start), owned, failed, h.metrics().DialFailuresIntercepted, readsBefore, reads, emptyBefore, empty, mintsBefore, mints, retiredBefore, retired, holdEmpty)
		if owned != 5 || !publicationReady || !time.Now().Before(publicationBound) {
			t.Fatalf("DISCOVERY_DEMAND_LATE_VISIBLE_NOT_RELISTED: owned=%d with an available fifth provider before the unchanged next SYN; publication_event=%t", owned, publicationReady)
		}
		if !time.Now().Before(deadline) || reads != readsBefore+2 || mints != mintsBefore+1 || retired != retiredBefore {
			t.Fatal("late admission exceeded its original read/mint/custody budget")
		}
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		sourceFirst.requireHeld(t)
		if !h.syn(failedPath) {
			t.Logf("DISCOVERY_MANUAL_WINNER_SCALARS %v", discoveryDemandManualWinnerSnapshot(h, failedPath, update, originalOwner, originalSerial, originalCredit, collapseBeforeLate))
			t.Fatal("original next SYN was not admitted")
		}
		if _, ok := h.synAck(failedPath); !ok || h.boundTo(failedPath) != h.channel("late") {
			t.Fatal("newly available exit did not answer the unchanged connect attempt")
		}
		sourceFirst.requireManualWinnerAndRelease(t)
		for index, path := range paths {
			if h.boundTo(path) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() ||
				!h.send(reraceSegment(path, uint32(4*len(payload)), payload)) || len(h.delivered(path)) != 6 {
				t.Fatal("level demand disturbed an original healthy sibling")
			}
		}
	})
}

// The callback is an observed completion, not an admission claim. Callers
// separately require the actual live healthy late owner and unchanged source
// outcome. No timer, generator notification, packet or success is forced.
func discoveryDemandTestObservePublication(window *multiClientWindow) <-chan struct{} {
	done := make(chan struct{})
	var once sync.Once
	window.afterExpandPingResultForTest = func() { once.Do(func() { close(done) }) }
	return done
}

func discoveryDemandTestAwaitPublication(t testing.TB, event <-chan struct{}, deadline time.Time) bool {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-event:
		return true
	case <-timer.C:
		return false
	}
}

func discoveryDemandTestEarlierTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}

// A full strict quality window keeps four established siblings and one idle
// slot. Only the new navigation's exact service fails on its original exits.
func TestDiscoveryDemandFullWindowServiceExhaustionFindsLateProvider(t *testing.T) {
	testDiscoveryDemandFullWindowPublicationOrder(t, false)
}

// The retained source may establish before an unchanged native offer passes
// its gate. That earlier success must keep its exact duplicate suppression.
func TestDiscoveryDemandFullWindowRetainedPublicationWinsBeforeNativeSyn(t *testing.T) {
	testDiscoveryDemandFullWindowPublicationOrder(t, true)
}

// Real API, dispatch and response barriers choose each winner while keeping
// the full strict window, four carrying siblings and original paid generation.
func testDiscoveryDemandFullWindowPublicationOrder(t *testing.T, automaticFirst bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(52891)
		specs := make([]reraceExitSpec, 5)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) && candidate.DestinationPort == path.DestinationPort {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) {
			settings.StrictWindowHandover = true
		})
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		synctest.Wait()
		window.stateLock.Lock()
		full := len(window.clients) == 5 && window.discoveryDemand.owner == nil
		window.stateLock.Unlock()
		if !full || window.strictWindowAdmissionAllowed(NewId(), window.settings.WindowSizes[WindowTypeQuality]) || h.multiClient.backendDegraded() {
			t.Fatal("full-window control lacks five healthy owners at its exact strict cap")
		}
		beforeReads, _, beforeMints, _ := generator.counts()
		publication := discoveryDemandTestObservePublication(window)
		start := time.Now()
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("original native navigation SYN was not source-admitted")
		}
		update := h.update(path)
		if update == nil {
			t.Fatal("source-admitted navigation lost its canonical update")
		}
		update.stateLock.Lock()
		refused, inbound, bound := len(update.dialFailedClients), update.receivedInbound.Load(), update.client.Load()
		update.stateLock.Unlock()
		if refused != 5 || inbound || bound != nil || h.metrics().DialFailuresIntercepted != 5 {
			t.Fatalf("full-window control did not observe five actual same-service refusals: failed=%d intercepted=%d", refused, h.metrics().DialFailuresIntercepted)
		}
		originalOwner := discoveryDemandServiceRefreshOwner(t, window, update)
		originalSerial, originalCredit := originalOwner.serial, originalOwner.credit
		publicationBound := start.Add(reraceSynSchedule[2])
		dispatchEntered, dispatchRelease, answeredEvent := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce, answeredOnce sync.Once
		var dispatchVisits, retainedGateVisits, sourceGateVisits atomic.Uint32
		var dispatchOwnerValid, retainedOwnerValid atomic.Bool
		releaseDispatch := func() { releaseOnce.Do(func() { close(dispatchRelease) }) }
		t.Cleanup(releaseDispatch)
		h.multiClient.beforeSynRetryDispatchForTest = func(current *multiClientChannelUpdate) {
			if current != update {
				return
			}
			current.stateLock.Lock()
			valid := current.synRetry == originalOwner && originalOwner.update == current && originalOwner.serial == originalSerial &&
				originalOwner.credit == originalCredit && originalOwner.packet != nil && originalOwner.sequence == path.SequenceNumber &&
				!originalOwner.queued && originalOwner.validWithLock(current)
			current.stateLock.Unlock()
			dispatchOwnerValid.Store(valid)
			dispatchVisits.Add(1)
			enteredOnce.Do(func() { close(dispatchEntered) })
			select {
			case <-dispatchRelease:
			case <-h.multiClient.ctx.Done():
			}
		}
		h.multiClient.SetReceivePacketCallback(discoveryDemandPublicationReceiveObserver(h.receive, func(reply *IpPath, packet []byte) {
			if reply == nil || !(reraceSameFlow(reply, path) || reraceSameFlow(reply, path.Reverse())) {
				return
			}
			if parsed, err := ParseIpPath(packet); err == nil && parsed.Syn && parsed.Ack && parsed.AckSequenceNumber == path.SequenceNumber+1 {
				answeredOnce.Do(func() { close(answeredEvent) })
			}
		}))
		generator.mutex.Lock()
		generator.holdNextAvailable = true
		generator.mutex.Unlock()
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if current != update || !reraceSameFlow(group.ipPath, path) {
				return
			}
			if group.isSynRetry() {
				retainedOwnerValid.Store(group.synRetry.update == update && group.synRetry.serial == originalSerial &&
					group.synRetry.source == h.source && group.ipPath.SequenceNumber == path.SequenceNumber)
				retainedGateVisits.Add(1)
				return
			}
			if sourceGateVisits.Add(1) != 1 {
				return
			}
			if snapshot != nil || !group.ipPath.Syn || group.ipPath.Ack || group.synRetry == nil ||
				group.synRetry.update != update || group.synRetry.nativeOwnerSerial != originalSerial || group.ipPath.SequenceNumber != path.SequenceNumber {
				t.Fatal("full-window publication gate lost the unchanged native source and exact retained owner")
			}
			if !discoveryDemandTestAwaitPublication(t, generator.availableEntered, publicationBound) {
				t.Fatal("full-window demand did not reach its real available API response")
			}
			close(generator.availableRelease)
			if !discoveryDemandTestAwaitPublication(t, publication, publicationBound) || !time.Now().Before(publicationBound) ||
				!discoveryDemandTestAwaitPublication(t, dispatchEntered, publicationBound) {
				t.Fatal("full-window demand did not publish and dispatch before the original next native SYN")
			}
			synctest.Wait()
			window.stateLock.Lock()
			finished := window.serviceRefreshOwner == nil && window.discoveryDemand.owner == nil && len(window.clients) == 5
			window.stateLock.Unlock()
			update.stateLock.Lock()
			held := update.synRetry == originalOwner && originalOwner.references == 2 && originalOwner.validWithLock(update) &&
				update.client.Load() == nil && !update.receivedInbound.Load()
			update.stateLock.Unlock()
			if !finished || !held || !dispatchOwnerValid.Load() || dispatchVisits.Load() != 1 || originalCredit.released.Load() ||
				retainedGateVisits.Load() != 0 || len(h.exit("late").syns(path)) != 0 {
				t.Fatal("full-window publication did not leave exactly cache and held dispatcher ownership")
			}
			if automaticFirst {
				releaseDispatch()
				if !discoveryDemandTestAwaitPublication(t, answeredEvent, publicationBound) {
					t.Fatal("full-window retained source did not deliver its actual SYN-ACK before native selection")
				}
				synctest.Wait()
				if h.update(path) != update || h.boundTo(path) != h.channel("late") || !update.receivedInbound.Load() ||
					len(h.exit("late").syns(path)) != 1 || retainedGateVisits.Load() != 1 || !retainedOwnerValid.Load() {
					t.Fatal("full-window retained winner lost the original source, physical SYN or response ownership")
				}
			}
		}
		// Physical availability follows the real refusals. It sends no generator
		// notification, changes no timer, and creates no discovery identity.
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		collapseBefore := h.multiClient.TcpCollapseDropCount()
		accepted := privateCrossWindowNativeSyn(h, path)
		if accepted != !automaticFirst {
			t.Fatalf("full-window winner admission mismatch: automatic_first=%t accepted=%t", automaticFirst, accepted)
		}
		// Use the next unchanged source retransmission boundary. Virtual time
		// advances only after the real enumerator and packet workers are idle.
		published := discoveryDemandTestAwaitPublication(t, publication, publicationBound)
		synctest.Wait()
		reads, _, mints, _ := generator.counts()
		window.stateLock.Lock()
		installed := len(window.clients)
		window.stateLock.Unlock()
		if installed > 5 || mints > beforeMints+1 || reads > beforeReads+3 {
			t.Fatalf("service recovery exceeded its existing window/read/mint bounds: installed=%d reads=%d mints=%d", installed, reads-beforeReads, mints-beforeMints)
		}
		if !published || reads == beforeReads {
			t.Fatalf("DISCOVERY_DEMAND_FULL_WINDOW_SERVICE_EXHAUSTION_DID_NOT_RELIST: published=%t reads=%d mints=%d installed=%d", published, reads-beforeReads, mints-beforeMints, installed)
		}
		late := h.channel("late")
		if late.IsDone() || late.isWarning() || installed != 5 {
			t.Fatal("bounded full-window recovery did not publish one healthy late owner at the existing ceiling")
		}
		if _, answered := h.synAck(path); !answered || h.boundTo(path) != late || h.update(path) != update {
			t.Fatal("published alternative did not establish the unchanged navigation generation")
		}
		wantedCollapse, wantedRetainedVisits := uint64(0), uint32(0)
		if automaticFirst {
			wantedCollapse, wantedRetainedVisits = 1, 1
		} else {
			update.stateLock.Lock()
			held := update.synRetry == nil && originalOwner.update == update && originalOwner.references == 1
			update.stateLock.Unlock()
			if !held || originalCredit.released.Load() {
				t.Fatal("full-window native winner lost its held dispatch reference or released credit early")
			}
		}
		if h.multiClient.TcpCollapseDropCount() != collapseBefore+wantedCollapse || sourceGateVisits.Load() != 1 ||
			retainedGateVisits.Load() != wantedRetainedVisits || len(h.exit("late").syns(path)) != 1 {
			t.Fatal("full-window winner changed exact duplicate suppression or sent duplicate retained bytes")
		}
		releaseDispatch()
		h.settle()
		synRetryControlReleased(t, originalOwner)
		if !originalCredit.released.Load() || !originalOwner.publicationRetired.Load() || originalOwner.publicationIndexed ||
			dispatchVisits.Load() != 1 || retainedGateVisits.Load() != wantedRetainedVisits {
			t.Fatal("full-window publication retained paid cache, dispatch or index ownership")
		}
		wire := h.exit("late").syns(path)
		reply, answered := h.synAck(path)
		parsed, err := ParseIpPath(reply.packet)
		if !answered || err != nil || parsed.AckSequenceNumber != path.SequenceNumber+1 || len(wire) != 1 || wire[0].path.SequenceNumber != path.SequenceNumber {
			t.Fatal("full-window publication changed the original native SYN or SYN-ACK generation")
		}
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		if privateCrossWindowNativeSyn(h, path) || h.multiClient.TcpCollapseDropCount() != collapseBefore+wantedCollapse+1 ||
			len(h.exit("late").syns(path)) != 1 || h.boundTo(path) != late {
			t.Fatal("full-window established source lost exact duplicate suppression")
		}
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
		t.Logf("full-window publication: automatic_first=%t native_accepted=%t source_gate_visits=%d retained_gate_visits=%d collapse_delta=%d late_syns=%d reads=%d mints=%d installed=%d credit_released=%t", automaticFirst, accepted, sourceGateVisits.Load(), retainedGateVisits.Load(), h.multiClient.TcpCollapseDropCount()-collapseBefore, len(h.exit("late").syns(path)), reads-beforeReads, mints-beforeMints, installed, originalCredit.released.Load())
		reraceAssertNoErrorDelivered(t, h, path)
	})
}

// A late provider's publication follows the source offer's completed selection.
// This isolates continuation from discovery in the existing vacant acquisition.
func TestDiscoveryDemandLatePublicationRetriesRetainedNativeSyn(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(52892)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) && candidate.DestinationPort == path.DestinationPort {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) {
			settings.StrictWindowHandover = true
		})
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		synctest.Wait()
		window.stateLock.Lock()
		deadline := window.discoveryDemand.deadline
		live := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen && time.Until(deadline) > reraceSynSchedule[2]
		window.stateLock.Unlock()
		if !live || len(h.liveChannels()) != 4 || h.multiClient.backendDegraded() {
			t.Fatal("late-publication control lacks the existing vacant live acquisition")
		}
		publication := discoveryDemandTestObservePublication(window)
		beforeReads, beforeEmpty, beforeMints, beforeRetired := generator.counts()
		start := time.Now()
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("original native navigation SYN was not admitted")
		}
		update := h.update(path)
		if update == nil {
			t.Fatal("original source lost its canonical update")
		}
		update.stateLock.Lock()
		refused, inbound, bound := len(update.dialFailedClients), update.receivedInbound.Load(), update.client.Load()
		update.stateLock.Unlock()
		reads, empty, _, _ := generator.counts()
		if refused != 4 || inbound || bound != nil || h.metrics().DialFailuresIntercepted != 4 || reads != beforeReads+1 || empty != beforeEmpty+1 {
			t.Fatal("late-publication control lacks four actual refusals and one completed empty listing")
		}
		synRetryControlOwner(t, update)
		generator.mutex.Lock()
		generator.holdNextAvailable = true
		generator.mutex.Unlock()
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("reasserting native source SYN was not admitted")
		}
		select {
		case <-generator.availableEntered:
		default:
			t.Fatal("actual late listing did not reach its owned availability barrier")
		}
		if len(h.liveChannels()) != 4 || len(h.exit("late").syns(path)) != 0 || update.receivedInbound.Load() {
			t.Fatal("late provider escaped before the reasserting source offer returned")
		}
		close(generator.availableRelease)
		publicationBound := discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[2]))
		if !discoveryDemandTestAwaitPublication(t, publication, publicationBound) {
			t.Fatal("existing vacant acquisition failed to publish the gated real late provider")
		}
		synctest.Wait()
		late := h.channel("late")
		reads, empty, mints, retired := generator.counts()
		if len(h.liveChannels()) != 5 || reads != beforeReads+2 || empty != beforeEmpty+1 || mints != beforeMints+1 || retired != beforeRetired {
			t.Fatal("late-publication control changed its original listing/mint/admission custody")
		}
		// Observe the target before healthy sibling sends can wake the parent
		// worker. Later progress is checked without changing this receipt.
		_, answered := h.synAck(path)
		targetOwner, targetSynCount := h.boundTo(path), len(h.exit("late").syns(path))
		sameUpdate, targetInbound := h.update(path) == update, update.receivedInbound.Load()
		discoveryDemandFullWindowAssertSiblings(t, h, paths, owners)
		if !answered || targetOwner != late || targetSynCount != 1 {
			t.Fatalf("SYN_RETRY_FRESH_PROVIDER_PUBLICATION_WAS_NOT_CONSUMED: provider_syns=%d answered=%t same_update=%t", targetSynCount, answered, sameUpdate)
		}
		if !sameUpdate || !targetInbound {
			t.Fatal("publication continuation established a different flow generation")
		}
		reraceAssertNoErrorDelivered(t, h, path)
	})
}

// Exercise each original body owner after the observation, not just its flags.
func discoveryDemandFullWindowAssertSiblings(t *testing.T, h *reraceHarness, paths []*IpPath, owners []*multiClientChannel) {
	t.Helper()
	payload := []byte("held original CSS sibling")
	for index, path := range paths {
		if h.boundTo(path) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() ||
			!h.send(reraceSegment(path, uint32(4*len(payload)), payload)) || len(h.delivered(path)) != 6 {
			t.Fatal("service recovery changed an original healthy body's owner or progress")
		}
	}
}

// The OLD source has only the cache reference. A service-capable source must
// identify this exact window reservation before its second reference is valid.
func discoveryDemandServiceRefreshOwner(t *testing.T, window *multiClientWindow, update *multiClientChannelUpdate) *tcpSynRetry {
	t.Helper()
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := update.synRetry
	expected := uint32(1)
	field := reflect.ValueOf(window).Elem().FieldByName("serviceRefreshOwner")
	if field.IsValid() && !field.IsNil() {
		if owner == nil || field.Pointer() != reflect.ValueOf(owner).Pointer() {
			t.Fatal("service window retained a different source owner")
		}
		expected++
	}
	if owner == nil || owner.packet == nil || owner.credit == nil || owner.serial == 0 || owner.references != expected {
		t.Fatalf("service source lacks exact cache/window ownership: expected_refs=%d", expected)
	}
	return owner
}

// Failure-only, sequential observations after h.syn already settled its actual
// result. No packet, wake, ownership or acceptance predicate is changed.
func discoveryDemandManualWinnerSnapshot(h *reraceHarness, path *IpPath, update *multiClientChannelUpdate, original *tcpSynRetry, serial uint64, credit *transferRetainedControlCredit, collapseBefore uint64) map[string]any {
	current := h.update(path)
	late := h.channel("late")
	wire := h.exit("late").syns(path)
	result := map[string]any{"same_update": current == update, "bound_late": h.boundTo(path) == late, "native_offers": len(h.sentSyns(path)), "late_wire_syns": len(wire), "original_serial": serial, "collapse_before": collapseBefore, "collapse_after": h.multiClient.TcpCollapseDropCount(), "late_done": late.IsDone(), "late_warning": late.isWarning()}
	update.stateLock.Lock()
	result["received_inbound"] = update.receivedInbound.Load()
	result["current_owner_present"] = update.synRetry != nil
	result["current_owner_same_original"] = update.synRetry == original
	result["failed_clients"] = len(update.dialFailedClients)
	if original != nil {
		result["original_refs"] = original.references
		result["original_queued"] = original.queued
		result["original_packet_present"] = original.packet != nil
		result["original_update_present"] = original.update != nil
		result["original_publication_indexed"] = original.publicationIndexed
	}
	update.stateLock.Unlock()
	result["original_credit_present"] = credit != nil
	if credit != nil {
		result["original_credit_released"] = credit.released.Load()
	}
	if len(wire) != 0 {
		result["wire_isn"] = wire[0].path.SequenceNumber
		result["wire_original_isn"] = wire[0].path.SequenceNumber == path.SequenceNumber
	}
	packet, answered := h.synAck(path)
	result["real_syn_ack"] = answered
	if answered {
		reply, err := ParseIpPath(packet.packet)
		result["syn_ack_parse_ok"] = err == nil
		if err == nil {
			result["ack_isn"] = reply.AckSequenceNumber
			result["ack_original_isn"] = reply.AckSequenceNumber == path.SequenceNumber+1
		}
	}
	return result
}
