package extender

// Tests of the measurement streams (lag.dev/PLAN.md M5 to M7, T1): the
// upload's keystream verification, exact and sampled, its counting as each
// read arrives and its accuracy (amendment 14), the stream refusals, the
// window from the first byte, the daily budget, and flat memory over a 1 GiB
// stream each way.

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect/protocol"
)

// A session admitted with no control socket, whose load the test drives.
func (self *lagApiFixture) directSession(maxStreamCount uint32) *lagApiSession {
	self.t.Helper()
	body := &protocol.LagTicketBody{
		TicketId:     testLagRandomBytes(16),
		TrackId:      testLagRandomBytes(16),
		ExpireTimeMs: uint64(time.Now().Add(2 * time.Minute).UnixMilli()),
		MaxStreams:   maxStreamCount,
	}
	session, err := newLagApiSession(self.api, nil, body)
	if err != nil {
		self.t.Fatal(err)
	}
	session.serverName = testLagName
	if code := self.api.admitSession(session); code != "" {
		self.t.Fatalf("admit %s", code)
	}
	self.t.Cleanup(func() {
		session.close()
		self.api.removeSession(session)
	})
	return session
}

// Opens a phase of the session's load as the loop would, with a duration and
// a cap of the test's choosing. An upload's load gets a download that is over.
func (self *lagApiFixture) directPhase(
	session *lagApiSession,
	direction lagApiDirection,
	duration time.Duration,
	maxByteCount int64,
) *lagApiPhase {
	newPhase := func(direction lagApiDirection) *lagApiPhase {
		return newLagApiPhase(
			self.api,
			session.ctx,
			direction,
			duration,
			lagApiInterval,
			session.maxStreamCount,
			maxByteCount,
			10*time.Second,
			func() {},
		)
	}
	phase := newPhase(direction)
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	if session.load == nil {
		session.load = &lagApiLoad{requestTime: time.Now()}
	}
	if direction == lagApiDirectionDownload {
		session.load.download = phase
	} else {
		if session.load.download == nil {
			download := newPhase(lagApiDirectionDownload)
			download.finish(lagApiPhaseEndTime)
			session.load.download = download
		}
		session.load.upload = phase
	}
	session.publishWithLock()
	return phase
}

// An upload stream as the browser opens it, with a reader for the extender's
// close.
type lagTestUpload struct {
	t      *testing.T
	conn   *websocket.Conn
	closed chan int
}

func (self *lagApiFixture) openUpload(session *lagApiSession, streamIndex int) *lagTestUpload {
	self.t.Helper()
	// one frame per message, as a browser sends it
	return self.openUploadWithWriteBuffer(session, streamIndex, lagApiBufferByteCount+16)
}

// An upload stream whose messages go out in frames of at most
// writeBufferSize, so one message can arrive in parts over time.
func (self *lagApiFixture) openUploadWithWriteBuffer(session *lagApiSession, streamIndex int, writeBufferSize int) *lagTestUpload {
	self.t.Helper()
	dialer := &websocket.Dialer{
		NetDialContext:   self.dialContext,
		TLSClientConfig:  self.httpClient().Transport.(*http.Transport).TLSClientConfig.Clone(),
		HandshakeTimeout: 5 * time.Second,
		WriteBufferSize:  writeBufferSize,
	}
	dialer.TLSClientConfig.ServerName = testLagName
	header := http.Header{}
	header.Set("Origin", testLagOrigin)
	conn, _, err := dialer.Dial(fmt.Sprintf("wss://%s/lag/v1/up?session=%s&stream=%d", testLagName, session.id, streamIndex), header)
	if err != nil {
		self.t.Fatal(err)
	}
	upload := &lagTestUpload{
		t:      self.t,
		conn:   conn,
		closed: make(chan int, 1),
	}
	go func() {
		for {
			if _, _, err := conn.NextReader(); err != nil {
				closeError := &websocket.CloseError{}
				if errors.As(err, &closeError) {
					upload.closed <- closeError.Code
				} else {
					upload.closed <- -1
				}
				return
			}
		}
	}()
	self.t.Cleanup(func() {
		conn.Close()
	})
	return upload
}

func (self *lagTestUpload) send(messageType int, message []byte) error {
	self.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return self.conn.WriteMessage(messageType, message)
}

func (self *lagTestUpload) waitClosed(timeout time.Duration) int {
	self.t.Helper()
	select {
	case code := <-self.closed:
		return code
	case <-time.After(timeout):
		self.t.Fatal("the upload was not closed")
		return 0
	}
}

func (self *lagTestUpload) close() {
	self.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	self.waitClosed(5 * time.Second)
}

// The keystream messages of one upload stream from offset 0 (M6).
func testLagUploadMessages(t *testing.T, uploadKey [16]byte, streamIndex int, byteCounts ...int) [][]byte {
	t.Helper()
	stream := testLagUploadStream(t, uploadKey[:], uint64(streamIndex))
	messages := [][]byte{}
	for _, byteCount := range byteCounts {
		message := make([]byte, byteCount)
		stream.XORKeyStream(message, message)
		messages = append(messages, message)
	}
	return messages
}

func testLagRepeat(byteCount int, count int, last ...int) []int {
	byteCounts := []int{}
	for i := 0; i < count; i += 1 {
		byteCounts = append(byteCounts, byteCount)
	}
	return append(byteCounts, last...)
}

// M6: the counter block of stream k at offset o is k, then o/16, each a
// big-endian uint64 -- the keystream is AES of exactly those blocks.
func TestLagApiCounterBlock(t *testing.T) {
	uploadKey := [16]byte{}
	copy(uploadKey[:], testLagRandomBytes(16))
	block, err := aes.NewCipher(uploadKey[:])
	if err != nil {
		t.Fatal(err)
	}
	for _, position := range []struct {
		streamIndex uint64
		offset      uint64
	}{
		{0, 0},
		{3, 0},
		{1, 65536},
		{2, 7 * 65536},
		{15, 1 << 40},
	} {
		counterBlock := lagApiCounterBlock(position.streamIndex, position.offset)
		expected := make([]byte, 16)
		binary.BigEndian.PutUint64(expected[0:8], position.streamIndex)
		binary.BigEndian.PutUint64(expected[8:16], position.offset/16)
		if string(counterBlock[:]) != string(expected) {
			t.Errorf("counter block %x, expected %x", counterBlock, expected)
		}
		// the keystream at the offset is the block function of consecutive
		// counters from there
		keystream := make([]byte, 48)
		stream := testLagUploadStream(t, uploadKey[:], position.streamIndex)
		if position.offset < 1<<20 {
			skip := make([]byte, position.offset)
			stream.XORKeyStream(skip, skip)
			stream.XORKeyStream(keystream, keystream)
			for i := uint64(0); i < 3; i += 1 {
				counter := make([]byte, 16)
				binary.BigEndian.PutUint64(counter[0:8], position.streamIndex)
				binary.BigEndian.PutUint64(counter[8:16], position.offset/16+i)
				encrypted := make([]byte, 16)
				block.Encrypt(encrypted, counter)
				if string(encrypted) != string(keystream[16*i:16*i+16]) {
					t.Errorf("stream %d offset %d block %d differs", position.streamIndex, position.offset, i)
				}
			}
		}
	}
}

// M6, M7: with AES instructions every read is checked as it arrives; the exact
// stream is taken in whole, a short last message included; a changed byte, a
// text message or a message after a short one each end the phase as a
// mismatch and close the stream with 4400. What arrived before the failure
// stays taken in -- for a changed byte, the reads of its message before the
// one that holds it -- and nothing from the failure on is. Taken in is what
// drew on the cap: the part of a stream's first group from before the
// window's first byte is never counted in the series (amendment 15).
func TestLagApiUploadExact(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.api.exactVerification = true
	session := fixture.directSession(4)

	phase := fixture.directPhase(session, lagApiDirectionUpload, 10*time.Second, 1<<30)
	upload := fixture.openUpload(session, 2)
	for _, message := range testLagUploadMessages(t, session.uploadKey, 2, testLagRepeat(65536, 8, 1000)...) {
		if err := upload.send(websocket.BinaryMessage, message); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		return phase.consumed() == 8*65536+1000
	})
	if phase.consumed() < phase.total() {
		t.Errorf("counted %d of %d", phase.total(), phase.consumed())
	}
	if _, _, end := phase.state(); end != lagApiPhaseRunning {
		t.Errorf("exact stream ended %d", end)
	}
	upload.close()
	if phase.streamCount() != 1 {
		t.Errorf("%d streams", phase.streamCount())
	}

	for name, sendMessages := range map[string]func(upload *lagTestUpload){
		"changed byte": func(upload *lagTestUpload) {
			messages := testLagUploadMessages(t, session.uploadKey, 1, testLagRepeat(65536, 4)...)
			messages[3][65535] ^= 0x80
			for _, message := range messages {
				upload.send(websocket.BinaryMessage, message)
			}
		},
		"text message": func(upload *lagTestUpload) {
			messages := testLagUploadMessages(t, session.uploadKey, 1, testLagRepeat(65536, 3)...)
			for _, message := range messages {
				upload.send(websocket.BinaryMessage, message)
			}
			upload.send(websocket.TextMessage, []byte("{}"))
		},
		"after a short one": func(upload *lagTestUpload) {
			messages := testLagUploadMessages(t, session.uploadKey, 1, testLagRepeat(65536, 2, 512, 512)...)
			for _, message := range messages {
				upload.send(websocket.BinaryMessage, message)
			}
		},
		"wrong stream": func(upload *lagTestUpload) {
			// stream 0's keystream on stream 1
			messages := testLagUploadMessages(t, session.uploadKey, 0, testLagRepeat(65536, 3)...)
			for _, message := range messages {
				upload.send(websocket.BinaryMessage, message)
			}
		},
	} {
		phase := fixture.directPhase(session, lagApiDirectionUpload, 10*time.Second, 1<<30)
		upload := fixture.openUpload(session, 1)
		sendMessages(upload)
		if code := upload.waitClosed(5 * time.Second); code != lagApiCloseCodeMismatch {
			t.Errorf("%s: closed %d", name, code)
		}
		if _, _, end := phase.state(); end != lagApiPhaseEndMismatch {
			t.Errorf("%s: ended %d", name, end)
		}
		// a read is at most one TLS record, 16 KiB, and never runs past its
		// message, so the read that holds a message's last byte starts at
		// least 48 KiB into it
		expected := map[string][2]uint64{
			"changed byte":      {3*65536 + 49152, 3*65536 + 65535},
			"text message":      {3 * 65536, 3 * 65536},
			"after a short one": {2*65536 + 512, 2*65536 + 512},
			"wrong stream":      {0, 0},
		}[name]
		if taken := phase.consumed(); taken < expected[0] || expected[1] < taken || taken < phase.total() {
			t.Errorf("%s: took in %d and counted %d, expected %d to %d", name, taken, phase.total(), expected[0], expected[1])
		}
	}
}

