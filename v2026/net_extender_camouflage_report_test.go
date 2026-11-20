package connect

// The camouflage report's sender (net_extender_camouflage_report.go;
// EXTENDER-SPLICE-ROLLOUT.md, Reporting path). The sender runs its own loop against a
// fake clock, a fake timer that hands the test every wait the loop parks on, and a
// fake route that hands the test every post and answers as the test says. Nothing
// waits on wall time.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A report as the sender posts it, every field set, which the operator's route pins
// too (server controller/extender_camouflage_report_test.go), so a change on either
// side shows on that side.
const testCamouflageReportContractBody = `{
	"report_id": "0123456789abcdef0123456789abcdef",
	"client_version": "2026.10.30-1085916410",
	"resource_version": 3,
	"ip_versions": [4, 6],
	"client": {
		"camouflage_won": 1,
		"legacy_won": 2,
		"both_failed": 3,
		"refused": 4,
		"camouflage_unauthenticated": 5,
		"camouflage_leaf_rejected": 6,
		"legacy_only_no_key": 7,
		"legacy_only_fingerprint": 8,
		"legacy_only_no_front_name": 9
	},
	"extender": {
		"authenticated": 11,
		"spliced": 12,
		"terminated": 13,
		"splice_fallback_terminated": 14,
		"replay_refused": 15,
		"time_window_failed": 16,
		"splice_dial_failed": 17,
		"borrow_verify_failed": 18,
		"targets": [
			{"name": "Listed.Example", "splice_count": 21, "relayed_byte_count": 2100},
			{"name": "unlisted.example", "splice_count": 22, "relayed_byte_count": 2200}
		],
		"covered": false,
		"degraded_reason": "splice_off"
	}
}`

type testCamouflageReportClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testCamouflageReportClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testCamouflageReportClock) advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// One post, and the way to answer it.
type testCamouflageReportPost struct {
	requestUrl string
	body       []byte
	byJwt      string
	ctx        context.Context
	reply      chan *extenderCamouflagePostResult
}

func (self *testCamouflageReportPost) args(t *testing.T) *ExtenderCamouflageReportArgs {
	t.Helper()
	args := &ExtenderCamouflageReportArgs{}
	if err := json.Unmarshal(self.body, args); err != nil {
		t.Fatal(err)
	}
	return args
}

func (self *testCamouflageReportPost) answer(outcome string) {
	self.reply <- &extenderCamouflagePostResult{
		responseBytes: []byte(fmt.Sprintf(`{"outcome":%q}`, outcome)),
	}
}

func (self *testCamouflageReportPost) fail(err error) {
	self.reply <- &extenderCamouflagePostResult{err: err}
}

// What the loop did next. It does exactly one per pass.
type testCamouflageReportEvent struct {
	// parked on a timer for this long
	parked bool
	wait   time.Duration
	// parked with nothing scheduled
	inactive bool
	post     *testCamouflageReportPost
}

// An extender's part that hands out what the test set, once.
type testCamouflageReportSource struct {
	stateLock sync.Mutex
	counts    *ExtenderCamouflageExtenderCounts
	discarded int
}

func (self *testCamouflageReportSource) TakeCamouflageReportCounts() *ExtenderCamouflageExtenderCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	counts := self.counts
	self.counts = &ExtenderCamouflageExtenderCounts{DegradedReason: "splice_off"}
	return counts
}

func (self *testCamouflageReportSource) DiscardCamouflageReportCounts() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.discarded += 1
	self.counts = &ExtenderCamouflageExtenderCounts{DegradedReason: "splice_off"}
}

type testCamouflageReportHarness struct {
	t        *testing.T
	clock    *testCamouflageReportClock
	settings *ExtenderCamouflageReporterSettings
	account  *MonitorValue[uint64]
	reporter *ExtenderCamouflageReporter
	events   chan *testCamouflageReportEvent
	fire     chan time.Time

	stateLock sync.Mutex
	byJwt     string
	counters  ExtenderTcpCamouflageClientStats
	reportIds int
}

func newTestCamouflageReportHarness(t *testing.T, configure func(settings *ExtenderCamouflageReporterSettings)) *testCamouflageReportHarness {
	t.Helper()
	self := &testCamouflageReportHarness{
		t:       t,
		clock:   &testCamouflageReportClock{now: time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)},
		account: NewMonitorValue[uint64](0),
		events:  make(chan *testCamouflageReportEvent, 16),
		fire:    make(chan time.Time, 1),
		byJwt:   "test-token",
	}
	settings := DefaultExtenderCamouflageReporterSettings()
	settings.ApiUrl = "https://api.report.example"
	settings.ByJwt = func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.byJwt
	}
	settings.AccountMonitor = self.account
	settings.ClientVersion = func() string { return "2026.10.30-1085916410" }
	settings.ResourceVersion = func() int { return 3 }
	settings.IpVersions = func() []int { return []int{4, 6} }
	settings.Now = self.clock.Now
	settings.After = func(wait time.Duration) <-chan time.Time {
		self.events <- &testCamouflageReportEvent{parked: true, wait: wait}
		return self.fire
	}
	settings.Random = func() float64 { return 0.5 }
	settings.NewReportId = func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reportIds += 1
		return fmt.Sprintf("%032x", self.reportIds)
	}
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		post := &testCamouflageReportPost{
			requestUrl: requestUrl,
			body:       slices.Clone(requestBodyBytes),
			byJwt:      byJwt,
			ctx:        ctx,
			reply:      make(chan *extenderCamouflagePostResult, 1),
		}
		self.events <- &testCamouflageReportEvent{post: post}
		select {
		case result := <-post.reply:
			return result.responseBytes, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	settings.onInactive = func() {
		self.events <- &testCamouflageReportEvent{inactive: true}
	}
	settings.clientLedger = &extenderCamouflageClientLedger{snapshot: self.snapshot}
	if configure != nil {
		configure(settings)
	}
	self.settings = settings
	self.reporter = NewExtenderCamouflageReporter(context.Background(), settings)
	t.Cleanup(self.reporter.Close)
	return self
}

