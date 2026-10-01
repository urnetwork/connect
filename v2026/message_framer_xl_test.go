package connect

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"weak"
)

const framerXlTestRPCLimit = 3 * 1024 * 1024

type framerXlRecordingWriter struct {
	bytes.Buffer
	writes []int
}

func (w *framerXlRecordingWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, len(p))
	return w.Buffer.Write(p)
}

func framerXlTestPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*31 + 17)
	}
	return p
}

func TestFramerXlWireAndMessageBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 64, 1200, 2044, 2045, math.MaxUint16, math.MaxUint16 + 1, framerXlTestRPCLimit} {
		for _, mode := range []string{"write", "batch-singleton", "storage-singleton", "small-storage-singleton"} {
			t.Run(fmt.Sprintf("%s/%d", mode, n), func(t *testing.T) {
				f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
				p := framerXlTestPayload(n)
				before := bytes.Clone(p)
				w := &framerXlRecordingWriter{}
				var err error
				switch mode {
				case "write":
					err = f.Write(w, p)
				case "batch-singleton":
					err = f.WriteBatch(w, [][]byte{p})
				case "storage-singleton":
					err = f.WriteBatchWithStorage(w, [][]byte{p}, make([]byte, n+4))
				case "small-storage-singleton":
					err = f.WriteBatchWithStorage(w, [][]byte{p}, make([]byte, 3))
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := w.Bytes(); len(got) != n+4 || binary.BigEndian.Uint32(got[:4]) != uint32(n) || !bytes.Equal(got[4:], p) {
					t.Fatal("uint32 frame header or payload changed")
				}
				if len(w.writes) != 1 {
					t.Fatalf("writes=%v, want exactly one complete-frame write", w.writes)
				}
				got, err := f.Read(&w.Buffer)
				if err != nil {
					t.Fatal(err)
				}
				defer MessagePoolReturn(got)
				if !bytes.Equal(got, before) || !bytes.Equal(p, before) || w.Len() != 0 {
					t.Fatal("round trip changed caller data or left wire bytes")
				}
			})
		}
	}
}

// Fragmentation and coalescing are properties of the stream, independent of
// the message boundaries carried by the uint32 headers.
func TestFramerXlFragmentedAndCoalescedReads(t *testing.T) {
	f := NewFramerXl(DefaultFramerXlSettings(4096))
	want := [][]byte{nil, []byte("first"), framerXlTestPayload(4096), []byte("last")}
	var wire bytes.Buffer
	if err := f.WriteBatch(&wire, want); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 2, 3, 127, wire.Len()} {
		r := &framerXlFragmentedReader{Reader: bytes.NewReader(wire.Bytes()), chunk: chunk}
		for i, expected := range want {
			got, err := f.Read(r)
			if err != nil {
				t.Fatalf("chunk=%d frame=%d: %v", chunk, i, err)
			}
			ok := bytes.Equal(got, expected)
			MessagePoolReturn(got)
			if !ok {
				t.Fatalf("chunk=%d frame=%d: changed payload", chunk, i)
			}
		}
		if got, err := f.Read(r); got != nil || !errors.Is(err, io.EOF) {
			t.Fatalf("chunk=%d: end=%v, %v", chunk, got, err)
		}
	}
}

type framerXlFragmentedReader struct {
	io.Reader
	chunk int
}

func (r *framerXlFragmentedReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(len(p), r.chunk)])
}

func TestFramerXlReadHeaderIsBoundedAndDoesNotReadPayload(t *testing.T) {
	for _, n := range []uint32{0, 1, 65535, 65536, framerXlTestRPCLimit, framerXlTestRPCLimit + 1, math.MaxUint32} {
		f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
		wire := make([]byte, 4+8)
		binary.BigEndian.PutUint32(wire[:4], n)
		copy(wire[4:], "payload!")
		r := bytes.NewReader(wire)
		got, err := f.ReadHeader(r)
		if n > framerXlTestRPCLimit {
			if err == nil || got != 0 {
				t.Fatalf("oversized header %d accepted", n)
			}
		} else if err != nil || uint32(got) != n {
			t.Fatalf("length %d: got=%d err=%v", n, got, err)
		}
		if r.Len() != 8 {
			t.Fatalf("length %d: header consumed payload bytes", n)
		}
	}
	// The full uint32 header is legal on a host whose int can represent it;
	// reading only its header must never materialize that large message.
	if strconv.IntSize == 64 {
		limit := uint64(math.MaxUint32)
		f := NewFramerXl(DefaultFramerXlSettings(int(limit)))
		got, err := f.ReadHeader(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}))
		if err != nil || uint64(got) != limit {
			t.Fatalf("uint32 header boundary: got=%d err=%v", got, err)
		}
	}
}

