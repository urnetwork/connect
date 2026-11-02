package connect

// net_extender_camouflage_report.go — the sender of the camouflage report
// (EXTENDER-UPDATE2 §3.3, rollout gates; EXTENDER-SPLICE-ROLLOUT.md, Reporting
// path).
//
// The client's tcp camouflage counters and an extender's are process memory, so
// the numeric rollout gates could not be read across a fleet. This posts what each
// has counted since it last reported to the operator, which adds it to Prometheus
// counters and keeps nothing else. A client and an extender run the same sender: an
// extender also has a source (SetSource), and a client has none.
//
// What is sent is counts, its build, the version of the borrow list it reads and
// the address families it dials, and for an extender the borrowed names it spliced
// to, which are public sites of the bundled list, and its coverage verdict. It is
// never a gated address, a key, a client id or a network id. The credential on the
// post authenticates it and spends a rate budget, and the operator records neither.
//
// When. One loop makes one post at a time, so everything is coalesced by
// construction. The first look is FirstInterval after the sender starts, jittered,
// so a process that fails its first dials and exits soon after still has its say. A
// look with nothing to say costs nothing, sends nothing and looks again that soon. A
// report that was delivered waits Interval, jittered, for the next. A client that
// never dials an extender never posts; an extender always has a verdict to give.
//
// Failure. One report is posted at most MaxAttemptCount times, with a doubling,
// jittered backoff, and for at most MaxReportAge. Then it is given up, and each
// report given up in a row doubles the wait for the next, to MaxInterval, so an
// operator that cannot be reached costs a few posts every few hours and a fleet that
// lost it does not return in step. An answer that says the reports are off waits
// DisabledWait.
//
// Counted once. Deltas are taken from a ledger that sits beside the counters it reads
// and has the same lifetime, and they are taken once:
//
//   - A retry posts the same report id and the same bytes. The operator claims an id
//     once, for longer than MaxReportAge, so a retry of a post whose answer was lost
//     is answered duplicate and adds nothing. Nothing new joins a report: what is
//     counted while one is outstanding waits for the next, under a new id.
//   - A restart loses what was not delivered, and sends it no more. The client's
//     counters and its ledger are both process memory, and an extender's are its
//     server's, so both start at zero together, and no state is kept across a
//     restart that could send a count twice.
//   - Two senders in one process share the ledger, so whichever takes a count is the
//     only one that has it: the client counters are process-wide and a process may
//     hold several network spaces.
//
// So every failure undercounts and none overcounts. A report given up is lost, a
// choice the gates can live with, since an id cannot be merged into a later report
// without risking both being counted. The excess past a bound stays in the ledger
// for the next report rather than being refused or lost.
//
// An account change (AccountMonitor) cancels the post that is out, ignores its answer,
// and drops the pending report and everything counted so far: what one identity
// did is never sent under the next. So does turning the sender off and on, so
// nothing from while it was off is sent. It starts off (SetEnabled).
//
// Nothing here logs above V(1), and what it logs is an outcome from a fixed set.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// The route, appended to the api url.
const ExtenderCamouflageReportPath = "/network/extender-camouflage-report"

// The operator's answers.
const (
	// counted now
	ExtenderCamouflageReportAccepted = "accepted"
	// counted by an earlier post of the same id
	ExtenderCamouflageReportDuplicate = "duplicate"
	// not counted; post again after retry_after_seconds
	ExtenderCamouflageReportRateLimited = "rate_limited"
	// not counted; the operator has the reports off
	ExtenderCamouflageReportDisabled = "disabled"
	// not counted, and another post of this report would not be either
	ExtenderCamouflageReportInvalid = "invalid"
	// not counted; post again later
	ExtenderCamouflageReportUnavailable = "unavailable"
)

