package extender

// The measurement streams (lag.dev/PLAN.md M5 to M7).
//
// A session's load is a download phase, then an upload phase. Each phase's
// window opens at its first byte, so connection setup does not count, and lasts
// the ticket's downloadMs or uploadMs; the browser opens maxStreams streams
// into it. A download stream is AES-128-CTR keystream under a fresh random key,
// written from one 64 KiB buffer through the window, and past it until the
// browser closes the stream, within a grace, or until a cap runs out (M5,
// amendment 15). An upload stream is a websocket of binary messages carrying
// AES-128-CTR keystream under the session's upload key, which the extender
// regenerates and compares read by read as the bytes arrive -- or, on hardware
// without AES instructions, on a random 1 in 16 messages chosen after each
// arrives (M6). Each read of an upload counts as it arrives, spread over the
// time since the stream's previous read (amendment 14): reads that arrive back
// to back spread together, a stream's first reads at the rate of the ones
// after, and reads that arrive past the window's end for what they carry from
// before it (amendment 15). Each stream holds one 64 KiB data buffer and one
// 64 KiB keystream buffer; nothing grows with the stream, and nothing is
// written to disk (M7).

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/cpu"
)

// The size of an upload message and of each stream buffer (M6, M7).
const lagApiBufferByteCount = 64 * 1024

// The most intervals a phase's series holds: A7 accepts at most 40 per
// direction.
const lagApiMaxIntervalCount = 40

// The furthest back an upload read is spread from its arrival (M6, amendment
// 14), so the bytes that end a stall are not smeared far back into it. The
// browser spreads its download reads with the same bound. So it is also how
// long past the window's end an upload's reads may still count: none that
// arrives later reaches back before the end (amendment 15).
const lagSpreadMax = time.Second

// Reads of an upload stream that arrive within this of the first read of
// their group, inclusive, are one group, back to back, and spread from where
// the group's span starts (M6 R1, amendment 15): a burst released after a
// stall spreads, all of it, over the stall. It is measured from the group's
// first read, not the previous read, so a steady fast stream is cut into
// groups this long rather than smeared over a long span. The browser groups
// its download reads the same way. So a stream is done this long after the
// first read of its first group that begins at or after the window's end:
// only a read that joins that group can count before the end, and every
// later group spans from the end on (M6 R3).
const lagSpreadJoin = 2 * time.Millisecond

// How long past a download window its streams may go on (M5, M7). Each keeps
// writing until the browser closes it, so the bytes still reaching the browser
// cover its own window, which opened a one-way delay after the extender's
// (amendment 15). A write still under way when the grace runs out is cut,
// which ends the connection.
const lagApiPhaseGrace = 2 * time.Second

// The most streams per direction, whatever a ticket says (M5).
const lagApiMaxStreamCount = 16

// How long a websocket close waits for the peer's own close frame before the
// connection is released (RFC 6455 7.1.1). Closing a socket while data still
// arrives resets it, and the reset can cost the peer the close code.
const lagApiCloseWait = 2 * time.Second

// On hardware without AES instructions one upload message in this many is
// verified (M6).
const lagApiSampleCount = 16

// The websocket close codes (M4, M6).
const (
	lagApiCloseCodeNormal   = websocket.CloseNormalClosure
	lagApiCloseCodeAway     = websocket.CloseGoingAway
	lagApiCloseCodeRefused  = 4403
	lagApiCloseCodeLimited  = 4429
	lagApiCloseCodeMismatch = 4400
	lagApiCloseCodeTimeout  = 4408
	lagApiCloseCodeInternal = 4500
)

// A zero source: keystream XOR zeros is the keystream itself. Shared and never
// written.
var lagApiZeroBytes [lagApiBufferByteCount]byte

// Whether this cpu has the AES instructions crypto/aes uses, which decides
// whether an upload is verified read by read or sampled (M6). It
// mirrors crypto/aes's own choice: AES-NI with SSE4.1 and SSSE3 on amd64, the
// Armv8 AES instructions on arm64, POWER8 on ppc64, KM-AES with KMC on s390x,
// and the generic tables everywhere else.
var lagApiAesHardware = func() bool {
	switch runtime.GOARCH {
	case "amd64":
		return cpu.X86.HasAES && cpu.X86.HasSSE41 && cpu.X86.HasSSSE3
	case "arm64":
		return cpu.ARM64.HasAES
	case "ppc64", "ppc64le":
		return true
	case "s390x":
		return cpu.S390X.HasAES && cpu.S390X.HasAESCBC
	default:
		return false
	}
}()

// Chooses whether one sampled upload message is verified, after it arrived
// (M6), from the runtime's unpredictable source.
func lagApiSampleMessage() bool {
	return mathrand.Uint32N(lagApiSampleCount) == 0
}

// The counter block of upload stream k at byte offset o (M6): k in bytes 0 to
// 7 and o/16 in bytes 8 to 15, each a big-endian uint64. In WebCrypto that is
// AES-CTR with this counter and a 64-bit length; cipher.NewCTR increments the
// whole block, which agrees for every offset a stream can reach.
func lagApiCounterBlock(streamIndex uint64, offset uint64) [aes.BlockSize]byte {
	var counterBlock [aes.BlockSize]byte
	binary.BigEndian.PutUint64(counterBlock[0:8], streamIndex)
	binary.BigEndian.PutUint64(counterBlock[8:16], offset/aes.BlockSize)
	return counterBlock
}

// The keystream of a download stream: AES-128-CTR under a fresh random key
// (M5). Nothing checks the bytes; they are random so nothing on the path can
// shrink them.
func newLagApiDownloadKeystream() (cipher.Stream, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	var counterBlock [aes.BlockSize]byte
	return cipher.NewCTR(block, counterBlock[:]), nil
}

// The direction of a phase (M4).
type lagApiDirection int

const (
	lagApiDirectionDownload lagApiDirection = iota + 1
	lagApiDirectionUpload
)

// How a phase ended (M4, M6).
type lagApiPhaseEnd int

const (
	lagApiPhaseRunning lagApiPhaseEnd = iota
	// its window ran out
	lagApiPhaseEndTime
	// the ticket's byte cap for the direction, or the daily budget
	lagApiPhaseEndCap
	// an upload message was not the expected keystream
	lagApiPhaseEndMismatch
	// no first byte within the first-byte wait
	lagApiPhaseEndTimeout
	// the load or the session ended it
	lagApiPhaseEndClosed
)

