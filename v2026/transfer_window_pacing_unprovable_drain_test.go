// Controlled drains need a physical tail whose eventual reply can prove delivery.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A twenty-second stall supplies real residence evidence, but a copied tail
// cannot establish which physical copy a reply covers. Force that state before
// or during the actual FIFO wait, without changing service or recovery clocks.
func testWindowPacingUnprovableDrain(t *testing.T, event string, before bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		service := &windowPacingService{drainMaximumTime: time.Minute, sent: 1000}
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(20256*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequence, tail, successor := NewId(), NewId(), NewId()
		service.beginWrite(sequence, tail, 1, start, false)
		if event != "carrier" {
			service.finishWrite(sequence, tail, true)
		}
		if event == "cancel" {
			// An older sibling remains physically outstanding after cancellation.
			sibling, message := NewId(), NewId()
			service.beginWrite(sibling, message, 1, start, false)
			service.finishWrite(sibling, message, true)
		}
		invalidate := func() {
			switch event {
			case "retry":
				service.beginWrite(sequence, tail, 1, time.Now(), true)
				service.finishWrite(sequence, tail, true)
				service.acknowledgeWrite(sequence, tail, 1, false, 0, time.Now())
				service.observe(1000, time.Now())
			case "invalidate":
				service.invalidateMessageProbe(sequence, tail)
			case "carrier":
				service.finishWrite(sequence, tail, false)
			case "cancel":
				owner := &windowBurstPacer{service: service, serviceSequenceId: sequence}
				owner.close()
			default:
				t.Fatalf("unknown event %q", event)
			}
		}
		if before {
			invalidate()
		}
		pacer := &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 1000000}
		ctx, cancel := context.WithCancel(context.Background())
		defer func() {
			cancel()
			synctest.Wait()
			pacer.close()
		}()
		type result struct {
			at  time.Time
			err error
		}
		done := make(chan result, 1)
		go func() {
			err := pacer.waitForServiceMessage(ctx, 1000, false, pacer.serviceSequenceId, successor, 2)
			done <- result{at: time.Now(), err: err}
		}()
		synctest.Wait()
		if !before {
			if service.drainUntil != start.Add(40512*time.Millisecond) || service.waiterHead != &pacer.waiter {
				t.Fatal("fixture did not enter the physical-tail drain")
			}
			time.Sleep(time.Millisecond)
			invalidate()
			synctest.Wait()
		}
		select {
		case got := <-done:
			if got.err != nil || got.at != time.Now() {
				t.Fatalf("event=%s before=%t: admission=%+v", event, before, got)
			}
		default:
			t.Fatalf("event=%s before=%t: unprovable drain retained FIFO head for %s", event, before, time.Until(service.drainUntil))
		}
		if service.drained || !service.roundTripProbe.sentAt.IsZero() || service.minRoundTrip != time.Millisecond {
			t.Fatalf("event=%s before=%t: abandoning a measurement invented drained RTT evidence", event, before)
		}
		if !service.drainUntil.IsZero() || service.drainServiceEpoch {
			t.Fatal("abandoned drain retained a timer or a service epoch reset")
		}
	})
}

// This is the silent-lane failure: a retry's logical delivery cannot satisfy
// the physical proof that a subsequent forty-second drain would require.
func TestWindowPacingRetriedTailCannotStartDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "retry", true)
}

// A shared sibling retry can invalidate the tail while the FIFO head sleeps.
func TestWindowPacingRetriedTailWakesDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "retry", false)
}

// The common recovery boundary may bypass H1 pacing after a route change.
func TestWindowPacingInvalidatedTailCannotStartDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "invalidate", true)
}

// The waiter must see lost proof without waiting for its original timer.
func TestWindowPacingInvalidatedTailWakesDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "invalidate", false)
}

// A failed write or another carrier cannot supply the promised H1 tail proof.
func TestWindowPacingUnconfirmedCarrierCannotStartDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "carrier", true)
}

// Writer confirmation may race with a sibling already waiting for that tail.
func TestWindowPacingUnconfirmedCarrierWakesDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "carrier", false)
}

// An older sibling cannot certify the physical bytes abandoned by cancellation.
func TestWindowPacingCanceledTailCannotStartDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "cancel", true)
}

// Removing logical ownership is not delivery, but can end an impossible pause.
func TestWindowPacingCanceledTailWakesDrain(t *testing.T) {
	testWindowPacingUnprovableDrain(t, "cancel", false)
}