func TestFramerXlReadTruncationAndErrors(t *testing.T) {
	f := NewFramerXl(DefaultFramerXlSettings(1200))
	for _, tc := range []struct {
		wire []byte
		want error
	}{
		{nil, io.EOF},
		{[]byte{0}, io.ErrUnexpectedEOF},
		{[]byte{0, 0}, io.ErrUnexpectedEOF},
		{[]byte{0, 0, 0}, io.ErrUnexpectedEOF},
		{[]byte{0, 0, 0, 10}, io.EOF},
		{[]byte{0, 0, 0, 10, 1, 2}, io.ErrUnexpectedEOF},
	} {
		got, err := f.Read(bytes.NewReader(tc.wire))
		if got != nil || !errors.Is(err, tc.want) {
			MessagePoolReturn(got)
			t.Fatalf("wire=%x: got=%v err=%v want=%v", tc.wire, got, err, tc.want)
		}
	}
	injected := errors.New("injected read failure")
	for _, prefix := range [][]byte{nil, {0, 0, 0, 10}, {0, 0, 0, 10, 1, 2, 3}} {
		r := io.MultiReader(bytes.NewReader(prefix), framerXlErrorReader{injected})
		got, err := f.Read(r)
		if got != nil || !errors.Is(err, injected) {
			MessagePoolReturn(got)
			t.Fatalf("injected read error was lost: %v", err)
		}
	}
}

func TestFramerXlZeroMessageLimit(t *testing.T) {
	f := NewFramerXl(DefaultFramerXlSettings(0))
	var wire bytes.Buffer
	if err := f.Write(&wire, nil); err != nil {
		t.Fatal(err)
	}
	got, err := f.Read(&wire)
	if err != nil || len(got) != 0 {
		MessagePoolReturn(got)
		t.Fatalf("empty frame under zero cap: %v", err)
	}
	MessagePoolReturn(got)
	if err := f.Write(&wire, []byte{1}); err == nil || wire.Len() != 0 {
		t.Fatal("nonempty write bypassed zero site cap")
	}
	if n, err := f.ReadHeader(bytes.NewReader([]byte{0, 0, 0, 1})); n != 0 || err == nil {
		t.Fatal("nonempty read bypassed zero site cap")
	}
}

type framerXlErrorReader struct{ err error }