// The operator's bounds on one report, which the sender stays inside. Past a count's
// bound the excess waits for the next report.
const (
	ExtenderCamouflageReportMaxClientCount         int64 = 1 << 16
	ExtenderCamouflageReportMaxExtenderCount       int64 = 1 << 26
	ExtenderCamouflageReportMaxTargetSpliceCount   int64 = 1 << 24
	ExtenderCamouflageReportMaxTargetByteCount     int64 = 1 << 44
	ExtenderCamouflageReportMaxTargetCount               = 32
	ExtenderCamouflageReportMaxTargetNameByteCount       = 253
	// the operator's bound on client_version
	extenderCamouflageReportMaxClientVersionByteCount = 64
)

// What a client's tcp camouflage came to since its last report, one field per
// counter of ExtenderTcpCamouflageClientStats.
type ExtenderCamouflageClientCounts struct {
	CamouflageWon             int64 `json:"camouflage_won,omitempty"`
	LegacyWon                 int64 `json:"legacy_won,omitempty"`
	BothFailed                int64 `json:"both_failed,omitempty"`
	Refused                   int64 `json:"refused,omitempty"`
	CamouflageUnauthenticated int64 `json:"camouflage_unauthenticated,omitempty"`
	CamouflageLeafRejected    int64 `json:"camouflage_leaf_rejected,omitempty"`
	LegacyOnlyNoKey           int64 `json:"legacy_only_no_key,omitempty"`
	LegacyOnlyFingerprint     int64 `json:"legacy_only_fingerprint,omitempty"`
	LegacyOnlyNoFrontName     int64 `json:"legacy_only_no_front_name,omitempty"`
}

// One borrowed site's splice load on an extender since its last report.
type ExtenderCamouflageTargetCounts struct {
	Name             string `json:"name"`
	SpliceCount      int64  `json:"splice_count,omitempty"`
	RelayedByteCount int64  `json:"relayed_byte_count,omitempty"`
}

// How an extender classified its tcp connections since its last report, one field
// per counter of extender.ExtenderCamouflageStats, and its coverage verdict as of
// the report (extender.ExtenderCamouflageStatus).
type ExtenderCamouflageExtenderCounts struct {
	Authenticated            int64                             `json:"authenticated,omitempty"`
	Spliced                  int64                             `json:"spliced,omitempty"`
	Terminated               int64                             `json:"terminated,omitempty"`
	SpliceFallbackTerminated int64                             `json:"splice_fallback_terminated,omitempty"`
	ReplayRefused            int64                             `json:"replay_refused,omitempty"`
	TimeWindowFailed         int64                             `json:"time_window_failed,omitempty"`
	SpliceDialFailed         int64                             `json:"splice_dial_failed,omitempty"`
	BorrowVerifyFailed       int64                             `json:"borrow_verify_failed,omitempty"`
	Targets                  []*ExtenderCamouflageTargetCounts `json:"targets,omitempty"`
	Covered                  bool                              `json:"covered,omitempty"`
	DegradedReason           string                            `json:"degraded_reason,omitempty"`
}

// One report as it is posted. The json is the operator's contract.
type ExtenderCamouflageReportArgs struct {
	// 32 lower case hex digits, random, the same across the retries of one report
	ReportId string `json:"report_id"`
	// the sender's build
	ClientVersion string `json:"client_version,omitempty"`
	// the version of the borrow list the sender reads, zero for none
	ResourceVersion int `json:"resource_version,omitempty"`
	// the families the sender dials extenders of; empty is any
	IpVersions []int                             `json:"ip_versions,omitempty"`
	Client     *ExtenderCamouflageClientCounts   `json:"client,omitempty"`
	Extender   *ExtenderCamouflageExtenderCounts `json:"extender,omitempty"`
}

