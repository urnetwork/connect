// The timer census (timer_census.go): with a sample period of 1, each counted
// site records each change once, at its own site, with the duration its Reset
// armed or as a stop. The census is process-wide, so every assertion reads
// only its own site and the distinct horizons it expects, which no unrelated
// goroutine produces.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A change as Record received it.
type timerCensusRecord struct {
	site    TimerCensusSite
	horizon time.Duration
	stop    bool
	count   int
}

// Collects the changes the process's census records while a test runs.
type timerCensusRecorder struct {
	stateLock sync.Mutex
	records   []timerCensusRecord
}

// Installs a recorder as the process's census until the test ends.
func installTimerCensusRecorder(t *testing.T, samplePeriod int) *timerCensusRecorder {
	t.Helper()
	recorder := &timerCensusRecorder{}
	SetTimerCensus(&TimerCensus{
		SamplePeriod: samplePeriod,
		Record:       recorder.record,
	})
	t.Cleanup(func() {
		SetTimerCensus(nil)
	})
	return recorder
}

func (self *timerCensusRecorder) record(site TimerCensusSite, horizon time.Duration, stop bool, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.records = append(self.records, timerCensusRecord{
		site:    site,
		horizon: horizon,
		stop:    stop,
		count:   count,
	})
}

// The records so far that match.
func (self *timerCensusRecorder) matching(match func(record timerCensusRecord) bool) []timerCensusRecord {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	matchingRecords := []timerCensusRecord{}
	for _, record := range self.records {
		if match(record) {
			matchingRecords = append(matchingRecords, record)
		}
	}
	return matchingRecords
}

// The Resets recorded at site that armed their timer horizon from now.
func (self *timerCensusRecorder) resets(site TimerCensusSite, horizon time.Duration) []timerCensusRecord {
	return self.matching(func(record timerCensusRecord) bool {
		return record.site == site && !record.stop && record.horizon == horizon
	})
}

// The stops recorded at site.
func (self *timerCensusRecorder) stops(site TimerCensusSite) []timerCensusRecord {
	return self.matching(func(record timerCensusRecord) bool {
		return record.site == site && record.stop
	})
}

// Requires want Resets at site that armed their timer horizon from now, each
// recorded as one change.
func requireTimerCensusResets(t *testing.T, recorder *timerCensusRecorder, what string, site TimerCensusSite, horizon time.Duration, want int) {
	t.Helper()
	resets := recorder.resets(site, horizon)
	if len(resets) != want {
		t.Fatalf("%s: the census recorded %d Resets at site %d for %s, want %d", what, len(resets), site, horizon, want)
	}
	for _, reset := range resets {
		if reset.count != 1 {
			t.Fatalf("%s: a Reset at site %d was recorded as %d changes, want 1", what, site, reset.count)
		}
	}
}

// With a sample period of 1, every change is recorded as one change, a Reset
// with its horizon and a stop with none.
func TestTimerCensusRecordsEveryChangeWithAPeriodOfOne(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 1)
	const horizon = 41*time.Second + 43*time.Millisecond
	const changeCount = 100
	for range changeCount {
		censusTimerReset(TimerCensusSiteTimer, horizon)
	}
	requireTimerCensusResets(t, recorder, "a Reset", TimerCensusSiteTimer, horizon, changeCount)

	before := len(recorder.stops(TimerCensusSiteSequenceDeadlineTimer))
	censusTimerStop(TimerCensusSiteSequenceDeadlineTimer)
	stops := recorder.stops(TimerCensusSiteSequenceDeadlineTimer)
	if len(stops) != before+1 {
		t.Fatalf("the census recorded %d stops, want %d", len(stops)-before, 1)
	}
	if stop := stops[len(stops)-1]; stop.horizon != 0 || stop.count != 1 {
		t.Fatalf("a stop was recorded as %+v, want no horizon and one change", stop)
	}
}

