package connect

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Canaries and blocked state (EXTENDER.md Q4): how a leak is attributed and
// how a block is known.
//
// A canary is an operator-run extender that appears on exactly one channel in
// exactly one place -- a dns canary in one region's sets and nowhere else, a
// gated canary in the gated partition its key hashes to and in no open
// channel -- and looks like any other record there. A censor that blocks it
// learned it from that one place, so a blocked canary names the channel and
// the partition or region that leaked, and nothing else it could have been.
// The placement is the operator's knowledge alone: nothing on the wire marks
// a canary.
//
// Blocked state is per country, because a censor is one country and a record
// blocked there answers everywhere else. A record is blocked in a country when
// enough distinct identities there reported it unreachable within the window
// while the operator's own probe, from outside, still reached it: a report
// against an extender that is simply down is not a block, and one reporter,
// however loud, is not enough. The state is bounded in reporters and entries.
//
// ExtenderBlockedState is safe for concurrent use; the placement functions
// are pure.

// A canary and the one place it is published.
type ExtenderCanary struct {
	KeyHex string
	// ExtenderChannelDns or ExtenderChannelGated
	Channel string
	// The region of a dns canary: the continent code of its sets, upper
	// case, empty for the default sets. Ignored for a gated canary, whose
	// place is its partition.
	Region string
}

// Where a canary is: the channel, and the partition of a gated canary or the
// region of a dns canary.
type ExtenderCanaryPlacement struct {
	Channel string
	// the gated partition; -1 on the dns channel
	Partition int
	// the dns region; empty on the gated channel
	Region string
}

func (self ExtenderCanaryPlacement) String() string {
	switch self.Channel {
	case ExtenderChannelDns:
		region := self.Region
		if region == "" {
			region = "default"
		}
		return fmt.Sprintf("%s:%s", self.Channel, region)
	default:
		return fmt.Sprintf("%s:%d", self.Channel, self.Partition)
	}
}

// Places one canary (Q4). `gatedPartitionCount` is the partition count of the
// gated fleet (ExtenderPartitionCount), which a gated canary's partition is
// taken against. A canary with no channel is placed nowhere.
func ExtenderCanaryPlace(secret []byte, canary *ExtenderCanary, gatedPartitionCount int) (ExtenderCanaryPlacement, bool) {
	if canary == nil {
		return ExtenderCanaryPlacement{}, false
	}
	switch canary.Channel {
	case ExtenderChannelDns:
		return ExtenderCanaryPlacement{
			Channel:   ExtenderChannelDns,
			Partition: -1,
			Region:    strings.ToUpper(strings.TrimSpace(canary.Region)),
		}, true
	case ExtenderChannelGated:
		return ExtenderCanaryPlacement{
			Channel:   ExtenderChannelGated,
			Partition: ExtenderRecordPartition(secret, ExtenderChannelGated, canary.KeyHex, gatedPartitionCount),
		}, true
	default:
		return ExtenderCanaryPlacement{}, false
	}
}

// The places that leaked: the placement of every canary among
// `blockedKeyHexes`, each place once, in channel then place order. A blocked
// record that is not a canary attributes nothing.
func ExtenderAttributeBlockedCanaries(
	secret []byte,
	canaries []*ExtenderCanary,
	blockedKeyHexes []string,
	gatedPartitionCount int,
) []ExtenderCanaryPlacement {
	blocked := map[string]bool{}
	for _, keyHex := range blockedKeyHexes {
		blocked[strings.ToLower(keyHex)] = true
	}
	placements := []ExtenderCanaryPlacement{}
	for _, canary := range canaries {
		if canary == nil || !blocked[strings.ToLower(canary.KeyHex)] {
			continue
		}
		placement, ok := ExtenderCanaryPlace(secret, canary, gatedPartitionCount)
		if !ok || slices.Contains(placements, placement) {
			continue
		}
		placements = append(placements, placement)
	}
	slices.SortFunc(placements, func(a ExtenderCanaryPlacement, b ExtenderCanaryPlacement) int {
		if c := strings.Compare(a.Channel, b.Channel); c != 0 {
			return c
		}
		if a.Partition != b.Partition {
			return a.Partition - b.Partition
		}
		return strings.Compare(a.Region, b.Region)
	})
	return placements
}

// The blocked-state numbers, all settings.
type ExtenderBlockedStateSettings struct {
	// Distinct reporters in a country within the report window that make a
	// block. Three: one is a bad network and two may be one person.
	ReportThreshold int
	ReportWindow    time.Duration
	// How recently the operator's probe must have reached the record for a
	// block to stand rather than an outage. The uptime probe runs every five
	// minutes and revokes after six failures, so an hour covers a record
	// that is still active.
	ProbeWindow time.Duration
	// Reporters remembered per record and country, and record-country
	// entries remembered at most. Beyond either the oldest goes.
	MaxReporterCount int
	MaxEntryCount    int

	// The only clock the state reads. Tests install a fake one.
	Now func() time.Time
}

func DefaultExtenderBlockedStateSettings() *ExtenderBlockedStateSettings {
	return &ExtenderBlockedStateSettings{
		ReportThreshold:  3,
		ReportWindow:     24 * time.Hour,
		ProbeWindow:      time.Hour,
		MaxReporterCount: 64,
		MaxEntryCount:    65536,
		Now:              time.Now,
	}
}