func (r framerXlErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestFramerXlBatchBoundsAndAllOrNothingValidation(t *testing.T) {
	settings := DefaultFramerXlSettings(65536)
	settings.MaxBatchLen = 24
	f := NewFramerXl(settings)
	valid := [][]byte{framerXlTestPayload(8), framerXlTestPayload(8)}
	for _, withStorage := range []bool{false, true} {
		w := &framerXlRecordingWriter{}
		var err error
		if withStorage {
			err = f.WriteBatchWithStorage(w, valid, make([]byte, 24))
		} else {
			err = f.WriteBatch(w, valid)
		}
		if err != nil || len(w.writes) != 1 || w.Len() != 24 {
			t.Fatalf("exact batch bound: writes=%v bytes=%d err=%v", w.writes, w.Len(), err)
		}
	}
	for _, messages := range [][][]byte{
		{framerXlTestPayload(8), framerXlTestPayload(9)}, // batch bound
		{nil, make([]byte, 65537)},                       // last message bound
		{make([]byte, 65537)},                            // singleton bound
		{nil, make([]byte, 65536)},                       // valid frames, batch too large
	} {
		for _, withStorage := range []bool{false, true} {
			w := &framerXlRecordingWriter{}
			var err error
			if withStorage {
				err = f.WriteBatchWithStorage(w, messages, make([]byte, 128*1024))
			} else {
				err = f.WriteBatch(w, messages)
			}
			if err == nil || len(w.writes) != 0 {
				t.Fatalf("invalid batch emitted bytes: %v %v", w.writes, err)
			}
		}
	}
	w := &framerXlRecordingWriter{}
	if err := f.WriteBatchWithStorage(w, valid, make([]byte, 23)); err == nil || len(w.writes) != 0 {
		t.Fatal("undersized scratch was accepted or emitted bytes")
	}
	if err := f.Write(w, make([]byte, 65537)); err == nil || len(w.writes) != 0 {
		t.Fatal("oversized singleton was accepted or emitted bytes")
	}
	if err := f.WriteBatch(w, nil); err != nil || len(w.writes) != 0 {
		t.Fatal("empty batch performed I/O")
	}
	if err := f.WriteBatchWithStorage(w, nil, nil); err != nil || len(w.writes) != 0 {
		t.Fatal("empty storage batch performed I/O")
	}
}

func TestFramerXlInvalidSettingsAndSnapshot(t *testing.T) {
	invalid := []*FramerXlSettings{
		nil,
		{MaxMessageLen: -1, MaxBatchLen: 16 * 1024},
		{MaxMessageLen: 1200, MaxBatchLen: 0},
		{MaxMessageLen: 1200, MaxBatchLen: 3},
		{MaxMessageLen: 1200, MaxBatchLen: math.MaxInt},
		{MaxMessageLen: math.MaxInt, MaxBatchLen: 16 * 1024},
	}
	if strconv.IntSize == 64 {
		tooLarge := uint64(math.MaxUint32) + 1
		invalid = append(invalid, &FramerXlSettings{MaxMessageLen: int(tooLarge), MaxBatchLen: 16 * 1024})
	}
	for _, settings := range invalid {
		f := NewFramerXl(settings)
		w := &framerXlRecordingWriter{}
		r := bytes.NewReader([]byte{0, 0, 0, 0})
		if _, err := f.ReadHeader(r); err == nil || r.Len() != 4 {
			t.Fatal("invalid settings consumed header")
		}
		if p, err := f.Read(r); p != nil || err == nil || r.Len() != 4 {
			MessagePoolReturn(p)
			t.Fatal("invalid settings consumed frame")
		}
		if err := f.Write(w, nil); err == nil {
			t.Fatal("invalid settings permitted Write")
		}
		if err := f.WriteBatch(w, nil); err == nil {
			t.Fatal("invalid settings permitted WriteBatch")
		}
		if err := f.WriteBatchWithStorage(w, nil, nil); err == nil || len(w.writes) != 0 {
			t.Fatal("invalid settings permitted I/O")
		}
	}
	settings := DefaultFramerXlSettings(8)
	f := NewFramerXl(settings)
	settings.MaxMessageLen = -1
	settings.MaxBatchLen = -1
	if err := f.Write(io.Discard, make([]byte, 8)); err != nil {
		t.Fatalf("settings mutation changed existing framer: %v", err)
	}
	if err := f.Write(io.Discard, make([]byte, 9)); err == nil {
		t.Fatal("settings snapshot lost configured cap")
	}
}

// Exercise checked size arithmetic at host-int boundaries without allocating
// giant slices or fabricating invalid pointers. The same encoder helper is
// used to admit every production batch.
func TestFramerXlBatchByteCountOverflow(t *testing.T) {
	for _, tc := range []struct {
		total, messageLen, want int
		invalid                 bool
	}{
		{0, 0, 4, false},
		{8, 1200, 1212, false},
		{math.MaxInt - 5, 1, math.MaxInt, false},
		{math.MaxInt - 4, 0, math.MaxInt, false},
		{math.MaxInt - 3, 0, 0, true},
		{math.MaxInt, 0, 0, true},
		{math.MaxInt - 6, 3, 0, true},
		{0, math.MaxInt, 0, true},
		{-1, 0, 0, true},
		{0, -1, 0, true},
	} {
		got, err := framerXlAddFrameByteCount(tc.total, tc.messageLen)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Fatalf("total=%d message=%d: got=%d err=%v", tc.total, tc.messageLen, got, err)
		}
	}
}