// A sample period of 4 records about a quarter of the changes, each as 4. The
// bounds are more than 18 standard deviations from the mean.
func TestTimerCensusRecordsASampleAsItsPeriod(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 4)
	const horizon = 47*time.Second + 53*time.Millisecond
	const changeCount = 4096
	for range changeCount {
		censusTimerReset(TimerCensusSiteTimer, horizon)
	}
	resets := recorder.resets(TimerCensusSiteTimer, horizon)
	if len(resets) < changeCount/8 || changeCount/2 < len(resets) {
		t.Fatalf("a period of 4 recorded %d of %d changes, want about a quarter", len(resets), changeCount)
	}
	for _, reset := range resets {
		if reset.count != 4 {
			t.Fatalf("a sampled Reset was recorded as %d changes, want the period, 4", reset.count)
		}
	}
}

// A removed census records nothing.
func TestTimerCensusRemovedRecordsNothing(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 1)
	SetTimerCensus(nil)
	const horizon = 59*time.Second + 61*time.Millisecond
	censusTimerReset(TimerCensusSiteTimer, horizon)
	censusTimerStop(TimerCensusSiteSequenceDeadlineTimer)
	if records := recorder.matching(func(timerCensusRecord) bool { return true }); len(records) != 0 {
		t.Fatalf("a removed census recorded %d changes", len(records))
	}
}

// A sample period must be a positive power of two, and a census must record.
func TestSetTimerCensusRefusesAPeriodThatIsNotAPositivePowerOfTwo(t *testing.T) {
	t.Cleanup(func() {
		SetTimerCensus(nil)
	})
	record := func(TimerCensusSite, time.Duration, bool, int) {}
	setPanics := func(census *TimerCensus) (panicked bool) {
		defer func() {
			panicked = recover() != nil
		}()
		SetTimerCensus(census)
		return false
	}
	for _, period := range []int{0, -1, -4, 3, 6, 48, 1000} {
		if !setPanics(&TimerCensus{SamplePeriod: period, Record: record}) {
			t.Errorf("a sample period of %d was accepted", period)
		}
	}
	if !setPanics(&TimerCensus{SamplePeriod: 64}) {
		t.Error("a census without Record was accepted")
	}
	for _, period := range []int{1, 2, 64, 1024} {
		if setPanics(&TimerCensus{SamplePeriod: period, Record: record}) {
			t.Errorf("a sample period of %d was refused", period)
		}
	}
}

// A reused timer's first wait creates it, an insert the census does not
// count, and each later wait's Reset counts once: under the generic site, or
// under a caller's own.
func TestTimerCensusCountsTheResetOfAReusedTimer(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		const createTimeout = 13*time.Second + 17*time.Millisecond
		const resetTimeout = 19*time.Second + 23*time.Millisecond
		const sitedTimeout = 29*time.Second + 31*time.Millisecond
		var timer *time.Timer
		resetOrCreateTimer(&timer, createTimeout)
		defer timer.Stop()
		requireTimerCensusResets(t, recorder, "the create", TimerCensusSiteTimer, createTimeout, 0)

		resetOrCreateTimer(&timer, resetTimeout)
		requireTimerCensusResets(t, recorder, "a Reset", TimerCensusSiteTimer, resetTimeout, 1)

		resetOrCreateCensusTimer(&timer, sitedTimeout, TimerCensusSiteRouteWriteTimer)
		requireTimerCensusResets(t, recorder, "a sited Reset", TimerCensusSiteRouteWriteTimer, sitedTimeout, 1)
		requireTimerCensusResets(t, recorder, "a sited Reset under the generic site", TimerCensusSiteTimer, sitedTimeout, 0)
	})
}

// A route write that waits for a writable route creates the selector's timer
// at its first wait and resets it at the next, which counts under the route
// write site, for the write's whole timeout.
func TestTimerCensusCountsTheRouteWriteTimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		selector := NewMultiRouteSelector(context.Background(), "synthetic-census", NewNoopLogger(), DestinationId(NewId()), false)
		defer selector.Close()
		// a route nobody reads, so a write waits out its timeout
		selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{make(Route)})
		message := MessagePoolGet(31)
		defer MessagePoolReturn(message)

		const firstTimeout = 7*time.Second + 3*time.Millisecond
		const secondTimeout = 11*time.Second + 5*time.Millisecond
		success, _, err := selector.writeDetailedWithCarrier(context.Background(), message, firstTimeout)
		if success || err != nil {
			t.Fatalf("a write to an unread route = %t, %v; want a timeout", success, err)
		}
		requireTimerCensusResets(t, recorder, "the first wait's create", TimerCensusSiteRouteWriteTimer, firstTimeout, 0)

		success, _, err = selector.writeDetailedWithCarrier(context.Background(), message, secondTimeout)
		if success || err != nil {
			t.Fatalf("a write to an unread route = %t, %v; want a timeout", success, err)
		}
		requireTimerCensusResets(t, recorder, "the second wait", TimerCensusSiteRouteWriteTimer, secondTimeout, 1)
		requireTimerCensusResets(t, recorder, "the second wait under the generic site", TimerCensusSiteTimer, secondTimeout, 0)
	})
}

