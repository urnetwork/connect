package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// The operator's last country across a restart (open bug P052). The country a
// dial fronts with is the operator's while its hint is current, else the
// network country the host reports, else the operator's last answer. Apple
// hosts report no network country, and Windows and Linux only with a modem,
// so on those the last answer is the only country while the hint cannot be
// read -- and the iOS packet tunnel extension usually ends with its tunnel.
// The directory stores the last answer with its time beside its records, and
// a start uses it, ranked last, until CountryHintMaxAge has passed since the
// operator gave it.

// The directory of one start of the process: whatever `store` holds is loaded
// at construction, and Close writes what the save window still holds. No root
// key is installed, so the stored records stay as they were loaded.
func newTestCountryStoreDirectory(
	t *testing.T,
	clock *testClock,
	store *testExtenderDirectoryStore,
) *ExtenderDirectory {
	t.Helper()
	settings := DefaultExtenderDirectorySettings()
	settings.Now = clock.Now
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	settings.Store = store
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, settings)
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory
}

// A store that holds the operator's answer `countryCode`, given at the clock's
// time, as a start that heard the operator leaves it.
func newTestCountryStore(t *testing.T, clock *testClock, countryCode string) *testExtenderDirectoryStore {
	t.Helper()
	store := newTestExtenderDirectoryStore()
	directory := newTestCountryStoreDirectory(t, clock, store)
	directory.SetCountryHint(countryCode)
	directory.Close()
	return store
}

// The country section the store holds, nil when there is none.
func testStoredCountryHint(t *testing.T, store *testExtenderDirectoryStore) *extenderDirectoryStoreCountryHint {
	t.Helper()
	_, stateBytes := store.counts()
	state := &extenderDirectoryStoreState{}
	if err := json.Unmarshal(stateBytes, state); err != nil {
		t.Fatal(err)
	}
	if len(state.CountryHint) == 0 {
		return nil
	}
	storeCountryHint := &extenderDirectoryStoreCountryHint{}
	if err := json.Unmarshal(state.CountryHint, storeCountryHint); err != nil {
		t.Fatal(err)
	}
	return storeCountryHint
}

// A strategy over `directory`, the host reported as dual stack and the global
// list "spoof.example".
func newTestCountryStoreStrategy(t *testing.T, directory *ExtenderDirectory) *ClientStrategy {
	t.Helper()
	t.Cleanup(setSpoofDomainsForTest([]string{"spoof.example"}))
	t.Cleanup(swapControlFamilyProbe(func(family int) bool { return true }))
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientStrategySettings()
	settings.ExtenderDirectory = directory
	clientStrategy := NewClientStrategy(ctx, settings)
	t.Cleanup(func() {
		clientStrategy.Close()
		cancel()
	})
	return clientStrategy
}

// The outer names of the extender dialers of one expand.
func testExtenderServerNames(dialers []*clientDialer) []string {
	serverNames := []string{}
	for _, dialer := range dialers {
		serverNames = append(serverNames, dialer.extenderConfig.Profile.ServerName)
	}
	return serverNames
}

// The bug: a start where the operator cannot be asked, on a host that reports
// no network country, had no country at all and fronted with the global list.
// Now a start whose store holds an answer within its max age is in that
// country from the first dial, before any hint has answered: the strategy's
// first extender dialers draw from its list.
func TestExtenderDirectoryRestartUsesTheStoredCountry(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	ruSpoofDomains := []string{"one.ru.example", "two.ru.example"}
	installTestCountrySpoofLists(t, map[string][]string{"ru": ruSpoofDomains})
	clock := newTestClock()
	store := newTestCountryStore(t, clock, "RU")

	clock.advance(24 * time.Hour)
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
	})
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("after a restart the country = %q, expected the stored one", countryCode)
	}
	if spoofDomains, countryCode := directorySpoofDomains(directory); !slices.Equal(spoofDomains, ruSpoofDomains) ||
		countryCode != "ru" {
		t.Fatalf("after a restart the list = %v (%q), expected the ru list", spoofDomains, countryCode)
	}

	clientStrategy := newTestCountryStoreStrategy(t, directory)
	ip := netip.MustParseAddr("192.0.2.110")
	record := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress(ip.String(), ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	expandedDialers := clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 3 {
		t.Fatalf("dialers = %d, expected one per carrier", len(expandedDialers))
	}
	for _, serverName := range testExtenderServerNames(expandedDialers) {
		if !slices.Contains(ruSpoofDomains, serverName) {
			t.Fatalf("dialer name = %q, expected a name of the stored country's list", serverName)
		}
	}
}

