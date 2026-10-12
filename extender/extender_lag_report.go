package extender

// The report queue of the measurement lifecycle (lag.dev/PLAN.md M8, A7).
//
// The API builds and signs one LagReport per session and submits it here; the
// queue posts them to the operator in batches, under the extender's
// activation credential, as the ping reporter posts pings
// (connect.ExtenderPingReporter). A report is opaque here: the queue keeps its
// serialized bytes and never reads the body, so the report's shape is the
// API's and the operator's alone.
//
// A submit never blocks the session that made it. A batch of up to
// ReportMaxBatchCount goes when it is full or ReportFlushTimeout after its
// first report. A post the operator did not answer is put back at the front
// and retried on a backoff that doubles to its max; the queue holds at most
// ReportMaxPendingCount reports and drops the oldest, so an operator that
// cannot be reached never grows the extender's memory. What is still queued
// when the extender closes is dropped: it is a measurement, and a post on the
// way out would hold the shutdown.
//
// The reporter is safe for concurrent use.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The report route (A7), appended to the api url.
const ExtenderLagReportPath = "/network/extender-lag-report"

// One post of the report route (A7): standard base64 of each serialized
// LagReport. The json is the server's contract.
type extenderLagReportArgs struct {
	Reports []string `json:"reports"`
}

// The operator's answer: how many it accepted, and why it rejected each of the
// others, by its index in the post. A rejection is the operator's verdict on
// what was measured, which a retry would only repeat.
type extenderLagReportResult struct {
	Accepted int                           `json:"accepted"`
	Rejected []*extenderLagReportRejection `json:"rejected,omitempty"`
}

type extenderLagReportRejection struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// Answers that end a batch without a retry, because the operator would refuse
// it again: the batch itself is malformed or too large.
var extenderLagReportFinalStatusCodes = []int{
	http.StatusBadRequest,
	http.StatusRequestEntityTooLarge,
	http.StatusUnprocessableEntity,
}

type extenderLagReporter struct {
	server   *ExtenderServer
	settings *ExtenderLagLifecycleSettings

	wakeMonitor *connect.Monitor

	stateLock sync.Mutex
	// serialized reports, oldest first
	pendingReports [][]byte
	// when the oldest pending report arrived, which starts the flush clock
	firstPendingTime time.Time
	droppedCount     int
}

// The reporter is running when this returns, as a worker of the server. The
// settings are the lifecycle's, with their defaults in place.
func newExtenderLagReporter(server *ExtenderServer, settings *ExtenderLagLifecycleSettings) *extenderLagReporter {
	self := &extenderLagReporter{
		server:         server,
		settings:       settings,
		wakeMonitor:    connect.NewMonitor(),
		pendingReports: [][]byte{},
	}
	if server.beginWorker() {
		go func() {
			defer server.endWorker()
			connect.HandleError(self.run)
		}()
	}
	return self
}

// Queues one signed report (M8). It never blocks; beyond the pending cap the
// oldest report is dropped. A report that does not serialize is dropped.
func (self *extenderLagReporter) submit(report *protocol.LagReport) {
	if report == nil {
		return
	}
	reportBytes, err := proto.Marshal(report)
	if err != nil {
		log.Printf("[extender] lag: report err = %s", err)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.settings.ReportMaxPendingCount <= len(self.pendingReports) {
		dropCount := len(self.pendingReports) - self.settings.ReportMaxPendingCount + 1
		self.pendingReports = slices.Delete(self.pendingReports, 0, dropCount)
		self.droppedCount += dropCount
	}
	if len(self.pendingReports) == 0 {
		self.firstPendingTime = self.settings.Now()
	}
	self.pendingReports = append(self.pendingReports, reportBytes)
	self.wakeMonitor.NotifyAll()
}

// Reports queued and not yet posted.
func (self *extenderLagReporter) pendingCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.pendingReports)
}

// Reports dropped because the queue was full.
func (self *extenderLagReporter) dropCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.droppedCount
}

// The post loop. It posts a batch when one is due, and otherwise waits for the
// next report, the flush clock or the end of a backoff.
func (self *extenderLagReporter) run() {
	settings := self.settings
	backoff := settings.ReportMinBackoff
	// no post before this, set by a post that failed
	var retryTime time.Time
	for {
		reportBatch, wait, wake := self.takeBatch(settings.Now(), retryTime)
		if 0 < len(reportBatch) {
			if self.post(reportBatch) {
				backoff = settings.ReportMinBackoff
				retryTime = time.Time{}
			} else {
				self.requeue(reportBatch)
				retryTime = settings.Now().Add(backoff)
				backoff = min(2*backoff, settings.ReportMaxBackoff)
			}
			// there may be more due; look again before waiting
			continue
		}
		// nil, which never fires, when nothing is pending: only a report or
		// the end wakes this
		var after <-chan time.Time
		if 0 < wait {
			after = settings.After(wait)
		}
		select {
		case <-self.server.ctx.Done():
			return
		case <-wake:
		case <-after:
		}
	}
}