// One phase of a load, its download or its upload (M5, M6): the streams the
// browser opens into it, its window from the first byte, the bytes the streams
// move per interval of the window, and the caps they draw on. Its series is
// sized once, so a stream of any length adds to it without growing it (M7).
//
// The phase ends at its window's end, or sooner when a cap, a mismatch, the
// load or the session ends it (M4, M8). A sampled upload failure can invalidate
// a time end until its admitted bytes finish verification. Ended by its time,
// a phase still has work until its window is final (amendment 15): an upload's
// reads still count for what they carry from before the end, until every
// stream still open is done or lagSpreadMax has passed (M6 R3); and a
// download's streams write on until the browser closes them, within
// lagApiPhaseGrace (M5). Closing the phase then -- the load moved on, or the
// session ended -- makes its window final at once. Ended any other way, its
// counting is final at its end. Upload settlement also waits for any sampled
// message with admitted bytes to finish; closing abandons that credit as
// unverified, without waiting for the reader.
type lagApiPhase struct {
	api            *extenderLagApi
	direction      lagApiDirection
	duration       time.Duration
	interval       time.Duration
	maxStreamCount int
	// wakes the session, at the first byte, at the end, and when the window
	// is final and its sampled verification has settled
	notify func()

	// done when the phase ends; an upload stream is then closed, though read
	// on while its reads may still count (amendment 15)
	ctx    context.Context
	cancel context.CancelFunc

	// the ticket's cap for this direction, and the bytes left under it
	maxByteCount int64
	capRemaining atomic.Int64
	// the series of the window, set at the first byte
	series atomic.Pointer[lagApiSeries]

	stateLock      sync.Mutex
	firstByteTime  time.Time
	endTime        time.Time
	windowTimer    *time.Timer
	firstByteTimer *time.Timer
	openStreams    []bool
	openedStreams  []bool
	// how each upload stream's reads count (M6), and the latest arrival of
	// any counted read (M4)
	streamCounts    []lagApiStreamCount
	lastCountedTime time.Time
	end             lagApiPhaseEnd
	endedTime       time.Time
	// a sampled message failed, or its admitted bytes could not be checked
	verificationFailed bool
	// nothing more counts in the window, nor is sent past it (amendment 15);
	// sampled verification may still hold settlement after counting ends
	// the timer that makes an upload's window final lagSpreadMax after its
	// end, and the one that makes it final when its last open stream is done,
	// set for that time
	final         bool
	finalTimer    *time.Timer
	doneTimer     *time.Timer
	doneTimerTime time.Time
}

// How one upload stream's reads count (M6, amendment 15). Its size is fixed,
// however long the stream (M7).
type lagApiStreamCount struct {
	// when the stream's previous read arrived; zero before its first
	previousTime time.Time
	// when the first read of its current group arrived, and where the
	// group's span starts
	groupTime      time.Time
	groupSpanStart time.Time
	// the bytes of its first group, held uncounted until its second group
	// begins; zero once they count
	heldByteCount int
	// lagSpreadJoin after the first read of its first group that began at or
	// after the window's end, zero before: nothing it reads after can count
	doneTime time.Time
	// a mismatch past the window's end stopped it, so nothing it reads counts
	stopped bool
	// its last message, a short one, has arrived whole: nothing valid can
	// follow it (M6), so nothing it reads after counts
	finished bool
	// a sampled message is being read; its admitted bytes remain unverified
	// until the whole message's sample decision and comparison finish
	sampling   bool
	unverified bool
}

// Whether upload stream k is done at now, with the phase's lock held: nothing
// it reads from now on can count (M6 R3, amendment 15).
func (self *lagApiPhase) streamDoneWithLock(streamIndex int, now time.Time) bool {
	count := &self.streamCounts[streamIndex]
	return count.stopped || count.finished || (!count.doneTime.IsZero() && now.After(count.doneTime))
}

// Opens a phase now (M4). Its window waits for the first byte, for at most
// firstByteWait.
func newLagApiPhase(
	api *extenderLagApi,
	parentCtx context.Context,
	direction lagApiDirection,
	duration time.Duration,
	interval time.Duration,
	maxStreamCount int,
	maxByteCount int64,
	firstByteWait time.Duration,
	notify func(),
) *lagApiPhase {
	maxStreamCount = max(0, min(maxStreamCount, lagApiMaxStreamCount))
	ctx, cancel := context.WithCancel(parentCtx)
	phase := &lagApiPhase{
		api:            api,
		direction:      direction,
		duration:       duration,
		interval:       interval,
		maxStreamCount: maxStreamCount,
		notify:         notify,
		ctx:            ctx,
		cancel:         cancel,
		maxByteCount:   maxByteCount,
		openStreams:    make([]bool, maxStreamCount),
		openedStreams:  make([]bool, maxStreamCount),
		streamCounts:   make([]lagApiStreamCount, maxStreamCount),
	}
	phase.capRemaining.Store(maxByteCount)
	phase.stateLock.Lock()
	defer phase.stateLock.Unlock()
	phase.firstByteTimer = time.AfterFunc(firstByteWait, func() {
		phase.finish(lagApiPhaseEndTimeout)
	})
	return phase
}

// The window's end, opening the window at now when this is the phase's first
// byte (M5, M6), and whether the window is still open. Whoever first sees the
// window has run out ends the phase, so its end does not wait on a timer.
func (self *lagApiPhase) window(now time.Time) (time.Time, bool) {
	opened := false
	ended := false
	finalized := false
	endTime, ok := func() (time.Time, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.end != lagApiPhaseRunning {
			return self.endTime, false
		}
		if self.firstByteTime.IsZero() {
			self.firstByteTime = now
			self.endTime = now.Add(self.duration)
			self.series.Store(newLagApiSeries(now, self.interval, self.duration+lagApiPhaseGrace))
			self.firstByteTimer.Stop()
			self.windowTimer = time.AfterFunc(self.duration, func() {
				self.finish(lagApiPhaseEndTime)
			})
			opened = true
		}
		if !now.Before(self.endTime) {
			ended, finalized = self.finishWithLock(now, lagApiPhaseEndTime)
			return self.endTime, false
		}
		return self.endTime, true
	}()
	if opened {
		self.notify()
	}
	self.changed(ended, finalized)
	return endTime, ok
}

