package connect

// vless_dial.go — one VLESS stream end to end, and the dial functions a client
// strategy dialer is built from (net_http.go).
//
// The tcp connection to the VLESS server goes through the strategy's own
// dial (`ConnectSettings.DialContext`), so the egress binding, resolver and
// any socks proxy apply to it exactly as to a direct dial. The destination
// name travels inside the VLESS request and is resolved by the server, which
// is what lets a VLESS dialer reach a host whose dns is blocked.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
)

// The user agent of the http transports' upgrade request: a current desktop
// browser, as other VLESS clients send.
const vlessUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

// DialVless opens one tcp stream to address (host:port) through the VLESS
// server of config. The returned connection carries the inner bytes only:
// the request header goes out with the first write and the response header
// is read off before the first read returns.
func DialVless(
	ctx context.Context,
	connectSettings *ConnectSettings,
	config *VlessConfig,
	network string,
	address string,
) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("vless: cannot carry %s", network)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("vless: destination port %q", portText)
	}
	userId, err := vlessId(config.Id)
	if err != nil {
		return nil, &VlessConfigError{Code: VlessErrorIdInvalid}
	}
	requestHeader, err := vlessRequestHeader(userId, config.Flow, host, uint16(port))
	if err != nil {
		return nil, err
	}

	serverConn, err := connectSettings.DialContext(ctx, "tcp", config.serverAddress())
	if err != nil {
		return nil, err
	}

	conn := serverConn
	var recordConn *vlessRecordConn
	if config.Flow == VlessFlowVision {
		recordConn = newVlessRecordConn(serverConn)
		conn = recordConn
	}

	network = config.network()
	httpTransport := network == VlessNetworkWs || network == VlessNetworkHttpUpgrade

	// each step closes the connection it was handed on its own error
	switch config.security() {
	case VlessSecurityTls:
		conn, err = vlessTlsClient(ctx, conn, config, httpTransport, connectSettings.TlsTimeout)
	case VlessSecurityReality:
		conn, err = vlessRealityClient(ctx, conn, config, connectSettings.TlsTimeout)
	}
	if err != nil {
		return nil, err
	}
	switch network {
	case VlessNetworkWs:
		conn, err = vlessWebSocketClient(ctx, conn, config, connectSettings.HandshakeTimeout)
	case VlessNetworkHttpUpgrade:
		conn, err = vlessHttpUpgradeClient(ctx, conn, config, connectSettings.HandshakeTimeout)
	}
	if err != nil {
		return nil, err
	}

	if config.Flow == VlessFlowVision {
		return newVlessVisionConn(conn, recordConn, userId, requestHeader), nil
	}
	return newVlessConn(conn, requestHeader), nil
}

// The plain dial of a VLESS strategy dialer: the stream itself, for http://
// and ws:// destinations.
func newVlessDialContext(connectSettings *ConnectSettings, config *VlessConfig) DialContextFunction {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		return DialVless(ctx, connectSettings, config, network, address)
	}
}

// The tls dial of a VLESS strategy dialer: the destination's own tls on top
// of the stream, verified as on every other dialer.
func newVlessDialTlsContext(
	connectSettings *ConnectSettings,
	config *VlessConfig,
	nextProtos []string,
) DialTlsContextFunction {
	dialContext := newVlessDialContext(connectSettings, config)
	innerBaseTlsConfig := newClientTlsConfig(connectSettings.TlsConfig, nextProtos)
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		streamConn, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		innerTlsConfig := innerBaseTlsConfig.Clone()
		if innerTlsConfig.ServerName == "" {
			innerTlsConfig.ServerName = host
		}
		tlsConn := tls.Client(streamConn, innerTlsConfig)
		// bounded so a slow or hostile VLESS server cannot hold the dial open
		innerCtx, innerCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
		defer innerCancel()
		if err := tlsConn.HandshakeContext(innerCtx); err != nil {
			tlsConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
}

// The outer tls of the tls security. Without a fingerprint this is the Go tls
// client; with one, the imitated browser hello. The http transports offer
// only http/1.1 unless the configuration names an alpn, because their
// upgrade is an http/1.1 request.
func vlessTlsClient(
	ctx context.Context,
	conn net.Conn,
	config *VlessConfig,
	httpTransport bool,
	handshakeTimeout time.Duration,
) (net.Conn, error) {
	alpns := config.Alpns
	if len(alpns) == 0 && httpTransport {
		alpns = []string{"http/1.1"}
	}
	// an ip literal is verified against the certificate's ip names and sends
	// no sni, which is what the Go client does with an ip server name
	serverName := config.ServerName
	if serverName == "" {
		serverName = config.Address
	}
	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer handshakeCancel()

	if config.Fingerprint == "" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         serverName,
			NextProtos:         alpns,
			InsecureSkipVerify: config.AllowInsecure,
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			tlsConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}

	helloId, err := vlessClientHelloId(config.Fingerprint)
	if err != nil {
		conn.Close()
		return nil, err
	}
	uconn := utls.UClient(conn, &utls.Config{
		ServerName:         serverName,
		NextProtos:         alpns,
		InsecureSkipVerify: config.AllowInsecure,
	}, helloId)
	if 0 < len(alpns) {
		// the imitated hello carries its browser's alpn list; replace it, the
		// way other clients do for the http transports
		if err := uconn.BuildHandshakeState(); err != nil {
			uconn.Close()
			return nil, err
		}
		replaced := false
		for _, extension := range uconn.Extensions {
			if alpnExtension, ok := extension.(*utls.ALPNExtension); ok {
				alpnExtension.AlpnProtocols = alpns
				replaced = true
				break
			}
		}
		if !replaced {
			uconn.Extensions = append(uconn.Extensions, &utls.ALPNExtension{AlpnProtocols: alpns})
		}
		if err := uconn.BuildHandshakeState(); err != nil {
			uconn.Close()
			return nil, err
		}
	}
	if err := uconn.HandshakeContext(handshakeCtx); err != nil {
		uconn.Close()
		return nil, err
	}
	return uconn, nil
}

