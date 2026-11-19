package connect

import (
	"encoding/json"
	"testing"
)

// ForceMinimum is a deliberate bootstrap mode: a newly connected provider has
// no latency or speed history until measurement traffic can reach it. Pin its
// wire name so API clients cannot silently fall back into that cold-start
// discovery deadlock.
func TestFindProviders2ArgsEncodesForceMinimum(t *testing.T) {
	body, err := json.Marshal(FindProviders2Args{ForceMinimum: true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["force_minimum"] != true {
		t.Fatalf("force_minimum = %#v, want true", decoded["force_minimum"])
	}
}

// Strict leaves the request unchanged on the wire until a client sets it, so
// the window's discovery, which never sets it, keeps the server's location
// fallback.
func TestFindProviders2ArgsEncodesStrictOnlyWhenSet(t *testing.T) {
	for _, strict := range []bool{false, true} {
		body, err := json.Marshal(FindProviders2Args{Strict: strict})
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		value, ok := decoded["strict"]
		if ok != strict || (strict && value != true) {
			t.Fatalf("strict=%t encoded %s", strict, body)
		}
	}
}

// A provider the server found above the requested city or region decodes
// with its match; one of the requested target decodes without.
func TestFindProvidersProviderDecodesTheFallbackMatch(t *testing.T) {
	var result FindProviders2Result
	err := json.Unmarshal([]byte(`{"providers": [
		{"client_id": "00000000-0000-0000-0000-000000000041", "estimated_bytes_per_second": 0, "has_estimated_bytes_per_second": false, "tier": 22, "match": "region"},
		{"client_id": "00000000-0000-0000-0000-000000000042", "estimated_bytes_per_second": 0, "has_estimated_bytes_per_second": false, "tier": 0}
	]}`), &result)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 2 || result.Providers[0].Match != "region" || result.Providers[1].Match != "" {
		t.Fatalf("providers = %+v", result.Providers)
	}
	body, err := json.Marshal(result.Providers[1])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["match"]; ok {
		t.Fatalf("a provider without a match encoded one: %s", body)
	}
}
