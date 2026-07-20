package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"time"
	// "fmt"
)

// Assumptions about our peer-to-peer connections:
// - a limited transmit buffer that uses semi-reliable delivery as flow control.
//   While the transfer client is the ultimate source of reliable delivery,
//   we require the p2p connection use semi-reliable delivery to back pressure the transfer rate,
//   which propagates through the entire multi-hop stream.
//   Without flow control we would have more mismatches in transfer rate
//   and retransmits from the transfer clients.
// - disconnect detection. Both peers should be aware when either side disconnects.
//   This is typically manifested in clean disconnect messages and heartbeat timeouts.
// - directed initializaton. One side of the connection will offer to connect
//   and the other side will respond. We assume this in our architecture. However,
//   directed is usually a superset of undirected, so this does not prevent an undirected
//   initializtion either.

// important - changing this will break compatibility with older clients
const ReadyHeader = "rdy"

func DefaultP2pTransportSettings() *P2pTransportSettings {
	return &P2pTransportSettings{
		WriteTimeout:        15 * time.Second,
		ReadTimeout:         15 * time.Second,
		ConnectTimeout:      15 * time.Second,
		ReconnectTimeout:    5 * time.Second,
		ChannelBufferSize:   32,
		MaxMessageByteCount: 64 * 1024,
	}
}

type PeerType = string

const (
	// the peer who initiates the transfer
	PeerTypeSource PeerType = "source"
	// the peer who is the destination of the transfer
	PeerTypeDestination PeerType = "destination"
)

type P2pTransportSettings struct {
	WriteTimeout      time.Duration
	ReadTimeout       time.Duration
	ConnectTimeout    time.Duration
	ReconnectTimeout  time.Duration
	ChannelBufferSize int
	// MaxMessageByteCount is the largest single message the transport reads or
	// writes. The detached WebRTC data channel is message-oriented: one pion
	// Read returns exactly one whole SCTP user message, and pion/sctp returns
	// io.ErrShortBuffer (leaving the message queued) when the read buffer is
	// smaller than the message. The on-wire framing is therefore the SCTP
	// message boundary itself — no length prefix — and the receive buffer must
	// be >= the largest TransferFrame that can arrive.
	MaxMessageByteCount int
}

type P2pTransport struct {
	ctx    context.Context
	cancel context.CancelFunc

	client *Client

	webRtcManager *WebRtcManager

	sendRouteManager    *RouteManager
	receiveRouteManager *RouteManager

	peerId   Id
	streamId Id
	peerType PeerType

	settings *P2pTransportSettings
}

func NewP2pTransport(
	ctx context.Context,
	client *Client,
	webRtcManager *WebRtcManager,
	sendRouteManager *RouteManager,
	receiveRouteManager *RouteManager,
	peerId Id,
	streamId Id,
	// this is the peer type of `peerId`. The current client is the complement.
	peerType PeerType,
	settings *P2pTransportSettings,
) *P2pTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	p2pTransport := &P2pTransport{
		ctx:                 cancelCtx,
		cancel:              cancel,
		client:              client,
		webRtcManager:       webRtcManager,
		sendRouteManager:    sendRouteManager,
		receiveRouteManager: receiveRouteManager,
		peerId:              peerId,
		streamId:            streamId,
		peerType:            peerType,
		settings:            settings,
	}
	go HandleError(p2pTransport.run, cancel)
	return p2pTransport
}

