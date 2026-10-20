//go:build !js

package connect

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
)

// Observe the pinned SCTP's existing congestion-control trace without changing
// its ACK policy or window. BlockWrite admits one message at a time; an empty
// SCTP pending queue need not mean the bounded application queue is empty.
type udpSctpWindowExperimentLogger struct {
	logging.LeveledLogger
	measuring         atomic.Bool
	admitted          atomic.Int64
	physicalWrites    atomic.Pointer[atomic.Int64]
	growth            atomic.Int64
	skippedBacklogged atomic.Int64
}

// This discriminator charges live SCTP payload conservatively at twice its
// size, in addition to the separately reserved fixed owner. Every fixture
// message is a single 1,134-byte DATA payload, so release by acknowledged
// payload count also releases whole message/chunk ownership. A production
// implementation would need the corresponding fragmented-message ledger.
type udpSctpDynamicServiceExperiment struct {
	stream     *udpSctpAckExperimentStream
	mutex      sync.Mutex
	payload    uint64
	writing    bool
	peakCharge atomic.Int64
}

func (q *udpSctpDynamicServiceExperiment) releaseWithLock() {
	buffered := q.stream.Stream.BufferedAmount()
	if buffered < q.payload {
		q.stream.budget.Release(ByteCount(2 * (q.payload - buffered)))
		q.payload = buffered
	}
	if buffered > 0 {
		q.stream.Stream.SetBufferedAmountLowThreshold(buffered - 1)
	}
}

func (q *udpSctpDynamicServiceExperiment) releaseAcknowledged() {
	q.mutex.Lock()
	if !q.writing {
		q.releaseWithLock()
	}
	q.mutex.Unlock()
	notifyP2pLegacySendQueue(q.stream.serviceWake)
}

func (q *udpSctpDynamicServiceExperiment) admit(length int) error {
	if q.stream.serviceLimit < uint64(length) {
		return io.ErrShortBuffer
	}
	for {
		capacity := q.stream.budget.CapacityNotify()
		q.mutex.Lock()
		q.releaseWithLock()
		if q.payload+uint64(length) <= q.stream.serviceLimit && q.stream.budget.TryReserve(ByteCount(2*length)) {
			q.payload += uint64(length)
			q.writing = true
			q.peakCharge.Store(max(q.peakCharge.Load(), int64(2*q.payload)))
			q.mutex.Unlock()
			return nil
		}
		q.mutex.Unlock()
		select {
		case <-q.stream.ctx.Done():
			return net.ErrClosed
		case <-capacity:
		case <-q.stream.serviceWake:
		}
	}
}

func (q *udpSctpDynamicServiceExperiment) finishWrite() {
	q.mutex.Lock()
	q.writing = false
	q.releaseWithLock()
	q.mutex.Unlock()
}

func (q *udpSctpDynamicServiceExperiment) close() {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	q.stream.budget.Release(ByteCount(2 * q.payload))
	q.payload = 0
}

func TestProviderUdpSctpDynamicServiceWindowExperiment(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, limit := range []ByteCount{kib(64), kib(96), kib(128)} {
		for _, delay := range []time.Duration{0, 50 * time.Microsecond} {
			t.Run(fmt.Sprintf("window-%dKiB-delay-%s", limit/1024, delay), func(t *testing.T) {
				budget := NewTransferMemoryBudget(kib(768))
				if !budget.TryReserve(kib(512)) {
					t.Fatal("existing SCTP owner admission failed")
				}
				result := runProviderUdpSctpQueueExperiment(t, 120*time.Millisecond, false, false, 4, 0,
					udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget, traceWindow: true, writeDelay: delay, serviceWindowByteCount: limit, dynamicServiceCharge: true})
				t.Logf("dynamic_service_window=%d write_delay=%s %+v", limit, delay, result)
				if result.retransmits != 0 || budget.UsedByteCount() != kib(512) || int64(limit) < result.peakSctpBufferedBytes || result.peakSharedBudgetBytes > kib(768) {
					t.Fatalf("dynamic service violated lossless/budget invariant: %+v used=%d", result, budget.UsedByteCount())
				}
				budget.Release(kib(512))
			})
		}
	}
}

// A known per-write service cost is independent of the race detector. Even
// the largest tested cost permits twice the source's offered packet rate; a
// refusal therefore cannot be explained by a slower-than-offer writer alone.
func TestProviderUdpSctpServiceDelayExperiment(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, delay := range []time.Duration{0, 50 * time.Microsecond, 200 * time.Microsecond, 500 * time.Microsecond, time.Millisecond} {
		t.Run(fmt.Sprintf("write-delay-%s", delay), func(t *testing.T) {
			budget := NewTransferMemoryBudget(kib(768))
			if !budget.TryReserve(kib(512)) {
				t.Fatal("existing SCTP owner admission failed")
			}
			result := runProviderUdpSctpQueueExperiment(t, 120*time.Millisecond, false, false, 4, 0,
				udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget, traceWindow: true, writeDelay: delay})
			t.Logf("write_delay=%s %+v", delay, result)
			if result.retransmits != 0 || budget.UsedByteCount() != kib(512) {
				t.Fatalf("service control violated lossless/budget invariant: %+v used=%d", result, budget.UsedByteCount())
			}
			budget.Release(kib(512))
		})
	}
}

