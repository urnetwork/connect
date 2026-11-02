package connect

// net_extender_camouflage_select.go — the borrowed name a client fronts the
// camouflaged tcp attempt to one extender with, and whether that extender covers
// every name its supported clients select (EXTENDER-UPDATE2 §3.1, §3.2).
//
// The splice shows a prober the real site of the hello's sni, so the camouflage
// holds only while every name a supported client fronts an extender with is one
// that extender verified and splices to: a prober replaying the sni it observed
// then meets the original site's own handshake. A name the extender does not
// cover is spliced to another borrowed site — a substitution, a degraded
// outcome, since the prober sees a certificate for a different name — or
// terminated, the legacy tell. Selection therefore follows rules that keep that
// set small and computable by the extender itself:
//
//   - One name per extender. A client fronts an extender with the name of its
//     list that scores highest under sha256(domain | identity key | name)
//     (rendezvous hashing). Every client of a list version fronts a given
//     extender with the same name, as every visitor of a site sends that site's
//     name, and the extender computes the very name its clients select.
//   - Stable across list changes. Adding or removing a name moves only the
//     extenders whose top name it is, so one list release changes few fronts.
//   - Staged additions. A name enters a list staged, selectable from a later
//     version: extenders accept and verify it before any client selects it.
//   - Retirement after the support window. A name a version stops selecting is
//     kept retiring, selectable until that version: extenders still cover it for
//     the clients of earlier versions.
//   - Country lists. A client selects from its country's list when one is
//     bundled, else from the global list, so an extender covers the name each
//     list selects for it.
//
// An extender of list version V covers the clients of versions V-back through
// V+forward (one each by default): for each list and each of those versions it
// computes the front name, and its coverage is complete only when it verified
// every one of them within the list's maximum measurement age.
//
// Everything here is a pure function of its arguments and safe for concurrent
// use; the lists are read through extenderCamouflageBorrowLists, which tests
// replace.

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"
	"time"
)

// Domain separation of the front-name score, so the score is never a value any
// other construction derives from the identity key.
const extenderCamouflageFrontNameDomain = "ur-extender-borrow-front-v1"

// The versions before and after its own whose clients an extender covers, when
// the list does not say.
const (
	defaultExtenderBorrowCompatibleVersionsBack    = 1
	defaultExtenderBorrowCompatibleVersionsForward = 1
)

// One name of a borrow list and the list versions whose clients select it.
type ExtenderBorrowName struct {
	Name string
	// The first list version whose clients select the name, zero for a name
	// selectable from the first version. A name whose first version is past
	// the list's own version is staged.
	SelectableFromVersion int
	// The first list version whose clients no longer select the name, zero
	// while it is still selectable. A name whose last version is at or before
	// the list's own version is retiring.
	SelectableUntilVersion int
}

// Whether the clients of one list version select the name.
func (self ExtenderBorrowName) selectableAt(version int) bool {
	if version < self.SelectableFromVersion {
		return false
	}
	if 0 < self.SelectableUntilVersion && self.SelectableUntilVersion <= version {
		return false
	}
	return true
}

// ExtenderBorrowList is one borrow list, the global list or one country's, with
// the measurement metadata the rollout gates on (§3.1). The measured resource
// and its manifest supply it; selection and coverage read it.
type ExtenderBorrowList struct {
	// "" for the global list, else the normalized country code
	CountryCode string
	// The list version, zero for an unversioned list.
	Version int
	// Whether the list carries a manifest this build understands. An
	// unmeasured list is never camouflage-compatible.
	Measured             bool
	MeasuredAt           time.Time
	MaxMeasurementAge    time.Duration
	RevalidationInterval time.Duration
	// The list versions before and after its own whose clients an extender of
	// this list covers. Zero takes one.
	CompatibleVersionsBack    int
	CompatibleVersionsForward int
	Names                     []ExtenderBorrowName
}

