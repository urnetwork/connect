package connect

// http3 platform transport
// versus the default platform transport, h3 is:
// - more cpu efficient.
//   The quic stream does not need to mask/unmask each byte before TLS.
// - faster to connect.
//   The quic stream uses 0rtt connect when possible to speed up the initial connection.
// - better throughput on poort networks.
//   quic optimizes congestion control to better handle poor network conditions.
// However, h3 is not available in all locations due to dpi/filtering.
// When available, it takes precedence over the default transport.

// FIXME merge h3 into the normal platform transport:
// - connect to both h1 and h3 in parallel
// - both share one route and internal mode decides which transport is used to send.
// - both transports receive .
//   the non primary transport should send a drain message to the server to remove weight for the transport
// - if a transport has not received a message in N seconds and is not the primary transport,
//   close the transport until there is no primary again

type H3PlatformTransportSettings struct {
	HttpConnectTimeout time.Duration
	WsHandshakeTimeout time.Duration
	AuthTimeout        time.Duration
	ReconnectTimeout   time.Duration
	PingTimeout        time.Duration
	WriteTimeout       time.Duration
	ReadTimeout        time.Duration
	TransportGenerator func() (sendTransport Transport, receiveTransport Transport)
}

func H3DefaultPlatformTransportSettings() *H3PlatformTransportSettings {
	pingTimeout := 1 * time.Second
	return &H3PlatformTransportSettings{
		HttpConnectTimeout: 2 * time.Second,
		WsHandshakeTimeout: 2 * time.Second,
		AuthTimeout:        2 * time.Second,
		ReconnectTimeout:   5 * time.Second,
		PingTimeout:        pingTimeout,
		WriteTimeout:       5 * time.Second,
		ReadTimeout:        15 * time.Second,
	}
}

func NewH3PlatformTransportWithDefaults(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
) *H3PlatformTransport {
	return NewH3PlatformTransport(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		DefaultH3PlatformTransportSettings(),
	)
}

func NewH3PlatformTransport(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	settings *PlatformTransportSettings,
) *H3PlatformTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &H3PlatformTransport{
		ctx:            cancelCtx,
		cancel:         cancel,
		clientStrategy: clientStrategy,
		routeManager:   routeManager,
		platformUrl:    platformUrl,
		auth:           auth,
		settings:       settings,
	}
	go transport.run()
	return transport
}

func (self *H3PlatformTransport) run() {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.auth.ClientId()

	// FIXME move auth into headers to speed up connect
	// Authorization: bearer
	// X-UR-AppVersion
	// X-UR-InstanceId
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
		connect := func() (*http3.Transport, *http3.RequestStream, error) {

			quicConfig := &quic.Config{
				HandshakeIdleTimeout: connectSettings.ConnectTimeout + connectSettings.TlsTimeout + connectSettings.HandshakeTimeout,
				Allow0RTT:            true,
				KeepAlivePeriod:      X,
				MaxIdleTimeout:       X,
				// FIXME other settigns?

				InitialStreamReceiveWindow:     initialStreamReceiveWindow,
				MaxStreamReceiveWindow:         maxStreamReceiveWindow,
				InitialConnectionReceiveWindow: initialConnectionReceiveWindow,
				MaxConnectionReceiveWindow:     maxConnectionReceiveWindow,
				AllowConnectionWindowIncrease:  config.AllowConnectionWindowIncrease,
				EnableDatagrams:                config.EnableDatagrams,
				InitialPacketSize:              initialPacketSize,
			}

			tlsConfig := &tls.Config{
				ServerName:         extenderConfig.Profile.ServerName,
				InsecureSkipVerify: true,
				// require 1.3 to mask self-signed certs
				MinVersion: tls.VersionTLS13,
			}

			quicConfig := &quic.Config{
				HandshakeIdleTimeout: connectSettings.ConnectTimeout + connectSettings.TlsTimeout + connectSettings.HandshakeTimeout,
			}
			quicConn, err := quic.DialAddr(ctx, authority, tlsConfig, quicConfig)
			if err != nil {
				return nil, err
			}

			h3Transport := &http3.Transport{
				TLSClientConfig: tlsConfig,
				QUICConfig:      quicConfig,
				EnableDatagrams: quicConfig.EnableDatagrams,
			}

			conn := h3Transport.NewClientConn(quicConn)
			stream, err := conn.OpenRequestStream()
			if err != nil {
				return nil, err
			}

			success := false
			defer func() {
				if !success {
					h3Transport.Close()
				}
			}()

			ws.SetWriteDeadline(time.Now().Add(self.settings.AuthTimeout))
			if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
				return nil, err
			}
			ws.SetReadDeadline(time.Now().Add(self.settings.AuthTimeout))
			if messageType, message, err := ws.ReadMessage(); err != nil {
				return nil, err
			} else {
				// verify the auth echo
				switch messageType {
				case websocket.BinaryMessage:
					if !bytes.Equal(authBytes, message) {
						return nil, fmt.Errorf("Auth response error: bad bytes.")
					}
				default:
					return nil, fmt.Errorf("Auth response error.")
				}
			}

			success = true
			return ws, nil
		}

		// quic

		transport

		t.NewClientConnection

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

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			send := make(chan []byte, TransportBufferSize)
			receive := make(chan []byte, TransportBufferSize)

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewPrioritySendGatewayTransport(0, 1.0)
				receiveTransport = NewReceiveGatewayTransport(0, 1.0)
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

						ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
						if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							glog.Infof("[ts]%s-> error = %s\n", clientId, err)
							return
						}
						glog.V(2).Infof("[ts]%s->\n", clientId)
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

func (self *H3PlatformTransport) Close() {
	self.cancel()
}

// type streamConn struct {
// 	stream quic.Stream
// }

// func newStreamConn(stream quic.Stream) *streamConn {
// 	return &streamConn{
// 		stream: stream,
// 	}
// }

// func (self *streamConn) Read(b []byte) (int, error) {
// 	return self.stream.Read(b)
// }

// func (self *streamConn) Write(b []byte) (int, error) {
// 	return self.stream.Write(b)
// }

// func (self *streamConn) Close() error {
// 	return self.stream.Close()
// }

// func (self *streamConn) LocalAddr() net.Addr {
// 	return nil
// }

// func (self *streamConn) RemoteAddr() net.Addr {
// 	return nil
// }

// func (self *streamConn) SetDeadline(t time.Time) error {
// 	return self.stream.SetDeadline(t)
// }

// func (self *streamConn) SetReadDeadline(t time.Time) error {
// 	return self.stream.SetReadDeadline(t)
// }

// func (self *streamConn) SetWriteDeadline(t time.Time) error {
// 	return self.stream.SetWriteDeadline(t)
// }
