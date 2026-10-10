// Route-stall diagnostics must not scan a deep flight when the observation
// cannot improve the client's recorded silence interval.
package connect

import (
	"testing"
	"time"
)

// Fixed clocks reproduce the no-op scans that amplified the virtual-time
// matrix. Work is counted directly, independently of scheduler and host speed.
func TestRouteStallUnchangedObservationDoesNotScanRetainedItems(t *testing.T) {
	for _, itemCount := range []int{32, 1024, 16384} {
		start := time.Unix(1700000000, 0)
		route := make(Route)
		sequence := &SendSequence{client: &Client{}}
		for range itemCount {
			sequence.sendItems = append(sequence.sendItems, &sendItem{carrierRoute: route, sendTime: start})
		}
		scans := 0
		sequence.beforeRouteRetainedCountForTest = func() { scans++ }
		now := start.Add(100 * time.Millisecond)
		sequence.observeRouteStall(now)
		if scans != 1 || sequence.client.routeUnacknowledgedNanos.Load() != uint64(100*time.Millisecond) ||
			sequence.client.routeRetainedItemCount.Load() != uint64(itemCount) {
			t.Fatalf("items=%d initial observation lost its duration or count", itemCount)
		}
		for range 8 {
			sequence.observeRouteStall(now)
		}
		// A newer Ack can shorten the candidate, and a different sequence
		// can already have recorded a longer gap on this same client.
		sequence.laneAcks[0] = laneAckSlot{route: route, lastAckNanos: now.UnixNano(), set: true}
		sequence.observeRouteStall(now)
		sequence.observeRouteStall(now.Add(50 * time.Millisecond))
		sequence.client.routeUnacknowledgedNanos.Store(uint64(time.Second))
		sequence.observeRouteStall(now.Add(500 * time.Millisecond))
		if scans != 1 {
			t.Fatalf("items=%d unchanged or shorter observations rescanned retained items %d times", itemCount, scans-1)
		}
		if sequence.client.routeRetainedItemCount.Load() != uint64(itemCount) {
			t.Fatal("a non-record observation replaced the recorded count")
		}
	}
}

// A new silence record still counts exactly the oldest unacknowledged item's
// physical route, including selectively acknowledged items retained there.
func TestRouteStallNewRecordCountsTheOldestOutstandingRoute(t *testing.T) {
	start := time.Unix(1700000000, 0)
	route, otherRoute := make(Route), make(Route)
	sequence := &SendSequence{client: &Client{}, sendItems: []*sendItem{
		nil,
		{carrierRoute: otherRoute, sendTime: start, selectiveAcked: true},
		{sendTime: start},
		{carrierRoute: route, sendTime: start},
		{carrierRoute: route, sendTime: start, selectiveAcked: true},
		{carrierRoute: otherRoute, sendTime: start},
	}}
	scans := 0
	sequence.beforeRouteRetainedCountForTest = func() { scans++ }
	sequence.observeRouteStall(start.Add(100 * time.Millisecond))
	if scans != 1 || sequence.client.routeRetainedItemCount.Load() != 2 {
		t.Fatal("initial record counted another route or omitted retained selective data")
	}
	sequence.sendItems = append(sequence.sendItems, &sendItem{carrierRoute: route, sendTime: start})
	sequence.observeRouteStall(start.Add(200 * time.Millisecond))
	if scans != 2 || sequence.client.routeUnacknowledgedNanos.Load() != uint64(200*time.Millisecond) ||
		sequence.client.routeRetainedItemCount.Load() != 3 {
		t.Fatal("a new record did not refresh both duration and retained count")
	}
	sequence.sendItems = sequence.sendItems[:3]
	sequence.observeRouteStall(start.Add(time.Second))
	if scans != 2 || sequence.client.routeUnacknowledgedNanos.Load() != uint64(200*time.Millisecond) ||
		sequence.client.routeRetainedItemCount.Load() != 3 {
		t.Fatal("a flight without an eligible route changed the record")
	}
}
