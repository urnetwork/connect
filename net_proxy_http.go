package connect

// HTTP CONNECT proxying (sidecar C8): a stream dial carried through a proxy
// that speaks RFC 9110 §9.3.6's CONNECT, for a host whose only way out is an
// HTTPS proxy, as a cloud agent's sandbox is. ProxySettings.Protocol selects
// it, and the zero value stays SOCKS5, so every setting written before it
// dials exactly as it did. A host reads its environment (HTTPS_PROXY) itself
// and fills ProxySettings: nothing here reads the environment.
//
// The proxy resolves the target's name, so the CONNECT names the host the
// caller dialed and the tls above it keeps that name as its sni. Only tcp is
// carried: a datagram dial through a proxy refuses, and never goes direct.
// A proxy's refusal, its 407 included, is a dial error that names only its
// status, never the target, the proxy or a credential.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// How a ProxySettings speaks to its proxy.
type ProxyProtocol string

const (
	// RFC 1928, with Auth as its username and password. What an empty
	// Protocol means.
	ProxyProtocolSocks5 ProxyProtocol = "socks5"
	// RFC 9110 §9.3.6 over a plain tcp connection to the proxy, with Auth as
	// Basic credentials in Proxy-Authorization.
	ProxyProtocolHttpConnect ProxyProtocol = "http_connect"
)

// The most of a CONNECT response's status line and headers a dial reads
// before it gives up on the proxy, the bound connect's own upgrade reads
// (http_upgrade.go). A format guard, not a tunable: a proxy that answers
// more is broken or hostile.
const httpConnectMaxResponseHeaderByteCount = 32 * 1024

// A CONNECT the proxy did not establish. It carries a bounded classification
// and the proxy's status, never the target, the proxy's address, a
// credential or anything the proxy wrote.
type HttpConnectProxyError struct {
	// The proxy's status, 0 when it sent none that parsed.
	StatusCode int
	// network, auth, refused, invalid-response or invalid-target.
	Reason string
}

func (self *HttpConnectProxyError) Error() string {
	return fmt.Sprintf("http connect proxy: %s (status %d)", self.Reason, self.StatusCode)
}

// Whether the proxy asked for credentials it was not given, or refused the
// ones it was: a 407 (RFC 9110 §15.5.8).
func (self *HttpConnectProxyError) AuthenticationRequired() bool {
	return self.StatusCode == http.StatusProxyAuthRequired
}

// The dial of one ProxySettings. exchangeTimeout bounds the connection to the
// proxy and its CONNECT exchange together, as ConnectTimeout bounds a direct
// connect; zero or less leaves only ctx to bound them. A SOCKS5 dial reads
// only ctx, as it always has.
func (self *ProxySettings) newDialContext(forward proxy.Dialer, exchangeTimeout time.Duration) DialContextFunction {
	switch self.Protocol {
	case "", ProxyProtocolSocks5:
		return self.newSocks5DialContext(forward)
	case ProxyProtocolHttpConnect:
		return func(ctx context.Context, network string, addr string) (net.Conn, error) {
			return self.dialHttpConnect(ctx, forward, exchangeTimeout, network, addr)
		}
	default:
		// an unknown protocol never falls back to a direct dial
		protocol := self.Protocol
		return func(ctx context.Context, network string, addr string) (net.Conn, error) {
			return nil, fmt.Errorf("unknown proxy protocol %q", protocol)
		}
	}
}

// One dial through an HTTP CONNECT proxy: connect to the proxy, ask it for
// addr, and hand back the tunnel. Every failure closes what it opened.
func (self *ProxySettings) dialHttpConnect(
	ctx context.Context,
	forward proxy.Dialer,
	exchangeTimeout time.Duration,
	network string,
	addr string,
) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// CONNECT carries a byte stream; a datagram never goes through the
		// proxy, and never goes around it here either
		return nil, &HttpConnectProxyError{Reason: "network"}
	}
	if !isHttpConnectTarget(addr) {
		return nil, &HttpConnectProxyError{Reason: "invalid-target"}
	}
	if 0 < exchangeTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, exchangeTimeout)
		defer cancel()
	}

	proxyNetwork := self.Network
	if proxyNetwork == "" {
		proxyNetwork = "tcp"
	}
	var conn net.Conn
	var err error
	if contextDialer, ok := forward.(proxy.ContextDialer); ok {
		conn, err = contextDialer.DialContext(ctx, proxyNetwork, self.Address)
	} else {
		conn, err = forward.Dial(proxyNetwork, self.Address)
	}
	if err != nil {
		return nil, err
	}

	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	// a cancel unblocks the exchange's reads and writes; a deadline bounds them
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(canceled)
		conn.SetDeadline(time.Unix(1, 0))
	})
	stopped := false
	defer func() {
		if !stopped && !stop() {
			<-canceled
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	request := &bytes.Buffer{}
	fmt.Fprintf(request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if self.Auth != nil {
		credentials := base64.StdEncoding.EncodeToString([]byte(self.Auth.User + ":" + self.Auth.Password))
		fmt.Fprintf(request, "Proxy-Authorization: Basic %s\r\n", credentials)
	}
	request.WriteString("\r\n")
	if _, err := conn.Write(request.Bytes()); err != nil {
		return nil, errors.Join(err, ctx.Err())
	}

	limited := &io.LimitedReader{R: conn, N: httpConnectMaxResponseHeaderByteCount}
	buffered := bufio.NewReaderSize(limited, 4096)
	response, err := http.ReadResponse(buffered, &http.Request{Method: http.MethodConnect})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(err, ctxErr)
		}
		var netErr net.Error
		if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, err
		}
		return nil, &HttpConnectProxyError{Reason: "invalid-response"}
	}
	// no body is read: a refusal's may be unbounded, and a tunnel has none
	if response.StatusCode == http.StatusProxyAuthRequired {
		return nil, &HttpConnectProxyError{StatusCode: response.StatusCode, Reason: "auth"}
	}
	if response.StatusCode/100 != 2 {
		return nil, &HttpConnectProxyError{StatusCode: response.StatusCode, Reason: "refused"}
	}

	if !stop() {
		// the cancel has run or is running: its past deadline holds the conn
		<-canceled
		stopped = true
		return nil, errors.Join(context.Canceled, ctx.Err())
	}
	stopped = true
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	success = true
	if 0 < buffered.Buffered() {
		// bytes the proxy sent past its response belong to the tunnel
		peeked, _ := buffered.Peek(buffered.Buffered())
		return &httpConnectConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(bytes.Clone(peeked)), conn)}, nil
	}
	return conn, nil
}

// Whether addr is a host:port a request line may carry: a port, and no
// space, control character or line break that could split the request.
func isHttpConnectTarget(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return false
	}
	return !strings.ContainsFunc(addr, func(r rune) bool {
		return r <= ' ' || r == 0x7f
	})
}

// A tunnel whose first bytes the CONNECT response's read had already taken.
type httpConnectConn struct {
	net.Conn
	reader io.Reader
}

func (self *httpConnectConn) Read(p []byte) (int, error) {
	return self.reader.Read(p)
}
