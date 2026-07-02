package connect

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"testing"

	"github.com/go-playground/assert/v2"
)

// writeDohWire answers the RFC 8484 wire query in r with the given records (A or AAAA matching the
// question type), or NXDOMAIN when nxdomain is set. An unparseable request gets a 400.
func writeDohWire(w http.ResponseWriter, r *http.Request, records []netip.Addr, ttl uint32, nxdomain bool) {
	raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var p dnsmessage.Parser
	header, err := p.Start(raw)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	q, err := p.Question()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h := dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true}
	if nxdomain {
		h.RCode = dnsmessage.RCodeNameError
	}
	b := dnsmessage.NewBuilder(nil, h)
	b.StartQuestions()
	b.Question(q)
	b.StartAnswers()
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: ttl}
	for _, ip := range records {
		switch {
		case q.Type == dnsmessage.TypeA && ip.Is4():
			b.AResource(rh, dnsmessage.AResource{A: ip.As4()})
		case q.Type == dnsmessage.TypeAAAA && ip.Is6() && !ip.Is4In6():
			b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: ip.As16()})
		}
	}
	resp, err := b.Finish()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	w.Write(resp)
}

func TestDohQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultDohSettings()

	testIp1, err := netip.ParseAddr("1.1.1.1")
	assert.Equal(t, err, nil)
	testIp2, err := netip.ParseAddr("10.10.10.10")
	assert.Equal(t, err, nil)

	for range 10 {
		ips := DohQuery(ctx, 4, "A", settings, "test1.bringyour.com")
		if len(ips) == 0 {
			// timeout, try again
			fmt.Printf("[doh]timeout. Will wait 1s and try again ...\n")
			select {
			case <-time.After(1 * time.Second):
				continue
			}
		}
		assert.Equal(t, len(ips), 2)
		ttl1 := ips[testIp1]
		assert.NotEqual(t, ttl1, 0)
		ttl2 := ips[testIp2]
		assert.NotEqual(t, ttl2, 0)
	}

}

func TestDohCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultDohSettings()

	dohCache := NewDohCache(settings)

	testIp1, err := netip.ParseAddr("1.1.1.1")
	assert.Equal(t, err, nil)
	testIp2, err := netip.ParseAddr("10.10.10.10")
	assert.Equal(t, err, nil)

	for range 10 {
		ips := dohCache.Query(ctx, "A", "test1.bringyour.com")
		if len(ips) == 0 {
			// timeout, try again
			fmt.Printf("[doh]timeout. Will wait 1s and try again ...\n")
			select {
			case <-time.After(1 * time.Second):
				continue
			}
		}
		assert.Equal(t, len(ips), 2)
		assert.Equal(t, slices.Contains(ips, testIp1), true)
		assert.Equal(t, slices.Contains(ips, testIp2), true)
	}

	for range 10 {
		ips := dohCache.Query(ctx, "A", "test-local.bringyour.com")
		assert.Equal(t, len(ips), 0)
	}

}

func TestDohCacheCachesMiss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		writeDohWire(w, r, nil, 0, true) // NXDOMAIN
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 1 * time.Second
	settings.MissExpiration = 1 * time.Minute
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}

	dohCache := NewDohCache(settings)

	for range 3 {
		ips := dohCache.Query(ctx, "A", "missing.example")
		assert.Equal(t, len(ips), 0)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&requestCount))
}

// TestDohCacheDoesNotCacheHttpError: an HTTP 5xx (transient server failure, not an
// authoritative NXDOMAIN) is not negative-cached — every query re-hits the resolver (a
// cached negative would be a single request). Contrast TestDohCacheCachesMiss.
func TestDohCacheDoesNotCacheHttpError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 1 * time.Second
	settings.MissExpiration = 1 * time.Minute
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}

	dohCache := NewDohCache(settings)
	for range 3 {
		assert.Equal(t, len(dohCache.Query(ctx, "A", "fail.example")), 0)
	}
	assert.Equal(t, int32(3), atomic.LoadInt32(&requestCount))
}

