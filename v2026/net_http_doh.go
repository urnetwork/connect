package connect

import (
	"context"
	"errors"
	"strings"
	"time"
	// "crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"

	"golang.org/x/exp/maps"
	"golang.org/x/net/idna"

	"golang.org/x/net/http2"
	// "github.com/urnetwork/glog/v2026"
)

// FIXME DoH certs need to be included in the pinned certs

func DefaultDohSettings() *DohSettings {
	return &DohSettings{
		ConnectSettings:     *DefaultConnectSettings(),
		IpVersion:           4,
		MissExpiration:      300 * time.Second,
		LocalExpiration:     300 * time.Second,
		CacheMaxEntries:     4096,
		DnsResolverSettings: DefaultDnsResolverSettings(),
	}
}

// the resolver tries the following sequence until there is a found record:
// 1. if enable remote doh, remote doh
// 2. if enable remote dns, remote dns
// 3. if enable local dns, local dns
// see:
// https://developers.cloudflare.com/1.1.1.1/ip-addresses/
// https://www.quad9.net/
// https://support.opendns.com/hc/en-us/articles/360038086532-Using-DNS-over-HTTPS-DoH-with-OpenDNS
func DefaultDnsResolverSettings() *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh: true,
		EnableLocalDns:  true,
		RemoteDohUrlsIpv4: []string{
			"https://1.1.1.1/dns-query",
		},
		LocalDnsIpv4: []string{
			"1.1.1.1",
		},
	}
}

type DohSettings struct {
	ConnectSettings
	IpVersion           int
	MissExpiration      time.Duration
	LocalExpiration     time.Duration
	CacheMaxEntries     int
	DnsResolverSettings *DnsResolverSettings
}

func (self *DohSettings) ResolverIp() string {
	switch self.IpVersion {
	case 4:
		return "ip4"
	case 6:
		return "ip6"
	default:
		return "ip"
	}
}

type DnsResolverSettings struct {
	EnableRemoteDoh   bool     `json:"enable_remote_doh,omitempty"`
	EnableRemoteDns   bool     `json:"enable_remote_dns,omitempty"`
	EnableLocalDns    bool     `json:"enable_local_dns,omitempty"`
	RemoteDohUrlsIpv4 []string `json:"remote_doh_urls_ipv4,omitempty"`
	RemoteDohUrlsIpv6 []string `json:"remote_doh_urls_ipv6,omitempty"`
	RemoteDnsIpv4     []string `json:"remote_dns_ipv4,omitempty"`
	RemoteDnsIpv6     []string `json:"remote_dns_ipv6,omitempty"`
	LocalDnsIpv4      []string `json:"local_dns_ipv4,omitempty"`
	LocalDnsIpv6      []string `json:"local_dns_ipv6,omitempty"`
}

func httpClientWithSettings(settings *DohSettings) *http.Client {
	tr := &http.Transport{
		DialContext:         settings.DialContext,
		TLSHandshakeTimeout: settings.TlsTimeout,
		// FIXME add the doh server certs to our pinned certs
		// TLSClientConfig:     settings.TlsConfig,
	}
	// most doh providers discontinued http1.1 late 2025
	// we force h2 instead of the default h1->h2 autonegotiate,
	// since that no longer works
	// see https://quad9.net/news/blog/doh-http-1-1-retirement/
	err := http2.ConfigureTransport(tr)
	if err != nil {
		panic(err)
	}
	httpClient := &http.Client{
		Timeout:   settings.RequestTimeout,
		Transport: tr,
	}
	return httpClient
}

type DohCache struct {
	httpClient     *http.Client
	remoteResolver *net.Resolver
	localResolver  *net.Resolver
	settings       *DohSettings
	log            Logger

	stateLock             sync.Mutex
	queryResultExpiration map[DohKey]*DohResult
}

