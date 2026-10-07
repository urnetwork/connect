package connect

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// The gated tier's release policy (EXTENDER.md Q3): what an authenticated
// identity is handed from the durable fleet, and how little.
//
// A release is deterministic in the identity and the epoch: the identity is
// placed in one partition of about sqrt(n) records for the life of the
// partition count (ExtenderPartitionMembers) and each epoch deals that
// partition in an order the identity alone gets (ExtenderPartitionOrder), of
// which it takes the first few that are eligible -- one while the identity is
// in probation, three after it, in the Lox shape. Asking again in the same
// epoch returns the same records, so a repeated request learns nothing, and a
// Sybil has to be many identities over many epochs, each rate limited and
// each spending its own partition. Two further caps hold whatever the
// partition: requests per identity and per vantage -- the requester's asn, or
// its prefix where no asn is known -- over a window, and distinct identities
// per record per country (Q4, Salmon's ten), so a long-lived extender is
// never the one everyone in a country is told about, which is what host
// profiling finds first. A record known blocked in the requester's country is
// skipped (ExtenderBlockedState).
//
// The policy reads and writes a ledger. ExtenderReleaseMemoryLedger is the
// in-process one, bounded, which an operator of one process and every test
// use; a replicated operator backs the same interface with its database. The
// policy holds no lock of its own: the ledger is what is shared.

// The defaults.
const (
	// An identity's epoch (Q3): the partition is dealt again this often, so
	// an identity learns at most its count per epoch. A week, since the
	// gated tier is the durable one and its records are refreshed on each
	// release rather than by a drip.
	ExtenderReleaseEpochTimeout = 7 * 24 * time.Hour
	// How long an identity is new (Lox: 30 days at level 0).
	ExtenderReleaseProbationTimeout = 30 * 24 * time.Hour
	// Records per release while new, and once trusted.
	ExtenderReleaseNewIdentityCount     = 1
	ExtenderReleaseTrustedIdentityCount = 3
	// Requests per identity, and per vantage, within the request window. A
	// release is one request a day in normal use, so the identity cap is a
	// client that retries; the vantage cap is generous because a vantage is
	// an asn with many users behind it, and what it bounds is a harvest run
	// from one network.
	ExtenderReleaseIdentityRequestLimit = 8
	ExtenderReleaseVantageRequestLimit  = 4096
	ExtenderReleaseRequestWindow        = time.Hour
	// Distinct identities one record is released to per country (Salmon's
	// ten), counted over the client window.
	ExtenderReleaseMaxClientsPerExtenderPerCountry = 10
	ExtenderReleaseClientWindow                    = 30 * 24 * time.Hour
)

var (
	ErrExtenderReleaseIdentityLimited = errors.New("the identity has reached its release request limit")
	ErrExtenderReleaseVantageLimited  = errors.New("the vantage has reached its release request limit")
	ErrExtenderReleaseLedgerFull      = errors.New("the release ledger is full")
)

// The policy numbers, all settings so a test pins every transition.
type ExtenderReleaseSettings struct {
	EpochTimeout                    time.Duration
	ProbationTimeout                time.Duration
	NewIdentityCount                int
	TrustedIdentityCount            int
	IdentityRequestLimit            int
	VantageRequestLimit             int
	RequestWindow                   time.Duration
	MaxClientsPerExtenderPerCountry int
	ClientWindow                    time.Duration

	// The only clock the policy reads. Tests install a fake one.
	Now func() time.Time
}

func DefaultExtenderReleaseSettings() *ExtenderReleaseSettings {
	return &ExtenderReleaseSettings{
		EpochTimeout:                    ExtenderReleaseEpochTimeout,
		ProbationTimeout:                ExtenderReleaseProbationTimeout,
		NewIdentityCount:                ExtenderReleaseNewIdentityCount,
		TrustedIdentityCount:            ExtenderReleaseTrustedIdentityCount,
		IdentityRequestLimit:            ExtenderReleaseIdentityRequestLimit,
		VantageRequestLimit:             ExtenderReleaseVantageRequestLimit,
		RequestWindow:                   ExtenderReleaseRequestWindow,
		MaxClientsPerExtenderPerCountry: ExtenderReleaseMaxClientsPerExtenderPerCountry,
		ClientWindow:                    ExtenderReleaseClientWindow,
		Now:                             time.Now,
	}
}

// Serializes one admission across every identity, vantage and country/key it
// touches. Replicas must share this transaction boundary in their durable store.
type ExtenderReleaseLedger interface {
	Transact(identity []byte, vantage string, countryCode string, keyHexes []string, apply func(ExtenderReleaseLedgerTx))
	IssuedKeyHexes(identity []byte, epoch uint64) ([]string, error)
}

