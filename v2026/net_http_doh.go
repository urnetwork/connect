package connect

import (
	"context"
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

	stateLock             sync.Mutex
	queryResultExpiration map[DohKey]*DohResult
}

func NewDohCache(settings *DohSettings) *DohCache {
	remoteResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			var localAddrs []string
			switch network {
			case "ip6":
				localAddrs = settings.DnsResolverSettings.RemoteDnsIpv6
			default:
				// the dialer can't race so use ipv4
				localAddrs = settings.DnsResolverSettings.RemoteDnsIpv4
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
			var localAddrs []string
			switch network {
			case "ip6":
				localAddrs = settings.DnsResolverSettings.LocalDnsIpv6
			default:
				// the dialer can't race so use ipv4
				localAddrs = settings.DnsResolverSettings.LocalDnsIpv4
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
		queryResultExpiration: map[DohKey]*DohResult{},
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
	}()
	if r != nil {
		ok := func() bool {
			if len(r.AddrExpirations) == 0 {
				if r.Time.Add(self.settings.MissExpiration).Before(now) {
					return false
				}
			}
			for _, expireTime := range r.AddrExpirations {
				if expireTime.Before(now) {
					return false
				}
			}
			return true
		}()
		if ok {
			ips := []netip.Addr{}
			for ip, _ := range r.AddrExpirations {
				ips = append(ips, ip)
			}
			return ips
		}
	}

	addrExpirations := map[netip.Addr]time.Time{}

	if self.settings.DnsResolverSettings.EnableLocalDns {
		addrTtls := DohQueryWithClient(ctx, self.httpClient, self.settings.IpVersion, q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range addrTtls {
			addrExpirations[addr] = now.Add(time.Duration(ttlSeconds) * time.Second)
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableRemoteDns {
		// try the remote resolver
		resolvedIps, err := self.remoteResolver.LookupIP(ctx, self.settings.ResolverIp(), q.Domain)
		if err == nil {
			for _, ip := range resolvedIps {
				addr, _ := netip.AddrFromSlice(ip.To4())

				addrExpirations[addr] = now.Add(self.settings.LocalExpiration)
			}
		} else {
			fmt.Printf("[doh]local (%s) err = %s\n", q.Domain, err)
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableLocalDns {
		// try the local resolver
		resolvedIps, err := self.localResolver.LookupIP(ctx, self.settings.ResolverIp(), q.Domain)
		if err == nil {
			for _, ip := range resolvedIps {
				addr, _ := netip.AddrFromSlice(ip.To4())

				addrExpirations[addr] = now.Add(self.settings.LocalExpiration)
			}
		} else {
			fmt.Printf("[doh]local (%s) err = %s\n", q.Domain, err)
		}
	}

	// resolve misses are not stored
	if 0 < len(addrExpirations) {
		r = &DohResult{
			Time:            now,
			AddrExpirations: addrExpirations,
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			self.queryResultExpiration[q] = r
		}()
	}

	ips := []netip.Addr{}
	for ip, _ := range addrExpirations {
		ips = append(ips, ip)
	}
	return ips
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
	// run all the queries in parallel to all servers

	queryCtx, queryCancel := context.WithCancel(ctx)
	defer queryCancel()

	switch recordType {
	case "A", "AAAA":
	default:
		return map[netip.Addr]int{}
	}

	query := func(dohUrl string, domain string) (result map[netip.Addr]int) {
		result = map[netip.Addr]int{}

		name, err := Punycode(domain)
		if err != nil {
			return
		}

		params := url.Values{}
		params.Add("name", name)
		params.Add("type", recordType)

		requestUrl := fmt.Sprintf("%s?%s", dohUrl, params.Encode())

		request, err := http.NewRequestWithContext(queryCtx, "GET", requestUrl, nil)
		if err != nil {
			return
		}

		request.Header.Set("Accept", "application/dns-json")
		// note, we do not set the User-Agent for DoH requests
		// see https://bugzilla.mozilla.org/show_bug.cgi?id=1543201#c4

		response, err := httpClient.Do(request)
		if err != nil {
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return
		}

		data, err := io.ReadAll(response.Body)
		if err != nil {
			return
		}

		dohResponse := &DohResponse{}
		err = json.Unmarshal(data, dohResponse)
		if err != nil {
			return
		}

		if dohResponse.Status != 0 {
			return
		}

		// ips := []netip.Addr{}
		for _, answer := range dohResponse.Answer {
			if ip, err := netip.ParseAddr(answer.Data); err == nil {
				// ips = append(ips, ip)
				result[ip] = max(result[ip], answer.TTL)
			}
		}

		return
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

	var outs []chan map[netip.Addr]int

	for _, dohUrl := range dohUrls {
		for _, domain := range domains {
			out := make(chan map[netip.Addr]int)
			outs = append(outs, out)
			go HandleError(func() {
				ips := query(dohUrl, domain)
				if 0 < len(ips) {
					select {
					case out <- ips:
					case <-queryCtx.Done():
					}
				}
				close(out)
			})
		}
	}

	endTime := time.Now().Add(settings.RequestTimeout)
	mergedIps := map[netip.Addr]int{}
	for _, out := range outs {
		timeout := endTime.Sub(time.Now())
		if timeout <= 0 {
			select {
			case <-queryCtx.Done():
			case ips, ok := <-out:
				if ok {
					maps.Copy(mergedIps, ips)
				}
			default:
			}
		} else {
			select {
			case <-queryCtx.Done():
			case ips, ok := <-out:
				if ok {
					maps.Copy(mergedIps, ips)
				}
			case <-time.After(timeout):
			}
		}
	}
	return mergedIps
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
