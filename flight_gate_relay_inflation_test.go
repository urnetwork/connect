package connect

// FLIGHTGATEFIX §21.5, measurement 1. The campaign's
// mixed-relay-queue-inflation schedule in process: one reliable lane whose
// drain rate steps down mid-transfer, a queue in front of it deep enough
// that a write does not block, and the default resend budget. It records
// what the storm depends on, so a later change can be judged against the
// same instrument.

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// relayInflationResult is one arm of the schedule.
type relayInflationResult struct {
	elapsed     time.Duration
	deadWindows int
	windows     int
	peakQueue   ByteCount
	stats       ClientSendRecoveryStatsSnapshot
}

// Builds the historical queue-depth control with the same configuration in
// the traffic measurement and its deterministic precondition check.
func newRelayInflationHarness(
	t testing.TB,
	queueFrames int,
	deferTimeoutResend bool,
	boundOff bool,
) *mixedLaneGapHarness {
	t.Helper()
	return newMixedLaneHarnessWithOptions(t, mixedLaneOptions{
		slowLatency:                50 * time.Millisecond,
		slowSerialization:          500 * time.Microsecond,
		slowStepAfter:              time.Second,
		slowSerializationAfterStep: 12 * time.Millisecond,
		slowQueueFrames:            queueFrames,
		directLaneDisabled:         true,
		deferTimeoutResend:         deferTimeoutResend,
		reliableAdmissionUnbounded: boundOff,
		constantSendWindow:         true,
	})
}

// Offers the payload over one reliable lane that slows mid-transfer,
// sampling the resend queue while it runs.
func runRelayInflation(
	t testing.TB,
	messageCount int,
	queueFrames int,
	deferTimeoutResend bool,
	boundOff bool,
) relayInflationResult {
	t.Helper()
	harness := newRelayInflationHarness(t, queueFrames, deferTimeoutResend, boundOff)
	start := time.Now()
	var peakQueue ByteCount
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				if _, byteCount, _ := harness.sender.ResendQueueSize(
					harness.receiverId, MultiHopId{}, false, false); peakQueue < byteCount {
					peakQueue = byteCount
				}
			}
		}
	}()
	stats := harness.run(t, messageCount)
	close(stop)
	<-done
	elapsed := time.Since(start)

	harness.deliveryLock.Lock()
	times := append([]time.Time(nil), harness.deliveryTimes...)
	harness.deliveryLock.Unlock()
	windows := max(1, int(elapsed/time.Second))
	perWindow := make([]int, windows)
	for _, at := range times {
		index := min(max(int(at.Sub(start)/time.Second), 0), windows-1)
		perWindow[index] += 1
	}
	mean := float64(len(times)) / float64(windows)
	deadWindows := 0
	for _, count := range perWindow {
		if float64(count) < mean/4 {
			deadWindows += 1
		}
	}
	return relayInflationResult{
		elapsed:     elapsed,
		deadWindows: deadWindows,
		windows:     windows,
		peakQueue:   peakQueue,
		stats:       stats,
	}
}

// The historical control requires a constant send window without newer
// pacing. A delivery-sized default otherwise silently enables H1 pacing.
func TestRelayInflationUsesConstantSendWindow(t *testing.T) {
	defer SetWindowSizing(DefaultWindowSizing())
	SetWindowSizing(WindowSizingFromDelivery)
	for _, c := range []struct {
		queueFrames        int
		deferTimeoutResend bool
	}{
		{queueFrames: 64, deferTimeoutResend: true},
		{queueFrames: 4096, deferTimeoutResend: false},
		{queueFrames: 4096, deferTimeoutResend: true},
	} {
		harness := newRelayInflationHarness(t, c.queueFrames, c.deferTimeoutResend, true)
		for _, endpoint := range []struct {
			name   string
			client *Client
		}{
			{name: "sender", client: harness.sender},
			{name: "receiver", client: harness.receiver},
		} {
			settings := endpoint.client.settings.SendBufferSettings
			if settings.WindowSizing != WindowSizingConstant ||
				settings.DeliverySizedWindowScale != 0 ||
				settings.TargetGoodputByteRate != 0 || settings.ResendQueueBudget != nil {
				t.Errorf("queue=%d defer=%t %s: historical control uses policy=%d scale=%d target=%d budget=%v; delivery sizing and H1 pacing must be off",
					c.queueFrames, c.deferTimeoutResend, endpoint.name, settings.WindowSizing,
					settings.DeliverySizedWindowScale, settings.TargetGoodputByteRate, settings.ResendQueueBudget)
			}
			if settings.ReliableAdmissionBoundedByDelivery {
				t.Errorf("queue=%d defer=%t %s: historical control enabled delivery-bounded admission",
					c.queueFrames, c.deferTimeoutResend, endpoint.name)
			}
		}
	}
	if got := DefaultWindowSizing(); got != WindowSizingFromDelivery {
		t.Fatalf("the historical control changed the process default to %d", got)
	}
	harness := newMixedLaneHarnessWithOptions(t, mixedLaneOptions{})
	if got := harness.sender.settings.SendBufferSettings.WindowSizing; got != WindowSizingFromDelivery {
		t.Fatalf("the ordinary mixed-lane fixture changed policy to %d", got)
	}
}

