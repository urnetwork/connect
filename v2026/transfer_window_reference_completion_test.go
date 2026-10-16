// Default-suite guards use the actual reference constructor, without a build
// tag or environment gate. Joined completion counts distinguish delivered bytes
// from a healthy acknowledged reference; cleanup failures remain separate.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Reverting only the calibration makes the original real cell fail these
// semantic checks before its configuration literals are considered.
func runWindowSettledReferenceCompletions(t *testing.T, payload int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		completions := &windowReferenceCompletions{}
		cell := windowSettledLargeMessageCell("ceiling", [2]ByteCount{1250000, 125000}, payload)
		cell.referenceCompletions = completions
		measurement := max(2*time.Second, time.Duration(64*int64(payload)*int64(time.Second)/int64(cell.Rate)))
		reading := measureWindowPathCell(t, cell, measurement)
		logWindowServiceReading(t, reading)
		before, closing := completions.preCleanup, completions.afterCleanup
		t.Logf("reference-completion configuration=%s payload=%d calibration=%d budget=%d joined=%t pre-cleanup=%+v cleanup=%+v",
			windowSettledLargeReferenceConfiguration, payload, cell.CalibrationWindow, cell.Budget,
			completions.joined, before, closing)
		started := before.Started + closing.Started
		firstWrites := before.FirstWrites + closing.FirstWrites
		terminals := before.Successes + before.Errors + closing.Successes + closing.Errors
		if !completions.joined || started == 0 || started != firstWrites || started != terminals {
			t.Fatalf("reference completion ownership incomplete: joined=%t started=%d first-writes=%d terminals=%d",
				completions.joined, started, firstWrites, terminals)
		}
		if before.Successes == 0 || before.Errors != 0 ||
			reading.SenderReceive.AckHandoffMissCount != 0 || reading.SenderReceive.AckHandoffDropCount != 0 ||
			reading.Recovery.RecoveryWriteErrorCount != 0 {
			t.Fatalf("reference retired admitted data: successes=%d terminal-errors=%d ACK-misses=%d ACK-drops=%d recovery-errors=%d",
				before.Successes, before.Errors, reading.SenderReceive.AckHandoffMissCount,
				reading.SenderReceive.AckHandoffDropCount, reading.Recovery.RecoveryWriteErrorCount)
		}
		if reading.Mbps < .9*float64(cell.Rate)*8/1e6 || reading.MinFlowMbps == 0 ||
			reading.RelayDrops != 0 || reading.MeasurementRelayDrops != 0 || reading.NatRefused != 0 ||
			reading.SenderReceive.AckHandoffQueueFullCount != 0 || reading.Receiver.AckRouteWriteErrorCount != 0 ||
			reading.MaxRelayQueued > 4096 || reading.MaxRelayQueuedBytes > int64(mib(8)) {
			t.Fatalf("reference failed unchanged capacity/flow/finite-relay/refusal gates: %+v", reading)
		}
		if cell.CalibrationWindow != kib(256) || reading.Window.Window != kib(256) || cell.Budget != mib(48) ||
			cell.RoundTrip != 100*time.Millisecond || cell.Compression != 10*time.Millisecond ||
			cell.Flows != 8 || !cell.RoundRobinOffer || cell.Lanes != 0 || cell.Rate != 125000 || cell.Drop ||
			cell.Warmup != 74054432*time.Microsecond || reading.Seconds != measurement.Seconds() {
			t.Fatal("reference changed the declared calibration, workload or interval")
		}
	})
}

// The actual 16 KiB reference must acknowledge without pre-cleanup failure.
func TestWindowSettledLargeReferenceCompletes16K(t *testing.T) {
	runWindowSettledReferenceCompletions(t, 16*1024)
}

// The actual 64 KiB reference must acknowledge without pre-cleanup failure.
func TestWindowSettledLargeReferenceCompletes64K(t *testing.T) {
	runWindowSettledReferenceCompletions(t, 64*1024)
}
