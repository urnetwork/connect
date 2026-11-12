// Strict windows plan with the admission gate's inputs and hand a warned
// client's slot to its replacement in one swap (CPUOPT3 D2). The composed
// tests run a real multi client over in-process providers in a synctest
// bubble: a strict 5/5/5 quality window, one spare provider, and the process
// counters (newWindowStatsTestHarness).
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
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
		// one per rotation of every hosted window: verbose only
		AssertEqual(t, len(h.log.linesWith("event=handover")), 0)

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
				offered := window.orderedClientsCrossTierForFlow(flowNeed{ipVersion: 0})
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
				for _, reservation := range []string{"none", "pass-only", "reserved", "victim-gone"} {
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
					case "pass-only":
						// a pass without a handover adds no capacity
						window.strictPass = &strictWindowPass{remainingCandidates: 1}
					case "reserved", "victim-gone":
						victim := window.clients[installedIds[0]]
						window.strictPass = &strictWindowPass{handover: &strictWindowHandover{victim: victim}}
						if reservation == "victim-gone" {
							delete(window.clients, installedIds[0])
						}
						reservationStands = reservation == "reserved"
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
		// an expansion asks while the window is full and no pass is running
		window.generatorMonitor.NotifyAll()
		synctest.Wait()
		reads, mints = counts()
		AssertEqual(t, [2]int{reads, mints}, [2]int{0, 0})
		// a pass that reserved nothing finds the window full too
		fullPass := window.beginStrictPass(&multiClientEvaluationOwner{}, 1, nil)
		window.generatorMonitor.NotifyAll()
		synctest.Wait()
		reads, mints = counts()
		AssertEqual(t, [2]int{reads, mints}, [2]int{0, 0})
		window.endStrictPass(fullPass)

		// a handover pass reserves the warned client's slot for one candidate
		handover := &strictWindowHandover{victim: victim}
		handoverPass := window.beginStrictPass(&multiClientEvaluationOwner{}, 1, handover)
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

		window.endStrictPass(handoverPass)
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
		strictPass := fixture.window.strictPass
		if strictPass == nil || strictPass.handover != handover || strictPass.owner == nil {
			t.Fatal("the pass does not hold its reservation")
		}
		// it stands for the one candidate the pass requests; only the
		// enumerator spends it, and this fixture offered its candidate itself
		AssertEqual(t, strictPass.remainingCandidates, 1)
	}()
	fixture.send(t, "initial-ping timeout", expirePing)
	if got := fixture.result(t, expandDone); got != 0 {
		t.Fatalf("timed-out ping admissions=%d, want 0", got)
	}
	func() {
		fixture.window.stateLock.Lock()
		defer fixture.window.stateLock.Unlock()
		if fixture.window.strictPass != nil {
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

// A strict window refilling one freed slot mints one candidate: the
// enumerator hands a pass only the candidates it requested, so nothing is
// minted ahead into a window its pass is already filling, to wait out
// WindowExpandArgsTimeout and be retired unused. The review's A2.
func TestStrictWindowRefillMintsOnlyWhatThePassTakes(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		h.addExit(reraceExitSpec{name: "spare-b"})
		h.addExit(reraceExitSpec{name: "spare-c"})
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		_, _, mintsBefore, _ := generator.counts()
		admittedBefore := windowStatsCandidateTotal(stats, windowCandidateAdmitted)

		dead := h.liveChannels()[0]
		dead.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
		dead.Cancel()
		h.sleep(5 * time.Minute)

		_, _, mints, _ := generator.counts()
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmitted)-admittedBefore, uint64(1))
		AssertEqual(t, mints-mintsBefore, 1)
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
	})
}

