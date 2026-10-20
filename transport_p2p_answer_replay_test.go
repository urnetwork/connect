//go:build !js

// Passive answer replay must recover an exact repeated offer after normal
// zero-wait admission refuses its answer, without advancing Pion a second time.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// Records ownership at the real Client sender, including its unchanged gate.
type answerReplayAttempt struct {
	signals      *protocol.ExchangeSignals
	opts         []any
	refused      bool
	rootReturned bool
}

// Observes synchronous reply attempts without replacing Client admission.
type answerReplaySignalSender struct {
	sender   *ClientSignalSender
	attempts []answerReplayAttempt
}

// Consumes the frame through the real sender and releases its test-only share.
func (self *answerReplaySignalSender) SendSignal(destinationId Id, frame *protocol.Frame, opts ...any) {
	signals := &protocol.ExchangeSignals{}
	if err := ProtoUnmarshal(frame.MessageBytes, signals); err != nil {
		panic(err)
	}
	witness := MessagePoolShareReadOnly(frame.MessageBytes)
	self.sender.SendSignal(destinationId, frame, opts...)
	self.attempts = append(self.attempts, answerReplayAttempt{
		signals:      signals,
		opts:         slices.Clone(opts),
		refused:      frame.MessageBytes == nil,
		rootReturned: MessagePoolReturn(witness),
	})
}

// No interfaces, multicast DNS, STUN, TURN, or remote services are available.
// Pion still performs the real offer/answer state transitions and validation.
func newAnswerReplayPion(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	settings := webrtc.SettingEngine{}
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetInterfaceFilter(func(string) bool { return false })
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pc.Close(); err != nil {
			t.Errorf("close signaling-only Pion peer: %v", err)
		}
	})
	return pc
}

// Holds every sequence before Run so each fixed slot is released explicitly.
func newAnswerReplayClient(t *testing.T) (*Client, *captureLogger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 1
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) { <-ctx.Done() }
	log := &captureLogger{}
	settings.Log = log
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("close answer replay Client: %v", err)
		}
	})
	return client, log
}

