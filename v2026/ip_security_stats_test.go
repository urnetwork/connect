// Security-policy statistics tests keep diagnostic accounting bounded without
// changing exact low-cardinality counts.
package connect

import (
	"net"
	"testing"
)

// TestSecurityPolicyStatsCollectorBoundsDestinationCardinality verifies that a
// long-running provider cannot retain every ephemeral port it has encountered.
func TestSecurityPolicyStatsCollectorBoundsDestinationCardinality(t *testing.T) {
	stats := DefaultSecurityPolicyStatsCollector()
	const extraDestinationCount = 100
	for port := 1; port <= securityPolicyStatsMaxDestinationsPerResult+extraDestinationCount; port++ {
		stats.AddDestination(
			&IpPath{
				Version:         4,
				Protocol:        IpProtocolTcp,
				DestinationIp:   net.IPv4(203, 0, 113, 1),
				DestinationPort: port,
			},
			SecurityPolicyResultAllow,
			1,
		)
	}

	snapshot := stats.Stats(false)
	destinationCounts := snapshot[SecurityPolicyResultAllow]
	if count := len(destinationCounts); count != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf(
			"destination cardinality = %d, want hard limit %d",
			count,
			securityPolicyStatsMaxDestinationsPerResult,
		)
	}
	firstDestination := SecurityDestination{
		Version:  4,
		Protocol: IpProtocolTcp,
		Port:     1,
	}
	if count := destinationCounts[firstDestination]; count != 1 {
		t.Fatalf("first exact destination count = %d, want 1", count)
	}
	overflowCount := uint64(extraDestinationCount + 1)
	if count := destinationCounts[securityPolicyStatsOverflowDestination]; count != overflowCount {
		t.Fatalf("overflow count = %d, want %d", count, overflowCount)
	}

	stats.AddDestination(
		&IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			DestinationIp:   net.IPv4(203, 0, 113, 1),
			DestinationPort: 1,
		},
		SecurityPolicyResultAllow,
		2,
	)
	snapshot = stats.Stats(false)
	if count := snapshot[SecurityPolicyResultAllow][firstDestination]; count != 3 {
		t.Fatalf("existing destination count after saturation = %d, want 3", count)
	}
	if count := snapshot[SecurityPolicyResultAllow][securityPolicyStatsOverflowDestination]; count != overflowCount {
		t.Fatalf("existing destination was misclassified as overflow: count = %d, want %d", count, overflowCount)
	}
}

// TestSecurityPolicyStatsCollectorResetRestoresExactCollection verifies that a
// reset clears both the counters and the cardinality budget.
func TestSecurityPolicyStatsCollectorResetRestoresExactCollection(t *testing.T) {
	stats := DefaultSecurityPolicyStatsCollector()
	for port := 1; port <= securityPolicyStatsMaxDestinationsPerResult+1; port++ {
		stats.AddSource(
			&IpPath{
				Version:    4,
				Protocol:   IpProtocolUdp,
				SourceIp:   net.IPv4(198, 51, 100, 2),
				SourcePort: port,
			},
			SecurityPolicyResultAllow,
			1,
		)
	}
	stats.Stats(true)
	if snapshot := stats.Stats(false); len(snapshot) != 0 {
		t.Fatalf("statistics after reset = %v, want empty", snapshot)
	}

	newSource := &IpPath{
		Version:    4,
		Protocol:   IpProtocolUdp,
		SourceIp:   net.IPv4(198, 51, 100, 2),
		SourcePort: securityPolicyStatsMaxDestinationsPerResult + 1,
	}
	stats.AddSource(newSource, SecurityPolicyResultAllow, 1)
	destination := newSecuritySourcePort(newSource)
	if count := stats.Stats(false)[SecurityPolicyResultAllow][destination]; count != 1 {
		t.Fatalf("exact destination count after reset = %d, want 1", count)
	}
}

// TestSecurityPolicyStatsCollectorBoundsUnknownResults verifies that arbitrary
// caller-provided result integers share one bounded diagnostic bucket.
func TestSecurityPolicyStatsCollectorBoundsUnknownResults(t *testing.T) {
	stats := DefaultSecurityPolicyStatsCollector()
	for result := 10; result < 10+securityPolicyStatsMaxDestinationsPerResult+100; result++ {
		stats.AddDestination(
			&IpPath{
				Version:         4,
				Protocol:        IpProtocolTcp,
				DestinationIp:   net.IPv4(192, 0, 2, 3),
				DestinationPort: result,
			},
			SecurityPolicyResult(result),
			1,
		)
	}

	snapshot := stats.Stats(false)
	if count := len(snapshot); count != 1 {
		t.Fatalf("result bucket count = %d, want 1", count)
	}
	if count := len(snapshot[securityPolicyStatsUnknownResult]); count != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf(
			"unknown-result destination cardinality = %d, want %d",
			count,
			securityPolicyStatsMaxDestinationsPerResult,
		)
	}
}
