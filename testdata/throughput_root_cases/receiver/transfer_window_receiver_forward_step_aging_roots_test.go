// Receiver ingress still includes a forward propagation discontinuity. A
// subsequent physical pair wholly on the new path must supply service evidence.
package connect

import (
	"testing"
	"time"
)

// Original offers precede the old ACK, so neither source-idle qualification
// nor an invented local wait may discard this continuously offered interval.
func TestWindowReceiverIngressForwardStepKeepsServiceUntilPostStepPair(t *testing.T) {
	for _, test := range []struct {
		name          string
		serialization time.Duration
		want          ByteCount
	}{
		{name: "same capacity", serialization: 213840 * time.Nanosecond, want: 12500000},
		{name: "real decrease", serialization: 2138400 * time.Nanosecond, want: 1250000},
	} {
		fixture := newReceiverPhysicalSpanFixture(t, 12500000)
		sequence := fixture.sequence(0)
		first := fixture.offer(sequence, 2673, 3999*time.Millisecond)
		crossing := fixture.offer(sequence, 37422, 4010*time.Millisecond)
		fresh := fixture.offer(sequence, 2673, 4010*time.Millisecond+225095*time.Nanosecond)
		fixture.ingress(first, 2673, 3999*time.Millisecond+150*time.Microsecond)
		fixture.ingress(crossing, 37422, 4610*time.Millisecond+150*time.Microsecond)
		fixture.ingress(fresh, 2673, 4610*time.Millisecond+150*time.Microsecond+test.serialization)
		fixture.ack(t, sequence, first, 4626*time.Millisecond)
		fixture.ack(t, sequence, crossing, 5229*time.Millisecond)
		crossingRate, _, latest := fixture.service.measured(time.Second, fixture.start.Add(5229*time.Millisecond))
		crossingRate = max(crossingRate, latest)
		if crossingRate != 12500000 || ingressCounterDiagnostic.resets != 0 {
			t.Errorf("%s cross-step delay replaced known service or fabricated source idle: rate=%d resets=%d", test.name, crossingRate, ingressCounterDiagnostic.resets)
		}
		fixture.ack(t, sequence, fresh, 5232*time.Millisecond)
		fixture.requireRate(t, 5232*time.Millisecond, test.want)
		t.Logf("%s cross-step-rate=%d post-step-rate=%d receiver-cross-span-nanos=%d", test.name, crossingRate, test.want, 611*time.Millisecond)
	}
}

// A recent fast peak temporarily masks a cross-step slow pair. Once the peak
// ages out, the crossing pair still includes changed propagation. Keep the
// prior service until fresh serialization distinguishes a true capacity change.
func TestWindowReceiverIngressForwardStepPeakExpiryDoesNotCreateSlowService(t *testing.T) {
	for _, test := range []struct {
		name          string
		serialization time.Duration
		want          ByteCount
	}{
		{name: "same capacity", serialization: 213840 * time.Nanosecond, want: 12500000},
		{name: "real decrease", serialization: 2138400 * time.Nanosecond, want: 1250000},
	} {
		fixture := newReceiverPhysicalSpanFixture(t, 12500000)
		sequence := fixture.sequence(0)
		// A 600 ms path permits this old ACK timing and retains a recent peak
		// across the 603 ms ACK separation using the existing sample horizon.
		fixture.service.observeReceiverRoundTrip(0, 610*time.Millisecond, 600*time.Millisecond, 10*time.Millisecond, fixture.start)
		seed := fixture.offer(sequence, 2673, 3998*time.Millisecond)
		first := fixture.offer(sequence, 2673, 3999*time.Millisecond)
		crossing := fixture.offer(sequence, 37422, 4010*time.Millisecond)
		fresh := fixture.offer(sequence, 2673, 4010*time.Millisecond+225095*time.Nanosecond)
		fixture.ingress(seed, 2673, 4299*time.Millisecond-213840*time.Nanosecond)
		fixture.ingress(first, 2673, 4299*time.Millisecond)
		fixture.ingress(crossing, 37422, 4910*time.Millisecond)
		fixture.ingress(fresh, 2673, 4910*time.Millisecond+test.serialization)
		fixture.ack(t, sequence, seed, 4625*time.Millisecond)
		fixture.ack(t, sequence, first, 4626*time.Millisecond)
		fixture.requireRate(t, 4626*time.Millisecond, 12500000)
		fixture.ack(t, sequence, crossing, 5229*time.Millisecond)
		fixture.requireRate(t, 5229*time.Millisecond, 12500000)
		var expired ByteCount
		for range 3 {
			rate, _, latest := fixture.service.measured(time.Second, fixture.start.Add(5300*time.Millisecond))
			expired = max(rate, latest)
			if expired != 12500000 || ingressCounterDiagnostic.resets != 0 {
				t.Errorf("%s peak expiry converted propagation delay into slower service: rate=%d resets=%d", test.name, expired, ingressCounterDiagnostic.resets)
			}
		}
		fixture.ack(t, sequence, fresh, 5301*time.Millisecond)
		fixture.requireRate(t, 5301*time.Millisecond, test.want)
		fixture.requireRate(t, 7*time.Second, test.want)
		t.Logf("%s recent-peak=%d expired-cross-rate=%d post-step-rate=%d", test.name, 12500000, expired, test.want)
	}
}
