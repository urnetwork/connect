package connect

import (
	"context"
	"encoding/hex"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// What a client treats as evidence that an extender is blocked for it, and the
// dials that gather it (EXTENDER-UPDATE2.md §1.3; EXTENDER.md R4).
//
// The blocked state is the operator's, per record and country: enough distinct
// clients there reported a record while the operator's own probe still reaches
// it from outside. A report names a key and nothing else, no carrier and no
// address, and the operator's probe dials tcp alone. So a report has to mean
// the whole record did not answer this client. It must not mean that one
// carrier failed, that a family is broken on this path, that a request through
// the extender failed further along, or that the client has no network.
//
// The rule. A record is reported only when all of these hold:
//
//  1. A real dial of its verified record failed, which arms it as a suspect.
//     That dial alone counts for nothing: it says nothing about the other
//     carriers, and a dial of the strategy can fail for reasons past the
//     extender. A suspect is a record the client was released (the gated tier)
//     or that its connection path drew (ConnectCandidates): one it uses, not
//     one it only heard of. Its record must be current under the root keys in
//     force, neither revoked nor expired, and neither a manual address nor this
//     extender's own record.
//  2. MinAttemptCount attempts failed, each AttemptInterval after the one
//     before, so at least an hour passes from the first to the last. An attempt
//     dials every carrier the record offers this client, on every address it
//     names in the families the client dials now, through the same dial as the
//     probe pass, each with the budget the operator's own probe has
//     (AttemptBlockSuspect). Tcp is first and must be among them: it is the one
//     carrier the operator's probe can be compared with, so a record with no
//     tcp carrier is never reported. Every dial must fail. This settles the
//     carrier question without a carrier on the wire: since a report means
//     every carrier failed, including the one the operator checks, a failure of
//     one carrier is never read as a block.
//  3. Each attempt is bracketed by proof that the client's own network works:
//     the control route answered before the attempt, or nothing is dialed, and
//     answered again after its last failure, or the attempt does not count. The
//     caller asks, and tells the directory only of the attempts that passed
//     (RecordBlockAttempts).
//  4. It all happened on one network path. A path change ends the attempt in
//     flight and drops every suspect (BlockEvidencePathChanged), so the next
//     attempt needs a failed dial on the new path and waits out
//     AttemptSettleTimeout after it. A failure while the network changed can
//     arm a suspect, and it never counts.
//  5. Nothing answered meanwhile. Any answer from any address of the record on
//     any carrier -- a success, or a limit (429), which only a reachable
//     extender can give -- drops the suspect, from an attempt or from any other
//     dial of this client.
//  6. The last attempt is recent: within ReportFreshTimeout, else another
//     attempt has to pass first, and within MaxWindow of the failure that armed
//     it, else the suspect is dropped.
//
// The bounds. At most MaxSuspectCount suspects are kept, an attempt pass takes
// at most MaxAttemptSuspectCount, and a record reported (ReportedQuietTimeout)
// or answering (AnsweredQuietTimeout) is left alone for a while. An attempt
// costs nothing on a healthy client, which arms no suspect.
//
// None of this changes what the client trusts or dials. An attempt never calls
// RecordFailure or RecordSuccess, so no hold, count, removal or policy mode
// moves because of it, and a report takes nothing out of the candidate order.
// The evidence is per process and never stored. Nothing here logs an address or
// a key.

// The numbers of the rule, all settings. The zero value is off.
type ExtenderBlockEvidenceSettings struct {
	// Whether the directory keeps evidence at all. Off by default; the sdk
	// turns it on with the gated preference, for a space with a bound identity.
	Enabled bool
	// How long after the failure that armed a suspect its first attempt waits,
	// which is also how soon after a path change one can start.
	AttemptSettleTimeout time.Duration
	// The least time from one attempt of a record to the next that counts.
	AttemptInterval time.Duration
	// The attempts that must fail. Three, each AttemptInterval apart, is an
	// hour from first to last.
	MinAttemptCount int
	// A report is due only this soon after the record's last attempt.
	ReportFreshTimeout time.Duration
	// A suspect not reported this long after the failure that armed it is
	// dropped, so old evidence never adds to new.
	MaxWindow time.Duration
	// A record reported is not a suspect again for this long. Long enough that
	// one client re-reports at about the report window of the blocked state.
	ReportedQuietTimeout time.Duration
	// A record that answered is not a suspect again for this long, so a record
	// that fails some other way than by being unreachable costs few attempts.
	AnsweredQuietTimeout time.Duration
	// Suspects kept at once. Past it a gated record replaces an open one, and
	// otherwise the new one is left out.
	MaxSuspectCount int
	// Records dialed by one attempt pass, gated first, then the oldest.
	MaxAttemptSuspectCount int
	// The budget of one dial of an attempt: the operator's own probe's
	// (server ExtenderProbeAddressTimeout), since a slow extender is not a
	// blocked one.
	ProbeTimeout time.Duration
}

func DefaultExtenderBlockEvidenceSettings() *ExtenderBlockEvidenceSettings {
	return &ExtenderBlockEvidenceSettings{
		Enabled:                false,
		AttemptSettleTimeout:   1 * time.Minute,
		AttemptInterval:        30 * time.Minute,
		MinAttemptCount:        3,
		ReportFreshTimeout:     30 * time.Minute,
		MaxWindow:              6 * time.Hour,
		ReportedQuietTimeout:   24 * time.Hour,
		AnsweredQuietTimeout:   6 * time.Hour,
		MaxSuspectCount:        16,
		MaxAttemptSuspectCount: 3,
		ProbeTimeout:           15 * time.Second,
	}
}

// The quiet records remembered at most. Past it the one whose quiet ends first
// goes.
const extenderBlockMaxQuietCount = 256

// The dials one attempt of one record makes at most. A root-signed record names
// two addresses of three carriers; one that names more than this is not
// attempted, since a pass could not finish it.
const extenderBlockMaxAttemptDialCount = 16

// What one attempt of one suspect came to, a fixed label set.
const (
	// every carrier of every address failed: evidence once the control route
	// answers again
	ExtenderBlockAttemptFailed = "failed"
	// an address answered on a carrier, or limited the request: the record is
	// reachable, and the suspect is dropped
	ExtenderBlockAttemptAnswered = "answered"
	// canceled, refused by this host's memory budget, or the record changed
	// under it: says nothing
	ExtenderBlockAttemptAborted = "aborted"
)

// A record under suspicion as a look hands it out, to attempt and to record.
type ExtenderBlockSuspect struct {
	PublicKey []byte

	keyHex string
	// the suspect it was read from: one dropped and armed again is another
	serial uint64
	// the addresses an attempt dials, the families it dials them in
	addressKey string
	ipVersions []int
}

// What the evidence asks for now.
type ExtenderBlockEvidenceLook struct {
	// The records a report is due for, gated first, then the oldest.
	DuePublicKeys [][]byte
	// The suspects an attempt pass dials now, gated first, then the oldest, at
	// most MaxAttemptSuspectCount.
	AttemptSuspects []ExtenderBlockSuspect
	// When the first suspect is due an attempt, at or before now when
	// AttemptSuspects has any, zero when none will be.
	NextAttemptTime time.Time
	// The suspects kept.
	SuspectCount int
}

// One record under suspicion.
type extenderBlockSuspect struct {
	serial  uint64
	armTime time.Time
	// the first attempt waits until then
	settleUntilTime time.Time

	// the attempts that passed, on the addresses of addressKey
	attemptCount    int
	lastAttemptTime time.Time
	addressKey      string
}

// The evidence state, under the directory's state lock. The zero value is off.
type extenderBlockEvidenceState struct {
	settings ExtenderBlockEvidenceSettings
	monitor  *Monitor

	suspects        map[string]*extenderBlockSuspect
	nextSerial      uint64
	quietUntilTimes map[string]time.Time
}

// Turns the evidence on with `settings`, or off with nil or a disabled one.
// Off forgets everything.
func (self *ExtenderDirectory) SetBlockEvidence(settings *ExtenderBlockEvidenceSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	state := &self.blockEvidence
	if settings == nil || !settings.Enabled {
		self.blockEvidenceForgetWithLock()
		state.settings = ExtenderBlockEvidenceSettings{}
		return
	}
	state.settings = *settings
}

// Notified whenever a failed dial arms a suspect, the one change the sender
// cannot time for itself.
func (self *ExtenderDirectory) BlockEvidenceMonitor() *Monitor {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.blockEvidenceMonitorWithLock()
}

func (self *ExtenderDirectory) blockEvidenceMonitorWithLock() *Monitor {
	if self.blockEvidence.monitor == nil {
		self.blockEvidence.monitor = NewMonitor()
	}
	return self.blockEvidence.monitor
}

// Forgets every suspect, quiet record and connection draw: an account change,
// since the records of one identity are not the next one's to report.
func (self *ExtenderDirectory) ForgetBlockEvidence() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.blockEvidenceForgetWithLock()
}

