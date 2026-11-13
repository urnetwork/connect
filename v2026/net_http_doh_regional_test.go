package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026/protocol"
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

// ru's recommendation is Yandex.DNS and the National DNS (NSDI) public
// resolvers a.res-nsdi.ru and b.res-nsdi.ru, never NSDI's authoritative root
// server a.auth-nsdi.ru (195.208.6.1), which offers no recursion.
func TestRuRecommendationNamesNsdiResolvers(t *testing.T) {
	if got, want := RegionalDnsServerIps("ru"), []string{"77.88.8.8", "195.208.4.1", "195.208.5.1"}; !slices.Equal(got, want) {
		t.Errorf("ru v4 recommendation = %v, want %v", got, want)
	}
	if got, want := RegionalDnsServerIpv6s("ru"), []string{"2a02:6b8::feed:0ff"}; !slices.Equal(got, want) {
		t.Errorf("ru v6 recommendation = %v, want %v", got, want)
	}
	nsdiAuthoritative := []string{"195.208.6.1", "2a0c:a9c7:a::1"}
	for _, server := range RegionalDnsServers() {
		if slices.Contains(nsdiAuthoritative, server.Ipv4) || slices.Contains(nsdiAuthoritative, server.Ipv6) {
			t.Errorf("%s %s names NSDI's authoritative root server: %s %s", server.CountryCode, server.Name, server.Ipv4, server.Ipv6)
		}
	}
}

// Serves one stream-framed dns query on conn (a non-packet conn, so the go
// resolver frames as over tcp) with the answer the dialed server gives:
// a recursive resolver answers a name in names with an A record and any
// other name with NXDOMAIN, and NSDI's authoritative root server 195.208.6.1
// answers as measured from it on 2026-11-11, with a referral to the TLD's
// servers, glue in the additional section, and neither the authoritative nor
// the recursion-available flag.
func serveRegionalDnsQuery(t *testing.T, conn net.Conn, server string, names []string, answer [4]byte) {
	defer conn.Close()
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		t.Error(err)
		return
	}
	payload := make([]byte, int(binary.BigEndian.Uint16(size[:])))
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Error(err)
		return
	}
	var query dnsmessage.Message
	if err := query.Unpack(payload); err != nil || len(query.Questions) != 1 {
		t.Error("invalid synthetic query")
		return
	}
	question := query.Questions[0]
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: query.RecursionDesired},
		Questions: query.Questions,
	}
	if server == "195.208.6.1" {
		nameServer := dnsmessage.MustNewName("a0.tld-servers.example.")
		response.Authorities = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("example."), Type: dnsmessage.TypeNS, Class: dnsmessage.ClassINET, TTL: 172800},
			Body:   &dnsmessage.NSResource{NS: nameServer},
		}}
		response.Additionals = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: nameServer, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 172800},
			Body:   &dnsmessage.AResource{A: [4]byte{199, 19, 56, 1}},
		}}
	} else if slices.Contains(names, question.Name.String()) {
		response.Header.RecursionAvailable = true
		response.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.AResource{A: answer},
		}}
	} else {
		response.Header.RecursionAvailable = true
		response.Header.RCode = dnsmessage.RCodeNameError
	}
	encoded, err := response.Pack()
	if err != nil {
		t.Error(err)
		return
	}
	binary.BigEndian.PutUint16(size[:], uint16(len(encoded)))
	if _, err := conn.Write(append(size[:], encoded...)); err != nil {
		t.Error(err)
	}
}

