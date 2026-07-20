package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"slices"
	"testing"
	"time"
)

func TestMultiRoute(t *testing.T) {
	// create route manager
	// add multiple transports and routes
	// multi route write, write a message
	// multi route reader, read a message

	WriteTimeout := 1 * time.Second
	ReadTimeout := 1 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientId := NewId()
	// client := NewClientWithDefaults(ctx, clientId)

	routeManager := NewRouteManager(ctx, "test")

	sendTransports := map[Transport][]Route{}
	receiveTransports := map[Transport][]Route{}

	transportCount := 20
	burstSize := 2048

	multiRouteWriter := routeManager.OpenMultiRouteWriter(DestinationId(clientId))

	multiRouteReader := routeManager.OpenMultiRouteReader(DestinationId(clientId))

	for i := 0; i < transportCount; i += 1 {
		r := make(chan []byte)
		sendRoutes := []Route{r}
		sendTransport := NewSendGatewayTransport()
		receiveRoutes := []Route{r}
		receiveTransport := NewReceiveGatewayTransport()

		sendTransports[sendTransport] = sendRoutes
		receiveTransports[receiveTransport] = receiveRoutes
	}

	go func() {
		for sendTransport, sendRoutes := range sendTransports {
			routeManager.UpdateTransport(sendTransport, sendRoutes)
		}
		for receiveTransport, receiveRoutes := range receiveTransports {
			routeManager.UpdateTransport(receiveTransport, receiveRoutes)
		}
	}()

	messageBytes := func(i int) []byte {
		b := new(bytes.Buffer)
		err := binary.Write(b, binary.LittleEndian, int64(i))
		if err != nil {
			panic(err)
		}
		return b.Bytes()
	}

	go func() {
		for i := 0; i < burstSize; i += 1 {
			multiRouteWriter.Write(ctx, messageBytes(i), WriteTimeout)
		}
	}()

	messages := [][]byte{}

	for i := 0; i < burstSize; i += 1 {
		b, err := multiRouteReader.Read(ctx, ReadTimeout)
		AssertEqual(t, err, nil)
		// AssertEqual(t, messageBytes(i), b)
		messages = append(messages, b)
	}

	AssertEqual(t, burstSize, len(messages))

	littleEndianCmp := func(a []byte, b []byte) int {
		if len(a) < len(b) {
			return -1
		} else if len(b) < len(a) {
			return 1
		}

		for i := len(a) - 1; 0 <= i; i -= 1 {
			aValue := a[i]
			bValue := b[i]
			if aValue < bValue {
				return -1
			} else if bValue < aValue {
				return 1
			}
		}

		return 0
	}
	slices.SortStableFunc(messages, littleEndianCmp)
	for i := 0; i < burstSize; i += 1 {
		AssertEqual(t, messageBytes(i), messages[i])
	}

	for sendTransport, _ := range sendTransports {
		routeManager.RemoveTransport(sendTransport)
	}
	for receiveTransport, _ := range receiveTransports {
		routeManager.RemoveTransport(receiveTransport)
	}
}

