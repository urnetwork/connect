// A discovery window's refill while the payer is out of balance (see
// ip_remote_multi_client_balance_hold.go). The composed tests run a real multi
// client over in-process providers in a synctest bubble: a strict 5/5/5
// quality window, one spare provider, and the process counters. Once the
// balance turns short, every new window client's contract is refused with
// InsufficientBalance by its control, as the platform refuses it, and the
// window loses an exit, so it must refill.
package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The counting discovery generator, whose new clients' contracts the payer's
// balance refuses while it is short, and which answers balance probes as the
// platform would.
type balanceHoldTestGenerator struct {
	*discoveryDemandTestGenerator

	stateLock sync.Mutex
	short     bool
	// while set, the next probe signals probeEntered and waits for
	// probeRelease
	holdNextProbe bool
	probeEntered  chan struct{}
	probeRelease  chan struct{}
	// every contract request a short client sent, all refused
	refusals int
	// the destination each probe asked for, and when
	probeDestinationIds []Id
	probeTimes          []time.Time
}

func (self *balanceHoldTestGenerator) setShort(short bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.short = short
}

func (self *balanceHoldTestGenerator) refusalCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.refusals
}

func (self *balanceHoldTestGenerator) probes() ([]Id, []time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.probeDestinationIds), slices.Clone(self.probeTimes)
}