func TestFramerXlSharedInputsAndStorageCanaries(t *testing.T) {
	f := NewFramerXl(DefaultFramerXlSettings(4096))
	backing := framerXlTestPayload(4096)
	before := bytes.Clone(backing)
	inputs := [][]byte{backing[:1200], backing[600:1800], backing[:1200]}
	allStorage := bytes.Repeat([]byte{0xc7}, 3*1204+32)
	w := &framerXlRecordingWriter{}
	if err := f.WriteBatchWithStorage(w, inputs, allStorage[16:len(allStorage)-16]); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 || !bytes.Equal(backing, before) {
		t.Fatal("batch changed shared source or made multiple writes")
	}
	for _, p := range inputs {
		got, err := f.Read(&w.Buffer)
		if err != nil {
			t.Fatal(err)
		}
		ok := bytes.Equal(got, p)
		MessagePoolReturn(got)
		if !ok {
			t.Fatal("shared source payload changed")
		}
	}
	if !bytes.Equal(allStorage[:16], bytes.Repeat([]byte{0xc7}, 16)) || !bytes.Equal(allStorage[len(allStorage)-16:], bytes.Repeat([]byte{0xc7}, 16)) {
		t.Fatal("framer exceeded scratch bounds")
	}
}

type framerXlFailWriter struct {
	calls, failCall, progress int
	err                       error
}

func (w *framerXlFailWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls != w.failCall {
		return len(p), nil
	}
	if w.progress < 0 {
		return len(p), w.err
	}
	return min(w.progress, len(p)), w.err
}

func TestFramerXlWriteFailureIsTerminal(t *testing.T) {
	injected := errors.New("injected write failure")
	f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
	for _, mode := range []string{"small", "large-frame", "singleton-storage", "batch", "batch-storage"} {
		for _, tc := range []struct {
			progress  int
			err, want error
		}{
			{0, nil, io.ErrShortWrite},
			{2, nil, io.ErrShortWrite},
			{0, injected, injected},
			{2, injected, injected},
			{-1, injected, injected},
		} {
			w := &framerXlFailWriter{failCall: 1, progress: tc.progress, err: tc.err}
			p := framerXlTestPayload(4096)
			before := bytes.Clone(p)
			var err error
			switch mode {
			case "small":
				err = f.Write(w, p[:64])
			case "large-frame":
				err = f.Write(w, p)
			case "singleton-storage":
				err = f.WriteBatchWithStorage(w, [][]byte{p}, make([]byte, len(p)+4))
			case "batch":
				err = f.WriteBatch(w, [][]byte{p, p})
			case "batch-storage":
				err = f.WriteBatchWithStorage(w, [][]byte{p, p}, make([]byte, 2*(len(p)+4)))
			}
			if !errors.Is(err, tc.want) || w.calls != w.failCall {
				t.Fatalf("%s: got err=%v calls=%d, want err=%v calls=%d", mode, err, w.calls, tc.want, w.failCall)
			}
			if !bytes.Equal(p, before) {
				t.Fatalf("%s: failure mutated input", mode)
			}
		}
	}
}

