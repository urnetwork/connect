//go:build !js

package connect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// Tests of the carrier's exchange signaling (EXTENDER.md S,
// net_extender_webrtc_signal.go): two managers bridged by an in-memory signal
// sender that delivers each frame to the destination manager's receive path,
// exactly as the exchange does, so what is proved is the manager's own
// routing of flagged frames. The pure tests use a fake answerer and no pion;
// the last one runs the real carrier over the virtual network.

// testSignalBridge routes frames between managers by client id.
type testSignalBridge struct {
	lock     sync.Mutex
	managers map[Id]*WebRtcManager
	// runs after each delivery, with the frame already applied
	afterDeliver func(fromId Id, toId Id)
	undelivered  atomic.Int32
}

func newTestSignalBridge() *testSignalBridge {
	return &testSignalBridge{managers: map[Id]*WebRtcManager{}}
}

func (self *testSignalBridge) manager(id Id) *WebRtcManager {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.managers[id]
}

// Installation and delivery can overlap when an earlier offer is answering.
func (self *testSignalBridge) setAfterDeliver(afterDeliver func(fromId Id, toId Id)) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.afterDeliver = afterDeliver
}

// Builds one manager on the bridge under its own id.
func (self *testSignalBridge) newManager(t *testing.T, ctx context.Context, configure func(settings *WebRtcSettings)) (*WebRtcManager, Id) {
	t.Helper()
	id := NewId()
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	if configure != nil {
		configure(settings)
	}
	manager := newTestWebRtcManager(t, ctx, &testSignalSender{bridge: self, selfId: id}, settings)
	func() {
		self.lock.Lock()
		defer self.lock.Unlock()
		self.managers[id] = manager
	}()
	return manager, id
}

// testSignalSender is one manager's SignalSender on the bridge. It consumes
// the frame as the client sender does.
type testSignalSender struct {
	bridge *testSignalBridge
	selfId Id
}

func (self *testSignalSender) SendSignal(destinationId Id, signal *protocol.Frame, opts ...any) {
	defer MessagePoolReturn(signal.MessageBytes)
	manager := self.bridge.manager(destinationId)
	if manager == nil {
		self.bridge.undelivered.Add(1)
		return
	}
	_ = manager.ReceiveSignal(
		TransferPath{SourceId: self.selfId, DestinationId: destinationId},
		TransferKey{},
		signal,
	)
	afterDeliver := func() func(Id, Id) {
		self.bridge.lock.Lock()
		defer self.bridge.lock.Unlock()
		return self.bridge.afterDeliver
	}()
	if afterDeliver != nil {
		afterDeliver(self.selfId, destinationId)
	}
}

// testOfferAnswerer answers with a fixed description, optionally holding
// each answer until released.
type testOfferAnswerer struct {
	answer  webrtc.SessionDescription
	err     error
	offers  chan webrtc.SessionDescription
	release chan struct{}
	calls   atomic.Int32
}

func newTestOfferAnswerer() *testOfferAnswerer {
	return &testOfferAnswerer{
		answer: webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "v=0\r\ns=answer\r\n"},
		offers: make(chan webrtc.SessionDescription, 8),
	}
}

func (self *testOfferAnswerer) AnswerWebRtcExtenderOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	self.calls.Add(1)
	self.offers <- offer
	if self.release != nil {
		select {
		case <-self.release:
		case <-ctx.Done():
			return webrtc.SessionDescription{}, ctx.Err()
		}
	}
	return self.answer, self.err
}

var testOffer = webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\ns=offer\r\n"}