func (self *P2pTransport) run() {
	defer self.cancel()

	for {
		// TODO using net.Conn as a stand in for the actual interface

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		var conn WebRtcConn
		var err error
		// note, one side of the P2P connection will be driving the setup process (active).
		// We arbitrarily choose the sender (peer is destination) as active.
		switch self.peerType {
		case PeerTypeDestination:
			conn, err = self.webRtcManager.NewP2pConnActive(self.ctx, NewTransferPath(self.client.ClientId(), self.peerId, self.streamId))
		case PeerTypeSource:
			conn, err = self.webRtcManager.NewP2pConnPassive(self.ctx, NewTransferPath(self.client.ClientId(), self.peerId, self.streamId))
		default:
			// unknown peer type
			return
		}
		if err != nil {
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
			}
			continue
		}

		// capture the immediate-reconnect signal BEFORE c() runs so it
		// observes any NotifyAll fired during c(); the underlying Monitor
		// returns a fresh (non-firing) channel after NotifyAll, so a late
		// capture would miss the signal.
		immediateReconnect := conn.ImmediateReconnect()

		// at this point, the connection should be able to ping the other side
		// now we wait for the entire stream to be ready by propagating the `ReaderHeader`
		c := func() {
			defer conn.Close()

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			// the peer's ready header must be consumed before the receive
			// transport starts reading. otherwise the two concurrent readers
			// race for the header, and the header reader can instead receive
			// a later (larger) frame, which fails the read with a short buffer
			// and tears down the conn.
			headerRead := make(chan struct{})

			go HandleError(func() {
				defer handleCancel()

				conn.SetWriteDeadline(time.Now().Add(self.settings.ConnectTimeout))
				_, err := conn.Write([]byte(ReadyHeader))
				if err != nil {
					self.client.log.V(1).Infof("[p2p]s(%s) ready header write err = %s\n", self.streamId, err)
					return
				}

				select {
				case <-handleCtx.Done():
					return
				case <-headerRead:
				}

				t, route := NewP2pReceiveTransport(handleCtx, handleCancel, conn, self.streamId, self.settings)

				updateRoute := func(connected bool) {
					if connected {
						self.receiveRouteManager.UpdateTransport(t, []Route{route})
					} else {
						self.receiveRouteManager.RemoveTransport(t)
					}
				}
				unsub := conn.AddConnectedCallback(updateRoute)
				defer unsub()
				updateRoute(conn.Connected())
				defer updateRoute(false)

				select {
				case <-handleCtx.Done():
					return
				}
			}, handleCancel)

			go HandleError(func() {
				defer handleCancel()

				select {
				case <-handleCtx.Done():
					return
				default:
				}

				// the detached data channel is message-oriented, and the sctp
				// read fails with a short buffer when the read buffer is smaller
				// than the message. a dcep ack can arrive before the peer's
				// ready header, and the datachannel filters dcep internally only
				// when the read buffer can hold the message. read one whole
				// message with a max size buffer and require it to be exactly
				// the ready header.
				headerBuf := make([]byte, self.settings.MaxMessageByteCount)
				conn.SetReadDeadline(time.Now().Add(self.settings.ConnectTimeout))
				n, err := conn.Read(headerBuf)
				if err != nil {
					self.client.log.V(1).Infof("[p2p]s(%s) ready header read err = %s\n", self.streamId, err)
					return
				}
				if !slices.Equal(headerBuf[:n], []byte(ReadyHeader)) {
					self.client.log.V(1).Infof("[p2p]s(%s) ready header mismatch = %x\n", self.streamId, headerBuf[:n])
					return
				}
				close(headerRead)

				t, route := NewP2pSendTransportForPeer(handleCtx, handleCancel, conn, self.peerId, self.streamId, self.settings)

				updateRoute := func(connected bool) {
					if connected {
						self.sendRouteManager.UpdateTransport(t, []Route{route})
					} else {
						self.sendRouteManager.RemoveTransport(t)
					}
				}
				unsub := conn.AddConnectedCallback(updateRoute)
				defer unsub()
				updateRoute(conn.Connected())
				defer updateRoute(false)

				select {
				case <-handleCtx.Done():
					return
				}
			}, handleCancel)

			select {
			case <-handleCtx.Done():
				return
			}
		}

		c()
		select {
		case <-self.ctx.Done():
			return
		case <-immediateReconnect:
			// peer requested fresh negotiation; skip the backoff delay
		case <-reconnect.After():
		}
	}
}

