package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceCohortRequiresAllowlistedDevicesAndIgnoresOthers(t *testing.T) {
	a, b := allowedDeviceSerials()[0], allowedDeviceSerials()[1]
	valid := "List of devices attached\n" + a + " device product:husky model:Pixel_8_Pro\n" + b + " device model:SM_S928U1\n"
	for _, tc := range []struct {
		name, text string
		wantErr    bool
	}{
		{"both online", valid, false},
		{"other device", valid + "other-online device model:Other\n", false},
		{"unauthorized extras", valid + "other-a unauthorized\nother-b offline\n", false},
		{"extra cannot substitute", "List of devices attached\n" + a + " device\nother device\n", true},
		{"missing", "List of devices attached\n", true},
		{"unauthorized allowlist", strings.Replace(valid, a+" device", a+" unauthorized", 1), true},
		{"offline allowlist", strings.Replace(valid, b+" device", b+" offline", 1), true},
		{"duplicate allowlist", valid + a + " device\n", true},
		{"empty", "", true},
		{"missing header", a + " device\n" + b + " device", true},
		{"malformed allowlist", valid + a + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeviceCohort(tc.text)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v; want error %v", err, tc.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), a) || strings.Contains(err.Error(), b)) {
				t.Fatal("public error exposed a device serial")
			}
		})
	}
	if _, err := adb("unlisted", "shell", "true"); err == nil {
		t.Fatal("adb accepted an unlisted serial")
	}
}

