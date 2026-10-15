package connect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/gorilla/websocket"
)

// Build fixtures independently from either Framer writer so matching parser
// and serializer bugs cannot make the wire-format tests pass together.
func framedMessageTestWire(protocol string, messages ...[]byte) []byte {
	var wire bytes.Buffer
	for _, message := range messages {
		var header [4]byte
		if protocol == H1FramerProtocol {
			binary.BigEndian.PutUint16(header[:2], uint16(len(message)))
			binary.BigEndian.PutUint16(header[2:], uint16(min(19, len(message))))
		} else {
			binary.BigEndian.PutUint32(header[:], uint32(len(message)))
		}
		wire.Write(header[:])
		wire.Write(message)
	}
	return wire.Bytes()
}

func TestFramedMessageConnProtocolSelectionAndLimits(t *testing.T) {
	for _, test := range []struct {
		name, protocol string
		maximum        int
	}{
		{"compact", H1FramerProtocol, math.MaxUint16},
		{"xl", H1FramerXlProtocol, 3 * 1024 * 1024},
	} {
		t.Run(test.name, func(t *testing.T) {
			lengths := []int{0, 1, 1200, 16381, math.MaxUint16}
			if test.protocol == H1FramerXlProtocol {
				lengths = append(lengths, math.MaxUint16+1, test.maximum)
			}
			messages := make([][]byte, len(lengths))
			for i, length := range lengths {
				messages[i] = bytes.Repeat([]byte{byte(31 + i)}, length)
			}
			raw := newH1UpgradeScriptConn(framedMessageTestWire(test.protocol, messages...))
			conn, err := NewFramedMessageConn(raw, test.protocol, test.maximum, nil)
			if err != nil {
				t.Fatal(err)
			}
			if conn.UnderlyingConn() != raw || conn.storage != nil {
				t.Fatal("idle/receive-only carrier changed socket or allocated writer scratch")
			}
			for i, want := range messages {
				kind, got, err := conn.ReadPooledMessage()
				if err != nil {
					t.Fatalf("message %d: %v", i, err)
				}
				match := kind == websocket.BinaryMessage && bytes.Equal(got, want)
				MessagePoolReturn(got)
				if !match {
					t.Fatalf("message %d: protocol %s changed message length/content or kind", i, test.protocol)
				}
			}
			if conn.storage != nil {
				t.Fatal("reads allocated retained writer storage")
			}
			if _, _, err := conn.ReadMessage(); !errors.Is(err, io.EOF) || !raw.closed.Load() {
				t.Fatalf("EOF must terminate stream: %v, closed=%v", err, raw.closed.Load())
			}
		})
	}
	for _, test := range []struct {
		protocol string
		maximum  int
	}{
		{H1FramerProtocol, -1}, {H1FramerProtocol, math.MaxUint16 + 1}, {"urnetwork-framer/2", 1200}, {"urnetwork-framerxl", 1200},
	} {
		if conn, err := NewFramedMessageConn(newH1UpgradeScriptConn(nil), test.protocol, test.maximum, nil); conn != nil || err == nil {
			t.Fatalf("accepted invalid settings: %q, %d", test.protocol, test.maximum)
		}
	}
	if conn, err := NewFramedMessageConn(nil, H1FramerProtocol, 1200, nil); conn != nil || err == nil {
		t.Fatal("accepted nil underlying connection")
	}
}

func TestFramedMessageConnStreamingBoundsAndPartialReader(t *testing.T) {
	const size = 3 * 1024 * 1024
	message := bytes.Repeat([]byte{0x81}, size)
	raw := newH1UpgradeScriptConn(framedMessageTestWire(H1FramerXlProtocol, message, nil, []byte("next")))
	conn, err := NewFramedMessageConn(raw, H1FramerXlProtocol, size, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := MessagePoolOutstandingCount()
	kind, reader, err := conn.NextReader()
	if err != nil || kind != websocket.BinaryMessage {
		t.Fatalf("NextReader: %d, %v", kind, err)
	}
	if raw.readBytes != 4 || MessagePoolOutstandingCount() != before || conn.storage != nil {
		t.Fatal("NextReader allocated/read a large body before consumption")
	}
	var prefix [1200]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil || !bytes.Equal(prefix[:], message[:1200]) {
		t.Fatalf("streamed prefix: %v", err)
	}
	// Asking for the next message drains only the old body, preserving the
	// zero-length heartbeat and subsequent application message boundaries.
	_, reader, err = conn.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := reader.Read(prefix[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("heartbeat consumed application bytes: %d, %v", n, err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil || string(got) != "next" {
		t.Fatalf("message after drained body/heartbeat: %q, %v", got, err)
	}
	if raw.maxReadSize > 32*1024 || conn.storage != nil || MessagePoolOutstandingCount() != before {
		t.Fatalf("large streaming read retained/allocated message storage; maximum read request=%d", raw.maxReadSize)
	}
}

func TestFramedMessageConnRejectsOversizeBeforeReadingBody(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			wire := framedMessageTestWire(protocol, make([]byte, 1201))
			raw := newH1UpgradeScriptConn(wire)
			stats := &H1PlusStats{}
			conn, err := NewFramedMessageConn(raw, protocol, 1200, stats)
			if err != nil {
				t.Fatal(err)
			}
			before := MessagePoolOutstandingCount()
			_, body, err := conn.ReadPooledMessage()
			if err == nil || body != nil || raw.readBytes != 4 || !raw.closed.Load() {
				t.Fatalf("oversized frame: err=%v, body=%d, read=%d, closed=%v", err, len(body), raw.readBytes, raw.closed.Load())
			}
			if MessagePoolOutstandingCount() != before || stats.Snapshot().ReadErrors != 1 {
				t.Fatal("oversized frame leaked pooled storage or failed error accounting")
			}
		})
	}
}

func TestReadH1PooledMessageHonorsPerCallLimit(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			raw := newH1UpgradeScriptConn(framedMessageTestWire(protocol, make([]byte, 1200)))
			conn, err := NewFramedMessageConn(raw, protocol, 4096, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := MessagePoolOutstandingCount()
			_, message, err := ReadH1PooledMessage(conn, 1000)
			if message != nil {
				MessagePoolReturn(message)
			}
			if err == nil || message != nil || !raw.closed.Load() {
				t.Fatalf("per-call limit ignored: len=%d, err=%v, closed=%v", len(message), err, raw.closed.Load())
			}
			if MessagePoolOutstandingCount() != before {
				t.Fatal("limit failure leaked pooled body")
			}
		})
	}
}

