package extender

// Sampled upload credit retains verification ownership across the measurement
// end, counting timers, read cancellation and forced settlement. All ordering
// comes from explicit barriers and the synthetic clock, without network I/O.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// Seeds one stream with two checked messages at 0 and 4 seconds. The returned
// short message follows their keystream and has not been read or sampled yet.
func testLagSampledUpload(t *testing.T, sampleMessage func() bool) (*lagApiPhase, *lagApiUploadStream, time.Time, []byte) {
	t.Helper()
	startTime := time.Now()
	phase := testLagUploadPhase(startTime, 1, 1024*1024*1024, 0)
	uploadKey := [16]byte{1, 2, 3, 4}
	stream, err := newLagApiUploadStream(phase, uploadKey, 0, false, sampleMessage)
	if err != nil {
		t.Fatal(err)
	}
	messages := testLagUploadMessages(t, uploadKey, 0, lagApiBufferByteCount, lagApiBufferByteCount, 16384)
	for index, message := range messages[:2] {
		time.Sleep(time.Until(startTime.Add(time.Duration(index) * 4 * time.Second)))
		if err := stream.readMessage(websocket.BinaryMessage, bytes.NewReader(message), time.Now); err != nil {
			t.Fatal(err)
		}
	}
	return phase, stream, startTime, messages[2]
}

// Builds the production report body from the real upload phase, without a
// control connection or report submission. Its empty download moved no bytes.
func testLagSampledUploadReport(phase *lagApiPhase) *protocol.LagReportBody {
	firstByteTime, _, _ := phase.state()
	session := &lagApiSession{
		api:       phase.api,
		startTime: firstByteTime,
		load: &lagApiLoad{
			requestTime: firstByteTime,
			download:    &lagApiPhase{},
			upload:      phase,
		},
	}
	return session.reportBody(time.Now())
}

// A selected short message arriving just after the window still contributes
// its span before the end; a failed comparison invalidates that credit.
func TestLagApiUploadSampledMismatchAcrossPhaseEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		phase, startTime, send := testLagShortUpload(t, false, func() bool { return true })
		defer phase.finish(lagApiPhaseEndClosed)
		for second := 0; second < 5; second += 1 {
			for streamIndex := 0; streamIndex < 2; streamIndex += 1 {
				send(streamIndex, time.Duration(second)*time.Second, lagApiBufferByteCount, false)
			}
		}
		send(1, 4500*time.Millisecond, 16384, false)
		send(0, 5050*time.Millisecond, 16384, true)
		_, endedTime, end := phase.state()
		if end != lagApiPhaseEndMismatch || !phase.windowFinal() || !endedTime.Equal(startTime.Add(5*time.Second)) {
			t.Errorf("failed selected sample ended %d at %s, final %t", end, endedTime.Sub(startTime), phase.windowFinal())
		}
		if (&lagApiLoad{upload: phase}).uploadVerified() || testLagSampledUploadReport(phase).UploadVerified {
			t.Error("failed selected sample retained verified upload credit across phase end")
		}
		if phase.total() != 8*65536+16384+15565 || phase.consumed() != 10*65536+2*16384 {
			t.Errorf("mismatch changed byte accounting: counted %d, consumed %d", phase.total(), phase.consumed())
		}
	})
}

