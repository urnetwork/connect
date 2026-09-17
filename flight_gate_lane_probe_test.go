package connect

// FLIGHTGATEFIX §26.2. A reliable lane's silence is a lane event, and the
// sender can read it by lane without any estimate: a reliable carrier
// retransmits below Transfer, so an item unacknowledged while later items
// on the same route are acknowledged was dropped at an endpoint, and one
// unacknowledged while nothing later on that route is acknowledged is
// queued or stalled. So gap recovery of a reliable-carried hole counts
// only its own route's later acknowledgements, and a timer firing on a
// route that has acknowledged nothing past the item probes that route's
// oldest outstanding item and holds the rest behind it.

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// laneScoreboard is a scoreboard whose items carry two distinct routes: a
// relay and a direct lane.
func laneScoreboard(t testing.TB, laneRule bool) (*SendSequence, []*sendItem, Route, Route) {
	t.Helper()
	sendTime := time.Unix(1_700_000_000, 0)
	sequence, items := newSelectiveAckRecoveryTestSequence(8, sendTime)
	sequence.client = &Client{}
	sequence.sendBufferSettings.ReliableLaneProvenRecovery = laneRule
	sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
	sequence.flightController.applyPolicy(transferFlightPolicySnapshot{
		generation:             1,
		limited:                true,
		reliableRouteAvailable: true,
	})
	sequence.rttWindow.CloseSendTime(uint64(time.Now().Add(-300 * time.Millisecond).UnixMilli()))
	relay := make(Route, 8)
	direct := make(Route, 8)
	return sequence, items, relay, direct
}

// laneHole makes item 0 a hole the relay carried, and acknowledges the
// later items on the stated route.
func laneHole(sequence *SendSequence, items []*sendItem, relay Route, provingRoute Route) {
	hole := items[0]
	hole.reliableCarrierObserved = true
	hole.unreliableFlightTracked = false
	hole.carrierRoute = relay
	for index := 1; index < len(items); index += 1 {
		items[index].selectiveAcked = true
		items[index].carrierRoute = provingRoute
		sequence.observeLaneAck(items[index], time.Now())
	}
}

// A relay-carried hole overtaken by acknowledgements from the direct lane
// is not proven: those replies say nothing about the relay's own leg.
func TestRelayHoleOvertakenByDirectAcksIsNotWritten(t *testing.T) {
	sendTime := time.Unix(1_700_000_000, 0)
	// past any time grace, so only the lane rule can hold it
	currentTime := sendTime.Add(10 * time.Second)
	for _, arm := range []struct {
		name     string
		laneRule bool
		written  bool
	}{
		{"as landed", false, true},
		{"reading the lane", true, false},
	} {
		sequence, items, relay, direct := laneScoreboard(t, arm.laneRule)
		laneHole(sequence, items, relay, direct)
		sequence.scheduleSelectiveAckRecovery(currentTime)
		if items[0].selectiveGapRecovered != arm.written {
			t.Errorf(
				"%s: a relay-carried hole overtaken by three direct-lane acknowledgements was "+
					"recovered=%v, want %v; the relay retransmits below Transfer, so only its own "+
					"acknowledgements prove a hole on it",
				arm.name, items[0].selectiveGapRecovered, arm.written,
			)
		}
	}
}

// A relay-carried hole proven by three later acknowledgements from the
// relay itself is written, on either arm: that is an endpoint drop.
func TestRelayHoleProvenByRelayAcksIsWritten(t *testing.T) {
	sendTime := time.Unix(1_700_000_000, 0)
	currentTime := sendTime.Add(10 * time.Second)
	for _, laneRule := range []bool{false, true} {
		sequence, items, relay, _ := laneScoreboard(t, laneRule)
		laneHole(sequence, items, relay, relay)
		sequence.scheduleSelectiveAckRecovery(currentTime)
		if !items[0].selectiveGapRecovered {
			t.Errorf("lane rule=%v: a relay-carried hole with three later relay acknowledgements "+
				"was not recovered; that is an endpoint drop and it must be", laneRule)
		}
	}
}

