// Selected byte provenance survives ties, reordering and bounded aggregation.
package connect

import (
	"reflect"
	"testing"
	"time"
	"unsafe"
)

// Real pending or confirmed offers supply each inert coalescer observation.
type windowServiceSuffixPoint struct {
	offset  time.Duration
	bytes   ByteCount
	pending bool
}

// No sender worker runs in this fixture. Its caller owns all sample inspection.
func coalesceWindowServiceSuffixPoints(t *testing.T, points []windowServiceSuffixPoint, order []int) (*windowPacingService, time.Time) {
	t.Helper()
	service, start := newWindowQualifiedServiceFixture(t, 100000, 0, time.Millisecond)
	fixture := newWindowReceiverCreditFixture(t, service, start, 0)
	base := start.Truncate(time.Second).Add(2 * time.Second)
	items := make([]*sendItem, len(points))
	for i, point := range points {
		if point.pending {
			items[i] = fixture.offer(uint64(i), point.bytes, start)
		} else {
			items[i] = fixture.write(uint64(i), point.bytes, start)
		}
	}
	fixture.write(uint64(len(points)), 3445, start)
	for _, i := range order {
		ack := fixture.ack(items[i], base.Add(points[i].offset), 0)
		ack.selective, ack.receiverAckDelaySet = true, false
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	}
	return service, base
}

// Every tied earliest credit is excluded. An earlier reordered checkpoint
// makes the former first selected, including any ambiguity it actually owns.
func TestWindowServiceRecoverySuffixTiesAndReordering(t *testing.T) {
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, points := range [][]windowServiceSuffixPoint{
		{{time.Millisecond, 31, true}, {time.Millisecond, 17, false}, {2 * time.Millisecond, 154, false}},
		{{time.Millisecond, 31, false}, {2 * time.Millisecond, 17, true}, {2 * time.Millisecond, 154, false}},
		{{time.Millisecond, 31, true}, {2 * time.Millisecond, 17, false}, {3 * time.Millisecond, 154, false}},
		{{time.Millisecond, 31, false}, {2 * time.Millisecond, 17, true}, {3 * time.Millisecond, 154, false}},
	} {
		for _, order := range orders {
			service, base := coalesceWindowServiceSuffixPoints(t, points, order)
			first, last := points[0].offset, points[0].offset
			for _, point := range points {
				first, last = min(first, point.offset), max(last, point.offset)
			}
			var total, firstBytes, selectedBytes ByteCount
			complete := true
			selectedFirst := last
			for _, point := range points {
				total += point.bytes
				if point.offset == first {
					firstBytes += point.bytes
				} else {
					selectedBytes += point.bytes
					selectedFirst = min(selectedFirst, point.offset)
					complete = complete && !point.pending
				}
			}
			sample := windowRecoveryServiceSampleAt(t, service, base.Add(last))
			selected := sample.coverageAfterFirst()
			if sample.bytes != total || sample.firstBytes != firstBytes || selected.bytes != selectedBytes ||
				selected.bytes != sample.bytes-sample.firstBytes || selected.supply.complete != complete ||
				!selected.supply.recorded || selected.firstAtNanos != base.Add(selectedFirst).UnixNano() ||
				selected.lastAtNanos != base.Add(last).UnixNano() || selected.supply.minimumPath != time.Millisecond {
				t.Fatalf("points=%+v order=%v: selected=%+v full=%+v", points, order, selected, sample)
			}
			if selected.supply.maximumGap < selectedFirst-first {
				t.Fatal("excluding first bytes erased the first-to-selected clock gap")
			}
		}
	}
}

// A real timing update rebuckets the three independently recorded credits.
// Newest-first rebuilding must not keep the old first as an excluded byte.
func TestWindowServiceRecoverySuffixRebucketPreservesSelectedBytes(t *testing.T) {
	points := []windowServiceSuffixPoint{
		{time.Millisecond, 31, true}, {21 * time.Millisecond, 17, false}, {41 * time.Millisecond, 154, false},
	}
	service, base := coalesceWindowServiceSuffixPoints(t, points, []int{0, 1, 2})
	at := base.Add(42 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 401*time.Millisecond, time.Millisecond, 400*time.Millisecond, at)
	if service.bucketInterval != 100*time.Millisecond {
		t.Fatalf("real timing update did not merge the intended buckets: %s", service.bucketInterval)
	}
	sample := windowRecoveryServiceSampleAt(t, service, base.Add(41*time.Millisecond))
	selected := sample.coverageAfterFirst()
	if sample.bytes != 202 || sample.firstBytes != 31 || selected.bytes != 171 ||
		selected.bytes != sample.bytes-sample.firstBytes || !selected.supply.complete ||
		selected.firstAtNanos != base.Add(21*time.Millisecond).UnixNano() ||
		selected.lastAtNanos != base.Add(41*time.Millisecond).UnixNano() ||
		selected.supply.maximumGap != 20*time.Millisecond {
		t.Fatalf("rebucket changed selected physical provenance: full=%+v selected=%+v", sample, selected)
	}
}