// Past its max age the stored country is never used: not by a start that
// loads it, and not by a running directory whose last answer ages out, which
// then draws its addresses again from the global list. The age counts from the
// operator's answer, not from the start.
func TestExtenderDirectoryStoredCountryExpires(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	ruSpoofDomains := []string{"one.ru.example", "two.ru.example"}
	installTestCountrySpoofLists(t, map[string][]string{"ru": ruSpoofDomains})
	maxAge := DefaultExtenderDirectorySettings().CountryHintMaxAge
	if maxAge < 7*24*time.Hour || 14*24*time.Hour < maxAge {
		t.Fatalf("max age = %s, expected the week or two the design holds the country for", maxAge)
	}

	// a start past the max age
	clock := newTestClock()
	store := newTestCountryStore(t, clock, "ru")
	clock.advance(maxAge)
	expired := newTestCountryStoreDirectory(t, clock, store)
	if countryCode := expired.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("a start %s after the answer is in %q, expected no country", maxAge, countryCode)
	}
	if spoofDomains, _ := directorySpoofDomains(expired); slices.Equal(spoofDomains, ruSpoofDomains) {
		t.Fatal("a start past the max age drew from the stored country's list")
	}
	// and nothing it saves brings the country back
	expired.AddBootstrap(netip.MustParseAddr("192.0.2.112"), ExtenderSourceManual)
	expired.Close()
	if storeCountryHint := testStoredCountryHint(t, store); storeCountryHint != nil {
		t.Fatalf("an expired country was saved again: %+v", storeCountryHint)
	}

	// a start just inside the max age, which then runs past it
	clock = newTestClock()
	store = newTestCountryStore(t, clock, "ru")
	clock.advance(maxAge - time.Hour)
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
	})
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("a start inside the max age is in %q, expected the stored country", countryCode)
	}
	clientStrategy := newTestCountryStoreStrategy(t, directory)
	ip := netip.MustParseAddr("192.0.2.111")
	record := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress(ip.String(), ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	for _, serverName := range testExtenderServerNames(clientStrategy.expandExtenderDialers()) {
		if !slices.Contains(ruSpoofDomains, serverName) {
			t.Fatalf("dialer name = %q, expected a name of the stored country's list", serverName)
		}
	}

	clock.advance(time.Hour - time.Millisecond)
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("a millisecond inside the max age the country = %q, expected the stored one", countryCode)
	}
	clock.advance(time.Millisecond)
	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("at the max age the country = %q, expected none", countryCode)
	}
	expandedDialers := clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 3 {
		t.Fatalf("redrawn dialers = %d, expected the address drawn again", len(expandedDialers))
	}
	for _, serverName := range testExtenderServerNames(testExtenderDialers(clientStrategy)) {
		if serverName != "spoof.example" {
			t.Fatalf("dialer name = %q past the max age, expected the global list", serverName)
		}
	}

	// an answer this process heard ages out the same way once it is stale
	directory.SetCountryHint("ru")
	directory.ExpireCountryHint()
	clock.advance(maxAge - time.Millisecond)
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("a stale answer inside the max age gives %q, expected it", countryCode)
	}
	clock.advance(time.Millisecond)
	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("a stale answer at the max age gives %q, expected none", countryCode)
	}
	// and a save leaves it out, since no start would use it
	directory.AddBootstrap(netip.MustParseAddr("192.0.2.114"), ExtenderSourceManual)
	directory.Close()
	if storeCountryHint := testStoredCountryHint(t, store); storeCountryHint != nil {
		t.Fatalf("a country past its max age was saved: %+v", storeCountryHint)
	}
}

