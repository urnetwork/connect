//go:build !js

package connect

// The peer-to-peer webrtc extender carrier (EXTENDER.md S; acceptance C2a).
// A home extender behind NAT has no public inbound address, so it cannot
// serve the tcp, udp or dns carriers. This carrier reaches it the other way
// round: the dialer -- a client, or the operator's activation probe -- sends
// an SDP offer to the extender through the exchange signaling, the extender
// answers, and ICE/STUN hole punching joins the two; the data channel that
// opens is one reliable byte stream, exactly what the other carriers yield,
// and the ordinary A3 request and inner bytes run over it.
//
// The carrier is not an ip:port dial, so dialExtenderStream hands a profile in
// ExtenderConnectModeWebRtc here before it builds a header. There is no outer
// tls to pin a leaf against: the data channel is already authenticated dtls,
// and the extender's identity is the challenge signature in its response, so
// a dial that knows the extender's key (a verified record, a probe) always
// challenges and verifies, whatever the caller asked for.
//
// The peer connections come from one factory per carrier, so a carrier's
// sessions share one certificate and inherit the per-session STUN pool
// (transport_p2p_webrtc_stun.go) and, on the extender, the browser dtls
// ClientHello (transport_p2p_webrtc_dtls.go): the extender is the SDP
// answerer and so the dtls client that sends the ClientHello mimicry shapes.
//
// The signaling is an interface. Production is the exchange: the dialer's
// manager sends the offer to the extender's client id and awaits the answer
// (net_extender_webrtc_signal.go); tests bridge the two sides in memory. The
// extender side is the same carrier bound to a stream handler, which the
// extender server implements by serving the stream as it serves a terminated
// tcp connection.
//
// Not built for js/wasm: the browser SDK reaches webrtc through the browser,
// not pion's detached data channels.
//
// Safe for concurrent use: dials and answers run in parallel on one carrier.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// The data channel label of the carrier. A peer connection opened for the
// carrier uses exactly this label; any other data channel on it is refused.
const webRtcExtenderDataChannelLabel = "ur-extender"

// WebRtcExtenderCarrier is one owner's carrier: the dial side on a device or
// the operator, the answer side on an extender, or both. It holds the peer
// connection factory the sessions share.
type WebRtcExtenderCarrier struct {
	ctx      context.Context
	cancel   context.CancelFunc
	log      Logger
	settings *WebRtcSettings
	resolver WebRtcExtenderExchangerResolver

	stateLock    sync.Mutex
	factory      *webRtcPeerConnectionFactory
	factoryErr   error
	closed       bool
	pendingDials map[*webRtcExtenderPendingDial]bool
	closeDone    chan struct{}

	// Nil in production; tests observe the answer side releasing a peer
	// connection whose data channel never opened.
	answerReleasedForTest func()
}

// The carrier owns cancellation until a dial has returned its stream or
// released its failed peer connection. Close joins that ownership boundary.
type webRtcExtenderPendingDial struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// DefaultWebRtcExtenderSettings are the peer connection settings of the
// carrier: the p2p defaults with the browser dtls ClientHello on, since the
// carrier is the censorship-circumvention path, and no datagram fast path,
// since the carrier is one byte stream.
func DefaultWebRtcExtenderSettings() *WebRtcSettings {
	settings := DefaultWebRtcSettings()
	settings.DtlsClientHelloMimicry = true
	settings.EnableDatagramFastPath = false
	return settings
}

