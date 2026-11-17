package connect

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The gated-preferred connection policy (EXTENDER-UPDATE2.md §1.1).
//
// The release route hands an authenticated identity a few gated records
// (EXTENDER.md R3), and the open tier is the sacrificial one every public
// channel carries. With the preference on, the connection path -- the client
// strategy's extender expansion, and nothing else: the feed, the probe pass
// and the gossip node draw as before -- dials gated records first and holds
// the open ones back while a gated attempt is within its budget. Preference
// applies only once an extender is needed: the strategy expands extenders when
// the dialers it already has did not deliver, so a healthy direct connection
// is never routed through one.
//
// The policy is a small state machine judged at each draw (ConnectCandidates):
//
//   - attempt: usable gated records exist and none has connected since the
//     attempt began. Only gated records (and manual addresses, which the user
//     configured) are drawn. The attempt ends in `gated` once a gated record
//     connects, or in `fallback` once no gated record is usable (they failed
//     and are held) or the budget (AttemptTimeout) has passed. Every dial in
//     the window shares the one budget.
//   - gated: a gated record connected. Only gated records are drawn, and the
//     strategy drains the open dialers it still holds (ConnectAddressUsable):
//     no new request uses them, and a live connection through one runs on.
//     During the attempt before it they keep serving, so an attempt on a
//     gated record that does not answer never strands a working connection.
//     When no gated record is usable any more the policy falls back.
//   - fallback: verified open records are drawn after the gated ones, under
//     the accessibility fallback of §1.1 rule 3, with the reason recorded. An
//     empty, limited or failed release is a reason like a failed gated dial: a
//     reachable release route alone never holds the fallback off. A gated
//     record that connects again, or one newly usable -- released since, or
//     its hold lapsed -- makes gated preferred again.
//   - off: the preference is disabled, and the draw is Candidates' own.
//
// A revoked or expired record is never a gated candidate, and nothing here
// makes an unverified address dialable. The policy never waits for a release:
// it decides from what the directory holds, and asks for a refresh through a
// monitor the release fetch watches (GatedRefreshMonitor), which coalesces.
//
// What leaves this client is unchanged by the tier: the open channels exclude
// gated and channel-restricted records (R1, R4), and the share export does
// too (publicSnapshotEntries, BuildExtenderShare). No address is ever logged
// or counted here, only modes, reasons and counts.

// The defaults of the gated preference.
const (
	// The bounded connection budget of one gated attempt (§1.1 rule 3): long
	// enough for the tcp, quic and dns carriers of a gated record to answer
	// on a slow path, short against the strategy's reconnect pace.
	DefaultExtenderGatedAttemptTimeout = 10 * time.Second
	// A release refresh is asked for when fewer current gated records than
	// this remain for the host's families (the low watermark).
	DefaultExtenderGatedLowWaterCount = 1
	// The most records one release response applies. A trusted identity is
	// entitled to three (ExtenderReleaseTrustedIdentityCount); a response
	// with more than this is not one the policy produced, and the rest is
	// dropped unread.
	ExtenderGatedReleaseMaxRecordCount = 8
)

// The gated preference of one directory. The zero value is off.
type ExtenderGatedPreferenceSettings struct {
	// Whether the connection path prefers gated records. Off by default; the
	// owner enables it per cohort (§ Delivery and rollout gates).
	Enabled bool
	// The budget of one gated attempt. <= 0 is the default.
	AttemptTimeout time.Duration
	// The low watermark. <= 0 is the default.
	LowWaterCount int
}

func DefaultExtenderGatedPreferenceSettings() *ExtenderGatedPreferenceSettings {
	return &ExtenderGatedPreferenceSettings{
		Enabled:        false,
		AttemptTimeout: DefaultExtenderGatedAttemptTimeout,
		LowWaterCount:  DefaultExtenderGatedLowWaterCount,
	}
}

// The modes of the policy.
const (
	ExtenderGatedModeOff      = "off"
	ExtenderGatedModeAttempt  = "attempt"
	ExtenderGatedModeGated    = "gated"
	ExtenderGatedModeFallback = "fallback"
)

