// Logical data lanes exist so that one IP flow its receiver cannot drain does
// not hold up the flows beside it (transfer_logical_lane.go). Every lane to a
// destination shares one window pacing service (SendBuffer windowPacingServices,
// keyed by the lane-zero base). That service counts the bytes a stalled lane
// leaves unacknowledged as queueing on the shared carrier
// (windowPacingService.backloggedWithLock), and then paces every sibling lane
// down to the rate the stalled lane is served.
package connect

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A data-only H1 send gateway, so the sender's route policy is the one a
// platform H1 carrier gets.
type lanePacingTestTransport struct {
	transportId Id
}

func (self *lanePacingTestTransport) TransportId() Id                  { return self.transportId }
func (self *lanePacingTestTransport) TransportType() TransportType     { return TransportTypeH1 }
func (self *lanePacingTestTransport) Priority() int                    { return 100 }
func (self *lanePacingTestTransport) Weight() float32                  { return 0 }
func (self *lanePacingTestTransport) MatchesReceive(TransferPath) bool { return false }
func (self *lanePacingTestTransport) Downgrade(TransferPath)           {}
func (self *lanePacingTestTransport) CanEvalRouteWeight(*RouteStats, map[Transport]*RouteStats) bool {
	return true
}
func (self *lanePacingTestTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	return 1.0 / float32(1+len(remainingStats))
}
func (self *lanePacingTestTransport) MatchesSend(destination TransferPath) bool {
	return destination.DestinationId != ControlId
}

const (
	lanePacingFlowCount     = 6
	lanePacingSendInterval  = 6 * time.Millisecond
	lanePacingStallStart    = 3 * time.Second
	lanePacingStallPerBatch = time.Second
	lanePacingDuration      = 45 * time.Second
)