// Implements MultiClientGeneratorBalanceProbe.
func (self *balanceHoldTestGenerator) ProbeBalance(ctx context.Context, destinationId Id) BalanceProbeOutcome {
	self.stateLock.Lock()
	hold := self.holdNextProbe
	self.holdNextProbe = false
	self.stateLock.Unlock()
	if hold {
		close(self.probeEntered)
		select {
		case <-self.probeRelease:
		case <-ctx.Done():
			return BalanceProbeUnknown
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.probeDestinationIds = append(self.probeDestinationIds, destinationId)
	self.probeTimes = append(self.probeTimes, time.Now())
	if self.short {
		return BalanceProbeShort
	}
	return BalanceProbeRestored
}

// While short, a client as the rerace generator builds it, except that its
// sends to the provider need a contract and its control refuses every one.
func (self *balanceHoldTestGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	self.stateLock.Lock()
	short := self.short
	self.stateLock.Unlock()
	if !short {
		return self.discoveryDemandTestGenerator.NewClient(ctx, args, clientSettings)
	}

	rerace := self.discoveryDemandTestGenerator.reraceGenerator
	rerace.mutex.Lock()
	exit := rerace.clientExits[args.ClientId]
	rerace.mutex.Unlock()
	if exit == nil {
		return nil, errors.New("client args were not minted for a destination")
	}
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	// virtual time precedes the default historical enable date
	clientSettings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(0, 0)
	oob := &balanceHoldTestOob{
		contractErrorOob: contractErrorOob{clientId: args.ClientId, errorCount: -1},
		generator:        self,
	}
	client := NewClient(ctx, args.ClientId, oob, clientSettings)
	routeSend := make(chan []byte)
	routeReceive := make(chan []byte)
	transportSend := NewSendGatewayTransport()
	transportReceive := NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(transportSend, []Route{routeSend})
	client.RouteManager().UpdateTransport(transportReceive, []Route{routeReceive})

	providerTransportSend := NewSendClientTransport(DestinationId(args.ClientId))
	providerTransportReceive := NewReceiveGatewayTransport()
	exit.provider.RouteManager().UpdateTransport(providerTransportReceive, []Route{routeSend})
	exit.provider.RouteManager().UpdateTransport(providerTransportSend, []Route{routeReceive})
	exit.provider.ContractManager().AddNoContractPeer(client.ClientId())

	rerace.mutex.Lock()
	defer rerace.mutex.Unlock()
	rerace.clients = append(rerace.clients, client)
	rerace.unsubs[args.ClientId] = func() {
		client.RouteManager().RemoveTransport(transportSend)
		client.RouteManager().RemoveTransport(transportReceive)
		exit.provider.RouteManager().RemoveTransport(providerTransportReceive)
		exit.provider.RouteManager().RemoveTransport(providerTransportSend)
	}
	return client, nil
}

// A control that refuses every contract for the payer's balance and counts
// each refusal with its generator.
type balanceHoldTestOob struct {
	contractErrorOob
	generator *balanceHoldTestGenerator
}

func (self *balanceHoldTestOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	for _, frame := range frames {
		if frame.MessageType == protocol.MessageType_TransferCreateContract {
			self.generator.stateLock.Lock()
			self.generator.refusals += 1
			self.generator.stateLock.Unlock()
		}
	}
	self.contractErrorOob.SendControl(frames, callback)
}

// The window stats harness over the balance generator.
func newBalanceHoldTestHarness(t *testing.T, configure ...func(*MultiClientSettings)) (*reraceHarness, *balanceHoldTestGenerator, *WindowClientStats) {
	t.Helper()
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)
	stats := NewWindowClientStats()
	ctx, cancel := context.WithCancel(t.Context())
	h := &reraceHarness{t: t, ctx: ctx,
		generator: &reraceGenerator{clientExits: map[Id]*reraceExit{}, unsubs: map[Id]func(){}},
		log:       newRecordingLogger(), source: SourceId(NewId()), synTimes: map[string][]time.Time{}}
	specs := []reraceExitSpec{{name: "a"}, {name: "b"}, {name: "c"}, {name: "d"}, {name: "e"}}
	for _, spec := range specs {
		h.addExit(spec)
	}
	g := &balanceHoldTestGenerator{discoveryDemandTestGenerator: &discoveryDemandTestGenerator{reraceGenerator: h.generator, retired: map[Id]int{},
		emptyEntered: make(chan struct{}), emptyRelease: make(chan struct{}),
		availableEntered: make(chan struct{}), availableRelease: make(chan struct{})}}
	t.Cleanup(func() {
		_, _, mints, retired := g.counts()
		if mints != retired {
			t.Errorf("joined public fixture retired %d of %d actual minted identities", retired, mints)
		}
	})
	settings := DefaultMultiClientSettings()
	settings.Log, settings.ProviderProbe, settings.HeartbeatInterval = h.log, false, 0
	settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 5, WindowSizeMax: 5, WindowSizeHardMax: 5}
	settings.StrictWindowSizeHardMax, settings.StandingReserve, settings.EvaluationPoolMultiple = true, false, 1
	settings.WindowClientStats = stats
	settings.MaxFlowsPerExit = DefaultMultiClientSettings().MaxFlowsPerExit
	settings.StrictWindowHandover = true
	for _, configureSettings := range configure {
		configureSettings(settings)
	}
	h.multiClient = NewRemoteUserNatMultiClient(ctx, g, h.receive, protocol.ProvideMode_Network, settings)
	t.Cleanup(func() {
		if t.Failed() {
			lines := h.log.lines()
			for _, line := range lines[max(0, len(lines)-200):] {
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
	synctest.Wait()
	h.addExit(reraceExitSpec{name: "spare"})
	return h, g, stats
}

// Forms the window, then the balance turns short and one exit is lost, so the
// window must refill against refused contracts. Returns the lost exit's
// replacement destinations: every provider not in the window.
func balanceHoldTestRunOutAndLoseAnExit(t *testing.T, h *reraceHarness, generator *balanceHoldTestGenerator) {
	t.Helper()
	h.sleep(time.Second)
	window := h.multiClient.windows[WindowTypeQuality]
	if installedCount, _ := strictHandoverTestCounts(window); installedCount != 5 {
		t.Fatalf("the window formed with %d clients, want 5", installedCount)
	}
	generator.setShort(true)
	lost := h.liveChannels()[0]
	lost.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
	lost.Cancel()
}

// The loop the probe exists to stop, with the probe off: once the balance is
// short, a below-target window builds a candidate on every expansion step,
// each candidate's contract wait is refused again and again, and each
// candidate is retired, for as long as the window lives.
func TestOutOfBalanceWindowRebuildsCandidatesWithoutTheProbe(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, _ := newBalanceHoldTestHarness(t)
		balanceHoldTestRunOutAndLoseAnExit(t, h, generator)
		h.sleep(2 * time.Minute)
		_, _, mintsBefore, retiredBefore := generator.counts()
		refusalsBefore := generator.refusalCount()

		h.sleep(10 * time.Minute)

		_, _, mints, retired := generator.counts()
		if mints-mintsBefore < 3 {
			t.Fatalf("%d candidates were minted in 10 minutes out of balance, want the loop of at least 3", mints-mintsBefore)
		}
		if retired-retiredBefore < 3 {
			t.Fatalf("%d candidates were retired in 10 minutes out of balance, want at least 3", retired-retiredBefore)
		}
		if refusals := generator.refusalCount() - refusalsBefore; refusals < 2*(mints-mintsBefore) {
			t.Fatalf("%d contract refusals for %d candidates, want several for each", refusals, mints-mintsBefore)
		}
		if probeDestinationIds, _ := generator.probes(); len(probeDestinationIds) != 0 {
			t.Fatalf("%d balance probes with the probe off", len(probeDestinationIds))
		}
	})
}

// With the probe on, the first refusal holds the window: it builds no more
// candidates, mints no more identities and asks for no more contracts, and
// each expansion step sends one balance probe for the refused provider
// instead, on the expansion backoff.
func TestOutOfBalanceWindowProbesInsteadOfBuilding(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.OutOfBalanceRefillProbe = true
		})
		window := h.multiClient.windows[WindowTypeQuality]
		balanceHoldTestRunOutAndLoseAnExit(t, h, generator)
		h.sleep(2 * time.Minute)
		reads, _, mintsBefore, _ := generator.counts()
		refusalsBefore := generator.refusalCount()
		if refusalsBefore == 0 {
			t.Fatal("the refill's candidate asked for no contract")
		}
		probeIdsBefore, _ := generator.probes()
		window.stateLock.Lock()
		failuresBefore := window.discoveryRetry.failureCount
		window.stateLock.Unlock()

		h.sleep(10 * time.Minute)

		readsAfter, _, mints, _ := generator.counts()
		AssertEqual(t, mints, mintsBefore)
		AssertEqual(t, readsAfter, reads)
		AssertEqual(t, generator.refusalCount(), refusalsBefore)
		probeDestinationIds, probeTimes := generator.probes()
		probes := len(probeDestinationIds) - len(probeIdsBefore)
		// one per expansion step: the backoff's steps reach its 2-minute cap,
		// jittered down to half, so 10 minutes hold at most about a dozen,
		// where a probe on every 15-second resize pass would send forty
		if probes < 3 || 16 < probes {
			t.Fatalf("%d balance probes in 10 held minutes, want one per expansion step", probes)
		}
		// each held step is unproductive, exactly as a failed expansion is
		window.stateLock.Lock()
		failures := window.discoveryRetry.failureCount
		window.stateLock.Unlock()
		AssertEqual(t, failures-failuresBefore, probes)
		// one probe per step: at the backoff's own spacing, never faster than
		// its minimum
		for i := 1; i < len(probeTimes); i += 1 {
			if gap := probeTimes[i].Sub(probeTimes[i-1]); gap < DefaultMultiClientSettings().DiscoveryExpandRetryMin/2 {
				t.Fatalf("probes %d and %d were %s apart, inside the backoff", i-1, i, gap)
			}
		}
		// each asks for a provider whose contract the balance refused, not for
		// one already in the window
		inWindow := map[Id]bool{}
		for _, client := range h.liveChannels() {
			inWindow[client.Destination().Tail()] = true
		}
		for _, destinationId := range probeDestinationIds {
			if inWindow[destinationId] {
				t.Fatalf("a probe asked for %s, an exit in the window", destinationId)
			}
		}
		held := stats.expandPasses[windowStatsWindowQuality][windowExpandPassHeldInsufficientBalance].Load()
		AssertEqual(t, held, uint64(len(probeDestinationIds)))
		AssertEqual(t, stats.balanceProbes[windowStatsWindowQuality][BalanceProbeShort].Load(), uint64(len(probeDestinationIds)))
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("the held window holds %d clients, want the 4 it kept", installedCount)
		}
		// the window is still held
		if _, ok := window.balanceRefillHeld(); !ok {
			t.Fatal("the window is no longer held while the balance is short")
		}
	})
}