// Why the open fallback was selected: a fixed label set, so a count by reason
// never carries anything of the client's.
const (
	// no gated record and no release answer yet: a cold client
	ExtenderGatedFallbackNoRecord = "no_gated_record"
	// the release answered with nothing usable, by its outcome (contract R)
	ExtenderGatedFallbackReleaseEmpty           = "release_empty"
	ExtenderGatedFallbackReleaseEpochSpent      = "release_epoch_spent"
	ExtenderGatedFallbackReleaseProbation       = "release_probation"
	ExtenderGatedFallbackReleaseCountryCapacity = "release_country_capacity"
	ExtenderGatedFallbackReleaseRateLimited     = "release_rate_limited"
	ExtenderGatedFallbackReleaseIncompatible    = "release_incompatible"
	ExtenderGatedFallbackReleaseUnavailable     = "release_unavailable"
	// the release request got no answer: transport, deadline, status, body
	ExtenderGatedFallbackReleaseFailed = "release_failed"
	// gated records are held but none is usable: they failed
	ExtenderGatedFallbackGatedFailed = "gated_failed"
	// the attempt's budget passed with no gated record connected
	ExtenderGatedFallbackGatedTimeout = "gated_timeout"
	// the control route could not be reached, and the cached open records
	// were let carry it (AllowOpenForControlReach, §1.1 rule 4)
	ExtenderGatedFallbackControlReach = "control_reach"
)

// Every fallback reason, the complete label set.
func ExtenderGatedFallbackReasons() []string {
	return []string{
		ExtenderGatedFallbackNoRecord,
		ExtenderGatedFallbackReleaseEmpty,
		ExtenderGatedFallbackReleaseEpochSpent,
		ExtenderGatedFallbackReleaseProbation,
		ExtenderGatedFallbackReleaseCountryCapacity,
		ExtenderGatedFallbackReleaseRateLimited,
		ExtenderGatedFallbackReleaseIncompatible,
		ExtenderGatedFallbackReleaseUnavailable,
		ExtenderGatedFallbackReleaseFailed,
		ExtenderGatedFallbackGatedFailed,
		ExtenderGatedFallbackGatedTimeout,
		ExtenderGatedFallbackControlReach,
	}
}

// What a release request came to, as the policy and its counts name it: an
// outcome of contract R, normalized (NormalizeExtenderGatedReleaseOutcome), or
// `failed` for a request that got no readable answer -- the client's own
// label, which no server answers with.
const ExtenderGatedReleaseFailed = "failed"

// The non-ok outcomes of contract R (ExtenderReleaseOutcome*,
// net_extender_directory_release.go), and the fallback each leads to when it
// leaves no usable gated record.
var extenderGatedReleaseFallbackReasons = map[string]string{
	ExtenderReleaseOutcomeEmpty:           ExtenderGatedFallbackReleaseEmpty,
	ExtenderReleaseOutcomeEpochSpent:      ExtenderGatedFallbackReleaseEpochSpent,
	ExtenderReleaseOutcomeProbation:       ExtenderGatedFallbackReleaseProbation,
	ExtenderReleaseOutcomeCountryCapacity: ExtenderGatedFallbackReleaseCountryCapacity,
	ExtenderReleaseOutcomeRateLimited:     ExtenderGatedFallbackReleaseRateLimited,
	ExtenderReleaseOutcomeIncompatible:    ExtenderGatedFallbackReleaseIncompatible,
	ExtenderReleaseOutcomeUnavailable:     ExtenderGatedFallbackReleaseUnavailable,
	ExtenderGatedReleaseFailed:            ExtenderGatedFallbackReleaseFailed,
}

// Normalizes a release outcome: `ok`, `failed` and the non-ok outcomes of
// contract R stand, case and space aside; anything else, an outcome a newer
// operator added included, is `empty`, as contract R says a client treats an
// outcome it does not know.
func NormalizeExtenderGatedReleaseOutcome(outcome string) string {
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if outcome == ExtenderReleaseOutcomeOk {
		return outcome
	}
	if _, ok := extenderGatedReleaseFallbackReasons[outcome]; ok {
		return outcome
	}
	return ExtenderReleaseOutcomeEmpty
}

// Every release outcome the counts name, the complete label set.
func ExtenderGatedReleaseOutcomes() []string {
	outcomes := []string{ExtenderReleaseOutcomeOk}
	for outcome := range extenderGatedReleaseFallbackReasons {
		outcomes = append(outcomes, outcome)
	}
	slices.Sort(outcomes[1:])
	return outcomes
}

// The classes of a connection candidate, which are also the labels of the
// candidate counts.
const (
	ExtenderGatedClassGated  = "gated"
	ExtenderGatedClassOpen   = "open"
	ExtenderGatedClassManual = "manual"
)

// The order of the classes in a draw: gated, then verified open, then manual
// addresses no record verifies.
const (
	extenderGatedOrderGated = iota
	extenderGatedOrderOpen
	extenderGatedOrderManual
)

var errExtenderReleaseRecordNotGated = errors.New("a released extender record is not in the gated tier")

