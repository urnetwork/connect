// Strict windows plan with the admission gate's inputs and hand a warned
// client's slot to its replacement in one swap (CPUOPT3 D2). The composed
// tests run a real multi client over in-process providers in a synctest
// bubble: a strict 5/5/5 quality window, one spare provider, and the process
// counters (newWindowStatsTestHarness).
package connect

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The installed and selectable (live, unwarned) client counts, read under the
// window lock as selection reads them.
func strictHandoverTestCounts(window *multiClientWindow) (installedCount int, selectableCount int) {
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	for _, client := range window.clients {
		if !client.IsDone() && !client.isWarning() {
			selectableCount += 1
		}
	}
	return len(window.clients), selectableCount
}

// Opens one TCP connection through the quality window and returns its flow
// and the client it is bound to. The flow then sits idle.
func bindStrictHandoverTestFlow(t *testing.T, h *reraceHarness, index int) (*IpPath, *multiClientChannel) {
	t.Helper()
	path := unboundFlowTcpPath(52950 + index)
	path.DestinationIp = net.IPv4(203, 0, 113, byte(90+index)).To4()
	if !h.syn(path) {
		t.Fatal("the window refused the flow's SYN")
	}
	owner := h.boundTo(path)
	if _, ok := h.synAck(path); !ok || owner == nil {
		t.Fatal("the flow did not connect through the window")
	}
	return path, owner
}

func strictHandoverTestInstalled(window *multiClientWindow, client *multiClientChannel) bool {
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	return window.clients[client.ClientId()] == client
}

func strictHandoverTestRetired(generator *discoveryDemandTestGenerator, clientId Id) int {
	generator.mutex.Lock()
	defer generator.mutex.Unlock()
	return generator.retired[clientId]
}

// A drained, flowless client in a strict window at its hard max hands its
// slot over: exactly one candidate is minted and built, it is admitted by
// the swap, the drained client is retired, and the window holds five clients
// at every point. Then the window is full again and builds nothing more.
// Before the handover, every pass built a candidate the hard max declined and
// the drained client kept its slot.
func TestStrictWindowLifetimeDrainHandsOverSlot(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		_, _, mintsBefore, _ := generator.counts()
		clientsBefore := len(generator.clientList())

		var pointsLock sync.Mutex
		points := []string{}
		recordPoint := func(point string) {
			installedCount, selectableCount := strictHandoverTestCounts(window)
			pointsLock.Lock()
			defer pointsLock.Unlock()
			points = append(points, fmt.Sprintf("%s installed=%d selectable=%d", point, installedCount, selectableCount))
		}
		generator.mutex.Lock()
		generator.configureNewClient = func(*MultiClientGeneratorClientArgs, *ClientSettings) {
			recordPoint("construction")
		}
		generator.mutex.Unlock()
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = func() {
				recordPoint("swap")
			}
		}()

		drained := h.liveChannels()[0]
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(20 * time.Second)
		recordPoint("after")

		_, _, mints, _ := generator.counts()
		AssertEqual(t, mints-mintsBefore, 1)
		AssertEqual(t, len(generator.clientList())-clientsBefore, 1)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
		AssertEqual(t, windowStatsCandidates(stats, windowExpandPassStrictHole, windowCandidateAdmittedHandover), uint64(1))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateDeclinedStrictCap), uint64(0))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(1))
		if strictHandoverTestInstalled(window, drained) || !drained.IsDone() {
			t.Fatal("the drained client kept its slot")
		}
		AssertEqual(t, strictHandoverTestRetired(generator, drained.ClientId()), 1)
		pointsLock.Lock()
		AssertEqual(t, points, []string{
			"construction installed=5 selectable=4",
			"swap installed=5 selectable=4",
			"after installed=5 selectable=5",
		})
		pointsLock.Unlock()
		AssertEqual(t, windowStatsWindows(stats, windowStateSelectableFull), int64(1))
		AssertEqual(t, windowStatsWindows(stats, windowStateStrictHole), int64(0))

		// full again: nothing more is listed, minted or built
		readsSteady, _, _, _ := generator.counts()
		h.sleep(3 * time.Minute)
		reads, _, mintsSteady, _ := generator.counts()
		AssertEqual(t, reads, readsSteady)
		AssertEqual(t, mintsSteady, mints)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
	})
}

