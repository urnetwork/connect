package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The MEMSTEADY mobile acceptance rules (connect/MEMSTEADY.md, "Scope and
// acceptance signals"), applied to goRuntimeBytes (the SDK sampler's
// go_total_bytes, logged by the transfer diagnostic seam every interval):
// every sampled phase at or below the iOS profile's hard 24 MiB cap, including
// five quiet connected minutes after a burst, and every temporary client
// released. There is no grace band above the cap.
const memsteadyTargetBytes = 24 * 1024 * 1024

type memsteadySample struct {
	Millis  int64
	Payload map[string]any
	Parts   map[string]bool
}

type memsteadyPhase struct {
	Samples int     `json:"samples"`
	P50MiB  float64 `json:"p50_mib"`
	P95MiB  float64 `json:"p95_mib"`
	MaxMiB  float64 `json:"max_mib"`
	LastMiB float64 `json:"last_mib"`
	PssP50  float64 `json:"pss_p50_mib"`
	PssMax  float64 `json:"pss_max_mib"`
}

type memsteadyBreach struct {
	Side    string         `json:"side"`
	Phase   string         `json:"phase"`
	Millis  int64          `json:"millis"`
	MiB     float64        `json:"mib"`
	Memory  map[string]any `json:"memory"`
	Windows int            `json:"window_client_count"`
}

type memsteadyByteBudget struct {
	TotalBytes    int64 `json:"total_bytes"`
	MaxUsedBytes  int64 `json:"max_used_bytes"`
	BaselineBytes int64 `json:"baseline_used_bytes"`
	EndBytes      int64 `json:"end_used_bytes"`
}

type memsteadySide struct {
	Role                               string                    `json:"role"`
	Burst                              memsteadyPhase            `json:"burst"`
	Quiet                              memsteadyPhase            `json:"quiet"`
	WindowClientsBase                  int                       `json:"window_clients_before_burst"`
	WindowClientsBurst                 int                       `json:"window_clients_burst_max"`
	WindowClientsEnd                   int                       `json:"window_clients_end"`
	PoolOutstandingEnd                 int64                     `json:"pool_outstanding_end"`
	TransportBudgetTotalBytes          int64                     `json:"transport_budget_total_bytes"`
	TransportBudgetMaxUsedBytes        int64                     `json:"transport_budget_max_used_bytes"`
	TransportBudgetBaselineBytes       int64                     `json:"transport_budget_baseline_used_bytes"`
	TransportBudgetEndBytes            int64                     `json:"transport_budget_end_used_bytes"`
	TransportBudgetMaxCount            int64                     `json:"transport_budget_max_count"`
	TransportBudgetPeakCount           int64                     `json:"transport_budget_peak_used_count"`
	TransportBudgetBaselineCount       int64                     `json:"transport_budget_baseline_used_count"`
	TransportBudgetEndCount            int64                     `json:"transport_budget_end_used_count"`
	TransportBudgetHandoffBytes        int64                     `json:"transport_budget_max_handoff_bytes"`
	DeviceTransportBudgetTotalBytes    int64                     `json:"device_transport_budget_total_bytes"`
	DeviceTransportBudgetMaxUsedBytes  int64                     `json:"device_transport_budget_max_used_bytes"`
	DeviceTransportBudgetBaselineBytes int64                     `json:"device_transport_budget_baseline_used_bytes"`
	DeviceTransportBudgetEndBytes      int64                     `json:"device_transport_budget_end_used_bytes"`
	DeviceTransportBudgetMaxCount      int64                     `json:"device_transport_budget_max_count"`
	DeviceTransportBudgetPeakCount     int64                     `json:"device_transport_budget_peak_used_count"`
	DeviceTransportBudgetBaselineCount int64                     `json:"device_transport_budget_baseline_used_count"`
	DeviceTransportBudgetEndCount      int64                     `json:"device_transport_budget_end_used_count"`
	DeviceTransportBudgetHandoffBytes  int64                     `json:"device_transport_budget_max_handoff_bytes"`
	TransferRootBudget                 memsteadyByteBudget       `json:"transfer_root_budget"`
	ClientTransferBudget               memsteadyByteBudget       `json:"client_transfer_budget"`
	ProviderTransferBudget             memsteadyByteBudget       `json:"provider_transfer_budget"`
	NatBudget                          memsteadyByteBudget       `json:"nat_budget"`
	PackQueueBudget                    memsteadyByteBudget       `json:"pack_queue_budget"`
	PeerPinBudget                      memsteadyByteBudget       `json:"peer_pin_budget"`
	TransferRecovery                   memsteadyTransferRecovery `json:"transfer_recovery"`
	Pass                               bool                      `json:"pass"`
	Failures                           []string                  `json:"failures"`
}

type memsteadySummary struct {
	Tag          string            `json:"tag"`
	Build        string            `json:"build"`
	ClientRole   string            `json:"client_role"`
	ProviderRole string            `json:"provider_role"`
	BurstSeconds int               `json:"burst_seconds"`
	QuietSeconds int               `json:"quiet_seconds"`
	BurstMbps    float64           `json:"burst_mbps"`
	LoadErrors   int64             `json:"load_errors"`
	P2pActive    bool              `json:"p2p_active"`
	Client       memsteadySide     `json:"client"`
	Provider     memsteadySide     `json:"provider"`
	Breaches     []memsteadyBreach `json:"breaches"`
	Pass         bool              `json:"pass"`
	Failures     []string          `json:"failures"`
}

type memsteadyMeta struct {
	Tag                     string           `json:"tag"`
	Build                   string           `json:"build"`
	ClientRole              string           `json:"client_role"`
	ProviderRole            string           `json:"provider_role"`
	BurstSeconds            int              `json:"burst_seconds"`
	QuietSeconds            int              `json:"quiet_seconds"`
	StartMillis             int64            `json:"start_millis"`
	BurstEnd                int64            `json:"burst_end_millis"`
	QuietStart              int64            `json:"quiet_start_millis"`
	EndMillis               int64            `json:"end_millis"`
	TunRxBytes              int64            `json:"burst_tun_rx_bytes"`
	AppVersion              string           `json:"app_version"`
	LoadBytes               int64            `json:"load_bytes"`
	LoadErrors              int64            `json:"load_errors"`
	LoadComplete            bool             `json:"load_complete"`
	CohortVerified          bool             `json:"cohort_verified"`
	ArtifactSHA256          string           `json:"artifact_sha256"`
	LoadSHA256              string           `json:"load_sha256"`
	BuildID                 string           `json:"build_id"`
	ClockOffsets            map[string]int64 `json:"device_clock_offsets_millis"`
	ProcessStable           bool             `json:"process_stable"`
	BaselineStart           int64            `json:"baseline_start_millis"`
	MemoryProfile           string           `json:"memory_profile"`
	DeviceMemoryTargetBytes int64            `json:"device_memory_target_bytes"`
	ProcessMemoryLimitBytes int64            `json:"process_memory_limit_bytes"`
	ProcessTransportBytes   int64            `json:"process_transport_budget_bytes"`
	ProcessTransportCount   int64            `json:"process_transport_max_count"`
	ClientID                string           `json:"client_id"`
	ProviderID              string           `json:"provider_id"`
}

// pssSample is one whole-app PSS reading (dumpsys meminfo TOTAL PSS), the
// secondary signal 13.7's kernel socket buffers show up in.
type pssSample struct {
	Millis int64  `json:"millis"`
	Side   string `json:"side"`
	PssKiB int64  `json:"pss_kib"`
}

func readPss(serial string) int64 {
	out, _ := adbShell(serial, "dumpsys meminfo "+appPackage+" 2>/dev/null | grep -m1 'TOTAL PSS:' ")
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "PSS:" && i+1 < len(fields) {
			v, _ := strconv.ParseInt(fields[i+1], 10, 64)
			return v
		}
	}
	return 0
}