func (self *ExtenderDirectory) blockEvidenceForgetWithLock() {
	state := &self.blockEvidence
	state.suspects = nil
	state.quietUntilTimes = nil
	for _, keyRecord := range self.keyHexRecords {
		keyRecord.connectDrawn = false
	}
}

// The network path changed (rule 4): every suspect is dropped, since what their
// attempts found describes the old path. Records left quiet stay quiet.
func (self *ExtenderDirectory) BlockEvidencePathChanged() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.blockEvidence.suspects = nil
}

// Whether a record of the current directory can be a suspect (rule 1).
func (self *ExtenderDirectory) blockEvidenceEligibleWithLock(keyHex string, now time.Time) bool {
	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil || keyRecord.recordBody == nil || self.keptKeyHexes[keyHex] {
		return false
	}
	if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
		return false
	}
	return ExtenderRecordGated(keyRecord.recordBody) || keyRecord.connectDrawn
}

// A dial of `address` failed: it may arm its record, and it counts for nothing
// else.
func (self *ExtenderDirectory) blockEvidenceFailedWithLock(address *extenderDirectoryAddress, now time.Time) {
	state := &self.blockEvidence
	if !state.settings.Enabled || address.publicKeyHex == "" || address.source == ExtenderSourceManual {
		return
	}
	keyHex := address.publicKeyHex
	if _, ok := state.suspects[keyHex]; ok {
		return
	}
	if extenderBefore(now, state.quietUntilTimes[keyHex]) {
		return
	}
	if !self.blockEvidenceEligibleWithLock(keyHex, now) {
		return
	}
	if state.settings.MaxSuspectCount <= len(state.suspects) && !self.blockEvidenceEvictWithLock(keyHex) {
		return
	}
	if state.suspects == nil {
		state.suspects = map[string]*extenderBlockSuspect{}
	}
	state.nextSerial += 1
	state.suspects[keyHex] = &extenderBlockSuspect{
		serial:          state.nextSerial,
		armTime:         now,
		settleUntilTime: now.Add(state.settings.AttemptSettleTimeout),
	}
	self.blockEvidenceMonitorWithLock().NotifyAll()
}

