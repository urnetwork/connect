package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"
)

// Country spoof lists (open bug P052). A dial fronts with a name of the list
// of the country its directory places the client in, when that country has a
// list of its own, and with the global list otherwise. The country is the
// operator's hint while it answers, and the network country the host reports
// while it cannot be asked. Every list here is synthetic: no real domain
// appears in the repository, and no country list is bundled yet.

// Installs synthetic country lists in place of the embedded ones, each written
// in the resource form the generator writes, until the test ends.
func installTestCountrySpoofLists(t *testing.T, countrySpoofDomains map[string][]string) {
	t.Helper()
	resources := fstest.MapFS{}
	for countryCode, spoofDomains := range countrySpoofDomains {
		resource, err := EncodeSpoofDomainsResource(spoofDomains)
		if err != nil {
			t.Fatal(err)
		}
		resources[SpoofResourcePath(countryCode)] = &fstest.MapFile{Data: resource}
	}
	t.Cleanup(setSpoofCountryResourcesForTest(resources))
}

// Sets the process network country until the test ends.
func setTestNetworkCountryCode(t *testing.T, countryCode string) {
	t.Helper()
	previousCountryCode := NetworkCountryCode()
	SetNetworkCountryCode(countryCode)
	t.Cleanup(func() {
		SetNetworkCountryCode(previousCountryCode)
	})
}