// TestDohCacheRetriesAfterTimeout: a timed-out query is not cached, so a retry after the
// resolver recovers resolves rather than returning a poisoned empty record.
func TestDohCacheRetriesAfterTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var failing atomic.Bool
	failing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			// stall until the client gives up (its request ctx is canceled on timeout)
			<-r.Context().Done()
			return
		}
		writeDohWire(w, r, []netip.Addr{testIp}, 60, false)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 500 * time.Millisecond
	settings.MissExpiration = 1 * time.Minute
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}

	dohCache := NewDohCache(settings)
	// first query times out -> empty, and must not be cached
	assert.Equal(t, len(dohCache.Query(ctx, "A", "recover.example")), 0)
	// resolver recovers; the retry must re-query and resolve, not return a cached empty
	failing.Store(false)
	ips := dohCache.Query(ctx, "A", "recover.example")
	assert.Equal(t, len(ips), 1)
	assert.Equal(t, slices.Contains(ips, testIp), true)
}

// TestDohCacheSingleFlight: concurrent identical queries coalesce onto a single upstream
// resolution rather than each firing its own DoH request (retry-storm / dup amplification).
func TestDohCacheSingleFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		// hold the request so concurrent callers overlap and coalesce onto this one
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
		writeDohWire(w, r, []netip.Addr{testIp}, 60, false)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}

	dohCache := NewDohCache(settings)

	const n = 16
	results := make(chan []netip.Addr, n)
	for range n {
		go func() {
			results <- dohCache.Query(ctx, "A", "coalesce.example")
		}()
	}
	for range n {
		addrs := <-results
		assert.Equal(t, len(addrs), 1)
		assert.Equal(t, slices.Contains(addrs, testIp), true)
	}

	// all 16 concurrent callers coalesced onto a single upstream request
	assert.Equal(t, int32(1), atomic.LoadInt32(&requestCount))
}

// TestDohWireFormat: a server is queried via RFC 8484 (?dns=<base64url>, application/dns-message)
// and its wire-format response is parsed.
func TestDohWireFormat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var gotWireQuery atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dns") != "" {
			gotWireQuery.Store(true)
		}
		writeDohWire(w, r, []netip.Addr{testIp}, 60, false)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}

	dohCache := NewDohCache(settings)
	addrs := dohCache.Query(ctx, "A", "wire.example")
	assert.Equal(t, gotWireQuery.Load(), true)
	assert.Equal(t, len(addrs), 1)
	assert.Equal(t, slices.Contains(addrs, testIp), true)
}

// TestDohFanoutFastestWins: with multiple resolvers fanned out at once (stagger disabled) a query
// returns as soon as one returns records — a slow/dead server does not delay the lookup.
func TestDohFanoutFastestWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(30 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{testIp}, 60, false)
	}))
	defer fast.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 8 * time.Second
	settings.DohServerStagger = 0 // fan out simultaneously to test fastest-wins
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{slow.URL, fast.URL}

	dohCache := NewDohCache(settings)
	start := time.Now()
	addrs := dohCache.Query(ctx, "A", "fanout.example")
	elapsed := time.Since(start)

	assert.Equal(t, len(addrs), 1)
	assert.Equal(t, slices.Contains(addrs, testIp), true)
	// must not have waited on the slow server
	if elapsed > 3*time.Second {
		t.Fatalf("fan-out waited %v for the slow server; should return on the fast answer", elapsed)
	}
}

// TestDohServerStagger: with the stagger enabled, a primary that answers within the stagger window
// means the next server is never launched — only one upstream request is made.
func TestDohServerStagger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var totalRequests int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&totalRequests, 1)
		writeDohWire(w, r, []netip.Addr{testIp}, 60, false)
	})
	a := httptest.NewServer(handler)
	defer a.Close()
	b := httptest.NewServer(handler)
	defer b.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 500 * time.Millisecond
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{a.URL, b.URL}

	dohCache := NewDohCache(settings)
	addrs := dohCache.Query(ctx, "A", "stagger.example")
	assert.Equal(t, slices.Contains(addrs, testIp), true)
	// the first-ordered server answers immediately, well within the 500ms stagger, so the second
	// server is never launched
	assert.Equal(t, int32(1), atomic.LoadInt32(&totalRequests))
}