// Makes room for a suspect of `keyHex`: a gated record replaces the newest open
// suspect. Reports whether there is room.
func (self *ExtenderDirectory) blockEvidenceEvictWithLock(keyHex string) bool {
	if !ExtenderRecordGated(self.keyHexRecords[keyHex].recordBody) {
		return false
	}
	state := &self.blockEvidence
	evictKeyHex := ""
	for _, suspectKeyHex := range slices.Sorted(maps.Keys(state.suspects)) {
		keyRecord := self.keyHexRecords[suspectKeyHex]
		if keyRecord != nil && ExtenderRecordGated(keyRecord.recordBody) {
			continue
		}
		if evictKeyHex == "" || state.suspects[evictKeyHex].armTime.Before(state.suspects[suspectKeyHex].armTime) {
			evictKeyHex = suspectKeyHex
		}
	}
	if evictKeyHex == "" {
		return false
	}
	delete(state.suspects, evictKeyHex)
	return true
}

// An address of a record answered a dial of this client, whichever dial
// (rule 5). A record with no suspect is not looked at further.
func (self *ExtenderDirectory) blockEvidenceAnsweredWithLock(keyHex string, now time.Time) {
	state := &self.blockEvidence
	if !state.settings.Enabled || keyHex == "" {
		return
	}
	if _, ok := state.suspects[keyHex]; !ok {
		return
	}
	delete(state.suspects, keyHex)
	self.blockEvidenceQuietWithLock(keyHex, now.Add(state.settings.AnsweredQuietTimeout))
}