func (self *testCamouflageReportHarness) snapshot() ExtenderTcpCamouflageClientStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counters
}

// Applies one change to the fake counters.
func (self *testCamouflageReportHarness) addCounts(update func(counters *ExtenderTcpCamouflageClientStats)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	update(&self.counters)
}

func (self *testCamouflageReportHarness) setByJwt(byJwt string) {
	self.stateLock.Lock()
	self.byJwt = byJwt
	self.stateLock.Unlock()
}

// A new identity, the way the space's account monitor says so.
func (self *testCamouflageReportHarness) changeAccount(byJwt string) {
	self.setByJwt(byJwt)
	self.account.Update(func(generation uint64) uint64 {
		return generation + 1
	})
}

func (self *testCamouflageReportHarness) next() *testCamouflageReportEvent {
	self.t.Helper()
	select {
	case event := <-self.events:
		return event
	case <-time.After(30 * time.Second):
		self.t.Fatal("the sender did nothing")
		return nil
	}
}

// The next pass must be a park, and returns its wait.
func (self *testCamouflageReportHarness) requireParked(what string) time.Duration {
	self.t.Helper()
	event := self.next()
	if !event.parked {
		self.t.Fatalf("%s: event = %+v, want a park", what, event)
	}
	return event.wait
}

// The next pass must wait with nothing scheduled.
func (self *testCamouflageReportHarness) requireInactive(what string) {
	self.t.Helper()
	event := self.next()
	if !event.inactive {
		self.t.Fatalf("%s: event = %+v, want no schedule", what, event)
	}
}

// The next pass must be a post.
func (self *testCamouflageReportHarness) requirePost(what string) *testCamouflageReportPost {
	self.t.Helper()
	event := self.next()
	if event.post == nil {
		self.t.Fatalf("%s: event = %+v, want a post", what, event)
	}
	return event.post
}

// Moves the clock by the wait and fires the timer, which ends the park.
func (self *testCamouflageReportHarness) fireAfter(wait time.Duration) {
	self.clock.advance(wait)
	self.fire <- self.clock.Now()
}

// Enables the sender, and takes its first park.
func (self *testCamouflageReportHarness) enable() time.Duration {
	self.t.Helper()
	self.requireInactive("before it is on")
	self.reporter.SetEnabled(true)
	return self.requireParked("the first look")
}

// Runs to the next post of a sender parked after `wait`.
func (self *testCamouflageReportHarness) postAfter(wait time.Duration, what string) *testCamouflageReportPost {
	self.t.Helper()
	self.fireAfter(wait)
	return self.requirePost(what)
}

func TestExtenderCamouflageReporterIsOffUntilEnabled(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 5
	})
	harness.requireInactive("off")
	// nothing wakes it: no timer is even set
	harness.clock.advance(24 * time.Hour)
	select {
	case event := <-harness.events:
		t.Fatalf("event = %+v while off", event)
	case <-time.After(50 * time.Millisecond):
	}
	if stats := harness.reporter.Stats(); stats != (ExtenderCamouflageReporterStats{}) {
		t.Errorf("stats = %+v", stats)
	}
}

// The first look is soon, jittered, and what it sends is what was counted since the
// sender came on. A report delivered waits an interval for the next, which carries
// only what is new, under a new id.
func TestExtenderCamouflageReporterSendsADeltaEachInterval(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	if wait != 10*time.Minute {
		t.Errorf("first look = %s, want the middle of 5 to 15 minutes", wait)
	}
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.CamouflageWonCount = 3
		counters.BothFailedCount = 1
	})
	post := harness.postAfter(wait, "the first report")
	if post.requestUrl != "https://api.report.example/network/extender-camouflage-report" {
		t.Errorf("url = %q", post.requestUrl)
	}
	if post.byJwt != "test-token" {
		t.Errorf("credential = %q", post.byJwt)
	}
	args := post.args(t)
	wantArgs := &ExtenderCamouflageReportArgs{
		ReportId:        fmt.Sprintf("%032x", 1),
		ClientVersion:   "2026.10.30-1085916410",
		ResourceVersion: 3,
		IpVersions:      []int{4, 6},
		Client:          &ExtenderCamouflageClientCounts{CamouflageWon: 3, BothFailed: 1},
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %+v, want %+v", args, wantArgs)
	}
	post.answer(ExtenderCamouflageReportAccepted)

	wait = harness.requireParked("after the report")
	if wait != time.Hour {
		t.Errorf("interval = %s, want the middle of an hour either way", wait)
	}
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 4
		counters.LegacyOnlyNoKeyCount = 2
	})
	post = harness.postAfter(wait, "the next report")
	args = post.args(t)
	if args.ReportId == wantArgs.ReportId {
		t.Errorf("a new report kept the old id")
	}
	if !reflect.DeepEqual(args.Client, &ExtenderCamouflageClientCounts{BothFailed: 3, LegacyOnlyNoKey: 2}) {
		t.Errorf("delta = %+v", args.Client)
	}
	post.answer(ExtenderCamouflageReportDuplicate)
	harness.requireParked("a duplicate answer is delivered too")
	stats := harness.reporter.Stats()
	if stats.ReportCount != 2 || stats.AttemptCount != 2 || stats.DeliveredCount != 2 {
		t.Errorf("stats = %+v", stats)
	}
}

