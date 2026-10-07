package connect

// The exchange signaling of the peer-to-peer webrtc extender carrier
// (EXTENDER.md S). The carrier's rendezvous rides the same ExchangeSignals
// frames the p2p transport negotiates with, routed by the exchange to the
// extender's client id, with the `extender_carrier` flag telling the two
// apart: a flagged offer is for an extender, not for a transport peer
// connection, and the manager hands it to the installed answerer rather than
// to the keyed peerConns. A receiver that predates the flag sees an offer for
// a stream it has no peer connection for and drops it, which is how an old
// extender declines.
//
// Dial side: an exchanger for one extender allocates a stream id, sends the
// offer and waits for the answer under that id. Answer side: a flagged offer
// is answered on a bounded worker pool, never on the signal receive worker --
// an answer gathers ICE candidates, which is seconds, and the receive
// contract forbids blocking there -- and the answer is sent back on the
// companion of the dialer's contract exactly as a passive p2p peer replies.
//
// Safe for concurrent use.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// Bound on the wait for the carrier's data channel to open once the SDP
// exchange is done, when the settings leave it zero.
const defaultWebRtcExtenderCarrierOpenTimeout = 30 * time.Second

// Offers answered at once by one extender when the settings leave it zero.
const defaultWebRtcExtenderCarrierAnswerConcurrency = 8

// ErrWebRtcExtenderCarrierUnavailable is the dial of a webrtc profile through
// connect settings that carry no carrier (ConnectSettings.WebRtcExtenderCarrier),
// or through one that is closed or not built for this platform.
var ErrWebRtcExtenderCarrierUnavailable = errors.New("the webrtc extender carrier is not available")

// ErrWebRtcExtenderNoSignaling is a dial of an extender the carrier has no
// signaling path to: no resolver, or one that does not know the extender.
var ErrWebRtcExtenderNoSignaling = errors.New("the extender has no webrtc signaling path")

// WebRtcExtenderOfferExchanger carries one dialer SDP offer to the extender and
// returns its SDP answer. Production is the exchange signaling
// (WebRtcManager.ExtenderCarrierExchanger); tests inject an in-memory bridge.
// The offer carries its gathered ICE candidates (no trickle), so one round
// trip is enough. Implementations must return when ctx ends; carrier shutdown
// joins an outstanding exchange before releasing its negotiation resources.
type WebRtcExtenderOfferExchanger interface {
	ExchangeOffer(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error)
}

// WebRtcExtenderExchangerResolver finds the signaling to one extender by its
// identity key, which is all a dial profile carries: a device resolves the
// record's rendezvous id through its directory and signals through its
// client; the operator's probe knows the one extender it is proving. false is
// an extender with no signaling path.
type WebRtcExtenderExchangerResolver func(extenderPublicKey []byte) (WebRtcExtenderOfferExchanger, bool)

// WebRtcExtenderStreamHandler serves one accepted carrier stream on the
// extender: the A3 request, then the forward or the service, exactly as a
// terminated tcp connection is served. It owns conn until it returns; the
// carrier releases the peer connection after.
type WebRtcExtenderStreamHandler interface {
	HandleWebRtcExtenderStream(ctx context.Context, conn net.Conn)
}

// WebRtcExtenderOfferAnswerer answers one dialer offer on an extender, which
// the extender role installs on its client's manager
// (WebRtcManager.SetExtenderCarrierAnswerer). The carrier implements it
// (WebRtcExtenderCarrier.Answerer).
type WebRtcExtenderOfferAnswerer interface {
	AnswerWebRtcExtenderOffer(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error)
}

// WebRtcExtenderSignalingStats counts what the signaling refused, which a
// status reads and the tests assert.
type WebRtcExtenderSignalingStats struct {
	// offers that arrived with no answerer installed
	UnansweredOfferCount uint64
	// offers dropped at the answer concurrency bound
	DroppedOfferCount uint64
	// answers for a stream no dial was waiting on
	UnknownAnswerCount uint64
	// offers whose answer failed
	FailedAnswerCount uint64
}

// webRtcExtenderSignaling is the manager's carrier signaling state.
type webRtcExtenderSignaling struct {
	ctx          context.Context
	log          Logger
	signalSender SignalSender
	settings     *WebRtcSettings

	stateLock         sync.Mutex
	closed            bool
	answerer          WebRtcExtenderOfferAnswerer
	answerWorkerCount int
	waiters           map[peerConnKey]chan *protocol.ExchangeSignal

	unansweredOfferCount atomic.Uint64
	droppedOfferCount    atomic.Uint64
	unknownAnswerCount   atomic.Uint64
	failedAnswerCount    atomic.Uint64

	// Nil in production; tests observe one answer, successful or not, after
	// its outcome is counted and its reply, if any, is sent.
	afterAnswerForTest func()
	// Nil in production; tests hold an answer at its lifecycle boundary.
	afterWaiterLookupForTest func()
}

