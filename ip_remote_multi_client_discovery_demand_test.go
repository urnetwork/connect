package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Counts the actual generator calls and can delay one already empty response.
// Availability changes only the provider set; no fixture notification is sent.
type discoveryDemandTestGenerator struct {
	*reraceGenerator
	mutex              sync.Mutex
	qualityReads       int
	emptyReads         int
	mints              int
	retired            map[Id]int
	holdNextEmpty      bool
	emptyEntered       chan struct{}
	emptyRelease       chan struct{}
	emptyOnce          sync.Once
	configureNewClient func(*MultiClientGeneratorClientArgs, *ClientSettings)
}

func (self *discoveryDemandTestGenerator) NextDestinationsWithIpFamily(count int, exclude []MultiHopId, rank string, family IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	return self.NextDestinationsWithIpFamilyContext(context.Background(), count, exclude, rank, family)
}

func (self *discoveryDemandTestGenerator) NextDestinationsWithIpFamilyContext(ctx context.Context, count int, exclude []MultiHopId, rank string, family IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := self.reraceGenerator.NextDestinationsWithIpFamily(count, exclude, rank, family)
	self.mutex.Lock()
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
	self.mutex.Unlock()
	if hold {
		self.emptyOnce.Do(func() { close(self.emptyEntered) })
		select {
		case <-self.emptyRelease:
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
	g := &discoveryDemandTestGenerator{reraceGenerator: h.generator, retired: map[Id]int{}, emptyEntered: make(chan struct{}), emptyRelease: make(chan struct{})}
	t.Cleanup(func() {
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
		select {
		case <-g.emptyRelease:
		default:
			close(g.emptyRelease)
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
		}
		// Strictly after the empty snapshot, construction changes availability
		// only. The next real source retransmit must reassert discovery demand.
		time.Sleep(100 * time.Millisecond)
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		if !h.syn(failedPath) {
			t.Fatal("same-SYN retransmit was refused at source")
		}
		if holdEmpty {
			close(generator.emptyRelease)
			synctest.Wait()
		}
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
		if !h.syn(failedPath) {
			t.Fatal("original next SYN was not admitted")
		}
		if _, ok := h.synAck(failedPath); !ok || h.boundTo(failedPath) != h.channel("late") {
			t.Fatal("newly available exit did not answer the unchanged connect attempt")
		}
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