// The policy's counts, all over fixed label sets.
type ExtenderGatedStats struct {
	// Connection draws, by the mode each was drawn in.
	DrawCounts map[string]uint64
	// Candidates handed to the connection path, by class.
	CandidateCounts map[string]uint64
	// Entries into the open fallback, by reason.
	FallbackCounts map[string]uint64
	// Returns from the fallback to gated: a gated record that connected
	// again, or one newly usable to try.
	RecoveryCount uint64
	// Release outcomes reported, by outcome.
	ReleaseOutcomeCounts map[string]uint64
	// Release refreshes the policy asked for: the low watermark, a fallback.
	RefreshWantedCount uint64
	// Addresses carrying a live connection now, by class (a gauge).
	InUseCounts map[string]int
}

// The policy as the status reads it.
type ExtenderGatedPreferenceStatus struct {
	Enabled bool
	Mode    string
	// Why the open fallback is in force, empty outside it.
	FallbackReason string
	// At the last judgment: the usable gated identities of the host's
	// families, and the current gated records that name one of them.
	UsableGatedCount int
	GatedRecordCount int
	// The last release outcome reported, empty before the first.
	ReleaseOutcome string
	Stats          ExtenderGatedStats
}

// The gated records of the host's families, for a release schedule (§1.3).
type ExtenderGatedRecordSummary struct {
	// Current gated records: verified, not revoked, not expired.
	CurrentCount int
	// Of those, the ones with a usable address now.
	UsableCount int
	// The earliest expiry among the current ones, zero for none.
	EarliestExpireTime time.Time
}

// The policy state, under the directory's state lock. The zero value is off.
type extenderGatedState struct {
	settings ExtenderGatedPreferenceSettings
	// the reset the state was judged in: a reset (E7) starts the policy over
	resetVersion uint64

	mode           string
	attemptTime    time.Time
	fallbackTime   time.Time
	fallbackReason string
	// the usable gated identities the fallback has seen, so only a newly
	// usable one is a reason to try gated again
	fallbackSeenKeyHexes map[string]bool
	releaseOutcome       string
	// whether the low watermark has been crossed and asked for a refresh
	lowWater bool
	// the host's families at the last draw, every family before the first
	ipVersions       []int
	usableGatedCount int
	gatedRecordCount int

	refreshMonitor *Monitor

	drawCounts           map[string]uint64
	candidateCounts      map[string]uint64
	fallbackCounts       map[string]uint64
	recoveryCount        uint64
	releaseOutcomeCounts map[string]uint64
	refreshWantedCount   uint64
}

// Replaces the gated preference. Turning it on or off starts the policy over
// at the next draw.
func (self *ExtenderDirectory) SetGatedPreference(settings *ExtenderGatedPreferenceSettings) {
	copied := ExtenderGatedPreferenceSettings{}
	if settings != nil {
		copied = *settings
	}
	if copied.AttemptTimeout <= 0 {
		copied.AttemptTimeout = DefaultExtenderGatedAttemptTimeout
	}
	if copied.LowWaterCount <= 0 {
		copied.LowWaterCount = DefaultExtenderGatedLowWaterCount
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	state := &self.gated
	if state.settings.Enabled != copied.Enabled {
		state.mode = ""
		state.lowWater = false
		state.fallbackSeenKeyHexes = nil
	}
	state.settings = copied
	if !copied.Enabled {
		state.mode = ExtenderGatedModeOff
		state.fallbackReason = ""
	}
}

// Whether the gated preference is on.
func (self *ExtenderDirectory) GatedPreferenceEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.gated.settings.Enabled
}

// The policy now, with its counts.
func (self *ExtenderDirectory) GatedPreference() ExtenderGatedPreferenceStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	state := &self.gated
	// empty while enabled and not yet judged: no draw has needed an extender
	mode := state.mode
	if !state.settings.Enabled {
		mode = ExtenderGatedModeOff
	}
	status := ExtenderGatedPreferenceStatus{
		Enabled:          state.settings.Enabled,
		Mode:             mode,
		UsableGatedCount: state.usableGatedCount,
		GatedRecordCount: state.gatedRecordCount,
		ReleaseOutcome:   state.releaseOutcome,
		Stats: ExtenderGatedStats{
			DrawCounts:           maps.Clone(state.drawCounts),
			CandidateCounts:      maps.Clone(state.candidateCounts),
			FallbackCounts:       maps.Clone(state.fallbackCounts),
			RecoveryCount:        state.recoveryCount,
			ReleaseOutcomeCounts: maps.Clone(state.releaseOutcomeCounts),
			RefreshWantedCount:   state.refreshWantedCount,
			InUseCounts:          self.gatedInUseCountsWithLock(),
		},
	}
	if mode == ExtenderGatedModeFallback {
		status.FallbackReason = state.fallbackReason
	}
	return status
}

