package connect

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

func TestReliabilityMetricsBlastRadius(t *testing.T) {
	m := newReliabilityMetrics()

	// two exits die, costing 3 and 5 flows
	m.exitLost(testRecoveryKeys(3, 1))
	m.exitLost(testRecoveryKeys(5, 2))

	s := m.snapshot()
	if s.ExitLossEvents != 2 {
		t.Errorf("exit loss events = %d, want 2", s.ExitLossEvents)
	}
	if s.FlowsLostToExit != 8 {
		t.Errorf("flows lost = %d, want 8", s.FlowsLostToExit)
	}
	if s.MaxFlowsLostInOneEvent != 5 {
		t.Errorf("max flows lost in one event = %d, want 5", s.MaxFlowsLostInOneEvent)
	}
	if s.MeanFlowsLostPerExitLoss != 4.0 {
		t.Errorf("blast radius = %v, want 4.0", s.MeanFlowsLostPerExitLoss)
	}
}

// An exit that dies carrying nothing costs the user nothing, but it is still a
// failure event -- counting it keeps the blast radius honest for a fix that
// works by spreading flows thinner.
func TestReliabilityMetricsEmptyExitLoss(t *testing.T) {
	m := newReliabilityMetrics()

	m.exitLost(nil)

	s := m.snapshot()
	if s.ExitLossEvents != 1 {
		t.Errorf("exit loss events = %d, want 1", s.ExitLossEvents)
	}
	if s.FlowsLostToExit != 0 {
		t.Errorf("flows lost = %d, want 0", s.FlowsLostToExit)
	}
	if s.MeanFlowsLostPerExitLoss != 0.0 {
		t.Errorf("blast radius = %v, want 0", s.MeanFlowsLostPerExitLoss)
	}
	if s.RecoveryPending != 0 {
		t.Errorf("pending = %d, want 0", s.RecoveryPending)
	}
}

func TestReliabilityMetricsRecovery(t *testing.T) {
	m := newReliabilityMetrics()

	ip := net.ParseIP("93.184.216.34").To4()
	m.exitLost([]recoveryKey{newRecoveryKey(ip, 443)})

	s := m.snapshot()
	if s.RecoveryPending != 1 {
		t.Fatalf("pending = %d, want 1", s.RecoveryPending)
	}
	if s.RecoveryCount != 0 {
		t.Fatalf("recovery count = %d, want 0 before the destination answers", s.RecoveryCount)
	}

	time.Sleep(20 * time.Millisecond)
	m.destinationReachable(ip, 443)

	s = m.snapshot()
	if s.RecoveryCount != 1 {
		t.Fatalf("recovery count = %d, want 1", s.RecoveryCount)
	}
	if s.RecoveryPending != 0 {
		t.Errorf("pending = %d, want 0 once recovered", s.RecoveryPending)
	}
	if s.RecoveryMeanNanos <= 0 {
		t.Errorf("recovery mean = %d, want positive", s.RecoveryMeanNanos)
	}
	if s.RecoveryMeanNanos != s.RecoveryMaxNanos {
		t.Errorf("mean %d != max %d for a single sample", s.RecoveryMeanNanos, s.RecoveryMaxNanos)
	}
}

// Traffic from somewhere that never lost an exit must not be counted as a
// recovery, or ordinary browsing would manufacture good numbers.
func TestReliabilityMetricsIgnoresUnrelatedTraffic(t *testing.T) {
	m := newReliabilityMetrics()

	lost := net.ParseIP("93.184.216.34").To4()
	other := net.ParseIP("1.1.1.1").To4()
	m.exitLost([]recoveryKey{newRecoveryKey(lost, 443)})

	// different host
	m.destinationReachable(other, 443)
	// right host, different port -- a different service, not the flow that died
	m.destinationReachable(lost, 80)

	s := m.snapshot()
	if s.RecoveryCount != 0 {
		t.Errorf("recovery count = %d, want 0", s.RecoveryCount)
	}
	if s.RecoveryPending != 1 {
		t.Errorf("pending = %d, want 1", s.RecoveryPending)
	}
}

