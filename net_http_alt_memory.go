package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http/httpguts"
)

const (
	altMaxUploadBytes = 64 * 1024
	altMaxHeaderBytes = 16 * 1024
)

var errAltRequestMemory = errors.New("alt HTTP request exceeds its retained upload budget")
var errAltStoppedUpload = errors.New("alt HTTP upload ended by normal stream cancellation")

// AltQuicUnsupportedRequestError lets a strategy select another carrier before
// opening an alt socket or consuming the request body. Alt serves bounded API
// exchanges, not CONNECT tunnels. The public HTTP/3 RequestStream API cannot
// write request trailers, so those requests also take another carrier.
type AltQuicUnsupportedRequestError struct{ Feature string }

func (self *AltQuicUnsupportedRequestError) Error() string {
	return "alt HTTP/3 carrier does not support " + self.Feature
}

type altQuicConnection struct {
	authority string
	conn      *quic.Conn
	client    *http3.ClientConn
	active    bool
}

// The fixed authority pool uses explicit RequestStreams, not the opaque
// Transport.RoundTrip writer. Registering before SendRequestHeader makes
// headers, body, and FIN synchronously cancellable by the send-root guard.
// HTTP/3 still owns framing, response decoding, gzip, response trailers, and
// connection-control handling. One request remains active through Body.Close
// and FIN or RESET_STREAM acknowledgment. http.Client owns redirects;
// ClientStrategy owns retries, with no implicit replay of API mutations here.
// Request trailers, CONNECT, and explicit 0-RTT pseudo-methods are refused.
// Full httptrace transport-lifecycle parity is not provided; the API callers
// use none of these features. RequestStream's own tracing hooks still run.
type altQuicBoundedTransport struct {
	transport   *http3.Transport
	slot        chan struct{}
	mutex       sync.Mutex
	connections [16]*altQuicConnection
	closed      bool
	dialCancel  context.CancelFunc
}

func newAltQuicBoundedTransport(transport *http3.Transport) *altQuicBoundedTransport {
	return &altQuicBoundedTransport{transport: transport, slot: make(chan struct{}, 1)}
}

func altRequestAuthority(request *http.Request) string {
	port := request.URL.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(request.URL.Hostname()), port)
}

func (self *altQuicBoundedTransport) connection(authority string) *quic.Conn {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, entry := range self.connections {
		if entry != nil && entry.authority == authority {
			return entry.conn
		}
	}
	return nil
}

func (self *altQuicBoundedTransport) acquireConnection(ctx context.Context, request *http.Request) (*altQuicConnection, error) {
	authority := altRequestAuthority(request)
	self.mutex.Lock()
	if self.closed {
		self.mutex.Unlock()
		return nil, http3.ErrTransportClosed
	}
	free := -1
	for i, entry := range self.connections {
		if entry == nil || entry.conn.Context().Err() != nil {
			self.connections[i] = nil
			free = i
		} else if entry.authority == authority {
			entry.active = true
			self.mutex.Unlock()
			return entry, nil
		}
	}
	self.mutex.Unlock()
	if free < 0 {
		return nil, errAltRequestMemory
	}
	tlsConfig := self.transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	tlsConfig = tlsConfig.Clone()
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = request.URL.Hostname()
	}
	tlsConfig.NextProtos = []string{http3.NextProtoH3}
	config := self.transport.QUICConfig
	if config == nil {
		config = &quic.Config{}
	}
	dialCtx, stopDial := context.WithCancel(ctx)
	self.mutex.Lock()
	if self.closed {
		self.mutex.Unlock()
		stopDial()
		return nil, http3.ErrTransportClosed
	}
	self.dialCancel = stopDial
	self.mutex.Unlock()
	conn, err := self.transport.Dial(dialCtx, authority, tlsConfig, config.Clone())
	self.mutex.Lock()
	self.dialCancel = nil
	closed := self.closed
	self.mutex.Unlock()
	stopDial()
	if err != nil {
		if closed {
			return nil, http3.ErrTransportClosed
		}
		return nil, err
	}
	if quicSendFlightForConn(conn) == nil {
		conn.CloseWithError(0, "alt connection missing send owner")
		return nil, errQuicSendFlight
	}
	entry := &altQuicConnection{authority: authority, conn: conn, client: self.transport.NewClientConn(conn), active: true}
	self.mutex.Lock()
	if self.closed {
		self.mutex.Unlock()
		conn.CloseWithError(0, "alt transport closed")
		return nil, http3.ErrTransportClosed
	}
	self.connections[free] = entry
	self.mutex.Unlock()
	// A remote idle timeout or GOAWAY must not leave a closed QUIC object
	// strongly rooted in an otherwise unused pool after its carrier lease ends.
	context.AfterFunc(conn.Context(), func() { self.releaseConnection(entry) })
	return entry, nil
}