// A transaction-local ledger view. Counts, reservations and epoch history are
// read and written together; the view must not escape its callback.
type ExtenderReleaseLedgerTx interface {
	// Requests by the identity, and by the vantage, at or after `since`.
	RequestCounts(identity []byte, vantage string, since time.Time) (identityCount int, vantageCount int)
	// Records one request, admitted or refused: a refused request counts, so
	// a client that hammers a closed gate does not open it by trying.
	RecordRequest(identity []byte, vantage string, now time.Time)
	// The distinct identities but `identity` the record was released to in
	// the country at or after `since`.
	ClientCount(keyHex string, countryCode string, identity []byte, since time.Time) int
	// Records one release of a record to an identity in a country.
	RecordRelease(identity []byte, keyHex string, countryCode string, epoch uint64, now time.Time)
	// Every key already disclosed to the identity in this epoch, including
	// keys that have since become unavailable, blocked or ineligible.
	IssuedKeyHexes(identity []byte, epoch uint64) ([]string, error)
}

// Where blocked state comes from (Q4). Nil skips nothing.
type ExtenderReleaseBlockedSource interface {
	Blocked(keyHex string, countryCode string) bool
}

// A durable source can evaluate the bounded candidate set in one query.
type ExtenderReleaseBlockedBatchSource interface {
	BlockedKeys(keyHexes []string, countryCode string) map[string]bool
}

// One release request.
type ExtenderReleaseRequest struct {
	// The authenticated identity: the account and the device together, as
	// bytes. The partition and the epoch's order are keyed by it.
	Identity []byte
	// When the identity was created, which decides probation. Zero is new.
	IdentityCreateTime time.Time
	// The requester's asn as text, or its prefix (ExtenderVantagePrefix)
	// where none is known: what the per-vantage limit counts by.
	Vantage string
	// The requester's country, lower case; empty is a country of its own.
	CountryCode string
	// When set, which records may be released to this requester at all --
	// the family it can dial, for one. Nil is every record.
	Eligible func(keyHex string) bool
}

// One release.
type ExtenderReleaseResult struct {
	// The released records' keys, in the epoch's order.
	KeyHexes []string
	// The epoch the release was dealt in.
	Epoch uint64
	// The identity's partition and the count of partitions.
	Partition      int
	PartitionCount int
	// How many the identity was entitled to, which is more than it got when
	// its partition ran short of eligible records.
	Count int
	// Whether the identity is still new.
	Probation bool
}

// The release policy, keyed by the operator's secret.
type ExtenderReleasePolicy struct {
	secret   []byte
	settings *ExtenderReleaseSettings
	ledger   ExtenderReleaseLedger
	blocked  ExtenderReleaseBlockedSource
}

func NewExtenderReleasePolicy(
	secret []byte,
	ledger ExtenderReleaseLedger,
	blocked ExtenderReleaseBlockedSource,
	settings *ExtenderReleaseSettings,
) *ExtenderReleasePolicy {
	if settings == nil {
		settings = DefaultExtenderReleaseSettings()
	}
	if settings.Now == nil {
		copied := *settings
		copied.Now = time.Now
		settings = &copied
	}
	return &ExtenderReleasePolicy{
		secret:   slices.Clone(secret),
		settings: settings,
		ledger:   ledger,
		blocked:  blocked,
	}
}