// A Pack that waits for its handoff creates the sequence's handoff timer at
// its first wait and resets it at the next, which counts under the receive
// handoff site.
func TestTimerCensusCountsTheReceiveHandoffTimer(t *testing.T) {
	// Keep the process-global pool statistics worker outside the bubble.
	MessagePoolReturn(MessagePoolGet(1))
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		const packByteCount = 40
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// the budget fits one Pack, so the next one waits for it
		budget := NewTransferMemoryBudget(packByteCount)
		settings := DefaultReceiveBufferSettingsWithBufferSize(8)
		settings.PackQueueBudget = budget
		newPack := func() *ReceivePack {
			return &ReceivePack{
				MessageByteCount:   packByteCount,
				TransferFrameBytes: make([]byte, packByteCount),
				TransportType:      TransportTypeH1,
			}
		}
		holder := newReceiveSequence(ctx, &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
		defer holder.Close()
		if accepted, err := holder.Pack(newPack(), 0); !accepted || err != nil {
			t.Fatalf("holder Pack = %t, %v", accepted, err)
		}
		waiting := newReceiveSequence(ctx, &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
		defer waiting.Close()
		packTimingOut := func(timeout time.Duration) {
			receivePack := newPack()
			accepted, err := waiting.Pack(receivePack, timeout)
			if accepted || err != nil {
				t.Fatalf("a Pack waiting on a full budget = %t, %v; want a timeout", accepted, err)
			}
			receivePack.messagePoolReturn()
		}

		const firstTimeout = 1300 * time.Millisecond
		const secondTimeout = 2900 * time.Millisecond
		packTimingOut(firstTimeout)
		requireTimerCensusResets(t, recorder, "the first wait's create", TimerCensusSiteReceiveHandoffTimer, firstTimeout, 0)
		packTimingOut(secondTimeout)
		requireTimerCensusResets(t, recorder, "the second wait", TimerCensusSiteReceiveHandoffTimer, secondTimeout, 1)
		requireTimerCensusResets(t, recorder, "the second wait under the generic site", TimerCensusSiteTimer, secondTimeout, 0)

		receivePack := <-holder.packs
		holder.releasePackQueue(receivePack)
		receivePack.messagePoolReturn()
	})
}

