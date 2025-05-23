package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	mathrand "math/rand"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/golang/glog"

	"github.com/urnetwork/connect/protocol"
)

// note that it is possible to have multiple transports for the same client destination
// e.g. platform, p2p, and a bunch of extenders

// extenders are identified and credited with the platform by ip address
// they forward to a special port, 8443, that whitelists their ip without rate limiting
// when an extender gets an http message from a client, it always connects tcp to connect.bringyour.com:8443
// appends the proxy protocol headers, and then forwards the bytes from the client
// https://docs.nginx.com/nginx/admin-guide/load-balancer/using-proxy-protocol/
// rate limit using $proxy_protocol_addr https://www.nginx.com/blog/rate-limiting-nginx/
// add the source ip as the X-Extender header

// the transport attempts to upgrade from http1 to http3
// versus the h1 transport, h3 is:
// - more cpu efficient.
//   The quic stream does not need to mask/unmask each byte before TLS.
// - faster to connect.
//   The quic stream uses 0rtt connect when possible to speed up the initial connection.
// - better throughput on poor networks.
//   quic optimizes congestion control to better handle poor network conditions.
// However, h3 is not available in all locations due to dpi/filtering.
// When available, it takes precedence over the default transport.

// packet translation mode gives options for how udp packets are formed on the wire
// We include options here that are known to help with availability

// When packet translation is set, the upgrade mode must be h3 only

type TransportMode string

// in order of increasing preference
const (
	// start all modes in skewed parallel and choose the best one
	TransportModeAuto      TransportMode = "auto"
	TransportModeH3DnsPump TransportMode = "h3dnspump"
	TransportModeH3Dns     TransportMode = "h3dns"
	TransportModeH1        TransportMode = "h1"
	TransportModeH3        TransportMode = "h3"
)

type ClientAuth struct {
	ByJwt string
	// ClientId Id
	InstanceId Id
	AppVersion string
}

func (self *ClientAuth) ClientId() (Id, error) {
	byJwt, err := ParseByJwtUnverified(self.ByJwt)
	if err != nil {
		return Id{}, err
	}
	return byJwt.ClientId, nil
}

// (ctx, network, address)
// type DialContextFunc func(ctx context.Context, network string, address string) (net.Conn, error)

type PlatformTransportSettings struct {
	HttpConnectTimeout   time.Duration
	WsHandshakeTimeout   time.Duration
	QuicConnectTimeout   time.Duration
	QuicHandshakeTimeout time.Duration
	QuicTlsConfig        *tls.Config
	AuthTimeout          time.Duration
	ReconnectTimeout     time.Duration
	PingTimeout          time.Duration
	WriteTimeout         time.Duration
	ReadTimeout          time.Duration
	TransportGenerator   func() (sendTransport Transport, receiveTransport Transport)
	TransportBufferSize  int
	InactiveDrainTimeout time.Duration
	// it smoothes out the h3 transition to not start/stop h1 if h3 connects in this time
	ModeInitialDelay time.Duration

	// FIXME
	DnsTlds [][]byte
}

func DefaultPlatformTransportSettings() *PlatformTransportSettings {
	pingTimeout := 1 * time.Second
	return &PlatformTransportSettings{
		HttpConnectTimeout:   2 * time.Second,
		WsHandshakeTimeout:   2 * time.Second,
		QuicConnectTimeout:   2 * time.Second,
		QuicHandshakeTimeout: 2 * time.Second,
		QuicTlsConfig:        nil,
		AuthTimeout:          2 * time.Second,
		ReconnectTimeout:     5 * time.Second,
		PingTimeout:          pingTimeout,
		WriteTimeout:         5 * time.Second,
		ReadTimeout:          15 * time.Second,
		TransportBufferSize:  1,
		InactiveDrainTimeout: 15 * time.Second,
		ModeInitialDelay:     1 * time.Second,
		// FIXME
		DnsTlds: nil,
	}
}

type PlatformTransport struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientStrategy *ClientStrategy
	routeManager   *RouteManager

	platformUrl string
	auth        *ClientAuth

	settings *PlatformTransportSettings

	stateLock      sync.Mutex
	modeMonitor    *Monitor
	availableModes map[TransportMode]bool
	targetMode     TransportMode
	mode           TransportMode
}

