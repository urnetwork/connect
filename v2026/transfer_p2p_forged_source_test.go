package connect

// Regression test for frames on a direct peer (P2P) route that name the
// platform or this client as their source. Nothing on the P2P path compares a
// frame's claimed source with the peer at the far end of the association, so
// the receive loop refuses the two sources a peer route can never carry.

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A frame naming the platform would be taken as control-plane traffic, which
// skips contracts and sessions and drives platform-only control; one naming
// this client would be taken as self traffic. Both are dropped and counted,
// and a frame naming the actual peer is still delivered.
func TestP2pRouteFrameClaimingPlatformOrSelfIsDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	peerId := NewId()
	client.ContractManager().AddNoContractPeer(peerId)
	inbound := make(Route, 8)
	client.RouteManager().UpdateTransport(
		NewReceiveGatewayTransportWithType(TransportTypeP2p),
		[]Route{inbound},
	)
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
		for len(inbound) != 0 {
			MessagePoolReturn(<-inbound)
		}
	})

	type delivery struct {
		sourceId Id
		content  string
	}
	delivered := make(chan delivery, 8)
	client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if simpleMessage, ok := message.(*protocol.SimpleMessage); ok {
				select {
				case delivered <- delivery{sourceId: source.SourceId, content: simpleMessage.Content}:
				default:
				}
			}
		}
	})

	send := func(sourceId Id, content string) {
		transferFrameBytes, err := proto.Marshal(&protocol.TransferFrame{
			TransferPath: TransferPath{SourceId: sourceId, DestinationId: client.ClientId()}.ToProtobuf(),
			Pack:         newReceiveLoopTestPack(t, NewId(), false, content),
		})
		if err != nil {
			t.Fatal(err)
		}
		inbound <- transferFrameBytes
	}
	send(ControlId, "as the platform")
	send(client.ClientId(), "as this client")
	send(peerId, "from the peer")

	// the loop handles the route in order, so the peer's delivery follows the
	// verdict on both forged frames
	select {
	case first := <-delivered:
		AssertEqual(t, peerId, first.sourceId)
		AssertEqual(t, "from the peer", first.content)
	case <-client.Done():
		t.Fatal("the client stopped")
	case <-time.After(10 * time.Second):
		t.Fatal("the peer's frame was not delivered")
	}
	AssertEqual(t, uint64(2), client.ReceiveStats().ForgedSourceDropCount)
}
