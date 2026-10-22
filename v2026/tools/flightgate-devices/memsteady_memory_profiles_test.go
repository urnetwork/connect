// Complete synthetic reports exercise both physical memory profiles through
// the same parser, metadata binding, hierarchy and quiet-recovery gates.
package main

import (
	"strconv"
	"strings"
	"testing"
)

// Builds both profiles independently of the production policy lookup so a
// wrong policy constant cannot manufacture its own passing fixture.
func memoryProfileReportFixture(t *testing.T, profile string) reportFixture {
	t.Helper()
	var targetBytes, h1Bytes int64
	switch profile {
	case "ios-memory-audit-v2":
		targetBytes, h1Bytes = 32*1024*1024, 256*1024
	case "android":
		targetBytes, h1Bytes = 64*1024*1024, 512*1024
	default:
		t.Fatalf("unknown synthetic profile %q", profile)
	}
	f := newReportFixture()
	f.meta.MemoryProfile = profile
	f.meta.DeviceMemoryTargetBytes = targetBytes
	f.meta.ProcessMemoryLimitBytes = targetBytes
	f.meta.ProcessTransportBytes = targetBytes / 4
	f.meta.ProcessTransportCount = 16
	for _, samples := range [][]memsteadySample{f.client, f.provider} {
		for _, sample := range samples {
			payload := sample.Payload
			payload["memoryProfile"] = profile
			payload["memory_profile_rate_bytes"] = float64(0)
			payload["go_total_bytes"] = float64(targetBytes * 5 / 8)
			payload["go_limit_bytes"] = float64(targetBytes)
			payload["device_memory_target_bytes"] = float64(targetBytes)
			payload["transfer_root_total_bytes"] = float64(targetBytes*9/20 + targetBytes/5)
			payload["client_transfer_total_bytes"] = float64(targetBytes * 9 / 20)
			payload["provider_transfer_total_bytes"] = float64(targetBytes / 10)
			payload["nat_budget_total_bytes"] = float64(targetBytes / 10)
			for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
				payload[prefix+"total_bytes"] = float64(targetBytes / 4)
				payload[prefix+"max_count"] = float64(16)
				setCarrierUsageFixture(payload, prefix, h1Bytes, 1)
			}
		}
	}
	return f
}

// Includes an exact-cap sample in every phase on both devices. Android values
// above 32 MiB must pass, and every profile keeps its independently rounded root.
func TestMemsteadyReportAcceptsCompleteMemoryProfiles(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		f := memoryProfileReportFixture(t, profile)
		for _, samples := range [][]memsteadySample{f.client, f.provider} {
			for _, sample := range samples {
				switch sample.Millis {
				case 90000, 120000, 164000, 470000:
					sample.Payload["go_total_bytes"] = float64(f.meta.DeviceMemoryTargetBytes)
				}
			}
		}
		// Optional post-window diagnostics have no ownership evidence and may
		// exceed the acceptance ceiling without becoming acceptance samples.
		f.client = append(f.client, memsteadySample{Millis: f.meta.EndMillis + 2000,
			Payload: map[string]any{"go_total_bytes": float64(f.meta.DeviceMemoryTargetBytes + 1)}})
		summary, err := runReportFixture(t, f)
		if err != nil || !summary.Pass || !summary.P2pActive || len(summary.Breaches) != 0 ||
			summary.MemoryProfile != profile || summary.GoRuntimeLimitBytes != f.meta.DeviceMemoryTargetBytes {
			t.Fatalf("%s complete profile rejected: %+v, %v", profile, summary, err)
		}
		for _, side := range []memsteadySide{summary.Client, summary.Provider} {
			if side.Quiet.Samples != 151 || side.Quiet.MaxMiB != float64(f.meta.DeviceMemoryTargetBytes)/(1024*1024) ||
				side.TransportBudgetTotalBytes != f.meta.DeviceMemoryTargetBytes/4 || side.TransportBudgetMaxCount != 16 ||
				side.DeviceTransportBudgetTotalBytes != f.meta.DeviceMemoryTargetBytes/4 || side.DeviceTransportBudgetMaxCount != 16 ||
				side.TransferRootBudget.TotalBytes != f.meta.DeviceMemoryTargetBytes*9/20+f.meta.DeviceMemoryTargetBytes/5 ||
				side.NatBudget.TotalBytes != f.meta.DeviceMemoryTargetBytes/10 || side.PeerPinBudget.TotalBytes != 1024*1024 {
				t.Fatalf("%s summary lost exact profile evidence: %+v", profile, side)
			}
		}
	}
}

