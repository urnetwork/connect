package connect

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
)

// The bundled spoof domain list (EXTENDER.md A10).
//
// A client dial fronts an extender with one of these names, so the names must
// not appear in the binary as plain strings where a scanner can lift the whole
// list out of a shipped app. The list is therefore carried as a gzip stream
// masked with a repeating key stored in the same resource, decoded once on
// first use. Masking is obfuscation, not secrecy: anyone who runs the code can
// recover the list, which is expected, since a probe can also discover any one
// name by connecting.
//
// The content is an operator decision, rebuilt with scripts/extender_spoof. A
// resource that decodes to nothing leaves every dial with no outer name at all
// rather than the operator's, which is the deliberate fallback of A10, so a
// list that is missing or unreadable degrades instead of leaking.

// The resource is `mask || xor(gzip(one domain per line), repeat(mask))`.
const extenderSpoofMaskByteCount = 8

// A country may have a list of its own (SpoofDomainsForCountry), bundled as
// `res/extender_spoof_<cc>.bin` in the same form. It is for a network that
// routes only the names of its own country -- a whitelist-only mobile network
// -- where the names of the global list are refused on sight. A dial takes the
// list of the country its directory places it in
// (ExtenderDirectory.SpoofCountryCode), and the global list when that country
// has none. No country list is bundled yet: what a country's network lets
// through has to be measured before its list can be written.
//
// The global list and every country list are embedded as one set of files and
// read by name, so the global resource is not embedded a second time.
//
//go:embed res/extender_spoof*.bin
var extenderSpoofResources embed.FS

// The bundled global list in its resource form.
var extenderSpoofResource, _ = extenderSpoofResources.ReadFile(SpoofResourcePath(""))

var (
	spoofDomainsOnce      sync.Once
	spoofDomainsStateLock sync.Mutex
	spoofDomainsValues    []string
	// the files the country lists are read from: the embedded resources,
	// unless a test installed others
	spoofCountryResources fs.FS = extenderSpoofResources
	// the decoded country lists by country code, read on first use. A country
	// with no resource, or one that does not decode, holds an empty list, so
	// it is looked up once
	spoofCountryDomainsValues = map[string][]string{}
)

// The bundled spoof domain list, decoded on first use. The result is a copy
// the caller may keep. An unreadable resource yields an empty list rather than
// failing a dial: a missing spoof list only disables random discovery.
func SpoofDomains() []string {
	spoofDomainsOnce.Do(func() {
		spoofDomains, err := DecodeSpoofDomainsResource(extenderSpoofResource)
		if err != nil {
			spoofDomains = []string{}
		}
		spoofDomainsStateLock.Lock()
		defer spoofDomainsStateLock.Unlock()
		if spoofDomainsValues == nil {
			spoofDomainsValues = spoofDomains
		}
	})
	spoofDomainsStateLock.Lock()
	defer spoofDomainsStateLock.Unlock()
	return slices.Clone(spoofDomainsValues)
}

// Installs a synthetic list in place of the bundled one and returns the
// restore. Tests use it so no real domain appears in the repository.
func setSpoofDomainsForTest(spoofDomains []string) func() {
	// load the bundled list first, so the restore puts the real list back and
	// a later SpoofDomains does not overwrite the override. Taking the once
	// with nothing loaded would save a nil previous and leave every caller
	// after the restore with an empty list, for the rest of the process
	SpoofDomains()
	spoofDomainsStateLock.Lock()
	defer spoofDomainsStateLock.Unlock()
	previousSpoofDomains := spoofDomainsValues
	spoofDomainsValues = slices.Clone(spoofDomains)
	return func() {
		spoofDomainsStateLock.Lock()
		defer spoofDomainsStateLock.Unlock()
		spoofDomainsValues = previousSpoofDomains
	}
}

// The resource path of a country's list, `res/extender_spoof_<cc>.bin`, and
// of the global list, `res/extender_spoof.bin`, for a code that does not
// normalize. The generator writes to it and the embedded set is read by it, so
// the two cannot name a list differently.
func SpoofResourcePath(countryCode string) string {
	if countryCode = NormalizeSpoofCountryCode(countryCode); countryCode != "" {
		return fmt.Sprintf("res/extender_spoof_%s.bin", countryCode)
	}
	return "res/extender_spoof.bin"
}

// The lower case ISO 3166-1 alpha-2 form a country's list is named by, and ""
// for anything that is not two letters: an empty or malformed code is the
// global list, never part of a file name.
func NormalizeSpoofCountryCode(countryCode string) string {
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	if len(countryCode) != 2 {
		return ""
	}
	for i := 0; i < len(countryCode); i += 1 {
		if countryCode[i] < 'a' || 'z' < countryCode[i] {
			return ""
		}
	}
	return countryCode
}

// The spoof list of one country: its own bundled list when it has one, and
// the global list (SpoofDomains) otherwise -- for a country with no list, for
// an empty or malformed code, and for a country list that is empty or does
// not decode, since an empty country list would leave a dial with no outer
// name where the global list has one. The result is a copy the caller may
// keep.
func SpoofDomainsForCountry(countryCode string) []string {
	spoofDomains, _ := spoofDomainsForCountry(countryCode)
	return spoofDomains
}

// The list SpoofDomainsForCountry answers, and the country whose list it is:
// the normalized code for a country's own list, "" for the global list. Two
// countries that both fall back answer the same "", which is what lets a
// strategy tell a change of list from a change of country.
func spoofDomainsForCountry(countryCode string) ([]string, string) {
	if countryCode = NormalizeSpoofCountryCode(countryCode); countryCode != "" {
		if spoofDomains := countrySpoofDomains(countryCode); 0 < len(spoofDomains) {
			return spoofDomains, countryCode
		}
	}
	return SpoofDomains(), ""
}

