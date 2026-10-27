package connect

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// Real enumerator calls own these counters. Barriers hold actual API results,
// never a production timer, source packet, or generator notification.
type discoveryDemandTestBudgetGenerator struct {
	testingEmptyMultiClientGenerator
	mutex                                              sync.Mutex
	workers                                            sync.WaitGroup
	results                                            map[MultiHopId]DestinationStats
	listingErr                                         error
	mintErr                                            error
	partial                                            bool
	fixed                                              bool
	reads, mints, created, retired, active, peak       int
	retireById                                         map[Id]int
	holdList, holdMint                                 bool
	listEntered, listRelease, mintEntered, mintRelease chan struct{}
	listOnce, mintOnce                                 sync.Once
}

func (self *discoveryDemandTestBudgetGenerator) FixedDestinationSize() (int, bool) {
	return 1, self.fixed
}

func (self *discoveryDemandTestBudgetGenerator) NextDestinations(count int, excluded []MultiHopId, rank string) (map[MultiHopId]DestinationStats, error) {
	return self.NextDestinationsContext(context.Background(), count, excluded, rank)
}

func (self *discoveryDemandTestBudgetGenerator) NextDestinationsContext(ctx context.Context, count int, excluded []MultiHopId, rank string) (map[MultiHopId]DestinationStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.workers.Add(1)
	defer self.workers.Done()
	self.mutex.Lock()
	self.reads++
	self.active++
	self.peak = max(self.peak, self.active)
	results, err, hold := maps.Clone(self.results), self.listingErr, self.holdList
	self.holdList = false
	self.mutex.Unlock()
	if hold {
		self.listOnce.Do(func() { close(self.listEntered) })
		select {
		case <-self.listRelease:
		case <-ctx.Done():
			if err == nil {
				results, err = nil, ctx.Err()
			}
		}
	}
	self.mutex.Lock()
	self.active--
	self.mutex.Unlock()
	return results, err
}

func (self *discoveryDemandTestBudgetGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	return self.NewClientArgsForDestinationContext(context.Background(), destination)
}

func (self *discoveryDemandTestBudgetGenerator) NewClientArgsForDestinationContext(ctx context.Context, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.workers.Add(1)
	defer self.workers.Done()
	self.mutex.Lock()
	self.mints++
	self.active++
	self.peak = max(self.peak, self.active)
	err, partial, hold := self.mintErr, self.partial, self.holdMint
	self.holdMint = false
	var args *MultiClientGeneratorClientArgs
	if err == nil || partial {
		args = &MultiClientGeneratorClientArgs{ClientId: NewId()}
		self.created++
	}
	self.mutex.Unlock()
	if hold {
		self.mintOnce.Do(func() { close(self.mintEntered) })
		select {
		case <-self.mintRelease:
		case <-ctx.Done():
			// Return any already-created identity even at cancellation. The
			// caller joins this response and owns its unused-args cleanup.
		}
	}
	self.mutex.Lock()
	self.active--
	self.mutex.Unlock()
	return args, err
}

func (self *discoveryDemandTestBudgetGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.retired++
	self.retireById[args.ClientId]++
}

func (self *discoveryDemandTestBudgetGenerator) counts() (reads, mints, created, retired, active, peak int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.reads, self.mints, self.created, self.retired, self.active, self.peak
}

type discoveryDemandTestBudgetFixture struct {
	window    *multiClientWindow
	generator *discoveryDemandTestBudgetGenerator
	update    *multiClientChannelUpdate
	client    *multiClientChannel
	owner     *multiClientEvaluationOwner
	cancel    context.CancelFunc
	done      chan struct{}
}

// The optional family-context capability is separate from the legacy ordinary
// listing method here, keeping the one ordinary setup read independently counted.
type discoveryDemandTestFamilyBudgetGenerator struct {
	*discoveryDemandTestBudgetGenerator
	families []IpFamilyFilter
}

func (self *discoveryDemandTestFamilyBudgetGenerator) NextDestinationsWithIpFamilyContext(ctx context.Context, count int, excluded []MultiHopId, rank string, family IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	self.mutex.Lock()
	self.families = append(self.families, family)
	self.mutex.Unlock()
	return self.NextDestinationsContext(ctx, count, excluded, rank)
}