// A valid label cannot authorize altered metadata or samples from the other
// cohort, even when the measured runtime is far below both ceilings.
func TestMemsteadyReportRejectsMisboundMemoryProfiles(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, test := range []struct {
			name   string
			mutate func(*reportFixture)
		}{
			{name: "missing profile", mutate: func(f *reportFixture) { f.meta.MemoryProfile = "" }},
			{name: "unknown profile", mutate: func(f *reportFixture) { f.meta.MemoryProfile = "custom-memory" }},
			{name: "legacy profile", mutate: func(f *reportFixture) { f.meta.MemoryProfile = "ios-memory-audit-v1" }},
			{name: "opposite profile", mutate: func(f *reportFixture) {
				if f.meta.MemoryProfile == "android" {
					f.meta.MemoryProfile = "ios-memory-audit-v2"
				} else {
					f.meta.MemoryProfile = "android"
				}
			}},
			{name: "metadata target", mutate: func(f *reportFixture) { f.meta.DeviceMemoryTargetBytes++ }},
			{name: "metadata soft limit", mutate: func(f *reportFixture) { f.meta.ProcessMemoryLimitBytes++ }},
			{name: "metadata carrier root", mutate: func(f *reportFixture) { f.meta.ProcessTransportBytes++ }},
			{name: "metadata carrier slots", mutate: func(f *reportFixture) { f.meta.ProcessTransportCount = 32 }},
			{name: "sample target", mutate: func(f *reportFixture) {
				f.client[30].Payload["device_memory_target_bytes"] = float64(f.meta.DeviceMemoryTargetBytes - 1)
			}},
			{name: "sample soft limit", mutate: func(f *reportFixture) {
				f.provider[30].Payload["go_limit_bytes"] = float64(f.meta.ProcessMemoryLimitBytes + 1)
			}},
			{name: "missing target", mutate: func(f *reportFixture) { delete(f.client[30].Payload, "device_memory_target_bytes") }},
			{name: "missing soft limit", mutate: func(f *reportFixture) { delete(f.provider[30].Payload, "go_limit_bytes") }},
			{name: "numeric-string target", mutate: func(f *reportFixture) {
				f.client[30].Payload["device_memory_target_bytes"] = strconv.FormatInt(f.meta.DeviceMemoryTargetBytes, 10)
			}},
			{name: "numeric-string soft limit", mutate: func(f *reportFixture) {
				f.provider[30].Payload["go_limit_bytes"] = strconv.FormatInt(f.meta.ProcessMemoryLimitBytes, 10)
			}},
			{name: "fractional runtime", mutate: func(f *reportFixture) { f.client[30].Payload["go_total_bytes"] = 1.5 }},
			{name: "runtime string", mutate: func(f *reportFixture) { f.provider[30].Payload["go_total_bytes"] = "1" }},
		} {
			f := memoryProfileReportFixture(t, profile)
			test.mutate(&f)
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass {
				t.Fatalf("%s %s was certified: %+v", profile, test.name, summary)
			}
		}
	}
}

