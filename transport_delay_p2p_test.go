//go:build !js

package connect

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// transportDelayP2pSignalReceiver delivers signal batches to a test manager. It
// records the delay header version on the batches that carry an SDP offer or
// answer, and can strip the field to model a peer build that predates it: an
// old peer neither sends the field nor reads it.
type transportDelayP2pSignalReceiver struct {
	manager *WebRtcManager
	strip   bool

	stateLock          sync.Mutex
	signalTypeVersions map[protocol.SignalType][]uint32
}

// testingSignalReceiver exposes the manager to the signal pipe's source lookup.
func (self *transportDelayP2pSignalReceiver) testingSignalReceiver() SignalReceiver {
	return self.manager
}

func (self *transportDelayP2pSignalReceiver) ReceiveSignal(
	source TransferPath,
	transferKey TransferKey,
	frame *protocol.Frame,
) error {
	signals := &protocol.ExchangeSignals{}
	if err := ProtoUnmarshal(frame.MessageBytes, signals); err != nil {
		return err
	}
	self.stateLock.Lock()
	if self.signalTypeVersions == nil {
		self.signalTypeVersions = map[protocol.SignalType][]uint32{}
	}
	for _, signal := range signals.Signals {
		switch signal.GetSignalType() {
		case protocol.SignalType_SdpOffer, protocol.SignalType_SdpAnswer:
			self.signalTypeVersions[signal.SignalType] = append(
				self.signalTypeVersions[signal.SignalType],
				signals.TransportDelayVersion,
			)
		}
	}
	self.stateLock.Unlock()
	if self.strip {
		signals.TransportDelayVersion = 0
	}
	return self.manager.ReceiveExchangeSignals(source, transferKey, signals)
}

// requireVersions checks that every batch of signalType carried version.
func (self *transportDelayP2pSignalReceiver) requireVersions(
	t *testing.T,
	signalType protocol.SignalType,
	version uint32,
) {
	t.Helper()
	self.stateLock.Lock()
	versions := slices.Clone(self.signalTypeVersions[signalType])
	self.stateLock.Unlock()
	if len(versions) == 0 {
		t.Fatalf("no %s batch was delivered", signalType)
	}
	for _, batchVersion := range versions {
		if batchVersion != version {
			t.Fatalf("%s batches carried versions %v, expected %d", signalType, versions, version)
		}
	}
}

// transportDelayP2pStatsRecorder collects published delay stats.
type transportDelayP2pStatsRecorder struct {
	stateLock sync.Mutex
	statsList []*TransportDelayStats
}

func (self *transportDelayP2pStatsRecorder) publish(stats *TransportDelayStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statsList = append(self.statsList, stats)
}

func (self *transportDelayP2pStatsRecorder) snapshot() []*TransportDelayStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.statsList)
}

// testTransportDelayP2pSettings measures on short intervals so a test sees
// several of them.
func testTransportDelayP2pSettings() *TransportDelaySettings {
	settings := DefaultTransportDelaySettings()
	settings.Interval = 50 * time.Millisecond
	settings.Warmup = 0
	settings.MinDelaySampleCount = 1
	return settings
}

// newTestWebRtcSettingsForTransportDelay is the hermetic same-host mode every
// establishing WebRTC test uses.
func newTestWebRtcSettingsForTransportDelay() *WebRtcSettings {
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.IceServerUrls = nil
	settings.UseLoopbackOnlyIceInterfaces = true
	return settings
}

// transportDelayP2pTestPair is one hermetic native association whose active
// side offers the delay header as its client settings say.
type transportDelayP2pTestPair struct {
	ctx       context.Context
	active    *peerConn
	passive   *peerConn
	activeId  Id
	passiveId Id
	streamId  Id
	toPassive *transportDelayP2pSignalReceiver
	toActive  *transportDelayP2pSignalReceiver
}

// newTransportDelayP2pTestPair establishes the association and runs the
// production "rdy" exchange, after which both descriptions have applied on
// both ends. legacyPeer strips the version from every batch both ways.
func newTransportDelayP2pTestPair(
	t *testing.T,
	offer *TransportDelaySettings,
	legacyPeer bool,
) *transportDelayP2pTestPair {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	activeSignals := newSignalPipe(nil)
	passiveSignals := newSignalPipe(nil)
	activeManager := newTestWebRtcManager(t, ctx, activeSignals, newTestWebRtcSettingsForTransportDelay())
	passiveManager := newTestWebRtcManager(t, ctx, passiveSignals, newTestWebRtcSettingsForTransportDelay())
	pair := &transportDelayP2pTestPair{
		ctx:       ctx,
		activeId:  NewId(),
		passiveId: NewId(),
		streamId:  NewId(),
		toPassive: &transportDelayP2pSignalReceiver{manager: passiveManager, strip: legacyPeer},
		toActive:  &transportDelayP2pSignalReceiver{manager: activeManager, strip: legacyPeer},
	}
	activeSignals.SetSignalReceiver(pair.toPassive)
	passiveSignals.SetSignalReceiver(pair.toActive)

	// the in-memory signal pipe drops signals for an unregistered stream, so
	// the passive end registers first
	passive, err := passiveManager.NewP2pConnPassive(
		ctx,
		NewTransferPath(pair.passiveId, pair.activeId, pair.streamId),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { passive.Close() })
	active, err := activeManager.newP2pConnActiveWithTransportDelay(
		ctx,
		NewTransferPath(pair.activeId, pair.passiveId, pair.streamId),
		offer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { active.Close() })
	pair.active = active.(*peerConn)
	pair.passive = passive.(*peerConn)

	settings := DefaultP2pTransportSettings()
	results := make(chan error, 4)
	for _, conn := range []net.Conn{pair.active, pair.passive} {
		go func() {
			conn.SetWriteDeadline(time.Now().Add(settings.ConnectTimeout))
			_, err := conn.Write([]byte(ReadyHeader))
			results <- err
		}()
		go func() {
			conn.SetReadDeadline(time.Now().Add(settings.ConnectTimeout))
			prefetched, err := readP2pReadyHeader(conn, settings)
			for _, message := range prefetched {
				MessagePoolReturn(message)
			}
			if err == nil && len(prefetched) != 0 {
				err = fmt.Errorf("%d frames preceded the ready header", len(prefetched))
			}
			results <- err
		}()
	}
	for range 4 {
		select {
		case <-ctx.Done():
			t.Fatalf("ready header exchange stalled: %v", ctx.Err())
		case err := <-results:
			if err != nil {
				t.Fatalf("ready header exchange: %v", err)
			}
		}
	}
	pair.active.SetDeadline(time.Time{})
	pair.passive.SetDeadline(time.Time{})
	return pair
}

