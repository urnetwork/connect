package connect

import (
	"context"
	// "fmt"
	"encoding/json"
	"net"
	"os"
	"sync"
	"time"
	// "slices"
	// "fmt"

	// "golang.org/x/exp/maps"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"
)

type WebRtcConn interface {
	net.Conn
	Connected() bool
	AddConnectedCallback(func(connected bool)) func()
}

func DefaultWebRtcSettings() *WebRtcSettings {
	return &WebRtcSettings{
		// FIXME
		// SendBufferSize: mib(1),

		ReceiveBufferSize:   mib(4),
		ReceiveMtu:          kib(4),
		DisconnectedTimeout: 30 * time.Second,
		FailedTimeout:       30 * time.Second,
		KeepAliveTimeout:    1 * time.Second,

		DataChannelLabel: "data",
		IceServerUrls: []string{
			"stun:openrelay.metered.ca:80",
			"stun:openrelay.metered.ca:443",
			"stun:stun.stunprotocol.org:3478",
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
			"stun:stun2.l.google.com:19302",
			"stun:stun3.l.google.com:19302",
			"stun:stun4.l.google.com:19302",
		},
	}
}

type WebRtcSettings struct {
	ReceiveBufferSize   ByteCount
	ReceiveMtu          ByteCount
	DisconnectedTimeout time.Duration
	FailedTimeout       time.Duration
	KeepAliveTimeout    time.Duration

	DataChannelLabel string

	// add stun:xxx urls here
	IceServerUrls []string
}

type WebRtcManager struct {
	ctx context.Context

	client *Client

	settings *WebRtcSettings

	stateLock sync.Mutex
	peerConns map[peerConnKey]*peerConn
}

func NewWebRtcManager(ctx context.Context, client *Client, settings *WebRtcSettings) *WebRtcManager {
	return &WebRtcManager{
		ctx:       ctx,
		client:    client,
		settings:  settings,
		peerConns: map[peerConnKey]*peerConn{},
	}
}

// ReceiveFunction
func (self *WebRtcManager) Receive(source TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	for _, frame := range frames {
		err := self.handleControlFrame(source, frame)
		if err != nil {
			glog.Infof("[peerconn]handle err=%s\n", err)
			// ignore error
		}
	}
}

func (self *WebRtcManager) handleControlFrame(source TransferPath, frame *protocol.Frame) error {
	switch frame.MessageType {
	case protocol.MessageType_TransferExchangeSignals:
		message, err := FromFrame(frame)
		if err != nil {
			return err
		}
		switch v := message.(type) {
		case *protocol.ExchangeSignals:
			key := peerConnKey{
				PeerId:   source.SourceId,
				StreamId: Id(v.StreamId),
				// ConnId: Id(v.ConnId),
			}
			var conn *peerConn
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				conn = self.peerConns[key]
			}()
			if conn != nil {
				for _, signal := range v.Signals {
					err := conn.ReceiveSignalFromPeer(signal)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// this should return an active, tested connection
func (self *WebRtcManager) NewP2pConnActive(ctx context.Context, peerId Id, streamId Id) (WebRtcConn, error) {
	return self.newP2pConn(ctx, peerId, streamId, true)
}

// this should return an active, tested connection
func (self *WebRtcManager) NewP2pConnPassive(ctx context.Context, peerId Id, streamId Id) (WebRtcConn, error) {
	return self.newP2pConn(ctx, peerId, streamId, false)
}

func (self *WebRtcManager) newP2pConn(ctx context.Context, peerId Id, streamId Id, active bool) (conn *peerConn, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	key := peerConnKey{
		PeerId:   peerId,
		StreamId: streamId,
	}

	conn, err = newPeerConn(ctx, key, active, self.client, self.settings)
	if err != nil {
		return
	}
	go HandleError(func() {
		defer func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			if conn == self.peerConns[key] {
				delete(self.peerConns, key)
			}
		}()
		conn.Run()
	})

	replacedConn := self.peerConns[key]
	if replacedConn != nil {
		replacedConn.Cancel()
	}
	self.peerConns[key] = conn
	return
}

type peerConnKey struct {
	PeerId   Id
	StreamId Id
}

// conforms to WebRtcConn
type peerConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	key    peerConnKey
	active bool

	client   *Client
	settings *WebRtcSettings

	api *webrtc.API
	pc  *webrtc.PeerConnection

	connectedCallbacks *CallbackList[func(connected bool)]
	connMonitor        *Monitor

	stateLock sync.Mutex
	// signals []*protocol.ExchangeSignal
	conn      datachannel.ReadWriteCloserDeadliner
	connected bool
	offer     *protocol.ExchangeSignal
	answer    *protocol.ExchangeSignal

	readDeadline  time.Time
	writeDeadline time.Time
}

func newPeerConn(ctx context.Context, key peerConnKey, active bool, client *Client, settings *WebRtcSettings) (*peerConn, error) {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()
	s.SetSCTPMaxReceiveBufferSize( /*16 * 1024 * 1024*/ uint32(settings.ReceiveBufferSize))
	s.SetReceiveMTU( /*16384*/ uint(settings.ReceiveMtu))
	s.SetICETimeouts(
		settings.DisconnectedTimeout,
		settings.FailedTimeout,
		settings.KeepAliveTimeout,
	)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			webrtc.ICEServer{
				URLs: settings.IceServerUrls,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	conn := &peerConn{
		ctx:                cancelCtx,
		cancel:             cancel,
		key:                key,
		active:             active,
		client:             client,
		settings:           settings,
		api:                api,
		pc:                 pc,
		connectedCallbacks: NewCallbackList[func(connected bool)](),
		connMonitor:        NewMonitor(),
	}
	return conn, nil
}

func (self *peerConn) Run() {
	defer func() {
		self.cancel()

		self.pc.Close()
		self.connMonitor.NotifyAll()
	}()

	self.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		connected := state == webrtc.ICEConnectionStateConnected
		glog.Infof("[peerconn]state=%v (%t)\n", state, connected)
		self.setConnected(connected)
	})

	if self.active {
		dc, err := self.pc.CreateDataChannel(self.settings.DataChannelLabel, nil)
		if err != nil {
			return
		}

		dc.OnOpen(func() {
			self.setOpenDataChannel(dc)
		})
	} else {
		self.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			dc.OnOpen(func() {
				self.setOpenDataChannel(dc)
			})
		})
	}

	if self.active {
		offer, err := self.pc.CreateOffer(nil)
		if err != nil {
			return
		}
		err = self.pc.SetLocalDescription(offer)
		if err != nil {
			return
		}

		offerBytes, err := json.Marshal(&offer)
		if err != nil {
			return
		}

		signal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_SdpOffer,
			Sdp:        offerBytes,
		}
		self.setOfferSignal(signal)
		self.sendSignal(signal)
	} else {
		signal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_WaitingForSdpOffer,
		}
		self.sendSignal(signal)
	}

	select {
	case <-self.ctx.Done():
	}
}