// A strict window's enumerator lists and mints only while admission has room,
// and no more than a running pass still wants; between passes it may mint the
// candidate the next pass takes. Every other window's enumerator is not held.
func TestStrictDiscoveryAdmissibleNeedsRoomAndThePassBudget(t *testing.T) {
	newWindow := func(strictWindowHandover bool, fixed bool, installedCount int) (*multiClientWindow, []*multiClientChannel) {
		window := discoveryRetryTestWindow(t)
		window.generator = &discoveryDemandTestBudgetGenerator{fixed: fixed}
		window.settings.StrictWindowSizeHardMax = true
		window.settings.StrictWindowHandover = strictWindowHandover
		window.settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2}
		clients := []*multiClientChannel{}
		for range installedCount {
			id := NewId()
			client := &multiClientChannel{args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: id}}}
			window.clients[id] = client
			clients = append(clients, client)
		}
		return window, clients
	}
	owner := &multiClientEvaluationOwner{}

	// room: between passes, and for a pass until it has its candidates
	window, _ := newWindow(true, false, 1)
	AssertEqual(t, window.strictDiscoveryAdmissible(), true)
	pass := window.beginStrictPass(owner, 1, nil)
	AssertEqual(t, window.strictDiscoveryAdmissible(), true)
	window.noteStrictPassCandidateOffered()
	AssertEqual(t, window.strictDiscoveryAdmissible(), false)
	window.endStrictPass(pass)
	AssertEqual(t, window.strictDiscoveryAdmissible(), true)

	// full: nothing between passes, and a pass wants a candidate only for a
	// reserved slot
	window, clients := newWindow(true, false, 2)
	AssertEqual(t, window.strictDiscoveryAdmissible(), false)
	pass = window.beginStrictPass(owner, 1, nil)
	AssertEqual(t, window.strictDiscoveryAdmissible(), false)
	window.endStrictPass(pass)
	window.beginStrictPass(owner, 1, &strictWindowHandover{victim: clients[0]})
	AssertEqual(t, window.strictDiscoveryAdmissible(), true)

	// the handover off, and a fixed window, are not held
	window, _ = newWindow(false, false, 2)
	AssertEqual(t, window.strictDiscoveryAdmissible(), true)
	window, _ = newWindow(true, true, 2)
	AssertEqual(t, window.strictDiscoveryAdmissible(), true)
}

// A candidate from a discovery demand read fills one of the slots its pass
// asked for, as an ordinary candidate does, so the enumerator lists and mints
// no more for the pass than it requested.
func TestStrictPassBudgetCountsDemandCandidates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window := discoveryRetryTestWindow(t)
		window.generator = &discoveryDemandTestBudgetGenerator{}
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		window.settings.StrictWindowSizeHardMax = true
		window.settings.StrictWindowHandover = true
		window.settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 3, WindowSizeHardMax: 3}
		installedId := NewId()
		window.clients[installedId] = &multiClientChannel{
			args: &multiClientChannelArgs{MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: installedId}},
		}
		owner := &multiClientEvaluationOwner{}
		pass := window.beginStrictPass(owner, 2, nil)
		defer window.endStrictPass(pass)
		window.beginDiscoveryAcquisition(owner, t.Context(), time.Now().Add(time.Minute), 3, 3)
		defer window.endDiscoveryAcquisition(owner)
		window.beginDiscoveryDemandSlot(owner)
		update := newMultiClientChannelUpdate(t.Context(), unboundFlowTcpPath(52996))
		defer update.Close()
		var read *discoveryDemandRead
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			demand := &window.discoveryDemand
			read = &discoveryDemandRead{
				owner:    owner,
				slot:     demand.slot,
				ctx:      demand.slotCtx,
				deadline: demand.deadline,
				update:   update,
				stamp:    update.discoveryAttemptStamp.Load(),
				members:  maps.Clone(window.clients),
			}
		}()
		if !window.discoveryDemandReadLive(read) {
			t.Fatal("the demand read is not live")
		}
		remaining := func() int {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			return pass.remainingCandidates
		}
		// the pass's receiver; it ends with the test
		received := make(chan *multiClientChannelArgs, 2)
		go func() {
			for range 2 {
				select {
				case args := <-window.clientChannelArgs:
					received <- args
				case <-t.Context().Done():
					return
				}
			}
		}()

		if !window.offerPassCandidate(read, &multiClientChannelArgs{}, time.Now().Add(time.Minute)) {
			t.Fatal("the pass did not take the demand candidate")
		}
		<-received
		AssertEqual(t, remaining(), 1)
		AssertEqual(t, window.strictDiscoveryAdmissible(), true)

		if !window.offerPassCandidate(nil, &multiClientChannelArgs{}, time.Now().Add(time.Minute)) {
			t.Fatal("the pass did not take the ordinary candidate")
		}
		<-received
		AssertEqual(t, remaining(), 0)
		AssertEqual(t, window.strictDiscoveryAdmissible(), false)
	})
}