// Waits until the directory's country is the expected one.
func waitForSpoofCountryCode(t *testing.T, directory *ExtenderDirectory, expected string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for directory.SpoofCountryCode() != expected {
		if time.Now().After(deadline) {
			t.Fatalf("spoof country = %q, expected %q", directory.SpoofCountryCode(), expected)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An operator hint whose answer a test changes while the client runs.
type testExtenderHint struct {
	stateLock sync.Mutex
	result    *ExtenderHintResult
	err       error
	count     int
}

// An operator that answers with the result until a test changes it.
func newTestExtenderHint(result *ExtenderHintResult) *testExtenderHint {
	return &testExtenderHint{
		result: result,
	}
}

// The settings seam.
func (self *testExtenderHint) Hint(ctx context.Context) (*ExtenderHintResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
	if self.err != nil {
		return nil, self.err
	}
	result := *self.result
	return &result, nil
}

// The operator answers with this from the next ask on.
func (self *testExtenderHint) Answer(result *ExtenderHintResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.result = result
	self.err = nil
}

// The operator cannot be asked from the next ask on.
func (self *testExtenderHint) Fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

// The asks so far.
func (self *testExtenderHint) Count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

// Waits until the hint has been asked at least count times.
func (self *testExtenderHint) waitForCount(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for self.Count() < count {
		if time.Now().After(deadline) {
			t.Fatalf("the hint was asked %d times, expected at least %d", self.Count(), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A country list is named by its lower case code, which is the one name the
// generator writes and the embedded set is read by. A code that is not two
// letters names the global list, never a file.
func TestSpoofResourcePathNamesTheCountryList(t *testing.T) {
	cases := []struct {
		countryCode  string
		resourcePath string
	}{
		{countryCode: "ru", resourcePath: "res/extender_spoof_ru.bin"},
		{countryCode: " RU ", resourcePath: "res/extender_spoof_ru.bin"},
		{countryCode: "", resourcePath: "res/extender_spoof.bin"},
		{countryCode: "rus", resourcePath: "res/extender_spoof.bin"},
		{countryCode: "r1", resourcePath: "res/extender_spoof.bin"},
		{countryCode: "../", resourcePath: "res/extender_spoof.bin"},
	}
	for _, c := range cases {
		if resourcePath := SpoofResourcePath(c.countryCode); resourcePath != c.resourcePath {
			t.Errorf("%q resource = %q, expected %q", c.countryCode, resourcePath, c.resourcePath)
		}
	}
	// the global list is read from the same embedded set by that name
	if len(extenderSpoofResource) == 0 {
		t.Fatal("the global resource was not read from the embedded set")
	}
}

// A country with no list of its own, an empty or malformed code, and a
// country list that is empty or does not decode all take the global list: an
// empty country list must not leave a dial with no outer name where the
// global list has one.
func TestSpoofDomainsForCountryFallsBackToTheGlobalList(t *testing.T) {
	t.Cleanup(setSpoofDomainsForTest([]string{"global.example"}))
	emptyResource, err := EncodeSpoofDomainsResource([]string{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(setSpoofCountryResourcesForTest(fstest.MapFS{
		"res/extender_spoof_by.bin": &fstest.MapFile{Data: emptyResource},
		"res/extender_spoof_kz.bin": &fstest.MapFile{Data: []byte("not a resource")},
	}))

	for _, countryCode := range []string{"ru", "by", "kz", "", "rus", "r1"} {
		spoofDomains := SpoofDomainsForCountry(countryCode)
		if !slices.Equal(spoofDomains, []string{"global.example"}) {
			t.Errorf("%q list = %v, expected the global list", countryCode, spoofDomains)
		}
		if _, listCountryCode := spoofDomainsForCountry(countryCode); listCountryCode != "" {
			t.Errorf("%q list is named %q, expected the global list", countryCode, listCountryCode)
		}
	}
	// nothing that does not decode to a name reaches the extender whitelist
	if allSpoofDomains := AllSpoofDomains(); !slices.Equal(allSpoofDomains, []string{"global.example"}) {
		t.Fatalf("all names = %v, expected the global list alone", allSpoofDomains)
	}
}

// A country's resource, written in the generator's form under the name the
// generator gives it, reads back through SpoofDomainsForCountry as written, in
// any case of its code; every other country keeps the global list. The
// extender whitelist takes every list. The restore puts the embedded lists
// back.
func TestSpoofDomainsForCountryRoundTripsACountryResource(t *testing.T) {
	t.Cleanup(setSpoofDomainsForTest([]string{"global.example", "shared.example"}))
	ruSpoofDomains := []string{"one.ru.example", "shared.example", "two.ru.example"}
	ruResource, err := EncodeSpoofDomainsResource(ruSpoofDomains)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedResource, err := EncodeSpoofDomainsResource([]string{"unrelated.example"})
	if err != nil {
		t.Fatal(err)
	}
	restore := setSpoofCountryResourcesForTest(fstest.MapFS{
		"res/extender_spoof_ru.bin": &fstest.MapFile{Data: ruResource},
		// names no country list may be read by: the global resource's own,
		// an upper case code and a code of three letters
		"res/extender_spoof.bin":     &fstest.MapFile{Data: unrelatedResource},
		"res/extender_spoof_RU.bin":  &fstest.MapFile{Data: unrelatedResource},
		"res/extender_spoof_rus.bin": &fstest.MapFile{Data: unrelatedResource},
	})

	for _, countryCode := range []string{"ru", "RU", " Ru "} {
		spoofDomains, listCountryCode := spoofDomainsForCountry(countryCode)
		if !slices.Equal(spoofDomains, ruSpoofDomains) || listCountryCode != "ru" {
			t.Fatalf("%q list = %v (%q), expected the ru list", countryCode, spoofDomains, listCountryCode)
		}
	}
	spoofDomains := SpoofDomainsForCountry("ru")
	spoofDomains[0] = "mutated.example"
	if SpoofDomainsForCountry("ru")[0] != "one.ru.example" {
		t.Fatal("the caller mutated the country list")
	}
	if spoofDomains := SpoofDomainsForCountry("de"); !slices.Equal(spoofDomains, []string{"global.example", "shared.example"}) {
		t.Fatalf("de list = %v, expected the global list", spoofDomains)
	}

	if countryCodes := spoofCountryCodes(); !slices.Equal(countryCodes, []string{"ru"}) {
		t.Fatalf("country lists = %v, expected ru alone", countryCodes)
	}
	expectedAllSpoofDomains := []string{"global.example", "shared.example", "one.ru.example", "two.ru.example"}
	if allSpoofDomains := AllSpoofDomains(); !slices.Equal(allSpoofDomains, expectedAllSpoofDomains) {
		t.Fatalf("all names = %v, expected %v", allSpoofDomains, expectedAllSpoofDomains)
	}

	restore()
	if spoofDomains := SpoofDomainsForCountry("ru"); !slices.Equal(spoofDomains, []string{"global.example", "shared.example"}) {
		t.Fatalf("restored ru list = %v, expected the global list", spoofDomains)
	}
}

// The wire name of the country is the server's contract.
func TestExtenderHintResultCarriesTheCountry(t *testing.T) {
	result := &ExtenderHintResult{}
	if err := json.Unmarshal([]byte(`{"continent_code": "EU", "country_code": "ru"}`), result); err != nil {
		t.Fatal(err)
	}
	if result.ContinentCode != "EU" || result.CountryCode != "ru" {
		t.Fatalf("hint = %+v", result)
	}
}

// The directory's country: the operator's while its hint is current, the
// network country the host reports while it is not, and the operator's last
// answer when the host reports none. The feed and probe dials draw from the
// list of that country.
func TestExtenderDirectorySpoofCountryCode(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	t.Cleanup(setSpoofDomainsForTest([]string{"global.example"}))
	installTestCountrySpoofLists(t, map[string][]string{
		"ru": {"one.ru.example"},
		"de": {"one.de.example"},
	})
	directory := NewExtenderDirectory(context.Background(), DefaultExtenderDirectorySettings())
	defer directory.Close()

	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("a new directory places the client in %q", countryCode)
	}

	SetNetworkCountryCode("RU")
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("with no hint the country = %q, expected the network country", countryCode)
	}
	if spoofDomains, countryCode := directorySpoofDomains(directory); !slices.Equal(spoofDomains, []string{"one.ru.example"}) ||
		countryCode != "ru" {
		t.Fatalf("with no hint the list = %v (%q), expected the ru list", spoofDomains, countryCode)
	}

	// the operator's answer is the rule while it is current
	if !directory.SetCountryHint("DE") {
		t.Fatal("the first country did not change the hint")
	}
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("with a current hint the country = %q, expected the operator's", countryCode)
	}
	if spoofDomains, _ := directorySpoofDomains(directory); !slices.Equal(spoofDomains, []string{"one.de.example"}) {
		t.Fatalf("with a current hint the list = %v, expected the de list", spoofDomains)
	}

	// a failed hint or a path change: the host's report stands in
	directory.ExpireCountryHint()
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("with a stale hint the country = %q, expected the network country", countryCode)
	}
	// and with no report, the operator's last answer is all there is
	SetNetworkCountryCode("")
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("with a stale hint and no report the country = %q, expected the last answer", countryCode)
	}

	// an operator that cannot place the client keeps the last answer, stale
	SetNetworkCountryCode("ru")
	directory.SetCountryHint("DE")
	if directory.SetCountryHint("") {
		t.Fatal("an empty answer changed the country")
	}
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("after an empty answer the country = %q, expected the network country", countryCode)
	}

	// a nil directory places nothing
	if spoofDomains, countryCode := directorySpoofDomains(nil); !slices.Equal(spoofDomains, []string{"global.example"}) ||
		countryCode != "" {
		t.Fatalf("a nil directory list = %v (%q), expected the global list", spoofDomains, countryCode)
	}
}

// The override of the bug: while the hint endpoint answers, its country is in
// force whatever the host reports; once it cannot be reached, the network
// country the host reports is.
//
// The test runs on the hint loop harness (newTestHintLoop) in a synctest
// bubble: a pass at a time, each settling the refresh and hint loops before
// anything is read. It used to poll for the country and read the continent at
// once, which failed when the read was between the two (refreshHint applies
// the country first). And it moved the clock as soon as the continent had
// landed, while the hint loop could still be about to record the read with
// the clock it then read (Answer), which left the next read never due.
func TestExtenderNetworkClientFallsBackToTheNetworkCountryWhenTheHintFails(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		setTestNetworkCountryCode(t, "RU")
		hint := newTestExtenderHint(&ExtenderHintResult{ContinentCode: "EU", CountryCode: "DE"})
		loop := newTestHintLoop(t, hint.Hint, nil)
		settings := DefaultExtenderNetworkClientSettings()

		// the first read has ended: the operator's country and continent are
		// in force
		synctest.Wait()
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "de" {
			t.Fatalf("spoof country = %q, expected the operator's", countryCode)
		}
		if continentCode := loop.directory.ContinentHint(); continentCode != "EU" {
			t.Fatalf("continent = %q, expected the operator's", continentCode)
		}

		// the operator is now unreachable; the next refresh finds out
		hint.Fail(fmt.Errorf("the operator is unreachable in this test"))
		count := hint.Count()
		loop.clock.advance(settings.RebootstrapTimeout)
		loop.pass(t)
		if readCount := hint.Count(); readCount != count+1 {
			t.Fatalf("hint reads = %d, expected the refresh to read it again", readCount)
		}
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "ru" {
			t.Fatalf("spoof country = %q, expected the network country", countryCode)
		}

		// the operator answers again: the first pass after the failure's
		// backoff reads it, and its country is back in force
		hint.Answer(&ExtenderHintResult{ContinentCode: "EU", CountryCode: "DE"})
		loop.clock.advance(settings.HintMinBackoff)
		loop.pass(t)
		if countryCode := loop.directory.SpoofCountryCode(); countryCode != "de" {
			t.Fatalf("spoof country = %q, expected the operator's again", countryCode)
		}
	})
}

// A path change makes the hint's country stale at once -- it placed the
// address of the old path -- and asks the operator again without waiting out
// the refresh period, so a phone that moves onto a network that refuses every
// operator address takes the network country for it from the first dial.
func TestExtenderNetworkClientAsksForTheHintAgainAfterAPathChange(t *testing.T) {
	setTestNetworkCountryCode(t, "ru")
	clock := newTestClock()
	hint := newTestExtenderHint(&ExtenderHintResult{ContinentCode: "EU", CountryCode: "de"})
	networkClient, directory, _ := newTestExtenderNetworkClient(t, clock, func(settings *ExtenderNetworkClientSettings) {
		settings.Hint = hint.Hint
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, nil
		}
	})
	waitForSpoofCountryCode(t, directory, "de")
	count := hint.Count()
	// no refresh is due: the answer holds on this path
	time.Sleep(50 * time.Millisecond)
	if hint.Count() != count || directory.SpoofCountryCode() != "de" {
		t.Fatalf("the hint was asked again (%d, %q) with nothing due", hint.Count(), directory.SpoofCountryCode())
	}

	hint.Fail(fmt.Errorf("no operator address is routable on this path"))
	networkClient.networkChanged()
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("after the path change the country = %q, expected the network country", countryCode)
	}
	// the clock has not moved: only the path change can have asked
	hint.waitForCount(t, count+1)
	if countryCode := directory.SpoofCountryCode(); countryCode != "ru" {
		t.Fatalf("after the failed hint the country = %q, expected the network country", countryCode)
	}
}

