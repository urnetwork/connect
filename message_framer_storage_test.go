// Verifies storage-backed singleton framing without changing the one-reader,
// one-writer contract, payload ownership, or legacy scratch fallback.
package connect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"time"
)

type framerStorageRecordingWriter struct {
	bytes.Buffer
	calls int
}

func (w *framerStorageRecordingWriter) Write(p []byte) (int, error) {
	w.calls++
	return w.Buffer.Write(p)
}

func TestFramerSingletonUsesSuppliedStorage(t *testing.T) {
	for _, size := range []int{0, 1, 64, 1200, 12288, math.MaxUint16} {
		f := NewFramer(DefaultFramerSettings(math.MaxUint16))
		p := bytes.Repeat([]byte{0x5c}, size)
		before := bytes.Clone(p)
		w := &framerStorageRecordingWriter{}
		if err := f.WriteBatchWithStorage(w, [][]byte{p}, make([]byte, size+4)); err != nil {
			t.Fatal(err)
		}
		if w.calls != 1 {
			t.Fatalf("size %d: storage-backed singleton made %d writes instead of one", size, w.calls)
		}
		got, err := f.Read(&w.Buffer)
		if err != nil || !bytes.Equal(got, before) {
			t.Fatal("singleton did not preserve wire")
		}
		MessagePoolReturn(got)
		if !bytes.Equal(p, before) {
			t.Fatal("caller data changed")
		}
	}
}
func TestFramerSingletonLegacyScratchFallback(t *testing.T) {
	for _, storageSize := range []int{0, 1, 1203} {
		f := NewFramer(DefaultFramerSettings(1200))
		p := bytes.Repeat([]byte{0x26}, 1200)
		w := &framerStorageRecordingWriter{}
		if err := f.WriteBatchWithStorage(w, [][]byte{p}, make([]byte, storageSize)); err != nil {
			t.Fatalf("legacy singleton scratch=%d: %v", storageSize, err)
		}
		got, err := f.Read(&w.Buffer)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatal("legacy fallback wire changed")
		}
		MessagePoolReturn(got)
	}
}

// Scratch is exclusive and disjoint, as required by WriteBatchWithStorage.
// Input messages may share or overlap each other; encoding must not mutate them.
func TestFramerSharedMessageBackingWithExclusiveStorage(t *testing.T) {
	for _, offsets := range [][]int{{0}, {4}, {600}, {0, 0}, {0, 600}, {600, 0}} {
		backing := make([]byte, 2000)
		for i := range backing {
			backing[i] = byte(i*17 + 3)
		}
		messages := make([][]byte, len(offsets))
		snapshots := make([][]byte, len(offsets))
		for i, offset := range offsets {
			messages[i] = backing[offset : offset+1200]
			snapshots[i] = bytes.Clone(messages[i])
		}
		allScratch := bytes.Repeat([]byte{0xf3}, (1200+4)*len(messages)+32)
		storage := allScratch[16 : len(allScratch)-16]
		w := &framerStorageRecordingWriter{}
		f := NewFramer(DefaultFramerSettings(1200))
		if err := f.WriteBatchWithStorage(w, messages, storage); err != nil {
			t.Fatal(err)
		}
		for i, want := range snapshots {
			got, err := f.Read(&w.Buffer)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("offsets=%v message=%d corrupt wire", offsets, i)
			}
			MessagePoolReturn(got)
			if !bytes.Equal(messages[i], want) {
				t.Fatal("shared input was mutated")
			}
		}
		if !bytes.Equal(allScratch[:16], bytes.Repeat([]byte{0xf3}, 16)) || !bytes.Equal(allScratch[len(allScratch)-16:], bytes.Repeat([]byte{0xf3}, 16)) {
			t.Fatal("scratch bounds exceeded")
		}
	}
}

type framerStorageTerminalWriter struct {
	calls, progress int
	err             error
}

func (w *framerStorageTerminalWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.progress < 0 {
		return len(p), w.err
	}
	return min(len(p), w.progress), w.err
}
func TestFramerStorageWriteErrorsAndLimits(t *testing.T) {
	injected := errors.New("injected write failure")
	for _, count := range []int{1, 11} {
		for _, tc := range []struct {
			progress  int
			err, want error
		}{{0, nil, io.ErrShortWrite}, {1199, nil, io.ErrShortWrite}, {0, injected, injected}, {600, injected, injected}, {-1, injected, injected}} {
			f := NewFramer(DefaultFramerSettings(1200))
			messages := make([][]byte, count)
			for i := range messages {
				messages[i] = bytes.Repeat([]byte{byte(i + 1)}, 1200)
			}
			w := &framerStorageTerminalWriter{progress: tc.progress, err: tc.err}
			if err := f.WriteBatchWithStorage(w, messages, make([]byte, 16*1024)); !errors.Is(err, tc.want) {
				t.Fatalf("count %d: error=%v want=%v", count, err, tc.want)
			}
			if w.calls != 1 {
				t.Fatal("partial failure made another write")
			}
			for i, p := range messages {
				if !bytes.Equal(p, bytes.Repeat([]byte{byte(i + 1)}, 1200)) {
					t.Fatal("partial error changed source")
				}
			}
		}
	}
	for _, messages := range [][][]byte{{make([]byte, 1201)}, {make([]byte, 1200), make([]byte, 1201)}} {
		w := &framerStorageRecordingWriter{}
		f := NewFramer(DefaultFramerSettings(1200))
		if err := f.WriteBatchWithStorage(w, messages, make([]byte, 16*1024)); err == nil {
			t.Fatal("oversize accepted")
		}
		if w.calls != 0 {
			t.Fatal("oversize emitted bytes")
		}
	}
	for _, messages := range [][][]byte{{make([]byte, math.MaxUint16+1)}, {make([]byte, 1200), make([]byte, math.MaxUint16+1)}} {
		w := &framerStorageRecordingWriter{}
		f := NewFramer(DefaultFramerSettings(2 * math.MaxUint16))
		if err := f.WriteBatchWithStorage(w, messages, make([]byte, 2*math.MaxUint16)); err == nil {
			t.Fatal("uint16 overflow accepted")
		}
		if w.calls != 0 {
			t.Fatal("overflow emitted bytes")
		}
	}
}