// Notified whenever the policy asks for a release refresh: the low watermark
// was crossed, or the policy fell back for a reason a release can mend. The
// release fetch coalesces what it hears with its other triggers.
func (self *ExtenderDirectory) GatedRefreshMonitor() *Monitor {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.gatedRefreshMonitorWithLock()
}

func (self *ExtenderDirectory) gatedRefreshMonitorWithLock() *Monitor {
	if self.gated.refreshMonitor == nil {
		self.gated.refreshMonitor = NewMonitor()
	}
	return self.gated.refreshMonitor
}

// Up to `count` connection candidates of each family of `ipVersions`, in that
// order: what the client strategy expands its extender dialers from (E2). With
// the preference off this is `Candidates` (or `VerifiedCandidates` with
// `verifiedOnly`) for each family. With it on the policy is judged first, over
// all of the families together, so one family's gated records hold back the
// other family's open ones too.
func (self *ExtenderDirectory) ConnectCandidates(
	ipVersions []int,
	count int,
	verifiedOnly bool,
	exclude []netip.Addr,
) [][]*ExtenderCandidate {
	familyCandidates := make([][]*ExtenderCandidate, len(ipVersions))
	for i := range familyCandidates {
		familyCandidates[i] = []*ExtenderCandidate{}
	}
	if count <= 0 || len(ipVersions) == 0 {
		return familyCandidates
	}
	excludeIps := map[netip.Addr]bool{}
	for _, ip := range exclude {
		excludeIps[ip.Unmap()] = true
	}
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if !self.gated.settings.Enabled {
		for i, ipVersion := range ipVersions {
			familyCandidates[i] = self.candidatesWithLock(ipVersion, count, verifiedOnly, excludeIps, now, nil)
		}
		return familyCandidates
	}

	self.gatedJudgeWithLock(ipVersions, now)
	class := self.gatedOrderWithLock(now)
	for i, ipVersion := range ipVersions {
		familyCandidates[i] = self.candidatesWithLock(ipVersion, count, verifiedOnly, excludeIps, now, class)
	}
	state := &self.gated
	state.drawCounts = extenderGatedCountAdd(state.drawCounts, state.mode, 1)
	for _, candidates := range familyCandidates {
		for _, candidate := range candidates {
			candidateClass := ExtenderGatedClassManual
			switch {
			case candidate.Verified && candidate.DirectoryTier == ExtenderDirectoryTierGated:
				candidateClass = ExtenderGatedClassGated
			case candidate.Verified:
				candidateClass = ExtenderGatedClassOpen
			}
			state.candidateCounts = extenderGatedCountAdd(state.candidateCounts, candidateClass, 1)
			self.blockEvidenceDrawnWithLock(candidate)
		}
	}
	return familyCandidates
}

// Whether the connection path may keep a dialer on `ip`: AddressUsable, and,
// once a gated record has connected (gated), not an open record. A dialer the
// strategy drops for this keeps its live connections; only new requests stop
// using it, which is how working connections drain naturally once gated has
// recovered (§1.1 rule 5). During an attempt the open dialers the strategy
// already holds keep serving -- new draws are gated alone -- so an attempt on
// a gated record that never answers does not strand a working connection for
// the attempt's budget. An expired gated record keeps the last-resort rule of
// AddressUsable.
func (self *ExtenderDirectory) ConnectAddressUsable(ip netip.Addr) bool {
	if !self.AddressUsable(ip) {
		return false
	}
	ip = ip.Unmap()
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if !self.gated.settings.Enabled || self.gated.mode != ExtenderGatedModeGated {
		return true
	}
	// the mode as of now: when the gated records stopped being usable since
	// the last draw, the policy falls back here, before an open dialer that
	// carries the client is drained
	self.gatedJudgeWithLock(nil, now)
	if self.gated.mode != ExtenderGatedModeGated {
		return true
	}
	address := self.ipAddresses[ip]
	if address == nil {
		return false
	}
	return self.gatedClassOrderWithLock(address) != extenderGatedOrderOpen
}

// Lets the cached verified open records carry the control route now (§1.1
// rule 4): when the policy holds them back for gated records, it falls back at
// once for the reason control_reach rather than at the end of the attempt's
// budget. Reports whether that made a verified open candidate dialable that
// was not: false with the preference off or already falling back, where they
// are dialable, and false when the directory holds none, where the caller goes
// on to another reach (the seed bootstrap of §2).
func (self *ExtenderDirectory) AllowOpenForControlReach() bool {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if !self.gated.settings.Enabled {
		return false
	}
	self.gatedJudgeWithLock(nil, now)
	switch self.gated.mode {
	case ExtenderGatedModeAttempt, ExtenderGatedModeGated:
	default:
		return false
	}
	self.gatedFallbackWithLock(
		now,
		ExtenderGatedFallbackControlReach,
		self.usableGatedKeyHexesWithLock(self.gated.ipVersions, now),
	)
	return 0 < self.usableOpenCountWithLock(self.gated.ipVersions, now)
}

