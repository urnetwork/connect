package extender

// The report queue of the lifecycle (lag.dev/PLAN.md M8, A7), against the test
// operator and clock of extender_lag_cert_test.go. A report is opaque to the
// queue, so the reports here are markers whose bodies say which one they are.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The ith marker report.
func testLagReport(i int) *protocol.LagReport {
	return &protocol.LagReport{
		Body:      []byte(fmt.Sprintf("report %d", i)),
		Signature: bytes.Repeat([]byte{byte(i)}, 64),
	}
}

// Checks one post: standard base64 of each serialized report, in order.
func expectTestLagReports(t *testing.T, postedReports []string, expectedReports []*protocol.LagReport) {
	t.Helper()
	if len(postedReports) != len(expectedReports) {
		t.Fatalf("posted %d reports, expected %d", len(postedReports), len(expectedReports))
	}
	for i, postedReport := range postedReports {
		reportBytes, err := base64.StdEncoding.DecodeString(postedReport)
		if err != nil {
			t.Fatal(err)
		}
		report := &protocol.LagReport{}
		if err := proto.Unmarshal(reportBytes, report); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(report, expectedReports[i]) {
			t.Fatalf("report %d = %q, expected %q", i, report.Body, expectedReports[i].Body)
		}
	}
}

// A batch goes when it is full, and what remains goes the flush timeout later,
// oldest first, under the activation credential (M8, A7).
func TestLagReportBatches(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	lifecycle := newTestLagLifecycle(t, api, clock, nil, nil)

	// The queue can wake for any of the reports and start a flush wait each
	// time, so how many waits it starts before the full batch goes depends on
	// how its loop and the submits interleave. The batch's post is answered
	// once every report is in, passing over those waits: the next wait is the
	// one the rest wait on. The clock moves only once the queue has started
	// it, or the wait would count from the moved clock and never come due.
	submitted := make(chan struct{})
	api.setBeforeReportAnswer(func() {
		<-submitted
		clock.skipWaits()
	})
	reports := []*protocol.LagReport{}
	for i := range 40 {
		report := testLagReport(i)
		reports = append(reports, report)
		lifecycle.Submit(report)
	}
	close(submitted)
	expectTestLagReports(t, api.waitPost(), reports[0:32])
	api.setBeforeReportAnswer(nil)
	// the clock has not moved, so the rest waits for its flush
	if wait := clock.nextWait(t); wait != 10*time.Second {
		t.Fatalf("flush wait = %s, expected the flush timeout", wait)
	}
	api.expectQuiet()
	clock.advance(10 * time.Second)
	expectTestLagReports(t, api.waitPost(), reports[32:40])
	api.expectQuiet()
}

// A post the operator did not answer is retried on a backoff that doubles, and
// a success resets it (M8).
func TestLagReportRetriesWithBackoff(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	api.setReportAnswer(http.StatusInternalServerError, nil)
	lifecycle := newTestLagLifecycle(t, api, clock, nil, nil)

	report := testLagReport(0)
	lifecycle.Submit(report)
	if wait := clock.nextWait(t); wait != 10*time.Second {
		t.Fatalf("flush wait = %s, expected the flush timeout", wait)
	}
	clock.advance(10 * time.Second)
	expectTestLagReports(t, api.waitPost(), []*protocol.LagReport{report})
	for _, backoff := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second} {
		if wait := clock.nextWait(t); wait != backoff {
			t.Fatalf("retry wait = %s, expected %s", wait, backoff)
		}
		api.expectQuiet()
		clock.advance(backoff)
		expectTestLagReports(t, api.waitPost(), []*protocol.LagReport{report})
	}

	// answered: done, and the next report is not held by the backoff
	api.setReportAnswer(http.StatusOK, nil)
	if wait := clock.nextWait(t); wait != 120*time.Second {
		t.Fatalf("retry wait = %s, expected 120s", wait)
	}
	clock.advance(120 * time.Second)
	expectTestLagReports(t, api.waitPost(), []*protocol.LagReport{report})
	next := testLagReport(1)
	lifecycle.Submit(next)
	if wait := clock.nextWait(t); wait != 10*time.Second {
		t.Fatalf("flush wait = %s, expected the flush timeout after a success", wait)
	}
	clock.advance(10 * time.Second)
	expectTestLagReports(t, api.waitPost(), []*protocol.LagReport{next})
}

