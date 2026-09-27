package connect

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// This is a wire-boundary test, not only a policy-helper test. An explicit
// NoAck hint must not bypass reliable TCP IP delivery in either API. A
// no-contract peer prevents head/contract promotion from hiding such a bug.
func TestTcpTransferAckFinalWireSingletonAndGroup(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("group=%t/requestAck=%t", grouped, requested), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					destination := NewId()
					observer, lifecycle := sendPackLifecycleTestObserver(destination)
					wire := make(chan *protocol.Pack, 4)
					settings := DefaultClientSettings()
					settings.Log = NewNoopLogger()
					settings.EncryptionSettings.Mode = EncryptionModeOff
					settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
					settings.SendBufferSettings.SendPackLifecycleObserver = observer
					settings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
						pack := decodeSendPackLifecycleWirePack(t, observation.TransferFrameBytes)
						if len(pack.Frames) > 0 && pack.Frames[0].MessageType == protocol.MessageType_IpIpPacketToProvider {
							select {
							case wire <- pack:
							default:
								t.Error("bounded wire observer overflow")
							}
						}
					}
					client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
					route := make(Route, 8)
					defer func() {
						cancel()
						client.CloseAndWait(context.Background())
						for len(route) > 0 {
							MessagePoolReturn(<-route)
						}
					}()
					client.ContractManager().AddNoContractPeer(destination)
					client.RouteManager().UpdateTransport(
						&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
					destinationPath, err := NewMultiHopId(destination)
					if err != nil {
						t.Fatal(err)
					}
					channel := newPacketTransferTestChannel()
					channel.ctx, channel.cancel, channel.client = ctx, cancel, client
					channel.args = &multiClientChannelArgs{Destination: destinationPath}
					path := icmpTcpTestPath(4)
					count := 1
					if grouped {
						count = 2
					}
					packets := make([]parsedPacket, count)
					for i := range packets {
						packet := MessagePoolGet(64)
						clear(packet)
						packet[0], packet[9], packet[32] = 0x45, 6, 0x50
						packets[i] = parsedPacket{packet: packet, ipPath: path}
					}
					var accepted bool
					if grouped {
						accepted, err = channel.SendGroupDetailedWithAck(&parsedPacketGroup{
							packets: packets, ipPath: path, byteCount: ByteCount(64 * count),
						}, time.Second, requested)
					} else {
						accepted, err = channel.SendDetailedWithAck(&packets[0], time.Second, requested)
					}
					if err != nil || !accepted {
						for _, packet := range packets {
							MessagePoolReturn(packet.packet)
						}
						t.Fatalf("admission=%t/%v", accepted, err)
					}
					var pack *protocol.Pack
					select {
					case pack = <-wire:
					case <-ctx.Done():
						t.Fatal("no final wire Pack")
					}
					if pack.Nack || len(pack.Frames) != count {
						t.Fatalf("TCP wire Nack=%t frames=%d, want Ack/%d", pack.Nack, len(pack.Frames), count)
					}
					select {
					case bytes := <-route:
						MessagePoolReturn(bytes)
					case <-ctx.Done():
						t.Fatal("wire was not physically written")
					}
					synctest.Wait()
					for len(lifecycle) > 0 {
						observation := <-lifecycle
						if !observation.AckRequired || observation.Phase == SendPackLifecyclePhaseTerminal {
							t.Fatalf("route write released reliable TCP ownership before peer ACK: %+v", observation)
						}
					}
					acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
					for {
						observation := waitSendPackLifecycleObservation(t, ctx, lifecycle)
						if !observation.AckRequired {
							t.Fatal("TCP lifecycle lost ACK-required ownership")
						}
						if observation.Phase == SendPackLifecyclePhaseTerminal {
							if observation.Err != nil {
								t.Fatal(observation.Err)
							}
							break
						}
					}
				})
			})
		}
	}
}

// Reliable Transfer keeps the exact packet while the inner sender's clock
// fires. Collapse prevention suppresses those redundant copies, but its
// bounded escape still admits one retry when the selected client stalls.
func TestTcpTransferAckCollapseCoupling(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("group=%t/direct=%t", grouped, direct), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					const hold = 500 * time.Millisecond
					parent, update := collapseTestClient(hold)
					selected := &multiClientChannel{settings: parent.settings, performanceProfile: &PerformanceProfile{AllowDirect: direct}}
					canSend := func(packet *parsedPacket) bool {
						if !selected.ipPacketTransferAckRequired(packet.ipPath) || !ipPacketTransferAckForRequest(packet.ipPath, false) {
							t.Fatal("TCP collapse lost reliable Transfer ownership")
						}
						if grouped {
							return parent.canSendPacketGroup(&parsedPacketGroup{packets: []parsedPacket{*packet, *packet}, ipPath: packet.ipPath}, update, selected)
						}
						return parent.canSendPacket(packet, update, selected)
					}
					packet := collapseTestPacket(1000, 5000, 100, false, false)
					if !canSend(packet) {
						t.Fatal("first packet was suppressed")
					}
					update.updateSequence(packet)
					for range 8 {
						if canSend(packet) {
							t.Fatal("redundant inner TCP retry bypassed Transfer recovery")
						}
					}
					time.Sleep(hold - time.Nanosecond)
					if canSend(packet) {
						t.Fatal("bounded hold released too early")
					}
					time.Sleep(time.Nanosecond)
					if !canSend(packet) || !canSend(packet) {
						t.Fatal("hold escape must remain immediately retryable until actual admission succeeds")
					}
					// The gate only observes eligibility. Commit the successful
					// selected-client admission, as for the first send above.
					update.updateSequence(packet)
					if canSend(packet) {
						t.Fatal("successful escape admission must restart the bounded hold")
					}
					if !canSend(collapseTestPacket(1100, 5000, 100, false, false)) {
						t.Fatal("new sequence data was suppressed with the duplicates")
					}
				})
			})
		}
	}
}
