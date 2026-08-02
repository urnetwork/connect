package connect

import (
	"context"
	"errors"
	"fmt"
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

// readP2pMessage reads one complete message from a message-oriented conn into
// an owned pool buffer. Pion/SCTP itself reports the required size in n with
// io.ErrShortBuffer, but the detached datachannel wrapper intentionally masks
// that value and returns n=0. Grow geometrically in that case; compatibility
// conns that preserve the size still jump directly to it. The queued SCTP
// message is not consumed on io.ErrShortBuffer.
func readP2pMessage(
	conn net.Conn,
	initialByteCount int,
	shortBufferFloorByteCount int,
	maxByteCount int,
) ([]byte, error) {
	maxByteCount = max(1, maxByteCount)
	readByteCount := min(max(1, initialByteCount), maxByteCount)
	shortBufferFloorByteCount = min(
		max(readByteCount, shortBufferFloorByteCount),
		maxByteCount,
	)
	for {
		readBuf := MessagePoolGet(readByteCount)
		n, err := conn.Read(readBuf)
		if errors.Is(err, io.ErrShortBuffer) {
			MessagePoolReturn(readBuf)
			if maxByteCount < n || readByteCount >= maxByteCount {
				return nil, fmt.Errorf(
					"p2p message exceeds maximum %d (reported=%d)",
					maxByteCount,
					n,
				)
			}
			nextReadByteCount := maxByteCount
			if readByteCount <= maxByteCount/2 {
				nextReadByteCount = readByteCount * 2
			}
			nextReadByteCount = max(nextReadByteCount, shortBufferFloorByteCount)
			if readByteCount < n {
				nextReadByteCount = min(maxByteCount, max(nextReadByteCount, n))
			}
			if nextReadByteCount <= readByteCount {
				return nil, fmt.Errorf(
					"p2p message cannot grow beyond %d (max=%d reported=%d)",
					readByteCount,
					maxByteCount,
					n,
				)
			}
			readByteCount = nextReadByteCount
			continue
		}
		if n < 0 || len(readBuf) < n {
			MessagePoolReturn(readBuf)
			return nil, fmt.Errorf(
				"p2p invalid receive byte count %d (buffer=%d)",
				n,
				len(readBuf),
			)
		}
		if n == 0 {
			MessagePoolReturn(readBuf)
			if err != nil {
				return nil, err
			}
			continue
		}
		return readBuf[:n], err
	}
}

// readP2pReadyHeader waits for the setup marker on a reliable-unordered data
// channel. Once one peer reads the other's marker it may install its send
// route before the other peer has read this marker, so a later transfer frame
// can legitimately be delivered first after packet reordering/loss. Retain at
// most the receive-route capacity and drop any excess (the transfer protocol
// retransmits unacked frames); this keeps setup memory bounded while avoiding
// a false header mismatch/reconnect loop.
func readP2pReadyHeader(
	conn net.Conn,
	settings *P2pTransportSettings,
) (prefetched [][]byte, err error) {
	defer func() {
		if err != nil {
			for _, message := range prefetched {
				MessagePoolReturn(message)
			}
			prefetched = nil
		}
	}()

	header := []byte(ReadyHeader)
	maxMessageByteCount := max(1, settings.MaxMessageByteCount)
	initialMessageByteCount := settings.InitialReadBufferByteCount
	if initialMessageByteCount <= 0 {
		initialMessageByteCount = min(4*1024, maxMessageByteCount)
	}
	maxPrefetched := max(0, settings.ChannelBufferSize)
	retain := func(message []byte) {
		if len(prefetched) < maxPrefetched {
			prefetched = append(prefetched, message)
		} else {
			MessagePoolReturn(message)
		}
	}

	for {
		message, readErr := readP2pMessage(
			conn,
			len(header),
			initialMessageByteCount,
			maxMessageByteCount,
		)
		if slices.Equal(message, header) {
			MessagePoolReturn(message)
			return prefetched, nil
		}
		if readErr != nil {
			MessagePoolReturn(message)
			return nil, readErr
		}
		retain(message)
	}
}

func DefaultP2pTransportSettings() *P2pTransportSettings {
	return &P2pTransportSettings{
		WriteTimeout:          15 * time.Second,
		ReadTimeout:           15 * time.Second,
		ConnectTimeout:        15 * time.Second,
		ReconnectTimeout:      5 * time.Second,
		AdmissionRetryTimeout: 30 * time.Second,
		// Four transfer batches absorb ordinary goroutine scheduling jitter.
		// A real detached-data-channel measurement sustained the same
		// 53-54 MiB/s at depths 1/4/8/32. Keeping 32 therefore added no
		// throughput, but allowed up to 2 MiB of MaxMessageByteCount payloads
		// to sit in each unbudgeted receive route. Four cuts that hard bound
		// to 256 KiB and shortens queueing latency.
		ChannelBufferSize: 4,
		// Transfer batching is capped at 3 KiB, so almost every data-channel
		// message fits in the 4 KiB pooled class. The receiver retries once
		// with Pion's exact required length for a legacy/atypical larger
		// message, then returns to this size.
		InitialReadBufferByteCount: 4 * 1024,
		MaxMessageByteCount:        64 * 1024,
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
	WriteTimeout     time.Duration
	ReadTimeout      time.Duration
	ConnectTimeout   time.Duration
	ReconnectTimeout time.Duration
	// AdmissionRetryTimeout is only the liveness fallback for a saturated
	// peer-connection count/memory budget. Normal retries are woken
	// immediately by a release, avoiding the former sub-second polling storm.
	AdmissionRetryTimeout time.Duration
	ChannelBufferSize     int
	// InitialReadBufferByteCount is the first pooled receive size.
	// io.ErrShortBuffer retries with Pion's exact required length up to
	// MaxMessageByteCount without consuming the queued SCTP message.
	InitialReadBufferByteCount int
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

	var setupError string
	var admissionTimer *time.Timer
	defer func() {
		if admissionTimer != nil {
			admissionTimer.Stop()
		}
	}()

	for {
		// TODO using net.Conn as a stand in for the actual interface

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		countNotify, budgetNotify := self.webRtcManager.AdmissionNotify()
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
			// Log a failure streak once. The former log-on-every-poll behavior
			// produced thousands of lines per device, consumed CPU, and evicted
			// the ICE trace needed to diagnose the failure.
			if nextSetupError := err.Error(); nextSetupError != setupError {
				setupError = nextSetupError
				self.client.log.Infof("[p2p]s(%s) <>%s setup refused = %s\n", self.streamId, self.peerId, err)
			}

			var admissionErr *peerConnectionAdmissionError
			if errors.As(err, &admissionErr) {
				timeout := self.settings.AdmissionRetryTimeout
				if timeout <= 0 {
					timeout = 30 * time.Second
				}
				timerC := resetOrCreateTimer(&admissionTimer, timeout)
				select {
				case <-self.ctx.Done():
					return
				case <-countNotify:
					admissionTimer.Stop()
				case <-budgetNotify:
					admissionTimer.Stop()
				case <-timerC:
				}
				continue
			}
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
			}
			continue
		}
		if setupError != "" {
			self.client.log.V(1).Infof("[p2p]s(%s) <>%s setup recovered\n", self.streamId, self.peerId)
			setupError = ""
		}

		// The signal is a persistent one-shot channel, so network changes or
		// remote restart requests cannot be lost if they race setup.
		immediateReconnect := conn.ImmediateReconnect()

		// at this point, the connection should be able to ping the other side
		// now we wait for the entire stream to be ready by propagating the `ReaderHeader`
		c := func() {
			defer conn.Close()

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			// The peer's ready header must be consumed by exactly one reader
			// before the receive transport starts. Because the channel is
			// reliable-unordered, that reader also boundedly prefetched any
			// transfer frames delivered ahead of the marker.
			headerRead := make(chan [][]byte)

			go HandleError(func() {
				defer handleCancel()

				conn.SetWriteDeadline(time.Now().Add(self.settings.ConnectTimeout))
				_, err := conn.Write([]byte(ReadyHeader))
				if err != nil {
					self.client.log.V(1).Infof("[p2p]s(%s) ready header write err = %s\n", self.streamId, err)
					return
				}

				var prefetched [][]byte
				select {
				case <-handleCtx.Done():
					return
				case prefetched = <-headerRead:
				}

				t, route := newP2pReceiveTransport(
					handleCtx,
					handleCancel,
					conn,
					self.streamId,
					self.settings,
					prefetched,
				)

				updateRoute := func(connected bool) {
					if connected {
						self.receiveRouteManager.UpdateTransport(t, []Route{route})
					} else {
						self.receiveRouteManager.RemoveTransport(t)
					}
				}
				unsub := conn.AddConnectedCallback(updateRoute)
				defer func() {
					unsub()
					updateRoute(false)
				}()

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

				// A peer can install its send route as soon as it reads our
				// marker. On the reliable-unordered channel, its subsequent
				// transfer frame may arrive before its own marker after loss.
				// Recognize and boundedly carry those frames into the receive
				// route instead of treating them as a setup failure.
				conn.SetReadDeadline(time.Now().Add(self.settings.ConnectTimeout))
				prefetched, err := readP2pReadyHeader(conn, self.settings)
				if err != nil {
					self.client.log.V(1).Infof("[p2p]s(%s) ready header read err = %s\n", self.streamId, err)
					return
				}
				select {
				case <-handleCtx.Done():
					for _, message := range prefetched {
						MessagePoolReturn(message)
					}
					return
				case headerRead <- prefetched:
				}

				t, route := NewP2pSendTransportForPeer(handleCtx, handleCancel, conn, self.peerId, self.streamId, self.settings)

				updateRoute := func(connected bool) {
					if connected {
						self.sendRouteManager.UpdateTransport(t, []Route{route})
					} else {
						self.sendRouteManager.RemoveTransport(t)
					}
				}
				unsub := conn.AddConnectedCallback(updateRoute)
				defer func() {
					unsub()
					updateRoute(false)
				}()

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
	return newP2pReceiveTransport(ctx, cancel, conn, streamId, settings, nil)
}

func newP2pReceiveTransport(
	ctx context.Context,
	cancel context.CancelFunc,
	conn net.Conn,
	streamId Id,
	settings *P2pTransportSettings,
	prefetched [][]byte,
) (Transport, Route) {
	receive := make(chan []byte, settings.ChannelBufferSize)
	for _, message := range prefetched {
		receive <- message
	}
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

	// The detached WebRTC data channel is message-oriented. Read directly into
	// the pooled buffer whose ownership is handed to the receive route. The
	// normal transfer batch fits the first 4 KiB attempt. A larger
	// compatibility message retries boundedly; detached Pion masks SCTP's
	// required length, so readP2pMessage grows geometrically when n is zero.
	maxReadByteCount := max(1, self.settings.MaxMessageByteCount)
	initialReadByteCount := self.settings.InitialReadBufferByteCount
	if initialReadByteCount <= 0 {
		initialReadByteCount = min(4*1024, maxReadByteCount)
	}
	initialReadByteCount = min(initialReadByteCount, maxReadByteCount)
	// The ready-header handshake leaves a finite deadline on the shared
	// net.Conn. Clear it once before entering the steady-state reader.
	// Connection liveness comes from ICE/PeerConnection failed-state
	// cancellation; rearming an otherwise ignored 15-second read timeout only
	// woke every idle peer forever.
	self.conn.SetReadDeadline(time.Time{})

	for {
		transferFrameBytes, err := readP2pMessage(
			self.conn,
			initialReadByteCount,
			initialReadByteCount,
			maxReadByteCount,
		)
		if 0 < len(transferFrameBytes) {
			// The route now owns this exact slice and returns it to the pool.
			select {
			case <-self.ctx.Done():
				MessagePoolReturn(transferFrameBytes)
				return
			case self.receive <- transferFrameBytes:
			}
		}
		if err != nil {
			// A non-WebRTC compatibility conn may still surface a deadline.
			// Do not mistake an idle timeout for a dead route.
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