// waitFastPath waits until both native fast-path carriers are ready.
func (self *transportDelayP2pTestPair) waitFastPath(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !self.active.FastPathReady() || !self.passive.FastPathReady() {
		if time.Now().After(deadline) {
			t.Fatalf(
				"fast carrier did not become ready: active=%t passive=%t",
				self.active.FastPathReady(),
				self.passive.FastPathReady(),
			)
		}
		time.Sleep(time.Millisecond)
	}
}

// transportDelayP2pTestRoutes are both directions of one association.
type transportDelayP2pTestRoutes struct {
	activeSend          Route
	passiveSend         Route
	activeReceiveLanes  []p2pReceiveRouteLane
	passiveReceiveLanes []p2pReceiveRouteLane
	transports          []Transport
}

// newTransportDelayP2pTestRoutes builds the production send and receive
// transports on both ends, each with the delay state of its end.
func newTransportDelayP2pTestRoutes(
	t *testing.T,
	ctx context.Context,
	pair *transportDelayP2pTestPair,
	settings *P2pTransportSettings,
	activeDelay *p2pTransportDelay,
	passiveDelay *p2pTransportDelay,
) *transportDelayP2pTestRoutes {
	t.Helper()
	routes := &transportDelayP2pTestRoutes{}
	transportCtx, transportCancel := context.WithCancel(ctx)
	t.Cleanup(transportCancel)
	activeSendTransport, activeSend := newP2pSendTransportForPeerWithDelay(
		transportCtx, transportCancel, pair.active, pair.passiveId, pair.streamId, settings, false, nil, activeDelay,
	)
	passiveSendTransport, passiveSend := newP2pSendTransportForPeerWithDelay(
		transportCtx, transportCancel, pair.passive, pair.activeId, pair.streamId, settings, false, nil, passiveDelay,
	)
	activeReceiveTransport, activeReceiveLanes := newP2pReceiveTransportWithLanes(
		transportCtx, transportCancel, pair.active, pair.streamId, settings, nil, nil, activeDelay,
	)
	passiveReceiveTransport, passiveReceiveLanes := newP2pReceiveTransportWithLanes(
		transportCtx, transportCancel, pair.passive, pair.streamId, settings, nil, nil, passiveDelay,
	)
	routes.activeSend = activeSend
	routes.passiveSend = passiveSend
	routes.activeReceiveLanes = activeReceiveLanes
	routes.passiveReceiveLanes = passiveReceiveLanes
	routes.transports = []Transport{
		activeSendTransport,
		passiveSendTransport,
		activeReceiveTransport,
		passiveReceiveTransport,
	}
	return routes
}

// sendAndReceive moves one message over send and requires it intact on
// either physical lane of the receiver.
func sendAndReceiveTransportDelayP2p(
	t *testing.T,
	ctx context.Context,
	send Route,
	receiveLanes []p2pReceiveRouteLane,
	message []byte,
) {
	t.Helper()
	pooled := MessagePoolCopy(message)
	select {
	case <-ctx.Done():
		MessagePoolReturn(pooled)
		t.Fatal("route stopped before send")
	case send <- pooled:
	}
	var received []byte
	select {
	case <-ctx.Done():
		t.Fatalf("%d-byte message was not delivered", len(message))
	case received = <-receiveLanes[0].route:
	case received = <-receiveLanes[1].route:
	}
	defer MessagePoolReturn(received)
	if !bytes.Equal(received, message) {
		t.Fatalf("delivered %d bytes, expected the %d-byte message unchanged", len(received), len(message))
	}
}

func transportDelayP2pTestMessage(byteCount int, seed int) []byte {
	message := make([]byte, byteCount)
	for i := range message {
		message[i] = byte((seed*31 + i) % 251)
	}
	return message
}

func TestTransportDelayP2pAcceptedVersion(t *testing.T) {
	AssertEqual(t, transportDelayP2pAcceptedVersion(0), uint32(0))
	AssertEqual(t, transportDelayP2pAcceptedVersion(1), uint32(1))
	// a newer offer is answered with the version this build speaks
	AssertEqual(t, transportDelayP2pAcceptedVersion(2), uint32(1))
}

// The negotiation matrix over real offer/answer exchanges. Both ends agree in
// every case, and a connection that did not negotiate keeps the legacy wire
// byte for byte.
func TestTransportDelayP2pNegotiationMatrix(t *testing.T) {
	cases := []struct {
		name          string
		offer         *TransportDelaySettings
		legacyPeer    bool
		offerVersion  uint32
		answerVersion uint32
		negotiated    bool
	}{
		{name: "both offer and accept", offer: testTransportDelayP2pSettings(), offerVersion: 1, answerVersion: 1, negotiated: true},
		{name: "legacy peer", offer: testTransportDelayP2pSettings(), legacyPeer: true, offerVersion: 1, answerVersion: 0},
		{name: "active without settings", offer: nil, offerVersion: 0, answerVersion: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pair := newTransportDelayP2pTestPair(t, testCase.offer, testCase.legacyPeer)
			pair.toPassive.requireVersions(t, protocol.SignalType_SdpOffer, testCase.offerVersion)
			pair.toActive.requireVersions(t, protocol.SignalType_SdpAnswer, testCase.answerVersion)
			AssertEqual(t, pair.active.transportDelayNegotiated(), testCase.negotiated)
			AssertEqual(t, pair.passive.transportDelayNegotiated(), testCase.negotiated)

			recorder := &transportDelayP2pStatsRecorder{}
			activeDelay := newP2pTransportDelayForConn(pair.active, true, testCase.offer, pair.activeId, recorder.publish)
			passiveDelay := newP2pTransportDelayForConn(pair.passive, false, nil, pair.passiveId, nil)
			AssertEqual(t, activeDelay != nil, testCase.negotiated)
			AssertEqual(t, passiveDelay != nil, testCase.negotiated)
			if passiveDelay != nil && passiveDelay.delay.measuring() {
				t.Fatal("the passive end measures; it must only stamp and echo")
			}

			// the wire of one legacy write, read raw on the other end
			wireSettings := DefaultP2pTransportSettings()
			wireSettings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
			wireCtx, wireCancel := context.WithCancel(pair.ctx)
			defer wireCancel()
			wireTransport, wireRoute := newP2pSendTransportForPeerWithDelay(
				wireCtx, wireCancel, pair.active, pair.passiveId, pair.streamId, wireSettings, false, nil, activeDelay,
			)
			message := transportDelayP2pTestMessage(700, 1)
			wireRoute <- MessagePoolCopy(message)
			pair.passive.SetReadDeadline(time.Now().Add(10 * time.Second))
			wireMessage, err := readP2pMessage(pair.passive, 4096, 4096, wireSettings.MaxMessageByteCount)
			if err != nil {
				t.Fatalf("raw read: %v", err)
			}
			pair.passive.SetReadDeadline(time.Time{})
			if testCase.negotiated {
				if len(wireMessage) != transportDelayHeaderByteCount+len(message) ||
					!bytes.Equal(wireMessage[transportDelayHeaderByteCount:], message) {
					t.Fatalf("negotiated wire message = %d bytes, expected header + %d", len(wireMessage), len(message))
				}
				// the generation's first write on the lane has no previous write
				if _, previousWrite, _ := parseTransportDelayHeader(wireMessage); previousWrite != 0 {
					t.Fatalf("first write reported a previous write of %d us", previousWrite)
				}
			} else if !bytes.Equal(wireMessage, message) {
				t.Fatalf("legacy wire message = %d bytes, expected the %d-byte message unchanged", len(wireMessage), len(message))
			}
			MessagePoolReturn(wireMessage)
			if err := wireTransport.(P2pRouteLifecycle).CloseAndWait(pair.ctx); err != nil {
				t.Fatal(err)
			}

			// production Auto lanes in both directions
			settings := DefaultP2pTransportSettings()
			routes := newTransportDelayP2pTestRoutes(t, pair.ctx, pair, settings, activeDelay, passiveDelay)
			for messageIndex, byteCount := range []int{1, 100, 300, 1188, 2000} {
				sendAndReceiveTransportDelayP2p(t, pair.ctx, routes.activeSend, routes.passiveReceiveLanes, transportDelayP2pTestMessage(byteCount, messageIndex))
				sendAndReceiveTransportDelayP2p(t, pair.ctx, routes.passiveSend, routes.activeReceiveLanes, transportDelayP2pTestMessage(byteCount, messageIndex+100))
			}
			snapshotP2pFastPathTestRoutes(t, settings.DataPlaneStats, routes.transports...)
			activeDelay.flush()
			passiveDelay.flush()
			if !testCase.negotiated && 0 < len(recorder.snapshot()) {
				t.Fatal("a connection without the header published delay stats")
			}
		})
	}
}

