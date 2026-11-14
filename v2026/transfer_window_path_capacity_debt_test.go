// Capacity debt is a fixture liveness allowance, not a bound implied by memory
// ownership. It keeps every fixed-origin payload deficit, including measurement.
package connect

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const windowPathDebtMaxIntervals = 128
const windowPathDebtMinimumReferenceBytes int64 = 90_000_000 / 8

type windowPathDebtInterval struct {
	At             time.Time
	Duration       time.Duration
	DeliveredBytes int64
	ExpectedBytes  int64
	DeficitBytes   int64
	DebtBytes      int64
	Final          bool
}

type windowPathDebtReading struct {
	Origin            time.Time
	ReferenceBytes    int64
	ReferenceDuration time.Duration
	CapBytes          int64
	DebtBytes         int64
	Complete          bool
	Error             string
	Intervals         []windowPathDebtInterval
}

// The minimum of three validated one-second ceiling bins is a conservative
// fixed payload reference. No candidate observation participates in calibration.
func windowPathDebtReference(intervalBytes []int64, intervalDurations []time.Duration) (int64, time.Duration, error) {
	if len(intervalBytes) != 3 || len(intervalDurations) != 3 {
		return 0, 0, fmt.Errorf("capacity debt reference requires three byte/duration bins: bytes=%d durations=%d", len(intervalBytes), len(intervalDurations))
	}
	reference := int64(math.MaxInt64)
	for i := range intervalBytes {
		if intervalDurations[i] != time.Second || intervalBytes[i] < windowPathDebtMinimumReferenceBytes {
			return 0, 0, fmt.Errorf("capacity debt reference bin %d is not one second at >=90 Mbps: bytes=%d duration=%s", i, intervalBytes[i], intervalDurations[i])
		}
		reference = min(reference, intervalBytes[i])
	}
	return reference, time.Second, nil
}

// There is deliberately no readiness input or reset operation. One owner feeds
// cumulative payload counts on the origin's one-second grid, then closes once.
type windowPathCapacityDebt struct {
	origin            time.Time
	referenceBytes    int64
	referenceDuration time.Duration
	capBytes          int64
	debtBytes         int64
	lastAt            time.Time
	lastDelivered     int64
	complete          bool
	err               error
	intervals         [windowPathDebtMaxIntervals]windowPathDebtInterval
	count             int
}

func newWindowPathCapacityDebt(referenceBytes int64, referenceDuration time.Duration, capBytes int64, origin time.Time, delivered int64) (*windowPathCapacityDebt, error) {
	if referenceDuration != time.Second || referenceBytes < windowPathDebtMinimumReferenceBytes || capBytes <= 0 || delivered < 0 {
		return nil, fmt.Errorf("invalid capacity debt inputs: reference=%d/%s cap=%d delivered=%d", referenceBytes, referenceDuration, capBytes, delivered)
	}
	return &windowPathCapacityDebt{
		origin: origin, referenceBytes: referenceBytes, referenceDuration: referenceDuration,
		capBytes: capBytes, lastAt: origin, lastDelivered: delivered,
	}, nil
}

func (self *windowPathCapacityDebt) fail(format string, args ...any) error {
	if self.err == nil {
		self.err = fmt.Errorf(format, args...)
	}
	return self.err
}