// The request path and query of the http transports. An `ed` (early data)
// query parameter is a client-side setting of other clients and is not sent:
// the server takes the stream without early data.
func vlessHttpRequestUrl(config *VlessConfig) *url.URL {
	path := config.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	requestUrl, err := url.Parse(path)
	if err != nil {
		return &url.URL{Path: path}
	}
	if requestUrl.RawQuery != "" {
		query := requestUrl.Query()
		query.Del("ed")
		requestUrl.RawQuery = query.Encode()
	}
	return &url.URL{Path: requestUrl.Path, RawPath: requestUrl.RawPath, RawQuery: requestUrl.RawQuery}
}

// Applies the handshake deadline of the http transports to conn: the
// configured timeout, the context's deadline, and an immediate deadline
// when the context ends first. The returned function clears it.
func vlessHandshakeDeadline(ctx context.Context, conn net.Conn, handshakeTimeout time.Duration) func() {
	deadline := time.Time{}
	if 0 < handshakeTimeout {
		deadline = time.Now().Add(handshakeTimeout)
	}
	if ctxDeadline, ok := ctx.Deadline(); ok && (deadline.IsZero() || ctxDeadline.Before(deadline)) {
		deadline = ctxDeadline
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() {
		conn.SetDeadline(time.Now())
	})
	return func() {
		stop()
		conn.SetDeadline(time.Time{})
	}
}

// The ws transport: a websocket upgrade over conn, then one binary message per
// write.
func vlessWebSocketClient(
	ctx context.Context,
	conn net.Conn,
	config *VlessConfig,
	handshakeTimeout time.Duration,
) (net.Conn, error) {
	requestUrl := vlessHttpRequestUrl(config)
	requestUrl.Scheme = "ws"
	requestUrl.Host = config.httpHost()

	dialed := false
	dialer := &websocket.Dialer{
		// the upgrade runs over the connection already made, never a new one
		NetDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			if dialed {
				return nil, errors.New("vless: websocket dialed twice")
			}
			dialed = true
			return conn, nil
		},
		HandshakeTimeout: handshakeTimeout,
	}
	header := http.Header{}
	header.Set("User-Agent", vlessUserAgent)
	wsConn, response, err := dialer.DialContext(ctx, requestUrl.String(), header)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return newVlessWebSocketConn(wsConn), nil
}

// A websocket as a byte stream: each write is one binary message, and reads
// run across message boundaries.
//
// Read and Write may be called concurrently with each other; concurrent
// writes are serialized.
type vlessWebSocketConn struct {
	wsConn *websocket.Conn

	writeLock sync.Mutex

	// the reading goroutine's current message
	reader io.Reader
}

func newVlessWebSocketConn(wsConn *websocket.Conn) *vlessWebSocketConn {
	return &vlessWebSocketConn{
		wsConn: wsConn,
	}
}

func (self *vlessWebSocketConn) Read(b []byte) (int, error) {
	for {
		if self.reader == nil {
			messageType, reader, err := self.wsConn.NextReader()
			if err != nil {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					return 0, io.EOF
				}
				return 0, err
			}
			if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
				continue
			}
			self.reader = reader
		}
		n, err := self.reader.Read(b)
		if errors.Is(err, io.EOF) {
			self.reader = nil
			if 0 < n {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (self *vlessWebSocketConn) Write(b []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if err := self.wsConn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (self *vlessWebSocketConn) Close() error {
	return self.wsConn.Close()
}

func (self *vlessWebSocketConn) LocalAddr() net.Addr {
	return self.wsConn.LocalAddr()
}

func (self *vlessWebSocketConn) RemoteAddr() net.Addr {
	return self.wsConn.RemoteAddr()
}

func (self *vlessWebSocketConn) SetDeadline(t time.Time) error {
	if err := self.wsConn.SetReadDeadline(t); err != nil {
		return err
	}
	return self.wsConn.SetWriteDeadline(t)
}

func (self *vlessWebSocketConn) SetReadDeadline(t time.Time) error {
	return self.wsConn.SetReadDeadline(t)
}

func (self *vlessWebSocketConn) SetWriteDeadline(t time.Time) error {
	return self.wsConn.SetWriteDeadline(t)
}

// The httpupgrade transport: an http/1.1 upgrade request over conn, after
// which the connection is the raw stream, with no websocket framing.
func vlessHttpUpgradeClient(
	ctx context.Context,
	conn net.Conn,
	config *VlessConfig,
	handshakeTimeout time.Duration,
) (net.Conn, error) {
	clearDeadline := vlessHandshakeDeadline(ctx, conn, handshakeTimeout)
	defer clearDeadline()

	request := &http.Request{
		Method:     http.MethodGet,
		URL:        vlessHttpRequestUrl(config),
		Host:       config.httpHost(),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
	}
	request.Header.Set("User-Agent", vlessUserAgent)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	if err := request.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		conn.Close()
		return nil, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!strings.EqualFold(response.Header.Get("Upgrade"), "websocket") ||
		!strings.EqualFold(response.Header.Get("Connection"), "upgrade") {
		conn.Close()
		return nil, fmt.Errorf("vless: httpupgrade answered %s", response.Status)
	}
	return &vlessBufferedConn{
		Conn:   conn,
		reader: reader,
	}, nil
}

// A connection whose first reads come from a reader that may already hold
// bytes read past an http response.
type vlessBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (self *vlessBufferedConn) Read(b []byte) (int, error) {
	return self.reader.Read(b)
}
