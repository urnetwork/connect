// SDK constructor profiles keep constrained Transfer performance tied to
// actual device settings without importing a dependent package or using a
// mobile runtime. Physical H1/TUN queues remain separate instruments.
package connect

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// Captured by tools/throughput-fix-2-sdk-settings.py through the real SDK
// constructors and sizing helpers. Each cell retains these resolved values.
//
//go:embed testdata/window_sdk_profiles.json
var windowSdkProfileBytes []byte

// Nil pools mean unbudgeted; a zero-sized pool would still be a real bound.
// Every field below is an allowlisted constructor value, not live app data.
type windowPathEndpointProfile struct {
	Name                         string
	MobilePolicy                 bool
	ExplicitH1                   bool
	ProcessBudget                ByteCount
	DeviceTarget                 ByteCount
	Providing                    bool
	WindowSizing                 WindowSizingPolicyKind
	ClientSendQueueCount         int
	SendQueueCount               int
	AckQueueCount                int
	SendPool                     *ByteCount
	ReceivePool                  *ByteCount
	PackPool                     *ByteCount
	SendInitial                  ByteCount
	SendMinimum                  ByteCount
	WindowScale                  int
	TargetGoodputByteRate        ByteCount
	LaneFloor                    ByteCount
	WindowCeiling                ByteCount
	ReceiveMaximum               ByteCount
	ReceiveMinimum               ByteCount
	RetainedReceiveAccounting    bool
	RetainedPackAccounting       bool
	AdvertiseReceiveWindow       bool
	AckCompressionNs             time.Duration
	LogicalDataLanes             int
	H1QueueCount                 int
	H1QueueBytes                 ByteCount
	H1QueueAdaptiveCount         int
	H1QueueAdaptiveStepCount     int
	H1QueueAdaptiveThreshold     int
	H1QueueAdaptiveWindowNs      time.Duration
	H1QueueAdaptiveBytes         ByteCount
	H1QueueAdaptiveStepBytes     ByteCount
	H1PackHandoffTimeoutNs       time.Duration
	ReliablePackHandoffTimeoutNs time.Duration
	H1AckHandoffTimeoutNs        time.Duration
	ReceiveQueueCount            int
	ReceiveQueueBytes            ByteCount
	MinimumMessageLimit          ByteCount
}

// Install the captured limits after selecting the experimental window rule.
// A fresh pool belongs to this endpoint; its logical lanes share that pool.
func (self *windowPathEndpointProfile) apply(settings *ClientSettings) {
	pool := func(total *ByteCount) *TransferMemoryBudget {
		if total == nil {
			return nil
		}
		return NewTransferMemoryBudget(*total)
	}
	settings.SendBufferSize = self.ClientSendQueueCount
	send, receive := settings.SendBufferSettings, settings.ReceiveBufferSettings
	send.SequenceBufferSize, send.AckBufferSize = self.SendQueueCount, self.AckQueueCount
	send.ResendQueueBudget = pool(self.SendPool)
	send.ResendQueueMaxByteCount, send.ResendQueueMinByteCount = self.SendInitial, self.SendMinimum
	send.LogicalDataLaneCount, send.LaneFloorByteCount = self.LogicalDataLanes, self.LaneFloor
	send.DeliverySizedWindowCeilingByteCount = self.WindowCeiling
	if send.WindowSizing == WindowSizingFromDelivery {
		send.DeliverySizedWindowScale, send.TargetGoodputByteRate = self.WindowScale, self.TargetGoodputByteRate
	}
	receive.ReceiveQueueBudget, receive.PackQueueBudget = pool(self.ReceivePool), pool(self.PackPool)
	receive.ReceiveQueueMaxByteCount, receive.ReceiveQueueMinByteCount = self.ReceiveMaximum, self.ReceiveMinimum
	receive.ReceiveQueueRetainedByteAccounting, receive.PackQueueRetainedByteAccounting = self.RetainedReceiveAccounting, self.RetainedPackAccounting
	receive.AdvertiseReceiveWindow, receive.AckCompressTimeout = self.AdvertiseReceiveWindow, self.AckCompressionNs
	receive.SequenceBufferSize, receive.SequenceBufferByteCount = self.ReceiveQueueCount, self.ReceiveQueueBytes
	receive.H1SequenceBufferSize, receive.H1SequenceBufferByteCount = self.H1QueueCount, self.H1QueueBytes
	receive.H1SequenceBufferAdaptiveMaxSize = self.H1QueueAdaptiveCount
	receive.H1SequenceBufferAdaptiveStepSize = self.H1QueueAdaptiveStepCount
	receive.H1SequenceBufferAdaptiveSaturationThreshold = self.H1QueueAdaptiveThreshold
	receive.H1SequenceBufferAdaptiveSaturationWindow = self.H1QueueAdaptiveWindowNs
	receive.H1SequenceBufferAdaptiveMaxByteCount = self.H1QueueAdaptiveBytes
	receive.H1SequenceBufferAdaptiveStepByteCount = self.H1QueueAdaptiveStepBytes
	receive.H1PackHandoffTimeout, receive.ReliablePackHandoffTimeout = self.H1PackHandoffTimeoutNs, self.ReliablePackHandoffTimeoutNs
	receive.H1AckHandoffTimeout = self.H1AckHandoffTimeoutNs
}