// A selection snapshot taken at a barrier inside admission, just before the
// swap, sees the hard max installed and every client but the warned one
// selectable, and the monitor still reports the minimum met; after the swap
// all five are selectable. The swap never removes the warned client before
// its replacement is evaluated and installed.
func TestStrictWindowNeverBelowMinimumDuringHandover(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)

		var snapshotLock sync.Mutex
		var installedAtSwap, offeredAtSwap int
		var minSatisfiedAtSwap bool
		swaps := 0
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = func() {
				installedCount, _ := strictHandoverTestCounts(window)
				// selection's offer without the rank gate counts every
				// selectable client
				offered := window.orderedClientsCrossTierForIpVersion(0)
				minSatisfied := h.multiClient.Monitor().WindowExpandEvent().MinSatisfied
				snapshotLock.Lock()
				defer snapshotLock.Unlock()
				swaps += 1
				installedAtSwap, offeredAtSwap, minSatisfiedAtSwap = installedCount, len(offered), minSatisfied
			}
		}()

		drained := h.liveChannels()[0]
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(20 * time.Second)

		snapshotLock.Lock()
		AssertEqual(t, swaps, 1)
		AssertEqual(t, installedAtSwap, 5)
		if offeredAtSwap < 4 {
			t.Fatalf("selection offered %d clients during the handover, want at least 4", offeredAtSwap)
		}
		AssertEqual(t, minSatisfiedAtSwap, true)
		snapshotLock.Unlock()
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
		AssertEqual(t, h.multiClient.Monitor().WindowExpandEvent().MinSatisfied, true)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
	})
}

// A drained client that still carries a flow cannot hand over before the
// collapse deadline, so a strict window at its hard max lists, mints and
// builds nothing at all, and the flow keeps being served by the drained
// client. Before the pre-check, every pass minted and built a candidate the
// hard max declined.
func TestStrictWindowFullDoesNotMint(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		path, drained := bindStrictHandoverTestFlow(t, h, 0)
		h.sleep(time.Second)
		readsBefore, _, mintsBefore, _ := generator.counts()
		clientsBefore := len(generator.clientList())

		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(3 * time.Minute)

		reads, _, mints, _ := generator.counts()
		AssertEqual(t, reads, readsBefore)
		AssertEqual(t, mints, mintsBefore)
		AssertEqual(t, len(generator.clientList()), clientsBefore)
		if skipped := stats.expandPasses[windowStatsWindowQuality][windowExpandPassSkippedStrictFull].Load(); skipped < 2 {
			t.Fatalf("%d passes were counted as skipped at the hard max, want every pass", skipped)
		}
		AssertEqual(t, stats.expandPasses[windowStatsWindowQuality][windowExpandPassStrictHole].Load(), uint64(0))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateDeclinedStrictCap), uint64(0))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		AssertEqual(t, windowStatsWindows(stats, windowStateStrictHole), int64(1))
		if !strictHandoverTestInstalled(window, drained) || drained.windowWarningCause() != windowWarningDrain {
			t.Fatal("the drained client carrying a flow left its slot or stopped draining")
		}

		// the flow is still served, by the drained client
		deliveredBefore := len(h.delivered(path))
		if !h.send(reraceSegment(path, 0, []byte("still served"))) {
			t.Fatal("the drained client refused its own flow's data")
		}
		if len(h.delivered(path)) <= deliveredBefore || h.boundTo(path) != drained {
			t.Fatal("the drained client stopped serving its flow")
		}
	})
}