// Present row profile/rate evidence is never optional or coercible. Both sides
// retain these checks through the final quiet sample.
func TestMemsteadyMemoryProfilesRequireMatchingSampleEvidence(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, test := range []struct {
			name, field string
			value       any
		}{
			{name: "unknown profile", field: "memoryProfile", value: "unknown-memory-profile"},
			{name: "legacy profile", field: "memoryProfile", value: "ios-memory-audit-v1"},
			{name: "empty profile", field: "memoryProfile", value: ""},
			{name: "null profile", field: "memoryProfile", value: nil},
			{name: "invalid profile type", field: "memoryProfile", value: float64(0)},
			{name: "diagnostic rate", field: "memory_profile_rate_bytes", value: float64(65536)},
			{name: "negative rate", field: "memory_profile_rate_bytes", value: float64(-1)},
			{name: "fractional rate", field: "memory_profile_rate_bytes", value: 0.5},
			{name: "numeric-string rate", field: "memory_profile_rate_bytes", value: "0"},
			{name: "null rate", field: "memory_profile_rate_bytes", value: nil},
		} {
			f := memoryProfileReportFixture(t, profile)
			f.provider[len(f.provider)-1].Payload[test.field] = test.value
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Provider.Failures, ";"), test.field) {
				t.Fatalf("%s %s was certified: %+v, %v", profile, test.name, summary.Provider, err)
			}
		}
		f := memoryProfileReportFixture(t, profile)
		otherProfile := "android"
		if profile == "android" {
			otherProfile = "ios-memory-audit-v2"
		}
		f.client[30].Payload["memoryProfile"] = otherProfile
		summary, err := runReportFixture(t, f)
		if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "memoryProfile") {
			t.Fatalf("%s sample labelled %s was certified: %+v, %v", profile, otherProfile, summary.Client, err)
		}
	}
}

// Historical standalone iOS samples may omit the outer fields. The new
// Android report cohort must provide an explicit matching profile and zero rate.
func TestMemsteadyMemoryProfilesKeepOnlyIosSampleAbsenceCompatibility(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, field := range []string{"memoryProfile", "memory_profile_rate_bytes"} {
			f := memoryProfileReportFixture(t, profile)
			delete(f.client[30].Payload, field)
			summary, err := runReportFixture(t, f)
			if profile == "ios-memory-audit-v2" {
				if err != nil || !summary.Pass {
					t.Fatalf("standalone iOS absent %s rejected: %+v, %v", field, summary.Client, err)
				}
			} else if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), field) {
				t.Fatalf("Android absent %s was certified: %+v, %v", field, summary.Client, err)
			}
		}
	}
}

// Whole-block gap tolerance must not allow a completely absent short drain
// phase, and later quiet evidence cannot substitute for an absent earlier phase.
func TestMemsteadyMemoryProfilesRequireEveryPhase(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, phase := range []struct {
			name     string
			from, to int64
		}{
			{name: "baseline", from: 80000, to: 99999},
			{name: "burst", from: 100000, to: 160000},
			{name: "drain", from: 160001, to: 169999},
			{name: "quiet", from: 170000, to: 470000},
			{name: "quiet gap", from: 300000, to: 330000},
			{name: "quiet tail", from: 440000, to: 470000},
		} {
			f := memoryProfileReportFixture(t, profile)
			kept := []memsteadySample{}
			for _, sample := range f.client {
				if sample.Millis < phase.from || phase.to < sample.Millis {
					kept = append(kept, sample)
				}
			}
			f.client = kept
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || len(summary.Client.Failures) == 0 {
				t.Fatalf("%s missing %s was certified: %+v", profile, phase.name, summary)
			}
		}
	}
}

// One extra byte in any phase or on either device remains a failure, including
// a final quiet peak too rare to affect p95.
func TestMemsteadyMemoryProfilesEnforceEveryRuntimePeak(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, side := range []string{"client", "provider"} {
			for _, phase := range []struct {
				name   string
				millis int64
			}{
				{name: "baseline", millis: 90000}, {name: "burst", millis: 120000},
				{name: "drain", millis: 164000}, {name: "quiet", millis: 180000},
				{name: "quiet", millis: 470000},
			} {
				f := memoryProfileReportFixture(t, profile)
				samples := f.client
				if side == "provider" {
					samples = f.provider
				}
				for _, sample := range samples {
					if sample.Millis == phase.millis {
						sample.Payload["go_total_bytes"] = float64(f.meta.DeviceMemoryTargetBytes + 1)
					}
				}
				summary, err := runReportFixture(t, f)
				if err == nil || summary.Pass || len(summary.Breaches) != 1 || summary.Breaches[0].Side != side ||
					summary.Breaches[0].Phase != phase.name || summary.Breaches[0].Millis != phase.millis ||
					summary.GoRuntimeLimitBytes != f.meta.DeviceMemoryTargetBytes {
					t.Fatalf("%s %s %s peak was not preserved: %+v, %v", profile, side, phase.name, summary, err)
				}
			}
		}
	}
}

