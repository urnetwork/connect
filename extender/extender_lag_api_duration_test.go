package extender

// Regression controls for the duration fixture, not a change to the API's cap
// semantics. The real writer reserves and refunds bytes under a fake clock;
// the real TLS load separately checks finite upload ownership through close.

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// A payload-discarding response sink. Its one writer owns the counters; tests
// inspect them only after synctest quiescence or the writer's completion join.
type lagTestDurationSink struct {
	header           http.Header
	byteCount        int64
	partialByteCount int
	writeError       error
	readDeadline     time.Time
	writeDeadline    time.Time
}

// Returns the one response header map.
func (self *lagTestDurationSink) Header() http.Header {
	return self.header
}

// The payload-only writer control does not need a serialized status line.
func (self *lagTestDurationSink) WriteHeader(status int) {}

// Counts accepted bytes without retaining their contents.
func (self *lagTestDurationSink) Write(bytes []byte) (int, error) {
	n := len(bytes)
	if self.writeError != nil {
		n = min(n, self.partialByteCount)
	}
	self.byteCount += int64(n)
	return n, self.writeError
}

// Records the controller operation without introducing a second clock.
func (self *lagTestDurationSink) SetReadDeadline(deadline time.Time) error {
	self.readDeadline = deadline
	return nil
}

// Records the same deadline the actual HTTP response would receive.
func (self *lagTestDurationSink) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadline = deadline
	return nil
}

// Uses the budget's real lock to order the snapshot before a later refund.
// synctest.Wait orders earlier work, not this read against future timer work.
func lagTestDurationBudgetUsed(budget *lagApiBudget) int64 {
	budget.stateLock.Lock()
	defer budget.stateLock.Unlock()
	return budget.used
}

// This is the fixture RED: the explicit pass-through control consumes a small
// phase cap while fake time is unchanged. The bounded fixture must instead
// retain one reservation at its write barrier and finish at the actual window.
func TestLagApiDurationFixtureKeepsDownloadTimeWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const duration = 500 * time.Millisecond
		const capByteCount = int64(4 * lagApiBufferByteCount)
		api := &extenderLagApi{budget: newLagApiBudget(0)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
			duration, lagApiInterval, 1, capByteCount, time.Second, func() {})
		defer phase.finish(lagApiPhaseEndClosed)
		stream, err := newLagApiDownloadKeystream()
		if err != nil {
			t.Fatal(err)
		}
		sink := &lagTestDurationSink{header: make(http.Header)}
		writer := newLagTestDurationWriter(sink, context.Background(), func() *lagApiPhase { return phase })
		done := make(chan struct{})
		go func() {
			defer close(done)
			api.writeDownload(context.Background(), writer, phase, stream)
		}()
		synctest.Wait()
		first, _, beforeEnd := phase.state()
		beforeWritten := sink.byteCount
		beforeRemaining := phase.capRemaining.Load()
		beforeBudgeted := lagTestDurationBudgetUsed(api.budget)

		// This advances the bubble's clock, not a wall-clock scheduler guess.
		time.Sleep(duration)
		<-done
		_, ended, end := phase.state()
		if beforeEnd != lagApiPhaseRunning || beforeWritten != testLagDurationByteCount ||
			beforeRemaining != lagApiBufferByteCount || beforeBudgeted != 3*lagApiBufferByteCount {
			t.Errorf("duration fixture exhausted or escaped the bounded window: end=%d written=%d remaining=%d budgeted=%d",
				beforeEnd, beforeWritten, beforeRemaining, beforeBudgeted)
		}
		if end != lagApiPhaseEndTime || ended.Sub(first) != duration {
			t.Errorf("duration fixture ended %d after %s, want time after %s", end, ended.Sub(first), duration)
		}
		if phase.total() != testLagDurationByteCount || phase.consumed() != testLagDurationByteCount ||
			phase.capRemaining.Load() != capByteCount-testLagDurationByteCount || lagTestDurationBudgetUsed(api.budget) != testLagDurationByteCount {
			t.Errorf("unwritten reservation was not refunded: total=%d consumed=%d remaining=%d budget=%d",
				phase.total(), phase.consumed(), phase.capRemaining.Load(), lagTestDurationBudgetUsed(api.budget))
		}
		if !sink.readDeadline.IsZero() || !sink.writeDeadline.Equal(first.Add(duration+lagApiPhaseGrace)) {
			t.Errorf("response-controller deadlines were changed: read=%s write=%s", sink.readDeadline, sink.writeDeadline)
		}
	})
}