func TestFramedMessageConnTruncationIsTerminal(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		for _, size := range []int{1, 3, 4, 7} {
			t.Run(fmt.Sprintf("%s/%d", protocol, size), func(t *testing.T) {
				wire := framedMessageTestWire(protocol, []byte("payload"))[:size]
				raw := newH1UpgradeScriptConn(wire)
				stats := &H1PlusStats{}
				conn, err := NewFramedMessageConn(raw, protocol, 1200, stats)
				if err != nil {
					t.Fatal(err)
				}
				before := MessagePoolOutstandingCount()
				_, message, err := conn.ReadPooledMessage()
				if !errors.Is(err, io.ErrUnexpectedEOF) || message != nil || !raw.closed.Load() {
					t.Fatalf("truncated frame: body=%d, err=%v, closed=%v", len(message), err, raw.closed.Load())
				}
				if stats.Snapshot().ReadErrors != 1 || MessagePoolOutstandingCount() != before {
					t.Fatal("truncation leaked ownership or failed error accounting")
				}
			})
		}
	}
}

func TestFramedMessageConnWritesOrderedBoundedBatches(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			messages := make([][]byte, 33)
			for i := range messages {
				messages[i] = bytes.Repeat([]byte{byte(i)}, 1200)
			}
			messages[5] = nil
			messages[len(messages)-1] = bytes.Repeat([]byte{0xb1}, 20000)
			raw := newH1UpgradeScriptConn(nil)
			stats := &H1PlusStats{}
			conn, err := NewFramedMessageConn(raw, protocol, math.MaxUint16, stats)
			if err != nil {
				t.Fatal(err)
			}
			before := MessagePoolOutstandingCount()
			if err := conn.WriteMessages(messages); err != nil {
				t.Fatal(err)
			}
			if len(conn.storage) > 16*1024 || cap(conn.storage) > 16*1024 || raw.writeCalls > 5 {
				t.Fatalf("unbounded scratch or lost ready batching: len=%d cap=%d writes=%d", len(conn.storage), cap(conn.storage), raw.writeCalls)
			}
			reader := bytes.NewReader(raw.output.Bytes())
			var total int
			for i, want := range messages {
				var header [4]byte
				if _, err := io.ReadFull(reader, header[:]); err != nil {
					t.Fatal(err)
				}
				length := int(binary.BigEndian.Uint32(header[:]))
				if protocol == H1FramerProtocol {
					length = int(binary.BigEndian.Uint16(header[:2]))
				}
				if length != len(want) {
					t.Fatalf("message %d length=%d want=%d", i, length, len(want))
				}
				got := make([]byte, length)
				if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("message %d changed/reordered: %v", i, err)
				}
				total += length
			}
			if reader.Len() != 0 || stats.Snapshot().Messages != uint64(len(messages)) || stats.Snapshot().Bytes != uint64(total) {
				t.Fatalf("extra bytes or incorrect accounting: remaining=%d stats=%+v", reader.Len(), stats.Snapshot())
			}
			if MessagePoolOutstandingCount() != before {
				t.Fatal("writer retained a temporary pooled prefix")
			}
		})
	}
}