// Root cause: without a rendezvous the carrier has no way to reach an
// extender. Observable: an offer sent to the extender's id through the
// exchange path comes back answered, and the extender saw exactly the offer
// the dialer sent.
func TestWebRtcExtenderSignalingAnswersAFlaggedOffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	dialer, _ := bridge.newManager(t, ctx, nil)
	extender, extenderId := bridge.newManager(t, ctx, nil)
	answerer := newTestOfferAnswerer()
	extender.SetExtenderCarrierAnswerer(answerer)

	answer, err := dialer.ExtenderCarrierExchanger(extenderId).ExchangeOffer(ctx, testOffer)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if answer.Type != webrtc.SDPTypeAnswer || answer.SDP != answerer.answer.SDP {
		t.Fatalf("answer = %+v, want the extender's", answer)
	}
	offer := <-answerer.offers
	if offer.Type != webrtc.SDPTypeOffer || offer.SDP != testOffer.SDP {
		t.Fatalf("the extender saw offer %+v", offer)
	}
	if stats := extender.ExtenderCarrierSignalingStats(); stats != (WebRtcExtenderSignalingStats{}) {
		t.Fatalf("extender stats = %+v, want nothing refused", stats)
	}
	if stats := dialer.ExtenderCarrierSignalingStats(); stats != (WebRtcExtenderSignalingStats{}) {
		t.Fatalf("dialer stats = %+v, want nothing refused", stats)
	}
	if bridge.undelivered.Load() != 0 {
		t.Fatalf("undelivered frames = %d", bridge.undelivered.Load())
	}
}

// A client that is not an extender declines every offer: nothing is sent
// back, the refusal is counted, and the dialer ends on its own context.
func TestWebRtcExtenderSignalingDeclinesWithoutAnAnswerer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	dialer, _ := bridge.newManager(t, ctx, nil)
	extender, extenderId := bridge.newManager(t, ctx, nil)

	dialCtx, dialCancel := context.WithCancel(ctx)
	defer dialCancel()
	bridge.setAfterDeliver(func(fromId Id, toId Id) {
		if toId == extenderId {
			// the offer has been applied and declined; nothing will answer
			dialCancel()
		}
	})
	_, err := dialer.ExtenderCarrierExchanger(extenderId).ExchangeOffer(dialCtx, testOffer)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the dialer's cancellation", err)
	}
	stats := extender.ExtenderCarrierSignalingStats()
	if stats.UnansweredOfferCount != 1 || stats.DroppedOfferCount != 0 {
		t.Fatalf("extender stats = %+v, want one unanswered offer", stats)
	}
}

// Root cause: an answer gathers candidates for seconds, so unbounded offers
// would pile up goroutines and peer connections on an extender. Observable:
// past the concurrency bound an offer is dropped and counted while the
// admitted one still completes.
func TestWebRtcExtenderSignalingBoundsTheAnswersInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	dialer, _ := bridge.newManager(t, ctx, nil)
	extender, extenderId := bridge.newManager(t, ctx, func(settings *WebRtcSettings) {
		settings.ExtenderCarrierAnswerConcurrency = 1
	})
	answerer := newTestOfferAnswerer()
	answerer.release = make(chan struct{})
	extender.SetExtenderCarrierAnswerer(answerer)

	firstDone := make(chan error, 1)
	go func() {
		_, err := dialer.ExtenderCarrierExchanger(extenderId).ExchangeOffer(ctx, testOffer)
		firstDone <- err
	}()
	// the first offer holds the one worker
	<-answerer.offers

	secondCtx, secondCancel := context.WithCancel(ctx)
	defer secondCancel()
	bridge.setAfterDeliver(func(fromId Id, toId Id) {
		if toId == extenderId {
			secondCancel()
		}
	})
	_, err := dialer.ExtenderCarrierExchanger(extenderId).ExchangeOffer(secondCtx, testOffer)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("second offer err = %v, want the dialer's cancellation", err)
	}
	if stats := extender.ExtenderCarrierSignalingStats(); stats.DroppedOfferCount != 1 {
		t.Fatalf("extender stats = %+v, want one dropped offer", stats)
	}
	if answerer.calls.Load() != 1 {
		t.Fatalf("answerer calls = %d, want the admitted offer only", answerer.calls.Load())
	}

	close(answerer.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("the admitted offer failed: %v", err)
	}
}