// A later original tail can replace ambiguous physical state. Its cumulative
// reply restores normal empty-flight probing instead of disabling it forever.
func TestWindowPacingFreshTailRestoresDrainedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		service := &windowPacingService{}
		sequence, copied, fresh, probe := NewId(), NewId(), NewId(), NewId()
		service.observeRoundTrip(time.Millisecond, 0, start)
		service.beginWrite(sequence, copied, 1, start, true)
		service.finishWrite(sequence, copied, true)
		service.acknowledgeWrite(sequence, copied, 1, false, 0, start)
		if service.drained || service.pendingWrites != 1 {
			t.Fatal("a copied tail supplied physical drain proof")
		}
		service.beginWrite(sequence, fresh, 2, start, false)
		service.finishWrite(sequence, fresh, true)
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(sequence, fresh, 2, false, 0, time.Now())
		if !service.drained || service.pendingWrites != 0 {
			t.Fatal("a fresh original tail failed to restore physical delivery proof")
		}
		service.beginWrite(sequence, probe, 3, time.Now(), false)
		service.finishWrite(sequence, probe, true)
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(sequence, probe, 3, false, 0, time.Now())
		if got := service.roundTrip(); got != 100*time.Millisecond {
			t.Fatalf("subsequent unqueued probe lost its measured floor: %s", got)
		}
	})
}

// Losing a later tail's drain proof does not duplicate an older unqueued
// probe; its own exact reply must still refresh the physical RTT baseline.
func TestWindowPacingAbortedDrainPreservesUnrelatedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		service := &windowPacingService{drainMaximumTime: time.Minute, drained: true}
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(20256*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		probeSequence, probe := NewId(), NewId()
		service.beginWrite(probeSequence, probe, 1, start, false)
		service.finishWrite(probeSequence, probe, true)
		tailSequence, tail := NewId(), NewId()
		service.beginWrite(tailSequence, tail, 1, start, false)
		service.finishWrite(tailSequence, tail, true)
		pacer := &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 1000000}
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); synctest.Wait(); pacer.close() }()
		done := make(chan error, 1)
		go func() {
			done <- pacer.waitForServiceMessage(ctx, 1000, false, pacer.serviceSequenceId, NewId(), 1)
		}()
		synctest.Wait()
		if service.drainUntil.IsZero() || service.roundTripProbe.messageId != probe || !service.roundTripProbe.written {
			t.Fatal("fixture lacks a sleeping drain and an independent confirmed probe")
		}
		service.invalidateMessageProbe(tailSequence, tail)
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("unprovable drain retained its waiter")
		}
		if service.roundTripProbe.messageId != probe || !service.roundTripProbe.written {
			t.Fatal("abandoned drain erased an unrelated unambiguous probe")
		}
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(probeSequence, probe, 1, false, 0, time.Now())
		if got := service.roundTrip(); got != 100*time.Millisecond {
			t.Fatalf("unrelated exact probe lost its measured baseline: %s", got)
		}
	})
}

// Repeated recovery and writer notifications describe one ambiguous tail.
// Replacing it with a fresh original must permit the next controlled drain.
func TestWindowPacingRepeatedTailInvalidationAllowsFreshDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		service := &windowPacingService{sent: 1000}
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(100*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequence, copied, fresh, probe := NewId(), NewId(), NewId(), NewId()
		service.beginWrite(sequence, copied, 1, start, true)
		for range 3 {
			service.invalidateMessageProbe(sequence, copied)
			service.finishWrite(sequence, copied, false)
		}
		service.beginWrite(sequence, fresh, 2, start, false)
		service.finishWrite(sequence, fresh, true)
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequence, rate: 1000000}
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); synctest.Wait(); pacer.close() }()
		done := make(chan error, 1)
		go func() { done <- pacer.waitForServiceMessage(ctx, 1000, false, sequence, probe, 3) }()
		synctest.Wait()
		if service.drainUntil != start.Add(200*time.Millisecond) || service.waiterHead != &pacer.waiter {
			t.Fatal("repeated invalidation disabled a fresh tail's controlled drain")
		}
		time.Sleep(10 * time.Millisecond)
		service.acknowledgeWrite(sequence, fresh, 2, false, 0, time.Now())
		service.observe(1000, time.Now())
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("fresh tail's cumulative reply did not release the drain")
		}
		service.finishWrite(sequence, probe, true)
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(sequence, probe, 3, false, 0, time.Now())
		if got := service.roundTrip(); got != 100*time.Millisecond {
			t.Fatalf("restored controlled probe lost its baseline: %s", got)
		}
	})
}
