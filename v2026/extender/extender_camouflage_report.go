package extender

import (
	"cmp"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

// extender_camouflage_report.go — this server's part of the camouflage report
// (connect/net_extender_camouflage_report.go; EXTENDER-SPLICE-ROLLOUT.md, Reporting
// path). What has been taken lives with the counters it reads, so a server that
// restarts starts both at zero, and two senders never send one count. An extender
// with nothing counted still has a verdict to give.

// How much of one borrowed site's load has been taken.
type extenderCamouflageTargetLedger struct {
	splices      connect.ExtenderCamouflageCountLedger
	relayedBytes connect.ExtenderCamouflageCountLedger
}

// How much of the server's camouflage counters has been taken.
type extenderCamouflageReportLedger struct {
	stateLock                sync.Mutex
	authenticated            connect.ExtenderCamouflageCountLedger
	spliced                  connect.ExtenderCamouflageCountLedger
	terminated               connect.ExtenderCamouflageCountLedger
	spliceFallbackTerminated connect.ExtenderCamouflageCountLedger
	replayRefused            connect.ExtenderCamouflageCountLedger
	timeWindowFailed         connect.ExtenderCamouflageCountLedger
	spliceDialFailed         connect.ExtenderCamouflageCountLedger
	borrowVerifyFailed       connect.ExtenderCamouflageCountLedger
	targets                  map[string]*extenderCamouflageTargetLedger
}

// The ledger of one borrowed site, made on first use.
func (self *extenderCamouflageReportLedger) target(name string) *extenderCamouflageTargetLedger {
	if self.targets == nil {
		self.targets = map[string]*extenderCamouflageTargetLedger{}
	}
	targetLedger, ok := self.targets[name]
	if !ok {
		targetLedger = &extenderCamouflageTargetLedger{}
		self.targets[name] = targetLedger
	}
	return targetLedger
}

// TakeCamouflageReportCounts is what the server has counted since the last take, each
// count within the operator's bound with the excess left for the next take, and its
// coverage verdict now. It holds at most ExtenderCamouflageReportMaxTargetCount
// borrowed sites, the largest new load first, and the others wait.
func (self *ExtenderServer) TakeCamouflageReportCounts() *connect.ExtenderCamouflageExtenderCounts {
	stats := self.CamouflageStats()
	status := self.CamouflageStatus()
	maxCount := connect.ExtenderCamouflageReportMaxExtenderCount
	ledger := &self.camouflageReportLedger

	ledger.stateLock.Lock()
	defer ledger.stateLock.Unlock()
	counts := &connect.ExtenderCamouflageExtenderCounts{
		Authenticated:            ledger.authenticated.Take(stats.AuthenticatedCount, maxCount),
		Spliced:                  ledger.spliced.Take(stats.SplicedCount, maxCount),
		Terminated:               ledger.terminated.Take(stats.TerminatedCount, maxCount),
		SpliceFallbackTerminated: ledger.spliceFallbackTerminated.Take(stats.SpliceFallbackTerminatedCount, maxCount),
		ReplayRefused:            ledger.replayRefused.Take(stats.ReplayRefusedCount, maxCount),
		TimeWindowFailed:         ledger.timeWindowFailed.Take(stats.TimeWindowFailedCount, maxCount),
		SpliceDialFailed:         ledger.spliceDialFailed.Take(stats.SpliceDialFailedCount, maxCount),
		BorrowVerifyFailed:       ledger.borrowVerifyFailed.Take(stats.BorrowVerifyFailCount, maxCount),
		Covered:                  status.Covered,
		DegradedReason:           status.DegradedReason,
	}

	// the sites with something new, the most new bytes first
	names := []string{}
	for name, load := range stats.TargetLoads {
		if connect.ExtenderCamouflageReportMaxTargetNameByteCount < len(name) {
			continue
		}
		targetLedger := ledger.target(name)
		if 0 < targetLedger.splices.Pending(load.SpliceCount) || 0 < targetLedger.relayedBytes.Pending(load.RelayedByteCount) {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a string, b string) int {
		aPending := ledger.target(a).relayedBytes.Pending(stats.TargetLoads[a].RelayedByteCount)
		bPending := ledger.target(b).relayedBytes.Pending(stats.TargetLoads[b].RelayedByteCount)
		return cmp.Or(cmp.Compare(bPending, aPending), strings.Compare(a, b))
	})
	for _, name := range names[:min(len(names), connect.ExtenderCamouflageReportMaxTargetCount)] {
		load := stats.TargetLoads[name]
		targetLedger := ledger.target(name)
		counts.Targets = append(counts.Targets, &connect.ExtenderCamouflageTargetCounts{
			Name:             name,
			SpliceCount:      targetLedger.splices.Take(load.SpliceCount, connect.ExtenderCamouflageReportMaxTargetSpliceCount),
			RelayedByteCount: targetLedger.relayedBytes.Take(load.RelayedByteCount, connect.ExtenderCamouflageReportMaxTargetByteCount),
		})
	}
	return counts
}

// DiscardCamouflageReportCounts makes everything counted so far never reported.
func (self *ExtenderServer) DiscardCamouflageReportCounts() {
	stats := self.CamouflageStats()
	ledger := &self.camouflageReportLedger

	ledger.stateLock.Lock()
	defer ledger.stateLock.Unlock()
	ledger.authenticated.Discard(stats.AuthenticatedCount)
	ledger.spliced.Discard(stats.SplicedCount)
	ledger.terminated.Discard(stats.TerminatedCount)
	ledger.spliceFallbackTerminated.Discard(stats.SpliceFallbackTerminatedCount)
	ledger.replayRefused.Discard(stats.ReplayRefusedCount)
	ledger.timeWindowFailed.Discard(stats.TimeWindowFailedCount)
	ledger.spliceDialFailed.Discard(stats.SpliceDialFailedCount)
	ledger.borrowVerifyFailed.Discard(stats.BorrowVerifyFailCount)
	for name, load := range stats.TargetLoads {
		targetLedger := ledger.target(name)
		targetLedger.splices.Discard(load.SpliceCount)
		targetLedger.relayedBytes.Discard(load.RelayedByteCount)
	}
}
