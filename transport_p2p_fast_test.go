//go:build !js

// This file pins the native fast carrier's hostile-input bounds, ownership,
// capability negotiation, and real SRTP message path.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// p2pFastPathTestPair owns a hermetic native WebRTC association and its
// matching stream identity.
type p2pFastPathTestPair struct {
	ctx          context.Context
	active       WebRtcConn
	passive      WebRtcConn
	streamId     Id
	signalErrors chan error
}

// p2pFastPathTestSignalReceiver records signaling errors that the production
// callback boundary intentionally cannot return to its sender.
type p2pFastPathTestSignalReceiver struct {
	receiver SignalReceiver
	errors   chan error
}

// ReceiveSignal delegates one frame and retains the first bounded diagnostic.
func (self *p2pFastPathTestSignalReceiver) ReceiveSignal(
	source TransferPath,
	frame *protocol.Frame,
) error {
	err := self.receiver.ReceiveSignal(source, frame)
	if err != nil {
		select {
		case self.errors <- err:
		default:
		}
	}
	return err
}

// newP2pFastPathTestPair establishes one loopback pair with independently
// configurable fast-path capability on each side.
func newP2pFastPathTestPair(
	t *testing.T,
	enableActive bool,
	enablePassive bool,
) *p2pFastPathTestPair {
	return newP2pFastPathTestPairForStreamWithStats(
		t,
		enableActive,
		enablePassive,
		NewId(),
		nil,
		nil,
	)
}

// A supplied stream identity lets several independent peer associations model
// the real source-to-destination chain without a direct-hop special case.
func newP2pFastPathTestPairForStream(
	t *testing.T,
	enableActive bool,
	enablePassive bool,
	streamId Id,
) *p2pFastPathTestPair {
	return newP2pFastPathTestPairForStreamWithStats(
		t,
		enableActive,
		enablePassive,
		streamId,
		nil,
		nil,
	)
}

// Optional counters expose carrier drops that occur before the P2P transport
// can observe a complete message.
func newP2pFastPathTestPairForStreamWithStats(
	t *testing.T,
	enableActive bool,
	enablePassive bool,
	streamId Id,
	activeStats *P2pDataPlaneStats,
	passiveStats *P2pDataPlaneStats,
) *p2pFastPathTestPair {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	settingsA := DefaultWebRtcSettings()
	settingsB := DefaultWebRtcSettings()
	settingsA.Log = NewNoopLogger()
	settingsB.Log = NewNoopLogger()
	settingsA.IceServerUrls = nil
	settingsB.IceServerUrls = nil
	settingsA.UseLoopbackOnlyIceInterfaces = true
	settingsB.UseLoopbackOnlyIceInterfaces = true
	settingsA.EnableDatagramFastPath = enableActive
	settingsB.EnableDatagramFastPath = enablePassive
	settingsA.DataPlaneStats = activeStats
	settingsB.DataPlaneStats = passiveStats

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)
	managerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	managerB := NewWebRtcManager(ctx, signalPipeB, settingsB)
	signalErrors := make(chan error, 8)
	signalPipeA.SetSignalReceiver(&p2pFastPathTestSignalReceiver{
		receiver: managerB,
		errors:   signalErrors,
	})
	signalPipeB.SetSignalReceiver(&p2pFastPathTestSignalReceiver{
		receiver: managerA,
		errors:   signalErrors,
	})
	peerIdA := NewId()
	peerIdB := NewId()
	passive, err := managerB.NewP2pConnPassive(
		ctx,
		NewTransferPath(peerIdB, peerIdA, streamId),
	)
	if err != nil {
		cancel()
		managerA.Close()
		managerB.Close()
		t.Fatal(err)
	}
	active, err := managerA.NewP2pConnActive(
		ctx,
		NewTransferPath(peerIdA, peerIdB, streamId),
	)
	if err != nil {
		passive.Close()
		cancel()
		managerA.Close()
		managerB.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		active.Close()
		passive.Close()
		managerA.Close()
		managerB.Close()
		cancel()
	})
	pair := &p2pFastPathTestPair{
		ctx:          ctx,
		active:       active,
		passive:      passive,
		streamId:     streamId,
		signalErrors: signalErrors,
	}
	pair.waitConnected(t)
	return pair
}