func (self *peerConn) ReceiveSignalFromPeer(signal *protocol.ExchangeSignal) error {
	switch signal.SignalType {
	case protocol.SignalType_SdpOffer:
		if !self.active && self.setOfferSignal(signal) {
			var offer webrtc.SessionDescription
			err := json.Unmarshal(signal.Sdp, &offer)
			if err != nil {
				return err
			}
			err = self.pc.SetRemoteDescription(offer)
			if err != nil {
				return err
			}
			answer, err := self.pc.CreateAnswer(nil)
			if err != nil {
				return err
			}
			err = self.pc.SetLocalDescription(answer)
			if err != nil {
				return err
			}

			answerBytes, err := json.Marshal(&answer)
			if err != nil {
				return err
			}

			signal := &protocol.ExchangeSignal{
				SignalType: protocol.SignalType_SdpAnswer,
				Sdp:        answerBytes,
			}
			self.setAnswerSignal(signal)
			self.sendSignal(signal)

			self.addIceCandidates()
		}
	case protocol.SignalType_SdpAnswer:
		if self.active && self.setAnswerSignal(signal) {
			var answer webrtc.SessionDescription
			err := json.Unmarshal(signal.Sdp, &answer)
			if err != nil {
				return err
			}
			err = self.pc.SetRemoteDescription(answer)
			if err != nil {
				return err
			}

			self.addIceCandidates()
		}
	case protocol.SignalType_IceCandidate:
		var candidate webrtc.ICECandidateInit
		err := json.Unmarshal(signal.IceCandidate, &candidate)
		if err != nil {
			return err
		}
		err = self.pc.AddICECandidate(candidate)
		if err != nil {
			return err
		}
	case protocol.SignalType_WaitingForSdpOffer:
		if self.active {

			if self.answerSignal() == nil {
				if signal := self.offerSignal(); signal != nil {
					self.sendSignal(signal)
				}
			} else {
				// already negotiated an answer with a peer
				// cancel this connection so a new one can start in the expected state
				self.cancel()
			}
		}
		// else ignore
	}
	return nil
}

func (self *peerConn) addIceCandidates() {
	self.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateBytes, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			return
		}
		signal := &protocol.ExchangeSignal{
			SignalType:   protocol.SignalType_IceCandidate,
			IceCandidate: candidateBytes,
		}
		self.sendSignal(signal)
	})
}

func (self *peerConn) Connected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.connected
}

func (self *peerConn) setConnected(connected bool) {
	changed := false

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.connected != connected {
			self.connected = connected
			changed = true
		}
	}()

	if changed {
		self.connectedChanged(self.Connected())
	}
}