// Every lookup with ru's recommendation resolves, whichever of its servers the
// plain dns path asks: each lookup dials the next server of the list in turn,
// so with 195.208.6.1 in the list every other lookup reached it, read its
// referral as no such host, and cached the name as missing. The dialer serves
// each server as measured, so nothing leaves the process. The names are
// rooted, so the host's resolv.conf search list adds no lookups.
func TestRuRecommendationResolvesThroughEveryServer(t *testing.T) {
	resolver := RegionalDnsResolverSettings("ru")
	settings := DefaultDohSettings()
	settings.IpVersion = 4
	settings.DnsResolverSettings = resolver
	answer := [4]byte{192, 0, 2, 41}
	names := []string{}
	for i := range len(resolver.RemoteDnsIpv4) + 1 {
		names = append(names, fmt.Sprintf("ru-lookup-%d.example.", i))
	}
	var dialLock sync.Mutex
	dialed := []string{}
	var serving sync.WaitGroup
	settings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "53" || !strings.HasPrefix(network, "udp") {
			return nil, errors.New("unexpected regional dns dial")
		}
		dialLock.Lock()
		dialed = append(dialed, host)
		dialLock.Unlock()
		client, server := net.Pipe()
		serving.Add(1)
		go func() {
			defer serving.Done()
			serveRegionalDnsQuery(t, server, host, names, answer)
		}()
		return client, nil
	}}
	cache := NewDohCache(settings)
	defer cache.Close()

	for i, name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		addrs, authoritative := cache.QueryResult(ctx, "A", name)
		cancel()
		dialLock.Lock()
		server := dialed[len(dialed)-1]
		dialLock.Unlock()
		if !authoritative || len(addrs) != 1 || addrs[0] != netip.AddrFrom4(answer) {
			t.Fatalf("lookup %d through %s failed: addrs=%v authoritative=%t", i, server, addrs, authoritative)
		}
	}
	serving.Wait()
	dialLock.Lock()
	defer dialLock.Unlock()
	for _, server := range resolver.RemoteDnsIpv4 {
		if !slices.Contains(dialed, server) {
			t.Fatalf("server %s was never asked: %v", server, dialed)
		}
	}
}

// The resolvers the 2026-11-11 measurements found unable to answer an exit in
// their country, which no recommendation may name again: 403.online's
// 10.202.10.10, a private address no query through the tunnel reaches; the
// resolvers of TTNET (195.175.39.39, 195.175.39.40) and Turkcell Superonline
// (212.252.114.8), which answer only their own networks' customers; and
// Turkmentelecom's (217.174.238.141), which refuses clients outside its
// network and answers the platform's own names with 127.0.0.1. The addresses
// are the operators' published resolver constants, the subject of the test,
// not live configuration.
var testRegionalDnsServersUnableToAnswerExits = []string{
	"10.202.10.10",
	"195.175.39.39",
	"195.175.39.40",
	"212.252.114.8",
	"217.174.238.141",
}

// The countries of the tests below: every connection region, the countries
// whose recommendation changed on 2026-11-11, and undisclosed.
func testRegionalDnsCountryCodes() []string {
	countryCodes := slices.Concat(IngressCountryCodes(), []string{"ir", "tr", "tm", ""})
	slices.Sort(countryCodes)
	return slices.Compact(countryCodes)
}

// Every regional server, as a udp query to port 53 from a public relationship,
// passes both roles' egress policy: the client's, which drops what it refuses
// before it leaves the device, and the provider's. A server the policy refuses
// can never answer the egress recommendation through the tunnel. ir named
// 403.online's private 10.202.10.10 until 2026-11-11, an incident in both
// roles.
func TestRegionalDnsServersPassTheEgressPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	query := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 1, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	payload, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	policies := testConsumerEgressPolicies(t, ctx, "198.51.100.0/24")
	for _, server := range RegionalDnsServers() {
		for _, address := range []string{server.Ipv4, server.Ipv6} {
			if address == "" {
				continue
			}
			addr, err := netip.ParseAddr(address)
			if err != nil {
				t.Errorf("%s %s names %q, which is not an address", server.CountryCode, server.Name, address)
				continue
			}
			if !isPublicUnicastAddr(addr) {
				t.Errorf("%s %s names %s, which is not public", server.CountryCode, server.Name, address)
			}
			version := 4
			if addr.Is6() {
				version = 6
			}
			for role, policy := range policies {
				path := testEgressPath(version, address)
				path.Protocol = IpProtocolUdp
				path.DestinationPort = 53
				path.Syn = false
				if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, path, payload); r != SecurityPolicyResultAllow {
					t.Errorf("%s %s: %s policy judges a udp query to %s:53 %s, want allow", server.CountryCode, server.Name, role, address, r)
				}
			}
		}
	}
}