// The stored country is the last fallback: the network country the host
// reports outranks it while there is one, and it stands in again once the host
// reports none.
func TestExtenderDirectoryNetworkCountryOutranksTheStoredCountry(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	installTestCountrySpoofLists(t, map[string][]string{
		"ru": {"one.ru.example"},
		"kz": {"one.kz.example"},
	})
	clock := newTestClock()
	store := newTestCountryStore(t, clock, "ru")
	clock.advance(24 * time.Hour)
	directory := newTestCountryStoreDirectory(t, clock, store)

	SetNetworkCountryCode("KZ")
	if countryCode := directory.SpoofCountryCode(); countryCode != "kz" {
		t.Fatalf("with a reported country the country = %q, expected the reported one", countryCode)
	}
	if spoofDomains, _ := directorySpoofDomains(directory); !slices.Equal(spoofDomains, []string{"one.kz.example"}) {
		t.Fatalf("with a reported country the list = %v, expected the kz list", spoofDomains)
	}
	SetNetworkCountryCode("")
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("with no report the country = %q, expected the stored one", countryCode)
	}
}

// A fresh answer replaces the stored country at once, and every answer renews
// the stored time, so the max age counts from the latest answer. An operator
// that cannot place the client renews nothing.
func TestExtenderDirectoryFreshHintReplacesTheStoredCountry(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	maxAge := DefaultExtenderDirectorySettings().CountryHintMaxAge
	clock := newTestClock()
	store := newTestCountryStore(t, clock, "ru")
	answerTime := clock.Now()

	clock.advance(24 * time.Hour)
	directory := newTestCountryStoreDirectory(t, clock, store)
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("after a restart the country = %q, expected the stored one", countryCode)
	}
	if !directory.SetCountryHint("DE") {
		t.Fatal("a fresh answer did not change the stored country")
	}
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("with a fresh answer the country = %q, expected it", countryCode)
	}
	// once stale, the last answer is the fresh one, not the stored one
	directory.ExpireCountryHint()
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("once stale the country = %q, expected the fresh answer", countryCode)
	}
	directory.Close()
	storeCountryHint := testStoredCountryHint(t, store)
	if storeCountryHint == nil || storeCountryHint.CountryCode != "de" ||
		storeCountryHint.TimeMs != clock.Now().UnixMilli() {
		t.Fatalf("stored = %+v, expected de at the time of the fresh answer", storeCountryHint)
	}

	// the age counts from the fresh answer: past the first answer's max age,
	// a start is still in the fresh answer's country
	clock.advance(maxAge - time.Hour)
	if !clock.Now().After(answerTime.Add(maxAge)) {
		t.Fatal("the test clock is not past the first answer's max age")
	}
	directory = newTestCountryStoreDirectory(t, clock, store)
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("after a second restart the country = %q, expected the fresh answer", countryCode)
	}

	// the same country answered again renews the time
	directory.SetCountryHint("de")
	renewTime := clock.Now()
	directory.Close()
	if storeCountryHint := testStoredCountryHint(t, store); storeCountryHint == nil ||
		storeCountryHint.CountryCode != "de" || storeCountryHint.TimeMs != renewTime.UnixMilli() {
		t.Fatalf("stored = %+v, expected de renewed at %s", storeCountryHint, renewTime)
	}

	// an operator that cannot place the client renews nothing, in the process
	// or in the store
	clock.advance(maxAge - time.Hour)
	directory = newTestCountryStoreDirectory(t, clock, store)
	if directory.SetCountryHint("") {
		t.Fatal("an empty answer changed the country")
	}
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("after an empty answer the country = %q, expected the last answer", countryCode)
	}
	directory.AddBootstrap(netip.MustParseAddr("192.0.2.113"), ExtenderSourceManual)
	directory.Close()
	if storeCountryHint := testStoredCountryHint(t, store); storeCountryHint == nil ||
		storeCountryHint.TimeMs != renewTime.UnixMilli() {
		t.Fatalf("stored = %+v, expected the empty answer to renew nothing", storeCountryHint)
	}
	clock.advance(time.Hour)
	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("past the last answer's max age the country = %q, expected none", countryCode)
	}
	directory = newTestCountryStoreDirectory(t, clock, store)
	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("a start past the last answer's max age is in %q, expected none", countryCode)
	}
}