// newTestTransportDelayP2pGateClient holds only what the P2P offer gate reads:
// the client's settings and the route manager its platform connections record
// edge answers on.
func newTestTransportDelayP2pGateClient(t *testing.T, settings *TransportDelaySettings) *Client {
	clientSettings := closeWaitClientSettings()
	clientSettings.TransportDelay = settings
	return &Client{
		routeManager: NewRouteManager(t.Context(), "transport-delay-p2p-gate"),
		settings:     clientSettings,
	}
}

// A new active generation offers the header only while the latest edge answer
// on the client's platform connections accepted it (DELAY.md §6.1): not before
// any edge answers, not after a decline, and again after the next acceptance.
// A client whose own settings do not offer never does.
func TestTransportDelayP2pOfferFollowsTheEdges(t *testing.T) {
	requireOffer := func(state string, client *Client, expected *TransportDelaySettings) {
		t.Helper()
		if offer := client.transportDelayP2pOffer(); offer != expected {
			t.Fatalf("%s: offer = %+v, expected %+v", state, offer, expected)
		}
	}
	settings := testTransportDelayP2pSettings()
	client := newTestTransportDelayP2pGateClient(t, settings)
	requireOffer("before any edge answers", client, nil)
	client.routeManager.noteTransportDelayEdgeAnswer(true)
	requireOffer("after an edge accepted", client, settings)
	client.routeManager.noteTransportDelayEdgeAnswer(false)
	requireOffer("after an edge declined", client, nil)
	client.routeManager.noteTransportDelayEdgeAnswer(true)
	requireOffer("after the next acceptance", client, settings)

	withoutSettings := newTestTransportDelayP2pGateClient(t, nil)
	withoutSettings.routeManager.noteTransportDelayEdgeAnswer(true)
	requireOffer("without settings", withoutSettings, nil)
}

// A generation reads the gate once, when it starts. One that negotiated while
// the edges accepted keeps the header after they decline, and the next one
// offers nothing and keeps the legacy wire (DELAY.md §6.1, §11).
func TestTransportDelayP2pEdgeDeclineStopsOnlyNewGenerations(t *testing.T) {
	settings := testTransportDelayP2pSettings()
	client := newTestTransportDelayP2pGateClient(t, settings)
	client.routeManager.noteTransportDelayEdgeAnswer(true)
	accepted := newTransportDelayP2pTestPair(t, client.transportDelayP2pOffer(), false)
	client.routeManager.noteTransportDelayEdgeAnswer(false)
	declined := newTransportDelayP2pTestPair(t, client.transportDelayP2pOffer(), false)

	accepted.toPassive.requireVersions(t, protocol.SignalType_SdpOffer, TransportDelayVersion)
	accepted.toActive.requireVersions(t, protocol.SignalType_SdpAnswer, TransportDelayVersion)
	declined.toPassive.requireVersions(t, protocol.SignalType_SdpOffer, 0)
	declined.toActive.requireVersions(t, protocol.SignalType_SdpAnswer, 0)
	for _, generation := range []struct {
		name       string
		pair       *transportDelayP2pTestPair
		negotiated bool
	}{
		{name: "the generation the edges accepted", pair: accepted, negotiated: true},
		{name: "the generation after the decline", pair: declined},
	} {
		// the delay state each end starts after "rdy", which decides its wire
		activeDelay := newP2pTransportDelayForConn(generation.pair.active, true, settings, generation.pair.activeId, nil)
		passiveDelay := newP2pTransportDelayForConn(generation.pair.passive, false, nil, generation.pair.passiveId, nil)
		if (activeDelay != nil) != generation.negotiated || (passiveDelay != nil) != generation.negotiated {
			t.Fatalf(
				"%s: header active=%t passive=%t, expected %t",
				generation.name,
				activeDelay != nil,
				passiveDelay != nil,
				generation.negotiated,
			)
		}
	}
}

