// Fixed history tests pin metadata provenance, lifecycle and allocation bounds.
package connect

import (
	"math"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// Neither a later hint nor independent minima may reprice the observed tuple.
func TestWindowPacingReceiverTimingPinsCompressionToItsSample(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 100*time.Millisecond, time.Millisecond, 0, at)
	service.observeReceiverRoundTrip(2, 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, at.Add(time.Millisecond))
	service.observeRoundTrip(5*time.Millisecond, 80*time.Millisecond, at.Add(2*time.Millisecond))
	minimum, residence, sampled := service.receiverWindowEstimate(at.Add(2 * time.Millisecond))
	if !sampled || minimum != time.Millisecond || residence != 30*time.Millisecond {
		t.Fatalf("historical tuple was repriced or unrelated minima combined: min=%s residence=%s sampled=%t", minimum, residence, sampled)
	}
}

// Absence does not become a sticky capability mode. Ordinary legacy samples
// consume count lifetime; an age-only statistics read does not erase history.
func TestWindowPacingReceiverTimingExpiresWithoutModeState(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.RttWindowSize, settings.RttWindowTimeout = 4, time.Second
	service := newWindowPacingService(settings)
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	before := *service.receiverRoundTrips
	before.samples = append([]windowReceiverRoundTripSample(nil), before.samples...)
	if _, _, sampled := service.receiverWindowEstimate(at.Add(time.Second)); !sampled {
		t.Fatal("sample expired at the still-inclusive lifetime boundary")
	}
	if _, _, sampled := service.receiverWindowEstimate(at.Add(time.Second + time.Nanosecond)); sampled {
		t.Fatal("expired metadata stayed active")
	}
	if !reflect.DeepEqual(before, *service.receiverRoundTrips) {
		t.Fatal("statistics expiration mutated the controller's history")
	}
	if _, _, sampled := service.receiverWindowEstimate(at); !sampled {
		t.Fatal("an expired statistics read erased the retained observation")
	}
	for i := 1; i <= settings.RttWindowSize; i++ {
		now := at.Add(time.Duration(i) * time.Millisecond)
		service.observeRoundTrip(30*time.Millisecond, 0, now)
		_, _, sampled := service.receiverWindowEstimate(now)
		if sampled != (i < settings.RttWindowSize) {
			t.Errorf("after %d legacy samples, metadata sampled=%t", i, sampled)
		}
	}
	if service.receiverRoundTrips.count != settings.RttWindowSize || len(service.receiverRoundTrips.samples) != settings.RttWindowSize {
		t.Fatal("mixed feedback escaped the configured storage bound")
	}
}

// New receiver measurements replace expired RTT evidence while raw fallback
// remains unchanged; no idle drain or new timeout is required.
func TestWindowPacingReceiverTimingAgesDuringContinuousLegacyFeedback(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.RttWindowTimeout = time.Second
	service := newWindowPacingService(settings)
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	service.observeRoundTrip(time.Second, 10*time.Millisecond, at.Add(900*time.Millisecond))
	at = at.Add(time.Second + time.Nanosecond)
	service.observeReceiverRoundTrip(2, 1210*time.Millisecond, 1200*time.Millisecond, 10*time.Millisecond, at)
	minimum, residence, sampled := service.receiverWindowEstimate(at)
	if !sampled || minimum != 1200*time.Millisecond || residence != 1210*time.Millisecond {
		t.Fatalf("expired short metadata still dominated the live receiver sample: %s/%s/%t", minimum, residence, sampled)
	}
	if service.minRoundTrip != 10300*time.Microsecond {
		t.Fatal("metadata integration changed the raw legacy floor")
	}
}

// Zero adjusted RTT is measured evidence. Invalid pairs do not overwrite it,
// and a large legal residence saturates rather than wrapping into the past.
func TestWindowPacingReceiverTimingValidatesPairsAndSaturates(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 25*time.Millisecond, 0, 0, at)
	for _, test := range []struct{ raw, adjusted time.Duration }{
		{raw: -1, adjusted: 0}, {raw: time.Millisecond, adjusted: -1},
		{raw: time.Millisecond, adjusted: 2 * time.Millisecond},
	} {
		service.observeReceiverRoundTrip(2, test.raw, test.adjusted, time.Second, at.Add(time.Millisecond))
	}
	minimum, residence, sampled := service.receiverWindowEstimate(at.Add(time.Millisecond))
	if !sampled || minimum != 0 || residence != 25*time.Millisecond {
		t.Fatalf("invalid or zero timing was misclassified: %s/%s/%t", minimum, residence, sampled)
	}
	if got := windowReceiverRoundTripResidence(time.Duration(math.MaxInt64), time.Duration(math.MaxInt64-1), 2); got != time.Duration(math.MaxInt64) {
		t.Fatalf("effective residence overflowed: %s", got)
	}
}

// Allocate one fixed ring on first metadata and reuse it for observation and
// readout. Legacy-only services do not allocate the ring.
func TestWindowPacingReceiverTimingHasFixedStorageAndNoHotAllocations(t *testing.T) {
	settings := DefaultSendBufferSettings()
	service := newWindowPacingService(settings)
	at := time.Unix(1700000000, 0)
	service.observeRoundTrip(time.Millisecond, 0, at)
	if service.receiverRoundTrips.samples != nil {
		t.Fatal("legacy feedback allocated a receiver timing ring")
	}
	service.observeReceiverRoundTrip(1, 25*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	for i := 0; i < settings.RttWindowSize; i++ {
		at = at.Add(time.Millisecond)
		service.observeReceiverRoundTrip(1, 25*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	}
	allocations := testing.AllocsPerRun(1000, func() {
		at = at.Add(time.Microsecond)
		service.observeReceiverRoundTrip(1, 25*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
		service.receiverWindowEstimate(at)
	})
	if allocations != 0 {
		t.Fatalf("receiver timing allocates on the hot path: %g", allocations)
	}
	if service.receiverRoundTrips.count != settings.RttWindowSize || cap(service.receiverRoundTrips.samples) != settings.RttWindowSize {
		t.Fatal("observations grew the configured ring")
	}
	t.Logf("service=%d history=%d sample=%d ring=%d sendItem=%d sequenceAck=%d", unsafe.Sizeof(*service), unsafe.Sizeof(*service.receiverRoundTrips), unsafe.Sizeof(windowReceiverRoundTripSample{}), cap(service.receiverRoundTrips.samples)*int(unsafe.Sizeof(windowReceiverRoundTripSample{})), unsafe.Sizeof(sendItem{}), unsafe.Sizeof(sequenceAck{}))
}

// Metadata authority ends with its ordinary lifetime. The compatibility
// path's bounded drain inference remains available after metadata expires.
func TestWindowPacingReceiverTimingExpiryRestoresLegacyDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.RttWindowTimeout = time.Second
		service := newWindowPacingService(settings)
		service.observeRoundTrip(time.Millisecond, 0, time.Now())
		for i := 0; i < 6; i++ {
			time.Sleep(10 * time.Millisecond)
			service.observeReceiverRoundTrip(1, 101*time.Millisecond, time.Millisecond, 0, time.Now())
		}
		time.Sleep(time.Second + time.Nanosecond)
		service.pendingWrites = 1
		service.burstMeter.update(time.Now(), 1000, 1000000)
		wait, _ := service.admitBurst(time.Now(), 100, false, &windowPacingWaiter{})
		if wait != 202*time.Millisecond {
			t.Fatalf("expired metadata left a sticky drain bypass: %s", wait)
		}
	})
}