func TestMemsteadyLoadAndRoles(t *testing.T) {
	a, b := allowedDeviceSerials()[0], allowedDeviceSerials()[1]
	if _, _, err := validateMemsteadyRoles(a, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateMemsteadyRoles(a, a); err == nil {
		t.Fatal("same-device roles accepted")
	}
	for _, tc := range []struct {
		name         string
		err          error
		start, end   string
		payload, tun int64
		wantErr      bool
	}{
		{"valid", nil, "tun0", "tun0", 1000, 1100, false},
		{"missing tunnel", nil, "", "", 1000, 1100, true},
		{"replaced tunnel", nil, "tun0", "tun1", 1000, 1100, true},
		{"failed helper", errors.New("exit 1"), "tun0", "tun0", 1000, 1100, true},
		{"no payload", nil, "tun0", "tun0", 0, 100, true},
		{"direct bypass", nil, "tun0", "tun0", 1000, 100, true},
		{"counter reset", nil, "tun0", "tun0", 1000, -100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMemsteadyLoad(tc.err, tc.start, tc.end, tc.payload, tc.tun)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type reportFixture struct {
	meta     memsteadyMeta
	client   []memsteadySample
	provider []memsteadySample
	editPart func(side string, millis int64, part map[string]any) bool
}

func newReportFixture() reportFixture {
	f := reportFixture{meta: memsteadyMeta{
		Tag: "fixture", Build: "test", ClientRole: "device-a", ProviderRole: "device-b",
		BurstSeconds: 60, QuietSeconds: 300, StartMillis: 100000, BurstEnd: 160000, QuietStart: 170000, EndMillis: 470000,
		TunRxBytes: 1100, LoadBytes: 1000, LoadComplete: true, CohortVerified: true, ProcessStable: true,
		ArtifactSHA256: strings.Repeat("a", 64), LoadSHA256: strings.Repeat("b", 64), BuildID: "fixture-build",
		ClockOffsets:  map[string]int64{"client": 500000, "provider": -40000},
		BaselineStart: 80000, MemoryProfile: iosMemoryAuditProfile,
		DeviceMemoryTargetBytes: iosDeviceTargetBytes, ProcessMemoryLimitBytes: iosProcessSoftLimitBytes,
		ProcessTransportBytes: iosCarrierRootBytes, ProcessTransportCount: iosCarrierRootMaxCount,
		ClientID: "fixture-client", ProviderID: "fixture-provider",
	}}
	for millis := int64(80000); millis <= f.meta.EndMillis; millis += 2000 {
		for _, side := range []string{"client", "provider"} {
			count := max(float64(0), float64(millis-f.meta.StartMillis)/1000)
			payload := map[string]any{
				"go_total_bytes": float64(20 * 1048576), "go_live_bytes": float64(8 * 1048576),
				"window_client_count": float64(1), "pool_outstanding": float64(3),
				"connect_enabled": side == "client", "location_network_peer": true, "location_is_device": true,
				"p2p":            map[string]any{"FastReceiveMessageCount": count, "FastSendMessageCount": count},
				"provide_mode":   float64(2),
				"go_limit_bytes": float64(iosProcessSoftLimitBytes), "device_memory_target_bytes": float64(iosDeviceTargetBytes),
				"client_id": "fixture-" + side, "location_client_id": "fixture-provider",
				"transport_budget_total_bytes":          float64(iosCarrierRootBytes),
				"transport_budget_used_bytes":           float64(256 * 1024),
				"transport_budget_max_count":            float64(iosCarrierRootMaxCount),
				"transport_budget_used_count":           float64(1),
				"transport_budget_pending_h1":           float64(0),
				"transport_budget_pending_h1_bytes":     float64(0),
				"transport_budget_reserved_bytes":       float64(256 * 1024),
				"transport_budget_released_bytes":       float64(0),
				"transport_budget_active_handoff_count": float64(0),
				"transport_budget_active_handoff_bytes": float64(0),
				"transport_budget_active_handoff_slots": float64(0),
				"transfer_root_total_bytes":             float64(iosTransferRootBytes),
				"transfer_root_used_bytes":              float64(iosPeerPinBudgetBytes + 256*1024),
				"transfer_root_reserved_bytes":          float64(iosPeerPinBudgetBytes + 256*1024),
				"transfer_root_released_bytes":          float64(0),
				"client_transfer_total_bytes":           float64(9 * 1024 * 1024),
				"client_transfer_used_bytes":            float64(iosPeerPinBudgetBytes + 128*1024),
				"provider_transfer_total_bytes":         float64(2 * 1024 * 1024),
				"provider_transfer_used_bytes":          float64(64 * 1024),
				"nat_budget_total_bytes":                float64(iosNatBudgetBytes),
				"nat_budget_used_bytes":                 float64(64 * 1024),
				"nat_budget_reserved_bytes":             float64(64 * 1024),
				"nat_budget_released_bytes":             float64(0),
				"pack_queue_total_bytes":                float64(256 * 1024),
				"pack_queue_used_bytes":                 float64(32 * 1024),
				"peer_pin_total_bytes":                  float64(iosPeerPinBudgetBytes),
				"peer_pin_used_bytes":                   float64(iosPeerPinBudgetBytes),
				"peer_pin_reserved_bytes":               float64(iosPeerPinBudgetBytes),
				"peer_pin_released_bytes":               float64(0),
				"peer_pin_count":                        float64(1),
				"peer_pin_capacity_refusals":            float64(0),
				"peer_pin_persistence_failures":         float64(0),
				"peer_pin_rollback_refusals":            float64(0),
				"peer_pin_state_failures":               float64(0),
			}
			for _, key := range []string{"active_handoff_id", "active_handoff_h1_bytes", "pair_id", "pair_h1_bytes", "pair_bytes", "pair_slots", "additional_pair_id", "additional_pair_h1_bytes", "additional_pair_bytes", "additional_pair_slots"} {
				payload["transport_budget_"+key] = float64(0)
			}
			for _, key := range []string{"active_handoff_from", "active_handoff_to", "pair_from", "pair_to", "pair_owner", "additional_pair_from", "additional_pair_to", "additional_pair_owner"} {
				payload["transport_budget_"+key] = ""
			}
			for key, value := range payload {
				if strings.HasPrefix(key, "transport_budget_") {
					payload["device_"+key] = value
				}
			}
			payload["device_transport_budget_total_bytes"] = float64(iosCarrierDeviceBytes)
			s := memsteadySample{Millis: millis, Payload: payload}
			if side == "client" {
				f.client = append(f.client, s)
			} else {
				f.provider = append(f.provider, s)
			}
		}
	}
	return f
}

func runReportFixture(t *testing.T, f reportFixture) (memsteadySummary, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJson(filepath.Join(dir, "meta.json"), f.meta); err != nil {
		t.Fatal(err)
	}
	for side, samples := range map[string][]memsteadySample{"client": f.client, "provider": f.provider} {
		var log strings.Builder
		for _, s := range samples {
			payload := map[string]any{"unix_millis": s.Millis + f.meta.ClockOffsets[side], "part": "memory"}
			devicePayload := map[string]any{"unix_millis": s.Millis + f.meta.ClockOffsets[side], "part": "memory_device_transport"}
			transferPayload := map[string]any{"unix_millis": s.Millis + f.meta.ClockOffsets[side], "part": "memory_device_transfer"}
			for key, value := range s.Payload {
				if strings.HasPrefix(key, "device_transport_budget_") {
					devicePayload[key] = value
				} else if isTransferBudgetField(key) {
					transferPayload[key] = value
				} else {
					payload[key] = value
				}
			}
			for _, part := range []map[string]any{payload, devicePayload, transferPayload} {
				if f.editPart != nil && !f.editPart(side, s.Millis, part) {
					continue
				}
				b, err := json.Marshal(part)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&log, "[flightgate] %s\n", b)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, side+".logcat"), []byte(log.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := memsteadyReport([]string{dir})
	b, readErr := os.ReadFile(filepath.Join(dir, "memsteady.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var summary memsteadySummary
	if json.Unmarshal(b, &summary) != nil {
		t.Fatal("invalid summary")
	}
	return summary, err
}

func TestMemsteadyReportRejectsInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*reportFixture)
	}{
		{"missing burst samples", func(f *reportFixture) {
			kept := []memsteadySample{}
			for _, s := range f.client {
				if s.Millis < f.meta.StartMillis || s.Millis > f.meta.BurstEnd {
					kept = append(kept, s)
				}
			}
			f.client = kept
		}},
		{"short quiet duration", func(f *reportFixture) { f.meta.EndMillis-- }},
		{"short quiet declaration", func(f *reportFixture) { f.meta.QuietSeconds = 299 }},
		{"quiet sample gap", func(f *reportFixture) { f.client = append(f.client[:60], f.client[90:]...) }},
		{"missing quiet tail", func(f *reportFixture) { f.client = f.client[:100] }},
		{"no baseline", func(f *reportFixture) { f.client = f.client[10:] }},
		{"no baseline runtime with valid state", func(f *reportFixture) {
			for _, sample := range f.client {
				if sample.Millis < f.meta.StartMillis {
					delete(sample.Payload, "go_total_bytes")
				}
			}
		}},
		{"no baseline interval", func(f *reportFixture) { f.meta.BaselineStart = 0 }},
		{"single missing memory part", func(f *reportFixture) { delete(f.client[20].Payload, "go_total_bytes") }},
		{"android profile", func(f *reportFixture) { f.meta.MemoryProfile = "android" }},
		{"wrong target", func(f *reportFixture) { f.meta.DeviceMemoryTargetBytes = 28 * 1048576 }},
		{"wrong manifest carrier root", func(f *reportFixture) { f.meta.ProcessTransportBytes = 16 * 1048576 }},
		{"wrong live soft limit", func(f *reportFixture) { f.client[15].Payload["go_limit_bytes"] = float64(40 * 1048576) }},
		{"missing live soft limit", func(f *reportFixture) { delete(f.client[15].Payload, "go_limit_bytes") }},
		{"wrong live target", func(f *reportFixture) { f.client[15].Payload["device_memory_target_bytes"] = float64(24 * 1048576) }},
		{"wrong peer identity", func(f *reportFixture) { f.client[15].Payload["location_client_id"] = "another-provider" }},
		{"wrong provider identity", func(f *reportFixture) { f.provider[15].Payload["client_id"] = "another-provider" }},
		{"missing runtime", func(f *reportFixture) {
			delete(f.client[40].Payload, "go_total_bytes")
			f.client[41].Payload["go_total_bytes"] = float64(0)
		}},
		{"load exited unsuccessfully", func(f *reportFixture) { f.meta.LoadComplete = false }},
		{"no helper payload", func(f *reportFixture) { f.meta.LoadBytes = 0 }},
		{"helper request errors", func(f *reportFixture) { f.meta.LoadErrors = 1 }},
		{"tunnel bypass", func(f *reportFixture) { f.meta.TunRxBytes = 20 }},
		{"app restart", func(f *reportFixture) { f.meta.ProcessStable = false }},
		{"allowlisted device lost", func(f *reportFixture) { f.meta.CohortVerified = false }},
		{"missing artifact", func(f *reportFixture) { f.meta.ArtifactSHA256 = "" }},
		{"missing load hash", func(f *reportFixture) { f.meta.LoadSHA256 = "" }},
		{"same roles", func(f *reportFixture) { f.meta.ProviderRole = f.meta.ClientRole }},
		{"disconnected during recovery", func(f *reportFixture) { f.client[70].Payload["connect_enabled"] = false }},
		{"not a pinned peer", func(f *reportFixture) { f.client[70].Payload["location_network_peer"] = false }},
		{"provider disabled", func(f *reportFixture) { f.provider[70].Payload["provide_mode"] = float64(0) }},
		{"temporary clients retained", func(f *reportFixture) { f.client[len(f.client)-1].Payload["window_client_count"] = float64(2) }},
		{"only old p2p traffic", func(f *reportFixture) {
			for _, s := range f.client {
				s.Payload["p2p"] = map[string]any{"FastReceiveMessageCount": float64(900)}
			}
		}},
		{"wrong p2p direction", func(f *reportFixture) {
			for _, s := range f.client {
				s.Payload["p2p"] = map[string]any{"FastSendMessageCount": float64(s.Millis)}
			}
		}},
		{"missing provider traffic", func(f *reportFixture) {
			for _, s := range f.provider {
				delete(s.Payload, "p2p")
			}
		}},
		{"missing carrier root", func(f *reportFixture) { delete(f.client[15].Payload, "transport_budget_total_bytes") }},
		{"wrong carrier root", func(f *reportFixture) { f.client[15].Payload["transport_budget_total_bytes"] = float64(16 * 1048576) }},
		{"carrier byte escape", func(f *reportFixture) {
			f.client[15].Payload["transport_budget_used_bytes"] = float64(iosCarrierRootBytes + 1)
		}},
		{"carrier slot escape", func(f *reportFixture) {
			f.client[15].Payload["transport_budget_used_count"] = float64(iosCarrierRootMaxCount + 1)
		}},
		{"carrier release imbalance", func(f *reportFixture) { f.client[15].Payload["transport_budget_released_bytes"] = float64(1) }},
		{"invalid carrier handoff", func(f *reportFixture) {
			f.client[15].Payload["transport_budget_active_handoff_count"] = float64(1)
		}},
		{"handoff left active", func(f *reportFixture) {
			last := f.client[len(f.client)-1].Payload
			last["transport_budget_active_handoff_count"] = float64(1)
			last["transport_budget_active_handoff_bytes"] = float64(256 * 1024)
			last["transport_budget_used_bytes"] = float64(512 * 1024)
			last["transport_budget_reserved_bytes"] = float64(512 * 1024)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReportFixture()
			tc.change(&f)
			summary, err := runReportFixture(t, f)
			if summary.Pass || err == nil {
				t.Fatalf("invalid block passed: summary=%+v error=%v", summary, err)
			}
		})
	}
}

func TestMemsteadyHardCapCoversEveryAcceptancePhase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		millis int64
	}{
		{"baseline", 90000}, {"burst", 120000}, {"drain", 164000}, {"quiet", 180000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReportFixture()
			for _, sample := range f.client {
				if sample.Millis == tc.millis {
					sample.Payload["go_total_bytes"] = float64(memsteadyTargetBytes + 1)
				}
			}
			s, err := runReportFixture(t, f)
			if err == nil || s.Pass || len(s.Breaches) != 1 || s.Breaches[0].Phase != tc.name {
				t.Fatalf("hard cap not enforced: %+v, %v", s, err)
			}
		})
	}
}

func TestMemsteadyPassingReportNormalizesDeviceClocks(t *testing.T) {
	f := newReportFixture()
	f.client[30].Payload["go_total_bytes"] = float64(memsteadyTargetBytes)
	// A forced heap profile outside the window is not an acceptance sample.
	f.client = append(f.client, memsteadySample{Millis: f.meta.EndMillis + 2000, Payload: map[string]any{"go_total_bytes": float64(40 * 1048576)}})
	s, err := runReportFixture(t, f)
	if err != nil || !s.Pass || s.Client.Burst.MaxMiB != 24 || s.Client.Quiet.Samples != 151 || !s.P2pActive {
		t.Fatalf("valid clock-normalized report failed: %+v, %v", s, err)
	}
}

func TestProcessCarrierBudgetAllowsOnlyTheReportedPairedHandoff(t *testing.T) {
	f := newReportFixture()
	sample := f.client[30].Payload
	setCarrierPairFixture(sample, "transport_budget_", "device", "h1", "h3_explicit", true)
	setCarrierPairFixture(sample, "device_transport_budget_", "device", "h1", "h3_explicit", true)
	setCarrierUsageFixture(sample, "transport_budget_", iosCarrierRootBytes+iosCarrierH1OverlapBytes, iosCarrierRootMaxCount+1)
	setCarrierUsageFixture(sample, "device_transport_budget_", iosCarrierDeviceBytes+iosCarrierH1OverlapBytes, iosCarrierRootMaxCount+1)
	s, err := runReportFixture(t, f)
	if err != nil || !s.Pass || s.Client.TransportBudgetHandoffBytes != 256*1024 {
		t.Fatalf("valid paired carrier handoff failed: %+v, %v", s, err)
	}
}

func TestMemsteadyCleanupPreservesRunAndCleanupErrors(t *testing.T) {
	runErr, cleanupErr := errors.New("failed block"), errors.New("failed disconnect")
	actions := []string{}
	err := withMemsteadyCleanup([]string{"client", "provider"}, func(command string, args []string) error {
		actions = append(actions, command+":"+args[1])
		if command == "disconnect" {
			return cleanupErr
		}
		return nil
	}, func() error { return runErr })
	if !errors.Is(err, runErr) || !errors.Is(err, cleanupErr) || strings.Join(actions, ",") != "disconnect:client,provide:client,disconnect:provider,provide:provider" {
		t.Fatalf("cleanup incomplete or hid errors: %v, %v", actions, err)
	}
}

func TestStatusMustMatchTheSameProvidingPeer(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`status {"peers":[{"device_name":"Pixel","provide_enabled":true}]}`, true},
		{`status {"peers":[{"device_name":"Pixel","provide_enabled":false},{"device_name":"Galaxy","provide_enabled":true}]}`, false},
		{`status {"peers":[{"device_name":"Pixel A","provide_enabled":true},{"device_name":"Pixel B","provide_enabled":true}]}`, false},
		{`status {"peers":[]}`, false},
	} {
		if got := statusHasProvidingPeer(tc.line, "pixel"); got != tc.want {
			t.Fatalf("status result %v, want %v: %s", got, tc.want, tc.line)
		}
	}
}

