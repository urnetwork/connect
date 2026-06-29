package connect

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/maps"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/idna"

	"golang.org/x/net/http2"
	// "github.com/urnetwork/glog"
)

// FIXME DoH certs need to be included in the pinned certs

func DefaultDohSettings() *DohSettings {
	return &DohSettings{
		ConnectSettings:          *DefaultConnectSettings(),
		IpVersion:                4,
		MissExpiration:           300 * time.Second,
		LocalExpiration:          300 * time.Second,
		MinCacheTtl:              30 * time.Second,
		CacheMaxEntries:          4096,
		MaxConcurrentResolutions: 64,
		DnsResolverSettings:      DefaultDnsResolverSettings(),
	}
}

// the resolver tries the following sequence until there is a found record:
// 1. if enable remote doh, remote doh
// 2. if enable local doh, local doh (host-dialed, e.g. a sidecar resolver)
// 3. if enable remote dns, remote dns
// 4. if enable local dns, local dns
//
// the remote doh servers are queried in parallel and the first server to return records wins
// (see dohQueryWithClientResult). each server is tagged with the format it speaks: Cloudflare and
// Google use the JSON API (application/dns-json, ?name=&type=); Quad9 and OpenDNS speak RFC 8484
// wire-format on :443 — both are supported. each must present an IP-SAN cert (these do). Quad9's
// JSON :5053 endpoint is retired / port-blocked, so it is queried as wire.
// https://developers.cloudflare.com/1.1.1.1/ip-addresses/
// https://developers.google.com/speed/public-dns/docs/doh/json
func DefaultDnsResolverSettings() *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh: true,
		EnableLocalDns:  true,
		RemoteDohServersIpv4: []DohServer{
			{Url: "https://1.1.1.1/dns-query", Format: DohFormatJson},        // Cloudflare
			{Url: "https://8.8.8.8/resolve", Format: DohFormatJson},          // Google
			{Url: "https://9.9.9.9/dns-query", Format: DohFormatWire},        // Quad9 (RFC 8484)
			{Url: "https://208.67.222.222/dns-query", Format: DohFormatWire}, // OpenDNS (RFC 8484)
		},
		LocalDnsIpv4: []string{
			"1.1.1.1",
		},
	}
}