// TestDohHttpConcurrencyLimit: MaxConcurrentHttpRequests hard-caps concurrent in-flight DoH
// requests across a cache, regardless of how wide the fan-out is.
func TestDohHttpConcurrencyLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var inFlight, maxInFlight int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		writeDohWire(w, r, []netip.Addr{testIp}, 60, false)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 0 // fan out simultaneously
	settings.MaxConcurrentHttpRequests = 2
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	// six servers fanned out at once; only MaxConcurrentHttpRequests may be in flight together
	urls := make([]string, 6)
	for i := range urls {
		urls[i] = server.URL
	}
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = urls

	dohCache := NewDohCache(settings)
	addrs := dohCache.Query(ctx, "A", "concurrency.example")
	assert.Equal(t, slices.Contains(addrs, testIp), true)
	if got := atomic.LoadInt32(&maxInFlight); got > 2 {
		t.Fatalf("max concurrent in-flight requests = %d, want <= 2", got)
	}
}

// TestDohCacheMinTtl: a record with a very low (here zero) DoH TTL is cached for at least
// MinCacheTtl, so it isn't re-resolved (a full fan-out) on nearly every query.
func TestDohCacheMinTtl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		writeDohWire(w, r, []netip.Addr{testIp}, 0, false)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.MinCacheTtl = 2 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}

	dohCache := NewDohCache(settings)

	addrs := dohCache.Query(ctx, "A", "lowttl.example")
	assert.Equal(t, len(addrs), 1)
	// past the record's real TTL (0) but within MinCacheTtl — still served from cache
	time.Sleep(500 * time.Millisecond)
	addrs = dohCache.Query(ctx, "A", "lowttl.example")
	assert.Equal(t, len(addrs), 1)
	assert.Equal(t, slices.Contains(addrs, testIp), true)
	// the floor kept it cached: a single upstream request despite the zero TTL
	assert.Equal(t, int32(1), atomic.LoadInt32(&requestCount))
}

// TestServerStatsTokenBucket: a server's score is the summed trailing-window success count, which
// decays as time passes without new successes.
func TestServerStatsTokenBucket(t *testing.T) {
	stats := newServerStats()
	base := time.Unix(1_700_000_000, 0)
	const url = "https://r.example/dns-query"

	// three successes now -> counted in all three windows (5/15/60m): score 3+3+3 = 9
	for range 3 {
		stats.recordAt(url, true, base)
	}
	score := func(now time.Time) float64 {
		stats.lock.Lock()
		defer stats.lock.Unlock()
		return stats.scoreLocked(url, now)
	}
	assert.Equal(t, score(base), float64(9))

	// failures earn nothing
	stats.recordAt(url, false, base)
	assert.Equal(t, score(base), float64(9))

	// some time later the score has decayed but is still positive (longer windows still count)
	mid := score(base.Add(6 * time.Minute))
	if !(0 < mid && mid < 9) {
		t.Fatalf("score after 6m = %v, want in (0,9)", mid)
	}

	// well past the longest window every bucket has aged out -> score 0
	assert.Equal(t, score(base.Add(3*time.Hour)), float64(0))
}

// TestServerStatsOrderBias: the weighted-random order favors the server with the stronger recent
// success history the large majority of the time, while the floor still lets others be tried.
func TestServerStatsOrderBias(t *testing.T) {
	stats := newServerStats()
	now := time.Unix(1_700_000_000, 0)
	good := "https://good.example/dns-query"
	bad := "https://bad.example/dns-query"
	for range 20 {
		stats.recordAt(good, true, now)
	}

	urls := []string{bad, good}
	goodFirst := 0
	for range 1000 {
		if stats.orderAt(urls, now)[0] == good {
			goodFirst++
		}
	}
	// good's weight (~60) dwarfs bad's floor (0.05), so it should lead the vast majority
	if goodFirst < 900 {
		t.Fatalf("good server led %d/1000, want >= 900", goodFirst)
	}
}