func TestStatusRequiresExactProvidingPeerID(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`status {"peers":[{"client_id":"wanted","device_name":"Same Name","provide_enabled":true}]}`, true},
		{`status {"peers":[{"client_id":"other","device_name":"Same Name","provide_enabled":true}]}`, false},
		{`status {"peers":[{"client_id":"wanted","provide_enabled":false},{"client_id":"other","provide_enabled":true}]}`, false},
		{`status {"peers":[{"client_id":"wanted","provide_enabled":true},{"client_id":"wanted","provide_enabled":true}]}`, false},
	} {
		if got := statusHasProvidingPeerID(tc.line, "wanted"); got != tc.want {
			t.Fatalf("exact peer match = %v, want %v: %s", got, tc.want, tc.line)
		}
	}
}

func TestLiveProfileMustBeTheIOSAuditArtifact(t *testing.T) {
	valid := memsteadyDeviceStatus{ClientID: "fixture-client", BuildID: "fixture-build", MemoryProfile: iosMemoryAuditProfile,
		DeviceMemoryTargetBytes: iosDeviceTargetBytes, ProcessMemoryLimitBytes: iosProcessSoftLimitBytes}
	if err := validateMemsteadyDeviceStatus(valid, "fixture-build"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*memsteadyDeviceStatus)
	}{
		{"android profile even at low values", func(s *memsteadyDeviceStatus) { s.MemoryProfile = "android" }},
		{"wrong target", func(s *memsteadyDeviceStatus) { s.DeviceMemoryTargetBytes = 28 * 1048576 }},
		{"wrong soft limit", func(s *memsteadyDeviceStatus) { s.ProcessMemoryLimitBytes = 40 * 1048576 }},
		{"stale build", func(s *memsteadyDeviceStatus) { s.BuildID = "old" }},
		{"missing identity", func(s *memsteadyDeviceStatus) { s.ClientID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.change(&s)
			if err := validateMemsteadyDeviceStatus(s, "fixture-build"); err == nil {
				t.Fatal("invalid live profile passed")
			}
		})
	}
}

func TestArtifactManifestRejectsDiagnosticAndIncompleteBuilds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eligible bool
		rate     int
		hash     string
		wantErr  bool
	}{
		{"acceptance", true, 0, strings.Repeat("a", 64), false},
		{"profile", true, 65536, strings.Repeat("a", 64), true},
		{"not eligible", false, 0, strings.Repeat("a", 64), true},
		{"incomplete", true, 0, "", true},
		{"invalid hash", true, 0, strings.Repeat("x", 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "build-manifest.json")
			if err := writeJson(path, acceptanceManifest{BuildID: "test", AcceptanceEligible: tc.eligible, MemProfileRate: tc.rate, APKSHA256: tc.hash,
				MemoryProfile: iosMemoryAuditProfile, DeviceMemoryTargetBytes: iosDeviceTargetBytes, ProcessMemoryLimitBytes: iosProcessSoftLimitBytes,
				ProcessTransportBytes: iosCarrierRootBytes, ProcessTransportCount: iosCarrierRootMaxCount}); err != nil {
				t.Fatal(err)
			}
			_, err := readAcceptanceManifest(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("manifest error: %v", err)
			}
		})
	}
}
