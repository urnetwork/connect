package extender

// The measurement streams (lag.dev/PLAN.md M5 to M7).
//
// A session's load is a download phase, then an upload phase. Each phase's
// window opens at its first byte, so connection setup does not count, and lasts
// the ticket's downloadMs or uploadMs; the browser opens maxStreams streams
// into it. A download stream is AES-128-CTR keystream under a fresh random key,
// written from one 64 KiB buffer until the window or a cap runs out (M5). An
// upload stream is a websocket of binary messages carrying AES-128-CTR
// keystream under the session's upload key, which the extender regenerates and
// compares message by message -- or, on hardware without AES instructions, on
// a random 1 in 16 messages chosen after each arrives (M6). Each stream holds
// one 64 KiB data buffer and one 64 KiB keystream buffer; nothing grows with
// the stream, and nothing is written to disk (M7).

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"io"
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

// How long past a download window a write already under way may take to
// finish, counted where it finishes. Nothing starts in it.
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
// whether an upload is verified message by message or sampled (M6). It
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
type lagApiPhase struct {
	api            *extenderLagApi
	direction      lagApiDirection
	duration       time.Duration
	interval       time.Duration
	maxStreamCount int
	// wakes the session, at the first byte and at the end
	notify func()

	// done when the phase ends; a stream then stops
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
	end            lagApiPhaseEnd
	endedTime      time.Time
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
	}
	phase.capRemaining.Store(maxByteCount)
	phase.stateLock.Lock()
	defer phase.stateLock.Unlock()
	phase.firstByteTimer = time.AfterFunc(firstByteWait, func() {
		phase.finish(lagApiPhaseEndTimeout)
	})
	return phase
}