func (self *altQuicBoundedTransport) releaseConnection(entry *altQuicConnection) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	entry.active = false
	if entry.conn.Context().Err() != nil {
		for i, current := range self.connections {
			if current == entry {
				self.connections[i] = nil
			}
		}
	}
}

func altRequestFitsHeaders(request *http.Request) bool {
	used := len(request.Method) + len(request.Host) + len(request.URL.String()) + 256
	for _, headers := range []http.Header{request.Header, request.Trailer} {
		for key, values := range headers {
			// Even a zero-value entry owns a key and map metadata.
			used += len(key) + 32
			for _, value := range values {
				used += len(key) + len(value) + 32
				if used > altMaxHeaderBytes {
					return false
				}
			}
		}
	}
	return used <= altMaxHeaderBytes
}

func (self *altQuicBoundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var validation error
	switch {
	case request.URL == nil || request.ContentLength > altMaxUploadBytes:
		validation = errAltRequestMemory
	case len(request.Trailer) > 0 || request.Header.Get("Trailer") != "":
		validation = &AltQuicUnsupportedRequestError{Feature: "request trailers"}
	case request.Method == http.MethodConnect:
		validation = &AltQuicUnsupportedRequestError{Feature: "CONNECT tunnels"}
	case request.Method == http3.MethodGet0RTT || request.Method == http3.MethodHead0RTT:
		validation = &AltQuicUnsupportedRequestError{Feature: "0-RTT requests"}
	case request.URL.Scheme != "https":
		validation = &AltQuicUnsupportedRequestError{Feature: "non-HTTPS requests"}
	case request.RequestURI != "" || request.Method != "" && !httpguts.ValidHeaderFieldName(request.Method):
		validation = fmt.Errorf("invalid alt HTTP request method or RequestURI")
	case !altRequestFitsHeaders(request):
		validation = errAltRequestMemory
	}
	if validation != nil {
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, validation
	}
	select {
	case self.slot <- struct{}{}:
	case <-request.Context().Done():
		if request.Body != nil {
			request.Body.Close()
		}
		return nil, request.Context().Err()
	}
	ctx, cancel := context.WithCancel(request.Context())
	body := &altQuicUploadBody{ReadCloser: request.Body}
	entry, err := self.acquireConnection(ctx, request)
	if err != nil {
		body.Close()
		cancel()
		<-self.slot
		return nil, err
	}
	stream, err := entry.client.OpenRequestStream(ctx)
	if err != nil {
		body.Close()
		entry.conn.CloseWithError(0, "alt stream open failed")
		self.releaseConnection(entry)
		cancel()
		<-self.slot
		return nil, err
	}
	flight := quicSendFlightForConn(entry.conn)
	writer := flight.newWriter(stream) // must precede even the request HEADERS
	var sendFinished atomic.Bool
	// True only when response closure precedes request-context cancellation.
	var responseFinished atomic.Bool
	stopRequest := context.AfterFunc(ctx, func() {
		stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		if !sendFinished.Load() {
			stream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
			body.Close()
		}
	})
	if deadline, ok := ctx.Deadline(); ok {
		stream.SetReadDeadline(deadline)
		writer.SetWriteDeadline(deadline)
	}
	headerRequest := request.Clone(ctx)
	if headerRequest.Method == "" {
		headerRequest.Method = http.MethodGet
	}
	if request.Body != nil && request.Body != http.NoBody {
		headerRequest.Body = http.NoBody
	} else {
		headerRequest.Body = nil
	}
	if err := stream.SendRequestHeader(headerRequest); err != nil {
		stopRequest()
		body.Close()
		entry.conn.CloseWithError(0, "alt request header failed")
		self.releaseConnection(entry)
		flight.retireWriter(writer)
		cancel()
		<-self.slot
		return nil, err
	}
	uploadDone := make(chan struct{})
	var uploadErr error // published by closing uploadDone
	sendBody := func() {
		defer close(uploadDone)
		defer body.Close()
		if request.Body != nil && request.Body != http.NoBody {
			uploadErr = writeAltQuicBody(ctx, writer, body, request)
		}
		var writeErr *altQuicUploadWriteError
		if errors.As(uploadErr, &writeErr) {
			if altUploadSendStopped(ctx, stream.Context(), writeErr.err, responseFinished.Load()) {
				uploadErr = errAltStoppedUpload
			} else {
				uploadErr = writeErr.err
			}
		} else if responseFinished.Load() && errors.Is(uploadErr, context.Canceled) &&
			altUploadStoppedNormally(ctx, stream.Context(), true) {
			// Body.Close must unblock a pending Read. Its resulting cancellation
			// is intentional only after this response was explicitly closed.
			uploadErr = errAltStoppedUpload
		}
		if uploadErr == nil {
			uploadErr = stream.Close()
			// quic-go's Close returns an untyped error after STOP_SENDING.
			// Only the actual stream cause can identify that normal send stop.
			if uploadErr != nil && altUploadStoppedNormally(ctx, stream.Context(), responseFinished.Load()) {
				uploadErr = errAltStoppedUpload
			}
		}
		if errors.Is(uploadErr, errAltStoppedUpload) {
			// A stopped request upload does not cancel the peer's response.
			// A credit wait can observe the stop before calling QUIC Write;
			// acknowledge it to quic-go so RESET ACK can retire the stream.
			stream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeNoError))
			uploadErr = nil
		}
		if uploadErr != nil {
			stream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
			stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		} else {
			sendFinished.Store(true)
		}
	}
	if request.Body != nil && request.Body != http.NoBody {
		go sendBody()
	} else {
		// No-body requests must publish their FIN before an immediate server
		// response can close the body and cancel an unstarted upload worker.
		sendBody()
	}
	var response *http.Response
	for informational := 0; ; informational++ {
		response, err = stream.ReadResponse()
		if err != nil || response.StatusCode < 100 || response.StatusCode >= 200 || response.StatusCode == 101 {
			break
		}
		if informational == 5 {
			err = fmt.Errorf("alt HTTP/3 sent too many informational responses")
			break
		}
	}
	if err != nil {
		stopRequest()
		stream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		body.Close()
		entry.conn.CloseWithError(0, "alt request failed")
		self.releaseConnection(entry)
		<-uploadDone
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if uploadErr != nil {
			err = uploadErr
		}
		flight.retireWriter(writer)
		cancel()
		<-self.slot
		return nil, err
	}
	state := entry.conn.ConnectionState().TLS
	response.TLS, response.Request = &state, request
	responseBody := &altQuicResponseBody{ReadCloser: response.Body, ctx: ctx}
	responseBody.finish = func() {
		// http.Client cancels its timeout context immediately after Body.Close
		// returns. Capture ordinary response closure, and stop the send side,
		// synchronously; that later cancellation must not masquerade as an
		// upload failure while the asynchronous terminal-ACK join is running.
		stopRequest()
		responseFinished.Store(ctx.Err() == nil)
		if !sendFinished.Load() {
			stream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		}
		go func() {
			defer cancel()
			defer func() { <-self.slot }()
			if !sendFinished.Load() {
				body.Close()
			}
			<-uploadDone
			drainCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer stop()
			if uploadErr != nil || flight.waitFinished(drainCtx, stream.StreamID()) != nil {
				entry.conn.CloseWithError(0, "alt request send drain")
			}
			flight.retireWriter(writer)
			self.releaseConnection(entry)
		}()
	}
	response.Body = responseBody
	stopBodyRequest := context.AfterFunc(ctx, func() { responseBody.Close() })
	stopConn := context.AfterFunc(entry.conn.Context(), func() { responseBody.Close() })
	responseBody.mutex.Lock()
	if responseBody.closed {
		stopBodyRequest()
		stopConn()
	} else {
		responseBody.stopRequest, responseBody.stopConn = stopBodyRequest, stopConn
	}
	responseBody.mutex.Unlock()
	return response, nil
}