func (self *windowPathCapacityDebt) observe(at time.Time, cumulativeDelivered int64, final bool) error {
	if self.err != nil {
		return self.err
	}
	if self.complete {
		return self.fail("capacity debt observation after final close")
	}
	if at.Before(self.lastAt) || cumulativeDelivered < self.lastDelivered {
		return self.fail("capacity debt time or payload regressed: at=%s previous=%s delivered=%d previous=%d", at, self.lastAt, cumulativeDelivered, self.lastDelivered)
	}
	span := at.Sub(self.lastAt)
	if (!final && span != time.Second) || (final && span > time.Second) {
		return self.fail("capacity debt observation missed fixed one-second grid: at=%s previous=%s span=%s final=%t", at, self.lastAt, span, final)
	}
	delivered := cumulativeDelivered - self.lastDelivered
	// A final boundary may coincide with an already-recorded grid tick. Extra
	// callbacks at that same clock are surplus, not repayment of an older bin.
	if final && span == 0 && delivered == 0 {
		self.complete = true
		return nil
	}
	if self.count == len(self.intervals) {
		return self.fail("capacity debt interval bound exceeded: maximum=%d", len(self.intervals))
	}
	// span is in [0, 1s]. Splitting quotient and remainder keeps both products
	// and their sum within int64 even for the largest valid reference. A final
	// fractional-byte entitlement is rounded down once, never accumulated.
	nanos, second := int64(span), int64(time.Second)
	expected := self.referenceBytes/second*nanos + (self.referenceBytes%second*nanos)/second
	deficit := max(int64(0), expected-delivered)
	if deficit > math.MaxInt64-self.debtBytes {
		return self.fail("capacity debt overflow: previous=%d deficit=%d", self.debtBytes, deficit)
	}
	self.debtBytes += deficit
	self.intervals[self.count] = windowPathDebtInterval{
		At: at, Duration: span, DeliveredBytes: delivered, ExpectedBytes: expected,
		DeficitBytes: deficit, DebtBytes: self.debtBytes, Final: final,
	}
	self.count++
	self.lastAt, self.lastDelivered, self.complete = at, cumulativeDelivered, final
	if self.debtBytes > self.capBytes {
		return self.fail("capacity debt cap exceeded: debt=%d cap=%d", self.debtBytes, self.capBytes)
	}
	return nil
}

// Copy the bounded ledger; a report cannot mutate the sampler's history.
func (self *windowPathCapacityDebt) snapshot() windowPathDebtReading {
	reading := windowPathDebtReading{
		Origin: self.origin, ReferenceBytes: self.referenceBytes, ReferenceDuration: self.referenceDuration,
		CapBytes: self.capBytes, DebtBytes: self.debtBytes, Complete: self.complete,
		Intervals: append([]windowPathDebtInterval(nil), self.intervals[:self.count]...),
	}
	if self.err != nil {
		reading.Error = self.err.Error()
	}
	return reading
}

func newWindowPathDebtControl(t *testing.T, reference, capBytes, delivered int64) (*windowPathCapacityDebt, time.Time) {
	t.Helper()
	origin := time.Unix(1_000, 0)
	debt, err := newWindowPathCapacityDebt(reference, time.Second, capBytes, origin, delivered)
	if err != nil {
		t.Fatal(err)
	}
	return debt, origin
}

func TestWindowPathCapacityDebtReferenceCalibration(t *testing.T) {
	bytes := []int64{12_000_000, 11_970_560, 11_973_120}
	durations := []time.Duration{time.Second, time.Second, time.Second}
	reference, duration, err := windowPathDebtReference(bytes, durations)
	if err != nil || reference != 11_970_560 || duration != time.Second {
		t.Fatalf("fixed minimum payload reference: bytes=%d duration=%s error=%v", reference, duration, err)
	}
	invalid := []struct {
		bytes     []int64
		durations []time.Duration
	}{
		{bytes[:2], durations},
		{append(append([]int64(nil), bytes...), 12_000_000), durations},
		{bytes, durations[:2]},
		{bytes, []time.Duration{time.Second, time.Second - time.Nanosecond, time.Second}},
		{[]int64{12_000_000, windowPathDebtMinimumReferenceBytes - 1, 12_000_000}, durations},
		{[]int64{12_000_000, -1, 12_000_000}, durations},
	}
	for i, item := range invalid {
		if _, _, err := windowPathDebtReference(item.bytes, item.durations); err == nil {
			t.Errorf("invalid reference %d was accepted", i)
		}
	}
	if bytes[0] != 12_000_000 || durations[0] != time.Second {
		t.Fatal("reference calibration modified its caller's bins")
	}
}

