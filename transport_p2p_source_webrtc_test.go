//go:build !js

package connect

// The P2P source rule over real loopback WebRTC associations, with production
// StreamSequences publishing the routes: the wiring that names a route's peer
// is exercised here and nowhere else. A forged frame is written into the
// peer's real send route, so it crosses the association exactly as a hostile
// peer's would.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The clients of one stream, in hop order, each with its own stream manager.
type p2pSourceStream struct {
	t        *testing.T
	ctx      context.Context
	streamId Id
	router   *streamIntermediarySignalRouter
	// signals readiness of the first client's stream route
	ready chan struct{}
}

// A no-contract client on close-wait settings, changed by each of `configure`
// in order.
func p2pSourceStreamClient(ctx context.Context, clientId Id, configure ...func(*ClientSettings)) *Client {
	settings := closeWaitClientSettings()
	for _, change := range configure {
		change(settings)
	}
	return NewClient(ctx, clientId, NewNoContractClientOob(), settings)
}

// A WebRTC manager for `clientId` that signals through the stream's in-memory
// router and gathers loopback candidates only, with no ICE servers.
func (self *p2pSourceStream) webRtcManager(clientId Id) *WebRtcManager {
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.UseLoopbackOnlyIceInterfaces = true
	return newTestWebRtcManager(self.t, self.ctx, self.router.sender(clientId), settings)
}

// A stream manager whose endpoints probe the whole path quickly. Readiness is
// the barrier: an endpoint's route is selectable only once the probe crossed
// every hop, so the first frame cannot race the association.
func (self *p2pSourceStream) streamManager(client *Client, webRtcManager *WebRtcManager, observeReady bool) *StreamManager {
	settings := DefaultStreamManagerSettings()
	transportSettings := settings.StreamBufferSettings.P2pTransportSettings
	transportSettings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
	transportSettings.EndToEndProbeInterval = 5 * time.Millisecond
	transportSettings.EndToEndProbeTimeout = 100 * time.Millisecond
	if observeReady {
		transportSettings.EndToEndProbeObserver = func(event P2pStreamProbeEvent) {
			if event.Type == P2pStreamProbeEventReadinessGranted {
				select {
				case self.ready <- struct{}{}:
				default:
				}
			}
		}
	}
	return NewStreamManager(self.ctx, client, webRtcManager, settings)
}

// Fails the test unless the stream is ready end to end within 30 seconds, and
// at once if in-memory signaling fails.
func (self *p2pSourceStream) requireReady() {
	self.t.Helper()
	select {
	case <-self.ready:
	case err := <-self.router.errors:
		self.t.Fatalf("in-memory signaling failed: %v", err)
	case <-time.After(30 * time.Second):
		self.t.Fatal("the stream did not become ready end to end")
	}
}

// Starts a stream through the clients in hop order. Passive sides register
// before their active neighbor offers. The first client is the source, whose
// endpoint carries the readiness signal.
func newP2pSourceStream(t *testing.T, ctx context.Context, clients ...*Client) *p2pSourceStream {
	t.Helper()
	self := &p2pSourceStream{
		t:        t,
		ctx:      ctx,
		streamId: NewId(),
		router:   newStreamIntermediarySignalRouter(),
		ready:    make(chan struct{}, 1),
	}
	webRtcManagers := make([]*WebRtcManager, len(clients))
	streamManagers := make([]*StreamManager, len(clients))
	for i, client := range clients {
		webRtcManagers[i] = self.webRtcManager(client.ClientId())
		self.router.register(client.ClientId(), webRtcManagers[i])
		streamManagers[i] = self.streamManager(client, webRtcManagers[i], i == 0)
	}
	for i := len(clients) - 1; 0 <= i; i -= 1 {
		var sourceId *Id
		var destinationId *Id
		if 0 < i {
			id := clients[i-1].ClientId()
			sourceId = &id
		}
		if i+1 < len(clients) {
			id := clients[i+1].ClientId()
			destinationId = &id
		}
		passiveReady := make(chan struct{})
		if sourceId != nil {
			var registered sync.Once
			want := NewTransferPath(clients[i].ClientId(), *sourceId, self.streamId)
			webRtcManagers[i].testingAfterPeerConnRegistered = func(path TransferPath, active bool) {
				if !active && path == want {
					registered.Do(func() {
						close(passiveReady)
					})
				}
			}
		} else {
			close(passiveReady)
		}
		sequence := NewStreamSequence(
			ctx,
			streamManagers[i],
			sourceId,
			destinationId,
			self.streamId,
			streamManagers[i].streamManagerSettings.StreamBufferSettings,
		)
		go sequence.Run()
		t.Cleanup(sequence.CloseAndWait)
		waitForStreamLifecycleSignal(t, passiveReady, "the passive side did not register")
	}
	return self
}

// Writes one transfer frame into the client's real send route for the stream.
func p2pSourceRawWrite(t *testing.T, ctx context.Context, client *Client, streamId Id, wire []byte) {
	t.Helper()
	writer := client.RouteManager().OpenMultiRouteWriter(StreamId(streamId))
	defer client.RouteManager().CloseMultiRouteWriter(writer)
	success, err := writer.WriteDetailed(ctx, wire, 30*time.Second)
	if err != nil || !success {
		MessagePoolReturn(wire)
		t.Fatalf("write = (%t, %v)", success, err)
	}
}