// testTransportDelayP2pLaneRoundTrip drives one forced lane both ways with the
// header and checks the payloads, the lane counters, and the active end's
// published measurement.
func testTransportDelayP2pLaneRoundTrip(
	t *testing.T,
	mode P2pDataPlaneMode,
	checkWire func(t *testing.T, pair *transportDelayP2pTestPair, send Route),
) {
	offer := testTransportDelayP2pSettings()
	pair := newTransportDelayP2pTestPair(t, offer, false)
	if mode == P2pDataPlaneModeFastOnly {
		pair.waitFastPath(t)
	}
	recorder := &transportDelayP2pStatsRecorder{}
	activeDelay := newP2pTransportDelayForConn(pair.active, true, offer, pair.activeId, recorder.publish)
	passiveDelay := newP2pTransportDelayForConn(pair.passive, false, nil, pair.passiveId, nil)
	if activeDelay == nil || passiveDelay == nil {
		t.Fatal("the association did not negotiate the delay header")
	}

	settings := DefaultP2pTransportSettings()
	settings.DataPlaneMode = mode
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	// the wire check owns the passive end's reader until it returns
	wireCtx, wireCancel := context.WithCancel(pair.ctx)
	wireTransport, wireRoute := newP2pSendTransportForPeerWithDelay(
		wireCtx, wireCancel, pair.active, pair.passiveId, pair.streamId, settings, false, nil, activeDelay,
	)
	checkWire(t, pair, wireRoute)
	if err := wireTransport.(P2pRouteLifecycle).CloseAndWait(pair.ctx); err != nil {
		t.Fatal(err)
	}
	wireCancel()

	routes := newTransportDelayP2pTestRoutes(t, pair.ctx, pair, settings, activeDelay, passiveDelay)
	// The sizes cover the synchronous legacy write, the compact legacy queue,
	// a fast-path fragment boundary, and a message just past the queue's slab
	// budget once the header is counted.
	byteCounts := []int{1, 100, 300, 1176, 1188, 2000, 4090, 8175}
	exchangeCount := 0
	for round := range 4 {
		for messageIndex, byteCount := range byteCounts {
			sendAndReceiveTransportDelayP2p(t, pair.ctx, routes.activeSend, routes.passiveReceiveLanes, transportDelayP2pTestMessage(byteCount, round*100+messageIndex))
			sendAndReceiveTransportDelayP2p(t, pair.ctx, routes.passiveSend, routes.activeReceiveLanes, transportDelayP2pTestMessage(byteCount, round*100+messageIndex+50))
			exchangeCount += 1
		}
		// span several measurement intervals
		time.Sleep(offer.Interval)
	}
	stats := snapshotP2pFastPathTestRoutes(t, settings.DataPlaneStats, routes.transports...)
	activeDelay.flush()
	passiveDelay.flush()

	// the wire check's message counts as sent but was read raw
	sentCount := uint64(2*exchangeCount + 1)
	receivedCount := uint64(2 * exchangeCount)
	if mode == P2pDataPlaneModeFastOnly {
		if stats.FastSendMessageCount != sentCount ||
			stats.FastReceiveMessageCount != receivedCount ||
			stats.LegacySendMessageCount != 0 ||
			stats.LegacyReceiveMessageCount != 0 {
			t.Fatalf("fast lane stats = %+v", stats)
		}
	} else if stats.LegacySendMessageCount != sentCount ||
		stats.LegacyReceiveMessageCount != receivedCount ||
		stats.FastSendMessageCount != 0 ||
		stats.FastReceiveMessageCount != 0 {
		t.Fatalf("legacy lane stats = %+v", stats)
	}

	published := recorder.snapshot()
	if len(published) < 2 {
		t.Fatalf("published %d intervals over several, expected at least 2", len(published))
	}
	var sendByteCount, receiveByteCount ByteCount
	sendQueueValid := false
	receiveQueueValid := false
	for _, interval := range published {
		if interval.TransportType != TransportTypeP2p || interval.ClientId != pair.activeId {
			t.Fatalf("interval attributed to %s/%s, expected p2p/%s", interval.TransportType, interval.ClientId, pair.activeId)
		}
		if interval.ConnectionId != published[0].ConnectionId {
			t.Fatal("one generation published under two connection ids")
		}
		sendByteCount += interval.SendByteCount
		receiveByteCount += interval.ReceiveByteCount
		sendQueueValid = sendQueueValid || interval.SendQueueDelayValid
		receiveQueueValid = receiveQueueValid || interval.ReceiveQueueDelayValid
	}
	if sendByteCount == 0 || receiveByteCount == 0 {
		t.Fatalf("measured send=%d receive=%d bytes, expected both", sendByteCount, receiveByteCount)
	}
	// down samples come from the peer's stamps, up samples from its echoes
	if !receiveQueueValid || !sendQueueValid {
		t.Fatalf("queue delay samples: send=%t receive=%t, expected both", sendQueueValid, receiveQueueValid)
	}
}

// The legacy SCTP lane carries the header in front of each SCTP message, for
// the send worker's own writes and the compact queue worker's alike.
func TestTransportDelayP2pLegacyLaneRoundTrip(t *testing.T) {
	testTransportDelayP2pLaneRoundTrip(t, P2pDataPlaneModeLegacyOnly, func(t *testing.T, pair *transportDelayP2pTestPair, send Route) {
		message := transportDelayP2pTestMessage(2000, 7)
		send <- MessagePoolCopy(message)
		pair.passive.SetReadDeadline(time.Now().Add(10 * time.Second))
		wireMessage, err := readP2pMessage(pair.passive, 4096, 4096, DefaultP2pTransportSettings().MaxMessageByteCount)
		pair.passive.SetReadDeadline(time.Time{})
		if err != nil {
			t.Fatalf("raw legacy read: %v", err)
		}
		defer MessagePoolReturn(wireMessage)
		if len(wireMessage) != transportDelayHeaderByteCount+len(message) ||
			!bytes.Equal(wireMessage[transportDelayHeaderByteCount:], message) {
			t.Fatalf("legacy wire message = %d bytes, expected header + %d", len(wireMessage), len(message))
		}
	})
}

// The fast lane carries the header as the first bytes of the message inside
// the unchanged 'URD' v2 envelope: the receiver's reassembler accepts it, and
// a message of exactly one fragment payload takes a second fragment.
func TestTransportDelayP2pFastLaneRoundTrip(t *testing.T) {
	testTransportDelayP2pLaneRoundTrip(t, P2pDataPlaneModeFastOnly, func(t *testing.T, pair *transportDelayP2pTestPair, send Route) {
		message := transportDelayP2pTestMessage(p2pFastPathFragmentPayloadByteCount, 9)
		send <- MessagePoolCopy(message)
		select {
		case <-pair.ctx.Done():
			t.Fatal("fast wire message was not delivered")
		case received := <-pair.passive.FastPathMessages():
			defer MessagePoolReturn(received.message)
			if len(received.message) != transportDelayHeaderByteCount+len(message) ||
				!bytes.Equal(received.message[transportDelayHeaderByteCount:], message) {
				t.Fatalf("fast wire message = %d bytes, expected header + %d", len(received.message), len(message))
			}
			if received.fragmentCount != 2 {
				t.Fatalf("fast wire message took %d fragments, expected 2", received.fragmentCount)
			}
		}
	})
}