func (self *ExtenderDirectory) blockEvidenceQuietWithLock(keyHex string, quietUntilTime time.Time) {
	state := &self.blockEvidence
	if state.quietUntilTimes == nil {
		state.quietUntilTimes = map[string]time.Time{}
	}
	state.quietUntilTimes[keyHex] = quietUntilTime
	if len(state.quietUntilTimes) <= extenderBlockMaxQuietCount {
		return
	}
	oldestKeyHex := ""
	for _, quietKeyHex := range slices.Sorted(maps.Keys(state.quietUntilTimes)) {
		if oldestKeyHex == "" || state.quietUntilTimes[quietKeyHex].Before(state.quietUntilTimes[oldestKeyHex]) {
			oldestKeyHex = quietKeyHex
		}
	}
	delete(state.quietUntilTimes, oldestKeyHex)
}

// The connection path drew this candidate, which is a record the client uses
// (rule 1).
func (self *ExtenderDirectory) blockEvidenceDrawnWithLock(candidate *ExtenderCandidate) {
	if !self.blockEvidence.settings.Enabled || len(candidate.PublicKey) == 0 {
		return
	}
	if keyRecord := self.keyHexRecords[hex.EncodeToString(candidate.PublicKey)]; keyRecord != nil {
		keyRecord.connectDrawn = true
	}
}

// What one attempt of a record dials: every address its current record names in
// `ipVersions`, each as a candidate (rule 2). The second value names that set
// of addresses, since evidence about other addresses is not evidence about
// these. Reports false when there is nothing the rule can judge: the record is
// not current, it names no address of the families, one of them is gone or
// another key's or manual, or one has no tcp carrier to compare with the
// operator's probe.
func (self *ExtenderDirectory) blockSuspectCandidatesWithLock(
	keyHex string,
	ipVersions []int,
	now time.Time,
) ([]*ExtenderCandidate, string, bool) {
	keyRecord := self.keyHexRecords[keyHex]
	if len(ipVersions) == 0 || keyRecord == nil || keyRecord.recordBody == nil {
		return nil, "", false
	}
	if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
		return nil, "", false
	}
	candidates := []*ExtenderCandidate{}
	addressNames := []string{}
	dialCount := 0
	for _, recordAddress := range keyRecord.recordBody.Addresses {
		ip, err := netip.ParseAddr(recordAddress.Ip)
		if err != nil {
			continue
		}
		ip = ip.Unmap()
		if !slices.Contains(ipVersions, addressIpVersion(ip)) || slices.Contains(addressNames, ip.String()) {
			continue
		}
		address := self.ipAddresses[ip]
		if address == nil || address.publicKeyHex != keyHex || address.source == ExtenderSourceManual {
			return nil, "", false
		}
		candidate := self.candidateWithLock(address, now)
		if !slices.Contains(candidate.Carriers, ExtenderCarrierTcp) {
			return nil, "", false
		}
		dialCount += len(candidate.Carriers)
		candidates = append(candidates, candidate)
		addressNames = append(addressNames, ip.String())
	}
	if len(candidates) == 0 || extenderBlockMaxAttemptDialCount < dialCount {
		return nil, "", false
	}
	slices.Sort(addressNames)
	return candidates, strings.Join(addressNames, ","), true
}

