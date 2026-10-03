package connect

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The record36 failure's observed chain is a missing request Pack ACK, then
// the ordinary 30-second send lifetime, then a hard channel error and one TCP
// reset. This deliberately does NOT claim to reproduce why H1 stopped making
// progress: the peer feedback boundary is controlled here, not a random link.
// A timely ACK and an acknowledged predecessor distinguish that ownership
// chain from an echo-server deadline or an aggregate last-progress timer.
func TestReadinessAckExpiryOwnsTcpFlowReset(t *testing.T) {
	for _, mode := range []string{"missing-request-ack", "acknowledged-predecessor", "timely-request-ack"} {
		t.Run(mode, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				log := newRecordingLogger()
				destination := NewId()
				settings := DefaultClientSettings()
				settings.Log = log
				settings.EncryptionSettings.Mode = EncryptionModeOff
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.SendBufferSettings.AckTimeout = DefaultMultiClientSettings().AckTimeout
				client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
				client.ContractManager().AddNoContractPeer(destination)
				route := make(Route, 128)
				client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
				defer func() {
					cancel()
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Errorf("join sender: %v", err)
					}
					for len(route) > 0 {
						MessagePoolReturn(<-route)
					}
				}()
				channel := newPacketTransferTestChannel()
				channel.settings = DefaultMultiClientSettings()
				channel.ctx, channel.cancel, channel.client, channel.log = ctx, cancel, client, log
				channel.clientReceiveUnsub = func() {}
				channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
				parent, _, forwarded, _ := rebindTestParent(t, false, nil)
				parent.log = log
				path := rebindTcpPath(12, 60353)
				path.Syn = false
				flow := rebindTestFlow(parent, channel, path, true)
				defer func() {
					for _, packet := range *forwarded {
						MessagePoolReturn(packet.Packet)
					}
				}()
				send := func(sequence uint32) *protocol.Pack {
					packet := ipOosTcpPacketSequence(path, tcpFlagAck, sequence, []byte("readiness request"))
					accepted, err := channel.SendDetailedWithAck(&parsedPacket{packet: packet, ipPath: path}, time.Second, true)
					if !accepted || err != nil {
						MessagePoolReturn(packet)
						t.Fatalf("request admission=%t/%v", accepted, err)
					}
					synctest.Wait()
					if len(route) != 1 {
						t.Fatalf("initial physical route owners=%d, want1", len(route))
					}
					wire := <-route
					pack := decodeSendPackLifecycleWirePack(t, wire)
					MessagePoolReturn(wire)
					return pack
				}
				var predecessor *protocol.Pack
				if mode == "acknowledged-predecessor" {
					predecessor = send(100)
					time.Sleep(time.Nanosecond) // distinct admission times, not a wall wait
				}
				start := time.Now()
				request := send(200)
				if predecessor != nil {
					time.Sleep(3393 * time.Millisecond)
					acknowledgeSendPackLifecycleWirePack(t, client, destination, predecessor)
					synctest.Wait()
					if !channel.lastSendAckTime.Equal(time.Now()) {
						t.Fatal("predecessor ACK did not reach the real channel callback")
					}
				}
				deadline := start.Add(30 * time.Second)
				time.Sleep(time.Until(deadline) - time.Nanosecond)
				synctest.Wait()
				if _, err := channel.WindowStats(); err != nil || channel.IsDone() || len(*forwarded) != 0 || flow.client.Load() != channel {
					t.Fatalf("request retired before its exact lifetime: %v", err)
				}
				if mode == "timely-request-ack" {
					acknowledgeSendPackLifecycleWirePack(t, client, destination, request)
					synctest.Wait()
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				var exits []string
				for _, line := range log.linesWith("event=sequence_exit") {
					if strings.Contains(line, "destination="+destination.String()) {
						exits = append(exits, line)
					}
				}
				_, err := channel.WindowStats()
				if mode == "timely-request-ack" {
					if err != nil || len(exits) != 0 || channel.IsDone() || len(*forwarded) != 0 || flow.client.Load() != channel {
						t.Fatalf("timely peer feedback reset the live flow: err=%v exits=%v", err, exits)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "Send sequence closed.") || len(exits) != 1 ||
					!strings.Contains(exits[0], "reason=ack_lifetime ") || !strings.Contains(exits[0], "ctx=<nil> parent_ctx=<nil>") ||
					!strings.Contains(exits[0], "message="+RequireIdFromBytes(request.MessageId).String()) {
					t.Fatalf("missing request feedback lost the exact terminal owner: err=%v exits=%v", err, exits)
				}
				// Drive the same two actions performed by resize's structural-error
				// branch, without adding a polling clock to this boundary test.
				channel.Close()
				parent.removeClient(channel)
				if len(*forwarded) != 1 || flow.client.Load() != nil || !channel.IsDone() {
					t.Fatalf("terminal channel did not reset exactly its one flow: packets=%d", len(*forwarded))
				}
				packet := (*forwarded)[0].Packet
				if len(packet) < 40 || packet[9] != byte(ipProtocolNumberTcp) || packet[33]&tcpFlagRst == 0 {
					t.Fatal("flow teardown is not an actual IPv4 TCP reset")
				}
				t.Logf("request elapsed=%s predecessor_ack=%t terminal=%v tcp_reset_count=1", time.Since(start), predecessor != nil, err)
			})
		})
	}
}