// Records what the last release request came to (contract R): `ok`, a non-ok
// outcome, or `failed`. It names the fallback when the release left no usable
// gated record (gatedUnusableReasonWithLock), and it is counted.
func (self *ExtenderDirectory) SetGatedReleaseOutcome(outcome string) {
	outcome = NormalizeExtenderGatedReleaseOutcome(outcome)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.gated.releaseOutcome = outcome
	self.gated.releaseOutcomeCounts = extenderGatedCountAdd(self.gated.releaseOutcomeCounts, outcome, 1)
}

// The gated records of the host's families now (§1.3).
func (self *ExtenderDirectory) GatedRecordSummary() ExtenderGatedRecordSummary {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	summary := ExtenderGatedRecordSummary{}
	ipVersions := self.gated.ipVersions
	for keyHex, keyRecord := range self.keyHexRecords {
		if !self.gatedRecordCurrentWithLock(keyHex, keyRecord, ipVersions, now) {
			continue
		}
		summary.CurrentCount += 1
		if 0 < keyRecord.recordBody.ExpireTimeMs {
			expireTime := time.UnixMilli(int64(keyRecord.recordBody.ExpireTimeMs))
			if summary.EarliestExpireTime.IsZero() || expireTime.Before(summary.EarliestExpireTime) {
				summary.EarliestExpireTime = expireTime
			}
		}
	}
	summary.UsableCount = len(self.usableGatedKeyHexesWithLock(ipVersions, now))
	return summary
}

// The directory's reset count. A release fetched in one generation applies
// only within it (ApplyReleaseRecord): a reset (E7) that lands while the
// request is out drops its answer.
func (self *ExtenderDirectory) ResetGeneration() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.resetVersion
}

// Applies one record of a gated release (R3; §1.1 rule 2) that was requested
// in reset generation `generation`, with source release, which is not a
// network event (K4). It is validated as every record is -- the root
// signature under the keys in force, an allowed network host, a key -- and
// further must be a current record of the gated tier, since the release route
// returns nothing else. A record requested before a reset is not applied
// after it.
func (self *ExtenderDirectory) ApplyReleaseRecord(
	record *protocol.ExtenderRecord,
	generation uint64,
) (changed bool, err error) {
	keySet, _ := self.rootKeysAndResetVersion()
	body, err := keySet.VerifyRecord(record)
	if err != nil {
		return false, err
	}
	if !ExtenderNetworkHostAllowed(body.NetworkHost, self.settings.NetworkHosts...) {
		return false, fmt.Errorf("extender record is for network host %q", body.NetworkHost)
	}
	if len(body.PublicKey) == 0 {
		return false, fmt.Errorf("extender record carries no public key")
	}
	if !ExtenderRecordGated(body) {
		return false, errExtenderReleaseRecordNotGated
	}
	if 0 < body.ExpireTimeMs {
		expireTime := time.UnixMilli(int64(body.ExpireTimeMs)).Add(self.settings.RecordExpireSkew)
		if expireTime.Before(self.settings.Now()) {
			return false, fmt.Errorf("a released extender record has expired")
		}
	}
	self.verified()
	return self.applyVerifiedRecord(record, body, keySet, generation, ExtenderSourceRelease)
}

// Drops every gated record the directory holds, with the addresses only they
// name, and reports how many. Revocations stay, so a replayed record cannot
// bring a key back; a manual address outlives its record as an unverified
// manual entry; this extender's own record (KeepPublicKey) stays. An account
// change does this (§1.1 rule 2): what the operator released to one identity
// is not the next one's to dial.
func (self *ExtenderDirectory) ForgetGatedRecords() int {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	count := 0
	for _, keyHex := range slices.Sorted(maps.Keys(self.keyHexRecords)) {
		keyRecord := self.keyHexRecords[keyHex]
		if keyRecord.recordBody == nil || !ExtenderRecordGated(keyRecord.recordBody) {
			continue
		}
		if self.keptKeyHexes[keyHex] {
			continue
		}
		keyRecord.record = nil
		keyRecord.recordBody = nil
		self.tierRemoveWithLock(keyHex, keyRecord)
		if keyRecord.revocation == nil {
			self.deleteKeyRecordWithLock(keyHex)
		}
		for ip, address := range self.ipAddresses {
			if address.publicKeyHex != keyHex {
				continue
			}
			if address.source == ExtenderSourceManual {
				address.publicKeyHex = ""
				continue
			}
			delete(self.ipAddresses, ip)
		}
		count += 1
	}
	if 0 < count {
		self.gated.mode = ""
		self.gated.lowWater = false
		self.gated.fallbackSeenKeyHexes = nil
		self.tierRebuildWithLock(now)
		self.changedWithLock()
	}
	return count
}