func TestP2pSendTransportMatchesPeerDestination(t *testing.T) {
	// when a stream is created, the stream send transport must carry
	// any traffic addressed to the peer,
	// not only traffic tagged with the stream id

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerId := NewId()
	otherId := NewId()
	streamId := NewId()

	routeManager := NewRouteManager(ctx, "test")

	// writers open before the stream transport exists
	peerWriter := routeManager.OpenMultiRouteWriter(DestinationId(peerId))
	defer routeManager.CloseMultiRouteWriter(peerWriter)

	peerStreamWriter := routeManager.OpenMultiRouteWriter(TransferPath{
		DestinationId: peerId,
		StreamId:      streamId,
	})
	defer routeManager.CloseMultiRouteWriter(peerStreamWriter)

	streamWriter := routeManager.OpenMultiRouteWriter(TransferPath{
		StreamId: streamId,
	})
	defer routeManager.CloseMultiRouteWriter(streamWriter)

	otherWriter := routeManager.OpenMultiRouteWriter(DestinationId(otherId))
	defer routeManager.CloseMultiRouteWriter(otherWriter)

	localConn, remoteConn := net.Pipe()
	defer localConn.Close()
	defer remoteConn.Close()

	transportCtx, transportCancel := context.WithCancel(ctx)
	defer transportCancel()

	transport, route := NewP2pSendTransportForPeer(
		transportCtx,
		transportCancel,
		localConn,
		peerId,
		streamId,
		DefaultP2pTransportSettings(),
	)
	routeManager.UpdateTransport(transport, []Route{route})
	defer routeManager.RemoveTransport(transport)

	// the stream transport matches the peer, the peer with stream, and the stream
	AssertEqual(t, 1, len(peerWriter.GetActiveRoutes()))
	AssertEqual(t, 1, len(peerStreamWriter.GetActiveRoutes()))
	AssertEqual(t, 1, len(streamWriter.GetActiveRoutes()))
	// the stream transport does not match other destinations
	AssertEqual(t, 0, len(otherWriter.GetActiveRoutes()))

	// a writer to the peer opened after the stream transport also matches
	latePeerWriter := routeManager.OpenMultiRouteWriter(DestinationId(peerId))
	defer routeManager.CloseMultiRouteWriter(latePeerWriter)
	AssertEqual(t, 1, len(latePeerWriter.GetActiveRoutes()))

	// traffic addressed to the peer flows over the stream transport conn
	message := []byte("traffic to the peer")
	err := peerWriter.Write(ctx, MessagePoolCopy(message), 1*time.Second)
	AssertEqual(t, err, nil)

	remoteConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 64)
	n, err := remoteConn.Read(b)
	AssertEqual(t, err, nil)
	AssertEqual(t, message, b[:n])
}

func TestP2pSendTransportTwoStreamsToSamePeer(t *testing.T) {
	// two streams to the same peer both carry traffic addressed to the peer:
	// a peer destination matches both stream transports,
	// a peer destination tagged with one stream matches both
	// (one by stream id, one by peer),
	// and a pure stream mask has no destination id
	// so it matches only its own stream transport

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerId := NewId()
	streamId1 := NewId()
	streamId2 := NewId()

	routeManager := NewRouteManager(ctx, "test")

	localConn1, remoteConn1 := net.Pipe()
	defer localConn1.Close()
	defer remoteConn1.Close()
	localConn2, remoteConn2 := net.Pipe()
	defer localConn2.Close()
	defer remoteConn2.Close()

	transportCtx, transportCancel := context.WithCancel(ctx)
	defer transportCancel()

	transport1, route1 := NewP2pSendTransportForPeer(
		transportCtx,
		transportCancel,
		localConn1,
		peerId,
		streamId1,
		DefaultP2pTransportSettings(),
	)
	transport2, route2 := NewP2pSendTransportForPeer(
		transportCtx,
		transportCancel,
		localConn2,
		peerId,
		streamId2,
		DefaultP2pTransportSettings(),
	)
	routeManager.UpdateTransport(transport1, []Route{route1})
	routeManager.UpdateTransport(transport2, []Route{route2})
	defer routeManager.RemoveTransport(transport1)
	defer routeManager.RemoveTransport(transport2)

	peerWriter := routeManager.OpenMultiRouteWriter(DestinationId(peerId))
	defer routeManager.CloseMultiRouteWriter(peerWriter)
	AssertEqual(t, 2, len(peerWriter.GetActiveRoutes()))

	peerStream1Writer := routeManager.OpenMultiRouteWriter(TransferPath{
		DestinationId: peerId,
		StreamId:      streamId1,
	})
	defer routeManager.CloseMultiRouteWriter(peerStream1Writer)
	AssertEqual(t, 2, len(peerStream1Writer.GetActiveRoutes()))

	stream1Writer := routeManager.OpenMultiRouteWriter(TransferPath{
		StreamId: streamId1,
	})
	defer routeManager.CloseMultiRouteWriter(stream1Writer)
	AssertEqual(t, 1, len(stream1Writer.GetActiveRoutes()))
}

