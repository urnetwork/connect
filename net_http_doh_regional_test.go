package connect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
)

// Regional bootstrap DoH tests (`DohSettings.RegionalRemoteDoh`): in a country
// with presets the control-name lookups query them ahead of the defaults, and
// nothing else changes -- not other countries, not the tunnel's resolver.

// With the ingress country cn the control settings put the cn presets ahead of the
// defaults, named servers included; elsewhere, and in the default (tunnel)
// settings, the lists are as configured.
func TestRegionalRemoteDohUrls(t *testing.T) {
	presetsIpv4, _ := RegionalControlDohUrls("cn")
	if len(presetsIpv4) == 0 {
		t.Fatal("no cn presets")
	}
	defaults := DefaultDnsResolverSettings()

	controlUrls := remoteDohUrls(IngressDohSettings("cn", nil, nil), 4)
	if expected := slices.Concat(presetsIpv4, defaults.RemoteDohUrlsIpv4); !slices.Equal(controlUrls, expected) {
		t.Fatalf("cn control v4 = %v, expected %v", controlUrls, expected)
	}
	namedDohUrl := "https://192.0.2.53/dns-query"
	namedUrls := remoteDohUrls(IngressDohSettings("cn", []string{namedDohUrl}, nil), 4)
	if expected := slices.Concat(presetsIpv4, []string{namedDohUrl}, defaults.RemoteDohUrlsIpv4); !slices.Equal(namedUrls, expected) {
		t.Fatalf("cn named v4 = %v, expected %v", namedUrls, expected)
	}
	// a named preset is not repeated
	repeatedUrls := remoteDohUrls(IngressDohSettings("cn", []string{presetsIpv4[0]}, nil), 4)
	if expected := slices.Concat(presetsIpv4, defaults.RemoteDohUrlsIpv4); !slices.Equal(repeatedUrls, expected) {
		t.Fatalf("cn repeated v4 = %v, expected %v", repeatedUrls, expected)
	}
	// the cn presets are v4 only
	if v6Urls := remoteDohUrls(IngressDohSettings("cn", nil, nil), 6); !slices.Equal(v6Urls, defaults.RemoteDohUrlsIpv6) {
		t.Fatalf("cn control v6 = %v, expected %v", v6Urls, defaults.RemoteDohUrlsIpv6)
	}
	// the tunnel's resolver never takes the presets
	if tunnelUrls := remoteDohUrls(DefaultDohSettings(), 4); !slices.Equal(tunnelUrls, defaults.RemoteDohUrlsIpv4) {
		t.Fatalf("cn default v4 = %v, expected %v", tunnelUrls, defaults.RemoteDohUrlsIpv4)
	}

	for _, countryCode := range []string{"us", "ru", ""} {
		if urls := remoteDohUrls(IngressDohSettings(countryCode, nil, nil), 4); !slices.Equal(urls, defaults.RemoteDohUrlsIpv4) {
			t.Fatalf("%q control v4 = %v, expected %v", countryCode, urls, defaults.RemoteDohUrlsIpv4)
		}
	}
}

// An ingress country cn on a network that black-holes the default DoH servers (support inbox
// 7956, 7957, 7973): a control-name lookup still resolves, through a cn preset.
// The transport answers only the preset hosts, so nothing leaves the process.
func TestRegionalRemoteDohResolvesWhenDefaultsBlocked(t *testing.T) {
	presetsIpv4, _ := RegionalControlDohUrls("cn")
	presetHosts := map[string]bool{}
	for _, presetUrl := range presetsIpv4 {
		_, addr, err := ParseControlDohUrl(presetUrl)
		if err != nil {
			t.Fatal(err)
		}
		presetHosts[addr.String()] = true
	}
	answer := netip.MustParseAddr("192.0.2.10")
	transport := dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if !presetHosts[request.URL.Hostname()] {
			return nil, errors.New("blocked")
		}
		recorder := httptest.NewRecorder()
		writeDohWire(recorder, request, []netip.Addr{answer}, 60, false)
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
	client := &http.Client{Transport: transport}

	settings := IngressDohSettings("cn", nil, nil)
	settings.DohServerStagger = 0

	addrs := DohQueryWithClient(context.Background(), client, 4, "A", settings, "api.example.test")
	if _, ok := addrs[answer]; !ok {
		t.Fatalf("cn lookup = %v, expected %v from a cn preset", addrs, answer)
	}

	// outside cn the presets are never asked
	settings = IngressDohSettings("us", nil, nil)
	settings.DohServerStagger = 0
	if addrs := DohQueryWithClient(context.Background(), client, 4, "A", settings, "api.example.test"); len(addrs) != 0 {
		t.Fatalf("us lookup = %v, expected none", addrs)
	}
}