// A store from before the country, and a country section this build cannot
// use -- the wrong json type, another version, a code that is not two letters,
// a time that is missing, ahead of the clock or past the max age -- leave no
// country, and the directory beside it still loads. An envelope that cannot be
// read at all loads nothing, its country included.
func TestExtenderDirectoryIgnoresAnUnusableStoredCountry(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	clock := newTestClock()
	maxAge := DefaultExtenderDirectorySettings().CountryHintMaxAge
	answerTimeMs := clock.Now().Add(-24 * time.Hour).UnixMilli()
	countryHint := func(version int, countryCode string, timeMs int64) string {
		return fmt.Sprintf(`{"version":%d,"country_code":%q,"time_ms":%d}`, version, countryCode, timeMs)
	}
	envelope := func(version int, countryHintJson string) []byte {
		if countryHintJson == "" {
			return []byte(fmt.Sprintf(`{"version":%d,"records":[],"addresses":[{"ip":"192.0.2.120","source":"manual"}]}`, version))
		}
		return []byte(fmt.Sprintf(
			`{"version":%d,"records":[],"addresses":[{"ip":"192.0.2.120","source":"manual"}],"country_hint":%s}`,
			version,
			countryHintJson,
		))
	}
	load := func(stateBytes []byte) *ExtenderDirectory {
		store := newTestExtenderDirectoryStore()
		store.stateBytes = stateBytes
		return newTestCountryStoreDirectory(t, clock, store)
	}

	// the control: a usable section, in any case, is restored
	if countryCode := load(envelope(1, countryHint(1, "RU", answerTimeMs))).SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("a usable section gives %q, expected ru", countryCode)
	}

	cases := []struct {
		name            string
		countryHintJson string
	}{
		{name: "a store from before the country", countryHintJson: ""},
		{name: "null", countryHintJson: `null`},
		{name: "a string", countryHintJson: `"ru"`},
		{name: "a list", countryHintJson: `["ru"]`},
		{name: "another version", countryHintJson: countryHint(2, "ru", answerTimeMs)},
		{name: "no version", countryHintJson: fmt.Sprintf(`{"country_code":"ru","time_ms":%d}`, answerTimeMs)},
		{name: "three letters", countryHintJson: countryHint(1, "rus", answerTimeMs)},
		{name: "not letters", countryHintJson: countryHint(1, "r1", answerTimeMs)},
		{name: "no country", countryHintJson: countryHint(1, "", answerTimeMs)},
		{name: "no time", countryHintJson: `{"version":1,"country_code":"ru"}`},
		{name: "a time that is not a number", countryHintJson: `{"version":1,"country_code":"ru","time_ms":"yesterday"}`},
		{name: "a time before the epoch", countryHintJson: countryHint(1, "ru", -1)},
		{name: "a time ahead of the clock", countryHintJson: countryHint(1, "ru", clock.Now().Add(time.Hour).UnixMilli())},
		{name: "a time past the max age", countryHintJson: countryHint(1, "ru", clock.Now().Add(-maxAge).UnixMilli())},
	}
	for _, c := range cases {
		directory := load(envelope(1, c.countryHintJson))
		if countryCode := directory.SpoofCountryCode(); countryCode != "" {
			t.Errorf("%s: the country = %q, expected none", c.name, countryCode)
		}
		if knownCount := directory.Snapshot().KnownCount; knownCount != 1 {
			t.Errorf("%s: known = %d, expected the stored address beside it", c.name, knownCount)
		}
	}

	// an envelope this build cannot read loads nothing, the country included
	for i, stateBytes := range [][]byte{
		envelope(ExtenderDirectoryStoreVersion+1, countryHint(1, "ru", answerTimeMs)),
		[]byte(fmt.Sprintf(`{"version":1,"country_hint":%s`, countryHint(1, "ru", answerTimeMs))),
	} {
		directory := load(stateBytes)
		if countryCode := directory.SpoofCountryCode(); countryCode != "" {
			t.Errorf("envelope %d: the country = %q, expected none", i, countryCode)
		}
		if knownCount := directory.Snapshot().KnownCount; knownCount != 0 {
			t.Errorf("envelope %d: known = %d, expected an empty directory", i, knownCount)
		}
	}

	// a store from before the country gains the section with the first answer
	store := newTestExtenderDirectoryStore()
	store.stateBytes = envelope(1, "")
	directory := newTestCountryStoreDirectory(t, clock, store)
	directory.SetCountryHint("ru")
	directory.Close()
	if storeCountryHint := testStoredCountryHint(t, store); storeCountryHint == nil || storeCountryHint.CountryCode != "ru" {
		t.Fatalf("stored = %+v, expected the first answer", storeCountryHint)
	}
}