// Whether a download stream may write at now, opening the window when this is
// the phase's first byte (M5), and the window's end. A stream writes through
// the window, and past it until the browser closes the stream, within
// lagApiPhaseGrace: the browser's window opened a one-way delay after the
// extender's, and the bytes that reach it cover its window only if the writes
// go on past this one (amendment 15). Writes past the window stop sooner when
// the load moves on or the session ends, which closes the phase.
func (self *lagApiPhase) writable(now time.Time) (time.Time, bool) {
	if endTime, ok := self.window(now); ok {
		return endTime, true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.endTime, self.end == lagApiPhaseEndTime && !self.final && now.Before(self.endTime.Add(lagApiPhaseGrace))
}

// Whether upload stream k's next message may still count, at now, opening the
// window when this is the phase's first byte (M6): while the window is open,
// and past its end until the stream is done or the window is final (amendment
// 15).
func (self *lagApiPhase) takes(streamIndex int, now time.Time) bool {
	if _, ok := self.window(now); ok {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.countingWithLock() && !self.streamDoneWithLock(streamIndex, now)
}

// Whether a stream may still move bytes in the phase: it has not ended, and
// its window is waiting for its first byte or still open.
func (self *lagApiPhase) running(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.end == lagApiPhaseRunning && (self.firstByteTime.IsZero() || now.Before(self.endTime))
}

// Opens stream k (M5, M6): false when k is out of range or already open, or
// the phase is over.
func (self *lagApiPhase) openStream(streamIndex int) bool {
	if !self.running(time.Now()) {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if streamIndex < 0 || self.maxStreamCount <= streamIndex || self.openStreams[streamIndex] {
		return false
	}
	self.openStreams[streamIndex] = true
	self.openedStreams[streamIndex] = true
	return true
}

// Closes stream k (M5, M6). An upload stream that closes with its first group
// still held counts the group by rule S, since no second group will come to
// place it; and once the window has run out, it may have been the last open
// stream not yet done, which makes the window final (amendment 15).
func (self *lagApiPhase) closeStream(streamIndex int) {
	finalized := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.openStreams[streamIndex] = false
		if !self.countingWithLock() {
			return false
		}
		self.placeHeldWithLock(streamIndex)
		return self.finalIfDoneWithLock(time.Now())
	}()
	self.changed(false, finalized)
}

// Finishes upload stream k at now: its last message, a short one, has arrived
// whole, checked as its verification checks it (M6). Nothing valid can follow
// a short message, so the stream is done, as one that closed is: its first
// group, if it is still held, counts by rule S, since no second group will
// come to place it, and it no longer holds the window open past its end. A
// browser near the window's end sends a stream's last message short, to fit
// the room its flow control has left; with no read past the end, under the
// rule that a stream is done lagSpreadJoin into its first group past the end
// it would hold the window open lagSpreadMax (M6 R3, amendment 15).
func (self *lagApiPhase) finishStream(streamIndex int, now time.Time) {
	finalized := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.countingWithLock() {
			return false
		}
		self.streamCounts[streamIndex].finished = true
		self.placeHeldWithLock(streamIndex)
		return self.finalIfDoneWithLock(now)
	}()
	self.changed(false, finalized)
}

// The distinct streams the browser opened in the phase (M8).
func (self *lagApiPhase) streamCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	streamCount := 0
	for _, opened := range self.openedStreams {
		if opened {
			streamCount += 1
		}
	}
	return streamCount
}

// Ends the phase, keeping the first reason. The reason is set before the
// context is done, so whatever the context wakes reads it. A phase closed or
// capped once its window had already run out ended by its time, whichever
// came first to say so, so the outcome does not depend on a timer's delay.
// Closing a phase that ended by its time ends what still goes on past its
// window, which is then final (amendment 15).
func (self *lagApiPhase) finish(end lagApiPhaseEnd) {
	ended, finalized := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		abandoned := false
		if end == lagApiPhaseEndClosed {
			// Closing cannot wait for a socket reader. Any unresolved credit
			// stays unverified, including if its comparison later succeeds.
			for streamIndex := range self.streamCounts {
				count := &self.streamCounts[streamIndex]
				if count.unverified {
					self.verificationFailed = true
					abandoned = true
				}
				count.sampling = false
				count.unverified = false
			}
		}
		ended, finalized := self.finishWithLock(time.Now(), end)
		return ended, finalized || abandoned
	}()
	self.changed(ended, finalized)
}

// finish at now, with the phase's lock held: whether this ended the phase,
// and whether it made the window final. The caller then calls changed.
func (self *lagApiPhase) finishWithLock(now time.Time, end lagApiPhaseEnd) (bool, bool) {
	if self.end != lagApiPhaseRunning {
		if end == lagApiPhaseEndClosed && self.end == lagApiPhaseEndTime && !self.final {
			self.finalizeWithLock()
			return false, true
		}
		return false, false
	}
	closing := end == lagApiPhaseEndClosed
	windowOver := !self.endTime.IsZero() && !now.Before(self.endTime)
	if windowOver && (end == lagApiPhaseEndClosed || end == lagApiPhaseEndCap) {
		end = lagApiPhaseEndTime
	}
	self.end = end
	self.endedTime = now
	if end == lagApiPhaseEndTime && !self.endTime.IsZero() {
		// the window's own end, whatever the timer's delay
		self.endedTime = self.endTime
	}
	self.firstByteTimer.Stop()
	if self.windowTimer != nil {
		self.windowTimer.Stop()
	}
	if end != lagApiPhaseEndTime || closing || self.endTime.IsZero() {
		self.finalizeWithLock()
		return true, true
	}
	if self.direction == lagApiDirectionUpload {
		// the reads that arrive past the end still count for the part of
		// their spans before it, until every open stream is done or no read
		// can reach back before the end
		self.finalTimer = time.AfterFunc(self.endTime.Add(lagSpreadMax).Sub(now), self.finalize)
		return true, self.finalIfDoneWithLock(now)
	}
	return true, false
}

// After a change made with the lock held: once the phase has ended its
// context is done, and the session wakes to read the change.
func (self *lagApiPhase) changed(ended bool, finalized bool) {
	if ended {
		self.cancel()
	}
	if ended || finalized {
		self.notify()
	}
}

// Makes the window of a phase that ended final, if it is not yet: the timer's,
// lagSpreadMax past an upload's end (M6 R3, amendment 15).
func (self *lagApiPhase) finalize() {
	finalized := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.end == lagApiPhaseRunning || self.final {
			return false
		}
		self.finalizeWithLock()
		return true
	}()
	self.changed(false, finalized)
}

// Makes the window final, with the phase's lock held: nothing more counts in
// it (amendment 15). A first group still held then counts by rule S, since no
// second group will come to place it.
func (self *lagApiPhase) finalizeWithLock() {
	if self.final {
		return
	}
	self.final = true
	if self.finalTimer != nil {
		self.finalTimer.Stop()
	}
	if self.doneTimer != nil {
		self.doneTimer.Stop()
	}
	for streamIndex := range self.streamCounts {
		self.placeHeldWithLock(streamIndex)
	}
}

// Makes an upload's window final at now once it has run out and every stream
// still open is done (M6 R3, amendment 15), with the phase's lock held. When
// every open stream has a time it is done by, though not all have passed,
// sets the done timer for the latest, so the window is final then without
// waiting for another read. Whether it is final now.
func (self *lagApiPhase) finalIfDoneWithLock(now time.Time) bool {
	if self.direction != lagApiDirectionUpload || self.end != lagApiPhaseEndTime || self.final {
		return false
	}
	var doneTime time.Time
	for streamIndex, open := range self.openStreams {
		count := &self.streamCounts[streamIndex]
		if !open || count.stopped || count.finished {
			continue
		}
		if count.doneTime.IsZero() {
			// it may still count, until lagSpreadMax past the end
			return false
		}
		if doneTime.Before(count.doneTime) {
			doneTime = count.doneTime
		}
	}
	if doneTime.IsZero() || now.After(doneTime) {
		self.finalizeWithLock()
		return true
	}
	if !self.doneTimerTime.Equal(doneTime) {
		if self.doneTimer != nil {
			self.doneTimer.Stop()
		}
		self.doneTimerTime = doneTime
		self.doneTimer = time.AfterFunc(doneTime.Add(time.Nanosecond).Sub(now), self.finalizeIfDone)
	}
	return false
}