// A smaller signed cap or daily allowance still wins before the duration.
// The exact-boundary and partial-frame cases keep the true admission contract.
func TestLagApiDurationFixturePreservesByteCaps(t *testing.T) {
	cases := []struct {
		name        string
		ticketBytes int64
		dailyBytes  int64
		wantBytes   int64
	}{
		{name: "ticket", ticketBytes: lagApiBufferByteCount, wantBytes: lagApiBufferByteCount},
		{name: "exact fixture bound", ticketBytes: testLagDurationByteCount, wantBytes: testLagDurationByteCount},
		{name: "partial frame", ticketBytes: lagApiBufferByteCount + 17, wantBytes: lagApiBufferByteCount + 17},
		{name: "daily", ticketBytes: 4 * lagApiBufferByteCount, dailyBytes: lagApiBufferByteCount, wantBytes: lagApiBufferByteCount},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			api := &extenderLagApi{budget: newLagApiBudget(c.dailyBytes)}
			phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
				time.Second, lagApiInterval, 1, c.ticketBytes, 2*time.Second, func() {})
			defer phase.finish(lagApiPhaseEndClosed)
			stream, err := newLagApiDownloadKeystream()
			if err != nil {
				t.Fatal(err)
			}
			sink := &lagTestDurationSink{header: make(http.Header)}
			writer := newLagTestDurationWriter(sink, context.Background(), func() *lagApiPhase { return phase })
			start := time.Now()
			api.writeDownload(context.Background(), writer, phase, stream)
			first, ended, end := phase.state()
			if end != lagApiPhaseEndCap || !first.Equal(start) || !ended.Equal(start) ||
				sink.byteCount != c.wantBytes || phase.consumed() != uint64(c.wantBytes) ||
				phase.total() != uint64(c.wantBytes) || lagTestDurationBudgetUsed(api.budget) != c.wantBytes ||
				phase.capRemaining.Load() != c.ticketBytes-c.wantBytes {
				t.Errorf("%s: cap/accounting changed: end=%d first=%s ended=%s written=%d consumed=%d total=%d budget=%d remaining=%d",
					c.name, end, first, ended, sink.byteCount, phase.consumed(), phase.total(), lagTestDurationBudgetUsed(api.budget), phase.capRemaining.Load())
			}
			if (0 < c.dailyBytes) != api.budget.exhausted(start) {
				t.Errorf("%s: daily exhaustion changed", c.name)
			}
		})
	}
}

// Request cancellation releases the held writer without falsely ending the
// phase by time or retaining the never-written third frame's reservation.
func TestLagApiDurationFixtureReturnsCanceledReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const capByteCount = int64(4 * lagApiBufferByteCount)
		api := &extenderLagApi{budget: newLagApiBudget(0)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
			time.Second, lagApiInterval, 1, capByteCount, 2*time.Second, func() {})
		defer phase.finish(lagApiPhaseEndClosed)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, err := newLagApiDownloadKeystream()
		if err != nil {
			t.Fatal(err)
		}
		sink := &lagTestDurationSink{header: make(http.Header)}
		writer := newLagTestDurationWriter(sink, ctx, func() *lagApiPhase { return phase })
		done := make(chan struct{})
		go func() {
			defer close(done)
			api.writeDownload(ctx, writer, phase, stream)
		}()
		synctest.Wait()
		reserved := lagTestDurationBudgetUsed(api.budget)
		cancel()
		<-done
		_, _, end := phase.state()
		if reserved != 3*lagApiBufferByteCount || end != lagApiPhaseRunning ||
			sink.byteCount != testLagDurationByteCount || lagTestDurationBudgetUsed(api.budget) != testLagDurationByteCount ||
			phase.consumed() != testLagDurationByteCount || phase.capRemaining.Load() != capByteCount-testLagDurationByteCount {
			t.Errorf("request cancellation lost reservation ownership: reserved=%d end=%d written=%d budget=%d consumed=%d remaining=%d",
				reserved, end, sink.byteCount, lagTestDurationBudgetUsed(api.budget), phase.consumed(), phase.capRemaining.Load())
		}
		phase.finish(lagApiPhaseEndClosed)
		<-phase.ctx.Done()
		if _, _, end := phase.state(); end != lagApiPhaseEndClosed {
			t.Errorf("explicit close ended %d", end)
		}
	})
}

// A real write failure returns only its unwritten tail to both independent
// budgets; the fixture must not suppress or double-refund that error.
func TestLagApiDurationFixtureReturnsPartialWriteReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const capByteCount = int64(4 * lagApiBufferByteCount)
		api := &extenderLagApi{budget: newLagApiBudget(capByteCount)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
			time.Second, lagApiInterval, 1, capByteCount, 2*time.Second, func() {})
		defer phase.finish(lagApiPhaseEndClosed)
		stream, err := newLagApiDownloadKeystream()
		if err != nil {
			t.Fatal(err)
		}
		sink := &lagTestDurationSink{header: make(http.Header), partialByteCount: 17, writeError: io.ErrClosedPipe}
		writer := newLagTestDurationWriter(sink, context.Background(), func() *lagApiPhase { return phase })
		api.writeDownload(context.Background(), writer, phase, stream)
		_, _, end := phase.state()
		if end != lagApiPhaseRunning || sink.byteCount != 17 || lagTestDurationBudgetUsed(api.budget) != 17 ||
			phase.consumed() != 17 || phase.total() != 17 || phase.capRemaining.Load() != capByteCount-17 {
			t.Errorf("partial-write ownership changed: end=%d written=%d budget=%d consumed=%d total=%d remaining=%d",
				end, sink.byteCount, lagTestDurationBudgetUsed(api.budget), phase.consumed(), phase.total(), phase.capRemaining.Load())
		}
	})
}