// Keep production's 256-KiB shared headroom, but precharge a bounded portion
// for SCTP's service window. The remaining compact queue must compete for
// exactly the same budget; no candidate obtains extra allowance by disabling
// BlockWrite. H1 and the negotiated native fast lane are not part of this
// test-only detached SCTP association.
func TestProviderUdpSctpBoundedServiceWindowExperiment(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, limit := range []ByteCount{0, kib(16), kib(32), kib(64), kib(96)} {
		for _, delay := range []time.Duration{0, 50 * time.Microsecond} {
			t.Run(fmt.Sprintf("window-%dKiB-delay-%s", limit/1024, delay), func(t *testing.T) {
				budget := NewTransferMemoryBudget(kib(768))
				if !budget.TryReserve(kib(512)) {
					t.Fatal("existing SCTP owner admission failed")
				}
				result := runProviderUdpSctpQueueExperiment(t, 120*time.Millisecond, false, false, 4, 0,
					udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget, traceWindow: true, writeDelay: delay, serviceWindowByteCount: limit})
				t.Logf("service_window=%d write_delay=%s %+v", limit, delay, result)
				if result.retransmits != 0 || budget.UsedByteCount() != kib(512) || (limit > 0 && int64(limit) < result.peakSctpBufferedBytes) {
					t.Fatalf("bounded service window violated lossless/budget invariant: %+v used=%d", result, budget.UsedByteCount())
				}
				budget.Release(kib(512))
			})
		}
	}
}

func newUdpSctpWindowExperimentLogger() *udpSctpWindowExperimentLogger {
	return &udpSctpWindowExperimentLogger{LeveledLogger: logging.NewDefaultLoggerFactory().NewLogger("sctp")}
}

func (l *udpSctpWindowExperimentLogger) NewLogger(string) logging.LeveledLogger { return l }

func (l *udpSctpWindowExperimentLogger) Tracef(format string, args ...any) {
	if !l.measuring.Load() {
		return
	}
	if strings.Contains(format, "updated cwnd=") {
		l.growth.Add(1)
	} else if strings.Contains(format, "cwnd did not grow:") && len(args) == 6 && args[5] == 0 {
		writes := l.physicalWrites.Load()
		if writes != nil && l.admitted.Load() > writes.Load() {
			l.skippedBacklogged.Add(1)
		}
	}
}

// Yielding the application writer is an explicit scheduler control, not a
// production candidate. The fixed offered traffic, lossless wire, shared
// reservation and terminal outcome accounting remain unchanged in every arm.
func TestProviderUdpSctpServiceSchedulingExperiment(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, arm := range []struct {
		name                string
		queue, trace, yield bool
	}{
		{"bounded-route-control", false, true, false},
		{"compact-control", true, false, false},
		{"compact-traced", true, true, false},
		{"compact-yield-before-write", true, true, true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			budget := NewTransferMemoryBudget(kib(768))
			if !budget.TryReserve(kib(512)) {
				t.Fatal("existing SCTP owner admission failed")
			}
			result := runProviderUdpSctpQueueExperiment(t, 120*time.Millisecond, false, false, 4, 0,
				udpSctpProductionQueueExperimentSettings{enabled: arm.queue, budget: budget, traceWindow: arm.trace, yieldBeforeWrite: arm.yield})
			t.Logf("%+v", result)
			if result.retransmits != 0 || budget.UsedByteCount() != kib(512) {
				t.Fatalf("service control violated lossless/budget invariant: %+v used=%d", result, budget.UsedByteCount())
			}
			budget.Release(kib(512))
		})
	}
}

// A loss/RTT comparison for isolated dependency candidates. All admitted
// identities must still arrive exactly once; source refusals and physical
// DATA retransmits remain separate measurements, never relabeled as success.
func TestProviderUdpSctpGrowthLossSafetyExperiment(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rtt := range []time.Duration{2 * time.Millisecond, 50 * time.Millisecond, 120 * time.Millisecond} {
		for _, loss := range []struct {
			name         string
			every, first int
		}{
			{"clean", 0, 0}, {"one-percent", 100, 0}, {"first-flight-outage", 0, 3},
		} {
			t.Run(fmt.Sprintf("rtt-%s-%s", rtt, loss.name), func(t *testing.T) {
				budget := NewTransferMemoryBudget(kib(768))
				if !budget.TryReserve(kib(512)) {
					t.Fatal("existing SCTP owner admission failed")
				}
				result := runProviderUdpSctpQueueExperiment(t, rtt, false, false, 4, 0,
					udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget, traceWindow: true, writeDelay: 50 * time.Microsecond, dropEveryDataPacket: loss.every, dropFirstDataPackets: loss.first})
				t.Logf("%+v", result)
				if budget.UsedByteCount() != kib(512) || result.peakSharedBudgetBytes > kib(768) {
					t.Fatalf("loss candidate leaked/overdrew shared budget: %+v", result)
				}
				// Each fixed-size wire datagram carries one DATA chunk. A
				// lossless reverse path therefore needs exactly one retransmit
				// per deliberately dropped datagram, not additional recovery.
				if result.retransmits != result.injectedLossPackets {
					t.Fatalf("loss fixture produced spurious or missing retransmits: %+v", result)
				}
				if loss.first > 0 && (result.injectedLossPackets != loss.first || result.retransmits < loss.first) {
					t.Fatalf("did not exercise lost initial flight recovery: %+v", result)
				}
				budget.Release(kib(512))
			})
		}
	}
}
