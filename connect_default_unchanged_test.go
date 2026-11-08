package connect

// A client that opts in to none of SUBPROTOCOL.md §11 behaves as it always has.
// Everything here uses only what predates §11, on purpose: these tests compile
// and pass, unchanged, on the commit before it, so each one states a behavior
// that did not move rather than one that merely holds today. The one thing that
// moved for such a client is §11.5's control-sourced subprotocol frame, which
// subprotocol_source_test.go pins.

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// A default client, and a line per thing it delivered, in order.
type defaultClientFixture struct {
	t          *testing.T
	client     *Client
	deliveries chan string
}

// Default settings with encryption and the control ping off and a quiet log.
// None of the opt-ins under test is set.
func defaultClientFixtureSettings() *ClientSettings {
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	return settings
}

// A default client whose generic receive callback records each simple
// message as "generic <source> <carrier> <content>". The client is canceled
// when the test ends.
func newDefaultClientFixture(t *testing.T) *defaultClientFixture {
	t.Helper()
	self := &defaultClientFixture{
		t:          t,
		client:     NewClient(t.Context(), NewId(), NewNoContractClientOob(), defaultClientFixtureSettings()),
		deliveries: make(chan string, 64),
	}
	t.Cleanup(self.client.Cancel)
	self.client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage {
				continue
			}
			var message protocol.SimpleMessage
			if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
				t.Error(err)
				continue
			}
			self.deliveries <- fmt.Sprintf("generic %s %s %s", defaultClientSourceName(self.client, source), peer.TransportType, message.Content)
		}
	})
	return self
}

// A source as a delivery line names it: control, self or peer.
func defaultClientSourceName(client *Client, source TransferPath) string {
	switch {
	case source.IsControlSource():
		return "control"
	case source.SourceId == client.ClientId():
		return "self"
	default:
		return "peer"
	}
}

// A route of the given carrier into the client.
func (self *defaultClientFixture) route(transportType TransportType) chan []byte {
	route := make(chan []byte)
	self.client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(transportType), []Route{route})
	return route
}

// Hands one sequence's first Pack to a route and takes the frame's bytes.
func (self *defaultClientFixture) inject(route chan []byte, sourceId Id, frames ...*protocol.Frame) {
	self.t.Helper()
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: sourceId, DestinationId: self.client.ClientId()}.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:  NewId().Bytes(),
			SequenceId: NewId().Bytes(),
			Head:       true,
			Frames:     frames,
		},
	})
	if err != nil {
		self.t.Fatal(err)
	}
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	select {
	case route <- wire:
	case <-time.After(30 * time.Second):
		self.t.Fatal("the client did not read its route")
	}
}

// Fails the test unless the next delivery line is `want`, waiting up to 30
// seconds.
func (self *defaultClientFixture) requireDelivery(want string) {
	self.t.Helper()
	select {
	case got := <-self.deliveries:
		if got != want {
			self.t.Fatalf("delivered %q, want %q", got, want)
		}
	case <-time.After(30 * time.Second):
		self.t.Fatalf("%q was not delivered", want)
	}
}

// A simple message frame carrying `content`.
func defaultClientSimpleFrame(content string) *protocol.Frame {
	return RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
}

// A subprotocol frame of `id` carrying `payload`.
func defaultClientSubprotocolFrame(id SubprotocolId, payload string) *protocol.Frame {
	return &protocol.Frame{
		MessageType:  protocol.MessageType_Subprotocol,
		MessageBytes: wrapSubprotocolBytes(id, []byte(payload)),
	}
}

// Peers reach generic callbacks and subprotocol listeners on every carrier, and
// the platform's top-level frames reach the generic callbacks.
func TestDefaultClientDeliversPeersAndControlFrames(t *testing.T) {
	for _, transportType := range []TransportType{
		TransportTypeH1,
		TransportTypeH3,
		TransportTypeH3Dns,
		TransportTypeH3DnsPump,
		TransportTypeP2p,
		TransportTypeUnknown,
	} {
		fixture := newDefaultClientFixture(t)
		route := fixture.route(transportType)
		const id SubprotocolId = 4500
		listened := make(chan string, 4)
		if _, err := fixture.client.AddSubprotocolRawCallback(id, func(source TransferPath, _ SubprotocolId, messageBytes []byte, peer Peer) {
			listened <- fmt.Sprintf("raw %s %s %s", defaultClientSourceName(fixture.client, source), peer.TransportType, messageBytes)
		}); err != nil {
			t.Fatal(err)
		}

		peerId := NewId()
		fixture.client.ContractManager().AddNoContractPeer(peerId)
		fixture.inject(route, peerId, defaultClientSubprotocolFrame(id, "hello"), defaultClientSimpleFrame("peer"))
		fixture.requireDelivery(fmt.Sprintf("generic peer %s peer", transportType))
		select {
		case got := <-listened:
			if want := fmt.Sprintf("raw peer %s hello", transportType); got != want {
				t.Fatalf("listener got %q, want %q", got, want)
			}
		default:
			t.Fatal("a peer's frame did not reach its listener before the batch's generic frames")
		}

		fixture.inject(route, ControlId, defaultClientSimpleFrame("control"))
		fixture.requireDelivery(fmt.Sprintf("generic control %s control", transportType))

		stats := fixture.client.SubprotocolStats()
		if stats.Received != 1 || stats.DroppedUnregistered != 0 || stats.DroppedDecode != 0 || stats.ReceivedById[id] != 1 {
			t.Fatalf("%s: stats %+v", transportType, stats)
		}
	}
}

