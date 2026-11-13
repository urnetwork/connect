// Measure pre-existing retained-item scans without sockets, goroutines or
// advancing clocks. These diagnostics set no host-dependent timing threshold.
package connect

import (
	"testing"
	"time"
)

// The measured calls must leave every retained item on its original timer and
// recovery state. Check both the fixture and the final state outside timing.
func assertSendSequenceScanItems(
	b *testing.B,
	sequence *SendSequence,
	items []*sendItem,
	sendTime time.Time,
) {
	b.Helper()
	if len(sequence.sendItems) != len(items) || sequence.resendQueue.Len() != len(items) ||
		sequence.selectiveGapRecoveryActive {
		b.Fatal("scan changed retained ownership or activated recovery")
	}
	resendTime := sendTime.Add(sequence.sendBufferSettings.SelectiveAckTimeout)
	for index, item := range items {
		if sequence.sendItems[index] != item || item.sendTime != sendTime ||
			item.resendTime != resendTime || item.sendCount != 1 || item.selectiveAcked ||
			item.selectiveGapRecovered || item.ackTailProbeCount != 0 ||
			item.recoveryKind != sendRecoveryNone || item.deferralOutstanding {
			b.Fatalf("scan changed timer or recovery state for retained item %d", index)
		}
	}
}

// A healthy cumulative reply bypasses the scoreboard without selective
// evidence. Keep that no-op branch fixed as retained flight size changes.
func benchmarkSendSequenceSelectiveAckRecoveryNoEvidence(b *testing.B, itemCount int) {
	sendTime := time.Unix(1700000000, 0)
	now := sendTime.Add(100 * time.Millisecond)
	sequence, items := newSelectiveAckRecoveryTestSequence(itemCount, sendTime)
	sequence.client = &Client{}
	assertSendSequenceScanItems(b, sequence, items, sendTime)
	var scheduled bool
	b.ReportAllocs()
	for b.Loop() {
		scheduled = sequence.scheduleSelectiveAckRecoveryAfterFeedback(now) || scheduled
	}
	if scheduled || sequence.client.routeUnacknowledgedNanos.Load() != 0 ||
		sequence.client.routeRetainedItemCount.Load() != 0 {
		b.Fatal("a no-evidence recovery scan scheduled work or changed route metrics")
	}
	assertSendSequenceScanItems(b, sequence, items, sendTime)
}

// Establish the route's high-water diagnostic once, then hold both its ACK clock
// and observation clock fixed. Repeated observations cannot improve that record.
func benchmarkSendSequenceRouteStallUnchanged(b *testing.B, itemCount int) {
	sendTime := time.Unix(1700000000, 0)
	now := sendTime.Add(100 * time.Millisecond)
	sequence, items := newSelectiveAckRecoveryTestSequence(itemCount, sendTime)
	sequence.client = &Client{}
	route := make(Route)
	for _, item := range items {
		item.carrierRoute = route
		item.reliableCarrierObserved = true
	}
	sequence.laneAcks[0] = laneAckSlot{
		route:        route,
		lastAckNanos: sendTime.Add(50 * time.Millisecond).UnixNano(),
		set:          true,
	}
	check := func() {
		assertSendSequenceScanItems(b, sequence, items, sendTime)
		if sequence.client.routeUnacknowledgedNanos.Load() != uint64(50*time.Millisecond) ||
			sequence.client.routeRetainedItemCount.Load() != uint64(itemCount) {
			b.Fatal("unchanged observation lost the route's recorded duration or retained count")
		}
	}
	sequence.observeRouteStall(now)
	check()
	b.ReportAllocs()
	for b.Loop() {
		sequence.observeRouteStall(now)
	}
	check()
}

// Small retained flight isolates the fixed recovery-call overhead.
func BenchmarkSendSequenceSelectiveAckRecoveryNoEvidence32(b *testing.B) {
	benchmarkSendSequenceSelectiveAckRecoveryNoEvidence(b, 32)
}

// A medium retained flight exposes recovery scan scaling.
func BenchmarkSendSequenceSelectiveAckRecoveryNoEvidence1024(b *testing.B) {
	benchmarkSendSequenceSelectiveAckRecoveryNoEvidence(b, 1024)
}

// A large retained flight stresses recovery work without physical traffic.
func BenchmarkSendSequenceSelectiveAckRecoveryNoEvidence16384(b *testing.B) {
	benchmarkSendSequenceSelectiveAckRecoveryNoEvidence(b, 16384)
}

// Small retained flight isolates the fixed route-observation overhead.
func BenchmarkSendSequenceRouteStallUnchanged32(b *testing.B) {
	benchmarkSendSequenceRouteStallUnchanged(b, 32)
}

// A medium retained flight exposes route-observation scan scaling.
func BenchmarkSendSequenceRouteStallUnchanged1024(b *testing.B) {
	benchmarkSendSequenceRouteStallUnchanged(b, 1024)
}

// A large retained flight stresses diagnostics that cannot change their result.
func BenchmarkSendSequenceRouteStallUnchanged16384(b *testing.B) {
	benchmarkSendSequenceRouteStallUnchanged(b, 16384)
}