// The next batch when one is due, else how long until one could be, and the
// channel the next report closes. Zero with no batch means nothing is pending.
// The channel is subscribed in the same locked scope as the read, as a submit
// notifies in the same scope as its append, so a report is either in this read
// or wakes the wait after it, never both.
func (self *extenderLagReporter) takeBatch(now time.Time, retryTime time.Time) ([][]byte, time.Duration, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	wake := self.wakeMonitor.NotifyChannel()
	if len(self.pendingReports) == 0 {
		return nil, 0, wake
	}
	if now.Before(retryTime) {
		return nil, retryTime.Sub(now), wake
	}
	flushTime := self.firstPendingTime.Add(self.settings.ReportFlushTimeout)
	if len(self.pendingReports) < self.settings.ReportMaxBatchCount && now.Before(flushTime) {
		return nil, flushTime.Sub(now), wake
	}
	count := min(len(self.pendingReports), self.settings.ReportMaxBatchCount)
	reportBatch := slices.Clone(self.pendingReports[0:count])
	self.pendingReports = slices.Delete(self.pendingReports, 0, count)
	if 0 < len(self.pendingReports) {
		// what remains arrived after the batch's first; its clock starts now
		// rather than being owed the whole timeout again
		self.firstPendingTime = now
	}
	return reportBatch, 0, wake
}

// Puts a batch the operator did not answer back at the front. Beyond the cap
// the oldest go, which are the batch's own.
func (self *extenderLagReporter) requeue(reportBatch [][]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pendingReports = append(slices.Clone(reportBatch), self.pendingReports...)
	if self.settings.ReportMaxPendingCount < len(self.pendingReports) {
		dropCount := len(self.pendingReports) - self.settings.ReportMaxPendingCount
		self.pendingReports = slices.Delete(self.pendingReports, 0, dropCount)
		self.droppedCount += dropCount
	}
	self.firstPendingTime = self.settings.Now()
}

// Posts one batch under the activation credential (A7). True when the batch is
// done with: the operator answered it, whatever it accepted, or refused the
// batch itself as one it would refuse again. False when it should be retried.
func (self *extenderLagReporter) post(reportBatch [][]byte) bool {
	args := &extenderLagReportArgs{
		Reports: make([]string, 0, len(reportBatch)),
	}
	for _, reportBytes := range reportBatch {
		args.Reports = append(args.Reports, base64.StdEncoding.EncodeToString(reportBytes))
	}
	argsBytes, err := json.Marshal(args)
	if err != nil {
		log.Printf("[extender] lag: report post err = %s", err)
		return true
	}

	ctx, cancel := context.WithTimeout(self.server.ctx, self.settings.RequestTimeout)
	defer cancel()
	byJwt := ""
	if self.settings.ByJwt != nil {
		byJwt = self.settings.ByJwt()
	}
	apiUrl := strings.TrimRight(strings.TrimSpace(self.settings.ApiUrl), "/")
	bodyBytes, err := connect.HttpPostWithStrategyRaw(
		ctx,
		self.settings.ClientStrategy,
		apiUrl+ExtenderLagReportPath,
		argsBytes,
		byJwt,
	)
	if err != nil {
		var statusError *connect.HttpStatusError
		if errors.As(err, &statusError) && slices.Contains(extenderLagReportFinalStatusCodes, statusError.StatusCode) {
			log.Printf("[extender] lag: %d reports refused = %s", len(reportBatch), err)
			return true
		}
		// a post the close interrupted is no failure worth a line
		if self.server.ctx.Err() == nil {
			log.Printf("[extender] lag: report post err = %s", err)
		}
		return false
	}

	// the operator took the batch; its verdicts are logged, never retried
	result := &extenderLagReportResult{}
	if err := json.Unmarshal(bodyBytes, result); err != nil {
		log.Printf("[extender] lag: report answer err = %s", err)
		return true
	}
	if 0 < len(result.Rejected) {
		reasons := []string{}
		for _, rejection := range result.Rejected {
			if rejection != nil && !slices.Contains(reasons, rejection.Reason) {
				reasons = append(reasons, rejection.Reason)
			}
		}
		log.Printf(
			"[extender] lag: the operator rejected %d of %d reports: %s",
			len(result.Rejected), len(reportBatch), strings.Join(reasons, "; "),
		)
	}
	return true
}
