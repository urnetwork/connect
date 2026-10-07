//go:build !js

package connect

// The peer-to-peer webrtc extender carrier (EXTENDER.md, webrtc extender
// section; acceptance C2a). A home extender behind NAT has no public inbound
// address, so it cannot serve the tcp/udp/dns carriers. Instead the dialer (a
// client, or the operator's activation probe) reaches it over a webrtc data
// channel: rendezvous through the exchange signaling, NAT traversal through
// ICE/STUN. The data channel is one reliable byte stream, exactly what the
// other carriers yield, and the same extender request/response runs over it.
//
// This carrier is not an ip:port dial, so it is deliberately NOT part of
// dialExtenderStream; it is a parallel path driven by signaling. The peer
// connections come from the shared factory, so they inherit the per-session
// STUN pool (phase 1) and, when enabled, the browser DTLS ClientHello mimicry
// (phase 2) — the extender is the SDP answerer, so it is the side that sends
// the DTLS ClientHello that mimicry shapes.
//
// Framing on the data channel mirrors the other carriers' self-delimiting
// frames (A3): a 4-byte big-endian length and the serialized ExtenderHeader
// from the dialer, then the serialized ExtenderResponse frame
// (ExtenderResponseFrame) from the extender, after which both ends use the
// stream raw. There is no outer TLS to terminate here: the data channel is
// already authenticated DTLS, and the extender identity is proven by the
// challenge signature in the response (C2a), not by an outer leaf.
//
// Not built for js/wasm: the browser SDK reaches webrtc through the browser,
// not pion's detached data channels.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// The data channel label of the webrtc extender carrier. A peer connection
// opened for this carrier uses exactly this label; any other data channel on
// the connection is refused.
const webRtcExtenderDataChannelLabel = "ur-extender"

// WebRtcExtenderOfferExchanger carries one dialer SDP offer to the extender and
// returns its SDP answer. Production wires this to the exchange signaling that
// transport_p2p_webrtc.go already carries (SendSignal of an SdpOffer, awaiting
// the SdpAnswer); tests inject an in-memory bridge. The offer already carries
// its gathered ICE candidates (no trickle), so one round trip is enough.
type WebRtcExtenderOfferExchanger interface {
	ExchangeOffer(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error)
}

// WebRtcExtenderCarrierHandler services one accepted carrier stream on the
// extender. It is given the dialer's parsed header and the data-channel
// connection positioned at the first byte after the header. The handler owns
// conn: it writes the ExtenderResponse frame (ExtenderResponseFrame, with the
// challenge signature) and then forwards or serves the stream, and it closes
// conn when done. Policy (whitelist, services, reverse proxy) lives with the
// extender server, not in connect, so it is injected here.
type WebRtcExtenderCarrierHandler interface {
	HandleExtenderStream(ctx context.Context, header *protocol.ExtenderHeader, conn net.Conn)
}

// webRtcDataChannelConn adapts a detached pion data channel to net.Conn so the
// carrier's byte stream can carry the ordinary extender request/response and
// forward. Closing it tears down the owning peer connection and releases the
// factory's per-generation address resolution.
type webRtcDataChannelConn struct {
	datachannel.ReadWriteCloserDeadliner
	peerConnection *webrtc.PeerConnection
	cancel         context.CancelFunc
}

func (self *webRtcDataChannelConn) LocalAddr() net.Addr  { return webRtcExtenderAddr{} }
func (self *webRtcDataChannelConn) RemoteAddr() net.Addr { return webRtcExtenderAddr{} }

func (self *webRtcDataChannelConn) SetDeadline(t time.Time) error {
	if err := self.SetReadDeadline(t); err != nil {
		return err
	}
	return self.SetWriteDeadline(t)
}

func (self *webRtcDataChannelConn) Close() error {
	err := self.ReadWriteCloserDeadliner.Close()
	if closeErr := self.peerConnection.Close(); err == nil {
		err = closeErr
	}
	self.cancel()
	return err
}

// webRtcExtenderAddr is the placeholder address of a data-channel conn: the
// carrier has no ip:port, and nothing on the extender path reads it.
type webRtcExtenderAddr struct{}