// A victim that ends with an error during its handover pass (here its
// transport is lost while the candidate is evaluated) is removed by its
// error, as the resize pass would have removed it: the transport_down
// removal, THE verdict line, and no handover removal, while the evaluated
// candidate still takes the slot. The review's A1.
func TestStrictHandoverVictimEndedBeforeTheSwapIsRemovedByItsError(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		drained := h.liveChannels()[0]
		var ended atomic.Bool
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = func() {
				if ended.CompareAndSwap(false, true) {
					drained.addError(errTransportDownTimeout)
				}
			}
		}()
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(40 * time.Second)

		if !ended.Load() {
			t.Fatal("no handover swap was attempted")
		}
		removalLines := 0
		for _, line := range h.log.linesWith("event=removal") {
			if strings.Contains(line, drained.ClientId().String()) {
				removalLines += 1
			}
		}
		AssertEqual(t, removalLines, 1)
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalTransportDown].Load(), uint64(1))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(0))
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
		if strictHandoverTestInstalled(window, drained) {
			t.Fatal("the ended victim kept its slot")
		}
		installedCount, selectableCount := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		AssertEqual(t, selectableCount, 5)
	})
}

// The window closes while a handover pass is between evaluation and the
// swap: nothing is installed into the closed window, the candidate is
// declined, and the pass and its reservation end with it. The review's A4.
func TestStrictHandoverWindowClosesBeforeTheSwap(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		drained := h.liveChannels()[0]
		var closed atomic.Bool
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			window.beforeStrictHandoverSwapForTest = func() {
				if closed.CompareAndSwap(false, true) {
					done := make(chan struct{})
					go func() {
						defer close(done)
						h.multiClient.Close()
					}()
					<-done
				}
			}
		}()
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(40 * time.Second)

		if !closed.Load() {
			t.Fatal("no handover swap was attempted")
		}
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			AssertEqual(t, len(window.clients), 0)
			if window.strictPass != nil {
				t.Fatal("the pass and its reservation outlived the closed window")
			}
		}()
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(0))
	})
}

// A victim that gains a flow after the pass chose it (here while the
// candidate is constructed) keeps its slot: admission re-checks the flow half
// of the collapse test and declines the candidate. The flow is recorded in
// the parent's flow bookkeeping, which the window's flow count reads. The
// review's A7.
func TestStrictHandoverVictimGainsAFlowAfterSelection(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		drained := h.liveChannels()[0]
		gained := newMultiClientChannelUpdate(h.ctx, unboundFlowTcpPath(52990))
		var gainedFlow atomic.Bool
		setGained := func(present bool) {
			h.multiClient.stateLock.Lock()
			defer h.multiClient.stateLock.Unlock()
			updates, ok := h.multiClient.clientUpdates[drained]
			if !ok {
				updates = map[*multiClientChannelUpdate]bool{}
				h.multiClient.clientUpdates[drained] = updates
			}
			if present {
				updates[gained] = true
			} else {
				delete(updates, gained)
			}
		}
		func() {
			generator.mutex.Lock()
			defer generator.mutex.Unlock()
			generator.configureNewClient = func(*MultiClientGeneratorClientArgs, *ClientSettings) {
				if gainedFlow.CompareAndSwap(false, true) {
					setGained(true)
				}
			}
		}()
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(30 * time.Second)

		if !gainedFlow.Load() {
			t.Fatal("no candidate was built")
		}
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		AssertEqual(t, windowStatsCandidates(stats, windowExpandPassStrictHole, windowCandidateDeclinedStrictCap), uint64(1))
		if !strictHandoverTestInstalled(window, drained) || drained.IsDone() {
			t.Fatal("a victim that gained a flow after selection was swapped out")
		}
		setGained(false)
		gained.Close()
	})
}

