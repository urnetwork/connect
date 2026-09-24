// A fresh whole-flight delivery interval can guide pacing while confirmed
// drained trains have not supplied a serialization pair. Until the path shows
// queueing, the pacer releases the admitted window over one residence;
// retained bytes never infer a serialization rate.
package connect

import (
	"context"
	"testing"
	"time"
)

// The discovery floor releases the admitted window over its residence.
func windowDiscoveryFloor(estimate SendWindowEstimate) ByteCount {
	return ByteCount(float64(estimate.Window) / estimate.WindowRoundTrip.Seconds())
}

// Match the constrained sdk opening with ample independently stated byte
// permission. The fixture owns all clocks and advances no background worker.
func newWindowCumulativePacingFixture(t *testing.T) (*SendSequence, time.Time) {
	t.Helper()
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueMaxByteCount = 512 * 1024
		settings.ResendQueueMinByteCount = 64 * 1024
		settings.ResendQueueBudget = NewTransferMemoryBudget(16 * 1024 * 1024)
		settings.TargetGoodputByteRate = 125000000
	})
	sequence.sequenceId = NewId()
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.windowPacer.service = newWindowPacingService(sequence.sendBufferSettings)
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	at := time.Unix(1700000000, 0)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: 8 * 1024 * 1024,
		ackCompressTimeoutSet: true,
	})
	sequence.receiveWindowSetAtNanos.Store(at.UnixNano())
	return sequence, at
}

// Each complete flight has one confirmed physical tail and one cumulative
// reply. The worker applies its logical checkpoint after physical accounting.
func sampleWindowCumulativePacing(t *testing.T, sequence *SendSequence, at time.Time, bytes ByteCount, replies int) time.Time {
	t.Helper()
	service := sequence.windowPacer.service
	for range replies {
		messageId := NewId()
		number := uint64(at.UnixNano())
		service.sent += bytes
		service.beginWrite(sequence.sequenceId, messageId, number, at, false)
		service.finishWrite(sequence.sequenceId, messageId, true)
		sentAt := at
		at = at.Add(10 * time.Millisecond)
		sequence.rttWindow.closeSendTime(uint64(sentAt.UnixMilli()), at)
		service.acknowledgeWrite(sequence.sequenceId, messageId, number, false, 0, at)
		service.observe(bytes, at)
		sequence.observeAckedBytesWithServiceCredit(bytes, 0, windowServiceAckCredit{}, at)
	}
	if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 0 || !service.drained {
		t.Fatalf("separate drained flights invented serialization: rate=%d latest=%d drained=%t", rate, latest, service.drained)
	}
	return at
}

// Repeated complete deliveries can double the opening without a serialization
// pair. The pacer releases that doubled window over one residence while the
// path shows no queue.
func TestWindowPacingCumulativeGrowthRaisesColdRate(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.ServiceSized || estimate.ServiceByteRate != 0 ||
		estimate.Window != 1024*1024 || estimate.DeliveredByteCount != 2*1024*1024 || estimate.Interval != 40*time.Millisecond {
		t.Fatalf("fixture did not qualify cumulative growth without service: %+v", estimate)
	}
	if estimate.PacingByteRate != windowDiscoveryFloor(estimate) || !estimate.PacingDiscovery {
		t.Errorf("fresh cumulative growth did not release the doubled window: %+v", estimate)
	}
}

// An isolated drained reply still lacks the full interval needed to infer a
// rate. It cannot enlarge the opening or its ordinary startup pacing.
func TestWindowPacingCumulativeSingleReplyKeepsBootstrap(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 1)
	estimate := sequence.sendWindowEstimate(at)
	if estimate.Sized || estimate.ServiceSized || estimate.ServiceByteRate != 0 ||
		estimate.Window != 512*1024 || estimate.PacingByteRate != 52428800 {
		t.Fatalf("one cold reply invented capacity: %+v", estimate)
	}
}

// A handful of control bytes can span the history interval without supplying
// a data-rate measurement. Preserve the opening for subsequent bulk traffic.
func TestWindowPacingCumulativeTinyRepliesKeepBootstrap(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 256, 5)
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.DeliveredByteCount != 1024 || estimate.ServiceByteRate != 0 ||
		estimate.Window != 512*1024 || estimate.PacingByteRate != 52428800 {
		t.Fatalf("tiny cumulative control traffic repriced the data opening: %+v", estimate)
	}
}

// Retention is memory permission. Clearing the measurement history infers no
// serialization rate, and the retained window is still released per residence
// only because the path has never queued.
func TestWindowPacingCumulativeMissingHistoryKeepsRetainedWindow(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	sequence.deliveredBytesCount = 0
	after := sequence.sendWindowEstimate(at)
	if before.Window != 1024*1024 || after.Sized || after.Window != before.Window ||
		after.ServiceByteRate != 0 || after.DeliveryByteRate != 0 || after.PacingByteRate != windowDiscoveryFloor(after) {
		t.Fatalf("missing evidence priced retained bytes: before=%+v after=%+v", before, after)
	}
}

