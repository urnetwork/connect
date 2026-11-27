package connect

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// An injected worker exit on either side of a real handoff must release
// the same leaf and parent exactly once. All input packs are ordinary data.
func TestReceivePackWorkerExitBalancesCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, stage := range []string{"pack_released", "head_buffered"} {
		for _, noAck := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				root := NewTransferMemoryBudget(64 * 1024)
				rx := NewTransferMemoryBudgetWithParent(32*1024, root)
				settings := closeWaitClientSettings()
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.EncryptionSettings.Mode = EncryptionModeOff
				settings.ControlPingTimeout = 0
				receive := settings.ReceiveBufferSettings
				receive.ReceiveQueueBudget = rx
				receive.ReceiveQueueMinByteCount = 0
				receive.ReceiveQueueRetainedByteAccounting = true
				receive.AllowLegacyNack = true
				source, sibling := NewId(), NewId()
				interrupted := false
				receive.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, at string) {
					if sequence != nil && sequence.source.SourceId == source && at == stage && !interrupted {
						interrupted = true
						if owner == nil || rx.UsedByteCount() == 0 {
							t.Error("worker had no charged owner")
						}
						panic("synthetic receive worker exit")
					}
				}
				client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
				t.Cleanup(func() {
					cancel()
					if err := client.CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
				})
				in := make(Route, 1)
				client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{in})
				siblingCount := 0
				client.AddReceiveCallback(func(path TransferPath, frames []*protocol.Frame, _ Peer) {
					if path.SourceId == sibling {
						siblingCount += len(frames)
					}
				})
				for _, peer := range []Id{source, sibling} {
					client.ContractManager().AddNoContractPeer(peer)
					pack := newReceiveLoopTestPack(t, NewId(), false, "ordinary data")
					pack.Nack = noAck && peer == source
					wire, err := ProtoMarshal(&protocol.TransferFrame{
						TransferPath: TransferPath{SourceId: peer, DestinationId: client.ClientId()}.ToProtobuf(), Pack: pack,
					})
					if err != nil {
						t.Fatal(err)
					}
					in <- wire
					synctest.Wait()
				}
				if !interrupted || siblingCount != 1 || rx.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
					t.Errorf("stage %s noAck=%t interrupted=%t sibling=%d leaf=%d parent=%d",
						stage, noAck, interrupted, siblingCount, rx.UsedByteCount(), root.UsedByteCount())
				}
			})
		}
	}
}