// A country's bundled list, decoded on first use; empty when there is none.
// The code is already normalized.
func countrySpoofDomains(countryCode string) []string {
	spoofDomainsStateLock.Lock()
	defer spoofDomainsStateLock.Unlock()

	spoofDomains, ok := spoofCountryDomainsValues[countryCode]
	if !ok {
		spoofDomains = []string{}
		if resource, err := fs.ReadFile(spoofCountryResources, SpoofResourcePath(countryCode)); err == nil {
			if decodedSpoofDomains, err := DecodeSpoofDomainsResource(resource); err == nil {
				spoofDomains = decodedSpoofDomains
			}
		}
		spoofCountryDomainsValues[countryCode] = spoofDomains
	}
	return slices.Clone(spoofDomains)
}

// The countries with a bundled list, in order.
func spoofCountryCodes() []string {
	resources := func() fs.FS {
		spoofDomainsStateLock.Lock()
		defer spoofDomainsStateLock.Unlock()
		return spoofCountryResources
	}()
	resourceDir := path.Dir(SpoofResourcePath(""))
	entries, err := fs.ReadDir(resources, resourceDir)
	if err != nil {
		return []string{}
	}
	countryCodes := []string{}
	for _, entry := range entries {
		countryCode := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "extender_spoof_"), ".bin")
		// only the name the path function gives a country is a country list;
		// the global resource normalizes to no country at all
		if countryCode == "" || NormalizeSpoofCountryCode(countryCode) != countryCode ||
			path.Join(resourceDir, entry.Name()) != SpoofResourcePath(countryCode) {
			continue
		}
		countryCodes = append(countryCodes, countryCode)
	}
	return countryCodes
}

// Every bundled name: the global list, then each country's list in country
// order, deduplicated. An extender proxies the real site of any of them (A5),
// so a prober that replays the name a client of any country fronted with sees
// that site rather than a refusal, wherever the extender runs.
func AllSpoofDomains() []string {
	allSpoofDomains := SpoofDomains()
	visited := map[string]bool{}
	for _, spoofDomain := range allSpoofDomains {
		visited[spoofDomain] = true
	}
	for _, countryCode := range spoofCountryCodes() {
		for _, spoofDomain := range countrySpoofDomains(countryCode) {
			if !visited[spoofDomain] {
				visited[spoofDomain] = true
				allSpoofDomains = append(allSpoofDomains, spoofDomain)
			}
		}
	}
	return allSpoofDomains
}

// Installs other files in place of the embedded country lists and returns the
// restore. A test writes a country's resource with EncodeSpoofDomainsResource
// and reads it back through SpoofDomainsForCountry, so no real domain appears
// in the repository. The global list is not read from them;
// setSpoofDomainsForTest replaces that one.
func setSpoofCountryResourcesForTest(resources fs.FS) func() {
	spoofDomainsStateLock.Lock()
	defer spoofDomainsStateLock.Unlock()
	previousResources := spoofCountryResources
	previousSpoofDomainsValues := spoofCountryDomainsValues
	spoofCountryResources = resources
	spoofCountryDomainsValues = map[string][]string{}
	return func() {
		spoofDomainsStateLock.Lock()
		defer spoofDomainsStateLock.Unlock()
		spoofCountryResources = previousResources
		spoofCountryDomainsValues = previousSpoofDomainsValues
	}
}

// Normalizes the plain text form: one domain per line, `#` comments and blank
// lines dropped, lowercased, deduplicated in first-seen order.
func ParseSpoofDomains(plainText []byte) []string {
	spoofDomains := []string{}
	visited := map[string]bool{}
	for _, line := range strings.Split(string(plainText), "\n") {
		if i := strings.IndexByte(line, '#'); 0 <= i {
			line = line[0:i]
		}
		spoofDomain := strings.ToLower(strings.TrimSpace(line))
		if spoofDomain == "" || visited[spoofDomain] {
			continue
		}
		visited[spoofDomain] = true
		spoofDomains = append(spoofDomains, spoofDomain)
	}
	return spoofDomains
}

// Builds the embedded resource from a domain list. The generator command uses
// it; tests use it with the decoder to pin the round trip.
func EncodeSpoofDomainsResource(spoofDomains []string) ([]byte, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	for _, spoofDomain := range spoofDomains {
		if _, err := writer.Write([]byte(spoofDomain + "\n")); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	mask := make([]byte, extenderSpoofMaskByteCount)
	if _, err := rand.Read(mask); err != nil {
		return nil, err
	}
	compressedBytes := compressed.Bytes()
	resource := make([]byte, extenderSpoofMaskByteCount+len(compressedBytes))
	copy(resource[0:extenderSpoofMaskByteCount], mask)
	for i, b := range compressedBytes {
		resource[extenderSpoofMaskByteCount+i] = b ^ mask[i%extenderSpoofMaskByteCount]
	}
	return resource, nil
}

// Recovers the domain list from the embedded resource form.
func DecodeSpoofDomainsResource(resource []byte) ([]string, error) {
	if len(resource) < extenderSpoofMaskByteCount {
		return nil, fmt.Errorf("spoof resource is %d bytes, expected at least %d", len(resource), extenderSpoofMaskByteCount)
	}
	mask := resource[0:extenderSpoofMaskByteCount]
	maskedBytes := resource[extenderSpoofMaskByteCount:]
	compressedBytes := make([]byte, len(maskedBytes))
	for i, b := range maskedBytes {
		compressedBytes[i] = b ^ mask[i%extenderSpoofMaskByteCount]
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressedBytes))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	plainText, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return ParseSpoofDomains(plainText), nil
}