// The done timer's: makes the window final if every open stream is done by
// now, and is set again if not (M6 R3, amendment 15).
func (self *lagApiPhase) finalizeIfDone() {
	finalized := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.doneTimerTime = time.Time{}
		return self.finalIfDoneWithLock(time.Now())
	}()
	self.changed(false, finalized)
}

// Whether reads may still count (M6, amendment 15), with the phase's lock
// held: while the phase runs, and once its window has run out, until the
// window is final.
func (self *lagApiPhase) countingWithLock() bool {
	return self.end == lagApiPhaseRunning || (self.end == lagApiPhaseEndTime && !self.final)
}

// The phase as the session sees it: its first byte (zero when none), when it
// ended (zero while running), and why. A phase that ended by its time ended
// at its window's end, though its window may not be final yet.
func (self *lagApiPhase) state() (firstByteTime time.Time, endedTime time.Time, end lagApiPhaseEnd) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.firstByteTime, self.endedTime, self.end
}

// The phase's end and settlement together: a late sampled failure may change
// a time end to a mismatch, so reconciliation must not read them separately.
func (self *lagApiPhase) settlementState() (firstByteTime time.Time, endedTime time.Time, end lagApiPhaseEnd, final bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.firstByteTime, self.endedTime, self.end, self.final && !self.pendingVerificationWithLock()
}

// Whether the phase's window is final (amendment 15): no more bytes count,
// and every sampled message with admitted bytes has settled its verification.
func (self *lagApiPhase) windowFinal() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.final && !self.pendingVerificationWithLock()
}

// Whether any admitted upload bytes still belong to a sampled message whose
// sample decision or comparison has not finished, with the phase's lock held.
func (self *lagApiPhase) pendingVerificationWithLock() bool {
	for streamIndex := range self.streamCounts {
		if self.streamCounts[streamIndex].unverified {
			return true
		}
	}
	return false
}

// Whether the upload started and its verification is settled without a
// mismatch or abandoned sampled bytes. Safe to read beside upload readers.
func (self *lagApiPhase) verified() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.firstByteTime.IsZero() && self.end != lagApiPhaseEndMismatch &&
		!self.verificationFailed && !self.pendingVerificationWithLock()
}

// Starts ownership of one sampled message. Only bytes admitted by receive
// keep settlement pending; a message drained after counting ends cannot do so.
func (self *lagApiPhase) beginSample(streamIndex int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.streamCounts[streamIndex].sampling = true
}

// Settles a sampled message's admitted bytes after its sample decision and
// comparison, or abandons them on a read error. Unlike an exact read rejected
// before counting, a failed sample invalidates its already admitted bytes
// even after the window's end or counting's final timer.
func (self *lagApiPhase) finishSample(streamIndex int, now time.Time, complete bool, mismatch bool) {
	ended, finalized := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		count := &self.streamCounts[streamIndex]
		count.sampling = false
		if !count.unverified {
			return false, false
		}
		count.unverified = false
		if !complete {
			self.verificationFailed = true
		}
		if mismatch {
			if self.end == lagApiPhaseRunning {
				if self.endTime.Before(now) {
					now = self.endTime
				}
				return self.finishWithLock(now, lagApiPhaseEndMismatch)
			}
			if self.end == lagApiPhaseEndTime {
				self.end = lagApiPhaseEndMismatch
				self.finalizeWithLock()
				return true, true
			}
		}
		return false, self.final && !self.pendingVerificationWithLock()
	}()
	self.changed(ended, finalized)
}

// The bytes per interval of the window, through its last counted interval: a
// download's sent series as the report signs it (M5, M8). Its streams write
// on past the window until the browser closes them, within the grace
// (amendment 15), so it holds those writes too, in the intervals past the
// window.
func (self *lagApiPhase) seriesValues() []uint64 {
	if series := self.series.Load(); series != nil {
		return series.values(0)
	}
	return []uint64{}
}

// The upload's series as the report signs it (M8, amendment 14): every
// interval from the window's first byte through the phase's end,
// ceil((end - first byte) / interval) of them, those that counted nothing as
// zeros, as the browser's figures count them, so a quiet end of the window
// stays in the signed p50. It never stops short of a counted byte, nor runs
// past the series. Before the phase ends, it runs through its last counted
// interval. Its values are final once the window is (amendment 15).
func (self *lagApiPhase) windowValues() []uint64 {
	firstByteTime, endedTime, _ := self.state()
	series := self.series.Load()
	if series == nil {
		return []uint64{}
	}
	intervalCount := 0
	if !endedTime.IsZero() {
		intervalCount = int((endedTime.Sub(firstByteTime) + series.interval - 1) / series.interval)
	}
	return series.values(intervalCount)
}

func (self *lagApiPhase) total() uint64 {
	if series := self.series.Load(); series != nil {
		return series.total()
	}
	return 0
}

// The bytes the phase's streams have moved so far, counted or not: what they
// took from the ticket's cap.
func (self *lagApiPhase) consumed() uint64 {
	return uint64(max(0, self.maxByteCount-self.capRemaining.Load()))
}

// Reserves up to byteCount bytes for a download write under the ticket's cap
// and the daily budget (M3, M5), returning what was granted. Zero ends the
// phase as capped.
func (self *lagApiPhase) reserve(now time.Time, byteCount int) int {
	granted := int64(0)
	for {
		capRemaining := self.capRemaining.Load()
		granted = min(int64(byteCount), capRemaining)
		if granted <= 0 {
			self.finish(lagApiPhaseEndCap)
			return 0
		}
		if self.capRemaining.CompareAndSwap(capRemaining, capRemaining-granted) {
			break
		}
	}
	budgeted := self.api.budget.take(now, granted)
	if budgeted < granted {
		self.capRemaining.Add(granted - budgeted)
	}
	if budgeted <= 0 {
		self.finish(lagApiPhaseEndCap)
		return 0
	}
	return int(budgeted)
}

// Returns the part of a reservation a write did not use.
func (self *lagApiPhase) unreserve(now time.Time, byteCount int) {
	if byteCount <= 0 {
		return
	}
	self.capRemaining.Add(int64(byteCount))
	self.api.budget.give(now, int64(byteCount))
}

// Counts bytes a download stream wrote into the interval of now (M8).
func (self *lagApiPhase) count(now time.Time, byteCount int) {
	if series := self.series.Load(); series != nil {
		series.add(now, byteCount)
	}
}