// Jitter spreads a fleet across its whole range, both ends.
func TestExtenderCamouflageReporterJitterStaysInItsRange(t *testing.T) {
	for _, c := range []struct {
		random float64
		first  time.Duration
		next   time.Duration
		retry  time.Duration
	}{
		{random: 0, first: 5 * time.Minute, next: 45 * time.Minute, retry: 5 * time.Minute},
		{random: 0.999, first: 15*time.Minute - 600*time.Millisecond, next: 75*time.Minute - 1800*time.Millisecond, retry: 6*time.Minute + 14925*time.Millisecond},
	} {
		harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
			settings.Random = func() float64 { return c.random }
		})
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.RefusedCount = 1
		})
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.RefusedCount = 2
		})
		if wait != c.first {
			t.Errorf("random %v: first = %s, want %s", c.random, wait, c.first)
		}
		post := harness.postAfter(wait, "the report")
		post.fail(errors.New("no answer"))
		wait = harness.requireParked("the retry")
		if wait != c.retry {
			t.Errorf("random %v: retry = %s, want %s", c.random, wait, c.retry)
		}
		post = harness.postAfter(wait, "the retry")
		post.answer(ExtenderCamouflageReportAccepted)
		if wait := harness.requireParked("the interval"); wait != c.next {
			t.Errorf("random %v: interval = %s, want %s", c.random, wait, c.next)
		}
		harness.reporter.Close()
	}
}

// A client with nothing to say makes no post, however long it runs, and what it
// comes to say goes out soon after.
func TestExtenderCamouflageReporterSendsNothingWithNothingToSay(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	for range 100 {
		harness.fireAfter(wait)
		wait = harness.requireParked("a look with nothing to say")
		if 15*time.Minute < wait {
			t.Fatalf("an idle look waited %s", wait)
		}
	}
	if stats := harness.reporter.Stats(); stats.ReportCount != 0 || stats.AttemptCount != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.RefusedCount = 1
	})
	post := harness.postAfter(wait, "the first thing to say")
	post.answer(ExtenderCamouflageReportAccepted)
	harness.requireParked("after it")
}

// The heart of it. A fake operator claims ids as the route does, and a post whose
// answer is lost has already been counted. Retried, it is the same id and the same
// bytes, so the total the operator holds is exactly what happened, whatever failed.
func TestExtenderCamouflageReporterRetryIsCountedOnce(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	operator := newTestCamouflageReportOperator()
	wait := harness.enable()

	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 5
	})
	post := harness.postAfter(wait, "the report")
	body := post.body
	id := post.args(t).ReportId
	// counted by the operator, and the answer never arrives
	operator.receive(t, post)
	post.fail(errors.New("the connection dropped after the request was sent"))

	wait = harness.requireParked("the first retry")
	if wait <= 0 {
		t.Fatalf("retry wait = %s", wait)
	}
	// what is counted while a report is outstanding waits for the next one
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 7
	})
	post = harness.postAfter(wait, "the retry")
	if string(post.body) != string(body) {
		t.Fatalf("the retry body differs:\n%s\n%s", post.body, body)
	}
	if post.args(t).ReportId != id {
		t.Errorf("the retry changed the id")
	}
	post.answer(operator.receive(t, post))

	wait = harness.requireParked("the next interval")
	post = harness.postAfter(wait, "the next report")
	if post.args(t).ReportId == id {
		t.Errorf("the next report kept the id")
	}
	post.answer(operator.receive(t, post))
	harness.requireParked("after the next report")

	if operator.bothFailed != 7 {
		t.Fatalf("the operator counted %d, and 7 happened", operator.bothFailed)
	}
}

// The route's claim and count, in the test's memory.
type testCamouflageReportOperator struct {
	claims     map[string]bool
	bothFailed int64
}

func newTestCamouflageReportOperator() *testCamouflageReportOperator {
	return &testCamouflageReportOperator{claims: map[string]bool{}}
}

func (self *testCamouflageReportOperator) receive(t *testing.T, post *testCamouflageReportPost) string {
	t.Helper()
	args := post.args(t)
	if self.claims[args.ReportId] {
		return ExtenderCamouflageReportDuplicate
	}
	self.claims[args.ReportId] = true
	if args.Client != nil {
		self.bothFailed += args.Client.BothFailed
	}
	return ExtenderCamouflageReportAccepted
}