// A top-up resumes the refill of a device a user is sending on at the next
// expansion step, the step at which the window would have expanded without the
// probe: the probe finds the balance restored and the same pass builds and
// admits the replacement. The restore spread leaves this timing exactly as it
// was (see ip_remote_multi_client_balance_restore_test.go for an idle device).
func TestOutOfBalanceWindowResumesWithinOneBackoffStep(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.OutOfBalanceRefillProbe = true
		})
		window := h.multiClient.windows[WindowTypeQuality]
		balanceHoldTestRunOutAndLoseAnExit(t, h, generator)
		h.sleep(5 * time.Minute)
		if _, ok := window.balanceRefillHeld(); !ok {
			t.Fatal("the window was not held after its refill's contracts were refused")
		}

		generator.setShort(false)
		toppedUp := time.Now()
		window.stateLock.Lock()
		nextStep := window.discoveryRetry.notBefore
		window.stateLock.Unlock()
		if !nextStep.After(toppedUp) {
			t.Fatalf("the held window has no next expansion step after the top-up")
		}

		// the user is still trying: a connection attempt inside
		// BalanceRestoreSpreadIdle of the step
		h.sleep(max(0, nextStep.Sub(toppedUp)-30*time.Second))
		h.syn(reraceFlow(4, 0, 40001))

		// nothing is built before the step, as without the probe
		h.sleep(nextStep.Sub(time.Now()) - time.Second)
		if installedCount, _ := strictHandoverTestCounts(window); installedCount != 4 {
			t.Fatalf("the window refilled to %d before its next expansion step", installedCount)
		}
		// and the step itself refills it
		h.sleep(2 * time.Second)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		if _, ok := window.balanceRefillHeld(); ok {
			t.Fatal("the window is still held after the probe found the balance restored")
		}
		AssertEqual(t, stats.balanceProbes[windowStatsWindowQuality][BalanceProbeRestored].Load(), uint64(1))
	})
}