// runMemsteady is one MEMSTEADY block on a connected tunnel with the p2p
// lane live: a burst, then quiet connected minutes, capturing the [flightgate]
// memory lines on both devices and whole-app PSS every 15 s.
func runMemsteady(args []string) error {
	fs := flag.NewFlagSet("memsteady", flag.ContinueOnError)
	client := fs.String("client", "", "client device serial")
	provider := fs.String("provider", "", "provider device serial")
	out := fs.String("out", "", "new run directory")
	tag := fs.String("tag", "", "run tag")
	build := fs.String("build", "", "build label")
	manifestPath := fs.String("build-manifest", "", "build-item manifest matching the APK installed on both devices")
	loadSHA := fs.String("load-sha256", "", "SHA-256 printed by load-build for the current helper")
	burstSeconds := fs.Int("burst-seconds", 60, "burst length")
	quietSeconds := fs.Int("quiet-seconds", 300, "quiet connected window after the burst (at least 300 seconds)")
	streams := fs.Int("streams", 4, "parallel download streams")
	url := fs.String("url", defaultLoadUrl, "download URL")
	heapProfiles := fs.Bool("heap-profile", false, "capture a diagnostic heap profile after acceptance samples")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clientRole, providerRole, err := validateMemsteadyRoles(*client, *provider)
	if err != nil {
		return err
	}
	if *out == "" || *manifestPath == "" || len(*loadSHA) != 64 || *burstSeconds <= 0 || *quietSeconds < 300 || *streams <= 0 {
		return errors.New("--out, --build-manifest, --load-sha256, positive burst/streams, and at least 300 quiet seconds are required")
	}
	if err := requireDeviceCohort(); err != nil {
		return err
	}
	manifest, err := readAcceptanceManifest(*manifestPath)
	if err != nil {
		return err
	}
	if err := verifyDeviceFile(*client, loadBinary, *loadSHA); err != nil {
		return fmt.Errorf("load helper provenance: %w", err)
	}
	processes := map[string]string{}
	identities := map[string]string{}
	offsets := map[string]int64{}
	for _, side := range []struct{ serial, name string }{{*client, "client"}, {*provider, "provider"}} {
		if err := verifyInstalledArtifact(side.serial, manifest.APKSHA256); err != nil {
			return fmt.Errorf("%s artifact: %w", side.name, err)
		}
		status, err := readMemsteadyDeviceStatus(side.serial)
		if err != nil {
			return err
		}
		if err := validateMemsteadyDeviceStatus(status, manifest.BuildID); err != nil {
			return fmt.Errorf("%s: %w", side.name, err)
		}
		identities[side.name] = status.ClientID
		pid, err := adbShell(side.serial, "pidof "+appPackage)
		if err != nil || pid == "" {
			return fmt.Errorf("%s app process is not running", side.name)
		}
		processes[side.name] = pid
		before := time.Now().UnixMilli()
		clock, err := adbShell(side.serial, "date +%s%3N")
		after := time.Now().UnixMilli()
		millis, parseErr := strconv.ParseInt(clock, 10, 64)
		if err != nil || parseErr != nil || millis < 1000000000000 {
			return fmt.Errorf("%s: cannot measure device clock offset", side.name)
		}
		offsets[side.name] = millis - (before+after)/2
	}
	tunName, _, _ := tunCounters(*client)
	if tunName == "" || !tunnelRoutesShell(*client) {
		return errors.New("client tunnel is missing or does not carry the load helper's traffic")
	}
	// Refuse reuse so a partial new attempt cannot inherit old passing logs.
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(*out, 0o700); err != nil {
		return fmt.Errorf("new run directory: %w", err)
	}
	appVersion, err := adbShell(*client, "dumpsys package "+appPackage+" | grep -m1 versionName | sed 's/.*=//'")
	if err != nil {
		return err
	}
	meta := memsteadyMeta{Tag: *tag, Build: *build, ClientRole: clientRole, ProviderRole: providerRole,
		BurstSeconds: *burstSeconds, QuietSeconds: *quietSeconds, AppVersion: strings.TrimSpace(appVersion),
		CohortVerified: true, ArtifactSHA256: manifest.APKSHA256, LoadSHA256: *loadSHA, BuildID: manifest.BuildID, ClockOffsets: offsets,
		MemoryProfile: manifest.MemoryProfile, DeviceMemoryTargetBytes: manifest.DeviceMemoryTargetBytes,
		ProcessMemoryLimitBytes: manifest.ProcessMemoryLimitBytes, ProcessTransportBytes: manifest.ProcessTransportBytes,
		ProcessTransportCount: manifest.ProcessTransportCount, ClientID: identities["client"], ProviderID: identities["provider"]}
	saveMeta := func() error { return writeJson(filepath.Join(*out, "meta.json"), meta) }
	if err := saveMeta(); err != nil {
		return err
	}
	captures := []*exec.Cmd{}
	captureFiles := []*os.File{}
	stopped := false
	stopCaptures := func() {
		if stopped {
			return
		}
		stopped = true
		for _, cmd := range captures {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		for _, file := range captureFiles {
			_ = file.Close()
		}
	}
	defer stopCaptures()
	for _, side := range []struct{ serial, name string }{{*client, "client"}, {*provider, "provider"}} {
		if _, err := adbShell(side.serial, "logcat -c"); err != nil {
			return fmt.Errorf("%s clear logcat: %w", side.name, err)
		}
		file, err := os.OpenFile(filepath.Join(*out, side.name+".logcat"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		captureFiles = append(captureFiles, file)
		cmd := exec.Command("adb", "-s", side.serial, "logcat", "-v", "epoch")
		cmd.Stdout, cmd.Stderr = file, file
		if err := cmd.Start(); err != nil {
			return err
		}
		captures = append(captures, cmd)
	}
	pss := []pssSample{}
	samplePss := func() error {
		if err := requireDeviceCohort(); err != nil {
			meta.CohortVerified = false
			_ = saveMeta()
			return err
		}
		now := time.Now().UnixMilli()
		pss = append(pss, pssSample{Millis: now, Side: "client", PssKiB: readPss(*client)})
		pss = append(pss, pssSample{Millis: now, Side: "provider", PssKiB: readPss(*provider)})
		return writeJson(filepath.Join(*out, "pss.json"), pss)
	}
	waitPhase := func(seconds int) error {
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(min(15*time.Second, time.Until(deadline)))
			if err := samplePss(); err != nil {
				return err
			}
		}
		return nil
	}
	fmt.Printf("memsteady %s (%s): client=%s provider=%s tun=%s\n", *tag, *build, clientRole, providerRole, tunName)
	meta.BaselineStart = time.Now().UnixMilli()
	if err := saveMeta(); err != nil {
		return err
	}
	if err := waitPhase(20); err != nil {
		return err
	}
	startTun, rx0, _ := tunCounters(*client)
	if startTun != tunName || !tunnelRoutesShell(*client) {
		return errors.New("client tunnel changed before the burst")
	}
	meta.StartMillis = time.Now().UnixMilli()
	if err := saveMeta(); err != nil {
		return err
	}
	loadFile, err := os.OpenFile(filepath.Join(*out, "load.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer loadFile.Close()
	loadCtx, cancelLoad := context.WithTimeout(context.Background(), time.Duration(*burstSeconds+30)*time.Second)
	defer cancelLoad()
	load := exec.CommandContext(loadCtx, "adb", "-s", *client, "shell",
		fmt.Sprintf("%s -url %s -streams %d -seconds %d", loadBinary, shellQuote(*url), *streams, *burstSeconds))
	load.Stdout, load.Stderr = loadFile, loadFile
	if err := load.Start(); err != nil {
		return err
	}
	loadDone := false
	defer func() {
		if !loadDone {
			cancelLoad()
			_ = load.Wait()
			_, _ = adbShell(*client, "pkill -f '^"+loadBinary+" '")
		}
	}()
	if err := waitPhase(*burstSeconds); err != nil {
		return err
	}
	loadErr := load.Wait()
	loadDone = true
	if err := loadFile.Close(); err != nil {
		return err
	}
	endTun, rx1, _ := tunCounters(*client)
	meta.BurstEnd = time.Now().UnixMilli()
	meta.TunRxBytes = rx1 - rx0
	var loadRecordErr error
	meta.LoadBytes, meta.LoadErrors, loadRecordErr = loadLogSummary(filepath.Join(*out, "load.log"))
	meta.LoadComplete = loadErr == nil && loadRecordErr == nil
	if err := saveMeta(); err != nil {
		return err
	}
	if err := validateMemsteadyLoad(loadErr, startTun, endTun, meta.LoadBytes, meta.TunRxBytes); err != nil {
		return err
	}
	if loadRecordErr != nil || meta.LoadErrors != 0 {
		return fmt.Errorf("load incomplete: request errors=%d, completion error=%v", meta.LoadErrors, loadRecordErr)
	}
	fmt.Printf("  burst: %.1f Mb/s over %.1fs\n", float64(meta.TunRxBytes)*8/float64(meta.BurstEnd-meta.StartMillis)/1000, float64(meta.BurstEnd-meta.StartMillis)/1000)
	if err := waitPhase(10); err != nil {
		return err
	}
	meta.QuietStart = time.Now().UnixMilli()
	if err := saveMeta(); err != nil {
		return err
	}
	if err := waitPhase(*quietSeconds); err != nil {
		return err
	}
	meta.EndMillis = time.Now().UnixMilli()
	for _, side := range []struct{ serial, name string }{{*client, "client"}, {*provider, "provider"}} {
		pid, err := adbShell(side.serial, "pidof "+appPackage)
		if err != nil || pid != processes[side.name] {
			return fmt.Errorf("%s app terminated or restarted during the block", side.name)
		}
	}
	meta.ProcessStable = true
	if err := requireDeviceCohort(); err != nil {
		meta.CohortVerified = false
		_ = saveMeta()
		return err
	}
	if err := saveMeta(); err != nil {
		return err
	}
	if *heapProfiles {
		// All acceptance timestamps end before this forced collection.
		for _, side := range []struct{ serial, name string }{{*client, "client"}, {*provider, "provider"}} {
			path, err := captureHeapProfile(side.serial, *tag+"-"+side.name, *out)
			if err != nil {
				return fmt.Errorf("%s heap profile: %w", side.name, err)
			}
			fmt.Printf("  %s heap profile: %s\n", side.name, path)
		}
	}
	time.Sleep(3 * time.Second)
	stopCaptures()
	return memsteadyReport([]string{*out})
}

func memsteadySamples(path string, clockOffset int64) ([]memsteadySample, map[int64]int, []diagSample, error) {
	all, err := parseDiag(path)
	if err != nil {
		return nil, nil, nil, err
	}
	// parseDiag joins parts by millis; the "memory" part fields land on the
	// payload as part=="memory" entries are merged like state fields
	samples := []memsteadySample{}
	windows := map[int64]int{}
	for i := range all {
		all[i].Millis -= clockOffset
		s := all[i]
		if _, ok := s.Payload["go_total_bytes"]; ok {
			samples = append(samples, memsteadySample{Millis: s.Millis, Payload: s.Payload, Parts: s.Parts})
		}
		if _, present := s.Payload["window_client_count"]; present {
			windows[s.Millis] = int(num(s.Payload, "window_client_count"))
		}
	}
	return samples, windows, all, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func phaseStats(samples []memsteadySample, from, to int64, pss []pssSample, side string) memsteadyPhase {
	values := []float64{}
	last := 0.0
	for _, s := range samples {
		if s.Millis < from || s.Millis > to {
			continue
		}
		mib := num(s.Payload, "go_total_bytes") / 1048576
		values = append(values, mib)
		last = mib
	}
	sort.Float64s(values)
	phase := memsteadyPhase{Samples: len(values), LastMiB: last}
	if len(values) > 0 {
		phase.P50MiB = percentile(values, 0.5)
		phase.P95MiB = percentile(values, 0.95)
		phase.MaxMiB = values[len(values)-1]
	}
	pssValues := []float64{}
	for _, p := range pss {
		if p.Side == side && p.Millis >= from && p.Millis <= to && p.PssKiB > 0 {
			pssValues = append(pssValues, float64(p.PssKiB)/1024)
		}
	}
	sort.Float64s(pssValues)
	if len(pssValues) > 0 {
		phase.PssP50 = percentile(pssValues, 0.5)
		phase.PssMax = pssValues[len(pssValues)-1]
	}
	return phase
}

// memsteadyReport derives memsteady.json from a run directory and appends a
// row to the MEMSTEADY.md table beside it.
func memsteadyReport(args []string) error {
	if len(args) < 1 {
		return errors.New("memsteady-report needs a run directory")
	}
	dir := args[0]
	var meta memsteadyMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	var pss []pssSample
	if b, err := os.ReadFile(filepath.Join(dir, "pss.json")); err == nil {
		_ = json.Unmarshal(b, &pss)
	}
	summary := memsteadySummary{Tag: meta.Tag, Build: meta.Build, ClientRole: meta.ClientRole, ProviderRole: meta.ProviderRole,
		BurstSeconds: meta.BurstSeconds, QuietSeconds: meta.QuietSeconds, LoadErrors: meta.LoadErrors, Breaches: []memsteadyBreach{}, Failures: []string{}, Pass: true}
	if meta.QuietSeconds < 300 || meta.EndMillis-meta.QuietStart < 300_000 {
		summary.Failures = append(summary.Failures, "quiet connected window is shorter than five minutes")
	}
	if meta.BaselineStart <= 0 || meta.StartMillis-meta.BaselineStart < 20_000 {
		summary.Failures = append(summary.Failures, "missing twenty-second baseline interval")
	}
	if meta.MemoryProfile != iosMemoryAuditProfile || meta.DeviceMemoryTargetBytes != iosDeviceTargetBytes ||
		meta.ProcessMemoryLimitBytes != iosProcessSoftLimitBytes || meta.ProcessTransportBytes != iosCarrierRootBytes ||
		meta.ProcessTransportCount != iosCarrierRootMaxCount {
		summary.Failures = append(summary.Failures, "not the explicit iOS memory audit 20/32 MiB profile with 8-MiB shared carrier root")
	}
	if meta.ClientID == "" || meta.ProviderID == "" || meta.ClientID == meta.ProviderID {
		summary.Failures = append(summary.Failures, "missing distinct exact client/provider identities")
	}
	if meta.StartMillis <= 0 || meta.BurstEnd <= meta.StartMillis || meta.QuietStart < meta.BurstEnd || meta.EndMillis <= meta.QuietStart {
		summary.Failures = append(summary.Failures, "invalid or incomplete measurement timestamps")
	}
	if meta.ClientRole == meta.ProviderRole || (meta.ClientRole != "device-a" && meta.ClientRole != "device-b") || (meta.ProviderRole != "device-a" && meta.ProviderRole != "device-b") {
		summary.Failures = append(summary.Failures, "distinct allowlisted client/provider roles are required")
	}
	if !meta.CohortVerified || !meta.ProcessStable || len(meta.ArtifactSHA256) != 64 || len(meta.LoadSHA256) != 64 || meta.BuildID == "" {
		summary.Failures = append(summary.Failures, "missing allowlisted-cohort, installed-artifact, or process-continuity evidence")
	}
	if !meta.LoadComplete || meta.LoadErrors != 0 || meta.LoadBytes <= 0 || meta.TunRxBytes <= 0 || float64(meta.TunRxBytes) < 0.9*float64(meta.LoadBytes) {
		summary.Failures = append(summary.Failures, "no successful tunneled burst payload")
	}
	if meta.BurstEnd > meta.StartMillis {
		summary.BurstMbps = float64(meta.TunRxBytes) * 8 / (float64(meta.BurstEnd-meta.StartMillis) / 1000) / 1e6
	}
	for _, side := range []string{"client", "provider"} {
		samples, windows, all, parseErr := memsteadySamples(filepath.Join(dir, side+".logcat"), meta.ClockOffsets[side])
		s := memsteadySide{Failures: []string{}, Pass: true}
		carrierBaselineSeen, carrierEndSeen := false, false
		transferBaselineSeen, transferEndSeen := false, false
		var transferSamples []memsteadyTransferBudgetAt
		lastCarrierHandoffCount := int64(0)
		lastDeviceCarrierHandoffCount := int64(0)
		if parseErr != nil {
			s.Failures = append(s.Failures, "cannot read diagnostic log: "+parseErr.Error())
		}
		for _, interval := range []struct {
			name     string
			from, to int64
		}{
			{"baseline", meta.BaselineStart, meta.StartMillis - 1}, {"burst", meta.StartMillis, meta.BurstEnd}, {"quiet", meta.QuietStart, meta.EndMillis}, {"whole block", meta.BaselineStart, meta.EndMillis},
		} {
			if err := sampleCoverage(samples, interval.from, interval.to); err != nil {
				s.Failures = append(s.Failures, interval.name+": "+err.Error())
			}
		}
		if side == "client" {
			s.Role = meta.ClientRole
		} else {
			s.Role = meta.ProviderRole
		}
		s.Burst = phaseStats(samples, meta.StartMillis, meta.BurstEnd, pss, side)
		s.Quiet = phaseStats(samples, meta.QuietStart, meta.EndMillis, pss, side)
		for _, sample := range samples {
			if sample.Millis > meta.EndMillis {
				continue // optional post-window heap profiles are diagnostic only
			}
			transfer, transferErr := validateDeviceTransferBudget(sample.Payload)
			if !sample.Parts["memory_device_transfer"] {
				s.Failures = append(s.Failures, "missing same-timestamp memory_device_transfer part")
			} else if transferErr != nil {
				s.Failures = append(s.Failures, "transfer budget hierarchy: "+transferErr.Error())
			} else {
				transferSamples = append(transferSamples, memsteadyTransferBudgetAt{Millis: sample.Millis, Budget: transfer})
				baseline := meta.BaselineStart <= sample.Millis && sample.Millis < meta.StartMillis
				transferBaselineSeen = transferBaselineSeen || baseline
				transferEndSeen = true
				for _, budget := range []struct {
					sample  transferByteBudgetSample
					summary *memsteadyByteBudget
				}{
					{transfer.Root, &s.TransferRootBudget}, {transfer.Client, &s.ClientTransferBudget},
					{transfer.Provider, &s.ProviderTransferBudget}, {transfer.Nat, &s.NatBudget},
					{transfer.Pack, &s.PackQueueBudget}, {transfer.Pins, &s.PeerPinBudget},
				} {
					budget.summary.TotalBytes = budget.sample.TotalBytes
					budget.summary.MaxUsedBytes = max(budget.summary.MaxUsedBytes, budget.sample.UsedBytes)
					budget.summary.EndBytes = budget.sample.UsedBytes
					if baseline {
						budget.summary.BaselineBytes = budget.sample.UsedBytes
					}
				}
			}
			carrier, deviceCarrier, carrierErr := validateCarrierBudgetHierarchy(sample.Payload)
			if carrierErr != nil {
				s.Failures = append(s.Failures, "carrier budget hierarchy: "+carrierErr.Error())
			} else {
				s.TransportBudgetTotalBytes = carrier.TotalBytes
				s.TransportBudgetMaxCount = carrier.MaxCount
				s.TransportBudgetMaxUsedBytes = max(s.TransportBudgetMaxUsedBytes, carrier.UsedBytes)
				s.TransportBudgetPeakCount = max(s.TransportBudgetPeakCount, carrier.UsedCount)
				s.TransportBudgetHandoffBytes = max(s.TransportBudgetHandoffBytes, carrier.HandoffBytes)
				s.DeviceTransportBudgetTotalBytes = deviceCarrier.TotalBytes
				s.DeviceTransportBudgetMaxCount = deviceCarrier.MaxCount
				s.DeviceTransportBudgetMaxUsedBytes = max(s.DeviceTransportBudgetMaxUsedBytes, deviceCarrier.UsedBytes)
				s.DeviceTransportBudgetPeakCount = max(s.DeviceTransportBudgetPeakCount, deviceCarrier.UsedCount)
				s.DeviceTransportBudgetHandoffBytes = max(s.DeviceTransportBudgetHandoffBytes, deviceCarrier.HandoffBytes)
				if meta.BaselineStart <= sample.Millis && sample.Millis < meta.StartMillis {
					carrierBaselineSeen = true
					s.TransportBudgetBaselineBytes = carrier.UsedBytes
					s.TransportBudgetBaselineCount = carrier.UsedCount
					s.DeviceTransportBudgetBaselineBytes = deviceCarrier.UsedBytes
					s.DeviceTransportBudgetBaselineCount = deviceCarrier.UsedCount
				}
				if sample.Millis <= meta.EndMillis {
					carrierEndSeen = true
					s.TransportBudgetEndBytes = carrier.UsedBytes
					s.TransportBudgetEndCount = carrier.UsedCount
					lastCarrierHandoffCount = carrier.HandoffCount
					s.DeviceTransportBudgetEndBytes = deviceCarrier.UsedBytes
					s.DeviceTransportBudgetEndCount = deviceCarrier.UsedCount
					lastDeviceCarrierHandoffCount = deviceCarrier.HandoffCount
				}
			}
			mib := num(sample.Payload, "go_total_bytes") / 1048576
			if mib <= 0 {
				s.Failures = append(s.Failures, "missing or nonpositive Go runtime bytes")
			}
			if num(sample.Payload, "go_limit_bytes") != iosProcessSoftLimitBytes || num(sample.Payload, "device_memory_target_bytes") != iosDeviceTargetBytes {
				s.Failures = append(s.Failures, "sample missing expected iOS 20-MiB target / 32-MiB Go soft limit")
			}
			wantID := meta.ClientID
			if side == "provider" {
				wantID = meta.ProviderID
			}
			if sample.Payload["client_id"] != wantID {
				s.Failures = append(s.Failures, "live device identity missing or changed")
			}
			if mib*1048576 > memsteadyTargetBytes {
				phase := "burst"
				if sample.Millis >= meta.QuietStart {
					phase = "quiet"
				} else if sample.Millis < meta.StartMillis {
					phase = "baseline"
				} else if sample.Millis > meta.BurstEnd {
					phase = "drain"
				}
				memory := map[string]any{}
				for k, v := range sample.Payload {
					if strings.HasPrefix(k, "go_") || strings.HasPrefix(k, "pool_") || strings.HasPrefix(k, "packet_pool") || strings.HasPrefix(k, "transport_budget_") || strings.HasPrefix(k, "device_transport_budget_") || isTransferBudgetField(k) || k == "goroutines" || k == "physical_bytes" || k == "gc_cycles" || k == "forced_gc_count" {
						memory[k] = v
					}
				}
				summary.Breaches = append(summary.Breaches, memsteadyBreach{Side: side, Phase: phase, Millis: sample.Millis, MiB: mib, Memory: memory, Windows: windows[sample.Millis]})
				s.Failures = append(s.Failures, phase+" runtime exceeds hard 24 MiB cap (see breach records)")
			}
			if sample.Millis >= meta.StartMillis {
				connected, present := sample.Payload["connect_enabled"].(bool)
				if !present || (side == "client" && !connected) {
					s.Failures = append(s.Failures, "missing connection state or client disconnected during the block")
				}
				if side == "client" && (sample.Payload["location_network_peer"] != true || sample.Payload["location_is_device"] != true || sample.Payload["location_client_id"] != meta.ProviderID) {
					s.Failures = append(s.Failures, "client is not pinned to the device network peer throughout the block")
				}
				if side == "provider" && num(sample.Payload, "provide_mode") <= 0 {
					s.Failures = append(s.Failures, "provider inactive or provider state missing during the block")
				}
			}
		}
		// temporary clients: window client count before the burst, its burst
		// maximum, and at the end of the quiet window
		base, burstMax, end := -1, 0, -1
		p2pBase, p2pEnd := -1.0, -1.0
		for _, a := range all {
			if a.Millis > meta.EndMillis {
				continue
			}
			if _, present := a.Payload["go_total_bytes"]; !present {
				s.Failures = append(s.Failures, "incomplete diagnostic sample: missing runtime memory part")
			}
			w, present := windows[a.Millis]
			if present && a.Millis < meta.StartMillis {
				base = w
			} else if present && a.Millis <= meta.BurstEnd {
				burstMax = max(burstMax, w)
			}
			if present {
				end = w
			}
			if _, present := a.Payload["pool_outstanding"]; present {
				s.PoolOutstandingEnd = int64(num(a.Payload, "pool_outstanding"))
			}
			p2p, _ := a.Payload["p2p"].(map[string]any)
			if p2p != nil && a.Millis <= meta.BurstEnd {
				count := num(p2p, "FastReceiveMessageCount") + num(p2p, "LegacyReceiveMessageCount")
				if side == "provider" {
					count = num(p2p, "FastSendMessageCount") + num(p2p, "LegacySendMessageCount")
				}
				if a.Millis < meta.StartMillis {
					p2pBase = count
				} else {
					p2pEnd = count
				}
			}
		}
		if p2pBase < 0 || p2pEnd <= p2pBase {
			s.Failures = append(s.Failures, "no P2P traffic counter progress during the burst")
		} else {
			summary.P2pActive = true
		}
		s.WindowClientsBase, s.WindowClientsBurst, s.WindowClientsEnd = base, burstMax, end
		if s.Quiet.Samples == 0 {
			s.Failures = append(s.Failures, "no quiet samples")
		}
		if s.Quiet.P50MiB*1048576 > memsteadyTargetBytes || s.Quiet.P95MiB*1048576 > memsteadyTargetBytes {
			s.Failures = append(s.Failures, fmt.Sprintf("quiet p50/p95 %.2f/%.2f MiB above 24 MiB", s.Quiet.P50MiB, s.Quiet.P95MiB))
		}
		// The product ceiling is hard (it is an iOS extension limit), so the
		// worst single sample decides, not only the percentiles.
		if s.Quiet.MaxMiB*1048576 > memsteadyTargetBytes {
			s.Failures = append(s.Failures, fmt.Sprintf("worst quiet sample %.2f MiB above 24 MiB", s.Quiet.MaxMiB))
		}
		if s.Burst.MaxMiB*1048576 > memsteadyTargetBytes {
			s.Failures = append(s.Failures, fmt.Sprintf("active max %.2f MiB above 24 MiB", s.Burst.MaxMiB))
		}
		if base < 0 || end < 0 {
			s.Failures = append(s.Failures, "missing temporary-client baseline or final count")
		} else if end > base {
			s.Failures = append(s.Failures, fmt.Sprintf("window clients %d at end vs %d before the burst", end, base))
		}
		if !carrierBaselineSeen || !carrierEndSeen {
			s.Failures = append(s.Failures, "missing root/device carrier-budget baseline or final evidence")
		} else {
			if lastCarrierHandoffCount != 0 || lastDeviceCarrierHandoffCount != 0 {
				s.Failures = append(s.Failures, "root/device carrier handoff remained active at quiet-window end")
			}
			if s.TransportBudgetEndBytes > s.TransportBudgetBaselineBytes || s.TransportBudgetEndCount > s.TransportBudgetBaselineCount {
				s.Failures = append(s.Failures, "process carrier bytes/slots did not return to the pre-burst baseline")
			}
			if s.DeviceTransportBudgetEndBytes > s.DeviceTransportBudgetBaselineBytes || s.DeviceTransportBudgetEndCount > s.DeviceTransportBudgetBaselineCount {
				s.Failures = append(s.Failures, "device carrier bytes/slots did not return to the pre-burst baseline")
			}
		}
		if !transferBaselineSeen || !transferEndSeen {
			s.Failures = append(s.Failures, "missing transfer-budget baseline or final evidence")
		} else {
			var recoveryErr error
			s.TransferRecovery, recoveryErr = verifyMemsteadyTransferRecovery(transferSamples, meta)
			if recoveryErr != nil {
				s.Failures = append(s.Failures, "transfer budget recovery: "+recoveryErr.Error())
			}
			for _, budget := range []struct {
				name    string
				summary memsteadyByteBudget
			}{
				{"transfer root", s.TransferRootBudget}, {"client transfer", s.ClientTransferBudget},
				{"provider transfer", s.ProviderTransferBudget}, {"NAT", s.NatBudget},
				{"Pack queue", s.PackQueueBudget},
			} {
				if budget.summary.EndBytes > budget.summary.BaselineBytes {
					if (budget.name == "transfer root" || budget.name == "NAT") && s.TransferRecovery.LateNatAdmissionAccepted {
						continue
					}
					s.Failures = append(s.Failures, budget.name+" bytes did not return to the pre-burst baseline")
				}
			}
		}
		s.Failures = uniqueFailures(s.Failures)
		s.Pass = len(s.Failures) == 0
		if side == "client" {
			summary.Client = s
		} else {
			summary.Provider = s
		}
		summary.Pass = summary.Pass && s.Pass
	}
	if len(summary.Breaches) > 0 || len(summary.Failures) > 0 {
		summary.Pass = false
	}
	if err := writeJson(filepath.Join(dir, "memsteady.json"), summary); err != nil {
		return err
	}
	headroom := math.Min(24-summary.Client.Quiet.MaxMiB, 24-summary.Provider.Quiet.MaxMiB)
	row := fmt.Sprintf("| %s | %s | %s→%s | %.1f | %.2f / %.2f / %.2f | %.2f / %.2f / %.2f | %.2f / %.2f | %+.2f | %.1f / %.1f | %.2f / %.2f | %d→%d / %d→%d | %.2f / %.2f | %d→%d / %d→%d | %.2f / %.2f | %.2f / %.2f | %d | %d→%d / %d→%d | %s |",
		summary.Tag, summary.Build, summary.ClientRole, summary.ProviderRole, summary.BurstMbps,
		summary.Client.Quiet.P50MiB, summary.Client.Quiet.P95MiB, summary.Client.Quiet.MaxMiB,
		summary.Provider.Quiet.P50MiB, summary.Provider.Quiet.P95MiB, summary.Provider.Quiet.MaxMiB,
		summary.Client.Burst.MaxMiB, summary.Provider.Burst.MaxMiB,
		headroom,
		summary.Client.Quiet.PssP50, summary.Provider.Quiet.PssP50,
		float64(summary.Client.TransportBudgetMaxUsedBytes)/1048576, float64(summary.Provider.TransportBudgetMaxUsedBytes)/1048576,
		summary.Client.TransportBudgetBaselineCount, summary.Client.TransportBudgetEndCount, summary.Provider.TransportBudgetBaselineCount, summary.Provider.TransportBudgetEndCount,
		float64(summary.Client.DeviceTransportBudgetMaxUsedBytes)/1048576, float64(summary.Provider.DeviceTransportBudgetMaxUsedBytes)/1048576,
		summary.Client.DeviceTransportBudgetBaselineCount, summary.Client.DeviceTransportBudgetEndCount, summary.Provider.DeviceTransportBudgetBaselineCount, summary.Provider.DeviceTransportBudgetEndCount,
		float64(summary.Client.TransferRootBudget.MaxUsedBytes)/1048576, float64(summary.Provider.TransferRootBudget.MaxUsedBytes)/1048576,
		float64(summary.Client.NatBudget.MaxUsedBytes)/1048576, float64(summary.Provider.NatBudget.MaxUsedBytes)/1048576,
		len(summary.Breaches),
		summary.Client.WindowClientsBase, summary.Client.WindowClientsEnd, summary.Provider.WindowClientsBase, summary.Provider.WindowClientsEnd,
		map[bool]string{true: "PASS", false: "FAIL: " + strings.Join(append(append(summary.Failures, summary.Client.Failures...), summary.Provider.Failures...), "; ")}[summary.Pass])
	table := filepath.Join(filepath.Dir(filepath.Clean(dir)), "MEMSTEADY.md")
	if _, err := os.Stat(table); err != nil {
		header := "# MEMSTEADY peer-burst/recovery blocks (goRuntimeBytes = go_total_bytes; MiB)\n\nThese blocks do not replace the carrier-specific forced-mode or browser performance matrices.\n\n| run | build | roles | burst Mb/s | client quiet p50/p95/max | provider quiet p50/p95/max | active max c/p | worst-case headroom | quiet PSS p50 c/p | process carrier max MiB c/p | process slots c/p (before→end) | device carrier max MiB c/p | device slots c/p (before→end) | transfer root max MiB c/p | NAT max MiB c/p | >24 MiB | window clients c/p (before→end) | verdict |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n"
		if err := os.WriteFile(table, []byte(header), 0o644); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(table, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(row + "\n")
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	fmt.Println(row)
	for _, breach := range summary.Breaches {
		fmt.Printf("  breach %s %s %.2f MiB: %v windows=%d\n", breach.Side, breach.Phase, breach.MiB, breach.Memory, breach.Windows)
	}
	if !summary.Pass {
		return errors.New("MEMSTEADY block failed; see memsteady.json for all failures")
	}
	return nil
}

// runMemsteadySeries runs the MEMSTEADY block on a list of builds, each in
// both role assignments (device-b client through device-a providing, then
// the swap), installing each build in place on both devices first. Builds
// are "label=apk-path" pairs, in order.
func runMemsteadySeries(args []string) error {
	fs := flag.NewFlagSet("memsteady-series", flag.ContinueOnError)
	deviceA := fs.String("device-a", "3B161FDJG001KT", "device-a serial")
	deviceB := fs.String("device-b", "R5CX21FY6ND", "device-b serial")
	nameA := fs.String("name-a", "Pixel", "device-a's device name substring as a peer")
	nameB := fs.String("name-b", "Samsung", "device-b's device name substring as a peer")
	out := fs.String("out", "", "series directory")
	burstSeconds := fs.Int("burst-seconds", 60, "burst length")
	quietSeconds := fs.Int("quiet-seconds", 300, "quiet connected window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, b, err := validateMemsteadyRoles(*deviceA, *deviceB)
	if err != nil || a != "device-a" || b != "device-b" {
		return errors.New("--device-a and --device-b must match their allowlisted roles")
	}
	if *out == "" || fs.NArg() == 0 || *burstSeconds <= 0 || *quietSeconds < 300 {
		return errors.New("--out, label=apk, positive burst, and at least 300 quiet seconds are required")
	}
	type buildSpec struct{ label, apk, manifest string }
	specs := []buildSpec{}
	for _, spec := range fs.Args() {
		label, apk, ok := strings.Cut(spec, "=")
		if !ok || label == "" || label == "." || label == ".." || filepath.Base(label) != label {
			return fmt.Errorf("bad build spec %q", spec)
		}
		manifestPath := filepath.Join(filepath.Dir(apk), "build-manifest.json")
		if _, err := verifyAcceptanceArtifact(apk, manifestPath); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		specs = append(specs, buildSpec{label, apk, manifestPath})
	}
	if err := requireDeviceCohort(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(*out, 0o700); err != nil {
		return err
	}
	loadSHA, err := buildAndInstallLoad()
	if err != nil {
		return err
	}
	if err := writeJson(filepath.Join(*out, "load-helper.json"), map[string]string{"sha256": loadSHA}); err != nil {
		return err
	}
	settle := func(serial string) error {
		if _, err := adbShell(serial, "monkey -p "+appPackage+" -c android.intent.category.LAUNCHER 1 >/dev/null 2>&1"); err != nil {
			return err
		}
		time.Sleep(8 * time.Second)
		return nil
	}
	action := func(command string, args []string) error {
		switch command {
		case "disconnect":
			return disconnect(args)
		case "provide":
			return provide(args)
		default:
			return fmt.Errorf("unknown role action %s", command)
		}
	}
	failures := []error{}
	for _, spec := range specs {
		fmt.Printf("build %s\n", spec.label)
		if err := requireDeviceCohort(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := install([]string{"--update", "--apk", spec.apk, "--build-manifest", spec.manifest, *deviceA, *deviceB}); err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, serial := range []string{*deviceA, *deviceB} {
			if err := settle(serial); err != nil {
				return errors.Join(append(failures, err)...)
			}
		}
		for _, assignment := range []struct{ tag, client, provider, peerName string }{
			{"A", *deviceB, *deviceA, *nameA}, {"B", *deviceA, *deviceB, *nameB},
		} {
			runTag := spec.label + "-" + assignment.tag
			err := withMemsteadyCleanup([]string{assignment.client, assignment.provider}, action, func() error {
				if err := requireDeviceCohort(); err != nil {
					return err
				}
				for _, step := range []struct {
					command string
					args    []string
				}{
					{"disconnect", []string{"--serial", assignment.client}},
					{"provide", []string{"--serial", assignment.client, "--control", "never"}},
					{"disconnect", []string{"--serial", assignment.provider}},
					{"provide", []string{"--serial", assignment.provider, "--control", "network", "--network", "all"}},
				} {
					if err := action(step.command, step.args); err != nil {
						return err
					}
				}
				providerStatus, err := readMemsteadyDeviceStatus(assignment.provider)
				if err != nil || providerStatus.ClientID == "" {
					return errors.New("cannot read provider's exact client identity")
				}
				if err := waitMemsteadyPeer(assignment.client, providerStatus.ClientID); err != nil {
					return err
				}
				if err := connectPeer([]string{"--serial", assignment.client, "--peer-id", providerStatus.ClientID}); err != nil {
					return err
				}
				time.Sleep(25 * time.Second)
				return runMemsteady([]string{
					"--client", assignment.client, "--provider", assignment.provider,
					"--out", filepath.Join(*out, runTag), "--tag", runTag, "--build", spec.label,
					"--build-manifest", spec.manifest,
					"--load-sha256", loadSHA,
					"--burst-seconds", strconv.Itoa(*burstSeconds), "--quiet-seconds", strconv.Itoa(*quietSeconds),
				})
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", runTag, err))
			}
		}
	}
	return errors.Join(failures...)
}

func withMemsteadyCleanup(serials []string, action func(string, []string) error, run func() error) (err error) {
	defer func() {
		for _, serial := range serials {
			// Cleanup remains available when the cohort gate fails mid-run.
			// Attempt every action even after another cleanup action fails.
			for _, step := range []struct {
				command string
				args    []string
			}{
				{"disconnect", []string{"--serial", serial}},
				{"provide", []string{"--serial", serial, "--control", "never"}},
			} {
				if cleanupErr := action(step.command, step.args); cleanupErr != nil {
					err = errors.Join(err, fmt.Errorf("cleanup %s: %w", step.command, cleanupErr))
				}
			}
		}
	}()
	return run()
}

func waitMemsteadyPeer(serial, clientID string) error {
	for i := 0; i < 12; i++ {
		line, err := broadcast(serial, "FG_STATUS", nil, "status {", 20*time.Second)
		if err == nil && statusHasProvidingPeerID(line, clientID) {
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return errors.New("requested providing peer did not become available")
}

func statusHasProvidingPeerID(line, clientID string) bool {
	_, body, ok := strings.Cut(line, "status ")
	if !ok || clientID == "" {
		return false
	}
	var status struct {
		Peers []struct {
			ClientID string `json:"client_id"`
			Enabled  bool   `json:"provide_enabled"`
		} `json:"peers"`
	}
	if json.Unmarshal([]byte(body), &status) != nil {
		return false
	}
	matches := 0
	for _, peer := range status.Peers {
		if peer.Enabled && peer.ClientID == clientID {
			matches++
		}
	}
	return matches == 1
}

func statusHasProvidingPeer(line, name string) bool {
	_, body, ok := strings.Cut(line, "status ")
	if !ok || name == "" {
		return false
	}
	var status struct {
		Peers []struct {
			Name    string `json:"device_name"`
			Enabled bool   `json:"provide_enabled"`
		} `json:"peers"`
	}
	if json.Unmarshal([]byte(body), &status) != nil {
		return false
	}
	matches := 0
	for _, peer := range status.Peers {
		if peer.Enabled && strings.Contains(strings.ToLower(peer.Name), strings.ToLower(name)) {
			matches++
		}
	}
	return matches == 1
}

// memsteadyAttribute prints, per block and per side, the quiet window's
// percentiles and what the worst sample held. The 24 MiB ceiling is a hard
// product limit and the blocks sit within a fraction of a MiB of it, so the
// question whoever picks this up will ask is which structure holds the bytes.
// The answer this readout gives is that the live heap is only a third of the
// envelope and tracks retained packet-pool ownership, while the rest is Go
// runtime structure no budget constant guards.
//
// Only the fields the periodic sample carries are shown. The split of the
// envelope into heap slack, goroutine stacks and GC metadata comes from the
// runtime's memory classes, which are logged with the heap profile rather
// than every interval, so read those from the "classes=" field of a
// heap-profile line in the same block.
func memsteadyAttribute(args []string) error {
	fs := flag.NewFlagSet("memsteady-attribute", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("memsteady-attribute needs one or more block directories")
	}
	fmt.Printf("%-16s %-9s %7s %7s %7s %9s %7s %9s %8s %9s\n",
		"block", "side", "p50", "p95", "worst", "headroom", "live", "poolMiB", "poolN", "goroutines")
	for _, dir := range fs.Args() {
		var meta memsteadyMeta
		b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &meta); err != nil {
			continue
		}
		for _, side := range []string{"client", "provider"} {
			samples, _, _, err := memsteadySamples(filepath.Join(dir, side+".logcat"), meta.ClockOffsets[side])
			if err != nil {
				return err
			}
			quiet := []memsteadySample{}
			for _, sample := range samples {
				if meta.QuietStart <= sample.Millis && sample.Millis <= meta.EndMillis {
					quiet = append(quiet, sample)
				}
			}
			if len(quiet) == 0 {
				continue
			}
			values := []float64{}
			var peak memsteadySample
			worst := 0.0
			for _, sample := range quiet {
				mib := num(sample.Payload, "go_total_bytes") / 1048576
				values = append(values, mib)
				if worst < mib {
					worst, peak = mib, sample
				}
			}
			sort.Float64s(values)
			fmt.Printf("%-16s %-9s %7.2f %7.2f %7.2f %+9.2f %7.2f %9.2f %8.0f %9.0f\n",
				filepath.Base(dir), side,
				percentile(values, 0.5), percentile(values, 0.95), worst,
				float64(memsteadyTargetBytes)/1048576-worst,
				num(peak.Payload, "go_live_bytes")/1048576,
				num(peak.Payload, "packet_pool_outstanding_bytes")/1048576,
				num(peak.Payload, "pool_outstanding"),
				num(peak.Payload, "goroutines"))
		}
	}
	return nil
}
