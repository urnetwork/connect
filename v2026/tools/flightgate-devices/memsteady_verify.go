package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

const (
	iosMemoryAuditProfile    = "ios-memory-audit-v1"
	iosDeviceTargetBytes     = 20 * 1024 * 1024
	iosProcessSoftLimitBytes = 32 * 1024 * 1024
	iosCarrierRootBytes      = 8 * 1024 * 1024
	iosCarrierRootMaxCount   = 16
	iosCarrierDeviceBytes    = 5 * 1024 * 1024
	iosCarrierH1OverlapBytes = 256 * 1024
	iosTransferRootBytes     = 13 * 1024 * 1024
	iosNatBudgetBytes        = 2 * 1024 * 1024
	iosPeerPinBudgetBytes    = 1024 * 1024
)

type transferByteBudgetSample struct {
	TotalBytes    int64
	UsedBytes     int64
	ReservedBytes int64
	ReleasedBytes int64
}

type deviceTransferBudgetSample struct {
	Root     transferByteBudgetSample
	Client   transferByteBudgetSample
	Provider transferByteBudgetSample
	Nat      transferByteBudgetSample
	Pack     transferByteBudgetSample
	Pins     transferByteBudgetSample
}

func isTransferBudgetField(key string) bool {
	for _, prefix := range []string{"transfer_root_", "client_transfer_", "provider_transfer_", "nat_budget_", "pack_queue_", "peer_pin_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// These child budgets are diagnostic subsets of the same root, not extra
// memory. Check each independently; adding them would double-count Pack and
// could also mistake overlapping permission ceilings for reservations.
func validateDeviceTransferBudget(payload map[string]any) (deviceTransferBudgetSample, error) {
	var sample deviceTransferBudgetSample
	for _, budget := range []struct {
		prefix   string
		sample   *transferByteBudgetSample
		ledger   bool
		expected int64
	}{
		{"transfer_root_", &sample.Root, true, iosTransferRootBytes},
		{"client_transfer_", &sample.Client, false, 0},
		{"provider_transfer_", &sample.Provider, false, 0},
		{"nat_budget_", &sample.Nat, true, iosNatBudgetBytes},
		{"pack_queue_", &sample.Pack, false, 0},
		{"peer_pin_", &sample.Pins, true, iosPeerPinBudgetBytes},
	} {
		fields := []struct {
			key    string
			value  *int64
			ledger bool
		}{
			{"total_bytes", &budget.sample.TotalBytes, false},
			{"used_bytes", &budget.sample.UsedBytes, false},
			{"reserved_bytes", &budget.sample.ReservedBytes, true},
			{"released_bytes", &budget.sample.ReleasedBytes, true},
		}
		for _, field := range fields {
			if field.ledger && !budget.ledger {
				continue
			}
			value, err := exactDiagInt(payload, budget.prefix+field.key)
			if err != nil {
				return sample, err
			}
			*field.value = value
		}
		if budget.expected != 0 && budget.sample.TotalBytes != budget.expected {
			return sample, fmt.Errorf("%s is %d bytes, want %d", budget.prefix, budget.sample.TotalBytes, budget.expected)
		}
		if budget.sample.UsedBytes > budget.sample.TotalBytes {
			return sample, fmt.Errorf("%s exceeds its byte ceiling", budget.prefix)
		}
		if budget.ledger && (budget.sample.ReleasedBytes > budget.sample.ReservedBytes ||
			budget.sample.ReservedBytes-budget.sample.ReleasedBytes != budget.sample.UsedBytes) {
			return sample, fmt.Errorf("%s reserve/release accounting is not balanced", budget.prefix)
		}
		if budget.sample.UsedBytes > sample.Root.UsedBytes {
			return sample, fmt.Errorf("%s claims escape the transfer-root accounting", budget.prefix)
		}
	}
	// The measured device remains alive through quiet recovery. Its durable
	// pin owner cannot be released while provider/window generations use it.
	// Post-Close samples are not required (the logger joins during Close).
	if sample.Pins.UsedBytes != iosPeerPinBudgetBytes {
		return sample, errors.New("peer_pin_ live owner must retain exactly 1 MiB")
	}
	if sample.Pins.UsedBytes > sample.Client.UsedBytes {
		return sample, errors.New("peer_pin_ claims escape the client-group accounting")
	}
	count, err := exactDiagInt(payload, "peer_pin_count")
	if err != nil {
		return sample, err
	}
	if count > 256 {
		return sample, errors.New("peer_pin_count exceeds the 256-peer envelope")
	}
	for _, key := range []string{"peer_pin_capacity_refusals", "peer_pin_persistence_failures", "peer_pin_rollback_refusals", "peer_pin_state_failures"} {
		value, err := exactDiagInt(payload, key)
		if err != nil {
			return sample, err
		}
		if value != 0 {
			return sample, fmt.Errorf("%s must remain zero throughout acceptance", key)
		}
	}
	return sample, nil
}

type carrierHandoffPairSample struct {
	ID      int64
	From    string
	To      string
	H1Bytes int64
	Bytes   int64
	Slots   int64
	Owner   string
}

type processCarrierBudgetSample struct {
	TotalBytes     int64
	UsedBytes      int64
	MaxCount       int64
	UsedCount      int64
	PendingH1Count int64
	PendingH1Bytes int64
	ReservedBytes  int64
	ReleasedBytes  int64
	HandoffCount   int64
	HandoffBytes   int64
	HandoffSlots   int64
	HandoffID      int64
	HandoffFrom    string
	HandoffTo      string
	HandoffH1Bytes int64
	Pair           carrierHandoffPairSample
	AdditionalPair carrierHandoffPairSample
}

func exactDiagInt(payload map[string]any, key string) (int64, error) {
	value, present := payload[key]
	if !present {
		return 0, fmt.Errorf("missing %s", key)
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number < 0 || (1<<53)-1 < number {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return int64(number), nil
}

// validateProcessCarrierBudget proves that direct H1/H3, DNS and extender
// claims share the iOS-profile process root. Handoffs may account both the old
// and new carrier briefly, but only by the explicitly reported paired overlap.
func validateProcessCarrierBudget(payload map[string]any) (processCarrierBudgetSample, error) {
	return validateCarrierBudget(payload, "transport_budget_", iosCarrierRootBytes)
}

func validateCarrierBudget(payload map[string]any, prefix string, expectedBytes int64) (processCarrierBudgetSample, error) {
	keys := []string{
		"total_bytes", "used_bytes", "max_count", "used_count", "pending_h1", "pending_h1_bytes",
		"reserved_bytes", "released_bytes", "active_handoff_count", "active_handoff_bytes", "active_handoff_slots",
		"active_handoff_id", "active_handoff_h1_bytes", "pair_id", "pair_h1_bytes", "pair_bytes", "pair_slots",
		"additional_pair_id", "additional_pair_h1_bytes", "additional_pair_bytes", "additional_pair_slots",
	}
	values := make([]int64, len(keys))
	for i, key := range keys {
		value, err := exactDiagInt(payload, prefix+key)
		if err != nil {
			return processCarrierBudgetSample{}, err
		}
		values[i] = value
	}
	sample := processCarrierBudgetSample{
		TotalBytes: values[0], UsedBytes: values[1], MaxCount: values[2], UsedCount: values[3],
		PendingH1Count: values[4], PendingH1Bytes: values[5], ReservedBytes: values[6], ReleasedBytes: values[7],
		HandoffCount: values[8], HandoffBytes: values[9], HandoffSlots: values[10],
		HandoffID: values[11], HandoffH1Bytes: values[12],
		Pair:           carrierHandoffPairSample{ID: values[13], H1Bytes: values[14], Bytes: values[15], Slots: values[16]},
		AdditionalPair: carrierHandoffPairSample{ID: values[17], H1Bytes: values[18], Bytes: values[19], Slots: values[20]},
	}
	for key, target := range map[string]*string{
		"active_handoff_from": &sample.HandoffFrom, "active_handoff_to": &sample.HandoffTo,
		"pair_from": &sample.Pair.From, "pair_to": &sample.Pair.To, "pair_owner": &sample.Pair.Owner,
		"additional_pair_from": &sample.AdditionalPair.From, "additional_pair_to": &sample.AdditionalPair.To,
		"additional_pair_owner": &sample.AdditionalPair.Owner,
	} {
		value, ok := payload[prefix+key].(string)
		if !ok {
			return sample, fmt.Errorf("missing or invalid %s%s", prefix, key)
		}
		*target = value
	}
	if sample.TotalBytes != expectedBytes || sample.MaxCount != iosCarrierRootMaxCount {
		return sample, fmt.Errorf("%s is %d bytes/%d slots, want %d/%d",
			prefix, sample.TotalBytes, sample.MaxCount, expectedBytes, iosCarrierRootMaxCount)
	}
	if sample.ReleasedBytes > sample.ReservedBytes || sample.ReservedBytes-sample.ReleasedBytes != sample.UsedBytes {
		return sample, fmt.Errorf("%s reserve/release accounting is not balanced", prefix)
	}
	active := carrierHandoffPairSample{
		ID: sample.HandoffID, From: sample.HandoffFrom, To: sample.HandoffTo,
		H1Bytes: sample.HandoffH1Bytes, Bytes: sample.HandoffBytes, Slots: sample.HandoffSlots,
	}
	if sample.HandoffCount > 1 || (sample.HandoffCount == 0 && active != (carrierHandoffPairSample{})) {
		return sample, fmt.Errorf("%s inactive or multiple handoff evidence is invalid", prefix)
	}
	if sample.Pair.ID == 0 {
		if sample.Pair != (carrierHandoffPairSample{}) || sample.HandoffCount != 0 {
			return sample, fmt.Errorf("%s has inactive pair residue or an unpaired loan", prefix)
		}
	} else {
		if err := validateCarrierHandoffPair(sample.Pair); err != nil {
			return sample, fmt.Errorf("%s: %w", prefix, err)
		}
		if sample.UsedBytes < 2*sample.Pair.Bytes || sample.UsedCount < 2*sample.Pair.Slots {
			return sample, fmt.Errorf("%s pair is not backed by two live carrier claims", prefix)
		}
		if sample.HandoffCount == 1 {
			active.Owner = sample.Pair.Owner
			if active != sample.Pair {
				return sample, fmt.Errorf("%s active loan does not match its H1 pair", prefix)
			}
		}
	}
	if sample.AdditionalPair.ID == 0 {
		if sample.AdditionalPair != (carrierHandoffPairSample{}) {
			return sample, fmt.Errorf("%s has inactive additional-pair residue", prefix)
		}
	} else {
		if sample.Pair.ID == 0 || sample.AdditionalPair.ID == sample.Pair.ID {
			return sample, fmt.Errorf("%s has an orphaned or duplicate additional pair", prefix)
		}
		if err := validateCarrierHandoffPair(sample.AdditionalPair); err != nil {
			return sample, fmt.Errorf("%s additional pair: %w", prefix, err)
		}
		if sample.UsedBytes < 2*sample.AdditionalPair.Bytes || sample.UsedCount < 2*sample.AdditionalPair.Slots {
			return sample, fmt.Errorf("%s additional pair is not backed by two live carrier claims", prefix)
		}
	}
	if sample.UsedBytes > sample.TotalBytes+sample.HandoffBytes ||
		sample.UsedCount > sample.MaxCount+sample.HandoffSlots {
		return sample, fmt.Errorf("%s exceeds its ceiling plus the explicit H1 overlap", prefix)
	}
	return sample, nil
}

func validateCarrierHandoffPair(pair carrierHandoffPairSample) error {
	validClass := func(class string) bool {
		return class == "h1" || class == "h3_auto" || class == "h3_explicit"
	}
	// This audit has one fixed iOS profile: an H1 endpoint always owns one
	// physical carrier slot and exactly 256 KiB. Upper bounds alone would
	// certify consistently underreported primary/additional/active evidence.
	if pair.ID <= 0 || !validClass(pair.From) || !validClass(pair.To) ||
		(pair.From != "h1" && pair.To != "h1") ||
		pair.H1Bytes != iosCarrierH1OverlapBytes || pair.Slots != 1 ||
		pair.Bytes <= 0 || pair.H1Bytes < pair.Bytes ||
		(pair.Owner != "device" && pair.Owner != "process" && pair.Owner != "other_device") {
		return errors.New("handoff pair requires valid identity, exactly one slot and a 256-KiB H1 claim, with overlap no larger than that claim")
	}
	return nil
}

func validateCarrierBudgetHierarchy(payload map[string]any) (processCarrierBudgetSample, processCarrierBudgetSample, error) {
	root, err := validateProcessCarrierBudget(payload)
	if err != nil {
		return root, processCarrierBudgetSample{}, err
	}
	child, err := validateCarrierBudget(payload, "device_transport_budget_", iosCarrierDeviceBytes)
	if err != nil {
		return root, child, err
	}
	if root.UsedBytes < child.UsedBytes || root.UsedCount < child.UsedCount ||
		root.ReservedBytes < child.ReservedBytes || root.ReleasedBytes < child.ReleasedBytes ||
		root.PendingH1Count < child.PendingH1Count || root.PendingH1Bytes < child.PendingH1Bytes {
		return root, child, errors.New("device carrier claims escape the process-root accounting")
	}
	rootPairs := [2]carrierHandoffPairSample{root.Pair, root.AdditionalPair}
	childPairs := [2]carrierHandoffPairSample{child.Pair, child.AdditionalPair}
	contains := func(pairs [2]carrierHandoffPairSample, want carrierHandoffPairSample) bool {
		return pairs[0] == want || pairs[1] == want
	}
	for _, pair := range rootPairs {
		if pair.ID == 0 {
			continue
		}
		// At most one actual lender exists per level. Additional pair evidence
		// proves shared ownership only; it never adds another byte/slot allowance.
		if pair.ID != root.HandoffID && pair.ID != child.HandoffID {
			return root, child, errors.New("carrier pair has no active root or device loan")
		}
		switch pair.Owner {
		case "device":
			if !contains(childPairs, pair) {
				return root, child, errors.New("device/root handoff pair evidence mismatches or is underreported")
			}
		case "process":
			if pair.ID != root.HandoffID || child.Pair.ID == pair.ID || child.AdditionalPair.ID == pair.ID {
				return root, child, errors.New("ownerless root handoff contradicts the device pair evidence")
			}
		default:
			return root, child, errors.New("root handoff belongs to an unaudited device")
		}
	}
	for _, pair := range childPairs {
		if pair.ID != 0 && (pair.Owner != "device" || !contains(rootPairs, pair)) {
			return root, child, errors.New("device handoff has no matching process pair evidence")
		}
	}
	return root, child, nil
}

type acceptanceManifest struct {
	BuildID                 string `json:"build_id"`
	APKSHA256               string `json:"apk_sha256"`
	AcceptanceEligible      bool   `json:"acceptance_eligible"`
	MemProfileRate          int    `json:"mem_profile_rate"`
	MemoryProfile           string `json:"memory_profile"`
	DeviceMemoryTargetBytes int64  `json:"device_memory_target_bytes"`
	ProcessMemoryLimitBytes int64  `json:"process_memory_limit_bytes"`
	ProcessTransportBytes   int64  `json:"process_transport_budget_bytes"`
	ProcessTransportCount   int64  `json:"process_transport_max_count"`
}

func readAcceptanceManifest(path string) (acceptanceManifest, error) {
	var manifest acceptanceManifest
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return manifest, err
	}
	hash, err := hex.DecodeString(manifest.APKSHA256)
	if err != nil || len(hash) != 32 || manifest.BuildID == "" || !manifest.AcceptanceEligible || manifest.MemProfileRate != 0 {
		return manifest, errors.New("completed acceptance artifact manifest required (profiling artifacts cannot pass)")
	}
	if manifest.MemoryProfile != iosMemoryAuditProfile || manifest.DeviceMemoryTargetBytes != iosDeviceTargetBytes ||
		manifest.ProcessMemoryLimitBytes != iosProcessSoftLimitBytes || manifest.ProcessTransportBytes != iosCarrierRootBytes ||
		manifest.ProcessTransportCount != iosCarrierRootMaxCount {
		return manifest, errors.New("explicit iOS memory audit profile (20 MiB device / 32 MiB process / 8 MiB carrier root) required")
	}
	return manifest, nil
}

// Verify the bytes as well as the audit profile before allowing an acceptance
// artifact to replace an installed app, including a newer version code.
func verifyAcceptanceArtifact(apk, manifestPath string) (acceptanceManifest, error) {
	manifest, err := readAcceptanceManifest(manifestPath)
	if err != nil {
		return manifest, err
	}
	hash, err := fileSHA256(apk)
	if err != nil {
		return manifest, fmt.Errorf("read acceptance APK: %w", err)
	}
	if hash != manifest.APKSHA256 {
		return manifest, errors.New("APK does not match completed build manifest")
	}
	return manifest, nil
}
type memsteadyDeviceStatus struct {
	ClientID                string `json:"client_id"`
	MemoryProfile           string `json:"memory_profile"`
	DeviceMemoryTargetBytes int64  `json:"device_memory_target_bytes"`
	ProcessMemoryLimitBytes int64  `json:"process_memory_limit_bytes"`
	BuildID                 string `json:"acceptance_build_id"`
}

func readMemsteadyDeviceStatus(serial string) (memsteadyDeviceStatus, error) {
	var status memsteadyDeviceStatus
	line, err := broadcast(serial, "FG_STATUS", nil, "status {", 20*time.Second)
	if err != nil {
		return status, err
	}
	_, body, ok := strings.Cut(line, "status ")
	if !ok {
		return status, errors.New("missing status JSON")
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		return status, err
	}
	return status, nil
}

func validateMemsteadyDeviceStatus(status memsteadyDeviceStatus, buildID string) error {
	if status.ClientID == "" || status.MemoryProfile != iosMemoryAuditProfile || status.DeviceMemoryTargetBytes != iosDeviceTargetBytes || status.ProcessMemoryLimitBytes != iosProcessSoftLimitBytes || status.BuildID != buildID {
		return errors.New("live device identity, build, or iOS profile evidence missing/mismatched")
	}
	return nil
}

func verifyInstalledArtifact(serial, wantHash string) error {
	out, err := adbShell(serial, "pm path "+appPackage)
	if err != nil {
		return err
	}
	paths := strings.Fields(out)
	if len(paths) != 1 || !strings.HasPrefix(paths[0], "package:") {
		return errors.New("expected one installed base APK")
	}
	path := strings.TrimPrefix(paths[0], "package:")
	return verifyDeviceFile(serial, path, wantHash)
}

func verifyDeviceFile(serial, path, wantHash string) error {
	out, err := adbShell(serial, "sha256sum "+shellQuote(path))
	fields := strings.Fields(out)
	if err != nil || len(fields) == 0 || fields[0] != wantHash {
		return errors.New("device file SHA-256 does not match the local artifact")
	}
	return nil
}

func validateMemsteadyRoles(client, provider string) (string, string, error) {
	c, err := role(client)
	if err != nil {
		return "", "", fmt.Errorf("client: %w", err)
	}
	p, err := role(provider)
	if err != nil {
		return "", "", fmt.Errorf("provider: %w", err)
	}
	if c == p {
		return "", "", errors.New("client and provider must be distinct allowlisted devices")
	}
	return c, p, nil
}

func validateMemsteadyLoad(loadErr error, startTun, endTun string, loadBytes, tunBytes int64) error {
	if loadErr != nil {
		return fmt.Errorf("load helper failed: %w", loadErr)
	}
	if startTun == "" || endTun != startTun {
		return errors.New("load did not retain the same tunnel")
	}
	if loadBytes <= 0 || tunBytes <= 0 || float64(tunBytes) < 0.9*float64(loadBytes) {
		return errors.New("no completed tunneled load: missing payload or workload bypassed the tunnel")
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func uniqueFailures(failures []string) []string {
	unique := []string{}
	seen := map[string]bool{}
	for _, failure := range failures {
		if !seen[failure] {
			seen[failure] = true
			unique = append(unique, failure)
		}
	}
	return unique
}

// The SDK diagnostic is built at 2 s by default (15 s maximum). Require
// coverage of both phase boundaries and every interval, including drains.
// A single quiet sample or an app death followed by a late restart cannot pass.
func sampleCoverage(samples []memsteadySample, from, to int64) error {
	const maxGap = int64(20_000)
	if from <= 0 || to <= from {
		return errors.New("invalid sample interval")
	}
	last := from
	count := 0
	for _, sample := range samples {
		if sample.Millis < from || sample.Millis > to {
			continue
		}
		if sample.Millis-last > maxGap {
			return fmt.Errorf("memory sample gap exceeds 20 s (%d ms)", sample.Millis-last)
		}
		last = sample.Millis
		count++
	}
	if count == 0 || to-last > maxGap {
		return errors.New("missing memory samples at phase end")
	}
	return nil
}
