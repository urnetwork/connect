package connect

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
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

const (
	// dohServerWeightFloor keeps every server a small chance of being tried first (exploration), so
	// a server that recovers can climb back even after a streak of failures.
	dohServerWeightFloor = 0.05
	// maxDohResponseBytes caps a DoH response body read (a memory guard); a real DNS answer is tiny,
	// so this only bounds a hostile or broken server.
	maxDohResponseBytes = 64 * 1024
)

// dohServerWindows are the trailing time spans over which each server's successful resolutions are
// counted (per-window token buckets). A success falls inside every window whose span covers it, so a
// recent success is counted in more windows and weighted more — the fan-out order then favors
// servers that have resolved most recently and most often. See serverStats.
var dohServerWindows = []time.Duration{
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

func DefaultDohSettings() *DohSettings {
	return &DohSettings{
		ConnectSettings: *DefaultConnectSettings(),
		IpVersion:       4,
		MissExpiration:  300 * time.Second,
		LocalExpiration: 300 * time.Second,
		MinCacheTtl:     30 * time.Second,
		// per doh cache, so scaled by the memory budget
		CacheMaxEntries:           MemoryScaledCount(4096, 512),
		MaxConcurrentResolutions:  64,
		MaxConcurrentHttpRequests: 16,
		DohServerStagger:          750 * time.Millisecond,
		DnsResolverSettings:       DefaultDnsResolverSettings(),
	}
}

// the resolver tries the following sequence until there is a found record:
// 1. if enable remote doh, remote doh
// 2. if enable local doh, local doh (host-dialed, e.g. a sidecar resolver)
// 3. if enable remote dns, remote dns
// 4. if enable local dns, local dns
//
// the remote doh servers are queried as RFC 8484 wire-format (application/dns-message) in a
// staggered, weighted-random order (see dohClient.queryResult); the first server to return records
// wins. each must present an IP-SAN cert when addressed by IP (these do). Cloudflare, Google, Quad9,
// and OpenDNS all serve wire-format on :443 /dns-query.
// https://developers.cloudflare.com/1.1.1.1/encryption/dns-over-https/
// https://developers.google.com/speed/public-dns/docs/doh
func DefaultDnsResolverSettings() *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh: true,
		EnableLocalDns:  true,
		RemoteDohUrlsIpv4: []string{
			"https://1.1.1.1/dns-query",        // Cloudflare
			"https://8.8.8.8/dns-query",        // Google
			"https://9.9.9.9/dns-query",        // Quad9
			"https://208.67.222.222/dns-query", // OpenDNS
		},
		// remote plain-dns servers, dialed through the tunnel. remote dns
		// stays disabled by default for general resolution — while disabled,
		// the doh server names are the only names permitted to resolve over
		// it (see DohCache.resolve), so a hostname-form doh server never
		// leaks to the local resolver
		RemoteDnsIpv4: []string{
			"1.1.1.1",        // Cloudflare
			"9.9.9.9",        // Quad9
			"8.8.8.8",        // Google
			"208.67.222.222", // OpenDNS
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
	// MaxConcurrentHttpRequests hard-caps concurrent in-flight DoH HTTP requests (DohCache.httpSem),
	// the dominant memory cost under load on a constrained host (the iOS network extension). It
	// bounds the actual requests regardless of resolution count or per-server fan-out. 0 uses a sane
	// default.
	MaxConcurrentHttpRequests int
	// DohServerStagger delays launching each additional DoH server within a fan-out: the first
	// server is queried immediately and each next one only if no answer has arrived within this
	// interval, so a healthy primary answers before the redundant servers fire. 0 fans out to all
	// servers at once.
	DohServerStagger time.Duration
	// MaxServersPerQuery caps how many DoH servers a single query fans out to (in weighted
	// order, so the best recent performers are the ones tried). On a dead path every launched
	// request hangs until the deadline holding memory, so a memory-constrained host caps the
	// fan-out and relies on the weighted rotation across queries to explore the other servers.
	// 0 fans out to all servers.
	MaxServersPerQuery  int
	DnsResolverSettings *DnsResolverSettings
	// DohServerResolvedCallback, when set, is called after a doh server name
	// (the hostname of a remote doh url) resolves, with the resolved
	// addresses. the upgrade mux records these into its ip→hostname reverse
	// index, so the server addresses are excluded from the override and
	// association logic along with the server names
	// (see `UpgradeMux.recordServerNames` and `SetBlockActionIgnoreHosts`)
	DohServerResolvedCallback func(domain string, addrs []netip.Addr)
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
	EnableRemoteDoh bool `json:"enable_remote_doh,omitempty"`
	EnableLocalDoh  bool `json:"enable_local_doh,omitempty"`
	EnableRemoteDns bool `json:"enable_remote_dns,omitempty"`
	EnableLocalDns  bool `json:"enable_local_dns,omitempty"`
	// DoH server URLs, queried as RFC 8484 wire-format (GET ?dns=<base64url DNS message>,
	// Accept application/dns-message). Each must present an IP-SAN cert when addressed by IP.
	RemoteDohUrlsIpv4 []string `json:"remote_doh_urls_ipv4,omitempty"`
	RemoteDohUrlsIpv6 []string `json:"remote_doh_urls_ipv6,omitempty"`
	LocalDohUrlsIpv4  []string `json:"local_doh_urls_ipv4,omitempty"`
	LocalDohUrlsIpv6  []string `json:"local_doh_urls_ipv6,omitempty"`
	RemoteDnsIpv4     []string `json:"remote_dns_ipv4,omitempty"`
	RemoteDnsIpv6     []string `json:"remote_dns_ipv6,omitempty"`
	LocalDnsIpv4      []string `json:"local_dns_ipv4,omitempty"`
	LocalDnsIpv6      []string `json:"local_dns_ipv6,omitempty"`

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
	// remoteClient resolves over the tun (settings.DialContext); localClient over the host. Both
	// share httpSem (the global in-flight cap) and the per-server success stats.
	remoteClient   *dohClient
	localClient    *dohClient
	remoteResolver *net.Resolver
	localResolver  *net.Resolver
	settings       *DohSettings
	log            Logger

	// the hostname-form remote doh server names (lowercase). these can not
	// resolve through remote doh (circular), so they resolve over remote
	// plain dns through the tunnel even when EnableRemoteDns is false — the
	// one permitted consumer while it is disabled (see resolve)
	dohServerNames map[string]bool

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

	httpClient := httpClientWithSettings(settings)
	localHttpClient := httpClientWithDialer(settings, netDialer.DialContext)
	// one in-flight-request semaphore and one stats table shared across the remote + local clients,
	// so the cap bounds the cache's total concurrent DoH requests
	httpSem := make(chan struct{}, maxConcurrentHttpRequests(settings))
	stats := newServerStats()

	// the hostname-form remote doh server names (see the field doc)
	dohServerNames := map[string]bool{}
	if settings.DnsResolverSettings != nil {
		dohUrlLists := [][]string{
			settings.DnsResolverSettings.RemoteDohUrlsIpv4,
			settings.DnsResolverSettings.RemoteDohUrlsIpv6,
		}
		for _, dohUrls := range dohUrlLists {
			for _, dohUrl := range dohUrls {
				u, err := url.Parse(strings.TrimSpace(dohUrl))
				if err != nil {
					continue
				}
				host := u.Hostname()
				if host == "" {
					continue
				}
				if _, err := netip.ParseAddr(host); err != nil {
					dohServerNames[strings.ToLower(host)] = true
				}
			}
		}
	}

	return &DohCache{
		remoteClient:          &dohClient{httpClient: httpClient, httpSem: httpSem, stats: stats},
		localClient:           &dohClient{httpClient: localHttpClient, httpSem: httpSem, stats: stats},
		remoteResolver:        remoteResolver,
		localResolver:         localResolver,
		settings:              settings,
		log:                   loggerOrDefault(settings.Log),
		dohServerNames:        dohServerNames,
		queryResultExpiration: map[DohKey]*DohResult{},
		inflight:              map[DohKey]*dohFlight{},
		resolveSem:            make(chan struct{}, maxResolutions),
	}
}

func maxConcurrentHttpRequests(settings *DohSettings) int {
	if 0 < settings.MaxConcurrentHttpRequests {
		return settings.MaxConcurrentHttpRequests
	}
	return 16
}

// Close releases the cache's pooled DoH connections (each an h2+TLS connection with its
// buffers and, for tun-dialed paths, its gVisor endpoint — plus keepalive pings while it
// idles). An owner replacing or discarding a cache must call it; without it the connections
// linger until the idle timeout. The cache remains usable — a later query re-dials.
func (self *DohCache) Close() {
	self.remoteClient.httpClient.CloseIdleConnections()
	self.localClient.httpClient.CloseIdleConnections()
}

// ShedMemory drops the query result cache and releases the pooled connections, for the host's
// memory pressure signal. Subsequent queries re-resolve and re-dial.
func (self *DohCache) ShedMemory() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		clear(self.queryResultExpiration)
	}()
	self.Close()
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

	// a doh server name can not resolve through doh (circular). it resolves
	// over remote plain dns through the tunnel instead — permitted even when
	// EnableRemoteDns is false, so a hostname-form doh server resolves
	// remotely rather than falling through to the local resolver. remote dns
	// remains disabled for every other name.
	dohServerName := self.dohServerNames[q.Domain]

	if !dohServerName && self.settings.DnsResolverSettings.EnableRemoteDoh {
		queryResult := self.remoteClient.queryResult(ctx, remoteDohUrls(self.settings, self.settings.IpVersion), q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(max(time.Duration(ttlSeconds)*time.Second, minCacheTtl))
		}
		if len(addrExpirations) == 0 && queryResult.Miss {
			cacheMiss = true
		}
	}

	if len(addrExpirations) == 0 && !dohServerName && self.settings.DnsResolverSettings.EnableLocalDoh {
		queryResult := self.localClient.queryResult(ctx, localDohUrls(self.settings, self.settings.IpVersion), q.RecordType, self.settings, q.Domain)

		for addr, ttlSeconds := range queryResult.AddrTtls {
			addrExpirations[addr] = now.Add(max(time.Duration(ttlSeconds)*time.Second, minCacheTtl))
		}
		if len(addrExpirations) == 0 && queryResult.Miss {
			cacheMiss = true
		}
	}

	if len(addrExpirations) == 0 && (dohServerName || self.settings.DnsResolverSettings.EnableRemoteDns) {
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

	if dohServerName && 0 < len(addrExpirations) && self.settings.DohServerResolvedCallback != nil {
		// surface the server addresses (e.g. into the mux reverse index, so
		// the ignore matcher covers them alongside the server name)
		addrs := []netip.Addr{}
		for addr := range addrExpirations {
			addrs = append(addrs, addr)
		}
		HandleError(func() {
			self.settings.DohServerResolvedCallback(q.Domain, addrs)
		})
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
	// a one-shot client: bound its in-flight requests, but keep no persistent per-server stats
	// (nil stats -> uniform-random fan-out order)
	c := &dohClient{
		httpClient: httpClient,
		httpSem:    make(chan struct{}, maxConcurrentHttpRequests(settings)),
		stats:      nil,
	}
	return c.queryResult(ctx, remoteDohUrls(settings, ipVersion), recordType, settings, domains...).AddrTtls
}

func dohUrlsFor(ipv4 []string, ipv6 []string, ipVersion int) []string {
	switch ipVersion {
	case 4:
		return ipv4
	case 6:
		return ipv6
	default:
		urls := append([]string{}, ipv4...)
		return append(urls, ipv6...)
	}
}

// remoteDohUrls/localDohUrls return the configured wire-format DoH server URLs for the ip version.
func remoteDohUrls(settings *DohSettings, ipVersion int) []string {
	rs := settings.DnsResolverSettings
	return dohUrlsFor(rs.RemoteDohUrlsIpv4, rs.RemoteDohUrlsIpv6, ipVersion)
}

func localDohUrls(settings *DohSettings, ipVersion int) []string {
	rs := settings.DnsResolverSettings
	return dohUrlsFor(rs.LocalDohUrlsIpv4, rs.LocalDohUrlsIpv6, ipVersion)
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

// dohClient issues RFC 8484 wire-format queries over one HTTP client. httpSem hard-caps concurrent
// in-flight requests (shared across a DohCache's remote + local clients); stats biases the fan-out
// order toward recently-successful servers. stats may be nil (one-shot queries: uniform-random
// order, no recording).
type dohClient struct {
	httpClient *http.Client
	httpSem    chan struct{}
	stats      *serverStats
}

// queryResult resolves recordType for the given domains across dohUrls (RFC 8484 wire), returning
// as soon as any server returns records. Servers are tried in a weighted-random order biased toward
// recent success, and launched one wave per DohServerStagger so a fast primary answers before the
// rest fire — which bounds concurrent in-flight requests and skips the redundant fan-out entirely
// when an early server wins.
func (self *dohClient) queryResult(
	ctx context.Context,
	dohUrls []string,
	recordType string,
	settings *DohSettings,
	domains ...string,
) *dohQueryResult {
	switch recordType {
	case "A", "AAAA":
	default:
		return newDohQueryResult()
	}
	if len(dohUrls) == 0 || settings.RequestTimeout <= 0 {
		return newDohQueryResult()
	}

	names := make([]string, 0, len(domains))
	for _, domain := range domains {
		name, err := Punycode(domain)
		if err != nil {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return newDohQueryResult()
	}

	queryCtx, queryCancel := context.WithTimeout(ctx, settings.RequestTimeout)
	defer queryCancel()

	// weighted-random order: the best recent performers tend to fire first (and the rest are
	// skipped when an early server wins)
	ordered := self.stats.order(dohUrls)
	if 0 < settings.MaxServersPerQuery && settings.MaxServersPerQuery < len(ordered) {
		ordered = ordered[:settings.MaxServersPerQuery]
	}

	queryCount := len(ordered) * len(names)
	receiveResults := make(chan *dohQueryResult, queryCount)

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopLaunching := func() { stopOnce.Do(func() { close(stop) }) }
	defer stopLaunching()

	stagger := settings.DohServerStagger

	// launcher: start one server-wave per stagger interval (in weighted order) until an early
	// server wins (stop), the deadline passes, or every server has been launched.
	go HandleError(func() {
		for i, dohUrl := range ordered {
			if 0 < i && 0 < stagger {
				select {
				case <-time.After(stagger):
				case <-stop:
					return
				case <-queryCtx.Done():
					return
				}
			}
			for _, name := range names {
				// acquire the in-flight slot here so work waiting on the cap parks in
				// this one launcher instead of one parked goroutine per (server, name);
				// the request goroutine owns the slot and releases it when done
				if self.httpSem != nil {
					select {
					case self.httpSem <- struct{}{}:
					case <-stop:
						return
					case <-queryCtx.Done():
						return
					}
				}
				go HandleError(func() {
					if self.httpSem != nil {
						defer func() { <-self.httpSem }()
					}
					result := self.queryWire(queryCtx, dohUrl, recordType, name)
					// a server that returns records or an authoritative no-record answer is healthy;
					// anything else (error, non-200, or no answer before it was beaten) counts
					// against it. The large stagger means the first wave is the usual winner, so a
					// later wave is only launched — and only judged — when an earlier server was
					// slow or failed.
					self.stats.record(dohUrl, 0 < len(result.AddrTtls) || result.Miss)
					select {
					case receiveResults <- result:
					case <-queryCtx.Done():
					}
				})
			}
		}
	})

	mergedResult := newDohQueryResult()
	for range queryCount {
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
				stopLaunching()
				return &dohQueryResult{
					AddrTtls: mergedResult.AddrTtls,
				}
			}
		}
	}
	mergedResult.Miss = len(mergedResult.AddrTtls) == 0 && mergedResult.Miss
	return mergedResult
}

// queryWire runs an RFC 8484 wire-format DoH query (Accept application/dns-message,
// GET ?dns=<base64url DNS message>). name must already be punycoded ascii.
func (self *dohClient) queryWire(ctx context.Context, dohUrl string, recordType string, name string) *dohQueryResult {
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

	// the caller holds an httpSem slot for the lifetime of this request (acquired before this
	// goroutine was spawned), bounding concurrent in-flight HTTP requests across the cache —
	// the memory governor for the iOS network extension
	response, err := self.httpClient.Do(request)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDohResponseBytes))
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