func NewPlatformTransportWithDefaults(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
) *PlatformTransport {
	return NewPlatformTransport(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		DefaultPlatformTransportSettings(),
	)
}

func NewPlatformTransport(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	return NewPlatformTransportWithTargetMode(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		TransportModeH1,
		settings,
	)
}

func NewPlatformTransportWithTargetMode(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	targetMode TransportMode,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &PlatformTransport{
		ctx:            cancelCtx,
		cancel:         cancel,
		clientStrategy: clientStrategy,
		routeManager:   routeManager,
		platformUrl:    platformUrl,
		auth:           auth,
		settings:       settings,
		modeMonitor:    NewMonitor(),
		availableModes: map[TransportMode]bool{},
		targetMode:     targetMode,
		mode:           TransportModeAuto,
	}
	go transport.run()
	return transport
}

func (self *PlatformTransport) setModeAvailable(mode TransportMode, available bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.availableModes[mode] = available
}

func (self *PlatformTransport) modeAvailable(mode TransportMode) (bool, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.availableModes[mode], self.modeMonitor.NotifyChannel()
}

func (self *PlatformTransport) modesAvailable() (map[TransportMode]bool, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return maps.Clone(self.availableModes), self.modeMonitor.NotifyChannel()
}

func (self *PlatformTransport) setActiveMode(mode TransportMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.mode = mode
}

func (self *PlatformTransport) activeMode() (TransportMode, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.mode, self.modeMonitor.NotifyChannel()
}

func (self *PlatformTransport) run() {
	defer self.cancel()

	switch self.targetMode {
	case TransportModeAuto:
		go self.runH3(TransportModeH3, 0)
		go self.runH1(self.settings.ModeInitialDelay)
		go self.runH3(TransportModeH3Dns, self.settings.ModeInitialDelay*2)
		go self.runH3(TransportModeH3DnsPump, self.settings.ModeInitialDelay*3)
	case TransportModeH3:
		go self.runH3(TransportModeH3, 0)
	case TransportModeH1:
		go self.runH1(0)
	case TransportModeH3Dns:
		go self.runH3(TransportModeH3Dns, 0)
	case TransportModeH3DnsPump:
		go self.runH3(TransportModeH3DnsPump, 0)
	}

	for {
		available, notify := self.modesAvailable()

		preferences := map[TransportMode]int{
			TransportModeAuto:      0,
			TransportModeH3DnsPump: 1,
			TransportModeH3Dns:     2,
			TransportModeH1:        3,
			TransportModeH3:        4,
		}
		// descending preference
		orderedModes := maps.Keys(preferences)
		slices.SortFunc(orderedModes, func(a TransportMode, b TransportMode) int {
			if a == b {
				return 0
			} else if isBetterMode(a, b) {
				return -1
			} else {
				return 1
			}
		})
		if 0 < len(orderedModes) {
			for _, mode := range orderedModes {
				if available[mode] {
					self.setActiveMode(mode)
					break
				}
			}
		} else {
			self.setActiveMode(TransportModeAuto)
		}

		select {
		case <-notify:
		case <-self.ctx.Done():
			return
		}
	}
}

// returns true is other is better than current
func isBetterMode(current TransportMode, other TransportMode) bool {
	preferences := map[TransportMode]int{
		TransportModeAuto:      0,
		TransportModeH3DnsPump: 1,
		TransportModeH3Dns:     2,
		TransportModeH1:        3,
		TransportModeH3:        4,
	}

	return preferences[current] < preferences[other]
}