// A report is posted at most four times, spaced by a doubling backoff, and then it
// is given up and lost: an id can't be merged into a later report without risking
// both being counted.
func TestExtenderCamouflageReporterGivesUpAfterBoundedRetries(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
		settings.Random = func() float64 { return 0 }
	})
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 1
	})
	wait := harness.enable()
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 2
	})
	retries := []time.Duration{}
	id := ""
	for attempt := range harness.settings.MaxAttemptCount {
		post := harness.postAfter(wait, fmt.Sprintf("post %d", attempt))
		args := post.args(t)
		if attempt == 0 {
			id = args.ReportId
		} else if args.ReportId != id {
			t.Fatalf("post %d changed the id", attempt)
		}
		post.fail(errors.New("no answer"))
		wait = harness.requireParked("after a failure")
		retries = append(retries, wait)
	}
	want := []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute,
		// given up: one report given up doubles the interval, with the most jitter
		// taken off
		2*time.Hour - 15*time.Minute,
	}
	if !slices.Equal(retries, want) {
		t.Errorf("waits = %v, want %v", retries, want)
	}
	stats := harness.reporter.Stats()
	if stats.AttemptCount != 4 || stats.GaveUpCount != 1 || stats.DeliveredCount != 0 {
		t.Errorf("stats = %+v", stats)
	}
	// the counts of the report given up do not come back, and the new ones do
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 9
	})
	post := harness.postAfter(wait, "the next report")
	if post.args(t).ReportId == id || post.args(t).Client.BothFailed != 7 {
		t.Errorf("the next report = %s", post.body)
	}
}

// An operator gone for a week costs a few posts every few hours, however long it
// stays gone, far under what the route's own budget allows.
func TestExtenderCamouflageReporterDoesNotStormWhileTheOperatorIsGone(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	counter := int64(0)
	start := harness.clock.Now()
	hourPosts := map[int64]int{}
	posts := 0
	for harness.clock.Now().Sub(start) < 7*24*time.Hour {
		counter += 1
		count := counter
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = count
		})
		harness.fireAfter(wait)
		event := harness.next()
		if event.post != nil {
			posts += 1
			hourPosts[harness.clock.Now().Sub(start).Milliseconds()/time.Hour.Milliseconds()] += 1
			event.post.fail(errors.New("no route"))
			event = harness.next()
		}
		if !event.parked {
			t.Fatalf("event = %+v", event)
		}
		wait = event.wait
	}
	// four posts per report, with six hours between reports once they settle
	if 7*24*4/6+8 < posts {
		t.Errorf("posts over a week = %d", posts)
	}
	for hour, count := range hourPosts {
		if extenderCamouflageTestRouteBudget < count {
			t.Errorf("hour %d had %d posts, over the route's budget", hour, count)
		}
	}
	if posts == 0 {
		t.Errorf("no posts at all")
	}
}

// The route's budget per client per hour (server extenderCamouflageReportRateLimit).
const extenderCamouflageTestRouteBudget = 12

// A rate limit waits the time the operator gave, bounded and jittered, and posts the
// same report again.
func TestExtenderCamouflageReporterWaitsOutARateLimit(t *testing.T) {
	for _, c := range []struct {
		retryAfterSeconds int
		want              time.Duration
	}{
		{retryAfterSeconds: 600, want: 10 * time.Minute},
		// below the floor and above the ceiling
		{retryAfterSeconds: 1, want: time.Minute},
		{retryAfterSeconds: 100000, want: time.Hour},
		{retryAfterSeconds: 0, want: time.Minute},
		{retryAfterSeconds: -5, want: time.Minute},
	} {
		harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
			settings.Random = func() float64 { return 0 }
		})
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.RefusedCount = 1
		})
		post := harness.postAfter(wait, "the report")
		id := post.args(t).ReportId
		post.reply <- &extenderCamouflagePostResult{
			responseBytes: []byte(fmt.Sprintf(`{"outcome":"rate_limited","retry_after_seconds":%d}`, c.retryAfterSeconds)),
		}
		wait = harness.requireParked("the rate limit")
		if wait != c.want {
			t.Errorf("retry after %d = %s, want %s", c.retryAfterSeconds, wait, c.want)
		}
		post = harness.postAfter(wait, "the retry")
		if post.args(t).ReportId != id {
			t.Errorf("the retry changed the id")
		}
		post.answer(ExtenderCamouflageReportAccepted)
		harness.requireParked("after")
		harness.reporter.Close()
	}
}

// An operator with the reports off, or one that does not serve the route, is asked
// again in hours, not minutes, and what was in the report is not retried.
func TestExtenderCamouflageReporterGoesAwayWhenRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(post *testCamouflageReportPost)
	}{
		{name: "disabled", answer: func(post *testCamouflageReportPost) { post.answer(ExtenderCamouflageReportDisabled) }},
		{name: "no route", answer: func(post *testCamouflageReportPost) {
			post.fail(&HttpStatusError{StatusCode: http.StatusNotFound, Status: "404 Not Found"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
				settings.Random = func() float64 { return 0.5 }
			})
			wait := harness.enable()
			harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
				counters.RefusedCount = 1
			})
			post := harness.postAfter(wait, "the report")
			c.answer(post)
			if wait := harness.requireParked("refused"); wait != 6*time.Hour {
				t.Errorf("wait = %s", wait)
			}
			stats := harness.reporter.Stats()
			if stats.AttemptCount != 1 || stats.RefusedCount != 1 || stats.GaveUpCount != 0 {
				t.Errorf("stats = %+v", stats)
			}
		})
	}
}

// What the operator will never take is dropped, and it slows the sender.
func TestExtenderCamouflageReporterDropsAnInvalidReport(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.RefusedCount = 1
	})
	post := harness.postAfter(wait, "the report")
	post.answer(ExtenderCamouflageReportInvalid)
	if wait := harness.requireParked("dropped"); wait != 2*time.Hour {
		t.Errorf("wait = %s, want one given up's doubled interval", wait)
	}
	if stats := harness.reporter.Stats(); stats.AttemptCount != 1 || stats.GaveUpCount != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