// Several flows to one host die together; the user is waiting from the first
// of them, so the earliest loss is the one that must be timed.
func TestReliabilityMetricsKeepsEarliestLoss(t *testing.T) {
	m := newReliabilityMetrics()

	ip := net.ParseIP("93.184.216.34").To4()
	key := newRecoveryKey(ip, 443)

	m.exitLost([]recoveryKey{key})
	time.Sleep(20 * time.Millisecond)
	m.exitLost([]recoveryKey{key})

	m.pendingLock.Lock()
	elapsed := time.Since(m.pending[key])
	m.pendingLock.Unlock()

	if elapsed < 20*time.Millisecond {
		t.Fatalf("second loss overwrote the first: elapsed %v, want >= 20ms", elapsed)
	}
}

// A destination that never comes back has to be counted, otherwise abandoning
// flows entirely would score better than recovering them slowly.
func TestReliabilityMetricsMissedRecovery(t *testing.T) {
	m := newReliabilityMetrics()

	ip := net.ParseIP("93.184.216.34").To4()
	m.exitLost([]recoveryKey{newRecoveryKey(ip, 443)})

	// age the entry past the window rather than sleeping a minute
	m.pendingLock.Lock()
	for key := range m.pending {
		m.pending[key] = time.Now().Add(-2 * recoveryTrackerMaxAge)
	}
	m.pendingLock.Unlock()

	// eviction runs on the next loss
	m.exitLost(testRecoveryKeys(1, 9))

	s := m.snapshot()
	if s.RecoveryMissed != 1 {
		t.Errorf("missed = %d, want 1", s.RecoveryMissed)
	}
	if s.RecoveryCount != 0 {
		t.Errorf("recovery count = %d, want 0", s.RecoveryCount)
	}

	// the expired destination coming back late is not a recovery
	m.destinationReachable(ip, 443)
	if got := m.snapshot().RecoveryCount; got != 0 {
		t.Errorf("recovery count = %d after a late answer, want 0", got)
	}
}

// Eviction is lazy -- it only runs when another exit dies -- so a pending
// destination can outlive the age bound. If it then answers, it is the user
// revisiting the site, not the tunnel recovering, and counting it inflates the
// average without limit. On device this reported "avg 2m, worst 9m" for a
// tunnel that was recovering in seconds.
func TestReliabilityMetricsLateAnswerIsNotARecovery(t *testing.T) {
	m := newReliabilityMetrics()

	ip := net.ParseIP("93.184.216.34").To4()
	m.exitLost([]recoveryKey{newRecoveryKey(ip, 443)})

	// age it past the window without triggering eviction
	m.pendingLock.Lock()
	for key := range m.pending {
		m.pending[key] = time.Now().Add(-2 * recoveryTrackerMaxAge)
	}
	m.pendingLock.Unlock()

	m.destinationReachable(ip, 443)

	s := m.snapshot()
	if s.RecoveryCount != 0 {
		t.Errorf("a late answer was counted as a recovery: count %d, mean %dns", s.RecoveryCount, s.RecoveryMeanNanos)
	}
	if s.RecoveryMissed != 1 {
		t.Errorf("missed = %d, want 1: the destination never came back inside the window", s.RecoveryMissed)
	}
	if s.RecoveryPending != 0 {
		t.Errorf("pending = %d, want 0: the entry must be retired either way", s.RecoveryPending)
	}
}

// Losses arriving faster than they expire must not grow the tracker without
// bound -- this is the path that runs when an exit carrying every flow dies.
func TestReliabilityMetricsBoundsPending(t *testing.T) {
	m := newReliabilityMetrics()

	m.exitLost(testRecoveryKeys(recoveryTrackerMaxEntries+500, 1))

	s := m.snapshot()
	if recoveryTrackerMaxEntries < s.RecoveryPending {
		t.Fatalf("pending %d exceeds bound %d", s.RecoveryPending, recoveryTrackerMaxEntries)
	}
	if s.RecoveryMissed != 500 {
		t.Errorf("missed = %d, want 500", s.RecoveryMissed)
	}
}