// The same old checkpoints remain useful for retained sizing, but an endpoint
// older than its measurement horizon cannot provide a current pacing rate.
func TestWindowPacingCumulativeStaleHistoryCannotReprice(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	after := sequence.sendWindowEstimate(at.Add(40*time.Millisecond + time.Nanosecond))
	if before.Window != 1024*1024 || after.Window != before.Window ||
		after.DeliveryByteRate != 0 || after.PacingByteRate != windowDiscoveryFloor(after) {
		t.Fatalf("stale cumulative evidence repriced retained capacity: before=%+v after=%+v", before, after)
	}
}

// A permission change invalidates the old cumulative interval even while its
// endpoint is recent. Retention must not bypass that existing proof boundary.
func TestWindowPacingCumulativePermissionStepRejectsOldRate(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 16 * 1024 * 1024})
	sequence.receiveWindowSetAtNanos.Store(at.Add(time.Nanosecond).UnixNano())
	after := sequence.sendWindowEstimate(at.Add(time.Nanosecond))
	if after.Sized || after.Window != before.Window ||
		after.DeliveryByteRate != 0 || after.PacingByteRate != windowDiscoveryFloor(after) {
		t.Fatalf("a permission step reused the old rate: before=%+v after=%+v", before, after)
	}
}

// Slower delivery whose replies show no queue means the source, not the path,
// was the limit. The pace does not fall without congestion evidence.
func TestWindowPacingCumulativeSlowUnqueuedDeliveryKeepsPace(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	at = sampleWindowCumulativePacing(t, sequence, at, 256*1024, 5)
	after := sequence.sendWindowEstimate(at)
	if !after.Sized || after.ServiceByteRate != 0 || after.Window != before.Window ||
		after.DeliveredByteCount != 1024*1024 || after.Interval != 40*time.Millisecond ||
		!after.PacingDiscovery || after.PacingByteRate != windowDiscoveryFloor(after) {
		t.Fatalf("unqueued slow delivery lowered the pace: before=%+v after=%+v", before, after)
	}
}

// A queued reply proves the path is the limit. Fresh slower delivery then
// lowers pacing while learned bytes stay available.
func TestWindowPacingCumulativeSlowQueuedDeliveryLowersOnlyRate(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	at = sampleWindowCumulativePacing(t, sequence, at, 256*1024, 5)
	sequence.windowPacer.service.observeRoundTrip(40*time.Millisecond, 0, at)
	after := sequence.sendWindowEstimate(at)
	if !after.Sized || after.ServiceByteRate != 0 || after.Window != before.Window ||
		after.DeliveredByteCount != 1024*1024 || after.Interval != 40*time.Millisecond ||
		after.PacingDiscovery || after.PacingByteRate != 28835840 {
		t.Fatalf("slow cumulative delivery did not lower pacing independently: before=%+v after=%+v", before, after)
	}
}

// A queued reply proves the slow service is the path's. Then even a faster
// qualified cumulative interval must defer to it.
func TestWindowPacingCumulativeRateDefersToSlowService(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	service := newWindowPacingService(sequence.sendBufferSettings)
	service.observeRoundTrip(10*time.Millisecond, 0, at.Add(-20*time.Millisecond))
	service.observeRoundTrip(40*time.Millisecond, 0, at.Add(-15*time.Millisecond))
	service.observe(4096, at.Add(-10*time.Millisecond))
	service.observe(4096, at)
	sequence.windowPacer.service = service
	after := sequence.sendWindowEstimate(at)
	if !after.Sized || after.ServiceByteRate != 409600 || !after.ServiceEstablished ||
		after.PacingDiscovery || after.PacingByteRate != 450560 || after.Window != before.Window {
		t.Fatalf("cumulative delivery overrode measured slow service: before=%+v after=%+v", before, after)
	}
}

// Measured service without queue evidence bounds capacity only from below, so
// discovery keeps releasing the admitted window.
func TestWindowPacingCumulativeUnqueuedSlowServiceStillDiscovers(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	before := sequence.sendWindowEstimate(at)
	service := newWindowPacingService(sequence.sendBufferSettings)
	service.observe(4096, at.Add(-10*time.Millisecond))
	service.observe(4096, at)
	sequence.windowPacer.service = service
	after := sequence.sendWindowEstimate(at)
	if after.ServiceByteRate != 409600 || !after.ServiceEstablished || !after.PacingDiscovery ||
		after.PacingByteRate != windowDiscoveryFloor(after) || after.Window != before.Window {
		t.Fatalf("unqueued slow service ended discovery: before=%+v after=%+v", before, after)
	}
}