func newWebRtcExtenderSignaling(
	ctx context.Context,
	log Logger,
	signalSender SignalSender,
	settings *WebRtcSettings,
) *webRtcExtenderSignaling {
	return &webRtcExtenderSignaling{
		ctx:          ctx,
		log:          log,
		signalSender: signalSender,
		settings:     settings,
		waiters:      map[peerConnKey]chan *protocol.ExchangeSignal{},
	}
}

func (self *webRtcExtenderSignaling) answerConcurrency() int {
	if self.settings != nil && 0 < self.settings.ExtenderCarrierAnswerConcurrency {
		return self.settings.ExtenderCarrierAnswerConcurrency
	}
	return defaultWebRtcExtenderCarrierAnswerConcurrency
}

// close refuses every later offer and wakes every waiting dial; the manager
// context, which the answers run on, ends them.
func (self *webRtcExtenderSignaling) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	for key, waiter := range self.waiters {
		close(waiter)
		delete(self.waiters, key)
	}
}

func (self *webRtcExtenderSignaling) setAnswerer(answerer WebRtcExtenderOfferAnswerer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answerer = answerer
}

func (self *webRtcExtenderSignaling) stats() WebRtcExtenderSignalingStats {
	return WebRtcExtenderSignalingStats{
		UnansweredOfferCount: self.unansweredOfferCount.Load(),
		DroppedOfferCount:    self.droppedOfferCount.Load(),
		UnknownAnswerCount:   self.unknownAnswerCount.Load(),
		FailedAnswerCount:    self.failedAnswerCount.Load(),
	}
}

// receive applies one flagged batch: offers go to the answerer, answers to
// the dial waiting under their stream id. Nothing here blocks.
func (self *webRtcExtenderSignaling) receive(
	source TransferPath,
	transferKey TransferKey,
	streamId Id,
	signals []*protocol.ExchangeSignal,
) error {
	for _, signal := range signals {
		if signal == nil {
			continue
		}
		switch signal.SignalType {
		case protocol.SignalType_SdpOffer:
			self.receiveOffer(source, transferKey, streamId, signal)
		case protocol.SignalType_SdpAnswer:
			self.receiveAnswer(source, streamId, signal)
		default:
			// the carrier trickles nothing and never waits for an offer
		}
	}
	return nil
}

// receiveOffer admits one offer onto the bounded answer workers.
func (self *webRtcExtenderSignaling) receiveOffer(
	source TransferPath,
	transferKey TransferKey,
	streamId Id,
	signal *protocol.ExchangeSignal,
) {
	var answerer WebRtcExtenderOfferAnswerer
	admitted := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.answerer == nil {
			return
		}
		answerer = self.answerer
		if self.answerConcurrency() <= self.answerWorkerCount {
			return
		}
		self.answerWorkerCount += 1
		admitted = true
	}()
	if answerer == nil {
		self.unansweredOfferCount.Add(1)
		return
	}
	if !admitted {
		self.droppedOfferCount.Add(1)
		return
	}
	go func() {
		defer func() {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.answerWorkerCount -= 1
			}()
			if self.afterAnswerForTest != nil {
				self.afterAnswerForTest()
			}
		}()
		self.answer(answerer, source, transferKey, streamId, signal)
	}()
}

// answer runs one offer through the answerer and sends the answer back to
// the dialer under the same stream id.
func (self *webRtcExtenderSignaling) answer(
	answerer WebRtcExtenderOfferAnswerer,
	source TransferPath,
	transferKey TransferKey,
	streamId Id,
	signal *protocol.ExchangeSignal,
) {
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(signal.Sdp, &offer); err != nil {
		self.failedAnswerCount.Add(1)
		return
	}
	answer, err := answerer.AnswerWebRtcExtenderOffer(self.ctx, offer)
	if err != nil {
		self.failedAnswerCount.Add(1)
		if self.log.V(1).Enabled() {
			self.log.Infof("[extender-webrtc]answer err = %s\n", err)
		}
		return
	}
	answerBytes, err := json.Marshal(&answer)
	if err != nil {
		self.failedAnswerCount.Add(1)
		return
	}
	// the reply rides the companion of the dialer's contract, as a passive
	// p2p peer's answer does, so it is deliverable whatever the dialer holds
	self.signalSender.SendSignal(
		source.SourceId,
		webRtcExtenderSignalFrame(streamId, protocol.SignalType_SdpAnswer, answerBytes),
		transferKey,
		transferOptionsSetForceStream{ForceStream: false},
		transferOptionsSetCompanionContract{CompanionContract: true},
		Ctx(self.ctx),
	)
}

