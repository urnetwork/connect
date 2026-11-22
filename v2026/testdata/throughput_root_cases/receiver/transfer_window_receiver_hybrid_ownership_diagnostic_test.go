// Unknown receiver timing cannot replace existing discovery. Only an accepted
// receiver interval owns later no-evidence holds in this isolated hybrid.
package connect

import (
	"testing"
	"time"
)

// A first named endpoint has no rate. Fresh ordinary ACK evidence still needs
// to replace an earlier startup estimate while no receiver pair is available.
func TestWindowReceiverIngressUnknownPairKeepsOrdinaryDiscovery(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 1250000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	if sender := ingressCounterDiagnostic.senders[fixture.service]; sender == nil || sender.latest != 0 {
		t.Fatal("fixture unexpectedly supplied a receiver rate")
	}
	fixture.service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, fixture.start.Add(100*time.Millisecond))
	for i := 0; i <= 8; i++ {
		at := fixture.start.Add(100*time.Millisecond + time.Duration(i)*50*time.Millisecond)
		fixture.service.sent += 625000
		fixture.service.observe(625000, at)
	}
	fixture.requireRate(t, 500*time.Millisecond, 12500000)
}

// Once a real receiver pair established capacity, later one-head source
// trains do not hand control to ordinary ACK gaps that price local idleness.
func TestWindowReceiverIngressEstablishedPairKeepsIdleHold(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 0)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	second := fixture.offer(sequence, 2673, 100*time.Microsecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ingress(second, 2673, 50*time.Millisecond+213840*time.Nanosecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, second, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 12500000)
	for i := 1; i <= 8; i++ {
		at := time.Duration(i) * time.Second
		receiverOfferTrainWindowWait(fixture, sequence, at-time.Millisecond, at)
		pack := fixture.offer(sequence, 2673, at)
		fixture.ingress(pack, 2673, at+50*time.Millisecond)
		fixture.ack(t, sequence, pack, at+100*time.Millisecond)
		fixture.service.observe(2673, fixture.start.Add(at+100*time.Millisecond))
		fixture.requireRate(t, at+100*time.Millisecond, 12500000)
	}
}