// The actual completed feedback summary remains sufficient after its ring
// entries retire; the excluded pending checkpoint cannot veto selected credit.
func TestWindowServiceRecoverySuffixCompletedFeedbackFallback(t *testing.T) {
	points := []windowServiceSuffixPoint{{time.Millisecond, 2315, true}, {2 * time.Millisecond, 154, false}}
	service, base := coalesceWindowServiceSuffixPoints(t, points, []int{0, 1})
	cycle := service.feedbackComplete
	if service.feedbackPending || cycle.bytes != 2469 || cycle.firstBytes != 2315 || cycle.coverageAfterFirst().bytes != 154 {
		t.Fatalf("actual coalescer did not complete the intended turn: %+v", cycle)
	}
	// Isolate the existing completed-summary fallback without changing it.
	clear(service.samples[:])
	if rate, _, latest := service.measured(time.Second, base.Add(2*time.Millisecond)); rate != 154000 || latest != 154000 {
		t.Fatalf("completed selected suffix lost its capacity after ring retirement: %d/%d", rate, latest)
	}
}

// This metadata-only control owns an inert summary at the already-proved drain
// boundary. It checks byte selection, not the independent drain proof policy.
func TestWindowServiceRecoverySuffixSyntheticBoundarySelectsAllBytes(t *testing.T) {
	points := []windowServiceSuffixPoint{{time.Millisecond, 31, true}, {2 * time.Millisecond, 154, false}}
	service, base := coalesceWindowServiceSuffixPoints(t, points, []int{0, 1})
	cycle := windowRecoveryServiceSampleAt(t, service, base.Add(2*time.Millisecond))
	full := cycle.coverage()
	service.feedbackCycle = cycle
	service.feedbackPending = true
	service.feedbackCycleBefore = base
	service.feedbackDrainAt = base.Add(2 * time.Millisecond)
	service.feedbackDrainPending = 0
	service.feedbackInterval = time.Second
	for range 2 {
		service.completeFeedbackCycleWithLock()
		got := service.feedbackCycle
		if got.firstBytes != 0 || got.firstAtNanos != base.UnixNano() || got.afterFirst != full ||
			got.afterFirst.bytes != got.bytes-got.firstBytes || got.afterFirst.supply.complete {
			t.Fatalf("zero-byte boundary lost or invented selected evidence: %+v", got)
		}
	}
}

// The suffix is fixed-size scalar state per service, never a retained-message
// field, event list or allocation on the ACK aggregation path.
func TestWindowServiceRecoverySuffixMemoryBound(t *testing.T) {
	if unsafe.Sizeof(windowServiceCoverage{}) > 192 || unsafe.Sizeof(windowServiceSample{}) > 512 ||
		unsafe.Sizeof(windowPacingService{}) > 32*1024 {
		t.Fatal("selected suffix exceeded its fixed per-service metadata bound")
	}
	var scalar func(reflect.Type) bool
	scalar = func(kind reflect.Type) bool {
		// This exact fixed identity carries no route, pointer, or payload.
		// All byte-size caps above remain unchanged.
		if kind == reflect.TypeOf(Id{}) {
			return kind.Kind() == reflect.Array && kind.Len() == 16 && kind.Elem().Kind() == reflect.Uint8
		}
		if kind.Kind() == reflect.Struct {
			for i := 0; i < kind.NumField(); i++ {
				if !scalar(kind.Field(i).Type) {
					return false
				}
			}
			return true
		}
		return kind.Kind() == reflect.Int64 || kind.Kind() == reflect.Bool
	}
	if !scalar(reflect.TypeOf(windowServiceCoverage{})) {
		t.Fatal("selected suffix acquired a pointer or variable-length member")
	}
	point := windowServiceSample{firstAtNanos: 100, lastAtNanos: 100, firstBytes: 1, bytes: 1}
	if allocated := testing.AllocsPerRun(100, func() {
		sample := point
		next := point
		next.firstAtNanos, next.lastAtNanos = 200, 200
		sample.mergeSupply(next)
	}); allocated != 0 {
		t.Fatalf("suffix aggregation allocated: %f", allocated)
	}
	t.Logf("coverage=%d sample=%d service=%d deliveryRing=%d sendItem=%d sequenceAck=%d",
		unsafe.Sizeof(windowServiceCoverage{}), unsafe.Sizeof(windowServiceSample{}), unsafe.Sizeof(windowPacingService{}),
		unsafe.Sizeof(windowServiceDeliveryRing{}), unsafe.Sizeof(sendItem{}), unsafe.Sizeof(sequenceAck{}))
}
