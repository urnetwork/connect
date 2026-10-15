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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net/http"
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
// stream is counted whole, a short last message included; a changed byte, a
// text message or a message after a short one each end the phase as a
// mismatch and close the stream with 4400. What arrived before the failure
// stays counted -- for a changed byte, the reads of its message before the
// one that holds it -- and nothing from the failure on is.
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
		return phase.total() == 8*65536+1000
	})
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
		if counted := phase.total(); counted < expected[0] || expected[1] < counted {
			t.Errorf("%s: counted %d, expected %d to %d", name, counted, expected[0], expected[1])
		}
	}
}

// An upload phase outside any session whose 5 s window opened at startTime,
// with the ticket's cap and the daily budget given (0 is none).
func testLagUploadPhase(startTime time.Time, maxStreamCount int, maxByteCount int64, budgetByteCount int64) *lagApiPhase {
	api := &extenderLagApi{budget: newLagApiBudget(budgetByteCount)}
	phase := newLagApiPhase(api, context.Background(), lagApiDirectionUpload,
		5*time.Second, lagApiInterval, maxStreamCount, maxByteCount, time.Minute, func() {})
	phase.window(startTime)
	return phase
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

// Amendment 14, rule S: each read of an upload stream counts as having arrived
// evenly since the stream's previous read -- the window's first byte for its
// first -- but never from more than lagSpreadMax before it, split at the
// interval boundaries into whole bytes that sum to the read. The window's
// first read, and a read at its stream's previous read's time, count whole
// where they arrived. A read at or past the window's end counts nothing and
// ends the phase by its time, and nothing counts after. Only counted reads
// draw on the cap and move the latest arrival progress reports (M4).
func TestLagApiUploadSpread(t *testing.T) {
	startTime := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time {
		return startTime.Add(time.Duration(ms) * time.Millisecond)
	}
	phase := testLagUploadPhase(startTime, 4, 1024*1024*1024, 0)
	defer phase.finish(lagApiPhaseEndClosed)
	expectSeries := func(step string, expected ...uint64) {
		t.Helper()
		if values := phase.seriesValues(); !slices.Equal(values, expected) {
			t.Fatalf("%s: series %v, expected %v", step, values, expected)
		}
	}

	// the window's first read, at its first byte, counts whole there; the
	// stream's next spreads back to it, within interval 0
	phase.receive(0, startTime, 1000)
	phase.receive(0, at(200), 2000)
	expectSeries("within an interval", 3000)
	// 50 ms either side of the 250 ms boundary
	phase.receive(0, at(300), 16384)
	expectSeries("across a boundary", 3000+8192, 8192)
	// a stream's first read spreads back to the window's first byte, not to
	// another stream's read: 1001 bytes over 750 ms, a third in each
	// interval, rounded at each cut so the pieces sum to the read
	phase.receive(1, at(750), 1001)
	expectSeries("a stream's first read", 11192+334, 8192+333, 334)
	// a read at its stream's previous read's time counts whole there: on a
	// boundary, in the interval it starts
	phase.receive(1, at(750), 500)
	expectSeries("no time since the stream's previous read", 11526, 8525, 334, 500)
	// 2 s after its stream's previous read, a read spreads over the last
	// lagSpreadMax only, 1300 to 2300 ms
	phase.receive(0, at(2300), 4000)
	expectSeries("after a stall", 11526, 8525, 334, 500, 0, 800, 1000, 1000, 1000, 200)
	// a read that arrived before the latest counted one counts as well, and
	// progress keeps the latest arrival
	phase.receive(2, at(2000), 100)
	expectSeries("an earlier arrival", 11526, 8525, 334, 500, 25, 825, 1025, 1025, 1000, 200)
	if byteCount, countedTime := phase.progress(); byteCount != 24985 || !countedTime.Equal(at(2300)) {
		t.Errorf("progress %d at %s", byteCount, countedTime.Sub(startTime))
	}
	// the window's last interval takes its share of a read just before the
	// end; a read at the end counts nothing and ends the phase by its time,
	// after which a read that arrived inside the window counts nothing either
	phase.receive(0, at(4900), 1000)
	phase.receive(1, at(5000), 7777)
	phase.receive(2, at(4950), 100)
	expectSeries("the window's end",
		11526, 8525, 334, 500, 25, 825, 1025, 1025, 1000, 200, 0, 0, 0, 0, 0, 100, 250, 250, 250, 150)
	if _, endedTime, end := phase.state(); end != lagApiPhaseEndTime || !endedTime.Equal(at(5000)) {
		t.Errorf("ended %d at %s", end, endedTime.Sub(startTime))
	}
	if byteCount, countedTime := phase.progress(); byteCount != 25985 || !countedTime.Equal(at(4900)) ||
		phase.consumed() != 25985 {
		t.Errorf("progress %d at %s, %d drawn on the cap", byteCount, countedTime.Sub(startTime), phase.consumed())
	}
}

// Amendment 14: a spread read's pieces are whole bytes that sum to the read,
// each within a byte of its interval's exact share of the span; a span of no
// length puts the read whole in the interval where it ends.
func TestLagApiSeriesAddSpread(t *testing.T) {
	startTime := time.Unix(1_700_000_000, 0)
	random := mathrand.New(mathrand.NewPCG(14, 0))
	for i := 0; i < 10000; i += 1 {
		series := newLagApiSeries(startTime, lagApiInterval, 7*time.Second)
		spanStart := time.Duration(random.Int64N(int64(5 * time.Second)))
		spanEnd := spanStart + time.Duration(random.Int64N(int64(lagSpreadMax)+1))
		if i%10 == 0 {
			spanEnd = spanStart
		}
		byteCount := 1 + random.IntN(64*1024)
		series.addSpread(startTime.Add(spanStart), startTime.Add(spanEnd), byteCount)
		total := uint64(0)
		for index, value := range series.values(0) {
			total += value
			share := 0.0
			if spanEnd == spanStart {
				if index == int(spanEnd/lagApiInterval) {
					share = float64(byteCount)
				}
			} else {
				overlap := min(spanEnd, time.Duration(index+1)*lagApiInterval) - max(spanStart, time.Duration(index)*lagApiInterval)
				share = float64(byteCount) * float64(max(0, overlap)) / float64(spanEnd-spanStart)
			}
			// two roundings of half a byte at most, and the float error
			if 1+1e-6 < math.Abs(float64(value)-share) {
				t.Fatalf("%d bytes over %s to %s: interval %d took %d of a %.2f share", byteCount, spanStart, spanEnd, index, value, share)
			}
		}
		if total != uint64(byteCount) {
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

		// bytes for 600 ms, then nothing until the window runs out
		phase, startTime := newPhase()
		phase.receive(0, startTime, 1000)
		phase.receive(0, startTime.Add(600*time.Millisecond), 6000)
		if values := phase.windowValues(); !slices.Equal(values, []uint64{3500, 2500, 1000}) {
			t.Errorf("before the end: %v", values)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		expected := append([]uint64{3500, 2500, 1000}, make([]uint64, 17)...)
		if _, _, end := phase.state(); end != lagApiPhaseEndTime || !slices.Equal(phase.windowValues(), expected) {
			t.Errorf("ended %d with %v", end, phase.windowValues())
		}
		if values := phase.seriesValues(); !slices.Equal(values, []uint64{3500, 2500, 1000}) {
			t.Errorf("trimmed %v", values)
		}

		// closed 1.1 s in: five intervals, through the one the end falls in
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
// 2.1 Mbps -- and in full. Exact and sampled checking count the same way.
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
			return phase.total() == lagApiBufferByteCount
		})
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
// not seen, and one in a chosen message ends the phase. Every read counts as
// it arrives (amendment 14), so the chosen message that failed was counted,
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
		return phase.total() == 6*65536
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
	if phase.total() != 5*65536 {
		t.Errorf("counted %d", phase.total())
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
// count, and lasts downloadMs from it; the streams then end.
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
	n, _ := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	elapsed := time.Since(startTime)
	waitFor(t, func() bool {
		_, _, end := phase.state()
		return end != lagApiPhaseRunning
	})
	firstByteTime, endedTime, end := phase.state()
	if end != lagApiPhaseEndTime || endedTime.Sub(firstByteTime) != 600*time.Millisecond {
		t.Errorf("ended %d after %s", end, endedTime.Sub(firstByteTime))
	}
	if firstByteTime.Before(startTime) || elapsed < 600*time.Millisecond || 3*time.Second < elapsed || n == 0 {
		t.Errorf("downloaded %d in %s", n, elapsed)
	}
	// three intervals of window, and room for a write that finished late
	values := phase.seriesValues()
	if len(values) < 2 || 11 < len(values) {
		t.Errorf("series %v", values)
	}
	total := uint64(0)
	for _, value := range values {
		total += value
	}
	if total != uint64(n) || phase.consumed() != uint64(n) {
		t.Errorf("counted %d consumed %d of %d", total, phase.consumed(), n)
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