func TestFramedMessageConnSingletonAndWriteFailures(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			raw := newH1UpgradeScriptConn(nil)
			stats := &H1PlusStats{}
			conn, err := NewFramedMessageConn(raw, protocol, 1200, stats)
			if err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []func() error{
				func() error { return conn.WriteMessages([][]byte{[]byte("valid"), make([]byte, 1201)}) },
				func() error { return conn.WriteMessage(websocket.TextMessage, []byte("text")) },
				func() error { return conn.WriteControl(websocket.PingMessage, nil, time.Now()) },
			} {
				if err := invalid(); err == nil || raw.writeCalls != 0 || conn.storage != nil {
					t.Fatalf("invalid write emitted/allocated bytes: err=%v writes=%d", err, raw.writeCalls)
				}
			}
			if err := conn.WriteMessages(nil); err != nil || conn.storage != nil {
				t.Fatal("empty batch performed work")
			}
			message := bytes.Repeat([]byte{0x37}, 1200)
			if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil || raw.writeCalls != 1 {
				t.Fatalf("singleton did not use one bounded write: err=%v writes=%d", err, raw.writeCalls)
			}
			if !bytes.Equal(message, bytes.Repeat([]byte{0x37}, 1200)) {
				t.Fatal("writer changed caller payload")
			}
			raw.writeLimit = 2
			if err := conn.WriteMessage(websocket.BinaryMessage, message); !errors.Is(err, io.ErrShortWrite) || !raw.closed.Load() {
				t.Fatalf("short write not terminal: %v, closed=%v", err, raw.closed.Load())
			}
			if stats.Snapshot().WriteErrors != 1 || stats.Snapshot().Messages != 1 {
				t.Fatalf("short write counted as successful message: %+v", stats.Snapshot())
			}
		})
	}
}

type framedXlTemporaryStorageProbe struct {
	*h1UpgradeScriptConn
	source   []byte
	frame    weak.Pointer[byte]
	frames   int
	valid    bool
	borrowed bool
}

func (c *framedXlTemporaryStorageProbe) Write(p []byte) (int, error) {
	c.frames++
	c.valid = len(p) == len(c.source)+4 && binary.BigEndian.Uint32(p[:4]) == uint32(len(c.source)) && bytes.Equal(p[4:], c.source)
	if len(p) != 0 {
		c.frame = weak.Make(&p[0])
		c.borrowed = &p[0] == &c.source[0]
	}
	return len(p), nil
}

func TestFramedMessageConnLargeXlUsesOneFrameLocalWrite(t *testing.T) {
	const maximum = 3 * 1024 * 1024
	source := bytes.Repeat([]byte{0x91}, maximum)
	probe := &framedXlTemporaryStorageProbe{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), source: source}
	stats := &H1PlusStats{}
	conn, err := NewFramedMessageConn(probe, H1FramerXlProtocol, maximum, stats)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, source); err != nil {
		t.Fatal(err)
	}
	if probe.frames != 1 || !probe.valid || probe.borrowed {
		t.Fatalf("large XL must submit one contiguous header+payload temp frame: writes=%d valid=%v borrowed=%v", probe.frames, probe.valid, probe.borrowed)
	}
	if len(conn.storage) != 16*1024 || cap(conn.storage) != 16*1024 {
		t.Fatalf("large XL grew retained scratch: len=%d cap=%d", len(conn.storage), cap(conn.storage))
	}
	if got := stats.Snapshot(); got.Writes != 1 || got.Flushes != 1 || got.Messages != 1 || got.Bytes != maximum {
		t.Fatalf("large XL accounting: %+v", got)
	}
	// The stream and original message remain live while the full-size frame
	// allocation must be reclaimable immediately after synchronous completion.
	// Weak observation avoids retaining the temporary merely to test it.
	runtime.GC()
	if probe.frame.Value() != nil {
		t.Fatal("full-size temporary frame remains retained by idle XL connection")
	}
	runtime.KeepAlive(conn)
	runtime.KeepAlive(source)
}

func TestFramedMessageConnOneReaderAndWriterInParallel(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			deadline := time.Now().Add(5 * time.Second)
			_ = left.SetDeadline(deadline)
			_ = right.SetDeadline(deadline)
			l, err := NewFramedMessageConn(left, protocol, 65535, nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := NewFramedMessageConn(right, protocol, 65535, nil)
			if err != nil {
				t.Fatal(err)
			}
			const count = 64
			done := make(chan error, 4)
			for _, conn := range []*FramedMessageConn{l, r} {
				go func() {
					for i := range count {
						message := bytes.Repeat([]byte{byte(i)}, 1200)
						if i%7 == 0 {
							message = nil
						}
						if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
							done <- err
							return
						}
					}
					done <- nil
				}()
				go func() {
					for i := range count {
						_, got, err := conn.ReadPooledMessage()
						if err != nil {
							done <- err
							return
						}
						want := bytes.Repeat([]byte{byte(i)}, 1200)
						if i%7 == 0 {
							want = nil
						}
						match := bytes.Equal(got, want)
						MessagePoolReturn(got)
						if !match {
							done <- fmt.Errorf("duplex message %d changed", i)
							return
						}
					}
					done <- nil
				}()
			}
			for range 4 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