// Counts one read of upload stream k as it arrives (M6, M8). Its bytes count
// as having arrived evenly over a span that ends at its arrival, split across
// the intervals the span crosses (amendment 14): a stream that sends
// continuously delivers a read's bytes over the time since its previous read.
// Counted whole where it arrived, a read stepped its interval by one TLS
// record, 0.52 Mbps, which put a slow uplink's p50 off by up to that step.
// Amendment 15 settles the span at the window's edges and in bursts:
//   - R1, groups: reads that arrive within lagSpreadJoin of the first read of
//     their group are one group, and each spans from where the group's span
//     starts. Any other read starts a group, which spans from the stream's
//     previous read, at most lagSpreadMax back and never before the window's
//     first byte, as rule S has it. So a burst that ends a stall spreads, all
//     of it, over the stall, rather than its first read alone and the rest in
//     one interval, a spike that read in the p95.
//   - R2, a stream's first group: it is held, uncounted, until its second
//     group begins, and then counts as having arrived at the rate of the
//     second group's first read, back from its own last read, at most
//     lagSpreadMax (placeFirstWithLock). Only the part from the window's first
//     byte on counts: the streams start together, and when the first record
//     to arrive opens the window the others are mostly on their way, so
//     counted from the window's first byte their first records crowded into
//     its first interval. A first group that no second group places counts
//     by rule S (placeHeldWithLock).
//   - R3, the window's end: past it the phase has ended by its time, but a
//     stream's reads still count for the part of their spans before the end,
//     which the read that straddles the end carries. A stream is done
//     lagSpreadJoin after the first read of its first group that begins at
//     or after the end: a read joins that group only within lagSpreadJoin,
//     and every later group spans from its previous read, at or after the
//     end, so nothing the stream reads after could count. So one read past
//     the end will do, with no second to show it. A stream whose last
//     message, a short one, has arrived whole is done too (finishStream).
//     The window is final once every stream still open is done, or
//     lagSpreadMax past the end.
//
// Reads count under the phase's lock and only until the window is final, so
// nothing is added after: the total the browser is then told is the total the
// report holds. Every read the phase takes in draws on the ticket's cap and
// the daily budget for all its bytes as it arrives, whatever part of it
// counts: a first group's while it is held and its part before the window's
// first byte, and past the window's end a read's part after the end, all of
// a read that spans from the end on included. They are bytes the extender
// moved, as a download's writes in its grace are (M5). Inside the window,
// past either the phase ends as capped; past the end it has ended by its time
// already. Reads that arrive once their stream is done or the window is
// final are drained, not taken in.
func (self *lagApiPhase) receive(streamIndex int, arrivedTime time.Time, byteCount int) {
	if byteCount <= 0 {
		return
	}
	taken, ended, finalized := func() (bool, bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		series := self.series.Load()
		count := &self.streamCounts[streamIndex]
		if series == nil || !self.countingWithLock() {
			return false, false, false
		}
		ended := false
		finalized := false
		if self.end == lagApiPhaseRunning && !arrivedTime.Before(self.endTime) {
			ended, finalized = self.finishWithLock(arrivedTime, lagApiPhaseEndTime)
		}
		if arrivedTime.After(self.endTime.Add(lagSpreadMax)) {
			// no read this late reaches back before the end: the window is
			// final lagSpreadMax past it, whatever the timer's delay, so this
			// read arrived once it was, and is not taken in
			self.finalizeWithLock()
			return false, ended, true
		}
		if self.final {
			return false, ended, finalized
		}
		if self.streamDoneWithLock(streamIndex, arrivedTime) {
			// nothing it reads now can count; and by its time the last open
			// stream may be done
			return false, ended, self.finalIfDoneWithLock(arrivedTime) || finalized
		}
		if count.sampling {
			count.unverified = true
		}
		if count.previousTime.IsZero() || arrivedTime.After(count.groupTime.Add(lagSpreadJoin)) {
			// a new group, which spans from the stream's previous read
			spanStart := arrivedTime.Add(-lagSpreadMax)
			if spanStart.Before(count.previousTime) {
				spanStart = count.previousTime
			}
			if spanStart.Before(self.firstByteTime) {
				spanStart = self.firstByteTime
			}
			if 0 < count.heldByteCount {
				// the stream's second group: it places the first
				self.placeFirstWithLock(streamIndex, arrivedTime, byteCount)
			}
			count.groupTime = arrivedTime
			count.groupSpanStart = spanStart
			if !arrivedTime.Before(self.endTime) && count.doneTime.IsZero() {
				// its first group to begin at or after the end: only a read
				// that joins it can still count before the end
				count.doneTime = arrivedTime.Add(lagSpreadJoin)
			}
		}
		if count.previousTime.IsZero() || 0 < count.heldByteCount {
			// the stream's first group, held until its second begins
			count.heldByteCount += byteCount
		} else {
			// its part before the end: none, when it spans from the end on,
			// though its bytes moved
			series.addSpread(count.groupSpanStart, arrivedTime, byteCount, self.endTime)
			if count.groupSpanStart.Before(self.endTime) {
				self.countedWithLock(arrivedTime)
			}
		}
		count.previousTime = arrivedTime
		if self.end == lagApiPhaseEndTime {
			finalized = self.finalIfDoneWithLock(arrivedTime) || finalized
		}
		return true, ended, finalized
	}()
	self.changed(ended, finalized)
	if !taken {
		return
	}
	if self.capRemaining.Add(-int64(byteCount)) <= 0 {
		self.finish(lagApiPhaseEndCap)
	}
	if self.api.budget.take(arrivedTime, int64(byteCount)) < int64(byteCount) {
		self.finish(lagApiPhaseEndCap)
	}
}

// Counts stream k's held first group once its second group begins with a read
// of byteCount bytes at arrivedTime, with the phase's lock held (M6 R2,
// amendment 15): as having arrived evenly at that read's rate, over the time
// its bytes would take at that rate back from the group's last read, at most
// lagSpreadMax. Only the part from the window's first byte on counts; the rest
// arrived before the window opened and is not counted at all. Most of the read
// that opened the window is such a part. With no time between the two groups
// there is no rate, and rule S places it.
func (self *lagApiPhase) placeFirstWithLock(streamIndex int, arrivedTime time.Time, byteCount int) {
	count := &self.streamCounts[streamIndex]
	lastTime := count.previousTime
	gap := arrivedTime.Sub(lastTime)
	if gap <= 0 {
		self.placeHeldWithLock(streamIndex)
		return
	}
	spanStart := lastTime.Add(-lagSpreadMax)
	if span := float64(count.heldByteCount) * float64(gap) / float64(byteCount); span < float64(lagSpreadMax) {
		spanStart = lastTime.Add(-time.Duration(span))
	}
	self.series.Load().addSpread(spanStart, lastTime, count.heldByteCount, self.endTime)
	count.heldByteCount = 0
	self.countedWithLock(lastTime)
}

// Counts stream k's first group, if it is still held, by rule S, with the
// phase's lock held (M6 R2, amendment 15): over the time back from its last
// read, at most lagSpreadMax and never before the window's first byte. For a
// first group no second group placed: its stream ended, or its window became
// final, first.
func (self *lagApiPhase) placeHeldWithLock(streamIndex int) {
	count := &self.streamCounts[streamIndex]
	if count.heldByteCount == 0 {
		return
	}
	lastTime := count.previousTime
	spanStart := lastTime.Add(-lagSpreadMax)
	if spanStart.Before(self.firstByteTime) {
		spanStart = self.firstByteTime
	}
	self.series.Load().addSpread(spanStart, lastTime, count.heldByteCount, self.endTime)
	count.heldByteCount = 0
	self.countedWithLock(lastTime)
}