// The section holds the two-letter code and the time of the operator's last
// answer and nothing else, and a directory the operator never placed writes
// none. The envelope keeps its version, so a build that predates the section
// -- which discards an envelope of another version whole -- still loads every
// record and address of a store this build writes.
func TestExtenderDirectoryStoresOnlyTheCountryAndItsTime(t *testing.T) {
	clock := newTestClock()
	store := newTestExtenderDirectoryStore()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
		settings.Store = store
	})
	record := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress("192.0.2.130"),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	directory.AddBootstrap(netip.MustParseAddr("192.0.2.131"), ExtenderSourceManual)

	// never placed: no section
	directory.Close()
	_, stateBytes := store.counts()
	envelopeFieldValues := map[string]json.RawMessage{}
	if err := json.Unmarshal(stateBytes, &envelopeFieldValues); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelopeFieldValues["country_hint"]; ok {
		t.Fatalf("a directory the operator never placed stored %s", envelopeFieldValues["country_hint"])
	}

	directory = newTestCountryStoreDirectory(t, clock, store)
	directory.SetCountryHint(" Ru ")
	directory.Close()
	_, stateBytes = store.counts()
	envelopeFieldValues = map[string]json.RawMessage{}
	if err := json.Unmarshal(stateBytes, &envelopeFieldValues); err != nil {
		t.Fatal(err)
	}
	countryHintFieldValues := map[string]any{}
	if err := json.Unmarshal(envelopeFieldValues["country_hint"], &countryHintFieldValues); err != nil {
		t.Fatalf("country section %s: %s", envelopeFieldValues["country_hint"], err)
	}
	expectedCountryHintFieldValues := map[string]any{
		"version":      float64(extenderDirectoryStoreCountryHintVersion),
		"country_code": "ru",
		"time_ms":      float64(clock.Now().UnixMilli()),
	}
	if !maps.Equal(countryHintFieldValues, expectedCountryHintFieldValues) {
		t.Fatalf("country section = %v, expected %v", countryHintFieldValues, expectedCountryHintFieldValues)
	}

	// the envelope as a build that predates the section reads it
	priorState := &struct {
		Version   int                              `json:"version"`
		Records   []*extenderDirectoryStoreRecord  `json:"records"`
		Addresses []*extenderDirectoryStoreAddress `json:"addresses"`
	}{}
	if err := json.Unmarshal(stateBytes, priorState); err != nil {
		t.Fatalf("a build that predates the section cannot read the store: %s", err)
	}
	if priorState.Version != 1 || len(priorState.Records) != 1 || len(priorState.Addresses) != 2 {
		t.Fatalf(
			"a build that predates the section reads version %d, %d records, %d addresses; expected version 1 with every one",
			priorState.Version,
			len(priorState.Records),
			len(priorState.Addresses),
		)
	}
}