// The production first-terminal rule already resolves a delayed timer: cap or
// close after the actual window is time, while an earlier cap stays cap.
func TestLagApiDurationFixturePreservesPhaseEndOrder(t *testing.T) {
	for _, requestedEnd := range []lagApiPhaseEnd{lagApiPhaseEndCap, lagApiPhaseEndClosed} {
		synctest.Test(t, func(t *testing.T) {
			api := &extenderLagApi{budget: newLagApiBudget(0)}
			phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
				500*time.Millisecond, lagApiInterval, 1, 4*lagApiBufferByteCount, time.Second, func() {})
			defer phase.finish(lagApiPhaseEndClosed)
			start := time.Now()
			if _, ok := phase.window(start); !ok || !phase.windowTimer.Stop() {
				t.Fatal("could not hold the window timer")
			}
			time.Sleep(500 * time.Millisecond)
			phase.finish(requestedEnd)
			_, ended, end := phase.state()
			if end != lagApiPhaseEndTime || !ended.Equal(start.Add(500*time.Millisecond)) {
				t.Errorf("late terminal %d replaced time: end=%d ended=%s", requestedEnd, end, ended)
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		api := &extenderLagApi{budget: newLagApiBudget(0)}
		phase := newLagApiPhase(api, context.Background(), lagApiDirectionDownload,
			500*time.Millisecond, lagApiInterval, 1, lagApiBufferByteCount, time.Second, func() {})
		defer phase.finish(lagApiPhaseEndClosed)
		start := time.Now()
		phase.window(start)
		phase.finish(lagApiPhaseEndCap)
		time.Sleep(500 * time.Millisecond)
		phase.finish(lagApiPhaseEndTime)
		_, ended, end := phase.state()
		if end != lagApiPhaseEndCap || !ended.Equal(start) {
			t.Errorf("earlier cap was replaced: end=%d ended=%s", end, ended)
		}
	})
}

// Finite upload frames leave the socket open until the real server phase sends
// its normal close. Real TLS/HTTP/WS, control done, reports and worker joins stay
// in the existing fixture; no extended timer or increased cap makes this complete.
func TestLagApiDurationFixtureLoadJoinsFiniteStreams(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.MaxStreams = 1
		body.DownloadMs = 500
		body.UploadMs = 500
		body.MaxDownloadBytes = 4 * lagApiBufferByteCount
		body.MaxUploadBytes = 4 * lagApiBufferByteCount
	}))
	load := session.runLoad(lagTestLoadOptions{uploadByteCount: testLagDurationByteCount})
	if load.result.Type != "loadResult" || load.result.EndReason != "complete" || !load.result.UploadVerified ||
		load.downloadedByteCount != testLagDurationByteCount || load.uploadedByteCount != testLagDurationByteCount ||
		load.downloadStreamCount != 1 || load.uploadStreamCount != 1 ||
		len(load.uploadCloseCodes) != 1 || load.uploadCloseCodes[0] != websocket.CloseNormalClosure ||
		load.doneTime.IsZero() || load.resultTime.Before(load.doneTime) || fixture.api.loadCountNow() != 0 {
		t.Fatalf("finite stream lifecycle: result=%+v down=%d up=%d streams=%d/%d closes=%v done=%s result=%s loads=%d",
			load.result.raw, load.downloadedByteCount, load.uploadedByteCount, load.downloadStreamCount,
			load.uploadStreamCount, load.uploadCloseCodes, load.doneTime, load.resultTime, fixture.api.loadCountNow())
	}
	session.close()
	report := fixture.nextReport(5 * time.Second)
	sum := func(values []uint64) uint64 {
		var count uint64
		for _, value := range values {
			count += value
		}
		return count
	}
	// the upload counts what arrived from the window's first byte on: its
	// first group's part from before it, at the rate of its second, does not
	// count (amendment 15), so the series holds at most what was sent
	if report.EndReason != protocol.LagEndReason_LAG_END_COMPLETE || !report.UploadVerified ||
		report.DownloadMs != 500 || report.UploadMs != 500 ||
		sum(report.DownloadSentBytes) != testLagDurationByteCount ||
		sum(report.UploadBytes) == 0 || testLagDurationByteCount < sum(report.UploadBytes) {
		t.Errorf("finite stream report: reason=%v verified=%t duration=%d/%d bytes=%d/%d",
			report.EndReason, report.UploadVerified, report.DownloadMs, report.UploadMs,
			sum(report.DownloadSentBytes), sum(report.UploadBytes))
	}
	load.checkUpload(t, report)
}
