package extender

// Tests of the measurement streams (lag.dev/PLAN.md M5 to M7, T1): the
// upload's keystream verification, exact and sampled, the stream refusals, the
// window from the first byte, the daily budget, and flat memory over a 1 GiB
// stream each way.

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
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
	session, err := newLagApiSession(self.api, nil, body, nil)
	if err != nil {
		self.t.Fatal(err)
	}
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
	dialer := &websocket.Dialer{
		NetDialContext:   self.dialContext,
		TLSClientConfig:  self.httpClient().Transport.(*http.Transport).TLSClientConfig.Clone(),
		HandshakeTimeout: 5 * time.Second,
		// one frame per message, as a browser sends it
		WriteBufferSize: lagApiBufferByteCount + 16,
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

// M6, M7: with AES instructions every message is checked; the exact stream is
// counted whole, a short last message included; a changed byte, a text message
// or a message after a short one each end the phase as a mismatch, close the
// stream with 4400, and count nothing of what failed.
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
		expected := map[string]uint64{
			"changed byte":      3 * 65536,
			"text message":      3 * 65536,
			"after a short one": 2*65536 + 512,
			"wrong stream":      0,
		}[name]
		if phase.total() != expected {
			t.Errorf("%s: counted %d, expected %d", name, phase.total(), expected)
		}
	}
}

// M6: without AES instructions one message in 16 is checked, chosen after it
// arrives, at its own offset; a change in a message that was not chosen is
// not seen, and one in a chosen message ends the phase.
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
	if phase.total() != 4*65536 {
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
	if testing.Short() {
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
	if testing.Short() {
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