// ir's recommendation is Shecan and Electro, in that order, v4 only: neither
// publishes a v6 resolver.
func TestIrRecommendationIsShecanAndElectro(t *testing.T) {
	if got, want := RegionalDnsServerIps("ir"), []string{"178.22.122.100", "78.157.42.100"}; !slices.Equal(got, want) {
		t.Errorf("ir v4 recommendation = %v, want %v", got, want)
	}
	if got := RegionalDnsServerIpv6s("ir"); len(got) != 0 {
		t.Errorf("ir v6 recommendation = %v, want none", got)
	}
	if strategy := IngressDnsStrategyForCountry("ir"); !slices.Equal(strategy.PlainDnsIpv4, []string{"178.22.122.100", "78.157.42.100"}) || len(strategy.PlainDnsIpv6) != 0 {
		t.Errorf("ir ingress plain dns = %v %v, want the recommendation", strategy.PlainDnsIpv4, strategy.PlainDnsIpv6)
	}
}

// tr and tm have no recommendation and are no connection region: a user who
// declared one is undisclosed, with the defaults' bootstrap plain servers, and
// the tunnel uses the default resolvers. tr's was dropped on 2026-11-11
// because DoH works in Turkey (owner decision), so the apps never suggest
// unencrypted dns on a Turkish exit; tm's because nothing could be verified
// from inside Turkmenistan.
func TestTrAndTmHaveNoRecommendation(t *testing.T) {
	for _, countryCode := range []string{"tr", "tm"} {
		if resolver := RegionalDnsResolverSettings(countryCode); resolver != nil {
			t.Errorf("%s recommendation = %+v, want none", countryCode, resolver)
		}
		if ipv4, ipv6 := RegionalDnsServerIps(countryCode), RegionalDnsServerIpv6s(countryCode); 0 < len(ipv4) || 0 < len(ipv6) {
			t.Errorf("%s regional servers = %v %v, want none", countryCode, ipv4, ipv6)
		}
		for _, server := range RegionalDnsServers() {
			if server.CountryCode == countryCode {
				t.Errorf("the regional servers name %s's %s %s", countryCode, server.Name, server.Ipv4)
			}
		}
		if slices.Contains(IngressCountryCodes(), countryCode) {
			t.Errorf("%s is a connection region: %v", countryCode, IngressCountryCodes())
		}
		strategy := IngressDnsStrategyForCountry(countryCode)
		if 0 < len(strategy.DohUrlsIpv4) || 0 < len(strategy.DohUrlsIpv6) || 0 < len(strategy.PlainDnsIpv4) || 0 < len(strategy.PlainDnsIpv6) {
			t.Errorf("%s ingress strategy = %+v, want the defaults", countryCode, strategy)
		}
		for _, network := range []string{"udp", "udp4", "udp6", "tcp"} {
			if got, want := bootstrapPlainDnsServers(IngressDohSettings(countryCode, nil, nil), network), bootstrapPlainDnsServers(IngressDohSettings("", nil, nil), network); !slices.Equal(got, want) {
				t.Errorf("%s bootstrap plain dns %s = %v, want the undisclosed %v", countryCode, network, got, want)
			}
		}
	}
}

