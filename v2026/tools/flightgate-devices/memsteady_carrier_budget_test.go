package main

import (
	"strings"
	"testing"
)

func setCarrierUsageFixture(payload map[string]any, prefix string, used, slots int64) {
	payload[prefix+"used_bytes"] = float64(used)
	payload[prefix+"reserved_bytes"] = float64(used)
	payload[prefix+"released_bytes"] = float64(0)
	payload[prefix+"used_count"] = float64(slots)
}

func setCarrierPairFixture(payload map[string]any, prefix, owner, from, to string, borrowed bool) {
	payload[prefix+"pair_id"] = float64(7)
	payload[prefix+"pair_from"] = from
	payload[prefix+"pair_to"] = to
	payload[prefix+"pair_owner"] = owner
	payload[prefix+"pair_h1_bytes"] = float64(iosCarrierH1OverlapBytes)
	payload[prefix+"pair_bytes"] = float64(iosCarrierH1OverlapBytes)
	payload[prefix+"pair_slots"] = float64(1)
	if borrowed {
		payload[prefix+"active_handoff_count"] = float64(1)
		for _, key := range []string{"id", "from", "to", "h1_bytes", "bytes", "slots"} {
			payload[prefix+"active_handoff_"+key] = payload[prefix+"pair_"+key]
		}
	}
}

func copyCarrierPairEvidence(payload map[string]any, destination, source string) {
	for _, field := range []string{"id", "from", "to", "h1_bytes", "bytes", "slots", "owner"} {
		payload[destination+field] = payload[source+field]
	}
}

func clearCarrierPairEvidence(payload map[string]any, prefix string) {
	for _, field := range []string{"id", "h1_bytes", "bytes", "slots"} {
		payload[prefix+field] = float64(0)
	}
	for _, field := range []string{"from", "to", "owner"} {
		payload[prefix+field] = ""
	}
}

func concurrentCarrierPairReportFixture(ownerlessRoot bool) reportFixture {
	f := newReportFixture()
	payload := f.client[30].Payload
	owner := "device"
	if ownerlessRoot {
		owner = "process"
	}
	setCarrierPairFixture(payload, "transport_budget_", owner, "h1", "h1", true)
	payload["transport_budget_pair_id"] = float64(8)
	payload["transport_budget_active_handoff_id"] = float64(8)
	setCarrierPairFixture(payload, "device_transport_budget_", "device", "h1", "h3_explicit", true)
	copyCarrierPairEvidence(payload, "transport_budget_additional_pair_", "device_transport_budget_pair_")
	if !ownerlessRoot {
		copyCarrierPairEvidence(payload, "device_transport_budget_additional_pair_", "transport_budget_pair_")
	}
	// The first loan remains paired while its old carrier drains, even after
	// unrelated releases put child use back at its normal limit. Root pressure
	// makes the second manager's replacement borrow only at the root.
	setCarrierUsageFixture(payload, "transport_budget_", iosCarrierRootBytes+iosCarrierH1OverlapBytes, 11)
	setCarrierUsageFixture(payload, "device_transport_budget_", iosCarrierDeviceBytes, 9)
	return f
}

func TestMemsteadyCarrierHierarchyAcceptsConcurrentDistinctPairs(t *testing.T) {
	for _, ownerless := range []bool{false, true} {
		f := concurrentCarrierPairReportFixture(ownerless)
		summary, err := runReportFixture(t, f)
		if err != nil || !summary.Pass || summary.Client.TransportBudgetHandoffBytes != iosCarrierH1OverlapBytes ||
			summary.Client.DeviceTransportBudgetHandoffBytes != iosCarrierH1OverlapBytes {
			t.Fatalf("concurrent paired ownership failed (ownerless %t): %+v, %v", ownerless, summary.Client, err)
		}
	}
}

func TestMemsteadyCarrierHierarchyRejectsMalformedConcurrentPairs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing root additional field", func(p map[string]any) { delete(p, "transport_budget_additional_pair_id") }},
		{"missing child additional field", func(p map[string]any) { delete(p, "device_transport_budget_additional_pair_owner") }},
		{"underreported root", func(p map[string]any) { clearCarrierPairEvidence(p, "transport_budget_additional_pair_") }},
		{"underreported child", func(p map[string]any) { clearCarrierPairEvidence(p, "device_transport_budget_additional_pair_") }},
		{"duplicate pair", func(p map[string]any) {
			copyCarrierPairEvidence(p, "transport_budget_additional_pair_", "transport_budget_pair_")
		}},
		{"inactive additional residue", func(p map[string]any) { p["transport_budget_additional_pair_id"] = float64(0) }},
		{"invented pair", func(p map[string]any) { p["transport_budget_additional_pair_id"] = float64(9) }},
		{"mismatched classes", func(p map[string]any) { p["transport_budget_additional_pair_to"] = "h3_auto" }},
		{"mismatched owner", func(p map[string]any) { p["device_transport_budget_additional_pair_owner"] = "process" }},
		{"additional non H1", func(p map[string]any) { p["transport_budget_additional_pair_from"] = "h3_auto" }},
		{"additional oversized H1", func(p map[string]any) {
			p["transport_budget_additional_pair_h1_bytes"] = float64(iosCarrierH1OverlapBytes + 1)
		}},
		{"additional oversized overlap", func(p map[string]any) {
			p["transport_budget_additional_pair_bytes"] = float64(iosCarrierH1OverlapBytes + 1)
		}},
		{"sum of byte loans", func(p map[string]any) {
			setCarrierUsageFixture(p, "transport_budget_", iosCarrierRootBytes+2*iosCarrierH1OverlapBytes, 11)
		}},
		{"sum of slot loans", func(p map[string]any) { p["transport_budget_used_count"] = float64(iosCarrierRootMaxCount + 2) }},
		{"hidden child lender", func(p map[string]any) {
			p["device_transport_budget_active_handoff_count"] = float64(0)
			for _, field := range []string{"id", "h1_bytes", "bytes", "slots"} {
				p["device_transport_budget_active_handoff_"+field] = float64(0)
			}
			p["device_transport_budget_active_handoff_from"] = ""
			p["device_transport_budget_active_handoff_to"] = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := concurrentCarrierPairReportFixture(false)
			test.change(f.client[30].Payload)
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "carrier budget hierarchy:") {
				t.Fatalf("invalid overlapping pair proof passed: %+v, %v", summary.Client, err)
			}
		})
	}
}