func (self *PlatformTransport) runH1(initialTimeout time.Duration) {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.auth.ClientId()

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	for {
		// wait until we are back in h1 or worse
		func() {
			for {
				mode, notify := self.activeMode()
				if isBetterMode(TransportModeH1, mode) {
					select {
					case <-self.ctx.Done():
						return
					case <-notify:
					}
				} else {
					return
				}
			}
		}()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		connect := func() (*websocket.Conn, error) {
			header := http.Header{}
			header.Add("Authorization", fmt.Sprintf("Bearer %s", self.auth.ByJwt))
			header.Add("X-UR-AppVersion", self.auth.AppVersion)
			header.Add("X-UR-InstanceId", self.auth.InstanceId.String())

			ws, _, err := self.clientStrategy.WsDialContext(self.ctx, self.platformUrl, header)
			if err != nil {
				return nil, err
			}

			return ws, nil
		}

		var ws *websocket.Conn
		var err error
		if glog.V(2) {
			ws, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			ws, err = connect()
		}
		if err != nil {
			glog.Infof("[t]auth error %s = %s\n", clientId, err)
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
				continue
			}
		}

		c := func() {
			defer ws.Close()

			self.setModeAvailable(TransportModeH1, true)
			defer self.setModeAvailable(TransportModeH1, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			readCounter := atomic.Uint64{}
			writeCounter := atomic.Uint64{}

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewSendGatewayTransport()
				receiveTransport = NewReceiveGatewayTransport()
			}

			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			defer func() {
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)

				// note `send` is not closed. This channel is left open.
				// it used to be closed after a delay, but it is not needed to close it.
			}()

			go func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if mode != TransportModeH1 {
						startReadCount := readCounter.Load()
						startWriteCount := writeCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readCounter.Load() == startReadCount && writeCounter.Load() == startWriteCount {
								handleCancel()
							}
						case <-notify:
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						}
					}
				}
			}()

			go func() {
				defer handleCancel()

				for {
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-send:
						if !ok {
							return
						}

						ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							glog.Infof("[ts]%s-> error = %s\n", clientId, err)
							return
						}
						glog.V(2).Infof("[ts]%s->\n", clientId)

						writeCounter.Add(1)
					case <-time.After(self.settings.PingTimeout):
						ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							return
						}
					}
				}
			}()

			go func() {
				defer func() {
					handleCancel()
					close(receive)
				}()

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
					messageType, message, err := ws.ReadMessage()
					if err != nil {
						glog.Infof("[tr]%s<- error = %s\n", clientId, err)
						return
					}

					readCounter.Add(1)

					switch messageType {
					case websocket.BinaryMessage:
						if 0 == len(message) {
							// ping
							glog.V(2).Infof("[tr]ping %s<-\n", clientId)
							continue
						}

						select {
						case <-handleCtx.Done():
							return
						case receive <- message:
							glog.V(2).Infof("[tr]%s<-\n", clientId)
						case <-time.After(self.settings.ReadTimeout):
							glog.Infof("[tr]drop %s<-\n", clientId)
						}
					default:
						glog.V(2).Infof("[tr]other=%s %s<-\n", messageType, clientId)
					}
				}
			}()

			select {
			case <-handleCtx.Done():
			}
		}

		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		if glog.V(2) {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}
	}
}