// An upload phase outside any session whose 5 s window opened at startTime,
// with the ticket's cap and the daily budget given (0 is none). Its streams
// opened before its first byte, as a browser's do.
func testLagUploadPhase(startTime time.Time, maxStreamCount int, maxByteCount int64, budgetByteCount int64) *lagApiPhase {
	api := &extenderLagApi{budget: newLagApiBudget(budgetByteCount)}
	phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
		5*time.Second, lagApiInterval, maxStreamCount, maxByteCount, time.Minute, func() {})
	for k := 0; k < maxStreamCount; k += 1 {
		phase.openStream(k)
	}
	phase.window(startTime)
	return phase
}

// Whether stream k of the phase is open.
func testLagStreamOpen(phase *lagApiPhase, streamIndex int) bool {
	phase.stateLock.Lock()
	defer phase.stateLock.Unlock()
	return phase.openStreams[streamIndex]
}

// The phase's series as the report signs it, checked against what is
// expected.
func testLagExpectSeries(t *testing.T, phase *lagApiPhase, step string, expected ...uint64) {
	t.Helper()
	if values := phase.windowValues(); !slices.Equal(values, expected) {
		t.Fatalf("%s: series %v, expected %v", step, values, expected)
	}
}

// A clock for a stream's reads: each reading is step past the one before.
func testLagStepClock(startTime time.Time, step time.Duration) func() time.Time {
	now := startTime
	return func() time.Time {
		now = now.Add(step)
		return now
	}
}

// A message that arrives in reads of at most partSize bytes, then ends.
type testLagPartReader struct {
	message  []byte
	partSize int
}

func (self *testLagPartReader) Read(buffer []byte) (int, error) {
	if len(self.message) == 0 {
		return 0, io.EOF
	}
	n := copy(buffer[:min(self.partSize, len(buffer))], self.message)
	self.message = self.message[n:]
	return n, nil
}

// Amendment 14, rule S, as amendment 15 keeps it: once a stream's first group
// has counted, each group of its reads counts as having arrived evenly since
// the stream's previous read, but never from more than lagSpreadMax before
// it, split at the interval boundaries into whole bytes that sum to the read.
// A read at the same moment as its stream's previous one joins that read's
// group, and spreads with it. Every read the phase takes draws on the cap as
// it arrives, a held first group's included; progress reports the bytes
// counted, and when the latest counted read arrived (M4).
func TestLagApiUploadSpread(t *testing.T) {
	startTime := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time {
		return startTime.Add(time.Duration(ms) * time.Millisecond)
	}
	phase := testLagUploadPhase(startTime, 4, 1024*1024*1024, 0)
	defer phase.finish(lagApiPhaseEndClosed)

	// the read that opens the window is its stream's first group: at the
	// rate of the stream's next read, 2000 bytes in 200 ms, its 1000 bytes
	// took the 100 ms before the window's first byte, so none counts; the
	// next spreads back to it, within interval 0
	phase.receive(0, startTime, 1000)
	phase.receive(0, at(200), 2000)
	testLagExpectSeries(t, phase, "within an interval", 2000)
	// 50 ms either side of the 250 ms boundary
	phase.receive(0, at(300), 16384)
	testLagExpectSeries(t, phase, "across a boundary", 2000+8192, 8192)
	// a stream's first group, 1001 bytes, at the rate of its second, 500
	// bytes 250 ms later: over the 500.5 ms before it, rounded at each cut so
	// the pieces sum to the group; and the second spreads back to the first
	phase.receive(1, at(750), 1001)
	phase.receive(1, at(1000), 500)
	testLagExpectSeries(t, phase, "a stream's second group", 10192+1, 8192+500, 500, 500)
	// a read at its stream's previous read's moment joins its group and
	// spreads back with it
	phase.receive(1, at(1000), 300)
	testLagExpectSeries(t, phase, "no time since the stream's previous read", 10193, 8692, 500, 800)
	// 2 s after its stream's previous read, a read spreads over the last
	// lagSpreadMax only, 1300 to 2300 ms
	phase.receive(0, at(2300), 4000)
	testLagExpectSeries(t, phase, "after a stall", 10193, 8692, 500, 800, 0, 800, 1000, 1000, 1000, 200)
	// reads that arrived before the latest counted one count as well, and
	// progress keeps the latest arrival
	phase.receive(2, at(1900), 100)
	phase.receive(2, at(2000), 100)
	testLagExpectSeries(t, phase, "an earlier arrival", 10193, 8692, 500, 800, 0, 800, 1000, 1200, 1000, 200)
	if byteCount, countedTime := phase.progress(); byteCount != 24385 || !countedTime.Equal(at(2300)) {
		t.Errorf("progress %d at %s", byteCount, countedTime.Sub(startTime))
	}
	// the window's last interval takes its share of a read just before the
	// end
	phase.receive(0, at(4900), 1000)
	testLagExpectSeries(t, phase, "the window's last interval",
		10193, 8692, 500, 800, 0, 800, 1000, 1200, 1000, 200, 0, 0, 0, 0, 0, 100, 250, 250, 250, 150)
	// everything taken in drew on the cap, the 1000 bytes that never counted
	// included
	if byteCount, countedTime := phase.progress(); byteCount != 25385 || !countedTime.Equal(at(4900)) ||
		phase.consumed() != 26385 {
		t.Errorf("progress %d at %s, %d drawn on the cap", byteCount, countedTime.Sub(startTime), phase.consumed())
	}
}

// Amendment 15: reads of a stream that arrive within lagSpreadJoin of the
// first read of their group are one group, and each spreads from where the
// group's span starts, so a burst released after a stall spreads, all of it,
// over the stall, rather than piling into the interval it arrived in. The
// 2 ms is measured from the group's first read, so a steady fast stream is
// cut into groups of 2 ms: its bytes count where they arrived, and are never
// smeared back over the time before the stream sped up.
func TestLagApiUploadGroups(t *testing.T) {
	startTime := time.Unix(1_700_000_000, 0)
	at := func(ms float64) time.Time {
		return startTime.Add(time.Duration(ms * float64(time.Millisecond)))
	}
	phase := testLagUploadPhase(startTime, 2, 1024*1024*1024, 0)
	defer phase.finish(lagApiPhaseEndClosed)

	// stream 0 counts from 100 ms: its first group took the time before the
	// window opened
	phase.receive(0, startTime, 16384)
	phase.receive(0, at(100), 16384)
	testLagExpectSeries(t, phase, "the stream's second group", 16384)
	// a stall, then four records back to back, 10 us apart: each spans the
	// stall, 100 to 900 ms, 150, 250, 250 and 150 ms of it in intervals 0 to
	// 3; then one 2.5 ms later starts a group of its own, which spans from
	// the burst's last read
	for i := 0; i < 4; i += 1 {
		phase.receive(0, at(900+0.01*float64(i)), 16384)
	}
	phase.receive(0, at(902.5), 16384)
	testLagExpectSeries(t, phase, "a burst after a stall", 16384+4*3072, 4*5120, 4*5120, 4*3072+16384)

	// stream 1 counts from 50 ms, then is quiet until it sends 1000 bytes
	// every 0.5 ms from 1100 ms to 1399.5 ms
	phase.receive(1, startTime, 1000)
	phase.receive(1, at(50), 1000)
	before := phase.windowValues()
	for i := 0; i < 600; i += 1 {
		phase.receive(1, at(1100+0.5*float64(i)), 1000)
	}
	values := phase.windowValues()
	added := func(index int) int64 {
		addedValue := int64(values[index])
		if index < len(before) {
			addedValue -= int64(before[index])
		}
		return addedValue
	}
	// the run's first group, five reads, spans the quiet time before it,
	// from 100 ms, a second back; each group after it spans no more than the
	// 2.5 ms since the group before, so intervals 4 and 5 hold the 300 reads
	// that arrived in each, give or take the group that straddles 1250 ms. A
	// group that ran on for as long as each read came within 2 ms of the one
	// before would span the run from 100 ms, and put most of it before 1000.
	if spread := added(0) + added(1) + added(2) + added(3); spread <= 0 || 5000 < spread {
		t.Errorf("%d bytes of the run before it began: %v", spread, values)
	}
	for _, index := range []int{4, 5} {
		if bytes := added(index); bytes < 300000-5000 || 300000+5000 < bytes {
			t.Errorf("interval %d took %d bytes of the 300000 that arrived in it: %v", index, bytes, values)
		}
	}
	if total := added(0) + added(1) + added(2) + added(3) + added(4) + added(5); total != 600000 {
		t.Errorf("counted %d of the run's 600000 bytes", total)
	}
}