func TestMemsteadyCarrierHierarchyRequiresInactiveAdditionalEvidence(t *testing.T) {
	f := newReportFixture()
	delete(f.client[30].Payload, "transport_budget_additional_pair_id")
	summary, err := runReportFixture(t, f)
	if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "missing transport_budget_additional_pair_id") {
		t.Fatalf("older incomplete carrier schema was certified: %+v, %v", summary.Client, err)
	}
}

func pairedCarrierReportFixture() reportFixture {
	f := newReportFixture()
	payload := f.client[30].Payload
	for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
		setCarrierPairFixture(payload, prefix, "device", "h1", "h3_explicit", true)
	}
	setCarrierUsageFixture(payload, "transport_budget_", iosCarrierRootBytes+iosCarrierH1OverlapBytes, 17)
	setCarrierUsageFixture(payload, "device_transport_budget_", iosCarrierDeviceBytes+iosCarrierH1OverlapBytes, 17)
	return f
}

func TestMemsteadyCarrierHierarchyAcceptsEachBorrowingScope(t *testing.T) {
	for _, test := range []struct {
		name                string
		rootLoan, childLoan bool
		owner, from, to     string
	}{
		{"both levels", true, true, "device", "h1", "h3_explicit"},
		{"child only", false, true, "device", "h3_auto", "h1"},
		{"root only device", true, false, "device", "h1", "h3_auto"},
		{"root only ownerless", true, false, "process", "h1", "h3_explicit"},
		{"H1 to H1", true, true, "device", "h1", "h1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReportFixture()
			payload := f.client[30].Payload
			rootBytes, childBytes := int64(iosCarrierDeviceBytes+iosCarrierH1OverlapBytes), int64(2*iosCarrierH1OverlapBytes)
			if test.rootLoan {
				rootBytes = iosCarrierRootBytes + iosCarrierH1OverlapBytes
			}
			if test.childLoan {
				childBytes = iosCarrierDeviceBytes + iosCarrierH1OverlapBytes
			}
			setCarrierPairFixture(payload, "transport_budget_", test.owner, test.from, test.to, test.rootLoan)
			setCarrierUsageFixture(payload, "transport_budget_", rootBytes, 4)
			if test.owner == "device" {
				setCarrierPairFixture(payload, "device_transport_budget_", "device", test.from, test.to, test.childLoan)
				setCarrierUsageFixture(payload, "device_transport_budget_", childBytes, 2)
			}
			summary, err := runReportFixture(t, f)
			if err != nil || !summary.Pass || summary.Client.DeviceTransportBudgetTotalBytes != iosCarrierDeviceBytes ||
				summary.Client.TransportBudgetEndBytes != iosCarrierH1OverlapBytes ||
				summary.Client.DeviceTransportBudgetEndBytes != iosCarrierH1OverlapBytes {
				t.Fatalf("valid borrowing scope failed: %+v, %v", summary, err)
			}
		})
	}
}

