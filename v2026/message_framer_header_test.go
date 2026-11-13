package connect

import (
	"bytes"
	"io"
	"testing"
)

func testHeaderFramer(maxMessageLen int) *Framer {
	settings := DefaultFramerSettings(maxMessageLen)
	settings.HeaderByteCount = transportDelayHeaderByteCount
	return NewFramer(settings)
}

func testFramerHeader(seed byte) []byte {
	header := make([]byte, transportDelayHeaderByteCount)
	for i := range header {
		header[i] = seed + byte(i)
	}
	return header
}

func testFramerMessage(n int, seed byte) []byte {
	message := make([]byte, n)
	for i := range message {
		message[i] = seed + byte(i*7)
	}
	return message
}

// Small, split-path and empty messages all round trip with their header, and
// the returned message is a pool buffer of exactly the message length.
func TestFramerWithHeaderRoundTrip(t *testing.T) {
	framer := testHeaderFramer(16 * 1024)
	for _, n := range []int{0, 1, 15, 300, 4096, 16 * 1024} {
		var stream bytes.Buffer
		header := testFramerHeader(byte(n))
		message := testFramerMessage(n, 3)
		if err := framer.WriteWithHeader(&stream, header, message); err != nil {
			t.Fatalf("write %d: %s", n, err)
		}
		if stream.Len() != 4+len(header)+n {
			t.Fatalf("write %d: wire %d bytes, expected %d", n, stream.Len(), 4+len(header)+n)
		}
		readHeader := make([]byte, len(header))
		read, err := framer.ReadWithHeader(&stream, readHeader)
		if err != nil {
			t.Fatalf("read %d: %s", n, err)
		}
		if !bytes.Equal(readHeader, header) || !bytes.Equal(read, message) {
			t.Fatalf("read %d: header or message mismatch", n)
		}
		// the read buffer is pooled exactly when a fresh buffer of its size is
		if expected := MessagePoolReturn(MessagePoolGet(n)); MessagePoolReturn(read) != expected {
			t.Fatalf("read %d: pool return %v, expected %v", n, !expected, expected)
		}
	}
}

// Every frame of one physical write carries the same header.
func TestFramerBatchWithHeaderStorage(t *testing.T) {
	framer := testHeaderFramer(16 * 1024)
	header := testFramerHeader(40)
	messages := [][]byte{testFramerMessage(20, 1), testFramerMessage(0, 2), testFramerMessage(700, 3)}
	storage := make([]byte, 16*1024)
	writes := 0
	var stream bytes.Buffer
	counting := writerFunc(func(p []byte) (int, error) {
		writes += 1
		return stream.Write(p)
	})
	if err := framer.WriteBatchWithHeaderStorage(counting, header, messages, storage); err != nil {
		t.Fatalf("batch: %s", err)
	}
	if writes != 1 {
		t.Fatalf("batch made %d writes, expected 1", writes)
	}
	for i, message := range messages {
		readHeader := make([]byte, len(header))
		read, err := framer.ReadWithHeader(&stream, readHeader)
		if err != nil {
			t.Fatalf("read %d: %s", i, err)
		}
		if !bytes.Equal(readHeader, header) || !bytes.Equal(read, message) {
			t.Fatalf("read %d: mismatch", i)
		}
		MessagePoolReturn(read)
	}
}

// A singleton larger than the storage keeps the split fallback.
func TestFramerBatchWithHeaderSingletonFallback(t *testing.T) {
	framer := testHeaderFramer(16 * 1024)
	header := testFramerHeader(9)
	message := testFramerMessage(8000, 5)
	var stream bytes.Buffer
	if err := framer.WriteBatchWithHeaderStorage(&stream, header, [][]byte{message}, make([]byte, 64)); err != nil {
		t.Fatalf("batch: %s", err)
	}
	readHeader := make([]byte, len(header))
	read, err := framer.ReadWithHeader(&stream, readHeader)
	if err != nil || !bytes.Equal(read, message) || !bytes.Equal(readHeader, header) {
		t.Fatalf("singleton fallback did not round trip: %v", err)
	}
	MessagePoolReturn(read)
}

// A truncated header or body fails and leaves the caller's header untouched.
func TestFramerReadWithHeaderTruncated(t *testing.T) {
	framer := testHeaderFramer(1024)
	var full bytes.Buffer
	if err := framer.WriteWithHeader(&full, testFramerHeader(1), testFramerMessage(100, 1)); err != nil {
		t.Fatal(err)
	}
	wire := full.Bytes()
	for _, cut := range []int{2, 4 + 6, 4 + transportDelayHeaderByteCount + 50} {
		readHeader := make([]byte, transportDelayHeaderByteCount)
		_, err := framer.ReadWithHeader(bytes.NewReader(wire[:cut]), readHeader)
		if err == nil {
			t.Fatalf("cut %d: expected an error", cut)
		}
		if !bytes.Equal(readHeader, make([]byte, transportDelayHeaderByteCount)) {
			t.Fatalf("cut %d: header written on error", cut)
		}
	}
}

func TestFramerWithHeaderRejectsWrongHeaderAndOversize(t *testing.T) {
	framer := testHeaderFramer(1024)
	var stream bytes.Buffer
	if err := framer.WriteWithHeader(&stream, make([]byte, 5), nil); err == nil {
		t.Fatalf("expected a header length error")
	}
	if err := framer.WriteWithHeader(&stream, testFramerHeader(0), make([]byte, 1025)); err == nil {
		t.Fatalf("expected a message length error")
	}
	if _, err := framer.ReadWithHeader(&stream, make([]byte, 5)); err == nil {
		t.Fatalf("expected a read header length error")
	}
	// a frame shorter than its header is rejected
	short := []byte{0, 4, 0, 0, 1, 2, 3, 4}
	if _, err := framer.ReadWithHeader(bytes.NewReader(short), make([]byte, transportDelayHeaderByteCount)); err == nil || err == io.EOF {
		t.Fatalf("expected a short frame error, got %v", err)
	}
}

type writerFunc func(p []byte) (int, error)

func (self writerFunc) Write(p []byte) (int, error) {
	return self(p)
}