func (webRtcExtenderAddr) Network() string { return ExtenderCarrierWebRtc }
func (webRtcExtenderAddr) String() string  { return ExtenderCarrierWebRtc }

// DialWebRtcExtenderCarrier opens the carrier to an extender and runs the A3
// request/response over it. It creates the offer peer connection from the
// shared factory, gathers candidates, exchanges the offer for the extender's
// answer through the injected signaling, waits for the data channel to open,
// then writes the request header and reads the response. On success the
// returned conn is positioned at the first byte after the response and owns the
// peer connection; the caller closes it. On any error nothing is returned open.
func DialWebRtcExtenderCarrier(
	ctx context.Context,
	settings *WebRtcSettings,
	exchanger WebRtcExtenderOfferExchanger,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	headerBytes, err := extenderRequestHeaderBytes(&ExtenderConfig{}, extenderDial)
	if err != nil {
		return nil, nil, err
	}

	peerConnection, cancel, err := newWebRtcExtenderPeerConnection(settings)
	if err != nil {
		return nil, nil, err
	}
	// Until ownership transfers to a returned conn, this function owns teardown.
	connected := false
	defer func() {
		if !connected {
			_ = peerConnection.Close()
			cancel()
		}
	}()

	ordered := true
	dataChannel, err := peerConnection.CreateDataChannel(
		webRtcExtenderDataChannelLabel,
		&webrtc.DataChannelInit{Ordered: &ordered},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create extender data channel: %w", err)
	}
	connCh := make(chan *webRtcDataChannelConn, 1)
	errCh := make(chan error, 1)
	dataChannel.OnOpen(func() {
		detached, detachErr := dataChannel.DetachWithDeadline()
		if detachErr != nil {
			select {
			case errCh <- fmt.Errorf("detach extender data channel: %w", detachErr):
			default:
			}
			return
		}
		select {
		case connCh <- &webRtcDataChannelConn{
			ReadWriteCloserDeadliner: detached,
			peerConnection:           peerConnection,
			cancel:                   cancel,
		}:
		default:
		}
	})

	answer, err := negotiateWebRtcExtenderOffer(ctx, peerConnection, exchanger)
	if err != nil {
		return nil, nil, err
	}
	_ = answer

	var conn *webRtcDataChannelConn
	select {
	case conn = <-connCh:
	case err = <-errCh:
		return nil, nil, err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	if err := writeExtenderRequestFrame(ctx, conn, headerBytes); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	response, err := readExtenderResponseFrameWithDeadline(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	connected = true
	return conn, response, nil
}

// AnswerWebRtcExtenderCarrierOffer answers one dialer offer on the extender: it
// builds the answer peer connection from the shared factory, returns the SDP
// answer for the signaling to send back, and asynchronously waits for the data
// channel to open, reads the request header, and hands the stream to handler.
// The returned answer carries the extender's gathered candidates. The peer
// connection lives until the handler returns or ctx is cancelled.
func AnswerWebRtcExtenderCarrierOffer(
	ctx context.Context,
	settings *WebRtcSettings,
	offer webrtc.SessionDescription,
	handler WebRtcExtenderCarrierHandler,
) (webrtc.SessionDescription, error) {
	peerConnection, cancel, err := newWebRtcExtenderPeerConnection(settings)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	started := false
	defer func() {
		if !started {
			_ = peerConnection.Close()
			cancel()
		}
	}()

	connCh := make(chan *webRtcDataChannelConn, 1)
	peerConnection.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		if dataChannel.Label() != webRtcExtenderDataChannelLabel {
			_ = dataChannel.Close()
			return
		}
		dataChannel.OnOpen(func() {
			detached, detachErr := dataChannel.DetachWithDeadline()
			if detachErr != nil {
				return
			}
			select {
			case connCh <- &webRtcDataChannelConn{
				ReadWriteCloserDeadliner: detached,
				peerConnection:           peerConnection,
				cancel:                   cancel,
			}:
			default:
			}
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
	}

	started = true
	go func() {
		defer func() {
			_ = peerConnection.Close()
			cancel()
		}()
		var conn *webRtcDataChannelConn
		select {
		case conn = <-connCh:
		case <-ctx.Done():
			return
		}
		header, err := readExtenderRequestFrameWithDeadline(ctx, conn)
		if err != nil {
			_ = conn.Close()
			return
		}
		// handler owns conn from here: it writes the response frame and
		// forwards, and closes conn when done.
		handler.HandleExtenderStream(ctx, header, conn)
	}()
	return *peerConnection.LocalDescription(), nil
}

// newWebRtcExtenderPeerConnection builds one carrier peer connection from the
// shared factory, so the carrier inherits the per-session STUN pool and the
// DTLS mimicry setting. A nil settings uses DefaultWebRtcSettings.
func newWebRtcExtenderPeerConnection(
	settings *WebRtcSettings,
) (*webrtc.PeerConnection, context.CancelFunc, error) {
	if settings == nil {
		settings = DefaultWebRtcSettings()
	}
	factory, _, err := newWebRtcPeerConnectionFactory(settings, nil)
	if err != nil {
		return nil, nil, err
	}
	peerConnection, cancel, err := factory.newPeerConnection(false)
	if err != nil {
		if factory.close != nil {
			_ = factory.close()
		}
		return nil, nil, err
	}
	return peerConnection, cancel, nil
}

// negotiateWebRtcExtenderOffer creates the offer, gathers candidates, exchanges
// it for the extender's answer through the signaling, and applies the answer.
func negotiateWebRtcExtenderOffer(
	ctx context.Context,
	peerConnection *webrtc.PeerConnection,
	exchanger WebRtcExtenderOfferExchanger,
) (webrtc.SessionDescription, error) {
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create extender offer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set extender offer: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	}
	answer, err := exchanger.ExchangeOffer(ctx, *peerConnection.LocalDescription())
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("exchange extender offer: %w", err)
	}
	if err := peerConnection.SetRemoteDescription(answer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set extender answer: %w", err)
	}
	return answer, nil
}

// writeExtenderRequestFrame writes the self-delimiting request header frame: a
// 4-byte big-endian length and the serialized header, mirroring the response
// frame. The write phase is bounded by ctx.
func writeExtenderRequestFrame(ctx context.Context, conn net.Conn, headerBytes []byte) error {
	if ExtenderMaxHeaderByteCount < len(headerBytes) {
		return fmt.Errorf("extender header is %d bytes, at most %d", len(headerBytes), ExtenderMaxHeaderByteCount)
	}
	frameBytes := make([]byte, 4+len(headerBytes))
	binary.BigEndian.PutUint32(frameBytes[0:4], uint32(len(headerBytes)))
	copy(frameBytes[4:], headerBytes)
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer conn.SetWriteDeadline(time.Time{})
	}
	_, err := conn.Write(frameBytes)
	return err
}

