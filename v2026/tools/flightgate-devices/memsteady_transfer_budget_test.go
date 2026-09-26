package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setTransferUsageFixture(payload map[string]any, prefix string, used int64) {
	payload[prefix+"used_bytes"] = float64(used)
	if prefix == "transfer_root_" || prefix == "nat_budget_" || prefix == "peer_pin_" {
		payload[prefix+"reserved_bytes"] = float64(used)
		payload[prefix+"released_bytes"] = float64(0)
	}
}

func TestDeviceTransferBudgetRequiresEveryExactField(t *testing.T) {
	payload := newReportFixture().client[0].Payload
	if _, err := validateDeviceTransferBudget(payload); err != nil {
		t.Fatal(err)
	}
	for key, original := range payload {
		if !isTransferBudgetField(key) {
			continue
		}
		t.Run(key, func(t *testing.T) {
			delete(payload, key)
			if _, err := validateDeviceTransferBudget(payload); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("missing field accepted or unattributed: %v", err)
			}
			for _, invalid := range []any{nil, "0", -1.0, 0.5, math.NaN(), math.Inf(1), float64(1 << 53)} {
				payload[key] = invalid
				if _, err := validateDeviceTransferBudget(payload); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("invalid field %v accepted or unattributed: %v", invalid, err)
				}
			}
			payload[key] = original
		})
	}
}

func TestDeviceTransferBudgetChecksSubsetsWithoutSumming(t *testing.T) {
	payload := newReportFixture().client[0].Payload
	sample, err := validateDeviceTransferBudget(payload)
	if err != nil {
		t.Fatal(err)
	}
	// Pack is already included in the client group. Summing all four child
	// samples would reject these legitimate bytes even though the root is exact.
	if sample.Client.UsedBytes+sample.Provider.UsedBytes+sample.Nat.UsedBytes+sample.Pack.UsedBytes <= sample.Root.UsedBytes {
		t.Fatal("fixture does not exercise non-additive child usage")
	}
	for _, prefix := range []string{"client_transfer_", "provider_transfer_", "nat_budget_", "pack_queue_"} {
		t.Run(prefix, func(t *testing.T) {
			payload := newReportFixture().client[0].Payload
			for _, child := range []string{"client_transfer_", "provider_transfer_", "nat_budget_", "pack_queue_"} {
				setTransferUsageFixture(payload, child, 0)
			}
			setTransferUsageFixture(payload, "transfer_root_", 1)
			setTransferUsageFixture(payload, prefix, 2)
			if _, err := validateDeviceTransferBudget(payload); err == nil || !strings.Contains(err.Error(), prefix+" claims escape") {
				t.Fatalf("child escaped the root: %v", err)
			}
		})
	}
}

func TestMemsteadyTransferBudgetRejectsInvalidEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong root", func(p map[string]any) { p["transfer_root_total_bytes"] = float64(14 * 1024 * 1024) }},
		{"wrong NAT", func(p map[string]any) { p["nat_budget_total_bytes"] = float64(3 * 1024 * 1024) }},
		{"root escape", func(p map[string]any) { setTransferUsageFixture(p, "transfer_root_", iosTransferRootBytes+1) }},
		{"NAT escape", func(p map[string]any) { setTransferUsageFixture(p, "nat_budget_", iosNatBudgetBytes+1) }},
		{"client escape", func(p map[string]any) { p["client_transfer_total_bytes"] = float64(1) }},
		{"provider escape", func(p map[string]any) { p["provider_transfer_total_bytes"] = float64(1) }},
		{"Pack escape", func(p map[string]any) { p["pack_queue_total_bytes"] = float64(1) }},
		{"hidden client charge", func(p map[string]any) { setTransferUsageFixture(p, "client_transfer_", 512*1024) }},
		{"missing teardown ledger", func(p map[string]any) { delete(p, "transfer_root_released_bytes") }},
		{"missing NAT ledger", func(p map[string]any) { delete(p, "nat_budget_released_bytes") }},
		{"root unbalanced", func(p map[string]any) { p["transfer_root_released_bytes"] = float64(1) }},
		{"NAT unbalanced", func(p map[string]any) { p["nat_budget_released_bytes"] = float64(1) }},
		{"root negative balance", func(p map[string]any) { p["transfer_root_released_bytes"] = float64(iosTransferRootBytes) }},
		{"NAT negative balance", func(p map[string]any) { p["nat_budget_released_bytes"] = float64(iosNatBudgetBytes) }},
		{"false root teardown", func(p map[string]any) { p["transfer_root_used_bytes"] = float64(0) }},
		{"false NAT teardown", func(p map[string]any) { p["nat_budget_used_bytes"] = float64(0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReportFixture()
			test.mutate(f.client[len(f.client)-1].Payload)
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "transfer budget hierarchy:") {
				t.Fatalf("invalid transfer evidence accepted: %+v, %v", summary.Client, err)
			}
		})
	}
}