// A window whose balance is never short runs exactly the ordinary refill with
// the probe on: one lost exit, one mint, one admission, and no probe.
func TestInBalanceWindowRefillUnchangedByTheProbe(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.OutOfBalanceRefillProbe = true
		})
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		_, _, mintsBefore, _ := generator.counts()
		admittedBefore := windowStatsCandidateTotal(stats, windowCandidateAdmitted)

		lost := h.liveChannels()[0]
		lost.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
		lost.Cancel()
		h.sleep(5 * time.Minute)

		_, _, mints, _ := generator.counts()
		AssertEqual(t, mints-mintsBefore, 1)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmitted)-admittedBefore, uint64(1))
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		probeDestinationIds, _ := generator.probes()
		AssertEqual(t, len(probeDestinationIds), 0)
		AssertEqual(t, stats.expandPasses[windowStatsWindowQuality][windowExpandPassHeldInsufficientBalance].Load(), uint64(0))
	})
}

// The enumerator lists when its generator monitor wakes it: an expansion
// does, and so do an outcome rebuild and a family probe. A held window's
// enumerator woken that way lists and mints nothing, even once its expansion
// step has come and the backoff no longer defers it, while the step's probe is
// out: the hold, not the backoff, keeps it quiet. A probe that outlasts a
// resize interval (a slow control) does not change that.
func TestHeldWindowEnumeratorListsNothingWhileItsProbeIsOut(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, _ := newBalanceHoldTestHarness(t, func(settings *MultiClientSettings) {
			settings.OutOfBalanceRefillProbe = true
		})
		window := h.multiClient.windows[WindowTypeQuality]
		balanceHoldTestRunOutAndLoseAnExit(t, h, generator)
		h.sleep(5 * time.Minute)
		if _, ok := window.balanceRefillHeld(); !ok {
			t.Fatal("the window was not held after its refill's contracts were refused")
		}
		generator.stateLock.Lock()
		generator.holdNextProbe = true
		generator.probeEntered = make(chan struct{})
		generator.probeRelease = make(chan struct{})
		generator.stateLock.Unlock()
		reads, _, mints, _ := generator.counts()
		// a wake that is not an expansion, as an outcome rebuild's
		window.generatorMonitor.NotifyAll()

		<-generator.probeEntered
		// every other goroutine has run to a wait with the step's deferral over
		synctest.Wait()
		if _, deferred := window.discoveryExpandDeferral(time.Now()); deferred {
			t.Fatal("the backoff still defers the window at its expansion step")
		}
		readsDuring, _, mintsDuring, _ := generator.counts()
		AssertEqual(t, readsDuring, reads)
		AssertEqual(t, mintsDuring, mints)
		h.sleep(2 * DefaultMultiClientSettings().WindowResizeTimeout)
		readsDuring, _, mintsDuring, _ = generator.counts()
		AssertEqual(t, readsDuring, reads)
		AssertEqual(t, mintsDuring, mints)

		close(generator.probeRelease)
		h.sleep(time.Second)
		readsAfter, _, mintsAfter, _ := generator.counts()
		AssertEqual(t, readsAfter, reads)
		AssertEqual(t, mintsAfter, mints)
	})
}

