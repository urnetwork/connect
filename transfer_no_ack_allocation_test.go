package connect

import (
	"context"
	"github.com/urnetwork/connect/protocol"
	"testing"
	"time"
)

type noAckAllocationTransport struct {
	*sendClientTransport
	kind TransportType
}

func (t *noAckAllocationTransport) TransportType() TransportType { return t.kind }

func newNoAckAllocationSequence(b testing.TB, kind TransportType) (*SendSequence, Route) {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	peer := NewId()
	sequence := NewSendSequence(ctx, client, nil, peer, MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, DefaultSendBufferSettings())
	sequence.sendBuffer = client.sendBuffer
	contract := &sequenceContract{log: settings.Log, contractId: NewId(), transferByteCount: 1 << 50, effectiveTransferByteCount: 1 << 50, path: TransferPath{SourceId: client.ClientId(), DestinationId: peer}}
	sequence.sendContract, sequence.sendContractAcked = contract, true
	sequence.openSendContracts[contract.contractId] = contract
	route := make(Route, 1)
	client.RouteManager().UpdateTransport(&noAckAllocationTransport{sendClientTransport: NewSendClientTransport(DestinationId(peer)), kind: kind}, []Route{route})
	sequence.openContractMultiRouteWriter()
	b.Cleanup(func() {
		sequence.cancel()
		sequence.closeContractMultiRouteWriter()
		cancel()
		join, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			b.Error(err)
		}
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
	})
	return sequence, route
}

func TestNoAckCallerFastPathKeepsModernAllocationBudget(t *testing.T) {
	for _, kind := range []TransportType{TransportTypeH1, TransportTypeH3, TransportTypeP2p} {
		t.Run(string(kind), func(t *testing.T) {
			sequence, route := newNoAckAllocationSequence(t, kind)
			sequence.sendBufferSettings.ProtocolVersion = 2
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals}
			pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: sequence.ctx, Destination: sequence.destination}
			allocations := testing.AllocsPerRun(1000, func() {
				frame.MessageBytes = MessagePoolGet(32)
				clear(frame.MessageBytes)
				snapshot := sequence.readNoAckFastPath(pack)
				if snapshot == nil || !sequence.writeNoAckFastPath(snapshot, pack) {
					MessagePoolReturn(frame.MessageBytes)
					t.Fatal("ready caller write refused")
				}
				MessagePoolReturn(<-route)
				sequence.applyNoAckFastPathAccounting()
			})
			// Baseline retains one 16-byte deferred accounting record. The
			// modern singleton frame array must not escape through the
			// shared serializer's separate legacy-protobuf branch.
			if allocations > 1 {
				t.Fatalf("modern caller allocated %g objects per write; want at most the existing one accounting record", allocations)
			}
		})
	}
}

func TestNoAckCallerFastPathLegacySerializationStillOwnsItsFrames(t *testing.T) {
	for _, kind := range []TransportType{TransportTypeH1, TransportTypeH3, TransportTypeP2p} {
		t.Run(string(kind), func(t *testing.T) {
			sequence, route := newNoAckAllocationSequence(t, kind)
			sequence.sendBufferSettings.ProtocolVersion = 1
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(32)}
			for i := range frame.MessageBytes {
				frame.MessageBytes[i] = byte(i)
			}
			pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: sequence.ctx, Destination: sequence.destination}
			snapshot := sequence.readNoAckFastPath(pack)
			if snapshot == nil || !sequence.writeNoAckFastPath(snapshot, pack) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("ready legacy caller write refused")
			}
			wire := <-route
			var outer protocol.TransferFrame
			if err := ProtoUnmarshal(wire, &outer); err != nil || outer.Frame == nil {
				t.Fatalf("legacy outer decode: %v", err)
			}
			var decoded protocol.Pack
			if err := ProtoUnmarshal(outer.Frame.MessageBytes, &decoded); err != nil {
				t.Fatalf("legacy pack decode: %v", err)
			}
			MessagePoolReturn(wire)
			if !decoded.GetNack() || len(decoded.Frames) != 1 || len(decoded.Frames[0].MessageBytes) != 32 {
				t.Fatal("legacy caller changed pack shape")
			}
			for i, value := range decoded.Frames[0].MessageBytes {
				if value != byte(i) {
					t.Fatalf("legacy frame byte %d = %d", i, value)
				}
			}
			sequence.applyNoAckFastPathAccounting()
			if sequence.sendContract.ackedByteCount != 32 {
				t.Fatalf("legacy write accounting = %d; want 32", sequence.sendContract.ackedByteCount)
			}
		})
	}
}