// Keeps the latest arrival of a counted read (M4), with the phase's lock held.
func (self *lagApiPhase) countedWithLock(arrivedTime time.Time) {
	if self.lastCountedTime.Before(arrivedTime) {
		self.lastCountedTime = arrivedTime
	}
}

// Ends the phase as a mismatch at now (M6): the stream's bytes were not the
// expected keystream. Once the window has run out, the phase ended by its time
// (amendment 15), and a mismatch then ends only its stream's counting. What
// counted before stays, and the load's outcome is its window's: every read
// that counted was checked, under exact verification, before it counted.
func (self *lagApiPhase) mismatch(streamIndex int, now time.Time) {
	ended, finalized := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		ended := false
		finalized := false
		if self.end == lagApiPhaseRunning && !self.endTime.IsZero() && !now.Before(self.endTime) {
			ended, finalized = self.finishWithLock(now, lagApiPhaseEndTime)
		}
		if self.end == lagApiPhaseRunning {
			return self.finishWithLock(now, lagApiPhaseEndMismatch)
		}
		if !self.countingWithLock() {
			return ended, finalized
		}
		self.streamCounts[streamIndex].stopped = true
		return ended, self.finalIfDoneWithLock(now) || finalized
	}()
	self.changed(ended, finalized)
}

// The upload's bytes counted so far and when the latest counted read arrived,
// zero before the first, read together (M4).
func (self *lagApiPhase) progress() (uint64, time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.total(), self.lastCountedTime
}

// The ms of a progress report that the browser takes as the upload's window
// being over (M4, S5): the window's length less half an interval, as it
// compares it, `ms ≥ uploadMs − intervalMs/2`.
func lagApiWindowOverMs(duration time.Duration, interval time.Duration) float64 {
	return float64(duration.Milliseconds()) - float64(interval.Milliseconds())/2
}

// An upload progress report (M4) and whether it goes out, read at one moment
// under the phase's lock, so its ms and the phase's state cannot disagree:
// the bytes counted so far, the ms into the window, and at, when the latest
// counted read arrived, in ms to the microsecond, held to ms. last is the
// report that says the window is over, sent once it is final, with the final
// total: its ms is the window's whole length, or the phase's for one that
// ended sooner. Any other report goes out only while the phase runs, and only
// with ms short of the length less half an interval, which the browser takes
// as the window's end: it would close its streams and say done before the
// reads that straddle the end arrive, so no report before the window is
// final may read as over (amendment 15). One that would is not sent. Neither
// ms nor at goes back from one report to the next.
func (self *lagApiPhase) progressReport(last bool) (uint64, int64, float64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := time.Now()
	if self.firstByteTime.IsZero() {
		return 0, 0, 0, false
	}
	ms := int64(0)
	if last {
		if !self.final || self.pendingVerificationWithLock() {
			return 0, 0, 0, false
		}
		ms = max(0, min(self.endedTime.Sub(self.firstByteTime), self.duration)).Milliseconds()
	} else {
		if self.end != lagApiPhaseRunning {
			return 0, 0, 0, false
		}
		ms = max(0, now.Sub(self.firstByteTime)).Milliseconds()
		if lagApiWindowOverMs(self.duration, self.interval) <= float64(ms) {
			return 0, 0, 0, false
		}
	}
	at := 0.0
	if !self.lastCountedTime.IsZero() {
		at = max(0, min(float64(ms), float64(self.lastCountedTime.Sub(self.firstByteTime).Microseconds())/1000))
	}
	return self.total(), ms, at, true
}

// Bytes per interval from a window's first byte (M8). Sized once and bounded
// by lagApiMaxIntervalCount; bytes outside it are not counted.
type lagApiSeries struct {
	startTime time.Time
	interval  time.Duration
	counts    []atomic.Uint64
}

func newLagApiSeries(startTime time.Time, interval time.Duration, duration time.Duration) *lagApiSeries {
	if interval <= 0 {
		interval = lagApiInterval
	}
	intervalCount := max(1, min(lagApiMaxIntervalCount, int((duration+interval-1)/interval)))
	return &lagApiSeries{
		startTime: startTime,
		interval:  interval,
		counts:    make([]atomic.Uint64, intervalCount),
	}
}

// The interval of the window that now falls in, from 0 at the first byte.
func (self *lagApiSeries) index(now time.Time) int {
	return int(max(0, now.Sub(self.startTime)) / self.interval)
}

func (self *lagApiSeries) add(now time.Time, byteCount int) {
	if byteCount <= 0 {
		return
	}
	index := self.index(now)
	if len(self.counts) <= index {
		return
	}
	self.counts[index].Add(uint64(byteCount))
}

// Adds bytes that arrived evenly over [startTime, endTime] (amendment 14), of
// which only the part from the series' start to untilTime counts (amendment
// 15): a stream's first group can reach back before the window's first byte,
// and a read that straddles the window's end carries bytes from after it. The
// span is cut at the interval boundaries inside it and at those two bounds,
// and each interval takes its piece's share of the span's length. The pieces
// are whole bytes: each cut rounds the share through it, so a piece is the
// difference of two rounded shares, and the pieces of a span wholly inside the
// bounds sum to byteCount. A span of no length adds them all at endTime, if
// that is inside the bounds.
func (self *lagApiSeries) addSpread(startTime time.Time, endTime time.Time, byteCount int, untilTime time.Time) {
	if byteCount <= 0 {
		return
	}
	spanStart := startTime.Sub(self.startTime)
	spanEnd := endTime.Sub(self.startTime)
	until := untilTime.Sub(self.startTime)
	if spanEnd <= spanStart {
		if 0 <= spanEnd && spanEnd < until {
			self.add(endTime, byteCount)
		}
		return
	}
	// the span's bytes through x, rounded
	through := func(x time.Duration) int {
		if spanEnd <= x {
			return byteCount
		}
		return int(math.Round(float64(byteCount) * float64(x-spanStart) / float64(spanEnd-spanStart)))
	}
	from := max(0, spanStart)
	to := min(spanEnd, until)
	for index := int(from / self.interval); index < len(self.counts); index += 1 {
		cutStart := max(from, time.Duration(index)*self.interval)
		cutEnd := min(to, time.Duration(index+1)*self.interval)
		if cutEnd <= cutStart {
			break
		}
		if piece := through(cutEnd) - through(cutStart); 0 < piece {
			self.counts[index].Add(uint64(piece))
		}
	}
}

// The counts, through the last interval that counted anything and at least
// the first intervalCount, empty ones as zeros, and never past the series.
func (self *lagApiSeries) values(intervalCount int) []uint64 {
	values := make([]uint64, len(self.counts))
	lastIndex := min(intervalCount, len(self.counts)) - 1
	for i := range self.counts {
		values[i] = self.counts[i].Load()
		if values[i] != 0 {
			lastIndex = max(lastIndex, i)
		}
	}
	return values[:max(0, lastIndex+1)]
}

