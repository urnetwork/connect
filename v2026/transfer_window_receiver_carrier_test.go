// Receiver timing follows the current carrier while service identity stays logical.
package connect

import (
	"context"
	"testing"
	"time"
)

// Shared timing contains confirmed H1 samples. A published non-H1 route must
// use this lane's own paired samples without evicting another H1 sibling's
// evidence or adding a physical route to the logical service identity.
func TestWindowPacingReceiverTimingFollowsCurrentCarrier(t *testing.T) {
	at := time.Unix(1700000000, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selectors := make([]*MultiRouteSelector, 2)
	sequences := make([]*SendSequence, 2)
	transports := make([]Transport, 2)
	for i := range sequences {
		selector := NewMultiRouteSelector(ctx, "receiver-timing-carrier", nil, TransferPath{}, true)
		defer selector.Close()
		transport := NewSendGatewayTransportWithType(TransportTypeH1)
		selector.updateTransportWithProperties(transport, []Route{make(Route, 1)}, TransferCarrierProperties{})
		selectors[i], transports[i] = selector, transport
		sequences[i] = newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
			settings.TargetGoodputByteRate = 125000000
		})
		sequences[i].contractMultiRouteWriter = selector
		sequences[i].observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(mib(48)),
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
		})
	}
	shared := newWindowPacingService(sequences[0].sendBufferSettings)
	shared.observeReceiverRoundTrip(1, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	for _, sequence := range sequences {
		sequence.windowPacer.service = shared
		if !sequence.transferFlightPolicy().h1Only {
			t.Fatal("fixture did not publish H1-only policy")
		}
		estimate := sequence.sendWindowEstimate(at)
		if estimate.RoundTrip != time.Millisecond || estimate.WindowRoundTrip != 11*time.Millisecond {
			t.Fatalf("unsampled H1 sibling did not share measured timing: %+v", estimate)
		}
	}
	h3 := NewSendGatewayTransportWithType(TransportTypeH3)
	selectors[0].updateTransportWithProperties(h3, []Route{make(Route, 1)}, TransferCarrierProperties{})
	sequences[0].rttWindow.observeReceiverRoundTrip(110*time.Millisecond, 10*time.Millisecond, 10000, at.Add(time.Millisecond))
	for _, mixed := range []bool{true, false} {
		if !mixed {
			selectors[0].updateTransportWithProperties(transports[0], nil, TransferCarrierProperties{})
		}
		if sequences[0].transferFlightPolicy().h1Only {
			t.Fatalf("mixed=%t route publication remained H1-only", mixed)
		}
		estimate := sequences[0].sendWindowEstimate(at.Add(time.Millisecond))
		t.Logf("mixed=%t network=%s residence=%s window=%d", mixed, estimate.RoundTrip, estimate.WindowRoundTrip, estimate.Window)
		if estimate.RoundTrip != 100*time.Millisecond || estimate.WindowRoundTrip != 110*time.Millisecond {
			t.Errorf("mixed=%t changed carrier borrowed unrelated H1 timing: network=%s residence=%s", mixed, estimate.RoundTrip, estimate.WindowRoundTrip)
		}
		sibling := sequences[1].sendWindowEstimate(at.Add(time.Millisecond))
		if sibling.RoundTrip != time.Millisecond || sibling.WindowRoundTrip != 11*time.Millisecond {
			t.Fatalf("mixed=%t route change erased H1 sibling evidence: %+v", mixed, sibling)
		}
	}
	shared.stateLock.Lock()
	retained := shared.receiverRoundTrips.count
	shared.stateLock.Unlock()
	if retained != 1 {
		t.Fatalf("reading changed carrier mutated shared history: count=%d", retained)
	}
	selectors[0].updateTransportWithProperties(transports[0], []Route{make(Route, 1)}, TransferCarrierProperties{})
	selectors[0].updateTransportWithProperties(h3, nil, TransferCarrierProperties{})
	restored := sequences[0].sendWindowEstimate(at.Add(time.Millisecond))
	if !sequences[0].transferFlightPolicy().h1Only || restored.RoundTrip != time.Millisecond || restored.WindowRoundTrip != 11*time.Millisecond {
		t.Fatalf("return to H1 failed to reuse intact logical service: %+v", restored)
	}
}