func TestDiscoveryDemandPendingV4ThenV6UsesLatestFamily(t *testing.T) {
	testDiscoveryDemandMixedFamily(t, 6)
}
func TestDiscoveryDemandPendingV6ThenV4UsesLatestFamily(t *testing.T) {
	testDiscoveryDemandMixedFamily(t, 4)
}
func testDiscoveryDemandMixedFamily(t *testing.T, latestVersion int) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		generator := &discoveryDemandTestFamilyBudgetGenerator{discoveryDemandTestBudgetGenerator: f.generator}
		f.window.generator = generator
		f.client.args.DestinationStats.IpFamily = IpFamilyDualstack
		v6 := newMultiClientChannelUpdate(t.Context(), reraceFlow(6, 0, 52970))
		defer v6.Close()
		v6.noteConnectAttempt(v6.ipPath)
		v6.client.Store(f.client)
		if _, first := v6.dialFailureStanding(f.client, true, true, true); !first {
			t.Fatal("mixed-family control lacks an actual IPv6 refusal")
		}
		request6 := func() { f.window.requestDiscoveryDemand(v6, 6, v6.ipPath, nil) }
		if latestVersion == 6 {
			f.request()
			request6()
		} else {
			request6()
			f.request()
		}
		f.start()
		generator.mutex.Lock()
		families := append([]IpFamilyFilter(nil), generator.families...)
		generator.mutex.Unlock()
		wanted := IpFamilyFilterV4Capable
		if latestVersion == 6 {
			wanted = IpFamilyFilterV6Capable
		}
		if len(families) != 1 || families[0] != wanted {
			t.Fatalf("DISCOVERY_DEMAND_PENDING_FAMILY_MISMATCH: latest_ipv=%d actual_api_families=%v expected=%s", latestVersion, families, wanted)
		}
		if reads, mints, _, _, _, _ := f.generator.counts(); reads != 2 || mints != 0 {
			t.Fatal("mixed-family coalescing multiplied reads or mints")
		}
	})
}

func newDiscoveryDemandTestBudgetFixture(t *testing.T) *discoveryDemandTestBudgetFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	g := &discoveryDemandTestBudgetGenerator{results: map[MultiHopId]DestinationStats{}, retireById: map[Id]int{},
		listEntered: make(chan struct{}), listRelease: make(chan struct{}), mintEntered: make(chan struct{}), mintRelease: make(chan struct{})}
	w := discoveryRetryTestWindow(t)
	w.ctx, w.cancel, w.generator = ctx, cancel, g
	w.clientChannelArgs = make(chan *multiClientChannelArgs)
	id := NewId()
	client := &multiClientChannel{ctx: ctx, settings: w.settings, args: &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: id}, Destination: RequireMultiHopId(NewId())}}
	w.clients[id] = client
	u := newMultiClientChannelUpdate(ctx, unboundFlowTcpPath(52900))
	u.noteConnectAttempt(u.ipPath)
	u.client.Store(client)
	if standing, first := u.dialFailureStanding(client, true, true, true); standing != dialFailureBound || !first {
		t.Fatal("budget fixture has no actual current failed SYN attempt")
	}
	f := &discoveryDemandTestBudgetFixture{window: w, generator: g, update: u, client: client, owner: &multiClientEvaluationOwner{}, cancel: cancel}
	w.beginDiscoveryAcquisition(f.owner, ctx, time.Now().Add(15*time.Second), 2, 2)
	w.beginDiscoveryDemandSlot(f.owner)
	t.Cleanup(func() {
		cancel()
		for _, release := range []chan struct{}{g.listRelease, g.mintRelease} {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		if f.done != nil {
			<-f.done
		}
		synctest.Wait()
		g.workers.Wait()
		w.endDiscoveryAcquisition(f.owner)
		u.Close()
		_, _, created, retired, active, peak := g.counts()
		if created != retired || active != 0 || peak > 1 {
			t.Errorf("joined demand API custody: created=%d retired=%d active=%d peak=%d", created, retired, active, peak)
		}
		g.mutex.Lock()
		defer g.mutex.Unlock()
		for _, count := range g.retireById {
			if count != 1 {
				t.Errorf("identity retired %d times", count)
			}
		}
	})
	return f
}

func (self *discoveryDemandTestBudgetFixture) start() {
	self.done = make(chan struct{})
	go func() { defer close(self.done); self.window.randomEnumerateClientArgs() }()
	synctest.Wait()
}