// Both profiles require complete timestamp parts, identity and workload proof;
// changing the ceiling does not weaken any of these independent gates.
func TestMemsteadyMemoryProfilesRetainCompleteEvidenceGates(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, test := range []struct {
			name   string
			mutate func(*reportFixture)
		}{
			{name: "carrier root", mutate: func(f *reportFixture) { f.client[30].Payload["transport_budget_total_bytes"] = float64(1) }},
			{name: "carrier device", mutate: func(f *reportFixture) { f.provider[30].Payload["device_transport_budget_total_bytes"] = float64(1) }},
			{name: "carrier root slots", mutate: func(f *reportFixture) { f.client[30].Payload["transport_budget_max_count"] = float64(32) }},
			{name: "carrier device slots", mutate: func(f *reportFixture) { f.provider[30].Payload["device_transport_budget_max_count"] = float64(32) }},
			{name: "carrier escaped root", mutate: func(f *reportFixture) {
				setCarrierUsageFixture(f.client[30].Payload, "device_transport_budget_", f.meta.ProcessTransportBytes, 2)
			}},
			{name: "carrier retained bytes", mutate: func(f *reportFixture) {
				last := f.client[len(f.client)-1].Payload
				setCarrierUsageFixture(last, "transport_budget_", int64(num(last, "transport_budget_used_bytes"))+1, 1)
			}},
			{name: "transfer root", mutate: func(f *reportFixture) {
				f.client[30].Payload["transfer_root_total_bytes"] = float64(f.meta.DeviceMemoryTargetBytes*9/20 + f.meta.DeviceMemoryTargetBytes/5 + 1)
			}},
			{name: "nat ceiling", mutate: func(f *reportFixture) {
				f.provider[30].Payload["nat_budget_total_bytes"] = float64(f.meta.DeviceMemoryTargetBytes/10 + 1)
			}},
			{name: "scaled pins", mutate: func(f *reportFixture) { f.client[30].Payload["peer_pin_total_bytes"] = float64(2 * 1024 * 1024) }},
			{name: "released live pins", mutate: func(f *reportFixture) { f.client[30].Payload["peer_pin_used_bytes"] = float64(0) }},
			{name: "pin failure", mutate: func(f *reportFixture) { f.provider[30].Payload["peer_pin_persistence_failures"] = float64(1) }},
			{name: "changed identity", mutate: func(f *reportFixture) { f.provider[30].Payload["client_id"] = "fixture-other-provider" }},
			{name: "changed pinned peer", mutate: func(f *reportFixture) { f.client[30].Payload["location_client_id"] = "fixture-other-provider" }},
			{name: "hidden load error", mutate: func(f *reportFixture) { f.meta.LoadErrors = 1 }},
			{name: "missing payload", mutate: func(f *reportFixture) { f.meta.LoadBytes = 0 }},
			{name: "restarted process", mutate: func(f *reportFixture) { f.meta.ProcessStable = false }},
			{name: "disconnected quiet", mutate: func(f *reportFixture) { f.client[len(f.client)-1].Payload["connect_enabled"] = false }},
			{name: "provider stopped", mutate: func(f *reportFixture) { f.provider[len(f.provider)-1].Payload["provide_mode"] = float64(0) }},
			{name: "temporary client retained", mutate: func(f *reportFixture) { f.client[len(f.client)-1].Payload["window_client_count"] = float64(2) }},
		} {
			f := memoryProfileReportFixture(t, profile)
			test.mutate(&f)
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass {
				t.Fatalf("%s %s bypassed evidence gates: %+v", profile, test.name, summary)
			}
		}
		for _, partName := range []string{"memory", "memory_device_transport", "memory_device_transfer"} {
			for _, shifted := range []bool{false, true} {
				f := memoryProfileReportFixture(t, profile)
				f.editPart = func(side string, millis int64, part map[string]any) bool {
					if side == "provider" && millis == 470000 && part["part"] == partName {
						if !shifted {
							return false
						}
						part["unix_millis"] = part["unix_millis"].(int64) + 1
					}
					return true
				}
				summary, err := runReportFixture(t, f)
				if err == nil || summary.Pass {
					t.Fatalf("%s incomplete %s (shifted=%t) was certified: %+v", profile, partName, shifted, summary)
				}
			}
		}
	}
}

