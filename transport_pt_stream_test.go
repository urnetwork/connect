// Scripted progress separates stream inactivity from total transfer duration
// without sleeping or depending on goroutine scheduling.
package connect

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// Each scheduled transition accepts at most one read buffer. An outstanding
// write expires if its current deadline precedes the next transition.
type packetTranslationScriptedWriteStream struct {
	now            time.Time
	deadline       time.Time
	progressDelays []time.Duration
	received       []byte
	writeBudgets   []time.Duration
}

// Records the budget at the current scripted instant.
func (self *packetTranslationScriptedWriteStream) SetWriteDeadline(deadline time.Time) error {
	self.deadline = deadline
	self.writeBudgets = append(self.writeBudgets, deadline.Sub(self.now))
	return nil
}

// Advances only at explicit progress or deadline transitions and keeps the
// accepted prefix observable after a partial write.
func (self *packetTranslationScriptedWriteStream) Write(data []byte) (n int, err error) {
	for n < len(data) {
		readyTime := self.now.Add(self.progressDelays[0])
		if !readyTime.Before(self.deadline) {
			self.now = self.deadline
			return n, os.ErrDeadlineExceeded
		}
		self.now = readyTime
		self.progressDelays = self.progressDelays[1:]
		m := min(packetTranslationTestIoByteCount, len(data)-n)
		self.received = append(self.received, data[n:n+m]...)
		n += m
	}
	return n, nil
}

// A progressing transfer can exceed one inactivity interval. The original
// single Write deterministically expires after accepting only its first block.
func TestPacketTranslationStreamWriteRenewsDeadlineAfterProgress(t *testing.T) {
	started := time.Unix(0, 0)
	stream := &packetTranslationScriptedWriteStream{
		now:            started,
		progressDelays: []time.Duration{3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second},
	}
	data := bytes.Repeat([]byte{0x13, 0x37, 0x42, 0x91}, packetTranslationTestIoByteCount)
	n, err := writePacketTranslationTestData(stream, data, func() time.Time {
		return stream.now.Add(5 * time.Second)
	})
	if err != nil || n != len(data) || !bytes.Equal(stream.received, data) {
		t.Fatalf("progressing write = %d/%d, %v; received=%d elapsed=%s", n, len(data), err, len(stream.received), stream.now.Sub(started))
	}
	if elapsed := stream.now.Sub(started); elapsed != 12*time.Second {
		t.Fatalf("transfer duration = %s, want four explicit progress transitions", elapsed)
	}
	for _, budget := range stream.writeBudgets {
		if budget != 5*time.Second {
			t.Fatalf("write budget changed to %s", budget)
		}
	}
}

// Renewing after progress still stops a stalled suffix at the same five-second
// inactivity bound and preserves the prefix already accepted by the stream.
func TestPacketTranslationStreamWriteStopsAtIdleDeadline(t *testing.T) {
	started := time.Unix(0, 0)
	stream := &packetTranslationScriptedWriteStream{
		now:            started,
		progressDelays: []time.Duration{3 * time.Second, 6 * time.Second},
	}
	data := make([]byte, 2*packetTranslationTestIoByteCount)
	n, err := writePacketTranslationTestData(stream, data, func() time.Time {
		return stream.now.Add(5 * time.Second)
	})
	if !errors.Is(err, os.ErrDeadlineExceeded) || n != packetTranslationTestIoByteCount {
		t.Fatalf("stalled write = %d, %v; want one accepted block then deadline", n, err)
	}
	if elapsed := stream.now.Sub(started); elapsed != 8*time.Second {
		t.Fatalf("stalled write expired at %s, want five seconds after its progress at three seconds", elapsed)
	}
}

// The caller's existing total attempt deadline remains authoritative even
// while every individual block makes progress inside the inactivity interval.
func TestPacketTranslationStreamWriteKeepsAttemptDeadline(t *testing.T) {
	started := time.Unix(0, 0)
	stream := &packetTranslationScriptedWriteStream{
		now: started,
		progressDelays: []time.Duration{
			3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second,
			3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second,
		},
	}
	attemptDeadline := started.Add(20 * time.Second)
	data := make([]byte, 8*packetTranslationTestIoByteCount)
	n, err := writePacketTranslationTestData(stream, data, func() time.Time {
		deadline := stream.now.Add(5 * time.Second)
		if attemptDeadline.Before(deadline) {
			return attemptDeadline
		}
		return deadline
	})
	if !errors.Is(err, os.ErrDeadlineExceeded) || n != 6*packetTranslationTestIoByteCount {
		t.Fatalf("attempt-limited write = %d, %v; want six accepted blocks then deadline", n, err)
	}
	if !stream.now.Equal(attemptDeadline) {
		t.Fatalf("attempt ended at %s, want %s", stream.now, attemptDeadline)
	}
}

// Supplies exact error results independently of progress scheduling.
type packetTranslationErrorWriteStream struct {
	writeByteCount     int
	writeErr           error
	deadlineErr        error
	completeWriteCount int
	writeCallCount     int
}

// Exposes a failure while arming the write deadline.
func (self *packetTranslationErrorWriteStream) SetWriteDeadline(time.Time) error {
	return self.deadlineErr
}

// Exposes a partial write without accepting a later suffix.
func (self *packetTranslationErrorWriteStream) Write(data []byte) (int, error) {
	self.writeCallCount++
	if self.writeCallCount <= self.completeWriteCount {
		return len(data), nil
	}
	return self.writeByteCount, self.writeErr
}

// A partial write must preserve its first error; a nil-error short write is
// invalid and must not skip data or be mistaken for a successful block.
func TestPacketTranslationStreamWritePreservesPartialError(t *testing.T) {
	writeErr := errors.New("synthetic stream failure")
	cases := []struct {
		completeWriteCount int
		writeByteCount     int
		writeErr           error
		wantErr            error
	}{
		{writeByteCount: 17, writeErr: writeErr, wantErr: writeErr},
		{completeWriteCount: 1, writeByteCount: 17, writeErr: writeErr, wantErr: writeErr},
		{writeByteCount: 17, wantErr: io.ErrShortWrite},
		{writeByteCount: 0, wantErr: io.ErrShortWrite},
		{completeWriteCount: 1, writeByteCount: 0, wantErr: io.ErrShortWrite},
	}
	for _, c := range cases {
		stream := &packetTranslationErrorWriteStream{
			writeByteCount:     c.writeByteCount,
			writeErr:           c.writeErr,
			completeWriteCount: c.completeWriteCount,
		}
		n, err := writePacketTranslationTestData(stream, make([]byte, 4096), time.Now)
		wantN := c.completeWriteCount*packetTranslationTestIoByteCount + c.writeByteCount
		wantCalls := c.completeWriteCount + 1
		if n != wantN || !errors.Is(err, c.wantErr) || stream.writeCallCount != wantCalls {
			t.Fatalf("partial write = %d, %v, calls=%d; want %d, %v, %d calls", n, err, stream.writeCallCount, wantN, c.wantErr, wantCalls)
		}
	}
}

// Deadline setup failure is the causal error; no write may follow it.
func TestPacketTranslationStreamWritePreservesDeadlineError(t *testing.T) {
	deadlineErr := errors.New("synthetic deadline failure")
	stream := &packetTranslationErrorWriteStream{deadlineErr: deadlineErr}
	n, err := writePacketTranslationTestData(stream, []byte("synthetic data"), time.Now)
	if n != 0 || !errors.Is(err, deadlineErr) || stream.writeCallCount != 0 {
		t.Fatalf("deadline failure = %d, %v, writes=%d", n, err, stream.writeCallCount)
	}
}