// serverStats tracks each DoH server's recent successful resolutions in per-window token buckets — a
// current and a previous bucket per dohServerWindows span — and scores a server by summing the
// trailing-window estimates, so the fan-out order favors servers that have resolved most recently
// and most often. All methods are safe for concurrent use and safe to call on a nil *serverStats (a
// no-op / uniform-random order).
type serverStats struct {
	lock  sync.Mutex
	byUrl map[string]*serverStat
}

func newServerStats() *serverStats {
	return &serverStats{byUrl: map[string]*serverStat{}}
}

// serverStat holds one tokenBucket per dohServerWindows span (parallel index), counting the
// server's successful resolutions.
type serverStat struct {
	windows []tokenBucket
}

// tokenBucket is a sliding-window-counter approximation over a fixed span: current counts events in
// the current interval [epoch*span, (epoch+1)*span) and previous the interval before it. The
// trailing-window estimate prorates previous by how much of it still falls within the last span.
type tokenBucket struct {
	epoch    int64
	current  float64
	previous float64
}

// roll advances the bucket to the interval containing now: a single-interval step shifts
// current->previous; a longer gap clears both (the events fell out of the trailing window).
func (self *tokenBucket) roll(span time.Duration, now time.Time) {
	epoch := now.UnixNano() / int64(span)
	switch {
	case epoch == self.epoch:
	case epoch == self.epoch+1:
		self.previous = self.current
		self.current = 0
	default:
		self.previous = 0
		self.current = 0
	}
	self.epoch = epoch
}