func TestMemsteadyTransferBudgetRequiresSameTimestampPart(t *testing.T) {
	for _, mutation := range []string{"missing", "mismatched timestamp", "wrong part", "fields without part"} {
		t.Run(mutation, func(t *testing.T) {
			f := newReportFixture()
			f.editPart = func(side string, millis int64, part map[string]any) bool {
				if side != "client" || millis != f.meta.EndMillis {
					return true
				}
				if mutation == "fields without part" && part["part"] == "memory" {
					for key, value := range f.client[len(f.client)-1].Payload {
						if isTransferBudgetField(key) {
							part[key] = value
						}
					}
				}
				if part["part"] != "memory_device_transfer" {
					return true
				}
				switch mutation {
				case "missing", "fields without part":
					return false
				case "mismatched timestamp":
					part["unix_millis"] = part["unix_millis"].(int64) - 1
				case "wrong part":
					part["part"] = "state"
				}
				return true
			}
			summary, err := runReportFixture(t, f)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "missing same-timestamp memory_device_transfer part") {
				t.Fatalf("unjoined transfer evidence accepted: %+v, %v", summary.Client, err)
			}
		})
	}
}

func TestMemsteadyTransferBudgetRejectsSubCeilingRecoveryLeaks(t *testing.T) {
	for _, test := range []struct{ prefix, name string }{
		{"transfer_root_", "transfer root"}, {"client_transfer_", "client transfer"},
		{"provider_transfer_", "provider transfer"}, {"nat_budget_", "NAT"}, {"pack_queue_", "Pack queue"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReportFixture()
			last := f.client[len(f.client)-1].Payload
			setTransferUsageFixture(last, test.prefix, int64(last[test.prefix+"used_bytes"].(float64))+1)
			summary, err := runReportFixture(t, f)
			failures := strings.Join(summary.Client.Failures, ";")
			if err == nil || summary.Pass || !strings.Contains(failures, test.name+" bytes did not return to the pre-burst baseline") || strings.Contains(failures, "transfer budget hierarchy:") {
				t.Fatalf("valid sub-ceiling leak was not attributed to recovery: %+v, %v", summary.Client, err)
			}
		})
	}
}