func dnsResolverAddrs(settings *DohSettings, remote bool, network string) []string {
	var ipv4 []string
	var ipv6 []string
	if remote {
		ipv4 = settings.DnsResolverSettings.RemoteDnsIpv4
		ipv6 = settings.DnsResolverSettings.RemoteDnsIpv6
	} else {
		ipv4 = settings.DnsResolverSettings.LocalDnsIpv4
		ipv6 = settings.DnsResolverSettings.LocalDnsIpv6
	}

	switch {
	case strings.HasSuffix(network, "6") || settings.IpVersion == 6:
		if 0 < len(ipv6) {
			return ipv6
		}
		return ipv4
	case strings.HasSuffix(network, "4") || settings.IpVersion == 4:
		if 0 < len(ipv4) {
			return ipv4
		}
		return ipv6
	default:
		addrs := append([]string{}, ipv4...)
		return append(addrs, ipv6...)
	}
}

func netIPAddr(ip net.IP) (netip.Addr, bool) {
	if ip4 := ip.To4(); ip4 != nil {
		addr, ok := netip.AddrFromSlice(ip4)
		return addr, ok
	}
	if ip16 := ip.To16(); ip16 != nil {
		addr, ok := netip.AddrFromSlice(ip16)
		return addr, ok
	}
	return netip.Addr{}, false
}

func authoritativeDnsMiss(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func NewDohCache(settings *DohSettings) *DohCache {
	remoteResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			localAddrs := dnsResolverAddrs(settings, true, network)
			if len(localAddrs) == 0 {
				return nil, fmt.Errorf("no remote DNS resolvers configured")
			}
			localAddr := localAddrs[mathrand.Intn(len(localAddrs))]
			addr = net.JoinHostPort(localAddr, port)
			return settings.DialContext(ctx, network, addr)
		},
	}

	netDialer := settings.NetDialer()
	localResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			localAddrs := dnsResolverAddrs(settings, false, network)
			if len(localAddrs) == 0 {
				return nil, fmt.Errorf("no local DNS resolvers configured")
			}
			localAddr := localAddrs[mathrand.Intn(len(localAddrs))]
			addr = net.JoinHostPort(localAddr, port)
			return netDialer.DialContext(ctx, network, addr)
		},
	}

	return &DohCache{
		httpClient:            httpClientWithSettings(settings),
		remoteResolver:        remoteResolver,
		localResolver:         localResolver,
		settings:              settings,
		log:                   loggerOrDefault(settings.Log),
		queryResultExpiration: map[DohKey]*DohResult{},
	}
}

func (self *DohCache) pruneCacheLocked(now time.Time, reserve int) {
	for key, result := range self.queryResultExpiration {
		if !result.Valid(now, self.settings.MissExpiration) {
			delete(self.queryResultExpiration, key)
		}
	}

	maxEntries := self.settings.CacheMaxEntries
	for maxEntries < len(self.queryResultExpiration)+reserve {
		var oldestKey DohKey
		var oldestTime time.Time
		found := false
		for key, result := range self.queryResultExpiration {
			if !found || result.Time.Before(oldestTime) {
				oldestKey = key
				oldestTime = result.Time
				found = true
			}
		}
		if !found {
			return
		}
		delete(self.queryResultExpiration, oldestKey)
	}
}