func (self *discoveryDemandTestBudgetFixture) request() {
	self.window.requestDiscoveryDemand(self.update, 4, self.update.ipPath, nil)
}

func TestDiscoveryDemandCoalescesAndPacesActualReads(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.request() // before the enumerator subscribes: the pending level survives.
		f.start()
		if reads, _, _, _, _, _ := f.generator.counts(); reads != 2 {
			t.Fatalf("pre-subscription demand lost: reads=%d", reads)
		}
		for batch := range 4 {
			var callers sync.WaitGroup
			for range 8 {
				callers.Add(1)
				go func() { defer callers.Done(); f.request() }()
			}
			callers.Wait()
			synctest.Wait()
			reads, _, _, _, _, _ := f.generator.counts()
			if reads != min(2+batch, 4) {
				t.Fatalf("demand bypassed actual read pacing: batch=%d reads=%d", batch, reads)
			}
			time.Sleep(time.Second)
			synctest.Wait()
		}
		reads, mints, _, _, _, _ := f.generator.counts()
		if reads != 4 || mints != 0 {
			t.Fatalf("empty demand amplified work: reads=%d mints=%d", reads, mints)
		}
	})
}

func TestDiscoveryDemandNeverEndsResizeRest(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.window.noteDiscoveryExpandUnproductive(time.Now())
		before := f.window.discoveryRetry
		f.start()
		for range 32 {
			f.request()
		}
		time.Sleep(time.Second)
		synctest.Wait()
		reads, mints, _, _, _, _ := f.generator.counts()
		if reads != 0 || mints != 0 || f.window.discoveryRetry != before {
			t.Fatal("demand changed an ordinary expansion rest")
		}
	})
}

func TestDiscoveryDemandPreservesListingRateLimitAuthority(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.generator.listingErr = errors.New("429 rate limit")
		f.start()
		for range 32 {
			f.request()
		}
		f.generator.mutex.Lock()
		f.generator.listingErr = nil
		f.generator.mutex.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if reads, _, _, _, _, _ := f.generator.counts(); reads != 1 {
			t.Fatal("demand woke a platform rate-limit wait")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		reads, mints, _, _, _, _ := f.generator.counts()
		if reads != 3 || mints != 0 || f.window.rateLimitStreak.Load() != 1 {
			t.Fatalf("answered listing reset refused-platform streak or amplified work: reads=%d mints=%d streak=%d", reads, mints, f.window.rateLimitStreak.Load())
		}
	})
}

func TestDiscoveryDemandNonemptyResultOwnsOneMint(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		for range 3 {
			f.generator.results[RequireMultiHopId(NewId())] = DestinationStats{}
		}
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait() // the one minted argument is held on its actual offer.
		for range 32 {
			f.request()
		}
		time.Sleep(time.Second)
		synctest.Wait()
		reads, mints, created, retired, _, _ := f.generator.counts()
		if reads != 2 || mints != 1 || created != 1 || retired != 0 {
			t.Fatalf("nonempty listing minted ahead of its request: reads=%d mints=%d created=%d retired=%d", reads, mints, created, retired)
		}
		f.window.endDiscoveryAcquisition(f.owner)
		synctest.Wait()
		_, mints, created, retired, _, _ = f.generator.counts()
		if mints != 1 || created != 1 || retired != 1 {
			t.Fatal("unused demand argument was not retired exactly once")
		}
	})
}

func TestDiscoveryDemandNormalArgsWinInflightListing(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		f.generator.results[RequireMultiHopId(NewId())] = DestinationStats{}
		f.generator.holdList = true
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait()
		select {
		case <-f.generator.listEntered:
		default:
			t.Fatal("demand listing did not enter its actual result barrier")
		}
		// The ordinary consumer takes another ready argument while the API
		// result is held. That closes this slot, not a later request's slot.
		if !f.window.acceptDiscoveryDemandArgs(f.owner, &multiClientChannelArgs{}) {
			t.Fatal("ordinary argument was rejected")
		}
		synctest.Wait()
		if reads, mints, _, _, active, _ := f.generator.counts(); reads != 2 || mints != 0 || active != 0 {
			t.Fatal("cooperative canceled listing was not joined before the next request")
		}
		f.window.beginDiscoveryDemandSlot(f.owner)
		f.window.generatorMonitor.NotifyAll() // the real next-slot producer edge.
		synctest.Wait()
		close(f.generator.listRelease)
		synctest.Wait()
		reads, mints, _, _, active, _ := f.generator.counts()
		// One ordinary read/mint may now serve the new slot. The stale demand
		// result itself must never consume it or add a second mint.
		if reads != 3 || mints != 1 || active != 0 {
			t.Fatalf("stale listing crossed request slots: reads=%d mints=%d active=%d", reads, mints, active)
		}
	})
}