// Every answer that cannot be read as counted or refused is a retry of the same
// report: the operator may have counted it, and the id makes that safe.
func TestExtenderCamouflageReporterRetriesAnAnswerItCannotRead(t *testing.T) {
	for name, reply := range map[string]*extenderCamouflagePostResult{
		"unavailable":     {responseBytes: []byte(`{"outcome":"unavailable"}`)},
		"a later outcome": {responseBytes: []byte(`{"outcome":"from_the_future"}`)},
		"no outcome":      {responseBytes: []byte(`{}`)},
		"not json":        {responseBytes: []byte(`<html>`)},
		"a server error":  {err: &HttpStatusError{StatusCode: http.StatusServiceUnavailable, Status: "503"}},
		"not authorized":  {err: &HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401"}},
		"a timeout":       {err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newTestCamouflageReportHarness(t, nil)
			wait := harness.enable()
			harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
				counters.RefusedCount = 1
			})
			post := harness.postAfter(wait, "the report")
			id := post.args(t).ReportId
			post.reply <- reply
			wait = harness.requireParked("a retry")
			if harness.settings.RetryMaxBackoff < wait {
				t.Errorf("wait = %s, want a retry's", wait)
			}
			post = harness.postAfter(wait, "the retry")
			if post.args(t).ReportId != id {
				t.Errorf("the retry changed the id")
			}
			post.answer(ExtenderCamouflageReportAccepted)
			harness.requireParked("after")
		})
	}
}

// A report is not retried once the operator may have forgotten its id, which a long
// wait from the operator or a sleeping device would otherwise do.
func TestExtenderCamouflageReporterNeverRetriesPastItsAge(t *testing.T) {
	t.Run("a retry time past its age", func(t *testing.T) {
		harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
			settings.Random = func() float64 { return 0 }
			settings.MaxReportAge = 90 * time.Minute
		})
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.RefusedCount = 1
		})
		// a second limit of an hour would post past the age, so it gives up at once
		post := harness.postAfter(wait, "the report")
		rateLimit := []byte(`{"outcome":"rate_limited","retry_after_seconds":3600}`)
		post.reply <- &extenderCamouflagePostResult{responseBytes: rateLimit}
		post = harness.postAfter(harness.requireParked("the limit"), "the retry")
		post.reply <- &extenderCamouflagePostResult{responseBytes: rateLimit}
		// given up: the interval after one given up, not the hour of a limit
		if wait := harness.requireParked("given up"); wait != 2*time.Hour-15*time.Minute {
			t.Errorf("wait = %s", wait)
		}
		stats := harness.reporter.Stats()
		if stats.AttemptCount != 2 || stats.GaveUpCount != 1 {
			t.Errorf("stats = %+v", stats)
		}
	})
	t.Run("a device that slept through its retry", func(t *testing.T) {
		harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
			settings.Random = func() float64 { return 0 }
		})
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.RefusedCount = 1
		})
		post := harness.postAfter(wait, "the report")
		post.fail(errors.New("no answer"))
		harness.requireParked("the retry")

		// the timer fires eight hours late: no post of the old id
		harness.fireAfter(8 * time.Hour)
		harness.requireParked("given up without a post")
		stats := harness.reporter.Stats()
		if stats.AttemptCount != 1 || stats.GaveUpCount != 1 {
			t.Errorf("stats = %+v", stats)
		}
	})
	t.Run("the age reads the wall clock", func(t *testing.T) {
		// a reading with a monotonic part and one without compare by the wall, which is
		// what a monotonic clock that stopped during a sleep leaves as the longer age.
		// The stopped clock itself cannot be made in a test.
		createTime := time.Now()
		pending := &extenderCamouflagePendingReport{createTime: createTime}
		now := createTime.Add(time.Minute)
		if age := pending.age(now); age != time.Minute {
			t.Errorf("age = %s", age)
		}
		if age := pending.age(now.Round(0).Add(8 * time.Hour)); age < 8*time.Hour {
			t.Errorf("age by the wall = %s", age)
		}
	})
}