// An answer for a stream no dial is waiting on is dropped and counted; it
// cannot reach a peer connection or a stale waiter.
func TestWebRtcExtenderSignalingDropsAnAnswerNobodyWaitsFor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	dialer, _ := bridge.newManager(t, ctx, nil)
	_, extenderId := bridge.newManager(t, ctx, nil)

	frame := webRtcExtenderSignalFrame(NewId(), protocol.SignalType_SdpAnswer, []byte(`{"type":"answer","sdp":"v=0"}`))
	defer MessagePoolReturn(frame.MessageBytes)
	if err := dialer.ReceiveSignal(TransferPath{SourceId: extenderId}, TransferKey{}, frame); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if stats := dialer.ExtenderCarrierSignalingStats(); stats.UnknownAnswerCount != 1 {
		t.Fatalf("dialer stats = %+v, want one unknown answer", stats)
	}
}

// Root cause: the carrier rides the p2p signaling frames, so without the
// flag a p2p offer for an unknown stream could reach the extender's answerer
// and an extender's answerer could capture p2p negotiation. Observable: an
// unflagged offer never reaches the answerer, the flagged one does.
func TestWebRtcExtenderSignalingLeavesP2pOffersToThePeerConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	_, dialerId := bridge.newManager(t, ctx, nil)
	extender, _ := bridge.newManager(t, ctx, nil)
	answerer := newTestOfferAnswerer()
	extender.SetExtenderCarrierAnswerer(answerer)
	answered := make(chan struct{}, 2)
	extender.extenderCarrier.afterAnswerForTest = func() { answered <- struct{}{} }

	p2pFrame := RequireToFrameWithDefaultProtocolVersion(&protocol.ExchangeSignals{
		StreamId: NewId().Bytes(),
		Signals: []*protocol.ExchangeSignal{
			{SignalType: protocol.SignalType_SdpOffer, Sdp: []byte(`{"type":"offer","sdp":"v=0"}`)},
		},
	})
	defer MessagePoolReturn(p2pFrame.MessageBytes)
	if err := extender.ReceiveSignal(TransferPath{SourceId: dialerId}, TransferKey{}, p2pFrame); err != nil {
		t.Fatalf("receive the p2p offer: %v", err)
	}
	if answerer.calls.Load() != 0 {
		t.Fatalf("a p2p offer reached the extender answerer")
	}
	if stats := extender.ExtenderCarrierSignalingStats(); stats != (WebRtcExtenderSignalingStats{}) {
		t.Fatalf("extender stats = %+v, want the p2p offer uncounted", stats)
	}

	carrierFrame := webRtcExtenderSignalFrame(NewId(), protocol.SignalType_SdpOffer, []byte(`{"type":"offer","sdp":"v=0"}`))
	defer MessagePoolReturn(carrierFrame.MessageBytes)
	if err := extender.ReceiveSignal(TransferPath{SourceId: dialerId}, TransferKey{}, carrierFrame); err != nil {
		t.Fatalf("receive the carrier offer: %v", err)
	}
	<-answered
	if answerer.calls.Load() != 1 {
		t.Fatalf("answerer calls = %d, want the flagged offer", answerer.calls.Load())
	}
}

// A failed answer is counted and sends nothing back.
func TestWebRtcExtenderSignalingCountsAFailedAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	dialer, dialerId := bridge.newManager(t, ctx, nil)
	extender, _ := bridge.newManager(t, ctx, nil)
	answerer := newTestOfferAnswerer()
	answerer.err = errors.New("no interface")
	extender.SetExtenderCarrierAnswerer(answerer)
	answered := make(chan struct{}, 1)
	extender.extenderCarrier.afterAnswerForTest = func() { answered <- struct{}{} }

	frame := webRtcExtenderSignalFrame(NewId(), protocol.SignalType_SdpOffer, []byte(`{"type":"offer","sdp":"v=0"}`))
	defer MessagePoolReturn(frame.MessageBytes)
	if err := extender.ReceiveSignal(TransferPath{SourceId: dialerId}, TransferKey{}, frame); err != nil {
		t.Fatal(err)
	}
	<-answered
	if stats := extender.ExtenderCarrierSignalingStats(); stats.FailedAnswerCount != 1 {
		t.Fatalf("extender stats = %+v, want one failed answer", stats)
	}
	if stats := dialer.ExtenderCarrierSignalingStats(); stats.UnknownAnswerCount != 0 {
		t.Fatalf("a failed answer sent something back: %+v", stats)
	}
}

