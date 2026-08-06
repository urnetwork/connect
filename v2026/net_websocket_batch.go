package connect

import (
	"io"
	"net"
	"time"
)

const webSocketWriteBatchMaxByteCount = 16 * 1024

// webSocketWriteBatchConn preserves the WebSocket byte stream while allowing
// one transport writer to combine several already-queued messages into one
// TLS Write. It starts in pass-through mode so the HTTP upgrade handshake
// cannot be retained; PlatformTransport explicitly brackets each ready batch.
//
// Batch methods and Write are used by the WebSocket's single writer goroutine.
// Read, deadlines, and Close retain net.Conn's normal concurrent contract by
// delegating directly to the underlying connection.
type webSocketWriteBatchConn struct {
	conn        net.Conn
	writeBuffer []byte
	batching    bool
}

func newWebSocketWriteBatchConn(conn net.Conn) *webSocketWriteBatchConn {
	return &webSocketWriteBatchConn{
		conn: conn,
	}
}

func (self *webSocketWriteBatchConn) beginWriteBatch() {
	if self.batching {
		panic("websocket write batch already active")
	}
	if self.writeBuffer == nil {
		self.writeBuffer = make([]byte, 0, webSocketWriteBatchMaxByteCount)
	} else {
		self.writeBuffer = self.writeBuffer[:0]
	}
	self.batching = true
}

func (self *webSocketWriteBatchConn) abortWriteBatch() {
	self.batching = false
	self.writeBuffer = self.writeBuffer[:0]
}

func (self *webSocketWriteBatchConn) flushWriteBuffer() error {
	if len(self.writeBuffer) == 0 {
		return nil
	}
	writeByteCount := len(self.writeBuffer)
	n, err := self.conn.Write(self.writeBuffer)
	self.writeBuffer = self.writeBuffer[:0]
	if err == nil && n != writeByteCount {
		return io.ErrShortWrite
	}
	return err
}

func (self *webSocketWriteBatchConn) flushWriteBatch() error {
	if !self.batching {
		return nil
	}
	self.batching = false
	return self.flushWriteBuffer()
}

func (self *webSocketWriteBatchConn) Read(buffer []byte) (int, error) {
	return self.conn.Read(buffer)
}

func (self *webSocketWriteBatchConn) Write(buffer []byte) (int, error) {
	if !self.batching {
		return self.conn.Write(buffer)
	}
	if webSocketWriteBatchMaxByteCount < len(self.writeBuffer)+len(buffer) {
		if err := self.flushWriteBuffer(); err != nil {
			return 0, err
		}
	}
	if webSocketWriteBatchMaxByteCount < len(buffer) {
		return self.conn.Write(buffer)
	}
	self.writeBuffer = append(self.writeBuffer, buffer...)
	return len(buffer), nil
}

func (self *webSocketWriteBatchConn) Close() error {
	return self.conn.Close()
}

func (self *webSocketWriteBatchConn) LocalAddr() net.Addr {
	return self.conn.LocalAddr()
}

func (self *webSocketWriteBatchConn) RemoteAddr() net.Addr {
	return self.conn.RemoteAddr()
}

func (self *webSocketWriteBatchConn) SetDeadline(deadline time.Time) error {
	return self.conn.SetDeadline(deadline)
}

func (self *webSocketWriteBatchConn) SetReadDeadline(deadline time.Time) error {
	return self.conn.SetReadDeadline(deadline)
}

func (self *webSocketWriteBatchConn) SetWriteDeadline(deadline time.Time) error {
	return self.conn.SetWriteDeadline(deadline)
}