// The names the clients of one list version select, normalized, distinct and
// in list order.
func (self *ExtenderBorrowList) SelectableNames(version int) []string {
	names := []string{}
	if self == nil {
		return names
	}
	for _, borrowName := range self.Names {
		if !borrowName.selectableAt(version) {
			continue
		}
		if name := normalizeExtenderBorrowName(borrowName.Name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// Every name of the list an extender accepts and verifies — selectable, staged
// and retiring — normalized, distinct and in list order.
func (self *ExtenderBorrowList) AcceptNames() []string {
	names := []string{}
	if self == nil {
		return names
	}
	for _, borrowName := range self.Names {
		if name := normalizeExtenderBorrowName(borrowName.Name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// The client list versions an extender of this list covers, oldest first.
func (self *ExtenderBorrowList) coveredVersions() []int {
	back := self.CompatibleVersionsBack
	if back <= 0 {
		back = defaultExtenderBorrowCompatibleVersionsBack
	}
	forward := self.CompatibleVersionsForward
	if forward <= 0 {
		forward = defaultExtenderBorrowCompatibleVersionsForward
	}
	versions := []int{}
	for version := self.Version - back; version <= self.Version+forward; version += 1 {
		if 0 <= version {
			versions = append(versions, version)
		}
	}
	return versions
}

// Whether the list's measurement is past its maximum age at now. A list that
// sets no maximum age never goes stale; an unmeasured list is handled by its
// caller before this matters.
func (self *ExtenderBorrowList) stale(now time.Time) bool {
	if self.MaxMeasurementAge <= 0 {
		return false
	}
	return self.MeasuredAt.Add(self.MaxMeasurementAge).Before(now)
}

// The lower-case, dot-trimmed form both ends compare borrowed names in.
func normalizeExtenderBorrowName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// ExtenderCamouflageFrontName is the borrowed name a client fronts the
// camouflaged tcp attempt to one extender with: of names, the one whose
// sha256(domain | identity key | name) is greatest (rendezvous hashing, see the
// file comment). Empty when names is empty or the key is not set, which drops
// the camouflaged attempt to the legacy one alone.
func ExtenderCamouflageFrontName(identityPublicKey []byte, names []string) string {
	if len(identityPublicKey) == 0 {
		return ""
	}
	var frontName string
	var frontScore []byte
	for _, name := range names {
		if name = normalizeExtenderBorrowName(name); name == "" {
			continue
		}
		score := extenderCamouflageFrontNameScore(identityPublicKey, name)
		// equal scores are a sha256 collision; the lesser name breaks the tie so
		// the choice never depends on list order
		if c := bytes.Compare(score, frontScore); frontScore == nil || 0 < c || (c == 0 && name < frontName) {
			frontName = name
			frontScore = score
		}
	}
	return frontName
}

// The rendezvous score of one name for one extender. Every field is length
// prefixed, so no key and name pair scores as another.
func extenderCamouflageFrontNameScore(identityPublicKey []byte, name string) []byte {
	hash := sha256.New()
	for _, field := range [][]byte{[]byte(extenderCamouflageFrontNameDomain), identityPublicKey, []byte(name)} {
		hash.Write([]byte{byte(len(field) >> 8), byte(len(field))})
		hash.Write(field)
	}
	return hash.Sum(nil)
}

// ExtenderCamouflageCoverage is whether an extender covers every name its
// supported clients front it with (§3.1, §3.3). It is complete only when every
// required name has a verification no older than its list's maximum
// measurement age; anything less is the degraded state, which must not be
// reported as camouflage coverage.
type ExtenderCamouflageCoverage struct {
	// The front names supported clients select for this extender: one per list
	// and covered client version, distinct and sorted.
	RequiredNames []string
	// The required names without a current verification.
	MissingNames []string
	// Every name this extender accepts, staged and retiring included: the names
	// a prober's hello is spliced to its own site for.
	AcceptNames []string
	// The accept names without a current verification. A prober fronting one
	// is substituted or terminated.
	UnverifiedAcceptNames []string
	// The number of lists whose measurement is past its maximum age, or that
	// are unmeasured.
	StaleListCount int
}

// Whether every required name is verified and every list is measured and
// current. An extender with no required name covers nothing.
func (self ExtenderCamouflageCoverage) Complete() bool {
	return 0 < len(self.RequiredNames) && len(self.MissingNames) == 0 && self.StaleListCount == 0
}

// EvaluateExtenderCamouflageCoverage computes the coverage of the extender with
// the given identity key from the bundled lists and the times it last verified
// each name (§3.1). A verification older than its list's maximum measurement age
// at now does not count; a name a list does not name is not required.
func EvaluateExtenderCamouflageCoverage(
	identityPublicKey []byte,
	borrowLists []*ExtenderBorrowList,
	verifiedTimes map[string]time.Time,
	now time.Time,
) ExtenderCamouflageCoverage {
	coverage := ExtenderCamouflageCoverage{
		RequiredNames:         []string{},
		MissingNames:          []string{},
		AcceptNames:           []string{},
		UnverifiedAcceptNames: []string{},
	}
	normalizedVerifiedTimes := map[string]time.Time{}
	for name, verifiedTime := range verifiedTimes {
		if name = normalizeExtenderBorrowName(name); name != "" {
			if previous, ok := normalizedVerifiedTimes[name]; !ok || previous.Before(verifiedTime) {
				normalizedVerifiedTimes[name] = verifiedTime
			}
		}
	}
	// the strictest maximum age of the lists that name each name
	maxAges := map[string]time.Duration{}
	required := map[string]bool{}
	accepted := map[string]bool{}
	for _, borrowList := range borrowLists {
		if borrowList == nil {
			continue
		}
		if !borrowList.Measured || borrowList.stale(now) {
			coverage.StaleListCount += 1
		}
		for _, name := range borrowList.AcceptNames() {
			accepted[name] = true
			if maxAge, ok := maxAges[name]; borrowList.MaxMeasurementAge > 0 && (!ok || maxAge <= 0 || borrowList.MaxMeasurementAge < maxAge) {
				maxAges[name] = borrowList.MaxMeasurementAge
			} else if !ok {
				maxAges[name] = borrowList.MaxMeasurementAge
			}
		}
		for _, version := range borrowList.coveredVersions() {
			if frontName := ExtenderCamouflageFrontName(identityPublicKey, borrowList.SelectableNames(version)); frontName != "" {
				required[frontName] = true
			}
		}
	}
	verified := func(name string) bool {
		verifiedTime, ok := normalizedVerifiedTimes[name]
		if !ok {
			return false
		}
		if maxAge := maxAges[name]; 0 < maxAge && verifiedTime.Add(maxAge).Before(now) {
			return false
		}
		return true
	}
	for name := range required {
		coverage.RequiredNames = append(coverage.RequiredNames, name)
		if !verified(name) {
			coverage.MissingNames = append(coverage.MissingNames, name)
		}
	}
	for name := range accepted {
		coverage.AcceptNames = append(coverage.AcceptNames, name)
		if !verified(name) {
			coverage.UnverifiedAcceptNames = append(coverage.UnverifiedAcceptNames, name)
		}
	}
	slices.Sort(coverage.RequiredNames)
	slices.Sort(coverage.MissingNames)
	slices.Sort(coverage.AcceptNames)
	slices.Sort(coverage.UnverifiedAcceptNames)
	return coverage
}

// The country a directory places this client in, as its borrow list is chosen
// by: the directory's raw SpoofCountryCode, which may name a country with a
// borrow list and no spoof list of its own. Empty without a directory.
func directoryBorrowCountryCode(directory *ExtenderDirectory) string {
	if directory == nil {
		return ""
	}
	return directory.SpoofCountryCode()
}

// The source of the bundled borrow lists, replaced in tests. The default reads
// the bundled resources (net_extender_borrow.go): the global list and each
// country's, as unversioned, unmeasured lists until the measured resource
// carries its manifest.
var (
	extenderCamouflageBorrowListsStateLock sync.Mutex
	extenderCamouflageBorrowListsSource    = bundledExtenderCamouflageBorrowLists
)

// The bundled lists: the global list first, then each country's in country
// order.
func bundledExtenderCamouflageBorrowLists() []*ExtenderBorrowList {
	borrowLists := []*ExtenderBorrowList{unversionedExtenderBorrowList("", BorrowDomains())}
	for _, countryCode := range borrowCountryCodes() {
		borrowLists = append(borrowLists, unversionedExtenderBorrowList(countryCode, countryBorrowDomains(countryCode)))
	}
	return borrowLists
}

// A list with no manifest: every name selectable, version zero, unmeasured.
func unversionedExtenderBorrowList(countryCode string, names []string) *ExtenderBorrowList {
	borrowList := &ExtenderBorrowList{
		CountryCode: countryCode,
		Names:       []ExtenderBorrowName{},
	}
	for _, name := range names {
		borrowList.Names = append(borrowList.Names, ExtenderBorrowName{Name: name})
	}
	return borrowList
}

// ExtenderCamouflageBorrowLists is every bundled borrow list: the global list
// first, then each country's. The lists are copies the caller may keep.
func ExtenderCamouflageBorrowLists() []*ExtenderBorrowList {
	source := func() func() []*ExtenderBorrowList {
		extenderCamouflageBorrowListsStateLock.Lock()
		defer extenderCamouflageBorrowListsStateLock.Unlock()
		return extenderCamouflageBorrowListsSource
	}()
	borrowLists := []*ExtenderBorrowList{}
	for _, borrowList := range source() {
		if borrowList != nil {
			borrowLists = append(borrowLists, cloneExtenderBorrowList(borrowList))
		}
	}
	return borrowLists
}

// ExtenderCamouflageBorrowListForCountry is the list a client in countryCode
// selects from: that country's list when one is bundled with a selectable name,
// else the global list, as BorrowDomainsForCountry chooses. Nil when there is
// no list at all.
func ExtenderCamouflageBorrowListForCountry(countryCode string) *ExtenderBorrowList {
	countryCode = NormalizeSpoofCountryCode(countryCode)
	var globalList *ExtenderBorrowList
	for _, borrowList := range ExtenderCamouflageBorrowLists() {
		switch borrowList.CountryCode {
		case "":
			if globalList == nil {
				globalList = borrowList
			}
		case countryCode:
			if countryCode != "" && 0 < len(borrowList.SelectableNames(borrowList.Version)) {
				return borrowList
			}
		}
	}
	return globalList
}

func cloneExtenderBorrowList(borrowList *ExtenderBorrowList) *ExtenderBorrowList {
	clone := *borrowList
	clone.Names = slices.Clone(borrowList.Names)
	return &clone
}

// Installs other lists in place of the bundled ones and returns the restore.
func setExtenderCamouflageBorrowListsForTest(borrowLists []*ExtenderBorrowList) func() {
	extenderCamouflageBorrowListsStateLock.Lock()
	defer extenderCamouflageBorrowListsStateLock.Unlock()
	previousSource := extenderCamouflageBorrowListsSource
	installed := []*ExtenderBorrowList{}
	for _, borrowList := range borrowLists {
		installed = append(installed, cloneExtenderBorrowList(borrowList))
	}
	extenderCamouflageBorrowListsSource = func() []*ExtenderBorrowList {
		return installed
	}
	return func() {
		extenderCamouflageBorrowListsStateLock.Lock()
		defer extenderCamouflageBorrowListsStateLock.Unlock()
		extenderCamouflageBorrowListsSource = previousSource
	}
}

// Testing_SetExtenderCamouflageBorrowLists installs synthetic borrow lists for
// tests in other packages (the extender fixture) and returns the restore.
func Testing_SetExtenderCamouflageBorrowLists(borrowLists []*ExtenderBorrowList) func() {
	return setExtenderCamouflageBorrowListsForTest(borrowLists)
}