func (self *DohCache) Query(ctx context.Context, recordType string, domain string) []netip.Addr {
	q := NewDohKey(recordType, domain)

	now := time.Now()

	var r *DohResult
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		r = self.queryResultExpiration[q]
		if r != nil && !r.Valid(now, self.settings.MissExpiration) {
			delete(self.queryResultExpiration, q)
			r = nil
		}
	}()
	if r != nil {
		return r.Addrs()
	}

	addrExpirations := map[netip.Addr]time.Time{}
	cacheMiss := false

	if self.settings.DnsResolverSettings.EnableRemoteDoh {
		queryResult := dohQueryWithClientResult(ctx, self.httpClient, self.settings.IpVersion, q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(time.Duration(ttlSeconds) * time.Second)
		}
		if len(addrExpirations) == 0 && queryResult.Miss {
			cacheMiss = true
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableRemoteDns {
		// try the remote resolver
		resolvedIps, err := self.remoteResolver.LookupIP(ctx, self.settings.ResolverIp(), q.Domain)
		if err == nil {
			found := false
			for _, ip := range resolvedIps {
				if addr, ok := netIPAddr(ip); ok {
					addrExpirations[addr] = now.Add(self.settings.LocalExpiration)
					found = true
				}
			}
			if !found {
				cacheMiss = true
			}
		} else if authoritativeDnsMiss(err) {
			cacheMiss = true
		} else if log := self.log.V(2); log.Enabled() {
			log.Infof("[doh]remote (%s) err = %s\n", q.Domain, err)
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableLocalDns {
		// try the local resolver
		resolvedIps, err := self.localResolver.LookupIP(ctx, self.settings.ResolverIp(), q.Domain)
		if err == nil {
			found := false
			for _, ip := range resolvedIps {
				if addr, ok := netIPAddr(ip); ok {
					addrExpirations[addr] = now.Add(self.settings.LocalExpiration)
					found = true
				}
			}
			if !found {
				cacheMiss = true
			}
		} else if authoritativeDnsMiss(err) {
			cacheMiss = true
		} else if log := self.log.V(2); log.Enabled() {
			log.Infof("[doh]local (%s) err = %s\n", q.Domain, err)
		}
	}

	if ctx.Err() == nil && (0 < len(addrExpirations) || cacheMiss) {
		r = &DohResult{
			Time:            now,
			AddrExpirations: addrExpirations,
			Miss:            cacheMiss && len(addrExpirations) == 0,
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			self.pruneCacheLocked(now, 1)
			self.queryResultExpiration[q] = r
		}()
	}

	return (&DohResult{
		Time:            now,
		AddrExpirations: addrExpirations,
	}).Addrs()
}

func DohQueryWithDefaults(ctx context.Context, recordType string, domains ...string) map[netip.Addr]int {
	return DohQuery(ctx, 0, recordType, DefaultDohSettings(), domains...)
}

// return ip -> ttl (seconds)
// use `ipVersion=0` to try all versions
func DohQuery(ctx context.Context, ipVersion int, recordType string, settings *DohSettings, domains ...string) map[netip.Addr]int {
	httpClient := httpClientWithSettings(settings)
	defer httpClient.CloseIdleConnections()

	return DohQueryWithClient(
		ctx,
		httpClient,
		ipVersion,
		recordType,
		settings,
		domains...,
	)
}

func DohQueryWithClient(
	ctx context.Context,
	httpClient *http.Client,
	ipVersion int,
	recordType string,
	settings *DohSettings,
	domains ...string,
) map[netip.Addr]int {
	return dohQueryWithClientResult(ctx, httpClient, ipVersion, recordType, settings, domains...).AddrTtls
}

type dohQueryResult struct {
	AddrTtls map[netip.Addr]int
	Miss     bool
}

func newDohQueryResult() *dohQueryResult {
	return &dohQueryResult{
		AddrTtls: map[netip.Addr]int{},
	}
}

func dohQueryWithClientResult(
	ctx context.Context,
	httpClient *http.Client,
	ipVersion int,
	recordType string,
	settings *DohSettings,
	domains ...string,
) *dohQueryResult {
	// run all the queries in parallel to all servers

	queryCtx, queryCancel := context.WithCancel(ctx)
	defer queryCancel()

	switch recordType {
	case "A", "AAAA":
	default:
		return newDohQueryResult()
	}

	query := func(dohUrl string, domain string) *dohQueryResult {
		result := newDohQueryResult()

		name, err := Punycode(domain)
		if err != nil {
			return result
		}

		params := url.Values{}
		params.Add("name", name)
		params.Add("type", recordType)

		requestUrl := fmt.Sprintf("%s?%s", dohUrl, params.Encode())

		request, err := http.NewRequestWithContext(queryCtx, "GET", requestUrl, nil)
		if err != nil {
			return result
		}

		request.Header.Set("Accept", "application/dns-json")
		// note, we do not set the User-Agent for DoH requests
		// see https://bugzilla.mozilla.org/show_bug.cgi?id=1543201#c4

		response, err := httpClient.Do(request)
		if err != nil {
			return result
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return result
		}

		data, err := io.ReadAll(response.Body)
		if err != nil {
			return result
		}

		dohResponse := &DohResponse{}
		err = json.Unmarshal(data, dohResponse)
		if err != nil {
			return result
		}

		switch dohResponse.Status {
		case 0:
		case 3:
			result.Miss = true
			return result
		default:
			return result
		}

		// ips := []netip.Addr{}
		for _, answer := range dohResponse.Answer {
			if ip, err := netip.ParseAddr(answer.Data); err == nil {
				// ips = append(ips, ip)
				result.AddrTtls[ip] = max(result.AddrTtls[ip], answer.TTL)
			}
		}
		if len(result.AddrTtls) == 0 {
			result.Miss = true
		}

		return result
	}

	var dohUrls []string
	switch ipVersion {
	case 4:
		dohUrls = settings.DnsResolverSettings.RemoteDohUrlsIpv4
	case 6:
		dohUrls = settings.DnsResolverSettings.RemoteDohUrlsIpv6
	default:
		// the resolver can race
		dohUrls = append(dohUrls, settings.DnsResolverSettings.RemoteDohUrlsIpv4...)
		dohUrls = append(dohUrls, settings.DnsResolverSettings.RemoteDohUrlsIpv6...)
	}

	queryCount := len(dohUrls) * len(domains)
	if queryCount == 0 || settings.RequestTimeout <= 0 {
		return newDohQueryResult()
	}
	receiveResults := make(chan *dohQueryResult, queryCount)
	for _, dohUrl := range dohUrls {
		for _, domain := range domains {
			go HandleError(func() {
				result := query(dohUrl, domain)
				select {
				case receiveResults <- result:
				case <-queryCtx.Done():
				}
			})
		}
	}

	endTime := time.Now().Add(settings.RequestTimeout)
	mergedResult := newDohQueryResult()
	for range queryCount {
		timeout := endTime.Sub(time.Now())
		if timeout <= 0 {
			return &dohQueryResult{
				AddrTtls: mergedResult.AddrTtls,
			}
		}
		select {
		case <-queryCtx.Done():
			return &dohQueryResult{
				AddrTtls: mergedResult.AddrTtls,
			}
		case result := <-receiveResults:
			maps.Copy(mergedResult.AddrTtls, result.AddrTtls)
			if result.Miss {
				mergedResult.Miss = true
			}
		case <-time.After(timeout):
			return &dohQueryResult{
				AddrTtls: mergedResult.AddrTtls,
			}
		}
	}
	mergedResult.Miss = len(mergedResult.AddrTtls) == 0 && mergedResult.Miss
	return mergedResult
}

type DohKey struct {
	RecordType string
	Domain     string
}

func NewDohKey(recordType string, domain string) DohKey {
	return DohKey{
		RecordType: strings.ToUpper(recordType),
		Domain:     strings.ToLower(domain),
	}
}

type DohResult struct {
	Time            time.Time
	AddrExpirations map[netip.Addr]time.Time
	Miss            bool
}

func (self *DohResult) Valid(now time.Time, missExpiration time.Duration) bool {
	if len(self.AddrExpirations) == 0 {
		return self.Miss && !self.Time.Add(missExpiration).Before(now)
	}
	for _, expireTime := range self.AddrExpirations {
		if expireTime.Before(now) {
			return false
		}
	}
	return true
}

func (self *DohResult) Addrs() []netip.Addr {
	ips := []netip.Addr{}
	for ip := range self.AddrExpirations {
		ips = append(ips, ip)
	}
	return ips
}

type DohQuestion struct {
	Name string `json:"name"`
	Type int    `json:"type"`
}

type DohAnswer struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	TTL  int    `json:"TTL"`
	Data string `json:"data"`
}

type DohResponse struct {
	Status   int           `json:"Status"`
	TC       bool          `json:"TC"`
	RD       bool          `json:"RD"`
	RA       bool          `json:"RA"`
	AD       bool          `json:"AD"`
	CD       bool          `json:"CD"`
	Question []DohQuestion `json:"Question"`
	Answer   []DohAnswer   `json:"Answer"`
}

func Punycode(domain string) (string, error) {
	name := strings.TrimSpace(domain)

	return idna.New(
		idna.MapForLookup(),
		idna.Transitional(true),
		idna.StrictDomainName(false),
	).ToASCII(name)
}
