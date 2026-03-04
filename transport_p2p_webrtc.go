package connect

import (
	"context"
	"fmt"
	"net"

	"github.com/pion/webrtc/v4"
)


type WebRtcSignal struct {
	SignalType SignalType
	Path TransferPath

}

type WebRtcSettings struct {
	SendBufferSize ByteCount
}

func DefaultWebRtcSettings() *WebRtcSettings {
	return &WebRtcSettings{
		// FIXME
		SendBufferSize: mib(1),
	}
}

// FIXME register as a listener on the client like stream manager
type WebRtcManager struct {
	ctx context.Context

	// FIXME api

	// clientId
	// clientOob

	webRtcSettings *WebRtcSettings
}

func NewWebRtcManager(ctx context.Context, webRtcSettings *WebRtcSettings) *WebRtcManager {
	return &WebRtcManager{
		ctx:            ctx,
		webRtcSettings: webRtcSettings,
	}
}


// FIXME client handle SignalExchange message


// FIXME receive active signal
// add to signal channel

func Receive(message MESSAGE) {

	// peer messages will have a reversed path from the local key
	transferPath := message.Path.Reverse()
}








// this should return an active, tested connection
func (self *WebRtcManager) NewP2pConnActive(ctx context.Context, peerId Id, streamId Id) (net.Conn, error) {
	// FIXME
	// 1. create active signal, set active signal to peer on stream id
	// 2. on close, remove active signal from source to peer
	// return nil, fmt.Errorf("Not implemented.")

	// FIXME oob send signal exchange message

	return self.newP2pConn(ctx, peerId, streamId, true)


}

// this should return an active, tested connection
func (self *WebRtcManager) NewP2pConnPassive(ctx context.Context, peerId Id, streamId Id) (net.Conn, error) {
	// FIXME
	// 1. on receive active signal, create passive signal and set from peer on stream id
	// 2. on close, remove passive signal
	// return nil, fmt.Errorf("Not implemented.")

	return self.newP2pConn(ctx, peerId, streamId, false)

}

func newP2pConn(ctx context.Context, peerId Id, streamId Id, active bool) (net.Conn, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	connId := NewId()
	path := PATH()

	conn, err = newPeerConn()
	if err != nil {
		return
	}
	go HandleError(func() {
		defer func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			
			delete(self.conns, path, connId)
		}
		conn.Run()
	})

	self.conns[path][connId] = conn
	return
}





// SignalMessage is the JSON envelope exchanged over the signaling WebSocket.
type SignalMessage struct {
	Type      string                     `json:"type"`
	SDP       *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
}




// conforms to net.Conn
type peerConn struct {
	transferPath TransferPath
	active bool

	pc *webrtc.PeerConnection
}

func newPeerConn() error {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()
	s.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)
	s.SetReceiveMTU(16384)
	s.SetICETimeouts(
		30*time.Second,       // disconnectedTimeout
		60*time.Second,       // failedTimeout
		500*time.Millisecond, // keepAliveInterval
	)

	iceServers := []webrtc.ICEServer{
		{URLs: []string{
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
			"stun:stun2.l.google.com:19302",
		}},
	}
	if turnServer != "" {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs: []string{turnServer},
		})
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})

	conn := &peerConn{

	}

	if active {
		dc, err := pc.CreateDataChannel("data", nil)
		if err != nil {
			log.Fatalf("Failed to create data channel: %v", err)
		}

		dc.OnOpen(func() {
			conn.setOpenDataChannel(dc)
		})
	} else {
		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			dc.OnOpen(func() {
				conn.setOpenDataChannel(dc)
			})
		})
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		if err := sendSignal(conn, SignalMessage{Type: "candidate", Candidate: &init}); err != nil {
			log.Printf("Failed to send ICE candidate: %v", err)
		}

		signal := &SIGNAL{}
		client.Send(signal)
	})

	client.Send(RequestSendSignals())

	// go HandleError(conn.run)

	return conn, nil
}