// The account change: the post that is out is canceled, what it would have said is
// not read, the report is dropped and so is everything counted so far. What the next
// identity sends is only what it did.
func TestExtenderCamouflageReporterAccountChangeDropsEverything(t *testing.T) {
	t.Run("with a post out", func(t *testing.T) {
		harness := newTestCamouflageReportHarness(t, nil)
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = 5
		})
		post := harness.postAfter(wait, "the report")
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = 8
		})
		harness.changeAccount("other-token")
		select {
		case <-post.ctx.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the post was not canceled")
		}
		wait = harness.requireParked("the new identity's first look")
		if stats := harness.reporter.Stats(); stats.DroppedCount != 1 || stats.DeliveredCount != 0 {
			t.Errorf("stats = %+v", stats)
		}

		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = 9
		})
		post = harness.postAfter(wait, "the new identity's report")
		if post.byJwt != "other-token" {
			t.Errorf("credential = %q", post.byJwt)
		}
		if count := post.args(t).Client.BothFailed; count != 1 {
			t.Errorf("the new identity reported %d, and did 1", count)
		}
	})
	t.Run("with an answer on its way", func(t *testing.T) {
		harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
			// a route that answers what it was answered, a cancel notwithstanding
			next := settings.Post
			settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
				return next(context.WithoutCancel(ctx), requestUrl, requestBodyBytes, byJwt)
			}
		})
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = 5
		})
		post := harness.postAfter(wait, "the report")
		harness.changeAccount("other-token")
		post.answer(ExtenderCamouflageReportAccepted)
		harness.requireParked("the new identity's first look")
		stats := harness.reporter.Stats()
		if stats.DeliveredCount != 0 || stats.DroppedCount != 1 {
			t.Errorf("the old identity's answer was read: %+v", stats)
		}
	})
	t.Run("with the answer and the change together", func(t *testing.T) {
		// the change lands as the answer does, so the wait may see either first, and
		// whichever it was the next identity reports only what it did
		var harness *testCamouflageReportHarness
		passCount := 0
		harness = newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
			next := settings.Post
			settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
				responseBytes, err := next(context.WithoutCancel(ctx), requestUrl, requestBodyBytes, byJwt)
				harness.changeAccount(fmt.Sprintf("token-%d", passCount))
				return responseBytes, err
			}
		})
		wait := harness.enable()
		for passCount = range 16 {
			count := int64(passCount + 1)
			harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
				counters.BothFailedCount = count
			})
			post := harness.postAfter(wait, "the report")
			if reported := post.args(t).Client.BothFailed; reported != 1 {
				t.Errorf("pass %d reported %d, and this identity did 1", passCount, reported)
			}
			post.answer(ExtenderCamouflageReportAccepted)
			wait = harness.requireParked("the next identity's first look")
		}
	})
	t.Run("in a backoff", func(t *testing.T) {
		harness := newTestCamouflageReportHarness(t, nil)
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = 5
		})
		post := harness.postAfter(wait, "the report")
		id := post.args(t).ReportId
		post.fail(errors.New("no answer"))
		harness.requireParked("the retry")
		harness.changeAccount("other-token")
		wait = harness.requireParked("the new identity's first look")
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = 6
		})
		post = harness.postAfter(wait, "the new identity's report")
		args := post.args(t)
		if args.ReportId == id || args.Client.BothFailed != 1 {
			t.Errorf("report = %s", post.body)
		}
	})
	t.Run("an extender's counts too", func(t *testing.T) {
		harness := newTestCamouflageReportHarness(t, nil)
		source := &testCamouflageReportSource{counts: &ExtenderCamouflageExtenderCounts{Spliced: 5}}
		harness.reporter.SetSource(source)
		harness.enable()
		discarded := source.discarded
		harness.changeAccount("other-token")
		harness.requireParked("the new identity's first look")
		if source.discarded <= discarded {
			t.Errorf("discarded %d times", source.discarded)
		}
	})
}

// Signed out, nothing is sent and nothing is scheduled, and what was counted then is
// not sent by whoever signs in.
func TestExtenderCamouflageReporterSendsNothingWithoutAnIdentity(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 5
	})
	post := harness.postAfter(wait, "the report")
	post.fail(errors.New("no answer"))
	harness.requireParked("the retry")

	harness.changeAccount("")
	harness.requireInactive("signed out")
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 9
	})
	harness.changeAccount("other-token")
	wait = harness.requireParked("signed in")
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 10
	})
	post = harness.postAfter(wait, "the new identity's report")
	if count := post.args(t).Client.BothFailed; count != 1 {
		t.Errorf("reported %d, and did 1", count)
	}
}

// Off cancels the post that is out and drops the report, and what is counted while
// off never goes out.
func TestExtenderCamouflageReporterTurnedOffDropsAndForgets(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 5
	})
	post := harness.postAfter(wait, "the report")

	harness.reporter.SetEnabled(false)
	select {
	case <-post.ctx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the post was not canceled")
	}
	harness.requireInactive("off")
	if stats := harness.reporter.Stats(); stats.DroppedCount != 1 {
		t.Errorf("stats = %+v", stats)
	}
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 9
	})

	harness.reporter.SetEnabled(true)
	wait = harness.requireParked("on again")
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.BothFailedCount = 10
	})
	post = harness.postAfter(wait, "the next report")
	if count := post.args(t).Client.BothFailed; count != 1 {
		t.Errorf("reported %d, and 1 happened since it came back on", count)
	}
}

// The client counters are process-wide and a process may hold several spaces, each
// with a sender. They share the ledger, so whichever takes a count is the only one
// that has it. A sender that restarts in the process reads the same ledger.
func TestExtenderCamouflageReporterSendersShareTheLedger(t *testing.T) {
	counters := &ExtenderTcpCamouflageClientStats{}
	ledger := &extenderCamouflageClientLedger{snapshot: func() ExtenderTcpCamouflageClientStats {
		return *counters
	}}
	counters.BothFailedCount = 5
	first := ledger.take()
	second := ledger.take()
	if first == nil || first.BothFailed != 5 || second != nil {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}

	configure := func(settings *ExtenderCamouflageReporterSettings) {
		settings.clientLedger = ledger
		settings.Random = func() float64 { return 0 }
	}
	a := newTestCamouflageReportHarness(t, configure)
	b := newTestCamouflageReportHarness(t, configure)
	waitA := a.enable()
	waitB := b.enable()
	counters.BothFailedCount = 12
	post := a.postAfter(waitA, "the first sender")
	if count := post.args(t).Client.BothFailed; count != 7 {
		t.Errorf("the first sender reported %d", count)
	}
	b.fireAfter(waitB)
	if wait := b.requireParked("the second sender has nothing to say"); wait <= 0 {
		t.Errorf("wait = %s", wait)
	}
	post.answer(ExtenderCamouflageReportAccepted)
	a.requireParked("after")
}

