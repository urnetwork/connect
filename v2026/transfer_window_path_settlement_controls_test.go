package connect

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These controls exercise the test observer with scalar copies. The separate
// controlled-drain regression supplies actual transport/writer provenance.
func newWindowSettlementControl(t *testing.T) (*windowPathSettlement, time.Time, windowPathCell) {
	t.Helper()
	start := time.Unix(1000, 0)
	cell := windowLongRingGrowthCell("delivery", 0)
	cell.Settlement = &windowPathSettlementPolicy{Version: "paired-circulation-v1", ReferenceBytes: 12_000_000,
		ReferenceDuration: time.Second, CapBytes: int64(cell.Budget), AckLifetime: DefaultSendBufferSettings().AckTimeout}
	o, err := newWindowPathSettlement(context.Background(), cell, start, cell.Settlement.AckLifetime)
	if err != nil {
		t.Fatal(err)
	}
	if o.stableTurn != 1510*time.Millisecond || o.rawResidenceCeiling != 1510*time.Millisecond {
		t.Fatal("the approved observation turn or raw-residence ceiling changed")
	}
	for i := range o.reading.Lanes {
		o.reading.Lanes[i].Owner = Id{byte(i + 1)}
	}
	return o, start, cell
}

func windowSettlementControlOriginal(o *windowPathSettlement, lane int, physical time.Time) windowPathSettlementOriginal {
	return windowPathSettlementOriginal{Sequence: o.reading.Lanes[lane].Owner, Message: Id{0, byte(lane + 1)},
		Number: uint64(100 + lane), Tag: uint64(physical.UnixMilli()), PhysicalAtNanos: physical.UnixNano(),
		Success: true, H1: true, SendCount: 1}
}

func windowSettlementControlProof(o *windowPathSettlement, lane int, physical, covered time.Time) {
	row := windowSettlementControlOriginal(o, lane, physical)
	o.original(uint32(lane+1), row)
	o.cumulative(uint32(lane+1), row.Sequence, sequenceAck{sequenceNumber: row.Number,
		messageId: row.Message, receivedAtNanos: covered.UnixNano()})
}

func windowSettlementControlProofs(o *windowPathSettlement, start time.Time) {
	for lane := range o.reading.Lanes {
		windowSettlementControlProof(o, lane, start.Add(10*time.Second), start.Add(12*time.Second))
	}
}

func windowSettlementControlState(start, at time.Time, arrivals ...time.Duration) windowPathSettlementState {
	s := windowPathSettlementState{At: at, QualityAt: start.Add(4 * time.Second), EpochAt: start.Add(4 * time.Second),
		BaselineSet: true, Baseline: windowPathSettlementTuple{AtNanos: start.Add(6 * time.Second).UnixNano(),
			Raw: 1210 * time.Millisecond, Adjusted: 1200 * time.Millisecond, Compression: 10 * time.Millisecond}}
	for i, arrival := range arrivals {
		s.Samples[i] = windowPathSettlementTuple{AtNanos: start.Add(arrival).UnixNano(),
			Raw: 1210 * time.Millisecond, Adjusted: 1200 * time.Millisecond, Compression: 10 * time.Millisecond}
	}
	s.Count = len(arrivals)
	return s
}

func windowSettlementControlObserve(o *windowPathSettlement, s windowPathSettlementState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observeWithLock(s)
}

func windowSettlementControlReady(t *testing.T, o *windowPathSettlement, at time.Time, want bool, label string) {
	t.Helper()
	ready, err := o.admit(at)
	if err != nil || ready != want {
		t.Fatalf("%s: ready=%t want=%t error=%v state=%+v", label, ready, want, err, o.snapshot())
	}
}