// A handover pass runs no capacity collapse: the swap keeps the window at its
// hard max, so a second drained client that still carries a flow, and sorts
// at the collapse's tail, is not judged again by a collapse that would only
// defer it. Kills the review's mutant M3 (keeping the victim in the pass's
// warned clients), which the collapse order masks when the victim is the
// tail.
func TestStrictWindowHandoverRunsNoCollapse(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		path, carrying := bindStrictHandoverTestFlow(t, h, 5)
		var flowless *multiClientChannel
		for _, client := range h.liveChannels() {
			if client != carrying {
				flowless = client
				break
			}
		}
		// the flow goes idle, so both stats windows empty
		h.sleep(31 * time.Second)
		// the flowless client is the older, so the collapse order (descending
		// duration) puts the carrying client at its tail
		ageWindowClientForTest(flowless, time.Minute, 3*time.Minute)
		ageWindowClientForTest(carrying, time.Minute, 2*time.Minute)
		h.sleep(20 * time.Second)

		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(1))
		if strictHandoverTestInstalled(window, flowless) {
			t.Fatal("the flowless drained client kept its slot")
		}
		if !strictHandoverTestInstalled(window, carrying) || h.boundTo(path) != carrying {
			t.Fatal("the drained client carrying a flow lost its slot or its flow")
		}
		AssertEqual(t, len(h.log.linesWith("event=collapse_defer")), 0)
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalCollapse].Load(), uint64(0))
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
	})
}

// A quality listing that takes `delay` (fake time): a platform that is slow
// but alive, inside WindowGeneratorTimeout.
type strictSlowListingTestGenerator struct {
	*discoveryDemandTestGenerator
	delay atomic.Int64
}

func (self *strictSlowListingTestGenerator) NextDestinationsWithIpFamily(count int, exclude []MultiHopId, rank string, family IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	if delay := time.Duration(self.delay.Load()); 0 < delay && rank == "quality" {
		time.Sleep(delay)
	}
	return self.discoveryDemandTestGenerator.NextDestinationsWithIpFamily(count, exclude, rank, family)
}

// A strict 5/5/5 quality window with the handover on, over five exits and a
// spare, whose generator's quality listings can be slowed.
func newStrictSlowListingTestHarness(t *testing.T) (*reraceHarness, *strictSlowListingTestGenerator, *WindowClientStats) {
	t.Helper()
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)
	ctx, cancel := context.WithCancel(t.Context())
	h := &reraceHarness{t: t, ctx: ctx,
		generator: &reraceGenerator{clientExits: map[Id]*reraceExit{}, unsubs: map[Id]func(){}},
		log:       newRecordingLogger(), source: SourceId(NewId()), synTimes: map[string][]time.Time{}}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		h.addExit(reraceExitSpec{name: name})
	}
	inner := &discoveryDemandTestGenerator{reraceGenerator: h.generator, retired: map[Id]int{},
		emptyEntered: make(chan struct{}), emptyRelease: make(chan struct{}),
		availableEntered: make(chan struct{}), availableRelease: make(chan struct{})}
	generator := &strictSlowListingTestGenerator{discoveryDemandTestGenerator: inner}
	stats := NewWindowClientStats()
	settings := DefaultMultiClientSettings()
	settings.Log, settings.ProviderProbe, settings.HeartbeatInterval = h.log, false, 0
	settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{WindowSizeMin: 5, WindowSizeMax: 5, WindowSizeHardMax: 5}
	settings.StrictWindowSizeHardMax, settings.StandingReserve, settings.EvaluationPoolMultiple = true, false, 1
	settings.WindowClientStats = stats
	settings.StrictWindowHandover = true
	h.multiClient = NewRemoteUserNatMultiClient(ctx, generator, h.receive, protocol.ProvideMode_Network, settings)
	t.Cleanup(func() {
		if t.Failed() {
			lines := h.log.lines()
			for _, line := range lines[max(0, len(lines)-80):] {
				t.Log(line)
			}
		}
		for _, release := range []chan struct{}{inner.emptyRelease, inner.availableRelease} {
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
	h.waitForExits(5)
	h.start = time.Now()
	synctest.Wait()
	h.addExit(reraceExitSpec{name: "spare"})
	return h, generator, stats
}

// A strict window refills a lost exit while every quality listing takes
// 17 s: longer than a pass's 15 s acquisition budget (WindowExpandTimeout),
// inside the 20 s WindowGeneratorTimeout, as on a slow platform or after an
// error backoff. The listing returns after its pass ended, so its candidate is
// minted and parked for the next pass; a window that mints only for a running
// pass discards it, and every later pass repeats the race. The review's N1.
func TestStrictWindowRefillsWhenListingsOutlastThePass(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newStrictSlowListingTestHarness(t)
		h.addExit(reraceExitSpec{name: "spare-b"})
		h.sleep(time.Second)
		readsBefore, _, mintsBefore, _ := generator.counts()
		admittedBefore := windowStatsCandidateTotal(stats, windowCandidateAdmitted)
		generator.delay.Store(int64(17 * time.Second))
		dead := h.liveChannels()[0]
		dead.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
		dead.Cancel()
		h.sleep(10 * time.Minute)

		reads, _, mints, _ := generator.counts()
		installedCount, selectableCount := strictHandoverTestCounts(h.multiClient.windows[WindowTypeQuality])
		t.Logf("slow listings, 10 min after one loss: listings=%d mints=%d installed=%d selectable=%d",
			reads-readsBefore, mints-mintsBefore, installedCount, selectableCount)
		if selectableCount != 5 {
			t.Fatalf("the window did not refill its lost exit: %d selectable after 10 minutes, %d listings, %d mints",
				selectableCount, reads-readsBefore, mints-mintsBefore)
		}
		// the freed slot cost one mint, and nothing was minted into the
		// refilled window
		AssertEqual(t, mints-mintsBefore, 1)
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmitted)-admittedBefore, uint64(1))
	})
}