// receiveAnswer hands one answer to the dial waiting under its stream id.
func (self *webRtcExtenderSignaling) receiveAnswer(
	source TransferPath,
	streamId Id,
	signal *protocol.ExchangeSignal,
) {
	key := peerConnKey{
		PeerId:   source.SourceId,
		StreamId: streamId,
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	waiter := self.waiters[key]
	if self.afterWaiterLookupForTest != nil {
		self.afterWaiterLookupForTest()
	}
	if waiter == nil {
		self.unknownAnswerCount.Add(1)
		return
	}
	select {
	case waiter <- signal:
	default:
		// one answer per dial; a duplicate is dropped
	}
}

// register claims the stream id of one dial; false when the signaling is
// closed.
func (self *webRtcExtenderSignaling) register(key peerConnKey) (chan *protocol.ExchangeSignal, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, false
	}
	waiter := make(chan *protocol.ExchangeSignal, 1)
	self.waiters[key] = waiter
	return waiter, true
}

func (self *webRtcExtenderSignaling) unregister(key peerConnKey) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.waiters, key)
}

// webRtcExtenderSignalFrame builds one flagged signal batch.
func webRtcExtenderSignalFrame(
	streamId Id,
	signalType protocol.SignalType,
	sdpBytes []byte,
) *protocol.Frame {
	return RequireToFrameWithDefaultProtocolVersion(&protocol.ExchangeSignals{
		StreamId:        streamId.Bytes(),
		ExtenderCarrier: true,
		Signals: []*protocol.ExchangeSignal{
			{
				SignalType: signalType,
				Sdp:        sdpBytes,
			},
		},
	})
}

// webRtcExtenderSignalExchanger is the dial side's exchanger for one
// extender: one offer, one stream id, one answer.
type webRtcExtenderSignalExchanger struct {
	signaling        *webRtcExtenderSignaling
	extenderClientId Id
}

func (self *webRtcExtenderSignalExchanger) ExchangeOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	streamId := NewId()
	key := peerConnKey{
		PeerId:   self.extenderClientId,
		StreamId: streamId,
	}
	waiter, ok := self.signaling.register(key)
	if !ok {
		return webrtc.SessionDescription{}, fmt.Errorf("the webrtc extender signaling is closed")
	}
	defer self.signaling.unregister(key)

	offerBytes, err := json.Marshal(&offer)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	self.signaling.signalSender.SendSignal(
		self.extenderClientId,
		webRtcExtenderSignalFrame(streamId, protocol.SignalType_SdpOffer, offerBytes),
		Ctx(ctx),
	)

	select {
	case signal, ok := <-waiter:
		if !ok || signal == nil {
			return webrtc.SessionDescription{}, fmt.Errorf("the webrtc extender signaling is closed")
		}
		var answer webrtc.SessionDescription
		if err := json.Unmarshal(signal.Sdp, &answer); err != nil {
			return webrtc.SessionDescription{}, fmt.Errorf("decode extender answer: %w", err)
		}
		return answer, nil
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	case <-self.signaling.ctx.Done():
		return webrtc.SessionDescription{}, fmt.Errorf("the webrtc extender signaling is closed")
	}
}

// SetExtenderCarrierAnswerer installs the extender side of the webrtc carrier:
// flagged offers that arrive for this client are answered by it (EXTENDER.md
// S). Nil, the default, declines every offer, which is what a client that is
// not an extender does.
func (self *WebRtcManager) SetExtenderCarrierAnswerer(answerer WebRtcExtenderOfferAnswerer) {
	self.extenderCarrier.setAnswerer(answerer)
}

// ExtenderCarrierExchanger is the dial side of the webrtc carrier for one
// extender: the exchanger sends the offer to the extender's rendezvous id
// through this client's exchange path and awaits the answer.
func (self *WebRtcManager) ExtenderCarrierExchanger(extenderClientId Id) WebRtcExtenderOfferExchanger {
	return &webRtcExtenderSignalExchanger{
		signaling:        self.extenderCarrier,
		extenderClientId: extenderClientId,
	}
}

// ExtenderCarrierSignalingStats reports what the carrier signaling refused.
func (self *WebRtcManager) ExtenderCarrierSignalingStats() WebRtcExtenderSignalingStats {
	return self.extenderCarrier.stats()
}

// HasExtenderCarrierAnswerer reports whether an extender answerer is
// installed, which is whether this client serves the webrtc carrier.
func (self *WebRtcManager) HasExtenderCarrierAnswerer() bool {
	self.extenderCarrier.stateLock.Lock()
	defer self.extenderCarrier.stateLock.Unlock()
	return self.extenderCarrier.answerer != nil
}