func TestWindowPathSettlementRequiresObservedPostPrefixTurn(t *testing.T) {
	o, start, _ := newWindowSettlementControl(t)
	windowSettlementControlProofs(o, start)
	windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(12600*time.Millisecond), 12*time.Second, 12600*time.Millisecond))
	// Clock passage alone cannot turn 600 ms of observed tuples into 1.51 s.
	windowSettlementControlReady(t, o, start.Add(13510*time.Millisecond), false, "wall clock without closing tuple")
	windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 12600*time.Millisecond, 13510*time.Millisecond))
	windowSettlementControlReady(t, o, start.Add(13510*time.Millisecond), true, "exact observed turn")

	o, start, _ = newWindowSettlementControl(t)
	for lane := 0; lane < 7; lane++ {
		windowSettlementControlProof(o, lane, start.Add(10*time.Second), start.Add(12*time.Second))
	}
	windowSettlementControlProof(o, 7, start.Add(10*time.Second), start.Add(12700*time.Millisecond))
	windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 12600*time.Millisecond, 13510*time.Millisecond))
	windowSettlementControlReady(t, o, start.Add(14210*time.Millisecond), false, "eighth owner needs its own closing endpoint")
	windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(14210*time.Millisecond), 12*time.Second, 12600*time.Millisecond, 13510*time.Millisecond, 14210*time.Millisecond))
	windowSettlementControlReady(t, o, start.Add(14210*time.Millisecond), true, "full observed turn after eighth prefix")
}

func TestWindowPathSettlementRejectsAmbiguousOriginalProof(t *testing.T) {
	cases := []struct {
		name string
		edit func(*windowPathSettlementOriginal, *sequenceAck, *Id)
	}{
		{"missing message", func(r *windowPathSettlementOriginal, _ *sequenceAck, _ *Id) { r.Message = Id{} }},
		{"failed physical write", func(r *windowPathSettlementOriginal, _ *sequenceAck, _ *Id) { r.Success = false }},
		{"non-H1 write", func(r *windowPathSettlementOriginal, _ *sequenceAck, _ *Id) { r.H1 = false }},
		{"retried original", func(r *windowPathSettlementOriginal, _ *sequenceAck, _ *Id) { r.Retried = true }},
		{"multiple sends", func(r *windowPathSettlementOriginal, _ *sequenceAck, _ *Id) { r.SendCount = 2 }},
		{"different owner", func(r *windowPathSettlementOriginal, _ *sequenceAck, _ *Id) { r.Sequence = Id{90} }},
		{"selective ACK", func(_ *windowPathSettlementOriginal, a *sequenceAck, _ *Id) { a.selective = true }},
		{"ACK below prefix", func(_ *windowPathSettlementOriginal, a *sequenceAck, _ *Id) { a.sequenceNumber-- }},
		{"ACK before physical write", func(r *windowPathSettlementOriginal, a *sequenceAck, _ *Id) { a.receivedAtNanos = r.PhysicalAtNanos }},
		{"other ACK owner", func(_ *windowPathSettlementOriginal, _ *sequenceAck, id *Id) { *id = Id{91} }},
	}
	for _, test := range cases {
		o, start, _ := newWindowSettlementControl(t)
		for lane := 1; lane < 8; lane++ {
			windowSettlementControlProof(o, lane, start.Add(10*time.Second), start.Add(12*time.Second))
		}
		row := windowSettlementControlOriginal(o, 0, start.Add(10*time.Second))
		ack := sequenceAck{sequenceNumber: row.Number, receivedAtNanos: start.Add(12 * time.Second).UnixNano()}
		ackOwner := row.Sequence
		test.edit(&row, &ack, &ackOwner)
		o.original(1, row)
		o.cumulative(1, ackOwner, ack)
		windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 13510*time.Millisecond))
		windowSettlementControlReady(t, o, start.Add(13510*time.Millisecond), false, test.name)
	}
	// A physical copy at the fence is still old, even with a later good ACK.
	o, start, _ := newWindowSettlementControl(t)
	for lane := range o.reading.Lanes {
		windowSettlementControlProof(o, lane, o.reading.FenceAt, start.Add(12*time.Second))
	}
	windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 13510*time.Millisecond))
	windowSettlementControlReady(t, o, start.Add(13510*time.Millisecond), false, "physical copy at the fence")
}

