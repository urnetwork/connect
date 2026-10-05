// Builds the bundled spoof domain resource (EXTENDER.md A10) from a plain
// text list, so operations can fill the list without the names appearing in
// the binary as plain strings.
//
//	go run ./scripts/extender_spoof -in domains.txt -out res/extender_spoof.bin
//
// The input is one domain per line; `#` comments and blank lines are ignored.
//
// `-in` is required. A run with no input would otherwise write the empty
// resource over the shipped list, which is a silent loss: the build still
// succeeds and every client stops fronting its extender dials. Pass `-empty`
// to ask for the empty resource on purpose.
//
// `-country <cc>` builds the list of one country instead, written to
// res/extender_spoof_<cc>.bin unless `-out` names another file. A client the
// directory places in that country fronts its dials with it, and every other
// client with the global list (connect.SpoofDomainsForCountry):
//
//	go run ./scripts/extender_spoof -country ru -in ru_domains.txt
//
// A country list cannot be empty: an empty one would only fall back to the
// global list, so the file is removed instead.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/urnetwork/connect"
)

func main() {
	inPath := flag.String("in", "", "plain text domain list, one per line; required unless -empty")
	outPath := flag.String("out", "", "resource file to write; res/extender_spoof.bin, or res/extender_spoof_<cc>.bin with -country, when empty")
	countryCode := flag.String("country", "", "ISO 3166-1 alpha-2 code of the country whose own list this is; empty for the global list")
	empty := flag.Bool("empty", false, "write the empty resource, which disables random extender discovery")
	flag.Parse()

	resourcePath, err := spoofResourceOutPath(*outPath, *countryCode, *empty)
	if err == nil {
		err = writeSpoofResource(*inPath, resourcePath, *empty)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		flag.Usage()
		os.Exit(1)
	}
}

// The file a run writes: `-out` when given, else the path the connect module
// reads the list by -- the global list's, or the country's with `-country`
// (connect.SpoofResourcePath). A country that is not two letters is refused
// rather than written under a name nothing reads, and so is an empty country
// list, which would only fall back to the global list.
func spoofResourceOutPath(outPath string, countryCode string, empty bool) (string, error) {
	normalizedCountryCode := connect.NormalizeSpoofCountryCode(countryCode)
	switch {
	case strings.TrimSpace(countryCode) != "" && normalizedCountryCode == "":
		return "", fmt.Errorf("-country %q is not an ISO 3166-1 alpha-2 code", countryCode)
	case normalizedCountryCode != "" && empty:
		return "", fmt.Errorf("an empty list for %s would only fall back to the global list; remove %s instead", normalizedCountryCode, connect.SpoofResourcePath(normalizedCountryCode))
	case outPath != "":
		return outPath, nil
	}
	return connect.SpoofResourcePath(normalizedCountryCode), nil
}

// Reads the list and writes the resource. An input path and `-empty` are
// mutually exclusive, and neither is refused rather than guessed: the caller
// that wanted a list would silently ship none.
func writeSpoofResource(inPath string, outPath string, empty bool) error {
	switch {
	case inPath == "" && !empty:
		return fmt.Errorf("extender_spoof needs -in <domain list>, or -empty to write the empty resource")
	case inPath != "" && empty:
		return fmt.Errorf("extender_spoof takes -in or -empty, not both")
	}

	plainText := []byte{}
	if inPath != "" {
		var err error
		if plainText, err = os.ReadFile(inPath); err != nil {
			return fmt.Errorf("read %s: %w", inPath, err)
		}
	}

	spoofDomains := connect.ParseSpoofDomains(plainText)
	if len(spoofDomains) == 0 && !empty {
		return fmt.Errorf("%s holds no domains; pass -empty to write the empty resource", inPath)
	}
	resource, err := connect.EncodeSpoofDomainsResource(spoofDomains)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := os.WriteFile(outPath, resource, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("wrote %s with %d domains (%d bytes)\n", outPath, len(spoofDomains), len(resource))
	return nil
}