// A restart is new counters and a new ledger together: what the old process took or
// did not take is gone, and the new one reports only what it has counted.
func TestExtenderCamouflageReporterRestartReportsOnlyTheNewProcess(t *testing.T) {
	operator := newTestCamouflageReportOperator()
	run := func(bothFailed int64, answer bool) {
		harness := newTestCamouflageReportHarness(t, nil)
		defer harness.reporter.Close()
		wait := harness.enable()
		harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
			counters.BothFailedCount = bothFailed
		})
		post := harness.postAfter(wait, "the report")
		// the same ids in each process would be a bug the operator would catch; make
		// each process's id its own
		post.args(t).ReportId = ""
		args := post.args(t)
		if args.Client.BothFailed != bothFailed {
			t.Errorf("reported %d, and the process counted %d", args.Client.BothFailed, bothFailed)
		}
		operator.bothFailed += args.Client.BothFailed
		if answer {
			post.answer(ExtenderCamouflageReportAccepted)
			harness.requireParked("after")
		}
	}
	// the first process reports, and dies with a post out
	run(5, true)
	run(3, false)
	if operator.bothFailed != 8 {
		t.Errorf("counted %d", operator.bothFailed)
	}
}

// A count past the operator's bound is held for the next report, not refused or lost.
func TestExtenderCamouflageReporterHoldsTheExcessForTheNextReport(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	bound := ExtenderCamouflageReportMaxClientCount
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.LegacyOnlyNoKeyCount = 2*bound + 3
		counters.RefusedCount = 1
	})
	post := harness.postAfter(wait, "the first report")
	if got := post.args(t).Client; got.LegacyOnlyNoKey != bound || got.Refused != 1 {
		t.Errorf("first = %+v", got)
	}
	post.answer(ExtenderCamouflageReportAccepted)
	post = harness.postAfter(harness.requireParked("the interval"), "the second report")
	if got := post.args(t).Client; got.LegacyOnlyNoKey != bound || got.Refused != 0 {
		t.Errorf("second = %+v", got)
	}
	post.answer(ExtenderCamouflageReportAccepted)
	post = harness.postAfter(harness.requireParked("the interval"), "the third report")
	if got := post.args(t).Client; got.LegacyOnlyNoKey != 3 {
		t.Errorf("third = %+v", got)
	}
	post.answer(ExtenderCamouflageReportAccepted)
	harness.requireParked("after")
}

// An extender reports each interval, a verdict with nothing counted among them, and
// what it took goes out once.
func TestExtenderCamouflageReporterReportsAnExtendersPartAndVerdict(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	source := &testCamouflageReportSource{}
	harness.reporter.SetSource(source)
	wait := harness.enable()
	source.stateLock.Lock()
	source.counts = &ExtenderCamouflageExtenderCounts{
		Spliced:        5,
		Targets:        []*ExtenderCamouflageTargetCounts{{Name: "borrowed.example", SpliceCount: 5, RelayedByteCount: 500}},
		Covered:        true,
		DegradedReason: "",
	}
	source.stateLock.Unlock()

	post := harness.postAfter(wait, "the first report")
	args := post.args(t)
	if args.Client != nil {
		t.Errorf("client part = %+v, with nothing counted", args.Client)
	}
	if args.Extender == nil || args.Extender.Spliced != 5 || !args.Extender.Covered || len(args.Extender.Targets) != 1 {
		t.Errorf("extender part = %s", post.body)
	}
	post.answer(ExtenderCamouflageReportAccepted)

	post = harness.postAfter(harness.requireParked("the interval"), "a report with nothing counted")
	args = post.args(t)
	if args.Extender == nil || args.Extender.Spliced != 0 || args.Extender.DegradedReason != "splice_off" {
		t.Errorf("verdict report = %s", post.body)
	}
	post.answer(ExtenderCamouflageReportAccepted)
	wait = harness.requireParked("the interval")

	// a role that closes takes its source with it, unless another took over
	other := &testCamouflageReportSource{}
	harness.reporter.ClearSource(other)
	if harness.reporter.currentSource() != ExtenderCamouflageReportSource(source) {
		t.Errorf("another role's clear removed the source")
	}
	harness.reporter.ClearSource(source)
	harness.fireAfter(wait)
	harness.requireParked("no source and nothing to say")
}

// The wire is exactly what the operator's route reads.
func TestExtenderCamouflageReportArgsAreTheRoutesContract(t *testing.T) {
	args := &ExtenderCamouflageReportArgs{
		ReportId:        "0123456789abcdef0123456789abcdef",
		ClientVersion:   "2026.10.30-1085916410",
		ResourceVersion: 3,
		IpVersions:      []int{4, 6},
		Client: &ExtenderCamouflageClientCounts{
			CamouflageWon: 1, LegacyWon: 2, BothFailed: 3, Refused: 4,
			CamouflageUnauthenticated: 5, CamouflageLeafRejected: 6,
			LegacyOnlyNoKey: 7, LegacyOnlyFingerprint: 8, LegacyOnlyNoFrontName: 9,
		},
		Extender: &ExtenderCamouflageExtenderCounts{
			Authenticated: 11, Spliced: 12, Terminated: 13, SpliceFallbackTerminated: 14,
			ReplayRefused: 15, TimeWindowFailed: 16, SpliceDialFailed: 17, BorrowVerifyFailed: 18,
			Targets: []*ExtenderCamouflageTargetCounts{
				{Name: "Listed.Example", SpliceCount: 21, RelayedByteCount: 2100},
				{Name: "unlisted.example", SpliceCount: 22, RelayedByteCount: 2200},
			},
			DegradedReason: "splice_off",
		},
	}
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{}
	if err := json.Unmarshal([]byte(testCamouflageReportContractBody), &want); err != nil {
		t.Fatal(err)
	}
	// covered false is its absence on the wire, and the route reads the two the same
	delete(want["extender"].(map[string]any), "covered")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire:\n%s\nwant:\n%s", body, testCamouflageReportContractBody)
	}
}

