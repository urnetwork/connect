package connect

import (
	"bytes"
	"context"
	"fmt"
	// "net"
	"crypto/tls"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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

type TransportMode string

const (
	H1 TransportMode = "h1"
	H3 TransportMode = "h3"
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
	H1InitialDelay time.Duration
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
		H1InitialDelay:       1 * time.Second,
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

	h3Framer *Framer

	stateLock      sync.Mutex
	modeMonitor    *Monitor
	availableModes map[TransportMode]bool
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
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &PlatformTransport{
		ctx:            cancelCtx,
		cancel:         cancel,
		clientStrategy: clientStrategy,
		routeManager:   routeManager,
		platformUrl:    platformUrl,
		auth:           auth,
		settings:       settings,
		h3Framer:       NewFramerWithDefaults(),
		modeMonitor:    NewMonitor(),
		availableModes: map[TransportMode]bool{},
		mode:           H1,
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

	go self.runH1()
	go self.runH3()

	for {
		h3, notify := self.modeAvailable(H3)
		if h3 {
			self.setActiveMode(H3)
		} else {
			self.setActiveMode(H1)
		}

		select {
		case <-notify:
		case <-self.ctx.Done():
			return
		}
	}
}

func (self *PlatformTransport) runH1() {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.auth.ClientId()

	if 0 < self.settings.H1InitialDelay {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.H1InitialDelay):
		}
	}

	for {
		// wait until we are back in h1 mode
		func() {
			for {
				mode, notify := self.activeMode()
				if mode == H1 {
					return
				}

				select {
				case <-self.ctx.Done():
					return
				case <-notify:
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

			self.setModeAvailable(H1, true)
			defer self.setModeAvailable(H1, false)

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
					if mode != H1 {
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

func (self *PlatformTransport) runH3() {
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

	for {
		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		connect := func() (quic.Stream, error) {
			quicConfig := &quic.Config{
				HandshakeIdleTimeout: self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout,
			}
			conn, err := quic.DialAddr(self.ctx, self.platformUrl, self.settings.QuicTlsConfig, quicConfig)
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
			if err := self.h3Framer.Write(stream, authBytes); err != nil {
				return nil, err
			}
			stream.SetReadDeadline(time.Now().Add(self.settings.AuthTimeout))
			if message, err := self.h3Framer.Read(stream); err != nil {
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

			self.setModeAvailable(H3, true)
			defer self.setModeAvailable(H3, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

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
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-send:
						if !ok {
							return
						}

						stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := self.h3Framer.Write(stream, message); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							glog.Infof("[ts]%s-> error = %s\n", clientId, err)
							return
						}
						glog.V(2).Infof("[ts]%s->\n", clientId)
					case <-time.After(self.settings.PingTimeout):
						stream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := self.h3Framer.Write(stream, make([]byte, 0)); err != nil {
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
					message, err := self.h3Framer.Read(stream)
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