// Three real ICE/DTLS/SRTP associations prove that each stream intermediary
// can receive and forward the same production fast carrier. Each forwarder is
// an independent sender goroutine, so a blocking send never runs in a receive
// callback.
func TestP2pTransportFastPathComposesAcrossThreeHops(t *testing.T) {
	const hopCount = 3
	streamId := NewId()
	pairs := make([]*p2pFastPathTestPair, hopCount)
	for hopIndex := range pairs {
		pairs[hopIndex] = newP2pFastPathTestPairForStream(
			t,
			true,
			true,
			streamId,
		)
	}

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()
	sendRoutes := make([]chan<- []byte, hopCount)
	receiveRoutes := make([]<-chan []byte, hopCount)
	settings := make([]*P2pTransportSettings, hopCount)
	for hopIndex, pair := range pairs {
		settings[hopIndex] = DefaultP2pTransportSettings()
		settings[hopIndex].DataPlaneMode = P2pDataPlaneModeFastOnly
		settings[hopIndex].DataPlaneStats = &P2pDataPlaneStats{}
		_, sendRoute := NewP2pSendTransport(
			testCtx,
			testCancel,
			pair.active,
			streamId,
			settings[hopIndex],
		)
		_, receiveRoute := NewP2pReceiveTransport(
			testCtx,
			testCancel,
			pair.passive,
			streamId,
			settings[hopIndex],
		)
		sendRoutes[hopIndex] = sendRoute
		receiveRoutes[hopIndex] = receiveRoute
	}

	var forwardWaitGroup sync.WaitGroup
	for intermediaryIndex := 0; intermediaryIndex < hopCount-1; intermediaryIndex += 1 {
		forwardWaitGroup.Add(1)
		go func() {
			defer forwardWaitGroup.Done()
			for {
				var message []byte
				select {
				case <-testCtx.Done():
					return
				case message = <-receiveRoutes[intermediaryIndex]:
				}
				select {
				case <-testCtx.Done():
					MessagePoolReturn(message)
					return
				case sendRoutes[intermediaryIndex+1] <- message:
				}
			}
		}()
	}
	defer func() {
		testCancel()
		forwardWaitGroup.Wait()
	}()

	message := bytes.Repeat([]byte{0x4f}, 12*1024)
	pooledMessage := MessagePoolCopy(message)
	select {
	case <-testCtx.Done():
		MessagePoolReturn(pooledMessage)
		t.Fatal("multi-hop fast route stopped before send")
	case sendRoutes[0] <- pooledMessage:
	}
	select {
	case <-testCtx.Done():
		t.Fatal("multi-hop fast route stopped before receive")
	case <-time.After(10 * time.Second):
		t.Fatal("multi-hop fast route timed out")
	case received := <-receiveRoutes[hopCount-1]:
		if !bytes.Equal(received, message) {
			MessagePoolReturn(received)
			t.Fatal("multi-hop fast route changed the message")
		}
		MessagePoolReturn(received)
	}
	for hopIndex, hopSettings := range settings {
		stats := hopSettings.DataPlaneStats.Snapshot()
		if stats.FastSendMessageCount != 1 ||
			stats.FastReceiveMessageCount != 1 ||
			stats.LegacySendMessageCount != 0 ||
			stats.LegacyReceiveMessageCount != 0 ||
			stats.FastFallbackCount != 0 ||
			stats.FastDropCount != 0 {
			t.Fatalf("hop %d used the wrong data plane: %+v", hopIndex, stats)
		}
	}
}

// waitConnected waits for the detached data channel that gates a usable P2P
// association, independent of whether the optional carrier is enabled.
func (self *p2pFastPathTestPair) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !self.active.Connected() || !self.passive.Connected() {
		if time.Now().After(deadline) {
			active := self.active.(*peerConn)
			passive := self.passive.(*peerConn)
			var signalErr error
			select {
			case signalErr = <-self.signalErrors:
			default:
			}
			t.Fatalf(
				"peer pair did not connect: active=%t/%v/%s/%s passive=%t/%v/%s/%s signal=%v",
				self.active.Connected(),
				context.Cause(active.ctx),
				active.pc.ConnectionState(),
				active.pc.ICEConnectionState(),
				self.passive.Connected(),
				context.Cause(passive.ctx),
				passive.pc.ConnectionState(),
				passive.pc.ICEConnectionState(),
				signalErr,
			)
		}
		time.Sleep(time.Millisecond)
	}
}