// Amendment 15: a stream's first group is held, uncounted, until its second
// group begins, and then counts as having arrived at the rate of the second
// group's first read, back from its own last read, at most lagSpreadMax.
// Only its part from the window's first byte on counts, so most of the read
// that opened the window never does. A first group that no second group
// places counts by rule S, from at most lagSpreadMax before its last read and
// never before the window's first byte: when its stream ends, when the window
// becomes final, and with no time between the groups. A held group draws on
// the cap as it arrives, and its reads are not progress's latest arrival
// until it counts.
func TestLagApiUploadFirstGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		at := func(ms int) time.Time {
			return startTime.Add(time.Duration(ms) * time.Millisecond)
		}
		api := &extenderLagApi{budget: newLagApiBudget(0)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
			5*time.Second, lagApiInterval, 5, 1<<30, time.Minute, func() {})
		for k := 0; k < 5; k += 1 {
			phase.openStream(k)
		}
		phase.window(startTime)

		// held: two reads 1 ms apart are one group, and nothing counts yet
		phase.receive(0, startTime, 16384)
		phase.receive(0, at(1), 16384)
		if byteCount, countedTime := phase.progress(); byteCount != 0 || !countedTime.IsZero() || phase.consumed() != 32768 {
			t.Errorf("held: counted %d at %s, %d drawn on the cap", byteCount, countedTime, phase.consumed())
		}
		// the second group begins, 8192 bytes 100 ms after the first's last
		// read: at that rate the first took 400 ms, 399 of them before the
		// window opened, so 1 ms of it, 82 bytes, counts
		phase.receive(0, at(101), 8192)
		testLagExpectSeries(t, phase, "the window's first group", 82+8192)
		if _, countedTime := phase.progress(); !countedTime.Equal(at(101)) {
			t.Errorf("latest arrival %s", countedTime.Sub(startTime))
		}
		// inside the window: 16384 bytes, then as much 100 ms later, place
		// the first over the 100 ms before it
		phase.receive(1, at(600), 16384)
		phase.receive(1, at(700), 16384)
		testLagExpectSeries(t, phase, "a first group's rate", 8274, 0, 32768)
		// at most lagSpreadMax back: 16384 bytes, then 1000 2 s later, a rate
		// at which the first would have taken 33 s
		phase.receive(2, at(1500), 16384)
		phase.receive(2, at(3500), 1000)
		testLagExpectSeries(t, phase, "at most lagSpreadMax back",
			8274, 0, 32768+4096, 4096, 4096, 4096, 0, 0, 0, 0, 250, 250, 250, 250)
		// the stream ends with its first group held: rule S, 1000 to 2000 ms
		phase.receive(3, at(2000), 6000)
		phase.closeStream(3)
		testLagExpectSeries(t, phase, "its stream ended",
			8274, 0, 36864, 4096, 5596, 5596, 1500, 1500, 0, 0, 250, 250, 250, 250)
		// the window becomes final with a first group held, lagSpreadMax past
		// the end, the only stream open not done: rule S, 3800 to 4800 ms
		phase.receive(4, at(4800), 2000)
		for k := 0; k < 3; k += 1 {
			phase.closeStream(k)
		}
		time.Sleep(5*time.Second + lagSpreadMax - time.Nanosecond)
		synctest.Wait()
		if _, _, end := phase.state(); end != lagApiPhaseEndTime || phase.windowFinal() {
			t.Fatalf("ended %d, final %t, before lagSpreadMax past the end", end, phase.windowFinal())
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !phase.windowFinal() {
			t.Fatal("not final lagSpreadMax past the end")
		}
		testLagExpectSeries(t, phase, "its window became final",
			8274, 0, 36864, 4096, 5596, 5596, 1500, 1500, 0, 0, 250, 250, 250, 250, 0, 400, 500, 500, 500, 100)
		if byteCount, countedTime := phase.progress(); byteCount != 66426 || !countedTime.Equal(at(4800)) ||
			phase.consumed() != 99112 {
			t.Errorf("counted %d to %s, %d drawn on the cap", byteCount, countedTime.Sub(startTime), phase.consumed())
		}

		// no time between the groups: rule S, 0 to 1000 ms
		phase = newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
			5*time.Second, lagApiInterval, 1, 1<<30, time.Minute, func() {})
		defer phase.finish(lagApiPhaseEndClosed)
		phase.openStream(0)
		startTime = time.Now()
		phase.window(startTime)
		phase.receive(0, startTime.Add(time.Second), 3000)
		func() {
			phase.stateLock.Lock()
			defer phase.stateLock.Unlock()
			phase.placeFirstWithLock(0, startTime.Add(time.Second), 3000)
		}()
		testLagExpectSeries(t, phase, "no time between the groups", 750, 750, 750, 750)
	})
}

// An upload phase in a bubble with streamCount streams open and its window
// open now, whose streams each count from 100 ms, 1000 bytes each in interval
// 0: their first groups, at 0, took the time before the window opened. Each
// call to its arrive sleeps until after past the window's first byte, then
// counts a read that arrives then, so its timers run as they would.
func testLagFinalPhase(streamCount int) (*lagApiPhase, time.Time, *atomic.Int64, func(streamIndex int, after time.Duration, byteCount int)) {
	api := &extenderLagApi{budget: newLagApiBudget(0)}
	notifyCount := &atomic.Int64{}
	phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
		5*time.Second, lagApiInterval, streamCount, 1<<30, time.Minute, func() {
			notifyCount.Add(1)
		})
	for k := 0; k < streamCount; k += 1 {
		phase.openStream(k)
	}
	startTime := time.Now()
	phase.window(startTime)
	arrive := func(streamIndex int, after time.Duration, byteCount int) {
		time.Sleep(time.Until(startTime.Add(after)))
		phase.receive(streamIndex, time.Now(), byteCount)
	}
	for k := 0; k < streamCount; k += 1 {
		arrive(k, 0, 1000)
	}
	for k := 0; k < streamCount; k += 1 {
		arrive(k, 100*time.Millisecond, 1000)
	}
	return phase, startTime, notifyCount, arrive
}

// ms milliseconds.
func testLagMs(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

// The 20 intervals of a 5 s window: first, then zeros, then the tail.
func testLagWindowSeries(first uint64, tail ...uint64) []uint64 {
	series := make([]uint64, 20)
	series[0] = first
	copy(series[len(series)-len(tail):], tail)
	return series
}

// Amendment 15: past the window's end the phase has ended by its time, at the
// end, but its streams' reads still count for the part of their spans before
// the end. A stream is done lagSpreadJoin after the first read of its first
// group that begins at or after the end: a read that joins that group, up to
// lagSpreadJoin after its first read, still counts its part before the end,
// and nothing the stream reads after does. The window is final once every
// stream still open is done -- when no read comes to show it, by the done
// timer; a stream that closes is no longer open -- or lagSpreadMax past the
// end, by the clock or by a read that arrives later than that. Then nothing
// more counts, and the session is told. The series is still the window's 20
// intervals.
func TestLagApiUploadWindowFinal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// every stream done, with no read to show it
		phase, startTime, notifyCount, arrive := testLagFinalPhase(3)
		// 3900 to 4900 ms
		arrive(0, testLagMs(4900), 1000)
		// past the end the phase has ended by its time, at the end, and a
		// read counts for its span's part before it, 4900 to 5000 of 4900 to
		// 5100. It begins stream 0's first group past the end.
		arrive(0, testLagMs(5100), 1000)
		if _, endedTime, end := phase.state(); end != lagApiPhaseEndTime || !endedTime.Equal(startTime.Add(5*time.Second)) || phase.windowFinal() {
			t.Errorf("ended %d at %s, final %t", end, endedTime.Sub(startTime), phase.windowFinal())
		}
		testLagExpectSeries(t, phase, "a read that straddles the end", testLagWindowSeries(3000, 100, 250, 250, 250, 150+500)...)
		// 2 ms later a read still joins that group, and counts its part
		// before the end: 100 of the 202 ms from 4900
		arrive(0, testLagMs(5102), 2020)
		testLagExpectSeries(t, phase, "a read that joins the group", testLagWindowSeries(3000, 100, 250, 250, 250, 650+1000)...)
		// after that, stream 0 is done: nothing more of it counts
		arrive(0, testLagMs(5102.001), 50000)
		testLagExpectSeries(t, phase, "a read once the stream is done", testLagWindowSeries(3000, 100, 250, 250, 250, 1650)...)
		// 4200 to 5200 ms, of which 4200 to 5000 counts
		arrive(1, testLagMs(5200), 2000)
		testLagExpectSeries(t, phase, "a stream quiet since before the end",
			testLagWindowSeries(3000, 100, 250+100, 250+500, 250+500, 1650+500)...)
		// stream 2 closes and is no longer open; stream 1 is not yet done
		phase.closeStream(2)
		if phase.windowFinal() {
			t.Error("final with a stream open and not done")
		}
		// stream 1 is done 2 ms after its group began, with no read to show
		// it, and the window is final
		notified := notifyCount.Load()
		time.Sleep(time.Until(startTime.Add(testLagMs(5202))))
		synctest.Wait()
		if phase.windowFinal() {
			t.Error("final with a read still able to join a group")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !phase.windowFinal() || notifyCount.Load() <= notified {
			t.Errorf("final %t once every stream was done, notified %d then %d", phase.windowFinal(), notified, notifyCount.Load())
		}
		arrive(1, testLagMs(5400), 1000)
		testLagExpectSeries(t, phase, "final", testLagWindowSeries(3000, 100, 350, 750, 750, 2150)...)
		if byteCount, countedTime := phase.progress(); byteCount != 3000+4100 || !countedTime.Equal(startTime.Add(testLagMs(5200))) {
			t.Errorf("counted %d to %s", byteCount, countedTime.Sub(startTime))
		}

		// a stream with no read past the end is never done: final
		// lagSpreadMax past the end, by the clock
		phase, startTime, notifyCount, arrive = testLagFinalPhase(2)
		arrive(0, testLagMs(4900), 1000)
		arrive(0, testLagMs(5100), 1000)
		time.Sleep(time.Until(startTime.Add(6*time.Second - time.Nanosecond)))
		synctest.Wait()
		if phase.windowFinal() {
			t.Error("final before lagSpreadMax past the end")
		}
		notified = notifyCount.Load()
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !phase.windowFinal() || notifyCount.Load() <= notified {
			t.Errorf("final %t lagSpreadMax past the end, notified %d then %d", phase.windowFinal(), notified, notifyCount.Load())
		}
		arrive(1, testLagMs(6100), 1000)
		testLagExpectSeries(t, phase, "final by the clock", testLagWindowSeries(2000, 100, 250, 250, 250, 650)...)

		// final by a read that arrives later than lagSpreadMax past the end,
		// with the clock still early: no read that late can reach back before
		// the end. One exactly lagSpreadMax past it spans from the end on.
		phase, startTime, _, _ = testLagFinalPhase(2)
		phase.receive(0, startTime.Add(testLagMs(5500)), 1000)
		if _, endedTime, end := phase.state(); end != lagApiPhaseEndTime || !endedTime.Equal(startTime.Add(5*time.Second)) || phase.windowFinal() {
			t.Errorf("ended %d at %s by a read, final %t", end, endedTime.Sub(startTime), phase.windowFinal())
		}
		phase.receive(1, startTime.Add(testLagMs(6000)), 1000)
		if phase.windowFinal() {
			t.Error("final on a read exactly lagSpreadMax past the end")
		}
		phase.receive(1, startTime.Add(testLagMs(6000.001)), 1000)
		if !phase.windowFinal() {
			t.Error("not final after a read later than lagSpreadMax past the end")
		}
		testLagExpectSeries(t, phase, "final by a late read", testLagWindowSeries(2000, 0, 0, 0, 250, 250)...)
	})
}