func (self *peerConn) Run() {



	// pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
	// 	log.Printf("ICE connection state: %s", state.String())
	// 	if state == webrtc.ICEConnectionStateConnected {
	// 		logSelectedPair(pc)
	// 	}
	// })

	if active {
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			log.Fatalf("Failed to create offer: %v", err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			log.Fatalf("Failed to set local description: %v", err)
		}

		if err := sendSignal(conn, SignalMessage{Type: "offer", SDP: &offer}); err != nil {
			log.Fatalf("Failed to send SDP offer: %v", err)
		}
		log.Println("Sent SDP offer")
	}


	update := func(signal SIGNAL) error {
		switch SIGNAL.SignalType {
		case SdpOffer:
			if !self.active {
				if err := pc.SetRemoteDescription(*msg.SDP); err != nil {
					log.Fatalf("Failed to set remote description: %v", err)
				}

				answer, err := pc.CreateAnswer(nil)
				if err != nil {
					log.Fatalf("Failed to create answer: %v", err)
				}
				if err := pc.SetLocalDescription(answer); err != nil {
					log.Fatalf("Failed to set local description: %v", err)
				}
			} else {
				ERROR()
			}
		case SdpAnswer:
			if self.active {
				if err := pc.SetRemoteDescription(*msg.SDP); err != nil {
					log.Fatalf("Failed to set remote description: %v", err)
				}
			} else {
				ERROR()
			}
		case IceCandidate:
			if err := pc.AddICECandidate(*msg.Candidate); err != nil {
				log.Printf("Failed to add ICE candidate: %v", err)
			}
		}
	}

	for {
		select {
		case <- ctx.Done():
			return
		case message := <- self.messages:
			if SIGNAL {
				update(signal)
			} else if SIGNALS {
				for _, signal := SIGNALS.Signals {
					update(signal)
				}

			} else if REQUESTSENDSIGNALS {
				// get all signals and send
				client.Send(ExchangeSignals{
					Reset: true,
					Signals: signals(),
				})
			}
		}
	}
}

func setOpenDataChannel(dc *webrtc.DataChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()


	self.dcMonitor.NotifyAll()
}

func addSignal() {
	// TODO save signal
	// TODO send signal
}

func signals() []SIGNAL {
	
}

func dataChannel(deadline time.Time) (*webrtc.DataChannel, error) {
	dc := func()(*webrtc.DataChannel) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.dc
	}
	if dc := dc(); dc != nil {
		return dc
	}
	update := self.dcMonitor.NotifyChannel()
	for {
		if deadline.IsZero() {
			select {
			case <- self.ctx.Done():
				return
			case <- update:
			}
		} else {
			timeout := deadline.Sub(time.Now())
			if timeout <= 0 {
				return nil, os.DeadlineError
			}
			select {
			case <- self.ctx.Done():
				return
			case <- update:
			case <- time.After(timeout):
				return nil, os.DeadlineError
			}
		}
		if dc := dc(); dc != nil {
			return dc
		}
	}
}


Read(b []byte) (n int, err error) {
	var deadline time.Time
	func()(*webrtc.DataChannel) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deadline = self.readDeadline
	}
	var dc *webrtc.DataChannel
	dc, err = self.dataChannel(deadline)
	if err != nil {
		return
	}
	n, err = dc.Read(b)
	return
}

Write(b []byte) (n int, err error) {
	var deadline time.Time
	func()(*webrtc.DataChannel) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deadline = self.writeDeadline
	}
	var dc *webrtc.DataChannel
	dc, err = self.dataChannel(deadline)
	if err != nil {
		return
	}
	n, err = dc.Write(b)
	return
}

Close() error {
	return self.pc.Close()
}

// LocalAddr returns the local network address, if known.
LocalAddr() Addr {
	sctp := pc.SCTP()
	if sctp == nil {
		return ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return ""
	}
	return pair.Local.Address
}

// RemoteAddr returns the remote network address, if known.
RemoteAddr() Addr {
	sctp := pc.SCTP()
	if sctp == nil {
		return ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return ""
	}
	return pair.Remote.Address
}


SetDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t
	self.writeDeadline = t
}

SetReadDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t
}

SetWriteDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.writeDeadline = t
}






// enum SignalType {
//     // reset means clear all previous signals for the transfer peer pair
//     Reset = 0;
//     SdpOffer = 1;
//     SdpAnswer = 2;
//     IceCandidate = 3;
// }

// // signals are sent in real time to the destination of the peer pair
// message ExchangeSignal {
//     // ulid
//     bytes stream_id = 1;
//     SignalType signal_type = 2;

//     // json encoded using the structure in the code
//     optional bytes Sdp = 3;
//     // json encoded using the structure in the code
//     optional bytes IceCandidate = 4;
// }

// message RequestExchangeSignals {
//     // ulid
//     bytes stream_id = 1;
// }