// The fragmenter reads each fragment from the header and the message without
// joining them; the fragments concatenate to header + message.
func TestCopyP2pFastPathFragmentSpansHeaderAndMessage(t *testing.T) {
	header := newTransportDelayHeader(0x01020304, 0x05060708, 0x090a0b0c)
	for _, messageByteCount := range []int{0, 1, 1175, 1176, 1177, 1188, 2364, 2365, 5000} {
		message := transportDelayP2pTestMessage(messageByteCount, messageByteCount)
		expected := append(slices.Clone(header[:]), message...)
		totalByteCount := len(expected)
		fragmentCount := p2pFastPathFragmentCount(totalByteCount)
		joined := []byte{}
		for fragmentIndex := range fragmentCount {
			offset := fragmentIndex * p2pFastPathFragmentPayloadByteCount
			fragment := make([]byte, min(p2pFastPathFragmentPayloadByteCount, totalByteCount-offset))
			copyP2pFastPathFragment(fragment, header[:], message, offset)
			joined = append(joined, fragment...)
		}
		if !bytes.Equal(joined, expected) {
			t.Fatalf("%d-byte message: fragments do not join to header + message", messageByteCount)
		}
	}
}

// newTestTransportDelayP2p is measuring delay state on controlled clocks.
func newTestTransportDelayP2p(settings *TransportDelaySettings) (
	*p2pTransportDelay,
	*time.Time,
	*int64,
	*transportDelayP2pStatsRecorder,
) {
	recorder := &transportDelayP2pStatsRecorder{}
	delay := newTransportDelay(settings, NewId(), TransportTypeP2p, recorder.publish)
	now := delay.startTime
	arrivalMicros := int64(0)
	delay.nowForTest = func() time.Time { return now }
	delay.clockForTest = func() int64 { return arrivalMicros }
	return newP2pTransportDelay(delay), &now, &arrivalMicros, recorder
}

func newTestTransportDelayP2pMessage(sendStart uint32, previousWrite uint32, payloadByteCount int) []byte {
	message := MessagePoolGet(transportDelayHeaderByteCount + payloadByteCount)
	header := newTransportDelayHeader(sendStart, previousWrite, 0)
	copy(message, header[:])
	for i := transportDelayHeaderByteCount; i < len(message); i++ {
		message[i] = byte(i)
	}
	return message
}

// SCTP delivers the legacy lane unordered, so a remote write pairs with the
// open group only when it starts at least the reported duration later. The
// lossy fast lane never pairs; both lanes give delay samples.
func TestTransportDelayP2pLanePairing(t *testing.T) {
	settings := testTransportDelayP2pSettings()
	settings.Interval = time.Second
	// 8 ms blocked in a 1 s interval
	settings.MinBusyFraction = 0.005

	receive := func(t *testing.T, receive func([]byte) []byte, arrivalMicros *int64, sendStart uint32, previousWrite uint32) {
		t.Helper()
		*arrivalMicros = int64(sendStart) + 20_000
		payload := receive(newTestTransportDelayP2pMessage(sendStart, previousWrite, 4000))
		if len(payload) != 4000 || payload[0] != byte(transportDelayHeaderByteCount) {
			t.Fatalf("stripped payload = %d bytes starting %d", len(payload), payload[0])
		}
		MessagePoolReturn(payload)
	}

	t.Run("legacy", func(t *testing.T) {
		delay, now, arrivalMicros, recorder := newTestTransportDelayP2p(settings)
		legacy := func(message []byte) []byte { return delay.receiveLegacy(message, false) }
		receive(t, legacy, arrivalMicros, 10_000, 0)
		// starts 2 ms later yet claims an 8 ms previous write: reordered
		receive(t, legacy, arrivalMicros, 12_000, 8_000)
		*now = now.Add(settings.Interval)
		receive(t, legacy, arrivalMicros, 40_000, 0)
		// starts 10 ms later and claims 8 ms: the write after the open group
		receive(t, legacy, arrivalMicros, 50_000, 8_000)
		*now = now.Add(settings.Interval)
		receive(t, legacy, arrivalMicros, 90_000, 0)
		published := recorder.snapshot()
		if len(published) != 2 {
			t.Fatalf("published %d intervals, expected 2", len(published))
		}
		if published[0].ReceiveBitsPerSecond != 0 {
			t.Fatalf("a reordered pairing produced %d bit/s", published[0].ReceiveBitsPerSecond)
		}
		if published[1].ReceiveBitsPerSecond == 0 {
			t.Fatal("an ordered pairing produced no rate")
		}
		if !published[1].ReceiveQueueDelayValid {
			t.Fatal("the legacy lane gave no delay sample")
		}
	})

	t.Run("fast", func(t *testing.T) {
		delay, now, arrivalMicros, recorder := newTestTransportDelayP2p(settings)
		fast := func(message []byte) []byte { return delay.receiveFast(message, 4) }
		receive(t, fast, arrivalMicros, 40_000, 0)
		receive(t, fast, arrivalMicros, 50_000, 8_000)
		*now = now.Add(settings.Interval)
		receive(t, fast, arrivalMicros, 90_000, 0)
		published := recorder.snapshot()
		if len(published) != 1 {
			t.Fatalf("published %d intervals, expected 1", len(published))
		}
		if published[0].ReceiveBitsPerSecond != 0 {
			t.Fatalf("the lossy lane paired a write: %d bit/s", published[0].ReceiveBitsPerSecond)
		}
		if !published[0].ReceiveQueueDelayValid {
			t.Fatal("the fast lane gave no delay sample")
		}
	})
}

// A message too short to carry the header is dropped and returned, and a
// header with no payload is accounted but never delivered.
func TestTransportDelayP2pReceiveRejectsMessagesWithoutPayload(t *testing.T) {
	delay, _, _, _ := newTestTransportDelayP2p(testTransportDelayP2pSettings())
	short := MessagePoolGet(transportDelayHeaderByteCount - 1)
	if payload := delay.receiveLegacy(short, false); payload != nil {
		t.Fatalf("a %d-byte message delivered %d bytes", transportDelayHeaderByteCount-1, len(payload))
	}
	headerOnly := newTestTransportDelayP2pMessage(1000, 0, 0)
	if payload := delay.receiveFast(headerOnly, 1); payload != nil {
		t.Fatalf("a header-only message delivered %d bytes", len(payload))
	}
}

// transportDelayP2pWireConn records every message written through it.
type transportDelayP2pWireConn struct {
	net.Conn

	stateLock sync.Mutex
	messages  [][]byte
}

func (self *transportDelayP2pWireConn) Write(message []byte) (int, error) {
	self.stateLock.Lock()
	self.messages = append(self.messages, bytes.Clone(message))
	self.stateLock.Unlock()
	return self.Conn.Write(message)
}

func (self *transportDelayP2pWireConn) written() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.messages)
}