// Read at most the admitted body and one refusal byte. A lying ContentLength
// cannot send its extra bytes; unknown bodies never become an unbounded slice.
func writeAltQuicBody(ctx context.Context, writer io.Writer, body io.Reader, request *http.Request) error {
	limit := altMaxUploadBytes
	if request.ContentLength > 0 {
		limit = min(limit, int(request.ContentLength))
	}
	var scratch [quicSendFlightChunk]byte
	written := 0
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := limit - written
		n, err := body.Read(scratch[:max(1, min(remaining, len(scratch)))])
		if remaining == 0 && n != 0 {
			return errAltRequestMemory
		}
		if n > 0 {
			emptyReads = 0
			if sent, writeErr := writer.Write(scratch[:n]); writeErr != nil {
				return &altQuicUploadWriteError{err: writeErr}
			} else if sent != n {
				return io.ErrShortWrite
			}
			written += n
		} else if err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return io.ErrNoProgress
			}
		}
		if err == io.EOF {
			if request.ContentLength > 0 && int64(written) != request.ContentLength {
				return io.ErrUnexpectedEOF
			}
			if len(request.Trailer) > 0 {
				return &AltQuicUnsupportedRequestError{Feature: "request trailers"}
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Keep send failures distinct from a body reader returning the same error.
// A remote STOP_SENDING must not hide local reader/length/trailer/budget errors.
type altQuicUploadWriteError struct{ err error }

func (e *altQuicUploadWriteError) Error() string { return e.err.Error() }
func (e *altQuicUploadWriteError) Unwrap() error { return e.err }

// Only normal peer upload completion or our own explicit response close can
// stop sending without failing the response. responseClosed means the response
// was closed while its context was live, before http.Client cancels its timer.
// A cancellation already present at response closure always retains priority.
func altUploadStoppedNormally(ctx, streamCtx context.Context, responseClosed bool) bool {
	var streamErr *quic.StreamError
	return (ctx.Err() == nil || responseClosed) && errors.As(context.Cause(streamCtx), &streamErr) &&
		(streamErr.Remote && streamErr.ErrorCode == quic.StreamErrorCode(http3.ErrCodeNoError) ||
			responseClosed && !streamErr.Remote && streamErr.ErrorCode == quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
}

func altUploadSendStopped(ctx, streamCtx context.Context, err error, responseClosed bool) bool {
	if !altUploadStoppedNormally(ctx, streamCtx, responseClosed) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true // the bounded writer's credit wait observes stream cancellation
	}
	var streamErr, cause *quic.StreamError
	return errors.As(err, &streamErr) && errors.As(context.Cause(streamCtx), &cause) &&
		streamErr.StreamID == cause.StreamID && streamErr.Remote == cause.Remote && streamErr.ErrorCode == cause.ErrorCode
}

func (self *altQuicBoundedTransport) closeConnections(idleOnly bool) {
	var conns [16]*quic.Conn
	var stopDial context.CancelFunc
	self.mutex.Lock()
	if !idleOnly {
		self.closed = true
		stopDial = self.dialCancel
	}
	for i, entry := range self.connections {
		if entry != nil && (!idleOnly || !entry.active) {
			conns[i] = entry.conn
			self.connections[i] = nil
		}
	}
	self.mutex.Unlock()
	if stopDial != nil {
		stopDial()
	}
	for _, conn := range conns {
		if conn != nil {
			conn.CloseWithError(0, "alt pool closed")
		}
	}
}

func (self *altQuicBoundedTransport) CloseIdleConnections() { self.closeConnections(true) }
func (self *altQuicBoundedTransport) Close() error {
	self.closeConnections(false)
	return nil
}

type altQuicUploadBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (self *altQuicUploadBody) Close() error {
	self.once.Do(func() {
		if self.ReadCloser != nil {
			self.err = self.ReadCloser.Close()
		}
	})
	return self.err
}

type altQuicResponseBody struct {
	io.ReadCloser
	ctx                   context.Context
	once                  sync.Once
	mutex                 sync.Mutex
	finish                func()
	stopRequest, stopConn func() bool
	err                   error
	closed                bool
}

func (self *altQuicResponseBody) Read(b []byte) (int, error) {
	self.mutex.Lock()
	reader := self.ReadCloser
	self.mutex.Unlock()
	if reader == nil {
		return 0, net.ErrClosed
	}
	n, err := reader.Read(b)
	if err != nil && self.ctx.Err() != nil {
		err = self.ctx.Err()
	}
	return n, err
}

func (self *altQuicResponseBody) Close() error {
	self.once.Do(func() {
		self.mutex.Lock()
		self.closed = true
		if self.stopRequest != nil {
			self.stopRequest()
		}
		if self.stopConn != nil {
			self.stopConn()
		}
		reader, finish := self.ReadCloser, self.finish
		self.ReadCloser, self.finish = nil, nil
		self.stopRequest, self.stopConn = nil, nil
		self.mutex.Unlock()
		self.err = reader.Close()
		finish()
	})
	return self.err
}