func TestWindowPathCapacityDebtKeepsDeficitsAcrossMeasurement(t *testing.T) {
	debt, origin := newWindowPathDebtControl(t, 12_000_000, 48*1024*1024, 1_000)
	// These are consecutive debt-clock bins. The harness may admit steady
	// measurement between them; that event neither calls nor resets this owner.
	for i, delivered := range []int64{6_001_000, 24_001_000, 33_001_000, 45_001_000, 57_001_000} {
		if err := debt.observe(origin.Add(time.Duration(i+1)*time.Second), delivered, false); err != nil {
			t.Fatal(err)
		}
	}
	reading := debt.snapshot()
	want := []int64{6_000_000, 6_000_000, 9_000_000, 9_000_000, 9_000_000}
	if len(reading.Intervals) != len(want) || reading.DebtBytes != 9_000_000 {
		t.Fatalf("transient/steady history changed: %+v", reading)
	}
	for i, interval := range reading.Intervals {
		if interval.DebtBytes != want[i] || interval.At != origin.Add(time.Duration(i+1)*time.Second) {
			t.Errorf("bin %d lost debt or fixed origin: %+v", i, interval)
		}
	}
	reading.Intervals[0].DebtBytes = 0
	if debt.snapshot().Intervals[0].DebtBytes != 6_000_000 {
		t.Fatal("published intervals alias the sampler's history")
	}
}

func TestWindowPathCapacityDebtFinalPartialAndSameClock(t *testing.T) {
	debt, origin := newWindowPathDebtControl(t, 12_000_003, math.MaxInt64, 0)
	if err := debt.observe(origin.Add(time.Second), 12_000_003, false); err != nil {
		t.Fatal(err)
	}
	if err := debt.observe(origin.Add(1250*time.Millisecond), 14_000_003, true); err != nil {
		t.Fatal(err)
	}
	reading := debt.snapshot()
	if !reading.Complete || len(reading.Intervals) != 2 || reading.Intervals[1].ExpectedBytes != 3_000_000 ||
		reading.Intervals[1].Duration != 250*time.Millisecond || reading.DebtBytes != 1_000_000 {
		t.Fatalf("final partial entitlement changed: %+v", reading)
	}
	if err := debt.observe(origin.Add(1500*time.Millisecond), 15_000_003, true); err == nil {
		t.Fatal("a second final partial was accepted")
	}
	for _, extra := range []int64{0, 123} {
		debt, origin = newWindowPathDebtControl(t, 12_000_000, math.MaxInt64, 0)
		if err := debt.observe(origin.Add(time.Second), 6_000_000, false); err != nil {
			t.Fatal(err)
		}
		if err := debt.observe(origin.Add(time.Second), 6_000_000+extra, true); err != nil {
			t.Fatal(err)
		}
		reading = debt.snapshot()
		if !reading.Complete || reading.DebtBytes != 6_000_000 || len(reading.Intervals) != 1+int(min(extra, 1)) {
			t.Fatalf("same-clock close repaid or duplicated debt: extra=%d reading=%+v", extra, reading)
		}
		if extra > 0 {
			last := reading.Intervals[1]
			if last.Duration != 0 || last.ExpectedBytes != 0 || last.DeliveredBytes != extra || last.DeficitBytes != 0 {
				t.Fatalf("same-clock callback bytes changed entitlement: %+v", last)
			}
		}
	}
}

func TestWindowPathCapacityDebtCapIsStrictAndFailureIsSticky(t *testing.T) {
	debt, origin := newWindowPathDebtControl(t, 12_000_000, 3_000_000, 0)
	if err := debt.observe(origin.Add(time.Second), 9_000_000, false); err != nil {
		t.Fatalf("exact cap was rejected: %v", err)
	}
	if err := debt.observe(origin.Add(2*time.Second), 21_000_000, false); err != nil {
		t.Fatal(err)
	}
	first := debt.observe(origin.Add(3*time.Second), 32_999_999, false)
	if first == nil || !strings.Contains(first.Error(), "cap exceeded") {
		t.Fatalf("one byte above cap did not fail: %v", first)
	}
	before := debt.snapshot()
	if before.DebtBytes != 3_000_001 || len(before.Intervals) != 3 || before.Error == "" {
		t.Fatalf("cap failure lost its offending bin: %+v", before)
	}
	if err := debt.observe(origin.Add(4*time.Second), 60_000_000, true); err != first {
		t.Fatalf("later surplus changed the first failure: before=%v after=%v", first, err)
	}
	if after := debt.snapshot(); after.DebtBytes != before.DebtBytes || len(after.Intervals) != len(before.Intervals) || after.Complete {
		t.Fatalf("failed ledger resumed or discarded history: %+v", after)
	}
}

