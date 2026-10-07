package connect

// Observes application reads after HTTP/WebSocket framing, preserving the TLS
// connection's concrete type and HTTP/2 negotiation. No payload is retained.

import (
	"context"
	"io"
	"net/http"
)

// The body remains owned by the HTTP response; Close forwards exactly once
// through the existing response lifecycle.
type strategyDeliveryBody struct {
	io.ReadCloser
	ctx  context.Context
	info *DialerInfo
}

// Borrows p; returned bytes remain owned by the caller.
func (self *strategyDeliveryBody) Read(p []byte) (int, error) {
	n, err := self.ReadCloser.Read(p)
	self.info.observeRead(self.ctx, n, err, true)
	return n, err
}

// Attaches the observer only after response headers, so handshakes and writes
// never count as verified application delivery.
func (self *ClientStrategy) observeHttpResponse(ctx context.Context, info *DialerInfo, response *http.Response, err error) {
	if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		if info != nil && info.delivery != nil {
			info.delivery.finish(verdictHandshake)
		}
		return
	}
	info.observeRead(ctx, 0, err, false)
	if err == nil && response != nil && response.Body != nil {
		response.Body = &strategyDeliveryBody{ReadCloser: response.Body, ctx: ctx, info: info}
	}
}

// Delegates all framing and lifetime behavior; received messages share one
// attempt's delivery budget across both read entry points.
type strategyDeliveryMessageConn struct {
	H1MessageConn
	ctx  context.Context
	info *DialerInfo
}

// Returns unpooled message bytes owned by the caller. Hot paths retain their
// pooled ownership contract through ReadH1PooledMessage.
func (self *strategyDeliveryMessageConn) ReadMessage() (int, []byte, error) {
	kind, message, err := self.H1MessageConn.ReadMessage()
	self.info.observeRead(self.ctx, len(message), err, false)
	return kind, message, err
}

// The reader borrows the underlying message until the next read; no buffer is
// retained by the observer.
func (self *strategyDeliveryMessageConn) NextReader() (int, io.Reader, error) {
	kind, reader, err := self.H1MessageConn.NextReader()
	self.info.observeRead(self.ctx, 0, err, false)
	if err == nil {
		reader = &strategyDeliveryReader{Reader: reader, ctx: self.ctx, info: self.info}
	}
	return kind, reader, err
}

// Preserves concrete framing access for batching, stats and progress hooks.
func unobservedH1MessageConn(conn H1MessageConn) H1MessageConn {
	if observed, ok := conn.(*strategyDeliveryMessageConn); ok {
		return observed.H1MessageConn
	}
	return conn
}

// A message reader EOF is a frame boundary, not a connection delivery verdict.
type strategyDeliveryReader struct {
	io.Reader
	ctx  context.Context
	info *DialerInfo
}

// Borrows p and records only bytes the framed application reader returned.
func (self *strategyDeliveryReader) Read(p []byte) (int, error) {
	n, err := self.Reader.Read(p)
	self.info.observeRead(self.ctx, n, err, false)
	return n, err
}