// NewWebRtcExtenderCarrier builds a carrier. resolver is the dial side's
// signaling; nil on an extender that only answers. The factory is built on
// the first session, so a carrier that is never used costs nothing.
func NewWebRtcExtenderCarrier(
	ctx context.Context,
	settings *WebRtcSettings,
	resolver WebRtcExtenderExchangerResolver,
) *WebRtcExtenderCarrier {
	if settings == nil {
		settings = DefaultWebRtcExtenderSettings()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	return &WebRtcExtenderCarrier{
		ctx:          cancelCtx,
		cancel:       cancel,
		log:          loggerOrDefault(settings.Log),
		settings:     settings,
		resolver:     resolver,
		pendingDials: map[*webRtcExtenderPendingDial]bool{},
		closeDone:    make(chan struct{}),
	}
}

// Close ends every wait of the carrier and releases its factory. Streams
// already handed out keep their peer connections until they are closed.
func (self *WebRtcExtenderCarrier) Close() {
	var factory *webRtcPeerConnectionFactory
	var pendingDials []*webRtcExtenderPendingDial
	closing := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return
		}
		self.closed = true
		closing = true
		factory = self.factory
		self.factory = nil
		for pendingDial := range self.pendingDials {
			pendingDials = append(pendingDials, pendingDial)
		}
	}()
	if !closing {
		<-self.closeDone
		return
	}
	self.cancel()
	for _, pendingDial := range pendingDials {
		pendingDial.cancel()
	}
	for _, pendingDial := range pendingDials {
		<-pendingDial.done
	}
	if factory != nil && factory.close != nil {
		_ = factory.close()
	}
	close(self.closeDone)
}

// Joins caller and carrier cancellation without transferring either to a
// successfully returned stream. The returned cleanup also joins the callback.
func (self *WebRtcExtenderCarrier) beginDial(ctx context.Context) (context.Context, func(), error) {
	dialCtx, cancel := context.WithCancel(ctx)
	pendingDial := &webRtcExtenderPendingDial{cancel: cancel, done: make(chan struct{})}
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.pendingDials[pendingDial] = true
		return true
	}()
	if !admitted {
		cancel()
		return nil, nil, ErrWebRtcExtenderCarrierUnavailable
	}
	carrierCanceled := make(chan struct{})
	stopCarrierCancel := context.AfterFunc(self.ctx, func() {
		cancel()
		close(carrierCanceled)
	})
	return dialCtx, func() {
		cancel()
		if !stopCarrierCancel() {
			<-carrierCanceled
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.pendingDials, pendingDial)
		close(pendingDial.done)
	}, nil
}

// The shared factory, built once. An error is retried on the next session
// rather than kept, since the usual cause (no usable interface) is transient.
func (self *WebRtcExtenderCarrier) peerConnectionFactory() (*webRtcPeerConnectionFactory, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, ErrWebRtcExtenderCarrierUnavailable
	}
	if self.factory != nil {
		return self.factory, nil
	}
	factory, _, err := newWebRtcPeerConnectionFactory(self.settings, nil)
	if err != nil {
		self.factoryErr = err
		return nil, err
	}
	self.factory = factory
	self.factoryErr = nil
	return factory, nil
}

func (self *WebRtcExtenderCarrier) openTimeout() time.Duration {
	if 0 < self.settings.ExtenderCarrierOpenTimeout {
		return self.settings.ExtenderCarrierOpenTimeout
	}
	return defaultWebRtcExtenderCarrierOpenTimeout
}

// dialExtenderWebRtc is the dialExtenderStream branch of the webrtc carrier.
func dialExtenderWebRtc(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	carrier := connectSettings.WebRtcExtenderCarrier
	if carrier == nil {
		return nil, nil, ErrWebRtcExtenderCarrierUnavailable
	}
	return carrier.Dial(ctx, connectSettings, extenderConfig, extenderDial)
}

// Dial opens the carrier to the extender the config names, resolving its
// signaling by the config's identity key, and runs the A3 request over it.
func (self *WebRtcExtenderCarrier) Dial(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if self.resolver == nil {
		return nil, nil, ErrWebRtcExtenderNoSignaling
	}
	exchanger, ok := self.resolver(extenderConfig.PublicKey)
	if !ok || exchanger == nil {
		return nil, nil, ErrWebRtcExtenderNoSignaling
	}
	return self.DialWithExchanger(ctx, connectSettings, exchanger, extenderConfig, extenderDial)
}