// What the evidence asks for at `now`, for a client that dials `ipVersions`.
// Suspects the rule can no longer judge are dropped here.
func (self *ExtenderDirectory) BlockEvidenceLook(ipVersions []int) ExtenderBlockEvidenceLook {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	state := &self.blockEvidence
	look := ExtenderBlockEvidenceLook{}
	if !state.settings.Enabled {
		return look
	}
	dueKeyHexes := []string{}
	attemptKeyHexes := []string{}
	addressKeys := map[string]string{}
	for keyHex, suspect := range state.suspects {
		_, addressKey, ok := self.blockSuspectCandidatesWithLock(keyHex, ipVersions, now)
		if !ok || !self.blockEvidenceEligibleWithLock(keyHex, now) ||
			state.settings.MaxWindow <= extenderElapsed(now, suspect.armTime) {
			delete(state.suspects, keyHex)
			continue
		}
		addressKeys[keyHex] = addressKey
		if state.settings.MinAttemptCount <= suspect.attemptCount && suspect.addressKey == addressKey &&
			extenderElapsed(now, suspect.lastAttemptTime) <= state.settings.ReportFreshTimeout {
			dueKeyHexes = append(dueKeyHexes, keyHex)
			continue
		}
		attemptTime := suspect.settleUntilTime
		if 0 < suspect.attemptCount {
			if nextTime := suspect.lastAttemptTime.Add(state.settings.AttemptInterval); attemptTime.Before(nextTime) {
				attemptTime = nextTime
			}
		}
		if look.NextAttemptTime.IsZero() || attemptTime.Before(look.NextAttemptTime) {
			look.NextAttemptTime = attemptTime
		}
		if !extenderBefore(now, attemptTime) {
			attemptKeyHexes = append(attemptKeyHexes, keyHex)
		}
	}
	look.SuspectCount = len(state.suspects)
	compare := func(a string, b string) int {
		aGated := ExtenderRecordGated(self.keyHexRecords[a].recordBody)
		bGated := ExtenderRecordGated(self.keyHexRecords[b].recordBody)
		if aGated != bGated {
			if aGated {
				return -1
			}
			return 1
		}
		if c := state.suspects[a].armTime.Compare(state.suspects[b].armTime); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	}
	slices.SortFunc(dueKeyHexes, compare)
	slices.SortFunc(attemptKeyHexes, compare)
	for _, keyHex := range dueKeyHexes {
		look.DuePublicKeys = append(look.DuePublicKeys, slices.Clone(self.keyHexRecords[keyHex].publicKey))
	}
	for _, keyHex := range attemptKeyHexes {
		if state.settings.MaxAttemptSuspectCount <= len(look.AttemptSuspects) {
			break
		}
		look.AttemptSuspects = append(look.AttemptSuspects, ExtenderBlockSuspect{
			PublicKey:  slices.Clone(self.keyHexRecords[keyHex].publicKey),
			keyHex:     keyHex,
			serial:     state.suspects[keyHex].serial,
			addressKey: addressKeys[keyHex],
			ipVersions: slices.Clone(ipVersions),
		})
	}
	return look
}