// Uses a fully backed loan at both levels, including the profile's exact H1
// claim. Primary and active records must agree before a one-slot loan qualifies.
func TestMemsteadyMemoryProfilesRequireExactCarrierHandoffs(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, delta := range []int64{0, -1, 1} {
			f := memoryProfileReportFixture(t, profile)
			payload := f.client[30].Payload
			h1Bytes := f.meta.DeviceMemoryTargetBytes / 128
			for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
				setCarrierPairFixture(payload, prefix, "device", "h1", "h3_explicit", true)
				for _, pair := range []string{"pair_", "active_handoff_"} {
					payload[prefix+pair+"h1_bytes"] = float64(h1Bytes + delta)
					payload[prefix+pair+"bytes"] = float64(h1Bytes + delta)
				}
				setCarrierUsageFixture(payload, prefix, f.meta.ProcessTransportBytes+h1Bytes+delta, 17)
			}
			summary, err := runReportFixture(t, f)
			if delta == 0 {
				if err != nil || !summary.Pass || summary.Client.TransportBudgetHandoffBytes != h1Bytes ||
					summary.Client.DeviceTransportBudgetHandoffBytes != h1Bytes {
					t.Fatalf("%s exact handoff rejected: %+v, %v", profile, summary, err)
				}
			} else if err == nil || summary.Pass {
				t.Fatalf("%s handoff byte delta %d was certified: %+v", profile, delta, summary)
			}
		}
	}
}

// Concurrent root and device loans retain distinct identities and share every
// copy of their exact claim; additional evidence never adds a second allowance.
func TestMemsteadyMemoryProfilesValidateConcurrentCarrierPairs(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		for _, ownerless := range []bool{false, true} {
			for _, malformed := range []string{"", "underreported claim", "zero-slot pair", "extra loan", "hidden child pair"} {
				f := memoryProfileReportFixture(t, profile)
				payload := f.client[30].Payload
				h1Bytes := f.meta.DeviceMemoryTargetBytes / 128
				owner := "device"
				if ownerless {
					owner = "process"
				}
				setCarrierPairFixture(payload, "transport_budget_", owner, "h1", "h1", true)
				setCarrierPairFixture(payload, "device_transport_budget_", "device", "h1", "h3_explicit", true)
				for _, prefix := range []string{"transport_budget_", "device_transport_budget_"} {
					for _, pair := range []string{"pair_", "active_handoff_"} {
						payload[prefix+pair+"h1_bytes"] = float64(h1Bytes)
						payload[prefix+pair+"bytes"] = float64(h1Bytes)
					}
				}
				payload["transport_budget_pair_id"] = float64(8)
				payload["transport_budget_active_handoff_id"] = float64(8)
				copyCarrierPairEvidence(payload, "transport_budget_additional_pair_", "device_transport_budget_pair_")
				if !ownerless {
					copyCarrierPairEvidence(payload, "device_transport_budget_additional_pair_", "transport_budget_pair_")
				}
				// Ordinary headroom prevents an underreported overlap from being
				// rejected incidentally by the byte-ceiling check.
				setCarrierUsageFixture(payload, "transport_budget_", f.meta.ProcessTransportBytes, 11)
				setCarrierUsageFixture(payload, "device_transport_budget_", f.meta.ProcessTransportBytes, 9)
				switch malformed {
				case "underreported claim", "zero-slot pair":
					for _, prefix := range []string{"device_transport_budget_pair_", "device_transport_budget_active_handoff_", "transport_budget_additional_pair_"} {
						if malformed == "zero-slot pair" {
							payload[prefix+"slots"] = float64(0)
						} else {
							payload[prefix+"h1_bytes"] = float64(h1Bytes - 1)
							payload[prefix+"bytes"] = float64(h1Bytes - 1)
						}
					}
				case "extra loan":
					setCarrierUsageFixture(payload, "transport_budget_", f.meta.ProcessTransportBytes+2*h1Bytes, 11)
				case "hidden child pair":
					clearCarrierPairEvidence(payload, "transport_budget_additional_pair_")
				}
				summary, err := runReportFixture(t, f)
				if malformed == "" {
					if err != nil || !summary.Pass || summary.Client.TransportBudgetHandoffBytes != h1Bytes ||
						summary.Client.DeviceTransportBudgetHandoffBytes != h1Bytes {
						t.Fatalf("%s concurrent loans (ownerless=%t) rejected: %+v, %v", profile, ownerless, summary, err)
					}
				} else if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "carrier budget hierarchy:") {
					t.Fatalf("%s %s (ownerless=%t) was certified: %+v, %v", profile, malformed, ownerless, summary, err)
				}
			}
		}
	}
}

