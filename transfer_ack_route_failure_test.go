// ACK route failure diagnostics distinguish the write budget from lifecycle errors.
package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Only the route-write timeout sentinel belongs in the existing timeout bucket.
func TestReceiveAckRouteWriteTimeoutClassification(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		timeouts uint64
	}{
		{name: "success"},
		{name: "route budget", err: errTransferRouteWriteTimeout, timeouts: 1},
		{name: "wrapped route budget", err: fmt.Errorf("synthetic writer: %w", errTransferRouteWriteTimeout), timeouts: 1},
		{name: "caller cancellation", err: context.Canceled},
		{name: "caller deadline", err: context.DeadlineExceeded},
		{name: "other writer failure", err: errors.New("synthetic writer failure")},
	} {
		client := &Client{}
		client.recordReceiveAckRouteWrite(TransportTypeH1, 0, false, false, c.err)
		stats := client.ReceiveStats()
		wantErrors := uint64(0)
		if c.err != nil {
			wantErrors = 1
		}
		if stats.AckRouteWriteCount != 1 || stats.AckRouteWriteErrorCount != wantErrors {
			t.Errorf("%s: writes=%d errors=%d, want 1/%d", c.name, stats.AckRouteWriteCount, stats.AckRouteWriteErrorCount, wantErrors)
		}
		if got := stats.AckRouteWriteTimeoutByTransport[TransportTypeH1]; got != c.timeouts {
			t.Errorf("%s: route timeouts=%d, want %d", c.name, got, c.timeouts)
		}
	}
}

// Failure-only waits do not change existing successful-write wait totals.
func TestReceiveAckRouteWriteFailureCounters(t *testing.T) {
	owner := &ackRouteWriteFailureCounters{}
	client := &Client{receiveAckRouteWriteFailures: &ackRouteWriteFailureCounters{}, ackRouteWriteFailureOwner: owner}
	for _, kind := range []transferWriteFailureKind{
		transferWriteFailureRouteTimeout, transferWriteFailureCallerCanceled,
		transferWriteFailureCallerDeadline, transferWriteFailureSelectorClosed, transferWriteFailureOther,
	} {
		client.recordReceiveAckRouteWriteDisposition(TransportTypeH1, 0, false, false, errors.New("synthetic unchanged error"), transferWriteDisposition{
			failureKind: kind, failureInitiallyBlocked: true, failureWaitDuration: 7 * time.Second,
		})
	}
	stats := client.ReceiveStats()
	want := AckRouteWriteFailureStatsSnapshot{
		ErrorCount: 5, RouteTimeoutCount: 1, CallerCanceledCount: 1, CallerDeadlineCount: 1,
		SelectorClosedCount: 1, OtherCount: 1, BlockedCount: 5, WaitDuration: 35 * time.Second, MaxWait: 7 * time.Second,
	}
	if stats.AckRouteWriteFailures == nil || *stats.AckRouteWriteFailures != want || owner.snapshot() == nil || *owner.snapshot() != want {
		t.Fatalf("failure evidence: client=%+v owner=%+v, want %+v", stats.AckRouteWriteFailures, owner.snapshot(), want)
	}
	if stats.AckRouteWriteCount != 5 || stats.AckRouteWriteErrorCount != 5 || stats.AckRouteWriteTimeoutByTransport[TransportTypeH1] != 1 || stats.AckRouteWriteBlockedCount != 0 || stats.AckRouteWriteWaitDuration != 0 || stats.AckRouteWriteMaxWait != 0 {
		t.Fatalf("legacy write/error/wait totals changed: %+v", stats)
	}
}

// Fixed atomics add no allocation on either success or preconstructed-error recording.
func TestReceiveAckRouteWriteFailureRecordingIsAllocationFree(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		client := &Client{}
		if enabled {
			client.receiveAckRouteWriteFailures = &ackRouteWriteFailureCounters{}
			client.ackRouteWriteFailureOwner = &ackRouteWriteFailureCounters{}
		}
		for _, err := range []error{nil, errTransferRouteWriteTimeout, context.Canceled, context.DeadlineExceeded, errors.New("synthetic custom error")} {
			if allocations := testing.AllocsPerRun(1000, func() {
				client.recordReceiveAckRouteWrite(TransportTypeH1, 0, false, false, err)
			}); allocations != 0 {
				t.Errorf("enabled=%t error=%v: allocations=%g, want 0", enabled, err, allocations)
			}
		}
		if !enabled && (client.receiveAckRouteWriteFailures != nil || client.ackRouteWriteFailureOwner != nil || client.ReceiveStats().AckRouteWriteFailures != nil) {
			t.Fatal("disabled recording installed diagnostic state")
		}
	}
}

// Measures the unchanged default recorder boundary without constructing errors per call.
func BenchmarkReceiveAckRouteWriteStatsError(b *testing.B) {
	client := &Client{}
	err := errors.New("synthetic writer error")
	b.ReportAllocs()
	for range b.N {
		client.recordReceiveAckRouteWrite(TransportTypeUnknown, 0, false, false, err)
	}
}