// The route's own acknowledgement history is what separates the two, and a
// generation change forgets it.
func TestLaneAcksAreRecordedPerRouteAndResetOnAGeneration(t *testing.T) {
	sequence, items, relay, direct := laneScoreboard(t, true)
	items[1].carrierRoute = relay
	items[1].sequenceNumber = 5
	sequence.observeLaneAck(items[1], time.Now())
	items[2].carrierRoute = direct
	items[2].sequenceNumber = 9
	sequence.observeLaneAck(items[2], time.Now())

	if highest, acked := sequence.laneHighestAcked(relay); !acked || highest != 5 {
		t.Fatalf("the relay's highest acknowledged number reads %d (acked=%v), want 5", highest, acked)
	}
	if highest, acked := sequence.laneHighestAcked(direct); !acked || highest != 9 {
		t.Fatalf("the direct lane's highest reads %d (acked=%v), want 9", highest, acked)
	}
	// it only ever advances
	items[1].sequenceNumber = 2
	sequence.observeLaneAck(items[1], time.Now())
	if highest, _ := sequence.laneHighestAcked(relay); highest != 5 {
		t.Fatalf("the relay's highest moved backwards to %d", highest)
	}
	sequence.resetLaneAcks(2)
	if _, acked := sequence.laneHighestAcked(relay); acked {
		t.Fatal("a route generation change did not forget the route's acknowledgement history")
	}
}

// The rule allocates nothing on the acknowledgement path.
func TestLaneAckTableAllocatesNothing(t *testing.T) {
	sequence, items, relay, _ := laneScoreboard(t, true)
	item := items[1]
	item.carrierRoute = relay
	number := uint64(0)
	if allocs := testing.AllocsPerRun(1000, func() {
		number += 1
		item.sequenceNumber = number
		sequence.observeLaneAck(item, time.Now())
		sequence.laneHighestAcked(relay)
		sequence.laneOldestOutstanding(relay)
	}); allocs != 0 {
		t.Fatalf("the lane acknowledgement table allocates %.1f per acknowledgement", allocs)
	}
}

// The rule is off by default in this commit. It no longer regresses M4's
// contract (ten of ten under the race detector with it on, FLIGHTGATEFIX
// §32.6); the flip is the campaign's decision, and it is one boolean.
func TestLaneProvenRecoveryIsOffByDefault(t *testing.T) {
	if DefaultSendBufferSettings().ReliableLaneProvenRecovery {
		t.Fatal("the lane rule is on by default; the flip is the campaign's decision")
	}
	// the setting is still the way to turn it off
	settings := DefaultSendBufferSettings()
	settings.ReliableLaneProvenRecovery = false
	sequence, _ := newSelectiveAckRecoveryTestSequence(1, time.Unix(1_700_000_000, 0))
	sequence.client = &Client{}
	sequence.sendBufferSettings = settings
	item := &sendItem{reliableCarrierObserved: true, carrierRoute: make(Route, 1)}
	if sequence.laneProvenRecovery(item) {
		t.Fatal("the setting no longer turns the lane rule off")
	}
}

// A fixed outstanding window on a silent reliable lane. The real sender and
// ack pump run against a virtual clock; taking each initial Pack before sending
// the next prevents batching from changing the number of outstanding positions.
// The peer acknowledges the retained window only after the stall ends.
func measureSilentLaneWindow(t *testing.T, windowSize int, laneRule bool) ClientSendRecoveryStatsSnapshot {
	t.Helper()
	var stats ClientSendRecoveryStatsSnapshot
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.MinResendInterval = 750 * time.Millisecond
		settings.SendBufferSettings.RttMinResendInterval = 750 * time.Millisecond
		settings.SendBufferSettings.DeferTimeoutResendWhileCumulativeProgress = true
		settings.SendBufferSettings.ReliableLaneProvenRecovery = laneRule
		// This row owns its window size; admission throughput must not choose it.
		settings.SendBufferSettings.ReliableAdmissionBoundedByDelivery = false
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		peerId := NewId()
		client.ContractManager().AddNoContractPeer(peerId)
		outbound := make(Route, 4*windowSize)
		inbound := make(Route, 4)
		client.RouteManager().UpdateTransport(
			NewSendGatewayTransportWithType(TransportTypeH1), []Route{outbound})
		client.RouteManager().UpdateTransport(
			NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{inbound})
		t.Cleanup(func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("close silent-lane sender: %v", err)
			}
			for _, route := range []Route{outbound, inbound} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
		})

		// Acknowledgement of position one gives every later position one free
		// deferral. Virtual time fixes the round trip independently of host load.
		for index := range 2 {
			sendTransferFlightTestMessage(t, client, peerId, index)
			pack := takeTransferFlightTestPack(t, outbound)
			time.Sleep(200 * time.Millisecond)
			acknowledgeTransferFlightTestPack(t, client, peerId, inbound, pack)
			synctest.Wait()
		}
		var lastPack *protocol.Pack
		var headSequenceNumber uint64
		for index := range windowSize {
			sendTransferFlightTestMessage(t, client, peerId, index+2)
			lastPack = takeTransferFlightTestPack(t, outbound)
			if index == 0 {
				headSequenceNumber = lastPack.SequenceNumber
			} else if lastPack.SequenceNumber != headSequenceNumber+uint64(index) {
				t.Fatalf("initial window position %d has sequence number %d", index, lastPack.SequenceNumber)
			}
			synctest.Wait()
		}
		// At a 750 ms floor, the per-item arm writes at 1.5 s after its
		// free deferral; the lane arm writes at 2.25 s after its doubled
		// deferral. Neither second write is due before the 2.75 s stall ends.
		time.Sleep(2750 * time.Millisecond)
		synctest.Wait()
		stats = client.SendRecoveryStats()

		// Count actual writes as well as counters, after the sender is blocked.
		writeCount := uint64(0)
		writtenSequenceNumbers := map[uint64]bool{}
		for len(outbound) != 0 {
			pack := takeTransferFlightTestPack(t, outbound)
			writtenSequenceNumbers[pack.SequenceNumber] = true
			writeCount += 1
			if laneRule && pack.SequenceNumber != headSequenceNumber {
				t.Errorf("silent lane rewrote position %d behind head %d", pack.SequenceNumber, headSequenceNumber)
			}
		}
		if writeCount != stats.TimeoutResendWriteCount {
			t.Errorf("route carried %d rewrites, counter recorded %d", writeCount, stats.TimeoutResendWriteCount)
		}
		if !laneRule && len(writtenSequenceNumbers) != windowSize {
			t.Errorf("per-item recovery rewrote %d of %d retained positions", len(writtenSequenceNumbers), windowSize)
		}
		acknowledgeTransferFlightTestPack(t, client, peerId, inbound, lastPack)
		synctest.Wait()
		client.sendBuffer.mutex.Lock()
		for id, sequence := range client.sendBuffer.sendSequences {
			if id.Destination == peerId && sequence.resendQueue.Len() != 0 {
				t.Errorf("the resumed peer left %d outstanding positions", sequence.resendQueue.Len())
			}
		}
		client.sendBuffer.mutex.Unlock()
	})
	return stats
}