// A comparison may cross the window while its timer is still runnable. Its
// mismatch keeps the measurement's end, independent of that timer's ordering.
func TestLagApiUploadSampledMismatchBeforeDelayedTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sampleCount := 0
		var comparisonTime time.Time
		phase, stream, startTime, message := testLagSampledUpload(t, func() bool {
			sampleCount += 1
			if sampleCount == 3 {
				comparisonTime = comparisonTime.Add(150 * time.Millisecond)
			}
			return true
		})
		defer phase.finish(lagApiPhaseEndClosed)
		message[len(message)-1] ^= 1
		comparisonTime = startTime.Add(4900 * time.Millisecond)
		if err := stream.readMessage(websocket.BinaryMessage, bytes.NewReader(message), func() time.Time { return comparisonTime }); err != nil {
			t.Fatal(err)
		}
		_, endedTime, end := phase.state()
		if end != lagApiPhaseEndMismatch || !endedTime.Equal(startTime.Add(5*time.Second)) || testLagSampledUploadReport(phase).UploadVerified {
			t.Errorf("delayed timer: ended %d at %s, verified %t", end, endedTime.Sub(startTime), testLagSampledUploadReport(phase).UploadVerified)
		}
	})
}

// The comparison barrier holds a fully received message across both the
// window and final-counting timers. Neither successful final progress nor a
// verified report may escape before the comparison; healthy metrics persist.
func TestLagApiUploadSampledComparisonOwnsSettlement(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			sampleCount := 0
			phase, stream, startTime, message := testLagSampledUpload(t, func() bool {
				sampleCount += 1
				if sampleCount == 3 {
					close(entered)
					<-release
				}
				return true
			})
			defer phase.finish(lagApiPhaseEndClosed)
			if corrupt {
				message[len(message)-1] ^= 1
			}
			time.Sleep(time.Until(startTime.Add(4900 * time.Millisecond)))
			done := make(chan error, 1)
			go func() {
				done <- stream.readMessage(websocket.BinaryMessage, bytes.NewReader(message), time.Now)
			}()
			<-entered
			time.Sleep(time.Until(startTime.Add(6100 * time.Millisecond)))
			synctest.Wait()
			if phase.windowFinal() || (&lagApiLoad{upload: phase}).uploadVerified() {
				t.Errorf("corrupt %t: pending sample finalized before comparison", corrupt)
			}
			if _, _, _, ok := phase.progressReport(true); ok {
				t.Errorf("corrupt %t: final progress escaped before comparison", corrupt)
			}
			if testLagSampledUploadReport(phase).UploadVerified {
				t.Errorf("corrupt %t: report verified bytes before comparison", corrupt)
			}
			before := phase.windowValues()
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			expectedEnd := lagApiPhaseEndTime
			if corrupt {
				expectedEnd = lagApiPhaseEndMismatch
			}
			_, endedTime, end := phase.state()
			if end != expectedEnd || !phase.windowFinal() || !endedTime.Equal(startTime.Add(5*time.Second)) {
				t.Errorf("corrupt %t: ended %d at %s, final %t", corrupt, end, endedTime.Sub(startTime), phase.windowFinal())
			}
			body := testLagSampledUploadReport(phase)
			if body.UploadVerified == corrupt || (&lagApiLoad{upload: phase}).uploadVerified() == corrupt {
				t.Errorf("corrupt %t: verification result %t", corrupt, body.UploadVerified)
			}
			if !slices.Equal(before, body.UploadBytes) || body.UploadMs != 5000 || len(body.UploadBytes) != 20 || body.UploadStreams != 1 {
				t.Errorf("corrupt %t: report changed the settled metrics: %v", corrupt, body)
			}
			if total, ms, at, ok := phase.progressReport(true); !ok || total != 65536+16384 || ms != 5000 || at != 4900 {
				t.Errorf("corrupt %t: final progress %d at %d ms, arrival %f, sent %t", corrupt, total, ms, at, ok)
			}
			budgetUsed := func() int64 {
				phase.api.budget.stateLock.Lock()
				defer phase.api.budget.stateLock.Unlock()
				return phase.api.budget.used
			}()
			if phase.consumed() != 2*65536+16384 || budgetUsed != 2*65536+16384 {
				t.Errorf("corrupt %t: cap %d and budget %d", corrupt, phase.consumed(), budgetUsed)
			}
		})
	}
}

