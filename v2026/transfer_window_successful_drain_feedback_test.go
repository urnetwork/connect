// A controlled drain can complete correctly after a partial ACK turn has
// already repriced a feedback gap as service. Physical proof must preserve the
// known serialization rate under each forced controller-read ordering.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Values captured at the forced read, dispatch and physical confirmation.
type successfulDrainRateReading struct {
	Initial, During, Probe, Confirmed ByteCount
	Total                             ByteCount
	Pause, FirstRead, Resumed         time.Duration
}

// The observed long-model geometry is reduced to eight physical tails and
// two compressed replies. No byte is offered while the FIFO is paused, and
// the same exact cumulative replies eventually prove all eight tails drained.
// Only the controller read between partial replies varies between controls.
func measureSuccessfulDrainPartialRead(t *testing.T, metadata, read, retain bool) successfulDrainRateReading {
	t.Helper()
	var result successfulDrainRateReading
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		service := newWindowPacingService(DefaultSendBufferSettings())
		if metadata {
			service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, origin)
		} else {
			service.observeRoundTrip(10300*time.Microsecond, 10*time.Millisecond, origin)
		}
		service.observe(2673, origin)
		time.Sleep(10 * time.Millisecond)
		service.observe(125000, time.Now())
		rate, _, latest := service.measured(time.Second, time.Now())
		result.Initial = max(rate, latest)
		if result.Initial != 12500000 {
			t.Fatalf("opening serializer=%d, want 12500000", result.Initial)
		}
		// Outstanding envelopes have been physically offered. Reservations for the
		// next original remain local and cannot certify a network queue.
		const flight ByteCount = 2131032
		service.sent = service.total + flight
		type tail struct{ sequence, message Id }
		var tails [8]tail
		for i := range tails {
			tails[i] = tail{sequence: NewId(), message: NewId()}
			service.beginWrite(tails[i].sequence, tails[i].message, 2, time.Now(), false)
			service.finishWrite(tails[i].sequence, tails[i].message, true)
		}
		if metadata {
			time.Sleep(591170080 * time.Nanosecond)
		} else {
			// Legacy drain triggering reads completed RTT buckets. A valid
			// older raw sample establishes that precondition before the
			// partial ACK turn; it credits no additional service bytes.
			time.Sleep(570 * time.Millisecond)
			service.observeRoundTrip(1218914705*time.Nanosecond, 10*time.Millisecond, time.Now())
			time.Sleep(21170080 * time.Nanosecond)
		}
		for i := range tails {
			if i > 0 {
				time.Sleep(213840 * time.Nanosecond)
			}
			at := time.Now()
			// These are newly delivered prefixes, not each sequence's physical tail.
			service.acknowledgeWrite(tails[i].sequence, NewId(), 1, false, 10*time.Millisecond, at)
			service.observe(2673, at)
			if metadata {
				service.observeReceiverRoundTrip(0, 1218914705*time.Nanosecond, 1208914705*time.Nanosecond, 10*time.Millisecond, at)
			} else {
				service.observeRoundTrip(1218914705*time.Nanosecond, 10*time.Millisecond, at)
			}
		}
		pacer := &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 12500000, estimateRate: 12500000}
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); synctest.Wait(); pacer.close() }()
		probe := NewId()
		done := make(chan error, 1)
		go func() { done <- pacer.waitForServiceMessage(ctx, 2673, false, pacer.serviceSequenceId, probe, 1) }()
		synctest.Wait()
		if !time.Now().Before(service.drainUntil) || service.waiterHead != &pacer.waiter || !service.drainServiceEpoch || service.pendingWrites != 8 {
			t.Fatal("fixture did not enter the controlled drain with all physical tails pending")
		}
		result.Pause = service.drainStartedAt.Sub(origin)
		time.Sleep(8503120 * time.Nanosecond)
		service.acknowledgeWrite(tails[0].sequence, NewId(), 1, false, 10*time.Millisecond, time.Now())
		service.observe(16038, time.Now())
		result.FirstRead = time.Since(origin)
		if read {
			rate, _, latest = service.measure(time.Second, time.Now(), retain)
			result.During = max(rate, latest)
		} else {
			result.During = service.serviceHoldRate
		}
		select {
		case <-done:
			t.Fatal("a partial reply released the controlled drain")
		default:
		}
		if !time.Now().Before(service.drainUntil) || !service.drainServiceEpoch || service.drained || !service.roundTripProbe.sentAt.IsZero() {
			t.Fatal("partial read aborted the drain or invented a completed probe")
		}
		// Complete the unchanged physical flight before the configured deadline.
		// Explicit tail acknowledgements, not timer expiry or byte count alone,
		// supply the empty-flight proof and release the reserved original.
		time.Sleep(time.Until(origin.Add(1210300 * time.Microsecond)))
		remaining := flight - 8*2673 - 16038
		for i, item := range tails {
			service.acknowledgeWrite(item.sequence, item.message, 2, false, 10*time.Millisecond, time.Now())
			bytes := remaining / ByteCount(len(tails)-i)
			service.observe(bytes, time.Now())
			remaining -= bytes
		}
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("complete physical tails failed to release reserved original")
		}
		result.Resumed = time.Since(origin)
		if !service.drainUntil.IsZero() || !service.roundTripProbe.resetService || service.roundTripProbe.messageId != probe {
			t.Fatal("successful drain did not produce the exact controlled probe")
		}
		result.Probe = service.roundTripProbe.serviceRate
		service.finishWrite(pacer.serviceSequenceId, probe, true)
		time.Sleep(1200 * time.Millisecond)
		if metadata {
			service.observeReceiverRoundTripForWrite(pacer.serviceSequenceId, probe, 0, 1200*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, time.Now())
		} else {
			service.observeRoundTrip(1200*time.Millisecond, 10*time.Millisecond, time.Now())
		}
		service.acknowledgeWrite(pacer.serviceSequenceId, probe, 1, false, 10*time.Millisecond, time.Now())
		service.observe(2673, time.Now())
		rate, total, latest := service.measured(time.Second, time.Now())
		result.Confirmed, result.Total = max(rate, latest), total
		if !metadata && service.receiverRoundTrips.count != 0 {
			t.Fatal("legacy control acquired receiver timing metadata")
		}
		if !service.roundTripProbe.sentAt.IsZero() || service.roundTrip() != 1200*time.Millisecond {
			t.Fatal("exact confirmed probe failed to refresh the path")
		}
		if total != 2673+125000+flight+2673 {
			t.Fatalf("delivery accounting changed across the drain: %d", total)
		}
	})
	t.Logf("successful-drain metadata=%t read=%t retain=%t initial=%d during=%d probe=%d confirmed=%d total=%d pause=%s first-read=%s resumed=%s", metadata, read, retain, result.Initial, result.During, result.Probe, result.Confirmed, result.Total, result.Pause, result.FirstRead, result.Resumed)
	return result
}