func TestWindowPathSettlementSameClockBadTupleRevokesQuiet(t *testing.T) {
	for _, badFirst := range []bool{false, true} {
		o, start, _ := newWindowSettlementControl(t)
		windowSettlementControlProofs(o, start)
		s := windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 13510*time.Millisecond)
		if !badFirst {
			windowSettlementControlObserve(o, s)
		}
		bad := s.Samples[1]
		bad.Adjusted, bad.Raw = 1700*time.Millisecond, 1710*time.Millisecond
		if badFirst {
			s.Samples[1], s.Samples[2] = bad, s.Samples[1]
		} else {
			s.Samples[2] = bad
		}
		s.Count = 3
		windowSettlementControlObserve(o, s)
		windowSettlementControlReady(t, o, s.At, false, "bad tuple sharing the newest good clock")
		if !o.snapshot().StableSince.IsZero() {
			t.Fatal("same-clock bad sample preserved the earlier quiet turn")
		}
		windowSettlementControlObserve(o, windowSettlementControlState(start, start.Add(15510*time.Millisecond), 14*time.Second, 15510*time.Millisecond))
		windowSettlementControlReady(t, o, start.Add(15510*time.Millisecond), true, "new complete turn strictly after bad sample")
	}
}

func TestWindowPathSettlementRejectsStaleAndInvalidTuples(t *testing.T) {
	cases := []struct {
		name string
		edit func(*windowPathSettlementState)
	}{
		{"missing baseline", func(s *windowPathSettlementState) { s.BaselineSet = false }},
		{"old path baseline", func(s *windowPathSettlementState) { s.Baseline.Adjusted = time.Millisecond }},
		{"ring overflow", func(s *windowPathSettlementState) { s.Overflow = true }},
		{"legacy newest tuple", func(s *windowPathSettlementState) { s.Samples[1].Adjusted = -1 }},
		{"adjusted below physical path", func(s *windowPathSettlementState) { s.Samples[1].Adjusted = time.Second }},
		{"excess adjusted residence", func(s *windowPathSettlementState) { s.Samples[1].Adjusted = 1501 * time.Millisecond }},
		{"raw below adjusted", func(s *windowPathSettlementState) { s.Samples[1].Raw = time.Second }},
		{"raw over fixed ceiling", func(s *windowPathSettlementState) { s.Samples[1].Raw = 1510*time.Millisecond + 1 }},
	}
	for _, test := range cases {
		o, start, _ := newWindowSettlementControl(t)
		windowSettlementControlProofs(o, start)
		s := windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 13510*time.Millisecond)
		test.edit(&s)
		windowSettlementControlObserve(o, s)
		windowSettlementControlReady(t, o, s.At, false, test.name)
	}
	o, start, _ := newWindowSettlementControl(t)
	windowSettlementControlProofs(o, start)
	s := windowSettlementControlState(start, start.Add(16*time.Second), 12*time.Second, 13510*time.Millisecond)
	windowSettlementControlObserve(o, s)
	windowSettlementControlReady(t, o, s.At, false, "completed but stale sample turn")
	// A sampling gap requires a new turn; the older start cannot bridge it.
	s = windowSettlementControlState(start, start.Add(16*time.Second), 12*time.Second, 16*time.Second)
	windowSettlementControlObserve(o, s)
	windowSettlementControlReady(t, o, s.At, false, "missing paired evidence across a gap")

	o, start, _ = newWindowSettlementControl(t)
	o.mu.Lock()
	o.resetWithLock(start.Add(11*time.Second), "new physical epoch")
	o.mu.Unlock()
	for lane := range o.reading.Lanes {
		windowSettlementControlProof(o, lane, start.Add(11100*time.Millisecond), start.Add(12400*time.Millisecond))
	}
	s = windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 13510*time.Millisecond)
	windowSettlementControlObserve(o, s)
	windowSettlementControlReady(t, o, s.At, false, "first tuple physically predates fence")
}