// Releases to one identity from the gated fleet `keyHexes`, in any order,
// repeats included. The error is the request limit that refused it; a
// request that is admitted but finds nothing eligible is an empty release,
// not an error.
func (self *ExtenderReleasePolicy) Release(
	request *ExtenderReleaseRequest,
	keyHexes []string,
) (*ExtenderReleaseResult, error) {
	if request == nil {
		return nil, fmt.Errorf("extender release request is missing")
	}
	if len(request.Identity) == 0 {
		return nil, fmt.Errorf("extender release request has no identity")
	}
	now := self.settings.Now()
	countryCode := strings.ToLower(strings.TrimSpace(request.CountryCode))

	probation := request.IdentityCreateTime.IsZero() ||
		now.Sub(request.IdentityCreateTime) < self.settings.ProbationTimeout
	count := self.settings.TrustedIdentityCount
	if probation {
		count = self.settings.NewIdentityCount
	}
	epoch := ExtenderEpoch(now, self.settings.EpochTimeout)
	members, partition, partitionCount := ExtenderPartitionMembers(
		self.secret,
		ExtenderChannelGated,
		request.Identity,
		keyHexes,
	)
	result := &ExtenderReleaseResult{
		KeyHexes:       []string{},
		Epoch:          epoch,
		Partition:      partition,
		PartitionCount: partitionCount,
		Count:          count,
		Probation:      probation,
	}
	// External eligibility and blocked-state reads happen before taking the
	// ledger transaction. The transaction owns only admission and accounting.
	// The snapshot bounds those reads to one partition plus prior disclosures.
	// A concurrent disclosure is re-read in the transaction; an unavailable
	// snapshot can only withhold that record until the next request.
	previousKeyHexes, snapshotErr := self.ledger.IssuedKeyHexes(request.Identity, epoch)
	candidateKeyHexes := append(slices.Clone(members), previousKeyHexes...)
	slices.Sort(candidateKeyHexes)
	candidateKeyHexes = slices.Compact(candidateKeyHexes)
	availableKeyHexes := map[string]bool{}
	for _, keyHex := range keyHexes {
		availableKeyHexes[strings.ToLower(keyHex)] = true
	}
	eligibleKeyHexes := map[string]bool{}
	var blockedKeyHexes map[string]bool
	batchBlocked, batch := self.blocked.(ExtenderReleaseBlockedBatchSource)
	if batch {
		blockedKeyHexes = batchBlocked.BlockedKeys(candidateKeyHexes, countryCode)
	}
	for _, keyHex := range candidateKeyHexes {
		keyHex = strings.ToLower(keyHex)
		if !availableKeyHexes[keyHex] {
			continue
		}
		if request.Eligible != nil && !request.Eligible(keyHex) {
			continue
		}
		if blockedKeyHexes[keyHex] || (!batch && self.blocked != nil && self.blocked.Blocked(keyHex, countryCode)) {
			continue
		}
		eligibleKeyHexes[keyHex] = true
	}
	var releaseErr error
	self.ledger.Transact(request.Identity, request.Vantage, countryCode, candidateKeyHexes, func(ledger ExtenderReleaseLedgerTx) {
		// Durable transactions may retry after a serialization/commit error.
		result.KeyHexes = result.KeyHexes[:0]
		releaseErr = nil
		identityCount, vantageCount := ledger.RequestCounts(request.Identity, request.Vantage, now.Add(-self.settings.RequestWindow))
		// Refusals commit too, so retrying cannot reopen a closed gate.
		ledger.RecordRequest(request.Identity, request.Vantage, now)
		if 0 < self.settings.IdentityRequestLimit && self.settings.IdentityRequestLimit <= identityCount {
			releaseErr = ErrExtenderReleaseIdentityLimited
			return
		}
		if 0 < self.settings.VantageRequestLimit && self.settings.VantageRequestLimit <= vantageCount {
			releaseErr = ErrExtenderReleaseVantageLimited
			return
		}
		if snapshotErr != nil {
			releaseErr = snapshotErr
			return
		}
		issuedKeyHexes, err := ledger.IssuedKeyHexes(request.Identity, epoch)
		if err != nil {
			releaseErr = err
			return
		}
		orderedKeyHexes := append(slices.Clone(issuedKeyHexes), ExtenderPartitionOrder(self.secret, ExtenderChannelGated, request.Identity, epoch, members)...)
		for _, keyHex := range orderedKeyHexes {
			if count <= len(result.KeyHexes) {
				break
			}
			if !eligibleKeyHexes[keyHex] || slices.Contains(result.KeyHexes, keyHex) {
				continue
			}
			issued := slices.Contains(issuedKeyHexes, keyHex)
			if !issued && count <= len(issuedKeyHexes) {
				continue
			}
			if 0 < self.settings.MaxClientsPerExtenderPerCountry && self.settings.MaxClientsPerExtenderPerCountry <= ledger.ClientCount(keyHex, countryCode, request.Identity, now.Add(-self.settings.ClientWindow)) {
				continue
			}
			ledger.RecordRelease(request.Identity, keyHex, countryCode, epoch, now)
			result.KeyHexes = append(result.KeyHexes, keyHex)
			if !issued {
				issuedKeyHexes = append(issuedKeyHexes, keyHex)
			}
		}
	})
	if releaseErr != nil {
		return nil, releaseErr
	}
	return result, nil
}

// The in-process ledger: bounded tables under one lock, pruned to their
// windows as they are read. Beyond its bounds the identity, vantage or record
// touched longest ago is forgotten, which is the oldest evidence and the
// cheapest to lose; a vantage forgotten starts its count again, so the bound
// is set well above what one process serves.
type ExtenderReleaseMemoryLedger struct {
	extenderReleaseMemoryTx
	stateLock sync.Mutex
}