// What the storm depends on. The queue in front of the lane is the
// variable: while it is shallow a full route channel blocks the sequence's
// own writes and throttles admission, and while it is deep nothing does,
// so the resend queue fills to its budget against a lane that has just
// proved it cannot drain what it holds. That happens with the deferred
// retransmit and without it alike, so the budget is not reached because
// the deferral withholds a write.
func TestInflatedRelayQueueDepthDominatesTheStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("relay queue inflation schedule")
	}
	const (
		messageCount    = 6000
		shallowQueue    = 64
		deepQueue       = 4096
		resendBudgetBar = 2 << 20
	)
	report := func(name string, result relayInflationResult) {
		t.Logf(
			"%s: %s, %d of %d windows dead, peak resend queue %d B, rto=%d deferred=%d gap=%d carrier-change=%d",
			name, result.elapsed.Truncate(time.Millisecond),
			result.deadWindows, result.windows, result.peakQueue,
			result.stats.TimeoutResendWriteCount,
			result.stats.TimeoutResendDeferCount,
			result.stats.SelectiveGapWriteCount,
			result.stats.CarrierChangeWriteCount,
		)
	}
	shallow := runRelayInflation(t, messageCount, shallowQueue, true, true)
	report("shallow queue, defer on ", shallow)
	deepOff := runRelayInflation(t, messageCount, deepQueue, false, true)
	report("deep queue,    defer off", deepOff)
	deepOn := runRelayInflation(t, messageCount, deepQueue, true, true)
	report("deep queue,    defer on ", deepOn)

	// the instrument still reproduces the storm it was built for
	if deepOff.stats.TimeoutResendWriteCount < 500 {
		t.Fatalf(
			"the deep-queue arm wrote only %d whole-window timeouts, so this no longer reproduces the storm",
			deepOff.stats.TimeoutResendWriteCount,
		)
	}
	// a shallow queue holds the resend queue far below the budget the deep
	// one reaches: the route channel is the throttle
	if resendBudgetBar/2 <= shallow.peakQueue {
		t.Errorf("the shallow-queue arm reached %d B of resend queue, so the channel is not throttling it",
			shallow.peakQueue)
	}
	// and the budget is reached with the deferral and without it alike
	for name, result := range map[string]relayInflationResult{
		"with the deferred retransmit":    deepOn,
		"without the deferred retransmit": deepOff,
	} {
		if result.peakQueue < resendBudgetBar/2 {
			t.Errorf(
				"the deep-queue arm %s peaked at %d B of resend queue, under half the budget: "+
					"the claim under test is that a deep queue lets admission reach the budget either way",
				name, result.peakQueue,
			)
		}
	}
}

// FLIGHTGATEFIX §23.3. What the six mixed cells' gap resends actually are.
// A direct lane beside a relay whose drain steps down mid-transfer is the
// shape those cells measure, and in it most scoreboard recoveries are of
// relay-carried holes whose own retransmit had already been deferred: the
// grace ends exactly when the timer first comes due, so the scoreboard
// writes what the timer declined. The counters attribute that rather than
// leaving it as a single number.
func TestGapWritesOfDeferredItemsAreAttributed(t *testing.T) {
	if testing.Short() {
		t.Skip("mixed route with an inflating relay")
	}
	assertMessagePoolOwnership(t)
	synctest.Test(t, testGapWritesOfDeferredItemsAreAttributed)
}

func testGapWritesOfDeferredItemsAreAttributed(t *testing.T) {
	const messageCount = 4000
	harness := newMixedLaneHarnessWithOptions(t, mixedLaneOptions{
		fastLatency:                20 * time.Millisecond,
		slowLatency:                200 * time.Millisecond,
		fastSerialization:          time.Millisecond,
		slowSerialization:          500 * time.Microsecond,
		slowStepAfter:              time.Second,
		slowSerializationAfterStep: 12 * time.Millisecond,
		slowQueueFrames:            4096,
		fastDropFraction:           0.01,
		replySerialization:         time.Millisecond,
		deferTimeoutResend:         true,
	})
	harness.startReverseLoad(5 * time.Millisecond)
	// Virtual time uses the fixture's delivery bound, not the suite's clock.
	stats := harness.run(struct{ testing.TB }{TB: t}, messageCount)
	fromDeferredRelay := stats.SelectiveGapWritesOfDeferredItems[gapHoleCarrierReliable]
	fromDeferredDirect := stats.SelectiveGapWritesOfDeferredItems[gapHoleCarrierUnreliable]
	t.Logf(
		"gap=%d of which deferred relay=%d direct=%d | rto=%d deferred=%d route generations=%d",
		stats.SelectiveGapWriteCount, fromDeferredRelay, fromDeferredDirect,
		stats.TimeoutResendWriteCount, stats.TimeoutResendDeferCount,
		stats.RouteGenerationChangeCount,
	)
	if stats.SelectiveGapWriteCount < 100 {
		t.Fatalf("only %d gap writes: this no longer reproduces the shape the counters attribute",
			stats.SelectiveGapWriteCount)
	}
	if attributed := fromDeferredRelay + fromDeferredDirect; attributed < stats.SelectiveGapWriteCount/2 {
		t.Fatalf(
			"only %d of %d gap writes are of items whose own retransmit was deferred; the counters "+
				"exist to say which recoveries the six mixed cells are behind on",
			attributed, stats.SelectiveGapWriteCount,
		)
	}
	if fromDeferredRelay <= fromDeferredDirect {
		t.Fatalf(
			"gap writes of deferred items split %d relay to %d direct: the mechanism is a "+
				"relay-carried hole overtaken by direct-lane acknowledgements",
			fromDeferredRelay, fromDeferredDirect,
		)
	}
}