func TestP2pSendTransportZeroPeerIdMatchesOnlyStream(t *testing.T) {
	// a send transport with no peer id must match only its stream.
	// the control mask and pure stream masks have a zero destination id
	// and must never match a stream transport by peer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamId := NewId()
	otherStreamId := NewId()

	routeManager := NewRouteManager(ctx, "test")

	localConn, remoteConn := net.Pipe()
	defer localConn.Close()
	defer remoteConn.Close()

	transportCtx, transportCancel := context.WithCancel(ctx)
	defer transportCancel()

	transport, route := NewP2pSendTransport(
		transportCtx,
		transportCancel,
		localConn,
		streamId,
		DefaultP2pTransportSettings(),
	)
	routeManager.UpdateTransport(transport, []Route{route})
	defer routeManager.RemoveTransport(transport)

	streamWriter := routeManager.OpenMultiRouteWriter(TransferPath{
		StreamId: streamId,
	})
	defer routeManager.CloseMultiRouteWriter(streamWriter)
	AssertEqual(t, 1, len(streamWriter.GetActiveRoutes()))

	controlWriter := routeManager.OpenMultiRouteWriter(DestinationId(ControlId))
	defer routeManager.CloseMultiRouteWriter(controlWriter)
	AssertEqual(t, 0, len(controlWriter.GetActiveRoutes()))

	otherStreamWriter := routeManager.OpenMultiRouteWriter(TransferPath{
		StreamId: otherStreamId,
	})
	defer routeManager.CloseMultiRouteWriter(otherStreamWriter)
	AssertEqual(t, 0, len(otherStreamWriter.GetActiveRoutes()))
}

func TestP2pSendTransportDowngradeMatchesPeer(t *testing.T) {
	// `Downgrade` mirrors `MatchesSend`: an audit/degrade signal addressed to
	// the peer must shed the stream transport carrying direct-peer traffic,
	// not only a signal tagged with the stream id. A zero peer id must never
	// match a path without a destination id (control / pure stream masks).

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerId := NewId()
	streamId := NewId()

	newTransport := func(withPeer bool) (Transport, context.Context, net.Conn) {
		localConn, remoteConn := net.Pipe()
		t.Cleanup(func() {
			localConn.Close()
			remoteConn.Close()
		})
		transportCtx, transportCancel := context.WithCancel(ctx)
		transportPeerId := Id{}
		if withPeer {
			transportPeerId = peerId
		}
		transport, _ := NewP2pSendTransportForPeer(
			transportCtx,
			transportCancel,
			localConn,
			transportPeerId,
			streamId,
			DefaultP2pTransportSettings(),
		)
		return transport, transportCtx, remoteConn
	}

	// an unrelated destination does not shed the transport
	transport, transportCtx, _ := newTransport(true)
	transport.Downgrade(DestinationId(NewId()))
	select {
	case <-transportCtx.Done():
		t.Fatal("downgrade for an unrelated destination must not cancel the transport")
	default:
	}

	// a signal carrying the peer destination (no stream id) sheds the transport
	transport.Downgrade(DestinationId(peerId))
	select {
	case <-transportCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("downgrade for the peer destination must cancel the transport")
	}

	// a signal tagged with the stream id still sheds the transport
	transport, transportCtx, _ = newTransport(true)
	transport.Downgrade(TransferPath{StreamId: streamId})
	select {
	case <-transportCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("downgrade for the stream must cancel the transport")
	}

	// a zero peer id never matches a path without a destination id
	transport, transportCtx, _ = newTransport(false)
	transport.Downgrade(TransferPath{StreamId: NewId()})
	transport.Downgrade(DestinationId(NewId()))
	select {
	case <-transportCtx.Done():
		t.Fatal("a zero-peer transport must only downgrade on its own stream")
	default:
	}
}