// Delivers a prefix, then holds the rest of one message at an explicit read
// barrier. A configured error abandons the message instead of completing it.
type lagTestSampledBarrierReader struct {
	prefix  []byte
	rest    []byte
	entered chan struct{}
	release chan struct{}
	err     error
}

// The second read proves the first prefix has already passed receive.
func (self *lagTestSampledBarrierReader) Read(buffer []byte) (int, error) {
	if len(self.prefix) != 0 {
		n := copy(buffer, self.prefix)
		self.prefix = self.prefix[n:]
		return n, nil
	}
	if self.entered != nil {
		close(self.entered)
		self.entered = nil
		<-self.release
	}
	if self.err != nil {
		return 0, self.err
	}
	if len(self.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(buffer, self.rest)
	self.rest = self.rest[n:]
	return n, nil
}

// A prefix credited before the end stays owned while its remaining bytes
// arrive after counting is final. Cancellation cannot turn that prefix into
// verified credit; a complete healthy sample can, without counting late bytes.
func TestLagApiUploadSampledPrefixOwnsSettlement(t *testing.T) {
	for _, c := range []struct {
		name    string
		corrupt bool
		err     error
	}{
		{name: "healthy"},
		{name: "corrupt", corrupt: true},
		{name: "canceled", err: context.Canceled},
	} {
		synctest.Test(t, func(t *testing.T) {
			phase, stream, startTime, message := testLagSampledUpload(t, func() bool { return true })
			defer phase.finish(lagApiPhaseEndClosed)
			if c.corrupt {
				message[len(message)-1] ^= 1
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			reader := &lagTestSampledBarrierReader{
				prefix: message[:8192], rest: message[8192:], entered: entered, release: release, err: c.err,
			}
			time.Sleep(time.Until(startTime.Add(4900 * time.Millisecond)))
			done := make(chan error, 1)
			go func() { done <- stream.readMessage(websocket.BinaryMessage, reader, time.Now) }()
			<-entered
			time.Sleep(time.Until(startTime.Add(6100 * time.Millisecond)))
			synctest.Wait()
			if phase.windowFinal() || testLagSampledUploadReport(phase).UploadVerified {
				t.Errorf("%s: credited prefix settled before the message", c.name)
			}
			close(release)
			if err := <-done; !errors.Is(err, c.err) {
				t.Fatalf("%s: read error %v, expected %v", c.name, err, c.err)
			}
			phase.closeStream(0)
			_, _, end := phase.state()
			expectedEnd := lagApiPhaseEndTime
			if c.corrupt {
				expectedEnd = lagApiPhaseEndMismatch
			}
			if end != expectedEnd || !phase.windowFinal() {
				t.Errorf("%s: ended %d, final %t", c.name, end, phase.windowFinal())
			}
			verified := !c.corrupt && c.err == nil
			if body := testLagSampledUploadReport(phase); body.UploadVerified != verified || phase.total() != 65536+8192 || phase.consumed() != 2*65536+8192 {
				t.Errorf("%s: verified %t, counted %d, consumed %d", c.name, body.UploadVerified, phase.total(), phase.consumed())
			}
		})
	}
}

// Closing or capping while a comparison is held never waits on it and never
// claims its bytes verified. A later healthy comparison cannot upgrade a
// report already made unverified by that abandonment.
func TestLagApiUploadSampledForcedSettlement(t *testing.T) {
	for _, capped := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			sampleCount := 0
			phase, stream, startTime, message := testLagSampledUpload(t, func() bool {
				sampleCount += 1
				if sampleCount == 3 {
					close(entered)
					<-release
				}
				return true
			})
			time.Sleep(time.Until(startTime.Add(4900 * time.Millisecond)))
			done := make(chan error, 1)
			go func() { done <- stream.readMessage(websocket.BinaryMessage, bytes.NewReader(message), time.Now) }()
			<-entered
			if capped {
				phase.finish(lagApiPhaseEndCap)
			} else {
				time.Sleep(time.Until(startTime.Add(5100 * time.Millisecond)))
			}
			phase.finish(lagApiPhaseEndClosed)
			before := testLagSampledUploadReport(phase)
			if !phase.windowFinal() || before.UploadVerified {
				t.Errorf("capped %t: forced settlement final %t, verified %t", capped, phase.windowFinal(), before.UploadVerified)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			after := testLagSampledUploadReport(phase)
			if after.UploadVerified || !slices.Equal(before.UploadBytes, after.UploadBytes) || after.UploadMs != before.UploadMs {
				t.Errorf("capped %t: late comparison changed forced settlement: before %v, after %v", capped, before, after)
			}
		})
	}
}