// Amendment 15: past the window's end, every read the phase takes in draws on
// the ticket's cap and the daily budget for all its bytes, though only its
// part before the end counts, or none of it for a read that spans from the
// end on: they are bytes the extender moved, as a download's writes in its
// grace are. Reads once the stream is done, lagSpreadJoin after its first
// group past the end began, or once the window is final, are drained and draw
// on neither.
func TestLagApiUploadPastTheEndDrawsOnTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &extenderLagApi{budget: newLagApiBudget(0)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
			5*time.Second, lagApiInterval, 2, 1<<30, time.Minute, func() {})
		for k := 0; k < 2; k += 1 {
			phase.openStream(k)
		}
		startTime := time.Now()
		phase.window(startTime)
		at := func(ms float64) time.Time {
			return startTime.Add(testLagMs(ms))
		}
		for k := 0; k < 2; k += 1 {
			phase.receive(k, startTime, 1000)
			phase.receive(k, at(100), 1000)
		}
		expectDrawn := func(step string, drawn uint64, counted uint64) {
			t.Helper()
			if phase.consumed() != drawn || uint64(lagTestDurationBudgetUsed(api.budget)) != drawn || phase.total() != counted {
				t.Errorf("%s: %d drawn on the cap and %d on the budget, %d counted; expected %d drawn, %d counted",
					step, phase.consumed(), lagTestDurationBudgetUsed(api.budget), phase.total(), drawn, counted)
			}
		}
		// both streams' first groups drew, and counted nothing
		expectDrawn("inside the window", 4000, 2000)
		// stream 0: 3900 to 4900 ms, then 4900 to 5100 ms, its first group
		// past the end: half of that counts, all of it draws
		phase.receive(0, at(4900), 1000)
		phase.receive(0, at(5100), 1000)
		expectDrawn("a read that straddles the end", 6000, 3500)
		// stream 1: a group that began before the end, joined 2 ms later past
		// it: 999 of the joining read's 1000 bytes count, all of them draw
		phase.receive(1, at(4999), 1000)
		phase.receive(1, at(5001), 1000)
		expectDrawn("a read that joins a group from before the end", 8000, 5499)
		// then stream 1's first group past the end, which spans from 5001 ms:
		// none of it counts, all of it draws, and so does a read that joins it
		phase.receive(1, at(5004), 1000)
		phase.receive(1, at(5006), 1000)
		expectDrawn("reads that span from the end on", 10000, 5499)
		// once each stream is done, nothing more of it is taken in: with both
		// done, the window is final
		phase.receive(1, at(5006.001), 1000)
		phase.receive(0, at(5102.001), 1000)
		expectDrawn("once the streams are done", 10000, 5499)
		if !phase.windowFinal() {
			t.Error("not final with every stream done")
		}
	})
}

// M4, amendment 15: a progress report reads its ms and the phase's state at
// one moment under the phase's lock, so none before the window is final
// reads as over: one goes out only while the phase runs, and only with ms
// short of the window's length less half an interval, which the browser
// takes as the window's end; one later than that is not sent. The report that
// says the window is over goes out once the window is final, with the
// window's length -- the phase's, when it ended sooner -- and the final
// total. at is when the latest counted read arrived, held to ms.
func TestLagApiUploadProgressReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &extenderLagApi{budget: newLagApiBudget(0)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
			time.Second, lagApiInterval, 1, 1<<30, time.Minute, func() {})
		phase.openStream(0)
		if _, _, _, ok := phase.progressReport(false); ok {
			t.Error("a progress before the first byte")
		}
		startTime := time.Now()
		phase.window(startTime)
		phase.receive(0, startTime, 1000)
		phase.receive(0, startTime.Add(100*time.Millisecond), 1000)
		// 875 ms is the window's length less half an interval
		if lagApiWindowOverMs(time.Second, lagApiInterval) != 875 {
			t.Fatalf("the window reads as over from %f ms", lagApiWindowOverMs(time.Second, lagApiInterval))
		}
		time.Sleep(875*time.Millisecond - time.Microsecond)
		if byteCount, ms, at, ok := phase.progressReport(false); !ok || byteCount != 1000 || ms != 874 || at != 100 {
			t.Errorf("progress of %d at %d ms, at %f, sent %t", byteCount, ms, at, ok)
		}
		time.Sleep(time.Microsecond)
		if _, ms, _, ok := phase.progressReport(false); ok {
			t.Errorf("a progress at %d ms, which reads as the window being over", ms)
		}
		if _, _, _, ok := phase.progressReport(true); ok {
			t.Error("the last progress before the window ended")
		}
		// the window runs out, and a read straddles its end, beginning the
		// stream's first group past it: ended by its time, and not final
		// while a read can still join that group, so no progress goes out
		time.Sleep(time.Until(startTime.Add(1100 * time.Millisecond)))
		phase.receive(0, time.Now(), 1000)
		if _, _, end := phase.state(); end != lagApiPhaseEndTime || phase.windowFinal() {
			t.Fatalf("ended %d, final %t", end, phase.windowFinal())
		}
		for _, last := range []bool{false, true} {
			if _, ms, _, ok := phase.progressReport(last); ok {
				t.Errorf("last %t: a progress at %d ms before the window was final", last, ms)
			}
		}
		// final once the stream is done, lagSpreadJoin after that group
		// began: the window's length and the final total, 900 bytes of the
		// straddling read's 1000 among it
		time.Sleep(lagSpreadJoin + time.Nanosecond)
		synctest.Wait()
		if byteCount, ms, at, ok := phase.progressReport(true); !phase.windowFinal() || !ok || byteCount != 1900 || ms != 1000 || at != 1000 {
			t.Errorf("last progress of %d at %d ms, at %f, sent %t, final %t", byteCount, ms, at, ok, phase.windowFinal())
		}
		if _, _, _, ok := phase.progressReport(false); ok {
			t.Error("a progress after the window was final")
		}

		// ended sooner, by the cap: the phase's length
		phase = newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
			time.Second, lagApiInterval, 1, 2500, time.Minute, func() {})
		phase.openStream(0)
		startTime = time.Now()
		phase.window(startTime)
		phase.receive(0, startTime, 1000)
		time.Sleep(300 * time.Millisecond)
		phase.receive(0, time.Now(), 2000)
		if _, _, end := phase.state(); end != lagApiPhaseEndCap {
			t.Fatalf("ended %d", end)
		}
		if byteCount, ms, at, ok := phase.progressReport(true); !ok || byteCount != 2000 || ms != 300 || at != 300 {
			t.Errorf("capped: last progress of %d at %d ms, at %f, sent %t", byteCount, ms, at, ok)
		}
	})
}

// Amendment 15: once the window has run out, a mismatch ends only its stream's
// counting -- the phase ended by its time, and every read that counted was
// checked -- and the stream is done. Inside the window a mismatch ends the
// phase, as before (M6).
func TestLagApiUploadMismatchPastTheEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		phase, startTime, _, _ := testLagFinalPhase(2)
		at := func(ms int) time.Time {
			return startTime.Add(time.Duration(ms) * time.Millisecond)
		}
		phase.receive(0, at(4900), 1000)
		phase.mismatch(0, at(5050))
		if _, endedTime, end := phase.state(); end != lagApiPhaseEndTime || !endedTime.Equal(at(5000)) || phase.windowFinal() {
			t.Fatalf("ended %d at %s, final %t", end, endedTime.Sub(startTime), phase.windowFinal())
		}
		phase.receive(0, at(5100), 1000)
		// stream 1's read that straddles the end still counts, and the
		// stream is done 2 ms after it
		phase.receive(1, at(5200), 2000)
		phase.receive(1, at(5300), 1000)
		if !phase.windowFinal() {
			t.Error("not final with every stream done")
		}
		if total := phase.total(); total != 2000+1000+1600 {
			t.Errorf("counted %d", total)
		}

		phase, startTime, _, _ = testLagFinalPhase(1)
		phase.mismatch(0, startTime.Add(4999*time.Millisecond))
		if _, _, end := phase.state(); end != lagApiPhaseEndMismatch || !phase.windowFinal() || phase.total() != 1000 {
			t.Errorf("inside the window: ended %d, final %t, counted %d", end, phase.windowFinal(), phase.total())
		}
	})
}