// A drained client hands over only once its last flow has ended and the
// stats window has emptied: no handover while the flow is live, the client is
// flowless at the swap, and exactly one handover follows.
func TestStrictWindowDrainedFlowsHandOverAfterFlowsEnd(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t, func(settings *MultiClientSettings) {
			settings.TcpSequenceIdleTimeout = 2 * time.Minute
		})
		window := h.multiClient.windows[WindowTypeQuality]
		_, drained := bindStrictHandoverTestFlow(t, h, 1)
		var flowsAtSwap atomic.Int64
		flowsAtSwap.Store(-1)
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = func() {
				flowsAtSwap.Store(int64(h.multiClient.clientFlowCount(drained)))
			}
		}()
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)

		h.sleep(90 * time.Second)
		AssertEqual(t, h.multiClient.clientFlowCount(drained), 1)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		if !strictHandoverTestInstalled(window, drained) {
			t.Fatal("the drained client left its slot while carrying a flow")
		}

		deadline := time.Now().Add(5 * time.Minute)
		for 0 < h.multiClient.clientFlowCount(drained) {
			if windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover) != 0 {
				t.Fatal("the drained client handed over while carrying a flow")
			}
			if deadline.Before(time.Now()) {
				t.Fatal("the idle flow never ended")
			}
			h.sleep(5 * time.Second)
		}
		h.sleep(time.Minute)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
		AssertEqual(t, flowsAtSwap.Load(), int64(0))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateDeclinedStrictCap), uint64(0))
		if strictHandoverTestInstalled(window, drained) {
			t.Fatal("the drained client kept its slot after its flow ended")
		}
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
	})
}

// Past the collapse deadline of two lifetimes, a drained client hands over
// even with a flow, which ends with it, as non-strict capacity collapse cuts
// it (owner answer to D2 question 1).
func TestStrictWindowCollapseDeadlineEscape(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		path, drained := bindStrictHandoverTestFlow(t, h, 2)
		// the flow goes idle, so the stats window empties
		h.sleep(31 * time.Second)
		lifetime := window.settings.MaxClientLifetime
		ageWindowClientForTest(drained, lifetime, time.Duration(collapseDeadlineLifetimes)*lifetime+time.Minute)
		h.sleep(20 * time.Second)

		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
		if strictHandoverTestInstalled(window, drained) || !drained.IsDone() {
			t.Fatal("a drained client past the collapse deadline kept its slot")
		}
		if h.boundTo(path) == drained {
			t.Fatal("the flow is still bound to the removed client")
		}
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
	})
}

// Inside the collapse deadline the same flow keeps the drained client in its
// slot: the deadline, not the drain, cuts a flow.
func TestStrictWindowCollapseDeadlineNotReachedKeepsFlow(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		path, drained := bindStrictHandoverTestFlow(t, h, 3)
		h.sleep(31 * time.Second)
		lifetime := window.settings.MaxClientLifetime
		ageWindowClientForTest(drained, lifetime, time.Duration(collapseDeadlineLifetimes)*lifetime-time.Minute)
		h.sleep(20 * time.Second)

		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		if !strictHandoverTestInstalled(window, drained) || h.boundTo(path) != drained {
			t.Fatal("a flow was cut before the collapse deadline")
		}
	})
}

// A slot that frees while a strict window sits full refills at once: the
// dead client's end wakes resize, and the next pass admits a candidate
// without waiting out the resize interval.
func TestStrictWindowFreedSlotRefillsImmediately(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		_, drained := bindStrictHandoverTestFlow(t, h, 4)
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(20 * time.Second)
		if stats.expandPasses[windowStatsWindowQuality][windowExpandPassSkippedStrictFull].Load() < 1 {
			t.Fatal("the full window did not skip its passes")
		}
		admittedBefore := windowStatsCandidateTotal(stats, windowCandidateAdmitted)

		var dead *multiClientChannel
		for _, client := range h.liveChannels() {
			if client != drained {
				dead = client
				break
			}
		}
		// a verdict ends the client, the way detectBlackhole does
		dead.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
		dead.Cancel()
		deadAt := time.Now()
		for windowStatsCandidateTotal(stats, windowCandidateAdmitted) == admittedBefore {
			if window.settings.WindowResizeTimeout <= time.Since(deadAt) {
				t.Fatalf("the freed slot waited %s for the resize interval", time.Since(deadAt))
			}
			h.sleep(100 * time.Millisecond)
		}
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalVerdict].Load(), uint64(1))
		if !strictHandoverTestInstalled(window, drained) {
			t.Fatal("the refill took the drained client's slot")
		}
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
	})
}