// A report holds what the contract names and nothing else: no field exists for an
// address, a key, an id or a credential, whatever the sender knows. Walks every json
// name of the types, so a field added later has to be added here on purpose.
func TestExtenderCamouflageReportCarriesNoIdentifier(t *testing.T) {
	allowed := map[string][]string{
		"report": {"report_id", "client_version", "resource_version", "ip_versions", "client", "extender"},
		"client": {
			"camouflage_won", "legacy_won", "both_failed", "refused",
			"camouflage_unauthenticated", "camouflage_leaf_rejected",
			"legacy_only_no_key", "legacy_only_fingerprint", "legacy_only_no_front_name",
		},
		"extender": {
			"authenticated", "spliced", "terminated", "splice_fallback_terminated",
			"replay_refused", "time_window_failed", "splice_dial_failed", "borrow_verify_failed",
			"targets", "covered", "degraded_reason",
		},
		"target": {"name", "splice_count", "relayed_byte_count"},
	}
	for name, value := range map[string]any{
		"report":   ExtenderCamouflageReportArgs{},
		"client":   ExtenderCamouflageClientCounts{},
		"extender": ExtenderCamouflageExtenderCounts{},
		"target":   ExtenderCamouflageTargetCounts{},
	} {
		valueType := reflect.TypeOf(value)
		fields := []string{}
		for i := range valueType.NumField() {
			fields = append(fields, strings.Split(valueType.Field(i).Tag.Get("json"), ",")[0])
		}
		slices.Sort(fields)
		want := slices.Clone(allowed[name])
		slices.Sort(want)
		if !slices.Equal(fields, want) {
			t.Errorf("%s fields = %v, want %v", name, fields, want)
		}
	}

	// and a sender given a token and a build string past its bound sends neither
	harness := newTestCamouflageReportHarness(t, func(settings *ExtenderCamouflageReporterSettings) {
		settings.ClientVersion = func() string { return strings.Repeat("v", 100) }
		settings.IpVersions = func() []int { return []int{4, 4, 5, 6, 6, 7} }
		settings.ResourceVersion = func() int { return -1 }
	})
	wait := harness.enable()
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.RefusedCount = 1
	})
	post := harness.postAfter(wait, "the report")
	if strings.Contains(string(post.body), "test-token") {
		t.Errorf("the credential is in the body: %s", post.body)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(post.body, &fields); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"client", "ip_versions", "report_id"}) {
		t.Errorf("fields = %v", keys)
	}
	if args := post.args(t); !slices.Equal(args.IpVersions, []int{4, 6}) {
		t.Errorf("families = %v", args.IpVersions)
	}
}

// The real counters read through their ledger, with a sender on the process's own.
func TestExtenderCamouflageClientLedgerReadsTheProcessCounters(t *testing.T) {
	ledger := &extenderCamouflageClientLedger{snapshot: ExtenderTcpCamouflageClientStatsSnapshot}
	ledger.discard()
	extenderTcpCamouflageRefusedCount.Add(3)
	counts := ledger.take()
	if counts == nil || counts.Refused < 3 {
		t.Errorf("counts = %+v", counts)
	}
	if defaultExtenderCamouflageClientLedger.snapshot == nil {
		t.Errorf("the process's ledger reads nothing")
	}
}

// Close joins the loop and cancels the post that is out.
func TestExtenderCamouflageReporterCloseCancelsThePost(t *testing.T) {
	harness := newTestCamouflageReportHarness(t, nil)
	wait := harness.enable()
	harness.addCounts(func(counters *ExtenderTcpCamouflageClientStats) {
		counters.RefusedCount = 1
	})
	post := harness.postAfter(wait, "the report")
	harness.reporter.Close()
	select {
	case <-post.ctx.Done():
	default:
		t.Errorf("the post is still out after Close")
	}
	// idempotent
	harness.reporter.Close()
}

// The ledger takes what is new, within a bound, and a discard takes it unsaid.
func TestExtenderCamouflageCountLedger(t *testing.T) {
	ledger := &ExtenderCamouflageCountLedger{}
	if pending := ledger.Pending(10); pending != 10 {
		t.Errorf("pending = %d", pending)
	}
	if count := ledger.Take(10, 4); count != 4 {
		t.Errorf("take = %d", count)
	}
	if count := ledger.Take(10, 100); count != 6 {
		t.Errorf("take = %d", count)
	}
	if count := ledger.Take(10, 100); count != 0 {
		t.Errorf("take = %d", count)
	}
	ledger.Discard(25)
	if count := ledger.Take(26, 100); count != 1 {
		t.Errorf("take after a discard = %d", count)
	}
	// a counter that went back is nothing new, and takes nothing back
	if count := ledger.Take(5, 100); count != 0 {
		t.Errorf("take of a lower counter = %d", count)
	}
	ledger.Discard(5)
	if count := ledger.Take(27, 100); count != 1 {
		t.Errorf("take after a lower discard = %d", count)
	}
}