func (self *tokenBucket) add(span time.Duration, now time.Time, n float64) {
	self.roll(span, now)
	self.current += n
}

// estimate returns the prorated event count over the trailing span ending at now.
func (self *tokenBucket) estimate(span time.Duration, now time.Time) float64 {
	self.roll(span, now)
	elapsed := now.UnixNano() - self.epoch*int64(span)
	frac := float64(int64(span)-elapsed) / float64(span)
	return self.current + self.previous*frac
}

// record credits a server with a successful resolution (ok == it returned records or an
// authoritative no-record answer); failures earn nothing and simply let the buckets decay.
func (self *serverStats) record(url string, ok bool) {
	self.recordAt(url, ok, time.Now())
}

func (self *serverStats) recordAt(url string, ok bool, now time.Time) {
	if self == nil || !ok {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()

	st := self.byUrl[url]
	if st == nil {
		st = &serverStat{windows: make([]tokenBucket, len(dohServerWindows))}
		self.byUrl[url] = st
	}
	for k, span := range dohServerWindows {
		st.windows[k].add(span, now, 1)
	}
}

// scoreLocked sums a server's trailing-window success estimates; an untried server scores 0.
func (self *serverStats) scoreLocked(url string, now time.Time) float64 {
	st := self.byUrl[url]
	if st == nil {
		return 0
	}
	var score float64
	for k, span := range dohServerWindows {
		score += st.windows[k].estimate(span, now)
	}
	return score
}

// order returns urls in a weighted-random permutation: a server's weight is its summed recent
// success score plus an exploration floor. Uses the Efraimidis–Spirakis weighted-permutation method
// (key = u^(1/w); higher weight -> earlier). A nil *serverStats yields a uniform-random shuffle.
func (self *serverStats) order(urls []string) []string {
	return self.orderAt(urls, time.Now())
}

func (self *serverStats) orderAt(urls []string, now time.Time) []string {
	ordered := append([]string{}, urls...)
	if len(ordered) <= 1 {
		return ordered
	}
	if self == nil {
		mathrand.Shuffle(len(ordered), func(i, j int) {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		})
		return ordered
	}

	type weighted struct {
		url string
		key float64
	}
	ws := make([]weighted, len(ordered))
	self.lock.Lock()
	for i, url := range ordered {
		weight := dohServerWeightFloor + self.scoreLocked(url, now)
		u := mathrand.Float64()
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		ws[i] = weighted{url: url, key: math.Pow(u, 1/weight)}
	}
	self.lock.Unlock()

	sort.Slice(ws, func(i, j int) bool {
		return ws[i].key > ws[j].key
	})
	for i := range ws {
		ordered[i] = ws[i].url
	}
	return ordered
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

func Punycode(domain string) (string, error) {
	name := strings.TrimSpace(domain)

	return idna.New(
		idna.MapForLookup(),
		idna.Transitional(true),
		idna.StrictDomainName(false),
	).ToASCII(name)
}