// A non-strict window is planned exactly as before whether or not the
// handover is on: a drained client's replacement is admitted over the hard
// max and the capacity collapse then removes the drained client. The two runs
// produce the same counters.
func TestNonStrictWindowTraceUnchanged(t *testing.T) {
	trace := func(strictWindowHandover bool) map[string]uint64 {
		counts := map[string]uint64{}
		synctest.Test(t, func(t *testing.T) {
			h, _, stats := newWindowStatsTestHarness(t, func(settings *MultiClientSettings) {
				settings.StrictWindowSizeHardMax = false
				settings.StrictWindowHandover = strictWindowHandover
			})
			window := h.multiClient.windows[WindowTypeQuality]
			h.sleep(time.Second)
			drained := h.liveChannels()[0]
			ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
			h.sleep(2 * time.Minute)
			if strictHandoverTestInstalled(window, drained) {
				t.Fatal("the non-strict collapse kept the drained client")
			}
			installedCount, selectableCount := strictHandoverTestCounts(window)
			AssertEqual(t, installedCount, 5)
			AssertEqual(t, selectableCount, 5)
			stats.VisitCandidates(func(window string, state string, outcome string, count uint64) {
				if window == "quality" && count != 0 {
					counts["candidate/"+state+"/"+outcome] = count
				}
			})
			stats.VisitRemovals(func(window string, cause string, count uint64) {
				if window == "quality" && count != 0 {
					counts["removal/"+cause] = count
				}
			})
			stats.VisitExpandPasses(func(window string, state string, count uint64) {
				if window == "quality" && count != 0 && state != windowExpandPassDeferred.String() {
					counts["pass/"+state] = count
				}
			})
		})
		return counts
	}
	GetMessagePoolAggregateStats()
	on := trace(true)
	off := trace(false)
	AssertEqual(t, on, off)
	AssertEqual(t, on, map[string]uint64{
		"candidate/below_min/admitted": 6,
		"removal/collapse":             1,
		"pass/below_min":               3,
	})
}

// The pre-check's capacity is positive exactly when the admission gate admits
// a new identity, or a live reservation still stands for a candidate; a
// same-identity replacement is admitted at the ceiling and needs no capacity.
func TestStrictAdmissionPrecheckEquivalence(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, hardMax := range []int{0, 1, 3, 5} {
			for installedCount := 0; installedCount <= hardMax+1; installedCount += 1 {
				for _, reservation := range []string{"none", "live", "spent", "victim-gone"} {
					// a pass reserves a slot only at exactly the hard max
					if reservation != "none" && (!strict || installedCount != hardMax || hardMax == 0) {
						continue
					}
					settings := DefaultMultiClientSettings()
					settings.StrictWindowSizeHardMax = strict
					window := &multiClientWindow{settings: settings, clients: map[Id]*multiClientChannel{}}
					installedIds := []Id{}
					for range installedCount {
						id := NewId()
						installedIds = append(installedIds, id)
						window.clients[id] = &multiClientChannel{
							args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: id}},
						}
					}
					reservationStands := false
					switch reservation {
					case "live", "spent", "victim-gone":
						victim := window.clients[installedIds[0]]
						remaining := 1
						if reservation == "spent" {
							remaining = 0
						}
						window.strictHandover = &strictWindowHandover{victim: victim, remainingCandidates: remaining}
						if reservation == "victim-gone" {
							delete(window.clients, installedIds[0])
						}
						reservationStands = reservation == "live"
					}
					windowSize := WindowSizeSettings{WindowSizeHardMax: hardMax}
					capacity := window.strictAdmissionCapacity(hardMax)
					gateAdmitsNew := window.strictWindowAdmissionAllowed(NewId(), windowSize)
					if want := gateAdmitsNew || (strict && 0 < hardMax && reservationStands); (0 < capacity) != want {
						t.Errorf("strict=%t hardMax=%d installed=%d reservation=%s: capacity=%d, gate admits a new identity=%t",
							strict, hardMax, installedCount, reservation, capacity, gateAdmitsNew)
					}
					if reservation == "none" && 0 < len(installedIds) && !window.strictWindowAdmissionAllowed(installedIds[0], windowSize) {
						t.Errorf("strict=%t hardMax=%d installed=%d: the gate refused a same-identity replacement", strict, hardMax, installedCount)
					}
					// without a reservation the capacity is the gate's own count
					if reservation == "none" && strict && 0 < hardMax {
						AssertEqual(t, capacity, hardMax-installedCount)
					}
				}
			}
		}
	}
}