// A victim that ends with a blackhole verdict before its swap is removed by
// its verdict, and the storm breaker counts the removal in its window whether
// or not the budget has room: the victim leaves either way, since its
// evaluated replacement already holds the slot. The review's N2.
func TestStrictHandoverVerdictVictimIsCountedByTheStormBreaker(t *testing.T) {
	for _, spent := range []bool{false, true} {
		t.Run(fmt.Sprintf("spent=%t", spent), func(t *testing.T) {
			GetMessagePoolAggregateStats()
			synctest.Test(t, func(t *testing.T) {
				h, _, stats := newWindowStatsTestHarness(t)
				window := h.multiClient.windows[WindowTypeQuality]
				h.sleep(time.Second)
				drained := h.liveChannels()[0]
				var before int
				var fired atomic.Bool
				func() {
					window.stateLock.Lock()
					defer window.stateLock.Unlock()
					window.beforeStrictHandoverSwapForTest = func() {
						if !fired.CompareAndSwap(false, true) {
							return
						}
						func() {
							window.stateLock.Lock()
							defer window.stateLock.Unlock()
							if spent {
								now := time.Now()
								budget := window.reliabilitySettings().RemovalBudgetCount
								window.verdictRemovalTimes = slices.Repeat([]time.Time{now}, budget)
							}
							before = len(window.verdictRemovalTimes)
						}()
						drained.addError(fmt.Errorf("Blackhole no-send-ack (synthetic)"))
					}
				}()
				ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
				h.sleep(20 * time.Second)

				if !fired.Load() {
					t.Fatal("no handover swap was attempted")
				}
				var after int
				func() {
					window.stateLock.Lock()
					defer window.stateLock.Unlock()
					after = len(window.verdictRemovalTimes)
				}()
				if strictHandoverTestInstalled(window, drained) {
					t.Fatal("the verdict victim kept its slot")
				}
				AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalVerdict].Load(), uint64(1))
				AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(0))
				AssertEqual(t, after, before+1)
			})
		})
	}
}

// A discovery that answers every find-providers2 with no providers and the
// platform override it is given.
type strictWindowHandoverTestDiscovery struct {
	mutex                sync.Mutex
	windowStrictHandover *bool
	err                  error
	requests             int
}

func (self *strictWindowHandoverTestDiscovery) FindProviders2(ctx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.requests += 1
	if self.err != nil {
		return nil, self.err
	}
	return &FindProviders2Result{WindowStrictHandover: self.windowStrictHandover}, nil
}

func (self *strictWindowHandoverTestDiscovery) answer(windowStrictHandover *bool, err error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.windowStrictHandover = windowStrictHandover
	self.err = err
}