// encodeP2pFastPathTestFragments creates wire payloads independently of the
// reassembler under test.
func encodeP2pFastPathTestFragments(
	t *testing.T,
	messageId uint32,
	message []byte,
) [][]byte {
	t.Helper()
	fragmentCount := p2pFastPathFragmentCount(len(message))
	fragments := make([][]byte, fragmentCount)
	for fragmentIndex := range fragmentCount {
		offset := fragmentIndex * p2pFastPathFragmentPayloadByteCount
		fragmentByteCount := min(
			p2pFastPathFragmentPayloadByteCount,
			len(message)-offset,
		)
		fragment := make(
			[]byte,
			p2pFastPathFragmentHeaderByteCount+fragmentByteCount,
		)
		err := writeP2pFastPathFragmentHeader(
			fragment,
			p2pFastPathFragmentHeader{
				messageId:     messageId,
				messageLength: len(message),
				fragmentIndex: fragmentIndex,
				fragmentCount: fragmentCount,
			},
		)
		if err != nil {
			t.Fatalf("encode fragment %d: %s", fragmentIndex, err)
		}
		copy(
			fragment[p2pFastPathFragmentHeaderByteCount:],
			message[offset:offset+fragmentByteCount],
		)
		fragments[fragmentIndex] = fragment
	}
	return fragments
}

// TestP2pFastPathReassemblyHandlesReorderingAndDuplicates verifies that a
// duplicated fragment does not advance completeness or corrupt later data.
func TestP2pFastPathReassemblyHandlesReorderingAndDuplicates(t *testing.T) {
	message := bytes.Repeat([]byte{0x5a}, 3*1024)
	fragments := encodeP2pFastPathTestFragments(t, 7, message)
	reassembler := newP2pFastPathReassembler(64 * 1024)
	defer reassembler.close()
	now := time.Now()

	order := []int{2, 0, 0, 1}
	var complete []byte
	for _, fragmentIndex := range order {
		var err error
		complete, err = reassembler.accept(fragments[fragmentIndex], now)
		if err != nil {
			t.Fatalf("accept fragment %d: %s", fragmentIndex, err)
		}
	}
	if !bytes.Equal(complete, message) {
		MessagePoolReturn(complete)
		t.Fatal("reordered message did not reassemble exactly")
	}
	MessagePoolReturn(complete)
}

// TestP2pFastPathReassemblyBoundsMalformedMessages checks wire sizes before a
// pooled complete-message allocation can occur.
func TestP2pFastPathReassemblyBoundsMalformedMessages(t *testing.T) {
	message := bytes.Repeat([]byte{0x7b}, 2*1024)
	fragment := encodeP2pFastPathTestFragments(t, 9, message)[0]
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "magic",
			mutate: func(packet []byte) {
				packet[0] = 0
			},
		},
		{
			name: "version",
			mutate: func(packet []byte) {
				packet[3] = 2
			},
		},
		{
			name: "message id",
			mutate: func(packet []byte) {
				clear(packet[4:8])
			},
		},
		{
			name: "fragment count",
			mutate: func(packet []byte) {
				packet[15] = p2pFastPathMaximumFragmentCount + 1
			},
		},
		{
			name: "truncated payload",
			mutate: func(packet []byte) {
				packet[len(packet)-1] = packet[len(packet)-1]
			},
		},
	}
	for _, test := range tests {
		packet := append([]byte(nil), fragment...)
		test.mutate(packet)
		if test.name == "truncated payload" {
			packet = packet[:len(packet)-1]
		}
		if _, err := parseP2pFastPathFragmentHeader(packet); err == nil {
			t.Fatalf("%s packet was accepted", test.name)
		}
	}

	reassembler := newP2pFastPathReassembler(1024)
	defer reassembler.close()
	if complete, err := reassembler.accept(fragment, time.Now()); err == nil || complete != nil {
		MessagePoolReturn(complete)
		t.Fatal("message above configured reassembly bound was accepted")
	}
}