// Which warned clients may hand over their slot (owner answer to D2 question
// 2: every backfill cause), and that the choice follows the capacity collapse
// test and the collapse's order.
func TestStrictHandoverVictimSelection(t *testing.T) {
	for cause, want := range map[windowWarningCause]bool{
		windowWarningDrain:      true,
		windowWarningSilent:     true,
		windowWarningQuarantine: true,
		windowWarningStarved:    true,
		windowWarningUnhealthy:  true,
		windowWarningCapacity:   false,
		windowWarningFamilySwap: false,
		windowWarningNone:       false,
	} {
		AssertEqual(t, strictHandoverCauseEligible(cause), want)
	}

	settings := DefaultMultiClientSettings()
	flows := map[*multiClientChannel]int{}
	window := &multiClientWindow{
		settings: settings,
		flowCountFunc: func(client *multiClientChannel) int {
			return flows[client]
		},
	}
	warned := func(cause warnCause) *multiClientChannel {
		client := stallTestChannel()
		client.setWarning(true, cause)
		return client
	}
	young := warned(warnDraining)
	busy := warned(warnDraining)
	carrying := warned(warnDraining)
	full := warned(warnCapacity)
	swapping := warned(warnFamilySwap)
	shielded := stallTestChannel()
	shielded.setWarning(false, warnUnhealthy)
	older := warned(warnSilent)
	oldest := warned(warnDraining)
	benched := stallTestChannel()
	benched.stateLock.Lock()
	benched.quarantined = true
	benched.stateLock.Unlock()
	flows[carrying] = 1
	weights := map[*multiClientChannel]float32{busy: 100}
	grace := settings.StatsWindowGraceperiod
	durations := map[*multiClientChannel]time.Duration{
		young:    grace - time.Second,
		busy:     time.Hour,
		carrying: time.Hour,
		full:     time.Hour,
		swapping: time.Hour,
		shielded: time.Hour,
		older:    90 * time.Minute,
		oldest:   100 * time.Minute,
		benched:  95 * time.Minute,
	}
	candidates := []*multiClientChannel{young, busy, carrying, full, swapping, shielded, oldest, older, benched}

	// the collapse sheds its tail first: the shortest-lived eligible client
	choose := func(clients []*multiClientChannel) *multiClientChannel {
		handover := window.selectStrictHandoverVictim(clients, weights, durations)
		if handover == nil {
			return nil
		}
		AssertEqual(t, handover.victimDuration, durations[handover.victim])
		return handover.victim
	}
	AssertEqual(t, choose(candidates), older)
	candidates = slices.DeleteFunc(candidates, func(client *multiClientChannel) bool { return client == older })
	AssertEqual(t, choose(candidates), benched)
	candidates = slices.DeleteFunc(candidates, func(client *multiClientChannel) bool { return client == benched })
	AssertEqual(t, choose(candidates), oldest)
	candidates = slices.DeleteFunc(candidates, func(client *multiClientChannel) bool { return client == oldest })
	// within the grace, carrying weight, carrying flows, a capacity or
	// family-swap warning, and a client new flows may still choose: none
	if victim := choose(candidates); victim != nil {
		t.Fatalf("an ineligible client was chosen to hand over: %p", victim)
	}
	// past the collapse deadline a flow no longer protects its slot
	durations[carrying] = time.Duration(collapseDeadlineLifetimes) * settings.MaxClientLifetime
	AssertEqual(t, choose(candidates), carrying)
}