func TestReliabilityMetricsReset(t *testing.T) {
	m := newReliabilityMetrics()

	ip := net.ParseIP("93.184.216.34").To4()
	m.flowOpened()
	m.exitLost([]recoveryKey{newRecoveryKey(ip, 443)})
	m.destinationReachable(ip, 443)

	m.reset()

	s := m.snapshot()
	if s.FlowsOpened != 0 || s.ExitLossEvents != 0 || s.FlowsLostToExit != 0 {
		t.Errorf("counters not cleared: %+v", s)
	}
	if s.RecoveryCount != 0 || s.RecoveryMissed != 0 || s.RecoveryMaxNanos != 0 {
		t.Errorf("recovery not cleared: %+v", s)
	}
	if s.RecoveryPending != 0 {
		t.Errorf("pending = %d, want 0", s.RecoveryPending)
	}
}

// The counters are written from the packet path, so concurrent use has to be
// clean under -race.
func TestReliabilityMetricsConcurrent(t *testing.T) {
	m := newReliabilityMetrics()

	wg := sync.WaitGroup{}
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys := testRecoveryKeys(16, i)
			for range 50 {
				m.flowOpened()
				m.exitLost(keys)
				for _, key := range keys {
					m.destinationReachable([]byte(key.ip), key.port)
				}
				m.snapshot()
			}
		}(i)
	}
	wg.Wait()

	s := m.snapshot()
	if s.FlowsOpened != 8*50 {
		t.Errorf("flows opened = %d, want %d", s.FlowsOpened, 8*50)
	}
	if s.ExitLossEvents != 8*50 {
		t.Errorf("exit loss events = %d, want %d", s.ExitLossEvents, 8*50)
	}
}

// The verdict-hold counters are defined here and incremented by the gating
// logic that lands separately; what this package owes them is the same
// contract as every other counter -- they count, they snapshot, they reset.
func TestReliabilityMetricsVerdictHoldCounters(t *testing.T) {
	m := newReliabilityMetrics()

	m.verdictHeldUplinkStale()
	m.verdictHeldUplinkStale()
	m.verdictHeldTransportDown()
	m.removalDeferred()
	m.removalDeferred()
	m.removalDeferred()

	s := m.snapshot()
	if s.VerdictsHeldUplinkStale != 2 {
		t.Errorf("verdicts held (uplink stale) = %d, want 2", s.VerdictsHeldUplinkStale)
	}
	if s.VerdictsHeldTransportDown != 1 {
		t.Errorf("verdicts held (transport down) = %d, want 1", s.VerdictsHeldTransportDown)
	}
	if s.RemovalsDeferred != 3 {
		t.Errorf("removals deferred = %d, want 3", s.RemovalsDeferred)
	}

	m.reset()
	s = m.snapshot()
	if s.VerdictsHeldUplinkStale != 0 || s.VerdictsHeldTransportDown != 0 || s.RemovalsDeferred != 0 {
		t.Errorf("verdict-hold counters not cleared: %+v", s)
	}
}

// Measurement sits on the packet path, so a client assembled without metrics
// has to keep forwarding traffic. Tests build RemoteUserNatMultiClient as a
// struct literal, which leaves the field nil -- and a panic there would take
// down the data path, not just the counters.
func TestReliabilityMetricsNilReceiver(t *testing.T) {
	var m *reliabilityMetrics

	ip := net.ParseIP("93.184.216.34").To4()
	m.flowOpened()
	m.exitLost([]recoveryKey{newRecoveryKey(ip, 443)})
	m.destinationReachable(ip, 443)
	m.verdictHeldUplinkStale()
	m.verdictHeldTransportDown()
	m.removalDeferred()
	m.reset()

	s := m.snapshot()
	if s == nil {
		t.Fatal("snapshot on a nil receiver returned nil")
	}
	if s.FlowsOpened != 0 || s.ExitLossEvents != 0 {
		t.Errorf("nil receiver produced counts: %+v", s)
	}
	if s.VerdictsHeldUplinkStale != 0 || s.VerdictsHeldTransportDown != 0 || s.RemovalsDeferred != 0 {
		t.Errorf("nil receiver produced verdict-hold counts: %+v", s)
	}
}

func testRecoveryKeys(n int, seed int) []recoveryKey {
	keys := []recoveryKey{}
	for i := range n {
		ip := net.ParseIP(fmt.Sprintf("10.%d.%d.%d", seed%256, (i/256)%256, i%256)).To4()
		keys = append(keys, newRecoveryKey(ip, 443))
	}
	return keys
}