// An upload phase in a bubble with two streams open, and a send that has
// stream k's next message, of byteCount bytes of its keystream, arrive after
// past now -- the first opens the window -- read by the stream in records of
// 16 KiB, and changed in its last byte when corrupt.
func testLagShortUpload(t *testing.T, exact bool, sampleMessage func() bool) (*lagApiPhase, time.Time, func(streamIndex int, after time.Duration, byteCount int, corrupt bool)) {
	api := &extenderLagApi{budget: newLagApiBudget(0)}
	phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
		5*time.Second, lagApiInterval, 2, 1<<30, time.Minute, func() {})
	uploadKey := [16]byte{}
	copy(uploadKey[:], testLagRandomBytes(16))
	streams := []*lagApiUploadStream{}
	keystreams := []cipher.Stream{}
	for k := 0; k < 2; k += 1 {
		phase.openStream(k)
		stream, err := newLagApiUploadStream(phase, uploadKey, k, exact, sampleMessage)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, stream)
		keystreams = append(keystreams, testLagUploadStream(t, uploadKey[:], uint64(k)))
	}
	startTime := time.Now()
	send := func(streamIndex int, after time.Duration, byteCount int, corrupt bool) {
		time.Sleep(time.Until(startTime.Add(after)))
		message := make([]byte, byteCount)
		keystreams[streamIndex].XORKeyStream(message, message)
		if corrupt {
			message[byteCount-1] ^= 1
		}
		reader := &testLagPartReader{message: message, partSize: 16384}
		if err := streams[streamIndex].readMessage(websocket.BinaryMessage, reader, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	return phase, startTime, send
}

// Amendment 15: a stream's last message may be short, as a browser sends it
// near the window's end to fit the room its flow control has left; nothing
// valid can follow one (M6). Once it has arrived whole, and been checked, its
// stream is done, as one that closed is: it no longer holds the window open
// past its end, though with no read past the end it would have held it
// lagSpreadMax. One that straddles the end counts its part before it, as any
// read does. A first group still held when its stream finishes counts by rule
// S. Under sampled verification the stream finishes once the sample is
// decided.
func TestLagApiUploadShortMessageFinishesStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seconds := func(count int) time.Duration {
			return time.Duration(count) * time.Second
		}
		expectFinal := func(step string, phase *lagApiPhase, end lagApiPhaseEnd, final bool) {
			t.Helper()
			if _, _, phaseEnd := phase.state(); phaseEnd != end || phase.windowFinal() != final {
				t.Fatalf("%s: ended %d, final %t; expected %d, %t", step, phaseEnd, phase.windowFinal(), end, final)
			}
		}

		// stream 0's short last message comes before the end; stream 1 goes
		// on past it, and is done 2 ms into its first group there: the
		// window is final then, not lagSpreadMax past the end
		phase, startTime, send := testLagShortUpload(t, true, nil)
		for count := 0; count < 5; count += 1 {
			send(0, seconds(count), lagApiBufferByteCount, false)
			send(1, seconds(count), lagApiBufferByteCount, false)
		}
		send(0, testLagMs(4500), 16384, false)
		expectFinal("a short message before the end", phase, lagApiPhaseRunning, false)
		send(1, testLagMs(5100), lagApiBufferByteCount, false)
		expectFinal("a stream past the end", phase, lagApiPhaseEndTime, false)
		time.Sleep(time.Until(startTime.Add(testLagMs(5102))))
		synctest.Wait()
		expectFinal("2 ms into its first group past the end", phase, lagApiPhaseEndTime, false)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		expectFinal("once stream 1 is done", phase, lagApiPhaseEndTime, true)
		// every message counts from 0 but each stream's first, which took
		// the time before the window opened; stream 1's past the end counts
		// 4100 to 5000 ms of its 4100 to 5100 ms
		if total := phase.total(); total != 4*65536+16384+4*65536+4*14746 {
			t.Errorf("counted %d", total)
		}

		// both streams' short last messages come before the end: the window
		// is final at the end itself
		phase, startTime, send = testLagShortUpload(t, true, nil)
		for count := 0; count < 5; count += 1 {
			send(0, seconds(count), lagApiBufferByteCount, false)
			if count < 4 {
				send(1, seconds(count), lagApiBufferByteCount, false)
			}
		}
		send(1, testLagMs(3500), 16384, false)
		send(0, testLagMs(4500), 16384, false)
		time.Sleep(time.Until(startTime.Add(5*time.Second - time.Nanosecond)))
		synctest.Wait()
		expectFinal("before the end", phase, lagApiPhaseRunning, false)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		expectFinal("at the end", phase, lagApiPhaseEndTime, true)

		// a short last message that straddles the end counts its part before
		// it, 4050 to 5000 of 4050 to 5050 ms, and with the other stream
		// finished the window is final as soon as it has arrived
		phase, startTime, send = testLagShortUpload(t, true, nil)
		for count := 0; count < 5; count += 1 {
			send(0, seconds(count), lagApiBufferByteCount, false)
			send(1, seconds(count), lagApiBufferByteCount, false)
		}
		send(1, testLagMs(4500), 16384, false)
		send(0, testLagMs(5050), 16384, false)
		expectFinal("a short message that straddles the end", phase, lagApiPhaseEndTime, true)
		if total := phase.total(); total != 8*65536+16384+15565 {
			t.Errorf("counted %d", total)
		}

		// a stream whose only message is short: its first group, held,
		// counts by rule S once the message is whole, 1000 to 2000 ms
		phase, _, send = testLagShortUpload(t, true, nil)
		send(1, 0, lagApiBufferByteCount, false)
		send(0, seconds(2), 16384, false)
		testLagExpectSeries(t, phase, "a first group held", 0, 0, 0, 0, 4096, 4096, 4096, 4096)
		phase.finish(lagApiPhaseEndClosed)

		// under sampled verification, chosen or not, the stream finishes once
		// the sample is decided; a chosen one that fails is a mismatch
		for _, chosen := range []bool{true, false} {
			phase, startTime, send = testLagShortUpload(t, false, func() bool {
				return chosen
			})
			for count := 0; count < 4; count += 1 {
				send(0, seconds(count), lagApiBufferByteCount, false)
				send(1, seconds(count), lagApiBufferByteCount, false)
			}
			send(0, testLagMs(3500), 16384, false)
			send(1, testLagMs(3500), 16384, false)
			time.Sleep(time.Until(startTime.Add(5 * time.Second)))
			synctest.Wait()
			expectFinal(fmt.Sprintf("sampled, chosen %t", chosen), phase, lagApiPhaseEndTime, true)
		}
		phase, _, send = testLagShortUpload(t, false, func() bool {
			return true
		})
		send(0, 0, lagApiBufferByteCount, false)
		send(0, seconds(1), 16384, true)
		expectFinal("a chosen short message that fails", phase, lagApiPhaseEndMismatch, true)
	})
}

// Amendments 14 and 15: a spread read's pieces are whole bytes, each within a
// byte of its interval's exact share of the span, and those of a span inside
// the series' start and the bound sum to the read. Only the part of a span
// from the series' start to the bound counts: a span that reaches back before
// the start or on past the bound loses the share outside, to within a byte. A
// span of no length puts the read whole in the interval where it ends, if
// that is inside the bounds.
func TestLagApiSeriesAddSpread(t *testing.T) {
	startTime := time.Unix(1_700_000_000, 0)
	random := mathrand.New(mathrand.NewPCG(14, 0))
	for i := 0; i < 20000; i += 1 {
		series := newLagApiSeries(startTime, lagApiInterval, 7*time.Second)
		until := 7 * time.Second
		spanStart := time.Duration(random.Int64N(int64(5 * time.Second)))
		if i%2 == 1 {
			// reaching back before the start, or on past a bound in the
			// window, or both
			spanStart -= time.Duration(random.Int64N(int64(lagSpreadMax)))
			until = time.Duration(random.Int64N(int64(6 * time.Second)))
		}
		spanEnd := spanStart + time.Duration(random.Int64N(int64(lagSpreadMax)+1))
		if i%10 == 0 {
			spanEnd = spanStart
		}
		byteCount := 1 + random.IntN(64*1024)
		series.addSpread(startTime.Add(spanStart), startTime.Add(spanEnd), byteCount, startTime.Add(until))
		total := uint64(0)
		expectedTotal := 0.0
		for index, value := range series.values(len(series.counts)) {
			total += value
			share := 0.0
			if spanEnd == spanStart {
				if 0 <= spanEnd && spanEnd < until && index == int(spanEnd/lagApiInterval) {
					share = float64(byteCount)
				}
			} else {
				overlap := min(spanEnd, until, time.Duration(index+1)*lagApiInterval) - max(spanStart, 0, time.Duration(index)*lagApiInterval)
				share = float64(byteCount) * float64(max(0, overlap)) / float64(spanEnd-spanStart)
			}
			expectedTotal += share
			// two roundings of half a byte at most, and the float error
			if 1+1e-6 < math.Abs(float64(value)-share) {
				t.Fatalf("%d bytes over %s to %s until %s: interval %d took %d of a %.2f share",
					byteCount, spanStart, spanEnd, until, index, value, share)
			}
		}
		// the pieces telescope: rounded at the first cut and the last
		if 1+1e-6 < math.Abs(float64(total)-expectedTotal) {
			t.Fatalf("%d bytes over %s to %s until %s: counted %d of %.2f", byteCount, spanStart, spanEnd, until, total, expectedTotal)
		}
		if 0 <= spanStart && spanEnd < until && total != uint64(byteCount) {
			t.Fatalf("%d bytes over %s to %s: counted %d", byteCount, spanStart, spanEnd, total)
		}
	}
}

// M8, amendment 14: the upload's series as the report signs it runs from the
// window's first byte through the phase's end, the intervals that counted
// nothing as zeros: the whole of a window that ran out, and through its end
// for a phase that ended early. Before its end it runs through its last
// counted interval. A download's sent series stays trimmed.
func TestLagApiUploadWindowValues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		newPhase := func() (*lagApiPhase, time.Time) {
			api := &extenderLagApi{budget: newLagApiBudget(0)}
			phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
				5*time.Second, lagApiInterval, 1, 1024*1024*1024, time.Minute, func() {})
			startTime := time.Now()
			phase.window(startTime)
			return phase, startTime
		}

		// bytes for 600 ms, then nothing until the window runs out; the first
		// read, which opened the window, took the time before it at the
		// second's rate, so none of it counts (amendment 15)
		phase, startTime := newPhase()
		phase.receive(0, startTime, 1000)
		phase.receive(0, startTime.Add(600*time.Millisecond), 6000)
		if values := phase.windowValues(); !slices.Equal(values, []uint64{2500, 2500, 1000}) {
			t.Errorf("before the end: %v", values)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		expected := append([]uint64{2500, 2500, 1000}, make([]uint64, 17)...)
		if _, _, end := phase.state(); end != lagApiPhaseEndTime || !slices.Equal(phase.windowValues(), expected) {
			t.Errorf("ended %d with %v", end, phase.windowValues())
		}
		if values := phase.seriesValues(); !slices.Equal(values, []uint64{2500, 2500, 1000}) {
			t.Errorf("trimmed %v", values)
		}

		// closed 1.1 s in: five intervals, through the one the end falls in;
		// the first read, with no second to place it, counts where it arrived
		phase, startTime = newPhase()
		phase.receive(0, startTime, 1000)
		time.Sleep(1100 * time.Millisecond)
		phase.finish(lagApiPhaseEndClosed)
		if _, _, end := phase.state(); end != lagApiPhaseEndClosed || !slices.Equal(phase.windowValues(), []uint64{1000, 0, 0, 0, 0}) {
			t.Errorf("ended %d with %v", end, phase.windowValues())
		}
	})
}