// The strategy's extender dialers take their outer names from the list of the
// directory's country, and when the client leaves that country, the dialers
// drawn from its list are dropped and their addresses drawn again from the
// list now in force. A country with no list of its own is the global list, so
// moving to one redraws nothing.
func TestClientStrategyDrawsExtenderNamesFromTheCountryList(t *testing.T) {
	setTestNetworkCountryCode(t, "ru")
	clock := newTestClock()
	// the strategy fixture installs the global list "spoof.example"
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	ruSpoofDomains := []string{"one.ru.example", "two.ru.example"}
	installTestCountrySpoofLists(t, map[string][]string{"ru": ruSpoofDomains})

	ip := netip.MustParseAddr("192.0.2.100")
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

	serverNames := func(dialers []*clientDialer) []string {
		names := []string{}
		for _, dialer := range dialers {
			names = append(names, dialer.extenderConfig.Profile.ServerName)
		}
		return names
	}

	expandedDialers := clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 3 {
		t.Fatalf("dialers = %d, expected one per carrier", len(expandedDialers))
	}
	for _, serverName := range serverNames(expandedDialers) {
		if !slices.Contains(ruSpoofDomains, serverName) {
			t.Fatalf("dialer name = %q, expected a name of the ru list", serverName)
		}
	}

	// the client leaves the country
	SetNetworkCountryCode("")
	expandedDialers = clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 3 {
		t.Fatalf("redrawn dialers = %d, expected the address drawn again", len(expandedDialers))
	}
	dialers := testExtenderDialers(clientStrategy)
	if len(dialers) != 3 {
		t.Fatalf("dialers = %d, expected the ru dialers dropped", len(dialers))
	}
	for _, serverName := range serverNames(dialers) {
		if serverName != "spoof.example" {
			t.Fatalf("dialer name = %q, expected the global list", serverName)
		}
	}

	// a country with no list of its own changes no list
	SetNetworkCountryCode("de")
	if expandedDialers := clientStrategy.expandExtenderDialers(); len(expandedDialers) != 0 {
		t.Fatalf("a country without a list redrew %d dialers", len(expandedDialers))
	}
	if dialers := testExtenderDialers(clientStrategy); len(dialers) != 3 {
		t.Fatalf("dialers = %d, expected the global dialers kept", len(dialers))
	}
}
