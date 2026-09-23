package connect

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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	H1FramerProtocol        = "urnetwork-framer/1"
	H1FramerXlProtocol      = "urnetwork-framerxl/1"
	h1UpgradeMaxHeaderBytes = 32 * 1024
	h1UpgradeTimeout        = 5 * time.Second
)

var h1PlusDisabled atomic.Bool

// SetH1PlusDisabled is the process-wide emergency switch. It forces ordinary
// WebSocket whatever the client and server settings, which enable H1+ by
// default.
func SetH1PlusDisabled(disabled bool) { h1PlusDisabled.Store(disabled) }

func H1PlusAvailable() bool { return runtime.GOOS != "js" && !h1PlusDisabled.Load() }

// HTTPUpgradeError contains only bounded protocol classifications, never a URL,
// credential, response body, or peer-supplied status text.
type HTTPUpgradeError struct {
	StatusCode int
	Reason     string
	Terminal   bool
}

func (e *HTTPUpgradeError) Error() string {
	return fmt.Sprintf("HTTP upgrade: %s (status %d)", e.Reason, e.StatusCode)
}

func HTTPUpgradeAllowsFallback(err error) bool {
	var upgradeErr *HTTPUpgradeError
	return errors.As(err, &upgradeErr) && !upgradeErr.Terminal
}