// M3, M6: an upload's reads draw on the ticket's cap and the daily budget one
// by one as they arrive, so the read that reaches either ends the phase as
// capped, partway through a message, counted with the reads before it; nothing
// after it counts.
func TestLagApiUploadCapsPerRead(t *testing.T) {
	// the window opens now: a cap reached once the window has run out by the
	// clock is its time instead (finish)
	startTime := time.Now()
	for _, c := range []struct {
		name            string
		maxByteCount    int64
		budgetByteCount int64
	}{
		{name: "cap", maxByteCount: 65536 + 20000},
		{name: "budget", maxByteCount: 1024 * 1024 * 1024, budgetByteCount: 65536 + 20000},
	} {
		phase := testLagUploadPhase(startTime, 1, c.maxByteCount, c.budgetByteCount)
		uploadKey := [16]byte{}
		copy(uploadKey[:], testLagRandomBytes(16))
		stream, err := newLagApiUploadStream(phase, uploadKey, 0, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		now := testLagStepClock(startTime, 10*time.Millisecond)
		for _, message := range testLagUploadMessages(t, uploadKey, 0, 65536, 65536) {
			if err := stream.readMessage(websocket.BinaryMessage, &testLagPartReader{message: message, partSize: 16384}, now); err != nil {
				t.Fatal(err)
			}
		}
		// the second message's second read reached it
		if _, _, end := phase.state(); end != lagApiPhaseEndCap || phase.total() != 65536+2*16384 {
			t.Errorf("%s: ended %d with %d counted", c.name, end, phase.total())
		}
		phase.finish(lagApiPhaseEndClosed)
	}
}

// M6, amendment 14: with AES instructions a read counts once it is checked,
// as it arrives, so a changed byte partway through a message ends the phase
// with the message's reads before it counted, and nothing from the read that
// holds it on. Without them every read counts on arrival, and the sample is
// chosen once its message has arrived whole: a chosen message that fails ends
// the phase with all of its reads counted. Either way nothing after it counts.
func TestLagApiUploadMismatchMidMessage(t *testing.T) {
	startTime := time.Unix(1_700_000_000, 0)
	for _, exact := range []bool{true, false} {
		phase := testLagUploadPhase(startTime, 1, 1024*1024*1024, 0)
		uploadKey := [16]byte{}
		copy(uploadKey[:], testLagRandomBytes(16))
		var reader *testLagPartReader
		stream, err := newLagApiUploadStream(phase, uploadKey, 0, exact, func() bool {
			if len(reader.message) != 0 {
				t.Errorf("exact %t: chosen with %d bytes of its message to come", exact, len(reader.message))
			}
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		messages := testLagUploadMessages(t, uploadKey, 0, testLagRepeat(65536, 5)...)
		// in message 3's third read
		messages[3][2*16384+100] ^= 1
		now := testLagStepClock(startTime, 10*time.Millisecond)
		for _, message := range messages {
			reader = &testLagPartReader{message: message, partSize: 16384}
			if err := stream.readMessage(websocket.BinaryMessage, reader, now); err != nil {
				t.Fatal(err)
			}
		}
		expected := uint64(4 * 65536)
		if exact {
			expected = 3*65536 + 2*16384
		}
		if _, _, end := phase.state(); end != lagApiPhaseEndMismatch || phase.total() != expected {
			t.Errorf("exact %t: ended %d with %d counted, expected %d", exact, end, phase.total(), expected)
		}
		phase.finish(lagApiPhaseEndClosed)
	}
}

// One read of a synthetic upload.
type testLagRead struct {
	arrivedTime time.Duration
	streamIndex int
	byteCount   int
}

// The reads of a synthetic upload on four streams (amendment 14's acceptance):
// 16 KiB each, at rates that sum to mbps with each stream up to 30% off an
// equal share, each from a random point in its own gap, with up to 5 ms of
// jitter; in order of arrival, from the first at 0, which opens the window,
// to past its end.
func testLagUploadReads(mbps float64, seed uint64) []testLagRead {
	random := mathrand.New(mathrand.NewPCG(seed, 0))
	weights := make([]float64, 4)
	weightSum := 0.0
	for k := range weights {
		weights[k] = 1 + 0.3*(2*random.Float64()-1)
		weightSum += weights[k]
	}
	reads := []testLagRead{}
	for k, weight := range weights {
		gapSeconds := 16384 / (mbps * 1e6 / 8 * weight / weightSum)
		for seconds := random.Float64() * gapSeconds; seconds < 8; seconds += gapSeconds {
			reads = append(reads, testLagRead{
				arrivedTime: time.Duration((seconds + 0.005*random.Float64()) * float64(time.Second)),
				streamIndex: k,
				byteCount:   16384,
			})
		}
	}
	slices.SortFunc(reads, func(a testLagRead, b testLagRead) int {
		return cmp.Compare(a.arrivedTime, b.arrivedTime)
	})
	firstTime := reads[0].arrivedTime
	for i := range reads {
		reads[i].arrivedTime -= firstTime
	}
	return reads
}

// The p-th percentile by nearest rank, as the server's lagNearestRank (A5).
func testLagNearestRank(values []float64, p float64) float64 {
	values = slices.Sorted(slices.Values(values))
	rank := int(math.Ceil(p / 100 * float64(len(values))))
	return values[min(max(rank-1, 0), len(values)-1)]
}

// The upload p50 in Mbps the server derives from a 5 s window's series (A5):
// over the intervals inside the window, as lagLoadIntervals keeps them.
func testLagUploadP50Mbps(byteCounts []uint64) float64 {
	intervalCount := int((5*time.Second + lagApiInterval - 1) / lagApiInterval)
	mbps := []float64{}
	for _, byteCount := range byteCounts[:min(intervalCount, len(byteCounts))] {
		mbps = append(mbps, float64(byteCount)*8/float64(lagApiInterval.Microseconds()))
	}
	if len(mbps) == 0 {
		return 0
	}
	return testLagNearestRank(mbps, 50)
}

// Amendment 14's acceptance, through the real counting: four upload streams of
// 16 KiB reads at slightly unequal rates with a few ms of jitter, counted as
// they arrive into a 5 s window of 250 ms intervals, give the p50 the server
// derives within 2% of the true rate from 1 to 50 Mbps, in each of 200 such
// uploads. At 0.5 Mbps a stream's reads come about lagSpreadMax apart or more,
// so one upload's p50 can be several percent off; the median of the 200 is
// within 5%. Counted whole where they arrived, as before, the same reads put
// 3 Mbps at 3.15, 4 at 4.19 and 5 at 4.72 -- steps of one read per interval.
func TestLagApiUploadSpreadAccuracy(t *testing.T) {
	const uploadCount = 200
	startTime := time.Unix(1_700_000_000, 0)
	t.Logf("p50 error over %d uploads: Mbps, median, p10, p90, min, max", uploadCount)
	for _, mbps := range []float64{0.5, 1, 2, 3, 4, 5, 10, 20, 50} {
		errorPercents := []float64{}
		for seed := uint64(0); seed < uploadCount; seed += 1 {
			phase := testLagUploadPhase(startTime, 4, 1024*1024*1024*1024, 0)
			for _, read := range testLagUploadReads(mbps, seed) {
				phase.receive(read.streamIndex, startTime.Add(read.arrivedTime), read.byteCount)
			}
			// the window runs out, if no read past it has ended it already, and
			// the series is the one the report signs
			phase.window(startTime.Add(5 * time.Second))
			errorPercents = append(errorPercents, 100*(testLagUploadP50Mbps(phase.windowValues())-mbps)/mbps)
		}
		median := testLagNearestRank(errorPercents, 50)
		minPercent := slices.Min(errorPercents)
		maxPercent := slices.Max(errorPercents)
		t.Logf("%5.1f %+6.2f%% %+6.2f%% %+6.2f%% %+6.2f%% %+6.2f%%", mbps, median,
			testLagNearestRank(errorPercents, 10), testLagNearestRank(errorPercents, 90), minPercent, maxPercent)
		if mbps < 1 {
			if 5 < math.Abs(median) {
				t.Errorf("%.1f Mbps: the median p50 is off by %+.2f%%", mbps, median)
			}
		} else if minPercent < -2 || 2 < maxPercent {
			t.Errorf("%.1f Mbps: p50s off by %+.2f%% to %+.2f%%", mbps, minPercent, maxPercent)
		}
	}
}

// M6, amendment 14, through a stream: one 64 KiB message sent slowly, in
// frames over about a second, as a slow uplink delivers it, is counted as its
// reads arrive, each spread over the time since the one before -- not whole in
// the one interval where it ended, which read as steps of 64 KiB per interval,
// 2.1 Mbps -- and in full, but for its first 8 KiB: they opened the window,
// and at the rate of what followed they took the time before it, so they do
// not count (amendment 15). Exact and sampled checking count the same way.
func TestLagApiUploadCountsBytesWhereTheyArrive(t *testing.T) {
	for _, exact := range []bool{true, false} {
		fixture := newLagApiFixture(t, nil)
		fixture.api.exactVerification = exact
		session := fixture.directSession(1)
		phase := fixture.directPhase(session, lagApiDirectionUpload, 5*time.Second, 1<<30)
		upload := fixture.openUploadWithWriteBuffer(session, 0, 4096)
		message := testLagUploadMessages(t, session.uploadKey, 0, lagApiBufferByteCount)[0]

		writer, err := upload.conn.NextWriter(websocket.BinaryMessage)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i += 1 {
			if _, err := writer.Write(message[i*8192 : (i+1)*8192]); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Millisecond)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool {
			return phase.consumed() == lagApiBufferByteCount
		})
		if total := phase.total(); total < lagApiBufferByteCount-8192 || lagApiBufferByteCount < total {
			t.Errorf("exact %t: counted %d", exact, total)
		}
		values := phase.seriesValues()
		counted := 0
		for _, value := range values {
			if value == lagApiBufferByteCount {
				t.Errorf("exact %t: the message was counted whole in one interval: %v", exact, values)
			}
			if 0 < value {
				counted += 1
			}
		}
		if counted < 3 {
			t.Errorf("exact %t: counted in %d intervals, expected the parts spread over at least 3: %v", exact, counted, values)
		}
		if _, _, end := phase.state(); end != lagApiPhaseRunning {
			t.Errorf("exact %t: ended %d", exact, end)
		}
		upload.close()
	}
}

// M6: without AES instructions one message in 16 is checked, chosen after it
// arrives, at its own offset; a change in a message that was not chosen is
// not seen, and one in a chosen message ends the phase. Every read is taken
// as it arrives (amendment 14), so the chosen message that failed was taken,
// and nothing after it is.
func TestLagApiUploadSampled(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.api.exactVerification = false
	var sampleCount atomic.Int64
	fixture.api.sampleMessage = func() bool {
		// every fourth message, starting with the first
		return sampleCount.Add(1)%4 == 1
	}
	session := fixture.directSession(4)

	phase := fixture.directPhase(session, lagApiDirectionUpload, 10*time.Second, 1<<30)
	upload := fixture.openUpload(session, 3)
	messages := testLagUploadMessages(t, session.uploadKey, 3, testLagRepeat(65536, 6)...)
	// message 2 is not chosen
	messages[2][7] ^= 1
	for _, message := range messages {
		if err := upload.send(websocket.BinaryMessage, message); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		return phase.consumed() == 6*65536
	})
	if _, _, end := phase.state(); end != lagApiPhaseRunning {
		t.Errorf("unsampled change ended the phase %d", end)
	}
	upload.close()

	sampleCount.Store(0)
	phase = fixture.directPhase(session, lagApiDirectionUpload, 10*time.Second, 1<<30)
	upload = fixture.openUpload(session, 3)
	messages = testLagUploadMessages(t, session.uploadKey, 3, testLagRepeat(65536, 6)...)
	// message 4 is chosen, at offset 4 * 65536
	messages[4][9] ^= 1
	for _, message := range messages {
		upload.send(websocket.BinaryMessage, message)
	}
	if code := upload.waitClosed(5 * time.Second); code != lagApiCloseCodeMismatch {
		t.Errorf("sampled change: closed %d", code)
	}
	if phase.consumed() != 5*65536 || phase.consumed() < phase.total() {
		t.Errorf("took in %d, counted %d", phase.consumed(), phase.total())
	}

	// the real choice: about one in sixteen
	chosenCount := 0
	for i := 0; i < 32000; i += 1 {
		if lagApiSampleMessage() {
			chosenCount += 1
		}
	}
	if chosenCount < 1600 || 2400 < chosenCount {
		t.Errorf("%d of 32000 chosen", chosenCount)
	}
}

// M5, M6: a stream outside its phase, past maxStreams, or already open is
// refused; a session that does not exist is 404, a malformed one 400.
func TestLagApiStreamRefusals(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.directSession(2)
	down := func(sessionText string, streamIndex string) int {
		response := fixture.get("/lag/v1/down?session="+sessionText+"&stream="+streamIndex, testLagOrigin)
		response.Body.Close()
		return response.StatusCode
	}
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.streamWait = 100 * time.Millisecond
	})
	for _, refusal := range []struct {
		session string
		stream  string
		status  int
	}{
		{session: "short", stream: "0", status: http.StatusBadRequest},
		{session: session.id.String(), stream: "x", status: http.StatusBadRequest},
		{session: session.id.String(), stream: "-1", status: http.StatusBadRequest},
		{session: session.id.String(), stream: "16", status: http.StatusBadRequest},
		{session: lagApiSessionId{}.String(), stream: "0", status: http.StatusNotFound},
		// no load yet: the stream waits for the phase, then gives up
		{session: session.id.String(), stream: "0", status: http.StatusConflict},
	} {
		if status := down(refusal.session, refusal.stream); status != refusal.status {
			t.Errorf("%s %s: %d, expected %d", refusal.session, refusal.stream, status, refusal.status)
		}
	}

	phase := fixture.directPhase(session, lagApiDirectionDownload, 2*time.Second, 1<<30)
	// past maxStreams
	if status := down(session.id.String(), "2"); status != http.StatusConflict {
		t.Errorf("stream past max: %d", status)
	}
	// one stream open at a time
	request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/lag/v1/down?session=%s&stream=1", testLagName, session.id), nil)
	request.Header.Set("Origin", testLagOrigin)
	response, err := fixture.httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/octet-stream" ||
		response.ContentLength != -1 || response.Header.Get("Content-Encoding") != "" ||
		len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Errorf("download: %d %v %d %v", response.StatusCode, response.Header, response.ContentLength, response.TransferEncoding)
	}
	if status := down(session.id.String(), "1"); status != http.StatusConflict {
		t.Errorf("stream already open: %d", status)
	}
	phase.finish(lagApiPhaseEndClosed)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	// the phase is over
	if status := down(session.id.String(), "0"); status != http.StatusConflict {
		t.Errorf("after the phase: %d", status)
	}

	// an upload stream is refused before its upgrade, with the same statuses:
	// the browser's client retries an upgrade that failed (S5)
	up := func(sessionText string, streamIndex string, origin string) int {
		conn, response, err := fixture.dialWebSocket("/lag/v1/up?session="+sessionText+"&stream="+streamIndex, origin)
		if err == nil {
			conn.Close()
			return http.StatusSwitchingProtocols
		}
		if response == nil {
			t.Fatal(err)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("refused upgrade headers %v", response.Header)
		}
		return response.StatusCode
	}
	fixture.directPhase(session, lagApiDirectionDownload, 2*time.Second, 1<<30)
	for _, refusal := range []struct {
		session string
		stream  string
		origin  string
		status  int
	}{
		{session: session.id.String(), stream: "0", origin: "https://evil.example", status: http.StatusForbidden},
		{session: session.id.String(), stream: "0", origin: "", status: http.StatusForbidden},
		{session: "nope", stream: "0", origin: testLagOrigin, status: http.StatusBadRequest},
		{session: lagApiSessionId{}.String(), stream: "0", origin: testLagOrigin, status: http.StatusNotFound},
		// the load is in its download: the stream waits for the upload, then gives up
		{session: session.id.String(), stream: "0", origin: testLagOrigin, status: http.StatusConflict},
	} {
		if status := up(refusal.session, refusal.stream, refusal.origin); status != refusal.status {
			t.Errorf("up %s %s from %q: %d, expected %d", refusal.session, refusal.stream, refusal.origin, status, refusal.status)
		}
	}
	uploadPhase := fixture.directPhase(session, lagApiDirectionUpload, 2*time.Second, 1<<30)
	if status := up(session.id.String(), "2", testLagOrigin); status != http.StatusConflict {
		t.Errorf("upload stream past max: %d", status)
	}
	upload := fixture.openUpload(session, 1)
	if status := up(session.id.String(), "1", testLagOrigin); status != http.StatusConflict {
		t.Errorf("upload stream already open: %d", status)
	}
	upload.close()
	uploadPhase.finish(lagApiPhaseEndClosed)
	if status := up(session.id.String(), "0", testLagOrigin); status != http.StatusConflict {
		t.Errorf("upload after the phase: %d", status)
	}
}

