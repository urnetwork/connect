package connect

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// A probe consumes the caller's packet only when admitted (including the
// deliberately stalled fixture). A refused wrapped-v1 frame must release its
// temporary marshal buffer without releasing the caller's input. The witness
// reference detects a missing or duplicate return even when a later checkout
// could otherwise conceal a pool ownership error.
func TestSingletonProbePacketOwnership(t *testing.T) {
	for _, version := range []int{1, DefaultProtocolVersion} {
		for _, outcome := range []string{
			"ack", "accepted_timeout", "accepted_cancel", "admission_timeout",
			"canceled", "no_client", "stalled",
		} {
			t.Run(fmt.Sprintf("v%d/%s", version, outcome), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					destination := NewId()
					settings := DefaultClientSettings()
					settings.Log = NewNoopLogger()
					settings.ProtocolVersion = version
					settings.SendBufferSettings.ProtocolVersion = version
					settings.EncryptionSettings.Mode = EncryptionModeOff
					settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
					settings.SendBufferSettings.AckTimeout = 30 * time.Second
					if outcome == "admission_timeout" {
						settings.SendBufferSettings.SequenceBufferSize = 0
						settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) { <-ctx.Done() }
					}
					sender := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
					sender.ContractManager().AddNoContractPeer(destination)
					route := make(Route, 16)
					if outcome == "accepted_timeout" || outcome == "accepted_cancel" {
						route = make(Route)
					}
					sender.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{
						sendClientTransport: NewSendClientTransport(DestinationId(destination)),
					}, []Route{route})
					_, channel, _ := probeTestParent(t)
					channel.ctx = ctx
					channel.client = sender
					channel.settings.ProtocolVersion = version
					channel.args.Destination = RequireMultiHopId(destination)
					channel.stalled.Store(outcome == "stalled")
					if outcome == "no_client" {
						channel.client = nil
					}
					if outcome == "canceled" {
						cancel()
					}
					packet := MessagePoolGet(64)
					clear(packet)
					witness := MessagePoolShareReadOnly(packet)
					accepted := channel.sendProbe(&parsedPacket{packet: packet}, 10*time.Millisecond)
					wantAccepted := outcome == "ack" || outcome == "accepted_timeout" ||
						outcome == "accepted_cancel" || outcome == "stalled"
					if accepted != wantAccepted {
						t.Errorf("accepted=%t, want %t", accepted, wantAccepted)
					}
					synctest.Wait()
					if outcome == "ack" {
						for len(route) > 0 {
							wire := <-route
							acknowledgeSendPackLifecycleWirePack(t, sender, destination, decodeSendPackLifecycleWirePack(t, wire))
							MessagePoolReturn(wire)
						}
					} else if outcome == "accepted_timeout" {
						time.Sleep(31 * time.Second)
						synctest.Wait()
					}
					cancel()
					if err := sender.CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
					for len(route) > 0 {
						MessagePoolReturn(<-route)
					}
					if !accepted && MessagePoolReturn(packet) {
						t.Error("refused probe consumed the caller's packet owner")
						return
					}
					if !MessagePoolReturn(witness) {
						t.Errorf("probe retained an owner after terminal %s", outcome)
						MessagePoolReturn(packet)
					}
				})
			})
		}
	}
}