func TestMemsteadyTransferBudgetReportsBaselinePeakAndReleasedEnd(t *testing.T) {
	f := newReportFixture()
	const rootPeak, natPeak = int64(iosPeerPinBudgetBytes + 1024*1024), int64(384 * 1024)
	for _, sample := range f.client {
		if sample.Millis < f.meta.StartMillis {
			continue
		}
		p := sample.Payload
		if sample.Millis <= f.meta.BurstEnd {
			for prefix, used := range map[string]int64{
				"transfer_root_": rootPeak, "client_transfer_": iosPeerPinBudgetBytes + 512*1024,
				"provider_transfer_": 128 * 1024, "nat_budget_": natPeak, "pack_queue_": 96 * 1024,
			} {
				setTransferUsageFixture(p, prefix, used)
			}
		} else if sample.Millis == f.meta.EndMillis {
			for _, prefix := range []string{"transfer_root_", "client_transfer_", "provider_transfer_", "nat_budget_", "pack_queue_"} {
				used := int64(0)
				if prefix == "transfer_root_" || prefix == "client_transfer_" {
					used = iosPeerPinBudgetBytes // connected device retains its pin store
				}
				setTransferUsageFixture(p, prefix, used)
			}
		}
		for prefix, reserved := range map[string]int64{"transfer_root_": rootPeak, "nat_budget_": natPeak} {
			p[prefix+"reserved_bytes"] = float64(reserved)
			p[prefix+"released_bytes"] = float64(reserved) - p[prefix+"used_bytes"].(float64)
		}
	}
	summary, err := runReportFixture(t, f)
	if err != nil || !summary.Pass {
		t.Fatalf("valid non-additive transfer recovery failed: %+v, %v", summary.Client, err)
	}
	for _, test := range []struct {
		name                       string
		got                        memsteadyByteBudget
		total, baseline, peak, end int64
	}{
		{"root", summary.Client.TransferRootBudget, iosTransferRootBytes, iosPeerPinBudgetBytes + 256*1024, rootPeak, iosPeerPinBudgetBytes},
		{"client", summary.Client.ClientTransferBudget, 9 * 1024 * 1024, iosPeerPinBudgetBytes + 128*1024, iosPeerPinBudgetBytes + 512*1024, iosPeerPinBudgetBytes},
		{"provider", summary.Client.ProviderTransferBudget, 2 * 1024 * 1024, 64 * 1024, 128 * 1024, 0},
		{"NAT", summary.Client.NatBudget, iosNatBudgetBytes, 64 * 1024, natPeak, 0},
		{"Pack", summary.Client.PackQueueBudget, 256 * 1024, 32 * 1024, 96 * 1024, 0},
		{"pins", summary.Client.PeerPinBudget, iosPeerPinBudgetBytes, iosPeerPinBudgetBytes, iosPeerPinBudgetBytes, iosPeerPinBudgetBytes},
	} {
		want := memsteadyByteBudget{TotalBytes: test.total, BaselineBytes: test.baseline, MaxUsedBytes: test.peak, EndBytes: test.end}
		if test.got != want {
			t.Errorf("%s summary = %+v, want %+v", test.name, test.got, want)
		}
	}
}

func TestMemsteadyPeerPinBudgetRequiresLiveExactFailureFreeOwner(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing child", func(p map[string]any) { delete(p, "peer_pin_total_bytes") }},
		{"wrong child", func(p map[string]any) { p["peer_pin_total_bytes"] = float64(2 * iosPeerPinBudgetBytes) }},
		{"released live owner", func(p map[string]any) { setTransferUsageFixture(p, "peer_pin_", 0) }},
		{"undercharged owner", func(p map[string]any) { setTransferUsageFixture(p, "peer_pin_", iosPeerPinBudgetBytes-1) }},
		{"unbalanced owner", func(p map[string]any) { p["peer_pin_released_bytes"] = float64(1) }},
		{"missing release evidence", func(p map[string]any) { delete(p, "peer_pin_released_bytes") }},
		{"root escape", func(p map[string]any) {
			setTransferUsageFixture(p, "transfer_root_", iosPeerPinBudgetBytes-1)
			setTransferUsageFixture(p, "client_transfer_", iosPeerPinBudgetBytes-1)
		}},
		{"client escape", func(p map[string]any) { setTransferUsageFixture(p, "client_transfer_", iosPeerPinBudgetBytes-1) }},
		{"peer count escape", func(p map[string]any) { p["peer_pin_count"] = float64(257) }},
		{"capacity refused", func(p map[string]any) { p["peer_pin_capacity_refusals"] = float64(1) }},
		{"persistence failed", func(p map[string]any) { p["peer_pin_persistence_failures"] = float64(1) }},
		{"rollback refused", func(p map[string]any) { p["peer_pin_rollback_refusals"] = float64(1) }},
		{"owner state failed", func(p map[string]any) { p["peer_pin_state_failures"] = float64(1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReportFixture()
			test.mutate(fixture.client[len(fixture.client)-1].Payload)
			summary, err := runReportFixture(t, fixture)
			if err == nil || summary.Pass || !strings.Contains(strings.Join(summary.Client.Failures, ";"), "transfer budget hierarchy:") {
				t.Fatalf("invalid pin admission evidence accepted: %+v %v", summary.Client, err)
			}
		})
	}
	fixture := newReportFixture()
	for _, sample := range fixture.client {
		sample.Payload["peer_pin_count"] = float64(256)
	}
	summary, err := runReportFixture(t, fixture)
	if err != nil || !summary.Pass || summary.Client.PeerPinBudget.EndBytes != iosPeerPinBudgetBytes {
		t.Fatalf("full, healthy, still-connected pin store rejected: %+v %v", summary.Client, err)
	}
}

