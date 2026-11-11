// The bounds, counts and error classes of SendControlWithRetry. Requests stay
// inside the synthetic platform of transfer_close_report_retry_test.go and
// every wait runs on the synctest clock. Retry waits take each nominal wait's
// lower half, so every time asserted is exact.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A frame carrying one identified close report, which the synthetic platform
// answers from its script.
func oobRetryTestFrame(t *testing.T) *protocol.Frame {
	t.Helper()
	frame, err := ToFrame(&protocol.CloseContract{
		ContractId:       NewId().Bytes(),
		AckedByteCount:   121,
		UnackedByteCount: 7,
		ReportId:         NewId().Bytes(),
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// A control over the platform's transport whose retry waits are each nominal
// wait's lower half.
func newOobRetryTestControl(t *testing.T, platform *closeReportPlatform) *ApiOutOfBandControl {
	t.Helper()
	api, strategy := authObservationTestApi(t.Context(), nil, true, platform)
	t.Cleanup(api.Close)
	control := newApiOutOfBandControl(t.Context(), strategy, "synthetic-window-token", "https://api.close-report.example", false)
	control.retryJitterForTest = func(time.Duration) time.Duration { return 0 }
	return control
}

// Sends one report with retry. Returns how long the request took and its
// callback error.
func sendOobRetryTestReport(t *testing.T, control OutOfBandControlWithRetry, retry *OobRetrySettings) (time.Duration, error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	control.SendControlWithRetry(context.Background(), []*protocol.Frame{oobRetryTestFrame(t)}, retry, func(_ []*protocol.Frame, err error) {
		done <- err
	})
	err := <-done
	return time.Since(start), err
}

// The virtual times of the platform's events, from start.
func closeReportEventOffsets(platform *closeReportPlatform, start time.Time) []time.Duration {
	_, _, eventTimes := platform.snapshot()
	offsets := []time.Duration{}
	for _, eventTime := range eventTimes {
		offsets = append(offsets, eventTime.Sub(start))
	}
	return offsets
}

// Each bound ends a request whose attempts keep failing: the attempt count, a
// wait that would cross the time budget, and the time budget itself, which
// cancels the running attempt.
func TestOobRetryBudgetBoundsAttemptsAndTime(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		unavailable := closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream")
		for _, c := range []struct {
			name   string
			answer closeReportAnswer
			retry  OobRetrySettings
			// when each attempt was answered, and when the request ended
			wantAnswers []time.Duration
			wantDone    time.Duration
			// whether the request ended on the platform's 503
			wantStatus bool
		}{
			{
				// waits of 0.5 and 1 s, then the attempts run out
				name:        "attempts",
				answer:      unavailable,
				retry:       OobRetrySettings{MaxAttemptCount: 3, RetryTimeout: time.Second, RetryMaxTimeout: 4 * time.Second},
				wantAnswers: []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond},
				wantDone:    1500 * time.Millisecond,
				wantStatus:  true,
			},
			{
				// waits of 0.5, 1, 2, 2, 2 and 2 s; the next 2 s wait would reach
				// the 10 s bound, so the request ends without it
				name:   "time between attempts",
				answer: unavailable,
				retry:  OobRetrySettings{MaxAttemptCount: 100, Timeout: 10 * time.Second, RetryTimeout: time.Second, RetryMaxTimeout: 4 * time.Second},
				wantAnswers: []time.Duration{
					0, 500 * time.Millisecond, 1500 * time.Millisecond, 3500 * time.Millisecond,
					5500 * time.Millisecond, 7500 * time.Millisecond, 9500 * time.Millisecond,
				},
				wantDone:   9500 * time.Millisecond,
				wantStatus: true,
			},
			{
				// the first attempt ends at its 15 s RequestTimeout; the second
				// starts 0.5 s later and is canceled at the 20 s bound
				name:        "time in an attempt",
				answer:      closeReportBlackHoleAnswer,
				retry:       OobRetrySettings{MaxAttemptCount: 100, Timeout: 20 * time.Second, RetryTimeout: time.Second, RetryMaxTimeout: 4 * time.Second},
				wantAnswers: []time.Duration{15 * time.Second, 20 * time.Second},
				wantDone:    20 * time.Second,
			},
		} {
			platform := &closeReportPlatform{t: t, answers: []closeReportAnswer{c.answer}}
			control := newOobRetryTestControl(t, platform)
			start := time.Now()
			done, err := sendOobRetryTestReport(t, control, &c.retry)
			if joinErr := control.CloseAndWait(t.Context()); joinErr != nil {
				t.Fatalf("%s: control join: %v", c.name, joinErr)
			}
			if answers := closeReportEventOffsets(platform, start); !slices.Equal(answers, c.wantAnswers) || done != c.wantDone {
				t.Fatalf("%s: attempts answered at %v and ended at %s, want %v and %s", c.name, answers, done, c.wantAnswers, c.wantDone)
			}
			var statusErr *HttpStatusError
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("after %d attempts", len(c.wantAnswers))) ||
				errors.As(err, &statusErr) != c.wantStatus {
				t.Fatalf("%s: request error %v does not name its attempts and last failure", c.name, err)
			}
			wantStats := OobRetryStats{RequestCount: 1, RetryCount: int64(len(c.wantAnswers) - 1), GiveUpCount: 1}
			if stats := control.RetryStats(); stats != wantStats {
				t.Fatalf("%s: counts %+v, want %+v", c.name, stats, wantStats)
			}
		}
	})
}