// TestP2pFastPathReassemblyExpiresAndReusesCollidingSlot verifies that a
// stale incomplete owner is released before another message reuses its slot.
func TestP2pFastPathReassemblyExpiresAndReusesCollidingSlot(t *testing.T) {
	reassembler := newP2pFastPathReassembler(64 * 1024)
	defer reassembler.close()
	first := encodeP2pFastPathTestFragments(
		t,
		11,
		bytes.Repeat([]byte{0x11}, 2*1024),
	)
	secondMessage := bytes.Repeat([]byte{0x22}, 2*1024)
	second := encodeP2pFastPathTestFragments(
		t,
		11+p2pFastPathReassemblySlotCount,
		secondMessage,
	)
	now := time.Now()
	if complete, err := reassembler.accept(first[0], now); err != nil || complete != nil {
		MessagePoolReturn(complete)
		t.Fatalf("first incomplete fragment result=%d err=%v", len(complete), err)
	}
	var complete []byte
	for _, fragment := range second {
		var err error
		complete, err = reassembler.accept(
			fragment,
			now.Add(p2pFastPathReassemblyTimeout+time.Millisecond),
		)
		if err != nil {
			t.Fatalf("colliding fragment: %s", err)
		}
	}
	if !bytes.Equal(complete, secondMessage) {
		MessagePoolReturn(complete)
		t.Fatal("colliding message did not replace the stale slot")
	}
	MessagePoolReturn(complete)
}