func TestParseDiagJoinsTransferBudgetLogcatFragments(t *testing.T) {
	payload := newReportFixture().client[0].Payload
	transfer := map[string]any{"part": "memory_device_transfer", "unix_millis": int64(123456)}
	for key, value := range payload {
		if isTransferBudgetField(key) {
			transfer[key] = value
		}
	}
	// Future additive fields can push this part across the gomobile stdout
	// bridge's 1,024-byte boundary. Keep reassembly independent of part type.
	transfer["future_evidence"] = strings.Repeat("x", 2048)
	encoded, err := json.Marshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	fmt.Fprintln(&log, `09-17 01:02:03.000 I GoLog   : I0917 01:02:03 [flightgate] {"part":"memory","unix_millis":123456,"go_total_bytes":20971520}`)
	for start := 0; start < len(encoded); start += 1024 {
		prefix := ""
		if start == 0 {
			prefix = "I0917 01:02:03 [flightgate] "
		}
		fmt.Fprintf(&log, "09-17 01:02:03.000 I GoLog   : %s%s\n", prefix, encoded[start:min(start+1024, len(encoded))])
	}
	fmt.Fprintln(&log, `09-17 01:02:03.000 I GoLog   : I0917 01:02:03 [flightgate] {"part":"memory_device_transport","unix_millis":123456,"device_transport_budget_total_bytes":5242880}`)
	path := filepath.Join(t.TempDir(), "logcat")
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	samples, err := parseDiag(path)
	if err != nil || len(samples) != 1 {
		t.Fatalf("joined sample = %+v, %v", samples, err)
	}
	sample := samples[0]
	if sample.Millis != 123456 || !sample.Parts["memory"] || !sample.Parts["memory_device_transport"] || !sample.Parts["memory_device_transfer"] ||
		num(sample.Payload, "go_total_bytes") != 20*1024*1024 || num(sample.Payload, "device_transport_budget_total_bytes") != iosCarrierDeviceBytes {
		t.Fatalf("same-timestamp parts were not retained: %+v", sample)
	}
	if _, err := validateDeviceTransferBudget(sample.Payload); err != nil {
		t.Fatalf("fragmented transfer fields were lost: %v", err)
	}
}

func TestParseDiagRejectsInvalidTimestamp(t *testing.T) {
	for _, test := range []struct {
		name      string
		timestamp any
	}{
		{"missing", nil}, {"string", "123456"}, {"fractional", 123456.5},
		{"negative", -1.0}, {"inexact integer", float64(1 << 53)},
	} {
		t.Run(test.name, func(t *testing.T) {
			part := map[string]any{"part": "memory_device_transfer"}
			if test.timestamp != nil {
				part["unix_millis"] = test.timestamp
			}
			encoded, err := json.Marshal(part)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "logcat")
			log := "[flightgate] {\"part\":\"memory\",\"unix_millis\":123456,\"go_total_bytes\":20971520}\n[flightgate] " + string(encoded) + "\n"
			if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := parseDiag(path); err == nil || !strings.Contains(err.Error(), "invalid diagnostic timestamp") {
				t.Fatalf("invalid part timestamp was silently rounded/joined: %v", err)
			}
		})
	}
}
