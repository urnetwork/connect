package main

import (
	"strings"
	"testing"
)

func TestCarrierHandoffPairRequiresExactIosH1Claim(t *testing.T) {
	valid := carrierHandoffPairSample{
		ID: 7, From: "h1", To: "h3_explicit", Owner: "device",
		H1Bytes: iosCarrierH1OverlapBytes, Bytes: iosCarrierH1OverlapBytes, Slots: 1,
	}
	if err := validateCarrierHandoffPair(valid); err != nil {
		t.Fatalf("exact iOS handoff refused: %v", err)
	}
	for _, test := range []struct {
		name    string
		h1Bytes int64
		slots   int64
	}{
		{"no slot", iosCarrierH1OverlapBytes, 0},
		{"negative slot", iosCarrierH1OverlapBytes, -1},
		{"two slots", iosCarrierH1OverlapBytes, 2},
		{"no H1 claim", 0, 1},
		{"one byte H1 claim", 1, 1},
		{"underreported H1 claim", iosCarrierH1OverlapBytes - 1, 1},
		{"oversized H1 claim", iosCarrierH1OverlapBytes + 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			pair := valid
			pair.H1Bytes, pair.Slots = test.h1Bytes, test.slots
			pair.Bytes = min(pair.Bytes, pair.H1Bytes)
			if err := validateCarrierHandoffPair(pair); err == nil {
				t.Fatalf("malformed iOS handoff accepted: %+v", pair)
			}
		})
	}
}

// Keep every copy of a pair internally consistent. A cross-level mismatch
// check alone cannot catch a coherently underreported H1 claim or zero-slot
// pair, including its active loan and the other level's additional evidence.
func TestMemsteadyCarrierHierarchyRejectsUnderreportedIosHandoffClaims(t *testing.T) {
	for _, scope := range []struct {
		name      string
		ownerless bool
		prefixes  []string
	}{
		{"root primary and child additional", false, []string{
			"transport_budget_pair_", "transport_budget_active_handoff_", "device_transport_budget_additional_pair_",
		}},
		{"child primary and root additional", false, []string{
			"device_transport_budget_pair_", "device_transport_budget_active_handoff_", "transport_budget_additional_pair_",
		}},
		{"ownerless root primary", true, []string{
			"transport_budget_pair_", "transport_budget_active_handoff_",
		}},
	} {
		for _, malformed := range []string{"zero slots", "underreported H1 bytes"} {
			t.Run(scope.name+"/"+malformed, func(t *testing.T) {
				fixture := concurrentCarrierPairReportFixture(scope.ownerless)
				payload := fixture.client[30].Payload
				// Unrelated claims may drain while a valid paired handoff remains
				// live. Leave ordinary headroom so a smaller reported overlap is
				// not rejected incidentally by the byte/slot capacity checks.
				setCarrierUsageFixture(payload, "transport_budget_", iosCarrierRootBytes, 11)
				if _, _, err := validateCarrierBudgetHierarchy(payload); err != nil {
					t.Fatalf("initial exact pair evidence is invalid: %v", err)
				}
				for _, prefix := range scope.prefixes {
					if malformed == "zero slots" {
						payload[prefix+"slots"] = float64(0)
					} else {
						payload[prefix+"h1_bytes"] = float64(iosCarrierH1OverlapBytes / 2)
						payload[prefix+"bytes"] = float64(iosCarrierH1OverlapBytes / 2)
					}
				}
				summary, err := runReportFixture(t, fixture)
				if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "carrier budget hierarchy:") {
					t.Fatalf("coherently underreported primary/additional/active evidence passed: %+v, %v", summary.Client, err)
				}
			})
		}
	}
}

func TestMemsteadyCarrierHierarchyRejectsMalformedActiveIosHandoffEvidence(t *testing.T) {
	for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
		for _, field := range []string{"slots", "h1_bytes"} {
			t.Run(prefix+field, func(t *testing.T) {
				fixture := pairedCarrierReportFixture()
				fixture.client[30].Payload[prefix+"active_handoff_"+field] = float64(0)
				summary, err := runReportFixture(t, fixture)
				if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "carrier budget hierarchy:") {
					t.Fatalf("malformed active handoff evidence passed: %+v, %v", summary.Client, err)
				}
			})
		}
	}
}
