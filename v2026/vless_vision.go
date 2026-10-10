package connect

// vless_vision.go — the client side of the xtls-rprx-vision flow.
//
// Vision hides the length pattern of the inner tls handshake. Each direction
// starts as a stream of padded blocks; the first block of each direction
// starts with the 16-byte user id:
//
//	command (1) | content length (2) | padding length (2) | content | padding
//
// Command 0 continues the padding, 1 ends it (the rest of that direction is
// plain, still inside the outer tls) and 2 ends it with direct (the rest of
// that direction goes over the raw tcp connection, outside the outer tls, so
// the inner tls records are not encrypted twice).
//
// This client pads its own direction until its first inner application data
// record and then ends with command 1, never 2: a server reads either, and
// staying inside the outer tls means the uplink never needs the raw socket.
// The server decides the downlink, and switches it to direct after the inner
// tls 1.3 handshake. The raw bytes it sends after that must not be swallowed
// by the outer tls layer's read-ahead, so the socket under the outer tls is a
// `vlessRecordConn`, which hands that layer exactly one tls record per read
// and never reads past it. When the direct command arrives the socket holds
// every byte that follows, and reads switch to it. This replaces the reach
// into the tls connection's private buffers that other implementations make.
//
// The padding lengths follow Xray's defaults (900, 500, 900, 256), so the
// blocks look like those of every other vision client.

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

const (
	vlessVisionCommandContinue = byte(0)
	vlessVisionCommandEnd      = byte(1)
	vlessVisionCommandDirect   = byte(2)

	// a block shorter than this is padded up to about a long padding
	vlessVisionLongPaddingThreshold = 900
	vlessVisionLongPaddingRange     = 500
	vlessVisionLongPaddingBase      = 900
	vlessVisionShortPaddingRange    = 256
	// Xray's 8 KiB buffer less the room it reserves for the block header and
	// the user id, which bounds both the content and the padding of a block
	vlessVisionBlockSize       = 8 * 1024
	vlessVisionMaxBlockContent = vlessVisionBlockSize - 21
	// the writes after which non-tls inner traffic stops padding, one before
	// Xray's filter count of 8 for compatibility with older servers
	vlessVisionPaddedWriteLimit = 7

	// the largest tls record: 2^14 plaintext plus the tls 1.2 expansion
	// allowance, which also covers tls 1.3
	vlessTlsMaxRecordLength = 16*1024 + 2048
	// the largest plaintext a tls read returns at once
	vlessTlsMaxPlaintextLength = 16*1024 + 256
)

// The socket under the outer tls of a vision stream. Until `setDirect`, reads
// hand the tls layer one record at a time and never read past it; after, they
// go straight to the socket. Writes always go straight to the socket.
//
// Only the goroutine that reads the vision stream reads this; that is also
// the only goroutine that calls `setDirect`.
type vlessRecordConn struct {
	net.Conn

	direct     bool
	record     []byte
	recordData []byte
}

func newVlessRecordConn(conn net.Conn) *vlessRecordConn {
	return &vlessRecordConn{
		Conn: conn,
	}
}

func (self *vlessRecordConn) setDirect() {
	self.direct = true
}

func (self *vlessRecordConn) Read(b []byte) (int, error) {
	if 0 < len(self.record) {
		n := copy(b, self.record)
		self.record = self.record[n:]
		return n, nil
	}
	if self.direct {
		return self.Conn.Read(b)
	}
	var header [5]byte
	if _, err := io.ReadFull(self.Conn, header[:]); err != nil {
		return 0, err
	}
	length := int(header[3])<<8 | int(header[4])
	if vlessTlsMaxRecordLength < length {
		return 0, fmt.Errorf("vless: tls record of %d bytes", length)
	}
	if cap(self.recordData) < 5+length {
		self.recordData = make([]byte, 5+length)
	}
	record := self.recordData[:5+length]
	copy(record, header[:])
	if _, err := io.ReadFull(self.Conn, record[5:]); err != nil {
		return 0, err
	}
	n := copy(b, record)
	self.record = record[n:]
	return n, nil
}