func (self *PlatformTransport) runH3(ptMode TransportMode, initialTimeout time.Duration) {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.auth.ClientId()

	authBytes, err := EncodeFrame(&protocol.Auth{
		ByJwt:      self.auth.ByJwt,
		AppVersion: self.auth.AppVersion,
		InstanceId: self.auth.InstanceId.Bytes(),
	})
	if err != nil {
		return
	}

	h3Framer := NewFramerWithDefaults()

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	for {
		// wait until we are back in the specific pt mode or auto mode
		func() {
			for {
				mode, notify := self.activeMode()
				if isBetterMode(ptMode, mode) {
					select {
					case <-self.ctx.Done():
						return
					case <-notify:
					}
				} else {
					return
				}
			}
		}()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		connect := func() (quic.Stream, error) {
			quicConfig := &quic.Config{
				HandshakeIdleTimeout: self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout,
			}

			var packetConn net.PacketConn

			udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
			if err != nil {
				return nil, err
			}
			// udpAddr, err := net.ResolveUDPAddr("udp", addr)
			// if err != nil {
			// 	return nil, err
			// }

			var udpAddr *net.UDPAddr
			switch ptMode {
			case TransportModeH3Dns:
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", tld, 53))
				ptSettings := &PacketTranslationSettings{
					DnsTlds: [][]byte{tld},
				}
				packetConn, err = NewPacketTranslation(self.ctx, PacketTranslationModeDns, udpConn, ptSettings)
				if err != nil {
					return nil, err
				}
			case TransportModeH3DnsPump:
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", tld, 53))
				ptSettings := &PacketTranslationSettings{
					DnsTlds: [][]byte{tld},
				}
				packetConn, err = NewPacketTranslation(self.ctx, PacketTranslationModeDnsPump, udpConn, ptSettings)
				if err != nil {
					return nil, err
				}
			default:
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", self.platformUrl, 443))
				packetConn = udpConn
			}

			quicTransport := &quic.Transport{
				Conn: packetConn,
				// createdConn: true,
				// isSingleUse: true,
			}

			// enable 0rtt if possible
			conn, err := quicTransport.DialEarly(self.ctx, udpAddr, self.settings.QuicTlsConfig, quicConfig)

			// conn, err := quic.Dial(self.ctx, packetConn, packetConn.ConnectedAddr(), self.settings.QuicTlsConfig, quicConfig)
			if err != nil {
				return nil, err
			}

			stream, err := conn.OpenStream()
			if err != nil {
				return nil, err
			}

			success := false
			defer func() {
				if !success {
					conn.CloseWithError(0, "")
				}
			}()

			stream.SetWriteDeadline(time.Now().Add(self.settings.AuthTimeout))
			if err := h3Framer.Write(stream, authBytes); err != nil {
				return nil, err
			}
			stream.SetReadDeadline(time.Now().Add(self.settings.AuthTimeout))
			if message, err := h3Framer.Read(stream); err != nil {
				return nil, err
			} else {
				// verify the auth echo
				if !bytes.Equal(authBytes, message) {
					return nil, fmt.Errorf("Auth response error: bad bytes.")
				}
			}

			success = true
			return stream, nil
		}

		var stream quic.Stream
		var err error
		if glog.V(2) {
			stream, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			stream, err = connect()
		}
		if err != nil {
			glog.Infof("[t]auth error %s = %s\n", clientId, err)
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
				continue
			}
		}

		c := func() {
			defer stream.Close()

			self.setModeAvailable(ptMode, true)
			defer self.setModeAvailable(ptMode, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			readCounter := atomic.Uint64{}
			writeCounter := atomic.Uint64{}

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewPrioritySendGatewayTransport(TransportMaxPriority, TransportMaxWeight)
				receiveTransport = NewPriorityReceiveGatewayTransport(TransportMaxPriority, TransportMaxWeight)
			}

			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})

			defer func() {
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)

				// note `send` is not closed. This channel is left open.
				// it used to be closed after a delay, but it is not needed to close it.
			}()

			go func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if mode != ptMode {
						startReadCount := readCounter.Load()
						startWriteCount := writeCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readCounter.Load() == startReadCount && writeCounter.Load() == startWriteCount {
								handleCancel()
							}
						case <-notify:
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						}
					}
				}
			}()

			go func() {
				defer handleCancel()

				for {
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-send:
						if !ok {
							return
						}

						stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := h3Framer.Write(stream, message); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							glog.Infof("[ts]%s-> error = %s\n", clientId, err)
							return
						}
						glog.V(2).Infof("[ts]%s->\n", clientId)
					case <-time.After(self.settings.PingTimeout):
						stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := h3Framer.Write(stream, make([]byte, 0)); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							return
						}
					}
				}
			}()

			go func() {
				defer func() {
					handleCancel()
					close(receive)
				}()

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					stream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
					message, err := h3Framer.Read(stream)
					if err != nil {
						glog.Infof("[tr]%s<- error = %s\n", clientId, err)
						return
					}

					if 0 == len(message) {
						// ping
						glog.V(2).Infof("[tr]ping %s<-\n", clientId)
						continue
					}

					select {
					case <-handleCtx.Done():
						return
					case receive <- message:
						glog.V(2).Infof("[tr]%s<-\n", clientId)
					case <-time.After(self.settings.ReadTimeout):
						glog.Infof("[tr]drop %s<-\n", clientId)
					}
				}
			}()

			select {
			case <-handleCtx.Done():
			}
		}
		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		if glog.V(2) {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}
	}
}

func (self *PlatformTransport) Close() {
	self.cancel()
}