// Produces balanced cumulative reservations, a sustained simultaneous quiet
// dip and later fresh NAT admissions without retaining any other child owner.
func memoryProfileRecoveryReportFixture(t *testing.T, profile string) reportFixture {
	t.Helper()
	f := memoryProfileReportFixture(t, profile)
	for _, sample := range f.provider {
		root := float64(1024*1024 + 256*1024)
		nat := float64(64 * 1024)
		if f.meta.StartMillis <= sample.Millis && sample.Millis < f.meta.QuietStart+30000 {
			root += 128 * 1024
			nat += 128 * 1024
		} else if f.meta.QuietStart+30000 <= sample.Millis && sample.Millis <= f.meta.QuietStart+60000 {
			root -= 32 * 1024
			nat -= 32 * 1024
		} else if f.meta.QuietStart+60000 < sample.Millis {
			root += 32 * 1024
			nat += 32 * 1024
		}
		sample.Payload["transfer_root_used_bytes"] = root
		sample.Payload["nat_budget_used_bytes"] = nat
	}
	rebalanceRecoveryFixture(f.provider)
	return f
}

// Preserves the existing late-NAT exception only when a complete recovery
// witness exists; no runtime, retained Pack, or cumulative-counter error hides.
func TestMemsteadyMemoryProfilesPreserveRecoveryWitnessGates(t *testing.T) {
	for _, profile := range []string{"ios-memory-audit-v2", "android"} {
		f := memoryProfileRecoveryReportFixture(t, profile)
		summary, err := runReportFixture(t, f)
		witness := summary.Provider.TransferRecovery.Witness
		if err != nil || !summary.Pass || !summary.Provider.TransferRecovery.LateNatAdmissionAccepted ||
			witness == nil || witness.EndMillis-witness.StartMillis < 20000 || witness.Samples < 11 {
			t.Fatalf("%s sustained synthetic recovery rejected: %+v, %v", profile, summary.Provider, err)
		}
		for _, test := range []struct {
			name, failure string
			mutate        func(*reportFixture)
		}{
			{name: "single-sample dip", failure: "NAT bytes did not return", mutate: func(f *reportFixture) {
				for _, sample := range f.provider {
					if f.meta.QuietStart+30000 < sample.Millis && sample.Millis <= f.meta.QuietStart+60000 {
						sample.Payload["transfer_root_used_bytes"] = float64(1024*1024 + 256*1024)
						sample.Payload["nat_budget_used_bytes"] = float64(64 * 1024)
					}
				}
				rebalanceRecoveryFixture(f.provider)
			}},
			{name: "late runtime peak", failure: "runtime exceeds hard", mutate: func(f *reportFixture) {
				f.provider[len(f.provider)-1].Payload["go_total_bytes"] = float64(f.meta.DeviceMemoryTargetBytes + 1)
			}},
			{name: "retained Pack", failure: "Pack queue bytes did not return", mutate: func(f *reportFixture) {
				last := f.provider[len(f.provider)-1].Payload
				last["pack_queue_used_bytes"] = num(last, "pack_queue_used_bytes") + 1
			}},
			{name: "reserve rewind", failure: "cumulative reserve/release counters decreased", mutate: func(f *reportFixture) {
				last := f.provider[len(f.provider)-1].Payload
				last["nat_budget_reserved_bytes"] = num(last, "nat_budget_reserved_bytes") - 1
				last["nat_budget_released_bytes"] = num(last, "nat_budget_released_bytes") - 1
			}},
		} {
			f := memoryProfileRecoveryReportFixture(t, profile)
			test.mutate(&f)
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Provider.Failures, ";"), test.failure) {
				t.Fatalf("%s %s bypassed recovery gates: %+v, %v", profile, test.name, summary.Provider, err)
			}
		}
	}
}