type DohSettings struct {
	ConnectSettings
	IpVersion       int
	MissExpiration  time.Duration
	LocalExpiration time.Duration
	// MinCacheTtl floors the cache lifetime of a resolved record so very-low / zero-TTL records
	// don't re-resolve (a full fan-out) on nearly every query. 0 disables the floor.
	MinCacheTtl     time.Duration
	CacheMaxEntries int
	// MaxConcurrentResolutions bounds in-flight resolutions (DohCache.resolveSem) so a burst
	// or flood of distinct names cannot fan out unbounded. 0 uses a sane default.
	MaxConcurrentResolutions int
	DnsResolverSettings      *DnsResolverSettings
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

// DohFormat is the query encoding a DoH server speaks; the client picks the request format per
// server (see dohQueryWithClientResult).
type DohFormat string

const (
	// DohFormatJson is the Google-style JSON API (GET ?name=&type=, Accept application/dns-json).
	// Cloudflare and Google support it; it is the default for an empty format and for the legacy
	// RemoteDohUrls/LocalDohUrls fields.
	DohFormatJson DohFormat = "json"
	// DohFormatWire is RFC 8484 (GET ?dns=<base64url DNS message>, Accept application/dns-message).
	// Universally supported (Cloudflare, Google, Quad9, OpenDNS, ...).
	DohFormatWire DohFormat = "wire"
)

// DohServer is a DoH endpoint tagged with the query format it speaks.
type DohServer struct {
	Url    string    `json:"url"`
	Format DohFormat `json:"format,omitempty"` // empty == DohFormatJson
}

type DnsResolverSettings struct {
	EnableRemoteDoh   bool     `json:"enable_remote_doh,omitempty"`
	EnableLocalDoh    bool     `json:"enable_local_doh,omitempty"`
	EnableRemoteDns   bool     `json:"enable_remote_dns,omitempty"`
	EnableLocalDns    bool     `json:"enable_local_dns,omitempty"`
	RemoteDohUrlsIpv4 []string `json:"remote_doh_urls_ipv4,omitempty"`
	RemoteDohUrlsIpv6 []string `json:"remote_doh_urls_ipv6,omitempty"`
	LocalDohUrlsIpv4  []string `json:"local_doh_urls_ipv4,omitempty"`
	LocalDohUrlsIpv6  []string `json:"local_doh_urls_ipv6,omitempty"`
	// the *DohServers fields carry a per-server format tag (json or RFC 8484 wire). the legacy
	// *DohUrls []string fields above are still honored and treated as json; prefer these for new
	// config and for any wire-only server (e.g. Quad9, OpenDNS).
	RemoteDohServersIpv4 []DohServer `json:"remote_doh_servers_ipv4,omitempty"`
	RemoteDohServersIpv6 []DohServer `json:"remote_doh_servers_ipv6,omitempty"`
	LocalDohServersIpv4  []DohServer `json:"local_doh_servers_ipv4,omitempty"`
	LocalDohServersIpv6  []DohServer `json:"local_doh_servers_ipv6,omitempty"`
	RemoteDnsIpv4        []string    `json:"remote_dns_ipv4,omitempty"`
	RemoteDnsIpv6        []string    `json:"remote_dns_ipv6,omitempty"`
	LocalDnsIpv4         []string    `json:"local_dns_ipv4,omitempty"`
	LocalDnsIpv6         []string    `json:"local_dns_ipv6,omitempty"`

	// TlsConfig, if set, is used by the DoH HTTP clients — production cert pinning,
	// or trusting a local server's cert in tests. Not serialized.
	TlsConfig *tls.Config `json:"-"`
}

func httpClientWithSettings(settings *DohSettings) *http.Client {
	return httpClientWithDialer(settings, settings.DialContext)
}

// httpClientWithDialer builds a DoH HTTP client over the given dialer. Remote DoH
// uses the tun dialer (settings.DialContext); local DoH uses the host dialer.
func httpClientWithDialer(settings *DohSettings, dialContext DialContextFunction) *http.Client {
	tr := &http.Transport{
		DialContext:         dialContext,
		TLSHandshakeTimeout: settings.TlsTimeout,
		// keep the (typically single) DoH connection pooled across bursts so lookups don't
		// re-pay a TCP+TLS handshake over the tunnel
		IdleConnTimeout: 5 * time.Minute,
	}
	if settings.DnsResolverSettings != nil {
		tr.TLSClientConfig = settings.DnsResolverSettings.TlsConfig
	}
	// most doh providers discontinued http1.1 late 2025; force h2 instead of the default
	// h1->h2 autonegotiate, since that no longer works.
	// see https://quad9.net/news/blog/doh-http-1-1-retirement/
	// ConfigureTransports (plural) returns the h2 transport so we can keep the connection
	// warm: ReadIdleTimeout sends keepalive PINGs while idle, which both holds the pooled
	// connection open across bursts and detects a dead tunnel so the next query re-dials
	// rather than stalling on a half-open connection.
	h2tr, err := http2.ConfigureTransports(tr)
	if err != nil {
		panic(err)
	}
	h2tr.ReadIdleTimeout = 30 * time.Second
	h2tr.PingTimeout = 15 * time.Second
	httpClient := &http.Client{
		Timeout:   settings.RequestTimeout,
		Transport: tr,
	}
	return httpClient
}

type DohCache struct {
	httpClient      *http.Client
	localHttpClient *http.Client
	remoteResolver  *net.Resolver
	localResolver   *net.Resolver
	settings        *DohSettings
	log             Logger

	stateLock             sync.Mutex
	queryResultExpiration map[DohKey]*DohResult
	// in-flight resolutions keyed by query: concurrent identical queries (retry storms, the
	// A/AAAA split, multi-client dups) coalesce onto one resolution (single-flight). guarded
	// by stateLock.
	inflight map[DohKey]*dohFlight

	// bounds concurrent resolutions so a flood of distinct names can't fan out unbounded
	resolveSem chan struct{}
}

// dohFlight is one in-flight resolution shared by every caller waiting on the same query. the
// leader resolves, sets addrs/authoritative, then closes done to release the waiters.
type dohFlight struct {
	done          chan struct{}
	addrs         []netip.Addr
	authoritative bool
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

	maxResolutions := settings.MaxConcurrentResolutions
	if maxResolutions <= 0 {
		maxResolutions = 64
	}

	return &DohCache{
		httpClient:            httpClientWithSettings(settings),
		localHttpClient:       httpClientWithDialer(settings, settings.NetDialer().DialContext),
		remoteResolver:        remoteResolver,
		localResolver:         localResolver,
		settings:              settings,
		log:                   loggerOrDefault(settings.Log),
		queryResultExpiration: map[DohKey]*DohResult{},
		inflight:              map[DohKey]*dohFlight{},
		resolveSem:            make(chan struct{}, maxResolutions),
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

// Query resolves a record to addresses, returning an empty slice both on an authoritative
// no-record answer and on a resolution failure. Use QueryResult to tell the two apart.
func (self *DohCache) Query(ctx context.Context, recordType string, domain string) []netip.Addr {
	addrs, _ := self.QueryResult(ctx, recordType, domain)
	return addrs
}

// QueryResult resolves a record and reports whether the answer was authoritative. authoritative
// is true when the resolver returned records or an authoritative no-record answer (NXDOMAIN /
// NODATA), and false when the resolution failed (timeout, ctx canceled, all resolvers errored)
// — a caller can map false+empty to SERVFAIL so a client retries instead of treating it as an
// authoritative "no address". Concurrent identical queries are coalesced onto one resolution
// (single-flight), and concurrent resolutions are bounded (MaxConcurrentResolutions).
func (self *DohCache) QueryResult(ctx context.Context, recordType string, domain string) ([]netip.Addr, bool) {
	q := NewDohKey(recordType, domain)
	now := time.Now()

	var fl *dohFlight
	var leader bool
	var hit bool
	var hitAddrs []netip.Addr
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if r := self.queryResultExpiration[q]; r != nil {
			if r.Valid(now, self.settings.MissExpiration) {
				hit = true
				hitAddrs = r.Addrs()
				return
			}
			delete(self.queryResultExpiration, q)
		}
		// single-flight: lead a new resolution for this key, or join the one already running
		if existing, ok := self.inflight[q]; ok {
			fl = existing
		} else {
			fl = &dohFlight{done: make(chan struct{})}
			self.inflight[q] = fl
			leader = true
		}
	}()
	if hit {
		// a cached entry (records or an authoritative miss) is itself authoritative
		return hitAddrs, true
	}

	if !leader {
		// a resolution for this key is already in flight; wait for it rather than firing a
		// duplicate, bounded by this caller's own ctx
		select {
		case <-fl.done:
			return fl.addrs, fl.authoritative
		case <-ctx.Done():
			return nil, false
		}
	}

	// leader: resolve once, publish to any waiters, and drop the in-flight entry
	defer func() {
		self.stateLock.Lock()
		delete(self.inflight, q)
		self.stateLock.Unlock()
		close(fl.done)
	}()
	// bound concurrent resolutions; shed (empty + non-authoritative -> SERVFAIL) if a slot is
	// not free before this caller's ctx expires
	select {
	case self.resolveSem <- struct{}{}:
		defer func() { <-self.resolveSem }()
	case <-ctx.Done():
		return nil, false
	}
	fl.addrs, fl.authoritative = self.resolve(ctx, q, now)
	return fl.addrs, fl.authoritative
}

// resolve runs the resolver chain (remote DoH -> local DoH -> remote DNS -> local DNS) for one
// query, caches an authoritative result, and returns the addresses plus whether the answer was
// authoritative. it is not single-flighted itself; QueryResult coalesces concurrent callers.
func (self *DohCache) resolve(ctx context.Context, q DohKey, now time.Time) ([]netip.Addr, bool) {
	addrExpirations := map[netip.Addr]time.Time{}
	cacheMiss := false
	minCacheTtl := self.settings.MinCacheTtl

	if self.settings.DnsResolverSettings.EnableRemoteDoh {
		queryResult := dohQueryWithClientResult(ctx, self.httpClient, remoteDohServers(self.settings, self.settings.IpVersion), self.settings.IpVersion, q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(max(time.Duration(ttlSeconds)*time.Second, minCacheTtl))
		}
		if len(addrExpirations) == 0 && queryResult.Miss {
			cacheMiss = true
		}
	}

	if len(addrExpirations) == 0 && self.settings.DnsResolverSettings.EnableLocalDoh {
		queryResult := dohQueryWithClientResult(ctx, self.localHttpClient, localDohServers(self.settings, self.settings.IpVersion), self.settings.IpVersion, q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(max(time.Duration(ttlSeconds)*time.Second, minCacheTtl))
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

	authoritative := 0 < len(addrExpirations) || cacheMiss
	if ctx.Err() == nil && authoritative {
		r := &DohResult{
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
	}).Addrs(), authoritative
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
	return dohQueryWithClientResult(ctx, httpClient, remoteDohServers(settings, ipVersion), ipVersion, recordType, settings, domains...).AddrTtls
}

func dohServersFor(ipv4 []DohServer, ipv6 []DohServer, ipVersion int) []DohServer {
	switch ipVersion {
	case 4:
		return ipv4
	case 6:
		return ipv6
	default:
		servers := append([]DohServer{}, ipv4...)
		return append(servers, ipv6...)
	}
}

// legacyDohServers adapts the format-less *DohUrls []string fields as json servers.
func legacyDohServers(urls []string) []DohServer {
	servers := make([]DohServer, len(urls))
	for i, u := range urls {
		servers[i] = DohServer{Url: u, Format: DohFormatJson}
	}
	return servers
}

// remoteDohServers/localDohServers return the tagged servers for the ip version, with the legacy
// *DohUrls fields appended as json servers.
func remoteDohServers(settings *DohSettings, ipVersion int) []DohServer {
	rs := settings.DnsResolverSettings
	servers := append([]DohServer{}, dohServersFor(rs.RemoteDohServersIpv4, rs.RemoteDohServersIpv6, ipVersion)...)
	return append(servers, dohServersFor(legacyDohServers(rs.RemoteDohUrlsIpv4), legacyDohServers(rs.RemoteDohUrlsIpv6), ipVersion)...)
}

func localDohServers(settings *DohSettings, ipVersion int) []DohServer {
	rs := settings.DnsResolverSettings
	servers := append([]DohServer{}, dohServersFor(rs.LocalDohServersIpv4, rs.LocalDohServersIpv6, ipVersion)...)
	return append(servers, dohServersFor(legacyDohServers(rs.LocalDohUrlsIpv4), legacyDohServers(rs.LocalDohUrlsIpv6), ipVersion)...)
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
	dohServers []DohServer,
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

	query := func(server DohServer, domain string) *dohQueryResult {
		name, err := Punycode(domain)
		if err != nil {
			return newDohQueryResult()
		}
		if server.Format == DohFormatWire {
			return dohQueryWire(queryCtx, httpClient, server.Url, recordType, name)
		}
		return dohQueryJson(queryCtx, httpClient, server.Url, recordType, name)
	}

	queryCount := len(dohServers) * len(domains)
	if queryCount == 0 || settings.RequestTimeout <= 0 {
		return newDohQueryResult()
	}
	receiveResults := make(chan *dohQueryResult, queryCount)
	for _, server := range dohServers {
		for _, domain := range domains {
			go HandleError(func() {
				result := query(server, domain)
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
			// fastest-record-wins: return as soon as any server returns records rather than
			// waiting for the rest, so a slow or dead server can't delay a successful lookup. an
			// authoritative miss is not short-circuited — keep collecting so a filtering
			// resolver's NXDOMAIN can't override a server that resolves the name.
			if 0 < len(mergedResult.AddrTtls) {
				return &dohQueryResult{
					AddrTtls: mergedResult.AddrTtls,
				}
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

// dohQueryJson runs a Google-style JSON DoH query (Accept application/dns-json, GET ?name=&type=).
// name must already be punycoded ascii.
func dohQueryJson(ctx context.Context, httpClient *http.Client, dohUrl string, recordType string, name string) *dohQueryResult {
	result := newDohQueryResult()

	params := url.Values{}
	params.Add("name", name)
	params.Add("type", recordType)
	requestUrl := fmt.Sprintf("%s?%s", dohUrl, params.Encode())

	request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
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
	if err := json.Unmarshal(data, dohResponse); err != nil {
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
	for _, answer := range dohResponse.Answer {
		if ip, err := netip.ParseAddr(answer.Data); err == nil {
			result.AddrTtls[ip] = max(result.AddrTtls[ip], answer.TTL)
		}
	}
	if len(result.AddrTtls) == 0 {
		result.Miss = true
	}
	return result
}

// dohQueryWire runs an RFC 8484 wire-format DoH query (Accept application/dns-message,
// GET ?dns=<base64url DNS message>). name must already be punycoded ascii.
func dohQueryWire(ctx context.Context, httpClient *http.Client, dohUrl string, recordType string, name string) *dohQueryResult {
	result := newDohQueryResult()

	dnsName, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return result
	}
	var qType dnsmessage.Type
	switch recordType {
	case "A":
		qType = dnsmessage.TypeA
	case "AAAA":
		qType = dnsmessage.TypeAAAA
	default:
		return result
	}
	// id 0 is recommended for DoH (RFC 8484 §4.1); recursion desired
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsName, Type: qType, Class: dnsmessage.ClassINET}},
	}
	wire, err := msg.Pack()
	if err != nil {
		return result
	}
	requestUrl := fmt.Sprintf("%s?dns=%s", dohUrl, base64.RawURLEncoding.EncodeToString(wire))

	request, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
	if err != nil {
		return result
	}
	request.Header.Set("Accept", "application/dns-message")

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
	return parseDohWire(data, qType)
}

// parseDohWire parses an RFC 8484 wire-format DNS response, extracting the A or AAAA answers
// matching qType. NXDOMAIN -> Miss; other non-success RCODEs -> failure (empty, not Miss).
func parseDohWire(data []byte, qType dnsmessage.Type) *dohQueryResult {
	result := newDohQueryResult()

	var p dnsmessage.Parser
	header, err := p.Start(data)
	if err != nil {
		return result
	}
	switch header.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError: // NXDOMAIN
		result.Miss = true
		return result
	default:
		return result
	}
	if err := p.SkipAllQuestions(); err != nil {
		return result
	}
	for {
		ah, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return result
		}
		switch {
		case ah.Type == dnsmessage.TypeA && qType == dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return result
			}
			ip := netip.AddrFrom4(r.A)
			result.AddrTtls[ip] = max(result.AddrTtls[ip], int(ah.TTL))
		case ah.Type == dnsmessage.TypeAAAA && qType == dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return result
			}
			ip := netip.AddrFrom16(r.AAAA)
			result.AddrTtls[ip] = max(result.AddrTtls[ip], int(ah.TTL))
		default:
			if err := p.SkipAnswer(); err != nil {
				return result
			}
		}
	}
	if len(result.AddrTtls) == 0 {
		result.Miss = true
	}
	return result
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
