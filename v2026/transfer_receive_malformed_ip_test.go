package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Exercise actual wire decoding and the receive worker with a hierarchical
// budget. No contract or no-contract exception authorizes the hostile peer.
func TestReceiveEmptyLegacyIpFramesReturnMemory(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		root := NewTransferMemoryBudget(32 * 1024)
		rx := NewTransferMemoryBudgetWithParent(16*1024, root)
		settings := closeWaitClientSettings()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.ControlPingTimeout = 0
		settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		settings.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		receive := settings.ReceiveBufferSettings
		receive.WindowSizing = WindowSizingConstant
		receive.ApplyWindowSizing()
		receive.ReceiveQueueBudget = rx
		receive.ReceiveQueueMinByteCount = 0
		receive.ReceiveQueueRetainedByteAccounting = true
		receive.PackQueueBudget = nil
		processed := 0
		receive.receiveOwnerStageForTest = func(_ *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if stage == "pack_released" {
				processed++
				if owner == nil || owner.receiveItem.memoryBudget != rx || rx.UsedByteCount() == 0 {
					t.Error("wire frame reached the worker without a receive-memory claim")
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		inbound := make(Route, 1)
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{inbound})
		t.Cleanup(func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(inbound) != 0 {
				MessagePoolReturn(<-inbound)
			}
			assertRetainedBudgetBalance(t, rx)
			assertRetainedBudgetBalance(t, root)
		})
		source := NewId()
		if client.ContractManager().ReceiveNoContract(source) {
			t.Fatal("hostile peer unexpectedly bypasses contracts")
		}
		synctest.Wait()
		for i := range 64 {
			pack := &protocol.Pack{
				MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), Head: true,
				Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider}},
			}
			wire, err := ProtoMarshal(&protocol.TransferFrame{
				TransferPath: TransferPath{SourceId: source, DestinationId: client.ClientId()}.ToProtobuf(),
				Pack:         pack,
			})
			if err != nil {
				t.Fatal(err)
			}
			inbound <- wire
			synctest.Wait()
			if processed != i+1 {
				t.Fatalf("only %d of %d empty frames reached the receive worker", processed, i+1)
			}
			if used := rx.UsedByteCount(); used != 0 {
				t.Fatalf("empty frame %d leaked %d receive bytes", i, used)
			}
			if client.IsDone() {
				t.Fatalf("empty frame %d stopped the client", i)
			}
		}
		// An unrelated authorized peer must still be able to receive after a
		// train whose cumulative claims exceed the entire receive budget.
		healthy := NewId()
		client.ContractManager().AddNoContractPeer(healthy)
		delivered := 0
		client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
			if source.SourceId == healthy {
				delivered += len(frames)
			}
		})
		wire, err := ProtoMarshal(&protocol.TransferFrame{
			TransferPath: TransferPath{SourceId: healthy, DestinationId: client.ClientId()}.ToProtobuf(),
			Pack:         newReceiveLoopTestPack(t, NewId(), false, "after empty frames"),
		})
		if err != nil {
			t.Fatal(err)
		}
		inbound <- wire
		synctest.Wait()
		if delivered != 1 {
			t.Fatalf("healthy peer delivered %d frames after malformed traffic, want 1", delivered)
		}
		if rx.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("receive claims remain after healthy delivery")
		}
	})
}