// A pending provider receipt puts the receive loop in its delivery wait. The
// loop's first wait creates the delivery timer and every later one resets it,
// as often as the receipt's progress wakes the loop, so the number of waits
// is the scheduler's and TestTimerCensusCountsTheResetOfAReusedTimer pins one
// count per Reset. Here, once the wait takes the next Pack with the receipt
// still pending, the loop waits again, and each Reset counts under the receive
// delivery site, for the wait's timeout: the idle timeout, or the gap timeout
// while a Pack is held.
func TestTimerCensusCountsTheReceiveDeliveryTimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		const idleTimeout = 97 * time.Minute
		const gapTimeout = 53 * time.Minute
		fixture := newReceiptAdmissionFixture(t, 1, func(settings *ReceiveBufferSettings) {
			settings.IdleTimeout = idleTimeout
			settings.GapTimeout = gapTimeout
		})
		waitResets := func(site TimerCensusSite) []timerCensusRecord {
			return recorder.matching(func(record timerCensusRecord) bool {
				return record.site == site && !record.stop &&
					(record.horizon == idleTimeout || record.horizon == gapTimeout)
			})
		}

		fixture.offer(fixture.sequenceId, 0, 0, NewId(), 41000, tcpFlagAck, []byte("synthetic pending predecessor"))
		sequence := fixture.sequence(fixture.sequenceId, 0)
		if sequence == nil || sequence.deliveryQueue == nil || !sequence.deliveryQueue.pending() {
			t.Fatal("the receipt did not leave a pending delivery")
		}
		resetsBefore := len(waitResets(TimerCensusSiteReceiveDeliveryTimer))

		waitTaken := 0
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, _ *decodedPackOwner, stage string) {
			if actual == sequence && stage == "delivery_wait_pack" {
				waitTaken++
			}
		}
		fixture.offer(fixture.sequenceId, 0, 1, NewId(), 41001, tcpFlagAck, []byte("synthetic next DATA"))
		if waitTaken != 1 || !sequence.deliveryQueue.pending() {
			t.Fatalf("the delivery wait took %d Packs with the receipt pending %t, want 1 and still pending", waitTaken, sequence.deliveryQueue.pending())
		}
		resets := waitResets(TimerCensusSiteReceiveDeliveryTimer)
		if len(resets) <= resetsBefore {
			t.Fatalf("the loop waited again after the wait took a Pack with %d more Resets counted, want at least 1", len(resets)-resetsBefore)
		}
		for _, reset := range resets {
			if reset.count != 1 {
				t.Fatalf("a delivery wait Reset was recorded as %d changes, want 1", reset.count)
			}
		}
		if generic := waitResets(TimerCensusSiteTimer); len(generic) != 0 {
			t.Fatalf("%d delivery wait Resets counted under the generic site, want none", len(generic))
		}
	})
}

// The ACK worker resets its compression timer once per new compression
// deadline, and each Reset counts under the ACK compression site, for exactly
// the time left to that deadline.
func TestTimerCensusCountsTheAckCompressTimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		const compressTimeout = 2 * time.Second
		arms := 0
		var armsLock sync.Mutex
		armCount := func() int {
			armsLock.Lock()
			defer armsLock.Unlock()
			return arms
		}
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = compressTimeout
			settings.AckGapWakeSelectiveCount = 0
			settings.afterAckCompressTimerArmForTest = func(receiveSequenceId) {
				armsLock.Lock()
				defer armsLock.Unlock()
				arms += 1
			}
		})
		// the idle head ack leaves at once, and starts the interval
		sequence.prime(t)

		// a selective ACK 300 ms in waits for the rest of the interval
		const firstWait = compressTimeout - 300*time.Millisecond
		time.Sleep(300 * time.Millisecond)
		sequence.updateSelective(2)
		synctest.Wait()
		if armCount() != 1 {
			t.Fatalf("the compression wait armed its timer %d times, want 1", armCount())
		}
		requireTimerCensusResets(t, recorder, "the first compression wait", TimerCensusSiteAckCompressTimer, firstWait, 1)

		// it leaves at the deadline, and one 450 ms into the next interval
		// waits for that interval's rest
		time.Sleep(firstWait)
		if _, ok := sequence.readAck(t, time.Second); !ok {
			t.Fatal("the selective ACK did not leave at its compression deadline")
		}
		const secondWait = compressTimeout - 450*time.Millisecond
		time.Sleep(450 * time.Millisecond)
		sequence.updateSelective(3)
		synctest.Wait()
		if armCount() != 2 {
			t.Fatalf("the compression waits armed the timer %d times, want 2", armCount())
		}
		requireTimerCensusResets(t, recorder, "the second compression wait", TimerCensusSiteAckCompressTimer, secondWait, 1)
	})
}

// A Pack call's wait resets its timer only for a deadline it has not armed,
// and each such Reset counts under the Pack wait site.
func TestTimerCensusCountsThePackWaitTimerForEachNewDeadline(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		const firstWait = 3*time.Second + 7*time.Millisecond
		const secondWait = 5*time.Second + 11*time.Millisecond
		wait := &sendPackWait{}
		defer wait.stop()

		// created for the first deadline, and kept for it
		first := time.Now().Add(firstWait)
		wait.until(first)
		wait.until(first)
		requireTimerCensusResets(t, recorder, "the create and an unchanged deadline", TimerCensusSitePackWaitTimer, firstWait, 0)

		// moved to a new deadline, then armed again for it once it fired
		second := time.Now().Add(secondWait)
		wait.until(second)
		requireTimerCensusResets(t, recorder, "a new deadline", TimerCensusSitePackWaitTimer, secondWait, 1)
		wait.fired()
		wait.until(second)
		requireTimerCensusResets(t, recorder, "the deadline again after a fire", TimerCensusSitePackWaitTimer, secondWait, 2)
	})
}