// newStampOnlyTransportDelayP2p is the delay state of a peer that stamps and
// echoes without measuring.
func newStampOnlyTransportDelayP2p() *p2pTransportDelay {
	return newP2pTransportDelay(newTransportDelay(nil, NewId(), TransportTypeP2p, nil))
}

// closeTransportDelayP2pRoutes joins send routes, closes the connections, then
// joins receive routes.
func closeTransportDelayP2pRoutes(t *testing.T, conns []net.Conn, transports ...Transport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, transport := range transports {
		transport.(P2pRouteLifecycle).Close()
	}
	for _, transport := range transports {
		if _, send := transport.(*P2pSendTransport); send {
			if err := transport.(P2pRouteLifecycle).CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, conn := range conns {
		conn.Close()
	}
	for _, transport := range transports {
		if err := transport.(P2pRouteLifecycle).CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// Endpoint probes classify on the unstamped envelope at both ends: the sender
// keeps them out of the data counters, and the receiver's handler sees the
// exact 40-byte envelope after the header is stripped.
func TestTransportDelayP2pProbeSeesUnstampedEnvelope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sendConn, receiveConn := net.Pipe()
	wire := &transportDelayP2pWireConn{Conn: sendConn}
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	streamId := NewId()
	handledProbes := make(chan []byte, 4)
	handler := func(message []byte) bool {
		if !isP2pStreamProbe(message) {
			return false
		}
		handledProbes <- bytes.Clone(message)
		return true
	}
	sendTransport, send := newP2pSendTransportForPeerWithDelay(
		ctx, cancel, wire, NewId(), streamId, settings, false, nil, newStampOnlyTransportDelayP2p(),
	)
	receiveTransport, receiveLanes := newP2pReceiveTransportWithLanes(
		ctx, cancel, receiveConn, streamId, settings, nil, handler, newStampOnlyTransportDelayP2p(),
	)

	probe := encodeP2pStreamProbe(p2pStreamProbeRequestType, streamId, NewId())
	expectedProbe := bytes.Clone(probe)
	send <- probe
	select {
	case <-ctx.Done():
		t.Fatal("probe was not handled")
	case handled := <-handledProbes:
		if !bytes.Equal(handled, expectedProbe) {
			t.Fatalf("handler saw a %d-byte probe, expected the %d-byte envelope", len(handled), len(expectedProbe))
		}
	}
	sendAndReceiveTransportDelayP2p(t, ctx, send, receiveLanes, transportDelayP2pTestMessage(300, 3))
	closeTransportDelayP2pRoutes(t, []net.Conn{sendConn, receiveConn}, sendTransport, receiveTransport)

	messages := wire.written()
	if len(messages) != 2 {
		t.Fatalf("wrote %d messages, expected 2", len(messages))
	}
	if len(messages[0]) != transportDelayHeaderByteCount+p2pStreamProbeByteCount ||
		!bytes.Equal(messages[0][transportDelayHeaderByteCount:], expectedProbe) {
		t.Fatalf("probe wire message = %d bytes, expected header + %d", len(messages[0]), p2pStreamProbeByteCount)
	}
	stats := settings.DataPlaneStats.Snapshot()
	if stats.LegacySendMessageCount != 1 || stats.LegacySendByteCount != 300 ||
		stats.LegacyReceiveMessageCount != 1 || stats.LegacyReceiveByteCount != 300 {
		t.Fatalf("data counters include the probe or the header: %+v", stats)
	}
}

// The header shares the physical message cap. On a negotiated connection the
// largest transfer frame is 12 bytes smaller and its stamped write fills the
// cap exactly; a frame over the reduced cap ends the send generation unsent.
func TestTransportDelayP2pHeaderSharesMessageCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sendConn, drainConn := net.Pipe()
	defer drainConn.Close()
	go func() {
		buffer := make([]byte, 4096)
		for {
			if _, err := drainConn.Read(buffer); err != nil {
				return
			}
		}
	}()
	wire := &transportDelayP2pWireConn{Conn: sendConn}
	settings := DefaultP2pTransportSettings()
	settings.MaxMessageByteCount = 1024
	// write synchronously, so the first frame is on the wire before the
	// oversized one ends the generation
	settings.LegacySendQueueByteCount = 0
	// the generation's end cancels only its own context
	transportCtx, transportCancel := context.WithCancel(ctx)
	defer transportCancel()
	sendTransport, send := newP2pSendTransportForPeerWithDelay(
		transportCtx, transportCancel, wire, NewId(), NewId(), settings, false, nil, newStampOnlyTransportDelayP2p(),
	)
	largest := transportDelayP2pTestMessage(settings.MaxMessageByteCount-transportDelayHeaderByteCount, 1)
	send <- MessagePoolCopy(largest)
	send <- MessagePoolCopy(transportDelayP2pTestMessage(len(largest)+1, 2))
	select {
	case <-ctx.Done():
		t.Fatal("an oversized frame did not end the send generation")
	case <-sendTransport.(P2pRouteLifecycle).Done():
	}
	sendConn.Close()
	messages := wire.written()
	if len(messages) != 1 {
		t.Fatalf("wrote %d messages, expected only the largest frame", len(messages))
	}
	if len(messages[0]) != settings.MaxMessageByteCount ||
		!bytes.Equal(messages[0][transportDelayHeaderByteCount:], largest) {
		t.Fatalf("largest frame wrote %d bytes, expected the %d-byte cap", len(messages[0]), settings.MaxMessageByteCount)
	}
}

// A frame the ready header reader prefetched is stripped like any other, but
// it waited behind the marker for a local reason, so it gives no delay
// sample: the echo our writers send is 0. A frame read in the steady state is
// a sample.
func TestTransportDelayP2pPrefetchedFrameIsStrippedAndCensored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sendConn, receiveConn := net.Pipe()
	settings := DefaultP2pTransportSettings()
	streamId := NewId()
	delaySettings := DefaultTransportDelaySettings()
	// only the prefetch censors; route handoff in this test never does
	delaySettings.ReceiverLimitedThreshold = time.Minute
	receiveDelay := newP2pTransportDelay(newTransportDelay(delaySettings, NewId(), TransportTypeP2p, nil))

	prefetchedPayload := transportDelayP2pTestMessage(500, 1)
	prefetched := MessagePoolGet(transportDelayHeaderByteCount + len(prefetchedPayload))
	prefetchedHeader := newTransportDelayHeader(1000, 0, 0)
	copy(prefetched, prefetchedHeader[:])
	copy(prefetched[transportDelayHeaderByteCount:], prefetchedPayload)
	receiveTransport, receiveLanes := newP2pReceiveTransportWithLanes(
		ctx, cancel, receiveConn, streamId, settings, [][]byte{prefetched}, nil, receiveDelay,
	)
	select {
	case <-ctx.Done():
		t.Fatal("prefetched frame was not delivered")
	case received := <-receiveLanes[0].route:
		if !bytes.Equal(received, prefetchedPayload) {
			t.Fatalf("prefetched frame delivered %d bytes, expected the %d-byte payload", len(received), len(prefetchedPayload))
		}
		MessagePoolReturn(received)
	}
	if echo := receiveDelay.delay.echo.Load(); echo != 0 {
		t.Fatalf("a prefetched frame was echoed as a %d us sample", echo)
	}

	steadyPayload := transportDelayP2pTestMessage(500, 2)
	steadyHeader := newTransportDelayHeader(2000, 0, 0)
	writeErr := make(chan error, 1)
	go func() {
		_, err := sendConn.Write(append(slices.Clone(steadyHeader[:]), steadyPayload...))
		writeErr <- err
	}()
	select {
	case <-ctx.Done():
		t.Fatal("steady-state frame was not delivered")
	case received := <-receiveLanes[0].route:
		if !bytes.Equal(received, steadyPayload) {
			t.Fatalf("steady-state frame delivered %d bytes", len(received))
		}
		MessagePoolReturn(received)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if echo := receiveDelay.delay.echo.Load(); echo == 0 {
		t.Fatal("a steady-state frame gave no delay sample")
	}
	closeTransportDelayP2pRoutes(t, []net.Conn{sendConn, receiveConn}, receiveTransport)
}

// A stream intermediary forwards bare transfer frames between two hops. Each
// hop strips and stamps on its own: the frame crosses the first, negotiated
// hop with one header and the second with one header only when that hop
// negotiated, never two, and both endpoints see the original bytes.
func TestTransportDelayP2pRelayStampsEachHopOnce(t *testing.T) {
	for _, secondHopNegotiated := range []bool{false, true} {
		t.Run(fmt.Sprintf("second hop negotiated %t", secondHopNegotiated), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			streamId := NewId()
			settings := DefaultP2pTransportSettings()
			sourceConn, relayInConn := net.Pipe()
			relayOutConn, destinationConn := net.Pipe()
			firstHopWire := &transportDelayP2pWireConn{Conn: sourceConn}
			secondHopWire := &transportDelayP2pWireConn{Conn: relayOutConn}

			var secondHopSendDelay, secondHopReceiveDelay *p2pTransportDelay
			if secondHopNegotiated {
				secondHopSendDelay = newStampOnlyTransportDelayP2p()
				secondHopReceiveDelay = newStampOnlyTransportDelayP2p()
			}
			destinationProbes := make(chan []byte, 4)
			destinationHandler := func(message []byte) bool {
				if !isP2pStreamProbe(message) {
					return false
				}
				destinationProbes <- bytes.Clone(message)
				return true
			}
			sourceTransport, sourceSend := newP2pSendTransportForPeerWithDelay(
				ctx, cancel, firstHopWire, NewId(), streamId, settings, false, nil, newStampOnlyTransportDelayP2p(),
			)
			// the intermediary relays probes unchanged, so it has no handler
			relayInTransport, relayInLanes := newP2pReceiveTransportWithLanes(
				ctx, cancel, relayInConn, streamId, settings, nil, nil, newStampOnlyTransportDelayP2p(),
			)
			relayOutTransport, relayOutSend := newP2pSendTransportForPeerWithDelay(
				ctx, cancel, secondHopWire, NewId(), streamId, settings, false, nil, secondHopSendDelay,
			)
			destinationTransport, destinationLanes := newP2pReceiveTransportWithLanes(
				ctx, cancel, destinationConn, streamId, settings, nil, destinationHandler, secondHopReceiveDelay,
			)
			forwardDone := make(chan struct{})
			go func() {
				defer close(forwardDone)
				for {
					var message []byte
					select {
					case <-ctx.Done():
						return
					case message = <-relayInLanes[0].route:
					case message = <-relayInLanes[1].route:
					}
					select {
					case <-ctx.Done():
						MessagePoolReturn(message)
						return
					case relayOutSend <- message:
					}
				}
			}()

			data := transportDelayP2pTestMessage(300, 5)
			sendAndReceiveTransportDelayP2p(t, ctx, sourceSend, destinationLanes, data)
			probe := encodeP2pStreamProbe(p2pStreamProbeRequestType, streamId, NewId())
			expectedProbe := bytes.Clone(probe)
			sourceSend <- probe
			select {
			case <-ctx.Done():
				t.Fatal("relayed probe was not handled at the destination")
			case handled := <-destinationProbes:
				if !bytes.Equal(handled, expectedProbe) {
					t.Fatal("the destination saw a changed probe envelope")
				}
			}
			cancel()
			<-forwardDone
			closeTransportDelayP2pRoutes(
				t,
				[]net.Conn{sourceConn, relayInConn, relayOutConn, destinationConn},
				sourceTransport,
				relayOutTransport,
				relayInTransport,
				destinationTransport,
			)

			secondHopHeaderByteCount := 0
			if secondHopNegotiated {
				secondHopHeaderByteCount = transportDelayHeaderByteCount
			}
			for hopIndex, hop := range []struct {
				wire            *transportDelayP2pWireConn
				headerByteCount int
			}{
				{wire: firstHopWire, headerByteCount: transportDelayHeaderByteCount},
				{wire: secondHopWire, headerByteCount: secondHopHeaderByteCount},
			} {
				messages := hop.wire.written()
				if len(messages) != 2 {
					t.Fatalf("hop %d wrote %d messages, expected 2", hopIndex, len(messages))
				}
				for messageIndex, expected := range [][]byte{data, expectedProbe} {
					message := messages[messageIndex]
					if len(message) != hop.headerByteCount+len(expected) ||
						!bytes.Equal(message[hop.headerByteCount:], expected) {
						t.Fatalf(
							"hop %d message %d = %d bytes, expected %d header bytes + %d",
							hopIndex,
							messageIndex,
							len(message),
							hop.headerByteCount,
							len(expected),
						)
					}
				}
			}
		})
	}
}

// The production association end to end: a client whose settings offer the
// header and whose edges accepted it negotiates it as the active side of a
// real offer/answer, both directions deliver through the route managers, and
// the generation's last interval is published to the offering client's
// subscribers when the generation ends. The passive client stamps and echoes
// and publishes nothing.
func TestTransportDelayP2pTransportNegotiatesAndFlushesOnGenerationEnd(t *testing.T) {
	testTransportDelayP2pTransportGeneration(t, true)
}

// The same client before any edge answered offers nothing: both directions
// deliver on the legacy wire, and nothing is published (DELAY.md §6.1).
func TestTransportDelayP2pTransportOffersNothingBeforeAnEdgeAccepts(t *testing.T) {
	testTransportDelayP2pTransportGeneration(t, false)
}

// One production association between an active client whose settings offer
// the header and a passive client. edgeAccepted records an edge's acceptance
// on the active client first, as its platform connections do.
func testTransportDelayP2pTransportGeneration(t *testing.T, edgeAccepted bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	delaySettings := testTransportDelayP2pSettings()
	// no interval closes on its own; the generation end publishes
	delaySettings.Interval = time.Hour
	activeClientSettings := closeWaitClientSettings()
	activeClientSettings.TransportDelay = delaySettings
	activeClient := NewClient(ctx, NewId(), NewNoContractClientOob(), activeClientSettings)
	passiveClient := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	if edgeAccepted {
		activeClient.RouteManager().noteTransportDelayEdgeAnswer(true)
	}
	defer func() {
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		for _, client := range []*Client{activeClient, passiveClient} {
			if err := client.CloseAndWait(joinCtx); err != nil {
				t.Errorf("join test client: %v", err)
			}
		}
	}()
	activeRecorder := &transportDelayP2pStatsRecorder{}
	passiveRecorder := &transportDelayP2pStatsRecorder{}
	defer activeClient.AddTransportDelayStatsCallback(activeRecorder.publish)()
	defer passiveClient.AddTransportDelayStatsCallback(passiveRecorder.publish)()

	activeSignals := newSignalPipe(nil)
	passiveSignals := newSignalPipe(nil)
	activeManager := newTestWebRtcManager(t, ctx, activeSignals, newTestWebRtcSettingsForTransportDelay())
	passiveManager := newTestWebRtcManager(t, ctx, passiveSignals, newTestWebRtcSettingsForTransportDelay())
	toPassive := &transportDelayP2pSignalReceiver{manager: passiveManager}
	toActive := &transportDelayP2pSignalReceiver{manager: activeManager}
	activeSignals.SetSignalReceiver(toPassive)
	passiveSignals.SetSignalReceiver(toActive)
	passiveRegistered := make(chan struct{}, 1)
	passiveManager.testingAfterPeerConnRegistered = func(TransferPath, bool) {
		select {
		case passiveRegistered <- struct{}{}:
		default:
		}
	}

	routeInstalled := make(chan P2pRouteState, 8)
	newSettings := func() *P2pTransportSettings {
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.RouteStateObserver = func(state P2pRouteState) {
			if state.Connected {
				select {
				case routeInstalled <- state:
				default:
				}
			}
		}
		return settings
	}
	streamId := NewId()
	activeSendRoutes := NewRouteManager(ctx, "delay-active-send")
	activeReceiveRoutes := NewRouteManager(ctx, "delay-active-receive")
	passiveSendRoutes := NewRouteManager(ctx, "delay-passive-send")
	passiveReceiveRoutes := NewRouteManager(ctx, "delay-passive-receive")
	passiveTransport := NewP2pTransport(
		ctx,
		passiveClient,
		passiveManager,
		passiveSendRoutes,
		passiveReceiveRoutes,
		activeClient.ClientId(),
		streamId,
		PeerTypeSource,
		newSettings(),
	)
	defer passiveTransport.Close()
	waitCloseWaitBarrier(t, ctx, passiveRegistered, "passive peer registration")
	activeTransport := NewP2pTransport(
		ctx,
		activeClient,
		activeManager,
		activeSendRoutes,
		activeReceiveRoutes,
		passiveClient.ClientId(),
		streamId,
		PeerTypeDestination,
		newSettings(),
	)
	defer activeTransport.Close()
	for range 4 {
		select {
		case <-ctx.Done():
			t.Fatal("P2P routes were not installed")
		case <-routeInstalled:
		}
	}
	expectedVersion := uint32(0)
	if edgeAccepted {
		expectedVersion = TransportDelayVersion
	}
	toPassive.requireVersions(t, protocol.SignalType_SdpOffer, expectedVersion)
	toActive.requireVersions(t, protocol.SignalType_SdpAnswer, expectedVersion)

	transfer := func(sendRoutes *RouteManager, destinationId Id, receiveRoutes *RouteManager, message []byte) {
		t.Helper()
		writer := sendRoutes.OpenMultiRouteWriter(DestinationId(destinationId))
		defer sendRoutes.CloseMultiRouteWriter(writer)
		reader := receiveRoutes.OpenMultiRouteReader(TransferPath{StreamId: streamId})
		defer receiveRoutes.CloseMultiRouteReader(reader)
		pooled := MessagePoolCopy(message)
		success, err := writer.WriteDetailed(ctx, pooled, 10*time.Second)
		if err != nil || !success {
			MessagePoolReturn(pooled)
			t.Fatalf("route write success=%t err=%v", success, err)
		}
		received, err := reader.Read(ctx, 10*time.Second)
		if err != nil || received == nil {
			t.Fatalf("route read err=%v", err)
		}
		defer MessagePoolReturn(received)
		if !bytes.Equal(received, message) {
			t.Fatalf("route delivered %d bytes, expected the %d-byte message unchanged", len(received), len(message))
		}
	}
	for messageIndex, byteCount := range []int{100, 300, 2000} {
		transfer(activeSendRoutes, passiveClient.ClientId(), passiveReceiveRoutes, transportDelayP2pTestMessage(byteCount, messageIndex))
		transfer(passiveSendRoutes, activeClient.ClientId(), activeReceiveRoutes, transportDelayP2pTestMessage(byteCount, messageIndex+10))
	}
	if published := activeRecorder.snapshot(); len(published) != 0 {
		t.Fatalf("published %d intervals before the generation ended", len(published))
	}

	if err := activeTransport.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := passiveTransport.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if passivePublished := passiveRecorder.snapshot(); len(passivePublished) != 0 {
		t.Fatalf("the passive client published %d intervals", len(passivePublished))
	}
	published := activeRecorder.snapshot()
	if !edgeAccepted {
		if len(published) != 0 {
			t.Fatalf("the legacy wire published %d intervals", len(published))
		}
		return
	}
	if len(published) != 1 {
		t.Fatalf("the generation end published %d intervals, expected 1", len(published))
	}
	interval := published[0]
	if interval.TransportType != TransportTypeP2p ||
		interval.ClientId != activeClient.ClientId() ||
		interval.ConnectionId == (Id{}) ||
		interval.SendByteCount == 0 ||
		interval.ReceiveByteCount == 0 {
		t.Fatalf("flushed interval = %+v", interval)
	}
	if !interval.ReceiveQueueDelayValid || !interval.SendQueueDelayValid {
		t.Fatalf("flushed interval has no queue samples: %+v", interval)
	}
}