// M5: the window opens at the phase's first byte, so connection setup does not
// count, and lasts downloadMs from it, the phase's end. The stream writes on
// past it until the browser closes the stream, whose own window opened later
// (amendment 15), and stops for the close, before the grace runs out: every
// byte it wrote, past the window too, is in the sent series and drew on the
// cap.
func TestLagApiDownloadWindow(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.directSession(4)
	phase := fixture.directPhase(session, lagApiDirectionDownload, 600*time.Millisecond, 1<<40)
	time.Sleep(300 * time.Millisecond)
	if firstByteTime, _, _ := phase.state(); !firstByteTime.IsZero() {
		t.Fatal("the window opened before a byte")
	}
	startTime := time.Now()
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.id), testLagOrigin)
	// the browser reads through its window and on past the extender's, then
	// closes; what the extender had written when its window ended
	buffer := make([]byte, 64*1024)
	n := int64(0)
	endedConsumed := int64(-1)
	for time.Since(startTime) < 1200*time.Millisecond {
		read, err := response.Body.Read(buffer)
		n += int64(read)
		if _, _, end := phase.state(); end != lagApiPhaseRunning && endedConsumed < 0 {
			endedConsumed = int64(phase.consumed())
		}
		if err != nil {
			t.Fatalf("the stream ended %s in: %v", time.Since(startTime), err)
		}
	}
	response.Body.Close()
	waitFor(t, func() bool {
		return !testLagStreamOpen(phase, 0)
	})
	stoppedTime := time.Now()
	firstByteTime, endedTime, end := phase.state()
	if end != lagApiPhaseEndTime || endedTime.Sub(firstByteTime) != 600*time.Millisecond || firstByteTime.Before(startTime) {
		t.Errorf("ended %d after %s", end, endedTime.Sub(firstByteTime))
	}
	if !stoppedTime.Before(endedTime.Add(lagApiPhaseGrace)) {
		t.Errorf("the stream stopped %s past the window, not for the close", stoppedTime.Sub(endedTime))
	}
	if endedConsumed < 0 || phase.consumed() <= uint64(endedConsumed) {
		t.Errorf("wrote %d by the window's end and %d in all", endedConsumed, phase.consumed())
	}
	// the window's three intervals, and the grace's past it, never past the
	// grace
	values := phase.seriesValues()
	if len(values) < 3 || 11 < len(values) {
		t.Errorf("series %v", values)
	}
	total := uint64(0)
	for _, value := range values {
		total += value
	}
	if total < uint64(n) || phase.consumed() != total {
		t.Errorf("counted %d consumed %d, read %d", total, phase.consumed(), n)
	}
}