// No regional recommendation, bootstrap preset, ingress strategy or bootstrap
// plain fallback of any country names a resolver that could not answer an
// exit in its country (testRegionalDnsServersUnableToAnswerExits).
func TestNoRegionalListNamesAServerUnableToAnswerExits(t *testing.T) {
	lists := map[string][]string{}
	for _, server := range RegionalDnsServers() {
		lists["regionalDnsServers."+server.CountryCode+"."+server.Name] = []string{server.Ipv4, server.Ipv6}
	}
	for _, countryCode := range testRegionalDnsCountryCodes() {
		prefix := "[" + countryCode + "]"
		for name, list := range testResolverLists(prefix+"RegionalDnsResolverSettings", RegionalDnsResolverSettings(countryCode)) {
			lists[name] = list
		}
		strategy := IngressDnsStrategyForCountry(countryCode)
		lists[prefix+"IngressDnsStrategy.PlainDnsIpv4"] = strategy.PlainDnsIpv4
		lists[prefix+"IngressDnsStrategy.PlainDnsIpv6"] = strategy.PlainDnsIpv6
		for _, network := range []string{"udp", "udp4", "udp6", "tcp"} {
			lists[prefix+"bootstrapPlainDnsServers."+network] = bootstrapPlainDnsServers(IngressDohSettings(countryCode, nil, nil), network)
		}
	}
	for name, list := range lists {
		for _, server := range list {
			if slices.Contains(testRegionalDnsServersUnableToAnswerExits, server) {
				t.Errorf("%s names %s, which cannot answer an exit in its country", name, server)
			}
		}
	}
}

// Every lookup through every country's recommendation resolves on the first
// server it asks, from an exit on any network of the country. The dialer
// models such an exit as measured on 2026-11-11: a server that is not public
// never answers (the egress policy drops the query before it leaves the
// device), and neither does a resolver that answers only its own network's
// customers (testRegionalDnsServersUnableToAnswerExits); every other server
// answers. The plain dns path takes the next server for each resolver attempt,
// so with tr's TTNET and Superonline no lookup resolved, and with ir's
// 10.202.10.10 every other lookup spent an attempt, a resolver timeout on a
// device, before it reached Shecan. The names are rooted, so the host's
// resolv.conf search list adds no lookups, and nothing leaves the process.
func TestRegionalRecommendationsResolveOnTheFirstServerFromAnyExit(t *testing.T) {
	answer := [4]byte{192, 0, 2, 42}
	for _, countryCode := range testRegionalDnsCountryCodes() {
		resolver := RegionalDnsResolverSettings(countryCode)
		if resolver == nil {
			continue
		}
		settings := DefaultDohSettings()
		settings.IpVersion = 4
		settings.DnsResolverSettings = resolver
		names := []string{}
		for i := range 2*len(resolver.RemoteDnsIpv4) + 1 {
			names = append(names, fmt.Sprintf("%s-lookup-%d.example.", countryCode, i))
		}
		var dialLock sync.Mutex
		dialed := []string{}
		var serving sync.WaitGroup
		settings.DialContextSettings = &DialContextSettings{DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "53" || !strings.HasPrefix(network, "udp") {
				return nil, errors.New("unexpected regional dns dial")
			}
			dialLock.Lock()
			dialed = append(dialed, host)
			dialLock.Unlock()
			addr, err := netip.ParseAddr(host)
			if err != nil || !isPublicUnicastAddr(addr) || slices.Contains(testRegionalDnsServersUnableToAnswerExits, host) {
				return nil, &net.OpError{Op: "read", Net: network, Err: errors.New("i/o timeout")}
			}
			client, server := net.Pipe()
			serving.Add(1)
			go func() {
				defer serving.Done()
				serveRegionalDnsQuery(t, server, host, names, answer)
			}()
			return client, nil
		}}
		cache := NewDohCache(settings)
		for i, name := range names {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			addrs, authoritative := cache.QueryResult(ctx, "A", name)
			cancel()
			if !authoritative || len(addrs) != 1 || addrs[0] != netip.AddrFrom4(answer) {
				t.Errorf("%s lookup %d did not resolve: addrs=%v authoritative=%t", countryCode, i, addrs, authoritative)
			}
		}
		cache.Close()
		serving.Wait()
		dialLock.Lock()
		if len(dialed) != len(names) {
			t.Errorf("%s: %d lookups dialed %d times, want one dial each: %v", countryCode, len(names), len(dialed), dialed)
		}
		for _, server := range resolver.RemoteDnsIpv4 {
			if !slices.Contains(dialed, server) {
				t.Errorf("%s: server %s was never asked: %v", countryCode, server, dialed)
			}
		}
		dialLock.Unlock()
	}
}
