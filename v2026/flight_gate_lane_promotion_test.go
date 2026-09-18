// Lane-head transitions keep the probe cadence when recovery changes carriers
// or one cumulative acknowledgement covers both original and repeated writes.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Owns a fixed flight with a measured round trip and deliberately backed-off
// timers. No workers or wall-clock scheduling choose the recovery ordering.
func newLaneHeadPromotionSequence(t *testing.T) (*SendSequence, []*sendItem, Route) {
	t.Helper()
	settings := DefaultSendBufferSettings()
	settings.ReliableLaneProvenRecovery = true
	sequence := &SendSequence{
		client:             &Client{},
		log:                NewNoopLogger(),
		sendBufferSettings: settings,
		resendQueue:        newResendQueue(nil, 0),
		flightController:   newSendFlightController(settings),
		rttWindow: NewRttWindow(NewNoopLogger(), settings.RttWindowSize,
			settings.RttWindowTimeout, settings.RttScale, settings.MinResendInterval,
			settings.RttMinResendInterval, settings.MaxResendInterval),
	}
	now := time.Now()
	sequence.rttWindow.CloseSendTime(uint64(now.Add(-400 * time.Millisecond).UnixMilli()))
	relay := make(Route, 4)
	for index := range 4 {
		item := &sendItem{
			transferItem: transferItem{
				messageId:        NewId(),
				sequenceNumber:   uint64(index + 1),
				messageByteCount: 1,
			},
			sendTime:           now,
			resendTime:         now.Add(settings.MaxResendInterval),
			sendCount:          1,
			transferFrameBytes: MessagePoolGet(1),
		}
		sequence.sendItems = append(sequence.sendItems, item)
		sequence.resendQueue.Add(item)
		sequence.observeCarrierWrite(item, transferWriteDisposition{route: relay, reliable: true})
	}
	t.Cleanup(func() {
		for _, item := range sequence.resendQueue.Clear() {
			item.messagePoolReturn()
		}
	})
	return sequence, append([]*sendItem(nil), sequence.sendItems...), relay
}

// Once the head is rewritten elsewhere it leaves its old lane's outstanding
// set. Its successor must not inherit the departed head's eight-second backoff.
func TestLaneHeadChangingCarrierPromotesItsSuccessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, disposition := range []transferWriteDisposition{
		{unreliable: true},
		{reliable: true},
		{},
	} {
		synctest.Test(t, func(t *testing.T) {
			sequence, items, relay := newLaneHeadPromotionSequence(t)
			disposition.route = make(Route, 4)
			head, successor := items[0], items[1]
			laterDue := items[2].resendTime
			wantDue := time.Now().Add(sequence.rttWindow.ProbeRtt())
			sequence.observeCarrierWrite(head, disposition)
			if !head.carrierChanged || sequence.laneOldestOutstanding(relay) != successor {
				t.Fatal("the rewritten head did not leave its original lane")
			}
			if successor.resendTime != wantDue {
				t.Fatalf("unreliable=%t reliable=%t: successor waits %s after its head changed carriers, want one probe interval %s",
					disposition.unreliable, disposition.reliable, successor.resendTime.Sub(time.Now()), wantDue.Sub(time.Now()))
			}
			if items[2].resendTime != laterDue || sequence.client.SendRecoveryStats().LaneHeadPromotionCount != 1 {
				t.Fatal("a carrier change promoted more than its old lane's next head")
			}
			sequence.observeLaneAck(head, time.Now())
			if highest, _ := sequence.laneHighestAcked(relay); highest != 0 {
				t.Fatalf("the ambiguous acknowledgement proved original-lane delivery through %d", highest)
			}
			if highest, _ := sequence.laneHighestAcked(disposition.route); highest != 0 {
				t.Fatalf("the ambiguous acknowledgement proved replacement-lane delivery through %d", highest)
			}
			if verdict := sequence.laneTimerVerdictFor(successor); verdict != laneTimerSilent {
				t.Fatalf("the successor acquired delivery evidence from the carrier change: %v", verdict)
			}
		})
	}
}