func TestDiscoveryDemandLateMintSuccessRetiresOnce(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		f.generator.results[RequireMultiHopId(NewId())] = DestinationStats{}
		f.generator.holdMint = true
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait()
		select {
		case <-f.generator.mintEntered:
		default:
			t.Fatal("demand mint did not enter its actual result barrier")
		}
		f.window.endDiscoveryAcquisition(f.owner)
		synctest.Wait()
		if reads, mints, _, retired, active, _ := f.generator.counts(); reads != 2 || mints != 1 || retired != 1 || active != 0 {
			t.Fatal("cooperative canceled mint failed to return and retire its identity")
		}
		f.generator.mutex.Lock()
		f.generator.results = map[MultiHopId]DestinationStats{}
		f.generator.mutex.Unlock()
		f.window.generatorMonitor.NotifyAll()
		close(f.generator.mintRelease)
		synctest.Wait()
		_, mints, created, retired, active, _ := f.generator.counts()
		if mints != 1 || created != 1 || retired != 1 || active != 0 {
			t.Fatal("late successful mint lost or duplicated retirement custody")
		}
	})
}

func TestDiscoveryDemandMintRefusalKeepsRateLimitStreak(t *testing.T) {
	testDiscoveryDemandMintRefusal(t, false)
}

func TestDiscoveryDemandPartialMintRefusalRetiresIdentity(t *testing.T) {
	testDiscoveryDemandMintRefusal(t, true)
}

func testDiscoveryDemandMintRefusal(t *testing.T, partial bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		f.generator.results[RequireMultiHopId(NewId())] = DestinationStats{}
		f.generator.mintErr, f.generator.partial = errors.New("429 rate limit"), partial
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait()
		for range 32 {
			f.request()
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if reads, mints, _, _, _, _ := f.generator.counts(); reads != 2 || mints != 1 || f.window.rateLimitStreak.Load() != 1 {
			t.Fatal("demand bypassed an actual mint-refusal wait")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		reads, mints, created, retired, _, _ := f.generator.counts()
		expectedCreated := 0
		if partial {
			expectedCreated = 1
		}
		if reads != 2 || mints != 1 || created != expectedCreated || retired != created {
			t.Fatal("refused/partial mint lost its one-attempt retirement budget")
		}
		f.generator.mutex.Lock()
		f.generator.results = map[MultiHopId]DestinationStats{}
		f.generator.mutex.Unlock()
		f.window.generatorMonitor.NotifyAll() // a separate ordinary producer opportunity.
		synctest.Wait()
		if reads, mints, _, _, _, _ := f.generator.counts(); reads != 3 || mints != 1 || f.window.rateLimitStreak.Load() != 1 {
			t.Fatal("a successful later listing reset the refused-mint streak")
		}
	})
}

func TestDiscoveryDemandDeadlineKeepsLateMintCustody(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		f.generator.results[RequireMultiHopId(NewId())] = DestinationStats{}
		f.generator.holdMint = true
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait()
		select {
		case <-f.generator.mintEntered:
		default:
			t.Fatal("deadline control lacks a real held mint")
		}
		time.Sleep(15*time.Second + time.Nanosecond) // the declared original acquisition deadline.
		synctest.Wait()
		if reads, mints, _, retired, active, _ := f.generator.counts(); reads != 2 || mints != 1 || retired != 1 || active != 0 {
			t.Fatal("cooperative acquisition expiry failed to join and retire its factory result")
		}
		f.generator.mutex.Lock()
		f.generator.results = map[MultiHopId]DestinationStats{}
		f.generator.mutex.Unlock()
		f.window.generatorMonitor.NotifyAll()
		close(f.generator.mintRelease)
		synctest.Wait()
		_, mints, created, retired, active, _ := f.generator.counts()
		if mints != 1 || created != 1 || retired != 1 || active != 0 {
			t.Fatal("deadline late success crossed into a future request")
		}
	})
}