// TestWebRtcFastPathNegotiatesAndTransfersThroughSrtp exercises the real ICE,
// DTLS, SRTP, fragmentation, and bounded reassembly path between two peers.
func TestWebRtcFastPathNegotiatesAndTransfersThroughSrtp(t *testing.T) {
	pair := newP2pFastPathTestPair(t, true, true)
	activeFast, ok := pair.active.(webRtcFastPathConn)
	if !ok {
		t.Fatal("native active peer does not expose the fast carrier")
	}
	passiveFast, ok := pair.passive.(webRtcFastPathConn)
	if !ok {
		t.Fatal("native passive peer does not expose the fast carrier")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !activeFast.FastPathReady() || !passiveFast.FastPathReady() {
		if time.Now().After(deadline) {
			t.Fatalf(
				"fast carrier did not bind: active=%t passive=%t",
				activeFast.FastPathReady(),
				passiveFast.FastPathReady(),
			)
		}
		time.Sleep(time.Millisecond)
	}

	message := bytes.Repeat([]byte{0xa5}, 12*1024)
	fragmentCount, err := activeFast.WriteFastPathMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if fragmentCount != p2pFastPathFragmentCount(len(message)) {
		t.Fatalf("fragment count = %d", fragmentCount)
	}
	select {
	case <-pair.ctx.Done():
		t.Fatal("timeout waiting for fast carrier message")
	case received := <-passiveFast.FastPathMessages():
		if !bytes.Equal(received.message, message) {
			MessagePoolReturn(received.message)
			t.Fatal("fast carrier changed the message")
		}
		MessagePoolReturn(received.message)
	}
}

// A full complete-message queue drops without blocking the SRTP reader and
// publishes the loss through the same counters used by route measurements.
func TestWebRtcFastPathCountsReceiveQueueDrop(t *testing.T) {
	stats := &P2pDataPlaneStats{}
	pair := newP2pFastPathTestPairForStreamWithStats(
		t,
		true,
		true,
		NewId(),
		nil,
		stats,
	)
	activeFast := pair.active.(webRtcFastPathConn)
	passivePeer := pair.passive.(*peerConn)
	passiveFast := pair.passive.(webRtcFastPathConn)
	deadline := time.Now().Add(10 * time.Second)
	for !activeFast.FastPathReady() || !passiveFast.FastPathReady() {
		if time.Now().After(deadline) {
			t.Fatal("fast carrier did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	messageCount := cap(passivePeer.fastPath.messages) + 1
	for messageIndex := range messageCount {
		message := []byte{0x71, byte(messageIndex)}
		if _, err := activeFast.WriteFastPathMessage(message); err != nil {
			t.Fatalf("write message %d: %s", messageIndex, err)
		}
	}
	for stats.Snapshot().FastDropCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("full fast receive queue did not record a drop")
		}
		time.Sleep(time.Millisecond)
	}
	if dropCount := stats.Snapshot().FastDropCount; dropCount != 1 {
		t.Fatalf("fast receive drop count=%d want=1", dropCount)
	}
	for range cap(passivePeer.fastPath.messages) {
		select {
		case received := <-passiveFast.FastPathMessages():
			MessagePoolReturn(received.message)
		case <-time.After(time.Second):
			t.Fatal("queued fast message was not available")
		}
	}
}

// TestP2pTransportFastOnlyUsesNoLegacyPayload verifies the production route
// worker, its forced-mode assertion, pooling handoff, and observable counters.
func TestP2pTransportFastOnlyUsesNoLegacyPayload(t *testing.T) {
	pair := newP2pFastPathTestPair(t, true, true)
	activeFast := pair.active.(webRtcFastPathConn)
	passiveFast := pair.passive.(webRtcFastPathConn)
	deadline := time.Now().Add(10 * time.Second)
	for !activeFast.FastPathReady() || !passiveFast.FastPathReady() {
		if time.Now().After(deadline) {
			t.Fatal("fast carrier did not become ready")
		}
		time.Sleep(time.Millisecond)
	}

	transportCtx, transportCancel := context.WithCancel(pair.ctx)
	defer transportCancel()
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneMode = P2pDataPlaneModeFastOnly
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	sendTransport, sendRoute := NewP2pSendTransport(
		transportCtx,
		transportCancel,
		pair.active,
		pair.streamId,
		settings,
	)
	receiveTransport, receiveRoute := NewP2pReceiveTransport(
		transportCtx,
		transportCancel,
		pair.passive,
		pair.streamId,
		settings,
	)
	_ = sendTransport
	_ = receiveTransport
	message := bytes.Repeat([]byte{0xc3}, 3*1024)
	pooledMessage := MessagePoolCopy(message)
	select {
	case <-transportCtx.Done():
		MessagePoolReturn(pooledMessage)
		t.Fatal("transport stopped before send")
	case sendRoute <- pooledMessage:
	}
	select {
	case <-transportCtx.Done():
		t.Fatal("transport stopped before receive")
	case received := <-receiveRoute:
		if !bytes.Equal(received, message) {
			MessagePoolReturn(received)
			t.Fatal("fast P2P route changed the message")
		}
		MessagePoolReturn(received)
	}
	for settings.DataPlaneStats.Snapshot().FastReceiveMessageCount != 1 {
		if time.Now().After(deadline) {
			t.Fatal("fast P2P receive counter did not advance")
		}
		time.Sleep(time.Millisecond)
	}
	stats := settings.DataPlaneStats.Snapshot()
	if stats.FastSendMessageCount != 1 ||
		stats.FastReceiveMessageCount != 1 ||
		stats.LegacySendMessageCount != 0 ||
		stats.LegacyReceiveMessageCount != 0 ||
		stats.FastFallbackCount != 0 {
		t.Fatalf("forced fast route stats = %+v", stats)
	}
}

// TestP2pTransportAutoSelectsFastPathForCapablePeer proves the production
// default switches to SRTP after mutual capability and readiness negotiation.
func TestP2pTransportAutoSelectsFastPathForCapablePeer(t *testing.T) {
	pair := newP2pFastPathTestPair(t, true, true)
	activeFast := pair.active.(webRtcFastPathConn)
	passiveFast := pair.passive.(webRtcFastPathConn)
	deadline := time.Now().Add(10 * time.Second)
	for !activeFast.FastPathReady() || !passiveFast.FastPathReady() {
		if time.Now().After(deadline) {
			t.Fatal("automatic fast carrier did not become ready")
		}
		time.Sleep(time.Millisecond)
	}

	transportCtx, transportCancel := context.WithCancel(pair.ctx)
	defer transportCancel()
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	sendTransport, sendRoute := NewP2pSendTransport(
		transportCtx,
		transportCancel,
		pair.active,
		pair.streamId,
		settings,
	)
	receiveTransport, receiveRoute := NewP2pReceiveTransport(
		transportCtx,
		transportCancel,
		pair.passive,
		pair.streamId,
		settings,
	)
	_ = sendTransport
	_ = receiveTransport
	message := bytes.Repeat([]byte{0x7c}, 2*1024)
	pooledMessage := MessagePoolCopy(message)
	select {
	case <-transportCtx.Done():
		MessagePoolReturn(pooledMessage)
		t.Fatal("automatic transport stopped before send")
	case sendRoute <- pooledMessage:
	}
	select {
	case <-transportCtx.Done():
		t.Fatal("automatic transport stopped before receive")
	case received := <-receiveRoute:
		if !bytes.Equal(received, message) {
			MessagePoolReturn(received)
			t.Fatal("automatic fast route changed the message")
		}
		MessagePoolReturn(received)
	}
	for settings.DataPlaneStats.Snapshot().FastReceiveMessageCount != 1 {
		if time.Now().After(deadline) {
			t.Fatal("automatic fast receive counter did not advance")
		}
		time.Sleep(time.Millisecond)
	}
	stats := settings.DataPlaneStats.Snapshot()
	if stats.FastSendMessageCount != 1 ||
		stats.FastReceiveMessageCount != 1 ||
		stats.LegacySendMessageCount != 0 ||
		stats.LegacyReceiveMessageCount != 0 ||
		stats.FastFallbackCount != 0 {
		t.Fatalf("automatic capable-peer stats = %+v", stats)
	}
}

// TestP2pTransportAutoFallsBackToLegacyPeer verifies rolling-upgrade
// compatibility when only one peer advertises the new RTP codec.
func TestP2pTransportAutoFallsBackToLegacyPeer(t *testing.T) {
	pair := newP2pFastPathTestPair(t, true, false)
	transportCtx, transportCancel := context.WithCancel(pair.ctx)
	defer transportCancel()
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneMode = P2pDataPlaneModeAuto
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	sendTransport, sendRoute := NewP2pSendTransport(
		transportCtx,
		transportCancel,
		pair.active,
		pair.streamId,
		settings,
	)
	receiveTransport, receiveRoute := NewP2pReceiveTransport(
		transportCtx,
		transportCancel,
		pair.passive,
		pair.streamId,
		settings,
	)
	_ = sendTransport
	_ = receiveTransport
	message := bytes.Repeat([]byte{0x8d}, 2*1024)
	pooledMessage := MessagePoolCopy(message)
	select {
	case <-transportCtx.Done():
		MessagePoolReturn(pooledMessage)
		t.Fatal("mixed transport stopped before send")
	case sendRoute <- pooledMessage:
	}
	select {
	case <-transportCtx.Done():
		t.Fatal("mixed transport stopped before receive")
	case received := <-receiveRoute:
		if !bytes.Equal(received, message) {
			MessagePoolReturn(received)
			t.Fatal("legacy fallback changed the message")
		}
		MessagePoolReturn(received)
	}
	deadline := time.Now().Add(time.Second)
	for settings.DataPlaneStats.Snapshot().LegacyReceiveMessageCount != 1 {
		if time.Now().After(deadline) {
			t.Fatal("legacy receive counter did not advance")
		}
		time.Sleep(time.Millisecond)
	}
	stats := settings.DataPlaneStats.Snapshot()
	if stats.FastSendMessageCount != 0 ||
		stats.LegacySendMessageCount != 1 ||
		stats.LegacyReceiveMessageCount != 1 ||
		stats.FastFallbackCount != 1 {
		t.Fatalf("mixed compatibility stats = %+v", stats)
	}
}

// A forced legacy client must omit the capability so an automatic peer cannot
// select a one-way fast lane. The caller's reusable settings stay unchanged.
func TestClientLegacyDataPlaneDoesNotAdvertiseFastPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	p2pSettings := settings.StreamManagerSettings.
		StreamBufferSettings.
		P2pTransportSettings
	p2pSettings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
	stats := &P2pDataPlaneStats{}
	p2pSettings.DataPlaneStats = stats
	client := NewClient(
		ctx,
		NewId(),
		NewNoContractClientOob(),
		settings,
	)
	defer client.Close()
	if !settings.WebRtcSettings.EnableDatagramFastPath {
		t.Fatal("client construction changed the caller's WebRTC settings")
	}
	if client.settings.WebRtcSettings.EnableDatagramFastPath {
		t.Fatal("legacy-only client advertised the fast-path codec")
	}
	if client.settings.WebRtcSettings.DataPlaneStats != stats {
		t.Fatal("WebRTC carrier did not share the P2P data-plane counters")
	}
}