func TestP2pSendTransportReconnectPeerDestination(t *testing.T) {
	// the p2p transport is added to and removed from the route manager
	// as the conn connects, disconnects, and reconnects.
	// a writer to the peer must track the transport across the flaps

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerId := NewId()
	streamId := NewId()

	routeManager := NewRouteManager(ctx, "test")

	peerWriter := routeManager.OpenMultiRouteWriter(DestinationId(peerId))
	defer routeManager.CloseMultiRouteWriter(peerWriter)

	localConn, remoteConn := net.Pipe()
	defer localConn.Close()
	defer remoteConn.Close()

	transportCtx, transportCancel := context.WithCancel(ctx)
	defer transportCancel()

	transport, route := NewP2pSendTransportForPeer(
		transportCtx,
		transportCancel,
		localConn,
		peerId,
		streamId,
		DefaultP2pTransportSettings(),
	)

	routeManager.UpdateTransport(transport, []Route{route})
	AssertEqual(t, 1, len(peerWriter.GetActiveRoutes()))

	routeManager.RemoveTransport(transport)
	AssertEqual(t, 0, len(peerWriter.GetActiveRoutes()))

	routeManager.UpdateTransport(transport, []Route{route})
	defer routeManager.RemoveTransport(transport)
	AssertEqual(t, 1, len(peerWriter.GetActiveRoutes()))

	// a write after reconnect flows over the stream transport conn
	message := []byte("after reconnect")
	err := peerWriter.Write(ctx, MessagePoolCopy(message), 1*time.Second)
	AssertEqual(t, err, nil)

	remoteConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 64)
	n, err := remoteConn.Read(b)
	AssertEqual(t, err, nil)
	AssertEqual(t, message, b[:n])
}

func TestP2pSendTransportPreferredOverGateway(t *testing.T) {
	// with both a gateway transport and a stream transport matching the peer,
	// the stream transport has priority 0 and route weight 1.0
	// which leaves the gateway route weight 0,
	// so all traffic to the peer flows over the stream.
	// the gateway route has no reader,
	// so a message routed to it would never reach the conn
	// and the read below would time out

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerId := NewId()
	streamId := NewId()

	routeManager := NewRouteManager(ctx, "test")

	gatewayRoute := make(chan []byte, 32)
	gatewayTransport := NewSendGatewayTransport()
	routeManager.UpdateTransport(gatewayTransport, []Route{gatewayRoute})
	defer routeManager.RemoveTransport(gatewayTransport)

	localConn, remoteConn := net.Pipe()
	defer localConn.Close()
	defer remoteConn.Close()

	transportCtx, transportCancel := context.WithCancel(ctx)
	defer transportCancel()

	transport, route := NewP2pSendTransportForPeer(
		transportCtx,
		transportCancel,
		localConn,
		peerId,
		streamId,
		DefaultP2pTransportSettings(),
	)
	routeManager.UpdateTransport(transport, []Route{route})
	defer routeManager.RemoveTransport(transport)

	peerWriter := routeManager.OpenMultiRouteWriter(DestinationId(peerId))
	defer routeManager.CloseMultiRouteWriter(peerWriter)
	AssertEqual(t, 2, len(peerWriter.GetActiveRoutes()))

	b := make([]byte, 64)
	burstSize := 16
	for i := 0; i < burstSize; i += 1 {
		message := []byte{byte(i)}
		err := peerWriter.Write(ctx, MessagePoolCopy(message), 1*time.Second)
		AssertEqual(t, err, nil)

		remoteConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := remoteConn.Read(b)
		AssertEqual(t, err, nil)
		AssertEqual(t, message, b[:n])
	}

	// nothing was routed to the gateway
	AssertEqual(t, 0, len(gatewayRoute))
}