// The in-memory transaction view is protected by its owner's stateLock.
type extenderReleaseMemoryTx struct {
	// identities and vantages tracked at most, and the request window the
	// tables are pruned to, which is the longest window a policy reads
	maxIdentityCount int
	maxVantageCount  int
	maxRecordCount   int
	requestWindow    time.Duration
	clientWindow     time.Duration

	// by identity hex: request times, oldest first
	identityRequestTimes map[string][]time.Time
	// by vantage: request times, oldest first
	vantageRequestTimes map[string][]time.Time
	// by key hex, by country, by identity hex: the last release time
	releaseTimes map[string]map[string]map[string]time.Time
	issuedKeys   map[string]*extenderReleaseEpochKeys
}

// Disclosure history outlives a record's eligibility and country changes.
type extenderReleaseEpochKeys struct {
	epoch    uint64
	keyHexes []string
}

// The ledger's own bounds, chosen for a process serving one operator.
const (
	ExtenderReleaseLedgerMaxIdentityCount = 65536
	ExtenderReleaseLedgerMaxVantageCount  = 65536
	ExtenderReleaseLedgerMaxRecordCount   = 65536
)

// The windows are those of the policy the ledger serves, so what is pruned
// is never what the policy would still read.
func NewExtenderReleaseMemoryLedger(requestWindow time.Duration, clientWindow time.Duration) *ExtenderReleaseMemoryLedger {
	return &ExtenderReleaseMemoryLedger{
		extenderReleaseMemoryTx: extenderReleaseMemoryTx{
			maxIdentityCount:     ExtenderReleaseLedgerMaxIdentityCount,
			maxVantageCount:      ExtenderReleaseLedgerMaxVantageCount,
			maxRecordCount:       ExtenderReleaseLedgerMaxRecordCount,
			requestWindow:        requestWindow,
			clientWindow:         clientWindow,
			identityRequestTimes: map[string][]time.Time{},
			vantageRequestTimes:  map[string][]time.Time{},
			releaseTimes:         map[string]map[string]map[string]time.Time{},
			issuedKeys:           map[string]*extenderReleaseEpochKeys{},
		},
	}
}

// The same lock covers the complete read/decide/write admission callback.
func (self *ExtenderReleaseMemoryLedger) Transact(_ []byte, _ string, _ string, _ []string, apply func(ExtenderReleaseLedgerTx)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	apply(&self.extenderReleaseMemoryTx)
}

// Returns a bounded eligibility snapshot; admission rechecks it in Transact.
func (self *ExtenderReleaseMemoryLedger) IssuedKeyHexes(identity []byte, epoch uint64) ([]string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.extenderReleaseMemoryTx.IssuedKeyHexes(identity, epoch)
}

// Active epoch disclosures are never evicted to admit another identity.
func (self *extenderReleaseMemoryTx) IssuedKeyHexes(identity []byte, epoch uint64) ([]string, error) {
	identityHex := hex.EncodeToString(identity)
	for key, entry := range self.issuedKeys {
		if entry.epoch < epoch {
			delete(self.issuedKeys, key)
		}
	}
	if entry := self.issuedKeys[identityHex]; entry != nil && entry.epoch == epoch {
		return slices.Clone(entry.keyHexes), nil
	}
	if 0 < self.maxIdentityCount && self.maxIdentityCount <= len(self.issuedKeys) {
		return nil, ErrExtenderReleaseLedgerFull
	}
	return nil, nil
}

// Replaces the table bounds, for a test that fills them.
func (self *ExtenderReleaseMemoryLedger) SetBounds(maxIdentityCount int, maxVantageCount int, maxRecordCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.maxIdentityCount = maxIdentityCount
	self.maxVantageCount = maxVantageCount
	self.maxRecordCount = maxRecordCount
}

func (self *ExtenderReleaseMemoryLedger) RequestCounts(identity []byte, vantage string, since time.Time) (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.extenderReleaseMemoryTx.RequestCounts(identity, vantage, since)
}

// Counts request evidence while the admission transaction owns the lock.
func (self *extenderReleaseMemoryTx) RequestCounts(identity []byte, vantage string, since time.Time) (int, int) {
	identityHex := hex.EncodeToString(identity)

	count := func(times []time.Time) int {
		n := 0
		for _, t := range times {
			if !t.Before(since) {
				n += 1
			}
		}
		return n
	}
	return count(self.identityRequestTimes[identityHex]), count(self.vantageRequestTimes[vantage])
}

func (self *ExtenderReleaseMemoryLedger) RecordRequest(identity []byte, vantage string, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.extenderReleaseMemoryTx.RecordRequest(identity, vantage, now)
}