// A generator that answers every probe with one outcome.
type balanceHoldFixedProbeGenerator struct {
	testingEmptyMultiClientGenerator
	outcome BalanceProbeOutcome
	probes  int
}

func (self *balanceHoldFixedProbeGenerator) ProbeBalance(ctx context.Context, destinationId Id) BalanceProbeOutcome {
	self.probes += 1
	return self.outcome
}

func newBalanceHoldTestWindow(generator MultiClientGenerator, enabled bool) *multiClientWindow {
	settings := DefaultMultiClientSettings()
	settings.OutOfBalanceRefillProbe = enabled
	return &multiClientWindow{
		ctx:           context.Background(),
		settings:      settings,
		generator:     generator,
		windowType:    WindowTypeQuality,
		resizeMonitor: NewMonitor(),
	}
}

func balanceHoldTestStatus(destinationId Id, contractError *protocol.ContractError) *ContractStatus {
	return &ContractStatus{
		Key:   ContractKey{Destination: DestinationId(destinationId)},
		Error: contractError,
	}
}

// A refusal for the balance holds the window and names the provider to probe;
// a later refusal names the newer one; a result about something else leaves
// the hold; a granted contract releases it and wakes the resize pass.
func TestBalanceHoldFollowsTheContractStatus(t *testing.T) {
	window := newBalanceHoldTestWindow(&balanceHoldFixedProbeGenerator{}, true)
	short := protocol.ContractError_InsufficientBalance
	setup := protocol.ContractError_Setup
	first, second := NewId(), NewId()

	window.noteContractStatusForBalanceHold(balanceHoldTestStatus(first, &short))
	destinationId, held := window.balanceRefillHeld()
	if !held || destinationId != first {
		t.Fatalf("held=%t destination=%s after a refusal for the balance", held, destinationId)
	}
	window.noteContractStatusForBalanceHold(balanceHoldTestStatus(second, &short))
	if destinationId, held = window.balanceRefillHeld(); !held || destinationId != second {
		t.Fatalf("held=%t destination=%s after a second refusal", held, destinationId)
	}
	window.noteContractStatusForBalanceHold(balanceHoldTestStatus(first, &setup))
	if _, held = window.balanceRefillHeld(); !held {
		t.Fatal("a result about something else released the hold")
	}

	resized := window.resizeMonitor.NotifyChannel()
	window.noteContractStatusForBalanceHold(balanceHoldTestStatus(first, nil))
	if _, held = window.balanceRefillHeld(); held {
		t.Fatal("a granted contract left the window held")
	}
	select {
	case <-resized:
	default:
		t.Fatal("the release did not wake the resize pass")
	}
}