// Reading a silent lane replaces a whole-window rewrite with its head's probe,
// regardless of how many positions the sender admitted before the stall. The
// old live-link row assumed at least 100; a valid 97-position window failed it.
func TestLaneProbeReplacesTheWholeWindowOnASilentLane(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, windowSize := range []int{97, 128} {
		perItem := measureSilentLaneWindow(t, windowSize, false)
		perLane := measureSilentLaneWindow(t, windowSize, true)
		t.Logf("window=%d: per-item writes=%d; per-lane writes=%d probes=%d deferred=%d held=%d",
			windowSize, perItem.TimeoutResendWriteCount, perLane.TimeoutResendWriteCount,
			perLane.LaneProbeWriteCount, perLane.TimeoutResendDeferCount, perLane.LaneProbeRideCount)
		if perItem.TimeoutResendWriteCount != uint64(windowSize) {
			t.Errorf("per-item recovery wrote %d times, want one per retained position (%d)",
				perItem.TimeoutResendWriteCount, windowSize)
		}
		if perLane.TimeoutResendWriteCount != 1 || perLane.LaneProbeWriteCount != 1 {
			t.Errorf("silent lane wrote %d retransmits and %d probes, want exactly one head probe",
				perLane.TimeoutResendWriteCount, perLane.LaneProbeWriteCount)
		}
		if perLane.LaneProvenTimeoutWriteCount != 0 ||
			perLane.TimeoutResendDeferCount != uint64(windowSize) ||
			perLane.LaneProbeRideCount != uint64(windowSize-1) {
			t.Errorf("window %d: endpoint writes=%d deferrals=%d holds=%d, want 0/%d/%d",
				windowSize, perLane.LaneProvenTimeoutWriteCount,
				perLane.TimeoutResendDeferCount, perLane.LaneProbeRideCount, windowSize, windowSize-1)
		}
	}
}

// A genuine endpoint drop on a reliable lane must still be recovered, and
// not materially later than today.
func TestEndpointDropOnAReliableLaneIsStillRecovered(t *testing.T) {
	if testing.Short() {
		t.Skip("reliable-lane endpoint drop")
	}
	measure := func(laneRule bool) (time.Duration, ClientSendRecoveryStatsSnapshot) {
		harness := newMixedLaneHarnessWithOptions(t, mixedLaneOptions{
			slowLatency:                50 * time.Millisecond,
			slowSerialization:          time.Millisecond,
			directLaneDisabled:         true,
			deferTimeoutResend:         true,
			slowDropFraction:           0.01,
			reliableLaneProvenRecovery: laneRule,
		})
		start := time.Now()
		stats := harness.run(t, 1500)
		return time.Since(start), stats
	}
	asLanded, landedStats := measure(false)
	perLane, laneStats := measure(true)
	t.Logf("as landed: %s, gap=%d rto=%d", asLanded.Truncate(time.Millisecond),
		landedStats.SelectiveGapWriteCount, landedStats.TimeoutResendWriteCount)
	t.Logf("per lane : %s, gap=%d rto=%d probes=%d", perLane.Truncate(time.Millisecond),
		laneStats.SelectiveGapWriteCount, laneStats.TimeoutResendWriteCount,
		laneStats.LaneProbeWriteCount)

	if landedStats.SelectiveGapWriteCount == 0 {
		t.Fatal("no gap recovery on a dropping reliable lane: this no longer reproduces a drop")
	}
	if laneStats.SelectiveGapWriteCount == 0 {
		t.Fatal("reading the lane recovered nothing on a dropping reliable lane")
	}
	// a single lane is unchanged by construction: every later ack is its own
	if tolerance := asLanded + asLanded/2; tolerance < perLane {
		t.Fatalf("reading the lane took %s against %s, so a genuine drop waits materially longer",
			perLane, asLanded)
	}
}