// Six flows on eight lanes each offer one 2000-byte message every 6ms. From
// lanePacingStallStart on, the receiver blocks one second in every delivery
// that carries flow 0, as a provider does when that flow's NAT queue refuses
// reliable ingress. Returns, per second, the deliveries of flows 1-5 together.
func exerciseLanePacingIsolation(t *testing.T, windowSizing WindowSizingPolicyKind, laneFloor ByteCount) []int {
	siblingPerSecond := make([]int, int(lanePacingDuration/time.Second))
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		log := &receiveGapTestLogger{Logger: NewNoopLogger()}
		settings := func() *ClientSettings {
			value := DefaultClientSettingsWithBufferSize(64)
			value.Log = log
			value.EncryptionSettings.Mode = EncryptionModeOff
			value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
			value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
			value.ContractManagerSettings.LegacyCreateContract = false
			value.ContractManagerSettings.InitialContractTransferByteCount = mib(256)
			value.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = mib(256)
			value.ContractManagerSettings.StandardContractTransferByteCount = mib(256)
			value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			// eight data lanes, as sdk mobile_memory_policy.go sets for H1
			value.SendBufferSettings.LogicalDataLaneCount = 8
			value.SendBufferSettings.LaneFloorByteCount = laneFloor
			value.SendBufferSettings.WindowSizing = windowSizing
			value.SendBufferSettings.ApplyWindowSizing()
			return value
		}
		start := time.Now()
		senderId, receiverId := NewId(), NewId()
		var sender, receiver *Client
		sender = NewClient(ctx, senderId, &idleContractTestOob{sourceId: senderId, peer: func() *Client { return receiver }}, settings())
		receiver = NewClient(ctx, receiverId, &idleContractTestOob{sourceId: receiverId, peer: func() *Client { return sender }}, settings())

		// one in-order relay per direction with a fixed 5ms one-way latency
		forwardIn, forwardOut := make(chan []byte, 1024), make(chan []byte, 1024)
		reverseIn, reverseOut := make(chan []byte, 1024), make(chan []byte, 1024)
		const latency = 5 * time.Millisecond
		type delayed struct {
			wire []byte
			at   time.Time
		}
		var workers sync.WaitGroup
		var lines []chan delayed
		relay := func(in <-chan []byte, out chan<- []byte) {
			line := make(chan delayed, 8192)
			lines = append(lines, line)
			workers.Add(2)
			go func() {
				defer workers.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case entry := <-line:
						if wait := time.Until(entry.at); 0 < wait {
							select {
							case <-time.After(wait):
							case <-ctx.Done():
								MessagePoolReturn(entry.wire)
								return
							}
						}
						select {
						case out <- entry.wire:
						case <-ctx.Done():
							MessagePoolReturn(entry.wire)
							return
						}
					}
				}
			}()
			go func() {
				defer workers.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case wire := <-in:
						select {
						case line <- delayed{wire: wire, at: time.Now().Add(latency)}:
						case <-ctx.Done():
							MessagePoolReturn(wire)
							return
						}
					}
				}
			}()
		}
		relay(forwardIn, forwardOut)
		relay(reverseIn, reverseOut)
		defer func() {
			cancel()
			workers.Wait()
			owner, release := context.WithTimeout(context.Background(), 5*time.Second)
			defer release()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(owner); err != nil {
					t.Error(err)
				}
			}
			for _, route := range []chan []byte{forwardIn, forwardOut, reverseIn, reverseOut} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
			// frames still on the simulated wire when the relays stopped
			for _, line := range lines {
				for len(line) != 0 {
					MessagePoolReturn((<-line).wire)
				}
			}
		}()
		sender.RouteManager().UpdateTransport(&lanePacingTestTransport{transportId: NewId()}, []Route{forwardIn})
		receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{forwardOut})
		receiver.RouteManager().UpdateTransport(&lanePacingTestTransport{transportId: NewId()}, []Route{reverseIn})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{reverseOut})
		for _, client := range []*Client{sender, receiver} {
			client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		}

		var countLock sync.Mutex
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			now := time.Now()
			second := int(now.Sub(start) / time.Second)
			stall := false
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TestSimpleMessage {
					continue
				}
				message, err := FromFrame(frame)
				if err != nil {
					continue
				}
				if flow := message.(*protocol.SimpleMessage).Content[0]; flow == 'a' {
					stall = true
				} else if second < len(siblingPerSecond) {
					countLock.Lock()
					siblingPerSecond[second] += 1
					countLock.Unlock()
				}
			}
			if stall && lanePacingStallStart <= now.Sub(start) {
				select {
				case <-time.After(lanePacingStallPerBatch):
				case <-ctx.Done():
				}
			}
		})

		for flow := range lanePacingFlowCount {
			content := string(rune('a'+flow)) + strings.Repeat("x", 1999)
			path := &IpPath{
				Version: 4, Protocol: IpProtocolTcp,
				SourceIp: net.IPv4(10, 0, 0, 1), SourcePort: 40000 + flow,
				DestinationIp: net.IPv4(10, 0, 0, 2), DestinationPort: 443,
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				for time.Since(start) < lanePacingDuration {
					select {
					case <-time.After(lanePacingSendInterval):
					case <-ctx.Done():
						return
					}
					frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
					if !sender.SendWithTimeout(frame, receiverId, func(error) {}, -1, scheduleIpFlow(path)) {
						MessagePoolReturn(frame.MessageBytes)
						return
					}
				}
			}()
		}
		select {
		case <-time.After(lanePacingDuration):
		case <-ctx.Done():
		}
	})
	return siblingPerSecond
}

// While one flow's receiver drains a single delivery per second, the other
// five flows keep at least half of what they offer in every three-second
// window. The constant window keeps them at their full offered rate; the
// shipping from-delivery rule must too, since each flow has its own lane.
func TestLogicalLanesIsolateAFlowItsReceiverCannotDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("45s of virtual time over the full Transfer stack")
	}
	offered := float64((lanePacingFlowCount - 1) * int(time.Second/lanePacingSendInterval))
	for _, row := range []struct {
		name      string
		sizing    WindowSizingPolicyKind
		laneFloor ByteCount
	}{
		{"constant window", WindowSizingConstant, kib(64)},
		{"from-delivery window", WindowSizingFromDelivery, kib(64)},
		// the sdk's mobile H1 profile sets the lanes without a floor
		{"from-delivery window without a lane floor", WindowSizingFromDelivery, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			perSecond := exerciseLanePacingIsolation(t, row.sizing, row.laneFloor)
			t.Logf("sibling deliveries per second: %v", perSecond)
			// from five seconds after the stall begins to the end of the run
			for second := int((lanePacingStallStart + 5*time.Second) / time.Second); second+3 < len(perSecond); second++ {
				window := perSecond[second] + perSecond[second+1] + perSecond[second+2]
				if float64(window) < 0.5*3*offered {
					t.Fatalf("seconds %d-%d: the unstalled flows delivered %d messages, under half the %.0f they offered", second, second+3, window, 3*offered)
				}
			}
		})
	}
}