// The api generator takes the platform's override from every find-providers2
// answer, on every path that asks: an answer that carries it sets it, an
// answer without it clears it, and a failed request keeps the last answer's.
// It crosses as `window_strict_handover`, absent when there is none.
func TestApiGeneratorTakesTheStrictWindowHandoverOverrideFromEachAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	discovery := &strictWindowHandoverTestDiscovery{}
	generator := newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{BestAvailable: true}}, discovery)
	off, on := false, true
	assertOverride := func(name string, wantEnabled bool, wantSet bool) {
		t.Helper()
		enabled, set := generator.StrictWindowHandoverOverride()
		if enabled != wantEnabled || set != wantSet {
			t.Fatalf("%s: override (%t, set %t), want (%t, set %t)", name, enabled, set, wantEnabled, wantSet)
		}
	}
	assertOverride("before any answer", false, false)
	for _, step := range []struct {
		name                 string
		windowStrictHandover *bool
		err                  error
		enabled              bool
		set                  bool
	}{
		{"an answer that turns it off", &off, nil, false, true},
		{"an answer without it", nil, nil, false, false},
		{"an answer that leaves it on", &on, nil, true, true},
		{"an answer that turns it off again", &off, nil, false, true},
		{"a failed request", nil, errors.New("discovery unavailable"), false, true},
	} {
		discovery.answer(step.windowStrictHandover, step.err)
		generator.NextDestinations(1, nil, "quality")
		assertOverride(step.name, step.enabled, step.set)
	}
	// the named-provider path asks find-providers2 too
	discovery.answer(nil, nil)
	if _, err := generator.NextDestinationsForClientId(NewId(), nil, "quality"); err != nil {
		t.Fatal(err)
	}
	assertOverride("a named-provider answer without it", false, false)

	for body, want := range map[string]*bool{
		`{"providers": [], "window_strict_handover": false}`: &off,
		`{"providers": [], "window_strict_handover": true}`:  &on,
		`{"providers": []}`: nil,
	} {
		result := &FindProviders2Result{}
		if err := json.Unmarshal([]byte(body), result); err != nil {
			t.Fatal(err)
		}
		if (result.WindowStrictHandover == nil) != (want == nil) || (want != nil && *result.WindowStrictHandover != *want) {
			t.Errorf("%s decoded to %v", body, result.WindowStrictHandover)
		}
	}
	encoded, err := json.Marshal(&FindProviders2Result{Providers: []*FindProvidersProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, string(encoded), `{"providers":[]}`)
}

// A generator that carries a platform override and discovers nothing.
type strictWindowHandoverOverrideTestGenerator struct {
	testingEmptyMultiClientGenerator
	override strictWindowHandoverOverride
}

func (self *strictWindowHandoverOverrideTestGenerator) StrictWindowHandoverOverride() (enabled bool, set bool) {
	return self.override.get()
}

// The platform's override can turn a strict window's handover off, never on:
// with no override, or one that leaves it on, the host's setting decides; off
// turns it off; and on does not turn on a handover the host left off, or one a
// window without a strict hard max never runs.
func TestStrictWindowHandoverFollowsThePlatformOverride(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		name     string
		strict   bool
		host     bool
		override *bool
		enabled  bool
	}{
		{"host on, no override", true, true, nil, true},
		{"host on, platform off", true, true, &off, false},
		{"host on, platform on", true, true, &on, true},
		{"host off, no override", true, false, nil, false},
		{"host off, platform on", true, false, &on, false},
		{"host off, platform off", true, false, &off, false},
		{"not strict, platform on", false, true, &on, false},
	} {
		generator := &strictWindowHandoverOverrideTestGenerator{}
		generator.override.observe(c.override)
		settings := DefaultMultiClientSettings()
		settings.StrictWindowSizeHardMax = c.strict
		settings.StrictWindowHandover = c.host
		window := &multiClientWindow{settings: settings, generator: generator, clients: map[Id]*multiClientChannel{}}
		if enabled := window.strictWindowHandoverEnabled(); enabled != c.enabled {
			t.Errorf("%s: handover enabled = %t, want %t", c.name, enabled, c.enabled)
		}
	}
	// a generator without the capability overrides nothing
	settings := DefaultMultiClientSettings()
	settings.StrictWindowSizeHardMax = true
	settings.StrictWindowHandover = true
	window := &multiClientWindow{settings: settings, generator: &testingEmptyMultiClientGenerator{}, clients: map[Id]*multiClientChannel{}}
	AssertEqual(t, window.strictWindowHandoverEnabled(), true)
}