// readExtenderRequestFrameWithDeadline reads exactly one request header frame,
// bounded by ctx, leaving the reader on the first byte after it.
func readExtenderRequestFrameWithDeadline(ctx context.Context, conn net.Conn) (*protocol.ExtenderHeader, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		defer conn.SetReadDeadline(time.Time{})
	}
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(conn, lengthBytes); err != nil {
		return nil, err
	}
	headerByteCount := int(binary.BigEndian.Uint32(lengthBytes))
	if ExtenderMaxHeaderByteCount < headerByteCount {
		return nil, fmt.Errorf("extender header is %d bytes, at most %d", headerByteCount, ExtenderMaxHeaderByteCount)
	}
	headerBytes := make([]byte, headerByteCount)
	if _, err := io.ReadFull(conn, headerBytes); err != nil {
		return nil, err
	}
	header := &protocol.ExtenderHeader{}
	if err := ProtoUnmarshal(headerBytes, header); err != nil {
		return nil, err
	}
	return header, nil
}

// readExtenderResponseFrameWithDeadline is ReadExtenderResponseFrame bounded by
// ctx on the carrier conn.
func readExtenderResponseFrameWithDeadline(ctx context.Context, conn net.Conn) (*protocol.ExtenderResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		defer conn.SetReadDeadline(time.Time{})
	}
	return ReadExtenderResponseFrame(conn)
}