func TestWindowPathSettlementCompletionNeedsNewOwnerProofs(t *testing.T) {
	cases := []struct {
		name          string
		before, after func(*windowPathSettlementState)
	}{
		{"quality completion", func(s *windowPathSettlementState) { s.QualityPending = true }, func(s *windowPathSettlementState) { s.QualityPending = false }},
		{"drain completion", func(s *windowPathSettlementState) { s.DrainEpoch = true }, func(s *windowPathSettlementState) { s.DrainEpoch = false }},
		{"probe completion", func(s *windowPathSettlementState) { s.ProbeAt, s.ProbePending = s.At.Add(-time.Second), true }, func(s *windowPathSettlementState) { s.ProbeAt, s.ProbePending = time.Time{}, false }},
		{"older epoch published later", func(*windowPathSettlementState) {}, func(s *windowPathSettlementState) { s.EpochAt = s.EpochAt.Add(-time.Second) }},
	}
	for _, test := range cases {
		o, start, _ := newWindowSettlementControl(t)
		s := windowSettlementControlState(start, start.Add(12*time.Second), 12*time.Second)
		test.before(&s)
		windowSettlementControlObserve(o, s)
		// These proofs precede completion even if its carried epoch clock is old.
		for lane := range o.reading.Lanes {
			windowSettlementControlProof(o, lane, start.Add(11500*time.Millisecond), start.Add(12500*time.Millisecond))
		}
		s.At = start.Add(13 * time.Second)
		test.after(&s)
		windowSettlementControlObserve(o, s)
		got := o.snapshot()
		if got.FenceAt != s.At || got.Resets == 0 {
			t.Fatalf("%s did not fence the observed completion: %+v", test.name, got)
		}
		for lane, row := range got.Lanes {
			if row.Original.Message != (Id{}) {
				t.Errorf("%s reused pre-completion lane %d", test.name, lane+1)
			}
		}
		// Copying an older publication back cannot erase the completion fence.
		older := s
		older.At = start.Add(12500 * time.Millisecond)
		windowSettlementControlObserve(o, older)
		if o.snapshot() != got {
			t.Fatalf("%s accepted an older publication", test.name)
		}
	}
}

func TestWindowPathSettlementAdmissionNeverRestarts(t *testing.T) {
	o, start, _ := newWindowSettlementControl(t)
	windowSettlementControlProofs(o, start)
	s := windowSettlementControlState(start, start.Add(13510*time.Millisecond), 12*time.Second, 13510*time.Millisecond)
	windowSettlementControlObserve(o, s)
	windowSettlementControlReady(t, o, s.At, true, "initial measurement admission")
	admitted := o.snapshot().AdmittedAt
	s.At, s.RecoveryAt = start.Add(14*time.Second), start.Add(14*time.Second)
	windowSettlementControlObserve(o, s)
	ready, err := o.admit(s.At)
	if ready || err == nil || !strings.Contains(err.Error(), "after measurement admission") {
		t.Fatalf("later recovery did not fail the existing measurement: ready=%t error=%v", ready, err)
	}
	s.At, s.Count = start.Add(18*time.Second), 0
	windowSettlementControlObserve(o, s)
	if ready, nextErr := o.admit(s.At); ready || nextErr == nil || nextErr.Error() != err.Error() || o.snapshot().AdmittedAt != admitted {
		t.Fatal("later evidence reset or replaced the failed measurement")
	}

	o, _, _ = newWindowSettlementControl(t)
	if ready, err := o.admit(o.snapshot().Deadline.Add(-time.Nanosecond)); ready || err != nil {
		t.Fatal("unsettled observer failed before its fixed deadline")
	}
	if ready, err := o.admit(o.snapshot().Deadline); ready || err == nil || !strings.Contains(err.Error(), "settlement deadline") {
		t.Fatal("unsettled observer did not fail at its fixed deadline")
	}
}