func TestDiscoveryDemandInflightSourceCancellationCannotMint(t *testing.T) {
	testDiscoveryDemandInflightProof(t, "cancel")
}

func TestDiscoveryDemandInflightInboundAnswerCannotMint(t *testing.T) {
	testDiscoveryDemandInflightProof(t, "inbound")
}

func TestDiscoveryDemandInflightSourceAbaCannotMint(t *testing.T) {
	testDiscoveryDemandInflightProof(t, "aba")
}

func TestDiscoveryDemandInflightMembershipChangeCannotMint(t *testing.T) {
	testDiscoveryDemandInflightProof(t, "membership")
}

func testDiscoveryDemandInflightProof(t *testing.T, kind string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		f.generator.results[RequireMultiHopId(NewId())] = DestinationStats{}
		f.generator.holdList = true
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait()
		select {
		case <-f.generator.listEntered:
		default:
			t.Fatal("proof control lacks an actual in-flight nonempty API result")
		}
		switch kind {
		case "cancel":
			f.update.Close()
		case "inbound":
			f.update.receivedInbound.Store(true)
		case "aba":
			path := *f.update.ipPath
			path.SequenceNumber++
			f.update.noteConnectAttempt(&path)
			path.SequenceNumber--
			f.update.noteConnectAttempt(&path)
		case "membership":
			f.window.stateLock.Lock()
			for id, client := range f.window.clients {
				f.window.clients[id] = &multiClientChannel{ctx: client.ctx, settings: client.settings, args: client.args}
			}
			f.window.stateLock.Unlock()
		}
		close(f.generator.listRelease)
		synctest.Wait()
		if reads, mints, _, _, active, _ := f.generator.counts(); reads != 2 || mints != 0 || active != 0 || f.window.errorStreak.Load() != 0 || f.window.rateLimitStreak.Load() != 0 {
			t.Fatal("stale in-flight observation minted a candidate or invented a platform failure")
		}
	})
}

func TestDiscoveryDemandLateRateLimitBlocksNextProducer(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.start()
		f.generator.mutex.Lock()
		f.generator.holdList = true
		f.generator.listingErr = errors.New("429 rate limit")
		f.generator.mutex.Unlock()
		f.request()
		synctest.Wait()
		f.window.endDiscoveryAcquisition(f.owner)
		synctest.Wait()
		f.window.generatorMonitor.NotifyAll()
		f.generator.mutex.Lock()
		f.generator.listingErr = nil
		f.generator.mutex.Unlock()
		close(f.generator.listRelease)
		synctest.Wait()
		if reads, _, _, _, _, _ := f.generator.counts(); reads != 2 || f.window.rateLimitStreak.Load() != 1 {
			t.Fatal("late backend refusal lost its platform authority")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if reads, _, _, _, _, _ := f.generator.counts(); reads != 2 {
			t.Fatal("normal producer bypassed a late rate-limit response")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if reads, _, _, _, _, _ := f.generator.counts(); reads != 3 || f.window.rateLimitStreak.Load() != 1 {
			t.Fatal("late rate-limit wait or successful-listing streak handling changed")
		}
	})
}

func TestDiscoveryDemandLocalCancellationIsNotBackendFailure(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var backendCalls atomic.Int64
		read := &discoveryDemandRead{ctx: ctx}
		_, _ = callDiscoveryDemand(f.window, read, time.Second, func(context.Context) (int, error) {
			if err := ctx.Err(); err != nil {
				return 0, &discoveryDemandLocalCancel{cause: err}
			}
			backendCalls.Add(1)
			return 1, nil
		})
		synctest.Wait()
		if backendCalls.Load() != 0 || f.window.errorStreak.Load() != 0 || f.window.rateLimitStreak.Load() != 0 {
			t.Fatal("local precall cancellation fabricated platform failure or fabricated backend work")
		}
	})
}