func (self *lagApiSeries) total() uint64 {
	total := uint64(0)
	for i := range self.counts {
		total += self.counts[i].Load()
	}
	return total
}

// The daily byte budget of loads across both directions (M3), per UTC day. It
// is kept in memory, so a restart starts the day's count over. A limit <= 0 is
// no budget.
type lagApiBudget struct {
	limit int64

	stateLock sync.Mutex
	day       int64
	used      int64
}

func newLagApiBudget(limit int64) *lagApiBudget {
	return &lagApiBudget{
		limit: limit,
	}
}

func lagApiUtcDay(now time.Time) int64 {
	return now.UTC().Unix() / (24 * 60 * 60)
}

// Moves the count to the day of now.
func (self *lagApiBudget) rollWithLock(now time.Time) {
	if day := lagApiUtcDay(now); day != self.day {
		self.day = day
		self.used = 0
	}
}

// Grants up to byteCount bytes of today's budget.
func (self *lagApiBudget) take(now time.Time, byteCount int64) int64 {
	if byteCount <= 0 {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.rollWithLock(now)
	granted := byteCount
	if 0 < self.limit {
		granted = max(0, min(byteCount, self.limit-self.used))
	}
	self.used += granted
	return granted
}

// Returns bytes taken and not used, when the day they were taken on is still
// today.
func (self *lagApiBudget) give(now time.Time, byteCount int64) {
	if byteCount <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if lagApiUtcDay(now) != self.day {
		return
	}
	self.used = max(0, self.used-byteCount)
}

// Whether nothing is left of today's budget.
func (self *lagApiBudget) exhausted(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.rollWithLock(now)
	return 0 < self.limit && self.limit <= self.used
}

// When today's budget is renewed: the next UTC midnight.
func (self *lagApiBudget) renewTime(now time.Time) time.Time {
	return time.Unix((lagApiUtcDay(now)+1)*24*60*60, 0)
}

// The session and stream a stream request names (M5, M6), or the status that
// refuses it: 400 for a malformed query, 404 for a session that is not open.
func (self *extenderLagApi) streamSession(r *http.Request) (*lagApiSession, int, int) {
	query := r.URL.Query()
	sessionId, ok := parseLagApiSessionId(query.Get("session"))
	if !ok {
		return nil, 0, http.StatusBadRequest
	}
	streamIndex, err := strconv.Atoi(query.Get("stream"))
	if err != nil || streamIndex < 0 || lagApiMaxStreamCount <= streamIndex {
		return nil, 0, http.StatusBadRequest
	}
	session := self.session(sessionId)
	if session == nil {
		return nil, 0, http.StatusNotFound
	}
	return session, streamIndex, 0
}

// GET /lag/v1/down?session=<id>&stream=<k> (M5): 200 with keystream under a
// fresh random key, chunked with no content length and no content encoding,
// during the session's download phase only: through downloadMs after the
// phase's first byte, and past it until the browser closes the stream, within
// the grace (amendment 15), or until a cap runs out. Outside the phase, or for
// a stream already open or past maxStreams, 409.
func (self *extenderLagApi) serveDown(w http.ResponseWriter, r *http.Request, originAllowed bool) {
	if !originAllowed {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	session, streamIndex, status := self.streamSession(r)
	if session == nil {
		w.WriteHeader(status)
		return
	}
	phase := session.waitPhase(r.Context(), lagApiDirectionDownload)
	if phase == nil || !phase.openStream(streamIndex) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	defer phase.closeStream(streamIndex)

	keystream, err := newLagApiDownloadKeystream()
	if err != nil {
		self.server.reportError("lag download", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	self.writeDownload(r.Context(), w, phase, keystream)
}

// Writes one download stream from one buffer (M5, M7): through the window, and
// past it until the browser closes the stream -- the request's context is
// then done, or a write fails -- within the grace (amendment 15). Each write
// draws on the ticket's cap and the daily budget, past the window as in it. A
// write still under way when the grace runs out is cut, which ends the
// connection.
func (self *extenderLagApi) writeDownload(
	ctx context.Context,
	w http.ResponseWriter,
	phase *lagApiPhase,
	keystream cipher.Stream,
) {
	controller := http.NewResponseController(w)
	// the request is read; a stream outlives the server's read deadline,
	// which would otherwise end it (M2)
	if err := controller.SetReadDeadline(time.Time{}); err != nil {
		self.server.reportError("lag download", err)
		return
	}
	buffer := make([]byte, lagApiBufferByteCount)
	deadlineSet := false
	for ctx.Err() == nil {
		now := time.Now()
		endTime, ok := phase.writable(now)
		if !ok {
			return
		}
		if !deadlineSet {
			if err := controller.SetWriteDeadline(endTime.Add(lagApiPhaseGrace)); err != nil {
				self.server.reportError("lag download", err)
				return
			}
			deadlineSet = true
		}
		byteCount := phase.reserve(now, len(buffer))
		if byteCount <= 0 {
			return
		}
		keystream.XORKeyStream(buffer[:byteCount], lagApiZeroBytes[:byteCount])
		writtenByteCount, err := w.Write(buffer[:byteCount])
		writtenTime := time.Now()
		phase.count(writtenTime, writtenByteCount)
		phase.unreserve(writtenTime, byteCount-writtenByteCount)
		if err != nil {
			return
		}
	}
}

// GET /lag/v1/up?session=<id>&stream=<k> (M6): a websocket of binary messages
// carrying keystream under the session's upload key, verified as they arrive,
// during the session's upload phase only. A stream the session cannot take is
// refused before the upgrade, with the statuses a download is refused with:
// the browser's client retries an upgrade that failed, since a stream can
// overtake the message that opens its phase, but takes an upgraded socket as
// a stream that opened (S5).
func (self *extenderLagApi) serveUp(w http.ResponseWriter, r *http.Request, originAllowed bool) {
	if !originAllowed {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	session, streamIndex, status := self.streamSession(r)
	if session == nil {
		w.WriteHeader(status)
		return
	}
	phase := session.waitPhase(r.Context(), lagApiDirectionUpload)
	if phase == nil || !phase.openStream(streamIndex) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	defer phase.closeStream(streamIndex)
	conn, err := self.upgrade(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	self.readUpload(conn, session.uploadKey, phase, streamIndex)
}

// Reads and verifies one upload stream (M6, M7), message by message (see
// lagApiUploadStream). When the phase ends the stream is closed -- 1000, or
// 4400 for a mismatch -- and read on until the browser's own close: what the
// browser sent before the close reached it still arrives, and past the
// window's end its reads count for the part of their spans before the end
// until the window is final (amendment 15). What arrives after is drained.
func (self *extenderLagApi) readUpload(
	conn *websocket.Conn,
	uploadKey [16]byte,
	phase *lagApiPhase,
	streamIndex int,
) {
	conn.SetReadLimit(lagApiBufferByteCount)
	stream, err := newLagApiUploadStream(phase, uploadKey, streamIndex, self.exactVerification, self.sampleMessage)
	if err != nil {
		self.server.reportError("lag upload", err)
		lagApiCloseWebSocket(conn, lagApiCloseCodeInternal, "internal")
		return
	}

	// whatever ends the phase closes the stream; the reader below then drains
	// to the browser's close, or gives up after the close wait. Joined before
	// the stream is done.
	closeSent := make(chan struct{})
	stopClose := context.AfterFunc(phase.ctx, func() {
		defer close(closeSent)
		_, _, end := phase.state()
		if end == lagApiPhaseEndMismatch {
			lagApiSendClose(conn, lagApiCloseCodeMismatch, "mismatch")
		} else {
			lagApiSendClose(conn, lagApiCloseCodeNormal, "")
		}
	})
	defer func() {
		if !stopClose() {
			<-closeSent
		}
	}()

	for {
		messageType, reader, err := conn.NextReader()
		if err != nil {
			// the browser's close, the drain's deadline, or a broken connection
			return
		}
		if err := stream.readMessage(messageType, reader, time.Now); err != nil {
			return
		}
	}
}

// One upload stream as the extender reads it (M6, M7): its keystream, its
// offset, and one data buffer and one keystream buffer for its life. Messages
// are 65536 bytes, the last possibly shorter, contiguous from offset 0. The
// first message opens the phase's window. Each read counts as it arrives, by
// the phase's receive (amendment 14): with AES instructions once its bytes have
// been compared with the running keystream at their offset; without, at once,
// and a random 1 in 16 messages, chosen after each has arrived, is compared
// whole. A mismatch, a text message or a message after a short one ends the
// phase as a mismatch, which ends the load; what was counted before it stays
// counted, but the load is not complete, so no speed is derived from it (A5).
// Past the window's end, the stream is read on while its reads may still
// count (amendment 15). An exact mismatch then ends only its count; a failed
// sample still invalidates the bytes it already admitted, and settlement
// waits for that sample decision and comparison. A short message finishes once
// it has arrived whole and been checked, under exact verification read by read
// and under sampled once the sample was chosen and compared or not: nothing
// valid can follow it, so the stream is done (amendment 15).
type lagApiUploadStream struct {
	phase         *lagApiPhase
	streamIndex   int
	block         cipher.Block
	exact         bool
	sampleMessage func() bool

	data      []byte
	keystream []byte
	// the keystream from the next byte on, with exact verification
	running cipher.Stream
	offset  uint64
	// the last message was short, so no other may follow it
	short bool
}

func newLagApiUploadStream(
	phase *lagApiPhase,
	uploadKey [16]byte,
	streamIndex int,
	exact bool,
	sampleMessage func() bool,
) (*lagApiUploadStream, error) {
	block, err := aes.NewCipher(uploadKey[:])
	if err != nil {
		return nil, err
	}
	stream := &lagApiUploadStream{
		phase:         phase,
		streamIndex:   streamIndex,
		block:         block,
		exact:         exact,
		sampleMessage: sampleMessage,
		data:          make([]byte, lagApiBufferByteCount),
		keystream:     make([]byte, lagApiBufferByteCount),
	}
	if exact {
		counterBlock := lagApiCounterBlock(uint64(streamIndex), 0)
		stream.running = cipher.NewCTR(block, counterBlock[:])
	}
	return stream, nil
}

// Reads one message from reader, verifying and counting it read by read as
// above, with each read's arrival from now: a read returns what the connection
// has delivered, so its time is its arrival, to within a TLS record. A message
// after a short one is a mismatch, whenever it comes. Otherwise a message that
// starts once none of its stream's reads can count -- the phase ended early,
// or past the window's end the stream is done or the window final -- is left
// unread, to be drained, not counted, as is the rest of one that failed its
// check. A full buffer is the whole message, since the read limit refuses a
// longer one before its bytes arrive. A short message, once it has arrived
// whole and been checked, finishes its stream (finishStream). An error is the
// connection's.
func (self *lagApiUploadStream) readMessage(messageType int, reader io.Reader, now func() time.Time) error {
	phase := self.phase
	startTime := now()
	if self.short {
		// only the last message may be shorter
		phase.mismatch(self.streamIndex, startTime)
		return nil
	}
	if !phase.takes(self.streamIndex, startTime) {
		return nil
	}
	if messageType != websocket.BinaryMessage {
		phase.mismatch(self.streamIndex, startTime)
		return nil
	}
	sampleComplete := false
	sampleMismatch := false
	if !self.exact {
		phase.beginSample(self.streamIndex)
		defer func() {
			phase.finishSample(self.streamIndex, now(), sampleComplete, sampleMismatch)
		}()
	}
	byteCount := 0
	lastTime := startTime
	for byteCount < len(self.data) {
		n, err := reader.Read(self.data[byteCount:])
		if 0 < n {
			arrivedTime := now()
			if self.exact {
				keystream := self.keystream[byteCount : byteCount+n]
				self.running.XORKeyStream(keystream, lagApiZeroBytes[:n])
				if !bytes.Equal(self.data[byteCount:byteCount+n], keystream) {
					phase.mismatch(self.streamIndex, arrivedTime)
					return nil
				}
			}
			phase.receive(self.streamIndex, arrivedTime, n)
			byteCount += n
			lastTime = arrivedTime
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	self.short = byteCount < len(self.data)
	if !self.exact && self.sampleMessage() {
		counterBlock := lagApiCounterBlock(uint64(self.streamIndex), self.offset)
		cipher.NewCTR(self.block, counterBlock[:]).XORKeyStream(self.keystream[:byteCount], lagApiZeroBytes[:byteCount])
		if !bytes.Equal(self.data[:byteCount], self.keystream[:byteCount]) {
			sampleMismatch = true
			return nil
		}
	}
	sampleComplete = true
	self.offset += uint64(byteCount)
	if self.short {
		// the stream's last message: it is done (amendment 15)
		phase.finishStream(self.streamIndex, lastTime)
	}
	return nil
}

// Sends a close frame and bounds how long the reader may wait for the peer's
// own close after it (RFC 6455 7.1.1). The reader of the connection drains
// until then; a reset in its place could lose the code at the peer. Safe to
// call beside the reader and the writer, and more than once.
func lagApiSendClose(conn *websocket.Conn, code int, text string) {
	deadline := time.Now().Add(lagApiCloseWait)
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), deadline)
	conn.NetConn().SetReadDeadline(deadline)
}

// Closes a websocket nothing else reads: the close frame, then a drain to the
// peer's close within the close wait, then the connection.
func lagApiCloseWebSocket(conn *websocket.Conn, code int, text string) {
	lagApiSendClose(conn, code, text)
	for {
		if _, _, err := conn.NextReader(); err != nil {
			break
		}
	}
	conn.Close()
}