// A retiring window client's identity waits for an in-flight report only
// within the report's budget: a report whose attempts all go unanswered ends
// at CloseReportRetry.Timeout with its running attempt canceled, and the
// identity retires then.
func TestWindowRetirementWaitsForCloseReportOnlyWithinBudget(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		platform := &closeReportPlatform{t: t, answers: []closeReportAnswer{closeReportBlackHoleAnswer}}
		generator := newCloseReportRetryGenerator(t, platform)
		outcomes := &closeReportOutcomeLogger{}
		settings := closeReportRetryClientSettings(outcomes)
		settings.ContractManagerSettings.CloseReportRetry = OobRetrySettings{
			MaxAttemptCount: 100,
			Timeout:         20 * time.Second,
			RetryTimeout:    time.Second,
			RetryMaxTimeout: 4 * time.Second,
		}
		contractId, reportTime := retireCloseReportRetryClient(t, generator, settings, func(control *ApiOutOfBandControl) {
			control.retryJitterForTest = func(time.Duration) time.Duration { return 0 }
		})
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatalf("generator retirement: %v", err)
		}
		requireCloseReportRequests(t, "retirement", platform, contractId, []string{"control no answer", "control no answer", "remove"})
		// the first attempt ends at its 15 s RequestTimeout; the second starts
		// 0.5 s later and is canceled at the 20 s budget, when the identity
		// retires
		want := []time.Duration{15 * time.Second, 20 * time.Second, 20 * time.Second}
		if offsets := closeReportEventOffsets(platform, reportTime); !slices.Equal(offsets, want) {
			t.Fatalf("retirement events at %v, want %v", offsets, want)
		}
		requireCloseReportOutcome(t, "retirement", outcomes, contractId, false)
	})
}

// A control counts each retried request once by its outcome, and the process
// counts move with it.
func TestOobRetryStatsCountEachOutcome(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		// the first request is processed on its retry, the second at once, and
		// the third is refused
		platform := &closeReportPlatform{t: t, answers: []closeReportAnswer{
			closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream"),
			closeReportProcessedAnswer(),
			closeReportProcessedAnswer(),
			closeReportStatusAnswer(http.StatusUnauthorized, "invalid client token"),
		}}
		control := newOobRetryTestControl(t, platform)
		retry := OobRetrySettings{MaxAttemptCount: 3, RetryTimeout: time.Second, RetryMaxTimeout: 4 * time.Second}
		before := OobRetryStatsSnapshot()
		errs := []error{}
		for range 3 {
			_, err := sendOobRetryTestReport(t, control, &retry)
			errs = append(errs, err)
		}
		if err := control.CloseAndWait(t.Context()); err != nil {
			t.Fatalf("control join: %v", err)
		}
		var statusErr *HttpStatusError
		if errs[0] != nil || errs[1] != nil || !errors.As(errs[2], &statusErr) ||
			statusErr.StatusCode != http.StatusUnauthorized || strings.Contains(errs[2].Error(), "attempt") {
			t.Fatalf("request outcomes %v, want two processed and one plain 401", errs)
		}
		want := OobRetryStats{RequestCount: 3, RetryCount: 1, RetrySuccessCount: 1, RefusedCount: 1}
		if stats := control.RetryStats(); stats != want {
			t.Fatalf("counts %+v, want %+v", stats, want)
		}
		// other owners may count meanwhile, so the process moves at least as far
		after := OobRetryStatsSnapshot()
		if after.RequestCount-before.RequestCount < want.RequestCount ||
			after.RetryCount-before.RetryCount < want.RetryCount ||
			after.RetrySuccessCount-before.RetrySuccessCount < want.RetrySuccessCount ||
			after.RefusedCount-before.RefusedCount < want.RefusedCount {
			t.Fatalf("process counts moved from %+v to %+v, want at least %+v", before, after, want)
		}
	})
}