type P2pSendTransport struct {
	transportId Id

	ctx      context.Context
	cancel   context.CancelFunc
	conn     net.Conn
	peerId   Id
	streamId Id
	send     chan []byte

	settings *P2pTransportSettings
}

func NewP2pSendTransport(
	ctx context.Context,
	cancel context.CancelFunc,
	conn net.Conn,
	streamId Id,
	settings *P2pTransportSettings,
) (Transport, Route) {
	return NewP2pSendTransportForPeer(ctx, cancel, conn, Id{}, streamId, settings)
}

// NewP2pSendTransportForPeer creates a P2P route that can be selected by both
// peer and stream. NewP2pSendTransport retains the pre-peer-id signature as a
// deprecated stream-only compatibility entry point.
func NewP2pSendTransportForPeer(
	ctx context.Context,
	cancel context.CancelFunc,
	conn net.Conn,
	peerId Id,
	streamId Id,
	settings *P2pTransportSettings,
) (Transport, Route) {
	send := make(chan []byte, settings.ChannelBufferSize)
	p2pSendTransport := &P2pSendTransport{
		transportId: NewId(),
		ctx:         ctx,
		cancel:      cancel,
		conn:        conn,
		peerId:      peerId,
		streamId:    streamId,
		send:        send,
		settings:    settings,
	}
	go HandleError(p2pSendTransport.run, cancel)
	return p2pSendTransport, send
}

func (self *P2pSendTransport) run() {
	defer self.cancel()
	// drain any pooled bytes the route manager already enqueued; the route
	// manager removes the transport asynchronously, so a brief window remains
	// where it may have written and the writer never consumed.
	defer func() {
		for {
			select {
			case b, ok := <-self.send:
				if !ok {
					return
				}
				MessagePoolReturn(b)
			default:
				return
			}
		}
	}()

	for {
		select {
		case <-self.ctx.Done():
			return
		case transferFrameBytes, ok := <-self.send:
			if !ok {
				return
			}

			// The detached WebRTC data channel is message-oriented: one Write
			// becomes one whole SCTP user message the peer reads back whole, so
			// the SCTP message boundary frames each TransferFrame natively — no
			// length prefix. Enforce the max message size up front.
			if len(transferFrameBytes) > self.settings.MaxMessageByteCount {
				DefaultLogger().V(1).Infof("[p2p]s(%s) send message too large = %d\n", self.streamId, len(transferFrameBytes))
				MessagePoolReturn(transferFrameBytes)
				return
			}
			self.conn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
			nw, err := self.conn.Write(transferFrameBytes)
			MessagePoolReturn(transferFrameBytes)
			if nw < len(transferFrameBytes) && err == nil {
				err = io.ErrShortWrite
			}
			if err != nil {
				DefaultLogger().V(1).Infof("[p2p]s(%s) send write err = %s\n", self.streamId, err)
				return
			}
		}
	}
}

func (self *P2pSendTransport) TransportId() Id {
	return self.transportId
}

// lower priority takes precedence
func (self *P2pSendTransport) Priority() int {
	// p2p routes have highest priority
	return 0
}

func (self *P2pSendTransport) Weight() float32 {
	// p2p routes have highest weight
	return 1.0
}