// The enumerator of a strict window at its hard max lists and mints nothing,
// even when an expansion asks; a pass's handover reservation lets it list
// once and mint exactly the candidates the reservation stands for, and none
// ahead.
func TestEnumeratorSkipsListingWhenStrictFull(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		generator := &discoveryDemandTestBudgetGenerator{
			results: map[MultiHopId]DestinationStats{
				RequireMultiHopId(NewId()): {},
				RequireMultiHopId(NewId()): {},
			},
			retireById:  map[Id]int{},
			listEntered: make(chan struct{}),
			listRelease: make(chan struct{}),
			mintEntered: make(chan struct{}),
			mintRelease: make(chan struct{}),
		}
		window := discoveryRetryTestWindow(t)
		window.ctx, window.cancel, window.generator = ctx, cancel, generator
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		window.settings.StrictWindowSizeHardMax = true
		window.settings.StrictWindowHandover = true
		window.settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1}
		victimId := NewId()
		victim := &multiClientChannel{ctx: ctx, settings: window.settings, warning: true, warnCause: warnDraining,
			args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: victimId}, Destination: RequireMultiHopId(NewId())}}
		window.clients[victimId] = victim

		done := make(chan struct{})
		go func() {
			defer close(done)
			window.randomEnumerateClientArgs()
		}()
		synctest.Wait()
		counts := func() (reads int, mints int) {
			generator.mutex.Lock()
			defer generator.mutex.Unlock()
			return generator.reads, generator.mints
		}
		reads, mints := counts()
		AssertEqual(t, [2]int{reads, mints}, [2]int{0, 0})
		// an expansion asks while the window is full and nothing is reserved
		window.generatorMonitor.NotifyAll()
		synctest.Wait()
		reads, mints = counts()
		AssertEqual(t, [2]int{reads, mints}, [2]int{0, 0})

		// a handover pass reserves the warned client's slot for one candidate
		handover := &strictWindowHandover{victim: victim}
		window.reserveStrictHandover(handover, &multiClientEvaluationOwner{}, 1)
		window.generatorMonitor.NotifyAll()
		synctest.Wait()
		reads, mints = counts()
		AssertEqual(t, [2]int{reads, mints}, [2]int{1, 1})
		offered := <-window.clientChannelArgs
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		reads, mints = counts()
		AssertEqual(t, [2]int{reads, mints}, [2]int{1, 1})

		window.releaseStrictHandover(handover)
		window.generator.RemoveClientArgs(&offered.MultiClientGeneratorClientArgs)
		cancel()
		<-done
	})
}

// Two drained clients hand over one slot per pass: the window never holds
// fewer than its hard max at any construction or swap, collapse removes
// nothing, and both drained clients are replaced and retired.
func TestStrictWindowHandsOverOneSlotPerPass(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		h.addExit(reraceExitSpec{name: "spare-b"})
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)

		var pointsLock sync.Mutex
		minInstalled := 5
		recordPoint := func() {
			installedCount, _ := strictHandoverTestCounts(window)
			pointsLock.Lock()
			defer pointsLock.Unlock()
			minInstalled = min(minInstalled, installedCount)
		}
		generator.mutex.Lock()
		generator.configureNewClient = func(*MultiClientGeneratorClientArgs, *ClientSettings) {
			recordPoint()
		}
		generator.mutex.Unlock()
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = recordPoint
		}()

		live := h.liveChannels()
		first, second := live[0], live[1]
		ageWindowClientForTest(first, time.Minute, 2*time.Minute)
		ageWindowClientForTest(second, time.Minute, 3*time.Minute)
		h.sleep(time.Minute)

		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(2))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(2))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalCollapse].Load(), uint64(0))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateDeclinedStrictCap), uint64(0))
		pointsLock.Lock()
		AssertEqual(t, minInstalled, 5)
		pointsLock.Unlock()
		for _, drained := range []*multiClientChannel{first, second} {
			if strictHandoverTestInstalled(window, drained) {
				t.Fatal("a drained client kept its slot")
			}
			AssertEqual(t, strictHandoverTestRetired(generator, drained.ClientId()), 1)
		}
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
	})
}