// In-process local control has no transport to fail transiently, and the zero
// value is no budget: each sends once, as SendControlWithCtx does.
func TestOobRetryLocalControlAndZeroValueSendOnce(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		platform := &closeReportPlatform{t: t, answers: []closeReportAnswer{
			closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream"),
		}}
		control := newOobRetryTestControl(t, platform)
		_, err := sendOobRetryTestReport(t, control, &OobRetrySettings{})
		if joinErr := control.CloseAndWait(t.Context()); joinErr != nil {
			t.Fatalf("control join: %v", joinErr)
		}
		var statusErr *HttpStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable || strings.Contains(err.Error(), "attempt") {
			t.Fatalf("zero value request error %v, want the plain 503", err)
		}
		if _, events, _ := platform.snapshot(); len(events) != 1 {
			t.Fatalf("zero value sent %d requests, want 1", len(events))
		}
		if stats := control.RetryStats(); stats != (OobRetryStats{}) {
			t.Fatalf("zero value counted %+v", stats)
		}

		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("local control escaped to http")
			return nil, errors.New("unexpected request")
		}))
		defer api.Close()
		var calls atomic.Int32
		local := NewApiOutOfBandControlWithLocalControl(t.Context(), strategy, lateLocalJwt(t, NewId()), "https://local.invalid", privateLocalControl(func(context.Context, string, *ConnectControlArgs) (*ConnectControlResult, error) {
			calls.Add(1)
			return nil, &HttpStatusError{StatusCode: http.StatusServiceUnavailable}
		}))
		local.retryJitterForTest = func(time.Duration) time.Duration { return 0 }
		_, err = sendOobRetryTestReport(t, local, &OobRetrySettings{MaxAttemptCount: 6, Timeout: time.Minute, RetryTimeout: time.Second, RetryMaxTimeout: 16 * time.Second})
		if joinErr := local.CloseAndWait(t.Context()); joinErr != nil {
			t.Fatalf("local control join: %v", joinErr)
		}
		if err == nil || calls.Load() != 1 {
			t.Fatalf("local control called %d times with error %v, want one failed call", calls.Load(), err)
		}
		if stats := local.RetryStats(); stats != (OobRetryStats{}) {
			t.Fatalf("local control counted %+v", stats)
		}
	})
}

// Retries follow the platform's answer: no answer, a 408, a 429 or a 5xx may
// pass when repeated, and every other answer is definitive.
func TestOobRetryableClassifiesFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "500", err: &HttpStatusError{StatusCode: http.StatusInternalServerError}, want: true},
		{name: "502", err: &HttpStatusError{StatusCode: http.StatusBadGateway}, want: true},
		{name: "503", err: &HttpStatusError{StatusCode: http.StatusServiceUnavailable}, want: true},
		{name: "504", err: &HttpStatusError{StatusCode: http.StatusGatewayTimeout}, want: true},
		{name: "599", err: &HttpStatusError{StatusCode: 599}, want: true},
		{name: "429", err: &HttpStatusError{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "408", err: &HttpStatusError{StatusCode: http.StatusRequestTimeout}, want: true},
		{name: "wrapped 503", err: fmt.Errorf("attempt: %w", &HttpStatusError{StatusCode: http.StatusServiceUnavailable}), want: true},
		{name: "exhausted", err: &HttpRequestExhaustedError{causes: []error{context.DeadlineExceeded}}, want: true},
		{name: "network", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "eof", err: io.EOF, want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "400", err: &HttpStatusError{StatusCode: http.StatusBadRequest}, want: false},
		{name: "401", err: &HttpStatusError{StatusCode: http.StatusUnauthorized}, want: false},
		{name: "402", err: &HttpStatusError{StatusCode: http.StatusPaymentRequired}, want: false},
		{name: "403", err: &HttpStatusError{StatusCode: http.StatusForbidden}, want: false},
		{name: "404", err: &HttpStatusError{StatusCode: http.StatusNotFound}, want: false},
		{name: "409", err: &HttpStatusError{StatusCode: http.StatusConflict}, want: false},
		{name: "600", err: &HttpStatusError{StatusCode: 600}, want: false},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "body too large", err: fmt.Errorf("%w: limit 2", ErrHttpResponseBodyTooLarge), want: false},
		{name: "rejection", err: errors.New("connect control rejected: Contract not found"), want: false},
		{name: "json", err: &json.SyntaxError{}, want: false},
	} {
		if got := oobRetryable(c.err); got != c.want {
			t.Errorf("%s: retryable %t, want %t", c.name, got, c.want)
		}
	}
}