// The platform turns the handover off for a window about to hand a drained
// client's slot over: the listing for the handover's candidate carries the
// override, and from that listing on the window plans as before the handover.
// The drained client keeps its slot, the candidate is refused at the hard max,
// and the pass's reservation ends with it.
func TestStrictWindowHandoverTurnsOffWithinOneListing(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, generator, stats := newWindowStatsTestHarness(t)
		window := h.multiClient.windows[WindowTypeQuality]
		h.sleep(time.Second)
		readsBefore, _, _, _ := generator.counts()
		off := false
		generator.platformStrictWindowHandover.Store(&off)
		drained := h.liveChannels()[0]
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(40 * time.Second)

		reads, _, _, _ := generator.counts()
		if reads == readsBefore {
			t.Fatal("the window listed nothing after the platform turned the handover off")
		}
		if enabled, set := generator.StrictWindowHandoverOverride(); enabled || !set {
			t.Fatal("the listing did not carry the platform's off")
		}
		AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), uint64(0))
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalHandover].Load(), uint64(0))
		if 0 == windowStatsCandidateTotal(stats, windowCandidateDeclinedStrictCap) {
			t.Fatal("no candidate was refused at the hard max, as before the handover")
		}
		if !strictHandoverTestInstalled(window, drained) {
			t.Fatal("the drained client lost its slot after the platform turned the handover off")
		}
		installedCount, _ := strictHandoverTestCounts(window)
		AssertEqual(t, installedCount, 5)
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			if window.strictPass != nil && window.strictPass.handover != nil {
				t.Fatal("a handover reservation outlived the platform's off")
			}
		}()
	})
}

// The platform turns the handover off while a pass holds a reservation: an off
// that arrives before admission takes the slot cancels the swap (the candidate
// is refused at the hard max and the victim keeps its slot), and one that
// arrives at the swap lets it finish. Either way the window never holds more
// than its hard max, the reservation ends with its pass, and the next drained
// client is not handed over.
func TestStrictWindowHandoverTurnedOffMidPassFallsBackCleanly(t *testing.T) {
	for _, atSwap := range []bool{false, true} {
		t.Run(fmt.Sprintf("at_swap=%t", atSwap), func(t *testing.T) {
			GetMessagePoolAggregateStats()
			synctest.Test(t, func(t *testing.T) {
				h, generator, stats := newWindowStatsTestHarness(t)
				window := h.multiClient.windows[WindowTypeQuality]
				h.sleep(time.Second)
				off := false
				var turnedOff atomic.Bool
				turnOff := func() {
					if turnedOff.CompareAndSwap(false, true) {
						// as another window's listing would carry it
						generator.platformStrictWindowHandover.Store(&off)
						generator.strictWindowHandoverOverride.observe(&off)
					}
				}
				if atSwap {
					func() {
						window.stateLock.Lock()
						defer window.stateLock.Unlock()
						window.beforeStrictHandoverSwapForTest = turnOff
					}()
				} else {
					func() {
						generator.mutex.Lock()
						defer generator.mutex.Unlock()
						generator.configureNewClient = func(*MultiClientGeneratorClientArgs, *ClientSettings) {
							turnOff()
						}
					}()
				}
				clients := h.liveChannels()
				drained, next := clients[0], clients[1]
				ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
				h.sleep(40 * time.Second)

				if !turnedOff.Load() {
					t.Fatal("the platform's off never arrived during the pass")
				}
				handovers := uint64(0)
				if atSwap {
					handovers = 1
				}
				AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), handovers)
				AssertEqual(t, strictHandoverTestInstalled(window, drained), !atSwap)

				// the next drained client is not handed over
				ageWindowClientForTest(next, time.Minute, 2*time.Minute)
				h.sleep(40 * time.Second)
				AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateAdmittedHandover), handovers)
				if !strictHandoverTestInstalled(window, next) {
					t.Fatal("a drained client was handed over after the platform turned the handover off")
				}
				installedCount, _ := strictHandoverTestCounts(window)
				AssertEqual(t, installedCount, 5)
				func() {
					window.stateLock.Lock()
					defer window.stateLock.Unlock()
					if window.strictPass != nil && window.strictPass.handover != nil {
						t.Fatal("a handover reservation outlived the platform's off")
					}
				}()
			})
		})
	}
}