// The families the client strategy dials extenders of now (E2): those the
// control family ledger allows, which is what the release request names as
// the families it can dial (`ip_versions`). Empty when it allows none.
func ExtenderDialIpVersions() []int {
	ipVersions := []int{}
	for _, ipVersion := range []int{4, 6} {
		if controlFamilyProbe(ipVersion) {
			ipVersions = append(ipVersions, ipVersion)
		}
	}
	return ipVersions
}

// Judges the mode at `now` for the host's families (nil keeps the last), and
// asks for a refresh on the low watermark.
func (self *ExtenderDirectory) gatedJudgeWithLock(ipVersions []int, now time.Time) {
	state := &self.gated
	if !state.settings.Enabled {
		state.mode = ExtenderGatedModeOff
		return
	}
	if state.resetVersion != self.resetVersion {
		// a reset (E7) cleared what the policy judged; it starts over
		state.resetVersion = self.resetVersion
		state.mode = ""
		state.lowWater = false
		state.fallbackSeenKeyHexes = nil
	}
	if 0 < len(ipVersions) {
		state.ipVersions = slices.Clone(ipVersions)
	}
	usableKeyHexes := self.usableGatedKeyHexesWithLock(state.ipVersions, now)
	state.usableGatedCount = len(usableKeyHexes)
	state.gatedRecordCount = self.currentGatedRecordCountWithLock(state.ipVersions, now)

	if state.gatedRecordCount < state.settings.LowWaterCount {
		if !state.lowWater {
			state.lowWater = true
			self.gatedRefreshWantedWithLock()
		}
	} else {
		state.lowWater = false
	}

	switch state.mode {
	case ExtenderGatedModeAttempt:
		switch {
		case self.gatedConnectedSinceWithLock(state.attemptTime):
			self.gatedModeWithLock(ExtenderGatedModeGated)
		case len(usableKeyHexes) == 0:
			self.gatedFallbackWithLock(now, self.gatedUnusableReasonWithLock(), usableKeyHexes)
		case state.settings.AttemptTimeout <= extenderElapsed(now, state.attemptTime):
			self.gatedFallbackWithLock(now, ExtenderGatedFallbackGatedTimeout, usableKeyHexes)
		}
	case ExtenderGatedModeGated:
		if len(usableKeyHexes) == 0 {
			self.gatedFallbackWithLock(now, self.gatedUnusableReasonWithLock(), usableKeyHexes)
		}
	case ExtenderGatedModeFallback:
		newlyUsable := false
		for keyHex := range usableKeyHexes {
			if !state.fallbackSeenKeyHexes[keyHex] {
				newlyUsable = true
				break
			}
		}
		// an identity that stops being usable leaves the seen set, so the
		// lapse of its hold makes it newly usable again
		state.fallbackSeenKeyHexes = usableKeyHexes
		switch {
		case 0 < len(usableKeyHexes) && self.gatedConnectedSinceWithLock(state.fallbackTime):
			state.recoveryCount += 1
			self.gatedModeWithLock(ExtenderGatedModeGated)
		case newlyUsable:
			state.recoveryCount += 1
			state.attemptTime = now
			self.gatedModeWithLock(ExtenderGatedModeAttempt)
		}
	default:
		if 0 < len(usableKeyHexes) {
			state.attemptTime = now
			self.gatedModeWithLock(ExtenderGatedModeAttempt)
		} else {
			self.gatedFallbackWithLock(now, self.gatedUnusableReasonWithLock(), usableKeyHexes)
		}
	}
}

// A current gated record connected (RecordSuccess), by a dial or a probe: an
// attempt or a fallback gives way to gated at once, rather than at the next
// draw, which a healthy connection may never need. The strategy drains its
// open dialers at its next collapse (ConnectAddressUsable).
func (self *ExtenderDirectory) gatedConnectedWithLock(address *extenderDirectoryAddress, now time.Time) {
	state := &self.gated
	if !state.settings.Enabled || state.resetVersion != self.resetVersion {
		return
	}
	if self.gatedClassOrderWithLock(address) != extenderGatedOrderGated || !self.addressActiveWithLock(address, now) {
		return
	}
	switch state.mode {
	case ExtenderGatedModeAttempt:
		self.gatedModeWithLock(ExtenderGatedModeGated)
	case ExtenderGatedModeFallback:
		state.recoveryCount += 1
		self.gatedModeWithLock(ExtenderGatedModeGated)
	}
}

