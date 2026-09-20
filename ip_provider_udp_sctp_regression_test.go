//go:build !js

package connect

import (
	"testing"
	"time"
)

func TestProviderUdpSctpCompactQueueAdmitsColdHighRttFixedOffer(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rtt := range []time.Duration{2 * time.Millisecond, 120 * time.Millisecond} {
		t.Run(rtt.String(), func(t *testing.T) {
			budget := NewTransferMemoryBudget(kib(768))
			if !budget.TryReserve(kib(512)) {
				t.Fatal("existing SCTP owner admission failed")
			}
			result := runProviderUdpSctpQueueExperiment(t, rtt, false, false, 4, 0, udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget})
			t.Logf("%+v", result)
			if result.admitted != 468 || result.refused != 0 || result.retransmits != 0 {
				t.Fatalf("fixed offer failed after compact admission: %+v", result)
			}
			if budget.UsedByteCount() != kib(512) {
				t.Fatalf("generation leaked shared budget: %d", budget.UsedByteCount())
			}
			budget.Release(kib(512))
		})
	}
}

func TestProviderUdpSctpCompactQueueExhaustedBudgetPreservesBoundedRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(kib(64))
	if !budget.TryReserve(kib(64)) {
		t.Fatal("competing reservation failed")
	}
	result := runProviderUdpSctpQueueExperiment(t, 120*time.Millisecond, false, false, 4, 0, udpSctpProductionQueueExperimentSettings{enabled: true, budget: budget})
	if result.beforeAckRefused == 0 || result.retransmits != 0 || budget.UsedByteCount() != kib(64) {
		t.Fatalf("budget exhaustion changed bounded source semantics: %+v used=%d", result, budget.UsedByteCount())
	}
	budget.Release(kib(64))
}