// The reference is limited by the same configured memory and peer permission.
// An unbudgeted sender cannot grow beyond its constructor's initial window.
func (self *windowPathEndpointProfile) windowLimit(peer *windowPathEndpointProfile) ByteCount {
	limit := self.SendInitial
	if self.SendPool != nil {
		limit = *self.SendPool
	}
	if self.WindowCeiling > 0 {
		limit = min(limit, self.WindowCeiling)
	}
	if peer.AdvertiseReceiveWindow {
		limit = min(limit, peer.ReceiveMaximum)
		if peer.ReceivePool != nil {
			limit = min(limit, *peer.ReceivePool)
		}
	}
	return limit
}

// Load the pinned constructor evidence once per top-level test. The fixture
// digest travels with every measured cell and is included in runner manifests.
func windowSdkProfiles(t *testing.T) ([]windowPathEndpointProfile, string) {
	t.Helper()
	var fixture struct {
		Profiles []windowPathEndpointProfile
	}
	if err := json.Unmarshal(windowSdkProfileBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Profiles) != 11 {
		t.Fatalf("incomplete SDK settings capture: %d profiles", len(fixture.Profiles))
	}
	return fixture.Profiles, fmt.Sprintf("%x", sha256.Sum256(windowSdkProfileBytes))
}

// Cross every constructor profile with the budgeted server in both directions.
// Short, medium and long feedback paths expose queue and memory binders; one
// and eight flows distinguish a single data lane from shared lane contention.
func TestWindowPathSdkProfiles(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	server := &profiles[1]
	for i := range profiles {
		for _, reverse := range []bool{false, true} {
			for _, roundTrip := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond, 400 * time.Millisecond} {
				for _, flows := range []int{1, 8} {
					sender, receiver := server, &profiles[i]
					if reverse {
						sender, receiver = receiver, sender
					}
					checkWindowSdkCell(t, windowPathCell{SenderProfile: sender, ReceiverProfile: receiver,
						ProfileFixtureSha256: digest, RoundTrip: roundTrip, Flows: flows,
						RoundRobinOffer: true, Payload: 1280, Rate: 125000000})
				}
			}
		}
	}
}

// Both serialized directions carry data and their peer's compressed ACKs.
// The same constrained endpoint must keep both directions moving at once.
func TestWindowPathSdkBidirectional(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	for _, profile := range profiles {
		if !profile.ExplicitH1 {
			continue
		}
		for _, roundTrip := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond, 400 * time.Millisecond} {
			for _, flows := range []int{1, 8} {
				checkWindowSdkCell(t, windowPathCell{SenderProfile: &profiles[1], ReceiverProfile: &profile,
					ProfileFixtureSha256: digest, Bidirectional: true, RoundTrip: roundTrip,
					Upload: flows == 8, Flows: flows, RoundRobinOffer: true, Payload: 1280, Rate: 125000000})
			}
		}
	}
}