func (self *P2pSendTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

func (self *P2pSendTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// p2p routes have highest weight
	return 1.0
}

func (self *P2pSendTransport) MatchesSend(destination TransferPath) bool {
	if destination.StreamId == self.streamId {
		return true
	}
	// the stream terminates at the peer,
	// so any destination addressed to the peer matches the stream transport.
	// the peer id must be non-zero so that a missing peer never matches
	// destination masks without a destination id (e.g. control or pure stream masks)
	return self.peerId != (Id{}) && destination.DestinationId == self.peerId
}

func (self *P2pSendTransport) MatchesReceive(destination TransferPath) bool {
	return false
}

func (self *P2pSendTransport) Downgrade(source TransferPath) {
	if source.StreamId == self.streamId {
		self.cancel()
		return
	}
	// mirror `MatchesSend`: the stream terminates at the peer, so an
	// audit/degrade signal for the peer must also shed this transport,
	// not just signals for the stream. The peer id must be non-zero so
	// that a missing peer never matches paths without a destination id.
	if self.peerId != (Id{}) && source.DestinationId == self.peerId {
		self.cancel()
	}
}

type P2pReceiveTransport struct {
	transportId Id

	ctx      context.Context
	cancel   context.CancelFunc
	conn     net.Conn
	streamId Id
	receive  chan []byte

	settings *P2pTransportSettings
}

func NewP2pReceiveTransport(
	ctx context.Context,
	cancel context.CancelFunc,
	conn net.Conn,
	streamId Id,
	settings *P2pTransportSettings,
) (Transport, Route) {
	receive := make(chan []byte, settings.ChannelBufferSize)
	p2pReceiveTransport := &P2pReceiveTransport{
		transportId: NewId(),
		ctx:         ctx,
		cancel:      cancel,
		conn:        conn,
		streamId:    streamId,
		receive:     receive,
		settings:    settings,
	}
	go HandleError(p2pReceiveTransport.run, cancel)
	return p2pReceiveTransport, receive
}

func (self *P2pReceiveTransport) run() {
	defer self.cancel()
	// drain any pooled bytes we wrote that the route manager hasn't consumed
	// yet at shutdown.
	defer func() {
		for {
			select {
			case b, ok := <-self.receive:
				if !ok {
					return
				}
				MessagePoolReturn(b)
			default:
				return
			}
		}
	}()

	// The detached WebRTC data channel is message-oriented: one Read returns one
	// whole SCTP user message (io.ErrShortBuffer if the buffer is too small for
	// it). Read each whole message into a single reused buffer, then copy the
	// exact bytes into a right-sized pooled buffer for the receive queue. This
	// keeps one max-message-sized allocation for the life of the transport
	// rather than taking — and, above a pool size class, un-pooling — a
	// max-message-sized buffer from the message pool on every read.
	readBuf := make([]byte, self.settings.MaxMessageByteCount)

	for {
		self.conn.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
		n, err := self.conn.Read(readBuf)
		if n > 0 {
			transferFrameBytes := MessagePoolCopy(readBuf[:n])
			select {
			case <-self.ctx.Done():
				MessagePoolReturn(transferFrameBytes)
				return
			case self.receive <- transferFrameBytes:
			}
		}
		if err != nil {
			// an idle read timeout is not a dead conn: conn liveness is handled
			// by the webrtc layer (ice keepalive and disconnect detection),
			// which removes the routes via the connected callback.
			// re-arm the read instead of tearing down an idle healthy conn.
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			DefaultLogger().V(1).Infof("[p2p]s(%s) receive read err = %s\n", self.streamId, err)
			return
		}
	}
}

func (self *P2pReceiveTransport) TransportId() Id {
	return self.transportId
}

// lower priority takes precedence
func (self *P2pReceiveTransport) Priority() int {
	// p2p routes have highest priority
	return 0
}

func (self *P2pReceiveTransport) Weight() float32 {
	// p2p routes have highest weight
	return 1.0
}

func (self *P2pReceiveTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

func (self *P2pReceiveTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// p2p routes have highest weight
	return 1.0
}

func (self *P2pReceiveTransport) MatchesSend(destination TransferPath) bool {
	return false
}

func (self *P2pReceiveTransport) MatchesReceive(destination TransferPath) bool {
	return true
}

func (self *P2pReceiveTransport) Downgrade(source TransferPath) {
	if source.StreamId == self.streamId {
		self.cancel()
	}
}
