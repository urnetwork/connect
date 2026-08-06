package connect

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type recordingWriteConn struct {
	writes [][]byte
}

func (self *recordingWriteConn) Read(buffer []byte) (int, error) {
	return 0, io.EOF
}

func (self *recordingWriteConn) Write(buffer []byte) (int, error) {
	self.writes = append(self.writes, bytes.Clone(buffer))
	return len(buffer), nil
}

func (self *recordingWriteConn) Close() error {
	return nil
}

func (self *recordingWriteConn) LocalAddr() net.Addr {
	return nil
}

func (self *recordingWriteConn) RemoteAddr() net.Addr {
	return nil
}

func (self *recordingWriteConn) SetDeadline(deadline time.Time) error {
	return nil
}

func (self *recordingWriteConn) SetReadDeadline(deadline time.Time) error {
	return nil
}

func (self *recordingWriteConn) SetWriteDeadline(deadline time.Time) error {
	return nil
}

func TestWebSocketWriteBatchPassesThroughBeforeBegin(t *testing.T) {
	underlying := &recordingWriteConn{}
	conn := newWebSocketWriteBatchConn(underlying)
	message := []byte("upgrade handshake")

	n, err := conn.Write(message)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, n, len(message))
	AssertEqual(t, len(underlying.writes), 1)
	if !bytes.Equal(underlying.writes[0], message) {
		t.Fatal("pass-through write changed the handshake bytes")
	}
}

func TestWebSocketWriteBatchCoalescesWithoutChangingBytes(t *testing.T) {
	underlying := &recordingWriteConn{}
	conn := newWebSocketWriteBatchConn(underlying)
	messages := [][]byte{
		[]byte("first frame"),
		[]byte("second frame"),
		[]byte("third frame"),
		[]byte("fourth frame"),
	}

	conn.beginWriteBatch()
	for _, message := range messages {
		if _, err := conn.Write(message); err != nil {
			t.Fatal(err)
		}
	}
	if len(underlying.writes) != 0 {
		t.Fatal("batch wrote before its explicit flush boundary")
	}
	if err := conn.flushWriteBatch(); err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, len(underlying.writes), 1)
	if !bytes.Equal(underlying.writes[0], bytes.Join(messages, nil)) {
		t.Fatal("coalesced write changed byte order or content")
	}
}

func TestWebSocketWriteBatchBoundsRetainedBuffer(t *testing.T) {
	underlying := &recordingWriteConn{}
	conn := newWebSocketWriteBatchConn(underlying)
	first := bytes.Repeat([]byte{0x11}, 10*1024)
	second := bytes.Repeat([]byte{0x22}, 10*1024)

	conn.beginWriteBatch()
	if _, err := conn.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(second); err != nil {
		t.Fatal(err)
	}
	if err := conn.flushWriteBatch(); err != nil {
		t.Fatal(err)
	}

	AssertEqual(t, cap(conn.writeBuffer), webSocketWriteBatchMaxByteCount)
	AssertEqual(t, len(underlying.writes), 2)
	if !bytes.Equal(underlying.writes[0], first) ||
		!bytes.Equal(underlying.writes[1], second) {
		t.Fatal("bounded flush changed byte order or content")
	}
}

func TestWebSocketWriteBatchAbortDropsUnflushedBytes(t *testing.T) {
	underlying := &recordingWriteConn{}
	conn := newWebSocketWriteBatchConn(underlying)

	conn.beginWriteBatch()
	if _, err := conn.Write([]byte("retired connection data")); err != nil {
		t.Fatal(err)
	}
	conn.abortWriteBatch()
	if err := conn.flushWriteBatch(); err != nil {
		t.Fatal(err)
	}
	if len(underlying.writes) != 0 {
		t.Fatal("aborted batch reached the retired connection")
	}
}

func TestWebSocketWriteBatchSteadyStateDoesNotAllocate(t *testing.T) {
	underlying := &immediateWriteConn{}
	conn := newWebSocketWriteBatchConn(underlying)
	message := make([]byte, 3*1024)
	conn.beginWriteBatch()
	conn.abortWriteBatch()

	allocCount := testing.AllocsPerRun(1_000, func() {
		conn.beginWriteBatch()
		for range platformWebSocketWriteBatchMaxMessages {
			if _, err := conn.Write(message); err != nil {
				panic(err)
			}
		}
		if err := conn.flushWriteBatch(); err != nil {
			panic(err)
		}
	})
	AssertEqual(t, allocCount, float64(0))
}

func BenchmarkWebSocketWriteBatchFourMessages(b *testing.B) {
	underlying := &immediateWriteConn{}
	conn := newWebSocketWriteBatchConn(underlying)
	message := make([]byte, 3*1024)
	conn.beginWriteBatch()
	conn.abortWriteBatch()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		conn.beginWriteBatch()
		for range platformWebSocketWriteBatchMaxMessages {
			if _, err := conn.Write(message); err != nil {
				b.Fatal(err)
			}
		}
		if err := conn.flushWriteBatch(); err != nil {
			b.Fatal(err)
		}
	}
}
