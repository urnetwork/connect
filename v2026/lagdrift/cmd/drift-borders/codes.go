package main

// Natural Earth's features keyed by GeoLite2's country codes
// (lag.dev/DRIFTTEST.md G1). The key is ISO_A2_EH, lower case. Where it is
// -99, a feature has no ISO 3166-1 code of its own and the table below gives
// the codes it belongs to. Where Natural Earth draws inside one feature a
// territory GeoLite2 codes apart, the splits below give its polygons their own
// code.

import (
	"fmt"
	"slices"
	"strings"
)

// The codes of a feature with ISO_A2_EH -99, by ADM0_A3. A disputed or
// undemarcated area goes to every claimant around it, so that no claimant's
// visitors are rejected for standing in it (G3). France, Norway and Kosovo
// have ISO_A2_EH since Natural Earth 5.1; they are here for a release whose
// ISO_A2 alone would leave them -99, and a release that gives them a code must
// give these.
var exceptionCodes = map[string][]string{
	"FRA": {"fr"},
	"NOR": {"no"},
	"KOS": {"xk"},
	// Somaliland: ISO 3166's Somalia
	"SOL": {"so"},
	// Northern Cyprus, and the UN buffer zone: ISO 3166's Cyprus
	"CYN": {"cy"},
	"CNM": {"cy"},
	// the UK's sovereign base areas on Cyprus, whose villages use Cyprus's
	// networks and whose bases use the UK's
	"ESB": {"cy", "gb"},
	"WSB": {"cy", "gb"},
	// Cuba's territory at Guantanamo Bay, leased to and run by the US
	"USG": {"cu", "us"},
	// the Siachen Glacier, held by India and claimed by Pakistan
	"KAS": {"in", "pk"},
	// the Southern Patagonian Ice Field, undemarcated between Argentina and
	// Chile
	"SPI": {"ar", "cl"},
	// Bir Tawil, between Egypt and Sudan, claimed by neither
	"BRT": {"eg", "sd"},
	// the Spratly Islands, held in parts by Vietnam, the Philippines, China,
	// Malaysia and Taiwan, and claimed by Brunei too
	"PGA": {"bn", "cn", "my", "ph", "tw", "vn"},
	// Scarborough Reef, claimed by China, the Philippines and Taiwan
	"SCR": {"cn", "ph", "tw"},
	// Bajo Nuevo Bank and Serranilla Bank, Colombia's by the ICJ's 2012 ruling
	"BJN": {"co"},
	"SER": {"co"},
}

// A territory Natural Earth draws inside another feature, by the box (degrees)
// its polygons' outlines lie in.
type split struct {
	code   string
	minLon float64
	minLat float64
	maxLon float64
	maxLat float64
}

func (self split) contains(minLon float64, minLat float64, maxLon float64, maxLat float64) bool {
	return self.minLon <= minLon && maxLon <= self.maxLon && self.minLat <= minLat && maxLat <= self.maxLat
}

// GeoLite2's codes for territories inside another feature, by its ADM0_A3. A
// polygon whose outline lies in a split's box gets the split's code instead of
// the feature's. At 1:10m every split must find a polygon, so a release that
// redraws them is noticed.
var splits = map[string][]split{
	// France's overseas departments
	"FRA": {
		{code: "gf", minLon: -55, minLat: 2, maxLon: -51, maxLat: 6.5},
		{code: "gp", minLon: -62, minLat: 15.7, maxLon: -60.9, maxLat: 16.6},
		{code: "mq", minLon: -61.4, minLat: 14.3, maxLon: -60.7, maxLat: 15},
		{code: "re", minLon: 55, minLat: -21.6, maxLon: 56, maxLat: -20.7},
		{code: "yt", minLon: 44.9, minLat: -13.2, maxLon: 45.5, maxLat: -12.5},
	},
	// Svalbard (with Bear Island) and Jan Mayen; Bouvet Island
	"NOR": {
		{code: "sj", minLon: 10, minLat: 74, maxLon: 35, maxLat: 81},
		{code: "sj", minLon: -9.5, minLat: 70.5, maxLon: -7.5, maxLat: 71.5},
		{code: "bv", minLon: 3, minLat: -54.7, maxLon: 4, maxLat: -54.2},
	},
	// Bonaire, Sint Eustatius and Saba
	"NLD": {
		{code: "bq", minLon: -69, minLat: 11.5, maxLon: -62, maxLat: 18},
	},
	// the Cocos (Keeling) Islands and Christmas Island
	"IOA": {
		{code: "cc", minLon: 96.5, minLat: -12.5, maxLon: 97.2, maxLat: -11.8},
		{code: "cx", minLon: 105.4, minLat: -10.7, maxLon: 105.9, maxLat: -10.3},
	},
	// Tokelau
	"NZL": {
		{code: "tk", minLon: -173, minLat: -9.6, maxLon: -171, maxLat: -8.3},
	},
}

// The codes a feature's polygons belong to, unless a split takes one.
func featureCodes(adm0A3 string, isoA2Eh string) ([]string, error) {
	iso := strings.ToLower(strings.TrimSpace(isoA2Eh))
	valid := len(iso) == 2 && 'a' <= iso[0] && iso[0] <= 'z' && 'a' <= iso[1] && iso[1] <= 'z'
	if codes, ok := exceptionCodes[adm0A3]; ok {
		if valid && !slices.Contains(codes, iso) {
			return nil, fmt.Errorf("feature %s has ISO_A2_EH %s, but the exceptions give %v", adm0A3, isoA2Eh, codes)
		}
		return codes, nil
	}
	if !valid {
		return nil, fmt.Errorf("feature %s has ISO_A2_EH %q and no exception", adm0A3, isoA2Eh)
	}
	return []string{iso}, nil
}