func (self *ExtenderDirectory) gatedModeWithLock(mode string) {
	if self.gated.mode == mode {
		return
	}
	self.gated.mode = mode
	if mode != ExtenderGatedModeFallback {
		self.gated.fallbackReason = ""
	}
	if self.log.V(1).Enabled() {
		self.log.Infof("[extender]gated preference %s\n", mode)
	}
}

// Enters the open fallback for `reason`. A fallback a release can mend asks
// for one; a fallback the release itself answered waits on the release
// schedule, which already knows.
func (self *ExtenderDirectory) gatedFallbackWithLock(now time.Time, reason string, usableKeyHexes map[string]bool) {
	state := &self.gated
	state.fallbackTime = now
	state.fallbackReason = reason
	state.fallbackSeenKeyHexes = maps.Clone(usableKeyHexes)
	if state.fallbackSeenKeyHexes == nil {
		state.fallbackSeenKeyHexes = map[string]bool{}
	}
	state.fallbackCounts = extenderGatedCountAdd(state.fallbackCounts, reason, 1)
	if self.log.V(1).Enabled() {
		self.log.Infof("[extender]gated preference fallback %s\n", reason)
	}
	state.mode = ExtenderGatedModeFallback
	if !strings.HasPrefix(reason, "release_") {
		self.gatedRefreshWantedWithLock()
	}
}

func (self *ExtenderDirectory) gatedRefreshWantedWithLock() {
	self.gated.refreshWantedCount += 1
	self.gatedRefreshMonitorWithLock().NotifyAll()
}

// Why no gated record is usable: they are held, or there is none, which the
// last release answer explains when it had one.
func (self *ExtenderDirectory) gatedUnusableReasonWithLock() string {
	if 0 < self.gated.gatedRecordCount {
		return ExtenderGatedFallbackGatedFailed
	}
	if reason, ok := extenderGatedReleaseFallbackReasons[self.gated.releaseOutcome]; ok {
		return reason
	}
	return ExtenderGatedFallbackNoRecord
}

// The class order of the mode in force, for candidatesWithLock: gated alone
// (with the manual addresses the user configured) while gated is preferred,
// every class gated first in the fallback.
func (self *ExtenderDirectory) gatedOrderWithLock(now time.Time) func(address *extenderDirectoryAddress) int {
	switch self.gated.mode {
	case ExtenderGatedModeAttempt, ExtenderGatedModeGated:
		return func(address *extenderDirectoryAddress) int {
			return self.gatedOnlyOrderWithLock(address, now)
		}
	case ExtenderGatedModeFallback:
		return self.gatedClassOrderWithLock
	default:
		return nil
	}
}

// The order of an address while gated records are preferred alone: a current
// gated record first, a manual address after, and -1 -- withheld -- for an
// open record and for an expired one, which are the fallback's.
func (self *ExtenderDirectory) gatedOnlyOrderWithLock(address *extenderDirectoryAddress, now time.Time) int {
	switch order := self.gatedClassOrderWithLock(address); order {
	case extenderGatedOrderGated:
		if self.keyRecordExpiredWithLock(self.keyHexRecords[address.publicKeyHex], now) {
			return -1
		}
		return order
	case extenderGatedOrderManual:
		return order
	default:
		return -1
	}
}

// The class of an address in draw order: a record of the gated tier, a
// verified open record, or an address no record verifies.
func (self *ExtenderDirectory) gatedClassOrderWithLock(address *extenderDirectoryAddress) int {
	if address.publicKeyHex == "" {
		return extenderGatedOrderManual
	}
	if keyRecord := self.keyHexRecords[address.publicKeyHex]; keyRecord != nil && ExtenderRecordGated(keyRecord.recordBody) {
		return extenderGatedOrderGated
	}
	return extenderGatedOrderOpen
}

// The gated identities with a usable address of the families now: verified,
// current, not revoked, not held, not limited.
func (self *ExtenderDirectory) usableGatedKeyHexesWithLock(ipVersions []int, now time.Time) map[string]bool {
	keyHexes := map[string]bool{}
	for ip, address := range self.ipAddresses {
		if address.publicKeyHex == "" || !extenderGatedFamilyWanted(ipVersions, ip) {
			continue
		}
		keyRecord := self.keyHexRecords[address.publicKeyHex]
		if keyRecord == nil || !ExtenderRecordGated(keyRecord.recordBody) {
			continue
		}
		if extenderBefore(now, address.holdUntilTime) || extenderBefore(now, address.limitedUntilTime) {
			continue
		}
		if !self.addressActiveWithLock(address, now) {
			continue
		}
		keyHexes[address.publicKeyHex] = true
	}
	return keyHexes
}

