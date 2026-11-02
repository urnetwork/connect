package connect

// Root-cause tests for the tcp camouflage client capability and the Phase B
// eligibility check (EXTENDER-UPDATE2 §3.2).

import (
	"testing"
	"time"
)

// A measured, current borrow list for a country, with one selectable name.
func measuredBorrowList(countryCode string, now time.Time) *ExtenderBorrowList {
	return &ExtenderBorrowList{
		CountryCode:          countryCode,
		Version:              3,
		Measured:             true,
		MeasuredAt:           now.Add(-time.Hour),
		MaxMeasurementAge:    30 * 24 * time.Hour,
		RevalidationInterval: 24 * time.Hour,
		Names:                []ExtenderBorrowName{{Name: "front.example"}},
	}
}

// A client with a measured, current borrow list and the Chrome hello claims
// tcp-camouflage-v1 (§3.2).
func TestExtenderClientCapabilitiesReadyClaimsCamouflage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{measuredBorrowList("", now)})
	defer restore()

	result := extenderTcpCamouflageReadinessAt(&ConnectSettings{}, "", now)
	if !result.Capable || result.Reason != ExtenderTcpCamouflageReadyOk {
		t.Fatalf("readiness = %+v, expected capable ok", result)
	}
	capabilities := ExtenderClientCapabilities(&ConnectSettings{}, "")
	if len(capabilities) != 1 || capabilities[0] != ExtenderCapabilityTcpCamouflageV1 {
		t.Fatalf("capabilities = %v, expected [%s]", capabilities, ExtenderCapabilityTcpCamouflageV1)
	}
}

// The "go" kill switch drops the capability: a client that sends the Go-TLS
// hello cannot authenticate, so it must not be issued Phase B records (§3.2).
func TestExtenderClientCapabilitiesFingerprintOverrideDropsCamouflage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{measuredBorrowList("", now)})
	defer restore()

	settings := &ConnectSettings{TlsClientHelloFingerprint: TlsClientHelloFingerprintGo}
	result := extenderTcpCamouflageReadinessAt(settings, "", now)
	if result.Capable || result.Reason != ExtenderTcpCamouflageReadyFingerprintOverride {
		t.Fatalf("readiness = %+v, expected not capable fingerprint_override", result)
	}
	if capabilities := ExtenderClientCapabilities(settings, ""); len(capabilities) != 0 {
		t.Fatalf("capabilities = %v, expected empty under the go kill switch", capabilities)
	}
}

// An unknown fingerprint value is not the Chrome hello either, so it drops the
// capability (conservative reading of §3.2).
func TestExtenderClientCapabilitiesUnknownFingerprintDropsCamouflage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{measuredBorrowList("", now)})
	defer restore()

	settings := &ConnectSettings{TlsClientHelloFingerprint: "firefox"}
	if result := extenderTcpCamouflageReadinessAt(settings, "", now); result.Capable {
		t.Fatalf("readiness = %+v, expected an unknown fingerprint to drop the capability", result)
	}
}

// With no borrowed name there is nothing to front, so the client does not claim
// the capability — the empty-resource state on origin/main today (§3.2).
func TestExtenderClientCapabilitiesNoBorrowListDropsCamouflage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{})
	defer restore()

	result := extenderTcpCamouflageReadinessAt(&ConnectSettings{}, "", now)
	if result.Capable || result.Reason != ExtenderTcpCamouflageReadyNoBorrowList {
		t.Fatalf("readiness = %+v, expected not capable no_borrow_list", result)
	}
}

// An unmeasured borrow list (names but no manifest) is not camouflage
// compatible: it may front names a cohort no longer covers (§3.1, §3.2).
func TestExtenderClientCapabilitiesUnmeasuredListIncompatible(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	unmeasured := &ExtenderBorrowList{Names: []ExtenderBorrowName{{Name: "front.example"}}}
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{unmeasured})
	defer restore()

	result := extenderTcpCamouflageReadinessAt(&ConnectSettings{}, "", now)
	if result.Capable || result.Reason != ExtenderTcpCamouflageReadyBorrowListIncompatible {
		t.Fatalf("readiness = %+v, expected not capable borrow_list_incompatible", result)
	}
}

