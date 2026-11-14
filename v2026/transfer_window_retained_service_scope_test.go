// Window growth must use serialization evidence owned by the current carrier.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// An idle sibling has its own logical history and opening but shares the H1
// service and memory pool. It has never delivered a cumulative sizing interval.
func newRetainedServiceSibling(t *testing.T, source *SendSequence, at time.Time) *SendSequence {
	t.Helper()
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueMaxByteCount = 2 * 1024 * 1024
		settings.ResendQueueMinByteCount = 256 * 1024
		settings.ResendQueueBudget = source.resendQueue.Budget()
		settings.TargetGoodputByteRate = 125000000
	})
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	sequence.sequenceId = NewId()
	sequence.windowPacer.service = source.windowPacer.service
	sequence.windowPacer.serviceSequenceId = sequence.sequenceId
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: 48 * 1024 * 1024,
		ackCompressTimeoutSet: true,
	})
	sequence.rttWindow.closeSendTime(uint64(at.Add(-400*time.Millisecond).UnixMilli()), at)
	return sequence
}

// Published unknown, H3, P2P, and mixed policies cannot permanently grow a
// logical window from another carrier's service and this lane's unrelated RTT.
func TestWindowRetainedSharedServiceCannotGrowOtherCarriers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, at := newRetainedServiceGrowthFixture(t, 512)
		service := source.windowPacer.service
		if rate, total, _ := service.measured(time.Second, at); rate != 125000000 || total != 2*1024*1024 {
			t.Fatalf("fixture did not establish independent H1 service: rate=%d total=%d", rate, total)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		for _, test := range []struct {
			name       string
			transports []TransportType
		}{
			{name: "unknown"},
			{name: "h3", transports: []TransportType{TransportTypeH3}},
			{name: "p2p", transports: []TransportType{TransportTypeP2p}},
			{name: "mixed", transports: []TransportType{TransportTypeH1, TransportTypeH3}},
		} {
			sequence := newRetainedServiceSibling(t, source, at)
			selector := NewMultiRouteSelector(ctx, "retained-service-scope", nil, TransferPath{}, true)
			defer selector.Close()
			for _, transport := range test.transports {
				selector.updateTransportWithProperties(NewSendGatewayTransportWithType(transport), []Route{make(Route, 1)}, TransferCarrierProperties{Unreliable: transport == TransportTypeP2p})
			}
			sequence.contractMultiRouteWriter = selector
			if sequence.transferFlightPolicy().h1Only {
				t.Fatalf("%s fixture published H1-only policy", test.name)
			}
			sequence.rttWindow.observeReceiverRoundTrip(2*time.Second, 0, 0, at)
			estimate := sequence.sendWindowEstimate(at)
			if estimate.RoundTrip != 2*time.Second || estimate.WindowRoundTrip != 2*time.Second || estimate.Sized {
				t.Fatalf("%s fixture lost independent local RTT or invented cumulative history: %+v", test.name, estimate)
			}
			if estimate.Window != 2*1024*1024 || estimate.LearnedWindow != 2*1024*1024 || estimate.ServiceSized {
				t.Errorf("%s carrier learned capacity from unrelated H1 service: %+v", test.name, estimate)
			}
		}
		if rate, total, _ := service.measured(time.Second, at); rate != 125000000 || total != 2*1024*1024 {
			t.Fatalf("non-H1 reads changed a live sibling's service: rate=%d total=%d", rate, total)
		}
	})
}

// A current H1 sibling can use the same service without first filling its
// own multi-RTT history. Excluding every shared service would lose this growth.
func TestWindowRetainedSharedServiceGrowsH1Sibling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, at := newRetainedServiceGrowthFixture(t, 512)
		sequence := newRetainedServiceSibling(t, source, at)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		selector := NewMultiRouteSelector(ctx, "retained-service-sibling", nil, TransferPath{}, true)
		defer selector.Close()
		selector.updateTransportWithProperties(NewSendGatewayTransportWithType(TransportTypeH1), []Route{make(Route, 1)}, TransferCarrierProperties{})
		sequence.contractMultiRouteWriter = selector
		if !sequence.transferFlightPolicy().h1Only {
			t.Fatal("fixture did not publish H1-only policy")
		}
		estimate := sequence.sendWindowEstimate(at)
		if estimate.Sized || !estimate.ServiceSized || !estimate.ServiceEstablished || estimate.ServiceByteRate != 125000000 || estimate.Window < 40000000 || estimate.Window > estimate.Ceiling {
			t.Fatalf("H1 sibling could not grow from its physical service: %+v", estimate)
		}
	})
}

// Without a shared carrier service, two explicit local delivery checkpoints
// can establish serialization before the longer cumulative horizon qualifies.
func TestWindowRetainedLocalServiceCanGrowWithoutH1Policy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, at := newRetainedWindowFixture(t)
		if sequence.windowPacer.service != nil || sequence.transferFlightPolicy().h1Only {
			t.Fatal("fixture unexpectedly shares H1 service")
		}
		sequence.observeDeliveredBytes(128*1024, at)
		at = at.Add(10 * time.Millisecond)
		sequence.observeDeliveredBytes(128*1024, at)
		estimate := sequence.sendWindowEstimate(at)
		if estimate.Sized || !estimate.ServiceSized || estimate.ServiceByteRate != 13107200 || estimate.Window != 1310720 || estimate.LearnedWindow != estimate.Window {
			t.Fatalf("independent local serialization failed to grow the opening: %+v", estimate)
		}
	})
}