func TestFramerXlDuplexReadWrite(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	deadline := time.Now().Add(10 * time.Second)
	if err := left.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := right.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	frames := []int{0, 1, 1200, 65536, framerXlTestRPCLimit, 17}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, conn := range []net.Conn{left, right} {
		f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, n := range frames {
				got, err := f.Read(conn)
				if err != nil {
					errs <- err
					return
				}
				ok := bytes.Equal(got, framerXlTestPayload(n))
				MessagePoolReturn(got)
				if !ok {
					errs <- fmt.Errorf("duplex payload mismatch at %d bytes", n)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			storage := make([]byte, 16*1024)
			for _, n := range frames {
				if err := f.WriteBatchWithStorage(conn, [][]byte{framerXlTestPayload(n)}, storage); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestFramerXlCancellationUnblocksRead(t *testing.T) {
	for _, headerOnly := range []bool{false, true} {
		left, right := net.Pipe()
		f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
		done := make(chan error, 1)
		go func() {
			if headerOnly {
				_, err := f.ReadHeader(left)
				done <- err
			} else {
				p, err := f.Read(left)
				MessagePoolReturn(p)
				done <- err
			}
		}()
		if !headerOnly {
			if _, err := right.Write([]byte{0, 0, 4, 0}); err != nil {
				t.Fatal(err)
			}
		}
		left.Close()
		right.Close()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("closed transport returned a successful frame")
			}
		case <-time.After(time.Second):
			t.Fatal("closed transport left read blocked")
		}
	}
}

func TestFramerXlCancellationUnblocksWrite(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
	done := make(chan error, 1)
	go func() {
		done <- f.Write(left, make([]byte, framerXlTestRPCLimit))
	}()
	// Reading just one byte guarantees that Write started but cannot finish
	// the complete frame until the stream is closed.
	var first [1]byte
	if _, err := io.ReadFull(right, first[:]); err != nil {
		t.Fatal(err)
	}
	left.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed transport returned a successful write")
		}
	case <-time.After(time.Second):
		t.Fatal("closed transport left write blocked")
	}
}

func TestFramerXlStorageDoesNotAllocate(t *testing.T) {
	f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
	for _, messages := range [][][]byte{
		{nil},
		{make([]byte, 1200)},
		{make([]byte, 1200), make([]byte, 1200)},
		{make([]byte, framerXlTestRPCLimit)},
	} {
		n := 0
		for _, p := range messages {
			n += len(p) + 4
		}
		storage := make([]byte, n)
		if got := testing.AllocsPerRun(100, func() {
			if err := f.WriteBatchWithStorage(io.Discard, messages, storage); err != nil {
				panic(err)
			}
		}); got != 0 {
			t.Fatalf("%d framed bytes: allocations=%g want=0", n, got)
		}
	}
}

func TestFramerXlPoolOwnershipAndFrameLocalScratch(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
	beforeTaken, beforeReturned, _ := MessagePoolCounts()
	beforeUnpooled, beforeUnpooledBytes := MessagePoolUnpooledCounts()
	for _, wire := range [][]byte{{0, 0, 4, 0, 1}, {0xff, 0xff, 0xff, 0xff}} {
		if p, err := f.Read(bytes.NewReader(wire)); p != nil || err == nil {
			MessagePoolReturn(p)
			t.Fatal("malformed frame returned a buffer")
		}
	}
	p := MessagePoolGet(1200)
	copy(p, framerXlTestPayload(len(p)))
	var wire bytes.Buffer
	if err := f.Write(&wire, p); err != nil {
		t.Fatal(err)
	}
	if pooled, shared := MessagePoolCheck(p); !pooled || shared {
		t.Fatal("write stole or shared caller pool ownership")
	}
	got, err := f.Read(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if pooled, shared := MessagePoolCheck(got); !pooled || shared {
		t.Fatal("successful read did not transfer pooled ownership")
	}
	MessagePoolReturn(got)
	MessagePoolReturn(p)
	if afterUnpooled, _ := MessagePoolUnpooledCounts(); afterUnpooled != beforeUnpooled {
		t.Fatal("small frames or invalid lengths allocated large scratch")
	}
	large := framerXlTestPayload(framerXlTestRPCLimit)
	for _, mode := range []string{"write", "batch-singleton", "small-storage"} {
		w := &framerXlFrameLocalWriter{payload: large}
		var err error
		switch mode {
		case "write":
			err = f.Write(w, large)
		case "batch-singleton":
			err = f.WriteBatch(w, [][]byte{large})
		case "small-storage":
			err = f.WriteBatchWithStorage(w, [][]byte{large}, make([]byte, 2048))
		}
		if err != nil || w.calls != 1 {
			t.Fatalf("%s: large frame did not use one frame-local write: %v", mode, err)
		}
		assertFramerXlFrameNotRetained(t, f, w)
	}
	for _, injected := range []error{io.ErrClosedPipe, io.ErrShortWrite} {
		w := &framerXlFrameLocalWriter{payload: large, err: injected}
		if err := f.Write(w, large); !errors.Is(err, injected) || w.calls != 1 {
			t.Fatalf("injected failure changed or retried: %v", err)
		}
		assertFramerXlFrameNotRetained(t, f, w)
	}
	if count, byteCount := MessagePoolUnpooledCounts(); count-beforeUnpooled != 5 || byteCount-beforeUnpooledBytes != uint64(5*(len(large)+4)) {
		t.Fatalf("large writes allocated count=%d bytes=%d, want five exact framed allocations", count-beforeUnpooled, byteCount-beforeUnpooledBytes)
	}
	afterTaken, afterReturned, _ := MessagePoolCounts()
	if afterTaken-beforeTaken != afterReturned-beforeReturned {
		t.Fatalf("pool imbalance: taken=%d returned=%d", afterTaken-beforeTaken, afterReturned-beforeReturned)
	}
	// Materializing one large frame acquires one exact large allocation. Return
	// does not retain it in a pool, independent of MaxMessageLen on idle framers.
	header := []byte{0, 0x30, 0, 0}
	largeRead, err := f.Read(io.MultiReader(bytes.NewReader(header), bytes.NewReader(large)))
	if err != nil || !bytes.Equal(largeRead, large) {
		MessagePoolReturn(largeRead)
		t.Fatalf("large read: %v", err)
	}
	if pooled, _ := MessagePoolCheck(largeRead); pooled {
		t.Fatal("3 MiB frame retained in a pool class")
	}
	if MessagePoolReturn(largeRead) {
		t.Fatal("large frame was retained after release")
	}
	if afterUnpooled, _ := MessagePoolUnpooledCounts(); afterUnpooled-beforeUnpooled != 6 {
		t.Fatalf("large read and write allocation count=%d want=6", afterUnpooled-beforeUnpooled)
	}
}

func TestFramerXlFrameLocalPoolRetentionBoundary(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	pools := orderedMessagePools()
	largest := pools[len(pools)-1].size
	f := NewFramerXl(DefaultFramerXlSettings(largest + 1))
	for _, size := range []int{2044, 2045, largest - 4, largest - 3} {
		w := &framerXlPoolBoundaryWriter{}
		before, _ := MessagePoolUnpooledCounts()
		if err := f.Write(w, framerXlTestPayload(size)); err != nil {
			t.Fatal(err)
		}
		after, _ := MessagePoolUnpooledCounts()
		pooled := size+4 <= largest
		if w.calls != 1 || w.pooled != pooled {
			t.Fatalf("payload=%d: writes=%d pooled=%t want pooled=%t", size, w.calls, w.pooled, pooled)
		}
		if pooled {
			if before != after || !MessagePoolReturn(w.observed) {
				t.Fatal("pooled frame-local owner was not released immediately")
			}
		} else if after-before != 1 {
			t.Fatal("frame above existing retention threshold entered a pool")
		}
	}
}

type framerXlPoolBoundaryWriter struct {
	calls    int
	pooled   bool
	observed []byte
}

func (w *framerXlPoolBoundaryWriter) Write(p []byte) (int, error) {
	w.calls++
	w.pooled, _ = MessagePoolCheck(p)
	if w.pooled {
		w.observed = MessagePoolShareReadOnly(p)
	}
	return len(p), nil
}

type framerXlFrameLocalWriter struct {
	payload []byte
	calls   int
	err     error
	frame   weak.Pointer[byte]
}

func (w *framerXlFrameLocalWriter) Write(p []byte) (int, error) {
	w.calls++
	if len(p) != len(w.payload)+4 || binary.BigEndian.Uint32(p[:4]) != uint32(len(w.payload)) || !bytes.Equal(p[4:], w.payload) || &p[4] == &w.payload[0] {
		return 0, fmt.Errorf("incorrect complete frame or borrowed input")
	}
	if pooled, _ := MessagePoolCheck(p); pooled {
		return 0, fmt.Errorf("large frame entered a retained pool class")
	}
	w.frame = weak.Make(&p[0])
	if errors.Is(w.err, io.ErrShortWrite) {
		return len(p) - 1, nil
	}
	return len(p), w.err
}

func assertFramerXlFrameNotRetained(t *testing.T, f *FramerXl, w *framerXlFrameLocalWriter) {
	t.Helper()
	// The large backing is not a tiny object, so weak pointer collection is
	// deterministic after GC. Keep the framer, writer and caller payload live:
	// this detects accidental retention by either the framer or a pool.
	runtime.GC()
	if w.frame.Value() != nil {
		t.Fatal("completed large frame is still retained")
	}
	runtime.KeepAlive(f)
	runtime.KeepAlive(w)
}

func BenchmarkFramerXlWrite(b *testing.B) {
	for _, size := range []int{64, 1200, 65536, framerXlTestRPCLimit} {
		for _, mode := range []string{"frame-local", "storage", "legacy-prefix"} {
			b.Run(fmt.Sprintf("%s/%d", mode, size), func(b *testing.B) {
				f := NewFramerXl(DefaultFramerXlSettings(framerXlTestRPCLimit))
				p := make([]byte, size)
				messages := [][]byte{p}
				storage := make([]byte, size+4)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for b.Loop() {
					var err error
					if mode == "storage" {
						err = f.WriteBatchWithStorage(io.Discard, messages, storage)
					} else if mode == "frame-local" {
						err = f.Write(io.Discard, p)
					} else {
						err = framerXlLegacyPrefixWrite(f, io.Discard, p)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Benchmark-only control preserves the previous two-handoff large-write
// behavior. Production always emits a large frame in one stream Write.
func framerXlLegacyPrefixWrite(f *FramerXl, w io.Writer, message []byte) error {
	if err := f.validateMessageLen(len(message)); err != nil {
		return err
	}
	prefixLen := min(2*1024, len(message)+4)
	prefix := MessagePoolGet(prefixLen)
	defer MessagePoolReturn(prefix)
	binary.BigEndian.PutUint32(prefix[:4], uint32(len(message)))
	copy(prefix[4:], message)
	if err := writeFramerXlBytes(w, prefix); err != nil {
		return err
	}
	if copied := prefixLen - 4; copied < len(message) {
		return writeFramerXlBytes(w, message[copied:])
	}
	return nil
}

// This benchmark includes both endpoints over real localhost TCP/TLS, verifies
// every decoded payload, and reports writes above TLS. A single large TLS Write
// still emits multiple TLS records. Large receive allocations are common to
// both arms; frame-local additionally allocates the complete outbound frame.
// Setup and 16 warm frames are excluded. No VPN, network RTT, or mobile memory
// profile is modeled here.
func BenchmarkFramerXlTLSWrite(b *testing.B) {
	for _, size := range []int{1200, 65536, framerXlTestRPCLimit} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			for _, mode := range []string{"legacy-prefix", "frame-local"} {
				b.Run(mode, func(b *testing.B) { benchmarkFramerXlTLSWrite(b, size, mode) })
			}
		})
	}
}

type framerXlBenchmarkWriter struct {
	io.Writer
	writes int
}

func (w *framerXlBenchmarkWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Writer.Write(p)
}

func benchmarkFramerXlTLSWrite(b *testing.B, size int, mode string) {
	b.StopTimer()
	f := NewFramerXl(DefaultFramerXlSettings(size))
	payload := framerXlTestPayload(size)
	finished := make(chan error, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			finished <- err
			return
		}
		defer raw.Close()
		if err := raw.SetDeadline(time.Now().Add(time.Minute)); err != nil {
			finished <- err
			return
		}
		_, err = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: urnetwork-framerxl/1\r\n\r\n")
		if err == nil {
			err = buffered.Flush()
		}
		if err != nil {
			finished <- err
			return
		}
		for i := range b.N + 16 {
			p, err := f.Read(buffered.Reader)
			correct := err == nil && bytes.Equal(p, payload)
			MessagePoolReturn(p)
			if !correct {
				finished <- fmt.Errorf("frame %d payload or read error: %v", i, err)
				return
			}
			if i == 15 {
				if _, err := raw.Write([]byte{1}); err != nil {
					finished <- err
					return
				}
			}
		}
		finished <- nil
	}))
	defer server.Close()
	config := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	config.MinVersion = tls.VersionTLS13
	config.NextProtos = []string{"http/1.1"}
	conn, err := tls.Dial("tcp", server.Listener.Addr().String(), config)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		b.Fatal(err)
	}
	if _, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: urnetwork-framerxl/1\r\n\r\n"); err != nil {
		b.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		b.Fatalf("benchmark upgrade failed: %v", err)
	}
	writer := &framerXlBenchmarkWriter{Writer: conn}
	write := func() error {
		if mode == "legacy-prefix" {
			return framerXlLegacyPrefixWrite(f, writer, payload)
		}
		return f.Write(writer, payload)
	}
	for range 16 {
		if err := write(); err != nil {
			b.Fatal(err)
		}
	}
	var ready [1]byte
	if _, err := io.ReadFull(reader, ready[:]); err != nil || ready[0] != 1 {
		b.Fatalf("benchmark warmup failed: %v", err)
	}
	writer.writes = 0
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	b.StartTimer()
	for range b.N {
		if err := write(); err != nil {
			b.Fatal(err)
		}
	}
	if err := <-finished; err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	b.ReportMetric(float64(writer.writes)/float64(b.N), "TLS-writes/frame")
}