func TestMemsteadyCarrierHierarchyRejectsFalseProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		paired bool
		mutate func(map[string]any)
	}{
		{"missing child", false, func(p map[string]any) { delete(p, "device_transport_budget_total_bytes") }},
		{"wrong child ceiling", false, func(p map[string]any) { p["device_transport_budget_total_bytes"] = float64(iosCarrierRootBytes) }},
		{"wrong child slot cap", false, func(p map[string]any) { p["device_transport_budget_max_count"] = float64(17) }},
		{"hidden child byte escape", false, func(p map[string]any) {
			setCarrierUsageFixture(p, "transport_budget_", 7*1024*1024, 1)
			setCarrierUsageFixture(p, "device_transport_budget_", 6*1024*1024, 1)
		}},
		{"hidden child slot escape", false, func(p map[string]any) { p["device_transport_budget_used_count"] = float64(17) }},
		{"child missing root charge", false, func(p map[string]any) { setCarrierUsageFixture(p, "device_transport_budget_", 1024*1024, 1) }},
		{"child release imbalance", false, func(p map[string]any) { p["device_transport_budget_released_bytes"] = float64(1) }},
		{"inactive root identity residue", false, func(p map[string]any) { p["transport_budget_active_handoff_id"] = float64(7) }},
		{"inactive child class residue", false, func(p map[string]any) { p["device_transport_budget_active_handoff_from"] = "h1" }},
		{"inactive pair residue", false, func(p map[string]any) { p["transport_budget_pair_owner"] = "process" }},
		{"fake pair no loan", false, func(p map[string]any) {
			for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
				setCarrierUsageFixture(p, prefix, 2*iosCarrierH1OverlapBytes, 2)
				setCarrierPairFixture(p, prefix, "device", "h1", "h3_explicit", false)
			}
		}},
		{"pair missing two live claims", true, func(p map[string]any) {
			setCarrierUsageFixture(p, "device_transport_budget_", iosCarrierH1OverlapBytes, 1)
		}},
		{"non H1 pair", true, func(p map[string]any) {
			for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
				setCarrierPairFixture(p, prefix, "device", "h3_auto", "h3_explicit", true)
			}
		}},
		{"oversized H1 overlap", true, func(p map[string]any) {
			for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
				p[prefix+"pair_h1_bytes"] = float64(iosCarrierH1OverlapBytes + 1)
				p[prefix+"active_handoff_h1_bytes"] = float64(iosCarrierH1OverlapBytes + 1)
			}
		}},
		{"oversized overlap beyond H1", true, func(p map[string]any) {
			p["transport_budget_pair_bytes"] = float64(iosCarrierH1OverlapBytes + 1)
			p["transport_budget_active_handoff_bytes"] = float64(iosCarrierH1OverlapBytes + 1)
		}},
		{"two borrowed slots", true, func(p map[string]any) {
			p["transport_budget_pair_slots"] = float64(2)
			p["transport_budget_active_handoff_slots"] = float64(2)
		}},
		{"root child pair mismatch", true, func(p map[string]any) {
			p["device_transport_budget_pair_id"] = float64(8)
			p["device_transport_budget_active_handoff_id"] = float64(8)
		}},
		{"active evidence mismatch", true, func(p map[string]any) { p["transport_budget_active_handoff_to"] = "h3_auto" }},
		{"unjustified ownerless", true, func(p map[string]any) { p["transport_budget_pair_owner"] = "process" }},
		{"missing owner", true, func(p map[string]any) { delete(p, "transport_budget_pair_owner") }},
		{"other device handoff", true, func(p map[string]any) { p["transport_budget_pair_owner"] = "other_device" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReportFixture()
			if test.paired {
				fixture = pairedCarrierReportFixture()
			}
			test.mutate(fixture.client[30].Payload)
			summary, err := runReportFixture(t, fixture)
			if err == nil || summary.Pass {
				t.Fatalf("invalid carrier proof passed: %+v, %v", summary, err)
			}
		})
	}
}

func TestMemsteadyCarrierRecoveryRejectsSubCeilingLeaks(t *testing.T) {
	for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
		for _, retained := range []string{"bytes", "slots"} {
			t.Run(prefix+retained, func(t *testing.T) {
				fixture := newReportFixture()
				// Leave generous ordinary root headroom so only the device
				// baseline catches a child leak, independently of root recovery.
				for _, sample := range fixture.client {
					setCarrierUsageFixture(sample.Payload, "transport_budget_", 2*1024*1024, 4)
				}
				last := fixture.client[len(fixture.client)-1].Payload
				if retained == "bytes" {
					last[prefix+"used_bytes"] = last[prefix+"used_bytes"].(float64) + 1
					last[prefix+"reserved_bytes"] = last[prefix+"reserved_bytes"].(float64) + 1
				} else {
					last[prefix+"used_count"] = last[prefix+"used_count"].(float64) + 1
				}
				if _, _, err := validateCarrierBudgetHierarchy(last); err != nil {
					t.Fatalf("sub-ceiling fixture should pass instantaneous admission: %v", err)
				}
				summary, err := runReportFixture(t, fixture)
				if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "pre-burst baseline") {
					t.Fatalf("sub-ceiling retained carrier passed recovery: %+v, %v", summary, err)
				}
			})
		}
	}
}

func TestMemsteadyCarrierRecoveryRejectsValidOutstandingHandoff(t *testing.T) {
	fixture := newReportFixture()
	last := fixture.client[len(fixture.client)-1].Payload
	for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
		setCarrierUsageFixture(last, prefix, 2*iosCarrierH1OverlapBytes, 2)
		setCarrierPairFixture(last, prefix, "device", "h1", "h1", true)
	}
	if _, _, err := validateCarrierBudgetHierarchy(last); err != nil {
		t.Fatal(err)
	}
	summary, err := runReportFixture(t, fixture)
	if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "handoff remained active") {
		t.Fatalf("quiet-end live pair passed recovery: %+v, %v", summary, err)
	}
}