func headerContainsToken(h http.Header, name, token string) bool {
	for _, line := range h.Values(name) {
		for _, part := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// IsFramedUpgrade recognizes an offered custom protocol, including malformed
// multiple selections, so callers reject those before reaching WebSocket auth.
func IsFramedUpgrade(r *http.Request, protocol string) bool {
	return headerContainsToken(r.Header, "Upgrade", protocol)
}

func exactUpgrade(h http.Header, protocol string) bool {
	values := h.Values("Upgrade")
	return len(values) == 1 && strings.TrimSpace(values[0]) == protocol
}

func supportedFramedProtocol(protocol string) bool {
	return protocol == H1FramerProtocol || protocol == H1FramerXlProtocol
}

func ValidateFramedUpgradeRequest(r *http.Request, protocol string) error {
	if !supportedFramedProtocol(protocol) || r.Method != http.MethodGet || r.ProtoMajor != 1 || r.ProtoMinor != 1 ||
		!exactUpgrade(r.Header, protocol) || !headerContainsToken(r.Header, "Connection", "upgrade") {
		return &HTTPUpgradeError{Reason: "invalid-request", StatusCode: http.StatusBadRequest}
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 || len(r.Header.Values("Transfer-Encoding")) != 0 ||
		len(r.Header.Values("Content-Length")) > 1 || r.Header.Get("Expect") != "" || len(r.Trailer) != 0 {
		return &HTTPUpgradeError{Reason: "request-body", StatusCode: http.StatusBadRequest}
	}
	if headerSize(r.Header)+len(r.RequestURI)+len(r.Host) > h1UpgradeMaxHeaderBytes {
		return &HTTPUpgradeError{Reason: "headers-too-large", StatusCode: http.StatusRequestHeaderFieldsTooLarge}
	}
	return nil
}

func headerSize(header http.Header) int {
	n := 0
	for name, values := range header {
		for _, value := range values {
			n += len(name) + len(value) + 4
		}
	}
	return n
}

// AcceptFramedUpgrade MUST be called only after the endpoint's authentication,
// revocation, membership and admission checks. It preserves bytes prefetched by
// net/http and clears its deadlines after a bounded 101 write.
func AcceptFramedUpgrade(w http.ResponseWriter, r *http.Request, protocol string, timeout time.Duration) (net.Conn, error) {
	if err := ValidateFramedUpgradeRequest(r, protocol); err != nil {
		return nil, err
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("HTTP upgrade: hijacking unavailable")
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 || h1UpgradeTimeout < timeout {
		timeout = h1UpgradeTimeout
	}
	if err = conn.SetWriteDeadline(time.Now().Add(timeout)); err == nil {
		_, err = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", protocol)
	}
	if err == nil {
		err = buffered.Flush()
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &httpUpgradeConn{Conn: conn, reader: buffered.Reader}, nil
}

// httpUpgradeConn releases the handshake buffer as soon as its prefetched bytes
// have been consumed. It has exactly one reader; writes can run independently.
type httpUpgradeConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *httpUpgradeConn) Read(p []byte) (int, error) {
	if c.reader != nil {
		if c.reader.Buffered() != 0 {
			return c.reader.Read(p)
		}
		c.reader = nil
	}
	return c.Conn.Read(p)
}

type upgradeHeaderReader struct {
	reader    io.Reader
	remaining int
}

func (r *upgradeHeaderReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, &HTTPUpgradeError{Reason: "headers-too-large"}
	}
	if 0 < r.remaining && r.remaining < len(p) {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	if 0 < r.remaining {
		r.remaining -= n
	}
	return n, err
}

// DialFramedUpgrade uses the same native H1 socket/TLS dial boundary as Gorilla.
// It never follows redirects, sends application bytes, or converts a failed
// upgrade socket into another carrier. Rejected connections are always closed.
func DialFramedUpgrade(ctx context.Context, address string, header http.Header, dialer *websocket.Dialer, protocol string) (net.Conn, error) {
	if !H1PlusAvailable() || !supportedFramedProtocol(protocol) {
		return nil, &HTTPUpgradeError{Reason: "unavailable"}
	}
	if dialer == nil {
		return nil, &HTTPUpgradeError{Reason: "missing-dialer", Terminal: true}
	}
	// Do not bypass a configured HTTP proxy. Until custom CONNECT negotiation
	// is supported, the ordinary Gorilla path remains responsible for it.
	if dialer.Proxy != nil {
		return nil, &HTTPUpgradeError{Reason: "http-proxy"}
	}
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return nil, &HTTPUpgradeError{Reason: "invalid-url", Terminal: true}
	}
	secure := u.Scheme == "wss" || u.Scheme == "https"
	if !secure && u.Scheme != "ws" && u.Scheme != "http" {
		return nil, &HTTPUpgradeError{Reason: "invalid-scheme", Terminal: true}
	}
	if secure {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	if headerSize(header)+len(u.RequestURI())+len(u.Host)+128 > h1UpgradeMaxHeaderBytes {
		return nil, &HTTPUpgradeError{Reason: "headers-too-large", Terminal: true}
	}
	timeout := h1UpgradeTimeout
	if 0 < dialer.HandshakeTimeout {
		timeout = min(timeout, dialer.HandshakeTimeout)
	}
	// Leave time for a fresh fallback within the existing overall deadline.
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)/2)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hostPort := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}
		hostPort = net.JoinHostPort(u.Hostname(), port)
	}
	plainDial := dialer.NetDialContext
	if plainDial == nil {
		plainDial = (&net.Dialer{}).DialContext
	}
	var conn net.Conn
	if secure && dialer.NetDialTLSContext != nil {
		conn, err = dialer.NetDialTLSContext(attemptCtx, "tcp", hostPort)
	} else {
		conn, err = plainDial(attemptCtx, "tcp", hostPort)
		if err == nil && secure {
			config := &tls.Config{MinVersion: tls.VersionTLS12}
			if dialer.TLSClientConfig != nil {
				config = dialer.TLSClientConfig.Clone()
			}
			if config.ServerName == "" {
				config.ServerName = u.Hostname()
			}
			config.NextProtos = []string{"http/1.1"}
			tlsConn := tls.Client(conn, config)
			err = tlsConn.HandshakeContext(attemptCtx)
			conn = tlsConn
		}
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	canceled := make(chan struct{})
	stop := context.AfterFunc(attemptCtx, func() { conn.Close(); close(canceled) })
	disarmed := false
	defer func() {
		if !disarmed && !stop() {
			<-canceled
		}
	}()
	if deadline, ok := attemptCtx.Deadline(); ok {
		if err = conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	request := &http.Request{Method: http.MethodGet, URL: u, Host: u.Host, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: header.Clone()}
	if request.Header == nil {
		request.Header = http.Header{}
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", protocol)
	request.Header.Del("Content-Length")
	request.Header.Del("Transfer-Encoding")
	if err = request.Write(conn); err != nil {
		return nil, err
	}
	limited := &upgradeHeaderReader{reader: conn, remaining: h1UpgradeMaxHeaderBytes}
	buffered := bufio.NewReaderSize(limited, 4096)
	response, err := http.ReadResponse(buffered, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var networkError net.Error
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &networkError) {
			// An intermediary may close an unknown upgrade, so allow one fresh
			// WS attempt. This is not persistent protocol-capability evidence.
			return nil, &HTTPUpgradeError{Reason: "response-io"}
		}
		return nil, &HTTPUpgradeError{Reason: "invalid-response"}
	}
	// No response body is drained: it can be unbounded or an upgraded socket.
	if response.StatusCode != http.StatusSwitchingProtocols {
		terminal := response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode/100 == 3
		return nil, &HTTPUpgradeError{StatusCode: response.StatusCode, Reason: "rejected", Terminal: terminal}
	}
	if response.ProtoMajor != 1 || response.ProtoMinor != 1 || !exactUpgrade(response.Header, protocol) ||
		!headerContainsToken(response.Header, "Connection", "upgrade") || response.ContentLength > 0 || len(response.TransferEncoding) != 0 ||
		len(response.Header.Values("Content-Length")) != 0 || len(response.Header.Values("Transfer-Encoding")) != 0 {
		return nil, &HTTPUpgradeError{StatusCode: 101, Reason: "invalid-selection"}
	}
	if !stop() {
		<-canceled
		disarmed = true
		return nil, attemptCtx.Err()
	}
	disarmed = true
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	limited.remaining = -1
	success = true
	return &httpUpgradeConn{Conn: conn, reader: buffered}, nil
}

// DialH1Messages tries the authenticated custom carrier once and falls back to
// a newly dialed, normally validated masked WebSocket only on capability
// failure. The common deadline bounds both attempts. Application bytes are
// never replayed, and auth/TLS failures never trigger downgrade.
func DialH1Messages(ctx context.Context, address string, header http.Header, dialer *websocket.Dialer, protocol string, maximum int, enabled bool, stats *H1PlusStats) (H1MessageConn, error) {
	if dialer == nil {
		return nil, errors.New("missing H1 dialer")
	}
	if dialer.HandshakeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialer.HandshakeTimeout)
		defer cancel()
	}
	if enabled && FramedUpgradePermitted(address, protocol) {
		start := time.Now()
		conn, err := DialFramedUpgrade(ctx, address, header, dialer, protocol)
		RecordH1PlusSelection(stats, time.Since(start), err)
		if err == nil {
			framed, frameErr := NewFramedMessageConn(conn, protocol, maximum, stats)
			if frameErr != nil {
				conn.Close()
			}
			return framed, frameErr
		}
		if !HTTPUpgradeAllowsFallback(err) {
			return nil, err
		}
		RecordFramedUpgradeFailure(address, protocol, err)
	}
	ws, response, err := dialer.DialContext(ctx, address, header)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, err
	}
	ws.SetReadLimit(int64(maximum))
	if stats != nil {
		stats.webSocketSelected.Add(1)
	}
	return ws, nil
}