// A P2P carrier takes any source from a client that did not ask otherwise, a
// multihop far end included: nothing in the receive loop compares them.
func TestDefaultClientP2pRouteAcceptsAnySource(t *testing.T) {
	fixture := newDefaultClientFixture(t)
	route := fixture.route(TransportTypeP2p)
	farId := NewId()
	fixture.client.ContractManager().AddNoContractPeer(farId)

	fixture.inject(route, farId, defaultClientSimpleFrame("far"))
	fixture.requireDelivery("generic peer p2p far")
	fixture.inject(route, ControlId, defaultClientSimpleFrame("control"))
	fixture.requireDelivery("generic control p2p control")
	fixture.inject(route, fixture.client.ClientId(), defaultClientSimpleFrame("self"))
	fixture.requireDelivery("generic self p2p self")
}

// A frame addressed elsewhere still reaches the forward callbacks from any
// source, a P2P route included.
func TestDefaultClientForwardsFromAnySource(t *testing.T) {
	fixture := newDefaultClientFixture(t)
	route := fixture.route(TransportTypeP2p)
	forwards := make(chan TransferPath, 1)
	fixture.client.AddForwardCallback(func(path TransferPath, _ []byte) {
		forwards <- path
	})
	sourceId := NewId()
	destinationId := NewId()
	frame := defaultClientSimpleFrame("elsewhere")
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: sourceId, DestinationId: destinationId}.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:  NewId().Bytes(),
			SequenceId: NewId().Bytes(),
			Head:       true,
			Frames:     []*protocol.Frame{frame},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	MessagePoolReturn(frame.MessageBytes)
	route <- wire
	select {
	case path := <-forwards:
		if path.SourceId != sourceId || path.DestinationId != destinationId {
			t.Fatalf("forwarded %s", path)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("not forwarded")
	}
}

// A receive batch forms from the fields it always did, so wrapped and plaintext
// items of one sequence share a callback, as they did before any key existed.
func TestDefaultClientBatchesWrappedAndPlaintextTogether(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), defaultClientFixtureSettings())
	defer client.Cancel()
	source := SourceId(NewId())
	sequence := NewReceiveSequence(ctx, client, source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
	sequence.peerAudit = NewSequencePeerAudit(client, source, 0)

	var callbacks []int
	callback := func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		callbacks = append(callbacks, len(frames))
	}
	for _, unwrapped := range []bool{true, false, false, true} {
		sequence.receiveHead(&receiveItem{
			frames:          []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage}},
			receiveCallback: callback,
			unwrapped:       unwrapped,
		})
	}
	sequence.flushDeliver()
	if !slices.Equal(callbacks, []int{4}) {
		t.Fatalf("callbacks %v, want one of four frames", callbacks)
	}
}

// The wire: a subprotocol frame's bytes and a transfer frame's bytes are the same,
// byte for byte, and the read loop's path parse agrees with them.
func TestDefaultWireBytesAreUnchanged(t *testing.T) {
	wrapped := wrapSubprotocolBytes(21842, []byte("hint"))
	defer MessagePoolReturn(wrapped)
	if got, want := hex.EncodeToString(wrapped), "08d2aa01120468696e74"; got != want {
		t.Fatalf("subprotocol bytes %s, want %s", got, want)
	}

	path := TransferPath{SourceId: Id{1}, DestinationId: Id{2}}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.TransferFrame{
		TransferPath: path.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:      (Id{3}).Bytes(),
			SequenceId:     (Id{4}).Bytes(),
			SequenceNumber: 7,
			Head:           true,
			Frames: []*protocol.Frame{{
				MessageType:  protocol.MessageType_Subprotocol,
				MessageBytes: []byte("x"),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(wire), "0a240a1002000000000000000000000000000000121001000000000000000000000000000000222f0a1003000000000000000000000000000000121004000000000000000000000000000000180720012a05081e120178"; got != want {
		t.Fatalf("transfer frame %s, want %s", got, want)
	}
	parsed, err := FilteredTransferPath(wire)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != path {
		t.Fatalf("path %s, want %s", parsed, path)
	}
}
