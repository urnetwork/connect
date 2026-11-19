package connect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

// Which alt host a request is sent to (EXTENDER.md L3, L4; connect#229).
//
// The alt url of a space is `alt.<space>`, derived from its platform url. A
// request that presents a family name of the space -- the extender activation
// posts to `api-v4.<space>` and `api-v6.<space>` -- is sent to that family's
// alt name, `alt-v4.<space>` or `alt-v6.<space>`, as the pinned platform
// transports pair `connect-v4` with `alt-v4`. The family alt names carry one
// family each, so the activation proves the address of the family it names.
//
// Each case serves the request from an in-process alt on loopback, and the
// strategy's resolver answers one alt name, the one the request must reach,
// and refuses every other: a request sent to any other alt name fails at once.

const testAltFamilySpace = "space.example"

// One GET of `/hello` on `apiHost` through one dialer: the body, or why the
// request did not complete.
func testAltGetApiHost(dialer *clientDialer, apiHost string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, fmt.Sprintf("https://%s/hello", apiHost), nil)
	if err != nil {
		return "", err
	}
	response, err := dialer.HttpClient().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", response.StatusCode)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", err
	}
	return string(bodyBytes), nil
}

// One request to `apiHost` through one alt carrier of a strategy whose alt url
// is `altUrl`, where the resolver answers `resolvedAltHost` alone, with the
// address of the in-process alt that serves `apiHost`. An empty `altUrl` takes
// the fixture's own address, which is an alt url set explicitly. Reports the
// sni alt saw, and why the request failed.
func testAltFamilyGet(
	t *testing.T,
	whodis bool,
	loopbackIp string,
	apiHost string,
	altUrl string,
	resolvedAltHost string,
) (string, error) {
	t.Helper()
	altServer := newTestAltServerForApiHost(t, whodis, loopbackIp, apiHost, nil)
	if altUrl == "" {
		altUrl = altServer.altUrl
	} else {
		// the space's alt name on the fixture's port, which pins both carriers
		_, port := altUrlHostPort(altServer.altUrl)
		altUrl = fmt.Sprintf("%s:%s", altUrl, strconv.Itoa(port))
	}
	resolver := newFamilyTestResolverOwning(t, resolvedAltHost, netip.MustParseAddr(loopbackIp))
	clientStrategy := newTestAltStrategyWithSettings(t, altServer, func(settings *ClientStrategySettings) {
		settings.AltUrl = altUrl
		settings.ConnectSettings.Resolver = resolver
	})
	description := "alt h3"
	if whodis {
		description = "alt whodis"
	}
	body, err := testAltGetApiHost(testAltDialer(t, clientStrategy, description), apiHost)
	if err != nil {
		return "", err
	}
	if body != testAltBodyText {
		return "", fmt.Errorf("body = %q", body)
	}
	serverName, _ := altServer.clientHello(t)
	return serverName, nil
}

// A family api name of the alt url's own space reaches alt at the family's alt
// name, over both alt carriers and both families, presenting the family api
// name itself as the sni. Before connect#229 every api name went to the alt
// url's own host, so an activation posted to `api-v4` never reached `alt-v4`.
func TestAltDialersSendAFamilyNameToItsFamilyAltHost(t *testing.T) {
	requireTestAltIpv6Loopback(t)
	cases := []struct {
		whodis          bool
		loopbackIp      string
		apiHost         string
		resolvedAltHost string
	}{
		{whodis: false, loopbackIp: "127.0.0.1", apiHost: "api-v4." + testAltFamilySpace, resolvedAltHost: "alt-v4." + testAltFamilySpace},
		{whodis: false, loopbackIp: "::1", apiHost: "api-v6." + testAltFamilySpace, resolvedAltHost: "alt-v6." + testAltFamilySpace},
		{whodis: true, loopbackIp: "127.0.0.1", apiHost: "api-v4." + testAltFamilySpace, resolvedAltHost: "alt-v4." + testAltFamilySpace},
		{whodis: true, loopbackIp: "::1", apiHost: "api-v6." + testAltFamilySpace, resolvedAltHost: "alt-v6." + testAltFamilySpace},
		// the connect family names pair the same way, as the pinned platform
		// transports already pair them
		{whodis: false, loopbackIp: "127.0.0.1", apiHost: "connect-v4." + testAltFamilySpace, resolvedAltHost: "alt-v4." + testAltFamilySpace},
	}
	for _, c := range cases {
		serverName, err := testAltFamilyGet(
			t, c.whodis, c.loopbackIp, c.apiHost, "https://alt."+testAltFamilySpace, c.resolvedAltHost)
		if err != nil {
			t.Errorf("%s whodis=%v did not reach %s: %v", c.apiHost, c.whodis, c.resolvedAltHost, err)
			continue
		}
		if serverName != c.apiHost {
			t.Errorf("%s whodis=%v presented sni %q", c.apiHost, c.whodis, serverName)
		}
	}
}

// Every other name keeps the alt url's own host: a family-agnostic name, which
// that host serves; a name with no service label; a family name of another env
// or another space; and any name when the alt url was set explicitly, as an ip
// literal. These are what a request reached before connect#229, unchanged.
func TestAltDialersKeepTheAltHostForEveryOtherName(t *testing.T) {
	cases := []struct {
		apiHost         string
		altUrl          string
		resolvedAltHost string
	}{
		{apiHost: "api." + testAltFamilySpace, altUrl: "https://alt." + testAltFamilySpace, resolvedAltHost: "alt." + testAltFamilySpace},
		{apiHost: "connect." + testAltFamilySpace, altUrl: "https://alt." + testAltFamilySpace, resolvedAltHost: "alt." + testAltFamilySpace},
		{apiHost: "auth." + testAltFamilySpace, altUrl: "https://alt." + testAltFamilySpace, resolvedAltHost: "alt." + testAltFamilySpace},
		{apiHost: "g2-api-v4." + testAltFamilySpace, altUrl: "https://alt." + testAltFamilySpace, resolvedAltHost: "alt." + testAltFamilySpace},
		{apiHost: "api-v4.other.example", altUrl: "https://alt." + testAltFamilySpace, resolvedAltHost: "alt." + testAltFamilySpace},
		// the fixture's own address, which no resolver is asked for
		{apiHost: "api-v4." + testAltFamilySpace, altUrl: "", resolvedAltHost: "alt-v4." + testAltFamilySpace},
	}
	for _, c := range cases {
		for _, whodis := range []bool{false, true} {
			serverName, err := testAltFamilyGet(t, whodis, "127.0.0.1", c.apiHost, c.altUrl, c.resolvedAltHost)
			if err != nil {
				t.Errorf("%s with alt url %q whodis=%v did not reach alt: %v", c.apiHost, c.altUrl, whodis, err)
				continue
			}
			if serverName != c.apiHost {
				t.Errorf("%s with alt url %q whodis=%v presented sni %q", c.apiHost, c.altUrl, whodis, serverName)
			}
		}
	}
}