// One VLESS stream with the vision flow, over the outer tls (or reality)
// connection `tlsConn` whose socket is `recordConn`.
//
// Read and Write may be called concurrently with each other, as on any
// net.Conn; concurrent writes are serialized. Reads must come from one
// goroutine at a time.
type vlessVisionConn struct {
	tlsConn    net.Conn
	recordConn *vlessRecordConn
	userId     [16]byte

	writeLock     sync.Mutex
	requestHeader []byte
	requestSent   bool
	writePadding  bool
	// the user id still has to lead a block
	writeUserIdPending bool
	// the first inner write was a tls client hello
	writeTls   bool
	writeCount int

	// the reading goroutine's state
	responseRead bool
	readMode     vlessVisionReadMode
	// the user id has been looked for at the start of the downlink
	readUserIdChecked bool
	readPending       []byte
	remainingCommand  int
	currentCommand    byte
	remainingContent  int
	remainingPadding  int
	readOut           []byte
	readErr           error
	readBuffer        []byte
}

type vlessVisionReadMode int

const (
	vlessVisionReadPadded vlessVisionReadMode = iota
	vlessVisionReadPlain
	vlessVisionReadDirect
)

func newVlessVisionConn(
	tlsConn net.Conn,
	recordConn *vlessRecordConn,
	userId [16]byte,
	requestHeader []byte,
) *vlessVisionConn {
	return &vlessVisionConn{
		tlsConn:            tlsConn,
		recordConn:         recordConn,
		userId:             userId,
		requestHeader:      requestHeader,
		writePadding:       true,
		writeUserIdPending: true,
	}
}

func isVlessVisionTlsApplicationData(b []byte) bool {
	return 3 <= len(b) && b[0] == 0x17 && b[1] == 0x03 && b[2] == 0x03
}

// Appends one padded block of content to packet.
func (self *vlessVisionConn) appendBlockWithLock(packet []byte, content []byte, command byte, longPadding bool) []byte {
	contentLength := len(content)
	var paddingLength int
	if longPadding && contentLength < vlessVisionLongPaddingThreshold {
		paddingLength = rand.IntN(vlessVisionLongPaddingRange) + vlessVisionLongPaddingBase - contentLength
	} else {
		paddingLength = rand.IntN(vlessVisionShortPaddingRange)
	}
	paddingLength = max(0, min(paddingLength, vlessVisionMaxBlockContent-contentLength))
	if self.writeUserIdPending {
		self.writeUserIdPending = false
		packet = append(packet, self.userId[:]...)
	}
	packet = append(
		packet,
		command,
		byte(contentLength>>8),
		byte(contentLength),
		byte(paddingLength>>8),
		byte(paddingLength),
	)
	packet = append(packet, content...)
	// the padding is not secret, only its length matters; zeros are what
	// other clients send
	packet = append(packet, make([]byte, paddingLength)...)
	return packet
}

func (self *vlessVisionConn) Write(b []byte) (int, error) {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()

	if !self.writePadding {
		return self.tlsConn.Write(b)
	}

	var packet []byte
	if !self.requestSent {
		self.requestSent = true
		packet = append(packet, self.requestHeader...)
		self.requestHeader = nil
	}
	if self.writeCount == 0 && isTlsClientHello(b) {
		self.writeTls = true
	}
	self.writeCount += 1

	// the write that carries the first application data record is the last
	// padded one; so is the last of a run of non-tls writes
	end := (self.writeTls && isVlessVisionTlsApplicationData(b)) ||
		(!self.writeTls && vlessVisionPaddedWriteLimit <= self.writeCount)
	// long padding hides the short handshake records, and the first
	// application data record is padded long once more, as Xray does
	longPadding := self.writeTls

	content := b
	for {
		chunk := content[:min(len(content), vlessVisionMaxBlockContent)]
		content = content[len(chunk):]
		command := vlessVisionCommandContinue
		if len(content) == 0 && end {
			command = vlessVisionCommandEnd
		}
		packet = self.appendBlockWithLock(packet, chunk, command, longPadding)
		if len(content) == 0 {
			break
		}
	}
	if end {
		self.writePadding = false
	}
	if _, err := self.tlsConn.Write(packet); err != nil {
		return 0, err
	}
	return len(b), nil
}

// The request header must reach the server before it answers anything. A read
// before any write sends it with an empty long-padded block, which is what
// other clients send when they have no data yet.
func (self *vlessVisionConn) sendRequestHeader() error {
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if self.requestSent {
		return nil
	}
	self.requestSent = true
	packet := append([]byte{}, self.requestHeader...)
	self.requestHeader = nil
	packet = self.appendBlockWithLock(packet, nil, vlessVisionCommandContinue, true)
	_, err := self.tlsConn.Write(packet)
	return err
}