// FLIGHTGATEFIX §33. The campaign's deep wedge, in process. A relay that
// goes silent for longer than the liveness cadence's cap leaves the lane
// rule with no reachable release condition: the rule withholds a firing
// until a later same-lane acknowledgement proves it, and a silent lane
// produces none by construction. The campaign measured what that costs,
// five runs over a hundred seconds in eighty with the rule on against none
// in eighty with it off, one of them twelve minutes on a link carrying
// nothing worse than one per cent loss.
//
// The row holds the relay for twenty seconds, well past the eight-second
// cap on the head probe's cadence, and asks what the two arms do once the
// lane comes back. The stall is common to both, so it is subtracted: what
// is measured is recovery after the impairment ends, which is where the
// campaign's wedges lived.
func TestSilentLaneLongerThanTheProbeCadenceStillDrains(t *testing.T) {
	if testing.Short() {
		t.Skip("relay stall")
	}
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const stall = 20 * time.Second
		measure := func(laneRule bool) (time.Duration, ClientSendRecoveryStatsSnapshot) {
			harness := newMixedLaneHarnessWithOptions(t, mixedLaneOptions{
				slowLatency:                200 * time.Millisecond,
				slowSerialization:          time.Millisecond,
				slowQueueFrames:            1024,
				directLaneDisabled:         true,
				deferTimeoutResend:         true,
				slowStallAfter:             1500 * time.Millisecond,
				slowStallFor:               stall,
				reliableLaneProvenRecovery: laneRule,
			})
			start := time.Now()
			snapshot := harness.run(struct{ testing.TB }{TB: t}, 3000)
			return time.Since(start), snapshot
		}
		perItemElapsed, perItem := measure(false)
		perLaneElapsed, perLane := measure(true)
		t.Logf("per item: %s rto=%d deferred=%d", perItemElapsed,
			perItem.TimeoutResendWriteCount, perItem.TimeoutResendDeferCount)
		t.Logf("per lane: %s rto=%d deferred=%d probes=%d held=%d", perLaneElapsed,
			perLane.TimeoutResendWriteCount, perLane.TimeoutResendDeferCount,
			perLane.LaneProbeWriteCount, perLane.LaneProbeRideCount)

		// Both arms wait out the stall; only what follows it is theirs.
		perItemAfter := perItemElapsed - stall
		perLaneAfter := perLaneElapsed - stall
		if perItemAfter <= 0 || perLaneAfter <= 0 {
			t.Fatalf("the stall did not bind: per item %s, per lane %s against a %s stall",
				perItemElapsed, perLaneElapsed, stall)
		}
		// The campaign's wedges ran one to two orders of magnitude past the
		// rule-off runs of the same cell. A factor of four is well clear of
		// run-to-run spread here and well under anything the campaign saw.
		if 4*perItemAfter < perLaneAfter {
			t.Fatalf(
				"reading the lane took %s to drain after the stall against %s per item, more than "+
					"four times: the rule's release condition is a later same-lane acknowledgement, "+
					"which a silent lane cannot produce, so the sender has no bound of its own "+
					"(FLIGHTGATEFIX §33.9)",
				perLaneAfter, perItemAfter,
			)
		}
		// A wedge is visible in the counters even when the clock happens to
		// escape: the sender holds recovery work it never writes.
		if perLane.TimeoutResendDeferCount != 0 &&
			100*perLane.TimeoutResendWriteCount < perLane.TimeoutResendDeferCount {
			t.Fatalf(
				"reading the lane wrote %d recovery messages against %d deferred, a write-to-defer "+
					"ratio under one per cent: the campaign's failed wedge sat at 0.01 for twelve "+
					"minutes (FLIGHTGATEFIX §33.9)",
				perLane.TimeoutResendWriteCount, perLane.TimeoutResendDeferCount,
			)
		}
	})
}