// The operator's answer.
type ExtenderCamouflageReportResult struct {
	Outcome           string `json:"outcome"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// ExtenderCamouflageCountLedger is how much of one cumulative counter has been taken.
// The zero value has taken nothing. Not safe for concurrent use: its owner holds the
// lock it holds its other counts under.
type ExtenderCamouflageCountLedger struct {
	taken int64
}

// Pending is what the counter has added since the last take.
func (self *ExtenderCamouflageCountLedger) Pending(current int64) int64 {
	return max(current-self.taken, 0)
}

// Take takes what the counter has added since the last take, at most maxCount, and
// leaves the rest for the next.
func (self *ExtenderCamouflageCountLedger) Take(current int64, maxCount int64) int64 {
	count := min(self.Pending(current), maxCount)
	self.taken += count
	return count
}

// Discard takes everything without saying so: what the counter holds now is never
// reported.
func (self *ExtenderCamouflageCountLedger) Discard(current int64) {
	self.taken = max(current, self.taken)
}

// What the process's client counters have reported, which lives with the counters.
type extenderCamouflageClientLedger struct {
	snapshot func() ExtenderTcpCamouflageClientStats

	stateLock                 sync.Mutex
	camouflageWon             ExtenderCamouflageCountLedger
	legacyWon                 ExtenderCamouflageCountLedger
	bothFailed                ExtenderCamouflageCountLedger
	refused                   ExtenderCamouflageCountLedger
	camouflageUnauthenticated ExtenderCamouflageCountLedger
	camouflageLeafRejected    ExtenderCamouflageCountLedger
	legacyOnlyNoKey           ExtenderCamouflageCountLedger
	legacyOnlyFingerprint     ExtenderCamouflageCountLedger
	legacyOnlyNoFrontName     ExtenderCamouflageCountLedger
}

var defaultExtenderCamouflageClientLedger = &extenderCamouflageClientLedger{
	snapshot: ExtenderTcpCamouflageClientStatsSnapshot,
}

// What the counters have added since the last take, nil when nothing.
func (self *extenderCamouflageClientLedger) take() *ExtenderCamouflageClientCounts {
	stats := self.snapshot()
	maxCount := ExtenderCamouflageReportMaxClientCount

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	counts := &ExtenderCamouflageClientCounts{
		CamouflageWon:             self.camouflageWon.Take(stats.CamouflageWonCount, maxCount),
		LegacyWon:                 self.legacyWon.Take(stats.LegacyWonCount, maxCount),
		BothFailed:                self.bothFailed.Take(stats.BothFailedCount, maxCount),
		Refused:                   self.refused.Take(stats.RefusedCount, maxCount),
		CamouflageUnauthenticated: self.camouflageUnauthenticated.Take(stats.CamouflageUnauthenticatedCount, maxCount),
		CamouflageLeafRejected:    self.camouflageLeafRejected.Take(stats.CamouflageLeafRejectedCount, maxCount),
		LegacyOnlyNoKey:           self.legacyOnlyNoKey.Take(stats.LegacyOnlyNoKeyCount, maxCount),
		LegacyOnlyFingerprint:     self.legacyOnlyFingerprint.Take(stats.LegacyOnlyFingerprintCount, maxCount),
		LegacyOnlyNoFrontName:     self.legacyOnlyNoFrontName.Take(stats.LegacyOnlyNoFrontNameCount, maxCount),
	}
	if *counts == (ExtenderCamouflageClientCounts{}) {
		return nil
	}
	return counts
}

// Everything counted so far is never reported.
func (self *extenderCamouflageClientLedger) discard() {
	stats := self.snapshot()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.camouflageWon.Discard(stats.CamouflageWonCount)
	self.legacyWon.Discard(stats.LegacyWonCount)
	self.bothFailed.Discard(stats.BothFailedCount)
	self.refused.Discard(stats.RefusedCount)
	self.camouflageUnauthenticated.Discard(stats.CamouflageUnauthenticatedCount)
	self.camouflageLeafRejected.Discard(stats.CamouflageLeafRejectedCount)
	self.legacyOnlyNoKey.Discard(stats.LegacyOnlyNoKeyCount)
	self.legacyOnlyFingerprint.Discard(stats.LegacyOnlyFingerprintCount)
	self.legacyOnlyNoFrontName.Discard(stats.LegacyOnlyNoFrontNameCount)
}

// ExtenderCamouflageReportSource is one running extender's part of the report.
// extender.ExtenderServer is one. Its ledger lives with its counters, so a server
// that restarts starts both at zero, and two senders never send one count.
type ExtenderCamouflageReportSource interface {
	// What the extender has counted since the last take, and its verdict now. Never
	// nil: an extender has a verdict to give even with nothing counted.
	TakeCamouflageReportCounts() *ExtenderCamouflageExtenderCounts
	// Everything counted so far is never reported.
	DiscardCamouflageReportCounts()
}

// The sender's numbers and seams.
type ExtenderCamouflageReporterSettings struct {
	Log Logger

	// The plain api url the report is posted to.
	ApiUrl string
	// The credential of the identity reporting, read at each look and each post so a
	// refresh is picked up. Empty is no identity, and nothing is sent.
	ByJwt func() string
	// The post.
	Post HttpPostRawFunction
	// Advances on every change of the identity. Nil for an identity that never
	// changes.
	AccountMonitor *MonitorValue[uint64]

	// The sender's build, and the version of the borrow list it reads, each read
	// when a report is made.
	ClientVersion   func() string
	ResourceVersion func() int
	// The families the sender dials extenders of, read when a report is made.
	IpVersions func() []int

	// The wait before the first look, and after a look with nothing to say: at least
	// FirstInterval and at most FirstIntervalJitter more.
	FirstInterval       time.Duration
	FirstIntervalJitter time.Duration
	// The wait after a report ends, within IntervalJitter either way.
	Interval       time.Duration
	IntervalJitter time.Duration
	// The wait after reports given up in a row doubles to this.
	MaxInterval time.Duration
	// The most posts of one report.
	MaxAttemptCount int
	// How long one report is posted for, which must stay under the time the operator
	// holds a report id claimed, three hours. A retry after that could count twice.
	MaxReportAge time.Duration
	// The backoff between posts of one report, doubling, with up to a quarter more.
	RetryMinBackoff time.Duration
	RetryMaxBackoff time.Duration
	// The bounds on a retry time the operator gives.
	MinRetryAfter time.Duration
	MaxRetryAfter time.Duration
	// The wait after an answer that says the reports are off, or that the route is
	// not served, within IntervalJitter either way.
	DisabledWait time.Duration
	// The deadline of one post.
	RequestTimeout time.Duration

	// The only clock the sender reads.
	Now func() time.Time
	// The wait between two looks.
	After func(wait time.Duration) <-chan time.Time
	// A uniform [0, 1) for every jitter.
	Random func() float64
	// A fresh report id: 32 lower case hex digits.
	NewReportId func() string

	// The ledger of the client counters, the process's unless a test gives its own.
	clientLedger *extenderCamouflageClientLedger
	// Called each time the loop waits with nothing scheduled.
	onInactive func()
}

// DefaultExtenderCamouflageReporterSettings: an hourly report, looked for 5 to 15
// minutes after a start; four posts of one report over at most two hours; a wait of up
// to six hours while reports are given up or off.
func DefaultExtenderCamouflageReporterSettings() *ExtenderCamouflageReporterSettings {
	return &ExtenderCamouflageReporterSettings{
		ClientVersion:       BuildVersion,
		ResourceVersion:     func() int { return extenderCamouflageBorrowListVersion("") },
		IpVersions:          ExtenderDialIpVersions,
		FirstInterval:       5 * time.Minute,
		FirstIntervalJitter: 10 * time.Minute,
		Interval:            time.Hour,
		IntervalJitter:      15 * time.Minute,
		MaxInterval:         6 * time.Hour,
		MaxAttemptCount:     4,
		MaxReportAge:        2 * time.Hour,
		RetryMinBackoff:     5 * time.Minute,
		RetryMaxBackoff:     30 * time.Minute,
		MinRetryAfter:       time.Minute,
		MaxRetryAfter:       time.Hour,
		DisabledWait:        6 * time.Hour,
		RequestTimeout:      30 * time.Second,
		Now:                 time.Now,
		After:               time.After,
		Random:              extenderCamouflageReportRandom,
		NewReportId:         newExtenderCamouflageReportId,
	}
}

// The version of the borrow list a client in a country selects from, zero for none.
func extenderCamouflageBorrowListVersion(countryCode string) int {
	borrowList := ExtenderCamouflageBorrowListForCountry(countryCode)
	if borrowList == nil {
		return 0
	}
	return borrowList.Version
}

// A uniform [0, 1) from the system's random source, so a fleet's jitter is not in
// step whatever the state of a package's generator.
func extenderCamouflageReportRandom() float64 {
	var randomBytes [2]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return 0.5
	}
	return float64(uint16(randomBytes[0])<<8|uint16(randomBytes[1])) / 65536
}

func newExtenderCamouflageReportId() string {
	var idBytes [16]byte
	// never fails (crypto/rand)
	rand.Read(idBytes[:])
	return hex.EncodeToString(idBytes[:])
}

// What the sender has done, for status and tests, over fixed sets.
type ExtenderCamouflageReporterStats struct {
	// reports made, each a new report id
	ReportCount uint64
	// posts, retries included
	AttemptCount uint64
	// reports the operator answered counted, accepted or duplicate
	DeliveredCount uint64
	// reports given up after their posts or their age
	GaveUpCount uint64
	// reports refused for what they hold, or because the reports are off
	RefusedCount uint64
	// reports dropped by an account change or by turning the sender off
	DroppedCount uint64
}

// One report on its way. Only the loop reads it.
type extenderCamouflagePendingReport struct {
	bodyBytes    []byte
	createTime   time.Time
	attemptCount int
}

// How old the report is by the larger of the two clocks. A timer and a monotonic
// reading can stop while a device sleeps, and the operator's claim on the report id
// does not, so a report does not get younger by being asleep.
func (self *extenderCamouflagePendingReport) age(now time.Time) time.Duration {
	return max(now.Sub(self.createTime), now.Round(0).Sub(self.createTime.Round(0)))
}

// The sender of one space or one extender process.
type ExtenderCamouflageReporter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	settings     *ExtenderCamouflageReporterSettings
	clientLedger *extenderCamouflageClientLedger

	wake *Monitor

	stateLock sync.Mutex
	enabled   bool
	source    ExtenderCamouflageReportSource
	stats     ExtenderCamouflageReporterStats

	// the loop's own
	active          bool
	accountSeen     uint64
	pending         *extenderCamouflagePendingReport
	nextTime        time.Time
	gaveUpRunLength int
}

// The sender is running, and off, when this returns.
func NewExtenderCamouflageReporter(
	ctx context.Context,
	settings *ExtenderCamouflageReporterSettings,
) *ExtenderCamouflageReporter {
	if settings == nil {
		settings = DefaultExtenderCamouflageReporterSettings()
	}
	copied := *settings
	settings = &copied
	settings.ApiUrl = strings.TrimRight(strings.TrimSpace(settings.ApiUrl), "/")
	if settings.ByJwt == nil {
		settings.ByJwt = func() string { return "" }
	}
	log := settings.Log
	if log == nil {
		log = DefaultLogger()
	}
	clientLedger := settings.clientLedger
	if clientLedger == nil {
		clientLedger = defaultExtenderCamouflageClientLedger
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderCamouflageReporter{
		ctx:          cancelCtx,
		cancel:       cancel,
		done:         make(chan struct{}),
		log:          log,
		settings:     settings,
		clientLedger: clientLedger,
		wake:         NewMonitor(),
	}
	if settings.AccountMonitor != nil {
		self.accountSeen = settings.AccountMonitor.Value()
	}
	go HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self
}

// Ends the loop and joins it. A post that is out is canceled.
func (self *ExtenderCamouflageReporter) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// Turns the sender on or off. Turning it off cancels the post that is out and drops
// its report, and whatever is counted while it is off is never sent.
func (self *ExtenderCamouflageReporter) SetEnabled(enabled bool) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.enabled == enabled {
			return false
		}
		self.enabled = enabled
		return true
	}()
	if changed {
		self.wake.NotifyAll()
	}
}

// SetSource sets the extender this sender also reports for, nil for none. It is
// read at each report, and a report on its way is not changed by it.
func (self *ExtenderCamouflageReporter) SetSource(source ExtenderCamouflageReportSource) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.source = source
}

// ClearSource clears the extender this sender reports for, if it is this one: a role
// that closes leaves another role's source alone.
func (self *ExtenderCamouflageReporter) ClearSource(source ExtenderCamouflageReportSource) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.source == source {
		self.source = nil
	}
}

// Source is the extender this sender reports for, nil for none.
func (self *ExtenderCamouflageReporter) Source() ExtenderCamouflageReportSource {
	return self.currentSource()
}

// Stats is what the sender has done so far.
func (self *ExtenderCamouflageReporter) Stats() ExtenderCamouflageReporterStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stats
}

func (self *ExtenderCamouflageReporter) currentSource() ExtenderCamouflageReportSource {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.source
}

// Applies one change to the stats.
func (self *ExtenderCamouflageReporter) count(update func(stats *ExtenderCamouflageReporterStats)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	update(&self.stats)
}

// The account generation and the channel its next change closes on; a nil channel,
// which never fires, for an identity that never changes.
func (self *ExtenderCamouflageReporter) accountGeneration() (uint64, chan struct{}) {
	if self.settings.AccountMonitor == nil {
		return 0, nil
	}
	return self.settings.AccountMonitor.Get()
}

// The loop: look whether a report is due, make it, or wait for the earliest of the
// due time, a change of the account and a change of the control.
func (self *ExtenderCamouflageReporter) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		// subscribe before the look, so a change during it is carried into the wait
		wake := self.wake.NotifyChannel()
		accountGeneration, accountChange := self.accountGeneration()

		wait, due, active := self.next(accountGeneration)
		if due {
			self.post(wake, accountChange)
			continue
		}
		var after <-chan time.Time
		if active {
			after = self.settings.After(wait)
		} else if self.settings.onInactive != nil {
			self.settings.onInactive()
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-accountChange:
		case <-after:
		}
	}
}

// Whether a post is due now, and else the wait until the next look; inactive while
// the sender is off or has no identity. A new account, or coming back from either,
// starts over from nothing.
func (self *ExtenderCamouflageReporter) next(accountGeneration uint64) (wait time.Duration, due bool, active bool) {
	now := self.settings.Now()
	if accountGeneration != self.accountSeen {
		self.accountSeen = accountGeneration
		self.startOver()
	}
	enabled := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.enabled
	}()
	if !enabled || self.settings.ByJwt() == "" {
		if self.active {
			self.active = false
			self.dropPending()
		}
		return 0, false, false
	}
	if !self.active {
		self.active = true
		self.startOver()
	}
	if self.nextTime.IsZero() {
		self.nextTime = now.Add(self.firstInterval())
	}
	if self.nextTime.After(now) {
		return self.nextTime.Sub(now), false, true
	}
	if self.pending != nil && self.settings.MaxReportAge <= self.pending.age(now) {
		// a retry that was due while the process slept, which could be counted twice
		// once the operator's claim on its id has gone
		self.gaveUp(now, "too_old")
		return self.nextTime.Sub(now), false, true
	}
	if self.pending == nil {
		self.pending = self.makeReport(now)
		if self.pending == nil {
			self.nextTime = now.Add(self.firstInterval())
			return self.nextTime.Sub(now), false, true
		}
	}
	return 0, true, true
}

// Drops the report on its way and everything counted so far.
func (self *ExtenderCamouflageReporter) startOver() {
	self.dropPending()
	self.clientLedger.discard()
	if source := self.currentSource(); source != nil {
		source.DiscardCamouflageReportCounts()
	}
	self.nextTime = time.Time{}
	self.gaveUpRunLength = 0
}

func (self *ExtenderCamouflageReporter) dropPending() {
	if self.pending == nil {
		return
	}
	self.pending = nil
	self.count(func(stats *ExtenderCamouflageReporterStats) {
		stats.DroppedCount += 1
	})
}

// Takes what is new and builds its report, nil with nothing to say.
func (self *ExtenderCamouflageReporter) makeReport(now time.Time) *extenderCamouflagePendingReport {
	clientCounts := self.clientLedger.take()
	var extenderCounts *ExtenderCamouflageExtenderCounts
	if source := self.currentSource(); source != nil {
		extenderCounts = source.TakeCamouflageReportCounts()
	}
	if clientCounts == nil && extenderCounts == nil {
		return nil
	}
	clientVersion := self.settings.ClientVersion()
	if extenderCamouflageReportMaxClientVersionByteCount < len(clientVersion) {
		clientVersion = ""
	}
	ipVersions := []int{}
	for _, ipVersion := range self.settings.IpVersions() {
		if (ipVersion == 4 || ipVersion == 6) && !slices.Contains(ipVersions, ipVersion) {
			ipVersions = append(ipVersions, ipVersion)
		}
	}
	bodyBytes, err := json.Marshal(&ExtenderCamouflageReportArgs{
		ReportId:        self.settings.NewReportId(),
		ClientVersion:   clientVersion,
		ResourceVersion: max(self.settings.ResourceVersion(), 0),
		IpVersions:      ipVersions,
		Client:          clientCounts,
		Extender:        extenderCounts,
	})
	if err != nil {
		return nil
	}
	self.count(func(stats *ExtenderCamouflageReporterStats) {
		stats.ReportCount += 1
	})
	return &extenderCamouflagePendingReport{
		bodyBytes:  bodyBytes,
		createTime: now,
	}
}

// What one post came to.
type extenderCamouflagePostResult struct {
	responseBytes []byte
	err           error
}

// One post of the pending report and what it came to. A change of the account or
// the control cancels it, and the next look decides what becomes of the report.
func (self *ExtenderCamouflageReporter) post(wake chan struct{}, accountChange chan struct{}) {
	byJwt := self.settings.ByJwt()
	if byJwt == "" {
		// the next look finds no identity
		return
	}
	pending := self.pending
	pending.attemptCount += 1
	self.count(func(stats *ExtenderCamouflageReporterStats) {
		stats.AttemptCount += 1
	})

	postCtx, postCancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer postCancel()
	resultChannel := make(chan *extenderCamouflagePostResult, 1)
	go HandleError(func() {
		responseBytes, err := self.settings.Post(postCtx, self.settings.ApiUrl+ExtenderCamouflageReportPath, pending.bodyBytes, byJwt)
		resultChannel <- &extenderCamouflagePostResult{responseBytes: responseBytes, err: err}
	}, func(err error) {
		resultChannel <- &extenderCamouflagePostResult{err: err}
	})

	var result *extenderCamouflagePostResult
	select {
	case result = <-resultChannel:
	case <-wake:
		postCancel()
		<-resultChannel
		return
	case <-accountChange:
		postCancel()
		<-resultChannel
		return
	case <-self.ctx.Done():
		postCancel()
		<-resultChannel
		return
	}
	// an answer that lands with an account change only moves what the next look
	// resets, so it needs no check here
	self.settle(result)
}

// Reads what a post came to, and sets the next look.
func (self *ExtenderCamouflageReporter) settle(result *extenderCamouflagePostResult) {
	now := self.settings.Now()
	if result.err != nil {
		var statusErr *HttpStatusError
		if errors.As(result.err, &statusErr) {
			switch statusErr.StatusCode {
			case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone:
				// an operator that does not serve the route yet
				self.refused(now, self.settings.DisabledWait, "route_absent")
				return
			}
		}
		self.retry(now, self.backoff(), "failed")
		return
	}
	answer := &ExtenderCamouflageReportResult{}
	if err := json.Unmarshal(result.responseBytes, answer); err != nil {
		self.retry(now, self.backoff(), "malformed_answer")
		return
	}
	switch answer.Outcome {
	case ExtenderCamouflageReportAccepted, ExtenderCamouflageReportDuplicate:
		self.pending = nil
		self.gaveUpRunLength = 0
		self.count(func(stats *ExtenderCamouflageReporterStats) {
			stats.DeliveredCount += 1
		})
		self.nextTime = now.Add(self.interval())
		self.logOutcome(answer.Outcome)
	case ExtenderCamouflageReportRateLimited:
		retryAfter := time.Duration(answer.RetryAfterSeconds) * time.Second
		retryAfter = min(max(retryAfter, self.settings.MinRetryAfter), self.settings.MaxRetryAfter)
		self.retry(now, retryAfter+self.retryJitter(retryAfter), answer.Outcome)
	case ExtenderCamouflageReportDisabled:
		self.refused(now, self.settings.DisabledWait, answer.Outcome)
	case ExtenderCamouflageReportInvalid:
		self.gaveUp(now, answer.Outcome)
	default:
		// unavailable, and an answer this client does not know
		self.retry(now, self.backoff(), "unavailable")
	}
}

// Posts the report again after `wait`, unless its posts or its age have run out.
func (self *ExtenderCamouflageReporter) retry(now time.Time, wait time.Duration, outcome string) {
	if self.settings.MaxAttemptCount <= self.pending.attemptCount ||
		self.settings.MaxReportAge < self.pending.age(now)+wait {
		self.gaveUp(now, outcome)
		return
	}
	retryTime := now.Add(wait)
	self.nextTime = retryTime
	self.logOutcome(outcome)
}

// The report is lost, and each one in a row makes the next wait longer.
func (self *ExtenderCamouflageReporter) gaveUp(now time.Time, outcome string) {
	self.pending = nil
	self.gaveUpRunLength += 1
	self.count(func(stats *ExtenderCamouflageReporterStats) {
		stats.GaveUpCount += 1
	})
	self.nextTime = now.Add(self.interval())
	self.logOutcome(outcome)
}

// The report is dropped because the operator will not take any, and the next look
// waits `wait`.
func (self *ExtenderCamouflageReporter) refused(now time.Time, wait time.Duration, outcome string) {
	self.pending = nil
	self.count(func(stats *ExtenderCamouflageReporterStats) {
		stats.RefusedCount += 1
	})
	self.nextTime = now.Add(wait + self.jitterEitherWay())
	self.logOutcome(outcome)
}

func (self *ExtenderCamouflageReporter) logOutcome(outcome string) {
	if self.log.V(1).Enabled() {
		self.log.Infof("[extender]camouflage report %s\n", outcome)
	}
}

// The wait before the first look, or after a look with nothing to say.
func (self *ExtenderCamouflageReporter) firstInterval() time.Duration {
	return self.settings.FirstInterval + time.Duration(self.settings.Random()*float64(self.settings.FirstIntervalJitter))
}

// The wait after a report ended, doubled for each report given up in a row.
func (self *ExtenderCamouflageReporter) interval() time.Duration {
	interval := self.settings.Interval
	for i := 0; i < self.gaveUpRunLength && interval < self.settings.MaxInterval; i += 1 {
		interval *= 2
	}
	return max(min(interval, self.settings.MaxInterval)+self.jitterEitherWay(), time.Second)
}

// Within IntervalJitter either way.
func (self *ExtenderCamouflageReporter) jitterEitherWay() time.Duration {
	return time.Duration((2*self.settings.Random() - 1) * float64(self.settings.IntervalJitter))
}

// The backoff before the next post of the pending report: doubling per post it has
// had, with up to a quarter more.
func (self *ExtenderCamouflageReporter) backoff() time.Duration {
	backoff := self.settings.RetryMinBackoff
	for i := 1; i < self.pending.attemptCount && backoff < self.settings.RetryMaxBackoff; i += 1 {
		backoff *= 2
	}
	backoff = min(backoff, self.settings.RetryMaxBackoff)
	return backoff + self.retryJitter(backoff)
}

// Up to a quarter of a wait, so a fleet that met one failure does not retry in step.
func (self *ExtenderCamouflageReporter) retryJitter(wait time.Duration) time.Duration {
	return time.Duration(self.settings.Random() * float64(wait) / 4)
}