// A dial waiting on an answer ends when its manager closes, not on its
// context alone.
func TestWebRtcExtenderSignalingEndsADialWhenTheManagerCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge := newTestSignalBridge()
	dialer, _ := bridge.newManager(t, ctx, nil)
	extender, extenderId := bridge.newManager(t, ctx, nil)
	answerer := newTestOfferAnswerer()
	answerer.release = make(chan struct{})
	extender.SetExtenderCarrierAnswerer(answerer)

	done := make(chan error, 1)
	go func() {
		_, err := dialer.ExtenderCarrierExchanger(extenderId).ExchangeOffer(ctx, testOffer)
		done <- err
	}()
	<-answerer.offers
	dialer.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("a dial outlived its manager")
		}
	case <-ctx.Done():
		t.Fatalf("the dial did not end with its manager")
	}
	// a dial on the closed manager is refused at once
	if _, err := dialer.ExtenderCarrierExchanger(extenderId).ExchangeOffer(ctx, testOffer); err == nil {
		t.Fatalf("a closed manager accepted a dial")
	}
}

// The whole rendezvous: the real carrier on both managers, the offer and
// answer crossing the bridged exchange path, and the hello forwarded over the
// data channel that results, behind the simulated NAT (C2a).
func TestWebRtcExtenderCarrierRendezvousOverTheExchangeSignaling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	nat := newTestWebRtcNat(t)
	operator := newTestWebRtcOperator(t)
	extender := newTestWebRtcExtender(t, operator.Listener.Addr().String())
	bridge := newTestSignalBridge()
	dialerManager, _ := bridge.newManager(t, ctx, nil)
	extenderManager, extenderId := bridge.newManager(t, ctx, nil)

	extenderCarrier := NewWebRtcExtenderCarrier(ctx, testWebRtcCarrierSettings(nat.extenderNet), nil)
	defer extenderCarrier.Close()
	extenderManager.SetExtenderCarrierAnswerer(extenderCarrier.Answerer(extender))

	// the dial side resolves the extender's rendezvous id, as a device does
	// from the record, and signals through its own manager
	dialerCarrier := NewWebRtcExtenderCarrier(
		ctx,
		testWebRtcCarrierSettings(nat.dialerNet),
		func(extenderPublicKey []byte) (WebRtcExtenderOfferExchanger, bool) {
			return dialerManager.ExtenderCarrierExchanger(extenderId), true
		},
	)
	defer dialerCarrier.Close()
	connectSettings := DefaultConnectSettings()
	connectSettings.WebRtcExtenderCarrier = dialerCarrier

	conn, response, err := DialExtender(ctx, connectSettings, &ExtenderConfig{
		Profile:   ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort},
		PublicKey: extender.publicKey,
	}, &ExtenderDial{DestinationHost: testWebRtcDestinationApi, DestinationPort: 443})
	if err != nil {
		t.Fatalf("dial through the exchange signaling: %v", err)
	}
	defer conn.Close()
	if response == nil || len(response.Carriers) != 1 || response.Carriers[0] != ExtenderCarrierWebRtc {
		t.Fatalf("response = %+v", response)
	}
	if clientAddress := testWebRtcHelloThrough(t, ctx, conn); clientAddress != testWebRtcHelloClientIp {
		t.Fatalf("hello client_address = %q", clientAddress)
	}
	if stats := extenderManager.ExtenderCarrierSignalingStats(); stats != (WebRtcExtenderSignalingStats{}) {
		t.Fatalf("extender stats = %+v", stats)
	}
}