func TestDiscoveryDemandStaleArgsLeaveCurrentSlotUntouched(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		oldSlot := f.window.discoveryDemand.slot
		f.window.beginDiscoveryDemandSlot(f.owner)
		current := f.window.discoveryDemand
		notify := f.window.generatorMonitor.NotifyChannel()
		stale := &multiClientChannelArgs{discoveryDemandOwner: f.owner, discoveryDemandSlot: oldSlot}
		if f.window.acceptDiscoveryDemandArgs(f.owner, stale) {
			t.Fatal("stale argument entered a successor slot")
		}
		if f.window.discoveryDemand.slot != current.slot || !f.window.discoveryDemand.slotOpen || current.slotCtx.Err() != nil {
			t.Fatal("stale argument canceled legitimate current request work")
		}
		select {
		case <-notify:
			t.Fatal("stale rejection fabricated a normal generator opportunity")
		default:
		}
	})
}

func TestDiscoveryDemandFullWindowDeclines(t *testing.T) {
	testDiscoveryDemandDeclined(t, "full")
}

func TestDiscoveryDemandWarnedOwnedCeilingDeclines(t *testing.T) {
	testDiscoveryDemandDeclined(t, "warned-full")
}

func TestDiscoveryDemandFixedGeneratorDeclines(t *testing.T) {
	testDiscoveryDemandDeclined(t, "fixed")
}

func TestDiscoveryDemandCanceledSourceDeclines(t *testing.T) {
	testDiscoveryDemandDeclined(t, "source-canceled")
}

func TestDiscoveryDemandEstablishedSourceDeclines(t *testing.T) {
	testDiscoveryDemandDeclined(t, "source-answered")
}

func TestDiscoveryDemandCanceledAcquisitionDeclines(t *testing.T) {
	testDiscoveryDemandDeclined(t, "epoch-canceled")
}

func testDiscoveryDemandDeclined(t *testing.T, kind string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		switch kind {
		case "full":
			f.window.settings.StrictWindowSizeHardMax = true
			id := NewId()
			second := &multiClientChannel{ctx: t.Context(), settings: f.window.settings,
				args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: id}, Destination: RequireMultiHopId(NewId())}}
			f.window.clients[id] = second
			f.update.client.Store(second)
			if _, first := f.update.dialFailureStanding(second, true, true, true); !first {
				t.Fatal("full-window premise lacks the second offered member's actual refusal")
			}
		case "warned-full":
			f.window.settings.StrictWindowSizeHardMax = true
			id := NewId()
			f.window.clients[id] = &multiClientChannel{ctx: t.Context(), settings: f.window.settings, warning: true,
				args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: id}}}
		case "fixed":
			f.window.endDiscoveryAcquisition(f.owner)
			f.generator.fixed = true
			f.window.beginDiscoveryAcquisition(f.owner, t.Context(), time.Now().Add(time.Minute), 2, 2)
			f.window.beginDiscoveryDemandSlot(f.owner)
		case "source-canceled":
			f.update.Close()
		case "source-answered":
			f.update.receivedInbound.Store(true)
		case "epoch-canceled":
			f.window.endDiscoveryAcquisition(f.owner)
		}
		before := f.window.discoveryRetry
		if (kind == "full" || kind == "warned-full") && (!f.window.settings.StrictWindowSizeHardMax || len(f.window.clients) != f.window.discoveryDemand.hardMax || f.window.discoveryDemand.targetSize != f.window.discoveryDemand.hardMax) {
			t.Fatal("strict full-window premise has inconsistent ownership/target settings")
		}
		f.request()
		if f.window.discoveryDemand.pending || f.window.discoveryRetry != before {
			t.Fatal("ineligible demand changed owner/backoff state")
		}
	})
}

func TestDiscoveryDemandNonStrictWarnedOwnerKeepsBackfill(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		if f.window.settings.StrictWindowSizeHardMax {
			t.Fatal("legacy characterization unexpectedly uses strict ownership")
		}
		id := NewId()
		warned := &multiClientChannel{ctx: t.Context(), settings: f.window.settings, warning: true,
			args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: id}, Destination: RequireMultiHopId(NewId())}}
		f.window.clients[id] = warned
		before := f.window.discoveryRetry
		f.request()
		if !f.window.discoveryDemand.pending {
			t.Fatal("non-strict warned-owner backfill lost its existing acquisition opportunity")
		}
		f.start()
		if reads, mints, _, _, _, _ := f.generator.counts(); reads != 2 || mints != 0 || f.window.clients[id] != warned || !warned.warning || f.window.discoveryRetry != before {
			t.Fatal("non-strict characterization changed ownership/backoff or exceeded one empty read")
		}
	})
}