// One attempt of one suspect (rule 2): every address, every carrier, tcp first,
// each with its own budget, until one answers. `probe` is one dial, nil an
// answer; the sdk's is ProbeExtenderLatency with no attestor, so the dial
// identifies no client. Nothing here touches a hold or a count, so an attempt
// moves nothing the candidate order reads. An answer drops the suspect at once;
// a failed attempt counts only once the caller has its proof of reach, and
// RecordBlockAttempts is told.
func (self *ExtenderDirectory) AttemptBlockSuspect(
	ctx context.Context,
	suspect ExtenderBlockSuspect,
	probe func(ctx context.Context, extenderConfig *ExtenderConfig) error,
) string {
	now := self.settings.Now()
	candidates, probeTimeout, ok := func() ([]*ExtenderCandidate, time.Duration, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		state := &self.blockEvidence
		stored := state.suspects[suspect.keyHex]
		if !state.settings.Enabled || stored == nil || stored.serial != suspect.serial {
			return nil, 0, false
		}
		candidates, addressKey, ok := self.blockSuspectCandidatesWithLock(suspect.keyHex, suspect.ipVersions, now)
		return candidates, state.settings.ProbeTimeout, ok && addressKey == suspect.addressKey
	}()
	if !ok {
		return ExtenderBlockAttemptAborted
	}

	// the directory is an external object; it is read with no lock held
	spoofDomains, _ := directorySpoofDomains(self)
	borrowCountryCode := directoryBorrowCountryCode(self)
	dialCount := 0
	for _, candidate := range candidates {
		for _, carrier := range orderedExtenderCarriers(candidate.Carriers) {
			connectMode, ok := ExtenderConnectModeForCarrier(carrier)
			if !ok {
				continue
			}
			extenderConfig := extenderFeedConfig(candidate, connectMode, spoofDomains, borrowCountryCode)
			if extenderConfig == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return ExtenderBlockAttemptAborted
			default:
			}
			dialCount += 1
			err := func() error {
				probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
				defer cancel()
				return probe(probeCtx, extenderConfig)
			}()
			var limitedErr *ExtenderLimitedError
			if err == nil || errors.As(err, &limitedErr) {
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					self.blockEvidenceAnsweredWithLock(suspect.keyHex, self.settings.Now())
				}()
				return ExtenderBlockAttemptAnswered
			}
			// a dial the caller ended, or this host's memory budget refused, says
			// nothing about the extender
			select {
			case <-ctx.Done():
				return ExtenderBlockAttemptAborted
			default:
			}
			if IsExtenderMemoryBudgetError(err) {
				return ExtenderBlockAttemptAborted
			}
		}
	}
	if dialCount == 0 {
		return ExtenderBlockAttemptAborted
	}
	return ExtenderBlockAttemptFailed
}

// The attempts of a pass that began at `startTime` failed, and the control
// route answered after the last of them (rule 3). A suspect dropped or armed
// again since is not this one, and an attempt too soon after the last does not
// count. An attempt on other addresses than the ones counted so far starts the
// count over: a routine endpoint change carries no evidence across.
func (self *ExtenderDirectory) RecordBlockAttempts(startTime time.Time, suspects []ExtenderBlockSuspect) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	state := &self.blockEvidence
	if !state.settings.Enabled {
		return
	}
	for _, suspect := range suspects {
		stored := state.suspects[suspect.keyHex]
		if stored == nil || stored.serial != suspect.serial {
			continue
		}
		if stored.addressKey != suspect.addressKey {
			stored.addressKey = suspect.addressKey
			stored.attemptCount = 0
		}
		if 0 < stored.attemptCount && extenderElapsed(startTime, stored.lastAttemptTime) < state.settings.AttemptInterval {
			continue
		}
		stored.attemptCount += 1
		stored.lastAttemptTime = startTime
	}
}

// The operator answered a report of `publicKey`, whatever it said. The record
// is left alone for ReportedQuietTimeout.
func (self *ExtenderDirectory) BlockEvidenceReported(publicKey []byte) {
	keyHex := hex.EncodeToString(publicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	state := &self.blockEvidence
	if !state.settings.Enabled {
		return
	}
	delete(state.suspects, keyHex)
	self.blockEvidenceQuietWithLock(keyHex, now.Add(state.settings.ReportedQuietTimeout))
}