func TestWindowPathSettlementRawCopyIsPureAndOwned(t *testing.T) {
	o, start, _ := newWindowSettlementControl(t)
	service := newWindowPacingService(DefaultSendBufferSettings())
	for i := 0; i < 3; i++ {
		service.observeReceiverRoundTrip(1, 1210*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, start.Add(time.Duration(12+i)*time.Second))
	}
	if service.receiverRoundTrips.count != 3 || service.roundTripStats.ring == nil {
		t.Fatal("actual paired observations did not populate both rings")
	}
	beforeReceiver := *service.receiverRoundTrips
	beforeReceiver.samples = append([]windowReceiverRoundTripSample(nil), beforeReceiver.samples...)
	beforeStats := service.roundTripStats
	rawBuckets := *beforeStats.ring
	rawBuckets.buckets = append([]windowStatsBucket(nil), rawBuckets.buckets...)
	beforeStats.ring = &rawBuckets
	beforeSamples, beforeTotal, beforeHold := service.samples, service.total, service.serviceHoldRate
	o.service = service
	o.capture(start.Add(1000 * time.Second)) // A mutating getter would expire/advance the old rings.
	if !reflect.DeepEqual(beforeReceiver, *service.receiverRoundTrips) || !reflect.DeepEqual(beforeStats, service.roundTripStats) ||
		beforeSamples != service.samples || beforeTotal != service.total || beforeHold != service.serviceHoldRate {
		t.Fatal("settlement observation changed production timing, delivery, or retained state")
	}
	state := windowPathCopySettlementState(service, start.Add(1000*time.Second))
	if state.Count != 3 || state.Samples[0].Raw != 1210*time.Millisecond {
		t.Fatal("raw copy lost stored tuples instead of preserving their ages")
	}
	state.Samples[0].Raw = -1
	if service.receiverRoundTrips.samples[service.receiverRoundTrips.tail].raw != 1210*time.Millisecond {
		t.Fatal("published tuple retained mutable production storage")
	}
	reading := o.snapshot()
	reading.LastState.Samples[0].Raw = -2
	if o.snapshot().LastState.Samples[0].Raw != 1210*time.Millisecond {
		t.Fatal("reading retained mutable observer storage")
	}
}

func TestWindowPathSettlementDefaultAndCachePoliciesStaySeparate(t *testing.T) {
	t.Setenv("CONNECT_WINDOW_PACING_TRACE", "")
	o, start, cell := newWindowSettlementControl(t)
	if ready, err := windowPathMeasurementReady(nil, start); !ready || err != nil {
		t.Fatal("default measurement acquired a settlement dependency")
	}
	key, shared := windowPathTrialKey(cell, 3*time.Second)
	if !shared {
		t.Fatal("settled trial did not produce an explicit cache key")
	}
	same := cell
	policy := *cell.Settlement
	same.Settlement = &policy
	if other, ok := windowPathTrialKey(same, 3*time.Second); !ok || other != key {
		t.Fatal("identical copied settlement policy did not share its cache entry")
	}
	cases := []struct {
		name string
		edit func(*windowPathCell)
	}{
		{"default", func(c *windowPathCell) { c.Settlement = nil }},
		{"different grid", func(c *windowPathCell) { c.MeasurementGrid = false }},
		{"version", func(c *windowPathCell) { c.Settlement.Version += "-changed" }},
		{"reference bytes", func(c *windowPathCell) { c.Settlement.ReferenceBytes++ }},
		{"reference span", func(c *windowPathCell) { c.Settlement.ReferenceDuration++ }},
		{"capacity allowance", func(c *windowPathCell) { c.Settlement.CapBytes++ }},
		{"absolute deadline", func(c *windowPathCell) { c.Settlement.AckLifetime++ }},
	}
	for _, test := range cases {
		changed, value := cell, *cell.Settlement
		changed.Settlement = &value
		test.edit(&changed)
		if other, ok := windowPathTrialKey(changed, 3*time.Second); !ok || other == key {
			t.Errorf("changed %s shared the approved reading", test.name)
		}
	}
	if o.snapshot().Deadline != start.Add(cell.RoundTripChangeAfter+cell.Settlement.AckLifetime) || o.snapshot().NominalAt != start.Add(12*time.Second) {
		t.Fatal("settlement moved the fixed nominal origin or absolute deadline")
	}
}