// The reports against one record from one country.
type extenderBlockedEntry struct {
	// by reporter hex, the last report time
	reporterTimes map[string]time.Time
	lastTime      time.Time
}

// Per-country blocked state (Q4).
type ExtenderBlockedState struct {
	settings *ExtenderBlockedStateSettings

	stateLock sync.Mutex
	// by key hex, by country
	entries map[string]map[string]*extenderBlockedEntry
	// how many record-country entries there are
	entryCount int
	// by key hex, the operator's last successful probe
	probeSuccessTimes map[string]time.Time
}

func NewExtenderBlockedState(settings *ExtenderBlockedStateSettings) *ExtenderBlockedState {
	if settings == nil {
		settings = DefaultExtenderBlockedStateSettings()
	}
	if settings.Now == nil {
		copied := *settings
		copied.Now = time.Now
		settings = &copied
	}
	return &ExtenderBlockedState{
		settings:          settings,
		entries:           map[string]map[string]*extenderBlockedEntry{},
		probeSuccessTimes: map[string]time.Time{},
	}
}

// Records that `reporter`, in `countryCode`, could not reach the record. A
// reporter is counted once however often it reports; a report from an
// unknown country is kept under the empty country.
func (self *ExtenderBlockedState) Report(keyHex string, countryCode string, reporter []byte) {
	keyHex = strings.ToLower(keyHex)
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	reporterHex := hex.EncodeToString(reporter)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	countryEntries, ok := self.entries[keyHex]
	if !ok {
		countryEntries = map[string]*extenderBlockedEntry{}
		self.entries[keyHex] = countryEntries
	}
	entry, ok := countryEntries[countryCode]
	if !ok {
		if 0 < self.settings.MaxEntryCount && self.settings.MaxEntryCount <= self.entryCount {
			self.evictOldestEntryWithLock()
		}
		// Evicting this key's last country removes its outer map too.
		self.entries[keyHex] = countryEntries
		entry = &extenderBlockedEntry{
			reporterTimes: map[string]time.Time{},
		}
		countryEntries[countryCode] = entry
		self.entryCount += 1
	}
	self.pruneEntryWithLock(entry, now)
	if _, ok := entry.reporterTimes[reporterHex]; !ok &&
		0 < self.settings.MaxReporterCount &&
		self.settings.MaxReporterCount <= len(entry.reporterTimes) {
		oldestReporterHex := ""
		var oldestTime time.Time
		for otherReporterHex, t := range entry.reporterTimes {
			if oldestReporterHex == "" || t.Before(oldestTime) {
				oldestReporterHex = otherReporterHex
				oldestTime = t
			}
		}
		delete(entry.reporterTimes, oldestReporterHex)
	}
	entry.reporterTimes[reporterHex] = now
	entry.lastTime = now
}

// Drops the reports older than the window.
func (self *ExtenderBlockedState) pruneEntryWithLock(entry *extenderBlockedEntry, now time.Time) {
	windowStart := now.Add(-self.settings.ReportWindow)
	for reporterHex, t := range entry.reporterTimes {
		if t.Before(windowStart) {
			delete(entry.reporterTimes, reporterHex)
		}
	}
}

// Forgets the record-country entry reported least recently.
func (self *ExtenderBlockedState) evictOldestEntryWithLock() {
	oldestKeyHex := ""
	oldestCountryCode := ""
	var oldestTime time.Time
	for keyHex, countryEntries := range self.entries {
		for countryCode, entry := range countryEntries {
			if oldestKeyHex == "" || entry.lastTime.Before(oldestTime) {
				oldestKeyHex = keyHex
				oldestCountryCode = countryCode
				oldestTime = entry.lastTime
			}
		}
	}
	if oldestKeyHex == "" {
		return
	}
	delete(self.entries[oldestKeyHex], oldestCountryCode)
	if len(self.entries[oldestKeyHex]) == 0 {
		delete(self.entries, oldestKeyHex)
	}
	self.entryCount -= 1
}

// Records that the operator's own probe reached the record now.
func (self *ExtenderBlockedState) ProbeSucceeded(keyHex string) {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.probeSuccessTimes[strings.ToLower(keyHex)] = now
}

// Whether the record is blocked in the country now: reporters at or above
// the threshold within the window, and a probe success within the probe
// window.
func (self *ExtenderBlockedState) Blocked(keyHex string, countryCode string) bool {
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.blockedWithLock(strings.ToLower(keyHex), strings.ToLower(strings.TrimSpace(countryCode)), now)
}

func (self *ExtenderBlockedState) blockedWithLock(keyHex string, countryCode string, now time.Time) bool {
	entry := self.entries[keyHex][countryCode]
	if entry == nil {
		return false
	}
	self.pruneEntryWithLock(entry, now)
	if len(entry.reporterTimes) < self.settings.ReportThreshold {
		return false
	}
	probeSuccessTime, ok := self.probeSuccessTimes[keyHex]
	if !ok {
		return false
	}
	return now.Sub(probeSuccessTime) < self.settings.ProbeWindow
}

// The records blocked in the country now, in key order.
func (self *ExtenderBlockedState) BlockedKeyHexes(countryCode string) []string {
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	keyHexes := []string{}
	for keyHex := range self.entries {
		if self.blockedWithLock(keyHex, countryCode, now) {
			keyHexes = append(keyHexes, keyHex)
		}
	}
	slices.Sort(keyHexes)
	return keyHexes
}