func (self *peerConn) AddConnectedCallback(connectedCallback func(connected bool)) func() {
	callbackId := self.connectedCallbacks.Add(connectedCallback)
	return func() {
		self.connectedCallbacks.Remove(callbackId)
	}
}

func (self *peerConn) connectedChanged(connected bool) {
	for _, callback := range self.connectedCallbacks.Get() {
		HandleError(func() {
			callback(connected)
		})
	}
}

func (self *peerConn) setOfferSignal(offer *protocol.ExchangeSignal) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.offer != nil {
		return false
	}
	self.offer = offer
	return true
}

func (self *peerConn) offerSignal() *protocol.ExchangeSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.offer
}

func (self *peerConn) setAnswerSignal(answer *protocol.ExchangeSignal) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.answer != nil {
		return false
	}
	self.answer = answer
	return true
}

func (self *peerConn) answerSignal() *protocol.ExchangeSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.answer
}

func (self *peerConn) sendSignal(signal *protocol.ExchangeSignal) {
	signals := &protocol.ExchangeSignals{
		StreamId: self.key.StreamId.Bytes(),
		Signals:  []*protocol.ExchangeSignal{signal},
	}
	self.client.Send(
		RequireToFrameWithDefaultProtocolVersion(signals),
		DestinationId(self.key.PeerId),
		nil,
	)
}

func (self *peerConn) setOpenDataChannel(dc *webrtc.DataChannel) error {
	conn, err := dc.DetachWithDeadline()
	if err != nil {
		return err
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.conn != nil {
			self.conn.Close()
		}
		self.conn = conn
		self.connMonitor.NotifyAll()
	}()

	return nil
}

func (self *peerConn) dataChannelConn(deadline time.Time) (datachannel.ReadWriteCloserDeadliner, error) {
	conn := func() (datachannel.ReadWriteCloserDeadliner, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.conn, self.connMonitor.NotifyChannel()
	}
	for {
		c, update := conn()
		if c != nil {
			return c, nil
		}
		if deadline.IsZero() {
			select {
			case <-self.ctx.Done():
				return nil, os.ErrClosed
			case <-update:
			}
		} else {
			timeout := deadline.Sub(time.Now())
			if timeout <= 0 {
				return nil, os.ErrDeadlineExceeded
			}
			select {
			case <-self.ctx.Done():
				return nil, os.ErrClosed
			case <-update:
			case <-time.After(timeout):
				return nil, os.ErrDeadlineExceeded
			}
		}
	}
}

func (self *peerConn) Read(b []byte) (n int, err error) {
	var deadline time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		deadline = self.readDeadline
	}()
	var c datachannel.ReadWriteCloserDeadliner
	c, err = self.dataChannelConn(deadline)
	if err != nil {
		return
	}
	c.SetReadDeadline(deadline)
	n, err = c.Read(b)
	return
}

func (self *peerConn) Write(b []byte) (n int, err error) {
	var deadline time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		deadline = self.writeDeadline
	}()
	var c datachannel.ReadWriteCloserDeadliner
	c, err = self.dataChannelConn(deadline)
	if err != nil {
		return
	}
	c.SetWriteDeadline(deadline)
	n, err = c.Write(b)
	return
}

// LocalAddr returns the local network address, if known.
func (self *peerConn) LocalAddr() net.Addr {
	sctp := self.pc.SCTP()
	if sctp == nil {
		return newWebRtcAddr("")
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return newWebRtcAddr("")
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return newWebRtcAddr("")
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return newWebRtcAddr("")
	}
	return newWebRtcAddr(pair.Local.Address)
}

// RemoteAddr returns the remote network address, if known.
func (self *peerConn) RemoteAddr() net.Addr {
	sctp := self.pc.SCTP()
	if sctp == nil {
		return newWebRtcAddr("")
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return newWebRtcAddr("")
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return newWebRtcAddr("")
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return newWebRtcAddr("")
	}
	return newWebRtcAddr(pair.Remote.Address)
}

func (self *peerConn) SetDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t
	self.writeDeadline = t

	return nil
}

func (self *peerConn) SetReadDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t

	return nil
}

func (self *peerConn) SetWriteDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.writeDeadline = t

	return nil
}

func (self *peerConn) Close() error {
	self.cancel()
	return nil
}

func (self *peerConn) Cancel() {
	self.cancel()
}

// conforms to `net.Addr`
type webRtcAddr struct {
	addr string
}

func newWebRtcAddr(addr string) net.Addr {
	return &webRtcAddr{
		addr: addr,
	}
}

func (self *webRtcAddr) Network() string {
	return "udp"
}

func (self *webRtcAddr) String() string {
	return self.addr
}