// A real controller read while the drain remains active must not convert
// the feedback gap into lower service and then pass it to the valid probe.
func TestWindowPacingSuccessfulDrainPartialReadKeepsService(t *testing.T) {
	reading := measureSuccessfulDrainPartialRead(t, true, true, true)
	if reading.During != reading.Initial || reading.Probe != reading.Initial || reading.Confirmed != reading.Initial {
		t.Fatalf("successful drain priced the feedback gap: initial=%d during=%d probe=%d confirmed=%d", reading.Initial, reading.During, reading.Probe, reading.Confirmed)
	}
}

// The identical physical schedule without an intermediate controller read
// keeps the measured rate and later confirms the same exact 1.2-second RTT.
func TestWindowPacingSuccessfulDrainWithoutPartialReadControl(t *testing.T) {
	reading := measureSuccessfulDrainPartialRead(t, true, false, true)
	if reading.Confirmed != reading.Initial {
		t.Fatalf("unread control changed service: %+v", reading)
	}
}

// Read-only statistics can display partial arithmetic but must not retain
// it for the later physical probe or change the measured path baseline.
func TestWindowPacingSuccessfulDrainPartialStatsReadControl(t *testing.T) {
	reading := measureSuccessfulDrainPartialRead(t, true, true, false)
	if reading.Confirmed != reading.Initial {
		t.Fatalf("statistics changed retained service: %+v", reading)
	}
}

// An optional receiver timing field cannot hide the same old-peer sampling
// defect. The complete actual drain/probe lifecycle below uses legacy RTT only.
func TestWindowPacingSuccessfulDrainLegacyPartialReadKeepsService(t *testing.T) {
	reading := measureSuccessfulDrainPartialRead(t, false, true, true)
	if reading.During != reading.Initial || reading.Probe != reading.Initial || reading.Confirmed != reading.Initial {
		t.Fatalf("legacy successful drain priced the feedback gap: initial=%d during=%d probe=%d confirmed=%d", reading.Initial, reading.During, reading.Probe, reading.Confirmed)
	}
}

// The same legacy-peer schedule without an intermediate controller read.
func TestWindowPacingSuccessfulDrainLegacyWithoutPartialReadControl(t *testing.T) {
	reading := measureSuccessfulDrainPartialRead(t, false, false, true)
	if reading.Confirmed != reading.Initial {
		t.Fatalf("legacy unread control changed service: %+v", reading)
	}
}

// Legacy-peer statistics must remain observational during a controlled drain.
func TestWindowPacingSuccessfulDrainLegacyPartialStatsReadControl(t *testing.T) {
	reading := measureSuccessfulDrainPartialRead(t, false, true, false)
	if reading.Confirmed != reading.Initial {
		t.Fatalf("legacy statistics changed retained service: %+v", reading)
	}
}