// A pacing-rate fallback cannot override byte permission or the independently
// configured target. Each bound is applied to the same fresh delivery evidence.
func TestWindowPacingCumulativeRatePreservesHardBounds(t *testing.T) {
	for _, bound := range []string{"peer", "memory", "configured"} {
		sequence, at := newWindowCumulativePacingFixture(t)
		at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
		switch bound {
		case "peer":
			sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 256 * 1024})
		case "memory":
			sequence.resendQueue.Budget().SetTotalByteCount(256 * 1024)
		case "configured":
			sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = 256 * 1024
		}
		sequence.sendBufferSettings.TargetGoodputByteRate = 1000000
		estimate := sequence.sendWindowEstimate(at)
		if !estimate.Sized || estimate.ServiceByteRate != 0 || estimate.Window != 256*1024 || estimate.Ceiling != 256*1024 ||
			estimate.PacingByteRate != estimate.PacingProbeByteRate || estimate.PacingByteRate > 1183432 {
			t.Errorf("%s bound was bypassed by cumulative pacing: %+v", bound, estimate)
		}
	}
}

// Logical delivery on an unknown, mixed or non-H1 carrier cannot reprice an
// absent shared H1 service. Its cumulative window candidate remains valid.
func TestWindowPacingCumulativeSharedServiceRejectsOtherCarriers(t *testing.T) {
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
		sequence, at := newWindowCumulativePacingFixture(t)
		at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
		selector := NewMultiRouteSelector(ctx, "cumulative-pacing-scope", nil, TransferPath{}, true)
		defer selector.Close()
		for _, transport := range test.transports {
			selector.updateTransportWithProperties(NewSendGatewayTransportWithType(transport), []Route{make(Route, 1)}, TransferCarrierProperties{Unreliable: transport == TransportTypeP2p})
		}
		sequence.contractMultiRouteWriter = selector
		if sequence.transferFlightPolicy().h1Only {
			t.Fatalf("%s fixture published H1-only policy", test.name)
		}
		estimate := sequence.sendWindowEstimate(at)
		if !estimate.Sized || estimate.Window != 1024*1024 || estimate.ServiceByteRate != 0 {
			t.Fatalf("%s lost qualified logical delivery: %+v", test.name, estimate)
		}
		if estimate.PacingByteRate != 52428800 {
			t.Errorf("%s logical carrier repriced the shared H1 fallback: %+v", test.name, estimate)
		}
	}
}

// A sibling sharing the service inherits the pace the path already accepted;
// its own window still bounds its flight. Positive service is shared too.
func TestWindowPacingCumulativeH1SiblingOwnsItsFallback(t *testing.T) {
	source, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, source, at, 512*1024, 5)
	sibling, _ := newWindowCumulativePacingFixture(t)
	sibling.windowPacer.service = source.windowPacer.service
	sibling.rttWindow.closeSendTime(uint64(at.Add(-10*time.Millisecond).UnixMilli()), at)
	fresh := source.sendWindowEstimate(at)
	idle := sibling.sendWindowEstimate(at)
	if fresh.PacingByteRate != windowDiscoveryFloor(fresh) || !fresh.Sized || idle.Sized ||
		idle.PacingByteRate != fresh.PacingByteRate || idle.Window != idle.Initial {
		t.Fatalf("cold siblings confused local cumulative evidence: fresh=%+v idle=%+v", fresh, idle)
	}
	service := newWindowPacingService(source.sendBufferSettings)
	service.observeRoundTrip(10*time.Millisecond, 0, at.Add(-20*time.Millisecond))
	service.observeRoundTrip(40*time.Millisecond, 0, at.Add(-15*time.Millisecond))
	service.observe(4096, at.Add(-10*time.Millisecond))
	service.observe(4096, at)
	for _, sequence := range []*SendSequence{source, sibling} {
		sequence.windowPacer.service = service
		estimate := sequence.sendWindowEstimate(at)
		if estimate.ServiceByteRate != 409600 || !estimate.ServiceEstablished || estimate.PacingByteRate != 450560 {
			t.Errorf("shared positive service did not own sibling pacing: %+v", estimate)
		}
	}
}

// Standalone sequence histories have no sibling carrier whose service they
// could misattribute. Their fresh logical interval remains a valid fallback.
func TestWindowPacingCumulativeStandaloneRateStillWorks(t *testing.T) {
	sequence, at := newWindowCumulativePacingFixture(t)
	at = sampleWindowCumulativePacing(t, sequence, at, 512*1024, 5)
	sequence.windowPacer.service = nil
	sequence.contractMultiRouteWriter = nil
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.ServiceByteRate != 0 || estimate.PacingByteRate != 57671680 || estimate.Window != 1024*1024 {
		t.Fatalf("standalone cumulative evidence lost its pacing fallback: %+v", estimate)
	}
}