// A batch the operator refuses as a batch, or answers with rejections, is not
// retried: either answer would only repeat (A7).
func TestLagReportDoesNotRetryAnAnsweredBatch(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	lifecycle := newTestLagLifecycle(t, api, clock, nil, nil)

	for i, answer := range []struct {
		status int
		result *extenderLagReportResult
	}{
		{status: http.StatusBadRequest},
		{status: http.StatusOK, result: &extenderLagReportResult{
			Rejected: []*extenderLagReportRejection{{Index: 0, Reason: "the ticket has a report"}},
		}},
	} {
		api.setReportAnswer(answer.status, answer.result)
		report := testLagReport(2 * i)
		lifecycle.Submit(report)
		clock.nextWait(t)
		clock.advance(10 * time.Second)
		expectTestLagReports(t, api.waitPost(), []*protocol.LagReport{report})

		// the next post carries the next report alone
		api.setReportAnswer(http.StatusOK, nil)
		next := testLagReport(2*i + 1)
		lifecycle.Submit(next)
		if wait := clock.nextWait(t); wait != 10*time.Second {
			t.Fatalf("flush wait = %s, expected no retry hold", wait)
		}
		clock.advance(10 * time.Second)
		expectTestLagReports(t, api.waitPost(), []*protocol.LagReport{next})
	}
}

// The queue holds at most its cap and drops the oldest, whether a report
// arrives over it or a failed batch is put back over it (M8).
func TestLagReportDropsTheOldest(t *testing.T) {
	settings := DefaultExtenderLagLifecycleSettings()
	settings.Now = newTestLagClock().Now
	// the queue alone, without its loop, so every step is the test's
	reporter := &extenderLagReporter{
		settings:       settings,
		wakeMonitor:    connect.NewMonitor(),
		pendingReports: [][]byte{},
	}
	reports := []*protocol.LagReport{}
	for i := range 340 {
		reports = append(reports, testLagReport(i))
	}
	pendingReports := func() []*protocol.LagReport {
		pending := []*protocol.LagReport{}
		for _, reportBytes := range reporter.pendingReports {
			report := &protocol.LagReport{}
			if err := proto.Unmarshal(reportBytes, report); err != nil {
				t.Fatal(err)
			}
			pending = append(pending, report)
		}
		return pending
	}
	expectPending := func(expectedReports []*protocol.LagReport, expectedDropCount int) {
		t.Helper()
		pending := pendingReports()
		if len(pending) != len(expectedReports) {
			t.Fatalf("pending = %d, expected %d", len(pending), len(expectedReports))
		}
		for i, report := range pending {
			if !proto.Equal(report, expectedReports[i]) {
				t.Fatalf("pending %d = %q, expected %q", i, report.Body, expectedReports[i].Body)
			}
		}
		if dropCount := reporter.dropCount(); dropCount != expectedDropCount {
			t.Fatalf("dropped = %d, expected %d", dropCount, expectedDropCount)
		}
	}

	for _, report := range reports[0:300] {
		reporter.submit(report)
	}
	expectPending(reports[44:300], 44)

	reportBatch, _, _ := reporter.takeBatch(settings.Now().Add(time.Hour), time.Time{})
	if len(reportBatch) != 32 {
		t.Fatalf("batch = %d, expected 32", len(reportBatch))
	}
	expectPending(reports[76:300], 44)
	for _, report := range reports[300:340] {
		reporter.submit(report)
	}
	expectPending(reports[84:340], 52)
	// the batch is older than everything queued, so it is what goes
	reporter.requeue(reportBatch)
	expectPending(reports[84:340], 84)

	// a nil report is no report
	reporter.submit(nil)
	expectPending(reports[84:340], 84)
}