// The window of the phase at now, opening it when this is the first byte (M5,
// M6), and whether bytes may still move in it. Whoever first sees the window
// has run out ends the phase, so its end does not wait on a timer.
func (self *lagApiPhase) window(now time.Time) (time.Time, bool) {
	opened := false
	over := false
	endTime, ok := func() (time.Time, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.end != lagApiPhaseRunning {
			return time.Time{}, false
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
		over = !now.Before(self.endTime)
		return self.endTime, !over
	}()
	if opened {
		self.notify()
	}
	if over {
		self.finish(lagApiPhaseEndTime)
	}
	return endTime, ok
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

func (self *lagApiPhase) closeStream(streamIndex int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.openStreams[streamIndex] = false
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
func (self *lagApiPhase) finish(end lagApiPhaseEnd) {
	first := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.end != lagApiPhaseRunning {
			return false
		}
		now := time.Now()
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
		return true
	}()
	if !first {
		return
	}
	self.cancel()
	self.notify()
}

// The phase as the session sees it: its first byte (zero when none), when it
// ended (zero while running), and why.
func (self *lagApiPhase) state() (firstByteTime time.Time, endedTime time.Time, end lagApiPhaseEnd) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.firstByteTime, self.endedTime, self.end
}

// The bytes per interval of the window, through its last counted interval, and
// their sum.
func (self *lagApiPhase) seriesValues() []uint64 {
	if series := self.series.Load(); series != nil {
		return series.values()
	}
	return []uint64{}
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

// Takes bytes an upload message brought from the ticket's cap and the daily
// budget, and counts them into their interval when they arrived inside the
// window (M6). They are counted only while the phase runs, under its lock, so
// nothing is added once it has ended: the total the browser is then told is
// the total the report holds. The bytes are already here, so they draw on the
// cap and the budget either way; past either the phase ends as capped.
func (self *lagApiPhase) receive(now time.Time, byteCount int) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.end == lagApiPhaseRunning && now.Before(self.endTime) {
			if series := self.series.Load(); series != nil {
				series.add(now, byteCount)
			}
		}
	}()
	if self.capRemaining.Add(-int64(byteCount)) <= 0 {
		self.finish(lagApiPhaseEndCap)
	}
	if self.api.budget.take(now, int64(byteCount)) < int64(byteCount) {
		self.finish(lagApiPhaseEndCap)
	}
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

func (self *lagApiSeries) add(now time.Time, byteCount int) {
	if byteCount <= 0 {
		return
	}
	elapsed := max(0, now.Sub(self.startTime))
	index := int(elapsed / self.interval)
	if len(self.counts) <= index {
		return
	}
	self.counts[index].Add(uint64(byteCount))
}

// The counts, through the last interval that counted anything.
func (self *lagApiSeries) values() []uint64 {
	values := make([]uint64, len(self.counts))
	lastIndex := -1
	for i := range self.counts {
		values[i] = self.counts[i].Load()
		if values[i] != 0 {
			lastIndex = i
		}
	}
	return values[:lastIndex+1]
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
// during the session's download phase only, until downloadMs after the phase's
// first byte or a cap runs out. Outside the phase, or for a stream already open
// or past maxStreams, 409.
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

// Writes one download stream from one buffer (M5, M7). A write still under way
// when the window runs out may finish within the grace; one past it is cut,
// which ends the connection.
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
		endTime, ok := phase.window(now)
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

// Reads and verifies one upload stream (M6, M7). Messages are 65536 bytes, the
// last possibly shorter, contiguous from offset 0. With AES instructions every
// message is compared with the keystream regenerated at its offset; without, a
// random 1 in 16, chosen after it arrives. A mismatch, a text message or a
// message after a short one ends the phase as a mismatch, which ends the load.
// The first message opens the window, and only bytes inside it are counted.
// When the phase ends the stream is closed -- 1000, or 4400 for a mismatch --
// and drained until the browser's own close.
func (self *extenderLagApi) readUpload(
	conn *websocket.Conn,
	uploadKey [16]byte,
	phase *lagApiPhase,
	streamIndex int,
) {
	conn.SetReadLimit(lagApiBufferByteCount)
	block, err := aes.NewCipher(uploadKey[:])
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

	// one data buffer and one keystream buffer for the stream's life (M7)
	data := make([]byte, lagApiBufferByteCount)
	keystream := make([]byte, lagApiBufferByteCount)
	exact := self.exactVerification
	var running cipher.Stream
	if exact {
		counterBlock := lagApiCounterBlock(uint64(streamIndex), 0)
		running = cipher.NewCTR(block, counterBlock[:])
	}
	offset := uint64(0)
	short := false
	for {
		messageType, reader, err := conn.NextReader()
		if err != nil {
			// the browser's close, the drain's deadline, or a broken connection
			return
		}
		if _, ok := phase.window(time.Now()); !ok {
			// past the window: drained, not counted
			continue
		}
		if messageType != websocket.BinaryMessage || short {
			phase.finish(lagApiPhaseEndMismatch)
			continue
		}
		byteCount, err := lagApiReadMessage(reader, data)
		if err != nil {
			return
		}
		// only the last message may be shorter
		short = byteCount < len(data)
		receivedTime := time.Now()

		if exact || self.sampleMessage() {
			stream := running
			if !exact {
				counterBlock := lagApiCounterBlock(uint64(streamIndex), offset)
				stream = cipher.NewCTR(block, counterBlock[:])
			}
			stream.XORKeyStream(keystream[:byteCount], lagApiZeroBytes[:byteCount])
			if !bytes.Equal(data[:byteCount], keystream[:byteCount]) {
				phase.finish(lagApiPhaseEndMismatch)
				continue
			}
		}
		offset += uint64(byteCount)
		phase.receive(receivedTime, byteCount)
	}
}

// Reads one message into buffer (M6). A full buffer is the whole message,
// since the read limit refuses a longer one before its bytes arrive.
func lagApiReadMessage(reader io.Reader, buffer []byte) (int, error) {
	byteCount := 0
	for byteCount < len(buffer) {
		n, err := reader.Read(buffer[byteCount:])
		byteCount += n
		if err == io.EOF {
			return byteCount, nil
		}
		if err != nil {
			return byteCount, err
		}
	}
	return byteCount, nil
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