// The usable verified open addresses of the families now, limited aside.
func (self *ExtenderDirectory) usableOpenCountWithLock(ipVersions []int, now time.Time) int {
	count := 0
	for ip, address := range self.ipAddresses {
		if address.publicKeyHex == "" || !extenderGatedFamilyWanted(ipVersions, ip) {
			continue
		}
		if self.gatedClassOrderWithLock(address) != extenderGatedOrderOpen {
			continue
		}
		if extenderBefore(now, address.holdUntilTime) || extenderBefore(now, address.limitedUntilTime) {
			continue
		}
		if !self.addressActiveWithLock(address, now) {
			continue
		}
		count += 1
	}
	return count
}

// The current gated records that name an address of the families, held or
// not.
func (self *ExtenderDirectory) currentGatedRecordCountWithLock(ipVersions []int, now time.Time) int {
	count := 0
	for keyHex, keyRecord := range self.keyHexRecords {
		if self.gatedRecordCurrentWithLock(keyHex, keyRecord, ipVersions, now) {
			count += 1
		}
	}
	return count
}

// Whether one identity holds a current gated record -- verified, not revoked,
// not expired -- that names an address of the families it still holds.
func (self *ExtenderDirectory) gatedRecordCurrentWithLock(
	keyHex string,
	keyRecord *extenderDirectoryRecord,
	ipVersions []int,
	now time.Time,
) bool {
	if keyRecord == nil || keyRecord.recordBody == nil || !ExtenderRecordGated(keyRecord.recordBody) {
		return false
	}
	if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
		return false
	}
	for _, ip := range keyRecord.ips {
		if address := self.ipAddresses[ip]; address != nil && address.publicKeyHex == keyHex && extenderGatedFamilyWanted(ipVersions, ip) {
			return true
		}
	}
	return false
}

// Whether a gated address connected at or after `since`, by a dial or a probe.
func (self *ExtenderDirectory) gatedConnectedSinceWithLock(since time.Time) bool {
	for _, address := range self.ipAddresses {
		if address.successCount == 0 || address.lastSuccessTime.Before(since) {
			continue
		}
		if self.gatedClassOrderWithLock(address) == extenderGatedOrderGated {
			return true
		}
	}
	return false
}

// The addresses with a live connection now, by class.
func (self *ExtenderDirectory) gatedInUseCountsWithLock() map[string]int {
	counts := map[string]int{}
	for ip, inUseCount := range self.ipInUseCounts {
		if inUseCount <= 0 {
			continue
		}
		address := self.ipAddresses[ip]
		if address == nil {
			continue
		}
		switch self.gatedClassOrderWithLock(address) {
		case extenderGatedOrderGated:
			counts[ExtenderGatedClassGated] += 1
		case extenderGatedOrderOpen:
			counts[ExtenderGatedClassOpen] += 1
		default:
			counts[ExtenderGatedClassManual] += 1
		}
	}
	return counts
}

// The addresses that must not leave this client on a public path (§1.3; R1,
// R4): those of a record that is not an ordinary open one, gated or
// channel-restricted, whatever source it arrived from. The share export
// leaves them out (BuildExtenderShare).
func (self *ExtenderDirectory) restrictedAddressesWithLock() map[netip.Addr]bool {
	restricted := map[netip.Addr]bool{}
	for ip, address := range self.ipAddresses {
		if address.publicKeyHex == "" {
			continue
		}
		keyRecord := self.keyHexRecords[address.publicKeyHex]
		if keyRecord == nil || keyRecord.recordBody == nil {
			continue
		}
		if !ExtenderRecordOpen(keyRecord.recordBody) {
			restricted[ip] = true
		}
	}
	return restricted
}

// The entries of Snapshot a public path may carry: every one whose address no
// gated or channel-restricted record names (restrictedAddressesWithLock). The
// snapshot and the restriction are read under one lock, so a record applied
// between the two never slips through.
func (self *ExtenderDirectory) publicSnapshotEntries() []*ExtenderDirectoryEntry {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	restricted := self.restrictedAddressesWithLock()
	return slices.DeleteFunc(self.snapshotWithLock(now).Entries, func(entry *ExtenderDirectoryEntry) bool {
		return restricted[entry.Ip]
	})
}

// Whether `ip` is of one of the families; none named is every family.
func extenderGatedFamilyWanted(ipVersions []int, ip netip.Addr) bool {
	return len(ipVersions) == 0 || slices.Contains(ipVersions, addressIpVersion(ip))
}

// Adds to one count of a fixed label set, making the map on first use.
func extenderGatedCountAdd(counts map[string]uint64, label string, n uint64) map[string]uint64 {
	if counts == nil {
		counts = map[string]uint64{}
	}
	counts[label] += n
	return counts
}