// An empty in-flight message owns no credit. Closing it after the window
// keeps earlier completed samples verified, including after its read fails.
func TestLagApiUploadSampledEmptyReadDoesNotHoldSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		phase, stream, startTime, _ := testLagSampledUpload(t, func() bool { return true })
		entered := make(chan struct{})
		release := make(chan struct{})
		reader := &lagTestSampledBarrierReader{entered: entered, release: release, err: context.Canceled}
		done := make(chan error, 1)
		go func() { done <- stream.readMessage(websocket.BinaryMessage, reader, time.Now) }()
		<-entered
		time.Sleep(time.Until(startTime.Add(6100 * time.Millisecond)))
		synctest.Wait()
		phase.finish(lagApiPhaseEndClosed)
		if !phase.windowFinal() || !testLagSampledUploadReport(phase).UploadVerified {
			t.Error("empty read held or invalidated completed sample credit")
		}
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if !testLagSampledUploadReport(phase).UploadVerified || phase.total() != 65536 || phase.consumed() != 2*65536 {
			t.Errorf("empty cancellation changed verified credit: counted %d, consumed %d", phase.total(), phase.consumed())
		}
	})
}

// Earlier corruption still ends the phase immediately. A healthy selected or
// unselected message at the late edge preserves the existing throughput,
// while exact verification rejects a late corrupt read before it counts.
func TestLagApiUploadSampledAdjacentVerification(t *testing.T) {
	for _, c := range []struct {
		name       string
		exact      bool
		chosen     bool
		corrupt    bool
		arrived    time.Duration
		end        lagApiPhaseEnd
		verified   bool
		lateCredit uint64
	}{
		{name: "earlier corruption", chosen: true, corrupt: true, arrived: 4900 * time.Millisecond, end: lagApiPhaseEndMismatch, lateCredit: 16384},
		{name: "healthy selected", chosen: true, arrived: 5050 * time.Millisecond, end: lagApiPhaseEndTime, verified: true, lateCredit: 15565},
		{name: "unselected corruption", corrupt: true, arrived: 5050 * time.Millisecond, end: lagApiPhaseEndTime, verified: true, lateCredit: 15565},
		{name: "exact late corruption", exact: true, corrupt: true, arrived: 5050 * time.Millisecond, end: lagApiPhaseEndTime, verified: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			phase, _, send := testLagShortUpload(t, c.exact, func() bool { return c.chosen })
			defer phase.finish(lagApiPhaseEndClosed)
			for second := 0; second < 5; second += 1 {
				for streamIndex := 0; streamIndex < 2; streamIndex += 1 {
					send(streamIndex, time.Duration(second)*time.Second, lagApiBufferByteCount, false)
				}
			}
			send(1, 4500*time.Millisecond, 16384, false)
			send(0, c.arrived, 16384, c.corrupt)
			_, _, end := phase.state()
			body := testLagSampledUploadReport(phase)
			if end != c.end || !phase.windowFinal() || body.UploadVerified != c.verified || phase.total() != 8*65536+16384+c.lateCredit {
				t.Errorf("%s: ended %d, final %t, verified %t, counted %d", c.name, end, phase.windowFinal(), body.UploadVerified, phase.total())
			}
		})
	}
}
