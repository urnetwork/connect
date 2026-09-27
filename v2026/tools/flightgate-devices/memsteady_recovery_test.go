package main

import (
	"encoding/csv"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Only the numeric transfer/runtime trajectory comes from the physical run.
// Unrelated report inputs use the usual passing fixture; no identities, flow
// destinations or credentials are retained. See testdata/README.md.
func physicalNatRecoveryFixture(t *testing.T) reportFixture {
	t.Helper()
	file, err := os.Open("testdata/memsteady-nat-recovery-a.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	f := newReportFixture()
	f.meta.BaselineStart = f.meta.StartMillis - 20814
	f.meta.BurstEnd = f.meta.StartMillis + 60699
	f.meta.QuietStart = f.meta.StartMillis + 71296
	f.meta.EndMillis = f.meta.StartMillis + 371818
	base := f.provider[0].Payload
	f.provider = nil
	for _, row := range rows[1:] {
		payload := make(map[string]any, len(base))
		for key, value := range base {
			payload[key] = value
		}
		var millis int64
		for i, cell := range row {
			value, err := strconv.ParseInt(cell, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				millis = f.meta.StartMillis + value
			} else {
				payload[rows[0][i]] = float64(value)
			}
		}
		payload["window_client_count"] = float64(0)
		payload["pack_queue_total_bytes"] = float64(1572864)
		count := max(float64(0), float64(millis-f.meta.StartMillis)/1000)
		payload["p2p"] = map[string]any{"FastReceiveMessageCount": count, "FastSendMessageCount": count}
		f.provider = append(f.provider, memsteadySample{Millis: millis, Payload: payload})
	}
	return f
}

func TestMemsteadyNatRecoveryPhysicalTrajectory(t *testing.T) {
	f := physicalNatRecoveryFixture(t)
	summary, err := runReportFixture(t, f)
	if err != nil || !summary.Pass {
		t.Fatalf("reclaimed physical burst rejected: %+v, %v", summary.Provider, err)
	}
	s := summary.Provider
	r := s.TransferRecovery
	if !r.LateNatAdmissionAccepted || r.Witness == nil {
		t.Fatalf("missing late admission evidence: %+v", r)
	}
	w := r.Witness
	if w.StartMillis < f.meta.QuietStart+30000 || w.EndMillis-w.StartMillis < 20000 || w.Samples < 2 ||
		w.RootUsedBytes >= s.TransferRootBudget.BaselineBytes || w.NatUsedBytes >= s.NatBudget.BaselineBytes {
		t.Fatalf("invalid sustained recovery witness: %+v", w)
	}
	if s.TransferRootBudget.BaselineBytes != 2902684 || s.TransferRootBudget.EndBytes != 3225532 ||
		s.NatBudget.BaselineBytes != 1329820 || s.NatBudget.EndBytes != 1652668 {
		t.Fatalf("physical baseline/end evidence changed: %+v", s)
	}
	if w.RootReservedAfterWitnessBytes-w.RootReleasedAfterWitnessBytes != s.TransferRootBudget.EndBytes-w.RootUsedBytes ||
		w.NatReservedAfterWitnessBytes-w.NatReleasedAfterWitnessBytes != s.NatBudget.EndBytes-w.NatUsedBytes ||
		w.NatReservedAfterWitnessBytes <= 0 || w.RootReservedAfterWitnessBytes < w.NatReservedAfterWitnessBytes {
		t.Fatalf("late claims are not balanced and parented: %+v", w)
	}
}

func rebalanceRecoveryFixture(samples []memsteadySample) {
	for _, prefix := range []string{"transfer_root_", "nat_budget_"} {
		var previous, reserved, released int64
		for _, sample := range samples {
			used := int64(num(sample.Payload, prefix+"used_bytes"))
			if used >= previous {
				reserved += used - previous
			} else {
				released += previous - used
			}
			sample.Payload[prefix+"reserved_bytes"] = float64(reserved)
			sample.Payload[prefix+"released_bytes"] = float64(released)
			previous = used
		}
	}
}

func TestMemsteadyNatRecoveryRequiresSustainedPostBurstWitness(t *testing.T) {
	for _, kind := range []string{"never recovered", "pre-burst dip", "burst dip", "drain dip", "early quiet dip", "single quiet dip", "short quiet dip", "disjoint minima", "Pack still retained"} {
		t.Run(kind, func(t *testing.T) {
			f := physicalNatRecoveryFixture(t)
			for _, sample := range f.provider {
				relative := sample.Millis - f.meta.StartMillis
				if sample.Millis >= f.meta.QuietStart {
					sample.Payload["nat_budget_used_bytes"] = float64(1329820 + 65536)
					sample.Payload["transfer_root_used_bytes"] = float64(2902684 + 65536)
				}
				dip := (kind == "pre-burst dip" && relative < -5000) ||
					(kind == "burst dip" && relative >= 0 && relative < 30000) ||
					(kind == "drain dip" && sample.Millis > f.meta.BurstEnd && sample.Millis < f.meta.QuietStart) ||
					(kind == "early quiet dip" && relative >= 71300 && relative < 101296) ||
					(kind == "single quiet dip" && relative >= 101000 && relative < 103000) ||
					(kind == "short quiet dip" && relative >= 101000 && relative < 111000) ||
					(kind == "Pack still retained" && relative >= 101000 && relative < 200000)
				if dip {
					sample.Payload["nat_budget_used_bytes"] = float64(1280368)
					sample.Payload["transfer_root_used_bytes"] = float64(2853232)
					if kind == "Pack still retained" {
						sample.Payload["pack_queue_used_bytes"] = float64(1)
					}
				}
				if kind == "disjoint minima" {
					if relative >= 101000 && relative < 200000 {
						sample.Payload["nat_budget_used_bytes"] = float64(1280368)
					} else if relative >= 200000 && relative < 300000 {
						sample.Payload["transfer_root_used_bytes"] = float64(2853232)
					}
				}
			}
			rebalanceRecoveryFixture(f.provider)
			summary, err := runReportFixture(t, f)
			failures := strings.Join(summary.Provider.Failures, ";")
			if err == nil || summary.Pass || summary.Provider.TransferRecovery.LateNatAdmissionAccepted ||
				!strings.Contains(failures, "NAT bytes did not return to the pre-burst baseline") || strings.Contains(failures, "transfer budget hierarchy:") {
				t.Fatalf("unproven recovery accepted or misattributed: %+v, %v", summary.Provider, err)
			}
		})
	}
}

func TestMemsteadyNatRecoveryPreservesOtherGates(t *testing.T) {
	for _, test := range []struct {
		name, failure string
		mutate        func(*reportFixture)
	}{
		{"extra root claim", "transfer root bytes did not return", func(f *reportFixture) {
			last := f.provider[len(f.provider)-1].Payload
			last["transfer_root_used_bytes"] = num(last, "transfer_root_used_bytes") + 1
			rebalanceRecoveryFixture(f.provider)
		}},
		{"reserve counter rewind", "cumulative reserve/release counters decreased", func(f *reportFixture) {
			last, previous := f.provider[len(f.provider)-1].Payload, f.provider[len(f.provider)-2].Payload
			last["nat_budget_reserved_bytes"] = num(previous, "nat_budget_reserved_bytes") - 1
			last["nat_budget_released_bytes"] = num(last, "nat_budget_reserved_bytes") - num(last, "nat_budget_used_bytes")
		}},
		{"release counter rewind", "cumulative reserve/release counters decreased", func(f *reportFixture) {
			last, previous := f.provider[len(f.provider)-1].Payload, f.provider[len(f.provider)-2].Payload
			last["nat_budget_released_bytes"] = num(previous, "nat_budget_released_bytes") - 1
			last["nat_budget_reserved_bytes"] = num(last, "nat_budget_released_bytes") + num(last, "nat_budget_used_bytes")
		}},
		{"unparented new NAT reservation", "NAT bytes did not return", func(f *reportFixture) {
			var reserve, release, previous float64
			for _, sample := range f.provider {
				used := num(sample.Payload, "transfer_root_used_bytes")
				reserve += max(used-previous, 0)
				release += max(previous-used, 0)
				sample.Payload["transfer_root_reserved_bytes"], sample.Payload["transfer_root_released_bytes"] = reserve, release
				previous = used
			}
		}},
		{"late runtime breach", "runtime exceeds hard 24 MiB", func(f *reportFixture) {
			f.provider[len(f.provider)-1].Payload["go_total_bytes"] = float64(memsteadyTargetBytes + 1)
		}},
		{"witness runtime breach", "runtime exceeds hard 24 MiB", func(f *reportFixture) {
			f.provider[80].Payload["go_total_bytes"] = float64(memsteadyTargetBytes + 1)
		}},
		{"NAT ceiling", "nat_budget_ exceeds its byte ceiling", func(f *reportFixture) {
			f.provider[len(f.provider)-1].Payload["nat_budget_used_bytes"] = float64(iosNatBudgetBytes + 1)
			rebalanceRecoveryFixture(f.provider)
		}},
		{"root ceiling", "transfer_root_ exceeds its byte ceiling", func(f *reportFixture) {
			f.provider[len(f.provider)-1].Payload["transfer_root_used_bytes"] = float64(iosTransferRootBytes + 1)
			rebalanceRecoveryFixture(f.provider)
		}},
		{"temporary client retained", "window clients 1 at end vs 0", func(f *reportFixture) {
			f.provider[len(f.provider)-1].Payload["window_client_count"] = float64(1)
		}},
		{"provider claim retained", "provider transfer bytes did not return", func(f *reportFixture) {
			last := f.provider[len(f.provider)-1].Payload
			last["provider_transfer_used_bytes"] = num(last, "provider_transfer_used_bytes") + 1
		}},
		{"Pack retained", "Pack queue bytes did not return", func(f *reportFixture) {
			f.provider[len(f.provider)-1].Payload["pack_queue_used_bytes"] = float64(1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := physicalNatRecoveryFixture(t)
			test.mutate(&f)
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Provider.Failures, ";"), test.failure) {
				t.Fatalf("%s escaped: %+v, %v", test.name, summary.Provider, err)
			}
		})
	}
}