func TestWindowPathCapacityDebtBoundsUnsettledZeroDelivery(t *testing.T) {
	debt, origin := newWindowPathDebtControl(t, 12_000_000, 48*1024*1024, 0)
	for second := 1; second <= 4; second++ {
		if err := debt.observe(origin.Add(time.Duration(second)*time.Second), 0, false); err != nil {
			t.Fatalf("outage failed before the declared cap: second=%d error=%v", second, err)
		}
	}
	if err := debt.observe(origin.Add(5*time.Second), 0, false); err == nil || !strings.Contains(err.Error(), "cap exceeded") {
		t.Fatalf("unsettled outage escaped debt cap: %v", err)
	}
	if reading := debt.snapshot(); reading.DebtBytes != 60_000_000 || len(reading.Intervals) != 5 {
		t.Fatalf("unsettled outage was not charged from its original boundary: %+v", reading)
	}
}

func TestWindowPathCapacityDebtRejectsInvalidClockAndPayload(t *testing.T) {
	origin := time.Unix(1_000, 0)
	for i, values := range []struct {
		reference int64
		duration  time.Duration
		cap       int64
		delivered int64
	}{
		{windowPathDebtMinimumReferenceBytes - 1, time.Second, 1, 0},
		{12_000_000, 0, 1, 0},
		{12_000_000, 2 * time.Second, 1, 0},
		{12_000_000, time.Second, 0, 0},
		{12_000_000, time.Second, 1, -1},
	} {
		if _, err := newWindowPathCapacityDebt(values.reference, values.duration, values.cap, origin, values.delivered); err == nil {
			t.Errorf("invalid constructor inputs %d were accepted", i)
		}
	}
	for i, item := range []struct {
		elapsed   time.Duration
		delivered int64
		final     bool
	}{
		{-time.Nanosecond, 1_000, false},
		{0, 1_000, false},
		{time.Second - time.Nanosecond, 1_000, false},
		{2 * time.Second, 1_000, false},
		{time.Second + time.Nanosecond, 1_000, true},
		{time.Second, 999, false},
	} {
		debt, origin := newWindowPathDebtControl(t, 12_000_000, math.MaxInt64, 1_000)
		if err := debt.observe(origin.Add(item.elapsed), item.delivered, item.final); err == nil {
			t.Errorf("invalid observation %d was accepted", i)
		}
		if reading := debt.snapshot(); len(reading.Intervals) != 0 || reading.DebtBytes != 0 || reading.Error == "" {
			t.Errorf("invalid observation %d changed accounting: %+v", i, reading)
		}
	}
}

func TestWindowPathCapacityDebtArithmeticAndHistoryBounds(t *testing.T) {
	debt, origin := newWindowPathDebtControl(t, math.MaxInt64, math.MaxInt64, 0)
	if err := debt.observe(origin.Add(500*time.Millisecond), 0, true); err != nil {
		t.Fatal(err)
	}
	if got := debt.snapshot().DebtBytes; got != math.MaxInt64/2 {
		t.Fatalf("large-reference partial arithmetic overflowed: %d", got)
	}
	debt, origin = newWindowPathDebtControl(t, math.MaxInt64, math.MaxInt64, 0)
	if err := debt.observe(origin.Add(time.Second), 0, false); err != nil {
		t.Fatal(err)
	}
	if err := debt.observe(origin.Add(2*time.Second), 0, false); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("cumulative debt overflow was accepted: %v", err)
	}
	if reading := debt.snapshot(); reading.DebtBytes != math.MaxInt64 || len(reading.Intervals) != 1 {
		t.Fatalf("overflow corrupted the last valid ledger: %+v", reading)
	}
	debt, origin = newWindowPathDebtControl(t, 12_000_000, math.MaxInt64, 0)
	for i := 1; i <= windowPathDebtMaxIntervals; i++ {
		if err := debt.observe(origin.Add(time.Duration(i)*time.Second), int64(i)*12_000_000, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := debt.observe(origin.Add((windowPathDebtMaxIntervals+1)*time.Second), (windowPathDebtMaxIntervals+1)*12_000_000, true); err == nil || !strings.Contains(err.Error(), "interval bound") {
		t.Fatalf("bounded history silently discarded intervals: %v", err)
	}
	if reading := debt.snapshot(); reading.DebtBytes != 0 || len(reading.Intervals) != windowPathDebtMaxIntervals {
		t.Fatalf("history overflow changed prior intervals: %+v", reading)
	}
}