func TestDiscoveryDemandPendingSourceCancellationRevokes(t *testing.T) {
	testDiscoveryDemandPendingProof(t, "cancel")
}

func TestDiscoveryDemandPendingAbaRevokes(t *testing.T) {
	testDiscoveryDemandPendingProof(t, "aba")
}

func TestDiscoveryDemandPendingReplacementRevokes(t *testing.T) {
	testDiscoveryDemandPendingProof(t, "membership")
}

func testDiscoveryDemandPendingProof(t *testing.T, kind string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		f.request()
		if !f.window.discoveryDemand.pending {
			t.Fatal("control lacks a queued current demand")
		}
		switch kind {
		case "cancel":
			f.update.Close()
		case "aba":
			path := *f.update.ipPath
			path.SequenceNumber++
			f.update.noteConnectAttempt(&path)
			path.SequenceNumber--
			f.update.noteConnectAttempt(&path)
		case "membership":
			for id, client := range f.window.clients {
				copy := &multiClientChannel{ctx: client.ctx, settings: client.settings, args: client.args}
				f.window.clients[id] = copy
			}
		}
		f.start()
		if reads, mints, _, _, _, _ := f.generator.counts(); reads != 1 || mints != 0 {
			t.Fatal("expired queued proof authorized an extra read or mint")
		}
	})
}

func TestDiscoveryDemandSourceClockAndAba(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newDiscoveryDemandTestBudgetFixture(t)
		u := f.update
		path := *u.ipPath
		// Start a genuine new unanswered source generation, without making a
		// refusal, strike, service-cache entry or successful receive.
		path.SequenceNumber++
		u.noteConnectAttempt(&path)
		start, stamp := u.discoveryAttemptStarted, u.discoveryAttemptStamp.Load()
		candidates := []*multiClientChannel{f.client}
		if _, eligible := u.observeDiscoveryAttempt(candidates, time.Now(), &path); eligible {
			t.Fatal("new SYN inherited the old attempt's silence age")
		}
		time.Sleep(time.Second)
		u.noteConnectAttempt(&path)
		if u.discoveryAttemptStarted != start || u.discoveryAttemptStamp.Load() != stamp {
			t.Fatal("same-ISN retransmit restarted the source clock")
		}
		time.Sleep(2 * time.Second)
		observation, eligible := u.observeDiscoveryAttempt(candidates, time.Now(), &path)
		if !eligible {
			t.Fatal("wide unanswered source clock never became demand")
		}
		u.stateLock.Lock()
		u.resetSynGenerationWithLock(path.SequenceNumber)
		u.stateLock.Unlock()
		if u.discoveryAttemptStamp.Load() != observation.stamp || u.discoveryAttemptStarted != start {
			t.Fatal("delayed source completion revoked the actual attempt clock")
		}
		other := path
		other.SequenceNumber++
		u.noteConnectAttempt(&other)
		u.noteConnectAttempt(&path)
		if u.discoveryAttemptStamp.Load() == observation.stamp || !u.discoveryAttemptStarted.After(start) {
			t.Fatal("observed ABA reused the prior attempt's identity/age")
		}
		if _, eligible := u.observeDiscoveryAttempt(candidates, time.Now(), &path); eligible {
			t.Fatal("ABA source inherited old silence demand")
		}
		u.stateLock.Lock()
		u.discoveryAttemptStamp.Add(1)
		u.sourceReset.Store(true)
		u.stateLock.Unlock()
		u.noteConnectAttempt(&path)
		time.Sleep(3 * time.Second)
		if _, eligible := u.observeDiscoveryAttempt(candidates, time.Now(), &path); eligible {
			t.Fatal("same-ISN replay reopened an explicitly reset source")
		}
	})
}

func TestDiscoveryDemandMetadataFootprint(t *testing.T) {
	t.Logf("discovery demand metadata: update_bytes=%d acquisition_bytes=%d read_bytes=%d source_clock_bytes=%d; one pending flow/member snapshot and one joined cooperative API call per window; no retained packets or per-flow timers", unsafe.Sizeof(multiClientChannelUpdate{}), unsafe.Sizeof(discoveryAcquisitionDemand{}), unsafe.Sizeof(discoveryDemandRead{}), unsafe.Sizeof(time.Time{})+unsafe.Sizeof(atomic.Uint64{}))
}