// An operator that answers only once the test lets it, and says when a read
// has started, so a test holds a read out on a barrier rather than on time.
type testGatedExtenderHint struct {
	// receives as a read starts; one buffered signal, the rest dropped
	reading chan struct{}
	// closed by the test to let every read answer
	release chan struct{}
	result  *ExtenderHintResult
}

// The settings seam.
func (self *testGatedExtenderHint) Hint(ctx context.Context) (*ExtenderHintResult, error) {
	select {
	case self.reading <- struct{}{}:
	default:
	}
	select {
	case <-self.release:
		result := *self.result
		return &result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A start of the network client with the stored country: while the first hint
// read is out, which is all there is where the operator cannot be reached
// directly, the client is in the stored country if it is within its max age
// and in none if it is not. The answer then replaces it, and is what the next
// start finds stored. The read is held on a barrier, and the answer is applied
// before the first read ends (initialHintDone), so no step waits on time.
func TestExtenderNetworkClientStartsInTheStoredCountry(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	ruSpoofDomains := []string{"one.ru.example", "two.ru.example"}
	installTestCountrySpoofLists(t, map[string][]string{"ru": ruSpoofDomains})
	maxAge := DefaultExtenderDirectorySettings().CountryHintMaxAge

	cases := []struct {
		name                string
		age                 time.Duration
		expectedCountryCode string
	}{
		{name: "within the max age", age: maxAge - time.Hour, expectedCountryCode: "ru"},
		{name: "past the max age", age: maxAge, expectedCountryCode: ""},
	}
	for _, c := range cases {
		clock := newTestClock()
		store := newTestCountryStore(t, clock, "ru")
		clock.advance(c.age)

		hint := &testGatedExtenderHint{
			reading: make(chan struct{}, 1),
			release: make(chan struct{}),
			result:  &ExtenderHintResult{ContinentCode: "EU", CountryCode: "DE"},
		}
		networkClient, directory, _ := newTestExtenderNetworkClientWithDirectory(
			t,
			clock,
			func(settings *ExtenderDirectorySettings) {
				settings.Store = store
			},
			func(settings *ExtenderNetworkClientSettings) {
				settings.Hint = hint.Hint
				// the read stays out until the test releases it
				settings.HelloTimeout = 30 * time.Second
				settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
					return nil, nil
				}
			},
		)
		select {
		case <-hint.reading:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the hint was never read", c.name)
		}
		if countryCode := directory.SpoofCountryCode(); countryCode != c.expectedCountryCode {
			t.Fatalf("%s: before the hint answers the country = %q, expected %q", c.name, countryCode, c.expectedCountryCode)
		}
		if spoofDomains, _ := directorySpoofDomains(directory); slices.Equal(spoofDomains, ruSpoofDomains) != (c.expectedCountryCode == "ru") {
			t.Fatalf("%s: before the hint answers the list = %v", c.name, spoofDomains)
		}

		close(hint.release)
		select {
		case <-networkClient.initialHintDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the first hint read never ended", c.name)
		}
		if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
			t.Fatalf("%s: after the answer the country = %q, expected it", c.name, countryCode)
		}
		networkClient.Close()
		directory.Close()
		if storeCountryHint := testStoredCountryHint(t, store); storeCountryHint == nil ||
			storeCountryHint.CountryCode != "de" || storeCountryHint.TimeMs != clock.Now().UnixMilli() {
			t.Fatalf("%s: stored = %+v, expected the answer", c.name, storeCountryHint)
		}
	}
}