// A pass holds its handover reservation for its whole life, for exactly the
// candidates it requests, and releases it when it returns, whatever its
// candidates did.
func TestStrictHandoverReservationEndsWithItsPass(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	liveCtx, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	victimId := NewId()
	victim := &multiClientChannel{
		ctx:     liveCtx,
		warning: true,
		args:    &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: victimId}},
	}
	fixture.window.clients[victimId] = victim
	expirePing := make(chan struct{})
	fixture.window.expireExpandPingForTest = expirePing
	handover := &strictWindowHandover{victim: victim}

	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassStrictHole, handover: handover})
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)
	func() {
		fixture.window.stateLock.Lock()
		defer fixture.window.stateLock.Unlock()
		if fixture.window.strictHandover != handover || handover.owner == nil {
			t.Fatal("the pass does not hold its reservation")
		}
		// it stands for the one candidate the pass requests; only the
		// enumerator spends it, and this fixture offered its candidate itself
		AssertEqual(t, handover.remainingCandidates, 1)
	}()
	fixture.send(t, "initial-ping timeout", expirePing)
	if got := fixture.result(t, expandDone); got != 0 {
		t.Fatalf("timed-out ping admissions=%d, want 0", got)
	}
	func() {
		fixture.window.stateLock.Lock()
		defer fixture.window.stateLock.Unlock()
		if fixture.window.strictHandover != nil {
			t.Fatal("the reservation outlived its pass")
		}
	}()
	if handover.completed.Load() || fixture.window.clients[victimId] != victim {
		t.Fatal("a failed candidate swapped the reserved client out")
	}
	fixture.releasePing()
	fixture.wait(t, "delayed initial-ping callback", fixture.pingResultDone)
	fixture.wait(t, "timed-out client cleanup", fixture.clientRemoved)
	assertOnlyCandidateOutcome(t, stats, windowExpandPassStrictHole, windowCandidateFailedPing)
}

// A warned client that recovers between the reservation and the swap keeps
// its slot: the swap checks the client is still out of selection in the
// section that would remove it, and the evaluated candidate is declined.
func TestStrictHandoverDeclinedWhenTheVictimRecovers(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		drained := h.liveChannels()[0]
		var recovered atomic.Bool
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = func() {
				if recovered.CompareAndSwap(false, true) {
					drained.setWarning(false, warnNone)
				}
			}
		}()
		_, _, mintsBefore, _ := generator.counts()
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		// the pass that reserved the slot ends with the decline; the expansion
		// backoff then holds the next pass, which would drain the client again
		deadline := time.Now().Add(30 * time.Second)
		for windowStatsCandidates(stats, windowExpandPassStrictHole, windowCandidateDeclinedStrictCap) == 0 {
			if deadline.Before(time.Now()) {
				t.Fatal("the handover pass never ended")
			}
			h.sleep(100 * time.Millisecond)
		}

		AssertEqual(t, recovered.Load(), true)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		AssertEqual(t, windowStatsCandidates(stats, windowExpandPassStrictHole, windowCandidateDeclinedStrictCap), uint64(1))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(0))
		if !strictHandoverTestInstalled(window, drained) || drained.IsDone() {
			t.Fatal("a recovered client was swapped out")
		}
		_, _, mints, _ := generator.counts()
		AssertEqual(t, mints-mintsBefore, 1)
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
	})
}

// Above its hard max (here after the window's limit shrinks) a strict window
// builds nothing and the capacity collapse sheds a drained client; only once
// it is back at the hard max does the next pass hand the other drained
// client's slot over. No pass is planned for a candidate admission could
// not take.
func TestStrictWindowAboveHardMaxCollapsesWithoutBuilding(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		live := h.liveChannels()
		ageWindowClientForTest(live[0], time.Minute, 2*time.Minute)
		ageWindowClientForTest(live[1], time.Minute, 3*time.Minute)
		h.multiClient.SetWindowSizeLimit(4)
		h.sleep(time.Minute)

		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalCollapse].Load(), uint64(1))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(1))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
		AssertEqual(t, stats.expandPasses[windowStatsWindowQuality][windowExpandPassStrictHole].Load(), uint64(1))
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 4)
		AssertEqual(t, selectableCount, 4)
	})
}

// The handover is a host's opt-in: the defaults leave it off, so a strict
// window built from them (every app's low-memory profile, and a c abi host)
// plans exactly as before until its host turns it on.
func TestStrictWindowHandoverIsOffByDefault(t *testing.T) {
	settings := DefaultMultiClientSettings()
	AssertEqual(t, settings.StrictWindowHandover, false)
	settings.StrictWindowSizeHardMax = true
	window := &multiClientWindow{settings: settings}
	AssertEqual(t, window.strictWindowHandoverEnabled(), false)
	settings.StrictWindowHandover = true
	AssertEqual(t, window.strictWindowHandoverEnabled(), true)
}