// Each window pacing wait resets the pacer's timer for its deadline, and each
// Reset counts under the window pacing site, the first wait's included.
func TestTimerCensusCountsEveryWindowPacingWait(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		const firstWait = 3*time.Millisecond + 700*time.Microsecond
		const secondWait = 9*time.Millisecond + 100*time.Microsecond
		service := newWindowPacingService(DefaultSendBufferSettings())
		pacer := &windowBurstPacer{service: service, rate: 10000000, estimateRate: 10000000}
		defer pacer.close()

		start := time.Now()
		if err := pacer.waitUntil(context.Background(), start.Add(firstWait)); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != firstWait {
			t.Fatalf("the pacing wait lasted %s, want %s", elapsed, firstWait)
		}
		requireTimerCensusResets(t, recorder, "the first pacing wait", TimerCensusSiteWindowPacingTimer, firstWait, 1)

		if err := pacer.waitUntil(context.Background(), time.Now().Add(secondWait)); err != nil {
			t.Fatal(err)
		}
		requireTimerCensusResets(t, recorder, "the second pacing wait", TimerCensusSiteWindowPacingTimer, secondWait, 1)
	})
}

// A sequence loop's deadline timer counts a stop when an earlier deadline or
// the loop's end stops a pending timer, and nothing for the first arm, a later
// deadline, or the Reset of a timer that fired, which is an insert.
func TestTimerCensusCountsOnlyTheSequenceDeadlineTimerStops(t *testing.T) {
	recorder := installTimerCensusRecorder(t, 1)
	synctest.Test(t, func(t *testing.T) {
		stopCount := func() int {
			return len(recorder.stops(TimerCensusSiteSequenceDeadlineTimer))
		}
		resetsAtSite := func() []timerCensusRecord {
			return recorder.matching(func(record timerCensusRecord) bool {
				return record.site == TimerCensusSiteSequenceDeadlineTimer && !record.stop
			})
		}
		timer := newSequenceDeadlineTimer()
		before := stopCount()

		// the first arm creates a timer, with nothing to stop
		timer.armBy(time.Now().Add(time.Hour))
		if stops := stopCount() - before; stops != 0 {
			t.Fatalf("the first arm counted %d stops", stops)
		}
		// an earlier deadline stops the pending timer for a new one
		timer.armBy(time.Now().Add(time.Minute))
		if stops := stopCount() - before; stops != 1 {
			t.Fatalf("an earlier deadline counted %d stops, want 1", stops)
		}
		// a later deadline keeps the armed timer
		if timer.armBy(time.Now().Add(2 * time.Hour)) {
			t.Fatal("a later deadline moved the armed timer")
		}
		if stops := stopCount() - before; stops != 1 {
			t.Fatalf("a later deadline counted %d more stops", stops-1)
		}

		// the armed timer fires, and the next arm resets the fired timer
		time.Sleep(time.Minute)
		<-timer.wake
		timer.woke()
		timer.armBy(time.Now().Add(time.Minute))
		if stops := stopCount() - before; stops != 1 {
			t.Fatalf("the Reset of a fired timer counted %d more stops", stops-1)
		}

		// the loop's end stops the pending timer, and a second stop finds
		// none pending
		timer.stop()
		if stops := stopCount() - before; stops != 2 {
			t.Fatalf("the stop of a pending timer counted %d stops, want 1", stops-1)
		}
		timer.stop()
		if stops := stopCount() - before; stops != 2 {
			t.Fatalf("a stop with no pending timer counted %d more stops", stops-2)
		}
		for _, stop := range recorder.stops(TimerCensusSiteSequenceDeadlineTimer) {
			if stop.horizon != 0 || stop.count != 1 {
				t.Fatalf("a stop was recorded as %+v, want no horizon and one change", stop)
			}
		}
		if resets := resetsAtSite(); len(resets) != 0 {
			t.Fatalf("the deadline timer counted %d Resets, want none: it never moves a pending timer", len(resets))
		}
	})
}