// Off, on a fixed window, or with a generator that cannot probe, nothing holds.
func TestBalanceHoldNeedsTheSettingADiscoveryWindowAndAProbe(t *testing.T) {
	short := protocol.ContractError_InsufficientBalance
	fixed := &contractStatusTestGenerator{fixed: true}
	for _, window := range []*multiClientWindow{
		newBalanceHoldTestWindow(&balanceHoldFixedProbeGenerator{}, false),
		newBalanceHoldTestWindow(fixed, true),
		newBalanceHoldTestWindow(&testingEmptyMultiClientGenerator{}, true),
	} {
		window.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), &short))
		if _, held := window.balanceRefillHeld(); held {
			t.Fatalf("a window held with generator %T and the setting %t", window.generator, window.settings.OutOfBalanceRefillProbe)
		}
	}
}

// What one held step does with each probe outcome: a refusal for the balance or
// a rate limit keeps the hold and counts as an unproductive expansion, so the
// backoff advances as it does for a failed expansion; a restored balance or a
// probe that could not answer releases the hold, and the pass expands.
func TestBalanceProbeOutcomeDecidesTheStep(t *testing.T) {
	short := protocol.ContractError_InsufficientBalance
	for _, c := range []struct {
		outcome BalanceProbeOutcome
		holds   bool
	}{
		{outcome: BalanceProbeShort, holds: true},
		{outcome: BalanceProbeLimited, holds: true},
		{outcome: BalanceProbeRestored, holds: false},
		{outcome: BalanceProbeUnknown, holds: false},
	} {
		generator := &balanceHoldFixedProbeGenerator{outcome: c.outcome}
		window := newBalanceHoldTestWindow(generator, true)
		stats := NewWindowClientStats()
		window.noteContractStatusForBalanceHold(balanceHoldTestStatus(NewId(), &short))

		stepHeld := window.probeBalanceHold(stats)

		if stepHeld != c.holds {
			t.Errorf("%s: the step held=%t, want %t", c.outcome, stepHeld, c.holds)
		}
		if _, held := window.balanceRefillHeld(); held != c.holds {
			t.Errorf("%s: the window held=%t after the step, want %t", c.outcome, held, c.holds)
		}
		window.stateLock.Lock()
		failureCount := window.discoveryRetry.failureCount
		window.stateLock.Unlock()
		if wantFailures := map[bool]int{true: 1, false: 0}[c.holds]; failureCount != wantFailures {
			t.Errorf("%s: the backoff counted %d unproductive steps, want %d", c.outcome, failureCount, wantFailures)
		}
		if generator.probes != 1 {
			t.Errorf("%s: %d probes for one step", c.outcome, generator.probes)
		}
		if got := stats.balanceProbes[windowStatsWindowQuality][c.outcome].Load(); got != 1 {
			t.Errorf("%s: the outcome was counted %d times", c.outcome, got)
		}
		if got := stats.expandPasses[windowStatsWindowQuality][windowExpandPassHeldInsufficientBalance].Load(); got != 1 {
			t.Errorf("%s: %d held passes counted", c.outcome, got)
		}
	}

	// a window that is not held probes nothing and builds as before
	generator := &balanceHoldFixedProbeGenerator{outcome: BalanceProbeShort}
	window := newBalanceHoldTestWindow(generator, true)
	if window.probeBalanceHold(NewWindowClientStats()) || generator.probes != 0 {
		t.Fatal("a window that was never refused probed or held")
	}
}