func TestFramerReadAndWriteMayRunConcurrently(t *testing.T) {
	f := NewFramer(DefaultFramerSettings(1200))
	p := bytes.Repeat([]byte{0x52}, 1200)
	data := make([]byte, 1204)
	binary.BigEndian.PutUint16(data, 1200)
	copy(data[4:], p)
	r := &framerStorageCyclingReader{data: data}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	go func() {
		defer wg.Done()
		<-start
		for range 10000 {
			got, err := f.Read(r)
			if err != nil {
				errs <- err
				return
			}
			ok := bytes.Equal(got, p)
			MessagePoolReturn(got)
			if !ok {
				errs <- fmt.Errorf("read payload corruption")
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		scratch := make([]byte, 1204)
		for range 10000 {
			if err := f.WriteBatchWithStorage(io.Discard, [][]byte{p}, scratch); err != nil {
				errs <- err
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// Blocking input must not prevent the independent writer from making progress.
type framerStorageBlockedReader struct {
	reader  io.Reader
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (r *framerStorageBlockedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.reader.Read(p)
}

func TestFramerWriterProgressesWhileReaderBlocked(t *testing.T) {
	framer := NewFramer(DefaultFramerSettings(1200))
	wire := make([]byte, 1204)
	binary.BigEndian.PutUint16(wire, 1200)
	reader := &framerStorageBlockedReader{reader: bytes.NewReader(wire), entered: make(chan struct{}), release: make(chan struct{})}
	readDone := make(chan error, 1)
	go func() { message, err := framer.Read(reader); MessagePoolReturn(message); readDone <- err }()
	<-reader.entered
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- framer.WriteBatchWithStorage(io.Discard, [][]byte{make([]byte, 1200)}, make([]byte, 1204))
	}()
	var writeErr error
	select {
	case writeErr = <-writeDone:
	case <-time.After(time.Second):
		writeErr = errors.New("writer blocked behind the independent reader")
	}
	close(reader.release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}

func TestFramerSingletonStorageDoesNotAllocate(t *testing.T) {
	for _, size := range []int{0, 64, 1200, 12288} {
		framer := NewFramer(DefaultFramerSettings(size))
		messages := [][]byte{make([]byte, size)}
		storage := make([]byte, size+4)
		allocations := testing.AllocsPerRun(1000, func() {
			if err := framer.WriteBatchWithStorage(io.Discard, messages, storage); err != nil {
				panic(err)
			}
		})
		if allocations != 0 {
			t.Fatalf("size %d: allocations/write=%g", size, allocations)
		}
	}
}

func TestFramerHeartbeatReusesOptionalStorage(t *testing.T) {
	for _, storageSize := range []int{0, 4, 16 * 1024} {
		framer := NewFramer(DefaultFramerSettings(1200))
		writer := &framerStorageRecordingWriter{}
		if err := framer.WriteBatchWithStorage(writer, [][]byte{nil}, make([]byte, storageSize)); err != nil {
			t.Fatal(err)
		}
		if writer.calls != 1 || !bytes.Equal(writer.Bytes(), []byte{0, 0, 0, 0}) {
			t.Fatalf("heartbeat scratch=%d changed framing", storageSize)
		}
	}
}
func TestFramerReadHeaderAndPayloadFailures(t *testing.T) {
	f := NewFramer(DefaultFramerSettings(1200))
	for _, wire := range [][]byte{nil, {0}, {0, 10}, {0, 10, 0}, {0, 10, 0, 0}, {0, 10, 0, 0, 1, 2, 3}, {0xff, 0xff, 0, 0}} {
		got, err := f.Read(bytes.NewReader(wire))
		if err == nil || got != nil {
			if got != nil {
				MessagePoolReturn(got)
			}
			t.Fatal("truncated or oversized frame accepted")
		}
	}
	// Failed header reads do not contaminate a later complete frame.
	var wire bytes.Buffer
	if err := f.Write(&wire, []byte("okay")); err != nil {
		t.Fatal(err)
	}
	got, err := f.Read(&wire)
	if err != nil || string(got) != "okay" {
		t.Fatal("reused read header retained stale bytes")
	}
	MessagePoolReturn(got)
}

// Repeats a complete frame without allocating in the read/write concurrency test.
type framerStorageCyclingReader struct {
	data     []byte
	position int
}

func (r *framerStorageCyclingReader) Read(p []byte) (int, error) {
	if r.position == len(r.data) {
		r.position = 0
	}
	n := copy(p, r.data[r.position:])
	r.position += n
	return n, nil
}

func BenchmarkFramerStorageWrite(b *testing.B) {
	for _, size := range []int{64, 1200, 12288} {
		counts := []int{1, 11}
		if size == 12288 {
			counts = []int{1}
		}
		for _, count := range counts {
			b.Run(fmt.Sprintf("payload%d/batch%d", size, count), func(b *testing.B) {
				framer := NewFramer(DefaultFramerSettings(size))
				messages := make([][]byte, count)
				for i := range messages {
					messages[i] = make([]byte, size)
				}
				storage := make([]byte, (size+4)*count)
				b.SetBytes(int64(size * count))
				b.ReportAllocs()
				for b.Loop() {
					if err := framer.WriteBatchWithStorage(io.Discard, messages, storage); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