// Uses the borrowed public frame entry. The deadline is a liveness backstop;
// the deliberately held admission slot establishes the refusal ordering.
func deliverAnswerReplayOffer(t *testing.T, manager *WebRtcManager, peerId Id, key TransferKey, signals *protocol.ExchangeSignals) {
	t.Helper()
	frame := RequireToFrameWithDefaultProtocolVersion(signals)
	done := make(chan error, 1)
	go func() {
		defer MessagePoolReturn(frame.MessageBytes)
		done <- manager.ReceiveSignal(SourceId(peerId), key, frame)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("receive offer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("offer reply blocked on a deliberately full Client admission gate")
	}
}

// Releases one actual queued Pack, including its admission and pooled owner.
func drainAnswerReplayPack(t *testing.T, sequence *SendSequence) *protocol.ExchangeSignals {
	t.Helper()
	var pack *SendPack
	select {
	case pack = <-sequence.packs:
	default:
		t.Fatal("expected an admitted Pack after the synchronous send returned")
	}
	defer pack.releaseRaw()
	defer pack.returnFrames()
	if pack.Frame.MessageType != protocol.MessageType_TransferExchangeSignals {
		return nil
	}
	signals := &protocol.ExchangeSignals{}
	if err := ProtoUnmarshal(pack.Frame.MessageBytes, signals); err != nil {
		t.Fatal(err)
	}
	return signals
}

// A real passive answer is cached before the Client's fixed slot refuses it.
// Repeating the same reset-marked offer must retry that answer through the
// same zero-wait gate, then complete the active Pion state after slot release.
func TestWebRtcPassiveAnswerReplayAfterPackAdmissionRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	client, log := newAnswerReplayClient(t)
	activePc := newAnswerReplayPion(t)
	passivePc := newAnswerReplayPion(t)
	if _, err := activePc.CreateDataChannel("synthetic-answer-replay", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := activePc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := activePc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	offerBytes, err := json.Marshal(&offer)
	if err != nil {
		t.Fatal(err)
	}
	peerId, streamId, remoteGeneration := NewId(), NewId(), NewId()
	replyKey := TransferKey{
		ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer,
		EncryptionCompanion: true,
	}
	occupied := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic-occupied-slot"})
	if !client.SendWithTimeout(occupied, peerId, nil, 0, replyKey,
		transferOptionsSetForceStream{ForceStream: false},
		transferOptionsSetCompanionContract{CompanionContract: true}) {
		MessagePoolReturn(occupied.MessageBytes)
		t.Fatal("failed to occupy the answer's actual Pack admission slot")
	}
	client.sendBuffer.mutex.Lock()
	sequence := client.sendBuffer.sendSequences[sendSequenceId{
		Destination: peerId, CompanionContract: true,
		EncryptionRole: sequenceTlsRoleServer, EncryptionCompanion: true,
	}]
	client.sendBuffer.mutex.Unlock()
	if sequence == nil {
		t.Fatal("answer route did not resolve to the occupied sequence")
	}
	peerCtx, cancelPeer := context.WithCancelCause(client.ctx)
	t.Cleanup(func() { cancelPeer(nil) })
	sender := &answerReplaySignalSender{sender: NewClientSignalSender(client)}
	conn := &peerConn{
		ctx: peerCtx, cancel: func() { cancelPeer(nil) }, log: NewNoopLogger(),
		key: peerConnKey{PeerId: peerId, StreamId: streamId}, pc: passivePc,
		signalSender: sender, signalGeneration: NewId(), immediateReconnect: make(chan struct{}),
	}
	manager := &WebRtcManager{log: NewNoopLogger(), peerConns: map[peerConnKey]*peerConn{conn.key: conn}}
	signals := &protocol.ExchangeSignals{
		StreamId: streamId.Bytes(), SenderGenerationId: remoteGeneration.Bytes(), ResetSignals: true,
		Signals: []*protocol.ExchangeSignal{{SignalType: protocol.SignalType_SdpOffer, Sdp: offerBytes}},
	}
	deliverAnswerReplayOffer(t, manager, peerId, replyKey, signals)
	cachedAnswer := conn.answerSignal()
	if conn.offerSignal() == nil || cachedAnswer == nil || len(sender.attempts) != 1 {
		t.Fatal("the initial offer did not cache an answer before its send attempt")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt != 0 {
			deliverAnswerReplayOffer(t, manager, peerId, replyKey, signals)
		}
		if len(sender.attempts) != attempt+1 {
			t.Fatalf("duplicate offer produced %d answer attempts; want %d while the original slot remains occupied", len(sender.attempts), attempt+1)
		}
		if !sender.attempts[attempt].refused || !sender.attempts[attempt].rootReturned {
			t.Fatal("a refused reply retained its pooled owner or bypassed admission")
		}
	}
	log.mu.Lock()
	refusalCount := 0
	for _, line := range log.info {
		if line == "[signal]send failed mode=receive-reply reason=not-admitted boundary=pack-admission kind=answer reset=false\n" {
			refusalCount++
		}
	}
	log.mu.Unlock()
	if refusalCount != 2 || len(sequence.packs) != 1 || conn.ctx.Err() != nil || client.ctx.Err() != nil {
		t.Fatalf("full-gate replay changed admission or lifetime: refusals=%d queued=%d peer=%v client=%v", refusalCount, len(sequence.packs), conn.ctx.Err(), client.ctx.Err())
	}
	if activePc.RemoteDescription() != nil {
		t.Fatal("a refused answer reached the active peer")
	}
	healthyPeerId := NewId()
	sendHealthySibling := func() {
		healthySibling := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic-healthy-sibling"})
		if !client.SendWithTimeout(healthySibling, healthyPeerId, nil, 0) {
			MessagePoolReturn(healthySibling.MessageBytes)
			t.Fatal("the refused/canceled peer prevented its healthy sibling from sending")
		}
		client.sendBuffer.mutex.Lock()
		healthySequence := client.sendBuffer.sendSequences[sendSequenceId{Destination: healthyPeerId}]
		client.sendBuffer.mutex.Unlock()
		if healthySequence == nil {
			t.Fatal("healthy sibling did not acquire its independent sequence")
		}
		drainAnswerReplayPack(t, healthySequence)
	}
	sendHealthySibling()
	drainAnswerReplayPack(t, sequence)
	deliverAnswerReplayOffer(t, manager, peerId, replyKey, signals)
	if len(sender.attempts) != 3 || sender.attempts[2].refused || sender.attempts[2].rootReturned {
		t.Fatal("the repeated offer did not transfer its cached answer into the released slot")
	}
	replayed := drainAnswerReplayPack(t, sequence)
	if replayed == nil || len(replayed.Signals) != 1 || !bytes.Equal(replayed.Signals[0].Sdp, cachedAnswer.Sdp) ||
		replayed.Signals[0].SignalType != protocol.SignalType_SdpAnswer || replayed.ResetSignals ||
		!bytes.Equal(replayed.SenderGenerationId, conn.signalGeneration.Bytes()) || conn.answerSignal() != cachedAnswer {
		t.Fatal("answer replay regenerated SDP, changed generation, or changed the reply reset flag")
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(replayed.Signals[0].Sdp, &answer); err != nil {
		t.Fatal(err)
	}
	if err := activePc.SetRemoteDescription(answer); err != nil || activePc.SignalingState() != webrtc.SignalingStateStable {
		t.Fatalf("replayed answer did not complete active Pion negotiation: %v", err)
	}
	cancelPeer(context.Canceled)
	deliverAnswerReplayOffer(t, manager, peerId, replyKey, signals)
	if len(sender.attempts) != 3 || client.ctx.Err() != nil {
		t.Fatal("a canceled peer replayed or canceled its shared Client")
	}
	sendHealthySibling()
}

// Only the exact cached offer may select its cached answer. Legacy peers have
// no generation tag, while a missing/changed tag on a tagged offer is distinct.
func TestWebRtcPassiveAnswerReplayRequiresMatchingOfferAndGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, tagged := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		generationId := NewId()
		acceptedOffer := &protocol.ExchangeSignal{SignalType: protocol.SignalType_SdpOffer, Sdp: []byte("synthetic-cached-offer")}
		cachedAnswer := &protocol.ExchangeSignal{SignalType: protocol.SignalType_SdpAnswer, Sdp: []byte("synthetic-cached-answer")}
		sender := &testingTransferKeySignalSender{sends: make(chan testingRecordedSignalSend, 8)}
		conn := &peerConn{
			ctx: ctx, log: NewNoopLogger(), signalSender: sender, signalGeneration: NewId(),
			key:   peerConnKey{PeerId: NewId(), StreamId: NewId()},
			offer: acceptedOffer, answer: cachedAnswer,
			remoteSignalGeneration: generationId, remoteSignalGenerationSet: tagged,
		}
		key := TransferKey{EncryptionRole: protocol.SequenceRole_SequenceRoleServer, EncryptionCompanion: true}
		// A nil Pion peer makes any second SDP application fail immediately.
		for _, rejected := range []struct {
			offer         []byte
			generationId  Id
			generationSet bool
		}{
			{offer: []byte("synthetic-different-offer"), generationId: generationId, generationSet: tagged},
			{offer: acceptedOffer.Sdp, generationId: NewId(), generationSet: true},
			{offer: acceptedOffer.Sdp, generationId: generationId, generationSet: !tagged},
		} {
			if err := conn.receiveSignalFromPeerWithTransferKey(&protocol.ExchangeSignal{
				SignalType: protocol.SignalType_SdpOffer, Sdp: rejected.offer,
			}, rejected.generationId, rejected.generationSet, key, true); err != nil {
				t.Fatal(err)
			}
		}
		if len(sender.sends) != 0 {
			t.Fatalf("tagged=%t: mismatched offer/generation replayed an answer", tagged)
		}
		if err := conn.receiveSignalFromPeerWithTransferKey(&protocol.ExchangeSignal{
			SignalType: protocol.SignalType_SdpOffer, Sdp: slices.Clone(acceptedOffer.Sdp),
		}, generationId, tagged, key, true); err != nil {
			t.Fatal(err)
		}
		if len(sender.sends) != 1 {
			t.Fatalf("tagged=%t: an identical decoded offer did not replay its cached answer", tagged)
		}
		sent := <-sender.sends
		if len(sent.signals.Signals) != 1 || !bytes.Equal(sent.signals.Signals[0].Sdp, cachedAnswer.Sdp) {
			t.Fatal("matching offer selected a different answer")
		}
		foundKey, nonBlocking := false, false
		for _, opt := range sent.opts {
			switch value := opt.(type) {
			case TransferKey:
				foundKey = value == key
			case signalSendNonBlocking:
				nonBlocking = true
			}
		}
		if !foundKey || !nonBlocking || conn.offerSignal() != acceptedOffer || conn.answerSignal() != cachedAnswer {
			t.Fatal("replay lost the immutable reply lane, zero-wait policy, or cached SDP")
		}
		conn.active = true
		if err := conn.receiveSignalFromPeerWithTransferKey(acceptedOffer, generationId, tagged, key, true); err != nil {
			t.Fatal(err)
		}
		conn.active = false
		conn.answer = nil
		if err := conn.receiveSignalFromPeerWithTransferKey(acceptedOffer, generationId, tagged, key, true); err != nil {
			t.Fatal(err)
		}
		if len(sender.sends) != 0 {
			t.Fatal("an active peer or an unfinished passive answer emitted a reply")
		}
	}
}
