package connect

// Temporary coupling 2 measurement. Not committed.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestZzC2ResendDiagnostic(t *testing.T) {
	if os.Getenv("ZZ_C2") == "" {
		t.Skip("diagnostic")
	}
	for _, row := range []struct {
		name      string
		laneFloor ByteCount
	}{
		{name: "from-delivery window", laneFloor: kib(64)},
		{name: "from-delivery window without a lane floor", laneFloor: 0},
	} {
		perSecond, stats, due, atStall := zzExerciseLanePacingResends(t, WindowSizingFromDelivery, row.laneFloor)
		t.Logf("%s: siblings %v", row.name, perSecond)
		t.Logf("%s: due checks per lane %v", row.name, due)
		t.Logf("%s: at stall start: %+v", row.name, atStall)
		t.Logf("%s: at end: %+v", row.name, stats)
	}
}

func zzExerciseLanePacingResends(t *testing.T, windowSizing WindowSizingPolicyKind, laneFloor ByteCount) ([]int, ClientSendRecoveryStatsSnapshot, map[uint32]int64, ClientSendRecoveryStatsSnapshot) {
	var stats, atStall ClientSendRecoveryStatsSnapshot
	dueCounts := map[uint32]int64{}
	var dueLock sync.Mutex
	siblingPerSecond := make([]int, int(lanePacingDuration/time.Second))
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
			value.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
				dueLock.Lock()
				dueCounts[id.LogicalLane]++
				dueLock.Unlock()
			}
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
			path := lanePacingFlowPath(flow)
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
		case <-time.After(lanePacingStallStart):
		case <-ctx.Done():
		}
		atStall = sender.SendRecoveryStats()
		select {
		case <-time.After(lanePacingDuration - lanePacingStallStart):
		case <-ctx.Done():
		}
		stats = sender.SendRecoveryStats()
	})
	dueLock.Lock()
	defer dueLock.Unlock()
	copied := map[uint32]int64{}
	for k, v := range dueCounts {
		copied[k] = v
	}
	return siblingPerSecond, stats, copied, atStall
}