// A response sink for writeDownload in a bubble whose writes take pace each,
// as a link of 64 KiB per pace would, and which keeps when its last write
// ended. A write that would end past the write deadline is cut there, with
// nothing of it written, as a connection's is.
type lagTestPacedSink struct {
	*lagTestDurationSink
	pace          time.Duration
	lastWriteTime time.Time
}

func (self *lagTestPacedSink) Write(bytes []byte) (int, error) {
	now := time.Now()
	if deadline := self.writeDeadline; !deadline.IsZero() && deadline.Before(now.Add(self.pace)) {
		time.Sleep(deadline.Sub(now))
		return 0, os.ErrDeadlineExceeded
	}
	time.Sleep(self.pace)
	self.lastWriteTime = time.Now()
	return self.lagTestDurationSink.Write(bytes)
}

// M5, M7, amendment 15: a download stream writes through its window and on
// past it, drawing on the cap and the daily budget as it goes, until the
// browser closes it; with no close it stops when lagApiPhaseGrace has run out;
// it stops sooner when the load moves on, or a cap runs out past the window.
// The phase ends at the window's end whatever its streams do after, and every
// byte written, past the window too, is in the sent series.
func TestLagApiDownloadWritesPastWindow(t *testing.T) {
	const duration = time.Second
	// writes end at multiples of 7 ms: none ends on the window's end or the
	// grace's
	const pace = 7 * time.Millisecond
	for _, c := range []struct {
		name string
		// the browser closes the stream this long after the first byte
		closeAfter time.Duration
		// the load moves on this long after the first byte
		movedOnAfter time.Duration
		maxByteCount int64
		// when the last write ends at the latest, from the first byte
		lastWriteBy time.Duration
	}{
		{name: "closed", closeAfter: 1300 * time.Millisecond, maxByteCount: 1 << 40, lastWriteBy: 1300*time.Millisecond + pace},
		// the write under way when the grace runs out is cut
		{name: "grace", maxByteCount: 1 << 40, lastWriteBy: duration + lagApiPhaseGrace},
		{name: "moved on", movedOnAfter: 1300 * time.Millisecond, maxByteCount: 1 << 40, lastWriteBy: 1300*time.Millisecond + pace},
		// 150 writes, ending 1050 ms in: the window's 142 and 8 past it
		{name: "capped", maxByteCount: 150 * lagApiBufferByteCount, lastWriteBy: 150 * pace},
	} {
		synctest.Test(t, func(t *testing.T) {
			api := &extenderLagApi{budget: newLagApiBudget(0)}
			phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
				duration, lagApiInterval, 1, c.maxByteCount, time.Minute, func() {})
			defer phase.finish(lagApiPhaseEndClosed)
			stream, err := newLagApiDownloadKeystream()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := &lagTestPacedSink{lagTestDurationSink: &lagTestDurationSink{header: make(http.Header)}, pace: pace}
			startTime := time.Now()
			done := make(chan struct{})
			go func() {
				defer close(done)
				api.writeDownload(ctx, sink, phase, stream)
			}()
			switch {
			case 0 < c.closeAfter:
				time.Sleep(c.closeAfter)
				cancel()
			case 0 < c.movedOnAfter:
				time.Sleep(c.movedOnAfter)
				phase.finish(lagApiPhaseEndClosed)
			}
			<-done
			firstByteTime, endedTime, end := phase.state()
			if !firstByteTime.Equal(startTime) || end != lagApiPhaseEndTime || endedTime.Sub(firstByteTime) != duration {
				t.Errorf("%s: ended %d after %s", c.name, end, endedTime.Sub(firstByteTime))
			}
			if lastWrite := sink.lastWriteTime.Sub(startTime); lastWrite <= duration || c.lastWriteBy < lastWrite {
				t.Errorf("%s: the last write ended %s in, expected past %s and by %s", c.name, lastWrite, duration, c.lastWriteBy)
			}
			values := phase.seriesValues()
			pastWindow := uint64(0)
			total := uint64(0)
			for index, value := range values {
				if int(duration/lagApiInterval) <= index {
					pastWindow += value
				}
				total += value
			}
			if pastWindow == 0 || total != uint64(sink.byteCount) || phase.consumed() != total ||
				uint64(lagTestDurationBudgetUsed(api.budget)) != total {
				t.Errorf("%s: wrote %d, counted %d, %d past the window, %d drawn on the cap and %d on the budget",
					c.name, sink.byteCount, total, pastWindow, phase.consumed(), lagTestDurationBudgetUsed(api.budget))
			}
			if 0 < c.closeAfter || 0 < c.movedOnAfter {
				if stoppedIndex := int((c.lastWriteBy + lagApiInterval - 1) / lagApiInterval); stoppedIndex < len(values) {
					t.Errorf("%s: written on past the stop: %v", c.name, values)
				}
			}
			if c.maxByteCount < 1<<40 && phase.consumed() != uint64(c.maxByteCount) {
				t.Errorf("%s: consumed %d of a cap of %d", c.name, phase.consumed(), c.maxByteCount)
			}
			if !sink.writeDeadline.Equal(firstByteTime.Add(duration + lagApiPhaseGrace)) {
				t.Errorf("%s: write deadline %s", c.name, sink.writeDeadline.Sub(firstByteTime))
			}
		})
	}
}

// M3: the budget renews at UTC midnight, and returned bytes go back only on
// the day they were taken.
func TestLagApiBudget(t *testing.T) {
	budget := newLagApiBudget(1000)
	day := time.Date(2026, 10, 12, 23, 59, 59, 0, time.UTC)
	if granted := budget.take(day, 600); granted != 600 {
		t.Errorf("granted %d", granted)
	}
	if granted := budget.take(day, 600); granted != 400 {
		t.Errorf("granted %d past the budget", granted)
	}
	if !budget.exhausted(day) {
		t.Error("not exhausted")
	}
	budget.give(day, 100)
	if budget.exhausted(day) || budget.take(day, 1000) != 100 {
		t.Error("returned bytes")
	}
	nextDay := day.Add(2 * time.Second)
	if budget.exhausted(nextDay) {
		t.Error("exhausted the next day")
	}
	if renewTime := budget.renewTime(day); !renewTime.Equal(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("renews at %s", renewTime)
	}
	budget.give(day, 500)
	if granted := budget.take(nextDay, 1000); granted != 1000 {
		t.Errorf("granted %d the next day", granted)
	}
}

// The heap in use after a collection, and what was allocated so far.
func testLagMemory() (uint64, uint64) {
	runtime.GC()
	memStats := runtime.MemStats{}
	runtime.ReadMemStats(&memStats)
	return memStats.HeapInuse, memStats.TotalAlloc
}

// M7: a 1 GiB download moves through one buffer: what the whole exchange
// allocates, the client's side included, stays a small fraction of the bytes,
// and the heap does not grow with the stream.
func TestLagApiDownloadFlatMemory(t *testing.T) {
	if testing.Short() || testLagRaceEnabled {
		t.Skip("a 1 GiB stream")
	}
	const byteCount = 1 << 30
	fixture := newLagApiFixture(t, nil)
	session := fixture.directSession(1)
	phase := fixture.directPhase(session, lagApiDirectionDownload, 5*time.Minute, byteCount)
	request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/lag/v1/down?session=%s&stream=0", testLagName, session.id), nil)
	request.Header.Set("Origin", testLagOrigin)
	client := fixture.httpClient()
	client.Timeout = 5 * time.Minute

	heapBefore, allocatedBefore := testLagMemory()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256*1024)
	n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, response.Body, buffer)
	response.Body.Close()
	heapAfter, allocatedAfter := testLagMemory()
	if err != nil || n != byteCount {
		t.Fatalf("downloaded %d: %v", n, err)
	}
	if _, _, end := phase.state(); end != lagApiPhaseEndCap {
		t.Errorf("ended %d", end)
	}
	allocated := allocatedAfter - allocatedBefore
	t.Logf("1 GiB down: allocated %d bytes, heap %d -> %d", allocated, heapBefore, heapAfter)
	if 32*1024*1024 < allocated {
		t.Errorf("allocated %d bytes for a 1 GiB stream", allocated)
	}
	if heapBefore+16*1024*1024 < heapAfter {
		t.Errorf("heap grew from %d to %d", heapBefore, heapAfter)
	}
}

// M7: a 1 GiB upload is read and verified through one data buffer and one
// keystream buffer.
func TestLagApiUploadFlatMemory(t *testing.T) {
	if testing.Short() || testLagRaceEnabled {
		t.Skip("a 1 GiB stream")
	}
	const byteCount = 1 << 30
	fixture := newLagApiFixture(t, nil)
	fixture.api.exactVerification = true
	session := fixture.directSession(1)
	phase := fixture.directPhase(session, lagApiDirectionUpload, 5*time.Minute, byteCount)
	upload := fixture.openUpload(session, 0)
	stream := testLagUploadStream(t, session.uploadKey[:], 0)
	message := make([]byte, lagApiBufferByteCount)

	heapBefore, allocatedBefore := testLagMemory()
	for sent := 0; sent < byteCount; sent += len(message) {
		clear(message)
		stream.XORKeyStream(message, message)
		if err := upload.send(websocket.BinaryMessage, message); err != nil {
			t.Fatalf("sent %d: %v", sent, err)
		}
	}
	if code := upload.waitClosed(time.Minute); code != websocket.CloseNormalClosure {
		t.Errorf("closed %d", code)
	}
	heapAfter, allocatedAfter := testLagMemory()
	// every byte was verified and drawn from the cap; the series itself only
	// spans the intervals a report may carry
	if consumed := phase.consumed(); consumed != byteCount {
		t.Errorf("verified %d", consumed)
	}
	if _, _, end := phase.state(); end != lagApiPhaseEndCap {
		t.Errorf("ended %d", end)
	}
	allocated := allocatedAfter - allocatedBefore
	t.Logf("1 GiB up: allocated %d bytes, heap %d -> %d", allocated, heapBefore, heapAfter)
	if 32*1024*1024 < allocated {
		t.Errorf("allocated %d bytes for a 1 GiB stream", allocated)
	}
	if heapBefore+16*1024*1024 < heapAfter {
		t.Errorf("heap grew from %d to %d", heapBefore, heapAfter)
	}
}
