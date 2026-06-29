package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		w.Header().Set("Content-Type", "application/dns-json")
		err := json.NewEncoder(w).Encode(&DohResponse{
			Status: 3,
		})
		assert.Equal(t, err, nil)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 1 * time.Second
	settings.MissExpiration = 1 * time.Minute
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}
	// isolate to this server (and exercise the legacy []string=json path); DefaultDohSettings
	// now seeds a real fan-out set in RemoteDohServersIpv4
	settings.DnsResolverSettings.RemoteDohServersIpv4 = nil

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
	// isolate to this server (and exercise the legacy []string=json path); DefaultDohSettings
	// now seeds a real fan-out set in RemoteDohServersIpv4
	settings.DnsResolverSettings.RemoteDohServersIpv4 = nil

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
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(&DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Type: 1, TTL: 60, Data: testIp.String()}},
		})
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 500 * time.Millisecond
	settings.MissExpiration = 1 * time.Minute
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}
	// isolate to this server (and exercise the legacy []string=json path); DefaultDohSettings
	// now seeds a real fan-out set in RemoteDohServersIpv4
	settings.DnsResolverSettings.RemoteDohServersIpv4 = nil

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
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(&DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Type: 1, TTL: 60, Data: testIp.String()}},
		})
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}
	// isolate to this server (and exercise the legacy []string=json path); DefaultDohSettings
	// now seeds a real fan-out set in RemoteDohServersIpv4
	settings.DnsResolverSettings.RemoteDohServersIpv4 = nil

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

// TestDohWireFormat: a server tagged wire is queried via RFC 8484 (?dns=<base64url>,
// application/dns-message) and its wire-format response is parsed.
func TestDohWireFormat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var gotWireQuery atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		gotWireQuery.Store(true)
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
		b.StartQuestions()
		b.Question(q)
		b.StartAnswers()
		b.AResource(
			dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60},
			dnsmessage.AResource{A: testIp.As4()},
		)
		resp, err := b.Finish()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(resp)
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{{Url: server.URL, Format: DohFormatWire}}

	dohCache := NewDohCache(settings)
	addrs := dohCache.Query(ctx, "A", "wire.example")
	assert.Equal(t, gotWireQuery.Load(), true)
	assert.Equal(t, len(addrs), 1)
	assert.Equal(t, slices.Contains(addrs, testIp), true)
}

// TestDohFanoutFastestWins: with multiple resolvers a query returns as soon as one returns
// records — a slow/dead server does not delay the lookup.
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
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(&DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Type: 1, TTL: 60, Data: testIp.String()}},
		})
	}))
	defer fast.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 8 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{
		{Url: slow.URL, Format: DohFormatJson},
		{Url: fast.URL, Format: DohFormatJson},
	}

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

// TestDohCacheMinTtl: a record with a very low (here zero) DoH TTL is cached for at least
// MinCacheTtl, so it isn't re-resolved (a full fan-out) on nearly every query.
func TestDohCacheMinTtl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testIp := netip.MustParseAddr("93.184.216.34")
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(&DohResponse{
			Status: 0,
			Answer: []DohAnswer{{Type: 1, TTL: 0, Data: testIp.String()}},
		})
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 5 * time.Second
	settings.MinCacheTtl = 2 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohServersIpv4 = []DohServer{{Url: server.URL, Format: DohFormatJson}}

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
