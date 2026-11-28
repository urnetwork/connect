// Transfer dispatch preserves the ordinary carrier for work outside the grant.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The ordinary carrier has a lower priority than the preferred P2P writer.
// Their independent queues make each accepted original share observable.
func streamForwardingScopeTestWriter(t *testing.T, association *streamForwardingAssociation) (*MultiRouteSelector, Route, Route) {
	t.Helper()
	selector := NewMultiRouteSelector(t.Context(), "forwarding-scope", nil, DestinationId(association.sendPath.DestinationId), false)
	t.Cleanup(selector.Close)
	binding := &streamForwardingBinding{readiness: association.readiness}
	binding.association.Store(association)
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
	sender := &P2pSendTransport{
		ctx: t.Context(), transportId: NewId(), streamId: association.grant.path.StreamId, peerId: NewId(),
		forwarding: binding, forwardingPhysicalLimit: 65536, settings: settings,
	}
	forwarded, ordinary := make(Route, 1), make(Route, 1)
	selector.updateTransportWithProperties(sender, []Route{forwarded}, p2pTransferCarrierProperties(sender))
	selector.updateTransportWithProperties(newTypedPriorityRouteTestTransport(TransportTypeH1, 1),
		[]Route{ordinary}, TransferCarrierProperties{})
	return selector, forwarded, ordinary
}

// Both terminal conditions observe custody of exactly the submitted wire.
func requireStreamForwardingScopeRoute(t *testing.T, name string, forwarded, ordinary Route, wire []byte, expected TransportType) {
	t.Helper()
	var actual TransportType
	var received []byte
	select {
	case received = <-forwarded:
		actual = TransportTypeP2p
	case received = <-ordinary:
		actual = TransportTypeH1
	default:
		t.Fatalf("%s did not publish an owned wire", name)
	}
	defer MessagePoolReturn(received)
	if actual != expected || len(received) != len(wire) || &received[0] != &wire[0] || !bytes.Equal(received, wire) {
		t.Fatalf("%s reached %s, want %s with the original wire", name, actual, expected)
	}
	if len(forwarded) != 0 || len(ordinary) != 0 {
		t.Fatalf("%s left a duplicate route share", name)
	}
}

// A P2P affinity keeps eligible work on the enrolled route and leaves other
// native work to the live ordinary sibling.
func TestStreamForwardingApplicationRoutesOnlyEnrolledFrames(t *testing.T) {
	origin, destination := streamForwardingApplicationPair(t)
	send := func(association *streamForwardingAssociation, name string, frame *protocol.TransferFrame, expected TransportType) {
		t.Helper()
		selector, forwarded, ordinary := streamForwardingScopeTestWriter(t, association)
		wire := MessagePoolCopy(marshalStreamForwardingTestFrame(t, frame))
		ok, disposition, err := selector.writeDetailedReplyWithCarrierPreference(t.Context(), wire, 0, TransportTypeP2p)
		if !ok || err != nil {
			MessagePoolReturn(wire)
			t.Fatalf("%s missed both available routes: %v", name, err)
		}
		requireStreamForwardingScopeRoute(t, name, forwarded, ordinary, wire, expected)
		if disposition.transportType != expected {
			t.Fatalf("%s recorded %s, want %s", name, disposition.transportType, expected)
		}
	}
	send(origin, "enrolled DATA", streamForwardingTestPack(origin, []byte("data"), false), TransportTypeP2p)
	other := streamForwardingTestPack(origin, []byte("different sequence"), false)
	other.Pack.SequenceId = NewId().Bytes()
	send(origin, "different native sequence", other, TransportTypeH1)
	other = streamForwardingTestPack(origin, []byte("new grant"), true)
	other.Pack.ContractId = NewId().Bytes()
	send(origin, "successor NoAck grant", other, TransportTypeH1)
	other = streamForwardingTestPack(origin, nil, false)
	other.Pack.Frames[0].MessageType = protocol.MessageType_TransferEncryptedControl
	send(origin, "ordinary control", other, TransportTypeH1)
	send(destination, "reverse application", streamForwardingTestPack(destination, []byte("reply"), false), TransportTypeH1)
	ack := &protocol.TransferFrame{TransferPath: destination.sendPath.ToProtobuf(), Ack: &protocol.Ack{
		MessageId: NewId().Bytes(), SequenceId: destination.grant.sequenceId.Bytes(),
	}}
	send(destination, "matching native ACK", ack, TransportTypeP2p)
	ack.Ack.SequenceId = NewId().Bytes()
	send(destination, "other native ACK", ack, TransportTypeH1)
}

// A blocked write waits for its ordinary sibling. A buffered forwarded queue
// stays available for an eligible native frame throughout.
func TestStreamForwardingApplicationWaitsForEligibleRoute(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		origin, _ := streamForwardingApplicationPair(t)
		selector, forwarded, ordinary := streamForwardingScopeTestWriter(t, origin)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		other := streamForwardingTestPack(origin, []byte("different sequence"), false)
		other.Pack.SequenceId = NewId().Bytes()
		wire := MessagePoolCopy(marshalStreamForwardingTestFrame(t, other))
		held := []byte("prior ordinary write")
		ordinary <- held
		done := make(chan error, 1)
		go func() {
			done <- selector.Write(ctx, wire, time.Second)
		}()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				MessagePoolReturn(wire)
			} else if len(forwarded) != 0 {
				MessagePoolReturn(<-forwarded)
			}
			t.Fatalf("out-of-scope write completed while its ordinary carrier was occupied: %v", err)
		default:
		}
		if previous := <-ordinary; &previous[0] != &held[0] {
			t.Fatal("ordinary queue lost its prior share")
		}
		if err := <-done; err != nil {
			MessagePoolReturn(wire)
			t.Fatal(err)
		}
		requireStreamForwardingScopeRoute(t, "waiting write", forwarded, ordinary, wire, TransportTypeH1)
	})
}