// Appends a request within the admission transaction.
func (self *extenderReleaseMemoryTx) RecordRequest(identity []byte, vantage string, now time.Time) {
	identityHex := hex.EncodeToString(identity)

	windowStart := now.Add(-self.requestWindow)
	record := func(times map[string][]time.Time, key string, maxCount int) {
		if _, ok := times[key]; !ok && maxCount <= len(times) {
			extenderReleaseEvictOldestWithLock(times, windowStart)
		}
		kept := times[key][:0]
		for _, t := range times[key] {
			if !t.Before(windowStart) {
				kept = append(kept, t)
			}
		}
		times[key] = append(kept, now)
	}
	record(self.identityRequestTimes, identityHex, self.maxIdentityCount)
	record(self.vantageRequestTimes, vantage, self.maxVantageCount)
}

// Makes room in a request table: everything whose newest request is before
// `windowStart` goes, and when nothing is, the one whose newest request is
// oldest.
func extenderReleaseEvictOldestWithLock(times map[string][]time.Time, windowStart time.Time) {
	oldestKey := ""
	var oldestTime time.Time
	for key, keyTimes := range times {
		newest := time.Time{}
		if 0 < len(keyTimes) {
			newest = keyTimes[len(keyTimes)-1]
		}
		if newest.Before(windowStart) {
			delete(times, key)
			continue
		}
		if oldestKey == "" || newest.Before(oldestTime) {
			oldestKey = key
			oldestTime = newest
		}
	}
	if len(times) == 0 {
		return
	}
	if oldestKey != "" {
		delete(times, oldestKey)
	}
}

func (self *ExtenderReleaseMemoryLedger) ClientCount(keyHex string, countryCode string, identity []byte, since time.Time) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.extenderReleaseMemoryTx.ClientCount(keyHex, countryCode, identity, since)
}

// Counts other identities while reservations are excluded.
func (self *extenderReleaseMemoryTx) ClientCount(keyHex string, countryCode string, identity []byte, since time.Time) int {
	identityHex := hex.EncodeToString(identity)

	count := 0
	for otherIdentityHex, releaseTime := range self.releaseTimes[strings.ToLower(keyHex)][countryCode] {
		if otherIdentityHex == identityHex || releaseTime.Before(since) {
			continue
		}
		count += 1
	}
	return count
}

func (self *ExtenderReleaseMemoryLedger) RecordRelease(identity []byte, keyHex string, countryCode string, epoch uint64, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.extenderReleaseMemoryTx.RecordRelease(identity, keyHex, countryCode, epoch, now)
}

// Reserves country capacity and permanently charges the epoch disclosure.
func (self *extenderReleaseMemoryTx) RecordRelease(identity []byte, keyHex string, countryCode string, epoch uint64, now time.Time) {
	identityHex := hex.EncodeToString(identity)
	keyHex = strings.ToLower(keyHex)
	issued := self.issuedKeys[identityHex]
	if issued == nil || issued.epoch != epoch {
		issued = &extenderReleaseEpochKeys{epoch: epoch}
		self.issuedKeys[identityHex] = issued
	}
	if !slices.Contains(issued.keyHexes, keyHex) {
		issued.keyHexes = append(issued.keyHexes, keyHex)
	}

	windowStart := now.Add(-self.clientWindow)
	countryReleaseTimes, ok := self.releaseTimes[keyHex]
	if !ok {
		if self.maxRecordCount <= len(self.releaseTimes) {
			// the record whose newest release is oldest goes
			oldestKeyHex := ""
			var oldestTime time.Time
			for otherKeyHex, otherCountryReleaseTimes := range self.releaseTimes {
				newest := time.Time{}
				for _, identityReleaseTimes := range otherCountryReleaseTimes {
					for _, t := range identityReleaseTimes {
						if newest.Before(t) {
							newest = t
						}
					}
				}
				if oldestKeyHex == "" || newest.Before(oldestTime) {
					oldestKeyHex = otherKeyHex
					oldestTime = newest
				}
			}
			delete(self.releaseTimes, oldestKeyHex)
		}
		countryReleaseTimes = map[string]map[string]time.Time{}
		self.releaseTimes[keyHex] = countryReleaseTimes
	}
	identityReleaseTimes, ok := countryReleaseTimes[countryCode]
	if !ok {
		identityReleaseTimes = map[string]time.Time{}
		countryReleaseTimes[countryCode] = identityReleaseTimes
	}
	for otherIdentityHex, t := range identityReleaseTimes {
		if t.Before(windowStart) {
			delete(identityReleaseTimes, otherIdentityHex)
		}
	}
	identityReleaseTimes[identityHex] = now
}