// Sends one honest message and waits for its transfer ack, which comes back
// over the same association.
func p2pSourceSend(t *testing.T, client *Client, destinationId Id, content string) {
	t.Helper()
	acked := make(chan error, 1)
	frame := sourceRuleSimpleFrame(content)
	if !client.SendWithTimeout(frame, destinationId, func(err error) {
		acked <- err
	}, 30*time.Second) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("send was not admitted")
	}
	select {
	case err := <-acked:
		if err != nil {
			t.Fatalf("ack = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no ack")
	}
}

// The source-rule fixture's delivery record, for a client that is reached
// over a real association instead of an injected route.
type p2pSourceRecorder struct {
	*sourceRuleFixture
}

// The real client under test, with no route but the association.
func newP2pSourceRecorder(t *testing.T, client *Client) *p2pSourceRecorder {
	self := &p2pSourceRecorder{
		sourceRuleFixture: &sourceRuleFixture{
			t:          t,
			client:     client,
			deliveries: make(chan sourceRuleDelivery, 64),
		},
	}
	client.AddReceiveCallback(self.receive)
	return self
}

// A settings change that signals `drops` each time `P2pPeerSourceOnly`
// refuses a frame. The signal blocks the receive loop until `drops` has room,
// so give it room for every refusal the test expects.
func p2pSourceDropSignal(drops chan struct{}) func(*ClientSettings) {
	return func(settings *ClientSettings) {
		settings.afterP2pSourceDropForTest = func() {
			drops <- struct{}{}
		}
	}
}

// A hostile peer on a real association: it holds a route the client trusts for
// its own traffic and writes whatever it likes down it. An opted-in client takes
// the peer's frames, both ways, and refuses the forged control id and the
// forged third client, with their subprotocol frames.
func TestP2pPeerSourceOnlyOverARealAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	drops := make(chan struct{}, 8)
	peerId := NewId()
	client := p2pSourceStreamClient(ctx, NewId(), requireP2pPeerSourceOnly, p2pSourceDropSignal(drops))
	defer client.Close()
	peer := p2pSourceStreamClient(ctx, peerId)
	defer peer.Close()
	client.ContractManager().AddNoContractPeer(peerId)
	peer.ContractManager().AddNoContractPeer(client.ClientId())
	recorder := newP2pSourceRecorder(t, client)
	receipts := newSubprotocolSourceReceipts()
	receipts.register(t, client, subprotocolSourceTestId, WithSubprotocolControlSource())

	stream := newP2pSourceStream(t, ctx, peer, client)
	stream.requireReady()

	p2pSourceSend(t, peer, client.ClientId(), "honest before")
	if delivery := recorder.requireDelivery("honest before"); delivery.source.SourceId != peerId ||
		delivery.peer.TransportType != TransportTypeP2p {
		t.Fatalf("delivery %+v", delivery)
	}

	for _, sourceId := range []Id{ControlId, NewId(), client.ClientId()} {
		p2pSourceRawWrite(t, ctx, peer, stream.streamId, sourceRulePackBytes(
			t,
			client.ClientId(),
			newSourceRuleSequence(sourceId),
			sourceRuleSimpleFrame("forged"),
			subprotocolTestFrame(subprotocolSourceTestId, []byte("forged")),
		))
		select {
		case <-drops:
		case <-ctx.Done():
			t.Fatal("the forged frame was not refused")
		}
	}

	p2pSourceSend(t, peer, client.ClientId(), "honest after")
	recorder.requireDelivery("honest after")
	if len(recorder.deliveries) != 0 {
		t.Fatalf("a forged frame was delivered: %+v", <-recorder.deliveries)
	}
	if raw, typed := receipts.get("raw"), receipts.get("typed"); len(raw) != 0 || len(typed) != 0 {
		t.Fatalf("a forged frame reached raw=%v typed=%v", raw, typed)
	}
	if stats := client.ReceiveStats(); stats.P2pSourceDropCount != 3 {
		t.Fatalf("stats %+v", stats)
	}
}