// Only unsupported protocol evidence is cached. Authentication, certificate,
// cancellation and network failures never poison capability or exit reputation.
var framedUpgradeCache = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

func framedUpgradeCacheKey(address, protocol string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	key := strings.ToLower(u.Scheme+"://"+u.Host) + " " + protocol
	if len(key) > 512 {
		return ""
	}
	return key
}

func FramedUpgradePermitted(address, protocol string) bool {
	if !H1PlusAvailable() {
		return false
	}
	key := framedUpgradeCacheKey(address, protocol)
	if key == "" {
		return true
	}
	framedUpgradeCache.Lock()
	defer framedUpgradeCache.Unlock()
	until := framedUpgradeCache.until[key]
	if until.Before(time.Now()) {
		delete(framedUpgradeCache.until, key)
		return true
	}
	return false
}

func RecordFramedUpgradeFailure(address, protocol string, err error) {
	if !HTTPUpgradeAllowsFallback(err) {
		return
	}
	var upgradeErr *HTTPUpgradeError
	if !errors.As(err, &upgradeErr) {
		return
	}
	switch upgradeErr.Reason {
	case "invalid-selection", "invalid-response":
	case "rejected":
		switch upgradeErr.StatusCode {
		case 200, 400, 404, 405, 426, 501:
		default:
			return
		}
	default:
		return
	}
	key := framedUpgradeCacheKey(address, protocol)
	if key == "" {
		return
	}
	framedUpgradeCache.Lock()
	defer framedUpgradeCache.Unlock()
	if len(framedUpgradeCache.until) >= 256 {
		clear(framedUpgradeCache.until)
	}
	framedUpgradeCache.until[key] = time.Now().Add(5 * time.Minute)
}