func (self *vlessVisionConn) Read(b []byte) (int, error) {
	if !self.responseRead {
		if err := self.sendRequestHeader(); err != nil {
			return 0, err
		}
		if err := readVlessResponseHeader(self.tlsConn); err != nil {
			return 0, err
		}
		self.responseRead = true
	}
	for {
		if 0 < len(self.readOut) {
			n := copy(b, self.readOut)
			self.readOut = self.readOut[n:]
			return n, nil
		}
		if self.readErr != nil {
			return 0, self.readErr
		}
		switch self.readMode {
		case vlessVisionReadDirect:
			return self.recordConn.Read(b)
		case vlessVisionReadPlain:
			return self.tlsConn.Read(b)
		}
		if self.readBuffer == nil {
			// a whole record's plaintext, so the outer tls never keeps part of
			// the record that carries the direct command
			self.readBuffer = make([]byte, vlessTlsMaxPlaintextLength)
		}
		n, err := self.tlsConn.Read(self.readBuffer)
		if 0 < n {
			if unpadErr := self.unpad(self.readBuffer[:n]); unpadErr != nil {
				self.readErr = unpadErr
			}
		}
		if err != nil && self.readErr == nil {
			self.readErr = err
		}
	}
}

// Runs the downlink bytes of one read through the block parser, appending the
// content to readOut and switching the read mode where the server ends the
// padding.
func (self *vlessVisionConn) unpad(chunk []byte) error {
	if !self.readUserIdChecked {
		self.readPending = append(self.readPending, chunk...)
		if len(self.readPending) < len(self.userId) {
			return nil
		}
		chunk = self.readPending
		self.readPending = nil
		self.readUserIdChecked = true
		if !bytes.Equal(chunk[:len(self.userId)], self.userId[:]) {
			// a server that does not pad: everything is plain
			self.readMode = vlessVisionReadPlain
			self.readOut = append(self.readOut, chunk...)
			return nil
		}
		chunk = chunk[len(self.userId):]
		self.remainingCommand = 5
	}
	for 0 < len(chunk) {
		switch {
		case 0 < self.remainingCommand:
			v := chunk[0]
			chunk = chunk[1:]
			switch self.remainingCommand {
			case 5:
				self.currentCommand = v
			case 4:
				self.remainingContent = int(v) << 8
			case 3:
				self.remainingContent |= int(v)
			case 2:
				self.remainingPadding = int(v) << 8
			case 1:
				self.remainingPadding |= int(v)
			}
			self.remainingCommand -= 1
		case 0 < self.remainingContent:
			n := min(len(chunk), self.remainingContent)
			self.readOut = append(self.readOut, chunk[:n]...)
			chunk = chunk[n:]
			self.remainingContent -= n
		default:
			n := min(len(chunk), self.remainingPadding)
			chunk = chunk[n:]
			self.remainingPadding -= n
		}
		if self.remainingCommand == 0 && self.remainingContent == 0 && self.remainingPadding == 0 {
			switch self.currentCommand {
			case vlessVisionCommandContinue:
				self.remainingCommand = 5
			case vlessVisionCommandEnd:
				self.readMode = vlessVisionReadPlain
				self.readOut = append(self.readOut, chunk...)
				return nil
			case vlessVisionCommandDirect:
				self.readMode = vlessVisionReadDirect
				self.recordConn.setDirect()
				// nothing should follow the block in its record; keep it if
				// something does, as other clients do
				self.readOut = append(self.readOut, chunk...)
				return nil
			default:
				return fmt.Errorf("vless: vision command %d", self.currentCommand)
			}
		}
	}
	return nil
}

func (self *vlessVisionConn) Close() error {
	return self.tlsConn.Close()
}

func (self *vlessVisionConn) LocalAddr() net.Addr {
	return self.tlsConn.LocalAddr()
}

func (self *vlessVisionConn) RemoteAddr() net.Addr {
	return self.tlsConn.RemoteAddr()
}

// The outer tls sets each deadline on its socket, which is also where direct
// reads come from.
func (self *vlessVisionConn) SetDeadline(t time.Time) error {
	return self.tlsConn.SetDeadline(t)
}

func (self *vlessVisionConn) SetReadDeadline(t time.Time) error {
	return self.tlsConn.SetReadDeadline(t)
}

func (self *vlessVisionConn) SetWriteDeadline(t time.Time) error {
	return self.tlsConn.SetWriteDeadline(t)
}