// Compare each direction to its own constrained ceiling. A fast sibling may
// not hide a stalled direction or lane, and no receiver refusal is acceptable.
func checkWindowSdkCell(t *testing.T, cell windowPathCell) {
	t.Helper()
	residence := cell.RoundTrip + max(cell.SenderProfile.AckCompressionNs, cell.ReceiverProfile.AckCompressionNs)
	sender, receiver := cell.SenderProfile, cell.ReceiverProfile
	if cell.Upload {
		sender, receiver = receiver, sender
	}
	directions := [][2]*windowPathEndpointProfile{{sender, receiver}}
	limit := sender.windowLimit(receiver)
	if cell.Bidirectional {
		limit = min(limit, receiver.windowLimit(sender))
		directions = append(directions, [2]*windowPathEndpointProfile{receiver, sender})
	}
	duration := time.Second
	if float64(limit)/residence.Seconds() < float64(cell.Rate)/2 {
		duration = max(duration, 20*residence)
	}
	var reference, candidate windowPathReading
	for _, arm := range []string{"ceiling", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			trial := cell
			trial.Arm, trial.Drop = arm, arm == "delivery"
			reading := measureWindowPathCell(t, trial, duration)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				reference = reading
			} else {
				candidate = reading
			}
		})
	}
	t.Logf("SDK sender=%s/%d/%t/%t receiver=%s/%d/%t/%t rtt=%s flows=%d both=%t reference=%.6f candidate=%.6f direction=%v/%v",
		cell.SenderProfile.Name, cell.SenderProfile.ProcessBudget, cell.SenderProfile.MobilePolicy, cell.SenderProfile.Providing,
		cell.ReceiverProfile.Name, cell.ReceiverProfile.ProcessBudget, cell.ReceiverProfile.MobilePolicy, cell.ReceiverProfile.Providing,
		cell.RoundTrip, cell.Flows, cell.Bidirectional, reference.Mbps, candidate.Mbps, reference.DirectionMbps, candidate.DirectionMbps)
	for _, reading := range []windowPathReading{reference, candidate} {
		if reading.Mbps <= 0 || reading.MinFlowMbps <= 0 || reading.MeasurementRelayDrops != 0 {
			t.Errorf("SDK %s dropped traffic or stalled a flow", reading.Cell.Arm)
		}
		for direction, rate := range reading.DirectionMbps {
			// A relative comparison cannot detect a missing serializer in both arms.
			ceiling := 1.01 * float64(max(cell.Rate, cell.RateAfter)) * 8 / 1e6
			if rate > ceiling {
				t.Errorf("SDK %s direction %d exceeds the physical link: %.6f > %.6f Mb/s", reading.Cell.Arm, direction, rate, ceiling)
			}
		}
		for _, receive := range []ClientReceiveStatsSnapshot{reading.Receiver, reading.SenderReceive} {
			if receive.ReceiveQueueDropCount != 0 || receive.ReceiveQueueEvictionCount != 0 ||
				receive.PackHandoffDropCount != 0 || receive.AckHandoffDropCount != 0 {
				t.Errorf("SDK %s refused traffic at a receive handoff", reading.Cell.Arm)
			}
		}
	}
	if candidate.Mbps < .9*reference.Mbps || candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
		t.Error("SDK profile lost attainable capacity or exceeded the finite relay queue")
	}
	for i, rate := range reference.DirectionMbps {
		peer := directions[i]
		feedbackSeconds := (cell.RoundTrip + peer[1].AckCompressionNs).Seconds()
		if cell.Bidirectional {
			// The FIFO can queue an ACK behind one opposing data flight.
			// Include that bounded residence in the calibration lower bound.
			lanes := max(1, min(cell.Flows, peer[1].LogicalDataLanes))
			opposingFlight := float64(peer[1].windowLimit(peer[0])) * float64(lanes)
			if peer[1].SendPool != nil {
				floor := peer[1].SendMinimum
				if peer[1].LogicalDataLanes > 0 {
					floor = peer[1].LaneFloor
				}
				opposingFlight = min(opposingFlight, float64(*peer[1].SendPool)+float64(floor)*float64(lanes))
			}
			feedbackSeconds += opposingFlight / float64(cell.Rate)
		}
		windowRate := float64(peer[0].windowLimit(peer[1])) / feedbackSeconds
		// Allow framing and interval-edge losses in the instrument, while
		// rejecting an underfilled constant-window reference as calibration.
		if rate < .85*min(float64(cell.Rate), windowRate)*8/1e6 || candidate.DirectionMbps[i] < .9*rate {
			t.Errorf("SDK direction %d reference=%.6f candidate=%.6f", i, rate, candidate.DirectionMbps[i])
		}
	}
	if candidate.Window.Window > sender.windowLimit(receiver) ||
		cell.Bidirectional && candidate.ReverseWindow.Window > receiver.windowLimit(sender) {
		t.Error("SDK profile exceeded its window permission")
	}
}