// DialWithExchanger opens the carrier through the given signaling and runs
// the A3 request over it. When the config names the extender's key the dial
// challenges and verifies the identity itself (there is no outer leaf), using
// the caller's challenge when it brought one. On success the returned stream
// is positioned at the first byte after the response and owns the peer
// connection; the caller closes it. On any error nothing is returned open.
func (self *WebRtcExtenderCarrier) DialWithExchanger(
	ctx context.Context,
	connectSettings *ConnectSettings,
	exchanger WebRtcExtenderOfferExchanger,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if exchanger == nil {
		return nil, nil, ErrWebRtcExtenderNoSignaling
	}
	ctx, finishDial, err := self.beginDial(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer finishDial()
	dial := ExtenderDial{}
	if extenderDial != nil {
		dial = *extenderDial
	}
	expectPublicKey := extenderConfig.PublicKey
	if 0 < len(expectPublicKey) && len(dial.Challenge) == 0 {
		challenge, err := NewExtenderChallenge()
		if err != nil {
			return nil, nil, err
		}
		dial.Challenge = challenge
	}
	headerBytes, err := extenderRequestHeaderBytes(extenderConfig, &dial)
	if err != nil {
		return nil, nil, err
	}

	factory, err := self.peerConnectionFactory()
	if err != nil {
		return nil, nil, err
	}
	peerConnection, cancelResolve, err := factory.newPeerConnection(false)
	if err != nil {
		return nil, nil, err
	}
	// until a stream owns it, this call owns the peer connection
	owned := false
	defer func() {
		if !owned {
			_ = peerConnection.Close()
			cancelResolve()
		}
	}()

	openCh, failCh := watchWebRtcExtenderPeerConnection(peerConnection)
	ordered := true
	dataChannel, err := peerConnection.CreateDataChannel(
		webRtcExtenderDataChannelLabel,
		&webrtc.DataChannelInit{Ordered: &ordered},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create extender data channel: %w", err)
	}
	dataChannel.OnOpen(func() {
		offerWebRtcExtenderDataChannel(dataChannel, openCh, failCh)
	})

	if err := self.negotiateOffer(ctx, peerConnection, exchanger); err != nil {
		return nil, nil, err
	}

	var detached datachannel.ReadWriteCloserDeadliner
	select {
	case detached = <-openCh:
	case err := <-failCh:
		return nil, nil, err
	case <-time.After(self.openTimeout()):
		return nil, nil, fmt.Errorf("extender data channel did not open within %s", self.openTimeout())
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-self.ctx.Done():
		return nil, nil, ErrWebRtcExtenderCarrierUnavailable
	}
	conn := newWebRtcDataChannelConn(detached, peerConnection, cancelResolve, int(self.settings.MaxMessageSize))
	owned = true

	streamConn, response, err := extenderStreamRequest(
		ctx,
		connectSettings,
		extenderConfig,
		conn,
		headerBytes,
		dial.RoundTrip,
	)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if 0 < len(expectPublicKey) {
		if response == nil || !bytes.Equal(response.PublicKey, expectPublicKey) {
			_ = streamConn.Close()
			return nil, nil, fmt.Errorf("extender published another identity key")
		}
		if !VerifyExtenderChallenge(expectPublicKey, dial.Challenge, response.ChallengeSignature) {
			_ = streamConn.Close()
			return nil, nil, fmt.Errorf("extender challenge signature does not verify")
		}
	}
	return streamConn, response, nil
}

// negotiateOffer creates the offer, gathers its candidates, exchanges it for
// the extender's answer and applies the answer. Gathering and signaling share
// the carrier's open bound, even when the caller has no deadline.
func (self *WebRtcExtenderCarrier) negotiateOffer(
	ctx context.Context,
	peerConnection *webrtc.PeerConnection,
	exchanger WebRtcExtenderOfferExchanger,
) error {
	ctx, cancel := context.WithTimeout(ctx, self.openTimeout())
	defer cancel()
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create extender offer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set extender offer: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return ctx.Err()
	case <-self.ctx.Done():
		return ErrWebRtcExtenderCarrierUnavailable
	}
	answer, err := exchanger.ExchangeOffer(ctx, *peerConnection.LocalDescription())
	if err != nil {
		return fmt.Errorf("exchange extender offer: %w", err)
	}
	if err := peerConnection.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set extender answer: %w", err)
	}
	return nil
}

// Answerer binds the carrier to the stream handler of an extender, which the
// extender role installs on its manager (WebRtcManager.SetExtenderCarrierAnswerer).
func (self *WebRtcExtenderCarrier) Answerer(handler WebRtcExtenderStreamHandler) WebRtcExtenderOfferAnswerer {
	return &webRtcExtenderCarrierAnswerer{
		carrier: self,
		handler: handler,
	}
}