// A transition promotes only an unacknowledged head whose lane rule was active.
// Same-lane writes, later items, and selectively acknowledged items stay inert.
func TestLaneCarrierChangePromotionRequiresAnUnacknowledgedHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		name      string
		itemIndex int
		selective bool
		sameRoute bool
		ruleOff   bool
	}{
		{name: "later item", itemIndex: 2},
		{name: "selectively acknowledged head", selective: true},
		{name: "same carrier", sameRoute: true},
		{name: "lane rule disabled", ruleOff: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			sequence, items, relay := newLaneHeadPromotionSequence(t)
			sequence.sendBufferSettings.ReliableLaneProvenRecovery = !test.ruleOff
			item := items[test.itemIndex]
			item.selectiveAcked = test.selective
			route := make(Route, 4)
			if test.sameRoute {
				route = relay
			}
			due := items[1].resendTime
			sequence.observeCarrierWrite(item, transferWriteDisposition{route: route, reliable: true})
			if items[1].resendTime != due || sequence.client.SendRecoveryStats().LaneHeadPromotionCount != 0 {
				t.Fatalf("%s unexpectedly promoted a lane head", test.name)
			}
		})
	}
}

// Ack coalescing may put an original write before a recovered item in one
// prefix. That first item must not hide the recovery that advances this lane.
func TestLaneCumulativeRecoveryPromotesAfterAnUnresentPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, selectivePrefix := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			sequence, items, relay := newLaneHeadPromotionSequence(t)
			items[0].selectiveAcked = selectivePrefix
			items[1].sendCount = 2
			successor := items[2]
			wantDue := time.Now().Add(sequence.rttWindow.ProbeRtt())
			sequence.receiveAck(items[1].messageId, false, sequenceTag{}, false)
			if sequence.laneOldestOutstanding(relay) != successor {
				t.Fatal("the cumulative acknowledgement did not release its whole prefix")
			}
			if successor.resendTime != wantDue {
				t.Fatalf("selective prefix=%t: cumulative recovery left its successor due in %s, want %s",
					selectivePrefix, successor.resendTime.Sub(time.Now()), wantDue.Sub(time.Now()))
			}
			if sequence.client.SendRecoveryStats().LaneHeadPromotionCount != 1 {
				t.Fatal("the cumulative prefix did not promote exactly one lane head")
			}
		})
	}
}

// Both acknowledgement paths promote a recovered lane's next position while
// preserving unrelated lanes and the free deferral for an original delivery.
func TestLaneHeadAcknowledgementsPromoteOnlyTheirLane(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, selective := range []bool{false, true} {
		for _, sendCount := range []int{1, 2} {
			synctest.Test(t, func(t *testing.T) {
				sequence, items, relay := newLaneHeadPromotionSequence(t)
				other := make(Route, 4)
				for _, index := range []int{1, 3} {
					items[index].carrierRoute = other
					items[index].reliableRoute = other
					sequence.observeLaneSend(items[index])
				}
				head := items[0]
				head.sendCount = sendCount
				successor := items[2]
				otherDue := items[1].resendTime
				wantDue := successor.resendTime
				wantPromotions := uint64(0)
				if sendCount > 1 {
					wantDue = time.Now().Add(sequence.rttWindow.ProbeRtt())
					wantPromotions = 1
				}
				sequence.receiveAck(head.messageId, selective, sequenceTag{}, false)
				if sequence.laneOldestOutstanding(relay) != successor ||
					successor.resendTime != wantDue || items[1].resendTime != otherDue ||
					sequence.client.SendRecoveryStats().LaneHeadPromotionCount != wantPromotions {
					t.Fatalf("selective=%t send count=%d: acknowledgement changed the wrong probe schedule", selective, sendCount)
				}
			})
		}
	}
}