// A client that did not opt in: a P2P carrier delivers whatever its peer writes,
// exactly as before, and the client's stats never move. §22.5's "ControlId push
// dropped over p2p" still holds for a listener that opted in to control frames,
// by the dispatch's own carrier check.
func TestDefaultClientP2pCarrierOverARealAssociationIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	peerId := NewId()
	thirdId := NewId()
	client := p2pSourceStreamClient(ctx, NewId())
	defer client.Close()
	peer := p2pSourceStreamClient(ctx, peerId)
	defer peer.Close()
	client.ContractManager().AddNoContractPeer(peerId)
	client.ContractManager().AddNoContractPeer(thirdId)
	peer.ContractManager().AddNoContractPeer(client.ClientId())
	recorder := newP2pSourceRecorder(t, client)
	receipts := newSubprotocolSourceReceipts()
	receipts.register(t, client, subprotocolSourceTestId, WithSubprotocolControlSource())

	stream := newP2pSourceStream(t, ctx, peer, client)
	stream.requireReady()

	p2pSourceSend(t, peer, client.ClientId(), "honest")
	recorder.requireDelivery("honest")

	p2pSourceRawWrite(t, ctx, peer, stream.streamId, sourceRulePackBytes(
		t,
		client.ClientId(),
		newSourceRuleSequence(thirdId),
		sourceRuleSimpleFrame("third"),
	))
	if delivery := recorder.requireDelivery("third"); delivery.source.SourceId != thirdId ||
		delivery.peer.TransportType != TransportTypeP2p {
		t.Fatalf("delivery %+v", delivery)
	}

	control := newSourceRuleSequence(ControlId)
	p2pSourceRawWrite(t, ctx, peer, stream.streamId, sourceRulePackBytes(
		t,
		client.ClientId(),
		control,
		subprotocolTestFrame(subprotocolSourceTestId, []byte("forged push")),
		sourceRuleSimpleFrame("control"),
	))
	if delivery := recorder.requireDelivery("control"); !delivery.source.IsControlSource() ||
		delivery.peer.TransportType != TransportTypeP2p {
		t.Fatalf("delivery %+v", delivery)
	}
	if raw, typed := receipts.get("raw"), receipts.get("typed"); len(raw) != 0 || len(typed) != 0 {
		t.Fatalf("a push forged over p2p reached raw=%v typed=%v", raw, typed)
	}
	if stats := client.SubprotocolStats(); stats.DroppedControlSource != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if stats := client.ReceiveStats(); stats.P2pSourceDropCount != 0 || stats.P2pSourceDropByteCount != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// A multihop stream: the far end's frames reach an endpoint over an association
// with the adjacent hop. A client that did not opt in keeps taking them. An
// opted-in client refuses them, since its carrier's peer is the hop, which is the
// stated cost of the setting (SUBPROTOCOL.md §11).
func TestMultihopFarEndOverARealStream(t *testing.T) {
	for _, c := range []struct {
		name      string
		configure []func(*ClientSettings)
		refused   bool
	}{
		{name: "default"},
		{name: "opted in", configure: []func(*ClientSettings){requireP2pPeerSourceOnly}, refused: true},
	} {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			drops := make(chan struct{}, 8)
			farId := NewId()
			hopId := NewId()
			far := p2pSourceStreamClient(ctx, farId)
			defer far.Close()
			hop := p2pSourceStreamClient(ctx, hopId)
			defer hop.Close()
			endpoint := p2pSourceStreamClient(ctx, NewId(), append(c.configure, p2pSourceDropSignal(drops))...)
			defer endpoint.Close()
			endpoint.ContractManager().AddNoContractPeer(farId)
			recorder := newP2pSourceRecorder(t, endpoint)

			stream := newP2pSourceStream(t, ctx, far, hop, endpoint)
			stream.requireReady()

			p2pSourceRawWrite(t, ctx, far, stream.streamId, sourceRulePackBytes(
				t,
				endpoint.ClientId(),
				newSourceRuleSequence(farId),
				sourceRuleSimpleFrame("far"),
			))
			if c.refused {
				select {
				case <-drops:
				case <-ctx.Done():
					t.Fatalf("%s: the far end's frame was not refused", c.name)
				}
				if len(recorder.deliveries) != 0 {
					t.Fatalf("%s: delivered %+v", c.name, <-recorder.deliveries)
				}
			} else {
				delivery := recorder.requireDelivery("far")
				if delivery.source.SourceId != farId || delivery.peer.TransportType != TransportTypeP2p {
					t.Fatalf("%s: delivery %+v", c.name, delivery)
				}
				if stats := endpoint.ReceiveStats(); stats.P2pSourceDropCount != 0 {
					t.Fatalf("%s: stats %+v", c.name, stats)
				}
			}
		}()
	}
}

// The relay in the middle of a stream forwards on private routes and never
// reads a frame's source, so the setting on an intermediary changes nothing: it
// is an endpoint rule. The far end's frame crosses an opted-in hop.
func TestP2pPeerSourceOnlyOnAnIntermediaryChangesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	farId := NewId()
	far := p2pSourceStreamClient(ctx, farId)
	defer far.Close()
	hop := p2pSourceStreamClient(ctx, NewId(), requireP2pPeerSourceOnly)
	defer hop.Close()
	endpoint := p2pSourceStreamClient(ctx, NewId())
	defer endpoint.Close()
	endpoint.ContractManager().AddNoContractPeer(farId)
	recorder := newP2pSourceRecorder(t, endpoint)

	stream := newP2pSourceStream(t, ctx, far, hop, endpoint)
	stream.requireReady()

	p2pSourceRawWrite(t, ctx, far, stream.streamId, sourceRulePackBytes(
		t,
		endpoint.ClientId(),
		newSourceRuleSequence(farId),
		sourceRuleSimpleFrame("far"),
	))
	recorder.requireDelivery("far")
	if stats := hop.ReceiveStats(); stats.P2pSourceDropCount != 0 {
		t.Fatalf("stats %+v", stats)
	}
}