type webRtcExtenderCarrierAnswerer struct {
	carrier *WebRtcExtenderCarrier
	handler WebRtcExtenderStreamHandler
}

func (self *webRtcExtenderCarrierAnswerer) AnswerWebRtcExtenderOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	return self.carrier.Answer(ctx, offer, self.handler)
}

// Answer answers one dialer offer on the extender: it builds the answer peer
// connection, returns the SDP answer for the signaling to send back, with the
// extender's gathered candidates, and serves the stream on its own goroutine
// once the data channel opens, handing it to handler. The peer connection
// lives until the handler returns, the channel fails to open within the open
// timeout, or ctx ends.
func (self *WebRtcExtenderCarrier) Answer(
	ctx context.Context,
	offer webrtc.SessionDescription,
	handler WebRtcExtenderStreamHandler,
) (webrtc.SessionDescription, error) {
	if handler == nil {
		return webrtc.SessionDescription{}, fmt.Errorf("the webrtc extender carrier has no stream handler")
	}
	factory, err := self.peerConnectionFactory()
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	peerConnection, cancelResolve, err := factory.newPeerConnection(false)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	started := false
	defer func() {
		if !started {
			_ = peerConnection.Close()
			cancelResolve()
		}
	}()

	openCh, failCh := watchWebRtcExtenderPeerConnection(peerConnection)
	peerConnection.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		if dataChannel.Label() != webRtcExtenderDataChannelLabel {
			// installing a handler replaces pion's default one: close what the
			// carrier does not consume. The channel has no stream yet when
			// this runs, and a close before it opens reaches nothing, so the
			// close waits for the open and resets the stream the peer sees.
			dataChannel.OnOpen(func() {
				_ = dataChannel.Close()
			})
			return
		}
		dataChannel.OnOpen(func() {
			offerWebRtcExtenderDataChannel(dataChannel, openCh, failCh)
		})
	})

	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set extender offer: %w", err)
	}
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create extender answer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err := peerConnection.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set extender answer: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	case <-self.ctx.Done():
		return webrtc.SessionDescription{}, ErrWebRtcExtenderCarrierUnavailable
	}

	started = true
	go func() {
		// the stream owns the peer connection once it exists; until then,
		// and when the channel never opens, this goroutine releases it
		released := false
		defer func() {
			if !released {
				_ = peerConnection.Close()
				cancelResolve()
				if self.answerReleasedForTest != nil {
					self.answerReleasedForTest()
				}
			}
		}()
		select {
		case detached := <-openCh:
			conn := newWebRtcDataChannelConn(detached, peerConnection, cancelResolve, int(self.settings.MaxMessageSize))
			released = true
			defer conn.Close()
			handler.HandleWebRtcExtenderStream(ctx, conn)
		case <-failCh:
		case <-time.After(self.openTimeout()):
		case <-ctx.Done():
		case <-self.ctx.Done():
		}
	}()
	return *peerConnection.LocalDescription(), nil
}

// watchWebRtcExtenderPeerConnection reports a peer connection that failed or
// closed before its data channel opened, so a wait for the channel ends on
// the failure rather than on the open timeout.
func watchWebRtcExtenderPeerConnection(
	peerConnection *webrtc.PeerConnection,
) (chan datachannel.ReadWriteCloserDeadliner, chan error) {
	openCh := make(chan datachannel.ReadWriteCloserDeadliner, 1)
	failCh := make(chan error, 1)
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			select {
			case failCh <- fmt.Errorf("extender peer connection %s", state):
			default:
			}
		}
	})
	return openCh, failCh
}

// offerWebRtcExtenderDataChannel detaches an open carrier channel and hands it
// to the one waiter; a second open on the same connection is closed, since a
// stream carries exactly one channel.
func offerWebRtcExtenderDataChannel(
	dataChannel *webrtc.DataChannel,
	openCh chan datachannel.ReadWriteCloserDeadliner,
	failCh chan error,
) {
	detached, err := detachWithDeadline(dataChannel)
	if err != nil {
		select {
		case failCh <- fmt.Errorf("detach extender data channel: %w", err):
		default:
		}
		return
	}
	select {
	case openCh <- detached:
	default:
		_ = detached.Close()
	}
}