// The gap belongs to the carrier observed before its retry. A successful retry
// may change that carrier; a prior deferral remains part of the item's history.
// Fill the other route so the real selector must use the requested recovery
// lane, then let the real send worker account for exactly one due gap write.
func TestDeferredGapWriteKeepsHoleCarrierAcrossRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, holeDirect := range []bool{false, true} {
		for _, recoveryDirect := range []bool{false, true} {
			for _, deferred := range []bool{false, true} {
				t.Run(fmt.Sprintf("hole_direct_%t/recovery_direct_%t/deferred_%t", holeDirect, recoveryDirect, deferred), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						settings := DefaultClientSettings()
						settings.Log = NewNoopLogger()
						settings.EncryptionSettings.Mode = EncryptionModeOff
						// Keep platform registration from sharing the two fixture routes.
						settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
						client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
						relay, direct := make(Route, 1), make(Route, 1)
						t.Cleanup(func() {
							cancel()
							closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
							defer closeCancel()
							if err := client.CloseAndWait(closeCtx); err != nil {
								t.Errorf("close attribution client: %v", err)
							}
							for _, route := range []Route{relay, direct} {
								select {
								case wire := <-route:
									MessagePoolReturn(wire)
								default:
								}
							}
						})
						client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{relay})
						client.RouteManager().UpdateTransportWithProperties(NewSendGatewayTransportWithType(TransportTypeP2p),
							[]Route{direct}, TransferCarrierProperties{Unreliable: true})
						destination := NewId()
						client.ContractManager().AddNoContractPeer(destination)
						sequence := NewSendSequence(ctx, client, nil, destination, MultiHopId{}, false, false, false,
							sequenceTlsRoleClient, false, settings.SendBufferSettings)
						sequence.contractMultiRouteWriter = client.RouteManager().OpenMultiRouteWriter(DestinationId(destination))
						sequence.contractMultiRouteWriterDestination = DestinationId(destination)
						item := &sendItem{
							transferItem: transferItem{messageId: NewId()},
							head:         true, forceUnwrapped: true, sendCount: 1, expectsAck: true,
							sendTime: time.Now(), resendTime: time.Now(), ackTimeout: settings.SendBufferSettings.AckTimeout,
							recoveryKind: sendRecoverySelectiveGap,
						}
						item.transferFrameBytes = marshalSendPackTransferFrame(&sendPackFrame{
							path:      sendTransferPath(client.ClientId(), DestinationId(destination)),
							messageId: item.messageId, sequenceId: sequence.sequenceId,
						})
						if deferred {
							item.timeoutDeferCount = 1
						}
						holeRoute := relay
						wantCarrier := gapHoleCarrierReliable
						if holeDirect {
							holeRoute, wantCarrier = direct, gapHoleCarrierUnreliable
						}
						sequence.observeCarrierWrite(item, transferWriteDisposition{
							route: holeRoute, reliable: !holeDirect, unreliable: holeDirect,
						})
						sequence.sendItems = []*sendItem{item}
						sequence.nextSequenceNumber = 1
						sequence.addResendItem(item)
						recoveryRoute := relay
						if recoveryDirect {
							recoveryRoute = direct
							relay <- nil
						} else {
							direct <- nil
						}
						done := make(chan struct{})
						go func() { defer close(done); sequence.Run() }()
						t.Cleanup(func() { sequence.cancel(); <-done })
						synctest.Wait()
						select {
						case wire := <-recoveryRoute:
							MessagePoolReturn(wire)
						default:
							t.Fatal("the selected recovery lane did not receive the gap write")
						}
						if item.carrierRoute != recoveryRoute {
							t.Fatal("the recovery did not update the item's carrier")
						}
						stats := client.SendRecoveryStats()
						want := [gapHoleCarrierCount]uint64{}
						if deferred {
							want[wantCarrier] = 1
						}
						if stats.SelectiveGapWriteCount != 1 || stats.RecoveryWriteErrorCount != 0 ||
							stats.SelectiveGapWritesOfDeferredItems != want {
							t.Fatalf("gap=%d failed=%d hole attribution=%v, want one write with %v",
								stats.SelectiveGapWriteCount, stats.RecoveryWriteErrorCount,
								stats.SelectiveGapWritesOfDeferredItems, want)
						}
					})
				})
			}
		}
	}
}