// A measured list past its maximum measurement age is stale, so the client
// stops claiming the capability until the resource is refreshed (§3.1, §3.3).
func TestExtenderClientCapabilitiesStaleListDropsCamouflage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	stale := measuredBorrowList("", now)
	stale.MeasuredAt = now.Add(-48 * time.Hour)
	stale.MaxMeasurementAge = 24 * time.Hour
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{stale})
	defer restore()

	result := extenderTcpCamouflageReadinessAt(&ConnectSettings{}, "", now)
	if result.Capable || result.Reason != ExtenderTcpCamouflageReadyBorrowListStale {
		t.Fatalf("readiness = %+v, expected not capable borrow_list_stale", result)
	}
}

// A client in a country selects its country's list; a country with no list of
// its own falls back to the global list (§3.1).
func TestExtenderClientCapabilitiesCountryList(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	restore := setExtenderCamouflageBorrowListsForTest([]*ExtenderBorrowList{
		measuredBorrowList("", now),
		measuredBorrowList("ru", now),
	})
	defer restore()

	// a country with its own measured list is ready
	if result := extenderTcpCamouflageReadinessAt(&ConnectSettings{}, "ru", now); !result.Capable {
		t.Fatalf("ru readiness = %+v, expected capable", result)
	}
	// a country with no list of its own uses the global list, which is measured
	if result := extenderTcpCamouflageReadinessAt(&ConnectSettings{}, "cn", now); !result.Capable {
		t.Fatalf("cn readiness = %+v, expected capable via the global list", result)
	}
}

// The eligibility check: a required capability this build knows and the client
// offered is satisfied; an unmet or unknown requirement is not; no requirement
// is always satisfied (§3.2, contract R).
func TestExtenderCapabilitiesSatisfied(t *testing.T) {
	offered := []string{ExtenderCapabilityTcpCamouflageV1}
	cases := []struct {
		name     string
		required []string
		offered  []string
		want     bool
	}{
		{"no requirement", nil, nil, true},
		{"met", []string{ExtenderCapabilityTcpCamouflageV1}, offered, true},
		{"unmet", []string{ExtenderCapabilityTcpCamouflageV1}, []string{}, false},
		{"unknown required never met", []string{"future-capability-v9"}, []string{"future-capability-v9"}, false},
		{"unknown offered ignored", []string{ExtenderCapabilityTcpCamouflageV1}, []string{"junk", ExtenderCapabilityTcpCamouflageV1}, true},
		{"case and space insensitive", []string{" TCP-Camouflage-V1 "}, []string{"tcp-camouflage-v1"}, true},
		{"empty requirement ignored", []string{""}, []string{}, true},
	}
	for _, testCase := range cases {
		if got := ExtenderCapabilitiesSatisfied(testCase.required, testCase.offered); got != testCase.want {
			t.Errorf("%s: satisfied = %t, want %t", testCase.name, got, testCase.want)
		}
	}
}

// A hostile offered list is bounded work and cannot smuggle a match past the
// count bound: a known capability past the count limit is not read (§3.2,
// never leak fleet details / bounded inputs).
func TestExtenderCapabilitiesSatisfiedBoundsOfferedList(t *testing.T) {
	offered := make([]string, 0, extenderMaxOfferedCapabilityCount+1)
	for i := 0; i < extenderMaxOfferedCapabilityCount; i += 1 {
		offered = append(offered, "filler")
	}
	offered = append(offered, ExtenderCapabilityTcpCamouflageV1)
	if ExtenderCapabilitiesSatisfied([]string{ExtenderCapabilityTcpCamouflageV1}, offered) {
		t.Fatal("a capability past the offered-count bound was read")
	}
}
